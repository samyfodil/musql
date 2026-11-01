package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/samyfodil/musql/cloud/state"
)

// TestProviderRefusals: a request without a session or with a forged one has
// no user, a token that does not parse or names a deleted user is refused,
// and once the store fails the provider reports it rather than answering.
func TestProviderRefusals(t *testing.T) {
	e := newEnv(t, WithKeyRotation(time.Hour))
	ctx := context.Background()
	if got := clientIP(&http.Request{RemoteAddr: "no-port"}); got != "no-port" {
		t.Errorf("an address without a port: %q", got)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if e.p.session(r) != nil {
		t.Error("a request without a session has a user")
	}
	r.AddCookie(&http.Cookie{Name: sessionName, Value: "forged"})
	if e.p.session(r) != nil {
		t.Error("a forged session has a user")
	}
	if _, err := e.p.Verify("not.a.token", UseAccess); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token that does not parse: %v", err)
	}

	zed, err := e.store.CreateUser(ctx, "zed@example.com", "Zed", state.HashPassword("zed-password"), false)
	if err != nil {
		t.Fatal(err)
	}
	access := func(sub string) string {
		t.Helper()
		tok, err := e.p.Sign(&Claims{Claims: jwt.Claims{Subject: sub, Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}, Use: UseAccess})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	gone := access(zed.ID)
	if err := e.store.DeleteUser(ctx, zed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.VerifyAccess(ctx, gone); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token for a deleted user: %v", err)
	}

	ada, err := e.store.UserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	live := access(ada.ID)
	e.store.Close()
	if _, err := e.p.Authenticate(ctx, "ada@example.com", "s3cret"); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Errorf("a sign-in with the store closed: %v", err)
	}
	if _, err := e.p.VerifyAccess(ctx, live); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token checked with the store closed: %v", err)
	}
	if _, err := e.p.Sign(&Claims{Claims: jwt.Claims{Subject: ada.ID, Expiry: jwt.NewNumericDate(time.Now().Add(24 * time.Hour))}, Use: UseAccess}); err == nil {
		t.Error("signed a token whose expiry could not be recorded")
	}
	if err := e.p.RotateIfDue(ctx, time.Now().Add(2*time.Hour)); err == nil {
		t.Error("rotated a key the store could not take")
	}
}
