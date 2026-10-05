package driver_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestAttachMemdbDeclinesCleanly: driver rejects shared memdb ATTACH URIs cleanly.
func TestAttachMemdbDeclinesCleanly(t *testing.T) {
	const memdbPath = "/r39a-driver-1"
	dir := t.TempDir()
	db, err := sql.Open("musql", filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = func() error {
		_, err := db.Exec(`ATTACH 'file:` + memdbPath + `?vfs=memdb' AS aux`)
		return err
	}()
	if err == nil {
		t.Fatal("ATTACH ... vfs=memdb through driver: got no error, want a clean decline")
	}
	if !strings.Contains(err.Error(), "vfs=memdb") {
		t.Errorf("decline error = %q, want it to name vfs=memdb", err.Error())
	}
	// The critical safety property: memdbPath must never have been touched as
	// a literal filesystem path.
	if _, statErr := os.Stat(memdbPath); statErr == nil {
		t.Fatalf("SAFETY: %q was created as a real file on disk -- the memdb name leaked into a literal path open", memdbPath)
		// Best-effort cleanup if this somehow ever fires.
	} else if !os.IsNotExist(statErr) {
		t.Logf("os.Stat(%q) = %v (not IsNotExist, but also not a created file -- likely a permission error, which is fine)", memdbPath, statErr)
	}
}
