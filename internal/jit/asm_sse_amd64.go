package jit

// SSE2 byte-vector instructions for the text operations. SSE2 is part of
// x86-64, so these need no CPU check. Encoded with the legacy prefixes: the
// mandatory prefix (66 or F3) comes before REX, and REX carries no W.

// XReg is an XMM register.
type XReg uint8

const (
	X0 XReg = iota
	X1
	X2
	X3
)

// sse emits prefix, an optional REX for the extended registers, 0F op, and a
// register-direct ModRM.
func (a *Asm) sse(prefix, op byte, reg, rm byte) {
	a.emit(prefix)
	if r := byte(0x40) | (reg>>3)<<2 | rm>>3; r != 0x40 {
		a.emit(r)
	}
	a.emit(0x0F, op, 0xC0|(reg&7)<<3|rm&7)
}

// MovdquLoad is  x = the 16 bytes at [base]. base must not be RSP or R12,
// which would need a SIB byte; a disp8 of 0 covers RBP and R13.
func (a *Asm) MovdquLoad(x XReg, base Reg) {
	a.emit(0xF3)
	if r := byte(0x40) | (byte(x)>>3)<<2 | byte(base)>>3; r != 0x40 {
		a.emit(r)
	}
	a.emit(0x0F, 0x6F, 0x40|(byte(x)&7)<<3|byte(base)&7, 0)
}

// Pcmpeqb is  x[i] = 0xFF where x[i] == y[i], else 0.
func (a *Asm) Pcmpeqb(x, y XReg) { a.sse(0x66, 0x74, byte(x), byte(y)) }

// Pxor is  x ^= y; Pxor(x, x) zeroes x.
func (a *Asm) Pxor(x, y XReg) { a.sse(0x66, 0xEF, byte(x), byte(y)) }

// Pmovmskb is  dst = one bit per byte of x: its high bit.
func (a *Asm) Pmovmskb(dst Reg, x XReg) { a.sse(0x66, 0xD7, byte(dst), byte(x)) }

// BsfRegReg is  dst = the index of src's lowest set bit (src non-zero).
func (a *Asm) BsfRegReg(dst, src Reg) {
	a.rex(dst, src)
	a.emit(0x0F, 0xBC, modrm(0b11, dst, src))
}

// MovzxByte is  dst = the byte at [base], zero-extended. base must not be RSP
// or R12; a disp8 of 0 covers RBP and R13.
func (a *Asm) MovzxByte(dst, base Reg) {
	if r := byte(0x40) | (byte(dst)>>3)<<2 | byte(base)>>3; r != 0x40 {
		a.emit(r)
	}
	a.emit(0x0F, 0xB6, 0x40|(byte(dst)&7)<<3|byte(base)&7, 0)
}

// MovRegReg32 is  dst = src's low 32 bits, zero-extended.
func (a *Asm) MovRegReg32(dst, src Reg) {
	if r := byte(0x40) | (byte(src)>>3)<<2 | byte(dst)>>3; r != 0x40 {
		a.emit(r)
	}
	a.emit(0x89, modrm(0b11, src, dst))
}

// TestRegImm8 sets flags from the low byte of r AND imm (r below RSP: AL, CL, DL, BL).
func (a *Asm) TestRegImm8(r Reg, imm byte) { a.emit(0xF6, modrm(0b11, 0, r), imm) }
