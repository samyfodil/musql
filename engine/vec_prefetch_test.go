package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// TestVecPrefetchAsParse holds the table of vector texts read ahead
// (vecPrefetch) to parsing at the call: whatever a text's parse gives,
// including the failure of the texts that do not parse and the second use of a
// text the table has already given up, parseVectorText gives the same.
func TestVecPrefetchAsParse(t *testing.T) {
	saved := vecWorkers
	vecWorkers = 4
	defer func() { vecWorkers = saved }()
	rng := rand.New(rand.NewSource(33))
	mangles := []string{",", "]", "[", " ", "\x00", "x", "-", "e", ".", ",,", "1e", "99999999999999999999999"}
	for _, typ := range []vecType{vecF32, vecF64} {
		var texts [][]byte
		for i := 0; i < 400; i++ {
			var b strings.Builder
			b.WriteString("[")
			for j, n := 0, 1+rng.Intn(300); j < n; j++ {
				if j > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, []string{"%.6g", "%g", "%.3f", "%e"}[rng.Intn(4)], rng.NormFloat64()*float64(1+rng.Intn(3)))
			}
			b.WriteString("]")
			s := b.String()
			if i%5 == 0 {
				at := rng.Intn(len(s))
				s = s[:at] + mangles[rng.Intn(len(mangles))] + s[at:]
			}
			texts = append(texts, []byte(s))
		}
		texts = append(texts, texts[3]) // a text twice: the second use parses
		vecPrefetch(texts, typ)
		for _, s := range texts {
			got, gerr := parseVectorText(s, typ)
			want, werr := parseVectorTextRaw(s, typ)
			if fmt.Sprint(gerr) != fmt.Sprint(werr) || fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
				t.Fatalf("%.60q: prefetched %v %v, parsed %v %v", s, got.dims, gerr, want.dims, werr)
			}
		}
		// Taken once: the table is empty of them now.
		vecMemo.Lock()
		left := len(vecMemo.m[vecMemoSlot(typ)])
		vecMemo.Unlock()
		if left != 0 {
			t.Fatalf("%d texts left in the table after every one was used", left)
		}
	}
}

// TestVecPrefetchConcurrent runs prefetches and parses on several goroutines at
// once, for the race detector and for the answers: a table another statement
// replaced costs a parse, never a wrong vector.
func TestVecPrefetchConcurrent(t *testing.T) {
	saved := vecWorkers
	vecWorkers = 4
	defer func() { vecWorkers = saved }()
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for round := 0; round < 20; round++ {
				var texts [][]byte
				for i := 0; i < 40; i++ {
					var b strings.Builder
					b.WriteString("[")
					for j := 0; j < 300; j++ {
						if j > 0 {
							b.WriteString(",")
						}
						fmt.Fprintf(&b, "%.6g", rng.NormFloat64())
					}
					b.WriteString("]")
					texts = append(texts, []byte(b.String()))
				}
				vecPrefetch(texts, vecF32)
				for _, s := range texts {
					got, gerr := parseVectorText(s, vecF32)
					want, werr := parseVectorTextRaw(s, vecF32)
					if gerr != nil || werr != nil || fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
						t.Errorf("goroutine %d: %v %v", g, gerr, werr)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}
