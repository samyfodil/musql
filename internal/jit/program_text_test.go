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

// textMatchModel is POpTextMatch's contract: LIKE's comparison of the text
// before its first NUL against an ASCII pattern, folding ASCII letters when
// fold is set.
func textMatchModel(s, pat []byte, mode TextMode, fold bool) bool {
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	low := func(b byte) byte {
		if fold && b >= 'A' && b <= 'Z' {
			return b + 0x20
		}
		return b
	}
	at := func(i int) bool {
		for j := range pat {
			if low(s[i+j]) != pat[j] {
				return false
			}
		}
		return true
	}
	switch mode {
	case TextEq:
		return len(s) == len(pat) && at(0)
	case TextPrefix:
		return len(s) >= len(pat) && at(0)
	case TextSuffix:
		return len(s) >= len(pat) && at(len(s)-len(pat))
	}
	for i := 0; i+len(pat) <= len(s); i++ {
		if at(i) {
			return true
		}
	}
	return false
}

// TestEmittedTextMatchMatchesLike runs every mode, folded and exact, over
// texts and patterns built to hit both the 16-byte path and the byte path:
// the heap is laid out so the last cells end at its edge.
func TestEmittedTextMatchMatchesLike(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	rng := rand.New(rand.NewSource(41))
	alpha := []byte("abcABC_-%xyzXYZ0")
	var strs [][]byte
	for i := 0; i < 4000; i++ {
		b := make([]byte, rng.Intn(30))
		for j := range b {
			b[j] = alpha[rng.Intn(len(alpha))]
		}
		switch rng.Intn(10) {
		case 0:
			if len(b) > 0 {
				b[rng.Intn(len(b))] = 0
			}
		case 1:
			b = append(b, "é"...)
		}
		strs = append(strs, b)
	}
	cells, heap := textCells(strs)
	heap = heap[:len(heap)-1] // no padding: the last cell ends at the heap's edge
	if len(heap) == 0 {
		t.Fatal("empty heap")
	}
	n := len(strs)
	for trial := 0; trial < 80; trial++ {
		mode := TextMode(trial % 4)
		fold := trial%2 == 0
		pl := 1 + rng.Intn(16)
		pat := make([]byte, pl)
		for j := range pat {
			pat[j] = alpha[rng.Intn(len(alpha))]
			if fold && pat[j] >= 'A' && pat[j] <= 'Z' {
				pat[j] += 0x20
			}
		}
		if trial%5 == 0 && n > 0 { // a pattern taken from a text, so matches happen
			s := strs[rng.Intn(n)]
			if len(s) >= 1 {
				lo := rng.Intn(len(s))
				hi := min(len(s), lo+1+rng.Intn(16))
				pat = bytes.ToLower(append([]byte(nil), s[lo:hi]...))
				if !fold {
					pat = append([]byte(nil), s[lo:hi]...)
				}
				if bytes.IndexByte(pat, 0) >= 0 || bytes.IndexFunc(pat, func(r rune) bool { return r >= 0x80 }) >= 0 {
					continue
				}
			}
		}
		want := make([]int64, n)
		var expect int64
		for i, s := range strs {
			if textMatchModel(s, pat, mode, fold) {
				want[i] = 1
				expect++
			}
		}
		insns := []ProgInsn{
			{Op: POpTextMatch, A: 1, B: 0, C: ProgNextRow, Pat: pat, Mode: mode, Fold: fold},
			{Op: POpLoadCol, A: 2, B: 1},
			{Op: POpCmp, A: 3, B: 1, C: 2, Cond: CondE},
			{Op: POpSkipIfZero, A: 3},
			{Op: POpAccCount},
		}
		code, err := EmitProgram(insns, 2)
		if err != nil {
			t.Fatal(err)
		}
		kern, err := Map(code)
		if err != nil {
			t.Fatal(err)
		}
		regs := make([]int64, 8)
		var out, ovf int64
		args := &ProgArgs{N: int64(n), Regs: &regs[0], Out: &out, Overflow: &ovf}
		args.Col[0], args.Col[1], args.Heap[0], args.HeapLen[0] = &cells[0], &want[0], &heap[0], int64(len(heap))
		kern.Call2(args)
		kern.Close()
		if out != int64(n) {
			t.Fatalf("mode %d fold %v pattern %q: %d of %d rows agree with LIKE (%d expected matches)", mode, fold, pat, out, n, expect)
		}
	}
}
