// Tests overflow handling of hex literals in trigger bodies. C SQLite defers
// validation to code generation (per firing), not parse time.
package compat

import "testing"

// TestTriggerGHexLiteralOverflow verifies hex overflow checking happens at
// code generation, not parse time, for trigger bodies
func TestTriggerGHexLiteralOverflow(t *testing.T) {
	flLockstep(t, "create-trigger-accepts-hex-overflow", []string{
		`CREATE TABLE t4(x)`,
		`CREATE TRIGGER tr4 AFTER INSERT ON t4 BEGIN SELECT 0x2147483648e0e0099 AS y WHERE y; END`,
		// Firing INSERT must reject the overflow
		`INSERT INTO t4 VALUES(1)`,
	}, `SELECT * FROM t4`)

	// Magnitude check isolation: both reject firing INSERT
	flLockstep(t, "firing-insert-rejects-hex-overflow", []string{
		`CREATE TABLE t4(x)`,
		`CREATE TRIGGER tr4 AFTER INSERT ON t4 BEGIN SELECT 0x2147483648e0e0099; END`,
		`INSERT INTO t4 VALUES(1)`,
	}, `SELECT * FROM t4`)
}
