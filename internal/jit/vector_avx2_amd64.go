//go:build amd64

package jit

import (
	"fmt"
	"math/bits"
)

// EmitVecDist emits the float32 vector-distance kernel: for each row, the sums
// libSQL's cosine (dot with the query, and the row's own sum of squares) or
// Euclidean (sum of squared differences) distance is computed from. The
// caller finishes each distance from them.
//
// A ymm register holds the same component of eight different ROWS, gathered
// from the heap by their offsets, so every row's sums are accumulated in
// component order with one rounding per product and per add (separate
// VMULPS and VADDPS, never a fused multiply-add) -- libSQL's float32 loop, to
// the bit, eight rows per instruction. Two tiles of eight are interleaved to
// cover the adds' latency.
//
// Args: A the heap, C the rows' int32 byte offsets into it, V the query's
// float32 components, N the rows (a multiple of 16), XA the dimensions (at
// least 1), Out a float32 per row (the dot product, or for L2 the sum), Out2
// a float32 per row (the row's sum of squares; unused for L2).
func EmitVecDist(l2 bool) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)   // heap
	a.MovRegMem(RDX, RDI, OffC)   // offsets
	a.MovRegMem(RCX, RDI, OffN)   // rows left
	a.MovRegMem(R9, RDI, OffXA)   // dims
	a.MovRegMem(R10, RDI, OffOut) // per-row sum
	a.MovRegMem(R11, RDI, OffOut2)

	// Tile one: index Y7, sums Y0 (and Y1). Tile two: index Y15, sums Y8 (and
	// Y9). Y2/Y10 take the gathered components, Y3 the query component, Y6/Y14
	// the gather masks, Y4/Y5/Y12/Y13 the products.
	a.Label("tile")
	a.VmovdquLoad(7, RDX)
	a.VmovdquLoadD(15, RDX, 32)
	a.VxorpsYmm(0, 0, 0)
	a.VxorpsYmm(8, 8, 8)
	if !l2 {
		a.VxorpsYmm(1, 1, 1)
		a.VxorpsYmm(9, 9, 9)
	}
	a.MovRegReg(RAX, RSI) // component i of every row: heap + 4i + offset
	a.MovRegMem(R8, RDI, OffV)
	a.MovRegReg(R12, R9)

	a.Label("comp")
	a.VpcmpeqdYmm(6, 6, 6) // all lanes: a gather clears its mask
	a.VpcmpeqdYmm(14, 14, 14)
	a.VgatherdpsYmm(2, RAX, 7, 6)
	a.VgatherdpsYmm(10, RAX, 15, 14)
	a.VbroadcastssMem(3, R8)
	if l2 {
		a.VsubpsYmm(4, 2, 3) // row - query
		a.VsubpsYmm(12, 10, 3)
		a.VmulpsYmm(4, 4, 4)
		a.VmulpsYmm(12, 12, 12)
		a.VaddpsYmm(0, 0, 4)
		a.VaddpsYmm(8, 8, 12)
	} else {
		a.VmulpsYmm(4, 2, 3) // row * query
		a.VmulpsYmm(12, 10, 3)
		a.VmulpsYmm(5, 2, 2) // row * row
		a.VmulpsYmm(13, 10, 10)
		a.VaddpsYmm(0, 0, 4)
		a.VaddpsYmm(8, 8, 12)
		a.VaddpsYmm(1, 1, 5)
		a.VaddpsYmm(9, 9, 13)
	}
	a.AddRegImm8(RAX, 4)
	a.AddRegImm8(R8, 4)
	a.DecReg(R12)
	a.Jcc(CondNE, "comp")

	a.VmovupsStore(R10, 0, 0)
	a.VmovupsStore(R10, 32, 8)
	if !l2 {
		a.VmovupsStore(R11, 0, 1)
		a.VmovupsStore(R11, 32, 9)
		a.AddRegImm8(R11, 64)
	}
	a.AddRegImm8(R10, 64)
	a.AddRegImm8(RDX, 64)
	a.AddRegImm8(RCX, -16)
	a.Jcc(CondNE, "tile")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// EmitVecDistStrided is EmitVecDist for rows laid out back to back at a fixed
