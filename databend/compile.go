package databend

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/choudharypankaj/lake-search/parser"
)

// MatchAll and MatchNone are the predicates emitted for an empty search.
//
// MatchAll exists because of the trap recorded in LOG_PIPELINE_FINDINGS.md §5.1:
// `match(msg,”)` matches nothing, raises no error, and the optimiser pushes
// match() into the index scan regardless of the surrounding boolean — so
// `(” = ” OR match(msg,”))` also returns zero rows. The only safe handling of
// an empty search box is SQL that contains no match() call at all.
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

	// UsesMatch reports whether the predicate contains a match() or query()
	// call. score() is only legal alongside one — see CompileScore.
	UsesMatch bool
}

// Compile renders a parsed query as a Databend predicate.
func Compile(n parser.Node, s Schema) (Result, error) {
	if s.Default == "" {
		return Result{}, fmt.Errorf("schema has no default field")
	}
	c := &compiler{schema: s}
	if n == nil {
		return Result{SQL: MatchAll}, nil
	}
	sql, err := c.node(n)
	if err != nil {
		return Result{}, err
	}
	return Result{SQL: sql, Warnings: c.warnings, UsesMatch: c.usesMatch}, nil
}

// CompileString parses and compiles in one step.
func CompileString(q string, s Schema) (Result, error) {
	return Compile(parser.Parse(q), s)
}

// CompileScore renders a predicate for a panel that also selects score().
//
// Databend rejects score() unless a match() or query() is present in the same
// query — `[1065] Score function must be used together with match or query
// function` — so a relevance panel cannot fall back to 1=1 on an empty search.
// It must return no rows instead.
func CompileScore(q string, s Schema) (Result, error) {
	r, err := CompileString(q, s)
	if err != nil {
		return r, err
	}
	if !r.UsesMatch {
		r.SQL = MatchNone
		r.Warnings = append(r.Warnings,
			"score() requires a match()/query() predicate; emitting 1=0 so the panel returns no rows")
	}
	return r, nil
}

type compiler struct {
	schema    Schema
	warnings  []string
	usesMatch bool
}

func (c *compiler) warn(format string, args ...interface{}) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

func (c *compiler) node(n parser.Node) (string, error) {
	switch t := n.(type) {
	case *parser.And:
		return c.join(t.Children, " AND ")
	case *parser.Or:
		return c.join(t.Children, " OR ")
	case *parser.Not:
		inner, err := c.node(t.Child)
		if err != nil {
			return "", err
		}
		// Negating a match() leans on the optimiser handling an inverted
		// index scan under NOT. It is correct in principle; the conformance
		// suite asserts it against real data because §5.1 showed that
		// match() pushdown does not always respect surrounding booleans.
		return "NOT (" + inner + ")", nil
	case *parser.Term:
		return c.term(t)
	case *parser.Range:
		return c.rangeNode(t)
	default:
		return "", fmt.Errorf("unsupported node %T", n)
	}
}

func (c *compiler) join(children []parser.Node, op string) (string, error) {
	parts := make([]string, 0, len(children))
	for _, ch := range children {
		sql, err := c.node(ch)
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
	}
	switch len(parts) {
	case 0:
		return MatchAll, nil
	case 1:
		return parts[0], nil
	default:
		return "(" + strings.Join(parts, op) + ")", nil
	}
}

func (c *compiler) term(t *parser.Term) (string, error) {
	name := t.Field
	if name == "" {
		name = c.schema.Default
	}
	f, known := c.schema.Lookup(name)
	if f.Column == "" {
		return "", fmt.Errorf("unknown field %q and no VARIANT column configured", name)
	}
	if !known && t.Field != "" {
		c.warn("field %q is not a column; reading it from the %s VARIANT (no index, full scan)",
			t.Field, c.schema.Variant)
	}

	if t.Exists {
		return c.exists(f), nil
	}

	switch f.Kind {
	case Text:
		return c.textTerm(f, t)
	case Number:
		if t.Prefix || t.Suffix || t.Fuzz > 0 {
			c.warn("wildcards and fuzziness are not meaningful on numeric field %q; comparing for equality", name)
		}
		if _, err := strconv.ParseFloat(t.Value, 64); err != nil {
			return "", fmt.Errorf("field %q is numeric but %q is not a number", name, t.Value)
		}
		return f.Column + " = " + t.Value, nil
	case Timestamp:
		return "", fmt.Errorf("field %q is a timestamp; use a range such as %s:>'2026-08-18 00:00:00'", name, name)
	default:
		return c.stringTerm(f, t)
	}
}

