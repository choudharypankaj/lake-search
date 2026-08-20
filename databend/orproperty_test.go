package databend

import (
	"regexp"
	"strings"
	"testing"
)

// The property: after a statement is compiled, no search function may sit in the
// OUTER scan beside a disjunction.
//
// This is the one assertion that catches the whole class. Four families of this
// defect shipped, each found a round after the last, because each was hunted as
// a shape rather than as a property: a bare text term under OR, a text term
// inside a conjunction under OR, a negated conjunction under OR, a fuzzy leaf
// under OR, and a degraded phrase under OR. Every one of them is the same
// sentence — a search function reachable from a disjunction — and every one of
// them fails this single check.
//
// Swept over a generated corpus rather than a hand-picked list, because the
// hand-picked lists are exactly what missed families two, three and four.

// searchMisplaced reports whether any search function in this SQL sits somewhere
// the engine's pruning makes unsound.
//
// The invariant, stated once so the check can be precise rather than a heuristic:
// within any single scan — the outer statement, or a subquery body — a search
// function prunes that scan to the blocks its index says match. That is CORRECT
// exactly when the search function's contribution is conjunctive at the top level
// of the scan, and wrong otherwise. So a search function may not sit
//
//   - under a disjunction, because the other disjunct's rows in the unvisited
//     blocks are lost, or
//   - under a negation, because the complement is taken only within the visited
//     blocks.
//
// Both directions of that were shipped defects, and both are the same sentence.
//
// It is written INDEPENDENTLY of the compiler's own outerSearchFuncs, on purpose:
// if the test shared the compiler's stripping logic, a bug there would hide
// itself from the test that exists to catch it. TestDetectorsAgree cross-checks
// them.
//
// Precision matters here, because two legitimate shapes look superficially like
// the bad ones and a coarse check flags them:
//
//	((level='E' OR level='W') AND query('line:x'))     the OR is a sibling conjunct
//	(query('line:x') AND COALESCE(NOT (level='E'), TRUE))   the NOT holds no search
//
// Both are correct — the search function is still conjunctive at the top level —
// so the check descends the parenthesis structure rather than scanning for
// co-occurrence.
func searchMisplaced(sql string) bool {
	return misplacedInScans(blankLiterals(sql))
}

// misplacedInScans applies the invariant to every scan, not only the outer one.
//
// A subquery body is its OWN scan, with its own pruning, so a search function
// misplaced inside one is just as wrong as in the outer statement. Merely
// stripping subquery bodies and checking the remainder misses that — and it
// missed it concretely: hoisting a negated conjunction into a disjunction's
// subquery carried the bare NOT inside with it, which returned 917,109 against a
// true 1,072,856 while looking clean from outside.
func misplacedInScans(s string) bool {
	for _, body := range selectSpanBodies(s) {
		if misplacedInScans(body) {
			return true
		}
	}
	outer := removeSelectSpans(s)
	return searchUnderNot(outer) || searchUnderOr(outer)
}

// selectSpanBodies returns the contents of each `(SELECT …)` span.
func selectSpanBodies(s string) []string {
	var out []string
	for {
		i := indexSelectOpen(s)
		if i < 0 {
			return out
		}
		body, ok := balancedBody(s, i)
		if !ok {
			return out
		}
		out = append(out, body)
		s = s[i+len(body)+2:]
	}
}

// searchUnderNot finds any `NOT (…)` whose balanced body holds a search call, at
// any depth: a search function under a negation is unsound wherever it sits.
func searchUnderNot(s string) bool {
	for i := 0; i+5 <= len(s); i++ {
		if !strings.HasPrefix(s[i:], "NOT (") {
			continue
		}
		if body, ok := balancedBody(s, i+4); ok && reSearchCall.MatchString(body) {
			return true
		}
	}
	return false
}

// searchUnderOr descends the parenthesis structure: at each level it splits on
// top-level ` OR `, and if that level really is a disjunction then no branch of
// it may contain a search call.
func searchUnderOr(s string) bool {
	if parts := splitTopLevelOr(s); len(parts) > 1 {
		for _, p := range parts {
			if reSearchCall.MatchString(p) {
				return true
			}
		}
	}
	for _, g := range topLevelGroups(s) {
		if searchUnderOr(g) {
			return true
		}
	}
	return false
}

