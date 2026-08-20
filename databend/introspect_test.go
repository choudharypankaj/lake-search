package databend

import (
	"fmt"
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

// The attribute bag was the one thing `verify` could not see: DESCRIBE does not
// enumerate a VARIANT's keys, so a declared key that vanished, changed shape or
// stopped casting was invisible. It is also the drift most likely to happen —
// log formats change without anyone touching code — and its consequence is the
// silent row drop this project exists to remove.
//
// Every assertion here is measured on logs.a2_bagdrift, a table built with two
// eras: an older one where every declared key is present, clean and scalar, and
// a recent one where latency_ms gained a unit suffix, gone_key stopped being
// written, shape_key became an object, and retries stayed clean as a control.
func TestBagDrift(t *testing.T) {
	schema := mustSchemaFile(t, "../testdata/schema-bagdrift.json")
	sh := mustShape(t, "../testdata/verify-bagdrift.txt", "logs.a2_bagdrift")
	rep := DriftDetail(sh, schema, "")
	got := strings.Join(rep.Drift, "\n")

	for _, tc := range []struct{ name, want string }{
		// A declared key that is no longer in the data: its typed comparisons
		// match nothing, silently and forever.
		{"key vanished", `declared key kv['gone_key'] appears in 0 of 7 rows`},
		// A declared numeric key whose values stopped casting. The ratio, not a
		// boolean: 2 of 7 and 3 of 250,036 want different reactions.
		{"stopped casting", `only 2 of its 7 values cast`},
		{"ratio is quantified", `(28.6%)`},
		{"and says what is lost", `silently drops the other 5 rows`},
		// Scalar to object: a path lookup means something different.
		{"shape changed", `kv['shape_key'] is not a scalar on 7 of its 7 rows (7 object, 0 array)`},
		// Shadowing: the shape that made `raw` answer nothing forever.
		{"shadowed by a column", `kv['latency_ms'] exists on 12 rows and "latency_ms" is ALSO an undeclared column`},
	} {
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: drift should mention %q; got:\n%s", tc.name, tc.want, got)
		}
	}

	// The control. `retries` is present on every row, casts on every row and is
	// a scalar, so it must produce nothing — otherwise the check is noise and
	// gets turned off.
	for _, line := range rep.Drift {
		if strings.Contains(line, "retries") {
			t.Errorf("retries is clean and must not be reported: %s", line)
		}
	}

	// The windows have to be stated, because existence and ratio want different
	// ones and a reader cannot judge the findings without knowing which.
	limits := strings.Join(rep.Limits, "\n")
	if !strings.Contains(limits, "statistics cover") || !strings.Contains(limits, "enumeration covers") {
		t.Errorf("both windows must be reported: %v", rep.Limits)
	}
	if !strings.Contains(limits, "a new key is not drift") {
		t.Errorf("the limits must say new keys are not reported: %v", rep.Limits)
	}
}

// Each check must fail when it is reverted, which for a pure function over probe
// text means mutating the text so the observation it reads goes away.
func TestBagDriftChecksAreLoadBearing(t *testing.T) {
	schema := mustSchemaFile(t, "../testdata/schema-bagdrift.json")
	raw, err := os.ReadFile("../testdata/verify-bagdrift.txt")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, from, to, gone string }{
		// Present = 0 is the only thing that says a key vanished. Give it rows
		// and the finding must disappear.
		{"key vanished", "bagstat|kv|gone_key|7|0|0|0|0", "bagstat|kv|gone_key|7|7|0|0|0",
			"gone_key"},
		// All values casting is the only thing that clears a numeric key.
		{"stopped casting", "bagstat|kv|latency_ms|7|7|2|0|0", "bagstat|kv|latency_ms|7|7|7|0|0",
			"only 2 of its 7"},
		// Zero objects and arrays is the only thing that clears a shape change.
		{"shape changed", "bagstat|kv|shape_key|7|7|0|7|0", "bagstat|kv|shape_key|7|7|0|0|0",
			"not a scalar"},
		// The enumeration row is the only thing that reports shadowing.
		{"shadowing", "bagname|kv|latency_ms|12", "bagname|kv|latency_ms|0", "exists on 12 rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := ParseShape(string(raw), "logs.a2_bagdrift")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(Drift(base, schema, ""), "\n"), tc.gone) {
				t.Fatalf("the finding is not present to begin with: %q", tc.gone)
			}
			mutated := strings.Replace(string(raw), tc.from, tc.to, 1)
			if mutated == string(raw) {
				t.Fatalf("fixture does not contain %q", tc.from)
			}
			sh, err := ParseShape(mutated, "logs.a2_bagdrift")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(Drift(sh, schema, ""), "\n"); strings.Contains(got, tc.gone) {
				t.Errorf("%q should be gone once the observation is; got:\n%s", tc.gone, got)
			}
		})
	}
}

