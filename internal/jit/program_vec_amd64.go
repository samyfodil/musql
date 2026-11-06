//go:build amd64

package jit

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// EmitProgramVec compiles a program into a row loop four rows wide (AVX2),
// or reports why it cannot. It takes the program as the scalar emitter does
// and the same ProgArgs, and answers the first N rounded down to a multiple of
// four rows as the scalar loop would -- except that it sets Overflow wherever
// it cannot vouch for that: an add or subtract that overflows, a sum whose
// rows' absolute values add past int64 (some order of them might overflow),
// a multiply of an operand outside int32 (the lanes multiply 32 by 32 bits),
// a shift amount outside 0..63. The caller then runs those rows through the
// scalar loop, which knows exactly.
//
// A row's tests (POpSkipIfZero, POpCmpSkip, a jump to the next row) clear
// its lane in a row mask instead of branching: every step runs for all four
// rows, and the mask keeps a failed row out of every accumulate and every
// Overflow test. So the program must be one straight run, with no value
// carried from one row to the next but the accumulators', and steps of the
// kinds planVec takes.
func EmitProgramVec(insns []ProgInsn, nCols int) ([]byte, error) {
	if !cpuHasAVX2() {
		return nil, fmt.Errorf("jit: AVX2 not available on this machine")
	}
	if nCols > MaxProgCols {
		return nil, fmt.Errorf("jit: %d column blocks, max %d", nCols, MaxProgCols)
	}
	v, err := planVec(fuseStraight(insns), nCols)
	if err != nil {
		return nil, err
	}
	return v.emit()
}

// The vector registers with a fixed role; Y0 to Y8 are planVec's.
const (
	vS0     = 9
	vS1     = 10
	vS2     = 11
	vZero   = 12
	vUnsafe = 13 // a lane's sign bit set: these rows go to the scalar loop
	vMask   = 14 // all-ones in the lanes of the rows still in
	vS3     = 15 // also emitLaneCmp's scratch
	vPool   = 9
)

// vecPlan is a program as the vector loop runs it.
type vecPlan struct {
	insns []ProgInsn
	nCols int
	reg   map[int]Reg // IR register -> its vector register
	// in are the registers the loop reads and never writes: the caller's
	// constants and parameters, broadcast from the file where they are read.
	in map[int]bool
	// consts are the constants broadcast from the pool after the code.
	consts map[int64]bool
	// acc is the accumulator's vector register (POpAccCount, POpAccSum,
	// POpAccMin, POpAccMax), when hasAcc; accOp the step that feeds it.
	acc    Reg
	hasAcc bool
	accOp  POp
	// absSum is, per summing step, the lanes' running sums of absolute values.
	absSum map[int]Reg
	// counters are the registers only counted into (r[B] of POpAccSum and
	// kin, r[A] of POpRAccCount), raccs those only accumulated into
	// (POpRAccSum, POpRAccMin, POpRAccMax): each a vector register whose
	// lanes are combined with the file's value as the run ends.
	counters, raccs []int
	raccOp          map[int]POp
}

// vecConstMulCheck and vecConstShift are the constants the int32 test of a
// multiply and an arithmetic shift right read.
const (
	vecConstMulCheck = 1 << 31
	vecConstShift    = 64
)

