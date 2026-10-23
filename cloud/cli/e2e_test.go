package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/samyfodil/musql/driver"

	"github.com/samyfodil/musql/cloud/cli"
	"github.com/samyfodil/musql/cloud/server"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t   *testing.T
	srv *server.Server
	url string
	dir string
}

func start(t *testing.T) *harness {
	t.Helper()
	data := t.TempDir()
	cfg, err := server.ParseConfig([]byte("data_dir: " + data + "\nadmin:\n  email: admin@example.com\n  password: admin-password\nnode:\n  location: fra\n"))
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	cfg.PublicURL = ts.URL
	srv, err := server.New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	h = srv.Handler()
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return &harness{t: t, srv: srv, url: ts.URL, dir: filepath.Join(t.TempDir(), "musql")}
}

// run runs the CLI and returns stdout; a failure fails the test.
func (h *harness) run(args ...string) string {
	h.t.Helper()
	out, err := h.try(args...)
	if err != nil {
		h.t.Fatalf("musql %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (h *harness) try(args ...string) (string, error) {
	var out bytes.Buffer
	err := cli.Run(context.Background(), cli.Env{Dir: h.dir, Stdin: strings.NewReader(""), Stdout: &out, Stderr: io.Discard}, args)
	return out.String(), err
}

// login drives the device flow: the CLI polls while the test approves its
// code through the server's own state, as the verify page would.
func (h *harness) login() {
	h.t.Helper()
	ctx := context.Background()
	stderr := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(ctx, cli.Env{Dir: h.dir, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: stderr}, []string{"auth", "login", "--headless", "--url", h.url})
	}()
	re := regexp.MustCompile(`confirm the code (\S+)`)
	var code string
	for deadline := time.Now().Add(10 * time.Second); code == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(stderr.String()); m != nil {
			code = m[1]
		}
	}
	if code == "" {
		h.t.Fatalf("no user code printed: %q", stderr.String())
	}
	u, err := h.srv.Store().UserByEmail(ctx, "admin@example.com")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.srv.Store().DecideDevice(ctx, code, u.ID, true); err != nil {
		h.t.Fatal(err)
	}
	if err := <-done; err != nil {
		h.t.Fatalf("login: %v", err)
	}
	if !strings.Contains(stderr.String(), "Signed in as admin@example.com") {
		h.t.Fatalf("login output: %q", stderr.String())
	}
}

