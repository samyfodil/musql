package engine_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestR25RowInNondeterministicElementDeclines verifies that row IN with
// nondeterministic elements is properly declined to avoid wrong answers.
func TestR25RowInNondeterministicElementDeclines(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r25rowin.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, q := range []string{
		"SELECT (abs(random())%2, 1) IN ((0,1),(1,1))",
		"SELECT (abs(random())%2, 1) NOT IN ((0,1),(1,1))",
		"SELECT (1, randomblob(4)) IN ((1,2),(3,4))",
	} {
		var got any
		if err := db.QueryRow(q).Scan(&got); err == nil {
			t.Errorf("%s: answered %v; a row value holding random()/randomblob() is written once per list element, so it must decline", q, got)
		} else if !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("%s: declined with %v, want an unsupported-shape decline", q, err)
		}
	}

	// A ONE-element list writes each element exactly once, so it is still
	// answered -- the guard must not be wider than the duplication it protects.
	var got int
	if err := db.QueryRow("SELECT (abs(random())%2, 1) IN ((0,1),(1,1),(2,1))").Scan(&got); err == nil {
		t.Error("three-element list: expected a decline")
	}
	if err := db.QueryRow("SELECT (1, 2) IN ((1,2),(3,4))").Scan(&got); err != nil {
		t.Fatalf("deterministic row-value IN: %v", err)
	} else if got != 1 {
		t.Errorf("(1,2) IN ((1,2),(3,4)) = %d, want 1", got)
	}
	if err := db.QueryRow("SELECT (abs(random())%2, 1) IN ((0,1))").Scan(&got); err != nil {
		t.Fatalf("single-element list must stay supported: %v", err)
	}
}
