package engine_test

import (
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func iv(n int64) engine.Value { return engine.Value{Typ: engine.Int, I: n} }

func ins(op engine.OpCode, p1, p2, p3 int, p4 any) engine.Instruction {
	return engine.Instruction{Op: op, P1: p1, P2: p2, P3: p3, P4: p4}
}

// TestProgramStmtSteps: a caller-built program pauses at each result row,
// and a value bound between steps is what the next step reads -- a program can
// run as long as it is stepped and take fresh input each time.
func TestProgramStmtSteps(t *testing.T) {
	s, err := engine.NewProgramStmt(&engine.Program{NReg: 2, Insns: []engine.Instruction{
		ins(engine.OpInteger, 0, 0, 0, nil),   // 0: r0 = 0
		ins(engine.OpVariable, 1, 1, 0, nil),  // 1: r1 = ?1
		ins(engine.OpAdd, 1, 0, 0, nil),       // 2: r0 = r0 + r1
		ins(engine.OpResultRow, 0, 1, 0, nil), // 3: emit r0
		ins(engine.OpGoto, 0, 1, 0, nil),      // 4: again
	}})
	if err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for _, step := range []int64{1, 10, 100, -5} {
		s.Bind(1, iv(step))
		row, err := s.Step()
		if err != nil {
			t.Fatal(err)
		}
		total += step
		if len(row) != 1 || row[0].I != total {
			t.Fatalf("after binding %d: %v, want %d", step, row, total)
		}
	}
}

// TestProgramStmtSubroutineAndFunctions: OP_Gosub / OP_Return call a
// subroutine and come back after the call, and a function runs by name or as
// the *ScalarFunction the caller holds.
func TestProgramStmtSubroutineAndFunctions(t *testing.T) {
	triple := &engine.ScalarFunction{Name: "triple", NArg: 1, Fn: func(a []engine.Value) (engine.Value, error) {
		return iv(a[0].I * 3), nil
	}}
	s, err := engine.NewProgramStmt(&engine.Program{NReg: 4, Insns: []engine.Instruction{
		ins(engine.OpInteger, -7, 0, 0, nil),    // 0: r0 = -7
		ins(engine.OpGosub, 3, 5, 0, nil),       // 1: call 5, return address in r3
		ins(engine.OpFunction, 1, 1, 2, triple), // 2: r2 = triple(r1)
		ins(engine.OpResultRow, 1, 2, 0, nil),   // 3: emit r1, r2
		ins(engine.OpHalt, 0, 0, 0, nil),        // 4
		ins(engine.OpFunction, 0, 1, 1, "abs"),  // 5: r1 = abs(r0)
		ins(engine.OpReturn, 3, 0, 0, nil),      // 6: back to 2
	}})
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.Step()
	if err != nil {
		t.Fatal(err)
	}
	if len(row) != 2 || row[0].I != 7 || row[1].I != 21 {
		t.Fatalf("got %v, want [7 21]", row)
	}
	if row, err := s.Step(); row != nil || err != nil {
		t.Fatalf("after the halt: %v %v", row, err)
	}
}

// TestProgramStmtReturnWithoutAddress: OP_Return through a register that holds
// no address falls through with P3 set, and is an error without it.
func TestProgramStmtReturnWithoutAddress(t *testing.T) {
	for _, p3 := range []int{1, 0} {
		s, err := engine.NewProgramStmt(&engine.Program{NReg: 2, Insns: []engine.Instruction{
			ins(engine.OpReturn, 0, 0, p3, nil),
			ins(engine.OpInteger, 9, 1, 0, nil),
			ins(engine.OpResultRow, 1, 1, 0, nil),
		}})
		if err != nil {
			t.Fatal(err)
		}
		row, err := s.Step()
		if p3 == 1 && (err != nil || len(row) != 1 || row[0].I != 9) {
			t.Fatalf("P3=1: %v %v, want a fall-through to 9", row, err)
		}
		if p3 == 0 && err == nil {
			t.Fatal("P3=0: a return with no address was accepted")
		}
	}
}

// TestProgramStmtValidates: a malformed program is refused before it runs.
func TestProgramStmtValidates(t *testing.T) {
	for name, p := range map[string]*engine.Program{
		"register":   {NReg: 1, Insns: []engine.Instruction{ins(engine.OpInteger, 1, 5, 0, nil)}},
		"jump":       {NReg: 1, Insns: []engine.Instruction{ins(engine.OpGoto, 0, 9, 0, nil)}},
		"result row": {NReg: 2, Insns: []engine.Instruction{ins(engine.OpResultRow, 1, 5, 0, nil)}},
		"p4 type":    {NReg: 1, Insns: []engine.Instruction{ins(engine.OpInt64, 0, 0, 0, "x")}},
		"function":   {NReg: 2, Insns: []engine.Instruction{ins(engine.OpFunction, 0, 1, 1, "no_such_fn")}},
		"opcode":     {NReg: 1, Insns: []engine.Instruction{ins(engine.OpOpenRead, 0, 2, 0, nil)}},
		"gosub":      {NReg: 1, Insns: []engine.Instruction{ins(engine.OpGosub, 3, 0, 0, nil)}},
		"halt error": {NReg: 1, Insns: []engine.Instruction{ins(engine.OpHaltError, 0, 0, 0, nil)}},
	} {
		if _, err := engine.NewProgramStmt(p); err == nil || !strings.Contains(err.Error(), "program instruction") {
			t.Errorf("%s: %v, want it refused", name, err)
		}
	}
}

// TestProgramStmtHaltError: OpHaltError's message is the step's error.
func TestProgramStmtHaltError(t *testing.T) {
	st, err := engine.NewProgramStmt(&engine.Program{NReg: 1, Insns: []engine.Instruction{
		ins(engine.OpHaltError, 0, 0, 0, "boom"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Step(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("step: %v, want boom", err)
	}
}
