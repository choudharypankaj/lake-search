package databend

import (
	"strings"
	"testing"
)

func TestCompile(t *testing.T) {
	s := K8sLogs()

	cases := []struct {
		name  string
		query string
		want  string
	}{
		// --- the empty-search trap (§5.1) ---
		{"empty", "", MatchAll},
		{"whitespace only", "   ", MatchAll},

		// --- plain terms ---
		//
		// Every full-text leaf merges into ONE query() call: Databend allows a
		// single search function per table, so `match(a) AND match(b)` is
		// rejected with [1065] duplicate search function.
		{"bare term", "TiFlash", `query('msg:TiFlash')`},
		{"two terms are ANDed", "peer status",
			`query('(msg:peer) AND (msg:status)')`},
		{"explicit AND", "peer AND status",
			`query('(msg:peer) AND (msg:status)')`},
		{"explicit OR", "peer OR status",
			`query('(msg:peer) OR (msg:status)')`},

		// --- phrases are order-sensitive via query() ---
		{"phrase", `"peer status"`, `query('msg:"peer status"')`},

		// --- negation, both spellings ---
		//
		// Negation stays *inside* the query() call rather than becoming
		// `query(a) AND NOT query(b)`, which would spend two search functions.
		// A leading negation cannot be a bare SQL NOT: `NOT (query(x))` returns
		// 0 rows rather than every row whenever x matches nothing, because the
		// search function prunes the scan before the NOT is evaluated. The
		// anti-join puts the search function in its own scan, where an empty
		// match correctly yields an empty exclusion set.
		{"NOT keyword", "NOT TiFlash", `COALESCE(msg NOT IN (SELECT msg FROM logs.k8s_logs WHERE msg IS NOT NULL AND query('msg:TiFlash')), TRUE)`},
		// Negatives are bare and trailing: `AND NOT` returns zero rows on this
		// engine, in every spelling.
		{"minus shorthand", "snapshot -TiFlash",
			`query('(msg:snapshot) NOT (msg:TiFlash)')`},
		{"two exclusions", "peer -status -region",
			`query('(msg:peer) NOT (msg:status) NOT (msg:region)')`},
		// All-negative folds through De Morgan into one positive query under a
		// single SQL NOT, rather than a purely negative query that matches
		// nothing.
		{"all negative folds", "-TiFlash -snapshot",
			`COALESCE(msg NOT IN (SELECT msg FROM logs.k8s_logs WHERE msg IS NOT NULL AND query('(msg:TiFlash) OR (msg:snapshot)')), TRUE)`},

		// --- field predicates on plain columns ---
		{"field equals", "level:error", `lower(level) = lower('error')`},
		{"field plus term", "level:error snapshot",
			`(lower(level) = lower('error') AND query('msg:snapshot'))`},

		// --- the two silent-failure traps (§5.10) ---
		{"fuzzy maps to the option form", "snapshoot~1",
			`match(msg, 'snapshoot', 'fuzziness=1')`},
		{"bare tilde defaults to one edit", "snapshoot~",
			`match(msg, 'snapshoot', 'fuzziness=1')`},
		{"prefix wildcard maps to LIKE", "snapsh*",
			`lower(msg) LIKE lower('snapsh%')`},
		{"substring wildcard maps to LIKE", "*napsho*",
			`lower(msg) LIKE lower('%napsho%')`},

		// --- existence and ranges ---
		{"existence", "pod:*", `(pod IS NOT NULL AND pod <> '')`},
		{"timestamp range", "ts:>2026-08-18", `ts > '2026-08-18'`},

		// --- VARIANT fallback for unmodelled [k=v] keys ---
		{"variant field", "store_id:7", `lower(kv['store_id']::VARCHAR) = lower('7')`},
		{"variant range casts", "store_id:>7", `kv['store_id']::VARCHAR::DOUBLE > 7`},

		// --- grouping ---
		{"parens", "(peer OR status) AND level:error",
			`(query('(msg:peer) OR (msg:status)') AND lower(level) = lower('error'))`},
		// A field in front of a group scopes the whole group. Without this the
		// field and the group were both dropped and the query compiled to
		// `1=1` — a filter that silently matches everything.
		{"field-scoped group", "level:(error OR warn)",
			`(lower(level) = lower('error') OR lower(level) = lower('warn'))`},
		{"field-scoped group defaults to AND", "level:(error warn)",
			`(lower(level) = lower('error') AND lower(level) = lower('warn'))`},
		{"field-scoped group on the text column stays one call", "msg:(peer OR region)",
			`query('(msg:peer) OR (msg:region)')`},
		{"negated group negates the whole group", "-level:(error OR warn)",
			`COALESCE(NOT ((lower(level) = lower('error') OR lower(level) = lower('warn'))), TRUE)`},
		// An inner field wins over the outer one, which is how every Lucene
		// front end reads it.
		{"inner field wins", "level:(component:tikv error)",
			`(lower(component) = lower('tikv') AND lower(level) = lower('error'))`},

		// --- OR with a negated child, folded through De Morgan ---
		//
		// `(a) OR NOT (b)` drops the negative silently, so the shape cannot be
		// emitted as written — but it can be rewritten into the one that works.
		{"or with a negation", "peer OR -status",
			`COALESCE(msg NOT IN (SELECT msg FROM logs.k8s_logs WHERE msg IS NOT NULL AND query('(msg:status) NOT ((msg:peer))')), TRUE)`},
		{"or with two negations", "peer OR region OR -status",
			`COALESCE(msg NOT IN (SELECT msg FROM logs.k8s_logs WHERE msg IS NOT NULL AND query('(msg:status) NOT ((msg:peer) OR (msg:region))')), TRUE)`},

		// --- Lucene's required-term marker is consumed, not searched for ---
		{"plus marker", "+peer +status", `query('(msg:peer) AND (msg:status)')`},

		// --- a colon is not always a field separator ---
		{"url is not a field lookup", "http://a.com", `query('msg:"http://a.com"')`},
		{"localhost port is not a field lookup", "localhost:3000",
			`query('msg:"localhost:3000"')`},
		{"a timestamp keeps its own colons", "ts:>2026-08-18T22:30:00Z",
			`ts > '2026-08-18T22:30:00Z'`},
		{"escaped colon is literal", `foo\:bar`, `query('msg:"foo:bar"')`},
		{"escaped colon in a field name", `a\:b:x`,
			`lower(kv['a:b']::VARCHAR) = lower('x')`},

		// --- dotted paths into the VARIANT bag ---
		{"variant prefix is consumed", "kv.container:vector",
			`lower(kv['container']::VARCHAR) = lower('vector')`},
		// A dotted name is ambiguous once the bag is open-ended: `a.b` may be a
		// path, or a flat key containing a dot. The flat reading is tried
		// first and the path is the fallback, so neither becomes unreachable.
		{"dotted path tries both readings", "kv.a.b:x",
			`lower(COALESCE(kv['a.b'], kv['a']['b'])::VARCHAR) = lower('x')`},

		// --- bracket ranges are plain SQL, never query() ---
		{"inclusive range", "ts:[2026-08-18 TO 2026-08-19]",
			`ts BETWEEN '2026-08-18' AND '2026-08-19'`},
		{"exclusive range", "ts:{2026-08-18 TO 2026-08-19}",
			`(ts > '2026-08-18' AND ts < '2026-08-19')`},
		{"half-open range", "ts:[2026-08-18 TO *}", `ts >= '2026-08-18'`},
		{"numeric range casts through the VARIANT", "store_id:[1 TO 100]",
			`kv['store_id']::VARCHAR::DOUBLE BETWEEN 1 AND 100`},
		{"unbounded both ways is existence", "store_id:[* TO *]",
			`(kv['store_id']::VARCHAR IS NOT NULL AND kv['store_id']::VARCHAR <> '')`},

		// --- regex and boost ---
		{"regex compiles to RLIKE", "msg:/peer.*status/", `msg RLIKE 'peer.*status'`},
		{"a path is not a regex", "/var/log/pods", `query('msg:"/var/log/pods"')`},
		{"boost rides inside the query() text", "region^5 OR peer",
			`query('((msg:region)^5) OR (msg:peer)')`},

		// --- negation is null-safe ---
		//
		// `NOT (col = 'x')` is NULL, and therefore excluded, wherever col is
		// NULL, so `x` and `-x` would not add up to the table.
		{"negated field predicate coalesces", "-level:error",
			`COALESCE(NOT (lower(level) = lower('error')), TRUE)`},

		// --- hyphens are part of words, not negation ---
		{"hyphen inside a word", "pd-0", `query('msg:pd-0')`},
		{"field value with hyphens", "pod:tikv-0", `lower(pod) = lower('tikv-0')`},

		// --- injection safety ---
		{"quote in term", "it's", `query('msg:"it''s"')`},
		{"backslash in term", `snapshot\path`, `query('msg:"snapshot\\\\path"')`},
		// `C:` parses as a field name, so this lands in the VARIANT — Lucene
		// reads it the same way. Worth asserting so the behaviour is a
		// decision rather than a surprise.
		{"colon in the middle reads as a field", `C:\tmp`,
			`lower(kv['C']::VARCHAR) = lower('\\tmp')`},
		// Two layers of escaping stack here: LIKE's `\%` keeps the percent
		// literal, then the SQL literal doubles the backslash. The final
		// trailing `%` is the wildcard from the user's `*`.
		{"literal percent is not a wildcard", "100%*",
			`lower(msg) LIKE lower('100\\%%')`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileString(tc.query, s)
			if err != nil {
				t.Fatalf("CompileString(%q) error: %v", tc.query, err)
			}
			if got.SQL != tc.want {
				t.Errorf("CompileString(%q)\n got: %s\nwant: %s", tc.query, got.SQL, tc.want)
			}
		})
	}
}

