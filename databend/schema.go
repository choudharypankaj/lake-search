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
	// the way it would in Kibana — and Databend has no ILIKE.
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
func variantPath(col, key string) string {
	return col + "['" + escapeString(key) + "']::VARCHAR"
}

// K8sLogs is the schema for the logs.k8s_logs table built in
// LOG_PIPELINE_FINDINGS.md: nine columns fed by a Vector DaemonSet, with an
// INVERTED index on msg and arbitrary parsed [k=v] pairs in kv.
func K8sLogs() Schema {
	return Schema{
		Default: "msg",
		Fields: map[string]Field{
			"msg":         {Column: "msg", Kind: Text},
			"message":     {Column: "msg", Kind: Text}, // ES/OTel habit
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
