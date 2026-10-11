//go:build arm64

package jit

// The vec1 kernels in NEON, the twins of vec1_avx2_amd64.go: the same Args,
// the same layouts, the same answers to the bit. A lane holds a row (.4S:
// four rows, .2D: two), a row's sums are accumulated in component order, and
// where a sum must be vec1's portable C to the bit products and sums are
// separate FMUL and FADD. FMLA is used only where it rounds as the separate
// pair does (a float32 difference widened and squared in float64 is exact,
// so the fused add rounds once, as the plain add does) or where the result
// is an estimate (EmitVec1Cand). Instruction words were checked against
// llvm-mc.

func (a *Arm) ldpQPost(t1, t2 VReg, n Reg64, imm int) {
	a.neon(0xACC00000 | uint32(imm/16&0x7F)<<15 | uint32(t2)<<10 | uint32(n)<<5 | uint32(t1))
}
func (a *Arm) ldpQ(t1, t2 VReg, n Reg64, off int) {
	a.neon(0xAD400000 | uint32(off/16&0x7F)<<15 | uint32(t2)<<10 | uint32(n)<<5 | uint32(t1))
}
func (a *Arm) stpQPost(t1, t2 VReg, n Reg64, imm int) {
	a.neon(0xAC800000 | uint32(imm/16&0x7F)<<15 | uint32(t2)<<10 | uint32(n)<<5 | uint32(t1))
}
func (a *Arm) ldrQOff(t VReg, n Reg64, off int) {
	a.neon(0x3DC00000 | uint32(off/16)<<10 | uint32(n)<<5 | uint32(t))
}
func (a *Arm) strSOff(t VReg, n Reg64, off int) {
	a.neon(0xBD000000 | uint32(off/4)<<10 | uint32(n)<<5 | uint32(t))
}
func (a *Arm) strbPost1(t, n Reg64) { a.neon(0x38001400 | uint32(n)<<5 | uint32(t)) }

// ld1Lane is  LD1 {t.S}[lane], [n].
func (a *Arm) ld1Lane(t VReg, lane int, n Reg64) {
	base := [4]uint32{0x0D408000, 0x0D409000, 0x4D408000, 0x4D409000}[lane]
	a.neon(base | uint32(n)<<5 | uint32(t))
}

// addShift is  ADD Xd, Xn, Xm, LSL #sh.
func (a *Arm) addShift(d, n, m Reg64, sh uint32) {
	a.neon(0x8B000000 | uint32(m)<<16 | sh<<10 | uint32(n)<<5 | uint32(d))
}

// subReg is  SUB Xd, Xn, Xm.
func (a *Arm) subReg(d, n, m Reg64) {
	a.neon(0xCB000000 | uint32(m)<<16 | uint32(n)<<5 | uint32(d))
}

func (a *Arm) fadd2d(d, n, m VReg)   { a.neon(v3(0x4E60D400, d, n, m)) }
func (a *Arm) fmul2d(d, n, m VReg)   { a.neon(v3(0x6E60DC00, d, n, m)) }
func (a *Arm) fmla2d(d, n, m VReg)   { a.neon(v3(0x4E60CC00, d, n, m)) }
func (a *Arm) fmla4s(d, n, m VReg)   { a.neon(v3(0x4E20CC00, d, n, m)) }
func (a *Arm) fminnm4s(d, n, m VReg) { a.neon(v3(0x4EA0C400, d, n, m)) }
func (a *Arm) fminnmv(d, n VReg)     { a.neon(v2(0x6EB0C800, d, n)) }
func (a *Arm) fcmge4s(d, n, m VReg)  { a.neon(v3(0x6E20E400, d, n, m)) }
func (a *Arm) fcmgt2d(d, n, m VReg)  { a.neon(v3(0x6EE0E400, d, n, m)) }
func (a *Arm) bit16b(d, n, m VReg)   { a.neon(v3(0x6EA01C00, d, n, m)) }
func (a *Arm) fcvtl(d, n VReg)       { a.neon(v2(0x0E617800, d, n)) }
func (a *Arm) fcvtl2(d, n VReg)      { a.neon(v2(0x4E617800, d, n)) }
func (a *Arm) fcvtn(d, n VReg)       { a.neon(v2(0x0E616800, d, n)) }
func (a *Arm) rev64_4s(d, n VReg)    { a.neon(v2(0x4EA00800, d, n)) }
func (a *Arm) trn1_4s(d, n, m VReg)  { a.neon(v3(0x4E802800, d, n, m)) }
func (a *Arm) zip1_2d(d, n, m VReg)  { a.neon(v3(0x4EC03800, d, n, m)) }
func (a *Arm) ext8(d, n, m VReg)     { a.neon(v3(0x6E004000, d, n, m)) } // #8
func (a *Arm) movV(d, n VReg)        { a.neon(v3(0x4EA01C00, d, n, n)) }

