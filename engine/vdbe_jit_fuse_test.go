package engine

import (
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// runModes runs a copy of insns three ways -- the VDBE alone, the JIT with
// fusion and the JIT without -- and fails on any difference in rows or error.
// It reports whether the JIT had native code for it.
func runModes(t *testing.T, name string, insns []Instruction, nReg int) bool {
	t.Helper()
	type result struct {
		rows [][]Value
		err  error
	}
	run := func(withJIT, fused bool) (result, bool) {
		jit.SetFusion(fused)
		defer jit.SetFusion(true)
		prog := &Program{Insns: append([]Instruction(nil), insns...), NReg: nReg, NResultCol: nReg}
		st, err := NewProgramStmt(prog)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		native := st.m.jx != nil
		if !withJIT {
			st.m.jx = nil
		}
		var r result
		for {
			row, err := st.Step()
			if err != nil || row == nil {
				r.err = err
				return r, native
			}
			r.rows = append(r.rows, append([]Value(nil), row...))
		}
	}
	want, _ := run(false, true)
	fused, native := run(true, true)
	unfused, _ := run(true, false)
	same := func(a, b result) bool {
		return (a.err == nil) == (b.err == nil) && (a.err == nil || a.err.Error() == b.err.Error()) && reflect.DeepEqual(a.rows, b.rows)
	}
	if !same(want, fused) || !same(want, unfused) {
		t.Fatalf("%s: JIT diverges\nprogram %v\nvdbe    %v %v\nfused   %v %v\nunfused %v %v", name, insns,
			want.rows, want.err, fused.rows, fused.err, unfused.rows, unfused.err)
	}
	return native
}

// TestVMJITFusionDifferential: random programs shaped like what the VDBE
// compiler emits for conditions -- a comparison stored into a register and
// tested right after, chains of them (AND/OR), NOT and its test, a register
// read again and again within a straight run, division and remainder -- with
// operands that fail every guard, inside a counted loop so values are carried
// round a backward jump. Each runs on the VDBE, fused and unfused; all three
// must agree. It also checks the fusion pass did fuse, and found dead stores.
func TestVMJITFusionDifferential(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	jit.VMFusionStatsForTest()
	const nReg = 10 // r8 is the loop counter, r9 its limit
	ints := []int64{0, 1, -1, 2, 3, 7, 63, 64, -64, 100, -100, math.MaxInt64, math.MinInt64, 1 << 40}
	rng := rand.New(rand.NewSource(11))
	native := 0
	const iters = 3000
	for iter := range iters {
		var insns []Instruction
		add := func(op OpCode, p1, p2, p3 int, p4 any, p5 uint16) {
			insns = append(insns, Instruction{Op: op, P1: p1, P2: p2, P3: p3, P4: p4, P5: p5})
		}
		r := func() int { return rng.Intn(8) }
		for reg := range 8 {
			switch rng.Intn(10) {
			case 0:
				add(OpNull, 0, reg, 0, nil, 0)
			case 1:
				add(OpReal, 0, reg, 0, 2.5, 0)
			case 2:
				add(OpString8, 0, reg, 0, []string{"3", "x"}[rng.Intn(2)], 0)
			default:
				add(OpInt64, 0, reg, 0, ints[rng.Intn(len(ints))], 0)
			}
		}
		add(OpInteger, 0, 8, 0, nil, 0)
		add(OpInteger, 1+rng.Intn(4), 9, 0, nil, 0)
		head := len(insns)
		body := 12 + rng.Intn(14)
		end := head + body // the loop's increment
		// One program in two jumps only to the loop's end, so its body is one
		// long straight run and what the emitter keeps between steps is used.
		straight := rng.Intn(2) == 0
		for len(insns) < end {
			pc := len(insns)
			fwd := func() int { // past what this step emits
				if straight {
					return end
				}
				return pc + 2 + rng.Intn(end-pc)
			}
			switch k := rng.Intn(12); {
			case k < 4 && pc+2 <= end:
				// A stored comparison and its test: one pair, or the head of a chain.
				d := r()
				add([]OpCode{OpEq, OpNe, OpLt, OpLe, OpGt, OpGe}[rng.Intn(6)], r(), d, r(), nil, p5StoreP2)
				add([]OpCode{OpIf, OpIfNot}[rng.Intn(2)], d, min(fwd(), end), rng.Intn(2), nil, 0)
			case k == 4 && pc+2 <= end:
				d := r()
				add(OpNot, r(), d, 0, nil, 0)
				add([]OpCode{OpIf, OpIfNot}[rng.Intn(2)], d, min(fwd(), end), rng.Intn(2), nil, 0)
			case k == 5:
				add([]OpCode{OpDivide, OpRemainder}[rng.Intn(2)], r(), r(), r(), nil, 0)
			case k == 6:
				add([]OpCode{OpAdd, OpSubtract, OpMultiply}[rng.Intn(3)], r(), r(), r(), nil, 0)
			case k == 7:
				add([]OpCode{OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight}[rng.Intn(4)], r(), r(), r(), nil, 0)
			case k == 8:
				add([]OpCode{OpSCopy, OpCopy}[rng.Intn(2)], r(), r(), 0, nil, 0)
			case k == 9:
				add(OpInteger, int(ints[rng.Intn(6)]), r(), 0, nil, 0)
			case k == 10:
				t := min(pc+1+rng.Intn(end-pc), end)
				if straight {
					t = end
				}
				add([]OpCode{OpIsNull, OpNotNull}[rng.Intn(2)], r(), t, 0, nil, 0)
			default:
				add(OpNegative, r(), r(), 0, nil, 0)
			}
		}
		// The loop: r8++, round again while r8 < r9; then the row.
		add(OpInteger, 1, 10, 0, nil, 0)
		add(OpAdd, 10, 8, 8, nil, 0)
		add(OpLt, 9, head, 8, nil, 0)
		add(OpResultRow, 0, 9, 0, nil, 0)
		add(OpHalt, 0, 0, 0, nil, 0)
		// Every other program with entries only where a long native run
		// follows: sparse entries leave long runs for the emitter to keep
		// what it knows through, dense ones put an entry stub at most pcs.
		vmJITMinWork = []int{3, 24}[iter%2]
		if runModes(t, fmt.Sprintf("iter %d", iter), insns, nReg+1) {
			native++
		}
	}
	pairs, dead := jit.VMFusionStatsForTest()
	if native < iters/2 || pairs == 0 || dead == 0 {
		t.Fatalf("%d of %d programs native, %d pairs fused, %d stores dead: the test checks too little", native, iters, pairs, dead)
	}
	t.Logf("%d programs agree on all three paths; %d pairs fused, %d stores dead", native, pairs, dead)
}

// TestVMJITFusionQueries: conditions as SQL writes them -- AND/OR/NOT chains,
// CASE, % and / -- over cursors of integers, NULLs, reals and text, with the
// JIT off, fused and unfused. Every answer must match.
func TestVMJITFusionQueries(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(5))
	val := func() string {
		switch rng.Intn(12) {
		case 0:
			return "NULL"
		case 1:
			return fmt.Sprintf("%g", float64(rng.Intn(9))/2)
		case 2:
			return fmt.Sprintf("'%d'", rng.Intn(6))
		case 3:
			return []string{"-1", "0", "9223372036854775807", "-9223372036854775808"}[rng.Intn(4)]
		default:
			return fmt.Sprint(rng.Intn(7) - 1)
		}
	}
	var setup []string
	for _, tbl := range []string{"a", "b"} {
		setup = append(setup, fmt.Sprintf("CREATE TABLE %s(id INTEGER PRIMARY KEY, x, y, z)", tbl))
		for i := 1; i <= 15+rng.Intn(10); i++ {
			setup = append(setup, fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, %s, %s)", tbl, i, val(), val(), val()))
		}
	}
	col := func(ts ...string) string {
		return ts[rng.Intn(len(ts))] + "." + []string{"x", "y", "z", "id"}[rng.Intn(4)]
	}
	var expr func(depth int, ts ...string) string
	term := func(ts ...string) string {
		l, r := col(ts...), col(ts...)
		if rng.Intn(3) == 0 {
			r = fmt.Sprint(rng.Intn(5) - 1)
		}
		switch rng.Intn(5) {
		case 0:
			l = fmt.Sprintf("(%s %% %s)", l, []string{"3", "-1", "0", col(ts...)}[rng.Intn(4)])
		case 1:
			l = fmt.Sprintf("(%s / %s)", l, []string{"2", "-1", "0", col(ts...)}[rng.Intn(4)])
		}
		return fmt.Sprintf("%s %s %s", l, []string{"=", "<", ">", "<=", ">=", "<>"}[rng.Intn(6)], r)
	}
	expr = func(depth int, ts ...string) string {
		if depth == 0 || rng.Intn(3) == 0 {
			return term(ts...)
		}
		switch rng.Intn(3) {
		case 0:
			return fmt.Sprintf("NOT (%s)", expr(depth-1, ts...))
		case 1:
			return fmt.Sprintf("(%s AND %s)", expr(depth-1, ts...), expr(depth-1, ts...))
		default:
			return fmt.Sprintf("(%s OR %s)", expr(depth-1, ts...), expr(depth-1, ts...))
		}
	}
	var queries []string
	for range 500 {
		switch rng.Intn(5) {
		case 0:
			queries = append(queries, fmt.Sprintf("SELECT a.id FROM a WHERE %s ORDER BY 1", expr(3, "a")))
		case 1:
			queries = append(queries, fmt.Sprintf("SELECT a.id, b.id FROM a, b WHERE %s ORDER BY 1, 2", expr(3, "a", "b")))
		case 2:
			queries = append(queries, fmt.Sprintf("SELECT sum(CASE WHEN %s THEN a.x ELSE a.y END), count(*) FROM a WHERE %s", expr(2, "a"), expr(2, "a")))
		case 3:
			queries = append(queries, fmt.Sprintf("SELECT a.id, %s, (%s) IS NULL FROM a ORDER BY 1", expr(3, "a"), expr(2, "a")))
		default:
			queries = append(queries, fmt.Sprintf("SELECT count(*) FROM (SELECT x, y, id FROM a) d, b WHERE %s", expr(3, "d", "b")))
		}
	}
	run := func(jitOn, fused bool) []string {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		jit.SetFusion(fused)
		defer func() { vmJITEnabled = old; jit.SetFusion(true) }()
		db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		p, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		out := make([]string, len(queries))
		for i, q := range queries {
			cols, rows, err := p.QueryArgs(q, nil)
			if err != nil {
				out[i] = "error: " + err.Error()
				continue
			}
			out[i] = fmt.Sprint(cols, rows)
		}
		return out
	}
	jit.VMFusionStatsForTest()
	want := run(false, true)
	fused := run(true, true)
	pairs, _ := jit.VMFusionStatsForTest()
	unfused := run(true, false)
	for i, q := range queries {
		if want[i] != fused[i] || want[i] != unfused[i] {
			t.Fatalf("JIT diverges on %s\nvdbe    %s\nfused   %s\nunfused %s", q, want[i], fused[i], unfused[i])
		}
	}
	failed := 0
	for _, g := range want {
		if strings.HasPrefix(g, "error: ") {
			failed++
		}
	}
	if failed > len(queries)/10 || pairs == 0 {
		t.Fatalf("%d of %d queries failed, %d pairs fused: the test checks too little (%s)", failed, len(queries), pairs, want[0])
	}
	t.Logf("%d queries agree; %d pairs fused", len(queries), pairs)
}
