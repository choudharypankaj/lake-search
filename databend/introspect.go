package databend

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Introspection turns a table nobody has described into a descriptor, in two
// steps and without a database driver.
//
// # Why two steps rather than a connection
//
// `introspect` EMITS probe SQL; the operator runs it through whatever client
// already holds their credentials; a second invocation CONSUMES that output.
// The tool never connects, which is what keeps this module dependency-free —
// the property that lets it be vendored into a Grafana datasource plugin — and
// it means introspection works against warehouses the tool could not reach at
// all: SSO-only, air-gapped, or reachable only through a datasource that holds
// the credentials already.
//
// It costs three things, all real: there is no per-expression validation loop
// (see VerifyProbe, which buys most of it back for one more paste), a descriptor
// is a snapshot that does not notice tomorrow's new column (see the columns
// digest), and paste fidelity has to be defended (see the section markers).
//
// # What it buys, and it is the whole reason to prefer it
//
// An offline probe the operator runs deliberately can afford to look at the
// DATA, which an interactive form debouncing at one second cannot.
//
// Be precise about what that buys, because the obvious claim is wrong. Over the
// live table bounded `ts < 2026-08-20 00:00:00` (997,592 rows) the bag key
// `duration` holds 2,046 values and 2 of them cast to a number — the rest are Go
// durations like `47.823614ms` — so `duration:>100` answers 0 of 2,046. The
// profile does NOT stop that: a numeric bound on a bag key compiles to TRY_CAST
// with or without a profile, byte for byte, and round 4b's warning fires either
// way. Nothing in this tool types that key numeric at any flag setting.
//
// What the profile buys is knowing. It is the only thing that can say the ratio
// is 2 of 2,046 rather than 2,046 of 2,046, and it names the key in `refused` so
// a human sees it before writing the query rather than after reading a zero. It
// also buys three decisions that are unavailable without it: -bag-numeric has
// nothing to act on, a mostly-empty column cannot be suppressed from `display`,
// and a VARCHAR holding instants cannot be recognised as a time column.
//
// # Provenance
//
// Every field records how it was decided. A guess laundered into configuration
// reads as fact to the next person, which is worse than no guess at all, so
// `from` says `canonical-name`, `content-sample`, `lone-candidate`,
// `derived-text-surface` or `type-only` — and `introspect.refused` says what was
// considered and rejected, with the evidence.

// ProbeVersion is stamped into every emitted probe and checked on the way back
// in. It changes when the probe's output shape changes.
const ProbeVersion = "v1"

const probeMarker = "lsprobe"

// Shape is what the first probe reports: the column list, the index
// definitions, and the cluster key.
type Shape struct {
	Table     string
	Columns   []Column
	Indexes   []Index
	ClusterBy []string

	// Digest is a stable fingerprint of the column list, so `verify` can report
	// that the table has drifted since the descriptor was written.
	Digest string

	// BagKeys are the declared bag keys' observations, keyed `<bag>.<key>`.
	BagKeys map[string]BagKeyStat

	// BagNames are keys observed under a name that the descriptor declares for
	// something else — a column, a field or an alias — keyed `<bag>.<key>`,
	// valued by the rows carrying them. This is the shadowing hazard, and it is
	// how `raw` bit us: the descriptor routed a name to the bag while the data
	// lived in a column of that name.
	BagNames map[string]int64

	// BagWindow is the bound the per-key statistics used and BagEnumWindow the
	// bound the name enumeration used. They differ on purpose; see
	// BagDriftProbe.
	BagWindow     string
	BagEnumWindow string

	// BagLimited, when non-empty, says the per-key statements were capped, so a
	// silent partial answer cannot read as a complete one.
	BagLimited string

	// bad collects markers that carried the protocol's prefix and then did not
	// match its shape. They are refused rather than guessed at, because a
	// misparsed marker reads as a measurement.
	bad []string
}

// Column is one column as the table describes itself.
type Column struct {
	Name string

	// Type is the declared type, normalised to one of "timestamp", "string",
	// "number", "variant", "boolean" or "other".
	Type string

	// RawType is what the engine actually said, kept for the note when a
	// column is refused.
	RawType string

	Nullable bool

	// Derived is the expression of a STORED computed column, empty otherwise.
	//
	// It is load-bearing twice. A computed column must never be proposed as a
	// plain column, because a writer cannot set it; and a STORED text column
	// whose expression reads the body column and the attribute bag is the
	// derived-text-surface shape, which is the best default field a table can
	// offer. Only DESCRIBE reports this: measured on logs.k8s_logs_v2, DESCRIBE
	// returns `STORED COMPUTED COLUMN` plus the expression in its fifth field,
	// while SHOW COLUMNS says nothing and system.columns leaves both
	// default_kind and default_expression empty.
	Derived string
}

// ProfileRow is one column's or one bag key's sampled value profile.
type ProfileRow struct {
	// Col is a column name, or `<bag>.<key>` for a bag key.
	Col string

	Scanned     int64
	NonNull     int64
	NumericOK   int64
	TimestampOK int64
	Distinct    int64
	MinLen      int64
	MaxLen      int64
}

// numericVerdict classifies the numeric cast rate, which is the observation the
// duration column turns on.
type numericVerdict int

const (
	numericNo numericVerdict = iota
	numericMixed
	numericYes
)

func (p ProfileRow) numeric() numericVerdict {
	switch {
	case p.NonNull == 0:
		return numericNo
	case p.NumericOK == p.NonNull:
		return numericYes
	case p.NumericOK == 0:
		return numericNo
	default:
		return numericMixed
	}
}

// NumericOKTimestamps is the count of sampled values that cast to TIMESTAMP.
func (p ProfileRow) NumericOKTimestamps() int64 { return p.TimestampOK }

func (p ProfileRow) allTimestamps() bool {
	return p.NonNull > 0 && p.TimestampOK == p.NonNull
}

// BagKeyStat is one DECLARED bag key's observation, as the verify probe reports
// it.
//
// A bag key has no schema, which is the point of a bag and also why it is the
// one thing `verify` could not see: DESCRIBE does not enumerate keys, so a key
// that vanished, changed type, or stopped casting was invisible to drift
// detection. It is the drift most likely to happen — log formats change without
// anyone touching code — and its consequence is precisely the silent row drop
// this project exists to remove.
type BagKeyStat struct {
	Bag string
	Key string

	// Scanned is the rows the window covered, so "present in 0" can be
	// distinguished from "the window was empty".
	Scanned int64

	// Present is the rows where the key exists at all.
	Present int64

	// NumericOK is the rows whose value casts to a number, for a key declared
	// Number.
	NumericOK int64

	// Objects and Arrays count the rows where the value is not a scalar. A
	// path lookup means something different against either: `k:v` is an
	// equality against a rendered JSON document rather than against a value.
	Objects int64
	Arrays  int64
}

// Profile is the set of profile rows, keyed by Col.
type Profile struct {
	Table string
	Rows  map[string]ProfileRow

	// Window is the bound the probe was emitted with, carried in the output so
	// the provenance block records what was actually measured rather than what
	// a second flag happened to say.
	Window string
}

// Scanned is the largest row count any branch of the profile saw. Zero means the
// window was empty, which is not the same as "not profiled" and must not be
// recorded as if evidence had been gathered.
func (p Profile) Scanned() int64 {
	var n int64
	for _, r := range p.Rows {
		if r.Scanned > n {
			n = r.Scanned
		}
	}
	return n
}

func (p Profile) row(col string) (ProfileRow, bool) {
	if p.Rows == nil {
		return ProfileRow{}, false
	}
	r, ok := p.Rows[col]
	return r, ok
}

// ---------------------------------------------------------------- emitting

// ShapeProbe emits the first probe: what the table is made of.
//
// Two statements, each preceded by a marked SELECT that labels the section and
// names the table. The marker is how the consumer defends against paste
// fidelity — a truncated capture, output from a different table, a client that
// prints a banner — and it converts a silently wrong descriptor into a refusal.
//
// DESCRIBE rather than SHOW COLUMNS or system.columns, and that is measured
// rather than assumed. All three report name and type; only DESCRIBE reports
// that a column is a STORED computed column and what its expression is. On
// logs.k8s_logs_v2 the `line` column comes back from DESCRIBE as
// `line VARCHAR YES NULL STORED COMPUTED COLUMN \`concat_ws(...)\“, from SHOW
// COLUMNS with a bare `NULL NULL`, and from system.columns with default_kind
// and default_expression both empty.
//
// SHOW CREATE TABLE is not optional. It is the only form that reports an
// index's tokenizer and filters, and this compiler cannot defer the index
// question to query time the way a token-function fallback can: one search
// function per statement, and a text field outside the index group is not slow,
// it is unusable. A filter declared that the index lacks changes the answer.
func ShapeProbe(table string) string {
	var b strings.Builder
	b.WriteString(probeHeader("shape", table))
	fmt.Fprintf(&b, `-- Run BOTH statements and save the whole output, markers included, to one file.
-- Nothing here reads data: these are metadata statements only.

%s
DESCRIBE %s;

%s
SHOW CREATE TABLE %s;
`,
		sectionMarker("columns", table), table,
		sectionMarker("create", table), table)
	return b.String()
}

// ProbeWindow bounds the value profile.
//
// Absolute bounds are offered alongside the rolling one because a rolling window
// is not reproducible and a frozen table has no recent rows at all. The frozen
// copy this repo measures against, logs.k8s_logs_v2, ends at
// 2026-08-19 22:18:58 — a `now() - 1 hour` bound over it profiles zero rows and
// every content decision then rests on no evidence while looking like it rests
// on some. Absolute bounds also mean a profile can be re-run and compared, which
// is what makes a descriptor's numbers checkable rather than merely asserted.
type ProbeWindow struct {
	// Hours is a rolling bound: `<time> >= now() - Hours hours`.
	Hours string

	// Since and Until are absolute, and win over Hours when set.
	Since string
	Until string
}

// predicate renders the bound against a time column, and describes itself.
func (w ProbeWindow) predicate(timeCol string) (sql, desc string) {
	if timeCol == "" {
		return "", ""
	}
	var parts []string
	if w.Since != "" {
		parts = append(parts, fmt.Sprintf("%s >= '%s'", timeCol, escapeString(w.Since)))
	}
	if w.Until != "" {
		parts = append(parts, fmt.Sprintf("%s < '%s'", timeCol, escapeString(w.Until)))
	}
	if len(parts) > 0 {
		return "\nWHERE " + strings.Join(parts, "\n  AND "), strings.Join(parts, " AND ")
	}
	if w.Hours != "" {
		return fmt.Sprintf("\nWHERE %s >= subtract_hours(now(), %s)", timeCol, w.Hours),
			fmt.Sprintf("the last %s hour(s) of %s", w.Hours, timeCol)
	}
	return "", ""
}

// Describe renders the window for the provenance block.
func (w ProbeWindow) Describe() string {
	switch {
	case w.Since != "" || w.Until != "":
		return strings.TrimSpace(w.Since + " .. " + w.Until)
	case w.Hours != "":
		return "last " + w.Hours + "h"
	}
	return "whole table"
}

// ProfileProbe emits the second probe: what the values in those columns
// actually look like.
//
// It needs the first probe's output, and not merely for convenience. Three
// things are impossible without it:
//
//   - the window. The probe is bounded to keep it cheap, and the bound needs
//     the timestamp column, which is what the shape probe found.
//   - the bag keys. A VARIANT column's keys are not in any schema — that is
//     the point of a bag — so they are discovered from the data, and which
//     columns are VARIANT comes from the shape.
//   - the casts. `TRY_CAST(ts AS DOUBLE)` is not a NULL, it is
//     `[1006] unable to cast type Timestamp to type Float64` and it fails the
//     whole statement. Every cast here therefore goes through `::VARCHAR`
//     first, which is legal on every type measured — timestamp, varchar and
//     variant — and makes one uniform expression work for all of them.
func ProfileProbe(shape Shape, window ProbeWindow, maxKeys int) (string, error) {
	if shape.Table == "" {
		return "", fmt.Errorf("introspect: the shape names no table")
	}
	if maxKeys <= 0 {
		maxKeys = 32
	}

	cols := shape.profilable()
	if len(cols) == 0 {
		return "", fmt.Errorf("introspect: %s has no columns to profile", shape.Table)
	}

	// A bound, not a LIMIT. A LIMIT would be applied after the aggregate and
	// profile the whole table anyway.
	where, desc := window.predicate(shape.timeColumn())

	var b strings.Builder
	b.WriteString(probeHeader("profile", shape.Table))
	if where == "" {
		b.WriteString("-- NOTE: no bound, so this profiles the WHOLE table. Pass -since/-until or\n")
		b.WriteString("--       -window, or add a bound by hand if that is too expensive.\n")
	} else {
		fmt.Fprintf(&b, "-- Bounded to %s.\n", desc)
	}
	b.WriteString(`-- Run all statements and append the output to the SAME file as the shape probe,
-- or save it separately and pass both to ` + "`introspect build`" + `.

`)
	b.WriteString(sectionMarker("profile", shape.Table) + "\n")
	b.WriteString(windowMarker(window) + "\n")

	branches := make([]string, 0, len(cols))
	for _, c := range cols {
		branches = append(branches, profileBranch(c.Name, quoteIdent(c.Name)+"::VARCHAR",
			shape.Table, where))
	}
	fmt.Fprintf(&b, "%s;\n", strings.Join(branches, "\nUNION ALL\n"))

	// Bag keys get their own statement per bag, because the key list has to be
	// discovered before it can be profiled and a single statement cannot do
	// both. The discovery query is emitted alongside so the operator can see
	// what is being asked; its output is not consumed.
	for _, c := range shape.Columns {
		if c.Type != "variant" || c.Derived != "" {
			continue
		}
		fmt.Fprintf(&b, `
-- Bag %[1]s: the %[2]d most common keys, and their value profiles.
-- A bag key's type is not in any schema, so it is read from the data. That is
-- what tells you a key like `+"`duration`"+` holds `+"`47.823614ms`"+` and that a bound on
-- it therefore answers with the 2 rows of 2,046 that happen to cast.
%[5]s
SELECT '%[6]s|%[7]s|bagkey|%[1]s|' || f.key || '|' || to_string(count(*)) AS lake_search_probe
FROM %[3]s, LATERAL FLATTEN(input => %[1]s) f%[4]s
GROUP BY f.key
ORDER BY count(*) DESC
LIMIT %[2]d;
`, quoteIdent(c.Name), maxKeys, shape.Table, where,
			sectionMarker("bagkeys", shape.Table), probeMarker, ProbeVersion)

		fmt.Fprintf(&b, `
-- Then profile those keys. Paste the key names from the previous statement into
-- `+"`introspect profile -keys k1,k2,…`"+` to have this generated for you, or edit here.
`)
	}
	return b.String(), nil
}

