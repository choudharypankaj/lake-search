package databend

import (
	"fmt"
	"strings"

	"github.com/choudharypankaj/lake-search/parser"
)

// A logging call site is a term shape of its own, and this file is the rule that
// recognises it.
//
// # The defect
//
// Every unified TiDB/TiKV/PD/TiCDC line carries its call site in brackets:
//
//	[2026/08/20 16:20:59.070 +00:00] [INFO] [compaction_runner.rs:360] ["collected 0 …"]
//
// A reader can see `compaction_runner.rs:360`, so a reader types it, and both
// ways of typing it answered zero. Measured over the closed window
// [2026-08-20 16:00:00, 16:25:00) — 8,142 rows, 30 of them from that file and 15
// from line 360 of it:
//
//	rows matching source_file LIKE '%compaction_runner%'                    30
//	                     … with `compaction_runner` in line                  0
//	                     … with `compaction_runner` in msg                   0
//	query('source_file:compaction_runner.rs')                               30
//	query('source_file:"compaction_runner.rs:360"')                         15
//	query('line:compaction_runner.rs')                                       0
//
// Two separate causes with one symptom. The collector parses the call site into
// its own column, and the derived text surface is the message plus the BAG's
// values, so the file position is in no surface a bare word reaches. And
// `compaction_runner.rs:360` parses as the field `compaction_runner.rs` with the
// value `360`, which is not a column, so it takes the VARIANT path and compiles
// to a lookup for a bag key nothing writes:
//
//	query('kv.compaction_runner.rs:360') AND lower(COALESCE(kv['compaction_runner.rs'],
//	  kv['compaction_runner']['rs'])::VARCHAR) = lower('360')          0 rows
//
// It warned, and the warning rode along as a SQL comment, which a Grafana panel
// does not show. Silent zero either way.
//
// # Why it is a UNION and not a redirect
//
// The obvious fix — send the term to the source-file column instead — is wrong,
// and this table refutes it rather than a hypothetical one. Three log formats
// arrive here and they do not agree on where the call site ends up. Same window,
// same 8,142 rows:
//
//	term                      query('line:…')   query('source_file:…')   union
//	compaction_runner.rs (tikv)             0                       30      30
//	compaction_runner.rs:360                0                       15      15
//	reflector.go (csi-driver)               0                       69      69
//	factory.go (named in a message)        69                        0      69
//	warnings.go:110 (controller)           32                        0      32
//
// The bracket format (tikv, tidb) has its position parsed out, so only
// `source_file` holds it. A logfmt line — `caller=…/rest/warnings.go:110` from a
// controller, `source=compact.go:565` from prometheus — has no bracket to parse,
// so `source_file` is empty and the position survives in the bag, and therefore
// in the derived line. klog (`E0820 … 1 reflector.go:205] "Failed to watch" …`)
// puts one file in the column and names others in the message.
//
// So a redirect would have answered 0 of 69 for `factory.go` and 0 of 32 for
// `warnings.go:110`, which is the same class of silent loss it was written to
// remove — and 143 of those 8,142 rows carry a `.rs`/`.go` file-shaped token in
// the text, so this is not a corner. Over the wider closed window
// [2026-08-20 00:00:00, 16:00:00) (286,604 rows) the same two terms are 2,559 of
// 2,559 and 1,280 of 1,280, with `warnings.go` splitting 1,280 in the text against
// 128 in the column and `compact.go` 19 against 16 — the two sides disjoint, since
// 1,280 + 128 = 1,408 and 19 + 16 = 35 are what the unions return.
//
// The text half is therefore never dropped, which TestSourceFileKeepsTheTextSurface
// pins structurally rather than by counting. It is not a strict superset of what
// the default surface answered before — the literal comparison below narrows it in
// one measured case — but it is never absent.
//
// Both halves go inside ONE query() call, which the engine allows because
// `source_file` sits in the same inverted index group as msg, line and kv.
// Measured, `query('(line:"compaction_runner.rs:360") OR (source_file:
// "compaction_runner.rs:360")')` is 15 and still composes with a term and a
// column filter.
//
// # Why the search function alone is not the answer
//
// The first version of this rule stopped at the disjunction above, and it was
// wrong in the other direction. `source_file:` is a TOKEN search and this analyzer
// splits on `_` as well as on `.`, so a name that ENDS another name matches both.
// Over `ts < '2026-08-20 16:25:00'` (1,292,338 rows):
//
//	query('source_file:manager.go')        513,055
//	source_file LIKE 'manager.go%'             199   the file itself
//	source_file LIKE '%_manager.go:%'      512,856   files ending with the name
//
// and 199 + 512,856 = 513,055 exactly, so the whole of the excess is the suffix
// family. A search for one file name returning 40% of the table is not a usable
// answer, and it is the same defect class as the silent zero it replaced: a wrong
// answer a reader cannot tell is wrong.
//
// The tool that fixes it is the one a95e957 landed: a single search function
// prunes the scan while ordinary predicates refine it. So the search function
// stays — it is what makes this cheap — and a literal comparison is ANDed onto it.
// See sourceFileFragment for the shapes, the oracle and the cost.
//
// The explicitly-scoped spellings are untouched: `source_file:manager.go` typed by
// hand is still a token search, still 513,055, and the gate pins it. Only the
// spelling this rule owns — the bare file name — is made exact.
//
// # The shape rule
//
// `name.ext` is a file name and `name.ext:digits` is a file position: name
// segments of file-name characters, an all-letters extension, and — for the
// position — a line number. Nothing is compared against a list of known
// extensions; see isSourceExtension for why the structural rule is both safer and
// the only one this data can justify.
//
// The tempting broader rule is "fire whenever the field is not a column", which
// needs no shape test at all. It is wrong: an undeclared name is the ORDINARY
// case on a log table, because the bag exists precisely so that `store_id:7` and
// `tableID:123` work without a declaration. Firing there would take over every
// bag lookup whose value happens to be numeric — measured, the key `tableID`
// alone is on 4,469 of the 8,142 rows in the window above. The shape is the only
// evidence available at compile time that a name is a file rather than a key.
//
// Three things outrank the shape, in this order, and each is a case where
// somebody has SAID what they meant: a declared field or alias, a bag addressed
// by its column name or prefix, and a bag key declared under exactly that
// spelling.
//
// # What the bag prefix costs, stated because it is not free
//
// The reference table names its bag column `kv`, and TiKV and TiDB both have
// source files called `kv.rs` and `kv.go`. So `kv.go:350` is ambiguous by
// construction: the bag reading is the key `go`, the file reading is the file
// `kv.go`. The bag wins, because the prefix is explicit and because the
// alternative cannot be spelled without an extension allowlist — `kv.go:350` and
// `kv.tableID:811` are the same shape, and only a list of known extensions
// separates them. That leaves four values unreachable by their own spelling
// (`kv.rs:1075`, `kv.go:350`, `kv.rs:846`, `kv.rs:802` — 1,894 rows across the
// whole table), each of them reachable as `source_file:"kv.go:350"` or
// `file:"kv.go:350"`. A deployment whose bag is called `attrs` has no such
// collision.
//
// Dropping the bag reading for a dotted name costs nothing measurable here, which
// is the other half of that decision: over that same window the bag holds 32,776
// key occurrences across 62 distinct keys and NONE of them contains a dot, so
// `kv['foo.bar']` and the nested `kv['foo']['bar']` are both empty for every
// dotted name a user can type.

