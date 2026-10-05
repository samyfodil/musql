// Differential gate for CREATE/DROP TRIGGER conformance: AFTER/BEFORE row
// triggers for INSERT/UPDATE[OF col-list]/DELETE, WHEN clauses, OLD/NEW
// references, firing order, DROP TRIGGER, DROP TABLE cascading, and persistence
// across Close -> OpenWrite -> fire.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// TestTriggerHandlingMatchesCSQLite drives the pure-Go engine and a live
// SQLite oracle through identical scripts testing: multiple AFTER INSERT
// triggers in reverse creation order; WHEN-gated BEFORE INSERT; UPDATE OF
// col-list with WHEN comparing OLD/NEW; BEFORE DELETE reading OLD; DROP
// TRIGGER; DROP TABLE cascading triggers; and persistence across Close ->
// OpenWrite -> fire. It verifies exact row content and integrity.
func TestTriggerHandlingMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					testTriggerScenario(t, pageSize)
				})
			}
		})
	}
}

func testTriggerScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("trigger_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection

	execPlainBoth(t, db, sdb, `CREATE TABLE t1(a INTEGER, b INTEGER)`)
	execPlainBoth(t, db, sdb, `CREATE TABLE log(msg TEXT)`)
	execPlainBoth(t, db, sdb, `CREATE TABLE t2(x INTEGER)`)

	// Multiple AFTER INSERT triggers fire in reverse creation order.
	execPlainBoth(t, db, sdb, `CREATE TRIGGER tr_ins_first AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('first-created:'||NEW.a); END`)
	execPlainBoth(t, db, sdb, `CREATE TRIGGER tr_ins_second AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('second-created:'||NEW.a); INSERT INTO t2 VALUES(NEW.a); END`)

	// WHEN-gated BEFORE INSERT: fires per-row before AFTER triggers.
	execPlainBoth(t, db, sdb, `CREATE TRIGGER tr_ins_before BEFORE INSERT ON t1 WHEN NEW.a > 1 BEGIN INSERT INTO log VALUES('before:'||NEW.a); END`)

	execConflictBoth(t, db, sdb, `INSERT INTO t1 VALUES(1,10),(2,20)`)

	// UPDATE OF col-list fires only when named column is in SET and WHEN condition holds.
	execPlainBoth(t, db, sdb, `CREATE TRIGGER tr_upd_b AFTER UPDATE OF b ON t1 WHEN NEW.b != OLD.b BEGIN INSERT INTO log VALUES('upd-b:'||OLD.b||'->'||NEW.b); END`)

	execConflictBoth(t, db, sdb, `UPDATE t1 SET b=99 WHERE a=1`) // b changes: fires
	execConflictBoth(t, db, sdb, `UPDATE t1 SET b=20 WHERE a=2`) // b assigned but UNCHANGED value: OF-list still applies, but WHEN (NEW.b!=OLD.b) suppresses it
	execConflictBoth(t, db, sdb, `UPDATE t1 SET a=a WHERE a=2`)  // b not in SET at all: OF-list itself excludes this trigger

	// BEFORE DELETE trigger reads pre-deletion values.
	execPlainBoth(t, db, sdb, `CREATE TRIGGER tr_del BEFORE DELETE ON t1 BEGIN INSERT INTO log VALUES('del:'||OLD.a||','||OLD.b); END`)
	execConflictBoth(t, db, sdb, `INSERT INTO t1 VALUES(3,30)`)
	execConflictBoth(t, db, sdb, `DELETE FROM t1 WHERE a=3`)

	// DROP TRIGGER stops it from firing.
	execPlainBoth(t, db, sdb, `DROP TRIGGER tr_upd_b`)
	execConflictBoth(t, db, sdb, `UPDATE t1 SET b=555 WHERE a=1`) // would have fired tr_upd_b before the DROP; must not log anything now

	// DROP TABLE cascades: all triggers on the dropped table are removed, verified
	// against the materialized file's sqlite_master via C SQLite.
	execPlainBoth(t, db, sdb, `DROP TABLE t1`)
	execPlainBoth(t, db, sdb, `CREATE TABLE t1(a INTEGER, b INTEGER)`)
	execConflictBoth(t, db, sdb, `INSERT INTO t1 VALUES(999,999)`) // no trigger left to fire

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	// Export the segment file to SQLite format for C SQLite to verify.
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
	assertNoTriggersOnTable(t, fdb, "t1")

	// Log table's exact row content proves firing order, WHEN gating, and OLD/NEW
	// values: per-row BEFORE/AFTER interleaving, not all-before-then-all-after.
	verifyTableViaCSQLite(t, fdb, wvTable{name: "log", colList: "msg", rows: []wvRow{
		{rowid: 1, cols: []any{"second-created:1"}}, // AFTER, row a=1 (no BEFORE: WHEN NEW.a>1 is false): most-recently-created (tr_ins_second) fires first
		{rowid: 2, cols: []any{"first-created:1"}},
		{rowid: 3, cols: []any{"before:2"}}, // BEFORE fires before AFTER for row a=2 (WHEN NEW.a>1 true)
		{rowid: 4, cols: []any{"second-created:2"}},
		{rowid: 5, cols: []any{"first-created:2"}},
		{rowid: 6, cols: []any{"upd-b:10->99"}}, // UPDATE t1 SET b=99 WHERE a=1
		// UPDATE ... b=20 WHERE a=2 (unchanged value) and SET a=a WHERE a=2 (b not in SET) log nothing
		{rowid: 7, cols: []any{"before:3"}}, // BEFORE fires again for the later INSERT row a=3 (WHEN NEW.a>1 still true)
		{rowid: 8, cols: []any{"second-created:3"}},
		{rowid: 9, cols: []any{"first-created:3"}},
		{rowid: 10, cols: []any{"del:3,30"}}, // BEFORE DELETE, row a=3
		// DROP TRIGGER tr_upd_b, then UPDATE b=555 WHERE a=1 logs nothing
		// DROP TABLE t1 (cascades all remaining triggers), recreate t1, INSERT logs nothing
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t2", colList: "x", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1)}},
		{rowid: 2, cols: []any{int64(2)}},
		{rowid: 3, cols: []any{int64(3)}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t1", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(999), int64(999)}},
	}})
	if err := fdb.Close(); err != nil {
		t.Fatalf("close fdb: %v", err)
	}

	// Close -> OpenWrite -> fire: triggers recovered from an existing file fire correctly.
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	execPlainBoth(t, db2, sdb, `CREATE TRIGGER tr_reopen AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('reopen:'||NEW.a); END`)
	if err := db2.Close(); err != nil {
		t.Fatalf("engine writer Close (registering tr_reopen): %v", err)
	}

	db3, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite (second reopen): %v", err)
	}
	execConflictBoth(t, db3, sdb, `INSERT INTO t1 VALUES(1000,1000)`)
	if err := db3.Close(); err != nil {
		t.Fatalf("engine writer Close (reopened): %v", err)
	}

	// Export and verify again after the second reopen.
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
	verifyTableViaCSQLite(t, fdb2, wvTable{name: "log", colList: "msg", rows: []wvRow{
		{rowid: 1, cols: []any{"second-created:1"}},
		{rowid: 2, cols: []any{"first-created:1"}},
		{rowid: 3, cols: []any{"before:2"}},
		{rowid: 4, cols: []any{"second-created:2"}},
		{rowid: 5, cols: []any{"first-created:2"}},
		{rowid: 6, cols: []any{"upd-b:10->99"}},
		{rowid: 7, cols: []any{"before:3"}},
		{rowid: 8, cols: []any{"second-created:3"}},
		{rowid: 9, cols: []any{"first-created:3"}},
		{rowid: 10, cols: []any{"del:3,30"}},
		{rowid: 11, cols: []any{"reopen:1000"}}, // the trigger recovered by OpenWrite fired on this session's own INSERT
	}})
	verifyTableViaCSQLite(t, fdb2, wvTable{name: "t1", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(999), int64(999)}},
		{rowid: 2, cols: []any{int64(1000), int64(1000)}},
	}})
}

// assertNoTriggersOnTable verifies that a DROP TABLE really did cascade every
// trigger registered against it into the rebuilt file, not just hide it.
func assertNoTriggersOnTable(t *testing.T, fdb *sql.DB, table string) {
	t.Helper()
	var count int
	if err := fdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND tbl_name=?`, table).Scan(&count); err != nil {
		t.Fatalf("counting triggers on %s: %v", table, err)
	}
	if count != 0 {
		t.Fatalf("table %s still has %d trigger(s) registered in the materialized file after DROP TABLE", table, count)
	}
}
