package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/state"
)

// TestAuthRefusals: a disabled user's API token, an admin call without a
// token, a member whose role cannot be read, a group still holding databases,
// and a role that is not one are refused.
func TestAuthRefusals(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	users, orgs, _ := e.clients()

	carl := must(users.Create(ctx, auth(e.admin, &v1.CreateUserRequest{Email: "carl@example.com", Password: "carl-password"}))).Msg.User
	_, secret, err := e.s.Store().CreatePlatformToken(ctx, carl.Id, "ci")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	must(users.Update(ctx, auth(e.admin, &v1.UpdateUserRequest{Id: carl.Id, Disabled: &yes})))
	if _, err := orgs.List(ctx, auth(secret, &v1.ListOrgsRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("a disabled user's API token: %v", err)
	}
	if _, err := users.List(ctx, auth("", &v1.ListUsersRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("an admin call without a token: %v", err)
	}

	restore := e.breakTable("members")
	if _, err := orgs.ListMembers(ctx, auth(bob, &v1.ListMembersRequest{Org: "acme"})); code(err) != connect.CodeInternal {
		t.Errorf("a member whose role cannot be read: %v", err)
	}
	restore()

	groups := cloudv1connect.NewGroupServiceClient(e.ts.Client(), e.ts.URL)
	if _, err := groups.Delete(ctx, auth(e.admin, &v1.DeleteGroupRequest{Org: "acme", Name: "default"})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a group still holding a database: %v", err)
	}
	if _, err := orgs.Invite(ctx, auth(e.admin, &v1.InviteRequest{Org: "acme", Email: "dee@example.com"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an invite without a role: %v", err)
	}
	if roleToProto(state.Role("emperor")) != v1.Role_ROLE_UNSPECIFIED {
		t.Error("a role that is not one reads back as one")
	}
}
