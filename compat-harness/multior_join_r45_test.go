package compat

// Tests multi-table WHERE_MULTI_OR in join queries.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

const r45Fixture = `
CREATE TABLE t1(a, b, c);
CREATE TABLE t2(a, b, c);
CREATE TABLE t3(a, b, c);
INSERT INTO t1 VALUES(1,1,5),(2,2,3),(1,3,1),(3,1,4),(2,1,2),(NULL,2,6);
INSERT INTO t2 VALUES(2,1,7),(1,2,2),(3,1,1),(1,3,2),(2,2,5),(1,1,9),(3,3,2),(2,1,1),(NULL,1,3);
INSERT INTO t3 VALUES(1,1,1),(2,2,2),(1,2,3),(3,1,1),(2,1,2);
`

var r45Schemas = []struct{ name, ddl string }{
	{"noidx", ``},
	{"idx", `CREATE INDEX t2b ON t2(b); CREATE INDEX t2c ON t2(c); CREATE INDEX t1b ON t1(b); CREATE INDEX t3b ON t3(b); CREATE INDEX t3c ON t3(c);`},
	{"idx-a", `CREATE INDEX t2b ON t2(b); CREATE INDEX t2c ON t2(c); CREATE INDEX t1b ON t1(b); CREATE INDEX t3b ON t3(b); CREATE INDEX t3c ON t3(c);` +
		`CREATE INDEX t1a ON t1(a); CREATE INDEX t2a ON t2(a); CREATE INDEX t3a ON t3(a);`},
	{"idx-a-analyze", `CREATE INDEX t2b ON t2(b); CREATE INDEX t2c ON t2(c); CREATE INDEX t1b ON t1(b); CREATE INDEX t3b ON t3(b); CREATE INDEX t3c ON t3(c);` +
		`CREATE INDEX t1a ON t1(a); CREATE INDEX t2a ON t2(a); CREATE INDEX t3a ON t3(a); ANALYZE;`},
	{"composite", `CREATE INDEX t2bc ON t2(b, c); CREATE INDEX t2ca ON t2(c, a); CREATE INDEX t3bc ON t3(b, c); CREATE INDEX t1c ON t1(c);`},
}

// r45Two are the two-table WHERE shapes, crossed with every two-table FROM.
var r45Two = []string{
	`t1.a=t2.a AND (t2.b=1 OR t2.c=2)`,
	`(t2.b=1 OR t2.c=2)`,
	`(t2.b=1 OR t2.c=2) AND t1.b=2`,
	`(t1.b=1 OR t2.c=2)`,
	`(t2.b=1 OR t2.c>=t1.c)`,
	`(t1.c<=t2.c) OR (t2.b=1)`,
	`(t2.rowid<=3) OR (t1.a<=t2.a)`,
	`((t2.b=1 AND t2.c>1) OR t2.c=2) AND t1.a=t2.a`,
	`(t2.b=1 OR t2.c=2 OR t2.a=3) AND t1.b=t2.b`,
	`(t1.a=1 OR t1.b=2) AND (t2.b=1 OR t2.c=2)`,
	`(t2.a IS NULL OR t2.c=2)`,
	`(t2.b=1 OR t2.c IS NULL)`,
	`(t2.c=2 OR t2.b=t1.b)`,
	// Two commuted copies: pOrWc appends them in REVERSE disjunct order.
	`(t1.c<=t2.c OR t1.b=t2.a OR t2.b=3)`,
	`(t2.b=t1.b OR t2.c=t1.c)`,
	// "x IS NOT NULL" is a WO_AND entry with a "x>NULL" virtual child.
	`(t2.b=1 OR t2.c IS NOT NULL) AND t1.a=t2.a`,
	`(t2.b BETWEEN 1 AND 2 OR t2.c=2)`,
	`(t2.b IN (1,3) OR t2.c=2) AND t1.b=2`,
	`(t2.a<t2.b OR t2.c=2) AND t1.a=t2.a`,
	// A case-1 OR (an IN) is WO_OR too, and so part of pAndExpr.
	`(t2.b=1 OR t2.c=2) AND (t2.a=1 OR t2.a=3)`,
}

// r45TwoOpen is case 1 over a COLUMN right-hand side: C rewrites it into
// "t2.c IN (t1.a, t1.b)" (whereexpr.c:809-900), which wherePlanOrToIn does not
// build, so the port declines -- never a MULTI_OR guess. The ORDER BY-with-ties
// readout of a declined join still comes out in this engine's own loop order,
// which is the pre-existing gap these cells measure (20 wrong at 030f87c9 and
// after): reported, not failed.
var r45TwoOpen = []string{
	`(t1.a=t2.c OR t1.b=t2.c)`,
	`(t2.c=t1.a OR t2.c=t1.b)`,
}

