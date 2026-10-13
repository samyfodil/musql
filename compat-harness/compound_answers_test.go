package compat

import (
	"fmt"
	"testing"
)

// The three shapes below were wrong or declined against C SQLite 3.53.3:
//
//   - count(*) over a UNION ALL deletes each arm's result list
//     (countOfViewOptimization, select.c:7137), but an aggregate written in an
//     arm that names the enclosing query's column has already made that query
//     an aggregate (resolve.c:1355-1361): one row, not one per row of t1.
//   - disableUnusedSubqueryResultColumns (select.c:5305) turns a subquery
//     column the enclosing query never names into NULL, so an error its
//     expression would raise (abs of the smallest integer) is never raised.
//   - PRAGMA table_info of a view over a view whose column is named count(*)
//     answered nothing.

var aggAssocSetup = []string{
	"CREATE TABLE t1(c1 INT, c2 TEXT)",
	"INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c')",
	"CREATE TABLE t2(d1 INT)",
	"INSERT INTO t2 VALUES(10),(20)",
}

// TestCountOfViewAggregateAssociation crosses aggregate calls with compound
// operators, arm positions and the query shapes that can hold the subquery.
func TestCountOfViewAggregateAssociation(t *testing.T) {
	aggs := []string{"max(c1)", "min(c1)", "sum(c1)", "count(c1)", "avg(c1)", "total(c1)", "group_concat(c2)", "count(DISTINCT c1)", "max(c1+1)"}
	var bodies []string
	for _, a := range aggs {
		bodies = append(bodies,
			"SELECT 1 UNION ALL SELECT "+a,
			"SELECT "+a+" UNION ALL SELECT 1",
			"SELECT "+a+" UNION ALL SELECT "+a,
			"SELECT 1 UNION SELECT "+a,
			"SELECT 1 EXCEPT SELECT "+a,
		)
	}
	bodies = append(bodies,
		"SELECT 1 UNION ALL SELECT max(c1) UNION ALL SELECT min(c1)",
		"SELECT 1 UNION ALL SELECT max(c1) WHERE 1",
		"SELECT max(d1) FROM t2 UNION ALL SELECT max(c1)",
		"SELECT 1 UNION ALL SELECT max(d1) FROM t2",
	)
	wraps := []string{
		"(SELECT count(*) FROM (%s))",
		"(SELECT count(*) FROM (%s) AS d)",
		"(SELECT count(*) FROM (%s) WHERE 1)",
		"(SELECT count(*) FROM (SELECT * FROM (%s)))",
		"(SELECT * FROM (%s))",
		"(SELECT max(1) FROM (%s))",
		"(%s)",
		"EXISTS(SELECT 1 FROM (%s))",
		"1 IN (SELECT 1 FROM (%s))",
	}
	outers := []string{
		"SELECT %s FROM t1",
		"SELECT %s FROM t1 WHERE c1>1",
		"SELECT %s FROM t1, t2",
		"SELECT %s FROM t1 LEFT JOIN t2 ON c1=d1",
		"SELECT %s FROM (SELECT * FROM t1)",
	}
	for i, b := range bodies {
		for j, w := range wraps {
			t.Run(fmt.Sprintf("body%d/wrap%d", i, j), func(t *testing.T) {
				stmts := append([]string{}, aggAssocSetup...)
				for _, o := range outers {
					stmts = append(stmts, fmt.Sprintf(o, fmt.Sprintf(w, b)))
				}
				differAllowingDeclines(t, "agg-assoc", stmts)
			})
		}
	}
}

// TestCountOfViewAggregateAssociationWrites: the rewritten statement's row
// count also decides how many rows a write inserts.
func TestCountOfViewAggregateAssociationWrites(t *testing.T) {
	differ(t, "insert-select", append(append([]string{}, aggAssocSetup...),
		"INSERT INTO t1(c1) SELECT (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT max(c1))) FROM t1",
		"SELECT c1 FROM t1 ORDER BY 1",
	))
}

const unusedColBad = "abs(-9223372036854775807-1)"

