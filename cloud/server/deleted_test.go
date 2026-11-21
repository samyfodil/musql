package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/state"
)

func ptrTo[T any](v T) *T { return &v }

// TestDeleteKeepsUntilRetention: a deleted database is closed and its name
// free, comes back with its data and history and new tokens, and is removed
// once its retention ends.
func TestDeleteKeepsUntilRetention(t *testing.T) {
	e := newEnv(t, "deleted_retention: 2h\n")
	bob := e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	e.exec("/db/acme/app", tok, "CREATE TABLE t(a)", "INSERT INTO t VALUES (1)")
	before := e.mark("acme", "app")
	e.exec("/db/acme/app", tok, "INSERT INTO t VALUES (2)")
	id := must(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"}))).Msg.Database.Id

	if _, err := dbs.Delete(ctx, auth(bob, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer's delete: %v", err)
	}
	deletedAt := time.Now()
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})))
	if status, _ := e.pipeline("/db/acme/app", tok, "SELECT 1"); status != http.StatusNotFound {
		t.Fatalf("Hrana for a deleted database: %d", status)
	}
	d, err := e.s.Store().DeletedDatabaseByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.s.dbPath(&d.Database)); err != nil {
		t.Fatalf("a deleted database's file: %v", err)
	}
	l := must(dbs.ListDeleted(ctx, auth(bob, &v1.ListDeletedRequest{Org: "acme"}))).Msg
	if len(l.Databases) != 1 || l.Databases[0].Database.Id != id || l.Databases[0].Database.Url != "" || l.Databases[0].Database.Group != "default" {
		t.Fatalf("listed: %v", l)
	}
	if exp := l.Databases[0].ExpiresAt.AsTime(); exp.Sub(deletedAt) < 2*time.Hour-2*time.Second || exp.Sub(deletedAt) > 2*time.Hour+2*time.Second {
		t.Fatalf("expires at %v, deleted at %v", exp, deletedAt)
	}

	// Its name is free; restoring it then needs another.
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app"})))
	if _, err := dbs.RestoreDeleted(ctx, auth(bob, &v1.RestoreDeletedRequest{Org: "acme", Id: id})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer's restore: %v", err)
	}
	if _, err := dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: id})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("restore under a taken name: %v", err)
	}
	if _, err := dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: "nope"})); code(err) != connect.CodeNotFound {
		t.Fatalf("restore of nothing: %v", err)
	}
	r := must(dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: id, NewName: "back"}))).Msg
	if len(r.Databases) != 1 || r.Databases[0].Name != "back" || r.Databases[0].Id != id {
		t.Fatalf("restored: %v", r)
	}
	if status, _ := e.pipeline("/db/acme/back", tok, "SELECT 1"); status != http.StatusUnauthorized {
		t.Fatalf("a token from before the delete: %d", status)
	}
	if res := e.exec("/db/acme/back", e.admin, "SELECT count(*) FROM t"); res[0].Response.Result.Rows[0][0].Value != "2" {
		t.Fatalf("restored rows: %+v", res)
	}
	// Its history came back with it.
	must(dbs.Restore(ctx, auth(e.admin, &v1.RestoreRequest{Org: "acme", Name: "back", At: timestamppb.New(before.at), NewName: "before"})))
	if got := e.exportDump("acme", "before"); got != before.want {
		t.Fatalf("history after restore:\n%s\nwant:\n%s", got, before.want)
	}
	e.exec("/db/acme/back", e.admin, "INSERT INTO t VALUES (3)")

	// Past its retention it is gone, files and record.
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "back"})))
	if err := e.s.sweepDeleted(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.s.dbPath(&d.Database)); err != nil {
		t.Fatalf("removed before its retention: %v", err)
	}
	if err := e.s.sweepDeleted(ctx, time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.s.dbPath(&d.Database)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted database's file past its retention: %v", err)
	}
	if _, err := os.Stat(e.s.histDir(&d.Database)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted database's history past its retention: %v", err)
	}
	if _, err := dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: id})); code(err) != connect.CodeNotFound {
		t.Fatalf("restore past the retention: %v", err)
	}

	// The org's retention, set by the hook, decides.
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "before"})))
	e.s.deletedRetention = func(_ context.Context, o *state.Org) time.Duration {
		if o.Slug == "acme" {
			return 0
		}
		return time.Hour
	}
	if l := must(dbs.ListDeleted(ctx, auth(e.admin, &v1.ListDeletedRequest{Org: "acme"}))).Msg; len(l.Databases) != 0 {
		t.Fatalf("listed past the org's retention: %v", l)
	}
	before2 := must(e.s.Store().DeletedDatabases(ctx, ""))
	if _, err := dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: before2[0].ID})); code(err) != connect.CodeNotFound {
		t.Fatalf("restore past the org's retention: %v", err)
	}
}

