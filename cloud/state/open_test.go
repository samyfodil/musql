package state

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRefuses: a store will not open a directory, or a database whose
// tables are in the way of its own.
func TestOpenRefuses(t *testing.T) {
	if s, err := Open(t.TempDir()); err == nil {
		s.Close()
		t.Error("opened a directory")
	}
	path := filepath.Join(t.TempDir(), "state.musq")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE users(x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(path); err == nil || !strings.Contains(err.Error(), "migration 1") {
		if s != nil {
			s.Close()
		}
		t.Errorf("a users table in the way: %v", err)
	}
}

// TestReplicatedSchemaStatements: a replicated store runs each schema
// statement once, skips a column already there, and refuses an ALTER that adds
// no column and a statement that fails.
func TestReplicatedSchemaStatements(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := migrations
	defer func() { migrations = old }()
	migrations = []string{`CREATE TABLE t(a); ALTER TABLE t ADD COLUMN b; ALTER TABLE t ADD COLUMN b`}
	if _, err := OpenReplicated(db, "n1"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{`ALTER TABLE t RENAME TO u`, `CREATE TABLE (`} {
		migrations = []string{m}
		if _, err := OpenReplicated(db, "n1"); err == nil {
			t.Errorf("%s: ran", m)
		}
	}
}
