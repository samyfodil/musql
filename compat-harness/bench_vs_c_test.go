// Performance benchmark comparing musql and C SQLite through database/sql.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"     // driver "sqlite3" (C SQLite, CGo)
	"github.com/samyfodil/musql/driver" // driver "musql"
)

// Benchmark sizing and seeding for reproducibility.
const benchSeed = 0x5eed

// benchN is the rows in t and in b: 100,000, or BENCH_ROWS when set (a larger
// table shows how a ratio moves as per-call overhead shrinks against the work).
var benchN = func() int {
	if n, err := strconv.Atoi(os.Getenv("BENCH_ROWS")); err == nil && n > 0 {
		return n
	}
	return 100_000
}()

// Iteration counts per workload type.
const (
	iterPoint   = 3000
	iterIndex   = 3000
	iterJoin    = 3000
	iterScan    = 40
	iterGroup   = 40
	iterOrder   = 40
	writeBatchN = 20_000 // rows inserted in the write-throughput workload
)

type benchEngine struct {
	label  string // human label in the report
	driver string // database/sql driver name
	path   string // its own on-disk file
}

// TestBenchVsC seeds identical disk-backed databases for all three engines and
// times each workload against each, printing one comparison table. Run it with:
//
//	go test ./compat-harness/ -run TestBenchVsC -v -timeout 30m
//
// (it is skipped under -short because seeding + timing 100k rows across three
// engines, one of which rebuilds its whole b-tree per write commit, is not a
// unit-test-speed operation).
func TestBenchVsC(t *testing.T) {
	if testing.Short() {
		t.Skip("bench_vs_c: skipped under -short (100k-row multi-engine benchmark)")
	}

	// TestMain (harness_test.go) forces the post-commit structural check on
	// for the whole binary; this benchmark opens driver directly
	// in-process (not via the worker-subprocess bridge the corpus tests use),
	// so it inherits it with no opt-out, and re-walking every committed
	// b-tree is not what this is timing.

	dir := t.TempDir()
	engines := []*benchEngine{
		{label: "musql", driver: driver.DriverName, path: filepath.Join(dir, "musql.db")},
		{label: "mattn-C", driver: "sqlite3", path: filepath.Join(dir, "mattn.db")},
	}

	// Seed every engine identically, and keep one live *sql.DB per engine.
	dbs := make(map[string]*sql.DB, len(engines))
	for _, e := range engines {
		t.Logf("seeding %-11s (%s): %d rows ...", e.label, e.driver, benchN)
		start := time.Now()
		db := seedBench(t, e)
		dbs[e.label] = db
		defer db.Close()
		t.Logf("seeding %-11s done in %s", e.label, time.Since(start).Round(time.Millisecond))
	}

	// results[workload][engineLabel] = per-op duration.
	results := map[string]map[string]time.Duration{}
	record := func(workload, label string, d time.Duration) {
		if results[workload] == nil {
			results[workload] = map[string]time.Duration{}
		}
		results[workload][label] = d
	}

	// ---- Workload 1: rowid point lookup (SELECT * FROM t WHERE id = ?) ----
	for _, e := range engines {
		d := timeLookup(t, dbs[e.label], "SELECT * FROM t WHERE id = ?", iterPoint,
			func(rng *rand.Rand) any { return 1 + rng.Intn(benchN) })
		record("1. rowid point lookup", e.label, d)
	}

	// ---- Workload 2: secondary-index equality (WHERE sec = ?) ----
	for _, e := range engines {
		d := timeLookup(t, dbs[e.label], "SELECT * FROM t WHERE sec = ?", iterIndex,
			func(rng *rand.Rand) any { return rng.Intn(benchN) })
		record("2. secondary-index eq", e.label, d)
	}

	// ---- Workload 3: indexed equi-join (both sides keyed by rowid) ----
	for _, e := range engines {
		d := timeLookup(t, dbs[e.label],
			"SELECT t.id, t.v, b.label FROM t JOIN b ON b.id = t.bid WHERE t.id = ?", iterJoin,
			func(rng *rand.Rand) any { return 1 + rng.Intn(benchN) })
		record("3. indexed equi-join", e.label, d)
	}

	// ---- Workload 4: full-table scan with a non-indexed filter ----
	for _, e := range engines {
		d := timeLookup(t, dbs[e.label], "SELECT count(*) FROM t WHERE v > ?", iterScan,
			func(rng *rand.Rand) any { return rng.Intn(1_000_000) })
		record("4. full-scan count filter", e.label, d)
	}

	// ---- Workload 5: aggregate / GROUP BY ----
	for _, e := range engines {
		d := timeQueryNoArgs(t, dbs[e.label],
			"SELECT k, count(*), sum(v) FROM t GROUP BY k", iterGroup)
		record("5. aggregate GROUP BY", e.label, d)
	}

	// ---- Workload 6: ORDER BY + LIMIT (non-indexed sort key) ----
	for _, e := range engines {
		d := timeQueryNoArgs(t, dbs[e.label],
			"SELECT id, v FROM t ORDER BY v DESC LIMIT 20", iterOrder)
		record("6. ORDER BY v DESC LIMIT 20", e.label, d)
	}

	// ---- Workload 7: write throughput (batch INSERT in one tx, bulk UPDATE, bulk DELETE) ----
	for _, e := range engines {
		ins, upd, del := timeWrites(t, dbs[e.label])
		record("7a. batch INSERT (per row)", e.label, ins)
		record("7b. bulk UPDATE (per stmt)", e.label, upd)
		record("7c. bulk DELETE (per stmt)", e.label, del)
	}

	// ---- report ----
	workloadOrder := []string{
		"1. rowid point lookup",
		"2. secondary-index eq",
		"3. indexed equi-join",
		"4. full-scan count filter",
		"5. aggregate GROUP BY",
		"6. ORDER BY v DESC LIMIT 20",
		"7a. batch INSERT (per row)",
		"7b. bulk UPDATE (per stmt)",
		"7c. bulk DELETE (per stmt)",
	}
	reportBench(t, workloadOrder, results)
}

