// Tests PRAGMA defer_foreign_keys issued outside a transaction.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// deferFKAutocommitCases each run on one engine.DB and one cgo connection, in
// lockstep, exactly like deferredFKCases (deferred_foreign_keys_test.go).
var deferFKAutocommitCases = []struct {
	name  string
	stmts []string
}{
	// fkey6.test 1.8, verbatim: the setter outside a transaction, then BEGIN
	// (which KEEPS it -- see this engine's doc comment for why), then a DELETE
	// that must be DEFERRED past its own statement. The eventual COMMIT then
	// fails the deferred check, and a failed commit does NOT clear the flag.
	{"fkey6-1.8-setter-then-begin-delete-defers", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
		`CREATE TABLE t3(u INTEGER PRIMARY KEY, v INTEGER REFERENCES t1(x))`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`INSERT INTO t3 VALUES(3,3)`,
		`PRAGMA defer_foreign_keys=ON`,
		`BEGIN`,
		`DELETE FROM t1 WHERE x=3`,
		`PRAGMA defer_foreign_keys`,
		`COMMIT`,
		`PRAGMA defer_foreign_keys`,
		`ROLLBACK`,
		`SELECT count(*) FROM t1`,
	}},
	// The getter and a KEPT pragma (locking_mode) between the setter and BEGIN
	// change nothing -- the flag is still on once the transaction opens, so
	// the DELETE still defers and the COMMIT still fails the check (both
	// sides), leaving the row in place -- ROLLBACK closes it out cleanly.
	{"kept-statements-between-setter-and-begin", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
		`CREATE TABLE t3(u INTEGER PRIMARY KEY, v INTEGER REFERENCES t1(x))`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`INSERT INTO t3 VALUES(3,3)`,
		`PRAGMA defer_foreign_keys=ON`,
		`PRAGMA defer_foreign_keys`,
		`PRAGMA locking_mode`,
		`BEGIN`,
		`DELETE FROM t1 WHERE x=3`,
		`COMMIT`,
		`ROLLBACK`,
		`SELECT count(*) FROM t1`,
	}},
	// An ordinary WRITE to an unrelated table CLEARS the flag (it compiles and
	// runs, hence bIsReader) -- so the RESTRICT this flag would otherwise have
	// disabled fires again, immediately, on the very next statement.
	{"an-intervening-write-clears-then-restrict-fires-immediately", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p2(a PRIMARY KEY, b)`,
		`CREATE TABLE c2(x, y REFERENCES p2 ON DELETE RESTRICT)`,
		`CREATE TABLE zz(q)`,
		`INSERT INTO p2 VALUES(1,'one')`,
		`INSERT INTO c2 VALUES('i',1)`,
		`PRAGMA defer_foreign_keys=ON`,
		`INSERT INTO zz VALUES(1)`, // ordinary DML: CLEARS
		`DELETE FROM p2 WHERE a=1`, // RESTRICT must be enforced again: both sides reject
		`SELECT a,b FROM p2`,
	}},
	// Setting it back to 0 in autocommit (itself a KEPT statement, per the
	// measured table) leaves the flag off for whatever comes next.
	{"setting-it-back-off-in-autocommit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p2(a PRIMARY KEY, b)`,
		`CREATE TABLE c2(x, y REFERENCES p2 ON DELETE RESTRICT)`,
		`INSERT INTO p2 VALUES(1,'one')`,
		`INSERT INTO c2 VALUES('i',1)`,
		`PRAGMA defer_foreign_keys=ON`,
		`PRAGMA defer_foreign_keys=OFF`,
		`DELETE FROM p2 WHERE a=1`, // RESTRICT still armed: both sides reject
	}},
}

// TestDeferForeignKeysAutocommit replays each script against engine.DB and
// C SQLite in lockstep, exactly like TestDeferredForeignKeys.
func TestDeferForeignKeysAutocommit(t *testing.T) {
	for _, tc := range deferFKAutocommitCases {
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
			cgodb.SetMaxOpenConns(1) // one logical connection: BEGIN must span statements

			for i, stmt := range tc.stmts {
				if tclIsQuery(stmt) {
					goCols, goRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
					if panicked {
						t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
					}
					cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, stmt)
					if (qerr != nil) != (cerr != nil) {
						t.Fatalf("stmt #%d %q: query error disagreement\n  go:  %v\n  cgo: %v", i, stmt, qerr, cerr)
					}
					if qerr != nil {
						continue
					}
					if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
						t.Fatalf("stmt #%d %q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v",
							i, stmt, reason, goCols, goRows, cgoCols, cgoRows)
					}
					continue
				}
				execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt)
				if panicked {
					t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
				}
				_, cerr := cgodb.Exec(stmt)
				if (execErr != nil) != (cerr != nil) {
					t.Fatalf("stmt #%d %q: exec error disagreement\n  go:  %v\n  cgo: %v", i, stmt, execErr, cerr)
				}
			}
		})
	}
}

