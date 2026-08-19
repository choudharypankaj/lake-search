package databend

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/choudharypankaj/lake-search/parser"
)

// MatchAll and MatchNone are the predicates emitted for an empty search.
//
// MatchAll exists because an *empty search argument* matches nothing, raises no
// error, and prunes the index scan before any surrounding boolean is evaluated:
// `match(msg,”)` is 0 rows, and so is `(1=1 OR match(msg,”))` — a literal-true
// disjunct cannot rescue it. The only safe handling of an empty search box is
// SQL containing no match() at all.
//
// The rule is about the empty argument, not about search functions under a
// boolean. A *non-empty* one composes with ordinary SQL exactly: measured,
// query('msg:peer') is 109,950, lower(level)=lower('warn') is 9,287, their
// conjunction 341, and their disjunction 118,896 = 109,950 + 9,287 - 341. The
// mixed branch of boolean() depends on that and is sound.
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
	schema   Schema
	warnings []string

	// searchFuncs counts search functions in the *outer* scan, which is what
	// the one-per-table rule and score() both care about. A search function
	// inside an anti-join subquery is a separate scan and is not counted:
	// measured, `match(msg,…) AND msg NOT IN (SELECT msg … WHERE query(…))`
	// runs and returns 17,608 rows, while `SELECT score() … ` with only the
	// subquery's search function still fails [1065] — the binder does not see
	// through the subquery either.
	searchFuncs int

	// textCols records every column that reached a text fragment, so a
	// negated fragment knows which column its anti-join keys on.
	textCols []string
}

func (c *compiler) warn(format string, args ...interface{}) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

func (c *compiler) noteTextCol(col string) {
	for _, existing := range c.textCols {
		if existing == col {
			return
		}
	}
	c.textCols = append(c.textCols, col)
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
	return c.wrapText(f)
}

// wrapText turns a query()-language expression into SQL, spending the
// statement's one search function.
//
// # Why a negated fragment is not simply wrapped in NOT
//
// `NOT (query(x))` is not the complement of `query(x)`. The optimiser pushes
// the search function into the index scan regardless of the surrounding
// boolean — the same mechanism that makes an empty search argument return
// nothing — so when x matches no row at all, the scan is pruned to nothing and
// the NOT returns **zero rows instead of every row**. Measured over a
// 152,317-row window:
//
//	token              query()   NOT (query())   anti-join
//	zzzznosuchtoken          0               0     152,317
//	qqqqwwww                 0               0     152,317
//	pdctl                    0               0     152,317
//	tiflash             23,381         128,936     128,936
//
// The three absent tokens are the defect: "everything except a term that does
// not occur here" is the whole window, and the bare NOT answers zero. It is a
// realistic thing to ask — excluding a noise pattern that happens not to occur
// in the selected time range — and it fails silently. No SQL-level wrapping
// rescues it: COALESCE, CASE, `= FALSE`, `AND TRUE` and `1=1 OR …` were all
// measured and all return zero, because the pruning happens before any of them
// is evaluated.
//
// The anti-join does rescue it, exactly, and keeps tokenised semantics rather
// than degrading to a substring match: the search function runs in its own
// scan, where pruning it to nothing correctly yields an empty exclusion set.
// A negation that keeps a positive term beside it — `a -b` — never comes here
// at all, because it stays inside one query() call where the positive drives
// the scan.
func (c *compiler) wrapText(f fragment) (string, error) {
	if f.negated {
		return c.antiJoin(f)
	}
	c.searchFuncs++
	return "query('" + escapeString(f.text) + "')", nil
}

