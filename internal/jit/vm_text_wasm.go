package jit

// The VM JIT's TEXT arms in wasm, as vm_text_amd64.go's, a byte at a time.
// txa and txb are the two byte addresses; t1 to t3 are free as in any arm.

// substr emits VSubstr.
func (e *vmEmitWasm) substr(pc int, in VInsn) {
	w, L := e.w, e.lay
	if in.Dead {
		e.forgetReg(in.C)
		return
	}
	w.Get(e.wb)
	e.exitIf(pc)
	for k, typ := range []byte{L.TypText, L.TypInt, L.TypInt} {
		e.loadTyp(in.A + k)
		w.I32(int32(typ))
		w.op(opI32Ne)
		if k > 0 {
			w.op(opI32Or)
		}
	}
	e.loadI64(in.A+2, L.OffI)
	w.I64(1)
	w.op(opI64Ne)
	w.op(opI32Or)
	e.exitIf(pc)
	// y from 1 to the length: y-1 below the length, unsigned.
	e.loadI64(in.A+1, L.OffI)
	w.Set(e.t1)
	w.Get(e.t1)
	w.I64(1)
	w.op(opI64Sub)
	e.loadI64(in.A, L.OffS+8)
	w.op(opI64GeU)
	e.exitIf(pc)
	// The first y bytes, none NUL or above 0x7f: b-1 below 0x7f, unsigned.
	e.loadI64(in.A, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	w.Get(e.t1)
	w.op(opI32WrapI64)
	w.Set(e.txb)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.txb)
	w.op(opI32Eqz)
	w.BrIf(1)
	w.Get(e.txa)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.I32(1)
	w.op(opI32Sub)
	w.I32(0x7f)
	w.op(opI32GeU)
	e.exitIf(pc)
	w.Get(e.txa)
	w.I32(1)
	w.op(opI32Add)
	w.Set(e.txa)
	w.Get(e.txb)
	w.I32(1)
	w.op(opI32Sub)
	w.Set(e.txb)
	w.Br(0)
	w.op(opEnd, opEnd)
	for _, f := range [][2]int64{{0, int64(L.TypText)}, {int64(L.OffI), 0}, {int64(L.OffF), 0}, {int64(L.OffS + 8), 1}, {int64(L.OffS + 16), 1}} {
		w.Get(e.regs)
		w.I64(f[1])
		w.StoreI64(e.addr(in.C, int(f[0])))
	}
	w.Get(e.regs)
	w.Get(e.txa)
	w.I32(1)
	w.op(opI32Sub)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.op(opI64ExtendI32U)
	w.I64(in.Imm)
	w.op(opI64Add)
	w.StoreI64(e.addr(in.C, L.OffS))
	e.forgetReg(in.C)
}

// textCmp emits VTextCmpJump and VTextCmpSet, with t1 1 when the operands
// are equal.
func (e *vmEmitWasm) textCmp(pc int, in VInsn) {
	w, L := e.w, e.lay
	if in.Op == VTextCmpSet {
		if in.Dead {
			e.forgetReg(in.C)
			return
		}
		e.guards(pc, e.dstNoS(in, in.C))
	}
	e.loadTyp(in.A)
	w.I32(int32(L.TypInt))
	w.op(opI32Eq)
	w.op(opIf, opVoid)
	e.loadTyp(in.B)
	w.I32(int32(L.TypInt))
	w.op(opI32Ne)
	e.exitIf(pc)
	e.loadI64(in.B, L.OffI)
	e.loadI64(in.A, L.OffI)
	w.op(opI64Eq)
	w.op(opI64ExtendI32U)
	w.Set(e.t1)
	w.op(opElse)
	e.loadTyp(in.A)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	e.loadTyp(in.B)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	w.op(opI32Or)
	w.Get(0)
	w.LoadI64(POffUTF16)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	w.op(opI32Or)
	e.exitIf(pc)
	w.I64(0)
	w.Set(e.t1)
	e.loadI64(in.A, L.OffS+8)
	e.loadI64(in.B, L.OffS+8)
	w.op(opI64Eq)
	w.op(opIf, opVoid)
	e.loadI64(in.A, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	e.loadI64(in.B, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txb)
	e.loadI64(in.A, L.OffS+8)
	w.Set(e.t2)
	w.I64(1)
	w.Set(e.t1)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t2)
	w.op(opI64Eqz)
	w.BrIf(1)
	w.Get(e.txa)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.Get(e.txb)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.op(opI32Ne)
	w.op(opIf, opVoid)
	w.I64(0)
	w.Set(e.t1)
	w.Br(2)
	w.op(opEnd)
	w.Get(e.txa)
	w.I32(1)
	w.op(opI32Add)
	w.Set(e.txa)
	w.Get(e.txb)
	w.I32(1)
	w.op(opI32Add)
	w.Set(e.txb)
	w.Get(e.t2)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.t2)
	w.Br(0)
	w.op(opEnd, opEnd) // the loop and its block
	w.op(opEnd)        // equal lengths
	w.op(opEnd)        // INTEGERs or TEXTs
	if in.Cond == CondNE {
		w.Get(e.t1)
		w.op(opI64Eqz)
		w.op(opI64ExtendI32U)
		w.Set(e.t1)
	}
	if in.Op == VTextCmpJump {
		w.Get(e.t1)
		w.op(opI32WrapI64)
		e.jumpIf(in.T)
		return
	}
	e.storeInt(in, in.C, e.t1)
}