// seedBench creates the schema+indexes and inserts benchN identical rows into
// engine e's own file, returning a live *sql.DB. All inserts run inside ONE
// transaction per table so musql pays its whole-table rebuild cost once
// (and mattn/modernc avoid per-row fsync) -- the same, fair bulk-load shape for
// all three.
func seedBench(t *testing.T, e *benchEngine) *sql.DB {
	t.Helper()
	db, err := sql.Open(e.driver, e.path)
	if err != nil {
		t.Fatalf("[%s] open: %v", e.label, err)
	}
	// One connection: musql's autocommit model has no shared cache to gain
	// from a pool, and pinning to one conn keeps the on-disk file single-writer
	// for all three, which is the fair comparison.
	db.SetMaxOpenConns(1)

	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("[%s] exec %q: %v", e.label, q, err)
		}
	}
	exec(`CREATE TABLE t (
		id      INTEGER PRIMARY KEY,
		sec     INTEGER,
		k       INTEGER,
		v       INTEGER,
		bid     INTEGER,
		payload TEXT
	)`)
	exec(`CREATE TABLE b (
		id    INTEGER PRIMARY KEY,
		label TEXT
	)`)
	exec(`CREATE INDEX idx_t_sec ON t(sec)`)

	rng := rand.New(rand.NewSource(benchSeed))

	// Table b first (t.bid references b.id).
	if err := bulkInsert(db, "INSERT INTO b(id,label) VALUES(?,?)", benchN, func(i int) []any {
		return []any{i, fmt.Sprintf("label-%d", i)}
	}); err != nil {
		t.Fatalf("[%s] seed b: %v", e.label, err)
	}
	// Table t.
	if err := bulkInsert(db, "INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)", benchN, func(i int) []any {
		return []any{
			i,                    // id (rowid, 1..benchN)
			rng.Intn(benchN),     // sec: ~unique secondary key
			rng.Intn(10),         // k: low-cardinality GROUP BY key
			rng.Intn(1_000_000),  // v: non-indexed value
			1 + rng.Intn(benchN), // bid: -> b.id
			fmt.Sprintf("row-%d-payload", i),
		}
	}); err != nil {
		t.Fatalf("[%s] seed t: %v", e.label, err)
	}
	return db
}

// bulkInsert runs n inserts inside a single transaction using a prepared stmt.
func bulkInsert(db *sql.DB, q string, n int, row func(i int) []any) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(q)
	if err != nil {
		tx.Rollback()
		return err
	}
	for i := 1; i <= n; i++ {
		if _, err := stmt.Exec(row(i)...); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}

// timeLookup warms once then times `iters` executions of a single-parameter
// query, fully draining every result row (so the engine really produces them).
// The PRNG is re-seeded from benchSeed so every engine sees the identical bind
// sequence. Returns per-op duration; logs and returns 0 on the first error.
func timeLookup(t *testing.T, db *sql.DB, query string, iters int, arg func(*rand.Rand) any) time.Duration {
	t.Helper()
	// warmup
	if err := runOne(db, query, arg(rand.New(rand.NewSource(benchSeed)))); err != nil {
		t.Logf("  (skip) %q: %v", query, err)
		return 0
	}
	rng := rand.New(rand.NewSource(benchSeed))
	start := time.Now()
	for i := 0; i < iters; i++ {
		if err := runOne(db, query, arg(rng)); err != nil {
			t.Logf("  (error) %q: %v", query, err)
			return 0
		}
	}
	return time.Since(start) / time.Duration(iters)
}

// timeQueryNoArgs warms once then times `iters` executions of an argument-less
// query, draining every row.
func timeQueryNoArgs(t *testing.T, db *sql.DB, query string, iters int) time.Duration {
	t.Helper()
	if err := runOne(db, query); err != nil {
		t.Logf("  (skip) %q: %v", query, err)
		return 0
	}
	start := time.Now()
	for i := 0; i < iters; i++ {
		if err := runOne(db, query); err != nil {
			t.Logf("  (error) %q: %v", query, err)
			return 0
		}
	}
	return time.Since(start) / time.Duration(iters)
}