// EmitVec1T8 is the twin of the AVX2 EmitVec1T8: the same Args (N groups of
// four blocks, two for Vec1L2D).
func EmitVec1T8(op Vec1Op) ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffXC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LdrImm(X5, X0, OffOut)
	blocks := []Reg64{X7, X8, X9, X10}
	nb := 4
	if op == Vec1L2D {
		nb = 2
	}
	a.Label("group")
	a.MovReg(X7, X1)
	for b := 1; b < nb; b++ {
		a.AddReg(blocks[b], blocks[b-1], X2)
	}
	for r := VReg(16); r < 32; r++ {
		a.movi0(r)
	}
	a.LdrImm(X11, X0, OffV)
	a.MovReg(X12, X4)
	a.Label("comp")
	a.ld1rPost4s(0, X11)
	for b := 0; b < nb; b++ {
		a.ldpQPost(1, 2, blocks[b], 32)
		switch op {
		case Vec1L2F:
			lo, hi := VReg(16+2*b), VReg(17+2*b)
			a.fsub4s(1, 0, 1)
			a.fsub4s(2, 0, 2)
			a.fmul4s(1, 1, 1)
			a.fmul4s(2, 2, 2)
			a.fadd4s(lo, lo, 1)
			a.fadd4s(hi, hi, 2)
		case Vec1DotF:
			lo, hi := VReg(16+2*b), VReg(17+2*b)
			a.fmul4s(1, 1, 0)
			a.fmul4s(2, 2, 0)
			a.fadd4s(lo, lo, 1)
			a.fadd4s(hi, hi, 2)
		case Vec1L2D:
			// Rows 0..7 of the block in four .2D sums.
			acc := VReg(16 + 4*b)
			a.fsub4s(1, 0, 1)
			a.fsub4s(2, 0, 2)
			a.fcvtl(3, 1)
			a.fcvtl2(4, 1)
			a.fcvtl(5, 2)
			a.fcvtl2(6, 2)
			a.fmla2d(acc, 3, 3)
			a.fmla2d(acc+1, 4, 4)
			a.fmla2d(acc+2, 5, 5)
			a.fmla2d(acc+3, 6, 6)
		}
	}
	a.SubsImm(X12, X12, 1)
	a.Bcond(CondNE, "comp")
	for r := VReg(16); r < 24; r += 2 {
		a.stpQPost(r, r+1, X5, 32)
	}
	a.MovReg(X1, blocks[nb-1]) // past the group's last block
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "group")
	a.Ret()
	return a.Code()
}

// EmitVec1Scan16 is the twin of the AVX2 EmitVec1Scan16. NEON has no gather:
// each code's table entry is loaded into its lane with LD1 (single lane), and
// the sixteen sums are then added a book at a time, as the AVX2 kernel adds
// its gathered registers.
func EmitVec1Scan16() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffXC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LdrImm(X5, X0, OffOut)
	a.MovImm16(X15, 1024)
	a.Label("blk")
	for r := VReg(16); r < 20; r++ {
		a.movi0(r)
	}
	a.MovReg(X7, X1)
	a.LdrImm(X8, X0, OffV)
	a.MovReg(X9, X4)
	a.Label("book")
	for j := range 16 {
		a.LdrbImm(X10, X7, j)
		a.addShift(X11, X8, X10, 2)
		a.ld1Lane(VReg(j/4), j%4, X11)
	}
	for k := VReg(0); k < 4; k++ {
		a.fadd4s(16+k, 16+k, k)
	}
	a.AddImm(X7, X7, 16)
	a.AddReg(X8, X8, X15)
	a.SubsImm(X9, X9, 1)
	a.Bcond(CondNE, "book")
	a.stpQPost(16, 17, X5, 32)
	a.stpQPost(18, 19, X5, 32)
	a.AddReg(X1, X1, X2)
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "blk")
	a.Ret()
	return a.Code()
}

