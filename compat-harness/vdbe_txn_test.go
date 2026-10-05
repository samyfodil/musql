// Tests explicit SQL transaction control (BEGIN, COMMIT, ROLLBACK).
// and rolled-back data never does (PRAGMA integrity_check='ok' on the
// resulting file, read back by C SQLite).
//
// Savepoints (SAVEPOINT/RELEASE/ROLLBACK TO) have their own gate in
// vdbe_savepoint_test.go; gate 12 below only checks that they and the
// whole-transaction statements here agree about who ends whose transaction.
//
// It also exercises the pre-existing database/sql driver transaction API
// (driver: sql.Tx via db.Begin()/tx.Commit()/tx.Rollback(), see
// driver/conn.go and tx.go) side by side with the new SQL-text
// BEGIN/COMMIT/ROLLBACK statements to confirm the reconciliation between
// the two entry points documented in conn.go's execTxnStmt: mixing them on
// one Conn does not desync or double-close the held transaction *DB.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	_ "github.com/samyfodil/musql/driver"
)

// execTxnBoth is execDropBoth's exact counterpart for this file: run sqlText
// against both the pure-Go engine writer (db) and a real, live SQLite
// connection (sdb), requiring identical success/failure and, on failure,
// identical error text (engine's own "engine: " prefix stripped).
func execTxnBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
	t.Helper()
	engErr := db.Exec(sqlText)
	_, realErr := sdb.Exec(sqlText)

	if (engErr == nil) != (realErr == nil) {
		t.Fatalf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engErr, realErr)
	}
	if engErr == nil {
		return
	}
	engMsg := strings.TrimPrefix(engErr.Error(), "engine: ")
	realMsg := realErr.Error()
	if engMsg != realMsg {
		t.Fatalf("Exec(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", sqlText, engMsg, realMsg)
	}
}

// txnIntRow is one row of an integer column, used by the count/select checks
// below -- deliberately simple (single-column int) since this file is about
// transaction boundaries, not general row-shape fidelity (already covered
// by write_verify_test.go/vdbe_drop_test.go).
type txnIntRow struct{ a int64 }

// txnEngineRowsOf reads "SELECT a FROM <table> ORDER BY a" through the engine's
// OWN read path (SnapshotPager -- see engine/writer.go's doc comment: this
// gives read-your-writes visibility into db's CURRENT, possibly
// uncommitted, in-memory row store, exactly what a mid-transaction
// assertion needs).
func txnEngineRowsOf(t *testing.T, db *engine.Session, table string) []txnIntRow {
	t.Helper()
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pager.Close()
	_, rows, err := pager.QueryArgs(fmt.Sprintf("SELECT a FROM %s ORDER BY a", table), nil)
	if err != nil {
		t.Fatalf("engine query %s: %v", table, err)
	}
	out := make([]txnIntRow, len(rows))
	for i, r := range rows {
		out[i] = txnIntRow{a: r[0].I}
	}
	return out
}

// txnRealRowsOf is txnEngineRowsOf's real-SQLite counterpart.
func txnRealRowsOf(t *testing.T, sdb *sql.DB, table string) []txnIntRow {
	t.Helper()
	rows, err := sdb.Query(fmt.Sprintf("SELECT a FROM %s ORDER BY a", table))
	if err != nil {
		t.Fatalf("real query %s: %v", table, err)
	}
	defer rows.Close()
	var out []txnIntRow
	for rows.Next() {
		var r txnIntRow
		if err := rows.Scan(&r.a); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// requireTxnRowsMatch asserts both engines currently agree on table's content
// -- the cross-check that proves a ROLLBACK/COMMIT actually took effect
// (not merely that the ROLLBACK/COMMIT statement itself returned success).
func requireTxnRowsMatch(t *testing.T, db *engine.Session, sdb *sql.DB, table string, want []int64) {
	t.Helper()
	got := txnEngineRowsOf(t, db, table)
	gotReal := txnRealRowsOf(t, sdb, table)
	assertRowsEqual := func(label string, rows []txnIntRow) {
		if len(rows) != len(want) {
			t.Fatalf("%s: %s: got %d rows %v, want %d %v", label, table, len(rows), rows, len(want), want)
		}
		for i, r := range rows {
			if r.a != want[i] {
				t.Fatalf("%s: %s: row %d = %d, want %d", label, table, i, r.a, want[i])
			}
		}
	}
	assertRowsEqual("engine", got)
	assertRowsEqual("C SQLite (oracle)", gotReal)
}

// requireSchemaObjectAbsent/Present check sqlite_master (C SQLite, the
// oracle sdb) for name -- used to confirm a ROLLBACK undid a CREATE TABLE/
// INDEX (or a COMMIT kept a DROP). The engine side of that same guarantee is
// checked at the very end of testTxnScenario, via a genuine Close-then-
// OpenWrite round trip read back by C SQLite.
func requireSchemaObjectAbsent(t *testing.T, sdb *sql.DB, name string) {
	t.Helper()
	var n int
	if err := sdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master for %s: %v", name, err)
	}
	if n != 0 {
		t.Fatalf("sqlite_master: %s unexpectedly present after rollback", name)
	}
}

func requireSchemaObjectPresent(t *testing.T, sdb *sql.DB, name string) {
	t.Helper()
	var n int
	if err := sdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master for %s: %v", name, err)
	}
	if n != 1 {
		t.Fatalf("sqlite_master: %s missing (want present)", name)
	}
}