var r45TwoFroms = []string{
	`t1, t2`,
	`t2, t1`,
	`t1 JOIN t2 ON t1.a=t2.a`,
	`t1 LEFT JOIN t2 ON t1.a=t2.a`,
}

// r45OnOr are FROM clauses whose ON carries the OR, crossed with a trivial
// WHERE and a filtering one.
var r45OnOr = []string{
	`t1 LEFT JOIN t2 ON t1.a=t2.a AND (t2.b=1 OR t2.c=2)`,
	`t1 LEFT JOIN t2 ON (t2.b=1 OR t2.c=2)`,
	`t1 JOIN t2 ON (t2.b=1 OR t2.c=2)`,
	`t1 JOIN t2 ON t1.a=t2.a AND (t2.b=1 OR t2.c=t1.c)`,
	`t1 LEFT JOIN t2 ON (t2.b=t1.b OR t2.c=t1.c)`,
	`t1 LEFT JOIN t2 ON t2.b=t1.b AND (t2.a=1 OR t2.c=2)`,
}

var r45Three = []string{
	`t1.a=t2.a AND t2.a=t3.a AND (t3.b=1 OR t3.c=2)`,
	`(t2.b=1 OR t2.c=2) AND t3.b=t2.b`,
	`(t2.b=1 OR t2.c=t3.c) AND t1.a=t2.a`,
	`(t3.b=1 OR t3.a=t1.a) AND t2.c=t3.c`,
	`(t2.b=1 OR t2.c=2) AND (t3.b=2 OR t3.c=1)`,
	`(t2.b=1 OR t2.c=2) AND t2.a=t3.a AND t3.b=t1.b`,
	`(t2.b=t1.b OR t2.c=t3.c) AND t3.a=1`,
}

type r45Query struct {
	shape string
	sql   string
	open  bool
}

func r45Queries() []r45Query {
	var out []r45Query
	two := func(from, where string) {
		w := ""
		if where != "" {
			w = " WHERE " + where
		}
		out = append(out,
			r45Query{shape: "gc|" + from + "|" + where, sql: "SELECT group_concat(t1.rowid||'.'||t2.rowid, ' ') FROM " + from + w},
			r45Query{shape: "limit|" + from + "|" + where, sql: "SELECT t1.rowid, t2.rowid FROM " + from + w + " LIMIT 7"},
			r45Query{shape: "ties|" + from + "|" + where, sql: "SELECT t1.rowid, t2.rowid FROM " + from + w + " ORDER BY t1.a LIMIT 9"},
		)
	}
	for _, f := range r45TwoFroms {
		for _, w := range r45Two {
			two(f, w)
		}
	}
	for _, f := range r45TwoFroms {
		n := len(out)
		for _, w := range r45TwoOpen {
			two(f, w)
		}
		for i := n; i < len(out); i++ {
			out[i].open = true
		}
	}
	for _, f := range r45OnOr {
		two(f, "")
		two(f, "t1.b<3")
		two(f, "t2.c IS NULL OR t2.b=2")
	}
	for _, w := range r45Three {
		out = append(out,
			r45Query{shape: "gc3|" + w, sql: "SELECT group_concat(t1.rowid||'.'||t2.rowid||'.'||t3.rowid, ' ') FROM t1, t2, t3 WHERE " + w},
			r45Query{shape: "limit3|" + w, sql: "SELECT t1.rowid, t2.rowid, t3.rowid FROM t1, t2, t3 WHERE " + w + " LIMIT 7"},
			r45Query{shape: "gc3rev|" + w, sql: "SELECT group_concat(t1.rowid||'.'||t2.rowid||'.'||t3.rowid, ' ') FROM t3, t2, t1 WHERE " + w},
		)
	}
	return out
}

