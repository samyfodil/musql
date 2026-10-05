package engine

import (
	"path/filepath"
	"testing"
)

// TestPragmaTuningDBAttachedPageSize verifies that pragma tuning for
// an ATTACHed database uses that database's page size, not main's.
func TestPragmaTuningDBAttachedPageSize(t *testing.T) {
	dir := t.TempDir()
	// Both files in our format to verify ATTACHed schema sees its own page size.
	mainSess, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer mainSess.Discard()
	if err := mainSess.Exec(`PRAGMA page_size = 4096`); err != nil {
		t.Fatal(err)
	}
	aux := filepath.Join(dir, "aux.musq")
	// Set a different page size on aux before it writes anything.
	auxSess, err := Create(aux)
	if err != nil {
		t.Fatal(err)
	}
	if err := auxSess.Exec(`PRAGMA page_size = 8192`); err != nil {
		t.Fatal(err)
	}
	// Write to the database to establish its page size.
	if err := auxSess.Exec(`CREATE TABLE auxt(a)`); err != nil {
		t.Fatal(err)
	}
	if err := auxSess.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mainSess.Exec(`ATTACH '` + aux + `' AS aux`); err != nil {
		t.Fatalf("ATTACH: %v", err)
	}
	db := mainSess.DB

	mainDB := db.pragmaTuningDB("main")
	if mainDB.PageSize != 4096 {
		t.Errorf("main PageSize = %d, want 4096", mainDB.PageSize)
	}
	auxTDB := db.pragmaTuningDB("aux")
	if !auxTDB.Attached {
		t.Errorf("aux PragmaTuningDB.Attached = false, want true")
	}
	if auxTDB.PageSize != 8192 {
		t.Errorf("aux PageSize = %d, want 8192 (main's own is 4096 -- this must not be reading main's)", auxTDB.PageSize)
	}

	// Verify the cache-size formula using the ATTACHed database's page size.
	pages, ok := numberOfCachePages(-2000, auxTDB.PageSize)
	if !ok {
		t.Fatalf("numberOfCachePages: ok=false with a known page size")
	}
	if want := int64(245); pages != want {
		t.Errorf("numberOfCachePages(-2000, 8192) = %d, want %d", pages, want)
	}
}
