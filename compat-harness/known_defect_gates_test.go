// Gates for known defects that were probed against C SQLite, confirmed as wrong answers, and then fixed.
// Each gate is written so reverting the fix makes it fail.
package compat

import (
	"context"
	"database/sql"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// TestAttachedPragmaObjectThroughDriver tests pragma lookups on attached databases without schema qualifiers.
func TestAttachedPragmaObjectThroughDriver(t *testing.T) {
	pureC, mattnC := crossConns(t, "attachpragma")

	for _, s := range []string{
		"CREATE TABLE aux.other (x INTEGER PRIMARY KEY)",
		"CREATE TABLE aux.at (a INTEGER, b TEXT, c INTEGER REFERENCES other(x))",
		"CREATE INDEX aux.ai ON at(b)",
		"CREATE UNIQUE INDEX aux.ai2 ON at(a,b)",
		// An object in the SECOND attached database: the search must reach past
		// the first one, in attach order.
		"CREATE TABLE aux2.far (p INTEGER, q TEXT)",
		"CREATE INDEX aux2.fi ON far(q)",
		// Main shadows attached table of the same name.
		"CREATE TABLE at2 (m INTEGER, n INTEGER)",
		"CREATE TABLE aux.at2 (zzz INTEGER)",
		"CREATE INDEX aux.at2i ON at2(zzz)",
	} {
		execBothConn(t, pureC, mattnC, "setup", s)
	}

	for _, tc := range []struct{ label, sql string }{
		{"index_list tvf", "SELECT * FROM pragma_index_list('at') ORDER BY name"},
		{"index_info tvf", "SELECT * FROM pragma_index_info('ai') ORDER BY seqno"},
		{"index_xinfo bare", "PRAGMA index_xinfo('ai')"},
		{"table_info tvf", "SELECT * FROM pragma_table_info('at') ORDER BY cid"},
		{"table_xinfo tvf", "SELECT * FROM pragma_table_xinfo('at') ORDER BY cid"},
		{"foreign_key_list tvf", "SELECT * FROM pragma_foreign_key_list('at')"},
		{"index_list bare", "PRAGMA index_list('at')"},
		{"index_info bare", "PRAGMA index_info('ai2')"},
		{"table_info bare", "PRAGMA table_info('at')"},
		{"second attached db", "SELECT * FROM pragma_index_list('far') ORDER BY name"},
		{"main shadows aux (empty is right)", "SELECT * FROM pragma_index_list('at2') ORDER BY name"},
		{"main shadows aux, columns", "SELECT * FROM pragma_table_info('at2') ORDER BY cid"},
		{"tvf joined to its own table", "SELECT il.name FROM pragma_index_list('at') il ORDER BY il.name"},
		{"tvf in a subquery", "SELECT count(*) FROM (SELECT * FROM pragma_table_info('at'))"},
		{"no such object stays empty", "SELECT * FROM pragma_index_list('nosuchtable')"},
	} {
		queryBothConn(t, pureC, mattnC, tc.label, tc.sql)
	}

	// In-transaction lookup with attachments.
	ctx := context.Background()
	pureTx, err := pureC.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("pure BeginTx: %v", err)
	}
	mattnTx, err := mattnC.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("mattn BeginTx: %v", err)
	}
	txQuery := "SELECT * FROM pragma_index_list('at') ORDER BY name"
	pRows, pErr := pureTx.QueryContext(ctx, txQuery)
	mRows, mErr := mattnTx.QueryContext(ctx, txQuery)
	switch {
	case pErr != nil:
		t.Errorf("in-transaction pragma: musql errored: %v", pErr)
	case mErr != nil:
		t.Errorf("in-transaction pragma: mattn errored: %v", mErr)
	default:
		pCols, pOut := collectRows(t, pRows)
		mCols, mOut := collectRows(t, mRows)
		if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
			t.Errorf("in-transaction pragma DIVERGES: %s\n  pure:  %v %v\n  mattn: %v %v", reason, pCols, pOut, mCols, mOut)
		}
	}
	if pRows != nil {
		pRows.Close()
	}
	if mRows != nil {
		mRows.Close()
	}
	pureTx.Rollback()
	mattnTx.Rollback()
}

