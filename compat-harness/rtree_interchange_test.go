package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// Tests that r-tree databases are interoperable: either engine can read
// what the other writes by using proper shadow table format.

// rtreeInterchangeWrite builds a multi-level r-tree with enough rows to test
// interior node traversal and bounding box pruning.
func rtreeInterchangeWrite() []string {
	stmts := []string{`CREATE VIRTUAL TABLE r USING rtree(id, x0, x1, y0, y1)`}
	for i := 1; i <= 500; i++ {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO r VALUES(%d,%d.0,%d.0,%d.0,%d.0)", i, i, i+1, i*2, i*2+1))
	}
	return stmts
}

// rtreeInterchangeRead uses range queries to exercise tree traversal and
// bounding box pruning, not just a bare scan.
var rtreeInterchangeRead = []string{
	`SELECT count(*), min(id), max(id), sum(x0), sum(y1) FROM r`,
	`SELECT count(*) FROM r WHERE x0 >= 100.0 AND x1 <= 201.0`,
	`SELECT id FROM r WHERE x0 >= 250.0 AND x0 <= 252.0 ORDER BY id`,
	`SELECT id FROM r WHERE y0 >= 900.0 AND y1 <= 941.0 ORDER BY id`,
	`SELECT id, x0, x1, y0, y1 FROM r WHERE id IN (1, 250, 500) ORDER BY id`,
}

// TestRtreeFileInterchange verifies that both engines produce identical results
// regardless of which wrote the database.
func TestRtreeFileInterchange(t *testing.T) {
	write := rtreeInterchangeWrite()
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "rt.db")
			wrote := runWithDSN(t, writer, dsn, write)
			for i, r := range wrote {
				if kind, _ := r["kind"].(string); kind == "error" {
					t.Fatalf("%s could not run the write script (the comparison would be vacuous): stmt #%d %s", writer, i, write[i])
				}
			}
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, rtreeInterchangeRead))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s wrote it] readers disagree\n  cgo reads:    %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}

// TestRtreeWrittenHereIsIntactToCSQLite closes the loop the other way: this
// engine writes the tree, and C SQLite -- which never saw it built --
// has to integrity_check the FILE, answer through the index, see the three
// shadow tables in its own catalog, and go on WRITING to it.
//
// The catalog assertion is not incidental. While rtree kept a private store,
// "CREATE VIRTUAL TABLE r USING rtree(...)" left ONE sqlite_master row where
// C SQLite leaves FOUR, which is what made sqlite_master itself decline
// here whenever an r-tree existed.
func TestRtreeWrittenHereIsIntactToCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "rt.db")
	write := rtreeInterchangeWrite()
	wrote := runWithDSN(t, "musql", dsn, write)
	for i, r := range wrote {
		if kind, _ := r["kind"].(string); kind == "error" {
			t.Fatalf("musql could not run the write script: stmt #%d %s", i, write[i])
		}
	}

	sdb, err := sql.Open("sqlite3", exportedForOracle(t, dsn))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	var ic string
	if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "ok" {
		t.Errorf("C SQLite reports the file as %q, want ok", ic)
	}

	// The catalog C SQLite expects: the vtab plus its three shadows.
	var names string
	rows, err := sdb.Query(`SELECT group_concat(name) FROM (SELECT name FROM sqlite_master ORDER BY name)`)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for rows.Next() {
		if err := rows.Scan(&names); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	rows.Close()
	if names != "r,r_node,r_parent,r_rowid" {
		t.Errorf("C SQLite sees catalog %q, want \"r,r_node,r_parent,r_rowid\"", names)
	}

	// It must be able to USE the index, not merely scan it...
	var n int
	if err := sdb.QueryRow(`SELECT count(*) FROM r WHERE x0 >= 100.0 AND x1 <= 201.0`).Scan(&n); err != nil {
		t.Fatalf("range query: %v", err)
	}
	if n != 101 {
		t.Errorf("C SQLite's range query found %d rows, want 101", n)
	}

	// ...and go on writing, which exercises its own insert/split against a
	// tree this engine shaped.
	if _, err := sdb.Exec(`INSERT INTO r VALUES(9001, 1000.0, 1001.0, 2000.0, 2001.0)`); err != nil {
		t.Fatalf("C SQLite could not INSERT into a tree this engine built: %v", err)
	}
	if err := sdb.QueryRow(`SELECT count(*) FROM r`).Scan(&n); err != nil {
		t.Fatalf("count after insert: %v", err)
	}
	if n != 501 {
		t.Errorf("after C SQLite's own INSERT the tree holds %d rows, want 501", n)
	}
	if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check after insert: %v", err)
	}
	if ic != "ok" {
		t.Errorf("after C SQLite's own INSERT the file is %q, want ok", ic)
	}
}

