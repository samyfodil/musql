package engine

import (
	"cmp"
	"math"
	"math/bits"
	"slices"
	"sort"
	"sync/atomic"
)

// segSortedSeeksServed counts the sorted seeks the segments served, so a test
// can tell them from the built rows'.
var segSortedSeeksServed atomic.Int64

// segSortedBuilds counts the segSortedRows built, so a test sees a reopened
// cursor take over the one it kept.
var segSortedBuilds atomic.Int64

// segBloomsBuilt counts the filters buildBloom made, so a test sees one
// answer its probes.
var segBloomsBuilt atomic.Int64

// segSortedRows is OpSortedSeek's index when the segments serve it: every row
// of the table's segments, as (segment, row), in the order of the seek's
// INTEGER column, ties in rowid order -- the order sortRowsBy's stable sort of
// the built rows gives. A seek positions the cursor on a run of segment rows,
// so the table is neither built nor normalized. ok is false when the segments
// cannot serve it, remembered so the next seek does not ask again.
type segSortedRows struct {
	hint *sortedSeekHint
	ok   bool
	keys []int64
	// list is the table's segments, and seg and rows each position's segment,
	// by its index in list, and row in it: no pointer per row.
	list []*segment
	seg  []uint16
	rows []int32
	// one is the table's one segment, when it is: then perm, its rows in the
	// column's order, and block, the column, are the segment's own (sortedOn),
	// and keys, list, seg and rows are not built.
	one   *segment
	perm  []int32
	block []int64
	// order is the automatic index key a run's rows follow, when the seek
	// stands in for the OpAutoIndexOrder that would have built and sorted the
	// table; ordered marks the runs already put in it, by first position.
	order *autoIndexKey
	// narrowKey sorts a run by the seek's further columns, then the rowid,
	// where no automatic index order does: a run's rows equal on them all
	// are then one stretch, still in rowid order, which narrow finds.
	narrowKey *autoIndexKey
	tbl       *resolvedTable
	enc       TextEncoding
	ordered   map[int]bool
	// distinct is how many values the column holds, counted the first time
	// shortRuns asks; 0 until then.
	distinct int
	// seeks is how many seeks the VDBE made through it, which decides when
	// native code's own index of the table is worth building
	// (sortedSeekServed).
	seeks int
	// from is the segments it was built from, which a cursor reopened on
	// the table checks before taking it over (segSortedStill).
	from []*segment
	// moreHint is the hint moreOrder last answered, with moreOrd and moreOK
	// its answer: a seek asks once per probe.
	moreHint *sortedSeekHint
	moreOrd  []int
	moreOK   bool
	// bloom holds every row's key, the column and the hint's further columns
	// as int64s, hashed (bloomKey): a probe whose key it lacks equals no row,
	// and is answered without a search, as C's OP_Filter answers one before
	// it searches its automatic index. Built once most of the first probes
	// have missed (misses); bloomTried stops a second attempt.
	bloom      []uint64
	bloomMask  uint64
	bloomTried bool
	misses     int
}

// segBloomMinRows is the fewest rows a table needs for a filter over its keys:
// below it, the search the filter saves costs no more than the hash.
const segBloomMinRows = 1024

// maxSegSortedRun bounds the rows a sorted seek keyed on further columns
// visits per probe when the segments serve it on its first column alone.
const maxSegSortedRun = 256

// shortRuns reports whether the column's runs average at most
// maxSegSortedRun rows. A seek keyed on further columns too is served on
// this one alone -- a superset of its rows, which the level's loop narrows,
// as it tests every conjunct -- only where that superset stays small.
func (s *segSortedRows) shortRuns() bool { return s.runsAtMost(maxSegSortedRun) }

// runsAtMost reports whether the column's runs average at most m rows.
func (s *segSortedRows) runsAtMost(m int) bool {
	n := len(s.keys)
	key := func(i int) int64 { return s.keys[i] }
	if s.one != nil {
		n, key = len(s.perm), func(i int) int64 { return s.block[s.perm[i]] }
	}
	if s.distinct == 0 {
		for i := range n {
			if i == 0 || key(i) != key(i-1) {
				s.distinct++
			}
		}
	}
	return n <= s.distinct*m
}

// segSortable reports whether cur is a table's one list of rows, which
// OpSortedSeek asks the segments to serve (segSortedSeek) before building it.
func (cur *vdbeCursor) segSortable() bool {
	return cur.rowStore == nil && !cur.streamable && !cur.seekReseek && !cur.refillEach && cur.sub == nil && cur.vt == nil &&
		(!cursorHasNoBtree(cur) || cur.fill != nil)
}

