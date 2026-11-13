// This file implements the VDBE's table cursor: a "current (rowid, decoded
// row)" view plus "restart from the first row" over a table scan or a segment
// row source.
//
// A cursor materializes its table once, on its first OpRewind, and later
// rewinds only reset a position index. That is required for correctness: the
// join codegen rewinds every inner cursor once per outer row, and a
// single-pass stream cannot be rewound.
//
// IPK rowid aliasing and REAL-affinity restore go through normalizeRow, so
// OpColumn and OpRowid see the same values every other reader does.
package engine

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
)

// vdbeCursor is one open table cursor. OpOpenRead creates it (openCursor);
// OpRewind (re)starts its iteration, materializing the underlying table on
// the first call (see this file's package doc comment); OpNext advances it;
// OpColumn/OpRowid read the current row straight off it (row/rowid/rowidNull
// below); OpNullRow puts it into the synthetic all-NULL state a LEFT JOIN's
// unmatched row needs; OpClose releases its materialized rows.
type vdbeCursor struct {
	// rjByRowid marks a correlated RIGHT/FULL JOIN operand, whose matches are
	// kept by rowid on the machine (vdbe.rjMatched) rather than in matched.
	rjByRowid bool

	// params is the running statement's bound parameters, set by OpRewind and
	// OpAutoIndexOrder. multiOrOrderCursor needs them: a WHERE disjunct can carry
	// a parameter, and evaluating it against nil params answered NULL, putting
	// every row in the last group (wrong row order for "a=? OR b=?").
	params []Value

	pager *ReadOnlyPager
	tbl   *resolvedTable

	rowids       []uint64  // materialized rowids, filled by rewind()'s first call; nil until then
	rows         [][]Value // parallel decoded (already normalizeRow'd) rows, unless lazyNorm
	materialized bool

	// lazyNorm says rows holds a row store's own STORED rows, not normalized
	// ones, and advance normalizes each into normBuf as it becomes current
	// (materializeRowStore). normBuf is reused row to row, which is sound
	// because every reader of rowVals copies out of it (readColumn, and
	// fullRow's callers) before the cursor moves.
	lazyNorm bool
	// writeHint is how many rows a write statement is about to change through
	// this cursor, when the seek that built it says (OpRewind reserves that much
	// journal); zero otherwise.
	writeHint int
	normBuf   []Value
	// jitLazyMask is jitCursorMask's columns of a lazyNorm row read as stored,
	// worked out on the first entry that asks (jitLazyMaskOK).
	jitLazyMask   uint64
	jitLazyMaskOK bool

	// rowStore, when non-nil, marks this as a WRITE-path cursor over a table's
	// in-memory logical rows (openRowStoreCursor, vdbe_write.go) rather than
	// over a b-tree. Its rows are built on the first rewind and not before --
	// see rewind, and materializeRowStore for why that matters.
	rowStore *tableMeta
	// rowsFrom and rowsSwaps are the store the rows were taken from and its
	// swap count then, for storedRow.
	rowsFrom  *rowStore
	rowsSwaps uint32
	// rowsWrites is the store's write count when the rows marked base in
	// rowsBase were last known to be its rows: at the rewind, and again after
	// each write of the UPDATE's own (opUpdateRow), for storedRow.
	rowsWrites uint64
	// overlaySized says a gathered UPDATE has sized the store's overlay for
	// this rewind's rows already (applyGathered).
	overlaySized bool
	// rowsBase, when not nil, is bit i set where rows[i] came from the
	// store's committed rows rather than its overlay (rowStore.eachOf).
	rowsBase []uint64
	// writeOrder, on a row-store cursor, is the order C's ONE-PASS UPDATE loop
	// visits the table in (DB.updateOnePassOrder), applied to the frozen rows
	// every time they are materialized. Set by OpAutoIndexOrder.
	writeOrder *autoIndexKey
	pos        int // index into rowids/rows of the current row; -1 before the first advance() of a rewind

	rowid     uint64
	rowidNull bool // true after OpNullRow (or, transiently, before any row exists); see OpRowid's body (vdbe.go)

	// rowVals is the current row. NOTHING MAY READ IT DIRECTLY: when rowRaw is
	// non-nil the row is MASKED -- only the columns in colMask hold decoded
	// Values and every other slot is the zero Value, which read as a result
	// would be a silent NULL, the worst outcome this optimization could have.
	// Read a single column with col() and the whole row with fullRow(); both
	// complete a masked row from rowRaw first. The field is named rowVals
	// rather than row precisely so that every reader had to be visited when
	// masking was introduced, and so a new one cannot be added by accident.
	rowVals []Value
	// nullVals is the all-NULL row nullRow hands out, made once per cursor:
	// nothing writes into it (segFullRow refills a buffer of its own).
	nullVals []Value

	// rowRaw is the CURRENT row's undecoded record while rowVals is masked,
	// nil once rowVals is complete (which is always, for every cursor whose
	// colMask is allColumns). It costs nothing to keep: for a row with no
	// overflow it is a sub-slice of the pager-owned page the row was decoded
	// from, which the decoded TEXT/BLOB Values already alias.
	rowRaw []byte

	// colMask is the set of columns this cursor's scan bothers to decode --
	// the ported form of OP_Column's "decode the column the opcode names and
	// nothing else" (vdbe.c:3185-3217). Derived from the compiled program by
	// cursorColumnMasks (vdbe.go) and installed by OpOpenRead; allColumns for
	// every cursor and every path that does not opt in. It is a PURE
	// PERFORMANCE HINT: col()/fullRow() re-decode rather than trust it.
	// scanMask is the columns a materialized cursor's scan decodes from the
	// segments (scanTableCols); the rest read valueUnread until fetched.
	scanMask columnMask
	colMask columnMask

	// seekConfigured/seekKey turn the first rewind() into a rowid point lookup
	// (SeekRowidSegments) instead of a full scan. Set by OpSeekRowidHint; the
	// WHERE conjunct is still evaluated on the fetched row, so the seek only
	// narrows the candidate set and cannot change the result.
	seekConfigured bool
	seekKey        int64
	// seekNone: the key is no integer even under numeric affinity, so no rowid
	// equals it.
	seekNone bool
	// seekSet: the rowids an IN list pins (OpSeekRowidSetHint), ascending and
	// distinct; like seekKey it only narrows.
	seekSetConfigured bool
	seekSet           []int64

	// rowidLo/rowidHi, when ranged, bound the rowids a segment scan visits
	// (OpSeekRowidRangeHint), inclusive. Like seekKey they only skip rows the
	// WHERE rejects.
	ranged           bool
	rowidLo, rowidHi int64

	// idxSeekConfigured/idxSeekProbe/idxSeekColl/idxSeekCol turn the first
	// rewind() into a secondary-index leading-column equality lookup
	// (SeekIndexRowidsSegments) plus a rowid fetch per candidate. Set by
	// OpSeekIndexHint only when the index's leading collation matches the
	// comparison's; idxSeekProbe is already coerced to the comparison's affinity.
	// Like the rowid seek, it only narrows candidates. The rowid seek wins when
	// both apply.
	idxSeekConfigured bool
	idxSeekProbe      Value
	idxSeekColl       string
	// idxSeekSet is the first key's IN list, when it is one (idxSeekKey.set).
	idxSeekSet []Value
	// idxSeekCol is the TABLE column the index leads on.
	idxSeekCol int
	// idxSeekMore: later OpSeekIndexHint slots, equalities on other columns
	// that each bound the rows alone; idxSeekNext is the slot expected next.
	idxSeekMore []idxSeekKey
	idxSeekNext int

	// seekReseek marks a CORRELATED (join inner-side) seek cursor whose seek key
	// changes per outer-loop iteration: unlike an ordinary single-table seek
	// (materialize once, then reset the position index on every later rewind),
	// such a cursor must DISCARD its materialized rows and re-run the seek on
	// every rewind() so it reflects the current outer row's key. Set by
	// OpSeekRowidHint/OpSeekIndexHint when P3==1 (emitted only by
	// emitJoinSeekHint, vdbe_join_seek.go); a plain single-table seek leaves it
	// false, so that path's materialize-once behaviour is byte-for-byte
	// unchanged. See rewind().
	seekReseek bool
	// idxSeekOrder is the key a correlated index seek's rows are sorted by
	// (indexSeekHint.order), nil to keep them in rowid order.
	idxSeekOrder *autoIndexKey
	// idxSeekRaiseFree is indexSeekHint.raiseFree.
	idxSeekRaiseFree bool
	// idxListOnce is set by OpAutoIndexOrder on an index seek that runs once:
	// its rows are positioned as a correlated seek's are, sorted by
	// idxSeekOrder, rather than built and permuted. idxListReady is that the
	// list OpAutoIndexOrder positioned stands for every rewind after it, as
	// the built rows would have.
	idxListOnce, idxListReady bool
	// keyListReady is the same for an outermost scan OpAutoIndexOrder
	// positioned on the table's rows in its key's order (segKeyOrderList).
	keyListReady bool

	// pendingOrder is an OpAutoIndexOrder not yet applied: the rows are put
	// in its order at the first rewind or seek (applyPendingOrder).
	pendingOrder *Instruction

	// sorted is OpSortedSeek's copy of the rows, sorted by its column, that
	// each seek narrows rows/rowids to a run of. nil until the first seek, and
	// dropped whenever the rows are built again.
	sorted *sortedRows
	// segSorted is the same served from the segments (segSortedSeek), kept
	// across rewinds since nothing is built.
	segSorted *segSortedRows
	// segOrder is the OpAutoIndexOrder segSortedSeek took over: its runs are
	// put in its order, and the rows are built in it if the segments stop
	// serving the seek.
	segOrder *Instruction

	// closed is set by close(); an open makes a fresh cursor.
	closed bool

	// matched is this cursor's RIGHT/FULL JOIN mark bitmap (parallel to
	// rows/rowids) -- nil until OpRightJoinMark or OpRightJoinSweepRewind/Next
	// first touches it, for a cursor that is never a RIGHT/FULL JOIN target
	// at all. See OpRightJoinMark/OpRightJoinSweepRewind/OpRightJoinSweepNext
	// (vdbe_op.go) and this file's markMatched/sweepUnmatchedFrom.
	matched []bool

	// multiOrGrp is parallel to rows while a multi-table WHERE_MULTI_OR level
	// is being ordered: the pass OpMultiOrTag filed each row under, consumed
	// (and cleared) by OpMultiOrSort. See multiOrSortTagged.
	multiOrGrp []int

	// vmTab is the cursor's table as native code seeks it (vmJIT.tabFor).
	vmTab *vmTab

	// streamIfOrdered is set by OpOpenRead (P5 4) on the outermost scan when
	// only its index-order key kept it from streaming: OpAutoIndexOrder lets it
	// stream when its rows already run in that order (segRowsInKeyOrder).
	streamIfOrdered bool

	// emptyKnown is whether tableEmpty has answered, and empty its answer.
	emptyKnown, empty bool

	// streamable is set by OpOpenRead (P5) when the join codegen proved this
	// cursor is rewound at most once and is not a RIGHT/FULL JOIN mark/sweep
	// target: the outermost level of a plain full scan with no seek hint. Such a
	// cursor iterates lazily via iter.Pull2 over ScanTable, holding only the
	// current row; every other cursor materializes. See emitJoinLoops.
	streamable bool

	// segSrc/segCur/segRow are the LAZY SEGMENT row source (segment_cursor.go),
	// the third stream kind. segCur non-nil means the current row lives in a
	// segment rather than in a decoded record: readColumn fetches one column
	// from it, and rowVals holds nothing anyone may read.
	segSrc *segRowSource
	// pointSrc is the source a rowid or index seek positions (segPointSeek,
	// segListSeek), held here so a seek per outer row allocates none.
	pointSrc segRowSource
	// seekGen counts the seeks segSrc was positioned by, for the JIT's copy
	// of their rows (seekSnap).
	seekGen uint64
	// seekSlab backs an index seek's fixed-up rows, reused by a reseek.
	seekSlab         []Value
	segCur           *segment
	segRow           int
	segFilter        *segRowFilter // the compiled pre-filter from OpRewind's P4
	segWidth         int           // segCur.Width(segRow), read once per row
	segSrcCols       []segColPlan  // segSrc.cols, hoisted out of the read path
	streamErrPending error

	// fill, when non-nil, produces this derived, CTE or virtual-table cursor's
	// rows on its first rewind rather than at OpOpenDerived. sqlite3WhereBegin
	// codes a materialization at the top of the loop of the level it belongs to
	// (the OP_Gosub to addrFillSub, under OP_Once), so a subquery joined after
	// an empty table, or behind a false constant WHERE term, never runs -- an
	// error its body would raise is not raised. keepSubtype is the derived
	// cursor's flattenedSubtypeKeep (see openDerivedCursor); clearSubtypes is
	// false for a virtual table, whose cursor keeps them.
	fill          func() ([][]Value, []int64, error)
	fillKeep      []bool
	clearSubtypes bool
	// refillEach keeps fill after it runs, so every rewind produces the rows
	// afresh: a co-routine C codes as an INNER loop, whose OP_InitCoroutine
	// sits inside the loop (wherecode.c:1550) and restarts the body each time
	// the loop is entered. Set by OpOpenDerived for derivedSource.rerun.
	refillEach bool
	// slotFill marks a cursor openSlotFillCursor made, which reopening its slot
	// resets in place: nothing but the slot holds it.
	slotFill bool
	// slot is what a reopen of such a cursor keeps (openSlotFillCursor).
	slot *slotFill

	// sub, when non-nil, makes this a DERIVED-table cursor whose rows come from
	// a co-routine (openStreamCursor, vdbe_stream.go) rather than a
	// materialized row list: each advance resumes the source program for one
	// row. Its errors travel through streamErrPending like stream's.
	sub *subStream

	// vt, when non-nil, makes this a virtual-table cursor read through the
	// module's live cursor (vtab_stream.go) rather than its materialized rows.
	vt *vtabStream

	// genCached memoizes hasGeneratedCols(tbl.cols) for this cursor, which
	// normalize() below would otherwise re-derive on EVERY row -- a full scan of
	// the column list per scanned row, ~7% of a filtered full-table scan, to
	// answer a question fixed for the table's whole lifetime. Deliberately
	// tri-state with the ZERO value meaning "not computed yet": a cursor built by
	// any of the three constructors below, or by a future fourth, computes it on
	// first use rather than silently inheriting a wrong `false` -- getting this
	// backwards would skip generated-column computation and answer a query with
	// stale NULLs. A vdbeCursor is per-program-run (openCursor and friends are
	// called from a freshly allocated vdbe, vdbe.go), never shared, so this
	// lazy write races with nothing.
	genCached int8 // 0 = not computed; 1 = has generated columns; 2 = has none

	// realCached memoizes "does tbl declare any REAL-affinity column?" on the
	// same tri-state, zero-value-safe scheme as genCached above. It gates
	// normalizeRowInto's per-column REAL-affinity sweep, which is otherwise
	// re-run over the whole column list for every scanned row of every table --
	// including the overwhelming majority that have no REAL column at all and
	// for which the sweep provably cannot change anything.
	realCached int8 // 0 = not computed; 1 = has a REAL-affinity column; 2 = has none
}