// TestDerivedDuplicateColumnNames tests duplicate column names in derived and parenthesized FROM items.
func TestDerivedDuplicateColumnNames(t *testing.T) {
	pureDB, mattnDB, _, _ := openPair(t, "derivedupnames")
	for _, s := range []string{
		"CREATE TABLE t1 (a INTEGER, b TEXT)",
		"CREATE TABLE t1b (a INTEGER, b TEXT)",
		"CREATE TABLE t2 (c INTEGER, d INTEGER)",
		"CREATE TABLE t3 (c INTEGER, e INTEGER)",
		"CREATE TABLE t4 (c INTEGER, f INTEGER)",
		"INSERT INTO t1 VALUES (1,'x'),(2,'y')",
		"INSERT INTO t1b VALUES (1,'p'),(3,'q')",
		"INSERT INTO t2 VALUES (1,2),(3,4)",
		"INSERT INTO t3 VALUES (1,9),(5,6)",
		"INSERT INTO t4 VALUES (1,7)",
	} {
		execBoth(t, pureDB, mattnDB, "setup", s)
	}

	// Duplicates across FROM items run and match.
	for _, tc := range []struct{ label, sql string }{
		{"paren table joined", "SELECT * FROM (t2) AS x JOIN t3 ON x.c=t3.c"},
		{"paren table unaliased", "SELECT * FROM (t2) JOIN t3 ON t2.c=t3.c"},
		{"two paren tables comma", "SELECT * FROM (t1),(t1b) ORDER BY 1,2,3,4"},
		{"two paren tables join", "SELECT * FROM (t1) JOIN (t1b) ON t1.a=t1b.a"},
		{"paren join group", "SELECT * FROM (t2 JOIN t3 ON t2.c=t3.c)"},
		{"paren cross join group", "SELECT * FROM (t2 CROSS JOIN t3) ORDER BY 1,2,3,4"},
		{"paren join group aliased members", "SELECT * FROM (t2 AS p JOIN t3 AS q ON p.c=q.c)"},
		{"paren group joined again", "SELECT * FROM (t2 JOIN t3 ON t2.c=t3.c) JOIN t2 AS t2b ON 1 ORDER BY 1,2,3,4,5,6"},
		{"derived table without internal dup", "SELECT * FROM (SELECT * FROM t2) AS x JOIN t3 ON x.c=t3.c"},
		{"paren tables aliased comma", "SELECT * FROM (t2) a,(t3) b ORDER BY 1,2,3,4"},
		{"paren left join", "SELECT * FROM (t2) LEFT JOIN (t3) ON t2.c=t3.c ORDER BY 1,2"},
		{"outer projection avoids the dup", "SELECT d FROM (SELECT * FROM t2 JOIN t3 ON t2.c=t3.c)"},
	} {
		assertRunsAndMatches(t, pureDB, mattnDB, tc.label, tc.sql)
	}

	// Internal duplicates within FROM items run and match names and values.
	for _, tc := range []struct{ label, sql string }{
		{"derived table internal dup", "SELECT * FROM (SELECT * FROM t2 JOIN t3 ON t2.c=t3.c)"},
		{"derived table internal dup aliased", "SELECT * FROM (SELECT * FROM t2 JOIN t3 ON t2.c=t3.c) AS z"},
		{"derived explicit qualified dup", "SELECT * FROM (SELECT t2.c, t3.c FROM t2 JOIN t3 ON t2.c=t3.c)"},
		{"derived duplicate aliases", "SELECT * FROM (SELECT 1 AS c, 2 AS c)"},
		{"derived internal dup then joined", "SELECT * FROM (SELECT * FROM t2 JOIN t3 ON t2.c=t3.c) JOIN t4 ON t4.c=1"},
	} {
		assertRunsAndMatches(t, pureDB, mattnDB, tc.label, tc.sql)
	}
}

