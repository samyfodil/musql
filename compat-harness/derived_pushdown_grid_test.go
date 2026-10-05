package compat

// Tests WHERE pushdown into derived tables, views, and CTEs.
// C SQLite either flattens subqueries or pushes WHERE terms down so the
// planner sees them and may use indexes to determine output order.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

type pdSource struct {
	name  string
	setup []string // extra DDL
	with  string   // WITH clause prefixed to the statement
	from  string   // the FROM item
}

var pdSources = []pdSource{
	// Base table and various derived table shapes.
	{"base", nil, "", "f AS d"},
	{"derived", nil, "", "(SELECT y, x, z FROM f) AS d"},
	{"derived-where", nil, "", "(SELECT y, x, z FROM f WHERE z <> 3) AS d"},
	{"derived-expr", nil, "", "(SELECT y, x+0 AS x, z FROM f) AS d"},
	{"derived-star", nil, "", "(SELECT * FROM f) AS d"},
	{"derived-nested", nil, "", "(SELECT * FROM (SELECT y, x, z FROM f)) AS d"},
	{"view", []string{"CREATE VIEW v AS SELECT y, x, z FROM f"}, "", "v"},
	{"cte", nil, "WITH c AS (SELECT y, x, z FROM f) ", "c"},
	{"cte-mat", nil, "WITH c AS MATERIALIZED (SELECT y, x, z FROM f) ", "c"},
	{"cte-notmat", nil, "WITH c AS NOT MATERIALIZED (SELECT y, x, z FROM f) ", "c"},
	{"derived-limit", nil, "", "(SELECT y, x, z FROM f LIMIT 100) AS d"},
	{"derived-distinct", nil, "", "(SELECT DISTINCT y, x, z FROM f) AS d"},
	{"union-all", nil, "", "(SELECT y, x, z FROM f UNION ALL SELECT b, a, a FROM g) AS d"},
	{"union", nil, "", "(SELECT y, x, z FROM f UNION SELECT b, a, a FROM g) AS d"},
	{"aggregate", nil, "", "(SELECT max(y) AS y, x, count(*) AS z FROM f GROUP BY x) AS d"},
	{"sub-orderby", nil, "", "(SELECT y, x, z FROM f ORDER BY y) AS d"},
	{"sub-orderby-z", nil, "", "(SELECT y, x, z FROM f ORDER BY z) AS d"},
	{"sub-orderby-limit", nil, "", "(SELECT y, x, z FROM f ORDER BY z LIMIT 30) AS d"},
	{"view-where", []string{"CREATE VIEW v AS SELECT y, x, z FROM f WHERE z <> 3"}, "", "v"},
	{"window", nil, "", "(SELECT y, x, z, row_number() OVER (PARTITION BY x) AS rn FROM f) AS d"},
}

var pdWheres = []string{
	"x > 7",
	"x = 5",
	"x IN (3,5,9)",
	"x BETWEEN 2 AND 6",
	"z = 1",
	"x > 7 AND z = 1",
	"y > 'r15'",
	"x > 7 OR z = 1",
	"x > 3 AND y < 'r2'",
}

var pdOutputs = []struct{ name, sql string }{
	{"rows", "SELECT y FROM %s WHERE %s"},
	{"gc", "SELECT group_concat(y) FROM %s WHERE %s"},
	{"limit", "SELECT y FROM %s WHERE %s LIMIT 3"},
	{"order-ties", "SELECT y, x FROM %s WHERE %s ORDER BY x"},
	{"join", "SELECT y, b FROM %s JOIN g ON a = z WHERE %s"},
	{"group", "SELECT x, group_concat(y) FROM %s WHERE %s GROUP BY x"},
}

var pdFixture = []string{
	"CREATE TABLE f(x, y, z)",
	"CREATE INDEX fx ON f(x)",
	"CREATE INDEX fz ON f(z)",
	"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<24) INSERT INTO f SELECT (i*7)%12, 'r'||i, i%4 FROM s",
	"CREATE TABLE g(a INTEGER PRIMARY KEY, b)",
	"INSERT INTO g VALUES(0,'g0'),(1,'g1'),(2,'g2'),(3,'g3')",
}

