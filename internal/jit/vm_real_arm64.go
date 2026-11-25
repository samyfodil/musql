//go:build arm64

package jit

import "math"

// The VM JIT's REAL arms (vm.go's VReal to VMath) on AArch64, as on x86-64
// (vm_real_amd64.go): a REAL operand is loaded into a D register from its F
// word, an INTEGER one converted from its I word, and every check runs before
// anything is written. A kernel's constants go in the next island's pool, so
// its literal loads stay within reach however large the program.

// vmFKernelGPRs are the general registers a math kernel may use in VM code:
// the emitter's scratch, outside the pool that caches values across steps.
// VM code keeps nothing in the vector registers.
var vmFKernelGPRs = []Reg64{X9, X10, X11, X12, X13}

// fldr and fstr move r[r]'s F word to and from d; an offset an immediate
// cannot reach goes through X11.
func (e *vmEmitArm) fldr(d VReg, r int) {
	if o := e.off(r, e.lay.OffF); o%8 == 0 && o/8 < 4096 {
		e.a.LdrD(d, X1, o)
		return
	}
	e.a.MovImm64(X11, int64(e.off(r, e.lay.OffF)))
	e.a.AddReg(X11, X1, X11)
	e.a.LdrD(d, X11, 0)
}

func (e *vmEmitArm) fstr(d VReg, r int) {
	if o := e.off(r, e.lay.OffF); o%8 == 0 && o/8 < 4096 {
		e.a.StrD(d, X1, o)
		return
	}
	e.a.MovImm64(X11, int64(e.off(r, e.lay.OffF)))
	e.a.AddReg(X11, X1, X11)
	e.a.StrD(d, X11, 0)
}

// vmLabel names a label for one of the REAL arms, unique in the program.
func (e *vmEmitArm) vmLabel(s string) string {
	e.skips++
	return "vr" + s + itoa(e.skips)
}

// fload puts r[r] in d as a double: a REAL's F, an INTEGER converted (only
// when intOK), a TEXT spelling a REAL (only when text, VFText), anything
// else exiting at pc. When useFlag is set, flag becomes 1 when the operand
// was a REAL; the caller sets it only where no operand is known to be one.
func (e *vmEmitArm) fload(pc, r int, d VReg, intOK, text bool, flag Reg64, useFlag bool) {
	a, L := e.a, e.lay
	if e.know.isFloat.has(r) {
		e.fldr(d, r)
		return
	}
	if e.know.isInt.has(r) && intOK {
		a.Scvtf(d, e.src(pc, r, X10))
		return
	}
	notReal, done := e.vmLabel("nr"), e.vmLabel("fl")
	e.ldrb(X12, r, L.OffTyp)
	a.CmpImm(X12, uint32(L.TypFloat))
	a.Bcond(CondNE, notReal)
	e.fldr(d, r)
	if useFlag {
		a.MovImm16(flag, 1)
	}
	if !intOK {
		e.know.isFloat.add(r)
	}
	e.cold = append(e.cold, func() {
		a.Label(notReal)
		if !intOK {
			e.exitAt(pc)
			return
		}
		a.CmpImm(X12, uint32(L.TypInt))
		notInt := e.vmLabel("ft")
		if text {
			a.Bcond(CondNE, notInt)
		} else {
			a.Bcond(CondNE, e.stub(pc))
		}
		e.ldr(X10, r, L.OffI)
		a.Scvtf(d, X10)
		a.B(done)
		if text {
			a.Label(notInt)
			e.ftext(pc, r, d, flag, useFlag)
			a.B(done)
		}
	})
	a.Label(done)
}

