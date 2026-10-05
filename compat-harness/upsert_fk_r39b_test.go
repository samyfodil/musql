// UPSERT ... DO UPDATE foreign key constraints.
// the file, not a decline.
//
// The gate is written the way deferred_foreign_keys_test.go is -- engine.DB and
// one cgo connection in lockstep, an exec agreeing about whether it ERRORS and
// a query agreeing about every cell -- because a deferred key is served only
// for a transaction the ENGINE opened with a SQL "BEGIN" (see that file's
// header). Every case therefore reads the table back: skipping the check is
// silent, so the ERROR is only half the contract and the surviving ROW is the
// other half.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var r39bUpsertFKCases = []struct {
	name  string
	stmts []string
}{
	// ---- the child side: DO UPDATE assigns the key ----
	//
	// The candidate row (1,1) is itself FK-clean, so the only thing that can
	// raise here is the DO UPDATE's own check on y=99.
	{"do-update-into-a-missing-parent-fails", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`INSERT INTO c(k,y) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET y=99`,
		`SELECT k,y FROM c`,
		`SELECT count(*) FROM pragma_foreign_key_check('c')`,
	}},
	{"do-update-into-a-present-parent-succeeds", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES(1,1)`,
		`INSERT INTO c(k,y) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET y=2`,
		`SELECT k,y FROM c`,
	}},
	// "excluded" is the ordinary way to write the SET, and it reaches the same
	// column: the mask is about WHICH column is assigned, never about what the
	// value came from.
	{"do-update-set-from-excluded-into-a-missing-parent-fails", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`INSERT INTO c(k,y) VALUES(1,99) ON CONFLICT(k) DO UPDATE SET y=excluded.y`,
		`SELECT k,y FROM c`,
	}},
	// fkChildIsModified's OTHER half (fkey.c:808): a SET that names no key
	// column consults no key at all, so a row that was ALREADY an orphan (made
	// while foreign_keys was off) can still be updated. This is the case a
	// "just pass nil" "fix" would break, which is why it sits next to the one
	// above.
	{"do-update-assigning-a-non-key-column-leaves-an-orphan-alone", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y REFERENCES p)`,
		`INSERT INTO c VALUES(1,'a',77)`,
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO c(k,x,y) VALUES(1,'a',77) ON CONFLICT(k) DO UPDATE SET x='b'`,
		`SELECT k,x,y FROM c`,
	}},
	// ...and assigning the key to the value it already holds DOES consult it:
	// aXRef[y] is >=0 whether or not the value changes.
	{"do-update-assigning-the-key-to-its-own-orphan-value-still-fails", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y REFERENCES p)`,
		`INSERT INTO c VALUES(1,'a',77)`,
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO c(k,x,y) VALUES(1,'a',77) ON CONFLICT(k) DO UPDATE SET y=y`,
		`SELECT k,x,y FROM c`,
	}},
	// A composite child key: assigning EITHER of its columns arms the check
	// (fkey.c:807 loops over every column of the key).
	{"do-update-assigning-one-column-of-a-composite-key-fails", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(a,b,PRIMARY KEY(a,b))`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y, FOREIGN KEY(x,y) REFERENCES p(a,b))`,
		`INSERT INTO p VALUES(1,2)`,
		`INSERT INTO c VALUES(1,1,2)`,
		`INSERT INTO c(k,x,y) VALUES(1,1,2) ON CONFLICT(k) DO UPDATE SET y=3`,
		`SELECT k,x,y FROM c`,
	}},
	// Two keys on one table, one of them assigned: the unassigned one must not
	// be dragged in, and the assigned one must still fire.
	{"do-update-consults-only-the-key-it-assigns", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE q(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p, z REFERENCES q)`,
		`INSERT INTO c VALUES(1,77,88)`,
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO q VALUES(88)`,
		`INSERT INTO c(k,y,z) VALUES(1,77,88) ON CONFLICT(k) DO UPDATE SET z=88`,
		`SELECT k,y,z FROM c`,
		`INSERT INTO c(k,y,z) VALUES(1,77,88) ON CONFLICT(k) DO UPDATE SET y=77`,
		`SELECT k,y,z FROM c`,
	}},

	// ---- the branches that must NOT gain a check ----
	//
	// DO NOTHING writes nothing, so there is no row mutation to check at all
	// (upsert.c only reaches sqlite3Update for the DO UPDATE branch).
	{"do-nothing-runs-no-check", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO c VALUES(1,77)`,
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO c(k,y) VALUES(1,88) ON CONFLICT(k) DO NOTHING`,
		`SELECT k,y FROM c`,
	}},
	// A DO UPDATE whose WHERE excludes the row is an UPDATE that matched zero
	// rows: no mutation, hence no check.
	{"do-update-with-a-false-where-runs-no-check", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO c VALUES(1,77)`,
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO c(k,y) VALUES(1,88) ON CONFLICT(k) DO UPDATE SET y=88 WHERE c.y > 100`,
		`SELECT k,y FROM c`,
	}},
	// No conflict at all: the row goes down the PLAIN insert path, which passes
	// no map (insert.c:1573) and so checks every key -- an orphan INSERT still
	// fails.
	{"no-conflict-takes-the-plain-insert-check", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`INSERT INTO c(k,y) VALUES(2,99) ON CONFLICT(k) DO UPDATE SET y=1`,
		`SELECT k,y FROM c ORDER BY k`,
	}},

	// ---- the parent side ----
	//
	// The upsert target is the PARENT here. Its key is a non-rowid UNIQUE
	// column because a DO UPDATE may not reassign the rowid alias.
	{"do-update-on-a-parent-cascades-to-its-children", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, k UNIQUE)`,
		`CREATE TABLE c(y REFERENCES p(k) ON UPDATE CASCADE)`,
		`INSERT INTO p VALUES(1,10)`,
		`INSERT INTO c VALUES(10)`,
		`INSERT INTO p(id,k) VALUES(1,10) ON CONFLICT(id) DO UPDATE SET k=20`,
		`SELECT id,k FROM p`,
		`SELECT y FROM c`,
	}},
	{"do-update-on-a-parent-sets-its-children-null", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, k UNIQUE)`,
		`CREATE TABLE c(y REFERENCES p(k) ON UPDATE SET NULL)`,
		`INSERT INTO p VALUES(1,10)`,
		`INSERT INTO c VALUES(10)`,
		`INSERT INTO p(id,k) VALUES(1,10) ON CONFLICT(id) DO UPDATE SET k=20`,
		`SELECT id,k FROM p`,
		`SELECT y FROM c`,
	}},
	{"do-update-on-a-restricted-parent-is-refused", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, k UNIQUE)`,
		`CREATE TABLE c(y REFERENCES p(k) ON UPDATE RESTRICT)`,
		`INSERT INTO p VALUES(1,10)`,
		`INSERT INTO c VALUES(10)`,
		`INSERT INTO p(id,k) VALUES(1,10) ON CONFLICT(id) DO UPDATE SET k=20`,
		`SELECT id,k FROM p`,
		`SELECT y FROM c`,
	}},
	// No action clause: orphaning a child by moving the parent key is the plain
	// immediate violation.
	{"do-update-orphaning-a-child-fails", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, k UNIQUE)`,
		`CREATE TABLE c(y REFERENCES p(k))`,
		`INSERT INTO p VALUES(1,10)`,
		`INSERT INTO c VALUES(10)`,
		`INSERT INTO p(id,k) VALUES(1,10) ON CONFLICT(id) DO UPDATE SET k=20`,
		`SELECT id,k FROM p`,
		`SELECT y FROM c`,
	}},

	// ---- DEFERRABLE INITIALLY DEFERRED, inside a transaction ----
	//
	// The violation must survive to the COMMIT, fail it, leave the transaction
	// open, and then clear once the parent row lands (the state machine
	// deferred_foreign_keys_test.go pins for a plain UPDATE).
	{"deferred-do-update-violation-fails-the-commit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`BEGIN`,
		`INSERT INTO c(k,y) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET y=99`,
		`SELECT k,y FROM c`,
		`COMMIT`,
		`INSERT INTO p VALUES(99)`,
		`COMMIT`,
		`SELECT k,y FROM c`,
	}},
	{"deferred-do-update-violation-repaired-before-commit", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`BEGIN`,
		`INSERT INTO c(k,y) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET y=99`,
		`INSERT INTO p VALUES(99)`,
		`COMMIT`,
		`SELECT k,y FROM c`,
	}},
	{"deferred-do-update-violation-rolled-back", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`BEGIN`,
		`INSERT INTO c(k,y) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET y=99`,
		`ROLLBACK`,
		`SELECT k,y FROM c`,
		`BEGIN`,
		`COMMIT`,
	}},
	// The immediate key inside an explicit transaction still fails AT THE
	// STATEMENT, and the statement's own effect is unwound while the
	// transaction's earlier work survives.
	{"immediate-do-update-fails-inside-a-transaction-and-unwinds-only-itself", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1,1)`,
		`BEGIN`,
		`INSERT INTO c VALUES(2,1)`,
		`INSERT INTO c(k,y) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET y=99`,
		`SELECT k,y FROM c ORDER BY k`,
		`COMMIT`,
		`SELECT k,y FROM c ORDER BY k`,
	}},

	// ---- a multi-row upsert: the check is per row ----
	//
	// Row a=1 conflicts and its DO UPDATE is clean; row a=2 conflicts and its
	// DO UPDATE orphans. The whole statement must fail and leave NEITHER row
	// changed.
	{"multi-row-do-update-fails-on-the-second-row", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES(1,1),(2,1)`,
		`INSERT INTO c(k,y) VALUES(1,2),(2,99) ON CONFLICT(k) DO UPDATE SET y=excluded.y`,
		`SELECT k,y FROM c ORDER BY k`,
	}},

	// ---- foreign_keys=OFF checks nothing, whichever branch runs ----
	{"foreign-keys-off-checks-nothing", []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, y REFERENCES p)`,
		`INSERT INTO c VALUES(1,1)`,
		`INSERT INTO c(k,y) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET y=99`,
		`SELECT k,y FROM c`,
	}},
}

// TestR39BUpsertForeignKeys replays each script against engine.DB and real C
// SQLite in lockstep. Both must agree on every statement: whether an exec
// errored, and every cell a query returned.
//
// If this fails on a DO UPDATE case with "exec error disagreement / go: <nil> /
// cgo: FOREIGN KEY constraint failed", the mask reaching opUpsertStore is the
// suspect: engine/vdbe_write.go's emitUpsertTail must put its colIdx into the
// updatePlan it hands OpUpsertStore, because fkColMask(n, nil) is an ALL-FALSE
// mask and fkChildSide reads that as "the SET list assigned nothing".
func TestR39BUpsertForeignKeys(t *testing.T) {
	for _, tc := range r39bUpsertFKCases {
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
