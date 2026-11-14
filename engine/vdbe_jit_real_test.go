package engine

import (
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// sameRowsBits is a == b with every REAL compared to the bit, so a -0.0 for a
// 0.0 is a difference.
func sameRowsBits(a, b [][]Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j, x := range a[i] {
			y := b[i][j]
			if x.Typ != y.Typ || x.Subtype != y.Subtype || x.intReal != y.intReal || x.I != y.I ||
				math.Float64bits(x.F) != math.Float64bits(y.F) || string(x.S) != string(y.S) {
				return false
			}
		}
	}
	return true
}

var vmRealFuncs = []struct {
	name string
	n    int
}{
	{"sqrt", 1}, {"sin", 1}, {"cos", 1}, {"tan", 1}, {"asin", 1}, {"acos", 1}, {"atan", 1}, {"atan2", 2},
	{"sinh", 1}, {"cosh", 1}, {"tanh", 1}, {"asinh", 1}, {"acosh", 1}, {"atanh", 1}, {"exp", 1},
	{"ln", 1}, {"log", 1}, {"log", 2}, {"log2", 1}, {"log10", 1}, {"pow", 2}, {"power", 2}, {"mod", 2},
	{"abs", 1}, {"ceil", 1}, {"ceiling", 1}, {"floor", 1}, {"trunc", 1}, {"pi", 0}, {"degrees", 1}, {"radians", 1},
}

// TestVMJITRealDifferential: random programs of REAL arithmetic, comparisons
// and math calls over operands that fail each guard of the REAL arms -- NULL,
// TEXT, two INTEGERs, an INTEGER a double does not hold, a zero divisor, a NaN
// answer -- run with the JIT and without, compared to the bit.
func TestVMJITRealDifferential(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled || !jit.VMReal {
		t.Skip("no REAL arms on this platform")
	}
	native, arms := vmRealDifferential(t, 11, 4000)
	if native < 3000 || arms < 20000 {
		t.Fatalf("%d of 4000 programs had native code, with %d REAL arms", native, arms)
	}
	t.Logf("%d programs with native code agree; %d REAL arms", native, arms)
}

