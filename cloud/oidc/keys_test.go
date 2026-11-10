package oidc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/samyfodil/musql/cloud/state"
)

func openStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "state.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestProviderKeys: a provider will not start on a store it cannot read, a
// stored key that is malformed, or keys that are all retired; a cluster node
// reloads the keys for a kid it does not know; and a provider whose own key
// another node retired keeps signing with it until it rotates.
func TestProviderKeys(t *testing.T) {
	ctx := context.Background()
	closed := openStore(t)
	closed.Close()
	if _, err := New(ctx, closed, "http://x"); err == nil {
		t.Error("started on a closed store")
	}
	bad := openStore(t)
	if err := bad.AddSigningKey(ctx, &state.SigningKey{KID: "k", PrivateKey: []byte{1, 2, 3}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, bad, "http://x"); err == nil {
		t.Error("started with a malformed key")
	}
	retired := openStore(t)
	if _, err := New(ctx, retired, "http://x"); err != nil {
		t.Fatal(err)
	}
	keys, err := retired.SigningKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys: %v %v", keys, err)
	}
	if err := retired.RetireSigningKey(ctx, keys[0].KID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, retired, "http://x"); err == nil {
		t.Error("started with every key retired")
	}

	store := openStore(t)
	p, err := New(ctx, store, "http://x/", WithNode("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Issuer() != "http://x" {
		t.Errorf("issuer %q", p.Issuer())
	}
	other, err := New(ctx, openStore(t), "http://x", WithNode("n2"))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.Sign(&Claims{Claims: jwt.Claims{Subject: "u", Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}, Use: UseDB})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Verify(foreign, UseDB); err == nil {
		t.Error("verified a token signed by a key this cluster does not hold")
	}
	p.mu.Lock()
	active := p.active.kid
	p.mu.Unlock()
	if err := store.RetireSigningKey(ctx, active, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := p.ReloadKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sign(&Claims{Claims: jwt.Claims{Subject: "u"}, Use: UseDB}); err != nil {
		t.Errorf("signing after the active key was retired elsewhere: %v", err)
	}
	store.Close()
	p.mu.Lock()
	p.lastReload = time.Time{}
	p.mu.Unlock()
	if _, err := p.Verify(foreign, UseDB); err == nil {
		t.Error("verified a token whose key could not be looked up")
	}
}

// TestSignStoresALostKey: a provider whose own key the state does not hold --
// a cluster's new master can be behind the one it was stored on -- stores it
// again when it next records an expiry, instead of failing every sign-in with
// "not found" (#129); and one that arrived meanwhile is updated as it is.
func TestSignStoresALostKey(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p, err := New(ctx, store, "http://x", WithNode("n1"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := store.SigningKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys: %v %v", keys, err)
	}
	lost := keys[0]
	if err := store.DeleteSigningKey(ctx, lost.KID); err != nil {
		t.Fatal(err)
	}
	claims := func(d time.Duration) *Claims {
		return &Claims{Claims: jwt.Claims{Subject: "u", Expiry: jwt.NewNumericDate(time.Now().Add(d))}, Use: UseAccess, ClientID: ClientConsole}
	}
	tok, err := p.Sign(claims(time.Hour))
	if err != nil {
		t.Fatalf("signing with a key the state lost: %v", err)
	}
	if _, err := p.Verify(tok, UseAccess); err != nil {
		t.Fatal(err)
	}
	keys, err = store.SigningKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].KID != lost.KID || string(keys[0].PrivateKey) != string(lost.PrivateKey) || keys[0].MaxExp < time.Now().Add(time.Hour).Unix() {
		t.Fatalf("the key is not stored again as it was: %v %v", keys, err)
	}
	if _, err := p.Sign(claims(72 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if keys, err = store.SigningKeys(ctx); err != nil || keys[0].MaxExp < time.Now().Add(72*time.Hour).Unix() {
		t.Fatalf("a later expiry is not recorded: %v %v", keys, err)
	}
}