// balancedBody returns the contents of the parenthesised span starting at the
// `(` at or after i.
func balancedBody(s string, i int) (string, bool) {
	for i < len(s) && s[i] != '(' {
		i++
	}
	if i >= len(s) {
		return "", false
	}
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[i+1 : j], true
			}
		}
	}
	return "", false
}

// splitTopLevelOr splits on ` OR ` outside any parentheses.
func splitTopLevelOr(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && strings.HasPrefix(s[i:], " OR ") {
			out = append(out, s[start:i])
			start = i + 4
			i += 3
		}
	}
	return append(out, s[start:])
}

// topLevelGroups returns the contents of each parenthesised span at depth 0.
func topLevelGroups(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				out = append(out, s[start:i])
			}
		}
	}
	return out
}

var reSearchCall = regexp.MustCompile(`(^|[^A-Za-z0-9_])(query|match)\(`)

// blankLiterals replaces the contents of every single-quoted literal with
// nothing, handling the doubled-quote escape this engine uses.
func blankLiterals(s string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'' && !in:
			in = true
			b.WriteByte('\'')
		case s[i] == '\'' && in:
			if i+1 < len(s) && s[i+1] == '\'' {
				i++ // an escaped quote inside the literal
				continue
			}
			in = false
			b.WriteByte('\'')
		case !in:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// removeSelectSpans deletes `( SELECT … )` spans, innermost-last, by repeatedly
// finding a `(SELECT` and scanning to its matching close paren. Written as a
// loop over indices rather than as a single pass, so it shares no code with the
// compiler's version.
func removeSelectSpans(s string) string {
	for {
		i := indexSelectOpen(s)
		if i < 0 {
			return s
		}
		depth, j := 0, i
		for ; j < len(s); j++ {
			if s[j] == '(' {
				depth++
			} else if s[j] == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if j >= len(s) {
			return s // unbalanced; leave it, which can only over-report
		}
		s = s[:i] + " " + s[j+1:]
	}
}

func indexSelectOpen(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '(' {
			continue
		}
		k := i + 1
		for k < len(s) && (s[k] == ' ' || s[k] == '\n' || s[k] == '\t') {
			k++
		}
		if k+6 <= len(s) && strings.EqualFold(s[k:k+6], "SELECT") {
			return i
		}
	}
	return -1
}

// orCorpus generates the shapes the parser accepts, two and three deep.
//
// The text leaves are deliberately RARE terms. `RemoteStopped` matches 725 rows
// live and touches 4 of the reference table's 5 blocks, so it misses one; a
// common term like `region` touches every block and makes the defect invisible.
// A corpus built on common terms would pass while the bug is fully present.
func orCorpus() []string {
	// Leaves that DO reach a search function, one per compilation path.
	text := []string{
		"RemoteStopped",                 // plain token
		"msg:RemoteStopped",             // field-scoped
		"RemoteStopped~1",               // fuzzy: born as sql, never had a text field
		"RemoteStopped^5",               // boost
		`"RemoteStopped to"`,            // phrase the analyzer collapses: the degrade path
		`"peer status"`,                 // ordinary phrase
		`"region peer"~3`,               // proximity
		"(RemoteStopped OR rejections)", // a group
		"(RemoteStopped rejections)",    // a conjunction of two text leaves
	}
	// Leaves that reach NO search function, to sit on the other side of the OR.
	plain := []string{
		"level:ERROR",
		"pod:*",
		"pod:tikv*",
		"msg:/Remote.*/",
		"err:RemoteStopped",
		"ts:>2026-08-18T00:00:00Z",
		"to", // a stopword: compiled as a scan, not a search function
	}

	var out []string
	add := func(qs ...string) { out = append(out, qs...) }

	for _, t := range text {
		for _, p := range plain {
			// Two deep, both orders, and the conjunction control.
			add(
				t+" OR "+p,
				p+" OR "+t,
				t+" "+p,
				"-"+t+" OR "+p,
				"NOT "+t+" OR "+p,
			)
			// Shapes a coarse detector would flag and which are correct: the
			// OR or the NOT is a sibling conjunct, not a parent of the search.
			add(
				"("+p+" OR level:WARN) "+t,
				t+" ("+p+" OR level:WARN)",
				t+" -"+p,
				"("+p+" OR level:WARN) "+t+" -pod:zzz",
			)
			// Three deep: the search function buried one level further down,
			// which is where families two to four lived.
			add(
				"("+t+" "+p+") OR level:WARN",
				"level:WARN OR ("+t+" "+p+")",
				"NOT ("+t+" "+p+") OR level:WARN",
				"level:WARN OR NOT ("+t+" "+p+")",
				"-("+t+" "+p+") OR level:WARN",
				"("+t+" OR "+p+") level:WARN",
				"level:WARN OR ("+t+" "+p+") OR level:INFO",
				"(("+t+" "+p+") pod:*) OR level:WARN",
			)
		}
		// Two text leaves in separate branches: each needs its own subquery.
		for _, u := range text {
			add("(" + t + " level:ERROR) OR (" + u + " level:WARN)")
		}
	}
	return out
}

// adversarialValues are search VALUES chosen to attack the structural passes
// rather than the shapes.
//
// This is where the coverage gap actually was. A SQL-derived count is
// leaf-agnostic, so adding more shapes finds nothing — twenty extra shapes up to
// four levels deep all came back clean. Every generated leaf carried a benign
// fixed value, and a value is the one part of the SQL that comes from a person:
// it lands inside a string literal, and a literal can contain parentheses, the
// word SELECT, and quote escapes. The compiler analysed structure WITHOUT
// masking literals, so `level:"(SELECT ("` made the subquery strip run past the
// closing quote and swallow the real query() after it — 27,502 rows against a
// true 60,727, driven by nothing but what somebody typed in the search box.
func adversarialValues() []string {
	return []string{
		"harmless", // the control: this must behave exactly like any other value

		// Parenthesis structure inside a value.
		"(SELECT (",              // the shipped defect
		"( select (",             // lower case: not a keyword-case guard
		"(SELECT",                // no closing paren at all
		"(", ")", "((()", "()))", // lone and unbalanced
		"(SELECT _row_id FROM t WHERE query(x))", // a whole fake subquery

		// The tokens the counter and the detector look for.
		"query(", "match(", " OR ", "NOT (", "AND",

		// Quote escaping: escapeString doubles a quote and escapes a backslash,
		// and either one misread as the literal's end puts the scanner back into
		// structure mode inside a value.
		"a'b", "a''b", `a\b`, `it's (SELECT (`,
	}
}

// valueCorpus puts each adversarial value into every position a value can
// occupy — a plain-column filter, a bag key, a phrase, a field-scoped term —
// inside the shapes where a mis-parse costs rows.
func valueCorpus() []string {
	var out []string
	for _, v := range adversarialValues() {
		q := `"` + v + `"`
		out = append(out,
			// The reported shape, and the same with each other leaf kind that
			// reaches a search function by a different path.
			"level:WARN OR (level:"+q+" RemoteStopped)",
			"level:WARN OR (level:"+q+" RemoteStopped~1)",
			"level:WARN OR (level:"+q+" "+`"RemoteStopped to"`+")",
			"NOT (level:"+q+" RemoteStopped) OR level:WARN",
			"-(level:"+q+" RemoteStopped) OR level:WARN",
			// The value in the bag, whose subscript is also a literal.
			"level:WARN OR (err:"+q+" RemoteStopped)",
			// The value in the searched text itself, so it lands inside query().
			"msg:"+q+" OR level:ERROR",
			"level:WARN OR (msg:"+q+" RemoteStopped)",
			// And three deep.
			"level:INFO OR ((level:"+q+" RemoteStopped) pod:*)",
		)
	}
	return out
}

// TestNoBareSearchFunctionBesideOr is the property, swept.
func TestNoBareSearchFunctionBesideOr(t *testing.T) {
	for _, name := range PresetNames() {
		s, _, err := Preset(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			corpus := append(orCorpus(), valueCorpus()...)
			compiled, refused := 0, 0
			for _, q := range corpus {
				r, err := CompileString(q, s)
				if err != nil {
					// A refusal is a correct outcome — it is the other half of
					// the fix. It just cannot be a wrong answer.
					refused++
					continue
				}
				compiled++
				if searchMisplaced(r.SQL) {
					t.Errorf("a search function sits under a disjunction or a negation\n  query: %s\n  sql:   %s",
						q, r.SQL)
				}
			}
			// Coverage guard: if a change made most of the corpus refuse, the
			// sweep would pass by asserting nothing.
			if compiled < len(corpus)/2 {
				t.Errorf("only %d of %d shapes compiled; the sweep is not proving much",
					compiled, len(corpus))
			}
			t.Logf("%d shapes: %d compiled, %d refused", len(corpus), compiled, refused)
		})
	}
}

