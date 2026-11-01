package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// TestOrgRPCs walks the org service's refusals and its answers: who may list,
// rename, delete, invite and change members, and that an org keeps an owner.
func TestOrgRPCs(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	users, orgs, _ := e.clients()

	if _, err := orgs.List(ctx, auth("", &v1.ListOrgsRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("list without a token: %v", err)
	}
	all := must(orgs.List(ctx, auth(e.admin, &v1.ListOrgsRequest{})))
	mine := must(orgs.List(ctx, auth(bob, &v1.ListOrgsRequest{})))
	if len(all.Msg.Orgs) <= len(mine.Msg.Orgs) || len(mine.Msg.Orgs) != 2 {
		t.Fatalf("admin sees %d orgs, bob %d", len(all.Msg.Orgs), len(mine.Msg.Orgs))
	}

	if _, err := orgs.Create(ctx, auth(bob, &v1.CreateOrgRequest{Slug: "Bad Slug"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a bad slug: %v", err)
	}
	if o := must(orgs.Create(ctx, auth(bob, &v1.CreateOrgRequest{Slug: "bobco"}))); o.Msg.Org.Name != "bobco" {
		t.Fatalf("an org without a name is named %q", o.Msg.Org.Name)
	}
	if _, err := orgs.Create(ctx, auth(bob, &v1.CreateOrgRequest{Slug: "bobco"})); err == nil {
		t.Fatal("a second org took a taken slug")
	}

	if _, err := orgs.Update(ctx, auth(bob, &v1.UpdateOrgRequest{Org: "acme", Name: "x"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer renames: %v", err)
	}
	if _, err := orgs.Update(ctx, auth(e.admin, &v1.UpdateOrgRequest{Org: "acme", Name: " "})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("an empty name: %v", err)
	}
	if o := must(orgs.Update(ctx, auth(e.admin, &v1.UpdateOrgRequest{Org: "acme", Name: "Acme Inc"}))); o.Msg.Org.Name != "Acme Inc" {
		t.Fatalf("renamed to %q", o.Msg.Org.Name)
	}

	if _, err := orgs.Invite(ctx, auth(bob, &v1.InviteRequest{Org: "acme", Email: "x@example.com", Role: v1.Role_ROLE_MEMBER})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer invites: %v", err)
	}
	if _, err := orgs.Invite(ctx, auth(e.admin, &v1.InviteRequest{Org: "acme", Email: "nobody", Role: v1.Role_ROLE_MEMBER})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("an invite without an email: %v", err)
	}
	must(users.Create(ctx, auth(e.admin, &v1.CreateUserRequest{Email: "ann@example.com", Name: "Ann", Password: "ann-password"})))
	inv := must(orgs.Invite(ctx, auth(e.admin, &v1.InviteRequest{Org: "acme", Email: "ann@example.com", Role: v1.Role_ROLE_ADMIN})))
	ann := e.tokenFor("ann@example.com")
	if l := must(orgs.ListInvites(ctx, auth(ann, &v1.ListInvitesRequest{}))); len(l.Msg.Invites) != 1 {
		t.Fatalf("ann's invites: %v", l.Msg)
	}
	if l := must(orgs.ListInvites(ctx, auth(e.admin, &v1.ListInvitesRequest{Org: "acme"}))); len(l.Msg.Invites) != 1 {
		t.Fatalf("acme's invites: %v", l.Msg)
	}
	if _, err := orgs.ListInvites(ctx, auth(bob, &v1.ListInvitesRequest{Org: "acme"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer lists invites: %v", err)
	}
	if _, err := orgs.AcceptInvite(ctx, auth(ann, &v1.AcceptInviteRequest{Id: "no-such-invite"})); err == nil {
		t.Fatal("accepted an invite that does not exist")
	}
	if o := must(orgs.AcceptInvite(ctx, auth(ann, &v1.AcceptInviteRequest{Id: inv.Msg.Invite.Id}))); o.Msg.Org.Slug != "acme" {
		t.Fatalf("accepted into %q", o.Msg.Org.Slug)
	}
	if _, err := orgs.Invite(ctx, auth(ann, &v1.InviteRequest{Org: "acme", Email: "o@example.com", Role: v1.Role_ROLE_OWNER})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("an admin invites an owner: %v", err)
	}

	bobID := must(e.s.Store().UserByEmail(ctx, "bob@example.com")).ID
	annID := must(e.s.Store().UserByEmail(ctx, "ann@example.com")).ID
	adminID := must(e.s.Store().UserByEmail(ctx, "admin@example.com")).ID
	if _, err := orgs.UpdateMember(ctx, auth(ann, &v1.UpdateMemberRequest{Org: "acme", UserId: "nobody", Role: v1.Role_ROLE_MEMBER})); code(err) != connect.CodeNotFound {
		t.Fatalf("a member that is not: %v", err)
	}
	if _, err := orgs.UpdateMember(ctx, auth(ann, &v1.UpdateMemberRequest{Org: "acme", UserId: bobID, Role: v1.Role_ROLE_OWNER})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("an admin makes an owner: %v", err)
	}
	if _, err := orgs.UpdateMember(ctx, auth(ann, &v1.UpdateMemberRequest{Org: "acme", UserId: adminID, Role: v1.Role_ROLE_MEMBER})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("an admin demotes an owner: %v", err)
	}
	must(orgs.UpdateMember(ctx, auth(ann, &v1.UpdateMemberRequest{Org: "acme", UserId: bobID, Role: v1.Role_ROLE_MEMBER})))

	if _, err := orgs.RemoveMember(ctx, auth(bob, &v1.RemoveMemberRequest{Org: "acme", UserId: annID})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a member removes another: %v", err)
	}
	if _, err := orgs.RemoveMember(ctx, auth(ann, &v1.RemoveMemberRequest{Org: "acme", UserId: adminID})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("an admin removes an owner: %v", err)
	}
	if _, err := orgs.RemoveMember(ctx, auth(e.admin, &v1.RemoveMemberRequest{Org: "acme", UserId: adminID})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("the last owner leaves: %v", err)
	}
	if _, err := orgs.RemoveMember(ctx, auth(ann, &v1.RemoveMemberRequest{Org: "acme", UserId: "nobody"})); code(err) != connect.CodeNotFound {
		t.Fatalf("removing a member that is not: %v", err)
	}
	must(orgs.RemoveMember(ctx, auth(bob, &v1.RemoveMemberRequest{Org: "acme", UserId: bobID})))
	must(orgs.UpdateMember(ctx, auth(e.admin, &v1.UpdateMemberRequest{Org: "acme", UserId: annID, Role: v1.Role_ROLE_OWNER})))
	must(orgs.RemoveMember(ctx, auth(ann, &v1.RemoveMemberRequest{Org: "acme", UserId: adminID})))
	if m := must(orgs.ListMembers(ctx, auth(ann, &v1.ListMembersRequest{Org: "acme"}))); len(m.Msg.Members) != 1 {
		t.Fatalf("members left: %v", m.Msg)
	}

	if _, err := orgs.Delete(ctx, auth(ann, &v1.DeleteOrgRequest{Org: "ann"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("a personal org deleted: %v", err)
	}
	must(orgs.Delete(ctx, auth(ann, &v1.DeleteOrgRequest{Org: "acme"})))
	if _, err := orgs.ListMembers(ctx, auth(ann, &v1.ListMembersRequest{Org: "acme"})); code(err) != connect.CodeNotFound {
		t.Fatalf("a deleted org: %v", err)
	}
}
