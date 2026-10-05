package compat

// This file measures OR-term hole behavior with and without indexes.
// It compares row order when the WHERE clause uses OR with rowid ranges.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
)

func TestR38AOrDrill(t *testing.T) {
	if testing.Short() || os.Getenv("R38A_OR") == "" {
		t.Skip("set R38A_OR=1 to run the measurement")
	}
	type cellKey struct{ schema, qual, where, agg string }
	var mu sync.Mutex
	tally := map[string]int{}
	var lines []string

	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, sch := range r38aSchemas {
		for _, ql := range r38aQuals {
			if ql.name == "indexed-by" {
				continue
			}
			for _, wh := range r38aWheres {
				for _, ag := range r38aAggs {
					k := cellKey{sch.name, ql.name, wh.name, ag.name}
					stmts := r38aStmts(sch.sql, ag.sql, ql.sql, wh.sql, "")
					wg.Add(1)
					sem <- struct{}{}
					go func(k cellKey, stmts []string) {
						defer wg.Done()
						defer func() { <-sem }()
						m := run(t, "musql", stmts)
						cg := run(t, "cgo", stmts)
						last := len(stmts) - 1
						mErr := m[last]["kind"] == "error"
						cErr := cg[last]["kind"] == "error"
						kind := "agreed"
						switch {
						case mErr && cErr:
							kind = "mutual"
						case mErr:
							kind = "declined"
						case cErr:
							kind = "serve-oracle-rejects"
						default:
							mb, _ := json.Marshal(m[last])
							cb, _ := json.Marshal(cg[last])
							if string(mb) != string(cb) {
								kind = "WRONG"
							}
						}
						mu.Lock()
						tally[kind+" qual="+k.qual+" where="+k.where]++
						if kind == "WRONG" || kind == "serve-oracle-rejects" {
							lines = append(lines, fmt.Sprintf("%s schema=%s qual=%s where=%s agg=%s",
								kind, k.schema, k.qual, k.where, k.agg))
						}
						mu.Unlock()
					}(k, stmts)
				}
			}
		}
	}
	wg.Wait()

	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("R38A-OR %-46s %d\n", k, tally[k])
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Printf("R38A-OR CELL %s\n", l)
	}
}
