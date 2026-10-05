// Tests the result-column naming pragmas through the driver layer.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// colNameSchema is deliberately not one table with one column: full_column_names
// qualifies with the REAL table name while short_column_names=OFF's "*" arm
// qualifies with the FROM item's alias, and a single unaliased table cannot tell
// those apart. Two tables sharing a column name pin the join shapes.
var colNameSchema = []string{
	`CREATE TABLE t1(a INTEGER, b TEXT)`,
	`CREATE TABLE t2(a INTEGER, c TEXT)`,
	`INSERT INTO t1 VALUES(1,'x'),(2,'y')`,
	`INSERT INTO t2 VALUES(1,'p'),(3,'q')`,
}

// colNameShapes are the SELECTs whose result-column NAMES the two pragmas move.
// Each is run under all four (full, short) settings.
var colNameShapes = []string{
	`SELECT a FROM t1 ORDER BY a`,
	`SELECT t1.a FROM t1 ORDER BY 1`,
	`SELECT a AS z FROM t1 ORDER BY 1`,      // an AS alias wins under every setting
	`SELECT a, b FROM t1 ORDER BY 1`,        // two column refs from one table
	`SELECT * FROM t1 ORDER BY 1`,           // "*" expansion
	`SELECT t1.* FROM t1 ORDER BY 1`,        // qualified "*"
	`SELECT a FROM t1 AS x ORDER BY 1`,      // full_column_names uses the REAL name, not "x"
	`SELECT x.a FROM t1 AS x ORDER BY 1`,    //   ...even when written through the alias
	`SELECT * FROM t1 AS x ORDER BY 1`,      // "*" under short=OFF uses the ALIAS
	`SELECT a+1 FROM t1 ORDER BY 1`,         // an expression keeps its source span
	`SELECT abs(a) FROM t1 ORDER BY 1`,      //   ...including a function call
	`SELECT t1.a, t2.a FROM t1, t2 ORDER BY 1, 2`,
	`SELECT * FROM t1, t2 ORDER BY 1`,
	`SELECT * FROM t1 JOIN t2 ON t1.a=t2.a ORDER BY 1`,
	`SELECT t1.a, c FROM t1 JOIN t2 ON t1.a=t2.a ORDER BY 1`,
	`SELECT a FROM t1 UNION ALL SELECT a FROM t2 ORDER BY 1`, // a compound names from its LEFT arm
}

// colNameCombos are the four (full_column_names, short_column_names) settings.
// A fresh connection is (OFF, ON): main.c's openDatabase seeds db->flags with
// SQLITE_ShortColNames and not SQLITE_FullColNames.
var colNameCombos = []struct {
	name  string
	setup []string
}{
	{"default", nil},
	{"full=ON", []string{`PRAGMA full_column_names=ON`}},
	{"short=OFF", []string{`PRAGMA short_column_names=OFF`}},
	{"full=ON,short=OFF", []string{`PRAGMA full_column_names=ON`, `PRAGMA short_column_names=OFF`}},
	{"short=OFF,full=ON", []string{`PRAGMA short_column_names=OFF`, `PRAGMA full_column_names=ON`}},
}

// TestTailR31ColumnNamePragmas is the battery: every shape under every combo.
func TestTailR31ColumnNamePragmas(t *testing.T) {
	for _, combo := range colNameCombos {
		for i, shape := range colNameShapes {
			stmts := append([]string{}, colNameSchema...)
			stmts = append(stmts, combo.setup...)
			stmts = append(stmts, shape)
			differ(t, fmt.Sprintf("colnames %s #%d", combo.name, i), stmts)
		}
	}
}

