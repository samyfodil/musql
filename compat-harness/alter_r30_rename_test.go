package compat

import "testing"

// Rename-cascade operations compared against C SQLite oracle.

// TestAlterR30RenameColumnToItself pins "ALTER TABLE t RENAME c TO c".
// Self-rename succeeds; collision fails with oracle's exact message.
func TestAlterR30RenameColumnToItself(t *testing.T) {
	differ(t, "alter-r30-rename-col-to-itself", []string{
		`CREATE TABLE Table0(Col0, Col1)`,
		`INSERT INTO Table0 VALUES(1, 2)`,
		`ALTER TABLE Table0 RENAME Col0 TO Col0`,
		`SELECT sql FROM sqlite_master WHERE name='Table0'`,
		`SELECT Col0, Col1 FROM Table0`,
	})
	// Case-only self-rename: still one column after the rewrite.
	differ(t, "alter-r30-rename-col-case-only", []string{
		`CREATE TABLE t1(aaa, bbb)`,
		`INSERT INTO t1 VALUES(7, 8)`,
		`ALTER TABLE t1 RENAME aaa TO AAA`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT AAA, bbb FROM t1`,
	})
	// A genuine collision must still fail, and fail identically.
	differ(t, "alter-r30-rename-col-real-collision", []string{
		`CREATE TABLE t2(a, b)`,
		`ALTER TABLE t2 RENAME b TO a`,
		`SELECT sql FROM sqlite_master WHERE name='t2'`,
	})
	// The collision must not be masked by an index/trigger cascade either.
	differ(t, "alter-r30-rename-col-self-with-index", []string{
		`CREATE TABLE t3(a, b)`,
		`CREATE INDEX i3 ON t3(b)`,
		`INSERT INTO t3 VALUES(1, 2)`,
		`ALTER TABLE t3 RENAME b TO b`,
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		`SELECT a FROM t3 WHERE b=2`,
	})
}

// TestAlterR30StringLiteralNames pins string literal names in ALTER statements.
// Quoting is observable in stored schema: different quote styles produce
// consistent double-quoted output in all dependent objects.
func TestAlterR30StringLiteralNames(t *testing.T) {
	differ(t, "alter-r30-rename-col-to-string", []string{
		`CREATE TABLE x1(one, two)`,
		`INSERT INTO x1 VALUES(1, 2)`,
		`ALTER TABLE x1 RENAME two TO 'four'`,
		`SELECT sql FROM sqlite_master WHERE name='x1'`,
		`SELECT one, four FROM x1`,
	})
	differ(t, "alter-r30-rename-col-from-string", []string{
		`CREATE TABLE x2(one, two)`,
		`INSERT INTO x2 VALUES(1, 2)`,
		`ALTER TABLE x2 RENAME 'two' TO three`,
		`SELECT sql FROM sqlite_master WHERE name='x2'`,
		`SELECT one, three FROM x2`,
	})
	for _, spelling := range []string{`"four"`, "[four]", "`four`"} {
		differ(t, "alter-r30-rename-col-quoted-"+spelling, []string{
			`CREATE TABLE x3(one, two)`,
			`INSERT INTO x3 VALUES(1, 2)`,
			`ALTER TABLE x3 RENAME two TO ` + spelling,
			`SELECT sql FROM sqlite_master WHERE name='x3'`,
			`SELECT one, four FROM x3`,
		})
	}
	differ(t, "alter-r30-rename-col-bare", []string{
		`CREATE TABLE x4(one, two)`,
		`ALTER TABLE x4 RENAME two TO four`,
		`SELECT sql FROM sqlite_master WHERE name='x4'`,
	})
	differ(t, "alter-r30-rename-col-quoted-cascade", []string{
		`CREATE TABLE x5(one, two)`,
		`CREATE INDEX x5i ON x5(two)`,
		`CREATE TRIGGER x5t AFTER UPDATE OF two ON x5 BEGIN SELECT new.two; END`,
		`CREATE VIEW x5v AS SELECT two FROM x5`,
		`INSERT INTO x5 VALUES(1, 2)`,
		`ALTER TABLE x5 RENAME two TO 'four'`,
		`SELECT * FROM x5v`,
		`UPDATE x5 SET "four"=9`,
		`SELECT one, "four" FROM x5`,
	})
	differ(t, "alter-r30-rename-table-to-string", []string{
		`CREATE TABLE y1(a, b)`,
		`INSERT INTO y1 VALUES(1, 2)`,
		`ALTER TABLE y1 RENAME TO 'y two'`,
		`SELECT sql FROM sqlite_master`,
		`SELECT a, b FROM "y two"`,
	})
	differ(t, "alter-r30-drop-column-string", []string{
		`CREATE TABLE y2(a, b)`,
		`INSERT INTO y2 VALUES(1, 2)`,
		`ALTER TABLE y2 DROP COLUMN 'b'`,
		`SELECT sql FROM sqlite_master WHERE name='y2'`,
		`SELECT * FROM y2`,
	})
}

// TestAlterR30StringLiteralColumnDef pins string literal column names in
// RENAME and DROP COLUMN with quoted definitions.
func TestAlterR30StringLiteralColumnDef(t *testing.T) {
	differ(t, "altercol-23.0-rename", []string{
		`CREATE TABLE t1('a'"b",c)`,
		`CREATE INDEX i1 ON t1('a')`,
		`INSERT INTO t1 VALUES(1,2), (3,4)`,
		`ALTER TABLE t1 RENAME COLUMN a TO x`,
		`PRAGMA integrity_check`,
		`SELECT sql FROM sqlite_schema WHERE name='t1'`,
		`SELECT x, c FROM t1`,
	})
	differ(t, "altercol-23.0-drop", []string{
		`CREATE TABLE t2('a'"b",c)`,
		`INSERT INTO t2 VALUES(1,2)`,
		`ALTER TABLE t2 DROP COLUMN a`,
		`SELECT sql FROM sqlite_schema WHERE name='t2'`,
		`SELECT * FROM t2`,
	})
}

// TestAlterR30TriggerOnStringTable pins CREATE TRIGGER with string literal
// table names; rename must update the trigger's stored SQL.
func TestAlterR30TriggerOnStringTable(t *testing.T) {
	differ(t, "alter-r30-trigger-on-string-table", []string{
		`CREATE TABLE t8(a, b, c)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER trig3 AFTER INSERT ON main.'t8' BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t8 VALUES(1, 2, 3)`,
		`SELECT * FROM log`,
		`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-trigger-on-string-table-renamed", []string{
		`CREATE TABLE t8(a, b, c)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER trig3 AFTER INSERT ON 't8' BEGIN INSERT INTO log VALUES(new.a); END`,
		`ALTER TABLE t8 RENAME TO t9`,
		`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
		`INSERT INTO t9 VALUES(4, 5, 6)`,
		`SELECT * FROM log`,
	})
	differ(t, "alter-r30-trigger-string-name", []string{
		`CREATE TABLE "ON"(a)`,
		`CREATE TRIGGER 'on'.trig4 AFTER INSERT ON 'ON' BEGIN SELECT 1; END`,
		`INSERT INTO "ON" VALUES(1)`,
		`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-trigger-string-value-untouched", []string{
		`CREATE TABLE t8(a)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER trig5 AFTER INSERT ON t8 BEGIN INSERT INTO log VALUES('t8'); END`,
		`ALTER TABLE t8 RENAME TO t9`,
		`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
		`INSERT INTO t9 VALUES(1)`,
		`SELECT * FROM log`,
	})
}

// TestAlterR30EmptyInList pins rename cascade with empty IN() expressions.
// Empty IN() operands are folded at parse time, affecting rename rewriting.
func TestAlterR30EmptyInList(t *testing.T) {
	differ(t, "alter-r30-empty-in-view-column", []string{
		`CREATE TABLE t1(a, b, c, d)`,
		`CREATE VIEW v1 AS SELECT * FROM t1 WHERE a=1 OR (b IN ())`,
		`INSERT INTO t1 VALUES(1, 2, 3, 4)`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-empty-in-view-table", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(a, b, c)`,
		`CREATE VIEW v1 AS SELECT * FROM t1 WHERE (
    SELECT t1.a FROM t1, t2
  ) IN () OR t1.a=5`,
		`INSERT INTO t1 VALUES(5, 1, 1)`,
		`ALTER TABLE t2 RENAME TO t3`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-empty-in-view-func-lhs", []string{
		`CREATE TABLE t1(a, b, c, d)`,
		`CREATE VIEW v1 AS SELECT * FROM t1 WHERE a=1 OR (abs(b) IN ())`,
		`INSERT INTO t1 VALUES(1, 2, 3, 4)`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT * FROM v1`,
	})
	differ(t, "alter-r30-empty-not-in-view", []string{
		`CREATE TABLE t1(a, b, c, d)`,
		`CREATE VIEW v1 AS SELECT * FROM t1 WHERE a=1 OR (b NOT IN ())`,
		`INSERT INTO t1 VALUES(1, 2, 3, 4)`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-empty-in-qualifier", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a FROM t1 WHERE a=1 OR (t1.b IN ())`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`ALTER TABLE t1 RENAME TO t1x`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-empty-in-trigger", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TRIGGER tr1 AFTER INSERT ON t1 WHEN new.a NOT NULL BEGIN
    SELECT true WHERE (SELECT a, b FROM (t1)) IN ();
  END`,
		`ALTER TABLE t1 RENAME TO t1x`,
	})
	differ(t, "alter-r30-empty-in-nothing-to-rewrite", []string{
		`CREATE TABLE s(col)`,
		`CREATE VIEW v AS SELECT (
    WITH x(a) AS(SELECT * FROM s) VALUES(RIGHT)
  ) IN()`,
		`CREATE TABLE a(a)`,
		`ALTER TABLE a RENAME a TO b`,
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-empty-in-elsewhere", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT b FROM t1 WHERE (a IN ()) OR c=2`,
		`INSERT INTO t1 VALUES(1, 2, 2)`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT * FROM v1`,
	})
}

// TestAlterR30QuotedOccurrence pins quoted occurrences in schema text.
// Replacement quoting depends on the original occurrence, not the rename style.
func TestAlterR30QuotedOccurrence(t *testing.T) {
	differ(t, "alter-r30-quoted-occurrence", []string{
		`CREATE TABLE t1("a b", c)`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`ALTER TABLE t1 RENAME "a b" TO d`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT d, c FROM t1`,
	})
	differ(t, "alter-r30-quoted-occurrence-index", []string{
		`CREATE TABLE t2([a b], c)`,
		`CREATE INDEX i2 ON t2([a b])`,
		`INSERT INTO t2 VALUES(1, 2)`,
		`ALTER TABLE t2 RENAME [a b] TO d`,
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		`SELECT c FROM t2 WHERE d=1`,
	})
}

// TestAlterR30EmptyInFuncBoundary pins function call detection in empty IN().
// True function calls preserve the operand for rewriting; structural keywords
// like CAST and NOT do not.
func TestAlterR30EmptyInFuncBoundary(t *testing.T) {
	differ(t, "alter-r30-empty-in-not-operand", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a FROM t1 WHERE a=1 OR ((NOT (b)) IN ())`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-empty-in-nested-func", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a FROM t1 WHERE a=1 OR ((abs(b)+1) IN ())`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "alter-r30-empty-in-subquery-func", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE VIEW v1 AS SELECT a FROM t1 WHERE a=1 OR ((SELECT abs(b)) IN ())`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
}
