// This file tests name resolution and aggregate-collation in various contexts.
// Each case asserts the oracle's answer, never "this declines".
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// r35dCompare builds both engines and compares queries.
func r35dCompare(t *testing.T, setup []string, qs ...string) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	for _, s := range setup {
		if eerr := edb.Exec(s); eerr != nil {
			t.Fatalf("engine setup %q: %v", s, eerr)
		}
		if _, cerr := cdb.Exec(s); cerr != nil {
			t.Fatalf("cgo setup %q: %v", s, cerr)
		}
	}
	for _, q := range qs {
		flCompareQuery(t, "r35d", edb, cdb, q)
	}
}

// TestR35DCorrelatedIntoDerivedTable tests derived tables inside subqueries
// with a live enclosing row.
func TestR35DCorrelatedIntoDerivedTable(t *testing.T) {
	r35dCompare(t, []string{
		"CREATE TABLE t1(x INTEGER, y INTEGER)",
		"INSERT INTO t1 VALUES(1,10),(2,20),(NULL,30)",
	},
		"SELECT sum((SELECT 1 FROM (SELECT 2 WHERE x IS NULL) WHERE 0)) FROM t1",
		"SELECT count((SELECT 1 FROM (SELECT 2 WHERE x IS NULL))) FROM t1",
		"SELECT (SELECT 1 FROM (SELECT 2 WHERE x IS NULL)) FROM t1",
		"SELECT x, (SELECT q FROM (SELECT x+100 AS q)) FROM t1",
		"SELECT EXISTS ( SELECT * FROM ( SELECT * FROM ( SELECT 1 ) WHERE Col0 = 1 GROUP BY 1 ) WHERE 0 ) FROM (SELECT 1 Col0) GROUP BY 1",
	)
}

// TestR35DFromlessSubqueryInAggArg tests nested subqueries in aggregate arguments.
func TestR35DFromlessSubqueryInAggArg(t *testing.T) {
	r35dCompare(t, []string{
		"CREATE TABLE aa(x INTEGER, y INTEGER)",
		"INSERT INTO aa VALUES(1,10),(2,20)",
		"CREATE TABLE bb(x INTEGER)",
		"INSERT INTO bb VALUES(5),(6)",
	},
		"SELECT (SELECT sum(x+(SELECT y)) FROM bb) FROM aa",
		"SELECT (SELECT sum(x) FROM bb) FROM aa",
		"SELECT (SELECT x+(SELECT y) FROM bb) FROM aa",
		"SELECT (SELECT total(x*(SELECT y)) FROM bb) FROM aa",
		"SELECT (SELECT count(*)+(SELECT y) FROM bb) FROM aa",
	)
}

// TestR35DAggArgSubqueryAssociation tests aggregate argument subquery association.
func TestR35DAggArgSubqueryAssociation(t *testing.T) {
	r35dCompare(t, []string{
		"CREATE TABLE aa(x INT)", "INSERT INTO aa(x) VALUES(123)",
		"CREATE TABLE bb(y INT)", "INSERT INTO bb(y) VALUES(456)",
	},
		"SELECT (SELECT sum(x+(SELECT y)) FROM bb) FROM aa",
		"SELECT (SELECT sum((SELECT y)) FROM bb) FROM aa",
		"SELECT (SELECT sum((SELECT x)) FROM bb) FROM aa",
		"SELECT (SELECT sum(y+(SELECT x)) FROM bb) FROM aa",
		"SELECT (SELECT sum((SELECT y FROM bb)) FROM bb) FROM aa",
		"SELECT (SELECT count((SELECT y)) FROM bb) FROM aa",
		"SELECT (SELECT sum((SELECT 1)) FROM bb) FROM aa",
	)
}

// TestR35DCompoundArmHoist tests aggregate association in UNION queries.
func TestR35DCompoundArmHoist(t *testing.T) {
	r35dCompare(t, []string{
		"CREATE TABLE t1(a INTEGER)",
		"INSERT INTO t1 VALUES(1),(2),(3)",
	},
		"SELECT (SELECT avg(a) UNION SELECT min(a) OVER()) FROM t1",
		"SELECT (SELECT avg(a) UNION ALL SELECT 9) FROM t1",
		"SELECT (SELECT count(a) UNION ALL SELECT 9) FROM t1",
		"SELECT (SELECT avg(a)) FROM t1",
		"SELECT (SELECT a UNION SELECT a) FROM t1",
	)
}

// TestR35DAggCollate tests collation in aggregate function calls.
func TestR35DAggCollate(t *testing.T) {
	r35dCompare(t, []string{
		"CREATE TABLE t1(g INT, c1 TEXT)",
		"INSERT INTO t1 VALUES(1,'abcd'),(1,'ABCD')",
	},
		"SELECT max(c1 COLLATE nocase) IN (SELECT 'aBCd') FROM t1",
		"SELECT group_concat(c1 COLLATE nocase) IN (SELECT 'aBCd') FROM t1",
		"SELECT max(c1 COLLATE nocase) = 'aBCd' FROM t1",
		"SELECT max(c1 COLLATE nocase) IN ('aBCd') FROM t1",
		"SELECT min(c1 COLLATE nocase) < 'B' FROM t1",
		"SELECT max(c1) = 'aBCd' FROM t1",
		"SELECT g, max(c1 COLLATE nocase) = 'aBCd' FROM t1 GROUP BY g",
	)
}

// TestR35DGroupOrderCollation tests collation in GROUP BY ORDER BY clauses.
func TestR35DGroupOrderCollation(t *testing.T) {
	r35dCompare(t, []string{
		"CREATE TABLE t1(g INT, b TEXT, n TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM)",
		"INSERT INTO t1 VALUES(1,'b','b','b '),(2,'A','A','A'),(3,'a','a','a  '),(4,'B','B','B ')",
	},
		"SELECT g, max(b) FROM t1 GROUP BY g ORDER BY max(b COLLATE nocase)",
		"SELECT g, max(b COLLATE nocase)||'' FROM t1 GROUP BY g ORDER BY max(b COLLATE nocase)||''",
		"SELECT g, max(b) FROM t1 GROUP BY g ORDER BY max(b) COLLATE nocase",
		"SELECT n, count(*) FROM t1 GROUP BY g ORDER BY n",
		"SELECT n AS z, count(*) FROM t1 GROUP BY g ORDER BY z",
		"SELECT n, count(*) FROM t1 GROUP BY g ORDER BY 1",
		"SELECT r, count(*) FROM t1 GROUP BY g ORDER BY 1",
		"SELECT g, min(n) FROM t1 GROUP BY g ORDER BY min(n)",
		"SELECT g, max(b) FROM t1 GROUP BY g ORDER BY max(b COLLATE rtrim), g",
		"SELECT g, max(b) FROM t1 GROUP BY g ORDER BY 2 COLLATE nocase",
	)
}