// hasRealCols reports whether this cursor's table declares any REAL-affinity
// column, memoized in realCached (see its doc comment).
func (cur *vdbeCursor) hasRealCols() bool {
	if cur.realCached == 0 {
		cur.realCached = 2
		for _, c := range cur.tbl.cols {
			if c.Aff == affReal {
				cur.realCached = 1
				break
			}
		}
	}
	return cur.realCached == 1
}

// columns is the column list of the table cur reads: a read cursor's, or a
// write cursor's (rowStore).
func (cur *vdbeCursor) columns() []columnInfo {
	if cur.tbl != nil {
		return cur.tbl.cols
	}
	if cur.rowStore != nil {
		return cur.rowStore.cols
	}
	return nil
}

// hasGeneratedCols reports whether this cursor's table declares any generated
// column, memoized in genCached (see its doc comment).
func (cur *vdbeCursor) hasGeneratedCols() bool {
	if cur.genCached == 0 {
		if hasGeneratedCols(cur.columns()) {
			cur.genCached = 1
		} else {
			cur.genCached = 2
		}
	}
	return cur.genCached == 1
}

// idxSeekRowids is the candidates of this cursor's equality seek, false when
// no key can be answered. Each key bounds the rows alone (every conjunct is
// re-checked), so the key the segments estimate smallest answers. A VIRTUAL
// generated column is never a key: rows from before its ADD COLUMN hold NULL
// in its slot.
func (cur *vdbeCursor) idxSeekRowids() ([]int64, bool) {
	stored := func(col int) bool {
		return col < 0 || col >= len(cur.tbl.cols) || !cur.tbl.cols[col].IsGenerated() || cur.tbl.cols[col].GeneratedStored
	}
	if len(cur.idxSeekMore) == 0 {
		if !stored(cur.idxSeekCol) {
			return nil, false
		}
		return cur.pager.SeekIndexRowidsByKeys(cur.tbl.root, []idxSeekKey{{col: cur.idxSeekCol, probe: cur.idxSeekProbe, coll: cur.idxSeekColl, set: cur.idxSeekSet}})
	}
	keys := make([]idxSeekKey, 0, 1+len(cur.idxSeekMore))
	if stored(cur.idxSeekCol) {
		keys = append(keys, idxSeekKey{col: cur.idxSeekCol, probe: cur.idxSeekProbe, coll: cur.idxSeekColl, set: cur.idxSeekSet})
	}
	for _, k := range cur.idxSeekMore {
		if stored(k.col) {
			keys = append(keys, k)
		}
	}
	return cur.pager.SeekIndexRowidsByKeys(cur.tbl.root, keys)
}

// seekNarrowRatio is how many rows a native walk reads in the time a seek
// fetches one (about 10 ns against 2.5 us, measured over 300k rows).
const seekNarrowRatio = 256

// readMask is the columns a materialized cursor decodes from the segments
// (scanMask): every one for a table computing generated columns, or one with
// no rowid to fetch the rest by.
func (cur *vdbeCursor) readMask() columnMask {
	cols := cur.scanMask
	if cols == 0 || cur.hasGeneratedCols() || cur.tbl.withoutRowid {
		return allColumns
	}
	if cols != allColumns && cur.tbl.ipkIndex >= 0 {
		cols |= 1 << uint(min(cur.tbl.ipkIndex, 63))
	}
	return cols
}

// seekIsWide reports whether cur's index seek is known to read more than a
// seekNarrowRatio-th of the table: an estimate says so. No estimate is not wide.
func (cur *vdbeCursor) seekIsWide() bool {
	if !cur.idxSeekConfigured || cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil {
		return false
	}
	src, root := cur.pager.segs, cur.tbl.root
	n := -1
	first := idxSeekKey{col: cur.idxSeekCol, probe: cur.idxSeekProbe, coll: cur.idxSeekColl, set: cur.idxSeekSet}
	for _, k := range append([]idxSeekKey{first}, cur.idxSeekMore...) {
		if !cur.idxSeekKeyServed(k) {
			continue
		}
		if c, _, ok := src.seekIndexKeyEstimate(root, k); ok && (n < 0 || c < n) {
			n = c
		}
	}
	if n < 0 {
		return false
	}
	total, ok := src.segTableRows(root)
	return ok && n > total/seekNarrowRatio
}

// rowidsAreWide reports whether n rows fetched one by one are more than a
// seekNarrowRatio-th of cur's table.
func (cur *vdbeCursor) rowidsAreWide(n int) bool {
	if cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil {
		return false
	}
	total, ok := cur.pager.segs.segTableRows(cur.tbl.root)
	return ok && n > total/seekNarrowRatio
}

// seekIsNarrow reports whether the seek reads at most a seekNarrowRatio-th of
// the table, so it should answer rather than a walk. An index seek counts the
// served key with the smallest estimate; no estimate leaves the walk.
func (cur *vdbeCursor) seekIsNarrow() bool {
	if cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil {
		return false
	}
	src, root := cur.pager.segs, cur.tbl.root
	n := -1
	switch {
	case cur.seekConfigured:
		n = 1
	case cur.seekSetConfigured:
		n = len(cur.seekSet)
	case cur.idxSeekConfigured:
		first := idxSeekKey{col: cur.idxSeekCol, probe: cur.idxSeekProbe, coll: cur.idxSeekColl, set: cur.idxSeekSet}
		for _, k := range append([]idxSeekKey{first}, cur.idxSeekMore...) {
			if !cur.idxSeekKeyServed(k) {
				continue
			}
			if c, _, ok := src.seekIndexKeyEstimate(root, k); ok && (n < 0 || c < n) {
				n = c
			}
		}
	}
	if n < 0 {
		return false
	}
	total, ok := src.segTableRows(root)
	return ok && n <= total/seekNarrowRatio
}

// idxSeekKeyServed reports whether the segments can answer key k: not a VIRTUAL
// generated column (idxSeekRowids), and no TEXT value under a sequence other
// than BINARY (SeekIndexRowidsSegments).
func (cur *vdbeCursor) idxSeekKeyServed(k idxSeekKey) bool {
	if c := k.col; c >= 0 && c < len(cur.tbl.cols) && cur.tbl.cols[c].IsGenerated() && !cur.tbl.cols[c].GeneratedStored {
		return false
	}
	if equalFoldName(effectiveCollation(k.coll), "BINARY") {
		return true
	}
	if k.set == nil {
		return k.probe.Typ != Text
	}
	for _, v := range k.set {
		if v.Typ == Text {
			return false
		}
	}
	return true
}

// writeSeekRowids is a write cursor's equality candidates: its store's own
// indexes where prefersEqIndex says so, else the segments' (fromSeg, which
// already drops deleted rows), smallest estimate first. A TEXT probe under a
// non-BINARY collation is no key: both indexes match exact bytes.
func (cur *vdbeCursor) writeSeekRowids() (hits []uint64, fromSeg, ok bool) {
	rows := cur.rowStore.rows
	askOne := func(col int, probe Value, coll string) ([]uint64, bool, bool) {
		if probe.Typ == Text && !equalFoldName(effectiveCollation(coll), "BINARY") {
			return nil, false, false
		}
		if rows.prefersEqIndex(col) {
			if hits, ok := rows.eqRowidsOf(col, probe); ok {
				return hits, false, true
			}
		}
		if hits, ok := rows.segEqRowids(col, probe); ok {
			return hits, true, true
		}
		if hits, ok := rows.eqRowidsOf(col, probe); ok {
			return hits, false, true
		}
		return nil, false, false
	}
	// An IN list's candidates are its values', in rowid order, each once;
	// fromSeg only when the segments named them all.
	ask := func(k idxSeekKey) ([]uint64, bool, bool) {
		if k.set == nil {
			return askOne(k.col, k.probe, k.coll)
		}
		var out []uint64
		fromSeg := true
		for _, v := range k.set {
			hits, seg, ok := askOne(k.col, v, k.coll)
			if !ok {
				return nil, false, false
			}
			out = append(out, hits...)
			fromSeg = fromSeg && seg
		}
		slices.SortFunc(out, cmpRowid)
		return slices.Compact(out), fromSeg, true
	}
	first := idxSeekKey{col: cur.idxSeekCol, probe: cur.idxSeekProbe, coll: cur.idxSeekColl, set: cur.idxSeekSet}
	if len(cur.idxSeekMore) == 0 {
		return ask(first)
	}
	keys := append([]idxSeekKey{first}, cur.idxSeekMore...)
	est := make([]int, len(keys))
	for i, k := range keys {
		est[i] = rows.segEqKeyEstimate(k)
	}
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(est[a], est[b]) })
	for _, i := range order {
		if hits, fromSeg, ok := ask(keys[i]); ok {
			return hits, fromSeg, true
		}
	}
	return nil, false, false
}

