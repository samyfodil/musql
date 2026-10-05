package engine

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestResolveAttachURIPathVFSParameter tests vfs= parameter handling.
// Unknown VFS names are errors, while known but unsupported names are declines.
func TestResolveAttachURIPathVFSParameter(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}

	unknownErr := db.Exec(`ATTACH 'file:` + filepath.Join(dir, "a.musq") + `?vfs=tvfs2' AS a`)
	if unknownErr == nil {
		t.Fatalf("vfs=tvfs2: expected an error, got none")
	}
	if errors.Is(unknownErr, errVDBEUnsupported) {
		t.Errorf("vfs=tvfs2: got a DECLINE (%v); want the ordinary \"no such vfs\" error C SQLite itself raises", unknownErr)
	}
	if got, want := unknownErr.Error(), "engine: no such vfs: tvfs2"; got != want {
		t.Errorf("vfs=tvfs2 error = %q, want %q", got, want)
	}

	knownErr := db.Exec(`ATTACH 'file:` + filepath.Join(dir, "b.musq") + `?vfs=unix' AS b`)
	if knownErr == nil {
		t.Fatalf("vfs=unix: expected a decline, got none")
	}
	if !errors.Is(knownErr, errVDBEUnsupported) {
		t.Errorf("vfs=unix: got %v, want a DECLINE (errVDBEUnsupported) -- C SQLite accepts this name, but this engine does not model it", knownErr)
	}
}
