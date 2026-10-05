// Tests PRAGMA integrity_check and quick_check arguments.
package compat

import (
	"fmt"
	"testing"
)

var icaSetup = []string{
	`CREATE TABLE real1(a INTEGER PRIMARY KEY, b)`,
	`INSERT INTO real1 VALUES(1,'x')`,
	`CREATE INDEX real1b ON real1(b)`,
	`CREATE VIEW v1 AS SELECT * FROM real1`,
	`CREATE TEMP TABLE tmp1(z)`,
}

func TestIntegrityCheckArgument(t *testing.T) {
	for _, pragma := range []string{"integrity_check", "quick_check"} {
		for _, arg := range []string{
			// a table that does not exist, in every quoting
			`(nope)`, `('nope')`, `("nope")`, `([nope])`,
			// a real table, a real INDEX (a table is required, so an index
			// name is rejected), a VIEW, and a TEMP table
			`(real1)`, `('real1')`, `(real1b)`, `(v1)`, `(tmp1)`,
			// the schema catalogs, which are always acceptable names
			`(sqlite_master)`, `(sqlite_schema)`,
			// a NUMERIC argument is a max-error count, not a table
			`(3)`, `(0)`, `('3')`,
			// no argument at all
			``,
		} {
			q := fmt.Sprintf(`PRAGMA %s%s`, pragma, arg)
			differ(t, q, append(append([]string{}, icaSetup...), q))
		}
	}
}

// TestIntegrityCheckArgumentQualified covers the schema-qualified spellings,
// which take the same argument and resolve it in that database.
func TestIntegrityCheckArgumentQualified(t *testing.T) {
	for _, q := range []string{
		`PRAGMA main.quick_check(nope)`,
		`PRAGMA main.quick_check(real1)`,
		`PRAGMA main.integrity_check(nope)`,
		`PRAGMA temp.quick_check(tmp1)`,
		`PRAGMA temp.quick_check(nope)`,
	} {
		differ(t, q, append(append([]string{}, icaSetup...), q))
	}
}
