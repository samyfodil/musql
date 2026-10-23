package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPassword(t *testing.T) {
	h := HashPassword("correct horse")
	if !VerifyPassword(h, "correct horse") || VerifyPassword(h, "wrong") {
		t.Fatal("verify")
	}
	if h == HashPassword("correct horse") {
		t.Fatal("salt not random")
	}
	for _, bad := range []string{"", "$argon2id$", "$argon2i$v=19$m=1,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$" + "a2V5a2V5a2V5a2V5a2V5a2V5"} {
		if CheckPasswordHash(bad) == nil || VerifyPassword(bad, "") {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestReopenKeepsSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.musq")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "a@x.io", "A", HashPassword("p"), true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if u, err := s.UserByEmail(ctx, "A@X.io"); err != nil || !u.Admin {
		t.Fatalf("%v %v", u, err)
	}
}

func TestCRUD(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "Bob@Example.com", "Bob", HashPassword("pw"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "bob@example.com", "B2", "", false); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate email: %v", err)
	}
	ms, err := s.UserOrgs(ctx, u.ID)
	if err != nil || len(ms) != 1 || ms[0].Org.Slug != "bob" || ms[0].Role != RoleOwner || !ms[0].Org.Personal {
		t.Fatalf("personal org: %+v %v", ms, err)
	}
	if err := s.EnsureGroupLocation(ctx, "fra"); err != nil {
		t.Fatal(err)
	}
	g, err := s.Group(ctx, ms[0].Org.ID, "default")
	if err != nil || g.Primary != "fra" || len(g.Locations) != 1 {
		t.Fatalf("default group: %+v %v", g, err)
	}

	u2, err := s.CreateUser(ctx, "bob@other.com", "Bob 2", HashPassword("pw"), false)
	if err != nil {
		t.Fatal(err)
	}
	ms2, err := s.UserOrgs(ctx, u2.ID)
	if err != nil || ms2[0].Org.Slug != "bob-2" {
		t.Fatalf("slug clash: %+v %v", ms2, err)
	}

	org, err := s.CreateOrg(ctx, "acme", "Acme", u.ID, "fra")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "Acme", u.ID, "fra"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate org: %v", err)
	}
	inv, err := s.CreateInvite(ctx, org.ID, "bob@other.com", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(ctx, inv.ID, u); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong user accepted: %v", err)
	}
	if _, err := s.AcceptInvite(ctx, inv.ID, u2); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.MemberRole(ctx, org.ID, u2.ID); r != RoleMember {
		t.Fatalf("role %q", r)
	}
	if n, _ := s.CountOwners(ctx, org.ID); n != 1 {
		t.Fatalf("owners %d", n)
	}

	d := &Database{OrgID: org.ID, Name: "app", GroupID: g.ID}
	if err := s.CreateDatabase(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDatabase(ctx, &Database{OrgID: org.ID, Name: "app", GroupID: g.ID}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate db: %v", err)
	}
	if err := s.RenameDatabase(ctx, d.ID, "app2"); err != nil {
		t.Fatal(err)
	}
	if err := s.BumpDatabaseTokens(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Database(ctx, org.ID, "app2")
	if err != nil || got.ID != d.ID || got.TokenGen != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.DeleteGroup(ctx, g.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete used group: %v", err)
	}
	if err := s.DeleteDatabase(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DatabaseByID(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	pt, secret, err := s.CreatePlatformToken(ctx, u.ID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if who, err := s.PlatformTokenUser(ctx, secret); err != nil || who.ID != u.ID {
		t.Fatalf("%v %v", who, err)
	}
	if err := s.RevokePlatformToken(ctx, u2.ID, pt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked another user's token")
	}
	if err := s.RevokePlatformToken(ctx, u.ID, pt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlatformTokenUser(ctx, secret); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked token still works")
	}

	if err := s.DeleteUser(ctx, u2.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.MemberRole(ctx, org.ID, u2.ID); r != "" {
		t.Fatal("membership survived")
	}
}

func TestRefreshRotation(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a, err := s.CreateRefreshToken(ctx, RefreshToken{UserID: "u", ClientID: "c", Scope: "openid"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := s.UseRefreshToken(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRefreshToken(ctx, *rt, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseRefreshToken(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatal("reuse accepted")
	}
	if _, err := s.UseRefreshToken(ctx, b); !errors.Is(err, ErrNotFound) {
		t.Fatal("family not revoked on reuse")
	}
}

func TestCodesAreSingleUse(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateAuthCode(ctx, AuthCode{ClientID: "console", UserID: "u"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeAuthCode(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeAuthCode(ctx, c); !errors.Is(err, ErrNotFound) {
		t.Fatal("code redeemed twice")
	}
	dc, err := s.CreateDeviceCode(ctx, "ABCD-EFGH", "cli", "openid", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := s.PollDevice(ctx, dc); err != nil || d.Status != DevicePending {
		t.Fatalf("%+v %v", d, err)
	}
	if err := s.DecideDevice(ctx, "ABCD-EFGH", "u", true); err != nil {
		t.Fatal(err)
	}
	if d, err := s.PollDevice(ctx, dc); err != nil || d.Status != DeviceApproved || d.UserID != "u" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := s.PollDevice(ctx, dc); !errors.Is(err, ErrNotFound) {
		t.Fatal("approved device code yielded twice")
	}
}
