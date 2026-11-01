package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

func TestAuthAndUserRPCs(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	users, _, _ := e.clients()
	authc := cloudv1connect.NewAuthServiceClient(e.ts.Client(), e.ts.URL)

	if _, err := authc.WhoAmI(ctx, auth("", &v1.WhoAmIRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("whoami without a token: %v", err)
	}
	if me := must(authc.WhoAmI(ctx, auth(bob, &v1.WhoAmIRequest{}))); me.Msg.User.Email != "bob@example.com" || len(me.Msg.Memberships) != 2 {
		t.Fatalf("whoami: %v", me.Msg)
	}

	if _, err := authc.CreatePlatformToken(ctx, auth(bob, &v1.CreatePlatformTokenRequest{Name: " "})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a token without a name: %v", err)
	}
	tok := must(authc.CreatePlatformToken(ctx, auth(bob, &v1.CreatePlatformTokenRequest{Name: "ci"})))
	if tok.Msg.Secret == "" {
		t.Fatal("a token without a secret")
	}
	if l := must(authc.ListPlatformTokens(ctx, auth(bob, &v1.ListPlatformTokensRequest{}))); len(l.Msg.Tokens) != 1 || l.Msg.Tokens[0].Name != "ci" {
		t.Fatalf("bob's tokens: %v", l.Msg)
	}
	must(authc.RevokePlatformToken(ctx, auth(bob, &v1.RevokePlatformTokenRequest{Id: tok.Msg.Token.Id})))
	if l := must(authc.ListPlatformTokens(ctx, auth(bob, &v1.ListPlatformTokensRequest{}))); len(l.Msg.Tokens) != 0 {
		t.Fatalf("a revoked token is listed: %v", l.Msg)
	}
	if _, err := authc.ListPlatformTokens(ctx, auth("", &v1.ListPlatformTokensRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("tokens without a token: %v", err)
	}

	for name, req := range map[string]*v1.CreateUserRequest{
		"no email":       {Email: "nobody", Password: "long-enough"},
		"short password": {Email: "c@example.com", Password: "short"},
	} {
		if _, err := users.Create(ctx, auth(e.admin, req)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := users.Create(ctx, auth(e.admin, &v1.CreateUserRequest{Email: "bob@example.com", Password: "bob-password"})); err == nil {
		t.Fatal("a second user took a taken email")
	}
	carl := must(users.Create(ctx, auth(e.admin, &v1.CreateUserRequest{Email: "carl@example.com", Name: "Carl", Password: "carl-password"}))).Msg.User
	if l := must(users.List(ctx, auth(e.admin, &v1.ListUsersRequest{}))); len(l.Msg.Users) != 3 {
		t.Fatalf("%d users", len(l.Msg.Users))
	}

	adminID := must(e.s.Store().UserByEmail(ctx, "admin@example.com")).ID
	no, renamed := false, "Carl B"
	if _, err := users.Update(ctx, auth(e.admin, &v1.UpdateUserRequest{Id: adminID, Admin: &no})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("an admin demotes itself: %v", err)
	}
	if _, err := users.Update(ctx, auth(e.admin, &v1.UpdateUserRequest{Id: "nobody", Name: &renamed})); code(err) != connect.CodeNotFound {
		t.Fatalf("updating no one: %v", err)
	}
	yes := true
	if u := must(users.Update(ctx, auth(e.admin, &v1.UpdateUserRequest{Id: carl.Id, Name: &renamed, Admin: &yes}))); u.Msg.User.Name != "Carl B" || !u.Msg.User.Admin {
		t.Fatalf("updated to %v", u.Msg.User)
	}
	must(users.Update(ctx, auth(e.admin, &v1.UpdateUserRequest{Id: carl.Id, Disabled: &yes})))

	if _, err := users.ResetPassword(ctx, auth(e.admin, &v1.ResetPasswordRequest{Id: carl.Id, Password: "x"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a short new password: %v", err)
	}
	must(users.ResetPassword(ctx, auth(e.admin, &v1.ResetPasswordRequest{Id: carl.Id, Password: "carl-password-2"})))
	if _, err := users.ResetPassword(ctx, auth(bob, &v1.ResetPasswordRequest{Id: carl.Id, Password: "carl-password-3"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a non-admin resets a password: %v", err)
	}

	if _, err := users.Delete(ctx, auth(e.admin, &v1.DeleteUserRequest{Id: adminID})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("an admin deletes itself: %v", err)
	}
	must(users.Delete(ctx, auth(e.admin, &v1.DeleteUserRequest{Id: carl.Id})))
	if _, err := users.Delete(ctx, auth(bob, &v1.DeleteUserRequest{Id: carl.Id})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a non-admin deletes: %v", err)
	}
}

func TestGroupRPCs(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	groups := cloudv1connect.NewGroupServiceClient(e.ts.Client(), e.ts.URL)

	if l := must(groups.List(ctx, auth(bob, &v1.ListGroupsRequest{Org: "acme"}))); len(l.Msg.Groups) != 1 || l.Msg.Groups[0].Name != "default" {
		t.Fatalf("acme's groups: %v", l.Msg)
	}
	if _, err := groups.Create(ctx, auth(bob, &v1.CreateGroupRequest{Org: "acme", Name: "eu"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer creates a group: %v", err)
	}
	if _, err := groups.Create(ctx, auth(e.admin, &v1.CreateGroupRequest{Org: "acme", Name: "EU!"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a bad group name: %v", err)
	}
	if _, err := groups.Create(ctx, auth(e.admin, &v1.CreateGroupRequest{Org: "acme", Name: "eu", Location: "mars"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("an unknown location: %v", err)
	}
	if g := must(groups.Create(ctx, auth(e.admin, &v1.CreateGroupRequest{Org: "acme", Name: "eu"}))); g.Msg.Group.Primary != "fra" {
		t.Fatalf("a group's primary: %q", g.Msg.Group.Primary)
	}

	if _, err := groups.AddLocation(ctx, auth(e.admin, &v1.AddLocationRequest{Org: "acme", Group: "eu", Location: "mars"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("adding an unknown location: %v", err)
	}
	if _, err := groups.AddLocation(ctx, auth(e.admin, &v1.AddLocationRequest{Org: "acme", Group: "nope", Location: "fra"})); code(err) != connect.CodeNotFound {
		t.Fatalf("adding to no group: %v", err)
	}
	if _, err := groups.RemoveLocation(ctx, auth(e.admin, &v1.RemoveLocationRequest{Org: "acme", Group: "eu", Location: "fra"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("removing the primary: %v", err)
	}
	if _, err := groups.RemoveLocation(ctx, auth(e.admin, &v1.RemoveLocationRequest{Org: "acme", Group: "nope", Location: "fra"})); code(err) != connect.CodeNotFound {
		t.Fatalf("removing from no group: %v", err)
	}

	if _, err := groups.ListLocations(ctx, auth("", &v1.ListLocationsRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("locations without a token: %v", err)
	}
	if l := must(groups.ListLocations(ctx, auth(bob, &v1.ListLocationsRequest{}))); len(l.Msg.Locations) != 1 || l.Msg.Locations[0].Code != "fra" {
		t.Fatalf("locations: %v", l.Msg)
	}

	if _, err := groups.Delete(ctx, auth(e.admin, &v1.DeleteGroupRequest{Org: "acme", Name: "nope"})); code(err) != connect.CodeNotFound {
		t.Fatalf("deleting no group: %v", err)
	}
	must(groups.Delete(ctx, auth(e.admin, &v1.DeleteGroupRequest{Org: "acme", Name: "eu"})))
}
