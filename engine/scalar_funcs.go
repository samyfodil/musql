// This file implements the scalar functions added to fill out SQLite 3.53.3
// conformance beyond the original core set (abs, length,
// lower, upper, substr, coalesce, ifnull, nullif, typeof, hex): round,
// sign, replace, instr, quote, char, unicode, unhex, zeroblob, randomblob,
// random, likelihood/likely/unlikely, iif, ltrim/rtrim/trim, and the
// 2-or-more-argument SCALAR forms of min/max (the 1-argument AGGREGATE form
// stays entirely in sql_agg.go -- see isAggregateCall's doc comment there).
// printf()/format() and the date/time family (date/time/datetime/
// julianday/unixepoch/strftime) each get their own file
// (scalar_printf.go, scalar_datetime.go) given their size.
//
// Every behavior here was verified directly against C SQLite
// (mattn/go-sqlite3, cgo) before being hardcoded -- see this package's
// compat-harness/vdbe_scalarfunc_test.go for the differential gate, and the
// doc comments below for the specific, sometimes-surprising verified
// findings (e.g. abs() on a TEXT/BLOB argument ALWAYS returns REAL, never
// INTEGER, even when the text parses as a bare integer -- fnAbs's own
// pre-existing default branch had this wrong; see its own updated doc
// comment).
package engine

import (
	"bytes"
	"fmt"
	"strings"
)

// textBeforeNUL returns s up to (but not including) its first NUL byte, or
// s unchanged when it has none.
//
// A SQLite TEXT value keeps its bytes verbatim -- an embedded NUL is NOT
// dropped, and every LENGTH-delimited consumer sees all of them. Verified
// directly against C SQLite with X = cast(x'410042' as text) ('A' NUL
// 'B'): hex(X) is '410042', and X's comparison, ||, upper()/lower(),
// instr(), trim()'s own subject, hex(), replace()'s replacement text and
// json_quote() all operate over all three bytes.
//
// But every consumer that reaches the value through sqlite3_value_text()
// and then walks it as a C STRING stops at the first NUL, and that is
// observable. All verified directly against mattn/go-sqlite3:
//
//	length(cast(x'0041' as text))          0, not 2
//	unicode(cast(x'0041' as text))         NULL, not 0
//	quote(X)                               'A'
//	substr(X,1,3)                          'A'
//	printf('%s',X) / '%q' / '%Q' / '%w' / '%z'
//	                                       'A' (and the width/precision
//	                                       pad the TRUNCATED string:
//	                                       printf('%5s',X) is "    A")
//	printf(cast(x'41002542' as text),7)    'A' -- the FORMAT string too
//	trim('AAB',X)                          'B' (the charset is just "A";
//	                                       a charset LEADING with a NUL
//	                                       reads empty, so trims nothing)
//	replace(cast(x'41004200' as text), cast(x'0042' as text), 'Z')
//	                                       unchanged -- replaceFunc's
//	                                       empty-pattern short-circuit is
//	                                       a C-string test (zPattern[0]==0),
//	                                       even though a needle whose NUL
//	                                       is NOT first still matches over
//	                                       full bytes
//	X LIKE 'A' / X GLOB 'A'                1 (both operands truncated)
//	'a%b' LIKE 'a#%b' ESCAPE cast(x'230041' as text)   1
//	unhex(cast(x'34310030' as text))       x'41'
//	json_valid(cast(x'7B7D00' as text))    1
//	date(cast(x'323032302D30312D303100' as text))      '2020-01-01'
//
// Call this at exactly those boundaries. NOT in valueToText, which is the
// length-delimited path everything else correctly uses.
func textBeforeNUL(s string) string {
	before, _, _ := strings.Cut(s, "\x00")
	return before
}

// sqlite3Utf8Trans1 is C SQLite's own table of the same name: the initial
// codepoint bits a lead byte 0xC0..0xFF contributes. It is reproduced verbatim
// rather than computed, because the last two entries (0xFE and 0xFF, which are
// not lead bytes in any UTF-8 revision) are 0 by fiat, not by masking.
var sqlite3Utf8Trans1 = [64]byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x00, 0x01, 0x02, 0x03, 0x00, 0x01, 0x00, 0x00,
}

