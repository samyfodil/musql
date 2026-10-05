//go:build arm64

package jit

import "fmt"

// A minimal AArch64 encoder for filter kernel instructions. Every instruction
// is one word, little-endian. Kernels use only X0-X10 (caller-saved).

// Reg64 is a general-purpose register number.
type Reg64 uint8

const (
	X0 Reg64 = iota
	X1
	X2
	X3
	X4
	X5
	X6
	X7
	X8
	X9
	X10
	X11
	X12
)

// xzr is the zero register, which shares encoding 31 with the stack pointer.
// Which one an instruction means is fixed by the instruction, not by the
// operand, so it is only ever used here where the manual says "zero".
const xzr Reg64 = 31

// VReg is a SIMD register number. The kernels stay in V0-V7: AAPCS makes them
// caller-saved outright, where V8-V15 have their low 64 bits preserved.
type VReg uint8

// Arm accumulates encoded instructions and resolves forward branches.
type Arm struct {
	buf    []uint32
	labels map[string]int // label -> instruction index
	fixups []armFixup
}

type armFixup struct {
	at    int // instruction index holding the displacement
	label string
	kind  armFixupKind
}

type armFixupKind uint8

const (
	fixCond armFixupKind = iota // B.cond / CBZ / CBNZ: imm19 at bits 5..23
)

// NewArm returns an empty encoder.
func NewArm() *Arm { return &Arm{labels: map[string]int{}} }

// Label marks the current position.
func (a *Arm) Label(name string) { a.labels[name] = len(a.buf) }

func (a *Arm) emit(w uint32) { a.buf = append(a.buf, w) }

// Code resolves every branch and returns the encoded bytes.
func (a *Arm) Code() ([]byte, error) {
	for _, f := range a.fixups {
		target, ok := a.labels[f.label]
		if !ok {
			return nil, fmt.Errorf("jit: unbound label %s", f.label)
		}
		// AArch64 branch displacements are in INSTRUCTIONS from the branch
		// itself, not in bytes and not from the following instruction. Both of
		// those differences are silent if you get them wrong: the jump lands
		// somewhere legal and the kernel answers something plausible.
		delta := target - f.at
		limit := 1 << 18 // imm19, the conditional forms
		if f.kind == fixBranch26 {
			limit = 1 << 25
		}
		if delta < -limit || delta >= limit {
			return nil, fmt.Errorf("jit: branch to %s out of range (%d)", f.label, delta)
		}
		switch f.kind {
		case fixCond:
			a.buf[f.at] |= uint32(delta&0x7FFFF) << 5
		case fixBranch26:
			a.buf[f.at] |= uint32(delta & 0x3FFFFFF)
		}
	}
	out := make([]byte, 0, len(a.buf)*4)
	for _, w := range a.buf {
		out = append(out, byte(w), byte(w>>8), byte(w>>16), byte(w>>24))
	}
	return out, nil
}

// armCond translates the portable condition into AArch64's 4-bit encoding.
//
// Cond's numeric values are x86 Jcc nibbles, which is an accident of which
// architecture was written first; nothing outside an encoder may depend on
// them. This is the translation table that keeps that true.
func armCond(c Cond) uint32 {
	switch c {
	case CondE:
		return 0x0 // EQ
	case CondNE:
		return 0x1 // NE
	case CondGE:
		return 0xA // GE
	case CondL:
		return 0xB // LT
	case CondG:
		return 0xC // GT
	default:
		return 0xD // LE
	}
}

// LdrImm emits LDR Xt, [Xn, #off] -- off must be a non-negative multiple of 8,
// which every Args field is by construction.
func (a *Arm) LdrImm(rt, rn Reg64, off int) {
	a.emit(0xF9400000 | uint32(off/8)<<10 | uint32(rn)<<5 | uint32(rt))
}

// StrImm emits STR Xt, [Xn, #off].
func (a *Arm) StrImm(rt, rn Reg64, off int) {
	a.emit(0xF9000000 | uint32(off/8)<<10 | uint32(rn)<<5 | uint32(rt))
}

