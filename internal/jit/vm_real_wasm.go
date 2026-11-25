package jit

// The VM JIT's REAL arms (vm.go's VReal to VMath) in wasm, as
// vm_real_amd64.go's: a REAL operand is its F word as an f64, an INTEGER one
// converted from its I word, and every check runs before anything is
// written, so an exit leaves the register file as the VDBE expects it.

// fcmpOp is the f64 comparison of r[B] Cond r[A].
func fcmpOp(c Cond) byte {
	switch c {
	case CondE:
		return opF64Eq
	case CondNE:
		return opF64Ne
	case CondL:
		return opF64Lt
	case CondG:
		return opF64Gt
	case CondLE:
		return opF64Le
	default: // CondGE
		return opF64Ge
	}
}

// flocals declares the REAL arms' scratch at first use: two f64s and the
// flag that an operand was a REAL.
func (e *vmEmitWasm) flocals() (uint32, uint32) {
	if e.f0 == 0 {
		e.f0, e.f1, e.fr = e.w.Local(wF64), e.w.Local(wF64), e.w.Local(wI32)
	}
	return e.f0, e.f1
}

func (e *vmEmitWasm) loadF64(r int) { e.w.Get(e.regs); e.w.LoadF64(e.addr(r, e.lay.OffF)) }

// fload sets dst to r[r] as a double: a REAL's F, an INTEGER converted (only
// when intOK), a TEXT spelling a REAL (only when text, VFText), anything
// else exiting at pc. With flag, a REAL sets fr.
func (e *vmEmitWasm) fload(pc, r int, dst uint32, intOK, text, flag bool) {
	w, L := e.w, e.lay
	switch {
	case e.know.isFloat.has(r):
		e.loadF64(r)
	case e.know.isInt.has(r) && intOK:
		e.loadI64(r, L.OffI)
		w.op(opF64ConvertI64S)
	case !intOK:
		e.loadTyp(r)
		w.I32(int32(L.TypFloat))
		w.op(opI32Ne)
		e.exitIf(pc)
		e.know.isFloat.add(r)
		e.loadF64(r)
	default:
		e.loadTyp(r)
		w.I32(int32(L.TypFloat))
		w.op(opI32Eq)
		w.op(opIf, wF64)
		if flag {
			w.I32(1)
			w.Set(e.fr)
		}
		e.loadF64(r)
		w.op(opElse)
		if text {
			e.loadTyp(r)
			w.I32(int32(L.TypInt))
			w.op(opI32Eq)
			w.op(opIf, wF64)
			e.loadI64(r, L.OffI)
			w.op(opF64ConvertI64S)
			w.op(opElse)
			e.ftext(pc, r, flag)
			w.op(opEnd)
		} else {
			e.loadTyp(r)
			w.I32(int32(L.TypInt))
			w.op(opI32Ne)
			e.exitIf(pc)
			e.loadI64(r, L.OffI)
			w.op(opF64ConvertI64S)
		}
		w.op(opEnd)
	}
	w.Set(dst)
}

