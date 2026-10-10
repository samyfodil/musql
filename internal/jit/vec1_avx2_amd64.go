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
// Two blocks go per iteration, to cover the adds' latency.
//
// Args: A the first block, XC a block's size in bytes (32 x dims), V the
// query's float32 components, N the block PAIRS (at least 1), XA the
// dimensions (at least 1), Out the sums: a float32 per row, or for Vec1L2D a
// float64.
func EmitVec1T8(op Vec1Op) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
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

// EmitVec1Best4 emits the nearest-row search vec1's training runs: vectors
// four at a time against rows laid out T8 (as EmitVec1T8 takes them), each
// distance the float32 sum of (x-q)^2, and for each vector and lane the
// smallest distance and the first row reaching it. A row is loaded once for
// the four vectors. Training is randomized, and vec1's own builds train with
// different arithmetic (its AVX2 build fuses the multiply-add, as this does
// where the machine can), so this is the scalar build's search in float32
// rather than to the bit.
//
// Args: A the first block, N the blocks (at least 1), XA the dimensions (at
// least 1), V the first vector, the vectors back to back, XC the groups of
// four (at least 1), C a table of int32 0..7, eight 8s and eight float32
// +Inf, Out per group the four vectors' eight lane minima (float32) then
// their rows (int32), 256 bytes.
func EmitVec1Best4() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(R9, RDI, OffXA)
	a.MovRegMem(R10, RDI, OffOut)
	a.MovRegMem(R13, RDI, OffXC)
	a.MovRegMem(R15, RDI, OffV)
	a.MovRegReg(RDX, R9) // a vector's bytes
	a.AddRegReg(RDX, RDX)
	a.AddRegReg(RDX, RDX)

	a.Label("group")
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(RAX, RDI, OffC)
	a.VmovdquLoad(14, RAX)      // the block's row numbers
	a.VmovdquLoadD(15, RAX, 32) // the step between blocks
	for vv := Reg(0); vv < 4; vv++ {
		a.VmovdquLoadD(6+vv, RAX, 64) // minima
		a.VpxorYmm(10+vv, 10+vv, 10+vv)
	}

	a.Label("blk")
	for vv := Reg(0); vv < 4; vv++ {
		a.VxorpsYmm(vv, vv, vv)
	}
	a.MovRegReg(RAX, R15)
	a.MovRegReg(RBX, RAX)
	a.AddRegReg(RBX, RDX)
	a.MovRegReg(R8, RBX)
	a.AddRegReg(R8, RDX)
	a.MovRegReg(R11, R8)
	a.AddRegReg(R11, RDX)
	a.MovRegReg(R12, R9)

	a.Label("comp")
	a.VmovdquLoad(4, RSI)
	for vv, p := range []Reg{RAX, RBX, R8, R11} {
		a.VbroadcastssMem(5, p)
		a.VsubpsYmm(5, 4, 5)
		a.fmaSqPs(Reg(vv), 5)
	}
	a.AddRegImm8(RSI, 32)
	for _, p := range []Reg{RAX, RBX, R8, R11} {
		a.AddRegImm8(p, 4)
	}
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	for vv := Reg(0); vv < 4; vv++ {
		a.VcmpltpsYmm(5, vv, 6+vv)
		a.VblendvpsYmm(6+vv, 6+vv, vv, 5)
		a.VblendvpsYmm(10+vv, 10+vv, 14, 5)
	}
	a.VpadddYmm(14, 14, 15)
	a.DecReg(RCX)
	a.Jcc(CondNE, "blk")

	for vv := Reg(0); vv < 4; vv++ {
		a.VmovupsStore(R10, int8(32*vv), 6+vv)
	}
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1)
	for vv := Reg(0); vv < 4; vv++ {
		a.VmovupsStore(R10, int8(32*vv), 10+vv)
	}
	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 1)
	a.MovRegReg(R15, R11) // the next group: past the fourth vector
	a.DecReg(R13)
	a.Jcc(CondNE, "group")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// fmaSqPs is  acc += x*x  in float32, x clobbered: fused where the machine
// has it.
func (a *Asm) fmaSqPs(acc, x Reg) {
	if cpu.X86.HasFMA {
		a.vinst(0xB8, acc, x, x, vexMap0F38, 0, 1, vexP66, false) // VFMADD231PS
		return
	}
	a.VmulpsYmm(x, x, x)
	a.VaddpsYmm(acc, acc, x)
}

