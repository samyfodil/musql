package engine

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

// TestFunctionNaNIsNull: a NaN a function returns is NULL, as
// sqlite3_result_double stores it.
func TestFunctionNaNIsNull(t *testing.T) {
	if err := RegisterFunction(ScalarFunction{Name: "nan_result", NArg: 0, Fn: func([]Value) (Value, error) {
		return Value{Typ: Float, F: math.NaN()}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`SELECT nan_result() IS NULL, typeof(nan_result())`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(typedRows(rows)); got != `[1:1|3:"null"|]` {
		t.Fatalf("got %s, want 1 and 'null'", got)
	}
}
