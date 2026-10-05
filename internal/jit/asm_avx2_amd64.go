//go:build amd64

package jit

// Package jit implements AVX2 encoding for the vector filter kernel.
// VPCMPGTQ compares four int64 lanes per instruction, unlike scalar loops.

const (
	vexMap0F   = 1
	vexMap0F38 = 2
	vexMap0F3A = 3

	vexP66 = 1
	vexPF3 = 2
)

// vex3 emits a three-byte VEX prefix.
func (a *Asm) vex3(reg, rm, vvvv Reg, mm, w, l, pp byte, noV bool) {
	rBit := byte(0)
	if reg >= 8 {
		rBit = 1
	}
	bBit := byte(0)
	if rm >= 8 {
		bBit = 1
	}
	v := byte(vvvv) & 0xF
	if noV {
		v = 0
	}
	a.emit(0xC4,
		(^rBit&1)<<7|(1)<<6|(^bBit&1)<<5|mm,
		w<<7|(^v&0xF)<<3|l<<2|pp)
}

// vinst emits a VEX instruction with a register-register ModRM.
func (a *Asm) vinst(op byte, dst, src1, src2 Reg, mm, w, l, pp byte, noV bool) {
	a.vex3(dst, src2, src1, mm, w, l, pp, noV)
	a.emit(op, modrm(0b11, dst, src2))
}

// VmovdquLoad is  dst = [src]  (256-bit, unaligned).
func (a *Asm) VmovdquLoad(dst, src Reg) {
	a.vex3(dst, src, 0, vexMap0F, 0, 1, vexPF3, true)
	a.emit(0x6F, modrm(0b00, dst, src))
}

// VpcmpgtqYmm is  dst = src1 > src2, lane-wise signed, all-ones where true.
func (a *Asm) VpcmpgtqYmm(dst, src1, src2 Reg) {
	a.vinst(0x37, dst, src1, src2, vexMap0F38, 0, 1, vexP66, false)
}

// VpcmpeqqYmm is  dst = src1 == src2, lane-wise.
func (a *Asm) VpcmpeqqYmm(dst, src1, src2 Reg) {
	a.vinst(0x29, dst, src1, src2, vexMap0F38, 0, 1, vexP66, false)
}

// VpandnYmm is  dst = (NOT src1) AND src2.
func (a *Asm) VpandnYmm(dst, src1, src2 Reg) {
	a.vinst(0xDF, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VpandYmm is  dst = src1 AND src2.
func (a *Asm) VpandYmm(dst, src1, src2 Reg) {
	a.vinst(0xDB, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VporYmm is  dst = src1 OR src2.
func (a *Asm) VporYmm(dst, src1, src2 Reg) {
	a.vinst(0xEB, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VpsubqYmm is  dst = src1 - src2. Subtracting an all-ones mask ADDS one, which
// is how a lane's verdict becomes a count with no branch.
func (a *Asm) VpsubqYmm(dst, src1, src2 Reg) {
	a.vinst(0xFB, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VpxorYmm is  dst = src1 XOR src2, used as dst = 0.
func (a *Asm) VpxorYmm(dst, src1, src2 Reg) {
	a.vinst(0xEF, dst, src1, src2, vexMap0F, 0, 1, vexP66, false)
}

// VpaddqXmm is  dst = src1 + src2, 128-bit.
func (a *Asm) VpaddqXmm(dst, src1, src2 Reg) {
	a.vinst(0xD4, dst, src1, src2, vexMap0F, 0, 0, vexP66, false)
}

// VmovqToXmm is  xmm = gpr.
func (a *Asm) VmovqToXmm(dst, src Reg) {
	a.vex3(dst, src, 0, vexMap0F, 1, 0, vexP66, true)
	a.emit(0x6E, modrm(0b11, dst, src))
}

// VmovqToReg is  gpr = xmm's low lane.
func (a *Asm) VmovqToReg(dst, src Reg) {
	a.vex3(src, dst, 0, vexMap0F, 1, 0, vexP66, true)
	a.emit(0x7E, modrm(0b11, src, dst))
}

// VpbroadcastqYmm fills every lane of dst with xmm src's low quadword.
func (a *Asm) VpbroadcastqYmm(dst, src Reg) {
	a.vex3(dst, src, 0, vexMap0F38, 0, 1, vexP66, true)
	a.emit(0x59, modrm(0b11, dst, src))
}

// Vextracti128 puts ymm src's high 128 bits into xmm dst.
func (a *Asm) Vextracti128(dst, src Reg, imm byte) {
	a.vex3(src, dst, 0, vexMap0F3A, 0, 1, vexP66, true)
	a.emit(0x39, modrm(0b11, src, dst), imm)
}

// Vpextrq is  gpr = xmm's quadword at index imm.
func (a *Asm) Vpextrq(dst, src Reg, imm byte) {
	a.vex3(src, dst, 0, vexMap0F3A, 1, 0, vexP66, true)
	a.emit(0x16, modrm(0b11, src, dst), imm)
}

// Vzeroupper clears the upper halves, which a kernel must do before returning
// to Go code that may use SSE.
func (a *Asm) Vzeroupper() { a.emit(0xC5, 0xF8, 0x77) }

// ShrRegImm8 is  dst >>= imm (unsigned).
func (a *Asm) ShrRegImm8(dst Reg, imm byte) {
	a.rex(0, dst)
	a.emit(0xC1, modrm(0b11, 5, dst), imm)
}

// AndRegImm8 is  dst &= imm.
func (a *Asm) AndRegImm8(dst Reg, imm int8) {
	a.rex(0, dst)
	a.emit(0x83, modrm(0b11, 4, dst), byte(imm))
}
