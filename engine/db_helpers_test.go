package engine

import (
	"fmt"
	"strings"
	"testing"
)

// Database helpers the engine's tests share: build one with SQL, run more
// statements on it, and render what it answers.

// buildDB builds a database at path from stmts.
func buildDB(t testing.TB, path string, stmts ...string) {
	t.Helper()
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := n.Exec(s); err != nil {
			n.Discard()
			t.Fatalf("%q: %v", s, err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

// execDB runs stmts against an existing database and commits them.
func execDB(t testing.TB, path string, stmts ...string) {
	t.Helper()
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := n.Exec(s); err != nil {
			n.Discard()
			t.Fatalf("%q: %v", s, err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

// queryDB renders q's answer over the database at path, "" and the
// error when it fails.
func queryDB(t *testing.T, path, q string) (string, error) {
	t.Helper()
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	_, rows, err := n.Query(q, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range rows {
		for _, c := range r {
			fmt.Fprintf(&b, "%d:%d:%v:%q|", c.Typ, c.I, c.F, string(c.S))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// catalogOf renders every schema object's type, name, table and SQL.
func catalogOf(t *testing.T, path string) string {
	t.Helper()
	out, err := queryDB(t, path, `SELECT type, name, tbl_name, coalesce(sql,'') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func seqOf(t *testing.T, path string) string {
	t.Helper()
	out, err := queryDB(t, path, `SELECT name, seq FROM sqlite_sequence ORDER BY name`)
	if err != nil {
		return "<no sqlite_sequence>"
	}
	return out
}

// allTablesOf renders every row of every table, so a restore that invented rows
// in a table nobody thought to check is still caught.
func allTablesOf(t *testing.T, path string) string {
	t.Helper()
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := n.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`, nil)
	n.Discard()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rows {
		name := string(r[0].S)
		out, qerr := queryDB(t, path, `SELECT * FROM `+quoteIdent(name)+` ORDER BY rowid`)
		if qerr != nil {
			t.Fatalf("reading %s: %v", name, qerr)
		}
		b.WriteString(name + ":\n" + out)
	}
	return b.String()
}

func itoaForTest(n int64) string { return fmt.Sprint(n) }
