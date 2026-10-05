package engine

import (
	"strings"
	"testing"
)

// The per-row uniqueness check (checkUniqueIndexesForRow, and findRowConflicts'
// probe of the store's conflict index) replaced re-validating the whole index
// on every row written. A probe that merely FAILED to find a conflict would
// still pass every statement-level test that expects no error, so these cases
// are built to reach every way it could: duplicate keys, NULL keys, DESC and
// NOCASE key columns, a composite key, and values whose TYPES order differently
// from their text (SQLite's INTEGER < TEXT).

// TestUniqueIndexStillEnforcedPerRow is the statement-level half: every shape
// whose violation used to be found by re-validating the whole index.
func TestUniqueIndexStillEnforcedPerRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		acts  []string
		wantE string
		dump  string
		rows  []string
	}{
		{
			name:  "UPDATE onto an existing key",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a)`, `INSERT INTO t VALUES(1,10),(2,20)`},
			acts:  []string{`UPDATE t SET a=10 WHERE id=2`},
			wantE: "UNIQUE constraint failed: t.a",
		},
		{
			name:  "INSERT onto an existing key",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a)`, `INSERT INTO t VALUES(1,10)`},
			acts:  []string{`INSERT INTO t VALUES(2,10)`},
			wantE: "UNIQUE constraint failed: t.a",
		},
		{
			name:  "NOCASE index sees 'A' and 'a' as one key",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a COLLATE NOCASE)`, `INSERT INTO t VALUES(1,'a')`},
			acts:  []string{`INSERT INTO t VALUES(2,'A')`},
			wantE: "UNIQUE constraint failed: t.a",
		},
		{
			name:  "composite key, only both columns together conflict",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a, b)`, `CREATE UNIQUE INDEX ia ON t(a,b)`, `INSERT INTO t VALUES(1,1,1)`},
			acts:  []string{`INSERT INTO t VALUES(2,1,2)`, `INSERT INTO t VALUES(3,1,1)`},
			wantE: "UNIQUE constraint failed: t.a, t.b",
		},
		{
			name:  "expression index still enforced (probe declines, scan runs)",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a, b)`, `CREATE UNIQUE INDEX ia ON t(a+b)`, `INSERT INTO t VALUES(1,1,2)`},
			acts:  []string{`INSERT INTO t VALUES(2,2,1)`},
			wantE: "UNIQUE constraint failed: index 'ia'",
		},
		{
			name:  "partial index still enforced (probe declines, scan runs)",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a, b)`, `CREATE UNIQUE INDEX ia ON t(a) WHERE b>0`, `INSERT INTO t VALUES(1,5,1)`},
			acts:  []string{`INSERT INTO t VALUES(2,5,1)`},
			wantE: "UNIQUE constraint failed: t.a",
		},
		{
			name:  "partial index: a row outside the WHERE is not in the index",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a, b)`, `CREATE UNIQUE INDEX ia ON t(a) WHERE b>0`, `INSERT INTO t VALUES(1,5,-1)`},
			acts:  []string{`INSERT INTO t VALUES(2,5,-1)`},
			dump:  `SELECT id,a FROM t ORDER BY id`,
			rows:  []string{"1,5", "2,5"},
		},
		{
			// The regression the probe could most plausibly introduce: a row's
			// OWN index entry is under its key, so a rewrite that leaves the key
			// alone must not read as a conflict with itself.
			name:  "UPDATE that leaves the key alone is not a self-conflict",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a, b)`, `CREATE UNIQUE INDEX ia ON t(a)`, `INSERT INTO t VALUES(1,10,0),(2,20,0)`},
			acts:  []string{`UPDATE t SET b=b+1`},
			dump:  `SELECT id,a,b FROM t ORDER BY id`,
			rows:  []string{"1,10,1", "2,20,1"},
		},
		{
			name:  "UPDATE that moves a key onto a freed one",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a)`, `INSERT INTO t VALUES(1,10),(2,20)`},
			acts:  []string{`DELETE FROM t WHERE id=1`, `UPDATE t SET a=10 WHERE id=2`},
			dump:  `SELECT id,a FROM t ORDER BY id`,
			rows:  []string{"2,10"},
		},
		{
			name:  "NULL keys never conflict",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a)`},
			acts:  []string{`INSERT INTO t VALUES(1,NULL),(2,NULL),(3,NULL)`},
			dump:  `SELECT count(*) FROM t`,
			rows:  []string{"3"},
		},
		{
			name:  "INTEGER and TEXT with the same text do not conflict",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a)`, `INSERT INTO t VALUES(1,2)`},
			acts:  []string{`INSERT INTO t VALUES(2,'2')`},
			dump:  `SELECT id,typeof(a) FROM t ORDER BY id`,
			rows:  []string{"1,integer", "2,text"},
		},
		{
			name:  "ON CONFLICT REPLACE resolves against the index",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a)`, `INSERT INTO t VALUES(1,10),(2,20)`},
			acts:  []string{`INSERT OR REPLACE INTO t VALUES(3,10)`},
			dump:  `SELECT id,a FROM t ORDER BY id`,
			rows:  []string{"2,20", "3,10"},
		},
		{
			name:  "ON CONFLICT IGNORE resolves against the index",
			setup: []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a)`, `CREATE UNIQUE INDEX ia ON t(a)`, `INSERT INTO t VALUES(1,10)`},
			acts:  []string{`INSERT OR IGNORE INTO t VALUES(2,10)`},
			dump:  `SELECT id,a FROM t ORDER BY id`,
			rows:  []string{"1,10"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dump := tc.dump
			if dump == "" {
				dump = `SELECT count(*) FROM t`
			}
			got, err := execScript(t, tc.setup, tc.acts, dump)
			if tc.wantE != "" {
				if err == nil {
					t.Fatalf("expected %q, got rows %v", tc.wantE, got)
				}
				if !strings.Contains(err.Error(), tc.wantE) {
					t.Fatalf("expected %q, got %v", tc.wantE, err)
				}
				return
			}
			wantRows(t, tc.name, got, err, tc.rows...)
		})
	}
}
