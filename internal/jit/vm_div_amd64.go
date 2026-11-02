//go:build amd64

package jit

import "math/bits"

// divConst is VDiv or VRem by d, a divisor from 2 the emitter knows
// (vmKnow.konst), where neither exit can happen: a multiply by d's magic
// number and shifts (sdivMagic), or shifts alone for a power of two, in place
// of an IDIV and its tens of cycles. RAX, RDX and R10 are scratch.
func (e *vmEmit) divConst(pc int, in VInsn, d int64) {
	vmDivConsts.Add(1)
	a := e.a
	n := e.src(pc, in.B, R9)
	if !in.Dead {
		e.destNoS(pc, in.C)
	}
	if d&(d-1) == 0 {
		// A negative n is biased by d-1, so the shift truncates toward 0.
		k := byte(bits.TrailingZeros64(uint64(d)))
		a.MovRegReg(RDX, n)
		a.SarRegImm8(RDX, 63)
		a.ShrRegImm8(RDX, 64-k)
		a.AddRegReg(RDX, n)
		a.SarRegImm8(RDX, k)
	} else {
		m, s := sdivMagic(d)
		a.MovRegImm64(RAX, m)
		a.ImulRDXRAX(n)
		if m < 0 {
			a.AddRegReg(RDX, n)
		}
		if s > 0 {
			a.SarRegImm8(RDX, byte(s))
		}
		a.MovRegReg(RAX, n)
		a.ShrRegImm8(RAX, 63)
		a.AddRegReg(RDX, RAX)
	}
	res := RDX
	if in.Op == VRem {
		a.ImulRegRegImm32(RDX, RDX, int32(d))
		a.MovRegReg(R10, n)
		a.SubRegReg(R10, RDX)
		res = R10
	}
	if m := e.dest(pc, in.C, res); m != res {
		a.MovRegReg(m, res)
		res = m
	}
	e.storeInt(pc, in, in.C, res)
}
