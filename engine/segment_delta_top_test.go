package engine

import (
	"math/rand/v2"
	"testing"
)

// TestDeltaMaxLiveFollowsEveryChange: maxLive is the largest live rowid after
// puts, kills and a commit's records, asked for in between or not.
func TestDeltaMaxLiveFollowsEveryChange(t *testing.T) {
	st := &segDeltaState{live: map[int]map[int64][]Value{}, dead: map[int]map[int64]bool{}}
	rng := rand.New(rand.NewPCG(3, 9))
	row := []Value{{Typ: Int, I: 1}}
	for i := range 20000 {
		ti := rng.IntN(2)
		rowid := rng.Int64N(64) - 8
		switch rng.IntN(4) {
		case 0:
			st.put(ti, rowid, row)
		case 1:
			st.kill(ti, rowid)
		case 2:
			st.applyRecs([]SegDeltaRecord{{Table: ti, Rowid: rowid, Vals: row}, {Table: ti, Rowid: rowid + 1, Vals: row}})
		default:
			st.applyRecs([]SegDeltaRecord{{Table: ti, Rowid: rowid, Kill: true}})
		}
		if rng.IntN(3) > 0 {
			continue
		}
		var want int64
		found := false
		for r := range st.live[ti] {
			if !found || r > want {
				want, found = r, true
			}
		}
		if got, ok := st.maxLive(ti); ok != found || got != want {
			t.Fatalf("change %d: maxLive(%d) = %d, %v; the live rows' largest is %d, %v", i, ti, got, ok, want, found)
		}
	}
}
