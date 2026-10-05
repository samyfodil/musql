package compat

// This file tests COMPOUND ORDER BY resolution scope. C SQLite restricts ORDER BY
// term resolution to explicit aliases and ordinals; this engine resolves against
// derived column names as well.

import (
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var coSchema = []string{
	"CREATE TABLE ot(q INTEGER, k INTEGER)",
	"INSERT INTO ot VALUES(5,1)",
}

var coCases = []struct {
	name  string
	q     string
	wrong bool
}{
	// RULE 2. "q+0" is the first arm's DERIVED result name (ENAME_SPAN), not an
	// AS clause, so resolveAsName does not match it; resolveOrderByTermToExprList
	// then resolves the double-quoted token as SQLite's double-quoted-string
	// misfeature rather than a column, which matches no result expression.
	{"derived-name-no-correlation", `SELECT q+0 FROM ot UNION SELECT 9 ORDER BY "q+0"`, true},
	// ...and with a real AS clause it is ENAME_NAME, so both engines accept.
	{"explicit-alias-control", `SELECT q+0 AS "q+0" FROM ot UNION SELECT 9 ORDER BY "q+0"`, false},

	// RULE 3, the no-outer-chain half. Arm 1 is FROM-less and ot.q is
	// correlated, so nc.pSrcList is empty and nc.pNext is 0: the term resolves
	// nowhere and the compound is rejected.
	{"correlated-arm-bare-name",
		"SELECT k, (SELECT ot.q UNION SELECT 9 ORDER BY q LIMIT 1) FROM ot GROUP BY k", true},
	// The same statement with the alias that makes rule 2 fire: accepted by both.
	{"correlated-arm-aliased",
		"SELECT k, (SELECT ot.q AS q UNION SELECT 9 ORDER BY q LIMIT 1) FROM ot GROUP BY k", false},
	// And with the term as an ORDINAL (rule 1): accepted by both.
	{"correlated-arm-ordinal",
		"SELECT k, (SELECT ot.q UNION SELECT 9 ORDER BY 1 LIMIT 1) FROM ot GROUP BY k", false},
	// Arm 1 supplying q from its OWN FROM is rule 3's supported case: the term
	// resolves against nc.pSrcList and sqlite3ExprCompare matches arm 1.
	{"own-from-control", "SELECT ot.q FROM ot UNION SELECT 9 ORDER BY q", false},
}

func TestR26CompoundOrderByScope(t *testing.T) {
	for _, tc := range coCases {
		t.Run(tc.name, func(t *testing.T) {
			stmts := append(append([]string(nil), coSchema...), tc.q)
			oracle, _ := json.Marshal(run(t, "cgo", stmts)[len(stmts)-1])
			got, _ := json.Marshal(run(t, "musql", stmts)[len(stmts)-1])
			switch {
			case string(got) == string(oracle) && tc.wrong:
				t.Fatalf("%q now MATCHES the oracle -- clear its wrong flag\n  both: %s", tc.q, oracle)
			case string(got) != string(oracle) && !tc.wrong:
				t.Fatalf("%q DIVERGES\n  cgo:    %s\n  musql: %s", tc.q, oracle, got)
			case tc.wrong:
				t.Logf("KNOWN WRONG (compound ORDER BY resolved in a wider scope than resolveCompoundOrderBy allows): %q\n  cgo:    %s\n  musql: %s", tc.q, oracle, got)
			}
		})
	}
}
