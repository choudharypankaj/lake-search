package parser

import "strings"

type tokKind int

const (
	tokEOF tokKind = iota
	tokWord
	tokQuoted
	tokColon
	tokLParen
	tokRParen
	tokAnd
	tokOr
	tokNot
	tokMinus // a `-` in term position, i.e. Lucene's negation shorthand
	tokRange // a bracket range as typed, e.g. `[100 TO 200]` or `{a TO *}`
	tokRegex // the body of a `/pattern/` regex term, slashes stripped
)

type token struct {
	kind tokKind
	val  string
	pos  int
}

// lex splits the raw search string into tokens.
//
// Word characters are "anything not otherwise special", which keeps operators
// like `>` and `~` and wildcards inside the word for the parser to interpret.
// That is intentional: `duration:>1000`, `snapshot~1` and `snapsh*` all arrive
// as a single word whose shape decides the node type.
//
// The colon is the one character whose meaning depends on context, and getting
// that wrong is a silent failure rather than a syntax error — see opensField.
func lex(s string) []token {
	var out []token
	i := 0

	for i < len(s) {
		c := s[i]

		switch {
		case isSpace(c):
			i++
			continue

		case c == '(':
			out = append(out, token{tokLParen, "(", i})
			i++
			continue

		case c == ')':
			out = append(out, token{tokRParen, ")", i})
			i++
			continue

		case c == '"':
			val, next := lexQuoted(s, i)
			out = append(out, token{tokQuoted, val, i})
			i = next
			continue

		case c == ':' && i > 0 && s[i-1] == '"' && len(out) > 0 && out[len(out)-1].kind == tokQuoted:
			// A colon touching a closing quote is a field separator, which is
			// the only way to name a bag key containing a space — and nine
			// keys in the reference table do, `msg type` among them. Without
			// this the colon falls into the word scanner, which cannot open a
			// field on an empty name, so `"msg type":MsgRequestVote` became a
			// phrase plus a search for the literal token `:MsgRequestVote`.
			// Zero rows either way, and the filter the user typed was gone.
			out = append(out, token{tokColon, ":", i})
			i++
			continue

		case (c == '[' || c == '{') && i > 0 && s[i-1] == ':':
			// A bracket range, and only ever directly after a field colon.
			// `{` in particular is ordinary text elsewhere — log lines are
			// full of JSON — so the range form is deliberately not recognised
			// in term position, and a bracket run with no `TO` inside falls
			// through to the word scanner unchanged.
			if val, next, ok := lexRange(s, i); ok {
				out = append(out, token{tokRange, val, i})
				i = next
				continue
			}

		case c == '/' && startsValue(s, i):
			// `/pattern/`. The closing slash has to end the token, so an
			// ordinary path — `/var/log/pods` — is not mistaken for a regex.
			if val, next, ok := lexRegex(s, i); ok {
				out = append(out, token{tokRegex, val, i})
				i = next
				continue
			}

		case c == '-' && startsTerm(s, i):
			// `-term` negates. The distinction that matters is positional: a
			// hyphen *inside* a word (`pd-0`, `us-west-2`) must stay part of
			// the word, so negation requires whitespace or `(` to the left and
			// a non-space to the right.
			out = append(out, token{tokMinus, "-", i})
			i++
			continue

		case c == '+' && startsTerm(s, i):
			// Lucene's required-term marker. Adjacency already means AND here,
			// so `+` carries no information and is consumed. Searching for it
			// literally — which is what happens if it is left in the word —
			// matches nothing.
			i++
			continue
		}

		start := i
		w, fieldColon, next := lexWord(s, i)
		i = next
		if i == start { // defensive: never fail to advance
			i++
			continue
		}

		if fieldColon >= 0 {
			// A field name is a name even when it spells an operator.
			out = append(out, token{tokWord, w, start})
			out = append(out, token{tokColon, ":", fieldColon})
			continue
		}

		// A word in value position is a value, whatever it spells. `msg:not`
		// names a field and a word; classified as an operator instead, the
		// value has nowhere to go, `parseFieldValue` returns nil, the leaf
		// vanishes and the whole query compiles to match-everything — the
		// filter is not wrong, it is *gone*. Measured: `msg:not` returned all
		// 449,893 rows of the frozen window against a true 22,850.
		if len(out) > 0 && out[len(out)-1].kind == tokColon {
			// One instant is one value even though it contains a space.
			// Without this, `ts:>2026-08-18 22:30:00` — the spelling the
			// compiler's own error message recommends — splits at the space:
			// the date becomes the bound and the clock becomes a separate
			// full-text phrase, so the query returns 0 rows *and* spends the
			// statement's single search function on the literal `22:30:00`.
			if n := clockRun(s, i, w); n > 0 {
				w += s[i : i+n]
				i += n
			}
			out = append(out, token{tokWord, w, start})
			continue
		}

		// A boolean word is a *candidate* operator here, in any case it was
		// typed. Whether it is actually applied is decided by position, in
		// demoteDanglingOperators below, and — for NOT alone — by spelling.
		//
		// Case cannot be the whole rule. Making only the uppercase spellings
		// operators is Lucene's own rule, but on this grammar it silently
		// turns `snapshot or peer` into a three-word conjunction and
		// `level:(error or warn)` into `level='error' AND level='or' AND
		// level='warn'` — a structural contradiction on a single-valued
		// column, 0 rows where 199,583 were asked for. Case cannot be no part
		// of the rule either: `msg:(not ready)` read as a negation returns the
		// *complement* of `ready`.
		//
		// The line between them is what the operator does to its operands.
		// `and`, `or`, `&&` and `||` only ever say how operands COMBINE — each
		// operand still means what it meant on its own — so mistaking the
		// English word for one of them costs a join shape, never a polarity.
		// `NOT` INVERTS the operand it takes: it turns "must match" into "must
		// not match", and getting that wrong hands back the complement of the
		// table. An inverting operator therefore needs the unambiguous
		// spelling; a combining one does not.
		//
		// So: `and`/`or` (any case) and `&&`/`||` are operators wherever an
		// infix operator is grammatical; `NOT` is an operator only in capitals
		// and only before an operand; every other occurrence is the word the
		// user typed, and the compiler says so.
		//
		// The colon rule above has already claimed everything in field-value
		// position, so `msg:not` never reaches this switch at all.
		switch {
		case w == "&&", strings.EqualFold(w, "AND"):
			out = append(out, token{tokAnd, w, start})
		case w == "||", strings.EqualFold(w, "OR"):
			out = append(out, token{tokOr, w, start})
		case w == "NOT":
			out = append(out, token{tokNot, w, start})
		default:
			out = append(out, token{tokWord, w, start})
		}
	}

	out = append(out, token{tokEOF, "", len(s)})
	return demoteDanglingOperators(out)
}

