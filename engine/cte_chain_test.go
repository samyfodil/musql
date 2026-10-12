package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCTEChainIsNotExponential: "c1 AS (SELECT v FROM c0), c2 AS (SELECT v
// FROM c1), ..." 80 deep (Turso's cte-chain-large-linear-regression). Each
// level used to run the level below once for its rows and again for its
// column metadata, so the cost doubled per level and this never finished.
func TestCTEChainIsNotExponential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	buildDB(t, path, `CREATE TABLE t(a)`)
	var b strings.Builder
	b.WriteString("WITH c0 AS (SELECT 1 AS v)")
	for i := 1; i <= 80; i++ {
		fmt.Fprintf(&b, ", c%d AS (SELECT v FROM c%d)", i, i-1)
	}
	b.WriteString(" SELECT v FROM c80")
	start := time.Now()
	got, err := queryDB(t, path, b.String())
	if err != nil {
		t.Fatal(err)
	}
	if got != "1:1:0:\"\"|\n" {
		t.Fatalf("got %q, want the integer 1", got)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("an 80-deep CTE chain took %v", d)
	}
}
