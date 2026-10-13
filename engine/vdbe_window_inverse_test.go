package engine

import (
	"math"
	"testing"
)

// TestGroupConcatWindowInverse follows func.c's GroupConcatCtx through the
// "empty prefix" case of moving_concat_json.sqltest: terms ('', '|'), ('', '|'),
// ('x', '|'), ('y', '--') over ROWS BETWEEN 1 PRECEDING AND CURRENT ROW answer
// NUL (an empty text C builds with length 1), '|', 'x', 'y'.
func TestGroupConcatWindowInverse(t *testing.T) {
	var g groupConcatWindow
	text := func(v Value) string {
		if v.Typ == Null {
			return "NULL"
		}
		return string(v.S)
	}
	g.step("", "|", true)
	if got := text(g.value()); got != "\x00" {
		t.Errorf("row 1: %q, want one NUL byte", got)
	}
	g.step("", "|", true)
	if got := text(g.value()); got != "|" {
		t.Errorf("row 2: %q, want |", got)
	}
	g.inverse("")
	g.step("x", "|", true)
	if got := text(g.value()); got != "x" {
		t.Errorf("row 3: %q, want x", got)
	}
	g.inverse("")
	g.step("y", "--", true)
	if got := text(g.value()); got != "y" {
		t.Errorf("row 4: %q, want y", got)
	}
	g.inverse("x")
	g.inverse("y")
	if got := text(g.value()); got != "NULL" {
		t.Errorf("emptied: %q, want NULL", got)
	}
}

// TestSumInverseKeepsInfinityPollution: taking +Inf back out of a running float
// total leaves NaN, which a recomputed frame would not have (sumInverse,
// func.c:1962).
func TestSumInverseKeepsInfinityPollution(t *testing.T) {
	it := &aggItem{kind: aggSum}
	for _, v := range []float64{1.5, math.Inf(1)} {
		it.cnt++
		it.sumStep(false, 0, v)
	}
	it.cnt--
	it.sumInverse(false, 0, math.Inf(1))
	got, err := it.finalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.Typ != Null {
		t.Errorf("sum after +Inf in and out = %v, want NULL (NaN)", got)
	}
}

// TestSumInverseIntegerOverflowLatches: an integer total that overflowed on the
// way in keeps the error even when the inverse brings it back into range.
func TestSumInverseIntegerOverflowLatches(t *testing.T) {
	it := &aggItem{kind: aggSum}
	for _, v := range []int64{math.MaxInt64, 5} {
		it.cnt++
		it.sumStep(true, v, float64(v))
	}
	it.cnt--
	it.sumInverse(true, math.MaxInt64, float64(math.MaxInt64))
	if _, err := it.finalize(); err == nil {
		t.Error("sum finalized after a transient overflow without \"integer overflow\"")
	}
}
