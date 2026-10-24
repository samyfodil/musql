package replication

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/musql/driver"
)

// ingestPages applies ops as Node does, a page of opsPerFetch per
// transaction, and reports how many did not land.
func ingestPages(s *Store, ops []Op) (dropped int) {
	for lo := 0; lo < len(ops); lo += opsPerFetch {
		page := ops[lo:min(lo+opsPerFetch, len(ops))]
		if k, err := s.ingestAll(context.Background(), page); err != nil {
			dropped += len(page) - max(k, 0)
		}
	}
	return dropped
}

// BenchmarkIngestPaged is B1 through the paged path the sync loop takes.
func BenchmarkIngestPaged(b *testing.B) {
	const n = 20000
	ops := benchOps(b, n)
	var elapsed time.Duration
	for b.Loop() {
		b.StopTimer()
		s, db := benchDest(b)
		b.StartTimer()
		start := time.Now()
		if d := ingestPages(s.store, ops); d != 0 {
			b.Fatalf("%d ops dropped", d)
		}
		elapsed += time.Since(start)
		b.StopTimer()
		benchCount(b, db, n)
		b.StartTimer()
	}
	b.ReportMetric(float64(b.N*len(ops))/elapsed.Seconds(), "ops/s")
}

// BenchmarkIngestPagedWithLocalWriters is B2 through the paged path.
func BenchmarkIngestPagedWithLocalWriters(b *testing.B) {
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
		dropped := ingestPages(s.store, ops)
		elapsed := time.Since(start)
		b.StopTimer()
		close(stop)
		wg.Wait()
		benchCount(b, db, n)
		b.ReportMetric(float64(len(ops))/elapsed.Seconds(), "ops/s")
		b.ReportMetric(float64(dropped), "dropped")
		b.ReportMetric(float64(busy.Load()), "busy-retries")
		b.ReportMetric(float64(local.Load())/elapsed.Seconds(), "local-writes/s")
		b.ReportMetric(float64(localErr.Load()), "local-errors")
		b.StartTimer()
	}
}
