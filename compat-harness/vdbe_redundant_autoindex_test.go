// This file tests redundant automatic indexes. When two UNIQUE/PRIMARY KEY
// constraints would build the same index, only one should be created.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// redundantAutoIndexCases tests cases where automatic indexes are redundant.
var redundantAutoIndexCases = []struct{ ddl, ins string }{
	// Same single column named twice -- one index (index-16.1/16.2/16.3).
	{`CREATE TABLE t(c UNIQUE PRIMARY KEY)`, `INSERT INTO t VALUES(1)`},
	{`CREATE TABLE t(c PRIMARY KEY, UNIQUE(c))`, `INSERT INTO t VALUES(1)`},
	{`CREATE TABLE t(c, UNIQUE(c), UNIQUE(c))`, `INSERT INTO t VALUES(1)`},
	// Same multi-column key, either order of declaration (index-16.4).
	{`CREATE TABLE t(c, d, UNIQUE(c,d), PRIMARY KEY(c,d))`, `INSERT INTO t VALUES(1,2)`},
	{`CREATE TABLE t(c, d, PRIMARY KEY(c,d), UNIQUE(c,d))`, `INSERT INTO t VALUES(1,2)`},
	// DESC is NOT part of the equivalence: these still collapse to one.
	{`CREATE TABLE t(c, d, UNIQUE(c,d), PRIMARY KEY(c DESC,d))`, `INSERT INTO t VALUES(1,2)`},
	// NOT redundant: different key columns (index-16.5), different order,
	// and a bare INTEGER PRIMARY KEY (the rowid alias, which builds no
	// automatic index of its own, so UNIQUE(a) is the only one).
	{`CREATE TABLE t(c, d, UNIQUE(c), PRIMARY KEY(c,d))`, `INSERT INTO t VALUES(1,2)`},
	{`CREATE TABLE t(c, d UNIQUE, UNIQUE(c), PRIMARY KEY(c,d))`, `INSERT INTO t VALUES(1,2)`},
	{`CREATE TABLE t(c, d, UNIQUE(c,d), UNIQUE(d,c))`, `INSERT INTO t VALUES(1,2)`},
	{`CREATE TABLE t(a INTEGER PRIMARY KEY, UNIQUE(a))`, `INSERT INTO t VALUES(1)`},
	// WITHOUT ROWID: the surviving index IS the table's own PK b-tree, so
	// the PRIMARY KEY spec has to resolve to it even though the spec that
	// built it was an earlier, redundant UNIQUE (this shape panicked while
	// the dedup still assumed one index per spec -- altercol.test).
	{`CREATE TABLE t(aaa,b,c,UNIQUE(aaA),PRIMARY KEY(aAa),UNIQUE(aAA)) WITHOUT ROWID`, `INSERT INTO t VALUES(1,2,3)`},
	{`CREATE TABLE t(a,b,PRIMARY KEY(a),UNIQUE(a)) WITHOUT ROWID`, `INSERT INTO t VALUES(1,2)`},
	// ON CONFLICT merge: an explicit action on either constraint carries
	// onto the surviving index.
	{`CREATE TABLE t(a PRIMARY KEY, UNIQUE(a) ON CONFLICT IGNORE)`, `INSERT INTO t VALUES(1)`},
	{`CREATE TABLE t(a PRIMARY KEY ON CONFLICT IGNORE, UNIQUE(a))`, `INSERT INTO t VALUES(1)`},
	{`CREATE TABLE t(a PRIMARY KEY ON CONFLICT IGNORE, UNIQUE(a) ON CONFLICT IGNORE)`, `INSERT INTO t VALUES(1)`},
}

// redundantAutoIndexRejected tests conflicting ON CONFLICT clauses.
var redundantAutoIndexRejected = []string{
	`CREATE TABLE t(a PRIMARY KEY ON CONFLICT FAIL, UNIQUE(a) ON CONFLICT IGNORE)`,
	`CREATE TABLE t(a PRIMARY KEY ON CONFLICT ABORT, UNIQUE(a) ON CONFLICT IGNORE)`,
}

