package oidc

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestEndpointRefusals walks the provider's endpoints through the answers a
// well-behaved client never sees: an unknown client, an unsupported response
// or grant type, a request without a session or a token, a denied device and
// a user disabled before the device's token was taken.
func TestEndpointRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	body := func(r *http.Response) string {
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}

	if r := e.get(e.ts.URL + "/oidc/logout"); r.StatusCode != http.StatusOK || !strings.Contains(body(r), "signed out") {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if r := e.get(e.ts.URL + "/oidc/logout?post_logout_redirect_uri=" + url.QueryEscape(e.ts.URL+"/console/")); r.Header.Get("Location") != e.ts.URL+"/console/" {
		t.Fatalf("logout's redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}

	q := url.Values{"client_id": {"nope"}, "redirect_uri": {e.ts.URL + "/console/callback"}, "response_type": {"code"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
	if r := e.get(e.ts.URL + "/oidc/authorize?" + q.Encode()); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown client: %d", r.StatusCode)
	}
	q.Set("client_id", "console")
	authorizeError := func(name string, q url.Values, want string) {
		t.Helper()
		loc, _ := e.get(e.ts.URL + "/oidc/authorize?" + q.Encode()).Location()
		if loc == nil || loc.Query().Get("error") != want {
			t.Errorf("%s: %v, want %s", name, loc, want)
		}
	}
	token := url.Values{}
	for k, v := range q {
		token[k] = v
	}
	token.Set("response_type", "token")
	authorizeError("an implicit grant", token, "unsupported_response_type")
	none := url.Values{}
	for k, v := range q {
		none[k] = v
	}
	none.Set("prompt", "none")
	authorizeError("prompt=none without a session", none, "login_required")

	if r := e.get(e.ts.URL + "/oidc/userinfo"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("userinfo without a token: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/oidc/userinfo", nil)
	req.Header.Set("Authorization", "Bearer not-a-token")
	if r := e.do(req); r.StatusCode != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("userinfo with a bad token: %d", r.StatusCode)
	}
	if m := decode(t, e.post(e.ts.URL+"/oidc/token", url.Values{"client_id": {"cli"}, "grant_type": {"password"}})); m["error"] != "unsupported_grant_type" {
		t.Fatalf("a password grant: %v", m)
	}
	if m := decode(t, e.post(e.ts.URL+"/oidc/device", url.Values{"client_id": {"nope"}})); m["error"] != "invalid_client" {
		t.Fatalf("a device for an unknown client: %v", m)
	}
	if r := e.post(e.ts.URL+"/oidc/device/verify", url.Values{"user_code": {"BCDF-GHJK"}, "action": {"approve"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("verifying without a session: %d", r.StatusCode)
	}

	e.login("s3cret")
	login := url.Values{}
	for k, v := range q {
		login[k] = v
	}
	login.Set("prompt", "login")
	if r := e.get(e.ts.URL + "/oidc/authorize?" + login.Encode()); r.StatusCode/100 != 3 || !strings.Contains(r.Header.Get("Location"), "/login") {
		t.Fatalf("prompt=login with a session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.get(e.ts.URL + "/oidc/device/verify?user_code=BCDF-GHJK"); r.StatusCode != http.StatusOK || !strings.Contains(body(r), "BCDF-GHJK") {
		t.Fatalf("the verify page: %d", r.StatusCode)
	}

	poll := func(da map[string]any) map[string]any {
		return decode(t, e.post(e.ts.URL+"/oidc/token", url.Values{"grant_type": {deviceGrant}, "client_id": {"cli"}, "device_code": {da["device_code"].(string)}}))
	}
	denied := decode(t, e.post(e.ts.URL+"/oidc/device", url.Values{"client_id": {"cli"}}))
	if r := e.post(e.ts.URL+"/oidc/device/verify", url.Values{"user_code": {denied["user_code"].(string)}, "action": {"deny"}}); !strings.Contains(body(r), "denied") {
		t.Fatal("no denial page")
	}
	if m := poll(denied); m["error"] != "access_denied" {
		t.Fatalf("a denied device: %v", m)
	}

	gone := decode(t, e.post(e.ts.URL+"/oidc/device", url.Values{"client_id": {"cli"}}))
	e.post(e.ts.URL+"/oidc/device/verify", url.Values{"user_code": {gone["user_code"].(string)}, "action": {"approve"}})
	u, err := e.store.UserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	u.Disabled = true
	if err := e.store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if m := poll(gone); m["error"] != "invalid_grant" {
		t.Fatalf("a device approved by a user disabled since: %v", m)
	}
}