// stride -- how a segment stores a column of equal-length vectors -- and a
// dimension count that is a multiple of 8. It reads each tile of eight rows
// with plain loads, eight components of each, and transposes the 8x8 block in
// registers so a lane again holds one row: every load is a sequential stream
// rather than a gather, and the sums are the same, in the same order.
//
// Args: A the first row, XC the stride in bytes, V the query, N the rows (a
// multiple of 8), XA the dimensions (a multiple of 8), Out and Out2 as for
// EmitVecDist.
func EmitVecDistStrided(l2 bool) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)  // the tile's first row
	a.MovRegMem(RBX, RDI, OffXC) // stride
	a.MovRegMem(RCX, RDI, OffN)  // rows left
	a.MovRegMem(R9, RDI, OffXA)
	a.ShrRegImm8(R9, 3) // blocks of eight components
	a.MovRegMem(R10, RDI, OffOut)
	a.MovRegMem(R11, RDI, OffOut2)
	a.LeaSIB(R13, RBX, RBX, 1) // 3 x stride: rows 0..3 are
	a.AddRegReg(R13, RBX)      // [p], [p+s], [p+2s], [p+3s]

	a.Label("tile")
	a.VxorpsYmm(14, 14, 14)
	if !l2 {
		a.VxorpsYmm(15, 15, 15)
	}
	a.MovRegReg(RAX, RSI)      // rows 0..3, at the block's component
	a.LeaSIB(RDX, RSI, RBX, 4) // rows 4..7
	a.MovRegMem(R8, RDI, OffV) // the block's query components
	a.MovRegReg(R12, R9)

	a.Label("block")
	for r, reg := range []Reg{0, 1, 2, 3} {
		a.vmovupsLoadRow(reg, RAX, r)
	}
	for r, reg := range []Reg{4, 5, 6, 7} {
		a.vmovupsLoadRow(reg, RDX, r)
	}
	// 8x8 transpose; afterwards component k of rows 0..7 is in col[k].
	a.VunpcklpsYmm(8, 0, 1)
	a.VunpckhpsYmm(0, 0, 1)
	a.VunpcklpsYmm(9, 2, 3)
	a.VunpckhpsYmm(2, 2, 3)
	a.VunpcklpsYmm(10, 4, 5)
	a.VunpckhpsYmm(4, 4, 5)
	a.VunpcklpsYmm(11, 6, 7)
	a.VunpckhpsYmm(6, 6, 7)
	a.VshufpsYmm(1, 8, 9, 0x44)
	a.VshufpsYmm(8, 8, 9, 0xEE)
	a.VshufpsYmm(3, 0, 2, 0x44)
	a.VshufpsYmm(0, 0, 2, 0xEE)
	a.VshufpsYmm(5, 10, 11, 0x44)
	a.VshufpsYmm(10, 10, 11, 0xEE)
	a.VshufpsYmm(7, 4, 6, 0x44)
	a.VshufpsYmm(4, 4, 6, 0xEE)
	a.Vperm2f128(2, 1, 5, 0x20)
	a.Vperm2f128(1, 1, 5, 0x31)
	a.Vperm2f128(6, 8, 10, 0x20)
	a.Vperm2f128(8, 8, 10, 0x31)
	a.Vperm2f128(9, 3, 7, 0x20)
	a.Vperm2f128(3, 3, 7, 0x31)
	a.Vperm2f128(11, 0, 4, 0x20)
	a.Vperm2f128(0, 0, 4, 0x31)
	col := [8]Reg{2, 6, 9, 11, 1, 8, 3, 0}
	for k, c := range col {
		a.VbroadcastssMemD(12, R8, int8(4*k))
		if l2 {
			a.VsubpsYmm(13, c, 12)
			a.VmulpsYmm(13, 13, 13)
			a.VaddpsYmm(14, 14, 13)
		} else {
			a.VmulpsYmm(13, c, 12)
			a.VaddpsYmm(14, 14, 13)
			a.VmulpsYmm(13, c, c)
			a.VaddpsYmm(15, 15, 13)
		}
	}
	a.AddRegImm8(RAX, 32)
	a.AddRegImm8(RDX, 32)
	a.AddRegImm8(R8, 32)
	a.DecReg(R12)
	a.Jcc(CondNE, "block")

	a.VmovupsStore(R10, 0, 14)
	a.AddRegImm8(R10, 32)
	if !l2 {
		a.VmovupsStore(R11, 0, 15)
		a.AddRegImm8(R11, 32)
	}
	a.LeaSIB(RSI, RSI, RBX, 8) // the next tile
	a.AddRegImm8(RCX, -8)
	a.Jcc(CondNE, "tile")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// vmovupsLoadRow loads row r (0..3) of the four at p: [p], [p+RBX],