// VcmpltpsYmm is  dst = src1 < src2, lane-wise, all-ones where true.
func (a *Asm) VcmpltpsYmm(dst, src1, src2 Reg) {
	a.vinst(0xC2, dst, src1, src2, vexMap0F, 0, 1, 0, false)
	a.emit(0x01)
}

// VblendvpsYmm is  dst = mask's sign bit ? src2 : src1, lane-wise.
func (a *Asm) VblendvpsYmm(dst, src1, src2, mask Reg) {
	a.vinst(0x4A, dst, src1, src2, vexMap0F3A, 0, 1, vexP66, false)
	a.emit(byte(mask) << 4)
}

// VpadddYmm is  dst = src1 + src2, eight int32 lanes.
func (a *Asm) VpadddYmm(dst, src1, src2 Reg) {
	a.vinst(0xFE, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
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

// EmitVec1T8x4 emits an estimate of four vectors' squared distances to every
// row of a T8 layout, for a search that then settles its answer exactly:
// float32 sums of (x-q)^2, fused multiply-adds where the machine has them.
// Each row is loaded once for the four vectors.
//
// Args: A the first block, N the blocks (at least 1), XA the dimensions (at
// least 1), V the first vector, XC the vectors' stride in bytes, Out per
// block the four vectors' eight sums in turn (32 float32).
func EmitVec1T8x4() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.MovRegMem(R10, RDI, OffOut)

	a.Label("blk")
	for vv := Reg(0); vv < 4; vv++ {
		a.VxorpsYmm(vv, vv, vv)
	}
	a.MovRegMem(RAX, RDI, OffV)
	a.MovRegMem(RDX, RDI, OffXC)
	a.MovRegReg(RBX, RAX)
	a.AddRegReg(RBX, RDX)
	a.MovRegReg(R8, RBX)
	a.AddRegReg(R8, RDX)
	a.MovRegReg(R11, R8)
	a.AddRegReg(R11, RDX)
	a.MovRegReg(R12, R9)

	a.Label("comp")
	a.VmovdquLoad(4, RSI)
	for vv, p := range []Reg{RAX, RBX, R8, R11} {
		t := 5 + Reg(vv)
		a.VbroadcastssMem(t, p)
		a.VsubpsYmm(t, 4, t)
		if cpu.X86.HasFMA {
			a.vinst(0xB8, Reg(vv), t, t, vexMap0F38, 0, 1, vexP66, false) // VFMADD231PS
		} else {
			a.VmulpsYmm(t, t, t)
			a.VaddpsYmm(Reg(vv), Reg(vv), t)
		}
	}
	a.AddRegImm8(RSI, 32)
	for _, p := range []Reg{RAX, RBX, R8, R11} {
		a.AddRegImm8(p, 4)
	}
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	for vv := Reg(0); vv < 4; vv++ {
		a.VmovupsStore(R10, int8(32*vv), vv)
	}
	a.AddRegImm8(R10, 64)
	a.AddRegImm8(R10, 64)
	a.DecReg(RCX)
	a.Jcc(CondNE, "blk")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// EmitVec1PQCand emits the first half of a PQ encode settled exactly: four
// vectors' sub-vectors against every codebook (laid out T8, 256 rows each,
// the codebooks back to back), the squared distances estimated in float32
// (as EmitVec1T8x4 does), and for each vector and codebook the rows whose
// estimate is within a bound of the smallest: estimates no larger than
// (lo + A) * F, lo the smallest and A and F the caller's error terms, the
// bound computed in float32 (the caller allows for its rounding). The caller computes those candidates' exact distances.
//
// Args: A the first codebook, N the codebooks (at least 1), XA a sub-vector's
// length (at least 1), V the first vector, XC the vectors' stride in bytes,
// C scratch: eight float32 A, eight F, then 4 KiB the kernel uses, Out per
// codebook the four vectors' candidate bit sets (32 bytes each, a bit per
// row) and then their lo (float32), 144 bytes.
func EmitVec1PQCand() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(R9, RDI, OffN)
	a.MovRegMem(RDX, RDI, OffXC)
	a.MovRegMem(R10, RDI, OffOut)
	a.MovRegMem(R15, RDI, OffV)

	a.Label("book")
	// The minima start at +Inf: all-ones shifted to 0xFF << 23.
	for vv := Reg(0); vv < 4; vv++ {
		a.VpcmpeqdYmm(8+vv, 8+vv, 8+vv)
		a.VpsrldYmm(8+vv, 8+vv, 24)
		a.VpslldYmm(8+vv, 8+vv, 23) // 0xFF << 23 = +Inf
	}
	a.MovRegMem(R13, RDI, OffC)
	a.AddRegImm8(R13, 64) // the estimates
	a.MovRegImm64(RCX, 32)

	a.Label("blk")
	for vv := Reg(0); vv < 4; vv++ {
		a.VxorpsYmm(vv, vv, vv)
	}
	a.MovRegReg(RAX, R15)
	a.MovRegReg(RBX, RAX)
	a.AddRegReg(RBX, RDX)
	a.MovRegReg(R8, RBX)
	a.AddRegReg(R8, RDX)
	a.MovRegReg(R11, R8)
	a.AddRegReg(R11, RDX)
	a.MovRegMem(R12, RDI, OffXA)

	a.Label("comp")
	a.VmovdquLoad(4, RSI)
	for vv, p := range []Reg{RAX, RBX, R8, R11} {
		a.VbroadcastssMem(5, p)
		a.VsubpsYmm(5, 4, 5)
		a.fmaSqPs(Reg(vv), 5)
	}
	a.AddRegImm8(RSI, 32)
	for _, p := range []Reg{RAX, RBX, R8, R11} {
		a.AddRegImm8(p, 4)
	}
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	for vv := Reg(0); vv < 4; vv++ {
		a.VmovupsStore(R13, int8(32*vv), vv)
		a.VminpsYmm(8+vv, vv, 8+vv) // a NaN estimate leaves the minimum be
	}
	a.AddRegImm8(R13, 127)
	a.AddRegImm8(R13, 1)
	a.DecReg(RCX)
	a.Jcc(CondNE, "blk")

	// Each minimum across its lanes, then the bound; lo stored for the caller.
	a.MovRegMem(RAX, RDI, OffC)
	a.MovRegReg(R11, R10) // where lo goes, past the bit sets
	a.AddRegImm8(R11, 127)
	a.AddRegImm8(R11, 1)
	for vv := Reg(0); vv < 4; vv++ {
		m := 8 + vv
		a.Vperm2f128(5, m, m, 0x01)
		a.VminpsYmm(m, 5, m)
		a.VpermilpsYmm(5, m, 0x4E)
		a.VminpsYmm(m, 5, m)
		a.VpermilpsYmm(5, m, 0xB1)
		a.VminpsYmm(m, 5, m)
		a.VmovssStore(R11, int8(4*vv), m)
		a.VmovdquLoad(5, RAX) // A
		a.VaddpsYmm(m, m, 5)
		a.VmovdquLoadD(6, RAX, 32) // F
		a.VmulpsYmm(m, m, 6)
	}

	a.MovRegMem(R13, RDI, OffC)
	a.AddRegImm8(R13, 64)
	a.MovRegReg(RBX, R10)
	a.MovRegImm64(RCX, 32)
	a.Label("mask")
	for vv := Reg(0); vv < 4; vv++ {
		a.VmovdquLoadD(5, R13, int8(32*vv))
		a.VcmplepsYmm(5, 5, 8+vv)
		a.VmovmskpsYmm(RAX, 5)
		a.MovMemByteAL(RBX, int8(32*vv))
	}
	a.AddRegImm8(R13, 127)
	a.AddRegImm8(R13, 1)
	a.AddRegImm8(RBX, 1)
	a.DecReg(RCX)
	a.Jcc(CondNE, "mask")

	a.AddRegImm8(R10, 127)
	a.AddRegImm8(R10, 17) // 144 bytes a codebook
	a.MovRegMem(RAX, RDI, OffXA)
	a.AddRegReg(R15, RAX) // the next sub-vectors
	a.AddRegReg(R15, RAX)
	a.AddRegReg(R15, RAX)
	a.AddRegReg(R15, RAX)
	a.DecReg(R9)
	a.Jcc(CondNE, "book")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
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
