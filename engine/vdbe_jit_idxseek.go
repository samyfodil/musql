package engine

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"slices"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// idxSeekSpec is a cursor whose equality index seeks native code makes
// (jit.VSeekIdx): the indexed column, the order the seek's rows are visited
// in, and the program's slot-0 OpSeekIndexHint and OpAutoIndexOrder, which the
// VDBE runs to take the cursor back (syncIdxSeek).
type idxSeekSpec struct {
	col   int
	order *autoIndexKey
	seek  int
	aio   int // -1 when the seek has none
	// listed is whether the VDBE walks the seek's rows as a list of segment
	// rows (a correlated seek, or one in an automatic index's order), whose
	// rows it counts as served (segRowsServed); it builds the others.
	listed bool
	// open is the pc of the cursor's OpOpenRead, which native code makes as
	// a VOpen (with its OpClose a VClose), or -1.
	open int
	// pinned is the columns the seek's slots and its loop's first tests hold
	// equal (loopPinnedCols): rows sharing all of them, the only ones the
	// loop keeps, run in rowid order when the order has no other column.
	pinned []int
	// sorted is the OpSortedSeek a seek stands for, when it does: its index is
	// each segment's rows in the order of sortCols (segment.sortedOn), the
	// order segSortedSeek puts a run in, and the VDBE takes the cursor back on
	// native code's run (syncSortedSeek).
	sorted    *sortedSeekHint
	sortCols  []int
	sortColls []string
}