// normalize applies normalizeRowInPlace's IPK-rowid-alias and REAL-affinity
// fix-ups to vals (which this cursor owns -- see normalizeRowInPlace's own doc
// comment for that requirement), passing this cursor's MEMOIZED
// generated-columns answer rather than paying for a fresh derivation per row.
func (cur *vdbeCursor) normalize(rowid uint64, vals []Value) []Value {
	if !cur.hasGeneratedCols() && len(vals) < len(cur.tbl.cols) {
		// A row written before an ALTER TABLE ADD COLUMN is narrower than the
		// column list; widen it before anything indexes it by column position.
		// See padStoredRow (query.go).
		vals = padStoredRow(cur.tbl.cols, vals)
	}
	if !cur.hasGeneratedCols() && !cur.hasRealCols() {
		// Neither fix-up normalizeRowInto performs can apply except the IPK
		// rowid-alias substitution, so do just that and skip the per-column
		// REAL-affinity sweep entirely. This is normalizeRowInto's own first
		// two lines verbatim, under a condition that makes its remaining loop
		// provably a no-op (no column has affReal, so its body never fires).
		if cur.tbl.ipkIndex >= 0 && vals[cur.tbl.ipkIndex].Typ == Null {
			vals[cur.tbl.ipkIndex] = Value{Typ: Int, I: int64(rowid)}
		}
		return vals
	}
	return normalizeRowInPlace(cur.tbl.name, cur.tbl.cols, cur.tbl.ipkIndex, rowid, vals, cur.hasGeneratedCols())
}

// normalizeFrom is normalize for a row this cursor may not own. A write
// session's live row store hands out its own slices (ReadOnlyPager.
// ServesLiveRows), which the store, its BEGIN/SAVEPOINT snapshots and every
// other cursor share, so those are fixed up in a copy (normalizeRow). Editing
// them in place wrote Int(rowid) into the stored IPK slot and Float into REAL
// columns on every read, so what a ROLLBACK restored depended on what had been
// read since BEGIN.
func (cur *vdbeCursor) normalizeFrom(shared bool, rowid uint64, vals []Value) []Value {
	if shared {
		return normalizeRow(cur.tbl.name, cur.tbl.cols, cur.tbl.ipkIndex, rowid, vals)
	}
	return cur.normalize(rowid, vals)
}

// openCursor creates a cursor for tbl, ready for its first OpRewind to
// materialize and iterate it. Unlike the single-table-only increment, no scan
// is started here -- OpRewind is what (re)starts iteration, since it may run
// more than once per program execution (once per outer-loop combination, for
// every join level but the outermost).
func openCursor(p *ReadOnlyPager, tbl *resolvedTable) *vdbeCursor {
	return &vdbeCursor{pager: p, tbl: tbl, colMask: allColumns, scanMask: allColumns}
}

// readColumn writes column i of the current row into *dst -- OpColumn's whole
// body. It writes through a pointer because a 48-byte Value return kept it
// from inlining into the hottest opcode; this way the unmasked path inlines to
// one branch on rowRaw.
//
// The mask is a hint: a column outside it is never answered from its
// undecoded slot (that would be a silent NULL). readColumnFromMaskedRow
// completes the row from the retained record first.
func (cur *vdbeCursor) readColumn(i int, dst *Value) error {
	if cur.segCur != nil {
		*dst = cur.segColumn(i)
		return nil
	}
	if cur.rowRaw != nil {
		return cur.readColumnFromMaskedRow(i, dst)
	}
	*dst = cur.rowVals[i]
	if dst.Typ >= valueGenErr {
		return cur.oddSlot(i, dst)
	}
	return nil
}

// oddSlot is readColumn's cold half for a slot holding a marker rather than a
// value: a VIRTUAL column's error, or a column a masked scan left unread,
// which brings the row in whole first.
func (cur *vdbeCursor) oddSlot(i int, dst *Value) error {
	if dst.Typ == valueUnread {
		if err := cur.fillUnread(); err != nil {
			return err
		}
		*dst = cur.rowVals[i]
	}
	if dst.Typ == valueGenErr {
		return takeGenErr(dst)
	}
	return nil
}

// fillUnread replaces the current row, which a masked scan decoded only in
// part (scanTableCols), with the whole row, in the cursor's list too.
func (cur *vdbeCursor) fillUnread() error {
	vals, found, served := cur.pager.SeekRowidSegments(cur.tbl.root, int64(cur.rowid))
	if !served || !found {
		return fmt.Errorf("engine: internal: row %d of %s, read in part, is not there whole", cur.rowid, cur.tbl.name)
	}
	cur.rowVals = cur.normalizeFrom(true, cur.rowid, vals)
	if cur.pos >= 0 && cur.pos < len(cur.rows) {
		cur.rows[cur.pos] = cur.rowVals
	}
	return nil
}

// takeGenErr is the error a valueGenErr slot read into *dst holds, with *dst
// left NULL so the marker goes no further than its read.
func takeGenErr(dst *Value) error {
	err := genErrOf(*dst)
	*dst = Value{}
	return err
}

// col is readColumn in expression position, for readers that want a value
// rather than a destination (the tests, mostly). One implementation, so the
// two cannot drift; the opcode itself uses readColumn.
func (cur *vdbeCursor) col(i int) (Value, error) {
	var v Value
	err := cur.readColumn(i, &v)
	return v, err
}

// readColumnFromMaskedRow is readColumn's cold half, reached only for a cursor
// holding a MASKED row -- the only state in which rowRaw is non-nil. Split out
// rather than inlined into readColumn so that readColumn itself stays within
// the inlining budget; see its doc comment for what that was worth.
func (cur *vdbeCursor) readColumnFromMaskedRow(i int, dst *Value) error {
	if !cur.colMask.has(i) {
		if err := cur.decodeFullRow(); err != nil {
			return err
		}
	}
	*dst = cur.rowVals[i]
	if dst.Typ == valueGenErr {
		return takeGenErr(dst)
	}
	return nil
}

// fullRow returns the current row with EVERY column decoded, for the readers
// that consume a whole row at once rather than naming a column (gatherCursorRow,
// gatherScopesRow, gatherFrameRow). Same discipline as readColumn: it completes a
// masked row rather than handing back its undecoded slots.
func (cur *vdbeCursor) fullRow() ([]Value, error) {
	if cur.segCur != nil {
		return cur.segFullRow(), nil
	}
	if cur.rowRaw != nil {
		if err := cur.decodeFullRow(); err != nil {
			return nil, err
		}
	}
	if slices.ContainsFunc(cur.rowVals, func(v Value) bool { return v.Typ == valueUnread }) {
		if err := cur.fillUnread(); err != nil {
			return nil, err
		}
	}
	// A whole row goes on to readers that do not name a column, so a VIRTUAL
	// column's error is raised for the row.
	if cur.hasGeneratedCols() {
		if err := rowGenErr(cur.rowVals); err != nil {
			return nil, err
		}
	}
	return cur.rowVals, nil
}

// decodeFullRow re-decodes the current row from its retained record with no
// mask and re-applies normalize as advance() does, so a completed row equals
// what an unmasked scan would have produced. It cannot fail in practice (the
// masked decode already validated the record), but the error is propagated
// because swallowing it would leave a masked row in place -- the silent NULL
// this design exists to prevent.
func (cur *vdbeCursor) decodeFullRow() error {
	vals, err := decodeRecordIntoEnc(cur.rowRaw, cur.rowVals, cur.pager.encoding())
	if err != nil {
		return err
	}
	cur.rowVals = cur.normalize(cur.rowid, vals)
	cur.rowRaw = nil
	return nil
}

// openDerivedCursor creates a cursor over a derived table's already-executed
// rows (OpOpenDerived). It is born materialized, so OpRewind only resets the
// position. tbl describes the output columns (used by nullRow for the column
// count); rowids is an all-zero parallel slice, since a derived table exposes
// no rowid.
//
// keep is flattening's exception (flattenedSubtypeKeep): a column C reads
// through a flattened subquery as a bare column reference keeps its subtype.
func openDerivedCursor(tbl *resolvedTable, rows [][]Value, keep []bool) *vdbeCursor {
	// A value read back as a column of a derived row source carries no subtype,
	// even if the sub-Program tagged it. subtype1.test states the rule for CTEs
	// (MATERIALIZED or not) and it holds for views too. A scalar subquery keeps
	// its tag (its value is the expression's result, not a column read), and so
	// does a virtual table column: SQLite re-runs xColumn per read, which is why
	// this clearing lives here and not in openMaterializedCursor.
	for _, row := range rows {
		clearDerivedSubtypes(row, keep)
		derivedRealAffinity(row, tbl)
	}
	return openMaterializedCursor(tbl, rows, nil)
}

// derivedRealAffinity applies a derived row source's REAL column affinity to
// row: a subquery's table is read like any other, and sqlite3ColumnDefault
// follows OP_Column on a REAL column with OP_RealAffinity, so an integer that
// came back out of a sorter or a record as one -- a MEM_IntReal value written
// as an integer (recordValue) -- reads as the REAL it was.
func derivedRealAffinity(row []Value, tbl *resolvedTable) {
	if tbl == nil {
		return
	}
	for i := range row {
		if i < len(tbl.cols) && tbl.cols[i].Aff == affReal && !tbl.cols[i].noRealRead && row[i].Typ == Int {
			row[i] = Value{Typ: Float, F: float64(row[i].I), Subtype: row[i].Subtype}
		}
	}
}

// clearDerivedSubtypes clears the subtype of every column of row that keep
// does not name.
func clearDerivedSubtypes(row []Value, keep []bool) {
	for i := range row {
		if i >= len(keep) || !keep[i] {
			row[i].Subtype = 0
		}
	}
}

// openMaterializedCursor is openDerivedCursor's more general form: a cursor
// born already materialized over rows, with REAL rowids (rowids[i] is row
// i's rowid) rather than openDerivedCursor's all-zero placeholder. Used for a
// VIRTUAL TABLE row source (vtab.go, resolveVtabSource,
// vdbe_join_codegen.go) -- unlike an ordinary derived table, a virtual
// table's cursor DOES expose a real rowid pseudo-column (its Rowid()), and
// materializeVtab (vtab.go) returns exactly this (rows, rowids) shape.
// rowids shorter than rows (including nil, openDerivedCursor's case) pads
// the remainder with 0, matching a plain derived table's rowid-less rows.
func openMaterializedCursor(tbl *resolvedTable, rows [][]Value, rowids []int64) *vdbeCursor {
	// The JSON subtype is deliberately NOT cleared here -- see openDerivedCursor
	// above, which clears it for its own rows before delegating.
	return &vdbeCursor{
		tbl:          tbl,
		rows:         rows,
		rowids:       materializedRowids(len(rows), rowids),
		materialized: true,
		colMask:      allColumns,
	}
}

