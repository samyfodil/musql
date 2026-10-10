// This file implements a small tokenizer for the SQL subset this package
// executes (see query.go for the supported grammar). It is shared by the
// SELECT parser (sql_parser.go) and the CREATE TABLE column-info parser used
// to recover column names/types/affinity from sqlite_schema's SQL text.
package engine

import (
	"fmt"
	"strings"
)

// tokKind identifies the lexical category of a token.
type tokKind int

const (
	tkEOF    tokKind = iota
	tkIdent          // bare or quoted identifier/keyword (case preserved in text)
	tkNumber         // integer or real literal, as written (e.g. "123", "1.5e10")
	tkString         // 'single quoted' string literal, decoded (escapes resolved)
	tkBlob           // X'...' blob literal, decoded
	tkPunct          // operator/punctuation: one of ( ) , . ; + - * / % || < <= > >= = == != <> -> ->> ~
	tkParam          // bound-parameter placeholder: "?", "?NNN", ":name", "@name", or "$name" (text carries the raw spelling, sigil included)
)

// token is one lexical token. Start/End are byte offsets into the original
// source string spanning the raw token text (including quotes for
// string/blob/quoted-identifier tokens); they let the parser recover
// verbatim source text for an expression, which SQLite uses as the default
// name of an unaliased result column.
type token struct {
	kind   tokKind
	text   string // ident name / number literal text / punctuation text
	str    string // decoded value, tkString only
	blob   []byte // decoded value, tkBlob only
	quoted bool   // tkIdent only: true for a "...", `...`, or [...] spelling
	// dquoted is quoted narrowed to double-quote spelling alone: a "..."-spelled
	// identifier that resolves to no column becomes a string literal, while
	// `...` and [...] stay hard column references. See ColumnExpr.FallbackLiteral.
	dquoted bool
	Start   int
	End     int
}

// upper returns text upper-cased for case-insensitive keyword comparison,
// using ASCII-only folding (not strings.ToUpper). C SQLite folds through a
// 256-byte table where only a-z move and bytes >= 0x80 map to themselves.
func (t token) upper() string { return asciiUpper(t.text) }

// asciiUpper folds a-z to uppercase; every other byte is unchanged. Returns s
// unchanged when nothing moves, saving allocation for already-uppercase keywords.
func asciiUpper(s string) string {
	i := 0
	for ; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			break
		}
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// lex tokenizes s, returning tokens terminated by a single tkEOF token.
// lexQuoted reads a quoted literal or identifier whose body starts at s[j],
// up to its closer; with double set, a doubled closer stands for one. It
// returns the body and the index past the closer, or false when the closer
// never comes. A body with no doubled closer -- nearly all of them -- is a
// substring of s, found with IndexByte rather than copied a byte at a time,
// which is what a statement carrying long literals (a vector('[...]') per
// row) spends its parse on.
func lexQuoted(s string, j int, closer byte, double bool) (string, int, bool) {
	start := j
	var esc []byte // the body so far, once an escape has made it differ from s
	for {
		k := strings.IndexByte(s[j:], closer)
		if k < 0 {
			return "", 0, false
		}
		j += k
		if double && j+1 < len(s) && s[j+1] == closer {
			esc = append(esc, s[start:j+1]...)
			j += 2
			start = j
			continue
		}
		if esc == nil {
			return s[start:j], j + 1, true
		}
		return string(append(esc, s[start:j]...)), j + 1, true
	}
}

