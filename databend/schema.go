// Package databend compiles a parsed search tree into a Databend SQL predicate,
// suitable for TiDB Cloud Lake (which is Databend-backed).
//
// This is the half that has to be right. The grammar in ../parser is commodity;
// everything engine-specific — which construct is index-backed, which one
// silently returns nothing, how a wildcard has to be rewritten — lives here.
package databend

import "strings"

// Kind decides how a field is searched.
type Kind int

const (
	// Text is a VARCHAR column carrying an INVERTED index, searched with
	// match() / query() and rankable with score().
	Text Kind = iota

	// String is a plain VARCHAR column, compared with = or LIKE.
	String

	// Number is any numeric column, supporting = and range comparisons.
	Number

	// Timestamp is a TIMESTAMP column, supporting range comparisons against
	// a literal.
	Timestamp
)

// Field describes one searchable field.
type Field struct {
	// Column is the SQL column expression. It is usually the field name, but
	// may be any expression — a STORED computed column, for instance.
	Column string

	Kind Kind

	// Ngram records that the column carries an NGRAM index, which makes LIKE
	// prefix and substring searches index-backed rather than a full scan. It
	// affects no SQL; it only decides whether a warning is emitted.
	Ngram bool
}

// Schema maps the field names a user types onto real columns.
type Schema struct {
	// Default is the field name a bare term searches, e.g. "msg".
	Default string

	// Table is the qualified table the predicate will run against.
	//
	// It is needed for exactly one construct: excluding a full-text term with
	// no positive term beside it. A bare SQL NOT around a search function is
	// not the complement of that search — the engine prunes the scan when the
	// search matches nothing, so `-absent_token` returns zero rows instead of
	// every row. The correct form is an anti-join, and an anti-join has to
	// name the table. Leave it empty and that one shape becomes a compile
	// error rather than a wrong answer.
	Table string

	// Fields is keyed by the lowercased name the user types.
	Fields map[string]Field

	// Variant, when non-empty, names a VARIANT column that unknown field
	// names are routed into: `store_id:7` becomes kv['store_id']::VARCHAR.
	//
	// This is what makes the schema open-ended. The unified TiDB/TiKV/PD/TiCDC
	// log format carries arbitrary [k=v] pairs with names that differ between
	// components, so no fixed column list can cover them.
	Variant string

	// CaseInsensitive compares plain string columns with lower() on both
	// sides. Defaults on, because `level:error` must match a stored "ERROR"
	// the way it would in Kibana.
	//
	// Databend does have ILIKE — measured, `pod ILIKE '%tikv%'` and
	// `lower(pod) LIKE lower('%tikv%')` return the same 189,623 rows — but
	// there is no case-insensitive `=`, so equality needs lower() on both
	// sides regardless. LIKE is spelled the same way for symmetry, and so
	// that one flag governs both.
	CaseInsensitive bool
}

// Lookup resolves a typed field name to a Field.
//
// The second return value reports whether the name was found in the schema;
// when it was not and a Variant column is configured, a synthetic Field over
// that column is returned instead.
func (s Schema) Lookup(name string) (Field, bool) {
	if f, ok := s.Fields[strings.ToLower(name)]; ok {
		return f, true
	}
	if s.Variant != "" {
		return Field{
			Column: variantPath(s.Variant, name),
			Kind:   String,
		}, false
	}
	return Field{}, false
}

// variantPath renders a VARIANT key access, cast to VARCHAR so it compares
// against string literals rather than JSON values.
//
// A dotted name is a path, not a key. Two rules make it read the way it is
// written: a leading `<variant>.` is the column itself and is consumed, so
// `kv.container` looks up `container` rather than a flat key literally named
// `kv.container`; and the remaining segments chain as subscripts, since
// `kv['a']['b']` is legal here and returns NULL rather than erroring when the
// path is absent.
func variantPath(col, key string) string {
	segments := strings.Split(key, ".")
	if len(segments) > 1 && strings.EqualFold(segments[0], col) {
		segments = segments[1:]
	}
	if len(segments) == 1 {
		return col + "['" + escapeString(segments[0]) + "']::VARCHAR"
	}

	// Both readings are legitimate once dots are in play: `a.b` may be a path
	// to a nested value, or it may be a flat key that happens to contain a dot
	// — bag keys come from log lines and nothing forbids one. Chaining alone
	// would make the flat key unreachable, so the flat form is tried first and
	// the path is the fallback. A missing subscript is NULL rather than an
	// error here, which is what makes the COALESCE work.
	var chain strings.Builder
	chain.WriteString(col)
	for _, seg := range segments {
		chain.WriteString("['")
		chain.WriteString(escapeString(seg))
		chain.WriteString("']")
	}
	return "COALESCE(" + col + "['" + escapeString(strings.Join(segments, ".")) + "'], " +
		chain.String() + ")::VARCHAR"
}

// K8sLogs is the schema for the logs.k8s_logs table built in
// LOG_PIPELINE_FINDINGS.md: nine columns fed by a Vector DaemonSet, with
// arbitrary parsed [k=v] pairs in kv and two indexes on msg — an INVERTED one
// carrying the english tokenizer, stopword filter and stemmer, and an NGRAM
// one that makes LIKE prefix and substring searches index-backed:
//
//	CREATE TABLE logs.k8s_logs (
//	  ts TIMESTAMP NULL, component VARCHAR NULL, level VARCHAR NULL,
//	  namespace VARCHAR NULL, pod VARCHAR NULL, node VARCHAR NULL,
//	  source_file VARCHAR NULL, msg VARCHAR NULL, kv VARIANT NULL,
//	  SYNC INVERTED INDEX idx_msg (msg)
//	    filters = 'english_stop,english_stemmer', tokenizer = 'english',
//	  SYNC NGRAM INDEX idx_msg_ng (msg)
//	) ENGINE=FUSE CLUSTER BY (to_date(ts), component)
//
// Declaring the NGRAM index here is what suppresses the full-scan warning on
// wildcard searches. Point this at a table built without it and the warning is
// correct, so drop the Ngram flag rather than the index.
func K8sLogs() Schema {
	return Schema{
		Default: "msg",
		Table:   "logs.k8s_logs",
		Fields: map[string]Field{
			"msg":         {Column: "msg", Kind: Text, Ngram: true},
			"message":     {Column: "msg", Kind: Text, Ngram: true}, // ES/OTel habit
			"ts":          {Column: "ts", Kind: Timestamp},
			"timestamp":   {Column: "ts", Kind: Timestamp},
			"component":   {Column: "component", Kind: String},
			"level":       {Column: "level", Kind: String},
			"namespace":   {Column: "namespace", Kind: String},
			"pod":         {Column: "pod", Kind: String},
			"node":        {Column: "node", Kind: String},
			"source_file": {Column: "source_file", Kind: String},
		},
		Variant:         "kv",
		CaseInsensitive: true,
	}
}