// BagKeyProfileProbe emits the value profile for a named set of bag keys.
//
// It is a separate call because the key list comes from the bag-key discovery
// statement, which is one round trip further out. Keeping it separate means the
// operator can profile only the keys they care about on a table with 594 of
// them — which is how many logs.k8s_logs_v2 actually has.
func BagKeyProfileProbe(shape Shape, bag string, keys []string, window ProbeWindow) (string, error) {
	if len(keys) == 0 {
		return "", fmt.Errorf("introspect: no bag keys given")
	}
	if bag == "" {
		if b := shape.loneBag(); b != "" {
			bag = b
		} else {
			return "", fmt.Errorf("introspect: %s has no single VARIANT column; name one with -bag",
				shape.Table)
		}
	}
	where, _ := window.predicate(shape.timeColumn())

	var b strings.Builder
	b.WriteString(probeHeader("profile", shape.Table))
	b.WriteString(sectionMarker("profile", shape.Table) + "\n")
	b.WriteString(windowMarker(window) + "\n")
	branches := make([]string, 0, len(keys))
	for _, k := range keys {
		expr := fmt.Sprintf("%s['%s']::VARCHAR", quoteIdent(bag), escapeString(k))
		branches = append(branches, profileBranch(bag+"."+k, expr, shape.Table, where))
	}
	fmt.Fprintf(&b, "%s;\n", strings.Join(branches, "\nUNION ALL\n"))
	return b.String(), nil
}

// profileBranch renders one column's profile as a single marked string, so no
// client's column formatting can garble it.
//
// Seven numbers, and each one decides something:
//
//	non_null / scanned   whether the column is populated at all
//	numeric_ok           the Number role, and the mixed-column trap
//	timestamp_ok         a VARCHAR that is really a timestamp
//	distinct             facet or identifier
//	min_len / max_len    a body candidate against a code or an id
func profileBranch(label, expr, table, where string) string {
	return fmt.Sprintf(`SELECT '%s|%s|prof|%s|'
    || to_string(count(*))                                  || '|'
    || to_string(count(%s))                                 || '|'
    || to_string(count(TRY_CAST(%s AS DOUBLE)))              || '|'
    || to_string(count(TRY_CAST(%s AS TIMESTAMP)))           || '|'
    || to_string(approx_count_distinct(%s))                  || '|'
    || to_string(coalesce(min(length(%s)), 0))               || '|'
    || to_string(coalesce(max(length(%s)), 0)) AS lake_search_probe
FROM %s%s`,
		probeMarker, ProbeVersion, escapeString(label),
		expr, expr, expr, expr, expr, expr, table, where)
}

// VerifyProbe emits one statement that binds every column expression in a
// descriptor without reading a row.
//
// This is what buys back most of the per-expression validation an interactive
// form gets for free. A hand-edited column expression is otherwise unchecked
// until someone runs a real query; `WHERE 1=0` makes the engine resolve every
// expression and return nothing, so the statement either binds or names the
// column that does not.
//
// It also re-emits DESCRIBE, so the consumer can compare the column digest and
// report that the table has gained or lost a column since the descriptor was
// written.
func VerifyProbe(s Schema) string {
	seen := map[string]bool{}
	var exprs []string
	for _, name := range sortedFieldNames(s) {
		f := s.Fields[name]
		if seen[f.Column] {
			continue
		}
		seen[f.Column] = true
		exprs = append(exprs, fmt.Sprintf("  %s AS %s", f.Column, quoteIdent(name)))
	}
	for _, b := range s.Bags {
		if seen[b.Column] {
			continue
		}
		seen[b.Column] = true
		exprs = append(exprs, fmt.Sprintf("  %s AS %s", quoteIdent(b.Column), quoteIdent(b.Column)))
	}

	var b strings.Builder
	b.WriteString(probeHeader("verify", s.Table))
	b.WriteString(`-- Binds every column expression this descriptor names, and reads no rows.
-- It either returns an empty result — everything resolves — or names the
-- expression that does not. This is the check an interactive form gets by
-- EXPLAINing each field as it is typed; here it costs one paste.

`)
	fmt.Fprintf(&b, "SELECT\n%s\nFROM %s\nWHERE 1=0;\n\n", strings.Join(exprs, ",\n"), s.Table)
	fmt.Fprintf(&b, `-- And the drift check. Save this output and feed it back:
--   lake-search introspect verify -preset … -shape <this output>
-- Binding alone does NOT catch drift, and that is measured rather than assumed:
-- a column added to the table is simply not mentioned by the statement above, so
-- it binds cleanly while every query naming that column is routed to the
-- attribute bag and matches nothing. Only comparing the column list finds it.
%s
DESCRIBE %s;

%s
SHOW CREATE TABLE %s;
`, sectionMarker("columns", s.Table), s.Table,
		sectionMarker("create", s.Table), s.Table)
	b.WriteString(BagDriftProbe(s, ProbeWindow{Hours: "24"}, ProbeWindow{}))
	return b.String()
}

// maxDeclaredKeysProbed caps the per-key statements one bag contributes, so a
// descriptor declaring hundreds of typed keys cannot emit unbounded SQL.
const maxDeclaredKeysProbed = 64

