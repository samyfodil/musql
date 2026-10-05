package engine

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkBulkUpdateScan benchmarks full-table UPDATE with no index on the WHERE clause.
func BenchmarkBulkUpdateScan(b *testing.B) {
	for _, n := range []int{25_000, 50_000, 100_000, 200_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			seed := buildBulkUpdateBenchFile(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				path := filepath.Join(b.TempDir(), fmt.Sprintf("u%d.sqlite", i))
				copyBenchFile(b, seed, path)
				b.StartTimer()

				db, err := OpenWrite(path)
				if err != nil {
					b.Fatal(err)
				}
				if _, _, err := db.ExecArgs(`UPDATE t SET v = v + 1 WHERE k = ?`,
					[]Value{{Typ: Int, I: 3}}); err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// buildBulkUpdateBenchFile seeds n rows with bench_vs_c's own generator (same
// seed, same column order, same three rng.Intn calls per row) and returns the
// file's path. Seeded ONCE per size and copied per iteration.
func buildBulkUpdateBenchFile(b *testing.B, n int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), fmt.Sprintf("seed%d.sqlite", n))
	db, err := Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`); err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(0x5eed)) // bench_vs_c_test.go's benchSeed
	for i := 1; i <= n; i++ {
		if _, _, err := db.ExecArgs(`INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)`,
			[]Value{
				{Typ: Int, I: int64(i)},
				{Typ: Int, I: int64(rng.Intn(n))},
				{Typ: Int, I: int64(rng.Intn(10))},
				{Typ: Int, I: int64(rng.Intn(1_000_000))},
				{Typ: Int, I: int64(1 + rng.Intn(n))},
				{Typ: Text, S: []byte(fmt.Sprintf("row-%d-payload", i))},
			}); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}

func copyBenchFile(b *testing.B, src, dst string) {
	b.Helper()
	in, err := os.Open(src)
	if err != nil {
		b.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		b.Fatal(err)
	}
	if err := out.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkBulkUpdateUniqueIndex is BenchmarkBulkUpdateScan's statement over a
// table carrying an ORDINARY UNIQUE index -- one declared with no ON CONFLICT
// clause, so updatePlan.conflictAware is false and opUpdateRow takes its
// post-write validation branch instead of the per-row probe.
//
// Separate from the plain benchmark because that branch's cost is not a
// constant factor: it re-validates the WHOLE index after EVERY updated row. The
// doubling row counts are what says so -- a 4x jump per doubling is quadratic,
// which is a different bug from a large constant.
func BenchmarkBulkUpdateUniqueIndex(b *testing.B) {
	for _, n := range []int{2_000, 4_000, 8_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			seed := buildBulkUpdateBenchFile(b, n)
			addBenchUniqueIndex(b, seed)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				path := filepath.Join(b.TempDir(), fmt.Sprintf("uq%d.sqlite", i))
				copyBenchFile(b, seed, path)
				b.StartTimer()

				db, err := OpenWrite(path)
				if err != nil {
					b.Fatal(err)
				}
				if _, _, err := db.ExecArgs(`UPDATE t SET v = v + 1 WHERE k = ?`,
					[]Value{{Typ: Int, I: 3}}); err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// addBenchUniqueIndex puts a UNIQUE index on the seeded file's sec column,
// which buildBulkUpdateBenchFile leaves unconstrained; the UPDATE does not
// touch it, so the index's CONTENT never changes and only the checking costs.
func addBenchUniqueIndex(b *testing.B, path string) {
	b.Helper()
	db, err := OpenWrite(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`UPDATE t SET sec = id`); err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX tsec ON t(sec)`); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkBulkInsert is compat-harness/bench_vs_c_test.go's workload 7a run
// ENGINE-DIRECT, at DOUBLING row counts: n INSERTs through one write session,
// which is what buildBulkUpdateBenchFile above already does and what the
// scoreboard charges 6.6x C SQLite for.
//
// Doubling counts matter here for a reason particular to this path: an INSERT
// touches one table, but the statement tail re-syncs the whole CATALOG
// (pageSyncCatalog), so the per-row cost is O(schema) even though nothing about
// the schema changed. A wide schema and a narrow one therefore answer different
// questions, and this one -- two tables, one index -- is the narrow case.
func BenchmarkBulkInsert(b *testing.B) {
	for _, n := range []int{12_500, 25_000, 50_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				buildBulkUpdateBenchFile(b, n)
			}
		})
	}
}
