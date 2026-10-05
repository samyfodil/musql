// Tests the separation of TEMP and main table catalogs.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// driverParity drives BOTH drivers -- the pure engine and C SQLite --
// through steps and compares every answer.
func driverParity(t *testing.T, name string, steps []string) {
	t.Helper()
	got := make([]string, len(steps))
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, err := sql.Open(drv, dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for i, q := range steps {
			out := queryString(t, db, q)
			if drv == "sqlite" {
				got[i] = out
				continue
			}
			if got[i] != out {
				t.Errorf("[%s] #%d %s\n  engine: %s\n  cgo:    %s", name, i, q, got[i], out)
			}
		}
		db.Close()
	}
}

func TestTempSchemaParity(t *testing.T) {
	// tkt2817.test itself: a temp tbl and a main tbl coexist, "main." picks
	// main's, an unqualified name picks temp's, and DROP TABLE takes the temp
	// one and leaves main's standing.
	driverParity(t, "coexist", []string{
		`CREATE TABLE base(x)`, `CREATE TEMP TABLE tbl(a,b,c)`, `INSERT INTO tbl VALUES(1,2,3)`,
		`CREATE TABLE main.tbl(a,b,c)`, `INSERT INTO main.tbl VALUES(9,9,9)`,
		`SELECT * FROM tbl`, `SELECT * FROM main.tbl`, `SELECT * FROM temp.tbl`,
		`CREATE INDEX main.tbli ON tbl(a,b,c)`,
		`INSERT INTO main.tbl SELECT a,b,c FROM temp.tbl`,
		`SELECT * FROM main.tbl`,
		`DROP TABLE tbl`, `SELECT * FROM tbl`, `SELECT * FROM temp.tbl`,
	})

	// view.test segment 7: a TEMP VIEW shadows a main table of the same name,
	// "DROP VIEW IF EXISTS temp.t1" targets the temp one, and a later bare
	// "DROP VIEW t1" finds it (and then reports "no such view" once it's gone,
	// rather than dropping main's table).
	driverParity(t, "view-shadow", []string{
		`CREATE TABLE t1(a,b,c)`, `INSERT INTO t1 VALUES(1,2,3)`,
		`CREATE TEMP VIEW t1 AS SELECT a,b FROM t1`, `SELECT * FROM temp.t1`,
		`DROP VIEW IF EXISTS temp.t1`, `CREATE TEMP VIEW t1(a,b) AS SELECT a,b FROM t1`,
		`SELECT * FROM temp.t1`, `DROP VIEW t1`, `DROP VIEW t1`, `SELECT * FROM t1`,
	})

	// Which catalog a new object lands in: a TEMP keyword, a "temp." qualifier
	// (which the keyword may agree with but not contradict), or -- for an index
	// or trigger -- the catalog of the table it targets.
	driverParity(t, "create-scope", []string{
		`CREATE TABLE m(a)`,
		`CREATE TEMP TABLE main.q(a)`, // "temporary table name must be unqualified"
		`CREATE TEMP TABLE temp.q(a)`, `CREATE TABLE temp.q2(a)`,
		`CREATE TEMP VIEW main.vv AS SELECT 1`,
		`CREATE INDEX temp.qi ON q(a)`, `CREATE INDEX qi2 ON q(a)`,
		`CREATE INDEX temp.mi ON m(a)`, // "cannot create a TEMP index on non-TEMP table"
		`CREATE TRIGGER trm AFTER INSERT ON q BEGIN SELECT 1; END`,
		`DROP TABLE main.q`, `DROP TABLE temp.m`, `DROP TABLE temp.q`, `DROP TABLE m`,
	})

	// Cross-catalog reach: a MAIN view or trigger may not reference a temp
	// object, while a TEMP one may reference either.
	driverParity(t, "cross-catalog", []string{
		`CREATE TABLE m(a)`, `INSERT INTO m VALUES(1)`, `CREATE TEMP TABLE tt(b)`,
		`INSERT INTO tt VALUES(2)`,
		`CREATE VIEW mv AS SELECT * FROM temp.tt`,
		`CREATE TEMP VIEW tv AS SELECT * FROM main.m`,
		`CREATE TEMP VIEW tv2 AS SELECT * FROM temp.tt`,
		`CREATE TRIGGER mtr AFTER INSERT ON m BEGIN SELECT * FROM temp.tt; END`,
		`CREATE TEMP TRIGGER ttr AFTER INSERT ON m BEGIN INSERT INTO tt VALUES(9); END`,
		`INSERT INTO m VALUES(3)`, `SELECT * FROM tt`, `SELECT * FROM tv`, `SELECT * FROM tv2`,
		`SELECT * FROM main.tt`, `SELECT * FROM temp.m`,
	})

	// A MAIN trigger reaching a temp table fails when the triggering statement
	// runs (not at CREATE TRIGGER) -- trigger1.test's own shape.
	driverParity(t, "main-trigger-temp-table", []string{
		`CREATE TABLE t1(a)`, `CREATE TEMP TABLE t2(x,y)`,
		`CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN INSERT INTO t2 VALUES(1,2); END`,
		`INSERT INTO t1 VALUES(1)`, `SELECT count(*) FROM t2`,
	})

	// A trigger fires for its OWN table's catalog only. "CREATE TEMP TRIGGER
	// trig1 ... ON main.t4" is a temp trigger on a MAIN table, so an insert
	// into temp.t4 must not fire it (trigger1.test 10.x); and a MAIN trigger's
	// unqualified ON clause binds to the MAIN table even where a temp one of
	// the same name shadows it (triggerD.test 3.x -- the ticket that file
	// exists for).
	driverParity(t, "trigger-catalog", []string{
		`CREATE TABLE main.t4(a,b,c)`, `CREATE TEMP TABLE temp.t4(a,b,c)`,
		`CREATE TABLE insert_log(db,a,b,c)`,
		`CREATE TEMP TRIGGER trig1 AFTER INSERT ON main.t4 BEGIN INSERT INTO insert_log VALUES('main', new.a, new.b, new.c); END`,
		`CREATE TEMP TRIGGER trig2 AFTER INSERT ON temp.t4 BEGIN INSERT INTO insert_log VALUES('temp', new.a, new.b, new.c); END`,
		`INSERT INTO main.t4 VALUES(1,2,3)`, `INSERT INTO temp.t4 VALUES(4,5,6)`,
		`SELECT * FROM insert_log`,
	})
	driverParity(t, "trigger-unqualified-binds-main", []string{
		`CREATE TABLE t300(x)`, `CREATE TABLE log300(y)`, `CREATE TEMP TABLE t300(x)`,
		`CREATE TRIGGER r301 AFTER INSERT ON t300 BEGIN INSERT INTO log300 VALUES(10000+new.x); END`,
		`INSERT INTO temp.t300 VALUES(4)`, `SELECT * FROM log300`,
		`INSERT INTO main.t300 VALUES(3)`, `SELECT * FROM log300`,
	})

	// Object-scoped PRAGMAs honour the qualifier; a temp-only table answers
	// zero rows under "main.".
	driverParity(t, "pragma-scope", []string{
		`CREATE TABLE m(a)`, `CREATE TEMP TABLE tt(b)`, `CREATE INDEX ti ON tt(b)`,
		`PRAGMA table_info(tt)`, `PRAGMA main.table_info(tt)`, `PRAGMA temp.table_info(tt)`,
		`PRAGMA index_list(tt)`, `PRAGMA main.table_info(m)`, `PRAGMA temp.table_info(m)`,
	})

	// A three-part "schema.table.column" reference names the catalog its FROM
	// item lives in -- including through an alias, which C SQLite also
	// accepts.
	driverParity(t, "three-part-column", []string{
		`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`,
		`SELECT main.t.a FROM t`, `SELECT main.al.a FROM t AS al`, `SELECT temp.t.a FROM t`,
		`CREATE TEMP TABLE t(b)`, `INSERT INTO t VALUES(2)`,
		`SELECT temp.t.b FROM t`, `SELECT main.t.a FROM main.t`, `SELECT temp.t.a FROM temp.t`,
		`SELECT count(*) FROM t JOIN main.t ON 1`,
	})
}

