package engine

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestVMJITDifferential: random programs over every opcode the VM JIT has a
// native form for, seeded with values that fail each of its guards -- NULL,
// REAL, TEXT, BLOB, the integers either side of an overflow and every shift
// amount that leaves 0..63 -- run with the JIT and without. Every row and every
// error must be identical: an instruction the native code gets wrong for ANY
// operand is a wrong answer, and this is the gate that sees it.
func TestVMJITDifferential(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	const nReg = 8
	ints := []int64{0, 1, -1, 2, 7, 63, 64, 65, -63, -64, -65, 100, -100,
		math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1, 1 << 32, -(1 << 32), 3037000500}
	rng := rand.New(rand.NewSource(1))
	native := 0
	for iter := range 4000 {
		var insns []Instruction
		add := func(op OpCode, p1, p2, p3 int, p4 any, p5 uint16) {
			insns = append(insns, Instruction{Op: op, P1: p1, P2: p2, P3: p3, P4: p4, P5: p5})
		}
		r := func() int { return rng.Intn(nReg) }
		for reg := range nReg {
			switch rng.Intn(8) {
			case 0:
				add(OpNull, 0, reg, 0, nil, 0)
			case 1:
				add(OpReal, 0, reg, 0, []float64{0.5, -2, 1e300, 3}[rng.Intn(4)], 0)
			case 2:
				add(OpString8, 0, reg, 0, []string{"", "12", "abc", "-3"}[rng.Intn(4)], 0)
			default:
				add(OpInt64, 0, reg, 0, ints[rng.Intn(len(ints))], 0)
			}
		}
		body := 25
		start := len(insns)
		end := start + body // the ResultRow
		for i := range body {
			pc := start + i
			fwd := pc + 1 + rng.Intn(end-pc) // forward only, up to the ResultRow
			switch rng.Intn(15) {
			case 0:
				add(OpInteger, int(ints[rng.Intn(5)]), r(), 0, nil, 0)
			case 1:
				add(OpInt64, 0, r(), 0, ints[rng.Intn(len(ints))], 0)
			case 2:
				add([]OpCode{OpSCopy, OpCopy}[rng.Intn(2)], r(), r(), 0, nil, 0)
			case 3, 4:
				add([]OpCode{OpAdd, OpSubtract, OpMultiply}[rng.Intn(3)], r(), r(), r(), nil, 0)
			case 5, 6:
				add([]OpCode{OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight}[rng.Intn(4)], r(), r(), r(), nil, 0)
			case 7, 8:
				p5 := []uint16{0, p5JumpIfNull, p5NullEq, p5StoreP2}[rng.Intn(4)]
				p2 := fwd
				if p5 == p5StoreP2 {
					p2 = r()
				}
				add([]OpCode{OpEq, OpNe, OpLt, OpLe, OpGt, OpGe}[rng.Intn(6)], r(), p2, r(), nil, p5)
			case 9:
				add([]OpCode{OpIf, OpIfNot}[rng.Intn(2)], r(), fwd, rng.Intn(2), nil, 0)
			case 10:
				add(OpGoto, 0, fwd, 0, nil, 0)
			case 11:
				add(OpNull, 0, r(), 0, nil, 0)
			case 12:
				add(OpString8, 0, r(), 0, "7", 0)
			default:
				add(OpReal, 0, r(), 0, 1.5, 0)
			}
		}
		add(OpResultRow, 0, nReg, 0, nil, 0)
		// A subroutine round trip after the row: Gosub to a Return.
		g := r()
		add(OpGosub, g, len(insns)+2, 0, nil, 0)
		add(OpGoto, 0, len(insns)+2, 0, nil, 0)
		add(OpReturn, g, 0, 0, nil, 0)
		add(OpResultRow, 0, nReg, 0, nil, 0)
		add(OpHalt, 0, 0, 0, nil, 0)

		prog := &Program{Insns: insns, NReg: nReg, NResultCol: nReg}
		run := func(withJIT bool) ([][]Value, error) {
			st, err := NewProgramStmt(prog)
			if err != nil {
				t.Fatalf("iter %d: %v", iter, err)
			}
			if withJIT && st.m.jx == nil {
				return nil, errNoNative
			}
			if !withJIT {
				st.m.jx = nil
			}
			var rows [][]Value
			for {
				row, err := st.Step()
				if err != nil || row == nil {
					return rows, err
				}
				rows = append(rows, append([]Value(nil), row...))
			}
		}
		want, wantErr := run(false)
		got, gotErr := run(true)
		if gotErr == errNoNative {
			continue // nothing worth entering: vmJITMinWork
		}
		if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) || !reflect.DeepEqual(want, got) {
			t.Fatalf("iter %d: JIT diverges\nprogram %v\nvdbe %v %v\njit  %v %v", iter, insns, want, wantErr, got, gotErr)
		}
		native++
	}
	if native < 3000 {
		t.Fatalf("only %d of 4000 programs had native code", native)
	}
	t.Logf("%d programs with native code agree", native)
}

var errNoNative = errors.New("no native code")

