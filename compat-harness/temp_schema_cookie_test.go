// Tests PRAGMA schema_version across temp DDL. Schema cookie must be separate
// from temp cache generation to avoid spurious bumps.
package compat

import (
	"fmt"
	"testing"
)

// TestTempSchemaCookie checks schema_version parity
func TestTempSchemaCookie(t *testing.T) {
	for i, setup := range [][]string{
		nil,
		{`CREATE TABLE a(x)`},
		{`CREATE TABLE a(x)`, `CREATE TABLE b(y)`},
		// Temp DDL cases
		{`CREATE TEMP TABLE tt(z)`},
		{`CREATE TABLE a(x)`, `CREATE TEMP TABLE tt(z)`},
		{`CREATE TABLE a(x)`, `CREATE TEMP TABLE tt(z)`, `DROP TABLE tt`},
		{`CREATE TEMP VIEW tv AS SELECT 1`},
		{`CREATE TABLE a(x)`, `CREATE TEMP VIEW tv AS SELECT * FROM a`},
		{`CREATE TABLE a(x)`, `CREATE TEMP VIEW tv AS SELECT * FROM a`, `DROP VIEW tv`},
		{`CREATE TEMP TABLE tt(z)`, `CREATE TEMP TABLE tu(w)`, `DROP TABLE tt`, `DROP TABLE tu`},
		// Interleaved persistent DDL
		{`CREATE TEMP TABLE tt(z)`, `CREATE TABLE a(x)`},
		{`CREATE TABLE a(x)`, `CREATE TEMP TABLE tt(z)`, `CREATE TABLE b(y)`},
		// Persistent DDL control cases
		{`CREATE TABLE a(x)`, `CREATE INDEX ax ON a(x)`},
		{`CREATE TABLE a(x)`, `CREATE VIEW av AS SELECT * FROM a`},
		{`CREATE TABLE a(x)`, `CREATE TRIGGER tr AFTER INSERT ON a BEGIN SELECT 1; END`},
		{`CREATE TABLE a(x)`, `ALTER TABLE a ADD COLUMN y`},
		{`CREATE TABLE a(x)`, `ALTER TABLE a RENAME TO a2`},
		{`CREATE TABLE a(x)`, `DROP TABLE a`},
		{`CREATE TABLE a(x)`, `INSERT INTO a VALUES(1)`}, // DML: no bump at all
		{`CREATE TABLE a(x)`, `CREATE TABLE b(y)`, `DROP TABLE a`},
	} {
		prDifferQ(t, fmt.Sprintf("[%d] schema_version", i), setup, `PRAGMA schema_version`)
		prDifferQ(t, fmt.Sprintf("[%d] main.schema_version", i), setup, `PRAGMA main.schema_version`)
	}
}

// TestTempSchemaCookieStillInvalidates verifies that cached plans are still
// invalidated when temp objects are recreated
func TestTempSchemaCookieStillInvalidates(t *testing.T) {
	cfaDiffer(t, "temp-recreate", []string{
		`CREATE TEMP TABLE tt(a, b)`,
		`INSERT INTO tt VALUES(1,2)`,
		`SELECT a, b FROM tt`,
		`DROP TABLE tt`,
		`CREATE TEMP TABLE tt(a, b, c)`,
		`INSERT INTO tt VALUES(3,4,5)`,
		`SELECT a, b, c FROM tt`,
		`SELECT count(*) FROM tt`,
	})
	// Same statement text across recreation
	cfaDiffer(t, "temp-recreate-same-text", []string{
		`CREATE TABLE keep(v)`,
		`CREATE TEMP TABLE tt(a)`,
		`INSERT INTO tt VALUES(1)`,
		`DROP TABLE tt`,
		`CREATE TEMP TABLE tt(a)`,
		`INSERT INTO tt VALUES(1)`,
		`SELECT count(*) FROM tt`,
	})
	// Temp table shadowing main table
	cfaDiffer(t, "temp-shadows-main", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE TEMP TABLE t(a)`,
		`INSERT INTO t VALUES(2)`,
		`SELECT a FROM t ORDER BY a`,
		`SELECT a FROM main.t ORDER BY a`,
		`SELECT a FROM temp.t ORDER BY a`,
	})
}
