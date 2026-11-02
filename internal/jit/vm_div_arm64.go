//go:build arm64

package jit

import "math/bits"

// divConst is VDiv or VRem by d, as vm_div_amd64.go's: SMULH and shifts in
// place of an SDIV. X4-X6 are scratch.
func (e *vmEmitArm) divConst(pc int, in VInsn, d int64) {
	vmDivConsts.Add(1)
	a := e.a
	n := e.src(pc, in.B, X9)
	if !in.Dead {
		e.destNoS(pc, in.C)
	}
	if d&(d-1) == 0 {
		k := uint32(bits.TrailingZeros64(uint64(d)))
		a.AsrImm(X4, n, 63)
		a.LsrImm(X4, X4, 64-k)
		a.AddReg(X4, X4, n)
		a.AsrImm(X4, X4, k)
	} else {
		m, s := sdivMagic(d)
		a.MovImm64(X5, m)
		a.Smulh(X4, n, X5)
		if m < 0 {
			a.AddReg(X4, X4, n)
		}
		if s > 0 {
			a.AsrImm(X4, X4, uint32(s))
		}
		a.LsrImm(X5, n, 63)
		a.AddReg(X4, X4, X5)
	}
	res := X4
	if in.Op == VRem {
		a.MovImm64(X5, d)
		a.Msub(X6, X4, X5, n)
		res = X6
	}
	m := e.dest(pc, in.C, res)
	if m != res {
		a.MovReg(m, res)
	}
	e.storeInt(in, in.C, m)
}
