package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestOIDCFromAnotherOrigin(t *testing.T) {
	const dev = "http://localhost:5173"
	e := newEnv(t, "console:\n  cors_origins: [\""+dev+"\"]\n")
	client := e.ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for _, p := range []string{"/oidc/token", "/oidc/userinfo", "/oidc/logout", "/oidc/revoke"} {
		for _, tc := range []struct{ origin, want string }{{dev, dev}, {"https://evil.example", ""}} {
			req, _ := http.NewRequest("OPTIONS", e.ts.URL+p, nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", "POST")
			resp := must(client.Do(req))
			resp.Body.Close()
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != tc.want {
				t.Errorf("preflight %s from %s: allow-origin %q, want %q", p, tc.origin, got, tc.want)
			}
		}
	}

	req, _ := http.NewRequest("POST", e.ts.URL+"/oidc/token", strings.NewReader("client_id=nope"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", dev)
	resp := must(client.Do(req))
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != dev {
		t.Fatalf("token response lacks CORS headers: %v", resp.Header)
	}

	authorize := func(redirect string) int {
		q := url.Values{"client_id": {"console"}, "redirect_uri": {redirect}, "response_type": {"code"},
			"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
		resp := must(client.Get(e.ts.URL + "/oidc/authorize?" + q.Encode()))
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, ok := range []string{dev + "/console/callback", dev + "/callback", e.ts.URL + "/console/callback"} {
		if got := authorize(ok); got != http.StatusFound {
			t.Errorf("redirect %s: %d, want a redirect to sign-in", ok, got)
		}
	}
	for _, bad := range []string{"http://localhost:5174/callback", dev + "/other", dev + "/callback/", "https://evil.example/callback"} {
		if got := authorize(bad); got != http.StatusBadRequest {
			t.Errorf("redirect %s: %d, want 400", bad, got)
		}
	}
}
