package compat

// This test exercises DISTINCT aggregates and group_concat across schemas,
// WHERE clauses, and GROUP/ORDER combinations to isolate which aggregate forms fail.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// r37dAgg is one select-list spelling under test.
var r37dAggs = []r36Cell{
	// Control aggregates without DISTINCT to catch plan-level divergences.
	{"ctl-count-star", `count(*)`},
	{"ctl-sum-a", `sum(a)`},
	{"count-dist-a", `count(DISTINCT a)`},
	{"count-dist-b", `count(DISTINCT b)`},
	{"sum-dist-a", `sum(DISTINCT a)`},
	{"avg-dist-a", `avg(DISTINCT a)`},
	{"total-dist-a", `total(DISTINCT a)`},
	{"min-dist-a", `min(DISTINCT a)`},
	{"max-dist-a", `max(DISTINCT a)`},
	{"gc-dist-b", `group_concat(DISTINCT b)`},
	{"gc-plain-b", `group_concat(b)`},
	{"gc-sep-b", `group_concat(b, '-')`},
	{"jga-dist-b", `json_group_array(DISTINCT b)`},
	{"concat-agg", `group_concat(b, '-'), count(DISTINCT a)`},
	{"count-dist-pair", `count(DISTINCT a), count(DISTINCT b)`},
}

var r37dSchemas = []r36Cell{
	{"noidx", ``},
	{"idx-a", `CREATE INDEX i1 ON t(a);`},
	{"idx-a-desc", `CREATE INDEX i1 ON t(a DESC);`},
	{"idx-ab", `CREATE INDEX i1 ON t(a,b);`},
	{"idx-ba", `CREATE INDEX i1 ON t(b,a);`},
	{"idx-a-nocase", `CREATE INDEX i1 ON t(a COLLATE NOCASE);`},
	{"idx-b-nocase", `CREATE INDEX i1 ON t(b COLLATE NOCASE);`},
	{"idx-uniq-c", `CREATE UNIQUE INDEX i1 ON t(c);`},
	{"two-idx", `CREATE INDEX i1 ON t(a); CREATE INDEX i2 ON t(b);`},
}

var r37dTails = []r36Cell{
	{"none", ``},
	{"group-a", `GROUP BY a`},
	{"group-b", `GROUP BY b`},
	{"group-a-order", `GROUP BY a ORDER BY a`},
	{"group-b-having", `GROUP BY b HAVING count(*) >= 1`},
	{"order-none", `ORDER BY 1`},
}

var r37dWheres = []r36Cell{
	{"none", ``},
	{"range", `WHERE a >= 1`},
	{"notnull", `WHERE a IS NOT NULL`},
}

// r37dFixture is the test data.
const r37dFixture = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
`

func r37dStmts(sch, agg, where, tail string) []string {
	var out []string
	for _, s := range strings.Split(r37dFixture+sch, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	q := "SELECT " + agg + " FROM t"
	if where != "" {
		q += " " + where
	}
	if tail != "" {
		q += " " + tail
	}
	return append(out, q)
}

// TestR37DDistinctAggBattery tests DISTINCT aggregates across different schemas
// and clauses, grouping failures by aggregate type to isolate which ones fail.
func TestR37DDistinctAggBattery(t *testing.T) {
	if testing.Short() {
		t.Skip("r37d distinct-aggregate battery: full run")
	}
	byAgg := map[string]int{}
	declBy := map[string]int{}
	var wrong []string
	cells := 0
	for _, sch := range r37dSchemas {
		for _, ag := range r37dAggs {
			for _, wh := range r37dWheres {
				for _, tl := range r37dTails {
					stmts := r37dStmts(sch.sql, ag.sql, wh.sql, tl.sql)
					cells++
					m := run(t, "musql", stmts)
					cg := run(t, "cgo", stmts)
					last := len(stmts) - 1
					mErr := m[last]["kind"] == "error"
					cErr := cg[last]["kind"] == "error"
					if mErr && cErr {
						continue
					}
					if mErr {
						declBy[ag.name]++
						continue
					}
					mb, _ := json.Marshal(m[last])
					cb, _ := json.Marshal(cg[last])
					if !cErr && string(mb) == string(cb) {
						continue
					}
					byAgg[ag.name]++
					wrong = append(wrong, fmt.Sprintf(
						"agg=%s schema=%s where=%s tail=%s\n    sql:    %s\n    cgo:    %s\n    musql: %s",
						ag.name, sch.name, wh.name, tl.name, stmts[last], cb, mb))
				}
			}
		}
	}
	t.Logf("R37D BATTERY: cells=%d wrong=%d", cells, len(wrong))
	keys := make([]string, 0, len(byAgg))
	for k := range byAgg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return byAgg[keys[i]] > byAgg[keys[j]] })
	for _, k := range keys {
		t.Logf("  WRONG   %-18s %d", k, byAgg[k])
	}
	dk := make([]string, 0, len(declBy))
	for k := range declBy {
		dk = append(dk, k)
	}
	sort.Strings(dk)
	for _, k := range dk {
		t.Logf("  decline %-18s %d", k, declBy[k])
	}
	lim := len(wrong)
	if os.Getenv("R37D_ALL") == "" && lim > 25 {
		lim = 25
	}
	for _, w := range wrong[:lim] {
		t.Errorf("R37D WRONG ANSWER: %s", w)
	}
}
