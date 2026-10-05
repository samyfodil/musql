// This file tests declared column collations (COLLATE NOCASE, RTRIM, etc.)
// in WHERE, ORDER BY, and UNIQUE index enforcement, and verifies collations
// persist across Close and reopen.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildCollateColumnDB creates a test database with declared-NOCASE,
// declared-RTRIM, plain BINARY columns, and UNIQUE indexes over them.
func buildCollateColumnDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("collatecol_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	exec := func(sqlText string) {
		t.Helper()
		if err := db.Exec(sqlText); err != nil {
			t.Fatalf("Exec(%s): %v", sqlText, err)
		}
	}

	exec(`CREATE TABLE strs (
		id INTEGER PRIMARY KEY,
		a TEXT COLLATE NOCASE,
		b TEXT COLLATE RTRIM,
		c TEXT
	)`)
	exec(`ALTER TABLE strs ADD COLUMN d TEXT COLLATE RTRIM`)
	exec(`CREATE UNIQUE INDEX idx_a ON strs(a)`)
	exec(`CREATE UNIQUE INDEX idx_c_nocase ON strs(c COLLATE NOCASE)`)

	rows := []struct{ a, b, c, d string }{
		{"abc", "xyz", "one", "p"},
		{"ABC2", "xyz ", "two", "q "},
		{"AbC3", "xyz  ", "three", "r  "},
		{"xyz", "abc", "four", "s"},
	}
	for i, r := range rows {
		exec(fmt.Sprintf(`INSERT INTO strs(id,a,b,c,d) VALUES (%d, '%s', '%s', '%s', '%s')`, i+1, r.a, r.b, r.c, r.d))
	}
	exec(`INSERT INTO strs(id,a,b,c,d) VALUES (5, NULL, NULL, 'five', NULL)`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// collateColumnUnorderedCorpus are queries testing declared-collation column matching.
var collateColumnUnorderedCorpus = []string{
	// Declared NOCASE column (a) as the default WHERE collation.
	"SELECT id FROM strs WHERE a = 'abc'",
	"SELECT id FROM strs WHERE a = 'ABC2'",
	"SELECT id FROM strs WHERE 'ABC2' = a",
	"SELECT id FROM strs WHERE a <> 'abc'",
	"SELECT id FROM strs WHERE a < 'b'",
	"SELECT id FROM strs WHERE a BETWEEN 'AA' AND 'ac'",
	"SELECT id FROM strs WHERE a IN ('abc', 'xyz')",
	// Declared RTRIM column (b) as the default WHERE collation.
	"SELECT id FROM strs WHERE b = 'xyz'",
	"SELECT id FROM strs WHERE b = 'abc'",
	// Plain (default BINARY) column c: unaffected baseline.
	"SELECT id FROM strs WHERE c = 'one'",
	"SELECT id FROM strs WHERE c = 'ONE'",
	// ALTER TABLE ADD COLUMN's own declared RTRIM (d).
	"SELECT id FROM strs WHERE d = 'p'",
	"SELECT id FROM strs WHERE d = 'q'",
	// NULL never (falsely) matches, declared collation notwithstanding.
	"SELECT id FROM strs WHERE a = NULL",
	"SELECT id FROM strs WHERE a IS NULL",
	// CASE WHEN comparing against a declared-collation column.
	"SELECT id, CASE a WHEN 'abc' THEN 1 ELSE 0 END FROM strs",

	// Explicit COLLATE overriding declared column collation.
	"SELECT id FROM strs WHERE a = 'ABC2' COLLATE BINARY",
	"SELECT id FROM strs WHERE a COLLATE BINARY = 'ABC2'",
	"SELECT id FROM strs WHERE c = 'ONE' COLLATE NOCASE",
	"SELECT id FROM strs WHERE c COLLATE NOCASE = 'one'",
}

// collateColumnOrderedCorpus are queries testing ORDER BY with declared collations.
var collateColumnOrderedCorpus = []string{
	"SELECT id FROM strs ORDER BY a, id",
	"SELECT id FROM strs ORDER BY a DESC, id",
	"SELECT id FROM strs ORDER BY c, id", // plain column: unaffected baseline

	// Explicit COLLATE override in ORDER BY.
	"SELECT id FROM strs ORDER BY a COLLATE BINARY, id", // explicit override -> BINARY order
	"SELECT id FROM strs ORDER BY c COLLATE NOCASE, id", // explicit override -> NOCASE order
}

// runCollateColumnCase runs a query and compares engine and oracle results.
func runCollateColumnCase(t *testing.T, p *engine.ReadOnlyPager, cdb *sql.DB, sqlText string, orderSensitive bool) {
	t.Helper()
	eCols, eVals, eErr := p.Query(sqlText)
	cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

	if eErr != nil || cErr != nil {
		t.Errorf("[%s] unexpected error\n  engine=%v\n  cgo=%v", sqlText, eErr, cErr)
		return
	}

	eRows := engineRowsToStrings(eVals)

	if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, orderSensitive); !ok {
		t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
			sqlText, reason, eCols, eRows, cCols, cRows)
	}

	if vCols, vVals, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, orderSensitive); !ok {
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
}

// TestCollateColumnTableParity tests declared collations at page sizes 512 and 4096.
func TestCollateColumnTableParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildCollateColumnDB(t, pageSize)

			cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()

			p, err := engine.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			for _, sqlText := range collateColumnUnorderedCorpus {
				t.Run(sqlText, func(t *testing.T) {
					runCollateColumnCase(t, p, cdb, sqlText, false)
				})
			}
			for _, sqlText := range collateColumnOrderedCorpus {
				t.Run(sqlText, func(t *testing.T) {
					runCollateColumnCase(t, p, cdb, sqlText, true)
				})
			}
		})
	}
}