// sqliteUTF8Skip is C SQLite's SQLITE_SKIP_UTF8 macro: the number of BYTES
// the character at the front of s occupies. A lead byte below 0xC0 -- including
// a bare continuation byte 0x80..0xBF, which no decoder would accept -- stands
// alone; from 0xC0 up, EVERY following continuation byte is swallowed, however
// many there are and whatever the lead byte's nominal length would be.
//
// This is deliberately NOT Go's utf8 package, and the difference is only
// visible on text that is not well-formed -- which SQLite stores and reports
// verbatim rather than sanitizing. Verified against the oracle (3.53.3):
//
//	length(cast(x'e282'     as text))  1, not 2   truncated 3-byte sequence
//	length(cast(x'f09f92'   as text))  1, not 3
//	length(cast(x'c080'     as text))  1, not 2   overlong
//	length(cast(x'eda080'   as text))  1, not 3   surrogate half
//	length(cast(x'fefe'     as text))  2          0xFE is a lead byte here,
//	                                              but 0xFE is not a
//	                                              continuation byte, so the
//	                                              second one starts a new
//	                                              character
//	length(cast(x'80'       as text))  1
//
// s must not contain the NUL that terminates the value (see textBeforeNUL);
// SQLite's own macro relies on that NUL to stop the continuation-byte loop.
func sqliteUTF8Skip(s []byte) int {
	if len(s) == 0 {
		return 0
	}
	n := 1
	if s[0] >= 0xc0 {
		for n < len(s) && s[n]&0xc0 == 0x80 {
			n++
		}
	}
	return n
}

// sqliteUTF8Next is C SQLite's READ_UTF8 macro: the codepoint of the
// character at the front of s, plus the byte count sqliteUTF8Skip would report.
// A sequence that decodes to something UTF-8 may not represent -- an overlong
// form, a surrogate half, or U+FFFE/U+FFFF -- reads as U+FFFD, but a merely
// TRUNCATED sequence keeps whatever bits it did carry, which is why
// unicode(cast(x'e282' as text)) is 130 and unicode(cast(x'f09f92' as text))
// is 2002 rather than either 65533 or an error (both verified against 3.53.3,
// as is unicode(cast(x'80' as text)) == 128 for a bare continuation byte).
func sqliteUTF8Next(s []byte) (cp int64, n int) {
	if len(s) == 0 {
		return 0, 0
	}
	c := int64(s[0])
	n = 1
	if s[0] >= 0xc0 {
		c = int64(sqlite3Utf8Trans1[s[0]-0xc0])
		for n < len(s) && s[n]&0xc0 == 0x80 {
			c = (c << 6) + int64(s[n]&0x3f)
			n++
		}
		if c < 0x80 || c&0xFFFFF800 == 0xD800 || c&0xFFFFFFFE == 0xFFFE {
			c = 0xFFFD
		}
	}
	return c, n
}

// sqliteUTF8Starts returns the byte offset of every character start in s,
// terminated by len(s) -- so the character range [i,j) spans bytes
// s[starts[i]:starts[j]] and len(starts)-1 is the character count. It is the
// one shape substr() needs: SQLite slices raw BYTES between two character
// positions, never re-encoding what it found there.
func sqliteUTF8Starts(s []byte) []int {
	starts := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		starts = append(starts, i)
		i += sqliteUTF8Skip(s[i:])
	}
	return append(starts, len(s))
}

// fnRound is roundFunc (func.c:451): Y is sqlite3_value_int64 clamped to
// [0,30] (func.c:458-460), X is sqlite3_value_double. A magnitude beyond 2^52
// has no fraction and is returned unchanged; Y==0 is the C cast
// (double)(sqlite_int64)(r+(r<0?-0.5:+0.5)), which is why
// round(0.49999999999999994) is 1.0 -- the addition itself rounds up to 1 --
// and why a negative X that rounds to zero is +0.0; any other Y renders X
// with "%!.*f" and reads it back through sqlite3AtoF (func.c:473-478), so
// round(-0.04,1) keeps its sign as -0.0 while round(-0.0,1) is 0.0. Always
// REAL, NULL if either argument is NULL.
func fnRound(args []Value) (Value, error) {
	n := int64(0)
	if len(args) == 2 {
		if args[1].Typ == Null {
			return Value{Typ: Null}, nil
		}
		n = min(max(sqliteValueInt64(args[1]), 0), 30)
	}
	if args[0].Typ == Null {
		return Value{Typ: Null}, nil
	}
	r := sqliteValueDouble(args[0])
	switch {
	case r < -4503599627370496.0 || r > +4503599627370496.0:
		// no fractional part, nothing to round
	case n == 0:
		if r < 0 {
			r = float64(int64(r - 0.5))
		} else {
			r = float64(int64(r + 0.5))
		}
	default:
		z, err := printfFloat('f', r, 0, int(n), false, 0, false, true, false, false, 0)
		if err != nil {
			return Value{}, err
		}
		r, _ = sqliteAtoF([]byte(z))
	}
	return Value{Typ: Float, F: r}, nil
}

