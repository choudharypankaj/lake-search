package databend

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/choudharypankaj/lake-search/parser"
)

// MatchAll and MatchNone are the predicates emitted for an empty search.
//
// MatchAll exists because `match(col,”)` matches nothing, raises no error, and
// the optimiser pushes match() into the index scan regardless of the surrounding
// boolean — so `(” = ” OR match(msg,”))` also returns zero rows. The only
// safe handling of an empty search box is SQL containing no match() at all.
const (
	MatchAll  = "1=1"
	MatchNone = "1=0"
)

// Result is a compiled predicate plus anything the caller should be told about
// how it will execute.
type Result struct {
	// SQL is a boolean expression safe to splice into a WHERE clause.
	SQL string

	// Warnings describe predicates that are correct but slow, or syntax that
	// was rewritten. They are advisory: the SQL is usable regardless.
	Warnings []string

	// UsesMatch reports whether the predicate contains a search function.
	// score() is only legal alongside one — see CompileScore.
	UsesMatch bool
}

// Compile renders a parsed query as a Databend predicate.
//
// # The one-search-function rule
//
// Measured on a live warehouse (Databend v0.34.0): a statement may contain **at
// most one** search function per table. Two of them — in any combination —
// fail with `[1065] … duplicate search function for table 0`:
//
//	match(msg,'a') AND match(msg,'b')     -- rejected
//	match(msg,'a') AND query('msg:b')     -- rejected
//	query('msg:a') AND query('msg:b')     -- rejected
//
// So boolean full-text logic cannot be composed with SQL AND/OR. It has to be
// pushed *inside* a single query() call, whose own mini-language supports it:
//
//	query('msg:peer AND msg:status')      -- 72,603 rows, agrees with LIKE
//	query('msg:peer NOT msg:status')      -- 18,839 = 91,442 - 72,603 exactly
//
// This compiler therefore collects every full-text leaf into one query() string
// and keeps structured predicates (columns, ranges, LIKE) as ordinary SQL,
// which are not search functions and combine freely. When a query genuinely
// needs two search functions it is rejected at compile time with an
// explanation, because the alternative is SQL that returns [1065] at runtime.
func Compile(n parser.Node, s Schema) (Result, error) {
	if s.Default == "" {
		return Result{}, fmt.Errorf("schema has no default field")
	}
	if n == nil {
		return Result{SQL: MatchAll}, nil
	}

	c := &compiler{schema: s}
	f, err := c.render(n)
	if err != nil {
		return Result{}, err
	}
	sql, err := c.finalize(f)
	if err != nil {
		return Result{}, err
	}
	if c.searchFuncs > 1 {
		return Result{}, fmt.Errorf(
			"this query needs %d search functions but Databend allows one per table: "+
				"put the full-text terms in a single group so they compile to one query() call "+
				"(for example `(a OR b) level:error` rather than `(a level:error) OR (b level:warn)`)",
			c.searchFuncs)
	}
	return Result{SQL: sql, Warnings: c.warnings, UsesMatch: c.searchFuncs > 0}, nil
}

// CompileString parses and compiles in one step.
func CompileString(q string, s Schema) (Result, error) {
	return Compile(parser.Parse(q), s)
}

// ScoreSentinel is a token chosen to match nothing. It exists so that a
// relevance panel with an empty search box still contains a search function.
const ScoreSentinel = "zzqqnolakesearchmatchqqzz"

// CompileScore renders a predicate for a panel that also selects score().
//
// Databend rejects score() unless a search function is present in the same
// statement: `[1065] [SQL-BINDER] Score function must be used together with
// match or query function`.
//
// The obvious workaround — emitting `1=0` when the box is empty — does **not**
// work, and this was verified against a live warehouse rather than assumed.
// The binder looks for a search function *anywhere in the statement*, so
// `SELECT score() … WHERE 1=0` still fails. Since the score() call sits in the
// select list, no predicate can rescue it.
//
// What does work is a search function that matches nothing: the binder is
// satisfied, the panel returns zero rows, and no error reaches the user.
func CompileScore(q string, s Schema) (Result, error) {
	r, err := CompileString(q, s)
	if err != nil {
		return r, err
	}
	if !r.UsesMatch {
		col := s.Default
		if f, ok := s.Fields[strings.ToLower(s.Default)]; ok {
			col = f.Column
		}
		r.SQL = fmt.Sprintf("match(%s, '%s')", col, ScoreSentinel)
		r.UsesMatch = true
		r.Warnings = append(r.Warnings,
			"score() needs a search function even when the search is empty; matching a sentinel token "+
				"so the panel returns no rows instead of [1065]")
	}
	return r, nil
}

