package databend

import (
	"strings"
	"testing"

	"github.com/choudharypankaj/lake-search/parser"
)

// The two spellings a reader takes off a rendered log line. Every `want` here was
// executed against the live table over the closed window
// [2026-08-20 16:00:00, 16:25:00) — 8,142 rows, 30 of them from
// compaction_runner.rs and 15 from line 360 of it — and the counts are in the
// case comments so a reader can check the compilation rather than trust it.
func TestSourceFilePosition(t *testing.T) {
	line := K8sLogsLine()
	plain := K8sLogs()

	cases := []struct {
		name   string
		schema Schema
		query  string
		want   string
	}{
		// The defect. Before this rule the position was read as the field
		// `compaction_runner.rs` with the value 360, took the VARIANT path and
		// asked for a bag key nothing writes — 0 rows, one warning, and the
		// warning arrives in Grafana as a SQL comment nobody sees.
		//
		// Quoted inside query(), because the colon would otherwise be read as a
		// field separator by the query language too. 15 rows.
		{"a file position", line, "compaction_runner.rs:360",
			`query('(line:"compaction_runner.rs:360") OR ` +
				`(source_file:"compaction_runner.rs:360")')`},
		// Unquoted: `.` and `_` are plain token characters and the analyzer splits
		// on them, so this is the token search that finds every line of the file.
		// 30 rows, all from the source_file side — but see
		// TestSourceFileSearchesBothSurfaces for the terms where it is the other
		// way round.
		{"a bare file name", line, "compaction_runner.rs",
			`query('(line:compaction_runner.rs) OR (source_file:compaction_runner.rs)')`},
		// A generated Go file has two dots and is still one file name.
		{"a name with two dots", line, "descriptor.pb.go:41",
			`query('(line:"descriptor.pb.go:41") OR (source_file:"descriptor.pb.go:41")')`},

		// It composes inside the statement's ONE search function, which is the
		// whole reason source_file is in the same inverted index group as msg,
		// line and kv rather than beside it in an index of its own. Measured, 15.
		{"beside a text term", line, "compaction_runner.rs:360 candidates",
			`query('((line:"compaction_runner.rs:360") OR ` +
				`(source_file:"compaction_runner.rs:360")) AND (line:candidates)')`},
		{"beside a column filter", line, "compaction_runner.rs:360 level:INFO",
			`(query('(line:"compaction_runner.rs:360") OR ` +
				`(source_file:"compaction_runner.rs:360")') AND lower(level) = lower('INFO'))`},
		// Negation keeps the position inside the same call. Measured 15: the 30
		// rows saying "compaction candidates" minus the 15 at line 360.
		{"excluded beside a term", line, "candidates -compaction_runner.rs:360",
			`query('(line:candidates) NOT ((line:"compaction_runner.rs:360") OR ` +
				`(source_file:"compaction_runner.rs:360"))')`},
		// Excluded alone it is an anti-join, exactly as any other lone exclusion
		// is: 8,142 - 15 = 8,127, measured.
		{"excluded alone", line, "-compaction_runner.rs:360",
			`_row_id NOT IN (SELECT _row_id FROM logs.k8s_logs ` +
				`WHERE query('(line:"compaction_runner.rs:360") OR ` +
				`(source_file:"compaction_runner.rs:360")'))`},
		// ORed with a column filter the whole disjunct moves into a row-key
		// subquery, because a search function left in a disjunction prunes the
		// scan and takes the other branch with it. Nothing about that is specific
		// to this rule; the point of the case is that the expansion reaches it.
		{"ORed with a column filter", line, "compaction_runner.rs:360 OR level:ERROR",
			`(_row_id IN (SELECT _row_id FROM logs.k8s_logs ` +
				`WHERE query('(line:"compaction_runner.rs:360") OR ` +
				`(source_file:"compaction_runner.rs:360")')) ` +
				`OR lower(level) = lower('ERROR'))`},

		// A wildcard in the line number asks for a run of lines. It cannot be one
		// token — the value carries `.` and `:` — so both halves keep the
		// substring reading, which is what the existing wildcard rule already
		// does for a pattern that spans tokens. Measured 15, the same as the
		// exact position, because 360 is the only 36x line in the window.
		{"a wildcard line number", line, "compaction_runner.rs:36*",
			`(lower(line) LIKE lower('%compaction\\_runner.rs:36%') OR ` +
				`lower(source_file) LIKE lower('%compaction\\_runner.rs:36%'))`},
		// Boost rides along inside the one call, as it does on any text term.
		// Measured 15, and the rows are unchanged — a boost only reorders score().
		{"boost", line, "compaction_runner.rs:360^2",
			`query('((line:"compaction_runner.rs:360")^2) OR ` +
				`((source_file:"compaction_runner.rs:360")^2)')`},
		// Fuzziness is dropped, and only on the spelling that carries a colon:
		// match()'s query text is parsed as `field:value`, so the position form is
		// [1903] Field does not exist while the name form is a working match().
		// See dropFuzzOnPosition.
		{"fuzziness on a position is dropped", line, "compaction_runner.rs:360~2",
			`query('(line:"compaction_runner.rs:360") OR ` +
				`(source_file:"compaction_runner.rs:360")')`},
		// A fuzzy NAME keeps its ~N, and a fuzzy term is a search function in its
		// own right — so the two halves need a scan each, which is what the
		// row-key subqueries are. Measured 30.
		{"fuzziness on a name is kept", line, "compaction_runner.rs~1",
			`(_row_id IN (SELECT _row_id FROM logs.k8s_logs ` +
				`WHERE match(line, 'compaction_runner.rs', 'fuzziness=1')) ` +
				`OR _row_id IN (SELECT _row_id FROM logs.k8s_logs ` +
				`WHERE match(source_file, 'compaction_runner.rs', 'fuzziness=1')))`},

		// The same two spellings against a schema whose file column is a plain
		// VARCHAR outside the index. An equality is the wrong reading of both,
		// because the column holds the whole call site: measured, these return the
		// same 15 and 30 as the indexed forms above.
		{"a position on a plain column", plain, "compaction_runner.rs:360",
			`(_row_id IN (SELECT _row_id FROM logs.k8s_logs ` +
				`WHERE query('msg:"compaction_runner.rs:360"')) ` +
				`OR lower(source_file) LIKE lower('%compaction\\_runner.rs:360%'))`},
		{"a name on a plain column", plain, "compaction_runner.rs",
			`(_row_id IN (SELECT _row_id FROM logs.k8s_logs ` +
				`WHERE query('msg:compaction_runner.rs')) ` +
				`OR lower(source_file) LIKE lower('%compaction\\_runner.rs%'))`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileString(tc.query, tc.schema)
			if err != nil {
				t.Fatalf("CompileString(%q): %v", tc.query, err)
			}
			if got.SQL != tc.want {
				t.Errorf("CompileString(%q)\n got: %s\nwant: %s", tc.query, got.SQL, tc.want)
			}
			// Searching a column the user did not name is exactly the class of
			// thing this repo warns about, so every expansion says so.
			if !hasWarning(got.Warnings, "reads as a source file") {
				t.Errorf("CompileString(%q) expanded silently: %v", tc.query, got.Warnings)
			}
			// One search function per scan is what the engine enforces, and the
			// expansion must not be the thing that breaks it.
			if n := maxScanSearchFuncs(got.SQL); n > 1 {
				t.Errorf("CompileString(%q) needs %d search functions in one scan: %s",
					tc.query, n, got.SQL)
			}
		})
	}
}

