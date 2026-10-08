// Arithmetic, bitwise and concatenation over Values, with SQLite's coercion
// and overflow rules.
//
// Integer arithmetic is checked: addInt64/subInt64/mulInt64 report overflow so
// the caller can promote to REAL exactly where SQLite does, rather than
// wrapping. Cross Int/Float promotion goes through float64, so integers beyond
// +/-2^53 mixed against a REAL lose precision as a naive conversion would;
// Int-vs-Int and Float-vs-Float are always exact.
//
// These are opcode bodies -- what OP_Add, OP_BitAnd and OP_Concat actually do
// (vdbe.c) -- not an evaluator.
package engine

import (
	"fmt"
	"math"
	"strconv"
)

func boolValue(b bool) Value {
	if b {
		return Value{Typ: Int, I: 1}
	}
	return Value{Typ: Int, I: 0}
}

// negateValueUnary is SQLite's unary "-" applied to an already-evaluated
// operand: NULL passes through; an integer negates (with the MinInt64
// overflow-to-REAL promotion); a REAL negates via IEEE negation (so -(+0.0)
// is -0.0, a distinction a "0 - x" subtraction would silently lose); anything
// else is coerced to a number via toNumericLoose first. It is the single
// implementation behind OpNegative (the VDBE's unary-minus opcode, vdbe.go).
// Negative zero is the reason it is a dedicated negation rather than codegen for
// "0 - x": that earlier lowering stored +0.0 where the operand's own IEEE
// negation gives -0.0. The read path's -0.0 == +0.0 comparison never noticed,
// but the write path's byte-exact stored double does.
func negateValueUnary(v Value) Value {
	if v.Typ == Null {
		return v
	}
	if v.Typ == Int {
		if v.I == math.MinInt64 {
			return Value{Typ: Float, F: -float64(v.I)} // overflow: promote to real
		}
		return Value{Typ: Int, I: -v.I}
	}
	if v.Typ == Float {
		return Value{Typ: Float, F: -v.F}
	}
	isF, i, f := toNumericArith(v)
	if isF {
		return Value{Typ: Float, F: -f}
	}
	return Value{Typ: Int, I: -i}
}

// bitNotValueUnary is SQLite's unary "~" applied to an already-evaluated
// operand: NULL passes through; every other operand is converted to an
// integer via bitwiseIntOperand and one's-complemented, ALWAYS yielding an
// INTEGER result (typeof(~3.7) is "integer", not "real" -- unlike unary "-",
// which preserves a REAL operand's storage class). It is the single
// implementation behind OpBitNot (the VDBE's bitwise-NOT opcode, vdbe.go),
// mirroring negateValueUnary's role for unary "-". Verified directly against
// C SQLite (mattn/go-sqlite3): ~5 = -6,
// ~NULL = NULL, ~'5' = -6, ~'abc' = -1 (no numeric prefix coerces to 0,
// ~0 = -1), ~3.5 = -4 (int(3.5)=3, ~3=-4), ~-5 = 4.
func bitNotValueUnary(v Value) Value {
	if v.Typ == Null {
		return v
	}
	return Value{Typ: Int, I: ^bitwiseIntOperand(v)}
}