// fnOctetLength implements octet_length(X) (SQLite 3.43+): the length of
// X's value in BYTES, as opposed to length()'s character count for TEXT --
// the two only diverge on TEXT containing any non-ASCII (multi-byte UTF-8)
// character; for NULL/BLOB/INTEGER/REAL they always agree (verified
// directly: octet_length(12345)==5, octet_length(7.5)==3, octet_length of a
// BLOB == its byte length == length()'s, matching fnLength's own Blob case;
// only the Text case differs, len(v.S) -- the raw UTF-8 byte count -- rather
// than fnLength's utf8.RuneCount).
func fnOctetLength(v Value) Value {
	switch v.Typ {
	case Null:
		return v
	case Blob, Text:
		return Value{Typ: Int, I: int64(len(v.S))}
	default:
		return Value{Typ: Int, I: int64(len(valueToText(v)))}
	}
}

// fnConcat implements concat(X1,X2,...) (SQLite 3.44+): every argument
// converted to its TEXT form (valueToText -- same conversion length()/quote()
// use; a BLOB's raw bytes pass through verbatim, exactly as valueToText's
// own Blob case), NULL arguments contributing an EMPTY string rather than
// propagating NULL the way the "||" operator does -- concat()'s entire
// reason to exist over plain "||" chaining. Always returns TEXT (never NULL,
// even when every argument is NULL: concat(NULL,NULL) == "" -- verified
// directly against mattn/go-sqlite3).
func fnConcat(args []Value) Value {
	var b strings.Builder
	for _, a := range args {
		if a.Typ != Null {
			b.WriteString(valueToText(a))
		}
	}
	return Value{Typ: Text, S: []byte(b.String())}
}

// fnConcatWs implements concat_ws(SEP,X1,X2,...) (SQLite 3.44+): joins every
// NON-NULL Xi (TEXT-converted the same way fnConcat does) with SEP between
// them, skipping NULL arguments ENTIRELY -- no empty placeholder, no doubled
// separator (concat_ws(',','a',NULL,'c') == "a,c", not "a,,c" -- verified
// directly). A NULL separator poisons the whole call: the result is NULL,
// not merely joined-with-empty-string (verified: concat_ws(NULL,'a','b') is
// NULL) -- the one way this function CAN return NULL.
func fnConcatWs(args []Value) Value {
	if args[0].Typ == Null {
		return Value{Typ: Null}
	}
	sep := valueToText(args[0])
	var parts []string
	for _, a := range args[1:] {
		if a.Typ != Null {
			parts = append(parts, valueToText(a))
		}
	}
	return Value{Typ: Text, S: []byte(strings.Join(parts, sep))}
}

// fnSign implements sign(X): NULL for NULL or BLOB (verified: sign(x'31')
// -- the blob byte 0x31, i.e. ASCII '1' -- is NULL, not 1; BLOB is never
// coerced numerically here, unlike abs()/round()/most arithmetic). A TEXT
// argument must be a FULLY well-formed number (parseFullNumeric -- leading/
// trailing whitespace only, no trailing garbage) or the result is NULL --
// verified: sign('5') is 1, but sign('  -3.5xyz') (valid numeric PREFIX,
// invalid full parse) is NULL, unlike abs()'s/round()'s lenient
// longest-numeric-prefix coercion. Result is always INTEGER -1/0/1.
func fnSign(v Value) Value {
	switch v.Typ {
	case Null, Blob:
		return Value{Typ: Null}
	case Int:
		return Value{Typ: Int, I: signOf(v.I)}
	case Float:
		return Value{Typ: Int, I: signOfFloat(v.F)}
	default: // Text
		isF, i, f, ok := parseFullNumeric(string(v.S))
		if !ok {
			return Value{Typ: Null}
		}
		if isF {
			return Value{Typ: Int, I: signOfFloat(f)}
		}
		return Value{Typ: Int, I: signOf(i)}
	}
}

func signOf(i int64) int64 {
	switch {
	case i > 0:
		return 1
	case i < 0:
		return -1
	default:
		return 0
	}
}

func signOfFloat(f float64) int64 {
	switch {
	case f > 0:
		return 1
	case f < 0:
		return -1
	default:
		return 0
	}
}

// fnReplace implements replace(X,Y,Z): NULL if any argument is NULL; all
// three arguments are coerced to TEXT (verified: replace(123,2,'X') ==
// "1X3"); an empty Y is a no-op returning X's text UNCHANGED (verified:
// replace('abc','','X') == "abc" -- NOT "X"-interleaved the way
// strings.ReplaceAll("abc","","X") would render it), matching real
// SQLite's own explicit empty-needle guard in func.c's replaceFunc. Always
// returns TEXT.
func fnReplace(args []Value) Value {
	// Argument order matters here, and is C SQLite's own (replaceFunc,
	// func.c): X and the PATTERN are NULL-checked first, then an EMPTY
	// pattern short-circuits to X -- returning it BEFORE the replacement
	// argument is ever looked at. So replace(X, '', NULL) is X, not NULL.
	// Verified directly: replace('abc', '', NULL) is 'abc', and
	// replace(x'92dba0', '', NULL) is the blob's bytes as TEXT (SQLite
	// converts X in place with sqlite3_value_text before returning it, so
	// the result's storage class is TEXT even for a BLOB X).
	if args[0].Typ == Null || args[1].Typ == Null {
		return Value{Typ: Null}
	}
	x, y := valueToText(args[0]), valueToText(args[1])
	// The empty-pattern test is on the pattern's C-STRING form, but the
	// search itself is over its full bytes -- see textBeforeNUL.
	if textBeforeNUL(y) == "" {
		return Value{Typ: Text, S: []byte(x)}
	}
	if args[2].Typ == Null {
		return Value{Typ: Null}
	}
	return Value{Typ: Text, S: []byte(strings.ReplaceAll(x, y, valueToText(args[2])))}
}

