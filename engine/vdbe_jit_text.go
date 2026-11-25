package engine

import (
	"math"
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
func vmTextLowering(prog *Program, storesText func(root uint32, col int) bool) (out []jit.VInsn, ok []bool, srcs []vmIntTextSrc) {
	if !jit.VMTextOrder {
		return nil, nil, nil
	}
	n := len(prog.Insns)
	tables := map[int]*resolvedTable{}
	text := make(vmRegs, prog.NReg)
	// consts is the string literal a register holds, of those text says are
	// TEXT, for a LIKE's pattern.
	consts := map[int]string{}
	// ints is the number a register is expected to hold: an INTEGER or a
	// REAL literal, the sum of two INTEGERs or one negated, or a bound
	// parameter, or a copy of one. It is the last such write in program order, which may not be
	// the one a run made (a CASE's arms); native code checks it at run time,
	// so a wrong guess costs only an exit.
	ints := vmIntSrcs{v: make([]vmIntTextSrc, prog.NReg), has: make(vmRegs, prog.NReg)}
	in := func(r int) bool { return r >= 0 && r < prog.NReg }
	for pc := range prog.Insns {
		ins := &prog.Insns[pc]
		set := func(r int, isText bool) {
			delete(consts, r)
			text.put(r, isText)
		}
		var cast *vmIntTextSrc
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
			if str, _ := ins.P4.(string); str != "" {
				consts[ins.P2] = str
			}
			// The constant's bytes, as OpString8 shares them; "" keeps the
			// VDBE's non-nil empty slice.
			if str, _ := ins.P4.(string); str != "" && int64(len(str)) < 1<<31 && in(ins.P2) {
				if ok == nil {
					out, ok = make([]jit.VInsn, n), make([]bool, n)
				}
				out[pc], ok[pc] = jit.VInsn{Op: jit.VText, A: ins.P2, B: len(str), Imm: int64(uintptr(unsafe.Pointer(unsafe.StringData(str))))}, true
			}
		case OpCast:
			// CAST('2002-04-02' AS date), its literal's number: what the VDBE
			// makes of the literal, where the register still holds it.
			str, isConst := consts[ins.P1]
			target, _ := ins.P4.(string)
			if !isConst || !in(ins.P1) || int64(len(str)) >= 1<<31 || target != "NUMERIC" && target != "INTEGER" && target != "REAL" {
				break
			}
			v := castValueEnc(Value{Typ: Text, S: []byte(str)}, target, UTF8)
			vi := jit.VInsn{Op: jit.VCastConst, A: ins.P1, B: len(str), Imm: int64(uintptr(unsafe.Pointer(unsafe.StringData(str))))}
			switch {
			case v.Typ == Int:
				vi.K = v.I
				cast = &vmIntTextSrc{kind: jit.VIntTextInt, k: v.I}
			case v.Typ == Float && jit.VMReal:
				vi.C, vi.K = 1, int64(math.Float64bits(v.F))
				cast = &vmIntTextSrc{kind: jit.VIntTextReal, k: vi.K}
			}
			if cast != nil {
				if ok == nil {
					out, ok = make([]jit.VInsn, n), make([]bool, n)
				}
				out[pc], ok[pc] = vi, true
				set(ins.P1, false)
			}
		case OpSCopy, OpCopy:
			set(ins.P2, text.has(ins.P1))
			if str, isConst := consts[ins.P1]; isConst {
				consts[ins.P2] = str
			}
		case OpLike:
			esc, isInt := ins.P4.(int)
			if pat := vmLikePat(consts[ins.P3], ins.P5&p5LikeNot != 0); pat != nil && isInt && esc < 0 && in(ins.P1) && in(ins.P2) && in(ins.P3) {
				if ok == nil {
					out, ok = make([]jit.VInsn, n), make([]bool, n)
				}
				out[pc], ok[pc] = jit.VInsn{Op: jit.VLike, A: ins.P1, B: ins.P3, C: ins.P2, Pat: pat}, true
			}
			set(ins.P2, false)
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			coll, _ := ins.P4.(string)
			if (text.has(ins.P1) || text.has(ins.P3)) && (coll == "" || strings.EqualFold(coll, "BINARY")) && in(ins.P1) && in(ins.P3) {
				if ok == nil {
					out, ok = make([]jit.VInsn, n), make([]bool, n)
				}
				imm := vmTextCmpNull(ins.P5)
				switch affinity(ins.P5 & p5AffMask) {
				case affNone:
					imm |= jit.VTextCmpNoAff
				case affNumeric, affInteger, affReal:
					imm |= 1
				}
				cond := vmCond[ins.Op]
				if ins.P5&p5StoreP2 == 0 {
					out[pc], ok[pc] = jit.VInsn{Op: jit.VTextCmpJump, A: ins.P1, B: ins.P3, T: ins.P2, Cond: cond, Imm: imm}, true
				} else if in(ins.P2) {
					out[pc], ok[pc] = jit.VInsn{Op: jit.VTextCmpSet, A: ins.P1, B: ins.P3, C: ins.P2, Cond: cond, Imm: imm}, true
				}
				if ok[pc] && jit.VMTextIntConst && !vmIntTextOffForTest && affinity(ins.P5&p5AffMask) == affText {
					// Under TEXT affinity a number is compared with a TEXT as its
					// text, which the VDBE makes on every row.
					src, isInt := ints.get(ins.P1)
					onA := isInt && text.has(ins.P3) && !text.has(ins.P1)
					if !onA {
						src, isInt = ints.get(ins.P3)
						isInt = isInt && text.has(ins.P1) && !text.has(ins.P3)
					}
					if isInt && len(srcs) < jit.MaxIntTexts {
						if src.param == 0 {
							src.text = appendNumberText(nil, src.kind, src.k)
						}
						out[pc].IntText = &jit.VIntText{OnA: onA, Slot: len(srcs)}
						srcs = append(srcs, src)
					}
				}
			}
			if ins.P5&p5StoreP2 != 0 {
				set(ins.P2, false)
			}
		default:
			// Any other write is not known to be a TEXT.
			for _, r := range []int{ins.P2, ins.P3} {
				if text.has(r) && writesReg(ins, r) {
					text.put(r, false)
					delete(consts, r)
				}
			}
		}
		a, _ := ints.get(ins.P1)
		b, _ := ints.get(ins.P2)
		switch {
		case cast != nil:
			ints.set(ins.P1, *cast)
		case ins.Op == OpInteger:
			ints.set(ins.P2, vmIntTextSrc{kind: jit.VIntTextInt, k: int64(ins.P1)})
		case ins.Op == OpReal && isFloat64(ins.P4):
			ints.set(ins.P2, vmIntTextSrc{kind: jit.VIntTextReal, k: int64(math.Float64bits(ins.P4.(float64)))})
		case ins.Op == OpVariable && ins.P1 >= 1:
			ints.set(ins.P2, vmIntTextSrc{param: ins.P1})
		case (ins.Op == OpSCopy || ins.Op == OpCopy && ins.P3 == 0) && hasSrc(ints, ins.P1):
			ints.set(ins.P2, a)
		case ins.Op == OpAdd && hasLiteral(ints, ins.P1, ins.P2) && (b.k >= 0) == (a.k+b.k >= a.k):
			ints.set(ins.P3, vmIntTextSrc{kind: jit.VIntTextInt, k: a.k + b.k})
		case ins.Op == OpNegative && hasLiteral(ints, ins.P1) && a.k != math.MinInt64:
			ints.set(ins.P2, vmIntTextSrc{kind: jit.VIntTextInt, k: -a.k})
		default:
			for _, r := range []int{ins.P2, ins.P3} {
				if writesReg(ins, r) {
					ints.del(r)
				}
			}
		}
	}
	return out, ok, srcs
}

