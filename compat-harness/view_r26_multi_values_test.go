// This file gates multi-row VALUES clause handling. A multi-row VALUES is one
// compound arm (following SQLite's sqlite3MultiValues rule), not one per row.
// Fallback conditions include WITH clause, non-constant rows, or affinity in
// the first row. Tests exercise all positions and operators.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

var mvSetup = []string{
	`CREATE TABLE t1(a INTEGER, b TEXT)`,
	`CREATE TABLE t2(f NUMERIC, g VARCHAR(9), h DoUbLe)`,
	`CREATE TABLE t3(m MYBLOB, n)`,
	`INSERT INTO t1 VALUES(1,'p')`,
	`INSERT INTO t2 VALUES(1,'5',2.0)`,
	`INSERT INTO t3 VALUES(x'00',7)`,
}

// mvProbe queries the stored schema text, reported type, and actual stored values.
func mvProbe(name string) []string {
	return []string{
		fmt.Sprintf(`SELECT sql FROM sqlite_master WHERE name='%s'`, name),
		fmt.Sprintf(`SELECT name, type FROM pragma_table_info('%s')`, name),
		fmt.Sprintf(`INSERT INTO %s VALUES(5),('5'),(5.0),(x'35'),(NULL)`, name),
		fmt.Sprintf(`SELECT typeof(z)||':'||quote(z) FROM %s ORDER BY 1`, name),
	}
}

