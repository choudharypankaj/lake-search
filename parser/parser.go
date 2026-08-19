package parser

import (
	"strconv"
	"strings"
)

// Parse turns search text into a syntax tree.
//
// It never returns an error. A search box is edited character by character, and
// a parser that rejects half-typed input produces a UI that flickers between
// "results" and "syntax error" on every keystroke. Unbalanced parens, dangling
// operators and unterminated quotes are all interpreted as generously as
// possible instead.
//
// An empty or whitespace-only query returns nil, which the emitter renders as a
// match-everything predicate. That case matters more than it looks: on Databend
// `match(col,”)` matches *nothing* and raises no error, so an empty search box
// must produce SQL containing no match() call at all.
func Parse(s string) Node {
	p := &parser{toks: lex(s)}
	n := p.parseOr()
	return n
}

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }
func (p *parser) atEnd() bool { return p.peek().kind == tokEOF }
func (p *parser) skip(k tokKind) bool {
	if p.peek().kind == k {
		p.i++
		return true
	}
	return false
}

func (p *parser) parseOr() Node {
	left := p.parseAnd()
	for p.peek().kind == tokOr {
		p.next()
		right := p.parseAnd()
		if right == nil {
			break // dangling OR: ignore it
		}
		if left == nil {
			left = right
			continue
		}
		if or, ok := left.(*Or); ok {
			or.Children = append(or.Children, right)
		} else {
			left = &Or{Children: []Node{left, right}}
		}
	}
	return left
}

func (p *parser) parseAnd() Node {
	var children []Node
	for {
		// An explicit AND is consumed and ignored — adjacency already means
		// AND, so `a AND b` and `a b` take the same path.
		p.skip(tokAnd)

		if p.atEnd() || p.peek().kind == tokRParen || p.peek().kind == tokOr {
			break
		}
		n := p.parseUnary()
		if n == nil {
			// A leaf that produced nothing — `field:` with no value yet, a
			// bare `*`, a dangling negation — must not truncate the rest of
			// the conjunction. It used to: `level:> peer` compiled to 1=1,
			// silently returning the whole table because the half-typed
			// bound took the real search term with it. Every nil path other
			// than end-of-input has consumed at least one token, so skipping
			// is always progress.
			if p.atEnd() {
				break
			}
			continue
		}
		children = append(children, n)
	}

	switch len(children) {
	case 0:
		return nil
	case 1:
		return children[0]
	default:
		return &And{Children: children}
	}
}

