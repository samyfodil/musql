// This file gates aggregates in FROM-less subquery bodies, verifying that
// aggregates either answer in the subquery or re-associate to the enclosing query.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildAggHoistDB creates the fixture database for aggregate hoisting tests.
func buildAggHoistDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agghoist.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a1 INTEGER)`,
		`INSERT INTO t1 VALUES (1), (2), (3)`,
		`CREATE TABLE t2(b1 INTEGER)`,
		`INSERT INTO t2 VALUES (4), (5)`,
		`CREATE TABLE x1(b)`,
		`INSERT INTO x1 VALUES (7), (8)`,
		`CREATE TABLE x2(c)`,
		`INSERT INTO x2 VALUES (1), (2)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// aggHoistFromlessServed: an aggregate in a FROM-less subquery body whose
// argument names no enclosing table (SQLite's -1), so it stays exactly where it
// is written and the enclosing query keeps its own row count. Every one of
// these declined as "vdbe: unsupported: aggregate" before the dispatch existed.
var aggHoistFromlessServed = []string{
	// The shape the bucket is named for.
	`SELECT (SELECT count(*)) FROM t1 ORDER BY t1.rowid`,
	`SELECT (SELECT count()) FROM t1 ORDER BY t1.rowid`,
	// aggnested-2.x: the aggregate's argument is a SUBQUERY of its own, so
	// every table it names is on sqlite3ReferencesSrcList's exclude list --
	// still -1, still stays.
	`SELECT (SELECT total((SELECT b FROM x1))) FROM x2 ORDER BY x2.rowid`,
	`SELECT (SELECT total((SELECT 2 FROM x1))) FROM x2 ORDER BY x2.rowid`,
	// A correlated reference sitting BESIDE the aggregate: count(*) is -1 and
	// stays, a1 is an ordinary correlated column, so the answer is one row per
	// enclosing row. This is what threading the enclosing compile into
	// compileNoFromAggregate buys.
	`SELECT (SELECT count(*)+a1) FROM t1 ORDER BY t1.rowid`,
	`SELECT (SELECT count(*)+t1.a1) FROM t1 ORDER BY t1.rowid`,
	// The single synthetic row this shape scans is still WHERE-gated, and the
	// query still yields exactly one row for the subquery to read.
	`SELECT (SELECT count(*) WHERE 1=0) FROM t1 ORDER BY t1.rowid`,
	// LIMIT 0 suppresses that row, so the scalar subquery is NULL.
	`SELECT (SELECT count(*) LIMIT 0) FROM t1 ORDER BY t1.rowid`,
	// Constant arguments, and an expression combining an aggregate with one.
	`SELECT (SELECT sum(5)+1) FROM t1 ORDER BY t1.rowid`,
	`SELECT (SELECT group_concat(7,'-')) FROM t1 ORDER BY t1.rowid`,
	// Beside an ordinary FROM-ful aggregate subquery, in the same select list.
	`SELECT (SELECT count(*) FROM t2), (SELECT count(*)) FROM t1 ORDER BY t1.rowid`,
	// EXISTS and IN spellings of the same body.
	`SELECT a1 FROM t1 WHERE EXISTS(SELECT count(*)) ORDER BY a1`,
	`SELECT a1 FROM t1 WHERE a1 IN (SELECT count(*)) ORDER BY a1`,
	// A FROM-less aggregate subquery nested two deep.
	`SELECT (SELECT (SELECT count(*))) FROM t1 ORDER BY t1.rowid`,
	// The DERIVED-TABLE spelling of the same body: resolveDerivedSource
	// (engine/vdbe_join_codegen.go) reaches the compiler through the same
	// compileSubProgram arm, so this shape came along with the dispatch.
	`SELECT * FROM (SELECT count(*))`,
	`SELECT * FROM (SELECT count(*) AS n) WHERE n=1`,
	`SELECT * FROM t1, (SELECT count(*)) ORDER BY a1`,
	`SELECT * FROM (SELECT total((SELECT b1 FROM t2)))`,
	`SELECT (SELECT y FROM (SELECT sum(5) AS y)) FROM t1 ORDER BY t1.rowid`,
	// SQLite's 0 -- the aggregate's argument names the ENCLOSING query's table,
	// so it is re-associated OUTWARD and the whole statement collapses to ONE
	// row. Every one of these was in aggHoistFromlessDeclines below until the
	// outward half of the rule was implemented (engine/vdbe_agg_hoist.go); they
	// are checked here for the row COUNT the re-association produces (1, not
	// t1's 3) as well as the value, which is what those decline-assertions
	// existed to protect. No ORDER BY: a re-associated query has one row.
	`SELECT (SELECT sum(a1)) FROM t1`,
	`SELECT (SELECT max(a1)+1) FROM t1`,
	`SELECT (SELECT group_concat(a1)) FROM t1`,
	`SELECT (SELECT count(a1)) FROM t1`,
	`SELECT (SELECT group_concat(a1,'x')) FROM t1`,
	// ...including through a second FROM-less level.
	`SELECT (SELECT (SELECT sum(a1))) FROM t1`,
	// ...and the QUALIFIED spelling, which used to be the one entry left in
	// aggHoistFromlessDeclines: the retry hoisted it and planGroupItem's
	// checkItemSubqueries then declined the item, because it could not tell a
	// call the enclosing query has already claimed from one it has not. It can
	// now (engine/sql_group.go's itemAggHoists), so this answers the same ONE
	// row (3) the unqualified spelling above does.
	`SELECT (SELECT count(t1.a1)) FROM t1`,
}

