// Test WITHOUT ROWID table support for CREATE, INSERT, UPDATE, DELETE, and recovery.
// Verify behavior matches C SQLite including error cases and read order.
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

// assertQueryErrorMatches verifies both engines error on a query, with matching text.
func assertQueryErrorMatches(t *testing.T, rp *engine.ReadOnlyPager, sdb *sql.DB, query string) {
	t.Helper()
	_, _, engErr := rp.QueryArgs(query, nil)
	_, realErr := sdb.Query(query)
	if engErr == nil || realErr == nil {
		t.Fatalf("Query(%s): expected both sides to error, got engine=%v, C SQLite=%v", query, engErr, realErr)
	}
	realMsg := realErr.Error()

	_, _, vdbeErr := rp.QueryArgs(query, nil)
	if vdbeErr == nil {
		t.Fatalf("Query(%s): expected QueryVDBE to also decline, got nil", query)
	}
	vdbeMsg := vdbeErr.Error()
	for _, prefix := range []string{"vdbe: unsupported: ", "vdbe: semantic error: ", "engine: "} {
		if strings.HasPrefix(vdbeMsg, prefix) {
			vdbeMsg = strings.TrimPrefix(vdbeMsg, prefix)
			break
		}
	}
	if vdbeMsg != realMsg {
		t.Fatalf("Query(%s): error text mismatch:\n  QueryVDBE (stripped): %q\n  C SQLite:          %q", query, vdbeMsg, realMsg)
	}
}

// wrLiveSnapshot returns a ReadOnlyPager over the current uncommitted content.
func wrLiveSnapshot(t *testing.T, db *engine.Session) *engine.ReadOnlyPager {
	t.Helper()
	rp, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("db.SnapshotPager: %v", err)
	}
	return rp
}

// verifyWithoutRowidTableViaCSQLite verifies WITHOUT ROWID table content and order match expected.
func verifyWithoutRowidTableViaCSQLite(t *testing.T, db *sql.DB, name, colList string, want [][]any) {
	t.Helper()
	q := fmt.Sprintf("SELECT %s FROM %s", colList, name)
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	defer rows.Close()

	nCol := len(strings.Split(colList, ","))
	var got [][]any
	for rows.Next() {
		dest := make([]any, nCol)
		ptrs := make([]any, nCol)
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", name, err)
		}
		got = append(got, dest)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows, want %d (got=%v want=%v)", name, len(got), len(want), got, want)
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s: row %d width: got %v, want %v", name, i, got[i], want[i])
		}
		for c := range got[i] {
			if !wvValueEqual(got[i][c], want[i][c]) {
				t.Errorf("%s: row %d col %d: got %#v, want %#v", name, i, c, got[i][c], want[i][c])
			}
		}
	}
}

func TestWithoutRowidMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					testWithoutRowidScenario(t, pageSize)
				})
			}
		})
	}
}

func testWithoutRowidScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("withoutrowid_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection -- see execConflictBoth's doc comment

	// Table t1: single-column INTEGER PRIMARY KEY WITHOUT ROWID.
	execPlainBoth(t, db, sdb, `CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT, c TEXT) WITHOUT ROWID`)
	for _, s := range []string{
		`INSERT INTO t1 VALUES(5,'hello','world')`,
		`INSERT INTO t1 VALUES(1,'aaa','bbb')`,
		`INSERT INTO t1 VALUES(3,'ccc','ddd')`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// Duplicate PRIMARY KEY constraint violation.
	execConflictBothAbortDefault(t, db, sdb, `INSERT INTO t1 VALUES(5,'dup','dup')`)

	execConflictBoth(t, db, sdb, `UPDATE t1 SET b='updated' WHERE a=3`)
	execConflictBoth(t, db, sdb, `DELETE FROM t1 WHERE a=1`)
	execConflictBoth(t, db, sdb, `INSERT INTO t1 VALUES(7,'foo','bar')`)

	// rowid/oid/_rowid_ are not valid pseudo-columns on a WITHOUT ROWID table.
	rp := wrLiveSnapshot(t, db)
	for _, col := range []string{"rowid", "oid", "_rowid_"} {
		assertQueryErrorMatches(t, rp, sdb, fmt.Sprintf("SELECT %s FROM t1", col))
	}
	// Verify PRAGMA table_info matches the oracle.
	assertPragmaEqual(t, rp, sdb, `PRAGMA table_info(t1)`)

	// Table t2: multi-column PRIMARY KEY WITHOUT ROWID with secondary index.
	execPlainBoth(t, db, sdb, `CREATE TABLE t2(x TEXT, y INTEGER, z TEXT, PRIMARY KEY(y,x)) WITHOUT ROWID`)
	execPlainBoth(t, db, sdb, `CREATE INDEX idx_t2z ON t2(z)`)
	for _, s := range []string{
		`INSERT INTO t2 VALUES('m',2,'n')`,
		`INSERT INTO t2 VALUES('a',1,'p')`,
		`INSERT INTO t2 VALUES('b',1,'q')`,
	} {
		execConflictBoth(t, db, sdb, s)
	}
	// Duplicate PRIMARY KEY tuple.
	execConflictBothAbortDefault(t, db, sdb, `INSERT INTO t2 VALUES('a',1,'zz')`)
	execConflictBoth(t, db, sdb, `UPDATE t2 SET z='q2' WHERE y=1 AND x='b'`)
	execConflictBoth(t, db, sdb, `DELETE FROM t2 WHERE y=2`)
	execConflictBoth(t, db, sdb, `INSERT INTO t2 VALUES('c',3,'r')`)

	rp = wrLiveSnapshot(t, db)
	assertQueryErrorMatches(t, rp, sdb, `SELECT rowid FROM t2`)
	assertPragmaEqual(t, rp, sdb, `PRAGMA table_info(t2)`)
	assertPragmaEqual(t, rp, sdb, `PRAGMA index_list(t2)`)

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	// Export and verify via the oracle using C SQLite.
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

	// Verify tables match expected content and PRIMARY KEY order.
	verifyWithoutRowidTableViaCSQLite(t, fdb, "t1", "a,b,c", [][]any{
		{int64(3), "updated", "ddd"},
		{int64(5), "hello", "world"},
		{int64(7), "foo", "bar"},
	})
	verifyWithoutRowidTableViaCSQLite(t, fdb, "t2", "y,x,z", [][]any{
		{int64(1), "a", "p"},
		{int64(1), "b", "q2"},
		{int64(3), "c", "r"},
	})
	if err := fdb.Close(); err != nil {
		t.Fatalf("close fdb: %v", err)
	}

	// Reopen and test INSERT/UPDATE/DELETE on recovered WITHOUT ROWID table.
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	execConflictBoth(t, db2, sdb, `INSERT INTO t1 VALUES(2,'reopened','row')`)
	// Duplicate PRIMARY KEY that already exists in the recovered table.
	execConflictBothAbortDefault(t, db2, sdb, `INSERT INTO t1 VALUES(3,'dup-after-reopen','x')`)
	if err := db2.Close(); err != nil {
		t.Fatalf("engine writer Close (reopened): %v", err)
	}

	// Export again and verify after reopening.
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
	verifyWithoutRowidTableViaCSQLite(t, fdb2, "t1", "a,b,c", [][]any{
		{int64(2), "reopened", "row"},
		{int64(3), "updated", "ddd"},
		{int64(5), "hello", "world"},
		{int64(7), "foo", "bar"},
	})
}