// The empty search must not merely be correct — it must contain no match() at
// all, because the optimiser pushes match() into the index scan regardless of
// the surrounding boolean.
func TestEmptySearchEmitsNoMatch(t *testing.T) {
	for _, q := range []string{"", "  ", "\t"} {
		got, err := CompileString(q, K8sLogs())
		if err != nil {
			t.Fatalf("CompileString(%q) error: %v", q, err)
		}
		if strings.Contains(got.SQL, "match(") || strings.Contains(got.SQL, "query(") {
			t.Errorf("empty search %q produced a text predicate: %s", q, got.SQL)
		}
		if got.UsesMatch {
			t.Errorf("empty search %q reported UsesMatch", q)
		}
	}
}

// score() is rejected without a match(), so a relevance panel must return no
// rows on an empty box rather than every row.
func TestCompileScore(t *testing.T) {
	s := K8sLogs()

	// `1=0` is NOT sufficient: the binder looks for a search function anywhere
	// in the statement, and score() sits in the select list. Verified live —
	// `SELECT score() ... WHERE 1=0` still returns [1065]. So an empty search
	// must emit a search function that matches nothing.
	want := "match(msg, '" + ScoreSentinel + "')"

	empty, err := CompileScore("", s)
	if err != nil {
		t.Fatal(err)
	}
	if empty.SQL != want {
		t.Errorf("empty score predicate = %s, want %s", empty.SQL, want)
	}
	if !empty.UsesMatch {
		t.Error("empty score predicate must still count as using a search function")
	}

	// A structured-only query has no search function either, so it takes the
	// same path.
	structured, err := CompileScore("level:error", s)
	if err != nil {
		t.Fatal(err)
	}
	if structured.SQL != want {
		t.Errorf("structured-only score predicate = %s, want %s", structured.SQL, want)
	}

	withText, err := CompileScore("snapshot", s)
	if err != nil {
		t.Fatal(err)
	}
	if withText.SQL != `query('msg:snapshot')` {
		t.Errorf("text score predicate = %s", withText.SQL)
	}
}

