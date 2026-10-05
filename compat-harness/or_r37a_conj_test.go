package compat

// Tests OR clauses combined with other WHERE conjuncts.

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

var r37aConjWhereAxis = []r36Cell{
	{"conj-before", `WHERE c<=40 AND (a<2 OR a>2)`},
	{"conj-after", `WHERE (a=1 OR b='x') AND c<=40`},
	{"two-or", `WHERE (a<2 OR a>2) AND (b='x' OR b='Y')`},
	{"conj-eq", `WHERE b='x' AND (a<2 OR a>2)`},
}

func TestR37aOrConjDrill(t *testing.T) {
	if testing.Short() {
		t.Skip("r37a OR-conjunct drill: full run")
	}
	type cell struct{ schema, read, where, tail string }
	var cells []cell
	var queries []string
	for _, rd := range r36ReadAxis.cells {
		for _, wh := range r37aConjWhereAxis {
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
				by["DECLINED-where="+c.where]++
			default:
				mb, _ := json.Marshal(mr)
				cb, _ := json.Marshal(cr)
				if !cErr && string(mb) == string(cb) {
					agreed++
					continue
				}
				wrong++
				for _, k := range []string{"where=" + c.where, "schema=" + c.schema,
					"read=" + c.read, "tail=" + c.tail} {
					by[k]++
				}
				if len(examples) < 20 {
					examples = append(examples, fmt.Sprintf(
						"schema=%s where=%s read=%s tail=%s\n     q:   %s\n     cgo: %s\n     mus: %s",
						c.schema, c.where, c.read, c.tail, queries[i], cb, mb))
				}
			}
		}
	}
	t.Logf("R37A OR-CONJ DRILL: cells=%d agreed=%d declined=%d mutualReject=%d WRONG=%d",
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
		t.Logf("  %-30s %d", k, by[k])
	}
	for _, e := range examples {
		t.Logf("  EXAMPLE %s", e)
	}
}
