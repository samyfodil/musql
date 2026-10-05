// Tests UPDATE with UNIQUE conflict resolution (FAIL, IGNORE, REPLACE, etc).
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// updateConflictCases tests UNIQUE conflict scenarios.
var updateConflictCases = []struct{ name, decl, ins, upd string }{
	{"declared FAIL", "b INTEGER UNIQUE ON CONFLICT FAIL", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE t SET b=b+10`},
	{"declared ABORT", "b INTEGER UNIQUE ON CONFLICT ABORT", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE t SET b=b+10`},
	{"declared IGNORE", "b INTEGER UNIQUE ON CONFLICT IGNORE", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE t SET b=b+10`},
	{"declared REPLACE", "b INTEGER UNIQUE ON CONFLICT REPLACE", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE t SET b=b+10`},
	{"declared ROLLBACK", "b INTEGER UNIQUE ON CONFLICT ROLLBACK", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE t SET b=b+10`},
	{"OR FAIL", "b INTEGER UNIQUE", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE OR FAIL t SET b=b+10`},
	{"OR IGNORE", "b INTEGER UNIQUE", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE OR IGNORE t SET b=b+10`},
	{"OR REPLACE", "b INTEGER UNIQUE", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE OR REPLACE t SET b=b+10`},
	// OR REPLACE victim is not one of the updated rows.
	{"OR REPLACE unmatched victim", "b INTEGER UNIQUE", `INSERT INTO t VALUES(1,1),(2,2),(9,11)`, `UPDATE OR REPLACE t SET b=b+10 WHERE id<=2`},
	// Plain path with no action.
	{"plain", "b INTEGER UNIQUE", `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`, `UPDATE t SET b=b+10`},
	// Multi-column UNIQUE constraints.
	{"multi-column", "b INTEGER, c INTEGER, UNIQUE(b,c)", `INSERT INTO t VALUES(1,1,1),(2,2,2)`, `UPDATE t SET b=1, c=1 WHERE id=2`},
}

func TestUpdateConflictParity(t *testing.T) {
	for _, tc := range updateConflictCases {
		t.Run(tc.name, func(t *testing.T) {
			ddl := `CREATE TABLE t(id INTEGER PRIMARY KEY, ` + tc.decl + `)`
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
			for _, s := range []string{ddl, tc.ins} {
				if err := edb.Exec(s); err != nil {
					t.Fatalf("engine setup %s: %v", s, err)
				}
				if _, err := cdb.Exec(s); err != nil {
					t.Fatalf("cgo setup %s: %v", s, err)
				}
			}

			eErr := edb.Exec(tc.upd)
			_, cErr := cdb.Exec(tc.upd)
			if (eErr == nil) != (cErr == nil) {
				t.Errorf("accept/reject disagrees\n  engine=%v\n  cgo=%v", eErr, cErr)
			} else if eErr != nil {
				// SQLite words a UNIQUE violation by COLUMN, never by index.
				got := strings.TrimPrefix(eErr.Error(), "engine: ")
				got = strings.TrimPrefix(got, "UPDATE t: ")
				if got != cErr.Error() {
					t.Errorf("error text mismatch\n  engine: %q\n  cgo:    %q", got, cErr.Error())
				}
			}

			p, err := edb.SnapshotPager()
			if err != nil {
				t.Fatal(err)
			}
			const q = `SELECT * FROM t ORDER BY id`
			eCols, eVals, qErr := p.QueryArgs(q, nil)
			if qErr != nil {
				t.Fatal(qErr)
			}
			cCols, cRows, sErr := cgoSelect(t, cdb, q, nil)
			if sErr != nil {
				t.Fatal(sErr)
			}
			eRows := engineRowsToStrings(eVals)
			if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
				t.Errorf("table contents DIVERGE after %q: %s\n  engine: %v\n  cgo:    %v", tc.upd, reason, eRows, cRows)
			}
		})
	}
}

// TestUpdateConflictFailKeepsChangeCount pins the OTHER half of FAIL: the
// rows applied BEFORE the offending one stay applied and stay counted. Real
// SQLite reports changes()=2 here (rows 1 and 2), with row 3 untouched.
func TestUpdateConflictFailKeepsChangeCount(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, b INTEGER UNIQUE ON CONFLICT FAIL)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
	} {
		if err := edb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	n, err := edb.Update(`UPDATE t SET b=b+10`)
	if err == nil {
		t.Fatal("expected a UNIQUE constraint failure")
	}
	if n != 2 {
		t.Errorf("rows reported updated = %d, want 2 (rows 1 and 2 stay applied under FAIL)", n)
	}
}
