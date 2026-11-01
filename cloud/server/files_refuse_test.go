package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// TestFileHelpersRefuse: an empty database cannot be made in no directory, a
// file that is not a database fails its integrity check, and an export names a
// format it knows.
func TestFileHelpersRefuse(t *testing.T) {
	if err := createEmpty(filepath.Join(t.TempDir(), "none", "x.musq")); err == nil {
		t.Error("made a database in no directory")
	}
	junk := filepath.Join(t.TempDir(), "junk.musq")
	if err := os.WriteFile(junk, []byte("not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := integrityOK(junk); err == nil {
		t.Error("a file that is not a database passed its integrity check")
	}
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	d := must(e.s.store.Database(ctx, must(e.s.store.OrgBySlug(ctx, "acme")).ID, "app"))
	if _, _, err := e.s.exportFile(ctx, d, v1.FileFormat(99)); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an export in a format it does not know: %v", err)
	}
}
