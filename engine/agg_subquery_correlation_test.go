package engine_test

import (
	"fmt"
	"strings"
	"testing"
)

// TestAggSubqueryCorrelation: a subquery inside an aggregate query's HAVING or
// aggregate argument makes that query correlated only when it names something
// outside itself. TPC-DS q23's "x IN (SELECT ... GROUP BY ... HAVING sum(...) >
// (SELECT max(...) FROM cte))" was marked correlated and re-ran its whole body
// for every probe (#376).
func TestAggSubqueryCorrelation(t *testing.T) {
	p := explainDB(t)
	for _, tc := range []struct {
		sql        string
		correlated bool
		want       string
	}{
		{`SELECT a FROM t WHERE b IN (SELECT x FROM u GROUP BY x HAVING count(*) >= (SELECT count(*) FROM u))`, false, "[1]"},
		{`SELECT a FROM t WHERE b IN (SELECT x FROM u, t t2 WHERE x = t2.b GROUP BY x HAVING count(*) >= (SELECT count(*) FROM u))`, false, "[1]"},
		{`WITH m AS (SELECT max(y) my FROM u) SELECT a FROM t WHERE b IN (SELECT x FROM u GROUP BY x HAVING sum(y) >= (SELECT * FROM m))`, false, "[1]"},
		{`SELECT a FROM t WHERE b IN (SELECT x FROM u GROUP BY x HAVING count(*) > (SELECT t.a - 1))`, true, "[1]"},
		{`SELECT (SELECT sum(x+(SELECT a)) FROM u) FROM t`, true, "[3 4]"},
	} {
		plan := strings.Join(explainPlanRows(t, p, "EXPLAIN QUERY PLAN "+tc.sql), " ")
		if got := strings.Contains(plan, "CORRELATED"); got != tc.correlated {
			t.Errorf("%s: correlated %v, want %v: %s", tc.sql, got, tc.correlated, plan)
		}
		_, rows, err := p.Query(tc.sql)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		var got []int64
		for _, r := range rows {
			got = append(got, r[0].I)
		}
		if fmt.Sprint(got) != tc.want {
			t.Errorf("%s: got %v, want %s", tc.sql, got, tc.want)
		}
	}
}