// bitwiseBinaryValue is SQLite's binary "&", "|", "<<", ">>" applied to two
// already-evaluated operands: the shared body of the VDBE's
// OpBitAnd/OpBitOr/OpShiftLeft/OpShiftRight (vdbe.go). Either operand NULL
// yields NULL; otherwise both convert via bitwiseIntOperand (the same
// sqlite3VdbeIntValue rule unary "~" uses) and the result is ALWAYS an
// INTEGER. Shift semantics reproduce OP_ShiftLeft/OP_ShiftRight exactly: a
// NEGATIVE shift count shifts the other direction (clamped to 64 when it is
// <= -64), a count >= 64 yields 0 -- except a right shift of a negative
// value, which yields -1 -- and a right shift sign-extends.
func bitwiseBinaryValue(op string, l, r Value) Value {
	if l.Typ == Null || r.Typ == Null {
		return Value{Typ: Null}
	}
	a, b := bitwiseIntOperand(l), bitwiseIntOperand(r)
	switch op {
	case "&":
		return Value{Typ: Int, I: a & b}
	case "|":
		return Value{Typ: Int, I: a | b}
	}
	if b == 0 {
		return Value{Typ: Int, I: a}
	}
	left := op == "<<"
	if b < 0 {
		left = !left
		if b > -64 {
			b = -b
		} else {
			b = 64
		}
	}
	if b >= 64 {
		if a >= 0 || left {
			return Value{Typ: Int, I: 0}
		}
		return Value{Typ: Int, I: -1}
	}
	u := uint64(a)
	if left {
		u <<= uint(b)
	} else {
		u >>= uint(b)
		if a < 0 {
			u |= ^uint64(0) << uint(64-b)
		}
	}
	return Value{Typ: Int, I: int64(u)}
}

// bitwiseIntOperand converts v to the int64 C SQLite's unary "~" operates
// on -- exactly sqlite3VdbeIntValue()'s own conversion (vdbe.c/vdbeaux.c): an INTEGER passes through unchanged; a REAL truncates toward
// zero, saturating to Min/MaxInt64 when out of range (floatToInt64Saturating,
// the SAME helper valueToInt64Trunc already uses for this exact conversion);
// TEXT/BLOB parse only their longest INTEGER-only leading prefix -- NOT a
// full numeric parse -- via parseIntegerPrefix, the same int-only-prefix
// rule CAST(TEXT AS INTEGER) uses, with no valid prefix treated as 0. This is
// DELIBERATELY narrower than toNumericLoose/valueToInt64Trunc (which parse a
// full REAL-shaped prefix, including '.'/exponent, before truncating): verified
// directly against C SQLite that ~'5.9e10' is -6 (parses only the leading
// "5", exactly as CAST('5.9e10' AS INTEGER) would, ignoring the fractional
// part and exponent entirely), NOT ~59000000000 -- i.e. sqlite3VdbeIntValue's
// TEXT/BLOB path is sqlite3Atoi64-based (integer-only), never routed through
// the general REAL-parsing coercion arithmetic/unary +/- use.
func bitwiseIntOperand(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Float:
		return floatToInt64Saturating(v.F)
	case Text, Blob:
		i, ok := parseIntegerPrefix(string(v.S))
		if !ok {
			return 0
		}
		return i
	default:
		return 0
	}
}

// finalizeIn combines the membership/NULL scan results (anyTrue, anyNullCmp)
// with the NOT flag into IN's final three-valued result: a definite match
// (anyTrue) always wins over NULL-ambiguity; NOT negates a definite result
// but leaves NULL as NULL (NOT NULL is NULL). Shared by the scalar and
// row-value IN opcode bodies (inSubMembership, vdbe.go).
func finalizeIn(anyTrue, anyNullCmp, not bool) Value {
	var res Value
	switch {
	case anyTrue:
		res = boolValue(true)
	case anyNullCmp:
		res = Value{Typ: Null}
	default:
		res = boolValue(false)
	}
	if not {
		if res.Typ == Null {
			return res
		}
		return boolValue(!isTruthy(res))
	}
	return res
}

// concatValues implements the "||" operator's value-level semantics (the body of
// the VDBE's OpConcat, reached through concatValuesEnc at vdbe.go): NULL if
// either operand is NULL; a raw byte concatenation when BOTH operands are blobs;
// else a TEXT concatenation of each operand's text rendering.
func concatValues(l, r Value) Value {
	if l.Typ == Null || r.Typ == Null {
		return Value{Typ: Null}
	}
	// "||" always yields TEXT, even when BOTH operands are blobs: verified
	// directly that "typeof(x'0123' || x'4567')" is text and
	// "quote(x'41' || x'42')" is 'AB' (pragma.test's t4, whose
	// "INSERT INTO t4(b) SELECT b||b||b||b FROM t4" stores TEXT). Only NULL
	// short-circuits, above. A blob's bytes are taken as text unchanged,
	// exactly like CAST(blob AS TEXT).
	// One buffer, sized up front, each operand's text appended straight in:
	// the same bytes as valueToText(l)+valueToText(r), without the two
	// intermediate strings, their concatenation and its copy back to bytes.
	buf := make([]byte, 0, concatTextLen(l)+concatTextLen(r))
	return Value{Typ: Text, S: appendValueText(appendValueText(buf, l), r)}
}

