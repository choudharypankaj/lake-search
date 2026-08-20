package databend

import (
	"os"
	"strings"
	"testing"
)

func mustShape(t *testing.T, path, table string) Shape {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := ParseShape(string(raw), table)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func mustProfile(t *testing.T, path, table string) (Profile, []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, keys, err := ParseProfile(string(raw), table)
	if err != nil {
		t.Fatal(err)
	}
	return p, keys
}

// The shape probe's output is parsed, not guessed at. Everything asserted here
// came back from a live warehouse and is committed as a fixture, which is the
// property that makes this testable at all: probe output is a file, so
// descriptor generation is a pure function.
func TestParseShape(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")

	if got := len(sh.Columns); got != 11 {
		t.Errorf("columns = %d, want 11", got)
	}
	// The STORED marker is the reason DESCRIBE is the probe rather than SHOW
	// COLUMNS or system.columns: measured, only DESCRIBE reports it.
	line, ok := sh.column("line")
	if !ok || line.Derived == "" {
		t.Fatalf("line should be a computed column: %+v", line)
	}
	if !strings.Contains(line.Derived, "concat_ws") {
		t.Errorf("line.Derived = %q", line.Derived)
	}
	// A plain column added later must NOT look computed.
	if raw, _ := sh.column("raw"); raw.Derived != "" {
		t.Errorf("raw is a plain column, got Derived=%q", raw.Derived)
	}
	if ts, _ := sh.column("ts"); ts.Type != "timestamp" {
		t.Errorf("ts type = %q", ts.Type)
	}
	if kv, _ := sh.column("kv"); kv.Type != "variant" {
		t.Errorf("kv type = %q", kv.Type)
	}

	// Indexes, with their options, which no other probe form reports.
	if len(sh.Indexes) != 2 {
		t.Fatalf("indexes = %d, want 2", len(sh.Indexes))
	}
	inv := sh.Indexes[0]
	if inv.Kind != InvertedIndex || inv.Name != "idx_msg" {
		t.Errorf("first index = %+v", inv)
	}
	if !sameSet(inv.Columns, []string{"msg", "kv", "line"}) {
		t.Errorf("idx_msg columns = %v", inv.Columns)
	}
	if inv.Tokenizer != "english" || !sameSet(inv.Filters, []string{"english_stop", "english_stemmer"}) {
		t.Errorf("idx_msg options: tokenizer=%q filters=%v", inv.Tokenizer, inv.Filters)
	}
	// The cluster key drives the timestamp role, and it has to survive a
	// function call: `to_date(ts), component` splits at the TOP-LEVEL comma
	// only, or the note reads `to_date(ts`.
	if !sameSet(sh.ClusterBy, []string{"to_date(ts)", "component"}) {
		t.Errorf("ClusterBy = %v", sh.ClusterBy)
	}
	if sh.timeColumn() != "ts" {
		t.Errorf("timeColumn = %q", sh.timeColumn())
	}
	if sh.Digest == "" {
		t.Error("no digest")
	}
}

// A paste from the wrong table, or a truncated one, must be refused. Both
// produce a confident and wrong descriptor otherwise, and the truncated case is
// not hypothetical — the first live run of this code was against output a shell
// pipeline had cut to three lines per statement, and it built a one-field
// descriptor without complaint.
func TestParseShapeRefusesBadInput(t *testing.T) {
	raw, err := os.ReadFile("../testdata/shape-k8s-logs.txt")
	if err != nil {
		t.Fatal(err)
	}
	full := string(raw)

	if _, err := ParseShape(full, "logs.something_else"); err == nil {
		t.Error("a paste from another table must be refused")
	} else if !strings.Contains(err.Error(), "logs.k8s_logs") {
		t.Errorf("the error should name the table it got: %v", err)
	}

	// Cut the DDL short: its closing paren is the completeness signal.
	cut := full[:strings.Index(full, ") ENGINE")]
	if _, err := ParseShape(cut, "logs.k8s_logs"); err == nil {
		t.Error("a truncated CREATE TABLE must be refused")
	} else if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("unhelpful error: %v", err)
	}

	if _, err := ParseShape("no markers here at all", "logs.k8s_logs"); err == nil {
		t.Error("output with no section markers must be refused")
	}

	// Drop one DESCRIBE row while leaving the DDL whole: the two sections list
	// the columns independently, so they check each other.
	lines := strings.Split(full, "\n")
	var kept []string
	for _, l := range lines {
		if strings.HasPrefix(l, "source_file\t") {
			continue
		}
		kept = append(kept, l)
	}
	if _, err := ParseShape(strings.Join(kept, "\n"), "logs.k8s_logs"); err == nil {
		t.Error("a DESCRIBE section missing a column the DDL names must be refused")
	} else if !strings.Contains(err.Error(), "source_file") {
		t.Errorf("the error should name the missing column: %v", err)
	}
}

