// Conversions between Values and their text/numeric representations: numeric
// parsing, REAL rendering, and CAST.
//
// formatFloatText is SQLite's REAL->TEXT rendering, vdbeMemRenderNum's
// "%!.*g" with 17 digits, through the printf port (printfFloat).
//
// The numeric-prefix parsers implement SQLite's permissive TEXT->number
// coercion, where a leading numeric prefix is taken and the rest ignored.
package engine

import (
	"fmt"
	"math"
	"strconv"
)

// limitOffsetValueToCount converts a LIMIT/OFFSET expression's Value into the
// integer count execSelect's *int64-based LIMIT/OFFSET application needs. It
// applies INTEGER affinity (applyAffinityToValue) and accepts the result only
// when it is an exact Int -- an integer literal, a whole-string-numeric TEXT
// like '2', a no-fractional-part REAL like 2.0 or 1e2, and TRUE/FALSE all fold
// that way. A negative Int (e.g. -1) passes through unchanged, matching this
// engine's and SQLite's own "negative LIMIT/OFFSET means unlimited"
// convention (limitOffsetRange, query.go).
//
// Anything that does NOT fold to an exact integer is C SQLite's "datatype mismatch".
func limitOffsetValueToCount(v Value) (int64, error) {
	folded := applyAffinityToValue(v, affInteger)
	if folded.Typ != Int {
		return 0, errLimitNotInteger
	}
	return folded.I, nil
}

// errLimitNotInteger is the value above, named so a caller that folded the
// clause AHEAD OF TIME can tell "this expression is not an integer" apart from
// "this expression could not be evaluated at all".
//
// The distinction is C's own, and it decides WHEN the error fires.
// computeLimitRegisters (select.c:2523) never evaluates the clause at compile
// time: for anything sqlite3ExprIsInteger cannot read off the grammar it emits
//
//	select.c:2547   sqlite3ExprCode(pParse, pLimit->pLeft, iLimit);
//	select.c:2548   sqlite3VdbeAddOp1(v, OP_MustBeInt, iLimit);
//
// so SQLITE_MISMATCH is raised by OP_MustBeInt when the SELECT RUNS -- and a
// subquery that never runs never raises it. resolveSubProgramLimitOffset
// (vdbe_codegen.go) folds instead, which is right for the VALUE and wrong for
// the TIMING; seeing this error is how it knows to stop folding that clause and
// let the codegen emit OpLimitCounter (this engine's OP_MustBeInt) instead.
var errLimitNotInteger = fmt.Errorf("engine: datatype mismatch")

// intFoldWide reports whether f has no fractional part, round-trips exactly
// through int64, and is NEITHER exactly math.MinInt64 NOR math.MaxInt64 --
// C SQLite's sqlite3VdbeIntegerAffinity rule (ticket #3922: the two
// extreme values are excluded because "addition overflow causes values to
// wrap around", making a round-trip match at exactly the boundary
// ambiguous) -- used when v is ALREADY Int/Float (an arithmetic result, a
// REAL literal, ...) going into NUMERIC/INTEGER/REAL affinity. Verified
// directly against C SQLite: e.g. a NUMERIC-affinity column storing an
// arithmetic result that lands on exactly -9223372036854775808.0 stays
// REAL, not INTEGER, even though the round-trip is exact -- see this
// package's compat-harness write fuzzer, write_fuzz_test.go, which is what
// surfaced this.
func intFoldWide(f float64) (int64, bool) {
	i, ok := exactInt64(f)
	if !ok || i == math.MinInt64 || i == math.MaxInt64 {
		return 0, false
	}
	return i, true
}

