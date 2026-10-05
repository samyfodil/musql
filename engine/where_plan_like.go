package engine

// Package engine optimizes LIKE and GLOB in WHERE clauses: a LIKE/GLOB with
// a literal prefix prefix gains virtual range terms for index seeks; non-wildcard
// length reduces output estimate. Parameterized patterns are refused.

// whereLikeShape describes a LIKE/GLOB term: like(pattern, x [, escape]).
type whereLikeShape struct {
	x, pattern, escape Expr
	glob, infix        bool
}

// whereLikeOf extracts a LIKE or GLOB term (operator or function call).
func whereLikeOf(e Expr) (whereLikeShape, bool) {
	switch x := e.(type) {
	case LikeExpr:
		if !x.Not {
			return whereLikeShape{x: x.X, pattern: x.Pattern, escape: x.Escape, infix: true}, true
		}
	case GlobExpr:
		if !x.Not {
			return whereLikeShape{x: x.X, pattern: x.Pattern, glob: true, infix: true}, true
		}
	case FuncExpr:
		if x.Star || x.Distinct || x.Over != nil || x.Filter != nil {
			return whereLikeShape{}, false
		}
		switch r33sFoldIdent(x.Name) {
		case "like":
			if len(x.Args) == 2 || len(x.Args) == 3 {
				s := whereLikeShape{pattern: x.Args[0], x: x.Args[1]}
				if len(x.Args) == 3 {
					s.escape = x.Args[2]
				}
				return s, true
			}
		case "glob":
			if len(x.Args) == 2 {
				return whereLikeShape{pattern: x.Args[0], x: x.Args[1], glob: true}, true
			}
		}
	}
	return whereLikeShape{}, false
}

// estLikePatternLength returns the longest non-wildcard byte run in pattern.
func estLikePatternLength(p Expr, like bool) int {
	sz := 0
	var walk func(Expr)
	walk = func(e Expr) {
		switch x := e.(type) {
		case LiteralExpr:
			if x.Val.Typ != Text {
				return
			}
			z := x.Val.S
			n := 0
			for i := 0; i < len(z); i++ {
				c := z[i]
				switch {
				case !like && c == '[':
					if i+1 < len(z) {
						i++
					}
					for i < len(z) && z[i] != ']' {
						i++
					}
				case like && (c == '%' || c == '_'), !like && (c == '*' || c == '?'):
				default:
					n++
				}
			}
			if n > sz {
				sz = n
			}
		case CollateExpr:
			walk(x.X)
		case UnaryExpr:
			walk(x.X)
		case BinaryExpr:
			walk(x.L)
			walk(x.R)
		case CastExpr:
			walk(x.X)
		case FuncExpr:
			for _, a := range x.Args {
				walk(a)
			}
		}
	}
	walk(p)
	return sz
}