// appendValueText appends valueToText(v)'s bytes to b.
func appendValueText(b []byte, v Value) []byte {
	switch v.Typ {
	case Int:
		return strconv.AppendInt(b, v.I, 10)
	case Text, Blob:
		return append(b, v.S...)
	case Float:
		return append(b, formatFloatText(v.F)...)
	}
	return b
}

// concatTextLen is an upper bound on len(valueToText(v)), for sizing.
func concatTextLen(v Value) int {
	switch v.Typ {
	case Int:
		return 20
	case Text, Blob:
		return len(v.S)
	case Float:
		return 32
	}
	return 0
}

// concatValuesEnc is concatValues in a database whose encoding is UTF-16.
//
// OP_Concat joins the operands' RAW BYTES -- a blob's verbatim, a text's in the
// database encoding -- and only then tags the result TEXT, so a UTF-16 result
// loses an odd trailing byte to sqlite3VdbeMemTranslate's "pMem->n &= ~1". This
// engine holds TEXT as UTF-8, so it has to decode the CONCATENATION rather than
// concatenate the decodings; the two differ exactly when a blob's byte count is
// odd or its bytes split a code unit. Verified directly against
// mattn/go-sqlite3 3.53.3:
//
//	encoding    expression                       hex(result)
//	UTF-16le    x'610062006300' || 'Z'           6100620063005A00   (length 4)
//	UTF-16be    x'610062006300' || 'Z'           610062006300005A
//	UTF-16le    x'61' || 'Z'                     615A               -- 3 bytes, TRUNCATED
//	UTF-16be    x'61' || 'Z'                     6100               -- ditto, other order
//	UTF-16le    x'e6bca2' || 'Z'                 E6BCA25A           -- 5 bytes, truncated
//	UTF-8       x'610062006300' || 'Z'           6100620063005A     -- no rounding at all
//
// musql answered 6100620063005A00 as 6100000062000000630000005A00: it had
// re-encoded each of the blob's bytes as its own character. Found by the
// round-24 ATTACH/encoding stream while fixing the FUNCTION boundary
// (coerceUTF16BlobArgs), which is the same rule one layer up.
func concatValuesEnc(l, r Value, enc TextEncoding) Value {
	if !isUTF16(enc) || (l.Typ != Blob && r.Typ != Blob) {
		return concatValues(l, r)
	}
	if l.Typ == Null || r.Typ == Null {
		return Value{Typ: Null}
	}
	raw := make([]byte, 0, len(l.S)+len(r.S)+8)
	raw = append(raw, concatRawBytes(l, enc)...)
	raw = append(raw, concatRawBytes(r, enc)...)
	// decodeTextBytes applies the "&^ 1" truncation itself.
	return Value{Typ: Text, S: decodeTextBytes(enc, raw)}
}

// concatRawBytes is the byte string OP_Concat appends for one operand: a blob's
// own bytes, or any other value's text rendered in the database encoding.
func concatRawBytes(v Value, enc TextEncoding) []byte {
	if v.Typ == Blob {
		return v.S
	}
	return encodeTextBytes(enc, []byte(valueToText(v)))
}