// Both surfaces have to be searched, because a file position is in the column on
// some rows and in the text on others — and which one depends on the log format
// rather than on anything a user can see. Measured over the closed window
// [2026-08-20 16:00:00, 16:25:00), 8,142 rows:
//
//	term                    line:…   source_file:…   union
//	factory.go                  69               0      69
//	warnings.go:110             32               0      32
//	reflector.go                 0              69      69
//	compaction_runner.rs         0              30      30
//
// So a redirect onto the source-file column alone would have answered 0 of 69 for
// `factory.go` and 0 of 32 for `warnings.go:110`. This test is the guard against
// the expansion quietly becoming a redirect again: both field names must appear,
// on both spellings.
func TestSourceFileSearchesBothSurfaces(t *testing.T) {
	s := K8sLogsLine()
	for _, q := range []string{"factory.go", "warnings.go:110", "compaction_runner.rs"} {
		got, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("CompileString(%q): %v", q, err)
		}
		for _, field := range []string{"line:", "source_file:"} {
			if !strings.Contains(got.SQL, field) {
				t.Errorf("CompileString(%q) does not search %s: %s", q, field, got.SQL)
			}
		}
	}
}

// The shape rule decides whether a term is expanded at all, so its negatives
// matter more than its positives: a term that is not a file has no business
// reaching a column that holds one.
func TestSourceFileShapeRule(t *testing.T) {
	names := []struct {
		in   string
		want bool
		why  string
	}{
		{"compaction_runner.rs", true, "the reference case"},
		{"endpoint.rs", true, ""},
		{"ddl_worker.go", true, ""},
		{"descriptor.pb.go", true, "a generated Go file has two dots"},
		{"Main.java", true, "capitals are ordinary in file names"},
		{"peer-store.go", true, "a hyphen is a file name character"},
		{"MOD.RS", true, "the extension is folded"},

		{"db_impl_compaction_flush.cc", true, "an extension this cluster has never emitted"},
		{"handler.py", true, "same, and the rule must not need a code change for it"},
		{"main.c", true, "a one-letter extension is still an extension"},

		{"10.0.0.1", false, "an address: the last segment is digits"},
		{"192.168.176.28", false, "an address that occurs in this table's node names"},
		{"v8.5.7", false, "a version: the last segment is digits"},
		{"compaction_runner.rs360", false, "the extension must be letters only"},
		{"store/peer.rs", false, "a path: `/` is not a file name character"},

		// These three are NOT files and the rule fires on them anyway. That is the
		// price of a structural extension test, and it is affordable only because
		// the term is expanded rather than redirected: the extra disjunct is on a
		// column that holds no such token, so it adds nothing and removes nothing.
		{"foo.bar", true, "not a file; harmless because the rule expands, never redirects"},
		{"example.com", true, "same"},
		{"k8s_logs.ts", true, "same — measured, a column reference in a logged statement"},
		{"peer.rs:360", false, "a position, not a name — isFilePosition's job"},
		{".rs", false, "no name"},
		{"peer.", false, "no extension"},
		{"peer", false, "no dot"},
		{"", false, ""},
		{"peer..rs", false, "an empty segment"},
		{"peer*.rs", false, "a wildcard is not a file name character"},
		{"kv.compaction_runner.rs", true, "shape only; the bag guard is a separate test"},
	}
	for _, tc := range names {
		if got := isSourceFileName(tc.in); got != tc.want {
			t.Errorf("isSourceFileName(%q) = %t, want %t — %s", tc.in, got, tc.want, tc.why)
		}
	}

	lines := []struct {
		in   string
		want bool
	}{
		{"360", true}, {"1", true}, {"36*", true}, {"3?0", true}, {"*360", true},
		{"", false}, {"abc", false}, {"*", false}, {"36a", false}, {"3.6", false},
		{"-1", false}, {"36 0", false},
	}
	for _, tc := range lines {
		if got := isLineNumber(tc.in); got != tc.want {
			t.Errorf("isLineNumber(%q) = %t, want %t", tc.in, got, tc.want)
		}
	}

	positions := []struct {
		in   string
		want bool
	}{
		{"peer.rs:360", true}, {"descriptor.pb.go:41", true},
		{"peer.rs", false}, {"peer.rs:", false}, {"peer.rs:abc", false},
		{"10.0.0.1:2379", false}, {"localhost:3000", false},
		{"http://a.com/main.go", false}, {"2026-08-20T16:20:59", false},
	}
	for _, tc := range positions {
		if got := isFilePosition(tc.in); got != tc.want {
			t.Errorf("isFilePosition(%q) = %t, want %t", tc.in, got, tc.want)
		}
	}
}

