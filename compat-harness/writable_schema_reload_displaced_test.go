package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestWritableSchemaReloadDisplacedIndexes verifies that writable_schema=RESET
// correctly detects when indexes are bound to wrong b-trees and declines appropriately.
func TestWritableSchemaReloadDisplacedIndexes(t *testing.T) {
	type step struct {
		sql   string
		serve bool
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{
			name: "pragma.test 3.40: two index rootpages swapped",
			steps: []step{
				{`CREATE TABLE t1( a INTEGER PRIMARY KEY, b TEXT COLLATE nocase, c INT COLLATE nocase, d TEXT )`, true},
				{`INSERT INTO t1(a,b,c,d) VALUES (1,'one','one','one'),(2,'two','two','two'),(3,'three','three','three'),(4,'four','four','four'),(5,'five','five','five')`, true},
				{`CREATE INDEX t1bcd ON t1(b,c,d)`, true},
				{`CREATE TABLE t2( a INTEGER PRIMARY KEY, b TEXT COLLATE nocase, c INT COLLATE nocase, d TEXT )`, true},
				{`INSERT INTO t2(a,b,c,d) VALUES (1,'one','one','one'),(2,'two','two','TWO'),(3,'three','THREE','three'),(4,'FOUR','four','four'),(5,'FIVE','FIVE','five')`, true},
				{`CREATE INDEX t2bcd ON t2(b,c,d)`, true},
				{`CREATE TEMP TABLE saved_schema AS SELECT name, rootpage FROM sqlite_schema`, true},
				{`PRAGMA writable_schema=ON`, true},
				{`UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM saved_schema WHERE name='t2bcd') WHERE name='t1bcd'`, true},
				{`UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM saved_schema WHERE name='t1bcd') WHERE name='t2bcd'`, true},
				{`PRAGMA Writable_schema=RESET`, true},
				{`SELECT integrity_check AS x FROM pragma_integrity_check ORDER BY 1`, true},
				// pragma_integrity_check with argument not yet supported.
				{`SELECT integrity_check AS x FROM pragma_integrity_check('t1') ORDER BY 1`, false},
				{`PRAGMA integrity_check(t1)`, true},
				{`PRAGMA quick_check`, true},
				// Answered through the swapped index by C SQLite, so they
				// must be refused here.
				{`SELECT a, d FROM t1 WHERE b='two'`, false},
				{`SELECT * FROM t1 ORDER BY b, c, d`, false},
				{`SELECT * FROM t2 NOT INDEXED WHERE d='TWO'`, false},
				{`INSERT INTO t1 VALUES(6, 'six', 'six', 'six')`, false},
				{`DELETE FROM t1 WHERE a=2`, false},
				{`REINDEX`, false},
				{`SELECT type, name, tbl_name FROM sqlite_schema ORDER BY name`, true},
				{`SELECT rootpage FROM sqlite_schema WHERE name='t1bcd'`, false},
				{`DROP TABLE t2`, true},
				{`SELECT integrity_check AS x FROM pragma_integrity_check ORDER BY 1`, true},
				{`CREATE TABLE t5(x)`, true},
				{`INSERT INTO t5 VALUES(1)`, true},
				{`SELECT * FROM t5`, true},
				{`SELECT * FROM t1`, false},
				{`DROP INDEX t1bcd`, true},
				{`SELECT * FROM t1 ORDER BY a`, true},
				{`PRAGMA integrity_check`, true},
			},
		},
		{
			name: "fkey1.test 8.2: autoindex row renamed onto its sibling's name",
			steps: []step{
				{`CREATE TABLE t1(a REFERENCES sqlite_stat1 ON DELETE CASCADE)`, true},
				{`CREATE TABLE t2(a TEXT PRIMARY KEY)`, true},
				{`PRAGMA writable_schema=ON`, true},
				{`CREATE TABLE sqlite_stat1(tbl INTEGER PRIMARY KEY DESC, idx UNIQUE DEFAULT NULL) WITHOUT ROWID`, true},
				{`UPDATE sqlite_schema SET name='sqlite_autoindex_sqlite_stat1_1' WHERE name='sqlite_autoindex_sqlite_stat1_2'`, true},
				{`PRAGMA writable_schema=RESET`, true},
				// Served steps first: every corruption error below leaves
				// C SQLite's connection in a state of its own.
				{`SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY name`, true},
				{`CREATE TABLE t3(x)`, true},
				{`INSERT INTO t3 VALUES(7)`, true},
				{`SELECT * FROM t3`, true},
				{`SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY name`, true},
				// Corrupted index access must be refused.
				{`SELECT * FROM sqlite_stat1`, false},
				{`INSERT INTO t1 VALUES(1)`, false},
				{`PRAGMA integrity_check`, false},
				{`REINDEX`, false},
				{`SELECT * FROM sqlite_stat1 WHERE idx='x'`, false},
				{`DROP INDEX sqlite_autoindex_t2_1`, false},
				{`DROP TABLE t2`, false},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			godb, err := engine.Create(filepath.Join(dir, "pure.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
			if err != nil {
				t.Fatal(err)
			}
			cgodb.SetMaxOpenConns(1) // writable_schema is per-CONNECTION state
			defer cgodb.Close()

			for i, st := range tc.steps {
				if strings.Contains(strings.ToLower(st.sql), "writable_schema=reset") {
					_, _, goErr := wsGoRun(godb, st.sql)
					if goErr == nil || !strings.Contains(goErr.Error(), "not reproducible against C SQLite") {
						t.Fatalf("step %d %q: want the out-of-scope decline, got %v", i, st.sql, goErr)
					}
					if _, _, cgoErr := wsCGORun(cgodb, st.sql); cgoErr != nil {
						t.Fatalf("step %d %q: C SQLite refused it too (%v) -- the case no longer measures a decline", i, st.sql, cgoErr)
					}
					return
				}
				goCols, goRows, goErr := wsGoRun(godb, st.sql)
				if goErr != nil {
					if st.serve {
						t.Fatalf("step %d %q: this engine declined a step it must serve: %v", i, st.sql, goErr)
					}
					// Declined here, so C SQLite runs it inside a savepoint
					// it rolls back: a write it performs must not leave the two
					// databases apart for the steps that follow.
					if _, err := cgodb.Exec(`SAVEPOINT probe`); err != nil {
						t.Fatal(err)
					}
					_, _, cgoErr := wsCGORun(cgodb, st.sql)
					// A corruption error rolls C SQLite's whole transaction
					// back, savepoint included, so these may fail with nothing
					// left to undo.
					cgodb.Exec(`ROLLBACK TO probe`)
					cgodb.Exec(`RELEASE probe`)
					t.Logf("step %d %q: declined (C SQLite: %v)", i, st.sql, cgoErr)
					continue
				}
				cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, st.sql)
				switch {
				case cgoErr != nil:
					t.Fatalf("step %d %q: C SQLite errored (%v) where this engine answered %v %v", i, st.sql, cgoErr, goCols, goRows)
				case !wsSameResult(goCols, goRows, cgoCols, cgoRows):
					t.Fatalf("step %d %q diverges:\n  pure: %v %v\n  real: %v %v", i, st.sql, goCols, goRows, cgoCols, cgoRows)
				}
			}
		})
	}
}