// lowerIdxSeeks lowers each integer equality index seek -- an OpSeekIndexHint's
// first slot, its other slots and an OpAutoIndexOrder, then the OpRewind -- to
// jit.VSeekIdx, gotos and jit.VSeekRewind, for a cursor every seek of which in
// the program is the same one and which native code seeks no other way. It
// returns the specs by cursor (nil when there are none), the seeks' pcs,
// native code's entries, and the pcs past each seek through its rewind, which
// must not be: the VDBE that runs a seek runs its OpAutoIndexOrder too.
func lowerIdxSeeks(prog *Program, vp []jit.VInsn, segCap []bool) (specs []*idxSeekSpec, pcs, inner []int) {
	if segCap == nil {
		return nil, nil, nil
	}
	type site struct {
		pc, end, c, key, t int
		sorted             bool
	}
	var sites []site
	specs = make([]*idxSeekSpec, len(segCap))
	bad := make([]bool, len(segCap))
	insns := prog.Insns
	for _, in := range insns {
		switch in.Op {
		case OpRightJoinMark, OpRightJoinSweepRewind, OpRightJoinSweepNext, OpRightJoinSweepOrder:
			if in.P1 >= 0 && in.P1 < len(bad) {
				bad[in.P1] = true
			}
		}
	}
	for pc, v := range vp {
		if v.Op == jit.VSeekRowid && v.A < len(bad) {
			bad[v.A] = true
		}
		in := &insns[pc]
		if in.Op == OpSortedSeek {
			spec, end, ok := sortedSeekSpec(insns, pc, segCap, prog.NReg)
			if !ok {
				if in.P1 >= 0 && in.P1 < len(bad) {
					bad[in.P1] = true
				}
				continue
			}
			c := in.P1
			if prev := specs[c]; prev != nil && (prev.sorted == nil || !prev.sorted.sameSort(spec.sorted) || prev.order != spec.order) {
				bad[c] = true
				continue
			}
			if specs[c] == nil {
				specs[c] = spec
			}
			sites = append(sites, site{pc, end, c, in.P2, insns[end].P2, true})
			continue
		}
		if in.Op != OpSeekIndexHint || in.P1 < 0 || in.P1 >= len(segCap) || !segCap[in.P1] || in.P2 < 0 || in.P2 >= prog.NReg {
			continue
		}
		c := in.P1
		h, ok := in.P4.(*indexSeekHint)
		if !ok || h.slot != 0 || h.in != 0 || h.leadingCol < 0 || h.aff != affNone && h.aff != affNumeric && h.aff != affInteger {
			continue
		}
		j := pc + 1
		pinned := []int{h.leadingCol}
		for j < len(insns) && insns[j].Op == OpSeekIndexHint && insns[j].P1 == c {
			h2, ok := insns[j].P4.(*indexSeekHint)
			if !ok || h2.slot == 0 || h2.in != 0 {
				break
			}
			if !h2.is {
				pinned = append(pinned, h2.leadingCol)
			}
			j++
		}
		spec := &idxSeekSpec{col: h.leadingCol, order: h.order, seek: pc, aio: -1, listed: in.P3 == 1, pinned: pinned, open: -1}
		if j < len(insns) && insns[j].Op == OpAutoIndexOrder && insns[j].P1 == c {
			k, ok := insns[j].P4.(*autoIndexKey)
			if !ok || h.order != nil {
				continue
			}
			spec.order, spec.aio, spec.listed = k, j, true
			j++
		}
		if j >= len(insns) || insns[j].Op != OpRewind || insns[j].P1 != c || insns[j].P4 != nil {
			continue
		}
		spec.pinned = append(spec.pinned, loopPinnedCols(insns, j+1, c)...)
		if o := spec.order; o != nil && (len(o.cols) == 0 || o.cols[0] != h.leadingCol || o.exprs != nil || o.multiOr != nil || o.rjSweep != nil) {
			continue
		}
		if prev := specs[c]; prev != nil && (prev.sorted != nil || prev.col != spec.col || prev.order != spec.order || prev.listed != spec.listed ||
			!slices.Equal(prev.pinned, spec.pinned)) {
			bad[c] = true
			continue
		}
		if specs[c] == nil {
			specs[c] = spec
		}
		sites = append(sites, site{pc, j, c, in.P2, insns[j].P2, false})
	}
	any := false
	for _, s := range sites {
		if bad[s.c] {
			continue
		}
		any = true
		if s.sorted {
			// A miss falls through to what comes before the rewind, which
			// takes the empty jump.
			vp[s.pc] = jit.VInsn{Op: jit.VSeekIdx, A: s.c, B: s.key, T: s.pc + 1}
			for q := s.pc + 1; q < s.end; q++ {
				inner = append(inner, q)
			}
			inner = append(inner, s.end)
			vp[s.end] = jit.VInsn{Op: jit.VSeekRewind, A: s.c, T: s.t}
			pcs = append(pcs, s.pc)
			continue
		}
		vp[s.pc] = jit.VInsn{Op: jit.VSeekIdx, A: s.c, B: s.key, T: s.t}
		for q := s.pc + 1; q < s.end; q++ {
			vp[q] = jit.VInsn{Op: jit.VGoto, T: q + 1}
			inner = append(inner, q)
		}
		inner = append(inner, s.end)
		vp[s.end] = jit.VInsn{Op: jit.VSeekRewind, A: s.c, T: s.t}
		// Only the seek is entered from the VDBE: entered later, native code
		// would skip what the VDBE had still to run of it.
		pcs = append(pcs, s.pc)
	}
	if !any {
		return nil, nil, nil
	}
	for c := range specs {
		if bad[c] {
			specs[c] = nil
		}
	}
	// A cursor's opens, all of one table, and its closes: native code makes
	// them too, so a seek in a subroutine opened per call stays native.
	for c, spec := range specs {
		if spec == nil || spec.sorted != nil {
			continue
		}
		var opens, closes []int
		same := true
		for pc, in := range insns {
			switch {
			case in.Op == OpOpenRead && in.P1 == c:
				if len(opens) > 0 {
					o := insns[opens[0]]
					same = same && in.P2 == o.P2 && in.P3 == o.P3 && in.P4 == o.P4 && in.P5 == o.P5
				}
				opens = append(opens, pc)
			case in.Op == OpClose && in.P1 == c:
				closes = append(closes, pc)
			case isCursorOpen(in.Op) && in.P1 == c:
				same = false
			}
		}
		if len(opens) == 0 || !same {
			continue
		}
		spec.open = opens[0]
		for _, q := range opens {
			vp[q] = jit.VInsn{Op: jit.VOpen, A: c}
			pcs = append(pcs, q)
		}
		for _, q := range closes {
			vp[q] = jit.VInsn{Op: jit.VClose, A: c}
			pcs = append(pcs, q)
		}
	}
	return specs, pcs, inner
}

