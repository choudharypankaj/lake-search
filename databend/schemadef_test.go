package databend

import (
	"strings"
	"testing"
)

// Every built-in preset must load. They are held as the on-disk JSON, so a
// preset that stops loading means the file format broke, not just the preset.
func TestPresetsLoad(t *testing.T) {
	names := PresetNames()
	if len(names) == 0 {
		t.Fatal("no presets")
	}
	for _, name := range names {
		s, notes, err := Preset(name)
		if err != nil {
			t.Fatalf("preset %q: %v", name, err)
		}
		if s.Default == "" || s.Table == "" {
			t.Errorf("preset %q: default=%q table=%q", name, s.Default, s.Table)
		}
		if len(notes) != 0 {
			t.Errorf("preset %q should be complete, got notes %v", name, notes)
		}
	}
	if _, _, err := Preset("nope"); err == nil {
		t.Error("expected an error for an unknown preset")
	}
}

// The per-field index flags must be derived from the index declarations, never
// stated twice. A descriptor that could claim `english_stop` on a column whose
// index has no such filter would route 33 ordinary words onto needless scans.
func TestIndexFlagsAreDerived(t *testing.T) {
	s := K8sLogs()
	msg := s.Fields["msg"]
	if msg.Index != "idx_msg" {
		t.Errorf("msg.Index = %q, want idx_msg", msg.Index)
	}
	if !msg.Ngram {
		t.Error("msg should be flagged Ngram: idx_msg_ng covers it")
	}
	if len(msg.StopWords) != 33 {
		t.Errorf("msg.StopWords = %d, want 33", len(msg.StopWords))
	}
	if !msg.IsStopWord("THE") {
		t.Error("stop set should be case-insensitive")
	}

	// An index with no english_stop filter must leave the set empty, or the
	// compiler routes ordinary words around an index that would have served
	// them.
	d := Def{
		Table: "t", Default: "body",
		Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
		Fields:  []FieldDef{{Name: "body", Kind: "text"}},
	}
	got, _, err := d.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Fields["body"].StopWords) != 0 {
		t.Error("no english_stop filter declared, so no stopwords should be assumed")
	}
	if got.Fields["body"].Ngram {
		t.Error("no ngram index declared, so LIKE must be reported as a scan")
	}
}