// A shadowing key where the descriptor DOES declare the column is a standing
// hazard, not drift — the field wins, which is documented precedence, so it is
// not a wrong answer. Reporting it as drift would make `verify` fail on every
// run against a correct descriptor, and a check that cries wolf gets turned off.
//
// Measured on the live table: kv['namespace'] exists on tens of thousands of
// rows while `namespace` is a declared column, and kv['body'] on 3 while `body`
// is a declared alias of `line`.
func TestShadowingIsAHazardNotDrift(t *testing.T) {
	line, _, err := Preset("k8s-logs-line")
	if err != nil {
		t.Fatal(err)
	}
	sh := mustShape(t, "../testdata/verify-k8s-logs.txt", "logs.k8s_logs")
	rep := DriftDetail(sh, line, "")

	if len(rep.Drift) != 0 {
		t.Errorf("the no-drift control must stay quiet; got:\n  %s",
			strings.Join(rep.Drift, "\n  "))
	}
	haz := strings.Join(rep.Hazards, "\n")
	for _, want := range []string{"kv['namespace']", "kv['body']", "kv['node']", "kv['component']"} {
		if !strings.Contains(haz, want) {
			t.Errorf("hazards should mention %s; got:\n%s", want, haz)
		}
	}
	// It has to say which side wins and how to reach the other, or it is a
	// worry rather than information.
	if !strings.Contains(haz, "The field wins") || !strings.Contains(haz, "kv.namespace:value") {
		t.Errorf("the hazard must say which side wins and how to reach the rows: %s", haz)
	}
	// And it must not be counted as drift by the flat accessor either.
	if len(Drift(sh, line, "")) != 0 {
		t.Error("Drift() must return only things that changed")
	}
}

// A descriptor with typed keys whose probe output has no bag section at all must
// say the bag was not looked at, rather than reporting nothing and reading as a
// clean bill of health.
func TestBagNotProbedIsALimitNotSilence(t *testing.T) {
	schema := mustSchemaFile(t, "../testdata/schema-bagdrift.json")
	// The old two-section verify output, before the bag statements existed.
	sh := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")
	sh.Table = "logs.a2_bagdrift"
	rep := DriftDetail(sh, schema, "")
	if !strings.Contains(strings.Join(rep.Limits, "\n"), "attribute bag was not probed") {
		t.Errorf("an unprobed bag must be reported as a limit: %v", rep.Limits)
	}
}

