package engine

import (
	"math/rand"
	"slices"
	"testing"
)

// TestAutoIndexOrderIntKeysAsValues holds autoIndexOrderCursor's flat
// integer-key sort to the Value comparison it replaces, over keys with many
// ties, negative values, a DESC column and the rowid last.
func TestAutoIndexOrderIntKeysAsValues(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	for trial := 0; trial < 200; trial++ {
		n := 2 + rng.Intn(300)
		cur := &vdbeCursor{}
		for i := 0; i < n; i++ {
			cur.rows = append(cur.rows, []Value{{Typ: Int, I: int64(rng.Intn(7) - 3)}, {Typ: Int, I: int64(rng.Intn(3))}})
			cur.rowids = append(cur.rowids, uint64(rng.Intn(50)))
		}
		key := &autoIndexKey{cols: []int{0, 1, -1}, colls: []string{"BINARY", "BINARY", "BINARY"}, desc: []bool{false, rng.Intn(2) == 0, false}}
		at := func(i, kc int) Value {
			if kc < 0 {
				return Value{Typ: Int, I: int64(cur.rowids[i])}
			}
			return cur.rows[i][kc]
		}
		perm := make([]int, n)
		for i := range perm {
			perm[i] = i
		}
		autoIndexSortValues(perm, key, at, UTF8)
		var want []uint64
		var wantRows [][]Value
		for _, p := range perm {
			want = append(want, cur.rowids[p])
			wantRows = append(wantRows, cur.rows[p])
		}
		autoIndexOrderCursor(cur, key, UTF8)
		if !slices.Equal(cur.rowids, want) {
			t.Fatalf("trial %d: rowids %v, want %v", trial, cur.rowids, want)
		}
		for i := range wantRows {
			if &cur.rows[i][0] != &wantRows[i][0] {
				t.Fatalf("trial %d: row %d is a different row", trial, i)
			}
		}
	}
}
