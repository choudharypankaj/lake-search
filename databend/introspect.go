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
// DATA, which an interactive form debouncing at one second cannot. That is not
// a theoretical advantage. Over logs.k8s_logs_v2 (967,912 rows, frozen
// ts < 2026-08-19 22:19:00) the bag key `duration` holds 1,945 values and 2 of
// them cast to a number — the rest are Go durations like `47.823614ms`. A
// type-only inference calls that key numeric and reproduces exactly the defect
// round 4 measured: `duration:>100` returning 0 rows out of 1,945, silently.
// The sampled profile sees 2/1,945 and refuses the Number role with the ratio
// in the note.
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

func (p ProfileRow) allTimestamps() bool {
	return p.NonNull > 0 && p.TimestampOK == p.NonNull
}

// Profile is the set of profile rows, keyed by Col.
type Profile struct {
	Table string
	Rows  map[string]ProfileRow
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
func ProfileProbe(shape Shape, window string, maxKeys int) (string, error) {
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

	where := ""
	if t := shape.timeColumn(); t != "" && window != "" {
		// A bound, not a LIMIT. A LIMIT would be applied after the aggregate
		// and profile the whole table anyway.
		where = fmt.Sprintf("\nWHERE %s >= subtract_hours(now(), %s)", t, window)
	}

	var b strings.Builder
	b.WriteString(probeHeader("profile", shape.Table))
	if where == "" {
		b.WriteString("-- NOTE: no timestamp column was found, so this profiles the WHOLE table.\n")
		b.WriteString("--       Add a bound by hand if that is too expensive.\n")
	} else {
		fmt.Fprintf(&b, "-- Bounded to the last %s hour(s) of %s.\n", window, shape.timeColumn())
	}
	b.WriteString(`-- Run all statements and append the output to the SAME file as the shape probe,
-- or save it separately and pass both to ` + "`introspect build`" + `.

`)
	b.WriteString(sectionMarker("profile", shape.Table) + "\n")

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
-- A bag key's type is not in any schema, so it is read from the data. This is
-- what stops a key like `+"`duration`"+` — whose values are `+"`47.823614ms`"+` — from
-- being typed as a number and then silently answering 0 rows to `+"`duration:>100`"+`.
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
func BagKeyProfileProbe(shape Shape, bag string, keys []string, window string) (string, error) {
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
	where := ""
	if t := shape.timeColumn(); t != "" && window != "" {
		where = fmt.Sprintf("\nWHERE %s >= subtract_hours(now(), %s)", t, window)
	}

	var b strings.Builder
	b.WriteString(probeHeader("profile", shape.Table))
	b.WriteString(sectionMarker("profile", shape.Table) + "\n")
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
	fmt.Fprintf(&b, `-- And the drift check: compare this against introspect.columns_digest.
%s
DESCRIBE %s;
`, sectionMarker("columns", s.Table), s.Table)
	return b.String()
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
	reClusterBy = regexp.MustCompile(`(?i)CLUSTER\s+BY\s*\(([^)]*)\)`)
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
			// A marked line from another section (a profile row appended to the
			// same file). Not ours.
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

func splitList(s string) []string {
	var out []string
	for _, one := range strings.Split(s, ",") {
		if one = strings.Trim(strings.TrimSpace(one), "`\""); one != "" {
			out = append(out, one)
		}
	}
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
	bodyNames      = []string{"msg", "message", "body", "log", "content", "line"}
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
	profiled := len(prof.Rows) > 0

	b := &builder{shape: shape, prof: prof, opts: opts, roles: map[string]string{}}
	if !profiled {
		b.refuse("no value profile was supplied, so every type rests on the declared type " +
			"alone. That is the blind spot that types a column of Go durations as a number: " +
			"run `introspect profile` and rebuild")
	}

	timeCol := b.pickTime()
	bags := b.pickBags()
	bodyCol := b.pickBody(bags)
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
	def.Fields = b.fields(group, bodyCol, timeCol)
	def.Display = b.display(bodyCol, timeCol, sevCol, compCol)

	def.Introspect = &IntrospectDef{
		Version: ProbeVersion, Table: shape.Table, ColumnsDigest: shape.Digest,
		Profiled: profiled, Window: opts.Window,
		Roles: b.roles, Refused: b.refused,
	}
	return def, b.notes, nil
}

type builder struct {
	shape   Shape
	prof    Profile
	opts    BuildOptions
	roles   map[string]string
	refused []string
	notes   []string
}

func (b *builder) refuse(format string, args ...interface{}) {
	b.refused = append(b.refused, fmt.Sprintf(format, args...))
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
			b.roles["time"] = fmt.Sprintf(
				"content-sample (%s is VARCHAR but every value casts to TIMESTAMP, and the "+
					"name is a time candidate; read through a cast)", c)
			return c
		}
		b.roles["time"] = "UNRESOLVED: no timestamp-typed column, and no VARCHAR column both " +
			"named like a time field and holding only timestamps"
		b.refuse("no time role: the descriptor will be refused at load. Name the column by " +
			"hand, or add a field of kind \"timestamp\" reading it through a cast")
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
// Guessing from the name is what makes `duration:>100` return 0 of 1,945 rows in
// silence.
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

func orNoneStr(s string) string {
	if s == "" {
		return "no index"
	}
	return s
}

// fields renders one FieldDef per column, with the kind each column's evidence
// supports and the aliases its role's name list can safely spare.
func (b *builder) fields(group, bodyCol, timeCol string) []FieldDef {
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
			fd.From = "content-sample (VARCHAR whose every sampled value casts to TIMESTAMP)"
		}
		fd.Aliases = b.aliasesFor(c.Name, bodyCol, timeCol)
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
func (b *builder) aliasesFor(col, bodyCol, timeCol string) []string {
	var list []string
	switch col {
	case bodyCol:
		list = bodyNames
	case timeCol:
		list = timeNames
	default:
		return nil
	}
	var out []string
	for _, name := range list {
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
	for col := range b.prof.Rows {
		if strings.HasSuffix(strings.ToLower(col), suffix) {
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
	for _, c := range b.shape.Columns {
		if c.Type != "string" || c.Name == bodyCol || c.Derived != "" {
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
