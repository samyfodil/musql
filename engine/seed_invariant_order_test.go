package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestSeedInvariantOrderedReplay: a one-table subquery planned where its
// enclosing loop count is unknown (a correlated term's trial compile), under DISTINCT or ORDER BY, is decided by
// replaying the solve rather than solving at every seed. The engine's tests
// still solve every seed and panic where the replay disagrees
// (seedInvariantVerify). A full scan, an automatic index and indexes that
// give or miss the order compete, so some of these do depend on the seed.
func TestSeedInvariantOrderedReplay(t *testing.T) {
	bodies := []string{
		`SELECT DISTINCT b + 1 FROM u WHERE d = 'd1' AND e = 3`,
		`SELECT DISTINCT b FROM u WHERE d = 'd2' AND e > 5`,
		`SELECT DISTINCT e FROM u WHERE d = 'd0'`,
		`SELECT DISTINCT b * 100 + e FROM u WHERE c = 2`,
		`SELECT b FROM u WHERE d = 'd1' ORDER BY b LIMIT 1`,
		`SELECT b FROM u WHERE e = 4 ORDER BY b DESC LIMIT 1`,
		`SELECT e FROM u WHERE d = 'd2' ORDER BY e, b LIMIT 1`,
		`SELECT DISTINCT a FROM u WHERE b = 7 AND e < 9`,
		`SELECT b FROM u WHERE a > 2 AND e = 1 ORDER BY a LIMIT 1`,
		`SELECT b FROM u WHERE c = 1 AND b > 3 ORDER BY b LIMIT 1`,
		`SELECT max(b) FROM (SELECT b FROM u WHERE e = 2 ORDER BY b)`,
	}
	before := seedInvariantReplays.Load()
	for _, n := range []int{3, 6, 9, 13, 20, 28, 40, 60, 97, 150, 300, 700, 1500} {
		for _, analyze := range []bool{false, true} {
			stmts := []string{
				`CREATE TABLE t (g INTEGER, x INTEGER)`,
				`CREATE TABLE u (a INTEGER, b INTEGER, c INTEGER, d TEXT, e INTEGER)`,
				`CREATE INDEX u_a ON u(a)`,
				`CREATE INDEX u_cb ON u(c, b)`,
				`CREATE INDEX u_b ON u(b)`,
			}
			for i := 0; i < n; i++ {
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d)`, i%7, i),
					fmt.Sprintf(`INSERT INTO u VALUES (%d, %d, %d, 'd%d', %d)`, i%13, i%29, i%5, i%3, i%11))
			}
			if analyze {
				stmts = append(stmts, `ANALYZE`)
			}
			p := newSegPair(t, stmts...)
			for _, b := range bodies {
				// Named as it is, the body binds nothing outside, so its term
				// is placed without a trial compile and plans where the loop
				// count is known. Naming t.x, it binds the enclosing row, and
				// the trial compile plans it while the count is not yet known.
				corr := strings.ReplaceAll(b, "FROM u WHERE", "FROM u WHERE t.x >= 0 AND")
				qs := []string{
					`SELECT a, sum(x) FROM t, u WHERE t.g = u.c AND u.b BETWEEN (` + b + `) AND 40 GROUP BY a ORDER BY a`,
					`WITH r AS (SELECT a, sum(x) s FROM t, u WHERE t.g = u.c AND u.e >= (` + b + `) GROUP BY a) SELECT count(*), sum(s) FROM r`,
				}
				// It runs once per joined row, so only over the smaller tables.
				if n <= 97 {
					qs = append(qs,
						`SELECT a, sum(x) FROM t, u WHERE t.g = u.c AND u.b BETWEEN (`+corr+`) AND 40 GROUP BY a ORDER BY a`,
						`WITH r AS (SELECT a, sum(x) s FROM t, u WHERE t.g = u.c AND u.e >= (`+corr+`) GROUP BY a) SELECT count(*), sum(s) FROM r`)
				}
				for _, q := range qs {
					got := p.mustFast(q)
					want := p.mustPlain(q)
					if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
						t.Errorf("n=%d analyze=%v %s:\n fast  %.300s\n plain %.300s", n, analyze, q, g, w)
					}
				}
			}
		}
	}
	if n := seedInvariantReplays.Load() - before; n < 50 {
		t.Errorf("%d solves replayed, want at least 50", n)
	}
}
