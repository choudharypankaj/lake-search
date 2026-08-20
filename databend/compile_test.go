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
		{"NOT keyword", "NOT TiFlash", `_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('msg:TiFlash'))`},
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
			`_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('(msg:TiFlash) OR (msg:snapshot)'))`},

		// --- field predicates on plain columns ---
		{"field equals", "level:error", `lower(level) = lower('error')`},
		{"field plus term", "level:error snapshot",
			`(lower(level) = lower('error') AND query('msg:snapshot'))`},

		// --- the two silent-failure traps (§5.10) ---
		{"fuzzy maps to the option form", "snapshoot~1",
			`match(msg, 'snapshoot', 'fuzziness=1')`},
		{"bare tilde defaults to one edit", "snapshoot~",
			`match(msg, 'snapshoot', 'fuzziness=1')`},
		// `snapsh*` asks for a TOKEN beginning with snapsh. Anchoring the
		// pattern at character 0 of the whole message answers a different
		// question (1,019 rows against 17,595 for the bare token) and so does
		// opening both ends of a LIKE, which lets the star run across word
		// boundaries: as '%reg%on%', `reg*on` is exactly RLIKE 'reg.*on' —
		// 21,278 rows against 17,082 for the token reading, and 899 of them
		// contain no word matching reg…on at all.
		{"a token prefix is a token, not a substring", "snapsh*",
			`lower(msg) RLIKE '(^|[^a-z0-9])snapsh[a-z0-9]*([^a-z0-9]|$)'`},
		{"a token suffix is not the same pattern as a prefix", "*egion",
			`lower(msg) RLIKE '(^|[^a-z0-9])[a-z0-9]*egion([^a-z0-9]|$)'`},
		{"stars on both ends stay inside one token", "*napsho*",
			`lower(msg) RLIKE '(^|[^a-z0-9])[a-z0-9]*napsho[a-z0-9]*([^a-z0-9]|$)'`},
		{"a mid-token wildcard is a pattern, not a truncation", "reg*on",
			`lower(msg) RLIKE '(^|[^a-z0-9])reg[a-z0-9]*on([^a-z0-9]|$)'`},
		{"a question mark inside a token is one token character", "snapsh?t",
			`lower(msg) RLIKE '(^|[^a-z0-9])snapsh[a-z0-9]t([^a-z0-9]|$)'`},
		// A pattern carrying characters the tokenizer splits on cannot be one
		// token, so the token reading would be meaningless and the substring
		// reading is kept — with a warning that says which one was used.
		{"a pattern that spans tokens stays a substring", "*0.0.0.0:8686/playground*",
			`lower(msg) LIKE lower('%0.0.0.0:8686/playground%')`},
		{"a hyphenated pattern spans tokens too", "tikv-tikv-*",
			`lower(msg) LIKE lower('%tikv-tikv-%')`},
		// A plain column is a different question: `pod:tikv*` really does mean
		// "the value starts with tikv", so it stays anchored.
		{"a wildcard on a plain column stays anchored", "pod:tikv*",
			`lower(pod) LIKE lower('tikv%')`},
		{"a question mark is one character", "pod:a?b",
			`lower(pod) LIKE lower('a_b')`},
		// The ordering that makes the two distinguishable: escaping first,
		// then the wildcard translation.
		{"a literal underscore is not a single-character wildcard", "pod:a_b",
			`lower(pod) = lower('a_b')`},
		{"wildcard and literal underscore in one value", "pod:a?b_c",
			`lower(pod) LIKE lower('a_b\\_c')`},

		// --- existence and ranges ---
		{"existence", "pod:*", `(pod IS NOT NULL AND pod <> '')`},
		{"timestamp range", "ts:>2026-08-18", `ts > '2026-08-18'`},

		// --- VARIANT fallback for unmodelled [k=v] keys ---
		{"variant field", "store_id:7", `lower(kv['store_id']::VARCHAR) = lower('7')`},
		// TRY_CAST rather than a cast, and the difference is a query that
		// answers versus one that dies. Over logs.k8s_logs_v2 (967,912 rows,
		// ts < 2026-08-19 22:19:00), 1,243 of store_id's 40,516 rows hold a
		// `Some(25)`-style debug rendering: `kv['store_id']::VARCHAR::DOUBLE >
		// 100` is [1006] invalid float literal, where the TRY_CAST form
		// returns the 39,140 rows that are numbers. Where the cast survives
		// the two agree exactly — both 32,929 for kv['term'] > 40.
		{"variant range casts", "store_id:>7", `TRY_CAST(kv['store_id']::VARCHAR AS DOUBLE) > 7`},

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
			`_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('(msg:status) NOT ((msg:peer))'))`},
		{"or with two negations", "peer OR region OR -status",
			`_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('(msg:status) NOT ((msg:peer) OR (msg:region))'))`},

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
			`TRY_CAST(kv['store_id']::VARCHAR AS DOUBLE) BETWEEN 1 AND 100`},
		// A bag key present with an empty value still exists, so an existence
		// test on a VARIANT key asks about the key and not about the value.
		// `<> ''` on kv['rest'] denies 100,635 rows that do have the key.
		{"unbounded both ways is existence", "store_id:[* TO *]",
			`kv['store_id'] IS NOT NULL`},
		{"existence on a bag key ignores the empty value", "rest:*",
			`kv['rest'] IS NOT NULL`},
		// A real column keeps the other reading: an empty level is not a level.
		{"existence on a real column excludes the empty string", "level:*",
			`(level IS NOT NULL AND level <> '')`},

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
			`lower(msg) LIKE lower('%100\\%%')`},
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

