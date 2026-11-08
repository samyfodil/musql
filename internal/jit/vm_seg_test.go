//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"encoding/binary"
	"math"
	"testing"
	"unsafe"
)

// TestVMSegColumn reads every kind of segment cell through a segment-capable
// VColumn, and the same instruction over a row list: a NULL and an exception
// exit, an integral double within 2^53 is an INTEGER, the rowid comes from a
// dense segment or its rowid block, and TEXT shares the heap.
func TestVMSegColumn(t *testing.T) {
	if !Available || !VMSegCursor {
		t.Skip("no segment cursors on this platform")
	}
	noGC(t)
	const n = 6
	ints := []int64{5, -7, 1 << 40, 0, 3, 9}
	floats := []float64{1.5, -2.25, 0, 3, 1e300, 0.1}
	nums := []float64{2, 2.5, 1 << 53, 1<<53 + 2, -1 << 53, math.NaN()}
	rowids := []uint64{10, 20, 30, 41, 52, 63}
	heap := []byte("abcdefgh")
	bounds := []uint32{0, 3, 3, 5, 8, 8, 8}
	blk := func(v []uint64) unsafe.Pointer { return unsafe.Pointer(unsafe.SliceData(v)) }
	words := func(f func(i int) uint64) []uint64 {
		w := make([]uint64, n)
		for i := range w {
			w[i] = f(i)
		}
		return w
	}
	intBlk := words(func(i int) uint64 { return uint64(ints[i]) })
	fltBlk := words(func(i int) uint64 { return math.Float64bits(floats[i]) })
	numBlk := words(func(i int) uint64 { return math.Float64bits(nums[i]) })
	bnd := make([]byte, 4*len(bounds))
	for i, b := range bounds {
		binary.LittleEndian.PutUint32(bnd[4*i:], b)
	}
	nulls := []byte{1 << 4} // row 4 is NULL
	exc := []uint64{1 << 1} // row 1 is an exception
	cols := []VSegCol{
		{Kind: VSegInt, Block: blk(intBlk), Nulls: unsafe.Pointer(&nulls[0])},
		{Kind: VSegFloat, Block: blk(fltBlk), Exc: unsafe.Pointer(&exc[0])},
		{Kind: VSegNumeric, Block: blk(numBlk)},
		{Kind: VSegIntReal, Block: blk(intBlk)},
		{Kind: VSegRowid},
		{Kind: VSegText, Block: unsafe.Pointer(&bnd[0]), Heap: unsafe.Pointer(&heap[0])},
		{Kind: VSegNone},
	}
	for _, dense := range []bool{true, false} {
		seg := &VSeg{Cols: unsafe.Pointer(&cols[0]), First: 100, Rowids: blk(rowids)}
		if dense {
			seg.Dense = 1
		}
		for c := range cols {
			for row := range n {
				regs := make([]tvalue, 2)
				regs[0] = tvalue{Typ: tvInt, I: 77}
				cur := &VCursor{N: n, Pos: int64(row), Last: -1, Ok: 1, Mask: 1<<6 - 1, Seg: unsafe.Pointer(seg)}
				pc := runVMCursor(t, []VInsn{{Op: VColumn, A: 0, B: c, C: 0, Imm: 1}, {Op: VExit}}, regs, nil, cur, 0)
				want, exits := tvalue{}, false
				switch c {
				case 0:
					want, exits = tvalue{Typ: tvInt, I: ints[row]}, row == 4
				case 1:
					want, exits = tvalue{Typ: tvReal, F: floats[row]}, row == 1
				case 2:
					if f := nums[row]; f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
						want = tvalue{Typ: tvInt, I: int64(f)}
					} else {
						want = tvalue{Typ: tvReal, F: f}
					}
				case 3:
					want = tvalue{Typ: tvReal, F: float64(ints[row])}
				case 4:
					want = tvalue{Typ: tvInt, I: int64(rowids[row])}
					if dense {
						want.I = 100 + int64(row)
					}
				case 5:
					want = tvalue{Typ: tvText, S: heap[bounds[row]:bounds[row+1]]}
				case 6:
					exits = true
				}
				if exits {
					if pc != 0 || regs[0].Typ != tvInt || regs[0].I != 77 {
						t.Fatalf("col %d row %d: pc %d r0 %+v, want an exit leaving r0", c, row, pc, regs[0])
					}
					continue
				}
				got := regs[0]
				same := pc == 1 && got.Typ == want.Typ && got.I == want.I && math.Float64bits(got.F) == math.Float64bits(want.F) &&
					string(got.S) == string(want.S) && len(got.S) == cap(got.S)
				if same && want.Typ == tvText && len(want.S) > 0 && &got.S[0] != &heap[bounds[row]] {
					same = false
				}
				if !same {
					t.Fatalf("dense %v col %d row %d: pc %d got %+v, want %+v", dense, c, row, pc, got, want)
				}
			}
		}
	}
	// The same instruction over a cursor with no segment reads its row list.
	rows := [][]tvalue{{{Typ: tvInt, I: 1}, {Typ: tvText, S: heap[:2]}}}
	regs := make([]tvalue, 2)
	cur := &VCursor{Rows: unsafe.Pointer(unsafe.SliceData(rows)), N: 1, Pos: 0, Last: -1, Ok: 1, Mask: 3}
	if pc := runVMCursor(t, []VInsn{{Op: VColumn, A: 0, B: 1, C: 0, Imm: 1}, {Op: VExit}}, regs, rows, cur, 0); pc != 1 ||
		regs[0].Typ != tvText || string(regs[0].S) != "ab" {
		t.Fatalf("row list: pc %d r0 %+v", pc, regs[0])
	}
}

