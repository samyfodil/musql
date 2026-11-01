package oidc

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestLoginNeedsCSRFToken(t *testing.T) {
	e := newEnv(t)
	form := func(csrf string) url.Values {
		return url.Values{"email": {"ada@example.com"}, "password": {"s3cret"}, "next": {"/console/"}, "csrf": {csrf}}
	}

	// No cookie, no field: a cross-site form post.
	if r := e.post(e.ts.URL+"/login", form("")); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	tok := e.csrf()
	if r := e.post(e.ts.URL+"/login", form("")); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie without field: %d", r.StatusCode)
	}
	if r := e.post(e.ts.URL+"/login", form(tok+"x")); r.StatusCode != http.StatusForbidden {
		t.Fatalf("mismatched field: %d", r.StatusCode)
	}
	if r := e.post(e.ts.URL+"/login", form(tok)); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("matching token: %d", r.StatusCode)
	}

	// A field alone, with no cookie, does not pass either.
	fresh := newEnv(t)
	if r := fresh.post(fresh.ts.URL+"/login", form(tok)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("field without cookie: %d", r.StatusCode)
	}
}

func TestFailedLoginRateLimit(t *testing.T) {
	e := newEnv(t, WithLoginLimits(4, 2, time.Hour))
	if r := e.login("wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first failure: %d", r.StatusCode)
	}
	if r := e.login("wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second failure: %d", r.StatusCode)
	}
	// The email is at its limit: even the right password is refused.
	r := e.login("s3cret")
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("over the email limit: %d %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	// The email limit ignores case.
	if r := e.signIn("Ada@Example.com", "s3cret", "/console/"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("other case: %d", r.StatusCode)
	}

	// Other emails from the same address count against the address.
	for _, who := range []string{"a@example.com", "b@example.com"} {
		if r := e.signIn(who, "x", "/console/"); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: %d", who, r.StatusCode)
		}
	}
	if r := e.signIn("c@example.com", "x", "/console/"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the address limit: %d", r.StatusCode)
	}
}

func TestLoginLimitWindowAndReset(t *testing.T) {
	e := newEnv(t, WithLoginLimits(-1, 2, time.Hour))
	now := time.Now()
	e.p.byMail.now = func() time.Time { return now }

	e.login("wrong")
	e.login("wrong")
	if r := e.login("s3cret"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("limited: %d", r.StatusCode)
	}
	now = now.Add(61 * time.Minute)
	if r := e.login("s3cret"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("after the window: %d", r.StatusCode)
	}

	// A success clears the email's count.
	e.login("wrong")
	if r := e.login("s3cret"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("one failure then success: %d", r.StatusCode)
	}
	e.login("wrong")
	if r := e.login("s3cret"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("success did not reset the count: %d", r.StatusCode)
	}
}