// evalArith is evalArithRaw plus C SQLite's NaN rule. SQLite has no NaN
// storage class: sqlite3VdbeMemSetDouble (vdbemem.c) turns any NaN REAL into
// NULL at the moment it is stored into a register, so a NaN can never be
// observed by a query. Every arithmetic REAL result flows through here, which
// is this engine's equivalent choke point -- verified directly against real
// SQLite that "1e308*1e308 - 1e308*1e308" (Inf - Inf) is NULL, not NaN, while
// the plain infinities ("1e308*1e308") survive as +Inf/-Inf unchanged.
func evalArith(op string, l, r Value) (Value, error) {
	v, err := evalArithRaw(op, l, r)
	if err == nil && v.Typ == Float && math.IsNaN(v.F) {
		return Value{Typ: Null}, nil
	}
	return v, err
}

// intArith is OP_Add's integer arm, vdbe.c:1908-1930 -- the `int_math` block
// guarded by `if( (type1 & type2 & MEM_Int)!=0 )`, where both operands are
// already INTEGERs and the operator is selected by a switch on the OPCODE.
// SQLite reaches it before any coercion, before any NULL test, and without ever
// naming the operator as text; this engine's general path (evalArith) reaches
// the same answers through arithOpName + a string switch, which is a departure
// from the C that costs real time in the innermost loop of every expression.
// Measured (BenchmarkExprCeiling, 100k-row scan of
// "v*2 + k*3 - sec > 1000000 AND bid < 90000 AND k <> 7", four of these per
// row): 120.3ms -> 94.3ms per scan, -21.6%. BenchmarkW4Ceiling, whose predicate
// contains no arithmetic at all, is the control and did not move.
//
// ok=false means "not this arm" -- either operand non-INTEGER, or an integer
// overflow, which is C's `goto fp_math`. The caller falls through to evalArith,
// which recomputes the identical REAL result the C's fp_math produces, so this
// is a fast path and never a second definition of arithmetic.
//
// Divide and Remainder carry their own C rules verbatim (vdbe.c:1916-1927): a
// zero divisor is NULL, SMALLEST_INT64/-1 escapes to fp_math, and "iA==-1 ->
// iA=1" guards the remainder's undefined case.
func intArith(opc OpCode, l, r Value) (Value, bool) {
	if l.Typ != Int || r.Typ != Int {
		return Value{}, false
	}
	// C names the RIGHT operand iA (pIn1) and the LEFT iB (pIn2), and computes
	// iB op= iA; this engine's opcode is likewise r[P3] = r[P2] op r[P1].
	iB, iA := l.I, r.I
	switch opc {
	case OpAdd:
		if v, ok := addInt64(iB, iA); ok {
			return Value{Typ: Int, I: v}, true
		}
	case OpSubtract:
		if v, ok := subInt64(iB, iA); ok {
			return Value{Typ: Int, I: v}, true
		}
	case OpMultiply:
		if v, ok := mulInt64(iB, iA); ok {
			return Value{Typ: Int, I: v}, true
		}
	case OpDivide:
		if iA == 0 {
			return Value{Typ: Null}, true
		}
		if iA == -1 && iB == math.MinInt64 {
			return Value{}, false // goto fp_math
		}
		return Value{Typ: Int, I: iB / iA}, true
	case OpRemainder:
		if iA == 0 {
			return Value{Typ: Null}, true
		}
		if iA == -1 {
			iA = 1
		}
		return Value{Typ: Int, I: iB % iA}, true
	}
	return Value{}, false
}