// runBoth runs prog with the JIT and without and fails on any difference.
func runBoth(t *testing.T, name string, prog *Program) [][]Value {
	t.Helper()
	run := func(withJIT bool) ([][]Value, error) {
		st, err := NewProgramStmt(prog)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if withJIT && st.m.jx == nil {
			t.Fatalf("%s: no native code", name)
		}
		if !withJIT {
			st.m.jx = nil
		}
		var rows [][]Value
		for {
			row, err := st.Step()
			if err != nil || row == nil {
				return rows, err
			}
			rows = append(rows, append([]Value(nil), row...))
		}
	}
	want, wantErr := run(false)
	got, gotErr := run(true)
	if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) || !reflect.DeepEqual(want, got) {
		t.Fatalf("%s: JIT diverges\nvdbe %v %v\njit  %v %v", name, want, wantErr, got, gotErr)
	}
	return want
}

// TestVMJITFarRegisters: a register file too large for an immediate offset
// (arm64 reaches 32KB, 682 registers, before it needs a register-offset load),
// with every kind of access -- type byte, value words, the S guard, a copy --
// on registers past that.
func TestVMJITFarRegisters(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	const base = 3000
	prog := &Program{NReg: base + 8, NResultCol: 3, Insns: []Instruction{
		{Op: OpInteger, P1: 0, P2: base + 0},                       // 0: i = 0
		{Op: OpInteger, P1: 0, P2: base + 1},                       // 1: sum = 0
		{Op: OpInteger, P1: 1, P2: base + 2},                       // 2: one = 1
		{Op: OpInt64, P2: base + 3, P4: int64(5000)},               // 3: limit
		{Op: OpAdd, P1: base + 0, P2: base + 1, P3: base + 1},      // 4: sum += i
		{Op: OpMultiply, P1: base + 2, P2: base + 1, P3: base + 4}, // 5: t = sum * one
		{Op: OpAdd, P1: base + 2, P2: base + 0, P3: base + 0},      // 6: i++
		{Op: OpLt, P1: base + 3, P2: 4, P3: base + 0},              // 7: if i < limit goto 4
		{Op: OpCopy, P1: base + 4, P2: base + 5},                   // 8
		{Op: OpResultRow, P1: base, P2: 2},                         // 9
		{Op: OpResultRow, P1: base + 4, P2: 2},                     // 10
		{Op: OpHalt},
	}}
	prog.NResultCol = 2
	rows := runBoth(t, "far registers", prog)
	if len(rows) != 2 || rows[0][1].I != 5000*4999/2 {
		t.Fatalf("answered %v", rows)
	}
}

// TestVMJITReturnAddresses: OP_Return through every address either side of
// the program's end, and through ones no program has. (0 is not one: it
// returns to this same OP_Return, forever, on both paths.)
func TestVMJITReturnAddresses(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	const n = 4
	for _, a := range []int64{n - 3, n - 2, n - 1, n, n + 1, -1, -2, math.MaxInt64, math.MinInt64} {
		prog := &Program{NReg: 1, NResultCol: 1, Insns: []Instruction{
			{Op: OpInt64, P2: 0, P4: a},
			{Op: OpReturn, P1: 0},
			{Op: OpResultRow, P1: 0, P2: 1},
			{Op: OpHalt},
		}}
		runBoth(t, "return to "+strconv.FormatInt(a, 10), prog)
	}
}

// TestVMJITFuel: a loop long enough to spend the fuel several times over, so
// the native code exits mid-loop and is entered again, must still count right.
func TestVMJITFuel(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	const iters = 3*1<<20 + 7
	prog := &Program{NReg: 4, NResultCol: 2, Insns: []Instruction{
		{Op: OpInteger, P1: 0, P2: 0},          // 0: i = 0
		{Op: OpInteger, P1: 0, P2: 1},          // 1: sum = 0
		{Op: OpInteger, P1: 1, P2: 2},          // 2: one = 1
		{Op: OpInt64, P2: 3, P4: int64(iters)}, // 3: limit
		{Op: OpAdd, P1: 0, P2: 1, P3: 1},       // 4: sum += i
		{Op: OpAdd, P1: 2, P2: 0, P3: 0},       // 5: i++
		{Op: OpLt, P1: 3, P2: 4, P3: 0},        // 6: if i < limit goto 4
		{Op: OpResultRow, P1: 0, P2: 2},        // 7
		{Op: OpHalt},
	}}
	rows := runBoth(t, "fuel loop", prog)
	if len(rows) != 1 || rows[0][0].I != iters || rows[0][1].I != iters*(iters-1)/2 {
		t.Fatalf("loop answered %v", rows)
	}
	// The answer cannot show that the code came back to Go between fuel
	// tanks -- only the count of entries can.
	st, err := NewProgramStmt(prog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Step(); err != nil {
		t.Fatal(err)
	}
	if want := iters / jit.VMFuel; st.m.jitEnters < want {
		t.Fatalf("%d entries for %d iterations: the fuel never ran out", st.m.jitEnters, iters)
	}
}

// lowEntryThreshold enters native code wherever there is any, as on amd64 and
// arm64: these tests check what the emitted code computes, and on js/wasm the
// platform's higher threshold would leave most of their programs on the VDBE.
func lowEntryThreshold(t *testing.T) {
	old := vmJITMinWork
	vmJITMinWork = 3
	t.Cleanup(func() { vmJITMinWork = old })
}
