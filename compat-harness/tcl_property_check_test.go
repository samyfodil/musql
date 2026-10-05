// Unit gates for the property tier.
// Each test plants a divergence and requires it to be caught as WRONG.
package compat

import (
	"fmt"
	"strings"
	"testing"
)

// row is a shorthand for one normalized result row.
func propRows(rows ...[]string) [][]string { return rows }

// tclFakeCols is n placeholder output-column names, for the plan cases whose
// assertions do not depend on what the columns are called.
func tclFakeCols(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("c%d", i)
	}
	return out
}

func TestTCLPropertyPlanDerivation(t *testing.T) {
	cases := []struct {
		name        string
		stmt        string
		ncols       int
		cols        []string // output column names; tclFakeCols(ncols) if nil
		wantCols    []tclColCompare
		rowCount    bool
		rowsAlign   bool
		orderStable bool
	}{{
		name:      "bare random is INTEGER, row count fixed",
		stmt:      "SELECT random() AS y FROM t1",
		ncols:     1,
		wantCols:  []tclColCompare{tclColClass},
		rowCount:  true,
		rowsAlign: true,
	}, {
		name:      "ORDER BY a random result column keeps the row set, loses the order",
		stmt:      "SELECT random() AS y FROM t1 ORDER BY 1",
		ncols:     1,
		wantCols:  []tclColCompare{tclColClass},
		rowCount:  true,
		rowsAlign: true,
		// orderStable is false: the sort key is the random column itself.
	}, {
		name:        "a deterministic ORDER BY still fixes the order",
		stmt:        "SELECT x, random() AS y FROM t1 ORDER BY x",
		cols:        []string{"x", "y"},
		ncols:       2,
		wantCols:    []tclColCompare{tclColStrict, tclColClass},
		rowCount:    true,
		rowsAlign:   true,
		orderStable: true,
	}, {
		name:     "random() in the WHERE clause can change which rows come back",
		stmt:     "SELECT x FROM t1 WHERE random() > 0",
		ncols:    1,
		wantCols: []tclColCompare{tclColStrict},
		// rowCount false: the generator escaped the result-column list.
	}, {
		name:     "DISTINCT collapses duplicate draws, so the row count moves",
		stmt:     "SELECT DISTINCT abs(random())%5 AS r FROM cnt",
		ncols:    1,
		wantCols: []tclColCompare{tclColClass},
	}, {
		name:     "GROUP BY over a random column likewise",
		stmt:     "SELECT abs(random())%5 AS r FROM cnt GROUP BY 1",
		ncols:    1,
		wantCols: []tclColCompare{tclColClass},
	}, {
		name:      "randomblob fixes the byte length",
		stmt:      "SELECT randomblob(32)",
		ncols:     1,
		wantCols:  []tclColCompare{tclColClassLen},
		rowCount:  true,
		rowsAlign: true,
	}, {
		name:      "hex() over randomblob keeps a fixed length",
		stmt:      "SELECT hex(randomblob(4000))",
		ncols:     1,
		wantCols:  []tclColCompare{tclColClassLen},
		rowCount:  true,
		rowsAlign: true,
	}, {
		name:      "hex() over random() does NOT: the digit count varies",
		stmt:      "SELECT hex(random())",
		ncols:     1,
		wantCols:  []tclColCompare{tclColNothing},
		rowCount:  true,
		rowsAlign: true,
	}, {
		name:      "a CASE over random() is genuinely NULL-or-INTEGER",
		stmt:      "SELECT CASE WHEN random()>0 THEN 1 END FROM t1",
		ncols:     1,
		wantCols:  []tclColCompare{tclColNothing},
		rowCount:  true,
		rowsAlign: true,
	}, {
		name:      "a star column is deterministic and still placed",
		stmt:      "SELECT *, random() FROM t1",
		ncols:     4,
		wantCols:  []tclColCompare{tclColStrict, tclColStrict, tclColStrict, tclColClass},
		rowCount:  true,
		rowsAlign: true,
	}, {
		name:      "a compound takes the weakest arm per position",
		stmt:      "SELECT 1, 2 UNION ALL SELECT random(), 3",
		ncols:     2,
		wantCols:  []tclColCompare{tclColClass, tclColStrict},
		rowCount:  true,
		rowsAlign: true,
	}, {
		name:      "an ORDER BY key that is NOT an output column leaves the order unjudgeable",
		stmt:      "SELECT random() AS y FROM t1 ORDER BY x",
		ncols:     1,
		cols:      []string{"y"},
		wantCols:  []tclColCompare{tclColClass},
		rowCount:  true,
		rowsAlign: true,
		// orderStable false: the sort key is invisible in the result, so a
		// tie group's order cannot be judged from it at all.
	}, {
		name:     "LIMIT plus a random column stops the rows lining up",
		stmt:     "SELECT random() AS y FROM t1 LIMIT 5",
		ncols:    1,
		wantCols: []tclColCompare{tclColClass},
		rowCount: true,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outCols := tc.cols
			if outCols == nil {
				outCols = tclFakeCols(tc.ncols)
			}
			plan, nondet := tclPropPlanFor(tc.stmt, outCols, tclRandomTainted{})
			if !nondet {
				t.Fatalf("tclPropPlanFor(%q) reported no nondeterminism", tc.stmt)
			}
			if len(plan.cols) != len(tc.wantCols) {
				t.Fatalf("cols = %v, want %v", plan.cols, tc.wantCols)
			}
			for i := range tc.wantCols {
				if plan.cols[i] != tc.wantCols[i] {
					t.Errorf("col %d comparability = %d, want %d", i, plan.cols[i], tc.wantCols[i])
				}
			}
			if plan.rowCount != tc.rowCount {
				t.Errorf("rowCount = %v, want %v", plan.rowCount, tc.rowCount)
			}
			if plan.rowsAlign != tc.rowsAlign {
				t.Errorf("rowsAlign = %v, want %v", plan.rowsAlign, tc.rowsAlign)
			}
			if plan.orderStable != tc.orderStable {
				t.Errorf("orderStable = %v, want %v", plan.orderStable, tc.orderStable)
			}
		})
	}
}