// TestRtreeShadowLifecycle gates the operations that must move or remove an
// r-tree's shadow tables ALONG WITH it. Both were live bugs the moment rtree
// started owning shadows at all, and neither is cosmetic:
//
//   - RENAME must carry all three (rtreeRename, rtree.c:3284-3286). Without
//     it the renamed table cannot be READ -- its own reader looks for
//     "<newname>_node" and finds the old name still sitting there.
//   - DROP must remove all three (rtree.c:1083-1085, xDestroy). Without it
//     C SQLite goes on listing them, and re-creating the same name
//     collides with its own leftovers.
func TestRtreeShadowLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"rename carries the shadows", []string{
			`CREATE VIRTUAL TABLE vr USING rtree(id, x0, x1)`,
			`INSERT INTO vr VALUES(1, 0.0, 1.0)`,
			`INSERT INTO vr VALUES(2, 5.0, 6.0)`,
			`ALTER TABLE vr RENAME TO vq`,
			`SELECT name FROM sqlite_master WHERE name LIKE 'v%' ORDER BY name`,
			`SELECT id, x0, x1 FROM vq ORDER BY id`,
			`INSERT INTO vq VALUES(3, 7.0, 8.0)`,
			`SELECT count(*) FROM vq`,
		}},
		{"drop removes the shadows, and the name is reusable", []string{
			`CREATE VIRTUAL TABLE vr USING rtree(id, x0, x1)`,
			`INSERT INTO vr VALUES(1, 0.0, 1.0)`,
			`DROP TABLE vr`,
			`SELECT count(*) FROM sqlite_master WHERE name LIKE 'vr%'`,
			`CREATE VIRTUAL TABLE vr USING rtree(id, x0, x1)`,
			`SELECT count(*) FROM sqlite_master WHERE name LIKE 'vr%'`,
			`SELECT count(*) FROM vr`,
		}},
		{"a temp rtree owns temp shadows", []string{
			`CREATE VIRTUAL TABLE temp.tr USING rtree(id, x0, x1)`,
			`INSERT INTO temp.tr VALUES(1, 0.0, 1.0)`,
			`SELECT name FROM sqlite_temp_master ORDER BY name`,
			`SELECT id, x0, x1 FROM temp.tr`,
		}},
		{"delete and update keep the tree readable", []string{
			`CREATE VIRTUAL TABLE vr USING rtree(id, x0, x1)`,
			`INSERT INTO vr VALUES(1, 0.0, 1.0)`,
			`INSERT INTO vr VALUES(2, 5.0, 6.0)`,
			`INSERT INTO vr VALUES(3, 9.0, 10.0)`,
			`DELETE FROM vr WHERE id = 2`,
			`UPDATE vr SET x1 = 4.0 WHERE id = 1`,
			`SELECT id, x0, x1 FROM vr ORDER BY id`,
			`SELECT count(*) FROM vr WHERE x0 >= 0.0 AND x1 <= 4.0`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "rtreelifecycle/"+tc.name, tc.stmts) })
	}
}

// TestRtreeI32FileInterchange is the same interchange gate for rtree_i32,
// which is a genuinely different code path: its coordinates are 4-byte
// SIGNED INTEGERS, not IEEE singles (rtree.c's RtreeCoord union), and it is
// the module whose declared column type is INT rather than REAL.
//
// The boundary values are the point of the fixture: -2147483648 and
// 2147483647 are exactly the int32 extremes, so a coordinate written or read
// through the float path would come back changed.
func TestRtreeI32FileInterchange(t *testing.T) {
	write := []string{`CREATE VIRTUAL TABLE q USING rtree_i32(id, x0, x1)`}
	for i := 1; i <= 120; i++ { // > 51, so the tree is multi-level
		write = append(write, fmt.Sprintf("INSERT INTO q VALUES(%d,%d,%d)", i, i*10, i*10+5))
	}
	write = append(write, `INSERT INTO q VALUES(999,-2147483648,2147483647)`)

	read := []string{
		`SELECT count(*), sum(x0), sum(x1) FROM q`,
		`SELECT id, x0, x1, typeof(x0) FROM q WHERE id IN (1, 120, 999) ORDER BY id`,
		`SELECT count(*) FROM q WHERE x0 >= 500 AND x1 <= 805`,
		`SELECT id FROM q WHERE x0 >= 1190 AND x1 <= 1205 ORDER BY id`,
	}
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "i32.db")
			wrote := runWithDSN(t, writer, dsn, write)
			for i, r := range wrote {
				if kind, _ := r["kind"].(string); kind == "error" {
					t.Fatalf("%s could not run the write script: stmt #%d %s", writer, i, write[i])
				}
			}
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, read))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s wrote it] readers disagree\n  cgo reads:    %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}