// EmitVec1RowsL2D is the twin of the AVX2 EmitVec1RowsL2D: tiles of eight
// rows, each four rows' components brought into lanes by LD4 (one lane), as
// EmitVecDistStrided does.
func EmitVec1RowsL2D() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffXC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LsrImm(X4, X4, 2) // blocks of four components
	a.LdrImm(X6, X0, OffOut)
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
		a.ld4Lane(0, r, rows[r])
		a.ld4Lane(4, r, rows[4+r])
	}
	a.LdrQPost(16, X16, 16)
	for k := range 4 {
		ca, cb := VReg(k), VReg(4+k)
		a.dupElem4s(17, 16, k)
		a.fsub4s(8, 17, ca)
		a.fsub4s(9, 17, cb)
		a.fcvtl(10, 8)
		a.fcvtl2(11, 8)
		a.fcvtl(12, 9)
		a.fcvtl2(13, 9)
		a.fmla2d(24, 10, 10)
		a.fmla2d(25, 11, 11)
		a.fmla2d(26, 12, 12)
		a.fmla2d(27, 13, 13)
	}
	a.SubsImm(X17, X17, 1)
	a.Bcond(CondNE, "block")
	a.stpQPost(24, 25, X6, 32)
	a.stpQPost(26, 27, X6, 32)
	a.addLSL3(X1, X1, X2)
	a.SubsImm(X3, X3, 8)
	a.Bcond(CondNE, "tile")
	a.Ret()
	return a.Code()
}

// EmitVec1BestD is the twin of the AVX2 EmitVec1BestD.
func EmitVec1BestD(round bool) ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffXC)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LdrImm(X15, X0, OffC)
	// V8..V11 the lanes' minima, V12..V15 their rows, V20..V23 the rows of the
	// block at hand, V24 the step between blocks.
	a.ldpQ(20, 21, X15, 0)
	a.ldpQ(22, 23, X15, 32)
	a.ldrQOff(24, X15, 64)
	a.ldrQOff(8, X15, 96)
	for r := VReg(9); r < 12; r++ {
		a.movV(r, 8)
	}
	for r := VReg(12); r < 16; r++ {
		a.movi0(r)
	}
	a.Label("pair")
	a.MovReg(X7, X1)
	a.AddReg(X8, X1, X2)
	for r := VReg(16); r < 20; r++ {
		a.movi0(r)
	}
	for r := VReg(26); r < 30; r++ {
		a.movi0(r)
	}
	a.LdrImm(X11, X0, OffV)
	a.MovReg(X12, X4)
	a.Label("comp")
	a.ld1rPost4s(0, X11)
	for b, p := range []Reg64{X7, X8} {
		acc := []VReg{16, 26}[b]
		a.ldpQPost(1, 2, p, 32)
		a.fsub4s(1, 0, 1)
		a.fsub4s(2, 0, 2)
		a.fcvtl(3, 1)
		a.fcvtl2(4, 1)
		a.fcvtl(5, 2)
		a.fcvtl2(6, 2)
		a.fmla2d(acc, 3, 3)
		a.fmla2d(acc+1, 4, 4)
		a.fmla2d(acc+2, 5, 5)
		a.fmla2d(acc+3, 6, 6)
	}
	a.SubsImm(X12, X12, 1)
	a.Bcond(CondNE, "comp")
	for _, acc := range []VReg{16, 26} {
		for i := VReg(0); i < 4; i++ {
			s := acc + i
			if round {
				a.fcvtn(1, s)
				a.fcvtl(s, 1)
			}
			a.fcmgt2d(1, 8+i, s) // the minimum is larger: s is the new one
			a.bit16b(8+i, s, 1)
			a.bit16b(12+i, 20+i, 1)
		}
		for i := VReg(0); i < 4; i++ {
			a.fadd2d(20+i, 20+i, 24)
		}
	}
	a.MovReg(X1, X8) // past the second block
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "pair")
	a.LdrImm(X5, X0, OffOut)
	a.LdrImm(X6, X0, OffOut2)
	a.stpQPost(8, 9, X5, 32)
	a.stpQPost(10, 11, X5, 32)
	a.stpQPost(12, 13, X6, 32)
	a.stpQPost(14, 15, X6, 32)
	a.Ret()
	return a.Code()
}

