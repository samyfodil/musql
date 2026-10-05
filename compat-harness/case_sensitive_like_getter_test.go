// This file tests PRAGMA case_sensitive_like (getter form).
// The bare getter returns no rows and has no side effects in C SQLite.
package compat

import "testing"

func TestCaseSensitiveLikeGetterIsANoop(t *testing.T) {
	differ(t, "case_sensitive_like getter", []string{
		`CREATE TABLE t(x)`,
		`INSERT INTO t VALUES('ABC'),('abc')`,
		`PRAGMA case_sensitive_like`,
		`SELECT x FROM t WHERE x LIKE 'abc' ORDER BY x`,
		`PRAGMA case_sensitive_like=ON`,
		`SELECT x FROM t WHERE x LIKE 'abc' ORDER BY x`,
		`PRAGMA case_sensitive_like`,
		`SELECT x FROM t WHERE x LIKE 'abc' ORDER BY x`,
		`PRAGMA case_sensitive_like=OFF`,
		`PRAGMA case_sensitive_like`,
		`SELECT x FROM t WHERE x LIKE 'abc' ORDER BY x`,
		`PRAGMA case_sensitive_like=ON`,
		`SELECT x FROM t WHERE x GLOB 'abc' ORDER BY x`,
		`PRAGMA case_sensitive_like=OFF`,
		`SELECT x FROM t WHERE x GLOB 'abc' ORDER BY x`,
	})
	differ(t, "case_sensitive_like getter and the like() function", []string{
		`PRAGMA case_sensitive_like=ON`,
		`PRAGMA case_sensitive_like`,
		`SELECT like('A','a') AS r`,
		`PRAGMA case_sensitive_like=OFF`,
		`PRAGMA case_sensitive_like`,
		`SELECT like('A','a') AS r`,
	})
	differ(t, "case_sensitive_like exotic values", []string{
		`PRAGMA case_sensitive_like=xyzzy`, `SELECT like('A','a') AS r`,
		`PRAGMA case_sensitive_like=2`, `SELECT like('A','a') AS r`,
		`PRAGMA case_sensitive_like=-1`, `SELECT like('A','a') AS r`,
		`PRAGMA case_sensitive_like=0x10`, `SELECT like('A','a') AS r`,
		`PRAGMA case_sensitive_like=256`, `SELECT like('A','a') AS r`,
	})
}
