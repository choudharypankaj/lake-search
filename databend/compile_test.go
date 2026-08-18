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
		{"NOT keyword", "NOT TiFlash", `NOT (query('msg:TiFlash'))`},
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
			`NOT (query('(msg:TiFlash) OR (msg:snapshot)'))`},

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

	empty, err := CompileScore("", s)
	if err != nil {
		t.Fatal(err)
	}
	if empty.SQL != MatchNone {
		t.Errorf("empty score predicate = %s, want %s", empty.SQL, MatchNone)
	}

	// A structured-only query has no match() either, so it must also be
	// downgraded rather than producing invalid SQL.
	structured, err := CompileScore("level:error", s)
	if err != nil {
		t.Fatal(err)
	}
	if structured.SQL != MatchNone {
		t.Errorf("structured-only score predicate = %s, want %s", structured.SQL, MatchNone)
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

	r, err := CompileString("snapsh*", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) == 0 {
		t.Error("wildcard on a column with no NGRAM index should warn about the full scan")
	}

	// Declaring the index removes the warning but not the SQL.
	s.Fields["msg"] = Field{Column: "msg", Kind: Text, Ngram: true}
	r, err = CompileString("snapsh*", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("no warning expected once an NGRAM index is declared, got %v", r.Warnings)
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

// OR with a negated child is rejected: Databend drops the negative clause
// silently, so `a OR -b` would quietly mean `a`.
func TestNegationUnderOrRejected(t *testing.T) {
	if _, err := CompileString("peer OR -status", K8sLogs()); err == nil {
		t.Error("expected an error: a negated term under OR is silently dropped by the engine")
	}
}
