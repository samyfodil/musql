// Rootpage tail-drop narrowing: tail deletions don't force rootpage decline.
package compat

import (
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// Replay delete4.test through tail-drop rootpage swap.
func TestRootpageTailServesDelete4FullSequence(t *testing.T) {
	stmts := []string{
		// test 6.0: no drop history yet.
		`CREATE TABLE t2(x INT)`,
		`INSERT INTO t2(x) VALUES(1),(2),(3),(4),(5)`,
		`DELETE FROM t2 WHERE EXISTS(SELECT 1 FROM t2 AS v WHERE v.x=t2.x-1)`,
		`SELECT x FROM t2`,
		// test 6.1: the tail drop-then-recreate this round narrows.
		`DROP TABLE IF EXISTS t2`,
		`CREATE TABLE t2(x INT)`,
		`INSERT INTO t2(x) VALUES(1),(2),(3),(4),(5)`,
		`DELETE FROM t2 WHERE EXISTS(SELECT 1 FROM t2 AS v WHERE v.x=t2.x+1)`,
		`SELECT x FROM t2`,
		// test 7.1: t3, WITHOUT ROWID with two indexes -- more tail growth.
		`CREATE TABLE t3(id INT PRIMARY KEY, a, b) WITHOUT ROWID`,
		`CREATE INDEX t3a ON t3(a)`,
		`CREATE INDEX t3b ON t3(b)`,
		`INSERT INTO t3 VALUES(1, 1, 1)`,
		`INSERT INTO t3 VALUES(2, 2, 2)`,
		`INSERT INTO t3 VALUES(3, 3, 3)`,
		`INSERT INTO t3 VALUES(4, 4, 1)`,
		`DELETE FROM t3 WHERE a=4 OR b=1`,
		`SELECT * FROM t3`,
		// test 7.2.0: t4/t5, then the mined statement itself.
		`CREATE TABLE t4(a PRIMARY KEY, b) WITHOUT ROWID`,
		`CREATE INDEX t4i ON t4(b)`,
		`INSERT INTO t4 VALUES(1, 'hello')`,
		`INSERT INTO t4 VALUES(2, 'world')`,
		`CREATE TABLE t5(a PRIMARY KEY, b) WITHOUT ROWID`,
		`CREATE INDEX t5i ON t5(b)`,
		`INSERT INTO t5 VALUES(1, 'hello')`,
		`INSERT INTO t5 VALUES(3, 'world')`,
		`PRAGMA writable_schema = 1`,
		`UPDATE sqlite_master SET rootpage = (SELECT rootpage FROM sqlite_master WHERE name = 't5') WHERE name = 't4'`,
		// A follow-on read, so a silently-wrong SERVED answer (not just an
		// unwanted decline) would also be caught: the swap really did apply.
		`SELECT rootpage FROM sqlite_master WHERE name='t4'`,
	}
	differ(t, "delete4.test full drop-then-swap sequence", stmts)
}

// TestRootpageTailServesAfterTailDropCrossSession is
// TestRootpageMarkerCrossSessionRepro's positive-space twin (that test is the
// negative control: a NON-tail drop history must still decline after a fresh
// reopen). Here the SAME shape of test -- one session bakes drop history,
// a completely fresh session/connection reopens the file -- uses a TAIL drop
// instead, and must now be SERVED, agreeing with the oracle, rather than
// declined: this is round 42's own cross-session persistence gate (byte 73,
// format.go/writer.go), proving the marker survives a reopen the same way
// byte 72's already does.
func TestRootpageTailServesAfterTailDropCrossSession(t *testing.T) {
	session1 := []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(a)`,
		`DROP TABLE t2`,
		`CREATE TABLE t3(a)`,
	}
	const createT4 = `CREATE TABLE t4(a)`
	const wsUpdate = `UPDATE sqlite_master SET tbl_name = CAST((SELECT rootpage FROM sqlite_master WHERE name='t3') AS TEXT) WHERE name='t1'`
	const readBack = `SELECT tbl_name FROM sqlite_master WHERE name='t1'`

	mattnDir := t.TempDir()
	mattnPath := filepath.Join(mattnDir, "m.sqlite")
	runSession(t, "sqlite3", mattnPath, session1...)
	mattnDB := openSession(t, "sqlite3", mattnPath)
	defer mattnDB.Close()
	if _, err := mattnDB.Exec(createT4); err != nil {
		t.Fatalf("mattn: %s: %v", createT4, err)
	}
	if _, err := mattnDB.Exec(`PRAGMA writable_schema=1`); err != nil {
		t.Fatalf("mattn: PRAGMA writable_schema=1: %v", err)
	}
	if _, err := mattnDB.Exec(wsUpdate); err != nil {
		t.Fatalf("mattn: %s: %v", wsUpdate, err)
	}
	var mattnGot string
	if err := mattnDB.QueryRow(readBack).Scan(&mattnGot); err != nil {
		t.Fatalf("mattn: %s: %v", readBack, err)
	}
	t.Logf("C SQLite's own answer for this tail-drop script: tbl_name=%s", mattnGot)

	pureDir := t.TempDir()
	purePath := filepath.Join(pureDir, "p.sqlite")
	runSession(t, driver.DriverName, purePath, session1...)

	pureDB := openSession(t, driver.DriverName, purePath)
	defer pureDB.Close()
	if _, err := pureDB.Exec(createT4); err != nil {
		t.Fatalf("pure: %s: %v", createT4, err)
	}
	if _, err := pureDB.Exec(`PRAGMA writable_schema=1`); err != nil {
		t.Fatalf("pure: PRAGMA writable_schema=1: %v", err)
	}
	if _, err := pureDB.Exec(wsUpdate); err != nil {
		t.Fatalf("writable_schema rootpage SET subquery declined on a fresh reopen after a TAIL drop -- want served (agreeing with the oracle): %v", err)
	}
	var pureGot string
	if err := pureDB.QueryRow(readBack).Scan(&pureGot); err != nil {
		t.Fatalf("pure: %s: %v", readBack, err)
	}
	if pureGot != mattnGot {
		t.Errorf("DIVERGES from mattn after a tail-drop cross-session reopen: pure=%q mattn=%q", pureGot, mattnGot)
	}
}
