package compat

import (
	"fmt"
	"testing"
)

// A deterministic FROM-clause subquery beside tables: the join follows C's
// loop order, so an aggregate that reads the arrival order (a REAL sum, a
// group_concat) answers instead of declining. q17 of TPC-H is the first shape
// (issue #165): no index on either table, so C may build an automatic one.
func TestDerivedJoinPlanShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE li(pk INT, q REAL, ep REAL)",
		"CREATE TABLE pt(pk INT, br TEXT)",
		"INSERT INTO pt VALUES(1,'a'),(2,'a'),(3,'b'),(4,'a')",
		"INSERT INTO li VALUES(1,1.5,0.1),(4,2.5,0.2),(2,3.5,0.3),(2,0.5,0.7),(3,1,1.1),(1,0.25,0.3),(4,9,0.1)",
		"CREATE TABLE ix(pk INT, v TEXT)", "CREATE INDEX ixpk ON ix(pk)",
		"INSERT INTO ix VALUES(3,'c'),(1,'a'),(2,'b'),(4,'d'),(1,'e')",
	}
	for i, q := range []string{
		"WITH a AS (SELECT pk, 0.2 * avg(q) AS t FROM li JOIN pt USING(pk) WHERE br = 'a' GROUP BY pk) " +
			"SELECT sum(ep) / 7.0 FROM li JOIN pt ON li.pk = pt.pk JOIN a ON a.pk = li.pk WHERE br = 'a' AND q < t * 5",
		"WITH a AS (SELECT pk, avg(q) AS t FROM li GROUP BY pk) SELECT sum(ep) FROM li JOIN a ON a.pk = li.pk WHERE q < t",
		"SELECT sum(ep) FROM li JOIN (SELECT pk, avg(q) AS t FROM li GROUP BY pk) a ON a.pk = li.pk",
		"SELECT group_concat(ep) FROM li JOIN (SELECT pk, avg(q) AS t FROM li GROUP BY pk) a ON a.pk = li.pk",
		"SELECT group_concat(ep) FROM (SELECT pk, avg(q) AS t FROM li GROUP BY pk) a JOIN li ON a.pk = li.pk",
		"SELECT group_concat(v) FROM ix JOIN (SELECT pk FROM pt WHERE br = 'a') a ON a.pk = ix.pk",
		"SELECT group_concat(v) FROM (SELECT pk FROM pt WHERE br = 'a' GROUP BY pk) a, ix WHERE a.pk = ix.pk",
		"SELECT group_concat(br) FROM pt, (SELECT pk, count(*) AS n FROM li GROUP BY pk) a WHERE a.pk = pt.pk AND n > 1",
		"SELECT group_concat(ep || br) FROM li, pt, (SELECT pk, max(q) AS m FROM li GROUP BY pk) a WHERE li.pk = pt.pk AND a.pk = li.pk AND q = m",
		"WITH a AS (SELECT pk, sum(q) AS s FROM li GROUP BY pk) SELECT pt.pk, group_concat(s) FROM pt JOIN a USING(pk) GROUP BY br",
		"SELECT ep FROM li JOIN (SELECT pk, avg(q) AS t FROM li GROUP BY pk) a ON a.pk = li.pk LIMIT 3",
		"SELECT sum(ep) FROM li JOIN (SELECT DISTINCT pk FROM pt) a ON a.pk = li.pk",
		// A LEFT JOIN its WHERE reduces to an inner one, which the plan may reorder.
		"SELECT group_concat(ep) FROM li LEFT JOIN (SELECT pk, avg(q) AS t FROM li GROUP BY pk) a ON a.pk = li.pk WHERE a.t > 1",
		"SELECT group_concat(ep) FROM pt LEFT JOIN (SELECT pk, avg(q) AS t FROM li GROUP BY pk) a ON a.pk = pt.pk JOIN li ON li.pk = a.pk WHERE a.t IS NOT NULL",
		"SELECT group_concat(ep) FROM li LEFT JOIN (SELECT pk FROM pt WHERE br = 'a') a ON a.pk = li.pk",
		// C reads a subquery through an automatic index keyed by every one of
		// its columns, so equal keys arrive in the order of the rest.
		"SELECT group_concat(q) FROM pt, (SELECT pk, q FROM li LIMIT 100) a WHERE a.pk = pt.pk",
		"SELECT group_concat(q) FROM pt, (SELECT pk, q FROM li ORDER BY q DESC LIMIT 100) a WHERE a.pk = pt.pk",
		"SELECT group_concat(q) FROM pt, (SELECT pk, max(q) q FROM li GROUP BY pk, ep ORDER BY q DESC) a WHERE a.pk = pt.pk",
		"SELECT group_concat(q) FROM (SELECT pk, q FROM li UNION ALL SELECT pk, ep FROM li) a, pt WHERE a.pk = pt.pk",
		"SELECT br, group_concat(q) FROM (SELECT pk, q FROM li UNION ALL SELECT pk, ep FROM li) a, pt WHERE a.pk = pt.pk GROUP BY br",
		"SELECT group_concat(q || '-' || ep) FROM pt, (SELECT pk, q, ep FROM li LIMIT 100) a WHERE a.pk = pt.pk AND a.ep > 0.1",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}