// BagDriftProbe emits the statements that make a bag's keys visible to drift
// detection.
//
// # Two windows, deliberately, because the questions are different
//
// The per-key statistics are bounded and RECENT, because "has this declared key
// stopped appearing" and "have its values stopped casting" are questions about
// now. An unbounded window answers them wrongly: a key that stopped being
// written an hour ago is still all over the history, so the check would find it
// and report nothing.
//
// The name enumeration is UNBOUNDED, because "is there a key shadowing a
// declared name" is a question about existence, and a bounded window can hide a
// key's existence entirely. That is measured rather than reasoned: `raw` is
// forward-only and profiles 0 of 997,592 rows over `ts < 2026-08-20 00:00:00`,
// so a historical window says it does not exist. A key present on three rows is
// still a hazard — `kv['body']` is exactly three rows on the live table — and
// missing it is the failure mode.
//
// Neither bound has an upper edge in the future. A `ts <` bound whose instant
// has not happened yet reads as frozen and is not, which is a mistake this repo
// has made twice.
//
// # Why the enumeration is name-directed rather than top-N
//
// Keys are unbounded in number, so the obvious shape is "the N most common keys,
// and say it was limited". This asks a narrower question instead: it lists only
// the keys whose names something in the descriptor already claims — a column, a
// field, an alias, a typed key — with an exact IN list. That is COMPLETE for the
// question being asked rather than truncated, so its output can never be
// mistaken for "these are all the keys": it never claims to be about all keys.
// A top-N enumeration would be both more expensive and less conclusive, because
// the dangerous shadowing key is a rare one — three rows, not three hundred
// thousand — and it is exactly what a top-N drops.
//
// What that leaves invisible is stated in the report rather than papered over: a
// key that is neither declared nor named after anything declared is not looked
// at, and a new key is not drift at all. A schemaless bag gains keys constantly;
// reporting those would be noise, and a check that cries wolf gets turned off.
func BagDriftProbe(s Schema, window ProbeWindow, enum ProbeWindow) string {
	if len(s.Bags) == 0 {
		return ""
	}

	// The names something in the descriptor claims. Sorted so the emitted SQL
	// is stable between runs and a diff of two probes is readable.
	claimed := map[string]bool{}
	for name, f := range s.Fields {
		claimed[strings.ToLower(name)] = true
		if isBareIdent(f.Column) {
			claimed[strings.ToLower(f.Column)] = true
		}
	}
	for _, b := range s.Bags {
		for k := range b.Keys {
			claimed[strings.ToLower(k)] = true
		}
	}
	names := make([]string, 0, len(claimed))
	for n := range claimed {
		names = append(names, n)
	}
	sort.Strings(names)

	where, desc := window.predicate(s.TimeColumn())
	enumWhere, enumDesc := enum.predicate(s.TimeColumn())
	typed := 0
	for _, b := range s.Bags {
		typed += len(b.Keys)
	}
	switch {
	case typed == 0:
		// Saying "statistics cover the last 24 hours" when no statement was
		// emitted would claim a measurement that did not happen.
		desc = "not measured: this descriptor types no bag key, so there is nothing to check " +
			"per key. Only the shadowing enumeration below ran"
	case desc == "":
		// Unbounded, and the probe is the only place that knows WHY: it has the
		// schema, so it can distinguish "no time column to bound on" from a
		// caller that passed no window.
		if s.TimeColumn() == "" {
			desc = "the whole table — this schema declares no time column, so there is nothing " +
				"to bound on, and a key that vanished recently is still visible in history " +
				"and cannot be detected here"
		} else {
			desc = "the whole table — no bound was requested, so a key that vanished recently " +
				"is still visible in history and cannot be detected here"
		}
	}
	if enumDesc == "" {
		enumDesc = "no bound (existence needs the widest window: a forward-only key is absent " +
			"from history and a rare key is absent from a recent window)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, `
-- The attribute bag. DESCRIBE does not enumerate a VARIANT's keys, so without
-- these statements a declared key that vanished, changed shape, or stopped
-- casting is invisible to the drift check above.
%s
%s
%s
`, sectionMarker("bagkeys", s.Table),
		bagWindowMarker("stats", orNoWindow(desc)),
		bagWindowMarker("enum", enumDesc))

	for _, bag := range s.Bags {
		keys := make([]string, 0, len(bag.Keys))
		for k := range bag.Keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > maxDeclaredKeysProbed {
			fmt.Fprintf(&b, "%s\n", bagLimitMarker(fmt.Sprintf(
				"bag %s declares %d typed keys — only the first %d are checked",
				bag.Column, len(keys), maxDeclaredKeysProbed)))
			keys = keys[:maxDeclaredKeysProbed]
		}
		if len(keys) > 0 {
			branches := make([]string, 0, len(keys))
			for _, k := range keys {
				branches = append(branches, bagStatBranch(bag.Column, k, s.Table, where))
			}
			fmt.Fprintf(&b, "\n-- Declared keys of %s, over %s.\n%s;\n",
				bag.Column, orNoWindow(desc), strings.Join(branches, "\nUNION ALL\n"))
		}

		if len(names) > 0 {
			quoted := make([]string, 0, len(names))
			for _, n := range names {
				quoted = append(quoted, "'"+escapeString(n)+"'")
			}
			fmt.Fprintf(&b, `
-- Keys of %[1]s whose name something in this descriptor already claims. An exact
-- list, not a top-N: the dangerous one is rare, and a top-N drops exactly that.
SELECT '%[5]s|%[6]s|bagname|%[1]s|' || f.key || '|' || to_string(count(*)) AS lake_search_probe
FROM %[2]s, LATERAL FLATTEN(input => %[1]s) f%[4]s
WHERE lower(f.key) IN (%[3]s)
GROUP BY f.key
ORDER BY count(*) DESC;
`, quoteIdent(bag.Column), s.Table, strings.Join(quoted, ", "), enumWhere,
				probeMarker, ProbeVersion)
		}
	}
	return b.String()
}

// bagStatBranch renders one declared key's observation as a single marked
// string, so no client's column formatting can garble it.
func bagStatBranch(bag, key, table, where string) string {
	v := fmt.Sprintf("%s['%s']", quoteIdent(bag), escapeString(key))
	return fmt.Sprintf(`SELECT '%s|%s|bagstat|%s|%s|'
    || to_string(count(*))                                      || '|'
    || to_string(count(%s))                                     || '|'
    || to_string(count(TRY_CAST(%s::VARCHAR AS DOUBLE)))         || '|'
    || to_string(count_if(json_typeof(%s) = 'OBJECT'))           || '|'
    || to_string(count_if(json_typeof(%s) = 'ARRAY')) AS lake_search_probe
FROM %s%s`,
		probeMarker, ProbeVersion, escapeString(bag), escapeString(key),
		v, v, v, v, table, where)
}

func bagWindowMarker(scope, desc string) string {
	return fmt.Sprintf("SELECT '%s|%s|bagwindow|%s|%s' AS lake_search_probe;",
		probeMarker, ProbeVersion, scope, escapeString(desc))
}

// bagLimitMarker announces a capped key list.
//
// The payload must contain no semicolon. Every instruction in this project says
// to strip `--` lines and then split on `;`, and this marker's text once did
// contain one: the statement was cut in half, both halves failed [1005], the cap
// was never announced, and the operator got two spurious parse errors on an
// otherwise clean probe. A marker whose whole job is to prevent a silent partial
// answer must not itself go missing.
//
// Enforced rather than remembered: the semicolon is replaced here, so a future
// caller cannot reintroduce it by writing a different sentence.
func bagLimitMarker(desc string) string {
	return fmt.Sprintf("SELECT '%s|%s|baglimit|%s' AS lake_search_probe;",
		probeMarker, ProbeVersion, escapeString(stripSemicolons(desc)))
}

// stripSemicolons makes a string safe to carry inside an emitted SQL literal
// under the strip-then-split procedure this project documents everywhere.
func stripSemicolons(s string) string {
	return strings.ReplaceAll(s, ";", " —")
}

// Drift reports what the table has that the descriptor does not, and vice versa.
//
// This exists because binding is not enough, and the gap is not theoretical. A
// plain column `raw` was added to logs.k8s_logs and the Vector pipeline started
// populating it; no descriptor declared it. The bind statement still succeeded —
// it never mentions a column nobody declared — while `raw:hello` compiled to
// kv['raw'], and `kv['raw'] IS NOT NULL` was 0 against `raw IS NOT NULL` of
// 3,711. So the query was silently empty forever AND the compiler's warning
// asserted something false, that `raw` is not a column. Comparing the column
// lists is the only thing that finds that.
// DriftReport separates findings that mean something CHANGED from standing
// facts that were always true, because the two want different reactions.
//
// The split is not tidiness. A shadowing bag key is a real hazard worth saying
// out loud — `kv['namespace']` exists on tens of thousands of rows while the
// descriptor declares a `namespace` COLUMN, so the obvious spelling cannot see
// them — but it is not drift, and reporting it as drift would make `verify`
// fail on every run against a correct descriptor. A check that cries wolf gets
// turned off, which is worse than not having it. So hazards print and do not
// affect the verdict; drift prints and does.
type DriftReport struct {
	// Drift is what changed. Non-empty means act.
	Drift []string

	// Hazards are standing ambiguities: true now, true when the descriptor was
	// written, and worth knowing.
	Hazards []string

	// Limits are the things this probe could not see, stated so that silence is
	// never mistaken for absence.
	Limits []string
}

// Findings is every line, drift first, for a caller that just wants to print.
func (r DriftReport) Findings() []string {
	out := append([]string{}, r.Drift...)
	out = append(out, r.Hazards...)
	return out
}

// Drift reports the findings as a flat list of things that CHANGED, for callers
// that want the verdict and nothing else.
func Drift(shape Shape, s Schema, digest string) []string {
	return DriftDetail(shape, s, digest).Drift
}

// DriftDetail is Drift with the hazards and limits kept separate.
func DriftDetail(shape Shape, s Schema, digest string) DriftReport {
	var rep DriftReport
	var out []string

	declared := map[string]bool{}
	for _, f := range s.Fields {
		declared[strings.ToLower(f.Column)] = true
	}
	for _, b := range s.Bags {
		declared[strings.ToLower(b.Column)] = true
	}

	for _, c := range shape.Columns {
		if declared[strings.ToLower(c.Name)] {
			continue
		}
		// The direction that matters. An undeclared column is not merely
		// missing: with a bag configured, every query naming it is routed into
		// the bag and answers nothing, and the advisory says the name "is not a
		// column", which is false.
		where := "it will be read from the attribute bag and match nothing"
		if len(s.Bags) == 0 {
			where = "a query naming it will be refused as an unknown field"
		}
		out = append(out, fmt.Sprintf(
			"column %q (%s) exists in the table and is NOT declared: %s", c.Name, c.RawType, where))
	}

	// Sorted so two runs over the same input report in the same order; a Go map
	// iterates randomly and a diff that reorders itself is a diff nobody reads.
	for _, name := range sortedFieldNames(s) {
		f := s.Fields[name]
		// Only bare column references are checked here; an expression is
		// checked by the bind statement instead.
		if !isBareIdent(f.Column) {
			continue
		}
		col, ok := shape.column(f.Column)
		if !ok {
			out = append(out, fmt.Sprintf(
				"field %q reads column %q, which the table no longer has", name, f.Column))
			continue
		}
		// The type. This was the blind spot: `ts` becoming a VARCHAR or `kv`
		// ceasing to be a VARIANT was reported as "no drift" against a preset,
		// because only an introspected descriptor carries a digest and nothing
		// else looked. The type is already in the DESCRIBE row being parsed —
		// it is quoted in the "NOT declared" message above — so comparing it is
		// nearly free, and it changes what the emitted SQL MEANS:
		// `ts:>2026-08-18T22:30:00Z` still compiles to a string comparison
		// against a VARCHAR with nothing said.
		if want := acceptableTypes(f.Kind); !want[col.Type] {
			out = append(out, fmt.Sprintf(
				"field %q is kind %q but column %q is %s in the table: a comparison on it is "+
					"compiled for %s and evaluated as %s, which changes what the answer means "+
					"rather than how fast it arrives",
				name, KindName(f.Kind), f.Column, col.RawType, KindName(f.Kind), col.Type))
		}
		// A computed column the table has redefined. `line` is the case that
		// matters: change its expression and every bare word searches something
		// else.
		if f.Derived != "" && col.Derived != "" && !sameExpr(f.Derived, col.Derived) {
			out = append(out, fmt.Sprintf(
				"field %q is a STORED computed column and the table's definition of it has "+
					"changed:\n      descriptor: %s\n      table:      %s",
				name, f.Derived, col.Derived))
		}
		if f.Derived != "" && col.Derived == "" {
			out = append(out, fmt.Sprintf(
				"field %q is declared as a STORED computed column but %q is a plain column in "+
					"the table now, so nothing maintains its value", name, f.Column))
		}
		if f.Derived == "" && col.Derived != "" {
			out = append(out, fmt.Sprintf(
				"column %q is a STORED computed column in the table and the descriptor does not "+
					"say so, so a writer may try to set it", f.Column))
		}
	}
	for _, bag := range s.Bags {
		col, ok := shape.column(bag.Column)
		if !ok {
			out = append(out, fmt.Sprintf(
				"bag %q is not a column of the table any more", bag.Column))
			continue
		}
		if col.Type != "variant" {
			out = append(out, fmt.Sprintf(
				"bag %q is %s in the table, not a VARIANT: every undeclared field name is read "+
					"from it as a JSON path, which a %s column cannot answer",
				bag.Column, col.RawType, col.Type))
		}
	}

	// Index drift. An index that grew is why a column can become searchable
	// without anyone editing a descriptor, and an index that shrank or lost its
	// filters changes answers rather than speed.
	byName := map[string]Index{}
	for _, ix := range shape.Indexes {
		byName[ix.Name] = ix
	}
	for _, want := range s.Indexes {
		got, ok := byName[want.Name]
		if !ok {
			out = append(out, fmt.Sprintf(
				"index %s is declared but the table does not have it: every field it makes "+
					"full-text is now unindexed, and a term on one of them cannot be compiled",
				want.Name))
			continue
		}
		if !sameSet(got.Columns, want.Columns) {
			out = append(out, fmt.Sprintf(
				"index %s covers (%s) in the table but (%s) in the descriptor",
				want.Name, strings.Join(got.Columns, ", "), strings.Join(want.Columns, ", ")))
		}
		if !sameSet(got.Filters, want.Filters) {
			out = append(out, fmt.Sprintf(
				"index %s has filters '%s' in the table but '%s' in the descriptor — a filter "+
					"declared that the index lacks sends ordinary words to an index that "+
					"deletes them, and one the index has but the descriptor does not sends "+
					"them to a needless scan",
				want.Name, strings.Join(got.Filters, ","), strings.Join(want.Filters, ",")))
		}
		if !strings.EqualFold(got.Tokenizer, want.Tokenizer) {
			out = append(out, fmt.Sprintf("index %s tokenizer is %q in the table and %q in the "+
				"descriptor", want.Name, got.Tokenizer, want.Tokenizer))
		}
		delete(byName, want.Name)
	}
	for _, ix := range byName {
		out = append(out, fmt.Sprintf(
			"index %s (%s %s) exists in the table and is not declared: the columns it covers "+
				"are searched by scan rather than by the index, or in the NGRAM case a wildcard "+
				"is warned about as a full scan when it is not one",
			ix.Name, ix.Kind, strings.Join(ix.Columns, ", ")))
	}

	if digest != "" && shape.Digest != digest {
		out = append(out, fmt.Sprintf(
			"the column-list digest has changed: %s at probe time, %s now", digest, shape.Digest))
	}

	rep.Drift = out
	bagDrift(&rep, shape, s)
	sort.Strings(rep.Drift)
	sort.Strings(rep.Hazards)
	sort.Strings(rep.Limits)
	return rep
}

// observedSpellings returns every spelling the data uses for a declared key whose
// own spelling found nothing, sorted, or nil when the data does not have the
// name in any case.
//
// It returns a LIST, not a match, and that is the whole point. This is the one
// place a case-insensitive comparison is correct — the question is "did someone
// type the right name in the wrong case", which is about human spelling rather
// than key identity — but a case-insensitive comparison over real bag keys can
// have more than one right answer, and picking one silently is the failure this
// function exists to report. Measured on logs.k8s_logs: `kv['safepoint']` is
// present on 1,740 rows and `kv['safePoint']` on 459. Both are live. A
// case-insensitive lookup for "safepoint" therefore has two answers, and a
// helper that returned the first one would return whichever the map iterated
// first.
func observedSpellings(shape Shape, bag, declared string) []string {
	prefix := bag + "."
	var out []string
	for full, rows := range shape.BagNames {
		if !strings.HasPrefix(full, prefix) || rows == 0 {
			continue
		}
		observed := full[len(prefix):]
		if observed != declared && strings.EqualFold(observed, declared) {
			out = append(out, observed)
		}
	}
	sort.Strings(out)
	return out
}

// bagDrift compares the descriptor's claims about the attribute bag against what
// the bag actually holds.
//
// Four things, and the priority order is by how silently each one fails.
func bagDrift(rep *DriftReport, shape Shape, s Schema) {
	if len(s.Bags) == 0 {
		return
	}
	if shape.BagKeys == nil && shape.BagNames == nil {
		rep.Limits = append(rep.Limits, "the attribute bag was not probed, so a declared key "+
			"that vanished, changed shape or stopped casting is invisible here. Re-run the "+
			"verify probe: DESCRIBE does not enumerate a VARIANT's keys, so those statements "+
			"are the only thing that can see them")
		return
	}
	if shape.BagLimited != "" {
		rep.Limits = append(rep.Limits, shape.BagLimited)
	}
	rep.Limits = append(rep.Limits, fmt.Sprintf(
		"bag key statistics cover: %s. The name enumeration covers: %s.",
		orNoWindow(shape.BagWindow), orNoWindow(shape.BagEnumWindow)))
	// Both directions of the window's blind spot, because disclosing only one
	// of them is what made the second invisible. The absence direction was
	// stated; the presence direction was not, and it is the one that returns a
	// wrong answer rather than no answer.
	rep.Limits = append(rep.Limits,
		"a bounded statistics window sees only that slice: a declared key ABSENT from it may "+
			"still exist in history, and — the direction that costs more — a key PRESENT and "+
			"clean in it may be dirty outside it. A numeric key that casts on every recent row "+
			"and on none of the older ones reports nothing here while a bound on it silently "+
			"drops the older rows; the compiler's own cast advisory fires on that query and "+
			"hands over a count predicate, which is the check that does see it")
	rep.Limits = append(rep.Limits,
		"only the names this descriptor already claims were enumerated, so a key named after "+
			"nothing declared was not looked at — and a new key is not drift, because a "+
			"schemaless bag gains keys constantly")

	// (1) A declared key that is no longer in the data. Its typed comparisons
	// then match nothing, silently and forever — the bag-level twin of the
	// column that nobody declared.
	// (2) A declared numeric key whose values stopped casting: round 4's silent
	// row drop arriving through a data change rather than a code change. The
	// ratio, not a boolean, because 3 of 250,036 and 923 of 6,604 want
	// different reactions.
	// (4) A declared key that stopped being a scalar. A path lookup against an
	// object or an array is a comparison against a rendered JSON document, so
	// `k:v` means something else than it did.
	for _, bag := range s.Bags {
		keys := make([]string, 0, len(bag.Keys))
		for k := range bag.Keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			kind := bag.Keys[key]
			st, ok := shape.BagKeys[bag.Column+"."+key]
			if !ok {
				rep.Limits = append(rep.Limits, fmt.Sprintf(
					"declared key %s['%s'] was not in the probe output, so nothing is known "+
						"about it", bag.Column, key))
				continue
			}
			if st.Scanned == 0 {
				rep.Limits = append(rep.Limits, fmt.Sprintf(
					"the statistics window is empty, so %s['%s'] could not be checked",
					bag.Column, key))
				continue
			}
			if st.Present == 0 {
				// Before calling it vanished, check whether it is merely spelled
				// differently. A VARIANT key is case-sensitive, so declaring
				// `tableid` against data holding `tableID` is a real and silent
				// mistake a person can make by hand — and it is a different
				// mistake from the key going away, so it gets a different
				// sentence. The enumeration observed the data's own spelling,
				// which is what makes this answerable.
				if actual := observedSpellings(shape, bag.Column, key); len(actual) > 0 {
					seen := make([]string, 0, len(actual))
					for _, a := range actual {
						seen = append(seen, fmt.Sprintf("%s['%s'] on %d rows",
							bag.Column, a, shape.BagNames[bag.Column+"."+a]))
					}
					which := fmt.Sprintf("Re-declare it as %q", actual[0])
					if len(actual) > 1 {
						// More than one right answer, so there is no right
						// answer to pick. Say which ones and make the person
						// choose: on the live table `safepoint` and `safePoint`
						// are both real keys, so a tool that chose for them
						// would be guessing at data.
						which = fmt.Sprintf("There are %d of them, so nothing can choose for "+
							"you — declare the one you mean", len(actual))
					}
					rep.Hazards = append(rep.Hazards, fmt.Sprintf(
						"declared key %s['%s'] is absent, but the data has the same name in a "+
							"different case: %s. VARIANT keys are case-sensitive on this engine, "+
							"so the declaration reaches nothing and `%s:value` queries a key "+
							"that does not exist. %s",
						bag.Column, key, strings.Join(seen, " and "), key, which))
					continue
				}
				rep.Drift = append(rep.Drift, fmt.Sprintf(
					"declared key %s['%s'] appears in 0 of %d rows over %s: every comparison "+
						"the descriptor types for it matches nothing, silently. Either the "+
						"writer stopped emitting it or the declaration outlived it",
					bag.Column, key, st.Scanned, orNoWindow(shape.BagWindow)))
				continue
			}
			if kind == Number && st.NumericOK < st.Present {
				rep.Drift = append(rep.Drift, fmt.Sprintf(
					"declared key %s['%s'] is typed \"number\" but only %d of its %d values "+
						"cast over %s (%.1f%%): a bound on it silently drops the other %d rows, "+
						"and an equality compares them as a number they are not. Retype it as "+
						"a string, or fix the writer",
					bag.Column, key, st.NumericOK, st.Present, orNoWindow(shape.BagWindow),
					100*float64(st.NumericOK)/float64(st.Present), st.Present-st.NumericOK))
			}
			if st.Objects > 0 || st.Arrays > 0 {
				rep.Drift = append(rep.Drift, fmt.Sprintf(
					"declared key %s['%s'] is not a scalar on %d of its %d rows (%d object, %d "+
						"array) over %s: `%s:value` compares against the rendered JSON document "+
						"rather than against a value, so it matches nothing a reader would "+
						"expect. Reach inside it with a dotted path instead",
					bag.Column, key, st.Objects+st.Arrays, st.Present, st.Objects, st.Arrays,
					orNoWindow(shape.BagWindow), key))
			}
		}
	}

	// (3) Shadowing, both directions.
	//
	// This is how `raw` bit us: the descriptor routed a name to the bag while
	// the data lived in a column of that name. The two directions differ in
	// severity, and conflating them is what would make this check noise.
	byName := make([]string, 0, len(shape.BagNames))
	for k := range shape.BagNames {
		byName = append(byName, k)
	}
	sort.Strings(byName)
	for _, full := range byName {
		rows := shape.BagNames[full]
		if rows == 0 {
			continue
		}
		i := strings.Index(full, ".")
		if i < 0 {
			continue
		}
		bagCol, key := full[:i], full[i+1:]

		// The descriptor's typed keys: a column has appeared with the name of a
		// declared key. That IS drift, because the column now wins and the
		// typed key becomes unreachable by its own spelling.
		typed := false
		for _, bag := range s.Bags {
			if !strings.EqualFold(bag.Column, bagCol) {
				continue
			}
			if _, ok := bag.Keys[key]; ok {
				typed = true
			}
		}
		f, declaredAsField := s.Fields[strings.ToLower(key)]
		if typed && declaredAsField {
			rep.Drift = append(rep.Drift, fmt.Sprintf(
				"%s['%s'] exists on %d rows AND %q is a declared field reading %q: the field "+
					"wins, so the typed bag key is unreachable by its own name and only "+
					"`%s.%s:value` reaches those rows",
				bagCol, key, rows, key, f.Column, bagCol, key))
			continue
		}
		if declaredAsField {
			// The standing hazard. The field wins — documented precedence — so
			// this is not a wrong answer, but the bag rows are invisible to the
			// obvious spelling and a reader should know.
			rep.Hazards = append(rep.Hazards, fmt.Sprintf(
				"%s['%s'] exists on %d rows and %q is also a declared field reading %q. The "+
					"field wins, so those bag rows are reachable only as `%s.%s:value`. Not a "+
					"wrong answer, but the obvious spelling cannot see them",
				bagCol, key, rows, key, f.Column, bagCol, key))
			continue
		}
		// Neither a field nor a typed key, yet the enumeration asked about it,
		// which means it matched a bare column name that nothing declares. The
		// column check above has already reported the undeclared column; this
		// adds what makes it dangerous rather than merely untidy.
		if _, isColumn := shape.column(key); isColumn {
			rep.Drift = append(rep.Drift, fmt.Sprintf(
				"%s['%s'] exists on %d rows and %q is ALSO an undeclared column of the table: "+
					"a query naming it reaches the bag rows and never the column's, which is "+
					"the shape that made `raw` answer nothing forever",
				bagCol, key, rows, key))
		}
	}
}

// isBareIdent reports whether a column expression is just a column name, so an
// expression-valued field is left to the bind statement.
// acceptableTypes is the set of declared column types a field of this kind can
// legitimately read.
//
// Text and String both accept `boolean`, because there is no Bool kind and a
// boolean column is compared as a string — that is a documented choice, not
// drift.
func acceptableTypes(k Kind) map[string]bool {
	switch k {
	case Number:
		return map[string]bool{"number": true}
	case Timestamp:
		return map[string]bool{"timestamp": true}
	default:
		return map[string]bool{"string": true, "boolean": true}
	}
}

// sameExpr compares two SQL expressions ignoring whitespace and the difference
// between this engine's own renderings of a string cast, which SHOW CREATE TABLE
// prints as `::STRING` where a descriptor is likely to say `::VARCHAR`. Neither
// difference is drift.
func sameExpr(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "::varchar", "::string")
		var out []rune
		for _, r := range s {
			if r != ' ' && r != '\t' && r != '\n' && r != '`' {
				out = append(out, r)
			}
		}
		return string(out)
	}
	return norm(a) == norm(b)
}

func isBareIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[strings.ToLower(x)]++
	}
	for _, x := range b {
		seen[strings.ToLower(x)]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func sortedFieldNames(s Schema) []string {
	names := make([]string, 0, len(s.Fields))
	for n := range s.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func probeHeader(kind, table string) string {
	return fmt.Sprintf(`-- lake-search introspect: %s probe (%s) for %s
--
-- Every line the consumer reads is tagged `+"`%s|%s|…`"+`, so a client banner, a
-- column header, a box-drawing border or an ANSI colour code is skipped rather
-- than misparsed. Keep the tagged lines; the rest does not matter.
`, kind, ProbeVersion, table, probeMarker, ProbeVersion)
}

// windowMarker puts the profile's bound INTO its output, so `build` reads the
// window rather than being told it a second time.
//
// It exists because the default path lied. `profile` bounds to one hour by
// default, `build`'s -window defaults to empty, the output carried no window,
// and the printed instructions never said to repeat the flag — so a user doing
// exactly what the tool printed got a descriptor whose provenance claimed the
// profile covered the whole table when it covered one hour. The machinery for
// the honest answer existed and the default path defeated it.
func windowMarker(w ProbeWindow) string {
	return fmt.Sprintf("SELECT '%s|%s|window|%s' AS lake_search_probe;",
		probeMarker, ProbeVersion, escapeString(w.Describe()))
}

func sectionMarker(kind, table string) string {
	return fmt.Sprintf("SELECT '%s|%s|section|%s|%s' AS lake_search_probe;",
		probeMarker, ProbeVersion, kind, escapeString(table))
}

// quoteIdent leaves ordinary identifiers alone and backtick-quotes anything
// else. A column name from a log table can be a reserved word — `format` is one
// on this engine — and an unquoted one is a parse error rather than a lookup
// failure.
func quoteIdent(name string) string {
	ok := name != ""
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			ok = false
		}
	}
	if ok && !reservedIdents[strings.ToLower(name)] {
		return name
	}
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// reservedIdents is not the engine's whole reserved list; it is the words a log
// table plausibly uses as a column name. `format` is here because the collector
// in pipeline/ writes exactly that key.
var reservedIdents = map[string]bool{
	"format": true, "table": true, "index": true, "order": true, "group": true,
	"select": true, "from": true, "where": true, "limit": true, "value": true,
	"values": true, "key": true, "type": true, "level": true, "user": true,
	"timestamp": true, "time": true, "date": true, "interval": true,
}

// ---------------------------------------------------------------- consuming

var (
	// `SYNC INVERTED INDEX idx_msg (msg, kv, line) filters = '…', tokenizer = 'english',`
	reIndex     = regexp.MustCompile(`(?i)(?:SYNC\s+|ASYNC\s+)?(INVERTED|NGRAM)\s+INDEX\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)]*)\)([^\n]*)`)
	reTokenizer = regexp.MustCompile(`(?i)tokenizer\s*=\s*'([^']*)'`)
	reFilters   = regexp.MustCompile(`(?i)filters\s*=\s*'([^']*)'`)
	// Greedy to the last `)` on the line, because a cluster key contains
	// function calls: `CLUSTER BY (to_date(ts), component)` stops at the first
	// `)` under a lazy match and yields `to_date(ts`, which then appears
	// verbatim in the provenance note. CLUSTER BY is the last clause SHOW
	// CREATE TABLE emits, so anchoring at end-of-line is safe.
	reClusterBy = regexp.MustCompile(`(?mi)CLUSTER\s+BY\s*\((.*)\)[^()]*$`)
	// The line that closes SHOW CREATE TABLE's column list.
	reCloseParen = regexp.MustCompile(`(?m)^\s*\)`)
	// `STORED COMPUTED COLUMN `expr`` from DESCRIBE, or `AS (expr) STORED` from
	// SHOW CREATE TABLE.
	reComputed = regexp.MustCompile("(?i)(?:STORED|VIRTUAL)\\s+COMPUTED\\s+COLUMN\\s+`(.*)`\\s*$")
)

// ParseShape reads the shape probe's output.
//
// It is a tolerant line scanner rather than a parser, because the input is
// whatever an unknown client printed. A section marker switches mode; lines
// that do not fit the current mode's shape are skipped, not rejected. The one
// thing it is strict about is the table name in the marker, because a paste from
// the wrong table is the failure mode that would otherwise produce a confident,
// wrong descriptor.
func ParseShape(out, wantTable string) (Shape, error) {
	sh := Shape{Table: wantTable}
	mode := ""
	var createLines []string
	sawSection := false

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(stripANSI(raw), " \t\r")
		if kind, table, ok := parseSection(line); ok {
			sawSection = true
			if wantTable != "" && !sameTable(table, wantTable) {
				return Shape{}, fmt.Errorf(
					"introspect: this output is for table %q but %q was asked for — "+
						"probe output and table must match, or the descriptor describes the wrong table",
					table, wantTable)
			}
			if sh.Table == "" {
				sh.Table = table
			}
			mode = kind
			continue
		}
		if strings.Contains(line, probeMarker+"|") {
			// A marked line. The bag observations are ours; a profile row
			// appended to the same file is not.
			readBagMarker(&sh, line)
			continue
		}
		switch mode {
		case "columns":
			if c, ok := parseDescribeRow(line); ok {
				sh.Columns = append(sh.Columns, c)
			}
		case "create":
			createLines = append(createLines, line)
		}
	}

	if !sawSection {
		return Shape{}, fmt.Errorf(
			"introspect: no %s section markers in this output. Run the SQL that "+
				"`introspect probe` printed, including the marker SELECTs, and capture all of it",
			probeMarker)
	}
	if len(sh.Columns) == 0 {
		return Shape{}, fmt.Errorf(
			"introspect: the columns section is empty — DESCRIBE %s returned nothing "+
				"the parser recognised", sh.Table)
	}

	ddl := strings.Join(createLines, "\n")

	// Completeness, cross-checked between the two sections.
	//
	// A truncated capture is the failure mode a marker cannot catch on its own:
	// the section header arrives, some rows arrive, and the parser happily
	// builds a descriptor out of the first three columns of a nine-column
	// table. This is not hypothetical — the first run of this code was against
	// output a shell pipeline had cut to three lines per statement, and it
	// produced a confident descriptor with one field in it.
	//
	// The two sections list the columns independently, so they check each
	// other. Any column named in the CREATE TABLE text that DESCRIBE did not
	// report means the DESCRIBE output was cut.
	if ddl != "" {
		// The DDL has to be whole. SHOW CREATE TABLE always closes the column
		// list on its own line, so a capture without that line is cut — and
		// this is the check that catches the case the column cross-check below
		// cannot, where BOTH sections were truncated by the same amount and
		// therefore agree with each other.
		if !reCloseParen.MatchString(ddl) {
			return Shape{}, fmt.Errorf(
				"introspect: the SHOW CREATE TABLE output is incomplete — its closing `)` is " +
					"missing, so the capture was cut. Re-run the probe and save all of the " +
					"output; a partial capture would build a descriptor out of the columns " +
					"that happened to fit")
		}
		var missing []string
		for _, name := range ddlColumnNames(ddl) {
			if _, ok := sh.column(name); !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return Shape{}, fmt.Errorf(
				"introspect: this output is incomplete — SHOW CREATE TABLE names %d column(s) "+
					"that DESCRIBE did not report (%s). The capture was probably truncated; "+
					"re-run the probe and save all of the output",
				len(missing), strings.Join(missing, ", "))
		}
	}

	sh.Indexes = parseIndexes(ddl)
	if m := reClusterBy.FindStringSubmatch(ddl); m != nil {
		sh.ClusterBy = splitList(m[1])
	}
	// SHOW CREATE TABLE reports computed columns too; DESCRIBE is the primary
	// source but this is a free cross-check for a client that mangled the
	// fifth DESCRIBE field.
	for i := range sh.Columns {
		if sh.Columns[i].Derived == "" {
			sh.Columns[i].Derived = derivedFromDDL(ddl, sh.Columns[i].Name)
		}
	}
	sh.Digest = columnsDigest(sh.Columns)
	if len(sh.bad) > 0 {
		return Shape{}, fmt.Errorf("introspect: %s", strings.Join(sh.bad, "; "))
	}
	return sh, nil
}

