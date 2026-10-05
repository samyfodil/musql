package compat

import "testing"

// Differential battery for cursor re-seek semantics in compiled DELETE and
// UPDATE loops with triggers. When a trigger rewrites a row not yet reached by
// the outer scan, the cursor must re-seek to read that row's current content.
func TestNestedTriggerReseek(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		// ---- ROOT 1: OLD.* must be the LIVE row, not the scan's image ----

		// A cascade rewrites row 2 while the DELETE loop is still on row 1.
		// C SQLite re-seeks before populating OLD.*, so row 2's AFTER
		// DELETE trigger logs 'UP'.
		{"cascade-rewrites-a-later-row-delete", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.b); INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b='UP' WHERE a=2; END`,
			`DELETE FROM t`,
		}, []string{`SELECT v,w FROM log ORDER BY v`, `SELECT a,b FROM t ORDER BY a`, `SELECT changes()`}},

		// The same staleness decides CONTROL FLOW: a WHEN guard over OLD.b
		// makes the trigger not fire at all.
		{"cascade-rewrites-a-later-row-when-guard", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v)`,
			`INSERT INTO t VALUES(1,'x'),(2,'y')`,
			`CREATE TRIGGER r AFTER DELETE ON t WHEN old.b='CASCADED' BEGIN INSERT INTO log VALUES(old.a); END`,
			`CREATE TRIGGER r2 AFTER DELETE ON t BEGIN INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ru AFTER INSERT ON u BEGIN UPDATE t SET b='CASCADED' WHERE a=new.x+1; END`,
			`DELETE FROM t`,
		}, []string{`SELECT v FROM log ORDER BY v`, `SELECT a,b FROM t ORDER BY a`}},

		// Rowid table (no INTEGER PRIMARY KEY), cascade through a second table.
		{"cascade-rewrites-a-later-row-rowid-table", []string{
			`CREATE TABLE t1(a,b)`, `CREATE TABLE u(z)`, `CREATE TABLE log(x)`,
			`INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c')`,
			`CREATE TRIGGER tu AFTER INSERT ON u BEGIN UPDATE t1 SET b='CHANGED' WHERE a=new.z; END`,
			`CREATE TRIGGER rd AFTER DELETE ON t1 BEGIN INSERT INTO u VALUES(old.a+2); INSERT INTO log VALUES('d:'||old.a||'/'||old.b); END`,
			`DELETE FROM t1 WHERE a=1 OR a=3`,
		}, []string{`SELECT x FROM log ORDER BY x`, `SELECT a,b FROM t1 ORDER BY a`}},

		// Same, with the cascade written INLINE in the DELETE trigger's own
		// body (a self-update on the table being scanned).
		{"cascade-rewrites-a-later-row-inline", []string{
			`CREATE TABLE t1(a,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c')`,
			`CREATE TRIGGER tu AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('u:'||old.a); END`,
			`CREATE TRIGGER rd AFTER DELETE ON t1 BEGIN UPDATE t1 SET b='CHANGED' WHERE a=old.a+2; INSERT INTO log VALUES('d:'||old.a||'/'||old.b); END`,
			`DELETE FROM t1 WHERE a=1 OR a=3`,
		}, []string{`SELECT x FROM log ORDER BY x`, `SELECT a,b FROM t1 ORDER BY a`}},

		// INTEGER PRIMARY KEY + an IN list, so the scan's candidate set is
		// chosen a different way.
		{"cascade-rewrites-a-later-row-ipk-in-list", []string{
			`CREATE TABLE t1(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(z)`, `CREATE TABLE log(x)`,
			`INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c')`,
			`CREATE TRIGGER tu AFTER INSERT ON u BEGIN UPDATE t1 SET b=b||'*' WHERE a=new.z; END`,
			`CREATE TRIGGER rd AFTER DELETE ON t1 BEGIN INSERT INTO u VALUES(old.a+2); INSERT INTO log VALUES('d:'||old.a||'/'||old.b); END`,
			`DELETE FROM t1 WHERE a IN (1,3)`,
		}, []string{`SELECT x FROM log ORDER BY x`, `SELECT a,b FROM t1 ORDER BY a`}},

		// The row is deleted by the cascade and RE-INSERTED at the same rowid
		// with different content: OLD.* must be the reborn row.
		{"cascade-deletes-and-reinserts-the-same-rowid", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a,old.b); INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN DELETE FROM t WHERE a=2; INSERT INTO t VALUES(2,'REBORN'); END`,
			`DELETE FROM t`,
		}, []string{`SELECT v,w FROM log ORDER BY rowid`, `SELECT a,b FROM t ORDER BY a`}},

		// The UPDATE loop's own re-seek: a cascade rewrites a row the UPDATE
		// scan has not reached, so its SET right-hand side must be evaluated
		// against the REWRITTEN value.
		{"cascade-rewrites-a-later-row-update-loop", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(old.a); INSERT INTO log VALUES(old.a||'/'||old.b||'->'||new.b); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b='UP' WHERE a=3; END`,
			`UPDATE t SET b=b||'!'`,
		}, []string{`SELECT v FROM log ORDER BY rowid`, `SELECT a,b FROM t ORDER BY a`, `SELECT changes()`}},

		// ---- ROOT 2: update.c's two guards ----

		// Row 1's BEFORE program cascades a DELETE of row 2. C SQLite's
		// top-of-loop re-seek (update.c:877) then passes over row 2 entirely;
		// firing its BEFORE trigger anyway cascaded a second DELETE that
		// destroyed row 3.
		{"before-cascade-deletes-a-later-row", []string{
			`CREATE TABLE tt(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE x(v)`, `CREATE TABLE log(n)`,
			`INSERT INTO tt VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER bu BEFORE UPDATE ON tt FOR EACH ROW BEGIN INSERT INTO x VALUES(old.a); END`,
			`CREATE TRIGGER xi AFTER INSERT ON x FOR EACH ROW BEGIN DELETE FROM tt WHERE a=new.v+1; END`,
			`CREATE TRIGGER au AFTER UPDATE ON tt FOR EACH ROW BEGIN INSERT INTO log VALUES(old.a); END`,
			`UPDATE tt SET b=b||'!'`,
		}, []string{`SELECT a,b FROM tt ORDER BY a`, `SELECT v FROM x ORDER BY v`, `SELECT n FROM log ORDER BY n`, `SELECT changes()`}},

		// The BEFORE program deletes the row being updated. The update is
		// abandoned (update.c:994-999) but everything the trigger wrote stays.
		{"before-cascade-deletes-its-own-row", []string{
			`CREATE TABLE tt(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE x(v)`, `CREATE TABLE log(n)`,
			`INSERT INTO tt VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER bu BEFORE UPDATE ON tt FOR EACH ROW BEGIN INSERT INTO x VALUES(old.a); END`,
			`CREATE TRIGGER xi AFTER INSERT ON x FOR EACH ROW BEGIN DELETE FROM tt WHERE a=new.v; END`,
			`CREATE TRIGGER au AFTER UPDATE ON tt FOR EACH ROW BEGIN INSERT INTO log VALUES(old.a); END`,
			`UPDATE tt SET b=b||'!' WHERE a=1`,
		}, []string{`SELECT a,b FROM tt ORDER BY a`, `SELECT v FROM x ORDER BY v`, `SELECT count(*) FROM x`, `SELECT n FROM log ORDER BY n`}},

		// ---- ROOT 3: delete.c's SECOND guard ----

		// The BEFORE DELETE program's cascade MOVES the row's rowid. The row
		// the cursor named is gone, so no delete, no AFTER trigger, no change
		// counted (delete.c:820-825).
		{"before-cascade-moves-the-rowid", []string{
			`CREATE TABLE tt(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE x(v)`, `CREATE TABLE log(n)`,
			`INSERT INTO tt VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER bd BEFORE DELETE ON tt FOR EACH ROW BEGIN INSERT INTO x VALUES(old.a); END`,
			`CREATE TRIGGER xi AFTER INSERT ON x FOR EACH ROW BEGIN UPDATE tt SET a=a+100 WHERE a=new.v; END`,
			`CREATE TRIGGER ad AFTER DELETE ON tt FOR EACH ROW BEGIN INSERT INTO log VALUES(old.a); END`,
			`DELETE FROM tt WHERE a=1`,
		}, []string{`SELECT a,b FROM tt ORDER BY a`, `SELECT n FROM log ORDER BY n`, `SELECT changes()`}},

		{"before-cascade-moves-the-rowid-two-rows", []string{
			`CREATE TABLE tt(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE x(v)`, `CREATE TABLE log(n)`,
			`INSERT INTO tt VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER bd BEFORE DELETE ON tt FOR EACH ROW BEGIN INSERT INTO x VALUES(old.a); END`,
			`CREATE TRIGGER xi AFTER INSERT ON x FOR EACH ROW BEGIN UPDATE tt SET a=a+100 WHERE a>=new.v; END`,
			`CREATE TRIGGER ad AFTER DELETE ON tt FOR EACH ROW BEGIN INSERT INTO log VALUES(old.a); END`,
			`DELETE FROM tt WHERE a<=2`,
		}, []string{`SELECT a,b FROM tt ORDER BY a`, `SELECT n FROM log ORDER BY n`, `SELECT changes()`}},

		// ---- ROOT 4: the rowid reserved before the BEFORE program ----

		// t carries only a BEFORE INSERT trigger; the cascade's write to t is
		// an UPDATE, an event t has no trigger for at all. The rowid this
		// compiler reserves before running the BEFORE program is the hazard --
		// insert.c runs the program first (insert.c:1494-1496) and allocates
		// afterwards (insert.c:1538-1541).
		{"before-insert-cascade-updates-the-target", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`,
			`INSERT INTO t VALUES(5,'five')`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(1); END`,
			`CREATE TRIGGER ua AFTER INSERT ON u BEGIN UPDATE t SET a=6 WHERE a=5; END`,
			`INSERT INTO t(b) VALUES('new')`,
		}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT x FROM u ORDER BY x`}},

		{"before-insert-cascade-deletes-the-target", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`,
			`INSERT INTO t VALUES(5,'five')`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(1); END`,
			`CREATE TRIGGER ua AFTER INSERT ON u BEGIN DELETE FROM t WHERE a=5; END`,
			`INSERT INTO t(b) VALUES('new')`,
		}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT x FROM u ORDER BY x`}},

		// ---- ROOT 5: a statement whose own row count is zero still wrote ----

		// Every outer row is abandoned by the guard above, so the statement
		// counts no rows -- but its trigger bodies wrote three tables, and
		// those writes have to survive the commit.
		{"zero-rows-affected-but-triggers-wrote", []string{
			`CREATE TABLE t1(a,b)`, `CREATE TABLE u(z)`, `CREATE TABLE log(x)`,
			`INSERT INTO t1 VALUES(1,'a'),(2,'b')`,
			`CREATE TRIGGER tu AFTER INSERT ON u BEGIN DELETE FROM t1 WHERE a=new.z; END`,
			`CREATE TRIGGER rb BEFORE UPDATE ON t1 BEGIN INSERT INTO u VALUES(old.a); INSERT INTO log VALUES('b'||old.a); END`,
			`CREATE TRIGGER ra AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('a'||old.a); END`,
			`UPDATE t1 SET b=b||'!'`,
		}, []string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT z FROM u ORDER BY z`, `SELECT x FROM log ORDER BY x`, `SELECT changes()`}},

		// The same root, reached WITHOUT any nesting and on the COMPILED path:
		// a BEFORE trigger that writes a log table and then abandons its own
		// row with RAISE(IGNORE). The statement's row count is 0, every row the
		// trigger wrote is real, and all of it used to be discarded at commit
		// while the statement reported success -- a live defect that predates
		// batch J entirely. These three are what MUTATION-TEST writeCtx.
		// wroteRows(): put its old "rowsAffected > 0" back and all three fail.
		{"raise-ignore-delete-still-commits-the-trigger-write", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1),(2)`,
			`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES(old.a); SELECT RAISE(IGNORE); END`,
			`DELETE FROM t`,
		}, []string{`SELECT a FROM t ORDER BY a`, `SELECT x FROM log ORDER BY x`, `SELECT changes()`}},

		{"raise-ignore-update-still-commits-the-trigger-write", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); SELECT RAISE(IGNORE); END`,
			`UPDATE t SET b=b||'!'`,
		}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT x FROM log ORDER BY x`, `SELECT changes()`}},

		{"raise-ignore-insert-still-commits-the-trigger-write", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); SELECT RAISE(IGNORE); END`,
			`INSERT INTO t VALUES(1),(2)`,
		}, []string{`SELECT a FROM t ORDER BY a`, `SELECT x FROM log ORDER BY x`, `SELECT changes()`}},

		// ---- the re-seek's NEIGHBOURS ----
		//
		// reseekRowStore re-reads through normalizeRow, so every fix-up
		// normalizeRow performs has to survive the second read: the IPK
		// rowid-alias substitution, the REAL-affinity restore, generated
		// columns, and the padding a row written before an ALTER TABLE ADD
		// COLUMN needs. All five below FAIL if the re-seek is reduced to a
		// presence test, so each is a witness and not decoration.
		{"reseek-without-rowid-table", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b) WITHOUT ROWID`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.b); INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b='UP' WHERE a=2; END`,
			`DELETE FROM t`,
		}, []string{`SELECT v,w FROM log ORDER BY v`, `SELECT a,b FROM t ORDER BY a`}},

		{"reseek-without-rowid-text-pk", []string{
			`CREATE TABLE t(k TEXT PRIMARY KEY, b) WITHOUT ROWID`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t VALUES('a','p'),('b','q')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.k, old.b); INSERT INTO u VALUES(old.k); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x='a' BEGIN UPDATE t SET b='UP' WHERE k='b'; END`,
			`DELETE FROM t`,
		}, []string{`SELECT v,w FROM log ORDER BY v`, `SELECT k,b FROM t ORDER BY k`}},

		{"reseek-recomputes-a-generated-column", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b, c AS (b||'#'))`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t(a,b) VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.c); INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b='UP' WHERE a=2; END`,
			`DELETE FROM t`,
		}, []string{`SELECT v,w FROM log ORDER BY v`}},

		{"reseek-restores-real-affinity", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b REAL)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t VALUES(1,1.0),(2,2.0)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.b); INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b=99 WHERE a=2; END`,
			`DELETE FROM t`,
		}, []string{`SELECT v,w,typeof(w) FROM log ORDER BY v`}},

		{"reseek-pads-a-row-older-than-an-added-column", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q')`,
			`ALTER TABLE t ADD COLUMN c DEFAULT 'z'`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.c); INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET c='UP' WHERE a=2; END`,
			`DELETE FROM t`,
		}, []string{`SELECT v,w FROM log ORDER BY v`}},

		// The UPDATE loop's SET right-hand side reads the re-seeked cursor too
		// (update.c:961), so the cascade's rewrite decides the STORED value.
		{"reseek-feeds-the-update-set-expression", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v)`,
			`INSERT INTO t VALUES(1,1),(2,10),(3,100)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(old.a); INSERT INTO log VALUES(old.a||':'||old.b||'->'||new.b); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b=b*1000 WHERE a=3; END`,
			`UPDATE t SET b=b+1`,
		}, []string{`SELECT v FROM log ORDER BY rowid`, `SELECT a,b FROM t ORDER BY a`, `SELECT changes()`}},

		// Inside an explicit transaction, so the commit path this statement
		// takes is the one commitIsNoOp guards rather than autocommit's.
		{"reseek-inside-an-explicit-transaction", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.b); INSERT INTO u VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b='UP' WHERE a=2; END`,
			`BEGIN`, `DELETE FROM t`, `COMMIT`,
		}, []string{`SELECT v,w FROM log ORDER BY v`, `SELECT a,b FROM t ORDER BY a`}},

		// ---- the FROZEN half ----
		//
		// The three below assert what the re-seek must NOT do. Only the row's
		// CONTENT is refreshed: the rowid SET stays exactly as
		// materializeRowStore froze it, which is SQLite's own pass-one list. A
		// row the cascade INSERTS is not picked up, a row it DELETES drops out,
		// and neither is revisited. They pass with the re-seek reduced to a
		// presence test, and are here to pin the boundary a "just
		// re-materialize the cursor" simplification would cross.
		{"frozen-set-ignores-a-cascade-insert", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO u VALUES(old.a); INSERT INTO log VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN INSERT INTO t VALUES(99,'NEW'); END`,
			`DELETE FROM t`,
		}, []string{`SELECT v FROM log ORDER BY rowid`, `SELECT a,b FROM t ORDER BY a`, `SELECT changes()`}},

		{"frozen-set-drops-a-cascade-deleted-row-update", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(old.a); INSERT INTO log VALUES(old.a); END`,
			`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN DELETE FROM t WHERE a=2; END`,
			`UPDATE t SET b=b||'!'`,
		}, []string{`SELECT v FROM log ORDER BY rowid`, `SELECT a,b FROM t ORDER BY a`, `SELECT changes()`}},

		{"recursive-triggers-on-self-update-during-a-delete", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN UPDATE t SET b='X' WHERE a=old.a+2; INSERT INTO log VALUES(old.a||'/'||old.b); END`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('u'||old.a); END`,
			`DELETE FROM t`,
		}, []string{`SELECT x FROM log ORDER BY rowid`, `SELECT a,b FROM t ORDER BY a`}},
	})
}
