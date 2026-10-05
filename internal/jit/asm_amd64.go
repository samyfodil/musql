//go:build amd64

package jit

// A minimal x86-64 encoder: only instructions the filter kernels use.

// Reg is a general-purpose register number.
type Reg uint8

const (
	RAX Reg = iota
	RCX
	RDX
	RBX
	RSP
	RBP
	RSI
	RDI
	R8
	R9
	R10
	R11
	R12
	R13
	R14
	R15
)

// Asm accumulates encoded instructions and resolves forward jumps.
type Asm struct {
	buf    []byte
	labels map[string]int
	fixups []fixup
}

type fixup struct {
	at    int // offset of the rel32 field
	label string
}

func NewAsm() *Asm { return &Asm{labels: map[string]int{}} }

// Code returns the encoded bytes with every jump resolved.
func (a *Asm) Code() ([]byte, error) {
	for _, f := range a.fixups {
		target, ok := a.labels[f.label]
		if !ok {
			return nil, errUnboundLabel(f.label)
		}
		rel := int32(target - (f.at + 4))
		a.buf[f.at] = byte(rel)
		a.buf[f.at+1] = byte(rel >> 8)
		a.buf[f.at+2] = byte(rel >> 16)
		a.buf[f.at+3] = byte(rel >> 24)
	}
	return a.buf, nil
}

type errUnboundLabel string

func (e errUnboundLabel) Error() string { return "jit: unbound label " + string(e) }

// Label marks the current position.
func (a *Asm) Label(name string) { a.labels[name] = len(a.buf) }

func (a *Asm) emit(b ...byte) { a.buf = append(a.buf, b...) }

// rex emits the REX prefix for a 64-bit operation on (reg, rm).
func (a *Asm) rex(reg, rm Reg) {
	b := byte(0x48)
	if reg >= R8 {
		b |= 0x4
	}
	if rm >= R8 {
		b |= 0x1
	}
	a.emit(b)
}

func modrm(mod byte, reg, rm Reg) byte { return mod<<6 | byte(reg&7)<<3 | byte(rm&7) }

// MovRegMem is  dst = [src+disp].
func (a *Asm) MovRegMem(dst, src Reg, disp int8) {
	a.rex(dst, src)
	a.emit(0x8B, modrm(0b01, dst, src), byte(disp))
	if src&7 == RSP {
		a.buf = append(a.buf[:len(a.buf)-1], 0x24, byte(disp))
	}
}

// MovMemReg is  [dst+disp] = src.
func (a *Asm) MovMemReg(dst Reg, disp int8, src Reg) {
	a.rex(src, dst)
	a.emit(0x89, modrm(0b01, src, dst), byte(disp))
}

// MovRegReg is  dst = src.
func (a *Asm) MovRegReg(dst, src Reg) {
	a.rex(src, dst)
	a.emit(0x89, modrm(0b11, src, dst))
}

// XorRegReg is  dst ^= src, used as dst = 0 when they are the same.
func (a *Asm) XorRegReg(dst, src Reg) {
	a.rex(src, dst)
	a.emit(0x31, modrm(0b11, src, dst))
}

// CmpRegReg compares a against b, setting flags for a signed test of a OP b.
func (a *Asm) CmpRegReg(x, y Reg) {
	a.rex(y, x)
	a.emit(0x39, modrm(0b11, y, x))
}

// AddRegImm8 is  dst += imm.
func (a *Asm) AddRegImm8(dst Reg, imm int8) {
	a.rex(0, dst)
	a.emit(0x83, modrm(0b11, 0, dst), byte(imm))
}

// IncReg is  dst++.
func (a *Asm) IncReg(dst Reg) {
	a.rex(0, dst)
	a.emit(0xFF, modrm(0b11, 0, dst))
}

// DecReg is  dst--.
func (a *Asm) DecReg(dst Reg) {
	a.rex(1, dst)
	a.emit(0xFF, modrm(0b11, 1, dst))
}

// TestRegReg sets flags from x AND y without storing the result.
func (a *Asm) TestRegReg(x, y Reg) {
	a.rex(y, x)
	a.emit(0x85, modrm(0b11, y, x))
}