// The value profile is the half that beats a type-only inference, and this is
// the case it exists for.
func TestProfileTypesFromContent(t *testing.T) {
	prof, discovered := mustProfile(t, "../testdata/profile-k8s-logs.txt", "logs.k8s_logs")
	if len(discovered) == 0 {
		t.Error("bag-key discovery rows should have been parsed")
	}

	// The duration column: 2 of 2,046 values cast, the rest are Go durations
	// like `47.823614ms`. A type-only inference does not call this numeric --
	// nothing promotes a bag key without a profile -- so what the ratio buys is
	// the refusal being RECORDED rather than assumed. `duration:>100` compiles
	// the same either way and warns either way; the profile is what makes the
	// 2-of-2,046 visible to whoever reads the descriptor.
	d, ok := prof.row("kv.duration")
	if !ok {
		t.Fatal("kv.duration missing from the profile")
	}
	if d.numeric() != numericMixed {
		t.Errorf("kv.duration should read as mixed, got %v (%d of %d cast)",
			d.numeric(), d.NumericOK, d.NonNull)
	}

	// A cleanly numeric key, for contrast.
	if r, ok := prof.row("kv.term"); ok && r.numeric() != numericYes {
		t.Errorf("kv.term should read as numeric: %d of %d", r.NumericOK, r.NonNull)
	}
	// The mixed key round 4 measured: 42,303 of 43,546.
	if r, ok := prof.row("kv.store_id"); ok && r.numeric() != numericMixed {
		t.Errorf("kv.store_id should read as mixed: %d of %d", r.NumericOK, r.NonNull)
	}
	// A timestamp column reads as all-timestamps and no-numbers, which is what
	// lets a VARCHAR holding instants be recognised.
	if r, ok := prof.row("ts"); ok {
		if !r.allTimestamps() || r.numeric() != numericNo {
			t.Errorf("ts profile = %+v", r)
		}
	}
}

// Introspecting the table the presets describe must reproduce them. Anything
// else means the generator and the hand-written descriptors disagree about the
// same table, and one of them is wrong.
func TestBuildReproducesPresets(t *testing.T) {
	for _, tc := range []struct {
		name, shape, profile, table, preset string
	}{
		{"live table", "../testdata/shape-k8s-logs.txt", "../testdata/profile-k8s-logs.txt",
			"logs.k8s_logs", "k8s-logs-line"},
		{"frozen copy", "../testdata/shape-k8s-logs-v2.txt", "../testdata/profile-k8s-logs.txt",
			"logs.k8s_logs_v2", "k8s-logs-line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := mustShape(t, tc.shape, tc.table)
			prof, keys := mustProfile(t, tc.profile, "")
			def, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys})
			if err != nil {
				t.Fatal(err)
			}
			got, notes, err := def.Schema()
			if err != nil {
				t.Fatalf("the generated descriptor must load: %v (notes %v)", err, notes)
			}
			want, _, err := Preset(tc.preset)
			if err != nil {
				t.Fatal(err)
			}
			if got.Default != want.Default {
				t.Errorf("default = %q, want %q", got.Default, want.Default)
			}
			if got.Time != want.Time || got.Severity != want.Severity {
				t.Errorf("roles: time=%q severity=%q, want %q/%q",
					got.Time, got.Severity, want.Time, want.Severity)
			}
			if len(got.Bags) != len(want.Bags) || got.Bags[0].Column != want.Bags[0].Column {
				t.Errorf("bags = %+v, want %+v", got.Bags, want.Bags)
			}
			// Every field the preset declares must be declared here too, with
			// the same kind and column. The generated one may declare MORE —
			// `raw` was such a case before the preset caught up.
			for name, wf := range want.Fields {
				gf, ok := got.Fields[name]
				if !ok {
					// An alias the generator does not invent by default.
					continue
				}
				if gf.Kind != wf.Kind || gf.Column != wf.Column {
					t.Errorf("field %q: got kind=%v column=%q, want kind=%v column=%q",
						name, gf.Kind, gf.Column, wf.Kind, wf.Column)
				}
			}
		})
	}
}

