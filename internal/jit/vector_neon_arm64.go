//go:build arm64

package jit

import "fmt"

// The vector-search kernels in NEON, the twins of the AVX2 ones in
// vector_avx2_amd64.go: same Args, same answers to the bit. A lane holds one
// ROW (.4S: four rows), each row's sums are accumulated in component order,
// and products and sums are separate FMUL and FADD -- never FMLA, which would
// round once where libSQL's float32 loop rounds twice.
//
// The strided kernel needs no transpose: LD4 (single structure, one lane)
// loads four consecutive floats of one row into the same lane of four
// registers, so four of them, one per row, leave component k of four rows in
// register k. Instruction words were checked against clang's assembler.

func (a *Arm) neon(w uint32) { a.emit(w) }

func v3(base uint32, d, n, m VReg) uint32 {
	return base | uint32(m)<<16 | uint32(n)<<5 | uint32(d)
}
func v2(base uint32, d, n VReg) uint32 { return base | uint32(n)<<5 | uint32(d) }

func (a *Arm) fmul4s(d, n, m VReg) { a.neon(v3(0x6E20DC00, d, n, m)) }
func (a *Arm) fadd4s(d, n, m VReg) { a.neon(v3(0x4E20D400, d, n, m)) }
func (a *Arm) fsub4s(d, n, m VReg) { a.neon(v3(0x4EA0D400, d, n, m)) }

// fmulElem4s is  d = n * m.s[idx], lane-wise.
func (a *Arm) fmulElem4s(d, n, m VReg, idx int) {
	a.neon(v3(0x4F809000, d, n, m) | uint32(idx&1)<<21 | uint32(idx>>1)<<11)
}

// dupElem4s is  d = m.s[idx] in every lane.
func (a *Arm) dupElem4s(d, m VReg, idx int) { a.neon(v2(0x4E040400|uint32(idx)<<19, d, m)) }

// ld4Lane is  LD4 {t, t+1, t+2, t+3}.S[lane], [n], #16.
func (a *Arm) ld4Lane(t VReg, lane int, n Reg64) {
	q, s := uint32(lane>>1), uint32(lane&1)
	a.neon(0x0DFFA000 | q<<30 | s<<12 | uint32(n)<<5 | uint32(t))
}

func (a *Arm) movi0(d VReg)               { a.neon(0x4F00E400 | uint32(d)) }
func (a *Arm) strQPost(t VReg, n Reg64)   { a.neon(0x3C810400 | uint32(n)<<5 | uint32(t)) }
func (a *Arm) strDPost(t VReg, n Reg64)   { a.neon(0xFC008400 | uint32(n)<<5 | uint32(t)) }
func (a *Arm) strWPost(t, n Reg64)        { a.neon(0xB8004400 | uint32(n)<<5 | uint32(t)) }
func (a *Arm) ld1rPost4s(t VReg, n Reg64) { a.neon(0x4DDFC800 | uint32(n)<<5 | uint32(t)) }
func (a *Arm) fmovWS(d Reg64, n VReg)     { a.neon(0x1E260000 | uint32(n)<<5 | uint32(d)) }
func (a *Arm) addLSL3(d, n, m Reg64)      { a.neon(0x8B000C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(d)) }
func (a *Arm) addvS(d, n VReg)            { a.neon(v2(0x4EB1B800, d, n)) }
func (a *Arm) smaxvS(d, n VReg)           { a.neon(v2(0x4EB0A800, d, n)) }
func (a *Arm) smax4s(d, n, m VReg)        { a.neon(v3(0x4EA06400, d, n, m)) }
func (a *Arm) add4s(d, n, m VReg)         { a.neon(v3(0x4EA08400, d, n, m)) }
func (a *Arm) and16b(d, n, m VReg)        { a.neon(v3(0x4E201C00, d, n, m)) }
func (a *Arm) abs4s(d, n VReg)            { a.neon(v2(0x4EA0B800, d, n)) }
func (a *Arm) fcvtns4s(d, n VReg)         { a.neon(v2(0x4E21A800, d, n)) }
func (a *Arm) sxtl8h(d, n VReg)           { a.neon(v2(0x0F08A400, d, n)) }
func (a *Arm) sxtl2_8h(d, n VReg)         { a.neon(v2(0x4F08A400, d, n)) }
func (a *Arm) smlal4s(d, n, m VReg)       { a.neon(v3(0x0E608000, d, n, m)) }
func (a *Arm) smlal2_4s(d, n, m VReg)     { a.neon(v3(0x4E608000, d, n, m)) }
func (a *Arm) sqxtn4h(d, n VReg)          { a.neon(v2(0x0E614800, d, n)) }
func (a *Arm) sqxtn2_8h(d, n VReg)        { a.neon(v2(0x4E614800, d, n)) }
func (a *Arm) sqxtn8b(d, n VReg)          { a.neon(v2(0x0E214800, d, n)) }
func (a *Arm) mvniSignMask(d VReg)        { a.neon(0x6F046400 | uint32(d)) } // 0x7fffffff per lane

