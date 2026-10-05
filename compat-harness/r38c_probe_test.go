package compat

// Probe to log order divergences in compound queries with ORDER BY.
// Skipped unless R38C_PROBE is set.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const r38cFixture = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
`

func r38cScript(setup, stmt string) []string {
	var out []string
	for _, s := range strings.Split(setup, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return append(out, stmt)
}

func TestR38CProbe(t *testing.T) {
	if os.Getenv("R38C_PROBE") == "" {
		t.Skip("scratch probe; set R38C_PROBE=1")
	}
	// Is the tied-key DESC order a FLATTENER question at all, or a plain
	// ORDER BY one? a=1 (integer, rowid 2) and a=1.0 (real, rowid 6) compare
	// equal, so their relative order is decided by the access path alone.
	schemas := []struct{ name, ddl string }{
		{"noidx", ``},
		{"idx-a", `CREATE INDEX i1 ON t(a);`},
		{"idx-cover", `CREATE INDEX i1 ON t(a,b,c);`},
	}
	stmts := []string{
		`SELECT a, typeof(a) FROM t ORDER BY a DESC`,
		`SELECT a, typeof(a) FROM t ORDER BY a`,
		`SELECT a, typeof(a) FROM t ORDER BY 1 DESC`,
		`SELECT a, typeof(a) FROM t WHERE a IN (1,2,3) ORDER BY a DESC`,
		`SELECT a, typeof(a) FROM t ORDER BY a DESC LIMIT 3`,
		`SELECT a, typeof(a) FROM (SELECT * FROM t) AS d ORDER BY a DESC`,
		`SELECT a, typeof(a) FROM t UNION ALL SELECT b, typeof(b) FROM t ORDER BY 1 DESC`,
	}
	for _, sc := range schemas {
		for _, s := range stmts {
			script := r38cScript(r38cFixture+sc.ddl, s)
			m := run(t, "musql", script)
			cg := run(t, "cgo", script)
			last := len(script) - 1
			mb, _ := json.Marshal(m[last])
			cb, _ := json.Marshal(cg[last])
			verdict := "AGREE"
			if string(mb) != string(cb) {
				verdict = "DIVERGE"
			}
			t.Logf("%-10s %-8s %s\n   cgo:  %s\n   mush: %s", sc.name, verdict, s, cb, mb)
		}
	}
}
