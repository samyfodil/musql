package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSortedSeekLastProbeIsPerCursor: each step of a recursive CTE opens a new
// cursor over e, a copy of the sort the statement keeps (OpSortedSeek's
// rowSorts), and each copy keeps the run of its own last probe. A copy that
// shared another's buffers found its key there beside a stale run, and the
// steps lost rows. The answers are C SQLite's.
func TestSortedSeekLastProbeIsPerCursor(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const want = "[[1 1 0] [2 3 48] [3 9 153] [4 18 369] [5 54 1188] [6 162 2754]]"
	for _, q := range []string{
		`WITH RECURSIVE e(x, y, z) AS (SELECT value % 5, value % 3, value FROM generate_series(1, 40)),
		  c(k, x, y, z) AS (SELECT 1, 1, 1, 0 UNION ALL SELECT k + 1, e.y + 1, (e.z + k) % 3, e.z FROM c, e WHERE e.x = c.x AND e.y = c.y AND k < 6)
		SELECT k, count(*), sum(z) FROM c GROUP BY k`,
		`WITH RECURSIVE e(x, y, z) AS (SELECT 'k' || (value % 5), 'k' || (value % 3), value FROM generate_series(1, 40)),
		  c(k, x, y, z) AS (SELECT 1, 'k1', 'k1', 0 UNION ALL
		    SELECT k + 1, 'k' || (substr(e.y, 2) + 1), 'k' || ((e.z + k) % 3), e.z FROM c, e WHERE e.x = c.x AND e.y = c.y AND k < 6)
		SELECT k, count(*), sum(z) FROM c GROUP BY k`,
	} {
		_, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := make([][]int64, len(rows))
		for i, row := range rows {
			for _, v := range row {
				got[i] = append(got[i], v.I)
			}
		}
		if fmt.Sprint(got) != want {
			t.Fatalf("%s:\n%v, want %s", q, got, want)
		}
	}
}