// TestTCLPropertyTierIsNotEnteredWithoutNondeterminism proves the tier cannot
// absorb an ordinary wrong answer: a statement with no random input at all
// never gets a plan, so runTCLSegment's WRONG arm still owns it.
func TestTCLPropertyTierIsNotEnteredWithoutNondeterminism(t *testing.T) {
	for _, stmt := range []string{
		"SELECT a, b FROM t1 ORDER BY a",
		"SELECT count(*) FROM t1",
		"SELECT randomized FROM t1", // an identifier that merely CONTAINS "random"
	} {
		if _, nondet := tclPropPlanFor(stmt, tclFakeCols(1), tclRandomTainted{}); nondet {
			t.Errorf("tclPropPlanFor(%q) claimed nondeterminism", stmt)
		}
	}
}

// TestTCLPropertyMatchCatchesPlantedDivergence is the mutation gate: each case
// plants ONE divergence in a result whose values are legitimately
// unreproducible, and requires the tier to report it WRONG.
func TestTCLPropertyMatchCatchesPlantedDivergence(t *testing.T) {
	// "SELECT x, random() AS y FROM t1 ORDER BY x" over a two-row t1.
	plan := tclPropPlan{
		cols:        []tclColCompare{tclColStrict, tclColClass},
		rowCount:    true,
		rowsAlign:   true,
		orderStable: true,
		why:         "test",
	}
	gCols := []string{"x", "y"}
	gRows := propRows([]string{"I:1", "I:8342734"}, []string{"I:2", "I:-99"})

	cases := []struct {
		name   string
		cCols  []string
		cRows  [][]string
		wantOK bool
	}{{
		name:   "same shape, different random draws: accepted",
		cCols:  []string{"x", "y"},
		cRows:  propRows([]string{"I:1", "I:5"}, []string{"I:2", "I:77777"}),
		wantOK: true,
	}, {
		name:  "column COUNT divergence",
		cCols: []string{"x"},
		cRows: propRows([]string{"I:1"}, []string{"I:2"}),
	}, {
		name:  "column NAME divergence",
		cCols: []string{"x", "z"},
		cRows: propRows([]string{"I:1", "I:5"}, []string{"I:2", "I:6"}),
	}, {
		name:  "row COUNT divergence",
		cCols: []string{"x", "y"},
		cRows: propRows([]string{"I:1", "I:5"}),
	}, {
		name:  "storage CLASS divergence in the random column (TEXT where C has INTEGER)",
		cCols: []string{"x", "y"},
		cRows: propRows([]string{"I:1", "T:5"}, []string{"I:2", "I:6"}),
	}, {
		name:  "VALUE divergence in the DETERMINISTIC column",
		cCols: []string{"x", "y"},
		cRows: propRows([]string{"I:1", "I:5"}, []string{"I:3", "I:6"}),
	}, {
		name:  "row ORDER divergence under a deterministic ORDER BY",
		cCols: []string{"x", "y"},
		cRows: propRows([]string{"I:2", "I:5"}, []string{"I:1", "I:6"}),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := tclPropertyMatch(plan, gCols, gRows, tc.cCols, tc.cRows)
			if ok != tc.wantOK {
				t.Fatalf("tclPropertyMatch = %v (%s), want %v", ok, reason, tc.wantOK)
			}
		})
	}
}