// antiJoin renders the complement of a full-text expression.
func (c *compiler) antiJoin(f fragment) (string, error) {
	switch {
	case len(c.textCols) == 0:
		return "", fmt.Errorf("internal: negated text fragment with no text column")
	case len(c.textCols) > 1:
		return "", fmt.Errorf(
			"excluding a full-text search across more than one indexed column (%s) is not "+
				"supported: the exclusion needs an anti-join, and it can key on only one column",
			strings.Join(c.textCols, ", "))
	case c.schema.Table == "":
		return "", fmt.Errorf(
			"excluding a full-text term with no positive term beside it needs the table name, " +
				"because `NOT (query(...))` returns 0 rows rather than every row whenever the " +
				"excluded expression matches nothing: set Schema.Table, or write `a -b` so the " +
				"exclusion stays inside the search function")
	}

	col := c.textCols[0]
	c.warn("excluding %q with no positive term beside it compiles to an anti-join against %s, "+
		"which scans the table a second time: `a -b` keeps the exclusion inside the one search "+
		"function and costs nothing extra", col, c.schema.Table)

	// COALESCE covers a NULL in the outer row — a row with no text at all
	// contains nothing, so it survives the exclusion — and the subquery
	// excludes NULLs of its own so a NULL can never poison the NOT IN.
	return fmt.Sprintf(
		"COALESCE(%s NOT IN (SELECT %s FROM %s WHERE %s IS NOT NULL AND query('%s')), TRUE)",
		col, col, c.schema.Table, col, escapeString(f.text)), nil
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
	case *parser.Between:
		return c.betweenNode(t)
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
	// COALESCE, not a bare NOT: every column in a log table is nullable, and
	// `NOT (col = 'x')` is NULL — and therefore excluded — wherever col is
	// NULL, so `x` and `-x` would not add up to the table. Measured on the
	// VARIANT path, where a missing key makes it bite immediately:
	// kv['container']='vector' is 6,742 and NOT(...) is 443,148, three short
	// of the 449,893 total; the COALESCE form returns 443,151 and partitions
	// exactly. Unknown means "not excluded".
	return fragment{sql: "COALESCE(NOT (" + f.sql + "), TRUE)"}, nil
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
		wrapped, err := c.wrapText(merged)
		if err != nil {
			return fragment{}, err
		}
		parts[textSlot] = wrapped
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
		// `(a) OR NOT (b)` really does drop the negative clause silently, so
		// the shape cannot be emitted as written — but it can be rewritten.
		// De Morgan folds an OR-with-negatives into the one clause shape this
		// engine evaluates correctly, positives ANDed and negatives bare and
		// trailing, under a single SQL NOT:
		//
		//	p1 OR p2 OR NOT n1 OR NOT n2
		//	 == NOT( n1 AND n2 AND NOT(p1 OR p2) )
		//	 -> NOT( query('(n1) AND (n2) NOT ((p1) OR (p2))') )
		//
		// Still one search function. Measured: `peer OR -status` becomes
		// NOT(query('(msg:status) NOT ((msg:peer))')) = 440,181 = 449,893 total
		// minus the 9,712 rows matching `(status) NOT (peer)`.
		//
		// The result stays a text fragment rather than being spent as SQL, so
		// it composes further. That is safe because a NOT nested inside a
		// negative group is evaluated correctly here — verified, not assumed:
		// query('(region) NOT ((peer) NOT ((store)))') returns 15,634, exactly
		// 20,144 - 4,853 + 343.
		expr := strings.Join(negatives, " AND ")
		expr += " NOT (" + strings.Join(positives, " OR ") + ")"
		return fragment{text: expr, negated: true}, nil
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

	if t.Regex {
		return c.regexTerm(f, t, name)
	}

	if t.Boost != "" && f.Kind != Text {
		return fragment{}, fmt.Errorf(
			"boost ^%s needs a full-text field: %q is compared, not scored, so there is no "+
				"relevance for a weight to move", t.Boost, name)
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
		// The remediation has to be a spelling that survives the lexer: a
		// quoted literal with a space in it does not, because the space ends
		// the word and the quotes end up inside the SQL literal.
		return fragment{}, fmt.Errorf(
			"field %q is a timestamp, so it needs a comparison rather than a value: "+
				"%s:>2026-08-18T22:30:00Z, or a two-sided %s:[2026-08-18 TO 2026-08-19]", name, name, name)
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
		if t.Boost != "" {
			c.warn("boost ^%s is not expressible on a fuzzy term: fuzziness reaches the engine "+
				"through match()'s option argument, which has no weight of its own", t.Boost)
		}
		c.searchFuncs++
		return fragment{sql: fmt.Sprintf("match(%s, '%s', 'fuzziness=%d')",
			f.Column, escapeString(t.Value), t.Fuzz)}, nil

	case t.Phrase:
		// Positions are stored: "peer status" returns 72,601 where the
		// reversed phrase returns 0.
		//
		// Proximity is real here and N is honoured, which is worth stating
		// because it is easy to measure wrongly — sample only the exact phrase
		// and a large N and both land on plateaus. The full ladder, frozen:
		//
		//	"region peer"      654    "peer status"       88,441
		//	"region peer"~0    654    "status peer"            0
		//	"region peer"~1    654    "status peer"~1          0
		//	"region peer"~2  4,593    "status peer"~2     88,441
		//	"region peer"~3  4,593    "status peer"~5     88,441
		//	"region peer"~10 4,853
		//	region AND peer  4,853
		//
		// Strictly monotone, converging on the unordered AND, and the reversed
		// phrase first matches at exactly ~2 — a transposition costs two,
		// which is textbook Lucene.
		c.noteTextCol(f.Column)
		return fragment{text: c.boost(f.Column+":"+quoteQueryValue(t.Value, true)+slopSuffix(t), t)}, nil

	default:
		c.noteTextCol(f.Column)
		return fragment{text: c.boost(f.Column+":"+quoteQueryValue(t.Value, false), t)}, nil
	}
}

