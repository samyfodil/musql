package engine

// Cross-database single-table equality must return the attached table's matching rows.

import (
	"fmt"
	"testing"

	"path/filepath"
)

func TestR30CrossDatabaseSingleTableSeekReadsTheRightFile(t *testing.T) {
	dir := t.TempDir()

	// Attached database with unindexed t2 and padding tables.
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

	main := r30AttachDB(t, filepath.Join(dir, "main.sqlite"), "main", []string{
		"CREATE TABLE pad1(a)", "CREATE TABLE pad2(a)", "CREATE TABLE pad3(a)",
		"CREATE TABLE t2(k,v)",
		"CREATE INDEX i2 ON t2(k,v)",
		"INSERT INTO t2 VALUES(1,'main-only')",
	})
	main.SetAttachedReaders([]string{"a"}, []*ReadOnlyPager{att})

	// Test both qualified and aliased table spellings.
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{"SELECT k, v FROM a.t2 WHERE k = 1", []string{"1|att-a", "1|att-c"}},
		{"SELECT z.k, z.v FROM a.t2 AS z WHERE z.k = 1", []string{"1|att-a", "1|att-c"}},
		{"SELECT k, v FROM a.t2 WHERE k = 3", []string{"3|att-d"}},
	} {
		_, rows, err := main.Query(tc.sql)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[fmt.Sprintf("%d|%s", valueInt(r[0]), valueText(r[1]))] = true
		}
		for _, want := range tc.want {
			if !got[want] {
				t.Fatalf("%s: row %q missing from %v -- the seek read the wrong database's b-tree", tc.sql, want, rows)
			}
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %d distinct rows %v, want exactly %d", tc.sql, len(got), rows, len(tc.want))
		}
	}
}
