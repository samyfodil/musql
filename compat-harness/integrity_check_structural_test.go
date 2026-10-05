// Tests structural integrity checks against C SQLite.
package compat

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// integrityCheckRowKind runs a single query through engine over dsn and
// reports "ok" iff the result is exactly one row holding the single string
// "ok" -- the shape both engines use for a clean PRAGMA integrity_check --
// or a short non-"ok" marker otherwise.
func integrityCheckRowKind(t *testing.T, eng, dsn, q string) string {
	t.Helper()
	res := runWithDSN(t, eng, dsn, []string{q})
	if len(res) != 1 {
		t.Fatalf("[%s] %s: expected exactly 1 statement result, got %d", eng, q, len(res))
	}
	if kind, _ := res[0]["kind"].(string); kind != "rows" {
		t.Fatalf("[%s] %s: expected a row-returning result, got kind=%v (%v)", eng, q, res[0]["kind"], res[0])
	}
	rows, _ := res[0]["rows"].([]any)
	if len(rows) == 1 {
		if row, ok := rows[0].([]any); ok && len(row) == 1 {
			// Cell values are normalized as "T:<text>" etc. (worker/main.go's
			// normalize) to preserve type across the JSON boundary.
			if s, ok := row[0].(string); ok && s == "T:ok" {
				return "ok"
			}
		}
	}
	return fmt.Sprintf("NOT-OK(%d row(s): %v)", len(rows), rows)
}

// TestIntegrityCheckStructuralPartialScopingAgainstOracle gates Stage 2a's
// finding 2: PRAGMA integrity_check(name), naming a single table, must NOT
// run the freelist-chain/full-page-coverage structural cross-check -- only
// the bare/whole-database form does (btree.c:11144-11170's own "aRoot[0]==0"
// partial-check branch). The header's own FreelistPages count is corrupted
// directly (a "leaked" page, invisible to any row-level query -- requirement
// (e) of Stage 2a's spec). C is asked both forms; musql's checker now runs
// inside ImportSQLite, which checks the whole database and must refuse.
func TestIntegrityCheckStructuralPartialScopingAgainstOracle(t *testing.T) {
	// Built by C: musql writes no SQLite pages, so a freelist page only exists in
	// a file C wrote (two sessions, so the DROP's page really reaches the chain).
	dsn := filepath.Join(t.TempDir(), "leak.db")
	runWithDSN(t, "cgo", dsn, []string{
		`CREATE TABLE a(x)`,
		`CREATE TABLE b(y)`,
		`INSERT INTO a VALUES(1)`,
		`INSERT INTO b VALUES(1)`,
	})
	runWithDSN(t, "cgo", dsn, []string{`DROP TABLE a`})

	b, err := os.ReadFile(dsn)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	real := binary.BigEndian.Uint32(b[36:40]) // header offset 36: FreelistPages
	if real == 0 {
		t.Fatalf("test setup did not produce a freelist (FreelistPages == 0)")
	}
	binary.BigEndian.PutUint32(b[36:40], real+7)
	if err := os.WriteFile(dsn, b, 0644); err != nil {
		t.Fatalf("write db: %v", err)
	}

	if got := integrityCheckRowKind(t, "cgo", dsn, `PRAGMA integrity_check`); got == "ok" {
		t.Errorf("[cgo] whole-database PRAGMA integrity_check should report the leaked freelist page, got %q", got)
	}
	if got := integrityCheckRowKind(t, "cgo", dsn, `PRAGMA integrity_check(b)`); got != "ok" {
		t.Errorf("[cgo] PRAGMA integrity_check(b) (single-object form) should skip the freelist check per btree.c:11144-11170, got %q", got)
	}
	// musql's checker runs at the import now, over the WHOLE database -- the
	// form that reports the leak -- so the import refuses the file.
	if _, refused := importRefusal(t, dsn); !refused {
		t.Error("ImportSQLite accepted a file whose whole-database integrity_check reports a leaked freelist page")
	}
}

// TestIntegrityCheckStructuralAutoVacuumCleanAgainstOracle gates Stage 2a's
// requirement (c): a legitimately auto-vacuum'd database must never be
// false-flagged (its pointer-map pages mistaken for orphans/leaks) --
// checked both directions (a database musql itself wrote with
// PRAGMA auto_vacuum=FULL, and one cgo wrote), read by both engines.
func TestIntegrityCheckStructuralAutoVacuumCleanAgainstOracle(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "av.db")
			runWithDSN(t, writer, dsn, []string{
				`PRAGMA auto_vacuum=FULL`,
				`CREATE TABLE t(a,b)`,
				`CREATE INDEX i ON t(b)`,
				`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
			})
			for _, reader := range engineOrder {
				reader := reader
				t.Run("read-by-"+reader, func(t *testing.T) {
					// The other engine's file through the converter: C checks the
					// pointer map the EXPORT built, which is the part that can go wrong.
					if got := integrityCheckRowKind(t, reader, pathForReader(t, reader, dsn), `PRAGMA integrity_check`); got != "ok" {
						t.Fatalf("[written by %s, read by %s] expected a clean auto-vacuum database, got %q", writer, reader, got)
					}
				})
			}
		})
	}
}