// segSortedSeek positions cur on the rows whose column h.col equals probe,
// straight from the segments, as sortedSeek narrows built rows. false, with
// nothing changed, where the segments are not the whole table or the column
// is not an INTEGER block in every one of them.
//
// An OpAutoIndexOrder waiting on cur is taken over rather than applied: a run
// of the built rows, once sorted by the key, is those rows in the key's order,
// so each run is put in that order here, without building the table.
//
// A key of further columns is served on its first alone where its runs are
// short (shortRuns): the run holds every row equal on all of them, in the
// order they would come, and the loop drops the rest.
func (cur *vdbeCursor) segSortedSeek(h *sortedSeekHint, probe Value, regs []Value, enc TextEncoding) bool {
	s, orderOp, ok := cur.segSortedReady(h, enc)
	if !ok {
		return false
	}
	s.seeks++
	if !s.bloomTried && s.seeks >= 64 && s.misses*2 >= s.seeks {
		s.buildBloom()
	}
	src := &cur.pointSrc
	if s.bloom != nil && !s.bloomHas(h, probe, regs) {
		cur.serveSegRun(orderOp, src.listSegs[:0], src.listRows[:0])
		return true
	}
	lo, hi := s.find(probe)
	s.orderRun(lo, hi)
	if ord, ok := s.moreOrder(h); ok {
		lo, hi = s.narrow(lo, hi, h, ord, regs)
	}
	if lo == hi {
		s.misses++
	}
	segs, rows := src.listSegs[:0], src.listRows[:0]
	if s.one != nil {
		for _, r := range s.perm[lo:hi] {
			segs, rows = append(segs, s.one), append(rows, int(r))
		}
	} else {
		for i := lo; i < hi; i++ {
			segs, rows = append(segs, s.list[s.seg[i]]), append(rows, int(s.rows[i]))
		}
	}
	cur.serveSegRun(orderOp, segs, rows)
	return true
}

// segSortedReady is segSortedSeek short of a probe: the cursor's
// segSortedRows for h and the automatic index order its runs take, made
// once, and whether the segments serve h's seeks.
func (cur *vdbeCursor) segSortedReady(h *sortedSeekHint, enc TextEncoding) (*segSortedRows, *Instruction, bool) {
	if h.built || cur.materialized || cur.fill != nil || cur.ranged || cur.seekConfigured || cur.idxSeekConfigured || cur.seekSetConfigured ||
		cur.segSorted == nil && (cur.segSrc != nil || cur.keyListReady) {
		return nil, nil, false
	}
	orderOp := cur.segOrder
	if cur.pendingOrder != nil {
		orderOp = cur.pendingOrder
	}
	var key *autoIndexKey
	if orderOp != nil {
		key = orderOp.P4.(*autoIndexKey)
	}
	s := cur.segSorted
	if s == nil || !s.hint.sameSort(h) || s.order != key {
		// One kept for key was made only after this test passed.
		if key != nil && !segRunOrderable(cur.tbl, key) {
			return nil, nil, false
		}
		if k := cur.keptSorted; k != nil && k.ok && k.hint.sameSort(h) && k.order == key && k.enc == enc && cur.pager.segSortedStill(cur, k) {
			s = k
		} else {
			s = cur.pager.segSortedRowsFor(cur, h)
			if s.ok && key != nil && s.one != nil {
				s.perm = slices.Clone(s.perm) // the segment's own, shared
			}
			s.order, s.tbl, s.enc = key, cur.tbl, enc
			if key == nil {
				s.narrowKey = narrowKeyFor(cur.tbl, h)
			}
		}
		cur.segSorted, cur.keptSorted = s, nil
	}
	if !s.ok {
		return nil, nil, false
	}
	if _, narrows := s.moreOrder(h); len(h.more) > 0 && !narrows && !s.shortRuns() {
		return nil, nil, false
	}
	return s, orderOp, true
}