// materializedRowids is n rows' rowids as a materialized cursor holds them,
// 0 past the end of rowids.
func materializedRowids(n int, rowids []int64) []uint64 {
	rids := make([]uint64, n)
	for i := range rids {
		if i < len(rowids) {
			rids[i] = uint64(rowids[i])
		}
	}
	return rids
}

// openSlotFillCursor is openFillCursor for a CTE's or a subquery's rows into
// slot p, reusing the cursor p holds when it was one of these: a subroutine
// that reads one again for every outer row (an EXISTS over it) allocated a
// cursor each time.
//
// A reopen by the same OpOpenDerived -- the same derived source and run-once
// slot over the same pager -- keeps its fill function, made once (newFill),
// and the rows that function last gave: a fill that gives the same rows again
// skips the pass over them and keeps their rowids (rewind).
//
// newFill also makes the fill's peek: the rows it would give, when it would
// give them without running anything (its run-once cache is filled), which
// lets native code reopen the cursor (vdbeCursor.slotReopenable).
func (m *vdbe) openSlotFillCursor(p, slot int, ds *derivedSource, pager *ReadOnlyPager, newFill func() (func() ([][]Value, []int64, error), func() ([][]Value, bool))) *vdbeCursor {
	if cur := m.cursors[p]; cur != nil && cur.slotFill {
		sf := cur.slot
		if sf.ds != ds || sf.slot != slot || sf.pager != pager {
			*sf = slotFill{ds: ds, slot: slot, pager: pager, norms: &m.stmtRoot().rowNorms}
			sf.fill, sf.peek = newFill()
		}
		cur.slotOpened()
		return cur
	}
	sf := &slotFill{ds: ds, slot: slot, pager: pager, norms: &m.stmtRoot().rowNorms}
	sf.fill, sf.peek = newFill()
	cur := openFillCursor(ds.tbl, sf.fill, true, ds.keepSubtype)
	cur.slotFill, cur.slot = true, sf
	return cur
}

// slotFill is a slot-fill cursor's fill function, what it reads, and the rows
// it last gave, as rewind left them.
type slotFill struct {
	ds    *derivedSource
	slot  int
	pager *ReadOnlyPager
	fill  func() ([][]Value, []int64, error)
	peek  func() ([][]Value, bool)
	rows  [][]Value
	ids   []uint64
	// norms is the statement root's rowNorms.
	norms *map[rowNormKey][][]Value
}

// normalized reports whether rows, the list a run-once slot holds, was already
// read through ds once in this statement, and so holds its REAL affinity and
// no subtype its read drops; and records it as read now. A correlated
// subquery opens its source in a machine of its own for every outer row, so
// the slot is new each time while the list is the same.
func (sf *slotFill) normalized(rows [][]Value) bool {
	if sf.norms == nil || len(rows) == 0 || !runOnceSlot(sf.ds) {
		return false
	}
	k := rowNormKey{ds: sf.ds, first: &rows[0], n: len(rows)}
	if _, ok := (*sf.norms)[k]; ok {
		return true
	}
	if *sf.norms == nil {
		*sf.norms = map[rowNormKey][][]Value{}
	}
	(*sf.norms)[k] = rows
	return false
}

// onceRows is the rows run-once slot slot holds, when it has run.
func (m *vdbe) onceRows(slot int) ([][]Value, bool) {
	if slot < 0 || slot >= len(m.subCache) {
		return nil, false
	}
	e := &m.subCache[slot]
	return e.rows, e.done
}

// slotOpened is a slot-fill cursor as openSlotFillCursor opens it again.
func (cur *vdbeCursor) slotOpened() {
	sf := cur.slot
	*cur = vdbeCursor{tbl: sf.ds.tbl, fill: sf.fill, clearSubtypes: true, fillKeep: sf.ds.keepSubtype, colMask: allColumns, pos: -1, slotFill: true, slot: sf}
}

// slotRewound is a slot-fill cursor as an open and a rewind leave it when the
// fill gives its slot's rows again (rewind).
func (cur *vdbeCursor) slotRewound() {
	sf := cur.slot
	*cur = vdbeCursor{tbl: sf.ds.tbl, clearSubtypes: true, fillKeep: sf.ds.keepSubtype, colMask: allColumns, pos: -1, slotFill: true, slot: sf,
		rows: sf.rows, rowids: sf.ids, materialized: true}
}

// slotReopenable reports whether an OpOpenDerived of ds into run-once slot
// slot would open cur again over the rows it last held: a slot-fill cursor
// of that source, whose fill would give them without running anything.
func (cur *vdbeCursor) slotReopenable(ds *derivedSource, slot int) bool {
	if cur == nil || !cur.slotFill {
		return false
	}
	sf := cur.slot
	if sf.ds != ds || sf.slot != slot || sf.peek == nil || len(sf.rows) == 0 || len(sf.ids) != len(sf.rows) {
		return false
	}
	rows, ok := sf.peek()
	return ok && sameRows(rows, sf.rows)
}

// sameRows reports whether a and b are the same rows: one array, one length.
func sameRows(a, b [][]Value) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}

// cursorHasNoBtree reports whether cur is a pre-materialized row source (a
// derived table, CTE or virtual table) with no pager or b-tree behind it.
//
// The seek-hint opcodes must skip such a cursor: seekReseek clears
// cur.materialized on the next rewind, which then dereferences the nil pager.
// e.g. an rtree joined to a table on "t2.id = r.rowid". Skipping a hint is
// always correct, since the keyed WHERE conjunct is re-evaluated anyway. A
// write-path row-store cursor is deliberately not covered: rewind
// materializes it from the row store before any b-tree branch.
func cursorHasNoBtree(cur *vdbeCursor) bool {
	return cur != nil && cur.pager == nil && cur.rowStore == nil
}

// rewind (re)starts the cursor's iteration at "before the first row" (the
// caller must still call advance to reach the first row itself, exactly
// mirroring C SQLite's OP_Rewind, which likewise leaves the FIRST row
// current as part of the same opcode -- see OpRewind's body in vdbe.go,
// which calls rewind then advance together). The underlying table scan is
// materialized only on this cursor's very first rewind (see this file's
// package doc comment); every later call just resets the position index.
// The bound kinds of OpSeekRowidRangeHint's P3.
const (
	rowidRangeGT = iota
	rowidRangeGE
	rowidRangeLT
	rowidRangeLE
)

// narrowRowidRange intersects the cursor's rowid range with one bound. A bound
// past the end of int64 (rowid > max, rowid < min) is left out: it would wrap.
func (cur *vdbeCursor) narrowRowidRange(kind int, k int64) {
	if !cur.ranged {
		cur.ranged, cur.rowidLo, cur.rowidHi = true, math.MinInt64, math.MaxInt64
	}
	switch kind {
	case rowidRangeGT:
		if k < math.MaxInt64 {
			cur.rowidLo = max(cur.rowidLo, k+1)
		}
	case rowidRangeGE:
		cur.rowidLo = max(cur.rowidLo, k)
	case rowidRangeLT:
		if k > math.MinInt64 {
			cur.rowidHi = min(cur.rowidHi, k-1)
		}
	case rowidRangeLE:
		cur.rowidHi = min(cur.rowidHi, k)
	}
}

// seekRowidKey is the rowid OP_SeekRowid seeks for v: numeric affinity, then a
// REAL with no fractional part as that integer, however far it is from zero
// (applyNumericAffinity's bTryForInt; "9007199254740992.0" seeks
// 9007199254740992). false is no integer, which no rowid equals.
func seekRowidKey(v Value) (int64, bool) {
	if v.Typ != Int {
		v = applyAffinityToValue(v, affNumeric)
		if v.Typ == Float {
			return intFoldWide(v.F)
		}
	}
	return v.I, v.Typ == Int
}

// narrowRowidRangeBy is narrowRowidRange for a bound of any class, read as
// OP_SeekGT and kin read it: numeric affinity, then a REAL by the integers
// either side. A NULL, TEXT or BLOB bound leaves no row for a lower bound and
// every row for an upper one. Exact, so the WHERE never runs on rows C skips.
func (cur *vdbeCursor) narrowRowidRangeBy(kind int, key Value) {
	if key.Typ != Int {
		key = applyAffinityToValue(key, affNumeric)
	}
	lower := kind == rowidRangeGT || kind == rowidRangeGE
	none := func() { cur.ranged, cur.rowidLo, cur.rowidHi = true, math.MaxInt64, math.MinInt64 }
	switch key.Typ {
	case Int:
		cur.narrowRowidRange(kind, key.I)
	case Float:
		// Integral values folded to Int above; what is left is no integer.
		switch f := key.F; {
		case f >= math.MaxInt64:
			if lower {
				none()
			}
		case f < math.MinInt64:
			if !lower {
				none()
			}
		case lower:
			cur.narrowRowidRange(rowidRangeGE, int64(math.Ceil(f)))
		default:
			cur.narrowRowidRange(rowidRangeLE, int64(math.Floor(f)))
		}
	case Null:
		none()
	default:
		if lower {
			none()
		}
	}
}

// tableEmpty reports whether the cursor's table has no row, from its first,
// for a cursor no loop reads (OpIfEmpty's own); ok is false where the answer
// is the rewind's. Known once, as a rewind materializes once.
func (cur *vdbeCursor) tableEmpty() (empty, ok bool) {
	if cur.emptyKnown {
		return cur.empty, true
	}
	if cur.tbl == nil || cur.pager == nil || cur.materialized || cur.fill != nil || cur.rowStore != nil || cur.sub != nil || cur.vt != nil ||
		cur.seekConfigured || cur.seekSetConfigured || cur.idxSeekConfigured || cur.seekReseek || cur.ranged || cur.segFilter != nil {
		return false, false
	}
	seq, errFn := cur.pager.scanTableCols(cur.tbl.root, 1)
	empty = true
	for range seq {
		empty = false
		break
	}
	if errFn() != nil {
		return false, false
	}
	cur.emptyKnown, cur.empty = true, empty
	return empty, true
}

// startSegSource makes src the cursor's row source: the lazy segment scan.
func (cur *vdbeCursor) startSegSource(src *segRowSource) {
	// The compiled pre-filter, when OpRewind carried one. Purely a
	// candidate-set restriction: the WHERE it was lowered from is still in the
	// program and still runs on every row it selects.
	src.filter = cur.segFilter
	src.params = cur.params
	src.ipkCol = cur.tbl.ipkIndex
	src.ranged, src.rowidLo, src.rowidHi = cur.ranged, cur.rowidLo, cur.rowidHi
	cur.segSrc = src
	cur.segSrcCols = src.cols
	cur.segCur = nil
	cur.seekGen = seekGens.Add(1)
	cur.streamErrPending = nil
	cur.pos = -1
	cur.rowidNull = false
}

// openFillCursor is a cursor whose rows fill produces on its first rewind; see
// vdbeCursor.fill. Until then it is unmaterialized, which is what makes a
// RIGHT JOIN sweep over a level the main pass never reached rewind it first.
func openFillCursor(tbl *resolvedTable, fill func() ([][]Value, []int64, error), clearSubtypes bool, keep []bool) *vdbeCursor {
	return &vdbeCursor{tbl: tbl, fill: fill, clearSubtypes: clearSubtypes, fillKeep: keep, colMask: allColumns, pos: -1}
}