// [p+2*RBX], [p+R13], RBX the stride and R13 three strides.
func (a *Asm) vmovupsLoadRow(dst, p Reg, r int) {
	switch r {
	case 0:
		a.vexm(dst, 0, p, 0, vexMap0F, 0, 1, 0)
		a.emit(0x10, modrm(0b00, dst, p))
	case 1, 2:
		a.vexm(dst, RBX, p, 0, vexMap0F, 0, 1, 0)
		a.emit(0x10, modrm(0b00, dst, RSP), byte(r-1)<<6|byte(RBX&7)<<3|byte(p&7))
	default:
		a.vexm(dst, R13, p, 0, vexMap0F, 0, 1, 0)
		a.emit(0x10, modrm(0b00, dst, RSP), byte(R13&7)<<3|byte(p&7))
	}
}

// LeaSIB is  dst = base + index*scale  (scale 1, 2, 4 or 8). base must not be
// RBP or R13.
func (a *Asm) LeaSIB(dst, base, index Reg, scale byte) {
	a.rexX(dst, index, base)
	a.emit(0x8D, modrm(0b00, dst, RSP), sib(byte(bits.TrailingZeros8(scale)), index, base))
}

// VunpcklpsYmm interleaves the low float32 pairs of each 128-bit half.
func (a *Asm) VunpcklpsYmm(dst, src1, src2 Reg) {
	a.vinst(0x14, dst, src1, src2, vexMap0F, 0, 1, 0, false)
}

// VunpckhpsYmm interleaves the high float32 pairs of each 128-bit half.
func (a *Asm) VunpckhpsYmm(dst, src1, src2 Reg) {
	a.vinst(0x15, dst, src1, src2, vexMap0F, 0, 1, 0, false)
}

// VshufpsYmm selects float32 lanes from src1 and src2 by imm, per half.
func (a *Asm) VshufpsYmm(dst, src1, src2 Reg, imm byte) {
	a.vinst(0xC6, dst, src1, src2, vexMap0F, 0, 1, 0, false)
	a.emit(imm)
}

// Vperm2f128 selects 128-bit halves of src1 and src2 by imm.
func (a *Asm) Vperm2f128(dst, src1, src2 Reg, imm byte) {
	a.vinst(0x06, dst, src1, src2, vexMap0F3A, 0, 1, vexP66, false)
	a.emit(imm)
}

// VbroadcastssMemD is  dst = the float32 at [base+disp8], in all eight lanes.
func (a *Asm) VbroadcastssMemD(dst, base Reg, disp int8) {
	a.vexm(dst, 0, base, 0, vexMap0F38, 0, 1, vexP66)
	a.emit(0x18, modrm(0b01, dst, base), byte(disp))
}