// LdrPost emits LDR Xt, [Xn], #imm -- load, then advance the cursor. One
// instruction where x86 needs a load and an add, which is the whole reason the
// arm64 scalar loop is shorter than its x86 twin.
func (a *Arm) LdrPost(rt, rn Reg64, imm int) {
	a.emit(0xF8400400 | uint32(imm&0x1FF)<<12 | uint32(rn)<<5 | uint32(rt))
}

// LdrQPost emits LDR Qt, [Xn], #imm: a 128-bit SIMD load with the same
// post-increment.
func (a *Arm) LdrQPost(vt VReg, rn Reg64, imm int) {
	a.emit(0x3CC00400 | uint32(imm&0x1FF)<<12 | uint32(rn)<<5 | uint32(vt))
}

// MovImm16 emits MOVZ Xd, #imm (zeroing the rest), which is how a kernel gets
// a small constant without a literal pool.
func (a *Arm) MovImm16(rd Reg64, imm uint16) {
	a.emit(0xD2800000 | uint32(imm)<<5 | uint32(rd))
}

// MovReg emits MOV Xd, Xm (ORR Xd, XZR, Xm).
func (a *Arm) MovReg(rd, rm Reg64) {
	a.emit(0xAA0003E0 | uint32(rm)<<16 | uint32(rd))
}

// AddImm emits ADD Xd, Xn, #imm (imm12, unshifted).
func (a *Arm) AddImm(rd, rn Reg64, imm uint32) {
	a.emit(0x91000000 | imm<<10 | uint32(rn)<<5 | uint32(rd))
}

// SubsImm emits SUBS Xd, Xn, #imm -- subtract AND set flags, which is the
// loop counter's decrement and its test in one instruction.
func (a *Arm) SubsImm(rd, rn Reg64, imm uint32) {
	a.emit(0xF1000000 | imm<<10 | uint32(rn)<<5 | uint32(rd))
}

// AddReg emits ADD Xd, Xn, Xm.
func (a *Arm) AddReg(rd, rn, rm Reg64) {
	a.emit(0x8B000000 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// AndReg emits AND Xd, Xn, Xm.
func (a *Arm) AndReg(rd, rn, rm Reg64) {
	a.emit(0x8A000000 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// LsrImm emits LSR Xd, Xn, #sh (UBFM with the shift's canonical fields).
func (a *Arm) LsrImm(rd, rn Reg64, sh uint32) {
	a.emit(0xD340_0000 | sh<<16 | 63<<10 | uint32(rn)<<5 | uint32(rd))
}

// AndImmLow emits AND Xd, Xn, #mask for a mask of the low n bits, the only
// shape the kernels need. AArch64's logical immediates are an encoded bit
// pattern rather than a literal, and (N=1, immr=0, imms=n-1) is the encoding
// for "n consecutive low ones".
func (a *Arm) AndImmLow(rd, rn Reg64, bits uint32) {
	a.emit(0x92400000 | (bits-1)<<10 | uint32(rn)<<5 | uint32(rd))
}

// Cmp emits CMP Xn, Xm (SUBS XZR, Xn, Xm).
func (a *Arm) Cmp(rn, rm Reg64) {
	a.emit(0xEB000000 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(xzr))
}

// Cset emits CSET Xd, cond -- 1 when the condition holds, 0 otherwise. This is
// what makes the scalar kernel branch-free where x86's has to jump.
func (a *Arm) Cset(rd Reg64, c Cond) {
	// CSET Xd, cond == CSINC Xd, XZR, XZR, invert(cond).
	a.emit(0x9A9F07E0 | armCond(c.Negate())<<12 | uint32(rd))
}

// Bcond emits B.cond to a label.
func (a *Arm) Bcond(c Cond, label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixCond})
	a.emit(0x54000000 | armCond(c))
}

// Cbz emits CBZ Xt, label -- branch when the register is zero, without
// disturbing the flags.
func (a *Arm) Cbz(rt Reg64, label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixCond})
	a.emit(0xB4000000 | uint32(rt))
}

// Ret emits RET (to X30).
func (a *Arm) Ret() { a.emit(0xD65F03C0) }

// --- NEON. Two int64 lanes where AVX2 has four. ---