// A text field whose column no inverted index covers is the failure this layer
// exists to catch: the query-time symptom is [1065], or worse, SQL that runs
// against the wrong column.
func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		def  Def
		want string
	}{
		{
			"text field with no inverted index",
			Def{Table: "t", Default: "body", Fields: []FieldDef{{Name: "body", Kind: "text"}}},
			"no inverted index covers",
		},
		{
			"default field is not text",
			Def{Table: "t", Default: "body", Fields: []FieldDef{{Name: "body", Kind: "string"}}},
			"a bare term has to reach an inverted index",
		},
		{
			"default field is not declared",
			Def{Table: "t", Default: "nope",
				Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
				Fields:  []FieldDef{{Name: "body", Kind: "text"}}},
			"is not declared",
		},
		{
			"unknown kind",
			Def{Table: "t", Default: "body", Fields: []FieldDef{{Name: "body", Kind: "fulltext"}}},
			"valid kinds are",
		},
		{
			"unknown index kind",
			Def{Table: "t", Default: "body",
				Indexes: []IndexDef{{Name: "i", Kind: "bloom", Columns: []string{"body"}}},
				Fields:  []FieldDef{{Name: "body", Kind: "text"}}},
			"valid kinds are",
		},
		{
			// The engine refuses it too: [1601] INVERTED index for columns
			// (line) already exist.
			"two inverted indexes over one column",
			Def{Table: "t", Default: "body",
				Indexes: []IndexDef{
					{Name: "i1", Kind: "inverted", Columns: []string{"body"}},
					{Name: "i2", Kind: "inverted", Columns: []string{"body"}},
				},
				Fields: []FieldDef{{Name: "body", Kind: "text"}}},
			"overlapping inverted index column sets",
		},
		{
			// One query() call reaches only the columns of one index, so a
			// schema spread across two describes a table where an ordinary
			// query cannot run.
			"search surfaces in two index groups",
			Def{Table: "t", Default: "body",
				Indexes: []IndexDef{
					{Name: "i1", Kind: "inverted", Columns: []string{"body"}},
					{Name: "i2", Kind: "inverted", Columns: []string{"trace"}},
				},
				Fields: []FieldDef{{Name: "body", Kind: "text"}, {Name: "trace", Kind: "text"}}},
			"spread across 2 inverted indexes",
		},
		{
			"time field is not a timestamp",
			Def{Table: "t", Default: "body", Time: "body",
				Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
				Fields:  []FieldDef{{Name: "body", Kind: "text"}}},
			"not \"timestamp\"",
		},
		{
			"duplicate name via an alias",
			Def{Table: "t", Default: "body",
				Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
				Fields: []FieldDef{
					{Name: "body", Kind: "text", Aliases: []string{"msg"}},
					{Name: "msg", Kind: "string"},
				}},
			"declared twice",
		},
		{
			"a bag key cannot be full text",
			Def{Table: "t", Default: "body",
				Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
				Fields:  []FieldDef{{Name: "body", Kind: "text"}},
				Bags:    []BagDef{{Column: "kv", Keys: map[string]string{"k": "text"}}}},
			"which a bag key cannot be",
		},
		{
			"bad time zone",
			Def{Table: "t", Default: "body", TimeZone: "UTC",
				Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
				Fields:  []FieldDef{{Name: "body", Kind: "text"}}},
			"is not a `+HH:MM`",
		},
		{
			"display names an undeclared field",
			Def{Table: "t", Default: "body", Display: []string{"nope"},
				Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
				Fields:  []FieldDef{{Name: "body", Kind: "text"}}},
			"display field \"nope\" is not declared",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.def.Schema()
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A schema that omits an optional role is valid and must say so at load time.
// Both of these facts are invisible at query time: a bagless schema refuses
// `store_id:7`, and a severity-less one leaves a log panel unable to colour
// anything.
func TestNotesAreLoud(t *testing.T) {
	d := Def{
		Table: "t", Default: "body",
		Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
		Fields:  []FieldDef{{Name: "body", Kind: "text"}},
	}
	s, notes, err := d.Schema()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"no attribute bag", "no severity field", "no time field"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes should mention %q; got %v", want, notes)
		}
	}

	// And the degradation the note describes has to be real.
	if _, err := CompileString("store_id:7", s); err == nil {
		t.Error("a schema with no bag must refuse an undeclared field name")
	}
	if s.Severity != "" || s.Time != "" {
		t.Error("roles should be empty, not guessed")
	}
	// Display falls back to what the schema does have.
	if got := strings.Join(s.DisplayColumns(), ","); got != "body" {
		t.Errorf("DisplayColumns = %q, want body", got)
	}
}

