//go:build amd64

package jit

import "math"

// The VM JIT's REAL arms (vm.go's VReal to VMath) on x86-64. A REAL operand
// is loaded into an XMM register from its F word, an INTEGER one converted
// from its I word; every check runs before anything is written, so an exit
// leaves the register file as the VDBE expects it.

// vmFKernelGPRs are the general registers a math kernel may use in VM code:
// scratch there, outside the pool that caches values across steps.
var vmFKernelGPRs = []Reg{RAX, RDX, R9, R10}

// vmFKernelXMMs are the XMM registers it may use; VM code keeps nothing in
// them.
var vmFKernelXMMs = []XReg{X0, X1, X2, X3, X4, X5, X6, X7, X8, X9, X10, X11, X12, X13, X14}

// fload puts r[r] in x as a double: a REAL's F, an INTEGER converted (only
// when intOK), a TEXT spelling a REAL (only when text, vmFText), anything
// else exiting at pc. isReal, when non-nil, gets 1 ORed in when the operand
// was a REAL; the caller passes it only where no operand is known to be one.
func (e *vmEmit) fload(pc, r int, x XReg, intOK, text bool, isReal Reg) {
	a, L := e.a, e.lay
	if e.know.isFloat.has(r) {
		a.MovsdLoad(x, RSI, e.d(r, L.OffF))
		return
	}
	if e.know.isInt.has(r) && intOK {
		a.Xorps(x, x)
		a.Cvtsi2sdq(x, e.src(pc, r, RAX))
		return
	}
	notReal, done := e.vmLabel(), e.vmLabel()
	a.CmpMemDByte(RSI, e.d(r, L.OffTyp), L.TypFloat)
	a.JccL(CondNE, notReal)
	a.MovsdLoad(x, RSI, e.d(r, L.OffF))
	if isReal != 0 {
		a.aluImm32(1, isReal, 1)
	}
	if !intOK {
		e.know.isFloat.add(r)
	}
	e.cold = append(e.cold, vmCold{body: func() {
		a.Bind(notReal)
		if !intOK {
			e.exitAt(pc)
			return
		}
		a.CmpMemDByte(RSI, e.d(r, L.OffTyp), L.TypInt)
		notInt := e.vmLabel()
		if text {
			a.JccL(CondNE, notInt)
		} else {
			e.jccStub(CondNE, pc)
		}
		a.MovRegMemD(RAX, RSI, e.d(r, L.OffI))
		a.Xorps(x, x)
		a.Cvtsi2sdq(x, RAX)
		a.JmpL(done)
		if text {
			a.Bind(notInt)
			e.ftext(pc, r, x, isReal)
			a.JmpL(done)
		}
	}})
	a.Bind(done)
}

// ftext puts in x the REAL a TEXT r[r] spells as [-]digits.digits, which
// arithmetic reads as sqlite3AtoF does: its digits s, below 2^53, over 10^k
// for its k <= 22 digits after the point, one correctly rounded divide
// (sqliteFp10Convert2's fast path), negated after. Any other value exits at
// pc, as does UTF-16 text, whose zero bytes are no digits. RAX, RDX, R9,
// R10, X2 and X3 are scratch.
func (e *vmEmit) ftext(pc, r int, x XReg, isReal Reg) {
	a, L := e.a, e.lay
	unsigned, loop, dot, fracLoop, conv := e.vmLabel(), e.vmLabel(), e.vmLabel(), e.vmLabel(), e.vmLabel()
	pLoop, pDone, pos := e.vmLabel(), e.vmLabel(), e.vmLabel()
	a.CmpMemDByte(RSI, e.d(r, L.OffTyp), L.TypText)
	e.jccStub(CondNE, pc)
	a.MovRegMemD(R9, RSI, e.d(r, L.OffS))
	a.MovRegMemD(R10, RSI, e.d(r, L.OffS+8))
	a.AddRegReg(R10, R9) // the end
	a.CmpRegReg(R9, R10)
	e.jccStub(CondE, pc)
	a.MovzxByte(RDX, R9)
	a.CmpRegImm32(RDX, '-')
	a.JccL(CondNE, unsigned)
	a.IncReg(R9)
	// A digit and the point at least.
	a.Bind(unsigned)
	a.MovRegReg(RDX, R10)
	a.SubRegReg(RDX, R9)
	a.CmpRegImm32(RDX, 2)
	e.jccStub(CondL, pc)
	a.XorRegReg(RAX, RAX)
	a.Bind(loop)
	a.CmpRegReg(R9, R10)
	e.jccStub(CondE, pc) // no point: an INTEGER's spelling
	a.MovzxByte(RDX, R9)
	a.IncReg(R9)
	a.CmpRegImm32(RDX, '.')
	a.JccL(CondE, dot)
	e.fdigit(pc)
	a.JmpL(loop)

	a.Bind(dot)
	a.MovRegReg(RDX, R10)
	a.SubRegReg(RDX, R9)
	a.CmpRegImm32(RDX, 22)
	e.jccStub(CondG, pc)
	a.MovqXR(X3, RDX) // k
	a.Bind(fracLoop)
	a.CmpRegReg(R9, R10)
	a.JccL(CondE, conv)
	a.MovzxByte(RDX, R9)
	a.IncReg(R9)
	e.fdigit(pc)
	a.JmpL(fracLoop)

	a.Bind(conv)
	a.MovqRX(RDX, X3)
	a.Xorps(x, x)
	a.Cvtsi2sdq(x, RAX)
	// 10^k by tens, each power up to 10^22 exact.
	a.MovRegImm64(R9, int64(math.Float64bits(1)))
	a.MovqXR(X2, R9)
	a.MovRegImm64(R9, int64(math.Float64bits(10)))
	a.MovqXR(X3, R9)
	a.Bind(pLoop)
	a.TestRegReg(RDX, RDX)
	a.JccL(CondE, pDone)
	a.Mulsd(X2, X3)
	a.DecReg(RDX)
	a.JmpL(pLoop)
	a.Bind(pDone)
	a.Divsd(x, X2)
	a.MovRegMemD(R9, RSI, e.d(r, L.OffS))
	a.MovzxByte(RDX, R9)
	a.CmpRegImm32(RDX, '-')
	a.JccL(CondNE, pos)
	a.MovqRX(RDX, x)
	a.MovRegImm64(R9, math.MinInt64)
	a.XorRegReg(RDX, R9)
	a.MovqXR(x, RDX)
	a.Bind(pos)
	if isReal != 0 {
		a.aluImm32(1, isReal, 1)
	}
}