// TestTCLPropertyStrictColumnHasFullPrecision verifies that deterministic
// columns are compared with full precision, not as sorted values.
func TestTCLPropertyStrictColumnHasFullPrecision(t *testing.T) {
	plan := tclPropPlan{
		cols:        []tclColCompare{tclColStrict, tclColClass},
		rowCount:    true,
		rowsAlign:   true,
		orderStable: true,
		why:         "test",
	}
	cols := []string{"big", "r"}

	// Differ in the 19th digit: identical under FormatFloat(f,'f',6,64).
	gRows := propRows([]string{"I:1000000000000000001", "I:7"})
	cRows := propRows([]string{"I:1000000000000000002", "I:9"})
	if ok, _ := tclPropertyMatch(plan, cols, gRows, cols, cRows); ok {
		t.Errorf("a 1-unit divergence in a large INTEGER was absorbed -- the strict column is being " +
			"compared through canonicalCell (a six-decimal SORT key) instead of cellsEqual")
	}

	// ...and the same for a REAL beyond six decimal places.
	gReal := propRows([]string{"F:1.0000001", "I:7"})
	cReal := propRows([]string{"F:1.0000002", "I:9"})
	if ok, _ := tclPropertyMatch(plan, cols, gReal, cols, cReal); ok {
		t.Errorf("a divergence in the 7th decimal place was absorbed for the same reason")
	}

	// CONTROL: cellsEqual's INTEGER/REAL storage tolerance must SURVIVE, or
	// this fix would trade a false accept for a false reject. "I:3" and "F:3"
	// are the same value in different storage classes and must still match.
	gTol := propRows([]string{"I:3", "I:7"})
	cTol := propRows([]string{"F:3", "I:9"})
	if ok, reason := tclPropertyMatch(plan, cols, gTol, cols, cTol); !ok {
		t.Errorf("I:3 vs F:3 rejected (%s) -- cellsEqual's storage tolerance was lost", reason)
	}
}

// TestTCLPropertyMatchGuardsRowWidth pins invariant 3 for the compare loop:
// a cgo row shorter than the engine row must REPORT, never index out of range.
// Not reachable today (both drivers build rows of len(cols)), but every other
// comparator here guards it and a panic is this project's hardest failure.
func TestTCLPropertyMatchGuardsRowWidth(t *testing.T) {
	plan := tclPropPlan{
		cols:      []tclColCompare{tclColStrict, tclColClass},
		rowCount:  true,
		rowsAlign: true,
		why:       "test",
	}
	cols := []string{"x", "y"}
	gRows := propRows([]string{"I:1", "I:8"})
	cRows := [][]string{{"I:1"}} // one cell short
	ok, reason := tclPropertyMatch(plan, cols, gRows, cols, cRows)
	if ok {
		t.Fatalf("a short row was accepted")
	}
	if !strings.Contains(reason, "column count") {
		t.Errorf("reason = %q, want it to name the row's column count", reason)
	}
}