// candMasks writes one vector's bit sets: for each block, a byte with bit l
// set when lane l's estimate is no larger than the bound in vb. X13 walks
// the estimates (128 bytes a block, this vector's 32 at its offset), X16 the
// output; X14 counts blocks. V28/V29 hold the lane weights 1,2,4,8 and
// 16,32,64,128.
func (a *Arm) candMasks(vb VReg, label string) {
	a.Label(label)
	a.ldpQ(0, 1, X13, 0)
	a.fcmge4s(0, vb, 0)
	a.fcmge4s(1, vb, 1)
	a.and16b(0, 0, 28)
	a.and16b(1, 1, 29)
	a.add4s(0, 0, 1)
	a.addvS(0, 0)
	a.fmovWS(X17, 0)
	a.strbPost1(X17, X16)
	a.AddImm(X13, X13, 128)
	a.SubsImm(X14, X14, 1)
	a.Bcond(CondNE, label)
}

// EmitVec1Cand is the twin of the AVX2 EmitVec1Cand. Its scratch also holds,
// at byte 96, the uint32 lane weights 1,2,4,8,16,32,64,128 a bit set is made
// from (NEON has no movemask).
func EmitVec1Cand() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X4, X0, OffN)
	a.LdrImm(X5, X0, OffXC)
	a.LdrImm(X6, X0, OffOut)
	a.LdrImm(X7, X0, OffV)
	a.LdrImm(X15, X0, OffC)
	a.LdrImm(X3, X0, OffXA) // a block's bytes: (XA+1) * 32
	a.AddImm(X3, X3, 1)
	for range 5 {
		a.AddReg(X3, X3, X3)
	}
	a.ldrQOff(13, X15, 64) // -2
	a.ldpQ(28, 29, X15, 96)

	a.Label("set")
	a.vec1CandPairs("pair", "comp", X1, X15)
	// Each minimum across its lanes, stored as lo, then the bound.
	for vv := range 4 {
		lo, hi := VReg(5+2*vv), VReg(6+2*vv)
		a.fminnm4s(lo, lo, hi)
		a.fminnmv(lo, lo)
		a.strSOff(lo, X6, 4*vv)
		a.dupElem4s(lo, lo, 0)
		a.ldrQOff(0, X15, 128+32*vv) // A
		a.fadd4s(lo, lo, 0)
		a.ldrQOff(0, X15, 32) // F
		a.fmul4s(lo, lo, 0)
	}
	a.AddImm(X16, X6, 16)
	for vv := range 4 {
		a.AddImm(X13, X15, uint32(256+32*vv))
		a.LdrImm(X14, X15, 0) // B
		a.candMasks(VReg(5+2*vv), "mask"+string(rune('0'+vv)))
	}
	a.MovReg(X6, X16) // the next set's record
	a.LdrImm(X17, X0, OffXA)
	a.addShift(X7, X7, X17, 2) // the next components
	a.SubsImm(X4, X4, 1)
	a.Bcond(CondNE, "set")
	a.Ret()
	return a.Code()
}

// vec1CandPairs is EmitVec1Cand's pass over one set: from X1 (advanced past
// the set), the vectors at X7 (stride X5), estimates written from scratch
// byte 256 on, the running minima in V5..V12 (two registers a vector).
func (a *Arm) vec1CandPairs(pair, comp string, set, scratch Reg64) {
	for r := VReg(5); r < 13; r++ {
		a.dupInf(r)
	}
	a.AddImm(X13, scratch, 256)
	a.LdrImm(X14, scratch, 0)
	a.LsrImm(X14, X14, 1)
	a.Label(pair)
	a.AddReg(X2, set, X3)
	for vv := range 4 {
		for _, r := range candAcc(vv) {
			a.movi0(r)
		}
	}
	a.vec1CandBody(comp, set)
	// The rows' squared norms, and the estimates.
	a.ldpQPost(0, 1, set, 32)
	a.ldpQPost(2, 3, X2, 32)
	a.vec1CandFinish()
	a.MovReg(set, X2) // the next pair
	a.SubsImm(X14, X14, 1)
	a.Bcond(CondNE, pair)
}

