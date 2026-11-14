package engine

import (
	"math"
	"math/rand"
	"testing"
)

// TestHashAggSortInts: groups with INTEGER and NULL keys drain as
// keyTupleLessDesc orders them, under every mix of descending columns.
func TestHashAggSortInts(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	ints := []int64{math.MinInt64, -1 << 40, -300, -1, 0, 1, 255, 256, 1 << 40, math.MaxInt64}
	for round := range 200 {
		nk := 1 + round%3
		var desc []bool
		for k := range nk {
			desc = append(desc, (round>>k)&1 == 1)
		}
		seen := map[string]bool{}
		var buckets []*hashAggBucket
		for range 300 {
			key := make([]Value, nk)
			for k := range key {
				if r.Intn(5) > 0 {
					key[k] = Value{Typ: Int, I: ints[r.Intn(len(ints))] + int64(r.Intn(3))}
				}
			}
			if kb := string(hashAggKeyBytes(nil, key)); !seen[kb] {
				seen[kb] = true
				buckets = append(buckets, &hashAggBucket{key: key})
			}
		}
		if !hashAggSortInts(buckets, desc) {
			t.Fatal("not sorted")
		}
		for i := 1; i < len(buckets); i++ {
			if !keyTupleLessDesc(buckets[i-1].key, buckets[i].key, desc, UTF8) {
				t.Fatalf("desc %v: %v before %v", desc, buckets[i-1].key, buckets[i].key)
			}
		}
	}
}
