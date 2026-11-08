package engine

import (
	"strings"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// vmTextLowering is, per pc, the TEXT arm a comparison lowers to where an
// operand is a TEXT by what the program says -- a column the table's
// segments store as TEXT, a string literal, a copy of either -- and ok
// false where it keeps its lowering. The INTEGER arm would exit on every
// such row; the TEXT arm compares two TEXTs, two INTEGERs, and exits on any
// other pair, and under a numeric affinity on a TEXT that is a whole number.
func vmTextLowering(prog *Program, storesText func(root uint32, col int) bool) (out []jit.VInsn, ok []bool) {
	if !jit.VMTextOrder {
		return nil, nil
	}
	n := len(prog.Insns)
	tables := map[int]*resolvedTable{}
	text := map[int]bool{}
	in := func(r int) bool { return r >= 0 && r < prog.NReg }
	for pc := range prog.Insns {
		ins := &prog.Insns[pc]
		set := func(r int, isText bool) {
			if isText {
				text[r] = true
			} else {
				delete(text, r)
			}
		}
		switch ins.Op {
		case OpOpenRead, OpOpenWrite:
			if t, isTbl := ins.P4.(*resolvedTable); isTbl {
				tables[ins.P1] = t
			}
		case OpColumn:
			t := tables[ins.P1]
			set(ins.P3, t != nil && storesText != nil && ins.P2 >= 0 && ins.P2 < len(t.cols) && storesText(t.root, ins.P2))
		case OpString8:
			set(ins.P2, true)
			// The constant's bytes, as OpString8 shares them; "" keeps the
			// VDBE's non-nil empty slice.
			if str, _ := ins.P4.(string); str != "" && len(str) < 1<<31 && in(ins.P2) {
				if ok == nil {
					out, ok = make([]jit.VInsn, n), make([]bool, n)
				}
				out[pc], ok[pc] = jit.VInsn{Op: jit.VText, A: ins.P2, B: len(str), Imm: int64(uintptr(unsafe.Pointer(unsafe.StringData(str))))}, true
			}
		case OpSCopy, OpCopy:
			set(ins.P2, text[ins.P1])
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			coll, _ := ins.P4.(string)
			if (text[ins.P1] || text[ins.P3]) && (coll == "" || strings.EqualFold(coll, "BINARY")) && in(ins.P1) && in(ins.P3) {
				if ok == nil {
					out, ok = make([]jit.VInsn, n), make([]bool, n)
				}
				var imm int64
				if affinity(ins.P5&p5AffMask) >= affNumeric {
					imm = 1
				}
				cond := vmCond[ins.Op]
				if ins.P5&p5StoreP2 == 0 {
					out[pc], ok[pc] = jit.VInsn{Op: jit.VTextCmpJump, A: ins.P1, B: ins.P3, T: ins.P2, Cond: cond, Imm: imm}, true
				} else if in(ins.P2) {
					out[pc], ok[pc] = jit.VInsn{Op: jit.VTextCmpSet, A: ins.P1, B: ins.P3, C: ins.P2, Cond: cond, Imm: imm}, true
				}
			}
			if ins.P5&p5StoreP2 != 0 {
				set(ins.P2, false)
			}
		default:
			// Any other write is not known to be a TEXT.
			for _, r := range []int{ins.P2, ins.P3} {
				if text[r] && writesReg(ins, r) {
					delete(text, r)
				}
			}
		}
	}
	return out, ok
}

// writesReg is a cautious guess that ins may write r: the destination slots
// of the opcodes that write one.
func writesReg(ins *Instruction, r int) bool {
	switch ins.Op {
	case OpInteger, OpInt64, OpReal, OpNull, OpBlob, OpVariable, OpNot, OpRowid:
		return ins.P2 == r
	}
	return ins.P3 == r || ins.P2 == r
}

// segStoresText reports whether a segment of the table at root stores column
// col as TEXT.
func (p *ReadOnlyPager) segStoresText(root uint32, col int) bool {
	if p == nil || p.segs == nil {
		return false
	}
	segs := p.segs.byRoot[root]
	if p.segs.isLive(root) {
		var ok bool
		if segs, ok = p.segs.liveBaseSegments(root); !ok {
			return false
		}
	}
	for _, s := range segs {
		if col < len(s.cols) && s.cols[col].phys == PhysText {
			return true
		}
	}
	return false
}