// TestTxnStatementsMatchCSQLite is the main BEGIN/COMMIT/ROLLBACK
// conformance gate: see the package doc comment above.
func TestTxnStatementsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testTxnScenario(t, pageSize)
		})
	}
}

func testTxnScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("txn_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection: BEGIN/COMMIT/ROLLBACK must land on the SAME connection as the statements around them

	// ---- 1. autocommit-time misuse: COMMIT/ROLLBACK/END with no active
	// transaction, verified against C SQLite's exact wording ----
	for _, s := range []string{`COMMIT`, `ROLLBACK`, `END`} {
		execTxnBoth(t, db, sdb, s)
	}

	// ---- 2. schema to work with ----
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
	} {
		execTxnBoth(t, db, sdb, s)
	}
	requireTxnRowsMatch(t, db, sdb, "t", []int64{1, 2, 3})

	// ---- 3. nested BEGIN ----
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `BEGIN`) // cannot start a transaction within a transaction
	execTxnBoth(t, db, sdb, `ROLLBACK`)

	// ---- 4. ROLLBACK undoes DML ----
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `INSERT INTO t VALUES(4),(5)`)
	requireTxnRowsMatch(t, db, sdb, "t", []int64{1, 2, 3, 4, 5}) // read-your-writes, still uncommitted
	execTxnBoth(t, db, sdb, `ROLLBACK`)
	requireTxnRowsMatch(t, db, sdb, "t", []int64{1, 2, 3}) // fully undone

	// ---- 5. COMMIT keeps DML ----
	execTxnBoth(t, db, sdb, `BEGIN TRANSACTION`)
	execTxnBoth(t, db, sdb, `UPDATE t SET a = a + 10 WHERE a = 2`)
	execTxnBoth(t, db, sdb, `DELETE FROM t WHERE a = 3`)
	execTxnBoth(t, db, sdb, `COMMIT TRANSACTION`)
	requireTxnRowsMatch(t, db, sdb, "t", []int64{1, 12})

	// ---- 6. BEGIN access-mode words + END as COMMIT's alias ----
	for _, mode := range []string{"DEFERRED", "IMMEDIATE", "EXCLUSIVE", "DEFERRED TRANSACTION"} {
		execTxnBoth(t, db, sdb, "BEGIN "+mode)
		execTxnBoth(t, db, sdb, `INSERT INTO t VALUES(100)`)
		execTxnBoth(t, db, sdb, `END`)
	}
	requireTxnRowsMatch(t, db, sdb, "t", []int64{1, 12, 100, 100, 100, 100})
	// clean up the four 100s added just above, back to a known baseline
	execTxnBoth(t, db, sdb, `DELETE FROM t WHERE a = 100`)
	requireTxnRowsMatch(t, db, sdb, "t", []int64{1, 12})

	// ---- 7. ROLLBACK undoes CREATE TABLE + CREATE INDEX + their rows ----
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `CREATE TABLE t2(x INTEGER)`)
	execTxnBoth(t, db, sdb, `CREATE INDEX idx2 ON t2(x)`)
	execTxnBoth(t, db, sdb, `INSERT INTO t2 VALUES(1)`)
	execTxnBoth(t, db, sdb, `ROLLBACK`)
	requireSchemaObjectAbsent(t, sdb, "t2")
	requireSchemaObjectAbsent(t, sdb, "idx2")
	// t2 must be gone on the ENGINE side too (Exec's own write-path dispatch
	// doesn't run SELECT at all -- see engine/insert_write.go's Exec -- so
	// this queries through SnapshotPager/QueryArgs, the same read path
	// txnEngineRowsOf uses, rather than execTxnBoth).
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, _, err = pager.QueryArgs(`SELECT * FROM t2`, nil)
	pager.Close()
	if err == nil {
		t.Fatalf("SELECT * FROM t2: want an error (table rolled back away), got success")
	}

	// ---- 8. COMMIT keeps CREATE TABLE ----
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `CREATE TABLE t3(a INTEGER)`)
	execTxnBoth(t, db, sdb, `INSERT INTO t3 VALUES(7)`)
	execTxnBoth(t, db, sdb, `COMMIT`)
	requireSchemaObjectPresent(t, sdb, "t3")
	requireTxnRowsMatch(t, db, sdb, "t3", []int64{7})

	// ---- 9. ROLLBACK undoes DROP TABLE (the dropped table reappears with
	// its original rows) ----
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `DROP TABLE t3`)
	execTxnBoth(t, db, sdb, `ROLLBACK`)
	requireSchemaObjectPresent(t, sdb, "t3")
	requireTxnRowsMatch(t, db, sdb, "t3", []int64{7})

	// ---- 10. COMMIT keeps DROP TABLE ----
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `DROP TABLE t3`)
	execTxnBoth(t, db, sdb, `COMMIT`)
	requireSchemaObjectAbsent(t, sdb, "t3")

	// ---- 11. a statement error mid-transaction is a STATEMENT-level
	// failure, not a transaction-level one: the transaction stays open (ON
	// CONFLICT ABORT default -- verified directly against C SQLite), so
	// a later statement in the same transaction still applies and COMMIT
	// still commits everything that DID succeed ----
	execTxnBoth(t, db, sdb, `CREATE TABLE u(a INTEGER UNIQUE)`)
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `INSERT INTO u VALUES(1)`)
	// UNIQUE constraint violation: both sides must reject it, but this
	// engine's constraint-error WORDING is not gated byte-for-byte against
	// C SQLite's (unlike the transaction-control errors above, which
	// are -- see execTxnBoth's doc comment); only success/failure agreement
	// is asserted here, exactly like the TCL corpus harness's own
	// "constraint" classification (tcl_test.go's tclClassifyExecErr).
	if err := db.Exec(`INSERT INTO u VALUES(1)`); err == nil {
		t.Fatalf("INSERT INTO u VALUES(1) (duplicate): want a UNIQUE constraint error, got success")
	}
	if _, err := sdb.Exec(`INSERT INTO u VALUES(1)`); err == nil {
		t.Fatalf("C SQLite INSERT INTO u VALUES(1) (duplicate): want a UNIQUE constraint error, got success")
	}
	execTxnBoth(t, db, sdb, `INSERT INTO nonexistent VALUES(1)`) // unrelated "no such table" error
	execTxnBoth(t, db, sdb, `INSERT INTO u VALUES(2)`)           // transaction still open: this still applies
	execTxnBoth(t, db, sdb, `COMMIT`)
	requireTxnRowsMatch(t, db, sdb, "u", []int64{1, 2})

	// ---- 12. SAVEPOINT/RELEASE/ROLLBACK TO interoperate with the
	// whole-transaction statements above (their own full differential gate
	// is vdbe_savepoint_test.go): a savepoint opened in autocommit starts a
	// transaction that RELEASE commits, and one opened inside a BEGIN is
	// destroyed by that transaction's ROLLBACK. Either way u ends exactly
	// as gate 11 left it. ----
	execTxnBoth(t, db, sdb, `SAVEPOINT sp1`)
	execTxnBoth(t, db, sdb, `INSERT INTO u VALUES(3)`)
	execTxnBoth(t, db, sdb, `ROLLBACK TO sp1`)
	requireTxnRowsMatch(t, db, sdb, "u", []int64{1, 2})
	execTxnBoth(t, db, sdb, `RELEASE sp1`) // sp1 started the transaction, so this commits it
	execTxnBoth(t, db, sdb, `BEGIN`)
	execTxnBoth(t, db, sdb, `SAVEPOINT sp2`)
	execTxnBoth(t, db, sdb, `INSERT INTO u VALUES(4)`)
	execTxnBoth(t, db, sdb, `ROLLBACK`)
	execTxnBoth(t, db, sdb, `RELEASE sp2`) // "no such savepoint": the ROLLBACK took it with it
	requireTxnRowsMatch(t, db, sdb, "u", []int64{1, 2})

	// ---- 13. an explicit transaction left open at Close is rolled back,
	// not silently persisted (see engine/writer.go's Close doc comment) ----
	if err := db.Exec(`BEGIN`); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if err := db.Exec(`INSERT INTO t VALUES(999)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close (with an abandoned open transaction): %v", err)
	}

	// ---- 14. Close-then-reopen: committed data persists, and the
	// abandoned-at-Close transaction's INSERT (999) does NOT ----
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close (phase 2, no-op passthrough): %v", err)
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

	verifyTableViaCSQLite(t, fdb, wvTable{name: "t", colList: "a", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1)}},
		{rowid: 2, cols: []any{int64(12)}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "u", colList: "a", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1)}},
		{rowid: 2, cols: []any{int64(2)}},
	}})
	var n999 int
	if err := fdb.QueryRow(`SELECT count(*) FROM t WHERE a = 999`).Scan(&n999); err != nil {
		t.Fatalf("query t for a=999: %v", err)
	}
	if n999 != 0 {
		t.Fatalf("a=999 (from the transaction abandoned at Close) persisted to disk -- want it rolled back")
	}
}

// TestTxnPureDriverAPI verifies the pre-existing database/sql driver
// transaction API (driver: sql.Tx via db.Begin()/tx.Commit()/
// tx.Rollback(), see driver/conn.go and the root package's tx.go for
// the analogous cgo-backed implementation) still works after this feature's
// changes, AND that it reconciles cleanly with the new SQL-text
// BEGIN/COMMIT/ROLLBACK statements (conn.go's execTxnStmt): both are just
// different spellings of the same c.tx-held-*DB operation, so mixing them
// on one connection must not desync or double-close anything.
func TestTxnPureDriverAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "driver_txn.sqlite")
	db, err := sql.Open("musql-pure", path)
	if err != nil {
		t.Fatalf("sql.Open(musql-pure): %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one logical connection: BeginTx's held *DB lives on Conn, not shared across pooled connections

	mustExec := func(s string, args ...any) {
		t.Helper()
		if _, err := db.Exec(s, args...); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count(%s): %v", table, err)
		}
		return n
	}

	mustExec(`CREATE TABLE p(a INTEGER)`)

	// ---- Go-API transaction: db.Begin()/tx.Commit() ----
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO p VALUES(1)`); err != nil {
		t.Fatalf("tx.Exec INSERT: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit: %v", err)
	}
	if n := count("p"); n != 1 {
		t.Fatalf("after tx.Commit: count(p) = %d, want 1", n)
	}

	// ---- Go-API transaction: db.Begin()/tx.Rollback() ----
	tx, err = db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO p VALUES(2)`); err != nil {
		t.Fatalf("tx.Exec INSERT: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("tx.Rollback: %v", err)
	}
	if n := count("p"); n != 1 {
		t.Fatalf("after tx.Rollback: count(p) = %d, want 1 (unchanged)", n)
	}

	// ---- SQL-text transaction: raw "BEGIN"/"COMMIT" statements on the same
	// *sql.DB (single connection, see SetMaxOpenConns(1) above) ----
	mustExec(`BEGIN`)
	mustExec(`INSERT INTO p VALUES(3)`)
	mustExec(`COMMIT`)
	if n := count("p"); n != 2 {
		t.Fatalf("after SQL-text COMMIT: count(p) = %d, want 2", n)
	}

	mustExec(`BEGIN`)
	mustExec(`INSERT INTO p VALUES(4)`)
	mustExec(`ROLLBACK`)
	if n := count("p"); n != 2 {
		t.Fatalf("after SQL-text ROLLBACK: count(p) = %d, want 2 (unchanged)", n)
	}

	// ---- nested BEGIN via SQL text is rejected, exactly like the direct
	// engine.DB gate above ----
	mustExec(`BEGIN`)
	if _, err := db.Exec(`BEGIN`); err == nil {
		t.Fatalf("nested SQL-text BEGIN: want an error, got success")
	} else if got := err.Error(); got != engine.ErrMsgTxnNested {
		t.Fatalf("nested SQL-text BEGIN: error = %q, want %q", got, engine.ErrMsgTxnNested)
	}
	mustExec(`ROLLBACK`)

	// ---- COMMIT/ROLLBACK with nothing active errors with the same wording
	// engine.DB itself gives (see TestTxnStatementsMatchCSQLite gate 1) --
	// driver's own conn.go's execTxnStmt reuses these exact exported
	// strings rather than a second, independently-worded copy (see its doc
	// comment).
	if _, err := db.Exec(`COMMIT`); err == nil || err.Error() != engine.ErrMsgNoActiveCommit {
		t.Fatalf("COMMIT with no active transaction: err = %v, want %q", err, engine.ErrMsgNoActiveCommit)
	}
	if _, err := db.Exec(`ROLLBACK`); err == nil || err.Error() != engine.ErrMsgNoActiveRollback {
		t.Fatalf("ROLLBACK with no active transaction: err = %v, want %q", err, engine.ErrMsgNoActiveRollback)
	}

	if n := count("p"); n != 2 {
		t.Fatalf("final count(p) = %d, want 2", n)
	}
}
