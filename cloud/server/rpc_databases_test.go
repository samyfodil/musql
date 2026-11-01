package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// TestDatabaseRPCRefusals: the database service's argument and lookup
// refusals, and the branch, rename and token paths that answer.
func TestDatabaseRPCRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	branchOf := func(name string) *v1.CreateDatabaseRequest_BranchOf {
		return &v1.CreateDatabaseRequest_BranchOf{BranchOf: name}
	}

	if l := must(dbs.List(ctx, auth(e.admin, &v1.ListDatabasesRequest{Org: "acme", Group: "default"}))); len(l.Msg.Databases) != 1 {
		t.Fatalf("the default group's databases: %v", l.Msg)
	}
	if _, err := dbs.List(ctx, auth(e.admin, &v1.ListDatabasesRequest{Org: "acme", Group: "nope"})); code(err) != connect.CodeNotFound {
		t.Fatalf("listing no group: %v", err)
	}
	if _, err := dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "nope"})); code(err) != connect.CodeNotFound {
		t.Fatalf("getting no database: %v", err)
	}

	for name, tc := range map[string]struct {
		req  *v1.CreateDatabaseRequest
		want connect.Code
	}{
		"a bad name":                {&v1.CreateDatabaseRequest{Org: "acme", Name: "Bad Name"}, connect.CodeInvalidArgument},
		"a branch of nothing":       {&v1.CreateDatabaseRequest{Org: "acme", Name: "b", Source: branchOf("nope")}, connect.CodeNotFound},
		"branch_at alone":           {&v1.CreateDatabaseRequest{Org: "acme", Name: "b", BranchAt: timestamppb.Now()}, connect.CodeInvalidArgument},
		"schema_only alone":         {&v1.CreateDatabaseRequest{Org: "acme", Name: "b", SchemaOnly: true}, connect.CodeInvalidArgument},
		"no such group":             {&v1.CreateDatabaseRequest{Org: "acme", Name: "b", Group: "nope"}, connect.CodeNotFound},
		"a name that is taken":      {&v1.CreateDatabaseRequest{Org: "acme", Name: "app"}, connect.CodeAlreadyExists},
		"a branch of a name taken":  {&v1.CreateDatabaseRequest{Org: "acme", Name: "app", Source: branchOf("app")}, connect.CodeAlreadyExists},
		"a branch at a bad instant": {&v1.CreateDatabaseRequest{Org: "acme", Name: "b2", Source: branchOf("app"), BranchAt: timestamppb.New(time.Unix(1, 0))}, connect.CodeFailedPrecondition},
	} {
		if _, err := dbs.Create(ctx, auth(e.admin, tc.req)); code(err) != tc.want {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if b := must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "copy", Source: branchOf("app"), SchemaOnly: true}))); b.Msg.Database.Name != "copy" {
		t.Fatalf("a schema-only branch: %v", b.Msg)
	}

	if _, err := dbs.Rename(ctx, auth(e.admin, &v1.RenameDatabaseRequest{Org: "acme", Name: "nope", NewName: "x"})); code(err) != connect.CodeNotFound {
		t.Fatalf("renaming no database: %v", err)
	}
	if _, err := dbs.Rename(ctx, auth(e.admin, &v1.RenameDatabaseRequest{Org: "acme", Name: "copy", NewName: "Bad Name"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a bad new name: %v", err)
	}
	if _, err := dbs.Rename(ctx, auth(e.admin, &v1.RenameDatabaseRequest{Org: "acme", Name: "copy", NewName: "app"})); err == nil {
		t.Fatal("renamed onto a name that is taken")
	}
	if r := must(dbs.Rename(ctx, auth(e.admin, &v1.RenameDatabaseRequest{Org: "acme", Name: "copy", NewName: "copy2"}))); r.Msg.Database.Name != "copy2" {
		t.Fatalf("renamed to %q", r.Msg.Database.Name)
	}

	for name, tc := range map[string]struct {
		req  *v1.CreateTokenRequest
		want connect.Code
	}{
		"an unknown access":         {&v1.CreateTokenRequest{Org: "acme", Name: "app", Access: v1.Access(99)}, connect.CodeInvalidArgument},
		"no name or group":          {&v1.CreateTokenRequest{Org: "acme"}, connect.CodeInvalidArgument},
		"an expiry gone":            {&v1.CreateTokenRequest{Org: "acme", Name: "app", ExpiresAt: timestamppb.New(time.Now().Add(-time.Hour))}, connect.CodeInvalidArgument},
		"no such database":          {&v1.CreateTokenRequest{Org: "acme", Name: "nope"}, connect.CodeNotFound},
		"no such group":             {&v1.CreateTokenRequest{Org: "acme", Group: "nope"}, connect.CodeNotFound},
		"a token in an org of none": {&v1.CreateTokenRequest{Org: "nope", Name: "app"}, connect.CodeNotFound},
	} {
		if _, err := dbs.CreateToken(ctx, auth(e.admin, tc.req)); code(err) != tc.want {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Group: "default", ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))})))

	for name, tc := range map[string]struct {
		req  *v1.InvalidateTokensRequest
		want connect.Code
	}{
		"no name or group": {&v1.InvalidateTokensRequest{Org: "acme"}, connect.CodeInvalidArgument},
		"no such database": {&v1.InvalidateTokensRequest{Org: "acme", Name: "nope"}, connect.CodeNotFound},
		"no such group":    {&v1.InvalidateTokensRequest{Org: "acme", Group: "nope"}, connect.CodeNotFound},
	} {
		if _, err := dbs.InvalidateTokens(ctx, auth(e.admin, tc.req)); code(err) != tc.want {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	must(dbs.InvalidateTokens(ctx, auth(e.admin, &v1.InvalidateTokensRequest{Org: "acme", Group: "default"})))
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "copy2"})))
}
