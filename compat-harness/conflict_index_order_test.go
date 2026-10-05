// Tests that conflicting unique constraints are reported in the correct order.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

func TestConflictIndexOrder_TwoUniqueConstraints_NewestReportedFirst(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go-main.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo-main.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cgodb.Close()

	setup := []string{
		`CREATE TABLE t2(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)`,
		`INSERT INTO t2 VALUES(1,'x','p')`,
		`INSERT INTO t2 VALUES(2,'y','q')`,
	}
	for _, s := range setup {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("go setup %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}

	// b conflicts with row 1 (the OLDER index), c conflicts with row 2 (the
	// NEWER index) -- C SQLite reports the newer index's constraint (c)
	// first.
	const conflictSQL = `INSERT INTO t2 VALUES(3,'x','q')`
	_, _, goErr := godb.ExecArgs(conflictSQL, nil)
	_, cgoErr := cgodb.Exec(conflictSQL)
	if goErr == nil || cgoErr == nil {
		t.Fatalf("expected both sides to error, got go=%v cgo=%v", goErr, cgoErr)
	}
	got := strings.TrimPrefix(goErr.Error(), "engine: ")
	if !strings.HasSuffix(got, cgoErr.Error()) {
		t.Fatalf("error text mismatch\n  engine: %q\n  cgo:    %q", got, cgoErr.Error())
	}
}

func TestConflictIndexOrder_TwoUniqueConstraints_FailKeepsNewestOrder(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go-main.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo-main.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cgodb.Close()

	setup := []string{
		`CREATE TABLE t2(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)`,
		`INSERT INTO t2 VALUES(1,'x','p')`,
		`INSERT INTO t2 VALUES(2,'y','q')`,
	}
	for _, s := range setup {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("go setup %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}

	const conflictSQL = `INSERT OR FAIL INTO t2 VALUES(3,'x','q')`
	_, _, goErr := godb.ExecArgs(conflictSQL, nil)
	_, cgoErr := cgodb.Exec(conflictSQL)
	if goErr == nil || cgoErr == nil {
		t.Fatalf("expected both sides to error, got go=%v cgo=%v", goErr, cgoErr)
	}
	got := strings.TrimPrefix(goErr.Error(), "engine: ")
	if !strings.HasSuffix(got, cgoErr.Error()) {
		t.Fatalf("error text mismatch\n  engine: %q\n  cgo:    %q", got, cgoErr.Error())
	}
}
