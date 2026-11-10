//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"strings"
	"testing"
	"unsafe"
)

// TestVMSubstr: the ASCII substrings native code answers, sharing the TEXT's
// bytes, and every argument it leaves to the engine with the registers as
// they were -- as it does with the write barrier on.
func TestVMSubstr(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	long := strings.Repeat("abcdefgh", 10) + "z"
	texts := [][]byte{[]byte("hello"), []byte(long), []byte("ab\x00cd"), []byte("abcdefghij\xc3\xa9"),
		[]byte("abcdefgh\xc3\xa9ij"), []byte("\xc3\xa9abcdefghij"), []byte("x")}
	type tcase struct{ s, y, z tvalue }
	var cases []tcase
	for _, s := range texts {
		for _, y := range []int64{-1, 0, 1, 2, 5, 6, 8, 9, 10, 11, 17, 81, 82} {
			for _, z := range []int64{-1, 0, 1, 2, 3, 8, 9, 100, 1 << 62} {
				cases = append(cases, tcase{tvalue{Typ: tvText, S: s}, tvalue{Typ: tvInt, I: y}, tvalue{Typ: tvInt, I: z}})
			}
		}
	}
	hello := tvalue{Typ: tvText, S: []byte("hello")}
	one := tvalue{Typ: tvInt, I: 1}
	cases = append(cases,
		tcase{hello, tvalue{Typ: tvReal, F: 2}, one},
		tcase{hello, tvalue{Typ: tvInt, I: 2}, tvalue{Typ: tvNull}},
		tcase{hello, tvalue{Typ: tvInt, I: 2}, tvalue{Typ: tvReal, F: 1}},
		tcase{tvalue{Typ: tvText}, one, one},
		tcase{tvalue{Typ: tvBlob, S: []byte("hello")}, one, one},
		tcase{tvalue{Typ: tvNull}, one, one})
	// native reports what native code answers: the bytes from y-1, n of them,
	// when every byte to their end is ASCII and none NUL.
	native := func(c tcase) (int64, bool) {
		if c.s.Typ != tvText || c.y.Typ != tvInt || c.z.Typ != tvInt || c.z.I < 1 || c.y.I < 1 || c.y.I > int64(len(c.s.S)) {
			return 0, false
		}
		n := min(c.z.I, int64(len(c.s.S))-(c.y.I-1))
		for _, b := range c.s.S[:c.y.I-1+n] {
			if b == 0 || b >= 0x80 {
				return 0, false
			}
		}
		return n, true
	}
	prog := []VInsn{{Op: VSubstr, A: 0, C: 3}, {Op: VExit}}
	old := []byte("old")
	for k, c := range cases {
		n, ok := native(c)
		for _, wb := range []bool{false, true} {
			regs := []tvalue{c.s, c.y, c.z, {Typ: tvText, S: old}}
			var args ProgArgs
			func() {
				if wb {
					defer ForceWriteBarrierOnForTest()()
				}
				runVM(t, prog, nil, regs, &args, 0)
			}()
			if !ok || wb {
				if args.PC != 0 || regs[3].Typ != tvText || &regs[3].S[0] != &old[0] {
					t.Fatalf("case %d %q y=%d z=%d (barrier on %v): pc %d, r3 %+v; want an exit at 0", k, c.s.S, c.y.I, c.z.I, wb, args.PC, regs[3])
				}
				continue
			}
			r := regs[3]
			if args.PC != 1 || r.Typ != tvText || r.Sub != 0 || r.I != 0 || r.F != 0 || int64(len(r.S)) != n || int64(cap(r.S)) != n || &r.S[0] != &c.s.S[c.y.I-1] {
				t.Fatalf("case %d %q y=%d z=%d: pc %d, r3 %+v (cap %d); want %d bytes from %d, shared", k, c.s.S, c.y.I, c.z.I, args.PC, r, cap(r.S), n, c.y.I-1)
			}
		}
	}
}

