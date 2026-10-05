package engine

import (
	"math"
	"testing"
)

// TestIntArithAgreesWithEvalArith gates the fast integer arithmetic path, verifying
// it matches the general path across boundary operands and all arithmetic opcodes.
func TestIntArithAgreesWithEvalArith(t *testing.T) {
	// Boundary operands: min, max, zero, edge 32/62-bit values.
	operands := []int64{
		0, 1, -1, 2, -2, 7, -7, 3, 10, 100,
		math.MaxInt64, math.MinInt64,
		math.MaxInt64 - 1, math.MinInt64 + 1,
		1 << 31, -(1 << 31), 1 << 62, -(1 << 62),
	}
	ops := []OpCode{OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder}
	for _, opc := range ops {
		name := arithOpName(opc)
		for _, a := range operands {
			for _, b := range operands {
				l := Value{Typ: Int, I: a}
				r := Value{Typ: Int, I: b}
				want, err := evalArith(name, l, r)
				if err != nil {
					t.Fatalf("%d %s %d: evalArith: %v", a, name, b, err)
				}
				got, ok := intArith(opc, l, r)
				if !ok {
					// Can decline only on overflow, not for valid INTEGER results.
					if want.Typ == Int {
						t.Fatalf("%d %s %d: intArith declined an INTEGER result %d", a, name, b, want.I)
					}
					continue
				}
				if got.Typ != want.Typ || got.I != want.I || got.F != want.F {
					t.Fatalf("%d %s %d: intArith %+v, evalArith %+v", a, name, b, got, want)
				}
			}
		}
	}
}

// TestIntArithDeclinesNonIntegers keeps the guard honest: the arm exists only
// for the MEM_Int/MEM_Int case, so anything else must fall through rather than
// be coerced here (C reaches its coercion through numericType at vdbe.c:1934,
// which is evalArith's job in this engine).
func TestIntArithDeclinesNonIntegers(t *testing.T) {
	nonInt := []Value{
		{Typ: Null},
		{Typ: Float, F: 1},
		{Typ: Text, S: []byte("2")},
		{Typ: Blob, S: []byte{1}},
	}
	one := Value{Typ: Int, I: 1}
	for _, v := range nonInt {
		for _, opc := range []OpCode{OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder} {
			if _, ok := intArith(opc, v, one); ok {
				t.Fatalf("intArith accepted %v as a left operand of %s", v.Typ, arithOpName(opc))
			}
			if _, ok := intArith(opc, one, v); ok {
				t.Fatalf("intArith accepted %v as a right operand of %s", v.Typ, arithOpName(opc))
			}
		}
	}
}

// TestAsciiEqualFoldMatchesAsciiFold is the check behind the collation name
// match (value_compare.go): asciiEqualFold must answer exactly what the
// asciiFold-and-switch it replaced answered, for every name the switch can see.
//
// The cases that matter are the ones a fold-then-compare and a fold-as-you-go
// can disagree on: a name of the right length but wrong content, a name
// differing only in case, and -- the reason this is ASCII-only rather than
// strings.EqualFold -- a name whose Unicode simple fold WOULD match but whose
// ASCII fold must not ("NOCASE" with the Kelvin sign U+212A for its K, which
// EqualFold treats as "k").
func TestAsciiEqualFoldMatchesAsciiFold(t *testing.T) {
	names := []string{
		"BINARY", "binary", "BiNaRy",
		"NOCASE", "nocase", "NoCaSe", "nocasE",
		"RTRIM", "rtrim", "RtRiM",
		"", "N", "NOCAS", "NOCASEX", "BINARZ", "RTRIN",
		"nocasKe", // Kelvin sign: strings.EqualFold would match, SQLite does not
		"É", "café",
	}
	for _, name := range names {
		folded := asciiFold(name, false)
		for _, want := range []string{"NOCASE", "RTRIM", "BINARY"} {
			got := asciiEqualFold(name, want)
			if expect := folded == want; got != expect {
				t.Fatalf("asciiEqualFold(%q, %q) = %v, asciiFold-and-compare = %v",
					name, want, got, expect)
			}
		}
	}
}
