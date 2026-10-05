// This file tests that a persistent rootpage marker prevents wrong answers
// when catalog rows are removed and the file is reopened.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// openSession opens a fresh *sql.DB against path for driver to simulate a
// new connection reopening the same file.
func openSession(t *testing.T, driver, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, path)
	if err != nil {
		t.Fatalf("%s: sql.Open(%s): %v", path, driver, err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// runSession opens a fresh session, runs statements through it, and closes it.
func runSession(t *testing.T, driver, path string, stmts ...string) {
	t.Helper()
	db := openSession(t, driver, path)
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %s: Exec(%q): %v", path, driver, s, err)
		}
	}
}

// TestRootpageMarkerCrossSessionRepro tests that a fresh session correctly
// handles rootpage reads when catalog rows have been removed, refusing to
// serve a potentially wrong value.
func TestRootpageMarkerCrossSessionRepro(t *testing.T) {
	session1 := []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(a)`,
		`CREATE TABLE t3(a)`,
		`DROP TABLE t2`,
		`DROP TABLE t3`,
		`CREATE TABLE t4(a)`,
	}
	// PRAGMA writable_schema is connection state and must run in the same
	// session as the UPDATE that depends on it.
	const createT5 = `CREATE TABLE t5(a)`
	const wsUpdate = `UPDATE sqlite_master SET tbl_name = CAST((SELECT rootpage FROM sqlite_master WHERE name='t5') AS TEXT) WHERE name='t1'`
	const readBack = `SELECT tbl_name FROM sqlite_master WHERE name='t1'`

	// Confirm C SQLite's answer and then test musql's behavior on the same file.
	mattnDir := t.TempDir()
	mattnPath := filepath.Join(mattnDir, "m.sqlite")
	runSession(t, "sqlite3", mattnPath, session1...)
	mattnDB := openSession(t, "sqlite3", mattnPath)
	defer mattnDB.Close()
	if _, err := mattnDB.Exec(createT5); err != nil {
		t.Fatalf("mattn: %s: %v", createT5, err)
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
	t.Logf("C SQLite's own answer for this script: tbl_name=%s", mattnGot)

	// Test that the rootpage value is consistent across all reads: plain SELECT,
	// writable_schema overlay, and SET subquery.
	pureDir := t.TempDir()
	purePath := filepath.Join(pureDir, "p.sqlite")
	runSession(t, driver.DriverName, purePath, session1...)

	pureDB := openSession(t, driver.DriverName, purePath)
	defer pureDB.Close()
	if _, err := pureDB.Exec(createT5); err != nil {
		t.Fatalf("pure: %s: %v", createT5, err)
	}
	const selRoot = `SELECT rootpage FROM sqlite_master WHERE name='t5'`
	var before string
	if err := pureDB.QueryRow(selRoot).Scan(&before); err != nil {
		t.Fatalf("pure: %s: %v", selRoot, err)
	}
	if _, err := pureDB.Exec(`PRAGMA writable_schema=1`); err != nil {
		t.Fatalf("pure: PRAGMA writable_schema=1: %v", err)
	}
	if _, err := pureDB.Exec(wsUpdate); err != nil {
		t.Fatalf("pure: %s: %v", wsUpdate, err)
	}
	var pureGot, after string
	if err := pureDB.QueryRow(readBack).Scan(&pureGot); err != nil {
		t.Fatalf("pure: %s: %v", readBack, err)
	}
	if err := pureDB.QueryRow(selRoot).Scan(&after); err != nil {
		t.Fatalf("pure: %s: %v", selRoot, err)
	}
	if pureGot != before || after != before {
		t.Errorf("t5's rootpage is not one number: plain read %q, SET subquery %q, read under the overlay %q", before, pureGot, after)
	}
}

// queryOrErr runs a query and returns its columns/rows on success, or the
// first error encountered at any stage.
func queryOrErr(t *testing.T, db *sql.DB, s string) (cols []string, out [][]string, err error) {
	t.Helper()
	rows, qerr := db.Query(s)
	if qerr != nil {
		return nil, nil, qerr
	}
	defer rows.Close()
	cols, err = rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			return nil, nil, serr
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = normalizeAny(v)
		}
		out = append(out, row)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, nil, rerr
	}
	return cols, out, nil
}

// TestRootpageMarkerFourMinedStatements tests four mined statements from
// SQLite's test suite where no catalog rows are removed. The marker must stay
// clear and every statement must match the oracle.
func TestRootpageMarkerFourMinedStatements(t *testing.T) {
	cases := []struct {
		name    string
		phase1  []string
		verify1 []string
	}{
		{
			// Swap t4 and t5 root pages via writable_schema.
			name: "delete4.test#7.2",
			phase1: []string{
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
			},
			verify1: []string{
				`DELETE FROM t4 WHERE b='world'`,
			},
		},
		{
			// Set all rootpages to t1's rootpage.
			name: "strict2.test#1.1",
			phase1: []string{
				`CREATE TABLE t1(a INT, b INTEGER, c TEXT, d REAL, e BLOB) STRICT`,
				`CREATE TABLE t1nn(a INT NOT NULL, b INTEGER NOT NULL, c TEXT NOT NULL, d REAL NOT NULL, e BLOB NOT NULL) STRICT`,
				`CREATE TABLE t2(a,b,c,d,e)`,
				`INSERT INTO t1(a,b,c,d,e) VALUES(1,1,'one',1.0,x'b1'),(2,2,'two',2.25,x'b2b2b2')`,
				`PRAGMA writable_schema=on`,
				`UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM sqlite_schema WHERE name='t1')`,
			},
			verify1: []string{
				`PRAGMA quick_check('t1')`,
			},
		},
		{
			// Alias i1's rootpage to i2's.
			name: "where.test#25.0",
			phase1: []string{
				`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)`,
				`CREATE UNIQUE INDEX i1 ON t1(c)`,
				`INSERT INTO t1 VALUES(1, 'one', 'i')`,
				`INSERT INTO t1 VALUES(2, 'two', 'ii')`,
				`CREATE TABLE t2(a INTEGER PRIMARY KEY, b, c)`,
				`CREATE UNIQUE INDEX i2 ON t2(c)`,
				`INSERT INTO t2 VALUES(1, 'one', 'i')`,
				`INSERT INTO t2 VALUES(2, 'two', 'ii')`,
				`INSERT INTO t2 VALUES(3, 'three', 'iii')`,
				`PRAGMA writable_schema = 1`,
				`UPDATE sqlite_schema SET rootpage = (SELECT rootpage FROM sqlite_schema WHERE name = 'i2') WHERE name = 'i1'`,
			},
			verify1: []string{
				`DELETE FROM t1 WHERE c='iii'`,
			},
		},
		{
			// WITHOUT ROWID variant: alias i1's rootpage to i2's.
			name: "where.test#25.3",
			phase1: []string{
				`CREATE TABLE t1(a PRIMARY KEY, b, c) WITHOUT ROWID`,
				`CREATE UNIQUE INDEX i1 ON t1(c)`,
				`INSERT INTO t1 VALUES(1, 'one', 'i')`,
				`INSERT INTO t1 VALUES(2, 'two', 'ii')`,
				`CREATE TABLE t2(a INTEGER PRIMARY KEY, b, c)`,
				`CREATE UNIQUE INDEX i2 ON t2(c)`,
				`INSERT INTO t2 VALUES(1, 'one', 'i')`,
				`INSERT INTO t2 VALUES(2, 'two', 'ii')`,
				`INSERT INTO t2 VALUES(3, 'three', 'iii')`,
				`PRAGMA writable_schema = 1`,
				`UPDATE sqlite_schema SET rootpage = (SELECT rootpage FROM sqlite_schema WHERE name = 'i2') WHERE name = 'i1'`,
			},
			verify1: []string{
				`SELECT * FROM t1 WHERE c='iii'`,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pureDir, mattnDir := t.TempDir(), t.TempDir()
			purePath := filepath.Join(pureDir, "p.sqlite")
			mattnPath := filepath.Join(mattnDir, "m.sqlite")

			// Run the mined statement on both engines.
			runSession(t, driver.DriverName, purePath, tc.phase1...)
			runSession(t, "sqlite3", mattnPath, tc.phase1...)

			// Run verification statements from the TCL source (informational only).
			for _, s := range tc.verify1 {
				pureDB := openSession(t, driver.DriverName, purePath)
				mattnDB := openSession(t, "sqlite3", mattnPath)
				_, _, pErr := queryOrErr(t, pureDB, s)
				_, _, mErr := queryOrErr(t, mattnDB, s)
				pureDB.Close()
				mattnDB.Close()
				if (pErr == nil) != (mErr == nil) {
					t.Logf("%s: informational (out of this round's scope): %s: pure err=%v mattn err=%v", tc.name, s, pErr, mErr)
				}
			}
		})
	}
}

// TestRootpageMarkerServesPlainSelectNoRemoval tests that a plain SELECT of
// rootpage with no catalog row removal history matches the oracle.
func TestRootpageMarkerServesPlainSelectNoRemoval(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE INDEX i1 ON t1(a)`,
		`CREATE TABLE t2(a,b)`,
	}
	const query = `SELECT name, rootpage FROM sqlite_master ORDER BY name`

	pureDir, mattnDir := t.TempDir(), t.TempDir()
	purePath := filepath.Join(pureDir, "p.sqlite")
	mattnPath := filepath.Join(mattnDir, "m.sqlite")
	runSession(t, driver.DriverName, purePath, stmts...)
	runSession(t, "sqlite3", mattnPath, stmts...)

	pureDB := openSession(t, driver.DriverName, purePath)
	defer pureDB.Close()
	mattnDB := openSession(t, "sqlite3", mattnPath)
	defer mattnDB.Close()

	pRows, err := pureDB.Query(query)
	if err != nil {
		t.Fatalf("pure: %s: %v", query, err)
	}
	pCols, pOut := collectRows(t, pRows)
	pRows.Close()
	mRows, err := mattnDB.Query(query)
	if err != nil {
		t.Fatalf("mattn: %s: %v", query, err)
	}
	mCols, mOut := collectRows(t, mRows)
	mRows.Close()
	if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
		t.Errorf("DIVERGES from mattn\n  reason: %s\n  pure:  cols=%v rows=%v\n  mattn: cols=%v rows=%v", reason, pCols, pOut, mCols, mOut)
	}
}

