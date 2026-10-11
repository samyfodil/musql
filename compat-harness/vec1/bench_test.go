package vec1

import (
	"container/heap"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// TestVec1Bench measures musql against C SQLite + vec1 on the same data:
// the time to insert, train and build an index, and each query's latency and
// recall@10 against the exact answer. Set VEC1_BENCH=1 to run; build with
// -tags vec1simd for the oracle compiled the way vec1 says to build it for
// speed. VEC1_BENCH_N (rows, default 100000), VEC1_BENCH_D (dimensions,
// 384), VEC1_BENCH_DATA (random or clustered), VEC1_BENCH_Q (queries, 200),
// VEC1_BENCH_THREADS (vec1_config nthread, 1) and VEC1_BENCH_INDEX (a list of
// flat, ivfpq, ivfopq, ivf, ivfbq) choose the run.
func TestVec1Bench(t *testing.T) {
	if os.Getenv("VEC1_BENCH") == "" {
		t.Skip("VEC1_BENCH not set")
	}
	n := envInt("VEC1_BENCH_N", 100_000)
	d := envInt("VEC1_BENCH_D", 384)
	nq := envInt("VEC1_BENCH_Q", 200)
	threads := envInt("VEC1_BENCH_THREADS", 1)
	data := envStr("VEC1_BENCH_DATA", "clustered")
	indexes := strings.Split(envStr("VEC1_BENCH_INDEX", "flat,ivfpq,ivfopq"), ",")
	if w := envInt("VEC1_BENCH_WORKERS", 0); w > 0 {
		engine.Configure(engine.WithWorkers(w))
		defer engine.Configure()
	}

	r := rand.New(rand.NewPCG(7, 7))
	var vecs [][]float32
	switch data {
	case "random":
		vecs = benchRandom(r, n+nq, d)
	default:
		vecs = benchClustered(r, n+nq, d)
	}
	queries := vecs[n:]
	vecs = vecs[:n]
	t.Logf("oracle build %s, %d CPUs, n=%d d=%d data=%s queries=%d nthread=%d", oracleBuild, runtime.NumCPU(), n, d, data, nq, threads)
	t0 := time.Now()
	truth := exactTop(vecs, queries, 10)
	t.Logf("exact answers in %v", time.Since(t0).Round(time.Millisecond))

	nbucket := int(math.Sqrt(float64(n)))
	type cfg struct {
		name   string
		model  string // JSON model for flat; vec1_train config otherwise
		train  bool
		k      int // K for the index
		rerank bool
		nprobe float64
	}
	var cfgs []cfg
	for _, ix := range indexes {
		switch ix {
		case "flat":
			cfgs = append(cfgs, cfg{name: "flat", model: `{index:"flat", distance:"l2"}`, k: 10})
		case "none":
			cfgs = append(cfgs, cfg{name: "none", k: 10})
		case "ivf":
			cfgs = append(cfgs, cfg{name: "ivf", model: fmt.Sprintf(`{nbucket:%d}`, nbucket), train: true, k: 10, nprobe: 0.05})
		case "ivfpq":
			cfgs = append(cfgs, cfg{name: "ivfpq", model: fmt.Sprintf(`{nbucket:%d, quantizer:"pq", codesize:%d}`, nbucket, d/8), train: true, k: 100, rerank: true, nprobe: 0.05})
		case "ivfopq":
			cfgs = append(cfgs, cfg{name: "ivfopq", model: fmt.Sprintf(`{nbucket:%d, quantizer:"opq", codesize:32}`, nbucket), train: true, k: 100, rerank: true, nprobe: 0.05})
		case "ivfbq":
			cfgs = append(cfgs, cfg{name: "ivfbq", model: fmt.Sprintf(`{nbucket:%d, quantizer:"bq"}`, nbucket), train: true, k: 100, rerank: true, nprobe: 0.05})
		}
	}

	type result struct {
		insert, train, build time.Duration
		lat                  []time.Duration
		recall               float64
	}
	dir := t.TempDir()
	for _, c := range cfgs {
		res := map[string]*result{}
		for _, eng := range []string{"C", "musql"} {
			path := filepath.Join(dir, fmt.Sprintf("%s-%s.db", eng, c.name))
			var db *sql.DB
			var err error
			if eng == "C" {
				db, err = openOracle(path)
			} else {
				db, err = sql.Open(driver.DriverName, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			// VEC1_BENCH_PROFILE=<prefix> writes musql's CPU profile of each
			// stage to <prefix>-<index>-<stage>.pprof.
			profile := func(stage string) func() {
				prefix := os.Getenv("VEC1_BENCH_PROFILE")
				if prefix == "" || eng != "musql" {
					return func() {}
				}
				f, err := os.Create(fmt.Sprintf("%s-%s-%s.pprof", prefix, c.name, stage))
				if err != nil {
					t.Fatal(err)
				}
				if err := pprof.StartCPUProfile(f); err != nil {
					t.Fatal(err)
				}
				return func() {
					pprof.StopCPUProfile()
					f.Close()
				}
			}
			mustExec(t, db, fmt.Sprintf("SELECT vec1_config('nthread', %d)", threads))
			mustExec(t, db, "CREATE VIRTUAL TABLE v USING vec1(vector)")
			rs := &result{}
			start := time.Now()
			stopInsert := profile("insert")
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			ins, err := tx.Prepare("INSERT INTO v(rowid, vector) VALUES(?, ?)")
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range vecs {
				if _, err := ins.Exec(i+1, vecBlob(v)); err != nil {
					t.Fatal(err)
				}
			}
			ins.Close()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			stopInsert()
			rs.insert = time.Since(start)
			var model any = c.model
			if c.train {
				// vec1's advice: train on about 100 vectors per bucket.
				step := max(1, n/(100*nbucket))
				start = time.Now()
				var blob []byte
				stop := profile("train")
				if err := db.QueryRow(fmt.Sprintf("SELECT vec1_train(vector, ?) FROM v WHERE rowid %% %d = 0", step), c.model).Scan(&blob); err != nil {
					t.Fatalf("%s %s train: %v", eng, c.name, err)
				}
				stop()
				rs.train = time.Since(start)
				model = blob
			}
			if c.model != "" || c.train {
				start = time.Now()
				stop := profile("build")
				mustExec(t, db, "INSERT INTO v(cmd, arg) VALUES('rebuild', ?)", model)
				stop()
				rs.build = time.Since(start)
			}
			// A table read more than written: compacted, as the libSQL
			// benchmarks do.
			if eng == "musql" {
				mustExec(t, db, "VACUUM")
			}
			// Searched by a connection that did not build it, as a database
			// is once it is served: both engines reopen the file.
			db.Close()
			if eng == "C" {
				db, err = openOracle(path)
			} else {
				db, err = sql.Open(driver.DriverName, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			mustExec(t, db, fmt.Sprintf("SELECT vec1_config('nthread', %d)", threads))
			params := fmt.Sprintf(`{k:%d`, c.k)
			if c.nprobe > 0 {
				params += fmt.Sprintf(`, nprobe:%g`, c.nprobe)
			}
			params += "}"
			q := "SELECT rowid FROM v(?, ?)"
			if c.rerank {
				q = "SELECT rowid FROM v(?, ?) ORDER BY vec1_l2_distance(?, vector) LIMIT 10"
			}
			stmt, err := db.Prepare(q)
			if err != nil {
				t.Fatal(err)
			}
			hits := 0
			stop := profile("query")
			// One warm-up, then each query once.
			for qi := -1; qi < len(queries); qi++ {
				qv := queries[max(qi, 0)]
				qb := vecBlob(qv)
				args := []any{qb, params}
				if c.rerank {
					args = append(args, qb)
				}
				start = time.Now()
				rows, err := stmt.Query(args...)
				if err != nil {
					t.Fatalf("%s %s query: %v", eng, c.name, err)
				}
				var got []int64
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					got = append(got, id)
				}
				rows.Close()
				el := time.Since(start)
				if qi < 0 {
					continue
				}
				rs.lat = append(rs.lat, el)
				if len(got) > 10 {
					got = got[:10]
				}
				want := map[int64]bool{}
				for _, id := range truth[qi] {
					want[id] = true
				}
				for _, id := range got {
					if want[id] {
						hits++
					}
				}
			}
			stmt.Close()
			stop()
			rs.recall = float64(hits) / float64(10*len(queries))
			res[eng] = rs
			db.Close()
		}
		for _, eng := range []string{"C", "musql"} {
			rs := res[eng]
			sort.Slice(rs.lat, func(i, j int) bool { return rs.lat[i] < rs.lat[j] })
			var sum time.Duration
			for _, l := range rs.lat {
				sum += l
			}
			t.Logf("%-7s %-6s insert %8v  train %8v  build %8v  query mean %8v p50 %8v p99 %8v  recall@10 %.3f",
				c.name, eng, rs.insert.Round(time.Millisecond), rs.train.Round(time.Millisecond), rs.build.Round(time.Millisecond),
				(sum / time.Duration(len(rs.lat))).Round(time.Microsecond), rs.lat[len(rs.lat)/2].Round(time.Microsecond),
				rs.lat[len(rs.lat)*99/100].Round(time.Microsecond), rs.recall)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	rows.Close()
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// benchRandom is independent N(0,1) components, as the libSQL benchmarks use.
func benchRandom(r *rand.Rand, n, d int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, d)
		for j := range v {
			v[j] = float32(r.NormFloat64())
		}
		out[i] = v
	}
	return out
}

// benchClustered is the libSQL benchmarks' clusteredBlobs: 1000 N(0,1)
// centres, each vector one of them plus N(0, 0.25) noise.
func benchClustered(r *rand.Rand, n, d int) [][]float32 {
	cr := rand.New(rand.NewPCG(42, 42))
	centres := make([][]float64, 1000)
	for i := range centres {
		centres[i] = make([]float64, d)
		for j := range centres[i] {
			centres[i][j] = cr.NormFloat64()
		}
	}
	out := make([][]float32, n)
	for i := range out {
		c := centres[r.IntN(len(centres))]
		v := make([]float32, d)
		for j := range v {
			v[j] = float32(c[j] + 0.5*r.NormFloat64())
		}
		out[i] = v
	}
	return out
}

// exactTop is each query's k nearest rowids (rowid = index+1) by squared L2,
// computed in float64, in parallel.
func exactTop(vecs, queries [][]float32, k int) [][]int64 {
	out := make([][]int64, len(queries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for qi, q := range queries {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			h := &maxHeap{}
			for i, v := range vecs {
				var s float64
				for j, x := range v {
					e := float64(x) - float64(q[j])
					s += e * e
				}
				if h.Len() < k {
					heap.Push(h, item{s, int64(i + 1)})
				} else if s < (*h)[0].d {
					(*h)[0] = item{s, int64(i + 1)}
					heap.Fix(h, 0)
				}
			}
			ids := make([]int64, h.Len())
			for i, it := range *h {
				ids[i] = it.id
			}
			out[qi] = ids
		}()
	}
	wg.Wait()
	return out
}

type item struct {
	d  float64
	id int64
}
type maxHeap []item

func (h maxHeap) Len() int           { return len(h) }
func (h maxHeap) Less(i, j int) bool { return h[i].d > h[j].d }
func (h maxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *maxHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
