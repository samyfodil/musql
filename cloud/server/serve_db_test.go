package server

import (
	"context"
	"net/http"
	"testing"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

// TestServeDBRefusals: Hrana answers 404 for an org or a database that is not
// there, and 503 for a database being restored or whose file went missing
// under the server, which AI SQL cannot read either.
func TestServeDBRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	for path, want := range map[string]int{
		"/db/nope/app":  http.StatusNotFound,
		"/db/acme/nope": http.StatusNotFound,
	} {
		if status, _ := e.pipeline(path, e.admin, "SELECT 1"); status != want {
			t.Errorf("%s: %d, want %d", path, status, want)
		}
	}
	ctx := context.Background()
	o, err := e.s.store.OrgBySlug(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.s.store.Database(ctx, o.ID, "app")
	if err != nil {
		t.Fatal(err)
	}
	unblock := e.s.pool.block(d.ID)
	if status, _ := e.pipeline("/db/acme/app", e.admin, "SELECT 1"); status != http.StatusServiceUnavailable {
		t.Errorf("a database being restored: %d", status)
	}
	unblock()
	e.s.pool.drop(d.ID)
	removeDBFiles(e.s.log, e.s.dbPath(d))
	if status, _ := e.pipeline("/db/acme/app", e.admin, "SELECT 1"); status != http.StatusServiceUnavailable {
		t.Fatalf("a database whose file is gone: %d", status)
	}
	e.s.gen = &fakeGen{replies: []string{"SELECT 1;"}}
	ai := cloudv1connect.NewAIServiceClient(e.ts.Client(), e.ts.URL)
	if _, err := ai.GenerateSQL(ctx, auth(e.admin, &v1.GenerateSQLRequest{Org: "acme", Database: "app", Prompt: "x"})); err == nil {
		t.Fatal("generated SQL for a database whose file is gone")
	}
}
