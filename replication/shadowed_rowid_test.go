package replication

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestShadowedRowidIsRefused: a table whose columns take every name of its
// rowid has no key replication can name its rows by, so Open refuses it and a
// store has no key to write its rows by.
func TestShadowedRowidIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "z.db")
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`CREATE TABLE z(rowid, _rowid_, oid)`); err != nil {
		t.Fatal(err)
	}
	db0.Close()
	db, err := Open(context.Background(), path, CRDT(), WithSite("a"))
	if err == nil {
		db.Close()
		t.Fatal("opened a table whose rows have no name")
	}
	if !strings.Contains(err.Error(), "columns shadow every name for the rowid") {
		t.Fatalf("refused for another reason: %v", err)
	}
	if _, err := keyNamesOf(&object{Name: "z", Cols: []column{{Name: "rowid"}, {Name: "_rowid_"}, {Name: "OID"}}}); err == nil {
		t.Fatal("a store keyed a table whose rows have no name")
	}
}