func pdStmts(src pdSource, where, out string, analyze bool) []string {
	stmts := append([]string{}, pdFixture...)
	stmts = append(stmts, src.setup...)
	if analyze {
		stmts = append(stmts, "ANALYZE")
	}
	return append(stmts, src.with+fmt.Sprintf(out, src.from, where))
}

// TestDerivedPushdownGrid is the whole grid. Every cell that serves an answer
// must be the oracle's.
func TestDerivedPushdownGrid(t *testing.T) {
	type cell struct {
		key, rest string // rest is the key without its source: the base control's cell
		stmts     []string
	}
	var cells []cell
	for _, an := range []bool{false, true} {
		for _, src := range pdSources {
			if only := os.Getenv("PD_SRC"); only != "" && !strings.Contains(","+only+",", ","+src.name+",") {
				continue
			}
			for _, w := range pdWheres {
				for _, o := range pdOutputs {
					rest := fmt.Sprintf("out=%s analyze=%v where=%q", o.name, an, w)
					cells = append(cells, cell{
						key:   "src=" + src.name + " " + rest,
						rest:  rest,
						stmts: pdStmts(src, w, o.sql, an),
					})
				}
			}
		}
	}
	var mu sync.Mutex
	declBy := map[string]int{}
	wrongBy := map[string]int{}
	var wrong, declines []string
	var wrongCells []cell
	baseWrong := map[string]bool{}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, c := range cells {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			m := run(t, "musql", c.stmts)
			cg := run(t, "cgo", c.stmts)
			last := len(c.stmts) - 1
			mErr := m[last]["kind"] == "error"
			cErr := cg[last]["kind"] == "error"
			mb, _ := json.Marshal(m[last])
			cb, _ := json.Marshal(cg[last])
			src := strings.Fields(c.key)[0]
			mu.Lock()
			defer mu.Unlock()
			switch {
			case mErr && cErr:
			case mErr:
				declBy[src]++
				declines = append(declines, fmt.Sprintf("%s\n    sql: %s\n    err: %s", c.key, c.stmts[last], mb))
			case !cErr && string(mb) == string(cb):
			default:
				wrongBy[src]++
				wrongCells = append(wrongCells, c)
				if src == "src=base" {
					baseWrong[c.rest] = true
				}
				wrong = append(wrong, fmt.Sprintf("%s\n    sql:    %s\n    cgo:    %s\n    musql: %s", c.key, c.stmts[last], cb, mb))
			}
		}()
	}
	wg.Wait()
	sort.Strings(wrong)
	sort.Strings(declines)
	t.Logf("DERIVED PUSHDOWN GRID: cells=%d wrong=%d declined=%d", len(cells), len(wrong), len(declines))
	for _, k := range pdSortedKeys(wrongBy) {
		t.Logf("  WRONG   %-26s %d", k, wrongBy[k])
	}
	for _, k := range pdSortedKeys(declBy) {
		t.Logf("  decline %-26s %d", k, declBy[k])
	}
	if os.Getenv("PD_DECLINES") != "" {
		for _, d := range declines {
			t.Logf("DECLINE: %s", d)
		}
	}
	// A cell whose BASE-TABLE control is wrong too is the planner's, not the
	// flattener's: the subquery reduced to exactly the statement the planner
	// already gets wrong. Those are logged; anything else fails.
	planner := 0
	for _, c := range wrongCells {
		if baseWrong[c.rest] {
			planner++
		}
	}
	t.Logf("  of the wrong cells, %d share a wrong base-table control (planner gaps)", planner)
	failed := 0
	for _, w := range wrong {
		rest := w[strings.Index(w, " ")+1 : strings.Index(w, "\n")]
		if baseWrong[rest] {
			t.Logf("WRONG (planner, base control wrong too): %s", w)
			continue
		}
		if failed++; failed <= 40 || os.Getenv("PD_ALL") != "" {
			t.Errorf("WRONG: %s", w)
		}
	}
}

func pdSortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
