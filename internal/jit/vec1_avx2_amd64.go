//go:build amd64

package jit

import (
	"fmt"

	"golang.org/x/sys/cpu"
)

// EmitVec1T8 emits the kernel vec1's searches and rebuilds compare a vector
// against many with: rows laid out T8 -- in blocks of eight, component by
// component, so a ymm register holds one component of eight rows -- and each
// row's sum accumulated in component order with one rounding per operation
// (separate multiply and add, never fused), as vec1's portable C sums it.
// Several blocks go per iteration, to cover the adds' latency: four for the
// float32 sums, two for Vec1L2D (whose float64 sums take twice the
// registers).
//
// Args: A the first block, XC a block's size in bytes (32 x dims), V the
// query's float32 components, N the groups of blocks (four, or two for
// Vec1L2D; at least 1), XA the dimensions (at least 1), Out the sums: a
// float32 per row, or for Vec1L2D a float64.
func EmitVec1T8(op Vec1Op) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	if op != Vec1L2D {
		return emitVec1T8F(op)
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RBX, RDI, OffXC)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.MovRegMem(R10, RDI, OffOut)

	// Block one: values Y2, sums Y0 (and Y1, the high half's float64s).
	// Block two: values Y8, sums Y6 (and Y7). Y3 the query component.
	a.Label("pair")
	for _, r := range []Reg{0, 1, 6, 7} {
		a.VxorpsYmm(r, r, r)
	}
	a.MovRegReg(RDX, RSI)
	a.AddRegReg(RDX, RBX) // the second block
	a.MovRegMem(R8, RDI, OffV)
	a.MovRegReg(R12, R9)

	a.Label("comp")
	a.VmovdquLoad(2, RSI)
	a.VmovdquLoad(8, RDX)
	a.VbroadcastssMem(3, R8)
	switch op {
	case Vec1L2F:
		a.VsubpsYmm(2, 3, 2)
		a.VsubpsYmm(8, 3, 8)
		a.VmulpsYmm(2, 2, 2)
		a.VmulpsYmm(8, 8, 8)
		a.VaddpsYmm(0, 0, 2)
		a.VaddpsYmm(6, 6, 8)
	case Vec1DotF:
		a.VmulpsYmm(2, 2, 3)
		a.VmulpsYmm(8, 8, 3)
		a.VaddpsYmm(0, 0, 2)
		a.VaddpsYmm(6, 6, 8)
	case Vec1L2D:
		a.VsubpsYmm(2, 3, 2)
		a.VsubpsYmm(8, 3, 8)
		a.Vextracti128(4, 2, 1) // rows 4..7
		a.Vextracti128(10, 8, 1)
		a.Vcvtps2pdYmm(5, 2) // rows 0..3, widened
		a.Vcvtps2pdYmm(4, 4)
		a.Vcvtps2pdYmm(11, 8)
		a.Vcvtps2pdYmm(10, 10)
		a.sqAccPd(0, 5)
		a.sqAccPd(1, 4)
		a.sqAccPd(6, 11)
		a.sqAccPd(7, 10)
	}
	a.AddRegImm8(RSI, 32)
	a.AddRegImm8(RDX, 32)
	a.AddRegImm8(R8, 4)
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	if op == Vec1L2D {
		a.VmovupsStore(R10, 0, 0)
		a.VmovupsStore(R10, 32, 1)
		a.VmovupsStore(R10, 64, 6)
		a.VmovupsStore(R10, 96, 7)
		a.AddRegImm8(R10, 64)
		a.AddRegImm8(R10, 64)
	} else {
		a.VmovupsStore(R10, 0, 0)
		a.VmovupsStore(R10, 32, 6)
		a.AddRegImm8(R10, 64)
	}
	a.MovRegReg(RSI, RDX) // past the second block
	a.DecReg(RCX)
	a.Jcc(CondNE, "pair")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// EmitVec1Scan16 emits vec1's PQ list scan (vec1ScanBlocked's sums): codes
