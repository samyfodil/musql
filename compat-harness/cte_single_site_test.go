package compat

import (
	"fmt"
	"testing"
)

// TestCTEReferencedOnceInsideAnother: TPC-DS q23's shape. A CTE named once,
// inside the body of another CTE that the main statement names twice (one
// UNION ALL arm each), is planned with its own row estimate, as the first copy
// C codes plans it; and an arm's IN subquery naming a CTE of the statement
// still tells which columns the arm uses. Both are what make C's plan, and so
// the order its REAL sums add in, followable (issue #169).
func TestCTEReferencedOnceInsideAnother(t *testing.T) {
	pre := []string{
		"CREATE TABLE ss(cust INT, item INT, qty INT, price REAL, d INT)",
		"CREATE TABLE cs(cust INT, item INT, qty INT, price REAL, d INT)",
		"CREATE TABLE ws(cust INT, item INT, qty INT, price REAL, d INT)",
		"CREATE TABLE cust(c_sk INTEGER PRIMARY KEY, last TEXT, first TEXT)",
		"CREATE TABLE dd(d INTEGER PRIMARY KEY, y INT, m INT)",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 60) INSERT INTO dd SELECT i, 1999 + i % 3, 1 + i % 12 FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 40) INSERT INTO cust SELECT i, 'L' || (i % 7), 'F' || (i % 5) FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 900) INSERT INTO ss SELECT 1 + (i * 7) % 40, 1 + i % 23, 1 + i % 5, (i % 13) * 0.1 + 0.07, 1 + (i * 11) % 60 FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 500) INSERT INTO cs SELECT 1 + (i * 3) % 40, 1 + i % 23, 1 + i % 4, (i % 11) * 0.3 + 0.01, 1 + (i * 7) % 60 FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 500) INSERT INTO ws SELECT 1 + (i * 13) % 40, 1 + i % 23, 1 + i % 3, (i % 9) * 0.7 + 0.03, 1 + (i * 5) % 60 FROM g",
	}
	with := `WITH freq AS (SELECT item, d, count(*) cnt FROM ss GROUP BY item, d HAVING count(*) > 1),
mx AS (SELECT max(csales) m FROM (SELECT cust.c_sk, sum(qty * price) csales FROM ss, cust, dd
        WHERE ss.cust = cust.c_sk AND ss.d = dd.d AND dd.y IN (2000, 2001) GROUP BY cust.c_sk)),
best AS (SELECT c_sk, sum(qty * price) s FROM ss, cust, mx WHERE ss.cust = c_sk
         GROUP BY c_sk HAVING sum(qty * price) > 0.5 * max(m)) `
	arm := func(t string) string {
		return fmt.Sprintf(`SELECT last, first, sum(%[1]s.qty * %[1]s.price) sales FROM %[1]s, cust, dd, best
WHERE dd.y = 2000 AND %[1]s.d = dd.d AND %[1]s.cust = best.c_sk AND %[1]s.cust = cust.c_sk
  AND %[1]s.item IN (SELECT item FROM freq) GROUP BY last, first`, t)
	}
	for i, q := range []string{
		with + "SELECT last, first, sales FROM (" + arm("cs") + " UNION ALL " + arm("ws") + ") ORDER BY 1, 2, 3",
		with + "SELECT c_sk, s FROM best ORDER BY 1",
		with + "SELECT * FROM (" + arm("cs") + ") ORDER BY 1, 2",
	} {
		differ(t, fmt.Sprintf("q23-%d", i), append(append([]string(nil), pre...), q))
	}
}
