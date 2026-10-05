package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNotIndexedAnswers verifies NOT INDEXED scan order against C SQLite.
//
// NOT INDEXED forces full-scan in rowid order instead of index order.
func TestNotIndexedAnswers(t *testing.T) {
	// Rowid and index order differ: descending keys show rowid scan yields
	// 30,20,10 while index walk yields 10,20,30.
	const fx = `CREATE TABLE t(k, v);
		CREATE INDEX ix ON t(k);
		INSERT INTO t VALUES(30,'a'),(20,'b'),(10,'c'),(20,'d');`
	const fxpk = `CREATE TABLE p(id INTEGER PRIMARY KEY, k);
		CREATE INDEX pix ON p(k);
		INSERT INTO p VALUES(3,30),(2,20),(1,10);`
	for _, tc := range []struct{ name, sql string }{
		{"no order by, indexed", fx + `SELECT k, v FROM t WHERE k=20`},
		{"no order by, not indexed", fx + `SELECT k, v FROM t NOT INDEXED WHERE k=20`},
		{"whole table not indexed", fx + `SELECT k, v FROM t NOT INDEXED`},
		{"whole table indexed", fx + `SELECT k, v FROM t`},
		{"aggregate over a not indexed scan", fx + `SELECT group_concat(k) FROM t NOT INDEXED`},
		{"aggregate over an ordinary scan", fx + `SELECT group_concat(k) FROM t`},
		{"not indexed with an inequality", fx + `SELECT k FROM t NOT INDEXED WHERE k>10`},
		{"not indexed with ORDER BY still sorts", fx + `SELECT k FROM t NOT INDEXED WHERE k>=10 ORDER BY k`},
		{"not indexed count", fx + `SELECT count(*) FROM t NOT INDEXED WHERE k=20`},
		// The INTEGER PRIMARY KEY is NOT taken away by NOT INDEXED.
		{"rowid lookup survives not indexed", fxpk + `SELECT k FROM p NOT INDEXED WHERE id=2`},
		{"rowid range survives not indexed", fxpk + `SELECT id, k FROM p NOT INDEXED WHERE id>=2`},
		{"secondary index is taken away", fxpk + `SELECT id FROM p NOT INDEXED WHERE k=20`},
		// A join where the inner side carries the hint.
		{"join inner not indexed", fx + `SELECT t.k, u.v FROM t, t AS u NOT INDEXED WHERE u.k=t.k AND t.k=20 ORDER BY t.k, u.v`},
		{"join inner plain", fx + `SELECT t.k, u.v FROM t, t AS u WHERE u.k=t.k AND t.k=20 ORDER BY t.k, u.v`},
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
