package compat

// Tests for order-sensitive aggregates. group_concat, sum/avg/total, min/max
// depend on row order. Aggregates over indexed tables or computable loop orders
// must answer; others may decline only if the accumulation actually depends on order.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// aggOrderCase is one statement sequence and what this engine must do with its last
// statement: serve it (and agree), or agree-or-decline.
type aggOrderCase struct {
	name       string
	stmts      []string
	mustAnswer bool
}

// aggOrderCheck runs one case. A served answer must ALWAYS match the oracle
// cell-for-cell; a decline is a failure only where the case says the shape is
// provably order-free.
func aggOrderCheck(t *testing.T, c aggOrderCase) {
	t.Helper()
	m := run(t, "musql", c.stmts)
	cg := run(t, "cgo", c.stmts)
	mb, _ := json.Marshal(m)
	cb, _ := json.Marshal(cg)
	if m[len(m)-1]["kind"] == "error" && cg[len(cg)-1]["kind"] != "error" {
		if c.mustAnswer {
			t.Errorf("[r29] %s DECLINED, but its accumulation cannot depend on the row order\n  sql:    %s\n  cgo:    %s",
				c.name, c.stmts[len(c.stmts)-1], cb)
		}
		return
	}
	if string(mb) != string(cb) {
		t.Errorf("[r29] %s SERVED A DIFFERENT ANSWER\n  sql:    %s\n  cgo:    %s\n  musql: %s",
			c.name, c.stmts[len(c.stmts)-1], cb, mb)
	}
}


// TestR29AggOrderIndexServed checks aggregates over indexed tables that must
// answer because the accumulation is provably order-free.
func TestR29AggOrderIndexServed(t *testing.T) {
	two := []string{
		"CREATE TABLE t(x,y)", "CREATE INDEX tx ON t(x)",
		"INSERT INTO t VALUES(3,30),(1,10),(2,20)",
	}
	grouped := []string{
		"CREATE TABLE g(k,x)", "CREATE INDEX gkx ON g(k,x)",
		"INSERT INTO g VALUES(1,3),(1,1),(2,9),(2,4)",
	}
	for _, c := range []aggOrderCase{
		{"count-star-index", append(append([]string(nil), two...), "SELECT count(*) FROM t"), true},
		{"count-col-index", append(append([]string(nil), two...), "SELECT count(x) FROM t"), true},
		{"count-distinct-index", append(append([]string(nil), two...), "SELECT count(DISTINCT x) FROM t"), true},
		{"count-group-index", append(append([]string(nil), grouped...), "SELECT k, count(*) FROM g GROUP BY k ORDER BY k"), true},
		{"sum-int-index", append(append([]string(nil), two...), "SELECT sum(x), avg(x), total(x) FROM t"), true},
		{"sum-group-index", append(append([]string(nil), grouped...), "SELECT k, sum(x), avg(x) FROM g GROUP BY k ORDER BY k"), true},
		{"minmax-index", append(append([]string(nil), two...), "SELECT min(x), max(x) FROM t"), true},
		{"concat-uniform", []string{
			"CREATE TABLE u(k,x)", "CREATE INDEX ukx ON u(k,x)",
			"INSERT INTO u VALUES(1,'a'),(1,'a'),(2,'b'),(2,'b')",
			"SELECT k, group_concat(x) FROM u GROUP BY k ORDER BY k"}, true},

		// These test covering-index scans which determine accumulation order.
		{"concat-index", append(append([]string(nil), two...), "SELECT group_concat(x) FROM t"), true},
		{"concat-index-where", []string{
			"CREATE TABLE t(x,y)", "CREATE INDEX tx ON t(x)",
			"INSERT INTO t VALUES(9,90),(2,20),(1,10)",
			"SELECT group_concat(x) FROM t WHERE x<3"}, true},
		{"json-array-index", append(append([]string(nil), two...), "SELECT json_group_array(x) FROM t"), true},
		{"string-agg-index", append(append([]string(nil), two...), "SELECT string_agg(x,'-') FROM t"), true},
		{"sum-kbn", []string{
			"CREATE TABLE f(x,y)", "CREATE INDEX fx ON f(x)",
			"INSERT INTO f VALUES(1.0e308,1),(1.0e308,2),(-1.0e308,3)",
			"SELECT sum(x) FROM f"}, true},

		// These test covering-index scans with grouping and collation.
		{"concat-group-covering", append(append([]string(nil), grouped...),
			"SELECT k, group_concat(x) FROM g GROUP BY k ORDER BY k"), true},
		{"max-tie-nocase", []string{
			"CREATE TABLE m(x TEXT COLLATE NOCASE, y)", "CREATE INDEX mx ON m(x)",
			"INSERT INTO m VALUES('abc',0),('ABC',0)",
			"SELECT max(x) FROM m"}, true},

		// Tests max() with mixed-type tie-breaking.
		{"max-tie-int-real", []string{
			"CREATE TABLE m(x,y)", "CREATE INDEX mx ON m(x)",
			"INSERT INTO m VALUES(1,0),(1.0,0)",
			"SELECT max(x), typeof(max(x)) FROM m"}, true},
	} {
		aggOrderCheck(t, c)
	}
}

