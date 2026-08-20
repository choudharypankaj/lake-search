package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/choudharypankaj/lake-search/databend"
)

// cmdIntrospect bootstraps a descriptor for a table nobody has described.
//
// It never connects to anything. `probe` and `profile` PRINT SQL, the operator
// runs it through whatever client already holds their credentials, and `build`
// turns that output into a descriptor. That keeps this binary free of a driver —
// which is what lets the compiler be vendored into a Grafana datasource plugin —
// and it means introspection works against warehouses this tool could not reach
// at all: SSO-only, air-gapped, or reachable only through a datasource that
// holds the credentials already.
//
// It is three steps rather than two, and the third is forced rather than chosen.
// The value profile cannot be written until the shape is known: its window needs
// the timestamp column, its bag-key branches need to know which columns are
// VARIANT, and its casts have to go through `::VARCHAR` because
// `TRY_CAST(ts AS DOUBLE)` is not a NULL but `[1006] unable to cast type
// Timestamp to type Float64`, which fails the whole statement.
func cmdIntrospect(args []string) int {
	if len(args) == 0 {
		introspectUsage()
		return 2
	}
	switch args[0] {
	case "probe":
		return introProbe(args[1:])
	case "profile":
		return introProfile(args[1:])
	case "build":
		return introBuild(args[1:])
	case "verify":
		return introVerify(args[1:])
	case "-h", "--help", "help":
		introspectUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown introspect step %q\n\n", args[0])
		introspectUsage()
		return 2
	}
}

func introspectUsage() {
	fmt.Fprint(os.Stderr, `lake-search introspect — build a schema descriptor for an unknown table

  1. probe   -table T                        print the shape probe; run it, save the output
  2. profile -table T -shape F [-window H]   print the value probe; run it, save the output
             [-bag B -keys k1,k2]            profile named bag keys (from the bagkey rows)
  3. build   -shape F [-profile F] [-o OUT]  write the descriptor

  verify  -schema D | -preset N              print SQL that binds every column expression
                                             and re-reads the table's shape
  verify  ... -shape F                       report the drift in that output

Nothing here connects to a database. Each step prints SQL for you to run through
whatever client you already use, and the next step reads what it printed.

The profile step is what makes this worth doing, and here is exactly what it buys.
A column's declared type does not tell you whether its values suit a role: a bag
key named 'duration' holding '47.823614ms' is a string no numeric comparison can
use, and only the values say so.

What it does NOT do is change how 'duration:>100' compiles. A numeric bound on a
bag key is TRY_CAST with or without a profile, so that query answers 0 of its
2,046 rows either way, and the compiler warns either way. What the profile buys
is knowing WHICH bounds are lossy and by how much -- it is the only thing that
can tell you the ratio is 2 of 2,046 -- plus three decisions that are otherwise
unavailable: -bag-numeric has nothing to act on without it, a mostly-empty column
cannot be kept out of 'display', and a VARCHAR holding instants cannot be
recognised as a time column.
`)
}

func introProbe(args []string) int {
	fs := flag.NewFlagSet("introspect probe", flag.ExitOnError)
	table := fs.String("table", "", "qualified table to describe")
	fs.Parse(args)
	if *table == "" {
		fmt.Fprintln(os.Stderr, "error: -table is required")
		return 2
	}
	fmt.Print(databend.ShapeProbe(*table))
	return 0
}

func introProfile(args []string) int {
	fs := flag.NewFlagSet("introspect profile", flag.ExitOnError)
	table := fs.String("table", "", "qualified table (must match the shape output)")
	shapeFile := fs.String("shape", "", "file holding the shape probe's output")
	window := fs.String("window", "1", "rolling bound: hours of history to profile")
	since := fs.String("since", "", "absolute lower bound on the time column (wins over -window)")
	until := fs.String("until", "", "absolute upper bound on the time column")
	maxKeys := fs.Int("keys-limit", 32, "how many bag keys to list")
	bag := fs.String("bag", "", "bag column for -keys")
	keys := fs.String("keys", "", "comma-separated bag keys to profile")
	fs.Parse(args)

	shape, ok := readShape(*shapeFile, *table)
	if !ok {
		return 1
	}
	win := databend.ProbeWindow{Hours: *window, Since: *since, Until: *until}
	if *keys != "" {
		sql, err := databend.BagKeyProfileProbe(shape, *bag, splitCSV(*keys), win)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Print(sql)
		return 0
	}
	sql, err := databend.ProfileProbe(shape, win, *maxKeys)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Print(sql)
	return 0
}

