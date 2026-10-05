package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCaseSensitiveLikeReturning verifies that PRAGMA case_sensitive_like is
// respected inside RETURNING clauses.
func TestCaseSensitiveLikeReturning(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"table returning", `PRAGMA case_sensitive_like=ON; CREATE TABLE t(x); INSERT INTO t VALUES('AbC') RETURNING x LIKE 'ab%'`},
		{"vtab returning", `PRAGMA case_sensitive_like=ON; CREATE VIRTUAL TABLE r USING rtree(id,x0,x1); INSERT INTO r VALUES(1,2,3) RETURNING 'AbC' LIKE 'ab%'`},
		{"plain select control", `PRAGMA case_sensitive_like=ON; CREATE TABLE t(x); INSERT INTO t VALUES('AbC'); SELECT x LIKE 'ab%' FROM t`},
		{"off control", `PRAGMA case_sensitive_like=OFF; CREATE TABLE t(x); INSERT INTO t VALUES('AbC') RETURNING x LIKE 'ab%'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stmts []string
			for _, s := range strings.Split(tc.sql, ";") {
				if s = strings.TrimSpace(s); s != "" {
					stmts = append(stmts, s)
				}
			}
			m := run(t, "musql", stmts)
			c := run(t, "cgo", stmts)
			for i := range stmts {
				mb, _ := json.Marshal(m[i])
				cb, _ := json.Marshal(c[i])
				if string(mb) != string(cb) {
					t.Errorf("STMT %s\n  cgo:    %s\n  musql: %s", stmts[i], cb, mb)
				}
			}
		})
	}
}