// sortedSeekSpec is the spec of the OpSortedSeek at pc when native code can
// make it as a VSeekIdx, and the pc of its rewind: a seek on an INTEGER
// column of a segment table, nothing between it and its rewind but constants
// (a LEFT JOIN's match flag, a literal the level's test compares), and its
// run in rowid order or in that of the cursor's one automatic index key, run
// once after its one open and before any seek. The key's columns the seek
// holds to one value (its own, sortedSeekPins) do not order the rows the
// loop keeps; the others must be ascending and not REAL.
func sortedSeekSpec(insns []Instruction, pc int, segCap []bool, nReg int) (*idxSeekSpec, int, bool) {
	in := &insns[pc]
	c := in.P1
	h, ok := in.P4.(*sortedSeekHint)
	if !ok || in.P3 != 1 || c < 0 || c >= len(segCap) || !segCap[c] || in.P2 < 0 || in.P2 >= nReg || h.built || h.col < 0 ||
		h.aff != affNone && h.aff != affNumeric && h.aff != affInteger {
		return nil, 0, false
	}
	end := pc + 1
	for end < len(insns) {
		switch insns[end].Op {
		case OpInteger, OpInt64, OpNull, OpReal, OpString8:
			end++
			continue
		}
		break
	}
	if end >= len(insns) || insns[end].Op != OpRewind || insns[end].P1 != c || insns[end].P4 != nil {
		return nil, 0, false
	}
	open, aio := -1, -1
	for q, x := range insns {
		switch {
		case x.P1 != c:
		case isCursorOpen(x.Op):
			if open >= 0 || x.Op != OpOpenRead {
				return nil, 0, false
			}
			open = q
		case x.Op == OpAutoIndexOrder:
			if aio >= 0 {
				return nil, 0, false
			}
			aio = q
		}
	}
	tbl, _ := insns[max(open, 0)].P4.(*resolvedTable)
	if open < 0 || open > pc || tbl == nil || h.col >= len(tbl.cols) || h.col == tbl.ipkIndex || tbl.cols[h.col].Aff == affReal {
		return nil, 0, false
	}
	spec := &idxSeekSpec{col: h.col, seek: pc, aio: -1, open: -1, sorted: h, sortCols: []int{h.col}, sortColls: []string{""}}
	if aio >= 0 {
		key, _ := insns[aio].P4.(*autoIndexKey)
		if aio < open || aio > pc || key == nil || !segRunOrderable(tbl, key) || key.rjSweep != nil {
			return nil, 0, false
		}
		for i, col := range key.cols {
			desc := i < len(key.desc) && key.desc[i]
			if col < 0 || col == tbl.ipkIndex {
				// The rowid, last and ascending: a run keeps its rows in rowid
				// order. (reverse_unordered_selects makes it descending.)
				if i != len(key.cols)-1 || desc {
					return nil, 0, false
				}
				continue
			}
			if col == h.col || sortedSeekPins(h, col, key.colls[i]) {
				continue // one value, whichever way it runs
			}
			if desc || tbl.cols[col].Aff == affReal {
				return nil, 0, false
			}
			spec.sortCols, spec.sortColls = append(spec.sortCols, col), append(spec.sortColls, key.colls[i])
		}
		spec.order = key
	}
	return spec, end, true
}

// sortedSeekPins reports whether a sorted seek holds column col to one value
// under collation coll: every row its loop keeps has it, so a run's order
// does not depend on it.
func sortedSeekPins(h *sortedSeekHint, col int, coll string) bool {
	for _, m := range h.more {
		if m.col == col && equalFoldName(effectiveCollation(m.coll), effectiveCollation(coll)) {
			return true
		}
	}
	return false
}

// openedTab is the table cursor c's OpOpenRead opens, as tabFor last worked
// it out, while that stands; nil when it does not or never was.
func (jx *vmJIT) openedTab(m *vdbe, c int) *vmTab {
	op := &unsafe.Slice(jx.insns, jx.n)[jx.idxSpecs[c].open]
	tbl, _ := op.P4.(*resolvedTable)
	rp, err := m.dbPager(op.P3)
	if tbl == nil || err != nil || rp == nil || rp.segs == nil {
		return nil
	}
	if t := jx.tabs[c].Load(); t.current(rp.segs, tbl.root) && !t.none {
		return t
	}
	return nil
}

// syncSeekOpen takes back a seek cursor native code may have opened or closed
// (jit.VReopenSeek): an open of native code's is the VDBE's OpOpenRead, and a
// cursor native code left closed is closed. It reports whether that is all
// there is to take back.
func (jx *vmJIT) syncSeekOpen(m *vdbe, c int, jc *jit.VCursor) bool {
	spec := jx.idxSpecs[c]
	if jc.Reopen == jit.VReopenSeekOpened {
		insns := unsafe.Slice(jx.insns, jx.n)
		if err := m.execOpenRead(&insns[spec.open], insns); err != nil {
			if m.jitErr == nil {
				m.jitErr = err
			}
			return true
		}
		m.jsnap[c].segLive, m.jsnap[c].live = false, false
		if jc.Ok != 0 && jc.State == 1 {
			if spec.listed {
				segRowsServed.Add(jc.Served)
			}
			return true
		}
	}
	if jc.Ok != 0 {
		return false
	}
	if jc.Dirty != 0 || jc.Reopen == jit.VReopenSeekOpened {
		if spec.listed {
			served := jc.Served
			if jc.State == 2 || jc.State == 4 || jc.State == 5 {
				served += jc.Last + 1
			}
			segRowsServed.Add(served)
		}
		if cur := m.cursors[c]; cur != nil && !cur.closed {
			cur.close()
		}
	}
	return true
}

// vmIdx is a cursor's table as VSeekIdx searches it (jit.VIdx, one a
// segment): each segment's index over the seek's column, each value's rows in
// the order the seek visits them. Worked out for a vmTab, it stands while that
// does; none is a table native code may not seek this way.
type vmIdx struct {
	tab  *vmTab
	segs []*segment
	keep [][]int // the widened rows each vidx entry points into
	// vals and starts are a sorted seek's own values and run starts, which
	// no file holds (buildSortedVMIdx).
	vals   [][]int64
	starts [][]int32
	// rowSegs and segPtrs are, for an index over several segments (one
	// vidx whose Segs is segPtrs), the segment of each of keep[0]'s rows.
	rowSegs []*segment
	segPtrs []unsafe.Pointer
	vidx    []jit.VIdx
	none    bool
}

