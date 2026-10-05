package compat

import "testing"

// TestUpdateSetLiveSelfRead compares with C SQLite an UPDATE whose SET
// subquery reads the table being updated AND is correlated to the row: C
// re-evaluates it per row against the live table (EP_VarSelect,
// resolve.c:1403-1404), so each row sees the rows before it already rewritten
// (engine's liveRowCtx, vdbe_live_read.go).
func TestUpdateSetLiveSelfRead(t *testing.T) {
	seed := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)`,
		`INSERT INTO t VALUES(1,10,'x'),(2,20,'y'),(3,30,'z'),(4,40,'w')`,
	}
	cases := []struct{ name, sql string }{
		{"running count", `UPDATE t SET b=b+(SELECT count(*) FROM t t2 WHERE t2.b<=t.b)`},
		{"sum of smaller live values", `UPDATE t AS o SET b=(SELECT total(b) FROM t WHERE a<o.a)+1`},
		{"row value swap", `UPDATE t SET (b,c)=(SELECT c,b FROM t t2 WHERE t2.a=t.a)`},
		{"reads the previous row", `UPDATE t SET b=coalesce((SELECT b FROM t t2 WHERE t2.a=t.a-1),0)+1`},
		{"filtering WHERE, no index", `UPDATE t SET b=(SELECT max(b) FROM t t2 WHERE t2.a<>t.a)+1 WHERE a%2=0`},
		{"nested correlated", `UPDATE t SET b=(SELECT count(*) FROM t x WHERE x.b IN (SELECT y.b FROM t y WHERE y.a<=t.a))`},
		{"FILTER correlation", `UPDATE t AS o SET b=(SELECT sum(b) FILTER (WHERE a<o.a) FROM t)`},
		{"OVER correlation", `UPDATE t AS o SET b=(SELECT sum(b) OVER (PARTITION BY o.a) FROM t LIMIT 1)`},
		{"WINDOW clause correlation", `UPDATE t AS o SET b=(SELECT sum(b) OVER w FROM t WINDOW w AS (ORDER BY o.a) LIMIT 1)`},
		{"EXISTS", `UPDATE t SET b=EXISTS(SELECT 1 FROM t t2 WHERE t2.b>t.b*2)`},
		{"IN", `UPDATE t SET b=(t.b+10 IN (SELECT b FROM t))`},
		{"compound", `UPDATE t AS o SET b=(SELECT b FROM t WHERE a=o.a-1 UNION ALL SELECT -1 LIMIT 1)`},
		{"scalar inside CASE", `UPDATE t SET c=CASE WHEN (SELECT sum(b) FROM t t2 WHERE t2.a<t.a)>25 THEN 'big' ELSE c END, b=b*2`},
		{"schema-qualified target", `UPDATE main.t SET b=(SELECT count(*) FROM main.t t2 WHERE t2.b<t.b)+b`},
		{"OR REPLACE", `UPDATE OR REPLACE t SET b=(SELECT max(b) FROM t t2 WHERE t2.a<t.a)`},
		{"one row", `UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<>t.a) WHERE a=3`},
		{"matches nothing", `UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<>t.a) WHERE a=99`},
	}
	for _, tc := range cases {
		differ(t, "live self-read: "+tc.name, append(append([]string{}, seed...),
			tc.sql, `SELECT changes()`, `SELECT a,b,c FROM t ORDER BY a`))
	}

	// Two passes: an index that a one-pass WHERE loop could walk, made
	// irrelevant by a trigger (update.c:733-739).
	differ(t, "live self-read: trigger forces rowid order over an index", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)`,
		`CREATE INDEX tc ON t(c)`,
		`CREATE TABLE log(a, b)`,
		`INSERT INTO t VALUES(1,10,'d'),(2,20,'c'),(3,30,'b'),(4,40,'a')`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.a, new.b); END`,
		`UPDATE t SET b=(SELECT total(b) FROM t t2 WHERE t2.a<t.a)+1 WHERE c>'a'`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT * FROM log ORDER BY rowid`,
	})
	// ...and by a WHERE subquery.
	differ(t, "live self-read: WHERE subquery forces rowid order over an index", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)`,
		`CREATE INDEX tc ON t(c)`,
		`INSERT INTO t VALUES(1,10,'d'),(2,20,'c'),(3,30,'b'),(4,40,'a')`,
		`UPDATE t SET b=(SELECT total(b) FROM t t2 WHERE t2.a<t.a)+1 WHERE c IN (SELECT c FROM t WHERE c>'a')`,
		`SELECT a,b,c FROM t ORDER BY a`,
	})
	// A trigger that rewrites a LATER row: the live read sees it.
	differ(t, "live self-read: trigger writes ahead of the scan", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t WHEN new.a=1 BEGIN UPDATE t SET b=b+100 WHERE a=3; END`,
		`UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<>t.a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "live self-read: running count over x, twice", []string{
		`CREATE TABLE t(x)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`UPDATE t SET x=x+(SELECT count(*) FROM t t2 WHERE t2.x<=t.x)`,
		`SELECT group_concat(x) FROM (SELECT x FROM t ORDER BY rowid)`,
		`UPDATE t SET x=x+(SELECT count(*) FROM t t2 WHERE t2.x<=t.x)`,
		`SELECT group_concat(x) FROM (SELECT x FROM t ORDER BY rowid)`,
	})
	differ(t, "live self-read: equal values, counted as they change", []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO t VALUES(1,30),(3,30)`,
		`UPDATE t SET v=v+(SELECT count(*) FROM t t2 WHERE t2.v<=t.v)`,
		`SELECT id,v FROM t ORDER BY id`,
	})
	differ(t, "live self-read: temp target", []string{
		`CREATE TEMP TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<=t.a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "live self-read: declared collation and affinity through the correlation", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, n INTEGER, v TEXT COLLATE NOCASE)`,
		`INSERT INTO t VALUES(1,5,'ABC'),(2,6,'abc'),(3,7,'Abd')`,
		`UPDATE t SET n=(SELECT count(*) FROM t t2 WHERE t2.v=t.v AND t2.n<=t.n)+n`,
		`SELECT a,n,v FROM t ORDER BY a`,
	})
}

// TestUpdateSetLiveSelfReadWith is the same per-row read under a leading WITH,
// including a CTE whose own body reads the target table.
func TestUpdateSetLiveSelfReadWith(t *testing.T) {
	seed := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`CREATE TABLE s(k, w)`,
		`INSERT INTO s VALUES(1,100),(2,200),(3,300)`,
	}
	for _, q := range []string{
		`WITH c AS (SELECT k, w FROM s) UPDATE t SET b=b+(SELECT count(*) FROM t t2 WHERE t2.b<=t.b)+(SELECT w FROM c WHERE k=t.a)`,
		`WITH c AS (SELECT a, b FROM t) UPDATE t SET b=(SELECT sum(b) FROM c WHERE c.a<=t.a)`,
		`WITH c AS (SELECT max(b) AS m FROM t) UPDATE t SET b=(SELECT m FROM c)+(SELECT count(*) FROM t t2 WHERE t2.b>t.b)`,
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n<3) UPDATE t SET b=(SELECT sum(t2.b) FROM t t2, r WHERE t2.a=r.n AND r.n<t.a)`,
	} {
		differ(t, "live self-read with WITH: "+q, append(append([]string{}, seed...), q, `SELECT a, b FROM t ORDER BY a`))
	}
	// The target reached through a VIEW, which a name match cannot see either.
	differ(t, "live self-read through a view", append(append([]string{}, seed...),
		`CREATE VIEW v AS SELECT a, b FROM t`,
		`UPDATE t SET b=(SELECT sum(b) FROM v WHERE v.a<=t.a)`,
		`SELECT a, b FROM t ORDER BY a`,
		`CREATE VIEW vv AS SELECT a*1 AS a, b FROM v WHERE b IS NOT NULL`,
		`UPDATE t SET b=b+(SELECT count(*) FROM vv WHERE vv.b<t.b)`,
		`SELECT a, b FROM t ORDER BY a`))
}