// serveSegRun positions cur before a seek's run of segment rows, taking
// over the automatic index order orderOp, as segSortedSeek leaves it.
func (cur *vdbeCursor) serveSegRun(orderOp *Instruction, segs []*segment, rows []int) {
	cur.segOrder, cur.pendingOrder = orderOp, nil
	src := &cur.pointSrc
	*src = segRowSource{list: true, listSegs: segs, listRows: rows, listOne: oneSegment(segs), listKeys: src.listKeys, listPerm: src.listPerm,
		listHits: src.listHits, cols: segColPlansFor(cur.tbl)}
	cur.seekGen = seekGens.Add(1)
	cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
	cur.segSrc, cur.segCur, cur.segSrcCols = src, nil, src.cols
	cur.streamErrPending = nil
	cur.pos = -1
	cur.rowidNull = false
	cur.keyListReady = true
	segSortedSeeksServed.Add(1)
}

// unserveSegSorted takes back a cursor segSortedSeek positioned when the
// segments stop serving its seeks: its rows are built, in the order the seek
// took over.
func (cur *vdbeCursor) unserveSegSorted() {
	if cur.segSorted == nil || !cur.keyListReady {
		return
	}
	cur.keyListReady, cur.segSrc, cur.segCur, cur.segSorted, cur.pos = false, nil, nil, nil, -1
	if cur.segOrder != nil {
		cur.pendingOrder, cur.segOrder = cur.segOrder, nil
	}
}

// segRunOrderable reports whether segSortedRows.orderRun can put a run in
// key's order: plain columns of the table, or the rowid.
func segRunOrderable(tbl *resolvedTable, key *autoIndexKey) bool {
	if key.multiOr != nil || key.exprs != nil || len(key.cols) == 0 || len(key.colls) < len(key.cols) {
		return false
	}
	for _, c := range key.cols {
		if c != -1 && (c < 0 || c >= len(tbl.cols)) {
			return false
		}
	}
	return true
}

// orderRun puts the run [lo, hi) in s.order's order, as autoIndexOrderCursor
// orders the built rows: the key's values as the built rows hold them, ties
// in the run's own order, which is the rowids'.
func (s *segSortedRows) orderRun(lo, hi int) {
	key := s.runKey()
	if key == nil || hi-lo < 2 || s.ordered[lo] {
		return
	}
	n := hi - lo
	nk := len(key.cols)
	// The key's leading columns that are the rowid or an INTEGER block with
	// no exception in every row's segment are read as int64s, without
	// decoding; the rest are read only for rows equal on those.
	ints, nulls := make([]int64, n*nk), make([]bool, n*nk)
	p := nk
	for i := range n {
		sg, r := s.at(lo + i)
		for k, c := range key.cols[:p] {
			if c < 0 || c == s.tbl.ipkIndex {
				ints[i*nk+k] = int64(sg.Rowid(r))
				continue
			}
			b, ok := sg.Int64Column(c)
			if !ok || sg.hasException(c) || s.tbl.cols[c].Aff == affReal {
				p = k
				break
			}
			if sg.cols[c].nullOff != 0 && sg.isNull(c, r) {
				nulls[i*nk+k] = true
			} else {
				ints[i*nk+k] = b[r]
			}
		}
	}
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	slices.SortFunc(perm, func(a, b int) int {
		for k := range nk {
			var c int
			if k < p {
				x, y := a*nk+k, b*nk+k
				switch {
				case nulls[x] && nulls[y]:
				case nulls[x]:
					c = -1
				case nulls[y]:
					c = 1
				default:
					c = cmp.Compare(ints[x], ints[y])
				}
			} else {
				c = compareValuesCollatedEnc(s.cell(lo+a, key.cols[k]), s.cell(lo+b, key.cols[k]), key.colls[k], s.enc)
			}
			if c != 0 {
				if k < len(key.desc) && key.desc[k] {
					return -c // as autoIndexSortValues explains
				}
				return c
			}
		}
		return a - b
	})
	if s.one != nil {
		run := slices.Clone(s.perm[lo:hi])
		for i, j := range perm {
			s.perm[lo+i] = run[j]
		}
	} else {
		seg, rows := slices.Clone(s.seg[lo:hi]), slices.Clone(s.rows[lo:hi])
		for i, j := range perm {
			s.seg[lo+i], s.rows[lo+i] = seg[j], rows[j]
		}
	}
	if s.ordered == nil {
		s.ordered = map[int]bool{}
	}
	s.ordered[lo] = true
}

// moreOrder is the seek's further columns in the order s.order sorts a run
// by them, as indexes into h.more, when it sorts by them first: then the
// rows of a run equal on them all are one stretch of it (narrow).
func (s *segSortedRows) moreOrder(h *sortedSeekHint) ([]int, bool) {
	if len(h.more) == 0 || s.runKey() == nil {
		return nil, false
	}
	if s.moreHint != h {
		s.moreOrd, s.moreOK = s.moreOrderOf(h)
		s.moreHint = h
	}
	return s.moreOrd, s.moreOK
}

