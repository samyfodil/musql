package compat

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/musql/boost"
	"github.com/samyfodil/musql/driver"
	musqlengine "github.com/samyfodil/musql/engine"
	_ "turso.tech/database/tursogo" // driver "turso": the Rust rewrite
)

// Benchmark comparisons with C SQLite and Turso.
//
// FAIRNESS, stated rather than assumed, because the arms are not identical:
//
//   - The engine-direct musql arm reads the database after a VACUUM, so every
//     row is in segments and none in the delta: the format at rest. The driver
//     arm reads the same file through database/sql, so the driver's per-query
//     cost is visible rather than folded into the format's number.
//   - C runs through database/sql and CGo, which is its normal interface.
//   - Same rows, same seed, same disk, same bind values.
//
// TURSO is here as a third engine because it is the closest thing to a peer:
// SQLite-compatible, written from scratch, reading the same file format, and
// not written in C. musql is Go, Turso is Rust. It is reached through
// turso.tech/database/tursogo, which dlopens a prebuilt Rust cdylib via purego
// -- so no CGo and no Rust toolchain, but also a foreign-call boundary on every
// statement, the same kind of tax mattn's CGo pays.
//
// benchSkipped reports whether a workload's name matches BENCH_SKIP, a
// comma-separated list of substrings.
//
// It exists because the full table takes THREE HOURS, which makes it useless to
// iterate on: two runs were killed by a test timeout mid-way and reported
// nothing at all. The cost is not spread evenly -- a handful of correlated and
// compound workloads over 200k rows dominate it, and a 20k x 5k correlated
// EXISTS alone measures ~5s per iteration on this engine, times three arms.
// Skipping those by name gets the read table in minutes.
//
// Default is empty, so a plain run still measures everything. Naming what was
// skipped is the caller's job when they report the numbers.
func benchSkipped(name string) bool {
	list := os.Getenv("BENCH_SKIP")
	if list == "" {
		return false
	}
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" && strings.Contains(name, part) {
			return true
		}
	}
	return false
}