// Jcc jumps to label when cond holds.
func (a *Asm) Jcc(cond Cond, label string) {
	a.emit(0x0F, 0x80|byte(cond))
	a.fixups = append(a.fixups, fixup{at: len(a.buf), label: label})
	a.emit(0, 0, 0, 0)
}

// Jmp jumps to label unconditionally.
func (a *Asm) Jmp(label string) {
	a.emit(0xE9)
	a.fixups = append(a.fixups, fixup{at: len(a.buf), label: label})
	a.emit(0, 0, 0, 0)
}

// Ret returns to the trampoline.
func (a *Asm) Ret() { a.emit(0xC3) }

// AddRegReg is  dst += src.
func (a *Asm) AddRegReg(dst, src Reg) {
	a.rex(src, dst)
	a.emit(0x01, modrm(0b11, src, dst))
}

// Indexed addressing and flag materialisation: base+index*8 lets one register
// serve as cursor and counter; SETcc avoids branches in the filter loop.

// rexX emits REX prefix for 64-bit with INDEX register; REX.X is its high bit.
func (a *Asm) rexX(reg, index, base Reg) {
	b := byte(0x48)
	if reg >= R8 {
		b |= 0x4
	}
	if index >= R8 {
		b |= 0x2
	}
	if base >= R8 {
		b |= 0x1
	}
	a.emit(b)
}

// sib encodes scale*index+base. scale is the SHIFT (3 means *8).
func sib(scale byte, index, base Reg) byte {
	return scale<<6 | byte(index&7)<<3 | byte(base&7)
}

// CmpMemIdxReg compares [base+index*8] against src, setting flags for column OP bound.
//
// mod=00 with rm=100 means "SIB, no displacement", which is only legal when the
// base is not RBP or R13; those encode disp32 instead. The filter kernel's
// bases come from Args and are never either, and a caller that changed that
// would get a silently different address, so it is asserted rather than
// handled.
func (a *Asm) CmpMemIdxReg(base, index Reg, src Reg) {
	a.rexX(src, index, base)
	a.emitSIB(0x39, src, index, base)
}

// emitSIB writes one base+index*8 memory operand, choosing the addressing form
// the base allows.
//
// mod=00 with rm=100 means "SIB, no displacement", and it is ILLEGAL when the
// base's low three bits are RBP's -- that encoding is spoken for by
// disp32-without-base. R13 shares those bits, so a program that hoisted a
// column base into R13 assembled into something that read a completely
// different address. Emitting an explicit zero displacement costs one byte and
// removes the special case, which is better than forbidding the register: the
// alternative was three column blocks instead of four.
func (a *Asm) emitSIB(op byte, reg, index, base Reg) {
	if base&7 == RBP {
		a.emit(op, modrm(0b01, reg, 0b100), sib(3, index, base), 0)
		return
	}
	a.emit(op, modrm(0b00, reg, 0b100), sib(3, index, base))
}

// LeaIdx is  dst = base + index*8, computed without touching flags.
func (a *Asm) LeaIdx(dst, base, index Reg) {
	a.rexX(dst, index, base)
	a.emitSIB(0x8D, dst, index, base)
}

// NegReg is  dst = -dst.
func (a *Asm) NegReg(dst Reg) {
	a.rex(0, dst)
	a.emit(0xF7, modrm(0b11, 3, dst))
}

// Setcc writes 1 into dst's LOW BYTE when cond holds and 0 when it does not.
//
// Only the low byte: the other seven are left exactly as they were, which is
// what makes this usable as a counter addend. Zero the register once before the
// loop and it stays a clean 0-or-1 forever, so no zero-extension is needed per
// row. The REX prefix is emitted unconditionally because without it byte
// operands 4-7 mean AH/CH/DH/BH rather than SPL/BPL/SIL/DIL.
func (a *Asm) Setcc(cond Cond, dst Reg) {
	b := byte(0x40)
	if dst >= R8 {
		b |= 0x1
	}
	a.emit(b, 0x0F, 0x90|byte(cond), modrm(0b11, 0, dst))
}

