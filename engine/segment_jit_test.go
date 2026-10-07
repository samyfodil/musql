package engine

import (
	"fmt"
	"math/rand"
	"runtime"
	"testing"
)

// JIT filter results must match the interpreter exactly.
func TestJITFilterMatchesInterpreter(t *testing.T) {
	if !JITEnabled() {
		t.Skip("no JIT on this machine; the interpreter is the only path")
	}
	ops := []segPredOp{segGT, segGE, segLT, segLE, segEQ, segNE}
	rng := rand.New(rand.NewSource(17))
	// Lengths test vector and scalar paths, including edge cases.
	for _, n := range []int{1, 3, 4, 5, 7, 8, 9, 2049} {
		cols := []columnInfo{{Name: "a"}, {Name: "b"}}
		rows := make([][]Value, n)
		rowids := make([]uint64, n)
		for i := range rows {
			rows[i] = []Value{
				{Typ: Int, I: int64(rng.Intn(40) - 20)},
				{Typ: Int, I: int64(rng.Intn(8) - 4)},
			}
			rowids[i] = uint64(i + 1)
		}
		raw, err := buildSegment(cols, rowids, rows)
		if err != nil {
			t.Fatalf("buildSegment: %v", err)
		}
		s, err := openSegment(raw)
		if err != nil {
			t.Fatalf("openSegment: %v", err)
		}
		for _, oa := range ops {
			for _, ob := range ops {
				for _, bounds := range [][2]int64{{0, 0}, {-20, -4}, {19, 3}, {5, -1}} {
					preds := []segPred{
						{Col: 0, Op: oa, Val: Value{Typ: Int, I: bounds[0]}},
						{Col: 1, Op: ob, Val: Value{Typ: Int, I: bounds[1]}},
					}
					got := jitFilterCount(s, preds)
					if got < 0 {
						t.Fatalf("n=%d: the JIT declined a shape it should emit", n)
					}
					// The interpreter's own answer, computed the long way.
					want := 0
					for i := range rows {
						if oa.satisfied(compareValues(s.Value(0, i), preds[0].Val)) &&
							ob.satisfied(compareValues(s.Value(1, i), preds[1].Val)) {
							want++
						}
					}
					if got != want {
						t.Fatalf("n=%d ops=(%d,%d) bounds=%v: JIT %d, interpreter %d",
							n, oa, ob, bounds, got, want)
					}
				}
			}
		}
	}
}

// Non-vacuity for the differential battery next door: if the JIT silently
// declined every shape, TestOperatorPairsAgreeWithTheEngine would still pass
// and would be testing the interpreter against itself.
func TestJITIsActuallyReachedByTheFilter(t *testing.T) {
	if !JITEnabled() {
		t.Skip("no JIT on this machine")
	}
	cols := []columnInfo{{Name: "a"}, {Name: "b"}}
	rows := make([][]Value, 100)
	rowids := make([]uint64, 100)
	for i := range rows {
		rows[i] = []Value{{Typ: Int, I: int64(i)}, {Typ: Int, I: int64(i % 5)}}
		rowids[i] = uint64(i + 1)
	}
	raw, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatalf("buildSegment: %v", err)
	}
	s, err := openSegment(raw)
	if err != nil {
		t.Fatalf("openSegment: %v", err)
	}
	preds := []segPred{
		{Col: 0, Op: segGT, Val: Value{Typ: Int, I: 50}},
		{Col: 1, Op: segNE, Val: Value{Typ: Int, I: 2}},
	}
	if got := jitFilterCount(s, preds); got < 0 {
		t.Fatal("the JIT declined the shape the whole filter path is built around")
	}
	if k := jitFilterKernel(preds); k == nil || k.Size == 0 {
		t.Fatal("no kernel was emitted")
	}
	// One predicate is emitted too, and must agree with the interpreter --
	// this used to assert a DECLINE, back when the branchy scalar kernel could
	// not beat Go at a single compare.
	if got := jitFilterCount(s, preds[:1]); got < 0 {
		t.Fatal("the JIT declined a one-predicate filter it now emits")
	} else if want := segFilterCount(s, preds[:1]); got != want {
		t.Fatalf("one predicate: JIT %d, interpreter %d", got, want)
	}
	if k := jitFilterKernel(preds[:1]); k == nil || k.Size == 0 {
		t.Fatal("no one-predicate kernel was emitted")
	}
	// Three or more still decline: there is no emitter for them.
	three := append(append([]segPred{}, preds...), segPred{Col: 0, Op: segLT, Val: Value{Typ: Int, I: 90}})
	if got := jitFilterCount(s, three); got != -1 {
		t.Fatalf("the JIT answered a three-predicate filter it does not emit: %d", got)
	}
	textCol := []segPred{
		{Col: 0, Op: segGT, Val: Value{Typ: Text, S: []byte("x")}},
		{Col: 1, Op: segNE, Val: Value{Typ: Int, I: 2}},
	}
	if got := jitFilterCount(s, textCol); got != -1 {
		t.Fatalf("the JIT answered a non-integer comparison: %d", got)
	}
	_ = fmt.Sprint
}

// programJITEnabled is JITEnabled for the paths the program and VM JITs serve:
// js/wasm has only the filter kernels so far.
func programJITEnabled() bool { return JITEnabled() && runtime.GOARCH != "wasm" }
