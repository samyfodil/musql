package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// The tests of the JIT's pointer stores: a copy of a TEXT or BLOB register
// (OP_SCopy), a column read of one, an integer or a NULL stored over one, and
// the row an OP_ResultRow copies out. Native code makes those stores itself
// while the GC's write barrier is off and exits while it is on (internal/jit's
// wb.go), so each test runs three ways -- the VDBE alone; the JIT with the
// collector held off, which keeps the barrier off and every store native; the
// JIT with the barrier forced on, which sends every pointer store back to the
// VDBE -- and a fourth, the JIT under a collector that never stops, checks
// what a missed barrier would break: a value freed while a register still
// held it.

// jitPointerModes runs f once per JIT mode, naming each.
func jitPointerModes(t *testing.T, f func(t *testing.T, mode string)) {
	t.Run("barrier off", func(t *testing.T) {
		old := debug.SetGCPercent(-1)
		runtime.GC()
		defer debug.SetGCPercent(old)
		f(t, "barrier off")
	})
	t.Run("barrier on", func(t *testing.T) {
		defer jit.ForceWriteBarrierOnForTest()()
		f(t, "barrier on")
	})
}

// TestVMJITPointerDifferential: random programs of copies, constants, NULLs,
// arithmetic and comparisons over registers holding TEXT, BLOB, NULL, REAL and
// INTEGER values, with result rows in the middle and at the end.
func TestVMJITPointerDifferential(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	const nReg = 8
	texts := []string{"", "a", "abc", "12", "-3", strings.Repeat("x", 100)}
	blobs := [][]byte{{}, {0}, {1, 2, 3}, []byte("blob")}
	progs := make([]*Program, 0, 3000)
	rng := rand.New(rand.NewSource(11))
	for range cap(progs) {
		var insns []Instruction
		add := func(op OpCode, p1, p2, p3 int, p4 any, p5 uint16) {
			insns = append(insns, Instruction{Op: op, P1: p1, P2: p2, P3: p3, P4: p4, P5: p5})
		}
		r := func() int { return rng.Intn(nReg) }
		value := func(reg int) {
			switch rng.Intn(6) {
			case 0:
				add(OpNull, 0, reg, 0, nil, 0)
			case 1:
				add(OpReal, 0, reg, 0, []float64{0.5, -2}[rng.Intn(2)], 0)
			case 2, 3:
				add(OpString8, 0, reg, 0, texts[rng.Intn(len(texts))], 0)
			case 4:
				add(OpBlob, 0, reg, 0, blobs[rng.Intn(len(blobs))], 0)
			default:
				add(OpInteger, rng.Intn(9)-4, reg, 0, nil, 0)
			}
		}
		for reg := range nReg {
			value(reg)
		}
		const body = 30
		start := len(insns)
		end := start + body
		for i := range body {
			pc := start + i
			fwd := pc + 1 + rng.Intn(end-pc)
			switch rng.Intn(12) {
			case 0, 1, 2, 3:
				add(OpSCopy, r(), r(), 0, nil, 0)
			case 4:
				add(OpCopy, r(), r(), 0, nil, 0)
			case 5:
				add(OpInteger, rng.Intn(5), r(), 0, nil, 0)
			case 6:
				add(OpNull, 0, r(), 0, nil, 0)
			case 7:
				add(OpAdd, r(), r(), r(), nil, 0)
			case 8:
				p := r()
				add(OpResultRow, p, rng.Intn(nReg-p+1), 0, nil, 0)
			case 9:
				add([]OpCode{OpEq, OpLt, OpGe}[rng.Intn(3)], r(), r(), r(), nil, p5StoreP2)
			case 10:
				add([]OpCode{OpIf, OpIfNot}[rng.Intn(2)], r(), fwd, rng.Intn(2), nil, 0)
			default:
				value(r())
			}
		}
		add(OpResultRow, 0, nReg, 0, nil, 0)
		add(OpHalt, 0, 0, 0, nil, 0)
		progs = append(progs, &Program{Insns: insns, NReg: nReg, NResultCol: nReg})
	}
	nativeRows := 0
	run := func(prog *Program, withJIT bool) ([][]Value, error) {
		st, err := NewProgramStmt(prog)
		if err != nil {
			t.Fatal(err)
		}
		if !withJIT {
			st.m.jx = nil
		}
		defer func() { nativeRows += st.m.jitRows }()
		var rows [][]Value
		for {
			row, err := st.Step()
			if err != nil || row == nil {
				return rows, err
			}
			rows = append(rows, append([]Value(nil), row...))
		}
	}
	want := make([]string, len(progs))
	for i, p := range progs {
		rows, err := run(p, false)
		want[i] = fmt.Sprint(rows, err)
	}
	jitPointerModes(t, func(t *testing.T, mode string) {
		native := 0
		nativeRows = 0
		for i, p := range progs {
			p.vmJIT = nil // compiled under this mode's flag
			if p.jitCode() == nil {
				continue
			}
			native++
			rows, err := run(p, true)
			if got := fmt.Sprint(rows, err); got != want[i] {
				t.Fatalf("%s: program %d: JIT diverges\nprogram %v\nvdbe %s\njit  %s", mode, i, p.Insns, want[i], got)
			}
		}
		if native < len(progs)/2 {
			t.Fatalf("%s: only %d of %d programs had native code", mode, native, len(progs))
		}
		// With the barrier off native code copies rows out; with it on, never.
		if (mode == "barrier off") != (nativeRows > 0) {
			t.Fatalf("%s: native code copied %d result rows", mode, nativeRows)
		}
		t.Logf("%s: %d programs agree, %d rows copied natively", mode, native, nativeRows)
	})
}