// Dup emits DUP Vd.2D, Xn: the comparison constant into both lanes.
func (a *Arm) Dup(vd VReg, rn Reg64) {
	a.emit(0x4E080C00 | uint32(rn)<<5 | uint32(vd))
}

// EorV emits EOR Vd.16B, Vn.16B, Vm.16B, which is how the accumulator is
// zeroed.
func (a *Arm) EorV(vd, vn, vm VReg) {
	a.emit(0x6E201C00 | uint32(vm)<<16 | uint32(vn)<<5 | uint32(vd))
}

// Cmgt emits CMGT Vd.2D, Vn.2D, Vm.2D -- signed greater-than, leaving each
// lane all-ones or zero. AVX2's VPCMPGTQ exactly.
func (a *Arm) Cmgt(vd, vn, vm VReg) {
	a.emit(0x4EE03400 | uint32(vm)<<16 | uint32(vn)<<5 | uint32(vd))
}

// Cmeq emits CMEQ Vd.2D, Vn.2D, Vm.2D.
func (a *Arm) Cmeq(vd, vn, vm VReg) {
	a.emit(0x6EE08C00 | uint32(vm)<<16 | uint32(vn)<<5 | uint32(vd))
}

// NotV emits NOT Vd.16B, Vn.16B. AArch64 has this outright, so negating a
// compare costs one instruction here against x86's "materialise all-ones, then
// ANDN" pair.
func (a *Arm) NotV(vd, vn VReg) {
	a.emit(0x6E205800 | uint32(vn)<<5 | uint32(vd))
}

// AndV emits AND Vd.16B, Vn.16B, Vm.16B.
func (a *Arm) AndV(vd, vn, vm VReg) {
	a.emit(0x4E201C00 | uint32(vm)<<16 | uint32(vn)<<5 | uint32(vd))
}

// SubV emits SUB Vd.2D, Vn.2D, Vm.2D: subtracting an all-ones lane adds one,
// which is the branch-free accumulate.
func (a *Arm) SubV(vd, vn, vm VReg) {
	a.emit(0x6EE08400 | uint32(vm)<<16 | uint32(vn)<<5 | uint32(vd))
}

// UmovD emits UMOV Xd, Vn.D[index] -- how the accumulator's lanes come back
// into general registers for the final sum.
func (a *Arm) UmovD(rd Reg64, vn VReg, index int) {
	imm5 := uint32(8)
	if index == 1 {
		imm5 = 24
	}
	a.emit(0x4E003C00 | imm5<<16 | uint32(vn)<<5 | uint32(rd))
}

// --- Conditional compare and conditional increment.
//
// These are what lets the scalar filter loop be seven instructions instead of
// ten, and they have no x86 counterpart worth the name.
//
// CCMP chains two comparisons THROUGH THE FLAGS: it performs the second
// compare only when a condition on the first holds, and otherwise writes a
// literal NZCV. Pick that literal so the second condition reads false and the
// pair behaves exactly like a short-circuit AND, with no intermediate register
// and no branch. CINC then folds "materialise the flag, add it" into one.

// Ccmp emits CCMP Xn, Xm, #nzcv, cond: compare when cond holds, otherwise set
// the flags to nzcv.
func (a *Arm) Ccmp(rn, rm Reg64, nzcv uint32, cond Cond) {
	a.emit(0xFA400000 | uint32(rm)<<16 | armCond(cond)<<12 | uint32(rn)<<5 | nzcv)
}

// Cinc emits CINC Xd, Xn, cond: Xd = Xn + 1 when cond holds, Xn otherwise.
func (a *Arm) Cinc(rd, rn Reg64, cond Cond) {
	// CINC Xd, Xn, cond == CSINC Xd, Xn, Xn, invert(cond).
	a.emit(0x9A800400 | uint32(rn)<<16 | armCond(cond.Negate())<<12 | uint32(rn)<<5 | uint32(rd))
}

// Csel emits CSEL Xd, Xn, Xm, cond -- Xd = cond ? Xn : Xm. The conditional
// move x86 has had since the Pentium Pro, and the reason a min()/max() loop
// here needs no branch either.
func (a *Arm) Csel(rd, rn, rm Reg64, c Cond) {
	a.emit(0x9A800000 | uint32(rm)<<16 | armCond(c)<<12 | uint32(rn)<<5 | uint32(rd))
}