// Bags resolve by prefix first and then by declaration order, and a typed key
// changes how a comparison is compiled rather than only how it is documented.
func TestBags(t *testing.T) {
	d := Def{
		Table: "t", Default: "body",
		Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body", "attrs"},
			Filters: []string{"english_stop", "english_stemmer"}}},
		Fields: []FieldDef{{Name: "body", Kind: "text"}},
		Bags: []BagDef{
			{Column: "res", Prefix: "resource"},
			{Column: "attrs", Keys: map[string]string{"latency_ms": "number"}},
		},
	}
	s, notes, err := d.Schema()
	if err != nil {
		t.Fatalf("%v (notes %v)", err, notes)
	}

	// The prefixed bag is reachable only by its prefix; everything else falls
	// to the catch-all.
	if got := mustCompile(t, "resource.pod:web-1", s); !strings.Contains(got, "res['pod']") {
		t.Errorf("prefixed name should read the res bag: %s", got)
	}
	if got := mustCompile(t, "pod:web-1", s); !strings.Contains(got, "attrs['pod']") {
		t.Errorf("unprefixed name should read the catch-all bag: %s", got)
	}

	// attrs is in the inverted index, so an equality is index-backed — and
	// still exact, because the equality rides along as a residual.
	got := mustCompile(t, "pod:web-1", s)
	if !strings.Contains(got, "query('attrs.pod:web-1')") || !strings.Contains(got, "= lower('web-1')") {
		t.Errorf("indexed bag equality should be query() AND the equality: %s", got)
	}
	// res is not in any index, so it stays a plain equality.
	if got := mustCompile(t, "resource.pod:web-1", s); strings.Contains(got, "query(") {
		t.Errorf("an unindexed bag must not reach for the index: %s", got)
	}
	// A stopword value is not reachable through the index at all — measured, a
	// row with kv={"verb":"the"} is returned by the equality and not by
	// query('kv.verb:the') — so it must skip it.
	if got := mustCompile(t, "verb:the", s); strings.Contains(got, "query(") {
		t.Errorf("a stopword bag value must skip the index: %s", got)
	}

	// A declared numeric key compares as a number.
	if got := mustCompile(t, "latency_ms:>30", s); got != "TRY_CAST(attrs['latency_ms']::VARCHAR AS DOUBLE) > 30" {
		t.Errorf("typed bag key: %s", got)
	}
	// An undeclared key resolves the same way at emission, which is why the
	// declaration is an override rather than a requirement.
	if got := mustCompile(t, "other_ms:>30", s); got != "TRY_CAST(attrs['other_ms']::VARCHAR AS DOUBLE) > 30" {
		t.Errorf("undeclared bag key: %s", got)
	}
}

// `kv.container` is the key `container` of column kv, not a key literally named
// `kv.container`, and every reading of the name has to agree about that. It did
// not once: the index path was built from the raw name and came out
// `kv.kv.container`, which the suite caught as 0 rows against 44,639.
func TestDottedBagNameIsNormalised(t *testing.T) {
	s := K8sLogsLine()
	plain := mustCompile(t, "container:vector", s)
	dotted := mustCompile(t, "kv.container:vector", s)
	if plain != dotted {
		t.Errorf("kv.container and container must compile identically:\n  %s\n  %s", plain, dotted)
	}
	if !strings.Contains(dotted, "query('kv.container:vector')") {
		t.Errorf("search path should be kv.container: %s", dotted)
	}
}

// The one-search-function rule has a column-group half: a single query() call
// reaches only the columns of one index. The load-time check refuses such a
// schema, so this exercises the compiler's own guard against a hand-built one.
func TestCrossIndexTextIsRefused(t *testing.T) {
	s := K8sLogs()
	s.Fields["trace"] = Field{Column: "trace", Kind: Text, Index: "idx_other"}
	if _, err := CompileString("msg:peer trace:abc", s); err == nil {
		t.Error("expected a refusal for text columns in two inverted indexes")
	} else if !strings.Contains(err.Error(), "different inverted indexes") {
		t.Errorf("unhelpful error: %v", err)
	}
	// Same index: composes freely. Measured, query('line:RemoteStopped AND
	// msg:rpc') returns 585 on a table indexed over both.
	if _, err := CompileString("msg:peer line:RemoteStopped", K8sLogsLine()); err != nil {
		t.Errorf("two columns of one index must compose: %v", err)
	}
}

// A role may be an expression rather than a bare column name, because a
// deployment's timestamp is not always a column you can name.
func TestRoleMayBeAnExpression(t *testing.T) {
	d := Def{
		Table: "t", Default: "body", Time: "ts", Severity: "sev",
		Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
		Fields: []FieldDef{
			{Name: "body", Kind: "text"},
			{Name: "ts", Column: "from_unixtime(ts_ms / 1000)", Kind: "timestamp"},
			{Name: "sev", Column: "upper(severity_text)", Kind: "string"},
		},
	}
	s, _, err := d.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.TimeColumn(); got != "from_unixtime(ts_ms / 1000)" {
		t.Errorf("TimeColumn = %q", got)
	}
	// An expression is aliased to the typed name in a select list, so a caller
	// gets a column it can address.
	if got := strings.Join(s.DisplayColumns(), ", "); got != "from_unixtime(ts_ms / 1000) AS ts, upper(severity_text) AS sev, body" {
		t.Errorf("DisplayColumns = %q", got)
	}
}

