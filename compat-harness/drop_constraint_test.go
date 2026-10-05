// Gates ALTER TABLE ... DROP CONSTRAINT, handling both table-level and
// column-level named constraints. Some constraint types (PRIMARY KEY, UNIQUE,
// FOREIGN KEY) cannot be dropped; constraints are recognized by keyword boundary,
// not by comma.
package compat

import "testing"

func TestAlterTableDropConstraint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"drops a single named table-level CHECK", []string{
			`CREATE TABLE t1(a INTEGER, b INTEGER, CONSTRAINT ck1 CHECK(a > 0))`,
			`ALTER TABLE t1 DROP CONSTRAINT ck1`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
			// The check no longer enforces.
			`INSERT INTO t1 VALUES(-5, 1)`,
		}},
		// insert4.test's own exact shape.
		{"insert4.test's own shape: two tables, one constraint each", []string{
			`CREATE TABLE src(x,y,z, CONSTRAINT c1 CHECK(rowid<=5))`,
			`CREATE TABLE dest(x,y,z, CONSTRAINT c2 CHECK(rowid<=5))`,
			`INSERT INTO src(x) VALUES(22),(33),(44)`,
			`INSERT INTO dest(x) VALUES(55),(66),(77)`,
			`ALTER TABLE src DROP CONSTRAINT c1`,
			`ALTER TABLE dest DROP CONSTRAINT c2`,
			`SELECT sql FROM sqlite_master WHERE type='table' ORDER BY name`,
			`INSERT INTO dest SELECT * FROM src`,
			`SELECT rowid, x FROM dest ORDER BY rowid`,
		}},
		{"the middle constraint of three, comma splicing", []string{
			`CREATE TABLE t1(a INTEGER, b INTEGER, CONSTRAINT ck1 CHECK(a > 0), CONSTRAINT ck2 CHECK(b > 0), CONSTRAINT ck3 CHECK(a < b))`,
			`ALTER TABLE t1 DROP CONSTRAINT ck2`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
			`INSERT INTO t1 VALUES(1,2)`,
			`INSERT INTO t1 VALUES(1,-5)`, // ck2 (b>0) would have caught this; ck3 (a<b) still does
		}},
		{"the last constraint, comma splicing", []string{
			`CREATE TABLE t1(a INTEGER, CONSTRAINT ck1 CHECK(a > 0), CONSTRAINT ck2 CHECK(a < 100))`,
			`ALTER TABLE t1 DROP CONSTRAINT ck2`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
			`INSERT INTO t1 VALUES(500)`, // ck2 no longer enforces; ck1 still does
			`INSERT INTO t1 VALUES(-5)`,
		}},
		// A view referencing the table needs no rewrite at all: dropping a
		// CHECK changes neither the table's own name nor any column's.
		{"a referencing view is left untouched", []string{
			`CREATE TABLE t1(a INTEGER, CONSTRAINT ck1 CHECK(a > 0))`,
			`CREATE VIEW v1 AS SELECT * FROM t1`,
			`ALTER TABLE t1 DROP CONSTRAINT ck1`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
			`SELECT sql FROM sqlite_master WHERE name='v1'`,
		}},
		// A nonexistent name, a named PRIMARY KEY, a named UNIQUE, and a
		// named FOREIGN KEY are all mutual rejects, each in C's own wording.
		{"nonexistent constraint name is a mutual reject", []string{
			`CREATE TABLE t1(a INTEGER, CONSTRAINT ck1 CHECK(a > 0))`,
			`ALTER TABLE t1 DROP CONSTRAINT nope`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
		}},
		{"named PRIMARY KEY is a mutual reject", []string{
			`CREATE TABLE t1(a INTEGER, b INTEGER, CONSTRAINT pk1 PRIMARY KEY(a))`,
			`ALTER TABLE t1 DROP CONSTRAINT pk1`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
		}},
		{"named UNIQUE is a mutual reject", []string{
			`CREATE TABLE t1(a INTEGER, b INTEGER, CONSTRAINT uq1 UNIQUE(b))`,
			`ALTER TABLE t1 DROP CONSTRAINT uq1`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
		}},
		{"named FOREIGN KEY is a mutual reject", []string{
			`CREATE TABLE p(a PRIMARY KEY)`,
			`CREATE TABLE t1(a INTEGER, CONSTRAINT fk1 FOREIGN KEY(a) REFERENCES p(a))`,
			`ALTER TABLE t1 DROP CONSTRAINT fk1`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
		}},
		{"consecutive table constraints with no comma between them", []string{
			`CREATE TABLE abc(a, b, c, CONSTRAINT one CHECK (a>b) FOREIGN KEY(a) REFERENCES abc)`,
			`ALTER TABLE abc DROP CONSTRAINT one`,
			`SELECT sql FROM sqlite_master WHERE name='abc'`,
		}},
		{"whitespace before the closing paren goes with the constraint", []string{
			"CREATE TABLE \"Test\" ( \n      \"IsActive\" INTEGER, \n      CONSTRAINT \"BooleanZeroOrOne\" CHECK (\"IsActive\" IN (0, 1)) \n  )",
			`ALTER TABLE Test DROP CONSTRAINT "BooleanZeroOrOne"`,
			`SELECT sql FROM sqlite_master WHERE name='Test'`,
			`INSERT INTO Test VALUES(7)`,
		}},
		{"a column-level named CHECK", []string{
			`CREATE TABLE t1(a INTEGER CONSTRAINT ck1 CHECK(a > 0) NOT NULL, b)`,
			`ALTER TABLE t1 DROP CONSTRAINT ck1`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
			`INSERT INTO t1 VALUES(-5, 1)`,
			`INSERT INTO t1 VALUES(NULL, 1)`,
		}},
		{"only the label in front of DEFAULT and COLLATE", []string{
			`CREATE TABLE abc(a, b CONSTRAINT two COLLATE nocase CHECK (a!=b), c CONSTRAINT one DEFAULT 'abc')`,
			`ALTER TABLE abc DROP CONSTRAINT one`,
			`ALTER TABLE abc DROP CONSTRAINT two`,
			`SELECT sql FROM sqlite_master WHERE name='abc'`,
			`INSERT INTO abc(a, b) VALUES('x', 'X')`,
			`SELECT * FROM abc`,
		}},
		{"a chain of labels", []string{
			`CREATE TABLE abc(a, b, c, CONSTRAINT one CONSTRAINT two CHECK (b!=c))`,
			`ALTER TABLE abc DROP CONSTRAINT one`,
			`SELECT sql FROM sqlite_master WHERE name='abc'`,
		}},
		{"a column-level named UNIQUE may not be dropped", []string{
			`CREATE TABLE t1(a INTEGER CONSTRAINT u1 UNIQUE, b)`,
			`ALTER TABLE t1 DROP CONSTRAINT u1`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
		}},
		{"a column named \"constraint\" is not confused with the keyword", []string{
			`CREATE TABLE t1(a INTEGER, "constraint" INTEGER)`,
			`ALTER TABLE t1 DROP CONSTRAINT foo`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "dropconstraint/"+tc.name, tc.stmts) })
	}
}
