//go:build (amd64 || arm64) && (unix || windows)

package jit

import (
	"math/rand"
	"testing"
)

// The JITted kernel against the Go the VDBE would otherwise run, on identical
// data. Both count "a > xa AND c != xc" over 100,000 rows.
func BenchmarkFilterJITvsGo(b *testing.B) {
	const n = 100_000
	rng := rand.New(rand.NewSource(0x5eed))
	a := make([]int64, n)
	c := make([]int64, n)
	for i := range a {
		a[i] = int64(rng.Intn(1_000_000))
		c[i] = int64(rng.Intn(10))
	}
	code, err := EmitFilterCount(CondG, true, CondNE)
	if err != nil {
		b.Fatal(err)
	}
	k, err := Map(code)
	if err != nil {
		b.Fatal(err)
	}
	defer k.Close()
	b.Logf("kernel is %d bytes of machine code", k.Size)

	var out int64
	args := &Args{A: &a[0], C: &c[0], N: n, XA: 500_000, XC: 7, Out: &out}
	k.Call(args)
	want := 0
	for i := range a {
		if a[i] > 500_000 && c[i] != 7 {
			want++
		}
	}
	if int(out) != want {
		b.Fatalf("kernel %d, Go %d -- measuring them would be meaningless", out, want)
	}

	b.Run("go_scalar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			count := 0
			for j := range a {
				if a[j] > 500_000 && c[j] != 7 {
					count++
				}
			}
			if count == 0 {
				b.Fatal("no rows")
			}
		}
	})
	b.Run("jit_scalar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			k.Call(args)
			if out == 0 {
				b.Fatal("no rows")
			}
		}
	})
}

// The vector kernel against the same Go loop, which is the comparison the
// backend question actually turns on: several int64 lanes per compare against
// one -- four on AVX2, two on NEON.
func BenchmarkFilterVectorvsGo(b *testing.B) {
	if !HasVector() {
		b.Skip("no vector unit on this machine")
	}
	const n = 100_000
	rng := rand.New(rand.NewSource(0x5eed))
	a := make([]int64, n)
	c := make([]int64, n)
	for i := range a {
		a[i] = int64(rng.Intn(1_000_000))
		c[i] = int64(rng.Intn(10))
	}
	code, err := EmitFilterCountSIMD(CondG, true, CondNE)
	if err != nil {
		b.Fatal(err)
	}
	k, err := Map(code)
	if err != nil {
		b.Fatal(err)
	}
	defer k.Close()
	b.Logf("vector kernel is %d bytes of machine code", k.Size)

	var out int64
	args := &Args{A: &a[0], C: &c[0], N: n, XA: 500_000, XC: 7, Out: &out}
	k.Call(args)
	want := 0
	for i := range a {
		if a[i] > 500_000 && c[i] != 7 {
			want++
		}
	}
	if int(out) != want {
		b.Fatalf("kernel %d, Go %d -- measuring them would be meaningless", out, want)
	}

	b.Run("go", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			count := 0
			for j := range a {
				if a[j] > 500_000 && c[j] != 7 {
					count++
				}
			}
			if count == 0 {
				b.Fatal("no rows")
			}
		}
	})
	b.Run("jit_vector", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			k.Call(args)
			if out == 0 {
				b.Fatal("no rows")
			}
		}
	})
}
