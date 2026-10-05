package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// The bounded top-N ORDER BY..LIMIT sorter (vdbe_sorter.go, wired by
// compileScanSorted) must be byte-identical to the full stable sort + LIMIT/
// OFFSET it replaces, ties at the LIMIT boundary included. Reference = the same
// query with NO limit (bound=0, full sort); candidate = ORDER BY .. LIMIT n
// OFFSET o (bound>0, max-heap). Low-cardinality keys force heavy ties -- exactly
// the boundary where a naive heap would diverge from a stable sort.
func TestVDBETopNMatchesFullSort(t *testing.T) {
	for _, card := range []int{1, 3, 7, 50, 500} {
		for seed := int64(0); seed < 6; seed++ {
			testVDBETopNOnce(t, 400, card, seed)
		}
	}
}

func testVDBETopNOnce(t *testing.T, nRows, keyCard int, seed int64) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("t_%d_%d.musq", keyCard, seed))
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ExecArgs("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, k2 INTEGER)", nil); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	for i := 1; i <= nRows; i++ {
		if _, _, err := db.ExecArgs(fmt.Sprintf("INSERT INTO t VALUES(%d,%d,%d)", i, rng.Intn(keyCard), rng.Intn(keyCard)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, order := range []string{"ORDER BY k", "ORDER BY k DESC", "ORDER BY k, k2", "ORDER BY k2 DESC, k"} {
		_, ref, err := p.QueryArgs("SELECT id, k, k2 FROM t "+order, nil)
		if err != nil {
			t.Fatalf("%s ref: %v", order, err)
		}
		for _, lim := range []int{1, 5, 20, 100, nRows, nRows + 10} {
			for _, off := range []int{0, 1, 13, 100} {
				q := fmt.Sprintf("SELECT id, k, k2 FROM t %s LIMIT %d OFFSET %d", order, lim, off)
				_, got, err := p.QueryArgs(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				lo := off
				if lo > len(ref) {
					lo = len(ref)
				}
				hi := lo + lim
				if hi > len(ref) {
					hi = len(ref)
				}
				want := ref[lo:hi]
				if len(got) != len(want) {
					t.Fatalf("%s (card=%d seed=%d): got %d want %d", q, keyCard, seed, len(got), len(want))
				}
				for r := range want {
					for c := range want[r] {
						if compareValues(got[r][c], want[r][c]) != 0 {
							t.Fatalf("%s (card=%d seed=%d) row %d col %d: got %v want %v", q, keyCard, seed, r, c, got[r][c], want[r][c])
						}
					}
				}
			}
		}
	}
}

func BenchmarkVDBEOrderByLimit(b *testing.B) {
	path := filepath.Join(b.TempDir(), "ob.musq")
	db, _ := Create(path)
	db.ExecArgs("CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)", nil)
	rng := rand.New(rand.NewSource(1))
	for i := 1; i <= 100000; i++ {
		db.ExecArgs(fmt.Sprintf("INSERT INTO t VALUES(%d,%d)", i, rng.Intn(1000000)), nil)
	}
	db.Close()
	p, _ := Open(path)
	defer p.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := p.QueryArgs("SELECT id, v FROM t ORDER BY v DESC LIMIT 20", nil); err != nil {
			b.Fatal(err)
		}
	}
}
