package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// TestCreateFromUploads: an upload can start a database with its schema
// alone, and a C SQLite file that does not import is refused.
func TestCreateFromUploads(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	e.exec("/db/acme/app", e.admin, "CREATE TABLE t(a TEXT)", "INSERT INTO t VALUES ('hello')")
	create := func(name string, schemaOnly bool, content []byte) error {
		up := e.upload("acme", content)
		_, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: name, SchemaOnly: schemaOnly,
			Source: &v1.CreateDatabaseRequest_Upload{Upload: up.Upload}}))
		return err
	}
	if err := create("shape", true, e.export("acme", "app", v1.FileFormat_FILE_FORMAT_MUSQL)); err != nil {
		t.Fatal(err)
	}
	if got := e.exec("/db/acme/shape", e.admin, "SELECT count(*) FROM t"); got[0].Response.Result.Rows[0][0].Value != "0" {
		t.Fatalf("a schema-only database from an upload: %+v", got)
	}
	bad := append(append([]byte{}, sqliteMagic...), bytes.Repeat([]byte{0xff}, 200)...)
	if err := create("bad", false, bad); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a C SQLite file that does not import: %v", err)
	}
}

// TestMoveFileAcrossFilesystems: where a rename cannot move the file, it is
// copied and the source removed; a missing source or a destination already
// there is refused.
func TestMoveFileAcrossFilesystems(t *testing.T) {
	shm, err := os.MkdirTemp("/dev/shm", "movefile")
	if err != nil {
		t.Skip("no /dev/shm here")
	}
	defer os.RemoveAll(shm)
	src, dst := filepath.Join(shm, "src"), filepath.Join(t.TempDir(), "dst")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "data" {
		t.Fatalf("moved %q, %v", b, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("the source stayed: %v", err)
	}
	if err := moveFile(src, dst+"2"); err == nil {
		t.Error("moved a missing file")
	}
	if err := os.WriteFile(src, []byte("again"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err == nil {
		t.Error("moved over a file already there")
	}
}