func (p *parser) parseUnary() Node {
	if p.peek().kind == tokNot || p.peek().kind == tokMinus {
		p.next()
		child := p.parseUnary()
		if child == nil {
			return nil // dangling negation
		}
		return &Not{Child: child}
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() Node {
	switch p.peek().kind {
	case tokLParen:
		p.next()
		inner := p.parseOr()
		p.skip(tokRParen) // tolerate a missing close paren
		return inner

	case tokQuoted:
		t := p.next()
		// A quoted token followed immediately by a colon is a field name, not
		// a phrase. The generous-parser policy is to drop punctuation it
		// cannot place, never to drop a filter, and dropping the `:value`
		// after a quoted name was doing exactly the latter.
		if p.peek().kind == tokColon {
			p.next()
			return p.parseFieldValue(t.val)
		}
		return p.phrase("", t.val)

	case tokRegex:
		t := p.next()
		return &Term{Value: t.val, Regex: true}

	case tokWord:
		w := p.next()
		// `field:value` — only if a colon follows immediately.
		if p.peek().kind == tokColon {
			p.next()
			return p.parseFieldValue(w.val)
		}
		return buildTerm("", w.val, false)

	default:
		// A stray colon, paren or operator: consume it so we always advance.
		// Never consume EOF — a dangling `NOT` would then walk the index past
		// the end of the token slice and panic on the next peek, which is a
		// crash rather than the generous reading Parse promises.
		if p.atEnd() {
			return nil
		}
		p.next()
		return nil
	}
}

// phrase builds a quoted term, attaching a `~N` proximity marker if one
// follows the closing quote.
//
// The marker has to be recognised here rather than left in the token stream:
// as an ordinary word `~3` it becomes a second search term, and searching a
// log table for the literal token `~3` matches nothing.
func (p *parser) phrase(field, value string) Node {
	t := &Term{Field: field, Value: value, Phrase: true}

	// The markers arrive as one word, because nothing separates them from each
	// other: `"a b"~3^2` lexes as the single word `~3^2`. Splitting them here
	// is what stops `~3` becoming a search term of its own.
	w := p.peek()
	if w.kind != tokWord {
		return t
	}
	base, tilde, hasTilde, boost := trailingMarkers(w.val)
	if base != "" || (!hasTilde && boost == "") {
		return t // not a bare marker run
	}
	p.next()
	if hasTilde {
		t.Slop = tilde
	}
	t.Boost = boost
	return t
}

// trailingMarkers splits a trailing run of Lucene's `~N` and `^N` markers off a
// word, returning what precedes them.
//
// It reads right to left so both orderings work and so a marker is only ever
// recognised at the end: `foo^bar` keeps its caret, because `bar` is not a
// number, and `1.5^2` yields the value 1.5 with a boost of 2.
func trailingMarkers(raw string) (base string, tilde int, hasTilde bool, boost string) {
	base = raw
	for {
		i := strings.LastIndexAny(base, "~^")
		if i < 0 {
			return base, tilde, hasTilde, boost
		}
		num := base[i+1:]
		switch base[i] {
		case '~':
			if num == "" {
				tilde, hasTilde = 1, true
			} else if n, err := strconv.Atoi(num); err == nil && n >= 0 {
				tilde, hasTilde = n, true
			} else {
				return base, tilde, hasTilde, boost
			}
		case '^':
			if _, err := strconv.ParseFloat(num, 64); err != nil {
				return base, tilde, hasTilde, boost
			}
			boost = num
		}
		base = base[:i]
	}
}

// parseFieldValue handles everything to the right of `field:`.
func (p *parser) parseFieldValue(field string) Node {
	switch p.peek().kind {
	case tokLParen:
		// `field:(a OR b)` — the field scopes the whole group, which is how an
		// OR over one field is written. Without this branch the field and the
		// group are both dropped and the query compiles to match-everything.
		p.next()
		inner := p.parseOr()
		p.skip(tokRParen) // tolerate a missing close paren
		stampField(inner, field)
		return inner

	case tokRange:
		t := p.next()
		return buildBetween(field, t.val)

	case tokRegex:
		t := p.next()
		return &Term{Field: field, Value: t.val, Regex: true}

	case tokQuoted:
		t := p.next()
		return p.phrase(field, t.val)

	case tokWord:
		v := p.next().val

		// Range: field:>1000, field:>=1000, field:<1000, field:<=1000
		if op, rest := splitRangeOp(v); op != "" {
			if rest == "" {
				// `field:>"2026-08-18 22:30:00"` — quoting the bound is the
				// other way to keep a space inside one value, and it is a
				// spelling the compiler recommends, so it has to work. The
				// operator ends the word at the opening quote, leaving the
				// value in the next token.
				if p.peek().kind == tokQuoted {
					return &Range{Field: field, Op: op, Value: p.next().val}
				}
				// `field:>` with the value not yet typed.
				return nil
			}
			return &Range{Field: field, Op: op, Value: rest}
		}

		// Existence: field:*
		if v == "*" {
			return &Term{Field: field, Exists: true}
		}

		return buildTerm(field, v, false)

	default:
		// `field:` with nothing after it yet.
		return nil
	}
}

// buildTerm interprets the two pieces of Lucene syntax that live inside a word:
// a trailing fuzziness marker, and wildcards anywhere in the value.
//
// Both are the traps documented in LOG_PIPELINE_FINDINGS.md §5.10: typed into
// Databend's query() a fuzzy term returns zero rows and a wildcard is truncated
// at the star, in both cases with no error. Recognising them here is the entire
// point of this project — the emitter maps them onto the forms that do work
// (fuzziness=N and LIKE).
func buildTerm(field, raw string, phrase bool) Node {
	t := &Term{Field: field, Phrase: phrase}

	// Fuzziness `term~N` and boost `term^N`, in either order. Only a *trailing*
	// run counts, so a tilde or caret inside a word — both occur in log lines —
	// is left alone, and a run that would consume the whole word is not a
	// marker at all but the search text itself.
	if base, tilde, hasTilde, boost := trailingMarkers(raw); base != "" {
		raw = base
		if hasTilde {
			t.Fuzz = tilde
		}
		t.Boost = boost
	}

	// Wildcards anywhere, not only at the ends. A star in the middle of a
	// word is the trap: forwarded into the engine's search syntax it is
	// truncated at the star, so `reg*on` becomes a search for `reg` and
	// returns registration lines with a straight face. The value keeps its
	// stars and question marks and the emitter turns them into a pattern.
	if strings.ContainsAny(raw, "*?") {
		if strings.Trim(raw, "*") == "" {
			// Nothing but stars: an existence test, not a pattern. A lone
			// `?` is left as a pattern, because it does constrain — it asks
			// for exactly one character.
			if field != "" {
				return &Term{Field: field, Exists: true}
			}
			return nil
		}
		t.Wildcard = true
	}

	t.Value = raw
	if t.Value == "" {
		return nil
	}
	return t
}

// splitRangeOp detects a comparison operator at the head of a value.
func splitRangeOp(v string) (op, rest string) {
	for _, candidate := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(v, candidate) {
			return candidate, strings.TrimSpace(v[len(candidate):])
		}
	}
	return "", v
}

// stampField pushes the field of a `field:(...)` group down onto every leaf
// inside it that did not name a field of its own.
//
// An inner field wins: in `foo:(bar:(baz) qux)` the `baz` leaf keeps `bar`, and
// only `qux` inherits `foo` — the reading every Lucene front end gives it.
func stampField(n Node, field string) {
	switch t := n.(type) {
	case *And:
		for _, c := range t.Children {
			stampField(c, field)
		}
	case *Or:
		for _, c := range t.Children {
			stampField(c, field)
		}
	case *Not:
		stampField(t.Child, field)
	case *Term:
		if t.Field == "" {
			t.Field = field
		}
	case *Range:
		if t.Field == "" {
			t.Field = field
		}
	case *Between:
		if t.Field == "" {
			t.Field = field
		}
	}
}

// buildBetween turns `[a TO b]`, `{a TO b}` and their mixed spellings into a
// Between node. `*` on either side means unbounded.
func buildBetween(field, raw string) Node {
	bounds := splitRangeBounds(raw)
	if bounds == nil {
		return nil
	}
	b := &Between{
		Field:  field,
		Lo:     bounds[0],
		Hi:     bounds[1],
		LoIncl: strings.HasPrefix(raw, "["),
		HiIncl: strings.HasSuffix(raw, "]"),
	}
	if b.Lo == "*" {
		b.Lo = ""
	}
	if b.Hi == "*" {
		b.Hi = ""
	}
	return b
}
