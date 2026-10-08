package engine

import "math"

// "SELECT DISTINCT col FROM t" over a columnar table: the loop built a record
// per row and probed the distinct set with it. When col is a clean int64 block
// (or the rowid alias) the distinct values come straight off the block, in the
// order the loop would first meet them -- segment by segment, row by row, which
// is rowid order -- and OpSegEmitRow hands them out. A collation cannot matter:
// every value is an integer.

// segDistinctPlan is what OpSegDistinct carries.
type segDistinctPlan struct {
	col int
}

// segDistinctPeephole rewrites
//
//	Init; DistinctOpen d; OpenRead c; <prologue>; Rewind c -> close
//	top:  Column c.col -> r; [SCopy r -> base]; MakeRecord base 1 -> rr
//	      Distinct d -> next, rr; ResultRow base 1
//	next: Next c -> top
//	close: Close c; Halt
//
// by putting an OpSegDistinct guard after the OpenRead and appending the emit
// loop OpSegOrderLimit uses. Anything else is left alone.
func segDistinctPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 11 || in[0].Op != OpInit || in[1].Op != OpDistinctOpen || in[2].Op != OpOpenRead {
		return false
	}
	set, cursor := in[1].P1, in[2].P1
	rewindAt := 3
	for rewindAt < len(in) && segIsScanPrologue(in[rewindAt].Op) {
		if in[rewindAt].Op == OpAutoIndexOrder {
			return false // index order, not rowid order: a different first row
		}
		rewindAt++
	}
	pc := rewindAt + 1
	if pc+5 >= len(in) || in[rewindAt].Op != OpRewind || in[rewindAt].P1 != cursor {
		return false
	}
	col := in[pc]
	if col.Op != OpColumn || col.P1 != cursor {
		return false
	}
	base := col.P3
	pc++
	if in[pc].Op == OpSCopy && in[pc].P1 == col.P3 {
		base = in[pc].P2
		pc++
	}
	if pc+5 >= len(in) {
		return false
	}
	rec, dist, row, next, cl, halt := in[pc], in[pc+1], in[pc+2], in[pc+3], in[pc+4], in[pc+5]
	nextAt := pc + 3
	if rec.Op != OpMakeRecord || rec.P1 != base || rec.P2 != 1 ||
		dist.Op != OpDistinct || dist.P1 != set || dist.P2 != nextAt || dist.P3 != rec.P3 ||
		row.Op != OpResultRow || row.P1 != base || row.P2 != 1 ||
		next.Op != OpNext || next.P1 != cursor || next.P2 != rewindAt+1 ||
		in[rewindAt].P2 != nextAt+1 || cl.Op != OpClose || cl.P1 != cursor || halt.Op != OpHalt || pc+5 != len(in)-1 {
		return false
	}
	out := make([]Instruction, 0, len(in)+5)
	out = append(out, in[0], in[1], in[2])
	emitAt := len(in) + 1
	out = append(out, Instruction{Op: OpSegDistinct, P1: base, P2: cursor, P3: emitAt, P4: &segDistinctPlan{col: col.P2}})
	for _, ins := range in[3:] {
		switch ins.Op {
		case OpRewind, OpNext, OpDistinct, OpGoto, OpIf, OpIfNot:
			ins.P2++
		}
		out = append(out, ins)
	}
	out = append(out,
		Instruction{Op: OpSegEmitRow, P1: base, P2: emitAt + 3, P3: 1},
		Instruction{Op: OpResultRow, P1: base, P2: 1},
		Instruction{Op: OpGoto, P2: emitAt},
		Instruction{Op: OpHalt})
	prog.Insns = out
	return true
}

// segDistinctTable answers the statement: col's distinct values in first-seen
// order, one row each. It serves a clean table only (no log to merge) and a
// column that is a clean int64 block or the rowid alias, else declines.
func (p *ReadOnlyPager) segDistinctTable(rootPage uint32, col, ipkCol int) ([][]Value, bool) {
	if p == nil || p.segs == nil || !p.segCleanFor(rootPage) {
		return nil, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return nil, false
	}
	blocks := make([][]int64, len(segs))
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for i, s := range segs {
		if col == ipkCol {
			continue // read from the rowid block below
		}
		if !segPredColsFilterable(s, []segPred{{Col: col}}) {
			return nil, false
		}
		b, okb := s.Int64Column(col)
		if !okb || len(b) < s.nRows {
			return nil, false
		}
		blocks[i] = b[:s.nRows]
		for _, v := range blocks[i] {
			lo, hi = min(lo, v), max(hi, v)
		}
	}
	// A narrow range is marked in a bitset, a probe per row costing a shift
	// and a mask; a wide one (or the rowid, unique anyway) in a map.
	var bits []uint64
	var seen map[int64]struct{}
	if col != ipkCol && lo <= hi && uint64(hi-lo) < 1<<22 {
		bits = make([]uint64, uint64(hi-lo)/64+1)
	} else {
		seen = map[int64]struct{}{}
	}
	var vals []Value
	for i, s := range segs {
		block := blocks[i]
		for r := 0; r < s.nRows; r++ {
			var v int64
			if block == nil {
				v = int64(s.Rowid(r))
			} else {
				v = block[r]
			}
			if bits != nil {
				o := uint64(v - lo)
				if bits[o/64]&(1<<(o%64)) != 0 {
					continue
				}
				bits[o/64] |= 1 << (o % 64)
			} else {
				if _, dup := seen[v]; dup {
					continue
				}
				seen[v] = struct{}{}
			}
			vals = append(vals, Value{Typ: Int, I: v})
		}
	}
	rows := make([][]Value, len(vals))
	for i := range vals {
		rows[i] = vals[i : i+1 : i+1]
	}
	return rows, true
}
