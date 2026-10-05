package vdbecc

import (
	"fmt"
	"strings"
)

// The textual IR is line oriented: every top-level entity starts on its own
// line, and every instruction is one line (a switch's case list aside, which the
// reader joins). So the lexer turns one logical line into tokens, and the
// parser walks the token slice.

type tokKind uint8

const (
	tkWord   tokKind = iota // keywords, type names, numbers, labels without sigil
	tkLocal                 // %name
	tkGlobal                // @name
	tkMeta                  // !name, #n: metadata and attribute groups
	tkStr                   // c"...", the raw text between the quotes
	tkPunct                 // one of ( ) [ ] { } < > , = * ...
)

type token struct {
	kind tokKind
	text string // for tkLocal/tkGlobal: the name without sigil or quotes
}

func (t token) String() string {
	switch t.kind {
	case tkLocal:
		return "%" + t.text
	case tkGlobal:
		return "@" + t.text
	case tkStr:
		return `c"` + t.text + `"`
	}
	return t.text
}

func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '_' || b == '.' || b == '-' || b == '$'
}

// tokenize splits one line. A ';' outside a string starts a comment.
func tokenize(line string) ([]token, error) {
	var out []token
	i := 0
	name := func() (string, error) { // after a sigil: a bare or quoted name
		if i < len(line) && line[i] == '"' {
			j := strings.IndexByte(line[i+1:], '"')
			if j < 0 {
				return "", fmt.Errorf("unterminated quoted name")
			}
			s := line[i+1 : i+1+j]
			i += j + 2
			return s, nil
		}
		start := i
		for i < len(line) && wordByte(line[i]) {
			i++
		}
		return line[start:i], nil
	}
	for i < len(line) {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == ';':
			return out, nil
		case c == '%' || c == '@':
			i++
			n, err := name()
			if err != nil {
				return nil, err
			}
			k := tkLocal
			if c == '@' {
				k = tkGlobal
			}
			out = append(out, token{k, n})
		case c == '!' || c == '#':
			i++
			n, err := name()
			if err != nil {
				return nil, err
			}
			out = append(out, token{tkMeta, string(c) + n})
		case c == 'c' && i+1 < len(line) && line[i+1] == '"':
			j := i + 2
			for j < len(line) && line[j] != '"' {
				j++
			}
			if j >= len(line) {
				return nil, fmt.Errorf("unterminated string constant")
			}
			out = append(out, token{tkStr, line[i+2 : j]})
			i = j + 1
		case c == '.' && strings.HasPrefix(line[i:], "..."):
			out = append(out, token{tkPunct, "..."})
			i += 3
		case wordByte(c):
			start := i
			for i < len(line) {
				if wordByte(line[i]) {
					i++
				} else if line[i] == '+' && (line[i-1] == 'e' || line[i-1] == 'E') && start < i-1 &&
					(line[start] >= '0' && line[start] <= '9' || line[start] == '-') {
					i++ // a float exponent sign: 1.5e+01
				} else {
					break
				}
			}
			out = append(out, token{tkWord, line[start:i]})
		case strings.IndexByte("()[]{}<>,=*:", c) >= 0:
			out = append(out, token{tkPunct, string(c)})
			i++
		case c == '"':
			j := strings.IndexByte(line[i+1:], '"')
			if j < 0 {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, token{tkWord, line[i : i+j+2]})
			i += j + 2
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	return out, nil
}

// cursor walks a token slice.
type cursor struct {
	toks []token
	pos  int
}

func (c *cursor) done() bool { return c.pos >= len(c.toks) }

func (c *cursor) peek() token {
	if c.done() {
		return token{kind: tkPunct, text: ""}
	}
	return c.toks[c.pos]
}

func (c *cursor) next() token {
	t := c.peek()
	if !c.done() {
		c.pos++
	}
	return t
}

// is reports whether the next token is the word or punctuation s.
func (c *cursor) is(s string) bool {
	t := c.peek()
	return (t.kind == tkWord || t.kind == tkPunct) && t.text == s
}

// accept consumes s if it is next.
func (c *cursor) accept(s string) bool {
	if c.is(s) {
		c.pos++
		return true
	}
	return false
}

func (c *cursor) want(s string) error {
	if c.accept(s) {
		return nil
	}
	return fmt.Errorf("expected %q, found %q", s, c.peek())
}

// acceptAny consumes every leading word in set.
func (c *cursor) acceptAny(set map[string]bool) {
	for c.peek().kind == tkWord && set[c.peek().text] {
		c.pos++
	}
}

// skipGroup consumes a balanced (...) or [...] or {...} starting here.
func (c *cursor) skipGroup() error {
	open := c.next().text
	closeOf := map[string]string{"(": ")", "[": "]", "{": "}", "<": ">"}[open]
	if closeOf == "" {
		return fmt.Errorf("expected a group, found %q", open)
	}
	depth := 1
	for depth > 0 {
		if c.done() {
			return fmt.Errorf("unbalanced %q", open)
		}
		t := c.next()
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case open:
			depth++
		case closeOf:
			depth--
		}
	}
	return nil
}

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}