// TestCollateColumnUniqueIndexRejectsCaseVariant tests UNIQUE index enforcement
// over declared-NOCASE columns, rejecting case variants at page sizes 512 and 4096.
func TestCollateColumnUniqueIndexRejectsCaseVariant(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			enginePath := filepath.Join(t.TempDir(), "unique_engine.sqlite")
			edb, err := engine.Create(enginePath)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer edb.Close()

			cgoPath := filepath.Join(t.TempDir(), "unique_cgo.sqlite")
			cdb, err := sql.Open("sqlite3", cgoPath)
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()

			setup := []string{
				`CREATE TABLE strs (id INTEGER PRIMARY KEY, a TEXT COLLATE NOCASE, c TEXT)`,
				`CREATE UNIQUE INDEX idx_a ON strs(a)`,
				`CREATE UNIQUE INDEX idx_c_nocase ON strs(c COLLATE NOCASE)`,
				`INSERT INTO strs(id,a,c) VALUES (1,'abc','one')`,
			}
			for _, s := range setup {
				if err := edb.Exec(s); err != nil {
					t.Fatalf("engine Exec(%s): %v", s, err)
				}
				if _, err := cdb.Exec(s); err != nil {
					t.Fatalf("cgo Exec(%s): %v", s, err)
				}
			}

			cases := []struct {
				name    string
				stmt    string
				wantErr bool
			}{
				{"a case-variant duplicate (declared NOCASE)", `INSERT INTO strs(id,a,c) VALUES (2,'ABC','two')`, true},
				{"a genuinely distinct value", `INSERT INTO strs(id,a,c) VALUES (2,'xyz','two')`, false},
				{"c case-variant duplicate (explicit index COLLATE NOCASE)", `INSERT INTO strs(id,a,c) VALUES (3,'qqq','ONE')`, true},
				{"c genuinely distinct value", `INSERT INTO strs(id,a,c) VALUES (3,'qqq','three')`, false},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					eErr := edb.Exec(c.stmt)
					_, cErr := cdb.Exec(c.stmt)
					if (eErr == nil) != (cErr == nil) {
						t.Fatalf("%s: engine err=%v, cgo err=%v (want both %s)", c.stmt, eErr, cErr,
							map[bool]string{true: "to error", false: "to succeed"}[c.wantErr])
					}
					if (eErr == nil) == c.wantErr {
						t.Fatalf("%s: engine err=%v -- wantErr=%v", c.stmt, eErr, c.wantErr)
					}
				})
			}
		})
	}
}

// TestCollateColumnSurvivesCloseReopen verifies declared collations persist
// across Close and reopen.
func TestCollateColumnSurvivesCloseReopen(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildCollateColumnDB(t, pageSize)

			// First reopen.
			p1, err := engine.Open(path)
			if err != nil {
				t.Fatalf("first Open: %v", err)
			}
			cols, rows, err := p1.Query("SELECT id FROM strs WHERE a = 'ABC2' ORDER BY id")
			if err != nil {
				t.Fatalf("first Open Query: %v", err)
			}
			if err := p1.Close(); err != nil {
				t.Fatalf("first Open Close: %v", err)
			}

			cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()
			cCols, cRows, cErr := cgoSelect(t, cdb, "SELECT id FROM strs WHERE a = 'ABC2' ORDER BY id", nil)
			if cErr != nil {
				t.Fatalf("cgo Query: %v", cErr)
			}
			if ok, reason := queryResultsMatch(cols, engineRowsToStrings(rows), cCols, cRows, true); !ok {
				t.Errorf("first Open DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
					reason, cols, rows, cCols, cRows)
			}

			// Second reopen via OpenWrite (the write path's own schema
			// recovery, engine/writer_open.go), followed by an unrelated
			// mutation and a second Close -- so the NOCASE index/column
			// survive not just a read-only reopen but a full write-mode
			// round trip too, and the declared collation is STILL honored
			// afterward.
			wdb, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			if err := wdb.Exec(`INSERT INTO strs(id,a,b,c,d) VALUES (6,'zzz','zzz','six','z')`); err != nil {
				t.Fatalf("OpenWrite Exec: %v", err)
			}
			if err := wdb.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}

			p2, err := engine.Open(path)
			if err != nil {
				t.Fatalf("second Open: %v", err)
			}
			defer p2.Close()
			cols2, rows2, err := p2.Query("SELECT id FROM strs WHERE a = 'ABC2' ORDER BY id")
			if err != nil {
				t.Fatalf("second Open Query: %v", err)
			}
			if ok, reason := queryResultsMatch(cols2, engineRowsToStrings(rows2), cCols, cRows, true); !ok {
				t.Errorf("post-OpenWrite-roundtrip DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
					reason, cols2, rows2, cCols, cRows)
			}

			// The reopened write handle's UNIQUE index (idx_a, over the
			// declared-NOCASE column a) must still reject a case-variant
			// duplicate, exactly like TestCollateColumnUniqueIndexRejectsCaseVariant
			// verifies for a fresh session.
			wdb2, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (unique check): %v", err)
			}
			defer wdb2.Close()
			if err := wdb2.Exec(`INSERT INTO strs(id,a,b,c,d) VALUES (7,'ABC2','x','seven','x')`); err == nil {
				t.Errorf("expected a UNIQUE violation inserting a NOCASE case-variant of 'ABC2' after reopen, got none")
			}
		})
	}
}
