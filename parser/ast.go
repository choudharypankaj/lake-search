// Package parser turns Lucene-style search text into a small, engine-neutral
// syntax tree.
//
// The split is deliberate and mirrors how HyperDX is built: a generic grammar
// front end (there, a fork of bripkens/lucene) feeding an engine-specific SQL
// emitter (there, a 2,200-line ClickHouse renderer). The grammar is commodity;
// the emitter is where every dialect trap lives. Keeping them apart means the
// Databend emitter in ../databend can be reviewed, tested and corrected on its
// own, and a second emitter can be added without touching this package.
//
// This package therefore knows nothing about SQL, Databend, indexes or columns.
package parser

// Node is one element of the parsed query.
type Node interface{ node() }

// And requires every child to match. It is also the *implicit* operator between
// adjacent terms: `foo bar` parses as And{foo, bar}.
//
// Lucene's own default is OR, but every log-search UI that matters — Kibana's
// KQL, ClickStack/HyperDX — defaults to AND, because with OR a second search
// term makes the result set larger, which is never what someone narrowing down
// an incident wants. We follow the log-search convention, not Lucene's.
type And struct{ Children []Node }

// Or matches if any child matches.
type Or struct{ Children []Node }

// Not inverts its child. Produced by both `NOT x` and `-x`.
type Not struct{ Child Node }

// Term is a single leaf match against one field.
type Term struct {
	// Field is the field name as typed, or "" for a bare term, which the
	// emitter routes to the schema's default field.
	Field string

	// Value is the unescaped search text.
	Value string

	// Phrase records that the value was double-quoted, requesting an
	// order-sensitive phrase match rather than a bag of tokens.
	Phrase bool

	// Fuzz is N from a trailing `~N` (a bare `~` means 1). Zero means no
	// fuzziness was requested.
	Fuzz int

	// Prefix and Suffix record wildcards: `foo*` sets Prefix, `*foo` sets
	// Suffix, `*foo*` sets both. A wildcard-only value (`field:*`) sets
	// Exists instead.
	Prefix bool
	Suffix bool

	// Exists is set by `field:*`, asking whether the field has any value.
	Exists bool
}

// Range is a numeric or temporal comparison: `duration:>1000`.
type Range struct {
	Field string
	Op    string // ">", ">=", "<", "<="
	Value string
}

func (*And) node()   {}
func (*Or) node()    {}
func (*Not) node()   {}
func (*Term) node()  {}
func (*Range) node() {}
