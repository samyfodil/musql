// Differential gate for ALTER TABLE RENAME COLUMN when an unrelated table
// has a same-named column, and a VIEW references the renamed table. Unrelated
// tables should not block the rename.
package compat

import "testing"

// TestRenameColumnViewUnrelatedShadowAgrees tests RENAME COLUMN when unrelated
// tables share the column name being renamed.
func TestRenameColumnViewUnrelatedShadowAgrees(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// Unrelated table a2 shares column name "c" with b1.
		{"altercol-8.4.1-vvv", []string{
			"CREATE TABLE a2(a INTEGER PRIMARY KEY, b, c)", // unrelated, shares column name "c"
			"CREATE TABLE b1(a, b, c)",
			"CREATE TABLE b2(x, y, z)",
			`CREATE VIEW vvv AS SELECT c+c || coalesce(c, c) FROM b1, b2 WHERE x=c GROUP BY c HAVING c>0`,
			`ALTER TABLE b1 RENAME c TO "a;b"`,
			"SELECT sql FROM sqlite_master WHERE name='vvv'",
		}},
		// Unrelated table a2 shares column name "b" with b1.
		{"altercol-8.4.2-www", []string{
			"CREATE TABLE a2(a INTEGER PRIMARY KEY, b, c)", // unrelated, shares column name "b"
			"CREATE TABLE b1(a, b, c)",
			"CREATE TABLE b2(x, y, z)",
			"CREATE VIEW www AS SELECT b FROM b1 UNION ALL SELECT y FROM b2",
			"ALTER TABLE b1 RENAME b TO bbb",
			"SELECT sql FROM sqlite_master WHERE name='www'",
			"INSERT INTO b1 VALUES(1,2,3)",
			"INSERT INTO b2 VALUES(4,5,6)",
			"SELECT * FROM www ORDER BY 1",
		}},
		// Unrelated table a1 shares column name "x" with b2.
		{"altercol-8.4.4-xxx", []string{
			"CREATE TABLE a1(x INTEGER, y TEXT, z BLOB, PRIMARY KEY(x))", // unrelated, shares column name "x"
			"CREATE TABLE b1(a, b, c)",
			"CREATE TABLE b2(x, y, z)",
			"CREATE VIEW xxx AS SELECT a FROM b1 UNION SELECT x FROM b2",
			"ALTER TABLE b2 RENAME x TO hello",
			"SELECT sql FROM sqlite_master WHERE name='xxx'",
			"INSERT INTO b1 VALUES(1,2,3)",
			"INSERT INTO b2 VALUES(9,5,6)",
			"SELECT * FROM xxx ORDER BY 1",
		}},
		// Unrelated table z has a column "f1" like the renamed column on x.
		{"altertab-15.3-y", []string{
			"CREATE TABLE z(f1 integer NOT NULL PRIMARY KEY)", // unrelated, shares column name "f1"
			"ALTER TABLE z RENAME TO z2",
			"CREATE TABLE x(f1 integer NOT NULL)",
			"CREATE VIEW y AS SELECT f1 AS f1 FROM x",
			"INSERT INTO x VALUES(1),(2),(3)",
			"ALTER TABLE x RENAME f1 TO f2",
			"SELECT * FROM x",
			"SELECT sql FROM sqlite_master WHERE name = 'y'",
			"SELECT * FROM y",
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestRenameColumnViewRelatedShadowIsAttributed tests RENAME COLUMN when the
// shadowing table appears in the affected view's body.
func TestRenameColumnViewRelatedShadowIsAttributed(t *testing.T) {
	differ(t, "related-shadow-view", []string{
		"CREATE TABLE shadow(a,c)",
		"CREATE TABLE t33a(a,b)",
		"CREATE VIEW v1 AS SELECT shadow.a, t33a.b FROM shadow, t33a WHERE shadow.c = t33a.b",
		"ALTER TABLE t33a RENAME COLUMN a TO aaa",
		"SELECT sql FROM sqlite_master WHERE name='v1'",
		"INSERT INTO shadow VALUES(7,2)",
		"INSERT INTO t33a VALUES(1,2)",
		"SELECT * FROM v1",
	})
}

// TestRenameColumnCrossTableTriggerNeverBlocksUnrelatedRename tests RENAME
// COLUMN when a trigger on unrelated table t3 references an unrelated t2, whose
// column name matches the column being renamed on a different table t4.
func TestRenameColumnCrossTableTriggerNeverBlocksUnrelatedRename(t *testing.T) {
	differ(t, "altertab2-8.4-8.5", []string{
		"CREATE TABLE t1(a, b, c)",
		"CREATE TABLE t2(a, b, c)",
		"CREATE TABLE t3(d, e, f)",
		"CREATE VIEW v1 AS SELECT * FROM t1",
		`CREATE TRIGGER tr AFTER INSERT ON t3 BEGIN
		   UPDATE t2 SET a = new.d;
		   SELECT a, b, c FROM v1;
		 END`,
		"CREATE TABLE t4(a, b)",
		"CREATE VIEW v4 AS SELECT * FROM t4 WHERE (a=1 AND 0) OR b=2",
		"ALTER TABLE t4 RENAME a TO c",
		"SELECT sql FROM sqlite_master WHERE name = 'v4'",
		"SELECT sql FROM sqlite_master WHERE name = 't4'",
		"SELECT sql FROM sqlite_master WHERE name = 'tr'",
		"INSERT INTO t3 VALUES(1, 2, 3)",
		"SELECT * FROM t2",
	})
}

// TestRenameColumnViewIndirectShadowStillDeclines tests RENAME COLUMN when a
// collision is reachable through a nested view, making the renamed column
// genuinely ambiguous.
func TestRenameColumnViewIndirectShadowStillDeclines(t *testing.T) {
	stmts := []string{
		"CREATE TABLE tbl(a, b)",
		"CREATE TABLE t(q, a)",
		"CREATE VIEW vt AS SELECT q, a FROM t",
		"CREATE VIEW av AS SELECT a FROM tbl, vt",
		"ALTER TABLE tbl RENAME a TO aaa",
	}
	res := run(t, "musql", stmts)
	last := res[len(res)-1]
	if last["kind"] != "error" {
		t.Fatalf("expected musql to decline: the colliding table is reachable only through the nested view vt, got: %v", last)
	}
	// The oracle must ALSO decline -- C SQLite's own resolver refuses this
	// as a genuine ambiguity, it does not merely happen to agree by luck.
	oracle := run(t, "cgo", stmts)
	oLast := oracle[len(oracle)-1]
	if oLast["kind"] != "error" {
		t.Fatalf("expected the oracle to also decline (ambiguous column name) -- pin is stale: %v", oLast)
	}
}
