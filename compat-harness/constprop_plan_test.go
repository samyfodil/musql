package compat

import (
	"fmt"
	"testing"
)

// TestConstantPropagationPlansAsC: propagateConstants (select.c tag-select-0330)
// fixes every other occurrence of a column a top-level "COLUMN = CONSTANT" term
// pins, and the planner then reads "u.a = t.b" as "u.a = 2" -- no dependency on
// t, so u may run first -- and "t.c = t.b" as "t.c = 2", an index equality.
// Over the self-join EXISTS "FROM v, t WHERE v.r = t.d AND b = 2 AND EXISTS
// (SELECT 1 FROM t AS t2 WHERE t2.a = t.b)" C walks the EXISTS table first; the
// port walked t first and answered in a different order.
func TestConstantPropagationPlansAsC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX td ON t(d)", "CREATE INDEX tbd ON t(b, d)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t SELECT n, n % 5, 41 - n, n % 7 FROM s",
		"CREATE TABLE v(p INTEGER PRIMARY KEY, q, r)", "CREATE INDEX vq ON v(q)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO v SELECT n, n % 6, n % 4 FROM s",
		"CREATE TABLE w(k TEXT, m BLOB, n INTEGER)", "CREATE INDEX wk ON w(k)", "CREATE INDEX wm ON w(m)", "CREATE INDEX wn ON w(n)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO w SELECT n % 5, n % 4, n FROM s",
	}
	qs := []string{
		"SELECT group_concat(t.a || ':' || v.p) FROM v, t WHERE v.r = t.d AND b = 2 AND EXISTS (SELECT 1 FROM t AS t2 WHERE t2.a = t.b)",
		"SELECT group_concat(t.a || ':' || v.p) FROM t, v WHERE t.b = 2 AND v.q = t.b",
		"SELECT group_concat(t.a || ':' || v.p) FROM t, v WHERE v.q = t.b AND t.b = 2",
		"SELECT group_concat(t.a || ':' || v.p) FROM t, v WHERE v.q = t.d AND t.d = 3 AND v.r > 1",
		"SELECT group_concat(t.a || ':' || v.p) FROM t JOIN v ON v.q = t.b WHERE t.b = 1",
		"SELECT group_concat(t.a || ':' || v.p) FROM t LEFT JOIN v ON v.q = t.b WHERE t.b = 1",
		"SELECT group_concat(a) FROM t WHERE b = 2 AND c = b",
		"SELECT group_concat(a) FROM t WHERE d = 3 AND c > d",
		"SELECT group_concat(a) FROM t WHERE d = 3 AND b = d AND c > 5",
		"SELECT group_concat(a) FROM t WHERE b = -1 + 3 AND d = b",
		"SELECT group_concat(a) FROM t WHERE b = CAST(2 AS INTEGER) AND d = b",
		"SELECT group_concat(a) FROM t WHERE b = 2 COLLATE nocase AND d = b",
		"SELECT group_concat(w.n || ':' || v.p) FROM w, v WHERE w.k = '2' AND v.q = w.k",
		"SELECT group_concat(w.n || ':' || v.p) FROM w, v WHERE w.m = 2 AND v.q = w.m",
		"SELECT group_concat(w.n || ':' || v.p) FROM w, v WHERE w.m = 2 AND v.q >= w.m AND v.q < 5",
		"SELECT group_concat(w.n || ':' || v.p) FROM w, v WHERE w.n = 7 AND v.p = w.n + 0",
	}
	for _, stat := range []bool{false, true} {
		seed := append([]string{}, base...)
		if stat {
			seed = append(seed, "ANALYZE")
		}
		for i, q := range qs {
			differ(t, fmt.Sprintf("constprop %d stat=%v", i, stat), append(append([]string{}, seed...), q))
		}
	}

	// The rules on WHICH terms fix a column, each where breaking it moves a
	// plan: over a typeless x.e the constant reaches only a comparison's
	// operands (bHasAffBlob), so "t.c IN (x.e, 9)" keeps its dependency on x
	// and "v.r > x.e" -- TEXT on the left -- keeps its right operand; and a
	// "t.b = 2 COLLATE nocase" fixes nothing, its comparison not being BINARY.
	rules := []string{
		"CREATE TABLE t(a, b INTEGER, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX td ON t(d)", "CREATE INDEX tbd ON t(b, d)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t SELECT n, n % 5, 41 - n, n % 7 FROM s",
		"CREATE TABLE v(p INTEGER PRIMARY KEY, q, r TEXT)", "CREATE INDEX vq ON v(q)", "CREATE INDEX vr ON v(r)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO v SELECT n, n % 6, n % 4 FROM s",
		"CREATE TABLE x(e, f)", "CREATE INDEX xe ON x(e)", "CREATE INDEX xf ON x(f)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO x SELECT n % 5, n FROM s",
	}
	for i, c := range []struct {
		q    string
		stat bool
	}{
		{"SELECT count(*), group_concat(x.e || ':' || t.c) FROM x, t WHERE x.e = 2 AND t.c IN (x.e, 9)", true},
		{"SELECT count(*), group_concat(x.e || ':' || t.c) FROM x, t WHERE x.e = -1+3 AND t.c IN (x.e, 9)", true},
		{"SELECT count(*), group_concat(x.e || ':' || v.r) FROM x, v WHERE x.e = 2 AND v.r > x.e", true},
		{"SELECT count(*), group_concat(t.b || ':' || x.e) FROM t, x WHERE t.b = 2 COLLATE nocase AND x.e > t.b", false},
		{"SELECT count(*), group_concat(t.b || ':' || x.e) FROM t, x WHERE t.b = 2 COLLATE nocase AND x.e <= t.b", false},
	} {
		seed := append([]string{}, rules...)
		if c.stat {
			seed = append(seed, "ANALYZE")
		}
		differ(t, fmt.Sprintf("constprop rule %d", i), append(seed, c.q))
	}
}
