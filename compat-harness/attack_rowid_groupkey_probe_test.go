// The OTHER arm columnRowValue (engine/row_scope.go) has no analogue for:
// groupKeyVals serves a correlated reference to a GROUP BY *key column* out of
// a collapsed group, but that map is keyed by a vals index, so the pseudo-rowid
// branch above it has no such fallback. These cases put the rowid IN the GROUP
// BY key and then reference it from a correlated position, which is the one
// place the missing fallback could surface.
package compat

import "testing"

func rowidGroupKeyBase() []string {
	return []string{
		`CREATE TABLE t1(a INTEGER)`,
		`INSERT INTO t1(rowid,a) VALUES(5,10),(9,20),(11,10)`,
		`CREATE TABLE t2(b INTEGER)`,
		`INSERT INTO t2 VALUES(1),(5),(9),(10),(11),(20)`,
	}
}

func TestProbeRowidAsGroupKey(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		{"group-by-rowid-correlated", []string{
			`SELECT t1.rowid, count(*), (SELECT count(*) FROM t2 WHERE t2.b = t1.rowid) FROM t1 GROUP BY t1.rowid ORDER BY t1.rowid`,
		}},
		{"group-by-rowid-having-correlated", []string{
			`SELECT t1.rowid, count(*) FROM t1 GROUP BY t1.rowid HAVING EXISTS(SELECT 1 FROM t2 WHERE t2.b = t1.rowid) ORDER BY t1.rowid`,
		}},
		{"group-by-rowid-orderby-correlated", []string{
			`SELECT t1.rowid, count(*) FROM t1 GROUP BY t1.rowid ORDER BY (SELECT -t1.rowid)`,
		}},
		{"group-by-expr-of-rowid-correlated", []string{
			`SELECT t1.rowid%2, count(*), (SELECT count(*) FROM t2 WHERE t2.b = t1.rowid) FROM t1 GROUP BY t1.rowid%2 ORDER BY 1`,
		}},
		{"agg-argument-subquery-rowid", []string{
			`SELECT a, sum((SELECT count(*) FROM t2 WHERE t2.b = t1.rowid)) FROM t1 GROUP BY a ORDER BY a`,
		}},
		{"agg-argument-subquery-rowid-nogroup", []string{
			`SELECT sum((SELECT count(*) FROM t2 WHERE t2.b = t1.rowid)) FROM t1`,
		}},
		{"distinct-correlated-rowid", []string{
			`SELECT DISTINCT (SELECT count(*) FROM t2 WHERE t2.b = t1.rowid) FROM t1 ORDER BY 1`,
		}},
		// A rowid GROUP BY key compared against a TEXT literal from a
		// correlated position -- the affinity half of the same arm.
		{"group-by-rowid-correlated-text-compare", []string{
			`SELECT t1.rowid, count(*), (SELECT t1.rowid = '5') FROM t1 GROUP BY t1.rowid ORDER BY t1.rowid`,
		}},
	} {
		differ(t, "probe-rowidkey-"+c.name, append(rowidGroupKeyBase(), c.tail...))
	}
}
