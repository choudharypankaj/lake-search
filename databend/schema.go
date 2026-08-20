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

	// Index names the INVERTED index this column belongs to. It matters for
	// exactly one rule, and that rule is not cosmetic: a single query() call
	// may span several columns only when one index covers all of them.
	// Measured on a table carrying two separate inverted indexes,
	// idx_line(line) and idx_line2(line2), each column searches fine alone
	// (1 row each) while `query('line:RemoteStopped AND line2:RemoteStopped')`
	// fails [1065] "columns line2, line don't have inverted index" — the
	// engine reports the whole set as unindexed rather than telling you they
	// live apart. So a compiler that merges text leaves without checking this
	// emits SQL that cannot run.
	//
	// Empty means unknown, which is the pre-existing behaviour: no check is
	// made and a single-text-column schema behaves exactly as before.
	Index string

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

	// Search is how the field is named inside a query() expression, when that
	// differs from Column. A VARIANT key is the case and it is the only one:
	// an inverted index covers a VARIANT column by JSON path, so the key
	// `err` of column `kv` is searched as `kv.err` and read as
	// kv['err']::VARCHAR.
	//
	// It is what makes a bag key index-backed rather than a full scan.
	// Measured on a 967,914-row copy indexed over (msg, line, kv):
	// query('kv.err:RemoteStopped') returns 507, exactly the 507 that
	// lower(kv['err']::VARCHAR) = lower('RemoteStopped') returns, and
	// query('msg:rpc AND kv.err:RemoteStopped') composes them in one call.
	// A key never declared anywhere is searchable the moment it is written,
	// with no DDL: that is the property that makes an open-ended bag work.
	//
	// Empty means the field is not reachable through an index by name, either
	// because its column carries none or because its key cannot be spelled in
	// the query language.
	Search string

	// Derived is the expression a STORED computed column is generated from,
	// carried through from the descriptor so drift detection can notice that
	// the table's definition of it has changed. A redefined derived column
	// changes what every bare word searches, which is a wrong answer rather
	// than a slow one.
	Derived string

	// Conversion, when non-empty, records that Column reaches this field's Kind
	// through a per-value cast, and describes what was converted. It makes the
	// compiler warn on every use, because such a cast does not fail — it
	// yields NULL, and the row leaves the comparison with nothing said.
	//
	// It exists because of an asymmetry that shipped. A numeric bound on a
	// string-valued expression warns loudly (see Numeric). A TIMESTAMP reached
	// the same way did not warn at all, and it is the worse of the two: a
	// numeric cast drops rows from one filter, while the time role gates every
	// time-bounded query there is — every dashboard panel, every
	// $__timeFilter, every conformance window. A value that does not cast
	// removes its row from all of them at once.
	//
	// Measured on a 3-row probe whose event_time holds two ISO timestamps and
	// the string `yesterday`: TRY_CAST(event_time AS TIMESTAMP) IS NOT NULL is
	// 2, and `event_time:>2026-08-01T00:00:00Z` returns 2 of 3 rows. The third
	// is invisible to every query that bounds by time.
	Conversion string

	// Numeric is the expression a numeric comparison reads, when that has to
	// differ from Column. A typed VARIANT key is the case: Column renders the
	// value as VARCHAR so equality and LIKE work on it, and a `>` has to read
	// the same key as a number instead.
	//
	// Empty means the expression is derived, and the derivation is where a
	// measured defect was fixed. A numeric bound against a VARCHAR-valued
	// expression used to compile to a hard `::DOUBLE`, which does not merely
	// mis-sort — it kills the statement. Over logs.k8s_logs_v2 (967,912 rows,
	// ts < 2026-08-19 22:19:00):
	//
	//	term:>40      kv['term']::VARCHAR::DOUBLE > 40         32,929 rows
	//	store_id:>100 kv['store_id']::VARCHAR::DOUBLE > 100    [1006] invalid
	//	                float literal ... to_float64('Some(25)')
	//	err:>5        kv['err']::VARCHAR::DOUBLE > 5           [1006]
	//	component:>5  component::DOUBLE > 5                    [1006] 'other'
	//
	// One non-numeric value anywhere in the column or key fails the whole
	// query, and mixed keys are the norm rather than the exception: 1,243 of
	// store_id's 40,516 rows are `Some(25)`-style debug renderings — 39,273
	// cast, so 40,516 − 39,273 = 1,243 do not. TRY_CAST
	// yields NULL for those rows instead, and agrees exactly with a hard cast
	// wherever the hard cast survives — 32,929 = 32,929 for term:>40 and
	// 0 = 0 for term:<9 against `::BIGINT` — so nothing is traded for it:
	//
	//	TRY_CAST(kv['term']::VARCHAR AS DOUBLE) > 40       32,929
	//	TRY_CAST(kv['store_id']::VARCHAR AS DOUBLE) > 100  39,140
	//	TRY_CAST(kv['err']::VARCHAR AS DOUBLE) > 5              0
	//	TRY_CAST(component AS DOUBLE) > 5                       0
	Numeric string

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

	// Example is one real value of this field, for the help text a reader
	// scans before typing anything. It exists because the generic form is
	// worse than the specific one: `component:value` tells someone the
	// syntax, `component:tikv` tells them the syntax AND that component is
	// the thing that says tikv, which is the half they cannot guess. Empty
	// means the help text falls back to the placeholder, which is the right
	// default -- a made-up value in a real deployment's help strip is a
	// search that returns nothing.
	Example string
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

