package replication

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/musql/driver"
)

const benchDDL = `CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b INTEGER, c REAL, d BLOB)`

// benchSource is a replicated database holding n rows of t, each its own
// insert op, after the ops that create t. They are written before sync starts:
// a lone node prunes what it alone holds.
func benchSource(tb testing.TB, n int, opts ...Option) (*Syncer, *sql.DB) {
	tb.Helper()
	ctx := context.Background()
	path := filepath.Join(tb.TempDir(), "src.musq")
	s, db, err := openSyncer(ctx, path, CRDT())
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := db.Exec(benchDDL); err != nil {
		tb.Fatal(err)
	}
	for lo := 0; lo < n; lo += 5000 {
		tx, err := db.Begin()
		if err != nil {
			tb.Fatal(err)
		}
		for i := lo; i < min(lo+5000, n); i++ {
			if _, err := tx.Exec(`INSERT INTO t(a, b, c, d) VALUES(?, ?, ?, ?)`, fmt.Sprintf("row %d", i), i, float64(i)/3, []byte{byte(i), byte(i >> 8)}); err != nil {
				tb.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			tb.Fatal(err)
		}
	}
	if len(opts) == 0 {
		tb.Cleanup(func() { db.Close() })
		return s, db
	}
	db.Close()
	if s, db, err = openSyncer(ctx, path, append([]Option{CRDT()}, opts...)...); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	return s, db
}

// benchOps are the ops of a source of n rows: the schema op first.
func benchOps(b *testing.B, n int) []Op {
	s, _ := benchSource(b, n)
	ops, err := s.store.OpsSince(context.Background(), s.site, 0, 0)
	if err != nil {
		b.Fatal(err)
	}
	if len(ops) <= n {
		b.Fatalf("%d ops, want more than %d", len(ops), n)
	}
	return ops
}

func benchDest(b *testing.B) (*Syncer, *sql.DB) {
	b.Helper()
	s, db, err := openSyncer(context.Background(), filepath.Join(b.TempDir(), "dst.musq"), CRDT())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return s, db
}

func benchCount(b *testing.B, db *sql.DB, want int) {
	b.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != want {
		b.Fatalf("count(*) = %d, %v; want %d", n, err, want)
	}
}

// BenchmarkIngest is B1: one node applying remote insert ops of 4 columns,
// nothing else writing.
func BenchmarkIngest(b *testing.B) {
	const n = 20000
	ops := benchOps(b, n)
	var elapsed time.Duration
	for b.Loop() {
		b.StopTimer()
		s, db := benchDest(b)
		b.StartTimer()
		start := time.Now()
		for _, op := range ops {
			if err := s.store.Ingest(context.Background(), op, false); err != nil {
				b.Fatal(err)
			}
		}
		elapsed += time.Since(start)
		b.StopTimer()
		benchCount(b, db, n)
		b.StartTimer()
	}
	b.ReportMetric(float64(b.N*len(ops))/elapsed.Seconds(), "ops/s")
}

// BenchmarkIngestConcurrent is B1 with the ops arriving from several senders
// at once, as pushes from many peers do (memTransport delivers each on its
// pusher's goroutine). Each Ingest still returns once its op is durable.
func BenchmarkIngestConcurrent(b *testing.B) {
	const n, senders = 20000, 16
	ops := benchOps(b, n)
	var elapsed time.Duration
	for b.Loop() {
		b.StopTimer()
		s, db := benchDest(b)
		b.StartTimer()
		start := time.Now()
		var next atomic.Int64
		var wg sync.WaitGroup
		for range senders {
			wg.Go(func() {
				for i := next.Add(1) - 1; i < int64(len(ops)); i = next.Add(1) - 1 {
					if err := s.store.Ingest(context.Background(), ops[i], false); err != nil {
						b.Error(err)
						return
					}
				}
			})
		}
		wg.Wait()
		elapsed += time.Since(start)
		b.StopTimer()
		benchCount(b, db, n)
		b.StartTimer()
	}
	b.ReportMetric(float64(b.N*len(ops))/elapsed.Seconds(), "ops/s")
}

// BenchmarkIngestWithLocalWriters is B2: B1 while local writers commit
// continuously on the same node. Every op must land.
func BenchmarkIngestWithLocalWriters(b *testing.B) {
	const n, writers = 20000, 2
	ops := benchOps(b, n)
	var busy atomic.Int64
	driver.BusyRetryAttemptHookForTest = func() { busy.Add(1) }
	defer func() { driver.BusyRetryAttemptHookForTest = nil }()
	for b.Loop() {
		b.StopTimer()
		s, db := benchDest(b)
		if _, err := db.Exec(`CREATE TABLE l(id INTEGER PRIMARY KEY, v)`); err != nil {
			b.Fatal(err)
		}
		busy.Store(0)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var local, localErr atomic.Int64
		for range writers {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := db.Exec(`INSERT INTO l(v) VALUES(?)`, time.Now().UnixNano()); err != nil {
						localErr.Add(1)
						continue
					}
					local.Add(1)
				}
			})
		}
		b.StartTimer()
		start := time.Now()
		dropped := 0
		for _, op := range ops {
			if err := s.store.Ingest(context.Background(), op, false); err != nil {
				dropped++
			}
		}
		elapsed := time.Since(start)
		b.StopTimer()
		close(stop)
		wg.Wait()
		b.ReportMetric(float64(len(ops))/elapsed.Seconds(), "ops/s")
		b.ReportMetric(float64(dropped), "dropped")
		b.ReportMetric(float64(busy.Load()), "busy-retries")
		b.ReportMetric(float64(local.Load())/elapsed.Seconds(), "local-writes/s")
		b.ReportMetric(float64(localErr.Load()), "local-errors")
		b.StartTimer()
	}
}

// BenchmarkCatchUp is B4: a new node taking a 100k-row database (BENCH_ROWS)
// from a peer over the in-memory transport.
func BenchmarkCatchUp(b *testing.B) {
	n := 100000
	if v, err := strconv.Atoi(os.Getenv("BENCH_ROWS")); err == nil && v > 0 {
		n = v
	}
	net := NewMemNetwork()
	src, _ := benchSource(b, n, memSync(net), WithSyncInterval(50*time.Millisecond))
	want := src.store.LocalVV()[src.site]
	for b.Loop() {
		start := time.Now()
		s, db, err := openSyncer(context.Background(), filepath.Join(b.TempDir(), "new.musq"), CRDT(), memSync(net), WithSyncInterval(50*time.Millisecond))
		if err != nil {
			b.Fatal(err)
		}
		for s.store.LocalVV()[src.site] < want {
			if time.Since(start) > 10*time.Minute {
				b.Fatalf("caught up to %d of %d", s.store.LocalVV()[src.site], want)
			}
			time.Sleep(5 * time.Millisecond)
		}
		b.ReportMetric(time.Since(start).Seconds(), "s/catch-up")
		b.StopTimer()
		benchCount(b, db, n)
		db.Close()
		b.StartTimer()
	}
}
