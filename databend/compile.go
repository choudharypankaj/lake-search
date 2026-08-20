package databend

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/choudharypankaj/lake-search/parser"
)

// MatchAll and MatchNone are the predicates emitted for an empty search.
//
// MatchAll exists because an *empty search argument* matches nothing, raises no
// error, and prunes the index scan before any surrounding boolean is evaluated:
// `match(msg,”)` is 0 rows, and so is `(1=1 OR match(msg,”))` — a literal-true
// disjunct cannot rescue it. The only safe handling of an empty search box is
// SQL containing no match() at all.
//
// The rule is about the empty argument, not about search functions under a
// boolean. A *non-empty* one composes with ordinary SQL exactly: measured,
// query('msg:peer') is 109,950, lower(level)=lower('warn') is 9,287, their
// conjunction 341, and their disjunction 118,896 = 109,950 + 9,287 - 341. The
// mixed branch of boolean() depends on that and is sound.
const (
	MatchAll  = "1=1"
	MatchNone = "1=0"
)

// Result is a compiled predicate plus anything the caller should be told about
// how it will execute.
type Result struct {
	// SQL is a boolean expression safe to splice into a WHERE clause.
	SQL string

	// Warnings describe predicates that are correct but slow, or syntax that
	// was rewritten. They are advisory: the SQL is usable regardless.
	Warnings []string

	// UsesMatch reports whether the predicate contains a search function.
	// score() is only legal alongside one — see CompileScore.
	UsesMatch bool
}

// Compile renders a parsed query as a Databend predicate.
//
// # The one-search-function rule
//
// Measured on a live warehouse (Databend v0.34.0): a statement may contain **at
// most one** search function per table. Two of them — in any combination —
// fail with `[1065] … duplicate search function for table 0`:
//
//	match(msg,'a') AND match(msg,'b')     -- rejected
//	match(msg,'a') AND query('msg:b')     -- rejected
//	query('msg:a') AND query('msg:b')     -- rejected
//
// So boolean full-text logic cannot be composed with SQL AND/OR. It has to be
// pushed *inside* a single query() call, whose own mini-language supports it:
//
//	query('msg:peer AND msg:status')      -- 72,603 rows, agrees with LIKE
//	query('msg:peer NOT msg:status')      -- 18,839 = 91,442 - 72,603 exactly
//
// This compiler therefore collects every full-text leaf into one query() string
// and keeps structured predicates (columns, ranges, LIKE) as ordinary SQL,
// which are not search functions and combine freely. When a query genuinely
// needs two search functions it is rejected at compile time with an
// explanation, because the alternative is SQL that returns [1065] at runtime.
func Compile(n parser.Node, s Schema) (Result, error) {
	if s.Default == "" {
		return Result{}, fmt.Errorf("schema has no default field")
	}
	if n == nil {
		return Result{SQL: MatchAll}, nil
	}

	c := &compiler{schema: s}
	f, err := c.render(n)
	if err != nil {
		return Result{}, err
	}
	sql, err := c.finalize(f)
	if err != nil {
		return Result{}, err
	}
	// Counted off the finished statement rather than tallied on the way down.
	// The tally had two increment sites and any new leaf emitting a search
	// function was a third one waiting to be forgotten; the string is the
	// truth, and a search function that ended up inside a subquery correctly
	// does not count against the outer scan.
	nSearch := outerSearchFuncs(sql)
	if nSearch > 1 {
		return Result{}, fmt.Errorf(
			"this query needs %d BARE search functions but Databend allows one per table "+
				"(a search function inside a row-key subquery does not count, which is how the "+
				"disjunction shapes compile): "+
				"put the full-text terms in a single group so they compile to one query() call "+
				"(for example `(a OR b) level:error` rather than `(a level:error) OR (b level:warn)`)",
			nSearch)
	}
	return Result{SQL: sql, Warnings: c.warnings, UsesMatch: nSearch > 0}, nil
}

// CompileString parses and compiles in one step.
func CompileString(q string, s Schema) (Result, error) {
	return Compile(parser.Parse(q), s)
}

// CompileScore renders the *predicate* for a panel that also selects score().
//
// It is the same predicate CompileString produces. That is the whole fix: this
// function used to overwrite it with a sentinel token chosen to match nothing,
// so that `component:tikv` — a perfectly good filter with no full-text term in
// it — reached the warehouse as `match(msg, 'zzqq…')` and the panel came back
// empty with the user's filter discarded. Every structured-only search hit it:
// `snapsh*`, `level:ERROR`, `pod:*`.
//
// # Why the fix cannot live in the predicate
//
// Databend rejects score() unless a search function is present in the same
// statement: `[1065] [SQL-BINDER] Score function must be used together with
// match or query function`. Re-measured live, `SELECT score() … WHERE
// lower(component) = lower('tikv')` still returns exactly that.
//
// No predicate rescues it. A search function that matches nothing satisfies
// the binder and then prunes the scan, in both directions — measured against a
// truth of 1,019 rows:
//
//	lower(msg) LIKE lower('snapsh%') OR  match(msg,'<sentinel>')      0
//	lower(msg) LIKE lower('snapsh%') AND NOT match(msg,'<sentinel>')  0
//	lower(msg) LIKE lower('snapsh%')                               1,019
//
// So the select list is the only place left, which is what CompileScoreExpr is
// for. The two must be used together: this one for the WHERE clause, that one
// for the score column.
func CompileScore(q string, s Schema) (Result, error) {
	r, err := CompileString(q, s)
	if err != nil {
		return r, err
	}
	if !r.UsesMatch {
		r.Warnings = append(r.Warnings,
			"this search compiles to structured SQL with no full-text term in it, so there is "+
				"nothing for score() to rank: select $__search_score_expr(...) rather than score() "+
				"and the column will be a constant 0 instead of [1065]")
	}
	return r, nil
}

// scoreUnavailableReason says why relevance is a constant, and how to get it
// back.
//
// score() is legal only beside a BARE search function in the same statement, so
// there are three ways to lose it, and they want different advice:
//
//	the search has no full-text term at all — a field filter, a wildcard on a
//	plain column, a range, an existence test — so there is nothing to rank;
//
//	the search is full-text but the compiler moved the search function into a
//	row-key subquery, which is what makes a disjunction correct on this engine.
//	That is new, and it is a real trade: `snapshot OR level:ERROR` ranks nothing
//	where `snapshot level:ERROR` ranks normally;
//
//	the only full-text term was excluded (`-term`), which is an anti-join, again
//	a subquery.
func scoreUnavailableReason(q, sql string) string {
	const tail = " The panel still returns the right rows and falls back to newest-first; only the " +
		"ordering is unavailable."
	switch {
	case strings.Contains(sql, "NOT IN (SELECT"):
		return "relevance is unavailable: the full-text part of this search is an exclusion, which " +
			"compiles to an anti-join, so its search function sits in a subquery where score() " +
			"cannot see it. Add a positive term beside the exclusion — `a -b` ranks, `-b` alone " +
			"cannot." + tail
	case strings.Contains(sql, "IN (SELECT"):
		return "relevance is unavailable: this search ORs a full-text term with a non-full-text " +
			"condition, so the search function had to move into a subquery — on this engine a " +
			"search function left in a disjunction prunes the scan and silently drops the other " +
			"branch. score() cannot reach into a subquery. Use AND instead of OR (`a level:error` " +
			"ranks, `a OR level:error` cannot), or put the full-text terms in one group: " +
			"`(a OR b) level:error`." + tail
	default:
		return "relevance is unavailable: this search has no full-text term to rank — a field " +
			"filter, a range, an existence test and a wildcard on a plain column are all compared " +
			"rather than scored, so there is no relevance to compute. Add a bare word or a quoted " +
			"phrase to get a ranking." + tail
	}
}

// CompileScoreExpr renders the *select-list* expression that goes beside the
// predicate from CompileScore: `score()` when the compiled predicate contains a
// search function, and the constant `0` when it does not.
//
// Both were measured live rather than reasoned about: `SELECT score() … WHERE
// query('msg:snapshot')` ranks 17,595 rows top score 4.76, and `SELECT 0 …
// WHERE lower(msg) LIKE lower('%snapsh%')` returns all 17,616 with a constant
// column. The alternative — leaving score() in the select list unconditionally
// — is the [1065] above, which is at least loud, but it takes out the panel for
// every structured-only search rather than merely leaving it unranked.
//
// A search function inside an anti-join subquery does not count, and that is
// not a guess: `SELECT score() … WHERE msg NOT IN (SELECT msg … WHERE query(…))`
// fails [1065] too, because the binder does not see through the subquery. So a
// purely negative search ranks 0, which is the honest answer for a query with
// no positive clause.
func CompileScoreExpr(q string, s Schema) (Result, error) {
	r, err := CompileString(q, s)
	if err != nil {
		return r, err
	}
	expr := "0"
	if !r.UsesMatch {
		// A constant 0 is the only legal select-list expression here, because
		// score() without a search function in the same statement is [1065] —
		// re-measured, `SELECT score() … WHERE lower(component) = lower('tikv')`
		// still fails. The panel therefore renders and falls back to
		// newest-first, which is the right degradation; what was missing is any
		// statement of WHY, so a reader sees an all-zero relevance column and no
		// reason for it.
		//
		// Naming the causes matters because the list grew. It used to be "your
		// search has no full-text term"; it now also includes a search that HAS
		// one which the compiler had to move into a subquery, which is the price
		// of the disjunction fix.
		r.Warnings = append(r.Warnings, scoreUnavailableReason(q, r.SQL))
	}
	if r.UsesMatch {
		expr = "score()"
		if strings.Contains(r.SQL, "fuzziness=") {
			// Measured: the same 4,080 rows come back either way, but with
			// fuzziness set the scores collapse from 4 distinct values
			// spanning 1.49-5.66 to a single 1.0. The row set is right and
			// only the ordering is meaningless, which is exactly the kind of
			// panel that keeps looking like it works. No SQL gives both, so
			// the honest move is to say so rather than silently drop either
			// the typo tolerance or the ranking.
			r.Warnings = append(r.Warnings,
				"score() is a constant 1.0 for a fuzzy term: fuzziness reaches the engine through "+
					"match()'s option argument, and the engine does not rank an expanded match. "+
					"The rows are right; the order is not. Drop the ~N to get a real ranking")
		}
	}
	r.SQL = expr
	return r, nil
}

// WarningComment renders a Result's warnings as one SQL comment, safe to splice
// beside the predicate.
//
// It exists because of the gap M11 names: the compiler is careful to produce
// exactly the right advisory — "field \"to\" is not a column", "wildcard served
// as a substring match" — and a Grafana macro returns a string, so every one of
// them dies at that boundary and the reader is left with an unexplained panel.
// A comment is not as good as a frame notice, but it is a channel this library
// can fill on its own: it reaches the query inspector's generated SQL, and it is
// inert. Measured, `… AND ((lower(msg) LIKE lower('%snapsh%')) /* … */)` returns
// the same 17,616 rows as the predicate alone.
//
// Empty when there is nothing to say, so the caller can concatenate blindly.
func WarningComment(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	return "/* lake-search: " + commentSafe(strings.Join(warnings, " | ")) + " */"
}