// EmitI8Dot emits the int8 dot-product kernel the vector search's bounding
// pass runs: for each row of int8 codes, the exact int32 sum of code[i] *
// query[i]. Sixteen codes are widened to int16 (VPMOVSXBW) and multiplied
// and pair-summed against the query (VPMADDWD) per instruction; no product
// or sum can overflow int32 (127*127*65536 < 2^31).
//
// Args: A the first row's codes (rows back to back, XA bytes each), C the
// query's codes widened to int16, N the rows (at least 1), XA the dimensions
// (a multiple of 16), Out an int32 per row.
func EmitI8Dot() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.ShrRegImm8(R9, 4) // blocks of sixteen
	a.MovRegMem(R10, RDI, OffOut)

	a.Label("row")
	a.VpxorYmm(0, 0, 0)
	a.MovRegMem(RDX, RDI, OffC)
	a.MovRegReg(R12, R9)
	a.Label("block")
	a.vexm(1, 0, RSI, 0, vexMap0F38, 0, 1, vexP66) // vpmovsxbw ymm1, [rsi]
	a.emit(0x20, modrm(0b00, 1, RSI))
	a.vexm(1, 0, RDX, 1, vexMap0F, 0, 1, vexP66) // vpmaddwd ymm1, ymm1, [rdx]
	a.emit(0xF5, modrm(0b00, 1, RDX))
	a.vinst(0xFE, 0, 0, 1, vexMap0F, 0, 1, vexP66, false) // vpaddd ymm0, ymm0, ymm1
	a.AddRegImm8(RSI, 16)
	a.AddRegImm8(RDX, 32)
	a.DecReg(R12)
	a.Jcc(CondNE, "block")

	// The eight int32 lanes' sum, into the row's slot.
	a.Vextracti128(1, 0, 1)
	a.vinst(0xFE, 0, 0, 1, vexMap0F, 0, 0, vexP66, false) // vpaddd xmm0, xmm0, xmm1
	a.vinst(0x70, 1, 0, 0, vexMap0F, 0, 0, vexP66, true)  // vpshufd xmm1, xmm0, 0x4E
	a.emit(0x4E)
	a.vinst(0xFE, 0, 0, 1, vexMap0F, 0, 0, vexP66, false)
	a.vinst(0x70, 1, 0, 0, vexMap0F, 0, 0, vexP66, true) // vpshufd xmm1, xmm0, 0xB1
	a.emit(0xB1)
	a.vinst(0xFE, 0, 0, 1, vexMap0F, 0, 0, vexP66, false)
	a.vex3(0, RAX, 0, vexMap0F, 0, 0, vexP66, true) // vmovd eax, xmm0
	a.emit(0x7E, modrm(0b11, 0, RAX))
	a.emit(0x41, 0x89, modrm(0b00, RAX, R10)) // mov [r10], eax
	a.AddRegImm8(R10, 4)
	a.DecReg(RCX)
	a.Jcc(CondNE, "row")

	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// EmitMaxAbsBits emits the first half of building a vector column's int8
