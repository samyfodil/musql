package compat

// Probe that compares statement results from both engines side by side.
// Skipped unless TAIL_R30_PROBE names a file of statements to probe.

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func tailR30Cells(cols []string, rows [][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cols=%v rows=[", cols)
	for i, r := range rows {
		if i > 0 {
			b.WriteString(" | ")
		}
		if i > 40 {
			b.WriteString("...")
			break
		}
		b.WriteString(strings.Join(r, ","))
	}
	b.WriteString("]")
	return b.String()
}

func tailR30Go(godb *engine.Session, stmt string) string {
	if tclIsQuery(stmt) {
		cols, rows, err, panicked, pv := tclSafeGoQuery(godb, stmt)
		switch {
		case panicked:
			return fmt.Sprintf("PANIC: %v", pv)
		case err != nil:
			return "ERR: " + err.Error()
		}
		return tailR30Cells(cols, rows)
	}
	err, panicked, pv := tclSafeExecArgs(godb, stmt)
	switch {
	case panicked:
		return fmt.Sprintf("PANIC: %v", pv)
	case err != nil:
		return "ERR: " + err.Error()
	}
	return "ok"
}

func tailR30CGO(cdb *sql.DB, stmt string) string {
	if tclIsQuery(stmt) {
		cols, rows, err := tclRunCGOQuery(cdb, stmt)
		if err != nil {
			return "ERR: " + err.Error()
		}
		return tailR30Cells(cols, rows)
	}
	if _, err := cdb.Exec(stmt); err != nil {
		return "ERR: " + err.Error()
	}
	return "ok"
}

func TestTailR30Probe(t *testing.T) {
	script := os.Getenv("TAIL_R30_PROBE")
	if script == "" {
		t.Skip("set TAIL_R30_PROBE to a statement file")
	}
	f, err := os.Open(script)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var stmts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		stmts = append(stmts, line)
	}
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for i, s := range stmts {
		g := tailR30Go(godb, s)
		c := tailR30CGO(cdb, s)
		mark := "  "
		if g != c {
			mark = "**"
		}
		t.Logf("%s #%d %s\n     go : %s\n     cgo: %s", mark, i, s, g, c)
	}
}
