// Tests the database/sql driver against the oracle using identical SQL programs.
// comment (driver/driver.go) for the autocommit-vs-transaction execution
// model this proves out.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// openPair opens a fresh, empty database file for each driver (distinct
// paths -- an engine sees no reason to know or care that another engine
// exists) and returns both live *sql.DB handles, closed automatically at
// test cleanup.
func openPair(t *testing.T, name string) (pureDB, mattnDB *sql.DB, purePath, mattnPath string) {
	t.Helper()
	dir := t.TempDir()
	purePath = filepath.Join(dir, name+".pure.sqlite")
	mattnPath = filepath.Join(dir, name+".mattn.sqlite")

	var err error
	pureDB, err = sql.Open(driver.DriverName, purePath)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", driver.DriverName, err)
	}
	t.Cleanup(func() { pureDB.Close() })

	mattnDB, err = sql.Open("sqlite3", mattnPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	t.Cleanup(func() { mattnDB.Close() })

	return pureDB, mattnDB, purePath, mattnPath
}

// execBoth runs query with args (IDENTICAL SQL text and IDENTICAL Go
// argument values on both sides -- both drivers go through database/sql's
// own DefaultParameterConverter, so there is no dual-argument translation
// needed here the way param_diff_test.go's lower-level engine.Value API
// requires) through both db handles and requires RowsAffected/LastInsertId
// to agree, returning the pure driver's Result for callers that want to
// assert a specific value too.
func execBoth(t *testing.T, pureDB, mattnDB *sql.DB, label, query string, args ...any) sql.Result {
	t.Helper()
	pureRes, err := pureDB.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: pure Exec(%q, %v): %v", label, query, args, err)
	}
	mattnRes, err := mattnDB.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: mattn Exec(%q, %v): %v", label, query, args, err)
	}
	pRA, err := pureRes.RowsAffected()
	if err != nil {
		t.Fatalf("%s: pure RowsAffected: %v", label, err)
	}
	mRA, err := mattnRes.RowsAffected()
	if err != nil {
		t.Fatalf("%s: mattn RowsAffected: %v", label, err)
	}
	if pRA != mRA {
		t.Errorf("%s: RowsAffected diverges: pure=%d mattn=%d (sql=%q args=%v)", label, pRA, mRA, query, args)
	}
	pLI, err := pureRes.LastInsertId()
	if err != nil {
		t.Fatalf("%s: pure LastInsertId: %v", label, err)
	}
	mLI, err := mattnRes.LastInsertId()
	if err != nil {
		t.Fatalf("%s: mattn LastInsertId: %v", label, err)
	}
	if pLI != mLI {
		t.Errorf("%s: LastInsertId diverges: pure=%d mattn=%d (sql=%q args=%v)", label, pLI, mLI, query, args)
	}
	return pureRes
}

// collectRows scans every row/column of rows into `any` (database/sql's own
// convertAssign passes a driver.Value straight through for an `any`
// destination) and normalizes each cell via normalizeAny (param_diff_test.go)
// into the same storage-class-tagged string scheme normalizeEngineValue
// uses, so pure-driver and mattn result sets -- scanned through completely
// ordinary database/sql code, no engine-internal types involved on either
// side -- can be compared cell-for-cell via queryResultsMatch
// (pureengine_test.go).
func collectRows(t *testing.T, rows *sql.Rows) (cols []string, out [][]string) {
	t.Helper()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = normalizeAny(v)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return cols, out
}

// queryBoth runs query with args identically against both db handles and
// requires the normalized result sets to match exactly (order-sensitive:
// every query below carries its own ORDER BY).
func queryBoth(t *testing.T, pureDB, mattnDB *sql.DB, label, query string, args ...any) {
	t.Helper()
	pRows, err := pureDB.Query(query, args...)
	if err != nil {
		t.Fatalf("%s: pure Query(%q, %v): %v", label, query, args, err)
	}
	defer pRows.Close()
	pCols, pOut := collectRows(t, pRows)

	mRows, err := mattnDB.Query(query, args...)
	if err != nil {
		t.Fatalf("%s: mattn Query(%q, %v): %v", label, query, args, err)
	}
	defer mRows.Close()
	mCols, mOut := collectRows(t, mRows)

	if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
		t.Errorf("%s: DIVERGES from mattn\n  sql:   %s\n  args:  %v\n  reason: %s\n  pure:  cols=%v rows=%v\n  mattn: cols=%v rows=%v",
			label, query, args, reason, pCols, pOut, mCols, mOut)
	}
}

