package engine

import (
	"math"
	"slices"
	"sort"
	"strings"
)

// RowCounter accumulates the rows statements read and write, counted the way
// libSQL counts LIBSQL_STMTSTATUS_ROWS_READ and LIBSQL_STMTSTATUS_ROWS_WRITTEN
// (its vdbe.c), which is what Turso bills by.
//
// Read is the rows a statement's cursors step onto in its tables and virtual
// tables: one per row a loop lands on, on any cursor, joins' inner sides and
// subqueries included. A loop a columnar kernel answers instead counts the rows
// that loop would have stepped onto. Rows a statement holds for itself -- a
// sorter's, a subquery's or a CTE's materialized result -- are not read again:
// they were counted when they were read from their table.
//
// Written is the rows a statement inserts, updates and deletes, plus the index
// entries an insert or an update writes, as C's OP_Insert, OP_IdxInsert,
// OP_Delete and OP_Clear count them there.
//
// The counts only grow. A connection keeps one counter and hands it to every
// pager and session its statements run on (SetRowCounter), so a statement's own
// counts are the difference across it.
type RowCounter struct {
	Read, Written int64
}

// SetRowCounter makes the statements run on p count their rows into rc; nil
// counts nothing.
func (p *ReadOnlyPager) SetRowCounter(rc *RowCounter) { p.rows = rc }

// SetRowCounter makes the statements run on db, and on every pager it hands
// out, count their rows into rc; nil counts nothing.
func (db *DB) SetRowCounter(rc *RowCounter) { db.rows = rc }

// foldRows adds the rows the machine counted to its statement's counter: the
// pager's for a read, the session's for a write.
func (m *vdbe) foldRows() {
	var rc *RowCounter
	if m.pager != nil {
		rc = m.pager.rows
	}
	if rc == nil && m.wctx != nil && m.wctx.db != nil {
		rc = m.wctx.db.rows
	}
	if rc != nil {
		rc.Read += m.nRead
		rc.Written += m.nWritten
	}
	m.nRead, m.nWritten = 0, 0
}

// billedCursor reports whether the rows cursor c of insns steps onto count as
// read.
func (m *vdbe) billedCursor(insns []Instruction, c int) bool {
	if len(insns) == 0 {
		return false
	}
	if m.billedOf != &insns[0] {
		m.billed, m.billedOf = billedCursors(insns, m.billed), &insns[0]
		m.bareCount = bareCountCursor(insns)
	}
	return c >= 0 && c < len(m.billed) && m.billed[c]
}

// bareCountCursor is the cursor of `SELECT count(*) FROM t`'s loop, which C
// answers with OP_Count rather than a walk (select.c's isSimpleCount), or -1.
func bareCountCursor(insns []Instruction) int {
	if len(insns) < 6 || insns[0].Op != OpInit || insns[1].Op != OpAggReset || insns[2].Op != OpOpenRead {
		return -1
	}
	c := insns[2].P1
	at := 3
	for at < len(insns) && insns[at].Op == OpAutoIndexOrder {
		at++
	}
	if at+2 >= len(insns) || insns[at].Op != OpRewind || insns[at].P1 != c {
		return -1
	}
	step, next := &insns[at+1], &insns[at+2]
	plan, ok := step.P4.(*aggPlan)
	if step.Op != OpAggStep || step.P3 != 0 || !ok || !segPlanIsPlainCountStar(plan) ||
		next.Op != OpNext || next.P1 != c || next.P2 != at+1 {
		return -1
	}
	return c
}