// ftext is vm_real_amd64.go's: the REAL a TEXT r[r] spells as
// [-]digits.digits, its type byte in X12. X9 walks the text to its end in
// X10, X11 is the digits, D2 and D3 scratch.
func (e *vmEmitArm) ftext(pc, r int, d VReg, flag Reg64, useFlag bool) {
	a, L := e.a, e.lay
	unsigned, loop, dot, fracLoop, conv := e.vmLabel("tu"), e.vmLabel("tl"), e.vmLabel("td"), e.vmLabel("tf"), e.vmLabel("tc")
	pLoop, pDone, pos := e.vmLabel("tp"), e.vmLabel("tq"), e.vmLabel("ts")
	a.CmpImm(X12, uint32(L.TypText))
	a.Bcond(CondNE, e.stub(pc))
	e.ldr(X9, r, L.OffS)
	e.ldr(X10, r, L.OffS+8)
	a.AddReg(X10, X10, X9) // the end
	a.Cmp(X9, X10)
	a.Bcond(CondE, e.stub(pc))
	a.LdrbImm(X12, X9, 0)
	a.CmpImm(X12, '-')
	a.Bcond(CondNE, unsigned)
	a.AddImm(X9, X9, 1)
	// A digit and the point at least.
	a.Label(unsigned)
	a.SubReg(X12, X10, X9)
	a.CmpImm(X12, 2)
	a.Bcond(CondL, e.stub(pc))
	a.MovImm16(X11, 0)
	a.Label(loop)
	a.Cmp(X9, X10)
	a.Bcond(CondE, e.stub(pc)) // no point: an INTEGER's spelling
	a.LdrbImm(X12, X9, 0)
	a.AddImm(X9, X9, 1)
	a.CmpImm(X12, '.')
	a.Bcond(CondE, dot)
	e.fdigit(pc)
	a.B(loop)

	a.Label(dot)
	a.SubReg(X12, X10, X9)
	a.CmpImm(X12, 22)
	a.Bcond(CondG, e.stub(pc))
	a.FmovToD(3, X12) // k
	a.Label(fracLoop)
	a.Cmp(X9, X10)
	a.Bcond(CondE, conv)
	a.LdrbImm(X12, X9, 0)
	a.AddImm(X9, X9, 1)
	e.fdigit(pc)
	a.B(fracLoop)

	a.Label(conv)
	a.FmovToX(X12, 3)
	a.Scvtf(d, X11)
	// 10^k by tens, each power up to 10^22 exact.
	a.MovImm64(X9, int64(math.Float64bits(1)))
	a.FmovToD(2, X9)
	a.MovImm64(X9, int64(math.Float64bits(10)))
	a.FmovToD(3, X9)
	a.Label(pLoop)
	a.Cbz(X12, pDone)
	a.Fmul(2, 2, 3)
	a.SubsImm(X12, X12, 1)
	a.B(pLoop)
	a.Label(pDone)
	a.Fdiv(d, d, 2)
	e.ldr(X9, r, L.OffS)
	a.LdrbImm(X12, X9, 0)
	a.CmpImm(X12, '-')
	a.Bcond(CondNE, pos)
	a.Fneg(d, d)
	a.Label(pos)
	if useFlag {
		a.MovImm16(flag, 1)
	}
}

// fdigit takes the byte in X12 into X11 as the next decimal digit, exiting at
// pc on any other byte and once X11 reaches 2^53.
func (e *vmEmitArm) fdigit(pc int) {
	a := e.a
	a.SubsImm(X12, X12, '0')
	a.CmpImm(X12, 9)
	a.BcondRaw(armHI, e.stub(pc))
	a.AddRegLSL3(X12, X12, X11) // the digit + 8s
	a.AddReg(X12, X12, X11)
	a.AddReg(X12, X12, X11)
	a.MovReg(X11, X12)
	a.LsrImm(X12, X11, 53)
	a.Cbnz(X12, e.stub(pc))
}

// storeFloat makes r[r] the REAL in d: Value{Typ: Float, F: d}.
func (e *vmEmitArm) storeFloat(pc int, in VInsn, r int, d VReg) {
	if in.Dead {
		e.clobber(r)
		return
	}
	e.destNoS(pc, r)
	e.a.MovImm16(X12, uint16(e.lay.TypFloat))
	e.str(X12, r, 0)
	e.str(xzr, r, e.lay.OffI)
	e.fstr(d, r)
	e.clearS(r)
	e.unmap(r)
	e.know.wroteFloat(r)
}