func planVec(insns []ProgInsn, nCols int) (*vecPlan, error) {
	v := &vecPlan{insns: insns, nCols: nCols, reg: map[int]Reg{}, in: map[int]bool{}, consts: map[int64]bool{},
		absSum: map[int]Reg{}, raccOp: map[int]POp{}}
	next := Reg(0)
	take := func() (Reg, error) {
		if next >= vPool {
			return 0, fmt.Errorf("jit: more than %d vector registers", vPool)
		}
		next++
		return next - 1, nil
	}
	defined, read := map[int]bool{}, map[int]bool{}
	special := map[int]bool{} // counters and register accumulators
	count := func(r int) {
		special[r] = true
		if !slices.Contains(v.counters, r) {
			v.counters = append(v.counters, r)
		}
	}
	for _, in := range insns {
		if in.BCol && (in.B < 0 || in.B >= nCols) || in.CCol && (in.C < 0 || in.C >= nCols) ||
			in.Op == POpLoadCol && (in.B < 0 || in.B >= nCols) {
			return nil, fmt.Errorf("jit: column out of range")
		}
		switch in.Op {
		case POpLoadCol, POpLoadReg, POpCmp, POpAnd, POpOr, POpNot, POpSkipIfZero, POpCmpSkip, POpAdd, POpSub, POpShl:
		case POpSetConst:
			v.consts[int64(in.B)] = true
		case POpMul:
			v.consts[vecConstMulCheck] = true
		case POpShr:
			v.consts[vecConstShift] = true
		case POpJump, POpJumpIfZero, POpJumpIfNotZero, POpCmpJump:
			// Only to the next row: a test that leaves it.
			if in.A != ProgNextRow && in.A < len(insns) {
				return nil, fmt.Errorf("jit: a jump within the row")
			}
		case POpAccCount, POpAccSum, POpAccMin, POpAccMax:
			if v.hasAcc {
				return nil, fmt.Errorf("jit: two accumulates into the accumulator")
			}
			v.hasAcc, v.accOp = true, in.Op
			if in.Op != POpAccCount {
				read[in.A] = true
				count(in.B)
			}
			continue
		case POpRAccCount:
			count(in.A)
			continue
		case POpRAccSum, POpRAccMin, POpRAccMax:
			if special[in.A] {
				return nil, fmt.Errorf("jit: one register accumulated twice")
			}
			special[in.A] = true
			v.raccs, v.raccOp[in.A] = append(v.raccs, in.A), in.Op
			read[in.B] = true
			continue
		default:
			return nil, fmt.Errorf("jit: op %d has no vector form", in.Op)
		}
		if in.CImm {
			v.consts[in.Imm] = true
		}
		st := stepOf(in)
		for _, r := range st.uses {
			read[r] = true
		}
		for _, r := range st.defs {
			defined[r] = true
		}
	}
	// A register the accumulates own is read and written by them alone.
	for r := range special {
		if defined[r] || read[r] {
			return nil, fmt.Errorf("jit: an accumulated register is also an operand")
		}
	}
	// No value crosses from one row to the next but the accumulators'.
	if nRegs := progRegs(insns); nRegs > 0 {
		_, liveIn := progLivenessInOut(insns, nRegs)
		for r := range defined {
			if liveIn[0].has(r) || liveIn[len(insns)].has(r) {
				return nil, fmt.Errorf("jit: register %d is carried from row to row", r)
			}
		}
	}
	for r := range read {
		if !defined[r] {
			v.in[r] = true
		}
	}
	if v.hasAcc {
		var err error
		if v.acc, err = take(); err != nil {
			return nil, err
		}
	}
	for i, in := range insns {
		if in.Op == POpAccSum || in.Op == POpRAccSum {
			y, err := take()
			if err != nil {
				return nil, err
			}
			v.absSum[i] = y
		}
	}
	for _, r := range append(slices.Clone(v.counters), v.raccs...) {
		y, err := take()
		if err != nil {
			return nil, err
		}
		v.reg[r] = y
	}
	// The rest hold a row's values, each from its first write to its last
	// read: in one straight run that is all the time it lives, and two whose
	// spans do not overlap share a register. A step's result may take the
	// register of an operand it reads last, as every step writes its result
	// after reading its operands.
	start, end := map[int]int{}, map[int]int{}
	for i, in := range insns {
		st := stepOf(in)
		for _, r := range append(slices.Clone(st.uses), st.defs...) {
			if !defined[r] {
				continue
			}
			if _, ok := start[r]; !ok {
				start[r] = i
			}
			end[r] = i
		}
	}
	temps := slices.Sorted(maps.Keys(start))
	slices.SortStableFunc(temps, func(x, y int) int { return start[x] - start[y] })
	var free []Reg
	for y := next; y < vPool; y++ {
		free = append(free, y)
	}
	var live []int
	for _, r := range temps {
		live = slices.DeleteFunc(live, func(o int) bool {
			if end[o] <= start[r] {
				free = append(free, v.reg[o])
				return true
			}
			return false
		})
		if len(free) == 0 {
			return nil, fmt.Errorf("jit: more than %d vector registers", vPool)
		}
		v.reg[r], free = free[len(free)-1], free[:len(free)-1]
		live = append(live, r)
	}
	return v, nil
}

// vecConstLabel is the pool label of constant k.
func vecConstLabel(k int64) string { return fmt.Sprintf("vk%x", uint64(k)) }