func r45Split(sql string) []string {
	var out []string
	for _, s := range strings.Split(sql, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func TestMultiOrJoinGrid(t *testing.T) {
	queries := r45Queries()
	var total, agreed, declined, mutual, wrong, openWrong, multi, multiAgreed int
	by := map[string]int{}
	var examples []string
	for _, sc := range r45Schemas {
		for _, pragma := range []string{"", "PRAGMA reverse_unordered_selects=1;"} {
			pre := r45Split(r45Fixture + sc.ddl + pragma)
			stmts := append([]string(nil), pre...)
			for _, q := range queries {
				stmts = append(stmts, q.sql)
			}
			m := run(t, "musql", stmts)
			cg := run(t, "cgo", stmts)
			// Which cells C answers through a MULTI-INDEX OR at all, so the
			// tally says how much of the grid exercises the join realization.
			eqp := append([]string(nil), pre...)
			for _, q := range queries {
				eqp = append(eqp, "EXPLAIN QUERY PLAN "+q.sql)
			}
			plans := run(t, "cgo", eqp)
			for i := range queries {
				pb, _ := json.Marshal(plans[len(pre)+i])
				if strings.Contains(string(pb), "MULTI-INDEX OR") {
					multi++
					if mb, _ := json.Marshal(m[len(pre)+i]); m[len(pre)+i]["kind"] != "error" {
						cb, _ := json.Marshal(cg[len(pre)+i])
						if string(mb) == string(cb) {
							multiAgreed++
						}
					}
				}
			}
			for i, q := range queries {
				mr, cr := m[len(pre)+i], cg[len(pre)+i]
				mErr, cErr := mr["kind"] == "error", cr["kind"] == "error"
				total++
				switch {
				case mErr && cErr:
					mutual++
				case mErr:
					declined++
					by["DECLINED "+strings.SplitN(q.shape, "|", 2)[1]]++
				default:
					mb, _ := json.Marshal(mr)
					cb, _ := json.Marshal(cr)
					if !cErr && string(mb) == string(cb) {
						agreed++
						continue
					}
					if q.open {
						openWrong++
						by["OPEN-WRONG "+strings.SplitN(q.shape, "|", 2)[1]]++
						continue
					}
					wrong++
					by["WRONG "+strings.SplitN(q.shape, "|", 2)[1]]++
					if len(examples) < 20 {
						examples = append(examples, fmt.Sprintf("[%s%s] %s\n    cgo:    %s\n    musql: %s",
							sc.name, strings.TrimSuffix(" "+pragma, " "), q.sql, cb, mb))
					}
				}
			}
		}
	}
	t.Logf("r45 multi-table MULTI_OR grid: total=%d agreed=%d declined=%d mutual-error=%d WRONG=%d known-open-wrong=%d; C plans a MULTI-INDEX OR in %d (%d of them agreed)",
		total, agreed, declined, mutual, wrong, openWrong, multi, multiAgreed)
	var keys []string
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return by[keys[i]] > by[keys[j]] || by[keys[i]] == by[keys[j]] && keys[i] < keys[j]
	})
	for i, k := range keys {
		if i >= 60 {
			break
		}
		t.Logf("  %4d  %s", by[k], k)
	}
	for _, e := range examples {
		t.Log(e)
	}
	if wrong > 0 {
		t.Errorf("%d wrong answers in the multi-table MULTI_OR grid", wrong)
	}
}

// TestMultiOrJoinRowid15 is rowid.test's rowid-15.1, which C plans as SCAN t1
// then a MULTI-INDEX OR over t2 whose second sub-scan is the COMMUTED copy of
// "t1.c0 <= t2.c0" (EXPLAIN QUERY PLAN: INDEX 1 "rowid<?", INDEX 3 "c0>?").
func TestMultiOrJoinRowid15(t *testing.T) {
	base := []string{
		"PRAGMA reverse_unordered_selects=true",
		"CREATE TABLE t1 (c0, c1)",
		"CREATE TABLE t2 (c0 INT UNIQUE)",
		"INSERT INTO t1(c0, c1) VALUES (0, 0), (0, NULL)",
		"INSERT INTO t2(c0) VALUES (1)",
	}
	q := "SELECT t2.c0, t1.c1 FROM t1, t2 WHERE (t2.rowid <= 'a') OR (t1.c0 <= t2.c0) LIMIT 100"
	differ(t, "rowid-15.1", append(append([]string{}, base...), q))
	// The same plan over enough rows for the per-outer-row group to matter.
	more := append(append([]string{}, base...),
		"INSERT INTO t1(c0, c1) VALUES (2, 'a'), (-1, 'b'), (5, 'c')",
		"INSERT INTO t2(c0) VALUES (3), (-2), (0), (4)")
	differ(t, "rowid-15.1 wide", append(more, q))
	differ(t, "rowid-15.1 wide gc", append(more,
		"SELECT group_concat(t2.c0||':'||t1.c1, ' ') FROM t1, t2 WHERE (t2.rowid <= 'a') OR (t1.c0 <= t2.c0)"))
	differ(t, "rowid-15.1 wide swapped", append(more,
		"SELECT group_concat(t2.c0||':'||t1.c1, ' ') FROM t1, t2 WHERE (t1.c0 <= t2.c0) OR (t2.rowid <= 'a')"))
}