// vmRealDifferential runs n random programs from seed, and counts those with
// native code and the REAL arms in them.
func vmRealDifferential(t *testing.T, seed int64, n int) (native, arms int) {
	const nReg = 8
	reals := []float64{0.5, math.Copysign(0, -1), 0, 1e308, -1e308, 3, 2.5, -7.25, 5e-324, 1 << 53, math.Inf(1), 1e-3, 0.75}
	ints := []int64{0, 1, -1, 3, 1 << 53, 1<<53 + 1, -(1 << 53) - 1, math.MaxInt64, math.MinInt64}
	texts := []string{"1.5", "abc", "12", ""}
	rng := rand.New(rand.NewSource(seed))
	for iter := range n {
		var insns []Instruction
		add := func(op OpCode, p1, p2, p3 int, p4 any, p5 uint16) {
			insns = append(insns, Instruction{Op: op, P1: p1, P2: p2, P3: p3, P4: p4, P5: p5})
		}
		r := func() int { return rng.Intn(nReg) }
		for reg := range nReg {
			switch rng.Intn(10) {
			case 0:
				add(OpNull, 0, reg, 0, nil, 0)
			case 1:
				add(OpString8, 0, reg, 0, texts[rng.Intn(len(texts))], 0)
			case 2, 3:
				add(OpInt64, 0, reg, 0, ints[rng.Intn(len(ints))], 0)
			default:
				add(OpReal, 0, reg, 0, reals[rng.Intn(len(reals))], 0)
			}
		}
		body := 25
		start := len(insns)
		end := start + body
		for i := range body {
			pc := start + i
			fwd := pc + 1 + rng.Intn(end-pc)
			switch rng.Intn(14) {
			case 0:
				add(OpReal, 0, r(), 0, reals[rng.Intn(len(reals))], 0)
			case 1, 2, 3:
				add([]OpCode{OpAdd, OpSubtract, OpMultiply, OpDivide}[rng.Intn(4)], r(), r(), r(), nil, 0)
			case 4, 5:
				p5 := []uint16{0, p5JumpIfNull, p5NullEq, p5StoreP2, p5StoreP2 | p5NullEq}[rng.Intn(5)]
				p5 |= uint16([]affinity{affNone, affNumeric, affInteger, affReal, affText}[rng.Intn(5)])
				p2 := fwd
				if p5&p5StoreP2 != 0 {
					p2 = r()
				}
				add([]OpCode{OpEq, OpNe, OpLt, OpLe, OpGt, OpGe}[rng.Intn(6)], r(), p2, r(), nil, p5)
			case 6, 7, 8:
				f := vmRealFuncs[rng.Intn(len(vmRealFuncs))]
				base := rng.Intn(nReg - f.n + 1)
				add(OpFunction, base, f.n, r(), f.name, 0)
			case 9:
				add([]OpCode{OpIf, OpIfNot}[rng.Intn(2)], r(), fwd, rng.Intn(2), nil, 0)
			case 10:
				add([]OpCode{OpIsNull, OpNotNull}[rng.Intn(2)], r(), fwd, 0, nil, 0)
			case 11:
				add([]OpCode{OpSCopy, OpCopy}[rng.Intn(2)], r(), r(), 0, nil, 0)
			case 12:
				add(OpNegative, r(), r(), 0, nil, 0)
			default:
				add(OpInt64, 0, r(), 0, ints[rng.Intn(len(ints))], 0)
			}
		}
		add(OpResultRow, 0, nReg, 0, nil, 0)
		add(OpHalt, 0, 0, 0, nil, 0)

		prog := &Program{Insns: insns, NReg: nReg, NResultCol: nReg}
		run := func(withJIT bool) ([][]Value, error) {
			st, err := NewProgramStmt(prog)
			if err != nil {
				t.Fatalf("iter %d: %v", iter, err)
			}
			if withJIT && st.m.jx == nil {
				return nil, errNoNative
			}
			if !withJIT {
				st.m.jx = nil
			}
			var rows [][]Value
			for {
				row, err := st.Step()
				if err != nil || row == nil {
					return rows, err
				}
				rows = append(rows, append([]Value(nil), row...))
			}
		}
		want, wantErr := run(false)
		got, gotErr := run(true)
		if gotErr == errNoNative {
			continue
		}
		if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) || !sameRowsBits(want, got) {
			t.Fatalf("seed %d iter %d: JIT diverges\nprogram %v\nvdbe %v %v\njit  %v %v", seed, iter, insns, want, wantErr, got, gotErr)
		}
		native++
		if _, ok := vmRealLowering(prog, nil, nil); ok != nil {
			for _, o := range ok {
				if o {
					arms++
				}
			}
		}
	}
	return native, arms
}

