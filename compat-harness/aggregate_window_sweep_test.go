package compat

import "testing"

// TestAggregateAndWindowSweep verifies aggregate and window functions over
// mixed types, including GROUP BY on collated columns.
func TestAggregateAndWindowSweep(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b,c COLLATE NOCASE)`,
		`INSERT INTO t VALUES(1,'x','A'),(2,'y','a'),(2,NULL,'B'),(NULL,'z','b'),
		 (3.5,'x','C'),(-1,x'00','c'),(9223372036854775807,'','d'),(0,'0','D')`,
		`CREATE TABLE e(a,b)`,
	}
	aggs := []string{
		"count(*)", "count(a)", "count(DISTINCT a)", "sum(a)", "total(a)", "avg(a)",
		"min(a)", "max(a)", "group_concat(b)", "group_concat(b,'-')",
		"group_concat(DISTINCT b)", "string_agg(b,'-')",
		"json_group_array(a)", "json_group_object(c,a)",
		"sum(DISTINCT a)", "avg(DISTINCT a)", "min(c)", "max(c)",
		"count(*) FILTER (WHERE a>1)", "sum(a) FILTER (WHERE a IS NOT NULL)",
		"group_concat(b) FILTER (WHERE b IS NOT NULL)",
		"sum(a*1.0)", "total(b)", "sum(b)", "avg(b)",
	}
	t.Run("aggregates", func(t *testing.T) {
		var qs []string
		for _, a := range aggs {
			qs = append(qs,
				"SELECT "+a+" FROM t",
				"SELECT "+a+" FROM e",
				"SELECT c, "+a+" FROM t GROUP BY c ORDER BY c",
				"SELECT "+a+" FROM t GROUP BY a HAVING "+a+" IS NOT NULL ORDER BY 1",
				"SELECT "+a+" OVER (ORDER BY a) FROM t ORDER BY 1",
				"SELECT "+a+" OVER (PARTITION BY c ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY 1",
			)
		}
		for _, q := range qs {
			q := q
			t.Run(q, func(t *testing.T) {
				differ(t, "aggsweep/"+q, append(append([]string{}, setup...), q))
			})
		}
	})
	t.Run("windows", func(t *testing.T) {
		wins := []string{"row_number()", "rank()", "dense_rank()", "percent_rank()",
			"cume_dist()", "ntile(3)", "lag(a)", "lead(a)", "first_value(a)",
			"last_value(a)", "nth_value(a,2)", "lag(a,2,'d')", "lead(a,0)"}
		var qs []string
		for _, w := range wins {
			qs = append(qs,
				"SELECT "+w+" OVER (ORDER BY a) FROM t ORDER BY 1",
				"SELECT "+w+" OVER (PARTITION BY c ORDER BY a) FROM t ORDER BY 1",
				"SELECT "+w+" OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM t ORDER BY 1",
				"SELECT "+w+" OVER (ORDER BY a GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY 1",
				"SELECT "+w+" OVER () FROM t ORDER BY 1",
				"SELECT "+w+" OVER (ORDER BY a DESC NULLS FIRST) FROM t ORDER BY 1",
			)
		}
		for _, q := range qs {
			q := q
			t.Run(q, func(t *testing.T) {
				differ(t, "aggsweep/"+q, append(append([]string{}, setup...), q))
			})
		}
	})
}