func TestWarnings(t *testing.T) {
	s := K8sLogs()

	// The reference table carries an NGRAM index on msg, so a wildcard there is
	// index-backed and warning about a full scan would be false.
	r, err := CompileString("snapsh*", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("no warning expected while an NGRAM index is declared, got %v", r.Warnings)
	}

	// Undeclaring the index brings the warning back, but changes no SQL.
	sql := r.SQL
	s.Fields["msg"] = Field{Column: "msg", Kind: Text}
	r, err = CompileString("snapsh*", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) == 0 {
		t.Error("wildcard on a column with no NGRAM index should warn about the full scan")
	}
	if r.SQL != sql {
		t.Errorf("declaring an index must not change the SQL:\n got %s\nwant %s", r.SQL, sql)
	}
}

// Parse promises never to error and to read half-typed input generously. It
// did not: a dangling `NOT` walked the token index past the end of the slice
// and panicked on the next peek, which through the Grafana macro takes out
// every panel on the dashboard at once and, in any consumer without a recover,
// crashes the process.
func TestDanglingOperatorsDoNotPanic(t *testing.T) {
	for _, q := range []string{
		"NOT", "NOT ", "error NOT", "error not", "level:ERROR NOT",
		"peer AND NOT", "NOT NOT", "peer OR", "-", "(", "peer -",
	} {
		t.Run(q, func(t *testing.T) {
			if _, err := CompileString(q, K8sLogs()); err != nil {
				t.Errorf("CompileString(%q) should be read generously, got %v", q, err)
			}
		})
	}
}