// EmitVecDistStrided is the strided distance kernel (see the AVX2 twin):
// eight rows per iteration as two tiles of four. Args as on amd64; XA a
// multiple of 4, N a multiple of 8.
func EmitVecDistStrided(l2 bool) ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffXC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LsrImm(X4, X4, 2) // blocks of four components
	a.LdrImm(X6, X0, OffOut)
	a.LdrImm(X7, X0, OffOut2)
	rows := [8]Reg64{X8, X9, X10, X11, X12, X13, X14, X15}

	a.Label("tile")
	a.MovReg(X8, X1)
	for r := 1; r < 8; r++ {
		a.AddReg(rows[r], rows[r-1], X2)
	}
	a.LdrImm(X16, X0, OffV)
	a.MovReg(X17, X4)
	for _, v := range []VReg{24, 25, 26, 27} {
		a.movi0(v)
	}

	a.Label("block")
	for r := range 4 {
		a.ld4Lane(0, r, rows[r])   // v0..v3: components of rows 0..3
		a.ld4Lane(4, r, rows[4+r]) // v4..v7: of rows 4..7
	}
	a.LdrQPost(16, X16, 16) // the query's four components
	for k := range 4 {
		ca, cb := VReg(k), VReg(4+k)
		if l2 {
			a.dupElem4s(17, 16, k)
			a.fsub4s(8, ca, 17)
			a.fsub4s(10, cb, 17)
			a.fmul4s(8, 8, 8)
			a.fmul4s(10, 10, 10)
			a.fadd4s(24, 24, 8)
			a.fadd4s(25, 25, 10)
		} else {
			a.fmulElem4s(8, ca, 16, k)
			a.fmulElem4s(10, cb, 16, k)
			a.fmul4s(9, ca, ca)
			a.fmul4s(11, cb, cb)
			a.fadd4s(24, 24, 8)
			a.fadd4s(25, 25, 10)
			a.fadd4s(26, 26, 9)
			a.fadd4s(27, 27, 11)
		}
	}
	a.SubsImm(X17, X17, 1)
	a.Bcond(CondNE, "block")

	a.strQPost(24, X6)
	a.strQPost(25, X6)
	if !l2 {
		a.strQPost(26, X7)
		a.strQPost(27, X7)
	}
	a.addLSL3(X1, X1, X2) // the next tile: eight strides on
	a.SubsImm(X3, X3, 8)
	a.Bcond(CondNE, "tile")
	a.Ret()
	return a.Code()
}

// EmitVecDist has no NEON twin: NEON has no gather. Rows not stored back to
// back take the engine's Go tiles.
func EmitVecDist(l2 bool) ([]byte, error) {
	return nil, fmt.Errorf("jit: no gathering vector kernel on arm64")
}

// EmitI8Dot is the int8 dot-product kernel: sixteen codes per step widened to
// int16 (SXTL, SXTL2) and multiply-accumulated into int32 lanes (SMLAL,
// SMLAL2). Args as on amd64; XA a multiple of 16.
func EmitI8Dot() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LsrImm(X4, X4, 4)
	a.LdrImm(X6, X0, OffOut)
	a.Label("row")
	a.movi0(3)
	a.MovReg(X16, X2)
	a.MovReg(X17, X4)
	a.Label("block")
	a.LdrQPost(0, X1, 16)
	a.sxtl8h(1, 0)
	a.sxtl2_8h(2, 0)
	a.LdrQPost(4, X16, 16)
	a.LdrQPost(5, X16, 16)
	a.smlal4s(3, 1, 4)
	a.smlal2_4s(3, 1, 4)
	a.smlal4s(3, 2, 5)
	a.smlal2_4s(3, 2, 5)
	a.SubsImm(X17, X17, 1)
	a.Bcond(CondNE, "block")
	a.addvS(3, 3)
	a.fmovWS(X9, 3)
	a.strWPost(X9, X6)
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "row")
	a.Ret()
	return a.Code()
}

// EmitMaxAbsBits is each row's largest |component| as float32 bits, a signed
// integer max over the bits with the sign masked (see the AVX2 twin). Args as
// on amd64; XA a multiple of 4.
func EmitMaxAbsBits() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffXC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LsrImm(X4, X4, 2)
	a.LdrImm(X6, X0, OffOut)
	a.mvniSignMask(7)
	a.Label("row")
	a.movi0(0)
	a.MovReg(X16, X1)
	a.MovReg(X17, X4)
	a.Label("block")
	a.LdrQPost(1, X16, 16)
	a.and16b(1, 1, 7)
	a.smax4s(0, 0, 1)
	a.SubsImm(X17, X17, 1)
	a.Bcond(CondNE, "block")
	a.smaxvS(0, 0)
	a.fmovWS(X9, 0)
	a.strWPost(X9, X6)
	a.AddReg(X1, X1, X2)
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "row")
	a.Ret()
	return a.Code()
}

// EmitQuantize is each row's codes, FCVTNS(x * 1/scale) -- round to nearest,
// ties to even, as VCVTPS2DQ and math.RoundToEven -- narrowed to int8 with
// saturation, and the row's sum of |code| (see the AVX2 twin). Args as on
// amd64; XA a multiple of 8.
func EmitQuantize() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffXC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LsrImm(X4, X4, 3)
	a.LdrImm(X5, X0, OffV)
	a.LdrImm(X6, X0, OffOut)
	a.LdrImm(X7, X0, OffOut2)
	a.Label("row")
	a.ld1rPost4s(16, X5)
	a.movi0(4)
	a.MovReg(X16, X1)
	a.MovReg(X17, X4)
	a.Label("block")
	a.LdrQPost(0, X16, 16)
	a.LdrQPost(1, X16, 16)
	a.fmul4s(0, 0, 16)
	a.fmul4s(1, 1, 16)
	a.fcvtns4s(0, 0)
	a.fcvtns4s(1, 1)
	a.abs4s(2, 0)
	a.add4s(4, 4, 2)
	a.abs4s(2, 1)
	a.add4s(4, 4, 2)
	a.sqxtn4h(0, 0)
	a.sqxtn2_8h(0, 1)
	a.sqxtn8b(0, 0)
	a.strDPost(0, X6)
	a.SubsImm(X17, X17, 1)
	a.Bcond(CondNE, "block")
	a.addvS(4, 4)
	a.fmovWS(X9, 4)
	a.strWPost(X9, X7)
	a.AddReg(X1, X1, X2)
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "row")
	a.Ret()
	return a.Code()
}
