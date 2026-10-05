// This file tests SAVEPOINT, RELEASE, and ROLLBACK TO against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// spHarness pairs an engine writer and a C SQLite connection for testing.
type spHarness struct {
	t   *testing.T
	db  *engine.Session
	sdb *sql.DB
}

func newSpHarness(t *testing.T) *spHarness {
	t.Helper()
	db, err := engine.Create(filepath.Join(t.TempDir(), "sp.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	t.Cleanup(func() { sdb.Close() })
	// One logical connection: a savepoint stack lives on the connection, so
	// every statement of the script must land on the same one.
	sdb.SetMaxOpenConns(1)
	return &spHarness{t: t, db: db, sdb: sdb}
}

// exec runs sqlText on both engines and requires them to agree on
// success/failure and, when both fail, on the exact message.
func (h *spHarness) exec(sqlText string) {
	h.t.Helper()
	engErr := h.db.Exec(sqlText)
	_, realErr := h.sdb.Exec(sqlText)
	if (engErr == nil) != (realErr == nil) {
		h.t.Fatalf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engErr, realErr)
	}
	if engErr == nil {
		return
	}
	if got, want := strings.TrimPrefix(engErr.Error(), "engine: "), realErr.Error(); got != want {
		h.t.Fatalf("Exec(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", sqlText, got, want)
	}
}

// execFailsBoth requires both engines to REJECT sqlText, without comparing
// the wording: unlike the transaction-control errors this file gates
// byte-for-byte, this engine's CONSTRAINT-error text is not pinned against
// C SQLite's (it prefixes the statement, "INSERT into t: UNIQUE
// constraint failed: t.a") -- the same split vdbe_txn_test.go's gate 11 and
// the TCL corpus's own "constraint" classification already make.
func (h *spHarness) execFailsBoth(sqlText string) {
	h.t.Helper()
	if err := h.db.Exec(sqlText); err == nil {
		h.t.Fatalf("Exec(%s): engine accepted it, want a constraint error", sqlText)
	}
	if _, err := h.sdb.Exec(sqlText); err == nil {
		h.t.Fatalf("Exec(%s): C SQLite accepted it, want a constraint error", sqlText)
	}
}

// execAll is exec over a sequence.
func (h *spHarness) execAll(stmts ...string) {
	h.t.Helper()
	for _, s := range stmts {
		h.exec(s)
	}
}

// rows reads "SELECT a FROM <table> ORDER BY a" from both engines (the
// engine side through SnapshotPager, its read-your-writes view of the
// current, possibly uncommitted, in-memory store) and requires them equal to
// want. This is what proves a ROLLBACK TO landed on the right savepoint --
// the statement returning success proves nothing on its own.
func (h *spHarness) rows(table string, want ...int64) {
	h.t.Helper()
	pager, err := h.db.SnapshotPager()
	if err != nil {
		h.t.Fatalf("SnapshotPager: %v", err)
	}
	_, gotRows, err := pager.QueryArgs(fmt.Sprintf("SELECT a FROM %s ORDER BY a", table), nil)
	pager.Close()
	if err != nil {
		h.t.Fatalf("engine query %s: %v", table, err)
	}
	got := make([]int64, len(gotRows))
	for i, r := range gotRows {
		got[i] = r[0].I
	}

	sqlRows, err := h.sdb.Query(fmt.Sprintf("SELECT a FROM %s ORDER BY a", table))
	if err != nil {
		h.t.Fatalf("real query %s: %v", table, err)
	}
	var gotReal []int64
	for sqlRows.Next() {
		var v int64
		if err := sqlRows.Scan(&v); err != nil {
			h.t.Fatalf("scan %s: %v", table, err)
		}
		gotReal = append(gotReal, v)
	}
	sqlRows.Close()
	if err := sqlRows.Err(); err != nil {
		h.t.Fatal(err)
	}

	fmtRows := func(v []int64) string { return fmt.Sprint(v) }
	if fmtRows(got) != fmtRows(want) {
		h.t.Fatalf("engine %s = %s, want %s", table, fmtRows(got), fmtRows(want))
	}
	if fmtRows(gotReal) != fmtRows(want) {
		h.t.Fatalf("C SQLite (oracle) %s = %s, want %s -- the oracle disagrees with this test's own expectation", table, fmtRows(gotReal), fmtRows(want))
	}
}

// requireInTxn asserts both engines are (or are not) inside an open
// transaction, probed the only way SQL text can ask: a BEGIN succeeds
// exactly when none is open. The probe undoes itself.
func (h *spHarness) requireInTxn(want bool) {
	h.t.Helper()
	engErr := h.db.Exec(`BEGIN`)
	_, realErr := h.sdb.Exec(`BEGIN`)
	if (engErr == nil) != (realErr == nil) {
		h.t.Fatalf("in-transaction probe: engine err=%v, C SQLite err=%v -- disagree", engErr, realErr)
	}
	if got := engErr != nil; got != want {
		h.t.Fatalf("in-transaction = %v, want %v (BEGIN said %v)", got, want, engErr)
	}
	if engErr == nil {
		h.execAll(`ROLLBACK`) // undo the probe's own transaction
	}
}

// TestSavepointMatchesCSQLite is the main gate: see the package doc
// comment above for the rule-by-rule list it walks.
func TestSavepointMatchesCSQLite(t *testing.T) {
	h := newSpHarness(t)

	// ---- 1. RELEASE / ROLLBACK TO in autocommit name nothing: "no such
	// savepoint", NOT a no-transaction error, and nothing changes ----
	h.execAll(`CREATE TABLE t(a INTEGER)`, `INSERT INTO t VALUES(1)`)
	h.execAll(`RELEASE x`, `ROLLBACK TO x`, `RELEASE SAVEPOINT x`, `ROLLBACK TO SAVEPOINT x`)
	h.requireInTxn(false)
	h.rows("t", 1)

	// ---- 2. SAVEPOINT in autocommit starts a transaction; a nested BEGIN
	// is rejected; ROLLBACK TO restores yet leaves it open; RELEASE of that
	// outermost savepoint COMMITS ----
	h.execAll(`SAVEPOINT s1`)
	h.requireInTxn(true)
	h.execAll(`BEGIN`) // cannot start a transaction within a transaction
	h.execAll(`INSERT INTO t VALUES(2)`)
	h.rows("t", 1, 2)
	h.execAll(`ROLLBACK TO s1`)
	h.requireInTxn(true) // ROLLBACK TO never ends the transaction
	h.rows("t", 1)
	h.execAll(`INSERT INTO t VALUES(3)`, `RELEASE s1`)
	h.requireInTxn(false) // RELEASE of the savepoint that started it commits
	h.rows("t", 1, 3)
	h.execAll(`RELEASE s1`) // gone now

	// ---- 3. plain ROLLBACK undoes a savepoint-started transaction whole,
	// and destroys the stack ----
	h.execAll(`SAVEPOINT s2`, `INSERT INTO t VALUES(4)`, `SAVEPOINT s3`, `INSERT INTO t VALUES(5)`)
	h.rows("t", 1, 3, 4, 5)
	h.execAll(`ROLLBACK`)
	h.requireInTxn(false)
	h.rows("t", 1, 3)
	h.execAll(`RELEASE s2`, `ROLLBACK TO s3`) // both "no such savepoint"

	// ---- 4. RELEASE pops the named savepoint AND every one after it,
	// KEEPING their changes ----
	h.execAll(`BEGIN`, `SAVEPOINT a`, `INSERT INTO t VALUES(6)`,
		`SAVEPOINT b`, `INSERT INTO t VALUES(7)`, `SAVEPOINT c`, `INSERT INTO t VALUES(8)`,
		`RELEASE b`)
	h.rows("t", 1, 3, 6, 7, 8) // RELEASE is a commit of the sub-transaction, not an undo
	h.execAll(`RELEASE c`)     // c went with b
	h.requireInTxn(true)       // the enclosing BEGIN is still open
	h.execAll(`ROLLBACK TO a`)
	h.rows("t", 1, 3)
	h.execAll(`ROLLBACK`)
	h.requireInTxn(false)

	// ---- 5. ROLLBACK TO does not pop: the same name works twice; and
	// savepoints opened AFTER it are destroyed by it ----
	h.execAll(`BEGIN`, `SAVEPOINT a`, `INSERT INTO t VALUES(9)`, `ROLLBACK TO a`)
	h.rows("t", 1, 3)
	h.execAll(`INSERT INTO t VALUES(10)`, `ROLLBACK TO a`) // reachable a second time
	h.rows("t", 1, 3)
	h.execAll(`SAVEPOINT b`, `INSERT INTO t VALUES(11)`, `ROLLBACK TO a`)
	h.rows("t", 1, 3)
	h.execAll(`ROLLBACK TO b`, `RELEASE b`) // b did not survive the ROLLBACK TO a
	h.execAll(`RELEASE a`)
	h.requireInTxn(true) // still inside the BEGIN
	h.execAll(`ROLLBACK`)

	// ---- 6. a repeated name resolves to the INNERMOST match ----
	h.execAll(`BEGIN`, `SAVEPOINT dup`, `INSERT INTO t VALUES(12)`,
		`SAVEPOINT dup`, `INSERT INTO t VALUES(13)`, `ROLLBACK TO dup`)
	h.rows("t", 1, 3, 12) // the inner dup, so 12 survives and 13 does not
	h.execAll(`RELEASE dup`)
	h.requireInTxn(true)
	h.execAll(`ROLLBACK TO dup`) // the OUTER dup is still there
	h.rows("t", 1, 3)
	h.execAll(`RELEASE dup`, `RELEASE dup`) // second one: gone
	h.execAll(`ROLLBACK`)

	// ---- 7. names are case-insensitive and quoting-insensitive ----
	h.execAll(`SAVEPOINT Abc`, `INSERT INTO t VALUES(14)`, `ROLLBACK TO ABC`)
	h.rows("t", 1, 3)
	h.execAll(`RELEASE "aBc"`)
	h.requireInTxn(false)
	h.execAll(`SAVEPOINT "a b"`, `RELEASE 'A B'`)
	h.requireInTxn(false)

	// ---- 8. a failed RELEASE / ROLLBACK TO leaves the stack intact ----
	h.execAll(`SAVEPOINT keep`, `INSERT INTO t VALUES(15)`, `RELEASE nope`, `ROLLBACK TO nope`)
	h.requireInTxn(true)
	h.rows("t", 1, 3, 15)
	h.execAll(`ROLLBACK TO keep`)
	h.rows("t", 1, 3)
	h.execAll(`RELEASE keep`)
	h.requireInTxn(false)

	// ---- 9. COMMIT with savepoints still open commits everything and
	// destroys the whole stack ----
	h.execAll(`BEGIN`, `SAVEPOINT p`, `INSERT INTO t VALUES(16)`,
		`SAVEPOINT q`, `INSERT INTO t VALUES(17)`, `COMMIT`)
	h.requireInTxn(false)
	h.rows("t", 1, 3, 16, 17)
	h.execAll(`ROLLBACK TO p`, `RELEASE q`)

	// ---- 10. DDL rolls back with the rest: CREATE TABLE/INDEX/VIEW and
	// DROP TABLE all undone by a ROLLBACK TO ----
	h.execAll(`SAVEPOINT ddl`,
		`CREATE TABLE t2(a INTEGER)`, `CREATE INDEX i2 ON t2(a)`, `CREATE VIEW v2 AS SELECT a FROM t`,
		`INSERT INTO t2 VALUES(1)`, `DROP TABLE t`)
	h.execAll(`ROLLBACK TO ddl`)
	h.requireObjectsAbsent("t2", "i2", "v2") // rolled back away, on both sides
	h.rows("t", 1, 3, 16, 17)                // and the dropped table is back, with its rows
	h.execAll(`RELEASE ddl`)
	h.requireInTxn(false)

	// ---- 11. RELEASE keeps DDL, exactly as it keeps rows ----
	h.execAll(`SAVEPOINT ddl2`, `CREATE TABLE t3(a INTEGER)`, `INSERT INTO t3 VALUES(42)`, `RELEASE ddl2`)
	h.requireInTxn(false)
	h.rows("t3", 42)
}

// requireObjectsAbsent asserts none of names is a schema object on either
// side -- the check a rolled-back CREATE needs. The engine side goes through
// SnapshotPager (its own read path; engine.DB.Exec is write-only), the
// oracle side through sqlite_master.
func (h *spHarness) requireObjectsAbsent(names ...string) {
	h.t.Helper()
	pager, err := h.db.SnapshotPager()
	if err != nil {
		h.t.Fatalf("SnapshotPager: %v", err)
	}
	defer pager.Close()
	for _, name := range names {
		_, rows, err := pager.QueryArgs(`SELECT count(*) FROM sqlite_master WHERE name = '`+name+`'`, nil)
		if err != nil {
			h.t.Fatalf("engine sqlite_master query for %s: %v", name, err)
		}
		if n := rows[0][0].I; n != 0 {
			h.t.Fatalf("engine: %s still in sqlite_master after the rollback (count=%d)", name, n)
		}
		var real int
		if err := h.sdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&real); err != nil {
			h.t.Fatalf("real sqlite_master query for %s: %v", name, err)
		}
		if real != 0 {
			h.t.Fatalf("C SQLite (oracle): %s still present after the rollback -- this test's own expectation is wrong", name)
		}
	}
}

