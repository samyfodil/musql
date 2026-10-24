package libp2p_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	p2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	replp2p "github.com/samyfodil/musql/examples/libp2p"
	"github.com/samyfodil/musql/replication"
)

const lagDDL = `CREATE TABLE t(id INTEGER PRIMARY KEY, w INTEGER, k INTEGER, ts INTEGER)`

func benchHosts(b *testing.B, n int) []host.Host {
	hs := make([]host.Host, n)
	for i := range hs {
		h, err := p2p.New(p2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { h.Close() })
		hs[i] = h
	}
	for i := range hs {
		for j := range i {
			if err := hs[i].Connect(context.Background(), peer.AddrInfo{ID: hs[j].ID(), Addrs: hs[j].Addrs()}); err != nil {
				b.Fatal(err)
			}
		}
	}
	return hs
}

func benchOpen(b *testing.B, h host.Host, name string, ddl ...string) *sql.DB {
	return benchOpenAt(b, h, filepath.Join(b.TempDir(), name+".musq"), ddl...)
}

func benchOpenAt(b *testing.B, h host.Host, path string, ddl ...string) *sql.DB {
	if len(ddl) > 0 {
		db0, err := sql.Open("sqlite", path)
		if err != nil {
			b.Fatal(err)
		}
		for _, s := range ddl {
			if _, err := db0.Exec(s); err != nil {
				b.Fatal(err)
			}
		}
		db0.Close()
	}
	db, err := replication.Open(context.Background(), path, replication.CRDT(), replp2p.Sync(h, "bench"), replication.WithSyncInterval(500*time.Millisecond))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

func benchSeconds() time.Duration {
	if s, err := strconv.Atoi(os.Getenv("BENCH_SECONDS")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 30 * time.Second
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[min(len(d)-1, int(float64(len(d))*p))]
}

// BenchmarkLag is B3: three nodes each writing 500 rows/s; the lag from a
// row's commit to the moment every other node shows it, and how long after
// the writes stop every node holds every row.
func BenchmarkLag(b *testing.B) {
	const nodes, rate = 3, 500
	dur := benchSeconds()
	for b.Loop() {
		hs := benchHosts(b, nodes)
		dbs := make([]*sql.DB, nodes)
		for i := range dbs {
			dbs[i] = benchOpen(b, hs[i], fmt.Sprint("n", i), lagDDL, `CREATE INDEX t_wk ON t(w, k)`)
		}
		time.Sleep(time.Second) // the gossipsub mesh
		committed := make([][]time.Time, nodes)
		seen := make([][][]time.Time, nodes) // seen[node][writer][k-1]
		for i := range seen {
			seen[i] = make([][]time.Time, nodes)
		}
		var mu sync.Mutex
		stop := make(chan struct{})
		var wg, watch sync.WaitGroup
		for w, db := range dbs {
			wg.Go(func() {
				t := time.NewTicker(time.Second / rate)
				defer t.Stop()
				end := time.Now().Add(dur)
				for k := 1; time.Now().Before(end); k++ {
					<-t.C
					if _, err := db.Exec(`INSERT INTO t(w, k, ts) VALUES(?, ?, ?)`, w, k, time.Now().UnixNano()); err != nil {
						b.Error(err)
						return
					}
					mu.Lock()
					committed[w] = append(committed[w], time.Now())
					mu.Unlock()
				}
			})
		}
		poll := func() {
			for x, db := range dbs {
				for w := range nodes {
					if w == x {
						continue
					}
					var k sql.NullInt64
					if err := db.QueryRow(`SELECT max(k) FROM t WHERE w = ?`, w).Scan(&k); err != nil {
						b.Error(err)
						return
					}
					now := time.Now()
					for len(seen[x][w]) < int(k.Int64) {
						seen[x][w] = append(seen[x][w], now)
					}
				}
			}
		}
		watch.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				poll()
				time.Sleep(2 * time.Millisecond)
			}
		})
		wg.Wait()
		stopped := time.Now()
		total := 0
		for _, c := range committed {
			total += len(c)
		}
		converged := time.Duration(-1)
		for time.Since(stopped) < 2*time.Minute {
			all := true
			for _, db := range dbs {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != total {
					all = false
					break
				}
			}
			if all {
				converged = time.Since(stopped)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(stop)
		watch.Wait()
		poll()
		var lags []time.Duration
		for w := range nodes {
			for k, at := range committed[w] {
				var last time.Time
				for x := range nodes {
					if x == w {
						continue
					}
					if k >= len(seen[x][w]) {
						last = time.Time{}
						break
					}
					if seen[x][w][k].After(last) {
						last = seen[x][w][k]
					}
				}
				if !last.IsZero() {
					lags = append(lags, max(last.Sub(at), 0))
				}
			}
		}
		b.ReportMetric(float64(total)/dur.Seconds()/nodes, "writes/s/node")
		b.ReportMetric(float64(pct(lags, 0.5).Milliseconds()), "p50-ms")
		b.ReportMetric(float64(pct(lags, 0.99).Milliseconds()), "p99-ms")
		b.ReportMetric(converged.Seconds(), "converged-s")
		b.ReportMetric(float64(total-len(lags)), "unseen")
	}
}

// BenchmarkCatchUp is B4 over libp2p: a new node taking a 100k-row database.
func BenchmarkCatchUp(b *testing.B) {
	const n = 100000
	for b.Loop() {
		hs := benchHosts(b, 2)
		// Written before sync starts: a lone node prunes what it alone holds.
		path := filepath.Join(b.TempDir(), "src.musq")
		db0, err := sql.Open("sqlite", path)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db0.Exec(lagDDL); err != nil {
			b.Fatal(err)
		}
		db0.Close()
		src, err := replication.Open(context.Background(), path, replication.CRDT())
		if err != nil {
			b.Fatal(err)
		}
		for lo := 0; lo < n; lo += 5000 {
			tx, err := src.Begin()
			if err != nil {
				b.Fatal(err)
			}
			for k := lo; k < lo+5000; k++ {
				if _, err := tx.Exec(`INSERT INTO t(w, k, ts) VALUES(0, ?, ?)`, k, k); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
		}
		src.Close()
		benchOpenAt(b, hs[0], path)
		start := time.Now()
		dst := benchOpen(b, hs[1], "dst")
		for {
			var got int
			if err := dst.QueryRow(`SELECT count(*) FROM t`).Scan(&got); err == nil && got == n {
				break
			}
			if time.Since(start) > 10*time.Minute {
				b.Fatal("no catch-up in 10 minutes")
			}
			time.Sleep(10 * time.Millisecond)
		}
		b.ReportMetric(time.Since(start).Seconds(), "s/catch-up")
	}
}
