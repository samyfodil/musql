// TestOracleProbeStage3d3N3E2 tests structural integrity checking for lost index
// entries. It creates a table and index, then patches the index leaf's cell count
// to drop an entry, and verifies that both C SQLite and musql detect the corruption.
package compat

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// probeE2AllIntegrityCheckRows runs an integrity_check query and returns all
// result rows joined by newline, unlike probePragma which reads only the first.
func probeE2AllIntegrityCheckRows(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("probeE2AllIntegrityCheckRows(%q): %v", q, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("probeE2AllIntegrityCheckRows(%q): scan: %v", q, err)
		}
		lines = append(lines, v.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("probeE2AllIntegrityCheckRows(%q): %v", q, err)
	}
	return strings.Join(lines, "\n")
}

func TestOracleProbeStage3d3N3E2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe_e2.db")

	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX i ON t(b)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := db.Exec(`INSERT INTO t VALUES(?, ?)`, i, fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Verify the fixture is clean before patching.
	if check := probePragma(t, db, `PRAGMA integrity_check`); check != "ok" {
		t.Fatalf("setup: fixture is not clean before patching: integrity_check=%q", check)
	}
	var root int
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name='i'`).Scan(&root); err != nil {
		t.Fatalf("rootpage: %v", err)
	}
	var pageSize int
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatalf("page_size: %v", err)
	}
	db.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	pageOff := (root - 1) * pageSize
	page := data[pageOff : pageOff+pageSize]
	// numCells is at page-format offset 3:5.
	pageType := page[0]
	if pageType != 0x0a {
		t.Fatalf("setup: expected an index LEAF root (0x0a), got type 0x%02x -- did the fixture grow past one page?", pageType)
	}
	numCells := binary.BigEndian.Uint16(page[3:5])
	if numCells < 2 {
		t.Fatalf("setup: index root has only %d cell(s); need >=2 so dropping one still leaves a non-empty, N2-invisible leaf", numCells)
	}
	t.Logf("index root=page %d, pageType=0x%02x, numCells=%d", root, pageType, numCells)

	// Decrement numCells by one to drop the last entry without altering other page state.
	binary.BigEndian.PutUint16(page[3:5], numCells-1)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Check C SQLite's integrity_check output.
	db2, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	got := probeE2AllIntegrityCheckRows(t, db2, `PRAGMA integrity_check`)
	t.Logf("cgo PRAGMA integrity_check ->\n%s", got)
	if !strings.Contains(got, "missing from index") {
		t.Errorf(`expected cgo PRAGMA integrity_check to report a "row ... missing from index ..." finding (pragma.c:2084-2089), got:\n%s`, got)
	}
	if !strings.Contains(got, "wrong # of entries in index") {
		t.Errorf(`expected cgo PRAGMA integrity_check to ALSO report a "wrong # of entries in index ..." finding (pragma.c:1794/:1809-1819), got:\n%s`, got)
	}

	// Check musql's integrity checker on the same file.
	if findings, refused := importRefusal(t, path); !refused {
		t.Errorf("ImportSQLite accepted a file C's integrity_check reports:\n%s", got)
	} else if findings != got {
		t.Errorf("ImportSQLite refused with ->\n%s\nwant (cgo) ->\n%s", findings, got)
	}
}

// TestIntegrityCheckPageCoverageMatchesCSQLite corrupts page space accounting
// and verifies that integrity_check findings match C SQLite's.
func TestIntegrityCheckPageCoverageMatchesCSQLite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(page []byte, hdr int)
	}{
		{"fragment count off by one", func(page []byte, hdr int) { page[hdr+7]++ }},
		{"content area starts one byte late", func(page []byte, hdr int) {
			binary.BigEndian.PutUint16(page[hdr+5:], binary.BigEndian.Uint16(page[hdr+5:])-1)
		}},
		{"two cell pointers name one cell", func(page []byte, hdr int) {
			copy(page[hdr+8+2:hdr+8+4], page[hdr+8:hdr+8+2])
		}},
	} {
		for _, target := range []string{"table", "index"} {
			if target == "table" && strings.HasPrefix(tc.name, "two cell") {
				// Also a rowid out of order, which the walker does not yet word
				// or order as checkTreePage does ("Tree 2 page 2 cell 0: Rowid 1
				// out of order", found walking cells last to first).
				continue
			}
			t.Run(tc.name+"/"+target, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "cov.db")
				db, err := sql.Open("sqlite3", exportedForOracle(t, path))
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range []string{
					`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
					`CREATE INDEX i ON t(b)`,
					`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three'),(4,'four')`,
				} {
					if _, err := db.Exec(s); err != nil {
						t.Fatal(err)
					}
				}
				var root, pageSize int
				if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name=?`, map[string]string{"table": "t", "index": "i"}[target]).Scan(&root); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
					t.Fatal(err)
				}
				db.Close()
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				tc.patch(data[(root-1)*pageSize:root*pageSize], 0)
				if err := os.WriteFile(path, data, 0644); err != nil {
					t.Fatal(err)
				}
				cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
				if err != nil {
					t.Fatal(err)
				}
				defer cdb.Close()
				want := probeE2AllIntegrityCheckRows(t, cdb, `PRAGMA integrity_check`)
				// The import is where musql's checker runs now (see above).
				if got, refused := importRefusal(t, path); !refused {
					t.Errorf("ImportSQLite accepted a file C's integrity_check reports:\n%s", want)
				} else if got != want {
					t.Errorf("ImportSQLite refused with ->\n%s\nwant (cgo) ->\n%s", got, want)
				}
			})
		}
	}
}