// idxFor returns cursor c's table as VSeekIdx searches it, or nil.
func (jx *vmJIT) idxFor(c int, t *vmTab, enc TextEncoding, s *segSortedRows) *vmIdx {
	x := jx.idxs[c].Load()
	if x == nil || x.tab != t {
		x = buildVMIdx(t, jx.idxSpecs[c], enc, s)
		jx.idxs[c].Store(x)
	}
	if x.none {
		return nil
	}
	return x
}

// buildVMIdx works out t's indexes for spec: every segment, in rowid order,
// with an index over the column that holds INTEGERs (the postings codec), and
// whose rows within each value already run in spec's order when it has one.
func buildVMIdx(t *vmTab, spec *idxSeekSpec, enc TextEncoding, s *segSortedRows) *vmIdx {
	x := &vmIdx{tab: t, none: true}
	if t == nil || t.none || len(t.at) == 0 || len(t.at) != len(t.segs) {
		return x
	}
	if spec.sorted != nil {
		if len(spec.sortCols) == 1 && s != nil && s.ok {
			if y := sortedVMIdxFrom(&vmIdx{tab: t, none: true}, s); !y.none {
				return y
			}
		}
		return buildSortedVMIdx(x, spec, enc)
	}
	var prevRid uint64
	for k, sg := range t.at {
		if sg.nRows == 0 || k > 0 && sg.Rowid(0) <= prevRid {
			return x
		}
		prevRid = sg.Rowid(sg.nRows - 1)
		sg.eqMu.Lock()
		d, ok := sg.diskIndexFor(spec.col)
		sg.eqMu.Unlock()
		if !ok || d.codec != segIdxCodecPostings || d.nVals == 0 || d.nRows == 0 || len(d.posts) < 4*d.nRows {
			return x
		}
		rows := make([]int, d.nRows)
		for i := range rows {
			rows[i] = int(binary.LittleEndian.Uint32(d.posts[4*i:]))
		}
		if spec.order != nil && !pinnedToRowid(spec) && !runsInKeyOrder(sg, d, rows, spec.order) {
			return x
		}
		x.vidx = append(x.vidx, jit.VIdx{Vals: unsafe.Pointer(unsafe.SliceData(d.vals)), NVals: int64(d.nVals),
			Starts: unsafe.Pointer(unsafe.SliceData(d.starts)), Rows: unsafe.Pointer(unsafe.SliceData(rows)), Seg: t.segs[k].Seg,
			Mask: t.segs[k].Mask, Lo: d.valueAt(0), Hi: d.valueAt(d.nVals - 1)})
		x.segs, x.keep = append(x.segs, sg), append(x.keep, rows)
	}
	for k := range x.vidx {
		x.vidx[k].Count = int64(len(x.vidx) - k)
	}
	x.none = false
	return x
}

// buildSortedVMIdx is buildVMIdx for a sorted seek: each segment's rows in
// the order of the spec's columns, ties in rowid order, as segSortedSeek
// orders a run, every column a clean INTEGER block.
func buildSortedVMIdx(x *vmIdx, spec *idxSeekSpec, enc TextEncoding) *vmIdx {
	if y := buildSegSortedVMIdx(&vmIdx{tab: x.tab, none: true}, spec); !y.none && !overlapping(y.vidx) {
		return y
	}
	if y := buildTableSortedVMIdx(&vmIdx{tab: x.tab, none: true}, spec); !y.none {
		return y
	}
	return buildTableSortedVMIdxValues(x, spec, enc)
}

// overlapping reports whether two segments' indexes share more than a key at
// their boundary: VSeekIdx exits on a key whose rows two segments hold, which
// for keys spread over every segment is every key.
func overlapping(vidx []jit.VIdx) bool {
	for k := 1; k < len(vidx); k++ {
		if vidx[k].Lo < vidx[k-1].Hi {
			return true
		}
	}
	return false
}

