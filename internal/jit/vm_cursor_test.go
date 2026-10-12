//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"testing"
	"unsafe"
)

// tvalue is a Value as the engine lays it out (48 bytes: the type and subtype
// in the first word, then I, F and the S slice header).
type tvalue struct {
	Typ uint8
	Sub uint8
	_   [6]byte
	I   int64
	F   float64
	S   []byte
}

const (
	tvNull = 0
	tvInt  = 1
	tvReal = 2
	tvText = 3
)

func TestVCursorLayout(t *testing.T) {
	var c VCursor
	for _, f := range []struct {
		name      string
		got, want uintptr
	}{
		{"size", unsafe.Sizeof(c), VCursorSize},
		{"Rows", unsafe.Offsetof(c.Rows), VCursorOffRows},
		{"N", unsafe.Offsetof(c.N), VCursorOffN},
		{"Pos", unsafe.Offsetof(c.Pos), VCursorOffPos},
		{"Last", unsafe.Offsetof(c.Last), VCursorOffLast},
		{"Ok", unsafe.Offsetof(c.Ok), VCursorOffOk},
		{"Dirty", unsafe.Offsetof(c.Dirty), VCursorOffDirty},
		{"Rewind", unsafe.Offsetof(c.Rewind), VCursorOffRew},
		{"Adv", unsafe.Offsetof(c.Adv), VCursorOffAdv},
		{"Mask", unsafe.Offsetof(c.Mask), VCursorOffMask},
	} {
		if f.got != f.want {
			t.Errorf("VCursor %s is %d, generated code uses %d", f.name, f.got, f.want)
		}
	}
	var v tvalue
	if unsafe.Sizeof(v) != 48 || unsafe.Offsetof(v.I) != 8 || unsafe.Offsetof(v.F) != 16 || unsafe.Offsetof(v.S) != 24 {
		t.Fatalf("the test's Value is laid out differently from the engine's")
	}
	if unsafe.Sizeof([]tvalue(nil)) != sliceHeaderSize {
		t.Fatalf("a slice header is %d bytes, generated code uses %d", unsafe.Sizeof([]tvalue(nil)), sliceHeaderSize)
	}
}

// runVMCursor runs prog over one cursor and returns where native code stopped.
func runVMCursor(t *testing.T, prog []VInsn, regs []tvalue, rows [][]tvalue, cur *VCursor, pc int) int {
	t.Helper()
	lay := ValueLayout{Size: 48, OffTyp: 0, OffI: 8, OffF: 16, OffS: 24, TypNull: tvNull, TypInt: tvInt}
	b, err := EmitVM(prog, len(regs), lay)
	if err != nil {
		t.Fatalf("EmitVM: %v", err)
	}
	code, err := Map(b)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	defer code.Close()
	args := ProgArgs{Regs: (*int64)(unsafe.Pointer(&regs[0])), PC: int64(pc), Cur: cur}
	code.Call2(&args)
	return int(args.PC)
}

