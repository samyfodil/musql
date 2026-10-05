// This file gates foreign key checks on generated columns that depend on updated columns.
// and left y=6 with no parent row in the FILE, where the oracle raises
// "FOREIGN KEY constraint failed". engine/fk.go's fkWidenGeneratedMask is the
// fix, applied in fkRowMutated so every write path gets it.
//
// The negative case is as load-bearing as the positive one: widening EVERY
// generated column instead of only the ones that read an assigned column is
// the opposite wrong answer, raising where the oracle consults nothing.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var r39bGenMaskCases = []struct {
	name  string
	stmts []string
}{
	// p holds 2 and 4, so y=x*2 resolves for x in {1,2} and not for x=3.
	{"update-that-feeds-the-generated-key-is-checked", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`INSERT INTO p VALUES(2),(4)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y AS (x*2) REFERENCES p(k))`,
		`INSERT INTO c(k,x) VALUES(1,1)`,
		`UPDATE c SET x = 3`,
		`SELECT k,x,y FROM c ORDER BY k`,
		`UPDATE c SET x = 2`,
		`SELECT k,x,y FROM c ORDER BY k`,
		`SELECT changes(), total_changes()`,
	}},
	{"stored-generated-key-is-checked-the-same-way", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`INSERT INTO p VALUES(2),(4)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y AS (x*2) STORED REFERENCES p(k))`,
		`INSERT INTO c(k,x) VALUES(1,2)`,
		`UPDATE c SET x = 3`,
		`SELECT k,x,y FROM c ORDER BY k`,
		`SELECT changes(), total_changes()`,
	}},
	// The FIXPOINT: z reads y, y reads x, and only x is assigned. A single pass
	// over the SET list reaches y and stops; SQLite's do/while reaches z.
	{"a-chain-of-generated-columns-is-followed-to-the-end", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`INSERT INTO p VALUES(3),(5)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y AS (x*2), z AS (y+1) REFERENCES p(k))`,
		`INSERT INTO c(k,x) VALUES(1,1)`,
		`UPDATE c SET x = 4`,
		`SELECT k,x,y,z FROM c ORDER BY k`,
		`UPDATE c SET x = 2`,
		`SELECT k,x,y,z FROM c ORDER BY k`,
		`SELECT changes(), total_changes()`,
	}},
	// THE NEGATIVE. The row is an orphan from the start (made while
	// foreign_keys was off): assigning a column the generator does NOT read
	// consults nothing, so it must succeed -- and the orphan stays.
	{"update-that-feeds-nothing-consults-nothing", []string{
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`INSERT INTO p VALUES(4)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, w, y AS (x*2) REFERENCES p(k))`,
		`INSERT INTO c(k,x,w) VALUES(1,1,'a')`,
		`PRAGMA foreign_keys=ON`,
		`UPDATE c SET w = 'b'`,
		`SELECT k,x,w,y FROM c ORDER BY k`,
		`SELECT changes(), total_changes()`,
		`UPDATE c SET x = x`,
		`SELECT k,x,w,y FROM c ORDER BY k`,
	}},
	// The same rule reached through an upsert's DO UPDATE arm, which is an
	// UPDATE (upsert.c:325) and carries the same map.
	{"upsert-do-update-feeding-the-generated-key-is-checked", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`INSERT INTO p VALUES(2),(4)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y AS (x*2) REFERENCES p(k))`,
		`INSERT INTO c(k,x) VALUES(1,1)`,
		`INSERT INTO c(k,x) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET x = 3`,
		`SELECT k,x,y FROM c ORDER BY k`,
		`INSERT INTO c(k,x) VALUES(1,1) ON CONFLICT(k) DO UPDATE SET x = 2`,
		`SELECT k,x,y FROM c ORDER BY k`,
		`SELECT changes(), total_changes()`,
	}},
	// A generated column reading the ROWID ALIAS, moved by the UPDATE:
	// fkChildIsModified's second clause (fkey.c:809) in the generated form.
	{"a-generated-key-over-the-rowid-alias", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`INSERT INTO p VALUES(2),(4)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, y AS (k*2) REFERENCES p(k))`,
		`INSERT INTO c(k,x) VALUES(1,'a')`,
		`UPDATE c SET k = 3`,
		`SELECT k,x,y FROM c ORDER BY k`,
		`UPDATE c SET k = 2`,
		`SELECT k,x,y FROM c ORDER BY k`,
		`SELECT changes(), total_changes()`,
	}},
	// A plain (non-generated) key on the same table must keep its own answer:
	// assigning only the generated key's input leaves the plain key alone.
	{"a-plain-key-beside-a-generated-one-keeps-its-own-filter", []string{
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`CREATE TABLE q(m INTEGER PRIMARY KEY)`,
		`INSERT INTO p VALUES(4)`,
		`CREATE TABLE c(k INTEGER PRIMARY KEY, x, n REFERENCES q(m), y AS (x*2) REFERENCES p(k))`,
		`INSERT INTO c(k,x,n) VALUES(1,2,77)`,
		`PRAGMA foreign_keys=ON`,
		`UPDATE c SET x = 2`,
		`SELECT k,x,n,y FROM c ORDER BY k`,
		`UPDATE c SET n = 77`,
		`SELECT k,x,n,y FROM c ORDER BY k`,
		`SELECT changes(), total_changes()`,
	}},
}

// TestR39BForeignKeyGeneratedColumnMask replays each script against engine.DB
// and C SQLite in lockstep.
//
// If a positive case fails with "go: <nil> / cgo: FOREIGN KEY constraint
// failed", the widening is missing: engine/fk.go's fkWidenGeneratedMask must
// mark a generated column whose generator reads an assigned one, as a FIXPOINT
// (update.c:536-549). If the NEGATIVE case fails the other way, the widening is
// too broad -- it must test what the generator actually reads, not merely that
// the column is generated.
func TestR39BForeignKeyGeneratedColumnMask(t *testing.T) {
	for _, tc := range r39bGenMaskCases {
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
			cgodb.SetMaxOpenConns(1)

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