// runOne executes a query and drains all rows/columns, discarding values.
func runOne(db *sql.DB, query string, args ...any) error {
	rows, err := db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	holders := make([]any, len(cols))
	scan := make([]any, len(cols))
	for i := range holders {
		scan[i] = &holders[i]
	}
	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			return err
		}
	}
	return rows.Err()
}

// timeWrites measures the three write workloads on db, in order:
//
//	7a batch INSERT of writeBatchN fresh rows in ONE transaction (per-row time);
//	7b a bulk UPDATE touching ~1/10 of the table (single statement);
//	7c a bulk DELETE removing those same rows (single statement).
//
// These mutate the database and run AFTER every read workload, so they don't
// perturb the earlier measurements. Ordering update-before-delete keeps both
// meaningful (the deleted rows are the ones just updated).
func timeWrites(t *testing.T, db *sql.DB) (ins, upd, del time.Duration) {
	t.Helper()

	// 7a: batch insert writeBatchN rows (ids past the seeded range) in one tx.
	start := time.Now()
	err := bulkInsert(db, "INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)", writeBatchN, func(i int) []any {
		id := benchN + i
		return []any{id, id % benchN, i % 10, i, 1, "w"}
	})
	if err != nil {
		t.Logf("  (error) batch INSERT: %v", err)
		return 0, 0, 0
	}
	ins = time.Since(start) / time.Duration(writeBatchN)

	// 7b and 7c: bulk UPDATE ~1/10 of the (now larger) table, then bulk DELETE
	// those same rows -- ONCE PER ROUND, over a DIFFERENT tenth each time.
	//
	// These were a single statement timed once, which is one sample. It showed:
	// the same commit measured 2.83x, 4.56x and 4.45x against C across runs, and
	// the ratio moved further between two runs on one box than the whole of this
	// session's write-path work moved it. A one-sample number is not a result, and
	// this table is what every claim about beating C rests on.
	//
	// A DIFFERENT k per round is what makes repetition possible at all: these
	// statements MUTATE, so re-running the same one measures an already-updated
	// (or already-deleted) tenth. k holds ten values spread evenly over the table,
	// so rounds are the same size and independent, and update-before-delete within
	// a round keeps 7c deleting exactly the rows 7b just touched -- which is the
	// ordering the two were written to have.
	//
	// Best-of, for the reason a latency benchmark takes the minimum: the noise is
	// additive -- a steal, a GC, an fsync behind someone else's -- so the fastest
	// round is closest to the work itself, and both engines are measured the same
	// way in the same process.
	const writeRounds = 5
	for r := 0; r < writeRounds; r++ {
		k := 3 + r // a distinct tenth per round; k is 0..9 across the table
		start = time.Now()
		if _, err := db.Exec("UPDATE t SET v = v + 1 WHERE k = ?", k); err != nil {
			t.Logf("  (error) bulk UPDATE: %v", err)
			return ins, 0, 0
		}
		if d := time.Since(start); upd == 0 || d < upd {
			upd = d
		}

		start = time.Now()
		if _, err := db.Exec("DELETE FROM t WHERE k = ?", k); err != nil {
			t.Logf("  (error) bulk DELETE: %v", err)
			return ins, upd, 0
		}
		if d := time.Since(start); del == 0 || d < del {
			del = d
		}
	}
	return ins, upd, del
}

// reportBench prints the final comparison table: one row per workload, one
// column per engine (per-op wall time), plus the musql-vs-mattn(C) ratio.
func reportBench(t *testing.T, order []string, results map[string]map[string]time.Duration) {
	t.Helper()
	const (
		musql = "musql"
		mattn = "mattn-C"
	)
	line := "----------------------------------------------------------------------------------------------"
	t.Logf("\nBENCHMARK: %d rows, disk-backed (t.TempDir), warm, seed=0x%x\n%s",
		benchN, benchSeed, line)
	t.Logf("%-30s | %14s | %14s | %s", "workload", musql, mattn+" (C)", "musql/C")
	t.Logf("%s", line)
	for _, w := range order {
		r := results[w]
		mv, mok := r[musql]
		cv, cok := r[mattn]
		ratio := "-"
		if mok && cok && cv > 0 {
			ratio = fmt.Sprintf("%.2fx", float64(mv)/float64(cv))
		}
		t.Logf("%-30s | %14s | %14s | %s",
			w, fmtDur(mv, mok), fmtDur(cv, cok), ratio)
	}
	t.Logf("%s", line)
	t.Logf("musql/C < 1.00 => musql FASTER than C SQLite; > 1.00 => slower.")
}

func fmtDur(d time.Duration, ok bool) string {
	if !ok || d == 0 {
		return "n/a"
	}
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fµs", float64(d.Nanoseconds())/1e3)
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	default:
		return fmt.Sprintf("%.3fs", d.Seconds())
	}
}