// aggHoistDerivedRejected is the derived table's own scope BARRIER, which the
// threaded-in enclosing compile must not have punched through: a derived table
// cannot see its sibling FROM items (resolveDerivedSource compiles it beneath
// derivedOuterBarrier), so sum(a1) here names nothing at all. C SQLite
// rejects it "no such column: a1" and so must this engine -- the direct
// counterpart of the "SELECT (SELECT count(*)+a1) FROM t1" case above, where
// the SAME reference IS in scope.
var aggHoistDerivedRejected = []string{
	`SELECT * FROM t1, (SELECT sum(a1))`,
	`SELECT * FROM t1, (SELECT count(*)+a1)`,
}

// aggHoistFromlessDeclines: the SAME shape, still declined. wantRows records
// what C SQLite answers, so the list stays honest evidence rather than an
// unexamined allowlist -- and fails loudly the day one of these starts
// answering, so it can be MOVED to the served list above rather than silently
// left as a gap.
//
// It is now EMPTY. The last entry was the QUALIFIED spelling
// "SELECT (SELECT count(t1.a1)) FROM t1", which moved up when the recording
// half of the enclosing-aggregate hoist landed. The list is kept, rather than
// deleted with its loop, because it is the shape of evidence this rule needs:
// the next spelling that turns out to be reachable belongs here first.
var aggHoistFromlessDeclines = []struct {
	sql      string
	wantRows int
}{}

func TestAggHoistFromlessSubqueryBody(t *testing.T) {
	path := buildAggHoistDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range aggHoistFromlessServed {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE declined where C SQLite answered %d row(s): %v", sqlText, len(cRows), vErr)
			continue
		}
		if len(vVals) != len(cRows) {
			t.Errorf("[%s] QueryVDBE returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(vVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				sqlText, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Errorf("[%s] p.Query declined where C SQLite answered: %v", sqlText, eErr)
			continue
		}
		if len(eVals) != len(cRows) {
			t.Errorf("[%s] p.Query returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(eVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				sqlText, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}

	for _, tc := range aggHoistFromlessDeclines {
		// The oracle half: prove the decline is covering a genuine
		// re-association. t1 has three rows; a re-associated aggregate makes the
		// whole statement one.
		_, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", tc.sql, cErr)
			continue
		}
		if len(cRows) != tc.wantRows {
			t.Errorf("[%s] C SQLite returned %d row(s), the re-association premise says %d -- this gate's oracle evidence no longer holds",
				tc.sql, len(cRows), tc.wantRows)
			continue
		}
		if _, rows, err := p.QueryArgs(tc.sql, nil); err == nil {
			t.Errorf("[%s] QueryVDBE answered %d row(s) where the aggregate re-associates to the enclosing query (C SQLite: %d) -- must decline",
				tc.sql, len(rows), len(cRows))
		}
		if _, rows, err := p.Query(tc.sql); err == nil {
			t.Errorf("[%s] p.Query answered %d row(s) where the aggregate re-associates to the enclosing query -- must decline", tc.sql, len(rows))
		}
	}

	for _, sqlText := range aggHoistDerivedRejected {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Errorf("[%s] C SQLite ACCEPTED this -- the derived-table barrier premise no longer holds", sqlText)
			continue
		}
		if _, rows, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] QueryVDBE answered %d row(s) where C SQLite rejects the statement", sqlText, len(rows))
		}
		if _, rows, err := p.Query(sqlText); err == nil {
			t.Errorf("[%s] p.Query answered %d row(s) where C SQLite rejects the statement", sqlText, len(rows))
		}
	}
}

// ---------------------------------------------------------------------------
// The other rule in the same bucket, and not an aggregate rule at all: a
// DERIVED TABLE's body reaching the query that encloses the query its FROM sits
// in. aggnested-6.x is where it surfaces --
//
//	SELECT (SELECT c FROM (SELECT t2.b AS c FROM t1) GROUP BY c HAVING t2.b)
//	  FROM t2 GROUP BY 'constant_string'
//
// -- which declined "no such table: t2" only when the enclosing query is an
// AGGREGATE or GROUP BY one. The plain row-mode spelling already answered,
// because there the derived table is compiled beneath a live enclosing
// *compiler* and compileColumn's ordinary outer-chain walk finds t2. An
// aggregate query's select-list item is evaluated INTERPRETIVELY instead
// (aggResult -> evalSubquery -> materializeOuterRefs), and
// that rewrite skipped FROM-item derived-table bodies on a premise its own doc
// comment stated and the oracle refutes: "neither can see an enclosing query's
// columns in C SQLite either".
//
// SQLite has no LATERAL, so what a derived table genuinely cannot see is its
// SIBLING FROM items -- which is exactly the CompoundArm rule the rewrite
// already applied, and is why the descent passes the pre-this-level shadow set.
// Both halves are pinned below.
// ---------------------------------------------------------------------------