// fdigit takes the byte in RDX into RAX as the next decimal digit, exiting
// at pc on any other byte and once RAX reaches 2^53.
func (e *vmEmit) fdigit(pc int) {
	a := e.a
	a.SubRegImm32(RDX, '0')
	a.CmpRegImm32(RDX, 9)
	e.jccStub(ccA, pc)
	a.ImulRegRegImm32(RAX, RAX, 10)
	a.AddRegReg(RAX, RDX)
	a.MovRegReg(RDX, RAX)
	a.ShrRegImm8(RDX, 53)
	a.TestRegReg(RDX, RDX)
	e.jccStub(CondNE, pc)
}

// vmLabel makes a label for one of the arms an instruction branches between.
func (e *vmEmit) vmLabel() Lbl { return e.a.NewLabel() }

// vmLabelName names a label unique in the program, for code that jumps by
// name (firEnv, the constant pool).
func (e *vmEmit) vmLabelName(s string) string {
	e.nLabel++
	return "vr" + s + itoa(e.nLabel)
}

// storeFloat makes r[r] the REAL in x: Value{Typ: Float, F: x}.
func (e *vmEmit) storeFloat(pc int, in VInsn, r int, x XReg) {
	if in.Dead {
		e.clobber(r)
		return
	}
	e.destNoS(pc, r)
	a, L := e.a, e.lay
	a.MovMemDImm(RSI, e.d(r, 0), int32(L.TypFloat))
	a.MovMemDImm(RSI, e.d(r, L.OffI), 0)
	a.MovsdStore(RSI, e.d(r, L.OffF), x)
	e.clearS(r)
	e.unmap(r)
	e.know.wroteFloat(r)
}