// TestMultiValuesCTAS verifies affinity folding in CREATE TABLE ... AS SELECT.
func TestMultiValuesCTAS(t *testing.T) {
	for _, ctas := range []string{
		// Trailing, co-routine: the VALUES contributes no affinity at all, so
		// g's TEXT is demoted by the clause's 0x07 data type.
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION ALL VALUES('x'),('z')`,
		`CREATE TABLE u AS SELECT a AS z FROM t1 UNION ALL VALUES('x'),('z')`,
		// ...and the SELECT-arm spelling of the same rows, which really is a
		// three-arm compound and really does keep TEXT. This pair is the whole
		// point: it must NOT move.
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION ALL SELECT 'x' UNION ALL SELECT 'z'`,
		// Single-row VALUES is not a multi-row VALUES.
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION ALL VALUES('x')`,
		// Condition (d): a CAST in the FIRST row takes the fallback, and the
		// wrapped rows then fold to TEXT, which agrees with g.
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION ALL VALUES(CAST('x' AS TEXT)),('z')`,
		// ...but a CAST in a LATER row does not: condition (d) reads row one only.
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION ALL VALUES('x'),(CAST('z' AS TEXT))`,
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION ALL VALUES('x'),('y'),(CAST('z' AS TEXT))`,
		// Leading position: here the fallback's arms splice in flat, so the
		// two methods genuinely disagree.
		`CREATE TABLE u AS VALUES('x'),('z') UNION ALL SELECT g FROM t2`,
		`CREATE TABLE u AS WITH q(k) AS (SELECT 1) VALUES('x'),('z') UNION ALL SELECT g FROM t2`,
		`CREATE TABLE u AS VALUES(CAST('x' AS TEXT)),('z') UNION ALL SELECT g FROM t2`,
		// Condition (a) in the trailing position, where it changes nothing.
		`CREATE TABLE u AS WITH q(k) AS (SELECT 1) SELECT g AS z FROM t2 UNION ALL VALUES('x'),('z')`,
		// Bare, and bare with a CAST first row (which is where FLEXNUM shows).
		`CREATE TABLE u AS VALUES('x'),('z')`,
		`CREATE TABLE u AS VALUES(1),(2)`,
		`CREATE TABLE u AS VALUES(CAST(1 AS INT)),(2)`,
		`CREATE TABLE u AS VALUES(CAST(1 AS REAL)),(2)`,
		`CREATE TABLE u AS VALUES('x'),(CAST(1 AS INT))`,
		// As a derived table rather than a compound arm.
		`CREATE TABLE u AS SELECT * FROM (VALUES('x'),('z'))`,
		`CREATE TABLE u AS SELECT * FROM (SELECT g FROM t2 UNION ALL VALUES('x'),('z'))`,
		// Three-plus arms around the clause, and a nested compound.
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION ALL VALUES('x'),('z') UNION ALL SELECT a FROM t1`,
		`CREATE TABLE u AS SELECT a AS z FROM t1 UNION ALL SELECT g FROM t2 UNION ALL VALUES('x'),('z')`,
		`CREATE TABLE u AS SELECT * FROM (SELECT g FROM t2 UNION ALL VALUES('x'),('z')) UNION ALL SELECT a FROM t1`,
		// Non-UNION-ALL operators.
		`CREATE TABLE u AS SELECT g AS z FROM t2 UNION VALUES('x'),('z')`,
		`CREATE TABLE u AS SELECT g AS z FROM t2 EXCEPT VALUES('x'),('z')`,
		`CREATE TABLE u AS SELECT g AS z FROM t2 INTERSECT VALUES('x'),('z')`,
		`CREATE TABLE u AS VALUES('x'),('z') UNION SELECT g FROM t2`,
		// A BLOB-affinity column arm, whose scan-stop bit is what tells a
		// typeless column from a literal.
		`CREATE TABLE u AS SELECT m AS z FROM t3 UNION ALL VALUES('x'),('z')`,
		`CREATE TABLE u AS SELECT n AS z FROM t3 UNION ALL VALUES('x'),('z')`,
	} {
		stmts := append(append([]string{}, mvSetup...), ctas)
		stmts = append(stmts, mvProbe("u")...)
		differ(t, ctas, stmts)
	}
}

// TestMultiValuesViewAndDerived verifies affinity folding in views and derived tables.
func TestMultiValuesViewAndDerived(t *testing.T) {
	for _, body := range []string{
		`SELECT g AS z FROM t2 UNION ALL VALUES('x'),('z')`,
		`VALUES('x'),('z') UNION ALL SELECT g AS z FROM t2`,
		`SELECT g AS z FROM t2 UNION ALL VALUES(CAST('x' AS TEXT)),('z')`,
		`SELECT g AS z FROM t2 UNION ALL VALUES('x'),(CAST(1 AS INT))`,
		`SELECT a AS z FROM t1 UNION ALL VALUES('x'),('z')`,
		`SELECT * FROM (VALUES('x'),('z'))`,
	} {
		stmts := append(append([]string{}, mvSetup...), `CREATE VIEW v AS `+body)
		stmts = append(stmts,
			`SELECT count(*) FROM v WHERE z=5`,
			`SELECT count(*) FROM v WHERE z='5'`,
			`SELECT count(*) FROM (`+body+`) WHERE z=5`,
			`SELECT count(*) FROM (`+body+`) WHERE z='5'`,
			`CREATE TABLE u AS SELECT z FROM v`,
			`SELECT sql FROM sqlite_master WHERE name='u'`,
			`CREATE VIEW vv AS SELECT z FROM v UNION ALL SELECT a FROM t1`,
			`SELECT count(*) FROM vv WHERE z=5`,
		)
		differ(t, body, stmts)
	}
}

// mvRowExprs are per-row expressions for fuzzing.
var mvRowExprs = []string{
	`'x'`, `1`, `2.5`, `NULL`, `x'00'`, `'5'`,
	`CAST(1 AS INT)`, `CAST('x' AS TEXT)`, `CAST(1 AS REAL)`, `CAST(1 AS NUMERIC)`,
	`CAST(1 AS BLOB)`, `CAST('x' AS wibble)`,
	`1+1`, `-1`, `+1`, `'a'||'b'`, `'x' COLLATE nocase`,
	`CASE WHEN 1 THEN 'a' ELSE 2 END`, `CASE WHEN 1 THEN 'a' END`,
	`abs(1)`, `upper('x')`,
}

// mvArmExprs are non-VALUES arm expressions.
var mvArmExprs = []string{
	`a`, `b`, `f`, `g`, `h`, `m`, `n`,
	`CAST(a AS TEXT)`, `CAST(a AS INT)`, `CAST(g AS BLOB)`,
	`1`, `'x'`, `2.5`, `NULL`, `x'00'`,
	`a+1`, `abs(a)`, `g||'q'`, `+a`, `-a`, `a COLLATE nocase`,
	`CASE WHEN a>0 THEN a ELSE g END`,
}

var mvOps = []string{" UNION ALL ", " UNION ", " EXCEPT ", " INTERSECT "}

// TestMultiValuesFuzz fuzz-tests affinity folding by randomizing rows, expressions,
// operators, and VALUES position, comparing schema text and stored values.
func TestMultiValuesFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260807))
	for k := 0; k < 140; k++ {
		k := k
		name := fmt.Sprintf("m%03d", k)
		t.Run(name, func(t *testing.T) {
			var rows []string
			for r := 0; r < 2+rng.Intn(3); r++ {
				rows = append(rows, "("+mvRowExprs[rng.Intn(len(mvRowExprs))]+")")
			}
			values := "VALUES" + strings.Join(rows, ",")
			var parts []string
			for a := 0; a < 1+rng.Intn(3); a++ {
				sel := "SELECT " + mvArmExprs[rng.Intn(len(mvArmExprs))]
				if len(parts) == 0 {
					sel += " AS z"
				}
				parts = append(parts, sel+" FROM t1, t2, t3")
			}
			at := rng.Intn(len(parts) + 1)
			parts = append(parts[:at], append([]string{values}, parts[at:]...)...)
			col := "z"
			if at == 0 {
				col = "column1"
			}
			body := strings.Join(parts, mvOps[rng.Intn(len(mvOps))])
			with := ""
			if rng.Intn(4) == 0 {
				with = "WITH q(k) AS (SELECT 1) "
			}
			stmts := append(append([]string{}, mvSetup...), with+`CREATE TABLE u AS `+body)
			stmts = append(stmts,
				`SELECT sql FROM sqlite_master WHERE name='u'`,
				`INSERT INTO u VALUES(5),('5'),(5.0),(x'35'),(NULL)`,
				`SELECT typeof(`+col+`)||':'||quote(`+col+`) FROM u ORDER BY 1`,
				`SELECT count(*) FROM (`+with+body+`) WHERE `+col+`=5`,
				`SELECT count(*) FROM (`+with+body+`) WHERE `+col+`='5'`,
				with+`CREATE VIEW v AS `+body,
				`SELECT name, type FROM pragma_table_info('v')`,
				`SELECT count(*) FROM v WHERE `+col+`=5`,
				`SELECT count(*) FROM v WHERE `+col+`='5'`,
			)
			differ(t, name+" "+body, stmts)
		})
	}
}
