package compat

import "testing"

// C evaluates a table's CHECK constraints in integrity_check row by row, so over
// a table without rows a CHECK whose expression raises is never reached and the
// answer is "ok" (func.test func-35.200), by Query and by Exec alike.
func TestIntegrityCheckSkipsTheChecksOfAnEmptyTable(t *testing.T) {
	setup := []string{
		"CREATE TABLE t0(c0 CHECK(ABS(-9223372036854775808)))",
		"CREATE TABLE w0(k PRIMARY KEY, c0 CHECK(ABS(-9223372036854775808))) WITHOUT ROWID",
	}
	differ(t, "integrity empty check", append(append([]string{}, setup...),
		"PRAGMA integrity_check",
		"PRAGMA quick_check",
		"PRAGMA integrity_check(t0)",
		"PRAGMA integrity_check(w0)",
		"PRAGMA quick_check(w0)",
		"INSERT INTO t0 VALUES(1)",
	))
	for _, q := range []string{"PRAGMA integrity_check", "PRAGMA quick_check(w0)"} {
		eoqDiffer(t, setup, q, "")
	}
}
