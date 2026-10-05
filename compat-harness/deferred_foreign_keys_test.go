// This file tests DEFERRABLE INITIALLY DEFERRED foreign keys, which are checked
// at COMMIT time rather than at statement end.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// deferredFKCases are deferred foreign key test cases.
var deferredFKCases = []struct {
	name  string
	stmts []string
}{
	{"failed-commit-leaves-the-transaction-open", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`SELECT count(*) FROM c`,
		`COMMIT`,
		`BEGIN`,
		`SELECT count(*) FROM c`,
		`COMMIT`,
		`INSERT INTO p VALUES(9)`,
		`COMMIT`,
		`COMMIT`,
		`ROLLBACK`,
		`SELECT count(*) FROM c`,
	}},
	{"failed-commit-then-rollback", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`COMMIT`,
		`ROLLBACK`,
		`SELECT count(*) FROM c`,
		`COMMIT`,
		`BEGIN`,
		`COMMIT`,
	}},
	{"violation-resolved-before-commit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`INSERT INTO p VALUES(9)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	{"resolving-child-row-deleted-before-commit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`DELETE FROM c`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// An UNRELATED parent key resolves nothing: the counter is keyed on the
	// child rows that actually match, not on "a parent row appeared".
	{"unrelated-parent-key-resolves-nothing", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`INSERT INTO p VALUES(7)`,
		`COMMIT`,
		`INSERT INTO p VALUES(9)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// The immediate counter's never-decrement-from-zero rule, re-derived for
	// the deferred one: with a pre-existing orphan (made while foreign_keys was
	// OFF), a DELETE is fine and "SET y=y" is not -- while assigning a column
	// the key does not name leaves the constraint alone entirely.
	{"deferred-counter-never-decrements-from-zero", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(x, y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO c VALUES('a',1)`,
		`PRAGMA foreign_keys=ON`,
		`BEGIN`,
		`UPDATE c SET x='b'`,
		`COMMIT`,
		`BEGIN`,
		`UPDATE c SET y=y`,
		`COMMIT`,
		`ROLLBACK`,
		`BEGIN`,
		`DELETE FROM c`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// An IMMEDIATE violation must not be cancelled by a DEFERRED decrement in
	// the same statement, which is what one shared counter would do: p is both
	// a child of q (deferred, pre-existing orphan) and a parent of k
	// (immediate), so deleting p's row counts +1 immediate for k's orphaned row
	// and would have taken it straight back off for q's.
	{"deferred-decrement-cannot-cancel-an-immediate-violation", []string{
		`CREATE TABLE q(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, b REFERENCES q DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TABLE k(y REFERENCES p)`,
		`INSERT INTO p VALUES(1,8)`,
		`INSERT INTO k VALUES(1)`,
		`PRAGMA foreign_keys=ON`,
		`DELETE FROM p WHERE id=1`,
		`SELECT count(*) FROM p`,
		`BEGIN`,
		`DELETE FROM p WHERE id=1`,
		`SELECT count(*) FROM p`,
		`COMMIT`,
	}},
	// ROLLBACK TO restores the counter with the rows; RELEASE of an INNER
	// savepoint keeps it.
	{"rollback-to-restores-the-counter", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(8)`,
		`SAVEPOINT s1`,
		`INSERT INTO c VALUES(9)`,
		`ROLLBACK TO s1`,
		`COMMIT`,
		`INSERT INTO p VALUES(8)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	{"rollback-to-a-savepoint-before-the-violation", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`SAVEPOINT a`,
		`INSERT INTO c VALUES(7)`,
		`SAVEPOINT b`,
		`INSERT INTO c VALUES(8)`,
		`ROLLBACK TO b`,
		`COMMIT`,
		`ROLLBACK TO a`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	{"release-of-an-inner-savepoint-keeps-the-violation", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`SAVEPOINT s1`,
		`INSERT INTO c VALUES(9)`,
		`RELEASE s1`,
		`COMMIT`,
		`ROLLBACK TO s1`,
		`ROLLBACK`,
		`SELECT count(*) FROM c`,
	}},
	// A SAVEPOINT in autocommit STARTS the transaction, so releasing it IS the
	// outermost commit -- it runs the check, and a failure changes nothing at
	// all: the row stays, the savepoint stays, and releasing it again after the
	// parent row lands succeeds.
	{"release-that-commits-runs-the-check-and-changes-nothing-on-failure", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`SAVEPOINT s1`,
		`INSERT INTO c VALUES(9)`,
		`RELEASE s1`,
		`SELECT count(*) FROM c`,
		`INSERT INTO p VALUES(9)`,
		`RELEASE s1`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// Only "DEFERRABLE INITIALLY DEFERRED" defers. Every other spelling of the
	// clause -- including "NOT DEFERRABLE INITIALLY DEFERRED" -- is immediate.
	{"only-initially-deferred-defers", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY IMMEDIATE)`,
		`CREATE TABLE d(y REFERENCES p NOT DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TABLE e(y REFERENCES p DEFERRABLE)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`INSERT INTO d VALUES(9)`,
		`INSERT INTO e VALUES(9)`,
		`COMMIT`,
	}},
	{"autocommit-deferred-behaves-immediately", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO c VALUES(9)`,
		`SELECT count(*) FROM c`,
		`INSERT INTO p VALUES(9)`,
		`INSERT INTO c VALUES(9)`,
		`SELECT count(*) FROM c`,
	}},
	// The ACTIONS are not deferred, only the check: a cascade removes the child
	// rows the instant the parent row goes, and RESTRICT still refuses at once.
	{"actions-still-fire-at-the-mutation", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cc(y REFERENCES p ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TABLE cn(y REFERENCES p ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO cc VALUES(1),(2)`,
		`INSERT INTO cn VALUES(1),(2)`,
		`BEGIN`,
		`DELETE FROM p WHERE id=1`,
		`SELECT count(*) FROM cc`,
		`SELECT y FROM cn ORDER BY rowid`,
		`COMMIT`,
		`SELECT count(*) FROM cc`,
		`SELECT y FROM cn ORDER BY rowid`,
	}},
	{"restrict-is-never-deferred", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES(1),(2)`,
		`BEGIN`,
		`DELETE FROM p WHERE id=1`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// A resolution error is still raised where it always was -- deferring the
	// CHECK does not defer the code-generation failure.
	{"resolution-errors-are-not-deferred", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE c(y REFERENCES nosuch DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(1)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// Two orphans, one repaired: the counter is a count, not a flag.
	{"two-violations-one-repaired", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(8)`,
		`INSERT INTO c VALUES(9)`,
		`INSERT INTO p VALUES(8)`,
		`COMMIT`,
		`INSERT INTO p VALUES(9)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	{"self-referencing-rows-that-point-at-each-other", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, par REFERENCES t DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO t VALUES(1,2)`,
		`INSERT INTO t VALUES(2,1)`,
		`COMMIT`,
		`SELECT count(*) FROM t`,
	}},
	{"composite-table-level-key", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a,b,PRIMARY KEY(a,b))`,
		`CREATE TABLE c(x,y,FOREIGN KEY(x,y) REFERENCES p(a,b) DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(1,2)`,
		`COMMIT`,
		`INSERT INTO p VALUES(1,2)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	{"insert-or-replace-orphans-through-its-victim-delete", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`BEGIN`,
		`INSERT OR REPLACE INTO c VALUES(1,9)`,
		`COMMIT`,
		`ROLLBACK`,
		`SELECT y FROM c`,
	}},
	{"parent-key-moved-out-from-under-a-child", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`UPDATE p SET id=2`,
		`SELECT y FROM c`,
		`COMMIT`,
		`UPDATE c SET y=2`,
		`COMMIT`,
		`SELECT y FROM c`,
	}},
	// A DDL statement inside the transaction rides along with everything else:
	// the failed COMMIT keeps it, the ROLLBACK after it takes it away.
	{"ddl-inside-a-transaction-that-fails-to-commit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`CREATE TABLE z(a)`,
		`COMMIT`,
		`SELECT count(*) FROM z`,
		`ROLLBACK`,
		`SELECT count(*) FROM sqlite_master WHERE name='z'`,
	}},
	// An "OR ROLLBACK" conflict tears the whole transaction down, counter
	// included -- the next BEGIN/COMMIT pair must be clean.
	{"conflict-rollback-clears-the-counter", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TABLE u(k UNIQUE)`,
		`INSERT INTO u VALUES(1)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`INSERT OR ROLLBACK INTO u VALUES(1)`,
		`SELECT count(*) FROM c`,
		`BEGIN`,
		`COMMIT`,
	}},
	{"foreign-keys-off-defers-nothing", []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
	}},
	// fkey6.test 1.20-1.22, verbatim in SQL: the parent delete leaves the
	// deferred violation standing, and deleting the CHILD row that carried it
	// takes it back off again.
	{"fkey6-parent-delete-then-child-delete", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2(y INTEGER PRIMARY KEY, z INTEGER REFERENCES t1(x) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE INDEX t2z ON t2(z)`,
		`CREATE TABLE t3(u INTEGER PRIMARY KEY, v INTEGER REFERENCES t1(x))`,
		`CREATE INDEX t3v ON t3(v)`,
		`INSERT INTO t1 VALUES(1),(2),(3),(4),(5)`,
		`INSERT INTO t2 VALUES(1,1),(2,2)`,
		`INSERT INTO t3 VALUES(3,3),(4,4)`,
		`DELETE FROM t1 WHERE x=2`,
		`BEGIN`,
		`DELETE FROM t1 WHERE x=1`,
		`COMMIT`,
		`DELETE FROM t2 WHERE y=1`,
		`COMMIT`,
		`SELECT count(*) FROM t1`,
		`SELECT count(*) FROM t2`,
	}},
	// e_fkey.test's own deferred case: the child row lands before the parent
	// row is deleted, so the transaction holds two violations at once.
	{"e_fkey-orphan-then-parent-delete", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t1(a PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'one')`,
		`CREATE TABLE cd(c, d, FOREIGN KEY(c) REFERENCES t1(a) DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO cd VALUES('x','y')`,
		`DELETE FROM t1 WHERE a = 1`,
		`SELECT count(*) FROM cd`,
		`COMMIT`,
		`DELETE FROM cd`,
		`COMMIT`,
		`SELECT count(*) FROM cd`,
	}},
	// DROP TABLE of a parent is the implicit "DELETE FROM parent" fk.go
	// describes -- a DEFERRABLE INITIALLY DEFERRED referrer's orphans are
	// booked into the SAME counter an ordinary DELETE would use, not reported
	// at the DROP itself. These four used to be the two declined shapes this
	// file's TestDeferredForeignKeyDeclines pinned (fkBeforeDropTable's own
	// doc comment has the fkCounterFor rewrite that closed them); moved here
	// once the oracle evidence below was in hand, verified directly against
	// mattn/go-sqlite3 3.53.3 and matching tkt-b1d3a2e531.test 1.2-1.4 and
	// fkey6.test 2.3-2.6 verbatim.
	{"drop-parent-then-commit-fails-then-rollback", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`DROP TABLE p`,
		`COMMIT`,
		`SELECT count(*) FROM c`,
		`ROLLBACK`,
		`SELECT name FROM sqlite_master WHERE name='p'`,
	}},
	// tkt-b1d3a2e531.test 1.2: dropping the CHILD too, in the same
	// transaction, resolves what the parent's drop orphaned and COMMIT
	// succeeds -- in either drop order (1.2 drops the parent first, 1.4 the
	// child first) and for both a separate rowid and an INTEGER PRIMARY KEY
	// child key (sections 1 and 2 of that file).
	{"drop-parent-then-drop-child-resolves-it", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`DROP TABLE p`,
		`DROP TABLE c`,
		`COMMIT`,
		`SELECT name FROM sqlite_master WHERE name IN ('p','c')`,
	}},
	{"drop-child-then-drop-parent-also-resolves-it", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y INTEGER PRIMARY KEY REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`DROP TABLE c`,
		`DROP TABLE p`,
		`COMMIT`,
		`SELECT name FROM sqlite_master WHERE name IN ('p','c')`,
	}},
	// tkt-b1d3a2e531.test 3.2/3.3: TWO deferred referrers, only one of them
	// dropped -- the failed COMMIT's own PRAGMA foreign_key_check names the
	// survivor, and dropping it is what the next COMMIT needs.
	{"drop-parent-with-two-referrers-one-resolved", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c1(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TABLE c2(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c1 VALUES(1)`,
		`INSERT INTO c2 VALUES(1)`,
		`BEGIN`,
		`DROP TABLE p`,
		`DROP TABLE c1`,
		`COMMIT`,
		`SELECT * FROM pragma_foreign_key_check()`,
		`DROP TABLE c2`,
		`COMMIT`,
		`SELECT name FROM sqlite_master WHERE name IN ('p','c1','c2')`,
	}},
	// A DEFERRED child resolves its OWN outstanding violation by dropping
	// itself, exactly like the parent-drop cases above but from the other
	// side (fkey6.test 2.4/2.6 do this through a DELETE and an extra INSERT;
	// this is the direct shape).
	{"drop-child-while-its-own-violation-is-outstanding", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`BEGIN`,
		`INSERT INTO c VALUES(9)`,
		`DROP TABLE c`,
		`COMMIT`,
		`SELECT name FROM sqlite_master WHERE name='c'`,
	}},
	// fkey6.test 2.3-2.6, verbatim: "PRAGMA defer_foreign_keys" reaches DROP
	// TABLE through every key, not just a DEFERRABLE INITIALLY DEFERRED one.
	{"fkey6-2.3-defer-pragma-drop-parent-then-rollback", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p1(a PRIMARY KEY)`,
		`INSERT INTO p1 VALUES('one'), ('two')`,
		`CREATE TABLE c1(x REFERENCES p1)`,
		`INSERT INTO c1 VALUES('two'), ('one')`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys = 1`,
		`DROP TABLE p1`,
		`ROLLBACK`,
		`PRAGMA defer_foreign_keys`,
		`SELECT name FROM sqlite_master WHERE name='p1'`,
	}},
	{"fkey6-2.4-defer-pragma-delete-parent-then-drop-child", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p1(a PRIMARY KEY)`,
		`INSERT INTO p1 VALUES('one'), ('two')`,
		`CREATE TABLE c1(x REFERENCES p1)`,
		`INSERT INTO c1 VALUES('two'), ('one')`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys = 1`,
		`DELETE FROM p1`,
		`DROP TABLE c1`,
		`COMMIT`,
		`PRAGMA defer_foreign_keys`,
		`SELECT count(*) FROM p1`,
		`SELECT name FROM sqlite_master WHERE name='c1'`,
	}},
	{"fkey6-2.6-defer-pragma-insert-orphan-then-drop-child", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p1(a PRIMARY KEY)`,
		`INSERT INTO p1 VALUES('one'), ('two')`,
		`CREATE TABLE c1(x REFERENCES p1)`,
		`INSERT INTO c1 VALUES('two'), ('one')`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys = 1`,
		`INSERT INTO c1 VALUES('three')`,
		`DROP TABLE c1`,
		`COMMIT`,
		`PRAGMA defer_foreign_keys`,
	}},
	// RESTRICT is never deferred by DEFERRABLE alone, including through a
	// DROP TABLE -- it still fails at the DROP itself, not the COMMIT.
	{"drop-parent-restrict-referrer-fails-immediately", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`DROP TABLE p`,
		`ROLLBACK`,
		`SELECT name FROM sqlite_master WHERE name='p'`,
	}},
	// ...but "PRAGMA defer_foreign_keys" disables that immediacy for RESTRICT
	// too (fkParentSide's own rule, reached here through a DROP instead of a
	// DELETE).
	{"drop-parent-restrict-referrer-deferred-by-pragma", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y REFERENCES p ON DELETE RESTRICT)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`BEGIN`,
		`PRAGMA defer_foreign_keys = 1`,
		`DROP TABLE p`,
		`DROP TABLE c`,
		`COMMIT`,
		`PRAGMA defer_foreign_keys`,
		`SELECT name FROM sqlite_master WHERE name IN ('p','c')`,
	}},
}

// TestDeferredForeignKeys replays each script against engine.DB and real C
// SQLite in lockstep. Both must agree on every statement: whether an exec
// errored, and every cell a query returned.
func TestDeferredForeignKeys(t *testing.T) {
	for _, tc := range deferredFKCases {
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