// TestTCLPropertyMatchCatchesBlobLengthDivergence is the byte-length half of
// the mutation gate, kept separate because it needs a randomblob plan.
func TestTCLPropertyMatchCatchesBlobLengthDivergence(t *testing.T) {
	// randomBlob fixes the length even though the bytes are not reproducible.
	plan := tclPropPlan{cols: []tclColCompare{tclColClassLen}, rowCount: true, rowsAlign: true, why: "test"}
	cols := []string{"randomblob(4)"}
	got := propRows([]string{"X:deadbeef"})
	for _, tc := range []struct {
		name   string
		cgo    [][]string
		wantOK bool
	}{
		{"different bytes, same length: accepted", propRows([]string{"X:0badc0de"}), true},
		{"a valid-UTF-8 draw renders as TEXT on one side only: still accepted", propRows([]string{"T:abcd"}), true},
		{"byte LENGTH divergence", propRows([]string{"X:deadbeefcafe"}), false},
		{"a TEXT-length divergence through the same rule", propRows([]string{"T:abcde"}), false},
		{"class divergence (INTEGER where a blob was required)", propRows([]string{"I:4"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := tclPropertyMatch(plan, cols, got, cols, tc.cgo)
			if ok != tc.wantOK {
				t.Fatalf("tclPropertyMatch = %v (%s), want %v", ok, reason, tc.wantOK)
			}
		})
	}
}

// TestTCLTaintedPlanKeepsWhatItCan pins the two taint kinds apart: values-only
// taint leaves the row count comparable for a query that cannot filter on
// those values, row taint leaves nothing but the columns.
func TestTCLTaintedPlanKeepsWhatItCan(t *testing.T) {
	values := tclRandomTainted{"t1": tclTaintValues}
	rowsOnly := tclRandomTainted{"t1": tclTaintRows}
	for _, tc := range []struct {
		name         string
		stmt         string
		tainted      tclRandomTainted
		wantRowCount bool
	}{
		{"plain read of a value-tainted table", "SELECT * FROM t1", values, true},
		{"...but not once it can filter on those values", "SELECT * FROM t1 WHERE d > 'x'", values, false},
		{"...nor once it can collapse them", "SELECT DISTINCT d FROM t1", values, false},
		{"...nor once it aggregates them", "SELECT count(DISTINCT d) FROM t1", values, false},
		{"a row-tainted table gives up the row count too", "SELECT * FROM t1", rowsOnly, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, nondet := tclPropPlanFor(tc.stmt, tclFakeCols(1), tc.tainted)
			if !nondet {
				t.Fatalf("tclPropPlanFor(%q) reported no nondeterminism", tc.stmt)
			}
			if plan.rowCount != tc.wantRowCount {
				t.Errorf("rowCount = %v, want %v", plan.rowCount, tc.wantRowCount)
			}
			for i, c := range plan.cols {
				if c != tclColNothing {
					t.Errorf("col %d = %d, want tclColNothing (taint is tracked per TABLE)", i, c)
				}
			}
		})
	}
}

// TestTCLRandomMaterializationBudget pins the resource guard on the two
// statements in the whole corpus that trip it, and on their neighbours that
// must not.
func TestTCLRandomMaterializationBudget(t *testing.T) {
	for _, tc := range []struct {
		stmt string
		over bool
	}{
		{"WITH r(x,y) AS ( SELECT 1, randomblob(1000) UNION ALL SELECT x+1, randomblob(1000) FROM r LIMIT 2200000 ) SELECT count(*) FROM r", true},
		{"WITH r(x,y) AS ( SELECT 1, randomblob(100) UNION ALL SELECT x+1, randomblob(100) FROM r LIMIT 1000000 ) SELECT count(x) FROM r", true},
		{"WITH r(x,y) AS ( SELECT 1, randomblob(1000) UNION ALL SELECT x+1, randomblob(1000) FROM r LIMIT 20000 ) SELECT count(*) FROM r", false},
		{"SELECT randomblob(5000)", false},
		{"SELECT hex(randomblob(4000))", false},
		{"SELECT c FROM t1 ORDER BY random() LIMIT 50000", false},
	} {
		// Tests the resource budget against expected limits.
		if over := tclRandomMaterialization(tc.stmt) > int64(tclRandomMaterializationCappedBudget); over != tc.over {
			t.Errorf("tclRandomMaterialization(%.60s...) over budget = %v, want %v", tc.stmt, over, tc.over)
		}
	}
}

