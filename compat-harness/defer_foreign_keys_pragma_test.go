// Differential gate for PRAGMA defer_foreign_keys: makes all foreign keys
// DEFERRABLE INITIALLY DEFERRED for the transaction. Tested through both engine
// and driver paths.
// are the oracle probes engine/fk.go's doc comment cites plus fkey6.test's own
// sequences, so the evidence and the gate cannot drift apart.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var deferFKPragmaCases = []struct {
	name  string
	stmts []string
}{
	// The headline behaviour: an IMMEDIATE constraint's violation survives the
	// statement and is reported by the COMMIT instead.
	{"an-immediate-violation-moves-to-the-commit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES('one'),('two')`,
		`INSERT INTO c VALUES('one')`,
		`BEGIN`,
		`DELETE FROM p`,
		`SELECT count(*) FROM p`,
		`COMMIT`,
		`SELECT count(*) FROM p`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`DELETE FROM p`,
		`SELECT count(*) FROM p`,
		`COMMIT`,
		`SELECT count(*) FROM p`,
		`ROLLBACK`,
		`SELECT count(*) FROM p`,
	}},
	// A failed COMMIT leaves the transaction -- and the flag, and the counter --
	// exactly as they were, and the same COMMIT succeeds once a statement
	// resolves the violation.
	{"failed-commit-keeps-the-flag-and-the-counter", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`DELETE FROM p`,
		`COMMIT`,
		`COMMIT`,
		`INSERT INTO p VALUES(1)`,
		`COMMIT`,
		`SELECT count(*) FROM p`,
		`SELECT count(*) FROM c`,
	}},
	// Setting it back to 0 ZEROES BOTH deferred counters, so the violation
	// vanishes.
	{"turning-it-off-zeroes-the-counter", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`DELETE FROM p`,
		`PRAGMA defer_foreign_keys=0`,
		`COMMIT`,
		`SELECT count(*) FROM p`,
		`SELECT count(*) FROM c`,
	}},
	{"turning-it-off-zeroes-a-deferrable-keys-count-too", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`DELETE FROM p`,
		`PRAGMA defer_foreign_keys=0`,
		`COMMIT`,
		`SELECT count(*) FROM p`,
	}},
	// The off-branch is WIDER than the pragma's own bucket, and this is the
	// case that proved it: with the flag NEVER turned on, a bare
	// "PRAGMA defer_foreign_keys=0" still wipes a DEFERRABLE INITIALLY DEFERRED
	// violation the transaction was already carrying. sqlite3Pragma zeroes
	// nDeferredCons alongside nDeferredImmCons.
	{"turning-it-off-zeroes-the-other-bucket-too", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`DELETE FROM p`,
		`PRAGMA defer_foreign_keys=0`,
		`COMMIT`,
		`ROLLBACK`,
		`SELECT count(*) FROM p`,
	}},
	// RESTRICT is DISABLED while it is on -- fkey6.test 3.2 and 3.3, verbatim.
	{"restrict-is-disabled", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p2(a PRIMARY KEY, b)`,
		`CREATE TABLE c2(x, y REFERENCES p2 ON DELETE RESTRICT ON UPDATE RESTRICT)`,
		`INSERT INTO p2 VALUES(1, 'one')`,
		`INSERT INTO p2 VALUES(2, 'two')`,
		`INSERT INTO c2 VALUES('i', 1)`,
		`BEGIN`,
		`UPDATE p2 SET a=a-1`,
		`COMMIT`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys = 1`,
		`UPDATE p2 SET a=a-1`,
		`COMMIT`,
		`SELECT a, b FROM p2 ORDER BY a`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys = 1`,
		`UPDATE p2 SET a=a-1`,
		`COMMIT`,
		`ROLLBACK`,
		`SELECT a, b FROM p2 ORDER BY a`,
	}},
	{"restrict-disabled-with-a-repairing-delete-trigger", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p2(a PRIMARY KEY, b)`,
		`CREATE TABLE c2(x, y REFERENCES p2 ON DELETE RESTRICT)`,
		`INSERT INTO p2 VALUES(1, 'one')`,
		`INSERT INTO p2 VALUES(2, 'two')`,
		`INSERT INTO c2 VALUES('i', 1)`,
		`CREATE TRIGGER p2t AFTER DELETE ON p2 BEGIN INSERT INTO p2 VALUES(old.a, 'deleted!'); END`,
		`BEGIN`,
		`DELETE FROM p2 WHERE a=1`,
		`COMMIT`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys = 1`,
		`DELETE FROM p2 WHERE a=1`,
		`COMMIT`,
		`SELECT a, b FROM p2 ORDER BY a`,
	}},
	// The lifetime rules: a SAVEPOINT, an inner RELEASE and a ROLLBACK TO all
	// leave it on; the outermost COMMIT/ROLLBACK switches it off, so the next
	// transaction's immediate violation is reported at its own statement again
	// (fkey6.test 1.10).
	{"savepoints-do-not-clear-it-and-the-commit-does", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`SAVEPOINT s1`,
		`DELETE FROM p`,
		`RELEASE s1`,
		`SAVEPOINT s2`,
		`ROLLBACK TO s2`,
		`RELEASE s2`,
		`COMMIT`,
		`ROLLBACK`,
		`SELECT count(*) FROM p`,
		`BEGIN`,
		`DELETE FROM p`,
		`ROLLBACK`,
		`SELECT count(*) FROM p`,
	}},
	// ROLLBACK TO restores the COUNTER with the rows: the violation created
	// after the savepoint is undone, so the COMMIT succeeds.
	{"rollback-to-restores-the-counter", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`SAVEPOINT s1`,
		`DELETE FROM p`,
		`ROLLBACK TO s1`,
		`COMMIT`,
		`SELECT count(*) FROM p`,
		`SELECT count(*) FROM c`,
	}},
	// A RELEASE that is itself the outermost commit runs the check and, when it
	// fails, changes nothing at all.
	{"release-of-the-outermost-savepoint-is-the-commit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`SAVEPOINT s1`,
		`DELETE FROM p`,
		`RELEASE s1`,
		`INSERT INTO p VALUES(1)`,
		`COMMIT`,
		`SELECT count(*) FROM p`,
	}},
	// It works with foreign_keys OFF, and turns nothing on by itself.
	{"with-foreign-keys-off-it-enforces-nothing", []string{
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`INSERT INTO c VALUES(9)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// A violation the flag deferred can be repaired by a LATER statement in the
	// same transaction -- the never-decrement-from-zero rule reads the flag's
	// own counter, not the constraint's.
	{"a-later-statement-repairs-it", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE c(x REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`INSERT INTO c VALUES(9)`,
		`INSERT INTO p VALUES(9)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys=1`,
		`INSERT INTO c VALUES(8)`,
		`DELETE FROM c WHERE x=8`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
}

// TestDeferForeignKeysPragma replays each script through both engines and
// requires the same accept/reject decision and the same rows.
func TestDeferForeignKeysPragma(t *testing.T) {
	for _, tc := range deferFKPragmaCases {
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

// TestDeferForeignKeysPragmaGetter pins what used to be this file's DECLINE
// list. Both shapes are served now:
//
//   - a non-canonical boolean spelling, because sqlite3GetBoolean
//     (pragma.c:97) sends a digit-leading value to (u8)sqlite3Atoi and reads
//     everything else against its six words, falling back to the FLAG default
//     of 0 (pragma.c:1164);
//   - the GETTER as a query, which used to have no read-side reporter at all
//     and answered "unsupported PRAGMA defer_foreign_keys" where C SQLite
//     answers one row.
//
// The flag is per-CONNECTION and lives for one transaction, so the row comes
// from the snapshot the read was taken against (ReadOnlyPager.deferFKs).
func TestDeferForeignKeysPragmaGetter(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want string
	}{
		{`1`, `I:1`}, {`2`, `I:1`}, {`7`, `I:1`}, {`0x1`, `I:1`},
		{`yes`, `I:1`}, {`true`, `I:1`}, {`ON`, `I:1`},
		{`0`, `I:0`}, {`off`, `I:0`}, {`no`, `I:0`}, {`false`, `I:0`},
		{`nonsense`, `I:0`}, {`-1`, `I:0`}, {`''`, `I:0`},
	} {
		t.Run(tc.set, func(t *testing.T) {
			godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			for _, stmt := range []string{`BEGIN`, `PRAGMA defer_foreign_keys=` + tc.set} {
				if execErr, panicked, pv := tclSafeExecArgs(godb, stmt); panicked {
					t.Fatalf("engine PANICKED on %q: %v", stmt, pv)
				} else if execErr != nil {
					t.Fatalf("engine DECLINED %q: %v", stmt, execErr)
				}
			}
			cols, rows, qerr, panicked, pv := tclSafeGoQuery(godb, `PRAGMA defer_foreign_keys`)
			if panicked {
				t.Fatalf("engine PANICKED on the getter: %v", pv)
			}
			if qerr != nil {
				t.Fatalf("engine DECLINED the getter after =%s: %v", tc.set, qerr)
			}
			if len(cols) != 1 || cols[0] != "defer_foreign_keys" || len(rows) != 1 || rows[0][0] != tc.want {
				t.Errorf("=%s: getter answered cols=%v rows=%v, want [[%s]]", tc.set, cols, rows, tc.want)
			}
		})
	}
}

// TestDeferForeignKeysNonCanonicalDefers pins what "PRAGMA
// defer_foreign_keys=2" now DOES, which the decline above only said this
// engine would not do: a nonzero value defers the immediate constraints to
// COMMIT, so the violating INSERT succeeds and the COMMIT is what fails.
//
// This is the pair the decline hid. sqlite3GetBoolean's digit branch reads
// "2" as 2 and every nonzero value is true, so "=2", "=7" and "=0x1" are all
// the same statement as "=1" -- while "=nonsense" is the FLAG DEFAULT, 0
// (pragma.c:1164), and leaves the constraint immediate.
func TestDeferForeignKeysNonCanonicalDefers(t *testing.T) {
	for _, v := range []string{"1", "2", "7", "0x1", "yes", "off", "nonsense", "-1"} {
		v := v
		t.Run(v, func(t *testing.T) {
			differ(t, "defer-fk/"+v, []string{
				`PRAGMA foreign_keys=ON`,
				`CREATE TABLE p(x INTEGER PRIMARY KEY)`,
				`CREATE TABLE c(y REFERENCES p(x))`,
				`BEGIN`,
				`PRAGMA defer_foreign_keys=` + v,
				`INSERT INTO c VALUES(99)`,
				`COMMIT`,
				`SELECT count(*) FROM c`,
			})
		})
	}
}

// TestDeferForeignKeysAutocommitLifetime pins "PRAGMA defer_foreign_keys" set
// OUTSIDE a transaction, which this driver used to REFUSE on the grounds that
// "the flag's effect would not survive" its throwaway per-statement session.
// The effect is what had to survive, and now does: Conn.deferFKs carries the
// bit, every session and read pager is stamped with it, and the ENGINE's own
// lifetime rules decide each transition.
//
// The rule those rules implement is C's: sqlite3VdbeHalt clears SQLITE_DeferFKs
// when a statement's compiled Vdbe set bIsReader AND left autocommit on
// (vdbeaux.c:3344/3401-3405/3432-3434, with sqlite3RollbackAll's twin at
// main.c:1530-1532). Every row below is that rule showing through, measured on
// 3.53.3 first:
//
//	=ON then its own getter          KEPT -- a bare pragma codes no reader
//	=ON then "SELECT 1"             KEPT -- no cursor anywhere
//	=ON then "SELECT count(*) FROM t"  CLEARED
//	=ON then a write                CLEARED
//	=ON then "PRAGMA user_version"  CLEARED -- that one codes a read txn
//	=ON then BEGIN                  KEPT -- BEGIN turns autoCommit off as part
//	                                of its own run, so its own Halt sees 0
//	COMMIT / ROLLBACK               CLEARED
//
// and the FIRST case is the one the refusal existed for: fkey6.test 1.8's
// "PRAGMA defer_foreign_keys=ON; BEGIN; DELETE FROM t1 WHERE x=3", where the
// DELETE must be DEFERRED past the BEGIN and the transaction must then commit
// once a later statement resolves the violation.
func TestDeferForeignKeysAutocommitLifetime(t *testing.T) {
	cases := []struct {
		name         string
		setup, steps []string
	}{
		{"fkey6 1.8: deferred past a later BEGIN", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
			`CREATE TABLE t2(y REFERENCES t1)`,
			`INSERT INTO t1 VALUES(1),(2),(3)`, `INSERT INTO t2 VALUES(3)`,
		}, []string{
			`PRAGMA defer_foreign_keys=ON`, `BEGIN`, `DELETE FROM t1 WHERE x=3`,
			`SELECT count(*) FROM t1`, `UPDATE t2 SET y=1`, `COMMIT`,
			`SELECT count(*) FROM t1`, `PRAGMA defer_foreign_keys`,
		}},
		{"unresolved at commit keeps the transaction open", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
			`CREATE TABLE t2(y REFERENCES t1)`,
			`INSERT INTO t1 VALUES(1)`, `INSERT INTO t2 VALUES(1)`,
		}, []string{
			`PRAGMA defer_foreign_keys=ON`, `BEGIN`, `DELETE FROM t1`, `COMMIT`,
			`SELECT count(*) FROM t1`,
		}},
		{"kept over its own getter", nil, []string{
			`PRAGMA defer_foreign_keys=ON`, `PRAGMA defer_foreign_keys`, `PRAGMA defer_foreign_keys`,
		}},
		{"kept over a FROM-less SELECT", nil, []string{
			`PRAGMA defer_foreign_keys=ON`, `SELECT 1`, `PRAGMA defer_foreign_keys`,
		}},
		{"cleared by a table read", []string{`CREATE TABLE t(a)`}, []string{
			`PRAGMA defer_foreign_keys=ON`, `SELECT count(*) FROM t`, `PRAGMA defer_foreign_keys`,
		}},
		{"cleared by a write", []string{`CREATE TABLE t(a)`}, []string{
			`PRAGMA defer_foreign_keys=ON`, `INSERT INTO t VALUES(1)`, `PRAGMA defer_foreign_keys`,
		}},
		{"cleared by a pragma that codes a read txn", nil, []string{
			`PRAGMA defer_foreign_keys=ON`, `PRAGMA user_version`, `PRAGMA defer_foreign_keys`,
		}},
		{"kept over a read of a table that does not exist", nil, []string{
			`PRAGMA defer_foreign_keys=ON`, `SELECT * FROM nosuch`, `PRAGMA defer_foreign_keys`,
		}},
		{"BEGIN keeps it, ROLLBACK clears it", nil, []string{
			`PRAGMA defer_foreign_keys=ON`, `BEGIN`, `PRAGMA defer_foreign_keys`,
			`ROLLBACK`, `PRAGMA defer_foreign_keys`,
		}},
		{"value spellings are sqlite3GetBoolean's", nil, []string{
			`PRAGMA defer_foreign_keys=2`, `PRAGMA defer_foreign_keys`,
			`PRAGMA defer_foreign_keys=bogus`, `PRAGMA defer_foreign_keys`,
		}},
		{"OFF clears it", nil, []string{
			`PRAGMA defer_foreign_keys=ON`, `PRAGMA defer_foreign_keys=OFF`,
			`PRAGMA defer_foreign_keys`,
		}},
	}
	for ci, c := range cases {
		ci, c := ci, c
		t.Run(fmt.Sprintf("%02d-%s", ci, c.name), func(t *testing.T) {
			var out [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range c.setup {
					if _, err := db.Exec(s); err != nil {
						t.Fatalf("%s: %v", s, err)
					}
				}
				for _, q := range c.steps {
					out[i] += " | " + renderQuery(db, q)
				}
				db.Close()
			}
			if out[0] != out[1] {
				t.Errorf("%v\n  cgo:%s\n  mus:%s", c.steps, out[0], out[1])
			}
		})
	}
}
