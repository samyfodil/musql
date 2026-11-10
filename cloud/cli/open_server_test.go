package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/cloud/cli"
	"github.com/samyfodil/musql/hrana"
)

// TestOpenServer: open given a musqld URL runs SQL on the server over Hrana,
// with the token from --auth-token or the environment's.
func TestOpenServer(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "srv.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := httptest.NewServer(hrana.NewServer(db, hrana.WithAuthToken("secret")))
	defer srv.Close()
	run := func(env cli.Env, stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		env.Dir, env.Stdin, env.Stdout, env.Stderr = t.TempDir(), strings.NewReader(stdin), &out, io.Discard
		err := cli.Run(context.Background(), env, append([]string{"open", srv.URL}, args...))
		return out.String(), err
	}
	if _, err := run(cli.Env{}, "", "--auth-token", "secret", "CREATE TABLE people(id INTEGER, name TEXT); INSERT INTO people VALUES (42, 'John');"); err != nil {
		t.Fatal(err)
	}
	if out, err := run(cli.Env{ServerToken: "secret"}, "", "SELECT id, name FROM people;"); err != nil || out != "id  name\n42  John\n" {
		t.Fatalf("query: %q %v", out, err)
	}
	if out, err := run(cli.Env{ServerToken: "secret"}, "SELECT count(*) AS n FROM people;\n", "--json"); err != nil || out != "[\n  {\"n\": 1}\n]\n" {
		t.Fatalf("piped: %q %v", out, err)
	}
	if _, err := run(cli.Env{}, "", "--auth-token", "wrong", "SELECT 1;"); err == nil {
		t.Fatal("a wrong token was accepted")
	}
	var name string
	if err := db.QueryRow("SELECT name FROM people WHERE id = 42").Scan(&name); err != nil || name != "John" {
		t.Fatalf("read back: %q %v", name, err)
	}
}
