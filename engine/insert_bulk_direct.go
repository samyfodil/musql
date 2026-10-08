package engine

// DIRECT-PATH BULK LOAD: an autocommit INSERT ... SELECT into an empty table
// builds the table's segments as its rows arrive, rather than storing each row
// in the row store, logging it, journalling it, and building the segments from
// all that again at commit -- Postgres's COPY, Oracle's direct-path insert.
//
// The row store holds a row as a []Value and the change log as another: a
// pointer per column per row that the garbage collector scans for as long as
// the load runs, which in a browser, with no parallel marking, was half of a
// 100k-row load's time. Here a row lives only until its segment is cut (one
// segment's worth at a time), and what is kept is the segment's bytes.
//
// It arms only where the shortcut cannot change an answer:
//
//   - the caller commits right after the statement (SetCommitsEachStatement),
//     outside a transaction, so nothing reads the table before the rewrite
//     that writes these segments out;
//   - the INSERT's program only builds and stores rows from its source
//     (insertPlan.appendArmable), so the statement does not read its target;
//   - the table is empty, an ordinary rowid table, with no UNIQUE, expression
//     or partial index, while foreign keys are off and no full change capture
//     is on: nothing per-row checks a stored row against another;
//   - rowids arrive strictly ascending, which with an empty table also rules
//     out a rowid conflict. The first that does not moves the rows built so
//     far into the row store, logged as ordinary inserts, and the statement
//     carries on the ordinary way.
//
// The commit sees a table written with no change record (rowsWrittenSinceCommit,
// mutatedOutsideChangeLog), so it rewrites the file, and the rewrite takes the
// built segments as they are (segmentFileContentsOf).

// segBulkLoad is a table's direct-path load in progress.
type segBulkLoad struct {
	cut  segCutter
	segs [][]byte // finished segments, in rowid order
	last uint64
	any  bool
}

// SetCommitsEachStatement tells the engine its caller commits after every
// autocommit statement, as the driver does: what lets an INSERT into an empty
// table build its segments directly (insert_bulk_direct.go). An engine-direct
// session batching statements until Close leaves it off.
func (db *DB) SetCommitsEachStatement(on bool) { db.commitsEachStatement = on }

// bulkLoadFor returns the direct-path load plan's INSERT writes through, arming
// it on the statement's first row when it can; nil for the ordinary path.
func (m *vdbe) bulkLoadFor(plan *insertPlan) *segBulkLoad {
	wc := m.wctx
	tbl := plan.tbl
	if wc.bulkChecked {
		if wc.bulkTbl == tbl {
			return tbl.bulk
		}
		return nil
	}
	wc.bulkChecked = true
	db := wc.db
	if !plan.appendArmable || !db.commitsEachStatement || db.inTransaction() || db.fkEnforce || db.captureFull ||
		tbl.isTemp || tbl.withoutRowid || tbl.aliasOf != "" || tbl.rows == nil || tbl.bulk != nil || tbl.rows.len() != 0 {
		return nil
	}
	for _, idx := range db.indexes {
		if indexBelongsTo(idx, tbl) && (idx.unique || idx.exprOrPartial) {
			return nil
		}
	}
	bl := &segBulkLoad{}
	cols := make([]columnInfo, len(tbl.cols))
	copy(cols, tbl.cols)
	bl.cut = segCutter{cols: cols, what: tbl.name, emit: func(raw []byte) error {
		bl.segs = append(bl.segs, raw)
		return nil
	}}
	tbl.bulk, wc.bulkTbl = bl, tbl
	wc.undoFn(func() { tbl.bulk = nil }) // a failed statement keeps none of it
	return bl
}

// bulkStore takes one row into the direct-path load, or reports false when the
// row breaks the ascending order -- after moving the built rows into the row
// store, so the caller stores this one the ordinary way.
func (m *vdbe) bulkStore(bl *segBulkLoad, tbl *tableMeta, rowid uint64, full []Value) (bool, error) {
	if bl.any && !rowidLess(bl.last, rowid) {
		return false, m.bulkDrain(bl, tbl)
	}
	if tbl.ipkIndex >= 0 {
		full[tbl.ipkIndex] = Value{Typ: Null} // stored NULL; the rowid carries the value
	}
	if err := bl.cut.add(rowid, full); err != nil {
		return false, err
	}
	bl.last, bl.any = rowid, true
	tbl.rowsWrittenSinceCommit = true
	return true, nil
}

// bulkDrain ends a direct-path load early: every row built so far goes into
// the row store as an ordinary logged, journalled insert.
func (m *vdbe) bulkDrain(bl *segBulkLoad, tbl *tableMeta) error {
	tbl.bulk, m.wctx.bulkTbl = nil, nil
	if err := bl.cut.flush(); err != nil {
		return err
	}
	for _, raw := range bl.segs {
		s, err := openSegment(raw)
		if err != nil {
			return err
		}
		for i := 0; i < s.nRows; i++ {
			rid, vals := s.Rowid(i), s.rowAt(i)
			tbl.putRow(rid, vals)
			m.wctx.db.noteRowChange(tbl, RowInsert, rid, nil, vals)
			m.wctx.undoDrop(tbl, rid)
		}
	}
	return nil
}

// bulkSegments finishes t's direct-path load for the file rewrite: its
// segments, in rowid order.
func (t *tableMeta) bulkSegments() ([][]byte, error) {
	if err := t.bulk.cut.flush(); err != nil {
		return nil, err
	}
	return t.bulk.segs, nil
}

// bulkIntoRowStore puts t's direct-path rows into its row store as rows the
// file already holds: not logged, not journalled, not written-since-commit.
// For a rewrite that wrote them but left the session reading its row stores.
func (t *tableMeta) bulkIntoRowStore() error {
	written := t.rowsWrittenSinceCommit
	for _, raw := range t.bulk.segs {
		s, err := openSegment(raw)
		if err != nil {
			return err
		}
		for i := 0; i < s.nRows; i++ {
			t.putRow(s.Rowid(i), s.rowAt(i))
		}
	}
	t.rowsWrittenSinceCommit = written
	return nil
}
