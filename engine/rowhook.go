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
func (db *DB) PeekRowChanges() []RowChange { return db.changeLog }

// TakeRowChanges returns and clears the changes recorded so far.
func (db *DB) TakeRowChanges() []RowChange {
	out := db.changeLog
	db.changeLog = nil
	return out
}

// changeMark returns a point in the change log to truncate back to (see
// truncateChanges), for a caller about to apply rows it may later undo.
func (db *DB) changeMark() int { return len(db.changeLog) }

// truncateChanges drops every change recorded since mark.
func (db *DB) truncateChanges(mark int) {
	if mark <= len(db.changeLog) {
		db.changeLog = db.changeLog[:mark]
	}
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
	// Grow changeLog by doubling to reduce allocations.
	if len(db.changeLog) == cap(db.changeLog) {
		n := 2 * cap(db.changeLog)
		if n == 0 {
			n = 8
		}
		db.changeLog = append(make([]RowChange, 0, n), db.changeLog...)
	}
	db.changeLog = append(db.changeLog, RowChange{
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
	mark = min(mark, len(db.changeLog))
	db.changeLog = slices.Insert(db.changeLog, mark, RowChange{Kind: RowSchema, SQL: sqlText})
}

// MovedFrom is the rowid an UPDATE moved this row off, and whether it moved.
func (c RowChange) MovedFrom() (int64, bool) { return c.movedFrom, c.moved }

// noteRowUpdate is noteRowChange for an UPDATE with possible rowid change.
func (db *DB) noteRowUpdate(tbl *tableMeta, oldRowid, newRowid uint64, old, new []Value) {
	db.noteRowChange(tbl, RowUpdate, newRowid, old, new)
	if !db.captureChanges || oldRowid == newRowid {
		return
	}
	c := &db.changeLog[len(db.changeLog)-1]
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
func (db *DB) HasUncommittedChanges() bool { return len(db.changeLog) > 0 }