// billedCursors marks the cursors insns opens on a table (OpOpenRead,
// OpOpenWrite), a virtual table or the schema catalog (OpOpenDerived), into
// buf's storage.
func billedCursors(insns []Instruction, buf []bool) []bool {
	buf = buf[:0]
	for i := range insns {
		in := &insns[i]
		billed := false
		switch in.Op {
		case OpOpenRead, OpOpenWrite:
			billed = true
		case OpOpenDerived:
			ds, ok := in.P4.(*derivedSource)
			billed = ok && ds != nil && (ds.vtab != nil || ds.catalogScope != scopeAny)
		case OpHashAggStep:
			// Its source's rows are counted as the groups go out
			// (hashAggState.streamReads).
			if plan, ok := in.P4.(*aggPlan); ok && plan.cStreams {
				for _, c := range plan.cursors {
					for len(buf) <= c {
						buf = append(buf, false)
					}
					buf[c] = false
				}
			}
			continue
		default:
			continue
		}
		c := in.P1
		if c < 0 {
			continue
		}
		for len(buf) <= c {
			buf = append(buf, false)
		}
		buf[c] = billed
	}
	return buf
}

// indexWrites is the index entries C writes (OP_IdxInsert) for each row a
// statement stores in tbl: every index for an insert, and for an update the
// indexes keyed on a column it assigns (set, tbl's column numbers). A partial
// index, whose entry depends on the row, and a vector index are left out, as is
// a WITHOUT ROWID table's own key, which is the row itself: the count never
// exceeds C's.
func (db *DB) indexWrites(tbl *tableMeta, update bool, set []int) int {
	n := 0
	for _, ix := range db.indexes {
		if ix.isTablePK || ix.vec != nil || ix.where != nil || !indexBelongsTo(ix, tbl) {
			continue
		}
		if update && !indexKeyAssigned(ix, set) {
			continue
		}
		n++
	}
	return n
}

func indexKeyAssigned(ix *indexMeta, set []int) bool {
	for _, c := range ix.colIdx {
		for _, s := range set {
			if c >= 0 && c == s {
				return true
			}
		}
	}
	return false
}

// guardReads is what a guard kernel that answered the loop over cur instead
// counts as read: the rows that loop would have stepped onto. Without a seek
// that is every row of the table. A seek the loop planned on a declared index
// or the rowid would have read only its own rows, which the kernel never
// looks up: what the seek's state says once its hints ran, else matched, the
// rows the kernel found satisfying the whole WHERE (which the seek's
// include), when it knows them (not negative), else nothing. prologueSeek is
// a seek planned in hints the kernel skipped (prologueSeeks).
func guardReads(cur *vdbeCursor, prologueSeek bool, matched int64) int64 {
	n := seekGuardReads(cur, prologueSeek, matched)
	if cur.billLimit > 0 {
		n = min(n, cur.billLimit-1)
	}
	return n
}

func seekGuardReads(cur *vdbeCursor, prologueSeek bool, matched int64) int64 {
	if !prologueSeek {
		if n, ok := cur.plannedSeekRows(); ok {
			if matched >= 0 {
				return min(n, matched)
			}
			return n
		}
		return cur.tableRows()
	}
	return max(matched, 0)
}

// plannedSeekRows is the rows cur's configured seek reads, when it has one a
// C plan would take too: a rowid, a rowid IN list or a declared index.
func (cur *vdbeCursor) plannedSeekRows() (int64, bool) {
	switch {
	case cur.seekConfigured:
		return 1, true
	case cur.seekSetConfigured:
		return int64(len(cur.seekSet)), true
	case cur.idxSeekConfigured && cur.idxSeekDeclared:
		n, _ := cur.seekEstimate()
		return int64(n), true
	case cur.ranged:
		return 0, true
	case cur.billRanged:
		return cur.billRangeRows(), true
	}
	return 0, false
}

// tableRows is the rows of cur's table, the segments' recorded counts with
// the delta merged.
func (cur *vdbeCursor) tableRows() int64 {
	if cur.tbl == nil || cur.pager == nil {
		return 0
	}
	n, _ := cur.pager.segFilterCountTable(cur.tbl.root, nil, cur.tbl.cols, cur.tbl.ipkIndex)
	return int64(n)
}