// commentSafe neutralises every sequence that can take text out of a `/* … */`
// comment and back into the statement.
//
// Warning text is not the library's own prose: it quotes the user's search
// value verbatim, so anything typable in the search box can reach it. Three
// sequences matter, and all three are typable:
//
//	*/  ends the comment early, so the rest of the advisory becomes SQL.
//	    Reachable as msg:"*/" — and also as msg:*/ once the wildcard warning
//	    quotes the pattern back.
//	/*  opens a nested comment on any engine that nests block comments
//	    (PostgreSQL does; the SQL standard leaves it open), which makes the
//	    first */ close the inner one and leaves the outer comment unclosed —
//	    swallowing the rest of the statement. Reachable as msg:"/*".
//	CR/LF ends a line, which matters if the comment is ever rendered into a
//	    `--` context by a caller; a newline inside `/* */` is legal but the
//	    one-line rendering is what makes the comment safe to append anywhere.
//
// Each is broken with a space rather than dropped, so the advisory still reads
// as what the user typed.
func commentSafe(s string) string {
	s = strings.ReplaceAll(s, "*/", "* /")
	s = strings.ReplaceAll(s, "/*", "/ *")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// variantKeyWarning is the advisory both VARIANT sites raise: `term` for an
// equality and `rangeField` for a comparison. One string, because the two used
// to drift apart and the trap is identical either way.
//
// It carries no row counts, and that is the fix rather than an omission. The
// text it replaced said "kv['tableid'] is 0 rows where kv['tableID'] is 1,365".
// Both halves were wrong by the time anyone read them: the table only grows, so
// the 1,365 was true at one bound on one afternoon and measured 4,403 two hours
// later; and the sentence compares two *key* lookups while the number was
// actually a key-plus-value predicate (kv['tableID'] = '574'), so the reading a
// user would check — how many rows carry the key at all — was off by a further
// order of magnitude. A warning that ships a decaying constant is wrong by
// construction. The asymmetry itself needs no number to be true, and it is
// pinned by two conformance cases instead: `tableid:*` must be zero while
// `tableID:*` must not.
//
// The key names are examples of the shape, not counts: Go's structured loggers
// emit camelCase keys, and this table carries tableID, reconcileID,
// controllerKind and TiFlash among others.
const variantKeyWarning = "field %q is not a column, so it is read from the %s VARIANT: no index, " +
	"a full scan, and the key is matched EXACTLY, case included. A mis-cased key is silently " +
	"empty rather than an error — kv['tableid'] and kv['tableID'] are different keys — and Go " +
	"loggers emit camelCase names like tableID, reconcileID, controllerKind and TiFlash"

// indexedVariantKeyWarning is the same advisory for a bag whose column is in the
// index group. Half of it stops being true there — the lookup is index-backed
// rather than a scan — and the half that matters more does not: the key is still
// matched exactly and a mis-cased key is still silently empty, because the JSON
// path is not analysed even though the value is.
const indexedVariantKeyWarning = "field %q is not a column, so it is read from the %s VARIANT. The " +
	"lookup is index-backed, but the KEY is still matched EXACTLY, case included: a mis-cased " +
	"key is silently empty rather than an error — kv['tableid'] and kv['tableID'] are different " +
	"keys — and Go loggers emit camelCase names like tableID, reconcileID, controllerKind and " +
	"TiFlash"

// variantWarning picks between them.
func variantWarning(f Field) string {
	if f.Search != "" {
		return indexedVariantKeyWarning
	}
	return variantKeyWarning
}

// fragment is a partially compiled subtree, in one of two languages.
//
// Keeping them apart is what makes the one-search-function rule enforceable:
// text fragments merge into a single query() call, SQL fragments compose
// freely, and the boundary between them is the only place a search function is
// ever spent.
type fragment struct {
	// text holds a query()-language expression, e.g. `msg:peer AND msg:status`.
	text string

	// sql holds ordinary SQL, e.g. `lower(level) = lower('error')`.
	sql string

	// negated applies to text fragments only. Negation is carried rather than
	// applied so that `a -b` stays inside one query() call instead of becoming
	// `query(a) AND NOT query(b)`, which would be two search functions.
	negated bool

	// residual is SQL that must be ANDed with the query() call this text
	// fragment ends up inside. One construct sets it: a quoted phrase the
	// analyzer collapses to a single token, which needs the token (so the
	// index and its stemming still do the work) *and* a literal scan for the
	// text between the tokens (so the adjacency the quotes asked for is
	// actually checked).
	//
	// It rides on the fragment rather than being spent at the leaf because
	// spending it there would cost the statement's one search function and
	// turn `"not ready" peer` into a compile error. Carried, it merges into
	// the same query() call as every other text leaf.
	residual string

	// plain is the same leaf rendered as pure SQL, for the two places a
	// residual cannot travel: under an OR, where the residual would wrongly
	// filter the other disjunct, and under a negation, where it cannot be
	// hoisted past the NOT. See degrade.
	plain string
}

func (f fragment) isText() bool { return f.text != "" }

// outerSearch counts the search functions in this fragment's SQL that sit in the
// OUTER scan — that is, not inside a subquery.
//
// It is DERIVED rather than stored, and that is the whole point of this design.
// The previous version carried an `searchFuncs int` field that each construction
// had to remember to set, and `fragment{sql: …}` appears 47 times in this file.
// Three of them forgot: `not()` discarded it, so
// `NOT (level:ERROR RemoteStopped) OR level:WARN` lost 17.5%; the fuzzy leaf
// never set it, so `level:WARN OR (level:ERROR RemoteStopped~1)` lost 54.1%; and
// `degrade()` dropped it, so `("RemoteStopped to" rejections) OR level:WARN`
// returned 0 against a truth of 60,727. Each was found, and fixed, one round
// after the last — because a field some constructions remember to set is a
// whitelist, and every new leaf or combinator is a chance to be left off it.
//
// Deriving it from the string cannot be forgotten. Whatever SQL a construction
// builds, present or future, the count follows it.
func (f fragment) outerSearch() int { return outerSearchFuncs(f.sql) }

// outerSearchFuncs counts search functions in SQL, ignoring any that sit inside
// a subquery body.
//
// A search function inside `(SELECT …)` prunes only that subquery's scan, which
// is precisely why hoisting one in there is the fix; so it must not be counted
// as living in the outer scan. Everything outside one must be, because this
// engine pushes a search function into the index scan regardless of the
// surrounding boolean.
func outerSearchFuncs(sql string) int {
	// Literals FIRST, before anything looks at structure. A search value is
	// untrusted input that ends up inside a string literal, and a literal can
	// contain parentheses and the word SELECT. Without masking,
	// `level:"(SELECT ("` made stripSubqueryBodies read the literal's `(SELECT`
	// as a real subquery and run past the closing quote, swallowing the genuine
	// query() that followed — so `level:WARN OR (level:"(SELECT (" RemoteStopped)`
	// was left unhoisted and returned 27,502 of a true 60,727, a 54.7% loss
	// driven by nothing more than what somebody typed in the search box.
	//
	// Any pass that treats this SQL as structure has to come after the mask.
	return countSearchCalls(stripSubqueryBodies(maskLiterals(sql)))
}

// maskLiterals overwrites the CONTENTS of every single-quoted literal with a
// filler, leaving the quotes and the overall length in place so that later
// passes see the same offsets.
//
// It handles both escapes this compiler emits — escapeString doubles a quote to
// `”` and a backslash to `\\` — because either one, misread as the end of the
// literal, puts the scanner back into "structure" mode inside a value.
//
// An UNTERMINATED literal is deliberately left untouched rather than masked to
// the end of the string. Erasing the tail would delete real query() calls after
// it and UNDER-count, which is the direction that silently loses rows; leaving it
// alone can only over-count, which costs a needless subquery or a refusal.
func maskLiterals(sql string) string {
	b := []byte(sql)

	// Check every literal is terminated BEFORE masking any of them. Bailing out
	// part-way through is not good enough: with an odd number of quotes the
	// scanner pairs them wrongly from the start, and `x = 'ab AND query('line:y')`
	// then had its `query(` masked away — an UNDER-count, which is the direction
	// that silently loses rows. This compiler never emits an unbalanced literal
	// (escapeString doubles every quote), so the case is defensive; it just has
	// to fail towards over-counting rather than under.
	for i := 0; i < len(b); i++ {
		if b[i] != '\'' {
			continue
		}
		end, ok := literalEnd(b, i)
		if !ok {
			return sql
		}
		i = end
	}

	for i := 0; i < len(b); i++ {
		if b[i] != '\'' {
			continue
		}
		end, _ := literalEnd(b, i)
		for j := i + 1; j < end; j++ {
			b[j] = 'x'
		}
		i = end
	}
	return string(b)
}

// literalEnd returns the index of the quote that closes the literal opened at
// `open`, skipping `”` and backslash escapes.
func literalEnd(b []byte, open int) (int, bool) {
	for j := open + 1; j < len(b); j++ {
		switch {
		case b[j] == '\\' && j+1 < len(b):
			j++ // the escaped byte cannot close the literal
		case b[j] == '\'':
			if j+1 < len(b) && b[j+1] == '\'' {
				j++ // a doubled quote is one escaped quote, not the end
				continue
			}
			return j, true
		}
	}
	return 0, false
}

// stripSubqueryBodies removes `(SELECT … )` spans, matching parentheses so a
// nested subquery goes with its parent.
func stripSubqueryBodies(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); {
		if sql[i] == '(' && startsSelect(sql[i+1:]) {
			depth := 0
			j := i
			for ; j < len(sql); j++ {
				switch sql[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
				if depth == 0 {
					break
				}
			}
			if j >= len(sql) {
				// Unbalanced `(SELECT` with no closing paren: keep the rest
				// rather than dropping it, so the count comes out too HIGH
				// rather than too low. Over-counting costs a needless subquery
				// or a refusal; under-counting loses rows in silence.
				//
				// That direction only actually holds because literals are
				// masked before this runs. It did not hold before: a literal
				// containing `(SELECT` and an unmatched `(` made this run past
				// the closing quote and swallow the real query() after it,
				// which is an UNDER-count and exactly the losing direction.
				break
			}
			i = j + 1
			continue
		}
		b.WriteByte(sql[i])
		i++
	}
	return b.String()
}

func startsSelect(s string) bool {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t') {
		s = s[1:]
	}
	return len(s) >= 6 && strings.EqualFold(s[:6], "SELECT")
}