// dupInf sets every lane of d to +Inf: MOVZ X17, #0x7F80, LSL #16, then DUP.
func (a *Arm) dupInf(d VReg) {
	a.neon(0xD2A00000 | 0x7F80<<5 | uint32(X17))
	a.Dup4s(d, X17)
}

// Dup4s is  DUP Vd.4S, Wn.
func (a *Arm) Dup4s(d VReg, n Reg64) { a.neon(0x4E040C00 | uint32(n)<<5 | uint32(d)) }

// vec1CandBody accumulates the dot products of the four vectors (X7, stride
// X5) with the pair of blocks at set and X2, sixteen sums in V16..V31 less
// V28/V29: vector vv's in V16+4vv.. for the first block's two registers and
// the second's -- remapped around the weights.
func (a *Arm) vec1CandBody(comp string, set Reg64) {
	a.MovReg(X8, X7)
	a.AddReg(X9, X8, X5)
	a.AddReg(X10, X9, X5)
	a.AddReg(X11, X10, X5)
	a.LdrImm(X12, X0, OffXA)
	a.Label(comp)
	a.ldpQPost(0, 1, set, 32)
	a.ldpQPost(2, 3, X2, 32)
	for vv, p := range []Reg64{X8, X9, X10, X11} {
		a.ld1rPost4s(4, p)
		acc := candAcc(vv)
		a.fmla4s(acc[0], 0, 4)
		a.fmla4s(acc[1], 1, 4)
		a.fmla4s(acc[2], 2, 4)
		a.fmla4s(acc[3], 3, 4)
	}
	a.SubsImm(X12, X12, 1)
	a.Bcond(CondNE, comp)
}

// candAcc is the four registers vector vv's sums are in: the first block's
// rows 0..3 and 4..7, then the second block's. V28 and V29 hold the weights.
func candAcc(vv int) [4]VReg {
	regs := []VReg{16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 30, 31, 14, 15}
	return [4]VReg{regs[4*vv], regs[4*vv+1], regs[4*vv+2], regs[4*vv+3]}
}

// vec1CandFinish turns the sums into estimates (norm - 2 dot), with the
// norms in V0,V1 (first block) and V2,V3, stores them at X13 (vector-major
// per block, the first block's 128 bytes then the second's) and folds them
// into the minima V5..V12.
func (a *Arm) vec1CandFinish() {
	for vv := range 4 {
		acc := candAcc(vv)
		for i := range 4 {
			a.fmul4s(acc[i], acc[i], 13) // exact: a power of two
			a.fadd4s(acc[i], acc[i], VReg(i))
		}
	}
	for blk := range 2 {
		for vv := range 4 {
			acc := candAcc(vv)
			a.stpQPost(acc[2*blk], acc[2*blk+1], X13, 32)
		}
	}
	for vv := range 4 {
		acc := candAcc(vv)
		lo, hi := VReg(5+2*vv), VReg(6+2*vv)
		a.fminnm4s(lo, lo, acc[0])
		a.fminnm4s(hi, hi, acc[1])
		a.fminnm4s(lo, lo, acc[2])
		a.fminnm4s(hi, hi, acc[3])
	}
}

