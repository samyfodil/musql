package engine

import (
	"errors"
	"testing"
)

// TestWindowIntRealOperandDeclines: a window function's operand holding the
// integer value of a REAL generated column reads back in C as a REAL or as an
// integer depending on which nested sub-select its window is coded in and on
// the other operands beside it (lag(b) is 1, max(b) OVER (ORDER BY k) beside
// a min(b) OVER () is 1.0), which the batch does not reproduce, so the query
// declines (windowRows.appendRow). Each of these answered an INTEGER where C
// answers a REAL.
func TestWindowIntRealOperandDeclines(t *testing.T) {
	p := openWindowFixture(t, []string{
		`CREATE TABLE g(k INTEGER PRIMARY KEY, a INT, b REAL GENERATED ALWAYS AS (a) VIRTUAL, p INT)`,
		`INSERT INTO g(k,a,p) VALUES (1,1,1),(2,2,1),(3,3,2)`,
	})
	for _, q := range []string{
		`SELECT quote(max(b) OVER (ORDER BY k)), quote(min(b) OVER ()) FROM g`,
		`SELECT quote(lead(b) OVER (ORDER BY k)), quote(lag(b + 0) OVER (ORDER BY k)) FROM g`,
		`SELECT group_concat(b) OVER (ORDER BY k), group_concat(b, '|') OVER (ORDER BY k ROWS 1 PRECEDING) FROM g`,
		`SELECT quote(b), quote(lag(b) OVER (ORDER BY k)) FROM g`,
		`SELECT quote(b), row_number() OVER (PARTITION BY b) FROM g`,
	} {
		if _, rows, err := p.Query(q); !errors.Is(err, errVDBEUnsupported) {
			t.Errorf("%s: got %v, %v; want a decline", q, rows, err)
		}
	}
	// No IntReal reaches the operand: served.
	if _, rows, err := p.Query(`SELECT quote(lag(b + 0) OVER (ORDER BY k)), quote(sum(a) OVER ()) FROM g ORDER BY k`); err != nil || len(rows) != 3 {
		t.Fatalf("an operand with no IntReal: %v, %v", rows, err)
	}
}