// fnInstr implements instr(X,Y): NULL if either argument is NULL. Byte
// offset (1-based) ONLY when BOTH X and Y are BLOB; otherwise both are
// coerced to TEXT and the offset is a CHARACTER (rune) count -- verified
// directly: instr(blob,blob) uses byte offsets (a multi-byte UTF-8 blob's
// "llo" match lands one past its text-mode position), but blob-vs-text or
// text-vs-blob (either side non-BLOB) always uses character offsets, even
// though one operand is technically a BLOB Value. An empty needle matches
// at position 1 (even instr('','') == 1); no match is 0. Always INTEGER.
func fnInstr(x, y Value) Value {
	if x.Typ == Null || y.Typ == Null {
		return Value{Typ: Null}
	}
	if x.Typ == Blob && y.Typ == Blob {
		if len(y.S) == 0 {
			return Value{Typ: Int, I: 1}
		}
		i := bytesIndex(x.S, y.S)
		if i < 0 {
			return Value{Typ: Int, I: 0}
		}
		return Value{Typ: Int, I: int64(i + 1)}
	}
	xs, ys := valueToText(x), valueToText(y)
	if ys == "" {
		return Value{Typ: Int, I: 1}
	}
	// The TEXT search matches on CHARACTER boundaries, not raw bytes: a
	// needle that happens to occur inside a multi-byte character's encoding
	// is NOT a match. Verified directly against C SQLite --
	// instr('xä€y', x'a4') is 0, even though 0xa4 is the second byte of 'ä',
	// while instr('xabcy', x'62') is 3 (instr.test).
	//
	// The boundaries are SQLite's own (sqliteUTF8Skip), not Go's: instrFunc
	// advances the haystack one byte at a time and then skips while the next
	// byte is a continuation byte, so a truncated sequence is ONE position, not
	// one per byte. Ranging over the string counted each rejected byte
	// separately, so instr(cast(x'e28241' as text),'A') answered 3 where real
	// SQLite answers 2 (verified against 3.53.3).
	hay := []byte(xs)
	pos := int64(1)
	for i := 0; i < len(hay); pos++ {
		if bytes.HasPrefix(hay[i:], []byte(ys)) {
			return Value{Typ: Int, I: pos}
		}
		i += sqliteUTF8Skip(hay[i:])
	}
	return Value{Typ: Int, I: 0}
}

func bytesIndex(hay, needle []byte) int {
	return strings.Index(string(hay), string(needle))
}

// fnQuote implements quote(X): TEXT/'..'-quoting (embedded ' doubled),
// BLOB/X'..' hex, NULL -> the literal TEXT "NULL" (NOT the SQL NULL value
// -- verified: typeof(quote(NULL)) is "text"), INTEGER/REAL rendered
// exactly as their own TEXT conversion (unquoted -- quote(1.5) == "1.5",
// quote(3.0) == "3.0"). Always returns TEXT.
func fnQuote(v Value) Value {
	switch v.Typ {
	case Null:
		return Value{Typ: Text, S: []byte("NULL")}
	case Float:
		return Value{Typ: Text, S: []byte(formatFloatLiteral(v.F))}
	case Int:
		return Value{Typ: Text, S: []byte(valueToText(v))}
	case Blob:
		const digits = "0123456789ABCDEF"
		out := make([]byte, 0, len(v.S)*2+3)
		out = append(out, 'X', '\'')
		for _, c := range v.S {
			out = append(out, digits[c>>4], digits[c&0xf])
		}
		out = append(out, '\'')
		return Value{Typ: Text, S: out}
	default: // Text
		// Byte-wise, not rune-wise: quote() reproduces its argument's bytes
		// exactly, doubling only the ASCII apostrophe. Ranging over the string
		// and re-encoding each rune replaced every byte Go's decoder rejected
		// with U+FFFD, so quote(cast(x'ff' as text)) rendered 27 EFBFBD 27 where
		// C SQLite renders 27 FF 27 (verified against 3.53.3) -- a quote()
		// whose output no longer round-trips to the value it was given.
		s := textBeforeNUL(string(v.S))
		out := make([]byte, 0, len(s)+2)
		out = append(out, '\'')
		for i := 0; i < len(s); i++ {
			if s[i] == '\'' {
				out = append(out, '\'')
			}
			out = append(out, s[i])
		}
		out = append(out, '\'')
		return Value{Typ: Text, S: out}
	}
}

