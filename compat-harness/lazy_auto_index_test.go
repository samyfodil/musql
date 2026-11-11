package compat

import (
	"fmt"
	"testing"
)

// C builds an automatic index where its loop begins, under OP_Once, so a
// loop the outer ones never reach builds nothing; the rows are ordered by it
// only when they are read. TPC-DS q61 is the case (issue #169): its first
// loop matches no row, and C answers without building any index.
func TestLazyAutoIndexShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE s(sk INT, off INT)", "INSERT INTO s VALUES(1,-6),(2,-7)",
		"CREATE TABLE f(sk INT, d INT, v REAL)",
		"INSERT INTO f VALUES(1,10,0.1),(2,20,0.7),(1,10,2.2),(2,30,1.3),(1,20,0.3),(2,10,1.1)",
		"CREATE TABLE d(dk INT, y INT)", "INSERT INTO d VALUES(10,1998),(20,1998),(30,1999)",
	}
	for i, q := range []string{
		"SELECT sum(v) FROM s, f, d WHERE f.sk = s.sk AND f.d = d.dk AND s.off = -5 AND d.y = 1998",
		"SELECT sum(v), group_concat(v) FROM s, f, d WHERE f.sk = s.sk AND f.d = d.dk AND s.off = -6 AND d.y = 1998",
		"SELECT group_concat(v) FROM s, f WHERE f.sk = s.sk AND s.off < -6",
		"SELECT group_concat(v) FROM s, (SELECT sk, v FROM f LIMIT 10) x WHERE x.sk = s.sk AND s.off = -5",
		"SELECT group_concat(v) FROM s, (SELECT sk, v FROM f LIMIT 10) x WHERE x.sk = s.sk AND s.off = -7",
		"SELECT s.sk, group_concat(v) FROM s LEFT JOIN f ON f.sk = s.sk AND f.d = 99 GROUP BY s.sk",
		"SELECT group_concat(coalesce(v, 'n') || '/' || coalesce(s.sk, 'n')) FROM s RIGHT JOIN f ON f.sk = s.sk AND s.off = -5",
		"SELECT group_concat(coalesce(v, 'n') || '/' || coalesce(s.sk, 'n')) FROM (SELECT * FROM s WHERE off = -5) s RIGHT JOIN f ON f.sk = s.sk",
		"SELECT group_concat(coalesce(x.v, 'n')) FROM s FULL JOIN (SELECT sk, v FROM f LIMIT 10) x ON x.sk = s.sk AND s.off = -5",
		"SELECT (SELECT group_concat(v) FROM f, d WHERE f.d = d.dk AND d.y = s.off) FROM s",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
