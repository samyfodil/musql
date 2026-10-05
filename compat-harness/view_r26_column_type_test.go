// Tests PRAGMA table_info reporting for view result columns: TYPE and NAME.
// Tests against C SQLite's columnType and ColumnsFromExprList rules.
// ONLY when building a TABLE's columns -- a view, a FROM-clause subquery, a
// CTE, CREATE TABLE AS. A top-level "SELECT g COLLATE nocase FROM t9" really
// is named "g COLLATE nocase", and that pair is what the cases below pin.
package compat

import (
	"fmt"
	"testing"
)

var vctSetup = []string{
	`CREATE TABLE t1(a INTEGER, b TEXT)`,
	`CREATE TABLE t2(f NUMERIC, g VARCHAR(9), h DoUbLe)`,
	`CREATE TABLE t3(m MYBLOB, n)`,
	`CREATE TABLE k(id INTEGER PRIMARY KEY, v TEXT)`,
	`INSERT INTO t1 VALUES(1,'p')`,
	`INSERT INTO t2 VALUES(1,'5',2.0)`,
	`INSERT INTO t3 VALUES(x'00',7)`,
	`INSERT INTO k VALUES(1,'a')`,
}

// TestViewColumnTypeAndName runs each view body through four surfaces: the
// reported cid/name/type, the view's own result-set column names, the SAME
// expression as a top-level result set (which must NOT take the peel), and a
// derived table over it (which must).
func TestViewColumnTypeAndName(t *testing.T) {
	for _, body := range []string{
		// COLLATE: transparent to the affinity, invisible to columnType, and
		// peeled out of the name.
		`SELECT g COLLATE nocase FROM t2`,
		`SELECT a COLLATE binary FROM t1`,
		`SELECT m COLLATE binary FROM t3`,
		`SELECT h COLLATE nocase FROM t2`,
		`SELECT g COLLATE nocase COLLATE rtrim FROM t2`,
		`SELECT CAST(a AS INT) COLLATE binary FROM t1`,
		`SELECT a COLLATE binary AS zz FROM t1`,
		// A rowid reference is "INTEGER", every spelling, IPK or not.
		`SELECT rowid FROM t1`,
		`SELECT t1.rowid FROM t1`,
		`SELECT rowid+0 FROM t1`,
		// A scalar subquery recurses into its own first result expression.
		`SELECT (SELECT g FROM t2)`,
		`SELECT (SELECT n FROM t3)`,
		`SELECT (SELECT a+1 FROM t1)`,
		`SELECT (SELECT rowid FROM t1)`,
		// A unary "+" hides the affinity; a unary "-" never had one.
		`SELECT +g FROM t2`,
		`SELECT -a FROM t1`,
		// Controls that were already right and must not move: bare columns of
		// every declared spelling, CASTs, literals, aggregates, an alias.
		`SELECT a,b FROM t1`,
		`SELECT f,g,h FROM t2`,
		`SELECT m,n FROM t3`,
		`SELECT t2.g FROM t2`,
		`SELECT a AS z FROM t1`,
		`SELECT CAST(a AS varchar(3)), CAST(a AS BLOB), CAST(a AS wibble) FROM t1`,
		`SELECT CAST(a AS NUMERIC), CAST(a AS int), CAST(a AS real) FROM t1`,
		`SELECT NULL, 'x', 1, 2.5, x'00'`,
		`SELECT count(*), max(a), a+1, abs(a), upper(b) FROM t1`,
		`SELECT (a) FROM t1`,
	} {
		stmts := append(append([]string{}, vctSetup...), `CREATE VIEW v AS `+body)
		stmts = append(stmts,
			`SELECT cid, name, type FROM pragma_table_info('v')`,
			`SELECT cid, name, type, hidden FROM pragma_table_xinfo('v')`,
			`SELECT * FROM v`,
			// The same expression as a RESULT SET keeps its verbatim text...
			body,
			// ...and as a derived table or a CTE it does not.
			`SELECT * FROM (`+body+`)`,
			`WITH c AS (`+body+`) SELECT * FROM c`,
			`CREATE TABLE u AS `+body,
			`SELECT sql FROM sqlite_master WHERE name='u'`,
		)
		differ(t, body, stmts)
	}
}

// TestViewColumnPreResolutionNaming covers the bodies whose name depends on
// WHEN the column list is built. sqlite3ColumnsFromExprList peels
// likely()/unlikely()/likelihood() only once EP_Unlikely has been set, and
// resolves a rowid alias to its INTEGER PRIMARY KEY column only once the
// reference is a TK_COLUMN -- both of which happen during resolution. A VIEW's
// list is built after resolution (sqlite3ResultSetOfSelect), an INLINE derived
// table's before it (selectExpander), so the same body names its column two
// different things:
//
//	CREATE VIEW w AS SELECT unlikely(h) FROM t9; SELECT * FROM w -> "h"
//	SELECT * FROM (SELECT unlikely(h) FROM t9)                   -> "unlikely(h)"
//	CREATE VIEW w AS SELECT oid FROM k; PRAGMA table_info(w)     -> "id"
//	SELECT * FROM (SELECT oid FROM k)                            -> "oid"
//
// The surfaces asserted here are the ones this engine reproduces. TWO are
// KNOWN WRONG and deliberately absent, both pre-existing and both owned by
// vdbe_join_codegen.go's own view/derived-table column list, which this stream
// does not own:
//
//	SELECT * FROM w        over a likely()-bearing view: "unlikely(h)", not "h"
//	SELECT * FROM (SELECT oid FROM k)                  : "id", not "oid"
func TestViewColumnPreResolutionNaming(t *testing.T) {
	for _, body := range []string{
		`SELECT likely(g) FROM t2`,
		`SELECT unlikely(a) FROM t1`,
		`SELECT likelihood(a,0.5) FROM t2, t1`,
		`SELECT oid FROM k`,
		`SELECT k.oid FROM k`,
		`SELECT _rowid_ FROM k`,
	} {
		stmts := append(append([]string{}, vctSetup...), `CREATE VIEW v AS `+body)
		stmts = append(stmts,
			`SELECT cid, name, type FROM pragma_table_info('v')`,
			`SELECT cid, name, type, hidden FROM pragma_table_xinfo('v')`,
			// The top-level result set keeps generateColumnNames' answer.
			body,
			`CREATE TABLE u AS `+body,
			`SELECT sql FROM sqlite_master WHERE name='u'`,
		)
		differ(t, body, stmts)
	}
}

// TestViewColumnTypeExplicitNames pins the "CREATE VIEW v(p,q)" rename list,
// which replaces the NAME and leaves the TYPE alone.
func TestViewColumnTypeExplicitNames(t *testing.T) {
	for _, c := range []struct{ cols, body string }{
		{`(p)`, `SELECT g COLLATE nocase FROM t2`},
		{`(p,q)`, `SELECT a COLLATE binary, likely(b) FROM t1`},
		{`(p)`, `SELECT rowid FROM t1`},
	} {
		stmts := append(append([]string{}, vctSetup...), fmt.Sprintf(`CREATE VIEW v%s AS %s`, c.cols, c.body))
		stmts = append(stmts,
			`SELECT cid, name, type FROM pragma_table_info('v')`,
			`SELECT * FROM v`)
		differ(t, c.cols+" "+c.body, stmts)
	}
}