// whereLikeRange is isLikeOrGlob (whereexpr.c:178) plus the bound-building
// half of exprAnalyze's LIKE block (whereexpr.c:1376-1453): the two virtual
// range terms, and isComplete -- the pattern is its prefix followed by one
// trailing % or *, which makes the pair the LIKE term's children. ok false when
// the optimization does not apply.
//
// lhsTextColumn is "pLeft is a TK_COLUMN with TEXT affinity, not of a virtual
// table": otherwise a prefix that reads as a number would make the range
// wrong, and the optimization is refused for one.
func whereLikeRange(s whereLikeShape, caseSensitiveLike, utf16le, lhsTextColumn bool) (lower, upper Expr, isComplete, ok bool) {
	// sqlite3IsLikeFunction (func.c:2398): the wildcards, the escape, and
	// whether the comparison ignores case.
	wc := [4]byte{'%', '_', 0, 0}
	noCase := !caseSensitiveLike
	if s.glob {
		wc = [4]byte{'*', '?', '[', 0}
		noCase = false
	}
	if s.escape != nil {
		lit, isLit := s.escape.(LiteralExpr)
		if !isLit || lit.Val.Typ != Text || len(lit.Val.S) != 1 ||
			lit.Val.S[0] == wc[0] || lit.Val.S[0] == wc[1] {
			return nil, nil, false, false
		}
		wc[3] = lit.Val.S[0]
	}
	pat, isLit := whereSkipCollate(s.pattern).(LiteralExpr)
	if !isLit || pat.Val.Typ != Text {
		return nil, nil, false, false
	}
	z := append([]byte(nil), pat.Val.S...)
	at := func(i int) byte {
		if i < len(z) {
			return z[i]
		}
		return 0
	}
	// The non-wildcard prefix, in bytes: escapes consumed, stopping at a
	// U+FFFD, malformed UTF-8, or -- in a UTF-16LE database -- any non-ASCII.
	cnt := 0
	var c byte
	for {
		c = at(cnt)
		if c == 0 || c == wc[0] || c == wc[1] || (wc[2] != 0 && c == wc[2]) {
			break
		}
		cnt++
		if wc[3] != 0 && c == wc[3] && at(cnt) > 0 && at(cnt) < 0x80 {
			cnt++
		} else if c >= 0x80 {
			r, n := utf8ReadSQLite(z[cnt-1:])
			if c == 0xff || r == 0xfffd || utf16le {
				cnt--
				break
			}
			cnt = cnt - 1 + n
		}
	}
	if !((cnt > 1 || (cnt > 0 && z[0] != wc[3])) && z[cnt-1] != 0xff) {
		return nil, nil, false, false
	}
	isComplete = c == wc[0] && at(cnt+1) == 0 && !utf16le
	var prefix []byte
	for i := 0; i < cnt; i++ {
		if wc[3] != 0 && z[i] == wc[3] {
			i++
		}
		prefix = append(prefix, z[i])
	}
	if !lhsTextColumn {
		isNum := false
		if _, rc := sqliteAtoF(prefix); rc > 0 {
			isNum = true
		} else if len(prefix) == 1 && prefix[0] == '-' {
			isNum = true
		} else {
			bumped := append([]byte(nil), prefix...)
			bumped[len(bumped)-1]++
			if _, rc := sqliteAtoF(bumped); rc > 0 {
				isNum = true
			}
		}
		if isNum {
			return nil, nil, false, false
		}
	}
	lo := append([]byte(nil), prefix...)
	hi := append([]byte(nil), prefix...)
	if noCase {
		// Upper-case below, lower-case above (upper sorts first in ASCII) so
		// the bounds also hold for a BLOB; sqlite3Toupper/Tolower are ASCII.
		for i := range lo {
			lo[i] = asciiUpperByte(lo[i])
			hi[i] = asciiLowerByte(hi[i])
		}
	}
	pc := len(hi) - 1
	if noCase {
		// Incrementing '@' would carry it into the letters, where case folding
		// breaks the inequality: run the full LIKE on every candidate instead.
		if hi[pc] == 'A'-1 {
			isComplete = false
		}
		hi[pc] = asciiLowerByte(hi[pc])
	}
	for hi[pc] == 0xbf && pc > 0 {
		hi[pc] = 0x80
		pc--
	}
	hi[pc]++
	coll := "BINARY"
	if noCase {
		coll = "NOCASE"
	}
	lower = BinaryExpr{Op: ">=", L: CollateExpr{X: s.x, Name: coll}, R: LiteralExpr{Val: Value{Typ: Text, S: lo}}}
	upper = BinaryExpr{Op: "<", L: CollateExpr{X: s.x, Name: coll}, R: LiteralExpr{Val: Value{Typ: Text, S: hi}}}
	return lower, upper, isComplete, true
}

func asciiUpperByte(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

// utf8ReadSQLite is sqlite3Utf8Read (utf.c) over one character at z[0]: the
// code point and the bytes it took, U+FFFD for an overlong or surrogate one.
func utf8ReadSQLite(z []byte) (uint32, int) {
	if len(z) == 0 {
		return 0, 0
	}
	c := uint32(z[0])
	n := 1
	if c >= 0xc0 {
		c = uint32(sqliteUtf8Trans1[c-0xc0])
		for n < len(z) && z[n]&0xc0 == 0x80 {
			c = (c << 6) + uint32(0x3f&z[n])
			n++
		}
		if c < 0x80 || c&0xFFFFF800 == 0xD800 || c&0xFFFFFFFE == 0xFFFE {
			c = 0xFFFD
		}
	}
	return c, n
}

// sqliteUtf8Trans1 is utf.c's sqlite3Utf8Trans1: the value bits of a UTF-8
// lead byte from 0xc0 up.
var sqliteUtf8Trans1 = [64]byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x00, 0x01, 0x02, 0x03, 0x00, 0x01, 0x00, 0x00,
}