// countReads is what C's OP_Count reads for a table, a page per few dozen
// rows, in musql's terms: one read per segment whose recorded row count it
// adds, and one for the rows the delta holds.
func (cur *vdbeCursor) countReads() int64 {
	if cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil {
		return 0
	}
	n := int64(len(cur.pager.segs.byRoot[cur.tbl.root]))
	if !cur.pager.segs.segDeltaCleanFor(cur.tbl.root) {
		n++
	}
	return n
}

// prologueSeeks reports whether the instructions from pc to the loop's
// OpRewind plan a seek C would take too: on the rowid or a declared index.
func prologueSeeks(insns []Instruction, pc int) bool {
	for ; pc < len(insns) && insns[pc].Op != OpRewind; pc++ {
		switch in := &insns[pc]; in.Op {
		case OpSeekRowidHint, OpSeekRowidRangeHint, OpSeekRowidSetHint:
			return true
		case OpSeekIndexHint:
			if h, ok := in.P4.(*indexSeekHint); ok && h.index != "" {
				return true
			}
		}
	}
	return false
}

// segFilterReads is guardReads for OpSegFilterCount at pc, whose answer is
// res: the bare count(*), which C answers with OP_Count, reads what that does.
func (m *vdbe) segFilterReads(cur *vdbeCursor, plan *segFilterPlan, insns []Instruction, pc int, res Value) int64 {
	return min(segFilterWalkReads(cur, plan, insns, pc, res), m.prologueReadBound(cur, insns, pc+1))
}

func segFilterWalkReads(cur *vdbeCursor, plan *segFilterPlan, insns []Instruction, pc int, res Value) int64 {
	seek := prologueSeeks(insns, pc+1)
	if !seek && !plan.isSum && len(plan.preds) == 0 && len(plan.semis) == 0 && len(plan.ins) == 0 && len(plan.likes) == 0 {
		return cur.countReads()
	}
	matched := int64(-1)
	if !plan.isSum && res.Typ == Int {
		matched = res.I
	}
	if !seek && matched >= 0 && prologueAutoSeek(insns, pc+1) {
		// The loop would seek the column's own equality index only where
		// that reads a sliver of the table (seekIsWide), else walk it all.
		if total := cur.tableRows(); matched > total/seekNarrowRatio {
			return total
		}
		return matched
	}
	return guardReads(cur, seek, matched)
}

// prologueAutoSeek reports whether the instructions from pc to the loop's
// OpRewind plan an automatic index seek (automaticSeekCandidates).
func prologueAutoSeek(insns []Instruction, pc int) bool {
	for ; pc < len(insns) && insns[pc].Op != OpRewind; pc++ {
		if h, ok := insns[pc].P4.(*indexSeekHint); ok && insns[pc].Op == OpSeekIndexHint && h.index == "" {
			return true
		}
	}
	return false
}

// segJoinReads is what a join kernel answering jp's loops counts as read: the
// outermost loop's rows, which it walks in full. The inner loops' rows, which
// it probes, and a seeking outer loop's (whose seek it does not take) are not
// counted, which keeps the count at or below C's for the same loops.
func (m *vdbe) segJoinReads(jp *segJoinPlan) int64 {
	if len(jp.cursors) == 0 {
		return 0
	}
	c := jp.cursors[0]
	if c < 0 || c >= len(m.cursors) || m.cursors[c] == nil {
		return 0
	}
	cur := m.cursors[c]
	if _, seeks := cur.plannedSeekRows(); seeks {
		return 0
	}
	return cur.tableRows()
}