// TestUnusedSubqueryColumns crosses subquery shapes with the ways the
// enclosing query can use their columns, as a derived table and as a view.
func TestUnusedSubqueryColumns(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(k INT, v TEXT, w REAL)",
		"INSERT INTO t VALUES(1,'a',1.5),(2,'b',2.5),(3,'c',3.5)",
		"CREATE TABLE u(k INT, z TEXT)",
		"INSERT INTO u VALUES(1,'p'),(3,'q')",
	}
	subs := []string{
		"SELECT k, v, " + unusedColBad + " AS x FROM t",
		"SELECT k, v, " + unusedColBad + " FROM t",
		"SELECT k, v, " + unusedColBad + " AS x FROM t WHERE k>1",
		"SELECT k, v, " + unusedColBad + " AS x FROM t LIMIT 2 OFFSET 1",
		"SELECT k, v, " + unusedColBad + " AS x FROM t ORDER BY 1",
		"SELECT k, v, " + unusedColBad + " AS x FROM t ORDER BY 3",
		"SELECT k, v, " + unusedColBad + " AS x FROM t ORDER BY k",
		"SELECT k, v, " + unusedColBad + " AS x FROM t ORDER BY x",
		"SELECT k, v, " + unusedColBad + " AS x FROM t UNION ALL SELECT k, v, 1 FROM u",
		"SELECT k, v, 1 AS x FROM u UNION ALL SELECT k, v, " + unusedColBad + " FROM t",
		"SELECT k, v, 1 AS x FROM u UNION ALL SELECT k, v, 2 FROM t UNION ALL SELECT k, v, " + unusedColBad + " FROM t",
		"SELECT k, v, " + unusedColBad + " AS x FROM t UNION SELECT k, v, 1 FROM u",
		"SELECT k, v, " + unusedColBad + " AS x FROM t EXCEPT SELECT k, v, 1 FROM u",
		"SELECT DISTINCT k, v, " + unusedColBad + " AS x FROM t",
		"SELECT k, v, " + unusedColBad + " AS x FROM t GROUP BY k",
		"SELECT max(k) AS k, v, " + unusedColBad + " AS x FROM t",
		"SELECT t.k, z, " + unusedColBad + " AS x FROM t JOIN u USING(k)",
		"SELECT t.k, z, " + unusedColBad + " AS x FROM t LEFT JOIN u ON t.k=u.k",
		"SELECT k, v, " + unusedColBad + " AS x, row_number() OVER () AS r FROM t",
		"SELECT k, v, (SELECT " + unusedColBad + ") AS x FROM t",
		"SELECT k, v, CASE WHEN k=2 THEN " + unusedColBad + " ELSE 1 END AS x FROM t",
		"SELECT k, v, k+9223372036854775807 AS x FROM t",
		"SELECT k AS a, v AS a, " + unusedColBad + " AS a FROM t",
		"SELECT k+0, v||'', " + unusedColBad + " FROM t",
	}
	uses := []string{
		"SELECT count(*) FROM %s",
		"SELECT count(k) FROM %s",
		"SELECT k FROM %s",
		"SELECT k, v FROM %s",
		"SELECT * FROM %s",
		"SELECT x FROM %s",
		"SELECT 1 FROM %s",
		"SELECT v FROM %s WHERE k>1",
		"SELECT v FROM %s WHERE x>1",
		"SELECT v FROM %s ORDER BY k",
		"SELECT k, count(*) FROM %s GROUP BY k",
		"SELECT max(k) FROM %s",
		"SELECT d.* FROM %s AS d",
		"SELECT d.k FROM %s AS d",
		"SELECT d.k, e.z FROM %s AS d JOIN u AS e USING(k)",
		"SELECT d.k FROM %s AS d JOIN u AS e ON d.k=e.k",
		"SELECT d.k FROM u AS e LEFT JOIN %s AS d ON d.k=e.k",
		"SELECT (SELECT d.v) FROM %s AS d",
		"SELECT k FROM %s LIMIT 1",
		"SELECT DISTINCT k FROM %s",
		"SELECT k FROM %s AS d WHERE EXISTS (SELECT 1 FROM u WHERE u.k=d.k)",
	}
	wraps := []string{
		"%s",
		"SELECT * FROM (%s)",
		"SELECT 1 WHERE EXISTS (%s)",
		"SELECT 1 WHERE 1 IN (%s)",
		"WITH c AS (%s) SELECT * FROM c",
		"SELECT 1 FROM t WHERE (%s) IS NOT NULL",
	}
	for i, s := range subs {
		t.Run(fmt.Sprintf("sub%d", i), func(t *testing.T) {
			stmts := append([]string{}, setup...)
			stmts = append(stmts, "CREATE VIEW vw AS "+s)
			for n, u := range uses {
				stmts = append(stmts,
					fmt.Sprintf(u, "("+s+")"),
					fmt.Sprintf(u, "vw"),
					fmt.Sprintf(wraps[n%len(wraps)], fmt.Sprintf(u, "("+s+")")),
					fmt.Sprintf(wraps[(n+3)%len(wraps)], fmt.Sprintf(u, "vw")),
				)
			}
			differAllowingDeclines(t, "unused-columns", stmts)
		})
	}
}

