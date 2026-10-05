package compat

import "testing"

// TestWindowSubqueryAggregateShadowing compares with C SQLite a window
// operand holding a subquery whose aggregate names a column: when the
// subquery's own FROM binds the name the aggregate is the subquery's
// (resolve.c:703-704 stops at the first NameContext that answers); when only
// the window query's FROM does, the aggregate re-associates outward and the
// window query becomes an aggregate query.
func TestWindowSubqueryAggregateShadowing(t *testing.T) {
	seed := []string{
		`CREATE TABLE t1(a INTEGER, b INTEGER)`,
		`INSERT INTO t1 VALUES(1,10),(2,20),(3,30)`,
		`CREATE TABLE t2(a INTEGER, c INTEGER)`,
		`INSERT INTO t2 VALUES(5,1),(7,2)`,
		`CREATE TABLE t3(z INTEGER)`,
		`INSERT INTO t3 VALUES(100)`,
		`CREATE TABLE e(a INTEGER)`,
		`CREATE VIEW v2 AS SELECT a, c FROM t2`,
	}
	for _, q := range []string{
		`SELECT a, sum(a) OVER (ORDER BY (SELECT max(a) FROM t2) , a) FROM t1 ORDER BY a`,
		`SELECT a, sum(b) FILTER (WHERE b > (SELECT max(a) FROM t2 x WHERE x.c=t1.a)) OVER () FROM t1 ORDER BY a`,
		`SELECT a, sum(b) FILTER (WHERE 3 > (SELECT count(a) FROM t2)) OVER () FROM t1 ORDER BY a`,
		`SELECT a, sum(b) FILTER (WHERE 3 > (SELECT count(x.a) FROM t2 x)) OVER () FROM t1 ORDER BY a`,
		`SELECT a, sum(b) FILTER (WHERE 3 > (SELECT count(rowid) FROM t2)) OVER () FROM t1 ORDER BY a`,
		`SELECT a, sum(b + (SELECT max(c) FROM t2)) OVER (ORDER BY a) FROM t1 ORDER BY a`,
		`SELECT a, sum(b + (SELECT max(z) FROM t3, t2 WHERE t2.a > t1.a)) OVER (ORDER BY a) FROM t1 ORDER BY a`,
		`SELECT k, (SELECT sum(n) FILTER (WHERE t.v > (SELECT max(n) FROM b b2 WHERE b2.n=b.n)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT (SELECT sum(n) FILTER (WHERE t.v > (SELECT max(n) FROM b b2 WHERE b2.n=b.n)) OVER () FROM b LIMIT 1) FROM t ORDER BY 1`,
		// The window query's own column, one subquery down with no inner
		// binding: outward, so an aggregate query.
		`SELECT count(*) OVER (ORDER BY (SELECT sum(b) FROM t3)) FROM t1`,
		`SELECT a, sum(b) FILTER (WHERE 3 > (SELECT count(a) FROM v2)) OVER () FROM t1 ORDER BY a`,
		// Still declined, not answered: the same outward shape over an EMPTY
		// table, which C SQLite answers with one row --
		//   SELECT sum(a) OVER (ORDER BY (SELECT max(e.a) FROM t3)) FROM e
	} {
		differ(t, "window subquery aggregate: "+q, append(append([]string{
			`CREATE TABLE t(k INTEGER, v INTEGER, s TEXT)`,
			`INSERT INTO t VALUES(1,10,'a'),(1,20,'b'),(2,30,'c')`,
			`CREATE TABLE b(n INTEGER)`,
			`INSERT INTO b VALUES(1),(2),(3)`,
		}, seed...), q))
	}
}
