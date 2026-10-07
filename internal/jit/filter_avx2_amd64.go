//go:build amd64

package jit

import "fmt"

// EmitFilterCountSIMD emits a vector kernel counting rows satisfying one or two
// integer comparisons, four int64 lanes per instruction. Supports only direct
// lane-wise comparisons (VPCMPGTQ, VPCMPEQQ) and their negations.
func EmitFilterCountSIMD(condA Cond, two bool, condC Cond) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()

	// Y1 = xa broadcast, Y2 = xc broadcast, Y0 = accumulator.
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(RAX, RDI, OffXA)
	if two {
		a.MovRegMem(RDX, RDI, OffC)
		a.MovRegMem(RBX, RDI, OffXC)
	}
	a.XorRegReg(R8, R8)
	a.VmovqToXmm(1, RAX)
	a.VpbroadcastqYmm(1, 1)
	if two {
		a.VmovqToXmm(2, RBX)
		a.VpbroadcastqYmm(2, 2)
	}
	a.VpxorYmm(0, 0, 0)

	a.MovRegReg(R10, RCX)
	a.ShrRegImm8(R10, 2) // full four-lane blocks
	a.AndRegImm8(RCX, 3) // scalar tail
	a.TestRegReg(R10, R10)
	a.Jcc(CondE, "tail")

	a.Label("block")
	a.VmovdquLoad(3, RSI)
	emitLaneCmp(a, 4, 3, 1, condA) // Y4 = a OP xa
	if two {
		a.VmovdquLoad(5, RDX)
		emitLaneCmp(a, 6, 5, 2, condC) // Y6 = c OP xc
		a.VpandYmm(7, 4, 6)            // both
		a.VpsubqYmm(0, 0, 7)           // += 1 per hit
	} else {
		a.VpsubqYmm(0, 0, 4) // += 1 per hit
	}
	a.AddRegImm8(RSI, 32)
	if two {
		a.AddRegImm8(RDX, 32)
	}
	a.DecReg(R10)
	a.Jcc(CondNE, "block")

	// Horizontal sum of the four lanes.
	a.Vextracti128(3, 0, 1)
	a.VpaddqXmm(0, 0, 3)
	a.VmovqToReg(R9, 0)
	a.AddRegReg(R8, R9)
	a.Vpextrq(R9, 0, 1)
	a.AddRegReg(R8, R9)

	a.Label("tail")
	a.TestRegReg(RCX, RCX)
	a.Jcc(CondE, "done")
	a.Label("tailloop")
	a.MovRegMem(R9, RSI, 0)
	a.CmpRegReg(R9, RAX)
	a.Jcc(condA.Negate(), "tailnext")
	if two {
		a.MovRegMem(R9, RDX, 0)
		a.CmpRegReg(R9, RBX)
		a.Jcc(condC.Negate(), "tailnext")
	}
	a.IncReg(R8)
	a.Label("tailnext")
	a.AddRegImm8(RSI, 8)
	if two {
		a.AddRegImm8(RDX, 8)
	}
	a.DecReg(RCX)
	a.Jcc(CondNE, "tailloop")

	a.Label("done")
	a.Vzeroupper()
	a.MovRegMem(R9, RDI, OffOut)
	a.MovMemReg(R9, 0, R8)
	a.Ret()
	return a.Code()
}

// emitLaneCmp leaves dst all-ones in each lane where "src OP bound" holds.
//
// x86 gives signed 64-bit lanes exactly two primitives, greater-than and
// equal-to, so the other four operators are composed here -- at EMIT time,
// where it costs nothing, rather than per row.
func emitLaneCmp(a *Asm, dst, src, bound Reg, cond Cond) {
	switch cond {
	case CondG: // src > bound
		a.VpcmpgtqYmm(dst, src, bound)
	case CondLE: // NOT (src > bound)
		a.VpcmpgtqYmm(dst, src, bound)
		a.VpcmpeqqYmm(15, 15, 15) // all ones
		a.VpandnYmm(dst, dst, 15)
	case CondL: // bound > src
		a.VpcmpgtqYmm(dst, bound, src)
	case CondGE: // NOT (bound > src)
		a.VpcmpgtqYmm(dst, bound, src)
		a.VpcmpeqqYmm(15, 15, 15)
		a.VpandnYmm(dst, dst, 15)
	case CondE:
		a.VpcmpeqqYmm(dst, src, bound)
	default: // CondNE: NOT equal
		a.VpcmpeqqYmm(dst, src, bound)
		a.VpcmpeqqYmm(15, 15, 15)
		a.VpandnYmm(dst, dst, 15)
	}
}

