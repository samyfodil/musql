package compat

import "testing"

// Tests that trigger bodies may write into virtual tables, and that
// NEW/OLD references work correctly in virtual table INSERTs. This pattern
// is commonly used to keep FTS indexes in sync with content tables.
func TestR33RTriggerBodyWritesVtab(t *testing.T) {
	differ(t, "r33r-trigger-inserts-into-fts4", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`CREATE VIRTUAL TABLE t USING fts4(a, b)`,
		`CREATE TRIGGER src_ai AFTER INSERT ON src BEGIN INSERT INTO t(rowid,a,b) VALUES(new.id,new.a,new.b); END`,
		`INSERT INTO src VALUES(1,'hello world','x')`,
		`INSERT INTO src VALUES(2,'goodbye world','y')`,
		`SELECT rowid, a, b FROM t ORDER BY rowid`,
		`SELECT rowid FROM t WHERE t MATCH 'world' ORDER BY rowid`,
	})
	// fts3 is the same module family with a different name.
	differ(t, "r33r-trigger-inserts-into-fts3", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a)`,
		`CREATE VIRTUAL TABLE t USING fts3(a)`,
		`CREATE TRIGGER src_ai AFTER INSERT ON src BEGIN INSERT INTO t(docid,a) VALUES(new.id,new.a); END`,
		`INSERT INTO src VALUES(7,'alpha beta')`,
		`SELECT docid, a FROM t`,
	})
	// A WHEN clause gating the body, and a body reading OLD on an UPDATE
	// trigger, both go through the same value path.
	differ(t, "r33r-trigger-when-gates-vtab-insert", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a)`,
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`CREATE TRIGGER src_au AFTER UPDATE ON src WHEN new.a IS NOT NULL BEGIN INSERT INTO t(rowid,a) VALUES(new.id, old.a || '/' || new.a); END`,
		`INSERT INTO src VALUES(1,'one')`,
		`UPDATE src SET a='two' WHERE id=1`,
		`SELECT rowid, a FROM t`,
	})
	// A body naming a table that genuinely does not exist must still fail, and
	// fail on the TRIGGERING statement even when it matches zero rows.
	differ(t, "r33r-trigger-body-missing-table-still-fails", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a)`,
		`CREATE TRIGGER src_ai AFTER INSERT ON src BEGIN INSERT INTO nosuchvtab(a) VALUES(new.a); END`,
		`INSERT INTO src VALUES(1,'x')`,
		`SELECT count(*) FROM src`,
	})
	// An rtree body target, for a module that is not fts at all.
	differ(t, "r33r-trigger-inserts-into-rtree", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, x0 REAL, x1 REAL)`,
		`CREATE VIRTUAL TABLE r USING rtree(id, minx, maxx)`,
		`CREATE TRIGGER src_ai AFTER INSERT ON src BEGIN INSERT INTO r VALUES(new.id, new.x0, new.x1); END`,
		`INSERT INTO src VALUES(1, 0.0, 1.0)`,
		`INSERT INTO src VALUES(2, 5.0, 6.0)`,
		`SELECT id, minx, maxx FROM r ORDER BY id`,
	})
}