// buildDerivedReachDB gives the shadowing case real teeth: t4 is a SIBLING FROM
// item aliased "x" that HAS a column b, while the enclosing query's t2 is also
// aliased "x". Only one of the two readings can be right, and they produce
// visibly different values (77 vs 1/2).
func buildDerivedReachDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "derivedreach.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES ('p'), ('q')`,
		`CREATE TABLE t2(b)`,
		`INSERT INTO t2 VALUES (1), (2)`,
		`CREATE TABLE t4(b)`,
		`INSERT INTO t4 VALUES (77)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

var derivedReachServed = []string{
	// aggnested-6.1.1 / 6.1.2, the corpus statements this closes.
	`SELECT (SELECT t2.b FROM (SELECT t2.b AS c FROM t1) GROUP BY 1 HAVING t2.b) FROM t2 GROUP BY 'constant_string'`,
	`SELECT (SELECT c FROM (SELECT t2.b AS c FROM t1) GROUP BY c HAVING t2.b) FROM t2 GROUP BY 'constant_string'`,
	// The same reach with a real GROUP BY key, so the derived table must see
	// THIS group's row rather than one fixed row: (1,2),(2,2) -- the count is
	// t1's two rows, and each group re-materializes the derived table.
	`SELECT b, (SELECT count(*) FROM (SELECT t2.b AS c FROM t1)) FROM t2 GROUP BY b ORDER BY b`,
	`SELECT b, (SELECT c FROM (SELECT t2.b AS c FROM t1 LIMIT 1)) FROM t2 GROUP BY b ORDER BY b`,
	// A whole-table aggregate enclosing query: the anchor row is max(b)'s,
	// so the derived table reads 2, not 1.
	`SELECT max(b), (SELECT c FROM (SELECT t2.b AS c FROM t1 LIMIT 1)) FROM t2`,
	// SHADOWING, the half that must NOT change: "x" names BOTH a sibling FROM
	// item of the derived table (t4 AS x, whose b is 77) and the enclosing
	// query's t2 AS x. SQLite has no LATERAL, so the sibling is not in scope and
	// x.b is the ENCLOSING row's b -- 1 then 2, never 77.
	`SELECT b, (SELECT c FROM (SELECT x.b AS c FROM t1 LIMIT 1), t4 AS x) FROM t2 AS x GROUP BY b ORDER BY b`,
	// ...and a qualifier the derived table's OWN FROM supplies still binds
	// there, not outward.
	`SELECT b, (SELECT c FROM (SELECT t1.a AS c FROM t1 LIMIT 1)) FROM t2 GROUP BY b ORDER BY b`,
	// Row mode, which already worked: the rewrite must not have changed it.
	`SELECT b, (SELECT c FROM (SELECT t2.b AS c FROM t1 LIMIT 1)) FROM t2 ORDER BY b`,
}

// derivedReachRejected: the SAME reference written at the derived table's own
// level names a sibling, which C SQLite rejects. Reaching outward must not
// invent a resolution SQLite itself refuses.
var derivedReachRejected = []string{
	`SELECT c FROM (SELECT t2.b AS c FROM t1), t2`,
	`SELECT max(b), (SELECT c FROM (SELECT zzz.b AS c FROM t1)) FROM t2`,
}

func TestDerivedTableEnclosingScopeReach(t *testing.T) {
	path := buildDerivedReachDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range derivedReachServed {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE declined where C SQLite answered %d row(s): %v", sqlText, len(cRows), vErr)
			continue
		}
		if len(vVals) != len(cRows) {
			t.Errorf("[%s] QueryVDBE returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(vVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				sqlText, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Errorf("[%s] p.Query declined where C SQLite answered: %v", sqlText, eErr)
			continue
		}
		if len(eVals) != len(cRows) {
			t.Errorf("[%s] p.Query returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(eVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				sqlText, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}

	for _, sqlText := range derivedReachRejected {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Errorf("[%s] C SQLite ACCEPTED this -- the rejection premise no longer holds", sqlText)
			continue
		}
		if _, rows, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] QueryVDBE answered %d row(s) where C SQLite rejects the statement", sqlText, len(rows))
		}
		if _, rows, err := p.Query(sqlText); err == nil {
			t.Errorf("[%s] p.Query answered %d row(s) where C SQLite rejects the statement", sqlText, len(rows))
		}
	}
}
