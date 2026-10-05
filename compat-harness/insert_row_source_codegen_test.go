package compat

import "testing"

// TestInsertRowSourceCodegen verifies compiled INSERT...SELECT programs match
// C SQLite results for various row sources: virtual tables, views, triggered
// tables, and upserts.
func TestInsertRowSourceCodegen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		// Virtual table row sources
		{"fts4-default-values", []string{
			`CREATE VIRTUAL TABLE f USING fts4(x,y)`,
			`INSERT INTO f DEFAULT VALUES`, `INSERT INTO f DEFAULT VALUES`,
			`SELECT docid, x IS NULL, y IS NULL FROM f ORDER BY docid`}},
		{"rtree-default-values", []string{
			`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r DEFAULT VALUES`, `INSERT INTO r DEFAULT VALUES`,
			`SELECT rowid,id,x0,x1 FROM r ORDER BY rowid`}},
		// Column list with DEFAULT VALUES is an arity error
		{"default-values-with-a-column-list-is-an-error", []string{
			`CREATE TABLE t(a,b)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
			`INSERT INTO t(a) DEFAULT VALUES`, `INSERT INTO f(x) DEFAULT VALUES`,
			`SELECT count(*) FROM t`, `SELECT count(*) FROM f`}},
		{"fts4-select-source", []string{
			`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`,
			`INSERT INTO s VALUES('alpha beta'),('gamma'),('delta')`,
			`INSERT INTO f SELECT x FROM s`,
			`SELECT docid,x FROM f ORDER BY docid`,
			`SELECT docid FROM f WHERE f MATCH 'beta'`, `SELECT changes()`}},
		{"fts4-select-source-column-list", []string{
			`CREATE VIRTUAL TABLE f USING fts4(x,y)`, `CREATE TABLE s(a,b)`,
			`INSERT INTO s VALUES('A','B'),('C','D')`,
			`INSERT INTO f(y,x) SELECT a,b FROM s`, `SELECT x,y FROM f ORDER BY docid`}},
		{"fts3-select-source-reads-the-target", []string{
			`CREATE VIRTUAL TABLE h USING fts3(w)`, `INSERT INTO h VALUES('a'),('b')`,
			`INSERT INTO h SELECT w FROM h`, `SELECT docid,w FROM h ORDER BY docid`}},
		{"rtree-select-source", []string{
			`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE s(id,x0,x1)`,
			`INSERT INTO s VALUES(1,0.0,1.0),(2,2.0,3.0)`,
			`INSERT INTO r SELECT id,x0,x1 FROM s`, `SELECT id,x0,x1 FROM r ORDER BY id`}},
		{"fts4-select-source-empty", []string{
			`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`,
			`INSERT INTO f SELECT x FROM s`, `SELECT count(*) FROM f`, `SELECT changes()`}},
		// Arity errors on SELECT sources including empty ones
		{"fts4-select-source-wrong-arity", []string{
			`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(a)`,
			`INSERT INTO s VALUES('q')`,
			`INSERT INTO f SELECT a,a FROM s`, `SELECT count(*) FROM f`}},
		{"fts4-select-source-wrong-arity-empty-source", []string{
			`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(a)`,
			`INSERT INTO f SELECT a,a FROM s`, `SELECT count(*) FROM f`}},
		// Command channel from SELECT source doesn't store rows
		{"fts4-command-channel-from-a-select-source", []string{
			`CREATE VIRTUAL TABLE g USING fts4(z)`,
			`INSERT INTO g VALUES('hello world'),('goodbye world')`,
			`CREATE TABLE cmd(c)`, `INSERT INTO cmd VALUES('optimize')`,
			`INSERT INTO g(g) SELECT c FROM cmd`, `SELECT docid,z FROM g ORDER BY docid`}},

		// View row sources
		{"view-select-source-fires-per-row", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO s VALUES(1,2),(3,4),(5,6)`,
			`INSERT INTO v SELECT a,c FROM s`, `SELECT a,c FROM b ORDER BY rowid`,
			`SELECT changes()`}},
		{"view-select-source-named-partial", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE s(z)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO s VALUES(7),(8)`,
			`INSERT INTO v(c) SELECT z FROM s`, `SELECT a IS NULL, c FROM b ORDER BY rowid`}},
		{"view-select-source-reads-its-own-base-table", []string{
			`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`INSERT INTO b VALUES(1),(2)`,
			`INSERT INTO v SELECT a FROM b`, `SELECT a FROM b ORDER BY rowid`}},
		{"view-select-source-raise-ignore", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE TABLE s(a)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1),(2),(3)`,
			`INSERT INTO v SELECT a FROM s`, `SELECT x FROM log ORDER BY rowid`}},
		// Program cache doesn't stale when source table changes
		{"view-select-source-not-cached-across-a-changed-source", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE s(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`INSERT INTO s VALUES(1)`, `INSERT INTO v SELECT a FROM s`,
			`INSERT INTO s VALUES(2)`, `INSERT INTO v SELECT a FROM s`,
			`SELECT a FROM b ORDER BY rowid`}},

		// Triggered table row sources
		{"triggered-table-after-fires-per-source-row", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1,'one'),(2,'two'),(3,'three')`,
			`INSERT INTO t SELECT a,b FROM s`, `SELECT a,b FROM t ORDER BY rowid`,
			`SELECT x FROM log ORDER BY rowid`, `SELECT changes()`}},
		{"triggered-table-new-rowid-before-vs-after", []string{
			`CREATE TABLE w(a)`, `CREATE TABLE ws(a)`, `CREATE TABLE wlog(t,r)`,
			`CREATE TRIGGER wb BEFORE INSERT ON w BEGIN INSERT INTO wlog VALUES('B',new.rowid); END`,
			`CREATE TRIGGER wa AFTER INSERT ON w BEGIN INSERT INTO wlog VALUES('A',new.rowid); END`,
			`INSERT INTO ws VALUES('p'),('q')`,
			`INSERT INTO w SELECT a FROM ws`, `SELECT t,r FROM wlog ORDER BY rowid`}},
		{"triggered-table-ipk-new-rowid-before-vs-after", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE s(b)`, `CREATE TABLE log(x,y)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a); END`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`,
			`INSERT INTO s VALUES('one'),('two')`,
			`INSERT INTO t(b) SELECT b FROM s`, `SELECT x,y FROM log ORDER BY rowid`,
			`SELECT a,b FROM t ORDER BY a`}},
		{"triggered-table-raise-ignore-skips-one-source-row", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1,'one'),(2,'two'),(3,'three')`,
			`INSERT INTO t SELECT a,b FROM s`, `SELECT a FROM t ORDER BY rowid`,
			`SELECT x FROM log ORDER BY rowid`, `SELECT changes()`}},
		{"triggered-table-body-read-is-live", []string{
			`CREATE TABLE u(a)`, `CREATE TABLE us(a)`, `CREATE TABLE ulog(n)`,
			`CREATE TRIGGER ua AFTER INSERT ON u BEGIN INSERT INTO ulog SELECT count(*) FROM u; END`,
			`INSERT INTO us VALUES(10),(20),(30)`,
			`INSERT INTO u SELECT a FROM us`, `SELECT n FROM ulog ORDER BY rowid`}},
		{"triggered-table-self-sourced", []string{
			`CREATE TABLE v(a)`, `CREATE TABLE vlog(x)`,
			`CREATE TRIGGER va AFTER INSERT ON v BEGIN INSERT INTO vlog VALUES(new.a); END`,
			`INSERT INTO v VALUES(1),(2)`,
			`INSERT INTO v SELECT a FROM v`, `SELECT a FROM v ORDER BY rowid`,
			`SELECT x FROM vlog ORDER BY rowid`}},
		{"triggered-table-nested-trigger", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ua AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a*10); END`,
			`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO s VALUES(1),(2)`,
			`INSERT INTO t SELECT a FROM s`, `SELECT x FROM log ORDER BY rowid`}},
		// Trigger disables xfer optimization, affecting arity check
		{"triggered-table-disables-the-xfer-optimization", []string{
			`CREATE TABLE src(a, g AS (a+1))`, `CREATE TABLE dst(a, g AS (a+1))`,
			`CREATE TABLE dst2(a, g AS (a+1))`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tr AFTER INSERT ON dst2 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO src VALUES(1),(2)`,
			`INSERT INTO dst SELECT * FROM src`, `SELECT a,g FROM dst ORDER BY rowid`,
			`INSERT INTO dst2 SELECT * FROM src`, `SELECT count(*) FROM dst2`,
			`SELECT count(*) FROM log`}},

		// Upsert with SELECT sources
		{"upsert-select-conflicts-with-this-statements-own-row", []string{
			`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`,
			`INSERT INTO s VALUES(1,'first'),(1,'second'),(2,'other')`,
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=coalesce(c,0)+1`,
			`SELECT a,b,c FROM t ORDER BY a`, `SELECT changes()`}},
		{"upsert-select-do-nothing", []string{
			`CREATE TABLE u(a UNIQUE,b)`, `CREATE TABLE us(a,b)`,
			`INSERT INTO u VALUES(1,'old')`, `INSERT INTO us VALUES(1,'new'),(2,'two')`,
			`INSERT INTO u(a,b) SELECT a,b FROM us WHERE true ON CONFLICT(a) DO NOTHING`,
			`SELECT a,b FROM u ORDER BY a`, `SELECT changes()`}},
		{"upsert-select-do-update-where-filters-per-row", []string{
			`CREATE TABLE v(a UNIQUE,b,c)`, `CREATE TABLE vs(a,b)`,
			`INSERT INTO v VALUES(1,'x',1),(2,'y',2)`, `INSERT INTO vs VALUES(1,'p'),(2,'q')`,
			`INSERT INTO v(a,b) SELECT a,b FROM vs WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE v.c=1`,
			`SELECT a,b,c FROM v ORDER BY a`, `SELECT changes()`}},
		{"upsert-select-excluded-is-the-current-row", []string{
			`CREATE TABLE w(a UNIQUE,b)`, `CREATE TABLE ws(a,b)`,
			`INSERT INTO w VALUES(1,'old'),(2,'old2')`, `INSERT INTO ws VALUES(1,'new'),(2,'new2')`,
			`INSERT INTO w(a,b) SELECT a,b FROM ws WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b||'!'`,
			`SELECT a,b FROM w ORDER BY a`}},

		// Trigger body with virtual table source
		{"body-source-select-over-an-fts4-table", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
			`INSERT INTO f VALUES('alpha'),('beta')`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO log SELECT x FROM f; END`,
			`INSERT INTO t VALUES(1,2)`, `SELECT x FROM log ORDER BY rowid`}},
		// The LIVE half, which is what makes the narrowing a promotion rather
		// than a coincidence: the body WRITES the vtab and then reads it, so a
		// frozen image would answer 1,1 (and 0,0,0 below).
		{"body-writes-then-reads-an-fts4-table", []string{
			`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO f VALUES(new.a); INSERT INTO log SELECT count(*) FROM f; END`,
			`INSERT INTO t VALUES('one'),('two')`, `SELECT n FROM log ORDER BY rowid`}},
		{"body-writes-then-reads-an-rtree", []string{
			`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO r VALUES(new.a,0.0,1.0); INSERT INTO log VALUES((SELECT count(*) FROM r)); END`,
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log ORDER BY rowid`}},
		{"when-guard-subquery-over-an-rtree", []string{
			`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM r)>0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`, `INSERT INTO r VALUES(1,0.0,1.0)`, `INSERT INTO t VALUES(2)`,
			`SELECT n FROM log ORDER BY rowid`}},
	} {
		t.Run(tc.name, func(t *testing.T) { differ(t, tc.name, tc.stmts) })
	}
}
