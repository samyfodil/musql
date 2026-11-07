package compat

import "testing"

// tpchShapesSetup is a small TPC-H: REAL prices whose sums depend on the order
// they are added in, and lineitem's two-column PRIMARY KEY, an index the
// planner may walk.
var tpchShapesSetup = []string{
	"CREATE TABLE customer(c_custkey INTEGER PRIMARY KEY, c_nationkey INT, c_mktsegment TEXT)",
	"CREATE TABLE orders(o_orderkey INTEGER PRIMARY KEY, o_custkey INT, o_orderdate TEXT, o_shippriority INT, o_orderpriority TEXT)",
	"CREATE TABLE lineitem(l_orderkey INT, l_linenumber INT, l_suppkey INT, l_extendedprice REAL, l_discount REAL, l_shipdate TEXT, l_commitdate TEXT, l_receiptdate TEXT, PRIMARY KEY(l_orderkey, l_linenumber))",
	"CREATE TABLE supplier(s_suppkey INTEGER PRIMARY KEY, s_nationkey INT)",
	"CREATE TABLE nation(n_nationkey INTEGER PRIMARY KEY, n_name TEXT)",
	"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<300) INSERT INTO customer SELECT i, i%7, CASE i%3 WHEN 0 THEN 'BUILDING' ELSE 'AUTO' END FROM s",
	"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<1500) INSERT INTO orders SELECT i, 1+(i*37)%300, date('1994-01-01', '+'||(i%700)||' days'), i%2, 'P'||(i%5) FROM s",
	"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<6000) INSERT INTO lineitem SELECT 1+(i*7)%1500, i, 1+i%40, (i*13%997)*1.37 + 0.01*(i%9), (i%10)*0.01, date('1994-01-01', '+'||(i%730)||' days'), date('1994-01-01', '+'||(i%711)||' days'), date('1994-01-01', '+'||(i%719)||' days') FROM s",
	"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<40) INSERT INTO supplier SELECT i, i%7 FROM s",
	"WITH RECURSIVE s(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM s WHERE i<6) INSERT INTO nation SELECT i, 'N'||i FROM s",
}

// TestTPCHShapesOrderedByAlias pins aggregate joins ordered by a result
// alias: the alias credits no column (resolveAsName), so the planner follows
// C's plan, and the REAL sums, added in C's row order, are C's to the bit.
func TestTPCHShapesOrderedByAlias(t *testing.T) {
	queries := []string{
		`SELECT l_orderkey, sum(l_extendedprice*(1-l_discount)) AS revenue, o_orderdate, o_shippriority FROM customer, orders, lineitem
		 WHERE c_mktsegment='BUILDING' AND c_custkey=o_custkey AND l_orderkey=o_orderkey AND o_orderdate < '1995-03-15' AND l_shipdate > '1995-03-15'
		 GROUP BY l_orderkey, o_orderdate, o_shippriority ORDER BY revenue DESC, o_orderdate LIMIT 10`,
		`SELECT n_name, sum(l_extendedprice*(1-l_discount)) AS revenue FROM customer, orders, lineitem, supplier, nation
		 WHERE c_custkey=o_custkey AND l_orderkey=o_orderkey AND l_suppkey=s_suppkey AND c_nationkey=s_nationkey AND s_nationkey=n_nationkey
		 GROUP BY n_name ORDER BY revenue DESC`,
		`SELECT c_custkey, sum(l_extendedprice*(1-l_discount)) AS revenue FROM customer, orders, lineitem
		 WHERE c_custkey=o_custkey AND l_orderkey=o_orderkey AND o_orderdate >= '1994-10-01'
		 GROUP BY c_custkey ORDER BY revenue DESC LIMIT 20`,
		`SELECT l_suppkey, sum(l_extendedprice*l_discount) AS value FROM lineitem, supplier WHERE l_suppkey=s_suppkey
		 GROUP BY l_suppkey HAVING sum(l_extendedprice*l_discount) > (SELECT sum(l_extendedprice*l_discount)*0.02 FROM lineitem, supplier WHERE l_suppkey=s_suppkey)
		 ORDER BY value DESC`,
		`SELECT o_orderpriority, count(*) AS order_count FROM orders WHERE o_orderdate >= '1994-07-01' AND o_orderdate < '1994-10-01'
		 AND EXISTS (SELECT * FROM lineitem WHERE l_orderkey=o_orderkey AND l_commitdate < l_receiptdate) GROUP BY o_orderpriority ORDER BY o_orderpriority`,
		`SELECT s_suppkey, count(*) AS numwait FROM supplier, lineitem l1, orders WHERE s_suppkey=l1.l_suppkey AND o_orderkey=l1.l_orderkey
		 AND l1.l_receiptdate > l1.l_commitdate AND EXISTS (SELECT * FROM lineitem l2 WHERE l2.l_orderkey=l1.l_orderkey AND l2.l_suppkey<>l1.l_suppkey)
		 AND NOT EXISTS (SELECT * FROM lineitem l3 WHERE l3.l_orderkey=l1.l_orderkey AND l3.l_suppkey<>l1.l_suppkey AND l3.l_receiptdate > l3.l_commitdate)
		 GROUP BY s_suppkey ORDER BY numwait DESC, s_suppkey LIMIT 10`,
		`SELECT o_orderkey AS k, sum(l_extendedprice) AS total FROM orders JOIN lineitem ON l_orderkey=o_orderkey GROUP BY k ORDER BY total, k LIMIT 15`,
	}
	for _, q := range queries {
		differ(t, q, append(append([]string(nil), tpchShapesSetup...), q))
	}
}

// TestExistsOverEmptyTable pins an EXISTS that finds nothing, after which the
// query asks whether its table is empty at all (OpIfEmpty), which one row
// answers: a table empty from the start, emptied by DELETEs after its rows
// were compacted, and holding rows only in the delta log.
func TestExistsOverEmptyTable(t *testing.T) {
	differ(t, "exists over an empty table", []string{
		"CREATE TABLE o(k INTEGER PRIMARY KEY, d TEXT)",
		"CREATE TABLE l(k INT, n INT, PRIMARY KEY(k, n))",
		"CREATE TABLE e(k INT)",
		"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<200) INSERT INTO o SELECT i, 'd'||(i%5) FROM s",
		"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<600) INSERT INTO l SELECT 1+(i*7)%250, i FROM s",
		"SELECT d, count(*) FROM o WHERE EXISTS (SELECT * FROM l WHERE l.k = o.k AND l.n % 3 = 0) GROUP BY d ORDER BY d",
		"SELECT count(*) FROM o WHERE EXISTS (SELECT * FROM e WHERE e.k = o.k)",
		"SELECT count(*) FROM o WHERE NOT EXISTS (SELECT * FROM e WHERE e.k = o.k)",
		"VACUUM",
		"DELETE FROM l",
		"SELECT count(*) FROM o WHERE EXISTS (SELECT * FROM l WHERE l.k = o.k)",
		"INSERT INTO e VALUES (3), (5)",
		"SELECT k FROM o WHERE EXISTS (SELECT * FROM e WHERE e.k = o.k) ORDER BY k",
		"SELECT count(*) FROM o WHERE NOT EXISTS (SELECT * FROM e WHERE e.k = o.k)",
	})
}