// ParseProfile reads the value-profile probe's output. Bag-key discovery rows
// are collected too, so `build` can say which keys exist even when their values
// were not profiled.
func ParseProfile(out, wantTable string) (Profile, []string, error) {
	p := Profile{Table: wantTable, Rows: map[string]ProfileRow{}}
	var bagKeys []string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(stripANSI(raw))
		i := strings.Index(line, probeMarker+"|")
		if i < 0 {
			continue
		}
		f := strings.Split(line[i:], "|")
		if len(f) < 4 || f[1] != ProbeVersion {
			continue
		}
		switch f[2] {
		case "section":
			if len(f) >= 5 && wantTable != "" && f[3] == "profile" && !sameTable(f[4], wantTable) {
				return Profile{}, nil, fmt.Errorf(
					"introspect: profile output is for table %q but %q was asked for",
					f[4], wantTable)
			}
		case "prof":
			if len(f) < 11 {
				continue
			}
			n := make([]int64, 7)
			bad := false
			for k := 0; k < 7; k++ {
				v, err := strconv.ParseInt(strings.TrimSpace(f[4+k]), 10, 64)
				if err != nil {
					bad = true
					break
				}
				n[k] = v
			}
			if bad {
				continue
			}
			p.Rows[f[3]] = ProfileRow{
				Col: f[3], Scanned: n[0], NonNull: n[1], NumericOK: n[2],
				TimestampOK: n[3], Distinct: n[4], MinLen: n[5], MaxLen: n[6],
			}
		case "window":
			if len(f) >= 4 {
				p.Window = f[3]
			}
		case "bagkey":
			if len(f) >= 5 {
				bagKeys = append(bagKeys, strings.Trim(f[3], "`")+"."+f[4])
			}
		}
	}
	if len(p.Rows) == 0 && len(bagKeys) == 0 {
		return Profile{}, nil, fmt.Errorf(
			"introspect: no %s profile rows in this output", probeMarker)
	}
	return p, bagKeys, nil
}

// readBagMarker consumes the bag section's marked rows.
//
// They are read wherever they appear rather than only inside their section, so
// output pasted in a different order than it was printed still parses. The
// markers are self-describing, which is the whole reason for tagging them.
func readBagMarker(sh *Shape, line string) {
	i := strings.Index(line, probeMarker+"|")
	if i < 0 {
		return
	}
	f := strings.Split(line[i:], "|")
	if len(f) < 4 || f[1] != ProbeVersion {
		return
	}
	switch f[2] {
	case "bagstat":
		if len(f) < 10 {
			return
		}
		n := make([]int64, 5)
		for k := 0; k < 5; k++ {
			v, err := strconv.ParseInt(strings.TrimSpace(f[5+k]), 10, 64)
			if err != nil {
				return
			}
			n[k] = v
		}
		if sh.BagKeys == nil {
			sh.BagKeys = map[string]BagKeyStat{}
		}
		bag, key := strings.Trim(f[3], "`"), f[4]
		sh.BagKeys[bag+"."+key] = BagKeyStat{
			Bag: bag, Key: key, Scanned: n[0], Present: n[1],
			NumericOK: n[2], Objects: n[3], Arrays: n[4],
		}
	case "bagname":
		if len(f) < 6 {
			return
		}
		v, err := strconv.ParseInt(strings.TrimSpace(f[5]), 10, 64)
		if err != nil {
			return
		}
		if sh.BagNames == nil {
			sh.BagNames = map[string]int64{}
		}
		sh.BagNames[strings.Trim(f[3], "`")+"."+f[4]] = v
	case "bagwindow":
		// Validated, not assumed. The whole point of the tagged-marker protocol
		// is that malformed input is REFUSED rather than misparsed, and this one
		// took its payload as the fourth field with no check: an extra pipe made
		// the report say "statistics cover enum; the enumeration covers stats",
		// which is nonsense presented as fact.
		if len(f) != 5 || (f[3] != "stats" && f[3] != "enum") {
			sh.bad = append(sh.bad, fmt.Sprintf(
				"a bagwindow marker is malformed (%q): expected exactly "+
					"`%s|%s|bagwindow|stats|<text>` or `…|enum|<text>`",
				strings.Join(f, "|"), probeMarker, ProbeVersion))
			return
		}
		if f[3] == "enum" {
			sh.BagEnumWindow = f[4]
		} else {
			sh.BagWindow = f[4]
		}
	case "baglimit":
		sh.BagLimited = f[3]
	}
}

func parseSection(line string) (kind, table string, ok bool) {
	i := strings.Index(line, probeMarker+"|"+ProbeVersion+"|section|")
	if i < 0 {
		return "", "", false
	}
	f := strings.Split(line[i:], "|")
	if len(f) < 5 {
		return "", "", false
	}
	return f[3], strings.TrimRight(f[4], "'\" \t"), true
}

// parseDescribeRow reads one DESCRIBE row: name, type, nullable, default, extra.
//
// Fields are tab-separated from this engine's CLI, but a client may render them
// with runs of spaces or box-drawing borders, so the split tolerates both and
// the row is rejected unless the second field looks like a type.
func parseDescribeRow(line string) (Column, bool) {
	s := strings.Trim(line, "|+ \t")
	if s == "" || strings.HasPrefix(s, "-") {
		return Column{}, false
	}
	var f []string
	if strings.Contains(s, "\t") {
		f = strings.Split(s, "\t")
	} else if strings.Contains(s, " | ") {
		f = strings.Split(s, " | ")
	} else {
		f = regexp.MustCompile(`\s{2,}`).Split(s, -1)
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	if len(f) < 2 || f[0] == "" || f[1] == "" {
		return Column{}, false
	}
	if strings.EqualFold(f[0], "Field") || strings.EqualFold(f[0], "name") {
		return Column{}, false // a header row
	}
	kind := normaliseType(f[1])
	if kind == "" {
		return Column{}, false
	}
	c := Column{Name: strings.Trim(f[0], "`"), Type: kind, RawType: f[1], Nullable: true}
	if len(f) >= 3 {
		c.Nullable = !strings.EqualFold(f[2], "NO")
	}
	if len(f) >= 5 {
		if m := reComputed.FindStringSubmatch(strings.TrimSpace(f[4])); m != nil {
			c.Derived = m[1]
		}
	}
	return c, true
}

// normaliseType maps a declared type onto the small set the role rules use,
// peeling nullability and parameters first — `Nullable(String)`,
// `varchar(16382)` and `VARCHAR` are one type. Returns "" for anything that is
// not a type at all, which is how a header or a border line gets rejected.
func normaliseType(t string) string {
	s := strings.ToLower(strings.TrimSpace(t))
	for {
		if inner, ok := peel(s, "nullable"); ok {
			s = inner
			continue
		}
		break
	}
	if i := strings.IndexByte(s, '('); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	switch s {
	case "timestamp", "datetime", "timestamp_tz", "date":
		return "timestamp"
	case "varchar", "string", "text", "char", "binary", "varbinary":
		return "string"
	case "int", "integer", "bigint", "smallint", "tinyint", "int8", "int16", "int32",
		"int64", "uint8", "uint16", "uint32", "uint64", "float", "float32", "float64",
		"double", "decimal", "numeric", "real":
		return "number"
	case "variant", "json", "map", "object":
		return "variant"
	case "boolean", "bool":
		return "boolean"
	case "array", "tuple", "bitmap", "geometry", "geography", "vector", "interval":
		return "other"
	}
	return ""
}

func peel(s, wrapper string) (string, bool) {
	if strings.HasPrefix(s, wrapper+"(") && strings.HasSuffix(s, ")") {
		return strings.TrimSpace(s[len(wrapper)+1 : len(s)-1]), true
	}
	return "", false
}

func parseIndexes(ddl string) []Index {
	var out []Index
	for _, m := range reIndex.FindAllStringSubmatch(ddl, -1) {
		ix := Index{Name: m[2], Columns: splitList(m[3])}
		if strings.EqualFold(m[1], "NGRAM") {
			ix.Kind = NgramIndex
		} else {
			ix.Kind = InvertedIndex
		}
		if t := reTokenizer.FindStringSubmatch(m[4]); t != nil {
			ix.Tokenizer = t[1]
		}
		if f := reFilters.FindStringSubmatch(m[4]); f != nil {
			for _, one := range strings.Split(f[1], ",") {
				if one = strings.TrimSpace(one); one != "" {
					ix.Filters = append(ix.Filters, one)
				}
			}
		}
		out = append(out, ix)
	}
	return out
}

// derivedFromDDL recovers a computed column's expression from SHOW CREATE
// TABLE's `name TYPE NULL AS (expr) STORED` form.
func derivedFromDDL(ddl, col string) string {
	for _, line := range strings.Split(ddl, "\n") {
		s := strings.TrimSpace(strings.Trim(line, ","))
		if !strings.HasPrefix(s, col+" ") && !strings.HasPrefix(s, "`"+col+"` ") {
			continue
		}
		i := strings.Index(s, " AS (")
		if i < 0 {
			continue
		}
		rest := s[i+5:]
		j := strings.LastIndex(rest, ") STORED")
		if j < 0 {
			j = strings.LastIndex(rest, ") VIRTUAL")
		}
		if j < 0 {
			continue
		}
		return strings.TrimSpace(rest[:j])
	}
	return ""
}

// splitList splits a comma-separated list at TOP-LEVEL commas only.
//
// A cluster key is `to_date(ts), component`, and splitting naively yields
// `to_date(ts` — which then appears verbatim in the provenance note, as it did
// before this was fixed.
// ddlColumnNames pulls the column names out of a CREATE TABLE body, so the two
// probe sections can check each other for truncation.
//
// It reads only lines that look like a column definition — an identifier
// followed by a type this compiler recognises — so index clauses, the CLUSTER BY
// tail and the engine clause contribute nothing.
func ddlColumnNames(ddl string) []string {
	var out []string
	for _, line := range strings.Split(ddl, "\n") {
		s := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), ","))
		if s == "" || strings.HasPrefix(strings.ToUpper(s), "CREATE ") {
			continue
		}
		up := strings.ToUpper(s)
		if strings.HasPrefix(up, "SYNC ") || strings.HasPrefix(up, "ASYNC ") ||
			strings.HasPrefix(up, "INVERTED ") || strings.HasPrefix(up, "NGRAM ") ||
			strings.HasPrefix(up, "CLUSTER ") || strings.HasPrefix(up, ")") {
			continue
		}
		f := strings.Fields(s)
		if len(f) < 2 {
			continue
		}
		name := strings.Trim(f[0], "`\"")
		if name == "" || normaliseType(f[1]) == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func splitList(s string) []string {
	var out []string
	depth, start := 0, 0
	flush := func(end int) {
		if one := strings.Trim(strings.TrimSpace(s[start:end]), "`\""); one != "" {
			out = append(out, one)
		}
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(s))
	return out
}

// stripANSI removes colour escapes so a coloured client's output parses.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func sameTable(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.Trim(strings.TrimSpace(s), "`\"'"))
		if i := strings.LastIndex(s, "."); i >= 0 {
			s = s[i+1:]
		}
		return s
	}
	return norm(a) == norm(b)
}

