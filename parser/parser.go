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
			break
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
		return &Term{Value: t.val, Phrase: true}

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
		p.next()
		return nil
	}
}

// parseFieldValue handles everything to the right of `field:`.
func (p *parser) parseFieldValue(field string) Node {
	switch p.peek().kind {
	case tokQuoted:
		t := p.next()
		return &Term{Field: field, Value: t.val, Phrase: true}

	case tokWord:
		v := p.next().val

		// Range: field:>1000, field:>=1000, field:<1000, field:<=1000
		if op, rest := splitRangeOp(v); op != "" {
			if rest == "" {
				// `field:>` with the number not yet typed.
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
// a trailing fuzziness marker and leading/trailing wildcards.
//
// Both are the traps documented in LOG_PIPELINE_FINDINGS.md §5.10: typed into
// Databend's query() they return zero rows and raise no error. Recognising them
// here is the entire point of this project — the emitter maps them onto the
// forms that do work (fuzziness=N and LIKE).
func buildTerm(field, raw string, phrase bool) Node {
	t := &Term{Field: field, Phrase: phrase}

	// Fuzziness: `term~`, `term~2`. Only a trailing marker counts, so a tilde
	// inside a word (rare, but possible in a log line) is left alone.
	if i := strings.LastIndexByte(raw, '~'); i > 0 && i < len(raw) {
		suffix := raw[i+1:]
		if suffix == "" {
			t.Fuzz = 1
			raw = raw[:i]
		} else if n, err := strconv.Atoi(suffix); err == nil && n >= 0 {
			t.Fuzz = n
			raw = raw[:i]
		}
	}

	if strings.HasPrefix(raw, "*") {
		t.Suffix = true
		raw = strings.TrimPrefix(raw, "*")
	}
	if strings.HasSuffix(raw, "*") {
		t.Prefix = true
		raw = strings.TrimSuffix(raw, "*")
	}

	t.Value = raw
	if t.Value == "" {
		// The word was nothing but wildcards.
		if field != "" {
			return &Term{Field: field, Exists: true}
		}
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
