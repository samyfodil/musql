package engine

import (
	"strconv"
	"testing"
)

// TestJournalModeOffStatementPartialFailure verifies that journal_mode=off
// keeps rows already stored when a multi-row statement fails partway through,
// while a full ROLLBACK still undoes everything.
func TestJournalModeOffStatementPartialFailure(t *testing.T) {
	path := t.TempDir() + "/off.sqlite"
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()

	mustExec := func(sql string) {
		t.Helper()
		if e := db.Exec(sql); e != nil {
			t.Fatalf("%s: %v", sql, e)
		}
	}
	scalar := func(sql string) Value {
		t.Helper()
		pg, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		defer pg.Close()
		_, rows, qerr := pg.Query(sql)
		if qerr != nil {
			t.Fatalf("%s: %v", sql, qerr)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("%s: want one row of one column, got %v", sql, rows)
		}
		return rows[0][0]
	}

	mustExec(`CREATE TABLE t(a INTEGER PRIMARY KEY)`)
	mustExec(`CREATE TABLE src(a)`)
	mustExec(`INSERT INTO src VALUES(1),(2),(3),(4),(5)`)
	mustExec(`INSERT INTO t VALUES(3)`)
	mustExec(`PRAGMA journal_mode=off`)
	mustExec(`BEGIN`)

	if e := db.Exec(`INSERT INTO t SELECT a FROM src`); e == nil {
		t.Fatal(`INSERT INTO t SELECT a FROM src: want a UNIQUE violation at a=3, got nil`)
	}

	// changes()/total_changes(): 0 and 6 -- the failed statement's own count
	// is reset (C SQLite's halt path does this unconditionally), even
	// though its rows physically survive; total_changes() never moved for it
	// either (6 is CREATE+5 src rows... wait, INSERT INTO src is 5 changes
	// and INSERT INTO t VALUES(3) is 1, so 6 before the failing statement).
	if got := scalar(`SELECT changes()`); got.I != 0 {
		t.Errorf("changes() after the partial failure = %d, want 0", got.I)
	}
	if got := scalar(`SELECT total_changes()`); got.I != 6 {
		t.Errorf("total_changes() after the partial failure = %d, want 6 (unmoved by the failed statement)", got.I)
	}
	// count(*): 3 -- the pre-existing row (a=3) plus a=1 and a=2, which were
	// stored before the statement hit the UNIQUE violation at a=3 and were
	// NOT undone (off has no per-statement sub-journal to undo them with).
	if got := scalar(`SELECT count(*) FROM t`); got.I != 3 {
		t.Errorf("count(*) after the partial failure = %d, want 3 (a=1,2 kept alongside the pre-existing a=3)", got.I)
	}

	mustExec(`ROLLBACK`)

	// A full ROLLBACK, unlike a per-statement undo, still works exactly like
	// every other journal mode: it discards the whole transaction's dirty
	// state rather than replaying a per-page log, so it needs no sub-journal
	// at all. Back to the single pre-transaction row.
	if got := scalar(`SELECT count(*) FROM t`); got.I != 1 {
		t.Errorf("count(*) after ROLLBACK = %d, want 1 -- a full ROLLBACK must still undo everything under off", got.I)
	}
}

// TestJournalModeOffUpdateSelfCorrectsOwnRow pins the bug class the statement
// test above would NOT have caught by itself: opUpdateRow/opUpsertStore
// (vdbe_write.go) detect a UNIQUE violation with a POST-WRITE probe of the
// row just stored (checkUniqueIndexesForRow), unlike INSERT's PRE-write
// findRowConflicts check -- so without putting the CURRENT row back
// immediately on that scan's failure, skipping the statement-journal replay
// under off left the row that never actually passed its own conflict check
// sitting in the table too, alongside the earlier row it collided with:
// TWO rows sharing the same UNIQUE value, a genuinely corrupt state
// (verified this could build an image at all only by luck; integrity_check
// below is what actually catches it). Verified directly against the oracle:
// "UPDATE t SET b='dup' WHERE a IN (1,2,3)" over a UNIQUE column leaves ONLY
// the first row at 'dup'; the second row's own attempt never lands.
func TestJournalModeOffUpdateSelfCorrectsOwnRow(t *testing.T) {
	path := t.TempDir() + "/off.sqlite"
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()

	mustExec := func(sql string) {
		t.Helper()
		if e := db.Exec(sql); e != nil {
			t.Fatalf("%s: %v", sql, e)
		}
	}

	mustExec(`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`)
	mustExec(`INSERT INTO t VALUES(1,'x')`)
	mustExec(`INSERT INTO t VALUES(2,'y')`)
	mustExec(`INSERT INTO t VALUES(3,'z')`)
	mustExec(`PRAGMA journal_mode=off`)
	mustExec(`BEGIN`)

	if e := db.Exec(`UPDATE t SET b='dup' WHERE a IN (1,2,3)`); e == nil {
		t.Fatal(`UPDATE t SET b='dup' WHERE a IN (1,2,3): want a UNIQUE violation, got nil`)
	}

	pg, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	defer pg.Close()
	_, rows, qerr := pg.Query(`SELECT a,b FROM t ORDER BY a`)
	if qerr != nil {
		t.Fatalf("SELECT a,b FROM t: %v", qerr)
	}
	want := [][2]string{{"1", "dup"}, {"2", "y"}, {"3", "z"}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %v", rows, want)
	}
	for i, r := range rows {
		if got := [2]string{intOrStr(r[0]), intOrStr(r[1])}; got != want[i] {
			t.Errorf("row %d = %v, want %v", i, got, want[i])
		}
	}
	if _, _, ierr := pg.Query(`PRAGMA integrity_check`); ierr != nil {
		t.Errorf("PRAGMA integrity_check: %v -- a genuinely duplicate UNIQUE value would fail this", ierr)
	}
}

func intOrStr(v Value) string {
	switch v.Typ {
	case Int:
		return strconv.FormatInt(v.I, 10)
	default:
		return string(v.S)
	}
}
