// Tests UPDATE ... FROM inside trigger bodies and INSTEAD OF UPDATE triggers
// on views. Only single-match joins are accepted (multi-match is deliberately declined).
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestWpathR24UpdateFromInTriggerBody tests UPDATE ... FROM in trigger bodies.
func TestWpathR24UpdateFromInTriggerBody(t *testing.T) {
	// triggerupfrom.test 1.0-1.3, the canonical shape: an AFTER INSERT trigger
	// whose body joins the inserted row against a lookup table.
	flLockstep(t, "after-insert-body", []string{
		`CREATE TABLE map(k, v)`,
		`INSERT INTO map VALUES(1, 'one'), (2, 'two'), (3, 'three'), (4, 'four')`,
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN
		   UPDATE t1 SET c = v FROM map WHERE k=new.a AND a=new.a;
		 END`,
		`INSERT INTO t1(a) VALUES(1)`,
		`INSERT INTO t1(a) VALUES(2), (3), (4), (5)`,
	}, `SELECT a, c FROM t1 ORDER BY a`)

	// in7.test 4.0: the FROM table is EMPTY, so the body's UPDATE ... FROM
	// matches nothing -- and the outer UPDATE's own write must still land.
	flLockstep(t, "empty-from-in-body", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,x'1111')`,
		`CREATE TABLE t2(c)`,
		`CREATE TRIGGER t1tr UPDATE ON t1 BEGIN
		   UPDATE t1 SET b=x'2222' FROM t2;
		 END`,
		`UPDATE t1 SET b=x'3333'`,
	}, `SELECT quote(b) FROM t1`)

	// A BEFORE DELETE body's UPDATE ... FROM, reading OLD (triggerupfrom 2.3).
	flLockstep(t, "before-delete-body-old", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)`,
		`INSERT INTO t1 VALUES(1,'p','x'),(2,'q','y'),(3,NULL,'z')`,
		`CREATE TABLE link(f, t)`,
		`INSERT INTO link VALUES(2, 1), (3, 2)`,
		`CREATE TRIGGER tr3 BEFORE DELETE ON t1 BEGIN
		   UPDATE t1 SET b=coalesce(old.b,old.c) FROM main.link WHERE a=t AND old.a=f;
		 END`,
		`DELETE FROM t1 WHERE a=2`,
	}, `SELECT * FROM t1 ORDER BY a`)

	// A leading WITH in front of a trigger body's UPDATE ... FROM: the CTE has
	// to reach the synthetic join, not just the statement's own subqueries.
	flLockstep(t, "cte-in-body", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'p'),(2,'q')`,
		`CREATE TABLE fire(n)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN
		   WITH data(k,v) AS (VALUES(1,'ten'),(2,'twenty'))
		   UPDATE t1 SET b=v FROM data WHERE a=k;
		 END`,
		`INSERT INTO fire VALUES(1)`,
	}, `SELECT * FROM t1 ORDER BY a`)

	// The body's UPDATE ... FROM names a column that exists nowhere: real
	// SQLite rejects it, and so must this engine -- lifting the eager
	// trigger-body validation must not turn a genuine error into silence.
	flLockstep(t, "bad-column-in-body", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'p')`,
		`CREATE TABLE map(k, v)`,
		`INSERT INTO map VALUES(1,'x')`,
		`CREATE TABLE fire(n)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN
		   UPDATE t1 SET b=nosuchcol FROM map WHERE a=k;
		 END`,
		`INSERT INTO fire VALUES(1)`,
	}, `SELECT * FROM t1 ORDER BY a`, `SELECT * FROM fire`)
}

// TestWpathR24UpdateFromTriggersAndCTEs pins the top-level FROM-form against a
// target that carries UPDATE triggers (previously declined outright) and the
// CTE-scoping rules the synthetic join has to follow.
func TestWpathR24UpdateFromTriggersAndCTEs(t *testing.T) {
	// A target that HAS UPDATE triggers: they fire once per updated row, in
	// ascending target-rowid order, NOT the join's order. m's rows are
	// deliberately in the reverse order so the two are distinguishable, and the
	// log is read back BY ROWID so the firing order itself is compared.
	flLockstep(t, "target-with-update-triggers", []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY, z)`,
		`INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c')`,
		`CREATE TABLE log(t)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t1 BEGIN INSERT INTO log VALUES('B'||old.x||old.z||'->'||new.z); END`,
		`CREATE TRIGGER ta AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('A'||old.x||old.z||'->'||new.z); END`,
		`CREATE TABLE m(k,v)`,
		`INSERT INTO m VALUES(3,'C'),(2,'B'),(1,'A')`,
		`UPDATE t1 SET z=m.v FROM m WHERE t1.x=m.k`,
	}, `SELECT rowid, t FROM log ORDER BY rowid`, `SELECT * FROM t1 ORDER BY x`)

	// The same, with an "UPDATE OF <col>" trigger that must NOT fire (the SET
	// list does not overlap its column list) and one that must.
	flLockstep(t, "target-update-of-column", []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY, y, z)`,
		`INSERT INTO t1 VALUES(1,'a','p'),(2,'b','q')`,
		`CREATE TABLE log(t)`,
		`CREATE TRIGGER ty AFTER UPDATE OF y ON t1 BEGIN INSERT INTO log VALUES('y'||old.x); END`,
		`CREATE TRIGGER tz AFTER UPDATE OF z ON t1 BEGIN INSERT INTO log VALUES('z'||old.x); END`,
		`CREATE TABLE m(k,v)`,
		`INSERT INTO m VALUES(1,'A'),(2,'B')`,
		`UPDATE t1 SET z=m.v FROM m WHERE t1.x=m.k`,
	}, `SELECT rowid, t FROM log ORDER BY rowid`, `SELECT * FROM t1 ORDER BY x`)

	// A BEFORE UPDATE trigger that RAISE(IGNORE)s one row: that row must keep
	// its OLD value while every other row still updates -- the "a halted
	// statement still publishes" shape, checked through a SINGLE Exec.
	flLockstep(t, "before-update-raise-ignore", []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY, z)`,
		`INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c')`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t1 WHEN old.x=2 BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TABLE m(k,v)`,
		`INSERT INTO m VALUES(1,'A'),(2,'B'),(3,'C')`,
		`UPDATE t1 SET z=m.v FROM m WHERE t1.x=m.k`,
	}, `SELECT * FROM t1 ORDER BY x`)

	// A CTE that SHADOWS the target table: C SQLite binds the UPDATE target
	// to the real TABLE, never to the CTE (upfrom1.test 4.1/4.2).
	flLockstep(t, "cte-shadowing-target", []string{
		`CREATE TABLE t1(x INT)`,
		`INSERT INTO t1 VALUES(1)`,
		`CREATE TABLE t2(y INT)`,
		`INSERT INTO t2 VALUES(2)`,
		`WITH t1 AS (SELECT y+100 AS x FROM t2) UPDATE t1 SET x=(SELECT x FROM t1)`,
		`WITH t1 AS (SELECT y+100 AS x FROM t2) UPDATE t1 SET x=x+y FROM t2`,
	}, `SELECT x, y FROM t1, t2`)

	// A CTE used as a FROM SOURCE at top level, alongside one that shadows.
	flLockstep(t, "cte-as-from-source", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'p'),(2,'q'),(3,'r')`,
		`WITH data(k,v) AS (VALUES(1,'ten'),(3,'thirty')) UPDATE t1 SET b=v FROM data WHERE a=k`,
	}, `SELECT * FROM t1 ORDER BY a`)

}