// slopSuffix renders a phrase's proximity marker. `~0` is left off: it asks
// for exactly the ordering an unadorned phrase already has, and the two were
// measured to return the same rows.
func slopSuffix(t *parser.Term) string {
	if t.Slop <= 0 {
		return ""
	}
	return "~" + strconv.Itoa(t.Slop)
}

// boost appends a relevance weight to a query()-language clause.
//
// It is a ranking device, not a filter: measured, `(msg:region)^5 OR (msg:peer)`
// returns the same 125,241 rows as the unweighted disjunction, and only the
// score() ordering moves. A search that never selects score() is therefore
// unaffected by it, which is worth saying out loud rather than letting someone
// conclude the boost did nothing.
func (c *compiler) boost(clause string, t *parser.Term) string {
	if t.Boost == "" {
		return clause
	}
	c.warn("boost ^%s only reorders score(); the set of matching rows is unchanged, "+
		"so it does nothing in a panel that does not select score()", t.Boost)
	return "(" + clause + ")^" + t.Boost
}

// regexTerm compiles `/pattern/` to RLIKE.
//
// RLIKE is not a search function, so a regex composes freely with the one
// query() call — but no index serves it, so it is a full scan and says so. Note
// that RLIKE is case-insensitive on this engine whatever the schema's
// CaseInsensitive setting says (measured: `msg RLIKE 'PEER.*STATUS'` and
// `msg RLIKE 'peer.*status'` both return 88,451, while `msg LIKE '%PEER%'`
// returns 0 against 109,878 for the lowercase pattern). A pattern that must be
// case-sensitive has to say so itself with `(?-i)`, which works.
func (c *compiler) regexTerm(f Field, t *parser.Term, name string) (fragment, error) {
	switch f.Kind {
	case Number, Timestamp:
		return fragment{}, fmt.Errorf(
			"field %q is not text; a regex term has nothing to match against", name)
	}
	if t.Fuzz > 0 || t.Boost != "" {
		c.warn("fuzziness and boost are not meaningful on a regex term; ignoring them on %q", f.Column)
	}
	c.warn("regex on %q compiles to RLIKE, which neither the inverted nor the NGRAM index "+
		"serves: this is a full scan", f.Column)
	return fragment{sql: f.Column + " RLIKE '" + escapeString(t.Value) + "'"}, nil
}

