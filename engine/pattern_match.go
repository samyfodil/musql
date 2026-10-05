// LIKE and GLOB matching: backtracking matchers over UTF-8.
// LIKE is case-insensitive for ASCII only; GLOB is case-sensitive with character classes.
package engine

import (
	"fmt"
)

// likeMatch implements LIKE semantics: '%' matches runs, '_' matches one character.
// The caseSensitive flag reflects the connection's "PRAGMA
// case_sensitive_like" setting (threaded from evalCtx.pager / machine.pager --
// see DB.caseSensitiveLike): false (default) folds ASCII letters
// case-insensitively; true compares every pattern/text rune by exact equality.
// Both operands are walked as C strings, so an embedded NUL ends either one
// (verified: "cast(x'410042' as text) LIKE 'A'" is 1) -- see textBeforeNUL.
func likeMatch(pattern, text string, caseSensitive bool) bool {
	return likeMatchRunes([]rune(textBeforeNUL(pattern)), []rune(textBeforeNUL(text)), 0, false, caseSensitive)
}

// likeMatchEscape is likeMatch with an ESCAPE character active: esc
// immediately preceding ANY following pattern rune (checked by EXACT rune
// equality, not the ASCII case-fold used for ordinary characters -- verified
// directly against C SQLite: with ESCAPE 'x', the pattern rune 'X' does
// NOT trigger escaping, even though 'X' and 'x' would ordinarily compare
// equal) makes that following rune literal -- both consumed as a pair, and
// the literal rune compared against the text using the SAME ASCII-fold rule
// as any ordinary pattern character (also verified directly: the escaped
// character's own comparison IS still case-folded, and this holds even when
// the escaped character is itself '%' or '_' -- e.g. ESCAPE '%' with pattern
// "abc%%" matches ONLY the literal text "abc%", since the first '%' is
// always the escape trigger and can never act as a wildcard once it's the
// escape character). An escape character with nothing following it (the
// final rune of the pattern) can never match anything -- verified directly:
// "'a\' LIKE 'a\' ESCAPE '\'" is FALSE even though the text is a
// byte-for-byte match of the pattern -- so likeMatchRunes fails the whole
// match outright the moment it reaches a dangling trailing escape, rather
// than falling through to compare it as an ordinary character.
func likeMatchEscape(pattern, text string, esc rune, caseSensitive bool) bool {
	return likeMatchRunes([]rune(textBeforeNUL(pattern)), []rune(textBeforeNUL(text)), esc, true, caseSensitive)
}

// likeRuneEqual compares one pattern rune against one text rune under the
// active case_sensitive_like setting: exact equality when caseSensitive, else
// the ASCII-fold rule LIKE uses by default. Verified directly against real
// SQLite that "PRAGMA case_sensitive_like=ON" makes even an ESCAPE-literalized
// character's own comparison case-sensitive ("'AB_C' LIKE 'ab\_c' ESCAPE '\'"
// is 0 under ON, 1 under OFF), which is why the escaped-pair branch below also
// routes through this helper.
func likeRuneEqual(a, b rune, caseSensitive bool) bool {
	if caseSensitive {
		return a == b
	}
	return runeEqualFoldASCII(a, b)
}

func likeMatchRunes(p, t []rune, esc rune, hasEsc bool, caseSensitive bool) bool {
	for len(p) > 0 {
		if hasEsc && p[0] == esc {
			if len(p) < 2 {
				return false
			}
			if len(t) == 0 || !likeRuneEqual(p[1], t[0], caseSensitive) {
				return false
			}
			p, t = p[2:], t[1:]
			continue
		}
		switch p[0] {
		case '%':
			for len(p) > 0 && p[0] == '%' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(t); i++ {
				if likeMatchRunes(p, t[i:], esc, hasEsc, caseSensitive) {
					return true
				}
			}
			return false
		case '_':
			if len(t) == 0 {
				return false
			}
			p, t = p[1:], t[1:]
		default:
			if len(t) == 0 || !likeRuneEqual(p[0], t[0], caseSensitive) {
				return false
			}
			p, t = p[1:], t[1:]
		}
	}
	return len(t) == 0
}

// likeEscapeRune validates an already-evaluated LIKE/like() ESCAPE operand.
// A NULL value is reported via isNull (r is meaningless in that case) --
// verified directly against C SQLite that ESCAPE NULL propagates like any
// other NULL LIKE operand ("SELECT 'a' LIKE 'a' ESCAPE NULL" is NULL, not an
// error) -- so callers must check isNull BEFORE deciding whether X/Pattern's
// own NULL-ness applies. Any non-NULL value is coerced with valueToText, the
// same loose TEXT-rendering LIKE's own X/Pattern operands use (verified
// directly: numeric ESCAPE 5 renders "5", a single character, and is
// accepted, while ESCAPE 5.0 renders "5.0", three characters, and is
// rejected exactly like any other multi-character ESCAPE value); anything
// that doesn't render to exactly one RUNE (not byte -- a multi-byte UTF-8
// escape character such as 'é' is fine) is the runtime error C SQLite
// raises -- verified directly, exact text "ESCAPE expression must be a
// single character" -- and, critically, this error fires regardless of
// whether X or Pattern is itself NULL or whether a surrounding WHERE would
// otherwise discard the row (verified directly: "SELECT NULL LIKE 'a' ESCAPE
// '\ab'" and "SELECT 1 WHERE 'a' LIKE 'a' ESCAPE '\ab'" both error rather
// than silently yielding NULL/no-rows), so callers must surface this error
// unconditionally rather than short-circuiting on a NULL X/Pattern first.
func likeEscapeRune(ev Value) (r rune, isNull bool, err error) {
	if ev.Typ == Null {
		return 0, true, nil
	}
	rs := []rune(textBeforeNUL(valueToText(ev)))
	if len(rs) != 1 {
		return 0, false, fmt.Errorf("engine: ESCAPE expression must be a single character")
	}
	return rs[0], false, nil
}