// buildTableSortedVMIdx is buildSortedVMIdx as one index over every segment of
// the table, as segSortedRowsFor and orderRun order the rows: a NULL key is in
// no run, and a run's rows go by the spec's columns, a NULL before any
// INTEGER, ties in rowid order.
func buildTableSortedVMIdx(x *vmIdx, spec *idxSeekSpec) *vmIdx {
	t := x.tab
	nk := len(spec.sortCols)
	if nk > 4 || nk == 0 || len(t.at) > 1<<16 {
		return x
	}
	type row struct {
		k     [4]int64
		nulls uint8
		seg   uint16
		r     int32
	}
	n := 0
	for _, sg := range t.at {
		n += sg.nRows
	}
	rows := make([]row, 0, n)
	var prevRid uint64
	mask := ^uint64(0)
	for si, sg := range t.at {
		if si > 0 && sg.Rowid(0) <= prevRid {
			return x
		}
		prevRid = sg.Rowid(sg.nRows - 1)
		mask &= t.segs[si].Mask
		blocks := make([][]int64, nk)
		allNull := make([]bool, nk)
		for j, c := range spec.sortCols {
			if sg.hasException(c) {
				return x
			}
			b, ok := sg.Int64Column(c)
			switch {
			case ok && len(b) >= sg.nRows:
				blocks[j] = b
			case j > 0 && sg.cols[c].phys == PhysNull:
				allNull[j] = true
			default:
				return x
			}
		}
		key := spec.sortCols[0]
		nulls := sg.cols[key].nullOff != 0
		for i := range sg.nRows {
			if nulls && sg.isNull(key, i) {
				continue
			}
			rw := row{seg: uint16(si), r: int32(i)}
			for j, c := range spec.sortCols {
				if allNull[j] || j > 0 && sg.cols[c].nullOff != 0 && sg.isNull(c, i) {
					rw.nulls |= 1 << j
				} else {
					rw.k[j] = blocks[j][i]
				}
			}
			rows = append(rows, rw)
		}
	}
	if len(rows) == 0 {
		return x
	}
	// Ties in rowid order: the rows were taken segment by segment.
	slices.SortFunc(rows, func(a, b row) int {
		for j := range nk {
			na, nb := a.nulls>>j&1, b.nulls>>j&1
			if c := cmp.Compare(nb, na); c != 0 {
				return c
			}
			if c := cmp.Compare(a.k[j], b.k[j]); na == 0 && c != 0 {
				return c
			}
		}
		if c := cmp.Compare(a.seg, b.seg); c != 0 {
			return c
		}
		return cmp.Compare(a.r, b.r)
	})
	return x.tableIndex(mask, len(rows), func(i int) (int, int, int64) { return int(rows[i].seg), int(rows[i].r), rows[i].k[0] })
}

// buildTableSortedVMIdxValues is buildTableSortedVMIdx over any columns: a
// run's rows go by the segments' own values, compared as orderRun's
// autoIndexSortValues compares them.
func buildTableSortedVMIdxValues(x *vmIdx, spec *idxSeekSpec, enc TextEncoding) *vmIdx {
	t := x.tab
	if len(t.at) > 1<<16 || len(spec.sortColls) != len(spec.sortCols) {
		return x
	}
	type row struct {
		k   int64
		seg uint16
		r   int32
	}
	n := 0
	for _, sg := range t.at {
		n += sg.nRows
	}
	rows := make([]row, 0, n)
	var prevRid uint64
	mask := ^uint64(0)
	key := spec.sortCols[0]
	for si, sg := range t.at {
		if si > 0 && sg.Rowid(0) <= prevRid {
			return x
		}
		prevRid = sg.Rowid(sg.nRows - 1)
		mask &= t.segs[si].Mask
		b, ok := sg.Int64Column(key)
		if !ok || len(b) < sg.nRows || sg.hasException(key) {
			return x
		}
		nulls := sg.cols[key].nullOff != 0
		for i := range sg.nRows {
			if !nulls || !sg.isNull(key, i) {
				rows = append(rows, row{k: b[i], seg: uint16(si), r: int32(i)})
			}
		}
	}
	if len(rows) == 0 {
		return x
	}
	// A column whose every value is a number or a NULL is read once, as
	// compareValuesCollatedEnc orders it (compareNumeric, a NULL first);
	// any other is read from its segment on each comparison.
	nums := make([][]Value, len(spec.sortCols))
	for j := 1; j < len(spec.sortCols); j++ {
		c, vs := spec.sortCols[j], make([]Value, len(rows))
		for i, rw := range rows {
			v := t.at[rw.seg].Value(c, int(rw.r))
			if v.Typ != Null && v.Typ != Int && v.Typ != Float {
				vs = nil
				break
			}
			vs[i] = v
		}
		nums[j] = vs
	}
	perm := make([]int32, len(rows))
	for i := range perm {
		perm[i] = int32(i)
	}
	slices.SortFunc(perm, func(a, b int32) int {
		ra, rb := rows[a], rows[b]
		if c := cmp.Compare(ra.k, rb.k); c != 0 {
			return c
		}
		for j := 1; j < len(spec.sortCols); j++ {
			var va, vb Value
			if nums[j] != nil {
				va, vb = nums[j][a], nums[j][b]
			} else {
				c := spec.sortCols[j]
				va, vb = t.at[ra.seg].Value(c, int(ra.r)), t.at[rb.seg].Value(c, int(rb.r))
			}
			if c := compareValuesCollatedEnc(va, vb, spec.sortColls[j], enc); c != 0 {
				return c
			}
		}
		return cmp.Compare(a, b) // rowid order
	})
	return x.tableIndex(mask, len(rows), func(i int) (int, int, int64) {
		rw := rows[perm[i]]
		return int(rw.seg), int(rw.r), rw.k
	})
}

