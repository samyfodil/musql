// Gates PRAGMA recursive_triggers=ON with a conflict-resolving INSERT/UPDATE
// against a table with DELETE triggers. C SQLite fires those triggers for the
// deleted victim row and re-checks uniqueness constraints afterward.
package engine

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rvdQuery runs a query against the database snapshot.
func rvdQuery(t *testing.T, db *Session, sql string) [][]Value {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := p.QueryArgs(sql, nil)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return rows
}

// rvdCell renders a Value as a comparable string.
func rvdCell(v Value) string {
	switch v.Typ {
	case Null:
		return "<NULL>"
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Text, Blob:
		return string(v.S)
	default:
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	}
}

func rvdRowStrings(rows [][]Value) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			parts[j] = rvdCell(v)
		}
		out[i] = strings.Join(parts, ",")
	}
	return out
}

func newRVDTestDB(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func rvdExecAll(t *testing.T, db *Session, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// TestReplaceVictimDeleteTriggersFireRowid verifies BEFORE and AFTER DELETE
// triggers fire for the victim row in an IPK/rowid conflict.
func TestReplaceVictimDeleteTriggersFireRowid(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(kind TEXT, a INTEGER)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('bdel', old.a); END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	if err := db.Exec(`INSERT OR REPLACE INTO t VALUES(1,'three')`); err != nil {
		t.Fatalf("INSERT OR REPLACE: %v", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"1,three"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT kind,a FROM log ORDER BY rowid`)), []string{"bdel,1", "del,1"}; !equalStrSlices(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

// TestReplaceVictimDeleteTriggersOffBaseline is a baseline: recursive_triggers=OFF
// means DELETE triggers do not fire.
func TestReplaceVictimDeleteTriggersOffBaseline(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(kind TEXT, a INTEGER)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('bdel', old.a); END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END`,
	)
	if err := db.Exec(`INSERT OR REPLACE INTO t VALUES(1,'three')`); err != nil {
		t.Fatalf("INSERT OR REPLACE: %v", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"1,three"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if rows := rvdQuery(t, db, `SELECT * FROM log`); len(rows) != 0 {
		t.Fatalf("log = %v, want empty (recursive_triggers is OFF)", rows)
	}
}

// TestReplaceVictimDeleteTriggersUniqueIndex verifies the UNIQUE-index case
// where the victim's rowid differs from the candidate's.
func TestReplaceVictimDeleteTriggersUniqueIndex(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`CREATE TABLE log(kind TEXT, a INTEGER)`,
		`INSERT INTO t VALUES(1,'x')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('bdel', old.a); END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	if err := db.Exec(`INSERT OR REPLACE INTO t VALUES(2,'x')`); err != nil {
		t.Fatalf("INSERT OR REPLACE: %v", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"2,x"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT kind,a FROM log ORDER BY rowid`)), []string{"bdel,1", "del,1"}; !equalStrSlices(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

// TestReplaceVictimDeleteTriggersUpdateOrReplace is the UPDATE variant of
// the UNIQUE-index case.
func TestReplaceVictimDeleteTriggersUpdateOrReplace(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`CREATE TABLE log(kind TEXT, a INTEGER)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(2,'y')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('bdel', old.a); END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	if err := db.Exec(`UPDATE OR REPLACE t SET b='x' WHERE a=2`); err != nil {
		t.Fatalf("UPDATE OR REPLACE: %v", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"2,x"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT kind,a FROM log ORDER BY rowid`)), []string{"bdel,1", "del,1"}; !equalStrSlices(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

// TestReplaceVictimDeleteTriggersRecheckAbort verifies the recheck: an AFTER
// DELETE trigger updates a sibling row to the candidate's value, and the
// recheck must abort the whole statement.
func TestReplaceVictimDeleteTriggersRecheckAbort(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(3,'z')`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN UPDATE t SET b='x' WHERE a=3; END`,
		`PRAGMA recursive_triggers=ON`,
	)
	err := db.Exec(`INSERT OR REPLACE INTO t VALUES(2,'x')`)
	if err == nil {
		t.Fatalf("INSERT OR REPLACE: expected a UNIQUE constraint failure from the recheck, got none")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") || !strings.Contains(err.Error(), "t.b") {
		t.Fatalf("error = %v, want a UNIQUE constraint failed: t.b", err)
	}
	// The whole statement is undone: row 3 is back at 'z' (the AFTER
	// trigger's own write reverted too), and no row 2 was ever added.
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t ORDER BY a`)), []string{"1,x", "3,z"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v (full statement rollback)", got, want)
	}
}

// TestReplaceVictimDeleteTriggersMultiTableRollback extends recheck-abort with
// a write to an unrelated table that must also be undone.
func TestReplaceVictimDeleteTriggersMultiTableRollback(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`CREATE TABLE other(x INTEGER)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(3,'z')`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO other VALUES(old.a); UPDATE t SET b='x' WHERE a=3; END`,
		`PRAGMA recursive_triggers=ON`,
	)
	err := db.Exec(`INSERT OR REPLACE INTO t VALUES(2,'x')`)
	if err == nil {
		t.Fatalf("INSERT OR REPLACE: expected a UNIQUE constraint failure, got none")
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t ORDER BY a`)), []string{"1,x", "3,z"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if rows := rvdQuery(t, db, `SELECT * FROM other`); len(rows) != 0 {
		t.Fatalf("other = %v, want empty -- the trigger's own write to an UNRELATED table must be rolled back too", rows)
	}
}

// TestReplaceVictimDeleteTriggersMultiRowRollback verifies that a later row's
// recheck failure undoes an earlier row's already-applied insert.
func TestReplaceVictimDeleteTriggersMultiRowRollback(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(3,'z')`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN UPDATE t SET b='x' WHERE a=3; END`,
		`PRAGMA recursive_triggers=ON`,
	)
	err := db.Exec(`INSERT OR REPLACE INTO t VALUES(50,'freshval'),(2,'x')`)
	if err == nil {
		t.Fatalf("INSERT OR REPLACE: expected a UNIQUE constraint failure, got none")
	}
	// Neither the fresh (50,'freshval') row from the FIRST tuple nor
	// anything from the second survives.
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t ORDER BY a`)), []string{"1,x", "3,z"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
}

// TestReplaceVictimDeleteTriggersRaiseIgnoreRowid verifies RAISE(IGNORE) in a
// BEFORE DELETE trigger: the row survives and recheck re-tests rowid existence.
func TestReplaceVictimDeleteTriggersRaiseIgnoreRowid(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN SELECT RAISE(IGNORE); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	err := db.Exec(`INSERT OR REPLACE INTO t VALUES(1,'three')`)
	if err == nil {
		t.Fatalf("INSERT OR REPLACE: expected a UNIQUE constraint failure, got none")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed: t.a") {
		t.Fatalf("error = %v, want UNIQUE constraint failed: t.a", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"1,one"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v (RAISE(IGNORE) survivor never overwritten)", got, want)
	}
}

// TestReplaceVictimDeleteTriggersRaiseIgnoreUniqueIndex is the UNIQUE-index
// variant where the victim survives and recheck catches the duplicate.
func TestReplaceVictimDeleteTriggersRaiseIgnoreUniqueIndex(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`INSERT INTO t VALUES(1,'x')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN SELECT RAISE(IGNORE); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	err := db.Exec(`INSERT OR REPLACE INTO t VALUES(2,'x')`)
	if err == nil {
		t.Fatalf("INSERT OR REPLACE: expected a UNIQUE constraint failure, got none")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") || !strings.Contains(err.Error(), "t.b") {
		t.Fatalf("error = %v, want UNIQUE constraint failed: t.b", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"1,x"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
}

// TestReplaceVictimOrconfInherited verifies that the DELETE trigger body
// inherits REPLACE conflict handling unconditionally, overriding even the
// trigger's own explicit conflict clause.
func TestReplaceVictimOrconfInherited(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`CREATE TABLE other(x INTEGER UNIQUE, y TEXT)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO other VALUES(1,'orig')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT OR IGNORE INTO other VALUES(1,'from-trigger'); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	if err := db.Exec(`INSERT OR REPLACE INTO t VALUES(2,'x')`); err != nil {
		t.Fatalf("INSERT OR REPLACE: %v", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"2,x"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM other`)), []string{"1,from-trigger"}; !equalStrSlices(got, want) {
		t.Fatalf("other = %v, want %v (the trigger's own OR IGNORE is overridden to REPLACE)", got, want)
	}
}

// TestReplaceVictimDeleteTriggersAfterRaiseAbort verifies that an AFTER DELETE
// trigger's RAISE(ABORT, msg) propagates and rolls back the whole statement.
func TestReplaceVictimDeleteTriggersAfterRaiseAbort(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE other(x INTEGER)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO other VALUES(old.a); END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN SELECT RAISE(ABORT,'nope'); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	err := db.Exec(`INSERT OR REPLACE INTO t VALUES(1,'two')`)
	if err == nil {
		t.Fatalf("INSERT OR REPLACE: expected the RAISE(ABORT) error, got none")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %v, want it to contain the raw RAISE message %q", err, "nope")
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"1,one"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if rows := rvdQuery(t, db, `SELECT * FROM other`); len(rows) != 0 {
		t.Fatalf("other = %v, want empty -- the BEFORE trigger's own write must be undone too", rows)
	}
}

// TestReplaceVictimDeleteTriggersFKRestrict verifies FK RESTRICT works with
// DELETE triggers: a referenced parent row refusing REPLACE rolls back the
// trigger's write too.
func TestReplaceVictimDeleteTriggersFKRestrict(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, pa INTEGER REFERENCES p(a) ON DELETE RESTRICT)`,
		`INSERT INTO p VALUES(1,'one')`,
		`INSERT INTO c VALUES(1,1)`,
		`CREATE TRIGGER bd BEFORE DELETE ON p BEGIN INSERT INTO p(a,b) VALUES(-1,'log') ON CONFLICT(a) DO NOTHING; END`,
		`PRAGMA recursive_triggers=ON`,
	)
	err := db.Exec(`INSERT OR REPLACE INTO p VALUES(1,'two')`)
	if err == nil {
		t.Fatalf("INSERT OR REPLACE: expected a FOREIGN KEY constraint failure, got none")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("error = %v, want a FOREIGN KEY constraint failure", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM p ORDER BY a`)), []string{"1,one"}; !equalStrSlices(got, want) {
		t.Fatalf("p = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM c`)), []string{"1,1"}; !equalStrSlices(got, want) {
		t.Fatalf("c = %v, want %v", got, want)
	}
}

// TestReplaceVictimDeleteTriggersInsertReturning verifies that INSERT OR REPLACE
// with RETURNING and DELETE triggers now compiles and works.
func TestReplaceVictimDeleteTriggersInsertReturning(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE log(x)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('d-'||old.a||'-'||old.b); END`,
		`INSERT INTO t VALUES(1,'one')`,
		`PRAGMA recursive_triggers=ON`,
	)
	cols, rows, err := db.execReturningViaVM(`INSERT OR REPLACE INTO t VALUES(1,'three') RETURNING *`, nil)
	if err != nil {
		t.Fatalf("INSERT OR REPLACE ... RETURNING: %v", err)
	}
	if got, want := rvdRowStrings(rows), []string{"1,three"}; !equalStrSlices(got, want) {
		t.Fatalf("RETURNING rows = %v (cols %v), want %v", got, cols, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t`)), []string{"1,three"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log`)), []string{"d-1-one"}; !equalStrSlices(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
	_, rows, err = db.execReturningViaVM(`INSERT OR REPLACE INTO t VALUES(1,'four'),(2,'five') RETURNING a,b`, nil)
	if err != nil {
		t.Fatalf("multi-row INSERT OR REPLACE ... RETURNING: %v", err)
	}
	if got, want := rvdRowStrings(rows), []string{"1,four", "2,five"}; !equalStrSlices(got, want) {
		t.Fatalf("RETURNING rows = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log`)), []string{"d-1-one", "d-1-three"}; !equalStrSlices(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

// TestReplaceVictimUpdateReturning verifies that UPDATE OR REPLACE with
// RETURNING and DELETE triggers now compiles and works.
func TestReplaceVictimUpdateReturning(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE t(a INTEGER UNIQUE, b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO t VALUES(2,'two')`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
		`PRAGMA recursive_triggers=ON`,
	)
	_, cerr := db.compileWrite(`UPDATE OR REPLACE t SET a=1 WHERE b='two' RETURNING a,b`)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	_, rows, err := db.execReturningViaVM(`UPDATE OR REPLACE t SET a=1 WHERE b='two' RETURNING a,b`, nil)
	if err != nil {
		t.Fatalf("UPDATE OR REPLACE ... RETURNING: %v", err)
	}
	if got, want := rvdRowStrings(rows), []string{"1,two"}; !equalStrSlices(got, want) {
		t.Fatalf("RETURNING rows = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t ORDER BY a`)), []string{"1,two"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log`)), []string{"1"}; !equalStrSlices(got, want) {
		t.Fatalf("log = %v, want %v (the victim's DELETE trigger must fire)", got, want)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
