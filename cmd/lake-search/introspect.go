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
                                             and re-reads DESCRIBE, to catch drift

Nothing here connects to a database. Each step prints SQL for you to run through
whatever client you already use, and the next step reads what it printed.

The profile step is what makes this worth doing. A column's declared type does
not tell you whether its values suit a role: a bag key named 'duration' holding
'47.823614ms' is a string that no numeric comparison can use, and only looking at
the values says so. Skip the profile and every type rests on the declared type
alone, which is the blind spot that answers 'duration:>100' with zero rows.
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
	window := fs.String("window", "1", "hours of history to profile; empty profiles everything")
	maxKeys := fs.Int("keys-limit", 32, "how many bag keys to list")
	bag := fs.String("bag", "", "bag column for -keys")
	keys := fs.String("keys", "", "comma-separated bag keys to profile")
	fs.Parse(args)

	shape, ok := readShape(*shapeFile, *table)
	if !ok {
		return 1
	}
	if *keys != "" {
		sql, err := databend.BagKeyProfileProbe(shape, *bag, splitCSV(*keys), *window)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Print(sql)
		return 0
	}
	sql, err := databend.ProfileProbe(shape, *window, *maxKeys)
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
	window := fs.String("window", "", "window the profile used, recorded as provenance")
	fs.Parse(args)

	shape, ok := readShape(*shapeFile, *table)
	if !ok {
		return 1
	}

	var prof databend.Profile
	if *profFile != "" {
		raw, err := os.ReadFile(*profFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		var keys []string
		prof, keys, err = databend.ParseProfile(string(raw), shape.Table)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if unprofiled := len(keys); unprofiled > 0 {
			fmt.Fprintf(os.Stderr,
				"note: %d bag keys were listed but not profiled; their types default to string. "+
					"Profile the ones you filter on: introspect profile -keys …\n", unprofiled)
		}
	} else {
		fmt.Fprintln(os.Stderr,
			"note: no -profile given, so every type rests on the declared type alone. That is "+
				"how a column of Go durations gets typed as a number.")
	}

	def, notes, err := databend.Build(shape, prof, databend.BuildOptions{Window: *window})
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
	if _, schemaNotes, err := def.Schema(); err != nil {
		fmt.Fprintln(os.Stderr, "\nthis descriptor does NOT load:", err)
		fmt.Fprintln(os.Stderr, "it is still written, so you can edit the missing part by hand.")
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
		return 0
	}
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	return 0
}

func introVerify(args []string) int {
	fs := flag.NewFlagSet("introspect verify", flag.ExitOnError)
	var sf schemaFlags
	sf.register(fs)
	fs.Parse(args)
	schema, err := sf.resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Print(databend.VerifyProbe(schema))
	return 0
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
