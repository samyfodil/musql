package cli_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/musql/cloud/cli"
)

// TestCommandsPassOnARefusal: every command hands back the error of a call the
// server refuses, here for a token it does not know.
func TestCommandsPassOnARefusal(t *testing.T) {
	h := start(t)
	file := filepath.Join(t.TempDir(), "x.musq")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.run("config", "set", "url", h.url)
	bad := cli.Env{Token: "not-a-token"}
	for _, args := range [][]string{
		{"auth", "whoami"},
		{"auth", "api-tokens", "list"},
		{"auth", "api-tokens", "mint", "x"},
		{"auth", "api-tokens", "revoke", "x"},
		{"org", "list"},
		{"org", "create", "x"},
		{"org", "switch", "x"},
		{"org", "members", "list"},
		{"org", "members", "invite", "a@example.com"},
		{"org", "members", "rm", "a@example.com"},
		{"org", "members", "role", "a@example.com", "member"},
		{"group", "list"},
		{"group", "create", "x"},
		{"group", "destroy", "x", "--yes"},
		{"group", "locations", "list"},
		{"group", "locations", "add", "x", "fra"},
		{"group", "locations", "rm", "x", "fra"},
		{"db", "list"},
		{"db", "create", "x"},
		{"db", "create", "x", "--from-file", file},
		{"db", "import", file},
		{"db", "show", "x"},
		{"db", "destroy", "x", "--yes"},
		{"db", "rename", "x", "y"},
		{"db", "tokens", "create", "x"},
		{"db", "tokens", "invalidate", "x", "--yes"},
		{"db", "export", "x", "-o", filepath.Join(t.TempDir(), "out.musq")},
		{"db", "restore", "x", "--timestamp", now},
		{"db", "branch", "x", "y"},
		{"db", "inspect", "x"},
		{"db", "shell", "x", "SELECT 1"},
	} {
		// Without --org, the command asks the server whose org is current first.
		for _, extra := range [][]string{{"--org", "acme"}, nil} {
			args := append(append([]string{}, args...), extra...)
			if _, _, err := h.tryEnv(bad, args...); err == nil || !strings.Contains(err.Error(), "not signed in") {
				t.Errorf("%s: %v", strings.Join(args, " "), err)
			}
		}
	}
	if _, err := h.try("auth", "token"); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("a token without a sign-in: %v", err)
	}
	if _, err := h.try("db", "export", "x", "--org", "acme", "-o", filepath.Join(t.TempDir(), "y.musq")); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("an export without a sign-in: %v", err)
	}
}

// TestLogoutEndsSignInEitherWay: logout forgets the sign-in whether or not
// there is one, a server to tell, or a server that answers; and login opens
// the browser unless told not to.
func TestLogoutEndsSignInEitherWay(t *testing.T) {
	h := start(t)
	h.run("auth", "logout")
	var opened string
	h.loginWith(func(u string) error {
		opened = u
		return nil
	})
	if !strings.Contains(opened, "/oidc/device/verify") {
		t.Fatalf("the browser opened %q", opened)
	}
	edit := func(f func(*cli.Config)) {
		t.Helper()
		c, err := cli.LoadConfig(h.dir)
		if err != nil {
			t.Fatal(err)
		}
		f(c)
		if err := c.Save(h.dir); err != nil {
			t.Fatal(err)
		}
	}
	edit(func(c *cli.Config) { c.URL = "http://127.0.0.1:1" })
	if _, errs, err := h.tryEnv(cli.Env{}, "auth", "logout"); err != nil || !strings.Contains(errs, "could not revoke") {
		t.Fatalf("a logout the server never heard: %q %v", errs, err)
	}
	edit(func(c *cli.Config) { c.URL, c.RefreshToken = "", "kept" })
	h.run("auth", "logout")
	if out := h.run("config", "show"); !strings.Contains(out, "signed in: no") {
		t.Fatalf("after logout: %q", out)
	}
}

// TestLoginRefusals: login needs a server URL and a server that answers, and
// ends when the sign-in is denied.
func TestLoginRefusals(t *testing.T) {
	h := start(t)
	if _, err := h.try("auth", "login", "--headless"); err == nil || !strings.Contains(err.Error(), "no server URL") {
		t.Errorf("a login with no server: %v", err)
	}
	if _, err := h.try("auth", "login", "--headless", "--url", "http://127.0.0.1:1"); err == nil {
		t.Error("a login to a server that is not there")
	}
	stderr := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(context.Background(), cli.Env{Dir: h.dir, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: stderr},
			[]string{"auth", "login", "--headless", "--url", h.url})
	}()
	re := regexp.MustCompile(`confirm the code (\S+)`)
	var code string
	for deadline := time.Now().Add(10 * time.Second); code == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(stderr.String()); m != nil {
			code = m[1]
		}
	}
	u, err := h.srv.Store().UserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Store().DecideDevice(context.Background(), code, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("a denied login: %v", err)
	}
}

// TestConfigAndRefreshFailures: a config file that does not parse stops every
// command, and a stored sign-in that cannot be refreshed, for want of a server
// or of a server that knows it, is no sign-in.
func TestConfigAndRefreshFailures(t *testing.T) {
	h := start(t)
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.try("config", "show"); err == nil {
		t.Error("ran with a config that does not parse")
	}
	for name, c := range map[string]cli.Config{
		"no server":            {RefreshToken: "kept"},
		"a server that forgot": {URL: h.url, RefreshToken: "unknown"},
	} {
		if err := c.Save(h.dir); err != nil {
			t.Fatal(err)
		}
		if _, err := h.try("auth", "whoami"); err == nil {
			t.Errorf("%s: signed in", name)
		}
	}
}