// `NOT (query(x))` is not the complement of `query(x)`: the search function is
// pushed into the index scan whatever the surrounding boolean, so when x
// matches no row the scan is pruned and the NOT returns zero rows instead of
// every row. Measured over a 152,317-row window, three absent tokens each gave
// 0 through the bare NOT and the full window through the anti-join.
//
// This is the shape a user reaches for when excluding a noise pattern that
// happens not to occur in the selected time range, so it is not a corner case.
func TestNegationDoesNotUseABareNot(t *testing.T) {
	s := K8sLogs()
	for _, q := range []string{"-pdctl", "-a -b", "peer OR -status", "level:error -status"} {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("CompileString(%q): %v", q, err)
		}
		if strings.Contains(r.SQL, "NOT (query(") || strings.Contains(r.SQL, "NOT (match(") {
			t.Errorf("CompileString(%q) wraps a search function in a bare NOT: %s", q, r.SQL)
		}
		if !strings.Contains(r.SQL, "NOT IN (SELECT") {
			t.Errorf("CompileString(%q) should exclude with an anti-join: %s", q, r.SQL)
		}
	}

	// A negation that keeps a positive term beside it never needs the
	// anti-join: it stays inside one query() call, where the positive drives
	// the scan and an absent excluded term costs nothing.
	r, err := CompileString("snapshot -zzzznotpresentzzzz", s)
	if err != nil {
		t.Fatal(err)
	}
	if r.SQL != `query('(msg:snapshot) NOT (msg:zzzznotpresentzzzz)')` {
		t.Errorf("a positive term should keep the exclusion inside the search function: %s", r.SQL)
	}

	// The anti-join names the table, so a schema that does not know its table
	// has to refuse that one shape rather than emit the wrong answer.
	s.Table = ""
	if _, err := CompileString("-pdctl", s); err == nil {
		t.Error("expected an error: an anti-join needs the table name")
	}
	if _, err := CompileString("snapshot -pdctl", s); err != nil {
		t.Errorf("a negation beside a positive term needs no table name: %v", err)
	}
}

// The exclusion's search function lives in its own scan, so it neither counts
// against the one-per-table rule nor satisfies score(): both were measured.
func TestAntiJoinSearchFunctionIsNotTheOuterOne(t *testing.T) {
	s := K8sLogs()

	// Fuzziness is a search function in the outer scan. Before the exclusion
	// moved into a subquery this pair was rejected as two search functions;
	// it now compiles, and returns 17,608 rows live.
	r, err := CompileString("snapshoot~1 -tiflash", s)
	if err != nil {
		t.Fatalf("a fuzzy term and an exclusion sit in different scans: %v", err)
	}
	if !strings.Contains(r.SQL, "match(msg, 'snapshoot', 'fuzziness=1')") {
		t.Errorf("got %s", r.SQL)
	}

	// score() is bound against the outer scan only, so a purely negative
	// search still needs the sentinel.
	neg, err := CompileScore("-tiflash", s)
	if err != nil {
		t.Fatal(err)
	}
	if neg.SQL != "match(msg, '"+ScoreSentinel+"')" {
		t.Errorf("score() over a purely negative search must fall back to the sentinel: %s", neg.SQL)
	}
}

func TestErrors(t *testing.T) {
	s := K8sLogs()
	s.Variant = "" // close the VARIANT escape hatch

	if _, err := CompileString("nosuchfield:x", s); err == nil {
		t.Error("expected an error for an unknown field with no VARIANT column")
	}
	if _, err := CompileString("msg:>5", K8sLogs()); err == nil {
		t.Error("expected an error for a range on a full-text field")
	}
}

// Databend allows one search function per table. Anything needing two must fail
// at compile time with an explanation, because the alternative is SQL that dies
// at runtime with [1065] duplicate search function for table 0.
func TestOneSearchFunctionRule(t *testing.T) {
	s := K8sLogs()

	// Text terms in different branches of a mixed boolean cannot be merged
	// into one query() call.
	if _, err := CompileString("(peer level:error) OR (status level:warn)", s); err == nil {
		t.Error("expected an error: this needs two query() calls")
	}

	// Fuzziness exists only as an option to match(), so a fuzzy term is a
	// search function of its own and cannot share a statement with another
	// full-text term.
	if _, err := CompileString("snapshoot~1 peer", s); err == nil {
		t.Error("expected an error: fuzzy term plus a second full-text term")
	}

	// But a fuzzy term composes freely with structured filters and LIKE,
	// neither of which is a search function. Both verified live.
	for _, q := range []string{"snapshoot~1 level:error", "snapshoot~1 region*"} {
		if _, err := CompileString(q, s); err != nil {
			t.Errorf("CompileString(%q) should compile: %v", q, err)
		}
	}
}