// TestWpathR24UpdateFromAgainstView pins the view form, including the two
// shapes that must stay REJECTED (no trigger at all, and a multi-match join).
func TestWpathR24UpdateFromAgainstView(t *testing.T) {
	// A view with no INSTEAD OF UPDATE trigger: still "cannot modify".
	flLockstep(t, "view-no-trigger", []string{
		`CREATE TABLE t1(x, y)`,
		`INSERT INTO t1 VALUES(1,'a'),(2,'b')`,
		`CREATE VIEW v1 AS SELECT * FROM t1`,
		`CREATE TABLE m(k, v)`,
		`INSERT INTO m VALUES(1,'X')`,
		`UPDATE v1 SET y=m.v FROM m WHERE v1.x=m.k`,
	}, `SELECT * FROM t1 ORDER BY x`)

	// triggerupfrom.test 4.3: the INSTEAD OF body only logs, so this pins the
	// OLD/NEW image the join hands the trigger, per row.
	flLockstep(t, "view-instead-of-log", []string{
		`CREATE TABLE t1(k, a, b)`,
		`INSERT INTO t1 VALUES('a', 1, 'one'), ('b', 2, 'two'), ('c', 3, 'three'), ('d', 4, 'four')`,
		`CREATE TABLE log(x)`,
		`CREATE VIEW v1 AS SELECT k, a, b FROM t1`,
		`CREATE TRIGGER tr1 INSTEAD OF UPDATE ON v1 BEGIN
		   INSERT INTO log VALUES('('||old.a||','||old.b||')->('||new.a||','||new.b||')');
		 END`,
		`CREATE TABLE map(k, v)`,
		`INSERT INTO map VALUES('b', 'twelve')`,
		`INSERT INTO map VALUES('d', 'fourteen')`,
		`UPDATE v1 SET a=map.v FROM map WHERE v1.k=map.k`,
	}, `SELECT * FROM log`, `SELECT * FROM t1 ORDER BY k`, `SELECT changes()`)

	// upfrom2.test 3.1: a leading WITH feeding the join, and a body that writes
	// through to the base table.
	flLockstep(t, "view-instead-of-cte", []string{
		`CREATE TABLE data(x, y, z)`,
		`CREATE TABLE log(t TEXT)`,
		`CREATE VIEW t1 AS SELECT * FROM data`,
		`CREATE TRIGGER t1_insert INSTEAD OF INSERT ON t1 BEGIN
		   INSERT INTO data VALUES(new.x, new.y, new.z);
		 END`,
		`CREATE TRIGGER t1_update INSTEAD OF UPDATE ON t1 BEGIN
		   INSERT INTO log VALUES(old.z || '->' || new.z);
		 END`,
		`INSERT INTO t1 VALUES(1, 'i', 'one')`,
		`INSERT INTO t1 VALUES(2, 'ii', 'two')`,
		`INSERT INTO t1 VALUES(3, 'iii', 'three')`,
		`WITH input(k, v) AS (VALUES(3, 'thirty'), (1, 'ten'))
		 UPDATE t1 SET z=v FROM input WHERE x=k`,
	}, `SELECT * FROM log`, `SELECT * FROM data ORDER BY x`)

	// upfrom2.test 7.1: the view's SELECT carries a window function, so it can
	// only be answered by materializing it.
	flLockstep(t, "view-window-source", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1(a) VALUES(11),(22),(33),(44),(55)`,
		`CREATE VIEW t2(b,c) AS SELECT a, COUNT(*) OVER () FROM t1`,
		`CREATE TABLE t3(x,y)`,
		`CREATE TRIGGER t2r1 INSTEAD OF UPDATE ON t2 BEGIN
		   INSERT INTO t3(x,y) VALUES(new.b,new.c);
		 END`,
		`UPDATE t2 SET c=t1.a FROM t1 WHERE t2.b=t1.a`,
	}, `SELECT * FROM t3 ORDER BY x`)

	// The view row matched by TWO join rows: the trigger fires twice in real
	// SQLite, in an order the query plan picks, so this engine declines. Both
	// halves of the lockstep must therefore DISAGREE here -- which flLockstep
	// would fail on -- so it is asserted directly instead, from both sides.
	wpathR24ViewMultiMatch(t)
}

// wpathR24ViewMultiMatch pins the one shape execInsteadOfUpdateFrom refuses,
// from both sides: musql must REJECT it (never guess a firing order), and real
// SQLite must still fire once per join row with the PRE-STATEMENT OLD image --
// the property that would make the decline closable if it ever became
// plan-independent.
func wpathR24ViewMultiMatch(t *testing.T) {
	t.Helper()
	script := []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY, z)`,
		`INSERT INTO t1 VALUES(1,'a'),(2,'b')`,
		`CREATE TABLE log(t)`,
		`CREATE VIEW v1 AS SELECT * FROM t1`,
		`CREATE TRIGGER v1tr INSTEAD OF UPDATE ON v1 BEGIN
		   INSERT INTO log VALUES(old.x||':'||old.z||'->'||new.z);
		   UPDATE t1 SET z=new.z WHERE x=new.x;
		 END`,
		`CREATE TABLE m(k,v)`,
		`INSERT INTO m VALUES(1,'p'),(1,'q'),(2,'r'),(2,'s')`,
	}
	upd := `UPDATE v1 SET z=m.v FROM m WHERE v1.x=m.k`

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
	for i, s := range script {
		if eerr := edb.Exec(s); eerr != nil {
			t.Fatalf("engine setup #%d %q: %v", i, s, eerr)
		}
		if _, cerr := cdb.Exec(s); cerr != nil {
			t.Fatalf("cgo setup #%d %q: %v", i, s, cerr)
		}
	}
	// musql side: rejected, and NOTHING applied (a decline that half-fires the
	// trigger would be far worse than the decline itself).
	if eerr := edb.Exec(upd); eerr == nil {
		t.Errorf("multi-match UPDATE ... FROM against a view was ACCEPTED; the firing order is query-plan dependent, so it must be declined")
	}
	p, perr := edb.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	defer p.Close()
	if _, rows, qerr := p.QueryArgs(`SELECT count(*) FROM log`, nil); qerr != nil {
		t.Fatalf("engine count: %v", qerr)
	} else if len(rows) != 1 || rows[0][0].I != 0 {
		t.Errorf("declined multi-match still fired the trigger: log holds %v", rows)
	}

	// Oracle side: fires once per join row, both fires on the same view row
	// seeing the SAME (pre-statement) OLD -- 1:a->p and 1:a->q, never 1:p->q.
	if _, cerr := cdb.Exec(upd); cerr != nil {
		t.Fatalf("cgo multi-match UPDATE: %v", cerr)
	}
	var got []string
	crows, cerr := cdb.Query(`SELECT t FROM log ORDER BY rowid`)
	if cerr != nil {
		t.Fatalf("cgo log: %v", cerr)
	}
	for crows.Next() {
		var s string
		if serr := crows.Scan(&s); serr != nil {
			t.Fatalf("cgo scan: %v", serr)
		}
		got = append(got, s)
	}
	if rerr := crows.Err(); rerr != nil {
		t.Fatalf("cgo log rows: %v", rerr)
	}
	crows.Close()
	want := []string{"1:a->p", "1:a->q", "2:b->r", "2:b->s"}
	if len(got) != len(want) {
		t.Fatalf("oracle fired %d times, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("oracle fire #%d = %q, want %q (whole log %v) -- if SQLite has changed to a single fire per view row, this decline has become closable", i, got[i], want[i], got)
		}
	}
}