// fragment is a partially compiled subtree, in one of two languages.
//
// Keeping them apart is what makes the one-search-function rule enforceable:
// text fragments merge into a single query() call, SQL fragments compose
// freely, and the boundary between them is the only place a search function is
// ever spent.
type fragment struct {
	// text holds a query()-language expression, e.g. `msg:peer AND msg:status`.
	text string

	// sql holds ordinary SQL, e.g. `lower(level) = lower('error')`.
	sql string

	// negated applies to text fragments only. Negation is carried rather than
	// applied so that `a -b` stays inside one query() call instead of becoming
	// `query(a) AND NOT query(b)`, which would be two search functions.
	negated bool
}

func (f fragment) isText() bool { return f.text != "" }

type compiler struct {
	schema      Schema
	warnings    []string
	searchFuncs int
}

func (c *compiler) warn(format string, args ...interface{}) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

// finalize converts the root fragment into SQL, spending a search function if
// the fragment is full-text.
func (c *compiler) finalize(f fragment) (string, error) {
	if !f.isText() {
		if f.sql == "" {
			return MatchAll, nil
		}
		return f.sql, nil
	}
	return c.wrapText(f), nil
}

// wrapText spends one search function, turning a query()-language expression
// into SQL.
func (c *compiler) wrapText(f fragment) string {
	c.searchFuncs++
	sql := "query('" + escapeString(f.text) + "')"
	if f.negated {
		// Verified: NOT around a whole query() call is legal and correct
		// (237,325 rows against a 73,201-row positive match on 309,007 total).
		return "NOT (" + sql + ")"
	}
	return sql
}

func (c *compiler) render(n parser.Node) (fragment, error) {
	switch t := n.(type) {
	case *parser.And:
		return c.boolean(t.Children, "AND")
	case *parser.Or:
		return c.boolean(t.Children, "OR")
	case *parser.Not:
		return c.not(t.Child)
	case *parser.Term:
		return c.term(t)
	case *parser.Range:
		return c.rangeNode(t)
	default:
		return fragment{}, fmt.Errorf("unsupported node %T", n)
	}
}

func (c *compiler) not(child parser.Node) (fragment, error) {
	f, err := c.render(child)
	if err != nil {
		return fragment{}, err
	}
	if f.isText() {
		// Carried, not applied — see fragment.negated.
		f.negated = !f.negated
		return f, nil
	}
	return fragment{sql: "NOT (" + f.sql + ")"}, nil
}

// boolean combines children, keeping everything in the text language when it
// can so that only one search function is spent.
func (c *compiler) boolean(children []parser.Node, op string) (fragment, error) {
	frags := make([]fragment, 0, len(children))
	allText := true
	for _, ch := range children {
		f, err := c.render(ch)
		if err != nil {
			return fragment{}, err
		}
		frags = append(frags, f)
		if !f.isText() {
			allText = false
		}
	}

	switch len(frags) {
	case 0:
		return fragment{}, nil
	case 1:
		return frags[0], nil
	}

	if allText {
		return c.textBoolean(frags, op)
	}

	// Mixed. The text children are merged into a single text expression first
	// and wrapped once — `level:error "peer status" -TiFlash` is an ordinary
	// query, and wrapping each text leaf separately would spend three search
	// functions on it and fail.
	var textFrags []fragment
	parts := make([]string, 0, len(frags))
	textSlot := -1
	for _, f := range frags {
		if f.isText() {
			if textSlot < 0 {
				textSlot = len(parts)
				parts = append(parts, "") // reserved, filled in below
			}
			textFrags = append(textFrags, f)
			continue
		}
		parts = append(parts, f.sql)
	}

	if len(textFrags) > 0 {
		merged := textFrags[0]
		if len(textFrags) > 1 {
			var err error
			merged, err = c.textBoolean(textFrags, op)
			if err != nil {
				return fragment{}, err
			}
		}
		parts[textSlot] = c.wrapText(merged)
	}

	return fragment{sql: "(" + strings.Join(parts, " "+op+" ") + ")"}, nil
}

