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
		{"bare term", "TiFlash", `match(msg, 'TiFlash')`},
		{"two terms are ANDed", "peer status",
			`(match(msg, 'peer') AND match(msg, 'status'))`},
		{"explicit AND", "peer AND status",
			`(match(msg, 'peer') AND match(msg, 'status'))`},
		{"explicit OR", "peer OR status",
			`(match(msg, 'peer') OR match(msg, 'status'))`},

		// --- phrases are order-sensitive via query() ---
		{"phrase", `"peer status"`, `query('msg:"peer status"')`},

		// --- negation, both spellings ---
		{"NOT keyword", "NOT TiFlash", `NOT (match(msg, 'TiFlash'))`},
		{"minus shorthand", "snapshot -TiFlash",
			`(match(msg, 'snapshot') AND NOT (match(msg, 'TiFlash')))`},

		// --- field predicates on plain columns ---
		{"field equals", "level:error", `lower(level) = lower('error')`},
		{"field plus term", "level:error snapshot",
			`(lower(level) = lower('error') AND match(msg, 'snapshot'))`},

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
			`((match(msg, 'peer') OR match(msg, 'status')) AND lower(level) = lower('error'))`},

		// --- hyphens are part of words, not negation ---
		{"hyphen inside a word", "pd-0", `match(msg, 'pd-0')`},
		{"field value with hyphens", "pod:tikv-0", `lower(pod) = lower('tikv-0')`},

		// --- injection safety ---
		{"quote in term", "it's", `match(msg, 'it''s')`},
		{"backslash in term", `snapshot\path`, `match(msg, 'snapshot\\path')`},
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
	if withText.SQL != `match(msg, 'snapshot')` {
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