// EmitVec1CandMany is the twin of the AVX2 EmitVec1CandMany.
func EmitVec1CandMany() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X15, X0, OffC)
	a.LdrImm(X5, X0, OffXC)
	a.LdrImm(X3, X0, OffXA)
	a.AddImm(X3, X3, 1)
	for range 5 {
		a.AddReg(X3, X3, X3)
	}
	a.ldrQOff(13, X15, 64) // -2
	a.ldpQ(28, 29, X15, 96)
	// Every group's minima start at +Inf.
	a.dupInf(0)
	a.AddImm(X16, X15, 128)
	a.LdrImm(X17, X0, OffN)
	a.Label("init")
	a.stpQPost(0, 0, X16, 32)
	a.stpQPost(0, 0, X16, 32)
	a.stpQPost(0, 0, X16, 32)
	a.stpQPost(0, 0, X16, 32)
	a.SubsImm(X17, X17, 1)
	a.Bcond(CondNE, "init")
	// X16 is past the minima, at the A; the estimates follow those.
	a.LdrImm(X17, X0, OffN)
	a.addShift(X13, X16, X17, 7)
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X14, X15, 0)
	a.LsrImm(X14, X14, 1)

	a.Label("pair")
	a.LdrImm(X7, X0, OffV)
	a.AddImm(X6, X15, 128) // group 0's minima
	a.LdrImm(X4, X0, OffN)
	a.AddReg(X2, X1, X3)
	a.Label("group")
	for vv := range 4 {
		for _, r := range candAcc(vv) {
			a.movi0(r)
		}
	}
	a.vec1CandBody("comp", X1)
	a.ldpQ(0, 1, X1, 0) // the rows' squared norms
	a.ldpQ(2, 3, X2, 0)
	// Back to the blocks' first components for the next group.
	a.subReg(X1, X1, X3)
	a.AddImm(X1, X1, 32)
	a.subReg(X2, X2, X3)
	a.AddImm(X2, X2, 32)
	// This group's minima into V5..V12, the estimates, the minima back.
	for r := VReg(5); r < 13; r += 2 {
		a.ldpQPost(r, r+1, X6, 32)
	}
	a.SubsImm(X6, X6, 128)
	a.vec1CandFinish()
	for r := VReg(5); r < 13; r += 2 {
		a.stpQPost(r, r+1, X6, 32)
	}
	a.addShift(X7, X7, X5, 2) // the next group's vectors
	a.SubsImm(X4, X4, 1)
	a.Bcond(CondNE, "group")
	a.AddReg(X1, X1, X3) // the next pair of blocks
	a.AddReg(X1, X1, X3)
	a.SubsImm(X14, X14, 1)
	a.Bcond(CondNE, "pair")

	// Per group: lo, the bound, the bit sets. X6 the group's minima, X7 its
	// A, X10 its estimates in the first pair, X11 the stride from pair to
	// pair (G * 256), X16 out.
	a.AddImm(X6, X15, 128)
	a.LdrImm(X17, X0, OffN)
	a.addShift(X7, X6, X17, 7)
	a.addShift(X10, X7, X17, 7)
	a.addShift(X11, xzr, X17, 8)
	a.LdrImm(X16, X0, OffOut)
	a.LdrImm(X4, X0, OffN)
	a.ldrQOff(30, X15, 32) // F
	a.Label("final")
	for vv := range 4 {
		lo := VReg(5 + 2*vv)
		a.ldpQ(lo, lo+1, X6, 32*vv)
		a.fminnm4s(lo, lo, lo+1)
		a.fminnmv(lo, lo)
		a.strSOff(lo, X16, 4*vv)
		a.dupElem4s(lo, lo, 0)
		a.ldrQOff(0, X7, 32*vv)
		a.fadd4s(lo, lo, 0)
		a.fmul4s(lo, lo, 30)
	}
	a.AddImm(X16, X16, 16)
	for vv := range 4 {
		a.AddImm(X13, X10, uint32(32*vv))
		a.LdrImm(X14, X15, 0)
		a.LsrImm(X14, X14, 1)
		lbl := "masks" + string(rune('0'+vv))
		a.Label(lbl)
		for blk := range 2 {
			a.ldpQ(0, 1, X13, 128*blk)
			a.fcmge4s(0, VReg(5+2*vv), 0)
			a.fcmge4s(1, VReg(5+2*vv), 1)
			a.and16b(0, 0, 28)
			a.and16b(1, 1, 29)
			a.add4s(0, 0, 1)
			a.addvS(0, 0)
			a.fmovWS(X17, 0)
			a.strbPost1(X17, X16)
		}
		a.AddReg(X13, X13, X11)
		a.SubsImm(X14, X14, 1)
		a.Bcond(CondNE, lbl)
	}
	a.AddImm(X6, X6, 128)
	a.AddImm(X7, X7, 128)
	a.AddImm(X10, X10, 256)
	a.SubsImm(X4, X4, 1)
	a.Bcond(CondNE, "final")
	a.Ret()
	return a.Code()
}