// TestVMSeekRowid seeks keys across a dense segment and one searched by its
// rowid block, through VSeekRowid and VSeekRewind, and reads the found row's
// column in segment mode: a key no segment holds jumps to T.
func TestVMSeekRowid(t *testing.T) {
	if !Available || !VMSegCursor {
		t.Skip("no segment cursors on this platform")
	}
	noGC(t)
	vals0 := []uint64{10, 11, 12, 13}     // rowids 100..103, dense
	vals1 := []uint64{20, 21, 22, 23, 24} // rowids below
	rids1 := []uint64{200, 205, 300, 301, 1 << 40}
	cols0 := []VSegCol{{Kind: VSegInt, Block: unsafe.Pointer(&vals0[0])}}
	cols1 := []VSegCol{{Kind: VSegInt, Block: unsafe.Pointer(&vals1[0])}}
	seg0 := &VSeg{Cols: unsafe.Pointer(&cols0[0]), First: 100, Dense: 1}
	seg1 := &VSeg{Cols: unsafe.Pointer(&cols1[0]), Rowids: unsafe.Pointer(&rids1[0])}
	segs := []VTabSeg{
		{Lo: 100, Hi: 103, Rows: 4, Seg: unsafe.Pointer(seg0), Mask: 1},
		{Lo: 200, Hi: 1 << 40, Rows: 5, Seg: unsafe.Pointer(seg1), Mask: 1},
	}
	tab := &VTab{Segs: unsafe.Pointer(&segs[0]), N: 2}
	prog := []VInsn{
		{Op: VSeekRowid, A: 0, B: 0, T: 3},
		{Op: VSeekRewind, A: 0},
		{Op: VColumn, A: 0, B: 0, C: 1, Imm: 1},
		{Op: VExit},
	}
	want := map[int64]int64{100: 10, 103: 13, 200: 20, 205: 21, 300: 22, 301: 23, 1 << 40: 24}
	for _, key := range []int64{99, 100, 103, 104, 199, 200, 201, 205, 300, 301, 302, 1 << 40, -5} {
		regs := make([]tvalue, 2)
		regs[0] = tvalue{Typ: tvInt, I: key}
		regs[1] = tvalue{Typ: tvInt, I: -1}
		cur := &VCursor{Last: -1, Tab: unsafe.Pointer(tab)}
		pc := runVMCursor(t, prog, regs, nil, cur, 0)
		v, found := want[key]
		switch {
		case pc != 3:
			t.Fatalf("key %d: stopped at %d", key, pc)
		case found && (regs[1].Typ != tvInt || regs[1].I != v || cur.State != 4 || cur.Served != 1 || cur.Key != key):
			t.Fatalf("key %d: r1 %+v state %d served %d", key, regs[1], cur.State, cur.Served)
		case !found && (regs[1].I != -1 || cur.State != 3 || cur.Key != key):
			t.Fatalf("key %d (none): r1 %+v state %d", key, regs[1], cur.State)
		}
	}
	// A REAL key is the VDBE's.
	regs := []tvalue{{Typ: tvReal, F: 100}, {Typ: tvInt, I: -1}}
	if pc := runVMCursor(t, prog, regs, nil, &VCursor{Last: -1, Tab: unsafe.Pointer(tab)}, 0); pc != 0 {
		t.Fatalf("a REAL key ran natively to %d", pc)
	}
}

