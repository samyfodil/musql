package compat

import "testing"

// TestR32NQuotefix checks double-quote fixup in ALTER TABLE RENAME COLUMN and DROP COLUMN
// across multiple object kinds, resolution scopes, and readout methods.
func TestR32NQuotefix(t *testing.T) {
	// ---- table CHECK / generated-column scopes ----
	differ(t, "r32n-check-mixed-readout", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck(x CHECK (x <> "x"), y CHECK ("y" <> 'q'), z, CHECK (z != "nope"))`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`INSERT INTO ck VALUES('x',1,2)`,
		`INSERT INTO ck VALUES('ok',1,2)`,
		`SELECT x,y,z FROM ck`,
	})
	differ(t, "r32n-check-rowid-resolves", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck(x, CHECK ("rowid" > 0), CHECK ("oid" > 0), CHECK ("_rowid_" > 0))`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-check-rowid-withoutrowid", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck(x PRIMARY KEY, CHECK ("rowid" IS NOT NULL)) WITHOUT ROWID`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-gencol-rowid-never-resolves", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE g(x, v AS (x || "rowid"), w AS ("x" || 'z') STORED)`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`INSERT INTO g(x) VALUES('p')`,
		`SELECT x, v, w FROM g`,
	})
	differ(t, "r32n-check-case-insensitive", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck(Abc, CHECK ("ABC" IS NOT NULL), CHECK ("aBcD" IS NOT NULL))`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-check-embedded-quotes", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck(x, CHECK (x <> "it's"), CHECK (x <> "a""b"))`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	// Column names are not expressions; only CHECK references to them are candidates.
	differ(t, "r32n-check-dquoted-column-name", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck("c d" CHECK ("c d" <> 'x'), "e" CHECK ("nope" IS NULL))`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`INSERT INTO ck VALUES('x', 1)`,
		`INSERT INTO ck VALUES('y', 1)`,
		`SELECT "c d", "e" FROM ck`,
	})
	differ(t, "r32n-check-forward-ref", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck(x CHECK ("later" IS NOT NULL), later)`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})

	// ---- indexes ----
	differ(t, "r32n-index-rowid-split", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1(a || "rowid") WHERE "rowid" > 0`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-index-collate-and-desc", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1("b" COLLATE NOCASE DESC, a || "nope")`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`INSERT INTO t1 VALUES(1,'B',3),(2,'a',4)`,
		`SELECT a,b,ccc FROM t1 ORDER BY b`,
	})
	// Bare double-quoted identifiers in indexes fail when quotefixed to string literals.
	differ(t, "r32n-index-bare-dqs-other-table", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(p, q)`,
		`CREATE INDEX i2 ON t2("nope")`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-index-bare-dqs-drop-column", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1("nope")`,
		`ALTER TABLE t1 DROP COLUMN b`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-index-partial-only", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1(a) WHERE "b" IS NOT NULL AND "zz" IS NOT NULL`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`INSERT INTO t1 VALUES(1,2,3),(4,NULL,6)`,
		`SELECT a FROM t1 WHERE b IS NOT NULL`,
	})

	// ---- views ----
	differ(t, "r32n-view-join-other-side", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(p, q)`,
		`CREATE VIEW v1 AS SELECT "a", "p", "zz" FROM t1 JOIN t2 ON t1.a = t2.p`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-view-subquery-inner-scope", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(p, q)`,
		`CREATE VIEW v1 AS SELECT (SELECT "q" FROM t2), "q" FROM t1`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-view-result-alias", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT a+b AS xx FROM t1 WHERE "xx" IS NOT NULL AND "yy" IS NULL`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-view-cte", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS WITH w(m) AS (SELECT a FROM t1) SELECT "m", "n" FROM w`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-view-compound", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(p, q)`,
		`CREATE VIEW v1 AS SELECT "a" FROM t1 UNION ALL SELECT "p" FROM t2 UNION ALL SELECT "zz" FROM t1`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-view-over-view", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT a AS m, b FROM t1`,
		`CREATE VIEW v2 AS SELECT "m", "b", "zz" FROM v1`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-view-rows-readout", []string{
		`CREATE TABLE t1(a, b, c)`,
		`INSERT INTO t1 VALUES(1,2,3),(4,5,6)`,
		`CREATE VIEW v1 AS SELECT "a", "zz" FROM t1 ORDER BY "a"`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT * FROM v1`,
	})
	// Views unaffected by DROP COLUMN still get quotefixed.
	differ(t, "r32n-view-drop-column-unrelated", []string{
		`CREATE TABLE t1(a, b, c)`,
		`INSERT INTO t1 VALUES(1,2,3)`,
		`CREATE VIEW v1 AS SELECT b, "zz" FROM t1`,
		`ALTER TABLE t1 DROP COLUMN a`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT * FROM v1`,
	})
	differ(t, "r32n-view-drop-column-referencing", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT a, "zz" FROM t1`,
		`ALTER TABLE t1 DROP COLUMN a`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})

	// ---- catalogs ----
	differ(t, "r32n-temp-view-main-alter", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TEMP VIEW tv AS SELECT "a", "zz" FROM t1`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT sql FROM temp.sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-temp-table-alter-leaves-main", []string{
		`CREATE TABLE keep(k, CHECK ("zz" IS NULL))`,
		`CREATE TEMP TABLE tt(a, b)`,
		`ALTER TABLE tt RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT sql FROM temp.sqlite_master ORDER BY name`,
	})

	// ---- ALTER forms that do NOT run the pass ----
	differ(t, "r32n-add-column-no-quotefix", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT "a", "zz" FROM t1`,
		`ALTER TABLE t1 ADD COLUMN d`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-drop-constraint-no-quotefix", []string{
		`CREATE TABLE t1(a, b, c, CONSTRAINT k CHECK (a > 0))`,
		`CREATE VIEW v1 AS SELECT "a", "zz" FROM t1`,
		`ALTER TABLE t1 DROP CONSTRAINT k`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
}