// assertRunsAndMatches is queryBoth with a non-fatal musql error, so a whole
// table of shapes reports how many of them a regression broke rather than
// stopping at the first one.
func assertRunsAndMatches(t *testing.T, pureDB, mattnDB *sql.DB, label, query string) {
	t.Helper()
	pRows, pErr := pureDB.Query(query)
	if pErr != nil {
		t.Errorf("%s: musql DECLINED a shape C SQLite names plainly\n  sql: %s\n  err: %v", label, query, pErr)
		return
	}
	defer pRows.Close()
	pCols, pOut := collectRows(t, pRows)
	mRows, mErr := mattnDB.Query(query)
	if mErr != nil {
		t.Fatalf("%s: mattn Query(%q): %v", label, query, mErr)
	}
	defer mRows.Close()
	mCols, mOut := collectRows(t, mRows)
	if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
		t.Errorf("%s: DIVERGES from mattn\n  sql:   %s\n  reason: %s\n  pure:  cols=%v rows=%v\n  mattn: cols=%v rows=%v",
			label, query, reason, pCols, pOut, mCols, mOut)
	}
}


// TestSchemaCatalogCreationOrder tests sqlite_master reports creation order, not sorted by kind.
func TestSchemaCatalogCreationOrder(t *testing.T) {
	// No ORDER BY: b-tree scan order is what is under test.
	const catalogQ = "SELECT type, name, tbl_name FROM sqlite_master"

	for _, tc := range []struct {
		name  string
		setup []string
	}{
		{"interleaved kinds", []string{
			"CREATE TABLE t1 (a INTEGER, b TEXT)",
			"CREATE INDEX i1 ON t1(b)",
			"CREATE TABLE t2 (x INTEGER)",
			"CREATE INDEX i2 ON t2(x)",
			"CREATE VIEW v1 AS SELECT * FROM t1",
			"CREATE TABLE t3 (y INTEGER PRIMARY KEY, z TEXT UNIQUE)",
			"CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN UPDATE t2 SET x=1; END",
			"CREATE INDEX i1b ON t1(a)",
		}},
		{"automatic indexes follow their table", []string{
			"CREATE TABLE t1 (a UNIQUE, b UNIQUE, c)",
			"CREATE TABLE t2 (d INTEGER)",
			"CREATE INDEX i1 ON t1(c)",
		}},
		{"sqlite_sequence takes the first AUTOINCREMENT table's slot", []string{
			"CREATE TABLE b (z INTEGER)",
			"CREATE INDEX bi ON b(z)",
			"CREATE TABLE a (x INTEGER PRIMARY KEY AUTOINCREMENT, y TEXT)",
			"CREATE TABLE c (w INTEGER)",
		}},
		{"CREATE TABLE AS SELECT", []string{
			"CREATE TABLE src (a INTEGER, b TEXT)",
			"INSERT INTO src VALUES (1,'p')",
			"CREATE INDEX si ON src(b)",
			"CREATE TABLE dst AS SELECT * FROM src",
			"CREATE TABLE z (q INTEGER)",
		}},
		{"ANALYZE's sqlite_stat1", []string{
			"CREATE TABLE t1 (a INTEGER)",
			"CREATE INDEX i1 ON t1(a)",
			"INSERT INTO t1 VALUES (1),(2)",
			"ANALYZE",
			"CREATE TABLE t2 (b INTEGER)",
		}},
		{"fts4 shadow tables follow their virtual table", []string{
			"CREATE TABLE pre (a INTEGER)",
			"CREATE VIRTUAL TABLE ft USING fts4(content)",
			"CREATE TABLE post (b INTEGER)",
		}},
		{"ALTER TABLE RENAME keeps the slot", []string{
			"CREATE TABLE t1 (a INTEGER)",
			"CREATE INDEX i1 ON t1(a)",
			"CREATE TABLE t2 (b INTEGER)",
			"ALTER TABLE t1 RENAME TO t1new",
			"CREATE TABLE t3 (c INTEGER)",
		}},
		{"DROP then re-CREATE takes a fresh slot", []string{
			"CREATE TABLE t1 (a INTEGER)",
			"CREATE TABLE t2 (b INTEGER)",
			"CREATE INDEX i2 ON t2(b)",
			"DROP TABLE t1",
			"CREATE TABLE t1 (a INTEGER)",
			"CREATE INDEX i1 ON t1(a)",
		}},
		{"WITHOUT ROWID (its PK index has no row of its own)", []string{
			"CREATE TABLE t1 (a INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID",
			"CREATE TABLE t2 (c INTEGER)",
			"CREATE INDEX i1 ON t1(b)",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pureDB, mattnDB, _, _ := openPair(t, "catorder")
			execBothNoResult(t, pureDB, mattnDB, tc.setup)
			assertRunsAndMatches(t, pureDB, mattnDB, tc.name, catalogQ)
		})
	}

	// The reported defect was "SELECT type,name,tbl_name FROM aux.sqlite_master"
	// -- an ATTACHed database's catalog, which the driver routes to that file.
	t.Run("attached database catalog", func(t *testing.T) {
		pureC, mattnC := crossConns(t, "catorderaux")
		for _, s := range []string{
			"CREATE TABLE aux.t1 (a INTEGER, b TEXT)",
			"CREATE INDEX aux.i1 ON t1(b)",
			"CREATE TABLE aux.t2 (x INTEGER)",
			"CREATE INDEX aux.i2 ON t2(x)",
			"CREATE VIEW aux.v1 AS SELECT * FROM t1",
			"CREATE TABLE aux.t3 (y INTEGER PRIMARY KEY, z TEXT UNIQUE)",
		} {
			execBothConn(t, pureC, mattnC, "setup", s)
		}
		queryBothConn(t, pureC, mattnC, "aux catalog order",
			"SELECT type, name, tbl_name FROM aux.sqlite_master")
	})

	// Reopening preserves creation order; new objects land after all existing ones.
	t.Run("survives a reopen", func(t *testing.T) {
		pureDB, mattnDB, purePath, mattnPath := openPair(t, "catorderreopen")
		execBothNoResult(t, pureDB, mattnDB, []string{
			"CREATE TABLE t1 (a INTEGER)",
			"CREATE INDEX i1 ON t1(a)",
			"CREATE TABLE t2 (b INTEGER, c TEXT UNIQUE)",
			"CREATE VIEW v1 AS SELECT * FROM t1",
			"CREATE INDEX i1b ON t1(a,a)",
			"CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN UPDATE t2 SET b=1; END",
		})
		pureDB.Close()
		mattnDB.Close()

		reopened := func(driverName, path, tag string) *sql.DB {
			db, err := sql.Open(driverName, path)
			if err != nil {
				t.Fatalf("%s reopen: %v", tag, err)
			}
			t.Cleanup(func() { db.Close() })
			return db
		}
		pure2 := reopened(driver.DriverName, purePath, "pure")
		mattn2 := reopened("sqlite3", mattnPath, "mattn")
		assertRunsAndMatches(t, pure2, mattn2, "reopen", catalogQ)
		execBothNoResult(t, pure2, mattn2, []string{"CREATE TABLE t3 (z INTEGER)", "CREATE INDEX i3 ON t3(z)"})
		assertRunsAndMatches(t, pure2, mattn2, "reopen then create", catalogQ)
	})
}

// execBothNoResult runs setup on both drivers without comparing RowsAffected.
func execBothNoResult(t *testing.T, pureDB, mattnDB *sql.DB, stmts []string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := pureDB.Exec(s); err != nil {
			t.Fatalf("setup: pure Exec(%q): %v", s, err)
		}
		if _, err := mattnDB.Exec(s); err != nil {
			t.Fatalf("setup: mattn Exec(%q): %v", s, err)
		}
	}
}
