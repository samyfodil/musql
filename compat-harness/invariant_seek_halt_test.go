package compat

import (
	"fmt"
	"testing"
)

// TestInvariantSeekHaltSweep: a join level sought by a key no outer row
// decides finds the same rows for every outer row, so when it finds none the
// join ends at once (seekKeyInvariant). Every placement of such a level --
// behind tables no term connects, under and over LEFT JOINs, in subqueries,
// with the key present and absent -- answers as C does.
func TestInvariantSeekHaltSweep(t *testing.T) {
	pre := []string{
		"CREATE TABLE cd(x INTEGER, g TEXT)", "CREATE TABLE hd(y INTEGER)",
		"CREATE TABLE dd(k INTEGER, yr INTEGER, nm TEXT)", "CREATE TABLE ss(k INTEGER, s INTEGER, v INTEGER)",
		"CREATE TABLE st(s INTEGER, w INTEGER)", "CREATE TABLE t(v)",
	}
	for i := 1; i <= 12; i++ {
		pre = append(pre, fmt.Sprintf("INSERT INTO cd VALUES(%d, '%c')", i, 'a'+i%3), fmt.Sprintf("INSERT INTO hd VALUES(%d)", i*10),
			fmt.Sprintf("INSERT INTO dd VALUES(%d, %d, 'n%d')", i, 1990+i%4, i%5), fmt.Sprintf("INSERT INTO ss VALUES(%d, %d, %d)", i%6, i%3, i*7),
			fmt.Sprintf("INSERT INTO st VALUES(%d, %d)", i%4, i))
	}
	pre = append(pre, "INSERT INTO t VALUES(1991),(2001),(NULL)")
	var cases [][]string
	for _, yr := range []string{"1991", "2001", "'1991'", "-7", "1990 + 1", "NULL", "CAST('1993' AS INTEGER)"} {
		for _, q := range []string{
			"SELECT count(*), sum(x), sum(v) FROM ss, st, cd, hd, dd WHERE st.s = ss.s AND ss.k = dd.k AND dd.yr = %s",
			"SELECT count(*) FROM cd, hd, dd WHERE dd.yr = %s",
			"SELECT count(*) FROM cd, hd, dd, ss WHERE ss.k = dd.k AND dd.yr = %s AND ss.v > x",
			"SELECT x, y FROM cd, hd, dd WHERE dd.yr = %s ORDER BY x, y LIMIT 5",
			"SELECT g, count(*) FROM cd, hd, dd WHERE dd.yr = %s GROUP BY g ORDER BY g",
			"SELECT count(*), sum(y) FROM cd LEFT JOIN hd ON y = x * 10, dd WHERE dd.yr = %s",
			"SELECT count(*), sum(dd.k) FROM cd, hd LEFT JOIN dd ON dd.yr = %s AND dd.k = cd.x",
			"SELECT count(*) FROM cd LEFT JOIN (hd, dd) ON y = x * 10 AND dd.yr = %s",
			"SELECT v, (SELECT count(*) FROM cd, hd, dd WHERE dd.yr = %s) FROM t ORDER BY v",
			"SELECT v, (SELECT count(*) FROM cd, hd, dd WHERE dd.yr = %s AND dd.yr = t.v) FROM t ORDER BY v",
			"SELECT count(*) FROM cd, hd, dd WHERE dd.yr = %s AND dd.nm = 'n1'",
			"SELECT EXISTS (SELECT 1 FROM cd, hd, dd WHERE dd.yr = %s)",
		} {
			cases = append(cases, append(append([]string(nil), pre...), fmt.Sprintf(q, yr)))
		}
		cases = append(cases, append(append([]string(nil), pre...),
			fmt.Sprintf("UPDATE cd SET x = x + 100 WHERE EXISTS (SELECT 1 FROM hd, dd WHERE dd.yr = %s)", yr), "SELECT sum(x) FROM cd"))
	}
	sweepAll(t, cases)
}
