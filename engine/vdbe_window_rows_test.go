package engine

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// TestWindowRowsRoundTrip checks that every Value the window batch buffers
// reads back exactly: each field, nil versus empty bytes, values that need
// the escape list, bytes larger than an arena chunk, and rows across many
// cell and row chunks.
func TestWindowRowsRoundTrip(t *testing.T) {
	big := []byte(strings.Repeat("y", winChunkMax+17))
	vals := []Value{
		{},
		{Typ: Int, I: -1},
		{Typ: Int, I: math.MinInt64},
		{Typ: Float, F: math.Copysign(0, -1)},
		{Typ: Float, F: math.NaN()},
		{Typ: Float, F: 3, intReal: true},
		{Typ: Float, F: 2.5, intReal: true},
		{Typ: Text, S: []byte("abc"), Subtype: 74},
		{Typ: Text, S: []byte{}},
		{Typ: Blob, S: []byte{}},
		{Typ: Blob, S: []byte{0, 1, 2}},
		{Typ: Text, S: []byte("i"), I: 7},
		{Typ: Text, S: []byte("f"), F: 1.25},
		{Typ: Text, S: []byte("both"), I: 7, F: 1.25},
		{Typ: Float, F: 1e300, I: 9},
	}
	// Every value here is payload of a column read with REAL affinity, where
	// an IntReal is stored as the REAL it reads as (appendRow).
	want := func(v Value) Value {
		v.intReal = false
		return v
	}
	var r windowRows
	r.appendRow([]Value{{Typ: Blob, S: big}, {Typ: Text, S: []byte("after")}}, allReal(2))
	if g := r.at(0, 0); !bytes.Equal(g.S, big) || g.Typ != Blob {
		t.Fatalf("big blob read back as %d bytes of type %v", len(g.S), g.Typ)
	}
	if g := r.at(0, 1); string(g.S) != "after" {
		t.Fatalf("value after the big blob: %q", g.S)
	}
	r = windowRows{}
	const n = 3 * (1 << winRowChunkLg)
	for i := range n {
		row := make([]Value, i%len(vals)+1)
		for j := range row {
			row[j] = vals[(i+j)%len(vals)]
		}
		r.appendRow(row, allReal(len(row)))
	}
	if r.len() != n {
		t.Fatalf("len %d, want %d", r.len(), n)
	}
	var buf []Value
	for i := range n {
		w := i%len(vals) + 1
		if r.width(i) != w {
			t.Fatalf("row %d: width %d, want %d", i, r.width(i), w)
		}
		got := r.rangeInto(i, 0, w, &buf)
		for j := range w {
			exp := want(vals[(i+j)%len(vals)])
			g := got[j]
			if g.Typ != exp.Typ || g.Subtype != exp.Subtype || g.intReal != exp.intReal || g.I != exp.I ||
				math.Float64bits(g.F) != math.Float64bits(exp.F) || (g.S == nil) != (exp.S == nil) || !bytes.Equal(g.S, exp.S) {
				t.Fatalf("row %d col %d: got %+v, want %+v", i, j, g, exp)
			}
			if cap(g.S) != len(g.S) {
				t.Fatalf("row %d col %d: bytes have spare capacity %d > %d", i, j, cap(g.S), len(g.S))
			}
			if a := r.at(i, j); !valuesIdentical(a, g) {
				t.Fatalf("row %d col %d: at %+v, rangeInto %+v", i, j, a, g)
			}
		}
		if tail := r.rangeInto(i, w-1, w+2, &buf); len(tail) != 3 || tail[1].Typ != Null || tail[2].Typ != Null {
			t.Fatalf("row %d: tail %+v, want its last value then two NULLs", i, tail)
		}
	}
	// A value handed out keeps its bytes however much more is appended.
	held := r.at(7, 0)
	before := append([]byte(nil), held.S...)
	for range 1000 {
		r.appendRow([]Value{{Typ: Text, S: []byte("later")}}, allReal(1))
	}
	if !bytes.Equal(held.S, before) {
		t.Fatalf("held bytes changed: %q, was %q", held.S, before)
	}
}

// wideWindowRows is n rows of nCols columns, one rowid and nOps operands,
// every cell distinct.
func wideWindowRows(n, nCols, nOps int) *windowRows {
	var r windowRows
	for i := range n {
		row := make([]Value, nCols+1+nOps)
		for j := range row {
			row[j] = Value{Typ: Int, I: int64(i*1000 + j)}
		}
		r.appendRow(row, make([]bool, nCols+1))
	}
	return &r
}