// intFoldNarrow reports whether f has no fractional part, round-trips
// exactly through int64, and is EITHER exactly zero OR within
// [-2^51, 2^51) -- C SQLite's sqlite3RealSameAsInt rule, used
// specifically when folding a Float that was just parsed out of TEXT for
// NUMERIC/INTEGER/REAL affinity (applyNumericAffinity's bTryForInt path,
// via alsoAnInt): a MUCH narrower range than intFoldWide's, deliberately,
// per C SQLite's own comment on sqlite3RealSameAsInt ("the two values
// are the same within the precision of the floating point value").
func intFoldNarrow(f float64) (int64, bool) {
	if f == 0 {
		return 0, true
	}
	i, ok := exactInt64(f)
	if !ok || i < -(1<<51) || i >= (1<<51) {
		return 0, false
	}
	return i, true
}

// exactInt64 reports whether f has no fractional part and round-trips
// exactly through int64 (float64(int64(f)) == f) -- the same round-trip
// test C SQLite's own integer/real storage-class folding uses.
func exactInt64(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f < -9223372036854775808 || f >= 9223372036854775808 {
		return 0, false
	}
	i := int64(f)
	return i, float64(i) == f
}

// isTruthy implements WHERE's truthiness test: a value is true iff it is
// non-NULL and numerically non-zero, with TEXT/BLOB coerced to numeric via
// the same lenient rule as arithmetic.
func isTruthy(v Value) bool {
	if v.Typ == Null {
		return false
	}
	isF, i, f := toNumericLoose(v)
	if isF {
		return f != 0
	}
	return i != 0
}

// ---- text <-> number conversions ----

// toNumericLoose converts v to a number using SQLite's lenient rule (used by
// arithmetic, unary +/-, and truthiness): INTEGER/REAL pass through; TEXT/
// BLOB use only their longest valid leading numeric prefix, treated as 0 if
// there isn't one; NULL is treated as 0 (callers needing NULL propagation
// must check for it themselves first).
// numericPrefixEndsAtNUL reports whether v is TEXT/BLOB whose longest numeric
// prefix is an INTEGER and whose first byte AFTER that prefix -- skipping any
// ASCII whitespace -- is a NUL. That one shape makes C SQLite's two
// string-to-number paths disagree with each other, in OPPOSITE directions from
// every other kind of trailing junk:
//
//	                                  "1"   "1x"   "1"+NUL
//	typeof('...' + 0)              integer integer   REAL
//	typeof(sum('...'))             integer  real   INTEGER
//
// so the NUL case is exactly the other two classes SWAPPED. Verified byte by
// byte against mattn/go-sqlite3: a NUL anywhere after the integer prefix
// triggers it as long as nothing but whitespace precedes it ("1"+NUL,
// "1"+NUL+"x", "1 "+NUL, "12"+NUL, "-1"+NUL, " 1"+NUL all do), while a NUL
// AFTER other junk does not ("1x"+NUL behaves like plain "1x"). A FLOAT prefix
// is unaffected ("1.0"+NUL and "1e0"+NUL are real on both paths anyway), and so
// is text with no numeric prefix at all.
//
// Deliberately NOT the embedded-NUL C-string rule (textBeforeNUL): treating the
// value as the truncated string "1" would make BOTH paths integer, which is
// wrong for arithmetic. This is its own, separate rule -- the two happen to
// involve the same byte and nothing else.
func numericPrefixEndsAtNUL(v Value) bool {
	if v.Typ != Text && v.Typ != Blob {
		return false
	}
	s := string(v.S)
	start, end, isF, matched := scanNumber(s)
	if !matched || isF || start >= end {
		return false
	}
	for k := end; k < len(s); k++ {
		if isSpaceByte(s[k]) {
			continue
		}
		return s[k] == 0
	}
	return false
}

// toNumericArith is toNumericLoose for the ARITHMETIC operators (+ - * / %, and
// unary "-"), which are the only consumers that treat an integer prefix ending
// at a NUL as a REAL. Every OTHER toNumericLoose caller -- abs(), round(), CAST,
// printf, comparisons -- already agrees with C SQLite on that shape and must
// keep the plain reading; verified individually, which is why this is a separate
// function rather than a change to toNumericLoose itself.
func toNumericArith(v Value) (isFloat bool, i int64, f float64) {
	isF, ii, ff := toNumericLoose(v)
	if !isF && numericPrefixEndsAtNUL(v) {
		return true, 0, float64(ii)
	}
	return isF, ii, ff
}

