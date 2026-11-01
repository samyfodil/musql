package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/samyfodil/musql/cloud/state"
)

type env struct {
	t      *testing.T
	ts     *httptest.Server
	p      *Provider
	store  *state.Store
	client *http.Client
}

func newEnv(t *testing.T, opts ...Option) *env {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	p, err := New(context.Background(), store, ts.URL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	h = p.Handler()
	if _, err := store.CreateUser(context.Background(), "ada@example.com", "Ada", state.HashPassword("s3cret"), true); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &env{t: t, ts: ts, p: p, store: store, client: c}
}

func (e *env) do(req *http.Request) *http.Response {
	e.t.Helper()
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) get(u string) *http.Response {
	req, _ := http.NewRequest("GET", u, nil)
	return e.do(req)
}

func (e *env) post(u string, form url.Values) *http.Response {
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.do(req)
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf loads the sign-in page, which sets the CSRF cookie, and returns the
// token its form carries.
func (e *env) csrf() string {
	e.t.Helper()
	resp := e.get(e.ts.URL + "/login")
	b, _ := io.ReadAll(resp.Body)
	m := csrfField.FindSubmatch(b)
	if m == nil {
		e.t.Fatalf("no csrf field in the sign-in page: %s", b)
	}
	return string(m[1])
}

// signIn posts the sign-in form with a valid CSRF token.
func (e *env) signIn(email, password, next string) *http.Response {
	e.t.Helper()
	return e.post(e.ts.URL+"/login", url.Values{"email": {email}, "password": {password}, "next": {next}, "csrf": {e.csrf()}})
}

func (e *env) login(password string) *http.Response {
	return e.signIn("ADA@example.com", password, "/console/")
}

func TestCodeFlowWithPKCE(t *testing.T) {
	e := newEnv(t)
	disc := decode(t, e.get(e.ts.URL+"/.well-known/openid-configuration"))
	if disc["issuer"] != e.ts.URL || disc["authorization_endpoint"] != e.ts.URL+"/oidc/authorize" {
		t.Fatalf("discovery: %v", disc)
	}
	jwks := decode(t, e.get(e.ts.URL+"/oidc/jwks"))
	if keys := jwks["keys"].([]any); len(keys) != 1 || keys[0].(map[string]any)["crv"] != "Ed25519" {
		t.Fatalf("jwks: %v", jwks)
	}

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	redirect := e.ts.URL + "/console/callback"
	q := url.Values{
		"client_id": {"console"}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {"openid email"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"state": {"st"}, "nonce": {"n0"},
	}
	authURL := e.ts.URL + "/oidc/authorize?" + q.Encode()

	resp := e.get(authURL)
	loc, _ := resp.Location()
	if resp.StatusCode != http.StatusFound || loc.Path != "/login" {
		t.Fatalf("unauthenticated authorize: %d %v", resp.StatusCode, loc)
	}
	next := loc.Query().Get("next")

	if r := e.login("wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", r.StatusCode)
	}
	resp = e.signIn("ada@example.com", "s3cret", next)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != next {
		t.Fatalf("login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp = e.get(e.ts.URL + next)
	loc, _ = resp.Location()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc.String(), redirect) || loc.Query().Get("state") != "st" {
		t.Fatalf("authorize: %d %v", resp.StatusCode, loc)
	}
	code := loc.Query().Get("code")

	tokURL := e.ts.URL + "/oidc/token"
	base := url.Values{"grant_type": {"authorization_code"}, "client_id": {"console"}, "code": {code}, "redirect_uri": {redirect}}
	bad := url.Values{}
	for k, v := range base {
		bad[k] = v
	}
	bad.Set("code_verifier", strings.Repeat("x", 50))
	if r := e.post(tokURL, bad); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong verifier: %d", r.StatusCode)
	}
	// A failed redemption still consumed the code.
	base.Set("code_verifier", verifier)
	if r := e.post(tokURL, base); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("code reused: %d", r.StatusCode)
	}

	// A fresh code, redeemed properly.
	resp = e.get(authURL)
	loc, _ = resp.Location()
	base.Set("code", loc.Query().Get("code"))
	resp = e.post(tokURL, base)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token: %d", resp.StatusCode)
	}
	tok := decode(t, resp)
	id, err := e.p.Verify(tok["id_token"].(string), UseID)
	if err != nil || id.Nonce != "n0" || id.Email != "ada@example.com" {
		t.Fatalf("id token: %+v %v", id, err)
	}
	if _, err := e.p.Verify(tok["id_token"].(string), UseAccess); err == nil {
		t.Fatal("id token accepted as access token")
	}

	req, _ := http.NewRequest("GET", e.ts.URL+"/oidc/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok["access_token"].(string))
	if info := decode(t, e.do(req)); info["email"] != "ada@example.com" {
		t.Fatalf("userinfo: %v", info)
	}

	refresh := tok["refresh_token"].(string)
	resp = e.post(tokURL, url.Values{"grant_type": {"refresh_token"}, "client_id": {"console"}, "refresh_token": {refresh}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: %d", resp.StatusCode)
	}
	rotated := decode(t, resp)["refresh_token"].(string)
	if rotated == refresh {
		t.Fatal("refresh token not rotated")
	}
	if r := e.post(tokURL, url.Values{"grant_type": {"refresh_token"}, "client_id": {"console"}, "refresh_token": {refresh}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("old refresh token reused: %d", r.StatusCode)
	}
	if r := e.post(tokURL, url.Values{"grant_type": {"refresh_token"}, "client_id": {"console"}, "refresh_token": {rotated}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("family survived reuse: %d", r.StatusCode)
	}
}

func TestAuthorizeRejects(t *testing.T) {
	e := newEnv(t)
	e.login("s3cret")
	q := url.Values{"client_id": {"console"}, "redirect_uri": {"https://evil.example/cb"}, "response_type": {"code"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
	if r := e.get(e.ts.URL + "/oidc/authorize?" + q.Encode()); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered redirect: %d", r.StatusCode)
	}
	q.Set("redirect_uri", e.ts.URL+"/console/callback")
	q.Set("code_challenge_method", "plain")
	r := e.get(e.ts.URL + "/oidc/authorize?" + q.Encode())
	if loc, _ := r.Location(); loc == nil || loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("plain PKCE: %v", loc)
	}
	r = e.signIn("ada@example.com", "s3cret", "//evil.example")
	if r.Header.Get("Location") != "/console/" {
		t.Fatalf("open redirect: %q", r.Header.Get("Location"))
	}
}

func TestDeviceFlow(t *testing.T) {
	e := newEnv(t)
	da := decode(t, e.post(e.ts.URL+"/oidc/device", url.Values{"client_id": {"cli"}, "scope": {"openid offline_access"}}))
	dc, uc := da["device_code"].(string), da["user_code"].(string)
	poll := url.Values{"grant_type": {deviceGrant}, "client_id": {"cli"}, "device_code": {dc}}
	if m := decode(t, e.post(e.ts.URL+"/oidc/token", poll)); m["error"] != "authorization_pending" {
		t.Fatalf("pending: %v", m)
	}
	if r := e.get(e.ts.URL + "/oidc/device/verify?user_code=" + uc); r.StatusCode != http.StatusFound {
		t.Fatalf("verify page without session: %d", r.StatusCode)
	}
	e.login("s3cret")
	if r := e.post(e.ts.URL+"/oidc/device/verify", url.Values{"user_code": {"ZZZZ-ZZZZ"}, "action": {"approve"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown code: %d", r.StatusCode)
	}
	typed := strings.ToLower(strings.ReplaceAll(uc, "-", ""))
	if r := e.post(e.ts.URL+"/oidc/device/verify", url.Values{"user_code": {typed}, "action": {"approve"}}); r.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d", r.StatusCode)
	}
	tok := decode(t, e.post(e.ts.URL+"/oidc/token", poll))
	u, err := e.p.VerifyAccess(context.Background(), tok["access_token"].(string))
	if err != nil || u.Email != "ada@example.com" {
		t.Fatalf("device token: %v %v", u, err)
	}
	if m := decode(t, e.post(e.ts.URL+"/oidc/token", poll)); m["error"] != "invalid_grant" {
		t.Fatalf("device code redeemed twice: %v", m)
	}
}

func TestDisabledUserTokensStop(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u, _ := e.store.UserByEmail(ctx, "ada@example.com")
	da := decode(t, e.post(e.ts.URL+"/oidc/device", url.Values{"client_id": {"cli"}}))
	e.login("s3cret")
	e.post(e.ts.URL+"/oidc/device/verify", url.Values{"user_code": {da["user_code"].(string)}, "action": {"approve"}})
	tok := decode(t, e.post(e.ts.URL+"/oidc/token", url.Values{"grant_type": {deviceGrant}, "client_id": {"cli"}, "device_code": {da["device_code"].(string)}}))
	u.Disabled = true
	if err := e.store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.VerifyAccess(ctx, tok["access_token"].(string)); err == nil {
		t.Fatal("disabled user's access token accepted")
	}
	if r := e.login("s3cret"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled user signed in: %d", r.StatusCode)
	}
}
