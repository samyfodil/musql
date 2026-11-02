//go:build arm64

package jit

// Scalar double instructions for the float kernels (fir_arm64.go), all of
// them base AArch64. A VReg names the D view of a SIMD register.

func (a *Arm) fp3(op uint32, d, n, m VReg) {
	a.emit(op | uint32(m)<<16 | uint32(n)<<5 | uint32(d))
}

func (a *Arm) Fadd(d, n, m VReg) { a.fp3(0x1E602800, d, n, m) }
func (a *Arm) Fsub(d, n, m VReg) { a.fp3(0x1E603800, d, n, m) }
func (a *Arm) Fmul(d, n, m VReg) { a.fp3(0x1E600800, d, n, m) }
func (a *Arm) Fdiv(d, n, m VReg) { a.fp3(0x1E601800, d, n, m) }

func (a *Arm) Fsqrt(d, n VReg) { a.emit(0x1E61C000 | uint32(n)<<5 | uint32(d)) }
func (a *Arm) Fabs(d, n VReg)  { a.emit(0x1E60C000 | uint32(n)<<5 | uint32(d)) }
func (a *Arm) Fneg(d, n VReg)  { a.emit(0x1E614000 | uint32(n)<<5 | uint32(d)) }

// FmovD is  d = n.
func (a *Arm) FmovD(d, n VReg) { a.emit(0x1E604000 | uint32(n)<<5 | uint32(d)) }

// Fmadd is  d = ac + n*m, rounded once.
func (a *Arm) Fmadd(d, n, m, ac VReg) {
	a.emit(0x1F400000 | uint32(m)<<16 | uint32(ac)<<10 | uint32(n)<<5 | uint32(d))
}

// Frint is  d = n rounded: 0 nearest-even (FRINTN), 1 down (FRINTM), 2 up
// (FRINTP), 3 toward zero (FRINTZ).
func (a *Arm) Frint(d, n VReg, mode int) {
	op := [...]uint32{0x1E644000, 0x1E654000, 0x1E64C000, 0x1E65C000}[mode]
	a.emit(op | uint32(n)<<5 | uint32(d))
}

// FmovToD is  d = n's bits; FmovToX  d = n's.
func (a *Arm) FmovToD(d VReg, n Reg64) { a.emit(0x9E670000 | uint32(n)<<5 | uint32(d)) }
func (a *Arm) FmovToX(d Reg64, n VReg) { a.emit(0x9E660000 | uint32(n)<<5 | uint32(d)) }

// Scvtf is  d = float64(int64 n); Fcvtzs  d = int64(n), truncated.
func (a *Arm) Scvtf(d VReg, n Reg64)  { a.emit(0x9E620000 | uint32(n)<<5 | uint32(d)) }
func (a *Arm) Fcvtzs(d Reg64, n VReg) { a.emit(0x9E780000 | uint32(n)<<5 | uint32(d)) }

// Fcmp compares n with m: NZCV = 0011 when either is a NaN.
func (a *Arm) Fcmp(n, m VReg) { a.emit(0x1E602000 | uint32(m)<<16 | uint32(n)<<5) }

// FcmpZero compares n with +0.0.
func (a *Arm) FcmpZero(n VReg) { a.emit(0x1E602008 | uint32(n)<<5) }

// CsetRaw is  d = 1 when the raw condition code holds, else 0 (CSINC d,
// XZR, XZR, the inverted condition).
func (a *Arm) CsetRaw(d Reg64, code uint32) {
	a.emit(0x9A9F07E0 | (code^1)<<12 | uint32(d))
}

// LdrD is  t = the double at [n + off]; off is a multiple of 8 below 32768.
func (a *Arm) LdrD(t VReg, n Reg64, off int) {
	a.emit(0xFD400000 | uint32(off/8)<<10 | uint32(n)<<5 | uint32(t))
}

// StrD is  [n + off] = t.
func (a *Arm) StrD(t VReg, n Reg64, off int) {
	a.emit(0xFD000000 | uint32(off/8)<<10 | uint32(n)<<5 | uint32(t))
}

// LdrDIdx is  t = the double at [n + m*8].
func (a *Arm) LdrDIdx(t VReg, n, m Reg64) {
	a.emit(0xFC607800 | uint32(m)<<16 | uint32(n)<<5 | uint32(t))
}

// LdrDLit is  t = the double at label, within 1MB.
func (a *Arm) LdrDLit(t VReg, label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixCond})
	a.emit(0x5C000000 | uint32(t))
}

// LdrXLit is  t = the word at label.
func (a *Arm) LdrXLit(t Reg64, label string) {
	a.fixups = append(a.fixups, armFixup{at: len(a.buf), label: label, kind: fixCond})
	a.emit(0x58000000 | uint32(t))
}

func (a *Arm) EorReg(d, n, m Reg64) { a.emit(0xCA000000 | uint32(m)<<16 | uint32(n)<<5 | uint32(d)) }
func (a *Arm) SubReg(d, n, m Reg64) { a.emit(0xCB000000 | uint32(m)<<16 | uint32(n)<<5 | uint32(d)) }

// LslImm is  d = n << sh (UBFM); AsrImm  d = n >> sh, arithmetic (SBFM).
func (a *Arm) LslImm(d, n Reg64, sh uint32) {
	a.emit(0xD3400000 | ((64-sh)&63)<<16 | (63-sh)<<10 | uint32(n)<<5 | uint32(d))
}
func (a *Arm) AsrImm(d, n Reg64, sh uint32) {
	a.emit(0x9340FC00 | sh<<16 | uint32(n)<<5 | uint32(d))
}

// Raw AArch64 condition codes beyond those armCond translates.
const (
	armLO = 0x3 // unsigned lower
	armMI = 0x4 // negative: FCMP's "less than"
	armPL = 0x5 // not negative: FCMP's "not less than", unordered included
	armVC = 0x7
)
