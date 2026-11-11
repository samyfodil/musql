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
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