func TestEndToEnd(t *testing.T) {
	h := start(t)
	if _, err := h.try("db", "list", "--url", h.url); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("before login: %v", err)
	}
	h.login()

	if out := h.run("auth", "whoami"); !strings.HasPrefix(out, "admin@example.com (server admin)") {
		t.Fatalf("whoami: %q", out)
	}

	// An expired access token is refreshed, and the refresh token rotated.
	before, err := cli.LoadConfig(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	stale := *before
	stale.Expiry = time.Now().Add(-time.Minute)
	if err := stale.Save(h.dir); err != nil {
		t.Fatal(err)
	}
	h.run("auth", "whoami")
	after, err := cli.LoadConfig(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.RefreshToken == before.RefreshToken || after.AccessToken == before.AccessToken || !after.Expiry.After(time.Now()) {
		t.Fatal("tokens were not refreshed")
	}

	h.run("org", "create", "acme", "--name", "Acme")
	h.run("org", "switch", "acme")
	if out := h.run("org", "list"); !strings.Contains(out, "acme*") {
		t.Fatalf("org list: %q", out)
	}
	if out := h.run("group", "list"); !strings.Contains(out, "default  fra") {
		t.Fatalf("group list: %q", out)
	}

	if out := h.run("db", "create", "app"); !strings.Contains(out, h.url+"/db/acme/app/") {
		t.Fatalf("db create: %q", out)
	}
	if out := h.run("db", "list"); !strings.Contains(out, "app   default  "+h.url+"/db/acme/app/") {
		t.Fatalf("db list: %q", out)
	}
	if out := h.run("db", "show", "app", "--url"); out != h.url+"/db/acme/app/\n" {
		t.Fatalf("db show --url: %q", out)
	}
	if tok := h.run("db", "tokens", "create", "app", "--read-only", "-e", "7d"); strings.Count(tok, ".") != 2 {
		t.Fatalf("token: %q", tok)
	}

	h.run("db", "shell", "app", "CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT); INSERT INTO t(name) VALUES ('ann'), ('bo'), (NULL);")
	out := h.run("db", "shell", "app", "SELECT id, name FROM t ORDER BY id;")
	if want := "id  name\n1   ann\n2   bo\n3   NULL\n"; out != want {
		t.Fatalf("shell: %q, want %q", out, want)
	}
	if out := h.run("db", "shell", "app", "SELECT count(*) AS n FROM t;", "--json"); out != "[\n  {\"n\": 3}\n]\n" {
		t.Fatalf("shell --json: %q", out)
	}

	musq := filepath.Join(t.TempDir(), "app.musq")
	h.run("db", "export", "app", "-o", musq)
	lite := filepath.Join(t.TempDir(), "app.sqlite")
	h.run("db", "export", "app", "--sqlite", "-o", lite)
	b, err := os.ReadFile(lite)
	if err != nil || !bytes.HasPrefix(b, []byte("SQLite format 3\x00")) {
		t.Fatalf("sqlite export: %v", err)
	}
	local, err := sql.Open("sqlite", musq)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := local.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 3 {
		t.Fatalf("exported file: %d %v", n, err)
	}
	local.Close()

	h.run("db", "import", lite, "--name", "copy")
	if out := h.run("db", "shell", "copy", "SELECT name FROM t WHERE id = 2;"); out != "name\nbo\n" {
		t.Fatalf("imported: %q", out)
	}
	h.run("db", "create", "fromfile", "--from-file", musq)

	if _, err := h.try("db", "restore", "app", "--timestamp", "2026-01-01T00:00:00Z"); err == nil || !strings.Contains(err.Error(), "before the oldest") {
		t.Fatalf("restore before history: %v", err)
	}
	h.run("db", "restore", "app", "--timestamp", time.Now().UTC().Format(time.RFC3339Nano), "--new-name", "restored")
	if out := h.run("db", "shell", "restored", "SELECT count(*) AS n FROM t;"); out != h.run("db", "shell", "app", "SELECT count(*) AS n FROM t;") {
		t.Fatalf("restored: %q", out)
	}
	if out := h.run("db", "inspect", "app"); !strings.Contains(out, "last 24h:") {
		t.Fatalf("inspect: %q", out)
	}

	h.run("db", "rename", "copy", "copy2")
	h.run("db", "destroy", "copy2", "--yes")
	if out := h.run("db", "list"); strings.Contains(out, "copy") {
		t.Fatalf("after destroy: %q", out)
	}

	secret := strings.TrimSpace(h.run("auth", "api-tokens", "mint", "ci"))
	var ci bytes.Buffer
	if err := cli.Run(context.Background(), cli.Env{Dir: t.TempDir(), Token: secret, Stdout: &ci, Stderr: io.Discard},
		[]string{"db", "list", "--url", h.url, "--org", "acme"}); err != nil || !strings.Contains(ci.String(), "app") {
		t.Fatalf("api token: %q %v", ci.String(), err)
	}

	kept, err := cli.LoadConfig(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	h.run("auth", "logout")
	if _, err := h.try("db", "list"); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("after logout: %v", err)
	}

	// A copy of the refresh token taken before logout is dead on the server.
	kept.AccessToken, kept.Expiry = "", time.Time{}
	if err := kept.Save(h.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := h.try("db", "list"); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("refresh token survived logout: %v", err)
	}
}

type lines struct {
	in      []string
	prompts []string
}

func (l *lines) ReadLine() (string, error) {
	if len(l.in) == 0 {
		return "", io.EOF
	}
	s := l.in[0]
	l.in = l.in[1:]
	return s, nil
}

func (l *lines) SetPrompt(p string) { l.prompts = append(l.prompts, p) }

func TestREPL(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out, errs bytes.Buffer
	sh := &cli.Shell{DB: db, Out: &out, Mode: "table"}
	in := &lines{in: []string{
		"CREATE TABLE a(x, y);",
		"INSERT INTO a VALUES",
		"  (1, 'one;'),",
		"  (2, 'two');",
		".tables",
		".schema a",
		".mode csv",
		"SELECT * FROM a ORDER BY x;",
		"SELECT nosuch;",
		".mode bogus",
		".quit",
		"SELECT 'not reached';",
	}}
	if err := sh.REPL(context.Background(), in, &errs); err != nil {
		t.Fatal(err)
	}
	want := "a\nCREATE TABLE a(x, y);\nx,y\n1,one;\n2,two\n"
	if out.String() != want {
		t.Fatalf("out %q, want %q", out.String(), want)
	}
	if !strings.Contains(errs.String(), "nosuch") || !strings.Contains(errs.String(), "usage: .mode") {
		t.Fatalf("errors: %q", errs.String())
	}
	if !strings.Contains(strings.Join(in.prompts, "|"), "...>") {
		t.Fatalf("no continuation prompt: %q", in.prompts)
	}
}
