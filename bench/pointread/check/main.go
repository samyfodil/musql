// Command check compares two pointread -json reports and fails unless musql is
// at least as good as C SQLite on load time, avg/p50/p99 read latency and bytes
// on disk.
//
//	go run ./check musql.json c-sqlite.json
//
// The other percentiles and max are printed but not gated: max is one sample,
// and on a shared CI machine one scheduler hiccup decides it for either engine.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

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

func load(path string) report {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var r report
	if err := json.Unmarshal(b, &r); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		os.Exit(2)
	}
	return r
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: check musql.json c-sqlite.json")
		os.Exit(2)
	}
	m, c := load(os.Args[1]), load(os.Args[2])
	if m.Rows != c.Rows || m.Reads != c.Reads {
		fmt.Fprintf(os.Stderr, "the runs differ in size: %d/%d vs %d/%d rows/reads\n", m.Rows, m.Reads, c.Rows, c.Reads)
		os.Exit(2)
	}
	dur := func(ns int64) string { return time.Duration(ns).String() }
	size := func(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
	metrics := []struct {
		name  string
		m, c  int64
		show  func(int64) string
		gated bool
	}{
		{"load", m.LoadNs, c.LoadNs, dur, true},
		{"avg", m.AvgNs, c.AvgNs, dur, true},
		{"p50", m.P50Ns, c.P50Ns, dur, true},
		{"p90", m.P90Ns, c.P90Ns, dur, false},
		{"p95", m.P95Ns, c.P95Ns, dur, false},
		{"p99", m.P99Ns, c.P99Ns, dur, true},
		{"p99.9", m.P999Ns, c.P999Ns, dur, false},
		{"max", m.MaxNs, c.MaxNs, dur, false},
		{"disk", m.DiskBytes, c.DiskBytes, size, true},
	}
	failed := false
	fmt.Printf("%-6s %14s %14s %7s\n", "metric", m.Engine, c.Engine, "ratio")
	for _, x := range metrics {
		note := ""
		if x.m > x.c {
			note = "  (worse, not gated)"
			if x.gated {
				note = "  WORSE"
				failed = true
			}
		}
		fmt.Printf("%-6s %14s %14s %6.2fx%s\n", x.name, x.show(x.m), x.show(x.c), float64(x.m)/float64(x.c), note)
	}
	if failed {
		fmt.Println("FAIL: musql is worse than C SQLite on a gated metric")
		os.Exit(1)
	}
	fmt.Println("ok: musql matches or beats C SQLite on every gated metric")
}