// Cmovcc is  dst = src when cond holds, and leaves dst alone otherwise.
//
// It replaces a second SETcc and the AND that combined the two: set the first
// predicate into dst, then conditionally move ZERO into it when the second
// fails. One instruction instead of two, and still no branch.
func (a *Asm) Cmovcc(cond Cond, dst, src Reg) {
	a.rex(dst, src)
	a.emit(0x0F, 0x40|byte(cond), modrm(0b11, dst, src))
}

// MovRegMemIdx is  dst = [base + index*8].
func (a *Asm) MovRegMemIdx(dst, base, index Reg) {
	a.rexX(dst, index, base)
	a.emitSIB(0x8B, dst, index, base)
}

// MovMemIdxReg is  [base + index*8] = src, MovRegMemIdx's mirror.
func (a *Asm) MovMemIdxReg(base, index, src Reg) {
	a.rexX(src, index, base)
	a.emitSIB(0x89, src, index, base)
}

// MovRegMem32 is  dst = [base + slot*8], for a register-file slot whose index
// may exceed a signed byte -- the fixed kernels only ever reach Args' handful
// of fields, but a compiled program indexes a register file.
func (a *Asm) MovRegMem32(dst, base Reg, slot int) {
	a.rex(dst, base)
	a.emit(0x8B, modrm(0b10, dst, base))
	a.emit32(uint32(int32(slot * 8)))
}

// MovMemReg32 is  [base + slot*8] = src.
func (a *Asm) MovMemReg32(base Reg, slot int, src Reg) {
	a.rex(src, base)
	a.emit(0x89, modrm(0b10, src, base))
	a.emit32(uint32(int32(slot * 8)))
}

// AndRegReg is  dst &= src.
func (a *Asm) AndRegReg(dst, src Reg) {
	a.rex(src, dst)
	a.emit(0x21, modrm(0b11, src, dst))
}

// OrRegReg is  dst |= src.
func (a *Asm) OrRegReg(dst, src Reg) {
	a.rex(src, dst)
	a.emit(0x09, modrm(0b11, src, dst))
}

// AddRegRegOverflow is  dst += src, recording signed overflow into flag.
//
// SQLite switches sum() to floating point when an integer sum overflows, and
// this cannot, so the caller has to know it happened. SETO reads exactly the
// flag ADD sets for a signed carry, and OR-ing it into a sticky register keeps
// the answer for the whole loop without a branch in the hot path.
func (a *Asm) AddRegRegOverflow(dst, src, flag Reg) {
	a.AddRegReg(dst, src)
	a.Setcc(condOverflow, R11)
	a.OrRegReg(flag, R11)
}

