package engine

import "math"

// A correlated EXISTS over a rowid equality, answered as a SEMI-JOIN over the
// two tables' column blocks rather than one seek per outer row:
//
//	SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.x < ?)
//
// is the number of t rows whose bid is the rowid of a b row satisfying b.x < ?.
// The b side is scanned once into a set of qualifying rowids; the t side is a
// probe of t.bid's int64 block against that set, beside whatever plain
// predicates the outer WHERE also has. Two passes over columns, where the loop
// it replaces runs a subroutine, a seek and a dozen instructions per outer row.
//
// It is one more predicate kind of the filtered count (segPeephole), so it is
// a guard in front of a loop that stays in the program: anything the blocks
// cannot answer exactly -- a NULL or non-integer key, a block with exceptions,
// rows in a log, a bound of the wrong class -- declines, and the inlined EXISTS
// (exists_inline.go) answers row by row.

// segPlanSemi is one EXISTS of the filtered count: the outer key column, and
// the inner table with the predicates its rows must satisfy.
type segPlanSemi struct {
	keyCol int // column of the OUTER table compared with the inner rowid
	bRoot  uint32
	bTbl   *resolvedTable
	bPreds []segPlanPred // over the inner table; on its rowid alias they read the rowid
}

// segParseSemiGroup reads, at pc, "Gosub to an inlined EXISTS subroutine; IfNot
// on its result to the row's Next" and checks the subroutine is exactly the
// rowid-seek EXISTS existsInline emits:
//
//	S:   Integer 0 -> dest ; Goto S+2
//	     OpenRead B (this database, a rowid table)
//	     Column cursor.keyCol -> k ; SeekRowidHint B k ; Rewind B -> closeB
//	top: Column B.ipk / Column cursor.keyCol ; Eq (stored) ; IfNot -> nextB
//	     <predicate groups over B, each failing to nextB>
//	     <constants the select list loads> ; Goto found
//	nextB: Next B -> top
//	closeB: Close B ; Goto done
//	found: Integer 1 -> dest ; Close B ; Return
//	done:  Return
func segParseSemiGroup(in []Instruction, pc, limit, cursor, nextAt int) (segPlanSemi, int, bool) {
	var no segPlanSemi
	if pc+1 >= limit {
		return no, 0, false
	}
	call, test := in[pc], in[pc+1]
	if call.Op != OpGosub || test.Op != OpIfNot || test.P2 != nextAt || test.P3 != 1 {
		return no, 0, false
	}
	ret, s := call.P1, call.P2
	at := func(i int) (Instruction, bool) {
		if i < 0 || i >= len(in) {
			return Instruction{}, false
		}
		return in[i], true
	}
	i0, ok0 := at(s)
	i1, ok1 := at(s + 1)
	open, ok2 := at(s + 2)
	key, ok3 := at(s + 3)
	seek, ok4 := at(s + 4)
	rew, ok5 := at(s + 5)
	if !(ok0 && ok1 && ok2 && ok3 && ok4 && ok5) {
		return no, 0, false
	}
	dest := test.P1
	if i0.Op != OpInteger || i0.P1 != 0 || i0.P2 != dest || i1.Op != OpGoto || i1.P2 != s+2 {
		return no, 0, false
	}
	bt, okT := open.P4.(*resolvedTable)
	if open.Op != OpOpenRead || open.P3 != 0 || !okT || bt == nil || bt.withoutRowid || bt.ipkIndex < 0 {
		return no, 0, false
	}
	b := open.P1
	if key.Op != OpColumn || key.P1 != cursor || seek.Op != OpSeekRowidHint || seek.P1 != b || seek.P2 != key.P3 || seek.P3 != 0 {
		return no, 0, false
	}
	if rew.Op != OpRewind || rew.P1 != b || rew.P4 != nil {
		return no, 0, false
	}
	top := s + 6
	// The equality: the inner rowid alias against the same outer column.
	ca, okA := at(top)
	cb, okB := at(top + 1)
	eq, okE := at(top + 2)
	fail, okF := at(top + 3)
	if !(okA && okB && okE && okF) || eq.Op != OpEq || eq.P5&p5StoreP2 == 0 || fail.Op != OpIfNot || fail.P1 != eq.P2 || fail.P3 != 1 {
		return no, 0, false
	}
	inner, outer := ca, cb
	if inner.P1 != b {
		inner, outer = cb, ca
	}
	if inner.Op != OpColumn || inner.P1 != b || inner.P2 != bt.ipkIndex ||
		outer.Op != OpColumn || outer.P1 != cursor || outer.P2 != key.P2 {
		return no, 0, false
	}
	if !((eq.P1 == inner.P3 && eq.P3 == outer.P3) || (eq.P1 == outer.P3 && eq.P3 == inner.P3)) {
		return no, 0, false
	}
	nextB := fail.P2
	nb, okN := at(nextB)
	if !okN || nb.Op != OpNext || nb.P1 != b || nb.P2 != top {
		return no, 0, false
	}
	preds, p, ok := segParsePredBlock(in, top+4, nextB, b, nextB)
	if !ok {
		return no, 0, false
	}
	// The select list: constants only, then the jump to found.
	for p < nextB && (in[p].Op == OpInteger || in[p].Op == OpNull || in[p].Op == OpSCopy || in[p].Op == OpString8 || in[p].Op == OpReal) {
		p++
	}
	gotoFound, okG := at(p)
	if !okG || p+1 != nextB || gotoFound.Op != OpGoto {
		return no, 0, false
	}
	found := gotoFound.P2
	cl, ok6 := at(nextB + 1)
	toDone, ok7 := at(nextB + 2)
	f0, ok8 := at(found)
	f1, ok9 := at(found + 1)
	f2, ok10 := at(found + 2)
	if !(ok6 && ok7 && ok8 && ok9 && ok10) || rew.P2 != nextB+1 || cl.Op != OpClose || cl.P1 != b || toDone.Op != OpGoto ||
		f0.Op != OpInteger || f0.P1 != 1 || f0.P2 != dest || f1.Op != OpClose || f1.P1 != b || f2.Op != OpReturn || f2.P1 != ret {
		return no, 0, false
	}
	done, okD := at(toDone.P2)
	if !okD || done.Op != OpReturn || done.P1 != ret {
		return no, 0, false
	}
	return segPlanSemi{keyCol: key.P2, bRoot: uint32(open.P2), bTbl: bt, bPreds: preds}, pc + 2, true
}