// score() is rejected without a search function, so the score expression — not
// the predicate — is what has to become conditional. The predicate must survive
// intact whatever the search was.
func TestCompileScore(t *testing.T) {
	s := K8sLogs()

	for _, q := range []string{"", "level:error", "component:tikv", "snapsh*", "-tiflash"} {
		pred, err := CompileScore(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		plain, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		// The defect this replaces: CompileScore overwrote SQL with a
		// match-nothing sentinel, so the whole filter was discarded and the
		// panel came back empty for every structured-only search.
		if pred.SQL != plain.SQL {
			t.Errorf("CompileScore(%q) changed the predicate: %s, want %s", q, pred.SQL, plain.SQL)
		}

		expr, err := CompileScoreExpr(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if expr.SQL != "0" {
			t.Errorf("CompileScoreExpr(%q) = %s, want 0 — there is no search function to rank", q, expr.SQL)
		}
	}

	// With a full-text term the ranking is real and both halves say so.
	withText, err := CompileScore("snapshot", s)
	if err != nil {
		t.Fatal(err)
	}
	if withText.SQL != `query('msg:snapshot')` {
		t.Errorf("text score predicate = %s", withText.SQL)
	}
	expr, err := CompileScoreExpr("snapshot", s)
	if err != nil {
		t.Fatal(err)
	}
	if expr.SQL != "score()" {
		t.Errorf("score expression = %s, want score()", expr.SQL)
	}

	// A fuzzy term ranks, but every row scores exactly 1.0, so the ordering is
	// meaningless and the only honest response is to say so.
	fuzzy, err := CompileScoreExpr("snapshoot~1", s)
	if err != nil {
		t.Fatal(err)
	}
	if fuzzy.SQL != "score()" {
		t.Errorf("fuzzy score expression = %s", fuzzy.SQL)
	}
	if !hasWarning(fuzzy.Warnings, "constant 1.0") {
		t.Errorf("a fuzzy ranking must warn that it is constant: %v", fuzzy.Warnings)
	}
}

func TestWarningComment(t *testing.T) {
	if got := WarningComment(nil); got != "" {
		t.Errorf("no warnings must render no comment, got %q", got)
	}
	// A warning that closed the comment early would splice the rest of itself
	// into the statement.
	got := WarningComment([]string{"a */ DROP TABLE x --", "b"})
	if strings.Contains(got, "*/ DROP") {
		t.Errorf("comment terminator not neutralised: %s", got)
	}
	if !strings.HasPrefix(got, "/* lake-search: ") || !strings.HasSuffix(got, " */") {
		t.Errorf("malformed comment: %s", got)
	}

	// The opener is the other half, and it was open. On an engine that nests
	// block comments the inner `/*` swallows the first `*/`, leaving the outer
	// comment unclosed and eating the rest of the statement.
	got = WarningComment([]string{"a /* b"})
	if strings.Contains(got, "/* b") {
		t.Errorf("nested comment opener not neutralised: %s", got)
	}
	if strings.Count(got, "/*") != 1 || strings.Count(got, "*/") != 1 {
		t.Errorf("a rendered comment must open and close exactly once: %s", got)
	}
	if strings.ContainsAny(WarningComment([]string{"a\r\nb"}), "\r\n") {
		t.Error("a warning must render on one line")
	}

	// Both sequences are reachable from the search box, which is the point:
	// warning text quotes the user's value verbatim. `msg:"/*"` is a phrase
	// the analyzer keeps no tokens of, so the collapsed-phrase warning quotes
	// it straight back.
	for _, q := range []string{`msg:"/*"`, `msg:"*/"`, `msg:"a */ b /* c"`} {
		r, err := CompileString(q, K8sLogs())
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		c := WarningComment(r.Warnings)
		if c == "" {
			// Nothing to render. `a */ b /* c` keeps the tokens a, b and c, so
			// it compiles through query() and the user's text is never quoted
			// back into a warning — the sequences never reach a comment at all.
			continue
		}
		if strings.Count(c, "/*") != 1 || strings.Count(c, "*/") != 1 {
			t.Errorf("%q produced an escapable comment: %s", q, c)
		}
	}

	// Assert commentSafe's property directly as well, so the test does not
	// depend on which constructs happen to quote the user's value back today.
	// Any future warning that embeds hostile text is covered by this.
	//
	// The interleaved spellings are the ones that pin the ORDER of the two
	// replacements. `*/` is neutralised first and `/*` second, so a `/*`
	// created by the first pass is still caught: `*/*` becomes `* /*` after
	// pass one and `* / *` after pass two. Swap the passes and `*/*` renders
	// as `* /*` with a live comment opener in it. Each of these carries the
	// two sequences overlapping in a different arrangement.
	for _, w := range []string{
		`a */ b /* c`, `*/*`, `/*/`, `*//*`, `/**/`, `**//`, `*/ /*`,
	} {
		c := WarningComment([]string{w})
		if strings.Count(c, "/*") != 1 || strings.Count(c, "*/") != 1 {
			t.Errorf("hostile warning text %q escaped the comment: %s", w, c)
		}
		if inner := strings.TrimSuffix(strings.TrimPrefix(c, "/* lake-search: "), " */"); strings.Contains(inner, "*/") || strings.Contains(inner, "/*") {
			t.Errorf("hostile warning text %q left a live sequence in the body: %s", w, c)
		}
	}
}

// The VARIANT key-case warning states an asymmetry, and it used to state it
// with a number: "kv['tableid'] is 0 rows where kv['tableID'] is 1,365". The
// table only grows, so that constant was true at one bound on one afternoon
// and measured 4,403 two hours later — and the sentence's plain reading, how
// many rows carry the key at all, was a different quantity again. A warning
// that ships a decaying count is wrong by construction.
func TestVariantKeyWarningCarriesNoCount(t *testing.T) {
	s := K8sLogs()
	for _, q := range []string{"tableID:574", "tableID:>1", "tableID:[1 TO 9]"} {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if !hasWarning(r.Warnings, "matched EXACTLY, case included") {
			t.Errorf("%q must warn about the key case: %v", q, r.Warnings)
		}
		for _, w := range r.Warnings {
			if !strings.Contains(w, "VARIANT") {
				continue
			}
			if strings.ContainsAny(w, "0123456789") {
				t.Errorf("the VARIANT warning must carry no row count: %q", w)
			}
		}
	}
}

// hasWarning reports whether any warning contains the given fragment.
func hasWarning(warnings []string, fragment string) bool {
	for _, w := range warnings {
		if strings.Contains(w, fragment) {
			return true
		}
	}
	return false
}

func TestWarnings(t *testing.T) {
	s := K8sLogs()

	// A token wildcard compiles to a word-boundary RLIKE, which neither the
	// inverted nor the NGRAM index serves, so the full-scan warning is true
	// here whatever indexes are declared — and the semantic half must say
	// which reading was taken, because the two readings differ by 21% of the
	// window on `reg*on`.
	r, err := CompileString("snapsh*", s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(r.Warnings, "matched as ONE token") {
		t.Errorf("a token wildcard must say it is a token match, got %v", r.Warnings)
	}
	if !hasWarning(r.Warnings, "full scan") {
		t.Errorf("RLIKE is served by no index here and must say so, got %v", r.Warnings)
	}

	// A pattern that cannot be one token keeps the substring reading, and the
	// warning has to name *that* reading rather than the token one — the
	// complaint the old text drew was that it disclosed only the widening at
	// the left edge and never that the match can span the whole line.
	r, err = CompileString("tikv-tikv-*", s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(r.Warnings, "UNANCHORED SUBSTRING") {
		t.Errorf("a substring wildcard must say it is a substring, got %v", r.Warnings)
	}
	if !hasWarning(r.Warnings, "crosses word boundaries") {
		t.Errorf("the substring warning must name the over-match, got %v", r.Warnings)
	}
	// The reference table carries an NGRAM index, which does serve LIKE.
	if hasWarning(r.Warnings, "full scan") {
		t.Errorf("no full-scan warning expected on the LIKE path while NGRAM is declared, got %v", r.Warnings)
	}

	// Undeclaring the index brings the full-scan warning back, but changes no SQL.
	sql := r.SQL
	s.Fields["msg"] = Field{Column: "msg", Kind: Text}
	r, err = CompileString("tikv-tikv-*", s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(r.Warnings, "full scan") {
		t.Error("wildcard on a column with no NGRAM index should warn about the full scan")
	}
	if r.SQL != sql {
		t.Errorf("declaring an index must not change the SQL:\n got %s\nwant %s", r.SQL, sql)
	}
}

// A token wildcard and a substring wildcard are different questions, and the
// syntax has to be able to tell them apart. Emitted as LIKE with both ends
// open, `*region`, `region*` and `*region*` were all '%region%': three
// spellings, one answer, and no way to ask for a suffix. Measured over the
// frozen window the three readings are 16,886 / 20,147 / 20,158.
func TestWildcardReadingsAreDistinct(t *testing.T) {
	s := K8sLogs()
	seen := map[string]string{}
	for _, q := range []string{"*region", "region*", "*region*", "reg*on"} {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if prev, ok := seen[r.SQL]; ok {
			t.Errorf("%q and %q compile to the same predicate %s", prev, q, r.SQL)
		}
		seen[r.SQL] = q
	}

	// tokenWildcard is the guard that keeps tokenPattern from emitting live
	// regex metacharacters. If it ever admits one, `a.b*` would match "axb".
	for _, v := range []string{"reg*on", "snapsh?t", "abc", "A1*"} {
		if !tokenWildcard(v) {
			t.Errorf("%q is a token pattern", v)
		}
	}
	for _, v := range []string{"a.b*", "tikv-tikv-*", "100%*", "*0.0.0.0:8686/x*", "", "a b*"} {
		if tokenWildcard(v) {
			t.Errorf("%q is not a token pattern", v)
		}
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

	// score() is bound against the outer scan only — the binder does not see
	// through the anti-join subquery — so a purely negative search ranks 0
	// rather than erroring, and keeps its predicate.
	neg, err := CompileScoreExpr("-tiflash", s)
	if err != nil {
		t.Fatal(err)
	}
	if neg.SQL != "0" {
		t.Errorf("score() over a purely negative search must be a constant: %s", neg.SQL)
	}
}

func TestErrors(t *testing.T) {
	s := K8sLogs()
	// Close the escape hatch. Both spellings have to go: Bags is where a
	// resolved schema keeps its VARIANT columns, and Variant is the one-column
	// shorthand a hand-built Schema may still use.
	s.Bags, s.Variant = nil, ""

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

	// Text terms in different branches of a mixed boolean still cannot be
	// MERGED into one query() call — but they no longer need to be. Each branch
	// is hoisted into its own row-key subquery, which is a separate scan, so the
	// one-per-statement rule is satisfied rather than violated. This used to be
	// a compile error.
	//
	// Verified live over ts < '2026-08-20 03:30:00': the emitted form returns
	// 368,007, and 368,002 + 5 - 0 is the same number computed from the two
	// branches independently.
	withKey := K8sLogsLine()
	r, err := CompileString("(peer level:error) OR (status level:warn)", withKey)
	if err != nil {
		t.Fatalf("two branches each with a search function should now compile: %v", err)
	}
	if n := strings.Count(r.SQL, "SELECT _row_id"); n != 2 {
		t.Errorf("expected two row-key subqueries, got %d: %s", n, r.SQL)
	}

	// Without a row key there is nowhere to put them, so it is still refused.
	noKey := withKey
	noKey.RowKey = ""
	if _, err := CompileString("(peer level:error) OR (status level:warn)", noKey); err == nil {
		t.Error("with no row key this must still be refused")
	}

	// Fuzziness exists only as an option to match(), so a fuzzy term is a search
	// function of its own and cannot share a SCAN with another full-text term.
	// It can share a statement: the conjunction puts the second one in a row-key
	// subquery, which is a scan of its own.
	//
	// This used to be refused, and the refusal was hiding a runtime failure
	// rather than preventing one. Wrapping the same conjunction in a disjunction
	// or a negation moved it into a subquery where the outer count saw nothing,
	// and the engine returned [1065] — 9 of 40 shapes in a live census, in three
	// wrappers, all of them the same three conjunctions the AND path refused.
	// Separating the scans fixes both spellings at once. Verified live: the pair
	// answers 725 rows, identically for all three ways of spreading two search
	// functions over two scans, and [1065] with both bare.
	r, err = CompileString("snapshoot~1 peer", s)
	if err != nil {
		t.Fatalf("a fuzzy term beside a text term should compile into two scans: %v", err)
	}
	if n := maxScanSearchFuncs(r.SQL); n != 1 {
		t.Errorf("expected one search function per scan, got %d: %s", n, r.SQL)
	}
	if searchCalls(r.SQL) != 2 {
		t.Errorf("expected both search functions to survive: %s", r.SQL)
	}
	if len(r.Warnings) == 0 {
		t.Error("a second scan is a cost the caller should hear about")
	}
	// With no row key there is nowhere to put the second one, so it is refused —
	// which is what every schema without a row key kept doing.
	sNoKey := s
	sNoKey.RowKey = ""
	if _, err := CompileString("snapshoot~1 peer", sNoKey); err == nil {
		t.Error("with no row key, two search functions in one scan must be refused")
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
	want := `_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('(msg:status) NOT ((msg:peer))'))`
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

// The index on msg declares `filters = 'english_stop'`, and the filter runs
// over the query as well as the document: the word is deleted before the index
// is consulted, so the clause matches nothing and nothing is raised. All 33
// words were checked individually against the live index — every one returns 0
// through query() — while the two controls that are not on the list return real
// counts, `from` 93,645 and `replica` 1,743.
func TestStopWords(t *testing.T) {
	s := K8sLogs()
	const noPattern = `lower(msg) RLIKE '(^|[^a-z0-9])no([^a-z0-9]|$)'`

	cases := map[string]string{
		// 130,002 rows contain the word `to`; query('msg:to') returns 0.
		"to":   `lower(msg) RLIKE '(^|[^a-z0-9])to([^a-z0-9]|$)'`,
		"no":   noPattern,
		"NO":   noPattern, // the set is matched case-insensitively
		`"no"`: noPattern, // a one-word phrase is the token
		// An ordinary word must keep going through the index, or every query
		// containing one becomes a needless full scan.
		"from": `query('msg:from')`,
		// A pattern is not analyzed, so a stopword with a wildcard stays a
		// pattern — a token pattern, which is the same word-boundary device
		// with a token-character run on the end. As a substring it was 177,913
		// rows over the frozen window against 136,162 for the token reading.
		"to*": `lower(msg) RLIKE '(^|[^a-z0-9])to[a-z0-9]*([^a-z0-9]|$)'`,
	}
	for q, want := range cases {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, r.SQL, want)
		}
	}

	// The negative case needs no code of its own: once a stopword leaf is a
	// SQL fragment, compiler.not already null-safes it. Measured, `replica -no`
	// is 87 rows and `replica no` is 1,656, which add to the 1,743 of
	// `replica`; before the fix the negated clause evaporated and `replica -no`
	// returned all 1,743.
	r, err := CompileString("replica -no", s)
	if err != nil {
		t.Fatal(err)
	}
	want := `(query('msg:replica') AND COALESCE(NOT (` + noPattern + `), TRUE))`
	if r.SQL != want {
		t.Errorf("excluding a stopword\n got: %s\nwant: %s", r.SQL, want)
	}
	if !hasWarning(r.Warnings, "stopword") {
		t.Errorf("a stopword rewrite must say so: %v", r.Warnings)
	}

	// A column with no declared filter must not take the branch at all.
	plain := Schema{Default: "msg", Fields: map[string]Field{"msg": {Column: "msg", Kind: Text}}}
	r, err = CompileString("to", plain)
	if err != nil {
		t.Fatal(err)
	}
	if r.SQL != `query('msg:to')` {
		t.Errorf("no declared stopword filter means no rewrite: %s", r.SQL)
	}
}

// A value is a value whatever it spells. Lexed as an operator, `msg:not` had
// nowhere to go: parseFieldValue has no case for tokNot, the leaf vanished and
// the whole query compiled to match-everything — 449,893 rows against a true
// 22,850. It has to land with the stopword fix, because `and`, `or` and `not`
// are all on the stop list too and fixing either alone still returns a wrong
// answer.
func TestOperatorWordInValuePosition(t *testing.T) {
	s := K8sLogs()
	const wbNot = `lower(msg) RLIKE '(^|[^a-z0-9])not([^a-z0-9]|$)'`
	const wbAnd = `lower(msg) RLIKE '(^|[^a-z0-9])and([^a-z0-9]|$)'`
	const wbOr = `lower(msg) RLIKE '(^|[^a-z0-9])or([^a-z0-9]|$)'`

	cases := map[string]string{
		"msg:not":   wbNot,
		"msg:and":   wbAnd,
		"level:not": `lower(level) = lower('not')`,

		// --- the forms the colon rule cannot reach ---
		//
		// A field-scoped group is value position too, and a query that is
		// nothing but an operator word has no operator position at all. Every
		// one of these used to lose its only filter: `msg:(not)`, `msg:(and)`
		// and a bare `not` all compiled to 1=1 — 449,893 rows over the frozen
		// window against 22,850 — and `msg:(peer AND not)` dropped the second
		// clause, returning `peer`'s 109,950 against a true 297. Worst of all,
		// `msg:(not ready)` became an anti-join over `ready` and returned the
		// COMPLEMENT of what was asked, 447,573 rows against 1,855.
		//
		// The rule is context-sensitive, not case-driven: a boolean word is
		// an operator only where an operator of its arity is GRAMMATICAL —
		// infix `and`/`or`/`&&`/`||` need an operand on their left, prefix
		// NOT needs one on its right — and NOT alone additionally needs the
		// uppercase spelling, because it INVERTS the term it takes while the
		// others only join terms that keep their own meaning either way.
		"msg:(not)":          wbNot,
		"msg:(and)":          wbAnd,
		"msg:(NOT)":          wbNot,
		"msg:(AND)":          wbAnd,
		"not":                wbNot,
		"NOT":                wbNot,
		"and":                wbAnd,
		"AND":                wbAnd,
		"or":                 wbOr,
		"OR":                 wbOr,
		"msg:(peer AND not)": `(query('msg:peer') AND ` + wbNot + `)`,
		"msg:(peer AND NOT)": `(query('msg:peer') AND ` + wbNot + `)`,
		"msg:(not ready)":    `(` + wbNot + ` AND query('msg:ready'))`,
		"not ready":          `(` + wbNot + ` AND query('msg:ready'))`,

		// The operators themselves must keep working in operator position.
		"a NOT b": `(lower(msg) RLIKE '(^|[^a-z0-9])a([^a-z0-9]|$)' AND ` +
			`_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('msg:b')))`,
		"peer AND status":       `query('(msg:peer) AND (msg:status)')`,
		"peer OR status":        `query('(msg:peer) OR (msg:status)')`,
		"peer && status":        `query('(msg:peer) AND (msg:status)')`,
		"peer || status":        `query('(msg:peer) OR (msg:status)')`,
		"NOT tiflash":           `_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('msg:tiflash'))`,
		"level:(error OR warn)": `(lower(level) = lower('error') OR lower(level) = lower('warn'))`,
		// A trailing AND is someone mid-keystroke: drop it, keep the term.
		// A trailing NOT has no such reading — there is nothing to negate, so
		// the word is what was typed.
		"peer AND": `query('msg:peer')`,
		"peer OR":  `query('msg:peer')`,

		// --- the case half of the rule, both directions ---
		//
		// Lowercase `and` and `or` in infix position are operators. Making
		// them values instead is Lucene's documented rule and it is wrong
		// here: it turned `snapshot or peer` into a three-word conjunction
		// and `level:(error or warn)` into level='error' AND level='or' AND
		// level='warn', which is a structural contradiction on a
		// single-valued column and returns 0 rows forever.
		"peer or status":        `query('(msg:peer) OR (msg:status)')`,
		"peer and status":       `query('(msg:peer) AND (msg:status)')`,
		"peer Or status":        `query('(msg:peer) OR (msg:status)')`,
		"peer aNd status":       `query('(msg:peer) AND (msg:status)')`,
		"level:(error or warn)": `(lower(level) = lower('error') OR lower(level) = lower('warn'))`,
		"msg:(peer or status)":  `query('(msg:peer) OR (msg:status)')`,

		// Lowercase `not` in the SAME grammatical position is a value,
		// because a negation read off an ambiguous English word hands back
		// the complement of the table. The uppercase spelling still negates.
		"peer not status": `(query('(msg:peer) AND (msg:status)') AND ` + wbNot + `)`,
		"peer NOT status": `query('(msg:peer) NOT (msg:status)')`,
		"msg:(NOT ready)": `_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs WHERE query('msg:ready'))`,

		// A leading infix operator will never acquire a left operand, so it
		// is the word; a trailing one is mid-keystroke, so it is dropped.
		"or peer":       `(` + wbOr + ` AND query('msg:peer'))`,
		"and peer":      `(` + wbAnd + ` AND query('msg:peer'))`,
		"peer or":       `query('msg:peer')`,
		"peer and":      `query('msg:peer')`,
		"msg:(or peer)": `(` + wbOr + ` AND query('msg:peer'))`,

		// The symbols have no case, so only position can disqualify them.
		// (`peer && status` and `peer || status` are pinned above, in the
		// operator-position block.)
		"&& peer": `query('(msg:"&&") AND (msg:peer)')`,
		"|| peer": `query('(msg:"||") AND (msg:peer)')`,

		// Quotes are unconditional: neither position nor spelling applies.
		`"not" ready`: `(` + wbNot + ` AND query('msg:ready'))`,
		`"or" peer`:   `(` + wbOr + ` AND query('msg:peer'))`,
	}
	for q, want := range cases {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, r.SQL, want)
		}
		if r.SQL == MatchAll {
			t.Errorf("%q compiled to match-everything: the filter is gone, not wrong", q)
		}
	}

	// Silence was half the defect: the row count was wrong AND nothing said
	// the word had been read as anything other than what was typed.
	for _, q := range []string{"not", "msg:(and)", "msg:(peer AND not)"} {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if !hasWarning(r.Warnings, "not applied as a boolean operator") {
			t.Errorf("%q must say the word was searched for, not applied: %v", q, r.Warnings)
		}
	}
}

func TestPhraseThatLosesItsTokens(t *testing.T) {
	s := K8sLogs()
	cases := map[string]string{
		// One token survives: the token keeps the index (and its stemming),
		// the residual scan checks the adjacency the quotes asked for.
		`"not ready"`:   `(query('msg:ready') AND lower(msg) LIKE lower('%not ready%'))`,
		`"the leader"`:  `(query('msg:leader') AND lower(msg) LIKE lower('%the leader%'))`,
		`"[pd]"`:        `(query('msg:pd') AND lower(msg) LIKE lower('%[pd]%'))`,
		`"not ready"~2`: `(query('msg:ready') AND lower(msg) LIKE lower('%not ready%'))`,
		// No token survives: nothing for the index to match, so the scan is
		// the whole answer and no search function is spent.
		`"not the"`: `lower(msg) LIKE lower('%not the%')`,
		// Two or more surviving tokens and the phrase works as written.
		`"peer status"`:      `query('msg:"peer status"')`,
		`"fail to get peer"`: `query('msg:"fail to get peer"')`,
		// Not a phrase, so not this branch — one token is what a term IS.
		"peer": `query('msg:peer')`,

		// --- the branch must not fire on a phrase the analyzer leaves alone ---
		//
		// This is where the token-count test was wrong. `"snapshots"` keeps
		// its one token and lost nothing, so the index answers it correctly
		// and stems it: query('msg:"snapshots"') is 17,595 rows over the
		// frozen window where lower(msg) LIKE '%snapshots%' is 9, a 1,955x
		// under-match. `""` destroys nothing either, and as '%%' it returned
		// the whole table against a correct 0.
		`"snapshots"`: `query('msg:"snapshots"')`,
		`"regions"`:   `query('msg:"regions"')`,
		`"region"`:    `query('msg:"region"')`,
		`""`:          `query('msg:""')`,
	}
	for q, want := range cases {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, r.SQL, want)
		}
	}

	// A phrase the analyzer leaves intact must raise nothing about being cut
	// down — the old text said `"snapshots"` "loses all but 1 token" when it
	// lost none, which is a false statement of fact shipped to the reader.
	r, err := CompileString(`"snapshots"`, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "token") {
			t.Errorf("an intact phrase must not be described as losing tokens: %q", w)
		}
	}

	// A star inside quotes is not a wildcard and the silence was the defect:
	// the analyzer splits the phrase at the star, so `"peer stat*"` becomes the
	// phrase "peer stat" and returns 0 rows on two disjoint windows where
	// `peer status` returns 88,441 and 38,076. A `?` is punctuation the same
	// way and is harmless — query('msg:"peer status?"') is 88,441, identical to
	// the clean phrase — so it must not raise anything.
	r, err = CompileString(`"peer stat*"`, s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(r.Warnings, "inside quotes is not a wildcard") {
		t.Errorf("a star inside a phrase must be explained: %v", r.Warnings)
	}
	r, err = CompileString(`"peer status?"`, s)
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(r.Warnings, "inside quotes is not a wildcard") {
		t.Errorf("a question mark in a phrase is harmless punctuation: %v", r.Warnings)
	}

	// The collapsed one says which token survived and what is checking the rest.
	r, err = CompileString(`"not ready"`, s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(r.Warnings, `cut down to the single token "ready"`) {
		t.Errorf("a collapsed phrase must name the surviving token: %v", r.Warnings)
	}

	// The residual is sound under every boolean, which is what lets the token
	// ride along at all. Under AND it merges into the one query() call; under
	// OR and under a negation it would wrongly filter the other side, so the
	// leaf falls back to the scan alone — measured, the same rows either way.
	shapes := map[string]string{
		`"not ready" peer`:     `(query('(msg:ready) AND (msg:peer)') AND lower(msg) LIKE lower('%not ready%'))`,
		`"not ready" -tiflash`: `(query('(msg:ready) NOT (msg:tiflash)') AND lower(msg) LIKE lower('%not ready%'))`,
		// The disjunct is a row-key membership test, not a bare query(): under
		// OR the engine prunes the scan whenever the search matches nothing and
		// discards the other branch with it.
		`"not ready" OR peer`: `(lower(msg) LIKE lower('%not ready%') OR ` +
			`_row_id IN (SELECT _row_id FROM logs.k8s_logs WHERE query('msg:peer')))`,
		`-"not ready"`: `COALESCE(NOT (lower(msg) LIKE lower('%not ready%')), TRUE)`,
	}
	for q, want := range shapes {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, r.SQL, want)
		}
	}
}

// One instant is one value even though it contains a space, and a bound that is
// not a complete instant is refused rather than rounded off. Both halves have
// to land together: a half-consumed value would otherwise become a silently
// widened window instead of an error.
func TestTimestampBounds(t *testing.T) {
	s := K8sLogs()
	ok := map[string]string{
		"ts:>2026-08-18 22:30:00":           `ts > '2026-08-18 22:30:00'`,
		`ts:>"2026-08-18 22:30:00"`:         `ts > '2026-08-18 22:30:00'`,
		"ts:>=2026-08-18 22:30":             `ts >= '2026-08-18 22:30'`,
		"ts:>2026-08-18 22:30:00.123+05:30": `ts > '2026-08-18 22:30:00.123+05:30'`,
		"ts:>2026-08-18 22:30:00Z":          `ts > '2026-08-18 22:30:00Z'`,
		"ts:>2026-08-18T22:30:00Z":          `ts > '2026-08-18T22:30:00Z'`,
		"ts:>2026-08-18":                    `ts > '2026-08-18'`,
		// No comparison operator and no date in front of it, so the clock is
		// an ordinary term: both conditions that keep the glue narrow are
		// doing work.
		"error 22:30:00": `query('(msg:error) AND (msg:"22:30:00")')`,
	}
	for q, want := range ok {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, r.SQL, want)
		}
	}

	// `ts > '2026-08-18T22'` is accepted by the engine and read as 22:00:00 —
	// measured 129,009 rows against 70,681 for the 22:30 being typed. There is
	// no safe guess between "they meant 22:00" and "the lexer ate the rest".
	//
	// The space spellings matter more than the T ones, because the space form
	// is what the compiler's own error message recommends — and it was the one
	// the guard could not see. `ts:>2026-08-18 22` split at the space and
	// compiled to `(ts > '2026-08-18' AND query('msg:22'))`: 3,779 rows over
	// the frozen window against the 129,009 of the bound being typed, with the
	// statement's one search function spent on a fragment of a timestamp. Both
	// spellings now take the same route to the same error.
	for _, q := range []string{
		"ts:>2026-08-18T22", "ts:>2026-08-18T22:3", "ts:>2026-08",
		"ts:>2026-08-18 22", "ts:>2026-08-18 22:3", "ts:>2026-08-18 22:30:0",
		// A run that does not end at a word boundary is not a clock either,
		// and gluing it on is what turns it into a refusal rather than a
		// silently different query.
		"ts:>2026-08-18 22:30:00abc",
		"ts:>garbage", "ts:[2026-08-18T22 TO *]", "ts:[2026-08-18 22 TO 2026-08-19]",
	} {
		if _, err := CompileString(q, s); err == nil {
			t.Errorf("%q: a bound that is not a complete instant must be refused", q)
		}
	}

	// The glue must not reach past a clock-shaped run. A word after a date
	// bound is an ordinary term, and both terms have to survive.
	r, err := CompileString("ts:>2026-08-18 peer", s)
	if err != nil {
		t.Fatal(err)
	}
	if r.SQL != `(ts > '2026-08-18' AND query('msg:peer'))` {
		t.Errorf("a word after a date bound is a term, not a clock: %s", r.SQL)
	}
}

