package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAggKeywordLiteralAnswers verifies aggregates of TRUE/FALSE literals
// work correctly in GROUP BY queries.
func TestAggKeywordLiteralAnswers(t *testing.T) {
	const fixture = `CREATE TABLE t(k,v); INSERT INTO t VALUES(1,10),(1,20),(2,30);`
	for _, tc := range []struct{ name, sql string }{
		{"max TRUE", fixture + `SELECT k, max(TRUE) FROM t GROUP BY k ORDER BY k`},
		{"sum FALSE", fixture + `SELECT k, sum(FALSE) FROM t GROUP BY k ORDER BY k`},
		{"group_concat TRUE", fixture + `SELECT k, group_concat(TRUE) FROM t GROUP BY k ORDER BY k`},
		{"count FILTER TRUE", fixture + `SELECT k, count(*) FILTER (WHERE TRUE) FROM t GROUP BY k ORDER BY k`},
		{"min of TRUE+v", fixture + `SELECT k, min(TRUE+v) FROM t GROUP BY k ORDER BY k`},
		{"sum CASE FALSE", fixture + `SELECT k, sum(CASE WHEN FALSE THEN 9 ELSE v END) FROM t GROUP BY k ORDER BY k`},
		// Column named "true" shadows the keyword.
		{"real column shadows", `CREATE TABLE s("true" INT, k); INSERT INTO s VALUES(7,1),(8,1); SELECT k, max(true) FROM s GROUP BY k ORDER BY k`},
		// A QUALIFIED spelling is a hard lookup with no fallback at all.
		{"qualified is an error", `CREATE TABLE u(k); INSERT INTO u VALUES(1); SELECT k, max(u.true) FROM u GROUP BY k ORDER BY k`},
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