// fnChar implements char(X1,X2,...): each argument is truncated to an
// integer codepoint (loose numeric coercion; NULL treated as 0 -- verified:
// hex(char(NULL)) == "00") and encoded as raw UTF-8 WITHOUT Go's usual
// surrogate-range validity rejection: C SQLite happily encodes a
// surrogate codepoint (e.g. char(55296), U+D800) as the naive 3-byte UTF-8
// sequence ED A0 80 -- verified directly (hex(char(55296)) == "EDA080"),
// which Go's utf8.EncodeRune/AppendRune would instead replace with U+FFFD.
// Only codepoints outside [0, 0x10FFFF] become the replacement character
// U+FFFD (hex(char(-1)) == hex(char(1114112)) == "EFBFBD"; hex(char(1114111))
// -- exactly 0x10FFFF -- encodes normally). Zero arguments returns "".
// Always returns TEXT.
func fnChar(args []Value) Value {
	var buf []byte
	for _, a := range args {
		var cp int64
		if a.Typ != Null {
			cp = valueToInt64Trunc(a)
		}
		buf = appendRawUTF8(buf, cp)
	}
	return Value{Typ: Text, S: buf}
}

// appendRawUTF8 appends cp's UTF-8 encoding to buf using the plain
// bit-packing algorithm with NO surrogate-range special-casing (see
// fnChar's doc comment for why this can't be utf8.AppendRune).
func appendRawUTF8(buf []byte, cp int64) []byte {
	switch {
	case cp < 0 || cp > 0x10FFFF:
		return append(buf, 0xEF, 0xBF, 0xBD) // U+FFFD
	case cp < 0x80:
		return append(buf, byte(cp))
	case cp < 0x800:
		return append(buf, byte(0xC0|(cp>>6)), byte(0x80|(cp&0x3F)))
	case cp < 0x10000:
		return append(buf, byte(0xE0|(cp>>12)), byte(0x80|((cp>>6)&0x3F)), byte(0x80|(cp&0x3F)))
	default:
		return append(buf, byte(0xF0|(cp>>18)), byte(0x80|((cp>>12)&0x3F)), byte(0x80|((cp>>6)&0x3F)), byte(0x80|(cp&0x3F)))
	}
}

// fnUnicode implements unicode(X): the codepoint of X's first UTF-8
// character (X coerced to TEXT). NULL for NULL OR an empty string --
// verified: unicode('') is NULL, not an error or 0. Always INTEGER.
func fnUnicode(v Value) Value {
	if v.Typ == Null {
		return Value{Typ: Null}
	}
	s := textBeforeNUL(valueToText(v))
	if s == "" {
		return Value{Typ: Null}
	}
	// SQLite's own reader, not Go's: utf8.DecodeRuneInString reports U+FFFD
	// after ONE byte for anything it rejects, where READ_UTF8 swallows the
	// continuation bytes and keeps the bits -- unicode(cast(x'e282' as text))
	// is 130 there and was 65533 here. See sqliteUTF8Next.
	cp, _ := sqliteUTF8Next([]byte(s))
	return Value{Typ: Int, I: cp}
}

// fnUnhex implements unhex(X) / unhex(X,Y): X must be TEXT/coercible-to-
// TEXT; NULL X or NULL Y (when given) yields NULL. Y, if given, is a set of
// characters ignorable BETWEEN complete byte pairs only -- verified
// directly: unhex('41 42',' ') == blob 0x41,0x42 (space at a byte
// boundary, ignored), but unhex('4 1',' ') is NULL (space appears MID-byte,
// after only one nibble of the pair -- rejected, not simply stripped
// first: a naive "strip all Y characters from X, then decode" model would
// wrongly accept this). Any character that is neither a hex digit nor an
// ignorable (at a boundary) is an immediate NULL, as is an odd total count
// of hex digits.
func fnUnhex(args []Value) Value {
	if args[0].Typ == Null {
		return Value{Typ: Null}
	}
	var ignore string
	hasIgnore := len(args) == 2
	if hasIgnore {
		if args[1].Typ == Null {
			return Value{Typ: Null}
		}
		// The IGNORE set is NOT read as a C string, unlike X itself --
		// verified: unhex('41X42', cast(x'005258' as text)) is x'4142',
		// i.e. the 'X' past the set's leading NUL still counts as
		// ignorable. See textBeforeNUL.
		ignore = valueToText(args[1])
	}
	s := textBeforeNUL(valueToText(args[0]))
	out := make([]byte, 0, len(s)/2)
	var hi byte
	haveHi := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if v, ok := hexDigitVal(c); ok {
			if !haveHi {
				hi, haveHi = v, true
			} else {
				out = append(out, hi<<4|v)
				haveHi = false
			}
			continue
		}
		if hasIgnore && !haveHi && strings.IndexByte(ignore, c) >= 0 {
			continue
		}
		return Value{Typ: Null}
	}
	if haveHi {
		return Value{Typ: Null}
	}
	return Value{Typ: Blob, S: out}
}

func hexDigitVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// fnZeroblob implements zeroblob(N): an all-zero BLOB of N bytes; a
// negative N is clamped to an empty blob (verified: zeroblob(-1) == the
// empty blob, not an error).
func fnZeroblob(v Value) (Value, error) {
	n := valueToInt64Trunc(v)
	if n < 0 {
		n = 0
	}
	if err := checkStringOrBlobLength(n); err != nil {
		return Value{}, err
	}
	return Value{Typ: Blob, S: make([]byte, n)}, nil
}

// sqliteMaxLength is SQLITE_MAX_LENGTH's default: the largest string or blob
// SQLite will produce. Verified directly that zeroblob(1000000000) is fine and
// zeroblob(1000000001) is "string or blob too big" (zeroblob.test), and that
// the same limit governs randomblob(), hex() and printf().
const sqliteMaxLength = 1000000000

// checkStringOrBlobLength reports C SQLite's own error for a string or blob
// result that would exceed sqliteMaxLength. Called BEFORE allocating, so an
// absurd length is refused rather than reserved.
func checkStringOrBlobLength(n int64) error {
	if n > sqliteMaxLength {
		return fmt.Errorf("engine: string or blob too big")
	}
	return nil
}

// fnIif implements iif(X,Y,Z): sugar for CASE WHEN X THEN Y ELSE Z END,
// using the same three-valued truthiness test (isTruthy) WHERE already
// uses -- a NULL condition takes the ELSE branch, exactly like CASE
// (verified: iif(NULL,2,3) == 3).
func fnIif(x, y, z Value) Value {
	if isTruthy(x) {
		return y
	}
	return z
}

// fnTrim implements trim/ltrim/rtrim(X[,Y]): X coerced to TEXT (verified:
// typeof(ltrim(123)) is "text", typeof(trim(x'000102')) is "text" -- a BLOB
// argument's raw bytes are taken as text unchanged, same as valueToText
// everywhere else). The default (Y omitted) strip set is EXACTLY the space
// character (0x20) -- verified directly: trim() leaves an embedded tab/
// newline untouched even though it strips leading/trailing spaces (real
// SQLite's trim() is NOT a general whitespace-trim). An explicit empty Y
// (trim(X,'')) is a no-op. left/right select which end(s) to strip.
func fnTrim(args []Value, left, right bool) Value {
	if args[0].Typ == Null {
		return Value{Typ: Null}
	}
	cutset := " "
	if len(args) == 2 {
		if args[1].Typ == Null {
			return Value{Typ: Null}
		}
		// The CHARSET is walked as a C string (so one leading with a NUL
		// is empty, and trims nothing); the SUBJECT keeps all its bytes.
		cutset = textBeforeNUL(valueToText(args[1]))
	}
	s := []byte(valueToText(args[0]))
	if cutset == "" {
		return Value{Typ: Text, S: s}
	}
	// The charset is split into CHARACTERS by SQLite's own reader and each is
	// matched against the subject as RAW BYTES (trimFunc builds azChar[]/aLen[]
	// and memcmp's them). strings.TrimLeft/TrimRight decode both sides as UTF-8
	// instead, which collapses every byte Go rejects to U+FFFD on BOTH sides and
	// so makes any two of them compare equal: trim(cast(x'e28241' as text),
	// cast(x'ff' as text)) stripped the leading E2 82 and answered "41", where
	// C SQLite leaves the value untouched (verified against 3.53.3).
	var chars [][]byte
	for cs := []byte(cutset); len(cs) > 0; {
		n := sqliteUTF8Skip(cs)
		chars = append(chars, cs[:n])
		cs = cs[n:]
	}
	matchAt := func(b []byte) int {
		for _, c := range chars {
			if bytes.HasPrefix(b, c) {
				return len(c)
			}
		}
		return 0
	}
	if left {
		for len(s) > 0 {
			n := matchAt(s)
			if n == 0 {
				break
			}
			s = s[n:]
		}
	}
	if right {
		for len(s) > 0 {
			// The trailing character is whichever charset entry ENDS the
			// subject, matched by bytes exactly as the leading side is.
			n := 0
			for _, c := range chars {
				if bytes.HasSuffix(s, c) {
					n = len(c)
					break
				}
			}
			if n == 0 {
				break
			}
			s = s[:len(s)-n]
		}
	}
	return Value{Typ: Text, S: s}
}