// ftext pushes the REAL a TEXT r[r] spells as [-]digits.digits, as
// vm_real_amd64.go's does. txa walks the text to its end in txb, t1 is the
// digits, t2 the digits after the point and then the power of ten's count,
// fc the byte and fp the power of ten.
func (e *vmEmitWasm) ftext(pc, r int, flag bool) {
	w, L := e.w, e.lay
	if e.fc == 0 {
		e.fc, e.fp = w.Local(wI32), w.Local(wF64)
	}
	digit := func() {
		w.Get(e.fc)
		w.I32('0')
		w.op(opI32Sub)
		w.Set(e.fc)
		w.Get(e.fc)
		w.I32(9)
		w.op(opI32GtU)
		e.exitIf(pc)
		w.Get(e.t1)
		w.I64(10)
		w.op(opI64Mul)
		w.Get(e.fc)
		w.op(opI64ExtendI32U)
		w.op(opI64Add)
		w.Set(e.t1)
		w.Get(e.t1)
		w.I64(53)
		w.op(opI64ShrU)
		w.op(opI64Eqz)
		w.op(opI32Eqz)
		e.exitIf(pc) // 2^53 and above
	}
	next := func() { // fc = the byte at txa, txa past it
		w.Get(e.txa)
		w.op(opI32Load8U)
		w.memarg(0, 0)
		w.Set(e.fc)
		w.Get(e.txa)
		w.I32(1)
		w.op(opI32Add)
		w.Set(e.txa)
	}
	e.loadTyp(r)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	e.exitIf(pc)
	e.loadI64(r, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	e.loadI64(r, L.OffS+8)
	w.op(opI32WrapI64)
	w.Get(e.txa)
	w.op(opI32Add)
	w.Set(e.txb) // the end
	w.Get(e.txa)
	w.Get(e.txb)
	w.op(opI32Eq)
	e.exitIf(pc)
	w.Get(e.txa)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.I32('-')
	w.op(opI32Eq)
	w.op(opIf, opVoid)
	w.Get(e.txa)
	w.I32(1)
	w.op(opI32Add)
	w.Set(e.txa)
	w.op(opEnd)
	// A digit and the point at least.
	w.Get(e.txb)
	w.Get(e.txa)
	w.op(opI32Sub)
	w.I32(2)
	w.op(opI32LtS)
	e.exitIf(pc)
	w.I64(0)
	w.Set(e.t1)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.txa)
	w.Get(e.txb)
	w.op(opI32Eq)
	e.exitIf(pc) // no point: an INTEGER's spelling
	next()
	w.Get(e.fc)
	w.I32('.')
	w.op(opI32Eq)
	w.BrIf(1)
	digit()
	w.Br(0)
	w.op(opEnd, opEnd)
	w.Get(e.txb)
	w.Get(e.txa)
	w.op(opI32Sub)
	w.op(opI64ExtendI32U)
	w.Set(e.t2)
	w.Get(e.t2)
	w.I64(22)
	w.op(opI64GtU)
	e.exitIf(pc)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.txa)
	w.Get(e.txb)
	w.op(opI32Eq)
	w.BrIf(1)
	next()
	digit()
	w.Br(0)
	w.op(opEnd, opEnd)
	// 10^k by tens, each power up to 10^22 exact.
	w.F64(1)
	w.Set(e.fp)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t2)
	w.op(opI64Eqz)
	w.BrIf(1)
	w.Get(e.fp)
	w.F64(10)
	w.op(opF64Mul)
	w.Set(e.fp)
	w.Get(e.t2)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.t2)
	w.Br(0)
	w.op(opEnd, opEnd)
	w.Get(e.t1)
	w.op(opF64ConvertI64S)
	w.Get(e.fp)
	w.op(opF64Div)
	w.Set(e.fp)
	w.Get(e.fp)
	w.op(opF64Neg)
	w.Get(e.fp)
	e.loadI64(r, L.OffS)
	w.op(opI32WrapI64)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.I32('-')
	w.op(opI32Eq)
	w.op(opSelect)
	if flag {
		w.I32(1)
		w.Set(e.fr)
	}
}

// fcmpLoad sets dst to a comparison's operand: a REAL, or an INTEGER within
// 2^53 of zero, which a double holds exactly and so compares as
// sqlite3IntFloatCompare would.
func (e *vmEmitWasm) fcmpLoad(pc, r int, dst uint32) {
	w, L := e.w, e.lay
	if e.know.isFloat.has(r) {
		e.loadF64(r)
		w.Set(dst)
		return
	}
	isInt := e.know.isInt.has(r)
	if !isInt {
		e.loadTyp(r)
		w.I32(int32(L.TypFloat))
		w.op(opI32Eq)
		w.op(opIf, wF64)
		e.loadF64(r)
		w.op(opElse)
		e.loadTyp(r)
		w.I32(int32(L.TypInt))
		w.op(opI32Ne)
		e.exitIf(pc)
	}
	e.loadI64(r, L.OffI)
	w.Set(e.t2)
	w.Get(e.t2)
	w.I64(1 << 53)
	w.op(opI64Add)
	w.I64(1 << 54)
	w.op(opI64GtU) // unsigned: outside [-2^53, 2^53]
	e.exitIf(pc)
	w.Get(e.t2)
	w.op(opF64ConvertI64S)
	if !isInt {
		w.op(opEnd)
	}
	w.Set(dst)
}

// exitNaN exits at pc when the f64 in x is a NaN: NULL, which is the VDBE's.
func (e *vmEmitWasm) exitNaN(pc int, x uint32) {
	e.w.Get(x)
	e.w.Get(x)
	e.w.op(opF64Ne)
	e.exitIf(pc)
}

// storeFloat makes r[r] the REAL in x: Value{Typ: Float, F: x}.
func (e *vmEmitWasm) storeFloat(pc int, in VInsn, r int, x uint32) {
	if in.Dead {
		e.forgetReg(r)
		return
	}
	e.guards(pc, e.dstNoS(in, r))
	w, L := e.w, e.lay
	w.Get(e.regs)
	w.I64(int64(L.TypFloat))
	w.StoreI64(e.addr(r, 0))
	w.Get(e.regs)
	w.I64(0)
	w.StoreI64(e.addr(r, L.OffI))
	w.Get(e.regs)
	w.Get(x)
	w.StoreF64(e.addr(r, L.OffF))
	e.clearS(r)
	e.know.wroteFloat(r)
}

var vmWasmFArith = map[VOp]byte{VFAdd: opF64Add, VFSub: opF64Sub, VFMul: opF64Mul, VFDiv: opF64Div}