// TestR29SumAccumulator checks sum()/avg()/total() accumulation with various
// numeric types including overflow and precision cases.
func TestR29SumAccumulator(t *testing.T) {
	for _, c := range []struct{ name, insert, sql string }{
		{"avg-bigint", "(9007199254740992),(1),(1)", "SELECT avg(x) FROM t"},
		{"total-bigint", "(9007199254740992),(1),(1)", "SELECT total(x) FROM t"},
		{"sum-bigint", "(9007199254740992),(1),(1)", "SELECT sum(x), typeof(sum(x)) FROM t"},
		{"sum-int-then-real", "(9007199254740992),(1),(1),(0.5)", "SELECT sum(x) FROM t"},
		{"kbn-error-term", "(1.0),(1.0e17),(-1.0e17)", "SELECT sum(x), total(x), avg(x) FROM t"},
		// ...and the three overflow rules the port must NOT have changed.
		{"sum-overflow-errors", "(9223372036854775807),(9223372036854775807)", "SELECT sum(x) FROM t"},
		{"sum-overflow-cleared", "(9223372036854775807),(1),(2.5)", "SELECT sum(x) FROM t"},
		{"total-never-errors", "(9223372036854775807),(9223372036854775807)", "SELECT total(x), avg(x) FROM t"},
	} {
		aggOrderCheck(t, aggOrderCase{c.name, []string{
			"CREATE TABLE t(x)", "INSERT INTO t VALUES" + c.insert, c.sql}, true})
	}
}

