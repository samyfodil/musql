package compat

import "testing"

// TestAggregateOrderBy tests SQLite 3.44's aggregate ORDER BY syntax.
// Tests buffer sorting, DISTINCT handling, min/max, collation, and query ownership.
func TestAggregateOrderBy(t *testing.T) {
	fixture := []string{
		"CREATE TABLE n(i INTEGER PRIMARY KEY, x, y)",
		"INSERT INTO n VALUES(1, 1, 'b'),(2, 1.0, 'a'),(3, 2, 'a')",
		"CREATE TABLE c(i INTEGER PRIMARY KEY, x TEXT COLLATE NOCASE, y)",
		"INSERT INTO c VALUES(1,'a',2),(2,'A',1),(3,'b',3)",
		"CREATE TABLE j(i INTEGER PRIMARY KEY, x, y)",
		"INSERT INTO j VALUES(1,NULL,3),(2,5,1),(3,NULL,2),(4,5,0)",
		"CREATE TABLE big(x, y)",
		"INSERT INTO big VALUES(1, -9223372036854775808)",
		"CREATE TABLE g(k, a, d)",
		"INSERT INTO g VALUES(1,'p',3),(2,'q',1),(1,'r',2),(2,'s',2),(1,'t',1)",
	}
	cases := []struct {
		name  string
		stmts []string
	}{
		{"sort-keys", []string{
			"SELECT group_concat(y ORDER BY x) FROM j",
			"SELECT group_concat(y ORDER BY x NULLS LAST) FROM j",
			"SELECT group_concat(y ORDER BY x DESC) FROM j",
			"SELECT group_concat(y ORDER BY x DESC NULLS FIRST) FROM j",
			"SELECT group_concat(i ORDER BY y) FROM n",
			"SELECT group_concat(i ORDER BY 1) FROM n",
			"SELECT group_concat(i ORDER BY i DESC, 1) FROM n",
			"SELECT string_agg(i, '-' ORDER BY y, i DESC) FROM n",
			"SELECT group_concat(x, y ORDER BY i) FROM n",
			"SELECT group_concat(x ORDER BY x COLLATE NOCASE) FROM (SELECT 'b' x UNION ALL SELECT 'A' UNION ALL SELECT 'a')",
		}},
		{"distinct", []string{
			"SELECT group_concat(DISTINCT x ORDER BY x) FROM n",
			"SELECT group_concat(DISTINCT x ORDER BY y) FROM n",
			"SELECT group_concat(DISTINCT x ORDER BY x) FROM c",
			"SELECT group_concat(DISTINCT x ORDER BY x DESC) FROM c",
			"SELECT group_concat(DISTINCT x ORDER BY y) FROM c",
			"SELECT count(DISTINCT x ORDER BY y) FROM n",
		}},
		{"kinds", []string{
			"SELECT json_group_array(x ORDER BY y) FROM j",
			"SELECT json_group_array(DISTINCT x ORDER BY y) FROM j",
			"SELECT json_group_array(DISTINCT x ORDER BY x) FROM j",
			"SELECT json_group_object(i, x ORDER BY y) FROM n",
			"SELECT sum(x ORDER BY y), total(x ORDER BY y DESC), avg(x ORDER BY y) FROM n",
			"SELECT count(ORDER BY nosuch) FROM big",
			"SELECT count(x ORDER BY y) FROM n",
		}},
		{"minmax", []string{
			"SELECT min(x ORDER BY abs(y)) FROM big",
			"SELECT max(x ORDER BY nosuch) FROM big",
		}},
		{"errors", []string{
			"SELECT group_concat(x ORDER BY abs(y)) FROM big",
			"SELECT group_concat(x ORDER BY x) OVER () FROM big",
			"SELECT abs(x ORDER BY x) FROM big",
			"SELECT abs(x ORDER BY x) FILTER (WHERE 1) FROM big",
			"SELECT group_concat(x ORDER BY count(*)) FROM big",
			"SELECT unknownfn(x ORDER BY x) FROM n",
			"SELECT abs(x, 1 ORDER BY x) FROM n",
			"SELECT abs(x ORDER BY x) FROM n GROUP BY x",
			// rewriteGroupExpr used to rebuild this call without its FILTER and
			// answer [1 2] where 3.53.3 refuses the statement.
			"SELECT abs(x) FILTER (WHERE x > 0) FROM n GROUP BY x",
		}},
		{"filter", []string{
			"SELECT group_concat(a ORDER BY d) FILTER (WHERE k=1) FROM g",
		}},
		{"grouped", []string{
			"SELECT k, group_concat(a ORDER BY d) FROM g GROUP BY k ORDER BY k",
			"SELECT k, group_concat(a ORDER BY d DESC, a) FROM g GROUP BY k HAVING group_concat(a ORDER BY a) > 'p' ORDER BY k",
			"SELECT k, group_concat(a ORDER BY d) AS s FROM g GROUP BY k ORDER BY group_concat(a ORDER BY a DESC)",
			"SELECT k, group_concat(a ORDER BY d), group_concat(a) FROM g GROUP BY k ORDER BY k",
		}},
		{"owner", []string{
			"SELECT (SELECT group_concat(n.i ORDER BY c.i) FROM c) FROM n ORDER BY n.i",
			"SELECT k, (SELECT group_concat(x ORDER BY g.d) FROM n WHERE n.i = g.k) FROM g ORDER BY k, d",
		}},
		{"view-cte", []string{
			"CREATE VIEW v AS SELECT k, group_concat(a ORDER BY d) AS s FROM g GROUP BY k",
			"SELECT * FROM v ORDER BY k",
			"SELECT sql FROM sqlite_schema WHERE name='v'",
			"WITH w AS (SELECT a, d FROM g WHERE k=2) SELECT group_concat(a ORDER BY d DESC) FROM w",
			"SELECT group_concat(a ORDER BY d) FROM (SELECT * FROM g ORDER BY a DESC)",
		}},
		{"dml", []string{
			"CREATE TABLE out(k, s)",
			"INSERT INTO out SELECT k, group_concat(a ORDER BY d) FROM g GROUP BY k",
			"UPDATE out SET s = (SELECT group_concat(a ORDER BY a DESC) FROM g WHERE g.k = out.k)",
			"SELECT * FROM out ORDER BY k",
			"CREATE TABLE lg(v)",
			"CREATE TRIGGER tg AFTER INSERT ON out BEGIN INSERT INTO lg SELECT group_concat(a ORDER BY d, a) FROM g WHERE k = new.k; END",
			"INSERT INTO out VALUES(1, 'x')",
			"SELECT * FROM lg",
		}},
		{"alter-rename", []string{
			"CREATE VIEW v2 AS SELECT group_concat(a ORDER BY d) AS s FROM g",
			"ALTER TABLE g RENAME COLUMN d TO dd",
			"SELECT sql FROM sqlite_schema WHERE name='v2'",
			"SELECT * FROM v2",
		}},
	}
	for _, tc := range cases {
		differ(t, "aggregate ORDER BY: "+tc.name, append(append([]string(nil), fixture...), tc.stmts...))
	}
}