// realInsn emits the REAL arms; false for any other op.
func (e *vmEmitArm) realInsn(pc int, in VInsn) (bool, error) {
	a := e.a
	switch in.Op {
	case VReal:
		if in.Dead {
			e.clobber(in.A)
			break
		}
		a.MovImm64(X9, in.Imm)
		a.FmovToD(0, X9)
		e.storeFloat(pc, VInsn{}, in.A, 0)
	case VFAdd, VFSub, VFMul, VFDiv:
		// Two INTEGERs are vm_real_amd64.go's: the INTEGER arm, here for an
		// add, subtract or multiply, a divide exiting.
		useFlag := !e.know.isFloat.has(in.A) && !e.know.isFloat.has(in.B)
		if useFlag {
			a.MovImm16(X13, 0)
		}
		e.fload(pc, in.B, 0, true, in.Imm&VFText != 0, X13, useFlag)
		e.fload(pc, in.A, 1, true, in.Imm&VFText != 0, X13, useFlag)
		ints := useFlag && in.Op != VFDiv && !in.Dead
		intArm, join := "", ""
		if useFlag {
			if ints {
				intArm, join = e.vmLabel("fi"), e.vmLabel("fj")
				e.destNoS(pc, in.C) // for both arms
				a.Cbz(X13, intArm)
			} else {
				a.Cbz(X13, e.stub(pc))
			}
		}
		switch in.Op {
		case VFAdd:
			a.Fadd(0, 0, 1)
		case VFSub:
			a.Fsub(0, 0, 1)
		case VFMul:
			a.Fmul(0, 0, 1)
		default:
			a.FcmpZero(1)
			a.Bcond(CondE, e.stub(pc)) // a zero divisor: NULL
			a.Fdiv(0, 0, 1)
		}
		a.Fcmp(0, 0)
		a.BcondRaw(armVS, e.stub(pc)) // NaN: NULL
		e.storeFloat(pc, in, in.C, 0)
		if ints {
			L := e.lay
			a.B(join)
			a.Label(intArm)
			e.ldr(X9, in.B, L.OffI)
			e.ldr(X10, in.A, L.OffI)
			switch in.Op {
			case VFAdd:
				a.AddsReg(X9, X9, X10)
				a.BcondRaw(armVS, e.stub(pc))
			case VFSub:
				a.SubsReg(X9, X9, X10)
				a.BcondRaw(armVS, e.stub(pc))
			default:
				a.Smulh(X12, X9, X10)
				a.Mul(X9, X9, X10)
				a.CmpAsr63(X12, X9)
				a.Bcond(CondNE, e.stub(pc))
			}
			a.MovImm16(X12, uint16(L.TypInt))
			e.str(X12, in.C, 0)
			e.str(X9, in.C, L.OffI)
			e.str(xzr, in.C, L.OffF)
			for k := range 3 {
				e.str(xzr, in.C, L.OffS+8*k)
			}
			a.Label(join)
			e.unmap(in.C)
			e.know.wroteAny(in.C, true)
		}
	case VFCmpJump, VFCmpSet, VFCmpBr:
		e.fcmpLoad(pc, in.B, 0)
		e.fcmpLoad(pc, in.A, 1)
		if in.Op != VFCmpJump && !in.Dead {
			e.destNoS(pc, in.C)
		}
		// Without a NaN, which exits, FCMP's flags answer the signed
		// conditions as a CMP's would.
		a.Fcmp(0, 1)
		a.BcondRaw(armVS, e.stub(pc))
		if in.Op == VFCmpJump {
			e.jccBlock(in.Cond, pc, in.T)
			break
		}
		if !in.Dead {
			m := e.dest(pc, in.C, X9)
			a.Cset(m, in.Cond)
			e.storeInt(in, in.C, m) // stores leave the flags alone
		} else {
			e.clobber(in.C)
		}
		if in.Op == VFCmpBr {
			c := in.Cond
			if in.Imm == 0 {
				c = c.Negate()
			}
			e.jccBlock(c, pc, in.T)
		}
	case VMath:
		return true, e.math(pc, in)
	default:
		return false, nil
	}
	return true, nil
}

// fcmpLoad loads a comparison's operand: a REAL, or an INTEGER within 2^53
// of zero, which a double holds exactly and so compares as
// sqlite3IntFloatCompare would.
func (e *vmEmitArm) fcmpLoad(pc, r int, d VReg) {
	a, L := e.a, e.lay
	if e.know.isFloat.has(r) {
		e.fldr(d, r)
		return
	}
	isInt, done := e.vmLabel("ci"), e.vmLabel("cd")
	e.ldrb(X12, r, L.OffTyp)
	a.CmpImm(X12, uint32(L.TypFloat))
	a.Bcond(CondNE, isInt)
	e.fldr(d, r)
	e.cold = append(e.cold, func() {
		a.Label(isInt)
		a.CmpImm(X12, uint32(L.TypInt))
		a.Bcond(CondNE, e.stub(pc))
		e.ldr(X10, r, L.OffI)
		a.MovImm64(X12, 1<<53)
		a.AddReg(X9, X10, X12)
		a.MovImm64(X12, 1<<54)
		a.Cmp(X9, X12)
		a.BcondRaw(armHI, e.stub(pc)) // unsigned: outside [-2^53, 2^53]
		a.Scvtf(d, X10)
		a.B(done)
	})
	a.Label(done)
}

// math emits VMath: the arguments as doubles into ProgArgs.Scratch, the
// kernel inline over them (its answer into Scratch[0], its spills above
// both), then the answer into r[C].
func (e *vmEmitArm) math(pc int, in VInsn) error {
	if in.Kernel == nil || in.B < 1 || in.B > 2 || in.Kernel.NArgs != in.B {
		e.exitAt(pc)
		return nil
	}
	a := e.a
	for i := 0; i < in.B; i++ {
		d := VReg(i)
		e.fload(pc, in.A+i, d, in.Imm == 0, false, 0, false)
		a.StrD(d, X0, POffScratch+8*i)
	}
	exit := e.vmLabel("mx")
	env := &firEnv{
		args:      []firLoc{firMem(X0, POffScratch), firMem(X0, POffScratch+8)}[:in.B],
		out:       firMem(X0, POffScratch),
		exit:      exit,
		gprs:      vmFKernelGPRs,
		fprs:      progKernelFPRs,
		spillBase: X0, spillOff: POffScratch + 16, spillN: ProgScratch - 2,
		prefix: e.vmLabel("k"), pool: e.consts,
	}
	if err := emitFirArm64(a, in.Kernel, env, cpuFeat); err != nil {
		return err
	}
	done := e.vmLabel("md")
	a.B(done)
	a.Label(exit)
	e.exitAt(pc)
	a.Label(done)
	a.LdrD(0, X0, POffScratch)
	a.Fcmp(0, 0)
	a.BcondRaw(armVS, e.stub(pc)) // NaN: NULL
	e.storeFloat(pc, in, in.C, 0)
	return nil
}
