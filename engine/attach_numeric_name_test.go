package engine

import (
	"strings"
	"testing"
)

// TestDetachNumericLiteralName: numeric literals as database names in ATTACH/DETACH.
func TestDetachNumericLiteralName(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(dir + "/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec("ATTACH '" + dir + "/n.db' AS '123'"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DETACH 12.5"); err == nil || !strings.Contains(err.Error(), "no such database: 12.5") {
		t.Errorf("DETACH 12.5: want no such database: 12.5, got %v", err)
	}
	if err := db.Exec("DETACH 0123"); err != nil {
		t.Errorf("DETACH 0123 with '123' attached: want it detached via its value text, got %v", err)
	}
	if err := db.Exec("DETACH 123"); err == nil || !strings.Contains(err.Error(), "no such database: 123") {
		t.Errorf("DETACH 123 after the detach: want no such database: 123, got %v", err)
	}
}