func runeEqualFoldASCII(a, b rune) bool {
	return a == b || asciiUpperRune(a) == asciiUpperRune(b)
}

func asciiUpperRune(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}

// globMatch implements SQLite's GLOB semantics, operating on runes (not
// bytes -- verified directly: "'café' GLOB 'caf?'" matches, one '?'
// consuming the whole multi-byte 'é' rune): '*' matches any run of zero or
// more characters, '?' matches exactly one character, and '[...]' matches a
// single character against a class (with 'a-z' ranges and '^' -- NOT '!' --
// negation; verified directly that "'abc' GLOB '[!a-c]bc'" behaves as if
// '!' were an ordinary class member, not a negation marker). Unlike LIKE,
// GLOB is always case-SENSITIVE and has no ESCAPE mechanism at all. This
// mirrors SQLite's own patternCompare (func.c) closely enough to reproduce
// its exact, sometimes-surprising edge cases, all verified directly against
// C SQLite:
//   - a literal ']' as the class's first member (immediately after '[' or
//     '[^') does not close the class, e.g. "'a]b' GLOB 'a[]]b'" matches.
//   - a literal '-' as the first class member (immediately after '[' or
//     '[^') is NOT a range operator, e.g. "'a-b' GLOB 'a[-x]b'" matches.
//   - an UNTERMINATED class (no closing ']' anywhere in the rest of the
//     pattern) makes the ENTIRE match attempt fail outright (return false),
//     not fall back to treating '[' as a literal character -- verified:
//     both "'abc[' GLOB 'abc['" and "'a' GLOB '[a'" are FALSE.
//
// Both operands are walked as C strings, exactly as in likeMatch.
func globMatch(pattern, text string) bool {
	return globMatchRunes([]rune(textBeforeNUL(pattern)), []rune(textBeforeNUL(text)))
}

func globMatchRunes(p, t []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(t); i++ {
				if globMatchRunes(p, t[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(t) == 0 {
				return false
			}
			p, t = p[1:], t[1:]
		case '[':
			if len(t) == 0 {
				return false
			}
			matched, rest, ok := globMatchClass(p, t[0])
			if !ok {
				// Unterminated bracket expression: the whole match attempt
				// fails (see doc comment above), not merely this position.
				return false
			}
			if !matched {
				return false
			}
			p, t = rest, t[1:]
		default:
			if len(t) == 0 || p[0] != t[0] {
				return false
			}
			p, t = p[1:], t[1:]
		}
	}
	return len(t) == 0
}

// globMatchClass parses the "[...]" class starting at p[0]=='[' and reports
// whether c is a member, plus p advanced past the class's closing ']' (rest)
// -- or ok=false if the class is never closed (no ']' before the pattern
// ends), in which case rest is meaningless and the caller must fail the
// whole match. Mirrors SQLite's own bracket-class scan (patternCompare,
// func.c) rune-for-rune: an optional leading '^' negates the whole class; a
// ']' or '-' appearing as the very first member (right after '[' or '[^')
// is a literal character, not the closer/a range operator; a '-' anywhere
// else, with both a preceding member and a following character that isn't
// ']', forms an inclusive range (verified: SQLite does NOT swap a
// backwards range like "[z-a]" into "[a-z]" -- it simply never matches,
// reproduced here by not swapping either).
func globMatchClass(p []rune, c rune) (matched bool, rest []rune, ok bool) {
	i := 1 // skip the leading '['
	invert := false
	if i < len(p) && p[i] == '^' {
		invert = true
		i++
	}
	seen := false
	havePrior := false
	var priorC rune
	first := true
	for {
		if i >= len(p) {
			return false, nil, false
		}
		ch := p[i]
		if ch == ']' && !first {
			i++
			break
		}
		first = false
		if ch == '-' && havePrior && i+1 < len(p) && p[i+1] != ']' {
			i++ // consume '-'
			hi := p[i]
			i++
			if c >= priorC && c <= hi {
				seen = true
			}
			havePrior = false
			continue
		}
		if ch == c {
			seen = true
		}
		priorC = ch
		havePrior = true
		i++
	}
	return seen != invert, p[i:], true
}

// ---- arithmetic ----
