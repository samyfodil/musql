// sqlean's regexp family (src/regexp): regexp(), which "X REGEXP Y" calls,
// regexp_like, regexp_substr, regexp_capture and regexp_replace.
//
// sqlean compiles patterns with PCRE2 (UTF and UCP); this runs them on Go's
// RE2 engine, after rewriting \d, \D, \w and \W to their Unicode classes as
// UCP has them. What RE2 does not have is refused as an invalid pattern:
// backreferences (\1), lookahead and lookbehind, atomic groups and possessive
// quantifiers, and the (?x) extended mode. Two PCRE2 behaviors differ without
// an error: \b is a boundary between ASCII word characters only, and $
// outside (?m) matches only at the very end, not also before a trailing
// newline. The replacement string
// follows PCRE2's extended syntax (regexpExpand).
package engine

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

func init() {
	sqleanFamilies["regexp"] = &sqleanFamily{scalars: []sqleanScalar{
		{name: "regexp_like", lo: 2, hi: 2, fn: func(args []Value) (Value, error) { return regexpLike(args[0], args[1]) }},
		{name: "regexp_substr", lo: 2, hi: 2, fn: regexpCapture},
		{name: "regexp_capture", lo: 2, hi: 3, fn: regexpCapture},
		{name: "regexp_replace", lo: 3, hi: 3, fn: regexpReplace},
		{name: "regexp", lo: 2, hi: 2, fn: func(args []Value) (Value, error) { return regexpLike(args[1], args[0]) }},
	}}
}

// regexpText is arg_text: a BLOB is no text (NULL), a number is its text. ok
// is false for a BLOB or invalid UTF-8 (REGEXP_INVALID), both NULL.
func regexpText(v Value) (string, bool) {
	if v.Typ == Blob {
		return "", false
	}
	s := sqleanBytes(v)
	return string(s), utf8.Valid(s)
}

// regexpCache holds compiled patterns, as sqlean keeps one per call site in
// auxdata.
var regexpCache struct {
	sync.Mutex
	m          map[string]*regexp.Regexp
	positional map[*regexp.Regexp]*regexpPos
}

// regexpCompile compiles a pattern, or refuses it with sqlean's "invalid
// pattern: " error.
func regexpCompile(v Value) (*regexp.Regexp, bool, error) {
	p, ok := regexpText(v)
	if !ok {
		return nil, false, nil
	}
	regexpCache.Lock()
	re := regexpCache.m[p]
	regexpCache.Unlock()
	if re != nil {
		return re, true, nil
	}
	re, err := regexp.Compile(regexpUCP(p))
	if err != nil {
		msg := err.Error()
		if e, ok := err.(interface{ Unwrap() error }); ok && e.Unwrap() != nil {
			msg = e.Unwrap().Error()
		}
		return nil, false, fmt.Errorf("invalid pattern: %s", strings.TrimPrefix(msg, "error parsing regexp: "))
	}
	regexpCache.Lock()
	if regexpCache.m == nil || len(regexpCache.m) >= 256 {
		regexpCache.m = map[string]*regexp.Regexp{}
	}
	regexpCache.m[p] = re
	regexpCache.Unlock()
	return re, true, nil
}

