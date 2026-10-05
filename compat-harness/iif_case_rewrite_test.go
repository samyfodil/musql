// Tests iif() and if() rewriting to CASE expressions, with variable argument
// counts.
package compat

import "testing"

func TestIifCaseRewrite(t *testing.T) {
	for _, q := range []string{
		`SELECT if(1,2,3)`,
		`SELECT if(1,2)`,
		`SELECT if(0,2)`,
		`SELECT if(1,2,3,4)`,
		`SELECT if(0,2,1,5)`,
		`SELECT if(0,2,0,5,99)`,
		`SELECT iif(1,2)`,
		`SELECT iif(1,2,3)`,
		`SELECT IIF(NULL,2,3)`,
		`SELECT count(*) FROM (SELECT 1) WHERE if(1,1,0)`,
	} {
		differ(t, "iif_case_rewrite", []string{q})
	}
}

// TestIifArityStaysDeclined verifies that calls with insufficient arguments
// are properly declined.
func TestIifArityStaysDeclined(t *testing.T) {
	for _, q := range []string{
		`SELECT if(1)`,
		`SELECT if()`,
		`SELECT iif(1)`,
	} {
		differ(t, "iif_arity_stays_declined", []string{q})
	}
}