// realInsn emits the REAL arms.
func (e *vmEmitWasm) realInsn(pc int, in VInsn) error {
	w := e.w
	f0, f1 := e.flocals()
	switch in.Op {
	case VReal:
		if in.Dead {
			e.forgetReg(in.A)
			break
		}
		w.I64(in.Imm)
		w.op(opF64ReinterpretI64)
		w.Set(f0)
		e.storeFloat(pc, in, in.A, f0)
	case VFAdd, VFSub, VFMul, VFDiv:
		// Two INTEGERs are the INTEGER arm, as vm_real_amd64.go's: for an
		// add, subtract or multiply, which overflows into a REAL only where
		// it must (that exits); a divide exits.
		flag := !e.know.isFloat.has(in.A) && !e.know.isFloat.has(in.B)
		if flag {
			w.I32(0)
			w.Set(e.fr)
		}
		e.fload(pc, in.B, f0, true, in.Imm&VFText != 0, flag)
		e.fload(pc, in.A, f1, true, in.Imm&VFText != 0, flag)
		if flag && in.Op != VFDiv && !in.Dead {
			e.realOrInts(pc, in, f0, f1)
			break
		}
		if flag {
			w.Get(e.fr)
			w.op(opI32Eqz)
			e.exitIf(pc)
		}
		if in.Op == VFDiv {
			w.Get(f1)
			w.F64(0)
			w.op(opF64Eq)
			e.exitIf(pc) // a zero divisor: NULL
		}
		w.Get(f0)
		w.Get(f1)
		w.op(vmWasmFArith[in.Op])
		w.Set(f0)
		e.exitNaN(pc, f0)
		e.storeFloat(pc, in, in.C, f0)
	case VFCmpJump, VFCmpSet, VFCmpBr:
		e.fcmpLoad(pc, in.B, f0)
		e.fcmpLoad(pc, in.A, f1)
		e.exitNaN(pc, f0) // a NaN, which no VDBE value holds
		e.exitNaN(pc, f1)
		if in.Op == VFCmpJump {
			w.Get(f0)
			w.Get(f1)
			w.op(fcmpOp(in.Cond))
			e.jumpIf(in.T)
			break
		}
		e.guards(pc, e.dstNoS(in, in.C))
		w.Get(f0)
		w.Get(f1)
		w.op(fcmpOp(in.Cond))
		w.op(opI64ExtendI32U)
		w.Set(e.t1)
		e.storeInt(in, in.C, e.t1)
		if in.Op == VFCmpBr {
			w.Get(e.t1)
			w.op(opI64Eqz)
			if in.Imm != 0 {
				w.op(opI32Eqz) // jump when 1
			}
			e.jumpIf(in.T)
		}
	case VMath:
		return e.math(pc, in)
	}
	return nil
}

// realOrInts is VFAdd, VFSub or VFMul over operands fload left in f0 and f1,
// fr saying whether either was a REAL: the REAL arm when one was, the INTEGER
// arm over their I words when neither was.
func (e *vmEmitWasm) realOrInts(pc int, in VInsn, f0, f1 uint32) {
	w, L := e.w, e.lay
	e.guards(pc, e.dstNoS(in, in.C)) // for both arms
	zero := e.zeroS.has(in.C)
	w.Get(e.fr)
	e.openIf()
	w.Get(f0)
	w.Get(f1)
	w.op(vmWasmFArith[in.Op])
	w.Set(f0)
	e.exitNaN(pc, f0)
	e.storeFloat(pc, in, in.C, f0)
	w.op(opElse)
	if zero {
		e.zeroS.add(in.C) // storeFloat cleared it for its own arm
	}
	e.loadI64(in.B, L.OffI)
	w.Set(e.t1)
	e.loadI64(in.A, L.OffI)
	w.Set(e.t2)
	w.I64(0)
	w.Set(e.ovf)
	emitCheckedArith(w, map[VOp]POp{VFAdd: POpAdd, VFSub: POpSub, VFMul: POpMul}[in.Op], e.t1, e.t2, e.t3, e.ovf)
	w.Get(e.ovf)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	e.exitIf(pc) // overflow: the VDBE's, which promotes to a REAL
	e.writeInt(in.C, e.t3)
	e.closeIf()
	e.forgetReg(in.C)
	e.know.noS.add(in.C)
}

// math emits VMath: the arguments as doubles, the kernel inline over them,
// its answer into r[C].
func (e *vmEmitWasm) math(pc int, in VInsn) error {
	if in.Kernel == nil || in.B < 1 || in.B > 2 || in.Kernel.NArgs != in.B {
		e.exitAt(pc)
		return nil
	}
	f0, _ := e.flocals()
	args := make([]uint32, in.B)
	for i := range args {
		args[i] = e.w.Local(wF64)
		e.fload(pc, in.A+i, args[i], in.Imm == 0, false, false)
	}
	if err := emitFirWasm(e.w, in.Kernel, args, f0, func() { e.exitAt(pc) }); err != nil {
		return err
	}
	e.exitNaN(pc, f0)
	e.storeFloat(pc, in, in.C, f0)
	return nil
}