func toNumericLoose(v Value) (isFloat bool, i int64, f float64) {
	switch v.Typ {
	case Int:
		return false, v.I, 0
	case Float:
		return true, 0, v.F
	case Text, Blob:
		isF, ii, ff, ok := parseNumericPrefix(string(v.S))
		if !ok {
			return false, 0, 0
		}
		return isF, ii, ff
	default:
		return false, 0, 0
	}
}

// parseNumericPrefix parses the longest valid numeric prefix of s (after
// skipping leading whitespace), ignoring anything after it. Used for
// arithmetic-style lenient coercion and CAST(... AS INTEGER/REAL).
func parseNumericPrefix(s string) (isFloat bool, i int64, f float64, ok bool) {
	start, end, isF, matched := scanNumber(s)
	if !matched {
		return false, 0, 0, false
	}
	return numberFromText(s[start:end], isF)
}

// parseIntegerPrefix scans the longest INTEGER-only prefix of s (optional
// leading whitespace, optional sign, then a run of digits -- no decimal
// point, no exponent), which is SQLite's specific rule for CAST(TEXT AS
// INTEGER): unlike parseNumericPrefix (used for CAST/coercion to REAL/
// NUMERIC, which reads a full numeric literal including '.'/exponent),
// casting TEXT to INTEGER truncates at the first non-digit and never treats
// a '.' or 'e'/'E' as part of the integer -- e.g. CAST('123.9' AS INTEGER)
// is 123 (stops at '.') and, notably, CAST('123e+5' AS INTEGER) is 123, NOT
// 12300000 (stops at 'e' -- the exponent is simply never considered part of
// an integer literal for this specific conversion, even though it very much
// is for CAST(... AS REAL)/parseNumericPrefix). Verified directly against
// C SQLite (mattn/go-sqlite3): CAST('1e300' AS INTEGER) is 1, not
// 1000...0 or an overflow saturation -- confirming the exponent is dropped
// entirely, not evaluated and then truncated. A digit run too long to fit
// an int64 saturates to Max/MinInt64 by sign, also matching C SQLite.
func parseIntegerPrefix(s string) (i int64, ok bool) {
	n := len(s)
	p := 0
	for p < n && isSpaceByte(s[p]) {
		p++
	}
	start := p
	neg := false
	if p < n && (s[p] == '+' || s[p] == '-') {
		neg = s[p] == '-'
		p++
	}
	digitsStart := p
	for p < n && s[p] >= '0' && s[p] <= '9' {
		p++
	}
	if p == digitsStart {
		return 0, false
	}
	v, err := strconv.ParseInt(s[start:p], 10, 64)
	if err != nil {
		if neg {
			return math.MinInt64, true
		}
		return math.MaxInt64, true
	}
	return v, true
}

// parseFullNumeric requires the entire string (aside from surrounding
// whitespace) to be a well-formed number. Used for type-affinity conversion
// and CAST(... AS NUMERIC), which (unlike CAST INTEGER/REAL) do not accept a
// merely-numeric-prefixed string such as "5abc".
func parseFullNumeric(s string) (isFloat bool, i int64, f float64, ok bool) {
	start, end, isF, matched := scanNumber(s)
	if !matched {
		return false, 0, 0, false
	}
	for k := end; k < len(s); k++ {
		if !isSpaceByte(s[k]) {
			return false, 0, 0, false
		}
	}
	return numberFromText(s[start:end], isF)
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// scanNumber scans a SQLite-style number (optional sign, digits, optional
// '.' fraction, optional exponent) starting after any leading whitespace in
// s. It reports the byte span [start,end) of the number and whether it has
// a decimal point or exponent (making it a REAL rather than an INTEGER
// literal). matched is false if no number is present at all.
func scanNumber(s string) (start, end int, isFloat, matched bool) {
	i := 0
	n := len(s)
	for i < n && isSpaceByte(s[i]) {
		i++
	}
	start = i
	if i < n && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digitsStart := i
	for i < n && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intDigits := i - digitsStart
	hasFrac := false
	if i < n && s[i] == '.' {
		i++
		fracStart := i
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		hasFrac = i > fracStart
		isFloat = true
	}
	if intDigits == 0 && !hasFrac {
		return 0, 0, false, false
	}
	if i < n && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < n && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < n && s[j] >= '0' && s[j] <= '9' {
			for j < n && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			i = j
			isFloat = true
		}
	}
	return start, i, isFloat, true
}

