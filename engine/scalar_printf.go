// This file implements printf and format SQL functions (identical functions).
// Supports printf dialect flags, width, precision, and conversions.
//     treated as NULL for that conversion, not an error (verified:
//     printf('%d %s',1) == "1 ", the %s silently getting NULL/"").
//   - %n emits nothing and does NOT consume an argument (verified via
//     printf('%n%d',1,2) == "1", not "2").
//   - %c takes the FIRST CHARACTER of the argument's own TEXT rendering
//     (NOT a codepoint-to-character conversion the way char() is) --
//     verified: printf('%c',65) == "6" (from text "65"), not "A". A
//     precision on %c REPEATS that character precision times (verified:
//     printf('%.5c','abc') == "aaaaa").
//   - For the INTEGER family (d/i/u/o/x/X) specifically, C SQLite's
//     zero-flag ('0') quirk: if present, it wins over BOTH '-' (always
//     zero-pads, never left-justifies) AND any given precision (precision
//     is simply ignored -- verified: printf('%010.3d',5) == "0000000005",
//     not the space-padded, precision-respecting "       005" a precision-
//     aware zero-flag would give). This quirk is specific to the integer
//     family: %f/%e/%g/%s/%c all follow the ordinary C convention where
//     '-' overrides '0' and precision is independent of the zero flag
//     (verified: printf('%-010f',3.5) == "3.500000  ", left-justified).
//   - %q escapes embedded single-quotes by doubling them (no outer
//     delimiters added); %w does the same for DOUBLE-quotes instead
//     (verified: printf('%w','a"b') == 'a""b'); %Q wraps in single quotes
//     AND escapes embedded ones, applied to ANY argument's own TEXT
//     rendering (verified: printf('%Q',x'4142') == "'AB'", i.e. never
//     quote()'s own X'..' blob-hex form), with NULL rendering as the bare
//     bare word NULL (no quotes) -- but %q(NULL) renders "(NULL)", WITH
//     parens, a different, also-verified special case.
package engine

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// printfMaxWidth is C SQLite's own limit on a printf() width/precision: the
// SQLITE_MAX_LENGTH string cap, reported with the same "string or blob too
// big" as an oversized zeroblob() (verified directly:
// "printf('%.*c', 1000000001, 'x')" -- zeroblob.test). A smaller safety valve
// used to reject widths C SQLite accepts.
const printfMaxWidth = sqliteMaxLength

// sqliteFPPrecisionLimit is printf.c's SQLITE_FP_PRECISION_LIMIT (100000000):
// the clamp applied to a %f/%e/%g conversion's precision before rendering,
// independent of and much smaller than printfMaxWidth.
const sqliteFPPrecisionLimit = 100_000_000