// demoteDanglingOperators turns an operator token with nothing to operate on
// back into the word it was typed as.
//
// This is the positional half of the operator rule; the switch in lex is the
// spelling half. A word can be unambiguously an operator and still have
// nothing to operate on: `NOT` on its own, `msg:(AND)`, `(or peer)`, `msg:(not
// ready)`. In each of these the keyword switch produced an operator token that
// the parser then dropped, taking the user's only filter with it and leaving
// `1=1`. Returning the whole table for a one-word search is the failure this
// library exists to remove, and it does not become acceptable because the word
// was capitalised.
//
// Grammatical position is the whole test, and it differs by arity: an infix
// `and`/`or`/`&&`/`||` needs a token that can END an operand on its left, and
// a prefix `NOT` needs a token that can START an operand on its right.
//
// Two passes, and the order matters. NOT is resolved first, because a NOT that
// demotes to a word gives the AND in front of it the right-hand operand it
// needs: in `peer AND NOT` the NOT becomes a word and the AND stays an
// operator, which is the reading that finds the rows containing both.
//
// A *trailing* AND or OR is deliberately left alone. `peer AND` is someone
// mid-keystroke, and the generous reading — drop the dangling operator, keep
// `peer` — is what a search box needs; demoting it instead would make the
// result set collapse between two keystrokes. A *leading* one has no such
// reading: there is nothing to the left to keep, and nothing the user is on
// their way to typing will ever put something there.
func demoteDanglingOperators(toks []token) []token {
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].kind == tokNot && !startsOperand(toks[i+1].kind) {
			toks[i].kind = tokWord
		}
	}
	for i := range toks {
		k := toks[i].kind
		if k != tokAnd && k != tokOr {
			continue
		}
		if i == 0 || !endsOperand(toks[i-1].kind) {
			toks[i].kind = tokWord
		}
	}
	return toks
}

// startsOperand reports whether a token can begin the thing an operator acts on.
func startsOperand(k tokKind) bool {
	switch k {
	case tokWord, tokQuoted, tokLParen, tokRange, tokRegex, tokNot, tokMinus:
		return true
	}
	return false
}

// endsOperand reports whether a token can end the thing to an operator's left.
func endsOperand(k tokKind) bool {
	switch k {
	case tokWord, tokQuoted, tokRParen, tokRange, tokRegex:
		return true
	}
	return false
}