// vmIntTextSrc is where an entry finds a TEXT compare's number
// (jit.VIntTextSlot): bound parameter param, or else the literal k of kind,
// whose text is text.
type vmIntTextSrc struct {
	param int
	kind  int64
	k     int64
	text  []byte
}

func isFloat64(p4 any) bool {
	_, is := p4.(float64)
	return is
}

func hasSrc(ints vmIntSrcs, r int) bool { return ints.has.has(r) }

// vmIntSrcs is vmTextLowering's expected number per register (vmRegs).
type vmIntSrcs struct {
	v   []vmIntTextSrc
	has vmRegs
}

func (m vmIntSrcs) get(r int) (vmIntTextSrc, bool) {
	if !m.has.has(r) {
		return vmIntTextSrc{}, false
	}
	return m.v[r], true
}

func (m vmIntSrcs) set(r int, src vmIntTextSrc) {
	if r >= 0 && r < len(m.v) {
		m.v[r] = src
		m.has[r] = true
	}
}

func (m vmIntSrcs) del(r int) { m.has.put(r, false) }

// hasLiteral reports whether each of rs is expected to hold an INTEGER
// literal.
func hasLiteral(ints vmIntSrcs, rs ...int) bool {
	for _, r := range rs {
		if src, isInt := ints.get(r); !isInt || src.param != 0 || src.kind != jit.VIntTextInt {
			return false
		}
	}
	return true
}

// vmIntTextOffForTest leaves a TEXT compare's INTEGER constant unread
// (jit.VIntText), so a test can count what reading it saves.
var vmIntTextOffForTest bool

// vmLikePat is the pattern VLike matches for the string literal pat, or nil
// where the engine's own likePlan would not take it (a '_', a NUL, a byte
// past ASCII) or it is too long to compare against immediates.
func vmLikePat(pat string, not bool) *jit.VLikePat {
	if pat == "" || len(pat) > 128 {
		return nil
	}
	for i := 0; i < len(pat); i++ {
		if c := pat[i]; c >= 0x80 || c == '_' || c == 0 {
			return nil
		}
	}
	lp := newLikePlan([]byte(pat))
	if !lp.ok {
		return nil
	}
	for _, piece := range lp.pieces {
		if len(piece) > 64 {
			return nil
		}
	}
	return &jit.VLikePat{Src: uintptr(unsafe.Pointer(unsafe.StringData(pat))), SrcLen: len(pat), Pieces: lp.pieces,
		Head: lp.head, Tail: lp.tail, Exact: lp.exact, Not: not}
}

// writesReg is a cautious guess that ins may write r: the destination slots
// of the opcodes that write one. A function's P2 is its argument count, and a
// jump's its target.
func writesReg(ins *Instruction, r int) bool {
	switch ins.Op {
	case OpInteger, OpInt64, OpReal, OpNull, OpBlob, OpVariable, OpNot, OpRowid:
		return ins.P2 == r
	case OpFunction:
		return ins.P3 == r
	case OpRewind, OpNext, OpIfNot, OpIf, OpIsNull, OpNotNull, OpGoto, OpInit:
		return false
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