// The detector and the compiler must agree about where the search functions are.
// They are written independently so that a bug in one is visible; this is what
// makes that visibility real.
func TestDetectorsAgree(t *testing.T) {
	s, _, err := Preset("k8s-logs-line")
	if err != nil {
		t.Fatal(err)
	}
	// The VALUE corpus is what turns this cross-check into a real guard. It only
	// ever ran over shapes whose leaves carried fixed benign values, so the two
	// implementations agreed for the wrong reason: neither was ever asked about
	// a literal that contains structure. Against `level:"(SELECT ("` the
	// compiler said there was no search function and the detector said there
	// was — a disagreement this test would have reported the moment a hostile
	// value was in the corpus.
	//
	// The two implementations stay deliberately separate. The compiler masks
	// literal contents in place, preserving length; the detector deletes them.
	// They agree on RESULTS, never on code, which is the only reason a bug in
	// one of them is visible here at all.
	checked := 0
	for _, q := range append(orCorpus(), valueCorpus()...) {
		r, err := CompileString(q, s)
		if err != nil {
			continue
		}
		checked++
		compilerSees := outerSearchFuncs(r.SQL) > 0
		detectorSees := misplacedSearchPresent(r.SQL)
		if compilerSees != detectorSees {
			t.Errorf("the compiler and the detector disagree about %q\n  compiler=%v detector=%v\n  sql: %s",
				q, compilerSees, detectorSees, r.SQL)
		}
	}
	if checked < 1000 {
		t.Errorf("only %d shapes cross-checked; the sweep is not proving much", checked)
	}
	t.Logf("%d shapes cross-checked", checked)
}

