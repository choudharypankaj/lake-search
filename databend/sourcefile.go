package databend

import (
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
// The disjunction cannot lose: it is a strict superset of what the default surface
// answered before, which is asserted structurally by
// TestSourceFileExpansionCannotLoseRows rather than by counting.
//
// Both halves go inside ONE query() call, which the engine allows because
// `source_file` sits in the same inverted index group as msg, line and kv.
// Measured, `query('(line:"compaction_runner.rs:360") OR (source_file:
// "compaction_runner.rs:360")')` is 15 and still composes with a term and a
// column filter.
//
// # What the token search costs, with the arithmetic
//
// `source_file:` is a TOKEN search and this analyzer splits on `_` as well as on
// `.`, so a name that ENDS another name matches both. Over `ts < '2026-08-20
// 16:25:00'` (1,292,338 rows):
//
//	query('source_file:manager.go')        513,055
//	source_file LIKE 'manager.go:%'            199   the file itself
//	source_file LIKE '%_manager.go:%'      512,856   files ending with the name
//
// and 199 + 512,856 = 513,055 exactly, so the whole of the excess is the suffix
// family. Positions inherit it in miniature: `server.go:342` is 1,720 rows where
// 9 carry that exact position, the rest being `*_server.go` files with a line 342.
//
// This is not new and it is not the expansion's doing — `source_file:manager.go`,
// the explicitly-scoped spelling, has answered exactly this since the column
// joined the index group, and the gate pins it unchanged. What the expansion does
// is make the bare spelling agree with the scoped one.
//
// The exact alternative was considered and refused: `lower(source_file) LIKE
// lower('manager.go:%')` returns the 199, but it is anchored at character 0 of the
// column, which assumes the value begins with a basename. The role promises only
// that the field holds a call site — a deployment that stores
// `components/raftstore/src/peer.rs:100` would get 0 rows from the anchored form,
// which is the silent zero this whole rule exists to remove. It is also a full
// scan on a 1.29M-row column, and it cannot share the statement's search
// function. The token search is wider; the anchored one is wrong on a shape the
// role allows.
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

// sourceFileNode expands a term that reads as a source file into a disjunction
// over the default surface and the source-file role, or returns nil when the term
// is not one.
//
// It builds NEW nodes rather than editing the ones it was given. Compile is
// public and a caller may compile the same parsed query twice — CompileScore and
// CompileScoreExpr each do — and a mutated tree would make the second compile see
// a term nobody typed.
//
// The children name their fields explicitly, which is also what stops this from
// recursing: a field the schema declares can never fire the rule, and both
// children name declared fields.
func (c *compiler) sourceFileNode(t *parser.Term) parser.Node {
	if c.schema.SourceFile == "" {
		// No role declared: the rule is off and every spelling compiles exactly
		// as it did before it existed. That is the whole of the promise made to a
		// deployment that has not opted in.
		return nil
	}
	role, ok := c.schema.Fields[c.schema.SourceFile]
	if !ok {
		// Unreachable through a loaded descriptor — Def.Schema refuses a role
		// naming an undeclared field — but Schema is a public struct anyone may
		// build by hand, and expanding onto a field that does not resolve would
		// emit a comparison against an empty column expression.
		return nil
	}

	// A regex is a pattern the user wrote for a specific column, and an existence
	// test asks about a KEY rather than about a value — expanding either would
	// change the question rather than the surface.
	if t.Regex || t.Exists {
		return nil
	}
	// A quoted BARE term is the escape hatch this rule needs to have: quoting is
	// how someone searches the message text and nothing else, so
	// `"compaction_runner.rs:360"` stays where it was pointed. A quoted VALUE
	// after a dotted field name — `compaction_runner.rs:"360"` — is a different
	// thing: the quotes are around the line number, and the term is still a file
	// position.
	if t.Phrase && t.Field == "" {
		return nil
	}

	position := t.Value
	if t.Field != "" {
		// `field:value`, where the field has to be a file and the value a line
		// number. Every guard here exists to keep a real lookup real.
		if !isSourceFileName(t.Field) || !isLineNumber(t.Value) {
			return nil
		}
		if _, declared := c.schema.Fields[strings.ToLower(t.Field)]; declared {
			// A declared field wins, whatever it is called. A deployment is
			// allowed a column named `parse.go`, and allowed to alias one so.
			return nil
		}
		if c.namesABag(t.Field) {
			// `kv.compaction_runner.rs:360` is the user addressing the bag by
			// name, and the tail looking like a file does not undo that.
			return nil
		}
		position = t.Field + ":" + t.Value
		c.warn("%q is a source file position, not a field lookup: %q is not a declared field and "+
			"a file name is not a bag key, so read as `field:value` it asks for a key nothing "+
			"writes — zero rows, and no error. It is searched in %q and %q instead",
			position, t.Field, c.schema.Default, c.schema.SourceFile)
	} else {
		// A bare word. Expanded only when the WHOLE word is a file name or a whole
		// file position; a word that merely contains one is text, and a phrase is
		// text by the guard above.
		if !isSourceFileName(t.Value) && !isFilePosition(t.Value) {
			return nil
		}
	}

	// One warning, whichever spelling arrived, and it has to carry two things: the
	// mechanism, and the choice this compiler made on the reader's behalf.
	//
	// The mechanism is that a call site lives in different places on the same
	// table depending on the log format, so one surface is never the whole answer.
	// The choice is that "lines that mention client.go" and "lines emitted by
	// client.go" are different questions and neither contains the other — on this
	// table a basename can be both — so the union answers both and the spellings
	// that separate them are handed over. Silently picking one is exactly what
	// this rule exists to stop, and picking one loudly is not much better.
	c.warn("%q reads as a source file, and a source file lives in two places on this table: a "+
		"collector that parses `[file.rs:360]` out of a bracket-format line puts it in %q and "+
		"leaves it out of the message, while a logfmt line carrying `caller=` or `source=` leaves "+
		"it in the text. Both are searched and the results ORed, because searching one alone "+
		"answers zero for the components that use the other format. That is two questions, so if "+
		"you meant one of them: %s:%s finds the lines that MENTION it, %s:%s the lines EMITTED by "+
		"it, and neither contains the other. Note that %q is matched by name rather than pinned: "+
		"this analyzer splits on `_` as well as `.`, so a file whose name ENDS with the one you "+
		"typed matches too — manager.go also finds tiflash_manager.go — and adding the line number "+
		"narrows that without closing it",
		position, c.schema.SourceFile,
		c.schema.Default, quotedIfNeeded(position),
		c.schema.SourceFile, quotedIfNeeded(position),
		c.schema.SourceFile)

	shared := *t
	shared.Field = ""
	shared.Value = position
	c.dropFuzzOnPosition(&shared)

	if strings.EqualFold(c.schema.Default, c.schema.SourceFile) {
		// A schema whose default field IS the source-file field wants one term,
		// not the same term twice.
		return c.roleSide(shared, role)
	}
	text := shared
	text.Field = c.schema.Default
	return &parser.Or{Children: []parser.Node{&text, c.roleSide(shared, role)}}
}

// dropFuzzOnPosition removes the one modifier a file POSITION cannot carry.
//
// The reason is the colon rather than the shape. Fuzziness reaches this engine
// only as match()'s option argument, and match()'s query text is parsed as
// `field:value` — so a value containing a colon is read as a field reference and
// the statement fails. Same window:
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

// roleSide renders the source-file half of the disjunction, adapted to the KIND
// of field the role names.
//
// A text field is in the inverted index group, so it joins the same query() call
// as the default surface and the pair costs one search function.
//
// A plain column has no tokens to search, and an equality is the wrong reading of
// both spellings: the stored value is the whole call site, so
// `source_file = 'compaction_runner.rs'` is zero rows for every one of that
// file's lines. The substring is the reading that holds either way — with the
// line number and without it — so the term becomes a wildcard one and is left to
// the ordinary LIKE path, which already says that no NGRAM index serves it.
// Measured on the k8s-logs preset, whose source_file is a plain VARCHAR:
// `lower(source_file) LIKE lower('%compaction\_runner.rs:360%')` is 15 and
// `…rs%'` is 30, the same as the indexed forms. It is a full scan of a 1.29M-row
// column, and the LIKE path says so.
func (c *compiler) roleSide(t parser.Term, role Field) parser.Node {
	t.Field = c.schema.SourceFile
	if role.Kind == Text {
		return &t
	}
	t.Value = "*" + t.Value + "*"
	t.Wildcard = true
	c.warn("%q is a plain column rather than an indexed text field, so it cannot be searched by "+
		"token and this half of the search is compiled as the substring %q: exact about the "+
		"characters, but it also matches a longer file name ending with them, and it is a scan. "+
		"The stars are this compiler's, not yours — which is why the wildcard advisory below "+
		"quotes them", c.schema.SourceFile, t.Value)
	return &t
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