// countSearchCalls counts `query(` and `match(` at an identifier boundary, so a
// column or function whose name merely ends in one of them is not miscounted.
func countSearchCalls(sql string) int {
	n := 0
	for _, name := range []string{"query(", "match("} {
		for i := 0; ; {
			j := strings.Index(sql[i:], name)
			if j < 0 {
				break
			}
			at := i + j
			if at == 0 || !isIdentByte(sql[at-1]) {
				n++
			}
			i = at + len(name)
		}
	}
	return n
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

type compiler struct {
	schema   Schema
	warnings []string

	// textCols records every column that reached a text fragment, so a
	// negated fragment knows which column its anti-join keys on.
	textCols []string

	// converted records which fields have already had their cast advisory
	// raised, so a two-sided range does not repeat it.
	converted map[string]bool

	// textIndexes records the distinct inverted indexes those columns belong
	// to. Every text leaf merges into one query() call, and that call is legal
	// across several columns only when a single index covers all of them —
	// measured, a table with separate idx_line(line) and idx_line2(line2)
	// answers each column alone but fails [1065] "columns line2, line don't
	// have inverted index" for `query('line:x AND line2:x')`. Two columns of
	// one index compose fine: over the frozen copy, `query('line:RemoteStopped
	// AND msg:rpc')` on a table indexed (msg, line) returns 585.
	//
	// A schema that does not name its indexes leaves this empty and nothing is
	// checked, which is the previous behaviour.
	textIndexes []string
}

func (c *compiler) warn(format string, args ...interface{}) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

// bagOf names the bag a field resolved out of, for the advisory. It repeats the
// routing rather than being told the answer because the warning is the only
// place the name is needed and Lookup's signature is a public contract.
func (c *compiler) bagOf(name string) string {
	f, known := c.schema.Lookup(name)
	if known || f.Presence == "" {
		return c.schema.Variant
	}
	if i := strings.IndexAny(f.Presence, "['"); i > 0 {
		return f.Presence[:i]
	}
	return c.schema.Variant
}

// noteConversion warns that this field is read through a per-value cast.
//
// It is the timestamp half of the advisory numericColumn gives, and it was
// missing. The asymmetry is what made it a defect rather than an omission: a
// numeric bound on a string-valued expression warned in full, naming the field
// and handing over a count, while a VARCHAR column read as an instant warned
// about nothing at all — and that is the worse of the two, because the time role
// gates EVERY time-bounded query rather than one filter. A value that does not
// cast leaves its row out of every dashboard panel at once.
//
// Warned once per field per statement, so a two-sided range does not say it
// twice.
func (c *compiler) noteConversion(f Field, name string) {
	if f.Conversion == "" {
		return
	}
	if c.converted == nil {
		c.converted = map[string]bool{}
	}
	if c.converted[name] {
		return
	}
	c.converted[name] = true
	msg := fmt.Sprintf("%q is not stored as %s — it is read through a cast (%s), and a value "+
		"that does not cast becomes NULL, so that row is EXCLUDED from the comparison rather "+
		"than raising an error. On the time field that removes the row from EVERY time-bounded "+
		"query, not just this one.", name, KindName(f.Kind), f.Conversion)

	// Only offer the count when the inner value can be named. Otherwise the
	// predicate would read `expr IS NOT NULL AND expr IS NULL`, which is always
	// 0 — and a reader who runs it concludes that nothing was dropped, which is
	// the opposite of what this warning is for.
	if inner, ok := unwrapCast(f.Column); ok {
		msg += fmt.Sprintf(" Count them with count_if(%s IS NOT NULL AND %s IS NULL)",
			inner, f.Column)
	} else {
		msg += " To count them, compare the rows where the underlying column has a value " +
			"against the rows where this expression is not null: the schema declares the " +
			"expression only, so the column it reads cannot be named here."
	}
	c.warn("%s", msg)
}

// unwrapCast recovers the inner expression a conversion is applied to, so the
// count predicate can ask "has a value but does not convert" rather than the
// vacuous "converts and does not convert".
//
// Three shapes, because a conversion is not always a TRY_CAST: `TRY_CAST(x AS
// T)`, a single-argument call like `to_timestamp(x)` -- which is how a BIGINT
// epoch column is read as an instant -- and `x::T`. Returns the input unchanged
// when it recognises none of them, and the caller MUST test for that: emitting
// `expr IS NOT NULL AND expr IS NULL` ships a predicate that is always 0, which
// is worse than omitting it, because a reader who runs it concludes nothing was
// dropped.
func unwrapCast(expr string) (string, bool) {
	e := strings.TrimSpace(expr)

	if strings.HasPrefix(strings.ToUpper(e), "TRY_CAST(") && strings.HasSuffix(e, ")") {
		inner := e[len("TRY_CAST(") : len(e)-1]
		if i := strings.LastIndex(strings.ToUpper(inner), " AS "); i > 0 {
			return strings.TrimSpace(inner[:i]), true
		}
	}

	// `x::T` -- take the left of the last `::`, which is the value being cast.
	if i := strings.LastIndex(e, "::"); i > 0 {
		return strings.TrimSpace(e[:i]), true
	}

	// A single-argument call: `name(arg)` with balanced parentheses and no
	// top-level comma, so `to_timestamp(ts_micros)` unwraps and
	// `concat_ws(' ', a, b)` deliberately does not.
	if i := strings.IndexByte(e, '('); i > 0 && strings.HasSuffix(e, ")") {
		name := e[:i]
		if isIdent(name) {
			arg, depth := e[i+1:len(e)-1], 0
			for _, r := range arg {
				switch r {
				case '(':
					depth++
				case ')':
					depth--
				case ',':
					if depth == 0 {
						return expr, false
					}
				}
			}
			if a := strings.TrimSpace(arg); a != "" {
				return a, true
			}
		}
	}

	return expr, false
}

// isIdent reports whether s is a bare SQL function or column name.
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// numericColumn is Field.NumericColumn plus the advisory it owes the caller.
//
// It warns whenever a conversion is actually introduced — that is, whenever the
// field is not already a numeric column. The warning is not decoration. TRY_CAST
// was chosen over a cast because a cast fails the whole statement on the first
// value that is not a number, and one bad value must not make a legitimate
// query unanswerable. But the price of that choice is that non-numeric values
// become NULL and their rows drop out of the comparison with nothing said, and
// the exposure on a real log table is not marginal. Over logs.k8s_logs_v2
// (967,912 rows, ts < 2026-08-19 22:19:00), rows that hold the key but whose
// value does not cast:
//
//	kv key        rows with key   silently dropped
//	store                16,154             15,784   97.7%
//	to                    6,604              5,681   86.0%
//	observe_id            3,369              3,369    100%
//	vote                  4,952              2,171   43.8%
//	duration              1,945              1,943   99.9%
//	store_id             40,516              1,243    3.1%
//	from                  9,937                360    3.6%
//	id                    5,059                  5
//	tableID             231,986                  3
//
// 30,559 rows across nine keys, and the distribution is worse than the total:
// the keys a human puts a bound on are the worst ones. Every one of duration's
// 1,945 values is a Go duration — `47.823614ms`, `39.422927ms` — so
// `duration:>100`, the most natural latency query there is, returns 0 of 1,945
// with no error at all. Under the old hard cast that same query was
// `[1006] invalid float literal ... to_float64('47.823614ms')`, which was
// useless as an answer but pointed straight at the unit suffix.
//
// So the warning has to carry the whole mitigation, and it has to be worth
// reading. It names the field, says the rows are excluded rather than counted,
// and hands over the predicate that counts them — because the caller is the only
// one who can run it.
//
// One thing it cannot do is reach a Grafana user. The frame-notice channel is
// not in the deployed plugin (docs/grafana-macro.md says so in the same words),
// so today this text arrives only in the SQL comment the query inspector shows.
// That is a gap in the plugin, not in the wording, and it is why the wording
// leans on the count-them predicate rather than on being noticed.
func (c *compiler) numericColumn(f Field, name string) string {
	expr := f.NumericColumn()
	if f.Kind == Number && f.Numeric == "" {
		// A real numeric column. No conversion, nothing to warn about.
		return expr
	}
	probe := f.Column
	if f.Presence != "" {
		probe = f.Presence
	}
	c.warn("comparing %q as a number converts it with TRY_CAST, because it is not a numeric "+
		"column: a value that is not a number becomes NULL, so that row is EXCLUDED from the "+
		"comparison rather than raising an error. A zero or short result may mean the values "+
		"carry a unit or a wrapper rather than that nothing matched — a Go duration "+
		"(`47.823614ms`) and a debug rendering (`Some(25)`) both fail to cast silently. Count "+
		"them with count_if(%s IS NOT NULL AND %s IS NULL)",
		name, probe, expr)
	return expr
}

func (c *compiler) noteText(f Field) {
	c.textCols = appendUnique(c.textCols, f.Column)
	if f.Index != "" {
		c.textIndexes = appendUnique(c.textIndexes, f.Index)
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// finalize converts the root fragment into SQL, spending a search function if
// the fragment is full-text.
func (c *compiler) finalize(f fragment) (string, error) {
	if !f.isText() {
		if f.sql == "" {
			return MatchAll, nil
		}
		return f.sql, nil
	}
	return c.wrapText(f)
}

// checkOneIndex refuses a text expression whose columns live in different
// inverted indexes.
//
// Every text leaf merges into one query() call, and that is the only shape
// this engine allows — but a single call reaches only the columns of a single
// index. Measured on a probe table with separate idx_line(line) and
// idx_line2(line2), each column answers alone (1 row each) while
// `query('line:RemoteStopped AND line2:RemoteStopped')` fails
// `[1065] columns line2, line don't have inverted index`. The message names
// both columns as unindexed, which is misleading enough that the error is
// worth pre-empting here with one that says what actually happened.
//
// Columns of the *same* index compose freely, which is the whole reason the
// derived text surface is indexed alongside msg rather than beside it: over
// the frozen copy, `query('line:RemoteStopped AND msg:rpc')` returns 585.
//
// A schema that names no indexes leaves textIndexes empty and nothing is
// checked. That is deliberate: an unnamed index is unknown, not absent, and
// refusing on a guess would break every schema written before indexes could be
// declared.
func (c *compiler) checkOneIndex() error {
	if len(c.textIndexes) < 2 {
		return nil
	}
	return fmt.Errorf(
		"this query searches columns in %d different inverted indexes (%s) but one query() "+
			"call reaches only the columns of one index: search a single text field, or index "+
			"the columns together (CREATE INVERTED INDEX ... ON <table>(%s))",
		len(c.textIndexes), strings.Join(c.textIndexes, ", "), strings.Join(c.textCols, ", "))
}

// wrapText turns a query()-language expression into SQL, spending the
// statement's one search function.
//
// # Why a negated fragment is not simply wrapped in NOT
//
// `NOT (query(x))` is not the complement of `query(x)`. The optimiser pushes
// the search function into the index scan regardless of the surrounding
// boolean — the same mechanism that makes an empty search argument return
// nothing — so when x matches no row at all, the scan is pruned to nothing and
// the NOT returns **zero rows instead of every row**. Measured over a
// 152,317-row window:
//
//	token              query()   NOT (query())   anti-join
//	zzzznosuchtoken          0               0     152,317
//	qqqqwwww                 0               0     152,317
//	pdctl                    0               0     152,317
//	tiflash             23,381         128,936     128,936
//
// The three absent tokens are the defect: "everything except a term that does
// not occur here" is the whole window, and the bare NOT answers zero. It is a
// realistic thing to ask — excluding a noise pattern that happens not to occur
// in the selected time range — and it fails silently. No SQL-level wrapping
// rescues it: COALESCE, CASE, `= FALSE`, `AND TRUE` and `1=1 OR …` were all
// measured and all return zero, because the pruning happens before any of them
// is evaluated.
//
// The anti-join does rescue it, exactly, and keeps tokenised semantics rather
// than degrading to a substring match: the search function runs in its own
// scan, where pruning it to nothing correctly yields an empty exclusion set.
// A negation that keeps a positive term beside it — `a -b` — never comes here
// at all, because it stays inside one query() call where the positive drives
// the scan.
func (c *compiler) wrapText(f fragment) (string, error) {
	if err := c.checkOneIndex(); err != nil {
		return "", err
	}
	if f.negated {
		return c.antiJoin(f)
	}
	sql := "query('" + escapeString(f.text) + "')"
	if f.residual != "" {
		// The residual of a collapsed phrase. It is ANDed here, at the one
		// point where the whole text expression becomes SQL, so it applies to
		// the conjunction the leaf is part of and never to a disjunction —
		// degrade() has already taken the OR and NOT cases away.
		sql = "(" + sql + " AND " + f.residual + ")"
	}
	return sql, nil
}

// degrade renders a residual-carrying text fragment as plain SQL.
//
// A residual is only sound under AND. Under `a OR "not ready"` it would filter
// the `a` disjunct as well, and under a negation it would have to be hoisted
// past a NOT that does not distribute over it. In both places the leaf falls
// back to its pure-SQL form, which is the substring scan alone: measured on two
// disjoint frozen windows, that returns exactly the same rows as the token and
// the scan together (250 = 250 and 1,682 = 1,682 for `"not ready"`), so the
// fallback loses nothing but the index.
//
// A fragment that has already been combined with others has no `plain` of its
// own; it is spent as SQL instead, which costs a search function and therefore
// behaves exactly as it did before this residual existed.
func (c *compiler) degrade(f fragment) (fragment, error) {
	if f.residual == "" || !f.isText() {
		return f, nil
	}
	if f.plain != "" {
		return fragment{sql: f.plain}, nil
	}
	sql, err := c.wrapText(f)
	if err != nil {
		return fragment{}, err
	}
	return fragment{sql: sql}, nil
}

// antiJoin renders the complement of a full-text expression.
func (c *compiler) antiJoin(f fragment) (string, error) {
	if c.schema.Table == "" {
		return "", fmt.Errorf(
			"excluding a full-text term with no positive term beside it needs the table name, " +
				"because `NOT (query(...))` returns 0 rows rather than every row whenever the " +
				"excluded expression matches nothing: set Schema.Table, or write `a -b` so the " +
				"exclusion stays inside the search function")
	}

	// Keyed on the row when the schema names one, which is the only exact
	// form. Over logs.k8s_logs, ts < 2026-08-20 03:30:00 (1,063,259 rows),
	// `-snapshot` should be 1,063,259 − 26,327 = 1,036,932, and the row-keyed
	// anti-join returns exactly that.
	//
	// No COALESCE: the row key is never NULL — verified, 0 nulls over 1,063,259
	// rows and over the 967,912-row frozen copy — so neither the outer value
	// nor the subquery can produce the NULL that the text-keyed form has to
	// defend against.
	if sql, ok := c.rowKeyMembership(f.text, "", true); ok {
		c.warn("excluding %q with no positive term beside it compiles to an anti-join against "+
			"%s, which scans the table a second time: `a -b` keeps the exclusion inside the one "+
			"search function and costs nothing extra", f.text, c.schema.Table)
		return sql, nil
	}

	// No row key: key on the text column instead. That is EXACT for every shape
	// this compiler emits, and the reason is structural rather than lucky, so it
	// is worth writing down.
	//
	// The anti-join excludes rows whose keyed value appears among the matching
	// rows' keyed values. When the keyed column is the same column the search
	// ran against, that expansion is idempotent: two rows with identical text
	// produce identical tokens, so the index either matches both or neither.
	// Measured — `query('msg:snapshot')` is 17,661 rows and
	// `msg IN (SELECT msg WHERE query('msg:snapshot'))` is also 17,661.
	//
	// And the compiler always keys on the column it searched: antiJoin uses
	// c.textCols[0], and a fragment spanning two text columns is refused just
	// below. So the collision case cannot arise. Verified across 14
	// term/preset combinations on a 1,063,259-row table — msg-keyed under the
	// msg-default preset and line-keyed under the line-default preset — every
	// one identical to the row-keyed answer.
	//
	// The shape that IS wrong is a MISMATCH: keying on msg while searching
	// line gives 1,035,332 against a truth of 1,036,932. That is a hand-written
	// combination, not one this compiler produces, and the row-key form is
	// preferred anyway because it stays correct if that ever changes and needs
	// no COALESCE.
	col := c.textCols[0]
	c.warn("excluding %q with no positive term beside it compiles to an anti-join against %s "+
		"keyed on %q, because this schema declares no row key. It is exact — the keyed column "+
		"is the column the search ran against, so two rows with the same text match or miss "+
		"together — but it scans the table a second time, and it would stop being exact if the "+
		"keying and the search ever diverged. Declaring \"row_key\" (`_row_id` on this engine) "+
		"removes both concerns. `a -b` keeps the exclusion inside the one search function and "+
		"costs nothing extra",
		col, c.schema.Table, col)

	// COALESCE covers a NULL in the outer row — a row with no text at all
	// contains nothing, so it survives the exclusion — and the subquery
	// excludes NULLs of its own so a NULL can never poison the NOT IN.
	return fmt.Sprintf(
		"COALESCE(%s NOT IN (SELECT %s FROM %s WHERE %s IS NOT NULL AND query('%s')), TRUE)",
		col, col, c.schema.Table, col, escapeString(f.text)), nil
}

// semiJoin renders a text expression as a row-key membership test, so it can
// stand inside a disjunction.
//
// # Why a bare query() cannot
//
// The optimiser pushes a search function into the index scan regardless of the
// surrounding boolean. That is the same mechanism that makes an empty search
// argument return nothing and that makes a bare `NOT (query(x))` return zero
// instead of everything — and under OR it is worse, because it silently discards
// the other branch. Measured over logs.k8s_logs, ts < 2026-08-20 03:30:00
// (1,063,259 rows), against truths computed independently:
//
//	                                                          emitted    truth
//	query('line:zzzznosuchtoken') OR lower(level)='ERROR'            0  402,974
//	query('line:zzzznosuchtoken') OR 1=1                             0  1,063,259
//	query('line:snapshot')        OR lower(level)='ERROR'      424,841  424,841
//
// The third line is why this went unnoticed: the shape is correct whenever the
// text side matches at least one row, and 26,327 + 402,974 − 4,460 = 424,841
// checks out. A typo in one branch of an OR was enough to void the query.
//
// Keyed on the row, both cases are exact — 402,974 and 424,841.
//
// # The fallback, and why it is exact
//
// A pure text fragment CAN be keyed on its own text column, and that is exact
// rather than approximate. The predicate inside the subquery is a function of
// that one column, so two rows with the same value match or miss together and
// expanding the match set by shared value is idempotent — measured,
// `query('line:region')` is 228,209 rows and
// `line IN (SELECT line WHERE query('line:region'))` is also 228,209, and the
// disjunction is exact for every term tried: 403,699 / 405,734 / 407,681 /
// 424,841 / 622,935, each equal to the row-keyed answer.
//
// An earlier version of this comment claimed the text-keyed form over-matched,
// with a percentage attached. It does not, and the way that number was got wrong
// is worth keeping: the measurement keyed on `msg` while searching `line` — the
// very mismatch this file warns about elsewhere — so it measured a shape nothing
// emits. Keyed on the column it searches there is no error to trade away.
//
// So the row key is preferred for cost — comparing an 18-byte identifier beats
// comparing a log line that can run to 29KB — and the text column is a correct
// fallback. What is NOT sound is keying a COMPOSITE branch this way, because
// such a branch constrains other columns too; see hoistIntoSubquery.
//
// Two shapes are still refused, and only two: a fragment spanning more than one
// text column, where keying on one of them is not idempotent, and a composite
// branch with no row key.
// rowKeyMembership is the one construction every subquery shape is built from.
//
// A search function inside a subquery prunes only that subquery's scan, which is
// the whole trick: the outer predicate then tests row membership, an ordinary
// comparison no optimiser rewrites. `IN` for a positive branch, `NOT IN` for a
// negated one — the same move, opposite polarity — so the disjunction fix and the
// negation fix are one mechanism rather than two similar-looking pieces of code.
//
// The repo already contained half of it: a negated branch ORed with SQL was
// always exact — `-zzzznosuchtoken OR level=ERROR` returns the whole table,
// 1,063,259 — precisely because the negation compiled to an anti-join. The bug
// was that the positive branch did not get the same treatment.
//
// # On stability, since everything here rests on it
//
// The subquery and the outer scan must agree about which rows exist, on a table
// taking thousands of rows a minute. They do, because one statement over a
// closed bound is one snapshot: the same semi-join returned 424,841 three times
// consecutively, and `_row_id IN (SELECT _row_id FROM t)` is the whole table
// while `NOT IN` of the same is 0 — self-identity across the boundary, measured.
//
// Returns ok=false when the schema has nothing to key on; the callers differ on
// what to do about that, and deliberately so.
func (c *compiler) rowKeyMembership(text, residual string, negate bool) (sql string, ok bool) {
	if c.schema.RowKey == "" || c.schema.Table == "" {
		return "", false
	}
	op := "IN"
	if negate {
		op = "NOT IN"
	}
	where := "query('" + escapeString(text) + "')"
	if residual != "" {
		// A collapsed phrase's literal scan belongs INSIDE the subquery, where
		// it narrows the same rows the search function selected. Hoisted
		// outside it would filter the other disjunct too.
		where += " AND " + residual
	}
	// No COALESCE. The row key is never NULL — verified, 0 nulls over 1,063,259
	// rows and over the 967,912-row frozen copy — so neither the outer value nor
	// the subquery can produce the NULL that a text-keyed NOT IN must defend
	// against.
	return fmt.Sprintf("%s %s (SELECT %s FROM %s WHERE %s)",
		c.schema.RowKey, op, c.schema.RowKey, c.schema.Table, where), true
}

// hoistIntoSubquery moves a whole COMPOSITE branch into a row-key subquery.
//
// A branch like `(lower(level)=lower('ERROR') AND query('line:RemoteStopped'))`
// contains a search function but is no longer a text fragment — the conjunction
// already collapsed it, which is exactly why a `text`-based guard could not see
// it. Under OR it therefore prunes the scan for the entire predicate:
// `level:WARN OR (level:ERROR RemoteStopped)` returned 27,502 where the truth is
// 60,727, losing 54.7% in silence, and that predicate was byte-identical before
// and after the round that was supposed to remove this class of loss.
//
// The whole branch goes inside, not just the search function, because the branch
// is a predicate over the same table and membership by row key is exactly
// equivalent.
//
// Only a row key will do here, and that is measured rather than cautious.
// Keying a composite branch on its text column is NOT idempotent, because the
// branch constrains other columns too: two rows sharing a text value can differ
// in `level`, and then expanding the match set by shared text pulls in a row the
// branch excluded. Measured on the live table, where exactly two `msg` values of
// 160,802 occur at more than one level — the row-keyed truth for `level:ERROR tso` is 1,728 rows and the
// msg-keyed form gives 1,884 — 156 over. The magnitude is a property of the data
// rather than a general ratio: it is however many rows share a text value across
// differing values of the other columns the branch constrains, which here is two
// msg values out of 160,802.
//
// An earlier version of this comment said 45 rows against 201, over by 4.5x.
// That was measured on a hand-built proxy predicate rather than on a shape this
// compiler emits, and it does not reproduce; the figures above are the real one.
//
// A pure text fragment has no such problem, which is why semiJoin can fall back
// and this cannot.
func (c *compiler) hoistIntoSubquery(f fragment) (string, error) {
	if c.schema.RowKey == "" || c.schema.Table == "" {
		return "", fmt.Errorf(
			"a condition combining a full-text term with something else cannot be ORed with "+
				"another condition unless the schema declares a row key, and this one declares "+
				"%s. This engine visits only the blocks its index says contain matches and "+
				"evaluates the rest of the predicate only there, so the other branch loses its "+
				"rows in every block the term did not reach — measured, "+
				"`level:WARN OR (level:ERROR RemoteStopped)` returned 27,502 of a true 60,727. "+
				"Keying the branch on its text column instead is not sound either, because the "+
				"branch constrains other columns too. Declare \"row_key\" (and \"table\"), or "+
				"rewrite so the full-text terms are ANDed with the rest rather than ORed",
			c.missingForSubquery())
	}
	return fmt.Sprintf("%s IN (SELECT %s FROM %s WHERE %s)",
		c.schema.RowKey, c.schema.RowKey, c.schema.Table, f.sql), nil
}

func (c *compiler) semiJoin(f fragment) (string, error) {
	if f.negated {
		// A NEGATED branch in a disjunction is the case that was already
		// correct, and it must stay that way. `-zzzznosuchtoken OR level=ERROR`
		// returns the whole table, 1,063,259, because the negation compiles to
		// an anti-join and the search function therefore prunes only its own
		// subquery. That is the pattern this whole change generalises, so the
		// negated branch delegates to it rather than being handled twice.
		//
		// It also means the no-row-key policy differs for the two polarities,
		// which is right rather than untidy: the pruning defect does not touch
		// the negated form at all — measured, it returns 1,063,259 under either
		// keying — so its only flaw is the text-collision over-exclusion, which
		// is bounded and warned about. The positive form has no bounded
		// fallback, so it is refused.
		return c.antiJoin(f)
	}
	if err := c.checkOneIndex(); err != nil {
		return "", err
	}
	if sql, ok := c.rowKeyMembership(f.text, f.residual, false); ok {
		return sql, nil
	}
	// No row key: key on the text column itself, which is exact when the
	// fragment searches exactly one. `len(c.textCols)` is a statement-wide count
	// rather than a per-fragment one, so this is conservative — it may refuse a
	// compilable query, but it never emits a wrong one.
	if c.schema.Table != "" && len(c.textCols) == 1 {
		col := c.textCols[0]
		where := "query('" + escapeString(f.text) + "')"
		if f.residual != "" {
			where += " AND " + f.residual
		}
		c.warn("a full-text term ORed with a non-full-text condition is compiled as a subquery, "+
			"because a search function inside a disjunction makes this engine visit only the "+
			"blocks its index says match and evaluate the other branch only there. With no row "+
			"key declared the subquery is keyed on %q, which is exact but compares whole values "+
			"— declare \"row_key\" (`_row_id` on this engine) to key on an identifier instead",
			col)
		return fmt.Sprintf("%s IN (SELECT %s FROM %s WHERE %s)",
			col, col, c.schema.Table, where), nil
	}
	{
		return "", fmt.Errorf(
			"a full-text term ORed with a non-full-text condition needs a row key, and this "+
				"schema declares %s. Without one the search function has to sit directly in the "+
				"disjunction, where this engine prunes the scan to nothing whenever the search "+
				"matches nothing and the other branch is discarded with it — measured, "+
				"`query(...) OR 1=1` returns 0 rows out of 1,063,259. Declare \"row_key\" (and "+
				"\"table\"), or rewrite the query so the full-text terms are ANDed rather than "+
				"ORed with the rest, for example `(a OR b) level:error` instead of "+
				"`a OR level:error`", c.reasonNoSubqueryKey())
	}
}

// reasonNoSubqueryKey explains why neither key is available, which by the time
// it is called means the fragment spans several text columns or there is no
// table.
func (c *compiler) reasonNoSubqueryKey() string {
	if len(c.textCols) > 1 {
		return fmt.Sprintf("no row key, and the search spans %d text columns (%s) so keying on "+
			"one of them would not be exact", len(c.textCols), strings.Join(c.textCols, ", "))
	}
	return c.missingForSubquery()
}

func (c *compiler) missingForSubquery() string {
	switch {
	case c.schema.RowKey == "" && c.schema.Table == "":
		return "neither a row key nor a table"
	case c.schema.RowKey == "":
		return "no row key"
	default:
		return "no table"
	}
}

func (c *compiler) render(n parser.Node) (fragment, error) {
	switch t := n.(type) {
	case *parser.And:
		return c.boolean(t.Children, "AND")
	case *parser.Or:
		return c.boolean(t.Children, "OR")
	case *parser.Not:
		return c.not(t.Child)
	case *parser.Term:
		return c.term(t)
	case *parser.Range:
		return c.rangeNode(t)
	case *parser.Between:
		return c.betweenNode(t)
	default:
		return fragment{}, fmt.Errorf("unsupported node %T", n)
	}
}

func (c *compiler) not(child parser.Node) (fragment, error) {
	f, err := c.render(child)
	if err != nil {
		return fragment{}, err
	}
	// A residual cannot be hoisted past a NOT: `NOT (token AND scan)` is not
	// `NOT token AND NOT scan`. The leaf falls back to its pure-SQL form.
	if f, err = c.degrade(f); err != nil {
		return fragment{}, err
	}
	if f.isText() {
		// Carried, not applied — see fragment.negated.
		f.negated = !f.negated
		return f, nil
	}
	// COALESCE, not a bare NOT: every column in a log table is nullable, and
	// `NOT (col = 'x')` is NULL — and therefore excluded — wherever col is
	// NULL, so `x` and `-x` would not add up to the table. Measured on the
	// VARIANT path, where a missing key makes it bite immediately:
	// kv['container']='vector' is 6,742 and NOT(...) is 443,148, three short
	// of the 449,893 total; the COALESCE form returns 443,151 and partitions
	// exactly. Unknown means "not excluded".
	// A bare NOT around a search function is the round-one defect, and it
	// reaches here whenever the negated child was a mixed conjunction that
	// collapsed its text into sql. The optimiser prunes the scan to the blocks
	// the index says match and then negates WITHIN them, so the rows in every
	// other block are lost. Measured over ts < 2026-08-20 04:00:00 (1,072,856
	// rows), where `level:ERROR AND line:RemoteStopped` is empty so the
	// complement is the whole table:
	//
	//	COALESCE(NOT ((level=ERROR AND query('line:RemoteStopped'))), TRUE)
	//	                                                    883,884
	//	_row_id NOT IN (SELECT _row_id … WHERE level=ERROR AND query(…))
	//	                                                  1,072,856   exact
	//
	// Hoisting the branch into a disjunction's subquery does not help, because
	// the bare NOT travels inside with it: that shape returned 917,109 against
	// the same 1,072,856. The negation itself has to become the anti-join.
	if f.outerSearch() > 0 {
		return c.negateWithRowKey(f)
	}
	return fragment{sql: "COALESCE(NOT (" + f.sql + "), TRUE)"}, nil
}

// negateWithRowKey complements a whole composite branch through an anti-join, so
// the search function inside it prunes only its own scan.
//
// Only a row key will do. Keying on the text column is not sound here for the
// same reason it is not sound in hoistIntoSubquery: the branch constrains other
// columns too, so expanding a match set by shared text is not idempotent.
func (c *compiler) negateWithRowKey(f fragment) (fragment, error) {
	if c.schema.RowKey == "" || c.schema.Table == "" {
		return fragment{}, fmt.Errorf(
			"negating a condition that combines a full-text term with something else needs a "+
				"row key, and this schema declares %s. A bare NOT around a search function is "+
				"not its complement on this engine — the scan is pruned to the blocks the index "+
				"says match and the negation is evaluated only there, so every other block's "+
				"rows are lost: measured, 883,884 rows against a true 1,072,856. Declare "+
				"\"row_key\" (and \"table\"), or negate the full-text term on its own "+
				"(`-term`) rather than negating the group it sits in",
			c.missingForSubquery())
	}
	return fragment{sql: fmt.Sprintf("%s NOT IN (SELECT %s FROM %s WHERE %s)",
		c.schema.RowKey, c.schema.RowKey, c.schema.Table, f.sql)}, nil
}

// boolean combines children, keeping everything in the text language when it
// can so that only one search function is spent.
func (c *compiler) boolean(children []parser.Node, op string) (fragment, error) {
	frags := make([]fragment, 0, len(children))
	allText := true
	for _, ch := range children {
		f, err := c.render(ch)
		if err != nil {
			return fragment{}, err
		}
		if op == "OR" {
			// A residual ANDed onto a disjunction would filter the other
			// disjuncts too, so the leaf that carries one is rendered as
			// plain SQL here instead.
			if f, err = c.degrade(f); err != nil {
				return fragment{}, err
			}
		}
		frags = append(frags, f)
		if !f.isText() {
			allText = false
		}
	}

	switch len(frags) {
	case 0:
		return fragment{}, nil
	case 1:
		return frags[0], nil
	}

	if allText {
		return c.textBoolean(frags, op)
	}

	// Mixed. The text children are merged into a single text expression first
	// and wrapped once — `level:error "peer status" -TiFlash` is an ordinary
	// query, and wrapping each text leaf separately would spend three search
	// functions on it and fail.
	var textFrags []fragment
	parts := make([]string, 0, len(frags))
	textSlot := -1
	for _, f := range frags { //nolint:dupl // rebuilt after the OR hoist below
		if f.isText() {
			if textSlot < 0 {
				textSlot = len(parts)
				parts = append(parts, "") // reserved, filled in below
			}
			textFrags = append(textFrags, f)
			continue
		}
		parts = append(parts, f.sql)
	}

	// Any child whose SQL already carries a search function cannot stand in a
	// disjunction either, and this is where three families were missed. A
	// conjunction collapses its text child into sql, a negation wraps sql in
	// COALESCE(NOT …), a degraded phrase spends its text as sql, and a fuzzy
	// leaf is born as sql — in every one of those the search function is inside
	// `sql` with no `text` left to notice it by. Asking the SQL directly is what
	// makes the check total rather than a list of remembered cases.
	//
	// The whole branch is hoisted, not just the search function: it is a
	// predicate over the same table, so membership by row key is exactly
	// equivalent.
	if op == "OR" {
		for i := range frags {
			if frags[i].isText() || frags[i].outerSearch() == 0 {
				continue
			}
			sub, err := c.hoistIntoSubquery(frags[i])
			if err != nil {
				return fragment{}, err
			}
			frags[i] = fragment{sql: sub}
		}
		// Rebuild parts from the (possibly rewritten) fragments.
		parts = parts[:0]
		textSlot = -1
		textFrags = nil
		for _, f := range frags {
			if f.isText() {
				if textSlot < 0 {
					textSlot = len(parts)
					parts = append(parts, "")
				}
				textFrags = append(textFrags, f)
				continue
			}
			parts = append(parts, f.sql)
		}
	}

	if len(textFrags) > 0 {
		merged := textFrags[0]
		if len(textFrags) > 1 {
			var err error
			merged, err = c.textBoolean(textFrags, op)
			if err != nil {
				return fragment{}, err
			}
		}
		// A disjunction is the shape that cannot hold a bare search function.
		// The optimiser pushes the search into the index scan whatever boolean
		// surrounds it, so when the search matches nothing the scan is pruned
		// to nothing and the OTHER disjunct is never evaluated — the whole
		// predicate returns zero rows. Measured, `query('line:zzzznosuchtoken')
		// OR 1=1` is 0 against a table of 1,063,259. It has to go in a
		// subquery.
		var wrapped string
		var err error
		if op == "OR" {
			wrapped, err = c.semiJoin(merged)
		} else {
			wrapped, err = c.wrapText(merged)
		}
		if err != nil {
			return fragment{}, err
		}
		parts[textSlot] = wrapped
	}

	// No count to accumulate: the assembled fragment's SQL carries whatever its
	// pieces put there, and outerSearch() reads it back off the string.
	return fragment{sql: "(" + strings.Join(parts, " "+op+" ") + ")"}, nil
}

// textBoolean combines full-text fragments inside the query() mini-language.
//
// Its shape is dictated by three measured behaviours, none of which are
// documented and all of which fail silently rather than erroring:
//
//	(a) AND NOT (b)          -> 0 rows.  `AND NOT` is broken in every spelling.
//	(a) NOT (b) AND (c)      -> ignores everything after the first NOT.
//	(a) OR NOT (b)           -> silently drops the negative clause entirely.
//
// The forms that do work, cross-checked against equivalent LIKE predicates:
//
//	(a) AND (b) NOT (c)      -> 72,603, matches LIKE exactly
//	(a) NOT (b) NOT (c)      -> 14,027, matches LIKE exactly
//
// So negatives must be bare (never `AND NOT`), must come last, and cannot
// appear under OR at all.
func (c *compiler) textBoolean(frags []fragment, op string) (fragment, error) {
	var positives, negatives, residuals []string
	for _, f := range frags {
		if f.negated {
			negatives = append(negatives, "("+f.text+")")
		} else {
			positives = append(positives, "("+f.text+")")
		}
		if f.residual != "" {
			// Only reachable on the AND path: not() degrades a negated
			// residual and boolean() degrades an ORed one, so by the time a
			// residual arrives here every fragment in the group is ANDed and
			// the residuals conjoin with them.
			residuals = append(residuals, f.residual)
		}
	}

	// All children negated: De Morgan turns this into a single positive query
	// wrapped in one SQL NOT, which is both legal and correct —
	// `NOT a AND NOT b` is `NOT (a OR b)`.
	if len(positives) == 0 {
		inner := "OR"
		if op == "OR" {
			inner = "AND" // NOT a OR NOT b  ==  NOT (a AND b)
		}
		return fragment{text: strings.Join(negatives, " "+inner+" "), negated: true}, nil
	}

	if len(negatives) > 0 && op == "OR" {
		// `(a) OR NOT (b)` really does drop the negative clause silently, so
		// the shape cannot be emitted as written — but it can be rewritten.
		// De Morgan folds an OR-with-negatives into the one clause shape this
		// engine evaluates correctly, positives ANDed and negatives bare and
		// trailing, under a single SQL NOT:
		//
		//	p1 OR p2 OR NOT n1 OR NOT n2
		//	 == NOT( n1 AND n2 AND NOT(p1 OR p2) )
		//	 -> NOT( query('(n1) AND (n2) NOT ((p1) OR (p2))') )
		//
		// Still one search function. Measured: `peer OR -status` becomes
		// NOT(query('(msg:status) NOT ((msg:peer))')) = 440,181 = 449,893 total
		// minus the 9,712 rows matching `(status) NOT (peer)`.
		//
		// The result stays a text fragment rather than being spent as SQL, so
		// it composes further. That is safe because a NOT nested inside a
		// negative group is evaluated correctly here — verified, not assumed:
		// query('(region) NOT ((peer) NOT ((store)))') returns 15,634, exactly
		// 20,144 - 4,853 + 343.
		expr := strings.Join(negatives, " AND ")
		expr += " NOT (" + strings.Join(positives, " OR ") + ")"
		return fragment{text: expr, negated: true}, nil
	}

	expr := strings.Join(positives, " "+op+" ")
	for _, n := range negatives {
		// Deliberately no operator: `AND NOT` returns zero rows.
		expr += " NOT " + n
	}
	return fragment{text: expr, residual: strings.Join(residuals, " AND ")}, nil
}

func (c *compiler) term(t *parser.Term) (fragment, error) {
	name := t.Field
	if name == "" {
		name = c.schema.Default
	}
	f, known := c.schema.Lookup(name)
	if f.Column == "" {
		return fragment{}, fmt.Errorf(
			"unknown field %q and no VARIANT column configured: this schema declares no "+
				"attribute bag, so a name it does not list cannot be read from anywhere", name)
	}
	if !known && t.Field != "" {
		c.warn(variantWarning(f), t.Field, c.bagOf(t.Field))
	}

	c.noteConversion(f, name)

	if t.Exists {
		return fragment{sql: c.exists(f)}, nil
	}

	if t.Regex {
		return c.regexTerm(f, t, name)
	}

	if isBooleanWord(t.Value) {
		// `and`, `or` and `not` are ordinary English words that occur in log
		// lines, and every one of them used to reach the parser as an operator
		// and then evaporate: `msg:(not)` and a bare `not` compiled to `1=1`,
		// the whole table, with nothing said. They are values here, and saying
		// so is the difference between "the tool searched for the word" and
		// "the tool decided the query was empty".
		//
		// The text has to state the whole rule, because the rule is
		// context-sensitive and the same word is an operator two characters
		// away. An earlier version said "only the uppercase spellings AND, OR
		// and NOT are operators" — false in the very statement it was printed
		// beside, which applied `&&`, and false again once lowercase `or` went
		// back to joining terms.
		c.warn("%q is searched for as a word here, not applied as a boolean operator. A boolean "+
			"word is an operator only where an operator is grammatical: and, or, && and || "+
			"between two terms, NOT before a term. NOT must be capitalised because it INVERTS "+
			"the term it takes, while and/or only join terms that keep their own meaning "+
			"either way. As a field's value (msg:not), inside quotes, or anywhere the position "+
			"is not grammatical, it is a value", t.Value)
	}

	if t.Boost != "" && f.Kind != Text {
		return fragment{}, fmt.Errorf(
			"boost ^%s needs a full-text field: %q is compared, not scored, so there is no "+
				"relevance for a weight to move", t.Boost, name)
	}

	switch f.Kind {
	case Text:
		return c.textTerm(f, t)
	case Number:
		if t.Wildcard || t.Fuzz > 0 {
			c.warn("wildcards and fuzziness are not meaningful on numeric field %q; comparing for equality", name)
		}
		if _, err := strconv.ParseFloat(t.Value, 64); err != nil {
			return fragment{}, fmt.Errorf("field %q is numeric but %q is not a number", name, t.Value)
		}
		return fragment{sql: c.numericColumn(f, name) + " = " + t.Value}, nil
	case Timestamp:
		// The remediation has to be a spelling that survives the lexer: a
		// quoted literal with a space in it does not, because the space ends
		// the word and the quotes end up inside the SQL literal.
		return fragment{}, fmt.Errorf(
			"field %q is a timestamp, so it needs a comparison rather than a value: "+
				"%s:>2026-08-18T22:30:00Z, or a two-sided %s:[2026-08-18 TO 2026-08-19]", name, name, name)
	default:
		return c.stringTerm(f, t)
	}
}

// isBooleanWord reports whether a value spells one of the boolean operators,
// in any case. It decides only whether to explain, never what to emit.
func isBooleanWord(v string) bool {
	switch {
	case strings.EqualFold(v, "and"), strings.EqualFold(v, "or"), strings.EqualFold(v, "not"):
		return true
	case v == "&&", v == "||":
		return true
	}
	return false
}

// textTerm produces either a text fragment (merged into the single query()
// call) or a SQL fragment (wildcards, which LIKE handles and which cost no
// search function).
func (c *compiler) textTerm(f Field, t *parser.Term) (fragment, error) {
	if t.Phrase && strings.Contains(t.Value, "*") {
		// Quotes turn a wildcard off, and the way they turn it off is worth
		// spelling out because the result is a silent zero rather than a
		// literal match. The analyzer treats `*` as punctuation, so it SPLITS
		// the phrase there and each fragment becomes a token of its own.
		// Measured over ts < '2026-08-19 00:00:00' and again over
		// [2026-08-19 00:00, 05:00):
		//
		//	query('msg:"peer status"')   88,441   38,076
		//	query('msg:"peer* status"')  88,441   38,076   <- star dropped, phrase intact
		//	query('msg:"peer stat*"')         0        0   <- now the phrase "peer stat"
		//	query('msg:"peer stat"')          0        0   <- and `stat` is not a token
		//	query('msg:"peer stat*us"')       0        0   <- three tokens now
		//
		// A `?` is punctuation too and is harmless for the same reason:
		// query('msg:"peer status?"') is 88,441, identical to the clean
		// phrase, so it raises nothing.
		c.warn("`*` inside quotes is not a wildcard: the analyzer reads it as punctuation and "+
			"splits %q there, so the phrase becomes the tokens on either side of the star and "+
			"a fragment that is not a whole token in the index makes the phrase match nothing "+
			"at all, silently. Drop the quotes if a wildcard is what was meant", t.Value)
	}

	switch {
	case t.Wildcard:
		// A wildcard cannot be forwarded into query(): the engine does not
		// ignore the star, it *truncates the term at it*, which is worse
		// because the answer is plausible rather than empty. Measured,
		// frozen window: query('msg:"reg*on"') returns 36 rows, none of
		// which contain "region" — they are `/registration/ebs.csi...`
		// lines matching the truncated token `reg` — against 20,144 for the
		// token itself.
		//
		// # Which question a wildcard asks
		//
		// `region*` means "a TOKEN beginning with region", and on a tokenised
		// column that is not the same request as "the line contains region
		// somewhere". The difference is not academic. Emitted as
		// LIKE '%reg%on%', `reg*on` is exactly RLIKE 'reg.*on' — measured,
		// both 21,278 over the frozen window — so the star runs across word
		// boundaries and over whole lines: 4,196 of those rows contain no word
		// matching reg…on at all (1,628 over a second, disjoint window), and
		// the token set is a strict subset of the substring one, 0 rows the
		// other way. It also collapses the syntax, because
		// `*region`, `region*` and `*region*` all become '%region%' and a
		// suffix wildcard stops being distinguishable from a prefix one.
		//
		// So a pattern that can describe one token is compiled as one token:
		// a word-boundary regex, the same device the stopword branch uses and
		// the same one the conformance suite trusts as ground truth. Measured
		// over the frozen window, `reg*on` is 17,082 that way against 21,278
		// as a substring, `*region` is 16,886 against `region*`'s 20,147, and
		// `to*` is 136,162 against 177,913.
		//
		// A pattern that *cannot* describe one token — one containing any
		// character the tokenizer splits on, like `*0.0.0.0:8686/playground*`
		// or `tikv-tikv-*` — is not a token wildcard in the first place, and
		// asking for word boundaries around it would be meaningless. Those
		// keep the substring reading, and the warning says which one was used.
		//
		// Neither is a search function, so both compose freely with the one
		// query() call the statement is allowed.
		if t.Fuzz > 0 {
			c.warn("fuzziness is ignored when a wildcard is present on %q", f.Column)
		}
		if f.Kind == Text && tokenWildcard(t.Value) {
			pat := c.tokenPattern(f, t.Value)
			c.warn("wildcard %q on the full-text column %q is matched as ONE token, with the "+
				"word-boundary regex %s: `*` and `?` stop at the token boundary, so it will not "+
				"span words the way a substring match does, and it does not stem — the bare "+
				"token finds inflections that the pattern does not. RLIKE is served by neither "+
				"the inverted nor the NGRAM index, so this is a full scan",
				t.Value, f.Column, pat)
			return fragment{sql: c.wordBoundaryPattern(f, pat)}, nil
		}
		sql := c.like(f, t.Value)
		if f.Kind == Text {
			c.warn("wildcard %q on the full-text column %q contains characters the tokenizer "+
				"splits on, so it cannot describe a single token: it is matched as an "+
				"UNANCHORED SUBSTRING — %s — which crosses word boundaries and can span the "+
				"whole line. It does not stem either", t.Value, f.Column, sql)
		}
		if !f.Ngram {
			c.warn("wildcard on %q falls back to LIKE; without an NGRAM index this is a full scan "+
				"(CREATE NGRAM INDEX ... ON <table>(%s))", f.Column, f.Column)
		}
		return fragment{sql: sql}, nil

	case f.IsStopWord(t.Value):
		// The index carries `filters = 'english_stop'`, and the filter runs
		// over the *query* as well as the document: the word is deleted
		// before the index is consulted, so the clause matches nothing and
		// nothing is raised. Measured over the frozen window, all 33 words in
		// the set return 0 through query() — `to` 0 against a true 130,002,
		// `not` 0 against 22,850, `no` 0 against 5,060.
		//
		// A word-boundary scan is the honest substitute. It is not a search
		// function, so it costs nothing against the one-call budget and
		// composes with a positive query() exactly: measured,
		// query('msg:replica') = 1,743, the scan for `no` = 5,060, their AND
		// = 1,656 and their OR = 5,147 = 1,743 + 5,060 - 1,656.
		//
		// Rejecting the word instead would be the worse answer: 5,060 rows
		// contain "no", and telling someone it cannot be searched for is a
		// lie where telling them it is slow is merely unwelcome.
		if t.Fuzz > 0 || t.Boost != "" || t.Slop > 0 {
			c.warn("fuzziness, boost and proximity all reach the engine through the search "+
				"function, which never sees the stopword %q; matching it literally instead",
				t.Value)
		}
		c.warn("%q is a stopword of the index on %q: the analyzer deletes it from the query "+
			"before the index is read, so a token search for it returns zero rows and no error. "+
			"Matching it with a word-boundary scan instead — exact, but a full scan, and it does "+
			"not stem the way the index would", t.Value, f.Column)
		return fragment{sql: c.wordBoundary(f, t.Value)}, nil

	case t.Fuzz > 0:
		// Fuzziness exists only as an option argument to match(); inside
		// query() the Lucene `~N` form returns zero rows silently. That makes
		// a fuzzy term a search function in its own right, so it cannot share
		// a statement with any other full-text term. It does compose with
		// structured filters and LIKE, both verified.
		if t.Boost != "" {
			c.warn("boost ^%s is not expressible on a fuzzy term: fuzziness reaches the engine "+
				"through match()'s option argument, which has no weight of its own", t.Boost)
		}
		return fragment{sql: fmt.Sprintf("match(%s, '%s', 'fuzziness=%d')",
			f.Column, escapeString(t.Value), t.Fuzz)}, nil

	case t.Phrase && analyzerCollapses(f, t.Value):
		// A phrase is a constraint on *positions*, and positions only exist
		// between tokens. The analyzer strips punctuation and stopwords from
		// the phrase before the index sees it, so a phrase the analyzer cuts
		// down to fewer than two tokens has no adjacency left to check — and
		// rather than say so, the engine drops the phrase constraint and
		// matches the survivor everywhere. Measured, frozen window
		// ts < '2026-08-19 00:00:00':
		//
		//	query('msg:"not ready"')          2,320
		//	query('msg:ready')                2,320   <- identical
		//	lower(msg) LIKE '%not ready%'       250   <- the truth
		//	msg RLIKE 'not ready'               250   <- independent oracle
		//	query('msg:"the leader"')         9,716 against a true 4
		//
		// # What this branch must NOT fire on
		//
		// The test is whether the analyzer *destroyed* something, not how many
		// tokens are left. A one-word phrase that survives the analyzer intact
		// — `"snapshots"` — lost nothing, and its index path is exactly right:
		// query('msg:"snapshots"') is 17,595 rows over the frozen window
		// because the index stems, where lower(msg) LIKE '%snapshots%' is 9.
		// Routing it here was a 1,955x under-match, and `""` — which destroys
		// nothing because there was nothing there — became `LIKE '%%'`, the
		// whole table, against a correct 0. So the guard compares the phrase
		// against the tokens that survive it: equal means untouched, and
		// untouched phrases keep the index.
		//
		// # The two survivors
		//
		// One surviving token: the token goes into query() so the index and
		// its stemming still do the work, and a literal scan for the whole
		// phrase rides along as a residual so the adjacency the quotes asked
		// for is actually checked. The residual is carried on the fragment
		// rather than spent here, which is what keeps `"not ready" peer` a
		// working one-search-function query instead of a compile error.
		//
		// Zero surviving tokens — `"not the"`, `"[!]"` — leave nothing for the
		// index to match, so the scan is the whole answer.
		//
		// Where either differs from a true phrase is whitespace and case
		// folding: the scan wants exactly the characters typed, so a doubled
		// space will not match and "cannot ready" will. The warning says so.
		toks := phraseTokens(f, t.Value)
		if t.Slop > 0 {
			c.warn("proximity ~%d is dropped on %q: after the analyzer removes stopwords and "+
				"punctuation this phrase keeps fewer than two tokens, so there are no positions "+
				"left to measure a distance between", t.Slop, t.Value)
		}
		if t.Boost != "" {
			c.warn("boost ^%s is dropped on %q, which is matched by scan rather than by the "+
				"index and therefore has no relevance score to weight", t.Boost, t.Value)
		}
		if !f.Ngram {
			c.warn("phrase %q falls back to LIKE; without an NGRAM index this is a full scan "+
				"(CREATE NGRAM INDEX ... ON <table>(%s))", t.Value, f.Column)
		}
		like := c.contains(f, t.Value)
		if len(toks) == 1 {
			c.warn("phrase %q is cut down to the single token %q by the analyzer on %q — the "+
				"rest is stopwords or punctuation, which the index does not store — and a "+
				"one-token phrase has no positions left to constrain, so a phrase query for it "+
				"silently matches that token everywhere. Compiling it as the token AND a literal "+
				"scan for the whole phrase instead: the scan is exact about spacing and case "+
				"where a phrase query is not", t.Value, toks[0], f.Column)
			c.noteText(f)
			return fragment{
				text:     f.Column + ":" + quoteQueryValue(toks[0], false),
				residual: like,
				plain:    like,
			}, nil
		}
		c.warn("phrase %q keeps no tokens at all after the analyzer on %q removes stopwords and "+
			"punctuation, so there is nothing left for the index to match. Matching the literal "+
			"text with a scan instead", t.Value, f.Column)
		return fragment{sql: like}, nil

	case t.Phrase:
		// Positions are stored: "peer status" returns 72,601 where the
		// reversed phrase returns 0.
		//
		// Proximity is real here and N is honoured, which is worth stating
		// because it is easy to measure wrongly — sample only the exact phrase
		// and a large N and both land on plateaus. The full ladder, frozen:
		//
		//	"region peer"      654    "peer status"       88,441
		//	"region peer"~0    654    "status peer"            0
		//	"region peer"~1    654    "status peer"~1          0
		//	"region peer"~2  4,593    "status peer"~2     88,441
		//	"region peer"~3  4,593    "status peer"~5     88,441
		//	"region peer"~10 4,853
		//	region AND peer  4,853
		//
		// Strictly monotone, converging on the unordered AND, and the reversed
		// phrase first matches at exactly ~2 — a transposition costs two,
		// which is textbook Lucene.
		c.noteText(f)
		return fragment{text: c.boost(f.Column+":"+quoteQueryValue(t.Value, true)+slopSuffix(t), t)}, nil

	default:
		c.noteText(f)
		return fragment{text: c.boost(f.Column+":"+quoteQueryValue(t.Value, false), t)}, nil
	}
}

// slopSuffix renders a phrase's proximity marker. `~0` is left off: it asks
// for exactly the ordering an unadorned phrase already has, and the two were
// measured to return the same rows.
func slopSuffix(t *parser.Term) string {
	if t.Slop <= 0 {
		return ""
	}
	return "~" + strconv.Itoa(t.Slop)
}

// boost appends a relevance weight to a query()-language clause.
//
// It is a ranking device, not a filter: measured, `(msg:region)^5 OR (msg:peer)`
// returns the same 125,241 rows as the unweighted disjunction, and only the
// score() ordering moves. A search that never selects score() is therefore
// unaffected by it, which is worth saying out loud rather than letting someone
// conclude the boost did nothing.
func (c *compiler) boost(clause string, t *parser.Term) string {
	if t.Boost == "" {
		return clause
	}
	c.warn("boost ^%s only reorders score(); the set of matching rows is unchanged, "+
		"so it does nothing in a panel that does not select score()", t.Boost)
	return "(" + clause + ")^" + t.Boost
}

// regexTerm compiles `/pattern/` to RLIKE.
//
// RLIKE is not a search function, so a regex composes freely with the one
// query() call — but no index serves it, so it is a full scan and says so. Note
// that RLIKE is case-insensitive on this engine whatever the schema's
// CaseInsensitive setting says (measured: `msg RLIKE 'PEER.*STATUS'` and
// `msg RLIKE 'peer.*status'` both return 88,451, while `msg LIKE '%PEER%'`
// returns 0 against 109,878 for the lowercase pattern). A pattern that must be
// case-sensitive has to say so itself with `(?-i)`, which works.
func (c *compiler) regexTerm(f Field, t *parser.Term, name string) (fragment, error) {
	switch f.Kind {
	case Number, Timestamp:
		return fragment{}, fmt.Errorf(
			"field %q is not text; a regex term has nothing to match against", name)
	}
	if t.Fuzz > 0 || t.Boost != "" {
		c.warn("fuzziness and boost are not meaningful on a regex term; ignoring them on %q", f.Column)
	}
	c.warn("regex on %q compiles to RLIKE, which neither the inverted nor the NGRAM index "+
		"serves: this is a full scan", f.Column)
	return fragment{sql: f.Column + " RLIKE '" + escapeString(t.Value) + "'"}, nil
}

// phraseTokens returns the tokens of a phrase that survive the column's
// analyzer: the text split on everything that is not a letter or a digit,
// with the index's stopwords removed.
//
// It is an approximation of the engine's tokenizer, and it only has to be
// right about the *count*, because the count is the whole decision: two or
// more surviving tokens and the phrase works as written, fewer and it has to
// be compiled some other way. Erring towards fewer tokens — which is what
// splitting on punctuation does — errs towards the scan, which is correct but
// slower, never towards the silent over-match.
func phraseTokens(f Field, phrase string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(phrase, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if f.IsStopWord(tok) {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// analyzerCollapses reports whether the column's analyzer destroys part of a
// phrase — the question that decides whether the phrase can keep its index
// path.
//
// It is deliberately not "how many tokens survive". `"snapshots"` survives with
// one token and lost nothing: the index stems it, query('msg:"snapshots"')
// finds 17,595 rows over the frozen window, and the substring scan the
// token-count test routed it to finds 9. `""` survives with none and also lost
// nothing. What matters is whether the tokens the index will see still spell
// the phrase that was typed: if they do, the phrase is a phrase and the index
// answers it correctly; if they do not, the engine has silently dropped a
// constraint and something else has to check it.
//
// Two or more surviving tokens always keep the index, because positions between
// them are stored and the phrase works as written — that is the case
// `"fail to get peer"` exercises, where the stopword gap is bridged by
// position.
func analyzerCollapses(f Field, phrase string) bool {
	toks := phraseTokens(f, phrase)
	if len(toks) >= 2 {
		return false
	}
	return strings.Join(toks, " ") != phrase
}

// contains matches a literal substring. Unlike like() it does no wildcard
// translation: this renders text the user quoted, where a `*` is a star.
func (c *compiler) contains(f Field, value string) string {
	lit := "'" + escapeString("%"+escapeLike(value)+"%") + "'"
	if c.schema.CaseInsensitive {
		return fmt.Sprintf("lower(%s) LIKE lower(%s)", f.Column, lit)
	}
	return fmt.Sprintf("%s LIKE %s", f.Column, lit)
}

// wordBoundary matches one word as a token would be matched, without the
// index: the word surrounded by anything that is not a word character.
//
// It is the substitute for a term the analyzer would delete, and the reason to
// trust it is a control rather than an argument. For `from`, which is *not* a
// stopword, the index and this regex agree exactly — query('msg:from') and
// lower(msg) RLIKE '(^|[^a-z0-9])from([^a-z0-9]|$)' both return 93,645 over
// the frozen window. So the form is the right ground truth for a token search,
// and it is also the guard that this branch is not firing on ordinary words.
//
// RLIKE is case-insensitive on this engine whatever the schema says — measured
// again here, the lower() and bare spellings return the same 130,002 for `to`
// — but the lower() is written out anyway when the schema asks for it, so the
// predicate does not depend on an undocumented engine default.
func (c *compiler) wordBoundary(f Field, word string) string {
	return c.wordBoundaryPattern(f, "(^|[^a-z0-9])"+escapeString(strings.ToLower(word))+"([^a-z0-9]|$)")
}

// wordBoundaryPattern renders an already-built token regex against the column.
// Split out of wordBoundary so the stopword branch and the wildcard branch
// spell the boundary the same way and cannot drift apart — the conformance
// suite treats this exact form as ground truth for what a token search means.
func (c *compiler) wordBoundaryPattern(f Field, pat string) string {
	if c.schema.CaseInsensitive {
		return fmt.Sprintf("lower(%s) RLIKE '%s'", f.Column, pat)
	}
	return fmt.Sprintf("%s RLIKE '%s'", f.Column, pat)
}

// tokenWildcard reports whether a wildcard value can describe a single
// analyzer token: every literal character in it is a token character.
//
// The 'english' tokenizer breaks on everything that is not a letter or a
// digit, which is the same rule wordBoundary's `[^a-z0-9]` encodes. So
// `region*` and `reg*on` are token patterns and `tikv-tikv-*`,
// `*0.0.0.0:8686/playground*` and `100%*` are not — the last three describe a
// run of text that spans several tokens, and a token reading of them would
// answer a question nobody asked.
func tokenWildcard(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r == '*' || r == '?':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// tokenPattern renders a token wildcard as a word-boundary regex.
//
//	region*   ->  (^|[^a-z0-9])region[a-z0-9]*([^a-z0-9]|$)
//	*region   ->  (^|[^a-z0-9])[a-z0-9]*region([^a-z0-9]|$)
//	reg*on    ->  (^|[^a-z0-9])reg[a-z0-9]*on([^a-z0-9]|$)
//	regio?    ->  (^|[^a-z0-9])regio[a-z0-9]([^a-z0-9]|$)
//
// `*` becomes "any run of token characters" and `?` exactly one, so both stop
// where the token stops. No regex escaping is needed and none is done:
// tokenWildcard has already established that every literal character is
// alphanumeric, which is the guard that makes this safe — call one without the
// other and a `.` or a `(` in the value would be live regex.
func (c *compiler) tokenPattern(f Field, value string) string {
	var b strings.Builder
	b.WriteString("(^|[^a-z0-9])")
	for _, r := range strings.ToLower(value) {
		switch r {
		case '*':
			b.WriteString("[a-z0-9]*")
		case '?':
			b.WriteString("[a-z0-9]")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("([^a-z0-9]|$)")
	return b.String()
}

func (c *compiler) stringTerm(f Field, t *parser.Term) (fragment, error) {
	if t.Fuzz > 0 {
		c.warn("fuzziness needs an inverted index; %q is a plain column, matching exactly instead", f.Column)
	}
	if t.Slop > 0 {
		c.warn("phrase proximity needs an inverted index to have positions to measure; %q is a "+
			"plain column, matching the phrase exactly instead", f.Column)
	}
	if t.Wildcard {
		return fragment{sql: c.like(f, t.Value)}, nil
	}

	lit := "'" + escapeString(t.Value) + "'"
	eq := f.Column + " = " + lit
	if c.schema.CaseInsensitive {
		// There is no case-insensitive `=` on this engine — ILIKE exists, but
		// it is a LIKE, so it would turn an equality into a pattern match —
		// which is why case-insensitivity is spelled out with lower() on both
		// sides rather than borrowed from an operator.
		eq = fmt.Sprintf("lower(%s) = lower(%s)", f.Column, lit)
	}
	if frag, ok := c.indexedEquality(f, t.Value, eq); ok {
		return frag, nil
	}
	return fragment{sql: eq}, nil
}

// indexedEquality adds the index to an equality on a VARIANT key, without
// changing what the equality means.
//
// An inverted index covers a VARIANT column by JSON path, so `kv['err'] = 'x'`
// — a full scan today — can be pruned by `query('kv.err:x')` first. The catch
// is that the two are not the same question: the equality is exact and the
// index is tokenised and stemmed, so the index is strictly wider. Measured over
// a 967,914-row copy indexed on (msg, line, kv):
//
//	                        query()   equality
//	kv.err:RemoteStopped        507        507   same question, by luck
//	kv.request:command          501          0   `batch_commands` stems to it
//
// So the index cannot replace the equality — 501 invented rows on the second
// line — but it can precede it. The pair compiles to the index fragment plus
// the equality as a residual, which is the same mechanism a collapsed phrase
// already uses, and measures identically to the equality alone: 507 and 0.
//
// Two shapes are excluded because the index would answer nothing and the AND
// would inherit that:
//
//   - a value the index deletes. A row written with kv = {"verb":"the"} is
//     returned by the equality (1) and not by query('kv.verb:the') (0), so a
//     stopword value has to skip the index entirely.
//   - a value with no token in it at all, which the analyzer removes the same
//     way.
//
// It also requires the key to be spellable in the query language and the bag
// to actually be indexed, which is what keeps this dormant on a table whose
// VARIANT column is not in the index group.
func (c *compiler) indexedEquality(f Field, value, eq string) (fragment, bool) {
	if f.Search == "" || !indexCanSee(f, value) {
		return fragment{}, false
	}
	c.noteText(f)
	return fragment{
		text:     f.Search + ":" + quoteQueryValue(value, false),
		residual: eq,
		plain:    eq,
	}, true
}

// indexCanSee reports whether the analyzer would leave this value anything for
// the index to match.
//
// It tests every token, not the whole value, and that distinction was a defect.
// The first version asked `f.IsStopWord(value)`, which compares the value
// against the 33-word stop set as a single word — so a value that is not itself
// one stopword but whose every *token* is one sailed straight past the guard
// into an index clause that could never match, and the AND with the equality
// inherited the emptiness. Measured on a probe table indexed over (msg, kv),
// one row per value:
//
//	value            equality   index   emitted, before   after
//	"the"                   1       0                 1       1   caught either way
//	"to be"                 1       0                 0       1   the defect
//	"a an"                  1       0                 0       1   the defect
//	"the end"               1       1                 1       1   `end` survives
//	"RemoteStopped"         1       1                 1       1
//
// `verb:"to be"` returned zero rows for a row that exists — the same silent
// empty the guard was written to prevent, one token wider.
//
// A value with no token at all is the same case with nothing surviving, so the
// two conditions collapse into one: if nothing survives the analyzer, do not
// reach for the index. Tokenisation here is deliberately crude — runs of
// alphanumerics — because the only decision it makes is whether to *add* an
// index clause beside an equality that already answers correctly alone. Being
// conservative costs a scan; being wrong returns nothing.
func indexCanSee(f Field, v string) bool {
	surviving := 0
	for _, tok := range tokenize(v) {
		if !f.IsStopWord(tok) {
			surviving++
		}
	}
	return surviving > 0
}

// tokenize splits a value into runs of alphanumerics, which is close enough to
// what the `english` tokenizer keeps for the one decision it informs.
func tokenize(v string) []string {
	var out []string
	start := -1
	for i, r := range v {
		alnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		switch {
		case alnum && start < 0:
			start = i
		case !alnum && start >= 0:
			out = append(out, v[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, v[start:])
	}
	return out
}

// like renders a wildcard value as a LIKE predicate.
//
// # Anchoring depends on what the column is
//
// A LIKE pattern is anchored at both ends: `'region%'` means "the *value*
// starts with region". On a plain column that is exactly what `pod:tikv*`
// asks for, and it is right. On the full-text column it is a different
// question from the one that was typed — `region*` means "a *token* starting
// with region, anywhere in the line", and a whole log message practically
// never starts with the word being searched for. Measured, frozen window:
//
//	lower(msg) LIKE lower('region%')     5,133   anchored at character 0
//	query('msg:region')                 20,144   the token search
//	lower(msg) LIKE lower('%region%')   20,158   unanchored
//
// So 74.5% of the answer was missing, and every row that came back was
// correct — which is precisely why a "returns rows" assertion could not see
// it. For a Text field both ends are therefore opened unless the user's own
// pattern already opens them; every other kind stays anchored.
func (c *compiler) like(f Field, value string) string {
	if !f.Ngram {
		// A LIKE is served by the NGRAM index or by nothing, and anchoring does
		// not change that: without an ngram index every wildcard on this column
		// is a full scan. It was silent until now, which mattered most on the
		// column added last — `raw` holds the whole original log line, is not in
		// any index, and `raw:*hello*` is the only useful way to search it, so
		// the one thing a user needs to be told is that it costs a scan.
		c.warn("wildcard %q on %q compiles to LIKE, and no NGRAM index covers that column, so "+
			"this is a full scan (CREATE NGRAM INDEX ... ON <table>(%s))",
			value, f.Column, f.Column)
	}
	pat := likePattern(value)
	if f.Kind == Text {
		if !strings.HasPrefix(value, "*") {
			pat = "%" + pat
		}
		if !strings.HasSuffix(value, "*") {
			pat = pat + "%"
		}
	}
	lit := "'" + escapeString(pat) + "'"
	if c.schema.CaseInsensitive {
		return fmt.Sprintf("lower(%s) LIKE lower(%s)", f.Column, lit)
	}
	return fmt.Sprintf("%s LIKE %s", f.Column, lit)
}

// exists compiles `field:*`.
//
// On a real column the useful reading of "has a value" excludes the empty
// string: an empty `level` is not a level. On a VARIANT key it is the opposite,
// and the difference is large. The `[k=v]` extractor writes an empty value
// whenever a log line says `key=` with nothing after it, so `<> ”` throws away
// exactly the rows that prove the key is there. Re-measured on
// logs.k8s_logs_v2 (967,912 rows, ts < 2026-08-19 22:19:00), because the
// store_id line below used to read 1,376 — a number that belongs to a different
// quantity and a smaller window:
//
//	kv['rest'] IS NOT NULL              433,901
//	kv['rest']::VARCHAR IS NOT NULL     433,901
//	kv['rest']::VARCHAR <> ''           333,266   <- 100,635 rows denied
//	kv['store_id'] IS NOT NULL           40,516   <- control, no empty values
//	kv['store_id']::VARCHAR <> ''        40,516      both readings agree
//
// The cast is innocent on this table — the two IS NOT NULL forms agree to the
// row, because no bag value here is a JSON null — but it is dropped anyway,
// because a JSON null *is* distinguishable from a missing key before the cast
// and not after it, and existence is precisely the question that distinction
// answers.
func (c *compiler) exists(f Field) string {
	if f.Presence != "" {
		return f.Presence + " IS NOT NULL"
	}
	switch f.Kind {
	case Number, Timestamp:
		return f.Column + " IS NOT NULL"
	default:
		return "(" + f.Column + " IS NOT NULL AND " + f.Column + " <> '')"
	}
}

func (c *compiler) rangeNode(r *parser.Range) (fragment, error) {
	f, err := c.rangeField(r.Field)
	if err != nil {
		return fragment{}, err
	}
	col, lits, err := c.bounds(f, r.Field, r.Value)
	if err != nil {
		return fragment{}, err
	}
	return fragment{sql: col + " " + r.Op + " " + lits[0]}, nil
}

// betweenNode compiles the bracket range forms.
//
// These are ordinary SQL comparisons, never the search mini-language: that has
// a range form of its own, but it is inverted-index-only and single-token-only
// — `query('msg:[a TO b]')` fails with `[1903] Unsupported query: Range query
// boundary cannot have multiple tokens` — so routing a bracket range through it
// would trade a working predicate for an error. As plain SQL it also costs no
// search function and composes with one freely.
func (c *compiler) betweenNode(b *parser.Between) (fragment, error) {
	f, err := c.rangeField(b.Field)
	if err != nil {
		return fragment{}, err
	}

	// `[* TO *]` asks only that the field have a value.
	if b.Lo == "" && b.Hi == "" {
		return fragment{sql: c.exists(f)}, nil
	}

	var vals []string
	if b.Lo != "" {
		vals = append(vals, b.Lo)
	}
	if b.Hi != "" {
		vals = append(vals, b.Hi)
	}
	col, lits, err := c.bounds(f, b.Field, vals...)
	if err != nil {
		return fragment{}, err
	}

	if b.Lo != "" && b.Hi != "" && b.LoIncl && b.HiIncl {
		return fragment{sql: col + " BETWEEN " + lits[0] + " AND " + lits[1]}, nil
	}

	var parts []string
	i := 0
	if b.Lo != "" {
		op := ">"
		if b.LoIncl {
			op = ">="
		}
		parts = append(parts, col+" "+op+" "+lits[i])
		i++
	}
	if b.Hi != "" {
		op := "<"
		if b.HiIncl {
			op = "<="
		}
		parts = append(parts, col+" "+op+" "+lits[i])
	}
	if len(parts) == 1 {
		return fragment{sql: parts[0]}, nil
	}
	return fragment{sql: "(" + strings.Join(parts, " AND ") + ")"}, nil
}

// rangeField resolves the field of any comparison and rejects the one kind
// that cannot carry one.
func (c *compiler) rangeField(name string) (Field, error) {
	f, known := c.schema.Lookup(name)
	if f.Column == "" {
		return Field{}, fmt.Errorf(
			"unknown field %q and no VARIANT column configured: this schema declares no "+
				"attribute bag, so a name it does not list cannot be read from anywhere", name)
	}
	if !known {
		c.warn(variantWarning(f), name, c.bagOf(name))
	}
	if f.Kind == Text {
		return Field{}, fmt.Errorf("field %q is full-text indexed; ranges are not meaningful on it", name)
	}
	c.noteConversion(f, name)
	return f, nil
}

// bounds renders the column expression and the literals of a comparison.
//
// Whether the bounds are numeric is decided once for the whole predicate rather
// than per bound, so a two-sided range cannot end up comparing one side as a
// number and the other as a string.
func (c *compiler) bounds(f Field, name string, vals ...string) (col string, lits []string, err error) {
	numeric := true
	for _, v := range vals {
		if _, e := strconv.ParseFloat(v, 64); e != nil {
			numeric = false
		}
	}

	switch f.Kind {
	case Number:
		if !numeric {
			for _, v := range vals {
				if _, e := strconv.ParseFloat(v, 64); e != nil {
					return "", nil, fmt.Errorf("field %q is numeric but %q is not a number", name, v)
				}
			}
		}
		return c.numericColumn(f, name), vals, nil

	case Timestamp:
		// The one place in this compiler where a loud error is the right
		// answer. Databend accepts a truncated literal and resolves it to the
		// top of whatever unit was left off: measured, `ts > '2026-08-18T22'`
		// returns 129,009 rows, exactly what `ts > '2026-08-18 22:00:00'`
		// returns, where the 22:30 the user was typing is 70,681. The window
		// silently widens by 58,328 rows and nothing says so. There is no safe
		// guess between "they meant 22:00" and "the lexer ate the rest", so
		// refuse rather than choose.
		for i, v := range vals {
			if !completeInstant(v) {
				return "", nil, fmt.Errorf(
					"a timestamp bound must be a complete instant, and %q is not: this engine "+
						"accepts it and reads it as the top of the unit, which silently widens the "+
						"window. Write the whole value, for example %s:>2026-08-18T22:30:00Z or "+
						"%s:>\"2026-08-18 22:30:00\"", v, name, name)
			}
			vals[i] = c.pinZone(v)
		}
		return f.Column, quoteAll(vals), nil

	default:
		// A string-valued expression compared against a number has to be
		// converted, and the conversion has to be TRY_CAST rather than a cast.
		// A cast does not merely mis-sort: it fails the whole statement on the
		// first value that is not a number, and one such value is enough.
		// Measured over logs.k8s_logs_v2 (967,912 rows, ts < 2026-08-19
		// 22:19:00), against a truth of 39,140 rows:
		//
		//	kv['store_id']::VARCHAR::DOUBLE > 100        [1006] invalid float
		//	                                              literal 'Some(25)'
		//	TRY_CAST(kv['store_id']::VARCHAR AS DOUBLE)      39,140
		//
		// 1,243 of that key's 40,516 rows are `Some(25)`-style debug
		// renderings, which is enough to lose the other 39,140. The
		// decomposition, so the figure is checkable: 40,516 rows hold the key,
		// 39,273 of them cast, 39,140 of those exceed 100 and 133 do not, and
		// 40,516 − 39,273 = 1,243 do not cast at all. An earlier version of
		// this comment said 1,376, which is 40,516 − 39,140 — the rows that
		// fail the predicate rather than the rows that fail the cast, folding
		// in those 133 real numbers.
		//
		// A plain column fails the same way — `component::DOUBLE > 5` is [1006]
		// on the value 'other', where TRY_CAST is 0 — so the fix is not
		// specific to bags.
		//
		// Nothing is traded for it: where the cast survives, the two agree
		// exactly. `kv['term']::VARCHAR::DOUBLE > 40` and the TRY_CAST form
		// both return 32,929.
		if numeric {
			return c.numericColumn(f, name), vals, nil
		}
		return f.Column, quoteAll(vals), nil
	}
}

// pinZone attaches the schema's fixed UTC offset to a bare literal, so the
// bound means the same instant whatever time zone the session is set to.
//
// Nothing happens unless Schema.TimeZone is set, which is the default. When it
// is, a bare date has to be expanded to midnight first: the engine parses
// `'2026-08-18 00:00:00+00:00'` and rejects `'2026-08-18+00:00'` with
// `[1006] cannot parse to type TIMESTAMP`, so appending the offset to a date is
// not a smaller version of the same change, it is a broken statement.
//
// A literal that already carries a zone is left alone — the user said what they
// meant, and overriding it would be the same class of silent rewrite this
// library exists to remove.
func (c *compiler) pinZone(v string) string {
	if c.schema.TimeZone == "" || hasZone(v) {
		return v
	}
	if len(v) == 10 { // a bare YYYY-MM-DD
		v += " 00:00:00"
	}
	return v + c.schema.TimeZone
}

// hasZone reports whether an instant already names its offset.
func hasZone(v string) bool {
	if strings.HasSuffix(v, "Z") || strings.HasSuffix(v, "z") {
		return true
	}
	// The offset sign can only appear after the clock, so anything before
	// position 11 is part of the date.
	return strings.LastIndexAny(v, "+-") > 10
}

// completeInstant reports whether v names a single point in time rather than a
// prefix of one.
//
//	2026-08-18                     a whole day, and unambiguous — accepted
//	2026-08-18T22:30               a whole minute — accepted
//	2026-08-18 22:30:00.123+05:30  accepted, in every spelling of the zone
//	2026-08-18T22                  a *prefix* — refused
//	2026-08                        a prefix — refused
//
// A bare date is deliberately allowed: "on the 18th" is what someone means by
// it, and the existing bracket-range spellings depend on it. What is refused
// is a clock the user began and did not finish, which is the shape a lexer
// bug or a fat finger produces and the shape the engine rounds off in silence.
func completeInstant(v string) bool {
	p := scanner{s: v}
	if !(p.digits(4) && p.byte('-') && p.digits(2) && p.byte('-') && p.digits(2)) {
		return false
	}
	if p.done() {
		return true // a bare date
	}
	if !(p.byte('T') || p.byte('t') || p.byte(' ')) {
		return false
	}
	if !(p.digits(2) && p.byte(':') && p.digits(2)) {
		return false
	}
	if p.byte(':') && !p.digits(2) {
		return false
	}
	if p.byte('.') && !p.someDigits() {
		return false
	}
	if p.done() {
		return true
	}
	if p.byte('Z') || p.byte('z') {
		return p.done()
	}
	if !(p.byte('+') || p.byte('-')) {
		return false
	}
	if !p.digits(2) {
		return false
	}
	p.byte(':') // the colon in the offset is optional
	return p.digits(2) && p.done()
}

// scanner is a byte cursor whose predicates consume on success and leave the
// cursor alone on failure, so completeInstant reads as the grammar it checks.
type scanner struct {
	s string
	i int
}

func (p *scanner) done() bool { return p.i >= len(p.s) }

func (p *scanner) byte(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *scanner) digits(n int) bool {
	if p.i+n > len(p.s) {
		return false
	}
	for k := 0; k < n; k++ {
		if c := p.s[p.i+k]; c < '0' || c > '9' {
			return false
		}
	}
	p.i += n
	return true
}

func (p *scanner) someDigits() bool {
	start := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	return p.i > start
}

func quoteAll(vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = "'" + escapeString(v) + "'"
	}
	return out
}

// quoteQueryValue renders a value inside the query() mini-language.
//
// Anything that is not a plain token is double-quoted, which also makes it a
// phrase. That is the safe default: an unquoted value containing a space would
// otherwise be split, and the default operator between bare terms inside
// query() is OR, not AND — measured, `query('msg:peer msg:status')` returns
// 93,425, exactly the union. This compiler never relies on that default and
// always emits explicit operators.
func quoteQueryValue(v string, phrase bool) string {
	if !phrase && isPlainToken(v) {
		return v
	}
	return `"` + escapeQueryPhrase(v) + `"`
}

func isPlainToken(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// escapeString makes a value safe inside a single-quoted Databend literal.
// Backslash is escaped as well as the quote, because Databend honours backslash
// escapes inside string literals.
func escapeString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return s
}

// escapeLike additionally neutralises the LIKE metacharacters so a literal % or
// _ in a search term does not become a wildcard.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// likePattern turns a search value carrying Lucene's wildcards into a LIKE
// pattern: `*` becomes `%`, `?` becomes `_`.
//
// The order is the whole point, and it is why this is not two ReplaceAll
// calls over the finished string. escapeLike turns a user's literal `_` into
// `\_`; the wildcard `?` has to become `_` *after* that, or the two are
// indistinguishable and `pod:a_b` starts matching `axb`. So the literal runs
// between wildcards are escaped first, one run at a time, and the wildcards
// are written out as themselves — a translation that cannot be reordered by
// accident.
func likePattern(s string) string {
	var b strings.Builder
	lit := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*':
			b.WriteString(escapeLike(s[lit:i]))
			b.WriteByte('%')
			lit = i + 1
		case '?':
			b.WriteString(escapeLike(s[lit:i]))
			b.WriteByte('_')
			lit = i + 1
		}
	}
	b.WriteString(escapeLike(s[lit:]))
	return b.String()
}

// escapeQueryPhrase escapes the double quotes that delimit a phrase inside the
// query() mini-language, before the whole expression is escaped again as a SQL
// literal.
func escapeQueryPhrase(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
