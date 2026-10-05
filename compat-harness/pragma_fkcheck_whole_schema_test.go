// Tests PRAGMA foreign_key_check with no table specified. The EXEC form is tested
// against the oracle since row order is hash-table dependent.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// fkCheckWholeCases define schema and probe scenarios. mustDecline names probes
// where this engine may be stricter than the oracle.
var fkCheckWholeCases = []struct {
	name        string
	setup       []string
	probes      []string
	mustDecline map[string]bool
}{
	{
		name: "no-foreign-keys-at-all",
		setup: []string{
			`CREATE TABLE a(x)`,
			`CREATE TABLE b(y)`,
			`CREATE VIEW v AS SELECT 1`,
			`INSERT INTO a VALUES(1)`,
		},
		probes: []string{`PRAGMA foreign_key_check`, `PRAGMA main.foreign_key_check`},
	},
	{
		name: "violations-but-no-mismatch",
		setup: []string{
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c1(x REFERENCES p)`,
			`CREATE TABLE c2(x REFERENCES p)`,
			`INSERT INTO p VALUES(1)`,
			`INSERT INTO c1 VALUES(1),(9)`,
			`INSERT INTO c2 VALUES(8),(NULL)`,
		},
		probes: []string{`PRAGMA foreign_key_check`, `PRAGMA main.foreign_key_check`},
	},
	{
		name: "one-mismatch-poisons-the-whole-statement",
		setup: []string{
			`CREATE TABLE p(a, b)`, // no unique key: any reference to it is a mismatch
			`CREATE TABLE p2(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE good(x REFERENCES p2)`,
			`CREATE TABLE bad(x REFERENCES p(a))`,
			`INSERT INTO good VALUES(9)`,
		},
		probes: []string{`PRAGMA foreign_key_check`, `PRAGMA foreign_key_check(good)`, `PRAGMA foreign_key_check(bad)`},
	},
	{
		name: "the-mismatch-is-gone-once-its-table-is",
		setup: []string{
			`CREATE TABLE p(a, b)`,
			`CREATE TABLE p2(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE good(x REFERENCES p2)`,
			`CREATE TABLE bad(x REFERENCES p(a))`,
			`INSERT INTO good VALUES(9)`,
			`DROP TABLE bad`,
		},
		probes: []string{`PRAGMA foreign_key_check`},
	},
	{
		name: "two-mismatches-that-would-report-different-text",
		setup: []string{
			`CREATE TABLE p(a, b)`,
			`CREATE VIEW pv AS SELECT 1 AS q`,
			`CREATE TABLE bad1(x REFERENCES p(a))`,
			`CREATE TABLE bad2(x REFERENCES pv(q))`,
		},
		// Both engines still ERROR -- this engine's is the decline, real
		// SQLite's is whichever mismatch its hash order reached first -- so the
		// agreement below holds without either side guessing.
		probes: []string{`PRAGMA foreign_key_check`},
	},
	{
		name: "virtual-tables-and-their-shadows-are-skipped",
		setup: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(z)`,
			`INSERT INTO ft VALUES('hello world')`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(x REFERENCES p)`,
			`INSERT INTO c VALUES(4)`,
		},
		probes: []string{`PRAGMA foreign_key_check`},
	},
	{
		name: "unqualified-is-main-only-with-temp-objects-live",
		setup: []string{
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE cm(x REFERENCES p)`,
			`INSERT INTO cm VALUES(7)`,
			`CREATE TEMP TABLE tp(id INTEGER PRIMARY KEY)`,
			`CREATE TEMP TABLE tc(x REFERENCES tp)`,
			`INSERT INTO tc VALUES(8)`,
		},
		probes: []string{
			`PRAGMA foreign_key_check`,
			`PRAGMA main.foreign_key_check`,
			`PRAGMA temp.foreign_key_check`,
			`PRAGMA foreign_key_check(tc)`,
		},
	},
	{
		name: "a-temp-only-mismatch-is-invisible-to-the-main-form",
		setup: []string{
			`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE cm(x REFERENCES p)`,
			`CREATE TEMP TABLE tnu(z)`,
			`CREATE TEMP TABLE tbad(x REFERENCES tnu(z))`,
		},
		probes: []string{
			`PRAGMA foreign_key_check`,
			`PRAGMA main.foreign_key_check`,
			`PRAGMA temp.foreign_key_check`,
		},
	},
	{
		name: "a-missing-parent-table-is-not-an-error",
		setup: []string{
			`CREATE TABLE c(x REFERENCES nosuchparent, y)`,
			`INSERT INTO c VALUES(1,1),(NULL,2)`,
		},
		probes: []string{`PRAGMA foreign_key_check`},
	},
	{
		name: "empty-schema",
		setup: []string{
			`CREATE TABLE q(a)`,
			`DROP TABLE q`,
		},
		probes: []string{`PRAGMA foreign_key_check`},
	},
}

// TestForeignKeyCheckWholeDatabaseExec runs each scenario's setup against both
// engines statement by statement, then requires every probe's ERROR DECISION to
// match. That is the whole contract of the exec form: C SQLite discards the
// rows there too, so the only observable is whether the statement failed.
func TestForeignKeyCheckWholeDatabaseExec(t *testing.T) {
	for _, tc := range fkCheckWholeCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fkwhole.sqlite")
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()

			sdb, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()
			sdb.SetMaxOpenConns(1) // one logical connection: temp objects must persist

			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("engine Exec(%s): %v", s, err)
				}
				if _, err := sdb.Exec(s); err != nil {
					t.Fatalf("C SQLite Exec(%s): %v", s, err)
				}
			}
			for _, s := range tc.probes {
				goErr := db.Exec(s)
				_, cErr := sdb.Exec(s)
				if tc.mustDecline[s] {
					if goErr == nil {
						t.Errorf("Exec(%s): expected this engine to decline, it answered", s)
					}
					continue
				}
				if (goErr != nil) != (cErr != nil) {
					t.Errorf("Exec(%s): engine err=%v, C SQLite err=%v (must agree)", s, goErr, cErr)
				}
			}
			// Everything after the pragma must still work identically -- a
			// reporter that left the write session disturbed would show here.
			for _, s := range []string{
				`CREATE TABLE after_t(a, b)`,
				`INSERT INTO after_t VALUES(1,'x'),(2,'y')`,
			} {
				if err := db.Exec(s); err != nil {
					t.Errorf("engine Exec(%s) after the pragma: %v", s, err)
				}
			}
		})
	}
}