// copy: each row's largest |component|, as float32 BITS. With the sign masked
// off, a float's bits are a non-negative int32 whose order is the floats'
// order, and NaN and Inf sort above every finite value, so an integer max
// (VPMAXSD) finds the largest magnitude and flags a non-finite component in
// one pass.
//
// Args: A the first row, XC the stride, N the rows (at least 1), XA the
// dimensions (a multiple of 8), Out a uint32 per row.
func EmitMaxAbsBits() ([]byte, error) {
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
	a.VpcmpeqdYmm(7, 7, 7)
	a.vpsrldImm(7, 7, 1) // 0x7fffffff in every lane

	a.Label("row")
	a.VpxorYmm(0, 0, 0)
	a.MovRegReg(RAX, RSI)
	a.MovRegReg(R12, R9)
	a.Label("block")
	a.VmovdquLoad(1, RAX)
	a.VpandYmm(1, 1, 7)
	a.vinst(0x3D, 0, 0, 1, vexMap0F38, 0, 1, vexP66, false) // vpmaxsd ymm0, ymm0, ymm1
	a.AddRegImm8(RAX, 32)
	a.DecReg(R12)
	a.Jcc(CondNE, "block")
	a.Vextracti128(1, 0, 1)
	a.vinst(0x3D, 0, 0, 1, vexMap0F38, 0, 0, vexP66, false) // vpmaxsd xmm0, xmm0, xmm1
	a.vinst(0x70, 1, 0, 0, vexMap0F, 0, 0, vexP66, true)
	a.emit(0x4E)
	a.vinst(0x3D, 0, 0, 1, vexMap0F38, 0, 0, vexP66, false)
	a.vinst(0x70, 1, 0, 0, vexMap0F, 0, 0, vexP66, true)
	a.emit(0xB1)
	a.vinst(0x3D, 0, 0, 1, vexMap0F38, 0, 0, vexP66, false)
	a.vex3(0, RAX, 0, vexMap0F, 0, 0, vexP66, true) // vmovd eax, xmm0
	a.emit(0x7E, modrm(0b11, 0, RAX))
	a.emit(0x41, 0x89, modrm(0b00, RAX, R10)) // mov [r10], eax
	a.AddRegImm8(R10, 4)
	a.AddRegReg(RSI, RBX)
	a.DecReg(RCX)
	a.Jcc(CondNE, "row")
	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// EmitQuantize emits the second half: each component times its row's 1/scale,
// rounded to the nearest integer (VCVTPS2DQ, round-to-nearest-even under
// the default MXCSR) and packed to int8, and each row's sum of |code|. The
// caller's scales keep every product within +-127.5, so the saturating packs
// never saturate.
//
// Args: A the first row, XC the stride, V a float32 1/scale per row, N the
// rows (at least 1), XA the dimensions (a multiple of 8), Out the codes (rows
// back to back), Out2 an int32 per row.
func EmitQuantize() ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RBX, RDI, OffXC)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(R9, RDI, OffXA)
	a.ShrRegImm8(R9, 3)
	a.MovRegMem(R8, RDI, OffV)
	a.MovRegMem(R10, RDI, OffOut)
	a.MovRegMem(R11, RDI, OffOut2)

	a.Label("row")
	a.VbroadcastssMem(3, R8)
	a.VpxorYmm(4, 4, 4)
	a.MovRegReg(RAX, RSI)
	a.MovRegReg(R12, R9)
	a.Label("block")
	a.VmovdquLoad(1, RAX)
	a.VmulpsYmm(1, 1, 3)
	a.vinst(0x5B, 1, 0, 1, vexMap0F, 0, 1, vexP66, true)   // vcvtps2dq ymm1, ymm1
	a.vinst(0x1E, 2, 0, 1, vexMap0F38, 0, 1, vexP66, true) // vpabsd ymm2, ymm1
	a.vinst(0xFE, 4, 4, 2, vexMap0F, 0, 1, vexP66, false)  // vpaddd ymm4, ymm4, ymm2
	a.Vextracti128(2, 1, 1)
	a.vinst(0x6B, 1, 1, 2, vexMap0F, 0, 0, vexP66, false) // vpackssdw xmm1, xmm1, xmm2
	a.vinst(0x63, 1, 1, 1, vexMap0F, 0, 0, vexP66, false) // vpacksswb xmm1, xmm1, xmm1
	a.vexm(1, 0, R10, 0, vexMap0F, 0, 0, vexP66)          // vmovq [r10], xmm1
	a.emit(0xD6, modrm(0b00, 1, R10))
	a.AddRegImm8(R10, 8)
	a.AddRegImm8(RAX, 32)
	a.DecReg(R12)
	a.Jcc(CondNE, "block")
	a.Vextracti128(2, 4, 1)
	a.vinst(0xFE, 4, 4, 2, vexMap0F, 0, 0, vexP66, false)
	a.vinst(0x70, 2, 0, 4, vexMap0F, 0, 0, vexP66, true)
	a.emit(0x4E)
	a.vinst(0xFE, 4, 4, 2, vexMap0F, 0, 0, vexP66, false)
	a.vinst(0x70, 2, 0, 4, vexMap0F, 0, 0, vexP66, true)
	a.emit(0xB1)
	a.vinst(0xFE, 4, 4, 2, vexMap0F, 0, 0, vexP66, false)
	a.vex3(4, RAX, 0, vexMap0F, 0, 0, vexP66, true) // vmovd eax, xmm4
	a.emit(0x7E, modrm(0b11, 4, RAX))
	a.emit(0x41, 0x89, modrm(0b00, RAX, R11)) // mov [r11], eax
	a.AddRegImm8(R11, 4)
	a.AddRegImm8(R8, 4)
	a.AddRegReg(RSI, RBX)
	a.DecReg(RCX)
	a.Jcc(CondNE, "row")
	a.Vzeroupper()
	a.Ret()
	return a.Code()
}

