package engine

import "sync"

// A pool of reusable VDBE machines, resized to fit each program (vdbe.c's
// sqlite3VdbeReset). Package-level to allow sharing across connections.
//
// WHAT MAY BE REUSED AND WHAT MAY NOT is the whole of the correctness argument,
// and it turns on one question: did the machine HAND THIS MEMORY OUT?
//
//   - regs is reused. Every one of its 27 read sites copies OUT of it
//     (copy(dst, m.regs[a:b]) or append([]Value(nil), ...)); nothing retains a
//     sub-slice. It is CLEARED on release, so a stale value cannot be read as
//     this execution's -- and clearing a []Value zeroes the Value structs, not
//     the byte arrays their .S fields pointed at, so text handed out earlier
//     stays valid.
//   - recRegs is reused on regs' terms. It holds only the HEADERS of records --
//     every reader stores or copies the record itself, never the slice of
//     them -- and the records are recChunk's, below, which is never reused. It
//     is cleared on release so the pool pins no record.
//   - recChunk is DROPPED, never reused. It is a bump allocator, and the
//     records it carves are stored in the row store BY REFERENCE (rowStore.put
//     keeps the caller's slice). Recycling it would overwrite committed rows
//     with the next statement's registers. Dropping it costs nothing: it was
//     allocated per execution already.
//   - Everything else is zeroed by reconstructing the struct from a composite
//     literal, so a field added later is zero by default rather than stale by
//     default. That is deliberate: the failure mode of forgetting one is a
//     wrong answer, and this makes forgetting the SAFE direction.
func (prog *Program) getMachine() *vdbe { return prog.getMachineFrom(nil) }

// getMachineFrom is getMachine taking *spare first when it holds a machine: a
// session's own slot, which a statement loop reuses without the pool's
// per-processor bookkeeping. The slot is emptied, so a nested run (a trigger
// body) goes to the pool.
func (prog *Program) getMachineFrom(spare **vdbe) *vdbe {
	var m *vdbe
	if spare != nil && *spare != nil {
		m, *spare = *spare, nil
	} else {
		m, _ = vdbeMachines.Get().(*vdbe)
	}
	if m == nil {
		m = &vdbe{}
	}
	m.regs = prog.regFile(m.regs, true)
	if cap(m.recRegs) < prog.NRecRegs {
		m.recRegs = make([][]Value, prog.NRecRegs)
	} else {
		m.recRegs = m.recRegs[:prog.NRecRegs]
	}
	if cap(m.cursors) < prog.NCursors {
		m.cursors = make([]*vdbeCursor, prog.NCursors)
	} else {
		// NOT cleared: the slots hold SCRUBBED CURSOR SHELLS that putMachine kept
		// on purpose (see there), and openCursorInto fills one rather than
		// allocating. A slot past the previous program's cursor count is nil, which
		// openCursorInto allocates for.
		m.cursors = m.cursors[:prog.NCursors]
	}
	return m
}

// openCursorInto is openCursor over the machine's own slot, reusing the shell
// pooled there when there is one. A vdbeCursor is 496 bytes and every statement
// with a FROM clause allocates at least one.
//
// The whole struct is REPLACED, never patched, so a field added to vdbeCursor
// later cannot arrive here still holding the last statement's value -- the same
// reason putMachine rebuilds the machine from a composite literal.
func (m *vdbe) openCursorInto(slot int, p *ReadOnlyPager, tbl *resolvedTable) *vdbeCursor {
	cur := m.cursorShell(slot)
	// A list seek's buffers outlive the cursor, for a subquery that opens it
	// once per outer row and seeks once (segListSeek).
	ps := &cur.pointSrc
	src := segRowSource{listSegs: ps.listSegs[:0], listRows: ps.listRows[:0], listKeys: ps.listKeys[:0], listPerm: ps.listPerm[:0], listHits: ps.listHits[:0]}
	*cur = vdbeCursor{pager: p, tbl: tbl, colMask: allColumns, pointSrc: src}
	return cur
}

// openRowStoreCursorInto is openRowStoreCursor over the machine's own slot. The
// write path opens one of these per statement -- 465 bytes on a single-row
// INSERT, second only to the change log.
func (m *vdbe) openRowStoreCursorInto(slot int, tbl *tableMeta) *vdbeCursor {
	cur := m.cursorShell(slot)
	// colMask: see openRowStoreCursor, whose invariant this states too.
	*cur = vdbeCursor{rowStore: tbl, pos: -1, colMask: allColumns}
	return cur
}

// cursorShell is the machine's slot, allocating one only the first time a
// program of this width runs on this machine.
func (m *vdbe) cursorShell(slot int) *vdbeCursor {
	cur := m.cursors[slot]
	if cur == nil {
		cur = &vdbeCursor{}
		m.cursors[slot] = cur
	}
	return cur
}

// putMachine returns m to prog's pool, scrubbed. See getMachine for why regs
// survives and recChunk does not.
//
// The scrub is also the TEST: anything that wrongly retained a register file
// reads zeroes afterwards, which the corpus and the harness see as a wrong
// answer rather than as an intermittent one.
func (prog *Program) putMachine(m *vdbe) { prog.putMachineTo(m, nil) }

