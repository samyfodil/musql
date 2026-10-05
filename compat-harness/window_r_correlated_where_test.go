// Differential tests of window queries with correlated subqueries in WHERE and window specs.
package compat

import "testing"

func TestWindowCorrelatedSubqueryInWhere(t *testing.T) {
	setup := []string{
		`CREATE TABLE tx(a INTEGER, b TEXT)`,
		`INSERT INTO tx VALUES(1,'x'),(2,'y'),(3,'z'),(4,'w')`,
		`CREATE TABLE map(v INTEGER, t TEXT)`,
		`INSERT INTO map VALUES(1,'one'),(2,'two'),(3,'three')`,
	}
	q := func(sql ...string) []string { return append(append([]string{}, setup...), sql...) }

	driverParity(t, "window-where-correlated", q(
		`SELECT a, sum(a) OVER () FROM tx WHERE EXISTS (SELECT 1 FROM map WHERE v=a)`,
		`SELECT a, sum(a) OVER () FROM tx WHERE a = (SELECT max(v) FROM map WHERE v<a)`,
		`SELECT a, sum(a) OVER () FROM tx WHERE a NOT IN (SELECT v FROM map WHERE v<a)`,
		`SELECT a, sum(a) OVER (ORDER BY a) FROM tx WHERE (SELECT t FROM map WHERE v=a) <> 'two'`,
		`SELECT a, sum(a) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM tx`+
			` WHERE EXISTS (SELECT 1 FROM map WHERE v=a) ORDER BY a`,
		`SELECT a, count(*) OVER (PARTITION BY a%2) FROM tx`+
			` WHERE a > (SELECT min(v) FROM map WHERE t<>'one') ORDER BY a`,
		`SELECT tx.a, m.t, row_number() OVER (ORDER BY tx.a) FROM tx LEFT JOIN map AS m ON m.v=tx.a`+
			` WHERE EXISTS (SELECT 1 FROM map WHERE v=tx.a) OR tx.a=4 ORDER BY tx.a`,
		`SELECT a, sum(a) OVER () FROM tx WHERE a IN (SELECT v FROM map WHERE t<>'two') ORDER BY a`,
	))

	driverParity(t, "window-operand-subquery", q(
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx ORDER BY a`,
		`SELECT a, sum(a) OVER win FROM tx WINDOW win AS (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) ORDER BY a`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a)) FROM tx ORDER BY a`,
		`SELECT a, sum(a) OVER (PARTITION BY a IN (SELECT v FROM map)) FROM tx ORDER BY a`,
		`SELECT a, sum(a) OVER (PARTITION BY EXISTS(SELECT 1 FROM map WHERE v=a)) FROM tx ORDER BY a`,
		`SELECT a, lead(a, (SELECT max(v) FROM map)) OVER (ORDER BY a) FROM tx ORDER BY a`,
		`SELECT a, nth_value(a, (SELECT min(v) FROM map)) OVER (ORDER BY a) FROM tx ORDER BY a`,
		`SELECT a, sum(a) FILTER (WHERE a IN (SELECT v FROM map)) OVER (ORDER BY a) FROM tx ORDER BY a`,
		`SELECT a, group_concat(b, (SELECT t FROM map WHERE v=1)) OVER (ORDER BY a) FROM tx ORDER BY a`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx ORDER BY 1`,
		`WITH m2 AS (SELECT * FROM map) SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM m2 WHERE v=a) ORDER BY a) FROM tx ORDER BY a`,
	))

	driverParity(t, "window-spec-subquery-two-levels-out", q(
		`SELECT a, (SELECT sum(m.v) OVER (ORDER BY (SELECT t FROM map WHERE v=tx.a)) FROM map AS m LIMIT 1) FROM tx ORDER BY a`,
		`SELECT a, (SELECT count(*) OVER (PARTITION BY (SELECT t FROM map WHERE v=tx.a)) FROM map AS m LIMIT 1) FROM tx ORDER BY a`,
	))
}
