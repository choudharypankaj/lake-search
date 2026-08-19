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

	// Presence is the expression an existence test reads, when that has to
	// differ from Column. A VARIANT key is the case: Column casts the value
	// to VARCHAR so it compares against a string literal, and an existence
	// test wants the key itself.
	//
	// Empty means Column serves both, which is true of every real column.
	Presence string

	// StopWords lists the words this column's inverted index deletes, at
	// index time and again at query time. Unlike Ngram it does change the
	// SQL, because a stopword forwarded into query() is not merely slow —
	// the analyzer removes it before the index is consulted, the clause
	// matches nothing, and no error is raised.
	//
	// Keyed lowercase. Nil means the column has no stopword filter, which is
	// the right default: a filter declared here that the index does not
	// actually have would route ordinary words onto a needless full scan.
	StopWords map[string]bool
}

// IsStopWord reports whether the whole of v is one word this field's index
// deletes. Only a single word can qualify — a phrase is the caller's problem,
// because the count of *surviving* tokens is what decides how a phrase has to
// be compiled, not whether any one of them is filtered.
func (f Field) IsStopWord(v string) bool {
	if len(f.StopWords) == 0 {
		return false
	}
	return f.StopWords[strings.ToLower(v)]
}

// EnglishStopWords is Lucene's standard English stop set, which is what
// `filters = 'english_stop'` installs.
//
// These 33 words are deleted from the query text before the index is
// consulted, so each of them is a search that returns zero rows and raises
// nothing. Verified word by word against the live index over the frozen
// window: all 33 return 0 through query(), while the two controls that are
// *not* on the list return real counts — `from` 93,645 and `replica` 1,743.
// The `from` control matters twice, because the word-boundary regex the
// emitter substitutes returns exactly 93,645 for it too: the rewrite and the
// index agree to the row on a word neither of them filters.
func EnglishStopWords() map[string]bool {
	words := []string{
		"a", "an", "and", "are", "as", "at", "be", "but", "by", "for", "if",
		"in", "into", "is", "it", "no", "not", "of", "on", "or", "such",
		"that", "the", "their", "then", "there", "these", "they", "this",
		"to", "was", "will", "with",
	}
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
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

	// TimeZone pins bare timestamp literals to a fixed UTC offset, written
	// as `+HH:MM` or `-HH:MM`. Empty — the default — leaves them as typed.
	//
	// It is optional rather than hard-coded to UTC because both halves of the
	// argument are true. A bare literal *is* read in the session's time zone,
	// and that is measurable rather than theoretical: the same pair of bounds
	// selects 152,317 rows from a UTC session and 249,253 from a Los Angeles
	// one, while pinning them with `+00:00` gives 152,317 from both. But the
	// engine also *renders* ts in the session zone, so a compiler that pins
	// the input while the reader sees local output has traded one mismatch
	// for another. The warehouse default here is UTC and the in-repo dashboard
	// pins "timezone": "utc", so the default is latent either way — which is
	// the argument for making it a deployment's choice and not this library's.
	TimeZone string

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
		expr, raw := variantPath(s.Variant, name)
		return Field{
			Column:   expr,
			Presence: raw,
			Kind:     String,
		}, false
	}
	return Field{}, false
}

// variantPath renders a VARIANT key access twice: cast to VARCHAR so it
// compares against string literals rather than JSON values, and uncast so an
// existence test can ask about the key rather than about the value.
//
// A dotted name is a path, not a key. Two rules make it read the way it is
// written: a leading `<variant>.` is the column itself and is consumed, so
// `kv.container` looks up `container` rather than a flat key literally named
// `kv.container`; and the remaining segments chain as subscripts, since
// `kv['a']['b']` is legal here and returns NULL rather than erroring when the
// path is absent.
func variantPath(col, key string) (expr, raw string) {
	segments := strings.Split(key, ".")
	if len(segments) > 1 && strings.EqualFold(segments[0], col) {
		segments = segments[1:]
	}
	if len(segments) == 1 {
		raw = col + "['" + escapeString(segments[0]) + "']"
		return raw + "::VARCHAR", raw
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
	raw = "COALESCE(" + col + "['" + escapeString(strings.Join(segments, ".")) + "'], " +
		chain.String() + ")"
	return raw + "::VARCHAR", raw
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
//
// `filters = 'english_stop'` is declared the same way, through StopWords, and
// for a sharper reason: it is the difference between a right and a wrong
// answer rather than between fast and slow. Build the table without that
// filter and the words become ordinary tokens, so drop the set here too or
// every one of them takes a needless scan.
func K8sLogs() Schema {
	stop := EnglishStopWords()
	return Schema{
		Default: "msg",
		Table:   "logs.k8s_logs",
		Fields: map[string]Field{
			"msg":         {Column: "msg", Kind: Text, Ngram: true, StopWords: stop},
			"message":     {Column: "msg", Kind: Text, Ngram: true, StopWords: stop}, // ES/OTel habit
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