// textBoolean combines full-text fragments inside the query() mini-language.
//
// Its shape is dictated by three measured behaviours, none of which are
// documented and all of which fail silently rather than erroring:
//
//	(a) AND NOT (b)          -> 0 rows.  `AND NOT` is broken in every spelling.
//	(a) NOT (b) AND (c)      -> ignores everything after the first NOT.
//	(a) OR NOT (b)           -> silently drops the negative clause entirely.
//
// The forms that do work, cross-checked against equivalent LIKE predicates:
//
//	(a) AND (b) NOT (c)      -> 72,603, matches LIKE exactly
//	(a) NOT (b) NOT (c)      -> 14,027, matches LIKE exactly
//
// So negatives must be bare (never `AND NOT`), must come last, and cannot
// appear under OR at all.
func (c *compiler) textBoolean(frags []fragment, op string) (fragment, error) {
	var positives, negatives []string
	for _, f := range frags {
		if f.negated {
			negatives = append(negatives, "("+f.text+")")
		} else {
			positives = append(positives, "("+f.text+")")
		}
	}

	// All children negated: De Morgan turns this into a single positive query
	// wrapped in one SQL NOT, which is both legal and correct —
	// `NOT a AND NOT b` is `NOT (a OR b)`.
	if len(positives) == 0 {
		inner := "OR"
		if op == "OR" {
			inner = "AND" // NOT a OR NOT b  ==  NOT (a AND b)
		}
		return fragment{text: strings.Join(negatives, " "+inner+" "), negated: true}, nil
	}

	if len(negatives) > 0 && op == "OR" {
		return fragment{}, fmt.Errorf(
			"a negated term cannot be combined with OR in full-text search: " +
				"Databend drops the negative clause silently, so `a OR -b` would quietly mean `a`. " +
				"Rewrite as `(a) -b` if you meant to exclude from the whole result")
	}

	expr := strings.Join(positives, " "+op+" ")
	for _, n := range negatives {
		// Deliberately no operator: `AND NOT` returns zero rows.
		expr += " NOT " + n
	}
	return fragment{text: expr}, nil
}

func (c *compiler) term(t *parser.Term) (fragment, error) {
	name := t.Field
	if name == "" {
		name = c.schema.Default
	}
	f, known := c.schema.Lookup(name)
	if f.Column == "" {
		return fragment{}, fmt.Errorf("unknown field %q and no VARIANT column configured", name)
	}
	if !known && t.Field != "" {
		c.warn("field %q is not a column; reading it from the %s VARIANT (no index, full scan)",
			t.Field, c.schema.Variant)
	}

	if t.Exists {
		return fragment{sql: c.exists(f)}, nil
	}

	switch f.Kind {
	case Text:
		return c.textTerm(f, t)
	case Number:
		if t.Prefix || t.Suffix || t.Fuzz > 0 {
			c.warn("wildcards and fuzziness are not meaningful on numeric field %q; comparing for equality", name)
		}
		if _, err := strconv.ParseFloat(t.Value, 64); err != nil {
			return fragment{}, fmt.Errorf("field %q is numeric but %q is not a number", name, t.Value)
		}
		return fragment{sql: f.Column + " = " + t.Value}, nil
	case Timestamp:
		return fragment{}, fmt.Errorf("field %q is a timestamp; use a range such as %s:>'2026-08-18 00:00:00'", name, name)
	default:
		return c.stringTerm(f, t)
	}
}

// textTerm produces either a text fragment (merged into the single query()
// call) or a SQL fragment (wildcards, which LIKE handles and which cost no
// search function).
func (c *compiler) textTerm(f Field, t *parser.Term) (fragment, error) {
	switch {
	case t.Prefix || t.Suffix:
		// `*` inside query() is silently ignored — every starred query equals
		// its unstarred form — so a wildcard has to become LIKE. Measured:
		// `snapsh*` gives 0 through query() but 1,019 through LIKE 'snapsh%'.
		//
		// LIKE is not a search function, so this composes freely with the one
		// query() call the statement is allowed.
		if !f.Ngram {
			c.warn("wildcard on %q falls back to LIKE; without an NGRAM index this is a full scan "+
				"(CREATE NGRAM INDEX ... ON <table>(%s))", f.Column, f.Column)
		}
		if t.Fuzz > 0 {
			c.warn("fuzziness is ignored when a wildcard is present on %q", f.Column)
		}
		return fragment{sql: c.like(f, t.Value, t.Prefix, t.Suffix)}, nil

	case t.Fuzz > 0:
		// Fuzziness exists only as an option argument to match(); inside
		// query() the Lucene `~N` form returns zero rows silently. That makes
		// a fuzzy term a search function in its own right, so it cannot share
		// a statement with any other full-text term. It does compose with
		// structured filters and LIKE, both verified.
		c.searchFuncs++
		return fragment{sql: fmt.Sprintf("match(%s, '%s', 'fuzziness=%d')",
			f.Column, escapeString(t.Value), t.Fuzz)}, nil

	case t.Phrase:
		// Positions are stored: "peer status" returns 72,601 where the
		// reversed phrase returns 0.
		return fragment{text: f.Column + ":" + quoteQueryValue(t.Value, true)}, nil

	default:
		return fragment{text: f.Column + ":" + quoteQueryValue(t.Value, false)}, nil
	}
}

