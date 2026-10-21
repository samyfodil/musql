package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// buildEquiJoinBenchDB mirrors compat-harness/bench_vs_c_test.go's workload 3
// schema+data exactly (same column names/types, same row counts) so this
// engine-level benchmark isolates the VDBE/pager cost from the driver's
// per-statement open+parse overhead that TestBenchVsC's numbers also include.
func buildEquiJoinBenchDB(b *testing.B) *ReadOnlyPager { return buildEquiJoinBenchDBVacuumed(b, false) }

// buildEquiJoinBenchDBVacuumed is buildEquiJoinBenchDB, with every row laid
// into segments by VACUUM when vacuum is set, as the compat harness's benchmark
// lays them.
func buildEquiJoinBenchDBVacuumed(b *testing.B, vacuum bool) *ReadOnlyPager {
	b.Helper()
	path := filepath.Join(b.TempDir(), "equijoinbench.sqlite")
	db, err := Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
	} {
		if err := db.Exec(s); err != nil {
			b.Fatal(err)
		}
	}
	const n = 100_000
	for i := 1; i <= n; i++ {
		if _, _, err := db.ExecArgs(`INSERT INTO b(id,label) VALUES(?,?)`,
			[]Value{{Typ: Int, I: int64(i)}, {Typ: Text, S: []byte(fmt.Sprintf("label-%d", i))}}); err != nil {
			b.Fatal(err)
		}
	}
	for i := 1; i <= n; i++ {
		bid := 1 + (i*7919)%n // deterministic pseudo-random spread, avoids a PRNG dependency
		if _, _, err := db.ExecArgs(`INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)`,
			[]Value{
				{Typ: Int, I: int64(i)},
				{Typ: Int, I: int64((i * 3) % n)},
				{Typ: Int, I: int64(i % 10)},
				{Typ: Int, I: int64((i * 13) % 1_000_000)},
				{Typ: Int, I: int64(bid)},
				{Typ: Text, S: []byte(fmt.Sprintf("row-%d-payload", i))},
			}); err != nil {
			b.Fatal(err)
		}
	}
	if vacuum {
		if err := db.Exec(`VACUUM`); err != nil {
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

// BenchmarkEquiJoinPointLookup matches TestBenchVsC's workload 3 exactly:
// SELECT t.id, t.v, b.label FROM t JOIN b ON b.id = t.bid WHERE t.id = ?
func BenchmarkEquiJoinPointLookup(b *testing.B) {
	p := buildEquiJoinBenchDB(b)
	const sql = `SELECT t.id, t.v, b.label FROM t JOIN b ON b.id = t.bid WHERE t.id = ?`
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		id := int64(1 + (n % 100_000))
		_, rows, err := p.QueryArgs(sql, []Value{{Typ: Int, I: id}})
		if err != nil {
			b.Fatal(err)
		}
		_ = rows
	}
}

// BenchmarkRowidPointLookupAlone isolates just "SELECT * FROM t WHERE id = ?"
// on the same DB, for comparison against the join above.
func BenchmarkRowidPointLookupAlone(b *testing.B) {
	p := buildEquiJoinBenchDB(b)
	const sql = `SELECT * FROM t WHERE id = ?`
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		id := int64(1 + (n % 100_000))
		_, rows, err := p.QueryArgs(sql, []Value{{Typ: Int, I: id}})
		if err != nil {
			b.Fatal(err)
		}
		_ = rows
	}
}

// BenchmarkIndexedEquiJoinCount is TestBenchColumnarVsC's "indexed equi-join":
// an index seek on t.sec (about one row) joined to b by rowid, counted.
func BenchmarkIndexedEquiJoinCount(b *testing.B) {
	p := buildEquiJoinBenchDBVacuumed(b, true)
	const sql = `SELECT count(*) FROM t JOIN b ON t.bid = b.id WHERE t.sec = ?`
	n := 0
	for b.Loop() {
		n++
		if _, _, err := p.QueryArgs(sql, []Value{{Typ: Int, I: int64(n % 100_000)}}); err != nil {
			b.Fatal(err)
		}
	}
}
