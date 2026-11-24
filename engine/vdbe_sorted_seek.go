package engine

import (
	"cmp"
	"slices"
	"sort"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// sortedRows is OpSortedSeek's state on a cursor (vdbeCursor.sorted).
type sortedRows struct {
	hint *sortedSeekHint
	// ok is false when the rows could not be sorted; the cursor then keeps
	// all of them.
	ok bool
	// rows and ids are the cursor's rows sorted by hint.col, stably, and pos
	// each one's place in orig/origIDs, the list as the cursor had it.
	rows    [][]Value
	ids     []uint64
	pos     []int32
	orig    [][]Value
	origIDs []uint64
	// ints is rows' key column when every value is an INTEGER.
	ints []int64
	// keyOrder is the order rows compares the hint's key columns in (0 is
	// hint.col, j its more[j-1]); nil is that order itself. A probe wants
	// the run equal on every column, which any order groups alike.
	keyOrder []int
	// base is where the current run starts in rows.
	base int
	// vs is rows, ids and ints as VSortSeek searches them (handSort).
	vs jit.VSort
	// keys is the probe's keys, and last the previous probe's (its bytes
	// copied, as a register's are reused) with the run it found: an inner
	// loop seeks with keys an outer row holds, the same each time round.
	// self is s while they are s's own: a copy (OpSortedSeek shares a sort
	// between cursors by value) starts them afresh.
	keys, last     []Value
	lastBuf        []byte
	lastLo, lastHi int
	self           *sortedRows
}

// probeKeys starts s.keys at probe, in buffers of s's own.
func (s *sortedRows) probeKeys(probe Value) {
	if s.self != s {
		s.self, s.keys, s.last, s.lastBuf = s, nil, nil, nil
	}
	s.keys = append(s.keys[:0], probe)
}

// lastRun is the run the previous probe found, when its keys are s.keys.
func (s *sortedRows) lastRun() (int, int, bool) {
	if len(s.last) != len(s.keys) || len(s.last) == 0 {
		return 0, 0, false
	}
	for i, k := range s.keys {
		if !valuesIdentical(k, s.last[i]) {
			return 0, 0, false
		}
	}
	return s.lastLo, s.lastHi, true
}

// keep makes lo..hi the run of s.keys, for the probe after.
func (s *sortedRows) keep(lo, hi int) (int, int) {
	s.last, s.lastBuf = append(s.last[:0], s.keys...), s.lastBuf[:0]
	for _, v := range s.last {
		if v.Typ == Text || v.Typ == Blob {
			s.lastBuf = append(s.lastBuf, v.S...)
		}
	}
	n := 0
	for i := range s.last {
		if v := &s.last[i]; v.Typ == Text || v.Typ == Blob {
			v.S = s.lastBuf[n : n+len(v.S) : n+len(v.S)]
			n += len(v.S)
		}
	}
	s.lastLo, s.lastHi = lo, hi
	return lo, hi
}

// sortedSeek narrows cur to the rows whose column h.col equals probe, sorting
// the full list by that column the first time.
func (cur *vdbeCursor) sortedSeek(h *sortedSeekHint, probe Value, regs []Value, enc TextEncoding) {
	s := cur.sorted
	if s == nil || !s.hint.sameSort(h) {
		cur.unsort()
		s = cur.sortRowsBy(h, enc)
		cur.sorted = s
	}
	if !s.ok {
		return
	}
	lo, hi := s.find(h, probe, regs, enc)
	cur.rows, cur.rowids, s.base = s.rows[lo:hi:hi], s.ids[lo:hi:hi], lo
}

// unsort gives cur back its full row list, as it was before any seek.
func (cur *vdbeCursor) unsort() {
	if s := cur.sorted; s != nil && s.ok {
		cur.rows, cur.rowids = s.orig, s.origIDs
	}
	cur.sorted = nil
}

// intSortKey is a row's leading key columns that every row holds as an
// INTEGER or a NULL (nulls, a bit per column), and its position.
type intSortKey struct {
	k     [4]int64
	nulls uint8
	p     int32
}

func (cur *vdbeCursor) sortRowsBy(h *sortedSeekHint, enc TextEncoding) *sortedRows {
	s := &sortedRows{hint: h}
	if !cur.materialized || cur.sub != nil || cur.vt != nil || cur.segSrc != nil || len(cur.rowids) != len(cur.rows) || len(cur.rows) > 1<<31-1 {
		return s
	}
	cur.normalizeRowsNow()
	n := len(cur.rows)
	k := 1 + len(h.more)
	keyCol := func(j int) (int, string) {
		if j == 0 {
			return h.col, h.coll
		}
		return h.more[j-1].col, h.more[j-1].coll
	}
	for _, row := range cur.rows {
		if h.col >= len(row) || slices.ContainsFunc(h.more, func(m sortedSeekCol) bool { return m.col >= len(row) }) {
			return s
		}
	}
	// The key columns every row holds as an INTEGER or a NULL, up to four, go
	// first and are copied out flat and compared as int64s: an integer
	// compares as one under any collating sequence (as autoIndexOrderCursor
	// does), and a NULL before it. Only rows they leave tied compare the rest
	// as Values.
	var order, rest []int
	for j := range k {
		col, _ := keyCol(j)
		if len(order) < len(intSortKey{}.k) && !slices.ContainsFunc(cur.rows, func(row []Value) bool { return row[col].Typ != Int && row[col].Typ != Null }) {
			order = append(order, j)
		} else {
			rest = append(rest, j)
		}
	}
	lead := len(order)
	order = append(order, rest...)
	keys := make([]intSortKey, n)
	for i, row := range cur.rows {
		for j := range lead {
			col, _ := keyCol(order[j])
			if v := row[col]; v.Typ == Null {
				keys[i].nulls |= 1 << uint(j)
			} else {
				keys[i].k[j] = v.I
			}
		}
		keys[i].p = int32(i)
	}
	// The original position breaks ties, so the order is total and a run keeps
	// the list's order.
	slices.SortFunc(keys, func(a, b intSortKey) int {
		for j := range lead {
			na, nb := a.nulls>>uint(j)&1, b.nulls>>uint(j)&1
			if c := cmp.Compare(nb, na); c != 0 {
				return c // a NULL first
			}
			if c := cmp.Compare(a.k[j], b.k[j]); na == 0 && c != 0 {
				return c
			}
		}
		for _, j := range order[lead:] {
			col, coll := keyCol(j)
			if c := compareValuesCollatedEnc(cur.rows[a.p][col], cur.rows[b.p][col], coll, enc); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.p, b.p)
	})
	pos := make([]int32, n)
	for i := range keys {
		pos[i] = keys[i].p
	}
	var ints []int64
	if lead == 1 && k == 1 && !slices.ContainsFunc(keys, func(e intSortKey) bool { return e.nulls != 0 }) {
		ints = make([]int64, n)
		for i := range keys {
			ints[i] = keys[i].k[0]
		}
	}
	s.rows, s.ids, s.ints = make([][]Value, n), make([]uint64, n), ints
	if !slices.IsSorted(order) {
		s.keyOrder = order
	}
	for i, p := range pos {
		s.rows[i], s.ids[i] = cur.rows[p], cur.rowids[p]
	}
	s.pos, s.orig, s.origIDs, s.ok = pos, cur.rows, cur.rowids, true
	return s
}

// find is the run of rows whose key column equals probe, and each further
// column the key in its register (regs), once every key takes its
// comparison's affinity, under its collation. A NULL equals nothing.
func (s *sortedRows) find(h *sortedSeekHint, probe Value, regs []Value, enc TextEncoding) (int, int) {
	probe = applyAffinityToValue(probe, h.aff)
	if probe.Typ == Null {
		return 0, 0
	}
	if len(h.more) > 0 {
		return s.findTuple(h, probe, regs, enc)
	}
	if s.ints != nil && probe.Typ == Int {
		lo, _ := slices.BinarySearch(s.ints, probe.I)
		hi := lo
		for hi < len(s.ints) && s.ints[hi] == probe.I {
			hi++
		}
		return lo, hi
	}
	s.probeKeys(probe)
	if lo, hi, ok := s.lastRun(); ok {
		return lo, hi
	}
	col, coll := s.hint.col, s.hint.coll
	lo := sort.Search(len(s.rows), func(i int) bool {
		return compareValuesCollatedEnc(s.rows[i][col], probe, coll, enc) >= 0
	})
	hi := lo + sort.Search(len(s.rows)-lo, func(i int) bool {
		return compareValuesCollatedEnc(s.rows[lo+i][col], probe, coll, enc) > 0
	})
	return s.keep(lo, hi)
}

// findTuple is find over the hint's further columns too.
func (s *sortedRows) findTuple(h *sortedSeekHint, probe Value, regs []Value, enc TextEncoding) (int, int) {
	s.probeKeys(probe)
	for _, mc := range h.more {
		k := applyAffinityToValue(regs[mc.reg], mc.aff)
		if k.Typ == Null {
			return 0, 0
		}
		s.keys = append(s.keys, k)
	}
	if lo, hi, ok := s.lastRun(); ok {
		return lo, hi
	}
	keys := s.keys
	cmpKey := func(row []Value, j int) int {
		if j == 0 {
			return compareValuesCollatedEnc(row[h.col], probe, h.coll, enc)
		}
		mc := h.more[j-1]
		return compareValuesCollatedEnc(row[mc.col], keys[j], mc.coll, enc)
	}
	cmpRow := func(row []Value) int {
		if s.keyOrder != nil {
			for _, j := range s.keyOrder {
				if c := cmpKey(row, j); c != 0 {
					return c
				}
			}
			return 0
		}
		for j := range len(h.more) + 1 {
			if c := cmpKey(row, j); c != 0 {
				return c
			}
		}
		return 0
	}
	lo := sort.Search(len(s.rows), func(i int) bool { return cmpRow(s.rows[i]) >= 0 })
	hi := lo + sort.Search(len(s.rows)-lo, func(i int) bool { return cmpRow(s.rows[lo+i]) > 0 })
	return s.keep(lo, hi)
}

// movedRegs is h with its further keys' registers renumbered by reg, for a
// program whose instructions another takes in (existsInline, recInline).
func (h *sortedSeekHint) movedRegs(reg func(int) int) *sortedSeekHint {
	if len(h.more) == 0 {
		return h
	}
	moved := *h
	moved.more = slices.Clone(h.more)
	for i := range moved.more {
		moved.more[i].reg = reg(moved.more[i].reg)
	}
	return &moved
}

// sameSort reports whether b sorts the rows as a does: the same columns under
// the same collations and affinities. A level a LEFT JOIN emits twice, for
// its matched rows and for its NULL row, seeks with two hints that do.
func (a *sortedSeekHint) sameSort(b *sortedSeekHint) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.col != b.col || a.coll != b.coll || a.aff != b.aff || len(a.more) != len(b.more) {
		return false
	}
	for i := range a.more {
		if x, y := a.more[i], b.more[i]; x.col != y.col || x.coll != y.coll || x.aff != y.aff {
			return false
		}
	}
	return true
}