// fnMinMaxScalar implements the 2-or-more-argument SCALAR min()/max() (the
// 1-argument AGGREGATE form is handled entirely by sql_agg.go -- see
// isAggregateCall). Any NULL argument makes the whole result NULL
// (verified: min(1,2,NULL,0) is NULL, unlike the aggregate form which
// skips NULLs). Comparison is compareValues' usual SQLite ordering
// (NULL < numeric < TEXT < BLOB, numeric cross-compared exactly). On a
// tie, the LATER argument wins and its own type/representation is kept --
// verified directly: typeof(min(1,1.0)) is "real" (the second, tied
// argument), typeof(min(1.0,1)) is "integer" (again the second) -- NOT
// "whichever came first"; C SQLite's own minmaxFunc replaces on >=/<=
// rather than >/< for exactly this reason.
func fnMinMaxScalar(name string, args []Value) Value {
	for _, a := range args {
		if a.Typ == Null {
			return Value{Typ: Null}
		}
	}
	best := args[0]
	isMin := name == "min"
	for _, a := range args[1:] {
		c := compareValues(a, best)
		if (isMin && c <= 0) || (!isMin && c >= 0) {
			best = a
		}
	}
	return best
}

// fnRandom implements random(): a uniformly-random INTEGER spanning the
// full int64 range (SQLite's own documented behavior), sourced from
// crypto/rand (see randomInt64 in scalar_random.go) -- NEVER gated against
// the oracle (nondeterministic by design, per this task's own instructions).
func fnRandom() Value {
	return Value{Typ: Int, I: randomInt64()}
}

// fnRandomblob implements randomblob(N): an N-byte BLOB of cryptographically
// random bytes (N<1 clamped to a 1-byte blob, matching C SQLite's own
// documented minimum). Nondeterministic; never gated against the oracle for
// its CONTENT (only its length/type are checked).
func fnRandomblob(v Value) (Value, error) {
	n := valueToInt64Trunc(v)
	if n < 1 {
		n = 1
	}
	if err := checkStringOrBlobLength(n); err != nil {
		return Value{}, err
	}
	buf := make([]byte, n)
	randomBytes(buf)
	return Value{Typ: Blob, S: buf}, nil
}

// fnUnistr implements SQLite's unistr(X): a work-alike of PostgreSQL's, which
// expands backslash escapes in X into the characters they name. Ported from
// unistrFunc/isNHex/sqlite3AppendOneUtf8Character (sqlite3-binding.c), not
// reconstructed -- its UTF-8 writer is deliberately NOT the standard one and
// that difference is observable.
//
// The five escapes, each requiring EXACTLY that many hex digits:
//
//	\\          a literal backslash
//	\XXXX       4 hex digits          (no introducer at all)
//	\uXXXX      4 hex digits
//	\+XXXXXX    6 hex digits
//	\UXXXXXXXX  8 hex digits
//
// Anything else after a backslash -- including a short digit run -- is
// "invalid Unicode escape", an ERROR, not a passthrough.
//
// The writer takes the LOW 21 BITS and never emits a replacement character:
// SQLite's own AppendOneUtf8Character has no 0x10FFFF ceiling and no surrogate
// check, so unistr('\UFFFFFFFF') is the four bytes F7 BF BF BF (func9.test
// asserts exactly that hex), where a standards-conforming encoder would refuse
// the value or substitute U+FFFD. char() is the one that clamps (x>0x10ffff ->
// 0xfffd); unistr() is not char().
//
// A non-TEXT argument returns NULL without evaluating anything, matching
// unistrFunc's own "if( zIn==0 ) return" on a NULL, and its text coercion for
// the rest.
func fnUnistr(v Value) (Value, error) {
	if v.Typ == Null {
		return Value{Typ: Null}, nil
	}
	// valueToText, not valueText: unistrFunc reads its argument through
	// sqlite3_value_text, which renders an INTEGER or REAL as its text form
	// ("unistr(123)" is '123') rather than as the empty string.
	in := []byte(valueToText(v))
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		if in[i] != '\\' {
			out = append(out, in[i])
			i++
			continue
		}
		if i+1 >= len(in) {
			return Value{}, fmt.Errorf("engine: invalid Unicode escape")
		}
		switch c := in[i+1]; {
		case c == '\\':
			out = append(out, '\\')
			i += 2
		case isHexDigitByte(c):
			v, ok := readNHex(in, i+1, 4)
			if !ok {
				return Value{}, fmt.Errorf("engine: invalid Unicode escape")
			}
			out = appendOneUTF8(out, v)
			i += 5
		case c == '+':
			v, ok := readNHex(in, i+2, 6)
			if !ok {
				return Value{}, fmt.Errorf("engine: invalid Unicode escape")
			}
			out = appendOneUTF8(out, v)
			i += 8
		case c == 'u':
			v, ok := readNHex(in, i+2, 4)
			if !ok {
				return Value{}, fmt.Errorf("engine: invalid Unicode escape")
			}
			out = appendOneUTF8(out, v)
			i += 6
		case c == 'U':
			v, ok := readNHex(in, i+2, 8)
			if !ok {
				return Value{}, fmt.Errorf("engine: invalid Unicode escape")
			}
			out = appendOneUTF8(out, v)
			i += 10
		default:
			return Value{}, fmt.Errorf("engine: invalid Unicode escape")
		}
	}
	return Value{Typ: Text, S: out}, nil
}

