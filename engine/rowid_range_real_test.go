package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestRowidRangeBeyondInt64: a REAL bound past either end of int64 compares
// with every rowid as numbers do: a lower bound above them all or an upper
// bound below them all leaves no row, the other way round every row.
func TestRowidRangeBeyondInt64(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.musq")
	stmts := []string{`CREATE TABLE t(a)`}
	for i := 1; i <= 50; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d)`, i))
	}
	buildDB(t, path, stmts...)
	check := func(stage string) {
		t.Helper()
		for q, want := range map[string]int{
			`SELECT a FROM t WHERE rowid > 1e19`:   0,
			`SELECT a FROM t WHERE rowid >= 1e19`:  0,
			`SELECT a FROM t WHERE rowid < 1e19`:   50,
			`SELECT a FROM t WHERE rowid < -1e19`:  0,
			`SELECT a FROM t WHERE rowid <= -1e19`: 0,
			`SELECT a FROM t WHERE rowid > -1e19`:  50,
		} {
			got, err := genRows(t, path, q)
			if err != nil {
				t.Fatalf("%s %s: %v", stage, q, err)
			}
			n := 0
			if got != "" {
				n = len(splitRows(got))
			}
			if n != want {
				t.Errorf("%s %s: %d rows, want %d", stage, q, n, want)
			}
		}
	}
	check("log")
	execDB(t, path, `VACUUM`)
	check("segments")
}

func splitRows(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
