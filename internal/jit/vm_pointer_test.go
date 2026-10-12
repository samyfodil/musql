//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"testing"
	"unsafe"
)

var tvLayout = ValueLayout{Size: 48, OffTyp: 0, OffI: 8, OffF: 16, OffS: 24, TypNull: tvNull, TypInt: tvInt}

// runVM compiles prog (fused, entered at the pcs entry marks; every pc when
// entry is nil) and runs it from pc over regs.
func runVM(t *testing.T, prog []VInsn, entry []bool, regs []tvalue, args *ProgArgs, pc int) {
	t.Helper()
	var b []byte
	var err error
	if entry == nil {
		b, err = EmitVM(prog, len(regs), tvLayout)
	} else {
		b, err = EmitVMFused(prog, len(regs), tvLayout, entry, true)
	}
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	code, err := Map(b)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	defer code.Close()
	args.Regs, args.PC = (*int64)(unsafe.Pointer(&regs[0])), int64(pc)
	code.Call2(args)
}

// TestVMPointerStores: each store that writes or overwrites a string pointer,
// with the write barrier off (made natively) and on (an exit that leaves every
// register as it was).
func TestVMPointerStores(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	text, blob := []byte("hello"), []byte{1, 2}
	cases := []struct {
		name string
		in   VInsn
		want func(regs []tvalue) bool
	}{
		{"a shallow copy of text", VInsn{Op: VCopy, A: 0, B: 2, Imm: 1}, func(r []tvalue) bool {
			return r[2].Typ == tvText && r[2].Sub == 74 && &r[2].S[0] == &text[0] && len(r[2].S) == 5
		}},
		{"a shallow copy over text", VInsn{Op: VCopy, A: 3, B: 1, Imm: 1}, func(r []tvalue) bool {
			return r[1].Typ == tvInt && r[1].I == 9 && r[1].S == nil && cap(r[1].S) == 0
		}},
		{"a shallow copy of text over a blob", VInsn{Op: VCopy, A: 0, B: 1, Imm: 1}, func(r []tvalue) bool {
			return r[1].Typ == tvText && &r[1].S[0] == &text[0]
		}},
		{"an integer over text", VInsn{Op: VInt, A: 0, Imm: 4}, func(r []tvalue) bool {
			return r[0].Typ == tvInt && r[0].Sub == 0 && r[0].I == 4 && r[0].S == nil && cap(r[0].S) == 0
		}},
		{"a NULL over a blob", VInsn{Op: VNull, C: 1}, func(r []tvalue) bool {
			return r[1].Typ == tvNull && r[1].S == nil && cap(r[1].S) == 0
		}},
		{"a sum over text", VInsn{Op: VAdd, A: 3, B: 3, C: 0}, func(r []tvalue) bool {
			return r[0].Typ == tvInt && r[0].I == 18 && r[0].S == nil
		}},
		{"a comparison over a blob", VInsn{Op: VCmpSet, A: 3, B: 3, C: 1, Cond: CondE}, func(r []tvalue) bool {
			return r[1].Typ == tvInt && r[1].I == 1 && r[1].S == nil
		}},
	}
	fresh := func() []tvalue {
		return []tvalue{{Typ: tvText, Sub: 74, S: text}, {Typ: tvBlob, S: blob}, {Typ: tvNull}, {Typ: tvInt, I: 9}}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			noGC(t)
			regs := fresh()
			runVM(t, []VInsn{c.in, {Op: VExit}}, nil, regs, &ProgArgs{}, 0)
			if !c.want(regs) {
				t.Fatalf("barrier off: regs %+v", regs)
			}
		})
		t.Run(c.name+", barrier on", func(t *testing.T) {
			defer ForceWriteBarrierOnForTest()()
			regs := fresh()
			var args ProgArgs
			runVM(t, []VInsn{c.in, {Op: VExit}}, nil, regs, &args, 0)
			if args.PC != 0 {
				t.Fatalf("ran natively to pc %d", args.PC)
			}
			for i, r := range fresh() {
				if regs[i].Typ != r.Typ || regs[i].I != r.I || string(regs[i].S) != string(r.S) {
					t.Fatalf("an exit changed r%d: %+v", i, regs[i])
				}
			}
		})
	}
}

const tvBlob = 4

