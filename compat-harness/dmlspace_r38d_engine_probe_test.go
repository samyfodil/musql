package compat

// r38d ENGINE-LEVEL PROBE — tests connection-state counters and constraints
// by calling the engine directly, bypassing the driver to isolate engine bugs
// from driver bugs.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// r38dEngineState executes statements and returns the connection state counters.
func r38dEngineState(t *testing.T, stmts []string) (changes, total, lastID int64) {
	t.Helper()
	db, err := engine.Create(t.TempDir()+"/r38d.sqlite")
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()
	for _, s := range stmts {
		if engine.StatementHasReturning(s) {
			if _, _, err := db.ExecReturningArgs(s, nil); err != nil {
				t.Logf("  (%s -> %v)", s, err)
			}
			continue
		}
		if _, _, err := db.ExecArgs(s, nil); err != nil {
			t.Logf("  (%s -> %v)", s, err)
		}
	}
	changes, total, lastID, _, _ = db.ConnState()
	return
}

func TestR38DEngineReturningCounters(t *testing.T) {
	base := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c)`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`,
	}
	trig := `CREATE TRIGGER g1 AFTER INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('ai',new.a,new.b); END`

	cases := []struct {
		name                   string
		stmts                  []string
		changes, total, lastID int64 // what C SQLite reports
		// open records that this case does NOT yet match the oracle, and what
		// the engine says instead. It is a pin, not an expectation: the case
		// fails if the engine moves OFF that value in either direction —
		// including onto the oracle's, which is the signal to delete the pin
		// and let the oracle assertion above stand on its own.
		open        bool
		openChanges int64
		openTotal   int64
		openLastID  int64
	}{
		// Control: no trigger. The RETURNING path already gets this right, which
		// is what makes the trigger cases below a TRIGGER question rather than a
		// RETURNING one.
		{"insert-returning-no-trigger",
			append(append([]string{}, base...), `INSERT INTO t(a,b,c) VALUES(4,'q',40) RETURNING *`),
			1, 4, 4,
			false, 0, 0, 0},
		// Same statement, one AFTER INSERT trigger. C SQLite: changes() is
		// the OUTER statement's own 1 row, and total_changes() counts the
		// trigger body's insert as well -> 5. This engine publishes NEITHER:
		// changes() kept the pre-statement 0 and total_changes() missed the
		// trigger body's row. The statement itself SUCCEEDED and returned the
		// right RETURNING row, so nothing but a counter readback saw it --
		// which is why the DML shape space needed a readback axis to find it.
		// CLOSED: execReturningStatement now returns its count (returning_write.go),
		// so the pin is gone and the oracle assertion stands alone.
		{"insert-returning-with-trigger",
			append(append([]string{}, base...), trig, `INSERT INTO t(a,b,c) VALUES(4,'q',40) RETURNING *`),
			1, 5, 4,
			false, 0, 0, 0},
		// The same statement WITHOUT RETURNING, as the discriminator: if this
		// one is right and the one above is wrong, the RETURNING path is where
		// the counters are lost.
		{"insert-plain-with-trigger",
			append(append([]string{}, base...), trig, `INSERT INTO t(a,b,c) VALUES(4,'q',40)`),
			1, 5, 4,
			false, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch, tot, id := r38dEngineState(t, c.stmts)
			if ch == c.changes && tot == c.total && id == c.lastID {
				if c.open {
					t.Errorf("this case now MATCHES C SQLite (changes=%d total=%d lastID=%d).\n"+
						"Delete its open/openChanges/openTotal/openLastID fields so the oracle\n"+
						"assertion below is the only thing holding it.", ch, tot, id)
				}
				return
			}
			if c.open && ch == c.openChanges && tot == c.openTotal && id == c.openLastID {
				t.Logf("KNOWN OPEN: engine ConnState = (changes=%d total=%d lastID=%d), C SQLite reports (%d,%d,%d)",
					ch, tot, id, c.changes, c.total, c.lastID)
				return
			}
			t.Errorf("engine ConnState = (changes=%d total=%d lastID=%d), C SQLite reports (%d,%d,%d)",
				ch, tot, id, c.changes, c.total, c.lastID)
		})
	}
}