// TestVMSeekIdx seeks keys through a segment's equality index with VSeekIdx
// and VSeekRewind, walks the run with VNext reading a column in segment mode,
// and jumps to T for a key no row holds; a REAL key and a cursor with no index
// exit.
func TestVMSeekIdx(t *testing.T) {
	if !Available || !VMSegCursor {
		t.Skip("no segment cursors on this platform")
	}
	noGC(t)
	// Rows 0..7: key column 0, value column 1.
	keys := []uint64{7, 3, 7, 1, 3, 7, 9, 3}
	vals := []uint64{100, 101, 102, 103, 104, 105, 106, 107}
	cols := []VSegCol{{Kind: VSegInt, Block: unsafe.Pointer(&keys[0])}, {Kind: VSegInt, Block: unsafe.Pointer(&vals[0])}}
	seg := &VSeg{Cols: unsafe.Pointer(&cols[0]), First: 1, Dense: 1}
	ivals := []int64{1, 3, 7, 9}
	starts := []int32{0, 1, 4, 7, 8}
	rows := []int{3, 1, 4, 7, 0, 2, 5, 6}
	ix := &VIdx{Vals: unsafe.Pointer(&ivals[0]), NVals: 4, Starts: unsafe.Pointer(&starts[0]), Rows: unsafe.Pointer(&rows[0]),
		Seg: unsafe.Pointer(seg), Mask: 3}
	prog := []VInsn{
		{Op: VSeekIdx, A: 0, B: 0, T: 5},
		{Op: VSeekRewind, A: 0},
		{Op: VColumn, A: 0, B: 1, C: 1, Imm: 1},
		{Op: VNext, A: 0, T: 2},
		{Op: VExit},
		{Op: VExit},
	}
	want := map[int64][]int{1: {3}, 3: {1, 4, 7}, 7: {0, 2, 5}, 9: {6}}
	for _, key := range []int64{0, 1, 2, 3, 7, 8, 9, 10, -1} {
		regs := []tvalue{{Typ: tvInt, I: key}, {Typ: tvInt, I: -1}}
		cur := &VCursor{Last: -1, Idx: unsafe.Pointer(ix)}
		pc := runVMCursor(t, prog, regs, nil, cur, 0)
		run, found := want[key]
		switch {
		case !found:
			if pc != 5 || cur.State != 3 || cur.Key != key || regs[1].I != -1 {
				t.Fatalf("key %d (none): pc %d state %d r1 %+v", key, pc, cur.State, regs[1])
			}
		case pc != 4 || cur.State != 4 || cur.N != int64(len(run)) || cur.Last != int64(len(run)-1) ||
			regs[1].Typ != tvInt || regs[1].I != int64(vals[run[len(run)-1]]):
			t.Fatalf("key %d: pc %d state %d N %d last %d r1 %+v", key, pc, cur.State, cur.N, cur.Last, regs[1])
		default:
			sel := unsafe.Slice((*int)(cur.Sel), cur.N)
			for i, r := range run {
				if sel[i] != r {
					t.Fatalf("key %d: run %v, want %v", key, sel, run)
				}
			}
		}
	}
	regs := []tvalue{{Typ: tvReal, F: 3}, {Typ: tvInt, I: -1}}
	if pc := runVMCursor(t, prog, regs, nil, &VCursor{Last: -1, Idx: unsafe.Pointer(ix)}, 0); pc != 0 {
		t.Fatalf("a REAL key ran natively to %d", pc)
	}
	regs = []tvalue{{Typ: tvInt, I: 3}, {Typ: tvInt, I: -1}}
	if pc := runVMCursor(t, prog, regs, nil, &VCursor{Last: -1}, 0); pc != 0 {
		t.Fatalf("a cursor with no index ran natively to %d", pc)
	}
}