// TimeZone is off by default, because pinning the input while the engine still
// renders ts in the session zone trades one mismatch for another. When it is
// set, a bare date has to be expanded to midnight first: the engine parses
// '2026-08-18 00:00:00+00:00' and rejects '2026-08-18+00:00'.
func TestTimeZonePinning(t *testing.T) {
	s := K8sLogs()
	if r, _ := CompileString("ts:>2026-08-18", s); r.SQL != `ts > '2026-08-18'` {
		t.Errorf("the default must leave literals as typed: %s", r.SQL)
	}

	s.TimeZone = "+00:00"
	cases := map[string]string{
		"ts:>2026-08-18":                `ts > '2026-08-18 00:00:00+00:00'`,
		"ts:>2026-08-18 22:30:00":       `ts > '2026-08-18 22:30:00+00:00'`,
		"ts:>2026-08-18T22:30:00Z":      `ts > '2026-08-18T22:30:00Z'`, // already zoned, left alone
		"ts:>2026-08-18T22:30:00+05:30": `ts > '2026-08-18T22:30:00+05:30'`,
	}
	for q, want := range cases {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, r.SQL, want)
		}
	}
}

// A quoted token followed immediately by a colon is a field name. Nine keys in
// the reference table contain a space, and no syntax reached any of them:
// `"msg type":MsgRequestVote` became a phrase plus a search for the literal
// token `:MsgRequestVote`, 0 rows, filter gone.
func TestQuotedFieldName(t *testing.T) {
	s := K8sLogs()
	cases := map[string]string{
		`"msg type":MsgRequestVote`: `lower(kv['msg type']::VARCHAR) = lower('MsgRequestVote')`,
		`"msg type":*`:              `kv['msg type'] IS NOT NULL`,
		// A quoted token with no colon after it is still a phrase.
		`"peer status"`: `query('msg:"peer status"')`,
		// A space before the colon means it is not a separator.
		`"peer status" component:tikv`: `(query('msg:"peer status"') AND lower(component) = lower('tikv'))`,
	}
	for q, want := range cases {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL != want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", q, r.SQL, want)
		}
	}
}

// A leaf that produces nothing must not truncate the conjunction around it.
// `level:> peer` compiled to 1=1 — the whole table, silently — because the
// half-typed bound took the real search term with it.
func TestEmptyLeafDoesNotTruncateTheConjunction(t *testing.T) {
	s := K8sLogs()
	for _, q := range []string{"level:> peer", "* peer", "peer level:>", "peer * status"} {
		r, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if r.SQL == MatchAll {
			t.Errorf("%q compiled to match-everything; the surviving terms were dropped", q)
		}
		if !strings.Contains(r.SQL, "msg:peer") {
			t.Errorf("%q lost its search term: %s", q, r.SQL)
		}
	}
}