// misplacedSearchPresent is the detector's own answer to "is there a search
// function anywhere in this statement", by its own reckoning of literals and
// subqueries, for comparison against the compiler's count.
func misplacedSearchPresent(sql string) bool {
	s := blankLiterals(sql)
	if reSearchCall.MatchString(removeSelectSpans(s)) {
		return true
	}
	return false
}

// The detector has to be able to fail, or the sweep above proves nothing. These
// are the four shipped defects as literal SQL.
func TestDetectorCatchesTheShippedDefects(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		{"family 1: bare text under OR",
			`(query('line:snapshot') OR lower(level) = lower('ERROR'))`, true},
		{"family 2: text inside a conjunction under OR",
			`(lower(level) = lower('WARN') OR (lower(level) = lower('ERROR') AND query('line:x')))`, true},
		{"family 3: negated conjunction under OR",
			`(COALESCE(NOT ((lower(level) = lower('E') AND query('line:x'))), TRUE) OR lower(level) = lower('W'))`, true},
		{"family 4: fuzzy leaf under OR",
			`(lower(level) = lower('W') OR (lower(level) = lower('E') AND match(line, 'x', 'fuzziness=1')))`, true},
		{"family 5: degraded phrase under OR",
			`((query('(line:a) AND (line:b)') AND lower(line) LIKE lower('%a b%')) OR lower(level) = lower('W'))`, true},

		// And the shapes that must NOT trip it.
		{"hoisted into a subquery",
			`(_row_id IN (SELECT _row_id FROM t WHERE query('line:x')) OR lower(level) = lower('E'))`, false},
		{"an OR inside the query language, not in SQL",
			`query('(line:a) OR (line:b)')`, false},
		{"a conjunction, where pruning is the right answer",
			`(query('line:x') AND lower(level) = lower('E'))`, false},
		{"an anti-join",
			`_row_id NOT IN (SELECT _row_id FROM t WHERE query('line:x'))`, false},
		{"two hoisted branches",
			`(_row_id IN (SELECT _row_id FROM t WHERE (query('line:a') AND lower(level) = lower('e'))) ` +
				`OR _row_id IN (SELECT _row_id FROM t WHERE (query('line:b') AND lower(level) = lower('w'))))`, false},
		{"a literal containing the word OR and the word query",
			`lower(msg) = lower('query( OR match(')`, false},

		// The negation half of the same invariant.
		{"bare NOT around a search function",
			`COALESCE(NOT ((lower(level) = lower('E') AND query('line:x'))), TRUE)`, true},
		{"bare NOT around a search function, hoisted but still bare inside",
			`(_row_id IN (SELECT _row_id FROM t WHERE COALESCE(NOT ((lower(level) = lower('E') ` +
				`AND query('line:x'))), TRUE)) OR lower(level) = lower('W'))`, true},
		{"the anti-join form of the same negation",
			`(_row_id NOT IN (SELECT _row_id FROM t WHERE (lower(level) = lower('E') ` +
				`AND query('line:x'))) OR lower(level) = lower('W'))`, false},

		// Two shapes a coarse co-occurrence check flags and which are correct:
		// the search function is still conjunctive at the top level.
		{"an OR in a sibling conjunct",
			`((lower(level) = lower('E') OR lower(level) = lower('W')) AND query('line:x'))`, false},
		{"a NOT holding no search function",
			`(query('line:x') AND COALESCE(NOT (lower(level) = lower('E')), TRUE))`, false},
		{"both of those at once",
			`((lower(level) = lower('E') OR lower(level) = lower('W')) AND query('line:x') ` +
				`AND COALESCE(NOT (lower(pod) = lower('p')), TRUE))`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := searchMisplaced(tc.sql); got != tc.want {
				t.Errorf("searchMisplaced = %v, want %v\n  %s", got, tc.want, tc.sql)
			}
		})
	}
}

