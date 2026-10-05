package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// BenchmarkDriverPointLookup and BenchmarkDriverEquiJoin isolate TestBenchVsC's
// workloads 1 and 3 through the SAME driver path (driver, disk-backed),
// with no C SQLite engine open alongside, so -cpuprofile attributes cleanly.
// Schema/seed mirrors seedBench exactly (can't reuse it directly: it takes
// *testing.T, not *testing.B).
func setupDriverBenchDB(b *testing.B) *sql.DB {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "musql.db")
	db, err := sql.Open(driver.DriverName, path)
	if err != nil {
		b.Fatal(err)
	}
	db.SetMaxOpenConns(1)

	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			b.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`)
	exec(`CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT)`)
	exec(`CREATE INDEX idx_t_sec ON t(sec)`)

	rng := rand.New(rand.NewSource(benchSeed))
	if err := bulkInsert(db, "INSERT INTO b(id,label) VALUES(?,?)", benchN, func(i int) []any {
		return []any{i, fmt.Sprintf("label-%d", i)}
	}); err != nil {
		b.Fatalf("seed b: %v", err)
	}
	if err := bulkInsert(db, "INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)", benchN, func(i int) []any {
		return []any{i, rng.Intn(benchN), rng.Intn(10), rng.Intn(1_000_000), 1 + rng.Intn(benchN), fmt.Sprintf("row-%d-payload", i)}
	}); err != nil {
		b.Fatalf("seed t: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

func BenchmarkDriverPointLookup(b *testing.B) {
	db := setupDriverBenchDB(b)
	rng := rand.New(rand.NewSource(benchSeed))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := runOne(db, "SELECT * FROM t WHERE id = ?", 1+rng.Intn(benchN)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDriverEquiJoin(b *testing.B) {
	db := setupDriverBenchDB(b)
	rng := rand.New(rand.NewSource(benchSeed))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := runOne(db, "SELECT t.id, t.v, b.label FROM t JOIN b ON b.id = t.bid WHERE t.id = ?", 1+rng.Intn(benchN)); err != nil {
			b.Fatal(err)
		}
	}
}