// lowerSortSeeks lowers each OpSortedSeek of a cursor native code walks as a
// row list -- one lowerIdxSeeks left to the VDBE -- to jit.VSortSeek, and the
// cursor's OpIfEmpty to jit.VSortEmpty, when its key is one
// INTEGER-comparable column and every sorted seek of the cursor sorts alike.
// It returns those sorts by cursor (nil when there are none) and the pcs.
func lowerSortSeeks(prog *Program, vp []jit.VInsn, idxSpecs []*idxSeekSpec) (sorts []*sortedSeekHint, pcs []int) {
	sorts = make([]*sortedSeekHint, prog.NCursors)
	bad := make([]bool, prog.NCursors)
	for _, in := range prog.Insns {
		switch in.Op {
		case OpRightJoinMark, OpRightJoinSweepRewind, OpRightJoinSweepNext, OpRightJoinSweepOrder:
			if in.P1 >= 0 && in.P1 < len(bad) {
				bad[in.P1] = true
			}
		}
	}
	for _, in := range prog.Insns {
		if in.Op != OpSortedSeek || in.P1 < 0 || in.P1 >= len(sorts) {
			continue
		}
		c := in.P1
		h, ok := in.P4.(*sortedSeekHint)
		if !ok || in.P2 < 0 || in.P2 >= prog.NReg || h.built || h.col < 0 || len(h.more) > 0 ||
			h.aff != affNone && h.aff != affNumeric && h.aff != affInteger || c < len(idxSpecs) && idxSpecs[c] != nil ||
			sorts[c] != nil && !sorts[c].sameSort(h) {
			bad[c] = true
			continue
		}
		if sorts[c] == nil {
			sorts[c] = h
		}
	}
	for pc, in := range prog.Insns {
		if in.P1 < 0 || in.P1 >= len(sorts) || sorts[in.P1] == nil || bad[in.P1] {
			continue
		}
		switch {
		case in.Op == OpSortedSeek:
			vp[pc] = jit.VInsn{Op: jit.VSortSeek, A: in.P1, B: in.P2}
			pcs = append(pcs, pc)
		case in.Op == OpIfEmpty && in.P4 == nil:
			// A miss of the seek before it asks whether the whole list is
			// empty, which the sort answers.
			vp[pc] = jit.VInsn{Op: jit.VSortEmpty, A: in.P1, T: in.P2}
			pcs = append(pcs, pc)
		}
	}
	if len(pcs) == 0 {
		return nil, nil
	}
	for c := range sorts {
		if bad[c] {
			sorts[c] = nil
		}
	}
	return sorts, pcs
}

// handSort hands cur's sorted rows to native code for VSortSeek, when the
// VDBE's next OpSortedSeek of h would search them as they stand: sorted by h's
// key, every key an INTEGER (ints), and nothing to order or rebuild first.
func handSort(cur *vdbeCursor, jc *jit.VCursor, h *sortedSeekHint) {
	s := cur.sorted
	if s == nil || !s.ok || s.ints == nil || !s.hint.sameSort(h) || cur.pendingOrder != nil || cur.segSorted != nil ||
		cur.rowStore != nil || cur.streamable || cur.seekReseek || cur.refillEach || len(s.ints) != len(s.rows) || len(s.ids) != len(s.rows) {
		return
	}
	s.vs = jit.VSort{Ints: unsafe.Pointer(unsafe.SliceData(s.ints)), Rows: unsafe.Pointer(unsafe.SliceData(s.rows)),
		Ids: unsafe.Pointer(unsafe.SliceData(s.ids)), N: int64(len(s.ints))}
	jc.Sort, jc.SortLo = unsafe.Pointer(&s.vs), -1
}
