// This file tests that min()/max() in a window's frame clause becomes the anchor row
// for bare columns in aggregate queries.
package compat

import "testing"

func TestWindowR24SpecMinMaxAnchorsTheBareColumn(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(g,a,c)`,
		`INSERT INTO t VALUES(1,50,'z'),(1,10,'a'),(2,1,'m'),(2,20,'b'),(3,30,'k')`,
	}
	q := func(sql ...string) []string { return append(append([]string{}, setup...), sql...) }

	differ(t, "spec min/max is the anchor", q(
		`SELECT g, c, count(*) OVER () FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, count(*) OVER (ORDER BY max(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, count(*) OVER (ORDER BY min(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, count(*) OVER (PARTITION BY max(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, count(*) OVER (PARTITION BY min(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, row_number() OVER (ORDER BY max(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT c, count(*) OVER (ORDER BY max(a)) FROM t`,
		`SELECT c, count(*) OVER (ORDER BY min(a)) FROM t`,
		`SELECT g, c, max(a), count(*) OVER (ORDER BY min(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, min(a), count(*) OVER (ORDER BY max(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, max(a), count(*) OVER (ORDER BY max(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, max(a) OVER () FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, max(a) OVER (ORDER BY min(a)) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, count(*) OVER w FROM t GROUP BY g WINDOW w AS (ORDER BY max(a)) ORDER BY g`,
	))

	differ(t, "window4.test 0 #72", []string{
		`CREATE TABLE ttt(b,c)`,
		`INSERT INTO ttt VALUES(1,9),(1,3),(2,7),(2,1)`,
		`SELECT max(b) OVER (ORDER BY max(c)) FROM ttt GROUP BY b`,
		`SELECT b, max(b) OVER (ORDER BY max(c)) FROM ttt GROUP BY b`,
	})

	differ(t, "HAVING/ORDER BY split", q(
		`SELECT g, c, count(*) OVER () FROM t GROUP BY g HAVING max(a)>0 ORDER BY min(a)`,
	))
	for _, s := range []string{
		`SELECT g, c, count(*) OVER (ORDER BY min(a)), max(a) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, c, count(*) OVER (ORDER BY min(a)) FROM t GROUP BY g HAVING max(a)>0`,
	} {
		res := run(t, "musql", append(append([]string{}, setup...), s))
		if res[len(res)-1]["kind"] != "error" {
			t.Errorf("expected a clean decline for %q, got %v", s, res[len(res)-1])
		}
	}
}