// TestVMTextOrder compares TEXTs with every ordered condition, as
// bytes.Compare orders them, and under a numeric affinity (Imm 1) exits when
// either is a whole number, which parseFullNumeric would convert.
func TestVMTextOrder(t *testing.T) {
	if !Available || !VMTextOrder {
		t.Skip("no ordered TEXT comparison on this platform")
	}
	noGC(t)
	words := []string{"", " ", "a", "ab", "abc", "b", "1995-03-15", "1995-03-16", "\xff", "12", " 12 ", "+1.5", "-.5",
		"1e5", "1E+5", "1e", "1e+", "5.", ".", "1x", "0x10", "\t7\n", "1.2.3", "e5", "+", ".e1", "9 9"}
	conds := []Cond{CondL, CondLE, CondG, CondGE, CondE, CondNE}
	for _, imm := range []int64{0, 1} {
		for _, x := range words {
			for _, y := range words {
				for _, cond := range conds {
					regs := make([]tvalue, 3)
					regs[0] = tvalue{Typ: tvText, S: []byte(y)} // A, the right operand
					regs[1] = tvalue{Typ: tvText, S: []byte(x)} // B, the left
					prog := []VInsn{{Op: VTextCmpSet, A: 0, B: 1, C: 2, Cond: cond, Imm: imm}, {Op: VExit}}
					pc := runVMCursor(t, prog, regs, nil, &VCursor{}, 0)
					if imm == 1 && (fullNumber(x) || fullNumber(y)) {
						if pc != 0 {
							t.Fatalf("%q vs %q: a whole number compared natively", x, y)
						}
						continue
					}
					c := 0
					switch {
					case x < y:
						c = -1
					case x > y:
						c = 1
					}
					want := map[Cond]bool{CondL: c < 0, CondLE: c <= 0, CondG: c > 0, CondGE: c >= 0, CondE: c == 0, CondNE: c != 0}[cond]
					if pc != 1 || regs[2].Typ != tvInt || (regs[2].I == 1) != want {
						t.Fatalf("imm %d: %q cond %x %q: pc %d r2 %+v, want %v", imm, x, cond, y, pc, regs[2], want)
					}
				}
			}
		}
	}
}

// fullNumber is the engine's parseFullNumeric, as a yes or no.
func fullNumber(s string) bool {
	sp := func(c byte) bool { return c == ' ' || c >= '\t' && c <= '\r' }
	dg := func(c byte) bool { return c >= '0' && c <= '9' }
	i, n := 0, len(s)
	for i < n && sp(s[i]) {
		i++
	}
	if i < n && (s[i] == '+' || s[i] == '-') {
		i++
	}
	d := 0
	for i < n && dg(s[i]) {
		i, d = i+1, d+1
	}
	if i < n && s[i] == '.' {
		i++
		for i < n && dg(s[i]) {
			i, d = i+1, d+1
		}
	}
	if d == 0 {
		return false
	}
	if i < n && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < n && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < n && dg(s[j]) {
			for j < n && dg(s[j]) {
				j++
			}
			i = j
		}
	}
	for i < n && sp(s[i]) {
		i++
	}
	return i == n
}

// TestVMText loads a constant TEXT, sharing its bytes with a capacity capped
// at the length, over a register that held a string.
func TestVMText(t *testing.T) {
	if !Available || !VMTextOrder {
		t.Skip("no constant TEXT on this platform")
	}
	noGC(t)
	str := "ECONOMY ANODIZED STEEL"
	regs := []tvalue{{Typ: tvText, S: []byte("old")}, {Typ: tvInt, I: 5}}
	prog := []VInsn{{Op: VText, A: 0, B: len(str), Imm: int64(uintptr(unsafe.Pointer(unsafe.StringData(str))))}, {Op: VExit}}
	pc := runVMCursor(t, prog, regs, nil, &VCursor{}, 0)
	got := regs[0]
	if pc != 1 || got.Typ != tvText || got.I != 0 || got.F != 0 || string(got.S) != str || len(got.S) != cap(got.S) ||
		unsafe.Pointer(&got.S[0]) != unsafe.Pointer(unsafe.StringData(str)) || regs[1].I != 5 {
		t.Fatalf("pc %d r0 %+v r1 %+v", pc, got, regs[1])
	}
}