// realInsn emits the REAL arms; false for any other op.
func (e *vmEmit) realInsn(pc int, in VInsn) (bool, error) {
	a := e.a
	switch in.Op {
	case VReal:
		if in.Dead {
			e.clobber(in.A)
			break
		}
		a.MovRegImm64(RAX, in.Imm)
		a.MovqXR(X0, RAX)
		e.storeFloat(pc, VInsn{}, in.A, X0)
	case VFAdd, VFSub, VFMul, VFDiv:
		// Two INTEGERs are the INTEGER arm's, which overflows into a REAL only
		// where it must: an add, subtract or multiply of two runs that arm
		// here, exiting on overflow; a divide exits.
		flag := Reg(0)
		if !e.know.isFloat.has(in.A) && !e.know.isFloat.has(in.B) {
			flag = RCX
			a.XorRegReg(RCX, RCX)
		}
		e.fload(pc, in.B, X0, true, in.Imm&VFText != 0, flag)
		e.fload(pc, in.A, X1, true, in.Imm&VFText != 0, flag)
		ints := flag != 0 && in.Op != VFDiv && !in.Dead
		var intArm, join Lbl
		if ints {
			e.destNoS(pc, in.C) // for both arms, ahead of the test: it sets the flags
		}
		if flag != 0 {
			a.TestRegReg(RCX, RCX)
			if ints {
				intArm, join = e.vmLabel(), e.vmLabel()
				a.JccL(CondE, intArm)
			} else {
				e.jccStub(CondE, pc)
			}
		}
		switch in.Op {
		case VFAdd:
			a.Addsd(X0, X1)
		case VFSub:
			a.Subsd(X0, X1)
		case VFMul:
			a.Mulsd(X0, X1)
		default:
			a.Xorps(X2, X2)
			a.Ucomisd(X1, X2)
			e.jccStub(CondE, pc) // a zero divisor: NULL
			a.Divsd(X0, X1)
		}
		a.Ucomisd(X0, X0)
		e.jccStub(ccP, pc) // NaN: NULL
		e.storeFloat(pc, in, in.C, X0)
		if ints {
			L := e.lay
			a.JmpL(join)
			a.Bind(intArm)
			a.MovRegMemD(RAX, RSI, e.d(in.B, L.OffI))
			a.MovRegMemD(RDX, RSI, e.d(in.A, L.OffI))
			switch in.Op {
			case VFAdd:
				a.AddRegReg(RAX, RDX)
			case VFSub:
				a.SubRegReg(RAX, RDX)
			default:
				a.ImulRegReg(RAX, RDX)
			}
			e.jccStub(condOverflow, pc)
			a.MovMemDImm(RSI, e.d(in.C, 0), int32(L.TypInt))
			a.MovMemDReg(RSI, e.d(in.C, L.OffI), RAX)
			a.MovMemDImm(RSI, e.d(in.C, L.OffF), 0)
			for k := range 3 {
				a.MovMemDImm(RSI, e.d(in.C, L.OffS+8*k), 0)
			}
			a.Bind(join)
			e.unmap(in.C)
			e.know.wroteAny(in.C, true)
		}
	case VFCmpJump, VFCmpSet, VFCmpBr:
		e.fcmpLoad(pc, in.B, X0)
		e.fcmpLoad(pc, in.A, X1)
		c := floatCond(in.Cond)
		if in.Op != VFCmpJump && !in.Dead {
			e.destNoS(pc, in.C)
			a.MovRegImm64(R10, 0)
		}
		a.Ucomisd(X0, X1)
		e.jccStub(ccP, pc) // a NaN, which no VDBE value holds
		if in.Op == VFCmpJump {
			e.jccBlock(c, pc, in.T)
			break
		}
		if !in.Dead {
			a.Setcc(c, R10)
			m := e.dest(pc, in.C, R10)
			if m != R10 {
				a.MovRegReg(m, R10)
			}
			e.storeInt(pc, in, in.C, m)
		} else {
			e.clobber(in.C)
		}
		if in.Op == VFCmpBr {
			if in.Imm == 0 {
				c = floatCond(in.Cond.Negate())
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
func (e *vmEmit) fcmpLoad(pc, r int, x XReg) {
	a, L := e.a, e.lay
	if e.know.isFloat.has(r) {
		a.MovsdLoad(x, RSI, e.d(r, L.OffF))
		return
	}
	isInt, done := e.vmLabel(), e.vmLabel()
	a.CmpMemDByte(RSI, e.d(r, L.OffTyp), L.TypFloat)
	a.JccL(CondNE, isInt)
	a.MovsdLoad(x, RSI, e.d(r, L.OffF))
	e.cold = append(e.cold, vmCold{body: func() {
		a.Bind(isInt)
		a.CmpMemDByte(RSI, e.d(r, L.OffTyp), L.TypInt)
		e.jccStub(CondNE, pc)
		a.MovRegMemD(RAX, RSI, e.d(r, L.OffI))
		a.MovRegReg(RDX, RAX)
		a.MovRegImm64(R9, 1<<53)
		a.AddRegReg(RDX, R9)
		a.MovRegImm64(R9, 1<<54)
		a.CmpRegReg(RDX, R9)
		e.jccStub(condAbove, pc) // unsigned: outside [-2^53, 2^53]
		a.Xorps(x, x)
		a.Cvtsi2sdq(x, RAX)
		a.JmpL(done)
	}})
	a.Bind(done)
}

// math emits VMath: the arguments as doubles into ProgArgs.Scratch, the
// kernel inline over them (its answer into Scratch[0], its spills above
// both), then the answer into r[C].
func (e *vmEmit) math(pc int, in VInsn) error {
	if in.Kernel == nil || in.B < 1 || in.B > 2 || in.Kernel.NArgs != in.B {
		e.exitAt(pc)
		return nil
	}
	a := e.a
	for i := 0; i < in.B; i++ {
		x := XReg(i)
		e.fload(pc, in.A+i, x, in.Imm == 0, false, 0)
		a.MovsdStore(RDI, int32(POffScratch+8*i), x)
	}
	exit := e.vmLabelName("mx")
	env := &firEnv{
		args:      []firLoc{firMem(RDI, POffScratch), firMem(RDI, POffScratch+8)}[:in.B],
		out:       firMem(RDI, POffScratch),
		exit:      exit,
		gprs:      vmFKernelGPRs,
		xmms:      vmFKernelXMMs,
		spillBase: RDI, spillDisp: POffScratch + 16, spillN: ProgScratch - 2,
		prefix: e.vmLabelName("k"), pool: e.consts,
	}
	if err := emitFirAmd64(a, in.Kernel, env, cpuFeat); err != nil {
		return err
	}
	done := e.vmLabel()
	a.JmpL(done)
	a.Label(exit)
	e.exitAt(pc)
	a.Bind(done)
	a.MovsdLoad(X0, RDI, POffScratch)
	a.Ucomisd(X0, X0)
	e.jccStub(ccP, pc) // NaN: NULL
	e.storeFloat(pc, in, in.C, X0)
	return nil
}
