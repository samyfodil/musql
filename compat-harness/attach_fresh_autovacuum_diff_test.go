// This file tests auto_vacuum behavior on fresh ATTACH statements.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// closeAndReadAutoVacuum closes both sides and re-opens the aux files directly.
func closeAndReadAutoVacuum(t *testing.T, p *attachPair, goAuxPath, cgoAuxPath string) (goVal, cgoVal int64) {
	t.Helper()
	if err := p.godb.Close(); err != nil {
		t.Fatalf("engine Close: %v", err)
	}
	if err := p.cgo.Close(); err != nil {
		t.Fatalf("cgo Close: %v", err)
	}

	gp, err := engine.Open(goAuxPath)
	if err != nil {
		t.Fatalf("engine.Open(%s): %v", goAuxPath, err)
	}
	defer gp.Close()
	_, rows, err := gp.QueryArgs("PRAGMA auto_vacuum", nil)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("engine PRAGMA auto_vacuum: rows=%v err=%v", rows, err)
	}
	goVal = rows[0][0].I

	cdb, err := sql.Open("sqlite3", cgoAuxPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s): %v", cgoAuxPath, err)
	}
	defer cdb.Close()
	if err := cdb.QueryRow("PRAGMA auto_vacuum").Scan(&cgoVal); err != nil {
		t.Fatalf("cgo PRAGMA auto_vacuum: %v", err)
	}
	return
}

// TestAttachFreshAutoVacuum tests auto_vacuum on a fresh ATTACH.
func TestAttachFreshAutoVacuum(t *testing.T) {
	for _, mode := range []string{"incremental", "full", "1", "2"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			goAux := filepath.Join(dir, "go-aux.db")
			cgoAux := filepath.Join(dir, "cgo-aux.db")
			p := newAttachPair(t, []string{goAux}, []string{cgoAux})
			p.agreeExec("ATTACH DATABASE '{0}' AS aux")
			p.agreeExec("PRAGMA aux.auto_vacuum=" + mode)

			goVal, cgoVal := closeAndReadAutoVacuum(t, p, goAux, cgoAux)
			if goVal != cgoVal {
				t.Errorf("mode=%s: engine auto_vacuum=%d, cgo auto_vacuum=%d", mode, goVal, cgoVal)
			}
			if goVal == 0 {
				t.Errorf("mode=%s: expected an immediate conversion (case 1), got auto_vacuum=0 on both", mode)
			}
		})
	}
}

// TestAttachPreexistingAutoVacuumIsIgnoredLikeCSQLite is the negative control.
func TestAttachPreexistingAutoVacuumIsIgnoredLikeCSQLite(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "preexisting", "PRAGMA user_version=7")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH DATABASE '{0}' AS aux")
	p.agreeExec("PRAGMA aux.auto_vacuum=incremental")
	goVal, cgoVal := closeAndReadAutoVacuum(t, p, goAux, cgoAux)
	if goVal != cgoVal {
		t.Errorf("after the ignored setter: engine auto_vacuum=%d, cgo auto_vacuum=%d", goVal, cgoVal)
	}
	if goVal != 0 {
		t.Errorf("the setter must be IGNORED on a pre-existing page 1, got auto_vacuum=%d", goVal)
	}
}

// TestAttachIntermediateWriteIsIgnoredLikeCSQLite tests statements that make ambiguity real.
func TestAttachIntermediateWriteIsIgnoredLikeCSQLite(t *testing.T) {
	cases := []struct {
		name     string
		prologue string
	}{
		{"user_version write", "PRAGMA aux.user_version=7"},
		{"switch into wal", "PRAGMA aux.journal_mode=wal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			goAux := filepath.Join(dir, "go-aux.db")
			cgoAux := filepath.Join(dir, "cgo-aux.db")
			p := newAttachPair(t, []string{goAux}, []string{cgoAux})
			p.agreeExec("ATTACH DATABASE '{0}' AS aux")
			p.agreeExec(c.prologue)
			p.agreeExec("PRAGMA aux.auto_vacuum=incremental")
			goVal, cgoVal := closeAndReadAutoVacuum(t, p, goAux, cgoAux)
			if goVal != cgoVal {
				t.Errorf("%s: engine auto_vacuum=%d, cgo auto_vacuum=%d", c.name, goVal, cgoVal)
			}
			if goVal != 0 {
				t.Errorf("%s: the setter must be IGNORED after a write, got auto_vacuum=%d", c.name, goVal)
			}
		})
	}
}

// TestAttachFreshAutoVacuumSurvivesBenignIntermediateStatements tests benign statements.
func TestAttachFreshAutoVacuumSurvivesBenignIntermediateStatements(t *testing.T) {
	cases := []struct {
		name     string
		prologue string
	}{
		{"page_size", "PRAGMA aux.page_size=2048"},
		{"secure_delete", "PRAGMA aux.secure_delete=1"},
		{"journal_mode persist", "PRAGMA aux.journal_mode=persist"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			goAux := filepath.Join(dir, "go-aux.db")
			cgoAux := filepath.Join(dir, "cgo-aux.db")
			p := newAttachPair(t, []string{goAux}, []string{cgoAux})
			p.agreeExec("ATTACH DATABASE '{0}' AS aux")
			p.agreeExec(c.prologue)
			p.agreeExec("PRAGMA aux.auto_vacuum=incremental")

			goVal, cgoVal := closeAndReadAutoVacuum(t, p, goAux, cgoAux)
			if goVal != cgoVal {
				t.Errorf("%s: engine auto_vacuum=%d, cgo auto_vacuum=%d", c.name, goVal, cgoVal)
			}
			if goVal == 0 {
				t.Errorf("%s: expected an immediate conversion (case 1), got auto_vacuum=0 on both", c.name)
			}
		})
	}
}

// TestAttachRealWriteBeforeAutoVacuumUnaffected is the "actual write" half of
// the task's own list: a real CREATE TABLE (which necessarily makes
// pageOneOnly's own PRE-EXISTING schemaCookie/len(tables) checks fail, wholly
// independent of this fix's new flag) must keep behaving exactly as it did
// before this change -- accepted, with the mode remembered rather than
// applied (C SQLite's own case 3, and not an error on either side, so the
// resulting file stays auto_vacuum=0 on both).
func TestAttachRealWriteBeforeAutoVacuumUnaffected(t *testing.T) {
	dir := t.TempDir()
	goAux := filepath.Join(dir, "go-aux.db")
	cgoAux := filepath.Join(dir, "cgo-aux.db")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH DATABASE '{0}' AS aux")
	p.agreeExec("CREATE TABLE aux.t(a)")
	p.agreeExec("PRAGMA aux.auto_vacuum=incremental")

	goVal, cgoVal := closeAndReadAutoVacuum(t, p, goAux, cgoAux)
	if goVal != cgoVal {
		t.Errorf("engine auto_vacuum=%d, cgo auto_vacuum=%d", goVal, cgoVal)
	}
	if goVal != 0 {
		t.Errorf("expected the setter to be remembered rather than applied (case 3), got auto_vacuum=%d on both", goVal)
	}
}
