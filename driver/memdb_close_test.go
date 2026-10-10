//go:build linux

package driver

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryDatabaseCloseUnmaps opens, fills, compacts, reads and closes
// in-memory databases in a loop, each with an in-memory attachment. Closing
// one must unmap its files and delete them, sidecars included. A session that
// kept its columnar read handle open left every closed database's file mapped
// -- its disk space allocated -- until the process exited, which a
// long-running process that opens in-memory databases ran out of disk on; and
// the delta log and lock file of every one were left in the temp directory.
func TestMemoryDatabaseCloseUnmaps(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	mappedDeleted := func() int {
		maps, err := os.ReadFile("/proc/self/maps")
		if err != nil {
			t.Skip(err)
		}
		n := 0
		for _, l := range strings.Split(string(maps), "\n") {
			if strings.Contains(l, "musql-mem-") && strings.HasSuffix(l, "(deleted)") {
				n++
			}
		}
		return n
	}
	before := mappedDeleted()
	for range 5 {
		db, err := sql.Open(DriverName, ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			"CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)",
			"WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 500) INSERT INTO t SELECT i, zeroblob(100) FROM c",
			"VACUUM",
			"ATTACH ':memory:' AS x",
			"CREATE TABLE x.u(a)",
			"INSERT INTO x.u VALUES (1)",
			"ATTACH ':memory:' AS y",
			"CREATE TABLE y.v(a)",
			"INSERT INTO y.v VALUES (1)",
			"DETACH y",
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(q, err)
			}
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM t WHERE id > 10").Scan(&n); err != nil || n != 490 {
			t.Fatal(n, err)
		}
		db.Close()
	}
	if after := mappedDeleted(); after > before {
		t.Fatalf("%d closed in-memory databases are still mapped", after-before)
	}
	left, _ := filepath.Glob(filepath.Join(tmp, "musql-*"))
	if len(left) > 0 {
		t.Fatalf("closed in-memory databases left %d files: %v", len(left), left)
	}
}