func (a *Asm) emit32(v uint32) {
	a.emit(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// MovRegImm64 is  dst = imm, as a full 64-bit immediate.
func (a *Asm) MovRegImm64(dst Reg, imm int64) {
	b := byte(0x48)
	if dst >= R8 {
		b |= 0x1
	}
	a.emit(b, 0xB8|byte(dst&7))
	v := uint64(imm)
	for i := 0; i < 8; i++ {
		a.emit(byte(v >> (8 * i)))
	}
}

// SubRegReg is  dst -= src.
func (a *Asm) SubRegReg(dst, src Reg) {
	a.rex(src, dst)
	a.emit(0x29, modrm(0b11, src, dst))
}

// ImulRegReg is  dst *= src, setting OF on signed overflow.
func (a *Asm) ImulRegReg(dst, src Reg) {
	a.rex(dst, src)
	a.emit(0x0F, 0xAF, modrm(0b11, dst, src))
}

// --- [base+disp32] operands, for the VM JIT: a register file indexed by a
// VDBE register number reaches past a signed byte, and a Value is wider than a
// SIB scale can express, so every access is one base register plus a 32-bit
// displacement computed at emit time.

// memD writes the ModRM and displacement of a [base+disp32] operand. RSP and
// R12 as a base need a SIB byte this does not emit; the VM JIT's bases are RSI
// and RDI, and anything else is a bug in the caller, not an input.
func (a *Asm) memD(reg, base Reg, disp int32) {
	if base&7 == RSP {
		panic("jit: memD with an RSP/R12 base")
	}
	a.emit(modrm(0b10, reg, base))
	a.emit32(uint32(disp))
}

// MovRegMemD is  dst = [base+disp].
func (a *Asm) MovRegMemD(dst, base Reg, disp int32) {
	a.rex(dst, base)
	a.emit(0x8B)
	a.memD(dst, base, disp)
}

// MovMemDReg is  [base+disp] = src.
func (a *Asm) MovMemDReg(base Reg, disp int32, src Reg) {
	a.rex(src, base)
	a.emit(0x89)
	a.memD(src, base, disp)
}

// MovMemDImm is  [base+disp] = imm, sign-extended to 64 bits.
func (a *Asm) MovMemDImm(base Reg, disp int32, imm int32) {
	a.rex(0, base)
	a.emit(0xC7)
	a.memD(0, base, disp)
	a.emit32(uint32(imm))
}

// CmpMemDImm8 compares the qword [base+disp] against imm.
func (a *Asm) CmpMemDImm8(base Reg, disp int32, imm int8) {
	a.rex(7, base)
	a.emit(0x83)
	a.memD(7, base, disp)
	a.emit(byte(imm))
}

// CmpMemDByte compares the BYTE [base+disp] against imm.
func (a *Asm) CmpMemDByte(base Reg, disp int32, imm byte) {
	if base >= R8 {
		a.emit(0x41)
	}
	a.emit(0x80)
	a.memD(7, base, disp)
	a.emit(imm)
}

// AluRegMemD is  dst = dst OP [base+disp] for OP one of AluAdd, AluSub,
// AluAnd, AluOr, AluCmp (flags only) or AluImul.
func (a *Asm) AluRegMemD(op AluOp, dst, base Reg, disp int32) {
	a.rex(dst, base)
	if op == AluImul {
		a.emit(0x0F, 0xAF)
	} else {
		a.emit(byte(op))
	}
	a.memD(dst, base, disp)
}

// AluOp is the opcode byte of a reg, r/m ALU instruction.
type AluOp byte

const (
	AluAdd  AluOp = 0x03
	AluOr   AluOp = 0x0B
	AluAnd  AluOp = 0x23
	AluSub  AluOp = 0x2B
	AluCmp  AluOp = 0x3B
	AluImul AluOp = 0xAF // two-byte 0F AF; handled specially
)

// ShlRegCL and SarRegCL shift dst by CL.
func (a *Asm) ShlRegCL(dst Reg) { a.rex(4, dst); a.emit(0xD3, modrm(0b11, 4, dst)) }
func (a *Asm) SarRegCL(dst Reg) { a.rex(7, dst); a.emit(0xD3, modrm(0b11, 7, dst)) }

// CmpRegImm32 compares dst against imm.
func (a *Asm) CmpRegImm32(dst Reg, imm int32) {
	a.rex(7, dst)
	a.emit(0x81, modrm(0b11, 7, dst))
	a.emit32(uint32(imm))
}

// LeaRIP is  dst = the address of label.
func (a *Asm) LeaRIP(dst Reg, label string) {
	a.rex(dst, 0)
	a.emit(0x8D, modrm(0b00, dst, RBP)) // rm=101 with mod=00 is RIP+disp32
	a.fixups = append(a.fixups, fixup{at: len(a.buf), label: label})
	a.emit(0, 0, 0, 0)
}

// MovsxdRegMemIdx4 is  dst = sign-extended int32 [base + index*4].
func (a *Asm) MovsxdRegMemIdx4(dst, base, index Reg) {
	if base&7 == RBP {
		panic("jit: MovsxdRegMemIdx4 with an RBP/R13 base")
	}
	a.rexX(dst, index, base)
	a.emit(0x63, modrm(0b00, dst, 0b100), sib(2, index, base))
}

// JmpReg jumps to the address in r.
func (a *Asm) JmpReg(r Reg) {
	if r >= R8 {
		a.emit(0x41)
	}
	a.emit(0xFF, modrm(0b11, 4, r))
}
