package compat

// Whereorin_r36a_drill_test measures WHERE/IN shape coverage across WHERE=or
// and WHERE=in cells.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// r36aWhereCells are test patterns for WHERE/IN conditions.
var r36aWhereCells = []r36Cell{
	{"or", `WHERE a = 1 OR a = 3`},
	{"or-three", `WHERE a = 1 OR a = 3 OR a = 2`},
	{"or-range", `WHERE a < 2 OR a > 2`},
	{"or-twocol", `WHERE a = 1 OR b = 'x'`},
	{"or-and", `WHERE (a = 1 OR a = 3) AND b <> 'q'`},
	{"in", `WHERE a IN (1,2,3)`},
	{"in-unsorted", `WHERE a IN (3,1,2)`},
	{"in-one", `WHERE a IN (2)`},
	{"in-dup", `WHERE a IN (3,3,1)`},
	{"not-in", `WHERE a NOT IN (3,1)`},
	{"in-b", `WHERE b IN ('x','Y')`},
}

// r36aSameMultiset reports whether two "rows" results hold the same rows in a
// different order (column names included). It is deliberately strict about the
// column header: a differing header is a naming bug, not an order one.
func r36aSameMultiset(a, b map[string]any) bool {
	ab, _ := json.Marshal(a["cols"])
	bb, _ := json.Marshal(b["cols"])
	if string(ab) != string(bb) {
		return false
	}
	key := func(m map[string]any) []string {
		rows, _ := m["rows"].([]any)
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			j, _ := json.Marshal(r)
			out = append(out, string(j))
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	if len(ka) != len(kb) {
		return false
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

func TestR36aWhereOrInDrill(t *testing.T) {
	if testing.Short() {
		t.Skip("r36a drill: full run")
	}
	if os.Getenv("R36A_DRILL") == "" {
		t.Skip("set R36A_DRILL=1 to run the measurement")
	}
	whs := r36aWhereCells
	if v := os.Getenv("R36A_WHERE"); v != "" {
		var sel []r36Cell
		for _, c := range whs {
			for _, n := range strings.Split(v, ",") {
				if c.name == strings.TrimSpace(n) {
					sel = append(sel, c)
				}
			}
		}
		whs = sel
	}
	froms := []r36Cell{{"plain", `t`}}
	if os.Getenv("R36A_FROM") != "" {
		froms = r36FromAxis.cells
	}

	type res struct {
		co   r36Coord
		kind string // "wrong" | "declined" | "mutual" | "agreed" | "serve-oracle-rejects"
	}
	var mu sync.Mutex
	var out []res

	// Six workers: the box is 20 cores shared with the other streams, and each
	// cell is two subprocesses. run() calls t.Fatalf, which from one of these
	// goroutines marks the test failed but Goexits the wrong one -- fine for a
	// measurement that is not a gate, and it only happens when a worker binary
	// itself fails to run.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, sc := range r36SchemaAxis.cells {
		for _, fr := range froms {
			for _, wh := range whs {
				for _, rd := range r36ReadAxis.cells {
					for _, tl := range r36TailAxis.cells {
						co := r36Coord{sc.name, fr.name, wh.name, rd.name, tl.name}
						stmts, ok := r36Build(co, sc.sql, fr.sql, wh.sql, rd.sql, tl.sql)
						if !ok {
							continue
						}
						wg.Add(1)
						sem <- struct{}{}
						go func(co r36Coord, stmts []string) {
							defer wg.Done()
							defer func() { <-sem }()
							m := run(t, "musql", stmts)
							cg := run(t, "cgo", stmts)
							last := len(stmts) - 1
							mErr := m[last]["kind"] == "error"
							cErr := cg[last]["kind"] == "error"
							k := "agreed"
							switch {
							case mErr && cErr:
								k = "mutual"
							case mErr:
								k = "declined"
							case cErr:
								k = "serve-oracle-rejects"
							default:
								mb, _ := json.Marshal(m[last])
								cb, _ := json.Marshal(cg[last])
								if string(mb) != string(cb) {
									// ORDER-ONLY vs CONTENT is the split that
									// decides whether one port closes a cell:
									// the access-path port can only fix the
									// order the same rows arrive in.
									k = "wrong-content"
									if r36aSameMultiset(m[last], cg[last]) {
										k = "wrong-order"
									}
								}
							}
							mu.Lock()
							out = append(out, res{co, k})
							mu.Unlock()
						}(co, stmts)
					}
				}
			}
		}
	}
	wg.Wait()

	tally := map[string]int{}
	byAxis := map[string]int{}
	var wrong []string
	for _, r := range out {
		tally[r.kind]++
		if strings.HasPrefix(r.kind, "wrong") || r.kind == "serve-oracle-rejects" {
			wrong = append(wrong, r.kind+" "+r.co.String())
			for _, k := range []string{"schema=" + r.co.schema, "from=" + r.co.from,
				"where=" + r.co.where, "read=" + r.co.read, "tail=" + r.co.tail} {
				byAxis[k]++
			}
		}
	}
	sort.Strings(wrong)
	fmt.Printf("R36A DRILL cells=%d %v\n", len(out), tally)
	keys := make([]string, 0, len(byAxis))
	for k := range byAxis {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if byAxis[keys[i]] != byAxis[keys[j]] {
			return byAxis[keys[i]] > byAxis[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		fmt.Printf("R36A AXIS %-28s %d\n", k, byAxis[k])
	}
	for _, w := range wrong {
		fmt.Printf("R36A WRONG %s\n", w)
	}
}
