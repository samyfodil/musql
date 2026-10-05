package compat

// Tests OR expressions with range and multi-column conditions.
// This is a measurement test, not a gate.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// r37aWhereAxis is the pair of OR shapes under study.
var r37aWhereAxis = []r36Cell{
	{"or-range", `WHERE a<2 OR a>2`},
	{"or-two-col", `WHERE a=1 OR b='x'`},
}

// r37aQuery assembles one cell's SELECT over the plain `t` FROM cell, applying
// r36Build's own well-formedness rules for that FROM item.
func r37aQuery(read, where, tail r36Cell) (string, bool) {
	sel := "SELECT "
	tl := tail.sql
	if tail.name == "distinct" {
		sel = "SELECT DISTINCT "
		tl = ""
	}
	stmt := sel + read.sql + " FROM t " + where.sql
	if tl != "" {
		stmt += " " + tl
	}
	return stmt, true
}

func r37aSchemaStmts(sch string) []string {
	var out []string
	for _, s := range strings.Split(r37aFixture+sch, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func TestR37aOrDrill(t *testing.T) {
	if testing.Short() {
		t.Skip("r37a OR drill: full run")
	}
	type cell struct{ schema, read, where, tail string }
	var cells []cell
	var queries []string
	for _, rd := range r36ReadAxis.cells {
		for _, wh := range r37aWhereAxis {
			for _, tl := range r36TailAxis.cells {
				q, ok := r37aQuery(rd, wh, tl)
				if !ok {
					continue
				}
				queries = append(queries, q)
				cells = append(cells, cell{read: rd.name, where: wh.name, tail: tl.name})
			}
		}
	}

	var total, agreed, declined, mutual, wrong int
	by := map[string]int{}
	var examples []string
	for _, sc := range r36SchemaAxis.cells {
		pre := r37aSchemaStmts(sc.sql)
		stmts := append(append([]string(nil), pre...), queries...)
		m := run(t, "musql", stmts)
		cg := run(t, "cgo", stmts)
		for i := range queries {
			mr, cr := m[len(pre)+i], cg[len(pre)+i]
			mErr, cErr := mr["kind"] == "error", cr["kind"] == "error"
			total++
			c := cells[i]
			c.schema = sc.name
			switch {
			case mErr && cErr:
				mutual++
			case mErr:
				declined++
				by["DECLINED-read="+c.read]++
				by["DECLINED-tail="+c.tail]++
				by["DECLINED-schema="+c.schema]++
			default:
				mb, _ := json.Marshal(mr)
				cb, _ := json.Marshal(cr)
				if !cErr && string(mb) == string(cb) {
					agreed++
					continue
				}
				wrong++
				for _, k := range []string{"where=" + c.where, "schema=" + c.schema,
					"read=" + c.read, "tail=" + c.tail, "pair=" + c.where + "/" + c.schema} {
					by[k]++
				}
				if len(examples) < 25 {
					examples = append(examples, fmt.Sprintf(
						"schema=%s where=%s read=%s tail=%s\n     q:   %s\n     cgo: %s\n     mus: %s",
						c.schema, c.where, c.read, c.tail, queries[i], cb, mb))
				}
			}
		}
	}
	t.Logf("R37A OR DRILL: cells=%d agreed=%d declined=%d mutualReject=%d WRONG=%d",
		total, agreed, declined, mutual, wrong)
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if by[keys[i]] != by[keys[j]] {
			return by[keys[i]] > by[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		// The DECLINED- keys count musql errors, everything else counts wrong
		// answers; both are bucketed the same way so a fix that turns one into the
		// other is visible in one report.
		t.Logf("  %-30s %d", k, by[k])
	}
	for _, e := range examples {
		t.Logf("  EXAMPLE %s", e)
	}
}
