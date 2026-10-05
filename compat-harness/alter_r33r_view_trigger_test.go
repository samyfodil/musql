package compat

import "testing"

// TestR33RAlterRenameViewTriggerColumns verifies ALTER TABLE's cascading
// effects into INSTEAD OF triggers on views when column names change.
func TestR33RAlterRenameViewTriggerColumns(t *testing.T) {
	differ(t, "r33r-view-trigger-rename-col", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a, b FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a, new.b); END`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
	})
	// SELECT * case.
	differ(t, "r33r-view-trigger-rename-star", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT * FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a, new.b); END`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
	})
	// WHEN clause case.
	differ(t, "r33r-view-trigger-rename-when", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a, b FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 WHEN new.a>0 BEGIN INSERT INTO t1(b) VALUES(new.b); END`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
	})
	// MAIN table rename affects TEMP view trigger.
	differ(t, "r33r-view-trigger-rename-temp", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TEMP VIEW v1 AS SELECT a, b FROM t1`,
		`CREATE TEMP TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a, new.b); END`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
	})
	// DROP COLUMN case.
	differ(t, "r33r-view-trigger-drop-col", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT * FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1(b) VALUES(new.a); END`,
		`ALTER TABLE t1 DROP COLUMN a`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
		`INSERT INTO t1 VALUES(3)`,
		`SELECT b FROM t1`,
	})

	// Legal cases that succeed:
	differ(t, "r33r-view-trigger-rename-untouched-col", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a, b FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1(b) VALUES(new.b); END`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
	})
	// TABLE RENAME TO case.
	differ(t, "r33r-view-trigger-rename-table", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT * FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a, new.b); END`,
		`ALTER TABLE t1 RENAME TO t9`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
	})
	// Aliased result column case.
	differ(t, "r33r-view-trigger-rename-aliased", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a AS a, b FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a, new.b); END`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`INSERT INTO v1 VALUES(1,2)`,
		`SELECT aaa, b FROM t1`,
	})
	// ...and so does an explicit "CREATE VIEW v1(a,b)" column list, which is
	// Table.pCheck for a view and which renameColumnFunc's view branch
	// (alter.c:1584) never walks.
	differ(t, "r33r-view-trigger-rename-collist", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1(a, b) AS SELECT a, b FROM t1`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a, new.b); END`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`INSERT INTO v1 VALUES(5,6)`,
		`SELECT aaa, b FROM t1`,
	})
}

// TestR33RDropColumnTriggerBody pins the same post-cascade pass for an
// ordinary TABLE trigger whose BODY -- not its OLD/NEW -- names the column
// being dropped.
//
// renameResolveTrigger sqlite3SelectPrep's every step's SELECT (alter.c:1367)
// and resolves each step's WHERE and expression list (alter.c:1415-1419)
// against that step's own SrcList, so a body reading a column the ALTER just
// removed fails the whole ALTER. This engine ran its equivalent check BEFORE
// the drop, where the column still existed, and so PERFORMED the ALTER: the
// readout is not the schema text but the table itself, which came back one
// column narrower with the trigger still firing against the old shape.
func TestR33RDropColumnTriggerBody(t *testing.T) {
	differ(t, "r33r-drop-col-body-select", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE fire(x, y)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN INSERT INTO t1(a,b) SELECT a, b FROM t1 WHERE a=new.x; END`,
		`ALTER TABLE t1 DROP COLUMN b`,
		`INSERT INTO fire VALUES(1,2)`,
		`SELECT * FROM t1`,
	})
	// A join alias in the source, so the reference is qualified.
	differ(t, "r33r-drop-col-body-qualified", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE fire(x, y)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN INSERT INTO t1(a,b) SELECT x.a, x.b FROM t1 AS x; END`,
		`ALTER TABLE t1 DROP COLUMN b`,
		`INSERT INTO fire VALUES(1,2)`,
		`SELECT * FROM t1`,
	})
	// A TEMP trigger over a main table takes the same pass.
	differ(t, "r33r-drop-col-body-temp", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE fire(x, y)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TEMP TRIGGER tr AFTER INSERT ON fire BEGIN INSERT INTO t1(a,b) SELECT a, b FROM t1; END`,
		`ALTER TABLE t1 DROP COLUMN b`,
		`INSERT INTO fire VALUES(1,2)`,
		`SELECT * FROM t1`,
	})
	// A body naming only the SURVIVING column keeps the ALTER legal, and the
	// trigger must still fire afterwards.
	differ(t, "r33r-drop-col-body-untouched", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE fire(x, y)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN INSERT INTO t1(a) SELECT a FROM t1; END`,
		`ALTER TABLE t1 DROP COLUMN b`,
		`INSERT INTO fire VALUES(1,2)`,
		`SELECT * FROM t1 ORDER BY rowid`,
	})
	// ...and a RENAME COLUMN, which REWRITES the body, must stay legal.
	differ(t, "r33r-rename-col-body-rewritten", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE fire(x, y)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN INSERT INTO t1(a,b) SELECT a, b FROM t1 WHERE a=new.x; END`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`INSERT INTO fire VALUES(1,2)`,
		`SELECT * FROM t1 ORDER BY rowid`,
	})
}

// TestR33RRenameColumnAliasPositions pins which tokens of a VIEW body an
// "ALTER TABLE ... RENAME COLUMN" may rewrite.
//
// renameColumnFunc's view branch (alter.c:1584) does exactly one thing --
// sqlite3WalkSelect with renameColumnExprCb (alter.c:1015) -- and that callback
// matches TK_COLUMN / TK_TRIGGER nodes only. A result-column ALIAS is an
// ExprList zEName, and the only place SQLite rewrites one is
// renameColumnElistNames (alter.c:1106), reached solely for a TRIGGER STEP
// whose target table is the altered table (alter.c:1654-1657) -- an UPDATE's
// SET list and an INSERT's column list, never a select-list alias.
//
// The readout is deliberately doubled: the stored SQL AND the view's own output
// column names, because renaming an alias silently renames the VIEW'S COLUMNS,
// which any plain query against the view then reports.
func TestR33RRenameColumnAliasPositions(t *testing.T) {
	differ(t, "r33r-alias-as-same-name", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a AS a, b FROM t1`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master WHERE name='v1'`,
		`SELECT * FROM v1`,
	})
	// An alias naming a DIFFERENT column: "b AS a" must keep its alias while
	// the separate "a AS b" has its SOURCE rewritten.
	differ(t, "r33r-alias-as-crossed", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT b AS a, a AS b FROM t1`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master WHERE name='v1'`,
		`SELECT * FROM v1`,
	})
	// CAST's type name is an AS position too, and the trailing alias is still
	// an alias.
	differ(t, "r33r-alias-as-cast", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT CAST(a AS TEXT) AS a, b FROM t1`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master WHERE name='v1'`,
		`SELECT * FROM v1`,
	})
	// A FROM-item alias is an AS position as well: the qualifier keeps the old
	// spelling (it names the ALIAS) while the column it qualifies is rewritten.
	differ(t, "r33r-alias-as-from", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a.a FROM t1 AS a`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master WHERE name='v1'`,
		`SELECT * FROM v1`,
	})
	// The declared column list survives untouched while the body is rewritten.
	differ(t, "r33r-alias-declared-collist", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1(a, b) AS SELECT a, b FROM t1`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master WHERE name='v1'`,
		`SELECT * FROM v1`,
	})
}
