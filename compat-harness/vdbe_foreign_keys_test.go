// FOREIGN KEY constraint enforcement tests, run through the driver to test
// that the pragma setting persists across statements.
package compat

import "testing"

// foreignKeyCases each run on one connection. Every SELECT is written to make
// the resulting STATE observable, because that is where an action's effect (or
// a wrongly-reverted statement) shows up.
var foreignKeyCases = []struct {
	name  string
	stmts []string
}{
	{"pragma-getter-setter", []string{
		`PRAGMA foreign_keys`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA foreign_keys`,
		`PRAGMA main.foreign_keys`,
		`PRAGMA foreign_keys=off`,
		`PRAGMA foreign_keys`,
		`PRAGMA foreign_keys=1`,
		`PRAGMA foreign_keys`,
	}},
	{"child-insert-update-delete", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE c(x, y REFERENCES p)`,
		`INSERT INTO p VALUES(1,'a'),(2,'b')`,
		`INSERT INTO c VALUES('k',1)`,
		`INSERT INTO c VALUES('k',9)`,
		`INSERT INTO c VALUES('k',NULL)`,
		`UPDATE c SET y=9 WHERE y=1`,
		`UPDATE c SET y=2 WHERE y=1`,
		`SELECT x,y FROM c ORDER BY rowid`,
		`DELETE FROM p WHERE id=2`,
		`DELETE FROM p WHERE id=1`,
		`UPDATE p SET id=5 WHERE id=2`,
		`SELECT id FROM p ORDER BY id`,
	}},
	{"statement-end-not-per-row", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, other REFERENCES t(id))`,
		`INSERT INTO t VALUES(1,2),(2,1)`,
		`SELECT id,other FROM t ORDER BY id`,
		`DELETE FROM t`,
		`INSERT INTO t VALUES(2,1),(1,2)`,
		`SELECT count(*) FROM t`,
		`INSERT INTO t SELECT 4,5 UNION ALL SELECT 5,4`,
		`SELECT count(*) FROM t`,
		`INSERT INTO t VALUES(9,99)`,
		`SELECT count(*) FROM t`,
	}},
	{"affinity-and-collation-of-the-seek", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES('1')`,
		`INSERT INTO c VALUES(1.0)`,
		`SELECT y, typeof(y) FROM c ORDER BY rowid`,
		`CREATE TABLE p2(k TEXT PRIMARY KEY)`,
		`CREATE TABLE c2(y REFERENCES p2)`,
		`INSERT INTO p2 VALUES('7')`,
		`INSERT INTO c2 VALUES(7)`,
		`CREATE TABLE p3(k TEXT PRIMARY KEY COLLATE NOCASE)`,
		`CREATE TABLE c3(y REFERENCES p3)`,
		`INSERT INTO p3 VALUES('AbC')`,
		`INSERT INTO c3 VALUES('abc')`,
		`CREATE TABLE pn(k TEXT PRIMARY KEY)`,
		`CREATE TABLE cn(y TEXT COLLATE NOCASE REFERENCES pn)`,
		`INSERT INTO pn VALUES('AB')`,
		`INSERT INTO cn VALUES('ab')`,
		`CREATE TABLE pb(k BLOB PRIMARY KEY)`,
		`CREATE TABLE cb(y REFERENCES pb)`,
		`INSERT INTO pb VALUES('x')`,
		`INSERT INTO cb VALUES('x')`,
		`INSERT INTO cb VALUES(x'78')`,
		`SELECT (SELECT count(*) FROM c2),(SELECT count(*) FROM c3),(SELECT count(*) FROM cn),(SELECT count(*) FROM cb)`,
	}},
	{"resolution-errors", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE c(y REFERENCES nosuch)`,
		`INSERT INTO c VALUES(1)`,
		`INSERT INTO c VALUES(NULL)`,
		`DELETE FROM c`,
		`SELECT count(*) FROM c`,
		`CREATE TABLE p(a,b)`,
		`CREATE TABLE d(y REFERENCES p(a))`,
		`INSERT INTO d VALUES(1)`,
		`INSERT INTO d VALUES(NULL)`,
		`CREATE TABLE e(y REFERENCES p)`,
		`INSERT INTO e VALUES(1)`,
		`CREATE UNIQUE INDEX pa ON p(a)`,
		`INSERT INTO d VALUES(1)`,
		`INSERT INTO p VALUES(1,1)`,
		`INSERT INTO d VALUES(1)`,
		`SELECT count(*) FROM d`,
		`CREATE VIEW pv AS SELECT a FROM p`,
		`CREATE TABLE f(y REFERENCES pv)`,
		`INSERT INTO f VALUES(1)`,
		`CREATE TABLE g(y REFERENCES p(rowid))`,
		`INSERT INTO g VALUES(1)`,
	}},
	{"composite-match-simple", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a,b,PRIMARY KEY(a,b))`,
		`CREATE TABLE c(x,y,FOREIGN KEY(x,y) REFERENCES p(a,b))`,
		`INSERT INTO p VALUES(1,2)`,
		`INSERT INTO c VALUES(1,2)`,
		`INSERT INTO c VALUES(1,3)`,
		`INSERT INTO c VALUES(1,NULL)`,
		`INSERT INTO c VALUES(NULL,NULL)`,
		`SELECT count(*) FROM c`,
		`CREATE TABLE p2(a,b,UNIQUE(a,b))`,
		`CREATE TABLE c3(x,y,FOREIGN KEY(y,x) REFERENCES p2(b,a))`,
		`INSERT INTO p2 VALUES(1,2)`,
		`INSERT INTO c3 VALUES(1,2)`,
		`INSERT INTO c3 VALUES(2,1)`,
		`SELECT count(*) FROM c3`,
	}},
	{"on-delete-cascade", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(x, y DEFAULT 77 REFERENCES p ON DELETE CASCADE)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES('k1',1),('k2',2)`,
		`DELETE FROM p WHERE id=1`,
		`SELECT x,y FROM c ORDER BY x`,
		`SELECT id FROM p ORDER BY id`,
	}},
	{"on-update-cascade", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(x, y DEFAULT 77 REFERENCES p ON UPDATE CASCADE)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES('k1',1),('k2',2)`,
		`UPDATE p SET id=9 WHERE id=1`,
		`SELECT x,y FROM c ORDER BY x`,
		`SELECT id FROM p ORDER BY id`,
	}},
	{"on-delete-set-null-and-set-default", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(x, y REFERENCES p ON DELETE SET NULL)`,
		`CREATE TABLE d(x, y DEFAULT 77 REFERENCES p ON DELETE SET DEFAULT)`,
		`CREATE TABLE e(x, y REFERENCES p ON DELETE SET DEFAULT)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES('k1',1),('k2',2)`,
		`INSERT INTO d VALUES('k1',1)`,
		`INSERT INTO e VALUES('k1',1)`,
		`DELETE FROM p WHERE id=1`,
		`SELECT x,y FROM c ORDER BY x`,
		`SELECT x,y FROM d ORDER BY x`,
		`SELECT x,y FROM e ORDER BY x`,
		`SELECT id FROM p ORDER BY id`,
	}},
	{"restrict-and-no-action", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cr(y REFERENCES p ON DELETE RESTRICT)`,
		`CREATE TABLE cn(y REFERENCES p ON DELETE NO ACTION)`,
		`CREATE TABLE cu(y REFERENCES p ON UPDATE RESTRICT)`,
		`INSERT INTO p VALUES(1),(2),(3)`,
		`INSERT INTO cr VALUES(1)`,
		`INSERT INTO cn VALUES(2)`,
		`INSERT INTO cu VALUES(3)`,
		`DELETE FROM p WHERE id=1`,
		`DELETE FROM p WHERE id=2`,
		`UPDATE p SET id=9 WHERE id=3`,
		`SELECT id FROM p ORDER BY id`,
	}},
	{"cascade-chain-and-self-reference", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE a(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, aid REFERENCES a ON DELETE CASCADE)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, bid REFERENCES b ON DELETE CASCADE)`,
		`INSERT INTO a VALUES(1)`,
		`INSERT INTO b VALUES(10,1)`,
		`INSERT INTO c VALUES(100,10)`,
		`DELETE FROM a`,
		`SELECT (SELECT count(*) FROM a),(SELECT count(*) FROM b),(SELECT count(*) FROM c)`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, parent REFERENCES t ON DELETE CASCADE)`,
		`INSERT INTO t VALUES(1,NULL),(2,1),(3,2)`,
		`DELETE FROM t WHERE id=1`,
		`SELECT id FROM t ORDER BY id`,
	}},
	{"self-reference-delete-all", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE tr(id INTEGER PRIMARY KEY, o REFERENCES tr ON DELETE RESTRICT)`,
		`CREATE TABLE tn(id INTEGER PRIMARY KEY, o REFERENCES tn ON DELETE NO ACTION)`,
		`CREATE TABLE tc(id INTEGER PRIMARY KEY, o REFERENCES tc ON DELETE CASCADE)`,
		`INSERT INTO tr VALUES(1,NULL),(2,1)`,
		`INSERT INTO tn VALUES(1,NULL),(2,1)`,
		`INSERT INTO tc VALUES(1,NULL),(2,1)`,
		`DELETE FROM tr`,
		`SELECT count(*) FROM tr`,
		`DELETE FROM tn`,
		`SELECT count(*) FROM tn`,
		`DELETE FROM tc`,
		`SELECT count(*) FROM tc`,
	}},
	{"parent-update-that-does-not-change-the-key", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE c(y REFERENCES p ON UPDATE SET NULL)`,
		`INSERT INTO p VALUES(1,'a')`,
		`INSERT INTO c VALUES(1)`,
		`UPDATE p SET id=id`,
		`SELECT y FROM c`,
		`UPDATE p SET v='b'`,
		`SELECT y FROM c`,
		`UPDATE p SET id=1`,
		`SELECT y FROM c`,
		`UPDATE p SET id=2`,
		`SELECT y FROM c`,
	}},
	{"pre-existing-violation-counter-rules", []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(x, y REFERENCES p)`,
		`INSERT INTO c VALUES('a',1)`,
		`PRAGMA foreign_keys=ON`,
		`UPDATE c SET x='b'`,
		`SELECT x,y FROM c`,
		`UPDATE c SET y=y`,
		`UPDATE c SET y=2 WHERE y=1`,
		`SELECT x,y FROM c`,
		`INSERT INTO c VALUES('z',3)`,
		`SELECT count(*) FROM c`,
		`INSERT INTO p VALUES(2)`,
		`DELETE FROM c`,
		`SELECT count(*) FROM c`,
	}},
	{"child-update-changing-only-the-rowid", []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO c VALUES(1,7)`,
		`PRAGMA foreign_keys=ON`,
		`UPDATE c SET k=2`,
		`SELECT k,y FROM c`,
	}},
	{"replace-and-upsert-on-the-parent", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE c(y REFERENCES p ON DELETE CASCADE)`,
		`INSERT INTO p VALUES(1,'a')`,
		`INSERT INTO c VALUES(1)`,
		`INSERT OR REPLACE INTO p VALUES(1,'b')`,
		`SELECT count(*) FROM c`,
		`CREATE TABLE p2(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE c2(y REFERENCES p2)`,
		`INSERT INTO p2 VALUES(1,'a')`,
		`INSERT INTO c2 VALUES(1)`,
		`INSERT OR REPLACE INTO p2 VALUES(1,'b')`,
		`SELECT count(*) FROM c2`,
		`CREATE TABLE p3(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE c3(y REFERENCES p3 ON UPDATE CASCADE)`,
		`INSERT INTO p3 VALUES(1,'a')`,
		`INSERT INTO c3 VALUES(1)`,
		`INSERT INTO p3 VALUES(1,'b') ON CONFLICT(id) DO UPDATE SET id=7`,
		`SELECT (SELECT group_concat(id) FROM p3),(SELECT group_concat(y) FROM c3)`,
	}},
	{"replace-driven-parent-delete-orphans-a-child", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, u UNIQUE)`,
		`CREATE TABLE c(y REFERENCES p)`,
		`INSERT INTO p VALUES(1,10)`,
		`INSERT INTO c VALUES(1)`,
		`INSERT OR REPLACE INTO p VALUES(2,10)`,
		`SELECT (SELECT group_concat(id) FROM p),(SELECT group_concat(y) FROM c)`,
	}},
	{"or-ignore-or-replace-never-suppress-a-violation", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p)`,
		`INSERT INTO c VALUES(1)`,
		`INSERT OR IGNORE INTO c VALUES(1)`,
		`INSERT OR REPLACE INTO c VALUES(1)`,
		`SELECT count(*) FROM c`,
	}},
	{"action-hits-the-child-constraints", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y NOT NULL REFERENCES p ON DELETE SET NULL)`,
		`CREATE TABLE d(y CHECK(y IS NOT NULL) REFERENCES p ON DELETE SET NULL)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES(1)`,
		`INSERT INTO d VALUES(2)`,
		`DELETE FROM p WHERE id=1`,
		`DELETE FROM p WHERE id=2`,
		`SELECT (SELECT count(*) FROM c),(SELECT count(*) FROM d),(SELECT count(*) FROM p)`,
	}},
	// e_fkey-52.6: a composite ON UPDATE CASCADE whose new parent key has a
	// NULL component. MATCH SIMPLE satisfies such a key vacuously, but the
	// CHILD ROW STILL EXISTS and still receives the propagated (partly-NULL)
	// values -- it must not be treated as an orphan and deleted. Every step
	// but the last already passed before the fix; the last one is the bug
	// itself (musql returned zero rows where C SQLite keeps the row with
	// d turned to NULL). See engine/fk.go's fkParentKeyRaw.
	{"cascade-propagates-a-null-parent-key-component", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE zeus(a INTEGER COLLATE NOCASE, b, PRIMARY KEY(a, b))`,
		`CREATE TABLE apollo(c, d, FOREIGN KEY(c, d) REFERENCES zeus ON UPDATE CASCADE)`,
		`INSERT INTO zeus VALUES('abc', 'xyz')`,
		`INSERT INTO apollo VALUES('ABC', 'xyz')`,
		`UPDATE zeus SET a = 1, b = 1`,
		`SELECT typeof(c), c, typeof(d), d FROM apollo`,
		`UPDATE zeus SET b = '1'`,
		`SELECT typeof(c), c, typeof(d), d FROM apollo`,
		`UPDATE zeus SET b = NULL`,
		`SELECT typeof(c), c, typeof(d), d FROM apollo`,
		`SELECT count(*) FROM zeus`,
		`SELECT count(*) FROM apollo`,
		`PRAGMA foreign_key_check`,
		// A SINGLE-column key cascading to NULL is the same rule without a
		// composite key to obscure it.
		`CREATE TABLE p1(k PRIMARY KEY)`,
		`CREATE TABLE c1(x REFERENCES p1 ON UPDATE CASCADE)`,
		`INSERT INTO p1 VALUES('k')`,
		`INSERT INTO c1 VALUES('k')`,
		`UPDATE p1 SET k = NULL`,
		`SELECT typeof(x), x FROM c1`,
		`SELECT count(*) FROM c1`,
		// A NULL propagated one level further: a child of the child.
		`CREATE TABLE g1(a COLLATE NOCASE, b, PRIMARY KEY(a, b))`,
		`CREATE TABLE g2(c, d, PRIMARY KEY(c, d), FOREIGN KEY(c, d) REFERENCES g1 ON UPDATE CASCADE)`,
		`CREATE TABLE g3(e, f, FOREIGN KEY(e, f) REFERENCES g2 ON UPDATE CASCADE)`,
		`INSERT INTO g1 VALUES('abc', 'xyz')`,
		`INSERT INTO g2 VALUES('abc', 'xyz')`,
		`INSERT INTO g3 VALUES('abc', 'xyz')`,
		`UPDATE g1 SET b = NULL`,
		`SELECT typeof(c), c, typeof(d), d FROM g2`,
		`SELECT typeof(e), e, typeof(f), f FROM g3`,
		`SELECT count(*) FROM g2`,
		`SELECT count(*) FROM g3`,
	}},
	// ON DELETE CASCADE stays a real delete when the parent row itself is
	// removed (as opposed to updated with a NULL component) -- fkApplyAction's
	// nil-newRow check must still fire for a genuine delete.
	{"delete-cascade-still-deletes-when-the-parent-row-is-gone", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE zdel(a COLLATE NOCASE, b, PRIMARY KEY(a, b))`,
		`CREATE TABLE cdel(c, d, FOREIGN KEY(c, d) REFERENCES zdel ON DELETE CASCADE)`,
		`INSERT INTO zdel VALUES('abc', 'xyz')`,
		`INSERT INTO cdel VALUES('abc', 'xyz')`,
		`DELETE FROM zdel WHERE a = 'abc'`,
		`SELECT count(*) FROM zdel`,
		`SELECT count(*) FROM cdel`,
	}},
	// ON UPDATE SET NULL and ON UPDATE SET DEFAULT never consult the new
	// parent key at all, so a NULL parent-key component is not their bug --
	// but SET DEFAULT can still (correctly) fail when the default values
	// don't happen to name an existing parent row.
	{"set-null-and-set-default-with-a-null-parent-key", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE zn(a COLLATE NOCASE, b, PRIMARY KEY(a, b))`,
		`CREATE TABLE cn(c, d, FOREIGN KEY(c, d) REFERENCES zn ON UPDATE SET NULL)`,
		`INSERT INTO zn VALUES('abc', 'xyz')`,
		`INSERT INTO cn VALUES('abc', 'xyz')`,
		`UPDATE zn SET b = NULL`,
		`SELECT typeof(c), c, typeof(d), d FROM cn`,
		`SELECT count(*) FROM cn`,
		`CREATE TABLE zd(a COLLATE NOCASE, b, PRIMARY KEY(a, b))`,
		`CREATE TABLE cd(c DEFAULT 'defc', d DEFAULT 'defd', FOREIGN KEY(c, d) REFERENCES zd ON UPDATE SET DEFAULT)`,
		`INSERT INTO zd VALUES('abc', 'xyz')`,
		`INSERT INTO cd VALUES('abc', 'xyz')`,
		`UPDATE zd SET b = NULL`,
		`SELECT typeof(c), c, typeof(d), d FROM cd`,
		`SELECT count(*) FROM cd`,
	}},
	// A NOT NULL child column still rejects the cascaded NULL outright rather
	// than silently dropping the row -- fkStoreChildRow's own constraint check
	// runs before the row is stored either way, unaffected by this fix.
	{"cascaded-null-still-hits-a-not-null-child-column", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE znn(a COLLATE NOCASE, b, PRIMARY KEY(a, b))`,
		`CREATE TABLE cnn(c, d NOT NULL, FOREIGN KEY(c, d) REFERENCES znn ON UPDATE CASCADE)`,
		`INSERT INTO znn VALUES('abc', 'xyz')`,
		`INSERT INTO cnn VALUES('abc', 'xyz')`,
		`UPDATE znn SET b = NULL`,
		`SELECT typeof(c), c, typeof(d), d FROM cnn`,
		`SELECT count(*) FROM cnn`,
	}},
	{"drop-table-parent-and-child", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`DROP TABLE p`,
		`SELECT count(*) FROM c`,
		`DROP TABLE c`,
		`SELECT count(*) FROM sqlite_master`,
	}},
	{"drop-table-parent-cascades", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p ON DELETE CASCADE)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`DROP TABLE p`,
		`SELECT count(*) FROM c`,
	}},
	{"drop-table-self-referencing-and-child-first", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, o REFERENCES t)`,
		`INSERT INTO t VALUES(1,NULL),(2,1)`,
		`DROP TABLE t`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`DROP TABLE c`,
		`DROP TABLE p`,
		`SELECT count(*) FROM sqlite_master`,
	}},
	{"add-column-references-default-rule", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(x)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES('k')`,
		`ALTER TABLE c ADD COLUMN y REFERENCES p`,
		`ALTER TABLE c ADD COLUMN z DEFAULT 5 REFERENCES p`,
		`ALTER TABLE c ADD COLUMN w DEFAULT NULL REFERENCES p`,
		`SELECT x,y,w FROM c`,
		`PRAGMA foreign_keys=OFF`,
		`ALTER TABLE c ADD COLUMN q DEFAULT 5 REFERENCES p`,
		`SELECT x,y,w,q FROM c`,
	}},
	{"parent-key-via-unique-index-and-without-rowid", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a,b UNIQUE)`,
		`CREATE TABLE c(y REFERENCES p(b))`,
		`INSERT INTO p VALUES(1,10)`,
		`INSERT INTO c VALUES(10)`,
		`INSERT INTO c VALUES(11)`,
		`CREATE TABLE wp(k TEXT PRIMARY KEY) WITHOUT ROWID`,
		`CREATE TABLE wc(a PRIMARY KEY, y REFERENCES wp ON DELETE CASCADE) WITHOUT ROWID`,
		`INSERT INTO wp VALUES('x')`,
		`INSERT INTO wc VALUES(1,'x')`,
		`INSERT INTO wc VALUES(2,'z')`,
		`DELETE FROM wp`,
		`SELECT (SELECT count(*) FROM wp),(SELECT count(*) FROM wc),(SELECT count(*) FROM c)`,
	}},
	{"two-foreign-keys-on-one-child", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p1(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE p2(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(a REFERENCES p1 ON DELETE CASCADE, b REFERENCES p2 ON DELETE SET NULL)`,
		`INSERT INTO p1 VALUES(1)`,
		`INSERT INTO p2 VALUES(2)`,
		`INSERT INTO c VALUES(1,2)`,
		`DELETE FROM p2`,
		`SELECT a,b FROM c`,
		`DELETE FROM p1`,
		`SELECT count(*) FROM c`,
	}},
	{"child-column-is-the-childs-own-ipk", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY REFERENCES p ON DELETE CASCADE)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES(1),(2)`,
		`DELETE FROM p WHERE id=1`,
		`SELECT id FROM c`,
	}},
	{"generated-child-column-and-quoted-parent-name", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(a, b AS (a+1) REFERENCES p)`,
		`INSERT INTO p VALUES(2)`,
		`INSERT INTO c VALUES(1)`,
		`INSERT INTO c VALUES(9)`,
		`CREATE TABLE "Par"(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE d(y REFERENCES PAR)`,
		`INSERT INTO "Par" VALUES(1)`,
		`INSERT INTO d VALUES(1)`,
		`INSERT INTO d VALUES(2)`,
		`SELECT (SELECT count(*) FROM c),(SELECT count(*) FROM d)`,
	}},
	{"immediate-inside-an-explicit-transaction", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p)`,
		`BEGIN`,
		`INSERT INTO c VALUES(1)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`COMMIT`,
		`SELECT (SELECT count(*) FROM p),(SELECT count(*) FROM c)`,
	}},
	{"setter-inside-a-transaction-is-ignored", []string{
		`PRAGMA foreign_keys=ON`,
		`BEGIN`,
		`PRAGMA foreign_keys=OFF`,
		`PRAGMA foreign_keys`,
		`COMMIT`,
		`PRAGMA foreign_keys`,
	}},
	{"temp-child-resolves-in-the-temp-catalog-only", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE tc(y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO tc VALUES(1)`,
		`INSERT INTO tc VALUES(2)`,
		`SELECT count(*) FROM tc`,
	}},
	{"foreign-key-check-and-integrity-check-ignore-the-flag", []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p)`,
		`INSERT INTO c VALUES(1)`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA integrity_check`,
		`PRAGMA foreign_key_check(c)`,
		`SELECT count(*) FROM c`,
	}},
	{"parent-index-must-use-the-default-collation", []string{
		// e_fkey.test 20.5/20.6: a UNIQUE index over the right columns is NOT
		// enough -- it must use each column's OWN declared collating sequence,
		// and a mismatch in EITHER direction is a "foreign key mismatch".
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p4(a PRIMARY KEY, b)`,
		`CREATE UNIQUE INDEX p4i ON p4(b COLLATE nocase)`,
		`CREATE TABLE c4(c REFERENCES p4(b), d)`,
		`INSERT INTO c4 VALUES('a','b')`,
		`DELETE FROM c4`,
		`CREATE TABLE p5(a PRIMARY KEY, b COLLATE nocase)`,
		`CREATE UNIQUE INDEX p5i ON p5(b COLLATE binary)`,
		`CREATE TABLE c5(c REFERENCES p5(b), d)`,
		`INSERT INTO c5 VALUES('a','b')`,
		// ...while an index that DOES inherit the column's collation resolves,
		// and the seek then folds case.
		`CREATE TABLE p8(a PRIMARY KEY, b COLLATE nocase)`,
		`CREATE UNIQUE INDEX p8i ON p8(b)`,
		`CREATE TABLE c8(c REFERENCES p8(b), d)`,
		`INSERT INTO p8 VALUES(1,'B')`,
		`INSERT INTO c8 VALUES('b','x')`,
		`CREATE TABLE p9(a PRIMARY KEY, b)`,
		`CREATE UNIQUE INDEX p9i ON p9(b)`,
		`CREATE TABLE c9(c REFERENCES p9(b), d)`,
		`INSERT INTO p9 VALUES(1,'B')`,
		`INSERT INTO c9 VALUES('B','x')`,
		`SELECT (SELECT count(*) FROM c8),(SELECT count(*) FROM c9),(SELECT count(*) FROM c4)`,
		// A parent key list that does not match the parent's PRIMARY KEY arity
		// is a mismatch too (e_fkey 20.7/20.8).
		`CREATE TABLE p6(a PRIMARY KEY, b)`,
		`CREATE TABLE c6(c, d, FOREIGN KEY(c, d) REFERENCES p6)`,
		`INSERT INTO c6 VALUES('a','b')`,
		`CREATE TABLE p7(a, b, PRIMARY KEY(a, b))`,
		`CREATE TABLE c7(c, d REFERENCES p7)`,
		`INSERT INTO c7 VALUES('a','b')`,
	}},
	{"action-reach-decides-which-mismatch-fires", []string{
		// fkey2.test's 20150416-100: t's foreign key is a mismatch, and whether
		// a statement on t1 sees it depends on which ACTION programs that
		// statement makes C SQLite generate. A plain INSERT generates none.
		`PRAGMA foreign_keys=1`,
		`CREATE TABLE t1(x PRIMARY KEY)`,
		`CREATE TABLE t(y REFERENCES t0(x) ON DELETE SET DEFAULT)`,
		`CREATE TABLE t0(y REFERENCES t1 ON DELETE SET NULL)`,
		`INSERT INTO t1 VALUES(9)`,
		`UPDATE t1 SET x=x`,
		`SELECT count(*) FROM t1`,
	}},
	{"replace-whose-cascade-orphans-the-new-row", []string{
		// fkey1-5.2, whose own comment says it: the REPLACE deletes (2,1), the
		// cascade then removes (3,2) -- which would have been the new row's
		// parent -- so the insert fails.
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t11(x INTEGER PRIMARY KEY, parent REFERENCES t11 ON DELETE CASCADE)`,
		`INSERT INTO t11 VALUES (1, NULL), (2, 1), (3, 2)`,
		`SELECT x,parent FROM t11 ORDER BY x`,
	}},
	{"enforcement-off-is-still-a-pure-no-op", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p ON DELETE CASCADE)`,
		`INSERT INTO c VALUES(1),(2),(3)`,
		`INSERT INTO p VALUES(1)`,
		`DELETE FROM p`,
		`SELECT count(*) FROM c`,
		`DROP TABLE p`,
		`SELECT count(*) FROM c`,
	}},
	// A DELETE TRIGGER on the parent does not stop the DROP, does not fire, and
	// does not stop the foreign key actions -- e_fkey.test's own R-11078-03945.
	// Verified against mattn/go-sqlite3 3.53.3; see
	// compat-harness/fkdrop_trigger_and_group_collation_test.go for the probes.
	{"drop-a-parent-that-has-a-delete-trigger", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t1(x PRIMARY KEY)`,
		`CREATE TABLE t2(y REFERENCES t1 ON DELETE CASCADE)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tt1 AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES(old.x); END`,
		`INSERT INTO t1 VALUES(1)`,
		`INSERT INTO t2 VALUES(1)`,
		`DROP TABLE t1`,
		`SELECT count(*) FROM t2`,
		`SELECT count(*) FROM log`,
	}},
}

func TestForeignKeyEnforcement(t *testing.T) {
	for _, tc := range foreignKeyCases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "fk/"+tc.name, tc.stmts) })
	}
}

// foreignKeyDeclinedCases are the shapes engine/fk.go deliberately DECLINES
// (see its doc comment's out-of-scope list). differ() cannot gate them -- a
// decline is an error where C SQLite succeeds -- so they are pinned here
// instead: each must be REJECTED by the pure engine, and the reason must be in
// the error, so the day one is implemented this test fails and the case moves
// into foreignKeyCases above.
var foreignKeyDeclinedCases = []struct {
	name  string
	setup []string
	stmt  string
	want  string
}{
	{
		// "action-would-fire-a-child-trigger" was here and is CLOSED: in C an
		// action IS a trigger program (fkActionTrigger, fkey.c), so its implicit
		// DELETE/UPDATE is ordinary and the child's own triggers run.
		// fkApplyAction fires them now, through firePlanForRow -- see
		// compat-harness/fk_action_child_triggers_test.go, whose fourteen shapes
		// were measured against 3.53.3 first.
		//
		// The ORDERING half is what remains, and it is a different case: C codes
		// an action's own NESTED actions ahead of the row's AFTER trigger
		// (sqlite3GenerateRowDelete emits sqlite3FkActions before the AFTER
		// block), so a CHAIN interleaves depth-first where this engine drains one
		// level at a time. Final state identical; the trigger bodies' view of it
		// not.
		name: "action-chain-whose-nested-action-must-precede-an-AFTER-trigger",
		setup: []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(y INTEGER PRIMARY KEY REFERENCES p ON DELETE CASCADE)`,
			`CREATE TABLE g(z REFERENCES c ON DELETE CASCADE)`,
			`CREATE TABLE log(m)`,
			`CREATE TRIGGER tc AFTER DELETE ON c BEGIN INSERT INTO log VALUES('c'); END`,
			`INSERT INTO p VALUES(1)`,
			`INSERT INTO c VALUES(1)`,
			`INSERT INTO g VALUES(1)`,
		},
		stmt: `DELETE FROM p`,
		want: "onward action",
	},
	{
		// Only a trigger whose BODY NO LONGER COMPILES is declined -- a healthy
		// one is accepted and does not fire, which is why the case that used to
		// live here now sits in foreignKeyCases above. This remaining decline is
		// an OVER-decline: C SQLite accepts this DROP too (see
		// compat-harness/fkdrop_trigger_and_group_collation_test.go for the
		// probes), so it costs a mirrored skip and never a wrong answer -- but
		// it is what the engine still does, and this pins where.
		name: "drop-a-parent-whose-delete-trigger-no-longer-compiles",
		setup: []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE log(m)`,
			`CREATE TABLE t1(x PRIMARY KEY)`,
			`CREATE TRIGGER tt1 AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES(old.nope); END`,
			`CREATE TABLE t2(y REFERENCES t1 ON DELETE CASCADE)`,
		},
		stmt: `DROP TABLE t1`,
		want: "does not compile",
	},
	{
		name: "replace-whose-action-acts-on-the-table-it-inserts-into",
		setup: []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE t11(x INTEGER PRIMARY KEY, parent REFERENCES t11 ON DELETE CASCADE)`,
			`INSERT INTO t11 VALUES (1, NULL), (2, 1), (3, 2)`,
		},
		stmt: `INSERT OR REPLACE INTO t11 VALUES (2, 3)`,
		want: "also inserts into",
	},
	{
		name: "mismatch-reached-through-an-action-chain",
		setup: []string{
			`PRAGMA foreign_keys=1`,
			`CREATE TABLE t1(x PRIMARY KEY)`,
			`CREATE TABLE t(y REFERENCES t0(x) ON DELETE SET DEFAULT)`,
			`CREATE TABLE t0(y REFERENCES t1 ON DELETE SET NULL)`,
		},
		stmt: `DELETE FROM t1`,
		want: "foreign key action",
	},
}