// EmitVec1WHT is the twin of the AVX2 EmitVec1WHT: strides 1 and 2 within a
// register (REV64 and EXT bring the partner alongside, TRN1 and ZIP1 keep
// the sum or the difference), the rest between registers.
func EmitVec1WHT() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X2, X0, OffN)
	a.addShift(X3, X1, X2, 2) // the end
	a.MovReg(X4, X1)
	a.Label("small")
	a.ldrQOff(0, X4, 0)
	a.rev64_4s(1, 0)
	a.fadd4s(2, 0, 1) // x0+x1 in lane 0
	a.fsub4s(3, 0, 1) // x0-x1 in lane 0
	a.trn1_4s(0, 2, 3)
	a.ext8(1, 0, 0)
	a.fadd4s(2, 0, 1) // x0+x2, x1+x3 in lanes 0, 1
	a.fsub4s(3, 0, 1) // x0-x2, x1-x3
	a.zip1_2d(0, 2, 3)
	a.strQPost(0, X4)
	a.Cmp(X4, X3)
	a.Bcond(CondL, "small")

	a.MovImm16(X5, 16) // the stride in bytes: four floats
	a.Label("stage")
	a.MovReg(X6, X1)
	a.Label("block")
	a.MovReg(X7, X6)
	a.AddReg(X8, X6, X5) // the block's second half
	a.MovReg(X9, X8)
	a.Label("bfly")
	a.ldrQOff(0, X7, 0)
	a.ldrQOff(1, X9, 0)
	a.fadd4s(2, 0, 1)
	a.fsub4s(3, 0, 1)
	a.strQPost(2, X7)
	a.strQPost(3, X9)
	a.Cmp(X7, X8)
	a.Bcond(CondL, "bfly")
	a.MovReg(X6, X9)
	a.Cmp(X6, X3)
	a.Bcond(CondL, "block")
	a.AddReg(X5, X5, X5)
	a.AddReg(X10, X1, X5)
	a.Cmp(X10, X3)
	a.Bcond(CondL, "stage")
	a.Ret()
	return a.Code()
}

// EmitVec1Rot4 is the twin of the AVX2 EmitVec1Rot4.
func EmitVec1Rot4() ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	a.LdrImm(X5, X0, OffXC)
	a.LdrImm(X6, X0, OffOut)
	a.addShift(X13, xzr, X4, 5) // a block's bytes: 32 * XA
	a.addShift(X14, xzr, X3, 6) // a result's bytes: 64 * N
	a.Label("pair")
	a.AddReg(X2, X1, X13)
	for vv := range 4 {
		for _, r := range candAcc(vv) {
			a.movi0(r)
		}
	}
	a.LdrImm(X8, X0, OffV)
	a.AddReg(X9, X8, X5)
	a.AddReg(X10, X9, X5)
	a.AddReg(X11, X10, X5)
	a.MovReg(X12, X4)
	a.Label("comp")
	a.ldpQPost(0, 1, X1, 32)
	a.ldpQPost(2, 3, X2, 32)
	for vv, p := range []Reg64{X8, X9, X10, X11} {
		a.ld1rPost4s(4, p)
		acc := candAcc(vv)
		for i := range 4 {
			a.fmul4s(VReg(5+i), VReg(i), 4)
		}
		for i := range 4 {
			a.fadd4s(acc[i], acc[i], VReg(5+i))
		}
	}
	a.SubsImm(X12, X12, 1)
	a.Bcond(CondNE, "comp")
	a.MovReg(X7, X6)
	for vv := range 4 {
		acc := candAcc(vv)
		a.stpQ(acc[0], acc[1], X7, 0)
		a.stpQ(acc[2], acc[3], X7, 32)
		a.AddReg(X7, X7, X14)
	}
	a.AddImm(X6, X6, 64)
	a.MovReg(X1, X2) // past the second block
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "pair")
	a.Ret()
	return a.Code()
}

func (a *Arm) stpQ(t1, t2 VReg, n Reg64, off int) {
	a.neon(0xAD000000 | uint32(off/16&0x7F)<<15 | uint32(t2)<<10 | uint32(n)<<5 | uint32(t1))
}
