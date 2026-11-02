package compat

import "testing"

// TestHashGroupByBareColumnsAgree pins GROUP BY shapes the hash path serves --
// a bare column under an expression key, and steps that read no row -- against
// the oracle.
func TestHashGroupByBareColumnsAgree(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(a, b, i)",
		"INSERT INTO t WITH RECURSIVE s(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM s WHERE v < 2000) SELECT v%7, v%11, v FROM s",
		"INSERT INTO t VALUES(NULL, 3, 5000), (2.0, NULL, 5001), ('x', 'y', 5002)",
	}
	for _, q := range []string{
		"SELECT a, sum(i) FROM t GROUP BY a+1",
		"SELECT b, sum(i), count(*) FROM t GROUP BY a+1",
		"SELECT a+1, sum(i) FROM t GROUP BY a+1",
		"SELECT b, max(i), min(i) FROM t GROUP BY a",
		"SELECT a, i FROM t GROUP BY b HAVING count(*) > 180",
		"SELECT a%2, b, total(i) FROM t GROUP BY a%2 ORDER BY a%2",
	} {
		aggOrderCheck(t, aggOrderCase{q, append(append([]string(nil), setup...), q), true})
	}
}