func evalArithRaw(op string, l, r Value) (Value, error) {
	if l.Typ == Null || r.Typ == Null {
		return Value{Typ: Null}, nil
	}
	lIsF, li, lf := toNumericArith(l)
	rIsF, ri, rf := toNumericArith(r)

	switch op {
	case "%":
		// "%" truncates BOTH operands to integers and computes an integer
		// remainder -- but its RESULT TYPE is REAL whenever either operand was
		// real, integer only when both were integers. Verified directly against
		// mattn/go-sqlite3: 5%2 is the integer 1, while 1.5%2, 5%2.0, 5.0%2.0,
		// '5.0'%2 and 7.9%3 are all the REAL 1.0 and -7.5%3 is the REAL -1.0 --
		// the values are the truncated integer remainder in every case, so only
		// the storage class differs. This engine returned Int unconditionally.
		// Both operands go through sqlite3VdbeIntValue, NOT the numeric
		// coercion that decided the result's storage class: OP_Remainder's
		// fp arm is "iA = sqlite3VdbeIntValue(pIn1); iB =
		// sqlite3VdbeIntValue(pIn2)" (vdbe.c). For a TEXT operand those two
		// differ -- numericType reads '1e308' as the REAL 1e308 (so the
		// result is real), while sqlite3VdbeIntValue reads its INTEGER
		// prefix, 1. So "5 % '1e308'" is 0.0, not 5.0.
		li2 := valueToInt64Trunc(l)
		ri2 := valueToInt64Trunc(r)
		if ri2 == 0 {
			return Value{Typ: Null}, nil
		}
		if ri2 == -1 {
			// vdbe.c's "if( iA==-1 ) iA = 1;", both arms of OP_Remainder.
			// It guards C's undefined SMALLEST_INT64 % -1; Go defines that
			// as 0 either way, so this only says what the C says.
			ri2 = 1
		}
		if lIsF || rIsF {
			return Value{Typ: Float, F: float64(li2 % ri2)}, nil
		}
		return Value{Typ: Int, I: li2 % ri2}, nil

	case "/":
		if !lIsF && !rIsF {
			if ri == 0 {
				return Value{Typ: Null}, nil
			}
			if li == math.MinInt64 && ri == -1 {
				return Value{Typ: Float, F: -float64(li)}, nil
			}
			return Value{Typ: Int, I: li / ri}, nil
		}
		rF := floatOf(rIsF, ri, rf)
		if rF == 0 {
			return Value{Typ: Null}, nil
		}
		return Value{Typ: Float, F: floatOf(lIsF, li, lf) / rF}, nil

	case "+":
		if !lIsF && !rIsF {
			if sum, ok := addInt64(li, ri); ok {
				return Value{Typ: Int, I: sum}, nil
			}
			return Value{Typ: Float, F: float64(li) + float64(ri)}, nil
		}
		return Value{Typ: Float, F: floatOf(lIsF, li, lf) + floatOf(rIsF, ri, rf)}, nil

	case "-":
		if !lIsF && !rIsF {
			if diff, ok := subInt64(li, ri); ok {
				return Value{Typ: Int, I: diff}, nil
			}
			return Value{Typ: Float, F: float64(li) - float64(ri)}, nil
		}
		return Value{Typ: Float, F: floatOf(lIsF, li, lf) - floatOf(rIsF, ri, rf)}, nil

	case "*":
		if !lIsF && !rIsF {
			if prod, ok := mulInt64(li, ri); ok {
				return Value{Typ: Int, I: prod}, nil
			}
			return Value{Typ: Float, F: float64(li) * float64(ri)}, nil
		}
		return Value{Typ: Float, F: floatOf(lIsF, li, lf) * floatOf(rIsF, ri, rf)}, nil
	}
	return Value{}, fmt.Errorf("engine: unknown arithmetic operator %q", op)
}

func floatOf(isFloat bool, i int64, f float64) float64 {
	if isFloat {
		return f
	}
	return float64(i)
}

func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (a >= 0 && b >= 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		return 0, false
	}
	return s, true
}

func subInt64(a, b int64) (int64, bool) {
	if b == math.MinInt64 {
		// -b is not representable, so addInt64 can't be used -- but a - b is
		// a + 2**63, which still FITS whenever a is negative (it lands in
		// [0, 2**63)) and only overflows for a >= 0. Two's-complement
		// subtraction already produces the right value in the fitting case.
		// Verified against C SQLite: typeof(-9223372036854775808 -
		// -9223372036854775808) is 'integer' (0), while
		// "0 - -9223372036854775808" does overflow to the REAL 9.223372e18.
		if a < 0 {
			return a - b, true
		}
		return 0, false
	}
	return addInt64(a, -b)
}

func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, false
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

// ---- comparison, ordering, affinity ----
