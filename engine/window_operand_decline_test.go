package engine

// Window operands are batch-column reads that compile-time decline if the
// compiler cannot lower them. Tests that unlowerable operands decline rather
// than execute, and that fallbacks serve common shapes.

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// Verifies windowOperand takes a column index, never an expression.
func TestWindowOperandTakesNoExpr(t *testing.T) {
	b, err := os.ReadFile("vdbe_window.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "func (m *vdbe) windowOperand(plan *windowPlan, entry []Value, col int) (Value, error)") {
		t.Error("windowOperand's signature changed -- it must take no Expr, so it cannot evaluate one")
	}
}

// Verifies unlowerable operands (nested windows, outward-associated aggregates) decline as errors.
func TestWindowUnlowerableOperandDeclines(t *testing.T) {
	p := openWindowFixture(t, []string{
		`CREATE TABLE tx(a INTEGER, b TEXT)`,
		`INSERT INTO tx VALUES(1,'x'),(2,'y'),(3,'z')`,
	})
	for _, sql := range []string{
		// Nested window call in positional argument
		`SELECT nth_value(a, abs(a) OVER (ORDER BY a)) OVER (ORDER BY a) FROM tx`,
		`SELECT lead(a, abs(a) OVER (ORDER BY a)) OVER (ORDER BY a) FROM tx`,
		// Outward-associated aggregate in positional argument
		`SELECT nth_value(a, (SELECT sum(tx.a))) OVER (ORDER BY a) FROM tx`,
	} {
		stmt, err := ParseSelect(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		_, cerr := compileSelectScanRow(p, stmt, nil, nil)
		if cerr == nil {
			t.Errorf("%s: compiled, but an operand the compiler cannot lower must be an error", sql)
			continue
		}
		if !errors.Is(cerr, errVDBEUnsupported) {
			t.Errorf("%s: declined with %v, want errVDBEUnsupported", sql, cerr)
		}
	}
}

// One recorded capability loss: outward-associated aggregates in operands decline.
// The fallback windowAggFallsBackToStepArgs handles most operand arguments.
func TestWindowAggArgNoLongerSoft(t *testing.T) {
	p := openWindowFixture(t, []string{
		`CREATE TABLE tx(a INTEGER, b TEXT)`,
		`INSERT INTO tx VALUES(1,'x'),(2,'y'),(3,'z')`,
	})
	const sql = `SELECT sum((SELECT sum(tx.a))) OVER (ORDER BY a) FROM tx`
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cerr := func() error {
		_, e := compileSelectScanRow(p, stmt, nil, nil)
		return e
	}()
	if cerr == nil {
		t.Fatalf("%s: compiled -- if the outward-re-associated aggregate now reaches the "+
			"scan body in a register, pin the ANSWER (6) against the oracle instead of this decline", sql)
	}
	if !errors.Is(cerr, errVDBEUnsupported) {
		t.Errorf("%s: declined with %v, want errVDBEUnsupported", sql, cerr)
	}
}

// Verifies the fallback compileWindowStepArgs handles ordinary aggregate operands.
func TestWindowAggArgStepArgFallback(t *testing.T) {
	p := openWindowFixture(t, []string{
		`CREATE TABLE tx(a INTEGER, b TEXT)`,
		`INSERT INTO tx VALUES(1,'x'),(2,'y'),(3,'z')`,
	})
	for _, sql := range []string{
		// Various aggregate window function calls
		`SELECT group_concat(b, '-') OVER (ORDER BY a) FROM tx`,
		`SELECT sum(a) OVER (ORDER BY a) FROM tx`,
		`SELECT sum(a) FILTER (WHERE a > 1) OVER (ORDER BY a) FROM tx`,
	} {
		stmt, err := ParseSelect(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		if _, cerr := compileSelectScanRow(p, stmt, nil, nil); cerr != nil {
			t.Errorf("%s: declined with %v", sql, cerr)
		}
	}
}