// fnPrintf implements printf()/format(): format is the first argument
// (coerced to TEXT), args the rest.
func fnPrintf(format string, args []Value) (Value, error) {
	// The FORMAT string is itself walked as a C string, so an embedded NUL
	// ends it -- verified: printf(cast(x'41002542' as text), 7) is "A", not
	// an error over the unrecognized "%B" past the NUL. The string-valued
	// conversions truncate their own arguments the same way; see
	// textBeforeNUL.
	format = textBeforeNUL(format)
	var out strings.Builder
	appended := false // anything appended yet, even an empty conversion
	argi := 0
	nextArg := func() Value {
		if argi >= len(args) {
			return Value{Typ: Null}
		}
		v := args[argi]
		argi++
		return v
	}

	i, n := 0, len(format)
	for i < n {
		c := format[i]
		if c != '%' {
			out.WriteByte(c)
			appended = true
			i++
			continue
		}
		j := i + 1
		if j >= n {
			// A bare trailing '%' with nothing after it at all renders as
			// a literal '%' (printf.c:255-258) -- verified directly
			// (printf('%') == "%") -- distinct from a specifier that starts
			// parsing (flags/width/precision) and then runs off the end,
			// which is etINVALID below.
			out.WriteByte('%')
			appended = true
			i = j
			continue
		}
		if format[j] == '%' {
			out.WriteByte('%')
			appended = true
			i = j + 1
			continue
		}

		var flagMinus, flagPlus, flagSpace, flagZero, flagAlt, flagComma, flagBang bool
	flagLoop:
		for j < n {
			switch format[j] {
			case '-':
				flagMinus = true
			// One flag_prefix holds both, so the later of '+' and ' ' wins
			// (printf.c:269-270).
			case '+':
				flagPlus, flagSpace = true, false
			case ' ':
				flagSpace, flagPlus = true, false
			case '0':
				flagZero = true
			case '#':
				flagAlt = true
			case ',':
				flagComma = true
			case '!':
				flagBang = true
			default:
				break flagLoop
			}
			j++
		}

		// A '*' width or precision is "(int)getIntArg()" (printf.c:308, 331):
		// sqlite3_value_int64 cut to its low 32 bits, so a width of 4294967297
		// is 1. A negative width left-justifies, and a negative PRECISION is
		// its magnitude, not "absent" -- "%.*f" of -3 is three places
		// (printf.c:335-337). Literal digits accumulate in an unsigned int and
		// keep the low 31 bits (printf.c:287-293, 341-346).
		width := 0
		if j < n && format[j] == '*' {
			w := int(int32(sqliteValueInt64(nextArg())))
			if w < 0 {
				flagMinus = true
				if w >= -2147483647 {
					w = -w
				} else {
					w = 0
				}
			}
			width = w
			j++
		} else {
			var wx uint32
			for j < n && format[j] >= '0' && format[j] <= '9' {
				wx = wx*10 + uint32(format[j]-'0')
				j++
			}
			width = int(wx & 0x7fffffff)
		}
		if width > printfMaxWidth {
			return Value{}, fmt.Errorf("engine: string or blob too big")
		}

		prec := -1
		if j < n && format[j] == '.' {
			j++
			if j < n && format[j] == '*' {
				prec = int(int32(sqliteValueInt64(nextArg())))
				if prec < 0 {
					if prec >= -2147483647 {
						prec = -prec
					} else {
						prec = -1
					}
				}
				j++
			} else {
				var px uint32
				for j < n && format[j] >= '0' && format[j] <= '9' {
					px = px*10 + uint32(format[j]-'0')
					j++
				}
				prec = int(px & 0x7fffffff)
			}
		}
		// The LENGTH MODIFIER "l"/"ll", which SQLite parses and then IGNORES:
		// every integer it formats is already 64-bit, so the modifier can only
		// ever be redundant (sqlite3_str_vappendf reads one 'l', then a second,
		// and sets flag_long -- which nothing downstream of the etRADIX cases
		// consults for an sqlite3_value argument). Verified directly against
		// 3.53.3, each pair identical:
		//
		//	%lld / %d   of 314159.2653      314159
		//	%lld / %d   of 12345678901234   12345678901234
		//	%llx / %x   of 255              ff
		//	%llu / %u   of -1               18446744073709551615
		//	%lf  / %f   of 1.5              1.500000
		//	%lls / %s   of 'x'              x
		//
		// At most two: a third 'l' is itself the conversion character, and
		// an invalid one (printf.c:276-285).
		for k := 0; k < 2 && j < n && format[j] == 'l'; k++ {
			j++
		}

		// Any character fmtinfo[] (printf.c:94-117) does not list -- "F", "h",
		// a '$' positional marker, the end of the string mid-specifier, and
		// the internal-only %T/%S -- is etINVALID, and printf.c:1033-1036
		// simply RETURNS: the output stops there, keeping everything
		// rendered so far. The result is NULL only when nothing was appended
		// yet, because printfFunc hands sqlite3StrAccumFinish's still-nil
		// buffer to sqlite3_result_text (func.c:326-332; see the "printf"
		// case in scalar_call.go). Verified against 3.53.3: printf('%F',1.5)
		// and printf('%-') are NULL, printf('a%F',1.5) and printf('a%-')
		// are 'a'.
		if j >= n || strings.IndexByte("dsgzqQwcouxXfeEGin%pr", format[j]) < 0 {
			if !appended {
				return Value{Typ: Null}, nil
			}
			return Value{Typ: Text, S: []byte(out.String())}, nil
		}
		verb := format[j]
		j++

		// The FLOAT verbs clamp their own precision (printfFloat); every
		// other verb keeps the ordinary too-big check.
		if prec > printfMaxWidth && strings.IndexByte("feEgG", verb) < 0 {
			return Value{}, fmt.Errorf("engine: string or blob too big")
		}

		field, err := printfField(verb, flagMinus, flagPlus, flagSpace, flagZero, flagAlt, flagComma, flagBang, width, prec, out.Len(), nextArg)
		if err != nil {
			return Value{}, err
		}
		out.WriteString(field)
		appended = true
		i = j
	}
	return Value{Typ: Text, S: []byte(out.String())}, nil
}