func (c *compiler) stringTerm(f Field, t *parser.Term) (fragment, error) {
	if t.Fuzz > 0 {
		c.warn("fuzziness needs an inverted index; %q is a plain column, matching exactly instead", f.Column)
	}
	if t.Prefix || t.Suffix {
		return fragment{sql: c.like(f, t.Value, t.Prefix, t.Suffix)}, nil
	}

	lit := "'" + escapeString(t.Value) + "'"
	if c.schema.CaseInsensitive {
		// Databend has no ILIKE, so case-insensitivity is explicit.
		return fragment{sql: fmt.Sprintf("lower(%s) = lower(%s)", f.Column, lit)}, nil
	}
	return fragment{sql: f.Column + " = " + lit}, nil
}

func (c *compiler) like(f Field, value string, prefix, suffix bool) string {
	pat := escapeLike(value)
	if suffix {
		pat = "%" + pat
	}
	if prefix {
		pat = pat + "%"
	}
	lit := "'" + escapeString(pat) + "'"
	if c.schema.CaseInsensitive {
		return fmt.Sprintf("lower(%s) LIKE lower(%s)", f.Column, lit)
	}
	return fmt.Sprintf("%s LIKE %s", f.Column, lit)
}

func (c *compiler) exists(f Field) string {
	switch f.Kind {
	case Number, Timestamp:
		return f.Column + " IS NOT NULL"
	default:
		return "(" + f.Column + " IS NOT NULL AND " + f.Column + " <> '')"
	}
}

func (c *compiler) rangeNode(r *parser.Range) (fragment, error) {
	f, known := c.schema.Lookup(r.Field)
	if f.Column == "" {
		return fragment{}, fmt.Errorf("unknown field %q and no VARIANT column configured", r.Field)
	}
	if !known {
		c.warn("field %q is not a column; reading it from the %s VARIANT (no index, full scan)",
			r.Field, c.schema.Variant)
	}

	switch f.Kind {
	case Number:
		if _, err := strconv.ParseFloat(r.Value, 64); err != nil {
			return fragment{}, fmt.Errorf("field %q is numeric but %q is not a number", r.Field, r.Value)
		}
		return fragment{sql: f.Column + " " + r.Op + " " + r.Value}, nil

	case Timestamp:
		return fragment{sql: fmt.Sprintf("%s %s '%s'", f.Column, r.Op, escapeString(r.Value))}, nil

	case Text:
		return fragment{}, fmt.Errorf("field %q is full-text indexed; ranges are not meaningful on it", r.Field)

	default:
		if _, err := strconv.ParseFloat(r.Value, 64); err == nil {
			return fragment{sql: fmt.Sprintf("%s::DOUBLE %s %s", f.Column, r.Op, r.Value)}, nil
		}
		return fragment{sql: fmt.Sprintf("%s %s '%s'", f.Column, r.Op, escapeString(r.Value))}, nil
	}
}

// quoteQueryValue renders a value inside the query() mini-language.
//
// Anything that is not a plain token is double-quoted, which also makes it a
// phrase. That is the safe default: an unquoted value containing a space would
// otherwise be split, and the default operator between bare terms inside
// query() is OR, not AND — measured, `query('msg:peer msg:status')` returns
// 93,425, exactly the union. This compiler never relies on that default and
// always emits explicit operators.
func quoteQueryValue(v string, phrase bool) string {
	if !phrase && isPlainToken(v) {
		return v
	}
	return `"` + escapeQueryPhrase(v) + `"`
}

func isPlainToken(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// escapeString makes a value safe inside a single-quoted Databend literal.
// Backslash is escaped as well as the quote, because Databend honours backslash
// escapes inside string literals.
func escapeString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return s
}

// escapeLike additionally neutralises the LIKE metacharacters so a literal % or
// _ in a search term does not become a wildcard.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// escapeQueryPhrase escapes the double quotes that delimit a phrase inside the
// query() mini-language, before the whole expression is escaped again as a SQL
// literal.
func escapeQueryPhrase(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
