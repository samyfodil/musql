package engine

import (
	"fmt"
	"testing"
)

// BenchmarkFullScanCountFilter benchmarks full table scan with a WHERE clause
// and count, at various row counts.
func BenchmarkFullScanCountFilter(b *testing.B) {
	for _, n := range []int{25_000, 50_000, 100_000, 200_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			path := buildBulkUpdateBenchFile(b, n)
			rp, err := Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer rp.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, rows, err := rp.QueryArgs(`SELECT count(*) FROM t WHERE v > ?`,
					[]Value{{Typ: Int, I: 500_000}})
				if err != nil {
					b.Fatal(err)
				}
				if len(rows) != 1 {
					b.Fatalf("got %d rows, want 1", len(rows))
				}
			}
		})
	}
}

// BenchmarkOrderByLimit benchmarks ORDER BY with LIMIT over a table scan.
func BenchmarkOrderByLimit(b *testing.B) {
	for _, n := range []int{25_000, 50_000, 100_000, 200_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			path := buildBulkUpdateBenchFile(b, n)
			rp, err := Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer rp.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, rows, err := rp.Query(`SELECT id, v FROM t ORDER BY v DESC LIMIT 20`)
				if err != nil {
					b.Fatal(err)
				}
				if len(rows) != 20 {
					b.Fatalf("got %d rows, want 20", len(rows))
				}
			}
		})
	}
}
