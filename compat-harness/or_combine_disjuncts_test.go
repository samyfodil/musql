package compat

import (
	"fmt"
	"testing"
)

// A two-way OR whose arms compare the same two operands ("x=A" in one arm,
// "x>A" or "x=A" in the other) gets the combined comparison as a further
// conjunct of the WHERE clause, which C's planner uses (whereCombineDisjuncts),
// so the join order and the order rows arrive in follow it (issue #179).
func TestOrCombineDisjunctsShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE l(pk INT, q REAL, mode TEXT, ins TEXT, price REAL)",
		"CREATE TABLE p(pk INT, brand TEXT, size INT)",
		"INSERT INTO l VALUES(1,5,'AIR','IN PERSON',1.5),(2,12,'AIR REG','IN PERSON',2.5),(1,25,'SHIP','IN PERSON',0.1)," +
			"(3,15,'AIR',NULL,9.75),(2,3,NULL,'IN PERSON',0.3),(4,22,'AIR','IN PERSON',1.1),(NULL,5,'AIR','IN PERSON',0.7),(3,8,'AIR REG','IN PERSON',0.2)",
		"INSERT INTO p VALUES(1,'B12',3),(2,'B23',8),(3,'B34',12),(4,NULL,5),(NULL,'B12',1)",
	}
	for i, q := range []string{
		"SELECT group_concat(price) FROM l, p WHERE (p.pk = l.pk AND ins = 'IN PERSON') OR (p.pk > l.pk AND ins = 'IN PERSON')",
		"SELECT group_concat(price) FROM l, p WHERE (p.pk = l.pk AND brand = 'B12') OR (p.pk = l.pk AND brand = 'B23' AND q > 1)",
		"SELECT group_concat(price || brand) FROM l, p WHERE (p.pk = l.pk AND size < 5) OR (l.pk = p.pk AND size > 10)",
		"SELECT group_concat(price) FROM l, p WHERE (q > 10 AND p.pk = l.pk) OR (q = 10 AND p.pk = l.pk)",
		"SELECT group_concat(price) FROM l, p WHERE (q BETWEEN 1 AND 12 AND p.pk = l.pk) OR (q >= 1 AND p.pk = l.pk AND size > 4)",
		"SELECT group_concat(price) FROM l, p WHERE p.pk = l.pk OR (p.pk = l.pk AND mode = 'AIR')",
		"SELECT group_concat(price) FROM l, p WHERE (mode = 'AIR' AND size = 3) OR (mode = 'AIR' AND size = 8)",
		"SELECT group_concat(price) FROM l, p WHERE (mode = 'air' AND p.pk = l.pk) OR (mode = 'AIR' AND p.pk = l.pk)",
		"SELECT group_concat(price) FROM l, p WHERE (l.q < p.size AND brand = 'B12') OR (l.q <= p.size AND brand = 'B34')",
		"SELECT sum(price), count(*) FROM l, p WHERE (p.pk = l.pk AND ins = 'IN PERSON') OR (p.pk > l.pk AND ins = 'IN PERSON')",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
