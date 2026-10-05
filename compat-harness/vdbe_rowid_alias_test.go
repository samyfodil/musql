// Tests rowid/oid/_rowid_ pseudo-column aliases in INSERT column lists and
// UPDATE SET targets for changing and setting rowid values.
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

// execRowidCollisionBoth is execConflictBothAbortDefault's (vdbe_conflict_test.go)
// sibling for a plain (no OR-clause) rowid collision reached through a
// rowid/oid/_rowid_ column-list alias: this write path's default-ABORT
// INSERT path (insertRowFromValues) reports a raw rowid collision as
// "UNIQUE constraint failed: duplicate rowid N" (engine/insert_write.go),
// a PRE-EXISTING wording that already differs from C SQLite's own
// "UNIQUE constraint failed: t.rowid" for ANY duplicate-rowid INSERT --
// via an ordinary explicit INTEGER PRIMARY KEY value, not just a rowid
// alias -- so this is not something this scoped feature introduces or
// should fix. Both sides must still fail, and both must still be
// classified as the SAME constraint kind (a genuine data rejection, not a
// grammar/scope gap), exactly like execConflictBothAbortDefault's own bar.
func execRowidCollisionBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
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
	if !strings.Contains(engMsg, "UNIQUE constraint failed") || !strings.Contains(realMsg, "UNIQUE constraint failed") {
		t.Fatalf("Exec(%s): expected both sides to report a UNIQUE constraint violation:\n  engine: %q\n  C SQLite: %q", sqlText, engMsg, realMsg)
	}
}

// TestRowidAliasColumnList is the rowid/oid/_rowid_-in-column-list/SET
// conformance gate: for each page size, and for each VDBEMode (on/off), it
// drives the pure-Go engine writer and a live real-SQLite oracle connection
// through an identical script covering: a plain (no IPK) table naming all
// three alias spellings, NULL/text-numeric/non-numeric/non-integral-float
// values, a duplicate-alias column list (last-wins), order-independence,
// UPDATE SET on the alias, and a bad-type/collision error case; a table
// with a REAL column literally named "oid" (shadowing); and a table WITH an
// INTEGER PRIMARY KEY column, including naming BOTH its own declared name
// and an alias in the same list/reversed order (verified NOT to be an
// error -- see buildFullRow's own doc comment for the exact last-wins rule
// this reproduces) -- then verifies the resulting file's exact row content
// and PRAGMA integrity_check='ok' against C SQLite, and that a
// Close->OpenWrite reopen still resolves a rowid-alias INSERT correctly.
func TestRowidAliasColumnList(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					testRowidAliasScenario(t, pageSize)
				})
			}
		})
	}
}

func testRowidAliasScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("rowidalias_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection, so DDL/DML/schema queries all see the SAME in-memory schema (see execConflictBoth's doc comment, vdbe_conflict_test.go)

	// ---- t1: a plain rowid table (no INTEGER PRIMARY KEY at all). ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t1(x TEXT)`)
	for _, s := range []string{
		`INSERT INTO t1(rowid, x) VALUES(5, 'a')`,
		`INSERT INTO t1(oid, x) VALUES(6, 'b')`,
		`INSERT INTO t1(_rowid_, x) VALUES(7, 'c')`,
		`INSERT INTO t1(x, rowid) VALUES('d', 100)`,             // order-independent: the name, not the position, matters
		`INSERT INTO t1(rowid, x) VALUES(NULL, 'e')`,            // NULL: "unspecified" -- auto-assign, same as an omitted/NULL IPK value, NOT an error
		`INSERT INTO t1(rowid, rowid, x) VALUES(200, 300, 'f')`, // duplicate alias position: LAST-wins (300), unlike an ordinary duplicate column name
		`INSERT INTO t1(rowid, x) VALUES('55', 'g')`,            // TEXT numeric string coerced to the integer rowid 55, exactly like a real IPK column's own affinity would
	} {
		execConflictBoth(t, db, sdb, s)
	}
	execPlainBoth(t, db, sdb, `INSERT INTO t1(rowid, x) VALUES('abc', 'bad')`)          // non-numeric TEXT: "datatype mismatch"
	execPlainBoth(t, db, sdb, `INSERT INTO t1(rowid, x) VALUES(5.5, 'bad2')`)           // non-integral REAL: "datatype mismatch"
	execRowidCollisionBoth(t, db, sdb, `INSERT INTO t1(rowid, x) VALUES(5, 'collide')`) // rowid 5 already taken (row 'a')

	execConflictBoth(t, db, sdb, `UPDATE t1 SET rowid = 900 WHERE x = 'a'`)
	execPlainBoth(t, db, sdb, `UPDATE t1 SET rowid = 'zzz' WHERE x = 'b'`) // "datatype mismatch"
	execPlainBoth(t, db, sdb, `UPDATE t1 SET rowid = NULL WHERE x = 'b'`)  // UPDATE, unlike INSERT, has no "auto-assign" concept: NULL is rejected too -- "datatype mismatch"

	// ---- t2: a REAL column literally named "oid" always shadows the
	// pseudo-column of the same name (checked first); rowid/_rowid_,
	// not being shadowed, still resolve to the actual rowid. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t2(oid TEXT, v INTEGER)`)
	execConflictBoth(t, db, sdb, `INSERT INTO t2(oid, v) VALUES('hello', 1)`) // real column: stores the TEXT value, auto-assigned rowid
	execConflictBoth(t, db, sdb, `INSERT INTO t2(rowid, v) VALUES(50, 2)`)    // pseudo rowid: sets the actual rowid to 50
	execConflictBoth(t, db, sdb, `UPDATE t2 SET rowid = 999 WHERE v = 1`)

	// ---- t3: a table WITH an INTEGER PRIMARY KEY column -- rowid/oid/
	// _rowid_ resolve to the SAME slot the IPK column's own declared name
	// ("id") would, including the LAST-wins interaction when both a rowid
	// alias AND "id" itself are named in the same list (verified directly
	// against C SQLite: this is NOT an error). ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t3(id INTEGER PRIMARY KEY, x TEXT)`)
	for _, s := range []string{
		`INSERT INTO t3(rowid, x) VALUES(42, 'a')`,
		`INSERT INTO t3(oid, x) VALUES(43, 'b')`,
		`INSERT INTO t3(id, rowid, x) VALUES(1, 2, 'c')`, // "id" first, "rowid" last -> the LAST-named position (2) wins
		`INSERT INTO t3(rowid, id, x) VALUES(4, 3, 'd')`, // reversed: "rowid" first, "id" last -> the LAST-named position (3) wins
	} {
		execConflictBoth(t, db, sdb, s)
	}
	execConflictBoth(t, db, sdb, `UPDATE t3 SET rowid = 500 WHERE x = 'a'`)         // moves 42 -> 500
	execConflictBoth(t, db, sdb, `UPDATE t3 SET id = 10, rowid = 20 WHERE x = 'b'`) // SET list is sequential, last-wins too: 20
	execConflictBoth(t, db, sdb, `UPDATE t3 SET rowid = 30, id = 40 WHERE x = 'c'`) // reversed: 40

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

	verifyTableViaCSQLite(t, fdb, wvTable{name: "t1", colList: "x", rows: []wvRow{
		{rowid: 6, cols: []any{"b"}},
		{rowid: 7, cols: []any{"c"}},
		{rowid: 55, cols: []any{"g"}},
		{rowid: 100, cols: []any{"d"}},
		{rowid: 101, cols: []any{"e"}},
		{rowid: 300, cols: []any{"f"}},
		{rowid: 900, cols: []any{"a"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t2", colList: "oid,v", rows: []wvRow{
		{rowid: 50, cols: []any{nil, int64(2)}},
		{rowid: 999, cols: []any{"hello", int64(1)}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t3", colList: "id,x", rows: []wvRow{
		{rowid: 3, cols: []any{int64(3), "d"}},
		{rowid: 20, cols: []any{int64(20), "b"}},
		{rowid: 40, cols: []any{int64(40), "c"}},
		{rowid: 500, cols: []any{int64(500), "a"}},
	}})
	if err := fdb.Close(); err != nil {
		t.Fatalf("close fdb: %v", err)
	}

	// ---- Close -> reopen: rowid-alias resolution still works correctly
	// against a table recovered from an existing file, not just one built
	// fresh this session. ----
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	execConflictBoth(t, db2, sdb, `INSERT INTO t1(rowid, x) VALUES(1000, 'reopened')`)
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
	verifyTableViaCSQLite(t, fdb2, wvTable{name: "t1", colList: "x", rows: []wvRow{
		{rowid: 6, cols: []any{"b"}},
		{rowid: 7, cols: []any{"c"}},
		{rowid: 55, cols: []any{"g"}},
		{rowid: 100, cols: []any{"d"}},
		{rowid: 101, cols: []any{"e"}},
		{rowid: 300, cols: []any{"f"}},
		{rowid: 900, cols: []any{"a"}},
		{rowid: 1000, cols: []any{"reopened"}},
	}})
}
