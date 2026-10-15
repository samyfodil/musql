package compat

import (
	"fmt"
	"strings"
	"testing"
)

// TestDerivedColumnNamedBeforeResolution: a derived table's or a CTE's
// columns are named while the body is still unresolved (selectExpander), so
// an unaliased identifier names its column after its token as written --
// "ABC" over a column abc, xyz for a double-quoted string with no such
// column -- where a view, named after resolution, keeps the column's own name
// or the string's span.
func TestDerivedColumnNamedBeforeResolution(t *testing.T) {
	pre := []string{"CREATE TABLE t(abc, d)", "INSERT INTO t VALUES(1,2)",
		"CREATE TABLE t2(id INTEGER PRIMARY KEY, x)", "INSERT INTO t2 VALUES(5,6)"}
	for i, q := range []string{
		`SELECT * FROM (SELECT ABC FROM t)`,
		`SELECT * FROM (SELECT t.ABC FROM t)`,
		`SELECT * FROM (SELECT main.t.ABC FROM t)`,
		`SELECT * FROM (SELECT "abc" FROM t)`,
		`SELECT * FROM (SELECT "xyz" FROM t)`,
		`SELECT * FROM (SELECT "xyz" COLLATE nocase FROM t)`,
		`SELECT * FROM (SELECT "a""b" FROM t)`,
		`SELECT * FROM (SELECT (ABC) FROM t)`,
		`SELECT * FROM (SELECT likely(ABC) FROM t)`,
		`WITH c AS (SELECT "xyz", "ABC" FROM t) SELECT * FROM c`,
		`WITH c(p, q) AS (SELECT "xyz", "ABC" FROM t) SELECT * FROM c`,
		`SELECT * FROM (SELECT "xyz" FROM t UNION ALL SELECT 1)`,
		`SELECT * FROM (SELECT 1 UNION ALL SELECT "xyz")`,
		`SELECT * FROM (SELECT "xyz" FROM t) WHERE xyz = 'xyz'`,
		`SELECT xyz FROM (SELECT "xyz" FROM t)`,
		`SELECT * FROM (SELECT rowid FROM t2)`,
		`SELECT * FROM (SELECT oid, t2.ROWID, _rowid_ FROM t2)`,
		`SELECT * FROM (SELECT ID, X FROM t2 ORDER BY id)`,
		`SELECT * FROM (SELECT ABC, abc FROM t)`,
		`SELECT * FROM t, (SELECT ABC FROM t) AS q`,
		`SELECT q.* FROM t, (SELECT D FROM t) AS q WHERE q.d = 2`,
		`SELECT * FROM (SELECT ABC FROM t) ORDER BY abc`,
		`SELECT * FROM (SELECT ABC FROM t GROUP BY abc)`,
		`SELECT * FROM (SELECT * FROM (SELECT ABC FROM t))`,
	} {
		differ(t, fmt.Sprintf("name-%d", i), append(append([]string(nil), pre...), q))
	}
	differ(t, "ctas-and-view", append(append([]string(nil), pre...),
		`CREATE TABLE ct AS SELECT * FROM (SELECT ABC, "xyz" FROM t)`,
		`SELECT sql FROM sqlite_master WHERE name = 'ct'`,
		`CREATE VIEW vv AS SELECT * FROM (SELECT ABC, "q" FROM t)`,
		`SELECT * FROM vv`, `PRAGMA table_info(vv)`,
		`CREATE VIEW v2 AS SELECT "xyz", ABC FROM t`, `SELECT * FROM v2`))
}