// TestTempObjectsVanishOnClose pins that a TEMP object lives no longer than the
// connection that created it, exactly like C SQLite -- the engine keeps one
// file, so it discards them at Conn.Close rather than by deleting a private
// temp database (engine.DropTempObjects), but the end state is identical.
func TestTempObjectsVanishOnClose(t *testing.T) {
	for _, drv := range []string{"sqlite", "sqlite3"} {
		path := filepath.Join(t.TempDir(), "lifetime.sqlite")
		db, err := sql.Open(drv, path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			`CREATE TABLE m(a)`, `INSERT INTO m VALUES(1)`,
			`CREATE TEMP TABLE tt(b)`, `INSERT INTO tt VALUES(2)`,
			`CREATE INDEX ti ON tt(b)`, `CREATE TEMP VIEW tv AS SELECT * FROM m`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", drv, q, err)
			}
		}
		if got := queryString(t, db, `SELECT * FROM tt`); got != "cols=[b] [2]" {
			t.Errorf("%s: in-session SELECT * FROM tt = %s", drv, got)
		}
		db.Close()

		db2, err := sql.Open(drv, path)
		if err != nil {
			t.Fatal(err)
		}
		db2.SetMaxOpenConns(1)
		if got := queryString(t, db2, `SELECT * FROM tt`); got != "ERR" {
			t.Errorf("%s: temp table survived the connection: %s", drv, got)
		}
		if got := queryString(t, db2, `SELECT * FROM tv`); got != "ERR" {
			t.Errorf("%s: temp view survived the connection: %s", drv, got)
		}
		if got := queryString(t, db2, `SELECT * FROM m`); got != "cols=[a] [1]" {
			t.Errorf("%s: main table did not survive: %s", drv, got)
		}
		if got := queryString(t, db2, `SELECT type,name FROM sqlite_master`); got != "cols=[type name] [table m]" {
			t.Errorf("%s: sqlite_master after close = %s", drv, got)
		}
		db2.Close()
	}
}

// TestTempSchemaNoLongerDeclines is the test that used to be
// TestTempSchemaDeclines. Every shape in it was refused for one reason: both
// catalogs lived in one schema b-tree and one file header. The TEMP database
// has its own of each now (engine/temp_store.go), so all five are ordinary
// statements -- and this asserts they agree with the oracle, which is what the
// declines were standing in for.
func TestTempSchemaNoLongerDeclines(t *testing.T) {
	setup := []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `CREATE TEMP TABLE t(b)`, `CREATE INDEX ti ON t(b)`}
	driverParity(t, "was-declined", append(setup,
		`SELECT * FROM sqlite_master`,
		`SELECT * FROM sqlite_temp_master`,
		`PRAGMA index_list(t)`,
		`PRAGMA temp.user_version`,
		`SELECT * FROM main.t, temp.t`,
	))
}
