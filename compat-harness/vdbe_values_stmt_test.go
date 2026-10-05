// Vdbe_values_stmt_test gates top-level VALUES statements.
package compat

import "testing"

// TestValuesStatementParity verifies VALUES statements work correctly.
func TestValuesStatementParity(t *testing.T) {
	driverParity(t, "values-stmt", []string{
		`VALUES(1),(2)`,
		`VALUES(1)`,
		`VALUES(1,'a'),(2,'b')`,
		`VALUES(NULL),(1)`,
		`VALUES(1+1)`,
		// A bare VALUES is a select-CORE, and ORDER BY/LIMIT attach to a select-
		// STATEMENT, so SQLite refuses both outright. The engine must refuse them
		// too: answering rows here would be a wrong answer, not a missing feature.
		// The guard keys off whether a compound was WRITTEN, because
		// parseValuesSelectCore desugars a multi-tuple VALUES into "SELECT ...
		// UNION ALL SELECT ..." and so populates Compound itself.
		`VALUES(1),(2) ORDER BY 1 DESC`,
		`VALUES(1),(2) LIMIT 1`,
		`VALUES(1) ORDER BY 1`,
		`VALUES(1) LIMIT 1`,
		// A real compound DOES take them.
		`VALUES(1) UNION ALL VALUES(2)`,
		`VALUES(1) UNION ALL SELECT 2 ORDER BY 1 DESC`,
		// And the CTE spelling, which worked before this and must keep working.
		`WITH c AS (VALUES(1),(2)) SELECT * FROM c ORDER BY 1 DESC`,
	})
	// NOT covered here: "SELECT * FROM (VALUES(1),(2))" -- a VALUES used as a
	// DERIVED TABLE. That is a separate, PRE-EXISTING gap (verified failing on
	// the tree before this change), not something this introduced.
}
