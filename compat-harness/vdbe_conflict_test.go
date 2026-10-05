// This file tests conflict-clause handling on INSERT/UPDATE against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// stripEngineErrContext removes engine-specific error context for comparison with C SQLite.
func stripEngineErrContext(msg string) string {
	msg = strings.TrimPrefix(msg, "vdbe: semantic error: ")
	msg = strings.TrimPrefix(msg, "engine: ")
	for _, prefix := range []string{"INSERT into ", "UPDATE "} {
		if !strings.HasPrefix(msg, prefix) {
			continue
		}
		if idx := strings.Index(msg, ": "); idx != -1 {
			return msg[idx+2:]
		}
	}
	return msg
}

// execConflictBoth runs sqlText (via ExecArgs, so RowsAffected/LastInsertId
// are available) against both the pure-Go engine writer (db) and a live real
// SQLite connection (sdb), requiring: identical success/failure; on
// failure, IDENTICAL error text (engErr's, with stripEngineErrContext's
// prefix/infix stripped); on success, identical RowsAffected AND
// LastInsertId.
func execConflictBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
	t.Helper()
	ra, li, engErr := db.ExecArgs(sqlText, nil)
	res, realErr := sdb.Exec(sqlText)

	if (engErr == nil) != (realErr == nil) {
		t.Fatalf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engErr, realErr)
	}
	if engErr != nil {
		engMsg := stripEngineErrContext(engErr.Error())
		realMsg := realErr.Error()
		if engMsg != realMsg {
			t.Fatalf("Exec(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", sqlText, engMsg, realMsg)
		}
		return
	}
	realRA, _ := res.RowsAffected()
	realLI, _ := res.LastInsertId()
	if ra != realRA {
		t.Fatalf("Exec(%s): RowsAffected: engine=%d, C SQLite=%d", sqlText, ra, realRA)
	}
	if li != realLI {
		t.Fatalf("Exec(%s): LastInsertId: engine=%d, C SQLite=%d", sqlText, li, realLI)
	}
}

// execPlainBoth runs sqlText (via plain Exec, no RowsAffected/LastInsertId
// comparison) against both sides, requiring identical success/failure and
// (on failure) identical stripped error text -- for statement kinds this
// gate doesn't itself claim byte-exact RowsAffected/LastInsertId parity for
// (CREATE TABLE, whose RowsAffected already differs between this engine and
// mattn for reasons unrelated to conflict handling; BEGIN/COMMIT, which
// engine.DB.ExecArgs doesn't route through the INSERT/UPDATE/DELETE cases at
// all and so always reports RowsAffected=0, unlike mattn's own driver).
func execPlainBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
	t.Helper()
	engErr := db.Exec(sqlText)
	_, realErr := sdb.Exec(sqlText)
	if (engErr == nil) != (realErr == nil) {
		t.Fatalf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engErr, realErr)
	}
	if engErr == nil {
		return
	}
	engMsg := stripEngineErrContext(engErr.Error())
	realMsg := realErr.Error()
	if engMsg != realMsg {
		t.Fatalf("Exec(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", sqlText, engMsg, realMsg)
	}
}

