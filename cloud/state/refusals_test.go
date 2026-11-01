package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestStoreRefusals: the store refuses a name or a code already taken and a
// token it does not know or that expired; an invite to a member raises its
// role and never lowers it; and a user created again gets its personal org
// back.
func TestStoreRefusals(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ann, err := s.CreateUser(ctx, "ann@example.com", "Ann", HashPassword("ann-password"), false)
	must(err)
	o, err := s.CreateOrg(ctx, "acme", "Acme", ann.ID, "")
	must(err)

	g, err := s.CreateGroup(ctx, o.ID, "eu", "", "")
	must(err)
	if g.Type != TypePlain {
		t.Errorf("a group of no type is of type %q", g.Type)
	}
	if _, err := s.CreateGroup(ctx, o.ID, "eu", "", ""); !errors.Is(err, ErrExists) {
		t.Errorf("a group name taken: %v", err)
	}

	bob, err := s.CreateUser(ctx, "bob@example.com", "Bob", HashPassword("bob-password"), false)
	must(err)
	role := func() Role {
		t.Helper()
		r, err := s.MemberRole(ctx, o.ID, bob.ID)
		must(err)
		return r
	}
	for _, r := range []Role{RoleViewer, RoleAdmin, RoleMember} {
		inv, err := s.CreateInvite(ctx, o.ID, "bob@example.com", r)
		must(err)
		if r == RoleViewer {
			if _, err := s.CreateInvite(ctx, o.ID, "bob@example.com", r); !errors.Is(err, ErrExists) {
				t.Errorf("an invite twice: %v", err)
			}
		}
		_, err = s.AcceptInvite(ctx, inv.ID, bob)
		must(err)
	}
	if r := role(); r != RoleAdmin {
		t.Errorf("after invites as viewer, admin and member: %s", r)
	}

	carl, err := s.CreateUser(ctx, "carl@example.com", "Carl", HashPassword("carl-password"), false)
	must(err)
	must(s.SetMember(ctx, o.ID, carl.ID, RoleMember))
	if r, err := s.MemberRole(ctx, o.ID, carl.ID); err != nil || r != RoleMember {
		t.Errorf("a member set: %s %v", r, err)
	}
	must(s.DeleteUser(ctx, carl.ID))
	again, err := s.CreateUser(ctx, "carl@example.com", "Carl", HashPassword("carl-password"), false)
	must(err)
	ms, err := s.UserOrgs(ctx, again.ID)
	must(err)
	if len(ms) != 1 || !ms[0].Org.Personal || ms[0].Role != RoleOwner {
		t.Errorf("a user created again: %+v", ms)
	}

	if _, err := s.PlatformTokenUser(ctx, "no-prefix"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a platform token without its prefix: %v", err)
	}
	rt, err := s.CreateRefreshToken(ctx, RefreshToken{UserID: ann.ID, ClientID: "cli"}, -time.Hour)
	must(err)
	if _, err := s.UseRefreshToken(ctx, rt); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired refresh token: %v", err)
	}
	code, err := s.CreateAuthCode(ctx, AuthCode{ClientID: "cli", UserID: ann.ID}, -time.Hour)
	must(err)
	if _, err := s.TakeAuthCode(ctx, code); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired auth code: %v", err)
	}
	_, err = s.CreateDeviceCode(ctx, "BCDF-GHJK", "cli", "", time.Minute)
	must(err)
	if _, err := s.CreateDeviceCode(ctx, "BCDF-GHJK", "cli", "", time.Minute); !errors.Is(err, ErrExists) {
		t.Errorf("a user code taken: %v", err)
	}
	must(s.DecideDevice(ctx, "BCDF-GHJK", ann.ID, true))
	if _, err := s.DeviceByUserCode(ctx, "BCDF-GHJK"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a device already decided: %v", err)
	}

	if s.Clustered() {
		t.Error("a store opened alone is clustered")
	}
	if held, err := s.ResolveHeld(ctx); held != nil || err != nil {
		t.Errorf("a store opened alone holds rows out: %v %v", held, err)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Ann.Lee":               "ann-lee",
		"___":                   "org",
		"-x-":                   "x",
		strings.Repeat("a", 50): strings.Repeat("a", 40),
	} {
		if got := slugify(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
