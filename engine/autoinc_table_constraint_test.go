package engine

import (
	"strings"
	"testing"
)

// TestAutoincTableConstraintBoundary verifies that AUTOINCREMENT with DESC or
// COLLATE orderings correctly declines.
func TestAutoincTableConstraintBoundary(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE bad3(x INTEGER, PRIMARY KEY(x DESC AUTOINCREMENT))",
		"CREATE TABLE bad7(x INTEGER, PRIMARY KEY(x COLLATE BINARY AUTOINCREMENT))",
	} {
		if err := db.Exec(s); err == nil || !strings.Contains(err.Error(), "not supported by this write path") {
			t.Errorf("%q: want the unpinned-semantics decline, got %v", s, err)
		}
	}
	// A quoted "autoincrement" is a NAME, not the keyword: the constraint
	// then refers to a (nonexistent) column, an error either way, but it
	// must not be treated as the autoinc marker.
	// "no such column: autoincrement" is C's own wording for it (resolve.c:785,
	// which resolves a table constraint's column list like any other name).
	if err := db.Exec(`CREATE TABLE q(x INTEGER, PRIMARY KEY(x, "autoincrement"))`); err == nil ||
		!strings.Contains(err.Error(), "no such column: autoincrement") {
		t.Errorf("quoted autoincrement: want the no-such-column error, got %v", err)
	}
}
