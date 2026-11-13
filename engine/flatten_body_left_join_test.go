package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFlattenBodyLeftJoin: a subquery whose body joins a table LEFT is
// flattened into its parent, as C flattens it (TPC-DS q93), rather than
// materialized; one that is itself a LEFT JOIN's right operand is not.
func TestFlattenBodyLeftJoin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.musq")
	buildDB(t, path,
		`CREATE TABLE ss(item INT, ticket INT, cust INT, qty INT, price REAL)`,
		`CREATE TABLE sr(item INT, ticket INT, rqty INT, reason INT)`,
		`CREATE TABLE reason(r_sk INT, r_desc TEXT)`)
	for _, tc := range []struct {
		q    string
		flat bool
	}{
		{`SELECT cust, sum(a) FROM (SELECT cust, CASE WHEN rqty IS NOT NULL THEN (qty - rqty) * price ELSE qty * price END a FROM ss LEFT JOIN sr ON sr.item = ss.item AND sr.ticket = ss.ticket, reason WHERE reason = r_sk AND r_desc = 'x') t GROUP BY cust`, true},
		{`SELECT cust, a FROM (SELECT cust, qty * price a FROM ss LEFT JOIN sr ON sr.item = ss.item) t`, true},
		{`SELECT r_desc, cust FROM reason LEFT JOIN (SELECT cust, reason FROM ss LEFT JOIN sr ON sr.item = ss.item) t ON t.reason = r_sk`, false},
	} {
		got, err := queryDB(t, path, "EXPLAIN QUERY PLAN "+tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if flat := !strings.Contains(got, "MATERIALIZE"); flat != tc.flat {
			t.Errorf("%s\nflattened %v, want %v:\n%s", tc.q, flat, tc.flat, got)
		}
	}
}