// TestSavepointTornDownByRollbackConflict pins what a transaction-wide
// unwind does to the savepoint stack. An "ON CONFLICT ROLLBACK" violation --
// and equally a RAISE(ROLLBACK) trigger -- destroys the WHOLE transaction,
// not just the offending statement, so every savepoint inside it goes with
// it and a following RELEASE/ROLLBACK TO must report "no such savepoint"
// rather than resurrect a stale snapshot. That is the case the engine's four
// mid-statement unwind sites (vdbe_write.go, conflict.go,
// write_update_delete.go, trigger.go) all funnel through clearTxnState for.
func TestSavepointTornDownByRollbackConflict(t *testing.T) {
	t.Run("on-conflict-rollback", func(t *testing.T) {
		h := newSpHarness(t)
		h.execAll(`CREATE TABLE t(a INTEGER UNIQUE ON CONFLICT ROLLBACK)`, `INSERT INTO t VALUES(1)`)
		h.execAll(`SAVEPOINT sp`, `INSERT INTO t VALUES(2)`)
		h.rows("t", 1, 2)
		h.execFailsBoth(`INSERT INTO t VALUES(1)`) // duplicate: unwinds the whole transaction
		h.requireInTxn(false)
		h.rows("t", 1) // the savepoint's own insert went too
		h.execAll(`ROLLBACK TO sp`, `RELEASE sp`)
	})

	t.Run("raise-rollback-trigger", func(t *testing.T) {
		h := newSpHarness(t)
		h.execAll(
			`CREATE TABLE t(a INTEGER)`,
			`INSERT INTO t VALUES(1)`,
			`CREATE TRIGGER tr BEFORE INSERT ON t WHEN NEW.a = 99 BEGIN SELECT RAISE(ROLLBACK, 'no'); END`,
		)
		h.execAll(`SAVEPOINT sp`, `INSERT INTO t VALUES(2)`)
		h.rows("t", 1, 2)
		h.execAll(`INSERT INTO t VALUES(99)`) // RAISE(ROLLBACK): the transaction is gone
		h.requireInTxn(false)
		h.rows("t", 1)
		h.execAll(`ROLLBACK TO sp`, `RELEASE sp`)
	})
}

