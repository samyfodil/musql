package server

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// TestUploadAndRestoreErrors: a chunk that waits for its upload's lock gives
// up when its call ends, a full disk is reported as such and any other disk
// failure as internal, an upload id that is not one is unknown, and a database
// being restored is unavailable.
func TestUploadAndRestoreErrors(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	up := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme"}))).Msg
	unlock, err := e.s.uploads.lock(ctx, up.Upload)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := dbs.UploadChunk(short, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: up.Upload, Content: []byte("x")})); err == nil {
		t.Error("a chunk went in while its upload was locked")
	}
	unlock()

	if err := e.s.diskErr("upload chunk", fmt.Errorf("write: %w", syscall.ENOSPC)); code(err) != connect.CodeResourceExhausted {
		t.Errorf("a full disk: %v", err)
	}
	if err := e.s.diskErr("upload chunk", errors.New("the disk went away")); code(err) != connect.CodeInternal {
		t.Errorf("another disk failure: %v", err)
	}
	if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "x", Source: &v1.CreateDatabaseRequest_Upload{Upload: "not an id!"}})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an upload id that is not one: %v", err)
	}
	if err := restoreErr(errUnavailable); code(err) != connect.CodeUnavailable {
		t.Errorf("a database being restored: %v", err)
	}
}
