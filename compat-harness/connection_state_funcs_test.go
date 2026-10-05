// This file tests connection-state functions: sqlite_version(), changes(),
// total_changes(), and last_insert_rowid().
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/driver"
)

// connStateProbe queries the connection state after each statement.
const connStateProbe = `SELECT changes(), total_changes(), last_insert_rowid()`

// connStateCases holds test scripts for counting rules.
var connStateCases = []struct {
	name  string
	stmts []string
}{
	// The base rules: a DML statement publishes its own row count (0 included),
	// and DDL/VACUUM/ANALYZE/PRAGMA publish nothing at all -- changes() still
	// reads whatever the last DML left. REPLACE counts only the row it stored,
	// never the ones it displaced, and INSERT OR IGNORE / DO NOTHING count only
	// what they actually wrote.
	{"statement-kinds-and-conflict-counting", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(2,'y'),(3,'z')`,
		`UPDATE t SET b='q' WHERE a=1`,
		`UPDATE t SET b='q2' WHERE a=999`,
		`DELETE FROM t WHERE a=999`,
		`DELETE FROM t WHERE a=3`,
		`CREATE TABLE u(x)`,
		`CREATE INDEX i1 ON t(b)`,
		`DROP TABLE u`,
		`ALTER TABLE t ADD COLUMN c`,
		`REPLACE INTO t VALUES(1,'w',NULL)`,
		`INSERT INTO t VALUES(4,'p',NULL),(5,'q',NULL)`,
		// Displaces BOTH row 4 (by rowid) and row 5 (by the UNIQUE key), and
		// still counts 1.
		`REPLACE INTO t VALUES(4,'q',NULL)`,
		`SELECT a,b FROM t ORDER BY a`,
		`INSERT OR IGNORE INTO t VALUES(1,'zzz',NULL),(99,'nnn',NULL)`,
		`INSERT OR IGNORE INTO t VALUES(1,'aaa',NULL)`,
		`INSERT INTO t VALUES(1,'y2',NULL) ON CONFLICT(a) DO UPDATE SET b='u'`,
		`INSERT INTO t VALUES(1,'y3',NULL) ON CONFLICT(a) DO NOTHING`,
		`DELETE FROM t`,
		`VACUUM`,
		`ANALYZE`,
		`PRAGMA user_version=4`,
	}},
	// A trigger's rows stay out of the outer statement's changes() and go into
	// total_changes(); a body statement reading changes() sees the body
	// statement BEFORE it, and the first one sees the value from before the
	// triggering statement started.
	{"after-trigger-rows-and-in-body-reads", []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(1); INSERT INTO log VALUES(2); END`,
		`INSERT INTO t VALUES(9)`,
		`INSERT INTO t VALUES(1),(2)`,
		`DELETE FROM log`,
		`CREATE TABLE t0(x)`,
		`INSERT INTO t0 VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE t1(k)`,
		`CREATE TABLE t2(a,b,c)`,
		`CREATE TRIGGER r1 AFTER INSERT ON t1 FOR EACH ROW BEGIN INSERT INTO t2 VALUES(NULL, changes(), NULL); UPDATE t0 SET x=x; UPDATE t2 SET b=changes(); END`,
		`DELETE FROM t0 WHERE x=99`,
		`INSERT INTO t1 VALUES(1)`,
		// b==5, the "UPDATE t0 SET x=x" step's own count -- not 0 (the value
		// before the statement) and not 1.
		`SELECT a,b,c FROM t2`,
	}},
	// A BEFORE trigger body is run by the TREE-WALKER (the bytecode trigger
	// lowering takes all-AFTER bodies only), so this is the same rules again
	// through the other write executor -- including a SET right-hand side and a
	// WHERE that call the functions, which the mined corpus is full of
	// ("update t2 set val3=1000+last_insert_rowid()").
	{"tree-walked-trigger-body-set-and-where", []string{
		`CREATE TABLE t0(x)`,
		`INSERT INTO t0 VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE t1(k INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2(k INTEGER PRIMARY KEY, v2, v3)`,
		`INSERT INTO t2 VALUES(1,0,0),(2,0,0)`,
		`CREATE TRIGGER b1 BEFORE INSERT ON t1 FOR EACH ROW BEGIN UPDATE t0 SET x=x; UPDATE t2 SET v2=changes(), v3=1000+last_insert_rowid(); DELETE FROM t2 WHERE k=changes(); END`,
		`DELETE FROM t0 WHERE x=99`,
		`INSERT INTO t1 VALUES(7)`,
		`SELECT k,v2,v3 FROM t2 ORDER BY k`,
		`SELECT count(*) FROM t2`,
	}},
	{"before-trigger-reads-the-pre-statement-value", []string{
		`CREATE TABLE t0(x)`,
		`INSERT INTO t0 VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE t3(k)`,
		`CREATE TABLE seen3(v)`,
		`CREATE TRIGGER r3 BEFORE INSERT ON t3 FOR EACH ROW BEGIN INSERT INTO seen3 VALUES(changes()); END`,
		`UPDATE t0 SET x=x`,
		`INSERT INTO t3 VALUES(1)`,
		`SELECT v FROM seen3`,
	}},
	// Nested frames: t1_i's step 1 fires t2_i twice, and each of those reads
	// only its OWN sub-program's count.
	{"nested-trigger-frames", []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE TABLE t2(a,b)`,
		`CREATE TABLE t3(a,b)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER t1_i BEFORE INSERT ON t1 BEGIN INSERT INTO t2 VALUES(new.a,new.b),(new.a,new.b); INSERT INTO log VALUES('t2->' || changes()); END`,
		`CREATE TRIGGER t2_i AFTER INSERT ON t2 BEGIN INSERT INTO t3 VALUES(new.a,new.b),(new.a,new.b),(new.a,new.b); INSERT INTO log VALUES('t3->' || changes()); END`,
		`INSERT INTO t1 VALUES(1,2)`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT count(*) FROM t2`,
		`SELECT count(*) FROM t3`,
	}},
	// last_insert_rowid() inside a trigger body is the body's own insert; the
	// instant the body ends it reverts to the triggering statement's.
	{"trigger-last-insert-rowid-is-save-restore", []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY, y)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y)`,
		`CREATE TABLE seen(v)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO t2 VALUES(500,1); INSERT INTO seen VALUES(last_insert_rowid()); END`,
		`INSERT INTO t1 VALUES(2,'a')`,
		`SELECT v FROM seen`,
	}},
	// A trigger's frame RESTORES changes() on the way out, so a SECOND trigger
	// on the same statement reads the value from before the FIRST one fired --
	// not that trigger's last body step (5) and not zero. SQLite fires the most
	// recently CREATED trigger first, so d2 (the writer) runs before d1 (the
	// reader). See enterTriggerFrame (engine/conn_state.go): this is the
	// three-way test that tells restore, leak and reset apart.
	{"a-triggers-frame-restores-changes-for-the-next-trigger", []string{
		`CREATE TABLE t0(x)`,
		`INSERT INTO t0 VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE t1(k)`,
		`CREATE TABLE seen(v)`,
		`CREATE TRIGGER d1 AFTER INSERT ON t1 BEGIN INSERT INTO seen VALUES(changes()); END`,
		`CREATE TRIGGER d2 AFTER INSERT ON t1 BEGIN UPDATE t0 SET x=x; END`,
		`UPDATE t0 SET x=x WHERE x<3`,
		`INSERT INTO t1 VALUES(1)`,
		`SELECT v FROM seen`,
	}},
	// The reverse firing order, which also pins the order itself: c2 (created
	// last) fires FIRST and reads the pre-statement 2, then c1's own second
	// body step reads c1's own first step's 5.
	{"trigger-firing-order-and-per-body-step-reads", []string{
		`CREATE TABLE t0(x)`,
		`INSERT INTO t0 VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE t1(k)`,
		`CREATE TABLE seen(v)`,
		`UPDATE t0 SET x=x WHERE x<3`,
		`CREATE TRIGGER c1 AFTER INSERT ON t1 BEGIN UPDATE t0 SET x=x; INSERT INTO seen VALUES(changes()); END`,
		`CREATE TRIGGER c2 AFTER INSERT ON t1 BEGIN INSERT INTO seen VALUES(changes()); END`,
		`INSERT INTO t1 VALUES(1)`,
		`SELECT v FROM seen ORDER BY rowid`,
	}},
	// A trigger whose WHEN is false leaves changes() exactly where it was: its
	// frame is still entered (C SQLite codes the WHEN test inside the
	// sub-program) and a frame halt is a restore. "never" fires FIRST here.
	{"trigger-with-a-false-when-changes-nothing", []string{
		`CREATE TABLE t0(x)`,
		`INSERT INTO t0 VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE t1(k)`,
		`CREATE TABLE seen(v)`,
		`CREATE TRIGGER after2 AFTER INSERT ON t1 BEGIN INSERT INTO seen VALUES(changes()); END`,
		`CREATE TRIGGER never AFTER INSERT ON t1 WHEN 0 BEGIN INSERT INTO t0 VALUES(99); END`,
		`INSERT INTO t1 VALUES(1)`,
		`SELECT v FROM seen`,
	}},
	// An INSTEAD OF trigger's own view INSERT counts 0 and leaves
	// last_insert_rowid() alone, while the body's rows still reach
	// total_changes().
	{"instead-of-trigger", []string{
		`CREATE TABLE t0(x)`,
		`INSERT INTO t0 VALUES(1),(2),(3)`,
		`CREATE TABLE t1(k)`,
		`CREATE TABLE n1(a,b)`,
		`CREATE VIEW v1 AS SELECT * FROM t1`,
		`CREATE TRIGGER r1 INSTEAD OF INSERT ON v1 FOR EACH ROW BEGIN INSERT INTO n1 VALUES(NULL, changes()); UPDATE t0 SET x=x*10; END`,
		`DELETE FROM t0 WHERE x=99`,
		`INSERT INTO v1 VALUES(1)`,
		`SELECT a,b FROM n1`,
	}},
	// ON DELETE CASCADE / SET NULL / ON UPDATE CASCADE child rows behave
	// exactly like trigger rows: out of changes(), into total_changes().
	{"foreign-key-actions", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p ON DELETE CASCADE)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES(1),(1),(2)`,
		`DELETE FROM p WHERE id=1`,
		`SELECT count(*) FROM c`,
	}},
	{"foreign-key-set-null-and-on-update", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cn(y REFERENCES p ON DELETE SET NULL)`,
		`CREATE TABLE cu(y REFERENCES p ON UPDATE CASCADE)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO cn VALUES(1),(2)`,
		`INSERT INTO cu VALUES(1),(2)`,
		`DELETE FROM p WHERE id=1`,
		`UPDATE p SET id=7 WHERE id=2`,
	}},
	// A statement that never STARTED (no such table/column, a syntax error)
	// leaves both counters alone. One that started and was undone publishes 0
	// -- but keeps the rowid of the row it undid. OR FAIL keeps its rows AND
	// counts them.
	{"failed-statements", []string{
		`CREATE TABLE u(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`INSERT INTO u VALUES(1,'x')`,
		`INSERT INTO u VALUES(2,'y'),(3,'x')`,
		`SELECT count(*) FROM u`,
		`INSERT INTO u VALUES(9,'k')`,
		`INSERT INTO u VALUES(9,'k2')`,
		`INSERT INTO nosuchtable VALUES(1)`,
		`UPDATE u SET nosuchcol=1`,
		`DELETE FROM u WHERE nosuchcol=1`,
		`INSERT OR FAIL INTO u VALUES(20,'y'),(21,'x'),(22,'w')`,
		`SELECT count(*) FROM u`,
		`DELETE FROM u WHERE a>100`,
	}},
	// The same abort rules again, but through statement shapes the write
	// codegen declines (an INSERT ... SELECT source, a table carrying a
	// trigger), so the tree-walking insert path's own rollback is exercised
	// too -- it has its own undo and used to put last_insert_rowid back.
	{"failed-statements-on-the-tree-walking-insert-path", []string{
		`CREATE TABLE u(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`INSERT INTO u VALUES(1,'x')`,
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(2,'y'),(3,'x')`,
		`INSERT INTO u SELECT a,b FROM src`,
		`SELECT count(*) FROM u`,
		`CREATE TABLE tg(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tgr BEFORE INSERT ON tg BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO tg VALUES(1,'x')`,
		`INSERT INTO tg VALUES(2,'y'),(3,'x')`,
		`SELECT count(*) FROM tg`,
		`SELECT count(*) FROM log`,
	}},
	{"raise-ignore-and-raise-abort", []string{
		`CREATE TABLE u(a)`,
		`CREATE TRIGGER ur BEFORE INSERT ON u WHEN new.a=2 BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO u VALUES(1),(2),(3)`,
		`SELECT count(*) FROM u`,
		`CREATE TABLE w(a)`,
		`CREATE TRIGGER wr BEFORE INSERT ON w WHEN new.a=2 BEGIN SELECT RAISE(ABORT,'no'); END`,
		`INSERT INTO w VALUES(1),(2),(3)`,
		`SELECT count(*) FROM w`,
	}},
	// None of the three is transactional: ROLLBACK, ROLLBACK TO and RELEASE
	// leave all of them exactly where the rolled-back statements put them.
	{"transactions-revert-none-of-it", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY)`,
		`INSERT INTO t VALUES(5)`,
		`BEGIN`,
		`INSERT INTO t VALUES(7)`,
		`ROLLBACK`,
		`SELECT count(*) FROM t`,
		`SAVEPOINT s1`,
		`INSERT INTO t VALUES(8)`,
		`ROLLBACK TO s1`,
		`RELEASE s1`,
		`SELECT count(*) FROM t`,
		`BEGIN`,
		`INSERT INTO t VALUES(11)`,
		`COMMIT`,
	}},
	// A WITHOUT ROWID insert never moves last_insert_rowid, but its UPDATE and
	// DELETE count normally.
	{"without-rowid", []string{
		`CREATE TABLE r(a INTEGER PRIMARY KEY)`,
		`INSERT INTO r VALUES(42)`,
		`CREATE TABLE w(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`INSERT INTO w VALUES('k',1)`,
		`INSERT INTO w VALUES('k2',2)`,
		`UPDATE w SET b=3`,
		`DELETE FROM w`,
		`CREATE TABLE s(a INTEGER PRIMARY KEY)`,
		`INSERT INTO s VALUES(7)`,
	}},
	// Read MID-statement: last_insert_rowid() advances per row inside a
	// multi-row VALUES, while changes() stays on the previous statement's
	// count for the whole of this one.
	{"mid-statement-reads", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(10,'seed')`,
		`INSERT INTO t(b) VALUES(last_insert_rowid()),(last_insert_rowid()),(last_insert_rowid())`,
		`SELECT a,b FROM t ORDER BY a`,
		`CREATE TABLE u(a INTEGER PRIMARY KEY, b)`,
		`DELETE FROM t WHERE a=999`,
		`INSERT INTO u(b) VALUES(changes()),(changes())`,
		`SELECT a,b FROM u ORDER BY a`,
		`UPDATE u SET b=changes()`,
		`SELECT a,b FROM u ORDER BY a`,
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(2),(3)`,
		`INSERT INTO t(b) SELECT a FROM src`,
		`CREATE TABLE e(a)`,
		`INSERT INTO t(b) SELECT a FROM e`,
		`INSERT INTO t(b) SELECT changes()`,
		`SELECT b FROM t ORDER BY a`,
	}},
	{"returning", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x') RETURNING a`,
		`UPDATE t SET b='y' RETURNING b`,
		`DELETE FROM t RETURNING a`,
	}},
	// The same statements run through the RETURNING RESULT-SET path
	// (ExecReturningArgs, which a driver reaches with Query rather than Exec)
	// -- a second write executor as far as the counters are concerned.
	{"returning-as-a-query", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`SELECT * FROM (SELECT 1) WHERE 0`, // no-op; keeps the counters at zero
		`INSERT INTO t VALUES(1,'x'),(2,'y') RETURNING a`,
		`UPDATE t SET b='z' RETURNING b`,
		`DELETE FROM t WHERE a=1 RETURNING a`,
		`SELECT count(*) FROM t`,
	}},
	{"delete-everything-and-drop", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3),(4)`,
		`DELETE FROM t`,
		`INSERT INTO t VALUES(1),(2)`,
		`DELETE FROM t WHERE 1`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`DROP TABLE t`,
	}},
	// The functions themselves, as plain FROM-less selects and inside other
	// expressions -- the shape the mined TCL corpus is full of.
	{"the-functions-as-fromless-selects", []string{
		`SELECT changes()`,
		`SELECT total_changes()`,
		`SELECT last_insert_rowid()`,
		`SELECT sqlite_version()`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY)`,
		`INSERT INTO t VALUES(4),(5)`,
		`SELECT changes(), total_changes()`,
		`SELECT printf('(%d)',changes())`,
		`SELECT last_insert_rowid()+1, changes()*10`,
		`SELECT typeof(changes()), typeof(total_changes()), typeof(last_insert_rowid()), typeof(sqlite_version())`,
		`SELECT CASE WHEN changes()=2 THEN 'two' ELSE 'other' END`,
		`SELECT a FROM t WHERE a=last_insert_rowid()`,
		`SELECT a, changes() FROM t ORDER BY a`,
	}},
}

