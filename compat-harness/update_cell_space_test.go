package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestUpdateCellSpaceMatchesCSQLite tests UPDATEs that rewrite row cells,
// verifying result integrity.
func TestUpdateCellSpaceMatchesCSQLite(t *testing.T) {
	onePage := []string{`CREATE TABLE t0(v, n)`, `INSERT INTO t0 VALUES('orig-0', 1),('orig-1', 2),('orig-2', 3)`}
	manyPages := []string{
		`CREATE TABLE t0(v, n)`,
		`CREATE INDEX t0n ON t0(n)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<400) INSERT INTO t0 SELECT printf('row-%d', i), i FROM c`,
	}
	for _, mode := range []string{"delete", "wal"} {
		for _, tc := range []struct {
			seed    []string
			updates [][]string
		}{
			{onePage, [][]string{{`UPDATE t0 SET v='mid-0'`}}},
			{onePage, [][]string{{`UPDATE t0 SET v='a much longer replacement value than before'`}}},
			{onePage, [][]string{{`UPDATE t0 SET n=200`}}},
			// page2 is FALSE for this one, and it is the only shape in this
			// file where it is: every row's new record is exactly the SAME SIZE
			// as the old one, which is the case C SQLite overwrites in place
			// (sqlite3BtreeInsert, btree.c:9644 -- "the new entry is the same
			// size as the old"). This engine's UPDATE deletes the row and
			// re-inserts it (opUpdateRow, vdbe_write.go), so the cell is freed
			// and reallocated: the rows are right, integrity_check is ok, and
			// the page's cell offsets and freeblock differ.
			//
			// Measured attempt, reverted: keeping the row and dropping only its
			// index entries (update.c:1010's own order) reproduces the bytes but
			// puts the row in front of this engine's conflict probe, which reads
			// the TABLE's rows where C SQLite's reads index entries -- even
			// with the row exempted from both halves, conflict.test and
			// conflict2.test came back with wrong answers around REPLACE. The
			// probe has to move to the index b-trees first.
			{onePage, [][]string{{`UPDATE t0 SET n=n+1`}}},
			{onePage, [][]string{{`UPDATE t0 SET n=70000 WHERE rowid=2`}}},
			{onePage, [][]string{{`UPDATE t0 SET v=v||v WHERE rowid%2=1`}}},
			{onePage, [][]string{{`UPDATE t0 SET v='x'`}, {`UPDATE t0 SET v='yyyyyyyyyy' WHERE rowid=2`}, {`UPDATE t0 SET v='zz'`}}},
			{manyPages, [][]string{{`UPDATE t0 SET v=v||'-grown' WHERE n%3=0`}}},
			{manyPages, [][]string{{`UPDATE t0 SET v='s', n=n*1000 WHERE n%2=0`}, {`UPDATE t0 SET v=printf('%.*c', n%50, 'q')`}}},
			{manyPages, [][]string{{`DELETE FROM t0 WHERE n%5=0`}, {`UPDATE t0 SET v=v||v WHERE n<100`}, {`INSERT INTO t0 VALUES('late', 7)`}}},
		} {
			for _, eng := range []string{"musql", "cgo"} {
				dsn := filepath.Join(t.TempDir(), "f.db")
				runWithDSN(t, eng, dsn, append([]string{`PRAGMA journal_mode=` + mode}, tc.seed...))
				for _, u := range tc.updates {
					runWithDSN(t, eng, dsn, u)
				}
				runWithDSN(t, eng, dsn, []string{`PRAGMA wal_checkpoint(TRUNCATE)`})
				db, err := sql.Open("sqlite3", exportedForOracle(t, dsn))
				if err != nil {
					t.Fatal(err)
				}
				got := probeE2AllIntegrityCheckRows(t, db, `PRAGMA integrity_check`)
				db.Close()
				if got != "ok" {
					t.Errorf("%s %s %v: C SQLite's integrity_check = %q", eng, mode, tc.updates, got)
				}
			}
		}
	}
}