// in blocks of sixteen rows, a block holding each codebook's sixteen code
// bytes in turn, and for every row the float32 sum of its codes' table
// entries, codebooks in order. Eight codes are widened to int32 and the
// entries gathered per instruction, a lane per row.
//
// Args: A the first block's first codebook's codes, XC a block's size in
// bytes, N the blocks (at least 1), XA the codebooks (at least 1), V the
// table (256 float32 per codebook), Out sixteen float32 per block.
func EmitVec1Scan16() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RBX, RDI, OffXC)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.MovRegMem(R10, RDI, OffOut)
	a.MovRegImm64(R11, 1024) // one codebook's table

	a.Label("blk")
	a.VxorpsYmm(0, 0, 0)
	a.VxorpsYmm(1, 1, 1)
	a.MovRegReg(RAX, RSI)
	a.MovRegMem(R8, RDI, OffV)
	a.MovRegReg(R12, R9)

	a.Label("book")
	a.VpmovzxbdLoad(2, RAX, 0)
	a.VpmovzxbdLoad(3, RAX, 8)
	a.VpcmpeqdYmm(4, 4, 4)
	a.VpcmpeqdYmm(5, 5, 5)
	a.VgatherdpsYmm4(6, R8, 2, 4)
	a.VgatherdpsYmm4(7, R8, 3, 5)
	a.VaddpsYmm(0, 0, 6)
	a.VaddpsYmm(1, 1, 7)
	a.AddRegImm8(RAX, 16)
	a.AddRegReg(R8, R11)
	a.DecReg(R12)
	a.Jcc(CondNE, "book")

	a.VmovupsStore(R10, 0, 0)
	a.VmovupsStore(R10, 32, 1)
	a.AddRegImm8(R10, 64)
	a.AddRegReg(RSI, RBX)
	a.DecReg(RCX)
	a.Jcc(CondNE, "blk")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// Vcvtps2pdYmm widens xmm src's four float32 to ymm dst's four float64.
func (a *Asm) Vcvtps2pdYmm(dst, src Reg) {
	a.vinst(0x5A, dst, 0, src, vexMap0F, 0, 1, 0, true)
}