func introBuild(args []string) int {
	fs := flag.NewFlagSet("introspect build", flag.ExitOnError)
	table := fs.String("table", "", "qualified table (defaults to the one in the probe output)")
	shapeFile := fs.String("shape", "", "file holding the shape probe's output")
	profFile := fs.String("profile", "", "file holding the value probe's output")
	out := fs.String("o", "", "write here instead of stdout")
	window := fs.String("window", "", "rolling window the profile used, recorded as provenance")
	since := fs.String("since", "", "absolute lower bound the profile used")
	until := fs.String("until", "", "absolute upper bound the profile used")
	aliases := fs.Bool("aliases", false, "offer role name lists as field aliases (see -h)")
	timeCol := fs.String("time-column", "",
		"confirm a time-role candidate the sample was too small to promote on its own")
	minSample := fs.Int64("min-sample", 0,
		"non-null sampled values a content promotion needs before it is applied (default 1000)")
	bagNumeric := fs.Bool("bag-numeric", false,
		"declare a bag key as a number when every sampled value casts. Off by default: it "+
			"changes only the equality, and changes it from index-backed to a full scan")
	fs.Parse(args)

	shape, ok := readShape(*shapeFile, *table)
	if !ok {
		return 1
	}

	var prof databend.Profile
	var discovered []string
	if *profFile != "" {
		raw, err := os.ReadFile(*profFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		prof, discovered, err = databend.ParseProfile(string(raw), shape.Table)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		// Only the keys that were LISTED and not also profiled. Counting the
		// whole listing meant the note fired in the same run that refused
		// kv.duration *because* it was profiled — saying 32 keys went
		// unprofiled while reporting findings from all 32. A note that is
		// always wrong trains the reader to skip the notes, which are this
		// tool's only mechanism for loud degradation.
		var unprofiled []string
		for _, k := range discovered {
			if _, ok := prof.Rows[k]; !ok {
				unprofiled = append(unprofiled, k)
			}
		}
		if n := len(unprofiled); n > 0 {
			shown := unprofiled
			if n > 6 {
				shown = unprofiled[:6]
			}
			fmt.Fprintf(os.Stderr,
				"note: %d of %d discovered bag keys were not profiled, so their types default "+
					"to string (%s%s). Profile the ones you filter on: "+
					"introspect profile -keys …\n",
				n, len(discovered), strings.Join(shown, ", "),
				map[bool]string{true: ", …"}[n > 6])
		}
	} else {
		fmt.Fprintln(os.Stderr,
			"note: no -profile given, so nothing here rests on the data. Types are unaffected — "+
				"a bag key is a string either way — but no bag key can be shown to be mixed, "+
				"nothing can be suppressed from `display` for being mostly empty, and a VARCHAR "+
				"holding instants cannot be recognised as a time column.")
	}

	win := databend.ProbeWindow{Hours: *window, Since: *since, Until: *until}
	def, notes, err := databend.Build(shape, prof, databend.BuildOptions{
		Window: win.Describe(), Aliases: *aliases, KnownBagKeys: discovered,
		BagNumeric: *bagNumeric, TimeColumn: *timeCol, MinPromoteSample: *minSample})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "note:", n)
	}
	if def.Introspect != nil {
		for _, r := range def.Introspect.Refused {
			fmt.Fprintln(os.Stderr, "refused:", r)
		}
	}

	// Resolve it here rather than leaving that to the next command. A generated
	// descriptor that does not load is the normal outcome for a table missing a
	// role, and saying so now — with the same message the loader would give —
	// is the difference between a tool that reports and one that emits a file
	// and lets a panel find out.
	//
	// The file is still written, because the fastest fix is usually to edit one
	// line of it. But the exit status is a failure, so that
	// `introspect build && deploy` cannot ship a descriptor that does not load.
	loads := true
	if def.Introspect != nil && len(def.Introspect.Blocked) > 0 {
		// Not a load failure — a decision the tool declined to make. The
		// descriptor is valid; it is just missing the part a human has to
		// supply, so the exit status says "do not deploy this yet".
		loads = false
		fmt.Fprintf(os.Stderr,
			"\n%d role(s) were declined for want of evidence; see `blocked` in the descriptor. "+
				"It is written and it loads, but the exit status is 1 so a script does not "+
				"deploy it.\n", len(def.Introspect.Blocked))
	}
	if _, schemaNotes, err := def.Schema(); err != nil {
		loads = false
		fmt.Fprintln(os.Stderr, "\nthis descriptor does NOT load:", err)
		fmt.Fprintln(os.Stderr, "it is written anyway so you can edit the missing part by hand, "+
			"but the exit status is 1 so a script does not deploy it.")
	} else {
		for _, n := range schemaNotes {
			fmt.Fprintln(os.Stderr, "schema note:", n)
		}
	}

	blob, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	blob = append(blob, '\n')
	if *out == "" {
		os.Stdout.Write(blob)
	} else {
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	}
	if !loads {
		return 1
	}
	return 0
}

func introVerify(args []string) int {
	fs := flag.NewFlagSet("introspect verify", flag.ExitOnError)
	var sf schemaFlags
	sf.register(fs)
	shapeFile := fs.String("shape", "",
		"output of the verify probe; given, drift is REPORTED instead of the probe being printed")
	fs.Parse(args)
	schema, err := sf.resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *shapeFile == "" {
		fmt.Print(databend.VerifyProbe(schema))
		return 0
	}

	raw, err := os.ReadFile(*shapeFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	shape, err := databend.ParseShape(string(raw), schema.Table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	findings := databend.Drift(shape, schema, sf.digest())
	if len(findings) == 0 {
		fmt.Printf("%s: no drift. %d columns and %d indexes, all declared.\n",
			shape.Table, len(shape.Columns), len(shape.Indexes))
		return 0
	}
	fmt.Printf("%s: %d drift finding(s).\n\n", shape.Table, len(findings))
	for _, f := range findings {
		fmt.Println("  \u2022", f)
	}
	fmt.Println("\nRe-run `introspect probe` and `build` to regenerate, or edit the descriptor.")
	return 1
}

func readShape(path, table string) (databend.Shape, bool) {
	if path == "" {
		fmt.Fprintln(os.Stderr, "error: -shape is required (the output of `introspect probe`)")
		return databend.Shape{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return databend.Shape{}, false
	}
	shape, err := databend.ParseShape(string(raw), table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return databend.Shape{}, false
	}
	return shape, true
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