// numberFromText converts the exact numeric text matched by scanNumber into
// a Value-shaped (isFloat,i,f) triple, falling back to float64 when integer
// syntax overflows int64 -- exactly SQLite's own rule for integer literals
// too large to represent exactly.
func numberFromText(sub string, isFloat bool) (bool, int64, float64, bool) {
	if !isFloat {
		if n, err := strconv.ParseInt(sub, 10, 64); err == nil {
			return false, n, 0, true
		}
		f, ok := parseFloatSaturating(sub)
		if !ok {
			return false, 0, 0, false
		}
		return true, 0, f, true
	}
	f, ok := parseFloatSaturating(sub)
	if !ok {
		return false, 0, 0, false
	}
	return true, 0, f, true
}

// parseFloatSaturating converts numeric text scanNumber matched the way every
// TEXT->REAL conversion in SQLite does, through sqlite3AtoF (util.c:871; the
// affinity and CAST paths reach it via sqlite3MemRealValueRC, vdbemem.c:735).
// That is not a correctly-rounded parse: it keeps about 19 significant digits
// and ignores the rest, so '3500000000000000.2500001' is 3500000000000000.0
// where strconv would give ...0.5 (util.c:857-869 names this very input). A
// magnitude out of range saturates to +/-Inf or underflows to 0 ("SELECT
// cast('-1e999' AS real)" is -Inf -- nan.test). Only text sqlite3AtoF does not
// accept whole is a failure.
func parseFloatSaturating(sub string) (float64, bool) {
	f, rc := sqliteAtoF([]byte(sub))
	return f, rc > 0
}

// valueToText renders v the way SQLite would coerce it to TEXT: NULL is
// treated as "" (callers needing NULL propagation check for it first),
// INTEGER/REAL are formatted per SQLite's number->text rules, and TEXT/BLOB
// are passed through their raw bytes (a BLOB's bytes reinterpreted as text,
// exactly what "CAST(blob AS TEXT)" and "blob || ..." do).
func valueToText(v Value) string {
	switch v.Typ {
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Float:
		return formatFloatText(v.F)
	case Text, Blob:
		return string(v.S)
	default:
		return ""
	}
}

// formatFloatText is vdbeMemRenderNum's REAL arm (vdbemem.c:126-132):
// sqlite3_str_appendf(&acc, "%!.*g", db->nFpDigit, r), where nFpDigit is 17
// unless SQLITE_DBCONFIG_FP_DIGITS changes it (main.c:3446), which nothing
// here can. The '!' flag is what makes it neither the shortest round-trip
// form nor plain %.17g: sqlite3FpDecode's precision-17 pass shortens a
// trailing run of 9s or 0s only when the shorter digits read back to the
// same double (util.c:1472-1503), so 49.47 renders "49.47" while
// -411606.84739757882 keeps all 17 digits although 16 would round-trip. The
// '!' also keeps the ".0" on a whole number ("3.0", "1.0e+300").
//
// A NaN never reaches SQLite's renderer -- sqlite3VdbeMemSetDouble stores it
// as NULL -- so it renders as the empty string a NULL would.
func formatFloatText(f float64) string {
	if math.IsNaN(f) {
		return ""
	}
	// Width 0 cannot trip printfFloat's too-big check.
	s, _ := printfFloat('g', f, 0, 17, false, 0, false, true, false, false, 0)
	return s
}