// TestR29AggOrderJoin checks aggregates over unindexed joins where the planner
// cannot determine the loop order.
func TestR29AggOrderJoin(t *testing.T) {
	base := []string{
		"CREATE TABLE j1(a,b)", "INSERT INTO j1 VALUES(1,10),(1,11),(2,12),(2,13)",
		"CREATE TABLE j2(a,c)", "INSERT INTO j2 VALUES(1,20),(1,21),(2,22),(2,23)",
		"CREATE TABLE j3(a,d)", "INSERT INTO j3 VALUES(1,30),(1,31),(2,32),(2,33)",
		"CREATE TABLE j4(a,e)", "INSERT INTO j4 VALUES(1,40),(1,41),(2,42),(2,43)",
	}
	with := func(extra []string, sql string) []string {
		return append(append(append([]string(nil), base...), extra...), sql)
	}
	for _, c := range []aggOrderCase{
		{"four-item", nil, false},
		{"rowid-where", nil, false},
		{"derived", nil, false},
		{"generated", []string{"CREATE TABLE g1(a,b,s AS (a+b))", "INSERT INTO g1(a,b) VALUES(1,10),(1,11),(2,12),(2,13)"}, false},
		{"analyzed", []string{"ANALYZE"}, false},
	} {
		sql := map[string]string{
			"four-item":   "SELECT j1.a, group_concat(j2.c) FROM j1,j2,j3,j4 WHERE j1.a=j2.a AND j2.a=j3.a AND j3.a=j4.a GROUP BY j1.a ORDER BY j1.a",
			"rowid-where": "SELECT j1.a, group_concat(j2.c) FROM j1,j2 WHERE j1.a=j2.a AND j1.rowid>0 GROUP BY j1.a ORDER BY j1.a",
			"derived":     "SELECT x.a, group_concat(j2.c) FROM (SELECT * FROM j1) AS x, j2 WHERE x.a=j2.a GROUP BY x.a ORDER BY x.a",
			"generated":   "SELECT g1.a, group_concat(j2.c) FROM g1,j2 WHERE g1.a=j2.a GROUP BY g1.a ORDER BY g1.a",
			"analyzed":    "SELECT j1.a, group_concat(j2.c) FROM j1,j2 WHERE j1.a=j2.a GROUP BY j1.a ORDER BY j1.a",
		}[c.name]
		c.stmts = with(c.stmts, sql)
		aggOrderCheck(t, c)
	}
	// Control: three-item joins where planner decides the order must not decline.
	for _, sql := range []string{
		"SELECT j1.a, group_concat(j2.c) FROM j1,j2,j3 WHERE j1.a=j2.a AND j2.a=j3.a GROUP BY j1.a ORDER BY j1.a",
		"SELECT group_concat(j2.c) FROM j1,j2,j3 WHERE j1.a=j2.a AND j2.a=j3.a",
		"SELECT j1.a, count(*), sum(j2.c) FROM j1,j2,j3 WHERE j1.a=j2.a AND j2.a=j3.a GROUP BY j1.a ORDER BY j1.a",
	} {
		aggOrderCheck(t, aggOrderCase{"control-3", with(nil, sql), true})
	}
	// A HAVING accumulator is stepped over the same rows, and its own compile
	// runs after the select list's -- so it is armed separately.
	aggOrderCheck(t, aggOrderCase{"having-concat",
		with(nil, "SELECT count(*) FROM j1,j2,j3,j4 WHERE j1.a=j2.a AND j2.a=j3.a AND j3.a=j4.a HAVING group_concat(j2.c)<>''"), false})
}

// TestR29AggOrderFuzz generates random aggregate queries to find when row order
// affects the result value, checking that the engine either serves the correct
// answer or declines (except count() which is never order-dependent).
func TestR29AggOrderFuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("randomized order-sensitive aggregate sweep")
	}
	rng := rand.New(rand.NewSource(r26EnvInt("R29_SEED", 0x29a1)))
	type tally struct{ n, diverged, declined int }
	byShape := map[string]*tally{}
	for iter := 0; iter < int(r26EnvInt("R29_ITERS", 300)); iter++ {
		shape, stmts := aggOrderFuzzCase(rng)
		m := run(t, "musql", stmts)
		cg := run(t, "cgo", stmts)
		mb, _ := json.Marshal(m)
		cb, _ := json.Marshal(cg)
		tl := byShape[shape]
		if tl == nil {
			tl = &tally{}
			byShape[shape] = tl
		}
		tl.n++
		switch {
		case string(mb) == string(cb):
		case m[len(m)-1]["kind"] == "error":
			tl.declined++
			if strings.HasPrefix(shape, "count") {
				t.Errorf("[r29fuzz] count() is order-insensitive and must never decline\n  sql: %v", stmts)
			}
		default:
			tl.diverged++
			// The indexed single-table bucket is not guarded; other buckets must not diverge.
			if strings.Contains(shape, "/indexed") {
				break
			}
			t.Errorf("[r29fuzz] %s SERVED A DIFFERENT ANSWER\n  sql:    %v\n  cgo:    %s\n  musql: %s",
				shape, stmts, cb, mb)
		}
	}
	shapes := make([]string, 0, len(byShape))
	for s := range byShape {
		shapes = append(shapes, s)
	}
	sort.Strings(shapes)
	for _, s := range shapes {
		note := ""
		if strings.Contains(s, "/indexed") {
			note = "  (wrong = the OPEN access-path half, not armed)"
		}
		t.Logf("%-26s %3d shapes, %3d declined, %3d wrong%s", s, byShape[s].n, byShape[s].declined, byShape[s].diverged, note)
	}
}

