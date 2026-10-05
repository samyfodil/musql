package compat

// NOT INDEXED with aggregate guards: test widened served set.
// The `plain` and `indexed-by` columns are CONTROLS: they share every axis with
// the `not-indexed` column, so a divergence that shows up in all three belongs
// to the surrounding plan and not to this change.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

type r38aCell struct{ name, sql string }

// r38aFixture: x disagrees with rowid about which row is first, x=1 appears as
// INTEGER 1 and REAL 1.0 (compare-equal, different BYTES, so which spelling
// survives a DISTINCT dedup is decided by ARRIVAL order), y holds 'x' twice and
// 'Y'/'X' which a NOCASE index makes compare-equal, and there is a NULL in x --
// the r36/r37d fixture, kept so a cell here is comparable with those rounds'.
const r38aFixture = `
CREATE TABLE t1(x, y, z);
INSERT INTO t1 VALUES(3,'x',10);
INSERT INTO t1 VALUES(1,'Y',20);
INSERT INTO t1 VALUES(2,'x',30);
INSERT INTO t1 VALUES(2,'z',40);
INSERT INTO t1 VALUES(NULL,'w',50);
INSERT INTO t1 VALUES(1.0,'X',60);
`

var r38aQuals = []r38aCell{
	{"plain", ``},
	{"not-indexed", `NOT INDEXED`},
	{"indexed-by", `INDEXED BY i1`}, // skipped when the schema declares no i1
}

var r38aSchemas = []r38aCell{
	{"noidx", ``},
	{"idx-x", `CREATE INDEX i1 ON t1(x);`},
	{"idx-x-desc", `CREATE INDEX i1 ON t1(x DESC);`},
	{"idx-xy", `CREATE INDEX i1 ON t1(x,y);`},
	{"idx-yx", `CREATE INDEX i1 ON t1(y,x);`},
	{"idx-y-nocase", `CREATE INDEX i1 ON t1(y COLLATE NOCASE);`},
	{"idx-uniq-z", `CREATE UNIQUE INDEX i1 ON t1(z);`},
	{"two-idx", `CREATE INDEX i1 ON t1(x); CREATE INDEX i2 ON t1(y);`},
	{"idx-cover", `CREATE INDEX i1 ON t1(x,y,z);`},
}

var r38aAggs = []r38aCell{
	{"ctl-count-star", `count(*)`},
	{"gc-x", `group_concat(x)`},
	{"gc-dist-x", `group_concat(DISTINCT x)`},
	{"gc-dist-y", `group_concat(DISTINCT y)`},
	{"gc-sep-y", `group_concat(y, ':')`},
	{"sum-dist-x", `sum(DISTINCT x)`},
	{"count-dist-x", `count(DISTINCT x)`},
	{"min-dist-x", `min(DISTINCT x)`},
	{"max-dist-y", `max(DISTINCT y)`},
	{"jga-dist-y", `json_group_array(DISTINCT y)`},
}

var r38aWheres = []r38aCell{
	{"none", ``},
	{"range", `WHERE x >= 1`},
	{"or-rowid", `WHERE rowid > 3 OR rowid < 3`},
	{"or-cols", `WHERE x = 2 OR y = 'x'`},
	{"in", `WHERE x IN (1,2,3)`},
}

var r38aTails = []r38aCell{
	{"none", ``},
	{"group-x", `GROUP BY x`},
	{"group-y", `GROUP BY y`},
	{"order-1", `ORDER BY 1`},
	{"limit", `LIMIT 2`},
}

func r38aStmts(sch, agg, qual, where, tail string) []string {
	var out []string
	for _, s := range strings.Split(r38aFixture+sch, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	q := "SELECT " + agg + " FROM t1"
	if qual != "" {
		q += " " + qual
	}
	if where != "" {
		q += " " + where
	}
	if tail != "" {
		q += " " + tail
	}
	return append(out, q)
}

// TestR38ANotIndexedAggBattery reports every divergence BY INDEX QUALIFIER, so
// the question this change raises -- "does serving NOT INDEXED serve anything
// WRONG" -- is answered by one column of the tally rather than inferred. A
// decline is counted separately and is never a failure; a differing served
// answer is.
func TestR38ANotIndexedAggBattery(t *testing.T) {
	if testing.Short() {
		t.Skip("r38a NOT INDEXED battery: full run")
	}
	byQual := map[string]int{}
	declBy := map[string]int{}
	servedBy := map[string]int{}
	var wrong []string
	cells := 0
	for _, sch := range r38aSchemas {
		for _, ql := range r38aQuals {
			if ql.name == "indexed-by" && !strings.Contains(sch.sql, "i1") {
				continue
			}
			for _, ag := range r38aAggs {
				for _, wh := range r38aWheres {
					for _, tl := range r38aTails {
						stmts := r38aStmts(sch.sql, ag.sql, ql.sql, wh.sql, tl.sql)
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
							declBy[ql.name]++
							continue
						}
						servedBy[ql.name]++
						mb, _ := json.Marshal(m[last])
						cb, _ := json.Marshal(cg[last])
						if !cErr && string(mb) == string(cb) {
							continue
						}
						byQual[ql.name]++
						wrong = append(wrong, fmt.Sprintf(
							"qual=%s schema=%s agg=%s where=%s tail=%s\n    sql:    %s\n    cgo:    %s\n    musql: %s",
							ql.name, sch.name, ag.name, wh.name, tl.name, stmts[last], cb, mb))
					}
				}
			}
		}
	}
	t.Logf("R38A BATTERY: cells=%d wrong=%d", cells, len(wrong))
	quals := make([]string, 0, len(r38aQuals))
	for _, q := range r38aQuals {
		quals = append(quals, q.name)
	}
	for _, k := range quals {
		t.Logf("  %-12s served=%d decline=%d WRONG=%d", k, servedBy[k], declBy[k], byQual[k])
	}
	sort.Strings(wrong)
	lim := len(wrong)
	if os.Getenv("R38A_ALL") == "" && lim > 25 {
		lim = 25
	}
	for _, w := range wrong[:lim] {
		t.Errorf("R38A WRONG ANSWER: %s", w)
	}
}