// printfField renders exactly one %-conversion's field (post flag/width/
// precision parsing), including width padding/justification. nChar is the
// length of the output rendered before it.
func printfField(verb byte, flagMinus, flagPlus, flagSpace, flagZero, flagAlt, flagComma, flagBang bool, width, prec, nChar int, nextArg func() Value) (string, error) {
	if strings.IndexByte("feEgG", verb) >= 0 {
		var prefix byte
		if flagPlus {
			prefix = '+'
		} else if flagSpace {
			prefix = ' '
		}
		return printfFloat(verb, sqliteValueDouble(nextArg()), width, prec, flagMinus, prefix, flagAlt, flagBang, flagZero, flagComma, nChar)
	}
	// The "," flag groups the INTEGER digits in threes. Among the integer
	// verbs it applies to the DECIMAL ones only -- verified directly against
	// mattn/go-sqlite3: "%,d"/"%,i"/"%,u" of 1234567 all render "1,234,567",
	// while the non-decimal "%,o" (4553207) and "%,x"/"%,X" (12d687/12D687),
	// and "%,s", are all left UNGROUPED (printf.c:417, cThousand=0 for
	// etRADIX). The FLOAT verbs group inside printfFloat.
	// Zero-padding goes in BEFORE the separators here, where the FLOAT verbs
	// group first and pad after -- both verified directly:
	//
	//	%,012d of 1234    "000,000,001,234"   pad to 12, THEN group
	//	%,015f of 1234.5  "0001,234.500000"   group, THEN pad to 15
	//	%,08d  of 1234    "00,001,234"        pad to 8, then group -- and the
	//	                                      result OVERSHOOTS the width
	if flagComma {
		switch verb {
		case 'd', 'i', 'u':
			padded := printfInt(nextArg(), 10, verb == 'u', flagMinus, flagPlus && verb != 'u', flagSpace && verb != 'u', flagZero, false, zeroPadWidth(flagZero, width), prec)
			return printfGroupNumeric(padded, flagMinus, width), nil
		}
	}
	switch verb {
	case 'd', 'i':
		return printfInt(nextArg(), 10, false, flagMinus, flagPlus, flagSpace, flagZero, false, width, prec), nil
	case 'u':
		return printfInt(nextArg(), 10, true, flagMinus, false, false, flagZero, false, width, prec), nil
	case 'o':
		return printfInt(nextArg(), 8, true, flagMinus, false, false, flagZero, flagAlt, width, prec), nil
	case 'x':
		return printfInt(nextArg(), 16, true, flagMinus, false, false, flagZero, flagAlt, width, prec), nil
	case 'X':
		return strings.ToUpper(printfInt(nextArg(), 16, true, flagMinus, false, false, flagZero, flagAlt, width, prec)), nil
	case 'p':
		// %p is base 16 like %x/%X, but its OWN unique combination of the
		// two: UPPERCASE digits (printf.c's fmtinfo row 20, "{'p', 16, 0,
		// etPOINTER, charset=0, prefix=1, 0}" -- charset 0 indexes aDigits'
		// uppercase half, same as %X) with a LOWERCASE "0x" alternate-form
		// prefix (prefix=1 indexes aPrefix at 'x','0' -- the SAME prefix
		// offset %x itself uses, NOT %X's own prefix=4 "0X"). Verified
		// directly: printf('%p',255) is "FF" and printf('%#p',255) is
		// "0xFF" -- uppercase digits, lowercase prefix, on the SAME call
		// where %#X would give "0XFF" (uppercased throughout). Building it
		// as %x then uppercasing and fixing the one "0X" back to "0x" is
		// safe: sign is always empty (unsigned), and padding is spaces/'0'
		// (case-invariant), so "0X" can appear at most once, right after
		// any padding, nowhere else in the string.
		return strings.Replace(strings.ToUpper(printfInt(nextArg(), 16, true, flagMinus, false, false, flagZero, flagAlt, width, prec)), "0X", "0x", 1), nil
	case '%':
		// etPERCENT (printf.c:770-774): a literal '%' that still takes a
		// width, as in "%5%". The bare "%%" never reaches here.
		return padString("%", width, flagMinus), nil
	case 's':
		return printfStringField(nextArg(), prec, width, flagMinus, flagBang, nil), nil
	case 'c':
		v := nextArg()
		// An argument with NO first character -- SQL NULL, or an EMPTY
		// string -- yields a NUL BYTE, not nothing: verified directly,
		// hex(printf('%c', NULL)) and hex(printf('%c', '')) are both "00",
		// and hex(printf('%.3c','')) is "000000" (the precision repeats the
		// NUL, exactly as it repeats an ordinary character). That matches
		// char(NULL)'s own NULL-as-codepoint-0 rule (fnChar's doc comment)
		// rather than %s/%q/%w's NULL-as-empty-string rule, and it applies
		// to the empty string too -- which is why this is not simply a
		// v.Typ == Null test.
		ch := "\x00"
		if s := valueToText(v); v.Typ != Null {
			if _, sz := utf8.DecodeRuneInString(s); sz > 0 {
				ch = s[:sz]
			}
		}
		count := 1
		if prec >= 0 {
			count = prec
		}
		// %c is the ONE verb whose width counts CHARACTERS, not bytes:
		// verified directly, printf('%8c', char(11106)) pads a single 3-byte
		// character with SEVEN spaces, and printf('%5.3c', char(1492)) pads
		// three 2-byte characters with TWO (printf.test). Every string verb
		// below pads by byte -- see padString.
		return padRunes(strings.Repeat(ch, count), width, flagMinus), nil
	case 'q':
		// "%#q" applies unistr()-style backslash escapes to every control
		// character and to backslash itself -- and for %q, unlike %Q, it does
		// so whether or not any control character is present (SQLite's own
		// "if( nCtrl || xtype==etESCAPE_q )"). See unistrEscapeBody.
		return printfStringField(printfEscapeNull(nextArg(), false), prec, width, flagMinus, flagBang, func(s string) string {
			if flagAlt {
				return unistrEscapeBody(s)
			}
			return escapeDouble(s, '\'')
		}), nil
	case 'w':
		return printfStringField(printfEscapeNull(nextArg(), false), prec, width, flagMinus, flagBang, func(s string) string {
			return escapeDouble(s, '"')
		}), nil
	case 'z':
		return printfStringField(nextArg(), prec, width, flagMinus, flagBang, nil), nil
	case 'Q':
		v := nextArg()
		if v.Typ == Null {
			// needQuote is "!isnull && xtype==etSQLESCAPE2" (printf.c), so a
			// NULL is the bare word -- but it is still a STRING FIELD, and
			// precision truncates it: "%.2Q" of NULL is "NU".
			return printfStringField(printfEscapeNull(v, true), prec, width, flagMinus, flagBang, nil), nil
		}
		// "%#Q" wraps the literal in unistr(...) with backslash escapes, but
		// ONLY when the string actually holds a control character: SQLite
		// clears the alternate-form flag when it finds none, so a merely
		// non-ASCII string quotes plainly. unistr_quote(X) is this exact
		// conversion (scalar_funcs.go's fnUnistrQuote).
		return printfStringField(v, prec, width, flagMinus, flagBang, func(s string) string {
			if flagAlt && hasControlByte(s) {
				return "unistr('" + unistrEscapeBody(s) + "')"
			}
			return "'" + escapeDouble(s, '\'') + "'"
		}), nil
	case 'n':
		return "", nil // consumes no argument; see file doc comment
	default:
		return "", fmt.Errorf("engine: printf(): unsupported conversion %%%c", verb)
	}
}