func (v *vecPlan) emit() ([]byte, error) {
	a := NewAsm()
	colReg := [MaxProgCols]Reg{RBX, R12, R13, R15}
	a.MovRegMem(RSI, RDI, POffRegs)
	a.MovRegMem(RCX, RDI, POffN)
	a.AndRegImm8(RCX, -4) // whole groups of four rows only
	for c := 0; c < v.nCols; c++ {
		a.MovRegMem(colReg[c], RDI, int8(POffCol+c*8))
	}
	broadcast := func(y Reg, k int64) {
		a.MovRegImm64(RAX, k)
		a.VmovqToXmm(y, RAX)
		a.VpbroadcastqYmm(y, y)
	}
	a.VpxorYmm(vZero, vZero, vZero)
	a.VpxorYmm(vUnsafe, vUnsafe, vUnsafe)
	switch {
	case v.hasAcc && v.accOp == POpAccMin:
		broadcast(v.acc, math.MaxInt64)
	case v.hasAcc && v.accOp == POpAccMax:
		broadcast(v.acc, math.MinInt64)
	case v.hasAcc:
		a.VpxorYmm(v.acc, v.acc, v.acc)
	}
	for _, y := range v.absSum {
		a.VpxorYmm(y, y, y)
	}
	for _, r := range v.counters {
		a.VpxorYmm(v.reg[r], v.reg[r], v.reg[r])
	}
	// A register accumulator starts from the file's value: in every lane for
	// an extreme, in lane 0 alone for a sum.
	for _, r := range v.raccs {
		y := v.reg[r]
		if v.raccOp[r] != POpRAccSum {
			a.VpbroadcastqMem(y, RSI, r)
			continue
		}
		a.MovRegMem32(RAX, RSI, r)
		a.VmovqToXmm(y, RAX)
	}
	a.TestRegReg(RCX, RCX)
	a.Jcc(CondE, "done")
	for c := 0; c < v.nCols; c++ {
		a.LeaIdx(colReg[c], colReg[c], RCX)
	}
	a.NegReg(RCX)

	// val is the register holding IR register r's four rows: its own, or a
	// caller's value broadcast into scratch.
	val := func(r int, scratch Reg) Reg {
		if v.in[r] {
			a.VpbroadcastqMem(scratch, RSI, r)
			return scratch
		}
		return v.reg[r]
	}
	// opnd is val for an operand that may be a column slot, loaded into
	// scratch, or an immediate, broadcast into it.
	opnd := func(r int, col, imm bool, k int64, scratch Reg) Reg {
		switch {
		case col:
			a.VmovdquLoadIdx(scratch, colReg[r], RCX)
			return scratch
		case imm:
			a.VpbroadcastqRIP(scratch, vecConstLabel(k))
			return scratch
		}
		return val(r, scratch)
	}
	// markSign flags the rows still in whose lane of x has its sign bit set;
	// t is scratch.
	markSign := func(x, t Reg) {
		a.VpandYmm(t, x, vMask)
		a.VporYmm(vUnsafe, vUnsafe, t)
	}
	// markNonzero flags the rows still in whose lane of x is not zero.
	markNonzero := func(x, t Reg) {
		a.VpcmpeqqYmm(t, x, vZero)
		a.VpandnYmm(t, t, vMask)
		a.VporYmm(vUnsafe, vUnsafe, t)
	}
	// markNotInt32 flags the rows still in where x is outside int32.
	markNotInt32 := func(x, t Reg) {
		a.VpbroadcastqRIP(t, vecConstLabel(vecConstMulCheck))
		a.VpaddqYmm(t, x, t)
		a.VpsrlqYmm(t, t, 32)
		markNonzero(t, t)
	}
	// abs adds the absolute values of x's rows still in to s, and leaves x
	// masked in vS1. A lane past int64 sets its sign bit, which stays: every
	// row is flagged. vS2 and vS3 are scratch.
	abs := func(x, s Reg) {
		a.VpandYmm(vS1, x, vMask)
		a.VpcmpgtqYmm(vS2, vZero, vS1)
		a.VpxorYmm(vS3, vS1, vS2)
		a.VpsubqYmm(vS3, vS3, vS2)
		a.VpaddqYmm(s, s, vS3)
		a.VporYmm(vUnsafe, vUnsafe, s)
	}

	padNops(a, loopAlign)
	a.Label("loop")
	a.VpcmpeqqYmm(vMask, vMask, vMask)
	for i, in := range v.insns {
		switch in.Op {
		case POpLoadCol:
			a.VmovdquLoadIdx(v.reg[in.A], colReg[in.B], RCX)
		case POpLoadReg:
			x := val(in.B, vS0)
			a.VporYmm(v.reg[in.A], x, x)
		case POpSetConst:
			a.VpbroadcastqRIP(v.reg[in.A], vecConstLabel(int64(in.B)))
		case POpCmp:
			emitLaneCmp(a, vS2, val(in.B, vS0), val(in.C, vS1), in.Cond)
			a.VpsrlqYmm(v.reg[in.A], vS2, 63)
		case POpAnd:
			a.VpandYmm(v.reg[in.A], val(in.B, vS0), val(in.C, vS1))
		case POpOr:
			a.VporYmm(v.reg[in.A], val(in.B, vS0), val(in.C, vS1))
		case POpNot:
			a.VpcmpeqqYmm(vS2, val(in.B, vS0), vZero)
			a.VpsrlqYmm(v.reg[in.A], vS2, 63)
		case POpSkipIfZero:
			a.VpcmpeqqYmm(vS2, val(in.A, vS0), vZero)
			a.VpandnYmm(vMask, vS2, vMask)
		case POpJumpIfZero: // to the next row, planVec made sure
			a.VpcmpeqqYmm(vS2, val(in.B, vS0), vZero)
			a.VpandnYmm(vMask, vS2, vMask)
		case POpJumpIfNotZero:
			a.VpcmpeqqYmm(vS2, val(in.B, vS0), vZero)
			a.VpandYmm(vMask, vMask, vS2)
		case POpJump:
			a.VpxorYmm(vMask, vMask, vMask)
		case POpCmpSkip, POpCmpJump:
			x := opnd(in.B, in.BCol, false, 0, vS0)
			emitLaneCmp(a, vS2, x, val(in.C, vS1), in.Cond)
			if in.Op == POpCmpSkip {
				a.VpandYmm(vMask, vMask, vS2)
			} else {
				a.VpandnYmm(vMask, vS2, vMask)
			}
		case POpAdd, POpSub:
			x := opnd(in.B, in.BCol, false, 0, vS0)
			y := opnd(in.C, in.CCol, in.CImm, in.Imm, vS1)
			if in.Op == POpAdd {
				// Overflow: both operands' signs differ from the result's.
				a.VpaddqYmm(vS2, x, y)
				a.VpxorYmm(vS3, y, vS2)
				a.VpxorYmm(vS0, x, vS2)
			} else {
				// Overflow: the operands' signs differ, and the result's
				// from the first's.
				a.VpsubqYmm(vS2, x, y)
				a.VpxorYmm(vS3, x, y)
				a.VpxorYmm(vS0, x, vS2)
			}
			a.VpandYmm(vS0, vS0, vS3)
			markSign(vS0, vS0)
			a.VporYmm(v.reg[in.A], vS2, vS2)
		case POpMul:
			x := opnd(in.B, in.BCol, false, 0, vS0)
			y := opnd(in.C, in.CCol, in.CImm, in.Imm, vS1)
			markNotInt32(x, vS2)
			if !in.CImm {
				markNotInt32(y, vS2)
			}
			a.VpmuldqYmm(v.reg[in.A], x, y)
		case POpShl, POpShr:
			x := opnd(in.B, in.BCol, false, 0, vS0)
			n := opnd(in.C, in.CCol, false, 0, vS1)
			a.VpsrlqYmm(vS2, n, 6)
			markNonzero(vS2, vS2)
			if in.Op == POpShl {
				a.VpsllvqYmm(v.reg[in.A], x, n)
				break
			}
			// Arithmetic: the logical shift, and the sign's copies shifted
			// into the top (none for an amount of 0: a shift by 64 gives 0).
			a.VpsrlvqYmm(vS2, x, n)
			a.VpbroadcastqRIP(vS3, vecConstLabel(vecConstShift))
			a.VpsubqYmm(vS3, vS3, n)
			a.VpcmpgtqYmm(vS1, vZero, x)
			a.VpsllvqYmm(vS1, vS1, vS3)
			a.VporYmm(v.reg[in.A], vS2, vS1)
		case POpAccCount:
			a.VpsubqYmm(v.acc, v.acc, vMask)
		case POpAccSum, POpRAccSum:
			s, x := v.acc, in.A
			if in.Op == POpRAccSum {
				s, x = v.reg[in.A], in.B
			}
			abs(val(x, vS0), v.absSum[i])
			a.VpaddqYmm(s, s, vS1) // x masked, from abs
			if in.Op == POpAccSum {
				a.VpsubqYmm(v.reg[in.B], v.reg[in.B], vMask)
			}
		case POpAccMin, POpAccMax, POpRAccMin, POpRAccMax:
			s, x := v.acc, in.A
			if in.Op == POpRAccMin || in.Op == POpRAccMax {
				s, x = v.reg[in.A], in.B
			}
			xv := val(x, vS0)
			if in.Op == POpAccMin || in.Op == POpRAccMin {
				a.VpcmpgtqYmm(vS1, s, xv) // take x where it is smaller
			} else {
				a.VpcmpgtqYmm(vS1, xv, s)
			}
			a.VpandYmm(vS1, vS1, vMask)
			a.VblendvpdYmm(s, s, xv, vS1)
			if in.Op == POpAccMin || in.Op == POpAccMax {
				a.VpsubqYmm(v.reg[in.B], v.reg[in.B], vMask)
			}
		case POpRAccCount:
			a.VpsubqYmm(v.reg[in.A], v.reg[in.A], vMask)
		}
	}
	a.AddRegImm8(RCX, 4)
	a.Jcc(CondNE, "loop")

	a.Label("done")
	// lanes calls f with each of y's four lanes in RDX.
	lanes := func(y Reg, f func(first bool)) {
		a.Vextracti128(vS0, y, 1)
		a.VmovqToReg(RDX, y)
		f(true)
		a.Vpextrq(RDX, y, 1)
		f(false)
		a.VmovqToReg(RDX, vS0)
		f(false)
		a.Vpextrq(RDX, vS0, 1)
		f(false)
	}
	// reduce leaves y's lanes combined in R8: summed, or the extreme.
	reduce := func(y Reg, op POp) {
		lanes(y, func(first bool) {
			switch {
			case first:
				a.MovRegReg(R8, RDX)
			case op == POpAccMin || op == POpRAccMin:
				a.CmpRegReg(R8, RDX)
				a.Cmovcc(CondG, R8, RDX)
			case op == POpAccMax || op == POpRAccMax:
				a.CmpRegReg(R8, RDX)
				a.Cmovcc(CondL, R8, RDX)
			default:
				a.AddRegReg(R8, RDX)
			}
		})
	}
	// R9 flags a sum whose absolute values, a register sum's starting value
	// with them, pass int64 across the lanes.
	a.XorRegReg(R9, R9)
	for i, in := range v.insns {
		s, ok := v.absSum[i]
		if !ok {
			continue
		}
		lanes(s, func(first bool) {
			if first {
				a.MovRegReg(R10, RDX)
				return
			}
			a.AddRegReg(R10, RDX)
			a.Jcc(condOverflow, "absovf")
		})
		if in.Op == POpRAccSum {
			a.MovRegMem32(RDX, RSI, in.A)
			a.MovRegReg(RAX, RDX)
			a.NegReg(RAX)
			a.Cmovcc(condSign, RAX, RDX) // |s0|, MinInt64 staying negative
			a.TestRegReg(RAX, RAX)
			a.Jcc(condSign, "absovf")
			a.AddRegReg(R10, RAX)
			a.Jcc(condOverflow, "absovf")
		}
	}
	a.Jmp("absok")
	a.Label("absovf")
	a.MovRegImm64(R9, 1)
	a.Label("absok")
	if v.hasAcc {
		reduce(v.acc, v.accOp)
	} else {
		a.XorRegReg(R8, R8)
	}
	a.MovRegMem(RDX, RDI, POffOut)
	a.MovMemReg(RDX, 0, R8)
	for _, r := range v.counters {
		reduce(v.reg[r], POpAccCount)
		a.MovRegMem32(RAX, RSI, r)
		a.AddRegReg(RAX, R8)
		a.MovMemReg32(RSI, r, RAX)
	}
	for _, r := range v.raccs {
		reduce(v.reg[r], v.raccOp[r])
		a.MovMemReg32(RSI, r, R8)
	}
	a.VmovmskpdYmm(RAX, vUnsafe)
	a.OrRegReg(RAX, R9)
	a.MovRegMem(RDX, RDI, POffOverflow)
	a.MovMemReg(RDX, 0, RAX)
	a.XorRegReg(RAX, RAX)
	a.MovMemReg(RDI, POffPC, RAX)
	a.Vzeroupper()
	a.Ret()
	for _, k := range slices.Sorted(maps.Keys(v.consts)) {
		a.Align(8)
		a.Label(vecConstLabel(k))
		for b := range 8 {
			a.emit(byte(uint64(k) >> (8 * b)))
		}
	}
	return a.Code()
}