// rowidBillBounds is where's conjuncts bounding the rowid (isRowid) by a
// non-negative integer literal or a bound parameter: "rowid < 5", "rowid
// BETWEEN 10 AND 30", either way round. C's loop takes them as its rowid range
// (where.c's WHERE_IPK with WHERE_COLUMN_RANGE) and steps onto the rows inside
// alone, so a loop here that walks more counts no more than those
// (emitRowidBillHint).
func rowidBillBounds(where Expr, isRowid func(Expr) bool) (keys []Expr, kinds []int) {
	if where == nil {
		return nil, nil
	}
	ops := map[string]int{">": rowidRangeGT, ">=": rowidRangeGE, "<": rowidRangeLT, "<=": rowidRangeLE}
	flip := []int{rowidRangeLT, rowidRangeLE, rowidRangeGT, rowidRangeGE}
	key := func(e Expr) bool {
		if _, ok := e.(ParamExpr); ok {
			return true
		}
		lit, ok := e.(LiteralExpr)
		return ok && !lit.materialized && lit.Val.Typ == Int && lit.Val.I >= 0
	}
	for _, cj := range seekConjuncts(where) {
		switch e := cj.(type) {
		case BinaryExpr:
			kind, ok := ops[e.Op]
			switch {
			case !ok:
			case isRowid(e.L) && key(e.R):
				keys, kinds = append(keys, e.R), append(kinds, kind)
			case isRowid(e.R) && key(e.L):
				keys, kinds = append(keys, e.L), append(kinds, flip[kind])
			}
		case BetweenExpr:
			if !e.Not && isRowid(e.X) && key(e.Lo) && key(e.Hi) {
				keys, kinds = append(keys, e.Lo, e.Hi), append(kinds, rowidRangeGE, rowidRangeLE)
			}
		}
	}
	return keys, kinds
}

// emitRowidBillHint emits, for cursor, the OpRowidReadBound that bounds the
// rows its loop counts as read by keys (rowidBillBounds), one per kind, and by
// limit, when positive one more than the rows a pass counts (planBillLimit).
// Each key goes straight into its register, so a kernel's prologue reads the
// bound as constants and the hint.
func (c *compiler) emitRowidBillHint(cursor int, keys []Expr, kinds []int, limit int) {
	if len(keys) == 0 && limit <= 0 {
		return
	}
	base := c.allocN(len(keys))
	for i, k := range keys {
		switch k := k.(type) {
		case ParamExpr:
			c.emit(Instruction{Op: OpVariable, P1: k.Index, P2: base + i})
		case LiteralExpr:
			if k.Val.I <= math.MaxInt32 {
				c.emit(Instruction{Op: OpInteger, P1: int(k.Val.I), P2: base + i})
			} else {
				c.emit(Instruction{Op: OpInt64, P2: base + i, P4: k.Val.I})
			}
		}
	}
	c.emit(Instruction{Op: OpRowidReadBound, P1: cursor, P2: base, P3: limit, P4: kinds})
}

// billRangeBy narrows the rowid range cur's loop counts its reads in by one
// bound per kind, read as narrowRowidRangeBy reads a seek's.
func (cur *vdbeCursor) billRangeBy(kinds []int, keys []Value) {
	if len(kinds) == 0 {
		return
	}
	ranged, lo, hi := cur.ranged, cur.rowidLo, cur.rowidHi
	cur.ranged, cur.rowidLo, cur.rowidHi = cur.billRanged, cur.billLo, cur.billHi
	for i, k := range kinds {
		cur.narrowRowidRangeBy(k, keys[i])
	}
	cur.billRanged, cur.billLo, cur.billHi = cur.ranged, cur.rowidLo, cur.rowidHi
	cur.ranged, cur.rowidLo, cur.rowidHi = ranged, lo, hi
}