// moreOrderOf is moreOrder worked out.
func (s *segSortedRows) moreOrderOf(h *sortedSeekHint) ([]int, bool) {
	key := s.runKey()
	ord := make([]int, 0, len(h.more))
	for k, c := range key.cols {
		if len(ord) == len(h.more) {
			break
		}
		if c == h.col {
			continue
		}
		j := slices.IndexFunc(h.more, func(m sortedSeekCol) bool { return m.col == c })
		if j < 0 || slices.Contains(ord, j) || k < len(key.desc) && key.desc[k] || k >= len(key.colls) ||
			!equalFoldName(effectiveCollation(key.colls[k]), effectiveCollation(h.more[j].coll)) {
			return nil, false
		}
		ord = append(ord, j)
	}
	return ord, len(ord) == len(h.more)
}

// narrow is the stretch of the run [lo, hi), in s.order's order, whose
// further columns equal their keys in regs, compared as the built rows'
// seek compares them (sortedRows.findTuple). A NULL key equals nothing.
func (s *segSortedRows) narrow(lo, hi int, h *sortedSeekHint, ord []int, regs []Value) (int, int) {
	keys := make([]Value, len(ord))
	for i, j := range ord {
		mc := h.more[j]
		if mc.reg < 0 || mc.reg >= len(regs) {
			return lo, hi
		}
		if keys[i] = applyAffinityToValue(regs[mc.reg], mc.aff); keys[i].Typ == Null {
			return lo, lo
		}
	}
	cmpAt := func(i int) int {
		for k, j := range ord {
			mc := h.more[j]
			c, ok := 0, false
			if keys[k].Typ == Int {
				c, ok = s.cmpCellInt(i, mc.col, keys[k].I)
			}
			if !ok {
				c = compareValuesCollatedEnc(s.cell(i, mc.col), keys[k], mc.coll, s.enc)
			}
			if c != 0 {
				return c
			}
		}
		return 0
	}
	a := lo + sort.Search(hi-lo, func(i int) bool { return cmpAt(lo+i) >= 0 })
	b := a + sort.Search(hi-a, func(i int) bool { return cmpAt(a+i) > 0 })
	return a, b
}

// cmpCellInt compares column c of the row at position i with k, where the row
// holds an INTEGER in the column's block, as orderRun reads it: no Value is
// decoded. false where it does not.
func (s *segSortedRows) cmpCellInt(i, c int, k int64) (int, bool) {
	sg, r := s.at(i)
	if c < 0 || c == s.tbl.ipkIndex {
		return cmp.Compare(int64(sg.Rowid(r)), k), true
	}
	if s.tbl.cols[c].Aff == affReal || sg.hasException(c) {
		return 0, false
	}
	b, ok := sg.Int64Column(c)
	if !ok || r >= len(b) || sg.cols[c].nullOff != 0 && sg.isNull(c, r) {
		return 0, false
	}
	return cmp.Compare(b[r], k), true
}

// cell is column c of the row at position i, as orderRun reads it.
func (s *segSortedRows) cell(i, c int) Value {
	sg, r := s.at(i)
	if c < 0 || c == s.tbl.ipkIndex {
		return Value{Typ: Int, I: int64(sg.Rowid(r))}
	}
	v := sg.Value(c, r)
	if v.Typ == Int && s.tbl.cols[c].Aff == affReal {
		v = Value{Typ: Float, F: float64(v.I)}
	}
	return v
}

// at is the segment and row at position i of the column's order.
func (s *segSortedRows) at(i int) (*segment, int) {
	if s.one != nil {
		return s.one, int(s.perm[i])
	}
	return s.list[s.seg[i]], int(s.rows[i])
}

