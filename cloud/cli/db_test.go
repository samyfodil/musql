package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/samyfodil/musql/cloud/cli"
	"github.com/samyfodil/musql/cloud/state"
)

// TestDatabaseCommands drives the db commands' flag refusals, their JSON and
// confirmation paths, a piped shell script, and a viewer's shell, which falls
// back to a read-only token.
func TestDatabaseCommands(t *testing.T) {
	h := start(t)
	if _, err := h.try("db", "list"); err == nil || !strings.Contains(err.Error(), "no server URL") {
		t.Fatalf("no server: %v", err)
	}
	h.login()
	if out := h.run("db", "list"); strings.TrimSpace(out) != "NAME  GROUP  URL" {
		t.Fatalf("the personal org's databases: %q", out)
	}
	h.run("org", "create", "acme")
	h.run("config", "set", "org", "acme")
	h.run("db", "create", "app")
	h.run("db", "shell", "app", "CREATE TABLE t(x); INSERT INTO t VALUES (1), (2);")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"db", "create", "x", "--from-file", "a.musq", "--from-db", "app"}, "cannot be combined"},
		{[]string{"db", "create", "x", "--resume", "abc"}, "--resume needs --from-file"},
		{[]string{"db", "create", "x", "--timestamp", "2026-01-01T00:00:00Z"}, "--timestamp needs --from-db"},
		{[]string{"db", "create", "x", "--from-db", "app", "--timestamp", "yesterday"}, "bad timestamp"},
		{[]string{"db", "branch", "app", "x", "--timestamp", "yesterday"}, "bad timestamp"},
		{[]string{"db", "restore", "app"}, "--timestamp is required"},
		{[]string{"db", "restore", "app", "--timestamp", "soon"}, "bad timestamp"},
		{[]string{"db", "tokens", "create", "app", "-e", "soon"}, "soon"},
		{[]string{"db", "destroy", "app"}, "not deleted"},
		{[]string{"db", "tokens", "invalidate", "app"}, "nothing invalidated"},
		{[]string{"db", "show", "nope"}, "not found"},
		{[]string{"db", "inspect", "nope"}, "not found"},
		{[]string{"db", "shell", "nope"}, "not found"},
		{[]string{"db", "rename", "nope", "x"}, "not found"},
		{[]string{"db", "export", "nope", "-o", filepath.Join(t.TempDir(), "x.musq")}, "not found"},
	} {
		if _, err := h.try(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}

	if out := unspaced(h.run("db", "create", "j", "--json")); !strings.Contains(out, `"name":"j"`) {
		t.Fatalf("create --json: %q", out)
	}
	if out := unspaced(h.run("db", "show", "app", "--json")); !strings.Contains(out, `"name":"app"`) || !strings.Contains(out, `"org":"acme"`) {
		t.Fatalf("show --json: %q", out)
	}
	if out := h.run("db", "inspect", "app", "--json"); !strings.Contains(out, `"points"`) {
		t.Fatalf("inspect --json: %q", out)
	}
	h.run("db", "branch", "app", "b")
	if out := h.run("db", "show", "b"); !strings.Contains(out, "Parent:") || !strings.Contains(out, "Parent at:") {
		t.Fatalf("a branch's parent: %q", out)
	}
	h.run("db", "create", "c", "--from-db", "app", "--timestamp", time.Now().UTC().Format(time.RFC3339Nano))
	if out := h.run("db", "shell", "c", "SELECT count(*) AS n FROM t;"); out != "n\n2\n" {
		t.Fatalf("a branch at a time: %q", out)
	}
	if out := h.run("db", "restore", "app", "--timestamp", time.Now().UTC().Format(time.RFC3339Nano)); !strings.Contains(out, "Restored into app") {
		t.Fatalf("restore in place: %q", out)
	}

	if tok := h.run("db", "tokens", "create", "default", "--group"); strings.Count(tok, ".") != 2 {
		t.Fatalf("a group token: %q", tok)
	}
	h.run("db", "tokens", "invalidate", "default", "--group", "--yes")
	if _, errs, err := h.tryEnv(cli.Env{Stdin: strings.NewReader("y\n")}, "db", "tokens", "invalidate", "app"); err != nil || !strings.Contains(errs, "[y/N]") {
		t.Fatalf("invalidate after a yes: %q %v", errs, err)
	}
	if out, _, err := h.tryEnv(cli.Env{Stdin: strings.NewReader("yes\n")}, "db", "destroy", "j"); err != nil || !strings.Contains(out, "Deleted j") {
		t.Fatalf("destroy after a yes: %q %v", out, err)
	}

	if out, _, err := h.tryEnv(cli.Env{Stdin: strings.NewReader("SELECT x FROM t ORDER BY x;")}, "db", "shell", "app"); err != nil || out != "x\n1\n2\n" {
		t.Fatalf("a piped script: %q %v", out, err)
	}
	if _, _, err := h.tryEnv(cli.Env{Stdin: iotest.ErrReader(errors.New("the pipe broke"))}, "db", "shell", "app"); err == nil {
		t.Fatal("a shell ran a script it could not read")
	}
	notTTY, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer notTTY.Close()
	if _, _, err := h.tryEnv(cli.Env{Interactive: true, StdinFd: int(notTTY.Fd())}, "db", "shell", "app"); err == nil {
		t.Fatal("an interactive shell started on a file")
	}

	for name, out := range map[string]string{
		"into no directory": filepath.Join(t.TempDir(), "none", "x.musq"),
		"over a directory":  t.TempDir(),
	} {
		if _, err := h.try("db", "export", "app", "-o", out); err == nil {
			t.Errorf("an export %s", name)
		}
	}

	t.Chdir(t.TempDir())
	if _, errs, err := h.tryEnv(cli.Env{}, "db", "export", "app"); err != nil || !strings.Contains(errs, "Wrote app.musq") {
		t.Fatalf("export to the default name: %q %v", errs, err)
	}
	if _, err := os.Stat("app.musq"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dan, err := h.srv.Store().CreateUser(ctx, "dan@example.com", "Dan", state.HashPassword("dan-password"), false)
	if err != nil {
		t.Fatal(err)
	}
	h.run("org", "members", "invite", "dan@example.com", "--role", "viewer")
	invs, err := h.srv.Store().EmailInvites(ctx, "dan@example.com")
	if err != nil || len(invs) != 1 {
		t.Fatalf("dan's invites: %v %v", invs, err)
	}
	if _, err := h.srv.Store().AcceptInvite(ctx, invs[0].ID, dan); err != nil {
		t.Fatal(err)
	}
	_, secret, err := h.srv.Store().CreatePlatformToken(ctx, dan.ID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	viewer := cli.Env{Token: secret}
	if out, _, err := h.tryEnv(viewer, "db", "shell", "app", "SELECT count(*) AS n FROM t;", "--org", "acme"); err != nil || out != "n\n2\n" {
		t.Fatalf("a viewer reads: %q %v", out, err)
	}
	if _, _, err := h.tryEnv(viewer, "db", "shell", "app", "INSERT INTO t VALUES (3);", "--org", "acme"); err == nil {
		t.Fatal("a viewer's shell wrote")
	}
}

// unspaced drops protojson's spacing, which varies from build to build.
func unspaced(s string) string { return strings.ReplaceAll(s, " ", "") }
