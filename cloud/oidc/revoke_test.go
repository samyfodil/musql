package oidc

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

// deviceTokens signs in as ada through the device flow as the given client.
func (e *env) deviceTokens(client string) map[string]any {
	e.t.Helper()
	da := decode(e.t, e.post(e.ts.URL+"/oidc/device", url.Values{"client_id": {client}, "scope": {"openid offline_access"}}))
	e.login("s3cret")
	e.post(e.ts.URL+"/oidc/device/verify", url.Values{"user_code": {da["user_code"].(string)}, "action": {"approve"}})
	return decode(e.t, e.post(e.ts.URL+"/oidc/token", url.Values{"grant_type": {deviceGrant}, "client_id": {client}, "device_code": {da["device_code"].(string)}}))
}

func (e *env) refresh(client, rt string) int {
	return e.post(e.ts.URL+"/oidc/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {rt}}).StatusCode
}

func TestRevokeRefreshToken(t *testing.T) {
	e := newEnv(t)
	tok := e.deviceTokens("cli")
	rt := tok["refresh_token"].(string)

	// Another client's revoke does nothing; the RFC still answers 200.
	if r := e.post(e.ts.URL+"/oidc/revoke", url.Values{"client_id": {"console"}, "token": {rt}}); r.StatusCode != http.StatusOK {
		t.Fatalf("revoke as another client: %d", r.StatusCode)
	}
	// Rotate once: the revoke must reach the live token's whole family.
	next := decode(t, e.post(e.ts.URL+"/oidc/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"cli"}, "refresh_token": {rt}}))["refresh_token"].(string)

	if r := e.post(e.ts.URL+"/oidc/revoke", url.Values{"client_id": {"cli"}, "token": {next}, "token_type_hint": {"refresh_token"}}); r.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if got := e.refresh("cli", next); got != http.StatusBadRequest {
		t.Fatalf("revoked refresh token still works: %d", got)
	}

	// Unknown tokens and access tokens are accepted without effect.
	for _, form := range []url.Values{
		{"client_id": {"cli"}, "token": {"no-such-token"}},
		{"client_id": {"cli"}, "token": {tok["access_token"].(string)}, "token_type_hint": {"access_token"}},
		{"client_id": {"cli"}},
	} {
		if r := e.post(e.ts.URL+"/oidc/revoke", form); r.StatusCode != http.StatusOK {
			t.Fatalf("revoke %v: %d", form, r.StatusCode)
		}
	}
	if _, err := e.p.VerifyAccess(context.Background(), tok["access_token"].(string)); err != nil {
		t.Fatalf("access token should live until it expires: %v", err)
	}
	if r := e.post(e.ts.URL+"/oidc/revoke", url.Values{"client_id": {"nobody"}, "token": {rt}}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown client: %d", r.StatusCode)
	}
	if disc := decode(t, e.get(e.ts.URL+"/.well-known/openid-configuration")); disc["revocation_endpoint"] != e.ts.URL+"/oidc/revoke" {
		t.Fatalf("discovery: %v", disc["revocation_endpoint"])
	}
}
