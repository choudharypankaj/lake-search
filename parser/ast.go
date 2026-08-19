// Package parser turns Lucene-style search text into a small, engine-neutral
// syntax tree.
//
// The split is deliberate, and it is how every mature implementation of this
// pattern ends up structured: a generic grammar front end feeding an
// engine-specific SQL emitter. The grammar is commodity — Lucene's shape has
// been stable for two decades — while the emitter is where every dialect trap
// lives, and it is invariably the larger half. Keeping them apart means the
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
// Lucene's own default is OR, but log-search interfaces conventionally default
// to AND — Kibana's KQL among them — because with OR a second search term makes
// the result set *larger*, which is never what someone narrowing down an
// incident wants. We follow the log-search convention, not Lucene's.
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

	// Slop is N from a `~N` after a closing quote — Lucene's phrase
	// proximity. Zero means none was requested, which includes `~0`: on a
	// phrase that asks for exactly the ordering an unadorned phrase already
	// has, so there is nothing to reject.
	Slop int

	// Boost is N from a trailing `^N`, kept as typed. It is a relevance
	// weight, not a filter: it changes the order score() produces and leaves
	// the matched set alone.
	Boost string

	// Regex records that Value is the body of a `/pattern/` term.
	Regex bool

	// Wildcard records that Value carries Lucene's wildcards — `*` for any
	// run of characters, `?` for exactly one — anywhere in the value, not
	// only at its ends. They are left in Value as typed, because where they
	// sit is what the emitter has to translate: `reg*on` is a different
	// pattern from `reg*` and from `*on`, and collapsing them all into a
	// pair of end flags is how a mid-word star ends up silently discarded.
	//
	// A value that is nothing but stars (`field:*`) sets Exists instead.
	Wildcard bool

	// Exists is set by `field:*`, asking whether the field has any value.
	Exists bool
}

// Range is a one-sided numeric or temporal comparison: `duration:>1000`.
type Range struct {
	Field string
	Op    string // ">", ">=", "<", "<="
	Value string
}

// Between is a two-sided range written in bracket form: `field:[a TO b]` for
// inclusive bounds, `{a TO b}` for exclusive ones, mixed spellings allowed. A
// bound of `*` is unbounded, so `[a TO *]` is a one-sided comparison and
// `[* TO *]` is an existence check.
type Between struct {
	Field  string
	Lo, Hi string // "" or "*" for an unbounded side
	LoIncl bool
	HiIncl bool
}

func (*And) node()     {}
func (*Or) node()      {}
func (*Not) node()     {}
func (*Term) node()    {}
func (*Range) node()   {}
func (*Between) node() {}
