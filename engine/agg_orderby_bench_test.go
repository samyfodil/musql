package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// Benchmarks for aggregate and ORDER BY operations, run engine-direct.
func buildAggOrderBenchDB(b *testing.B) *ReadOnlyPager {
	b.Helper()
	path := filepath.Join(b.TempDir(), "aggorderbench.sqlite")
	db, err := Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`); err != nil {
		b.Fatal(err)
	}
	const n = 100_000
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
	p, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { p.Close() })
	return p
}

// BenchmarkAggGroupBy is workload 5: SELECT k, count(*), sum(v) FROM t GROUP BY k
func BenchmarkAggGroupBy(b *testing.B) {
	p := buildAggOrderBenchDB(b)
	const sql = `SELECT k, count(*), sum(v) FROM t GROUP BY k`
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, rows, err := p.QueryArgs(sql, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 10 {
			b.Fatalf("got %d groups, want 10", len(rows))
		}
	}
}

// BenchmarkOrderByLimit20 is workload 6: SELECT id, v FROM t ORDER BY v DESC LIMIT 20
func BenchmarkOrderByLimit20(b *testing.B) {
	p := buildAggOrderBenchDB(b)
	const sql = `SELECT id, v FROM t ORDER BY v DESC LIMIT 20`
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, rows, err := p.QueryArgs(sql, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 20 {
			b.Fatalf("got %d rows, want 20", len(rows))
		}
	}
}

// BenchmarkOrderByLimit20Ascending is the same query over the ADVERSARIAL
// ordering for the bounded sorter -- an ascending v, so every row displaces the
// heap's worst and OpSorterCheck can never skip a record. It is here so a claim
// about workload 6 cannot hide a regression in the case the optimization does
// not help.
func BenchmarkOrderByLimit20Ascending(b *testing.B) {
	p := buildEquiJoinBenchDB(b) // v = (i*13) % 1_000_000: ascending over 100k rows
	const sql = `SELECT id, v FROM t ORDER BY v DESC LIMIT 20`
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, rows, err := p.QueryArgs(sql, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 20 {
			b.Fatalf("got %d rows, want 20", len(rows))
		}
	}
}

// BenchmarkScanCountFilter is workload 4 (SELECT count(*) FROM t WHERE v > ?),
// and BenchmarkScanSumFilter is its lowered sibling: count(*) has no argument to
// lower, sum(v) does, and both run the WHOLE-TABLE aggregate path
// (compileScanAggregate's OpAggStep P3==0), the other place stampAggArgRegs
// applies.
func BenchmarkScanCountFilter(b *testing.B) {
	p := buildAggOrderBenchDB(b)
	const sql = `SELECT count(*) FROM t WHERE v > 500000`
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, _, err := p.QueryArgs(sql, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanSumFilter(b *testing.B) {
	p := buildAggOrderBenchDB(b)
	const sql = `SELECT sum(v) FROM t WHERE v > 500000`
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, _, err := p.QueryArgs(sql, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOrderByFull is the UNBOUNDED sorter (no LIMIT), the path
// OpSorterCheck is never emitted for -- a regression guard for entryBefore now
// routing through keyLess.
func BenchmarkOrderByFull(b *testing.B) {
	p := buildAggOrderBenchDB(b)
	const sql = `SELECT id, v FROM t ORDER BY v DESC`
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, _, err := p.QueryArgs(sql, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAggGroupBySorted is workload 5 forced onto the SORT-based drain by a
// trailing ORDER BY -- the path whose accumulators are never lowered, so it
// measures whether routing step through aggItem.rowValue cost anything.
func BenchmarkAggGroupBySorted(b *testing.B) {
	p := buildAggOrderBenchDB(b)
	const sql = `SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k`
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, rows, err := p.QueryArgs(sql, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 10 {
			b.Fatalf("got %d groups, want 10", len(rows))
		}
	}
}
