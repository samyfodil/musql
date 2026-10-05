package engine

import "testing"

// TestCompareOpIntegerArmBeatsAffinity pins the ORDER ported from
// vdbe.c:2288 -- the "common case of comparison of two integers" arm runs
// before the affinity block at :2346, so a TEXT comparison affinity never
// reaches two INTEGER operands.
//
// This is the case that was wrong before the port and that no corpus statement
// reached: both operands INTEGER, P5 carrying TEXT affinity. This engine
// stringified both (affinity.go) and compared the text, so "10 < 9" was
// TRUE. C cannot do that: the integer arm answers first, and its own
// TEXT-affinity branch is guarded by `((flags1|flags3) & MEM_Str)!=0` (:2360),
// which two integers do not satisfy.
//
// Runs compareOp directly rather than through SQL because the compiler does not
// currently emit this combination -- the rule is C's, and it has to hold for
// whatever does emit it next.
func TestCompareOpIntegerArmBeatsAffinity(t *testing.T) {
	const (
		regRight  = 1
		regResult = 2
		regLeft   = 3
	)
	for _, c := range []struct {
		name     string
		op       OpCode
		left     int64
		right    int64
		wantTrue bool
	}{
		// "10" < "9" as TEXT, 10 < 9 as integers. The whole point.
		{"10<9", OpLt, 10, 9, false},
		{"9<10", OpLt, 9, 10, true},
		{"10>9", OpGt, 10, 9, true},
		{"10=10", OpEq, 10, 10, true},
		{"10=9", OpEq, 10, 9, false},
		{"10<>9", OpNe, 10, 9, true},
		{"9<=9", OpLe, 9, 9, true},
		{"10<=9", OpLe, 10, 9, false},
		{"9>=10", OpGe, 9, 10, false},
		{"10>=10", OpGe, 10, 10, true},
		// The sign of the comparison must not come from int64 wraparound.
		{"min<max", OpLt, -9223372036854775808, 9223372036854775807, true},
		{"max>min", OpGt, 9223372036854775807, -9223372036854775808, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, aff := range []affinity{affNone, affText, affNumeric, affInteger, affReal} {
				m := &vdbe{regs: make([]Value, 8)}
				m.regs[regLeft] = Value{Typ: Int, I: c.left}
				m.regs[regRight] = Value{Typ: Int, I: c.right}
				op := &Instruction{
					Op: c.op, P1: regRight, P2: regResult, P3: regLeft,
					P5: p5StoreP2 | uint16(aff),
				}
				if jump, _ := m.compareOp(op); jump {
					t.Fatalf("aff %d: p5StoreP2 was set, compareOp asked to jump", aff)
				}
				got := m.regs[regResult]
				if got.Typ != Int {
					t.Fatalf("aff %d: result %+v is not a boolean INTEGER", aff, got)
				}
				if (got.I != 0) != c.wantTrue {
					t.Fatalf("aff %d: %d %s %d = %d, want %v",
						aff, c.left, arithlessOpName(c.op), c.right, got.I, c.wantTrue)
				}
			}
		})
	}
}

// arithlessOpName spells a comparison opcode for a failure message; the engine
// has no such mapping of its own because nothing but a test needs one.
func arithlessOpName(op OpCode) string {
	switch op {
	case OpEq:
		return "="
	case OpNe:
		return "<>"
	case OpLt:
		return "<"
	case OpLe:
		return "<="
	case OpGt:
		return ">"
	case OpGe:
		return ">="
	}
	return "?"
}