// TestExistsDoesNotCodeItsResultList: EXISTS delivers rows to SRT_Exists, and
// selectInnerLoop codes no result expression for it (select.c:1202).
func TestExistsDoesNotCodeItsResultList(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(k INT)",
		"INSERT INTO t VALUES(1),(2)",
	}
	for _, q := range []string{
		"SELECT k FROM t WHERE EXISTS (SELECT " + unusedColBad + " FROM t)",
		"SELECT EXISTS (SELECT " + unusedColBad + " FROM t)",
		"SELECT NOT EXISTS (SELECT " + unusedColBad + " FROM t WHERE k > 5)",
		"SELECT EXISTS (SELECT " + unusedColBad + ", k FROM t WHERE k = 2)",
		"SELECT EXISTS (SELECT (SELECT " + unusedColBad + ") FROM t)",
		"SELECT EXISTS (SELECT DISTINCT " + unusedColBad + " FROM t)",
		"SELECT EXISTS (SELECT " + unusedColBad + " FROM t ORDER BY 1)",
		"SELECT EXISTS (SELECT " + unusedColBad + " UNION SELECT 1)",
		"SELECT EXISTS (SELECT zz FROM t)",
		"SELECT EXISTS (SELECT " + unusedColBad + ", zz FROM t)",
	} {
		t.Run(q, func(t *testing.T) {
			differ(t, "exists", append(append([]string{}, setup...), q))
		})
	}
}

// TestPragmaTableInfoOfViews: a view's columns are named by resolving its
// body, whatever the FROM holds, and an expression-named column keeps its name
// through other views.
func TestPragmaTableInfoOfViews(t *testing.T) {
	exprs := []string{
		"count(*)", "max(a)", "min(b)", "sum(c)", "a+b", "a+1", "a", "g", "e", "d", "i", "rowid", "t1.a", "*",
		"cast(a AS TEXT)", "cast(c AS NUMERIC)", "'lit'", "1.5", "NULL",
		"(SELECT max(a) FROM t1)", "(SELECT g FROM t1)", "CASE WHEN a THEN b ELSE c END", "-a", "+g", "a IS NULL",
		"\"count(*)\"", "a AS \"a b\"", "a AS \"count(*)\"", "count(*) AS \"max(a)\"", "b AS a", "likely(a)",
	}
	setup := []string{
		"CREATE TABLE t1(a INT, b TEXT, c REAL, d, e NUMERIC, f BLOB, g DECIMAL(10,2), i INTEGER PRIMARY KEY)",
	}
	for n, e := range exprs {
		t.Run(fmt.Sprintf("expr%d", n), func(t *testing.T) {
			stmts := append([]string{}, setup...)
			stmts = append(stmts,
				"CREATE VIEW v1 AS SELECT "+e+" FROM t1",
				"CREATE VIEW v2 AS SELECT * FROM v1",
				"CREATE VIEW v3 AS SELECT * FROM (SELECT * FROM v2)",
				"CREATE VIEW v4 AS SELECT * FROM v1 UNION ALL SELECT * FROM v2",
				"CREATE VIEW v5 AS SELECT count(*) FROM v1",
				"CREATE VIEW v6 AS SELECT x.* FROM v2 AS x",
				"CREATE VIEW v7 AS WITH c AS (SELECT * FROM v1) SELECT * FROM c",
				"CREATE VIEW v8 AS SELECT * FROM v1 AS p JOIN t1 ON 1",
				"CREATE VIEW v9(p, q, r) AS SELECT * FROM v1",
			)
			for v := 1; v <= 9; v++ {
				stmts = append(stmts,
					fmt.Sprintf("PRAGMA table_info(v%d)", v),
					fmt.Sprintf("PRAGMA table_xinfo(v%d)", v),
					fmt.Sprintf("SELECT * FROM pragma_table_info('v%d')", v),
					fmt.Sprintf("SELECT * FROM pragma_table_xinfo('v%d')", v),
				)
			}
			differ(t, "view-info", stmts)
		})
	}
}

// TestPragmaTableInfoOfBrokenViews: naming a view's columns resolves its body,
// so an unknown or ambiguous name in it fails the PRAGMA, run as a query or
// not.
func TestPragmaTableInfoOfBrokenViews(t *testing.T) {
	setup := []string{
		"CREATE TABLE t1(a, b)",
		"CREATE TABLE t2(a, b)",
		"CREATE VIEW v0 AS SELECT a, b FROM t1",
		"CREATE VIEW vi AS SELECT a, b FROM v0 JOIN t2 USING(a)",
		"CREATE VIEW vj AS SELECT zz FROM v0",
		"CREATE VIEW vk AS SELECT a FROM nosuchtable",
		"CREATE VIEW vl(p) AS SELECT t1.* FROM t1",
	}
	for _, q := range []string{
		"PRAGMA table_info(vi)", "PRAGMA table_xinfo(vj)", "PRAGMA table_info(vk)", "PRAGMA table_info(vl)",
		"SELECT * FROM pragma_table_info('vi')", "SELECT * FROM pragma_table_info('vj')",
		"SELECT * FROM pragma_table_info('vk')", "SELECT * FROM pragma_table_info('vl')",
	} {
		t.Run(q, func(t *testing.T) {
			differ(t, "broken-view", append(append([]string{}, setup...), q))
		})
	}
}
