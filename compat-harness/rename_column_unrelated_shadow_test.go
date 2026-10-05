// This file tests RENAME COLUMN when unrelated tables have same-named columns.
// The rename should only affect columns actually referenced in triggers,
// not all columns with that name.
package compat

import "testing"

// TestRenameColumnUnrelatedShadowAgrees tests rename with unrelated shadowing columns.
func TestRenameColumnUnrelatedShadowAgrees(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"func-33.20", []string{
			"CREATE TABLE t29b(a,b,c,d,e,f,g,h,i)", // unrelated, shares column name "a"
			"CREATE TABLE t33a(a,b)",
			"CREATE TABLE t33b(x,y)",
			`CREATE TRIGGER r1 AFTER INSERT ON t33a BEGIN
			   INSERT INTO t33b(x,y) VALUES(new.a,new.b);
			 END`,
			"ALTER TABLE t33a RENAME COLUMN a TO aaa",
			"SELECT sql FROM sqlite_master WHERE name='r1'",
			"INSERT INTO t33a VALUES(1,2)",
			"SELECT * FROM t33b",
		}},
		{"multiple-unrelated-shadows", []string{
			"CREATE TABLE t29b(a,b,c,d,e,f,g,h,i)", // unrelated shadow #1
			"CREATE TABLE zzother(a,q)",            // unrelated shadow #2
			"CREATE TABLE t33a(a,b)",
			"CREATE TABLE t33b(x,y)",
			`CREATE TRIGGER r1 AFTER INSERT ON t33a BEGIN
			   INSERT INTO t33b(x,y) VALUES(new.a,new.b);
			 END`,
			"ALTER TABLE t33a RENAME COLUMN a TO aaa",
			"SELECT sql FROM sqlite_master WHERE name='r1'",
			"INSERT INTO t33a VALUES(1,2)",
			"SELECT * FROM t33b",
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestRenameColumnRelatedShadowIsAttributed tests rename when the shadowing table's
// name appears in the trigger body.
func TestRenameColumnRelatedShadowIsAttributed(t *testing.T) {
	differ(t, "related-shadow-trigger", []string{
		"CREATE TABLE shadow(a,c)",
		"CREATE TABLE t33a(a,b)",
		"CREATE TABLE t33b(x,y)",
		`CREATE TRIGGER r1 AFTER INSERT ON t33a BEGIN
		   INSERT INTO t33b(x,y) SELECT shadow.a, new.b FROM shadow WHERE shadow.c = new.b;
		 END`,
		"ALTER TABLE t33a RENAME COLUMN a TO aaa",
		"SELECT sql FROM sqlite_master WHERE name='r1'",
		"SELECT sql FROM sqlite_master WHERE name='t33a'",
		"INSERT INTO shadow VALUES(7,2)",
		"INSERT INTO t33a VALUES(1,2)",
		"SELECT * FROM t33b",
	})
}
