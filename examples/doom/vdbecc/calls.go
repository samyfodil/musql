package vdbecc

import (
	"strings"

	"github.com/samyfodil/musql/engine"
)

// builtins are callees the compiler implements itself rather than calling.
var builtins = map[string]func(*gen, *CallInstr){
	"vdbe_fetch_input": (*gen).fetchInput,
	"vdbe_present":     (*gen).present,
	"exit":             (*gen).exitCall,
	"abort":            (*gen).exitCall,
	"memcpy":           (*gen).memOp,
	"memmove":          (*gen).memOp,
	"memset":           (*gen).memOp,
}

// intrinsics are LLVM's, matched by prefix: their names end in type suffixes.
var intrinsics = []struct {
	prefix string
	lower  func(*gen, *CallInstr)
}{
	{"llvm.va_start", (*gen).vaStart},
	{"llvm.va_copy", (*gen).vaCopy},
	{"llvm.va_end", nil},
	{"llvm.lifetime", nil},
	{"llvm.dbg", nil},
	{"llvm.assume", nil},
	{"llvm.experimental", nil},
	{"llvm.memcpy", (*gen).memOp},
	{"llvm.memmove", (*gen).memOp},
	{"llvm.memset", (*gen).memOp},
	{"llvm.fshl.", (*gen).rotate},
	{"llvm.usub.sat.", (*gen).subSaturate},
	{"llvm.fmuladd.", (*gen).mulAdd},
	{"llvm.fabs.", (*gen).abs},
	{"llvm.abs.", (*gen).abs},
	{"llvm.smax.", (*gen).minMax},
	{"llvm.smin.", (*gen).minMax},
	{"llvm.umax.", (*gen).minMax},
	{"llvm.umin.", (*gen).minMax},
}

func (g *gen) call(c *CallInstr) {
	if c.Callee == "" {
		g.mov(g.r(c.Target), g.callee)
		g.invoke(c, g.dispatch)
		return
	}
	if lower, ok := builtins[c.Callee]; ok && (!strings.HasPrefix(c.Callee, "mem") || len(c.Args) >= 3) {
		lower(g, c)
		return
	}
	if strings.HasPrefix(c.Callee, "llvm.") {
		for _, in := range intrinsics {
			if strings.HasPrefix(c.Callee, in.prefix) {
				if in.lower != nil {
					in.lower(g, c)
				}
				return
			}
		}
		g.failf("unsupported intrinsic @%s", c.Callee)
		return
	}
	p, ok := g.plans[c.Callee]
	if !ok {
		g.failf("call to undefined function @%s", c.Callee)
		return
	}
	g.invoke(c, p.entry)
}

// invoke passes c's arguments and Gosubs to.
func (g *gen) invoke(c *CallInstr, to label) {
	if len(c.Args) > len(g.args) {
		g.failf("a call with %d arguments, at most %d", len(c.Args), len(g.args))
		return
	}
	for i, a := range c.Args {
		g.mov(g.r(a.Val), g.args[i])
	}
	g.jump(engine.OpGosub, g.link, 0, to)
	if c.Dst != "" {
		g.mov(g.ret, g.dst(c.Dst))
	}
}

// result is c's destination, which the intrinsic needs.
func (g *gen) result(c *CallInstr) int {
	if c.Dst == "" {
		g.failf("@%s's result is unused", c.Callee)
	}
	return g.dst(c.Dst)
}

// fetchInput fills the caller's buffer from ?1..?10, as 8-byte slots: the host
// binds them while the program is paused at a frame, and an unbound one reads
// as 0.
func (g *gen) fetchInput(c *CallInstr) {
	g.mov(g.r(c.Args[0].Val), g.t1)
	for i := range 10 {
		bound := g.label()
		g.op(engine.OpVariable, i+1, g.t3, 0, nil)
		g.jump(engine.OpNotNull, g.t3, 0, bound)
		g.set(g.t3, 0)
		g.place(bound)
		g.plus(g.t1, int64(i)*8, g.t2)
		g.store(g.t2, 8, g.t3)
	}
}

// present emits a frame as a result row; the next Step resumes the program.
func (g *gen) present(*CallInstr) {
	if g.opts.Frame == "" {
		g.failf("vdbe_present needs Options.Frame")
		return
	}
	g.set(g.out, 0)
	g.emitRow()
}

// exitCall ends the program from any depth with a recognizable result: the
// exit status minus a million, or minus two million for abort.
func (g *gen) exitCall(c *CallInstr) {
	if c.Callee == "exit" && len(c.Args) > 0 {
		g.plus(g.r(c.Args[0].Val), -1_000_000, g.ret)
	} else {
		g.set(g.ret, -2_000_000)
	}
	g.goTo(g.exit)
}

