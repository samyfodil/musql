package compat

// r37a scratch probe: prints both engines' answers for hand-chosen OR shapes.
// Not a gate -- it never fails; it exists so a rule can be READ off the oracle
// while WHERE_MULTI_OR is being ported. Deleted before the branch lands if it
// has not grown into a gate.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const r37aFixture = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
`

func r37aStmts(schema, tail string) []string {
	var out []string
	for _, s := range strings.Split(r37aFixture+schema, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return append(out, tail)
}

func TestR37aOrProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("probe")
	}
	schemas := strings.Split(os.Getenv("R37A_SCHEMAS"), ";;")
	if len(schemas) == 1 && schemas[0] == "" {
		schemas = []string{
			``,
			`CREATE INDEX i1 ON t(a);`,
			`CREATE INDEX i1 ON t(a); CREATE INDEX i2 ON t(b);`,
			`CREATE INDEX i1 ON t(a,b);`,
			`CREATE INDEX i1 ON t(b,a);`,
		}
	}
	queries := strings.Split(os.Getenv("R37A_QUERIES"), ";;")
	if len(queries) == 1 && queries[0] == "" {
		queries = []string{
			`SELECT * FROM t WHERE a<2 OR a>2`,
			`SELECT * FROM t WHERE a=1 OR b='x'`,
			`EXPLAIN QUERY PLAN SELECT * FROM t WHERE a<2 OR a>2`,
			`EXPLAIN QUERY PLAN SELECT * FROM t WHERE a=1 OR b='x'`,
		}
	}
	for _, sch := range schemas {
		for _, q := range queries {
			stmts := r37aStmts(sch, q)
			m := run(t, "musql", stmts)
			cg := run(t, "cgo", stmts)
			last := len(stmts) - 1
			mb, _ := json.Marshal(m[last])
			cb, _ := json.Marshal(cg[last])
			verdict := "AGREE"
			if string(mb) != string(cb) {
				verdict = "DIFFER"
			}
			t.Logf("[%s] sch=%q q=%q\n   cgo: %s\n   mus: %s", verdict, sch, q, cb, mb)
		}
	}
}
