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

  compile [-score] [-quiet] <query>     print the WHERE predicate
  sql [-table T] [-limit N] <query>     print a complete SELECT
  conform [-file F] [-table T]          print the row-count conformance script

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
func takeFlags(args []string, flags map[string]*bool) []string {
	for len(args) > 0 {
		if args[0] == "--" {
			return args[1:]
		}
		p, ok := flags[args[0]]
		if !ok {
			break
		}
		*p = true
		args = args[1:]
	}
	return args
}

func cmdCompile(args []string) int {
	var scoreV, quietV bool
	score, quiet := &scoreV, &quietV
	rest := takeFlags(args, map[string]*bool{
		"-score": score, "--score": score,
		"-quiet": quiet, "--quiet": quiet,
	})

	q := strings.Join(rest, " ")
	schema := databend.K8sLogs()

	var (
		r   databend.Result
		err error
	)
	if *score {
		r, err = databend.CompileScore(q, schema)
	} else {
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
	table := fs.String("table", "logs.k8s_logs", "table to query")
	limit := fs.Int("limit", 100, "row limit")
	fs.Parse(args)

	// The schema carries the table too: excluding a full-text term compiles to
	// an anti-join, which has to name the same table the SELECT reads.
	schema := databend.K8sLogs()
	schema.Table = *table

	r, err := databend.CompileString(strings.Join(fs.Args(), " "), schema)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printWarnings(r)

	fmt.Printf(`SELECT ts, level, component, pod, msg
FROM %s
WHERE %s
ORDER BY ts DESC
LIMIT %d;
`, *table, r.SQL, *limit)
	return 0
}

// conformance mirrors testdata/conformance.json.
type conformance struct {
	Table string `json:"table"`
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

	schema := databend.K8sLogs()
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