func TestVMCursorColumnAndNext(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	// r0, r1 = the row's columns 0 and 1; r2 += r0 + r1 (r2 starts at 0).
	prog := []VInsn{
		{Op: VColumn, A: 0, B: 0, C: 0},
		{Op: VColumn, A: 0, B: 1, C: 1},
		{Op: VAdd, A: 0, B: 1, C: 3},
		{Op: VAdd, A: 3, B: 2, C: 2},
		{Op: VNext, A: 0, T: 0},
		{Op: VExit},
	}
	newRows := func(n int) [][]tvalue {
		rows := make([][]tvalue, n)
		for i := range rows {
			rows[i] = []tvalue{{Typ: tvInt, I: int64(i)}, {Typ: tvInt, I: 10 * int64(i)}, {Typ: tvNull}}
		}
		return rows
	}
	cursor := func(rows [][]tvalue, pos int64) *VCursor {
		return &VCursor{Rows: unsafe.Pointer(unsafe.SliceData(rows)), N: int64(len(rows)), Pos: pos, Last: -1, Ok: 1, Rewind: 1, Adv: 1, Mask: ^uint64(0)}
	}
	newRegs := func() []tvalue {
		regs := make([]tvalue, 4)
		regs[2] = tvalue{Typ: tvInt}
		return regs
	}

	t.Run("a scan to the end", func(t *testing.T) {
		rows := newRows(100)
		regs, cur := newRegs(), cursor(rows, 0)
		if pc := runVMCursor(t, prog, regs, rows, cur, 0); pc != 5 {
			t.Fatalf("stopped at %d, want 5 (off the end)", pc)
		}
		if want := int64(11 * 99 * 100 / 2); regs[2].I != want {
			t.Fatalf("sum %d, want %d", regs[2].I, want)
		}
		if cur.Pos != 100 || cur.Last != 99 || cur.Dirty != 1 {
			t.Fatalf("cursor left at pos %d last %d dirty %d, want 100, 99, 1", cur.Pos, cur.Last, cur.Dirty)
		}
	})
	t.Run("the column is copied whole", func(t *testing.T) {
		rows := [][]tvalue{{{Typ: tvReal, F: 2.5, Sub: 74}, {Typ: tvNull}}}
		regs := newRegs()
		regs[0].I = 99 // stale words of the destination are overwritten
		cur := cursor(rows, 0)
		pc := runVMCursor(t, []VInsn{{Op: VColumn, A: 0, B: 0, C: 0}, {Op: VExit}}, regs, rows, cur, 0)
		if pc != 1 || regs[0].Typ != tvReal || regs[0].F != 2.5 || regs[0].I != 0 || regs[0].Sub != 74 {
			t.Fatalf("pc %d, r0 = %+v", pc, regs[0])
		}
		if cur.Dirty != 0 {
			t.Fatalf("a read dirtied the cursor")
		}
	})
	t.Run("what exits leaves the registers alone", func(t *testing.T) {
		text := []byte("abc")
		cases := []struct {
			name string
			rows [][]tvalue
			pos  int64
			ok   int64
			col  int
		}{
			{"a text value", [][]tvalue{{{Typ: tvText, S: text}}}, 0, 1, 0},
			{"a column past the row", [][]tvalue{{{Typ: tvInt}}}, 0, 1, 1},
			{"before the first row", [][]tvalue{{{Typ: tvInt}}}, -1, 1, 0},
			{"past the last row", [][]tvalue{{{Typ: tvInt}}}, 1, 1, 0},
			{"a cursor native code may not read", [][]tvalue{{{Typ: tvInt}}}, 0, 0, 0},
		}
		for _, c := range cases {
			regs := newRegs()
			regs[0] = tvalue{Typ: tvInt, I: 7}
			cur := cursor(c.rows, c.pos)
			cur.Ok = c.ok
			pc := runVMCursor(t, []VInsn{{Op: VColumn, A: 0, B: c.col, C: 0}, {Op: VExit}}, regs, c.rows, cur, 0)
			if pc != 0 || regs[0].Typ != tvInt || regs[0].I != 7 {
				t.Errorf("%s: pc %d, r0 = %+v", c.name, pc, regs[0])
			}
		}
	})
	t.Run("a column outside the mask exits", func(t *testing.T) {
		rows := newRows(2)
		regs := newRegs()
		regs[0] = tvalue{Typ: tvInt, I: 7}
		cur := cursor(rows, 0)
		cur.Mask = 1 << 1 // column 1 only
		prog := []VInsn{{Op: VColumn, A: 0, B: 0, C: 0}, {Op: VColumn, A: 0, B: 1, C: 1}, {Op: VExit}}
		if pc := runVMCursor(t, prog, regs, rows, cur, 0); pc != 0 || regs[0].I != 7 {
			t.Fatalf("column 0, outside the mask: pc %d r0 %+v", pc, regs[0])
		}
		if pc := runVMCursor(t, prog, regs, rows, cur, 1); pc != 2 || regs[1].I != 0 {
			t.Fatalf("column 1, in the mask: pc %d r1 %+v", pc, regs[1])
		}
	})
	t.Run("next on a cursor native code may read but not move", func(t *testing.T) {
		rows := newRows(2)
		cur := cursor(rows, 0)
		cur.Adv = 0
		pc := runVMCursor(t, []VInsn{{Op: VNext, A: 0, T: 2}, {Op: VExit}, {Op: VExit}}, newRegs(), rows, cur, 0)
		if pc != 0 || cur.Pos != 0 || cur.Dirty != 0 {
			t.Fatalf("pc %d pos %d dirty %d, want the cursor untouched at pc 0", pc, cur.Pos, cur.Dirty)
		}
	})
	t.Run("a destination holding a string is not written", func(t *testing.T) {
		rows := newRows(1)
		regs := newRegs()
		regs[0] = tvalue{Typ: tvText, S: []byte("x")}
		cur := cursor(rows, 0)
		if pc := runVMCursor(t, []VInsn{{Op: VColumn, A: 0, B: 0, C: 0}, {Op: VExit}}, regs, rows, cur, 0); pc != 0 {
			t.Fatalf("pc %d", pc)
		}
		if string(regs[0].S) != "x" {
			t.Fatalf("r0 = %+v", regs[0])
		}
	})
	t.Run("next from before the first row", func(t *testing.T) {
		rows := newRows(3)
		cur := cursor(rows, -1)
		pc := runVMCursor(t, []VInsn{{Op: VNext, A: 0, T: 2}, {Op: VExit}, {Op: VExit}}, newRegs(), rows, cur, 0)
		if pc != 2 || cur.Pos != 0 || cur.Last != 0 {
			t.Fatalf("pc %d pos %d last %d, want 2, 0, 0", pc, cur.Pos, cur.Last)
		}
	})
	t.Run("next off the end, repeatedly", func(t *testing.T) {
		rows := newRows(2)
		cur := cursor(rows, 1)
		pc := runVMCursor(t, []VInsn{{Op: VNext, A: 0, T: 2}, {Op: VExit}, {Op: VExit}}, newRegs(), rows, cur, 0)
		if pc != 1 || cur.Pos != 2 || cur.Last != -1 || cur.Dirty != 1 {
			t.Fatalf("pc %d pos %d last %d dirty %d, want 1, 2, -1, 1", pc, cur.Pos, cur.Last, cur.Dirty)
		}
		pc = runVMCursor(t, []VInsn{{Op: VNext, A: 0, T: 2}, {Op: VExit}, {Op: VExit}}, newRegs(), rows, cur, 0)
		if pc != 1 || cur.Pos != 3 {
			t.Fatalf("pc %d pos %d, want 1, 3", pc, cur.Pos)
		}
	})
	t.Run("next on a cursor native code may not advance", func(t *testing.T) {
		rows := newRows(2)
		cur := cursor(rows, 0)
		cur.Ok = 0
		pc := runVMCursor(t, []VInsn{{Op: VNext, A: 0, T: 2}, {Op: VExit}, {Op: VExit}}, newRegs(), rows, cur, 0)
		if pc != 0 || cur.Pos != 0 || cur.Dirty != 0 {
			t.Fatalf("pc %d pos %d dirty %d, want the cursor untouched at pc 0", pc, cur.Pos, cur.Dirty)
		}
	})
	t.Run("the second cursor of a table", func(t *testing.T) {
		rows := newRows(4)
		curs := []VCursor{{}, *cursor(rows, 0)}
		regs := newRegs()
		pc := runVMCursor(t, []VInsn{{Op: VColumn, A: 1, B: 1, C: 0}, {Op: VNext, A: 1, T: 0}, {Op: VExit}}, regs, rows, &curs[0], 0)
		// curs[0] is unusable and unreferenced; cursor 1 runs the scan.
		if pc != 2 || curs[1].Pos != 4 || regs[0].I != 30 {
			t.Fatalf("pc %d pos %d r0 %d, want 2, 4, 30", pc, curs[1].Pos, regs[0].I)
		}
	})
	t.Run("rewind", func(t *testing.T) {
		prog := []VInsn{{Op: VRewind, A: 0, T: 2}, {Op: VExit}, {Op: VExit}}
		rows := newRows(3)
		cur := cursor(rows, 2)
		if pc := runVMCursor(t, prog, newRegs(), rows, cur, 0); pc != 1 || cur.Pos != 0 || cur.Last != 0 || cur.Dirty != 1 {
			t.Fatalf("pc %d pos %d last %d dirty %d, want 1, 0, 0, 1", pc, cur.Pos, cur.Last, cur.Dirty)
		}
		empty := newRows(0)
		cur = cursor(empty, 5)
		cur.Rows = nil
		if pc := runVMCursor(t, prog, newRegs(), empty, cur, 0); pc != 2 || cur.Pos != 0 || cur.Last != -1 || cur.Dirty != 1 {
			t.Fatalf("empty: pc %d pos %d last %d dirty %d, want 2, 0, -1, 1", pc, cur.Pos, cur.Last, cur.Dirty)
		}
		cur = cursor(rows, 1)
		cur.Ok = 0
		if pc := runVMCursor(t, prog, newRegs(), rows, cur, 0); pc != 0 || cur.Pos != 1 || cur.Dirty != 0 {
			t.Fatalf("unusable: pc %d pos %d dirty %d, want the cursor untouched at pc 0", pc, cur.Pos, cur.Dirty)
		}
		cur = cursor(rows, 1)
		cur.Rewind = 0 // readable and advanceable, but a rewind builds rows
		if pc := runVMCursor(t, prog, newRegs(), rows, cur, 0); pc != 0 || cur.Pos != 1 || cur.Dirty != 0 {
			t.Fatalf("re-seeking: pc %d pos %d dirty %d, want the cursor untouched at pc 0", pc, cur.Pos, cur.Dirty)
		}
	})
}