// TestVMJITRealQueries: row queries over REAL columns -- arithmetic, math
// calls and comparisons on the rows the VM JIT reads -- with the JIT and
// without, over REALs, integral REALs, INTEGERs, NULLs and TEXT a REAL column
// keeps as it is.
func TestVMJITRealQueries(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled || !jit.VMReal {
		t.Skip("no REAL arms on this platform")
	}
	rng := rand.New(rand.NewSource(5))
	val := func() string {
		switch rng.Intn(12) {
		case 0:
			return "NULL"
		case 1:
			return []string{"'abc'", "'2.5'", "''"}[rng.Intn(3)]
		case 2:
			return fmt.Sprint(rng.Intn(9) - 4)
		case 3:
			return []string{"0.0", "-0.0", "1e308", "-1e308", "9007199254740993", "4.0"}[rng.Intn(6)]
		default:
			return fmt.Sprintf("%.17g", (rng.Float64()-0.5)*math.Pow(10, float64(rng.Intn(6)-2)))
		}
	}
	setup := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, x REAL, y REAL, i INTEGER, n)`}
	for id := 1; id <= 60; id++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %s, %s, %s, %s)", id, val(), val(), val(), val()))
	}
	exprs := []string{
		"x", "x*2+1", "x+y", "x-y", "x*y", "x/y", "y/x", "x/0", "x+i", "i*1.5", "-x", "x*-1", "n+0.5", "n*x",
		"abs(x)", "abs(-y)", "sqrt(x)", "sqrt(abs(x))", "exp(x/1000)", "ln(x)", "log(x)", "log(2, x)", "log(x, y)",
		"log2(abs(y)+1)", "log10(x)", "pow(x, y)", "pow(abs(x), 0.5)", "power(y, 2)", "sin(x)", "cos(y)", "tan(x)",
		"asin(x)", "acos(y/10)", "atan(x)", "atan2(x, y)", "sinh(y)", "cosh(x/100)", "tanh(y)", "asinh(x)",
		"acosh(abs(x)+1)", "atanh(y/10)", "mod(x, 3)", "mod(y, x)", "ceil(x)", "ceiling(y)", "floor(x)", "trunc(y)",
		"floor(i)", "degrees(x)", "radians(y)", "pi()*x", "x > y", "x <= 0.5", "x = y", "x <> 1.5", "x > i",
		"sqrt(x) > 1", "x IS y", "(x+y)/(x-y)",
	}
	preds := []string{
		"x > 0.5", "x < y", "x >= -1 AND y < 2", "sqrt(x) > 1", "x/y > 1", "ln(x) IS NULL", "x*2 > i",
		"abs(x) < 1e-3", "x > 1 OR y < -1", "x = 4.0", "n > 0.25", "pow(x, 2) < 10",
	}
	var queries []string
	for _, e := range exprs {
		queries = append(queries, fmt.Sprintf("SELECT id, %s FROM t ORDER BY id", e))
	}
	for _, p := range preds {
		queries = append(queries, fmt.Sprintf("SELECT id, x, y FROM t WHERE %s ORDER BY id", p))
		queries = append(queries, fmt.Sprintf("SELECT id, x*y, exp(x/1000) FROM t WHERE %s ORDER BY id", p))
	}
	run := func(jitOn bool) ([]string, int) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		db, err := Create(filepath.Join(t.TempDir(), "r.musq"))
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
		arms := 0
		for i, q := range queries {
			cols, rows, err := p.QueryArgs(q, nil)
			if err != nil {
				out[i] = "error: " + err.Error()
				continue
			}
			var b strings.Builder
			fmt.Fprint(&b, cols)
			for _, row := range rows {
				for _, v := range row {
					fmt.Fprintf(&b, " %d:%d:%x:%q", v.Typ, v.I, math.Float64bits(v.F), v.S)
				}
				b.WriteString(";")
			}
			out[i] = b.String()
			if stmt, err := ParseSelect(q); err == nil {
				if prog, err := compileSelectScan(p, stmt, nil); err == nil {
					if _, ok := vmRealLowering(prog, nil, nil); ok != nil {
						for _, o := range ok {
							if o {
								arms++
							}
						}
					}
				}
			}
		}
		return out, arms
	}
	want, _ := run(false)
	got, arms := run(true)
	for i := range queries {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges on %s\nvdbe %s\njit  %s", queries[i], want[i], got[i])
		}
		if strings.HasPrefix(got[i], "error: ") {
			t.Fatalf("%s: %s", queries[i], got[i])
		}
	}
	if arms < len(queries) {
		t.Fatalf("%d REAL arms over %d queries: the test checks little", arms, len(queries))
	}
	t.Logf("%d queries agree; %d REAL arms", len(queries), arms)
}

// TestVMRealLoweringGuessesStoredReals: a column of INTEGER affinity is
// guessed REAL when the table's segments store it so, and its arithmetic
// lowers to the REAL arm; without the hint it keeps the INTEGER arm.
func TestVMRealLoweringGuessesStoredReals(t *testing.T) {
	tbl := &resolvedTable{root: 7, cols: []columnInfo{{Name: "price", Aff: affInteger}}}
	prog := &Program{NReg: 4, Insns: []Instruction{
		{Op: OpOpenRead, P1: 0, P2: 7, P4: tbl},
		{Op: OpColumn, P1: 0, P2: 0, P3: 1},
		{Op: OpInteger, P1: 2, P2: 2},
		{Op: OpMultiply, P1: 2, P2: 1, P3: 3},
	}}
	stored := func(root uint32, col int) bool { return root == 7 && col == 0 }
	out, ok := vmRealLowering(prog, stored, nil)
	if ok == nil || !ok[3] || out[3].Op != jit.VFMul {
		t.Fatalf("a stored-REAL column's multiply lowered to %v, want VFMul", out[3])
	}
	if _, ok := vmRealLowering(prog, nil, nil); ok != nil && ok[3] {
		t.Fatalf("an INTEGER column's multiply took the REAL arm with no hint")
	}
}