// A subquery that only renames another's columns is flattened away in C,
// leaving the inner one in its place; a UNION, INTERSECT or EXCEPT inside is
// coded by merging under an ORDER BY C invents, which sets its row estimate
// and the order its rows arrive in. TPC-DS q2 and q8 are the shapes (issue
// #169).
func TestDerivedJoinPassThroughShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE ws(d INT, p REAL)", "CREATE TABLE cs(d INT, p REAL)",
		"CREATE TABLE dd(sk INT, wk INT, nm TEXT)",
		"INSERT INTO ws VALUES(1,0.1),(2,0.7),(3,1.3),(1,2.2),(4,0.3)",
		"INSERT INTO cs VALUES(2,0.2),(3,0.9),(1,1.1),(4,0.05)",
		"INSERT INTO dd VALUES(1,10,'Sun'),(2,10,'Mon'),(3,11,'Sun'),(4,11,'Mon')",
		"CREATE TABLE a(x, y)", "INSERT INTO a VALUES(3,'c'),(1,'z'),(2,'b'),(1,'a'),(5,'q')",
		"CREATE TABLE b(x, y)", "INSERT INTO b VALUES(2,'b'),(1,'a'),(3,'c'),(1,'z'),(9,'x')",
		"CREATE TABLE s(x, v REAL)", "INSERT INTO s VALUES(1,0.1),(2,0.2),(3,0.3),(1,0.7),(5,1e16),(2,-1e16)",
	}
	for i, q := range []string{
		"WITH w AS (SELECT d, p FROM (SELECT d, p FROM ws UNION ALL SELECT d, p FROM cs) sq) " +
			"SELECT wk, sum(CASE WHEN nm = 'Sun' THEN p END), group_concat(p) FROM w, dd WHERE sk = d GROUP BY wk",
		"SELECT wk, group_concat(p) FROM (SELECT d, p FROM (SELECT d, p FROM ws UNION ALL SELECT d, p FROM cs) sq) w, dd WHERE sk = d GROUP BY wk",
		"SELECT group_concat(t.x || t.y) FROM (SELECT x, y FROM a INTERSECT SELECT x, y FROM b) t, a WHERE a.x = t.x",
		"SELECT group_concat(t.x || t.y) FROM a, (SELECT x, y FROM a EXCEPT SELECT x, y FROM b) t WHERE a.x = t.x",
		"SELECT group_concat(t.x || t.y) FROM (SELECT x, y FROM a UNION SELECT x, y FROM b) t, a WHERE a.x = t.x",
		"SELECT sum(v) FROM s, (SELECT x FROM (SELECT x FROM a INTERSECT SELECT x FROM b) a2) v1 WHERE s.x = v1.x",
		"SELECT sum(v) FROM s, (SELECT x FROM (SELECT x FROM a INTERSECT SELECT x FROM (SELECT x, count(*) n FROM b GROUP BY x HAVING n > 0) b1) a2) v1 WHERE s.x = v1.x",
		"SELECT sum(v), group_concat(v1.x) FROM s, (SELECT x FROM (SELECT x FROM a EXCEPT SELECT x FROM b) a2) v1 WHERE s.x >= v1.x",
		"SELECT group_concat(v1.y) FROM (SELECT x, y FROM (SELECT x, y FROM a UNION SELECT x, y FROM b) a2) v1, s WHERE s.x = v1.x",
		"SELECT group_concat(v1.yy) FROM (SELECT x, y AS yy FROM (SELECT x, y FROM a UNION SELECT x, y FROM b) a2) v1, s WHERE s.x = v1.x",
		"SELECT v1.x, sum(v) FROM s, (SELECT x FROM (SELECT x FROM a INTERSECT SELECT x FROM b) a2) v1 WHERE s.x = v1.x GROUP BY v1.x ORDER BY v1.x",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