// TestPureDriverExecQueryParams is the driver's core CRUD gate: CREATE
// TABLE, parameterized INSERT via both db.Exec and a prepared+reused
// db.Prepare/stmt.Exec, and parameterized SELECT via db.Query/rows.Scan --
// with RowsAffected/LastInsertId checked at every mutating step.
func TestPureDriverExecQueryParams(t *testing.T) {
	pureDB, mattnDB, _, _ := openPair(t, "crud")

	execBoth(t, pureDB, mattnDB, "CREATE TABLE",
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, val REAL)")

	execBoth(t, pureDB, mattnDB, "INSERT via db.Exec",
		"INSERT INTO t (name, val) VALUES (?, ?)", "a", 1.5)

	// Prepared statement, reused three times with different arguments --
	// exercises Stmt.NumInput/Exec being called repeatedly on the SAME
	// prepared object (see driver/stmt.go's Stmt, which caches its
	// ParamInfo once at Prepare time rather than re-parsing per Exec).
	pureStmt, err := pureDB.Prepare("INSERT INTO t (name, val) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("pure Prepare: %v", err)
	}
	defer pureStmt.Close()
	mattnStmt, err := mattnDB.Prepare("INSERT INTO t (name, val) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("mattn Prepare: %v", err)
	}
	defer mattnStmt.Close()

	for _, row := range []struct {
		name string
		val  float64
	}{
		{"b", 2.5},
		{"c", 3.5},
		{"d", 4.5},
	} {
		pRes, err := pureStmt.Exec(row.name, row.val)
		if err != nil {
			t.Fatalf("pure stmt.Exec(%v): %v", row, err)
		}
		mRes, err := mattnStmt.Exec(row.name, row.val)
		if err != nil {
			t.Fatalf("mattn stmt.Exec(%v): %v", row, err)
		}
		pRA, _ := pRes.RowsAffected()
		mRA, _ := mRes.RowsAffected()
		pLI, _ := pRes.LastInsertId()
		mLI, _ := mRes.LastInsertId()
		if pRA != mRA || pLI != mLI {
			t.Errorf("prepared insert %v: pure=(RA=%d LI=%d) mattn=(RA=%d LI=%d)", row, pRA, pLI, mRA, mLI)
		}
	}

	// Parameterized SELECT, via db.Query (not the prepared stmt) -- proves
	// Conn.Prepare/Stmt aren't the only path that binds correctly.
	queryBoth(t, pureDB, mattnDB, "SELECT with param",
		"SELECT id, name, val FROM t WHERE id >= ? ORDER BY id", int64(2))

	// Named parameters (sql.Named), mixed with the table's earlier rows.
	execBoth(t, pureDB, mattnDB, "UPDATE via sql.Named",
		"UPDATE t SET val = :v WHERE name = :n", sql.Named("v", 9.75), sql.Named("n", "a"))
	queryBoth(t, pureDB, mattnDB, "SELECT after named UPDATE",
		"SELECT id, name, val FROM t ORDER BY id")

	// A parameterized DELETE, to round out INSERT/UPDATE/DELETE all being
	// exercised through the same *sql.DB program.
	execBoth(t, pureDB, mattnDB, "DELETE via db.Exec",
		"DELETE FROM t WHERE name = ?", "b")
	queryBoth(t, pureDB, mattnDB, "SELECT after DELETE",
		"SELECT id, name, val FROM t ORDER BY id")
}