// The file path and the embedded preset must be the same code path.
func TestParseDefRejectsUnknownKeys(t *testing.T) {
	if _, err := ParseDef([]byte(`{"table":"t","dfault":"body"}`)); err == nil {
		t.Error("a misspelled key must be an error, not a silent default")
	}
}

func mustCompile(t *testing.T, q string, s Schema) string {
	t.Helper()
	r, err := CompileString(q, s)
	if err != nil {
		t.Fatalf("CompileString(%q): %v", q, err)
	}
	return r.SQL
}

// The shipped example must load, because it is what a deployment copies. It is
// deliberately nothing like the built-in table: a different name, an expression
// for the time role, an expression for severity, two bags one of which is
// prefixed, typed bag keys, and a numeric column. If the schema layer can serve
// it, "generic" is a property rather than a claim.
func TestExampleSchemaFileLoads(t *testing.T) {
	s, notes, err := LoadSchema("../testdata/schema-app-logs.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("the example should be complete, got %v", notes)
	}
	if s.Table != "app.request_log" || s.Default != "line" {
		t.Errorf("table=%q default=%q", s.Table, s.Default)
	}
	// to_timestamp, not from_unixtime: the latter does not exist on this engine
	// (v0.34.0), so the example descriptor would have named a function that
	// cannot run -- an example nobody can execute teaches the wrong thing.
	// to_timestamp reads the magnitude, so it takes the microseconds directly.
	if got := s.TimeColumn(); got != "to_timestamp(ts_micros)" {
		t.Errorf("TimeColumn = %q", got)
	}

	// Each shape the example exists to demonstrate, compiled.
	for q, want := range map[string]string{
		"timeout":                "query('line:timeout')",
		"msg:timeout":            "query('message:timeout')",
		"level:error":            "lower(upper(severity_text)) = lower('error')",
		"status:500":             "status = 500",
		"latency_ms:>250":        "TRY_CAST(attrs['latency_ms']::VARCHAR AS DOUBLE) > 250",
		"resource.k8s_pod:web-1": "lower(resource_attrs['k8s_pod']::VARCHAR) = lower('web-1')",
		"user_agent:curl":        "query('attrs.user_agent:curl')",
	} {
		got := mustCompile(t, q, s)
		if !strings.Contains(got, want) {
			t.Errorf("CompileString(%q) = %s, want it to contain %s", q, got, want)
		}
	}
}

// A bag value whose every token the analyzer deletes cannot be found through the
// index, so the index clause must not be added beside the equality that can find
// it. The guard used to test the whole value against the stop set, which caught
// `the` and let `to be` through — and the AND with an index clause that matches
// nothing returns nothing.
//
// Measured on a probe table indexed over (msg, kv), one row per value: the
// equality finds all five, query('kv.verb:<v>') finds only the last two, and the
// pair therefore returned 0 rows for `to be` and `a an` before this was fixed.
func TestStopwordBagValueSkipsIndex(t *testing.T) {
	s := K8sLogsLine()
	for _, tc := range []struct {
		value    string
		useIndex bool
		why      string
	}{
		{"the", false, "a single stopword"},
		{"to be", false, "every token is a stopword"},
		{"a an", false, "every token is a stopword"},
		{"THE", false, "the stop set is case-insensitive"},
		{"the end", true, "`end` survives the analyzer"},
		{"RemoteStopped", true, "an ordinary value"},
		{"---", false, "no token at all"},
		{"", false, "nothing at all"},
	} {
		got := mustCompile(t, "verb:"+quoteIfSpaced(tc.value), s)
		used := strings.Contains(got, "query(")
		if used != tc.useIndex {
			t.Errorf("verb:%q (%s) index=%v, want %v: %s", tc.value, tc.why, used, tc.useIndex, got)
		}
		// Whatever happens, the equality must survive — it is the clause that
		// actually answers the question.
		if !strings.Contains(got, "= lower('"+tc.value+"')") &&
			!strings.Contains(got, "= lower('"+strings.ToLower(tc.value)+"')") {
			t.Errorf("verb:%q lost its equality: %s", tc.value, got)
		}
	}
}