// TestConnectionStateFuncs replays each script against engine.DB and real C
// SQLite in lockstep, comparing all three counters after EVERY statement as
// well as each query's own rows.
func TestConnectionStateFuncs(t *testing.T) {
	for _, tc := range connStateCases {
		t.Run(tc.name, func(t *testing.T) {
			godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer cgodb.Close()
			cgodb.SetMaxOpenConns(1) // one logical connection: the counters ARE the connection

			for i, stmt := range tc.stmts {
				if tc.name == "returning-as-a-query" && engine.StatementHasReturning(stmt) {
					// Deliberately the RESULT-SET entry point, not ExecArgs:
					// it is a separate write executor (execReturningViaVM) and
					// has to publish the counters identically.
					if _, _, rerr := godb.ExecReturningArgs(stmt, nil); rerr != nil {
						t.Fatalf("stmt #%d %q: ExecReturningArgs: %v", i, stmt, rerr)
					}
					if _, cerr := cgodb.Exec(stmt); cerr != nil {
						t.Fatalf("stmt #%d %q: cgo: %v", i, stmt, cerr)
					}
					assertConnCountersMatch(t, godb, cgodb, i, stmt)
					continue
				}
				if tclIsQuery(stmt) {
					goCols, goRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
					if panicked {
						t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
					}
					cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, stmt)
					if (qerr != nil) != (cerr != nil) {
						t.Fatalf("stmt #%d %q: query error disagreement\n  go:  %v\n  cgo: %v", i, stmt, qerr, cerr)
					}
					if qerr == nil {
						if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
							t.Fatalf("stmt #%d %q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v",
								i, stmt, reason, goCols, goRows, cgoCols, cgoRows)
						}
					}
				} else {
					execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt)
					if panicked {
						t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
					}
					_, cerr := cgodb.Exec(stmt)
					if (execErr != nil) != (cerr != nil) {
						t.Fatalf("stmt #%d %q: exec error disagreement\n  go:  %v\n  cgo: %v", i, stmt, execErr, cerr)
					}
				}
				// The point of the whole file: after EVERY statement, whatever
				// it was and however it went, both connections must report the
				// same three counters.
				assertConnCountersMatch(t, godb, cgodb, i, stmt)
			}
		})
	}
}