// `(a) OR NOT (b)` drops the negative clause silently, so the shape cannot be
// emitted as written. De Morgan turns it into the one shape the engine gets
// right — positives ANDed, negatives bare and trailing — under a single SQL
// NOT, still inside one search function.
//
// Verified live: this predicate returns 440,181 rows, which is the 449,893-row
// table minus the 9,712 rows matching `status -peer`. And with an inner
// expression that matches nothing — `peer OR -zzzznosuchtoken`, where the
// exclusion is vacuously true — it returns all 449,893, which the bare-NOT
// form did not.
func TestOrWithNegationFoldsThroughDeMorgan(t *testing.T) {
	r, err := CompileString("peer OR -status", K8sLogs())
	if err != nil {
		t.Fatalf("`a OR -b` should compile, not refuse: %v", err)
	}
	want := `COALESCE(msg NOT IN (SELECT msg FROM logs.k8s_logs WHERE msg IS NOT NULL AND query('(msg:status) NOT ((msg:peer))')), TRUE)`
	if r.SQL != want {
		t.Errorf("got  %s\nwant %s", r.SQL, want)
	}
	if strings.Count(r.SQL, "query(")+strings.Count(r.SQL, "match(") != 1 {
		t.Errorf("the fold must stay inside one search function: %s", r.SQL)
	}

	// A folded fragment is still a text fragment, so it composes further
	// rather than being spent as SQL on the spot. Nesting a NOT inside a
	// negative group is correct here — measured, `(region) NOT ((peer) NOT
	// ((store)))` returns 15,634 = 20,144 - 4,853 + 343.
	nested, err := CompileString("(peer OR -status) region", K8sLogs())
	if err != nil {
		t.Fatalf("a folded fragment should still compose: %v", err)
	}
	if strings.Count(nested.SQL, "query(") != 1 {
		t.Errorf("composition must not spend a second search function: %s", nested.SQL)
	}
}

// Phrase proximity is real on this engine and N is honoured, so the marker is
// forwarded rather than dropped or refused. Measured, frozen:
//
//	"region peer"    654    "region peer"~2  4,593    "region peer"~10 4,853
//	"region peer"~1  654    "region peer"~3  4,593    region AND peer  4,853
//
// Strictly monotone, converging on the unordered AND. Sampling only the exact
// phrase and a large N misses this, because both sit on plateaus.
func TestPhraseSlop(t *testing.T) {
	s := K8sLogs()
	cases := map[string]string{
		`"peer status"~3`:      `query('msg:"peer status"~3')`,
		`"peer status"~`:       `query('msg:"peer status"~1')`,
		`msg:"peer status"~10`: `query('msg:"peer status"~10')`,
		// `~0` asks for exactly the ordering a plain phrase already has, and
		// the two were measured to return the same rows, so it is left off.
		`"peer status"~0`: `query('msg:"peer status"')`,
		// The markers arrive as one word and have to be split, or `~3` becomes
		// a search for the literal token `~3` and matches nothing.
		`"peer status"~3^2`: `query('(msg:"peer status"~3)^2')`,
		// Slop composes, and stays inside the single search function.
		`"peer status"~3 store`: `query('(msg:"peer status"~3) AND (msg:store)')`,
	}
	for q, want := range cases {
		got, err := CompileString(q, s)
		if err != nil {
			t.Errorf("CompileString(%q): %v", q, err)
			continue
		}
		if got.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, got.SQL, want)
		}
	}

	// A plain column has no positions to measure against, so the marker is
	// reported rather than silently honoured.
	r, err := CompileString(`pod:"a b"~2`, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) == 0 {
		t.Error("slop on a plain column should warn")
	}
}

// Markers are only markers at the end of a word, and only when a value
// precedes them.
func TestTrailingMarkers(t *testing.T) {
	for q, want := range map[string]string{
		"region^5":      `query('(msg:region)^5')`,
		"snapshoot^2~1": `match(msg, 'snapshoot', 'fuzziness=1')`,
		"foo^bar":       `query('msg:"foo^bar"')`,
		"peer^":         `query('msg:"peer^"')`,
		"~3":            `query('msg:"~3"')`,
		"1.5^2":         `query('(msg:1.5)^2')`,
	} {
		got, err := CompileString(q, K8sLogs())
		if err != nil {
			t.Errorf("CompileString(%q): %v", q, err)
			continue
		}
		if got.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, got.SQL, want)
		}
	}
}
