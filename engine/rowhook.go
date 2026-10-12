package engine

import "slices"

// Package engine records row changes into an in-session log if capture is
// enabled. The log is drained by TakeRowChanges before commit. A rollback
// truncates the log, so it only contains changes that are actually committed.
// Changes made by trigger bodies are not recorded.

// RowChangeKind is the kind of mutation a RowChange describes.
type RowChangeKind int

// The three row mutations recorded in the change log.
const (
	RowInsert RowChangeKind = iota + 1
	RowUpdate
	RowDelete
	// RowSchema is a DDL statement that ran on main, in its place among the row
	// changes; SQL is its text. Recorded under full capture only (journalDDL).
	RowSchema
)

// RowChange is one logical row mutation. Old is nil for an insert, New is nil
// for a delete; both are set for an update. Under full capture
// (EnableRowChangeCapture) both are fully-owned copies (the engine's own row
// values may alias a mapped page), so a consumer may retain them.
type RowChange struct {
	Table string
	Cols  []string // column names, ordinal order -- indexes Old/New
	Kind  RowChangeKind
	Rowid int64
	Old   []Value
	New   []Value
	SQL   string // RowSchema only

	// movedFrom is the rowid an UPDATE moved this row from (when moved is true).
	movedFrom int64
	moved     bool

	// Temp says the table is in the TEMP catalog rather than main (needed to
	// uniquely identify the table, as both catalogs may have tables with the same name).
	Temp bool
}

// EnableRowChangeCapture turns full row-change recording on: every mutation
// with old and new images and column names. Off by default.
func (db *DB) EnableRowChangeCapture() { db.captureChanges, db.captureFull = true, true }

// RowChangeCaptureEnabled reports whether full capture is enabled.
func (db *DB) RowChangeCaptureEnabled() bool { return db.captureFull }

// enableDeltaRowCapture turns on minimal capture (delta-only).
func (db *DB) enableDeltaRowCapture() { db.captureChanges = true }

// PeekRowChanges returns the changes recorded so far without clearing them.
//
// The slice is a copy, in one piece; the log itself is chunked (changelog.go).
func (db *DB) PeekRowChanges() []RowChange { return db.changeLog.flat(0) }

// TakeRowChanges returns and clears the changes recorded so far.
func (db *DB) TakeRowChanges() []RowChange {
	out := db.changeLog.flat(0)
	db.changeLog.reset()
	return out
}

// takeChangeLog hands the log to a commit, which reads it with all() and gives
// it back with recycleChangeLog. The session starts a fresh, empty one.
func (db *DB) takeChangeLog() *changeLog {
	l := new(changeLog)
	*l, db.changeLog = db.changeLog, changeLog{}
	return l
}

// recycleChangeLog gives a log taken by takeChangeLog back as the session's
// empty one, cleared so it pins no row. Only when nothing has been logged since
// and capture is delta-only: under full capture a consumer may still hold the
// changes. Its chunks are kept (changeLog.reset), so a transaction of up to
// 65,536 rows logs without allocating.
func (db *DB) recycleChangeLog(log *changeLog) {
	if db.captureFull || db.changeLog.n != 0 {
		return
	}
	log.reset()
	db.changeLog = *log
}

// changeMark returns a point in the change log to truncate back to (see
// truncateChanges), for a caller about to apply rows it may later undo.
func (db *DB) changeMark() int { return db.changeLog.len() }

// truncateChanges drops every change recorded since mark.
func (db *DB) truncateChanges(mark int) {
	db.changeLog.truncate(mark)
}

// noteRowChange records one mutation, if capture is on.
func (db *DB) noteRowChange(tbl *tableMeta, kind RowChangeKind, rowid uint64, old, new []Value) {
	if !db.captureChanges {
		return
	}
	// Old and Cols are only filled under full capture; under delta-only, Old is nil.
	var cols []string
	var logged []Value
	if db.captureFull {
		cols = make([]string, len(tbl.cols))
		for i, c := range tbl.cols {
			cols[i] = c.Name
		}
		old, logged = copyHookRow(tbl, rowid, old), copyHookRow(tbl, rowid, new)
	} else {
		// Delta-only: New is the stored row; nil if table has spilled.
		old, logged = nil, new
		if tbl.rows != nil && tbl.rows.spill != nil {
			logged = nil
		}
	}
	db.changeLog.push(RowChange{
		Table: tbl.name,
		Temp:  tbl.isTemp,
		Cols:  cols,
		Kind:  kind,
		Rowid: int64(rowid),
		Old:   old,
		New:   logged,
	})
}

// journalDDL records a DDL statement that succeeded. It is ordered against row
// changes and truncated on rollback. Inserted at mark so consumers know the
// table before its rows.
func (db *DB) journalDDL(sqlText string, mark int) {
	if !db.captureChanges || !db.captureFull {
		return
	}
	if _, ddl := StatementTargetSchema(sqlText); !ddl {
		return
	}
	// Rare (a DDL statement under full capture), so the log is rebuilt around
	// the new entry rather than made to support insertion.
	mark = min(mark, db.changeLog.len())
	flat := slices.Insert(db.changeLog.flat(0), mark, RowChange{Kind: RowSchema, SQL: sqlText})
	db.changeLog.reset()
	for _, c := range flat {
		db.changeLog.push(c)
	}
}

// MovedFrom is the rowid an UPDATE moved this row off, and whether it moved.
func (c RowChange) MovedFrom() (int64, bool) { return c.movedFrom, c.moved }

// noteRowUpdate is noteRowChange for an UPDATE with possible rowid change.
func (db *DB) noteRowUpdate(tbl *tableMeta, oldRowid, newRowid uint64, old, new []Value) {
	db.noteRowChange(tbl, RowUpdate, newRowid, old, new)
	if !db.captureChanges || oldRowid == newRowid {
		return
	}
	c := db.changeLog.last()
	c.movedFrom, c.moved = int64(oldRowid), true
	if db.captureFull {
		c.Old = copyHookRow(tbl, oldRowid, old) // its IPK is the OLD rowid
	}
}

// copyHookRow deep-copies a row, filling the rowid into the IPK column.
func copyHookRow(tbl *tableMeta, rowid uint64, vals []Value) []Value {
	if vals == nil {
		return nil
	}
	out := make([]Value, len(vals))
	copy(out, vals)
	for i := range out {
		if out[i].S != nil {
			out[i].S = append([]byte(nil), out[i].S...)
		}
	}
	if len(out) == len(tbl.cols) {
		normalizeRowInto(out, tbl.cols, tbl.ipkIndex, rowid)
	}
	return out
}

// HasUncommittedChanges reports whether this session has uncommitted changes.
func (db *DB) HasUncommittedChanges() bool { return db.changeLog.len() > 0 }
