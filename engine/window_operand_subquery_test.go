package engine

// Tests window operand lowering to batch columns, verifying which subqueries
// in PARTITION BY, ORDER BY, and function arguments are compiled into columns.

import "testing"

func windowOperandFixture(t *testing.T) *ReadOnlyPager {
	t.Helper()
	return openWindowFixture(t, []string{
		`CREATE TABLE tx(a INTEGER, b TEXT)`,
		`INSERT INTO tx VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE map(v INTEGER, t TEXT)`,
		`INSERT INTO map VALUES(1,'one'),(2,'two'),(3,'three')`,
	})
}

// TestWindowSubqueryOperandsLower verifies that operand columns exist for
// lowered subqueries in window specs.
func TestWindowSubqueryOperandsLower(t *testing.T) {
	p := windowOperandFixture(t)
	for _, tc := range []struct {
		sql  string
		part int // expected number of lowered PARTITION BY keys
		ord  int // ... ORDER BY keys
		args int // ... window-function arguments
	}{
		// A CORRELATED scalar subquery, in both key positions and under a
		// named window -- the shapes the corpus actually holds (window9.test).
		{`SELECT sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`, 1, 1, 1},
		{`SELECT sum(a) OVER win FROM tx WINDOW win AS (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a)`, 1, 1, 1},
		{`SELECT sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a)) FROM tx`, 0, 1, 1},
		// UNCORRELATED, and the IN / EXISTS spellings.
		{`SELECT sum(a) OVER (PARTITION BY (SELECT max(v) FROM map)) FROM tx`, 1, 0, 1},
		{`SELECT sum(a) OVER (PARTITION BY a IN (SELECT v FROM map)) FROM tx`, 1, 0, 1},
		{`SELECT sum(a) OVER (PARTITION BY EXISTS(SELECT 1 FROM map WHERE v=a)) FROM tx`, 1, 0, 1},
		// A positional function's ARGUMENT, which the frame code reads back.
		{`SELECT lead(a, (SELECT max(v) FROM map)) OVER (ORDER BY a) FROM tx`, 0, 1, 2},
		{`SELECT nth_value(a, (SELECT min(v) FROM map)) OVER (ORDER BY a) FROM tx`, 0, 1, 2},
		// An aggregate window call's FILTER is an operand too.
		{`SELECT sum(a) FILTER (WHERE a IN (SELECT v FROM map)) OVER (ORDER BY a) FROM tx`, 0, 1, 1},
	} {
		plan := windowPlanOf(t, p, tc.sql)
		if len(plan.calls) != 1 {
			t.Fatalf("%s: %d calls", tc.sql, len(plan.calls))
		}
		call := plan.calls[0]
		lowered := func(cols []int) int {
			n := 0
			for _, c := range cols {
				if c >= 0 {
					n++
				}
			}
			return n
		}
		if got := lowered(call.partCols); got != tc.part {
			t.Errorf("%s: %d/%d PARTITION BY keys lowered (cols %v)", tc.sql, got, tc.part, call.partCols)
		}
		if got := lowered(call.orderCols); got != tc.ord {
			t.Errorf("%s: %d/%d ORDER BY keys lowered (cols %v)", tc.sql, got, tc.ord, call.orderCols)
		}
		if got := lowered(call.argCols); got != tc.args {
			t.Errorf("%s: %d/%d arguments lowered (cols %v)", tc.sql, got, tc.args, call.argCols)
		}
	}
}

// TestWindowOutwardAggSubqueryOperand verifies that aggregates in window spec
// subqueries re-associate correctly and that statements compile even when
// operands cannot be lowered.
func TestWindowOutwardAggSubqueryOperand(t *testing.T) {
	p := windowOperandFixture(t)
	for _, sql := range []string{
		`SELECT a, sum(a) OVER (ORDER BY (SELECT sum(tx.a))) FROM tx`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT count(tx.a))) FROM tx`,
	} {
		stmt, err := ParseSelect(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		if _, err := compileSelectScanRow(p, stmt, nil, nil); err != nil {
			t.Errorf("%s: declined %v, but this family answers now", sql, err)
		}
	}
	// And the predicate itself, over the scopes the plan would carry: a spec
	// subquery whose aggregate names a column of THIS query is refused; one
	// naming nothing of this query's is not.
	scopes := []tableScope{{name: "tx", cols: []columnInfo{{Name: "a"}, {Name: "b"}},
		colIndex: map[string]int{"a": 0, "b": 1}}}
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{`SELECT sum(a) OVER (ORDER BY (SELECT sum(tx.a))) FROM tx`, false},
		{`SELECT sum(a) OVER (ORDER BY (SELECT max(v) FROM map)) FROM tx`, true},
		{`SELECT sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a)) FROM tx`, true},
	} {
		stmt, err := ParseSelect(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		var calls []windowCall
		rewriteWindowCalls(stmt.Columns[0].Expr, &calls)
		if len(calls) != 1 || calls[0].spec == nil || len(calls[0].spec.OrderBy) != 1 {
			t.Fatalf("%s: unexpected parse shape", tc.sql)
		}
		if got := windowOperandLowerable(nil, calls[0].spec.OrderBy[0].Expr, scopes); got != tc.want {
			t.Errorf("%s: windowOperandLowerable = %v, want %v", tc.sql, got, tc.want)
		}
	}
}
