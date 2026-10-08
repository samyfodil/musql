// This file runs derived row sources as co-routines: the source program is
// resumed once per row its consumer asks for, instead of being materialized first.
// Used for INSERT ... SELECT sources and outermost FROM-clause subqueries.
// A co-routine yields the same rows in the same order as materialization would.
package engine

import "fmt"

// streamMode is derivedSource.stream.
type streamMode uint8

const (
	// streamNever materializes the source.
	streamNever streamMode = iota
	// streamAlways runs the source as a co-routine whenever it opens: an
	// INSERT ... SELECT source that insert.c:1167 would not route through a
	// temp table (compileInsertSelectWrite).
	streamAlways
	// streamIfYielding runs the source as a co-routine only while the program
	// opening it is itself a co-routine, and only if that program does not
	// gather every row before producing one (programAccumulates): the outermost
	// derived source of a FROM clause (emitJoinLoops). A top-level read never
	// yields, so it never streams.
	streamIfYielding
)

// streamSink is the INSERT a chain of co-routines ultimately feeds. It is
// created by the streamAlways source that starts the chain and shared, by
// pointer, with every co-routine beneath it, so a recursive CTE deep in the
// chain can ask what its rows will end up in.
type streamSink struct {
	plan *insertPlan

	// bounded is true while that INSERT is an armed bulk append
	// (insert_stream_bounded.go): its rows leave memory as they arrive, so
	// what the chain produces is bounded by time and disk rather than by
	// resident memory. See recQueueFill for the caps it selects.
	bounded bool
}

// subStream is one co-routine: a machine running prog with yield set, plus the
// rows its last resume returned and not yet handed out.
type subStream struct {
	m       *vdbe
	prog    *Program
	buf     [][]Value
	done    bool
	started bool
	keep    []bool // derivedSource.keepSubtype
}

// streamsDerived reports whether OpOpenDerived should open ds as a co-routine
// rather than materialize it. insns is the opening machine's own program.
//
// Every source whose rows depend on something a co-routine would not see
// stays materialized: a correlated body (run per outer row through
// execWithParent), a compound (whose arms are the frames and which has no
// instructions to yield from), a LIVE body (re-lowered by runSubOnce against a
// fresh image), and the UPDATE ... FROM destinations, which post-process the
// whole row set.
func (m *vdbe) streamsDerived(ds *derivedSource, insns []Instruction) bool {
	if ds.stream == streamNever || ds.prog == nil || ds.prog.Correlated || ds.prog.Compound != nil ||
		ds.prog.LiveSource != nil || ds.upfrom != nil || ds.viewUpfrom != nil {
		return false
	}
	if ds.stream == streamAlways {
		return true
	}
	if !m.yield {
		return false
	}
	if !m.accumChecked {
		m.accumulates = programAccumulates(insns)
		m.accumChecked = true
	}
	return !m.accumulates
}

// newSubStream builds the co-routine for ds on pager: the machine runSubOnce
// would have run prog on (execTrig, with this machine as the source of the
// pseudo-rows), marked to yield. A streamAlways source starts a new sink; any
// other shares this machine's.
func (m *vdbe) newSubStream(ds *derivedSource, pager *ReadOnlyPager) *subStream {
	child := ds.prog.newMachine(pager, nil, m.params, m)
	child.yield, child.yieldReuse = true, true
	child.sink = m.sink
	if ds.stream == streamAlways {
		child.sink = &streamSink{plan: ds.sinkPlan}
		// The rows this source produces leave memory as they arrive -- see
		// insert_stream_bounded.go -- so the recursion feeding it may run past
		// the in-memory caps.
		if m.wctx != nil && ds.sinkPlan != nil && ds.sinkPlan.appendArmable {
			child.sink.bounded = true
		}
	}
	return &subStream{m: child, prog: ds.prog, keep: ds.keepSubtype}
}

// next returns the co-routine's next row, resuming it when nothing is
// buffered. ok is false once the program has ended; an error ends it too.
func (s *subStream) next() (row []Value, ok bool, err error) {
	for len(s.buf) == 0 {
		if s.done {
			return nil, false, nil
		}
		rows, rerr := s.resume()
		if rerr != nil {
			s.done, s.buf = true, nil
			return nil, false, rerr
		}
		s.buf = rows
		if s.m.done {
			s.done = true
		}
	}
	row = s.buf[0]
	s.buf[0] = nil
	s.buf = s.buf[1:]
	return row, true, nil
}

// resume runs the co-routine up to its next yield, with the WITH-clause scope
// its program was compiled under installed for exactly that span -- the
// bracket execOuterTrig puts around a whole run. Brackets of nested
// co-routines nest last-in first-out, since a resume only ever happens inside
// its consumer's own.
func (s *subStream) resume() ([][]Value, error) {
	pager := s.m.pager
	if s.prog.CTEScopeSnapshot != nil && pager != nil {
		saved := pager.cteScopes
		pager.cteScopes = s.prog.CTEScopeSnapshot
		defer func() { pager.cteScopes = saved }()
	}
	return s.m.run(s.prog.Insns)
}

// openStreamCursor opens a derived-table cursor whose rows come from a
// co-routine. Like openDerivedCursor's, its rows expose no rowid and carry no
// JSON subtype; unlike them, they exist only one at a time.
func openStreamCursor(tbl *resolvedTable, s *subStream) *vdbeCursor {
	return &vdbeCursor{tbl: tbl, sub: s, materialized: true, colMask: allColumns, pos: -1}
}