// TestVMResultRow: the row copied into the buffer, the return to Go after it,
// and the exits -- too little room, the engine's refusal, the barrier on.
func TestVMResultRow(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	text := []byte("t")
	prog := []VInsn{{Op: VInt, A: 0, Imm: 5}, {Op: VResultRow, A: 0, B: 3}, {Op: VExit}}
	newRegs := func() []tvalue { return []tvalue{{}, {Typ: tvText, S: text}, {Typ: tvNull}, {Typ: tvInt, I: -1}} }
	for _, c := range []struct {
		name   string
		cap    int64
		native bool
	}{{"room", 3, true}, {"more room", 8, true}, {"too little", 2, false}, {"refused", -1, false}} {
		regs := newRegs()
		buf := make([]tvalue, 8)
		args := ProgArgs{RowCap: c.cap, RowBuf: (*int64)(unsafe.Pointer(&buf[0]))}
		runVM(t, prog, nil, regs, &args, 0)
		if !c.native {
			if args.PC != 1 || args.Yield != 0 || buf[0].Typ != 0 {
				t.Fatalf("%s: pc %d yield %d", c.name, args.PC, args.Yield)
			}
			continue
		}
		if args.PC != 2 || args.Yield != 4 {
			t.Fatalf("%s: pc %d yield %d, want 2 and 4", c.name, args.PC, args.Yield)
		}
		if buf[0].Typ != tvInt || buf[0].I != 5 || buf[1].Typ != tvText || &buf[1].S[0] != &text[0] || buf[2].Typ != tvNull || buf[3].Typ != 0 {
			t.Fatalf("%s: row %+v", c.name, buf[:4])
		}
	}
	// A row of no registers needs no room.
	regs := newRegs()
	args := ProgArgs{}
	runVM(t, []VInsn{{Op: VResultRow, A: 0, B: 0}}, nil, regs, &args, 0)
	if args.PC != 1 || args.Yield != 1 {
		t.Fatalf("an empty row: pc %d yield %d", args.PC, args.Yield)
	}
	t.Run("barrier on", func(t *testing.T) {
		defer ForceWriteBarrierOnForTest()()
		buf := make([]tvalue, 8)
		args := ProgArgs{RowCap: 8, RowBuf: (*int64)(unsafe.Pointer(&buf[0]))}
		runVM(t, prog, nil, newRegs(), &args, 1)
		if args.PC != 1 || args.Yield != 0 {
			t.Fatalf("pc %d yield %d", args.PC, args.Yield)
		}
	})
}

// TestVMResultRowGathered: a loop that emits a row per iteration goes on
// natively after each, writing the rows and their headers until one of the
// buffers is full, and then exits at the OP_ResultRow with what it wrote
// accounted for.
func TestVMResultRowGathered(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	text := []byte("t")
	// r0 = i; r2 = r1 (text); row(r0, r2); i++; if i < 10 goto 1
	prog := []VInsn{
		{Op: VInt, A: 0, Imm: 0},
		{Op: VCopy, A: 1, B: 2, Imm: 1},
		{Op: VResultRow, A: 0, B: 3},
		{Op: VAdd, A: 3, B: 0, C: 0},
		{Op: VCmpJump, A: 4, B: 0, T: 1, Cond: CondL},
		{Op: VExit},
	}
	for _, c := range []struct {
		name        string
		values, hdr int
		rows, pc    int
	}{
		{"room for all", 30, 10, 10, 5},
		{"values run out", 3 * 4, 10, 4, 2},
		{"headers run out", 30, 7, 7, 2},
		{"no headers", 30, 0, 0, 2},
	} {
		regs := []tvalue{{}, {Typ: tvText, S: text}, {}, {Typ: tvInt, I: 1}, {Typ: tvInt, I: 10}}
		vals := make([]tvalue, c.values+1)
		hdrs := make([][]tvalue, c.hdr+1)
		args := ProgArgs{RowCap: int64(c.values), RowBuf: (*int64)(unsafe.Pointer(&vals[0])),
			RowsBuf: (*int64)(unsafe.Pointer(&hdrs[0])), RowsCap: int64(c.hdr)}
		runVM(t, prog, nil, regs, &args, 0)
		if int(args.Yield) != c.rows || int(args.RowUsed) != 3*c.rows || int(args.PC) != c.pc {
			t.Fatalf("%s: %d rows, %d values, stopped at %d; want %d, %d, %d", c.name, args.Yield, args.RowUsed, args.PC, c.rows, 3*c.rows, c.pc)
		}
		for i, h := range hdrs[:c.rows] {
			if len(h) != 3 || cap(h) != 3 || &h[0] != &vals[3*i] || h[0].I != int64(i) || &h[2].S[0] != &text[0] {
				t.Fatalf("%s: row %d = %+v", c.name, i, h)
			}
		}
		if hdrs[c.rows] != nil || vals[3*c.rows].Typ != 0 {
			t.Fatalf("%s: wrote past the rows it reported", c.name)
		}
	}
}