// jitTextSetup is tables of text, blob, NULL and numeric columns, wide enough
// for scans that return many rows.
func jitTextSetup(rng *rand.Rand, rows int) []string {
	val := func() string {
		switch rng.Intn(8) {
		case 0:
			return "NULL"
		case 1:
			return fmt.Sprint(rng.Intn(5))
		case 2:
			return fmt.Sprintf("x'%02x%02x'", rng.Intn(256), rng.Intn(4))
		case 3:
			return fmt.Sprintf("%g", float64(rng.Intn(9))/2)
		default:
			return fmt.Sprintf("'%s%d'", []string{"k", "key", "a longer text value "}[rng.Intn(3)], rng.Intn(6))
		}
	}
	var setup []string
	for _, tbl := range []string{"a", "b"} {
		setup = append(setup, fmt.Sprintf("CREATE TABLE %s(id INTEGER PRIMARY KEY, s TEXT, t, u BLOB, n)", tbl))
		for i := 1; i <= rows; i++ {
			setup = append(setup, fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, %s, %s, %s)", tbl, i, val(), val(), val(), val()))
		}
	}
	return setup
}

// jitTextQueries is text joins, projections of text and blob columns, scans
// that return every row, subqueries and derived tables.
func jitTextQueries(rng *rand.Rand, n int) []string {
	col := func(t string) string { return t + "." + []string{"s", "t", "u", "n", "id"}[rng.Intn(5)] }
	var qs []string
	for range n {
		switch rng.Intn(8) {
		case 0:
			qs = append(qs, "SELECT * FROM a")
		case 1:
			qs = append(qs, fmt.Sprintf("SELECT %s, %s, %s FROM a", col("a"), col("a"), col("a")))
		case 2:
			qs = append(qs, fmt.Sprintf("SELECT a.id, b.id, %s, %s FROM a JOIN b ON a.s = b.s ORDER BY 1, 2", col("a"), col("b")))
		case 3:
			qs = append(qs, fmt.Sprintf("SELECT a.s, b.t FROM a, b WHERE %s = %s ORDER BY a.id, b.id", col("a"), col("b")))
		case 4:
			qs = append(qs, fmt.Sprintf("SELECT d.s, d.u FROM (SELECT s, u, id FROM a WHERE id > %d) d ORDER BY d.id", rng.Intn(10)))
		case 5:
			qs = append(qs, fmt.Sprintf("SELECT id, (SELECT b.s FROM b WHERE b.t = a.t ORDER BY b.id LIMIT 1) FROM a WHERE %s IS NOT NULL", col("a")))
		case 6:
			qs = append(qs, fmt.Sprintf("SELECT %s, count(*) FROM a GROUP BY 1 ORDER BY 1", col("a")))
		default:
			qs = append(qs, fmt.Sprintf("SELECT a.u, b.u, a.s || b.s FROM a LEFT JOIN b ON a.t = b.t ORDER BY a.id, b.id LIMIT %d", 5+rng.Intn(50)))
		}
	}
	return qs
}

// jitTextDML is UPDATEs and DELETEs whose WHERE scans read text columns, and
// whose SETs write them: write-path cursors, walked by OP_Next.
func jitTextDML(rng *rand.Rand, n int) []string {
	var qs []string
	for range n {
		switch rng.Intn(5) {
		case 0:
			qs = append(qs, fmt.Sprintf("UPDATE a SET s = t || '%d' WHERE s > 'key%d'", rng.Intn(9), rng.Intn(6)))
		case 1:
			qs = append(qs, fmt.Sprintf("UPDATE b SET u = s, n = n + 1 WHERE t IS NOT s AND id %% %d = 0", 1+rng.Intn(4)))
		case 2:
			qs = append(qs, fmt.Sprintf("DELETE FROM b WHERE s = 'k%d' OR u IS NULL AND id > %d", rng.Intn(6), rng.Intn(60)))
		case 3:
			qs = append(qs, "UPDATE a SET t = (SELECT b.s FROM b WHERE b.id = a.id) WHERE id IN (SELECT id FROM b WHERE b.t > a.s)")
		default:
			qs = append(qs, fmt.Sprintf("INSERT INTO b(s, t, u, n) SELECT t, s, u, id FROM a WHERE id < %d", rng.Intn(20)))
		}
	}
	return qs
}

