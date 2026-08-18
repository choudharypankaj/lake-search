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
func lex(s string) []token {
	var out []token
	i := 0

	for i < len(s) {
		c := s[i]

		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
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

		case c == ':':
			out = append(out, token{tokColon, ":", i})
			i++
			continue

		case c == '"':
			val, next := lexQuoted(s, i)
			out = append(out, token{tokQuoted, val, i})
			i = next
			continue

		case c == '-' && startsTerm(s, i):
			// `-term` negates. The distinction that matters is positional: a
			// hyphen *inside* a word (`pd-0`, `us-west-2`) must stay part of
			// the word, so negation requires whitespace or `(` to the left and
			// a non-space to the right.
			out = append(out, token{tokMinus, "-", i})
			i++
			continue
		}

		start := i
		for i < len(s) && !isSpecial(s[i]) {
			i++
		}
		if i == start { // defensive: never fail to advance
			i++
			continue
		}
		w := s[start:i]

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

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isSpecial(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '(', ')', ':', '"':
		return true
	}
	return false
}
