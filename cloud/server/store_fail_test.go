package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

// breakTable renames a table of the server's state out of the way, so the
// store's next use of it fails, and returns what puts it back.
func (e *env) breakTable(table string) (restore func()) {
	e.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.s.cfg.DataDir, "state.musq"))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_broken`); err != nil {
		e.t.Fatal(err)
	}
	return func() {
		if _, err := db.Exec(`ALTER TABLE ` + table + `_broken RENAME TO ` + table); err != nil {
			e.t.Fatal(err)
		}
		db.Close()
	}
}

// TestStoreFailuresAreInternal: a call that passed its checks and then meets a
// store that fails answers Internal, never as though what it asked about were
// not there.
func TestStoreFailuresAreInternal(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	users, orgs, dbs := e.clients()
	groups := cloudv1connect.NewGroupServiceClient(e.ts.Client(), e.ts.URL)
	authc := cloudv1connect.NewAuthServiceClient(e.ts.Client(), e.ts.URL)
	must(orgs.Invite(ctx, auth(e.admin, &v1.InviteRequest{Org: "acme", Email: "ann@example.com", Role: v1.Role_ROLE_MEMBER})))
	must(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "doomed"})))
	adminID := must(e.s.Store().UserByEmail(ctx, "admin@example.com")).ID
	bobID := must(e.s.Store().UserByEmail(ctx, "bob@example.com")).ID
	for _, tc := range []struct {
		table string
		calls map[string]func() error
	}{
		{"platform_tokens", map[string]func() error{
			"list api tokens": func() error {
				return errOf(authc.ListPlatformTokens(ctx, auth(e.admin, &v1.ListPlatformTokensRequest{})))
			},
			"mint an api token": func() error {
				return errOf(authc.CreatePlatformToken(ctx, auth(e.admin, &v1.CreatePlatformTokenRequest{Name: "ci"})))
			},
			"revoke one": func() error {
				return errOf(authc.RevokePlatformToken(ctx, auth(e.admin, &v1.RevokePlatformTokenRequest{Id: "x"})))
			},
		}},
		{"orgs", map[string]func() error{
			"an admin's orgs": func() error { return errOf(orgs.List(ctx, auth(e.admin, &v1.ListOrgsRequest{}))) },
			"a member's orgs": func() error { return errOf(orgs.List(ctx, auth(bob, &v1.ListOrgsRequest{}))) },
		}},
		{"members", map[string]func() error{
			"list members": func() error { return errOf(orgs.ListMembers(ctx, auth(e.admin, &v1.ListMembersRequest{Org: "acme"}))) },
			"change a member": func() error {
				return errOf(orgs.UpdateMember(ctx, auth(e.admin, &v1.UpdateMemberRequest{Org: "acme", UserId: bobID, Role: v1.Role_ROLE_MEMBER})))
			},
		}},
		{"invites", map[string]func() error{
			"delete an org": func() error { return errOf(orgs.Delete(ctx, auth(e.admin, &v1.DeleteOrgRequest{Org: "doomed"}))) },
			"invite": func() error {
				return errOf(orgs.Invite(ctx, auth(e.admin, &v1.InviteRequest{Org: "acme", Email: "bo@example.com", Role: v1.Role_ROLE_MEMBER})))
			},
			"an org's invites":  func() error { return errOf(orgs.ListInvites(ctx, auth(e.admin, &v1.ListInvitesRequest{Org: "acme"}))) },
			"one's own invites": func() error { return errOf(orgs.ListInvites(ctx, auth(e.admin, &v1.ListInvitesRequest{}))) },
			"accept one":        func() error { return errOf(orgs.AcceptInvite(ctx, auth(e.admin, &v1.AcceptInviteRequest{Id: "x"}))) },
		}},
		{"groups", map[string]func() error{
			"list groups": func() error { return errOf(groups.List(ctx, auth(e.admin, &v1.ListGroupsRequest{Org: "acme"}))) },
			"create a group": func() error {
				return errOf(groups.Create(ctx, auth(e.admin, &v1.CreateGroupRequest{Org: "acme", Name: "eu"})))
			},
			"create an org": func() error { return errOf(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "other"}))) },
			"create a user": func() error {
				return errOf(users.Create(ctx, auth(e.admin, &v1.CreateUserRequest{Email: "cy@example.com", Password: "cy-password"})))
			},
			"create a db": func() error {
				return errOf(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "x"})))
			},
		}},
		{"group_locations", map[string]func() error{
			"add a location": func() error {
				return errOf(groups.AddLocation(ctx, auth(e.admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "fra"})))
			},
			"remove a location": func() error {
				return errOf(groups.RemoveLocation(ctx, auth(e.admin, &v1.RemoveLocationRequest{Org: "acme", Group: "default", Location: "x"})))
			},
		}},
		{"databases", map[string]func() error{
			"list dbs": func() error { return errOf(dbs.List(ctx, auth(e.admin, &v1.ListDatabasesRequest{Org: "acme"}))) },
			"record a db": func() error {
				return errOf(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "y"})))
			},
			"get a db": func() error {
				return errOf(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"})))
			},
			"rename a db": func() error {
				return errOf(dbs.Rename(ctx, auth(e.admin, &v1.RenameDatabaseRequest{Org: "acme", Name: "app", NewName: "b"})))
			},
			"a db's token": func() error {
				return errOf(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"})))
			},
			"invalidate tokens": func() error {
				return errOf(dbs.InvalidateTokens(ctx, auth(e.admin, &v1.InvalidateTokensRequest{Org: "acme", Name: "app"})))
			},
			"delete a group": func() error {
				return errOf(groups.Delete(ctx, auth(e.admin, &v1.DeleteGroupRequest{Org: "acme", Name: "default"})))
			},
			"delete an org": func() error { return errOf(orgs.Delete(ctx, auth(e.admin, &v1.DeleteOrgRequest{Org: "acme"}))) },
		}},
		{"refresh_tokens", map[string]func() error{
			"reset a password": func() error {
				return errOf(users.ResetPassword(ctx, auth(e.admin, &v1.ResetPasswordRequest{Id: adminID, Password: "admin-password-2"})))
			},
		}},
		{"usage_hours", map[string]func() error{
			"a db's usage":   func() error { return errOf(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme", Name: "app"}))) },
			"an org's usage": func() error { return errOf(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme"}))) },
		}},
		{"usage_queries", map[string]func() error{
			"a db's stats": func() error { return errOf(dbs.Stats(ctx, auth(e.admin, &v1.StatsRequest{Org: "acme", Name: "app"}))) },
		}},
	} {
		restore := e.breakTable(tc.table)
		for name, f := range tc.calls {
			if err := f(); code(err) != connect.CodeInternal {
				t.Errorf("%s without %s: %v", name, tc.table, err)
			}
		}
		restore()
	}
}

func errOf[T any](_ *connect.Response[T], err error) error { return err }

// TestOIDCStoreFailuresAreServerErrors: the provider answers server_error when
// the store fails under a device code, a revoke or a token it is issuing.
func TestOIDCStoreFailuresAreServerErrors(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	post := func(path string, form url.Values) string {
		t.Helper()
		resp, err := e.ts.Client().PostForm(e.ts.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]string
		json.NewDecoder(resp.Body).Decode(&m)
		return m["error"]
	}
	restore := e.breakTable("device_codes")
	if got := post("/oidc/device", url.Values{"client_id": {"cli"}}); got != "server_error" {
		t.Errorf("a device code without its table: %q", got)
	}
	restore()

	restore = e.breakTable("refresh_tokens")
	if got := post("/oidc/revoke", url.Values{"client_id": {"cli"}, "token": {"x"}}); got != "server_error" {
		t.Errorf("a revoke without its table: %q", got)
	}
	restore()

	var da struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	resp, err := e.ts.Client().PostForm(e.ts.URL+"/oidc/device", url.Values{"client_id": {"cli"}})
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&da)
	resp.Body.Close()
	admin := must(e.s.Store().UserByEmail(ctx, "admin@example.com"))
	if err := e.s.Store().DecideDevice(ctx, da.UserCode, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	restore = e.breakTable("refresh_tokens")
	defer restore()
	if got := post("/oidc/token", url.Values{"client_id": {"cli"}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {da.DeviceCode}}); got != "server_error" {
		t.Errorf("a token whose refresh token cannot be stored: %q", got)
	}
}
