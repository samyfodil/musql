package engine

import (
	"slices"
	"sync/atomic"
)

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

// THE SAME LOAD INSIDE A TRANSACTION. An application loading a table opens a
// transaction and runs one prepared "INSERT ... VALUES (?, ?)" per row, and none
// of the conditions above held: the rows went through the row store, the change
// log and the spill file, and were built into segments again at COMMIT -- 2x C
// SQLite's time for 10M rows. A load armed inside a transaction lives across its
// statements (inTx), under three more rules:
//
//   - only an INSERT the load can take touches the table while it is armed:
//     every other statement, every read pager and every snapshot first DRAINS
//     the load into the row store as ordinary logged inserts (drainTxBulks), so
//     nothing ever reads the table without its rows;
//   - a statement that fails keeps none of ITS rows and all of the earlier
//     statements' (truncateTo), and a transaction or savepoint rollback, which
//     restores a snapshot taken before the load was armed (captureSnapshot
//     drains first), drops it whole (restoreSnapshot);
//   - COMMIT rewrites the file from the built segments, as above.

// segBulkLoad is a table's direct-path load in progress.
type segBulkLoad struct {
	cut  segCutter
	segs [][]byte // finished segments, in rowid order
	last uint64
	any  bool
	// inTx: armed inside a transaction, so it outlives its statement and is on
	// db.txBulks.
	inTx bool
	// rows is how many rows it holds: what a statement's mark counts.
	rows int
	db   *DB // for inTx's undo, which a journal entry runs without one
	// segBytes is the built segments' size, which a load inside a transaction
	// holds in memory until COMMIT: past txBulkMaxBytes it hands its rows to
	// the row store, whose spill keeps a transaction's memory bounded.
	segBytes int
	// stream, when set, is an append load into a table that has rows: its
	// rows stream into the segment the commit writes as one delta record
	// (insert_append_delta.go), and cut is unused.
	stream *segStream
}

// txBulkMaxBytes bounds the segments a transaction's load holds in memory. A
// variable so a test can make every load overflow.
var txBulkMaxBytes = 1 << 30

// SetTxBulkMaxBytesForTest sets txBulkMaxBytes and returns the old value.
func SetTxBulkMaxBytesForTest(n int) int { old := txBulkMaxBytes; txBulkMaxBytes = n; return old }

// txAppendArmable reports whether plan may write through a load armed inside a
// transaction: an INSERT ... SELECT the autocommit load takes, or an INSERT ...
// VALUES that does nothing but build and store its rows.
func (p *insertPlan) txAppendArmable() bool { return p.appendArmable || p.valuesArmable }

// bulkTxOK reports whether the session lets a load into tbl live inside the
// transaction now open: the per-row checks the load skips must have nothing to
// check, as for the autocommit load.
func bulkTxOK(db *DB, tbl *tableMeta) bool {
	if !db.commitsEachStatement || !db.inTransaction() || db.fkEnforce || db.captureFull ||
		tbl.isTemp || tbl.withoutRowid || tbl.aliasOf != "" || tbl.rows == nil {
		return false
	}
	for _, idx := range db.indexes {
		if indexBelongsTo(idx, tbl) && (idx.unique || idx.exprOrPartial) {
			return false
		}
	}
	return true
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
	if db.inTransaction() {
		return m.bulkLoadInTx(plan)
	}
	if !plan.appendArmable || !db.commitsEachStatement || db.fkEnforce || db.captureFull ||
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
	}, own: true}
	tbl.bulk, wc.bulkTbl = bl, tbl
	wc.undoFn(func() { tbl.bulk = nil }) // a failed statement keeps none of it
	return bl
}

// bulkLoadInTx is bulkLoadFor inside a transaction: the table's load if it has
// one, or a new one for an empty table, with this statement's undo set to
// truncate it back to where the statement found it.
func (m *vdbe) bulkLoadInTx(plan *insertPlan) *segBulkLoad {
	wc := m.wctx
	db, tbl := wc.db, plan.tbl
	// A table with rows and no load is the ordinary path whatever else holds:
	// asked first, because a batch INSERT into a loaded table asks once a row
	// and bulkTxOK walks every index.
	if !plan.txAppendArmable() {
		return nil
	}
	bl := tbl.bulk
	switch {
	case bl != nil && !bl.inTx:
		return nil
	case bl == nil && tbl.rows != nil && !tbl.rows.isEmpty():
		if !bulkTxOK(db, tbl) {
			return nil
		}
		bl = db.armAppendLoad(tbl)
		if bl == nil {
			return nil
		}
	case bl == nil:
		if !bulkTxOK(db, tbl) {
			return nil
		}
		if !tbl.rows.isEmpty() {
			return nil
		}
		bl = &segBulkLoad{inTx: true, db: db}
		cols := make([]columnInfo, len(tbl.cols))
		copy(cols, tbl.cols)
		bl.cut = segCutter{cols: cols, what: tbl.name, emit: func(raw []byte) error {
			bl.segs = append(bl.segs, raw)
			bl.segBytes += len(raw)
			return nil
		}, async: true, own: true}
		tbl.bulk = bl
		db.txBulks = append(db.txBulks, tbl)
	}
	wc.bulkTbl, wc.bulkMark = tbl, bl.rows
	wc.journalAppend(undoEntry{bulk: bl, tbl: tbl, bulkTo: bl.rows})
	return bl
}

