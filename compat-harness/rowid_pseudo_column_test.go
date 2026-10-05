package compat

// Tests the implicit rowid/oid/_rowid_ pseudo-column on ordinary tables.

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// rowidCase executes statements on both engines and compares results.
func rowidCase(t *testing.T, name string, stmts []string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		goPath := filepath.Join(t.TempDir(), "go.db")
		cgoPath := filepath.Join(t.TempDir(), "cgo.db")
		godb, err := engine.Create(goPath)
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		defer godb.Discard()
		cgodb, err := sql.Open("sqlite3", cgoPath)
		if err != nil {
			t.Fatalf("sql.Open(sqlite3): %v", err)
		}
		defer cgodb.Close()

		for _, stmt := range stmts {
			if tclIsQuery(stmt) {
				gotCols, gotRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
				if panicked {
					t.Fatalf("PANIC evaluating %q: %v", stmt, panicVal)
				}
				if qerr != nil {
					t.Fatalf("engine query %q: %v", stmt, qerr)
				}
				wantCols, wantRows, werr := tclRunCGOQuery(cgodb, stmt)
				if werr != nil {
					t.Fatalf("cgo query %q: %v", stmt, werr)
				}
				if ok, reason := queryResultsMatch(gotCols, gotRows, wantCols, wantRows, true); !ok {
					t.Errorf("%q: MISMATCH: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						stmt, reason, gotCols, gotRows, wantCols, wantRows)
				}
				continue
			}
			execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt)
			if panicked {
				t.Fatalf("PANIC executing %q: %v", stmt, panicVal)
			}
			if execErr != nil {
				t.Fatalf("engine exec %q: %v", stmt, execErr)
			}
			if _, err := cgodb.Exec(stmt); err != nil {
				t.Fatalf("cgo exec %q: %v", stmt, err)
			}
		}
	})
}

// TestRowidPseudoColumn exercises exactly the scenarios called out in the
// feature's task description: a plain (no INTEGER PRIMARY KEY) table's
// rowid/oid/_rowid_ in the SELECT list, WHERE, ORDER BY, and a JOIN...ON; an
// INTEGER PRIMARY KEY table (rowid aliases that column); and the shadowing
// rule (a real column named oid/rowid always wins over the pseudo-column of
// the same name, on a per-table, per-name basis).
func TestRowidPseudoColumn(t *testing.T) {
	rowidCase(t, "no_ipk_basic", []string{
		"CREATE TABLE t(a INTEGER, b TEXT)",
		"INSERT INTO t VALUES(10,'x'),(20,'y'),(30,'z')",
		"SELECT rowid, a, b FROM t",
		"SELECT rowid, a FROM t WHERE rowid = 2",
		"SELECT a FROM t ORDER BY rowid DESC",
		"SELECT oid, a FROM t",
		"SELECT _rowid_, a FROM t",
		"SELECT * FROM t", // "*" must NOT include rowid
		"SELECT max(rowid), min(rowid), count(rowid), count(*) FROM t",
		"SELECT rowid, count(*) FROM t GROUP BY rowid",
		"SELECT rowid, count(*) FROM t GROUP BY rowid HAVING rowid > 1",
	})

	rowidCase(t, "no_ipk_join", []string{
		"CREATE TABLE t1(a INTEGER)",
		"CREATE TABLE t2(b INTEGER)",
		"INSERT INTO t1 VALUES(100),(200),(300)",
		"INSERT INTO t2 VALUES(1),(2),(3)",
		// t1.rowid=1,2,3 (ascending insert order); t2.b+... links back to t1.rowid.
		"SELECT t1.rowid, t1.a, t2.rowid, t2.b FROM t1 JOIN t2 ON t1.rowid = t2.b",
		"SELECT t1.rowid, t1.a, t2.rowid, t2.b FROM t1 LEFT JOIN t2 ON t1.rowid = t2.b + 100",
		"SELECT t1.a, (SELECT t2.b FROM t2 WHERE t2.rowid = t1.rowid) FROM t1",
	})

	rowidCase(t, "ipk_alias", []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO t VALUES(5,'five'),(9,'nine')",
		"SELECT rowid, id, v FROM t", // rowid == id everywhere
		"SELECT id FROM t WHERE rowid = 9",
		"SELECT id, v FROM t ORDER BY rowid DESC",
		"UPDATE t SET v = 'updated' WHERE rowid = 5",
		"SELECT id, v FROM t WHERE rowid = 5",
		"DELETE FROM t WHERE rowid = 9",
		"SELECT rowid, id FROM t",
	})

	rowidCase(t, "shadowing_oid_column", []string{
		"CREATE TABLE t(oid TEXT, v INTEGER)",
		"INSERT INTO t VALUES('alpha', 1),('beta', 2)",
		"SELECT oid, v FROM t",     // real column: the TEXT values, not the rowid
		"SELECT rowid, v FROM t",   // pseudo rowid: oid is shadowed, but rowid/_rowid_ aren't
		"SELECT _rowid_, v FROM t", // pseudo rowid too
		"SELECT rowid, oid, v FROM t WHERE rowid = 1",
	})

	rowidCase(t, "shadowing_rowid_column", []string{
		"CREATE TABLE t(rowid TEXT, v INTEGER)",
		"INSERT INTO t VALUES('r1', 1),('r2', 2)",
		"SELECT rowid, v FROM t",   // real column: rowid itself is shadowed here
		"SELECT oid, v FROM t",     // pseudo rowid: rowid is shadowed, but oid/_rowid_ aren't
		"SELECT _rowid_, v FROM t", // pseudo rowid too
	})
}