// The derived text surface has to be recognised from its EXPRESSION, not its
// name: it reads the message column and the attribute bag, and it carries the
// inverted index. Picking the message column instead would leave a bare word
// unable to find text the collector moved into the bag, which is the whole
// defect round 4 fixed.
func TestBuildPrefersTheDerivedSurface(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")
	prof, _ := mustProfile(t, "../testdata/profile-k8s-logs.txt", "")
	def, _, err := Build(sh, prof, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if def.Default != "line" {
		t.Fatalf("default = %q, want the derived column line", def.Default)
	}
	if !strings.Contains(def.Introspect.Roles["default"], "derived-text-surface") {
		t.Errorf("provenance should say why: %q", def.Introspect.Roles["default"])
	}
	// And the message column stays declared and stays narrow, so `msg:` still
	// asks about the message alone.
	var msg *FieldDef
	for i := range def.Fields {
		if def.Fields[i].Name == "msg" {
			msg = &def.Fields[i]
		}
	}
	if msg == nil || msg.Kind != "text" {
		t.Errorf("msg should remain a text field: %+v", msg)
	}
}

// Every field carries how it was decided. A guess laundered into configuration
// reads as fact to the next person.
func TestBuildRecordsProvenance(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")
	prof, keys := mustProfile(t, "../testdata/profile-k8s-logs.txt", "")
	def, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range def.Fields {
		if f.From == "" {
			t.Errorf("field %q has no provenance", f.Name)
		}
	}
	for _, b := range def.Bags {
		if b.From == "" {
			t.Errorf("bag %q has no provenance", b.Column)
		}
	}
	in := def.Introspect
	if in == nil || !in.Profiled || in.ColumnsDigest == "" {
		t.Fatalf("introspect block = %+v", in)
	}
	for _, role := range []string{"default", "time", "severity", "bags", "aliases"} {
		if in.Roles[role] == "" {
			t.Errorf("no provenance for role %q", role)
		}
	}
	// The refusals are the most important part: the record that a plausible
	// inference was deliberately not made.
	joined := strings.Join(in.Refused, "\n")
	for _, want := range []string{"kv.duration", "kv.store_id", "raw"} {
		if !strings.Contains(joined, want) {
			t.Errorf("refusals should mention %q; got %v", want, in.Refused)
		}
	}
	// A profile with no content evidence must say so rather than quietly
	// deciding from declared types alone.
	bare, _, err := Build(sh, Profile{}, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if bare.Introspect.Profiled {
		t.Error("Profiled should be false with no profile")
	}
	if !strings.Contains(strings.Join(bare.Introspect.Refused, "\n"), "no value profile") {
		t.Errorf("an unprofiled build must say so: %v", bare.Introspect.Refused)
	}
}

// The awkward table: every shape here is one the generator has to get right or
// refuse out loud.
func TestBuildAwkwardTable(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-awkward.txt", "logs.a2_awkward")
	prof, keys := mustProfile(t, "../testdata/profile-awkward.txt", "")
	def, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys})
	if err != nil {
		t.Fatal(err)
	}

	// An 8-row sample must NOT promote the time role, however clean it is. The
	// time role gates every time-bounded query, so a value that does not cast
	// leaves its row out of every panel at once, and eight rows is a
	// coincidence with a sample size rather than evidence.
	if def.Time != "" {
		t.Errorf("time = %q, want it declined on an 8-row sample", def.Time)
	}
	if len(def.Introspect.Blocked) == 0 {
		t.Error("declining the time role must be recorded as blocking, so build exits non-zero")
	}
	if !strings.Contains(def.Introspect.Roles["time"], "CANDIDATE, NOT APPLIED") {
		t.Errorf("the candidate should be recorded for a human: %q", def.Introspect.Roles["time"])
	}
	// It stays a plain string field meanwhile, rather than becoming a timestamp
	// nobody authorised.
	for _, f := range def.Fields {
		if f.Name == "event_time" && (f.Kind != "string" || f.Column != "") {
			t.Errorf("event_time should stay a plain string field: %+v", f)
		}
	}

	// Severity at rank 3 of its list, and the body at rank 5 of its.
	if def.Severity != "severity_text" {
		t.Errorf("severity = %q", def.Severity)
	}
	if def.Default != "content" {
		t.Errorf("default = %q", def.Default)
	}

	joined := strings.Join(def.Introspect.Refused, "\n")
	for _, want := range []string{
		"attrs.latency", // reads numeric, holds `1.5ms`
		"latency_ms",    // same, as a column
		"resource",      // a second VARIANT with no name match
		"tags",          // an ARRAY: no field kind can search it
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("refusals should mention %q; got %v", want, def.Introspect.Refused)
		}
	}
	// The bag is the one that matched a canonical name, and the second VARIANT
	// is not silently adopted.
	if len(def.Bags) != 1 || def.Bags[0].Column != "attrs" {
		t.Errorf("bags = %+v", def.Bags)
	}
	if _, _, err := def.Schema(); err != nil {
		t.Fatalf("the awkward descriptor should load once its index exists: %v", err)
	}
}

