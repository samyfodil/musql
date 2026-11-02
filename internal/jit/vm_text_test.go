//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"strings"
	"testing"
	"unsafe"
)

// tvASCII is VSubstr's table, a package variable as the engine's is: its
// address is in the code, and only a uintptr holds it there, so a local one
// could be on a stack that moves.
var tvASCII = func() (b [128]byte) {
	for i := range b {
		b[i] = byte(i)
	}
	return
}()

// TestVMSubstr: the one-character substrings native code answers, from the
// table, and every argument it leaves to the engine with the registers as
// they were -- as it does with the write barrier on.
func TestVMSubstr(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	table := &tvASCII
	long := strings.Repeat("abcdefgh", 10) + "z"
	cases := []struct {
		s      tvalue
		y, n   tvalue
		native bool
	}{
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvInt, I: 1}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvInt, I: 5}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte(long)}, tvalue{Typ: tvInt, I: 81}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte(long)}, tvalue{Typ: tvInt, I: 8}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte(long)}, tvalue{Typ: tvInt, I: 9}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte(long)}, tvalue{Typ: tvInt, I: 17}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte("ab\x00cd")}, tvalue{Typ: tvInt, I: 2}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte("abcdefghij\xc3\xa9")}, tvalue{Typ: tvInt, I: 10}, tvalue{Typ: tvInt, I: 1}, true},
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvInt, I: 6}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvInt, I: 0}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvInt, I: -1}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvInt, I: 2}, tvalue{Typ: tvInt, I: 2}, false},
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvReal, F: 2}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("hello")}, tvalue{Typ: tvInt, I: 2}, tvalue{Typ: tvNull}, false},
		{tvalue{Typ: tvText, S: []byte("ab\x00cd")}, tvalue{Typ: tvInt, I: 3}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("ab\x00cd")}, tvalue{Typ: tvInt, I: 4}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("abcdefghij\xc3\xa9")}, tvalue{Typ: tvInt, I: 11}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("abcdefgh\xc3\xa9ij")}, tvalue{Typ: tvInt, I: 11}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText, S: []byte("\xc3\xa9abcdefghij")}, tvalue{Typ: tvInt, I: 3}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvText}, tvalue{Typ: tvInt, I: 1}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvBlob, S: []byte("hello")}, tvalue{Typ: tvInt, I: 1}, tvalue{Typ: tvInt, I: 1}, false},
		{tvalue{Typ: tvNull}, tvalue{Typ: tvInt, I: 1}, tvalue{Typ: tvInt, I: 1}, false},
	}
	prog := []VInsn{{Op: VSubstr, A: 0, C: 3, Imm: int64(uintptr(unsafe.Pointer(&table[0])))}, {Op: VExit}}
	old := []byte("old")
	for k, c := range cases {
		for _, wb := range []bool{false, true} {
			regs := []tvalue{c.s, c.y, c.n, {Typ: tvText, S: old}}
			var args ProgArgs
			func() {
				if wb {
					defer ForceWriteBarrierOnForTest()()
				}
				runVM(t, prog, nil, regs, &args, 0)
			}()
			if !c.native || wb {
				if args.PC != 0 || regs[3].Typ != tvText || &regs[3].S[0] != &old[0] {
					t.Fatalf("case %d (barrier on %v): pc %d, r3 %+v; want an exit at 0", k, wb, args.PC, regs[3])
				}
				continue
			}
			want := c.s.S[c.y.I-1]
			r := regs[3]
			if args.PC != 1 || r.Typ != tvText || r.Sub != 0 || r.I != 0 || r.F != 0 || len(r.S) != 1 || cap(r.S) != 1 || &r.S[0] != &table[want] {
				t.Fatalf("case %d: pc %d, r3 %+v (cap %d); want %q from the table", k, args.PC, r, cap(r.S), want)
			}
		}
	}
}