// TestR38DEngineUpsertForeignKey verifies that foreign key constraints are
// enforced in an upsert's DO UPDATE arm.
func TestR38DEngineUpsertForeignKey(t *testing.T) {
	setup := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10'),(20,'p20'),(30,'p30')`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`,
	}
	cases := []struct {
		name string
		dml  string
		// open marks the shape this engine gets wrong. C SQLite rejects EVERY
		// case here with "FOREIGN KEY constraint failed"; c=100 has no parent row
		// in p. A case that starts erroring fails with an instruction to delete
		// the flag, so closing B3 cannot leave a stale pin behind.
		open bool
	}{
		// Control: the same write spelled as a plain UPDATE.
		{"plain-update", `UPDATE t SET c = 100 WHERE a = 2`, false},
		// Control: the same value arriving as a fresh INSERT.
		{"plain-insert", `INSERT INTO t(a,b,c) VALUES(5,'w',100)`, false},
		// The finding, now CLOSED. The INSERT arm conflicts on a=2 and hands off
		// to DO UPDATE, which used to write c=100 with no FK check at all --
		// sqlite3UpsertDoUpdate codes that arm by calling sqlite3Update()
		// outright (upsert.c:325), so it gets the identical FK enforcement a
		// plain UPDATE does, which is what the plain-update control above pins.
		{"upsert-do-update", `INSERT INTO t(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = 100`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, err := engine.Create(t.TempDir()+"/r38d-fk.sqlite")
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()
			for _, s := range setup {
				if _, _, err := db.ExecArgs(s, nil); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			_, _, err = db.ExecArgs(c.dml, nil)
			if err != nil {
				if c.open {
					t.Errorf("%q now enforces the foreign key (%v), which is what C SQLite does.\n"+
						"Delete this case's open flag so the rejection is asserted outright, and drop\n"+
						"the B3 entries from r38dOpen in dmlspace_r38d_test.go.", c.dml, err)
				}
				return
			}
			if c.open {
				t.Logf("KNOWN OPEN (B3): %q was ACCEPTED; C SQLite rejects it with\n"+
					"    FOREIGN KEY constraint failed. The row now holds c=100 with no parent in p.", c.dml)
				return
			}
			t.Errorf("%q was accepted; C SQLite rejects it with FOREIGN KEY constraint failed", c.dml)
		})
	}
}

// TestR38DEngineFailedStatementCounters verifies that OR ROLLBACK and upsert
// trigger firing order both update connection state counters correctly.
func TestR38DEngineFailedStatementCounters(t *testing.T) {
	base := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c, CHECK(c < 100))`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`,
	}
	logBU := `CREATE TRIGGER g1 BEFORE UPDATE ON t BEGIN INSERT INTO log(e,x,y) VALUES('bu',old.a,new.b); END`
	logAI := `CREATE TRIGGER g1 AFTER INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('ai',new.a,new.b); END`
	upsert := `INSERT OR FAIL INTO t(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = excluded.c + 1`
	cases := []struct {
		name    string
		stmts   []string
		wantErr string // a substring both engines' error for the last statement carries
	}{
		{"or-rollback-in-begin", []string{`BEGIN`, `INSERT OR ROLLBACK INTO t(a,b,c) VALUES(4,'q',40),(2,'y',50)`}, "UNIQUE"},
		{"or-rollback-in-savepoint-trigger", []string{logAI, `SAVEPOINT sp`, `INSERT OR ROLLBACK INTO t(a,b,c) VALUES(5,'r',60),(4,'q',40),(2,'x',50)`}, "UNIQUE"},
		{"or-rollback-autocommit", []string{`INSERT OR ROLLBACK INTO t(a,b,c) VALUES(4,'q',40),(2,'y',50)`}, "UNIQUE"},
		{"upsert-before-update-then-check", []string{logBU, upsert}, "CHECK"},
		{"upsert-before-update-raise", []string{`CREATE TRIGGER g1 BEFORE UPDATE ON t BEGIN SELECT RAISE(ABORT,'trig'); END`, upsert}, "trig"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmts := append(append([]string{}, base...), c.stmts...)
			last := stmts[len(stmts)-1]

			db, err := engine.Create(filepath.Join(t.TempDir(), "m.sqlite"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()
			for _, s := range stmts[:len(stmts)-1] {
				if _, _, err := db.ExecArgs(s, nil); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			_, _, gErr := db.ExecArgs(last, nil)
			gc, gt, gid, _, _ := db.ConnState()

			cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()
			cdb.SetMaxOpenConns(1)
			for _, s := range stmts[:len(stmts)-1] {
				if _, err := cdb.Exec(s); err != nil {
					t.Fatalf("oracle setup %q: %v", s, err)
				}
			}
			_, cErr := cdb.Exec(last)
			var cc, ct, cid int64
			if err := cdb.QueryRow(`SELECT changes(), total_changes(), last_insert_rowid()`).Scan(&cc, &ct, &cid); err != nil {
				t.Fatal(err)
			}

			if cErr == nil || !strings.Contains(cErr.Error(), c.wantErr) {
				t.Fatalf("fixture assumption wrong: oracle's error for %q is %v, wanted it to contain %q", last, cErr, c.wantErr)
			}
			if gErr == nil || !strings.Contains(gErr.Error(), c.wantErr) {
				t.Errorf("engine's error for %q is %v; the oracle's is %v", last, gErr, cErr)
			}
			if gc != cc || gt != ct || gid != cid {
				t.Errorf("engine ConnState = (changes=%d total=%d lastID=%d), C SQLite reports (%d,%d,%d)", gc, gt, gid, cc, ct, cid)
			}
		})
	}
}
