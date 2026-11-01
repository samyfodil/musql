package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/samyfodil/musql/cloud/state"
)

// TestManagementCommands drives the auth, config, org and group commands a
// signed-in admin has, and their refusals.
func TestManagementCommands(t *testing.T) {
	h := start(t)
	if out := h.run("config", "show"); !strings.Contains(out, "signed in: no") {
		t.Fatalf("config show before login: %q", out)
	}
	h.login()
	if out := h.run("config", "show"); !strings.Contains(out, "signed in: yes") || !strings.Contains(out, h.url) {
		t.Fatalf("config show: %q", out)
	}
	if tok := strings.TrimSpace(h.run("auth", "token")); strings.Count(tok, ".") != 2 {
		t.Fatalf("auth token: %q", tok)
	}
	if out := h.run("auth", "whoami", "--json"); !strings.Contains(out, `"email"`) || !strings.Contains(out, `"admin@example.com"`) {
		t.Fatalf("whoami --json: %q", out)
	}

	minted := h.run("auth", "api-tokens", "mint", "ci", "--json")
	if !strings.Contains(minted, `"secret"`) {
		t.Fatalf("mint --json: %q", minted)
	}
	list := h.run("auth", "api-tokens", "list")
	if !strings.Contains(list, "ci") {
		t.Fatalf("api-tokens list: %q", list)
	}
	id := strings.Fields(strings.Split(list, "\n")[1])[0]
	h.run("auth", "api-tokens", "revoke", id)
	if out := h.run("auth", "api-tokens", "list"); strings.Contains(out, " ci ") {
		t.Fatalf("a revoked token is listed: %q", out)
	}

	if _, err := h.try("config", "set", "colour", "blue"); err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("an unknown setting: %v", err)
	}

	h.run("org", "create", "acme")
	if _, err := h.try("org", "switch", "nope"); err == nil || !strings.Contains(err.Error(), "no organization") {
		t.Fatalf("switching to no org: %v", err)
	}
	h.run("config", "set", "org", "acme")
	ctx := context.Background()
	if _, err := h.srv.Store().CreateUser(ctx, "dan@example.com", "Dan", state.HashPassword("dan-password"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.try("org", "members", "invite", "dan@example.com", "--role", "emperor"); err == nil {
		t.Fatal("invited with a role that does not exist")
	}
	if out := h.run("org", "members", "invite", "dan@example.com", "--role", "viewer"); !strings.Contains(out, "Invited dan@example.com to acme as viewer") {
		t.Fatalf("invite: %q", out)
	}
	dan, err := h.srv.Store().UserByEmail(ctx, "dan@example.com")
	if err != nil {
		t.Fatal(err)
	}
	invs, err := h.srv.Store().EmailInvites(ctx, "dan@example.com")
	if err != nil || len(invs) != 1 {
		t.Fatalf("dan's invites: %v %v", invs, err)
	}
	if _, err := h.srv.Store().AcceptInvite(ctx, invs[0].ID, dan); err != nil {
		t.Fatal(err)
	}
	if out := h.run("org", "members", "list"); !strings.Contains(out, "dan@example.com") || !strings.Contains(out, "viewer") {
		t.Fatalf("members: %q", out)
	}
	if _, err := h.try("org", "members", "role", "dan@example.com", "emperor"); err == nil {
		t.Fatal("set a role that does not exist")
	}
	if _, err := h.try("org", "members", "role", "eve@example.com", "member"); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("a role for a non-member: %v", err)
	}
	h.run("org", "members", "role", "dan@example.com", "member")
	h.run("org", "members", "rm", "dan@example.com")
	if out := h.run("org", "members", "list"); strings.Contains(out, "dan@example.com") {
		t.Fatalf("a removed member is listed: %q", out)
	}

	if out := h.run("group", "create", "eu"); !strings.Contains(out, "Created group eu in fra") {
		t.Fatalf("group create: %q", out)
	}
	if out := h.run("group", "locations", "list"); !strings.Contains(out, "fra") {
		t.Fatalf("locations: %q", out)
	}
	if _, err := h.try("group", "locations", "add", "eu", "mars"); err == nil {
		t.Fatal("added an unknown location")
	}
	if _, err := h.try("group", "locations", "add", "eu", "fra"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("adding a location the group has: %v", err)
	}
	if _, err := h.try("group", "locations", "rm", "eu", "fra"); err == nil {
		t.Fatal("removed a group's primary location")
	}
	if _, err := h.try("group", "destroy", "eu"); err == nil || !strings.Contains(err.Error(), "not deleted") {
		t.Fatalf("destroy without confirming: %v", err)
	}
	h.run("group", "destroy", "eu", "--yes")
	if out := h.run("group", "list"); strings.Contains(out, "eu ") {
		t.Fatalf("a destroyed group is listed: %q", out)
	}

	h.run("config", "set", "url", h.url+"/")
	h.run("config", "set", "url", "http://elsewhere.example")
	if out := h.run("config", "show"); !strings.Contains(out, "signed in: no") {
		t.Fatalf("a new server keeps the old tokens: %q", out)
	}
}