func (cur *vdbeCursor) rewind() error {
	if cur.sorted != nil && (!cur.materialized || cur.fill != nil || cur.seekReseek) {
		// The rows are about to be built again, possibly into the same arrays.
		cur.sorted = nil
	}
	if cur.sub != nil {
		return cur.rewindStream()
	}
	if cur.vt != nil {
		if streamed, err := cur.rewindVtab(); streamed || err != nil {
			return err
		}
	}
	if cur.fill != nil {
		fill := cur.fill
		if !cur.refillEach {
			cur.fill = nil
		}
		rows, rowids, err := fill()
		var tail error
		var te *windowTailErr
		if errors.As(err, &te) {
			// The rows the reader meets before the window select's error,
			// which it meets on asking for the next (windowTailErr).
			rows, rowids, tail, err = te.rows, nil, te.err, nil
		}
		if err != nil {
			return err
		}
		cur.streamErrPending = tail
		if sf := cur.slot; sf != nil && rowids == nil && len(rows) > 0 && len(rows) == len(sf.rows) && &rows[0] == &sf.rows[0] {
			// The rows this slot's last fill gave, as it left them.
			cur.rows, cur.rowids, cur.materialized = rows, sf.ids, true
		} else {
			if cur.clearSubtypes && (sf == nil || cur.refillEach || !sf.normalized(rows)) {
				for _, row := range rows {
					clearDerivedSubtypes(row, cur.fillKeep)
					derivedRealAffinity(row, cur.tbl)
				}
			}
			cur.rows, cur.rowids, cur.materialized = rows, materializedRowids(len(rows), rowids), true
			if sf := cur.slot; sf != nil && rowids == nil {
				sf.rows, sf.ids = rows, cur.rowids
			}
		}
	}
	// A columnar view of this table, when one is attached, is served through
	// ScanTable (segment_read.go) -- so the streaming b-tree cursor is declined
	// for it. Without this the two would SPLIT: a streamable scan would read
	// the b-tree while every other read of the same table read segments. Both
	// are correct, which is exactly what makes the split dangerous: it would
	// make a "columnar" measurement quietly a b-tree one.
	if cur.streamable && !cur.seekConfigured && !cur.idxSeekConfigured && !cur.seekSetConfigured && !cur.tbl.withoutRowid {
		// A columnar view of this table serves the scan ONE COLUMN AT A TIME
		// (segment_cursor.go) when it can. Tried before the b-tree stream and
		// declining to it, so the two never split: a table either scans from
		// segments or from its b-tree, never both in one program.
		if src := cur.pager.newSegRowSource(cur); src != nil {
			cur.startSegSource(src)
			return nil
		}
	}
	if cur.seekReseek {
		// Correlated join seek: the key (idxSeekProbe/seekKey, just re-set by
		// this iteration's OpSeekIndexHint/OpSeekRowidHint) differs from the
		// previous outer row's, so drop the previously materialized rows and
		// re-run the seek below rather than re-iterating the stale set.
		cur.materialized = false
		cur.rows = nil
		cur.lazyNorm = false
		cur.rowids = nil
		cur.matched = nil
	}
	if !cur.materialized {
		if cur.keyListReady && cur.pointSrc.list {
			// The list is the cursor's own (pointSrc), walked to its end or
			// not; a walk past the end let go of segSrc.
			cur.segSrc, cur.segCur = &cur.pointSrc, nil
			cur.pointSrc.listPos = 0
			cur.pos = -1
			return nil
		}
		if cur.rowStore != nil {
			// Whichever exit below built the rows, they are walked in the
			// planned one-pass order when one was installed.
			defer cur.applyWriteOrder()
			// A write-path row-store cursor: rows come from the in-memory row store and
			// are built here, on first rewind, not at open -- OpOpenWrite is emitted by
			// every write compile, including INSERT ... VALUES, and materializing eagerly
			// made bulk loads quadratic. See materializeRowStore.
			//
			// A rowid point lookup skips that walk, which keeps "UPDATE ... WHERE id = ?"
			// from being linear in the table. As on the read path, the keyed WHERE
			// conjunct is re-tested on the fetched row, so the seek only narrows.
			//
			// cur.tbl is nil here (openRowStoreCursor sets only rowStore), so every field
			// comes from the store's own metadata.
			tbl := cur.rowStore
			cur.rowsFrom, cur.rowsBase = nil, nil // set again only where rows are the store's own (storedRow)
			if cur.seekConfigured && !tbl.withoutRowid && tbl.rows != nil {
				rid := uint64(cur.seekKey)
				cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
				if vals, found := tbl.rows.get(rid); found && !cur.seekNone {
					// normalizeRow, not normalizeRowInto over an arena: that is
					// materializeRowStore's own fallback for a single row, and
					// with one row there is nothing for an arena to amortize.
					cur.rows = append(cur.rows, normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, vals))
					cur.rowids = append(cur.rowids, rid)
				}
				cur.materialized = true
				cur.pos = -1
				cur.rowidNull = false
				return nil
			}
			// An IN list over the rowid: the rows it names, ascending, and
			// nothing else is built.
			if cur.seekSetConfigured && !tbl.withoutRowid && tbl.rows != nil {
				hits := make([]uint64, len(cur.seekSet))
				for i, k := range cur.seekSet {
					hits[i] = uint64(k)
				}
				cur.materializeWriteHits(hits, false)
				return nil
			}
			// A column equality (emitWriteIndexSeekHint): the store's equality
			// indexes name the candidates, ascending, and nothing else is built.
			// A key no index can answer falls to the next, and none to the walk.
			if cur.idxSeekConfigured && !tbl.withoutRowid && tbl.rows != nil {
				if hits, fromSeg, ok := cur.writeSeekRowids(); ok {
					writeIndexSeeksServed.Add(1)
					cur.materializeWriteHits(hits, fromSeg)
					return nil
				}
			}
			cur.materializeRowStore()
			cur.pos = -1
			cur.rowidNull = false
			return nil
		}
		// A ROWID SEEK IS SERVED FROM THE SEGMENTS instead of thrown away.
		// Dropping it made "WHERE id = ?" a full scan, which measured 1437x
		// slower than C SQLite at 100,000 rows; segment_seek.go answers it by
		// binary search over the rowid array a segment already stores, with
		// the delta's two maps in front. Same precedence as the merged scan,
		// same normalize, so a seeked row is the row a scan would have found.
		if cur.seekConfigured && !cur.tbl.withoutRowid {
			if cur.seekNone {
				cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
				cur.segSrc, cur.segCur = nil, nil
				cur.materialized = true
				cur.pos = -1
				cur.rowidNull = false
				return nil
			}
			// Positioned, not built, when the segments are the whole answer:
			// columns are read one at a time as the program asks for them.
			// Not marked materialized, so the next rewind seeks again.
			if src, served := cur.pager.segPointSeek(cur, cur.seekKey, &cur.pointSrc); served {
				cur.seekGen = seekGens.Add(1)
				cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
				cur.segSrc, cur.segCur = src, nil
				if src != nil {
					cur.segSrcCols = src.cols
				}
				cur.streamErrPending = nil
				cur.pos = -1
				cur.rowidNull = false
				return nil
			}
			raw, found, served := cur.pager.SeekRowidSegments(cur.tbl.root, cur.seekKey)
			if served {
				cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
				if found {
					cur.rowids = append(cur.rowids, uint64(cur.seekKey))
					cur.rows = append(cur.rows, cur.normalizeFrom(cur.pager.ServesLiveRows(cur.tbl.root), uint64(cur.seekKey), raw))
				}
				cur.materialized = true
				cur.pos = -1
				cur.rowidNull = false
				return nil
			}
		}
		// An IN list over the rowid, each key fetched as the point seek above
		// fetches one. A key the segments do not answer leaves the whole set
		// to the scan below.
		if cur.seekSetConfigured && !cur.tbl.withoutRowid && cur.materializeRowidSet() {
			return nil
		}
		// ...and the SECONDARY-INDEX equality seek, from the leading column's
		// own posting list. It only PRUNES -- the keyed conjunct is re-checked
		// on every row -- so a rowid the list carries that no longer exists is
		// dropped by the row fetch below.
		// A streaming cursor's seek that would fetch a wide share of the
		// table is left for the stream below when it only prunes -- no row
		// it skips could raise -- since fetching its rows one by one costs
		// far more than walking them (seekNarrowRatio).
		prunesOnly := cur.idxSeekRaiseFree && cur.idxSeekOrder == nil && cur.streamable
		if cur.idxSeekConfigured && prunesOnly && cur.seekIsWide() {
			cur.idxSeekConfigured = false
		}
		if cur.idxSeekConfigured && !cur.tbl.withoutRowid {
			if cur.idxListReady && cur.pointSrc.list {
				cur.segSrc, cur.segCur = &cur.pointSrc, nil
				cur.pointSrc.listPos = 0
				cur.pos = -1
				return nil
			}
			cur.idxListReady = false
			if cur.seekReseek || cur.idxListOnce {
				if src, served := cur.pager.segListSeekIndex(cur, &cur.pointSrc); served && src.sortListBy(cur.idxSeekOrder) {
					segIndexSeeksServed.Add(1)
					cur.seekGen = seekGens.Add(1)
					cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
					cur.segSrc, cur.segCur, cur.segSrcCols = src, nil, src.cols
					cur.streamErrPending = nil
					cur.pos = -1
					cur.rowidNull = false
					return nil
				}
			}
			rowids, served := cur.idxSeekRowids()
			if served && prunesOnly && !cur.seekReseek && !cur.idxListOnce && cur.rowidsAreWide(len(rowids)) {
				// No estimate said so, but the list itself does.
				served = false
			}
			if served {
				segIndexSeeksServed.Add(1)
				cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
				// Only for a correlated seek, which nothing reorders: a seek
				// that materializes once may have its rows permuted in place
				// (OpAutoIndexOrder) or tagged by position (OpMultiOrTag).
				if cur.seekReseek || cur.idxListOnce {
					if src, served := cur.pager.segListSeek(cur, rowids, &cur.pointSrc); served && src.sortListBy(cur.idxSeekOrder) {
						cur.seekGen = seekGens.Add(1)
						// Positioned, not built, as a rowid seek is above.
						cur.segSrc, cur.segCur, cur.segSrcCols = src, nil, src.cols
						cur.streamErrPending = nil
						cur.pos = -1
						cur.rowidNull = false
						return nil
					}
				}
				shared := cur.pager.ServesLiveRows(cur.tbl.root)
				// A reseek's rows replace the last seek's, so the copies a fix-up
				// needs are carved from one buffer the cursor keeps rather than
				// allocated per row: an INTEGER PRIMARY KEY, stored NULL, needs
				// one on every row.
				slab := cur.seekSlab[:0]
				plain := shared && !cur.hasGeneratedCols()
				cols := cur.readMask()
				for _, rid := range rowids {
					raw, found, ok := cur.pager.SeekRowidSegmentsCols(cur.tbl.root, rid, cols)
					if !ok || !found {
						continue
					}
					cur.rowids = append(cur.rowids, uint64(rid))
					if plain && len(raw) == len(cur.tbl.cols) && rowNeedsNormalize(cur.tbl.cols, cur.tbl.ipkIndex, raw) {
						at := len(slab)
						slab = append(slab, raw...)
						row := slab[at:len(slab):len(slab)]
						normalizeRowInto(row, cur.tbl.cols, cur.tbl.ipkIndex, uint64(rid))
						cur.rows = append(cur.rows, row)
						continue
					}
					cur.rows = append(cur.rows, cur.normalizeFrom(shared, uint64(rid), raw))
				}
				if cur.seekReseek {
					cur.seekSlab = slab
				}
				if cur.idxSeekOrder != nil {
					autoIndexOrderCursor(cur, cur.idxSeekOrder, cur.pager.encoding())
				}
				cur.materialized = true
				cur.pos = -1
				cur.rowidNull = false
				return nil
			}
		}
		// A seek the segments cannot serve is a scan -- the lazy one where this
		// cursor may stream, as it would have been with no seek planned. An
		// automatic seek (automaticSeekCandidates) declines this way for any
		// column the segments keep no equality index for.
		cur.seekConfigured, cur.idxSeekConfigured, cur.seekSetConfigured = false, false, false
		if cur.streamable && !cur.tbl.withoutRowid {
			if src := cur.pager.newSegRowSource(cur); src != nil {
				cur.startSegSource(src)
				return nil
			}
		}
		// Only the columns the program reads are decoded (scanTableCols); a
		// table computing generated columns, or one with no rowid to fetch the
		// rest by, is read whole.
		seq, errFn := cur.pager.scanTableCols(cur.tbl.root, cur.readMask())
		shared := cur.pager.ServesLiveRows(cur.tbl.root)
		// The copies a shared row's fix-up needs are carved from chunks, as a
		// seek's are, rather than allocated one per row: an INTEGER PRIMARY
		// KEY, stored NULL, needs one on every row.
		plain := shared && !cur.hasGeneratedCols()
		var slab []Value
		interrupted := false
		for rowid, vals := range seq {
			if len(cur.rowids)&0xffff == 0xffff && cur.pager.interrupted() {
				interrupted = true
				break
			}
			// The rowid range bounds this scan as it does the segment one, the
			// rows of the log included. Rowids ascend, so the upper bound ends it.
			if cur.ranged && int64(rowid) > cur.rowidHi {
				break
			}
			if cur.ranged && int64(rowid) < cur.rowidLo {
				continue
			}
			cur.rowids = append(cur.rowids, rowid)
			if n := len(cur.tbl.cols); plain && len(vals) == n && rowNeedsNormalize(cur.tbl.cols, cur.tbl.ipkIndex, vals) {
				if len(slab) < n {
					slab = make([]Value, 256*n)
				}
				row := slab[:n:n]
				slab = slab[n:]
				copy(row, vals)
				normalizeRowInto(row, cur.tbl.cols, cur.tbl.ipkIndex, rowid)
				cur.rows = append(cur.rows, row)
				continue
			}
			cur.rows = append(cur.rows, cur.normalizeFrom(shared, rowid, vals))
		}
		if interrupted {
			return ErrInterrupted
		}
		if err := errFn(); err != nil {
			return err
		}
		// A WITHOUT ROWID TABLE IS ITS PRIMARY KEY INDEX, so that is the order
		// its scan yields -- and a SEGMENT-backed one has no b-tree to get it
		// from: this scan reads a row store keyed by a synthetic id, so the rows
		// come out in the order they were WRITTEN. That is not a harmless order
		// difference, it changes DATA: "CREATE TABLE t(k INTEGER PRIMARY KEY
		// DESC, v) WITHOUT ROWID" holding 1,2,3 then "UPDATE OR REPLACE t SET
		// k=k+1" leaves THREE rows in C -- which walks 3,2,1, so every move
		// lands on a key just vacated -- and ONE here, walking 1,2,3 where each
		// move replaces the row ahead of it.
		if cur.tbl.withoutRowid {
			cur.orderByWithoutRowidPK()
		}
		if cur.idxSeekOrder != nil {
			// The scan a seek fell back to, in the order its index walk visits.
			autoIndexOrderCursor(cur, cur.idxSeekOrder, cur.pager.encoding())
		}
		cur.materialized = true
	}
	cur.pos = -1
	cur.rowidNull = false
	return nil
}