// TestAggregateOrderByCursorZeroDeclines pins obKeyArgCursorZeroRisk
// (vdbe_agg_codegen.go): where 3.53.3's key-is-the-argument test compares a
// column of cursor 0 equal to ANOTHER table's column at the same position, C
// aggregates the ORDER BY key instead of the argument. That depends on C's
// cursor numbering, so the shape declines -- and the oracle half of each case
// shows it really does answer, so the decline is a real gap, not a mutual
// rejection.
func TestAggregateOrderByCursorZeroDeclines(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(x, y)", "INSERT INTO t VALUES(1,'a'),(2,'b')",
		"CREATE TABLE u(x, y)", "INSERT INTO u VALUES(10,'p'),(20,'q')",
		"CREATE TABLE n(i INTEGER PRIMARY KEY)", "INSERT INTO n VALUES(1),(2),(3)",
		"CREATE TABLE c(i INTEGER PRIMARY KEY)", "INSERT INTO c VALUES(1),(2),(3)",
	}
	for _, q := range []string{
		"SELECT group_concat(u.x ORDER BY t.x) FROM t, u", // '1,1,2,2' in 3.53.3
		"SELECT group_concat(u.x ORDER BY t.x) FROM u, t", // '10,20,10,20'
		"SELECT (SELECT group_concat(c.i ORDER BY n.i) FROM c) FROM n",
	} {
		stmts := append(append([]string(nil), setup...), q)
		c := run(t, "cgo", stmts)
		m := run(t, "musql", stmts)
		if last := c[len(c)-1]; last["kind"] == "error" {
			t.Errorf("fixture assumption wrong: the oracle refuses %q: %v", q, last)
		}
		last := m[len(m)-1]
		if last["kind"] != "error" {
			t.Errorf("%q: want the cursor-0 decline, got %v", q, last)
		}
	}
	// The same shapes that CANNOT hit it stay served: one table, or different
	// column positions, or a compound key against a plain column.
	differ(t, "aggregate ORDER BY: cursor-0 neighbours", append(append([]string(nil), setup...),
		"SELECT group_concat(x ORDER BY y) FROM t",
		"SELECT group_concat(u.y ORDER BY t.x) FROM t, u",
		"SELECT group_concat(u.x ORDER BY t.x + 0) FROM t, u",
		"SELECT group_concat(u.x ORDER BY t.x COLLATE NOCASE) FROM t, u",
		"SELECT group_concat(rowid ORDER BY x) FROM t",
	))
}