// TestPureDriverColumnTypeRoundTrip binds one value of every supported Go
// argument type (int64, float64, string, []byte, nil, bool) as a parameter,
// stores it, and reads it back via rows.Scan into the matching typed
// destination (including sql.NullString for the NULL case) -- proving the
// full engine.Value <-> driver.Value conversion round-trips identically to
// mattn on both the write and the read side.
func TestPureDriverColumnTypeRoundTrip(t *testing.T) {
	pureDB, mattnDB, _, _ := openPair(t, "types")

	execBoth(t, pureDB, mattnDB, "CREATE TABLE",
		"CREATE TABLE tt (i INTEGER, f REAL, s TEXT, b BLOB, n TEXT, flag INTEGER)")
	execBoth(t, pureDB, mattnDB, "INSERT every type",
		"INSERT INTO tt (i, f, s, b, n, flag) VALUES (?, ?, ?, ?, ?, ?)",
		int64(-42), float64(2.25), "hello, world", []byte{0xCA, 0xFE, 0x00, 0xBE}, nil, true)

	queryBoth(t, pureDB, mattnDB, "SELECT every type back",
		"SELECT i, f, s, b, n, flag FROM tt")

	// Typed Scan, on the pure driver alone (queryBoth above already proved
	// cell-for-cell equality via generic `any` scanning): confirms each
	// column also converts cleanly into the specific Go type a real program
	// would actually use, including sql.NullString for the NULL column.
	var i int64
	var f float64
	var s string
	var b []byte
	var n sql.NullString
	var flag int64
	if err := pureDB.QueryRow("SELECT i, f, s, b, n, flag FROM tt").Scan(&i, &f, &s, &b, &n, &flag); err != nil {
		t.Fatalf("typed Scan: %v", err)
	}
	if i != -42 || f != 2.25 || s != "hello, world" || string(b) != "\xca\xfe\x00\xbe" || n.Valid || flag != 1 {
		t.Errorf("typed Scan mismatch: i=%d f=%v s=%q b=%x n.Valid=%v flag=%d", i, f, s, b, n.Valid, flag)
	}
}

// TestPureDriverTransactionSemantics is the transaction-model gate: the
// SAME database/sql program (BeginTx, an INSERT, a read-your-writes SELECT
// still inside the transaction, Commit, a post-commit SELECT; then a SECOND
// transaction that INSERTs and Rollbacks instead) run through both drivers
// must agree at every checkpoint -- proving driver's rebuild-on-flush
// model (engine.DB.SnapshotPager for in-tx reads, Close for Commit, Discard
// for Rollback -- see driver/conn.go's doc comment) behaves exactly like
// an ordinary SQLite transaction from database/sql's point of view.
func TestPureDriverTransactionSemantics(t *testing.T) {
	pureDB, mattnDB, _, _ := openPair(t, "tx")

	execBoth(t, pureDB, mattnDB, "CREATE TABLE",
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	execBoth(t, pureDB, mattnDB, "seed row",
		"INSERT INTO t (name) VALUES (?)", "seed")

	pureTx, err := pureDB.Begin()
	if err != nil {
		t.Fatalf("pure Begin: %v", err)
	}
	mattnTx, err := mattnDB.Begin()
	if err != nil {
		t.Fatalf("mattn Begin: %v", err)
	}

	if _, err := pureTx.Exec("INSERT INTO t (name) VALUES (?)", "committed"); err != nil {
		t.Fatalf("pure tx.Exec: %v", err)
	}
	if _, err := mattnTx.Exec("INSERT INTO t (name) VALUES (?)", "committed"); err != nil {
		t.Fatalf("mattn tx.Exec: %v", err)
	}

	// Read-your-writes: a SELECT run on the SAME transaction must see the
	// just-inserted, still-uncommitted row.
	pRows, err := pureTx.Query("SELECT name FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("pure tx.Query: %v", err)
	}
	pCols, pOut := collectRows(t, pRows)
	pRows.Close()

	mRows, err := mattnTx.Query("SELECT name FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("mattn tx.Query: %v", err)
	}
	mCols, mOut := collectRows(t, mRows)
	mRows.Close()

	if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
		t.Errorf("in-tx read-your-writes DIVERGES: reason=%s\n  pure=%v\n  mattn=%v", reason, pOut, mOut)
	}
	if len(pOut) != 2 {
		t.Fatalf("in-tx SELECT: got %d rows, want 2 (seed + committed, uncommitted-but-visible)", len(pOut))
	}

	if err := pureTx.Commit(); err != nil {
		t.Fatalf("pure Commit: %v", err)
	}
	if err := mattnTx.Commit(); err != nil {
		t.Fatalf("mattn Commit: %v", err)
	}

	// Post-commit: the row must persist for a brand new, ordinary
	// (non-transactional) query.
	queryBoth(t, pureDB, mattnDB, "post-commit SELECT",
		"SELECT name FROM t ORDER BY id")

	// A second transaction that inserts, then Rollbacks: its row must NOT
	// persist.
	pureTx2, err := pureDB.Begin()
	if err != nil {
		t.Fatalf("pure Begin (2nd): %v", err)
	}
	mattnTx2, err := mattnDB.Begin()
	if err != nil {
		t.Fatalf("mattn Begin (2nd): %v", err)
	}
	if _, err := pureTx2.Exec("INSERT INTO t (name) VALUES (?)", "rolled-back"); err != nil {
		t.Fatalf("pure tx2.Exec: %v", err)
	}
	if _, err := mattnTx2.Exec("INSERT INTO t (name) VALUES (?)", "rolled-back"); err != nil {
		t.Fatalf("mattn tx2.Exec: %v", err)
	}
	if err := pureTx2.Rollback(); err != nil {
		t.Fatalf("pure Rollback: %v", err)
	}
	if err := mattnTx2.Rollback(); err != nil {
		t.Fatalf("mattn Rollback: %v", err)
	}

	queryBoth(t, pureDB, mattnDB, "post-rollback SELECT (must not include rolled-back row)",
		"SELECT name FROM t ORDER BY id")

	var count int
	if err := pureDB.QueryRow("SELECT count(*) FROM t WHERE name = ?", "rolled-back").Scan(&count); err != nil {
		t.Fatalf("post-rollback count query: %v", err)
	}
	if count != 0 {
		t.Fatalf("post-rollback: rolled-back row is still present (count=%d, want 0)", count)
	}
}

