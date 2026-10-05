// Differential tests of C SQLite's outward aggregate-hoisting rule:
// an aggregate with only outer-query references moves to the outer query.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildAggOutwardHoistDB builds test tables with different row counts and schemas
// to distinguish hoisted vs. un-hoisted aggregate results.
func buildAggOutwardHoistDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agghoistout.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a1 INTEGER, b INTEGER)`,
		`INSERT INTO t1 VALUES (1,10),(2,20),(3,30)`,
		`CREATE TABLE t2(b1 INTEGER, x INTEGER)`,
		`INSERT INTO t2 VALUES (4,1),(5,0)`,
		`CREATE TABLE t3(a1 INTEGER, z INTEGER)`,
		`INSERT INTO t3 VALUES (100,7),(200,8)`,
		`CREATE TABLE e1(q INTEGER)`,
		`CREATE TABLE t7(c7)`,
		`INSERT INTO t7 VALUES(1),(2)`,
		`CREATE TABLE t8(c8)`,
		`INSERT INTO t8 VALUES(10),(20),(30)`,
		`CREATE TABLE t9(c9)`,
		`INSERT INTO t9 VALUES(100),(200),(300),(400)`,
		`CREATE TABLE aa(x INT)`,
		`INSERT INTO aa(x) VALUES(123)`,
		`CREATE TABLE bb(y INT)`,
		`INSERT INTO bb(y) VALUES(456)`,
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

// aggHoistServed: the aggregate MOVES OUT, so the enclosing query becomes an
// aggregate query and answers ONE row (or one row per GROUP, where it has
// groups). wantRows is the oracle's own count, asserted against the oracle first
// so a fixture drift can never silently weaken this list into a tautology.
var aggHoistServed = []struct {
	sql      string
	wantRows int
}{
	// The shape the rule is named for -- three t1 rows collapse to one.
	{`SELECT (SELECT count(a1) FROM t2) FROM t1`, 1},
	{`SELECT (SELECT sum(a1) FROM t2) FROM t1`, 1},
	{`SELECT (SELECT string_agg(a1,'x') FROM t2) FROM t1`, 1},
	// The body still scans its OWN FROM: no rows there makes the whole scalar
	// subquery NULL even though the hoisted aggregate has a value.
	{`SELECT (SELECT count(a1) FROM e1) FROM t1`, 1},
	{`SELECT (SELECT count(a1) FROM t2 WHERE b1=99) FROM t1`, 1},
	{`SELECT (SELECT count(a1) FROM t2 LIMIT 0) FROM t1`, 1},
	// ...and reads its own first row alongside the hoisted value.
	{`SELECT (SELECT count(a1)+b1 FROM t2) FROM t1`, 1},
	// A bare column of the now-aggregate enclosing query.
	{`SELECT a1, (SELECT count(a1) FROM t2) FROM t1`, 1},
	{`SELECT b, (SELECT count(a1) FROM t2) FROM t1`, 1},
	// The hoisted accumulator sees the enclosing query's POST-WHERE rows.
	{`SELECT (SELECT count(a1) FROM t2) FROM t1 WHERE a1>1`, 1},
	{`SELECT (SELECT sum(a1) FROM t2) FROM t1 WHERE a1<>2`, 1},
	// One output row cannot be reordered, deduped, or sliced past.
	{`SELECT DISTINCT (SELECT count(a1) FROM t2) FROM t1`, 1},
	{`SELECT (SELECT count(a1) FROM t2) FROM t1 ORDER BY a1`, 1},
	{`SELECT (SELECT count(a1) FROM t2) FROM t1 ORDER BY 1`, 1},
	{`SELECT (SELECT count(a1) FROM t2) FROM t1 LIMIT 2`, 1},
	{`SELECT (SELECT count(a1) FROM t2) FROM t1 LIMIT 0`, 0},
	// Two aggregates in ONE body, only one of which moves (the other is t8's).
	{`SELECT (SELECT max(c7)+max(c8) FROM t8) FROM t7`, 1},
	// The QUALIFIED spelling, over a t3 that HAS its own a1: "t1.a1" names
	// t1's, so sqlite3ReferencesSrcList answers 0 for t3 and the aggregate
	// moves out. cgo: the single row 6. This was a decline until the recording
	// half of the enclosing-aggregate hoist landed (engine/sql_group.go's
	// itemAggHoists) -- the hoist itself always happened, and checkItemSubqueries
	// then threw the item away.
	{`SELECT (SELECT sum(t1.a1) FROM t3) FROM t1`, 1},
	{`SELECT (SELECT string_agg(a1,'x') || '-' || string_agg(b1,'y') FROM t2) FROM t1`, 1},
	// Three levels, one aggregate owned by each: max(c7)->t7, max(c8)->t8,
	// max(c9) stays. 2+30+400 = 432, in ONE row.
	{`SELECT (SELECT (SELECT max(c7)+max(c8)+max(c9) FROM t9) FROM t8) FROM t7`, 1},
	// Hoisting TWO levels out, past a query that supplies neither reference.
	{`SELECT (SELECT (SELECT max(c7)+c8+c9 FROM t9) FROM t8) FROM t7`, 1},
	// The hoisted call reached through a nested FROM-less body...
	{`SELECT (SELECT (SELECT count(a1)) FROM t2) FROM t1`, 1},
	// ...and through a DERIVED TABLE in the body's own FROM (aggnested-5.x).
	{`SELECT (SELECT y FROM (SELECT sum(a1) AS y) AS d) FROM t1`, 1},
	{`SELECT (SELECT y FROM (SELECT z AS y FROM (SELECT sum(a1) AS z) AS d2) AS d) FROM t1`, 1},
	// Beside a subquery of its own that keeps ITS aggregate local (t3 has a1).
	{`SELECT (SELECT count(a1) + (SELECT count(a1) FROM t3) FROM t2) FROM t1`, 1},
	// A count(*) whose only reference is its FILTER, naming only t1: SQLite's
	// -1 ("no table at all") becomes 0 ("only another context's tables"), so it
	// moves and the statement is ONE row.
	{`SELECT (SELECT count(*) FILTER(WHERE a1>1) FROM t2) FROM t1`, 1},
	// The owner is a LEFT JOIN, and the argument is a column of the null-extended
	// side: the accumulator steps the join's own rows, so count(z) is 0.
	{`SELECT (SELECT count(z) FROM t2) FROM t1 LEFT JOIN t3 ON t1.a1=t3.a1`, 1},
	// aggnested-4.2.
	{`SELECT (SELECT sum(x+y) FROM bb) FROM aa`, 1},
	// aggnested-4.1: the argument's own FROM-LESS subquery reads the INNER
	// query's table, which is sqlite3ReferencesSrcList's bit 1 (expr.c:7200 --
	// selectRefEnter pushes nothing for a FROM-less body), so sum() STAYS with
	// bb and the answer is the single row 579. Moved out of aggHoistDeclines by
	// the port of that descent (aggArgHasLocalColumnRef, sql_agg.go).
	{`SELECT (SELECT sum(x+(SELECT y)) FROM bb) FROM aa`, 1},
	// An UNQUALIFIED correlated reference beside the hoisted value. The body is
	// materialized by a rewrite that substitutes QUALIFIED references only, so
	// this needed the sub-Program to RE-RUN per enclosing row instead of being
	// cached -- which is what marking a subquery-bearing aggregate item
	// correlated now does (markSubqueryCorrelated, vdbe_agg_codegen.go). Two
	// rows, matching the oracle. Moved out of aggHoistDeclines.
	{`SELECT (SELECT (SELECT c7+max(c8)+c9 FROM t9) FROM t8) FROM t7`, 2},
}

// aggHoistAnchor: a hoisted min()/max() is one of the ENCLOSING query's
// aggregates, so its winning row anchors that query's bare columns. Kept apart
// from aggHoistServed because it is the case a first implementation got WRONG
// (b answered 10, C SQLite 30) rather than merely declined.
var aggHoistAnchor = []struct {
	sql      string
	wantRows int
}{
	{`SELECT b, (SELECT max(a1) FROM t2) FROM t1`, 1},
	{`SELECT b, (SELECT min(a1) FROM t2) FROM t1`, 1},
	{`SELECT a1, (SELECT max(a1) FROM t2) FROM t1`, 1},
	// No min/max anywhere: the anchor is the FIRST row, not the last.
	{`SELECT b, (SELECT count(a1) FROM t2) FROM t1`, 1},
}

// aggHoistStaysLocal: the OTHER side of the same boundary -- SQLite's 1 and -1,
// where the aggregate does NOT move and the enclosing query keeps every row.
// A hoist here would be a silent wrong row count, so these are as load-bearing
// as the served list.
var aggHoistStaysLocal = []struct {
	sql      string
	wantRows int
}{
	// The inner FROM SUPPLIES the column, so it is the inner query's aggregate.
	{`SELECT (SELECT count(a1) FROM t3) FROM t1`, 3},
	{`SELECT (SELECT count(a1) FROM (SELECT a1 FROM t3)) FROM t1`, 3},
	{`SELECT (SELECT count(a1) FROM t3 AS t1) FROM t1`, 3},
	// One local reference anywhere in the argument list is enough.
	{`SELECT (SELECT count(a1+b1) FROM t2) FROM t1`, 3},
	{`SELECT (SELECT group_concat(b1,a1) FROM t2) FROM t1`, 3},
	{`SELECT (SELECT group_concat(a1,b1) FROM t2) FROM t1`, 3},
	// ...including a reference that appears only in the FILTER. filter1-6.1.
	{`SELECT (SELECT count(a1) FILTER(WHERE x) FROM t2) FROM t1`, 3},
	// SQLite's -1: no table named at all, or only the argument's own subquery's.
	{`SELECT (SELECT count(*) FROM t2) FROM t1`, 3},
	{`SELECT (SELECT total((SELECT b1 FROM t2)) FROM t2) FROM t1`, 3},
	// Only the INNERMOST aggregate moves (to t8), so t7 keeps its two rows.
	{`SELECT c7, (SELECT max(c8) FROM t8) FROM t7`, 2},
}

// aggHoistDeclines: shapes this engine still refuses. Each records the oracle's
// own row count, so the list doubles as the evidence that the decline is
// covering a genuine gap rather than a shape nobody checked -- and as the alarm
// that fires if one of them silently starts answering.
var aggHoistDeclines = []struct {
	sql      string
	wantRows int
	why      string
}{
	// The enclosing query is ALREADY an aggregate/GROUP BY query, so its
	// select-list subquery bodies compile as ITEM programs with no enclosing
	// *compiler and no live hoistBox for the discovery to record against
	// (hoistOwnerFor, engine/vdbe_agg_hoist.go; the reasoning is spelled out in
	// full at sql_group.go's checkItemSubqueries), and the hoist is never
	// discovered. This is the largest remaining sub-shape.
	{`SELECT max(b), (SELECT count(a1) FROM t2) FROM t1`, 1, "enclosing query is already an aggregate"},
	{`SELECT max(b), (SELECT max(a1) FROM t2) FROM t1`, 1, "enclosing query is already an aggregate"},
	{`SELECT (SELECT count(a1) FROM t2) FROM t1 GROUP BY a1`, 3, "enclosing query is a GROUP BY query"},
	// Two SEPARATE bodies in one item, both hoisting: attempt 1 fails at the
	// first, and the retry no longer compiles the second.
	{`SELECT (SELECT count(a1) FROM t2) + (SELECT count(a1) FROM t2) FROM t1`, 1, "a second subquery body hoisting from the same item"},
	// A hoisted min/max beside another min/max: which one anchors the bare
	// columns is select-list order, and a hoisted call's place in that order is
	// not known here.
	{`SELECT b, (SELECT max(a1)+min(a1) FROM t2) FROM t1`, 1, "hoisted min/max combined with another min/max"},
	// The hoisted body is NOT in the enclosing select list, so there is nowhere
	// to attach the accumulator.
	{`SELECT a1 FROM t1 WHERE EXISTS(SELECT count(a1) FROM t2)`, 3, "hoisted body outside the enclosing select list"},
}

// aggHoistMutualReject: C SQLite rejects these outright, and so must this
// engine -- the hoist must never invent a resolution SQLite itself refuses.
var aggHoistMutualReject = []string{
	// Ambiguous in the OWNER's own FROM: t1.a1 and q.a1 both match.
	`SELECT (SELECT count(a1) FROM t2) FROM t1, t3 AS q`,
	// "misuse of aggregate" -- an aggregate cannot be re-associated into a
	// WHERE clause.
	`SELECT b FROM t1 WHERE b > (SELECT count(a1) FROM t2)`,
	`SELECT a1 FROM t1 WHERE a1 IN (SELECT count(a1) FROM t2)`,
}

func TestAggOutwardHoist(t *testing.T) {
	path := buildAggOutwardHoistDB(t)

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

	answer := func(sqlText string, wantRows int) {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
			return
		}
		if len(cRows) != wantRows {
			t.Errorf("[%s] C SQLite returned %d row(s), this gate's recorded count is %d -- the association premise no longer holds",
				sqlText, len(cRows), wantRows)
			return
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE declined where C SQLite answered %d row(s): %v", sqlText, len(cRows), vErr)
			return
		}
		if len(vVals) != len(cRows) {
			t.Errorf("[%s] QueryVDBE returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(vVals), len(cRows))
			return
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				sqlText, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			return
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Errorf("[%s] p.Query declined where C SQLite answered: %v", sqlText, eErr)
			return
		}
		if len(eVals) != len(cRows) {
			t.Errorf("[%s] p.Query returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(eVals), len(cRows))
			return
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				sqlText, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}

	for _, tc := range aggHoistServed {
		answer(tc.sql, tc.wantRows)
	}
	for _, tc := range aggHoistAnchor {
		answer(tc.sql, tc.wantRows)
	}
	for _, tc := range aggHoistStaysLocal {
		answer(tc.sql, tc.wantRows)
	}

	for _, tc := range aggHoistDeclines {
		_, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", tc.sql, cErr)
			continue
		}
		if len(cRows) != tc.wantRows {
			t.Errorf("[%s] C SQLite returned %d row(s), this gate records %d -- the premise behind the decline (%s) no longer holds",
				tc.sql, len(cRows), tc.wantRows, tc.why)
			continue
		}
		if _, rows, err := p.QueryArgs(tc.sql, nil); err == nil {
			t.Errorf("[%s] QueryVDBE answered %d row(s) for a shape this engine does not implement (%s; C SQLite: %d row(s)) -- it must decline, or be verified and moved to the served list",
				tc.sql, len(rows), tc.why, len(cRows))
		}
		if _, rows, err := p.Query(tc.sql); err == nil {
			t.Errorf("[%s] p.Query answered %d row(s) for a shape this engine does not implement (%s) -- it must decline", tc.sql, len(rows), tc.why)
		}
	}

	for _, sqlText := range aggHoistMutualReject {
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
