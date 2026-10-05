package compat

// Trigger bodies must reach OLD./NEW. rows in all contexts, including
// virtual-table write paths and table-valued functions.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestR35BTriggerOldRowReachesVtabDelete checks OLD row access in rtree DELETE.
func TestR35BTriggerOldRowReachesVtabDelete(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b)",
		"CREATE VIRTUAL TABLE t2 USING rtree(id,x0,x1)",
		"INSERT INTO t1 VALUES(1,'apple'),(2,'fig'),(3,'pear')",
		"INSERT INTO t2 VALUES(1,1.0,2.0),(2,2.0,3.0),(3,1.5,3.5)",
		"CREATE TRIGGER r1 AFTER UPDATE ON t1 BEGIN DELETE FROM t2 WHERE id = OLD.a; END",
		"ALTER TABLE t1 RENAME TO t3",
		"UPDATE t3 SET b='peach' WHERE a=2",
		"SELECT * FROM t2 ORDER BY 1",
	}
	if !differ(t, "r35b/trigger-old-rtree-delete", stmts) {
		t.Errorf("a trigger body's DELETE from a virtual table must see OLD (engine/vtab_write.go: deleteVtab's outer)")
	}
}

// TestR35BTriggerOldRowReachesVtabUpdate checks OLD row access in rtree UPDATE.
func TestR35BTriggerOldRowReachesVtabUpdate(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b)",
		"CREATE VIRTUAL TABLE t2 USING rtree(id,x0,x1)",
		"INSERT INTO t1 VALUES(1,'apple'),(2,'fig'),(3,'pear')",
		"INSERT INTO t2 VALUES(1,1.0,2.0),(2,2.0,3.0),(3,1.5,3.5)",
		"CREATE TRIGGER r2 AFTER UPDATE ON t1 BEGIN UPDATE t2 SET x1=x1+OLD.a WHERE id = OLD.a; END",
		"UPDATE t1 SET b='peach' WHERE a=3",
		"SELECT * FROM t2 ORDER BY 1",
	}
	if !differ(t, "r35b/trigger-old-rtree-update", stmts) {
		t.Errorf("a trigger body's UPDATE of a virtual table must see OLD in both WHERE and SET (engine/vtab_write.go: updateVtab's outer)")
	}
}

// TestR35BTriggerNewRowReachesTableValuedFunc is attach.test 5.10: a body
// step's SELECT source is a table-valued function whose ARGUMENT names the
// trigger's NEW row. C SQLite codes that argument against the NEW registers
// like any other body expression, so it is constant for the invocation.
func TestR35BTriggerNewRowReachesTableValuedFunc(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t1(x)",
		"CREATE TABLE t2(a,b)",
		"CREATE TRIGGER x1 AFTER INSERT ON t1 BEGIN INSERT INTO t2(a,b) SELECT key, value FROM json_each(NEW.x); END",
		`INSERT INTO t1(x) VALUES('{"a":1}')`,
		`INSERT INTO t1(x) VALUES('[10,20,30]')`,
		"SELECT a,b FROM t2 ORDER BY rowid",
	}
	if !differ(t, "r35b/trigger-new-json-each", stmts) {
		t.Errorf("a trigger body's table-valued function argument must see NEW (engine/join.go: the errVtabArgNotConstant retry)")
	}
}

// TestR35BTriggerOldRowReachesTableValuedFunc is the DELETE-side twin, and also
// covers a body step that reads the function through a WHERE rather than
// straight out of its select list.
func TestR35BTriggerOldRowReachesTableValuedFunc(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t1(x)",
		"CREATE TABLE t2(a)",
		`INSERT INTO t1(x) VALUES('[1,2,3]'),('[4]')`,
		"CREATE TRIGGER x2 AFTER DELETE ON t1 BEGIN INSERT INTO t2(a) SELECT value FROM json_each(OLD.x) WHERE value > 1; END",
		"DELETE FROM t1",
		"SELECT a FROM t2 ORDER BY a",
	}
	if !differ(t, "r35b/trigger-old-json-each", stmts) {
		t.Errorf("a trigger body's table-valued function argument must see OLD (engine/join.go: the errVtabArgNotConstant retry)")
	}
}

// TestR35BTableValuedFuncArgUnknownColumnStillFails pins the boundary the retry
// must NOT cross. The retry gives a table-valued function whose argument cannot
// be evaluated HERE a columns-only resolution, which is right for a reference
// to the enclosing trigger row and would be WRONG -- an empty scan where an
// error belongs -- for a name that exists nowhere. Both engines must still
// refuse this one.
func TestR35BTableValuedFuncArgUnknownColumnStillFails(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t1(x)",
		"CREATE TABLE t2(a)",
		"CREATE TRIGGER x3 AFTER INSERT ON t1 BEGIN INSERT INTO t2(a) SELECT value FROM json_each(NEW.nosuchcol); END",
		`INSERT INTO t1(x) VALUES('[1,2]')`,
		"SELECT count(*) FROM t2",
	}
	if !differ(t, "r35b/tvf-arg-unknown-column", stmts) {
		t.Errorf("a table-valued function argument naming a column that exists nowhere must still be refused, not resolved as an empty scan")
	}
}