func quoteIfSpaced(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, " ") {
		return `"` + v + `"`
	}
	return v
}

// Two bags cannot share a prefix: resolution stops at the first match, so the
// second would be declared and unreachable.
func TestBagPrefixCollisions(t *testing.T) {
	base := func(bags ...BagDef) Def {
		return Def{Table: "t", Default: "body",
			Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
			Fields:  []FieldDef{{Name: "body", Kind: "text"}},
			Bags:    bags}
	}
	_, _, err := base(BagDef{Column: "b1", Prefix: "p"}, BagDef{Column: "b2", Prefix: "p"}).Schema()
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("duplicate prefix should be refused, got %v", err)
	}

	// A prefix equal to another bag's column is legal but surprising, so it is
	// a note rather than an error.
	_, notes, err := base(BagDef{Column: "b1", Prefix: "kvbag"}, BagDef{Column: "kvbag"}).Schema()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "never to kvbag") {
		t.Errorf("expected a note about the shadowed column, got %v", notes)
	}
}

// Every numeric conversion must warn, and a real numeric column must not.
//
// This test exists because the warning was documented before it was written.
// TRY_CAST was chosen over a cast so that one non-numeric value cannot kill a
// legitimate query, and the price of that choice is that such rows drop out
// silently — 30,559 rows across nine keys on the live table, including all
// 1,943 castable-as-nothing values of `duration`, where `duration:>100` returns
// 0 of 1,945. The warning is the whole mitigation, so its absence is the defect.
func TestNumericConversionWarns(t *testing.T) {
	k8s := K8sLogs()
	app, _, err := LoadSchema("../testdata/schema-app-logs.json")
	if err != nil {
		t.Fatal(err)
	}

	const want = "TRY_CAST"
	for _, tc := range []struct {
		name   string
		schema Schema
		query  string
		warns  bool
	}{
		{"plain string column, range", k8s, "component:>5", true},
		{"bag key, range", k8s, "store_id:>100", true},
		{"bag key, two-sided range", k8s, "store_id:[1 TO 100]", true},
		{"declared numeric bag key", app, "latency_ms:>250", true},
		{"declared numeric bag key, equality", app, "latency_ms:250", true},
		{"real numeric column, range", app, "status:>499", false},
		{"real numeric column, equality", app, "status:500", false},
		{"no numeric comparison at all", k8s, "component:tikv", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := CompileString(tc.query, tc.schema)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for _, w := range r.Warnings {
				if strings.Contains(w, want) {
					got = w
				}
			}
			if tc.warns && got == "" {
				t.Fatalf("no cast warning for %q; warnings were %v", tc.query, r.Warnings)
			}
			if !tc.warns {
				if got != "" {
					t.Fatalf("unexpected cast warning for %q: %s", tc.query, got)
				}
				return
			}
			// The wording carries the mitigation, so assert on the parts a
			// reader has to be able to act on rather than on the whole string.
			field := strings.SplitN(tc.query, ":", 2)[0]
			for _, must := range []string{
				`"` + field + `"`, // names the field
				"EXCLUDED",        // says what happens to the rows
				"count_if(",       // hands over the predicate that counts them
			} {
				if !strings.Contains(got, must) {
					t.Errorf("warning for %q does not mention %s: %s", tc.query, must, got)
				}
			}
		})
	}
}
