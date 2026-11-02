//go:build amd64

package jit

import (
	"fmt"
	"math"

	"github.com/samyfodil/musql/internal/fir"
)

// Lowering a fir kernel to x86-64, inline in the code around it.
//
// Each value gets one home for its whole life: a register of its class, or a
// spill slot. A use in the other class (a double's bits read as an integer)
// moves it across. Constants are not homed: each use loads one, from the
// constant pool (doubles) or as an immediate (integers). The first two
// integer and three float registers the caller offers are kept back as
// scratch, for operands that are not in a register and for results whose home
// is a slot.

// firLoc is where a value lives outside the kernel.
type firLoc struct {
	mem  bool
	xmm  bool
	reg  Reg  // a general register, or the base of mem
	xr   XReg // an XMM register
	disp int32
}

func firMem(base Reg, disp int32) firLoc { return firLoc{mem: true, reg: base, disp: disp} }

// firEnv is what the code around a kernel hands it.
type firEnv struct {
	args []firLoc // each argument's double
	out  firLoc   // where the answer goes
	exit string   // where an Exit jumps
	gprs []Reg    // general registers the kernel may change: 2 or more
	xmms []XReg   // XMM registers it may change: 4 or more
	// spill slots: the words at spillBase+spillDisp, 8 apart.
	spillBase Reg
	spillDisp int32
	spillN    int
	prefix    string // label prefix, unique in the program
	pool      *firPool
}

// firPool is the constants a program's kernels load, laid out after its code.
type firPool struct {
	labels map[[2]uint64]string
	order  [][2]uint64
}

func newFirPool() *firPool { return &firPool{labels: map[[2]uint64]string{}} }

// label names the 16 bytes lo, hi in the pool (16-aligned, so ANDPD and
// XORPD may read them).
func (p *firPool) label(lo, hi uint64) string {
	k := [2]uint64{lo, hi}
	if l, ok := p.labels[k]; ok {
		return l
	}
	l := "fp" + itoa(len(p.order))
	p.labels[k] = l
	p.order = append(p.order, k)
	return l
}

func (p *firPool) emit(a *Asm) {
	if p == nil || len(p.order) == 0 {
		return
	}
	a.Align(16)
	for _, k := range p.order {
		a.Label(p.labels[k])
		for _, w := range k {
			a.emit(byte(w), byte(w>>8), byte(w>>16), byte(w>>24), byte(w>>32), byte(w>>40), byte(w>>48), byte(w>>56))
		}
	}
}

// firAmd64 is one kernel being emitted.
type firAmd64 struct {
	a    *Asm
	f    *fir.Func
	env  *firEnv
	feat cpuFeatures
	home []firHome
	gpr  []Reg  // allocatable general registers
	xmm  []XReg // allocatable XMM registers
	t0   Reg
	t1   Reg
	s0   XReg
	s1   XReg
	s2   XReg
	n    int  // labels made
	fmod bool // the kernel has an OpFMod
}

// emitFirAmd64 emits f inline: arguments read from env.args, the answer
// written to env.out, an Exit jumping to env.exit.
func emitFirAmd64(a *Asm, f *fir.Func, env *firEnv, feat cpuFeatures) error {
	if len(env.gprs) < 2 || len(env.xmms) < 4 || len(env.args) != f.NArgs {
		return fmt.Errorf("jit: kernel %s: too few registers", f.Name)
	}
	if f.Fused && !feat.fma {
		return fmt.Errorf("jit: kernel %s is built fused and this CPU has no FMA", f.Name)
	}
	e := &firAmd64{a: a, f: f, env: env, feat: feat,
		t0: env.gprs[0], t1: env.gprs[1], s0: env.xmms[0], s1: env.xmms[1], s2: env.xmms[2],
		gpr: env.gprs[2:], xmm: env.xmms[3:]}
	if err := e.check(f.Body); err != nil {
		return err
	}
	if err := e.allocate(); err != nil {
		return err
	}
	end := e.label("end")
	if err := e.steps(f.Body, end); err != nil {
		return err
	}
	a.Label(end)
	return nil
}