// textTerm is the interesting one: four different SQL forms, chosen by shape.
func (c *compiler) textTerm(f Field, t *parser.Term) (string, error) {
	switch {
	case t.Prefix || t.Suffix:
		// The trap from §5.10: `query('msg:pref*')` silently ignores the star
		// and returns whatever the bare stem returns — so `snapsh*` yields 0
		// while `snapshot*` yields the full baseline, which looks like the
		// wildcard worked. LIKE is the mechanism that actually does prefix
		// matching, accelerated by an NGRAM index if one exists.
		if !f.Ngram {
			c.warn("wildcard on %q falls back to LIKE; without an NGRAM index this is a full scan "+
				"(CREATE NGRAM INDEX ... ON <table>(%s))", f.Column, f.Column)
		}
		if t.Fuzz > 0 {
			c.warn("fuzziness is ignored when a wildcard is present on %q", f.Column)
		}
		return c.like(f, t.Value, t.Prefix, t.Suffix), nil

	case t.Phrase:
		// Phrase search goes through query(), which stores positions:
		// "peer status" matched 72,601 rows while "status peer" matched 0.
		c.usesMatch = true
		inner := f.Column + `:"` + escapeQueryPhrase(t.Value) + `"`
		return "query('" + escapeString(inner) + "')", nil

	case t.Fuzz > 0:
		// `term~N` is Lucene syntax and returns zero rows inside query();
		// the option form is index-backed and correct.
		//
		// Edit distance is measured against the *stem*, not the word the user
		// typed: `unreachable` is indexed as `unreach`, so `unreachble` is two
		// edits away, not one. Any UI exposing this must say so.
		c.usesMatch = true
		return fmt.Sprintf("match(%s, '%s', 'fuzziness=%d')",
			f.Column, escapeString(t.Value), t.Fuzz), nil

	default:
		c.usesMatch = true
		return fmt.Sprintf("match(%s, '%s')", f.Column, escapeString(t.Value)), nil
	}
}

func (c *compiler) stringTerm(f Field, t *parser.Term) (string, error) {
	if t.Fuzz > 0 {
		c.warn("fuzziness needs an inverted index; %q is a plain column, matching exactly instead", f.Column)
	}
	if t.Prefix || t.Suffix {
		return c.like(f, t.Value, t.Prefix, t.Suffix), nil
	}

	lit := "'" + escapeString(t.Value) + "'"
	if c.schema.CaseInsensitive {
		// Databend has no ILIKE, so case-insensitivity is explicit.
		return fmt.Sprintf("lower(%s) = lower(%s)", f.Column, lit), nil
	}
	return f.Column + " = " + lit, nil
}

// like builds a LIKE predicate. A leading star means "ends with", a trailing
// star "starts with", both means "contains", and a bare term with neither is
// reached only from a plain string field, where the caller wants equality.
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

func (c *compiler) rangeNode(r *parser.Range) (string, error) {
	f, known := c.schema.Lookup(r.Field)
	if f.Column == "" {
		return "", fmt.Errorf("unknown field %q and no VARIANT column configured", r.Field)
	}
	if !known {
		c.warn("field %q is not a column; reading it from the %s VARIANT (no index, full scan)",
			r.Field, c.schema.Variant)
	}

	switch f.Kind {
	case Number:
		if _, err := strconv.ParseFloat(r.Value, 64); err != nil {
			return "", fmt.Errorf("field %q is numeric but %q is not a number", r.Field, r.Value)
		}
		return f.Column + " " + r.Op + " " + r.Value, nil

	case Timestamp:
		return fmt.Sprintf("%s %s '%s'", f.Column, r.Op, escapeString(r.Value)), nil

	case Text:
		return "", fmt.Errorf("field %q is full-text indexed; ranges are not meaningful on it", r.Field)

	default:
		// A VARIANT-derived or plain string column compared as a number: cast
		// so `store_id:>7` orders numerically rather than lexicographically.
		if _, err := strconv.ParseFloat(r.Value, 64); err == nil {
			return fmt.Sprintf("%s::DOUBLE %s %s", f.Column, r.Op, r.Value), nil
		}
		return fmt.Sprintf("%s %s '%s'", f.Column, r.Op, escapeString(r.Value)), nil
	}
}

// escapeString makes a value safe inside a single-quoted Databend literal.
// Backslash is escaped as well as the quote, because Databend honours
// backslash escapes inside string literals — a Windows path or a regex in a log
// line would otherwise be mangled or, worse, break out of the literal.
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