// TestTailR31ColumnNamePragmaGetters gates the GETTERs against the oracle's own
// answers, and -- the part the driver used to lose -- that a setter is still
// readable from the NEXT statement. pragma.c's PragTyp_FLAG getter arm is
// returnSingleInt over db->flags, one INTEGER column named after the pragma.
func TestTailR31ColumnNamePragmaGetters(t *testing.T) {
	differ(t, "colnames getters", []string{
		`PRAGMA full_column_names`,  // fresh connection: 0
		`PRAGMA short_column_names`, //                   1
		`PRAGMA full_column_names=ON`,
		`PRAGMA full_column_names`,
		`PRAGMA short_column_names`,
		`PRAGMA short_column_names=OFF`,
		`PRAGMA full_column_names`,
		`PRAGMA short_column_names`,
		`PRAGMA full_column_names=OFF`,
		`PRAGMA short_column_names=ON`,
		`PRAGMA full_column_names`,
		`PRAGMA short_column_names`,
	})
	// The setting must survive statements that open and close whole sessions --
	// DDL, DML, and an explicit transaction -- since that is exactly what a
	// per-statement engine session forgets.
	differ(t, "colnames survive intervening statements", []string{
		`PRAGMA full_column_names=ON`,
		`CREATE TABLE t1(a INTEGER, b TEXT)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA full_column_names`,
		`SELECT a FROM t1`,
		`BEGIN`,
		`INSERT INTO t1 VALUES(2,'y')`,
		`SELECT a FROM t1 ORDER BY a`,
		`ROLLBACK`,
		`PRAGMA full_column_names`,
		`SELECT a FROM t1 ORDER BY a`,
	})
	// A setter issued INSIDE a transaction: pragma.c masks only
	// SQLITE_ForeignKeys out of a db->flags write while autoCommit is 0
	// (PragTyp_FLAG's "if( db->autoCommit==0 ){ mask &= ~(SQLITE_ForeignKeys); }"),
	// so these two take effect at once and a ROLLBACK does not undo them.
	differ(t, "colnames setter inside a transaction", []string{
		`CREATE TABLE t1(a INTEGER, b TEXT)`,
		`BEGIN`,
		`PRAGMA full_column_names=ON`,
		`PRAGMA full_column_names`,
		`SELECT a FROM t1`,
		`ROLLBACK`,
		`PRAGMA full_column_names`,
		`SELECT a FROM t1`,
	})
	// The SAME statement text either side of a setting change. This is the shape
	// a compiled-plan cache gets wrong: musql's warm read pager memoizes a
	// Program by SQL TEXT and the Program carries its own ColNames, so without
	// the invalidation in engine's SetFullColumnNames the second and third
	// SELECTs here replay the first one's names. Byte-identical text is
	// load-bearing -- vary it and the cache misses and the bug hides.
	differ(t, "colnames across a flag change, identical statement text", []string{
		`CREATE TABLE t1(a INTEGER, b TEXT)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`SELECT a, b FROM t1`,
		`PRAGMA full_column_names=ON`,
		`SELECT a, b FROM t1`,
		`PRAGMA short_column_names=OFF`,
		`SELECT a, b FROM t1`,
		`PRAGMA full_column_names=OFF`,
		`SELECT a, b FROM t1`,
		`PRAGMA short_column_names=ON`,
		`SELECT a, b FROM t1`,
	})
	// Same, for the "*" arm, whose short_column_names=OFF spelling qualifies with
	// the FROM item's ALIAS rather than the real table name.
	differ(t, "colnames across a flag change, star over an alias", []string{
		`CREATE TABLE t1(a INTEGER, b TEXT)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`SELECT * FROM t1 AS q`,
		`PRAGMA full_column_names=ON`,
		`SELECT * FROM t1 AS q`,
		`PRAGMA short_column_names=OFF`,
		`SELECT * FROM t1 AS q`,
	})
	// A schema QUALIFIER is ignored: PragTyp_FLAG writes db->flags, which is per
	// connection and not per database (see engine's attachedPragmaScope).
	differ(t, "colnames qualifier is ignored", []string{
		`CREATE TABLE t1(a INTEGER, b TEXT)`,
		`PRAGMA temp.full_column_names=ON`,
		`PRAGMA full_column_names`,
		`PRAGMA main.full_column_names`,
		`SELECT a FROM t1`,
	})
}