// TestPureDriverOnDiskCompatibility proves the pure driver's on-disk file is
// a byte-format-valid SQLite database, not merely internally self-consistent:
// after running a short program through the pure driver, C SQLite
// (mattn) must be able to open that EXACT file, report integrity_check =
// "ok", and read back the same rows an equivalent mattn-segment database
// produced from the identical program.
func TestPureDriverOnDiskCompatibility(t *testing.T) {
	pureDB, mattnDB, purePath, _ := openPair(t, "ondisk")

	execBoth(t, pureDB, mattnDB, "CREATE TABLE",
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, val REAL, note BLOB)")
	execBoth(t, pureDB, mattnDB, "INSERT rows",
		"INSERT INTO t (name, val, note) VALUES (?, ?, ?)", "a", 1.5, []byte{0x01, 0x02})
	execBoth(t, pureDB, mattnDB, "INSERT more rows",
		"INSERT INTO t (name, val, note) VALUES (?, ?, ?)", "b", 2.5, nil)
	execBoth(t, pureDB, mattnDB, "UPDATE",
		"UPDATE t SET val = ? WHERE name = ?", 99.0, "a")
	execBoth(t, pureDB, mattnDB, "DELETE",
		"DELETE FROM t WHERE name = ?", "b")

	// Close the pure driver's *sql.DB so every autocommit statement's
	// underlying engine.DB.Close has already durably flushed (it always has,
	// by the time Exec returns -- see driver/conn.go's execArgs -- this
	// Close just releases database/sql's own pooled *Conn).
	if err := pureDB.Close(); err != nil {
		t.Fatalf("pure db.Close: %v", err)
	}

	// C is handed the EXPORT: musql's own file is a segment (AGENTS.md Rule 3).
	verifyDB, err := sql.Open("sqlite3", exportedForOracle(t, purePath))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3) on pure-driver file: %v", err)
	}
	defer verifyDB.Close()

	var integrity string
	if err := verifyDB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check on pure-driver file = %q, want \"ok\"", integrity)
	}

	// Row-for-row comparison: mattn reading the PURE-DRIVER-WRITTEN file
	// must match mattn reading its OWN file, for the identical program.
	pRows, err := verifyDB.Query("SELECT id, name, val, note FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("mattn-on-pure-file Query: %v", err)
	}
	pCols, pOut := collectRows(t, pRows)
	pRows.Close()

	mRows, err := mattnDB.Query("SELECT id, name, val, note FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("mattn-native Query: %v", err)
	}
	mCols, mOut := collectRows(t, mRows)
	mRows.Close()

	if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
		t.Errorf("on-disk content DIVERGES: reason=%s\n  mattn-on-pure-file=%v\n  mattn-native=%v", reason, pOut, mOut)
	}
}
