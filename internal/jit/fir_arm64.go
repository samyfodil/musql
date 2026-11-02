//go:build arm64

package jit

import (
	"fmt"

	"github.com/samyfodil/musql/internal/fir"
)

// Lowering a fir kernel to AArch64, inline in the code around it: the
// allocation of fir_alloc.go, then each step as one or two instructions, the
// ISA being three-operand. Constants come from a literal pool after the
// code, or as an immediate; the first two integer and three floating-point
// registers offered are kept back as scratch, as on amd64.

// firLoc is where a value lives outside the kernel.
type firLoc struct {
	mem bool
	fp  bool
	reg Reg64 // a general register, or the base of mem
	vr  VReg
	off int
}

func firMem(base Reg64, off int) firLoc { return firLoc{mem: true, reg: base, off: off} }

// firEnv is what the code around a kernel hands it.
type firEnv struct {
	args      []firLoc
	out       firLoc
	exit      string
	gprs      []Reg64
	fprs      []VReg
	spillBase Reg64
	spillOff  int
	spillN    int
	prefix    string
	pool      *firPool
}

// firPool is the doubles a program's kernels load, laid out after its code.
// prefix keeps the labels of a program's several pools apart.
type firPool struct {
	labels map[uint64]string
	order  []uint64
	prefix string
}

func newFirPool() *firPool { return &firPool{labels: map[uint64]string{}} }

func (p *firPool) label(u uint64) string {
	if l, ok := p.labels[u]; ok {
		return l
	}
	l := p.prefix + "fp" + itoa(len(p.order))
	p.labels[u] = l
	p.order = append(p.order, u)
	return l
}

func (p *firPool) emit(a *Arm) {
	if p == nil || len(p.order) == 0 {
		return
	}
	if len(a.buf)%2 != 0 {
		a.emit(0xD503201F) // NOP: the pool's words are 8-aligned
	}
	for _, u := range p.order {
		a.Label(p.labels[u])
		a.emit(uint32(u))
		a.emit(uint32(u >> 32))
	}
}

type firArm64 struct {
	a      *Arm
	f      *fir.Func
	env    *firEnv
	home   []firHome
	gpr    []Reg64
	fpr    []VReg
	t0, t1 Reg64
	s0, s1 VReg
	s2     VReg
	n      int
}

// emitFirArm64 emits f inline: arguments read from env.args, the answer
// written to env.out, an Exit branching to env.exit.
func emitFirArm64(a *Arm, f *fir.Func, env *firEnv, feat cpuFeatures) error {
	if len(env.gprs) < 2 || len(env.fprs) < 4 || len(env.args) != f.NArgs {
		return fmt.Errorf("jit: kernel %s: too few registers", f.Name)
	}
	if f.Fused && !feat.fma {
		return fmt.Errorf("jit: kernel %s is built fused and this CPU has no FMA", f.Name)
	}
	rounds := firHasOp(f.Body, fir.OpRoundEven) || firHasOp(f.Body, fir.OpFloor) || firHasOp(f.Body, fir.OpCeil) ||
		firHasOp(f.Body, fir.OpTrunc)
	switch {
	case firHasOp(f.Body, fir.OpFMod):
		return fmt.Errorf("jit: kernel %s: no fmod on arm64", f.Name)
	case !feat.fma && firHasOp(f.Body, fir.OpFMA):
		return fmt.Errorf("jit: kernel %s needs FMA", f.Name)
	case !feat.round && rounds:
		return fmt.Errorf("jit: kernel %s needs FRINT", f.Name)
	}
	e := &firArm64{a: a, f: f, env: env,
		t0: env.gprs[0], t1: env.gprs[1], s0: env.fprs[0], s1: env.fprs[1], s2: env.fprs[2],
		gpr: env.gprs[2:], fpr: env.fprs[3:]}
	home, err := firAllocate(f, len(e.gpr), len(e.fpr), env.spillN)
	if err != nil {
		return err
	}
	e.home = home
	end := e.label("end")
	e.steps(f.Body, end)
	a.Label(end)
	return nil
}

func (e *firArm64) label(s string) string {
	e.n++
	return e.env.prefix + s + itoa(e.n)
}

func (e *firArm64) slotOff(i int) int { return e.env.spillOff + i*8 }

func (e *firArm64) fget(v fir.Val, s VReg) VReg {
	h := e.home[v]
	switch {
	case h.isK:
		e.a.LdrDLit(s, e.env.pool.label(h.bits))
		return s
	case h.slot >= 0:
		e.a.LdrD(s, e.env.spillBase, e.slotOff(h.slot))
		return s
	case h.class == fir.F:
		return e.fpr[h.reg]
	}
	e.a.FmovToD(s, e.gpr[h.reg])
	return s
}