// The rule must never take a lookup that means something else. Each of these
// compiles today, and the shape rule is only allowed to fire where a `field:value`
// reading is WRONG.
func TestSourceFileRuleDoesNotStealRealLookups(t *testing.T) {
	s := K8sLogsLine()
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"a declared column", "level:ERROR", `lower(level) = lower('ERROR')`},
		{"a declared text column", "line:compaction_runner.rs",
			`query('line:compaction_runner.rs')`},
		{"the role's own field", "source_file:compaction_runner",
			`query('source_file:compaction_runner')`},
		{"the role's alias", "file:compaction_runner",
			`query('source_file:compaction_runner')`},
		// A bag key whose value is numeric is the ORDINARY case on a log table,
		// which is why "the field is not a column" cannot be the rule.
		{"a bag key", "tableID:123",
			`(query('kv.tableID:123') AND lower(kv['tableID']::VARCHAR) = lower('123'))`},
		{"a dotted bag key", "kv.format:json",
			`(query('kv.format:json') AND lower(kv['format']::VARCHAR) = lower('json'))`},
		// The bag addressed explicitly, with a tail that is exactly the shape the
		// rule looks for. The user said `kv.`, so it means the bag.
		// The bag addressed explicitly, with a tail that is exactly the shape the
		// rule looks for. The user said `kv.`, so it means the bag — and the same
		// precedence is what leaves `kv.go:350` unreachable by its own spelling,
		// which the header comment states as a cost rather than hiding.
		{"the bag prefix wins over the shape", "kv.compaction_runner.rs:360",
			`(query('kv.compaction_runner.rs:360') AND ` +
				`lower(COALESCE(kv['compaction_runner.rs'], ` +
				`kv['compaction_runner']['rs'])::VARCHAR) = lower('360'))`},
		{"a real file position under the bag prefix stays the bag", "kv.go:350",
			`(query('kv.go:350') AND lower(kv['go']::VARCHAR) = lower('350'))`},
		{"a real bag key under the bag prefix stays the bag", "kv.tableID:811",
			`(query('kv.tableID:811') AND lower(kv['tableID']::VARCHAR) = lower('811'))`},
		{"a file name with a non-numeric value", "compaction_runner.rs:abc",
			`(query('kv.compaction_runner.rs:abc') AND ` +
				`lower(COALESCE(kv['compaction_runner.rs'], ` +
				`kv['compaction_runner']['rs'])::VARCHAR) = lower('abc'))`},
		// Quotes narrow the search to the text surface, which is what makes them
		// the escape hatch: a phrase is the user asking for these characters in
		// the message and nowhere else.
		{"a phrase stays literal", `"compaction_runner.rs:360"`,
			`query('line:"compaction_runner.rs:360"')`},
		{"a phrase name stays literal", `"compaction_runner.rs"`,
			`query('line:"compaction_runner.rs"')`},
		{"a regex stays a regex", "/compaction_runner.rs:360/",
			`line RLIKE 'compaction_runner.rs:360'`},
		// An existence test asks about a key, not about a value, so expanding it
		// would change the question rather than the surface. Left alone.
		{"an existence test is not expanded", "compaction_runner.rs:*",
			`COALESCE(kv['compaction_runner.rs'], kv['compaction_runner']['rs']) IS NOT NULL`},
		// Ordinary dotted text on the default surface.
		{"an address", "10.0.0.1", `query('line:10.0.0.1')`},
		{"an address with a port", "10.0.0.1:8080", `query('line:"10.0.0.1:8080"')`},
		{"a node name", "192.168.176.28", `query('line:192.168.176.28')`},
		{"a version", "v8.5.7", `query('line:v8.5.7')`},
		// No index clause beside it: a path segment starting with a digit cannot be
		// spelled inside query(), so the bag equality stands alone. Pre-existing,
		// and asserted so the rule is visibly not the thing that changed it.
		{"a version with a digit value", "v8.5.7:1",
			`lower(COALESCE(kv['v8.5.7'], kv['v8']['5']['7'])::VARCHAR) = lower('1')`},
		{"a dotted name with a non-numeric value", "foo.bar:baz",
			`(query('kv.foo.bar:baz') AND ` +
				`lower(COALESCE(kv['foo.bar'], kv['foo']['bar'])::VARCHAR) = lower('baz'))`},
		{"a URL", "http://0.0.0.0:8686/playground",
			`query('line:"http://0.0.0.0:8686/playground"')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileString(tc.query, s)
			if err != nil {
				t.Fatalf("CompileString(%q): %v", tc.query, err)
			}
			if got.SQL != tc.want {
				t.Errorf("CompileString(%q)\n got: %s\nwant: %s", tc.query, got.SQL, tc.want)
			}
			if hasWarning(got.Warnings, "reads as a source file") {
				t.Errorf("CompileString(%q) was expanded: %v", tc.query, got.Warnings)
			}
			if strings.Contains(got.SQL, "source_file") &&
				!strings.Contains(tc.query, "source_file") && !strings.Contains(tc.query, "file:") {
				t.Errorf("CompileString(%q) reached source_file: %s", tc.query, got.SQL)
			}
		})
	}
}

// A declared name wins over the shape, whatever it is called. A deployment is
// allowed a column named like a source file, and a bag is allowed a key named
// like one.
func TestDeclaredNamesOutrankTheFileShape(t *testing.T) {
	d := Def{
		Table: "t", Default: "body", SourceFile: "caller", RowKey: "_row_id",
		Indexes: []IndexDef{
			{Name: "i", Kind: "inverted", Columns: []string{"body", "caller", "kv"}},
		},
		Bags: []BagDef{{Column: "kv", Keys: map[string]string{"worker.go": "number"}}},
		Fields: []FieldDef{
			{Name: "body", Kind: "text"},
			{Name: "caller", Kind: "text"},
			// A column spelled like a file. Unlikely, and the rule must still not
			// take it.
			{Name: "parser.go", Kind: "number"},
		},
	}
	s, _, err := d.Schema()
	if err != nil {
		t.Fatal(err)
	}

	if got := mustCompile(t, "parser.go:12", s); got != "parser.go = 12" {
		t.Errorf("a declared field must keep its lookup, got %s", got)
	}
	// A key the schema declares under exactly this spelling is a declaration of
	// intent, and a declaration outranks a shape.
	if got := mustCompile(t, "worker.go:12", s); !strings.Contains(got, "kv['worker.go']") {
		t.Errorf("a declared bag key must keep its lookup, got %s", got)
	}
	// An undeclared file name still expands, and onto the deployment's own column
	// name rather than a hardcoded one.
	want := `query('(body:"peer.rs:360") OR (caller:"peer.rs:360")')`
	if got := mustCompile(t, "peer.rs:360", s); got != want {
		t.Errorf("expansion should name the declared role\n got: %s\nwant: %s", got, want)
	}
}

// A schema whose default field IS the source-file field wants one term, not the
// same term twice — two identical halves would spell a disjunction the engine has
// to evaluate for nothing.
func TestSourceFileRoleOnTheDefaultField(t *testing.T) {
	d := Def{
		Table: "t", Default: "body", SourceFile: "body", RowKey: "_row_id",
		Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
		Fields:  []FieldDef{{Name: "body", Kind: "text"}},
	}
	s, _, err := d.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if got := mustCompile(t, "peer.rs:360", s); got != `query('body:"peer.rs:360"')` {
		t.Errorf("one field means one clause, got %s", got)
	}
}

// A deployment that declares no source-file role keeps today's behaviour exactly,
// which is the promise that makes the rule safe to add. These three strings are
// what the compiler emitted at a95e957, before the rule existed.
func TestNoSourceFileRoleIsTodaysBehaviour(t *testing.T) {
	s := K8sLogsLine()
	s.SourceFile = ""
	for q, want := range map[string]string{
		"compaction_runner.rs:360": `(query('kv.compaction_runner.rs:360') AND ` +
			`lower(COALESCE(kv['compaction_runner.rs'], ` +
			`kv['compaction_runner']['rs'])::VARCHAR) = lower('360'))`,
		"compaction_runner.rs": `query('line:compaction_runner.rs')`,
		"descriptor.pb.go:41": `(query('kv.descriptor.pb.go:41') AND ` +
			`lower(COALESCE(kv['descriptor.pb.go'], ` +
			`kv['descriptor']['pb']['go'])::VARCHAR) = lower('41'))`,
	} {
		got, err := CompileString(q, s)
		if err != nil {
			t.Fatalf("CompileString(%q): %v", q, err)
		}
		if got.SQL != want {
			t.Errorf("with no role, CompileString(%q)\n got: %s\nwant: %s", q, got.SQL, want)
		}
		if hasWarning(got.Warnings, "reads as a source file") {
			t.Errorf("with no role there is nothing to expand onto: %v", got.Warnings)
		}
	}
}

// The role has to name a field that can hold `file.rs:360`, and a descriptor is
// the right place to find that out.
func TestSourceFileRoleIsChecked(t *testing.T) {
	base := func(kind string) Def {
		d := Def{
			Table: "t", Default: "body", SourceFile: "caller", RowKey: "_row_id",
			Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body", "caller"}}},
			Fields: []FieldDef{
				{Name: "body", Kind: "text"},
				{Name: "caller", Kind: kind},
			},
		}
		if kind != "text" {
			// Only a text field needs the index, and a field outside the group
			// must not be indexed alongside one — so the index shrinks with it.
			d.Indexes = []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}}
		}
		return d
	}

	// A role naming a field nobody declared is the same mistake as a severity
	// role naming one, and it fails the same way.
	d := base("text")
	d.SourceFile = "nope"
	if _, _, err := d.Schema(); err == nil ||
		!strings.Contains(err.Error(), `source_file field "nope" is not declared`) {
		t.Errorf("an undeclared role must be refused, got %v", err)
	}

	// A number cannot hold a file position, so a schema claiming one is
	// describing a role it does not have.
	if _, _, err := base("number").Schema(); err == nil ||
		!strings.Contains(err.Error(), "a source file position is text") {
		t.Errorf("a numeric role must be refused, got %v", err)
	}

	// Both usable kinds load, and the SQL says which one it got: a token search
	// inside the one query() call, or a substring scan beside it.
	for kind, want := range map[string]string{
		"text": `query('(body:"peer.rs:360") OR (caller:"peer.rs:360")')`,
		"string": `(_row_id IN (SELECT _row_id FROM t WHERE query('body:"peer.rs:360"')) OR ` +
			`lower(caller) LIKE lower('%peer.rs:360%'))`,
	} {
		s, notes, err := base(kind).Schema()
		if err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
		if got := mustCompile(t, "peer.rs:360", s); got != want {
			t.Errorf("kind %q\n got: %s\nwant: %s", kind, got, want)
		}
		// Absence of a role is not noted; presence must not be either.
		for _, n := range notes {
			if strings.Contains(n, "source") {
				t.Errorf("kind %q: unexpected note %q", kind, n)
			}
		}
	}
}

// The expansion must not edit the tree it was given. Compile is public and a
// caller may compile the same parsed query twice — CompileScore and
// CompileScoreExpr each do — and a mutated node would make the second compile see
// a term nobody typed.
func TestSourceFileExpansionDoesNotMutateTheTree(t *testing.T) {
	s := K8sLogsLine()
	n := parser.Parse("compaction_runner.rs:360")

	first, err := Compile(n, s)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(n, s)
	if err != nil {
		t.Fatal(err)
	}
	if first.SQL != second.SQL {
		t.Errorf("compiling the same tree twice differed:\n1: %s\n2: %s", first.SQL, second.SQL)
	}
	if term, ok := n.(*parser.Term); !ok || term.Field != "compaction_runner.rs" || term.Value != "360" {
		t.Errorf("the parsed term was rewritten in place: %#v", n)
	}
}

// An extension this warehouse has never emitted must behave exactly like one it
// has, or the rule is a list of remembered ecosystems rather than a shape.
//
// The reason this matters more than it looks: source_file holds 972 distinct
// values over `ts < '2026-08-20 16:25:00'` and only two extensions occur in any of
// them, 745 `.go` and 223 `.rs`. So
// a rule keyed on `{go, rs}` passes every test this data can pose and fails
// silently the first time a C++ or Python component logs into the table. No data
// can catch that; only the rule's shape can.
func TestSourceFileExtensionIsStructural(t *testing.T) {
	s := K8sLogsLine()
	for _, tc := range []struct{ q, want string }{
		{"db_impl_compaction_flush.cc:1042",
			`query('(line:"db_impl_compaction_flush.cc:1042") OR ` +
				`(source_file:"db_impl_compaction_flush.cc:1042")')`},
		{"handler.py:88",
			`query('(line:"handler.py:88") OR (source_file:"handler.py:88")')`},
		{"Server.java:12",
			`query('(line:"Server.java:12") OR (source_file:"Server.java:12")')`},
		{"main.c:5", `query('(line:"main.c:5") OR (source_file:"main.c:5")')`},
	} {
		if got := mustCompile(t, tc.q, s); got != tc.want {
			t.Errorf("CompileString(%q)\n got: %s\nwant: %s", tc.q, got, tc.want)
		}
	}
}

// The expansion must be a strict superset of what the default surface answered
// before, which is what makes a loose shape rule affordable: a term the rule
// takes for a file and which is not one still finds everything it used to.
//
// Asserted structurally rather than by row count — the pre-fix clause has to
// appear verbatim inside the post-fix predicate — so it holds for every term
// rather than for the ones somebody thought to count.
func TestSourceFileExpansionCannotLoseRows(t *testing.T) {
	with := K8sLogsLine()
	without := K8sLogsLine()
	without.SourceFile = ""

	for _, q := range []string{
		"foo.bar", "example.com", "k8s_logs.ts", "us-west-2.compute.internal",
		"client.go", "factory.go", "compaction_runner.rs", "handler.py",
	} {
		base := mustCompile(t, q, without)
		got := mustCompile(t, q, with)
		inner := strings.TrimSuffix(strings.TrimPrefix(base, "query('"), "')")
		if inner == base {
			t.Fatalf("%q: baseline is not a single query() call: %s", q, base)
		}
		if !strings.Contains(got, inner) {
			t.Errorf("%q drops the pre-fix clause %q:\n got: %s", q, inner, got)
		}
	}
}