// TestExpiredNotRestored: a deleted database a cluster node's sweep marked
// past its retention, which keeps its record a while after removing its
// files, is neither listed nor restored when the retention grows.
func TestExpiredNotRestored(t *testing.T) {
	e := newEnv(t, "deleted_retention: 2h\n")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	id := must(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"}))).Msg.Database.Id
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})))
	if err := e.s.Store().ExpireDeletedDatabase(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.s.deletedRetention = func(context.Context, *state.Org) time.Duration { return 30 * 24 * time.Hour }
	if l := must(dbs.ListDeleted(ctx, auth(e.admin, &v1.ListDeletedRequest{Org: "acme"}))).Msg; len(l.Databases) != 0 {
		t.Fatalf("an expired database listed: %v", l)
	}
	if _, err := dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: id})); code(err) != connect.CodeNotFound {
		t.Fatalf("restore an expired database: %v", err)
	}
}

// TestDeleteProtection: a protected database or group is refused, and so is
// the org holding it.
func TestDeleteProtection(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	_, orgs, dbs := e.clients()
	groups := cloudv1connect.NewGroupServiceClient(e.ts.Client(), e.ts.URL)

	if _, err := dbs.Update(ctx, auth(bob, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", DeleteProtection: ptrTo(true)})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer's update: %v", err)
	}
	u := must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", DeleteProtection: ptrTo(true)}))).Msg
	if !u.Database.DeleteProtection || u.Database.BlockReads || u.Database.BlockWrites {
		t.Fatalf("updated: %v", u)
	}
	if _, err := dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete a protected database: %v", err)
	}
	if _, err := groups.Delete(ctx, auth(e.admin, &v1.DeleteGroupRequest{Org: "acme", Name: "default"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete a group holding a protected database: %v", err)
	}
	if _, err := orgs.Delete(ctx, auth(e.admin, &v1.DeleteOrgRequest{Org: "acme"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete an org holding a protected database: %v", err)
	}
	// An update that leaves it out keeps it.
	must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockWrites: ptrTo(false)})))
	if g := must(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"}))).Msg; !g.Database.DeleteProtection {
		t.Fatalf("protection lost: %v", g)
	}
	must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", DeleteProtection: ptrTo(false)})))

	gu := must(groups.Update(ctx, auth(e.admin, &v1.UpdateGroupRequest{Org: "acme", Name: "default", DeleteProtection: ptrTo(true)}))).Msg
	if !gu.Group.DeleteProtection || gu.Group.Id == "" {
		t.Fatalf("group updated: %v", gu)
	}
	if _, err := groups.Delete(ctx, auth(e.admin, &v1.DeleteGroupRequest{Org: "acme", Name: "default"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete a protected group: %v", err)
	}
	if _, err := orgs.Delete(ctx, auth(e.admin, &v1.DeleteOrgRequest{Org: "acme"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete an org holding a protected group: %v", err)
	}
	must(groups.Update(ctx, auth(e.admin, &v1.UpdateGroupRequest{Org: "acme", Name: "default", DeleteProtection: ptrTo(false)})))
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})))

	// An org's delete takes its deleted databases along.
	d := must(e.s.Store().DeletedDatabases(ctx, ""))[0]
	must(orgs.Delete(ctx, auth(e.admin, &v1.DeleteOrgRequest{Org: "acme"})))
	if _, err := os.Stat(e.s.dbPath(&d.Database)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted database's file once its org is deleted: %v", err)
	}
	if err := e.s.sweepDeleted(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Store().DeletedDatabaseByID(ctx, d.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("a deleted database's record once its org is deleted: %v", err)
	}
}