// sortedVMIdxFrom is buildSortedVMIdx from the VDBE's own segSortedRows for
// the seek, with nothing to sort: its runs are in rowid order, or, those the
// VDBE put in its automatic index's order, in an order that keeps the rows a
// loop keeps in rowid order (the key's other columns are ones the seek holds
// to one value). Its rows are copied: orderRun reorders them in place.
func sortedVMIdxFrom(x *vmIdx, s *segSortedRows) *vmIdx {
	t := x.tab
	at := make(map[*segment]int, len(t.at))
	mask := ^uint64(0)
	for k, sg := range t.at {
		at[sg] = k
		mask &= t.segs[k].Mask
	}
	n := len(s.keys)
	if s.one != nil {
		n = len(s.perm)
	}
	if n == 0 {
		return x
	}
	if s.one != nil {
		k, ok := at[s.one]
		if !ok {
			return x
		}
		return x.tableIndex(mask, n, func(i int) (int, int, int64) { return k, int(s.perm[i]), s.block[s.perm[i]] })
	}
	for _, sg := range s.segs {
		if _, ok := at[sg]; !ok {
			return x
		}
	}
	return x.tableIndex(mask, n, func(i int) (int, int, int64) { return at[s.segs[i]], s.rows[i], s.keys[i] })
}

// tableIndex makes x the one index over every segment whose n rows, in
// order, are at(i): segment, row and key.
func (x *vmIdx) tableIndex(mask uint64, n int, at func(i int) (seg, r int, k int64)) *vmIdx {
	t := x.tab
	keep := make([]int, n)
	x.rowSegs, x.segPtrs = make([]*segment, n), make([]unsafe.Pointer, n)
	var vals []int64
	var starts []int32
	for i := range n {
		seg, r, k := at(i)
		keep[i], x.rowSegs[i], x.segPtrs[i] = r, t.at[seg], t.segs[seg].Seg
		if i == 0 || k != vals[len(vals)-1] {
			vals, starts = append(vals, k), append(starts, int32(i))
		}
	}
	starts = append(starts, int32(n))
	x.vidx = []jit.VIdx{{Vals: unsafe.Pointer(unsafe.SliceData(vals)), NVals: int64(len(vals)), Starts: unsafe.Pointer(unsafe.SliceData(starts)),
		Rows: unsafe.Pointer(unsafe.SliceData(keep)), Seg: t.segs[0].Seg, Mask: mask, Count: 1, Lo: vals[0], Hi: vals[len(vals)-1]}}
	if len(t.at) > 1 {
		// Each row's segment; one segment's rows are its own (VRowid reads
		// them natively).
		x.vidx[0].Segs = unsafe.Pointer(unsafe.SliceData(x.segPtrs))
	}
	x.keep, x.vals, x.starts = [][]int{keep}, [][]int64{vals}, [][]int32{starts}
	x.none = false
	return x
}

// buildSegSortedVMIdx is buildSortedVMIdx as each segment's own index, over
// segments whose columns are clean INTEGER blocks (segment.sortedOn).
func buildSegSortedVMIdx(x *vmIdx, spec *idxSeekSpec) *vmIdx {
	t := x.tab
	var prevRid uint64
	for k, sg := range t.at {
		if k > 0 && sg.Rowid(0) <= prevRid {
			return x
		}
		prevRid = sg.Rowid(sg.nRows - 1)
		blocks, perm, ok := sg.sortedOn(spec.sortCols)
		if !ok || len(perm) == 0 {
			return x
		}
		b := blocks[0]
		rows := make([]int, len(perm))
		var vals []int64
		var starts []int32
		for i, r := range perm {
			rows[i] = int(r)
			if i == 0 || b[r] != b[perm[i-1]] {
				vals, starts = append(vals, b[r]), append(starts, int32(i))
			}
		}
		starts = append(starts, int32(len(perm)))
		x.vidx = append(x.vidx, jit.VIdx{Vals: unsafe.Pointer(unsafe.SliceData(vals)), NVals: int64(len(vals)),
			Starts: unsafe.Pointer(unsafe.SliceData(starts)), Rows: unsafe.Pointer(unsafe.SliceData(rows)), Seg: t.segs[k].Seg,
			Mask: t.segs[k].Mask, Lo: vals[0], Hi: vals[len(vals)-1]})
		x.segs, x.keep = append(x.segs, sg), append(x.keep, rows)
		x.vals, x.starts = append(x.vals, vals), append(x.starts, starts)
	}
	for k := range x.vidx {
		x.vidx[k].Count = int64(len(x.vidx) - k)
	}
	x.none = false
	return x
}

