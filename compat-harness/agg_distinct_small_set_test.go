package compat

import (
	"fmt"
	"testing"
)

// TestAggDistinctAcrossSetSizes: DISTINCT aggregates over groups holding from
// one to a few dozen distinct values -- a small set is compared in turn, a
// larger one indexed by hash -- with values that compare equal across storage
// classes (1 and 1.0), values that do not ('1' and 1, x'31' and '1'), NULLs,
// and a NOCASE column, grouped by a TEXT key through a join.
func TestAggDistinctAcrossSetSizes(t *testing.T) {
	schema := []string{
		`CREATE TABLE g(id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE v(gid INTEGER, x, c TEXT COLLATE NOCASE)`,
	}
	for i := 1; i <= 12; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO g VALUES(%d, 'g%d')`, i, i%5))
	}
	vals := []string{"1", "1.0", "'1'", "x'31'", "NULL", "2", "2.5", "'b'", "'B'", "-0.0", "0"}
	cs := []string{"'a'", "'A'", "'b'", "NULL", "'ab'", "'AB'", "''"}
	for i := 0; i < 600; i++ {
		gid := i%12 + 1
		x := vals[i%len(vals)]
		if gid > 6 {
			x = fmt.Sprint(i % (gid * 3))
		}
		schema = append(schema, fmt.Sprintf(`INSERT INTO v VALUES(%d, %s, %s)`, gid, x, cs[(i/3)%len(cs)]))
	}
	for _, q := range []string{
		`SELECT name, count(DISTINCT x), count(DISTINCT c) FROM g JOIN v ON v.gid = g.id GROUP BY name ORDER BY name`,
		`SELECT gid, count(DISTINCT x), sum(DISTINCT x), group_concat(DISTINCT c) FROM v GROUP BY gid ORDER BY gid`,
		`SELECT name, total(DISTINCT x), count(DISTINCT x COLLATE NOCASE) FROM g, v WHERE v.gid = g.id GROUP BY name ORDER BY 1`,
		`SELECT count(DISTINCT x), count(DISTINCT c), avg(DISTINCT x) FROM v`,
	} {
		differ(t, "agg-distinct-set-sizes", append(append([]string{}, schema...), q))
	}
}
