//go:build (amd64 || arm64) && (unix || windows)

package jit

import (
	"math/rand"
	"testing"
	"unsafe"
)

// TestArgsLayout checks the Args struct layout used by generated code.
func TestArgsLayout(t *testing.T) {
	var a Args
	for _, c := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"A", unsafe.Offsetof(a.A), OffA},
		{"C", unsafe.Offsetof(a.C), OffC},
		{"N", unsafe.Offsetof(a.N), OffN},
		{"XA", unsafe.Offsetof(a.XA), OffXA},
		{"XC", unsafe.Offsetof(a.XC), OffXC},
		{"Out", unsafe.Offsetof(a.Out), OffOut},
		{"V", unsafe.Offsetof(a.V), OffV},
		{"Out2", unsafe.Offsetof(a.Out2), OffOut2},
	} {
		if c.got != c.want {
			t.Errorf("Args.%s is at %d, generated code reads %d", c.name, c.got, c.want)
		}
	}
}

// TestEmittedFilterMatchesGo verifies kernel output matches Go for all operators.
func TestEmittedFilterMatchesGo(t *testing.T) {
	conds := []struct {
		c    Cond
		name string
		f    func(x, y int64) bool
	}{
		{CondG, ">", func(x, y int64) bool { return x > y }},
		{CondGE, ">=", func(x, y int64) bool { return x >= y }},
		{CondL, "<", func(x, y int64) bool { return x < y }},
		{CondLE, "<=", func(x, y int64) bool { return x <= y }},
		{CondE, "==", func(x, y int64) bool { return x == y }},
		{CondNE, "!=", func(x, y int64) bool { return x != y }},
	}
	rng := rand.New(rand.NewSource(3))
	for _, n := range []int{0, 1, 2, 3, 17, 1000} {
		a := make([]int64, n)
		c := make([]int64, n)
		for i := range a {
			a[i] = int64(rng.Intn(400) - 200) // negatives: an unsigned compare would differ
			c[i] = int64(rng.Intn(10) - 5)
		}
		for _, ca := range conds {
			// Single predicate.
			code, err := EmitFilterCount(ca.c, false, CondE)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			k, err := Map(code)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			var out int64
			args := &Args{N: int64(n), XA: 0, Out: &out}
			if n > 0 {
				args.A, args.C = &a[0], &c[0]
			}
			k.Call(args)
			want := 0
			for _, v := range a {
				if ca.f(v, 0) {
					want++
				}
			}
			if int(out) != want {
				t.Fatalf("n=%d %s: kernel %d, Go %d", n, ca.name, out, want)
			}
			k.Close()

			// Two predicates.
			for _, cc := range conds {
				code, err := EmitFilterCount(ca.c, true, cc.c)
				if err != nil {
					t.Fatalf("emit: %v", err)
				}
				k, err := Map(code)
				if err != nil {
					t.Fatalf("map: %v", err)
				}
				out = 0
				args := &Args{N: int64(n), XA: 0, XC: 1, Out: &out}
				if n > 0 {
					args.A, args.C = &a[0], &c[0]
				}
				k.Call(args)
				want := 0
				for i := range a {
					if ca.f(a[i], 0) && cc.f(c[i], 1) {
						want++
					}
				}
				if int(out) != want {
					t.Fatalf("n=%d %s AND %s: kernel %d, Go %d", n, ca.name, cc.name, out, want)
				}
				k.Close()
			}
		}
	}
}

// TestEmittedVectorFilterMatchesGo verifies vector kernel output matches Go.
func TestEmittedVectorFilterMatchesGo(t *testing.T) {
	if !HasVector() {
		t.Skip("no vector unit on this machine")
	}
	conds := []struct {
		c    Cond
		name string
		f    func(x, y int64) bool
	}{
		{CondG, ">", func(x, y int64) bool { return x > y }},
		{CondGE, ">=", func(x, y int64) bool { return x >= y }},
		{CondL, "<", func(x, y int64) bool { return x < y }},
		{CondLE, "<=", func(x, y int64) bool { return x <= y }},
		{CondE, "==", func(x, y int64) bool { return x == y }},
		{CondNE, "!=", func(x, y int64) bool { return x != y }},
	}
	rng := rand.New(rand.NewSource(5))
	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 1001} {
		a := make([]int64, n)
		c := make([]int64, n)
		for i := range a {
			a[i] = int64(rng.Intn(20) - 10)
			c[i] = int64(rng.Intn(6) - 3)
		}
		// Single-column form first.
		for _, ca := range conds {
			code, err := EmitFilterCountSIMD(ca.c, false, CondE)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			k, err := Map(code)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			var out int64
			args := &Args{N: int64(n), XA: 0, Out: &out}
			if n > 0 {
				args.A = &a[0]
			}
			k.Call(args)
			want := 0
			for _, v := range a {
				if ca.f(v, 0) {
					want++
				}
			}
			if int(out) != want {
				t.Fatalf("n=%d %s (one column): vector kernel %d, Go %d", n, ca.name, out, want)
			}
			k.Close()
		}
		for _, ca := range conds {
			for _, cc := range conds {
				code, err := EmitFilterCountSIMD(ca.c, true, cc.c)
				if err != nil {
					t.Fatalf("emit: %v", err)
				}
				k, err := Map(code)
				if err != nil {
					t.Fatalf("map: %v", err)
				}
				var out int64
				args := &Args{N: int64(n), XA: 0, XC: -1, Out: &out}
				if n > 0 {
					args.A, args.C = &a[0], &c[0]
				}
				k.Call(args)
				want := 0
				for i := range a {
					if ca.f(a[i], 0) && cc.f(c[i], -1) {
						want++
					}
				}
				if int(out) != want {
					t.Fatalf("n=%d %s AND %s: vector kernel %d, Go %d", n, ca.name, cc.name, out, want)
				}
				k.Close()
			}
		}
	}
}