// nzcvFalseFor returns an NZCV literal on which cond reads FALSE, which is what
// CCMP must write when the first predicate already failed.
//
// N=8 Z=4 C=2 V=1. EQ is Z, NE is !Z, GE is N==V, LT is N!=V, GT is !Z && N==V,
// LE is Z || N!=V -- so clearing everything falsifies EQ, LT and LE, Z alone
// falsifies NE and GT, and N without V falsifies GE.
func nzcvFalseFor(cond Cond) uint32 {
	switch cond {
	case CondNE, CondG:
		return 0x4 // Z set
	case CondGE:
		return 0x8 // N set, V clear
	default: // CondE, CondL, CondLE
		return 0x0
	}
}

// --- What the PROGRAM emitter needs beyond the fixed kernels.
//
// The IR is architecture-neutral, so this is an encoder job rather than a
// redesign: loads through a register index, an unconditional branch, a
// non-zero test, a full 64-bit immediate, and an add that sets the flags so
// overflow can be read out of it.

// fixBranch26 is an unconditional B's imm26 field, at bits 0..25.
const fixBranch26 armFixupKind = 1

// LdrRegIdx emits LDR Xt, [Xn, Xm, LSL #3] -- the column read, whose index is
// a register rather than a constant.
func (a *Arm) LdrRegIdx(rt, rn, rm Reg64) {
	a.emit(0xF8607800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rt))
}

// OrrReg emits ORR Xd, Xn, Xm.
func (a *Arm) OrrReg(rd, rn, rm Reg64) {
	a.emit(0xAA000000 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// AddRegLSL3 emits ADD Xd, Xn, Xm, LSL #3 -- advancing a column base by a row
// count.
func (a *Arm) AddRegLSL3(rd, rn, rm Reg64) {
	a.emit(0x8B000000 | uint32(rm)<<16 | 3<<10 | uint32(rn)<<5 | uint32(rd))
}

// NegReg emits NEG Xd, Xm (SUB Xd, XZR, Xm).
func (a *Arm) NegReg(rd, rm Reg64) {
	a.emit(0xCB000000 | uint32(rm)<<16 | uint32(xzr)<<5 | uint32(rd))
}

// AddsReg emits ADDS Xd, Xn, Xm: add AND set the flags, so a following CSET can
// read the signed-overflow bit.
func (a *Arm) AddsReg(rd, rn, rm Reg64) {
	a.emit(0xAB000000 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// CsetOverflow emits CSET Xd, VS -- 1 when the last flag-setting instruction
// signed-overflowed.
//
// CSET encodes the INVERTED condition, so VS (0b0110) is written as VC
// (0b0111). armCond has no case for either: no comparison produces them, and
// putting them in the portable Cond set would make them look like orderings.
func (a *Arm) CsetOverflow(rd Reg64) {
	a.emit(0x9A9F07E0 | 0x7<<12 | uint32(rd))
}

// MovImm64 emits the MOVZ/MOVK sequence for an arbitrary 64-bit constant.
// AArch64 has no 64-bit immediate form, so a constant arrives sixteen bits at a
// time -- and the halves that are zero are skipped, which makes a small
// constant one instruction.
func (a *Arm) MovImm64(rd Reg64, imm int64) {
	v := uint64(imm)
	a.emit(0xD2800000 | uint32(v&0xFFFF)<<5 | uint32(rd)) // MOVZ, hw=0
	for hw := uint32(1); hw < 4; hw++ {
		part := uint32((v >> (16 * hw)) & 0xFFFF)
		if part == 0 {
			continue
		}
		a.emit(0xF2800000 | hw<<21 | part<<5 | uint32(rd)) // MOVK
	}
}

// Cbnz emits CBNZ Xt, label -- branch when the register is NOT zero.
func (a *Arm) Cbnz(rt Reg64, label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixCond})
	a.emit(0xB5000000 | uint32(rt))
}

// B emits an unconditional branch, whose displacement is imm26 rather than the
// conditional forms' imm19.
func (a *Arm) B(label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixBranch26})
	a.emit(0x14000000)
}