// assertConnCountersMatch asks both engines for changes()/total_changes()/
// last_insert_rowid() in one statement and compares all three.
func assertConnCountersMatch(t *testing.T, godb *engine.Session, cgodb *sql.DB, i int, after string) {
	t.Helper()
	goCols, goRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, connStateProbe)
	if panicked {
		t.Fatalf("after stmt #%d %q: %s PANICKED: %v", i, after, connStateProbe, panicVal)
	}
	if qerr != nil {
		t.Fatalf("after stmt #%d %q: %s failed on the engine: %v", i, after, connStateProbe, qerr)
	}
	cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, connStateProbe)
	if cerr != nil {
		t.Fatalf("after stmt #%d %q: %s failed on cgo: %v", i, after, connStateProbe, cerr)
	}
	if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
		t.Fatalf("after stmt #%d %q: connection counters diverge (%s)\n  go:  %v\n  cgo: %v",
			i, after, reason, goRows, cgoRows)
	}
}

// TestSQLiteVersionMatchesTheOracle is the loud failure engine/conn_state.go's
// SQLiteVersion promises. sqlite_version() is a build-time constant in a pure
// Go engine, so the only thing keeping it honest is this comparison against
// the linked C library the whole harness uses as its oracle: if that library
// ever moves, this fails by name and the constant (plus every "verified
// against 3.53.3" comment in engine/) has to be revisited deliberately.
func TestSQLiteVersionMatchesTheOracle(t *testing.T) {
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	var oracle string
	if err := cgodb.QueryRow(`SELECT sqlite_version()`).Scan(&oracle); err != nil {
		t.Fatalf("oracle SELECT sqlite_version(): %v", err)
	}
	if oracle != engine.SQLiteVersion {
		t.Fatalf("the oracle's SQLite version moved: linked library reports %q, "+
			"engine.SQLiteVersion is %q.\n"+
			"sqlite_version() is a build-time constant in this engine, so it can only "+
			"stay byte-exact by being updated here deliberately -- and every "+
			"\"verified against %s\" rule in engine/ was probed against the OLD library, "+
			"so re-derive before simply bumping the constant.",
			oracle, engine.SQLiteVersion, engine.SQLiteVersion)
	}
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	_, rows, qerr, panicked, panicVal := tclSafeGoQuery(godb, `SELECT sqlite_version()`)
	if panicked {
		t.Fatalf("SELECT sqlite_version() PANICKED: %v", panicVal)
	}
	if qerr != nil {
		t.Fatalf("SELECT sqlite_version(): %v", qerr)
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != "T:"+oracle {
		t.Fatalf("SELECT sqlite_version() = %v, want one row of %q", rows, "T:"+oracle)
	}
}

// TestConnectionStateArity pins the arity rejection: C SQLite raises
// "wrong number of arguments to function X()" at PREPARE time for all four,
// so this engine must ERROR rather than ignore the argument.
func TestConnectionStateArity(t *testing.T) {
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	for _, name := range []string{"sqlite_version", "changes", "total_changes", "last_insert_rowid"} {
		stmt := fmt.Sprintf("SELECT %s(1)", name)
		_, _, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
		if panicked {
			t.Fatalf("%s PANICKED: %v", stmt, panicVal)
		}
		if qerr == nil {
			t.Fatalf("%s: expected an error, got success", stmt)
		}
		if !strings.Contains(qerr.Error(), "wrong number of arguments") {
			t.Fatalf("%s: error %q does not say \"wrong number of arguments\"", stmt, qerr)
		}
		if _, cerr := tclRunCGOQueryErr(cgodb, stmt); cerr == nil {
			t.Fatalf("%s: the oracle ACCEPTED this -- the rule this test pins has changed", stmt)
		}
	}
}

// tclRunCGOQueryErr is tclRunCGOQuery reduced to "did it error", for the arity
// check above.
func tclRunCGOQueryErr(cgodb *sql.DB, stmt string) ([][]string, error) {
	_, rows, err := tclRunCGOQuery(cgodb, stmt)
	return rows, err
}

// TestConnectionStateVirtualTableDecline pins the deliberate gap a
// virtual-table write leaves: C SQLite's own counters then carry the
// module's shadow-table writes, so total_changes() is unanswerable for the rest
// of the session (engine/conn_state.go's markConnStateOpaque).
//
// last_insert_rowid() USED to go with it and no longer does. An fts3/fts4 row
// INSERT ends in an OP_VUpdate that overwrites db->lastRowid with the docid the
// module reported, so that counter is exactly reproducible whatever the shadow
// SQL did on the way (engine/conn_state.go's markVtabInsertRowid, a general
// OP_VUpdate rule fts3/fts4 no longer has exclusively -- see
// vtab_lastrowid_conflict_test.go/fts5_lastrowid_conflict_test.go for
// rtree/fts5's own row INSERTs) -- and this test now pins the ANSWER. It
// stays declined after the fts3/fts4 DELETE and UPDATE below, whose
// values really are module internals: over a session reading 14, an fts3 DELETE
// leaves it at 14, an fts4 DELETE reads 0 (its %_stat row has rowid 0 and is
// rewritten last), an fts3 UPDATE reads the updated docid and an fts4 UPDATE
// reads 0. compat-harness/fts3_last_insert_rowid_test.go is the differential
// half; this one is engine-direct, because a fix in one has repeatedly not been
// a fix in the other.
//
// changes() was never affected: it counts only the rows the statement reported,
// which DOES match the oracle across fts3/fts5/rtree.
func TestConnectionStateVirtualTableDecline(t *testing.T) {
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	for _, s := range []string{
		`CREATE VIRTUAL TABLE ft USING fts3(c)`,
		`INSERT INTO ft(c) VALUES('hello')`,
	} {
		if execErr, panicked, panicVal := tclSafeExecArgs(godb, s); panicked {
			t.Fatalf("%q PANICKED: %v", s, panicVal)
		} else if execErr != nil {
			t.Fatalf("%q: %v", s, execErr)
		}
	}
	{
		s := `SELECT total_changes()`
		_, _, qerr, panicked, panicVal := tclSafeGoQuery(godb, s)
		if panicked {
			t.Fatalf("%q PANICKED: %v", s, panicVal)
		}
		if qerr == nil {
			t.Fatalf("%q: expected a decline after a virtual-table write, got success", s)
		}
		if !strings.Contains(qerr.Error(), "not reproducible") {
			t.Fatalf("%q: decline text %q does not explain itself", s, qerr)
		}
	}
	// last_insert_rowid() and changes() are both answered, and both right: the
	// fts3 insert stored one row at docid 1 and C SQLite reports 1 and 1.
	for _, tc := range []struct{ stmt, want string }{
		{`SELECT last_insert_rowid()`, "I:1"},
		{`SELECT changes()`, "I:1"},
	} {
		cols, rows, qerr, panicked, panicVal := tclSafeGoQuery(godb, tc.stmt)
		if panicked {
			t.Fatalf("%s PANICKED: %v", tc.stmt, panicVal)
		}
		if qerr != nil {
			t.Fatalf("%s after a virtual-table write: %v", tc.stmt, qerr)
		}
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != tc.want {
			t.Fatalf("%s = %v (cols %v), want one row of %s", tc.stmt, rows, cols, tc.want)
		}
	}

	// The shape the mined corpus actually uses (fts3e.test): the function is
	// read from inside the VALUES of an fts3 INSERT. That path used to build its
	// parameter scope with neither a session nor a snapshot in reach, so it
	// declined -- and before THAT it answered 0, stored docid 0, and took eleven
	// later statements of the file down with it, which is why
	// connStateSource.known exists. It now reads the live session, so this
	// stores docid 2 and the counter follows it.
	stmt := `INSERT INTO ft(docid, c) VALUES(last_insert_rowid() + 1, 'again')`
	if execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt); panicked {
		t.Fatalf("%q PANICKED: %v", stmt, panicVal)
	} else if execErr != nil {
		t.Fatalf("%q: %v", stmt, execErr)
	}
	cols, rows, qerr, panicked, panicVal := tclSafeGoQuery(godb, `SELECT docid FROM ft ORDER BY docid`)
	if panicked {
		t.Fatalf("SELECT docid PANICKED: %v", panicVal)
	}
	if qerr != nil {
		t.Fatalf("SELECT docid after the self-referential insert: %v", qerr)
	}
	if len(rows) != 2 || rows[0][0] != "I:1" || rows[1][0] != "I:2" {
		t.Fatalf("SELECT docid FROM ft = %v (cols %v), want I:1 then I:2", rows, cols)
	}

	// ...and a DELETE puts last_insert_rowid() back out of reach for good: real
	// fts3 leaves whatever its own shadow SQL last stored there, which this
	// engine's staging never wrote. total_changes() was already gone.
	del := `DELETE FROM ft WHERE docid=1`
	if execErr, panicked, panicVal := tclSafeExecArgs(godb, del); panicked {
		t.Fatalf("%q PANICKED: %v", del, panicVal)
	} else if execErr != nil {
		t.Fatalf("%q failed -- the assertion below would pass vacuously: %v", del, execErr)
	}
	for _, s := range []string{`SELECT last_insert_rowid()`, `SELECT total_changes()`} {
		_, _, qerr, panicked, panicVal := tclSafeGoQuery(godb, s)
		if panicked {
			t.Fatalf("%q PANICKED: %v", s, panicVal)
		}
		if qerr == nil {
			t.Fatalf("%q: expected a decline after an fts3 DELETE, got success", s)
		}
		if !strings.Contains(qerr.Error(), "not reproducible") {
			t.Fatalf("%q: decline text %q does not explain itself", s, qerr)
		}
	}
}