// multiOrOrderCursor permutes a materialized cursor's rows into the order a
// WHERE_MULTI_OR loop emits them: for each disjunct in clause order, the rows
// its sub-scan produces that no earlier one did, in that sub-scan's index
// order. See multiOrOrder for the wherecode.c arm this reproduces.
//
// Only the order changes, never the row set. Rows matching no disjunct are not
// in the answer, so they are parked at the end. A disjunct that fails to
// evaluate counts as not satisfied, as in a WHERE. Each disjunct is a compiled
// program over a row-in-registers block, built by wherePlanMultiOrOrder.
func multiOrOrderCursor(cur *vdbeCursor, mo *multiOrOrder, enc TextEncoding) {
	if mo == nil || len(mo.disjuncts) == 0 || len(mo.keys) != len(mo.disjuncts) ||
		len(cur.rowids) != len(cur.rows) || len(cur.rows) < 2 {
		return
	}
	grp := make([]int, len(cur.rows))
	for i, row := range cur.rows {
		grp[i] = len(mo.disjuncts)
		ctx := &evalCtx{
			tables: mo.scopes,
			vals:   row,
			rowids: []Value{{Typ: Int, I: int64(cur.rowids[i])}},
			pager:  cur.pager,
			params: cur.params,
		}
		for d, prog := range mo.disjuncts {
			v, err := prog.eval(ctx)
			if err == nil && isTruthy(v) {
				grp[i] = d
				break
			}
		}
	}
	perm := make([]int, len(cur.rows))
	for i := range perm {
		perm[i] = i
	}
	ats := make([]func(i, k int) Value, len(mo.keys))
	for g, key := range mo.keys {
		if key == nil {
			continue
		}
		at, ok := autoIndexKeyValues(cur, key)
		if !ok {
			return
		}
		ats[g] = at
	}
	slices.SortStableFunc(perm, func(x, y int) int {
		if grp[x] != grp[y] {
			return grp[x] - grp[y]
		}
		if grp[x] >= len(mo.keys) {
			return 0 // unmatched rows: order unobservable, keep arrival order
		}
		key := mo.keys[grp[x]]
		if key == nil {
			return 0 // that sub-scan is the ascending-rowid table scan
		}
		at := ats[grp[x]]
		for k := range key.cols {
			c := compareValuesCollatedEnc(at(x, k), at(y, k), key.colls[k], enc)
			if c == 0 {
				continue
			}
			if k < len(key.desc) && key.desc[k] {
				return -c
			}
			return c
		}
		return 0
	})
	rows := make([][]Value, len(perm))
	rowids := make([]uint64, len(perm))
	for i, p := range perm {
		rows[i], rowids[i] = cur.rows[p], cur.rowids[p]
	}
	cur.rows, cur.rowids = rows, rowids
}

// multiOrSortTagged is multiOrOrderCursor for the MULTI-TABLE form of a
// WHERE_MULTI_OR level (multiOrOrder.passes): which pass each row belongs to
// was decided by the compiled passes themselves, against the outer rows bound
// right now, and filed on the cursor by OpMultiOrTag. This only sorts: by pass,
// then by that pass's sub-scan key. A nil key is the sub-scan's
// ascending-rowid table scan, compared by rowid explicitly because the rows no
// longer arrive in rowid order after the previous outer row's sort.
//
// An error rather than a guess when the tags do not cover the rows one-for-one
// or the cursor carries a RIGHT JOIN mark bitmap this would have to permute
// too: neither can happen on the path that emits these opcodes.
func multiOrSortTagged(cur *vdbeCursor, mo *multiOrOrder, enc TextEncoding) error {
	if mo == nil || len(cur.multiOrGrp) != len(cur.rows) || len(cur.rowids) != len(cur.rows) || cur.matched != nil {
		return fmt.Errorf("vdbe: OpMultiOrSort without a pass for every row")
	}
	cur.normalizeRowsNow()
	grp := cur.multiOrGrp
	perm := make([]int, len(cur.rows))
	for i := range perm {
		perm[i] = i
	}
	ats := make([]func(i, k int) Value, len(mo.keys))
	for g, key := range mo.keys {
		if key == nil {
			continue
		}
		at, ok := autoIndexKeyValues(cur, key)
		if !ok {
			return fmt.Errorf("vdbe: OpMultiOrSort over an expression key it cannot compute")
		}
		ats[g] = at
	}
	byRowid := func(x, y int) int {
		return cmp.Compare(int64(cur.rowids[x]), int64(cur.rowids[y]))
	}
	slices.SortStableFunc(perm, func(x, y int) int {
		if grp[x] != grp[y] {
			return grp[x] - grp[y]
		}
		if grp[x] < 0 || grp[x] >= len(mo.keys) || mo.keys[grp[x]] == nil {
			return byRowid(x, y)
		}
		key := mo.keys[grp[x]]
		at := ats[grp[x]]
		for k := range key.cols {
			c := compareValuesCollatedEnc(at(x, k), at(y, k), key.colls[k], enc)
			if c == 0 {
				continue
			}
			if k < len(key.desc) && key.desc[k] {
				return -c
			}
			return c
		}
		return byRowid(x, y)
	})
	rows := make([][]Value, len(perm))
	rowids := make([]uint64, len(perm))
	for i, p := range perm {
		rows[i], rowids[i] = cur.rows[p], cur.rowids[p]
	}
	cur.rows, cur.rowids = rows, rowids
	return nil
}

// leadingIndexColumn returns the name of a CREATE INDEX statement's FIRST key
// column, false if it has none it can name (unparseable, or led by an
// expression rather than a bare column).
func leadingIndexColumn(sqlText string) (string, bool) {
	stmt, err := parseCreateIndexStmt(sqlText)
	if err != nil || len(stmt.cols) == 0 || stmt.cols[0] == "" {
		return "", false
	}
	return stmt.cols[0], true
}

// advance moves to the next row, reporting whether one exists. For a streaming
// cursor it pulls the next row off the b-tree iterator (normalizeRow'd exactly
// as the materialized path does, so a streamed row is bit-for-bit identical);
// when the pull is exhausted it captures the scan's deferred error (if any)
// into streamErrPending for OpRewind/OpNext to propagate, since advance itself
// can only return a bool.
func (cur *vdbeCursor) advance() bool {
	if cur.sub != nil {
		return cur.advanceStream()
	}
	if cur.vt != nil {
		return cur.advanceVtab()
	}
	if cur.segSrc != nil {
		s, row, rowid, ok := cur.segSrc.next()
		if !ok {
			cur.segSrc = nil
			cur.segCur = nil
			return false
		}
		cur.pos++
		cur.rowid = rowid
		cur.segCur, cur.segRow = s, row
		// The row's stored width is a per-ROW fact; reading it once here keeps
		// it out of the per-COLUMN path.
		cur.segWidth = s.Width(row)
		if cur.segWidth > len(cur.segSrcCols) {
			cur.segWidth = len(cur.segSrcCols)
		}
		// No row is built. rowVals is left exactly as it was and readColumn
		// never consults it while segCur is set -- see the field comment.
		cur.rowRaw = nil
		return true
	}
	cur.pos++
	if cur.pos < 0 || cur.pos >= len(cur.rowids) {
		return false
	}
	cur.rowid = cur.rowids[cur.pos]
	cur.rowVals = cur.rows[cur.pos]
	if cur.lazyNorm {
		cur.rowVals = cur.normalizeStored(cur.rowVals)
	}
	cur.rowRaw = nil // a materialized row is always complete
	return true
}

// normalizeStored is materializeRowStore's deferred fix-up for the row about to
// become current: the row store's own slice when it needs none, else a copy in
// normBuf. It must never write the stored row, which the store, its snapshots
// and other cursors share.
func (cur *vdbeCursor) normalizeStored(row []Value) []Value {
	tbl := cur.rowStore
	if !rowNeedsNormalize(tbl.cols, tbl.ipkIndex, row) {
		return row
	}
	cur.normBuf = append(cur.normBuf[:0], row...)
	normalizeRowInto(cur.normBuf, tbl.cols, tbl.ipkIndex, cur.rowid)
	return cur.normBuf
}