// NumericColumn is the expression a numeric comparison against this field
// reads.
//
// A field declared Number is already a number and is read as it stands.
// Anything else is a string-valued expression that has to be converted, and the
// conversion is TRY_CAST rather than a cast for the reason recorded on Numeric:
// a cast fails the statement on the first value that is not a number, and log
// attributes are mixed by nature.
func (f Field) NumericColumn() string {
	if f.Numeric != "" {
		return f.Numeric
	}
	if f.Kind == Number {
		return f.Column
	}
	return "TRY_CAST(" + f.Column + " AS DOUBLE)"
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

	// Time, Severity and Display are the deployment's *roles*, named by field
	// name rather than by column, so a caller that has to build a statement
	// around the predicate does not have to know this deployment's spelling.
	//
	// They exist because the alternative was a hardcoded select list. `SELECT
	// ts, level, component, pod, msg` was written into the CLI and into the
	// dashboard generator, which meant the library was schema-driven and the
	// two things that actually issue SQL were not.
	//
	// Time is the timestamp column a log view orders by. Severity is the
	// level column, and is optional: plenty of log tables have none, and a
	// deployment without one must be told so at load time rather than
	// discovering it when a panel renders every line as INFO.
	Time     string
	Severity string

	// Display lists the field names a row-level view selects, in order. It
	// defaults to Time, Severity and Default when a schema does not say.
	Display []string

	// Indexes records the table's indexes as declared. The compiler reads only
	// the per-Field summary derived from them — Field.Index, Field.Ngram,
	// Field.StopWords — but keeping the declarations lets a schema be checked
	// against the engine and a migration be written from it.
	Indexes []Index

	// Bags are the VARIANT columns unknown field names are routed into, in
	// resolution order: `store_id:7` becomes kv['store_id']::VARCHAR.
	//
	// This is what makes the schema open-ended. The unified TiDB/TiKV/PD/TiCDC
	// log format carries arbitrary [k=v] pairs with names that differ between
	// components, so no fixed column list can cover them.
	//
	// There is more than one because a table split by role — resource
	// attributes in one bag, event attributes in another — is the ordinary
	// shape rather than the exception, and a single column can reach only one
	// of them. A bag with a Prefix is addressed explicitly
	// (`resource.k8s.pod.name`); a bag without one is a catch-all and is tried
	// in declaration order.
	Bags []Bag

	// Variant is the single-bag spelling, kept because it is the field callers
	// built Schema values with by hand. It behaves as one trailing catch-all
	// bag. Prefer Bags.
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

// Bag is one VARIANT column that undeclared field names are read from.
type Bag struct {
	// Column is the VARIANT column.
	Column string

	// Prefix, when set, is the name segment that addresses this bag
	// explicitly: with Prefix "resource", `resource.pod` reads
	// resource_attrs['pod']. A bag with no prefix is a catch-all.
	Prefix string

	// Index names the inverted index covering Column, and StopWords are the
	// words that index deletes. Both are derived from the schema's index
	// declarations rather than set by hand.
	//
	// They are what let a bag key be searched through the index. StopWords is
	// load-bearing rather than advisory: a bag value that is a stopword is not
	// reachable through the index at all, and the index says so by returning
	// nothing rather than by erroring. Measured — a row written with
	// kv = {"verb":"the"} is found by lower(kv['verb']::VARCHAR)=lower('the')
	// (1 row) and not by query('kv.verb:the') (0 rows). So a stopword value
	// has to skip the index entirely.
	Index     string
	StopWords map[string]bool

	// Keys types individual keys. A key not listed here is String, which is
	// the only safe default — a bag key's type is not knowable from the
	// schema, and guessing numeric would turn every equality into a cast.
	//
	// # What declaring a key actually changes, and what it does not
	//
	// It does NOT change how a `>` is compiled. A bound whose literal is a
	// number converts the key either way, declared or not — that rule lives in
	// bounds() and applies to undeclared keys too, which is the whole reason
	// declaring one is an override rather than a requirement:
	//
	//	term:<9   undeclared   TRY_CAST(kv['term']::VARCHAR AS DOUBLE) < 9
	//	term:<9   declared     TRY_CAST(kv['term']::VARCHAR AS DOUBLE) < 9
	//
	// What it changes is the *equality*, which has no literal to read a type
	// from and therefore defaults to a string compare:
	//
	//	term:40   undeclared   lower(kv['term']::VARCHAR) = lower('40')
	//	term:40   declared     TRY_CAST(kv['term']::VARCHAR AS DOUBLE) = 40
	//
	// On this table's `term` the two agree — both 26 rows over
	// logs.k8s_logs_v2 (967,912 rows, ts < 2026-08-19 22:19:00) — because every
	// value is a canonical integer. The difference bites on a non-canonical
	// spelling, where the string compare is simply wrong: measured,
	// lower('040') = lower('40') is false while TRY_CAST('040' AS DOUBLE) = 40
	// is true, and the same for '40.0'.
	//
	// For the record, because an earlier version of this comment claimed
	// otherwise: comparing a bag key as *text* is not a defect this compiler
	// ever had. `kv['term']::VARCHAR < '9'` returns 32,961 rows where the truth
	// is 0 — every value from 10 to 99 is textually less than "9" — but that is
	// SQL nothing here has emitted. The real numeric defect was the hard cast;
	// see Field.Numeric.
	// Keyed by the EXACT declared spelling. Field names are folded and bag keys
	// are not; see the comment at the assignment site in Def.Schema. Lookup is
	// exact too, so the three things that must agree — what was declared, what
	// is looked up, and what the emitted subscript says — all agree.
	Keys map[string]Kind
}

// Lookup resolves a typed field name to a Field.
//
// The second return value reports whether the name was found in the schema's
// declared fields; when it was not and a bag can serve it, a synthetic Field
// over that bag is returned instead.
//
// Bag resolution mirrors how the name reads. A name whose first segment is a
// bag's prefix belongs to that bag and the prefix is consumed, so a deployment
// can be unambiguous when it needs to be. Everything else falls to the
// catch-all bags in declaration order.
func (s Schema) Lookup(name string) (Field, bool) {
	if f, ok := s.Fields[strings.ToLower(name)]; ok {
		return f, true
	}
	bags := s.bags()
	for _, b := range bags {
		if b.Prefix == "" {
			continue
		}
		if rest, ok := trimPrefixSegment(name, b.Prefix); ok {
			return b.field(rest), false
		}
	}
	for _, b := range bags {
		if b.Prefix == "" {
			return b.field(name), false
		}
	}
	return Field{}, false
}

// bags returns the resolution order, with the deprecated single Variant column
// appended as a trailing catch-all.
func (s Schema) bags() []Bag {
	if s.Variant == "" {
		return s.Bags
	}
	for _, b := range s.Bags {
		if b.Column == s.Variant {
			return s.Bags
		}
	}
	return append(append([]Bag{}, s.Bags...), Bag{Column: s.Variant})
}

// field renders one key of this bag.
func (b Bag) field(key string) Field {
	// The key is normalised first, so that every reading of it agrees. A
	// leading `<column>.` is the column itself and is consumed — `kv.container`
	// is the key `container` — and forgetting that once was a real bug: the
	// search path was built from the raw name and came out as `kv.kv.container`,
	// which is a key that does not exist. The suite caught it as a bag lookup
	// returning 0 against 44,639.
	key = normalizeKey(b.Column, key)
	expr, raw := variantPath(b.Column, key)
	kind := String
	// Exact, not folded, and for the same reason the declaration is stored
	// exactly: the subscript this Field will carry is built from the name the
	// user typed, so a declaration under a different spelling describes a
	// different key. `tableID: number` must not make `tableid:5` numeric,
	// because kv['tableid'] is not the key that was typed about.
	if k, ok := b.Keys[lastSegment(key)]; ok {
		kind = k
	}
	f := Field{Column: expr, Presence: raw, Kind: kind, Index: b.Index, StopWords: b.StopWords}
	if b.Index != "" && searchableKeyPath(key) {
		f.Search = b.Column + "." + key
	}
	if kind == Number {
		// The value is stored as JSON, so a numeric comparison reads it
		// through VARCHAR and converts. TRY_CAST rather than a cast: see
		// Field.Numeric.
		f.Numeric = "TRY_CAST(" + expr + " AS DOUBLE)"
	}
	return f
}

// normalizeKey consumes a leading `<column>.` so that `kv.container` and
// `container` are the same key of column kv.
func normalizeKey(col, key string) string {
	if rest, ok := trimPrefixSegment(key, col); ok {
		return rest
	}
	return key
}

// searchableKeyPath reports whether a bag key can be spelled inside a query()
// expression as `<column>.<key>`.
//
// The query language has no quoting for a field name, so a key with a space or
// a bracket in it — and bag keys come from log lines, where `msg type` is a
// real key — cannot be addressed there. Such a key stays on the equality path,
// which reads it as a subscript and is exact about it.
func searchableKeyPath(key string) bool {
	if key == "" {
		return false
	}
	for _, seg := range strings.Split(key, ".") {
		if seg == "" {
			return false
		}
		for i, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			case r >= '0' && r <= '9' && i > 0:
			default:
				return false
			}
		}
	}
	return true
}

