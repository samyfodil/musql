package driver_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	_ "github.com/samyfodil/musql/driver"
)

// The vs-C scoreboard's 7a at driver level: a 100,000-row table with a
// secondary index, then 20,000 prepared INSERTs of the six-column row in one
// transaction whose COMMIT is inside what is timed. Reported per row.
func BenchmarkBatchInsert7a(b *testing.B) { benchWrites(b, false, false, false, "") }

// ...after reads on the same connection that seeded it.
func BenchmarkBatchInsert7aRead(b *testing.B) { benchWrites(b, false, true, false, "") }

// ...and idle for a few seconds, as the scoreboard's other engine's workloads make it.
func BenchmarkBatchInsert7aIdle(b *testing.B) { benchWrites(b, false, true, true, "") }

// ...and after the database was closed, reopened and read, as the harness does.
func BenchmarkBatchInsert7aReopened(b *testing.B) { benchWrites(b, true, true, false, "") }

// The scoreboard's 7b and 7c: one UPDATE or DELETE of a tenth of the table
// (k = 3) in autocommit, after 7a's batch, with reads before and idle time
// between, as the scoreboard has them. Reported per statement.
func BenchmarkScoreboard7b(b *testing.B) {
	benchWrites(b, false, true, true, "UPDATE t SET v = v + 1 WHERE k = ?")
}

func BenchmarkScoreboard7c(b *testing.B) {
	benchWrites(b, false, true, true, "DELETE FROM t WHERE k = ?")
}

// benchWrites seeds the scoreboard's table, runs 7a's batch and, when stmt is
// set, times that statement (with k = 3) instead of the batch.
func benchWrites(b *testing.B, reopen, read, idle bool, stmt string) {
	const seed, batch = 100000, 20000
	b.ReportAllocs()
	var timed int64
	for it := 0; it < b.N; it++ {
		b.StopTimer()
		dir, err := os.MkdirTemp("", "bi7a")
		if err != nil {
			b.Fatal(err)
		}
		open := func() *sql.DB {
			db, err := sql.Open("sqlite", filepath.Join(dir, "musql.db"))
			if err != nil {
				b.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			return db
		}
		db := open()
		for _, q := range []string{
			`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`,
			`CREATE INDEX idx_t_sec ON t(sec)`,
		} {
			if _, err := db.Exec(q); err != nil {
				b.Fatal(err)
			}
		}
		rng := rand.New(rand.NewSource(0x5eed))
		load := func(n int, row func(i int) []any) {
			tx, err := db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			st, err := tx.Prepare(`INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)`)
			if err != nil {
				b.Fatal(err)
			}
			for i := 1; i <= n; i++ {
				if _, err := st.Exec(row(i)...); err != nil {
					b.Fatal(err)
				}
			}
			st.Close()
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
		}
		batchRow := func(i int) []any {
			id := seed + i
			return []any{id, id % seed, i % 10, i, 1, "w"}
		}
		load(seed, func(i int) []any {
			return []any{i, rng.Intn(seed), rng.Intn(10), rng.Intn(1000000), 1 + rng.Intn(seed), fmt.Sprintf("row-%d-payload", i)}
		})
		if reopen {
			db.Close()
			db = open()
		}
		if read {
			for _, q := range []string{
				`SELECT * FROM t WHERE id = 5`,
				`SELECT * FROM t WHERE sec = 5`,
				`SELECT count(*) FROM t WHERE v > 5000`,
				`SELECT k, count(*), sum(v) FROM t GROUP BY k`,
				`SELECT id, v FROM t ORDER BY v DESC LIMIT 20`,
			} {
				rows, err := db.Query(q)
				if err != nil {
					b.Fatal(err)
				}
				for rows.Next() {
				}
				rows.Close()
			}
		}
		if stmt != "" {
			load(batch, batchRow)
		}
		if idle {
			// What the scoreboard's interleaved engines do: time passes between
			// this engine's reads and its writes.
			runtime.GC()
			time.Sleep(3 * time.Second)
		}
		writeAllocProfile("start")
		stopCPU := startCPUProfile()
		b.StartTimer()
		pprof.Do(context.Background(), pprof.Labels("phase", "timed"), func(context.Context) {
			if stmt == "" {
				load(batch, batchRow)
				timed += batch
				return
			}
			if _, err := db.Exec(stmt, 3); err != nil {
				b.Fatal(err)
			}
			timed++
		})
		b.StopTimer()
		stopCPU()
		writeAllocProfile("end")
		db.Close()
		os.RemoveAll(dir)
	}
	unit := "ns/row"
	if stmt != "" {
		unit = "ns/stmt"
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(timed), unit)
}

// writeAllocProfile writes the heap allocation profile to $BENCH_ALLOC_PROF.<name>
// when that variable names a prefix; `go tool pprof -base start end` is then
// the allocations of the timed section alone, without the seeding.
func writeAllocProfile(name string) {
	prefix := os.Getenv("BENCH_ALLOC_PROF")
	if prefix == "" {
		return
	}
	f, err := os.Create(prefix + "." + name)
	if err != nil {
		return
	}
	defer f.Close()
	runtime.GC()
	pprof.Lookup("allocs").WriteTo(f, 0)
}

// startCPUProfile profiles the timed section to $BENCH_CPU_PROF when it is set.
func startCPUProfile() func() {
	path := os.Getenv("BENCH_CPU_PROF")
	if path == "" {
		return func() {}
	}
	f, err := os.Create(path)
	if err != nil {
		return func() {}
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		return func() {}
	}
	return func() { pprof.StopCPUProfile(); f.Close() }
}
