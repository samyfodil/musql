package compat

// Shape-space enumeration: systematic cross-product of schema, FROM, WHERE, read, and tail axes.
// Every cell is compared against the oracle. Divergences are reported by cell coordinates.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r36Axis is one independent dimension of the shape space. Cells are named so a
// divergence report reads as coordinates.
type r36Axis struct {
	name  string
	cells []r36Cell
}

type r36Cell struct {
	name string
	// sql is the fragment this cell contributes; its meaning depends on the axis.
	sql string
}

// Schema axis: index shapes and declared collations change plans without changing statements.
var r36SchemaAxis = r36Axis{"schema", []r36Cell{
	{"noidx", ``},
	{"idx-a", `CREATE INDEX i1 ON t(a);`},
	{"idx-a-desc", `CREATE INDEX i1 ON t(a DESC);`},
	{"idx-ab", `CREATE INDEX i1 ON t(a,b);`},
	{"idx-ba", `CREATE INDEX i1 ON t(b,a);`},
	{"idx-a-nocase", `CREATE INDEX i1 ON t(a COLLATE NOCASE);`},
	{"idx-cover", `CREATE INDEX i1 ON t(a,b,c);`},
	{"idx-uniq-b", `CREATE UNIQUE INDEX i1 ON t(b);`},
	{"two-idx", `CREATE INDEX i1 ON t(a); CREATE INDEX i2 ON t(b);`},
}}

// FROM axis: self-joins and derived tables expose duplicate names and loop-order effects.
var r36FromAxis = r36Axis{"from", []r36Cell{
	{"plain", `t`},
	{"alias", `t AS x`},
	{"self-join", `t, t AS u`},
	{"left-join", `t LEFT JOIN t AS u ON u.a = t.a`},
	{"right-join", `t RIGHT JOIN t AS u ON u.a = t.a`},
	{"full-join", `t FULL JOIN t AS u ON u.a = t.a`},
	{"using-join", `t JOIN t AS u USING (a)`},
	{"natural-join", `t NATURAL JOIN t AS u`},
	{"cross-join", `t CROSS JOIN t AS u ON u.a = t.a`},
	{"derived", `(SELECT * FROM t) AS d`},
	{"derived-limit", `(SELECT * FROM t LIMIT 3) AS d`},
	{"cte", `c`}, // prefixed by WITH below
}}

// WHERE axis: term kinds reach different optimization paths.
var r36WhereAxis = r36Axis{"where", []r36Cell{
	{"none", ``},
	{"eq", `WHERE a = 2`},
	{"range", `WHERE a >= 2`},
	{"two-sided", `WHERE a > 1 AND a < 4`},
	{"between", `WHERE a BETWEEN 2 AND 4`},
	{"not-between", `WHERE a NOT BETWEEN 2 AND 4`},
	{"in", `WHERE a IN (1,2,3)`},
	{"isnull", `WHERE a IS NULL`},
	{"notnull", `WHERE a IS NOT NULL`},
	{"like", `WHERE b LIKE 'x%'`},
	{"or", `WHERE a = 1 OR a = 3`},
	{"in-select", `WHERE a IN (SELECT a FROM t WHERE a > 1)`},
	{"exists", `WHERE EXISTS (SELECT 1 FROM t AS e WHERE e.a = t.a)`},
	{"scalar-sub", `WHERE a = (SELECT max(a) FROM t)`},
	{"collate", `WHERE b = 'x' COLLATE NOCASE`},
	{"cast", `WHERE CAST(a AS TEXT) > '1'`},
}}

// READ axis: the same plan reported through different select lists is a different observation.
var r36ReadAxis = r36Axis{"read", []r36Cell{
	{"star", `*`},
	{"bare", `a, b`},
	{"typeof", `a, typeof(a)`},
	{"quote", `quote(a), quote(b)`},
	{"expr", `a+0, b||''`},
	{"rowid", `rowid, a`},
	{"count", `count(*)`},
	{"groupconcat", `group_concat(b)`},
	{"minmax", `min(a), max(a)`},
	{"sum", `sum(a), avg(a), total(a)`},
	{"bare-agg", `a, count(*)`},
	{"bare-magnet", `a, b, max(a)`},
	{"scalar-sub", `a, (SELECT max(a) FROM t AS s)`},
	{"corr-sub", `a, (SELECT count(*) FROM t AS s WHERE s.a = t.a)`},
	{"exists-sub", `a, EXISTS (SELECT 1 FROM t AS s WHERE s.a > t.a)`},
	{"case", `CASE WHEN a IS NULL THEN 'n' ELSE 'v' END, a`},
	{"collate-read", `a COLLATE NOCASE, b COLLATE NOCASE`},
	{"concat-agg", `group_concat(b, '-'), count(DISTINCT a)`},
}}