// Confirming the candidate by hand applies it — and the applied form warns on
// every use, which is the half that was missing entirely.
//
// The asymmetry was the defect: a numeric bound on a string-valued expression
// warned in full while a VARCHAR read as an instant warned about nothing, and
// the timestamp case is the worse of the two because it gates every
// time-bounded query rather than one filter.
func TestConfirmedTimePromotionWarns(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-awkward.txt", "logs.a2_awkward")
	prof, keys := mustProfile(t, "../testdata/profile-awkward.txt", "")

	def, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys, TimeColumn: "event_time"})
	if err != nil {
		t.Fatal(err)
	}
	if def.Time != "event_time" {
		t.Fatalf("time = %q, want event_time once confirmed", def.Time)
	}
	if len(def.Introspect.Blocked) != 0 {
		t.Errorf("a confirmed role is not blocking: %v", def.Introspect.Blocked)
	}
	var et FieldDef
	for _, f := range def.Fields {
		if f.Name == "event_time" {
			et = f
		}
	}
	if et.Kind != "timestamp" || !strings.Contains(et.Column, "TRY_CAST") {
		t.Fatalf("event_time = %+v", et)
	}
	// The declaration that makes the compiler warn.
	if et.Conversion == "" {
		t.Fatal("a cast-valued column must declare its conversion, or nothing warns")
	}
	if !strings.Contains(et.From, "user-supplied") {
		t.Errorf("provenance should say a human confirmed it: %q", et.From)
	}

	schema, _, err := def.Schema()
	if err != nil {
		t.Fatal(err)
	}
	// Every shape that reads the field must warn, not just the range.
	for _, q := range []string{
		"event_time:>2026-08-20T01:00:03Z",
		"event_time:[2026-08-19 TO 2026-08-21]",
		"event_time:*",
	} {
		r, err := CompileString(q, schema)
		if err != nil {
			t.Fatalf("CompileString(%q): %v", q, err)
		}
		var got string
		for _, w := range r.Warnings {
			if strings.Contains(w, "read through a cast") {
				got = w
			}
		}
		if got == "" {
			t.Errorf("no cast warning for %q; warnings were %v", q, r.Warnings)
			continue
		}
		for _, must := range []string{`"event_time"`, "EXCLUDED", "EVERY time-bounded", "count_if("} {
			if !strings.Contains(got, must) {
				t.Errorf("warning for %q omits %s: %s", q, must, got)
			}
		}
		// The count predicate has to be answerable: asking for rows that both
		// cast and do not cast is vacuous.
		if strings.Contains(got, "count_if(TRY_CAST(event_time AS TIMESTAMP) IS NOT NULL AND") {
			t.Errorf("the count predicate is vacuous: %s", got)
		}
	}
	// And a real TIMESTAMP column must NOT warn, or the advisory is noise.
	k8s, _, _ := Preset("k8s-logs-line")
	r, err := CompileString("ts:>2026-08-18T22:30:00Z", k8s)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "read through a cast") {
			t.Errorf("a real timestamp column must not warn: %s", w)
		}
	}
}

