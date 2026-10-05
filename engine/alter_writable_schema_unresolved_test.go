package engine

import (
	"path/filepath"
	"testing"
)

// TestRenameLeavesUnresolvableObjectsAlone tests ALTER RENAME with unresolvable schemas.
func TestRenameLeavesUnresolvableObjectsAlone(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "ws.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE t4(id INTEGER PRIMARY KEY, c1 INT, c2 INT)`,
		// View with unresolvable column.
		`CREATE VIEW t4v1 AS SELECT id, c1, c99 FROM t4`,
		// Trigger with unresolvable table reference.
		`CREATE TRIGGER r3 AFTER INSERT ON t1 BEGIN
			INSERT INTO t3(x,y) VALUES(new.a, new.b);
			INSERT INTO t4(c1) VALUES(new.b);
		END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	before := map[string]string{}
	snapSQL := func(into map[string]string) {
		for _, v := range db.views {
			into["view:"+v.name] = v.sql
		}
		for _, tr := range db.triggers {
			into["trigger:"+tr.name] = tr.sql
		}
	}
	snapSQL(before)

	if err := db.Exec(`PRAGMA writable_schema=ON`); err != nil {
		t.Fatalf("writable_schema: %v", err)
	}
	if err := db.Exec(`ALTER TABLE t4 RENAME TO t4new`); err != nil {
		t.Fatalf("ALTER TABLE t4 RENAME TO t4new: %v", err)
	}

	after := map[string]string{}
	snapSQL(after)
	for k, was := range before {
		if now := after[k]; now != was {
			t.Errorf("%s was rewritten but cannot be re-parsed, so C SQLite leaves it alone (alter.c:1884)\n  before: %s\n  after:  %s", k, was, now)
		}
	}
}