// VmulpdYmm is  dst = src1 * src2, four float64 lanes.
func (a *Asm) VmulpdYmm(dst, src1, src2 Reg) {
	a.vinst(0x59, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VaddpdYmm is  dst = src1 + src2, four float64 lanes.
func (a *Asm) VaddpdYmm(dst, src1, src2 Reg) {
	a.vinst(0x58, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VpmovzxbdLoad is  dst = the eight bytes at [base+disp8], zero-extended to
// int32 lanes. base must not be RSP, RBP, R12 or R13.
func (a *Asm) VpmovzxbdLoad(dst, base Reg, disp int8) {
	a.vexm(dst, 0, base, 0, vexMap0F38, 0, 1, vexP66)
	a.emit(0x31, modrm(0b01, dst, base), byte(disp))
}

// VgatherdpsYmm4 is VgatherdpsYmm with the index scaled by 4: dst[i] = the
// float32 at [base + 4*index[i]].
func (a *Asm) VgatherdpsYmm4(dst, base, index, mask Reg) {
	a.vexm(dst, index, base, mask, vexMap0F38, 0, 1, vexP66)
	a.emit(0x92, modrm(0b00, dst, RSP), 2<<6|byte(index&7)<<3|byte(base&7))
}

// EmitVec1RowsL2D emits vec1L2Dist for rows stored back to back, as a flat
// vec1 index's lists hold them: each difference in float32, squared and
// summed in float64, in component order. Tiles of eight rows are read and
// transposed as EmitVecDistStrided reads them, so a lane holds a row.
//
// Args: A the first row, XC the stride in bytes, V the query, N the rows (a
// multiple of 8), XA the dimensions (a multiple of 8), Out a float64 per row.
func EmitVec1RowsL2D() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RBX, RDI, OffXC)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.ShrRegImm8(R9, 3)
	a.MovRegMem(R10, RDI, OffOut)
	a.LeaSIB(R13, RBX, RBX, 1)
	a.AddRegReg(R13, RBX)

	a.Label("tile")
	a.VxorpsYmm(14, 14, 14) // rows 0..3, float64
	a.VxorpsYmm(15, 15, 15) // rows 4..7
	a.MovRegReg(RAX, RSI)
	a.LeaSIB(RDX, RSI, RBX, 4)
	a.MovRegMem(R8, RDI, OffV)
	a.MovRegReg(R12, R9)

	a.Label("block")
	col := a.loadTransposed8()
	for k, c := range col {
		a.VbroadcastssMemD(12, R8, int8(4*k))
		a.VsubpsYmm(13, 12, c)
		a.Vextracti128(12, 13, 1)
		a.Vcvtps2pdYmm(13, 13)
		a.Vcvtps2pdYmm(12, 12)
		a.sqAccPd(14, 13)
		a.sqAccPd(15, 12)
	}
	a.AddRegImm8(RAX, 32)
	a.AddRegImm8(RDX, 32)
	a.AddRegImm8(R8, 32)
	a.DecReg(R12)
	a.Jcc(CondNE, "block")

	a.VmovupsStore(R10, 0, 14)
	a.VmovupsStore(R10, 32, 15)
	a.AddRegImm8(R10, 64)
	a.LeaSIB(RSI, RSI, RBX, 8)
	a.AddRegImm8(RCX, -8)
	a.Jcc(CondNE, "tile")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// sqAccPd is  acc += x*x  in float64, x clobbered. x holds float32 values
// widened, so x*x is exact and a fused multiply-add rounds once, as the
// separate multiply and add do: the two are the same sum, and the fused one
// is used where the machine has it.
func (a *Asm) sqAccPd(acc, x Reg) {
	if cpu.X86.HasFMA {
		a.vinst(0xB8, acc, x, x, vexMap0F38, 1, 1, vexP66, false) // VFMADD231PD
		return
	}
	a.VmulpdYmm(x, x, x)
	a.VaddpdYmm(acc, acc, x)
}

// EmitVec1BestD emits vec1PqBestMatch over rows laid out T8 (as EmitVec1T8
// takes them): every row's vec1L2Dist from the query -- each difference in
// float32, squared and summed in float64, in component order -- and for each
// of the eight lanes the smallest distance and the first row reaching it.
// With round each distance is first rounded to float32, as the scalar
// vec1BestMatchN compares them. The caller picks among the lanes.
//
// Args: A the first block, XC a block's size in bytes, V the query, N the
// block pairs (at least 1), XA the dimensions (at least 1), C a table of
// float64 0..7, four 8s and four +Inf, Out the lanes' minima (eight
// float64), Out2 their rows (eight float64).
func EmitVec1BestD(round bool) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RBX, RDI, OffXC)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.MovRegMem(RDX, RDI, OffC)
	// Y9/Y10 the lanes' minima, Y11/Y12 their rows, Y13/Y14 the rows of the
	// block at hand, Y15 the step between blocks.
	a.VmovdquLoadD(9, RDX, 96)
	a.VmovdquLoadD(10, RDX, 96)
	a.VpxorYmm(11, 11, 11)
	a.VpxorYmm(12, 12, 12)
	a.VmovdquLoad(13, RDX)
	a.VmovdquLoadD(14, RDX, 32)
	a.VmovdquLoadD(15, RDX, 64)

	a.Label("pair")
	for _, r := range []Reg{0, 1, 2, 3} {
		a.VxorpsYmm(r, r, r)
	}
	a.MovRegReg(RDX, RSI)
	a.AddRegReg(RDX, RBX)
	a.MovRegMem(R8, RDI, OffV)
	a.MovRegReg(R12, R9)

	a.Label("comp")
	a.VmovdquLoad(4, RSI)
	a.VmovdquLoad(5, RDX)
	a.VbroadcastssMem(6, R8)
	a.VsubpsYmm(4, 6, 4)
	a.VsubpsYmm(5, 6, 5)
	a.Vextracti128(7, 4, 1)
	a.Vextracti128(8, 5, 1)
	a.Vcvtps2pdYmm(4, 4)
	a.Vcvtps2pdYmm(7, 7)
	a.Vcvtps2pdYmm(5, 5)
	a.Vcvtps2pdYmm(8, 8)
	a.sqAccPd(0, 4)
	a.sqAccPd(1, 7)
	a.sqAccPd(2, 5)
	a.sqAccPd(3, 8)
	a.AddRegImm8(RSI, 32)
	a.AddRegImm8(RDX, 32)
	a.AddRegImm8(R8, 4)
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	for _, half := range [][2]Reg{{0, 1}, {2, 3}} {
		for i, acc := range half {
			if round {
				a.Vcvtpd2psYmm(7, acc)
				a.Vcvtps2pdYmm(acc, 7)
			}
			best, row, cur := 9+Reg(i), 11+Reg(i), 13+Reg(i)
			a.VcmpltpdYmm(7, acc, best)
			a.VblendvpdYmm(best, best, acc, 7)
			a.VblendvpdYmm(row, row, cur, 7)
		}
		a.VaddpdYmm(13, 13, 15)
		a.VaddpdYmm(14, 14, 15)
	}
	a.MovRegReg(RSI, RDX)
	a.DecReg(RCX)
	a.Jcc(CondNE, "pair")

	a.MovRegMem(R10, RDI, OffOut)
	a.MovRegMem(R11, RDI, OffOut2)
	a.VmovupsStore(R10, 0, 9)
	a.VmovupsStore(R10, 32, 10)
	a.VmovupsStore(R11, 0, 11)
	a.VmovupsStore(R11, 32, 12)
	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// Vcvtpd2psYmm narrows ymm src's four float64 to xmm dst's four float32.
func (a *Asm) Vcvtpd2psYmm(dst, src Reg) {
	a.vinst(0x5A, dst, 0, src, vexMap0F, 0, 1, vexP66, true)
}

// VcmpltpdYmm is  dst = src1 < src2, four float64 lanes, all-ones where true.
func (a *Asm) VcmpltpdYmm(dst, src1, src2 Reg) {
	a.vinst(0xC2, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
	a.emit(0x01)
}

// VblendvpdYmm is  dst = mask's sign bit ? src2 : src1, four float64 lanes.
func (a *Asm) VblendvpdYmm(dst, src1, src2, mask Reg) {
	a.vinst(0x4B, dst, src1, src2, vexMap0F3A, 0, 1, vexP66, false)
	a.emit(byte(mask) << 4)
}

// EmitVec1Cand emits the first half of a nearest-row search settled
// exactly: four vectors against row sets laid out T8 (each set B blocks of
// eight rows, the sets back to back -- a PQ encode's codebooks, or one set
// of centroids), each vector's part for a set the next XA components, every
// row's squared distance estimated in float32 less the vector's own squared
// norm, ||x||^2 - 2 x.q (one multiply-add a component), and for each vector
// and set the rows whose estimate is within a bound of the smallest:
// estimates no larger than (lo + A) * F, lo the smallest and A (per vector)
// and F the caller's error terms, the bound computed in float32 (the caller
// allows for its rounding). The caller computes those candidates' exact
// distances. Each block of eight rows is followed by their squared norms, as
// one more component; fused multiply-adds are used where the machine has
// them.
//
// Args: A the first set, N the sets (at least 1), XA a vector's components
// per set (at least 1), V the first vector, XC the vectors' stride in bytes,
// C scratch: B as an int64, eight float32 F at byte 32, eight float32 -2 at
// byte 64, the four vectors' A (eight float32 each) from byte 128, then from
// byte 256 128*B bytes the kernel uses; Out per set the four vectors' lo (float32) and
// then their candidate bit sets (B bytes each, a bit per row), 16 + 4*B
// bytes.
func EmitVec1Cand() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(R9, RDI, OffN)
	a.MovRegMem(RDX, RDI, OffXC) // the vectors' stride: vector k at RAX + k*RDX
	a.MovRegMem(R10, RDI, OffOut)
	a.MovRegMem(R15, RDI, OffV)
	a.MovRegMem(RAX, RDI, OffC)
	a.VmovdquLoadD(12, RAX, 64)  // -2
	a.MovRegMem(R11, RDI, OffXA) // a block's bytes: (XA+1) * 32
	a.IncReg(R11)
	for range 5 {
		a.AddRegReg(R11, R11)
	}

	a.Label("set")
	a.MovRegReg(RBX, RDX) // three strides
	a.AddRegReg(RBX, RDX)
	a.AddRegReg(RBX, RDX)
	// The minima start at +Inf: all-ones shifted to 0xFF << 23.
	for vv := Reg(0); vv < 4; vv++ {
		a.VpcmpeqdYmm(8+vv, 8+vv, 8+vv)
		a.VpsrldYmm(8+vv, 8+vv, 24)
		a.VpslldYmm(8+vv, 8+vv, 23)
	}
	a.MovRegMem(R13, RDI, OffC)
	a.MovRegImm64(RCX, 256)
	a.AddRegReg(R13, RCX) // the estimates
	a.MovRegMem(RCX, RDI, OffC)
	a.MovRegMem(RCX, RCX, 0) // B, then the pairs of blocks
	a.ShrRegImm8(RCX, 1)
	a.MovRegReg(R8, RSI)
	a.AddRegReg(R8, R11) // the pair's second block

	// Two blocks an iteration, eight chains of multiply-adds: sums Y0..Y3
	// (first block) and Y4..Y7 (second), components Y13 and Y14, the
	// vector's component Y15.
	a.Label("pair")
	for r := Reg(0); r < 8; r++ {
		a.VxorpsYmm(r, r, r)
	}
	a.MovRegReg(RAX, R15)
	a.MovRegMem(R12, RDI, OffXA)

	a.Label("comp")
	a.VmovdquLoad(13, RSI)
	a.VmovdquLoadD(14, R8, 0)
	for vv := Reg(0); vv < 4; vv++ {
		switch vv {
		case 0:
			a.VbroadcastssMem(15, RAX)
		case 1:
			a.vbroadcastssSIB(15, RAX, RDX, 0)
		case 2:
			a.vbroadcastssSIB(15, RAX, RDX, 1)
		case 3:
			a.vbroadcastssSIB(15, RAX, RBX, 0)
		}
		a.fmaPs(vv, 13, 15)
		a.fmaPs(4+vv, 14, 15)
	}
	a.AddRegImm8(RSI, 32)
	a.AddRegImm8(R8, 32)
	a.AddRegImm8(RAX, 4)
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	a.VmovdquLoad(13, RSI) // the rows' squared norms
	a.VmovdquLoadD(14, R8, 0)
	for vv := Reg(0); vv < 4; vv++ {
		a.VmulpsYmm(vv, vv, 12) // exact: a power of two
		a.VaddpsYmm(vv, vv, 13)
		a.VmulpsYmm(4+vv, 4+vv, 12)
		a.VaddpsYmm(4+vv, 4+vv, 14)
	}
	for blk := Reg(0); blk < 2; blk++ {
		for vv := Reg(0); vv < 4; vv++ {
			e := 4*blk + vv
			a.VmovupsStore(R13, int8(32*vv), e)
			a.VminpsYmm(8+vv, e, 8+vv) // a NaN estimate leaves the minimum be
		}
		a.AddRegImm8(R13, 127)
		a.AddRegImm8(R13, 1)
	}
	a.AddRegImm8(R8, 32) // past the second block's norms: the next pair
	a.MovRegReg(RSI, R8)
	a.AddRegReg(R8, R11)
	a.DecReg(RCX)
	a.Jcc(CondNE, "pair")

	// Each minimum across its lanes, stored as lo, then the bound.
	a.MovRegMem(RAX, RDI, OffC)
	a.VmovdquLoadD(14, RAX, 32) // F
	a.AddRegImm8(RAX, 127)
	a.AddRegImm8(RAX, 1) // the vectors' A
	for vv := Reg(0); vv < 4; vv++ {
		m := 8 + vv
		a.Vperm2f128(13, m, m, 0x01)
		a.VminpsYmm(m, 13, m)
		a.VpermilpsYmm(13, m, 0x4E)
		a.VminpsYmm(m, 13, m)
		a.VpermilpsYmm(13, m, 0xB1)
		a.VminpsYmm(m, 13, m)
		a.VmovssStore(R10, int8(4*vv), m)
		a.VmovdquLoadD(13, RAX, int8(32*vv))
		a.VaddpsYmm(m, m, 13)
		a.VmulpsYmm(m, m, 14)
	}

	// The bit sets, a vector at a time.
	a.MovRegReg(RBX, R10)
	a.AddRegImm8(RBX, 16)
	for vv := Reg(0); vv < 4; vv++ {
		a.MovRegMem(R13, RDI, OffC)
		a.MovRegMem(RCX, R13, 0) // B
		a.MovRegImm64(RAX, int64(256+32*int(vv)))
		a.AddRegReg(R13, RAX) // this vector's estimates
		a.Label(fmt.Sprintf("mask%d", vv))
		a.VmovdquLoadD(13, R13, 0) // mod 00 with R13 would be RIP-relative
		a.VcmplepsYmm(13, 13, 8+vv)
		a.VmovmskpsYmm(RAX, 13)
		a.MovMemByteAL(RBX, 0)
		a.AddRegImm8(R13, 127)
		a.AddRegImm8(R13, 1)
		a.AddRegImm8(RBX, 1)
		a.DecReg(RCX)
		a.Jcc(CondNE, fmt.Sprintf("mask%d", vv))
	}
	a.MovRegReg(R10, RBX) // the next set's record

	a.MovRegMem(RAX, RDI, OffXA)
	a.AddRegReg(R15, RAX) // the next components
	a.AddRegReg(R15, RAX)
	a.AddRegReg(R15, RAX)
	a.AddRegReg(R15, RAX)
	a.DecReg(R9)
	a.Jcc(CondNE, "set")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// vbroadcastssSIB is  dst = the float32 at [base + index<<scale], in all
// eight lanes. base must not be RBP or R13.
func (a *Asm) vbroadcastssSIB(dst, base, index Reg, scale byte) {
	a.vexm(dst, index, base, 0, vexMap0F38, 0, 1, vexP66)
	a.emit(0x18, modrm(0b00, dst, RSP), sib(scale, index, base))
}

// fmaPs is  acc += x*y  in float32: fused where the machine has it.
func (a *Asm) fmaPs(acc, x, y Reg) {
	if cpu.X86.HasFMA {
		a.vinst(0xB8, acc, x, y, vexMap0F38, 0, 1, vexP66, false) // VFMADD231PS
		return
	}
	a.VmulpsYmm(y, x, y)
	a.VaddpsYmm(acc, acc, y)
}

// VminpsYmm is  dst = src1 < src2 ? src1 : src2, lane-wise: src2 where
// either is NaN.
func (a *Asm) VminpsYmm(dst, src1, src2 Reg) {
	a.vinst(0x5D, dst, src1, src2, vexMap0F, 0, 1, 0, false)
}

// VpermilpsYmm permutes float32 lanes within each 128-bit half by imm.
func (a *Asm) VpermilpsYmm(dst, src Reg, imm byte) {
	a.vinst(0x04, dst, 0, src, vexMap0F3A, 0, 1, vexP66, true)
	a.emit(imm)
}

// VcmplepsYmm is  dst = src1 <= src2, lane-wise, all-ones where true.
func (a *Asm) VcmplepsYmm(dst, src1, src2 Reg) {
	a.vinst(0xC2, dst, src1, src2, vexMap0F, 0, 1, 0, false)
	a.emit(0x02)
}

// VmovmskpsYmm is  gpr dst = the eight lanes' sign bits.
func (a *Asm) VmovmskpsYmm(dst, src Reg) {
	a.vinst(0x50, dst, 0, src, vexMap0F, 0, 1, 0, true)
}

// MovMemByteAL is  byte [base+disp8] = AL. base must not be RSP or R12.
func (a *Asm) MovMemByteAL(base Reg, disp int8) {
	if base >= R8 {
		a.emit(0x41)
	}
	a.emit(0x88, modrm(0b01, RAX, base), byte(disp))
}

// VmovssStore is  float32 [base+disp8] = xmm src's low lane.
func (a *Asm) VmovssStore(base Reg, disp int8, src Reg) {
	a.vexm(src, 0, base, 0, vexMap0F, 0, 0, vexPF3)
	a.emit(0x11, modrm(0b01, src, base), byte(disp))
}

// VpsrldYmm is  dst = src >> imm, eight uint32 lanes.
func (a *Asm) VpsrldYmm(dst, src Reg, imm byte) {
	a.vex3(2, src, dst, vexMap0F, 0, 1, vexP66, false) // 72 /2
	a.emit(0x72, modrm(0b11, 2, src), imm)
}

// VpslldYmm is  dst = src << imm, eight uint32 lanes.
func (a *Asm) VpslldYmm(dst, src Reg, imm byte) {
	a.vex3(6, src, dst, vexMap0F, 0, 1, vexP66, false) // 72 /6
	a.emit(0x72, modrm(0b11, 6, src), imm)
}

// emitVec1T8F is EmitVec1T8's float32 sums, four blocks an iteration: four
// independent chains of adds, each row's still in component order.
func emitVec1T8F(op Vec1Op) ([]byte, error) {
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RBX, RDI, OffXC)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.MovRegMem(R10, RDI, OffOut)

	// Sums Y0..Y3, values Y4..Y7, Y8 the query component.
	blocks := []Reg{RSI, RDX, R11, R13}
	a.Label("quad")
	for r := Reg(0); r < 4; r++ {
		a.VxorpsYmm(r, r, r)
	}
	a.MovRegReg(RDX, RSI)
	a.AddRegReg(RDX, RBX)
	a.MovRegReg(R11, RDX)
	a.AddRegReg(R11, RBX)
	a.MovRegReg(R13, R11)
	a.AddRegReg(R13, RBX)
	a.MovRegMem(R8, RDI, OffV)
	a.MovRegReg(R12, R9)

	a.Label("comp")
	a.VbroadcastssMem(8, R8)
	for i, p := range blocks {
		v := 4 + Reg(i)
		a.VmovdquLoadD(v, p, 0) // disp8: mod 00 with R13 would be RIP-relative
		if op == Vec1DotF {
			a.VmulpsYmm(v, v, 8)
		} else {
			a.VsubpsYmm(v, 8, v)
			a.VmulpsYmm(v, v, v)
		}
		a.VaddpsYmm(Reg(i), Reg(i), v)
	}
	for _, p := range blocks {
		a.AddRegImm8(p, 32)
	}
	a.AddRegImm8(R8, 4)
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	for i := Reg(0); i < 4; i++ {
		a.VmovupsStore(R10, int8(32*i), i)
	}
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1)
	a.MovRegReg(RSI, R13) // past the fourth block
	a.DecReg(RCX)
	a.Jcc(CondNE, "quad")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// EmitVec1WHT emits vec1's Walsh-Hadamard transform of a power-of-two run
// of float32s, in place: stages of butterflies (a, b) -> (a+b, a-b) at
// strides 1, 2, 4, ... in turn, each element's operations those of the
// scalar loop, eight at a time. The strides under eight are done within a
// register (a permute brings each element's partner alongside, a blend
// keeps the sum or the difference), the rest between registers.
//
// Args: A the run, N its length (a power of two, at least 16).
func EmitVec1WHT() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RAX, RDI, OffN)
	a.AddRegReg(RAX, RAX)
	a.AddRegReg(RAX, RAX)
	a.AddRegReg(RAX, RSI) // the end

	// Strides 1, 2 and 4, register by register.
	a.MovRegReg(R10, RSI)
	a.Label("small")
	a.VmovdquLoad(0, R10)
	for _, st := range []struct {
		perm  func(dst, src Reg)
		blend byte
	}{
		{func(d, s Reg) { a.VpermilpsYmm(d, s, 0xB1) }, 0xAA},
		{func(d, s Reg) { a.VpermilpsYmm(d, s, 0x4E) }, 0xCC},
		{func(d, s Reg) { a.Vperm2f128(d, s, s, 0x01) }, 0xF0},
	} {
		st.perm(1, 0)        // each element's partner
		a.VaddpsYmm(2, 0, 1) // a+b where the element is the pair's first
		a.VsubpsYmm(3, 1, 0) // a-b where it is the second
		a.VblendpsYmm(0, 2, 3, st.blend)
	}
	a.VmovupsStore(R10, 0, 0)
	a.AddRegImm8(R10, 32)
	a.CmpRegReg(R10, RAX)
	a.Jcc(CondL, "small")

	// Strides of eight and more: RDX the stride in bytes.
	a.MovRegImm64(RDX, 32)
	a.Label("stage")
	a.MovRegReg(R10, RSI) // the block of 2 strides
	a.Label("block")
	a.MovRegReg(R11, R10)
	a.MovRegReg(R12, R10)
	a.AddRegReg(R12, RDX) // the block's second half
	a.MovRegReg(R8, R12)
	a.Label("bfly")
	a.VmovdquLoad(0, R11)
	a.VmovdquLoadD(1, R8, 0)
	a.VaddpsYmm(2, 0, 1)
	a.VsubpsYmm(3, 0, 1)
	a.VmovupsStore(R11, 0, 2)
	a.VmovupsStore(R8, 0, 3)
	a.AddRegImm8(R11, 32)
	a.AddRegImm8(R8, 32)
	a.CmpRegReg(R11, R12)
	a.Jcc(CondL, "bfly")
	a.MovRegReg(R10, R8) // the next block
	a.CmpRegReg(R10, RAX)
	a.Jcc(CondL, "block")
	a.AddRegReg(RDX, RDX)
	a.MovRegReg(R9, RSI)
	a.AddRegReg(R9, RDX)
	a.CmpRegReg(R9, RAX) // a stride of the whole run is past the last stage
	a.Jcc(CondL, "stage")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// VblendpsYmm is  dst lane i = imm bit i ? src2 : src1.
func (a *Asm) VblendpsYmm(dst, src1, src2 Reg, imm byte) {
	a.vinst(0x0C, dst, src1, src2, vexMap0F3A, 0, 1, vexP66, false)
	a.emit(imm)
}

// EmitVec1CandMany is EmitVec1Cand for one set of rows and many vectors: G
// groups of four, every group's estimates computed a pair of blocks at a
// time with the pair outermost, so a pair of blocks (two blocks of rows, a
// few KiB) is read from memory once for all the groups rather than once for
// each. A set of rows larger than the caches (the centroids of a big index)
// is otherwise streamed through them once for every four vectors.
//
// Args: A the set (B blocks, B even), N the groups G (at least 1), XA the
// dimensions, V the first vector, the vectors back to back at XC bytes each,
// C scratch: B as an int64, eight float32 F at byte 32, eight float32 -2 at
// byte 64; from byte 128 the groups' running minima (128 bytes each), then
// their A (eight float32 for each of four vectors, 128 bytes a group), then
// the estimates, 256 bytes per pair of blocks and group; Out per group the
// four vectors' lo and bit sets, as EmitVec1Cand writes them.
func EmitVec1CandMany() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RAX, RDI, OffC)
	a.VmovdquLoadD(12, RAX, 64) // -2
	a.MovRegMem(RDX, RDI, OffXC)
	a.MovRegMem(R11, RDI, OffXA) // a block's bytes: (XA+1) * 32
	a.IncReg(R11)
	for range 5 {
		a.AddRegReg(R11, R11)
	}
	a.MovRegReg(RBX, RDX) // three strides
	a.AddRegReg(RBX, RDX)
	a.AddRegReg(RBX, RDX)

	// Every group's minima start at +Inf.
	a.VpcmpeqdYmm(8, 8, 8)
	a.VpsrldYmm(8, 8, 24)
	a.VpslldYmm(8, 8, 23)
	a.MovRegMem(R10, RDI, OffC)
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1)
	a.MovRegMem(R9, RDI, OffN)
	a.Label("init")
	for vv := Reg(0); vv < 4; vv++ {
		a.VmovupsStore(R10, int8(32*vv), 8)
	}
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1)
	a.DecReg(R9)
	a.Jcc(CondNE, "init")
	// R10 is past the minima, at the A; the estimates follow those.
	a.MovRegMem(R13, RDI, OffN)
	for range 7 {
		a.AddRegReg(R13, R13)
	}
	a.AddRegReg(R13, R10)

	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegReg(R8, RSI)
	a.AddRegReg(R8, R11)
	a.MovRegMem(RCX, RDI, OffC)
	a.MovRegMem(RCX, RCX, 0) // B, then the pairs of blocks
	a.ShrRegImm8(RCX, 1)

	a.Label("pair")
	a.MovRegMem(R15, RDI, OffV)
	a.MovRegMem(R10, RDI, OffC)
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1) // group 0's minima
	a.MovRegMem(R9, RDI, OffN)

	a.Label("group")
	for r := Reg(0); r < 8; r++ {
		a.VxorpsYmm(r, r, r)
	}
	a.MovRegReg(RAX, R15)
	a.MovRegMem(R12, RDI, OffXA)
	a.Label("comp")
	a.VmovdquLoad(13, RSI)
	a.VmovdquLoadD(14, R8, 0)
	for vv := Reg(0); vv < 4; vv++ {
		switch vv {
		case 0:
			a.VbroadcastssMem(15, RAX)
		case 1:
			a.vbroadcastssSIB(15, RAX, RDX, 0)
		case 2:
			a.vbroadcastssSIB(15, RAX, RDX, 1)
		case 3:
			a.vbroadcastssSIB(15, RAX, RBX, 0)
		}
		a.fmaPs(vv, 13, 15)
		a.fmaPs(4+vv, 14, 15)
	}
	a.AddRegImm8(RSI, 32)
	a.AddRegImm8(R8, 32)
	a.AddRegImm8(RAX, 4)
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	a.VmovdquLoad(13, RSI) // the rows' squared norms
	a.VmovdquLoadD(14, R8, 0)
	for _, p := range []Reg{RSI, R8} { // back to the blocks' first component
		a.SubRegReg(p, R11)
		a.AddRegImm8(p, 32)
	}
	for vv := Reg(0); vv < 4; vv++ {
		a.VmulpsYmm(vv, vv, 12) // exact: a power of two
		a.VaddpsYmm(vv, vv, 13)
		a.VmulpsYmm(4+vv, 4+vv, 12)
		a.VaddpsYmm(4+vv, 4+vv, 14)
	}
	for blk := Reg(0); blk < 2; blk++ {
		for vv := Reg(0); vv < 4; vv++ {
			a.VmovupsStore(R13, int8(32*vv), 4*blk+vv)
		}
		a.AddRegImm8(R13, 127)
		a.AddRegImm8(R13, 1)
	}
	for vv := Reg(0); vv < 4; vv++ {
		a.VmovdquLoadD(8+vv, R10, int8(32*vv))
		a.VminpsYmm(8+vv, vv, 8+vv) // a NaN estimate leaves the minimum be
		a.VminpsYmm(8+vv, 4+vv, 8+vv)
		a.VmovupsStore(R10, int8(32*vv), 8+vv)
	}
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1)
	a.AddRegReg(R15, RBX) // the next group's vectors
	a.AddRegReg(R15, RDX)
	a.DecReg(R9)
	a.Jcc(CondNE, "group")

	a.AddRegReg(RSI, R11) // the next pair of blocks
	a.AddRegReg(RSI, R11)
	a.MovRegReg(R8, RSI)
	a.AddRegReg(R8, R11)
	a.DecReg(RCX)
	a.Jcc(CondNE, "pair")

	// Per group: each minimum across its lanes, stored as lo, the bound, and
	// the bit sets. R10 the group's minima, R15 its A, R13 its estimates in
	// the first pair, RDX the stride from pair to pair (G * 256), RBX out.
	a.MovRegMem(R10, RDI, OffC)
	a.MovRegReg(RAX, R10)
	a.VmovdquLoadD(14, RAX, 32) // F
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1)
	a.MovRegMem(RDX, RDI, OffN)
	for range 7 {
		a.AddRegReg(RDX, RDX)
	}
	a.MovRegReg(R15, R10)
	a.AddRegReg(R15, RDX) // the A
	a.MovRegReg(R13, R15)
	a.AddRegReg(R13, RDX) // the estimates
	a.AddRegReg(RDX, RDX)
	a.MovRegMem(RBX, RDI, OffOut)
	a.MovRegMem(R9, RDI, OffN)

	a.Label("final")
	for vv := Reg(0); vv < 4; vv++ {
		m := 8 + vv
		a.VmovdquLoadD(m, R10, int8(32*vv))
		a.Vperm2f128(13, m, m, 0x01)
		a.VminpsYmm(m, 13, m)
		a.VpermilpsYmm(13, m, 0x4E)
		a.VminpsYmm(m, 13, m)
		a.VpermilpsYmm(13, m, 0xB1)
		a.VminpsYmm(m, 13, m)
		a.VmovssStore(RBX, int8(4*vv), m)
		a.VmovdquLoadD(13, R15, int8(32*vv))
		a.VaddpsYmm(m, m, 13)
		a.VmulpsYmm(m, m, 14)
	}
	a.AddRegImm8(RBX, 16)
	for vv := Reg(0); vv < 4; vv++ {
		a.MovRegReg(R11, R13) // at the pair's second block: both in disp8 reach
		a.AddRegImm8(R11, int8(32*vv))
		a.AddRegImm8(R11, 127)
		a.AddRegImm8(R11, 1)
		a.MovRegMem(RCX, RDI, OffC)
		a.MovRegMem(RCX, RCX, 0)
		a.ShrRegImm8(RCX, 1)
		a.Label(fmt.Sprintf("masks%d", vv))
		for blk := 0; blk < 2; blk++ {
			a.VmovdquLoadD(13, R11, int8(128*blk-128))
			a.VcmplepsYmm(13, 13, 8+vv)
			a.VmovmskpsYmm(RAX, 13)
			a.MovMemByteAL(RBX, int8(blk))
		}
		a.AddRegImm8(RBX, 2)
		a.AddRegReg(R11, RDX)
		a.DecReg(RCX)
		a.Jcc(CondNE, fmt.Sprintf("masks%d", vv))
	}
	a.AddRegImm8(R10, 127) // the next group
	a.AddRegImm8(R10, 1)
	a.AddRegImm8(R15, 127)
	a.AddRegImm8(R15, 1)
	a.MovRegImm64(RAX, 256)
	a.AddRegReg(R13, RAX)
	a.DecReg(R9)
	a.Jcc(CondNE, "final")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}