// segSortedRowsFor builds cur's segSortedRows for h: the committed segments,
// with nothing in the log or the session's store over them, ascending by
// rowid, each as wide as the table and holding h.col as an int64 block with no
// exception. A NULL equals no probe, so its rows are left out. A REAL column's
// values are read as REALs, and the INTEGER PRIMARY KEY's are the rowids; both
// are left to the built rows.
func (p *ReadOnlyPager) segSortedRowsFor(cur *vdbeCursor, h *sortedSeekHint) *segSortedRows {
	segSortedBuilds.Add(1)
	s := &segSortedRows{hint: h}
	segs, ok := p.segSortedSegs(cur, h)
	if !ok {
		return s
	}
	s.from = slices.Clone(segs)
	n, nonEmpty := 0, 0
	var one *segment
	for _, sg := range segs {
		if sg == nil || len(sg.cols) != len(cur.tbl.cols) {
			return s
		}
		n += sg.nRows
		if sg.nRows > 0 {
			nonEmpty, one = nonEmpty+1, sg
		}
	}
	if nonEmpty == 1 {
		// sortedOn's stable sort keeps a segment's rows, which are in rowid
		// order, in that order among equal keys.
		if blocks, perm, ok := one.sortedOn([]int{h.col}); ok {
			s.one, s.perm, s.block, s.ok = one, perm, blocks[0], true
			return s
		}
	}
	if len(segs) > 1<<16 {
		return s
	}
	keys, kseg, krows := make([]int64, 0, n), make([]uint16, 0, n), make([]int32, 0, n)
	var prevRid uint64
	for si, sg := range segs {
		if sg.nRows == 0 {
			continue
		}
		if len(keys) > 0 && sg.Rowid(0) <= prevRid {
			return s
		}
		b, ok := sg.Int64Column(h.col)
		if !ok || len(b) < sg.nRows || sg.hasException(h.col) {
			return s
		}
		nulls := sg.cols[h.col].nullOff != 0
		for i := range sg.nRows {
			if !nulls || !sg.isNull(h.col, i) {
				keys, kseg, krows = append(keys, b[i]), append(kseg, uint16(si)), append(krows, int32(i))
			}
		}
		prevRid = sg.Rowid(sg.nRows - 1)
	}
	perm := radixSortInt64(keys)
	s.list, s.keys, s.seg, s.rows = slices.Clone(segs), keys, make([]uint16, len(perm)), make([]int32, len(perm))
	for i, j := range perm {
		s.seg[i], s.rows[i] = kseg[j], krows[j]
	}
	s.ok = true
	return s
}

// segSortedSegs is the committed segments segSortedRowsFor reads for cur's
// table and h's column, or false where they are not the whole table.
func (p *ReadOnlyPager) segSortedSegs(cur *vdbeCursor, h *sortedSeekHint) ([]*segment, bool) {
	if !p.segSeekable(cur) || h.col < 0 || h.col >= len(cur.tbl.cols) || h.col == cur.tbl.ipkIndex || cur.tbl.cols[h.col].Aff == affReal {
		return nil, false
	}
	root := cur.tbl.root
	if p.segs.isLive(root) {
		return p.segs.liveBaseSegments(root)
	}
	return p.segs.byRoot[root], p.segs.cleanFor(root)
}

// segSortedStill reports whether s, built for cur's table, still holds its
// rows: the segments it was built from are still the whole table.
func (p *ReadOnlyPager) segSortedStill(cur *vdbeCursor, s *segSortedRows) bool {
	segs, ok := p.segSortedSegs(cur, s.hint)
	return ok && slices.Equal(segs, s.from)
}

// find is the run of rows whose key equals probe once probe takes the
// comparison's affinity. Every key is an INTEGER: a REAL equals one only when
// it is that integer exactly, and NULL, TEXT and BLOB equal none.
func (s *segSortedRows) find(probe Value) (int, int) {
	k, ok := segSeekInt(applyAffinityToValue(probe, s.hint.aff))
	if !ok {
		return 0, 0
	}
	if s.one != nil {
		lo, _ := slices.BinarySearchFunc(s.perm, k, func(r int32, k int64) int { return cmp.Compare(s.block[r], k) })
		return lo, runEnd(lo, len(s.perm), func(i int) bool { return s.block[s.perm[i]] > k })
	}
	lo, _ := slices.BinarySearch(s.keys, k)
	return lo, runEnd(lo, len(s.keys), func(i int) bool { return s.keys[i] > k })
}

// segSeekInt is the INTEGER a key equals, when it equals one: a REAL only
// when it is that integer exactly, and NULL, TEXT and BLOB none.
func segSeekInt(v Value) (int64, bool) {
	switch v.Typ {
	case Int:
		return v.I, true
	case Float:
		f := v.F
		if f != math.Trunc(f) || f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
			return 0, false
		}
		return int64(f), true
	}
	return 0, false
}