// putMachineTo is putMachine into *spare when that slot is empty, else the pool.
// The machine is scrubbed the same way either way.
func (prog *Program) putMachineTo(m *vdbe, spare **vdbe) {
	regs, cursors, recRegs := m.regs, m.cursors, m.recRegs[:cap(m.recRegs)]
	// The column masks, a pure function of the instructions they are tagged
	// with (cursorMask), kept as they are.
	masks, masksOf, masksN, masksCurs := m.colMasks, m.colMasksOf, m.colMasksN, m.colMasksCurs
	// The last accumulator set, for the next execution to rebuild: nothing
	// reads it once this one has finished.
	aggSpare := m.aggAccs
	if aggSpare == nil {
		aggSpare = m.aggSpare
	}
	// Past the file's length too: native code keeps the parameters and outer
	// columns there (jitExtra), and a program of more registers taking this
	// machine would start with one of them in a register it has not written.
	clear(regs[:max(len(regs), min(cap(regs), prog.NReg+prog.jitExtra()))])
	clear(recRegs)
	// The cursor SHELLS are kept and their CONTENTS dropped. Keeping them saves
	// the 496-byte allocation every FROM clause makes; scrubbing them is what
	// stops a finished scan's decoded rows from being pinned by the pool, and
	// stops the next statement from reading the last one's row by accident.
	for _, c := range cursors {
		if c != nil {
			*c = vdbeCursor{pointSrc: keptListBuffers(&c.pointSrc)}
		}
	}
	// The sub-cache slice too (reuseSubCache), emptied to its full capacity
	// so no cached row is kept alive by the pool.
	sub := m.subCache[:cap(m.subCache)]
	clear(sub)
	// The JIT's cursor views and row copies too (vmJIT.enter), scrubbed: they
	// were three allocations of every point read a native program ran.
	jcur, jsnap, jlive := m.jcur, m.jsnap, m.jLive[:0]
	clear(jcur)
	for i := range jsnap {
		sn := &jsnap[i]
		clear(sn.row[:cap(sn.row)])
		clear(sn.slab[:cap(sn.slab)])
		clear(sn.rows[:cap(sn.rows)])
		*sn = jitSnap{row: sn.row[:0], slab: sn.slab[:0], rows: sn.rows[:0], rids: sn.rids[:0]}
	}
	// The aggregate item machine's shell too (aggItemVM), scrubbed like this one
	// with its register file and sub-cache kept: it was a 1 KB allocation of
	// every aggregate query.
	agg := m.aggVM
	if agg != nil {
		ar, as := agg.regs[:cap(agg.regs)], agg.subCache[:cap(agg.subCache)]
		clear(ar)
		clear(as)
		*agg = vdbe{regs: ar[:0], subCache: as[:0]}
	}
	// And the aggregate row context's shell (aggRowCtx), which every use
	// rebuilds whole.
	actx := m.aggCtx
	if actx != nil {
		*actx = evalCtx{}
	}
	// The all-NULL row a row-free aggregate step hands on (aggNullRow), cleared.
	nullRow := m.aggNullRow[:cap(m.aggNullRow)]
	clear(nullRow)
	// The kept virtual-table scans (keptVtabStream), their last row cleared.
	vtKept := m.vtKept
	for _, k := range vtKept {
		clear(k.s.row)
	}
	// Zeroed in place, then the kept slices put back: the same machine as the
	// composite literal this was, without building that 1 KB literal and
	// copying it over -- most of what returning a machine cost per statement.
	*m = vdbe{}
	m.regs, m.cursors, m.recRegs, m.subCache, m.jcur, m.jsnap, m.jLive = regs, cursors, recRegs, sub[:0], jcur, jsnap, jlive
	m.aggVM, m.aggCtx, m.aggNullRow = agg, actx, nullRow
	m.colMasks, m.colMasksOf, m.colMasksN, m.colMasksCurs = masks, masksOf, masksN, masksCurs
	m.aggSpare = aggSpare
	if spare != nil && *spare == nil {
		// A session's own slot outlives the pool's cycles, so it keeps no
		// scan.
		*spare = m
		return
	}
	m.vtKept = vtKept
	vdbeMachines.Put(m)
}

// maxKeptList is the most rows a cursor's list buffers may hold and still be
// pooled with its shell.
const maxKeptList = 1 << 16

// keptListBuffers is src's list buffers emptied, for the next statement's list
// (an AutoIndexOrder walk builds one per execution): listSegs cleared, so the
// pool pins no segment, and none kept past maxKeptList rows.
func keptListBuffers(src *segRowSource) segRowSource {
	if cap(src.listSegs) > maxKeptList || cap(src.listRows) > maxKeptList || cap(src.listKeys) > maxKeptList ||
		cap(src.listPerm) > maxKeptList || cap(src.listHits) > maxKeptList {
		return segRowSource{}
	}
	segs := src.listSegs[:cap(src.listSegs)]
	clear(segs)
	return segRowSource{listSegs: segs[:0], listRows: src.listRows[:0], listKeys: src.listKeys[:0], listPerm: src.listPerm[:0], listHits: src.listHits[:0]}
}

// vdbeMachines holds every released machine, whatever program it ran.
var vdbeMachines sync.Pool
