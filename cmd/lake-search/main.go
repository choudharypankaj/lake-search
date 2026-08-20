// Command lake-search compiles Lucene-style search text into Databend SQL.
//
//	lake-search compile 'level:error snapshoot~1'
//	lake-search sql -limit 50 'component:tikv "peer status"'
//	lake-search conform > conformance.sql
//
// It has no database driver and makes no network calls: it prints SQL for you
// to run through whatever client you already use (lakesql, the Grafana panel,
// the REST endpoint). That keeps the module dependency-free so it can be
// vendored into the Grafana datasource plugin without dragging anything along.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/choudharypankaj/lake-search/databend"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "compile":
		os.Exit(cmdCompile(os.Args[2:]))
	case "sql":
		os.Exit(cmdSQL(os.Args[2:]))
	case "conform":
		os.Exit(cmdConform(os.Args[2:]))
	case "schema":
		os.Exit(cmdSchema(os.Args[2:]))
	case "introspect":
		os.Exit(cmdIntrospect(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lake-search — Lucene-style search compiled to Databend SQL

  compile [-score] [-score-expr] [-quiet] <query>
                                        print the WHERE predicate, or with
                                        -score-expr the select-list ranking
                                        expression that goes beside it
  sql [-table T] [-limit N] [-score] <query>
                                        print a complete SELECT
  conform [-file F] [-table T]          print the row-count conformance script
  schema                                validate the schema and describe it
  introspect probe|profile|build        bootstrap a descriptor for an unknown
                                        table, by printing SQL for you to run

Every command takes -schema FILE or -preset NAME, or reads LAKE_SEARCH_SCHEMA.
A schema is data: it names your table, its columns and their kinds, so pointing
this at your own log table is a file rather than a patch.

Warnings go to stderr, SQL to stdout, so output can be piped safely.
`)
}

// takeFlags pulls known flags off the front of the argument list and returns
// the rest verbatim as the query.
//
// The standard flag package cannot be used here: `lake-search compile -TiFlash`
// is a perfectly ordinary search meaning "exclude TiFlash", and flag parsing
// would reject it as an unknown flag. A search tool whose syntax collides with
// its own CLI is a bad tool, so the flags are matched explicitly and everything
// else — leading dash or not — is search text.
func takeFlags(args []string, flags map[string]*bool, strs map[string]*string) []string {
	for len(args) > 0 {
		if args[0] == "--" {
			return args[1:]
		}
		if p, ok := flags[args[0]]; ok {
			*p = true
			args = args[1:]
			continue
		}
		if p, ok := strs[args[0]]; ok {
			if len(args) < 2 {
				fmt.Fprintf(os.Stderr, "error: %s needs a value\n", args[0])
				os.Exit(2)
			}
			*p = args[1]
			args = args[2:]
			continue
		}
		break
	}
	return args
}

// schemaFlags is the schema selector every command shares.
//
// The default is the shipped preset, so the tool still works with no arguments
// against the table it was built for. Everything else is data: -schema names a
// file, -preset names a built-in, and LAKE_SEARCH_SCHEMA is the same file for a
// deployment that would rather not repeat the flag.
type schemaFlags struct {
	file   string
	preset string
}

func (sf *schemaFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&sf.file, "schema", "", "schema definition file (JSON)")
	fs.StringVar(&sf.preset, "preset", "", "built-in schema preset: "+
		strings.Join(databend.PresetNames(), ", "))
}

func (sf *schemaFlags) strFlags() map[string]*string {
	return map[string]*string{
		"-schema": &sf.file, "--schema": &sf.file,
		"-preset": &sf.preset, "--preset": &sf.preset,
	}
}

// resolve loads the schema and prints the notes it earned.
//
// The notes are printed unconditionally, and that is the point of them. A
// schema with no VARIANT bag turns `store_id:7` into a compile error; a schema
// with no severity field leaves a log panel unable to colour anything. Both are
// legal shapes and neither is visible at query time, so the loader says so once,
// out loud, when the file is read — the alternative is a user discovering it
// from an empty panel.
func (sf *schemaFlags) resolve() (databend.Schema, error) {
	file, preset := sf.file, sf.preset
	if file == "" && preset == "" {
		file = os.Getenv("LAKE_SEARCH_SCHEMA")
	}
	switch {
	case file != "" && preset != "":
		return databend.Schema{}, fmt.Errorf("give -schema or -preset, not both")
	case file != "":
		s, notes, err := databend.LoadSchema(file)
		printNotes(file, notes)
		return s, err
	case preset != "":
		s, notes, err := databend.Preset(preset)
		printNotes("preset "+preset, notes)
		return s, err
	default:
		return databend.K8sLogs(), nil
	}
}

func printNotes(src string, notes []string) {
	for _, n := range notes {
		fmt.Fprintf(os.Stderr, "schema %s: %s\n", src, n)
	}
}

func cmdCompile(args []string) int {
	var scoreV, exprV, quietV bool
	var sf schemaFlags
	score, expr, quiet := &scoreV, &exprV, &quietV
	rest := takeFlags(args, map[string]*bool{
		"-score": score, "--score": score,
		"-score-expr": expr, "--score-expr": expr,
		"-quiet": quiet, "--quiet": quiet,
	}, sf.strFlags())

	q := strings.Join(rest, " ")
	schema, err := sf.resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	var r databend.Result
	switch {
	case *expr:
		// The select-list half of a relevance panel. It is a separate call
		// rather than a second field on one result because the two expand in
		// different places in the statement, through different macros.
		r, err = databend.CompileScoreExpr(q, schema)
	case *score:
		r, err = databend.CompileScore(q, schema)
	default:
		r, err = databend.CompileString(q, schema)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !*quiet {
		printWarnings(r)
	}
	fmt.Println(r.SQL)
	return 0
}

func cmdSQL(args []string) int {
	fs := flag.NewFlagSet("sql", flag.ExitOnError)
	var sf schemaFlags
	sf.register(fs)
	table := fs.String("table", "", "table to query, overriding the schema's")
	limit := fs.Int("limit", 100, "row limit")
	score := fs.Bool("score", false, "select a BM25 relevance column and order by it")
	fs.Parse(args)

	schema, err := sf.resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// The schema carries the table too: excluding a full-text term compiles to
	// an anti-join, which has to name the same table the SELECT reads.
	if *table != "" {
		schema.Table = *table
	}
	if schema.Table == "" {
		fmt.Fprintln(os.Stderr, "error: no table: give -table or a schema that names one")
		return 1
	}
	q := strings.Join(fs.Args(), " ")

	if *score {
		return sqlScore(q, schema, *limit)
	}

	r, err := databend.CompileString(q, schema)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printWarnings(r)

	// The select list and the sort come from the schema's roles rather than
	// from a literal written here. `SELECT ts, level, component, pod, msg` was
	// what used to be printed, which made the library schema-driven and the
	// command that emits SQL not.
	fmt.Printf(`SELECT %s
FROM %s
WHERE %s%s
LIMIT %d;
`, strings.Join(schema.DisplayColumns(), ", "), schema.Table, r.SQL,
		orderBy(schema, ""), *limit)
	return 0
}

// orderBy renders the sort a log view wants, newest first, from the schema's
// time role. A schema that declares no time field gets no ORDER BY rather than
// a guess: guessing is how a panel ends up sorted by a column that is not time.
func orderBy(schema databend.Schema, lead string) string {
	var parts []string
	if lead != "" {
		parts = append(parts, lead)
	}
	if col := schema.TimeColumn(); col != "" {
		parts = append(parts, col+" DESC")
	}
	if len(parts) == 0 {
		return ""
	}
	return "\nORDER BY " + strings.Join(parts, ", ")
}

// sqlScore prints the relevance-panel shape, which is the only shape where the
// select list and the predicate have to be compiled together.
//
// score() is legal only alongside a search function, so a search that compiles
// to structured SQL — `component:tikv`, `snapsh*`, `level:ERROR` — must select
// a constant instead. Selecting score() unconditionally is [1065]; discarding
// the predicate instead, which is what this tool used to do, is an empty panel
// with the user's filter silently thrown away.
func sqlScore(q string, schema databend.Schema, limit int) int {
	pred, err := databend.CompileScore(q, schema)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	expr, err := databend.CompileScoreExpr(q, schema)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printWarnings(pred)
	printWarnings(databend.Result{Warnings: extraWarnings(pred.Warnings, expr.Warnings)})

	fmt.Printf(`SELECT %s AS relevance, %s
FROM %s
WHERE %s%s
LIMIT %d;
`, expr.SQL, strings.Join(schema.DisplayColumns(), ", "), schema.Table, pred.SQL,
		orderBy(schema, "relevance DESC"), limit)
	return 0
}

// extraWarnings returns the warnings the score expression raised that the
// predicate did not, so the same advisory is not printed twice.
func extraWarnings(already, all []string) []string {
	seen := make(map[string]bool, len(already))
	for _, w := range already {
		seen[w] = true
	}
	var out []string
	for _, w := range all {
		if !seen[w] {
			out = append(out, w)
		}
	}
	return out
}

// conformance mirrors testdata/conformance.json.
type conformance struct {
	Table string `json:"table"`

	// Preset and Schema name the schema the fixture's queries are written
	// against. A suite that asserts on a derived column is meaningless under a
	// schema that does not declare one, so the fixture says which it needs
	// instead of inheriting whatever the CLI defaults to.
	Preset string `json:"preset,omitempty"`
	Schema string `json:"schema,omitempty"`

	Note  string `json:"note"`
	Cases []struct {
		Name     string `json:"name"`
		Query    string `json:"query"`
		Baseline string `json:"baseline"`
		Witness  string `json:"witness"`
		Compare  string `json:"compare"`
		Note     string `json:"note"`
	} `json:"cases"`
}

// cmdConform turns the fixtures into a SQL script whose output is a PASS/FAIL
// column per case.
//
// This exists because of the rule in LOG_PIPELINE_FINDINGS.md §5.10: on this
// engine a wrong query is indistinguishable from an empty result, so a harness
// must assert on row counts and never on "the query executed successfully".
// Baselines are expressed as other searches rather than fixed numbers, so the
// suite keeps working as the table grows.
func cmdConform(args []string) int {
	fs := flag.NewFlagSet("conform", flag.ExitOnError)
	var sf schemaFlags
	sf.register(fs)
	file := fs.String("file", "testdata/conformance.json", "fixture file")
	table := fs.String("table", "", "override the table named in the fixture")
	fs.Parse(args)

	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var c conformance
	if err := json.Unmarshal(raw, &c); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *table != "" {
		c.Table = *table
	}

	// The fixture's own schema wins unless the caller names one on the command
	// line, so `lake-search conform -file X` always runs X against the schema
	// X was written for.
	if sf.file == "" && sf.preset == "" {
		sf.file, sf.preset = c.Schema, c.Preset
	}
	schema, err := sf.resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	schema.Table = c.Table
	fmt.Printf("-- lake-search conformance suite over %s\n", c.Table)
	fmt.Printf("-- %s\n--\n", wrapComment(c.Note))
	fmt.Println("-- Every statement below prints PASS or FAIL. Any FAIL is a real defect:")
	fmt.Println("-- either the compiler or the engine, and the note says which is suspected.")

	failures := 0
	for _, tc := range c.Cases {
		pred, err := databend.CompileString(tc.Query, schema)
		if err != nil {
			fmt.Fprintf(os.Stderr, "case %q: %v\n", tc.Name, err)
			failures++
			continue
		}

		var basePred string
		if tc.Baseline != "" {
			b, err := databend.CompileString(tc.Baseline, schema)
			if err != nil {
				fmt.Fprintf(os.Stderr, "case %q baseline: %v\n", tc.Name, err)
				failures++
				continue
			}
			basePred = b.SQL
		}

		var witPred string
		if tc.Witness != "" {
			w, err := databend.CompileString(tc.Witness, schema)
			if err != nil {
				fmt.Fprintf(os.Stderr, "case %q witness: %v\n", tc.Name, err)
				failures++
				continue
			}
			witPred = w.SQL
		}

		cond, err := condition(tc.Compare, basePred != "", witPred != "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "case %q: %v\n", tc.Name, err)
			failures++
			continue
		}

		fmt.Printf("\n-- %s\n", tc.Name)
		if tc.Note != "" {
			fmt.Printf("-- %s\n", wrapComment(tc.Note))
		}
		fmt.Printf("--   query: %s\n", displayQuery(tc.Query))
		if tc.Baseline != "" {
			fmt.Printf("--   baseline: %s\n", displayQuery(tc.Baseline))
		}
		if tc.Witness != "" {
			fmt.Printf("--   witness: %s\n", displayQuery(tc.Witness))
		}
		// The counts are derived tables rather than select-list subqueries so
		// the CASE can reference them. A select-list alias is not reliably
		// visible to a sibling expression, and a suite that errors on every
		// case is worse than no suite at all.
		fmt.Printf("SELECT '%s' AS case_name,\n", sqlLiteral(tc.Name))
		fmt.Printf("       a.actual,\n")
		if basePred != "" {
			fmt.Printf("       b.baseline,\n")
		}
		if witPred != "" {
			fmt.Printf("       c.witness,\n")
		}
		fmt.Printf("       CASE WHEN %s THEN 'PASS' ELSE 'FAIL' END AS result\n", cond)
		fmt.Printf("FROM (SELECT count(*) AS actual FROM %s WHERE %s) a", c.Table, pred.SQL)
		if basePred != "" {
			fmt.Printf(",\n     (SELECT count(*) AS baseline FROM %s WHERE %s) b", c.Table, basePred)
		}
		if witPred != "" {
			fmt.Printf(",\n     (SELECT count(*) AS witness FROM %s WHERE %s) c", c.Table, witPred)
		}
		fmt.Println(";")
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "\n%d case(s) failed to compile\n", failures)
		return 1
	}
	return 0
}

// condition renders the comparison as SQL over the two scalar subqueries.
func condition(compare string, hasBaseline, hasWitness bool) (string, error) {
	needsBaseline := func() error {
		if !hasBaseline {
			return fmt.Errorf("compare %q needs a baseline", compare)
		}
		return nil
	}

	switch compare {
	case "executes":
		// For cases whose point is that the statement parses at all — escaping
		// and injection safety — where the row count is incidental.
		return "a.actual >= 0", nil
	case "zero":
		return "a.actual = 0", nil
	case "nonzero":
		return "a.actual > 0", nil
	case "eq", "le", "ge", "lt", "gt", "ne":
		if err := needsBaseline(); err != nil {
			return "", err
		}
		ops := map[string]string{"eq": "=", "ne": "<>", "le": "<=", "ge": ">=", "lt": "<", "gt": ">"}
		// A baseline of zero would make every comparison vacuously true, so
		// the assertion also requires the baseline to have found something.
		return fmt.Sprintf("b.baseline > 0 AND a.actual %s b.baseline", ops[compare]), nil
	case "narrows":
		// Same as "le", plus the half that matters on this engine: a
		// mistranslated query returns zero rows, and zero is <= any baseline,
		// so a plain "le" passes for exactly the failure the suite exists to
		// catch. Only use it where the two searches genuinely co-occur.
		if err := needsBaseline(); err != nil {
			return "", err
		}
		return "b.baseline > 0 AND a.actual > 0 AND a.actual <= b.baseline", nil
	case "shrinks":
		// "narrows" with the equality taken away, which is the difference
		// between an assertion and a formality wherever the two searches are
		// meant to ask *different* questions.
		//
		// It exists because of a case that could not fail. `pod:tikv-tikv-??????`
		// was asserted to narrow `pod:tikv-tikv-*`, and the two are provably
		// the same 189,623 rows on this table — every pod with that prefix has
		// exactly six characters after it — so "narrows" degenerated to
		// "nonzero" and would have passed just as well if `?` had compiled to
		// `%`. The same shape hid the wildcard over-match: a token wildcard
		// bounded only from below by `ge` passes for any superset, `1=1`
		// included.
		//
		// So: strictly fewer rows than the baseline, and not zero. Both halves
		// are load-bearing — `lt` alone is satisfied by the empty result this
		// engine returns for a mistranslated query.
		if err := needsBaseline(); err != nil {
			return "", err
		}
		return "b.baseline > 0 AND a.actual > 0 AND a.actual < b.baseline", nil
	case "partitions":
		// The strongest form available: actual and witness must be exactly
		// complementary halves of the baseline, so `a -b` is checked against
		// `a` and `a AND b` rather than merely being "not bigger than a".
		// Both halves must be nonempty or the identity is trivial.
		if err := needsBaseline(); err != nil {
			return "", err
		}
		if !hasWitness {
			return "", fmt.Errorf("compare %q needs a witness", compare)
		}
		return "a.actual > 0 AND c.witness > 0 AND a.actual + c.witness = b.baseline", nil
	default:
		return "", fmt.Errorf("unknown compare %q", compare)
	}
}

// cmdSchema validates a schema and prints what it resolved to.
//
// It exists so that a deployment writing its own schema file finds out what is
// wrong from a command, at the moment it writes the file, rather than from a
// panel that renders nothing an hour later. Everything printed here is derived:
// which index makes a field full-text, whether a column's LIKE searches are
// index-backed, how many words that index deletes. Those are the three facts
// that decide whether a search returns the right rows, and none of them is
// visible in the file itself.
func cmdSchema(args []string) int {
	fs := flag.NewFlagSet("schema", flag.ExitOnError)
	var sf schemaFlags
	sf.register(fs)
	asJSON := fs.Bool("json", false, "emit the resolved schema as JSON instead of prose")
	fs.Parse(args)

	schema, err := sf.resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if *asJSON {
		// The machine-readable form exists so that other tools do not have to
		// re-implement resolution. The dashboard generator is the one that
		// needs it: it is Python, the presets are Go constants, and the
		// alternative was a second copy of the built-in table's column list
		// maintained by hand in the generator — which is exactly the coupling
		// this round set out to remove.
		//
		// It emits the *resolved* schema, so aliases are expanded and every
		// field carries the column expression it actually compiles to.
		out, err := json.MarshalIndent(resolvedSchema(schema), "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println(string(out))
		return 0
	}

	fmt.Printf("table:            %s\n", orNone(schema.Table))
	fmt.Printf("default field:    %s\n", schema.Default)
	fmt.Printf("time field:       %s\n", orNone(schema.Time))
	fmt.Printf("severity field:   %s\n", orNone(schema.Severity))
	fmt.Printf("bags:             %s\n", orNone(bagSummary(schema)))
	fmt.Printf("time zone:        %s\n", orDefault(schema.TimeZone, "session"))
	fmt.Printf("case-insensitive: %t\n", schema.CaseInsensitive)
	fmt.Printf("display:          %s\n", strings.Join(schema.DisplayColumns(), ", "))

	fmt.Println("\nindexes:")
	if len(schema.Indexes) == 0 {
		fmt.Println("  (none declared — full-text fields cannot be checked)")
	}
	for _, ix := range schema.Indexes {
		fmt.Printf("  %-12s %-8s (%s)", ix.Name, ix.Kind, strings.Join(ix.Columns, ", "))
		if ix.Tokenizer != "" {
			fmt.Printf(" tokenizer=%s", ix.Tokenizer)
		}
		if len(ix.Filters) > 0 {
			fmt.Printf(" filters=%s", strings.Join(ix.Filters, ","))
		}
		fmt.Println()
	}

	fmt.Println("\nfields:")
	for _, name := range sortedNames(schema) {
		f := schema.Fields[name]
		fmt.Printf("  %-14s %-10s %s", name, databend.KindName(f.Kind), f.Column)
		if f.Index != "" {
			fmt.Printf(" [inverted %s]", f.Index)
		}
		if f.Ngram {
			fmt.Print(" [ngram]")
		}
		if n := len(f.StopWords); n > 0 {
			fmt.Printf(" [%d stopwords]", n)
		}
		fmt.Println()
	}
	return 0
}

// resolvedSchema is the JSON shape `schema -json` prints. It is deliberately
// flat and role-first: a consumer wants to ask "does this table have a severity
// column, and what is it called", not to re-run field resolution.
type resolved struct {
	Table    string            `json:"table"`
	Default  string            `json:"default"`
	Time     string            `json:"time,omitempty"`
	Severity string            `json:"severity,omitempty"`
	Display  []string          `json:"display"`
	Bags     []resolvedBag     `json:"bags,omitempty"`
	Fields   map[string]rField `json:"fields"`
}

type resolvedBag struct {
	Column string `json:"column"`
	Prefix string `json:"prefix,omitempty"`
	Index  string `json:"index,omitempty"`
}

type rField struct {
	Column string `json:"column"`
	Kind   string `json:"kind"`
	Index  string `json:"index,omitempty"`
	Ngram  bool   `json:"ngram,omitempty"`

	// Carried through so the dashboard generator can put a real value in the
	// help text instead of the word "value".
	Example string `json:"example,omitempty"`
}

func resolvedSchema(s databend.Schema) resolved {
	r := resolved{
		Table: s.Table, Default: s.Default, Time: s.Time, Severity: s.Severity,
		Display: s.Display, Fields: make(map[string]rField, len(s.Fields)),
	}
	if r.Display == nil {
		r.Display = []string{}
	}
	for name, f := range s.Fields {
		r.Fields[name] = rField{
			Column: f.Column, Kind: databend.KindName(f.Kind),
			Index: f.Index, Ngram: f.Ngram, Example: f.Example,
		}
	}
	for _, b := range s.Bags {
		r.Bags = append(r.Bags, resolvedBag{Column: b.Column, Prefix: b.Prefix, Index: b.Index})
	}
	return r
}

func sortedNames(s databend.Schema) []string {
	names := make([]string, 0, len(s.Fields))
	for n := range s.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// bagSummary says, per bag, the two things that decide what a bag key search
// does: whether it is reachable through an index, and how it is addressed.
func bagSummary(s databend.Schema) string {
	var out []string
	for _, b := range s.Bags {
		d := b.Column
		if b.Prefix != "" {
			d += " prefix=" + b.Prefix
		} else {
			d += " (catch-all)"
		}
		if b.Index != "" {
			d += " [inverted " + b.Index + "]"
		} else {
			d += " [not indexed: key search is a full scan]"
		}
		if n := len(b.Keys); n > 0 {
			d += fmt.Sprintf(" %d typed key(s)", n)
		}
		out = append(out, d)
	}
	return strings.Join(out, "; ")
}

func orNone(s string) string { return orDefault(s, "(none)") }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func printWarnings(r databend.Result) {
	for _, w := range r.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
}

func sqlLiteral(s string) string { return strings.ReplaceAll(s, "'", "''") }

// displayQuery renders a query inside a SQL line comment. An empty query is
// shown explicitly, since "no query" is itself one of the cases under test.
func displayQuery(q string) string {
	if strings.TrimSpace(q) == "" {
		return "(empty)"
	}
	return strings.ReplaceAll(q, "\n", " ")
}

func wrapComment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "--", "-")
}