// TestDeleteGroupAndRestore: deleting a group deletes its databases, which
// come back with it.
func TestDeleteGroupAndRestore(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	groups := cloudv1connect.NewGroupServiceClient(e.ts.Client(), e.ts.URL)
	must(groups.Create(ctx, auth(e.admin, &v1.CreateGroupRequest{Org: "acme", Name: "eu"})))
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "shop", Group: "eu"})))
	e.exec("/db/acme/shop", e.admin, "CREATE TABLE t(a)", "INSERT INTO t VALUES (1), (2)")

	must(groups.Delete(ctx, auth(e.admin, &v1.DeleteGroupRequest{Org: "acme", Name: "eu"})))
	if status, _ := e.pipeline("/db/acme/shop", e.admin, "SELECT 1"); status != http.StatusNotFound {
		t.Fatalf("Hrana for a database deleted with its group: %d", status)
	}
	l := must(dbs.ListDeleted(ctx, auth(e.admin, &v1.ListDeletedRequest{Org: "acme"}))).Msg
	if len(l.Groups) != 1 || len(l.Databases) != 1 || l.Databases[0].DeletedWithGroup != l.Groups[0].Group.Id || l.Databases[0].Database.Group != "eu" {
		t.Fatalf("listed: %v", l)
	}
	if _, err := dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: l.Databases[0].Database.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("restore a database whose group is deleted: %v", err)
	}
	r := must(dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: l.Groups[0].Group.Id}))).Msg
	if r.Group.GetName() != "eu" || len(r.Databases) != 1 || r.Databases[0].Name != "shop" {
		t.Fatalf("restored: %v", r)
	}
	if res := e.exec("/db/acme/shop", e.admin, "SELECT count(*) FROM t"); res[0].Response.Result.Rows[0][0].Value != "2" {
		t.Fatalf("restored rows: %+v", res)
	}
}

// TestBlocks: blocked reads and writes are refused over Hrana with the code
// BLOCKED, on a stream already open too.
func TestBlocks(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	e.exec("/db/acme/app", e.admin, "CREATE TABLE t(a)", "INSERT INTO t VALUES (1)")

	must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockWrites: ptrTo(true)})))
	_, res := e.pipeline("/db/acme/app", e.admin, "SELECT count(*) FROM t", "INSERT INTO t VALUES (2)", "PRAGMA query_only = 0", "DELETE FROM t")
	if res[0].Type != "ok" || res[1].Error.Code != "BLOCKED" || res[3].Error.Code != "BLOCKED" {
		t.Fatalf("writes blocked: %+v", res)
	}
	must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockWrites: ptrTo(false), BlockReads: ptrTo(true)})))
	if _, res := e.pipeline("/db/acme/app", e.admin, "SELECT 1"); res[0].Error.Code != "BLOCKED" {
		t.Fatalf("reads blocked: %+v", res)
	}
	must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockReads: ptrTo(false)})))
	if res := e.exec("/db/acme/app", e.admin, "INSERT INTO t VALUES (3)", "SELECT count(*) FROM t"); res[1].Response.Result.Rows[0][0].Value != "2" {
		t.Fatalf("unblocked: %+v", res)
	}
}