// The emitted statements have to be the ones that were measured, and the two
// windows have to differ.
func TestBagDriftProbeEmission(t *testing.T) {
	line, _, err := Preset("k8s-logs-line")
	if err != nil {
		t.Fatal(err)
	}
	sql := BagDriftProbe(line, ProbeWindow{Hours: "24"}, ProbeWindow{})

	if !strings.Contains(sql, "LATERAL FLATTEN(input => kv)") {
		t.Error("the enumeration needs FLATTEN")
	}
	// Name-directed, not top-N: an exact IN list of the names the descriptor
	// claims, and no LIMIT, so the output cannot be mistaken for "all the keys".
	if !strings.Contains(sql, "WHERE lower(f.key) IN (") {
		t.Error("the enumeration must be name-directed")
	}
	if strings.Contains(sql, "ORDER BY count(*) DESC;\nLIMIT") || strings.Contains(sql, "LIMIT 3") {
		t.Error("the name enumeration must not be truncated")
	}
	// With no typed keys there is no per-key statement, so the marker must not
	// claim a statistics window that measured nothing.
	if !strings.Contains(sql, "bagwindow|stats|not measured") {
		t.Error("with no typed keys the statistics window must say nothing was measured")
	}
	if !strings.Contains(sql, "bagwindow|enum|") || !strings.Contains(sql, "bagwindow|stats|") {
		t.Error("both windows must be carried in the output")
	}
	// No bound whose upper edge has not happened yet.
	if strings.Contains(sql, "ts < '") {
		t.Error("an upper bound in the emitted probe risks an instant that has not happened")
	}
	// A bag with no typed keys emits no per-key statements, only the
	// enumeration — the shipped preset declares none.
	if strings.Contains(sql, "bagstat") {
		t.Error("k8s-logs-line declares no typed keys, so no per-key statement should be emitted")
	}

	// With typed keys, one branch each, and the cap is announced when it bites.
	withKeys := mustSchemaFile(t, "../testdata/schema-bagdrift.json")
	sql = BagDriftProbe(withKeys, ProbeWindow{Hours: "24"}, ProbeWindow{})
	for _, k := range []string{"gone_key", "latency_ms", "retries", "shape_key"} {
		if !strings.Contains(sql, "bagstat|kv|"+k) {
			t.Errorf("no statement for declared key %q", k)
		}
	}
	if !strings.Contains(sql, "json_typeof(") {
		t.Error("shape detection needs json_typeof")
	}
	// Now the statistics window is real and bounded, and the enumeration still
	// is not — the two questions want different windows.
	if !strings.Contains(sql, "subtract_hours(now(), 24)") {
		t.Error("the per-key statistics window should be bounded and recent")
	}
	if strings.Count(sql, "subtract_hours(now(), 24)") != 4 {
		t.Errorf("every per-key branch should carry the bound, got %d",
			strings.Count(sql, "subtract_hours(now(), 24)"))
	}

	big := withKeys
	big.Bags = []Bag{{Column: "kv", Keys: map[string]Kind{}}}
	for i := 0; i < maxDeclaredKeysProbed+5; i++ {
		big.Bags[0].Keys[fmt.Sprintf("k%03d", i)] = String
	}
	sql = BagDriftProbe(big, ProbeWindow{Hours: "24"}, ProbeWindow{})
	if !strings.Contains(sql, "baglimit|") {
		t.Error("a capped key list must say so in the output")
	}
	if n := strings.Count(sql, "bagstat|kv|"); n != maxDeclaredKeysProbed {
		t.Errorf("emitted %d per-key statements, want the cap of %d", n, maxDeclaredKeysProbed)
	}
}

// Paste fidelity has to keep working now that verify emits a third section.
func TestVerifyOutputRefusesBadInput(t *testing.T) {
	raw, err := os.ReadFile("../testdata/verify-bagdrift.txt")
	if err != nil {
		t.Fatal(err)
	}
	full := string(raw)

	if _, err := ParseShape(full, "logs.somewhere_else"); err == nil {
		t.Error("a paste from another table must be refused")
	}
	cut := full[:strings.Index(full, ") ENGINE")]
	if _, err := ParseShape(cut, "logs.a2_bagdrift"); err == nil {
		t.Error("a truncated CREATE TABLE must be refused even with bag rows present")
	}

	// Noise tolerance: a client banner, box drawing, ANSI colour and a row
	// count must not stop the bag rows parsing.
	noisy := "Welcome to some client 1.2.3\n\x1b[32m+------+\x1b[0m\n" + full + "\n(12 rows)\n"
	sh, err := ParseShape(noisy, "logs.a2_bagdrift")
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.BagKeys) != 4 {
		t.Errorf("bag rows should survive noise: got %d", len(sh.BagKeys))
	}
	if sh.BagWindow == "" || sh.BagEnumWindow == "" {
		t.Error("both windows should survive noise")
	}
}

