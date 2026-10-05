package engine

// Tests window aggregate FILTER lowering and SUBTYPE argument compilation to
// verify that register mappings are correct at runtime.

import (
	"errors"
	"testing"
)

func windowStepArgsFixture(t *testing.T) *ReadOnlyPager {
	t.Helper()
	return openWindowFixture(t, []string{
		`CREATE TABLE tj(id INTEGER, x TEXT, j TEXT)`,
		`INSERT INTO tj VALUES(1,'a','[1]'),(2,'b','[2]'),(3,'c','[3]')`,
	})
}

// TestWindowFilterAlwaysBuffered verifies that FILTERs are always lowered to
// batch columns regardless of their aggregate's argument lowering.
func TestWindowFilterAlwaysBuffered(t *testing.T) {
	p := windowStepArgsFixture(t)
	for _, sql := range []string{
		`SELECT count() FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj`,
		`SELECT count(*) FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj`,
		`SELECT sum(id) FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj`,
		`SELECT json_group_array(json(j)) FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj`,
		`SELECT json_group_object(x, json(j)) FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj`,
	} {
		plan := windowPlanOf(t, p, sql)
		if len(plan.calls) != 1 {
			t.Fatalf("%s: %d calls", sql, len(plan.calls))
		}
		if plan.calls[0].filterCol < 0 {
			t.Errorf("%s: FILTER not lowered (filterCol %d)", sql, plan.calls[0].filterCol)
		}
	}
}

// TestWindowStepArgsCompile verifies that only json_group_array and
// json_group_object get step-time argument programs.
func TestWindowStepArgsCompile(t *testing.T) {
	p := windowStepArgsFixture(t)
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{`SELECT json_group_array(json(j)) OVER (ORDER BY id) FROM tj`, true},
		{`SELECT json_group_object(x, json(j)) OVER (ORDER BY id) FROM tj`, true},
		{`SELECT json_group_array(json(j)) FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj`, true},
		// Buffered instead: its argument is on windowAggArgsLowerable's list.
		{`SELECT sum(id) OVER (ORDER BY id) FROM tj`, false},
		{`SELECT group_concat(x, '-') OVER (ORDER BY id) FROM tj`, false},
		{`SELECT count(*) OVER (ORDER BY id) FROM tj`, false},
		// Positional: read by the frame code out of a batch column.
		{`SELECT lead(x) OVER (ORDER BY id) FROM tj`, false},
	} {
		plan := windowPlanOf(t, p, tc.sql)
		if len(plan.calls) != 1 {
			t.Fatalf("%s: %d calls", tc.sql, len(plan.calls))
		}
		if got := plan.calls[0].stepArgs != nil; got != tc.want {
			t.Errorf("%s: stepArgs != nil is %v, want %v", tc.sql, got, tc.want)
		}
	}
}

// TestWindowStepArgRegsStamped verifies that step-argument registers are
// assigned to the correct operand positions in the window aggregate context.
func TestWindowStepArgRegsStamped(t *testing.T) {
	p := windowStepArgsFixture(t)
	for _, tc := range []struct {
		sql      string
		wantArg  int
		wantSep  int
		nStepArg int
	}{
		{`SELECT json_group_array(json(j)) OVER (ORDER BY id) FROM tj`, 1, 0, 1},
		{`SELECT json_group_object(x, json(j)) OVER (ORDER BY id) FROM tj`, 1, 2, 2},
	} {
		plan := windowPlanOf(t, p, tc.sql)
		call := plan.calls[0]
		if call.stepArgs == nil {
			t.Fatalf("%s: no step-args program", tc.sql)
		}
		if got := len(call.stepArgs.resultReg); got != tc.nStepArg {
			t.Errorf("%s: %d step-arg result registers, want %d", tc.sql, got, tc.nStepArg)
		}
		// The window calls' own results are NOT part of this program: a step
		// runs before any of them exists.
		if call.stepArgs.nWin != 0 {
			t.Errorf("%s: step-args program reserves %d window-result registers, want 0", tc.sql, call.stepArgs.nWin)
		}
		var tmpl aggItem
		stampWindowStepArgRegs(&tmpl, plan, call)
		if got := tmpl.rowRegs[aggExprArg] - plan.nOps; got != tc.wantArg {
			t.Errorf("%s: aggExprArg at nOps+%d, want nOps+%d", tc.sql, got, tc.wantArg)
		}
		sep := tmpl.rowRegs[aggExprSep]
		if sep != 0 {
			sep -= plan.nOps
		}
		if sep != tc.wantSep {
			t.Errorf("%s: aggExprSep at nOps+%d, want nOps+%d", tc.sql, sep, tc.wantSep)
		}
	}
}

// TestWindowStepArgsRefusedShapes verifies that nested window functions inside
// step arguments are correctly rejected.
func TestWindowStepArgsRefusedShapes(t *testing.T) {
	p := windowStepArgsFixture(t)
	const sql = `SELECT json_group_array(abs(id) OVER (ORDER BY id)) OVER (ORDER BY id) FROM tj`
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cerr := func() error {
		_, e := compileSelectScanRow(p, stmt, nil, nil)
		return e
	}()
	if cerr == nil {
		t.Fatal("a nested window call in a SUBTYPE argument must not compile -- it would drop the OVER")
	}
	if !errors.Is(cerr, errVDBEUnsupported) {
		t.Errorf("declined with %v, want errVDBEUnsupported", cerr)
	}
}