// lexWord reads one word, deciding for each colon it meets whether that colon
// opens a field or is ordinary text.
//
// It returns the word's value, the source position of the colon that opens a
// field (or -1), and the index to resume from. When a field colon is found the
// word ends there and the caller emits the colon separately; the colon itself
// is not consumed into the word.
func lexWord(s string, i int) (word string, fieldColon, next int) {
	var (
		val   strings.Builder // what the user meant
		shape strings.Builder // the same run with escapes neutralised
	)

	for i < len(s) {
		c := s[i]

		// `\:` is a literal colon, on either side of a field separator, so a
		// bag key that itself contains a colon stays addressable.
		if c == '\\' && i+1 < len(s) && s[i+1] == ':' {
			val.WriteByte(':')
			shape.WriteByte('_') // escaped: still a legal name character
			i += 2
			continue
		}

		if c == ':' {
			if opensField(shape.String(), s, i) {
				return val.String(), i, i + 1
			}
			val.WriteByte(':')
			shape.WriteByte(':') // poisons the name, so later colons glue too
			i++
			continue
		}

		if isSpecial(c) {
			break
		}
		val.WriteByte(c)
		shape.WriteByte(c)
		i++
	}
	return val.String(), -1, i
}

// opensField decides whether the colon at s[i] is a field separator.
//
// Getting this wrong is not a syntax error, it is a wrong answer: read as a
// separator, `http://a.com` becomes a lookup of a field named `http` and
// returns nothing, and `2026-08-18T22:30:00Z` is truncated at the first colon
// to a bound the engine happily accepts as 22:00:00.
//
// A colon opens a field only when all of these hold:
//
//	the run before it is a legal field name — `[A-Za-z_][A-Za-z0-9_.-]*`,
//	  which is what keeps a timestamp's own colons out of the decision;
//	it was not written `\:`, which always means a literal colon;
//	it is not followed by `//`, which makes it a URL scheme, for every
//	  scheme and every occurrence rather than the first one;
//	it is not `localhost:<port>`, the one host:port pair common enough in
//	  log lines to be worth naming.
func opensField(name string, s string, i int) bool {
	if !isFieldName(name) {
		return false
	}
	if i+1 >= len(s) {
		// `field:` with the value not yet typed. Kept as a field so a search
		// box mid-keystroke behaves the way it did before this rule existed.
		return true
	}
	if s[i+1] == '/' && i+2 < len(s) && s[i+2] == '/' {
		return false
	}
	if strings.EqualFold(name, "localhost") && isPortRun(s, i+1) {
		return false
	}
	return true
}