// TestVMSubstrChecked: in one entry, substr over bytes an earlier substr
// checked answers without checking them again, and checks only those past
// them; a NUL past them still exits, and so does one past them in the same
// TEXT a later, longer substr reaches.
func TestVMSubstrChecked(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	// r0 the TEXT, r1 y, r2 1; r3 = substr; r4 = y2; r5 = substr at y2; r6 =
	// y3, r7 = substr at y3. The three run one after another.
	prog := []VInsn{
		{Op: VSubstr, A: 0, C: 3},
		{Op: VCopy, A: 4, B: 1},
		{Op: VSubstr, A: 0, C: 5},
		{Op: VCopy, A: 6, B: 1},
		{Op: VSubstr, A: 0, C: 7},
		{Op: VExit},
	}
	s := strings.Repeat("abcdefgh", 4) + "\x00tail"
	for _, c := range []struct {
		ys     [3]int64
		exitAt int64 // -1 for none
	}{
		{[3]int64{20, 5, 30}, -1},
		{[3]int64{30, 30, 1}, -1},
		{[3]int64{1, 32, 32}, -1},
		{[3]int64{20, 33, 1}, 2},
		{[3]int64{32, 10, 35}, 4},
	} {
		regs := make([]tvalue, 8)
		regs[0] = tvalue{Typ: tvText, S: []byte(s)}
		regs[2] = tvalue{Typ: tvInt, I: 1}
		prog[1] = VInsn{Op: VInt, A: 1, Imm: c.ys[1]}
		prog[3] = VInsn{Op: VInt, A: 1, Imm: c.ys[2]}
		regs[1] = tvalue{Typ: tvInt, I: c.ys[0]}
		var args ProgArgs
		runVM(t, prog, nil, regs, &args, 0)
		want := int64(5)
		if c.exitAt >= 0 {
			want = c.exitAt
		}
		if args.PC != want {
			t.Fatalf("%v: pc %d, want %d", c.ys, args.PC, want)
		}
		for k, r := range []int{3, 5, 7} {
			if int64(2*k) >= want {
				break
			}
			if regs[r].Typ != tvText || len(regs[r].S) != 1 || regs[r].S[0] != s[c.ys[k]-1] {
				t.Fatalf("%v: r%d %+v, want %q", c.ys, r, regs[r], s[c.ys[k]-1])
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

// TestVMInText matches TEXTs against a list of TEXTs byte for byte, an
// empty TEXT and an empty list included; a non-TEXT operand and UTF-16
// comparisons exit.
func TestVMInText(t *testing.T) {
	if !Available || !VMTextOrder {
		t.Skip("no TEXT ops on this platform")
	}
	noGC(t)
	words := []string{"SM CASE", "SM BOX", "", "SM PACK", "SM PKG", "AIR"}
	lits := make([]VTextLit, len(words))
	keep := make([][]byte, len(words))
	for i, w := range words {
		keep[i] = append([]byte(w), 0)
		lits[i] = VTextLit{Ptr: unsafe.Pointer(&keep[i][0]), Len: int64(len(w))}
	}
	for _, n := range []int{0, 1, len(words)} {
		prog := []VInsn{{Op: VInText, A: 0, B: n, C: 1, Imm: int64(uintptr(unsafe.Pointer(&lits[0])))}, {Op: VExit}}
		for _, x := range []string{"SM CASE", "SM BOX", "SM BO", "SM BOXX", "", "AIR", "air", "SM PKG", "SM CAS\\x00"} {
			regs := []tvalue{{Typ: tvText, S: []byte(x)}, {Typ: tvInt, I: 7}}
			pc := runVMCursor(t, prog, regs, nil, &VCursor{}, 0)
			want := int64(0)
			for _, w := range words[:n] {
				if w == x {
					want = 1
				}
			}
			if pc != 1 || regs[1].Typ != tvInt || regs[1].I != want {
				t.Fatalf("%d words, %q: pc %d r1 %+v, want %d", n, x, pc, regs[1], want)
			}
		}
	}
	prog := []VInsn{{Op: VInText, A: 0, B: len(words), C: 1, Imm: int64(uintptr(unsafe.Pointer(&lits[0])))}, {Op: VExit}}
	if pc := runVMCursor(t, prog, []tvalue{{Typ: tvInt, I: 3}, {}}, nil, &VCursor{}, 0); pc != 0 {
		t.Fatalf("an INTEGER operand ran natively to %d", pc)
	}
	regs := []tvalue{{Typ: tvText, S: []byte("AIR")}, {}}
	if pc := runVMCursorArgs(t, prog, regs, nil, &VCursor{}, &ProgArgs{UTF16: 1}); pc != 0 {
		t.Fatalf("a UTF-16 comparison ran natively to %d", pc)
	}
}