// TestVMTextCmp: = and <> over two TEXTs and two INTEGERs, in both forms,
// and the pairs that exit: mixed, NULL, BLOB, and two TEXTs in a UTF-16
// database.
func TestVMTextCmp(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	text := func(s string) tvalue { return tvalue{Typ: tvText, S: []byte(s)} }
	cases := []struct {
		a, b  tvalue
		utf16 bool
		eq    int // 1 equal, 0 not, -1 an exit
	}{
		{text("abc"), text("abc"), false, 1},
		{text("abc"), text("abd"), false, 0},
		{text("abc"), text("ab"), false, 0},
		{text(""), text(""), false, 1},
		{tvalue{Typ: tvText}, text(""), false, 1},
		{text("5"), text("5"), false, 1},
		{text("\xff"), text("\xef\xbf\xbd"), false, 0},
		{text(strings.Repeat("x", 100)), text(strings.Repeat("x", 100)), false, 1},
		{text(strings.Repeat("x", 99) + "y"), text(strings.Repeat("x", 100)), false, 0},
		{tvalue{Typ: tvInt, I: 5}, tvalue{Typ: tvInt, I: 5}, false, 1},
		{tvalue{Typ: tvInt, I: 5}, tvalue{Typ: tvInt, I: -5}, false, 0},
		{tvalue{Typ: tvInt, I: 5}, tvalue{Typ: tvInt, I: 5}, true, 1},
		{text("abc"), text("abc"), true, -1},
		{text("5"), tvalue{Typ: tvInt, I: 5}, false, -1},
		{tvalue{Typ: tvInt, I: 5}, text("5"), false, -1},
		{text("a"), tvalue{Typ: tvNull}, false, -1},
		{tvalue{Typ: tvNull}, tvalue{Typ: tvNull}, false, -1},
		{tvalue{Typ: tvBlob, S: []byte("a")}, tvalue{Typ: tvBlob, S: []byte("a")}, false, -1},
		{tvalue{Typ: tvReal, F: 1}, tvalue{Typ: tvReal, F: 1}, false, -1},
	}
	for k, c := range cases {
		for _, cond := range []Cond{CondE, CondNE} {
			want := c.eq
			if want >= 0 && cond == CondNE {
				want = 1 - want
			}
			var utf16 int64
			if c.utf16 {
				utf16 = 1
			}
			// The store form, over a TEXT it must clear.
			regs := []tvalue{c.a, c.b, text("old")}
			args := ProgArgs{UTF16: utf16}
			runVM(t, []VInsn{{Op: VTextCmpSet, A: 0, B: 1, C: 2, Cond: cond}, {Op: VExit}}, nil, regs, &args, 0)
			switch {
			case want < 0 && (args.PC != 0 || string(regs[2].S) != "old"):
				t.Fatalf("case %d %v: set ran to pc %d, r2 %+v; want an exit", k, cond, args.PC, regs[2])
			case want >= 0 && (args.PC != 1 || regs[2].Typ != tvInt || regs[2].I != int64(want) || regs[2].S != nil):
				t.Fatalf("case %d %v: set pc %d, r2 %+v; want %d", k, cond, args.PC, regs[2], want)
			}
			// After an operand is known to be an INTEGER (a fused program
			// that wrote it), the other must be one too.
			if c.a.Typ == tvInt {
				regs = []tvalue{{}, c.b, text("old")}
				args = ProgArgs{UTF16: utf16}
				prog := []VInsn{{Op: VInt, A: 0, Imm: c.a.I}, {Op: VTextCmpSet, A: 0, B: 1, C: 2, Cond: cond}, {Op: VExit}}
				runVM(t, prog, []bool{true, false, false}, regs, &args, 0)
				switch {
				case want < 0 && (args.PC != 1 || string(regs[2].S) != "old"):
					t.Fatalf("case %d %v: known INTEGER set ran to pc %d, r2 %+v; want an exit at 1", k, cond, args.PC, regs[2])
				case want >= 0 && (args.PC != 2 || regs[2].Typ != tvInt || regs[2].I != int64(want)):
					t.Fatalf("case %d %v: known INTEGER set pc %d, r2 %+v; want %d", k, cond, args.PC, regs[2], want)
				}
			}
			// The jump form: to 2 when the condition holds, else on to 1.
			regs = []tvalue{c.a, c.b}
			args = ProgArgs{UTF16: utf16}
			runVM(t, []VInsn{{Op: VTextCmpJump, A: 0, B: 1, T: 2, Cond: cond}, {Op: VExit}, {Op: VExit}}, nil, regs, &args, 0)
			if pc := args.PC; want < 0 && pc != 0 || want == 1 && pc != 2 || want == 0 && pc != 1 {
				t.Fatalf("case %d %v: jump to pc %d; want %d", k, cond, pc, want)
			}
		}
	}
}
