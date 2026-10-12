package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestVecParallelAsSequential holds the chunked parse of a long vector text to
// the sequential one: the same vector, or the same error, for valid texts of
// both component types and for texts mangled in every way a chunk boundary
// could mishandle -- a stray ']' or NUL, a missing or doubled comma, spaces,
// junk after the end, a bad number in any chunk.
func TestVecParallelAsSequential(t *testing.T) {
	saved := vecWorkers
	vecWorkers = 4
	defer func() { vecWorkers, vecParallelOffForTest = saved, false }()
	rng := rand.New(rand.NewSource(21))
	forms := []string{"%.6g", "%g", "%.3f", "%e", "%.0f"}
	check := func(s string) {
		t.Helper()
		for _, typ := range []vecType{vecF32, vecF64} {
			vecParallelOffForTest = false
			par, perr := parseVectorText([]byte(s), typ)
			vecParallelOffForTest = true
			seq, serr := parseVectorText([]byte(s), typ)
			vecParallelOffForTest = false
			if fmt.Sprint(perr) != fmt.Sprint(serr) || fmt.Sprintf("%v", par) != fmt.Sprintf("%v", seq) {
				t.Fatalf("%.80q (%d bytes): parallel %v %v, sequential %v %v", s, len(s), par.dims, perr, seq.dims, serr)
			}
		}
	}
	mangles := []string{",", "]", "[", " ", "\x00", "x", "-", "e", ".", ",,", ", ", "1e", "\t", "99999999999999999999999"}
	for trial := 0; trial < 600; trial++ {
		n := 300 + rng.Intn(2500)
		form := forms[rng.Intn(len(forms))]
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
				if rng.Intn(50) == 0 {
					b.WriteString(" ")
				}
			}
			fmt.Fprintf(&b, form, rng.NormFloat64()*float64(1+rng.Intn(3)))
		}
		b.WriteString("]")
		s := b.String()
		check(s)
		check(s + " ")
		check(s + "\x00junk,1,2")
		check(s + "x")
		check(" " + s)
		for m := 0; m < 6; m++ {
			at := rng.Intn(len(s))
			mut := mangles[rng.Intn(len(mangles))]
			switch rng.Intn(3) {
			case 0:
				check(s[:at] + mut + s[at+1:])
			case 1:
				check(s[:at] + mut + s[at:])
			default:
				check(s[:at])
			}
		}
	}
	// A text with no component past a chunk's boundary, and the shortest that splits.
	check("[" + strings.Repeat("1,", 3000) + "2]")
	check("[" + strings.Repeat("1,", 3000) + "2")
	check("[" + strings.Repeat("1,", 3000) + "]")
	check("[" + strings.Repeat("1,", 20000) + "2]")
}