// columnsDigest is a stable fingerprint of the column list, so drift is
// detectable. FNV-1a rather than a hash import: this is a change detector, not
// a security boundary, and the module carries no dependencies.
func columnsDigest(cols []Column) string {
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		names = append(names, c.Name+":"+c.Type)
	}
	sort.Strings(names)
	var h uint64 = 14695981039346656037
	for _, s := range names {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= 1099511628211
		}
	}
	return fmt.Sprintf("fnv1a64:%016x", h)
}

// ---------------------------------------------------------------- shape helpers

func (s Shape) profilable() []Column {
	var out []Column
	for _, c := range s.Columns {
		if c.Type == "other" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// timeColumn picks the column a bound should be written against: a
// timestamp-typed one, preferring one that appears in the cluster key.
//
// This is the one role that does not use a name list, and the preference is not
// cosmetic — a bound on a clustered column prunes blocks, so it is the
// difference between a cheap probe and a full scan.
func (s Shape) timeColumn() string {
	inKey := map[string]bool{}
	for _, k := range s.ClusterBy {
		for _, name := range identsIn(k) {
			inKey[strings.ToLower(name)] = true
		}
	}
	var first string
	for _, c := range s.Columns {
		if c.Type != "timestamp" {
			continue
		}
		if inKey[strings.ToLower(c.Name)] {
			return c.Name
		}
		if first == "" {
			first = c.Name
		}
	}
	return first
}

// identsIn pulls bare identifiers out of a cluster-key expression, so
// `to_date(ts)` contributes `to_date` and `ts` — over-inclusive on purpose,
// since the only use is a membership test against real column names.
func identsIn(expr string) []string {
	return regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`).FindAllString(expr, -1)
}

func (s Shape) loneBag() string {
	var found string
	for _, c := range s.Columns {
		if c.Type != "variant" || c.Derived != "" {
			continue
		}
		if found != "" {
			return ""
		}
		found = c.Name
	}
	return found
}

// invertedIndexOf returns the inverted index covering a column, if any.
func (s Shape) invertedIndexOf(col string) string {
	for _, ix := range s.Indexes {
		if ix.Kind == InvertedIndex && ix.covers(col) {
			return ix.Name
		}
	}
	return ""
}

func (s Shape) hasNgram(col string) bool {
	for _, ix := range s.Indexes {
		if ix.Kind == NgramIndex && ix.covers(col) {
			return true
		}
	}
	return false
}

func (s Shape) column(name string) (Column, bool) {
	for _, c := range s.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return Column{}, false
}

// ---------------------------------------------------------------- building

// Candidate name lists, highest priority first, matched case-insensitively.
//
// The order is upstream's and so is the shape: a type gate, then a ranked
// canonical-name match, then — for bag roles only — a lone-compatible-column
// fallback. The restraint in that last line is the important part and it is
// deliberately copied: a lone String column is NOT guessed as the body. Getting
// the bag wrong costs a lookup; getting the body wrong points every free-text
// search in the deployment at the wrong column, and it looks like it works.
var (
	// bodyNames ranks the candidates for the default field. It is the union of
	// the two lists below, in priority order.
	bodyNames = []string{"msg", "message", "body", "log", "content", "line"}

	// The union splits when a table has BOTH a message column and a derived
	// column reconstructing the whole line, because then the two names mean
	// different things and mapping them to one column loses the distinction.
	//
	//   surfaceNames  name the whole log record — what a reader sees, what a
	//                 logs panel puts in its `body` field — so they belong to
	//                 the default field, derived or not.
	//   messageNames  name the message field specifically, so they belong to
	//                 the column that matched the list by name.
	//
	// It matters because the two are measurably different questions: over
	// logs.k8s_logs_v2, query('line:snapshot') is 25,488 rows and
	// query('msg:snapshot') is 17,649. On a table with no derived column both
	// lists land on the same column and the split costs nothing.
	surfaceNames   = []string{"body", "line", "content", "log"}
	messageNames   = []string{"msg", "message"}
	severityNames  = []string{"level", "severity", "severity_text", "log_level", "loglevel"}
	componentNames = []string{"component", "service", "service_name", "app"}
	bagNames       = []string{"kv", "attributes", "attrs", "tags", "labels"}
	timeNames      = []string{"ts", "timestamp", "time", "event_time", "_timestamp", "created_at"}
)

// numericLookingNames are the name shapes that invite a Number role. They exist
// only so that refusing one can say why, which is the whole point of the
// content sample: `duration` reads as a number and holds `47.823614ms`.
var numericLookingSuffixes = []string{"_ms", "_us", "_ns", "_sec", "_seconds", "_bytes",
	"_size", "_count", "_total", "_id", "_num"}
var numericLookingNames = []string{"duration", "latency", "elapsed", "took", "takes",
	"size", "count", "bytes", "length", "age", "delay", "rtt"}

func looksNumeric(name string) bool {
	n := strings.ToLower(name)
	for _, w := range numericLookingNames {
		if n == w {
			return true
		}
	}
	for _, s := range numericLookingSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// BuildOptions carries what the probes could not tell us.
type BuildOptions struct {
	// Window describes the profile's bound, recorded as provenance.
	Window string

	// MaxFacetDistinct suppresses a high-cardinality column from Display: a
	// column with one value per row is an identifier, not something to show in
	// a log view. Zero uses the default.
	MaxFacetDistinct int64

	// Aliases offers the other names in a role's candidate list as aliases —
	// `message` for a `msg` body, `timestamp` for a `ts` time column.
	//
	// It is OFF by default, and that is the one place this generator is more
	// conservative than it could be. An alias's only failure mode is shadowing
	// a bag key, and that failure is silent in the worst direction: a missing
	// alias makes `message:x` a bag lookup that returns nothing and the user
	// notices, while a wrong alias makes `body:x` answer with the message text
	// and the user does not. The names in these lists are not hypothetical bag
	// keys either — measured on logs.k8s_logs_v2, `body` is a real key on 3
	// rows, `service` on 476,490, `labels` on 47 and `component` on 1.
	//
	// A collision can only be ruled out for keys the probe actually saw, and a
	// table with 594 keys will not have had all of them profiled, so "no
	// collision found" is weaker than "no collision". Opting in records how
	// many keys the check covered.
	Aliases bool

	// KnownBagKeys are every `<bag>.<key>` the probe reported, profiled or
	// merely discovered, used for the alias collision check.
	KnownBagKeys []string

	// MinPromoteSample is how many non-null sampled values a CONTENT promotion
	// needs before it is applied rather than merely proposed. Zero uses the
	// default.
	//
	// It exists for the time role specifically. Promoting a VARCHAR column to
	// TIMESTAMP on the strength of eight rows is not evidence, it is a
	// coincidence with a sample size, and the time role is the one role that
	// gates every time-bounded query in the deployment: a value that does not
	// cast leaves its row out of every panel at once. So below this threshold
	// the candidate is RECORDED for a human to confirm with -time-column, and
	// the role is left empty — which makes the descriptor fail to load naming
	// it, rather than load and quietly lose rows.
	//
	// The rule itself is sound at any size — on a 3-row probe where 2 of 3 cast
	// it correctly declined — but a clean sample must not buy silence.
	MinPromoteSample int64

	// TimeColumn names the time field by hand, confirming a candidate the
	// sample was too small to promote on its own.
	TimeColumn string

	// BagNumeric declares a bag key as Number when every sampled value casts.
	//
	// It is OFF by default, and the reason is a measurement rather than
	// caution. Declaring a bag key numeric changes exactly one thing — the
	// EQUALITY — and it changes it for the worse on this engine. An undeclared
	// key's equality is compiled as an index-backed lookup plus an exact
	// residual, `query('kv.term:40') AND lower(kv['term']::VARCHAR) = '40'`;
	// declared Number it becomes `TRY_CAST(kv['term']::VARCHAR AS DOUBLE) = 40`,
	// which is a full scan, because a numeric field never reaches the indexed
	// equality path. Bounds convert either way, so nothing is gained there.
	//
	// What it buys is matching a non-canonical spelling: `040` and `40.0` equal
	// 40 as numbers and not as strings. Measured over logs.k8s_logs_v2
	// (967,912 rows, ts < 2026-08-19 22:19:00), kv['term'] has 0 values with a
	// leading zero and 0 with a decimal point, and all three spellings of
	// `term:40` return the same 26 rows. So the default trade — keep the index,
	// keep the string equality — is the right one, and the evidence for the
	// other choice is recorded either way so an operator can make it knowingly.
	BagNumeric bool
}

// Build turns a shape, and optionally a value profile, into a descriptor.
//
// Roles are decided in the order upstream uses, and the one that does not use a
// name list is the timestamp: a timestamp-typed column, preferring one in the
// cluster key, else left EMPTY. Every unmatched role is left empty on purpose.
// The existing load-time validation then refuses the descriptor and names the
// missing role, which is a better outcome than a guess that loads: a wrong
// severity column renders every line at one level, and nothing says so.
//
// The content sample can DEMOTE a type but never promote a column's declared
// one. A column declared VARCHAR stays a string even if every value casts,
// because Number changes how equality is emitted and the declared type is the
// only thing the engine itself guarantees. Bag keys are the exception, and the
// only exception, because a bag key has no declared type at all — which is
// exactly why the duration bug lives there and why the sample is what kills it.
func Build(shape Shape, prof Profile, opts BuildOptions) (Def, []string, error) {
	if shape.Table == "" {
		return Def{}, nil, fmt.Errorf("introspect: the shape names no table")
	}
	if len(shape.Columns) == 0 {
		return Def{}, nil, fmt.Errorf("introspect: %s has no columns", shape.Table)
	}
	if opts.MaxFacetDistinct == 0 {
		opts.MaxFacetDistinct = 10000
	}
	if opts.MinPromoteSample == 0 {
		opts.MinPromoteSample = 1000
	}
	// A profile that scanned no rows is not evidence. It used to be recorded as
	// `profiled: true` with every per-role note correctly saying "no content
	// evidence" — the flag and the notes contradicting each other in the one
	// block whose whole job is answering "how much evidence backed this?".
	scanned := prof.Scanned()
	profiled := len(prof.Rows) > 0 && scanned > 0

	// The profile's own window wins. An explicit -window is a fallback for
	// output produced before the marker existed.
	window := prof.Window
	if window == "" {
		window = opts.Window
	}

	b := &builder{shape: shape, prof: prof, opts: opts, roles: map[string]string{},
		known: map[string]bool{}}
	for _, k := range opts.KnownBagKeys {
		b.known[strings.ToLower(k)] = true
	}
	for k := range prof.Rows {
		if strings.Contains(k, ".") {
			b.known[strings.ToLower(k)] = true
		}
	}
	switch {
	case len(prof.Rows) == 0:
		b.refuse("no value profile was supplied, so nothing here rests on the data: no bag key " +
			"can be shown to be mixed, no column can be suppressed from `display` for being " +
			"mostly empty, and a VARCHAR holding instants cannot be recognised. Run " +
			"`introspect profile` and rebuild")
	case scanned == 0:
		b.refuse("the value profile scanned 0 rows — its window is empty, so it is recorded as "+
			"UNPROFILED. Every type here rests on the declared type alone. Re-profile with a "+
			"window that contains data (this one was %q)", window)
	}

	timeCol := b.pickTime()
	bags := b.pickBags()
	bodyCol := b.pickBody(bags)
	// The column whose NAME matched the body list, which is not always the
	// default field: when a derived text surface wins, the message column is
	// still the thing `message` is a synonym for.
	msgCol, _ := b.rank(bodyNames, "string")
	b.defaultCol = bodyCol
	sevCol := b.pickSeverity()
	compCol := b.pickComponent()

	// The index group. Only the group covering the body is declared, because a
	// descriptor whose searchable surfaces span two inverted indexes is refused
	// at load — one query() call reaches the columns of one index and no more.
	group := ""
	if bodyCol != "" {
		group = shape.invertedIndexOf(bodyCol)
	}
	indexes := b.pickIndexes(group)

	def := Def{
		Table:   shape.Table,
		Default: bodyCol,
		Time:    timeCol,
		Indexes: indexes,
	}
	if sevCol != "" {
		def.Severity = sevCol
	}
	def.Bags = bags
	def.Fields = b.fields(group, bodyCol, msgCol, timeCol)
	def.Display = b.display(bodyCol, timeCol, sevCol, compCol)

	if opts.Aliases {
		b.roles["aliases"] = fmt.Sprintf(
			"offered from the role name lists, checked against the %d bag keys the probe "+
				"reported. A key the probe did not see cannot be ruled out", len(b.known))
	} else {
		b.roles["aliases"] = "none offered (-aliases turns them on). An alias that shadows a " +
			"bag key answers a question about the bag with the aliased column's value, and " +
			"nothing says so"
	}
	def.Introspect = &IntrospectDef{
		Version: ProbeVersion, Table: shape.Table, ColumnsDigest: shape.Digest,
		Profiled: profiled, Rows: scanned, Window: window,
		Roles: b.roles, Refused: b.refused, Blocked: b.blocked,
	}
	return def, b.notes, nil
}

type builder struct {
	shape   Shape
	prof    Profile
	opts    BuildOptions
	roles   map[string]string
	refused []string
	blocked []string
	notes   []string

	// known is every bag key the probe saw, lowercased, for the alias check.
	known map[string]bool

	// defaultCol is the chosen default field, so the whole-record alias names
	// attach to it rather than to whichever column matched by name.
	defaultCol string

	// timeFrom is the provenance of a content-promoted time column, which
	// differs depending on whether a human confirmed it.
	timeFrom string
}

func (b *builder) refuse(format string, args ...interface{}) {
	b.refused = append(b.refused, fmt.Sprintf(format, args...))
}

// block records a refusal that needs a human before the descriptor is useful.
// It is reported like any other, and it also makes `introspect build` exit
// non-zero, so `build && deploy` cannot ship a descriptor whose most
// consequential role was declined for want of evidence.
func (b *builder) block(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	b.refused = append(b.refused, msg)
	b.blocked = append(b.blocked, msg)
}

func (b *builder) note(format string, args ...interface{}) {
	b.notes = append(b.notes, fmt.Sprintf(format, args...))
}

// rank returns the first column whose name matches the candidate list, and the
// rank it matched at, restricted to columns of the given normalised type.
func (b *builder) rank(names []string, wantType string) (col string, why string) {
	for i, want := range names {
		for _, c := range b.shape.Columns {
			if !strings.EqualFold(c.Name, want) || c.Type != wantType {
				continue
			}
			return c.Name, fmt.Sprintf("canonical-name (%s, rank %d of %s)",
				c.Name, i+1, strings.Join(names, ","))
		}
	}
	return "", ""
}

func (b *builder) pickTime() string {
	col := b.shape.timeColumn()
	if col == "" {
		var stringy []string
		for _, c := range b.shape.Columns {
			if c.Type != "string" {
				continue
			}
			if r, ok := b.prof.row(c.Name); ok && r.allTimestamps() && nameIn(timeNames, c.Name) {
				stringy = append(stringy, c.Name)
			}
		}
		if len(stringy) == 1 {
			// A VARCHAR that holds only timestamps AND is named like a time
			// column. Both halves are required: content alone would promote any
			// stringified date, and the name alone is the guess that types a
			// free-text field as time.
			c := stringy[0]
			r, _ := b.prof.row(c)
			confirmed := strings.EqualFold(b.opts.TimeColumn, c)
			if r.NonNull < b.opts.MinPromoteSample && !confirmed {
				// A candidate, not a decision. The time role gates every
				// time-bounded query, so a sample this small must be confirmed
				// by a human rather than believed.
				b.roles["time"] = fmt.Sprintf(
					"CANDIDATE, NOT APPLIED: %s is VARCHAR and all %d of its sampled values "+
						"cast to TIMESTAMP, but %d values is below the %d needed to promote a "+
						"column on content alone. Confirm with -time-column %s",
					c, r.NonNull, r.NonNull, b.opts.MinPromoteSample, c)
				b.block("no time role applied: %q looks like one — VARCHAR, named like a time "+
					"field, and all %d sampled values cast — but the time role gates EVERY "+
					"time-bounded query, so a value that does not cast leaves its row out of "+
					"every panel at once, and %d rows is not enough evidence to accept that "+
					"risk. Re-profile over more data, or confirm with -time-column %s. Until "+
					"then the role is empty: the descriptor still LOADS (a table with no time "+
					"column is legal) and says so in a note, %q stays a plain string field, "+
					"the dashboard generator refuses to emit any panel, and `sql` emits no "+
					"ORDER BY",
					c, r.NonNull, r.NonNull, c, c)
				return ""
			}
			why := "content-sample"
			if confirmed {
				why = "user-supplied, confirming a content-sample candidate"
			}
			b.timeFrom = why + " (VARCHAR whose every sampled value casts to TIMESTAMP)"
			b.roles["time"] = fmt.Sprintf(
				"%s (%s is VARCHAR but all %d sampled values cast to TIMESTAMP, and the name is "+
					"a time candidate; read through a cast, so a value that does not cast is "+
					"EXCLUDED from every time-bounded query)", why, c, r.NonNull)
			b.refuse("time role %q is read through TRY_CAST because the column is VARCHAR, not "+
				"TIMESTAMP: %d of %d sampled values cast. A value that does not cast becomes "+
				"NULL and its row is excluded from every time-bounded query — not just one "+
				"filter — with no error. Count them with count_if(%s IS NOT NULL AND "+
				"TRY_CAST(%s AS TIMESTAMP) IS NULL)", c, r.NumericOKTimestamps(), r.NonNull,
				quoteIdent(c), quoteIdent(c))
			return c
		}
		// Report a near miss, because "2,000 of 2,001 values cast" is the
		// actionable fact and silence about it is what sends someone hunting.
		// A single uncastable value is enough to decline: the time role gates
		// every time-bounded query, so that one row would be invisible to all
		// of them.
		var near []string
		for _, c := range b.shape.Columns {
			if c.Type != "string" || !nameIn(timeNames, c.Name) {
				continue
			}
			if r, ok := b.prof.row(c.Name); ok && r.NonNull > 0 && r.TimestampOK > 0 &&
				r.TimestampOK < r.NonNull {
				near = append(near, fmt.Sprintf("%s (%d of %d values cast; %d would be invisible "+
					"to every time-bounded query)", c.Name, r.TimestampOK, r.NonNull,
					r.NonNull-r.TimestampOK))
			}
		}
		b.roles["time"] = "UNRESOLVED: no timestamp-typed column, and no VARCHAR column both " +
			"named like a time field and holding only timestamps"
		if len(near) > 0 {
			b.roles["time"] += " — near miss: " + strings.Join(near, "; ")
		}
		msg := "no time role. The descriptor still LOADS — a table with no time column is legal — " +
			"and says so in a note, but the dashboard generator refuses to emit any panel and " +
			"`sql` emits no ORDER BY. Declare a field of kind \"timestamp\" by hand, reading the " +
			"column through a cast if it is not stored as one"
		if len(near) > 0 {
			msg += ". Near miss: " + strings.Join(near, "; ")
		}
		b.block("%s", msg)
		return ""
	}
	inKey := false
	for _, k := range b.shape.ClusterBy {
		for _, id := range identsIn(k) {
			if strings.EqualFold(id, col) {
				inKey = true
			}
		}
	}
	if inKey {
		b.roles["time"] = fmt.Sprintf("type-only (%s is timestamp-typed and appears in the "+
			"cluster key %s, so a bound on it prunes blocks)", col, strings.Join(b.shape.ClusterBy, ", "))
	} else {
		b.roles["time"] = fmt.Sprintf(
			"type-only (%s is the only timestamp-typed column; it is NOT in the cluster key, "+
				"so a bound on it does not prune", col)
	}
	return col
}

// pickBody is the one role with a rule of its own, because this deployment has
// a shape upstream does not: a STORED computed column that reconstructs the
// whole log line.
//
// A STORED text column, covered by an inverted index, whose expression reads
// both a body-candidate column and a bag column, IS the derived text surface —
// the thing round 4 built so that a bare word finds text the collector moved out
// of the message and into the attribute bag. It is a strictly better default
// than the message column, and the evidence for it is in the expression rather
// than in the name, which is why it is checked before the name list.
//
// Measured on logs.k8s_logs_v2: `line` is STORED from
// `concat_ws(' ', msg, nullif(json_path_query_array(object_delete(kv, …)…)))`,
// covered by idx_msg, and query('line:RemoteStopped') is 605 rows against 0 for
// query('msg:RemoteStopped').
func (b *builder) pickBody(bags []BagDef) string {
	bagCols := map[string]bool{}
	for _, bg := range bags {
		bagCols[strings.ToLower(bg.Column)] = true
	}
	for _, c := range b.shape.Columns {
		if c.Derived == "" || c.Type != "string" {
			continue
		}
		ix := b.shape.invertedIndexOf(c.Name)
		if ix == "" {
			continue
		}
		refsBody, refsBag := "", ""
		for _, id := range identsIn(c.Derived) {
			if nameIn(bodyNames, id) {
				if _, ok := b.shape.column(id); ok {
					refsBody = id
				}
			}
			if bagCols[strings.ToLower(id)] {
				refsBag = id
			}
		}
		if refsBody != "" && refsBag != "" {
			b.roles["default"] = fmt.Sprintf(
				"derived-text-surface (%s is a STORED column reading %s and the %s bag, and "+
					"index %s covers it, so a bare word reaches text the collector moved out "+
					"of %s)", c.Name, refsBody, refsBag, ix, refsBody)
			return c.Name
		}
	}

	col, why := b.rank(bodyNames, "string")
	if col == "" {
		b.roles["default"] = "UNRESOLVED: no column named like a log body. A lone String " +
			"column is deliberately NOT guessed as the body — getting it wrong points every " +
			"free-text search at the wrong column and looks like it works"
		b.refuse("no default field: the descriptor will be refused at load. Name the body " +
			"column by hand")
		return ""
	}
	if ix := b.shape.invertedIndexOf(col); ix == "" {
		// Recorded, and left as kind "text" on purpose: the load-time check
		// then refuses the descriptor with a message naming the column and the
		// missing index, which is the only honest outcome. This compiler cannot
		// fall back to a token function the way a system with a slow-but-correct
		// path can — outside the index group a text field is unusable, not slow.
		b.roles["default"] = why + " — NOT INDEXED"
		b.refuse("no inverted index covers %q, the chosen default field. The descriptor "+
			"records it as kind \"text\" so that loading fails and names it, because a bare "+
			"term on an unindexed column cannot be compiled: one search function per "+
			"statement, and no index means no search function. Either build the index or "+
			"choose a different default", col)
	} else {
		b.roles["default"] = why + fmt.Sprintf(" — index %s covers it", ix)
	}
	if r, ok := b.prof.row(col); ok && r.NonNull > 0 {
		if r.MaxLen > 0 && r.MaxLen < 16 {
			b.refuse("%q was chosen as the default field by name, but its longest value in the "+
				"sampled window is %d characters — that is a code or an enum, not a log body. "+
				"Check it", col, r.MaxLen)
		}
	}
	return col
}

func (b *builder) pickSeverity() string {
	col, why := b.rank(severityNames, "string")
	if col == "" {
		b.roles["severity"] = "UNRESOLVED: no column named like a severity. Not guessed — a " +
			"wrong severity column renders every line at one level and nothing says so"
		return ""
	}
	if r, ok := b.prof.row(col); ok && r.NonNull > 0 {
		if r.Distinct > 64 {
			b.refuse("%q matches the severity name list but has ~%d distinct values in the "+
				"sampled window; a severity column has a handful. Left unset", col, r.Distinct)
			b.roles["severity"] = fmt.Sprintf(
				"REFUSED: %s matched by name but content says ~%d distinct values", col, r.Distinct)
			return ""
		}
		b.roles["severity"] = why + fmt.Sprintf(" — content-sample agrees: ~%d distinct values",
			r.Distinct)
		return col
	}
	b.roles["severity"] = why + " — no content evidence"
	return col
}

func (b *builder) pickComponent() string {
	col, why := b.rank(componentNames, "string")
	if col == "" {
		b.roles["component"] = "UNRESOLVED: no column named like a component or service"
		return ""
	}
	// There is no component role in Schema, so this only orders Display. Said
	// out loud rather than implied, because a reader of the provenance block
	// would otherwise look for a role that does not exist.
	b.roles["component"] = why + " — used only to order `display`; Schema has no component role"
	return col
}

func (b *builder) pickBags() []BagDef {
	var variants []Column
	for _, c := range b.shape.Columns {
		if c.Type == "variant" && c.Derived == "" {
			variants = append(variants, c)
		}
	}
	if len(variants) == 0 {
		b.roles["bags"] = "none: no VARIANT column"
		b.refuse("no attribute bag: a field name the descriptor does not list will be a " +
			"compile error rather than a bag lookup")
		return nil
	}

	var out []BagDef
	named := map[string]string{}
	for i, want := range bagNames {
		for _, c := range variants {
			if strings.EqualFold(c.Name, want) {
				named[c.Name] = fmt.Sprintf("canonical-name (rank %d of %s)", i+1,
					strings.Join(bagNames, ","))
			}
		}
	}
	for _, c := range variants {
		why, ok := named[c.Name]
		if !ok {
			if len(variants) == 1 {
				// The lone-compatible-column fallback, allowed for bag roles
				// and only for bag roles. A misidentified bag costs a lookup
				// that returns nothing; a misidentified body costs every search.
				why = "lone-candidate (the only VARIANT column, so no name match was needed)"
			} else {
				b.refuse("VARIANT column %q was not declared as a bag: it matches no bag name "+
					"and there are %d VARIANT columns, so the lone-candidate fallback does not "+
					"apply. Add it by hand if it holds attributes", c.Name, len(variants))
				continue
			}
		}
		bd := BagDef{Column: c.Name, From: why}
		bd.Keys = b.bagKeys(c.Name)
		out = append(out, bd)
	}
	if len(out) > 1 {
		b.note("%d catch-all bags were declared; an undeclared field name reaches only the "+
			"first. Give the others a prefix if they should be addressable", len(out))
	}
	if len(out) > 0 {
		parts := make([]string, 0, len(out))
		for _, bd := range out {
			parts = append(parts, bd.Column+": "+bd.From)
		}
		b.roles["bags"] = strings.Join(parts, "; ")
	}
	return out
}

// bagKeys types the bag keys the profile covered.
//
// This is the only place a content sample PROMOTES a type, and it is where the
// duration defect dies. A bag key has no declared type — that is what a bag is
// for — so the alternatives are to guess from the name or to look at the values.
// Guessing from the name is how `duration` ends up declared numeric; but note
// that `duration:>100` returns 0 of its rows whatever the declaration says,
// because a numeric bound converts either way. The declaration matters for the
// EQUALITY, and the sample matters for knowing the bound is lossy at all.
//
// Three outcomes, and two of them are refusals:
//
//	numeric_ok == non_null > 0        Number is safe
//	0 < numeric_ok < non_null         String, and say the ratio — a MIXED key is
//	                                  the dangerous one, because a bound on it
//	                                  silently drops the rows that do not cast
//	numeric_ok == 0                   String; if the name looks numeric, say so
func (b *builder) bagKeys(bag string) map[string]string {
	out := map[string]string{}
	prefix := strings.ToLower(bag) + "."
	for col, r := range b.prof.Rows {
		if !strings.HasPrefix(strings.ToLower(col), prefix) {
			continue
		}
		key := col[len(prefix):]
		switch r.numeric() {
		case numericYes:
			if !b.opts.BagNumeric {
				b.roles["bagkey:"+key] = fmt.Sprintf(
					"all %d sampled values cast to a number, but the key is left as a string: "+
						"declaring it Number changes only the equality, and changes it from an "+
						"index-backed lookup to a full scan. -bag-numeric declares it anyway",
					r.NonNull)
				continue
			}
			out[key] = "number"
		case numericMixed:
			b.refuse("bag key %q is NOT typed as a number: %d of its %d values cast (%.1f%%), "+
				"so a bound on it silently drops the other %d rows. Mixed keys are the "+
				"dangerous ones — the query answers, and the answer is short",
				col, r.NumericOK, r.NonNull, 100*float64(r.NumericOK)/float64(r.NonNull),
				r.NonNull-r.NumericOK)
		case numericNo:
			if r.NonNull > 0 && looksNumeric(key) {
				b.refuse("bag key %q reads like a number and is NOT one: 0 of its %d values "+
					"cast. Typed as a number it would answer every bound with 0 rows and raise "+
					"nothing — check the values for a unit suffix", col, r.NonNull)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pickIndexes declares the inverted index covering the body plus every ngram
// index, and drops any other inverted index.
//
// Dropping is not tidying. A descriptor whose text fields sit in two inverted
// indexes is refused at load, because one query() call reaches the columns of
// one index — measured, a table with separate idx_line(line) and idx_line2(line2)
// answers each column alone and fails `[1065] columns line2, line don't have
// inverted index` for a query naming both. So the second group is declared
// nowhere, its columns come out as kind "string", and the note says which
// searchability was given up.
func (b *builder) pickIndexes(group string) []IndexDef {
	var out []IndexDef
	for _, ix := range b.shape.Indexes {
		switch {
		case ix.Kind == NgramIndex:
		case ix.Name == group:
		default:
			b.refuse("inverted index %s (%s) is not declared: the descriptor can carry one "+
				"inverted index group — one query() call reaches the columns of one index — "+
				"and %s covers the default field. Columns %s are therefore kind \"string\", "+
				"searched by LIKE rather than by the index",
				ix.Name, strings.Join(ix.Columns, ", "), orNoneStr(group),
				strings.Join(ix.Columns, ", "))
			continue
		}
		out = append(out, IndexDef{
			Name: ix.Name, Kind: ix.Kind.String(), Columns: ix.Columns,
			Tokenizer: ix.Tokenizer, Filters: ix.Filters,
		})
	}
	return out
}

// castRate describes a column's numeric cast rate in words, for a note.
func castRate(r ProfileRow) string {
	switch {
	case r.NumericOK == 0:
		return fmt.Sprintf("none of its %d sampled values cast", r.NonNull)
	default:
		return fmt.Sprintf("only %d of its %d sampled values cast (%.1f%%)",
			r.NumericOK, r.NonNull, 100*float64(r.NumericOK)/float64(r.NonNull))
	}
}

// orNoWindow describes a window the probe reported, or says the report is
// missing it.
//
// Three cases, because two of them were previously collapsed into one wrong
// sentence. This helper runs at DRIFT time, over a paste; it cannot know why a
// marker is absent, so it must not guess. It previously asserted "this schema
// declares no time column", which is false for a descriptor that declares one
// and whose marker was merely trimmed — and it said it of the ENUMERATION window
// too, which is never time-bounded under any circumstances.
//
// The reason a statistics window is unbounded is known at PROBE time, where the
// schema is in hand, so the probe writes it into the marker itself (see
// BagDriftProbe). Here there is only one honest thing to say when the marker is
// gone: it is gone.
func orNoWindow(s string) string {
	if s == "" {
		return "not stated in this output — the paste predates the bag section, or its window " +
			"marker was trimmed"
	}
	return s
}

func orNoneStr(s string) string {
	if s == "" {
		return "no index"
	}
	return s
}

// fields renders one FieldDef per column, with the kind each column's evidence
// supports and the aliases its role's name list can safely spare.
func (b *builder) fields(group, bodyCol, msgCol, timeCol string) []FieldDef {
	var out []FieldDef
	for _, c := range b.shape.Columns {
		if c.Type == "variant" || c.Type == "other" {
			// A bag is declared as a bag, not as a field; an ARRAY or a TUPLE
			// has no comparison this compiler can render.
			if c.Type == "other" {
				b.refuse("column %q (%s) is not declared: no field kind here can search it",
					c.Name, c.RawType)
			}
			continue
		}
		fd := FieldDef{Name: c.Name}
		switch {
		case c.Name == bodyCol:
			fd.Kind = "text"
			fd.From = "role:default"
		case c.Type == "timestamp":
			fd.Kind = "timestamp"
			fd.From = "type-only"
		case c.Type == "number":
			fd.Kind = "number"
			fd.From = "type-only"
		case c.Type == "boolean":
			fd.Kind = "string"
			fd.From = "type-only (boolean is compared as a string; there is no Bool kind)"
		case group != "" && b.shape.invertedIndexOf(c.Name) == group:
			// Another column of the body's index group. It is genuinely
			// full-text searchable and composes with the body inside one
			// query() call, which is the whole reason the group matters.
			fd.Kind = "text"
			fd.From = "index-group (covered by " + group + ", the default field's index)"
		default:
			fd.Kind = "string"
			fd.From = "type-only"
			if r, ok := b.prof.row(c.Name); ok && r.NonNull > 0 && looksNumeric(c.Name) &&
				r.numeric() != numericYes {
				// A column, so Number was never available — the declared type
				// decides and this one is a string. Said anyway, because the
				// operator reading this report is the person who would
				// otherwise write `latency_ms:>100` and read the zero as an
				// answer.
				b.refuse("column %q reads like a number and %s: a bound on it converts with "+
					"TRY_CAST, so values that are not numbers are excluded rather than "+
					"counted — check them for a unit suffix",
					c.Name, castRate(r))
			}
		}
		if c.Derived != "" {
			fd.Derived = c.Derived
			if fd.From == "role:default" {
				fd.From = "derived-text-surface"
			} else {
				fd.From += " (STORED computed column)"
			}
		}
		if timeCol != "" && c.Name == timeCol && c.Type == "string" {
			// A VARCHAR holding timestamps, promoted on content evidence. The
			// cast goes in the column expression so every comparison reads it
			// as an instant.
			fd.Kind = "timestamp"
			fd.Column = fmt.Sprintf("TRY_CAST(%s AS TIMESTAMP)", quoteIdent(c.Name))
			fd.Conversion = fmt.Sprintf("the column is %s, read as an instant", c.RawType)
			fd.From = b.timeFrom
		}
		fd.Aliases = b.aliasesFor(c.Name, msgCol, timeCol)
		if r, ok := b.prof.row(c.Name); ok && r.NonNull > 0 && r.MaxLen > 0 {
			// One real value would be better, but the profile carries only
			// min/max so the example is a length hint rather than a sample.
			_ = r
		}
		out = append(out, fd)
	}
	return out
}

// aliasesFor offers the other names in a role's candidate list, minus any that
// would shadow something real.
//
// Two exclusions, and the second is not hypothetical. A name that is already a
// column of this table obviously cannot be an alias for a different one. And a
// name that appears as a KEY of the attribute bag must not become an alias
// either, because it would silently stop reaching the bag: measured on
// logs.k8s_logs_v2, `body` is a real bag key on 3 rows, `service` on 476,490,
// `labels` on 47 and `component` on 1. Aliasing `body` to the message column
// would answer a question about kv['body'] with the message text.
// aliasesFor offers the other names in a role's candidate list as aliases for
// the column that matched that list by name.
//
// It keys on the NAME-MATCHED column, not on the role. When a derived text
// surface wins the default-field role, `message` is still a synonym for the
// message column and not for the reconstruction of the whole line — the two
// differ, which is the entire point of the derived column. Attaching body
// aliases to the derived surface made `message:peer` compile to
// `query('line:peer')` where the hand-written descriptor gives
// `query('msg:peer')`, and those are different questions: over
// logs.k8s_logs_v2, query('line:snapshot') is 25,488 rows and
// query('msg:snapshot') is 17,649.
//
// The derived surface itself gets no aliases. It is not a synonym for anything
// a user would type; it is a superset, and naming it as though it were the
// message is how a reader stops being able to ask about the message alone.
func (b *builder) aliasesFor(col, msgCol, timeCol string) []string {
	if !b.opts.Aliases {
		return nil
	}
	var list []string
	if col == b.defaultCol {
		list = append(list, surfaceNames...)
	}
	if col == msgCol {
		list = append(list, messageNames...)
	}
	if col == timeCol {
		list = append(list, timeNames...)
	}
	if len(list) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range list {
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		if strings.EqualFold(name, col) {
			continue
		}
		if _, isColumn := b.shape.column(name); isColumn {
			continue
		}
		if b.bagKeyExists(name) {
			b.refuse("%q was not offered as an alias for %q: it is a key of the attribute bag, "+
				"so aliasing it would answer a question about the bag with %q's value",
				name, col, col)
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (b *builder) bagKeyExists(name string) bool {
	suffix := "." + strings.ToLower(name)
	for col := range b.known {
		if strings.HasSuffix(col, suffix) {
			return true
		}
	}
	return false
}

// display orders the row-level view: time, severity, the component-ish column,
// then the remaining plain columns, then the body last.
//
// The body goes last because it is the wide one, and a wide column in the middle
// pushes everything after it off the screen. High-cardinality columns are
// dropped: a column with close to one distinct value per row is an identifier,
// and a log view showing it shows nothing twice.
func (b *builder) display(bodyCol, timeCol, sevCol, compCol string) []string {
	var out []string
	add := func(n string) {
		if n == "" {
			return
		}
		for _, e := range out {
			if e == n {
				return
			}
		}
		out = append(out, n)
	}
	add(timeCol)
	add(sevCol)
	add(compCol)
	textCols := map[string]bool{}
	for _, ix := range b.shape.Indexes {
		if ix.Kind != InvertedIndex {
			continue
		}
		for _, col := range ix.Columns {
			textCols[strings.ToLower(col)] = true
		}
	}
	for _, c := range b.shape.Columns {
		if c.Type != "string" || c.Name == bodyCol || c.Derived != "" {
			continue
		}
		if textCols[strings.ToLower(c.Name)] {
			// A second full-text column is the message behind a derived
			// surface. Showing both puts the same words on the row twice and
			// pushes everything else off the screen.
			continue
		}
		if r, ok := b.prof.row(c.Name); ok {
			if r.Distinct > b.opts.MaxFacetDistinct && r.NonNull > 0 &&
				r.Distinct*2 > r.NonNull {
				b.refuse("column %q is not in `display`: ~%d distinct values over %d rows is an "+
					"identifier, not something a log view can show usefully",
					c.Name, r.Distinct, r.NonNull)
				continue
			}
			if r.Scanned > 0 && r.NonNull*20 < r.Scanned {
				// A forward-only column is the case: `raw` was added to
				// logs.k8s_logs and the collector began populating it, so
				// 5,112 rows of 1,016,392 have a value and the rest never
				// will. A log view that reserves a column for it shows an
				// empty one nearly always. It stays a declared FIELD — that
				// part is correctness, and leaving it undeclared routes
				// `raw:x` into the bag where it matches nothing — but it is
				// not something to put on every row.
				b.refuse("column %q is not in `display`: only %d of %d sampled rows have a "+
					"value (%.1f%%), so a column reserved for it is empty nearly always. It is "+
					"still declared as a field, which is what stops a query naming it from "+
					"being routed into the attribute bag",
					c.Name, r.NonNull, r.Scanned, 100*float64(r.NonNull)/float64(r.Scanned))
				continue
			}
		}
		add(c.Name)
	}
	add(bodyCol)
	return out
}

func nameIn(list []string, name string) bool {
	for _, n := range list {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