// The masker is the foundation of the whole derivation, so it is tested on its
// own rather than only through the sweep.
//
// It preserves length and the quotes, replacing only the contents, so that every
// later pass sees the same offsets. The escapes matter because escapeString
// doubles a quote to `”` and a backslash to `\\`: either one misread as the end
// of the literal puts the scanner back into structure mode inside a value, which
// is exactly the defect.
func TestMaskLiterals(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no literal", `a = b`, `a = b`},
		{"plain", `a = 'xy'`, `a = 'xx'`},
		{"parens inside a literal are not structure",
			`lower(level) = lower('(SELECT (') AND query('line:x')`,
			`lower(level) = lower('xxxxxxxxx') AND query('xxxxxx')`},
		{"a doubled quote does not end the literal",
			`x = 'a''b' AND query('line:y')`,
			`x = 'xxxx' AND query('xxxxxx')`},
		{"a backslash escape does not end the literal",
			`x = 'a\'b' AND query('line:y')`,
			`x = 'xxxx' AND query('xxxxxx')`},
		{"two literals",
			`'ab' = 'cd'`, `'xx' = 'xx'`},
		// An unterminated literal is left ALONE rather than masked to the end:
		// erasing the tail would delete real query() calls and under-count,
		// which is the direction that silently loses rows.
		// An unbalanced literal is left ALONE rather than masked, so the count
		// comes out too high rather than too low. Masking part-way through
		// paired the quotes wrongly and erased the real query() — an
		// under-count, which is the direction that loses rows.
		{"unbalanced quotes leave structure intact",
			`x = 'ab AND query('line:y')`,
			`x = 'ab AND query('line:y')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := maskLiterals(tc.in)
			if got != tc.want {
				t.Errorf("maskLiterals(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
			if len(got) != len(tc.in) {
				t.Errorf("length changed: %d -> %d", len(tc.in), len(got))
			}
		})
	}
}

// The counter must see through a hostile value, which is the property the whole
// class rests on now.
func TestOuterSearchFuncsSeesThroughValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want int
	}{
		{"the shipped defect: a literal containing (SELECT and an open paren",
			`(lower(level) = lower('WARN') OR (lower(level) = lower('(SELECT (') AND query('line:x')))`, 1},
		{"lower case, so it is not a keyword-case guard",
			`(lower(level) = lower('( select (') AND query('line:x'))`, 1},
		{"a value that looks like a whole subquery",
			`(lower(level) = lower('(SELECT _row_id FROM t WHERE query(x))') AND query('line:x'))`, 1},
		{"a value containing the counted token itself",
			`lower(level) = lower('query(')`, 0},
		{"a real subquery still does not count",
			`_row_id IN (SELECT _row_id FROM t WHERE query('line:x'))`, 0},
		{"a real subquery beside a bare one counts only the bare one",
			`(_row_id IN (SELECT _row_id FROM t WHERE query('line:a')) AND query('line:b'))`, 1},
		{"two bare ones",
			`(query('line:a') AND match(line, 'b', 'fuzziness=1'))`, 2},
		{"a column name ending in match is not a search call",
			`lower(rematch(x)) = lower('y')`, 0},
		// The unbalanced case must over-count, never under-count.
		{"unbalanced quotes still see the search call",
			`x = 'ab AND query('line:y')`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outerSearchFuncs(tc.sql); got != tc.want {
				t.Errorf("outerSearchFuncs = %d, want %d\n  %s", got, tc.want, tc.sql)
			}
		})
	}
}