// isHexDigitByte is sqlite3Isxdigit for one byte.
func isHexDigitByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// readNHex is isNHex: exactly n hex digits starting at off, or ok=false. A
// short run at the end of the string fails rather than reading past it.
func readNHex(s []byte, off, n int) (uint32, bool) {
	if off+n > len(s) {
		return 0, false
	}
	var v uint32
	for i := 0; i < n; i++ {
		c := s[off+i]
		if !isHexDigitByte(c) {
			return 0, false
		}
		var d uint32
		switch {
		case c <= '9':
			d = uint32(c - '0')
		case c >= 'a':
			d = uint32(c-'a') + 10
		default:
			d = uint32(c-'A') + 10
		}
		v = v<<4 + d
	}
	return v, true
}

// appendOneUTF8 is sqlite3AppendOneUtf8Character verbatim -- see fnUnistr for
// why it is not the standard encoder.
func appendOneUTF8(dst []byte, v uint32) []byte {
	switch {
	case v < 0x80:
		return append(dst, byte(v&0xff))
	case v < 0x800:
		return append(dst, 0xc0+byte((v>>6)&0x1f), 0x80+byte(v&0x3f))
	case v < 0x10000:
		return append(dst, 0xe0+byte((v>>12)&0x0f), 0x80+byte((v>>6)&0x3f), 0x80+byte(v&0x3f))
	}
	return append(dst, 0xf0+byte((v>>18)&0x07), 0x80+byte((v>>12)&0x3f),
		0x80+byte((v>>6)&0x3f), 0x80+byte(v&0x3f))
}

// fnUnistrQuote implements SQLite's unistr_quote(X): quote() with printf's
// ALTERNATE FORM, which the amalgamation spells as quoteFunc with
// sqlite3QuoteValue(..., bEscape=1) -> "%#Q" (sqlite3-binding.c). Ported from
// that printf case, not reconstructed.
//
// For every storage class except TEXT it IS quote(). For TEXT the "#" does
// nothing UNLESS the string contains at least one CONTROL byte (<= 0x1f):
//
//	unistr_quote('Gäste')      ->  'Gäste'          no control byte
//	unistr_quote('a'||char(9)) ->  unistr('a	')
//
// -- SQLite's own code clears the alternate-form flag when nCtrl is zero, so
// a non-ASCII character is NOT enough to trigger the wrapper. That is the
// rule func9.test's "unistr_quote(unistr('Gäste'))" pins, and the reason
// this cannot be written as "escape anything non-ASCII".
//
// Inside the wrapper: a backslash doubles, a control byte becomes \u00XY with
// LOWER-CASE hex, an apostrophe still doubles, and every other byte passes
// through untouched -- bytes, not runes, exactly like quote() itself.
func fnUnistrQuote(v Value) Value {
	if v.Typ != Text {
		return fnQuote(v)
	}
	// The value's text STOPS at its first NUL, exactly as quote() reads it
	// (textBeforeNUL) -- "unistr_quote(unistr('\\u0000'))" is '' in real
	// SQLite, not a wrapped escape, because sqlite3_value_text hands the
	// conversion an empty string.
	s := textBeforeNUL(string(v.S))
	if !hasControlByte(s) {
		return fnQuote(Value{Typ: Text, S: []byte(s)})
	}
	return Value{Typ: Text, S: []byte("unistr('" + unistrEscapeBody(s) + "')")}
}

// hasControlByte reports whether s holds a byte <= 0x1f -- SQLite's own test
// for whether printf's "#" alternate form does anything to a %Q conversion.
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x1f {
			return true
		}
	}
	return false
}

// unistrEscapeBody renders s the way printf's "%#q"/"%#Q" alternate form does:
// a backslash doubles, a control byte becomes \u00XY with LOWER-CASE hex, an
// apostrophe still doubles, and every other byte passes through untouched --
// BYTES, not runes, exactly like quote() itself. Ported from the etESCAPE_q
// case of sqlite3_str_vappendf (sqlite3-binding.c).
func unistrEscapeBody(s string) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			out = append(out, '\'', '\'')
		case c == '\\':
			out = append(out, '\\', '\\')
		case c <= 0x1f:
			out = append(out, '\\', 'u', '0', '0')
			if c >= 0x10 {
				out = append(out, '1')
			} else {
				out = append(out, '0')
			}
			out = append(out, hex[c&0xf])
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