// TestVMSegRowid reads the rowid of a cursor in segment mode: from the rowid
// block when there is one, else a dense segment's first plus the row; a
// cursor with neither exits.
func TestVMSegRowid(t *testing.T) {
	if !Available || !VMSegCursor {
		t.Skip("no segment cursors on this platform")
	}
	noGC(t)
	rids := []uint64{7, 9, 40}
	dense := &VSeg{First: 100, Dense: 1}
	sparse := &VSeg{Rowids: unsafe.Pointer(&rids[0])}
	for _, c := range []struct {
		seg  *VSeg
		rids unsafe.Pointer
		want int64 // -1: exits
	}{{dense, nil, 102}, {sparse, unsafe.Pointer(&rids[0]), 40}, {&VSeg{}, nil, -1}, {nil, nil, -1}} {
		regs := []tvalue{{Typ: tvInt, I: -9}}
		cur := &VCursor{N: 3, Pos: 2, Last: -1, Ok: 1, Rowids: c.rids, Seg: unsafe.Pointer(c.seg)}
		pc := runVMCursor(t, []VInsn{{Op: VRowid, A: 0, C: 0}, {Op: VExit}}, regs, nil, cur, 0)
		switch {
		case c.want < 0 && (pc != 0 || regs[0].I != -9):
			t.Fatalf("%+v: pc %d r0 %+v, want an exit", c, pc, regs[0])
		case c.want >= 0 && (pc != 1 || regs[0].Typ != tvInt || regs[0].I != c.want):
			t.Fatalf("%+v: pc %d r0 %+v, want %d", c, pc, regs[0], c.want)
		}
	}
}

// TestVMSegSel walks a cursor in segment mode through a selection (VCursor.Sel):
// each position reads its column and its rowid from the segment row the
// selection names, over a dense segment and one with a rowid block.
func TestVMSegSel(t *testing.T) {
	if !Available || !VMSegCursor {
		t.Skip("no segment cursors on this platform")
	}
	noGC(t)
	vals := []uint64{10, 11, 12, 13, 14, 15}
	rids := []uint64{100, 200, 300, 400, 500, 600}
	sel := []int{4, 0, 5, 2}
	cols := []VSegCol{{Kind: VSegInt, Block: unsafe.Pointer(&vals[0])}}
	// r0 += column 0, r1 += rowid, over every position.
	prog := []VInsn{
		{Op: VColumn, A: 0, B: 0, C: 2, Imm: 1},
		{Op: VAdd, A: 2, B: 0, C: 0},
		{Op: VRowid, A: 0, C: 3},
		{Op: VAdd, A: 3, B: 1, C: 1},
		{Op: VNext, A: 0, T: 0},
		{Op: VExit},
	}
	for _, dense := range []bool{true, false} {
		seg := &VSeg{Cols: unsafe.Pointer(&cols[0]), First: 1000, Rowids: unsafe.Pointer(&rids[0])}
		wantR := int64(0)
		for _, r := range sel {
			wantR += int64(rids[r])
		}
		cur := &VCursor{N: int64(len(sel)), Pos: 0, Last: -1, Ok: 1, Adv: 1, Mask: 1, Seg: unsafe.Pointer(seg), Rowids: seg.Rowids, Sel: unsafe.Pointer(&sel[0])}
		if dense {
			seg.Dense, cur.Rowids = 1, nil
			wantR = 0
			for _, r := range sel {
				wantR += 1000 + int64(r)
			}
		}
		regs := []tvalue{{Typ: tvInt}, {Typ: tvInt}, {}, {}}
		pc := runVMCursor(t, prog, regs, nil, cur, 0)
		if pc != len(prog)-1 || regs[0].I != 14+10+15+12 || regs[1].I != wantR || cur.Last != int64(len(sel)-1) {
			t.Fatalf("dense %v: pc %d sum %d rowids %d (want %d) last %d", dense, pc, regs[0].I, regs[1].I, wantR, cur.Last)
		}
	}
}

