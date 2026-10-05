package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// tableSegments builds a table and returns segments, column names, and a reference counter.
func tableSegments(t *testing.T, stmts ...string) ([]*segment, []string, func(q string, args ...Value) int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.musq")
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	segPeepholesOffForTest = true
	t.Cleanup(func() { segPeepholesOffForTest = false })
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Discard() })
	tbl := n.findTableMetaIn(scopeMain, "t")
	if tbl == nil {
		t.Fatal("no table t")
	}
	segs := n.segments.segs.byRoot[tbl.rootPage]
	if len(segs) == 0 {
		t.Fatal("table t has no segments after VACUUM")
	}
	var cols []string
	for _, c := range tbl.cols {
		cols = append(cols, c.Name)
	}
	ref := func(q string, args ...Value) int {
		t.Helper()
		_, rows, qerr := n.Query(q, args)
		if qerr != nil {
			t.Fatalf("reference %q: %v", q, qerr)
		}
		return int(rows[0][0].I)
	}
	return segs, cols, ref
}

func TestSegmentScanAgreesWithTheEngine(t *testing.T) {
	stmts := []string{`CREATE TABLE t(clean INTEGER, nullable INTEGER, mixed, txt TEXT)`}
	for i := 0; i < 1500; i++ {
		var nullable, mixed string
		switch {
		case i%13 == 0:
			nullable = "NULL"
		default:
			nullable = fmt.Sprintf("%d", i-700)
		}
		switch {
		case i == 99: // one stray text: the exception list
			mixed = "'ninety nine'"
		case i%29 == 0: // some NULLs
			mixed = "NULL"
		default:
			mixed = fmt.Sprintf("%d", 1000-i)
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%s,%s,'s%d')`, i-750, nullable, mixed, i))
	}
	segs, cols, ref := tableSegments(t, stmts...)

	sawSlice, sawValues := false, false
	for col, name := range cols {
		for _, s := range segs {
			if kind, _ := s.scanKind(col); kind == segScanSlice {
				sawSlice = true
			} else {
				sawValues = true
			}
		}
		for _, bound := range []int64{-100000, -751, -750, -1, 0, 99, 500, 1000, 100000} {
			want := ref(fmt.Sprintf(`SELECT count(*) FROM t WHERE %s > ?`, quoteIdent(name)), Value{Typ: Int, I: bound})
			got := 0
			for _, s := range segs {
				got += segmentCountGreater(s, col, bound)
			}
			if got != want {
				t.Fatalf("column %s > %d: segments say %d, the engine says %d", name, bound, got, want)
			}
		}
	}
	if !sawSlice || !sawValues {
		t.Fatalf("this fixture exercised slice=%v values=%v; it must reach both paths to prove anything", sawSlice, sawValues)
	}
}

// Multi-predicate filters, differential against the engine -- the same shape
// that caught a wrong answer the first time it ran.
//
// The operators matter individually here, not just as a set. compareValues
// ORDERS a NULL below every value, which is right for a sort and wrong for a
// WHERE: taken literally it makes "NULL < 5" and "NULL <> 5" satisfied, where
// SQL says a comparison involving NULL is NULL and NULL is not TRUE. Only < , <=
// and <> expose that, and only on a column that actually holds NULLs, so the
// fixture has both.
func TestSegmentFilterAgreesWithTheEngine(t *testing.T) {
	stmts := []string{`CREATE TABLE t(a INTEGER, b INTEGER, c)`}
	for i := 0; i < 900; i++ {
		b := fmt.Sprintf("%d", i%17)
		if i%11 == 0 {
			b = "NULL"
		}
		c := fmt.Sprintf("%d", 900-i)
		switch {
		case i == 400:
			c = "'text value'" // an exception, so column c cannot take the fast path
		case i%23 == 0:
			c = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%s,%s)`, i-450, b, c))
	}
	segs, _, ref := tableSegments(t, stmts...)

	ops := []struct {
		op  segPredOp
		sql string
	}{{segGT, ">"}, {segGE, ">="}, {segLT, "<"}, {segLE, "<="}, {segEQ, "="}, {segNE, "<>"}}
	cases := 0
	for ci, col := range []string{"a", "b", "c"} {
		for _, o := range ops {
			for _, bound := range []int64{-451, -450, 0, 5, 16, 450, 901} {
				where := fmt.Sprintf("%s %s ?", quoteIdent(col), o.sql)
				want := ref(`SELECT count(*) FROM t WHERE `+where, Value{Typ: Int, I: bound})
				got := 0
				for _, s := range segs {
					got += segFilterCount(s, []segPred{{Col: ci, Op: o.op, Val: Value{Typ: Int, I: bound}}})
				}
				if got != want {
					t.Fatalf("%s %s %d: segments say %d, engine says %d", col, o.sql, bound, got, want)
				}
				cases++
			}
		}
	}
	// ...and two predicates together, which is what the kernels are for.
	for _, bound := range []int64{-200, 0, 200} {
		want := ref(`SELECT count(*) FROM t WHERE a > ? AND b < 9`, Value{Typ: Int, I: bound})
		got := 0
		for _, s := range segs {
			got += segFilterCount(s, []segPred{
				{Col: 0, Op: segGT, Val: Value{Typ: Int, I: bound}},
				{Col: 1, Op: segLT, Val: Value{Typ: Int, I: 9}},
			})
		}
		if got != want {
			t.Fatalf("a > %d AND b < 9: segments say %d, engine says %d", bound, got, want)
		}
		cases++
	}
	if cases < 100 {
		t.Fatalf("only %d comparisons ran; this test would prove little", cases)
	}
}