// printfEscapeNull is the %q/%Q/%w substitution for a NULL argument:
// "isnull = escarg==0; if( isnull ) escarg = (xtype==etSQLESCAPE2 ? \"NULL\" :
// \"(NULL)\");" (printf.c). It is the one place these three verbs differ from
// %s, which renders a NULL as the EMPTY string -- and this engine rendered
// %q and %w that way too, so printf('%q', NULL) was '' where C SQLite
// says '(NULL)'.
func printfEscapeNull(v Value, isQ bool) Value {
	if v.Typ != Null {
		return v
	}
	if isQ {
		return Value{Typ: Text, S: []byte("NULL")}
	}
	return Value{Typ: Text, S: []byte("(NULL)")}
}

// printfFloat is the etFLOAT/etEXP/etGENERIC arm of sqlite3_str_vappendf
// (printf.c:528-763) for verb f, e, E, g or G, including its own width
// padding: every REAL SQLite renders as text goes through here --
// REAL->TEXT is "%!.17g" (formatFloatText), quote() and JSON are "%!0.17g",
// round() is "%!.*f". prefix is flag_prefix ('+', ' ' or 0) and nChar the
// output already accumulated, which the too-big check counts.
//
// The '!' flag (flagAltForm2) lets sqliteFpDecode keep 20 significant digits
// instead of 16, trims trailing zeros from %f and %e, and always keeps one
// digit after the decimal point: "%!f" of 42 is "42.0", "%!.20g" of 0.1 is
// "0.1000000000000000056".
func printfFloat(verb byte, realvalue float64, width, precision int, flagLeftJustify bool, prefix byte, flagAlternateForm, flagAltForm2, flagZeroPad, cThousand bool, nChar int) (string, error) {
	const (
		etFLOAT = iota + 1
		etEXP
		etGENERIC
	)
	xtype := etFLOAT
	switch verb {
	case 'e', 'E':
		xtype = etEXP
	case 'g', 'G':
		xtype = etGENERIC
	}
	// fmtinfo's charset (printf.c:96-106): 'E' and 'G' index aDigits' upper
	// half.
	expChar := byte('e')
	if verb == 'E' || verb == 'G' {
		expChar = 'E'
	}

	if precision < 0 {
		precision = 6
	}
	// SQLITE_FP_PRECISION_LIMIT (printf.c:186-189, 542-546): the #ifndef
	// guards a differently-named macro, so the clamp is always compiled in --
	// printf('%.*g',2147483647,0.01) is "0.01", not an error.
	if precision > sqliteFPPrecisionLimit {
		precision = sqliteFPPrecisionLimit
	}
	var iRound int
	switch xtype {
	case etFLOAT:
		iRound = -precision
	case etGENERIC:
		if precision == 0 {
			precision = 1
		}
		iRound = precision
	default:
		iRound = precision + 1
	}
	mxRound := 16
	if flagAltForm2 {
		mxRound = 20
	}
	s := sqliteFpDecode(realvalue, iRound, mxRound)
	if s.isSpecial != 0 {
		// printf.c:556-578. Under the '0' flag an infinity is instead the
		// digit 9 at decimal exponent 1000, rendered below like any number:
		// "%!0.17g" of an infinity is "9.0e+999".
		var word string
		switch {
		case s.isSpecial == 2 && flagZeroPad:
			word = "null"
		case s.isSpecial == 2:
			word = "NaN"
		case flagZeroPad:
			s.z = []byte{'9'}
			s.iDP = 1000
			s.n = 1
		case s.sign == '-':
			word = "-Inf"
		case prefix != 0:
			word = string(prefix) + "Inf"
		default:
			word = "Inf"
		}
		if word != "" {
			// The "break" to the common tail (printf.c:1046-1053): spaces only.
			return padString(word, width, flagLeftJustify), nil
		}
	}
	if s.sign == '-' {
		// A '-' is dropped only when '#' is set, '+' is not, the conversion
		// is %f, and the value displayed is zero (printf.c:579-596).
		if !flagAlternateForm || prefix != 0 || xtype != etFLOAT || s.iDP > iRound {
			prefix = '-'
		} else {
			prefix = 0
		}
	}

	exp := s.iDP - 1
	var flagRtz bool
	if xtype == etGENERIC {
		precision--
		flagRtz = !flagAlternateForm
		if exp < -4 || exp > precision {
			xtype = etEXP
		} else {
			precision -= exp
			xtype = etFLOAT
		}
	} else {
		flagRtz = flagAltForm2
	}
	e2 := 0
	if xtype != etEXP {
		e2 = s.iDP - 1
	}

	// szBufNeeded (printf.c:624-640): the accumulator is enlarged by this
	// much before rendering, and sqlite3StrAccumEnlarge reports SQLITE_TOOBIG
	// once nChar+N+1 passes SQLITE_LIMIT_LENGTH (printf.c:1121-1131) -- even
	// when the rendering itself would have been shorter.
	szBufNeeded := int64(max(e2, 0)) + int64(precision) + int64(width) + 10
	if cThousand && e2 > 0 {
		szBufNeeded += int64((e2 + 2) / 3)
	}
	if int64(nChar)+szBufNeeded+1 > sqliteMaxLength {
		return "", fmt.Errorf("engine: string or blob too big")
	}

	flagDp := precision > 0 || flagAlternateForm || flagAltForm2
	buf := make([]byte, 0, 32)
	if prefix != 0 {
		buf = append(buf, prefix)
	}
	// Digits prior to the decimal point.
	j := 0
	switch {
	case e2 < 0:
		buf = append(buf, '0')
	case cThousand:
		for ; e2 >= 0; e2-- {
			if j < s.n {
				buf = append(buf, s.z[j])
				j++
			} else {
				buf = append(buf, '0')
			}
			if e2%3 == 0 && e2 > 1 {
				buf = append(buf, ',')
			}
		}
	default:
		j = min(e2+1, s.n)
		buf = append(buf, s.z[:j]...)
		e2 -= j
		if e2 >= 0 {
			buf = append(buf, strings.Repeat("0", e2+1)...)
			e2 = -1
		}
	}
	if flagDp {
		buf = append(buf, '.')
	}
	// "0" digits after the decimal point but before the first significant
	// digit.
	if e2 < -1 && precision > 0 {
		nn := min(-1-e2, precision)
		buf = append(buf, strings.Repeat("0", nn)...)
		precision -= nn
	}
	// Significant digits after the decimal point.
	if precision > 0 {
		if nn := min(s.n-j, precision); nn > 0 {
			buf = append(buf, s.z[j:j+nn]...)
			precision -= nn
		}
		if precision > 0 && !flagRtz {
			buf = append(buf, strings.Repeat("0", precision)...)
		}
	}
	// Remove trailing zeros, and the "." if no digits follow it -- except
	// that '!' keeps one "0" there.
	if flagRtz && flagDp {
		for len(buf) > 0 && buf[len(buf)-1] == '0' {
			buf = buf[:len(buf)-1]
		}
		if len(buf) > 0 && buf[len(buf)-1] == '.' {
			if flagAltForm2 {
				buf = append(buf, '0')
			} else {
				buf = buf[:len(buf)-1]
			}
		}
	}
	if xtype == etEXP {
		exp = s.iDP - 1
		buf = append(buf, expChar)
		if exp < 0 {
			buf = append(buf, '-')
			exp = -exp
		} else {
			buf = append(buf, '+')
		}
		if exp >= 100 {
			buf = append(buf, byte(exp/100)+'0')
			exp %= 100
		}
		buf = append(buf, byte(exp/10)+'0', byte(exp%10)+'0')
	}

	if nPad := width - len(buf); nPad > 0 {
		switch {
		case flagLeftJustify:
			buf = append(buf, strings.Repeat(" ", nPad)...)
		case !flagZeroPad:
			buf = append([]byte(strings.Repeat(" ", nPad)), buf...)
		default:
			// Zeros go after the sign, and after grouping: "%,015f" of 1234.5
			// is "0001,234.500000" (printf.c:736-744).
			adj := 0
			if prefix != 0 {
				adj = 1
			}
			buf = append(buf[:adj:adj], append([]byte(strings.Repeat("0", nPad)), buf[adj:]...)...)
		}
	}
	return string(buf), nil
}