func (e *firArm64) iget(v fir.Val, t Reg64) Reg64 {
	h := e.home[v]
	switch {
	case h.isK:
		e.a.MovImm64(t, int64(h.bits))
		return t
	case h.slot >= 0:
		e.a.LdrImm(t, e.env.spillBase, e.slotOff(h.slot))
		return t
	case h.class == fir.I:
		return e.gpr[h.reg]
	}
	e.a.FmovToX(t, e.fpr[h.reg])
	return t
}

func (e *firArm64) fdst(v fir.Val) VReg {
	if h := e.home[v]; h.slot < 0 && h.reg >= 0 && h.class == fir.F {
		return e.fpr[h.reg]
	}
	return e.s0
}

func (e *firArm64) idst(v fir.Val) Reg64 {
	if h := e.home[v]; h.slot < 0 && h.reg >= 0 && h.class == fir.I {
		return e.gpr[h.reg]
	}
	return e.t0
}

func (e *firArm64) fput(v fir.Val, r VReg) {
	h := e.home[v]
	switch {
	case h.slot >= 0:
		e.a.StrD(r, e.env.spillBase, e.slotOff(h.slot))
	case e.fpr[h.reg] != r:
		e.a.FmovD(e.fpr[h.reg], r)
	}
}

func (e *firArm64) iput(v fir.Val, r Reg64) {
	h := e.home[v]
	switch {
	case h.slot >= 0:
		e.a.StrImm(r, e.env.spillBase, e.slotOff(h.slot))
	case h.class == fir.F:
		e.a.FmovToD(e.fpr[h.reg], r)
	case e.gpr[h.reg] != r:
		e.a.MovReg(e.gpr[h.reg], r)
	}
}

func (e *firArm64) steps(steps []fir.Step, end string) {
	for i := range steps {
		e.step(&steps[i], end)
	}
}

func (e *firArm64) step(st *fir.Step, end string) {
	a := e.a
	switch st.Op {
	case fir.OpConst:
	case fir.OpArg:
		src := e.env.args[st.Imm]
		switch {
		case src.mem:
			r := e.fdst(st.Dst)
			a.LdrD(r, src.reg, src.off)
			e.fput(st.Dst, r)
		case src.fp:
			e.fput(st.Dst, src.vr)
		default:
			e.iput(st.Dst, src.reg)
		}
	case fir.OpCopy:
		if e.f.Class[st.Dst] == fir.F {
			e.fput(st.Dst, e.fget(st.A, e.s0))
		} else {
			e.iput(st.Dst, e.iget(st.A, e.t0))
		}
	case fir.OpFAdd, fir.OpFSub, fir.OpFMul, fir.OpFDiv:
		rb, ra := e.fget(st.B, e.s1), e.fget(st.A, e.s0)
		r := e.fdst(st.Dst)
		switch st.Op {
		case fir.OpFAdd:
			a.Fadd(r, ra, rb)
		case fir.OpFSub:
			a.Fsub(r, ra, rb)
		case fir.OpFMul:
			a.Fmul(r, ra, rb)
		default:
			a.Fdiv(r, ra, rb)
		}
		e.fput(st.Dst, r)
	case fir.OpFMA, fir.OpMAdd:
		rc, rb, ra := e.fget(st.C, e.s2), e.fget(st.B, e.s1), e.fget(st.A, e.s0)
		r := e.fdst(st.Dst)
		if st.Op == fir.OpFMA || e.f.Fused {
			a.Fmadd(r, ra, rb, rc)
		} else {
			tmp := r
			if r == rc {
				tmp = e.s0
			}
			a.Fmul(tmp, ra, rb)
			a.Fadd(r, tmp, rc)
		}
		e.fput(st.Dst, r)
	case fir.OpFSqrt, fir.OpFAbs, fir.OpFNeg, fir.OpRoundEven, fir.OpFloor, fir.OpCeil, fir.OpTrunc:
		ra := e.fget(st.A, e.s0)
		r := e.fdst(st.Dst)
		switch st.Op {
		case fir.OpFSqrt:
			a.Fsqrt(r, ra)
		case fir.OpFAbs:
			a.Fabs(r, ra)
		case fir.OpFNeg:
			a.Fneg(r, ra)
		case fir.OpRoundEven:
			a.Frint(r, ra, 0)
		case fir.OpFloor:
			a.Frint(r, ra, 1)
		case fir.OpCeil:
			a.Frint(r, ra, 2)
		default:
			a.Frint(r, ra, 3)
		}
		e.fput(st.Dst, r)
	case fir.OpIToF:
		ri := e.iget(st.A, e.t0)
		r := e.fdst(st.Dst)
		a.Scvtf(r, ri)
		e.fput(st.Dst, r)
	case fir.OpFToI:
		rx := e.fget(st.A, e.s0)
		r := e.idst(st.Dst)
		a.Fcvtzs(r, rx)
		e.iput(st.Dst, r)
	case fir.OpAdd, fir.OpSub, fir.OpMul, fir.OpAnd, fir.OpOr, fir.OpXor:
		rb, ra := e.iget(st.B, e.t1), e.iget(st.A, e.t0)
		r := e.idst(st.Dst)
		switch st.Op {
		case fir.OpAdd:
			a.AddReg(r, ra, rb)
		case fir.OpSub:
			a.SubReg(r, ra, rb)
		case fir.OpMul:
			a.Mul(r, ra, rb)
		case fir.OpAnd:
			a.AndReg(r, ra, rb)
		case fir.OpOr:
			a.OrrReg(r, ra, rb)
		default:
			a.EorReg(r, ra, rb)
		}
		e.iput(st.Dst, r)
	case fir.OpShl, fir.OpShr, fir.OpSar:
		ra := e.iget(st.A, e.t0)
		r := e.idst(st.Dst)
		n := uint32(st.Imm & 63)
		switch {
		case n == 0:
			if r != ra {
				a.MovReg(r, ra)
			}
		case st.Op == fir.OpShl:
			a.LslImm(r, ra, n)
		case st.Op == fir.OpShr:
			a.LsrImm(r, ra, n)
		default:
			a.AsrImm(r, ra, n)
		}
		e.iput(st.Dst, r)
	case fir.OpLoadF, fir.OpLoadI:
		idx := e.iget(st.A, e.t0)
		a.MovImm64(e.t1, int64(uintptr(st.Tab)))
		if st.Op == fir.OpLoadF {
			r := e.fdst(st.Dst)
			a.LdrDIdx(r, e.t1, idx)
			e.fput(st.Dst, r)
		} else {
			r := e.idst(st.Dst)
			a.LdrRegIdx(r, e.t1, idx)
			e.iput(st.Dst, r)
		}
	case fir.OpIf:
		els, after := e.label("else"), e.label("fi")
		target := after
		if st.Else != nil {
			target = els
		}
		e.branchUnless(st, target)
		e.steps(st.Then, end)
		if st.Else != nil {
			if !firEnds(st.Then) {
				a.B(after)
			}
			a.Label(els)
			e.steps(st.Else, end)
		}
		a.Label(after)
	case fir.OpExit:
		a.B(e.env.exit)
	case fir.OpReturn:
		out := e.env.out
		switch {
		case out.mem:
			a.StrD(e.fget(st.A, e.s0), out.reg, out.off)
		case out.fp:
			if r := e.fget(st.A, e.s0); r != out.vr {
				a.FmovD(out.vr, r)
			}
		default:
			if r := e.iget(st.A, e.t0); r != out.reg {
				a.MovReg(out.reg, r)
			}
		}
		a.B(end)
	}
}

