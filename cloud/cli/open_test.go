package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/cloud/cli"
)

func TestOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.musq")
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := cli.Run(context.Background(), cli.Env{Dir: t.TempDir(), Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: io.Discard},
			append([]string{"open", path}, args...))
		return out.String(), err
	}
	if _, err := run("", "CREATE TABLE people(id INTEGER, name TEXT); BEGIN; INSERT INTO people VALUES (42, 'John'); COMMIT;"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("", "SELECT id, name FROM people;"); err != nil || out != "id  name\n42  John\n" {
		t.Fatalf("query: %q %v", out, err)
	}
	if out, err := run("SELECT count(*) AS n FROM people;\n", "--json"); err != nil || out != "[\n  {\"n\": 1}\n]\n" {
		t.Fatalf("piped: %q %v", out, err)
	}
	if _, err := run("", "SELECT nosuch FROM people;"); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("bad column: %v", err)
	}

	// The file is an ordinary musql database.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow("SELECT name FROM people WHERE id = 42").Scan(&name); err != nil || name != "John" {
		t.Fatalf("read back: %q %v", name, err)
	}
}

func TestShellTimer(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out, errs bytes.Buffer
	sh := &cli.Shell{DB: db, Out: &out, Mode: "table"}
	in := &lines{in: []string{".timer on", "SELECT 1 AS one;", ".timer off", "SELECT 2 AS two;", ".timer sometimes"}}
	if err := sh.REPL(context.Background(), in, &errs); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Count(got, "Run Time: real ") != 1 || !strings.HasPrefix(got, "one\n1\nRun Time: real ") || !strings.HasSuffix(got, "two\n2\n") {
		t.Fatalf("out %q", got)
	}
	if !strings.Contains(errs.String(), "usage: .timer on|off") {
		t.Fatalf("errors %q", errs.String())
	}
}