// TestDeferForeignKeysAutocommitUnknownShapeDeclines is the excluded class:
// every shape deferFKAutocommitClearOf (and its read-side twin) deliberately
// leaves Unknown, proven to still decline cleanly -- errVDBEUnsupported, never
// a silently wrong flag state. Engine-direct only: these never reach a
// held transaction, so driver's own throwaway-session guard is a
// different file's concern (TestDeferForeignKeysAutocommitDeclinedThroughDriver,
// below).
func TestDeferForeignKeysAutocommitUnknownShapeDeclines(t *testing.T) {
	newSetDB := func(t *testing.T) *engine.Session {
		t.Helper()
		godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		t.Cleanup(func() { godb.Discard() })
		for _, s := range []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
			`CREATE TABLE t3(u INTEGER PRIMARY KEY, v INTEGER REFERENCES t1(x))`,
			`CREATE INDEX t3v ON t3(v)`,
			`INSERT INTO t1 VALUES(1),(2),(3)`,
			`INSERT INTO t3 VALUES(3,3)`,
			`PRAGMA defer_foreign_keys=ON`,
		} {
			if _, _, err := godb.ExecArgs(s, nil); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		return godb
	}

	t.Run("reindex-is-unclassified", func(t *testing.T) {
		godb := newSetDB(t)
		if _, _, err := godb.ExecArgs(`REINDEX`, nil); err == nil {
			t.Fatal("expected a decline (REINDEX's clear/keep depends on whether there is an index to rebuild), got success")
		} else if !strings.Contains(err.Error(), "outside the table") {
			t.Fatalf("wrong decline reason: %v", err)
		}
		// The flag itself must be UNTOUCHED by the declined statement -- still
		// on, exactly as it was, ready for the transaction that actually
		// consumes it.
		if !godb.DeferForeignKeys() {
			t.Fatal("a DECLINED statement must not have changed the flag")
		}
	})

	t.Run("a-cross-database-write-is-unclassified", func(t *testing.T) {
		godb := newSetDB(t)
		aux := filepath.Join(t.TempDir(), "aux.db")
		if _, _, err := godb.ExecArgs(`ATTACH '`+aux+`' AS aux`, nil); err != nil {
			t.Fatalf("ATTACH: %v", err)
		}
		if _, _, err := godb.ExecArgs(`CREATE TABLE aux.z(a)`, nil); err != nil {
			t.Fatalf("CREATE TABLE aux.z: %v", err)
		}
		// Re-set the flag: ATTACH itself is a measured CLEARED statement (see
		// the classifier's own verb table), so it already turned this off --
		// exactly the general decay rule TestDeferForeignKeysAutocommit's own
		// "an-intervening-write-clears" case pins directly.
		if !godb.DeferForeignKeys() {
			godb.SetDeferForeignKeys(true)
		}
		if _, _, err := godb.ExecArgs(`INSERT INTO aux.z VALUES(1)`, nil); err == nil {
			t.Fatal("expected a decline (a write into an ATTACHed database is delegated to ITS OWN session, whose classification this file cannot see), got success")
		} else if !strings.Contains(err.Error(), "outside the table") {
			t.Fatalf("wrong decline reason: %v", err)
		}
	})

	t.Run("a-read-with-a-cte-is-unclassified", func(t *testing.T) {
		godb := newSetDB(t)
		p, err := godb.SnapshotPager()
		if err != nil {
			t.Fatalf("SnapshotPager: %v", err)
		}
		if _, _, err := p.QueryArgs(`WITH q AS (SELECT 1) SELECT * FROM q, t1`, nil); err == nil {
			t.Fatal("expected a decline (the read classifier declines any CTE), got success")
		} else if !strings.Contains(err.Error(), "outside the shapes") {
			t.Fatalf("wrong decline reason: %v", err)
		}
		if !godb.DeferForeignKeys() {
			t.Fatal("a DECLINED read must not have changed the flag")
		}
	})

	// The reads C classifies by whether their program coded OP_Transaction
	// (vdbeaux.c:3434): a FROM-less "SELECT 1" did not and KEEPS the flag, a
	// table read did and CLEARS it, and a read that fails to PREPARE never had
	// a program and KEEPS it (pragma_r25_defer_fk_table_test.go measures all
	// three). These used to decline; each is its own fresh database, so no
	// earlier read can have cleared the flag already.
	for _, c := range []struct {
		sql    string
		fails  bool
		clears bool
	}{
		{`SELECT 1`, false, false},
		{`SELECT * FROM t1`, false, true},
		{`SELECT nosuchcolumn FROM t1`, true, false},
	} {
		t.Run(c.sql, func(t *testing.T) {
			godb := newSetDB(t)
			p, err := godb.SnapshotPager()
			if err != nil {
				t.Fatalf("SnapshotPager: %v", err)
			}
			_, _, qerr := p.QueryArgs(c.sql, nil)
			if (qerr != nil) != c.fails {
				t.Fatalf("error = %v, want failure=%v", qerr, c.fails)
			}
			if got := !godb.DeferForeignKeys(); got != c.clears {
				t.Fatalf("flag cleared = %v, want %v", got, c.clears)
			}
		})
	}
}

