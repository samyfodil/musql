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
func (prog *Program) getMachine() *vdbe {
	m, _ := vdbeMachines.Get().(*vdbe)
	if m == nil {
		m = &vdbe{}
	}
	if cap(m.regs) < prog.NReg {
		m.regs = make([]Value, prog.NReg)
	} else {
		m.regs = m.regs[:prog.NReg]
	}
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
	*cur = vdbeCursor{pager: p, tbl: tbl, colMask: allColumns}
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
func (prog *Program) putMachine(m *vdbe) {
	regs, cursors, recRegs := m.regs, m.cursors, m.recRegs[:cap(m.recRegs)]
	clear(regs)
	clear(recRegs)
	// The cursor SHELLS are kept and their CONTENTS dropped. Keeping them saves
	// the 496-byte allocation every FROM clause makes; scrubbing them is what
	// stops a finished scan's decoded rows from being pinned by the pool, and
	// stops the next statement from reading the last one's row by accident.
	for _, c := range cursors {
		if c != nil {
			*c = vdbeCursor{}
		}
	}
	*m = vdbe{regs: regs, cursors: cursors, recRegs: recRegs}
	vdbeMachines.Put(m)
}

// vdbeMachines holds every released machine, whatever program it ran.
var vdbeMachines sync.Pool