// TestConnectionStateThroughDriver is the same three counters read back
// through database/sql, where every autocommit statement runs on its OWN
// throwaway engine session and reads run against a pager with no session at
// all -- so this is really a test of driver's carry-across bridge
// (Conn.noteConnState / Conn.stampConnState). It is separate from the
// engine-direct cases above because a fix in one has repeatedly not been a fix
// in the other.
func TestConnectionStateThroughDriver(t *testing.T) {
	godb, err := sql.Open(driver.DriverName, filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	defer godb.Close()
	godb.SetMaxOpenConns(1)
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	script := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(2,'y'),(3,'z')`,
		`UPDATE t SET b='q' WHERE a=1`,
		`UPDATE t SET b='q2' WHERE a=999`,
		`CREATE TABLE u(x)`,
		`DELETE FROM t WHERE a=3`,
		`INSERT OR IGNORE INTO t VALUES(1,'zzz')`,
		`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO w VALUES('a',1)`,
		`BEGIN`,
		`INSERT INTO t VALUES(50,'m')`,
		`ROLLBACK`,
	}
	for i, s := range script {
		_, gerr := godb.Exec(s)
		_, cerr := cgodb.Exec(s)
		if (gerr != nil) != (cerr != nil) {
			t.Fatalf("stmt #%d %q: exec error disagreement\n  go:  %v\n  cgo: %v", i, s, gerr, cerr)
		}
		var gch, gtot, gli int64
		if err := godb.QueryRow(connStateProbe).Scan(&gch, &gtot, &gli); err != nil {
			t.Fatalf("after stmt #%d %q: driver %s: %v", i, s, connStateProbe, err)
		}
		var cch, ctot, cli int64
		if err := cgodb.QueryRow(connStateProbe).Scan(&cch, &ctot, &cli); err != nil {
			t.Fatalf("after stmt #%d %q: cgo %s: %v", i, s, connStateProbe, err)
		}
		if gch != cch || gtot != ctot || gli != cli {
			t.Fatalf("after stmt #%d %q: driver counters diverge\n  go:  changes=%d total=%d rowid=%d\n  cgo: changes=%d total=%d rowid=%d",
				i, s, gch, gtot, gli, cch, ctot, cli)
		}
	}
}