// TestSavepointClosePersistence checks the disk half of the story separately
// (the scripted gate above never commits to a file): a savepoint left open
// when the session closes is rolled back like any other in-flight
// transaction, and a RELEASEd one persists -- read back by C SQLite.
func TestSavepointClosePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sp_persist.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER)`,
		`SAVEPOINT kept`,
		`INSERT INTO t VALUES(1)`,
		`RELEASE kept`, // commits: 1 must reach the file
		`SAVEPOINT abandoned`,
		`INSERT INTO t VALUES(2)`, // never released: must NOT reach the file
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
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
	if err := fdb.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
	}
	rows, err := fdb.Query(`SELECT a FROM t ORDER BY a`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if fmt.Sprint(got) != fmt.Sprint([]int64{1}) {
		t.Fatalf("on-disk t = %v, want [1] (the released savepoint's row only)", got)
	}
}

// TestSavepointNameKeywordParity re-derives, from the oracle, which keywords
// C SQLite accepts as a BARE savepoint name, and requires the engine to
// agree on every one. engine/txn.go's parseSavepointName implements this by
// reusing nonIdentifierKeywords -- the set already derived for bare COLUMN
// names -- on the empirical finding that the two sets are identical; this
// test is what would catch that ceasing to be true.
func TestSavepointNameKeywordParity(t *testing.T) {
	var mismatch []string
	for _, kw := range sqlKeywords {
		stmt := `SAVEPOINT ` + kw
		edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cdb, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		eErr := edb.Exec(stmt)
		_, cErr := cdb.Exec(stmt)
		if (eErr == nil) != (cErr == nil) {
			mismatch = append(mismatch, fmt.Sprintf("%s(engine=%v,c=%v)", kw, eErr, cErr))
		}
		edb.Close()
		cdb.Close()
	}
	if len(mismatch) > 0 {
		sort.Strings(mismatch)
		t.Errorf("keywords where the engine and C SQLite disagree about being a bare savepoint name: %s", strings.Join(mismatch, " "))
	}
}

// TestSavepointGrammarParity pins the shapes around the name: the optional
// SAVEPOINT keyword after RELEASE / TO (consumed greedily, so bare
// "RELEASE savepoint" is a syntax error while quoted `RELEASE "savepoint"`
// is not), "ROLLBACK TRANSACTION TO", every quoting style, a string literal
// as a name, and the malformed forms with no name at all. Only agreement on
// accept/reject is required -- this engine does not reproduce SQLite's
// "incomplete input"/"near ...: syntax error" wording for a bad parse.
func TestSavepointGrammarParity(t *testing.T) {
	stmts := []string{
		`SAVEPOINT a`,
		`ROLLBACK TRANSACTION TO a`,
		`ROLLBACK TRANSACTION TO SAVEPOINT a`,
		`ROLLBACK TO SAVEPOINT a`,
		`RELEASE SAVEPOINT a`,
		`SAVEPOINT b`,
		`RELEASE  SAVEPOINT  b ;`,
		`SAVEPOINT "quoted name"`,
		`RELEASE "quoted name"`,
		`SAVEPOINT [bracket]`,
		"RELEASE `bracket`",
		`SAVEPOINT 'literal'`,
		`RELEASE 'literal'`,
		`SAVEPOINT savepoint`,   // legal: SAVEPOINT is a fallback keyword
		`RELEASE savepoint`,     // NOT legal: reads as RELEASE SAVEPOINT <missing>
		`RELEASE "savepoint"`,   // legal again once quoted
		`SAVEPOINT`,             // no name
		`RELEASE`,               // no name
		`ROLLBACK TO`,           // no name
		`SAVEPOINT 1`,           // a number is not a name
		`SAVEPOINT a b`,         // trailing garbage
		`RELEASE TRANSACTION a`, // RELEASE has no TRANSACTION form
		`RELEASE a`,             // unwind whatever is still open
		`ROLLBACK`,
	}
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	for _, s := range stmts {
		eErr := edb.Exec(s)
		_, cErr := cdb.Exec(s)
		if (eErr == nil) != (cErr == nil) {
			t.Errorf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", s, eErr, cErr)
		}
	}
}

// TestSavepointThroughPureDriver replaces a gate that pinned the OPPOSITE:
// driver used to reject every savepoint statement, because its
// per-statement autocommit session model would have discarded them. It now
// holds a session across a savepoint exactly as it does across a BEGIN, so
// the statements must SUCCEED and, more importantly, must actually undo.
//
// The decline they pinned was never conservative: with each statement
// committing in its own session, "SAVEPOINT s; INSERT; ROLLBACK" left the row
// in place where C SQLite discards it. The cell-level agreement with the
// oracle is gated in compat-harness/savepoint_driver_test.go; this is the
// driver-level assertion that the routing exists at all.
func TestSavepointThroughPureDriver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sp_driver.sqlite")
	db, err := sql.Open("musql", path)
	if err != nil {
		t.Fatalf("sql.Open(musql): %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE t(a INTEGER)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	// A savepoint in autocommit starts a transaction; releasing it commits.
	for _, s := range []string{`SAVEPOINT s`, `INSERT INTO t VALUES(1)`, `RELEASE s`} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	// ...and ROLLBACK TO must genuinely undo, which is what the decline could
	// not do.
	for _, s := range []string{`SAVEPOINT s`, `INSERT INTO t VALUES(2)`, `ROLLBACK TO s`, `RELEASE s`} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	// BEGIN/COMMIT/ROLLBACK still work alongside.
	for _, s := range []string{`BEGIN`, `INSERT INTO t VALUES(3)`, `COMMIT`} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count(t) = %d, want 2 (the ROLLBACK TO must have discarded row 2)", n)
	}
}