// truncateTo undoes a failed statement's rows: the load keeps its first n. A
// load drained since holds nothing to undo -- the drain journalled the
// statement's rows as ordinary inserts.
func (bl *segBulkLoad) truncateTo(tbl *tableMeta, n int) {
	db := bl.db
	if tbl.bulk != bl || bl.rows <= n {
		return
	}
	if bl.stream != nil {
		bl.stream.truncate(n)
		bl.rows = n
		if n == 0 {
			db.dropTxBulk(tbl)
			return
		}
		bl.last = bl.stream.rowids[n-1]
		return
	}
	if err := bl.cut.wait(); err != nil { // the segment in flight is rows we keep or cut
		db.pendingLoadErr = err
		return
	}
	if n == 0 {
		db.dropTxBulk(tbl)
		return
	}
	if drop := bl.rows - n; drop < len(bl.cut.rows) {
		k := len(bl.cut.rows) - drop
		clear(bl.cut.rows[k:])
		bl.cut.rowids, bl.cut.rows = bl.cut.rowids[:k], bl.cut.rows[:k]
		bl.cut.bytes = 0
		for _, r := range bl.cut.rows {
			bl.cut.bytes += rowMemSize(r)
		}
		bl.rows = n
		bl.last = bl.cut.rowids[k-1]
		return
	}
	// The statement's rows reach into a built segment: rebuild from the rows
	// before it. Only a failed multi-row statement gets here.
	var rowids []uint64
	var rows [][]Value
	bl.eachRow(func(rid uint64, vals []Value) bool {
		if len(rows) == n {
			return false
		}
		rowids, rows = append(rowids, rid), append(rows, vals)
		return true
	})
	bl.segs, bl.rows, bl.segBytes = nil, 0, 0
	clear(bl.cut.rows)
	bl.cut.rowids, bl.cut.rows, bl.cut.bytes = bl.cut.rowids[:0], bl.cut.rows[:0], 0
	bl.cut.flat = nil // rows may still point into it: the re-added ones get storage of their own
	for i, rid := range rowids {
		// A cut here only builds a segment of rows already validated once;
		// it can fail only as the first build could have.
		if err := bl.cut.add(rid, rows[i]); err != nil {
			db.pendingLoadErr = err
			return
		}
		bl.rows++
	}
	bl.last = rowids[len(rowids)-1]
}

// eachRow walks the load's rows in order, built segments first, until fn
// returns false.
func (bl *segBulkLoad) eachRow(fn func(rid uint64, vals []Value) bool) {
	if bl.stream != nil {
		bl.stream.eachRow(fn)
		return
	}
	if err := bl.cut.wait(); err != nil && bl.db != nil {
		bl.db.pendingLoadErr = err
	}
	for _, raw := range bl.segs {
		s, err := openSegment(raw)
		if err != nil {
			continue // built by this load; openSegment cannot refuse it
		}
		for i := 0; i < s.nRows; i++ {
			if !fn(s.Rowid(i), s.rowAt(i)) {
				return
			}
		}
	}
	for i, rid := range bl.cut.rowids {
		if !fn(rid, bl.cut.rows[i]) {
			return
		}
	}
}

// txBulkRows counts rows a transaction's load took, so a test can tell the
// path ran.
var txBulkRows atomic.Int64

// TxBulkRowsForTest reads and clears txBulkRows.
func TxBulkRowsForTest() int64 { return txBulkRows.Swap(0) }

// txAppendTarget is the table this program appends to through a transaction's
// direct-path load -- an INSERT that only builds and stores rows -- or nil.
// commit is a COMMIT, which reads no rows and leaves every load to the file
// rewrite that follows it.
func (prog *Program) txAppendTarget() (tbl *tableMeta, commit bool) {
	// Asked before every statement while a load is armed, so the answer is
	// kept, against the instructions it was taken from (a Program copied by
	// value may carry others).
	if c := prog.txAppend; c != nil && len(prog.Insns) > 0 && c.first == &prog.Insns[0] {
		return c.tbl, c.commit
	}
	tbl, commit = prog.txAppendTargetOf()
	if len(prog.Insns) > 0 {
		prog.txAppend = &txAppendVerdict{first: &prog.Insns[0], tbl: tbl, commit: commit}
	}
	return tbl, commit
}

// txAppendVerdict is txAppendTarget's kept answer for the instructions
// starting at first.
type txAppendVerdict struct {
	first  *Instruction
	tbl    *tableMeta
	commit bool
}