// regexpUCP rewrites the PCRE2_UCP classes RE2 reads as ASCII: \d is \p{Nd},
// \w is \p{L}, \p{N}, \p{Mn}, \p{Pc}, and their negations. Inside a bracket
// \W, which would need a negated class within a class, is left as RE2 has it.
func regexpUCP(p string) string {
	const word = `\p{L}\p{N}\p{Mn}\p{Pc}`
	if !strings.Contains(p, `\`) {
		return p
	}
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			i++
			switch e := p[i]; {
			case e == 'd':
				b.WriteString(`\p{Nd}`)
			case e == 'D' && !inClass:
				b.WriteString(`\P{Nd}`)
			case e == 'w' && inClass:
				b.WriteString(word)
			case e == 'w':
				b.WriteString(`[` + word + `]`)
			case e == 'W' && !inClass:
				b.WriteString(`[^` + word + `]`)
			default:
				b.WriteByte('\\')
				b.WriteByte(e)
			}
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
			// A ']' right after '[' or '[^' is a literal.
			if i+1 < len(p) && p[i+1] == '^' {
				i++
				b.WriteByte('^')
			}
			if i+1 < len(p) && p[i+1] == ']' {
				i++
				b.WriteByte(']')
			}
		case c == ']' && inClass:
			inClass = false
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// regexpLike is fn_like: whether the pattern matches somewhere in s.
func regexpLike(sv, pv Value) (Value, error) {
	if sv.Typ == Null || pv.Typ == Null {
		return sqleanNull, nil
	}
	s, ok := regexpText(sv)
	if !ok {
		return sqleanNull, nil
	}
	re, ok, err := regexpCompile(pv)
	if !ok {
		return sqleanNull, err
	}
	if re.MatchString(s) {
		return sqleanInt(1), nil
	}
	return sqleanInt(0), nil
}

// regexpCapture is fn_capture: the text of a group (0, the whole match, by
// default) of the first match, NULL for no match or a group that did not
// take part in it.
func regexpCapture(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := regexpText(args[0])
	if !ok {
		return sqleanNull, nil
	}
	var group int64
	if len(args) == 3 {
		g := args[2]
		if g.Typ == Blob {
			return sqleanNull, nil
		}
		isInt, n, _, ok := mathNumericType(g)
		if !ok || !isInt || n < 0 {
			return Value{}, fmt.Errorf("invalid group: %s", valueToText(g))
		}
		group = n
	}
	re, ok, err := regexpCompile(args[1])
	if !ok {
		return sqleanNull, err
	}
	if group > int64(re.NumSubexp()) {
		return Value{}, fmt.Errorf("invalid group: %s", valueToText(args[2]))
	}
	m := re.FindStringSubmatchIndex(s)
	if m == nil || m[2*group] < 0 {
		return sqleanNull, nil
	}
	return sqleanText(s[m[2*group]:m[2*group+1]]), nil
}

// regexpReplace is fn_replace: every match replaced.
func regexpReplace(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := regexpText(args[0])
	if !ok {
		return sqleanNull, nil
	}
	repl, ok := regexpText(args[2])
	if !ok {
		return sqleanNull, nil
	}
	re, ok, err := regexpCompile(args[1])
	if !ok {
		return sqleanNull, err
	}
	var out strings.Builder
	last := 0
	if !regexpNullable(re) {
		// No match is empty, and then Go's matches are PCRE2's.
		for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
			out.WriteString(s[last:m[0]])
			if err := regexpExpand(&out, re, repl, s, m); err != nil {
				return Value{}, err
			}
			last = m[1]
		}
		out.WriteString(s[last:])
		return sqleanText(out.String()), nil
	}
	// pcre2_substitute's PCRE2_SUBSTITUTE_GLOBAL loop. Go's FindAll drops an
	// empty match right after a match, which PCRE2 keeps ("b*" over "abc" is
	// "-a--c-"); after an empty match PCRE2 retries at the same place for a
	// non-empty one (PCRE2_NOTEMPTY_ATSTART | PCRE2_ANCHORED), which RE2 cannot
	// pick, so where one exists this declines.
	x, err := regexpPositional(re)
	if err != nil {
		return Value{}, err
	}
	for pos := 0; pos <= len(s); {
		var m []int
		if pos == 0 {
			m = re.FindStringSubmatchIndex(s)
		} else {
			_, w := utf8.DecodeLastRuneInString(s[:pos])
			if m = x.from.FindStringSubmatchIndex(s[pos-w:]); m != nil {
				m = m[2:]
				for i := range m {
					if m[i] >= 0 {
						m[i] += pos - w
					}
				}
			}
		}
		if m == nil {
			break
		}
		out.WriteString(s[last:m[0]])
		if err := regexpExpand(&out, re, repl, s, m); err != nil {
			return Value{}, err
		}
		last, pos = m[1], m[1]
		if m[0] != m[1] {
			continue
		}
		if pos == len(s) {
			break
		}
		w := 0
		if pos > 0 {
			_, w = utf8.DecodeLastRuneInString(s[:pos])
		}
		if n := x.at[min(w, 1)].FindStringIndex(s[pos-w:]); n != nil && n[1] > w {
			return Value{}, fmt.Errorf("%w: regexp_replace(): an empty match where the pattern could also match a non-empty string", errVDBEUnsupported)
		}
		_, w = utf8.DecodeRuneInString(s[pos:])
		pos += w
	}
	out.WriteString(s[last:])
	return sqleanText(out.String()), nil
}

// regexpNullable reports whether re might match an empty string, counting
// every assertion as one that holds.
func regexpNullable(re *regexp.Regexp) bool {
	t, err := syntax.Parse(re.String(), syntax.Perl)
	return err != nil || syntaxNullable(t)
}

func syntaxNullable(t *syntax.Regexp) bool {
	switch t.Op {
	case syntax.OpNoMatch, syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return false
	case syntax.OpLiteral:
		return len(t.Rune) == 0
	case syntax.OpCapture, syntax.OpPlus:
		return syntaxNullable(t.Sub[0])
	case syntax.OpRepeat:
		return t.Min == 0 || syntaxNullable(t.Sub[0])
	case syntax.OpConcat:
		for _, s := range t.Sub {
			if !syntaxNullable(s) {
				return false
			}
		}
		return true
	case syntax.OpAlternate:
		for _, s := range t.Sub {
			if syntaxNullable(s) {
				return true
			}
		}
		return false
	}
	return true
}

// regexpPositional is re searching from a position past the start of the
// text, over the text from the rune before it: that rune is consumed first so
// \b and (?m)^ see it, and the lazy run before group 1 makes group 1 the
// leftmost match from the position, as PCRE2's start_offset finds it. at[w]
// is re anchored after w runes (0 or 1), leftmost-longest, to ask whether a
// non-empty match starts there.
func regexpPositional(re *regexp.Regexp) (*regexpPos, error) {
	regexpCache.Lock()
	x := regexpCache.positional[re]
	regexpCache.Unlock()
	if x != nil {
		return x, nil
	}
	p := re.String()
	x = &regexpPos{}
	var errs [3]error
	x.from, errs[0] = regexp.Compile(`\A(?s:.)(?s:.*?)(` + p + `)`)
	x.at[0], errs[1] = regexp.Compile(`\A(?:` + p + `)`)
	x.at[1], errs[2] = regexp.Compile(`\A(?s:.)(?:` + p + `)`)
	for _, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("%w: regexp_replace(): %v", errVDBEUnsupported, err)
		}
	}
	x.at[0].Longest()
	x.at[1].Longest()
	regexpCache.Lock()
	if regexpCache.positional == nil || len(regexpCache.positional) >= 256 {
		regexpCache.positional = map[*regexp.Regexp]*regexpPos{}
	}
	regexpCache.positional[re] = x
	regexpCache.Unlock()
	return x, nil
}

type regexpPos struct {
	from *regexp.Regexp
	at   [2]*regexp.Regexp
}

// regexpExpand writes repl for one match, in PCRE2's extended substitution
// syntax (PCRE2_SUBSTITUTE_EXTENDED | PCRE2_SUBSTITUTE_UNSET_EMPTY): $n, ${n},
// $name and ${name} insert a group (empty if it did not take part), $$ a
// dollar; \U and \L change the case of what follows until \E, \u and \l of
// the next character; a backslash otherwise escapes as in a pattern.
func regexpExpand(out *strings.Builder, re *regexp.Regexp, repl, s string, m []int) error {
	bad := func(off int, msg string) error {
		return fmt.Errorf("invalid replacement: %s at offset %d", msg, off)
	}
	var caseAll, caseNext byte
	emit := func(t string) {
		for _, r := range t {
			switch {
			case caseNext == 'u':
				r = unicode.ToUpper(r)
			case caseNext == 'l':
				r = unicode.ToLower(r)
			case caseAll == 'U':
				r = unicode.ToUpper(r)
			case caseAll == 'L':
				r = unicode.ToLower(r)
			}
			caseNext = 0
			out.WriteRune(r)
		}
	}
	group := func(g int) {
		if m[2*g] >= 0 {
			emit(s[m[2*g]:m[2*g+1]])
		}
	}
	for i := 0; i < len(repl); {
		c := repl[i]
		switch c {
		case '$':
			if i+1 >= len(repl) {
				return bad(i+1, "expected closing curly bracket in replacement string")
			}
			j := i + 1
			if repl[j] == '$' {
				emit("$")
				i = j + 1
				continue
			}
			braced := repl[j] == '{'
			if braced {
				j++
			}
			k := j
			for k < len(repl) && (repl[k] == '_' || repl[k] >= '0' && repl[k] <= '9' || repl[k] >= 'a' && repl[k] <= 'z' || repl[k] >= 'A' && repl[k] <= 'Z') {
				if repl[j] >= '0' && repl[j] <= '9' && !(repl[k] >= '0' && repl[k] <= '9') {
					break
				}
				k++
			}
			name := repl[j:k]
			if name == "" {
				return bad(k, "invalid replacement string")
			}
			if braced {
				if k >= len(repl) || repl[k] != '}' {
					return bad(k, "expected closing curly bracket in replacement string")
				}
				k++
			}
			g := -1
			if n, err := strconv.Atoi(name); err == nil {
				g = n
			} else {
				g = re.SubexpIndex(name)
			}
			if g < 0 || g > re.NumSubexp() {
				return bad(k, "unknown substring")
			}
			group(g)
			i = k
		case '\\':
			if i+1 >= len(repl) {
				return bad(i+1, "\\ at end of pattern")
			}
			e := repl[i+1]
			i += 2
			switch e {
			case 'U', 'L':
				caseAll = e
			case 'E':
				caseAll, caseNext = 0, 0
			case 'u', 'l':
				caseNext = e
			case 'n':
				emit("\n")
			case 't':
				emit("\t")
			case 'r':
				emit("\r")
			case 'f':
				emit("\f")
			case 'a':
				emit("\a")
			case 'e':
				emit("\x1b")
			case 'x':
				hex := ""
				if i < len(repl) && repl[i] == '{' {
					end := strings.IndexByte(repl[i:], '}')
					if end < 0 {
						return bad(i, "missing terminating } in \\x{}")
					}
					hex, i = repl[i+1:i+end], i+end+1
				} else {
					for len(hex) < 2 && i < len(repl) && strings.IndexByte("0123456789abcdefABCDEF", repl[i]) >= 0 {
						hex += repl[i : i+1]
						i++
					}
				}
				n, err := strconv.ParseUint("0"+hex, 16, 32)
				if err != nil || n > unicode.MaxRune {
					return bad(i, "character code point value in \\x{} or \\o{} is too large")
				}
				emit(string(rune(n)))
			default:
				if e >= '0' && e <= '9' || e >= 'a' && e <= 'z' || e >= 'A' && e <= 'Z' {
					return bad(i, "unrecognized escape in replacement string")
				}
				emit(string(e))
			}
		default:
			_, size := utf8.DecodeRuneInString(repl[i:])
			emit(repl[i : i+size])
			i += size
		}
	}
	return nil
}
