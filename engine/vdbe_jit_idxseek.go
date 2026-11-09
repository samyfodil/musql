package engine

import (
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
	type site struct{ pc, end, c, key, t int }
	var sites []site
	specs = make([]*idxSeekSpec, len(segCap))
	bad := make([]bool, len(segCap))
	insns := prog.Insns
	for _, in := range insns {
		switch in.Op {
		case OpRightJoinMark, OpRightJoinSweepRewind, OpRightJoinSweepNext, OpRightJoinSweepOrder, OpSortedSeek:
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
		if prev := specs[c]; prev != nil && (prev.col != spec.col || prev.order != spec.order || prev.listed != spec.listed || !slices.Equal(prev.pinned, spec.pinned)) {
			bad[c] = true
			continue
		}
		if specs[c] == nil {
			specs[c] = spec
		}
		sites = append(sites, site{pc, j, c, in.P2, insns[j].P2})
	}
	any := false
	for _, s := range sites {
		if bad[s.c] {
			continue
		}
		any = true
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
		if spec == nil {
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
	vidx []jit.VIdx
	none bool
}

// idxFor returns cursor c's table as VSeekIdx searches it, or nil.
func (jx *vmJIT) idxFor(c int, t *vmTab) *vmIdx {
	x := jx.idxs[c].Load()
	if x == nil || x.tab != t {
		x = buildVMIdx(t, jx.idxSpecs[c])
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
func buildVMIdx(t *vmTab, spec *idxSeekSpec) *vmIdx {
	x := &vmIdx{tab: t, none: true}
	if t == nil || t.none || len(t.at) == 0 || len(t.at) != len(t.segs) {
		return x
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