// SubsReg emits SUBS Xd, Xn, Xm: subtract and set the flags.
func (a *Arm) SubsReg(rd, rn, rm Reg64) {
	a.emit(0xEB000000 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// Mul emits MUL Xd, Xn, Xm (MADD with XZR as the addend).
func (a *Arm) Mul(rd, rn, rm Reg64) {
	a.emit(0x9B000000 | uint32(rm)<<16 | uint32(xzr)<<10 | uint32(rn)<<5 | uint32(rd))
}

// Smulh emits SMULH Xd, Xn, Xm -- the HIGH 64 bits of a signed 64x64 product.
//
// AArch64 has no flag-setting multiply, so overflow is detected by comparing
// this against the low half's sign extension: they agree exactly when the
// product fits in 64 bits.
func (a *Arm) Smulh(rd, rn, rm Reg64) {
	a.emit(0x9B407C00 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// CmpAsr63 emits CMP Xn, Xm, ASR #63 -- comparing a value against another's
// sign bit replicated, which is the multiply-overflow test above.
func (a *Arm) CmpAsr63(rn, rm Reg64) {
	a.emit(0xEB800000 | uint32(rm)<<16 | 63<<10 | uint32(rn)<<5 | uint32(xzr))
}

// --- What the VM emitter needs beyond the program emitter (vm_arm64.go):
// byte loads for a Value's type, register-offset forms for a register file
// larger than an immediate reaches, an indirect branch for the dispatch table,
// and the two shifts by a register.

// LdrbImm emits LDRB Wt, [Xn, #off] for an unscaled off below 4096.
func (a *Arm) LdrbImm(rt, rn Reg64, off int) {
	a.emit(0x39400000 | uint32(off)<<10 | uint32(rn)<<5 | uint32(rt))
}

// LdrbReg emits LDRB Wt, [Xn, Xm].
func (a *Arm) LdrbReg(rt, rn, rm Reg64) {
	a.emit(0x38606800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rt))
}

// LdrReg emits LDR Xt, [Xn, Xm].
func (a *Arm) LdrReg(rt, rn, rm Reg64) {
	a.emit(0xF8606800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rt))
}

// StrReg emits STR Xt, [Xn, Xm].
func (a *Arm) StrReg(rt, rn, rm Reg64) {
	a.emit(0xF8206800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rt))
}

// LdrswRegLSL2 emits LDRSW Xt, [Xn, Xm, LSL #2] -- a dispatch table's int32
// entry, sign-extended.
func (a *Arm) LdrswRegLSL2(rt, rn, rm Reg64) {
	a.emit(0xB8A07800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rt))
}

// Adr emits ADR Xd, label. Its displacement is in bytes, but a label is always
// on an instruction boundary, so immlo is zero and immhi sits exactly where the
// conditional branches keep their imm19.
func (a *Arm) Adr(rd Reg64, label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixCond})
	a.emit(0x10000000 | uint32(rd))
}

// Br emits BR Xn.
func (a *Arm) Br(rn Reg64) { a.emit(0xD61F0000 | uint32(rn)<<5) }

// CmpImm emits CMP Xn, #imm (SUBS XZR, Xn, #imm12).
func (a *Arm) CmpImm(rn Reg64, imm uint32) {
	a.emit(0xF1000000 | imm<<10 | uint32(rn)<<5 | uint32(xzr))
}

// Lslv emits LSL Xd, Xn, Xm; Asrv emits ASR Xd, Xn, Xm. Both take the amount
// modulo 64, which is why the caller range-checks it first.
func (a *Arm) Lslv(rd, rn, rm Reg64) {
	a.emit(0x9AC02000 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

func (a *Arm) Asrv(rd, rn, rm Reg64) {
	a.emit(0x9AC02800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// BcondRaw emits B.cond with an AArch64 condition code no portable Cond has:
// VS (0x6) after an add or subtract, HI (0x8) for the unsigned range checks.
func (a *Arm) BcondRaw(code uint32, label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixCond})
	a.emit(0x54000000 | code)
}

const (
	armVS = 0x6 // signed overflow
	armHI = 0x8 // unsigned higher
)