// runTextWorkload runs setup, then each of queries and dml interleaved, and
// returns every answer and the tables' final contents.
func runTextWorkload(t *testing.T, dir string, setup, queries, dml []string) []string {
	t.Helper()
	db, err := Create(filepath.Join(dir, fmt.Sprintf("t%d.musq", rand.Int63())))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var out []string
	query := func(q string) {
		p, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		cols, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			out = append(out, q+": error: "+err.Error())
			return
		}
		out = append(out, fmt.Sprint(q, cols, rows))
	}
	for i, q := range queries {
		query(q)
		if i < len(dml) {
			if err := db.Exec(dml[i]); err != nil {
				out = append(out, dml[i]+": error: "+err.Error())
			}
		}
	}
	query("SELECT * FROM a ORDER BY id")
	query("SELECT * FROM b ORDER BY id")
	return out
}

// TestVMJITTextDifferential: the text workload with the JIT and without, in
// both barrier modes. Every answer and the final tables must match.
func TestVMJITTextDifferential(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(5))
	setup := jitTextSetup(rng, 60)
	queries := jitTextQueries(rng, 300)
	dml := jitTextDML(rng, 120)
	dir := t.TempDir()
	withJIT := func(on bool) []string {
		old := vmJITEnabled
		vmJITEnabled = on
		defer func() { vmJITEnabled = old }()
		return runTextWorkload(t, dir, setup, queries, dml)
	}
	want := withJIT(false)
	jitPointerModes(t, func(t *testing.T, mode string) {
		before, beforeW := vmJITCursorSyncs.Load(), vmJITWriteCursorSyncs.Load()
		got := withJIT(true)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: JIT diverges\nvdbe %s\njit  %s", mode, want[i], got[i])
			}
		}
		syncs, writes := vmJITCursorSyncs.Load()-before, vmJITWriteCursorSyncs.Load()-beforeW
		if syncs == 0 || writes == 0 {
			t.Fatalf("%s: %d entries advanced a cursor natively, %d a write-path one: the test checks too little", mode, syncs, writes)
		}
		t.Logf("%s: %d answers agree; %d entries advanced cursors natively, %d of them write-path", mode, len(want), syncs, writes)
	})
}

// TestVMJITGCStress runs the text workload with the JIT while the collector
// runs without pause beside it -- runtime.GC in a loop, and GOGC churned
// between 1 and 400 -- so that collections start and end at every point of
// the native code's life, and a pointer store that skipped a barrier it needed
// would let a value be freed and its memory reused under a register still
// holding it. Every answer must match the VDBE's, every round. Run it under
// GODEBUG=gccheckmark=1 too, which re-checks every mark the concurrent phase
// made.
func TestVMJITGCStress(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	if runtime.GOOS == "js" {
		// One thread: a collector looping beside the workload is the
		// workload's own thread, and the run takes the better part of an hour
		// to prove what one thread already guarantees -- no collection starts
		// while a kernel runs. The barrier-on path is the forced-flag tests'.
		t.Skip("js/wasm has one thread; nothing runs beside a kernel")
	}
	rounds := 6
	if testing.Short() {
		rounds = 2
	}
	rng := rand.New(rand.NewSource(9))
	setup := jitTextSetup(rng, 80)
	queries := jitTextQueries(rng, 200)
	dml := jitTextDML(rng, 80)
	dir := t.TempDir()
	old := vmJITEnabled
	vmJITEnabled = false
	want := runTextWorkload(t, dir, setup, queries, dml)
	vmJITEnabled = old

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			runtime.GC()
		}
	}()
	go func() {
		defer wg.Done()
		pct := []int{1, 400, 1, 50, 1, 10}
		for i := 0; !stop.Load(); i++ {
			debug.SetGCPercent(pct[i%len(pct)])
			runtime.Gosched()
		}
	}()
	defer func() {
		stop.Store(true)
		wg.Wait()
		debug.SetGCPercent(100)
	}()
	for round := range rounds {
		got := runTextWorkload(t, dir, setup, queries, dml)
		if !reflect.DeepEqual(got, want) {
			for i := range want {
				if i >= len(got) || got[i] != want[i] {
					g := "<missing>"
					if i < len(got) {
						g = got[i]
					}
					t.Fatalf("round %d: JIT under GC diverges\nvdbe %s\njit  %s", round, want[i], g)
				}
			}
			t.Fatalf("round %d: %d answers, want %d", round, len(got), len(want))
		}
	}
}
