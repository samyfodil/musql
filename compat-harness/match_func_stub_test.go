// Tests the MATCH function stub, which is a global function that always fails
// when evaluated, allowing virtual tables to override it.
package compat

import "testing"

func TestMatchFuncStub(t *testing.T) {
	for _, q := range []string{
		`SELECT match(1,2)`,
		`SELECT match('a','b')`,
		`SELECT 1 WHERE 0 AND match(1,2)`,
		`SELECT match(1)`,
		`SELECT match(1,2,3)`,
	} {
		differ(t, "match_func_stub", []string{q})
	}
}
