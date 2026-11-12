package compat

import (
	"fmt"
	"testing"
)

// A join level C walks as a WHERE_MULTI_OR union of index sub-scans, one per
// disjunct, seeks the equality every disjunct carries before filing its rows
// under the disjunct each passes: TPC-H q19 over keyed tables (issue #182).
// The filing and its sort decide the order, as before.
func TestMultiOrJoinSeekShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE p(pk INTEGER, brand TEXT, size INT, PRIMARY KEY (pk))",
		"CREATE TABLE l(ok INT, ln INT, pk INT, q REAL, mode TEXT, price REAL, PRIMARY KEY (ok, ln))",
		"CREATE INDEX lpk ON l(pk)",
		"INSERT INTO p VALUES(1,'B12',3),(2,'B23',8),(3,'B34',12),(4,'B12',5),(5,'B23',1)",
		"INSERT INTO l VALUES(1,1,1,5,'AIR',1.5),(1,2,2,12,'AIR REG',2.5),(2,1,1,25,'SHIP',0.1),(2,2,3,15,'AIR',9.75)," +
			"(3,1,2,3,NULL,0.3),(3,2,4,22,'AIR',1.1),(4,1,5,5,'AIR',0.7),(4,2,3,8,'AIR REG',0.2),(5,1,4,2,'AIR',0.05)",
	}
	for i, q := range []string{
		"SELECT sum(price), group_concat(price) FROM l, p WHERE " +
			"(p.pk = l.pk AND brand = 'B12' AND q BETWEEN 1 AND 11 AND mode IN ('AIR', 'AIR REG')) OR " +
			"(p.pk = l.pk AND brand = 'B23' AND q BETWEEN 10 AND 20 AND mode IN ('AIR', 'AIR REG')) OR " +
			"(p.pk = l.pk AND brand = 'B34' AND q BETWEEN 1 AND 30 AND mode IN ('AIR', 'AIR REG'))",
		"SELECT group_concat(price || brand) FROM l, p WHERE (p.pk = l.pk AND size < 5) OR (p.pk = l.pk AND brand = 'B34') OR (p.pk = l.pk AND q > 20)",
		"SELECT group_concat(l.ok || ':' || p.pk) FROM p, l WHERE (l.pk = p.pk AND q < 6) OR (l.pk = p.pk AND size = 12) OR (l.pk = p.pk AND mode IS NULL)",
		"SELECT group_concat(price) FROM l, p WHERE (p.pk = l.pk AND size < 5) OR (p.pk = l.pk + 1 AND size > 5)",
		"SELECT count(*), group_concat(price) FROM l, p WHERE (p.pk = l.pk AND brand = 'B12') OR (p.pk = l.pk AND brand = 'B23') OR (p.pk = l.pk AND brand = 'B34')",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