// The "defer_foreign_keys outside an explicit transaction" case used to sit at
// the end of that list and is CLOSED: the pragma's whole autocommit lifetime is
// carried on the Conn now, including C's rule that sqlite3VdbeHalt clears the
// flag when the statement's Vdbe was a reader and left autocommit on
// (vdbeaux.c:3401-3405). See compat-harness/defer_foreign_keys_pragma_test.go,
// whose eleven sequences pin it, and driver's Conn.deferFKs.

func TestForeignKeyDeclines(t *testing.T) {
	for _, tc := range foreignKeyDeclinedCases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, "musql", append(append([]string{}, tc.setup...), tc.stmt))
			last := res[len(res)-1]
			if last["kind"] != "error" {
				t.Fatalf("%s: expected a decline, got %v", tc.stmt, last)
			}
		})
	}
}

// TestDeferredForeignKeysThroughTheDriver is the case that used to head
// foreignKeyDeclinedCases: a DEFERRABLE INITIALLY DEFERRED key inside a
// DRIVER-held transaction. The decline's reason was that this driver's
// "BEGIN" holds an engine session open and commits it by CLOSING it, so there
// was no COMMIT statement for the deferred check to run at, and nowhere to put
// C SQLite's "a failed COMMIT leaves the transaction open".
//
// Both halves have a home now: the driver runs the check itself before it
// commits the session it is holding (engine.CheckDeferredForeignKeys, which
// is fkCommitCheck, i.e. sqlite3VdbeCheckFk's deferred arm), and it leaves
// conn.tx in place when that reports a violation -- so the transaction really
// does stay open and the ROLLBACK after it works. That makes this a differ()
// case: the whole script, including the COMMIT's error and the row count
// after it, must match the oracle.
func TestDeferredForeignKeysThroughTheDriver(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"violation-reported-at-commit", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
			`BEGIN`, `INSERT INTO c VALUES(1)`, `COMMIT`,
			`SELECT count(*) FROM c`,
		}},
		{"a-failed-commit-leaves-the-transaction-open", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
			`BEGIN`, `INSERT INTO c VALUES(1)`, `COMMIT`,
			`ROLLBACK`, `SELECT count(*) FROM c`,
		}},
		{"resolved-before-the-commit", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
			`BEGIN`, `INSERT INTO c VALUES(1)`, `INSERT INTO p VALUES(1)`, `COMMIT`,
			`SELECT count(*) FROM c`,
		}},
		{"a-rollback-discards-it", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
			`BEGIN`, `INSERT INTO c VALUES(1)`, `ROLLBACK`,
			`SELECT count(*) FROM c`,
		}},
		{"a-savepoint-release-is-a-commit", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
			`SAVEPOINT s`, `INSERT INTO c VALUES(1)`, `RELEASE s`,
			`SELECT count(*) FROM c`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "deferred-fk/"+tc.name, tc.stmts) })
	}
}