// normalizeRowsNow ends lazyNorm by normalizing every row in place of its
// stored one, for a reader that compares rows by column rather than walking
// them one at a time (autoIndexOrderCursor). Each fixed-up row is a copy.
func (cur *vdbeCursor) normalizeRowsNow() {
	if !cur.lazyNorm {
		return
	}
	cur.lazyNorm = false
	tbl := cur.rowStore
	for i, row := range cur.rows {
		cur.rows[i] = normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, cur.rowids[i], row)
	}
}

// nullRow puts the cursor into the synthetic all-NULL state OpNullRow needs:
// every column reads NULL and the rowid reads NULL too -- exactly
// normalizeRow's zero Value (Null is Value's zero ValueType) for every
// column, and rowidNull for OpRowid's body (vdbe.go) to check.
func (cur *vdbeCursor) nullRow() {
	cur.segCur = nil // an all-NULL row is not a segment row
	if len(cur.nullVals) != len(cur.tbl.cols) {
		cur.nullVals = make([]Value, len(cur.tbl.cols))
	}
	cur.rowVals = cur.nullVals
	cur.rowRaw = nil // an all-NULL row is complete by construction
	cur.rowidNull = true
}

// close releases the cursor's materialized rows (and drops any streaming
// b-tree cursor, which owns no goroutine). Safe to call more than once, and
// safe even if rewind was never called.
func (cur *vdbeCursor) close() {
	cur.closed = true
	// Same discipline for the segment row: segCur is what tells readColumn to
	// go to the segment instead of rowVals, so a closed cursor must not keep
	// pointing at a row it no longer scans.
	cur.segSrc = nil
	cur.segCur = nil
	cur.sub = nil
	cur.rowids = nil
	cur.rows = nil
	cur.sorted, cur.segSorted, cur.segOrder = nil, nil, nil
	cur.lazyNorm = false
	// BOTH halves of the current row, never just one. rowRaw is what tells
	// readColumn a masked rowVals is incomplete, so clearing rowRaw while
	// leaving rowVals behind would leave the cursor in the one state the mask
	// design forbids: a column outside the mask answered from its undecoded
	// slot, i.e. a silent NULL. Nil-ing rowVals makes a post-close read a
	// nil-slice panic in a test rather than a plausible wrong answer in
	// production -- and no legitimate reader exists, since close() has already
	// dropped stream/rowids/rows.
	cur.rowVals = nil
	cur.rowRaw = nil
}

// markMatched records that cur's CURRENT row (cur.pos, as left by the last
// advance()) has satisfied a RIGHT/FULL JOIN's own ON condition at least
// once -- OpRightJoinMark's body (vdbe.go). Lazily allocates matched (sized
// to the now-final rows count -- materialization always happens before any
// mark can be recorded, since marking only ever follows a real advance()).
// A no-op if cur.pos is out of range (defensive; never actually happens --
// OpRightJoinMark only ever runs immediately after a successful ON test
// inside the cursor's own Rewind/Next loop body).
func (cur *vdbeCursor) markMatched() {
	n, at := len(cur.rows), cur.pos
	if s := cur.sorted; s != nil && s.ok {
		// A seek's run: the mark goes on the row's place in the full list,
		// which the sweep walks.
		n, at = len(s.orig), -1
		if cur.pos >= 0 && s.base+cur.pos < len(s.pos) {
			at = int(s.pos[s.base+cur.pos])
		}
	}
	if cur.matched == nil {
		cur.matched = make([]bool, n)
	}
	if at >= 0 && at < len(cur.matched) {
		cur.matched[at] = true
	}
}

// sweepUnmatchedFrom positions cur at the first row at-or-after index start
// (a plain linear scan over rows, independent of cur.pos/advance -- the
// RIGHT/FULL JOIN second pass this drives runs entirely after the main
// Rewind/Next loop above has finished with this cursor) whose matched bit is
// still unset, reporting whether one was found. Lazily allocates matched (a
// cursor that never once matched, e.g. an entirely empty preceding table,
// reaches here with matched still nil -- every one of its rows is then
// correctly "unmatched"). OpRightJoinSweepRewind (start==0) and
// OpRightJoinSweepNext (start==cur.pos+1) are this method's only two
// callers (vdbe.go).
//
// byRowid, for a cursor matched by rowid (vdbeCursor.rjByRowid), is the set of
// rowids its main pass matched, which decides instead of the bits.
func (cur *vdbeCursor) sweepUnmatchedFrom(start int, byRowid map[uint64]bool) bool {
	if cur.matched == nil {
		cur.matched = make([]bool, len(cur.rows))
	}
	for i := start; i < len(cur.rows); i++ {
		matched := cur.matched[i]
		if cur.rjByRowid {
			matched = byRowid[cur.rowids[i]]
		}
		if !matched {
			cur.pos = i
			cur.rowid = cur.rowids[i]
			cur.rowVals = cur.rows[i]
			if cur.lazyNorm {
				cur.rowVals = cur.normalizeStored(cur.rowVals)
			}
			cur.rowRaw = nil // a materialized row is always complete
			cur.rowidNull = false
			return true
		}
	}
	return false
}

// materializeRowStore fills this cursor from its table's in-memory row store
// in signed-rowid order, normalizing each row as the b-tree path does. Called
// from rewind, i.e. only once a write program actually scans the table.
//
// It is O(table) per write statement, so it avoids what it can without
// changing a value:
//
//   - rows and rowid order come from one pass (rowStore.eachSorted) rather than
//     a key list followed by a b-tree descent per row;
//   - hasGeneratedCols is asked once, not per row;
//   - rows are shared (snapshots, other cursors) and must not be edited, so
//     they are fixed up lazily as they become current (lazyNorm) into one
//     reused buffer.
//
// The whole-store snapshot is load-bearing: it freezes the rowid set and every
// row's slice header before the scan loop runs, while opUpdateRow mutates
// tbl.rows underneath it. That is the Halloween-problem barrier. The map
// store's memoized order is only ever replaced, never edited, so it stays as
// frozen as a fresh one.
func (cur *vdbeCursor) materializeRowStore() {
	tbl := cur.rowStore
	if materializeRowStoreOrderHookForTest != nil {
		materializeRowStoreOrderHookForTest(tbl)
	}
	// ONE pass over the store (eachSorted, row_store.go) rather than a rowid
	// list plus a keyed lookup per rowid. rows holds the store's own slices.
	// SIZED UP FRONT from the store's own count. Go grows a large slice by 1.25x,
	// so appending N times from nil allocates about 5N -- and this walks the whole
	// table, so over 100,000 rows that was ~4 MB of rowids and ~12 MB of row
	// headers per statement, nearly all of it immediately garbage. The same defect
	// was in the change log and in the delta-record conversion it feeds; this is
	// the read side of the same statement.
	n := tbl.rows.len()
	var rowids []uint64
	var rows [][]Value
	if f, st := cur.segFilter, tbl.rows; f != nil && st != nil && st.seg != nil && !readsVirtual(tbl.cols, f.cols) {
		// A write scan the filter peephole judged safe to restrict
		// (segWriteFilterPeephole): only the base rows that can match, and
		// every row the overlay holds, are decoded. Sized for the overlay, which
		// arrives whole, plus a small margin -- not n, nor a fraction of it: the
		// point is that most of the table never arrives, and a point UPDATE
		// keeps one row.
		rowids = make([]uint64, 0, len(st.m)+16)
		rows = make([][]Value, 0, len(st.m)+16)
		st.segEachSortedOver(func(y func(uint64, []Value) bool) {
			st.seg.walkFiltered(f, cur.params, y)
		}, func(rowid uint64, vals []Value) bool {
			rowids = append(rowids, rowid)
			rows = append(rows, vals)
			return true
		})
	} else {
		rowids = make([]uint64, 0, n)
		rows = make([][]Value, 0, n)
		tbl.rows.eachSorted(func(rowid uint64, vals []Value) {
			rowids = append(rowids, rowid)
			rows = append(rows, vals)
		})
	}

	ncols := len(tbl.cols)
	// canNormalize mirrors normalizeRow's own per-row needsCopy test, hoisted to
	// the only part of it that is a property of the TABLE: with no IPK column and
	// no REAL-affinity column, no row can ever need a copy, so the arena below
	// would be pure waste. Generated columns take normalizeRow's widening path
	// and are left to it entirely.
	lazy := rowStoreNormalizesLazily(tbl)

	// The store's rows are shared and must stay untouched, so the fix-up is
	// DEFERRED to the moment each row becomes current (lazyNorm, advance): the
	// frozen slice headers are all the Halloween barrier needs, and a stored row
	// is never edited in place. This used to copy every row of the table into an
	// arena up front -- 33% of every byte a 10,000-row UPDATE over 100,000 rows
	// allocated, for an IPK slot a column read could answer from the rowid.
	for i, rid := range rowids {
		vals := rows[i]
		if lazy && len(vals) == ncols {
			continue // normalized as it becomes current
		}
		// A generated-column table (normalizeRow may WIDEN the row) or a stored
		// row that is not exactly ncols wide takes normalizeRow unchanged, and so
		// does a table no fix-up can apply to (it returns the stored row).
		rows[i] = normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, vals)
	}
	cur.rowids, cur.rows, cur.materialized, cur.lazyNorm = rowids, rows, true, lazy
	cur.rowsFrom, cur.rowsSwaps = tbl.rows, 0
	if tbl.rows != nil {
		cur.rowsSwaps = tbl.rows.swaps
	}
	// A WITHOUT ROWID TABLE IS ITS PRIMARY KEY INDEX, and this store is keyed by a
	// synthetic id, so eachSorted just handed over WRITE order. The read path has
	// the same correction (see rewind's own note) and the WRITE path needs it for a
	// stronger reason than order: the visit order decides which row an OR REPLACE
	// move lands on. "CREATE TABLE t(k INTEGER PRIMARY KEY DESC, v) WITHOUT ROWID"
	// holding 1,2,3 then "UPDATE OR REPLACE t SET k=k+1" leaves THREE rows in C,
	// which walks 3,2,1, and left ONE here.
	if tbl.withoutRowid {
		cur.orderByWithoutRowidPKOf(tbl)
	}
}

// storedRow is the current row as the store handed it to this cursor, with
// the store's swap and write counts then, for rowStore.rowSeen; nil when the cursor holds
// no such row for rowid -- not materialized from st, or the row it holds is a
// normalized copy rather than the store's own.
func (cur *vdbeCursor) storedRow(st *rowStore, rowid uint64) ([]Value, uint32, uint64, seenOrigin) {
	if cur.rowStore == nil || st == nil || cur.rowsFrom != st || !cur.lazyNorm ||
		cur.pos < 0 || cur.pos >= len(cur.rows) || cur.pos >= len(cur.rowids) || cur.rowids[cur.pos] != rowid {
		return nil, 0, 0, seenUnknown
	}
	row := cur.rows[cur.pos]
	if len(row) != len(cur.rowStore.cols) {
		return nil, 0, 0, seenUnknown // normalized when materialized: not the stored slice
	}
	origin := seenUnknown
	if cur.rowsBase != nil {
		origin = seenOverlay
		if cur.rowsBase[cur.pos/64]&(1<<(cur.pos%64)) != 0 {
			origin = seenBase
		}
	}
	return row, cur.rowsSwaps, cur.rowsWrites, origin
}

// storedColumn is column c of the cursor's current row as OP_Column reads it:
// the row its position names, normalized as advance makes it current.
func (cur *vdbeCursor) storedColumn(c int) Value {
	if cur.pos < 0 || cur.pos >= len(cur.rows) {
		return Value{Typ: Null}
	}
	row := cur.rows[cur.pos]
	if cur.lazyNorm {
		row = cur.normalizeStored(row)
	}
	if c >= len(row) {
		return Value{Typ: Null}
	}
	return row[c]
}