// TestTCLWriteTargetShapes verifies that write targets are correctly identified.
func TestTCLWriteTargetShapes(t *testing.T) {
	for _, tc := range []struct{ stmt, verb, target string }{
		{"INSERT INTO t1 VALUES(1)", "INSERT", "t1"},
		{"INSERT INTO t1(a,ax,b) SELECT 1,random(),2 FROM c", "INSERT", "t1"},
		{"WITH RECURSIVE c(i) AS (VALUES(1)) INSERT INTO t1(a,ax,b) SELECT printf('%02x',i), random(), i FROM c", "INSERT", "t1"},
		{"WITH cnt(i) AS (SELECT 1) INSERT INTO t1 SELECT i%2, randomblob(500) FROM cnt", "INSERT", "t1"},
		{"INSERT INTO main.t1(a) VALUES(randomblob(4))", "INSERT", "t1"},
		{"UPDATE t1 SET d = randomblob(1000)", "UPDATE", "t1"},
		{"CREATE TABLE t9(a,b)", "CREATE", "t9"},
		{"DROP TABLE t9", "DROP", "t9"},
		{"SELECT * FROM t1", "", ""},
	} {
		verb, target := tclWriteTarget(tc.stmt)
		if verb != tc.verb || target != tc.target {
			t.Errorf("tclWriteTarget(%q) = (%q, %q), want (%q, %q)", tc.stmt, verb, target, tc.verb, tc.target)
		}
	}
}

// TestTCLPropertyMatchToleratesOrderByTies proves the tier gives a
// deterministic ORDER BY the same tie tolerance the strict compare gives
// (tclOrderByTieOK): SQL leaves the order WITHIN a tie group unspecified, so
// two engines may legitimately disagree there -- but only there.
func TestTCLPropertyMatchToleratesOrderByTies(t *testing.T) {
	// "SELECT a, random() AS y FROM t1 ORDER BY a", where a ties.
	plan := tclPropPlan{
		cols:        []tclColCompare{tclColStrict, tclColClass},
		rowCount:    true,
		rowsAlign:   true,
		orderStable: true,
		orderKeys:   []int{0},
		why:         "test",
	}
	cols := []string{"a", "y"}
	// Two rows with a=1 and one with a=2: the a=1 pair may come in either
	// order, and their random column differs on both sides regardless.
	got := propRows([]string{"I:1", "T:p"}, []string{"I:1", "T:q"}, []string{"I:2", "T:r"})
	for _, tc := range []struct {
		name   string
		cgo    [][]string
		wantOK bool
	}{{
		name:   "tied rows swapped: accepted",
		cgo:    propRows([]string{"I:1", "T:s"}, []string{"I:1", "T:t"}, []string{"I:2", "T:u"}),
		wantOK: true,
	}, {
		name: "a NON-tied row moved: still WRONG",
		cgo:  propRows([]string{"I:2", "T:s"}, []string{"I:1", "T:t"}, []string{"I:1", "T:u"}),
	}, {
		name: "a key VALUE changed: still WRONG",
		cgo:  propRows([]string{"I:1", "T:s"}, []string{"I:1", "T:t"}, []string{"I:3", "T:u"}),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			// The random column is TEXT on both sides here, so only its class
			// is asserted; the tie tolerance is what decides each case.
			ok, reason := tclPropertyMatch(plan, cols, got, cols, tc.cgo)
			if ok != tc.wantOK {
				t.Fatalf("tclPropertyMatch = %v (%s), want %v", ok, reason, tc.wantOK)
			}
		})
	}
}

// TestTCLPropertyTieToleranceStillComparesTheRows is the other half of the tie
// tolerance: agreeing on every ORDER BY key at every position is NOT enough on
// its own. Two results can honor the same ORDER BY and still hold DIFFERENT
// rows, and that is a wrong answer, not a tie.
func TestTCLPropertyTieToleranceStillComparesTheRows(t *testing.T) {
	plan := tclPropPlan{
		cols:        []tclColCompare{tclColStrict, tclColStrict},
		rowCount:    true,
		rowsAlign:   true,
		orderStable: true,
		orderKeys:   []int{0},
		why:         "test",
	}
	cols := []string{"a", "b"}
	got := propRows([]string{"I:1", "T:a"}, []string{"I:1", "T:b"}, []string{"I:2", "T:c"})
	// Same key at every position (1,1,2), so the key check alone accepts --
	// but the rows differ: "b" became "z".
	cgo := propRows([]string{"I:1", "T:a"}, []string{"I:1", "T:z"}, []string{"I:2", "T:c"})
	if ok, _ := tclPropertyMatch(plan, cols, got, cols, cgo); ok {
		t.Error("a changed row inside a tie group was accepted as a tie")
	}
	// ...and the genuine tie (the same two rows, swapped) still is accepted.
	swapped := propRows([]string{"I:1", "T:b"}, []string{"I:1", "T:a"}, []string{"I:2", "T:c"})
	if ok, reason := tclPropertyMatch(plan, cols, got, cols, swapped); !ok {
		t.Errorf("a genuine tie reorder was rejected: %s", reason)
	}
}
