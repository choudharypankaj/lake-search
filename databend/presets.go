package databend

// The built-in presets, held as the same JSON a deployment would write to a
// file. Keeping them in the on-disk form rather than as Go literals means the
// file format is exercised by every test in this package: a preset that stops
// loading is a broken format, not just a broken preset.

// k8sLogsDef describes logs.k8s_logs as it stands today — nine columns fed by
// a Vector DaemonSet, arbitrary parsed [k=v] pairs in kv, and two indexes on
// msg:
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
// That block is what SHOW CREATE TABLE *prints*, and it is NOT what the engine
// *accepts*. Two traps in it, both measured:
//
//   - The inline index clauses are rejected: replaying this statement gives
//     `[1005] unexpected INVERTED, expecting INTEGER, …`. An index has to be a
//     separate CREATE INVERTED INDEX / CREATE NGRAM INDEX statement.
//   - The comma between `filters = …` and `tokenizer = …` is part of the
//     rendering, not of the option syntax. `CREATE INVERTED INDEX … tokenizer =
//     'english', filters = '…'` is [1005]; the options are separated by
//     whitespace, `tokenizer = 'english' filters = '…'`.
//
// Copy the shape from here, never the punctuation.
//
// Both indexes are declared here rather than assumed. The NGRAM declaration is
// what suppresses the full-scan warning on wildcard searches, and the
// `english_stop` filter is what tells the compiler that 33 ordinary words are
// deleted before the index is consulted — the difference between a right and a
// wrong answer, not between fast and slow. Point this at a table built without
// either and the declaration is what has to change.
const k8sLogsDef = `{
  "table": "logs.k8s_logs",
  "default": "msg",
  "time": "ts",
  "severity": "level",
  "variant": "kv",
  "case_insensitive": true,
  "display": ["ts", "level", "component", "pod", "node", "source_file", "msg"],
  "indexes": [
    {"name": "idx_msg", "kind": "inverted", "columns": ["msg"],
     "tokenizer": "english", "filters": ["english_stop", "english_stemmer"]},
    {"name": "idx_msg_ng", "kind": "ngram", "columns": ["msg"]}
  ],
  "fields": [
    {"name": "msg", "kind": "text", "aliases": ["message"]},
    {"name": "ts", "kind": "timestamp", "aliases": ["timestamp"]},
    {"name": "component", "kind": "string", "example": "tikv"},
    {"name": "level", "kind": "string"},
    {"name": "namespace", "kind": "string"},
    {"name": "pod", "kind": "string"},
    {"name": "node", "kind": "string"},
    {"name": "source_file", "kind": "string"}
  ]
}`