// trimPrefixSegment removes a leading `<prefix>.` from a dotted name.
func trimPrefixSegment(name, prefix string) (string, bool) {
	if len(name) <= len(prefix)+1 || name[len(prefix)] != '.' {
		return "", false
	}
	if !strings.EqualFold(name[:len(prefix)], prefix) {
		return "", false
	}
	return name[len(prefix)+1:], true
}

// lastSegment is the key a Keys entry is written against: a dotted path is
// typed by its leaf, so `resource.latency_ms` and `latency_ms` mean the same
// declaration.
func lastSegment(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
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
	// Kept even though Bag.field normalises first: variantPath is reachable
	// from a hand-built Schema too, and the guard is idempotent.
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

// K8sLogs is the built-in schema for logs.k8s_logs as it stands today: nine
// columns, arbitrary parsed [k=v] pairs in kv, and two indexes on msg. The
// declaration itself lives in presets.go, in the same JSON a deployment would
// write to a file.
//
// It exists as a function because it is the convenient default for the CLI and
// for callers who want the shipped shape. A deployment with its own log table
// should not edit it: point lake-search at a schema file instead, or pick
// another preset by name. See Preset and LoadSchema.
func K8sLogs() Schema { return mustPreset("k8s-logs") }

// K8sLogsLine is logs.k8s_logs after the derived-text-surface migration: the
// same table plus a STORED `line` column that concatenates the message with the
// values of the attribute bag, and one inverted index spanning both text
// columns so a bare word finds text the pipeline moved out of msg.
//
// It is a separate preset rather than a change to K8sLogs because the migration
// is a deployment decision. Pointing this at an unmigrated table gives [1065]
// on the first bare term, which is the honest outcome — the alternative is a
// library that assumes a column the table does not have.
func K8sLogsLine() Schema { return mustPreset("k8s-logs-line") }

// mustPreset panics on a broken built-in. A preset is a compile-time constant
// in this package, so a failure here is a bug in this file and not something a
// caller can handle; the package tests load every preset for exactly that
// reason.
func mustPreset(name string) Schema {
	s, _, err := Preset(name)
	if err != nil {
		panic("databend: built-in preset " + name + ": " + err.Error())
	}
	return s
}