// TestDerivedRealColumnReadsReal: a REAL column of a subquery's table is read
// with OP_RealAffinity after it (sqlite3ColumnDefault), so an integral REAL
// that came back out of a sorter or a record as an integer -- a VIRTUAL REAL
// generated column's MEM_IntReal -- reads as REAL again.
func TestDerivedRealColumnReadsReal(t *testing.T) {
	pre := []string{"CREATE TABLE t1 (b REAL, c REAL AS (b), d INT)",
		"INSERT INTO t1 VALUES (20.0, 1), (3.5, 2), (7.0, 3)"}
	var cases [][]string
	for _, q := range []string{
		"SELECT typeof(c), c FROM t1 ORDER BY c",
		"SELECT typeof(c), c FROM t1 ORDER BY c LIMIT 2",
		"SELECT typeof(c), c FROM (SELECT c FROM t1 ORDER BY c)",
		"SELECT typeof(c), c FROM (SELECT c FROM t1 ORDER BY c LIMIT 2)",
		"SELECT typeof(c), c FROM (SELECT c FROM t1 LIMIT 2)",
		"SELECT typeof(c), c FROM (SELECT c FROM t1 ORDER BY d LIMIT 2)",
		"SELECT typeof(c), c FROM (SELECT c, d FROM t1 ORDER BY d DESC LIMIT 2)",
		"SELECT typeof(x), x FROM (SELECT c AS x FROM t1 ORDER BY c LIMIT 2 OFFSET 1)",
		"SELECT typeof(c), c FROM (SELECT c FROM t1) ORDER BY c",
		"SELECT typeof(c), c FROM (SELECT c FROM t1 ORDER BY c) LIMIT 2",
		"SELECT typeof(c), c FROM (SELECT DISTINCT c FROM t1)",
		"SELECT typeof(c), c FROM (SELECT c FROM t1 GROUP BY c)",
		"SELECT typeof(c), c FROM (SELECT c FROM t1 UNION ALL SELECT c FROM t1 ORDER BY 1 LIMIT 3)",
		"WITH q AS (SELECT c FROM t1 ORDER BY c LIMIT 2) SELECT typeof(c), c FROM q",
		"WITH q AS MATERIALIZED (SELECT c FROM t1) SELECT typeof(c), c FROM q",
		"SELECT typeof(m), m FROM (SELECT max(c) AS m FROM t1)",
		"SELECT typeof(c) FROM t1 WHERE c IN (SELECT c FROM t1 ORDER BY c LIMIT 1)",
		"SELECT (SELECT typeof(c) FROM (SELECT c FROM t1 ORDER BY c LIMIT 1))",
		"SELECT c + 0, typeof(c) FROM (SELECT c FROM t1 ORDER BY c LIMIT 2)",
		"SELECT typeof(c) FROM (SELECT c FROM t1 ORDER BY c LIMIT 2), (SELECT 1)",
	} {
		cases = append(cases, append(append([]string(nil), pre...), q))
	}
	cases = append(cases, append(append([]string(nil), pre...),
		"CREATE VIEW v AS SELECT c FROM t1 ORDER BY c", "SELECT typeof(c), c FROM v"))
	// A compound's column is REAL only as sqlite3SubqueryColumnTypes folds
	// it: a leftmost CAST makes SQLITE_AFF_FLEXNUM, which reads back as is.
	r := []string{"CREATE TABLE r(b REAL, i INT, s TEXT)", "INSERT INTO r VALUES(1.5, 5, 'x')"}
	for _, q := range []string{
		"SELECT typeof(c), c FROM (SELECT b AS c FROM r UNION ALL SELECT i FROM r)",
		"SELECT typeof(c), c FROM (SELECT b AS c FROM r UNION ALL SELECT i FROM r UNION ALL SELECT s FROM r)",
		"SELECT typeof(c), c FROM (SELECT b AS c FROM r UNION ALL SELECT 7)",
		"SELECT typeof(c), c FROM (SELECT b AS c FROM r UNION ALL SELECT 'q')",
		"SELECT typeof(c), c FROM (SELECT b AS c FROM r UNION SELECT i FROM r)",
		"SELECT typeof(c), c FROM (SELECT CAST(i AS REAL) AS c FROM r UNION ALL SELECT i FROM r)",
		"SELECT typeof(c), c FROM (SELECT CAST(i AS REAL) AS c FROM r UNION ALL SELECT i FROM r ORDER BY 1)",
		"WITH q AS (SELECT CAST(i AS REAL) AS c FROM r UNION ALL SELECT i FROM r) SELECT typeof(c), c FROM q",
		"WITH q AS (SELECT b AS c FROM r UNION ALL SELECT i FROM r) SELECT typeof(c), c FROM q",
		"SELECT typeof(c), c FROM (SELECT max(b, i) AS c FROM r)",
	} {
		cases = append(cases, append(append([]string(nil), r...), q))
	}
	sweepAll(t, cases)
}

