//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"bytes"
	"math/rand"
	"testing"
	"unicode/utf8"
)

// textCells lays strs out as cells (offset | length<<32) over one heap.
func textCells(strs [][]byte) (cells []int64, heap []byte) {
	for _, s := range strs {
		cells = append(cells, int64(len(heap))|int64(len(s))<<32)
		heap = append(heap, s...)
	}
	heap = append(heap, 0) // a heap is never empty, so &heap[0] exists
	return cells, heap
}

func testTextStrings(seed int64, n int) [][]byte {
	rng := rand.New(rand.NewSource(seed))
	out := [][]byte{nil, []byte("a"), []byte("x\x00yz"), []byte("\x00"), []byte("é"), []byte("abcdefghijklmnop"),
		[]byte("abcdefghijklmno"), []byte("abcdefghijklmnopq"), bytes.Repeat([]byte("z"), 32), []byte("0123456789abcdef\x00tail"),
		[]byte("0123456789abcdefé"), []byte("ascii then \x00 é after the NUL")}
	for len(out) < n {
		l := rng.Intn(40)
		b := make([]byte, l)
		for i := range b {
			b[i] = byte('a' + rng.Intn(26))
		}
		switch rng.Intn(8) {
		case 0:
			if l > 0 {
				b[rng.Intn(l)] = 0
			}
		case 1:
			b = append(b, "ü"...)
		}
		out = append(out, b)
	}
	return out
}

// textLenModel is POpTextLen's contract: (length, false) for a cell it counts,
// (_, true) for one it sends to its fallback. Whole 16-byte chunks are tested
// for high bits before their NUL is looked for, so a byte >= 0x80 anywhere in
// the chunk holding the NUL also falls back -- conservative, never wrong: the
// fallback is the exact count.
func textLenModel(s []byte) (int64, bool) {
	i := 0
	for ; len(s)-i >= 16; i += 16 {
		c := s[i : i+16]
		for _, b := range c {
			if b >= 0x80 {
				return 0, true
			}
		}
		if j := bytes.IndexByte(c, 0); j >= 0 {
			return int64(i + j), false
		}
	}
	for ; i < len(s); i++ {
		if s[i] >= 0x80 {
			return 0, true
		}
		if s[i] == 0 {
			return int64(i), false
		}
	}
	return int64(len(s)), false
}

// TestEmittedTextLenMatchesLength: POpTextLen counts pure-ASCII text before
// its first NUL, and sends exactly the cells its contract says to the
// fallback; and its counts agree with length()'s rule.
func TestEmittedTextLenMatchesLength(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	strs := testTextStrings(31, 3000)
	cells, heap := textCells(strs)
	n := len(strs)
	want := make([]int64, n)
	var counted, fallbacks int64
	for i, s := range strs {
		l, fb := textLenModel(s)
		if fb {
			want[i] = -1
			fallbacks++
			continue
		}
		// The model's count is length()'s: characters before the NUL, all ASCII.
		before := s
		if j := bytes.IndexByte(s, 0); j >= 0 {
			before = s[:j]
		}
		if int64(utf8.RuneCount(before)) != l {
			t.Fatalf("model length %d for %q", l, s)
		}
		want[i] = l
		counted++
	}
	run := func(insns []ProgInsn) int64 {
		code, err := EmitProgram(insns, 2)
		if err != nil {
			t.Fatal(err)
		}
		kern, err := Map(code)
		if err != nil {
			t.Fatal(err)
		}
		defer kern.Close()
		regs := make([]int64, 8)
		var out, ovf int64
		args := &ProgArgs{N: int64(n), Regs: &regs[0], Out: &out, Overflow: &ovf}
		args.Col[0], args.Col[1], args.Heap[0] = &cells[0], &want[0], &heap[0]
		kern.Call2(args)
		return out
	}
	// Rows whose native length equals the model's (a fallback row's want is
	// -1, which no length equals, and it jumps away anyway).
	matched := run([]ProgInsn{
		{Op: POpTextLen, A: 1, B: 0, C: ProgNextRow},
		{Op: POpLoadCol, A: 2, B: 1},
		{Op: POpCmp, A: 3, B: 1, C: 2, Cond: CondE},
		{Op: POpSkipIfZero, A: 3},
		{Op: POpAccCount},
	})
	// Rows sent to the fallback.
	fell := run([]ProgInsn{
		{Op: POpTextLen, A: 1, B: 0, C: 2},
		{Op: POpJump, A: ProgNextRow},
		{Op: POpAccCount},
	})
	if matched != counted || fell != fallbacks {
		t.Fatalf("native lengths matching %d (want %d), fallbacks %d (want %d) of %d", matched, counted, fell, fallbacks, n)
	}
}
