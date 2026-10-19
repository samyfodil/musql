package engine

import (
	"math/rand"
	"testing"
)

// TestRowidBits holds rowidBits to a map: it may answer "maybe" for a rowid
// the set lacks only once it has given up (full), and never "no" for one it
// holds, through adds and deletes that grow it either way, negative rowids
// included, and a span too wide to keep.
func TestRowidBits(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 50; trial++ {
		var b rowidBits
		set := map[int64]bool{}
		base := rng.Int63n(1<<20) - 1<<19
		spread := int64(1 << (4 + rng.Intn(12)))
		if trial%10 == 9 {
			spread = rowidBitsMax * 4 // wide enough to give up
		}
		for op := 0; op < 3000; op++ {
			r := base + rng.Int63n(spread) - spread/2
			if rng.Intn(4) == 0 {
				b.del(r)
				delete(set, r)
			} else {
				b.add(r)
				set[r] = true
			}
		}
		for r := range set {
			if !b.has(r) {
				t.Fatalf("trial %d: rowid %d is in the set, has says no", trial, r)
			}
		}
		if !b.full {
			for k := 0; k < 3000; k++ {
				r := base + rng.Int63n(spread*2) - spread
				if b.has(r) != set[r] {
					t.Fatalf("trial %d: has(%d) = %v, set says %v", trial, r, b.has(r), set[r])
				}
			}
		}
	}
	var nilBits *rowidBits
	if !nilBits.has(5) {
		t.Fatal("a nil rowidBits must answer maybe")
	}
}
