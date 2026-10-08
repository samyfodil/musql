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
	"fmt"
	"slices"
)

// vdbeCursor is one open table cursor. OpOpenRead creates it (openCursor);
// OpRewind (re)starts its iteration, materializing the underlying table on
// the first call (see this file's package doc comment); OpNext advances it;
// OpColumn/OpRowid read the current row straight off it (row/rowid/rowidNull
// below); OpNullRow puts it into the synthetic all-NULL state a LEFT JOIN's
// unmatched row needs; OpClose releases its materialized rows.
type vdbeCursor struct {
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
	normBuf  []Value

	// rowStore, when non-nil, marks this as a WRITE-path cursor over a table's
	// in-memory logical rows (openRowStoreCursor, vdbe_write.go) rather than
	// over a b-tree. Its rows are built on the first rewind and not before --
	// see rewind, and materializeRowStore for why that matters.
	rowStore *tableMeta
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
	colMask columnMask

	// seekConfigured/seekKey turn the first rewind() into a rowid point lookup
	// (SeekRowidSegments) instead of a full scan. Set by OpSeekRowidHint only for
	// a runtime-Integer key; the WHERE conjunct is still evaluated on the fetched
	// row, so the seek only narrows the candidate set and cannot change the
	// result. A non-Integer key leaves it unset and the full scan runs.
	seekConfigured bool
	seekKey        int64

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
	// idxSeekCol is the TABLE column the index leads on.
	idxSeekCol int

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
	// pointSrc is the source a rowid point seek positions (segPointSeek),
	// held here so a seek per outer row allocates none.
	pointSrc         segRowSource
	segCur           *segment
	segRow           int
	segFilter        *segRowFilter // the compiled pre-filter from OpRewind's P4
	segWidth         int           // segCur.Width(segRow), read once per row
	segSrcCols       []segColPlan  // segSrc.cols, hoisted out of the read path
	streamErrPending error

	// sub, when non-nil, makes this a DERIVED-table cursor whose rows come from
	// a co-routine (openStreamCursor, vdbe_stream.go) rather than a
	// materialized row list: each advance resumes the source program for one
	// row. Its errors travel through streamErrPending like stream's.
	sub *subStream

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

// hasGeneratedCols reports whether this cursor's table declares any generated
// column, memoized in genCached (see its doc comment).
func (cur *vdbeCursor) hasGeneratedCols() bool {
	if cur.genCached == 0 {
		if hasGeneratedCols(cur.tbl.cols) {
			cur.genCached = 1
		} else {
			cur.genCached = 2
		}
	}
	return cur.genCached == 1
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
	return &vdbeCursor{pager: p, tbl: tbl, colMask: allColumns}
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
	return nil
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
	}
	return openMaterializedCursor(tbl, rows, nil)
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
	rids := make([]uint64, len(rows))
	for i := range rids {
		if i < len(rowids) {
			rids[i] = uint64(rowids[i])
		}
	}
	// The JSON subtype is deliberately NOT cleared here -- see openDerivedCursor
	// above, which clears it for its own rows before delegating.
	return &vdbeCursor{
		tbl:          tbl,
		rows:         rows,
		rowids:       rids,
		materialized: true,
		colMask:      allColumns,
	}
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
// startSegSource makes src the cursor's row source: the lazy segment scan.
func (cur *vdbeCursor) startSegSource(src *segRowSource) {
	// The compiled pre-filter, when OpRewind carried one. Purely a
	// candidate-set restriction: the WHERE it was lowered from is still in the
	// program and still runs on every row it selects.
	src.filter = cur.segFilter
	src.params = cur.params
	src.ipkCol = cur.tbl.ipkIndex
	cur.segSrc = src
	cur.segSrcCols = src.cols
	cur.segCur = nil
	cur.streamErrPending = nil
	cur.pos = -1
	cur.rowidNull = false
}

func (cur *vdbeCursor) rewind() error {
	if cur.sub != nil {
		return cur.rewindStream()
	}
	// A columnar view of this table, when one is attached, is served through
	// ScanTable (segment_read.go) -- so the streaming b-tree cursor is declined
	// for it. Without this the two would SPLIT: a streamable scan would read
	// the b-tree while every other read of the same table read segments. Both
	// are correct, which is exactly what makes the split dangerous: it would
	// make a "columnar" measurement quietly a b-tree one.
	if cur.streamable && !cur.seekConfigured && !cur.idxSeekConfigured && !cur.tbl.withoutRowid {
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
			if cur.seekConfigured && !tbl.withoutRowid && tbl.rows != nil {
				rid := uint64(cur.seekKey)
				cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
				if vals, found := tbl.rows.get(rid); found {
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
			// Positioned, not built, when the segments are the whole answer:
			// columns are read one at a time as the program asks for them.
			// Not marked materialized, so the next rewind seeks again.
			if src, served := cur.pager.segPointSeek(cur, cur.seekKey, &cur.pointSrc); served {
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
		// ...and the SECONDARY-INDEX equality seek, from the leading column's
		// own posting list. It only PRUNES -- the keyed conjunct is re-checked
		// on every row -- so a rowid the list carries that no longer exists is
		// dropped by the row fetch below.
		// A VIRTUAL generated column is not stored: a row written before
		// "ALTER TABLE ADD COLUMN ... AS (expr)" holds NULL in its slot, which
		// the posting list files as matching nothing, so the seek lost it
		// (C's CREATE INDEX computes the key from the expression). Scanned.
		virtualKey := cur.idxSeekCol >= 0 && cur.idxSeekCol < len(cur.tbl.cols) &&
			cur.tbl.cols[cur.idxSeekCol].IsGenerated() && !cur.tbl.cols[cur.idxSeekCol].GeneratedStored
		if cur.idxSeekConfigured && !cur.tbl.withoutRowid && !virtualKey {
			rowids, served := cur.pager.SeekIndexRowidsSegments(cur.tbl.root, cur.idxSeekCol, cur.idxSeekProbe, cur.idxSeekColl)
			if served {
				segIndexSeeksServed++
				cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
				shared := cur.pager.ServesLiveRows(cur.tbl.root)
				for _, rid := range rowids {
					raw, found, ok := cur.pager.SeekRowidSegments(cur.tbl.root, rid)
					if !ok || !found {
						continue
					}
					cur.rowids = append(cur.rowids, uint64(rid))
					cur.rows = append(cur.rows, cur.normalizeFrom(shared, uint64(rid), raw))
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
		cur.seekConfigured, cur.idxSeekConfigured = false, false
		if cur.streamable && !cur.tbl.withoutRowid {
			if src := cur.pager.newSegRowSource(cur); src != nil {
				cur.startSegSource(src)
				return nil
			}
		}
		seq, errFn := cur.pager.ScanTable(cur.tbl.root)
		shared := cur.pager.ServesLiveRows(cur.tbl.root)
		for rowid, vals := range seq {
			cur.rowids = append(cur.rowids, rowid)
			cur.rows = append(cur.rows, cur.normalizeFrom(shared, rowid, vals))
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
	at := func(i, kc int) Value {
		if kc < 0 {
			return Value{Typ: Int, I: int64(cur.rowids[i])}
		}
		if row := cur.rows[i]; kc < len(row) {
			return row[kc]
		}
		return Value{Typ: Null}
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
		for k, kc := range key.cols {
			c := compareValuesCollatedEnc(at(x, kc), at(y, kc), key.colls[k], enc)
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
	at := func(i, kc int) Value {
		if kc < 0 {
			return Value{Typ: Int, I: int64(cur.rowids[i])}
		}
		if row := cur.rows[i]; kc < len(row) {
			return row[kc]
		}
		return Value{Typ: Null}
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
		for k, kc := range key.cols {
			c := compareValuesCollatedEnc(at(x, kc), at(y, kc), key.colls[k], enc)
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
	cur.rowVals = make([]Value, len(cur.tbl.cols))
	cur.rowRaw = nil // an all-NULL row is complete by construction
	cur.rowidNull = true
}

// close releases the cursor's materialized rows (and drops any streaming
// b-tree cursor, which owns no goroutine). Safe to call more than once, and
// safe even if rewind was never called.
func (cur *vdbeCursor) close() {
	// Same discipline for the segment row: segCur is what tells readColumn to
	// go to the segment instead of rowVals, so a closed cursor must not keep
	// pointing at a row it no longer scans.
	cur.segSrc = nil
	cur.segCur = nil
	cur.sub = nil
	cur.rowids = nil
	cur.rows = nil
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
	if cur.matched == nil {
		cur.matched = make([]bool, len(cur.rows))
	}
	if cur.pos >= 0 && cur.pos < len(cur.matched) {
		cur.matched[cur.pos] = true
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
func (cur *vdbeCursor) sweepUnmatchedFrom(start int) bool {
	if cur.matched == nil {
		cur.matched = make([]bool, len(cur.rows))
	}
	for i := start; i < len(cur.rows); i++ {
		if !cur.matched[i] {
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
	if f, st := cur.segFilter, tbl.rows; f != nil && st != nil && st.seg != nil {
		// A write scan the filter peephole judged safe to restrict
		// (segWriteFilterPeephole): only the base rows that can match, and
		// every row the overlay holds, are decoded. Sized for the overlay, which
		// arrives whole, plus a margin -- not n: the point is that most of the
		// table never arrives.
		rowids = make([]uint64, 0, len(st.m)+n/8)
		rows = make([][]Value, 0, len(st.m)+n/8)
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
	genCols := hasGeneratedCols(tbl.cols)
	canNormalize := tbl.ipkIndex >= 0
	if !canNormalize {
		for _, c := range tbl.cols {
			if c.Aff == affReal {
				canNormalize = true
				break
			}
		}
	}

	// The store's rows are shared and must stay untouched, so the fix-up is
	// DEFERRED to the moment each row becomes current (lazyNorm, advance): the
	// frozen slice headers are all the Halloween barrier needs, and a stored row
	// is never edited in place. This used to copy every row of the table into an
	// arena up front -- 33% of every byte a 10,000-row UPDATE over 100,000 rows
	// allocated, for an IPK slot a column read could answer from the rowid.
	lazy := !genCols && canNormalize
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
