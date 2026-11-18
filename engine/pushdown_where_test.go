package engine

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPushDownIntoAggregateBody checks where the parent's terms land in an
// aggregate body: on its WHERE when the GROUP BY holds every column they read
// under BINARY (havingToWhere), on its HAVING otherwise, and nowhere for a CTE
// named twice or AS MATERIALIZED, or a body with a HAVING of its own. The
// answers are the harness's (TestPushDownIntoAggregate); this holds the plan.
func TestPushDownIntoAggregateBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path, `CREATE TABLE t(id INTEGER PRIMARY KEY, g INT, k INT, v INT, w TEXT COLLATE NOCASE)`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Each pushed conjunct is written as the columns it reads.
	terms := func(e Expr) string {
		var out []string
		for _, c := range splitTopLevelAnd(e) {
			var cols []string
			r41Walk(c, func(x Expr) {
				if ce, ok := x.(ColumnExpr); ok {
					cols = append(cols, ce.Name)
				}
			})
			out = append(out, strings.Join(cols, "+"))
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		sql, where, having string
		pushed             bool
	}{
		{`WITH s AS (SELECT g, k, sum(v) AS sv FROM t GROUP BY g, k) SELECT * FROM s WHERE g = 3`, "g", "", true},
		{`WITH s AS (SELECT g AS yr, k, sum(v) AS sv FROM t GROUP BY g, k) SELECT * FROM s WHERE yr = 2 AND sv > 1`, "", "", false},
		{`SELECT * FROM (SELECT g, k, sum(v) FROM t GROUP BY g) WHERE k = 1 AND g > 2`, "g", "k", true},
		{`SELECT * FROM (SELECT g, k, sum(v) FROM t GROUP BY g, k) WHERE k = 1 AND g > 2 AND g < k`, "g+k,g,k", "", true},
		{`SELECT * FROM (SELECT w, count(*) AS c FROM t GROUP BY w) WHERE w = 'a'`, "", "w", true},
		{`SELECT * FROM (SELECT max(g) AS m, k FROM t) WHERE k = 2`, "", "k", true},
		{`WITH s AS (SELECT g, sum(v) AS sv FROM t GROUP BY g) SELECT * FROM s a, s b WHERE a.g = 2`, "", "", false},
		{`WITH s AS MATERIALIZED (SELECT g, sum(v) AS sv FROM t GROUP BY g) SELECT * FROM s WHERE g = 2`, "", "", false},
		{`WITH s AS (SELECT g, sum(v) AS sv FROM t GROUP BY g), r AS (SELECT * FROM s) SELECT * FROM r, s WHERE s.g = 2`, "", "", false},
		{`SELECT * FROM (SELECT g, count(*) AS c FROM t GROUP BY g HAVING c > 3) WHERE g = 1`, "", "", false},
		{`SELECT * FROM (SELECT g + 0 AS g2, count(*) AS c FROM t GROUP BY g + 0) WHERE g2 = 1`, "", "", false},
	} {
		stmt, err := ParseSelect(c.sql)
		if err != nil {
			t.Fatal(err)
		}
		pop := p.pushCTEScopeOf(stmt)
		out := p.r41PushDown(stmt)
		pop()
		var got []string
		if out != stmt {
			for _, it := range out.From {
				if it.Subquery != nil {
					got = append(got, terms(it.Subquery.Where), terms(it.Subquery.Having))
				}
			}
		}
		want := []string{c.where, c.having}
		if !c.pushed {
			want = nil
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s\n  pushed WHERE, HAVING: %s, want %s", c.sql, fmt.Sprint(got), fmt.Sprint(want))
		}
	}
}
