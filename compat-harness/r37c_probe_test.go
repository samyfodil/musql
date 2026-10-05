package compat

// Scratch probe for stream r37c (derived tables / CTEs / the flattener's LIMIT
// transfer). Prints both engines' answers for a hand-written statement list so a
// shape-space cell can be inspected directly. Not a gate -- see the r37c gates
// for those.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const r37cFixture = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
`

func r37cSplit(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TestR37CProbe runs R37C_SQL (semicolon-separated, prepended by the shape-space
// fixture unless R37C_NOFIX is set) through both engines and logs each answer.
func TestR37CProbe(t *testing.T) {
	sql := os.Getenv("R37C_SQL")
	if sql == "" {
		t.Skip("set R37C_SQL")
	}
	stmts := r37cSplit(sql)
	if os.Getenv("R37C_NOFIX") == "" {
		stmts = append(r37cSplit(r37cFixture), stmts...)
	}
	m := run(t, "musql", stmts)
	cg := run(t, "cgo", stmts)
	for i, s := range stmts {
		mb, _ := json.Marshal(m[i])
		cb, _ := json.Marshal(cg[i])
		mark := "  "
		if string(mb) != string(cb) {
			mark = "!!"
		}
		t.Logf("%s [%d] %s\n     cgo:    %s\n     musql: %s", mark, i, s, cb, mb)
	}
}

// r37cRunDirect runs stmts in-process through driverName and returns one line
// per statement -- including the ERROR TEXT, which the worker deliberately
// hides so a mutual rejection reads as agreement.
func r37cRunDirect(t *testing.T, driverName, dsn string, stmts []string) []string {
	t.Helper()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := make([]string, len(stmts))
	for i, s := range stmts {
		rows, qerr := db.Query(s)
		if qerr != nil {
			out[i] = "ERR: " + qerr.Error()
			continue
		}
		cols, cerr := rows.Columns()
		if cerr != nil {
			rows.Close()
			out[i] = "ERR(cols): " + cerr.Error()
			continue
		}
		line := fmt.Sprintf("cols=%v rows=", cols)
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for j := range vals {
				ptrs[j] = &vals[j]
			}
			if serr := rows.Scan(ptrs...); serr != nil {
				line += "SCANERR:" + serr.Error()
				break
			}
			line += fmt.Sprintf("%v;", vals)
		}
		if rerr := rows.Err(); rerr != nil {
			line += " ERR:" + rerr.Error()
		}
		rows.Close()
		out[i] = line
	}
	return out
}

// TestR37CDirect is TestR37CProbe with error text.
func TestR37CDirect(t *testing.T) {
	raw := os.Getenv("R37C_SQL")
	if raw == "" {
		t.Skip("set R37C_SQL")
	}
	stmts := r37cSplit(raw)
	if os.Getenv("R37C_NOFIX") == "" {
		stmts = append(r37cSplit(r37cFixture), stmts...)
	}
	dir := t.TempDir()
	cg := r37cRunDirect(t, "sqlite3", filepath.Join(dir, "c.db"), stmts)
	mu := r37cRunDirect(t, "sqlite", filepath.Join(dir, "m.db"), stmts)
	for i, s := range stmts {
		mark := "  "
		if cg[i] != mu[i] {
			mark = "!!"
		}
		t.Logf("%s [%d] %s\n     cgo:    %s\n     musql: %s", mark, i, s, cg[i], mu[i])
	}
}
