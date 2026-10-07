package jit

import (
	"math/rand"
	"testing"
)

// TestEmittedVectorSumMatchesGo checks EmitFilterSumSIMD against a Go
// reference for every operator, one and two predicates, and lengths that end
// in every possible scalar tail.
func TestEmittedVectorSumMatchesGo(t *testing.T) {
	if !HasVector() {
		t.Skip("no vector unit on this machine")
	}
	conds := []struct {
		c Cond
		f func(x, y int64) bool
	}{
		{CondG, func(x, y int64) bool { return x > y }},
		{CondGE, func(x, y int64) bool { return x >= y }},
		{CondL, func(x, y int64) bool { return x < y }},
		{CondLE, func(x, y int64) bool { return x <= y }},
		{CondE, func(x, y int64) bool { return x == y }},
		{CondNE, func(x, y int64) bool { return x != y }},
	}
	rng := rand.New(rand.NewSource(9))
	for _, n := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 1003} {
		a, c, v := make([]int64, n), make([]int64, n), make([]int64, n)
		for i := range a {
			a[i], c[i] = int64(rng.Intn(20)-10), int64(rng.Intn(6)-3)
			v[i] = rng.Int63n(1<<40) - 1<<39
		}
		run := func(ca, cc int, two bool) {
			code, err := EmitFilterSumSIMD(conds[ca].c, two, conds[cc].c)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			k, err := Map(code)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			defer k.Close()
			var sum, cnt int64
			args := &Args{N: int64(n), XA: 0, XC: -1, Out: &sum, Out2: &cnt}
			if n > 0 {
				args.A, args.C, args.V = &a[0], &c[0], &v[0]
			}
			k.Call(args)
			var wantSum, wantCnt int64
			for i := range a {
				if conds[ca].f(a[i], 0) && (!two || conds[cc].f(c[i], -1)) {
					wantSum += v[i]
					wantCnt++
				}
			}
			if sum != wantSum || cnt != wantCnt {
				t.Fatalf("n=%d cond %d/%d two=%v: kernel sum=%d count=%d, Go sum=%d count=%d",
					n, ca, cc, two, sum, cnt, wantSum, wantCnt)
			}
		}
		for ca := range conds {
			run(ca, 0, false)
			for cc := range conds {
				run(ca, cc, true)
			}
		}
	}
}