func isFieldName(name string) bool {
	if name == "" {
		return false
	}
	for j := 0; j < len(name); j++ {
		c := name[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case j > 0 && (c >= '0' && c <= '9' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// isPortRun reports whether s[i:] begins with one to five digits that end the
// word — the shape of a port number.
func isPortRun(s string, i int) bool {
	n := 0
	for i+n < len(s) && s[i+n] >= '0' && s[i+n] <= '9' {
		n++
	}
	if n == 0 || n > 5 {
		return false
	}
	return i+n >= len(s) || isSpace(s[i+n]) || isSpecial(s[i+n])
}

// lexQuoted reads a double-quoted string, honouring `\"` and `\\`, and tolerates
// an unterminated quote by running to end of input — a user mid-typing in a
// search box should get a best-effort match, not a parse error.
func lexQuoted(s string, i int) (string, int) {
	var b strings.Builder
	i++ // opening quote
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		if s[i] == '"' {
			return b.String(), i + 1
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), i
}

// lexRange reads `[a TO b]` / `{a TO b}` verbatim, brackets included, so the
// parser can read inclusivity off the delimiters.
//
// The `TO` is required: without it the run is ordinary text and the caller
// falls back to word scanning. An unterminated bracket runs to end of input,
// matching how an unterminated quote is handled.
func lexRange(s string, i int) (val string, next int, ok bool) {
	end := len(s)
	for j := i + 1; j < len(s); j++ {
		if s[j] == ']' || s[j] == '}' {
			end = j + 1
			break
		}
	}
	val = s[i:end]
	if !hasRangeSeparator(val) {
		return "", i, false
	}
	return val, end, true
}

// hasRangeSeparator looks for a ` TO ` between the brackets, case-insensitively.
func hasRangeSeparator(s string) bool {
	return splitRangeBounds(s) != nil
}

// splitRangeBounds returns the two bounds of a bracket range, or nil when the
// text is not one. Exported behaviour lives in the parser; this is the shared
// scanner both it and the lexer need.
func splitRangeBounds(s string) []string {
	body := s
	if len(body) > 0 && (body[0] == '[' || body[0] == '{') {
		body = body[1:]
	}
	if len(body) > 0 && (body[len(body)-1] == ']' || body[len(body)-1] == '}') {
		body = body[:len(body)-1]
	}
	up := strings.ToUpper(body)
	for k := 0; k+4 <= len(up); k++ {
		if up[k] == ' ' && strings.HasPrefix(up[k:], " TO ") {
			return []string{strings.TrimSpace(body[:k]), strings.TrimSpace(body[k+4:])}
		}
	}
	return nil
}

// lexRegex reads `/pattern/`, honouring `\/`.
//
// The closing slash must end the token — followed by whitespace, `)` or end of
// input — so an ordinary path like `/var/log/pods` is left as a word rather
// than silently becoming the regex `var`.
func lexRegex(s string, i int) (val string, next int, ok bool) {
	var b strings.Builder
	j := i + 1
	for j < len(s) {
		if s[j] == '\\' && j+1 < len(s) && s[j+1] == '/' {
			b.WriteByte('/')
			j += 2
			continue
		}
		if s[j] == '/' {
			if j+1 >= len(s) || isSpace(s[j+1]) || s[j+1] == ')' {
				if b.Len() == 0 {
					return "", i, false
				}
				return b.String(), j + 1, true
			}
			b.WriteByte('/')
			j++
			continue
		}
		b.WriteByte(s[j])
		j++
	}
	return "", i, false
}

// clockRun reports the length of a ` HH:MM(:SS(.fff))(zone)` run at s[i:] that
// belongs to the word already read, or 0 when there is none.
//
// It is a purely lexical rule, because this package knows nothing about
// schemas and cannot ask whether the field is a timestamp. Two conditions
// keep it from gluing anything else: the word must open with a comparison
// operator, and it must end with a `YYYY-MM-DD` date. `error 22:30:00` and
// `msg:foo 12:00` are both left alone.
//
// # Why a half-typed clock is glued on too
//
// The run is taken whole — every byte up to the next space or special
// character — rather than only when it parses as a complete clock. That looks
// like the more permissive rule and is in fact the stricter one, because the
// value then reaches the compiler's complete-instant check, which refuses it
// loudly.
//
// Refusing to glue is what made the guard unreachable for the spelling it was
// written for. `ts:>2026-08-18 22` split at the space: the date became the
// bound and `22` became a full-text term, so the query compiled to
// `(ts > '2026-08-18' AND query('msg:22'))` — 3,779 rows over the frozen
// window against the 129,009 of the `ts > '2026-08-18 22:00:00'` that was
// being typed, *and* it spent the statement's single search function on a
// fragment of a timestamp. The `T`-joined spelling `ts:>2026-08-18T22` errored
// correctly all along, because there is no space to split at. Gluing the run
// makes both spellings take the same route to the same error.
//
// The run still has to start with a digit, which is what keeps
// `ts:>2026-08-18 peer` two terms.
func clockRun(s string, i int, word string) int {
	if op, rest := splitRangeOp(word); op == "" || !endsWithDate(rest) {
		return 0
	}
	if i >= len(s) || s[i] != ' ' {
		return 0
	}
	j := i + 1
	if digits(s, j) == 0 {
		// Not a clock and not a half-typed one either: a word after a date
		// bound is an ordinary term. `ts:>2026-08-18 peer` stays two terms.
		return 0
	}
	// A zone written with a space before it, `22:30:00 +05:30`, is a third run
	// and is still left alone: it does not start with a digit, so the loop
	// below never reaches it and the clock in front of it is complete on its
	// own.
	for j < len(s) && !isSpace(s[j]) && !isSpecial(s[j]) {
		j++
	}
	return j - i
}

func digits(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] >= '0' && s[i+n] <= '9' {
		n++
	}
	return n
}

// endsWithDate reports whether v ends in `YYYY-MM-DD`.
func endsWithDate(v string) bool {
	if len(v) < 10 {
		return false
	}
	d := v[len(v)-10:]
	for k, c := range []byte(d) {
		if k == 4 || k == 7 {
			if c != '-' {
				return false
			}
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// startsTerm reports whether the byte at i begins a new term, which is what
// makes a `-` there a negation operator rather than an ordinary hyphen.
func startsTerm(s string, i int) bool {
	if i+1 >= len(s) || isSpace(s[i+1]) {
		return false // a trailing or lone `-` is just text
	}
	if i == 0 {
		return true
	}
	prev := s[i-1]
	return isSpace(prev) || prev == '('
}

// startsValue reports whether the byte at i begins either a new term or the
// value half of `field:`. A field colon is emitted as its own token, so at the
// top of the scan loop a preceding `:` can only be that separator.
func startsValue(s string, i int) bool {
	if i == 0 {
		return true
	}
	prev := s[i-1]
	return isSpace(prev) || prev == '(' || prev == ':'
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// isSpecial lists the characters that always end a word. The colon is not among
// them: whether it ends a word is decided per occurrence by opensField.
func isSpecial(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '(', ')', '"':
		return true
	}
	return false
}
