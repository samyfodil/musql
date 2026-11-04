package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
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

// TestShellRead: .read runs a file's statements and dot-commands as if typed,
// its last statement with or without a ";", and carries on past an error as
// the prompt does; a .quit in it ends the shell, and a file reading itself
// stops.
func TestShellRead(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := write("a.txt", ".mode csv\r\nselect 1 a,2 b")
	b := write("b with space.sql", ".read "+a+"\nSELECT nosuch;\nSELECT 3 AS c;\n")
	q := write("q.sql", "SELECT 4 AS d;\n.quit\n")
	self := write("self.sql", ".read "+filepath.Join(dir, "self.sql")+"\n")
	run := func(in ...string) (string, string) {
		var out, errs bytes.Buffer
		sh := &cli.Shell{DB: db, Out: &out, Mode: "table"}
		if err := sh.REPL(context.Background(), &lines{in: in}, &errs); err != nil {
			t.Fatal(err)
		}
		return out.String(), errs.String()
	}
	if out, errs := run(".read " + a); out != "a,b\n1,2\n" || errs != "" {
		t.Fatalf("the issue's file: %q %q", out, errs)
	}
	if out, errs := run(`.read "`+b+`"`, "SELECT 5 AS e;"); out != "a,b\n1,2\nc\n3\ne\n5\n" || !strings.Contains(errs, "nosuch") {
		t.Fatalf("nested, past an error: %q %q", out, errs)
	}
	if out, _ := run(".read "+q, "SELECT 6 AS f;"); out != "d\n4\n" {
		t.Fatalf(".quit in a file: %q", out)
	}
	if _, errs := run(".read " + self); !strings.Contains(errs, "one inside another") {
		t.Fatalf("a file reading itself: %q", errs)
	}
	if _, errs := run(".read "+filepath.Join(dir, "none.sql"), ".read"); !strings.Contains(errs, "none.sql") || !strings.Contains(errs, "usage: .read FILE") {
		t.Fatalf("no file: %q", errs)
	}
}
