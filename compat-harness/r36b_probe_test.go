package compat

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestR36bProbe is a scratch probe: R36B_SQL holds ';'-separated statements, run
// against both engines with their outputs printed side by side. Not a gate.
func TestR36bProbe(t *testing.T) {
	sql := os.Getenv("R36B_SQL")
	if sql == "" {
		t.Skip("set R36B_SQL")
	}
	var stmts []string
	for _, s := range strings.Split(r36bFixture+sql, ";") {
		if s = strings.TrimSpace(s); s != "" {
			stmts = append(stmts, s)
		}
	}
	m := run(t, "musql", stmts)
	c := run(t, "cgo", stmts)
	for i := range stmts {
		mb, _ := json.Marshal(m[i])
		cb, _ := json.Marshal(c[i])
		if string(mb) == string(cb) {
			if os.Getenv("R36B_V") != "" {
				t.Logf("SAME %s\n  both:   %s", stmts[i], cb)
			}
			continue
		}
		t.Errorf("STMT %s\n  cgo:    %s\n  musql: %s", stmts[i], cb, mb)
	}
}

const r36bFixture = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
`
