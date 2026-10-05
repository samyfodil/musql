package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// buildJoinBenchDB builds a small outer table (200 rows) and a large inner
// table (50k rows) with a secondary index on the join column and an INTEGER
// PRIMARY KEY, so a two-table equi-join's inner side is a heavy full scan
// without the seek (200 * 50k = 10M inner-row touches) and an index/rowid seek
// with it.
func buildJoinBenchDB(b *testing.B) *ReadOnlyPager {
	b.Helper()
	path := filepath.Join(b.TempDir(), "joinbench.sqlite")
	db, err := Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE o (id INTEGER PRIMARY KEY, k INTEGER)`,
		`CREATE TABLE i (id INTEGER PRIMARY KEY, fk INTEGER, payload TEXT)`,
		`CREATE INDEX ifk ON i(fk)`,
	} {
		if err := db.Exec(s); err != nil {
			b.Fatal(err)
		}
	}
	// Selective join: 50k inner rows over 5000 distinct fk buckets (~10 inner
	// rows per join key), 200 outer rows -- the canonical "large indexed inner,
	// few matches per outer row" shape a nested-loop full scan handles worst.
	const outerN, innerN, buckets = 200, 50000, 5000
	for id := 1; id <= outerN; id++ {
		if _, _, err := db.ExecArgs(`INSERT INTO o(id,k) VALUES(?,?)`,
			[]Value{{Typ: Int, I: int64(id)}, {Typ: Int, I: int64(id)}}); err != nil {
			b.Fatal(err)
		}
	}
	for id := 1; id <= innerN; id++ {
		if _, _, err := db.ExecArgs(`INSERT INTO i(id,fk,payload) VALUES(?,?,?)`,
			[]Value{{Typ: Int, I: int64(id)}, {Typ: Int, I: int64(id % buckets)},
				{Typ: Text, S: []byte(fmt.Sprintf("payload-%d", id))}}); err != nil {
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

// benchJoin runs sql on the VDBE with the join-seek optimization forced on or
// off (joinSeekDisabled), so the two sub-benchmarks isolate exactly the seek's
// effect on the identical bytecode engine.
func benchJoin(b *testing.B, p *ReadOnlyPager, sql string, seekOn bool) {
	savedDisable := joinSeekDisabled
	joinSeekDisabled = !seekOn
	defer func() { joinSeekDisabled = savedDisable }()

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, rows, err := p.QueryArgs(sql, nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = rows
	}
}

// BenchmarkJoinIndexSeek measures a two-table equi-join on an indexed inner
// column, before (FullScan) and after (Seek) the index-driven inner seek.
func BenchmarkJoinIndexSeek(b *testing.B) {
	p := buildJoinBenchDB(b)
	const sql = `SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k`
	b.Run("FullScan", func(b *testing.B) { benchJoin(b, p, sql, false) })
	b.Run("Seek", func(b *testing.B) { benchJoin(b, p, sql, true) })
}

// BenchmarkJoinRowidSeek measures a two-table equi-join on the inner rowid
// (INTEGER PRIMARY KEY), before and after the rowid point-lookup seek.
func BenchmarkJoinRowidSeek(b *testing.B) {
	p := buildJoinBenchDB(b)
	const sql = `SELECT o.id, i.id FROM o JOIN i ON i.id = o.k`
	b.Run("FullScan", func(b *testing.B) { benchJoin(b, p, sql, false) })
	b.Run("Seek", func(b *testing.B) { benchJoin(b, p, sql, true) })
}
