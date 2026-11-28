package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// TestDatabaseHandler: a WithDatabaseHandler pattern is served under the
// database's URL after the token and the database's blocks and access
// policy are checked; every other path is still Hrana's.
func TestDatabaseHandler(t *testing.T) {
	var mu sync.Mutex
	var got DatabaseRequest
	pols := &policies{m: map[string]Policy{}}
	e := newEnv(t, "", WithAccessPolicy(pols.policy), WithDatabaseHandler("GET /info", func(r DatabaseRequest) http.Handler {
		mu.Lock()
		got = r
		mu.Unlock()
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"db": r.Database.Name, "ro": r.Access.ReadOnly})
		})
	}), WithDatabaseHandler("GET /t/{name}", func(DatabaseRequest) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.PathValue("name")) })
	}), WithDatabaseHandler("GET /files/", func(DatabaseRequest) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "files") })
	}))
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	full := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	ro := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app", Access: v1.Access_ACCESS_READ_ONLY}))).Msg.Token

	client := *e.ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	get := func(path, tok string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", e.ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, strings.TrimSpace(string(b))
	}

	for _, c := range []struct {
		path, tok string
		status    int
		body      string
	}{
		{"/db/acme/app/info", full, 200, `{"db":"app","ro":false}`},
		{"/db/acme/app/info", ro, 200, `{"db":"app","ro":true}`},
		{"/db/acme/app/info", e.admin, 200, `{"db":"app","ro":false}`},
		{"/db/acme/app/info", "", 401, ""},
		{"/db/acme/app/info", "nope", 401, ""},
		{"/db/acme/nope/info", full, 404, ""},
		{"/db/acme/app/t/orders", full, 200, "orders"},
		{"/db/acme/app/files/", full, 200, "files"},
		// The mux would redirect this to /files/, outside the database's
		// URL: it is Hrana's, which has no such path.
		{"/db/acme/app/files", full, 404, ""},
	} {
		status, body := get(c.path, c.tok)
		if status != c.status || c.body != "" && body != c.body {
			t.Errorf("%s: %d %s, want %d %s", c.path, status, body, c.status, c.body)
		}
	}
	mu.Lock()
	r := got
	mu.Unlock()
	if r.File != e.s.dbPath(r.Database) || r.Org.Slug != "acme" {
		t.Errorf("request: %+v", r)
	}
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() {
		t.Fatalf("dir %s: %v", r.Dir, err)
	}

	// Hrana keeps the rest of the URL.
	if status, res := e.pipeline("/db/acme/app", full, "SELECT 1"); status != 200 || res[0].Error.Message != "" {
		t.Errorf("hrana beside it: %d %+v", status, res)
	}

	pols.set("acme", Policy{Access: ReadOnly})
	if _, body := get("/db/acme/app/info", full); body != `{"db":"app","ro":true}` {
		t.Errorf("read-only policy: %s", body)
	}
	pols.set("acme", Policy{Access: Deny, Reason: `the "app" quota is used up`})
	var refused struct{ Message string }
	if status, body := get("/db/acme/app/info", full); status != 403 || json.Unmarshal([]byte(body), &refused) != nil || refused.Message != `the "app" quota is used up` {
		t.Errorf("denied: %d %s", status, body)
	}
	pols.set("acme", Policy{})

	must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockReads: ptrTo(true)})))
	if status, body := get("/db/acme/app/info", full); status != 403 || !strings.Contains(body, "reads are blocked") {
		t.Errorf("reads blocked: %d %s", status, body)
	}
	must(dbs.Update(ctx, auth(e.admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockReads: ptrTo(false)})))

	e.s.removeFiles(r.Database)
	if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
		t.Errorf("dir left after the database's files: %v", err)
	}
}

// TestDatabaseHandlerBytesServed: the response body bytes a database handler
// writes count in the database's usage; a refused request's and a HEAD's do
// not.
func TestDatabaseHandlerBytesServed(t *testing.T) {
	const n = 3000
	e := newEnv(t, "", WithDatabaseHandler("GET /blob", func(DatabaseRequest) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, strings.Repeat("a", 1000))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush: %v", err)
			}
			f, ok := w.(http.Flusher)
			if !ok {
				t.Errorf("the handler's writer is not an http.Flusher")
			} else {
				f.Flush()
			}
			if _, ok := w.(http.Hijacker); !ok {
				t.Errorf("the handler's writer is not an http.Hijacker")
			}
			io.Copy(w, strings.NewReader(strings.Repeat("b", n-1000)))
		})
	}))
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	for _, c := range []struct {
		method, tok string
		status      int
	}{{"GET", tok, 200}, {"GET", "nope", 401}, {"HEAD", tok, 200}} {
		req, err := http.NewRequest(c.method, e.ts.URL+"/db/acme/app/blob", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.tok)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != c.status || c.status == 200 && c.method == "GET" && len(b) != n {
			t.Fatalf("%s %s: %d, %d bytes, %v", c.method, c.tok, resp.StatusCode, len(b), err)
		}
	}
	u := must(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme", Name: "app"}))).Msg
	if len(u.Points) != 1 || u.Points[0].BytesServed != n {
		t.Fatalf("usage %+v", u.Points)
	}
	org := must(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme"}))).Msg
	if len(org.Points) != 1 || org.Points[0].BytesServed != n {
		t.Fatalf("org usage %+v", org.Points)
	}
}