// Drift compares column TYPES, which it did not. Against a preset, `ts`
// becoming a VARCHAR or `kv` ceasing to be a VARIANT was reported as "no
// drift", because only an introspected descriptor carries a digest and nothing
// else looked — so the round's own guard test inherited the blind spot.
func TestDriftComparesTypes(t *testing.T) {
	raw, err := os.ReadFile("../testdata/shape-k8s-logs.txt")
	if err != nil {
		t.Fatal(err)
	}
	line, _, err := Preset("k8s-logs-line")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, from, to, want string }{
		{"time column becomes varchar", "ts	TIMESTAMP", "ts	VARCHAR", `field "ts" is kind "timestamp"`},
		{"severity becomes numeric", "level	VARCHAR", "level	BIGINT", `field "level" is kind "string"`},
		{"bag stops being a variant", "kv	VARIANT", "kv	VARCHAR", `bag "kv" is VARCHAR`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(string(raw), tc.from, tc.to, 1)
			if mutated == string(raw) {
				t.Fatalf("fixture does not contain %q", tc.from)
			}
			sh, err := ParseShape(mutated, "logs.k8s_logs")
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(Drift(sh, line, ""), "\n")
			if !strings.Contains(got, tc.want) {
				t.Errorf("drift should mention %q; got:\n%s", tc.want, got)
			}
		})
	}

	// A STORED column the table has redefined changes what every bare word
	// searches, so it is drift too.
	mutated := strings.Replace(string(raw), "object_delete(kv, 'container', 'service', 'format')",
		"object_delete(kv, 'container')", -1)
	sh, err := ParseShape(mutated, "logs.k8s_logs")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(Drift(sh, line, ""), "\n"); !strings.Contains(got, "definition of it has changed") {
		t.Errorf("a redefined computed column should be drift; got:\n%s", got)
	}

	// Control: the unmutated fixture, whose descriptor spells the cast
	// `::VARCHAR` where the engine prints `::STRING`, must report nothing.
	sh, err = ParseShape(string(raw), "logs.k8s_logs")
	if err != nil {
		t.Fatal(err)
	}
	if got := Drift(sh, line, ""); len(got) != 0 {
		t.Errorf("no drift expected, got %v", got)
	}
}