// EmitFilterSumSIMD emits a vector kernel that sums the V column over rows
// satisfying one or two integer comparisons, and counts those rows: Out gets
// the sum, Out2 the count. Each lane's mask is ANDed with the values before
// the add, so a row that fails contributes zero.
//
// It cannot detect overflow; the caller uses it only when the column's range
// proves no partial sum overflows (segSumCannotOverflow).
func EmitFilterSumSIMD(condA Cond, two bool, condC Cond) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()

	// Y1 = xa, Y2 = xc, Y0 = sum, Y8 = count.
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(R11, RDI, OffV)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(RAX, RDI, OffXA)
	if two {
		a.MovRegMem(RDX, RDI, OffC)
		a.MovRegMem(RBX, RDI, OffXC)
	}
	a.XorRegReg(R8, R8)
	a.XorRegReg(R12, R12)
	a.VmovqToXmm(1, RAX)
	a.VpbroadcastqYmm(1, 1)
	if two {
		a.VmovqToXmm(2, RBX)
		a.VpbroadcastqYmm(2, 2)
	}
	a.VpxorYmm(0, 0, 0)
	a.VpxorYmm(8, 8, 8)

	a.MovRegReg(R10, RCX)
	a.ShrRegImm8(R10, 2)
	a.AndRegImm8(RCX, 3)
	a.TestRegReg(R10, R10)
	a.Jcc(CondE, "tail")

	a.Label("block")
	a.VmovdquLoad(3, RSI)
	emitLaneCmp(a, 4, 3, 1, condA)
	mask := Reg(4)
	if two {
		a.VmovdquLoad(5, RDX)
		emitLaneCmp(a, 6, 5, 2, condC)
		a.VpandYmm(7, 4, 6)
		mask = 7
	}
	a.VmovdquLoad(9, R11)
	a.VpandYmm(10, 9, mask)
	a.VpaddqYmm(0, 0, 10)
	a.VpsubqYmm(8, 8, mask) // += 1 per hit
	a.AddRegImm8(RSI, 32)
	a.AddRegImm8(R11, 32)
	if two {
		a.AddRegImm8(RDX, 32)
	}
	a.DecReg(R10)
	a.Jcc(CondNE, "block")

	a.Vextracti128(3, 0, 1)
	a.VpaddqXmm(0, 0, 3)
	a.VmovqToReg(R9, 0)
	a.AddRegReg(R8, R9)
	a.Vpextrq(R9, 0, 1)
	a.AddRegReg(R8, R9)
	a.Vextracti128(3, 8, 1)
	a.VpaddqXmm(8, 8, 3)
	a.VmovqToReg(R9, 8)
	a.AddRegReg(R12, R9)
	a.Vpextrq(R9, 8, 1)
	a.AddRegReg(R12, R9)

	a.Label("tail")
	a.TestRegReg(RCX, RCX)
	a.Jcc(CondE, "done")
	a.Label("tailloop")
	a.MovRegMem(R9, RSI, 0)
	a.CmpRegReg(R9, RAX)
	a.Jcc(condA.Negate(), "tailnext")
	if two {
		a.MovRegMem(R9, RDX, 0)
		a.CmpRegReg(R9, RBX)
		a.Jcc(condC.Negate(), "tailnext")
	}
	a.MovRegMem(R9, R11, 0)
	a.AddRegReg(R8, R9)
	a.IncReg(R12)
	a.Label("tailnext")
	a.AddRegImm8(RSI, 8)
	a.AddRegImm8(R11, 8)
	if two {
		a.AddRegImm8(RDX, 8)
	}
	a.DecReg(RCX)
	a.Jcc(CondNE, "tailloop")

	a.Label("done")
	a.Vzeroupper()
	a.MovRegMem(R9, RDI, OffOut)
	a.MovMemReg(R9, 0, R8)
	a.MovRegMem(R9, RDI, OffOut2)
	a.MovMemReg(R9, 0, R12)
	a.Ret()
	return a.Code()
}