// check refuses an op this CPU cannot run.
func (e *firAmd64) check(steps []fir.Step) error {
	for _, st := range steps {
		switch st.Op {
		case fir.OpFMA:
			if !e.feat.fma {
				return fmt.Errorf("jit: kernel %s needs FMA", e.f.Name)
			}
		case fir.OpRoundEven, fir.OpFloor, fir.OpCeil, fir.OpTrunc:
			if !e.feat.round {
				return fmt.Errorf("jit: kernel %s needs SSE4.1", e.f.Name)
			}
		case fir.OpFMod:
			// FNSTSW writes AX, which must be scratch.
			if e.t0 != RAX || e.env.spillN < 3 {
				return fmt.Errorf("jit: kernel %s: fmod needs RAX and two spill slots", e.f.Name)
			}
			e.fmod = true
		case fir.OpIf:
			if err := e.check(st.Then); err != nil {
				return err
			}
			if err := e.check(st.Else); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *firAmd64) label(s string) string {
	e.n++
	return e.env.prefix + s + itoa(e.n)
}

// allocate gives every value a home (firAllocate), keeping the last two
// spill slots for fmod's x87 transfers when the kernel has one.
func (e *firAmd64) allocate() error {
	n := e.env.spillN
	if e.fmod {
		n -= 2
	}
	home, err := firAllocate(e.f, len(e.gpr), len(e.xmm), n)
	e.home = home
	return err
}

func (e *firAmd64) slotDisp(i int) int32 { return e.env.spillDisp + int32(i)*8 }

// fget is a register holding v as a double: its own, or scratch s.
func (e *firAmd64) fget(v fir.Val, s XReg) XReg {
	h := e.home[v]
	switch {
	case h.isK:
		e.a.MovsdRIP(s, e.env.pool.label(h.bits, 0))
		return s
	case h.slot >= 0:
		e.a.MovsdLoad(s, e.env.spillBase, e.slotDisp(h.slot))
		return s
	case h.class == fir.F:
		return e.xmm[h.reg]
	}
	e.a.MovqXR(s, e.gpr[h.reg])
	return s
}

// iget is a register holding v's bits: its own, or scratch t.
func (e *firAmd64) iget(v fir.Val, t Reg) Reg {
	h := e.home[v]
	switch {
	case h.isK:
		e.movImm(t, h.bits)
		return t
	case h.slot >= 0:
		e.a.MovRegMemD(t, e.env.spillBase, e.slotDisp(h.slot))
		return t
	case h.class == fir.I:
		return e.gpr[h.reg]
	}
	e.a.MovqRX(t, e.xmm[h.reg])
	return t
}

// fset puts v's double in r.
func (e *firAmd64) fset(r XReg, v fir.Val) {
	h := e.home[v]
	switch {
	case h.isK:
		e.a.MovsdRIP(r, e.env.pool.label(h.bits, 0))
	case h.slot >= 0:
		e.a.MovsdLoad(r, e.env.spillBase, e.slotDisp(h.slot))
	case h.class == fir.I:
		e.a.MovqXR(r, e.gpr[h.reg])
	case e.xmm[h.reg] != r:
		e.a.Movapd(r, e.xmm[h.reg])
	}
}

// iset puts v's bits in r.
func (e *firAmd64) iset(r Reg, v fir.Val) {
	h := e.home[v]
	switch {
	case h.isK:
		e.movImm(r, h.bits)
	case h.slot >= 0:
		e.a.MovRegMemD(r, e.env.spillBase, e.slotDisp(h.slot))
	case h.class == fir.F:
		e.a.MovqRX(r, e.xmm[h.reg])
	case e.gpr[h.reg] != r:
		e.a.MovRegReg(r, e.gpr[h.reg])
	}
}

func (e *firAmd64) movImm(r Reg, u uint64) {
	if u == 0 {
		e.a.XorRegReg(r, r)
		return
	}
	e.a.MovRegImm64(r, int64(u))
}

// fdst is the register a double result is computed in.
func (e *firAmd64) fdst(v fir.Val) XReg {
	if h := e.home[v]; h.slot < 0 && h.reg >= 0 && h.class == fir.F {
		return e.xmm[h.reg]
	}
	return e.s0
}

func (e *firAmd64) idst(v fir.Val) Reg {
	if h := e.home[v]; h.slot < 0 && h.reg >= 0 && h.class == fir.I {
		return e.gpr[h.reg]
	}
	return e.t0
}

// fput stores a double result computed in r into v's home.
func (e *firAmd64) fput(v fir.Val, r XReg) {
	h := e.home[v]
	switch {
	case h.slot >= 0:
		e.a.MovsdStore(e.env.spillBase, e.slotDisp(h.slot), r)
	case e.xmm[h.reg] != r:
		e.a.Movapd(e.xmm[h.reg], r)
	}
}

func (e *firAmd64) iput(v fir.Val, r Reg) {
	h := e.home[v]
	switch {
	case h.slot >= 0:
		e.a.MovMemDReg(e.env.spillBase, e.slotDisp(h.slot), r)
	case h.class == fir.F:
		e.a.MovqXR(e.xmm[h.reg], r)
	case e.gpr[h.reg] != r:
		e.a.MovRegReg(e.gpr[h.reg], r)
	}
}

// imm32 reports whether v is a constant an instruction can carry, sign-extended.
func (e *firAmd64) imm32(v fir.Val) (int32, bool) {
	h := e.home[v]
	if !h.isK || int64(h.bits) != int64(int32(h.bits)) {
		return 0, false
	}
	return int32(h.bits), true
}

func (e *firAmd64) steps(steps []fir.Step, end string) error {
	for i := range steps {
		if err := e.step(&steps[i], end); err != nil {
			return err
		}
	}
	return nil
}

func (e *firAmd64) step(st *fir.Step, end string) error {
	a := e.a
	switch st.Op {
	case fir.OpConst:
	case fir.OpArg:
		src := e.env.args[st.Imm]
		switch {
		case src.mem:
			r := e.fdst(st.Dst)
			a.MovsdLoad(r, src.reg, src.disp)
			e.fput(st.Dst, r)
		case src.xmm:
			e.fput(st.Dst, src.xr)
		default:
			e.iput(st.Dst, src.reg)
		}
	case fir.OpCopy:
		if e.f.Class[st.Dst] == fir.F {
			r := e.fdst(st.Dst)
			e.fset(r, st.A)
			e.fput(st.Dst, r)
		} else {
			r := e.idst(st.Dst)
			e.iset(r, st.A)
			e.iput(st.Dst, r)
		}
	case fir.OpFAdd, fir.OpFSub, fir.OpFMul, fir.OpFDiv:
		rb := e.fget(st.B, e.s1)
		r := e.fdst(st.Dst)
		e.fset(r, st.A)
		switch st.Op {
		case fir.OpFAdd:
			a.Addsd(r, rb)
		case fir.OpFSub:
			a.Subsd(r, rb)
		case fir.OpFMul:
			a.Mulsd(r, rb)
		default:
			a.Divsd(r, rb)
		}
		e.fput(st.Dst, r)
	case fir.OpFMA, fir.OpMAdd:
		rc := e.fget(st.C, e.s2)
		rb := e.fget(st.B, e.s1)
		r := e.fdst(st.Dst)
		fused := st.Op == fir.OpFMA || e.f.Fused
		if !fused && rc == r {
			a.Movapd(e.s2, rc)
			rc = e.s2
		}
		e.fset(r, st.A)
		if fused {
			a.Vfmadd213sd(r, rb, rc)
		} else {
			a.Mulsd(r, rb)
			a.Addsd(r, rc)
		}
		e.fput(st.Dst, r)
	case fir.OpFMod:
		rb := e.fget(st.B, e.s1)
		ra := e.fget(st.A, e.s0)
		base := e.env.spillBase
		x, y := e.slotDisp(e.env.spillN-2), e.slotDisp(e.env.spillN-1)
		a.MovsdStore(base, y, rb)
		a.MovsdStore(base, x, ra)
		a.FldM64(base, y)
		a.FldM64(base, x)
		loop := e.label("prem")
		a.Label(loop)
		a.Fprem()
		a.FnstswAX()
		a.TestAHImm8(4) // C2: the remainder is partial
		a.Jcc(CondNE, loop)
		a.FstpM64(base, x)
		a.FstpST0()
		r := e.fdst(st.Dst)
		a.MovsdLoad(r, base, x)
		e.fput(st.Dst, r)
	case fir.OpFSqrt, fir.OpRoundEven, fir.OpFloor, fir.OpCeil, fir.OpTrunc:
		ra := e.fget(st.A, e.s0)
		r := e.fdst(st.Dst)
		switch st.Op {
		case fir.OpFSqrt:
			a.Sqrtsd(r, ra)
		case fir.OpRoundEven:
			a.Roundsd(r, ra, 0)
		case fir.OpFloor:
			a.Roundsd(r, ra, 1)
		case fir.OpCeil:
			a.Roundsd(r, ra, 2)
		default:
			a.Roundsd(r, ra, 3)
		}
		e.fput(st.Dst, r)
	case fir.OpFAbs, fir.OpFNeg:
		r := e.fdst(st.Dst)
		e.fset(r, st.A)
		if st.Op == fir.OpFAbs {
			a.AndpdRIP(r, e.env.pool.label(math.MaxInt64, 0))
		} else {
			a.XorpdRIP(r, e.env.pool.label(1<<63, 0))
		}
		e.fput(st.Dst, r)
	case fir.OpIToF:
		ri := e.iget(st.A, e.t0)
		r := e.fdst(st.Dst)
		a.Xorps(r, r)
		a.Cvtsi2sdq(r, ri)
		e.fput(st.Dst, r)
	case fir.OpFToI:
		rx := e.fget(st.A, e.s0)
		r := e.idst(st.Dst)
		a.Cvttsd2siq(r, rx)
		e.iput(st.Dst, r)
	case fir.OpAdd, fir.OpSub, fir.OpMul, fir.OpAnd, fir.OpOr, fir.OpXor:
		r := e.idst(st.Dst)
		imm, isImm := e.imm32(st.B)
		if st.Op == fir.OpMul {
			isImm = false
		}
		var rb Reg
		if !isImm {
			rb = e.iget(st.B, e.t1)
		}
		e.iset(r, st.A)
		digit := map[fir.Op]byte{fir.OpAdd: 0, fir.OpOr: 1, fir.OpAnd: 4, fir.OpSub: 5, fir.OpXor: 6}[st.Op]
		switch {
		case isImm:
			a.aluImm32(digit, r, imm)
		case st.Op == fir.OpAdd:
			a.AddRegReg(r, rb)
		case st.Op == fir.OpSub:
			a.SubRegReg(r, rb)
		case st.Op == fir.OpMul:
			a.ImulRegReg(r, rb)
		case st.Op == fir.OpAnd:
			a.AndRegReg(r, rb)
		case st.Op == fir.OpOr:
			a.OrRegReg(r, rb)
		default:
			a.XorRegReg(r, rb)
		}
		e.iput(st.Dst, r)
	case fir.OpShl, fir.OpShr, fir.OpSar:
		r := e.idst(st.Dst)
		e.iset(r, st.A)
		n := byte(st.Imm & 63)
		switch st.Op {
		case fir.OpShl:
			a.ShlRegImm8(r, n)
		case fir.OpShr:
			a.ShrRegImm8(r, n)
		default:
			a.SarRegImm8(r, n)
		}
		e.iput(st.Dst, r)
	case fir.OpLoadF, fir.OpLoadI:
		idx := e.iget(st.A, e.t0)
		a.MovRegImm64(e.t1, int64(uintptr(st.Tab)))
		if st.Op == fir.OpLoadF {
			r := e.fdst(st.Dst)
			a.MovsdLoadIdx(r, e.t1, idx)
			e.fput(st.Dst, r)
		} else {
			r := e.idst(st.Dst)
			a.MovRegMemIdx(r, e.t1, idx)
			e.iput(st.Dst, r)
		}
	case fir.OpIf:
		els := e.label("else")
		after := e.label("fi")
		target := after
		if st.Else != nil {
			target = els
		}
		e.branchUnless(st, target)
		if err := e.steps(st.Then, end); err != nil {
			return err
		}
		if st.Else != nil {
			if !firEnds(st.Then) {
				a.Jmp(after)
			}
			a.Label(els)
			if err := e.steps(st.Else, end); err != nil {
				return err
			}
		}
		a.Label(after)
	case fir.OpExit:
		a.Jmp(e.env.exit)
	case fir.OpReturn:
		out := e.env.out
		switch {
		case out.mem:
			a.MovsdStore(out.reg, out.disp, e.fget(st.A, e.s0))
		case out.xmm:
			if r := e.fget(st.A, e.s0); r != out.xr {
				a.Movapd(out.xr, r)
			}
		default:
			if r := e.iget(st.A, e.t0); r != out.reg {
				a.MovRegReg(out.reg, r)
			}
		}
		a.Jmp(end)
	default:
		return fmt.Errorf("jit: kernel %s: op %d", e.f.Name, st.Op)
	}
	return nil
}

// branchUnless jumps to target unless st's comparison holds.
func (e *firAmd64) branchUnless(st *fir.Step, target string) {
	a := e.a
	if st.Cmp.Float() {
		x, y := e.fget(st.A, e.s0), e.fget(st.B, e.s1)
		switch st.Cmp {
		case fir.FEq:
			a.Ucomisd(x, y)
			a.Jcc(ccP, target)
			a.Jcc(CondNE, target)
		case fir.FNe:
			yes := e.label("ne")
			a.Ucomisd(x, y)
			a.Jcc(ccP, yes)
			a.Jcc(CondE, target)
			a.Label(yes)
		case fir.FLt: // y > x
			a.Ucomisd(y, x)
			a.Jcc(ccBE, target)
		case fir.FLe:
			a.Ucomisd(y, x)
			a.Jcc(ccB, target)
		case fir.FGt:
			a.Ucomisd(x, y)
			a.Jcc(ccBE, target)
		default: // FGe
			a.Ucomisd(x, y)
			a.Jcc(ccB, target)
		}
		return
	}
	x := e.iget(st.A, e.t0)
	if imm, ok := e.imm32(st.B); ok {
		a.CmpRegImm32(x, imm)
	} else {
		a.CmpRegReg(x, e.iget(st.B, e.t1))
	}
	not := map[fir.Cmp]Cond{
		fir.IEq: CondNE, fir.INe: CondE, fir.ILt: CondGE, fir.ILe: CondG, fir.IGt: CondLE, fir.IGe: CondL,
		fir.ULt: ccAE, fir.ULe: ccA, fir.UGt: ccBE, fir.UGe: ccB,
	}[st.Cmp]
	a.Jcc(not, target)
}

// KernelOK reports whether f compiles into a program on this CPU: the
// instructions it needs exist, and its values fit the registers and spill
// slots a program gives a kernel.
func KernelOK(f *fir.Func) bool {
	a := NewAsm()
	env := &firEnv{
		args:      []firLoc{firMem(RSI, 0), firMem(RSI, 8)}[:min(f.NArgs, 2)],
		out:       firMem(RSI, 0),
		exit:      "x",
		gprs:      []Reg{RAX, RDX, R10, R11},
		xmms:      []XReg{X0, X1, X2, X3, X4, X5, X6, X7, X8, X9, X10, X11, X12},
		spillBase: RDI, spillDisp: POffScratch, spillN: ProgScratch,
		prefix: "t", pool: newFirPool(),
	}
	return f.NArgs <= 2 && emitFirAmd64(a, f, env, cpuFeat) == nil
}