// bloomKey mixes one more of a key's int64s into its hash.
func bloomKey(x uint64, v int64) uint64 {
	x ^= uint64(v)
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// buildBloom fills s.bloom from every row the segments hold, when the column
// and each further column of s.hint are INTEGER blocks with no exception
// (or the rowid) in every segment: then a key that is no tuple of int64s
// equals no row, and one that is equals only rows its hash marks. A row with
// a NULL in its key equals no probe and is left out.
func (s *segSortedRows) buildBloom() {
	s.bloomTried = true
	cols := []int{s.hint.col}
	for _, mc := range s.hint.more {
		if mc.col >= 0 && mc.col != s.tbl.ipkIndex && (mc.col >= len(s.tbl.cols) || s.tbl.cols[mc.col].Aff == affReal) {
			return
		}
		cols = append(cols, mc.col)
	}
	n := 0
	for _, sg := range s.from {
		n += sg.nRows
	}
	if n < segBloomMinRows {
		return
	}
	m := uint64(1) << bits.Len64(uint64(n)*8-1)
	bloom := make([]uint64, m/64)
	blocks := make([][]int64, len(cols))
	for _, sg := range s.from {
		if sg.nRows == 0 {
			continue
		}
		for j, c := range cols {
			blocks[j] = nil
			if c < 0 || c == s.tbl.ipkIndex {
				continue
			}
			b, ok := sg.Int64Column(c)
			if !ok || len(b) < sg.nRows || sg.hasException(c) {
				return
			}
			blocks[j] = b
		}
	rows:
		for r := range sg.nRows {
			var x uint64
			for j, c := range cols {
				v := int64(sg.Rowid(r))
				if blocks[j] != nil {
					if sg.cols[c].nullOff != 0 && sg.isNull(c, r) {
						continue rows
					}
					v = blocks[j][r]
				}
				x = bloomKey(x, v)
			}
			b1, b2 := x&(m-1), x>>32&(m-1)
			bloom[b1/64] |= 1 << (b1 % 64)
			bloom[b2/64] |= 1 << (b2 % 64)
		}
	}
	s.bloom, s.bloomMask = bloom, m-1
	segBloomsBuilt.Add(1)
}

// bloomHas reports whether a row may equal the probe and h's further keys in
// regs, each taking its comparison's affinity: false only where none does.
func (s *segSortedRows) bloomHas(h *sortedSeekHint, probe Value, regs []Value) bool {
	k, ok := segSeekInt(applyAffinityToValue(probe, h.aff))
	if !ok {
		return false
	}
	x := bloomKey(0, k)
	for _, mc := range h.more {
		if mc.reg < 0 || mc.reg >= len(regs) {
			return true
		}
		v, ok := segSeekInt(applyAffinityToValue(regs[mc.reg], mc.aff))
		if !ok {
			return false
		}
		x = bloomKey(x, v)
	}
	b1, b2 := x&s.bloomMask, x>>32&s.bloomMask
	return s.bloom[b1/64]&(1<<(b1%64)) != 0 && s.bloom[b2/64]&(1<<(b2%64)) != 0
}

// runEnd is the first position from lo to n that is past, which holds from
// some position on: galloped to, so a short run costs a few probes and a run
// of thousands of rows a few dozen.
func runEnd(lo, n int, past func(int) bool) int {
	bound, step := lo, 1
	for bound < n && !past(bound) {
		lo, bound, step = bound+1, bound+step, step*2
	}
	bound = min(bound, n)
	return lo + sort.Search(bound-lo, func(i int) bool { return past(lo + i) })
}

// runKey is the order a run is put in: the automatic index's, or narrowKey.
func (s *segSortedRows) runKey() *autoIndexKey {
	if s.order != nil {
		return s.order
	}
	return s.narrowKey
}

// narrowKeyFor is the key that sorts a run of h's seek by its further
// columns, then the rowid, as the built rows' seek sorts them
// (sortRowsBy); nil where h has none or one is not a column.
func narrowKeyFor(tbl *resolvedTable, h *sortedSeekHint) *autoIndexKey {
	if len(h.more) == 0 {
		return nil
	}
	key := &autoIndexKey{}
	for _, m := range h.more {
		if m.col < 0 || m.col >= len(tbl.cols) {
			return nil
		}
		key.cols, key.colls = append(key.cols, m.col), append(key.colls, m.coll)
	}
	key.cols, key.colls = append(key.cols, -1), append(key.colls, "")
	return key
}