// TestVMSegColumnPredicted reads columns of a segment cursor in one run with
// a predicted kind (T): plain columns of that kind take the shared checks and
// the direct read, and a column with a NULL bitmap or exceptions, one of
// another kind, a row list, a NULL and an exception all read as without a
// prediction.
func TestVMSegColumnPredicted(t *testing.T) {
	if !Available || !VMSegCursor {
		t.Skip("no segment cursors on this platform")
	}
	noGC(t)
	ints := []uint64{5, 6, 7, 8}
	floats := []uint64{math.Float64bits(1.5), math.Float64bits(2.5), math.Float64bits(3.5), math.Float64bits(4.5)}
	nulls := []byte{1 << 2} // row 2 of column 2 is NULL
	exc := []uint64{1 << 3} // row 3 of column 3 is an exception
	cols := []VSegCol{
		{Kind: VSegInt, Block: unsafe.Pointer(&ints[0]), Plain: VSegInt},
		{Kind: VSegFloat, Block: unsafe.Pointer(&floats[0]), Plain: VSegFloat},
		{Kind: VSegInt, Block: unsafe.Pointer(&ints[0]), Nulls: unsafe.Pointer(&nulls[0])},
		{Kind: VSegInt, Block: unsafe.Pointer(&ints[0]), Exc: unsafe.Pointer(&exc[0])},
	}
	seg := &VSeg{Cols: unsafe.Pointer(&cols[0]), First: 1, Dense: 1}
	// Column 1 predicted INTEGER though it is REAL: the full read.
	prog := []VInsn{
		{Op: VColumn, A: 0, B: 0, C: 0, Imm: 1, T: VSegInt},
		{Op: VColumn, A: 0, B: 1, C: 1, Imm: 1, T: VSegInt},
		{Op: VColumn, A: 0, B: 2, C: 2, Imm: 1, T: VSegInt},
		{Op: VColumn, A: 0, B: 3, C: 3, Imm: 1, T: VSegInt},
		{Op: VColumn, A: 0, B: 0, C: 4, Imm: 1, T: VSegInt},
		{Op: VExit},
	}
	lay := ValueLayout{Size: 48, OffTyp: 0, OffI: 8, OffF: 16, OffS: 24, TypNull: tvNull, TypInt: tvInt, TypFloat: tvReal, TypText: tvText}
	entry := make([]bool, len(prog))
	entry[0] = true
	b, err := EmitVMFused(prog, 5, lay, entry, false)
	if err != nil {
		t.Fatal(err)
	}
	code, err := Map(b)
	if err != nil {
		t.Fatal(err)
	}
	defer code.Close()
	for row := range 4 {
		regs := make([]tvalue, 5)
		for i := range regs {
			regs[i] = tvalue{Typ: tvInt, I: -1}
		}
		cur := &VCursor{N: 4, Pos: int64(row), Last: -1, Ok: 1, Mask: 15, Seg: unsafe.Pointer(seg)}
		args := ProgArgs{Regs: (*int64)(unsafe.Pointer(&regs[0])), Cur: cur}
		code.Call2(&args)
		want := []tvalue{{Typ: tvInt, I: int64(ints[row])}, {Typ: tvReal, F: math.Float64frombits(floats[row])},
			{Typ: tvInt, I: int64(ints[row])}, {Typ: tvInt, I: int64(ints[row])}, {Typ: tvInt, I: int64(ints[row])}}
		stop := 5
		switch row {
		case 2:
			stop = 2 // the NULL exits at column 2
		case 3:
			stop = 3 // the exception exits at column 3
		}
		if int(args.PC) != stop {
			t.Fatalf("row %d: stopped at %d, want %d", row, args.PC, stop)
		}
		for i := range stop {
			if !same(regs[i], want[i]) {
				t.Fatalf("row %d r%d: %+v, want %+v", row, i, regs[i], want[i])
			}
		}
	}
	// A row list under the same predicted reads.
	rows := [][]tvalue{{{Typ: tvInt, I: 1}, {Typ: tvReal, F: 2}, {Typ: tvInt, I: 3}, {Typ: tvInt, I: 4}}}
	regs := make([]tvalue, 5)
	cur := &VCursor{Rows: unsafe.Pointer(unsafe.SliceData(rows)), N: 1, Pos: 0, Last: -1, Ok: 1, Mask: 15}
	args := ProgArgs{Regs: (*int64)(unsafe.Pointer(&regs[0])), Cur: cur}
	code.Call2(&args)
	if args.PC != 5 || regs[0].I != 1 || regs[1].F != 2 || regs[2].I != 3 || regs[3].I != 4 || regs[4].I != 1 {
		t.Fatalf("row list: pc %d regs %+v", args.PC, regs)
	}
}