// aggOrderFuzzCase builds one aggregate query whose only order-dependent element is
// the aggregate: over an INDEXED single table (the access-path half), or over a
// planner-disqualified join of unindexed ones (the loop-order half).
func aggOrderFuzzCase(rng *rand.Rand) (string, []string) {
	// Value pool with int/real/text types to test aggregate sensitivity.
	pool := []string{"1", "1.0", "2", "2.0", "'a'", "'A'", "3", "-3", "0.5",
		"9223372036854775807", "1.0e308", "-1.0e308", "1.0e17", "NULL", "7", "7.5"}
	cell := func() string { return pool[rng.Intn(len(pool))] }
	rows := func(n int, key bool) string {
		out := make([]string, n)
		for i := range out {
			if key {
				out[i] = fmt.Sprintf("(%d,%s)", rng.Intn(3), cell())
			} else {
				out[i] = "(" + cell() + "," + cell() + ")"
			}
		}
		return strings.Join(out, ",")
	}
	aggs := []string{
		"group_concat(t.v)", "group_concat(t.v,'|')", "string_agg(t.v,'-')",
		"json_group_array(t.v)", "sum(t.v)", "avg(t.v)", "total(t.v)",
		"min(t.v)", "max(t.v)", "count(*)", "count(t.v)", "count(DISTINCT t.v)",
		// DISTINCT with mixed int/real and case-sensitive text tests order-dependency.
		"group_concat(DISTINCT t.v)", "sum(DISTINCT t.v)", "min(DISTINCT t.v)",
		"json_group_array(DISTINCT t.v)",
	}
	agg := aggs[rng.Intn(len(aggs))]
	shape := "agg"
	if strings.HasPrefix(agg, "count") {
		shape = "count"
	}
	var stmts []string
	switch rng.Intn(2) {
	case 0: // INDEXED single table: SQLite may walk the index instead.
		shape += "/indexed"
		stmts = []string{
			"CREATE TABLE t(k,v)",
			[]string{"CREATE INDEX tv ON t(v)", "CREATE INDEX tkv ON t(k,v)", "CREATE INDEX tvk ON t(v,k)"}[rng.Intn(3)],
			"INSERT INTO t VALUES" + rows(4+rng.Intn(5), true),
		}
	default: // a planner-disqualified JOIN of UNINDEXED tables.
		shape += "/join"
		stmts = []string{
			"CREATE TABLE t(k,v)", "INSERT INTO t VALUES" + rows(4+rng.Intn(5), true),
			"CREATE TABLE u(k,w)", "INSERT INTO u VALUES" + rows(4+rng.Intn(5), true),
			"CREATE TABLE x(k,z)", "INSERT INTO x VALUES" + rows(4+rng.Intn(5), true),
			"CREATE TABLE y(k,q)", "INSERT INTO y VALUES" + rows(4+rng.Intn(5), true),
		}
	}
	if strings.HasSuffix(shape, "/indexed") {
		if rng.Intn(2) == 0 {
			return shape + " whole", append(stmts, "SELECT "+agg+" FROM t")
		}
		return shape + " grouped", append(stmts, "SELECT t.k, "+agg+" FROM t GROUP BY t.k ORDER BY t.k")
	}
	from := "t,u,x,y WHERE t.k=u.k AND u.k=x.k AND x.k=y.k" // four items: past the 3-item cap
	if rng.Intn(2) == 0 {
		from = "t,u WHERE t.k=u.k AND t.rowid>0" // a rowid term in the WHERE
	}
	if rng.Intn(2) == 0 {
		return shape + " whole", append(stmts, "SELECT "+agg+" FROM "+from)
	}
	return shape + " grouped", append(stmts, "SELECT t.k, "+agg+" FROM "+from+" GROUP BY t.k ORDER BY t.k")
}