func (prog *Program) txAppendTargetOf() (tbl *tableMeta, commit bool) {
	if len(prog.Insns) <= 3 {
		for i := range prog.Insns {
			if in := &prog.Insns[i]; in.Op == OpTxn {
				if p, ok := in.P4.(*txnPlan); ok && p.verb == txnCommit {
					return nil, true
				}
			}
		}
	}
	for i := range prog.Insns {
		if in := &prog.Insns[i]; in.Op == OpInsert {
			p, ok := in.P4.(*insertPlan)
			if !ok || !p.txAppendArmable() || (tbl != nil && p.tbl != tbl) {
				return nil, false
			}
			tbl = p.tbl
		}
	}
	return tbl, false
}

// dropTxBulk forgets tbl's transaction load and its rows.
func (db *DB) dropTxBulk(tbl *tableMeta) {
	tbl.bulk = nil
	if i := slices.Index(db.txBulks, tbl); i >= 0 {
		db.txBulks = slices.Delete(db.txBulks, i, i+1)
	}
}

// dropTxBulks forgets every transaction load: a rollback to a snapshot taken
// before any of them was armed.
func (db *DB) dropTxBulks() {
	for _, t := range db.txBulks {
		t.bulk = nil
	}
	db.txBulks = db.txBulks[:0]
}

// drainTxBulks moves every transaction load but keep's into its row store, as
// the ordinary inserts it stands for: logged for the commit and the
// transaction's undo, and -- for the rows the running statement wrote --
// journalled to that statement, so a failure still takes them back. A drain
// that cannot complete leaves the error for the statement (pendingLoadErr).
func (db *DB) drainTxBulks(keep *tableMeta) {
	if len(db.txBulks) == 0 || (len(db.txBulks) == 1 && db.txBulks[0] == keep) {
		return
	}
	for _, t := range slices.Clone(db.txBulks) {
		if t == keep {
			continue
		}
		bl := t.bulk
		db.dropTxBulk(t)
		if bl == nil {
			continue
		}
		wc := db.activeWC
		mark := bl.rows
		if wc != nil && wc.bulkTbl == t {
			mark = wc.bulkMark
			wc.bulkTbl = nil
		}
		k := 0
		bl.eachRow(func(rid uint64, vals []Value) bool {
			t.putRow(rid, vals)
			db.noteRowChange(t, RowInsert, rid, nil, vals)
			if k >= mark {
				wc.undoDrop(t, rid)
			}
			k++
			return true
		})
	}
}

// bulkStore takes one row into the direct-path load, or reports false when the
// row breaks the ascending order -- after moving the built rows into the row
// store, so the caller stores this one the ordinary way.
func (m *vdbe) bulkStore(bl *segBulkLoad, tbl *tableMeta, rowid uint64, full []Value) (bool, error) {
	if bl.any && !rowidLess(bl.last, rowid) {
		if bl.inTx {
			// Out of order: the load's rows go to the row store, the earlier
			// statements' as theirs and this statement's journalled to it.
			m.wctx.db.drainTxBulks(nil)
			return false, nil
		}
		return false, m.bulkDrain(bl, tbl)
	}
	if tbl.ipkIndex >= 0 {
		full[tbl.ipkIndex] = Value{Typ: Null} // stored NULL; the rowid carries the value
	}
	if bl.stream != nil {
		if !bl.stream.add(rowid, full) {
			// Not full width: the load's rows go to the row store and this one
			// the ordinary way, as for a row out of order.
			m.wctx.db.drainTxBulks(nil)
			return false, nil
		}
	} else if err := bl.cut.add(rowid, full); err != nil {
		return false, err
	}
	bl.last, bl.any = rowid, true
	bl.rows++
	// As storeRow does: last_insert_rowid() is the last row this INSERT
	// stored, whichever way it was stored. The load never set it, so an
	// autocommit INSERT ... SELECT into an empty table left it unchanged.
	m.wctx.db.lastInsertRowid = int64(rowid)
	if bl.inTx {
		txBulkRows.Add(1)
		if bl.stream != nil {
			bl.segBytes = bl.stream.memBytes()
		}
		if bl.segBytes > txBulkMaxBytes {
			// Too big to hold until COMMIT: the rows go to the row store, this
			// statement's journalled to it, and the statement carries on there.
			m.wctx.db.drainTxBulks(nil)
		}
	}
	tbl.rowsWrittenSinceCommit = true
	return true, nil
}

// bulkDrain ends a direct-path load early: every row built so far goes into
// the row store as an ordinary logged, journalled insert.
func (m *vdbe) bulkDrain(bl *segBulkLoad, tbl *tableMeta) error {
	if bl.inTx {
		m.wctx.db.drainTxBulks(nil)
		return nil
	}
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
	if err := t.bulk.cut.finish(); err != nil {
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