const redundantAutoIndexQuery = `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='t' ORDER BY name`

// Test PRAGMA index_list for redundant indexes.
const redundantAutoIndexPragma = `PRAGMA index_list(t)`

func TestRedundantAutoIndexParity(t *testing.T) {
	for _, tc := range redundantAutoIndexCases {
		t.Run(tc.ddl, func(t *testing.T) {
			edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer edb.Close()
			cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()

			if err := edb.Exec(tc.ddl); err != nil {
				t.Fatalf("engine rejected %s: %v", tc.ddl, err)
			}
			if _, err := cdb.Exec(tc.ddl); err != nil {
				t.Fatalf("cgo rejected %s: %v", tc.ddl, err)
			}

			// The duplicate INSERT: both engines must agree on whether it is
			// a violation (the merged ON CONFLICT action).
			for pass := 0; pass < 2; pass++ {
				eErr := edb.Exec(tc.ins)
				_, cErr := cdb.Exec(tc.ins)
				if (eErr != nil) != (cErr != nil) {
					t.Errorf("INSERT pass %d disagrees\n  engine=%v\n  cgo=%v", pass, eErr, cErr)
				}
			}

			p, err := edb.SnapshotPager()
			if err != nil {
				t.Fatal(err)
			}
			eCols, eVals, eErr := p.QueryArgs(redundantAutoIndexQuery, nil)
			if eErr != nil {
				t.Fatal(eErr)
			}
			cCols, cRows, cErr := cgoSelect(t, cdb, redundantAutoIndexQuery, nil)
			if cErr != nil {
				t.Fatal(cErr)
			}
			eRows := engineRowsToStrings(eVals)
			if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
				t.Errorf("sqlite_master index rows DIVERGE: %s\n  engine: %v\n  cgo:    %v", reason, eRows, cRows)
			}

			pCols, pVals, pErr := p.QueryArgs(redundantAutoIndexPragma, nil)
			if pErr != nil {
				t.Fatal(pErr)
			}
			pcCols, pcRows, pcErr := cgoSelect(t, cdb, redundantAutoIndexPragma, nil)
			if pcErr != nil {
				t.Fatal(pcErr)
			}
			pRows := engineRowsToStrings(pVals)
			if ok, reason := queryResultsMatch(pCols, pRows, pcCols, pcRows, true); !ok {
				t.Errorf("PRAGMA index_list DIVERGES: %s\n  engine: %v\n  cgo:    %v", reason, pRows, pcRows)
			}
		})
	}
}

func TestRedundantAutoIndexConflictingOnConflictRejected(t *testing.T) {
	for _, ddl := range redundantAutoIndexRejected {
		t.Run(ddl, func(t *testing.T) {
			edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer edb.Close()
			cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()

			eErr := edb.Exec(ddl)
			_, cErr := cdb.Exec(ddl)
			if eErr == nil || cErr == nil {
				t.Fatalf("both engines must reject\n  engine=%v\n  cgo=%v", eErr, cErr)
			}
			if got, want := eErr.Error(), cErr.Error(); got != want {
				t.Errorf("error text mismatch\n  engine: %q\n  cgo:    %q", got, want)
			}
		})
	}
}

// TestRedundantAutoIndexSurvivesReopen verifies redundancy is preserved on reopen.
func TestRedundantAutoIndexSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t(c UNIQUE PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = engine.OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO t VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The oracle reads the export of the segment file.
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	var n int
	if err := cdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='t'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("after reopen: %d automatic indexes, want 1", n)
	}
	// Verify integrity.
	var ic string
	if err := cdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatal(err)
	}
	if ic != "ok" {
		t.Errorf("integrity_check: %s", ic)
	}
}