// formatFloatLiteral is "%!0.17g", the rendering quote() (sqlite3QuoteValue,
// func.c:1114) and JSON (jsonAppendSqlValue, json.c:813) give a REAL:
// formatFloatText's, except that the '0' flag spells an infinity 9.0e+999 --
// a literal that reads back as the same infinity -- and a NaN null
// (printf.c:556-564).
func formatFloatLiteral(f float64) string {
	// Width 0 cannot trip printfFloat's too-big check.
	s, _ := printfFloat('g', f, 0, 17, false, 0, false, true, true, false, 0)
	return s
}

// ---- CAST ----

// castValueEnc is castValue in a database whose text encoding may not be UTF-8.
// Only the two casts that CROSS the text/blob boundary care, and both were
// verified directly against mattn/go-sqlite3 3.53.3 in both byte orders:
//
//	(utf16le) SELECT hex(CAST('a' AS BLOB))     -> 6100   (UTF-8: 61)
//	(utf16be) SELECT hex(CAST('a' AS BLOB))     -> 0061
//	(utf16le) SELECT hex(CAST(1.5 AS TEXT))     -> 31002E003500
//	(utf16le) SELECT CAST(x'6100' AS TEXT)      -> 'a'
//	(utf16be) SELECT CAST(x'6100' AS TEXT)      -> the single char U+6100
//	(utf16*)  SELECT length(CAST(x'61' AS TEXT))-> 0, an ODD byte count is empty
func castValueEnc(v Value, typ string, enc TextEncoding) Value {
	if v.Typ == Null {
		return v
	}
	switch typ {
	case "TEXT":
		// A TEXT cast of an already-TEXT value is a no-op on the bytes, and real
		// SQLite carries the function subtype (Value.Subtype, record.go) straight through
		// it: "json_quote(CAST(json_array(1) AS TEXT))" answers [1], not the
		// string "[1]" -- likewise for VARCHAR/CHARACTER(n)/CLOB, every spelling
		// with TEXT affinity, and for a doubled cast. A cast that actually
		// CHANGES the value drops it (CAST(... AS NUMERIC) answers 0), which
		// falls out of the other arms building fresh Values.
		if v.Typ == Text {
			return v
		}
		if v.Typ == Blob {
			// The blob's bytes ARE the encoded text, so they are DECODED rather
			// than reinterpreted -- and an odd byte count yields the empty
			// string (see this function's doc comment).
			return Value{Typ: Text, S: decodeTextBytes(enc, v.S)}
		}
		return Value{Typ: Text, S: []byte(valueToText(v))}

	case "BLOB":
		if v.Typ == Blob {
			return v
		}
		// A text value's bytes are its ENCODED form; every other type renders
		// to text first and is then encoded, which is why this goes through
		// encodeTextBytes rather than valueToText alone.
		return Value{Typ: Blob, S: encodeTextBytes(enc, []byte(valueToText(v)))}

	case "INTEGER":
		switch v.Typ {
		case Int:
			return v
		case Float:
			return Value{Typ: Int, I: floatToInt64Trunc(v.F)}
		default:
			// NOT parseNumericPrefix: CAST(TEXT AS INTEGER) has its own,
			// narrower prefix rule -- see parseIntegerPrefix's doc comment.
			i, ok := parseIntegerPrefix(string(v.S))
			if !ok {
				return Value{Typ: Int, I: 0}
			}
			return Value{Typ: Int, I: i}
		}

	case "REAL":
		switch v.Typ {
		case Float:
			return v
		case Int:
			return Value{Typ: Float, F: float64(v.I)}
		default:
			isF, i, f, ok := parseNumericPrefix(string(v.S))
			if !ok {
				return Value{Typ: Float, F: 0}
			}
			if isF {
				return Value{Typ: Float, F: f}
			}
			return Value{Typ: Float, F: float64(i)}
		}

	case "NUMERIC":
		// Unlike applying NUMERIC/INTEGER/REAL *affinity* (which requires the
		// whole string to be a well-formed number or it stays TEXT), CAST(...
		// AS NUMERIC) is lenient exactly like CAST INTEGER/REAL: it takes the
		// longest valid leading numeric prefix and defaults to integer 0 if
		// there isn't one at all (verified against C SQLite: e.g.
		// CAST('Widget' AS NUMERIC) is 0, CAST('42abc' AS NUMERIC) is 42).
		switch v.Typ {
		case Int, Float:
			return v
		case Blob:
			v = Value{Typ: Text, S: v.S} // blob bytes reinterpreted as text first
		}
		if isF, i, f, ok := parseNumericPrefix(string(v.S)); ok {
			if isF {
				// A TEXT source that parses to a REAL still folds to INTEGER
				// when the value is exactly representable as one -- the same
				// rule (and the same narrow bound) NUMERIC affinity applies to
				// text. Verified directly: CAST('-5.0' AS NUMERIC) and
				// CAST('-5.25e+2' AS NUMERIC) are the INTEGERs -5 and -525,
				// while CAST('5.5' ...) and CAST('9223372036854775807.0' ...)
				// stay REAL (e_expr.test). A source that was ALREADY a REAL
				// value keeps its type -- that is the "case Int, Float" return
				// above, matching CAST(-5.0 AS NUMERIC) -> real.
				return foldAlreadyNumeric(Value{Typ: Float, F: f}, affNumeric, intFoldNarrow)
			}
			return Value{Typ: Int, I: i}
		}
		return Value{Typ: Int, I: 0}
	}
	return v
}

