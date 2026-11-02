//go:build amd64

package jit

// Scalar double instructions for the float kernels (fir_amd64.go). SSE2 is
// part of x86-64; ROUNDSD needs SSE4.1 and the VFMADD forms FMA3, which the
// kernels ask cpuFeat about before emitting them.

const (
	X4 XReg = iota + 4
	X5
	X6
	X7
	X8
	X9
	X10
	X11
	X12
	X13
	X14
	X15
)

// x86 condition nibbles beyond the signed ones Cond names: the unsigned and
// parity tests UCOMISD and unsigned compares need.
const (
	ccB  Cond = 0x2
	ccAE Cond = 0x3
	ccBE Cond = 0x6
	ccA  Cond = 0x7
	ccP  Cond = 0xA
	ccNP Cond = 0xB
)

// fpRex emits a REX prefix for reg, index and rm, when one is needed.
func (a *Asm) fpRex(w bool, reg, index, rm byte) {
	r := byte(0x40)
	if w {
		r |= 8
	}
	r |= (reg >> 3 & 1) << 2
	r |= (index >> 3 & 1) << 1
	r |= rm >> 3 & 1
	if r != 0x40 {
		a.emit(r)
	}
}

// fpRR is  prefix [REX] 0F op modrm(reg, rm), register-direct.
func (a *Asm) fpRR(prefix byte, w bool, op []byte, reg, rm byte) {
	if prefix != 0 {
		a.emit(prefix)
	}
	a.fpRex(w, reg, 0, rm)
	a.emit(0x0F)
	a.emit(op...)
	a.emit(0xC0 | (reg&7)<<3 | rm&7)
}

// fpRM is fpRR with the operand [base+disp].
func (a *Asm) fpRM(prefix byte, w bool, op []byte, reg byte, base Reg, disp int32) {
	if prefix != 0 {
		a.emit(prefix)
	}
	a.fpRex(w, reg, 0, byte(base))
	a.emit(0x0F)
	a.emit(op...)
	a.memAny(reg, base, disp)
}

// memAny is the ModRM (and SIB) for [base+disp32], any base.
func (a *Asm) memAny(reg byte, base Reg, disp int32) {
	if base&7 == RSP {
		a.emit(0x80|(reg&7)<<3|4, 0x24)
	} else {
		a.emit(0x80 | (reg&7)<<3 | byte(base)&7)
	}
	a.emit32(uint32(disp))
}

// fpRIdx is fpRR with the operand [base+index*8].
func (a *Asm) fpRIdx(prefix byte, w bool, op []byte, reg byte, base, index Reg) {
	if prefix != 0 {
		a.emit(prefix)
	}
	a.fpRex(w, reg, byte(index), byte(base))
	a.emit(0x0F)
	a.emit(op...)
	if base&7 == RBP {
		a.emit(0x44|(reg&7)<<3, sib(3, index, base), 0)
		return
	}
	a.emit(0x04|(reg&7)<<3, sib(3, index, base))
}

// fpRIP is fpRR with the operand [rip+label].
func (a *Asm) fpRIP(prefix byte, op []byte, reg byte, label string) {
	if prefix != 0 {
		a.emit(prefix)
	}
	a.fpRex(false, reg, 0, 0)
	a.emit(0x0F)
	a.emit(op...)
	a.emit(0x05 | (reg&7)<<3)
	a.fixups = append(a.fixups, fixup{at: len(a.buf), label: label})
	a.emit(0, 0, 0, 0)
}