// loopPinnedCols is the columns of cursor c the loop opening at pc holds equal
// to another cursor's before anything else: each a comparison
//
//	Column c.x -> r1; Column o.y -> r2; Eq r1, r2 (BINARY); IfNot -> Next c
//
// whose failure skips the row, with o another cursor, positioned outside the
// loop since nothing has moved inside it yet.
func loopPinnedCols(insns []Instruction, pc, c int) []int {
	var cols []int
	for pc+3 < len(insns) {
		a, b, eq, skip := insns[pc], insns[pc+1], insns[pc+2], insns[pc+3]
		if a.Op != OpColumn || b.Op != OpColumn || eq.Op != OpEq || skip.Op != OpIfNot || skip.P1 != eq.P2 {
			break
		}
		if coll, _ := eq.P4.(string); coll != "" && !equalFoldName(effectiveCollation(coll), "BINARY") {
			break
		}
		if a.P1 != c {
			a, b = b, a
		}
		if a.P1 != c || b.P1 == c || !(eq.P1 == a.P3 && eq.P3 == b.P3 || eq.P1 == b.P3 && eq.P3 == a.P3) {
			break
		}
		if t := skip.P2; t < 0 || t >= len(insns) || insns[t].Op != OpNext || insns[t].P1 != c {
			break
		}
		cols = append(cols, a.P2)
		pc += 4
	}
	return cols
}

// pinnedToRowid reports whether spec's seek and loop hold equal every column
// of its order but the trailing rowid: the rows the loop keeps then run in
// rowid order, as the index lists each value's rows.
func pinnedToRowid(spec *idxSeekSpec) bool {
	cols := spec.order.cols
	n := len(cols)
	if n > 0 && cols[n-1] == -1 {
		n--
	}
	for _, col := range cols[:n] {
		if !slices.Contains(spec.pinned, col) {
			return false
		}
	}
	return true
}

// runsInKeyOrder reports whether each value's rows, in the index's order, are
// already in key's order. key leads with the indexed column, which a run
// shares, so only its columns after that -- clean INTEGER blocks -- and then
// the rowid order a run's rows; ties keep the index's order, as the seek's
// stable sort does (sortListBy).
func runsInKeyOrder(sg *segment, d *segDiskIndex, rows []int, key *autoIndexKey) bool {
	rest := key.cols[1:]
	blocks := make([][]int64, len(rest))
	for k, c := range rest {
		if c < 0 {
			if k != len(rest)-1 {
				return false
			}
			continue
		}
		b, ok := segCleanInt64Column(sg, c)
		if !ok || len(b) < sg.nRows {
			return false
		}
		blocks[k] = b
	}
	for i := 0; i < d.nVals; i++ {
		for p := int(d.start(i)) + 1; p < int(d.start(i+1)); p++ {
			a, b := rows[p-1], rows[p]
			for k, c := range rest {
				var u, v int64
				if c < 0 {
					u, v = int64(sg.Rowid(a)), int64(sg.Rowid(b))
				} else {
					u, v = blocks[k][a], blocks[k][b]
				}
				if u == v {
					continue
				}
				if desc := k+1 < len(key.desc) && key.desc[k+1]; (u < v) == desc {
					return false
				}
				break
			}
		}
	}
	return true
}

// sortedSeekServed reports whether the VDBE would serve cursor cur's sorted
// seeks from the segments (segSortedSeek), as native code serves them, and
// makes it ready to: only then does native code seek it.
//
// Only once the VDBE has made enough of the seeks itself (cur.segSorted) to
// pay for the index -- a sort of the table -- that native code then builds:
// an entry that built the table's sorted rows for a seek its run might never
// reach (an outer loop with no rows), or for a handful, paid for them on
// every query.
func (jx *vmJIT) sortedSeekServed(m *vdbe, cur *vdbeCursor, spec *idxSeekSpec) bool {
	s := cur.segSorted
	if !cur.segSortable() || s == nil || !s.ok {
		return false
	}
	if n := max(len(s.keys), len(s.perm)); s.seeks < max(16, n/64) {
		return false
	}
	_, orderOp, ok := cur.segSortedReady(spec.sorted, m.encoding())
	var key *autoIndexKey
	if orderOp != nil {
		key = orderOp.P4.(*autoIndexKey)
	}
	return ok && key == spec.order
}

