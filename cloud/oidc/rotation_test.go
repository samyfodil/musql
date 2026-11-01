package oidc

import (
	"context"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const day = 24 * time.Hour

func dbToken(t *testing.T, p *Provider, exp *jwt.NumericDate) string {
	t.Helper()
	tok, err := p.Sign(&Claims{Claims: jwt.Claims{Subject: "u", Expiry: exp}, Use: UseDB, Access: "full"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func kidOf(t *testing.T, tok string) string {
	t.Helper()
	parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Headers[0].KeyID
}

func jwksSize(e *env) int {
	return len(decode(e.t, e.get(e.ts.URL+"/oidc/jwks"))["keys"].([]any))
}

func TestSigningKeyRotation(t *testing.T) {
	e := newEnv(t, WithKeyRotation(30*day))
	ctx := context.Background()
	now := time.Now()

	short := dbToken(t, e.p, jwt.NewNumericDate(now.Add(time.Hour)))
	long := dbToken(t, e.p, jwt.NewNumericDate(now.Add(90*day)))
	first := kidOf(t, long)

	if err := e.p.RotateIfDue(ctx, now.Add(29*day)); err != nil || jwksSize(e) != 1 {
		t.Fatalf("rotated early: %v, %d keys", err, jwksSize(e))
	}

	// The old key stays while a token it signed is still good.
	if err := e.p.RotateIfDue(ctx, now.Add(31*day)); err != nil {
		t.Fatal(err)
	}
	if jwksSize(e) != 2 {
		t.Fatalf("after rotation: %d keys, want 2", jwksSize(e))
	}
	for _, tok := range []string{short, long} {
		if _, err := e.p.Verify(tok, UseDB); err != nil {
			t.Fatalf("token signed before rotation: %v", err)
		}
	}
	second := dbToken(t, e.p, jwt.NewNumericDate(now.Add(time.Hour)))
	if kidOf(t, second) == first {
		t.Fatal("still signing with the old key")
	}

	// Not due again until another period has passed.
	if err := e.p.RotateIfDue(ctx, now.Add(40*day)); err != nil || jwksSize(e) != 2 {
		t.Fatalf("%v, %d keys", err, jwksSize(e))
	}

	// The second key signed only a short-lived token: gone at the next
	// rotation. The first one is held by its 90-day token.
	if err := e.p.RotateIfDue(ctx, now.Add(62*day)); err != nil {
		t.Fatal(err)
	}
	if jwksSize(e) != 2 {
		t.Fatalf("after second rotation: %d keys, want 2", jwksSize(e))
	}
	if err := e.p.RotateIfDue(ctx, now.Add(100*day)); err != nil {
		t.Fatal(err)
	}
	if jwksSize(e) != 1 {
		t.Fatalf("after the long token expired: %d keys, want 1", jwksSize(e))
	}
	if _, err := e.p.Verify(long, UseDB); err == nil {
		t.Fatal("token of a dropped key still verifies")
	}

	// A restart keeps the newest key signing.
	again, err := New(ctx, e.store, e.ts.URL, WithKeyRotation(30*day))
	if err != nil {
		t.Fatal(err)
	}
	if kidOf(t, dbToken(t, again, jwt.NewNumericDate(now.Add(time.Hour)))) != kidOf(t, dbToken(t, e.p, jwt.NewNumericDate(now.Add(time.Hour)))) {
		t.Fatal("restart changed the signing key")
	}
}

func TestKeyOfNonExpiringTokenIsKept(t *testing.T) {
	e := newEnv(t, WithKeyRotation(30*day))
	ctx := context.Background()
	now := time.Now()

	forever := dbToken(t, e.p, nil)
	if err := e.p.RotateIfDue(ctx, now.Add(31*day)); err != nil {
		t.Fatal(err)
	}
	if err := e.p.RotateIfDue(ctx, now.Add(3650*day)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Verify(forever, UseDB); err != nil {
		t.Fatalf("non-expiring token: %v", err)
	}
	keys, err := e.store.SigningKeys(ctx)
	if err != nil || len(keys) < 2 {
		t.Fatalf("%d keys stored, %v", len(keys), err)
	}
}

func TestNoRotationByDefault(t *testing.T) {
	e := newEnv(t)
	if err := e.p.RotateIfDue(context.Background(), time.Now().Add(3650*day)); err != nil || jwksSize(e) != 1 {
		t.Fatalf("%v, %d keys", err, jwksSize(e))
	}
}