// TestVMFusedPairEntry: an entry at the second pc of a fused compare-and-test
// runs the test alone, on the register the VDBE stored, and goes on from
// there; an entry at the first runs the pair.
func TestVMFusedPairEntry(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	// r2 = r0 < r1; if !r2 goto 4; r3 = 1; exit; r3 = 2; exit
	prog := []VInsn{
		{Op: VCmpSet, A: 1, B: 0, C: 2, Cond: CondL},
		{Op: VIf, A: 2, T: 4, Imm: 0},
		{Op: VInt, A: 3, Imm: 1},
		{Op: VExit},
		{Op: VInt, A: 3, Imm: 2},
		{Op: VExit},
	}
	entry := []bool{true, true, true, false, true, false}
	run := func(r0, r1 int64, r2 tvalue, pc int) ([]tvalue, int64) {
		regs := []tvalue{{Typ: tvInt, I: r0}, {Typ: tvInt, I: r1}, r2, {Typ: tvInt}}
		var args ProgArgs
		runVM(t, append([]VInsn(nil), prog...), append([]bool(nil), entry...), regs, &args, pc)
		return regs, args.PC
	}
	for _, c := range []struct {
		r0, r1 int64
		r2     tvalue
		pc     int
		want   int64
	}{
		{1, 2, tvalue{}, 0, 1},                             // the pair: 1 < 2
		{3, 2, tvalue{}, 0, 2},                             // the pair: 3 >= 2
		{3, 2, tvalue{Typ: tvInt, I: 1}, 1, 1},             // the test alone, on r2 = 1
		{1, 2, tvalue{Typ: tvInt, I: 0}, 1, 2},             // the test alone, on r2 = 0
		{1, 2, tvalue{Typ: tvNull}, 1, 1},                  // NULL with C = 0 falls through
		{1, 2, tvalue{Typ: tvText, S: []byte("1")}, 1, -1}, // text is the VDBE's
	} {
		regs, pc := run(c.r0, c.r1, c.r2, c.pc)
		if c.want < 0 {
			if pc != 1 {
				t.Errorf("entry %d on %+v: native code ran to %d, want an exit at 1", c.pc, c.r2, pc)
			}
			continue
		}
		if regs[3].I != c.want {
			t.Errorf("entry %d, r0 %d r1 %d r2 %+v: r3 = %d, want %d (stopped at %d)", c.pc, c.r0, c.r1, c.r2, regs[3].I, c.want, pc)
		}
	}
	pairs, _ := VMFusionStatsForTest()
	if pairs == 0 {
		t.Fatalf("the pair was not fused: the test checks nothing")
	}
}

// TestVMPointerStoresFar: the pointer stores on registers too far for an
// immediate offset (arm64 reaches 32KB, 682 registers), where a load or store
// goes through a scratch register of its own.
func TestVMPointerStoresFar(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	const base = 3000
	text, other := []byte("far"), []byte("x")
	regs := make([]tvalue, base+8)
	regs[base] = tvalue{Typ: tvText, S: text}
	regs[base+1] = tvalue{Typ: tvText, S: other}
	regs[base+2] = tvalue{Typ: tvText, S: other}
	regs[base+3] = tvalue{Typ: tvText, S: other}
	rows := [][]tvalue{{{Typ: tvText, S: text}}}
	cur := VCursor{Rows: unsafe.Pointer(unsafe.SliceData(rows)), N: 1, Pos: 0, Last: -1, Ok: 1, Mask: ^uint64(0)}
	prog := []VInsn{
		{Op: VCopy, A: base, B: base + 1, Imm: 1}, // text over text
		{Op: VColumn, A: 0, B: 0, C: base + 2},    // a text column over text
		{Op: VInt, A: base + 3, Imm: 7},           // an integer over text
		{Op: VExit},
	}
	args := ProgArgs{Cur: &cur}
	runVM(t, prog, nil, regs, &args, 0)
	if args.PC != 3 {
		t.Fatalf("stopped at %d", args.PC)
	}
	for _, r := range []int{base + 1, base + 2} {
		if regs[r].Typ != tvText || &regs[r].S[0] != &text[0] || len(regs[r].S) != 3 {
			t.Fatalf("r%d = %+v", r, regs[r])
		}
	}
	if regs[base+3].Typ != tvInt || regs[base+3].I != 7 || regs[base+3].S != nil || cap(regs[base+3].S) != 0 {
		t.Fatalf("r%d = %+v", base+3, regs[base+3])
	}
}