// syncSortedSeek takes back a cursor native code seeked for an
// OpSortedSeek: native code's run is put on the cursor as segSortedSeek
// puts one, then the cursor rewound and walked to the row native code left
// it on, or past the last.
func (m *vdbe) syncSortedSeek(jx *vmJIT, c int, jc *jit.VCursor, spec *idxSeekSpec) error {
	cur := m.cursors[c]
	x := jx.idxs[c].Load()
	if cur == nil || x == nil || x.none {
		return fmt.Errorf("engine: internal: cursor %d seeked natively has no seek to take back", c)
	}
	_, orderOp, ok := cur.segSortedReady(spec.sorted, m.encoding())
	if !ok {
		return fmt.Errorf("engine: internal: cursor %d seeked natively is not one the segments serve", c)
	}
	src := &cur.pointSrc
	segs, rows := src.listSegs[:0], src.listRows[:0]
	switch {
	case jc.State == 3:
	case x.rowSegs != nil:
		at := int((uintptr(jc.Sel) - uintptr(unsafe.Pointer(unsafe.SliceData(x.keep[0])))) / 8)
		if jc.Sel == nil || jc.N <= 0 || at < 0 || at+int(jc.N) > len(x.rowSegs) {
			return fmt.Errorf("engine: internal: cursor %d seeked natively is on no run of its index", c)
		}
		segs, rows = append(segs, x.rowSegs[at:at+int(jc.N)]...), append(rows, x.keep[0][at:at+int(jc.N)]...)
	default:
		k := slices.IndexFunc(x.vidx, func(v jit.VIdx) bool { return v.Seg == jc.Seg })
		if k < 0 || jc.Sel == nil || jc.N <= 0 {
			return fmt.Errorf("engine: internal: cursor %d seeked natively is in no segment of its table", c)
		}
		for _, r := range unsafe.Slice((*int)(jc.Sel), jc.N) {
			segs, rows = append(segs, x.segs[k]), append(rows, r)
		}
	}
	cur.serveSegRun(orderOp, segs, rows)
	if jc.State == 2 {
		return nil // the rewind is still to come
	}
	cur.segFilter, cur.params = nil, m.params
	if err := cur.rewind(); err != nil {
		return err
	}
	if jc.State == 3 {
		return nil
	}
	for range jc.Last + 1 {
		if !cur.advance() {
			if cur.streamErrPending != nil {
				return cur.streamErrPending
			}
			return fmt.Errorf("engine: internal: cursor %d has fewer rows than native code seeked", c)
		}
	}
	if jc.Pos >= jc.N {
		for cur.advance() {
		}
	}
	return cur.streamErrPending
}

// syncIdxSeek brings a cursor native code seeked (jit.VSeekIdx) up to date:
// the VDBE runs the program's own seek of the last key -- its OpSeekIndexHint,
// its OpAutoIndexOrder and the rewind -- and walks on to the row native code
// left the cursor on, or past the last, so the cursor is as the VDBE would
// have left it however it serves the seek.
func (m *vdbe) syncIdxSeek(jx *vmJIT, c int, jc *jit.VCursor, spec *idxSeekSpec) error {
	insns := unsafe.Slice(jx.insns, jx.n)
	cur := m.cursors[c]
	x := jx.idxs[c].Load()
	if cur == nil || x == nil || x.none {
		return fmt.Errorf("engine: internal: cursor %d seeked natively has no seek to take back", c)
	}
	op := &insns[spec.seek]
	saved := m.regs[op.P2]
	m.regs[op.P2] = Value{Typ: Int, I: jc.Key}
	err := m.execSeekIndexHint(op)
	m.regs[op.P2] = saved
	if err != nil {
		return err
	}
	if spec.aio >= 0 {
		if err := m.execAutoIndexOrder(&insns[spec.aio]); err != nil {
			return err
		}
	}
	cur.segFilter, cur.params = nil, m.params
	if err := cur.rewind(); err != nil {
		return err
	}
	if jc.State == 3 {
		if cur.advance() {
			return fmt.Errorf("engine: internal: cursor %d has rows for key %d native code found none for", c, jc.Key)
		}
		return cur.streamErrPending
	}
	if jc.Last < 0 || jc.Last >= jc.N || jc.Sel == nil {
		return fmt.Errorf("engine: internal: cursor %d seeked natively is on no row", c)
	}
	k := slices.IndexFunc(x.vidx, func(v jit.VIdx) bool { return v.Seg == jc.Seg })
	if k < 0 {
		return fmt.Errorf("engine: internal: cursor %d seeked natively is in no segment of its table", c)
	}
	target := x.segs[k].Rowid(*(*int)(unsafe.Add(jc.Sel, 8*jc.Last)))
	past, found := jc.Pos >= jc.N, false
	for cur.advance() {
		if !found && cur.rowid == target {
			found = true
			if !past {
				return nil
			}
		}
	}
	if cur.streamErrPending != nil {
		return cur.streamErrPending
	}
	if !found {
		return fmt.Errorf("engine: internal: cursor %d has no row %d native code seeked to", c, target)
	}
	return nil
}
