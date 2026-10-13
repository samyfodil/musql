// Command pointread measures a database/sql driver on two things: loading a
// table in one bulk transaction, and random point reads by primary key from
// concurrent goroutines. Every read's value is checked, so a fast wrong answer
// fails the run instead of winning it.
//
// The engine is chosen at build time: -tags musql or -tags mattn (C SQLite).
//
//	go run -tags musql . -rows 1000000 -reads 20000
//
// See bench/pointread/run, which runs both engines and compares them.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"
)

// report is the -json output, read by ./check.
type report struct {
	Engine    string `json:"engine"`
	Rows      int    `json:"rows"`
	Reads     int    `json:"reads"`
	LoadNs    int64  `json:"load_ns"`
	AvgNs     int64  `json:"avg_ns"`
	P50Ns     int64  `json:"p50_ns"`
	P90Ns     int64  `json:"p90_ns"`
	P95Ns     int64  `json:"p95_ns"`
	P99Ns     int64  `json:"p99_ns"`
	P999Ns    int64  `json:"p999_ns"`
	MaxNs     int64  `json:"max_ns"`
	DiskBytes int64  `json:"disk_bytes"`
}

func valueFor(k int) string { return fmt.Sprintf("value-%d", k) }

func main() {
	rows := flag.Int("rows", 10_000_000, "rows to load")
	workers := flag.Int("workers", 12, "concurrent readers")
	reads := flag.Int("reads", 50_000, "point reads per reader")
	dir := flag.String("dir", "", "directory for the database (default: a new temp dir, removed at exit)")
	jsonOut := flag.String("json", "", "also write the report as JSON to this file")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile of the whole run to this file")
	traceOut := flag.String("trace", "", "write a runtime/trace of the read phase to this file")
	flag.Parse()
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			fail(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fail(err)
		}
		defer pprof.StopCPUProfile()
	}
	if *rows <= 0 || *workers <= 0 || *reads <= 0 {
		fail(fmt.Errorf("-rows, -workers and -reads must be positive"))
	}

	d := *dir
	if d == "" {
		tmp, err := os.MkdirTemp("", "pointread-")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(tmp)
		d = tmp
	}
	path := filepath.Join(d, "pointread."+engine)
	db, err := openDB(path)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(*workers)
	db.SetMaxIdleConns(*workers)

	r := report{Engine: engine, Rows: *rows, Reads: *workers * *reads}
	load, err := loadTable(db, *rows)
	if err != nil {
		fail(err)
	}
	r.LoadNs = int64(load)

	if *traceOut != "" {
		f, err := os.Create(*traceOut)
		if err != nil {
			fail(err)
		}
		if err := trace.Start(f); err != nil {
			fail(err)
		}
	}
	lat, err := readAll(db, *rows, *workers, *reads)
	if *traceOut != "" {
		trace.Stop()
	}
	if err != nil {
		fail(err)
	}
	slices.Sort(lat)
	var sum time.Duration
	for _, l := range lat {
		sum += l
	}
	at := func(q float64) int64 { return int64(lat[min(int(float64(len(lat))*q), len(lat)-1)]) }
	r.AvgNs = int64(sum) / int64(len(lat))
	r.P50Ns, r.P90Ns, r.P95Ns = at(0.50), at(0.90), at(0.95)
	r.P99Ns, r.P999Ns, r.MaxNs = at(0.99), at(0.999), int64(lat[len(lat)-1])

	// Size while still open: whatever sidecar files the engine keeps count too.
	files, err := os.ReadDir(d)
	if err != nil {
		fail(err)
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), filepath.Base(path)) {
			continue
		}
		info, err := f.Info()
		if err != nil {
			fail(err)
		}
		r.DiskBytes += info.Size()
	}

	printReport(r)
	if *jsonOut != "" {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			fail(err)
		}
	}
}

// loadTable creates the table and inserts keys 1..n in one transaction through
// one prepared statement, the way an application would bulk-load.
func loadTable(db *sql.DB, n int) (time.Duration, error) {
	start := time.Now()
	if _, err := db.Exec("CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	ins, err := tx.Prepare("INSERT INTO kv (k, v) VALUES (?, ?)")
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	for k := 1; k <= n; k++ {
		if _, err := ins.Exec(k, valueFor(k)); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("insert %d: %w", k, err)
		}
	}
	if err := ins.Close(); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// readAll runs perWorker random point reads on each of workers goroutines and
// returns every read's latency. A wrong or missing value is an error.
func readAll(db *sql.DB, rows, workers, perWorker int) ([]time.Duration, error) {
	lat := make([]time.Duration, workers*perWorker)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w)+1, uint64(time.Now().UnixNano())))
			mine := lat[w*perWorker : (w+1)*perWorker]
			for i := range mine {
				k := rng.IntN(rows) + 1
				var v string
				t := time.Now()
				err := db.QueryRow("SELECT v FROM kv WHERE k = ?", k).Scan(&v)
				mine[i] = time.Since(t)
				if err != nil {
					errs[w] = fmt.Errorf("read %d: %w", k, err)
					return
				}
				if v != valueFor(k) {
					errs[w] = fmt.Errorf("read %d: got %q, want %q", k, v, valueFor(k))
					return
				}
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return lat, nil
}

func printReport(r report) {
	d := func(ns int64) time.Duration { return time.Duration(ns) }
	fmt.Printf("engine %s: %d rows, %d reads, all values checked\n", r.Engine, r.Rows, r.Reads)
	fmt.Printf("  load   %v\n", d(r.LoadNs).Round(time.Millisecond))
	fmt.Printf("  read   avg %v  p50 %v  p90 %v  p95 %v  p99 %v  p99.9 %v  max %v\n",
		d(r.AvgNs), d(r.P50Ns), d(r.P90Ns), d(r.P95Ns), d(r.P99Ns), d(r.P999Ns), d(r.MaxNs))
	fmt.Printf("  disk   %.1f MiB (%d bytes)\n", float64(r.DiskBytes)/(1<<20), r.DiskBytes)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pointread:", err)
	os.Exit(1)
}