// branchUnless branches to target unless st's comparison holds. After FCMP an
// unordered pair is NZCV 0011, which each "not" condition below includes.
func (e *firArm64) branchUnless(st *fir.Step, target string) {
	a := e.a
	if st.Cmp.Float() {
		x, y := e.fget(st.A, e.s0), e.fget(st.B, e.s1)
		a.Fcmp(x, y)
		not := map[fir.Cmp]uint32{
			fir.FEq: 0x1,   // NE
			fir.FNe: 0x0,   // EQ
			fir.FLt: armPL, // not MI
			fir.FLe: armHI, // not LS
			fir.FGt: 0xD,   // LE: not GT
			fir.FGe: 0xB,   // LT: not GE
		}[st.Cmp]
		a.BcondRaw(not, target)
		return
	}
	x, y := e.iget(st.A, e.t0), e.iget(st.B, e.t1)
	a.Cmp(x, y)
	not := map[fir.Cmp]uint32{
		fir.IEq: 0x1, fir.INe: 0x0, fir.ILt: 0xA, fir.ILe: 0xC, fir.IGt: 0xD, fir.IGe: 0xB,
		fir.ULt: armHS, fir.ULe: armHI, fir.UGt: armLS, fir.UGe: armLO,
	}[st.Cmp]
	a.BcondRaw(not, target)
}

// KernelOK reports whether f compiles into a program here.
func KernelOK(f *fir.Func) bool {
	a := NewArm()
	env := &firEnv{
		args: []firLoc{firMem(X1, 0), firMem(X1, 8)}[:min(f.NArgs, 2)], out: firMem(X1, 0), exit: "x",
		gprs: []Reg64{X9, X10, X11, X12, X13, X14}, fprs: progKernelFPRs,
		spillBase: X0, spillOff: POffScratch, spillN: ProgScratch, prefix: "t", pool: newFirPool(),
	}
	return f.NArgs <= 2 && emitFirArm64(a, f, env, cpuFeat) == nil
}

// progKernelFPRs are the registers a program's kernels may use: all but
// V30 and V31, which hold its REAL accumulator.
var progKernelFPRs = func() []VReg {
	r := make([]VReg, 30)
	for i := range r {
		r[i] = VReg(i)
	}
	return r
}()