// TestDeferForeignKeysHeldTransactionStillDeclines pins the ONE narrower gap
// left after this bucket's acceptance: a transaction a DRIVER holds open with
// no engine-level BEGIN underneath (db.heldTransaction, driver's own
// Conn.tx model) has no engine-level COMMIT left to run the deferred check
// against -- fkDeferredUnmodelled's own gap (engine/fk.go), unchanged by this
// bucket.
func TestDeferForeignKeysHeldTransactionStillDeclines(t *testing.T) {
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	godb.MarkHeldTransaction()
	_, _, err = godb.ExecArgs(`PRAGMA defer_foreign_keys=ON`, nil)
	if err == nil {
		t.Fatal("expected a decline inside a driver-held, non-engine-level transaction, got success")
	}
	if !strings.Contains(err.Error(), "driver-held transaction") {
		t.Fatalf("wrong decline reason: %v", err)
	}
}

// TestDeferForeignKeysAutocommitThroughDriver was the gate for driver
// DECLINING the setter outside a held transaction, on the reason that its
// throwaway per-statement session could not carry the flag. It carries it now
// (Conn.deferFKs), so this gates the SERVED behaviour instead: the setter
// succeeds in autocommit, the flag survives to a transaction a LATER statement
// opens, and the connection is unharmed either way. The lifetime itself --
// which statements clear it and which keep it -- is
// TestDeferForeignKeysAutocommitLifetime's table
// (defer_foreign_keys_pragma_test.go), compared against the oracle row by row.
func TestDeferForeignKeysAutocommitThroughDriver(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r41.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
		t.Fatalf("the autocommit setter must be served now: %v", err)
	}
	// It must be READABLE right back -- a bare pragma codes no reader, so real
	// SQLite keeps it (vdbeaux.c:871-910's bIsReader).
	var on int
	if err := db.QueryRow(`PRAGMA defer_foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("getter: %v", err)
	} else if on != 1 {
		t.Fatalf("the flag must still be on right after the setter, got %d", on)
	}
	// ...and the connection is perfectly usable, the flag clearing on the first
	// statement that codes a reader exactly as C's does.
	if _, err := db.Exec(`CREATE TABLE t(a)`); err != nil {
		t.Fatalf("CREATE TABLE after the pragma: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES(1)`); err != nil {
		t.Fatalf("INSERT after the pragma: %v", err)
	}

	// Inside a driver-held "BEGIN" (no engine-level transaction underneath)
	// the setter is SERVED -- a different code path than the driver-level
	// guard above (c.tx != nil skips it entirely), and the one that used to
	// carry the engine's fkDeferredUnmodelled decline. It is served because
	// the driver now promises to run the COMMIT check itself
	// (MarkDeferredFKCommitter), so the deferral has a place to be reported:
	// the violating INSERT succeeds and the COMMIT is what fails, exactly as
	// C SQLite does it.
	// foreign_keys and the schema first: C SQLite IGNORES "PRAGMA
	// foreign_keys" inside a transaction (pragma.c's PragFlg_NeedSchema arm
	// refuses to change it once one is open), so enforcement has to be on
	// before the BEGIN or there is no violation to defer.
	for _, stmt := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(x INTEGER PRIMARY KEY)`,
		`CREATE TABLE ch(y REFERENCES p(x))`,
		`BEGIN`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
		t.Fatalf("the setter inside a driver-held BEGIN: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ch VALUES(99)`); err != nil {
		t.Fatalf("the deferred INSERT must succeed, got: %v", err)
	}
	if _, err := db.Exec(`COMMIT`); err == nil {
		t.Fatal("the COMMIT must report the deferred FOREIGN KEY violation, got success")
	}
	// ...and a failed COMMIT leaves the transaction OPEN, as C SQLite's
	// does, so there is still something to roll back.
	if _, err := db.Exec(`ROLLBACK`); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
}
