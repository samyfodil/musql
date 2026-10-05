package engine

import (
	"path/filepath"
	"testing"
)

// TestRtreeNoOpWriteSkipsShadowSync pins that an r-tree write matching no row
// leaves the shadow tables alone, as rtree.c's never-reached xUpdate does: with
// %_node dropped, "DELETE ... WHERE rowid=0" succeeds in one session
// (rtreeA.test 3.3.0), while a DELETE that removes a row still fails.
func TestRtreeNoOpWriteSkipsShadowSync(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE t1 USING rtree(id, x1, x2)",
		"INSERT INTO t1 VALUES(1, 1, 2), (2, 3, 4)",
		"DROP TABLE t1_node",
		"DELETE FROM t1 WHERE rowid = 0",
		"UPDATE t1 SET x1 = 0 WHERE rowid = 0",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Exec("DELETE FROM t1 WHERE rowid = 1"); err == nil {
		t.Fatal("DELETE of a real row with no %_node succeeded")
	}
}
