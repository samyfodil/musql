// Stream r35d scratch probe. Not a gate -- it prints what each side answers so
// a decline's ROOT can be identified before anything is changed. Run with
// R35D_PROBE=1.
package compat

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func r35dProbe(t *testing.T, setup []string, queries []string) {
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
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	for _, q := range queries {
		ec, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		t.Logf("Q: %s", q)
		if eerr != nil {
			t.Logf("   musql ERR: %v", eerr)
		} else {
			t.Logf("   musql: %v %v", ec, engineRowsToStrings(ev))
		}
		if cerr != nil {
			t.Logf("   cgo    ERR: %v", cerr)
		} else {
			t.Logf("   cgo:    %v %v", cc, cr)
		}
	}
}

func TestR35DProbe(t *testing.T) {
	if os.Getenv("R35D_PROBE") == "" {
		t.Skip("set R35D_PROBE=1")
	}
	t.Run("derivedCorrelated", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(x INTEGER, y INTEGER)",
			"INSERT INTO t1 VALUES(1,10),(2,20),(NULL,30)",
			"CREATE TABLE invoice(name TEXT, amount INT)",
			"INSERT INTO invoice VALUES('a',5),('a',4),('b',1)",
		}, []string{
			"SELECT sum((SELECT 1 FROM (SELECT 2 WHERE x IS NULL) WHERE 0)) FROM t1",
			"SELECT (SELECT 1 FROM (SELECT 2 WHERE x IS NULL)) FROM t1",
			"SELECT x, (SELECT q FROM (SELECT x+100 AS q)) FROM t1",
			"SELECT EXISTS ( SELECT * FROM ( SELECT * FROM ( SELECT 1 ) WHERE Col0 = 1 GROUP BY 1 ) WHERE 0 ) FROM (SELECT 1 Col0) GROUP BY 1",
			"SELECT sum(amount), name from invoice group by name having (select v > 6 from (select sum(amount) v) t)",
		})
	})
	t.Run("fromlessChain", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE aa(x INTEGER, y INTEGER)",
			"INSERT INTO aa VALUES(1,10),(2,20)",
			"CREATE TABLE bb(x INTEGER)",
			"INSERT INTO bb VALUES(5),(6)",
		}, []string{
			"SELECT (SELECT sum(x+(SELECT y)) FROM bb) FROM aa",
			"SELECT (SELECT (SELECT y)) FROM aa",
			"SELECT (SELECT x+(SELECT y) FROM bb) FROM aa",
			"SELECT (SELECT sum(x) FROM bb) FROM aa",
		})
	})
	t.Run("compoundFromless", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(a INTEGER)",
			"INSERT INTO t1 VALUES(1),(2),(3)",
		}, []string{
			"SELECT (SELECT avg(a) UNION SELECT min(a) OVER()) FROM t1",
			"SELECT (SELECT avg(a)) FROM t1",
			"SELECT (SELECT a UNION SELECT a) FROM t1",
			"SELECT (SELECT min(a) OVER()) FROM t1",
		})
	})
	t.Run("aggnested41", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE aa(x INT)", "INSERT INTO aa(x) VALUES(123)",
			"CREATE TABLE bb(y INT)", "INSERT INTO bb(y) VALUES(456)",
		}, []string{
			"SELECT (SELECT sum(x+(SELECT y)) FROM bb) FROM aa",
			"SELECT (SELECT sum((SELECT y)) FROM bb) FROM aa",
			"SELECT (SELECT sum((SELECT x)) FROM bb) FROM aa",
			"SELECT (SELECT sum(y+(SELECT x)) FROM bb) FROM aa",
			"SELECT (SELECT sum((SELECT y FROM bb)) FROM bb) FROM aa",
			"SELECT (SELECT count((SELECT y)) FROM bb) FROM aa",
			"SELECT (SELECT sum((SELECT 1)) FROM bb) FROM aa",
		})
	})
	t.Run("compoundHoist", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(a INTEGER)",
			"INSERT INTO t1 VALUES(1),(2),(3)",
			"CREATE TABLE t2(a INTEGER, b INTEGER)",
			"INSERT INTO t2 VALUES(1,10),(1,20),(2,30)",
		}, []string{
			"SELECT (SELECT avg(a) UNION SELECT min(a) OVER()) FROM t1",
			"SELECT (SELECT avg(a) UNION SELECT min(a) OVER ()) FROM t2 GROUP BY a ORDER BY 1",
			"SELECT (SELECT avg(a) UNION ALL SELECT 9) FROM t1",
			"SELECT (SELECT count(a) UNION ALL SELECT 9) FROM t1",
		})
	})
	t.Run("cteAliasRef", func(t *testing.T) {
		r35dProbe(t, nil, []string{
			"SELECT 1 AS c WHERE ( SELECT ( WITH t1(a) AS (VALUES( c )) SELECT ( SELECT t1a.a FROM t1 AS t1a, t1 AS t1x ) FROM t1 AS xyz GROUP BY 1 ) )",
			"SELECT 1 AS c WHERE (SELECT c)",
			"SELECT 1 AS c WHERE (WITH q(a) AS (VALUES(c)) SELECT a FROM q)",
		})
	})
	t.Run("windowShapes", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(x INTEGER, y INTEGER, b INTEGER, c1 TEXT)",
			"INSERT INTO t1 VALUES(1,2,3,'abcd'),(4,5,6,'ABCD')",
			"CREATE TABLE t2(a INTEGER, b INTEGER, d INTEGER)",
			"INSERT INTO t2 VALUES(1,1,1)",
		}, []string{
			"SELECT sum(b) over( ORDER BY ( SELECT max(b) OVER( ORDER BY sum( (SELECT x AS c UNION SELECT 1234 ORDER BY c) ) ) AS e ORDER BY e ) ) FROM t1",
			"SELECT max(c1 COLLATE nocase) IN (SELECT 'aBCd') FROM t1",
			"SELECT max(c1 COLLATE nocase) = 'aBCd' FROM t1",
			"SELECT max(c1 COLLATE nocase) IN ('aBCd') FROM t1",
			"SELECT max(c1) = 'aBCd' FROM t1",
			"SELECT x, max(c1 COLLATE nocase) = 'aBCd' FROM t1 GROUP BY x",
			"SELECT min(c1 COLLATE nocase) < 'B' FROM t1",
			"SELECT group_concat(c1 COLLATE nocase) = 'abcd,ABCD' FROM t1",
			"SELECT rowid, max(b COLLATE nocase)||'' FROM t1 GROUP BY rowid ORDER BY max(b COLLATE nocase)||''",
		})
	})
	t.Run("updateWindowAgg", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(x INTEGER, y INTEGER)",
			"INSERT INTO t1 VALUES(1,2),(3,4)",
			"CREATE TABLE t2(a INTEGER, b INTEGER, d INTEGER)",
			"INSERT INTO t2 VALUES(1,1,1)",
		}, []string{
			"SELECT ( SELECT max( t1.x ) OVER( PARTITION BY sum( (SELECT t1.y) ) ) ) FROM t1",
		})
	})
	t.Run("groupOrderColl", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(g INT, b TEXT, n TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM)",
			"INSERT INTO t1 VALUES(1,'b','b','b '),(2,'A','A','A'),(3,'a','a','a  '),(4,'B','B','B ')",
		}, []string{
			"SELECT g, max(b) FROM t1 GROUP BY g ORDER BY max(b COLLATE nocase)",
			"SELECT g, max(b COLLATE nocase)||'' FROM t1 GROUP BY g ORDER BY max(b COLLATE nocase)||''",
			"SELECT g, max(b) FROM t1 GROUP BY g ORDER BY max(b) COLLATE nocase",
			"SELECT n, count(*) FROM t1 GROUP BY g ORDER BY n",
			"SELECT n AS z, count(*) FROM t1 GROUP BY g ORDER BY z",
			"SELECT n, count(*) FROM t1 GROUP BY g ORDER BY 1",
			"SELECT r, count(*) FROM t1 GROUP BY g ORDER BY 1",
			"SELECT g, min(n) FROM t1 GROUP BY g ORDER BY min(n)",
			"SELECT b, count(*) FROM t1 GROUP BY g ORDER BY b COLLATE nocase DESC",
			"SELECT DISTINCT n, count(*) FROM t1 GROUP BY g ORDER BY n",
			"SELECT g, max(b) FROM t1 GROUP BY g ORDER BY max(b COLLATE rtrim), g",
		})
	})
	t.Run("orderTies", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(g INT, b TEXT, n TEXT COLLATE NOCASE)",
			"INSERT INTO t1 VALUES(1,'b','b'),(2,'A','A'),(3,'a','a'),(4,'B','B')",
		}, []string{
			"SELECT g, b FROM t1 GROUP BY g ORDER BY length(b) DESC",
			"SELECT g, b FROM t1 GROUP BY g ORDER BY length(b)",
			"SELECT g, b FROM t1 GROUP BY g ORDER BY (g>2) DESC",
			"SELECT g, b FROM t1 GROUP BY g ORDER BY (g>2)",
			"SELECT g, b FROM t1 GROUP BY g ORDER BY 0 DESC",
			"SELECT g, n FROM t1 GROUP BY g ORDER BY n DESC",
			"SELECT g, n FROM t1 GROUP BY g ORDER BY n",
			"SELECT g, b FROM t1 GROUP BY g ORDER BY b COLLATE nocase DESC",
			"SELECT g, b FROM t1 GROUP BY g ORDER BY b COLLATE nocase",
		})
	})
	t.Run("limitExpr", func(t *testing.T) {
		r35dProbe(t, []string{
			"CREATE TABLE t1(a INTEGER)",
			"INSERT INTO t1 VALUES(1)",
			"CREATE TABLE t2(b INTEGER)",
			"INSERT INTO t2 VALUES(7),(8)",
		}, []string{
			"SELECT( SELECT max(b) LIMIT ( SELECT total( (SELECT a FROM t1) ) ) ) FROM t2",
			"SELECT * FROM (SELECT b FROM t2 LIMIT 1+0)",
			"SELECT (SELECT b FROM t2 LIMIT 1+0)",
		})
	})
}