func lex(s string) ([]token, error) {
	// Pre-sized for typical SQL token density, capped: a statement made of
	// long literals (rows of vector('[...]')) has far fewer tokens than its
	// length suggests, and an uncapped guess zeroed megabytes per statement.
	// append grows it past the cap when a statement needs more.
	n := len(s)
	toks := make([]token, 0, min(n/4, 4096)+8)
	i := 0

	isIdentStart := func(c byte) bool {
		return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
	}
	isIdentCont := func(c byte) bool {
		return isIdentStart(c) || (c >= '0' && c <= '9')
	}
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }

	for i < n {
		c := s[i]
		start := i
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			i++

		case c == '-' && i+1 < n && s[i+1] == '-': // line comment
			for i < n && s[i] != '\n' {
				i++
			}

		// Bare "/*" with no third byte is not a comment but operators, which errors.
		// Requiring a third byte here handles that distinction.
		case c == '/' && i+2 < n && s[i+1] == '*': // block comment
			// An unterminated block comment runs to end of input and is not an error.
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				i = n
				break
			}
			i = i + 2 + j + 2

		case (c == 'x' || c == 'X') && i+1 < n && s[i+1] == '\'':
			j := i + 2
			hstart := j
			for j < n && s[j] != '\'' {
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("engine: unterminated blob literal")
			}
			hexStr := s[hstart:j]
			if len(hexStr)%2 != 0 {
				return nil, fmt.Errorf("engine: blob literal %q has an odd number of hex digits", hexStr)
			}
			b := make([]byte, len(hexStr)/2)
			for k := 0; k < len(b); k++ {
				hi, ok1 := hexDigit(hexStr[2*k])
				lo, ok2 := hexDigit(hexStr[2*k+1])
				if !ok1 || !ok2 {
					return nil, fmt.Errorf("engine: invalid hex digit in blob literal %q", hexStr)
				}
				b[k] = hi<<4 | lo
			}
			i = j + 1
			toks = append(toks, token{kind: tkBlob, blob: b, Start: start, End: i})

		case c == '\'': // string literal; '' escapes a literal quote
			lit, j, ok := lexQuoted(s, i+1, '\'', true)
			if !ok {
				return nil, fmt.Errorf("engine: unterminated string literal")
			}
			i = j
			toks = append(toks, token{kind: tkString, str: lit, Start: start, End: i})

		case c == '"' || c == '`' || c == '[': // quoted identifiers
			closer := byte('"')
			doubleEscape := true
			switch c {
			case '`':
				closer = '`'
			case '[':
				closer = ']'
				doubleEscape = false
			}
			text, j, ok := lexQuoted(s, i+1, closer, doubleEscape)
			if !ok {
				return nil, fmt.Errorf("engine: unterminated quoted identifier")
			}
			i = j
			toks = append(toks, token{kind: tkIdent, text: text, quoted: true, dquoted: closer == '"', Start: start, End: i})

		case c == '0' && i+1 < n && (s[i+1] == 'x' || s[i+1] == 'X'):
			// Hexadecimal integer literal: "0x"/"0X" followed by hex digits.
			// Checked before the general digit case so "0x..." doesn't fall into
			// decimal scanning. If followed by an identifier byte, it's an error.
			j := i + 2
			for j < n {
				if _, ok := hexDigit(s[j]); !ok {
					if s[j] == digitSeparator {
						j++
						continue
					}
					break
				}
				j++
			}
			if j == i+2 || (j < n && isIdentCont(s[j])) {
				k := j
				for k < n && isIdentCont(s[k]) {
					k++
				}
				return nil, fmt.Errorf("engine: unrecognized token: %q", s[start:k])
			}
			text, serr := dequoteNumber(s[start:j], true)
			if serr != nil {
				return nil, serr
			}
			i = j
			toks = append(toks, token{kind: tkNumber, text: text, Start: start, End: i})

		case isDigit(c) || (c == '.' && i+1 < n && isDigit(s[i+1])):
			j := i
			digits := func() {
				for j < n && (isDigit(s[j]) || s[j] == digitSeparator) {
					j++
				}
			}
			digits()
			if j < n && s[j] == '.' {
				j++
				digits()
			}
			if j < n && (s[j] == 'e' || s[j] == 'E') {
				k := j + 1
				if k < n && (s[k] == '+' || s[k] == '-') {
					k++
				}
				if k < n && isDigit(s[k]) {
					j = k
					digits()
				}
			}
			// A numeric literal immediately followed by an identifier-start
			// character without separator ("123abc", "1.5e10x") is a lex error.
			if j < n && isIdentStart(s[j]) {
				k := j
				for k < n && isIdentCont(s[k]) {
					k++
				}
				return nil, fmt.Errorf("engine: unrecognized token: %q", s[start:k])
			}
			text, serr := dequoteNumber(s[start:j], false)
			if serr != nil {
				return nil, serr
			}
			i = j
			toks = append(toks, token{kind: tkNumber, text: text, Start: start, End: i})

		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentCont(s[j]) {
				j++
			}
			i = j
			toks = append(toks, token{kind: tkIdent, text: s[start:i], Start: start, End: i})

		case c == '?': // anonymous "?" or explicitly-numbered "?NNN"
			j := i + 1
			for j < n && isDigit(s[j]) {
				j++
			}
			i = j
			toks = append(toks, token{kind: tkParam, text: s[start:i], Start: start, End: i})

		case (c == ':' || c == '@' || c == '$') && i+1 < n && isIdentStart(s[i+1]):
			// Named parameter: ":name", "@name", or "$name" (TCL forms not supported).
			j := i + 1
			for j < n && isIdentCont(s[j]) {
				j++
			}
			i = j
			toks = append(toks, token{kind: tkParam, text: s[start:i], Start: start, End: i})

		default:
			// "->>" is checked before "->" to match the longer token first.
			if i+3 <= n && s[i:i+3] == "->>" {
				i += 3
				toks = append(toks, token{kind: tkPunct, text: "->>", Start: start, End: i})
				continue
			}
			two := ""
			if i+2 <= n {
				two = s[i : i+2]
			}
			switch two {
			case "<=", ">=", "==", "!=", "<>", "||", "->", "<<", ">>":
				i += 2
				toks = append(toks, token{kind: tkPunct, text: two, Start: start, End: i})
			default:
				if strings.IndexByte("()+-*/%,.;<>=~&|", c) < 0 {
					return nil, fmt.Errorf("engine: unexpected character %q", c)
				}
				i++
				toks = append(toks, token{kind: tkPunct, text: string(c), Start: start, End: i})
			}
		}
	}
	toks = append(toks, token{kind: tkEOF, Start: n, End: n})
	return toks, nil
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// SplitStatements splits a SQL script into individual statements in order.
// Splitting cannot be done by text alone: the lexer is required because a
// semicolon inside a string literal, quoted identifier, or comment is not a
// boundary. Trigger bodies are tracked: semicolons inside a trigger body do
// not end the statement. Empty statements produce no entry.
func SplitStatements(sqlText string) ([]string, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	var out []string
	first := -1 // index into toks of the current statement's first token
	inBody := false
	sawBegin := false
	depth := 0
	flush := func(end int) {
		if first >= 0 {
			if s := strings.TrimSpace(sqlText[toks[first].Start:end]); s != "" {
				out = append(out, s)
			}
		}
		first, inBody, sawBegin, depth = -1, false, false, 0
	}
	for i, t := range toks {
		if t.kind == tkEOF {
			break
		}
		if t.kind == tkPunct && t.text == ";" {
			if !inBody {
				flush(t.Start)
			}
			continue
		}
		if first < 0 {
			first = i
			// A CREATE [OR REPLACE] [TEMP|TEMPORARY] TRIGGER opens a body whose
			// own ";"s must not split the script.
			if t.kind == tkIdent && !t.quoted && t.upper() == "CREATE" {
				for j := i + 1; j < len(toks) && j <= i+4; j++ {
					if toks[j].kind != tkIdent || toks[j].quoted {
						break
					}
					u := toks[j].upper()
					if u == "TRIGGER" {
						inBody = true
						break
					}
					if u != "TEMP" && u != "TEMPORARY" && u != "OR" && u != "REPLACE" {
						break
					}
				}
			}
		}
		if inBody && t.kind == tkIdent && !t.quoted {
			switch t.upper() {
			case "BEGIN":
				// The body's BEGIN. Nothing before it is counted.
				sawBegin = true
				depth++
			case "CASE":
				if sawBegin {
					depth++
				}
			case "END":
				if sawBegin {
					if depth--; depth <= 0 {
						inBody = false
					}
				}
			}
		}
	}
	flush(len(sqlText))
	return out, nil
}

// digitSeparator is '_': a numeric literal may carry it between digits.
const digitSeparator = '_'

// dequoteNumber removes every separator from a numeric literal and rejects one
// not between two digits. hex indicates an "0x" literal (neighbors tested with
// hex digit rules, not decimal digit rules).
func dequoteNumber(lit string, hex bool) (string, error) {
	if !strings.ContainsRune(lit, digitSeparator) {
		return lit, nil
	}
	isPart := func(b byte) bool { return b >= '0' && b <= '9' }
	if hex {
		isPart = func(b byte) bool { _, ok := hexDigit(b); return ok }
	}
	var b strings.Builder
	b.Grow(len(lit))
	for i := 0; i < len(lit); i++ {
		if lit[i] != digitSeparator {
			b.WriteByte(lit[i])
			continue
		}
		if i == 0 || i+1 >= len(lit) || !isPart(lit[i-1]) || !isPart(lit[i+1]) {
			return "", fmt.Errorf("engine: unrecognized token: %q", lit)
		}
	}
	return b.String(), nil
}