func (c *compiler) stringTerm(f Field, t *parser.Term) (fragment, error) {
	if t.Fuzz > 0 {
		c.warn("fuzziness needs an inverted index; %q is a plain column, matching exactly instead", f.Column)
	}
	if t.Slop > 0 {
		c.warn("phrase proximity needs an inverted index to have positions to measure; %q is a "+
			"plain column, matching the phrase exactly instead", f.Column)
	}
	if t.Prefix || t.Suffix {
		return fragment{sql: c.like(f, t.Value, t.Prefix, t.Suffix)}, nil
	}

	lit := "'" + escapeString(t.Value) + "'"
	if c.schema.CaseInsensitive {
		// There is no case-insensitive `=` on this engine — ILIKE exists, but
		// it is a LIKE, so it would turn an equality into a pattern match —
		// which is why case-insensitivity is spelled out with lower() on both
		// sides rather than borrowed from an operator.
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
	f, err := c.rangeField(r.Field)
	if err != nil {
		return fragment{}, err
	}
	col, lits, err := c.bounds(f, r.Field, r.Value)
	if err != nil {
		return fragment{}, err
	}
	return fragment{sql: col + " " + r.Op + " " + lits[0]}, nil
}

// betweenNode compiles the bracket range forms.
//
// These are ordinary SQL comparisons, never the search mini-language: that has
// a range form of its own, but it is inverted-index-only and single-token-only
// — `query('msg:[a TO b]')` fails with `[1903] Unsupported query: Range query
// boundary cannot have multiple tokens` — so routing a bracket range through it
// would trade a working predicate for an error. As plain SQL it also costs no
// search function and composes with one freely.
func (c *compiler) betweenNode(b *parser.Between) (fragment, error) {
	f, err := c.rangeField(b.Field)
	if err != nil {
		return fragment{}, err
	}

	// `[* TO *]` asks only that the field have a value.
	if b.Lo == "" && b.Hi == "" {
		return fragment{sql: c.exists(f)}, nil
	}

	var vals []string
	if b.Lo != "" {
		vals = append(vals, b.Lo)
	}
	if b.Hi != "" {
		vals = append(vals, b.Hi)
	}
	col, lits, err := c.bounds(f, b.Field, vals...)
	if err != nil {
		return fragment{}, err
	}

	if b.Lo != "" && b.Hi != "" && b.LoIncl && b.HiIncl {
		return fragment{sql: col + " BETWEEN " + lits[0] + " AND " + lits[1]}, nil
	}

	var parts []string
	i := 0
	if b.Lo != "" {
		op := ">"
		if b.LoIncl {
			op = ">="
		}
		parts = append(parts, col+" "+op+" "+lits[i])
		i++
	}
	if b.Hi != "" {
		op := "<"
		if b.HiIncl {
			op = "<="
		}
		parts = append(parts, col+" "+op+" "+lits[i])
	}
	if len(parts) == 1 {
		return fragment{sql: parts[0]}, nil
	}
	return fragment{sql: "(" + strings.Join(parts, " AND ") + ")"}, nil
}

// rangeField resolves the field of any comparison and rejects the one kind
// that cannot carry one.
func (c *compiler) rangeField(name string) (Field, error) {
	f, known := c.schema.Lookup(name)
	if f.Column == "" {
		return Field{}, fmt.Errorf("unknown field %q and no VARIANT column configured", name)
	}
	if !known {
		c.warn("field %q is not a column; reading it from the %s VARIANT (no index, full scan)",
			name, c.schema.Variant)
	}
	if f.Kind == Text {
		return Field{}, fmt.Errorf("field %q is full-text indexed; ranges are not meaningful on it", name)
	}
	return f, nil
}

// bounds renders the column expression and the literals of a comparison.
//
// Whether the bounds are numeric is decided once for the whole predicate rather
// than per bound, so a two-sided range cannot end up comparing one side as a
// number and the other as a string.
func (c *compiler) bounds(f Field, name string, vals ...string) (col string, lits []string, err error) {
	numeric := true
	for _, v := range vals {
		if _, e := strconv.ParseFloat(v, 64); e != nil {
			numeric = false
		}
	}

	switch f.Kind {
	case Number:
		if !numeric {
			for _, v := range vals {
				if _, e := strconv.ParseFloat(v, 64); e != nil {
					return "", nil, fmt.Errorf("field %q is numeric but %q is not a number", name, v)
				}
			}
		}
		return f.Column, vals, nil

	case Timestamp:
		return f.Column, quoteAll(vals), nil

	default:
		// A VARIANT value arrives as VARCHAR, so a numeric comparison needs the
		// second cast; a string one compares as it stands.
		if numeric {
			return f.Column + "::DOUBLE", vals, nil
		}
		return f.Column, quoteAll(vals), nil
	}
}

func quoteAll(vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = "'" + escapeString(v) + "'"
	}
	return out
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