// vpsrldImm is  dst = src >> imm, eight uint32 lanes.
func (a *Asm) vpsrldImm(dst, src Reg, imm byte) {
	a.vex3(2, src, dst, vexMap0F, 0, 1, vexP66, false)
	a.emit(0x72, modrm(0b11, 2, src), imm)
}

// vexm emits a three-byte VEX prefix for an instruction with a memory operand:
// R from reg, X from the SIB index (a vector register for a gather), B from
// the base.
func (a *Asm) vexm(reg, index, base, vvvv Reg, mm, w, l, pp byte) {
	bit := func(r Reg) byte { return ^byte(r>>3) & 1 }
	a.emit(0xC4,
		bit(reg)<<7|bit(index)<<6|bit(base)<<5|mm,
		w<<7|(^byte(vvvv)&0xF)<<3|l<<2|pp)
}

// VmovdquLoadD is  dst = [base+disp8]  (256-bit, unaligned).
func (a *Asm) VmovdquLoadD(dst, base Reg, disp int8) {
	a.vexm(dst, 0, base, 0, vexMap0F, 0, 1, vexPF3)
	a.emit(0x6F, modrm(0b01, dst, base), byte(disp))
}

// VxorpsYmm is  dst = src1 XOR src2.
func (a *Asm) VxorpsYmm(dst, src1, src2 Reg) {
	a.vinst(0x57, dst, src1, src2, vexMap0F, 0, 1, 0, false)
}

// VaddpsYmm is  dst = src1 + src2, eight float32 lanes.
func (a *Asm) VaddpsYmm(dst, src1, src2 Reg) {
	a.vinst(0x58, dst, src1, src2, vexMap0F, 0, 1, 0, false)
}

// VmulpsYmm is  dst = src1 * src2, eight float32 lanes.
func (a *Asm) VmulpsYmm(dst, src1, src2 Reg) {
	a.vinst(0x59, dst, src1, src2, vexMap0F, 0, 1, 0, false)
}

// VsubpsYmm is  dst = src1 - src2, eight float32 lanes.
func (a *Asm) VsubpsYmm(dst, src1, src2 Reg) {
	a.vinst(0x5C, dst, src1, src2, vexMap0F, 0, 1, 0, false)
}

// VpcmpeqdYmm is  dst = src1 == src2, eight int32 lanes; with dst = src1 =
// src2 it sets every bit.
func (a *Asm) VpcmpeqdYmm(dst, src1, src2 Reg) {
	a.vinst(0x76, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VbroadcastssMem is  dst = the float32 at [base], in all eight lanes.
func (a *Asm) VbroadcastssMem(dst, base Reg) {
	a.vexm(dst, 0, base, 0, vexMap0F38, 0, 1, vexP66)
	a.emit(0x18, modrm(0b00, dst, base))
}

// VgatherdpsYmm is  dst[i] = the float32 at [base + index[i]] for every lane
// whose mask sign bit is set, clearing the mask as it goes. base must not be
// RBP or R13 (mod 00 would mean disp32), and dst, index and mask must differ.
func (a *Asm) VgatherdpsYmm(dst, base, index, mask Reg) {
	a.vexm(dst, index, base, mask, vexMap0F38, 0, 1, vexP66)
	a.emit(0x92, modrm(0b00, dst, RSP), byte(index&7)<<3|byte(base&7))
}

// VmovupsStore is  [base+disp8] = src  (256-bit, unaligned).
func (a *Asm) VmovupsStore(base Reg, disp int8, src Reg) {
	a.vexm(src, 0, base, 0, vexMap0F, 0, 1, 0)
	a.emit(0x11, modrm(0b01, src, base), byte(disp))
}