// TestTempTriggerHashOrder: TEMP triggers on a non-temp table fire in the
// reverse of the walk of C's temp schema trigger hash (sqlite3TriggerList),
// which from five triggers on has buckets (hash.c), through drops, re-creates,
// an ALTER and a ROLLBACK that reload the temp schema.
func TestTempTriggerHashOrder(t *testing.T) {
	trig := func(name string) string {
		return fmt.Sprintf("CREATE TEMP TRIGGER %s AFTER INSERT ON t BEGIN INSERT INTO log VALUES('%s'); END", name, name)
	}
	names := []string{"a1", "b2", "c3", "d4", "e5", "f6", "Hx", "i9", "jj", "k_10", "zz11"}
	for n := 2; n <= len(names); n++ {
		s := []string{"CREATE TABLE t(x)", "CREATE TABLE log(n)",
			"CREATE TRIGGER m1 AFTER INSERT ON t BEGIN INSERT INTO log VALUES('m1'); END"}
		for _, nm := range names[:n] {
			s = append(s, trig(nm))
		}
		s = append(s, "INSERT INTO t VALUES(1)", "SELECT group_concat(n, ' ') FROM log", "DELETE FROM log",
			"DROP TRIGGER "+names[n/2], trig("re"+names[n/2]),
			"INSERT INTO t VALUES(2)", "SELECT group_concat(n, ' ') FROM log", "DELETE FROM log",
			"ALTER TABLE t ADD COLUMN y",
			"INSERT INTO t VALUES(3, 3)", "SELECT group_concat(n, ' ') FROM log", "DELETE FROM log",
			"BEGIN", "CREATE TEMP TABLE zz(a)", "ROLLBACK",
			"INSERT INTO t VALUES(4, 4)", "SELECT group_concat(n, ' ') FROM log", "DELETE FROM log",
			"BEGIN", trig("k1"), "SAVEPOINT s", "DROP TRIGGER "+names[0], "ROLLBACK TO s",
			"INSERT INTO t VALUES(5, 5)", "SELECT group_concat(n, ' ') FROM log", "COMMIT", "DELETE FROM log",
			"DROP TRIGGER "+names[1],
			"INSERT INTO t VALUES(6, 6)", "SELECT group_concat(n, ' ') FROM log")
		differ(t, fmt.Sprintf("hash-%d", n), s)
	}
	// Many: the bucket count grows to its cap.
	s := []string{"CREATE TABLE t(x)", "CREATE TABLE log(n)"}
	for i := range 40 {
		s = append(s, trig(fmt.Sprintf("tr_%d", i)))
	}
	s = append(s, "INSERT INTO t VALUES(1)", "SELECT group_concat(n, ' ') FROM log", "DELETE FROM log")
	for i := 0; i < 40; i += 3 {
		s = append(s, fmt.Sprintf("DROP TRIGGER tr_%d", i))
	}
	s = append(s, "INSERT INTO t VALUES(2)", "SELECT group_concat(n, ' ') FROM log")
	differ(t, "hash-many", s)
}

// TestWindowManyPartitionsAndRunningFrames is Turso's window-multiple-functions
// over 10000 rows: thousands of partitions, and frames that start at
// UNBOUNDED PRECEDING over large peer groups, each once quadratic here.
func TestWindowManyPartitionsAndRunningFrames(t *testing.T) {
	var vals []string
	for i := 1; i <= 10000; i++ {
		vals = append(vals, fmt.Sprintf("('f%d','l%d','c%d','s%d','%05d',%d)", (i*7919)%300, (i*104729)%500, (i*31)%20, (i*17)%50, (i*2654435761)%100000, 1+(i*37)%100))
	}
	stmts := []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, first_name TEXT, last_name TEXT, city TEXT, state TEXT, zipcode TEXT, age INTEGER)"}
	for i := 0; i < len(vals); i += 500 {
		stmts = append(stmts, "INSERT INTO users (first_name, last_name, city, state, zipcode, age) VALUES "+strings.Join(vals[i:i+500], ","))
	}
	stmts = append(stmts,
		"SELECT first_name, last_name, max(age) OVER (PARTITION BY zipcode), max(age) OVER (PARTITION BY city), min(age) OVER (PARTITION BY state, city ORDER BY last_name), count(*) OVER (PARTITION BY state, city ORDER BY last_name), sum(age) OVER (ORDER BY city), min(age) OVER (ORDER BY city), age FROM users ORDER BY first_name, id LIMIT 10",
		"SELECT id, avg(age) OVER (PARTITION BY zipcode ORDER BY id), total(age) OVER (ORDER BY city ROWS UNBOUNDED PRECEDING), count(age) OVER (PARTITION BY city) FROM users ORDER BY id LIMIT 20",
		"SELECT id, max(last_name) OVER (PARTITION BY lower(city) ORDER BY age RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM users ORDER BY id LIMIT 20",
		"SELECT count(DISTINCT zipcode), max(c) FROM (SELECT zipcode, count(*) OVER (PARTITION BY zipcode) AS c FROM users)",
	)
	differ(t, "window-many", stmts)
}