// billRangeRows is at most the rows of cur's table inside its counting range
// (billRangeBy): those of the row store a write session reads, else the
// segments' rows there less every row the delta removes or replaces.
func (cur *vdbeCursor) billRangeRows() int64 {
	var rows *rowStore
	switch {
	case cur.rowStore != nil:
		rows = cur.rowStore.rows
	case cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil:
		return 0
	case cur.pager.segs.isLive(cur.tbl.root):
		live, _, err := cur.pager.segs.liveRows(cur.tbl.root)
		if err != nil {
			return 0
		}
		rows = live
	}
	if rows != nil {
		ids := rows.sortedRowids()
		lo := sort.Search(len(ids), func(i int) bool { return int64(ids[i]) >= cur.billLo })
		hi := sort.Search(len(ids), func(i int) bool { return int64(ids[i]) > cur.billHi })
		return int64(max(hi-lo, 0))
	}
	segs := cur.pager.segs.byRoot[cur.tbl.root]
	n := 0
	for _, s := range segs {
		from, to := s.rowidSpan(cur.billLo, cur.billHi)
		n += to - from
	}
	if !cur.pager.segs.segDeltaCleanFor(cur.tbl.root) {
		sub, _, ok := cur.pager.segs.segDeltaCorrection(cur.tbl.root, segs, nil)
		if !ok {
			return 0
		}
		n -= sub
	}
	return int64(max(n, 0))
}

// emitWriteBillHint is emitRowidBillHint for an UPDATE or DELETE scanning tbl
// on cursor.
func (c *compiler) emitWriteBillHint(tbl *tableMeta, cursor int, where Expr) {
	if tbl == nil || tbl.withoutRowid {
		return
	}
	keys, kinds := rowidBillBounds(where, func(e Expr) bool { return writeRowidRef(c, tbl, cursor, e) })
	c.emitRowidBillHint(cursor, keys, kinds, 0)
}

// planBillBounds gives each source of srcs whose loop seeks no rowid the
// bounds where puts on its rowid (rowidBillBounds), which its loop's reads
// are counted within, unless they are its rowid range's own. A WITHOUT ROWID
// table whose whole key where pins counts one row a pass, the one C's seek on
// that key lands on.
func planBillBounds(c *compiler, srcs []joinSource, where Expr) {
	for i, s := range srcs {
		if s.seekKeyExpr != nil || s.seekSet != nil || s.derived != nil || s.tbl == nil || s.left || s.rightOuter {
			continue
		}
		if s.tbl.withoutRowid {
			if keyPinned(c, s, where) {
				srcs[i].billLimit = 2
			}
			continue
		}
		keys, kinds := rowidBillBounds(where, func(e Expr) bool { return isScopeRowidRef(c, s, e) })
		if len(keys) > 1 || len(keys) == 1 && s.rowidRange == nil {
			srcs[i].billKeys, srcs[i].billKinds = keys, kinds
		}
	}
}

// keyPinned reports whether where's conjuncts set every PRIMARY KEY column of
// s, a WITHOUT ROWID table, equal to a literal or a bound parameter.
func keyPinned(c *compiler, s joinSource, where Expr) bool {
	if where == nil || len(s.tbl.pkColIdx) == 0 {
		return false
	}
	pinned := make([]bool, len(s.tbl.cols))
	for _, cj := range seekConjuncts(where) {
		be, ok := cj.(BinaryExpr)
		if !ok || be.Op != "=" && be.Op != "==" {
			continue
		}
		for _, side := range [2][2]Expr{{be.L, be.R}, {be.R, be.L}} {
			if col, ok := scopeColumn(c, s, side[0]); ok && isSeekKeyCandidate(side[1]) {
				pinned[col] = true
			}
		}
	}
	for _, col := range s.tbl.pkColIdx {
		if col < 0 || col >= len(pinned) || !pinned[col] {
			return false
		}
	}
	return true
}

// scopeColumn is the column of s that e names, when it names one.
func scopeColumn(c *compiler, s joinSource, e Expr) (int, bool) {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.UsingRepr || ce.Schema != "" {
		return 0, false
	}
	cursor, col, isRowid, found, _, hard := resolveInScopes(c.scopes, ce, c.pager)
	return col, hard == nil && found && !isRowid && cursor == s.scope.cursor
}

