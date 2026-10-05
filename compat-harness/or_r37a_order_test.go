package compat

// This file tests the WHERE_MULTI_OR row order against the oracle.
// The row order is a concatenation of sub-scans per disjunct, de-duplicated by rowid.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r37aOrderFixture creates test data with duplicate values for index testing.
const r37aOrderFixture = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
`

var r37aOrderSchemas = []struct{ name, ddl string }{
	{"noidx", ``},
	{"idx-a", `CREATE INDEX i1 ON t(a);`},
	{"idx-a-desc", `CREATE INDEX i1 ON t(a DESC);`},
	{"idx-ab", `CREATE INDEX i1 ON t(a,b);`},
	{"idx-ba", `CREATE INDEX i1 ON t(b,a);`},
	{"two-idx", `CREATE INDEX i1 ON t(a); CREATE INDEX i2 ON t(b);`},
	{"idx-cover", `CREATE INDEX i1 ON t(a,b,c);`},
	{"idx-a-nocase", `CREATE INDEX i1 ON t(a COLLATE NOCASE);`},
	{"idx-uniq-b", `CREATE UNIQUE INDEX i1 ON t(b);`},
}

// Queries test row order, using group_concat to expose ordering.
var r37aOrderQueries = []string{
	// Basic disjunctions.
	`SELECT * FROM t WHERE a<2 OR a>2`,
	`SELECT * FROM t WHERE a=1 OR b='x'`,
	`SELECT group_concat(b) FROM t WHERE a<2 OR a>2`,
	`SELECT group_concat(b) FROM t WHERE a=1 OR b='x'`,
	// Disjunct order affects row order.
	`SELECT group_concat(b) FROM t WHERE a>2 OR a<2`,
	`SELECT group_concat(b) FROM t WHERE b='x' OR a=1`,
	// De-duplication by rowid.
	`SELECT group_concat(b) FROM t WHERE b='x' OR a<3`,
	`SELECT group_concat(b) FROM t WHERE a<3 OR b='x'`,
	// Three disjuncts and other columns.
	`SELECT group_concat(b) FROM t WHERE a=1 OR b='z' OR c=10`,
	`SELECT group_concat(b) FROM t WHERE a>=2 OR b='Y' OR a IS NULL`,
	// LIMIT and ORDER BY.
	`SELECT a,b FROM t WHERE a<2 OR a>2 LIMIT 2`,
	`SELECT a,b FROM t WHERE a=1 OR b='x' LIMIT 2`,
	`SELECT a,b FROM t WHERE a<2 OR a>2 ORDER BY length(b) DESC`,
	// Aggregates.
	`SELECT a, b, max(c) FROM t WHERE a=1 OR b='x'`,
	`SELECT b, count(*) FROM t WHERE a<2 OR a>2 GROUP BY b`,
	// OR combined with AND.
	`SELECT group_concat(b) FROM t WHERE c<=40 AND (a<2 OR a>2)`,
	`SELECT group_concat(b) FROM t WHERE b='x' AND (a<2 OR a>2)`,
	`SELECT group_concat(b) FROM t WHERE (a<2 OR a>2) AND (b='x' OR b='Y')`,
	// OR converted to IN.
	`SELECT group_concat(b) FROM t WHERE a=1 OR a=3`,
	// COLLATE prevents MULTI_OR.
	`SELECT group_concat(b) FROM t WHERE a=1 OR b='x' COLLATE NOCASE`,
	`SELECT group_concat(b) FROM t WHERE a COLLATE NOCASE=1 OR b='z'`,
	// Unindexable disjuncts.
	`SELECT group_concat(b) FROM t WHERE a=1 OR c=30`,
	// ROWID disjuncts.
	`SELECT group_concat(b) FROM t WHERE rowid>2 OR rowid<2`,
	`SELECT group_concat(b) FROM t WHERE rowid<2 OR rowid>2`,
	`SELECT group_concat(b) FROM t WHERE rowid>4 OR rowid<2`,
	`SELECT a,b FROM t WHERE rowid>4 OR rowid<2 LIMIT 2`,
	`SELECT group_concat(b) FROM t WHERE c>40 OR c<20`,
	// DISTINCT.
	`SELECT DISTINCT a FROM t WHERE a<2 OR a>2`,
	`SELECT DISTINCT b FROM t WHERE a=1 OR b='x'`,
}

// r37aOrOrderKnownOpen lists two known-open cases that are not closed.
// Both are shapes the engine refuses to optimize with MULTI_OR.
func TestR37aOrOrder(t *testing.T) {
	for _, sc := range r37aOrderSchemas {
		var pre []string
		for _, s := range strings.Split(r37aOrderFixture+sc.ddl, ";") {
			if s = strings.TrimSpace(s); s != "" {
				pre = append(pre, s)
			}
		}
		for _, q := range r37aOrderQueries {
			stmts := append(append([]string(nil), pre...), q)
			differ(t, "r37a-order/"+sc.name, stmts)
		}
	}
}

// TestR37aOrOrderKnownOpen measures the known-open cases to track progress.
func TestR37aOrOrderKnownOpen(t *testing.T) {
	open := []string{
		`SELECT group_concat(b) FROM t WHERE a>2 OR a=2`,
		`SELECT group_concat(b) FROM t WHERE a=1 OR a IS NOT NULL`,
	}
	n, total := 0, 0
	for _, sc := range r37aOrderSchemas {
		var pre []string
		for _, s := range strings.Split(r37aOrderFixture+sc.ddl, ";") {
			if s = strings.TrimSpace(s); s != "" {
				pre = append(pre, s)
			}
		}
		for _, q := range open {
			stmts := append(append([]string(nil), pre...), q)
			m := run(t, "musql", stmts)
			cg := run(t, "cgo", stmts)
			last := len(stmts) - 1
			total++
			mb, _ := json.Marshal(m[last])
			cb, _ := json.Marshal(cg[last])
			if string(mb) != string(cb) {
				n++
				t.Logf("  still open [%s] %s\n    cgo: %s\n    mus: %s", sc.name, q, cb, mb)
			}
		}
	}
	t.Logf("R37A known-open: %d of %d", n, total)
}

// TestR37aOrOrderReverse tests the same shapes with reverse_unordered_selects.
func TestR37aOrOrderReverse(t *testing.T) {
	for _, sc := range r37aOrderSchemas {
		var pre []string
		for _, s := range strings.Split(r37aOrderFixture+sc.ddl, ";") {
			if s = strings.TrimSpace(s); s != "" {
				pre = append(pre, s)
			}
		}
		pre = append(pre, "PRAGMA reverse_unordered_selects=ON")
		for _, q := range r37aOrderQueries {
			stmts := append(append([]string(nil), pre...), q)
			differ(t, "r37a-order-rev/"+sc.name, stmts)
		}
	}
}
