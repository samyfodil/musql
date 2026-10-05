package vdbecc

import (
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/samyfodil/musql/engine"
)

// label names an instruction address that may not be known yet.
type label int

// asm accumulates VDBE instructions. A jump's P2 holds a label number until
// link turns it into an address. Operand order follows the VDBE: arithmetic is
// r[P3] = r[P2] op r[P1], and a comparison jumps to P2 when r[P3] op r[P1].
type asm struct {
	code    []engine.Instruction
	nreg    int
	at      []int // label -> address, -1 while unplaced
	pending []int // instructions whose P2 is a label

	ints   map[int64]int // constant pool, loaded once at startup
	floats map[uint64]int
}

func newAsm() *asm { return &asm{ints: map[int64]int{}, floats: map[uint64]int{}} }

func (a *asm) reg() int {
	a.nreg++
	return a.nreg - 1
}

func (a *asm) label() label {
	a.at = append(a.at, -1)
	return label(len(a.at) - 1)
}

func (a *asm) place(l label) { a.at[l] = len(a.code) }

func (a *asm) op(op engine.OpCode, p1, p2, p3 int, p4 any) {
	a.code = append(a.code, engine.Instruction{Op: op, P1: p1, P2: p2, P3: p3, P4: p4})
}

func (a *asm) jump(op engine.OpCode, p1, p3 int, to label) {
	a.pending = append(a.pending, len(a.code))
	a.op(op, p1, int(to), p3, nil)
}

func (a *asm) goTo(to label) { a.jump(engine.OpGoto, 0, 0, to) }

// ifTrue jumps when r is non-zero.
func (a *asm) ifTrue(r int, to label) { a.jump(engine.OpIf, r, 0, to) }

// k is a register that holds v for the whole run. It must never be written.
func (a *asm) k(v int64) int {
	r, ok := a.ints[v]
	if !ok {
		r = a.reg()
		a.ints[v] = r
	}
	return r
}

func (a *asm) kf(v float64) int {
	b := math.Float64bits(v)
	r, ok := a.floats[b]
	if !ok {
		r = a.reg()
		a.floats[b] = r
	}
	return r
}

func (a *asm) set(d int, v int64) {
	if v == int64(int32(v)) {
		a.op(engine.OpInteger, int(v), d, 0, nil)
	} else {
		a.op(engine.OpInt64, 0, d, 0, v)
	}
}

func (a *asm) mov(src, dst int) {
	if src != dst {
		a.op(engine.OpSCopy, src, dst, 0, nil)
	}
}

// do is d = x op y.
func (a *asm) do(op engine.OpCode, x, y, d int) { a.op(op, y, x, d, nil) }

func (a *asm) plus(x int, c int64, d int) {
	if c == 0 {
		a.mov(x, d)
	} else {
		a.do(engine.OpAdd, x, a.k(c), d)
	}
}

// signExtend narrows r in place to a bits-wide value, sign-extended to 64.
func (a *asm) signExtend(r, bits int) {
	switch {
	case bits >= 64:
	case bits == 1:
		a.do(engine.OpBitAnd, r, a.k(1), r)
	default:
		s := a.k(int64(64 - bits))
		a.do(engine.OpShiftLeft, r, s, r)
		a.do(engine.OpShiftRight, r, s, r)
	}
}

// zeroExtend is d = the low bits of x, as an unsigned value.
func (a *asm) zeroExtend(x, bits, d int) {
	if bits >= 64 {
		a.mov(x, d)
		return
	}
	a.do(engine.OpBitAnd, x, a.k(int64(uint64(1)<<bits-1)), d)
}

var predOps = [...]engine.OpCode{
	PredEQ: engine.OpEq, PredNE: engine.OpNe, PredLT: engine.OpLt, PredLE: engine.OpLe,
	PredGT: engine.OpGt, PredGE: engine.OpGe,
}

// when jumps to l if x p y, comparing as signed: callers zero-extend narrow
// operands of an unsigned predicate first.
func (a *asm) when(p Pred, x, y int, to label) { a.jump(predOps[p.signed()], y, x, to) }

func (a *asm) fn(f *engine.ScalarFunction, first, n, d int) { a.op(engine.OpFunction, first, n, d, f) }

func (a *asm) fail(msg string) { a.op(engine.OpHaltError, 0, 0, 0, msg) }

// loadPool emits the constant pool's initialization, in a stable order.
func (a *asm) loadPool() {
	for _, v := range slices.Sorted(maps.Keys(a.ints)) {
		a.set(a.ints[v], v)
	}
	for _, b := range slices.Sorted(maps.Keys(a.floats)) {
		a.op(engine.OpReal, 0, a.floats[b], 0, math.Float64frombits(b))
	}
}

// resolve resolves every label.
func (a *asm) resolve() error {
	for _, pc := range a.pending {
		to := a.at[a.code[pc].P2]
		if to < 0 {
			return fmt.Errorf("vdbecc: label %d was never placed", a.code[pc].P2)
		}
		a.code[pc].P2 = to
	}
	return nil
}