// segSemis resolves each semi-join's inner bounds, as segPlanPreds resolves
// the outer ones; false declines.
func (m *vdbe) segSemis(plan []segPlanSemi) ([]segSemi, bool) {
	out := make([]segSemi, len(plan))
	for i, sj := range plan {
		preds, ok := m.segPlanPreds(&segFilterPlan{preds: sj.bPreds})
		if !ok {
			return nil, false
		}
		out[i] = segSemi{keyCol: sj.keyCol, bRoot: sj.bRoot, bTbl: sj.bTbl, bPreds: preds}
	}
	return out, true
}

// segSemi is a segPlanSemi with its bounds resolved for this run.
type segSemi struct {
	keyCol int
	bRoot  uint32
	bTbl   *resolvedTable
	bPreds []segPred
}

// segCleanSegments is the segments that are the whole of rootPage's rows: a
// table with nothing pending in this pager's log, or a write session's store
// that holds no row of its own over such segments. ok is false otherwise.
func (p *ReadOnlyPager) segCleanSegments(rootPage uint32) ([]*segment, bool) {
	if p == nil || p.segs == nil {
		return nil, false
	}
	if p.segs.isLive(rootPage) {
		return p.segs.liveBaseSegments(rootPage)
	}
	if !p.segCleanFor(rootPage) {
		return nil, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	return segs, ok
}

// segSemiCountTable counts the rows of tRoot (whose table is tTbl) satisfying
// preds and every semi-join, or reports false to decline.
func (p *ReadOnlyPager) segSemiCountTable(tRoot uint32, tTbl *resolvedTable, preds []segPred, semis []segSemi) (int, bool) {
	tSegs, ok := p.segCleanSegments(tRoot)
	if !ok || tTbl == nil {
		return 0, false
	}
	for i := range preds {
		if preds[i].Val.Typ == Text && p.meta.encoding != UTF8 {
			return 0, false
		}
	}
	sets := make([]*segRowidSet, len(semis))
	for i, sj := range semis {
		set, ok := p.segSemiBuild(sj)
		if !ok {
			return 0, false
		}
		sets[i] = set
	}
	total := 0
	var flags [segFilterBatch]uint8
	for _, s := range tSegs {
		if len(s.cols) != len(tTbl.cols) || !segPredColsFilterable(s, preds) {
			return 0, false
		}
		// Each key column: the rowid itself, or a clean int64 block -- a NULL
		// or a value of another class could compare equal to a rowid through
		// affinity, which this does not model, so it declines.
		keys := make([][]int64, len(semis))
		for i, sj := range semis {
			if sj.keyCol == tTbl.ipkIndex {
				continue // nil: read the rowid
			}
			if !segPredColsFilterable(s, []segPred{{Col: sj.keyCol}}) {
				return 0, false
			}
			col, ok := s.Int64Column(sj.keyCol)
			if !ok {
				return 0, false
			}
			keys[i] = col
		}
		for lo := 0; lo < s.nRows; lo += segFilterBatch {
			live := flags[:min(lo+segFilterBatch, s.nRows)-lo]
			for i := range live {
				live[i] = 1
			}
			for _, pr := range preds {
				segApplyPred(s, pr, lo, live)
			}
			for r, f := range live {
				if f == 0 {
					continue
				}
				in := true
				for i, set := range sets {
					var k int64
					if keys[i] == nil {
						k = int64(s.Rowid(lo + r))
					} else {
						k = keys[i][lo+r]
					}
					if !set.has(k) {
						in = false
						break
					}
				}
				if in {
					total++
				}
			}
		}
	}
	return total, true
}

// segRowidSet is the inner side of a semi-join: the qualifying rowids, as a
// bitmap over their range when that is dense enough, else a map.
type segRowidSet struct {
	lo   int64
	bits []uint64
	m    map[int64]struct{}
}

func (s *segRowidSet) has(k int64) bool {
	if s.m != nil {
		_, ok := s.m[k]
		return ok
	}
	if k < s.lo {
		return false
	}
	i := uint64(k - s.lo)
	return i/64 < uint64(len(s.bits)) && s.bits[i/64]&(1<<(i%64)) != 0
}

// segSemiBuild scans the inner table's segments once into the set of rowids
// whose rows satisfy sj.bPreds. A predicate on the rowid alias reads the
// rowid, since the block stores that column as NULL; only an INTEGER bound is
// compared there, as an integer.
func (p *ReadOnlyPager) segSemiBuild(sj segSemi) (*segRowidSet, bool) {
	segs, ok := p.segCleanSegments(sj.bRoot)
	if !ok || sj.bTbl == nil {
		return nil, false
	}
	var colPreds, ridPreds []segPred
	for _, pr := range sj.bPreds {
		if pr.Col == sj.bTbl.ipkIndex {
			if pr.Val.Typ != Int {
				return nil, false
			}
			ridPreds = append(ridPreds, pr)
			continue
		}
		if pr.Val.Typ == Text && p.meta.encoding != UTF8 {
			return nil, false
		}
		colPreds = append(colPreds, pr)
	}
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for _, s := range segs {
		if s.nRows == 0 {
			continue
		}
		if len(s.cols) != len(sj.bTbl.cols) || !segPredColsFilterable(s, colPreds) {
			return nil, false
		}
		lo, hi = min(lo, int64(s.Rowid(0))), max(hi, int64(s.Rowid(s.nRows-1)))
	}
	set := &segRowidSet{lo: lo}
	if lo > hi {
		set.m = map[int64]struct{}{}
		return set, true
	}
	if span := uint64(hi - lo); span < 1<<28 {
		set.bits = make([]uint64, span/64+1)
	} else {
		set.m = map[int64]struct{}{}
	}
	var flags [segFilterBatch]uint8
	for _, s := range segs {
		for blo := 0; blo < s.nRows; blo += segFilterBatch {
			live := flags[:min(blo+segFilterBatch, s.nRows)-blo]
			for i := range live {
				live[i] = 1
			}
			for _, pr := range colPreds {
				segApplyPred(s, pr, blo, live)
			}
			for r, f := range live {
				if f == 0 {
					continue
				}
				rid := int64(s.Rowid(blo + r))
				match := true
				for _, pr := range ridPreds {
					if !pr.Op.satisfied(cmpInt64(rid, pr.Val.I)) {
						match = false
						break
					}
				}
				if !match {
					continue
				}
				if set.m != nil {
					set.m[rid] = struct{}{}
				} else {
					i := uint64(rid - lo)
					set.bits[i/64] |= 1 << (i % 64)
				}
			}
		}
	}
	return set, true
}