// escapeDouble doubles every occurrence of quoteChar in s (the shared
// escaping rule behind %q's ', %w's ", and %Q's '), adding no delimiters of
// its own.
func escapeDouble(s string, quoteChar byte) string {
	if strings.IndexByte(s, quoteChar) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == quoteChar {
			b.WriteByte(quoteChar)
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// truncatePrecision truncates s to its first n BYTES (n<0 means no
// truncation -- precision omitted).
func truncatePrecision(s string, n int) string {
	if n < 0 {
		return s
	}
	if n >= len(s) {
		return s
	}
	// SQLite measures a %s/%q/%Q/%w precision in BYTES, not characters, and
	// will happily cut a multi-byte character in half: verified directly,
	// printf('%.6s', 'הנה מה־טוב') is 'הנה' (three 2-byte characters),
	// printf('%.5s', 'הנה') keeps two and a half, and printf('%.1s','הנה')
	// yields a lone continuation byte. printf2.test asserts exactly this.
	return s[:n]
}

// padString pads/justifies s to width with spaces (never zeros -- used by
// the non-numeric conversions, which never honor the zero flag).
// zeroPadWidth is the width to hand the INTEGER renderer when the "," flag is
// active: the zeros go in before the separators do, and the grouped result may
// then overshoot the width ("%,08d" of 1234 is the ten-character
// "00,001,234"). Space padding, by contrast, always applies to the
// ALREADY-grouped string ("%,12d" of 1234567 is "   1,234,567", not
// "     1,234,567"), so it is withheld here and applied afterwards by
// printfGroupNumeric.
func zeroPadWidth(flagZero bool, width int) int {
	if flagZero {
		return width
	}
	return 0
}

// printfStringField renders one STRING-family verb (%s/%q/%Q/%w/%z): the
// argument's C-string form (textBeforeNUL), cut to prec, decorated by wrap
// (nil for none), then padded to width.
//
// The "!" flag switches BOTH the precision and the width from BYTES to
// CHARACTERS, and it applies to EVERY verb here -- not just %s. Verified
// directly against mattn/go-sqlite3 with a Hebrew (2-byte-per-character)
// string: "%.3s" cuts 3 BYTES and leaves a mangled half character while
// "%!.3s" keeps 3 whole ones; "%!.3q"/"%!.3w"/"%!.3z" do the same; "%!.3Q"
// is "'הנה'"; and the width counts characters of the DECORATED result, so
// "%!7.3Q" pads to seven characters COUNTING BOTH QUOTES, and "%!9.3q" pads
// three characters out with six spaces.
func printfStringField(v Value, prec, width int, flagMinus, flagBang bool, wrap func(string) string) string {
	s := textBeforeNUL(valueToText(v))
	if flagBang {
		s = truncatePrecisionRunes(s, prec)
	} else {
		s = truncatePrecision(s, prec)
	}
	if wrap != nil {
		s = wrap(s)
	}
	if flagBang {
		return padRunes(s, width, flagMinus)
	}
	return padString(s, width, flagMinus)
}

// printfGroupNumeric inserts the "," flag's separators into an integer
// rendering's run of digits (skipping any sign), then space-pads the result
// out to width -- printfInt has already applied any zero padding.
func printfGroupNumeric(s string, flagMinus bool, width int) string {
	start := 0
	for start < len(s) && (s[start] == '-' || s[start] == '+' || s[start] == ' ') {
		start++
	}
	end := start
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if n := end - start; n > 3 {
		var b strings.Builder
		b.WriteString(s[:start])
		for i, c := range []byte(s[start:end]) {
			if i > 0 && (n-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteByte(c)
		}
		b.WriteString(s[end:])
		s = b.String()
	}
	return padString(s, width, flagMinus)
}

// truncatePrecisionRunes is truncatePrecision counting CHARACTERS instead of
// bytes, for the "!" flag -- see printfField's %s case.
func truncatePrecisionRunes(s string, n int) string {
	if n < 0 || n >= len(s) {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func padRunes(s string, width int, left bool) string {
	pad := width - utf8.RuneCountInString(s)
	if pad <= 0 {
		return s
	}
	if left {
		return s + strings.Repeat(" ", pad)
	}
	return strings.Repeat(" ", pad) + s
}

// padString pads a STRING verb's rendering to width, counting BYTES -- see
// padRunes for %c, the one verb that counts characters instead.
func padString(s string, width int, left bool) string {
	// Width, like precision, is a BYTE count in SQLite for %s/%q/%Q/%w:
	// printf('%8s','הנה') pads a 6-byte string with TWO spaces, not five.
	pad := width - len(s)
	if pad <= 0 {
		return s
	}
	if left {
		return s + strings.Repeat(" ", pad)
	}
	return strings.Repeat(" ", pad) + s
}

// printfInt renders an integer-family conversion (d/i/u/o/x/X, the base
// selecting the digit radix and unsigned selecting whether a negative
// INTEGER argument's magnitude is taken via two's-complement bit
// reinterpretation (u/o/x/X) or its ordinary signed absolute value (d/i)).
// See this file's doc comment for the verified, non-standard zero-flag
// quirk this replicates: zero flag, if present, overrides BOTH '-' and any
// given precision outright (precision is simply not applied at all in that
// case), always producing a zero-padded, right-justified field.
func printfInt(v Value, base int, unsigned bool, flagMinus, flagPlus, flagSpace, flagZero, flagAlt bool, width, prec int) string {
	iv := valueToInt64Trunc(v)
	var sign string
	var mag uint64
	if unsigned {
		mag = uint64(iv)
	} else if iv < 0 {
		sign = "-"
		mag = uint64(-(iv + 1)) + 1 // avoids overflow at math.MinInt64
	} else {
		mag = uint64(iv)
		if flagPlus {
			sign = "+"
		} else if flagSpace {
			sign = " "
		}
	}
	digits := strconv.FormatUint(mag, base)

	altPrefix := ""
	if flagAlt && mag != 0 {
		switch base {
		case 8:
			if !strings.HasPrefix(digits, "0") {
				altPrefix = "0"
			}
		case 16:
			altPrefix = "0x"
		}
	}

	if flagZero {
		// Verified, non-standard: the width budget here counts only
		// sign+digits -- any '#' alt-form prefix is inserted AFTER the
		// zero-padding is sized, so it extends the field beyond width
		// rather than eating into the padding (printf('%#010x',255) ==
		// "0x00000000ff", 12 chars for a "width" of 10).
		pad := width - len(sign+digits)
		if pad > 0 {
			return sign + altPrefix + strings.Repeat("0", pad) + digits
		}
		return sign + altPrefix + digits
	}

	if prec >= 0 && len(digits) < prec {
		digits = strings.Repeat("0", prec-len(digits)) + digits
	}
	body := sign + altPrefix + digits
	pad := width - len(body)
	if pad <= 0 {
		return body
	}
	if flagMinus {
		return body + strings.Repeat(" ", pad)
	}
	return strings.Repeat(" ", pad) + body
}

// valueToFloatLoose converts v to float64 using the same lenient
// (longest-numeric-prefix, NULL/unparseable-as-0) coercion as arithmetic.
func valueToFloatLoose(v Value) float64 {
	isF, i, f := toNumericLoose(v)
	if isF {
		return f
	}
	return float64(i)
}
