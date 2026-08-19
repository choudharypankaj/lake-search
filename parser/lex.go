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

		switch strings.ToUpper(w) {
		case "AND", "&&":
			out = append(out, token{tokAnd, w, start})
		case "OR", "||":
			out = append(out, token{tokOr, w, start})
		case "NOT":
			out = append(out, token{tokNot, w, start})
		default:
			out = append(out, token{tokWord, w, start})
		}
	}

	out = append(out, token{tokEOF, "", len(s)})
	return out
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