// A profile whose window is empty is not evidence, and must not be recorded as
// though it were. The window itself now travels in the profile output, because
// the two steps' defaults disagreed and the output is the only thing that knows
// what was measured.
func TestProfileWindowAndEmptyProfile(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")

	sql, err := ProfileProbe(sh, ProbeWindow{Hours: "1"}, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "|window|last 1h") {
		t.Error("the profile probe must carry its own window into its output")
	}

	// Round-trip it: a build that is given no -window still records the truth.
	out := "lsprobe|v1|section|profile|logs.k8s_logs\n" +
		"lsprobe|v1|window|last 1h\n" +
		"lsprobe|v1|prof|ts|10|10|0|10|10|26|26\n"
	prof, _, err := ParseProfile(out, "logs.k8s_logs")
	if err != nil {
		t.Fatal(err)
	}
	if prof.Window != "last 1h" {
		t.Errorf("Window = %q", prof.Window)
	}
	def, _, err := Build(sh, prof, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if def.Introspect.Window != "last 1h" {
		t.Errorf("recorded window = %q, want the profile's own", def.Introspect.Window)
	}
	if !def.Introspect.Profiled || def.Introspect.Rows != 10 {
		t.Errorf("profiled=%v rows=%d", def.Introspect.Profiled, def.Introspect.Rows)
	}

	// Zero rows scanned: the window was empty.
	empty := "lsprobe|v1|section|profile|logs.k8s_logs\n" +
		"lsprobe|v1|window|last 1h\n" +
		"lsprobe|v1|prof|ts|0|0|0|0|0|0|0\n"
	prof, _, err = ParseProfile(empty, "logs.k8s_logs")
	if err != nil {
		t.Fatal(err)
	}
	def, _, err = Build(sh, prof, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if def.Introspect.Profiled {
		t.Error("a profile that scanned 0 rows must not be recorded as profiled")
	}
	if !strings.Contains(strings.Join(def.Introspect.Refused, "\n"), "scanned 0 rows") {
		t.Errorf("it must say so: %v", def.Introspect.Refused)
	}
}

// A text field outside the index group is not slow, it is unusable — one search
// function per statement, and no index means no search function. So a default
// field with no inverted index must be recorded as text ANYWAY, so that loading
// fails and names it.
func TestBuildRefusesAnUnindexedDefault(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-awkward.txt", "logs.a2_awkward")
	sh.Indexes = nil // as the table was before its index existed
	def, _, err := Build(sh, Profile{}, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if def.Default != "content" {
		t.Fatalf("default = %q", def.Default)
	}
	if !strings.Contains(strings.Join(def.Introspect.Refused, "\n"), "no inverted index covers") {
		t.Errorf("refusals should name the missing index: %v", def.Introspect.Refused)
	}
	_, _, err = def.Schema()
	if err == nil {
		t.Fatal("a descriptor whose default field is unindexed must not load")
	}
	if !strings.Contains(err.Error(), "content") {
		t.Errorf("the load error must name the column: %v", err)
	}
}

// The regression guard for the defect that reached production: a real column
// that no descriptor declares.
//
// `raw` was added to logs.k8s_logs and the collector began populating it. No
// preset declared it, so `raw:hello` compiled to a bag lookup —
// `kv['raw'] IS NOT NULL` was 0 against `raw IS NOT NULL` of 5,112 — and the
// warning asserted that raw "is not a column", which was false. Binding did not
// catch it: the bind statement never mentions a column nobody declared, so it
// succeeded. Only comparing the column lists finds it, which is what Drift does.
//
// The fixture goes stale on purpose. When the table gains a column, this test
// fails until the preset declares it, and updating the fixture is part of that
// change rather than a way around it.
func TestPresetDeclaresEveryLiveColumn(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")
	line, _, err := Preset("k8s-logs-line")
	if err != nil {
		t.Fatal(err)
	}
	if got := Drift(sh, line, ""); len(got) != 0 {
		t.Errorf("k8s-logs-line has drifted from logs.k8s_logs:\n  %s",
			strings.Join(got, "\n  "))
	}

	// Two-sided: dropping the declaration must be caught, and caught with a
	// message that says what goes wrong rather than merely that something did.
	stripped := line
	stripped.Fields = map[string]Field{}
	for n, f := range line.Fields {
		if f.Column == "raw" {
			continue
		}
		stripped.Fields[n] = f
	}
	got := Drift(sh, stripped, "")
	if len(got) != 1 || !strings.Contains(got[0], "raw") {
		t.Fatalf("dropping raw should be one finding naming it, got %v", got)
	}
	if !strings.Contains(got[0], "match nothing") {
		t.Errorf("the finding should say what goes wrong: %q", got[0])
	}
}

// Drift reports the other directions too: an index that grew, a column that
// vanished, a digest that moved.
func TestDriftDirections(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")

	// The pre-migration preset against the migrated, extended table. This is
	// real drift, not a constructed case: the table gained `line` and `raw`, and
	// both indexes grew.
	pre, _, err := Preset("k8s-logs")
	if err != nil {
		t.Fatal(err)
	}
	pre.Table = "logs.k8s_logs"
	got := strings.Join(Drift(sh, pre, ""), "\n")
	for _, want := range []string{"\"line\"", "\"raw\"", "idx_msg covers", "idx_msg_ng covers"} {
		if !strings.Contains(got, want) {
			t.Errorf("drift should mention %s:\n%s", want, got)
		}
	}

	// A column the descriptor names and the table does not.
	line, _, _ := Preset("k8s-logs-line")
	line.Fields["gone"] = Field{Column: "no_such_column", Kind: String}
	if !strings.Contains(strings.Join(Drift(sh, line, ""), "\n"), "no longer has") {
		t.Error("a vanished column should be reported")
	}

	// A digest that moved.
	if !strings.Contains(strings.Join(Drift(sh, line, "fnv1a64:0000000000000000"), "\n"),
		"digest has changed") {
		t.Error("a changed digest should be reported")
	}
}

// The probe emitters have to produce SQL that runs, and the two things most
// easily got wrong are a reserved word as a column name and a cast that is a
// type error rather than a NULL.
func TestProbeEmission(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")

	shape := ShapeProbe("logs.k8s_logs")
	for _, want := range []string{"DESCRIBE logs.k8s_logs", "SHOW CREATE TABLE logs.k8s_logs",
		"section|columns", "section|create"} {
		if !strings.Contains(shape, want) {
			t.Errorf("shape probe missing %q", want)
		}
	}

	prof, err := ProfileProbe(sh, ProbeWindow{Until: "2026-08-20 00:00:00"}, 32)
	if err != nil {
		t.Fatal(err)
	}
	// `level` is a reserved word here, so an unquoted reference is a parse
	// error rather than a lookup failure.
	if !strings.Contains(prof, "`level`::VARCHAR") {
		t.Error("a reserved-word column must be quoted in the profile probe")
	}
	// Every cast goes through ::VARCHAR, because `TRY_CAST(ts AS DOUBLE)` is
	// not a NULL — it is [1006] unable to cast type Timestamp to type Float64,
	// and it fails the whole statement.
	if strings.Contains(prof, "TRY_CAST(ts AS") {
		t.Error("the numeric cast must go through ::VARCHAR")
	}
	if !strings.Contains(prof, "ts < '2026-08-20 00:00:00'") {
		t.Error("the absolute bound should be in the WHERE clause")
	}
	if !strings.Contains(prof, "FLATTEN(input => kv)") {
		t.Error("bag-key discovery should be emitted for the VARIANT column")
	}

	// A rolling window instead.
	prof2, err := ProfileProbe(sh, ProbeWindow{Hours: "3"}, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prof2, "subtract_hours(now(), 3)") {
		t.Error("a rolling window should be emitted")
	}

	keys, err := BagKeyProfileProbe(sh, "kv", []string{"duration", "store_id"},
		ProbeWindow{Until: "2026-08-20 00:00:00"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(keys, "kv['duration']::VARCHAR") {
		t.Errorf("bag key probe: %s", keys)
	}

	sch, _, _ := Preset("k8s-logs-line")
	v := VerifyProbe(sch)
	if !strings.Contains(v, "WHERE 1=0") || !strings.Contains(v, "DESCRIBE logs.k8s_logs") {
		t.Errorf("verify probe: %s", v)
	}
	if !strings.Contains(v, "SHOW CREATE TABLE") {
		t.Error("verify must re-read the indexes too, or index drift is invisible")
	}
}

// Bag keys are left as strings even when every sampled value casts, and that is
// a measurement rather than timidity: declaring one Number changes only the
// equality, and changes it from an index-backed lookup to a full scan.
func TestBagNumericIsOptIn(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")
	prof, keys := mustProfile(t, "../testdata/profile-k8s-logs.txt", "")

	off, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if len(off.Bags) != 1 || len(off.Bags[0].Keys) != 0 {
		t.Errorf("no key should be typed by default: %+v", off.Bags[0].Keys)
	}
	if !strings.Contains(strings.Join(rolesOf(off), "\n"), "full scan") {
		t.Error("the provenance should say what declaring it would cost")
	}

	on, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys, BagNumeric: true})
	if err != nil {
		t.Fatal(err)
	}
	if on.Bags[0].Keys["term"] != "number" {
		t.Errorf("-bag-numeric should type a cleanly numeric key: %+v", on.Bags[0].Keys)
	}
	// And never a mixed one, whichever way the flag is set.
	if _, typed := on.Bags[0].Keys["store_id"]; typed {
		t.Error("a mixed key must never be typed as a number")
	}
	if _, typed := on.Bags[0].Keys["duration"]; typed {
		t.Error("kv.duration must never be typed as a number")
	}
}

func rolesOf(d Def) []string {
	var out []string
	for _, v := range d.Introspect.Roles {
		out = append(out, v)
	}
	return out
}

// Aliases are off by default because their only failure mode is silent and in
// the wrong direction, and the names in the candidate lists are real bag keys on
// this very table.
func TestAliasesAreOptInAndCollisionChecked(t *testing.T) {
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")
	prof, keys := mustProfile(t, "../testdata/profile-k8s-logs.txt", "")

	off, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range off.Fields {
		if len(f.Aliases) != 0 {
			t.Errorf("field %q should have no aliases by default: %v", f.Name, f.Aliases)
		}
	}

	on, _, err := Build(sh, prof, BuildOptions{KnownBagKeys: keys, Aliases: true})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]FieldDef{}
	for _, f := range on.Fields {
		byName[f.Name] = f
	}
	// `message` names the MESSAGE column, not the reconstruction of the whole
	// line. The two are different questions: query('line:snapshot') is 25,488
	// rows and query('msg:snapshot') is 17,649.
	if !hasStr(byName["msg"].Aliases, "message") {
		t.Errorf("message should alias msg: %v", byName["msg"].Aliases)
	}
	if hasStr(byName["line"].Aliases, "message") {
		t.Errorf("message must not alias the derived surface: %v", byName["line"].Aliases)
	}
	// `body` names what a reader sees, which IS the derived surface.
	if !hasStr(byName["line"].Aliases, "body") {
		t.Errorf("body should alias the default field: %v", byName["line"].Aliases)
	}
	// A collision with a real bag key must be refused rather than shadowed. The
	// profile has to have seen the key for this to fire, which is why opting in
	// records how many keys were checked.
	sh2 := sh
	prof2 := Profile{Rows: map[string]ProfileRow{}}
	for k, v := range prof.Rows {
		prof2.Rows[k] = v
	}
	prof2.Rows["kv.body"] = ProfileRow{Col: "kv.body", Scanned: 10, NonNull: 3}
	col, _, err := Build(sh2, prof2, BuildOptions{Aliases: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range col.Fields {
		if f.Name == "line" && hasStr(f.Aliases, "body") {
			t.Error("body is a real bag key here, so it must not become an alias")
		}
	}
	if !strings.Contains(strings.Join(col.Introspect.Refused, "\n"), "alias") {
		t.Errorf("the refusal should be recorded: %v", col.Introspect.Refused)
	}
}

func hasStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// normaliseType peels nullability and parameters, which is what lets one rule
// serve `Nullable(String)`, `varchar(16382)` and `VARCHAR`.
func TestNormaliseType(t *testing.T) {
	for in, want := range map[string]string{
		"TIMESTAMP": "timestamp", "Nullable(Timestamp)": "timestamp", "DATE": "timestamp",
		"VARCHAR": "string", "varchar(16382)": "string", "Nullable(String)": "string",
		"BIGINT": "number", "Nullable(Nullable(Int64))": "number", "DECIMAL(10,2)": "number",
		"VARIANT": "variant", "JSON": "variant",
		"BOOLEAN":       "boolean",
		"ARRAY(STRING)": "other", "ARRAY(VARCHAR NULL)": "other",
		"Field": "", "---": "", "": "",
	} {
		if got := normaliseType(in); got != want {
			t.Errorf("normaliseType(%q) = %q, want %q", in, got, want)
		}
	}
}
