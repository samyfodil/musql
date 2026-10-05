package compat

// TestR32KJoinIdxGate tests multi-table index planning against random generated
// query shapes, comparing results against the C SQLite oracle.

import (
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestR32KJoinIdxGate(t *testing.T) {
	nCase := r31Env("R32K_CASES", 400)
	rng := rand.New(rand.NewSource(int64(r31Env("R32K_SEED", 32))))

	cases := make([]r31JoinCase, nCase)
	for i := range cases {
		cases[i] = r31Gen(rng, i)
	}

	byIdx := map[string]int{}
	total := 0
	var firstBad string
	for start := 0; start < nCase; start += r31Batch {
		end := start + r31Batch
		if end > nCase {
			end = nCase
		}
		var stmts []string
		qAt := make([]int, 0, r31Batch)
		for _, c := range cases[start:end] {
			stmts = append(stmts, c.setup...)
			qAt = append(qAt, len(stmts))
			stmts = append(stmts, c.query)
		}
		cgo := run(t, "cgo", stmts)
		mush := run(t, "musql", stmts)
		for k, c := range cases[start:end] {
			i := qAt[k]
			cb, _ := json.Marshal(cgo[i])
			mb, _ := json.Marshal(mush[i])
			if string(cb) == string(mb) {
				continue
			}
			total++
			byIdx[c.idx]++
			if firstBad == "" {
				firstBad = strings.Join(c.setup, "; ") + "\n      " + c.query +
					"\n      cgo:    " + string(cb) + "\n      musql: " + string(mb)
			}
		}
	}
	if total == 0 {
		t.Logf("R32K join-index gate: %d/%d shapes agree with the oracle", nCase, nCase)
		return
	}
	keys := make([]string, 0, len(byIdx))
	for k := range byIdx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("R32K diverge %-10s %d", k, byIdx[k])
	}
	t.Errorf("%d/%d indexed-join shapes diverge from the oracle; first:\n      %s",
		total, nCase, firstBad)
}