// memOp is memcpy, memmove or memset, a single call over (dst, src|byte, n).
// The libc forms return dst.
func (g *gen) memOp(c *CallInstr) {
	for i, w := range []int{g.w0, g.w1, g.w2} {
		g.mov(g.r(c.Args[i].Val), w)
	}
	f := g.mem.memmove
	if strings.Contains(c.Callee, "memset") {
		f = g.mem.memset
	}
	g.fn(f, g.w0, 3, g.sink)
	if c.Dst != "" {
		g.mov(g.r(c.Args[0].Val), g.dst(c.Dst))
	}
}

// vaStart points the va_list past the named parameters' slots in the spill
// area the prologue wrote.
func (g *gen) vaStart(c *CallInstr) {
	if g.p.vaSpill < 0 {
		g.failf("va_start in a function that is not variadic")
		return
	}
	g.mov(g.r(c.Args[0].Val), g.t1)
	first := int64(len(g.p.preserve))*8 + g.p.vaSpill + int64(len(g.p.fn.Params))*8
	g.plus(g.sp, first, g.t2)
	g.store(g.t1, 8, g.t2)
}

func (g *gen) vaCopy(c *CallInstr) {
	g.mov(g.r(c.Args[0].Val), g.t1)
	g.load(g.r(c.Args[1].Val), 8, true, g.t3)
	g.store(g.t1, 8, g.t3)
}

// rotate is fshl(x, y, k): (x << k) | (y >> (bits-k)), with k taken mod bits.
func (g *gen) rotate(c *CallInstr) {
	bits := intBits(c.Args[0].Type)
	d := g.result(c)
	g.mov(g.r(c.Args[0].Val), g.t1)
	g.mov(g.r(c.Args[1].Val), g.t2)
	g.do(engine.OpBitAnd, g.r(c.Args[2].Val), g.k(int64(bits-1)), d)
	none, done := g.label(), g.label()
	g.when(PredEQ, d, g.k(0), none)
	g.do(engine.OpShiftLeft, g.t1, d, g.t1)
	g.do(engine.OpSubtract, g.k(int64(bits)), d, g.t3)
	g.zeroExtend(g.t2, bits, g.t2)
	g.do(engine.OpShiftRight, g.t2, g.t3, g.t2)
	g.do(engine.OpBitOr, g.t1, g.t2, d)
	g.signExtend(d, bits)
	g.goTo(done)
	g.place(none)
	g.mov(g.t1, d)
	g.place(done)
}

func (g *gen) subSaturate(c *CallInstr) {
	bits := intBits(c.Args[0].Type)
	d := g.result(c)
	g.zeroExtend(g.r(c.Args[0].Val), bits, g.t1)
	g.zeroExtend(g.r(c.Args[1].Val), bits, g.t2)
	floor, done := g.label(), g.label()
	g.when(PredLT, g.t1, g.t2, floor)
	g.do(engine.OpSubtract, g.t1, g.t2, d)
	g.signExtend(d, bits)
	g.goTo(done)
	g.place(floor)
	g.set(d, 0)
	g.place(done)
}

func (g *gen) mulAdd(c *CallInstr) {
	d := g.result(c)
	g.do(engine.OpMultiply, g.r(c.Args[0].Val), g.r(c.Args[1].Val), g.t1)
	g.do(engine.OpAdd, g.t1, g.r(c.Args[2].Val), d)
}

func (g *gen) abs(c *CallInstr) {
	d := g.result(c)
	g.mov(g.r(c.Args[0].Val), d)
	done := g.label()
	g.when(PredGE, d, g.k(0), done)
	g.op(engine.OpNegative, d, d, 0, nil)
	g.place(done)
}

func (g *gen) minMax(c *CallInstr) {
	bits := intBits(c.Args[0].Type)
	unsigned := strings.HasPrefix(c.Callee, "llvm.u")
	d := g.result(c)
	g.mov(g.r(c.Args[0].Val), g.t1)
	g.mov(g.r(c.Args[1].Val), g.t2)
	if unsigned {
		g.zeroExtend(g.t1, bits, g.t1)
		g.zeroExtend(g.t2, bits, g.t2)
	}
	keepFirst := PredLE
	if strings.Contains(c.Callee, "max.") {
		keepFirst = PredGE
	}
	first, done := g.label(), g.label()
	g.when(keepFirst, g.t1, g.t2, first)
	g.mov(g.t2, d)
	g.goTo(done)
	g.place(first)
	g.mov(g.t1, d)
	g.place(done)
	if unsigned {
		g.signExtend(d, bits)
	}
}