func floatToInt64Trunc(f float64) int64 {
	if math.IsNaN(f) {
		return 0
	}
	if f >= 9223372036854775807 {
		return math.MaxInt64
	}
	if f <= -9223372036854775808 {
		return math.MinInt64
	}
	return int64(f)
}

// ---- scalar functions ----

// valueToInt64Trunc is sqlite3_value_int64 -- sqlite3VdbeIntValue
// (vdbemem.c): an INTEGER is itself, a REAL is doubleToInt64, and a TEXT or
// BLOB goes through memIntValue, i.e. sqlite3Atoi64.
//
// The TEXT rule is the one that is easy to get wrong, and this engine did:
// sqlite3Atoi64 reads an INTEGER prefix and stops at the first byte that is
// not a digit, so an EXPONENT is never evaluated. '1e5' is 1, not 100000 --
// measured against the 3.53.3 oracle, where zeroblob('1e5') is one byte,
// substr('abcdefghij','1e5') is the whole string, char('1e5') is U+0001 and
// printf('%d','1e5') is "1". Reading the longest NUMERIC prefix instead (the
// arithmetic coercion, toNumericLoose) answered 100000 for all four, which is
// a wrong answer rather than a gap. It is the same rule CAST(TEXT AS INTEGER)
// follows for the same reason -- both land on sqlite3VdbeIntValue -- and
// parseIntegerPrefix is where this engine already had it.
//
// A REAL magnitude outside int64's range saturates to MinInt64/MaxInt64
// rather than using Go's int64(f) conversion.
func valueToInt64Trunc(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Float:
		return floatToInt64Saturating(v.F)
	case Text, Blob:
		i, _ := parseIntegerPrefix(string(v.S))
		return i // memIntValue ignores sqlite3Atoi64's status; 0 on no digits
	default:
		return 0
	}
}

// floatToInt64Saturating converts f to the nearest representable int64,
// clamping (not wrapping) when f's magnitude is out of range, and treating
// NaN as 0 (defensively; NaN cannot arise from ordinary SQL arithmetic --
// see compareIntFloat's own NaN doc comment).
func floatToInt64Saturating(f float64) int64 {
	switch {
	case math.IsNaN(f):
		return 0
	case f >= 9223372036854775808.0: // >= 2^63: exceeds MaxInt64
		return math.MaxInt64
	case f < -9223372036854775808.0: // < -2^63: below MinInt64
		return math.MinInt64
	default:
		return int64(f)
	}
}