// rewindStream is rewind() for a co-routine cursor. A co-routine can be read
// once: emitJoinLoops streams only a source whose loop is entered once per
// program run, so a second rewind means that promise was broken, and it is
// reported rather than answered from a stream that has already moved on.
func (cur *vdbeCursor) rewindStream() error {
	if cur.sub.started {
		return fmt.Errorf("engine: internal: a derived table read as a co-routine was rewound a second time")
	}
	cur.sub.started = true
	cur.pos = -1
	cur.rowidNull = false
	cur.streamErrPending = nil
	return nil
}

// advanceStream is advance() for a co-routine cursor: the next row, or false at
// the end -- with a resume's error parked in streamErrPending for
// OpRewind/OpNext to raise.
func (cur *vdbeCursor) advanceStream() bool {
	row, ok, err := cur.sub.next()
	if err != nil {
		cur.streamErrPending = err
		return false
	}
	if !ok {
		return false
	}
	// A column read out of a derived row carries no subtype, unless flattening
	// keeps it -- see openDerivedCursor.
	clearDerivedSubtypes(row, cur.sub.keep)
	cur.pos++
	cur.rowid = 0
	cur.rowVals = row
	cur.rowRaw = nil
	return true
}

// programAccumulates reports whether insns hold an opcode that gathers every
// row of the scan before any result row exists -- a sorter, a DISTINCT set, an
// aggregate, a GROUP BY + DISTINCT batch or a window batch. Such a program
// would pull its whole source before yielding once, so streaming that source
// buys nothing and only moves when its errors surface.
func programAccumulates(insns []Instruction) bool {
	for i := range insns {
		switch insns[i].Op {
		case OpSorterInsert, OpDistinct, OpAggStep, OpHashAggStep, OpGroupBatchAppend, OpWindowAppend:
			return true
		}
	}
	return false
}

// programReadsTable is readsTable (insert.c:232-264) for this engine's
// programs: whether prog, or anything it runs, could read the table named
// target. A virtual table counts only when it is the target, as there, and a
// live sub-program by the lowering it carries. It answers TRUE for anything
// else it cannot see into -- a catalog, a CTE body compiled at run time, or a
// P4 payload it does not know -- because a false "reads" only materializes the source, which is
// exactly what happened before co-routines, while a false "does not read"
// would let the insertion loop's own writes race the source.
//
// The name alone decides, in any database: a same-named table in another
// catalog is a false positive, never a false negative.
func programReadsTable(prog *Program, target string) bool {
	return programReadsTableSeen(prog, target, map[*Program]bool{})
}

func programReadsTableSeen(prog *Program, target string, seen map[*Program]bool) bool {
	if prog == nil || seen[prog] {
		return false
	}
	seen[prog] = true
	// A LIVE sub-program is walked like any other: it is re-lowered at run time
	// from the same AST over the same schema, so the lowering it carries from
	// compile time opens exactly what the run-time one will
	// (compileLiveSubProgram, vdbe_live_read.go).
	if prog.Compound != nil {
		for _, arm := range prog.Compound.arms {
			if arm == nil || programReadsTableSeen(arm, target, seen) {
				return true
			}
		}
	}
	for i := range prog.Insns {
		in := &prog.Insns[i]
		switch p4 := in.P4.(type) {
		case nil, string, int64, float64, int, []byte, []int, []string, affinity,
			*recQueueSpec, *sorterKeyInfo, *indexSeekHint, *autoIndexKey:
		case *resolvedTable:
			if in.Op == OpOpenRead && (p4 == nil || equalFoldName(p4.name, target)) {
				return true
			}
		case *Program:
			if programReadsTableSeen(p4, target, seen) {
				return true
			}
		case *inSubPlan:
			if p4 == nil || programReadsTableSeen(p4.prog, target, seen) {
				return true
			}
		case *rowSubPlan:
			if p4 == nil || programReadsTableSeen(p4.prog, target, seen) {
				return true
			}
		case *aggPlan:
			if aggPlanReadsTable(p4, target, seen) {
				return true
			}
		case *aggResultInfo:
			if p4 == nil || aggPlanReadsTable(p4.plan, target, seen) {
				return true
			}
		case *derivedSource:
			switch {
			case p4 == nil:
				return true
			case p4.recSelf != nil:
			case p4.vtab != nil:
				// A virtual table reads the target only when it IS the target:
				// readsTable counts an OP_VOpen of the target's own VTable and
				// no other (insert.c:255-260).
				if equalFoldName(p4.vtab.Table, target) {
					return true
				}
			case p4.prog != nil && p4.upfrom == nil && p4.viewUpfrom == nil:
				if programReadsTableSeen(p4.prog, target, seen) {
					return true
				}
			default:
				return true
			}
		default:
			return true
		}
	}
	return false
}

// aggPlanReadsTable is programReadsTable over the programs an aggregate's
// output, HAVING and ORDER BY items compile to.
func aggPlanReadsTable(plan *aggPlan, target string, seen map[*Program]bool) bool {
	if plan == nil {
		return true
	}
	items := append([]*itemPlan{plan.havingPlan}, plan.outPlans...)
	for _, o := range plan.orderPlans {
		items = append(items, o.item)
	}
	for _, it := range items {
		if it != nil && it.prog != nil && programReadsTableSeen(it.prog.prog, target, seen) {
			return true
		}
	}
	return false
}