// TestWindowStepDecodesOnlyWhatItReads pins what an aggregate step decodes
// from a buffered row. Its context's columns and rowids are read only to
// resolve a collation, once per accumulator, so a sum() decodes them for its
// first row and never again, while over a RIGHT or FULL join's USING column a
// max() decodes each row until its collation is resolved; of the operand
// block only the cells the accumulator's registers name are decoded. Decoding whole rows per step made
// a window over a wide table several times slower than before the batch held
// its rows as cells.
func TestWindowStepDecodesOnlyWhatItReads(t *testing.T) {
	const nCols, nOps = 30, 9
	batch := wideWindowRows(3, nCols, nOps)
	// A FULL JOIN's USING column resolves its collation against the row.
	plan := &windowPlan{nCols: nCols, nRowids: 1, scopes: []tableScope{{coalesceFallback: map[string][]int{}}}}
	vals := make([][]Value, 3)
	rows := newWindowRowCtxs(&vdbe{}, plan, batch, vals)
	if rows.rowFree {
		t.Fatal("a scope with a coalesce fallback must not be row-free")
	}

	sum := &aggItem{aggItemPlan: &aggItemPlan{kind: aggSum}}
	if sum.rowCollationPending() {
		t.Fatal("sum() resolves no collation, yet reports one pending")
	}
	for ri := range 3 {
		ctx := rows.at(ri, sum)
		if len(ctx.vals) != nCols || len(ctx.rowids) != 1 {
			t.Fatalf("row %d: context of %d columns and %d rowids", ri, len(ctx.vals), len(ctx.rowids))
		}
		if ctx.vals[nCols-1].I != int64(nCols-1) {
			t.Fatalf("row %d: sum()'s context was decoded again (last column %d)", ri, ctx.vals[nCols-1].I)
		}
	}

	mx := &aggItem{aggItemPlan: &aggItemPlan{kind: aggMax}}
	for ri := range 3 {
		if ctx := rows.at(ri, mx); ctx.vals[0].I != int64(ri*1000) || ctx.rowids[0].I != int64(ri*1000+nCols) {
			t.Fatalf("row %d: max()'s context holds %d/%d before its collation is resolved", ri, ctx.vals[0].I, ctx.rowids[0].I)
		}
	}
	mx.argCollSet = true
	if ctx := rows.at(1, mx); ctx.vals[0].I != 2000 {
		t.Fatalf("max()'s context was decoded after its collation was resolved (%d)", ctx.vals[0].I)
	}

	// With no such column no collation depends on the row: max() decodes none.
	free := newWindowRowCtxs(&vdbe{}, &windowPlan{nCols: nCols, nRowids: 1}, batch, vals)
	mx2 := &aggItem{aggItemPlan: &aggItemPlan{kind: aggMax}}
	for ri := range 3 {
		if ctx := free.at(ri, mx2); ctx.vals[0].I != 0 {
			t.Fatalf("row %d: max()'s context was decoded though no collation depends on the row", ri)
		}
	}

	ob := &aggItem{aggItemPlan: &aggItemPlan{kind: aggGroupConcat, orderBy: []OrderTerm{{}}}}
	a, b := rows.at(0, ob), rows.at(1, ob)
	if a == b || a.vals[0].I != 0 || b.vals[0].I != 1000 {
		t.Fatal("an ORDER BY aggregate's contexts must be its own until its collations resolve")
	}

	sum.rowRegs[aggExprArg] = 3
	read := sum.operandRegsRead()
	if len(read) != 1 || read[0] != 2 {
		t.Fatalf("operandRegsRead %v, want [2]", read)
	}
	var buf []Value
	for ri := range 3 {
		regs := batch.pickInto(ri, nCols+1, nOps, read, &buf)
		if len(regs) != nOps {
			t.Fatalf("row %d: %d operand registers, want %d", ri, len(regs), nOps)
		}
		for i, v := range regs {
			if i == 2 {
				if v.I != int64(ri*1000+nCols+1+2) {
					t.Fatalf("row %d: operand 2 is %d", ri, v.I)
				}
			} else if v.Typ != Null {
				t.Fatalf("row %d: operand %d was decoded though no register names it", ri, i)
			}
		}
	}
}

// TestWindowHeadRowDecodedOnce pins that the row a key's collations are
// resolved against (windowKeyCollations, once per partition and call) is
// decoded once for the batch, where it was a whole row decoded into a slice
// of its own every time.
func TestWindowHeadRowDecodedOnce(t *testing.T) {
	batch := wideWindowRows(2, 30, 9)
	h := batch.headRow(31)
	if len(h) != 31 || h[30].I != 30 {
		t.Fatalf("headRow: %d values", len(h))
	}
	if n := testing.AllocsPerRun(100, func() { batch.headRow(31) }); n != 0 {
		t.Fatalf("headRow allocates %v times per call once decoded", n)
	}
	plan := &windowPlan{nCols: 30, nRowids: 1}
	m := &vdbe{}
	vals := make([][]Value, 2)
	exprs := []Expr{LiteralExpr{}}
	m.windowKeyCollations(plan, exprs, batch, vals)
	before := testing.AllocsPerRun(100, func() { m.windowKeyCollations(plan, exprs, batch, vals) })
	if before > 2 {
		t.Fatalf("windowKeyCollations allocates %v times per call", before)
	}
}

func allReal(n int) []bool {
	b := make([]bool, n)
	for i := range b {
		b[i] = true
	}
	return b
}