func mustSchemaFile(t *testing.T, path string) Schema {
	t.Helper()
	s, _, err := LoadSchema(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A VARIANT key's SPELLING is its identity on this engine, so a declared key
// must never be folded. Measured on the live table: kv['tableID'] is present on
// 280,556 rows and kv['tableid'] on 0.
//
// Folding it made the bag-statistics probe ask about a key that does not exist,
// so a descriptor correctly declaring `tableID` — one the tool GENERATED —
// reported the key as vanished and exited 1. That is the cry-wolf failure the
// whole check is premised on avoiding, and the library was making the exact
// mistake its own advisory warns users about.
func TestBagKeySpellingIsPreserved(t *testing.T) {
	def := Def{
		Table: "t", Default: "body", Time: "ts",
		Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
		Fields: []FieldDef{
			{Name: "body", Kind: "text"},
			{Name: "ts", Kind: "timestamp"},
		},
		Bags: []BagDef{{Column: "kv", Keys: map[string]string{
			"tableID": "number", "reconcileID": "string",
		}}},
	}
	s, _, err := def.Schema()
	if err != nil {
		t.Fatal(err)
	}

	// Stored exactly as declared. Under the fold these keys were `tableid` and
	// `reconcileid`.
	if _, ok := s.Bags[0].Keys["tableID"]; !ok {
		t.Fatalf("the declared spelling must survive: %v", s.Bags[0].Keys)
	}
	if _, ok := s.Bags[0].Keys["tableid"]; ok {
		t.Error("a folded spelling must NOT be stored")
	}

	// Looked up exactly. `tableID:5` gets the declared numeric kind; `tableid:5`
	// is a different key and must not inherit it, because the subscript the
	// Field carries is built from the name the user typed.
	if got := mustCompile(t, "tableID:5", s); got != "TRY_CAST(kv['tableID']::VARCHAR AS DOUBLE) = 5" {
		t.Errorf("declared spelling: %s", got)
	}
	if got := mustCompile(t, "tableid:5", s); strings.Contains(got, "TRY_CAST") {
		t.Errorf("a differently-cased key must not inherit the declaration: %s", got)
	}

	// And the probe asks about the declared spelling, which is the half that
	// was broken.
	sql := BagDriftProbe(s, ProbeWindow{Hours: "24"}, ProbeWindow{})
	if !strings.Contains(sql, "kv['tableID']") {
		t.Error("the probe must subscript the declared spelling")
	}
	if strings.Contains(sql, "kv['tableid']") {
		t.Error("the probe must not subscript a folded spelling")
	}
	if !strings.Contains(sql, "bagstat|kv|tableID") {
		t.Error("the marker must carry the declared spelling, or Drift cannot match the stat")
	}

	// End to end: the probe output a correct descriptor produces must report no
	// drift. This is Agent 3's blocking case.
	out := "lsprobe|v1|section|columns|t\nbody\tVARCHAR\tYES\tNULL\t\nts\tTIMESTAMP\tYES\tNULL\t\n" +
		"kv\tVARIANT\tYES\tNULL\t\n" +
		"lsprobe|v1|section|create|t\nt\t\"CREATE TABLE t (\n  body VARCHAR NULL,\n" +
		"  ts TIMESTAMP NULL,\n  kv VARIANT NULL,\n" +
		"  SYNC INVERTED INDEX i (body)\n) ENGINE=FUSE\"\n" +
		"lsprobe|v1|section|bagkeys|t\n" +
		"lsprobe|v1|bagwindow|stats|the last 24 hour(s) of ts\n" +
		"lsprobe|v1|bagwindow|enum|no bound\n" +
		"lsprobe|v1|bagstat|kv|tableID|300|300|300|0|0\n" +
		"lsprobe|v1|bagstat|kv|reconcileID|300|300|0|0|0\n" +
		"lsprobe|v1|bagname|kv|tableID|300\n" +
		"lsprobe|v1|bagname|kv|reconcileID|300\n"
	sh, err := ParseShape(out, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := Drift(sh, s, ""); len(got) != 0 {
		t.Errorf("a correct camelCase descriptor must report no drift; got:\n  %s",
			strings.Join(got, "\n  "))
	}
}

// The other direction of the same fact: a hand-written descriptor that declares
// the wrong case is a real and silent mistake, and it deserves its own sentence
// rather than being reported as a key that vanished.
func TestBagKeyCaseMismatchIsAHazard(t *testing.T) {
	def := Def{
		Table: "t", Default: "body", Time: "ts",
		Indexes: []IndexDef{{Name: "i", Kind: "inverted", Columns: []string{"body"}}},
		Fields: []FieldDef{
			{Name: "body", Kind: "text"},
			{Name: "ts", Kind: "timestamp"},
		},
		// Declared lowercase; the data has camelCase.
		Bags: []BagDef{{Column: "kv", Keys: map[string]string{"tableid": "number"}}},
	}
	s, _, err := def.Schema()
	if err != nil {
		t.Fatal(err)
	}
	out := "lsprobe|v1|section|columns|t\nbody\tVARCHAR\tYES\tNULL\t\nts\tTIMESTAMP\tYES\tNULL\t\n" +
		"kv\tVARIANT\tYES\tNULL\t\n" +
		"lsprobe|v1|section|create|t\nt\t\"CREATE TABLE t (\n  body VARCHAR NULL,\n" +
		"  ts TIMESTAMP NULL,\n  kv VARIANT NULL,\n" +
		"  SYNC INVERTED INDEX i (body)\n) ENGINE=FUSE\"\n" +
		"lsprobe|v1|section|bagkeys|t\n" +
		"lsprobe|v1|bagwindow|stats|the last 24 hour(s) of ts\n" +
		"lsprobe|v1|bagwindow|enum|no bound\n" +
		"lsprobe|v1|bagstat|kv|tableid|300|0|0|0|0\n" +
		"lsprobe|v1|bagname|kv|tableID|300\n"
	sh, err := ParseShape(out, "t")
	if err != nil {
		t.Fatal(err)
	}
	rep := DriftDetail(sh, s, "")

	// Not drift: the key did not vanish, it was misspelled.
	for _, d := range rep.Drift {
		if strings.Contains(d, "outlived it") {
			t.Errorf("a case mismatch must not be reported as a vanished key: %s", d)
		}
	}
	haz := strings.Join(rep.Hazards, "\n")
	for _, want := range []string{
		"kv['tableid'] is absent", "different case: kv['tableID'] on 300 rows",
		"case-sensitive", `Re-declare it as "tableID"`,
	} {
		if !strings.Contains(haz, want) {
			t.Errorf("the hazard should mention %q; got:\n%s", want, haz)
		}
	}

	// Two spellings, both live. This is not hypothetical: on logs.k8s_logs
	// kv['safepoint'] is present on 1,740 rows and kv['safePoint'] on 459, so a
	// case-insensitive lookup has two right answers and must not pick one.
	both := strings.Replace(out, "lsprobe|v1|bagname|kv|tableID|300\n",
		"lsprobe|v1|bagname|kv|tableID|300\nlsprobe|v1|bagname|kv|TableID|7\n", 1)
	shBoth, err := ParseShape(both, "t")
	if err != nil {
		t.Fatal(err)
	}
	hazBoth := strings.Join(DriftDetail(shBoth, s, "").Hazards, "\n")
	for _, want := range []string{
		"kv['TableID'] on 7 rows", "kv['tableID'] on 300 rows",
		"There are 2 of them", "nothing can choose for you",
	} {
		if !strings.Contains(hazBoth, want) {
			t.Errorf("with two candidate spellings the hazard should mention %q; got:\n%s",
				want, hazBoth)
		}
	}
	if strings.Contains(hazBoth, "Re-declare it as") {
		t.Error("with two candidates it must not name one as though it were the answer")
	}

	// And a key that genuinely vanished — no spelling of it in the data — is
	// still drift, or the hazard would swallow the real finding.
	out2 := strings.Replace(out, "lsprobe|v1|bagname|kv|tableID|300\n", "", 1)
	sh2, err := ParseShape(out2, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(Drift(sh2, s, ""), "\n"); !strings.Contains(got, "outlived it") {
		t.Errorf("a genuinely absent key must still be drift; got:\n%s", got)
	}
}

// Every script this tool emits must survive the procedure this project documents
// everywhere: strip `--` lines, then split on `;`.
//
// The `baglimit` marker did not. Its payload contained a semicolon, so the
// statement was cut in half, both halves failed [1005], the cap was never
// announced, and the operator got two spurious parse errors on an otherwise
// clean probe — a marker whose entire job is to prevent a silent partial answer,
// going silently missing.
//
// Asserted generically rather than for that one marker, so the next sentence
// somebody writes into a literal cannot reintroduce it: every statement the
// documented split produces must have balanced quotes.
func TestEmittedScriptsSurviveTheDocumentedSplit(t *testing.T) {
	line, _, err := Preset("k8s-logs-line")
	if err != nil {
		t.Fatal(err)
	}
	shape := mustShape(t, "../testdata/shape-k8s-logs.txt", "logs.k8s_logs")
	prof, err := ProfileProbe(shape, ProbeWindow{Hours: "1"}, 32)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := BagKeyProfileProbe(shape, "kv", []string{"duration", "store_id"},
		ProbeWindow{Hours: "1"})
	if err != nil {
		t.Fatal(err)
	}

	// A schema whose typed-key count trips the cap, so the baglimit marker is
	// actually in the corpus under test.
	capped := line
	capped.Bags = []Bag{{Column: "kv", Keys: map[string]Kind{}}}
	for i := 0; i < maxDeclaredKeysProbed+5; i++ {
		capped.Bags[0].Keys[fmt.Sprintf("k%03d", i)] = String
	}

	scripts := map[string]string{
		"shape":    ShapeProbe("logs.k8s_logs"),
		"profile":  prof,
		"bagkeys":  keys,
		"verify":   VerifyProbe(line),
		"bagdrift": BagDriftProbe(capped, ProbeWindow{Hours: "24"}, ProbeWindow{}),
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			for _, stmt := range splitLikeTheDocs(script) {
				if strings.Count(stmt, "'")%2 != 0 {
					t.Errorf("a statement has unbalanced quotes after the documented split, so "+
						"a literal contains a semicolon:\n%s", stmt)
				}
			}
		})
	}

	// And the cap text specifically must arrive whole, in one statement.
	const want = "only the first 64 are checked"
	found := 0
	for _, stmt := range splitLikeTheDocs(scripts["bagdrift"]) {
		if strings.Contains(stmt, want) {
			found++
		}
	}
	if found != 1 {
		t.Errorf("the cap announcement should survive as exactly one statement, got %d", found)
	}
}

// splitLikeTheDocs is the procedure every instruction in this project specifies:
// strip comment lines, then split on `;`.
func splitLikeTheDocs(script string) []string {
	var kept []string
	for _, l := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "--") {
			continue
		}
		kept = append(kept, l)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(kept, " "), ";") {
		if strings.TrimSpace(stmt) != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// A window the report cannot see is reported as unseen, not explained away.
//
// The fallback previously asserted "this schema declares no time column", which
// is false for a descriptor that declares one and whose marker was trimmed — and
// it said it of the ENUMERATION window too, which is never time-bounded under
// any circumstances. The reason is knowable only where the schema is in hand, so
// the probe writes it into the marker and the report never guesses.
func TestWindowFallbackDoesNotInventAReason(t *testing.T) {
	withTime := mustSchemaFile(t, "../testdata/schema-bagdrift.json")
	raw, err := os.ReadFile("../testdata/verify-bagdrift.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Strip both window markers, as an older or trimmed capture would.
	var kept []string
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.Contains(l, "|bagwindow|") {
			kept = append(kept, l)
		}
	}
	sh, err := ParseShape(strings.Join(kept, "\n"), "logs.a2_bagdrift")
	if err != nil {
		t.Fatal(err)
	}
	limits := strings.Join(DriftDetail(sh, withTime, "").Limits, "\n")
	if strings.Contains(limits, "declares no time column") {
		t.Errorf("the report must not claim a reason it cannot know: %s", limits)
	}
	if !strings.Contains(limits, "not stated in this output") {
		t.Errorf("an absent marker should say it is absent: %s", limits)
	}

	// Probe side, where the reason IS knowable: both cases, distinctly.
	noTime := withTime
	noTime.Time = ""
	sql := BagDriftProbe(noTime, ProbeWindow{}, ProbeWindow{})
	if !strings.Contains(sql, "this schema declares no time column") {
		t.Error("with no time column the probe should say that is why it is unbounded")
	}
	sql = BagDriftProbe(withTime, ProbeWindow{}, ProbeWindow{})
	if strings.Contains(sql, "declares no time column") {
		t.Error("with a time column the probe must not claim there is none")
	}
	if !strings.Contains(sql, "no bound was requested") {
		t.Errorf("it should say what actually happened: %s", sql)
	}
}

// Both directions of the statistics window's blind spot must be disclosed. Only
// the absence direction was, and the presence direction is the one that returns
// a wrong answer rather than no answer: a key clean in the window and dirty
// outside it reports nothing.
func TestLimitsDiscloseBothDirections(t *testing.T) {
	schema := mustSchemaFile(t, "../testdata/schema-bagdrift.json")
	sh := mustShape(t, "../testdata/verify-bagdrift.txt", "logs.a2_bagdrift")
	limits := strings.Join(DriftDetail(sh, schema, "").Limits, "\n")
	for _, want := range []string{
		"ABSENT from it may still exist in history",
		"PRESENT and clean in it may be dirty outside it",
		"a new key is not drift",
	} {
		if !strings.Contains(limits, want) {
			t.Errorf("limits should disclose %q; got:\n%s", want, limits)
		}
	}
}

// A marker carrying the protocol's prefix and not its shape is refused, because
// the point of tagging is that malformed input cannot be mistaken for a
// measurement. An extra pipe field previously made the report say "statistics
// cover enum; the enumeration covers stats".
func TestMalformedBagWindowIsRefused(t *testing.T) {
	raw, err := os.ReadFile("../testdata/verify-bagdrift.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"extra field", "|bagwindow|stats|", "|bagwindow|stats|extra|"},
		{"unknown scope", "|bagwindow|stats|", "|bagwindow|middle|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(string(raw), tc.from, tc.to, 1)
			if mutated == string(raw) {
				t.Fatalf("fixture does not contain %q", tc.from)
			}
			_, err := ParseShape(mutated, "logs.a2_bagdrift")
			if err == nil {
				t.Fatal("a malformed bagwindow marker must be refused")
			}
			if !strings.Contains(err.Error(), "malformed") {
				t.Errorf("unhelpful error: %v", err)
			}
		})
	}
	// Control: the unmutated fixture parses.
	if _, err := ParseShape(string(raw), "logs.a2_bagdrift"); err != nil {
		t.Fatalf("the valid fixture must still parse: %v", err)
	}
}