// k8sLogsLineDef is the same table after the derived-text-surface migration in
// round4-live-migration.sql: one extra STORED column, and one inverted index
// spanning both text columns.
//
// # What the derived surface is for
//
// The pipeline lifts `k=v` pairs out of the message into the kv bag, so text a
// reader can see in the original line is not in msg any more. Measured on the
// frozen copy logs.k8s_logs_v2 (967,912 rows, ts < 2026-08-19 22:19:00):
// `err=RemoteStopped` lives in the bag, so `msg LIKE '%RemoteStopped%'` is 0
// rows while `line LIKE '%RemoteStopped%'` is 605. Through the index after the
// migration: query('msg:RemoteStopped') is 0 and query('line:RemoteStopped') is
// 605, matching the LIKE oracle exactly.
//
// # Why one index over both columns
//
// A single query() call may span several columns only when one index covers all
// of them, and two indexes cannot be mixed: on a probe table carrying separate
// idx_line(line) and idx_line2(line2), each searches fine alone while
// `query('line:x AND line2:x')` fails [1065]. One index over (msg, line)
// therefore buys both halves at once — `msg:` keeps its narrow scope, a bare
// term reaches the whole reconstructed line, and the two compose inside the
// statement's single search function. Verified: query('line:RemoteStopped AND
// msg:rpc') is 585, and the controls do not move — query('msg:peer') is 337,971
// before and after, query('msg:"peer status"') 311,971 before and after, and
// query('msg:to') stays 0 because the stopword filter is still installed.
//
// The default field is `line`, which is the whole point: a bare word finds text
// the pipeline moved into the bag. `msg` remains declared and remains narrow,
// so a user who wants only the message half still has it.
//
// # Why the bag is in the group too, and what that does not buy
//
// An inverted index covers a VARIANT column by JSON path, so putting kv in the
// group makes every bag key index-backed by name — with no per-key DDL and no
// per-key declaration, including keys that first appear after the index was
// built. Measured on a 967,914-row copy indexed over (msg, line, kv):
// query('kv.err:RemoteStopped') is 507, query('msg:rpc AND kv.err:RemoteStopped')
// composes them in one call, and a nested path resolves. `err:RemoteStopped`
// stops being a full scan.
//
// What it does *not* buy is the bare word, and that is why `line` is still
// here. There is no all-fields search in this query language: query() with no
// field is an error and so is `kv.*:x`. Only an explicit cross-field OR works,
// and a compiler cannot write one over keys it does not know. A single derived
// column that already contains the bag's values is the only way "type a word,
// find it anywhere in the line" can work.
//
// Cost, measured on the same copy: REFRESH INVERTED INDEX takes 3.26s over
// (msg, line) and 7.02s over (msg, line, kv) for 967,912 rows across 7 blocks —
// the bag roughly doubles the build. system.tables reported no change in
// index_size across the rebuild, so that figure is not being quoted as a size
// measurement.
//
// # The derived expression, and what it deliberately leaves out
//
// `line` concatenates msg with the bag's *values*, not its `key=value` pairs.
// The trade-off is real in both directions. Values-only means a key name is
// reachable only as field syntax — a bare `err` does not find every row that
// merely has an err key, which is the noise the pair form would add — while the
// pair form would make `err` match hundreds of thousands of rows that share
// nothing but a key. Field-scoped search covers the key case properly now that
// the bag is indexed, so the noise is not worth buying.
//
// Three keys are deleted before the concatenation — container, service, format
// — because the collector injects them and they are not part of the line a
// reader saw. nullif keeps a row whose bag has nothing else from gaining a
// stray `[]`.
//
// # `raw`, and why it is a string
//
// `raw` is a plain column holding the log line exactly as the container wrote
// it, populated forward-only by the collector. It is declared here because NOT
// declaring a real column is a silent wrong answer, not an omission: with a bag
// configured, `raw:hello` was routed into it and compiled to
// `query('kv.raw:hello') AND lower(kv['raw']::VARCHAR) = lower('hello')` —
// measured over `[2026-08-20 01:00, 02:00)` (18,883 rows), `kv['raw'] IS NOT
// NULL` is 0 while `raw IS NOT NULL` is 18,883, so that query was empty forever
// and would have stayed empty however many rows arrived. The advisory it
// raised made it worse by asserting something false, that "raw is not a column".
//
// Kind `string` is the only honest choice: `raw` is not in the inverted index
// group, and declaring a text field outside the group is refused at load — one
// query() call reaches the columns of one index. So `raw:x` is an exact equality
// on a whole log line, which is nearly useless, and `raw:*hello*` is the usable
// form: it compiles to LIKE and warns that nothing indexes it, because the NGRAM
// index covers (msg, line) and not raw.
//
// It is deliberately absent from `display`, and the reason has a shape rather
// than a number. `raw` is forward-only: the rows written before the column
// existed cannot acquire a value, so its fill rate is near zero across
// accumulated history and rising toward one going forward. Both halves,
// two-sided so they stay checkable, and both windows CLOSED so they stay true:
// over `ts < '2026-08-20 00:00:00'` (997,592 rows) 0 have a value, and over
// `[2026-08-20 01:00, 02:00)` (18,883 rows) — an hour entirely after the
// collector change — 18,883 do, all of them.
//
// The earlier version of this comment quoted `[00:00, 02:00)` as 18,260 of
// 31,948. Both numbers were wrong the moment they were written, because 02:00
// had not happened yet: the window kept filling, and the same query read
// 23,880 of 37,568 an hour later. A bound in the future is not a bound. The
// window also straddled the collector change, which is what made the ratio
// meaningless rather than merely stale — 57.2% described the position of one
// clock tick inside the window, not anything about the column.
//
// A single whole-table ratio would be quoting a moment for a different reason:
// it read 3,711 of 1,014,991 when this comment was first written and 12,796 of
// 1,024,076 an hour later. That is why the figures above are bounded on both
// sides, sit wholly on one side of the change, and the argument below rests on
// the shape rather than on any of them.
//
// So a column reserved for it in a log view is mostly empty *today*, and the
// dashboard surfaces it where it belongs instead, as `coalesce(raw, line)` in
// the log body — which is right whichever way the ratio goes. Note that the
// introspector's own 5%-of-sampled-rows suppression rule is window-dependent
// for exactly this reason and will start keeping `raw` in `display` once the
// profiled window sits after the change; that is the rule working, not failing.
//
// # A divergence worth stating
//
// A bare word here searches the message *and* the attribute values. The
// underlying query language does not do that and neither do comparable tools:
// this is a deliberate choice, bought with a stored column, because a user who
// can see a value in a rendered log line reasonably expects to be able to
// search for it.
const k8sLogsLineDef = `{
  "table": "logs.k8s_logs",
  "default": "line",
  "time": "ts",
  "severity": "level",
  "variant": "kv",
  "case_insensitive": true,
  "display": ["ts", "level", "component", "pod", "node", "source_file", "line"],
  "indexes": [
    {"name": "idx_msg", "kind": "inverted", "columns": ["msg", "line", "kv", "source_file"],
     "tokenizer": "english", "filters": ["english_stop", "english_stemmer"]},
    {"name": "idx_msg_ng", "kind": "ngram", "columns": ["msg", "line"]}
  ],
  "bags": [{"column": "kv"}],
  "fields": [
    {"name": "line", "kind": "text", "aliases": ["body"],
     "derived": "concat_ws(' ', msg, nullif(json_path_query_array(object_delete(kv,'container','service','format'),'$.*')::VARCHAR,'[]'))"},
    {"name": "msg", "kind": "text", "aliases": ["message"]},
    {"name": "ts", "kind": "timestamp", "aliases": ["timestamp"]},
    {"name": "component", "kind": "string", "example": "tikv"},
    {"name": "level", "kind": "string"},
    {"name": "namespace", "kind": "string"},
    {"name": "pod", "kind": "string"},
    {"name": "node", "kind": "string"},
    {"name": "source_file", "kind": "text", "aliases": ["file"],
     "example": "compaction_runner.rs"},
    {"name": "raw", "kind": "string"}
  ]
}`
