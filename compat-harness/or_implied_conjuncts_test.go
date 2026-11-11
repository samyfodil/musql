package compat

import (
	"fmt"
	"testing"
)

// The conjuncts every disjunct of an OR carries are tested where their tables
// are bound, before the join reaches the OR. TPC-H q19 is the shape (issue
// #171): each arm repeats the join and the lineitem filters. Only pruning:
// the OR still decides, NULLs included.
func TestOrImpliedConjunctShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE l(pk INT, q REAL, mode TEXT, ins TEXT, price REAL)",
		"CREATE TABLE p(pk INT, brand TEXT, size INT)",
		"INSERT INTO l VALUES(1,5,'AIR','IN PERSON',1.5),(2,12,'AIR REG','IN PERSON',2.5),(1,25,'SHIP','IN PERSON',0.1)," +
			"(3,15,'AIR',NULL,9.75),(2,3,NULL,'IN PERSON',0.3),(4,22,'AIR','IN PERSON',1.1),(NULL,5,'AIR','IN PERSON',0.7),(3,8,'AIR REG','IN PERSON',0.2)",
		"INSERT INTO p VALUES(1,'B12',3),(2,'B23',8),(3,'B34',12),(4,NULL,5),(NULL,'B12',1)",
	}
	for i, q := range []string{
		"SELECT sum(price), group_concat(price) FROM l, p WHERE " +
			"(p.pk = l.pk AND brand = 'B12' AND q BETWEEN 1 AND 11 AND mode IN ('AIR', 'AIR REG') AND ins = 'IN PERSON') OR " +
			"(p.pk = l.pk AND brand = 'B23' AND q BETWEEN 10 AND 20 AND mode IN ('AIR', 'AIR REG') AND ins = 'IN PERSON') OR " +
			"(p.pk = l.pk AND brand = 'B34' AND q BETWEEN 1 AND 30 AND mode IN ('AIR', 'AIR REG') AND ins = 'IN PERSON')",
		"SELECT group_concat(l.pk || p.pk) FROM l, p WHERE (p.pk = l.pk AND mode = 'AIR') OR (p.pk = l.pk AND mode = 'AIR' AND size > 4)",
		"SELECT group_concat(l.pk || p.pk) FROM l, p WHERE (mode IS NULL AND p.pk = l.pk) OR (p.pk = l.pk AND mode IS NULL)",
		"SELECT group_concat(price) FROM l, p WHERE (p.pk = l.pk AND ins = 'IN PERSON') OR (p.pk = l.pk AND brand IS NULL)",
		"SELECT group_concat(price) FROM l, p WHERE (p.pk = l.pk AND q > 10) OR (p.pk = l.pk AND q > 10 AND brand = 'B12') OR q > 20",
		"SELECT group_concat(coalesce(brand, '-')) FROM l LEFT JOIN p ON p.pk = l.pk WHERE (brand = 'B12' AND mode = 'AIR') OR (brand = 'B23' AND mode = 'AIR')",
		"SELECT group_concat(price) FROM l, p WHERE (p.pk = l.pk AND abs(q) > 4 AND mode = 'AIR') OR (p.pk = l.pk AND abs(q) > 4 AND size = 12)",
		"SELECT count(*), sum(price) FROM l, p WHERE (p.pk = l.pk AND mode <> 'SHIP') OR (p.pk = l.pk AND mode <> 'SHIP' AND size IS NULL)",
		"SELECT group_concat(price) FROM l, p WHERE (p.pk = l.pk AND l.pk IN (1, 2) AND size < 10) OR (l.pk IN (1, 2) AND p.pk = l.pk AND brand = 'B23')",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