// execConflictBothAbortDefault is execConflictBoth's relaxed sibling, used
// ONLY for the plain default-ABORT (no OR-clause/UPSERT at all) demonstration
// statement in this file's script: that ONE form deliberately still routes
// through the ORIGINAL, unchanged verifyInsertUniqueOrRollback/
// checkUniqueIndexesForTable batch-check pair (see insertRowsWithConflict's
// own doc comment for why: so no pre-existing plain-INSERT behavior can
// regress from adding conflict-clause support), which reports a UNIQUE
// violation via a DIFFERENT, older wording ("UNIQUE constraint failed: index
// <name> (table <t>, rows with rowid <a> and <b>)") that predates this task
// and is not itself part of what conflict-handling needed to change --
// exactly why the TCL/SLT/driver-conformance gates already treat any
// "UNIQUE constraint failed" text as a match without comparing it exactly
// (see tclClassifyExecErr, tcl_test.go). This helper mirrors that same
// looser, pre-existing bar: success/failure must still agree, and BOTH
// sides' error text must still be classified as the SAME constraint kind,
// but not byte-for-byte identical.
func execConflictBothAbortDefault(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
	t.Helper()
	_, _, engErr := db.ExecArgs(sqlText, nil)
	_, realErr := sdb.Exec(sqlText)
	if (engErr == nil) != (realErr == nil) {
		t.Fatalf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engErr, realErr)
	}
	if engErr == nil {
		return
	}
	engMsg := stripEngineErrContext(engErr.Error())
	realMsg := realErr.Error()
	if !strings.Contains(engMsg, "UNIQUE constraint failed") || !strings.Contains(realMsg, "UNIQUE constraint failed") {
		t.Fatalf("Exec(%s): expected both sides to report a UNIQUE constraint violation:\n  engine: %q\n  C SQLite: %q", sqlText, engMsg, realMsg)
	}
}

// TestConflictHandlingMatchesCSQLite is the conflict-clause/UPSERT
// conformance gate: for each page size, and for each entry in engineModes
// (there is exactly one -- see vdbe_modes_test.go), it drives the pure-Go
// engine writer and a live real-SQLite oracle connection through an
// identical script covering every conflict action, REPLACE INTO's rowid-vs-
// unique-column distinction (including a row conflicting on TWO unique
// indexes at once), UPSERT DO NOTHING/DO UPDATE (with "excluded." and a
// suppressing WHERE), and ROLLBACK's whole-transaction scope -- then
// verifies the resulting file's exact row content and PRAGMA
// integrity_check='ok' against C SQLite, and that a Close->OpenWrite
// reopen still resolves a conflict correctly.
func TestConflictHandlingMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					testConflictScenario(t, pageSize)
				})
			}
		})
	}
}

func testConflictScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("conflict_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection, so DDL/DML/schema queries all see the SAME in-memory schema (see execConflictBoth's doc comment)

	// ---- t1: IGNORE / FAIL / (default) ABORT / REPLACE INTO shorthand /
	// ROLLBACK (both with and without an enclosing explicit transaction) --
	// a single UNIQUE column, no INTEGER PRIMARY KEY (plain hidden rowid). ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t1(a INTEGER UNIQUE, b TEXT)`)
	for _, s := range []string{
		`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`,

		// OR IGNORE: rows conflicting with an EXISTING row (a=1, a=2) are
		// silently skipped; the other two (a=4, a=5) are inserted normally.
		`INSERT OR IGNORE INTO t1 VALUES(1,'IGNORED'),(4,'w'),(2,'IGNORED2'),(5,'v')`,

		// OR FAIL: errors on the THIRD row (a=1, conflicting with the
		// original row), but the two rows before it (a=6, a=7) stay applied
		// -- unlike ABORT below -- and the row after it (a=8) is never even
		// attempted.
		`INSERT OR FAIL INTO t1 VALUES(6,'ok6'),(7,'ok7'),(1,'conflict'),(8,'never')`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// Default ABORT (no OR-clause at all): errors on the same kind of
	// conflict, but undoes EVERY row this statement inserted (a=9 is rolled
	// back too, unlike OR FAIL's a=6/a=7 above). This ONE form still routes
	// through the pre-existing, unchanged batch-check error path -- see
	// execConflictBothAbortDefault's own doc comment for why it alone uses
	// the looser (class-only, not exact-text) comparison.
	execConflictBothAbortDefault(t, db, sdb, `INSERT INTO t1 VALUES(9,'ok9'),(1,'conflictAbort'),(10,'never2')`)

	for _, s := range []string{
		// REPLACE INTO (shorthand for INSERT OR REPLACE INTO): deletes the
		// conflicting existing row (a=1's original rowid) and inserts the
		// new row under a FRESH auto-assigned rowid (verified directly
		// against C SQLite: a UNIQUE-COLUMN conflict, as opposed to an
		// explicit-rowid collision, never reuses the deleted row's rowid).
		`REPLACE INTO t1 VALUES(1,'replaced-one')`,

		// OR ROLLBACK with NO explicit transaction active: "the enclosing
		// transaction" is just this one statement, so it behaves exactly
		// like ABORT (verified directly).
		`INSERT OR ROLLBACK INTO t1 VALUES(2,'conflictRollback')`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// OR ROLLBACK's real behavior -- undoing the WHOLE enclosing explicit
	// transaction, including an EARLIER statement's own already-applied
	// change, and ending the transaction outright (a later COMMIT then
	// errors "no transaction is active", exactly like C SQLite) --
	// verified directly, requires an actual BEGIN around it.
	execPlainBoth(t, db, sdb, `BEGIN`)
	for _, s := range []string{
		`INSERT INTO t1 VALUES(100,'in-txn')`,
		`INSERT OR ROLLBACK INTO t1 VALUES(2,'conflictRollback2')`, // still conflicts with the surviving original a=2 row
		`INSERT INTO t1 VALUES(200,'after-rollback')`,              // now running in a fresh autocommit statement
	} {
		execConflictBoth(t, db, sdb, s)
	}
	// no transaction is active anymore (OR ROLLBACK above already ended it)
	// -- COMMIT errors, matching C SQLite.
	execPlainBoth(t, db, sdb, `COMMIT`)

	// ---- t2: REPLACE conflicting on TWO unique indexes at once -- both
	// existing rows must be deleted. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t2(a INTEGER UNIQUE, b INTEGER UNIQUE, c TEXT)`)
	for _, s := range []string{
		`INSERT INTO t2 VALUES(1,10,'r1'),(2,20,'r2')`,
		`INSERT OR REPLACE INTO t2 VALUES(1,20,'new')`, // conflicts with rowid1 (a=1) AND rowid2 (b=20)
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- t3: a rowid-conflict REPLACE (explicit INTEGER PRIMARY KEY value
	// collides with an existing row) -- unlike a unique-COLUMN conflict,
	// this reuses the SAME rowid rather than assigning a fresh one. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t3(id INTEGER PRIMARY KEY, b TEXT)`)
	for _, s := range []string{
		`INSERT INTO t3 VALUES(1,'r1'),(2,'r2')`,
		`INSERT OR REPLACE INTO t3 VALUES(1,'newrow1')`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- t4: UPSERT -- ON CONFLICT DO NOTHING, DO UPDATE with "excluded."
	// (against both the target table's own name and unqualified), and a
	// suppressing WHERE. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t4(a INTEGER UNIQUE, b TEXT, n INTEGER)`)
	for _, s := range []string{
		`INSERT INTO t4 VALUES(1,'x',1)`,
		`INSERT INTO t4 VALUES(1,'y',1) ON CONFLICT DO NOTHING`,
		`INSERT INTO t4 VALUES(1,'y',1) ON CONFLICT(a) DO UPDATE SET b=excluded.b, n=t4.n+excluded.n`,
		`INSERT INTO t4 VALUES(1,'never',2) ON CONFLICT(a) DO UPDATE SET n=excluded.n WHERE t4.n<1`, // t4.n is 2 here, not <1 -- silent no-op
		`INSERT INTO t4 VALUES(2,'z',5) ON CONFLICT(a) DO UPDATE SET b=excluded.b`,                  // no conflict at all -- plain insert
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- UPDATE OR IGNORE/REPLACE, and an UPSERT conflict-target
	// mismatch (a real, un-absorbed UNIQUE error, not silently swallowed). ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t5(a INTEGER UNIQUE, b INTEGER UNIQUE)`)
	for _, s := range []string{
		`INSERT INTO t5 VALUES(1,10),(2,20)`,
		`UPDATE OR IGNORE t5 SET a=1 WHERE a=2`,                 // would collide with a=1 -- silently skipped
		`UPDATE OR REPLACE t5 SET a=1 WHERE a=2`,                // deletes the OTHER conflicting row (rowid1), keeps its OWN rowid (now a=1,b=20)
		`INSERT INTO t5 VALUES(5,10)`,                           // no conflict at all -- a fresh row to conflict against below
		`INSERT INTO t5 VALUES(3,10) ON CONFLICT(a) DO NOTHING`, // conflicts via b=10 (against the row just inserted), not the named target a -- ordinary UNIQUE error
	} {
		execConflictBoth(t, db, sdb, s)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	// THE ORACLE IS ASKED ABOUT THE EXPORT: the file this engine wrote is a segment
	// file and C cannot read one, so the interchange claim runs through
	// ExportSQLite -- which tests the conversion too. See
	// convert_for_oracle_test.go.
	exported := filepath.Join(t.TempDir(), "exported-for-oracle.db")
	if xerr := sqliteconv.Export(path, exported, 0); xerr != nil {
		t.Fatalf("ExportSQLite: %v", xerr)
	}
	fdb, err := sql.Open("sqlite3", exported)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s): %v", exported, err)
	}
	defer fdb.Close()

	var integrity string
	if err := fdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
	}

	verifyTableViaCSQLite(t, fdb, wvTable{name: "t1", colList: "a,b", rows: []wvRow{
		{rowid: 2, cols: []any{int64(2), "y"}},
		{rowid: 3, cols: []any{int64(3), "z"}},
		{rowid: 4, cols: []any{int64(4), "w"}},
		{rowid: 5, cols: []any{int64(5), "v"}},
		{rowid: 6, cols: []any{int64(6), "ok6"}},
		{rowid: 7, cols: []any{int64(7), "ok7"}},
		{rowid: 8, cols: []any{int64(1), "replaced-one"}},
		{rowid: 9, cols: []any{int64(200), "after-rollback"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t2", colList: "a,b,c", rows: []wvRow{
		{rowid: 3, cols: []any{int64(1), int64(20), "new"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t3", colList: "id,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "newrow1"}},
		{rowid: 2, cols: []any{int64(2), "r2"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t4", colList: "a,b,n", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "y", int64(2)}},
		{rowid: 2, cols: []any{int64(2), "z", int64(5)}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t5", colList: "a,b", rows: []wvRow{
		{rowid: 2, cols: []any{int64(1), int64(20)}},
		{rowid: 3, cols: []any{int64(5), int64(10)}},
	}})
	if err := fdb.Close(); err != nil {
		t.Fatalf("close fdb: %v", err)
	}

	// ---- Close -> reopen: conflict resolution still works correctly
	// against a table recovered from an existing file, not just one built
	// fresh this session. ----
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	execConflictBoth(t, db2, sdb, `INSERT OR REPLACE INTO t1 VALUES(2,'reopened-replace')`)
	if err := db2.Close(); err != nil {
		t.Fatalf("engine writer Close (reopened): %v", err)
	}

	// ...and the ORACLE reads the EXPORT again, for the reason above: a second
	// export, because the engine has written to the file since the first one.
	exported2 := filepath.Join(t.TempDir(), "exported-after-reopen.db")
	if xerr := sqliteconv.Export(path, exported2, 0); xerr != nil {
		t.Fatalf("ExportSQLite (after reopen): %v", xerr)
	}
	fdb2, err := sql.Open("sqlite3", exported2)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s) after reopen: %v", exported2, err)
	}
	defer fdb2.Close()
	if err := fdb2.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check (after reopen): %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check (after reopen) = %q, want \"ok\"", integrity)
	}
	verifyTableViaCSQLite(t, fdb2, wvTable{name: "t1", colList: "a,b", rows: []wvRow{
		{rowid: 3, cols: []any{int64(3), "z"}},
		{rowid: 4, cols: []any{int64(4), "w"}},
		{rowid: 5, cols: []any{int64(5), "v"}},
		{rowid: 6, cols: []any{int64(6), "ok6"}},
		{rowid: 7, cols: []any{int64(7), "ok7"}},
		{rowid: 8, cols: []any{int64(1), "replaced-one"}},
		{rowid: 9, cols: []any{int64(200), "after-rollback"}},
		{rowid: 10, cols: []any{int64(2), "reopened-replace"}},
	}})
}
