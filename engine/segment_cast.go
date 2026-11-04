package engine

import "slices"

// CAST in a lowered body. OpCast changes its register in place, r[P1] =
// CAST(r[P1] AS P4), and a program register has one representation, so a
// cast from a REAL to an INTEGER cannot be lowered where it stands.
// segRenameCasts moves the value before the cast instead: the instruction
// that wrote it, and the reads up to the cast, use a fresh register, and the
// cast writes the original one from it (P1 the source, P2 the destination,
// P3 segCastRenamed). Every read after the cast, inside the body or past it,
// still names the register it named.

// segCastRenamed is a renamed OpCast's P3.
const segCastRenamed = 1

// segCastNative is the affinity a renamed cast converts to when native code
// converts to it, INTEGER or REAL; affNone for any other.
func segCastNative(in Instruction) affinity {
	if in.Op != OpCast || in.P3 != segCastRenamed {
		return affNone
	}
	switch typ, _ := in.P4.(string); typ {
	case "INTEGER":
		return affInteger
	case "REAL":
		return affReal
	}
	return affNone
}

// segRenameCasts is body, whose first instruction is at address base, with
// every OpCast it can rename renamed. It renames one whose value an
// instruction of the body wrote on a straight run to it: no jump between, and
// none landing after the write, so every path to the cast passes the write
// and every path from the write passes the cast. Any other is left as it was,
// which the lowering refuses or runs as the VDBE does.
func segRenameCasts(body []Instruction, base int) []Instruction {
	if !slices.ContainsFunc(body, func(in Instruction) bool { return in.Op == OpCast && in.P3 != segCastRenamed }) {
		return body
	}
	next := 0
	for _, in := range body {
		reads, writes, _, _ := segInsnRegs(in)
		ops := segLowerRegOperands(in)
		for _, r := range append(append(reads, writes...), ops[:]...) {
			next = max(next, r+1)
		}
	}
	target := make([]bool, len(body))
	for _, in := range body {
		if _, _, jump, _ := segInsnRegs(in); jump || in.Op == OpGoto {
			if t := in.P2 - base; t >= 0 && t < len(body) {
				target[t] = true
			}
		}
	}
	out := slices.Clone(body)
	for i, in := range out {
		if in.Op != OpCast || in.P3 == segCastRenamed {
			continue
		}
		r := in.P1
		w := -1
		for j := i - 1; j >= 0 && !target[j+1]; j-- {
			_, writes, jump, ok := segInsnRegs(out[j])
			if !ok || jump {
				break
			}
			if slices.Contains(writes, r) {
				w = j
				break
			}
		}
		if w < 0 {
			continue
		}
		renamed := slices.Clone(out[w : i+1])
		if !segRenameWrite(&renamed[0], r, next) {
			continue
		}
		ok := true
		for k := 1; k < len(renamed)-1 && ok; k++ {
			ok = segRenameReads(&renamed[k], r, next)
		}
		if !ok {
			continue
		}
		renamed[len(renamed)-1] = Instruction{Op: OpCast, P1: next, P2: r, P3: segCastRenamed, P4: in.P4}
		copy(out[w:], renamed)
		next++
	}
	return out
}

// segRenameWrite makes in, which writes r and does not read it, write f.
func segRenameWrite(in *Instruction, r, f int) bool {
	if reads, _, _, _ := segInsnRegs(*in); slices.Contains(reads, r) {
		return false
	}
	switch in.Op {
	case OpColumn, OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat,
		OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight, OpFunction:
		in.P3 = f
	case OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable, OpSCopy, OpCopy, OpNot, OpNegative, OpBitNot:
		in.P2 = f
	case OpCast:
		if in.P3 != segCastRenamed {
			return false
		}
		in.P2 = f
	default:
		return false
	}
	return true
}

// segRenameReads makes in read f where it reads r; false when in writes r,
// or reads it where a register cannot be renamed (a call's argument range).
func segRenameReads(in *Instruction, r, f int) bool {
	reads, writes, _, ok := segInsnRegs(*in)
	if !ok || slices.Contains(writes, r) {
		return false
	}
	if !slices.Contains(reads, r) {
		return true
	}
	swap := func(p *int) {
		if *p == r {
			*p = f
		}
	}
	switch in.Op {
	case OpSCopy, OpCopy, OpNot, OpNegative, OpBitNot, OpIf, OpIfNot, OpIsNull, OpNotNull, OpInList:
		swap(&in.P1)
	case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		swap(&in.P1)
		swap(&in.P2)
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		swap(&in.P1)
		swap(&in.P3)
	case OpCast:
		swap(&in.P1)
	default:
		return false
	}
	return true
}