// TAIL axis: grouping, ordering, distinctness and limit decide observable plan order.
var r36TailAxis = r36Axis{"tail", []r36Cell{
	{"none", ``},
	{"limit", `LIMIT 2`},
	{"order-a", `ORDER BY a`},
	{"order-a-desc", `ORDER BY a DESC`},
	{"order-limit", `ORDER BY a LIMIT 2`},
	{"group-a", `GROUP BY a`},
	{"group-a-having", `GROUP BY a HAVING count(*) >= 1`},
	{"group-limit", `GROUP BY a LIMIT 2`},
	{"order-tie-desc", `ORDER BY length(b) DESC`},
	{"order-two-key", `ORDER BY b, a`},
	{"order-collate", `ORDER BY b COLLATE NOCASE`},
	{"order-nulls-last", `ORDER BY a NULLS LAST`},
	{"order-expr", `ORDER BY a+0 DESC`},
	{"offset", `ORDER BY a LIMIT 2 OFFSET 1`},
	{"distinct", ``}, // handled as a SELECT-modifier below
}}

// r36Fixture: rowid order, ascending-a order and descending-a order are three different permutations.
const r36Fixture = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
`

// r36Coord is a divergence's coordinates in the space.
type r36Coord struct{ schema, from, where, read, tail string }

func (c r36Coord) String() string {
	return fmt.Sprintf("schema=%s from=%s where=%s read=%s tail=%s",
		c.schema, c.from, c.where, c.read, c.tail)
}

// r36Build assembles one cell's statement list. It returns ok=false for well-formed SQL that isn't a parity question.
func r36Build(co r36Coord, sch, from, where, read, tail string) ([]string, bool) {
	// A bare/expr readout under GROUP BY is legal in SQLite but queries the anchor row.
	// A rowid readout through a derived table or CTE has no rowid.
	if strings.Contains(read, "rowid") && (co.from == "derived" || co.from == "cte") {
		return nil, false
	}
	// A self-join with an unqualified select list is ambiguous except for `*`.
	if co.from == "self-join" && read != "*" && !strings.HasPrefix(read, "count") {
		return nil, false
	}
	sel := "SELECT "
	if co.tail == "distinct" {
		sel = "SELECT DISTINCT "
		tail = ""
	}
	stmt := sel + read + " FROM " + from
	if where != "" {
		stmt += " " + where
	}
	if tail != "" {
		stmt += " " + tail
	}
	if co.from == "cte" {
		stmt = "WITH c AS (SELECT * FROM t) " + stmt
	}
	stmts := []string{}
	for _, s := range strings.Split(r36Fixture+sch, ";") {
		if s = strings.TrimSpace(s); s != "" {
			stmts = append(stmts, s)
		}
	}
	return append(stmts, stmt), true
}

// TestR36ShapeSpace walks the cross-product and reports divergences by cell coordinates.
// A wrong answer is the invariant to catch; declines are counted separately.
func TestR36ShapeSpace(t *testing.T) {
	if testing.Short() {
		t.Skip("shape-space enumeration: full run")
	}
	all := [][]r36Cell{
		r36SchemaAxis.cells, r36FromAxis.cells, r36WhereAxis.cells,
		r36ReadAxis.cells, r36TailAxis.cells,
	}
	names := []string{"schema", "from", "where", "read", "tail"}

	// Three modes: full cross-product, restricted axes, or pairwise covering array (default).
	var combos [][]int
	switch {
	case os.Getenv("R36_FULL") != "":
		combos = r36FullProduct(all)
	case os.Getenv("R36_AXES") != "":
		pin := map[string]bool{}
		for _, a := range strings.Split(os.Getenv("R36_AXES"), ",") {
			pin[strings.TrimSpace(a)] = true
		}
		sel := make([][]r36Cell, len(all))
		for i := range all {
			if pin[names[i]] {
				sel[i] = all[i]
			} else {
				sel[i] = all[i][:1]
			}
		}
		combos = r36FullProduct(sel)
	default:
		combos = r36Pairwise(r36Sizes(all))
	}

	var cells, agreed, declined, mutual int
	wrongBy := map[string][]string{} // axis-cell -> example coords (CO-OCCURRENCE)
	var wrongCoords []string
	var wrongIx [][]int // the same cells as index vectors, for attribution

	for _, ix := range combos {
		func() {
			sc, fr := all[0][ix[0]], all[1][ix[1]]
			wh, rd, tl := all[2][ix[2]], all[3][ix[3]], all[4][ix[4]]
			{
				{
					{
						co := r36Coord{sc.name, fr.name, wh.name, rd.name, tl.name}
						stmts, ok := r36Build(co, sc.sql, fr.sql, wh.sql, rd.sql, tl.sql)
						if !ok {
							return
						}
						cells++
						m := run(t, "musql", stmts)
						cg := run(t, "cgo", stmts)
						last := len(stmts) - 1
						mErr := m[last]["kind"] == "error"
						cErr := cg[last]["kind"] == "error"
						switch {
						case mErr && cErr:
							mutual++
						case mErr:
							declined++
						case cErr:
							// The oracle rejects what we serve: that IS a wrong
							// answer, and the direction the corpus under-reports.
							wrongCoords = append(wrongCoords, co.String()+" [we serve, oracle rejects]")
							wrongIx = append(wrongIx, append([]int(nil), ix...))
							for _, k := range []string{"schema=" + co.schema, "from=" + co.from,
								"where=" + co.where, "read=" + co.read, "tail=" + co.tail} {
								wrongBy[k] = append(wrongBy[k], co.String())
							}
						default:
							mb, _ := json.Marshal(m[last])
							cb, _ := json.Marshal(cg[last])
							if r36ResultsMatch(stmts[last], m[last], cg[last]) {
								agreed++
							} else {
								// The coordinates name the RULE; the statement
								// and the two answers are what a fix needs, and
								// reconstructing them by hand from five axis
								// cells is where a reader gets the shape subtly
								// wrong and chases a cell that already agrees.
								t.Logf("R36 DIFF %s\n    sql: %s\n    cgo: %s\n    mus: %s",
									co, stmts[last], cb, mb)
								wrongCoords = append(wrongCoords, co.String())
								wrongIx = append(wrongIx, append([]int(nil), ix...))
								for _, k := range []string{"schema=" + co.schema, "from=" + co.from,
									"where=" + co.where, "read=" + co.read, "tail=" + co.tail} {
									wrongBy[k] = append(wrongBy[k], co.String())
								}
							}
						}
					}
				}
			}
		}()
	}

	t.Logf("R36 SHAPE SPACE: cells=%d agreed=%d declined=%d mutualReject=%d WRONG=%d",
		cells, agreed, declined, mutual, len(wrongCoords))

	// Attribution: for every wrong cell, reset axes to baseline and re-run to identify the cause.
	if len(wrongCoords) > 0 && os.Getenv("R36_NO_ATTRIBUTION") == "" {
		blame := map[string]int{}
		for _, ix := range wrongIx {
			for ax := 0; ax < len(all); ax++ {
				if ix[ax] == 0 {
					continue // already the baseline on this axis
				}
				alt := append([]int(nil), ix...)
				alt[ax] = 0
				sc, fr := all[0][alt[0]], all[1][alt[1]]
				wh, rd, tl := all[2][alt[2]], all[3][alt[3]], all[4][alt[4]]
				co := r36Coord{sc.name, fr.name, wh.name, rd.name, tl.name}
				stmts, ok := r36Build(co, sc.sql, fr.sql, wh.sql, rd.sql, tl.sql)
				if !ok {
					continue
				}
				m := run(t, "musql", stmts)
				cg := run(t, "cgo", stmts)
				last := len(stmts) - 1
				if m[last]["kind"] == "error" || cg[last]["kind"] == "error" {
					continue
				}
				mb, _ := json.Marshal(m[last])
				cb, _ := json.Marshal(cg[last])
				if string(mb) == string(cb) {
					// Resetting THIS axis fixed it, so this axis is causal.
					blame[names[ax]+"="+all[ax][ix[ax]].name]++
				}
			}
		}
		if len(blame) > 0 {
			bk := make([]string, 0, len(blame))
			for k := range blame {
				bk = append(bk, k)
			}
			sort.Slice(bk, func(i, j int) bool { return blame[bk[i]] > blame[bk[j]] })
			t.Logf("R36 CAUSAL ATTRIBUTION (resetting this axis makes the cell agree):")
			for _, k := range bk {
				t.Logf("  %-24s implicated in %d wrong cells", k, blame[k])
			}
		}
	}

	if len(wrongCoords) > 0 {
		// Report by axis cell: these are co-occurrence counts, not causes.
		keys := make([]string, 0, len(wrongBy))
		for k := range wrongBy {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return len(wrongBy[keys[i]]) > len(wrongBy[keys[j]]) })
		for _, k := range keys[:min(12, len(keys))] {
			t.Logf("  %-24s %d wrong", k, len(wrongBy[k]))
		}
		for _, c := range wrongCoords[:min(20, len(wrongCoords))] {
			t.Errorf("R36 WRONG ANSWER: %s", c)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// r36FullProduct walks every combination — the overnight mode.
func r36FullProduct(axes [][]r36Cell) [][]int {
	out := [][]int{{}}
	for _, ax := range axes {
		next := make([][]int, 0, len(out)*len(ax))
		for _, pre := range out {
			for i := range ax {
				next = append(next, append(append([]int(nil), pre...), i))
			}
		}
		out = next
	}
	return out
}

// r36Sizes reduces an axis list to the only thing the covering array needs.
func r36Sizes(axes [][]r36Cell) []int {
	out := make([]int, len(axes))
	for i := range axes {
		out[i] = len(axes[i])
	}
	return out
}

// r36Pairwise builds a covering array: every (axis_i cell, axis_j cell) pair for
// every i<j appears in at least one row. Greedy — repeatedly take the candidate
// row covering the most still-uncovered pairs — which is not minimal but is
// within a small factor and needs no solver. Deterministic: no randomness, so a
// failing cell reproduces exactly.
//
// It takes CELL COUNTS, not cells: nothing about a cell's SQL matters to the
// combinatorics, so the DML shape space (dmlspace_r38d_test.go) reuses this
// rather than carrying a second copy of the greedy loop.
func r36Pairwise(sizes []int) [][]int {
	type pair struct{ ai, av, bi, bv int }
	need := map[pair]bool{}
	for i := 0; i < len(sizes); i++ {
		for j := i + 1; j < len(sizes); j++ {
			for a := 0; a < sizes[i]; a++ {
				for b := 0; b < sizes[j]; b++ {
					need[pair{i, a, j, b}] = true
				}
			}
		}
	}
	var rows [][]int
	for len(need) > 0 {
		best := make([]int, len(sizes))
		bestN := -1
		// Seed from the most-constrained uncovered pair, then fill greedily.
		for cand := 0; cand < 64; cand++ {
			row := make([]int, len(sizes))
			for i := range sizes {
				bestCell, bestCover := 0, -1
				for c := 0; c < sizes[i]; c++ {
					cover := 0
					for j := 0; j < i; j++ {
						if need[pair{j, row[j], i, c}] {
							cover++
						}
					}
					for j := i + 1; j < len(sizes); j++ {
						for d := 0; d < sizes[j]; d++ {
							if need[pair{i, c, j, d}] {
								cover++
								break
							}
						}
					}
					if cover > bestCover || (cover == bestCover && (c+cand)%sizes[i] == 0) {
						bestCell, bestCover = c, cover
					}
				}
				row[i] = bestCell
			}
			n := 0
			for i := 0; i < len(sizes); i++ {
				for j := i + 1; j < len(sizes); j++ {
					if need[pair{i, row[i], j, row[j]}] {
						n++
					}
				}
			}
			if n > bestN {
				best, bestN = row, n
			}
		}
		if bestN <= 0 {
			// Nothing greedy covers anything new: emit one row per remaining
			// pair so the array is genuinely covering rather than approximately.
			for p := range need {
				row := make([]int, len(sizes))
				row[p.ai], row[p.bi] = p.av, p.bv
				rows = append(rows, row)
				delete(need, p)
			}
			break
		}
		for i := 0; i < len(sizes); i++ {
			for j := i + 1; j < len(sizes); j++ {
				delete(need, pair{i, best[i], j, best[j]})
			}
		}
		rows = append(rows, best)
	}
	return rows
}

// r36ResultsMatch compares cell answers: column names, row multiset, and order only where ORDER BY pins it.
func r36ResultsMatch(stmt string, mus, cgo map[string]any) bool {
	if fmt.Sprint(mus["kind"]) != fmt.Sprint(cgo["kind"]) {
		return false
	}
	mc, _ := json.Marshal(mus["cols"])
	cc, _ := json.Marshal(cgo["cols"])
	if string(mc) != string(cc) {
		return false
	}
	mRows, cRows := r36RowStrings(mus), r36RowStrings(cgo)
	if len(mRows) != len(cRows) {
		return false
	}
	ms := append([]string(nil), mRows...)
	cs := append([]string(nil), cRows...)
	sort.Strings(ms)
	sort.Strings(cs)
	for i := range ms {
		if ms[i] != cs[i] {
			return false
		}
	}
	if !tclHasTopLevelOrderBy(stmt) {
		return true
	}
	cols := r36ColNames(cgo)
	idxs, ok := tclOrderByKeyIndices(stmt, cols)
	if !ok {
		return true // an expression key: nothing here can say what it ordered by
	}
	mCells, cCells := r36RowCells(mus), r36RowCells(cgo)
	for r := range mCells {
		for _, k := range idxs {
			if k >= len(mCells[r]) || k >= len(cCells[r]) {
				return false
			}
			if mCells[r][k] != cCells[r][k] {
				return false
			}
		}
	}
	return true
}

// r36RowCells is one result's rows as string cells; r36RowStrings is the same
// rows rendered one string per row, for the multiset compare.
func r36RowCells(res map[string]any) [][]string {
	raw, _ := res["rows"].([]any)
	out := make([][]string, 0, len(raw))
	for _, r := range raw {
		cells, _ := r.([]any)
		row := make([]string, len(cells))
		for i, c := range cells {
			row[i] = fmt.Sprint(c)
		}
		out = append(out, row)
	}
	return out
}

func r36RowStrings(res map[string]any) []string {
	rows := r36RowCells(res)
	out := make([]string, len(rows))
	for i, r := range rows {
		b, _ := json.Marshal(r)
		out[i] = string(b)
	}
	return out
}

func r36ColNames(res map[string]any) []string {
	raw, _ := res["cols"].([]any)
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		out = append(out, fmt.Sprint(c))
	}
	return out
}

// TestR36MatchRejectsARealOrderBug guards the ORDER BY comparison: pinned order must still fail when reversed.
func TestR36MatchRejectsARealOrderBug(t *testing.T) {
	cols := []any{"a", "b"}
	mk := func(rows ...[]any) map[string]any {
		rr := make([]any, len(rows))
		for i, r := range rows {
			rr[i] = r
		}
		return map[string]any{"kind": "rows", "cols": cols, "rows": rr}
	}
	asc := mk([]any{"I:1", "x"}, []any{"I:2", "y"})
	desc := mk([]any{"I:2", "y"}, []any{"I:1", "x"})
	if r36ResultsMatch("SELECT a, b FROM t ORDER BY a", asc, desc) {
		t.Error("a reversed ORDER BY a key sequence must NOT compare equal")
	}
	if !r36ResultsMatch("SELECT a, b FROM t", asc, desc) {
		t.Error("with no ORDER BY the same two rows are the same multiset")
	}
	if !r36ResultsMatch("SELECT a, b FROM t ORDER BY a", asc, asc) {
		t.Error("identical results must compare equal")
	}
	if r36ResultsMatch("SELECT a, b FROM t", asc, mk([]any{"I:1", "x"}, []any{"I:3", "y"})) {
		t.Error("a different VALUE must not compare equal, ORDER BY or not")
	}
	if r36ResultsMatch("SELECT a, b FROM t", asc, mk([]any{"I:1", "x"})) {
		t.Error("a different ROW COUNT must not compare equal")
	}
}