// rowStoreNormalizesLazily reports whether a write cursor may hold tbl's stored
// rows as they are and fix them up as each becomes current (lazyNorm): the table
// has a column the fix-up applies to (an INTEGER PRIMARY KEY or a REAL one) and
// no generated column, whose rows normalizeRow may widen.
func rowStoreNormalizesLazily(tbl *tableMeta) bool {
	if hasGeneratedCols(tbl.cols) {
		return false
	}
	if tbl.ipkIndex >= 0 {
		return true
	}
	for _, c := range tbl.cols {
		if c.Aff == affReal {
			return true
		}
	}
	return false
}

// materializeRowStoreOrderHookForTest, when non-nil, is called with the row
// store a scan is about to read, before materializeRowStore consults its
// cached order. Test-only; nil in every shipped path. It lets a test check
// that rowStore.sorted matches sortedRowidsOf at the one moment it must, or
// clear sortedValid to force the uncached path.
var materializeRowStoreOrderHookForTest func(tbl *tableMeta)

// reseekRowStore repositions a write-path row-store cursor on the row its
// current rowid names, re-reading that row's current content, and reports
// whether the row still exists. It is OpNotExists' whole body -- OP_NotExists
// is a seek, not a presence test (vdbe.c:5536/5540, MoveTo + CACHE_STALE).
// The OLD.* loads after it (delete.c:804, update.c:900-910) therefore read the
// row as it is now.
//
// Only the content is refreshed: cur.rows/cur.rowids, the Halloween snapshot,
// stay frozen, so the rowid set the loop visits does not change. That is
// SQLite's own two-pass shape (update.c:847-877; a RowSet for DELETE).
//
// It uses normalizeRow rather than normalize(), since tbl.rows[rowid] belongs
// to the live store, not the cursor.
func (cur *vdbeCursor) reseekRowStore(tbl *tableMeta) bool {
	vals, ok := tbl.rows.get(cur.rowid)
	if !ok {
		return false
	}
	cur.rowVals = normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, cur.rowid, vals)
	cur.rowRaw = nil // a row-store row is always complete
	return true
}

// rowNeedsNormalize is normalizeRow's own needsCopy test for a non-generated
// table, factored out so materializeRowStore can ask it without also paying for
// the copy normalizeRow would make. Must stay in step with normalizeRow
// (query.go): a row needs fixing up when its INTEGER PRIMARY KEY slot is the
// stored NULL that OpColumn must read back as the rowid, or when a
// REAL-affinity column holds an Int that must be restored to Float.
func rowNeedsNormalize(cols []columnInfo, ipkIndex int, vals []Value) bool {
	if ipkIndex >= 0 && vals[ipkIndex].Typ == Null {
		return true
	}
	for i, c := range cols {
		if c.Aff == affReal && vals[i].Typ == Int {
			return true
		}
	}
	return false
}

// reseekRowStoreByPK is reseekRowStore for WITHOUT ROWID tables. update.c's
// pass two seeks by the PRIMARY KEY record pass one froze (update.c:868-869,
// the arm at update.c:847 that a conflict-resolving or triggered UPDATE always
// takes, update.c:733-739). So a later iteration updates whichever row now
// holds that key, and a key nobody holds drops out. That is visible: over
// "t(k INTEGER PRIMARY KEY,v) WITHOUT ROWID" with (1,'x'),(2,'y'),
// "UPDATE OR REPLACE t SET k=k+1" leaves one row (3,'x'), changes()=2.
// Seeking by the row store's internal id, which a PK change does not move,
// got this wrong.
//
// Only the content and internal id are refreshed; the frozen key set stays.
// The key is read from cur.rowVals via indexColumnValue and compared with
// compareValuesCollatedEnc under the PK's collations, the same pair
// findRowConflicts uses. A PK is unique, so map order is unobservable.
func (cur *vdbeCursor) reseekRowStoreByPK(db *DB, tbl *tableMeta) bool {
	pk := tbl.pkIndex
	if pk == nil || len(pk.colIdx) == 0 {
		// Not a shape this can key on (a WITHOUT ROWID table always has one --
		// see tableMeta.pkIndex); fall back to the internal-id seek rather than
		// silently matching every row.
		return cur.reseekRowStore(tbl)
	}
	key := make([]Value, len(pk.colIdx))
	for i, ci := range pk.colIdx {
		if ci < 0 || ci >= len(cur.rowVals) {
			return cur.reseekRowStore(tbl)
		}
		key[i] = indexColumnValue(tbl, cur.rowid, cur.rowVals, ci)
	}
	enc := db.encoding()
	if rowids, ok := tbl.rows.uniqConflictCandidates(tbl, pk, key, enc); ok {
		for _, rid := range rowids {
			if rvals, have := tbl.rows.get(rid); have && uniqueKeyMatches(db, tbl, pk, key, rid, rvals) {
				cur.rowid = rid
				cur.rowVals = normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, rvals)
				cur.rowRaw = nil
				return true
			}
		}
		return false
	}
	for rid, rvals := range tbl.rows.all() {
		match := true
		for i, ci := range pk.colIdx {
			ev := indexColumnValue(tbl, rid, rvals, ci)
			if compareValuesCollatedEnc(ev, key[i], effectiveCollation(atOrEmpty(pk.colCollation, i)), enc) != 0 {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		cur.rowid = rid
		cur.rowVals = normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, rvals)
		cur.rowRaw = nil // a row-store row is always complete
		return true
	}
	return false
}

// applyWriteOrder permutes a row-store cursor's materialized rows into
// writeOrder. A no-op without one.
func (cur *vdbeCursor) applyWriteOrder() {
	if cur.writeOrder == nil || !cur.materialized {
		return
	}
	cur.rowsBase = nil // positional, and the rows are about to move
	enc := UTF8
	if cur.pager != nil {
		enc = cur.pager.encoding()
	}
	autoIndexOrderCursor(cur, cur.writeOrder, enc)
}

// orderByWithoutRowidPKOf is orderByWithoutRowidPK for a write-path cursor,
// whose table is a *tableMeta rather than a resolvedTable.
func (cur *vdbeCursor) orderByWithoutRowidPKOf(tbl *tableMeta) {
	if len(cur.rows) < 2 || tbl == nil {
		return
	}
	// tableMeta carries the PK as an indexMeta -- the synthetic index that keys
	// and orders the table's own b-tree (tableMeta.pkIndex) -- which already has
	// the resolved column indices, the effective collations and the DESC bits.
	pk := tbl.pkIndex
	if pk == nil || len(pk.colIdx) == 0 {
		return
	}
	key := &autoIndexKey{}
	for i, c := range pk.colIdx {
		if c < 0 || c >= len(tbl.cols) {
			return
		}
		key.cols = append(key.cols, c)
		coll := ""
		if i < len(pk.colCollation) {
			coll = pk.colCollation[i]
		}
		key.colls = append(key.colls, coll)
		desc := false
		if i < len(pk.colDesc) {
			desc = pk.colDesc[i]
		}
		key.desc = append(key.desc, desc)
	}
	enc := UTF8
	if cur.pager != nil {
		enc = cur.pager.encoding()
	}
	autoIndexOrderCursor(cur, key, enc)
}

// orderByWithoutRowidPK puts a materialized WITHOUT ROWID cursor's rows into
// PRIMARY KEY order -- the order the table's own index b-tree yields. Only
// needed for a segment-backed table; the key's direction and collations come
// from the declaration, as an index key's do.
func (cur *vdbeCursor) orderByWithoutRowidPK() {
	if len(cur.rows) < 2 || len(cur.tbl.pkColIdx) == 0 {
		return
	}
	key := &autoIndexKey{
		cols:  make([]int, 0, len(cur.tbl.pkColIdx)),
		colls: make([]string, 0, len(cur.tbl.pkColIdx)),
		desc:  make([]bool, 0, len(cur.tbl.pkColIdx)),
	}
	for i, c := range cur.tbl.pkColIdx {
		if c < 0 || c >= len(cur.tbl.cols) {
			return
		}
		key.cols = append(key.cols, c)
		key.colls = append(key.colls, cur.tbl.cols[c].Collation)
		desc := false
		if i < len(cur.tbl.pkDesc) {
			desc = cur.tbl.pkDesc[i]
		}
		key.desc = append(key.desc, desc)
	}
	autoIndexOrderCursor(cur, key, cur.pager.encoding())
}

// materializeWriteHits builds a write cursor's rows from the rowids a seek
// named, dropping those its store does not hold (fromSeg: the segments already
// dropped deleted rows).
func (cur *vdbeCursor) materializeWriteHits(hits []uint64, fromSeg bool) {
	tbl := cur.rowStore
	cur.rowids, cur.rows = slices.Grow(cur.rowids[:0], len(hits)), slices.Grow(cur.rows[:0], len(hits))
	// Rows are fixed up as each becomes current (lazyNorm), as
	// materializeRowStore does, rather than copied here.
	lazy := rowStoreNormalizesLazily(tbl)
	ncols := len(tbl.cols)
	base := make([]uint64, (len(hits)+63)/64)
	tbl.rows.eachOf(hits, fromSeg, func(rid uint64, vals []Value, fromBase bool) {
		if !lazy || len(vals) != ncols {
			vals = normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, vals)
		}
		if fromBase {
			i := len(cur.rows)
			base[i/64] |= 1 << (i % 64)
		}
		cur.rows = append(cur.rows, vals)
		cur.rowids = append(cur.rowids, rid)
	})
	cur.lazyNorm = lazy
	cur.rowsFrom, cur.rowsSwaps, cur.rowsWrites = tbl.rows, tbl.rows.swaps, tbl.rows.writes
	cur.overlaySized = false
	cur.writeHint = len(cur.rows)
	if !slices.IsSortedFunc(cur.rowids, cmpRowid) { // the short rows come unioned at the end
		sortRowsByRowid(cur)
	} else {
		cur.rowsBase = base // positions still those eachOf saw
	}
	cur.materialized = true
	cur.pos = -1
	cur.rowidNull = false
}

// materializeRowidSet builds the rows of an IN list over the rowid
// (seekSet) from the segments, reporting false, with nothing built, when they
// do not answer for this table.
func (cur *vdbeCursor) materializeRowidSet() bool {
	cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
	cur.segSrc, cur.segCur = nil, nil
	shared := cur.pager.ServesLiveRows(cur.tbl.root)
	for _, rid := range cur.seekSet {
		raw, found, served := cur.pager.SeekRowidSegments(cur.tbl.root, rid)
		if !served {
			cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
			return false
		}
		if found {
			cur.rowids = append(cur.rowids, uint64(rid))
			cur.rows = append(cur.rows, cur.normalizeFrom(shared, uint64(rid), raw))
		}
	}
	cur.materialized = true
	cur.pos = -1
	cur.rowidNull = false
	return true
}

// cmpRowid orders two rowids as the signed integers they are.
func cmpRowid(a, b uint64) int { return cmp.Compare(int64(a), int64(b)) }

// sortRowsByRowid orders cur's materialized rows by rowid, keeping each row
// with its rowid.
func sortRowsByRowid(cur *vdbeCursor) {
	idx := make([]int, len(cur.rowids))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return cmpRowid(cur.rowids[a], cur.rowids[b]) })
	rids, rows := make([]uint64, len(idx)), make([][]Value, len(idx))
	for i, j := range idx {
		rids[i], rows[i] = cur.rowids[j], cur.rows[j]
	}
	cur.rowids, cur.rows = rids, rows
}
