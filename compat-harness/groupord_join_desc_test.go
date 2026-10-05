package compat

// GROUP BY emission order over a JOIN whose GROUP BY the nested-loop plan
// already satisfies. select.c:8545 opens no sorter when
// "sqlite3WhereIsOrdered(pWInfo)==pGroupBy->nExpr", and for WHERE_GROUPBY
// wherePathSatisfiesOrderBy ignores the sort direction (where.c:5411), so an
// outer loop walked through "CREATE INDEX i1 ON t(a DESC)" emits its groups
// DESCENDING. musql re-sorted every multi-table plan's groups ascending; this
// was r36bOpenTail's "group" family (join_using_r36b_test.go), 81 cells.

import (
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestGroupByJoinDescIndexEmissionOrder(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a,b,c)",
		"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
		"CREATE TABLE z(a,d)",
		"INSERT INTO z VALUES(1,'p'),(2,'q'),(3,'r'),(2,'s'),(NULL,'u')",
	}
	idxs := []string{
		"CREATE INDEX i1 ON t(a DESC)",
		"CREATE INDEX i1 ON t(a)",
		"CREATE INDEX i1 ON t(a DESC, b)",
		"CREATE INDEX i1 ON t(a, b DESC)",
		"CREATE INDEX i1 ON t(b DESC, a)",
		"CREATE INDEX i1 ON t(a DESC); CREATE INDEX j1 ON z(a DESC)",
		"CREATE INDEX j1 ON z(a DESC)",
	}
	queries := []string{
		"SELECT group_concat(t.b) FROM t JOIN z ON t.a=z.a GROUP BY t.a",
		"SELECT t.a, count(*), group_concat(z.d) FROM t JOIN z ON t.a=z.a GROUP BY t.a",
		"SELECT t.a, group_concat(t.b) FROM t LEFT JOIN z ON t.a=z.a GROUP BY t.a",
		"SELECT a, group_concat(t.b) FROM t JOIN z USING (a) GROUP BY a",
		"SELECT t.a, t.b, count(*) FROM t JOIN z ON t.a=z.a GROUP BY t.a, t.b",
		"SELECT t.b, t.a, count(*) FROM t JOIN z ON t.a=z.a GROUP BY t.b, t.a",
		"SELECT z.a, count(*) FROM t, z WHERE t.a=z.a GROUP BY z.a",
		"SELECT count(*), z.a FROM t, z GROUP BY z.a",
		"SELECT count(*), t.a FROM z, t GROUP BY t.a",
		"SELECT t.a, count(*) FROM t JOIN z ON t.a=z.a GROUP BY t.a ORDER BY count(*)",
		"SELECT DISTINCT t.a FROM t JOIN z ON t.a=z.a GROUP BY t.a",
	}
	for _, idx := range idxs {
		for _, q := range queries {
			stmts := append([]string{}, base...)
			for _, d := range strings.Split(idx, ";") {
				stmts = append(stmts, strings.TrimSpace(d))
			}
			stmts = append(stmts, q)
			differ(t, idx+" | "+q, stmts)
		}
	}
}

// TestGroupBySelfJoinQualifiedAmbiguous pins the six "group" cells of the r36b
// battery that were NOT an emission-order difference: over an UNALIASED
// self-join, "t.b" names two FROM items and is "ambiguous column name: t.b"
// (resolve.c:436-447, :785), GROUP BY or not. The GROUP BY planner's resolver
// took the first copy and answered.
func TestGroupBySelfJoinQualifiedAmbiguous(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a,b,c)",
		"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
	}
	for _, q := range []string{
		"SELECT t.b FROM t JOIN t USING (a) GROUP BY a",
		"SELECT t.b, t.c FROM t JOIN t USING (a) GROUP BY a",
		"SELECT t.b FROM t JOIN t USING (a) GROUP BY t.a",
		"SELECT max(t.c) FROM t JOIN t USING (a) GROUP BY a",
		"SELECT t.a, count(*) FROM t JOIN t USING (a) GROUP BY a",
		"SELECT t.b, count(*) FROM t NATURAL JOIN t GROUP BY a",
		"SELECT t.b, count(*) FROM t JOIN t AS u USING (a) GROUP BY a",
		"SELECT t.b FROM t, t GROUP BY t.a",
	} {
		differ(t, q, append(append([]string{}, base...), q))
	}
}