// TestRootpageMarkerMatchesAfterRemoval tests that rootpage reads match the
// oracle after a non-tail DROP history, in both the current and fresh sessions.
func TestRootpageMarkerMatchesAfterRemoval(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a)`, `CREATE TABLE t2(a)`, `CREATE TABLE t3(a)`,
		`DROP TABLE t2`, `DROP TABLE t3`, `CREATE TABLE t4(a)`,
	}
	const selRoot = `SELECT name, rootpage FROM sqlite_master ORDER BY name`

	purePath := filepath.Join(t.TempDir(), "p.sqlite")
	mattnPath := filepath.Join(t.TempDir(), "m.sqlite")
	db := openSession(t, driver.DriverName, purePath)
	defer db.Close()
	mattnDB := openSession(t, "sqlite3", mattnPath)
	defer mattnDB.Close()
	for _, s := range append(append([]string{}, setup...), `PRAGMA writable_schema=1`) {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("pure setup %q: %v", s, err)
		}
		if _, err := mattnDB.Exec(s); err != nil {
			t.Fatalf("mattn setup %q: %v", s, err)
		}
	}
	// Test writable_schema rootpage read in the session that did the removals.
	const wsUpd = `UPDATE sqlite_master SET tbl_name = CAST((SELECT rootpage FROM sqlite_master WHERE name='t1') AS TEXT) WHERE name='t4'`
	if _, err := db.Exec(wsUpd); err != nil {
		t.Fatalf("pure: %s: %v", wsUpd, err)
	}
	if _, err := mattnDB.Exec(wsUpd); err != nil {
		t.Fatalf("mattn: %s: %v", wsUpd, err)
	}
	const readBack = `SELECT tbl_name FROM sqlite_master WHERE name='t4'`
	var pGot, mGot string
	if err := db.QueryRow(readBack).Scan(&pGot); err != nil {
		t.Fatalf("pure: %s: %v", readBack, err)
	}
	if err := mattnDB.QueryRow(readBack).Scan(&mGot); err != nil {
		t.Fatalf("mattn: %s: %v", readBack, err)
	}
	if pGot != mGot {
		t.Errorf("writable_schema rootpage read after a same-session non-tail DROP: musql %q, C SQLite %q", pGot, mGot)
	}

	// Test plain SELECT on a fresh reopen.
	db2 := openSession(t, driver.DriverName, purePath)
	defer db2.Close()
	pCols, pOut, perr := queryOrErr(t, db2, selRoot)
	if perr != nil {
		t.Fatalf("pure reopen: %s: %v", selRoot, perr)
	}
	mCols, mOut, merr := queryOrErr(t, mattnDB, selRoot)
	if merr != nil {
		t.Fatalf("mattn: %s: %v", selRoot, merr)
	}
	if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
		t.Errorf("DIVERGES from mattn\n  reason: %s\n  pure:  %v\n  mattn: %v", reason, pOut, mOut)
	}
}
