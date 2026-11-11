package compat

import (
	"fmt"
	"testing"
)

// A WHERE term past a LEFT JOIN is tested where its tables are bound, as C
// tests it, and a seek keys on every equality of its level. TPC-DS q40 and
// q78 are the shapes (issue #169): a fact table LEFT JOIN its returns on two
// columns, then the dimensions.
func TestLeftJoinPushdownShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE cs(o INT, i INT, w INT, d INT, p REAL)",
		"CREATE TABLE cr(o INT, i INT, cash REAL)",
		"CREATE TABLE it(sk INT, price REAL, id TEXT)", "CREATE TABLE wh(sk INT, st TEXT)", "CREATE TABLE dd(sk INT, y INT)",
		"INSERT INTO cs VALUES(1,1,1,0,1.5),(1,2,2,1,2.5),(2,1,1,2,0.1),(3,3,2,0,9.75),(4,2,1,1,0.3),(5,1,2,2,1.1),(2,2,1,0,0.7)",
		"INSERT INTO cr VALUES(1,1,0.5),(2,1,0.05),(4,2,0.3),(4,2,0.1),(9,9,1)",
		"INSERT INTO it VALUES(1,1.0,'a'),(2,1.3,'b'),(3,2.0,'c')", "INSERT INTO wh VALUES(1,'TN'),(2,'GA')",
		"INSERT INTO dd VALUES(0,2000),(1,2001),(2,2000)",
	}
	for i, q := range []string{
		"SELECT st, id, sum(p - coalesce(cash, 0)) FROM cs LEFT JOIN cr ON cs.o = cr.o AND cs.i = cr.i, wh, it, dd " +
			"WHERE it.price BETWEEN 0.99 AND 1.49 AND it.sk = cs.i AND cs.w = wh.sk AND cs.d = dd.sk GROUP BY st, id ORDER BY st, id",
		"SELECT group_concat(p || ':' || coalesce(cash, 'n')) FROM cs LEFT JOIN cr ON cs.o = cr.o AND cs.i = cr.i, it WHERE it.sk = cs.i AND it.price < 1.5",
		"SELECT group_concat(p) FROM cs LEFT JOIN cr ON cr.o = cs.o AND cs.i = cr.i JOIN dd ON cs.d = dd.sk WHERE cr.o IS NULL",
		"SELECT y, sum(p), group_concat(p) FROM cs LEFT JOIN cr ON cr.o = cs.o AND cs.i = cr.i JOIN dd ON cs.d = dd.sk WHERE cr.o IS NULL GROUP BY y",
		"SELECT group_concat(p || coalesce(cash, '')) FROM cs LEFT JOIN cr ON cs.o = cr.o AND cs.i = cr.i, it WHERE it.sk = cs.i AND coalesce(cr.cash, 0) < 0.2",
		"SELECT group_concat(p || st) FROM cs LEFT JOIN cr ON cs.o = cr.o, wh WHERE wh.sk = cs.w AND (cr.cash IS NULL OR wh.st = 'GA')",
		"SELECT group_concat(a.p || '/' || b.p) FROM cs a LEFT JOIN cr ON a.o = cr.o AND a.i = cr.i JOIN cs b ON b.o = a.o AND b.i <> a.i AND b.w = a.w",
		"SELECT count(*), group_concat(x.o || x.i) FROM cs x JOIN cs y ON x.o = y.o AND x.i = y.i AND x.d = y.d LEFT JOIN cr ON cr.o = x.o",
		"SELECT group_concat(p) FROM cs LEFT JOIN cr ON cs.o = cr.o AND cs.i = cr.i LEFT JOIN wh ON wh.sk = cs.w AND wh.st = 'TN' WHERE wh.sk IS NOT NULL",
		"SELECT group_concat(id) FROM cs LEFT JOIN cr ON cs.o = cr.o, it WHERE it.sk = cs.i AND it.id = 'a' AND cr.cash IS NULL",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