// isSourceExtension reports whether ext is the extension half of a source file
// name: one or more letters and nothing else.
//
// STRUCTURAL, not a list of known extensions, and the reason is that this
// warehouse cannot tell the two apart. Its source_file column holds 972 distinct
// values (`ts < '2026-08-20 16:25:00'`, 1,292,338 rows) and exactly two extensions
// occur in any of them: 745 `.go` positions and 223 `.rs`. No `.cc`, no `.py`, no
// `.java`, no `.h`, anywhere. So a rule keyed on `{go, rs}` would pass every test
// this data can pose while being wrong the first time a C++ or Python component
// logs into the table — a silent zero, on a cluster nobody was looking at, months
// later.
//
// The answer for an extension this cluster has never emitted is therefore: it
// behaves exactly like `.rs`. `db_impl_compaction_flush.cc:1042` and
// `handler.py:88` expand the same way, with no code change, because the rule
// reads the shape rather than a list somebody remembered to extend.
//
// What makes the loose rule safe is that the term is EXPANDED and never
// redirected. A false positive — `foo.bar`, `example.com`, `k8s_logs.ts` (a
// column reference in a logged statement, four rows over
// [2026-08-20 00:00:00, 16:00:00)) — adds a
// disjunct on a column that holds no such token, so it contributes nothing and
// takes nothing away. Under a redirect the same false positive would have thrown
// the answer away, and then the allowlist would have been load-bearing.
//
// Requiring LETTERS is the part that carries weight, and it is what keeps the
// near-misses out: `10.0.0.1`, `10.0.0.1:8080`, `192.168.176.28`, `v8.5.7` and
// `v8.5.7:360` all end in a digit run, so none of them is a file.
func isSourceExtension(ext string) bool {
	if ext == "" {
		return false
	}
	for i := 0; i < len(ext); i++ {
		switch c := ext[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// sourceFileFragment compiles a term that reads as a source file, or reports
// ok=false when the term is not one.
//
// # The shape, and why it is a search function AND a literal filter
//
// The search function is a SUPERSET that prunes the scan; the literal comparison
// beside it is what makes the answer exact. That is the tool a95e957 landed — one
// search function per scan, with ordinary predicates composing freely around it —
// and it is the only way to have both here, because the query language cannot
// anchor and a LIKE cannot prune.
//
// It has to be both, because the token search alone is not a usable answer. Over
// `ts < '2026-08-20 16:25:00'` (1,292,338 rows):
//
//	                              token union   exact truth   this compiler
//	manager.go                        514,446         1,590           1,590
//	client.go                          41,086        17,487          17,487
//	compaction_runner.rs                3,790         3,790           3,790
//	compaction_runner.rs:360            1,571         1,571           1,571
//	warnings.go                         3,810         3,810           3,810
//	server.go:342                       1,720         1,720           1,720
//	tidb_monitor_controller.go:101      7,752         7,752           7,752
//
// `manager.go` was 2,584× too many rows — 40% of the table — because this
// analyzer splits on `_` as well as on `.`, so the token pair [manager, go] is
// inside every `*_manager.go`. Exactly: `source_file LIKE 'manager.go%'` is 199
// and `LIKE '%_manager.go:%'` is 512,856, and 199 + 512,856 = 513,055 is the token
// search. The truth column above is `source_file LIKE 'X%' OR line LIKE '%X%'`
// computed with no compiler in the loop, and this compiler now equals it on all
// nine terms measured.
//
// Cost, same window, best of three runs:
//
//	                                   compaction_runner.rs   manager.go
//	search function alone (was)                      0.12s        0.17s
//	search function AND filter (is)                  0.24s        0.39s
//	filter alone, no search function                 0.39s        0.39s
//
// So the refinement roughly doubles a sub-second query, and the search function is
// still earning its place: 0.24s against 0.39s for the same rows when the term is
// selective. On `manager.go` the two are equal, because a token search matching
// 40% of the table prunes nothing — which is the same fact as the defect.
//
// # What the filter is
//
//	a position  source_file = 'X'        OR source_file LIKE '%/X'
//	a name      source_file LIKE 'X%'    OR source_file LIKE '%/X%'
//	either      OR line LIKE '%X%'
//
// The `%/X` disjuncts are what keep this honest on a deployment that stores a path
// — `components/raftstore/src/peer.rs:100` — because the role promises only that
// the field holds a call site, not that it begins with a basename. Neither
// disjunct is index-served; the search function beside them is.
//
// `LIKE 'X%'` rather than `LIKE 'X:%'` is deliberate: it makes no assumption about
// the separator between the name and the line, so `file.rs:360`, `file.rs 360` and
// a bare `file.rs` all match. What it costs is a file whose name is a longer
// dotted name beginning with the one typed — `client.go` would also answer for a
// hypothetical `client.gopher` — and this table has no such pair among its 262
// distinct basenames (the five prefix pairs it does have all involve `raft`, which
// has no dot and so cannot reach this rule).
//
// # The text half is an intersection, and that is the point
//
// The filter's prose disjunct is `line LIKE '%X%'`, ANDed with the analyzed match
// the search function makes. Neither test is right on its own, and their blind
// spots are opposite:
//
//	the ANALYZER splits on `-` and `.` alike, so it cannot tell the Go module
//	`client-go` from the file `client.go`. Measured, `query('line:client.go')` is
//	16,501 rows and the literal is 11,769; all 4,732 it drops carry `client-go` —
//	`/go/pkg/mod/k8s.io/client-go` (3,548), `github.com/tikv/client-go/v2/tikv.`
//	(852), `pkg/mod/k8s.io/client-go` (230),
//	`k8s.io/client-go/informers/factory.go` (79), `/gomodcache/k8s.io/client-go`
//	(23) — which is 4,732 exactly.
//
//	the LITERAL has no word boundaries, so it cannot tell `grpcutil.go` from
//	`util.go`, `leaderelection.go` from `election.go`, `sysinfo.go` from
//	`info.go`, `observer.go` from `server.go`, `terror.go` from `error.go`,
//	`tlsconfig.go` from `config.go`, `contexthandler.go` from `handler.go`,
//	`runtime.goexit` from `runtime.go`, or `main.go:1542` from `main.go:154`.
//
// Their conjunction rejects both families, which makes it a better answer than
// either — and than the obvious oracle. Measured over `ts < '2026-08-20 16:25:00'`
// against `source_file LIKE 'X%' OR line LIKE '%X%'` for every one of the table's
// mined values: 963 of 971 positions and 250 of 262 basenames are equal to it, and
// every one of the 20 differences is the ORACLE being wrong — 13 of them a
// substring artifact from the list above, and the other 7 the holes this file
// already records (three values with no dot, four under the `kv.` prefix).
//
// # Why the AND is safe, verified rather than assumed
//
// ANDing a filter onto a search function loses every row the search function
// missed, so the search function has to cover the filter. On the FILE column it
// does: for all 1,233 mined values, `count(F)` equals `count(F AND Q)` — 0 terms
// where the anchored comparison matches a row the search function did not.
//
// On the TEXT column it deliberately does not, and that is the intersection above
// rather than a loss.
func (c *compiler) sourceFileFragment(t *parser.Term) (fragment, bool, error) {
	position, isPos, ok := c.sourceFileMatch(t)
	if !ok {
		return fragment{}, false, nil
	}
	role := c.schema.Fields[c.schema.SourceFile]
	def := c.schema.Fields[c.schema.Default]

	term := *t
	term.Field = ""
	term.Value = position
	c.dropFuzzOnPosition(&term)
	c.warnSourceFile(position, isPos, role)

	// Fuzziness is a request for an approximate answer, so it keeps the
	// approximate path — match() on each column, nothing literal beside it, since
	// a literal filter would delete every row the edit distance was for. It needs
	// an inverted index to be expressible at all.
	if term.Fuzz > 0 && role.Kind == Text {
		left, right := term, term
		left.Field = c.schema.Default
		right.Field = c.schema.SourceFile
		f, err := c.render(&parser.Or{Children: []parser.Node{&left, &right}})
		return f, true, err
	}

	filter := c.sourceFileFilter(def, role, position, isPos, term.Wildcard)

	// Two cases have no search function to prune with, and the filter is then the
	// whole answer: a role whose column carries no inverted index (the k8s-logs
	// preset), and a wildcard, which cannot be spelled inside query() at all.
	// Measured, both are exact and both are a scan: 0.28s for `manager.go` on
	// k8s-logs against 0.15s for the token form it replaces, which returned
	// 514,443 rows instead of 1,587.
	if role.Kind != Text || term.Wildcard {
		if term.Fuzz > 0 || term.Boost != "" {
			// Neither is expressible on a comparison: fuzziness reaches the engine
			// through match() and a boost only reorders score(), and this half of
			// the search is compared rather than scored. Said out loud, because
			// silently ignoring a modifier is how `^2` used to be a compile error
			// here and is now a no-op.
			c.warn("fuzziness and boost are dropped on %q: it is matched by literal comparison "+
				"rather than through the index — %s carries no inverted index here, or the "+
				"wildcard cannot be spelled inside a search function — so there is no analyzer "+
				"to tolerate an edit and no relevance to weight", position, c.schema.SourceFile)
		}
		return fragment{sql: filter}, true, nil
	}

	c.noteText(def)
	c.noteText(role)
	expr := def.Column + ":" + quoteQueryValue(position, false)
	if role.Column != def.Column {
		// Two surfaces, one search function: legal because the schema layer
		// refuses a text field outside the one inverted index group.
		expr = "(" + expr + ") OR (" + role.Column + ":" +
			quoteQueryValue(position, false) + ")"
	}
	// A boost wraps the whole union rather than each half; verified legal and
	// row-identical — `query('((line:X) OR (source_file:X))^2')` returns the same
	// 1,571 as the unboosted form.
	expr = c.boost(expr, &term)
	// plain is the filter on its own, for the two places a residual cannot travel:
	// under an OR, where it would filter the other disjunct, and under a NOT,
	// where it cannot be hoisted past the negation. It is EXACT — the same 1,590 /
	// 3,790 / 1,571 — and only loses the pruning, which is 0.39s against 0.24s.
	return fragment{text: expr, residual: filter, plain: filter}, true, nil
}

// sourceFileMatch decides whether a term is a source file, and returns the whole
// call site it spells.
//
// Every guard here exists to keep a real lookup real; see the rule's description
// at the top of this file.
func (c *compiler) sourceFileMatch(t *parser.Term) (position string, isPos, ok bool) {
	if c.schema.SourceFile == "" {
		// No role declared: the rule is off and every spelling compiles exactly
		// as it did before it existed. That is the whole of the promise made to a
		// deployment that has not opted in.
		return "", false, false
	}
	if _, declared := c.schema.Fields[c.schema.SourceFile]; !declared {
		// Unreachable through a loaded descriptor — Def.Schema refuses a role
		// naming an undeclared field — but Schema is a public struct anyone may
		// build by hand, and compiling against a field that does not resolve
		// would emit a comparison on an empty column expression.
		return "", false, false
	}
	if _, ok := c.schema.Fields[c.schema.Default]; !ok {
		return "", false, false
	}

	// A regex is a pattern the user wrote for a specific column, and an existence
	// test asks about a KEY rather than about a value — expanding either would
	// change the question rather than the surface.
	if t.Regex || t.Exists {
		return "", false, false
	}
	// A quoted BARE term is the escape hatch this rule needs to have: quoting is
	// how someone searches the message text and nothing else, so
	// `"compaction_runner.rs:360"` stays where it was pointed. A quoted VALUE
	// after a dotted field name — `compaction_runner.rs:"360"` — is a different
	// thing: the quotes are around the line number, and the term is still a file
	// position.
	if t.Phrase && t.Field == "" {
		return "", false, false
	}

	if t.Field == "" {
		// A bare word. Taken only when the WHOLE word is a file name or a whole
		// file position; a word that merely contains one is text.
		switch {
		case isFilePosition(t.Value):
			return t.Value, true, true
		case isSourceFileName(t.Value):
			return t.Value, false, true
		}
		return "", false, false
	}

	// `field:value`, where the field has to be a file and the value a line number.
	if !isSourceFileName(t.Field) || !isLineNumber(t.Value) {
		return "", false, false
	}
	if _, declared := c.schema.Fields[strings.ToLower(t.Field)]; declared {
		// A declared field wins, whatever it is called. A deployment is allowed a
		// column named `parse.go`, and allowed to alias one so.
		return "", false, false
	}
	if c.namesABag(t.Field) {
		// `kv.compaction_runner.rs:360` is the user addressing the bag by name,
		// and the tail looking like a file does not undo that.
		return "", false, false
	}
	c.warn("%q is a source file position, not a field lookup: %q is not a declared field and "+
		"a file name is not a bag key, so read as `field:value` it asks for a key nothing "+
		"writes — zero rows, and no error. It is searched in %q and %q instead",
		t.Field+":"+t.Value, t.Field, c.schema.Default, c.schema.SourceFile)
	return t.Field + ":" + t.Value, true, true
}

// sourceFileFilter is the literal predicate that makes the file half exact.
//
// The shapes are in the doc comment on sourceFileFragment. Everything here is
// built with the same two helpers the rest of the compiler uses, so the escaping
// of a name full of `_` — a LIKE wildcard — is the escaping that is already
// tested: `compaction_runner.rs` becomes `compaction\_runner.rs`.
func (c *compiler) sourceFileFilter(def, role Field, value string, isPos, wild bool) string {
	pat := escapeLike(value)
	if wild {
		// The user's own `*` and `?` become LIKE wildcards, and the literal runs
		// between them are escaped one run at a time; see likePattern.
		pat = likePattern(value)
	}
	// One column serving both roles is one reading: if the source-file field IS
	// the default surface, a mention and an emission are the same row, so the
	// substring test is the whole of the union.
	if role.Column == def.Column {
		return "(" + c.likePattern(def.Column, surround(pat)) + ")"
	}
	var parts []string
	if isPos && !wild {
		// A position names one line, so the value is the whole of it.
		parts = append(parts, c.equals(role.Column, value))
		parts = append(parts, c.likePattern(role.Column, "%/"+pat))
	} else {
		parts = append(parts, c.likePattern(role.Column, suffixStar(pat)))
		parts = append(parts, c.likePattern(role.Column, "%/"+suffixStar(pat)))
	}
	parts = append(parts, c.likePattern(def.Column, surround(pat)))
	return "(" + strings.Join(parts, " OR ") + ")"
}

// surround opens both ends of a pattern for the substring test on the text
// surface, without doubling a wildcard the user's own pattern already put there.
func surround(pat string) string {
	if !strings.HasPrefix(pat, "%") {
		pat = "%" + pat
	}
	return suffixStar(pat)
}

// suffixStar appends the trailing wildcard that turns a name into a prefix match,
// unless the pattern already ends in one.
func suffixStar(pat string) string {
	if strings.HasSuffix(pat, "%") {
		return pat
	}
	return pat + "%"
}

// equals renders a case-folded equality, the way stringTerm does.
func (c *compiler) equals(col, value string) string {
	lit := "'" + escapeString(value) + "'"
	if c.schema.CaseInsensitive {
		return fmt.Sprintf("lower(%s) = lower(%s)", col, lit)
	}
	return col + " = " + lit
}

// likePattern renders a LIKE against an already-built pattern, the way like() and
// contains() do — case-folded on both sides when the schema asks for it, because
// there is no case-insensitive comparison operator on this engine.
func (c *compiler) likePattern(col, pattern string) string {
	lit := "'" + escapeString(pattern) + "'"
	if c.schema.CaseInsensitive {
		return fmt.Sprintf("lower(%s) LIKE lower(%s)", col, lit)
	}
	return fmt.Sprintf("%s LIKE %s", col, lit)
}

// warnSourceFile says what was searched, and hands over the one spelling that
// narrows it.
//
// It carries no row counts on purpose. The text it replaced named the suffix
// family — `manager.go also finds tiflash_manager.go` — which was true of the
// token search and is no longer true of what this compiler emits; a warning that
// describes an older version of the code is worse than none.
func (c *compiler) warnSourceFile(position string, isPos bool, role Field) {
	whole := "the whole name"
	if isPos {
		whole = "the whole position"
	}
	c.warn("%q reads as a source file, and a source file lives in two places on this table: a "+
		"collector that parses `[file.rs:360]` out of a bracket-format line puts it in %q and "+
		"leaves it out of the message, while a logfmt line carrying `caller=` or `source=` "+
		"leaves it in the text. Both are searched and the results ORed, because searching one "+
		"alone answers zero for the components that use the other format. Quote the term to "+
		"search the message text alone",
		position, c.schema.SourceFile)
	c.warn("%q is matched on %s — the value of %q, or a path ending in it — and the text half "+
		"is matched as a literal substring. Both are comparisons rather than token searches, "+
		"and that is what makes the answer exact: the analyzer splits on `_` and `-` as well "+
		"as `.`, so a token search cannot tell `manager.go` from `tiflash_manager.go` nor "+
		"`client.go` from the module `client-go`. %s:%s typed explicitly is still a token "+
		"search and answers for all of them",
		position, whole, c.schema.SourceFile, c.schema.SourceFile, quotedIfNeeded(position))
}

// dropFuzzOnPosition removes the one modifier a file POSITION cannot carry.
//
// The reason is the colon rather than the shape. Fuzziness reaches this engine
// only as match()'s option argument, and match()'s query text is parsed as
// `field:value` — so a value containing a colon is read as a field reference and
// the statement fails. Measured over [2026-08-20 16:00:00, 16:25:00):
//
//	match(source_file, 'compaction_runner.rs:360', 'fuzziness=2')
//	                          [1903] Field does not exist: 'compaction_runner.rs'
//	match(source_file, 'compaction_runner.rs',     'fuzziness=1')             30
//	query('source_file:"compaction_runner.rs:360"')                           15
//
// So a file NAME keeps its `~N` and a file POSITION loses it, and the difference
// is exactly the colon. Nothing worth having is lost: edit distance over a whole
// call site is not a question about line numbers, and it does not behave like one
// either — `match(source_file, 'compaction_runnr.rs', 'fuzziness=2')`, one edit
// from a file with 30 rows, returns 0.
func (c *compiler) dropFuzzOnPosition(t *parser.Term) {
	if t.Fuzz <= 0 || !strings.Contains(t.Value, ":") {
		return
	}
	c.warn("fuzziness ~%d is dropped on the file position %q, because of the colon: fuzziness "+
		"reaches this engine only through match(), whose query text is parsed as `field:value`, "+
		"so the file name is read as a field name and the statement fails — measured, "+
		"`match(source_file, 'compaction_runner.rs:360', 'fuzziness=2')` is `[1903] Field does "+
		"not exist: 'compaction_runner.rs'`. Matching the position exactly instead; a file NAME "+
		"has no colon and keeps its ~N", t.Fuzz, t.Value)
	t.Fuzz = 0
}

// namesABag reports whether a dotted name addresses a bag rather than a file.
//
// Three ways to have said so: the bag's own column name, its declared prefix, or
// a key the schema declares under exactly this spelling. Bag keys are compared
// exactly and never folded, for the reason recorded at the declaration site in
// Def.Schema — kv['tableid'] and kv['tableID'] are different keys — and a
// declaration is the one piece of evidence that outranks a shape.
func (c *compiler) namesABag(name string) bool {
	for _, b := range c.schema.bags() {
		if _, ok := trimPrefixSegment(name, b.Column); ok {
			return true
		}
		if b.Prefix != "" {
			if _, ok := trimPrefixSegment(name, b.Prefix); ok {
				return true
			}
		}
		if _, ok := b.Keys[name]; ok {
			return true
		}
	}
	return false
}

// isSourceFileName reports whether s spells a source file name: one or more name
// segments and a trailing all-letters extension.
//
// `compaction_runner.rs`, `tidb_dashboard_controller.go` and the generated-Go
// `descriptor.pb.go` all qualify; `10.0.0.1`, `192.168.176.28` and `v8.5.7` do
// not, because their last segment is digits. `foo.bar` and `example.com` DO
// qualify, and that is deliberate — see isSourceExtension for why a loose rule is
// safe once the term is expanded rather than redirected. A segment is letters,
// digits, `_` or `-`, which is what a file name is and what keeps a path
// (`store/peer.rs`) and a URL out.
func isSourceFileName(s string) bool {
	i := strings.LastIndexByte(s, '.')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	if !isSourceExtension(s[i+1:]) {
		return false
	}
	for _, seg := range strings.Split(s[:i], ".") {
		if !isFileNameSegment(seg) {
			return false
		}
	}
	return true
}

func isFileNameSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		switch c := seg[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// isLineNumber reports whether v is a line number, optionally carrying Lucene's
// wildcards so that `compaction_runner.rs:36*` asks for lines 360-369 rather than
// becoming a bag lookup for a key that does not exist.
//
// At least one digit is required, so `foo.rs:*` — which the parser reads as an
// existence test anyway — cannot arrive here as a line number.
func isLineNumber(v string) bool {
	digits := false
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= '0' && c <= '9':
			digits = true
		case c == '*' || c == '?':
		default:
			return false
		}
	}
	return digits
}

// isFilePosition reports whether v is a whole file position in one token.
//
// It is reachable because the lexer only splits at a colon whose left-hand run is
// a legal field name: `compaction_runner.rs:360` splits, and `2foo.rs:360` —
// leading digit — arrives whole. Recognising both spellings costs three lines and
// means the rule is about the token's shape rather than about which of two paths
// the lexer happened to take.
func isFilePosition(v string) bool {
	i := strings.LastIndexByte(v, ':')
	if i < 0 {
		return false
	}
	return isSourceFileName(v[:i]) && isLineNumber(v[i+1:])
}

// quotedIfNeeded renders a term the way the user would have to type it to reach
// one surface only, which means quoting a position so its colon is not read as a
// field separator.
func quotedIfNeeded(v string) string {
	if strings.Contains(v, ":") {
		return `"` + v + `"`
	}
	return v
}