// All thirty-six operator pairs, differential against the engine.
//
// A filter is not one comparison, and the rare pairs are where a wrong
// comparison hides: no benchmark reaches "<= AND <>", and a hand-picked fixture
// would not either. Every pair runs against the engine's own answer for the
// same SQL, over a clean column pair and a NULL-bearing one so both the fast
// path and the fallback are on it.
func TestOperatorPairsAgreeWithTheEngine(t *testing.T) {
	// a and b are CLEAN, which is what makes the generated path reachable; n
	// carries NULLs so the fallback is covered too, and the test ASSERTS which
	// path each column takes rather than assuming.
	stmts := []string{`CREATE TABLE t(a INTEGER, b INTEGER, n INTEGER)`}
	for i := 0; i < 600; i++ {
		n := fmt.Sprintf("%d", i%5)
		if i%37 == 0 {
			n = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%s)`, i%11-5, i%7, n))
	}
	segs, _, ref := tableSegments(t, stmts...)
	ops := []struct {
		op  segPredOp
		sql string
	}{{segGT, ">"}, {segGE, ">="}, {segLT, "<"}, {segLE, "<="}, {segEQ, "="}, {segNE, "<>"}}

	for _, s := range segs {
		for _, c := range []int{0, 1} {
			if kind, why := s.scanKind(c); kind != segScanSlice {
				t.Fatalf("column %d is not a fixed-width block (%s), so no pair below would reach the fast path", c, why)
			}
		}
		if kind, _ := s.scanKind(2); kind != segScanValues {
			t.Fatal("column n was meant to carry NULLs and decline the fast path")
		}
	}
	pairs := 0
	for _, oa := range ops {
		for _, ob := range ops {
			for _, bounds := range [][2]int64{{0, 3}, {-5, 0}, {5, 6}, {-6, 7}} {
				for _, rhs := range []struct {
					col  int
					name string
				}{{1, "b"}, {2, "n"}} {
					want := ref(fmt.Sprintf(`SELECT count(*) FROM t WHERE a %s ? AND %s %s ?`, oa.sql, rhs.name, ob.sql),
						Value{Typ: Int, I: bounds[0]}, Value{Typ: Int, I: bounds[1]})
					got := 0
					for _, s := range segs {
						got += segFilterCount(s, []segPred{
							{Col: 0, Op: oa.op, Val: Value{Typ: Int, I: bounds[0]}},
							{Col: rhs.col, Op: ob.op, Val: Value{Typ: Int, I: bounds[1]}},
						})
					}
					if got != want {
						t.Fatalf("a %s %d AND %s %s %d: segments say %d, engine says %d",
							oa.sql, bounds[0], rhs.name, ob.sql, bounds[1], got, want)
					}
					pairs++
				}
			}
		}
	}
	if pairs != 36*4*2 {
		t.Fatalf("ran %d cases, want %d -- every operator pair must be covered on both paths", pairs, 36*4*2)
	}
}
