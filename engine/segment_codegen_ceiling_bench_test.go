package engine

import (
	"fmt"
	"testing"
)

// Measure code generator value: compare kernels, fused loops, and ideal ceiling.
func BenchmarkCodegenCeiling(b *testing.B) {
	const n = 100_000
	seed := buildBulkUpdateBenchFile(b, n)
	segs := segmentsOf(b, seed, "t")
	for _, s := range segs {
		for _, c := range []int{2, 3} {
			if kind, why := s.scanKind(c); kind != segScanSlice {
				b.Fatalf("column %d is not a fixed-width block (%s); every arm here assumes it is", c, why)
			}
		}
	}
	// v > 500000 AND k <> 7
	preds := []segPred{
		{Col: 3, Op: segGT, Val: Value{Typ: Int, I: 500_000}},
		{Col: 2, Op: segNE, Val: Value{Typ: Int, I: 7}},
	}

	b.Run("kernels", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for _, s := range segs {
				count += segFilterCount(s, preds)
			}
			if count == 0 {
				b.Fatal("no rows")
			}
		}
	})

	b.Run("fused", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for _, s := range segs {
				v, _ := s.Int64Column(3)
				k, _ := s.Int64Column(2)
				for j := range v {
					if v[j] > 500_000 && k[j] != 7 {
						count++
					}
				}
			}
			if count == 0 {
				b.Fatal("no rows")
			}
		}
	})

	// What a code generator would ACTUALLY emit for two predicates: no
	// short-circuit. "&&" is a BRANCH, and the arm above pays for it -- which
	// is why its single-predicate cousin, where Go emits a conditional
	// increment and no branch at all, is an order of magnitude quicker. A
	// generator has no reason to branch here and every reason not to.
	b.Run("fused_branchless", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for _, s := range segs {
				v, _ := s.Int64Column(3)
				k, _ := s.Int64Column(2)
				for j := range v {
					hit := 1
					if v[j] <= 500_000 {
						hit = 0
					}
					if k[j] == 7 {
						hit = 0
					}
					count += hit
				}
			}
			if count == 0 {
				b.Fatal("no rows")
			}
		}
	})

	b.Run("ideal", func(b *testing.B) {
		type pair struct{ v, k []int64 }
		cols := make([]pair, 0, len(segs))
		for _, s := range segs {
			v, _ := s.Int64Column(3)
			k, _ := s.Int64Column(2)
			cols = append(cols, pair{v, k})
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for _, c := range cols {
				v, k := c.v, c.k
				for j := range v {
					if v[j] > 500_000 && k[j] != 7 {
						count++
					}
				}
			}
			if count == 0 {
				b.Fatal("no rows")
			}
		}
	})
}

// The same question across SELECTIVITY, which is the axis that decides it.
//
// A fused loop's cost depends on whether its branch predicts; branch-free
// kernels cost the same whatever the data does. So a single measurement
// answers nothing -- at 45% selectivity, which is where the benchmark above
// happens to sit, the branch is a coin flip and kernels win by construction.
// The format spike found the two crossing over on synthetic data; this asks the
// same of the real format.
func BenchmarkCodegenCeilingBySelectivity(b *testing.B) {
	const n = 100_000
	seed := buildBulkUpdateBenchFile(b, n)
	segs := segmentsOf(b, seed, "t")
	// v is uniform over [0,1e6), so the bound sets selectivity directly.
	for _, tc := range []struct {
		pct   int
		bound int64
	}{{1, 990_000}, {25, 750_000}, {50, 500_000}, {75, 250_000}, {99, 10_000}} {
		preds := []segPred{{Col: 3, Op: segGT, Val: Value{Typ: Int, I: tc.bound}}}
		b.Run(fmt.Sprintf("sel=%d%%/kernels", tc.pct), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				count := 0
				for _, s := range segs {
					count += segFilterCount(s, preds)
				}
				_ = count
			}
		})
		b.Run(fmt.Sprintf("sel=%d%%/fused", tc.pct), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				count := 0
				for _, s := range segs {
					v, _ := s.Int64Column(3)
					for j := range v {
						if v[j] > tc.bound {
							count++
						}
					}
				}
				_ = count
			}
		})
	}
}

// segmentsOf folds path's every row into segments (VACUUM) and returns table's,
// mapped from the file -- the blocks a query over it reads.
func segmentsOf(b *testing.B, path, table string) []*segment {
	b.Helper()
	execDB(b, path, `VACUUM`)
	f, err := OpenSegmentFile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { f.Close() })
	for _, t := range f.Tables() {
		if t.Name == table {
			segs, serr := t.openSegments()
			if serr != nil {
				b.Fatal(serr)
			}
			return segs
		}
	}
	b.Fatalf("table %s is not in %s", table, path)
	return nil
}