// orderedBy reports whether C walks s in the order of key, the rowid or the
// leading column of a declared index.
func orderedBy(c *compiler, s joinSource, key Expr) bool {
	if !s.tbl.withoutRowid && isScopeRowidRef(c, s, key) {
		return true
	}
	col, ok := scopeColumn(c, s, key)
	if !ok || s.notIndexed {
		return false
	}
	cands, err := c.pager.secondaryIndexSeekCandidates(s.scope.tableName, s.tbl.root, s.tbl.cols)
	return err == nil && slices.ContainsFunc(cands, func(x indexSeekCandidate) bool { return x.leadingCol == col })
}

// planBillMinMax caps the rows counted as read for "SELECT min(x) FROM t" or
// max, with no WHERE, at one: where x is the rowid or leads a declared index,
// C reads the one row at that end (select.c's minMaxQuery).
func planBillMinMax(c *compiler, srcs []joinSource, stmt *SelectStmt) {
	if len(srcs) != 1 || stmt.Where != nil || len(stmt.GroupBy) != 0 || len(stmt.Columns) != 1 || c.pager == nil {
		return
	}
	s := srcs[0]
	f, ok := stmt.Columns[0].Expr.(FuncExpr)
	if !ok || f.Distinct || f.Star || len(f.Args) != 1 || !strings.EqualFold(f.Name, "min") && !strings.EqualFold(f.Name, "max") ||
		s.derived != nil || s.tbl == nil || s.dbIdx != 0 {
		return
	}
	if orderedBy(c, s, f.Args[0]) {
		srcs[0].billLimit = 2
	}
}

// planBillLimit caps the rows counted as read for a one-table SELECT with no
// WHERE, ordered first by the rowid or by the leading column of a declared
// index, with a LIMIT: C walks the rowid or the index in that order and stops
// after LIMIT + OFFSET rows (where.c's ORDER BY consumption).
func planBillLimit(c *compiler, srcs []joinSource, stmt *SelectStmt) {
	if len(srcs) != 1 || stmt.Where != nil || stmt.Limit == nil || *stmt.Limit < 0 || len(stmt.OrderBy) == 0 ||
		len(stmt.GroupBy) != 0 || stmt.Distinct || len(stmt.Compound) != 0 {
		return
	}
	s := srcs[0]
	if s.derived != nil || s.tbl == nil || s.dbIdx != 0 || c.pager == nil {
		return
	}
	n := *stmt.Limit
	if stmt.Offset != nil && *stmt.Offset > 0 {
		n += *stmt.Offset
	}
	ordered := orderedBy(c, s, stmt.OrderBy[0].Expr)
	if ordered && n < math.MaxInt32 {
		srcs[0].billLimit = int(n) + 1
	}
}

// prologueReadBound is the cap the OpRowidReadBound in the prologue of cur's
// loop, from pc to its OpRewind, puts on the loop's reads, for a kernel that
// answers the loop before the prologue runs: its bounds are the constants
// and parameters the prologue loads, read here. No bound is no cap.
func (m *vdbe) prologueReadBound(cur *vdbeCursor, insns []Instruction, pc int) int64 {
	from := pc
	for ; pc < len(insns) && insns[pc].Op != OpRewind; pc++ {
		in := &insns[pc]
		if in.Op != OpRowidReadBound {
			continue
		}
		kinds, _ := in.P4.([]int)
		keys := make([]Value, len(kinds))
		for i := range keys {
			keys[i] = Value{Typ: Null}
			for j := from; j < pc; j++ {
				switch k := &insns[j]; {
				case k.P2 != in.P2+i:
				case k.Op == OpInteger:
					keys[i] = Value{Typ: Int, I: int64(k.P1)}
				case k.Op == OpInt64:
					if v, ok := k.P4.(int64); ok {
						keys[i] = Value{Typ: Int, I: v}
					}
				case k.Op == OpVariable:
					keys[i] = paramAt(m.params, k.P1)
				}
			}
		}
		cur.billLimit = int64(in.P3)
		cur.billRangeBy(kinds, keys)
		return cur.readCap()
	}
	return math.MaxInt64
}
