package engine

// Cross-database equi-joins must return the attached table's matching rows.

import (
	"fmt"
	"path/filepath"
	"testing"
)

// r30AttachDB builds a database at path from stmts and returns a read pager over
// it with localSchema set to schemaName (what a "<schemaName>.t" reference
// resolved on it must match).
func r30AttachDB(t *testing.T, path, schemaName string, stmts []string) *ReadOnlyPager {
	t.Helper()
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p.SetLocalSchema(schemaName)
	t.Cleanup(func() { p.Close() })
	return p
}

func TestR30CrossDatabaseSeekReadsTheRightFile(t *testing.T) {
	dir := t.TempDir()

	// Attached database with t2 (unindexed) and other tables to create different page layouts.
	att := r30AttachDB(t, filepath.Join(dir, "att.sqlite"), "a", []string{
		"CREATE TABLE q1(a,b)", "CREATE INDEX qi1 ON q1(a,b)",
		"CREATE TABLE q2(a,b)", "CREATE INDEX qi2 ON q2(a,b)",
		"CREATE TABLE q3(a,b)", "CREATE INDEX qi3 ON q3(a,b)",
		"INSERT INTO q1 VALUES(1,'x'),(2,'y'),(3,'z')",
		"INSERT INTO q2 VALUES(1,'x'),(2,'y'),(3,'z')",
		"INSERT INTO q3 VALUES(1,'x'),(2,'y'),(3,'z')",
		"CREATE TABLE t2(k,v)",
		"INSERT INTO t2 VALUES(1,'att-a'),(2,'att-b'),(1,'att-c'),(3,'att-d')",
	})

	// Main database with indexed t2 at a different page location.
	main := r30AttachDB(t, filepath.Join(dir, "main.sqlite"), "main", []string{
		"CREATE TABLE pad1(a)", "CREATE TABLE pad2(a)", "CREATE TABLE pad3(a)",
		"CREATE TABLE t1(x)",
		"CREATE TABLE t2(k,v)",
		"CREATE INDEX i2 ON t2(k,v)",
		"INSERT INTO t1 VALUES(1),(2),(3)",
		"INSERT INTO t2 VALUES(1,'main-only')",
	})
	main.SetAttachedReaders([]string{"a"}, []*ReadOnlyPager{att})

	// Test both qualified and aliased table spellings.
	for _, sql := range []string{
		"SELECT t1.x, a.t2.v FROM t1, a.t2 WHERE a.t2.k = t1.x",
		"SELECT t1.x, z.v FROM t1, a.t2 AS z WHERE z.k = t1.x",
		"SELECT t1.x, z.v FROM t1 JOIN a.t2 AS z ON z.k = t1.x",
		"SELECT t1.x, z.v FROM a.t2 AS z, t1 WHERE z.k = t1.x",
	} {
		_, rows, err := main.Query(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			if len(r) != 2 {
				t.Fatalf("%s: row %v has %d columns, want 2", sql, r, len(r))
			}
			got[fmt.Sprintf("%d|%s", valueInt(r[0]), valueText(r[1]))] = true
		}
		for _, want := range []string{"1|att-a", "1|att-c", "2|att-b", "3|att-d"} {
			if !got[want] {
				t.Fatalf("%s: row %q missing from %v -- the join read the wrong database's b-tree", sql, want, rows)
			}
		}
		if len(got) != 4 {
			t.Fatalf("%s: got %d distinct rows %v, want exactly 4", sql, len(got), rows)
		}
	}
}
