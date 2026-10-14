package compat

// A walk of an index whose key holds expressions visits rows in the order of
// the computed key values, as the b-tree stores them. The order a scan through
// such an index delivers is observable with no ORDER BY, and as the tie order
// under one that does not order every column.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var exprIndexWalkSchema = []string{
	"CREATE TABLE l(x INT, k INT)",
	"CREATE TABLE r(a INT, b INT, c TEXT)",
	"CREATE TABLE s(a INT, d INT)",
	"CREATE INDEX r_all ON r(a, coalesce(b,7), b IS NULL, ifnull(c,'none'), CASE WHEN b IS NULL THEN 'nb' ELSE 'b' END, b + 1, length(c))",
	"CREATE INDEX r_expr_first ON r(coalesce(b,7), a)",
	"CREATE INDEX s_expr ON s(a, coalesce(d,-1))",
	"INSERT INTO l VALUES (1, 1), (2, 1), (3, 2), (4, 2), (NULL, 3)",
	"INSERT INTO r VALUES (1, 10, 'x'), (1, NULL, NULL), (2, NULL, 'yy'), (5, 50, 'z'), (NULL, 7, 'n')",
	"INSERT INTO s VALUES (1, NULL), (2, 20), (4, NULL)",
}

func TestExprIndexWalkOrder(t *testing.T) {
	for _, q := range []string{
		"SELECT rowid, a, b FROM r INDEXED BY r_expr_first",
		"SELECT rowid, a, b FROM r INDEXED BY r_all",
		"SELECT rowid, a, b FROM r INDEXED BY r_all WHERE a = 1",
		"SELECT rowid, coalesce(b,7), a FROM r INDEXED BY r_expr_first",
		"SELECT l.x, r.rowid FROM l JOIN r INDEXED BY r_all ON r.a = l.x",
		"SELECT l.x, coalesce(r.b,7) FROM l LEFT JOIN r INDEXED BY r_all ON r.a = l.x ORDER BY 1",
		"SELECT l.x, coalesce(s.d,-1), coalesce(r.b,7) FROM l LEFT JOIN s ON s.a = l.x LEFT JOIN r INDEXED BY r_all ON r.a = s.a",
		"SELECT l.x, coalesce(s.d,-1), coalesce(r.b,7) FROM l LEFT JOIN s ON s.a = l.x LEFT JOIN r INDEXED BY r_all ON r.a = s.a ORDER BY 1, 2",
	} {
		t.Run(q, func(t *testing.T) {
			differ(t, q, append(append([]string{}, exprIndexWalkSchema...), q))
		})
	}
}