func (a *Asm) Movapd(x, y XReg)                 { a.fpRR(0x66, false, []byte{0x28}, byte(x), byte(y)) }
func (a *Asm) MovsdLoad(x XReg, b Reg, d int32) { a.fpRM(0xF2, false, []byte{0x10}, byte(x), b, d) }
func (a *Asm) MovsdStore(b Reg, d int32, x XReg) {
	a.fpRM(0xF2, false, []byte{0x11}, byte(x), b, d)
}
func (a *Asm) MovsdLoadIdx(x XReg, base, index Reg) {
	a.fpRIdx(0xF2, false, []byte{0x10}, byte(x), base, index)
}
func (a *Asm) MovsdRIP(x XReg, label string) { a.fpRIP(0xF2, []byte{0x10}, byte(x), label) }
func (a *Asm) Addsd(x, y XReg)               { a.fpRR(0xF2, false, []byte{0x58}, byte(x), byte(y)) }
func (a *Asm) Mulsd(x, y XReg)               { a.fpRR(0xF2, false, []byte{0x59}, byte(x), byte(y)) }
func (a *Asm) Subsd(x, y XReg)               { a.fpRR(0xF2, false, []byte{0x5C}, byte(x), byte(y)) }
func (a *Asm) Divsd(x, y XReg)               { a.fpRR(0xF2, false, []byte{0x5E}, byte(x), byte(y)) }
func (a *Asm) Sqrtsd(x, y XReg)              { a.fpRR(0xF2, false, []byte{0x51}, byte(x), byte(y)) }
func (a *Asm) Ucomisd(x, y XReg)             { a.fpRR(0x66, false, []byte{0x2E}, byte(x), byte(y)) }
func (a *Asm) Xorps(x, y XReg)               { a.fpRR(0, false, []byte{0x57}, byte(x), byte(y)) }

// AndpdRIP and XorpdRIP read 16 bytes at a 16-aligned label.
func (a *Asm) AndpdRIP(x XReg, label string) { a.fpRIP(0x66, []byte{0x54}, byte(x), label) }
func (a *Asm) XorpdRIP(x XReg, label string) { a.fpRIP(0x66, []byte{0x57}, byte(x), label) }

// Roundsd is  x = y rounded as mode says: 0 nearest-even, 1 down, 2 up, 3
// toward zero. SSE4.1.
func (a *Asm) Roundsd(x, y XReg, mode byte) {
	a.fpRR(0x66, false, []byte{0x3A, 0x0B}, byte(x), byte(y))
	a.emit(mode | 8) // 8: no precision exception
}

// Cvtsi2sdq is  x = float64(int64 r).
func (a *Asm) Cvtsi2sdq(x XReg, r Reg) { a.fpRR(0xF2, true, []byte{0x2A}, byte(x), byte(r)) }

// Cvttsd2siq is  r = int64(x), truncated.
func (a *Asm) Cvttsd2siq(r Reg, x XReg) { a.fpRR(0xF2, true, []byte{0x2C}, byte(r), byte(x)) }

// MovqXR is  x = r's bits; MovqRX  r = x's.
func (a *Asm) MovqXR(x XReg, r Reg) { a.fpRR(0x66, true, []byte{0x6E}, byte(x), byte(r)) }
func (a *Asm) MovqRX(r Reg, x XReg) { a.fpRR(0x66, true, []byte{0x7E}, byte(x), byte(r)) }

// Vfmadd213sd is  x = y*x + z, rounded once. FMA3.
func (a *Asm) Vfmadd213sd(x, y, z XReg) {
	a.vinst(0xA9, Reg(x), Reg(y), Reg(z), vexMap0F38, 1, 0, vexP66, false)
}

// ShlRegImm8 is  dst <<= n.
func (a *Asm) ShlRegImm8(dst Reg, n byte) {
	a.rex(4, dst)
	a.emit(0xC1, modrm(0b11, 4, dst), n)
}

// aluImm32 is  dst = dst op imm, sign-extended: op is the ModRM digit (0 ADD,
// 1 OR, 4 AND, 5 SUB, 6 XOR, 7 CMP).
func (a *Asm) aluImm32(op byte, dst Reg, imm int32) {
	a.rex(Reg(op), dst)
	a.emit(0x81, modrm(0b11, Reg(op), dst))
	a.emit32(uint32(imm))
}
