package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestSegmentDeltaMergeCountMatchesScan gates the delta merge arithmetic,
// comparing fast-path answers against full table scans across randomized write sequences.
func TestSegmentDeltaMergeCountMatchesScan(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC0FFEE))
	for round := 0; round < 12; round++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("m%d.musq", round))
		sess, err := Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)`); err != nil {
			t.Fatal(err)
		}
		const n = 300
		for i := 1; i <= n; i++ {
			if e := sess.Exec(fmt.Sprintf(`INSERT INTO t(id,v) VALUES(%d,%d)`, i, rng.Intn(100))); e != nil {
				t.Fatal(e)
			}
		}
		// Commit, so the rows are in the FILE as column blocks.
		if _, e := sess.Commit(); e != nil {
			t.Fatal(e)
		}
		// Then a randomised batch of changes, which becomes the DELTA the merge
		// has to correct for -- and commit it, because a table with UNCOMMITTED
		// writes is excluded from the fast path outright (rowsWrittenSinceCommit).
		for i := 0; i < 60; i++ {
			switch rng.Intn(3) {
			case 0: // supersede an existing row
				if e := sess.Exec(fmt.Sprintf(`UPDATE t SET v = %d WHERE id = %d`, rng.Intn(100), rng.Intn(n)+1)); e != nil {
					t.Fatal(e)
				}
			case 1: // kill one
				if e := sess.Exec(fmt.Sprintf(`DELETE FROM t WHERE id = %d`, rng.Intn(n)+1)); e != nil {
					t.Fatal(e)
				}
			case 2: // a rowid no segment holds
				if e := sess.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO t(id,v) VALUES(%d,%d)`, n+1+rng.Intn(80), rng.Intn(100))); e != nil {
					t.Fatal(e)
				}
			}
		}
		if _, e := sess.Commit(); e != nil {
			t.Fatal(e)
		}

		// The SCAN is the oracle: count the rows a full read returns, in Go, and
		// compare against what the engine answers for the same predicate. The
		// engine's answer comes through the fast path when it is available and
		// through its own scan when it is not, and the two must agree either way --
		// which is exactly the property the merge exists to preserve.
		rp, rerr := sess.ReadPager()
		if rerr != nil {
			t.Fatal(rerr)
		}
		root := uint32(0)
		schema, serr := rp.Schema()
		if serr != nil {
			t.Fatal(serr)
		}
		for _, r := range schema {
			if r.Type == "table" && r.Name == "t" {
				root = r.RootPage
			}
		}
		live := map[int64]int64{}
		seq, errFn := rp.ScanTable(root)
		for rid, vals := range seq {
			live[int64(rid)] = vals[1].I
		}
		if e := errFn(); e != nil {
			t.Fatal(e)
		}
		rp.Close()

		for _, bound := range []int64{0, 1, 25, 50, 99, 100} {
			for _, op := range []string{"<", "<=", "=", ">", ">="} {
				want := 0
				for _, v := range live {
					switch op {
					case "<":
						if v < bound {
							want++
						}
					case "<=":
						if v <= bound {
							want++
						}
					case "=":
						if v == bound {
							want++
						}
					case ">":
						if v > bound {
							want++
						}
					case ">=":
						if v >= bound {
							want++
						}
					}
				}
				q := fmt.Sprintf(`SELECT count(*) FROM t WHERE v %s %d`, op, bound)
				_, rows, qerr := sess.Query(q, nil)
				if qerr != nil {
					t.Fatalf("round %d: %s: %v", round, q, qerr)
				}
				if len(rows) != 1 || int(rows[0][0].I) != want {
					t.Fatalf("round %d: %s answered %v, the scan says %d (rows live=%d)",
						round, q, rows, want, len(live))
				}
			}
		}
		// ...and the unfiltered count, which takes the recorded-per-segment sum
		// rather than the kernel and needs the same correction.
		_, rows, qerr := sess.Query(`SELECT count(*) FROM t`, nil)
		if qerr != nil {
			t.Fatal(qerr)
		}
		if len(rows) != 1 || int(rows[0][0].I) != len(live) {
			t.Fatalf("round %d: count(*) answered %v, the scan says %d", round, rows, len(live))
		}
		if e := sess.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
