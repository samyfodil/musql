//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"unsafe"
)

// TestVMLike matches every pattern shape against texts in both cases, as the
// engine's likePlan matches them: anchored or not at either end, pieces that
// overlap, an exact pattern, NOT LIKE; and exits on a NUL in the text, a
// pattern register that is not the constant, and case_sensitive_like.
func TestVMLike(t *testing.T) {
	if !Available || !VMTextOrder {
		t.Skip("no native LIKE on this platform")
	}
	noGC(t)
	pats := []string{"%green%", "MEDIUM POLISHED%", "%BRASS", "%Customer%Complaints%", "abc", "a%b%c", "%", "%%",
		"ab%ab", "x%", "%x", "%a%a%", "%ab%ba%", "aB", "%-%", "a%"}
	rng := rand.New(rand.NewSource(5))
	texts := []string{"", "green", "GREEN apple", "a forest green", "MEDIUM POLISHED TIN", "medium polished",
		"LARGE BRASS", "brass", "xxCustomerxxComplaintsxx", "Complaints Customer", "abc", "ABC", "abcd", "ab", "aba",
		"abab", "x", "xa", "ax", "aaa", "abba", "a-b", "-"}
	for range 400 {
		var b strings.Builder
		for range rng.Intn(9) {
			b.WriteByte("abcABCx- %"[rng.Intn(10)])
		}
		texts = append(texts, b.String())
	}
	for _, pat := range pats {
		for _, not := range []bool{false, true} {
			p := likePatOf(pat, not)
			prog := []VInsn{{Op: VLike, A: 0, B: 1, C: 2, Pat: p}, {Op: VExit}}
			for _, text := range texts {
				regs := []tvalue{{Typ: tvText, S: []byte(text)}, {Typ: tvText, S: unsafe.Slice(unsafe.StringData(pat), len(pat))}, {}}
				pc := runVMCursor(t, prog, regs, nil, &VCursor{}, 0)
				want := likeRef(pat, text) != not
				if pc != 1 || regs[2].Typ != tvInt || (regs[2].I == 1) != want {
					t.Fatalf("%q LIKE %q (not %v): pc %d r2 %+v, want %v", text, pat, not, pc, regs[2], want)
				}
			}
		}
	}
	p := likePatOf("%green%", false)
	prog := []VInsn{{Op: VLike, A: 0, B: 1, C: 2, Pat: p}, {Op: VExit}}
	src := unsafe.Slice(unsafe.StringData("%green%"), 7)
	for name, regs := range map[string][]tvalue{
		"NUL in the text":    {{Typ: tvText, S: []byte("gr\x00een")}, {Typ: tvText, S: src}, {}},
		"another pattern":    {{Typ: tvText, S: []byte("green")}, {Typ: tvText, S: []byte("%green%")}, {}},
		"an INTEGER text":    {{Typ: tvInt, I: 7}, {Typ: tvText, S: src}, {}},
		"a NULL pattern":     {{Typ: tvText, S: []byte("green")}, {}, {}},
		"a shorter constant": {{Typ: tvText, S: []byte("green")}, {Typ: tvText, S: src[:6]}, {}},
	} {
		if pc := runVMCursor(t, prog, regs, nil, &VCursor{}, 0); pc != 0 {
			t.Fatalf("%s: ran natively to %d", name, pc)
		}
	}
}

// likePatOf splits pat as the engine's newLikePlan does.
func likePatOf(pat string, not bool) *VLikePat {
	p := &VLikePat{Src: uintptr(unsafe.Pointer(unsafe.StringData(pat))), SrcLen: len(pat), Not: not}
	if !strings.Contains(pat, "%") {
		p.Exact, p.Pieces = true, [][]byte{[]byte(pat)}
		return p
	}
	p.Head, p.Tail = pat[0] != '%', pat[len(pat)-1] != '%'
	for _, piece := range strings.Split(pat, "%") {
		if piece != "" {
			p.Pieces = append(p.Pieces, []byte(piece))
		}
	}
	return p
}

// likeRef is LIKE over ASCII with '%' only, by backtracking.
func likeRef(pat, text string) bool {
	if pat == "" {
		return text == ""
	}
	if pat[0] == '%' {
		for i := 0; i <= len(text); i++ {
			if likeRef(pat[1:], text[i:]) {
				return true
			}
		}
		return false
	}
	return text != "" && bytes.EqualFold([]byte{pat[0]}, []byte{text[0]}) && likeRef(pat[1:], text[1:])
}
