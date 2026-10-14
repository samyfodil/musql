package compat

import (
	"strings"
	"testing"
)

// servedMatchesC is for a shape this engine used to decline and now answers:
// the answer must be C's, column names and cells both, in C's order when the
// query has an ORDER BY.
func servedMatchesC(t *testing.T, q string, eCols []string, eRows [][]string, cCols []string, cRows [][]string) {
	t.Helper()
	if ok, why := queryResultsMatch(eCols, eRows, cCols, cRows, strings.Contains(strings.ToUpper(q), "ORDER BY")); !ok {
		t.Errorf("[%s] answered but DIVERGES from C SQLite: %s\n  musql: cols=%v rows=%v\n  cgo:   cols=%v rows=%v", q, why, eCols, eRows, cCols, cRows)
	}
}