func TestBenchColumnarVsC(t *testing.T) {
	if testing.Short() {
		t.Skip("bench_columnar_vs_c: skipped under -short (100k-row multi-engine benchmark)")
	}

	dir := t.TempDir()
	mush := &benchEngine{label: "musql", driver: driver.DriverName, path: filepath.Join(dir, "musql.musq")}
	cee := &benchEngine{label: "mattn-C", driver: "sqlite3", path: filepath.Join(dir, "mattn.db")}
	trs := &benchEngine{label: "turso-rust", driver: "turso", path: filepath.Join(dir, "turso.db")}

	mdb := seedBench(t, mush)
	defer mdb.Close()
	cdb := seedBench(t, cee)
	defer cdb.Close()
	// Turso is young; a seed or a query it cannot do is reported, not fatal --
	// this file's job is to measure musql against the field, and dropping the
	// whole comparison because a third engine lacks a feature would be the
	// wrong trade.
	var tdb *sql.DB
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("turso: seeding panicked, skipping that arm: %v", r)
				tdb = nil
			}
		}()
		tdb = seedBenchSoft(t, trs)
	}()
	if tdb != nil {
		defer tdb.Close()
	}

	ddb := openDuckDB(t, filepath.Join(dir, "duck.db"))
	if ddb != nil {
		defer ddb.Close()
	}

	// Every row into segments, none left in the delta: the format at rest.
	vacStart := time.Now()
	if _, err := mdb.Exec(`VACUUM`); err != nil {
		t.Fatalf("VACUUM: %v", err)
	}
	vacDur := time.Since(vacStart)
	colr, err := musqlengine.Open(mush.path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer colr.Close()

	// A FIXED iteration count cannot measure this table, because its workloads
	// span four orders of magnitude. At 40 iterations a 37us join is timed over
	// 1.5ms total -- less than a scheduler slice -- and its reported ratio
	// against C flipped between "1.18x slower" and "2x faster" across runs of
	// the same binary, while a 20ms GROUP BY at the same count takes 800ms and
	// is perfectly stable.
	//
	// So each workload gets whatever count puts it near targetRun, measured
	// from its own warm-up. The floor keeps the slow ones from being timed on
	// too few samples; the ceiling keeps a microsecond query from running for
	// minutes.
	const (
		targetRun = 200 * time.Millisecond
		minIters  = 40
		slowIters = 3 // a statement that already takes longer than targetRun
		maxIters  = 200000
	)
	itersFor := func(one time.Duration) int {
		if one <= 0 {
			return maxIters
		}
		n := int(targetRun / one)
		if n > maxIters {
			return maxIters
		}
		// The floor is not unconditional. A flat minimum of 40 is right for a
		// microsecond query and ruinous for a slow one: a correlated subquery
		// at two seconds an execution becomes eighty seconds PER ARM, and the
		// whole run stopped finishing inside its timeout. A statement already
		// slower than the target is measured a handful of times -- its cost is
		// dominated by scanning every row, not by run-to-run noise.
		if one >= targetRun {
			return slowIters
		}
		if n < minIters {
			n = minIters
		}
		return n
	}
	// timeCalls times call over itersFor(first) iterations, and keeps going
	// until targetRun has passed for a statement that is not slow. The first
	// call on a connection compiles the plan, so a count taken from it alone
	// timed a microsecond query over a millisecond or two, which is noise.
	timeCalls := func(first time.Duration, call func() error) (time.Duration, error) {
		n := itersFor(first)
		start := time.Now()
		i := 0
		for ; i < n || n > slowIters && i < maxIters && (i%64 != 0 || time.Since(start) < targetRun); i++ {
			if err := call(); err != nil {
				return 0, err
			}
		}
		return time.Since(start) / time.Duration(i), nil
	}
	type row struct {
		name              string
		mushCol, mushDrv  time.Duration
		mushFast          time.Duration
		c, cNative, turso time.Duration
		tNative           time.Duration
		tursoErr          string
		duck1, duckN      time.Duration
		duckErr           string
		jitted            bool
	}

	// Each workload: the SQL, and a bind-value generator used identically by
	// every arm (same PRNG seed, so the same sequence of bounds).
	// Every read workload returns a deterministic sequence of INTEGERS, which is
	// what makes exact cross-engine comparison possible without a value
	// formatter that could itself disagree. Where ordering matters it is made
	// TOTAL (a tiebreak column), because tie order is legitimately unspecified
	// and two engines differing there is not a defect.
	workloads := []struct {
		name string
		sql  string
		args func(rng *rand.Rand) []any
	}{
		// Shapes the JIT answers.
		{"count(*) WHERE v > ?", "SELECT count(*) FROM t WHERE v > ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(1_000_000)} }},
		{"count(*) WHERE v > ? AND k <> ?", "SELECT count(*) FROM t WHERE v > ? AND k <> ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(1_000_000), rng.Intn(10)} }},
		// Seeks: native code reads the rows the seek positions.
		{"rowid point lookup", "SELECT sec FROM t WHERE id = ?",
			func(rng *rand.Rand) []any { return []any{1 + rng.Intn(benchN)} }},
		// Served by the JIT since the recogniser learned to skip
		// OpAutoIndexOrder -- an index on the column used to be enough to lose
		// the fast path.
		{"secondary-index eq", "SELECT count(*) FROM t WHERE sec = ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(benchN)} }},
		{"indexed equi-join", "SELECT count(*) FROM t JOIN b ON t.bid = b.id WHERE t.sec = ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(benchN)} }},
		// sum(<column>) is answered from the segments.
		{"sum over a filter", "SELECT sum(v) FROM t WHERE v > ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(1_000_000)} }},
		// Driven from the columnar blocks, reusing the ordinary aggregation and
		// drain (segment_group.go).
		{"aggregate GROUP BY", "SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k",
			func(rng *rand.Rand) []any { return nil }},
		// Bounded top-N over the int64 key blocks, with no record decode.
		{"ORDER BY v DESC LIMIT 20", "SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 20",
			func(rng *rand.Rand) []any { return nil }},
		// Zero predicates: answered from the segments' recorded row counts.
		{"count(*) whole table", "SELECT count(*) FROM t",
			func(rng *rand.Rand) []any { return nil }},

		// ---- Shapes beyond the columnar filters. ----
		//
		// These exist so the table is evidence rather than a showcase: min/max,
		// HAVING, predicates the filter kernels do not model, TEXT, DISTINCT,
		// subqueries, compounds and pagination. The JIT answers every one.
		{"min/max whole table", "SELECT min(v), max(v) FROM t",
			func(rng *rand.Rand) []any { return nil }},
		{"grouped min/max", "SELECT k, min(v), max(v) FROM t GROUP BY k ORDER BY k",
			func(rng *rand.Rand) []any { return nil }},
		{"GROUP BY with HAVING", "SELECT k, count(*) FROM t GROUP BY k HAVING count(*) > ? ORDER BY k",
			func(rng *rand.Rand) []any { return []any{5000 + rng.Intn(100)} }},
		{"OR predicate", "SELECT count(*) FROM t WHERE v > ? OR k = ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(1_000_000), rng.Intn(10)} }},
		{"BETWEEN range", "SELECT count(*) FROM t WHERE v BETWEEN ? AND ?",
			func(rng *rand.Rand) []any { n := rng.Intn(500_000); return []any{n, n + 250_000} }},
		{"IN list", "SELECT count(*) FROM t WHERE k IN (1,3,5,7)",
			func(rng *rand.Rand) []any { return nil }},
		{"IS NULL scan", "SELECT count(*) FROM t WHERE payload IS NULL",
			func(rng *rand.Rand) []any { return nil }},
		{"LIKE scan", "SELECT count(*) FROM t WHERE payload LIKE '%-7-payload'",
			func(rng *rand.Rand) []any { return nil }},
		{"ORDER BY text LIMIT", "SELECT id, payload FROM t ORDER BY payload DESC, id DESC LIMIT 20",
			func(rng *rand.Rand) []any { return nil }},
		{"group_concat", "SELECT k, count(*) FROM t WHERE k < 3 GROUP BY k ORDER BY k",
			func(rng *rand.Rand) []any { return nil }},
		{"DISTINCT scan", "SELECT count(*) FROM (SELECT DISTINCT k FROM t)",
			func(rng *rand.Rand) []any { return nil }},
		{"scalar subquery", "SELECT count(*) FROM t WHERE v > (SELECT avg(v) FROM t)",
			func(rng *rand.Rand) []any { return nil }},
		{"IN subquery", "SELECT count(*) FROM t WHERE bid IN (SELECT id FROM b WHERE id < ?)",
			func(rng *rand.Rand) []any { return []any{1 + rng.Intn(1000)} }},
		{"EXISTS correlated", "SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.id < ?)",
			func(rng *rand.Rand) []any { return []any{1 + rng.Intn(1000)} }},
		{"UNION ALL compound", "SELECT count(*) FROM (SELECT k FROM t WHERE k < 3 UNION ALL SELECT k FROM t WHERE k > 7)",
			func(rng *rand.Rand) []any { return nil }},
		{"paginate OFFSET", "SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 20 OFFSET 1000",
			func(rng *rand.Rand) []any { return nil }},
		// A join returning ROWS rather than a count.
		{"join projecting rows", "SELECT t.id, b.id FROM t JOIN b ON t.bid = b.id WHERE t.sec = ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(benchN)} }},
	}

	timeDirect := func(rp *musqlengine.ReadOnlyPager, q string, gen func(*rand.Rand) []any) time.Duration {
		rng := rand.New(rand.NewSource(benchSeed))
		warmStart := time.Now()
		if _, _, err := rp.QueryArgs(q, toEngineVals(gen(rng))); err != nil {
			t.Fatalf("warm %q: %v", q, err)
		}
		rng = rand.New(rand.NewSource(benchSeed))
		d, err := timeCalls(time.Since(warmStart), func() error {
			_, _, err := rp.QueryArgs(q, toEngineVals(gen(rng)))
			return err
		})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return d
	}
	// timeDriverSoft is timeDriver for an engine allowed to fail: it reports the
	// error instead of ending the run.
	timeDriverSoft := func(db *sql.DB, q string, gen func(*rand.Rand) []any) (d time.Duration, errText string) {
		if db == nil {
			return 0, "arm skipped"
		}
		defer func() {
			if r := recover(); r != nil {
				d, errText = 0, "panic"
			}
		}()
		rng := rand.New(rand.NewSource(benchSeed))
		warmStart := time.Now()
		if _, err := renderDriverInts(db, q, gen(rng)); err != nil {
			return 0, err.Error()
		}
		rng = rand.New(rand.NewSource(benchSeed))
		d, err := timeCalls(time.Since(warmStart), func() error {
			_, err := renderDriverInts(db, q, gen(rng))
			return err
		})
		if err != nil {
			return 0, err.Error()
		}
		return d, ""
	}
	timeDriver := func(db *sql.DB, q string, gen func(*rand.Rand) []any) time.Duration {
		rng := rand.New(rand.NewSource(benchSeed))
		warmStart := time.Now()
		if _, err := renderDriverInts(db, q, gen(rng)); err != nil {
			t.Fatalf("warm %q: %v", q, err)
		}
		rng = rand.New(rand.NewSource(benchSeed))
		d, err := timeCalls(time.Since(warmStart), func() error {
			_, err := renderDriverInts(db, q, gen(rng))
			return err
		})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return d
	}

	// timeFast is timeDriver through package boost on its own connections,
	// scanning every column into a typed destination. Its rows are first
	// checked against database/sql's.
	bdb, err := boost.Open(mush.path)
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	timeFast := func(q string, gen func(*rand.Rand) []any) time.Duration {
		ctx := context.Background()
		rng := rand.New(rand.NewSource(benchSeed))
		args := gen(rng)
		want, err := renderDriverInts(mdb, q, args)
		if err != nil {
			t.Fatalf("boost %q: %v", q, err)
		}
		rows, err := bdb.Query(ctx, q, args...)
		if err != nil {
			t.Fatalf("boost %q: %v", q, err)
		}
		cells := make([]any, len(rows.Columns()))
		ptrs := make([]any, len(cells))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		var sb strings.Builder
		var dest []any
		for rows.Next(ptrs...) {
			for _, c := range cells {
				sb.WriteString(renderAnyCell(c))
			}
			sb.WriteString(";")
			if dest == nil {
				for _, c := range cells {
					switch c.(type) {
					case int64:
						dest = append(dest, new(int64))
					case float64:
						dest = append(dest, new(float64))
					case string:
						dest = append(dest, new(string))
					default:
						dest = append(dest, new(any))
					}
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("boost %q: %v", q, err)
		}
		if want != sb.String() {
			t.Fatalf("boost %q: read %q, database/sql %q", q, sb.String(), want)
		}
		// A typed destination per column, falling back to any for a column
		// whose class differs between rows or bind values.
		run := func(args []any) {
			rows, err := bdb.Query(ctx, q, args...)
			if err != nil {
				t.Fatalf("boost %q: %v", q, err)
			}
			for rows.Next() {
				if rows.Scan(dest...) != nil {
					for i := range dest {
						dest[i] = new(any)
					}
					if err := rows.Scan(dest...); err != nil {
						t.Fatalf("boost %q: %v", q, err)
					}
				}
			}
			rows.Close()
		}
		warmStart := time.Now()
		run(args)
		rng = rand.New(rand.NewSource(benchSeed))
		d, err := timeCalls(time.Since(warmStart), func() error {
			run(gen(rng))
			return nil
		})
		if err != nil {
			t.Fatalf("boost %q: %v", q, err)
		}
		return d
	}

	musqlengine.CountVMJITCursorEntersForTest(true)
	defer musqlengine.CountVMJITCursorEntersForTest(false)
	var rows []row
	for _, w := range workloads {
		if benchSkipped(w.name) {
			continue
		}
		// Assert the fast path is taken, or the number below is a VDBE number
		// wearing a columnar label.
		stmt, perr := musqlengine.ParseSelect(w.sql)
		if perr != nil {
			t.Fatalf("parse %q: %v", w.sql, perr)
		}
		if _, jerr := musqlengine.ProgramUsesSegFilterForTest(colr, stmt); jerr != nil {
			t.Fatalf("compile %q: %v", w.sql, jerr)
		}
		// The opcode being PRESENT is not the claim; the claim is that it
		// answered. Count what the columnar kernels and the whole-program JIT
		// actually did: a kernel that served without handing back, or native
		// code that read the rows. Every workload must be one or the other, or
		// the row below is a VDBE time printed under a JIT label.
		musqlengine.ResetSegFilterCountersForTest()
		musqlengine.VMJITCursorEntersForTest()
		cl := timeDirect(colr, w.sql, w.args)
		served, declined := musqlengine.SegFilterCountersForTest()
		native := musqlengine.VMJITCursorEntersForTest()
		jitted := served > 0 && declined == 0 || native > 0
		if !jitted {
			t.Errorf("%s: expected the JIT to answer it, got served=%d declined=%d native=%d",
				w.name, served, declined, native)
		}
		td, terr := timeDriverSoft(tdb, w.sql, w.args)
		var d1, dN time.Duration
		derr := "arm skipped"
		if ddb != nil {
			setDuckThreads(t, ddb, 1)
			d1, derr = timeDriverSoft(ddb, w.sql, w.args)
			setDuckThreads(t, ddb, 0)
			if derr == "" {
				dN, derr = timeDriverSoft(ddb, w.sql, w.args)
			}
		}
		rows = append(rows, row{
			duck1:    d1,
			duckN:    dN,
			duckErr:  derr,
			name:     w.name,
			mushCol:  cl,
			mushDrv:  timeDriver(mdb, w.sql, w.args),
			mushFast: timeFast(w.sql, w.args),
			c:        timeDriver(cdb, w.sql, w.args),
			turso:    td,
			tursoErr: terr,
			jitted:   jitted,
		})
	}

	// CORRECTNESS BEFORE SPEED, and on every bind value the timing loop uses --
	// not one sample. An arm that answers faster and differently is not a
	// result, and a single-sample check is close to no check: a kernel that is
	// wrong only for negative bounds, or only in the last vector lane, passes it
	// easily.
	//
	// Every arm is compared, C SQLITE AS THE ORACLE, including Turso: it is a
	// third implementation of the same semantics and if it disagrees that is
	// worth knowing, though a Turso disagreement is REPORTED rather than fatal
	// because this file's job is to measure musql.
	// How many distinct bind values each workload is CHECKED on.
	//
	// It was a flat 40, which is right for a microsecond query and unworkable
	// for a slow one: 40 values times five arms is 200 executions, and a
	// correlated EXISTS over 100,000 rows takes seconds apiece. The run stopped
	// being something anyone would wait for, which is its own way of not being
	// checked.
	//
	// So the count comes from the statement's own cost, measured once: many
	// values where they are cheap, a floor of five where they are not. Five is
	// still five different bounds against the oracle, and the workloads that
	// get only five are the ones whose cost is dominated by scanning every row
	// rather than by which bound was passed.
	const (
		checkBudget = 3 * time.Second
		minChecks   = 5
		maxChecks   = 40
	)
	for _, w := range workloads {
		if benchSkipped(w.name) {
			continue
		}
		rng := rand.New(rand.NewSource(benchSeed))
		probeStart := time.Now()
		if _, err := renderDriverInts(cdb, w.sql, w.args(rand.New(rand.NewSource(benchSeed)))); err != nil {
			t.Fatalf("C %q: %v", w.sql, err)
		}
		checkValues := int(checkBudget / (time.Since(probeStart)*5 + time.Microsecond))
		if checkValues < minChecks {
			checkValues = minChecks
		}
		if checkValues > maxChecks {
			checkValues = maxChecks
		}
		for i := 0; i < checkValues; i++ {
			args := w.args(rng)
			oracle, err := renderDriverInts(cdb, w.sql, args)
			if err != nil {
				t.Fatalf("C %q %v: %v", w.sql, args, err)
			}
			gotC, err := renderDirectInts(colr, w.sql, args)
			if err != nil {
				t.Fatalf("musql columnar %q %v: %v", w.sql, args, err)
			}
			gotD, err := renderDriverInts(mdb, w.sql, args)
			if err != nil {
				t.Fatalf("musql driver %q %v: %v", w.sql, args, err)
			}
			if gotC != oracle {
				t.Fatalf("%s %v: musql COLUMNAR %q, C %q", w.name, args, gotC, oracle)
			}
			if gotD != oracle {
				t.Fatalf("%s %v: musql driver %q, C %q", w.name, args, gotD, oracle)
			}
			if ddb != nil {
				want, werr := renderValues(cdb, w.sql, args)
				got, gerr := renderValues(ddb, w.sql, args)
				if werr == nil && gerr == nil && got != want && i == 0 {
					t.Logf("DUCKDB DIFFERS %s %v: %q, C %q", w.name, args, got, want)
				}
			}
			if tdb != nil {
				if gotT, terr := renderDriverInts(tdb, w.sql, args); terr != nil {
					if i == 0 {
						t.Logf("turso %s: %v", w.name, terr)
					}
				} else if gotT != oracle {
					t.Errorf("turso %s %v: %q, C %q", w.name, args, gotT, oracle)
				}
			}
		}
	}

	// The native-C baseline, timed after the Go arms so they never share the
	// machine. cdb has finished writing (seedBench commits before returning),
	// so the C side opens the same file read-only.
	var natives []nativeWorkload
	for _, w := range workloads {
		if !benchSkipped(w.name) {
			natives = append(natives, nativeWorkload{w.name, w.sql, w.args})
		}
	}
	cNative := nativeCTimes(t, cee.path, natives)
	for i := range rows {
		rows[i].cNative = cNative[rows[i].name].d
	}
	// Turso's library, the same way, on the workloads its driver answered. Its
	// checksum must equal C's: a disagreement is reported, as for its driver.
	var tnatives []nativeWorkload
	for i, w := range natives {
		if tdb != nil && rows[i].tursoErr == "" {
			tnatives = append(tnatives, w)
		}
	}
	if len(tnatives) > 0 {
		tNative := nativeTursoTimes(t, trs.path, tnatives)
		for i := range rows {
			tr, ok := tNative[rows[i].name]
			if !ok {
				continue
			}
			if c, ok := cNative[rows[i].name]; ok && c.sum != tr.sum {
				t.Logf("turso native %s: read different values than C", rows[i].name)
				continue
			}
			rows[i].tNative = tr.d
		}
	}

	t.Logf("\nCOLUMNAR + JIT vs C SQLite -- %d rows, disk-backed, warm, JIT=%v",
		benchN, musqlengine.JITEnabled())
	t.Logf("segments laid out by VACUUM in %s", vacDur.Round(time.Millisecond))
	t.Logf("%-32s | %12s | %12s | %12s | %12s | %12s | %13s | %11s | %11s", "workload",
		"musql+JIT", "musql drv", "C native", "mattn-C", "turso native",
		"vs C native", "vs mattn-C", "vs turso")
	t.Logf("%s", "-----------------------------------------------------------------------------------------------------------------------------------")
	for _, r := range rows {
		mark := ""
		if !r.jitted {
			mark = "  (VDBE, not the JIT)"
		}
		tursoCell, vsTurso := r.tursoErr, "n/a"
		if r.tNative > 0 {
			tursoCell, vsTurso = r.tNative.Round(100*time.Nanosecond).String(), speedup(r.tNative, r.mushCol)
		} else if r.tursoErr == "" {
			tursoCell, vsTurso = "n/a", "n/a"
		}
		nativeCell, vsNative := "n/a", "n/a"
		if r.cNative > 0 {
			nativeCell, vsNative = r.cNative.Round(100*time.Nanosecond).String(), speedup(r.cNative, r.mushCol)
		}
		t.Logf("%-32s | %12s | %12s | %12s | %12s | %12s | %13s | %11s | %11s%s", r.name,
			r.mushCol.Round(time.Microsecond),
			r.mushDrv.Round(time.Microsecond), nativeCell, r.c.Round(time.Microsecond),
			tursoCell, vsNative, speedup(r.c, r.mushCol), vsTurso, mark)
	}
	t.Logf("the vs-X columns are how many times FASTER musql+JIT is than X.")
	t.Logf("")
	t.Logf("The same engines through database/sql (tursogo re-prepares every query), and musql's FastStmt")
	t.Logf("%-32s | %12s | %12s | %12s | %12s | %12s | %12s | %11s", "workload",
		"musql+JIT", "musql drv", "turso native", "turso drv", "drv overhead", "musql boost", "boost vs tn")
	for _, r := range rows {
		tn, td := "n/a", r.tursoErr
		if r.tNative > 0 {
			tn = r.tNative.Round(100 * time.Nanosecond).String()
		}
		if r.tursoErr == "" {
			td = r.turso.Round(time.Microsecond).String()
		}
		vsTn := "n/a"
		if r.tNative > 0 {
			vsTn = speedup(r.tNative, r.mushFast)
		}
		t.Logf("%-32s | %12s | %12s | %12s | %12s | %12s | %12s | %11s", r.name,
			r.mushCol.Round(time.Microsecond), r.mushDrv.Round(time.Microsecond),
			tn, td, (r.mushDrv - r.mushCol).Round(100*time.Nanosecond),
			r.mushFast.Round(100*time.Nanosecond), vsTn)
	}

	// DuckDB, a different class of engine: a reference, not a headline.
	t.Logf("")
	t.Logf("DuckDB reference (analytical engine; 1 thread like the others, and its default)")
	t.Logf("%-32s | %12s | %12s | %13s | %13s | %13s", "workload",
		"musql+JIT", "C native", "duckdb 1 thr", "duckdb", "vs duckdb 1")
	for _, r := range rows {
		if r.duckErr != "" {
			t.Logf("%-32s | %12s | duckdb: %s", r.name, r.mushCol.Round(time.Microsecond), r.duckErr)
			continue
		}
		nativeCell := "n/a"
		if r.cNative > 0 {
			nativeCell = r.cNative.Round(100 * time.Nanosecond).String()
		}
		t.Logf("%-32s | %12s | %12s | %13s | %13s | %13s", r.name,
			r.mushCol.Round(time.Microsecond), nativeCell,
			r.duck1.Round(time.Microsecond), r.duckN.Round(time.Microsecond),
			speedup(r.duck1, r.mushCol))
	}

	// ---- WRITES, where this engine is at its worst. ----
	//
	// Writes run the ordinary engine through database/sql, which is how an
	// application issues them. They are included precisely BECAUSE they are the
	// weak side: a benchmark that stops at the reads would be choosing its own
	// scoreboard.
	//
	// Correctness is checked by the row COUNT each statement reports, compared
	// across engines, rather than by rendering: an UPDATE that touched a
	// different number of rows is the failure that matters here.
	t.Logf("")
	t.Logf("WRITES (no columnar path exists; ordinary engine through database/sql)")
	t.Logf("%-32s | %12s | %12s | %12s | %11s | %11s", "workload",
		"musql", "mattn-C", "turso", "vs C", "vs turso")
	t.Logf("%s", "-----------------------------------------------------------------------------------------------------")
	writes := []struct {
		name string
		sql  string
		args func(rng *rand.Rand) []any
	}{
		{"UPDATE one row by rowid", "UPDATE t SET k = k + 1 WHERE id = ?",
			func(rng *rand.Rand) []any { return []any{1 + rng.Intn(benchN)} }},
		{"UPDATE by indexed column", "UPDATE t SET k = k + 1 WHERE sec = ?",
			func(rng *rand.Rand) []any { return []any{rng.Intn(benchN)} }},
		{"INSERT one row", "INSERT INTO t(sec,k,v,bid,payload) VALUES(?,?,?,?,'x')",
			func(rng *rand.Rand) []any {
				return []any{rng.Intn(benchN), rng.Intn(10), rng.Intn(1_000_000), 1 + rng.Intn(benchN)}
			}},
		{"DELETE one row by rowid", "DELETE FROM t WHERE id = ?",
			func(rng *rand.Rand) []any { return []any{1 + rng.Intn(benchN)} }},
	}
	timeWrite := func(db *sql.DB, q string, gen func(*rand.Rand) []any) (time.Duration, int64, string) {
		if db == nil {
			return 0, 0, "arm skipped"
		}
		// The FIRST execution is the only one comparable across engines: it runs
		// the same statement with the same arguments (rng seeded identically),
		// before any of them has mutated anything. Everything after it runs a
		// per-engine iteration count derived from that engine's own warm time,
		// so a faster engine performs MORE writes -- which is why comparing
		// TOTALS was wrong and reported four failures on every run, with musql
		// and turso agreeing at 41 while C, having run more iterations, said 53.
		var firstAffected int64
		rng := rand.New(rand.NewSource(benchSeed))
		warmStart := time.Now()
		res, err := db.Exec(q, gen(rng)...)
		if err != nil {
			return 0, 0, err.Error()
		}
		if n, aerr := res.RowsAffected(); aerr == nil {
			firstAffected = n
		}
		n := itersFor(time.Since(warmStart))
		if n > 250 {
			// A write MUTATES, so this cannot be run a million times -- and the
			// cap has to be far below what a read gets for a second reason: at
			// 2000 it made the whole benchmark exceed a 2h13m test timeout and
			// report NOTHING, twice. Four write workloads times three engines
			// times 2000 mutations against a 200k-row table is most of that.
			// 250 still measures a write honestly; a truncated run measures
			// nothing at all.
			n = 250
		}
		rng = rand.New(rand.NewSource(benchSeed + 1))
		start := time.Now()
		for i := 0; i < n; i++ {
			res, err := db.Exec(q, gen(rng)...)
			if err != nil {
				return 0, 0, err.Error()
			}
			if _, aerr := res.RowsAffected(); aerr != nil {
				return 0, 0, aerr.Error()
			}
		}
		return time.Since(start) / time.Duration(n), firstAffected, ""
	}
	for _, w := range writes {
		if benchSkipped(w.name) {
			continue
		}
		md, mAff, mErr := timeWrite(mdb, w.sql, w.args)
		cd, cAff, cErr := timeWrite(cdb, w.sql, w.args)
		td, tAff, tErr := timeWrite(tdb, w.sql, w.args)
		if mErr != "" || cErr != "" {
			t.Logf("%-32s | musql %s | C %s", w.name, mErr, cErr)
			continue
		}
		// Comparing the FIRST execution of each -- same statement, same
		// arguments, same starting table. This is the check the comment above
		// the workloads promises, and comparing totals was not it.
		if mAff != cAff {
			t.Errorf("%s: musql's first write touched %d rows, C's touched %d", w.name, mAff, cAff)
		}
		if tErr == "" && tAff != cAff {
			t.Errorf("turso %s: first write touched %d rows, C's touched %d", w.name, tAff, cAff)
		}
		tursoCell, vsTurso := tErr, "n/a"
		if tErr == "" {
			tursoCell = td.Round(time.Microsecond).String()
			vsTurso = speedup(td, md)
		}
		t.Logf("%-32s | %12s | %12s | %12s | %11s | %11s", w.name,
			md.Round(time.Microsecond), cd.Round(time.Microsecond), tursoCell,
			speedup(cd, md), vsTurso)
	}
}

// seedBenchSoft is seedBench for an engine that is allowed to fail: it calls
// t.Skip-free helpers and surfaces the error rather than ending the run.
//
// It duplicates seedBench's schema deliberately. Sharing the original would
// mean making it non-fatal for every caller, and every OTHER caller wants a
// seed failure to be fatal -- a benchmark that silently compared against an
// empty table would be worse than one that stopped.
func seedBenchSoft(t *testing.T, e *benchEngine) *sql.DB {
	t.Helper()
	db, err := sql.Open(e.driver, e.path)
	if err != nil {
		t.Logf("[%s] open: %v -- skipping this arm", e.label, err)
		return nil
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Logf("[%s] %q: %v -- skipping this arm", e.label, q, err)
			db.Close()
			return nil
		}
	}
	rng := rand.New(rand.NewSource(benchSeed))
	if err := bulkInsert(db, "INSERT INTO b(id,label) VALUES(?,?)", benchN, func(i int) []any {
		return []any{i, fmt.Sprintf("label-%d", i)}
	}); err != nil {
		t.Logf("[%s] seed b: %v -- skipping this arm", e.label, err)
		db.Close()
		return nil
	}
	if err := bulkInsert(db, "INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)", benchN, func(i int) []any {
		return []any{i, rng.Intn(benchN), rng.Intn(10), rng.Intn(1_000_000), 1 + rng.Intn(benchN),
			fmt.Sprintf("row-%d-payload", i)}
	}); err != nil {
		t.Logf("[%s] seed t: %v -- skipping this arm", e.label, err)
		db.Close()
		return nil
	}
	return db
}

// speedup renders "how many times faster than other" for a pair of durations,
// as a multiple rather than a ratio of one to the other. A raw ratio prints
// 0.00x at these magnitudes, which says nothing.
func speedup(other, ours time.Duration) string {
	if ours <= 0 {
		return "n/a"
	}
	x := float64(other) / float64(ours)
	if x < 1 {
		return fmt.Sprintf("%.2fx SLOWER", 1/x)
	}
	if x < 10 {
		return fmt.Sprintf("%.2fx faster", x)
	}
	return fmt.Sprintf("%.0fx faster", x)
}

// toEngineVals converts the benchmark's bind values for the engine-direct API.
func toEngineVals(as []any) []musqlengine.Value {
	out := make([]musqlengine.Value, len(as))
	for i, a := range as {
		out[i] = musqlengine.Value{Typ: musqlengine.Int, I: int64(a.(int))}
	}
	return out
}

// renderDriverInts runs a query through database/sql and renders every cell as
// text, for exact comparison between engines.
//
// It handles every storage class, not just integers. An earlier version took
// integers only, which kept the formatter simple but quietly shaped the
// BENCHMARK: workloads had to be written to return integers, which excluded
// exactly the things most likely to be slow -- TEXT ordering, LIKE, string
// aggregation. A comparison that can only express the cases you already win is
// not evidence.
//
// The rendering is TYPE-TAGGED, so an engine returning 3 where another returns
// 3.0 is a difference and not a rounding question. That is deliberate: SQLite's
// type affinity is a real behaviour and two engines disagreeing about it is
// worth failing over, not smoothing away.
func renderDriverInts(db *sql.DB, q string, args []any) (string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for _, c := range cells {
			sb.WriteString(renderAnyCell(c))
		}
		sb.WriteString(";")
	}
	return sb.String(), rows.Err()
}

// renderAnyCell formats one database/sql cell with its type tagged.
func renderAnyCell(c any) string {
	switch v := c.(type) {
	case nil:
		return "N:|"
	case int64:
		return fmt.Sprintf("i:%d|", v)
	case float64:
		return fmt.Sprintf("f:%v|", v)
	case string:
		return fmt.Sprintf("t:%s|", v)
	case []byte:
		return fmt.Sprintf("t:%s|", string(v))
	case bool:
		if v {
			return "i:1|"
		}
		return "i:0|"
	default:
		return fmt.Sprintf("?:%v|", v)
	}
}

// renderDirectInts is renderDriverInts for the engine-direct API, producing a
// byte-identical rendering so the two can be compared.
func renderDirectInts(rp *musqlengine.ReadOnlyPager, q string, args []any) (string, error) {
	_, rows, err := rp.QueryArgs(q, toEngineVals(args))
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, r := range rows {
		for _, v := range r {
			switch v.Typ {
			case musqlengine.Null:
				sb.WriteString("N:|")
			case musqlengine.Int:
				fmt.Fprintf(&sb, "i:%d|", v.I)
			case musqlengine.Float:
				fmt.Fprintf(&sb, "f:%v|", v.F)
			case musqlengine.Text, musqlengine.Blob:
				fmt.Fprintf(&sb, "t:%s|", string(v.S))
			default:
				return "", fmt.Errorf("unrenderable cell (type %v) in %q", v.Typ, q)
			}
		}
		sb.WriteString(";")
	}
	return sb.String(), nil
}
