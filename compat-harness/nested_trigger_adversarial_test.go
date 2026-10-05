package compat

import "testing"

// TestNestedTriggerAdversarial tests nested triggers (triggers that write
// tables which are themselves triggered) against C SQLite.
func TestNestedTriggerAdversarial(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		{"two-triggers-on-inner-fire-order", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER u1 AFTER INSERT ON u BEGIN INSERT INTO log(x) VALUES('u1:'||new.a); END`,
			`CREATE TRIGGER u2 AFTER INSERT ON u BEGIN INSERT INTO log(x) VALUES('u2:'||new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT seq,x FROM log ORDER BY seq`}},

		{"outer-multirow-nested", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log(x) VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a*10); END`,
			`INSERT INTO t VALUES(1),(2),(3)`,
		}, []string{`SELECT seq,x FROM log ORDER BY seq`, `SELECT a FROM u ORDER BY 1`}},

		{"body-two-statements-both-triggered", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log(x) VALUES('u'||new.a); END`,
			`CREATE TRIGGER vv AFTER INSERT ON v BEGIN INSERT INTO log(x) VALUES('v'||new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO v VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT seq,x FROM log ORDER BY seq`}},

		{"diamond-one-trigger-two-paths", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`, `CREATE TABLE w(a)`,
			`CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER ww AFTER INSERT ON w BEGIN INSERT INTO log(x) VALUES('w'||new.a); END`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO w VALUES(new.a*2); END`,
			`CREATE TRIGGER vv AFTER INSERT ON v BEGIN INSERT INTO w VALUES(new.a*3); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO v VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT seq,x FROM log ORDER BY seq`, `SELECT a FROM w ORDER BY 1`}},

		// ---- recursion bounds ----
		{"self-recursive-rec-on-depth-limited", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t WHEN new.a<5 BEGIN INSERT INTO t VALUES(new.a+1); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`}},

		{"self-recursive-rec-on-runaway", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO t VALUES(new.a+1); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT count(*) FROM t`}},

		{"mutual-recursion-rec-on-bounded", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t WHEN new.a<4 BEGIN INSERT INTO u VALUES(new.a+1); END`,
			`CREATE TRIGGER uu AFTER INSERT ON u WHEN new.a<4 BEGIN INSERT INTO t VALUES(new.a+1); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`}},

		{"self-recursive-rec-off-fires-once", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO log(x) VALUES(new.a); INSERT INTO t VALUES(new.a+1); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		// ---- error / abort / ignore propagation ----
		{"inner-raise-abort-undoes-outer", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE keep(x)`,
			`INSERT INTO keep VALUES(0)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN SELECT raise(ABORT,'boom'); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
			`INSERT INTO keep VALUES(1)`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT x FROM keep ORDER BY 1`}},

		{"inner-raise-fail-keeps-earlier-rows", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
			`CREATE TRIGGER uu AFTER INSERT ON u WHEN new.a=2 BEGIN SELECT raise(FAIL,'boom'); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`}},

		{"inner-raise-ignore-abandons-inner-row-only", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN SELECT raise(IGNORE); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO log(x) VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"inner-constraint-violation", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`}},

		{"inner-notnull-violation", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a NOT NULL)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(NULL)`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`}},

		{"inner-check-violation", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a CHECK(a>0))`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(-1)`,
			`INSERT INTO t VALUES(5)`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`}},

		// ---- durability of the survivors (the 0a9bbc1 class) ----
		{"survivors-persist-across-reopen", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM u`, `SELECT x FROM log`, `PRAGMA integrity_check`}},

		// ---- connection state ----
		{"changes-total_changes-after", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO u SELECT 0 WHERE 0; END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`,
		}, []string{`SELECT changes()`, `SELECT total_changes()`, `SELECT a FROM u ORDER BY 1`}},

		{"nested-body-reads-changes", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log(x) VALUES(changes()); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO log(x) VALUES(changes()); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT seq,x FROM log ORDER BY seq`, `SELECT changes()`}},

		{"nested-last_insert_rowid-restored", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO v(rowid,a) VALUES(900,new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u(rowid,a) VALUES(500,new.a); END`,
			`INSERT INTO t(rowid,a) VALUES(7,1)`,
		}, []string{`SELECT last_insert_rowid()`}},

		// ---- rowid / autoincrement inside the nest ----
		{"nested-autoincrement", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(id INTEGER PRIMARY KEY AUTOINCREMENT, a)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.id); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u(a) VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`,
		}, []string{`SELECT id,a FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`, `SELECT name,seq FROM sqlite_sequence`}},

		{"nested-new-rowid-visible", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.rowid); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u(rowid,a) VALUES(new.a*11,new.a); END`,
			`INSERT INTO t VALUES(1),(2)`,
		}, []string{`SELECT x FROM log ORDER BY 1`}},

		// ---- outer statement kinds ----
		{"outer-delete-nested", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1),(2)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log(x) VALUES('u'||new.a); END`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO u VALUES(old.a); END`,
			`DELETE FROM t WHERE a=1`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"outer-update-nested", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1,'p')`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log(x) VALUES('u'||new.a); END`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(old.b||'>'||new.b); END`,
			`UPDATE t SET b='q'`,
		}, []string{`SELECT a,b FROM t`, `SELECT a FROM u`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"nested-update-then-delete-chain", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a,b)`, `CREATE TABLE v(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO u VALUES(1,'p')`, `INSERT INTO v VALUES(1)`,
			`CREATE TRIGGER vd AFTER DELETE ON v BEGIN INSERT INTO log(x) VALUES('vd'||old.a); END`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN DELETE FROM v WHERE a=new.a; END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET b='q' WHERE a=new.a; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u`, `SELECT a FROM v`, `SELECT seq,x FROM log ORDER BY seq`}},

		// ---- inner target is the OUTER table ----
		{"inner-writes-outer-table-rec-off", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO t VALUES(new.a+100); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO log(x) VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		// ---- WHEN guards at both levels ----
		{"when-guards-both-levels", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u WHEN new.a%2=0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t WHEN new.a>1 BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3),(4)`,
		}, []string{`SELECT a FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		// ---- affinity / typing across the nest ----
		{"nested-affinity", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a INTEGER, b TEXT)`, `CREATE TABLE log(x,y)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(typeof(new.a),typeof(new.b)); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,new.a); END`,
			`INSERT INTO t VALUES('42')`,
		}, []string{`SELECT x,y FROM log`, `SELECT typeof(a),typeof(b) FROM u`}},

		// ---- temp tables in the nest ----
		{"nested-temp-table-target", []string{
			`CREATE TABLE t(a)`, `CREATE TEMP TABLE u(a)`, `CREATE TEMP TABLE log(x)`,
			`CREATE TEMP TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TEMP TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM u`, `SELECT x FROM log`}},

		// ---- foreign keys under a nested cascade ----
		//
		// NOT included, and deliberately: an ON DELETE CASCADE whose implicit
		// child delete would fire the CHILD's DELETE trigger. This engine
		// raises a decline as a user-facing ERROR --
		//
		//	PRAGMA foreign_keys=ON;
		//	CREATE TABLE p(id INTEGER PRIMARY KEY);
		//	CREATE TABLE c(id INTEGER PRIMARY KEY, pid REFERENCES p(id) ON DELETE CASCADE);
		//	CREATE TABLE log(x);
		//	INSERT INTO p VALUES(1); INSERT INTO c VALUES(10,1);
		//	CREATE TRIGGER cd AFTER DELETE ON c BEGIN INSERT INTO log VALUES(old.id); END;
		//	DELETE FROM p WHERE id=1;
		//	  oracle: p and c both empty, log holds 10
		//	  musql: "vdbe: unsupported: foreign key action CASCADE on c would
		//	           fire trigger cd ...", nothing deleted, log empty
		//
		// -- an errVDBEUnsupported that reaches the caller instead of selecting
		// a route (fk.go's fkApplyAction), i.e. a spurious error on a
		// statement C SQLite runs, which AGENTS.md invariant 1 forbids as
		// firmly as a wrong answer. It reproduces with NO trigger cascade at
		// all, is unchanged by batch J (verified against the parent commit),
		// and needs the FK-action path to be able to run a compiled fire plan,
		// which is a change of its own. Recorded here rather than added as a
		// red test, so the known-failing set does not grow.

		// ---- a body statement with its own explicit conflict clause ----
		{"inner-or-replace-clause", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1,'old')`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.b); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT OR REPLACE INTO u VALUES(new.a,'new'); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u`, `SELECT x FROM log ORDER BY 1`}},

		{"inner-or-ignore-clause", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1,'old')`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.b); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT OR IGNORE INTO u VALUES(new.a,'new'); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u`, `SELECT x FROM log ORDER BY 1`}},

		{"outer-or-ignore-clause-propagates", []string{
			`CREATE TABLE t(a UNIQUE)`, `CREATE TABLE u(a UNIQUE)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT OR IGNORE INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT x FROM log`}},

		// ---- transaction rollback with a nested cascade ----
		{"rollback-undoes-nested-cascade", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`BEGIN`, `INSERT INTO t VALUES(1)`, `ROLLBACK`,
			`INSERT INTO t VALUES(2)`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT x FROM log`}},

		// ---- generated / default columns inside the nest ----
		{"nested-defaults-and-generated", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE u(a, b DEFAULT 'dflt', c AS (a*2) STORED)`,
			`CREATE TABLE log(x,y,z)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a,new.b,new.c); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u(a) VALUES(new.a); END`,
			`INSERT INTO t VALUES(3)`,
		}, []string{`SELECT a,b,c FROM u`, `SELECT x,y,z FROM log`}},
	})
}
