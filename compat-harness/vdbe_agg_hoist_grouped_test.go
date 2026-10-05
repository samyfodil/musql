// Hoisted aggregates in GROUP BY contexts.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildAggGroupedHoistDB. t34 is subquery.test's own 3.4.1 fixture, shaped so
// the two groups have DIFFERENT averages (4.5 and 4.0) and so the correct
// answer (1 row) and the anchor-row answer (2 rows) can never be confused. t4
// is in.test's 6.7 fixture verbatim, whose collation-sensitive join makes the
// hoisted sum() a different value per group. g1/g2 give the plain shapes a
// fixture with an empty group and a NULL.
func buildAggGroupedHoistDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agghoistgrouped.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t34(x,y)`,
		`INSERT INTO t34 VALUES(106,4),(107,3),(106,5),(107,5)`,
		`CREATE TABLE t4(a TEXT, b INT)`,
		`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2)`,
		`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
		`CREATE TABLE g1(k INTEGER, v INTEGER)`,
		`INSERT INTO g1 VALUES(1,10),(1,20),(2,30),(2,NULL),(3,40)`,
		`CREATE TABLE g2(w INTEGER)`,
		`INSERT INTO g2 VALUES(1),(2)`,
		`CREATE TABLE e1(q INTEGER)`,
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

// aggGroupedHoistServed: shapes whose inner aggregate belongs to the enclosing
// AGGREGATE query, and which this engine must now answer byte-for-byte.
// wantRows is the oracle's own count, asserted against the oracle first so a
// fixture drift can never silently weaken a case into a tautology.
var aggGroupedHoistServed = []struct {
	sql      string
	wantRows int
}{
	// Was on the decline list, on the premise that a hoisted min() would move
	// the honored anchor row. It does not have to: this shape reads NO bare
	// column of the grouped query, so there is no anchor for the hoist to move
	// -- the only reader is the subquery itself, and the min() it hoists is the
	// value it wants. Its max()/bare-v siblings are still declined (they do read
	// one), and this now matches 3.53.3 row for row: (1,10),(2,30),(3,40).
	{`SELECT k, (SELECT min(g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	// subquery.test 3.4.1, verbatim. The anchor-row reading answers 2 rows.
	{`SELECT a.x, avg(a.y)
              FROM t34 AS a
             GROUP BY a.x
             HAVING NOT EXISTS( SELECT b.x, avg(b.y)
                                  FROM t34 AS b
                                 GROUP BY b.x
                                 HAVING avg(a.y) > avg(b.y))`, 1},
	// subquery.test 3.4.2, verbatim: TWO bodies in the SELECT LIST, each with
	// its own copy of the same hoisted call, and the enclosing query keeps all
	// of its groups.
	{`SELECT
               a.x,
               avg(a.y),
               NOT EXISTS ( SELECT b.x, avg(b.y)
                              FROM t34 AS b
                              GROUP BY b.x
                             HAVING avg(a.y) > avg(b.y)),
               EXISTS ( SELECT c.x, avg(c.y)
                          FROM t34 AS c
                          GROUP BY c.x
                         HAVING avg(a.y) > avg(c.y))
              FROM t34 AS a
             GROUP BY a.x
             ORDER BY a.x`, 2},
	// subquery.test 3.4.1 again, but with the outer aggregate spelled as a
	// select-list ALIAS ("avg1"/"avg2") rather than written out longhand in
	// the inner HAVING -- C SQLite's resolveAlias (resolve.c:69-101)
	// splices a COPY of the outer's already-resolved "avg(a.y)" in for the
	// bare "avg1" reference, then the ordinary outward-association walk
	// re-attributes that spliced copy exactly as it would the longhand call.
	// Anchor-row reading (pre-fix) answered BOTH groups, same as 3.4.1's own.
	{`SELECT a.x, avg(a.y) AS avg1
              FROM t34 AS a
             GROUP BY a.x
             HAVING NOT EXISTS( SELECT b.x, avg(b.y) AS avg2
                                  FROM t34 AS b
                                 GROUP BY b.x
                                 HAVING avg1 > avg2)`, 1},
	// THREE levels deep, with the bare alias reference in the INNERMOST
	// level's own WHERE and the matching alias defined at the MIDDLE level
	// (not the top) -- the exact shape an earlier, REJECTED attempt at this
	// mechanism got wrong. That attempt kept a single outerCols field fixed
	// at the TOP query and never updated it as the walk descended, so a bare
	// name written two levels down skipped straight past the middle level's
	// own "avg1" to the top's -- answering 1 row, (107,4), where C SQLite
	// answers 0. See outerScopeLevel's doc comment (engine/sql_group.go) for
	// the fix: a proper nearest-first CHAIN of enclosing levels, threaded
	// through the recursion, so an intermediate level's own alias shadows one
	// further out exactly as the top-level case above already does.
	{`SELECT a.x, avg(a.y) AS avg1 FROM t34 AS a GROUP BY a.x
             HAVING NOT EXISTS(
               SELECT b.x, avg(b.y) AS avg1 FROM t34 AS b GROUP BY b.x
               HAVING EXISTS(SELECT 1 WHERE avg1 > 4.2))`, 0},
	// FOUR levels deep: the alias reference sits in the INNERMOST level's own
	// WHERE, the matching "avg1" is defined TWO levels out (the SECOND level
	// from the top, i.e. the second-from-innermost's own enclosing level),
	// and the level directly between them defines no alias at all -- so the
	// chain must walk straight past that alias-less intermediate level (it
	// neither shadows nor matches) to find the correct one further out,
	// rather than either stopping short or skipping too far to the top.
	{`SELECT a.x, avg(a.y) AS avg1 FROM t34 AS a GROUP BY a.x
             HAVING NOT EXISTS(
               SELECT b.x, avg(b.y) AS avg1 FROM t34 AS b GROUP BY b.x
               HAVING EXISTS(
                 SELECT 1 WHERE EXISTS(
                   SELECT 1 WHERE avg1 > 4.2)))`, 0},
	// The SAME four-level nesting with NO alias defined anywhere except the
	// TOP -- confirms the chain correctly threads all the way out through
	// two alias-less intermediate levels when nothing shadows along the way,
	// rather than over-correcting into a decline or a false shadow.
	{`SELECT a.x, avg(a.y) AS avg1 FROM t34 AS a GROUP BY a.x
             HAVING NOT EXISTS(
               SELECT b.x, avg(b.y) FROM t34 AS b GROUP BY b.x
               HAVING EXISTS(
                 SELECT 1 WHERE EXISTS(
                   SELECT 1 WHERE avg1 > 4.2)))`, 1},
	// in.test 6.7 and 6.8: the same rule with the hoisted sum() in a FROM-less
	// HAVING subquery, over t4 -- INDEXED (t4x, an expression index on
	// (a, +a COLLATE NOCASE)), which used to make wherePlanMultiTableOrder
	// refuse a 2-table FROM carrying one (wherePlanIndexList was called with
	// stmt=nil for every multi-table caller). Fixed: a pure-expression index
	// (no partial WHERE) needs no cross-source implication proof, so it is now
	// represented for stmt != nil regardless of table count -- verified
	// byte-for-byte against the oracle including group_concat's order (the
	// anchor-sensitive part), not just the row count.
	{`SELECT a0.a, group_concat(a1.a) AS b
              FROM t4 AS a0 JOIN t4 AS a1
             GROUP BY a0.a
            HAVING (SELECT sum( (a1.a == +a0.a COLLATE NOCASE) IN (SELECT b FROM t4)))`, 3},
	{`SELECT a0.a, group_concat(a1.a) AS b
              FROM t4 AS a0 JOIN t4 AS a1
             GROUP BY a0.a
            HAVING (SELECT sum( (a1.a GLOB +a0.a COLLATE NOCASE) IN (SELECT b FROM t4)))`, 3},

	// --- the same rule in its plainest forms, so the boundary is pinned by
	// more than the four corpus statements above. Every count below is the
	// oracle's own. g1(k,v) = (1,10),(1,20),(2,30),(2,NULL),(3,40), so the
	// three group sums are 30, 30 and 40 -- deliberately NOT all distinct, so a
	// per-group value that leaked from the wrong group is still visible.
	// -----------------------------------------------------------------
	//
	// A scalar subquery in the SELECT LIST of a GROUP BY query: sum(g1.v) is the
	// enclosing GROUP's sum, evaluated once per group, and the body still scans
	// its own FROM. cgo: (1,30),(2,30),(3,40).
	{`SELECT k, (SELECT sum(g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	// count() counts the group's NON-NULL values: (1,2),(2,1),(3,1), where the
	// anchor-row reading would answer 1 everywhere.
	{`SELECT k, (SELECT count(g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	// ...beside a column of the body's OWN first row: 30+1, 30+1, 40+1.
	{`SELECT k, (SELECT sum(g1.v)+w FROM g2) FROM g1 GROUP BY k`, 3},
	// ...with the enclosing query's own aggregate beside it, so the two must
	// agree group for group.
	{`SELECT k, sum(v), (SELECT sum(g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	{`SELECT k, count(*), (SELECT count(g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	// The body's own FROM is EMPTY: the hoisted aggregate still has a value, but
	// the scalar subquery is NULL. cgo: (1,NULL),(2,NULL),(3,NULL).
	{`SELECT k, (SELECT sum(g1.v) FROM e1) FROM g1 GROUP BY k`, 3},
	// A FROM-less body -- SQLite's -1 case for the body itself, so the outward
	// walk leaves the enclosing query as the owner.
	{`SELECT k, (SELECT sum(g1.v)) FROM g1 GROUP BY k`, 3},
	{`SELECT k, (SELECT sum(g1.v)*2) FROM g1 GROUP BY k`, 3},
	// A CORRELATED body: g1.k outside the aggregate is still the ordinary
	// anchor-row reference (which for a GROUP BY key is the group's own value),
	// while g1.v inside it is the group's sum. cgo: (1,30),(2,30),(3,NULL).
	{`SELECT k, (SELECT sum(g1.v) FROM g2 WHERE w=g1.k) FROM g1 GROUP BY k`, 3},
	// group_concat's per-group text: '10,20', '30', '40'.
	{`SELECT k, (SELECT group_concat(g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	// In HAVING rather than the select list -- and with a threshold that
	// DISCRIMINATES between the groups, so a leaked value changes the row count.
	{`SELECT k FROM g1 GROUP BY k HAVING (SELECT sum(g1.v) FROM g2) > 25`, 3},
	{`SELECT k FROM g1 GROUP BY k HAVING (SELECT sum(g1.v) FROM g2) > 35`, 1},
	{`SELECT k FROM g1 GROUP BY k HAVING (SELECT sum(g1.v)) > 35`, 1},
	// Two bodies in one item, one hoisted and one not.
	{`SELECT k, (SELECT count(g1.v) FROM g2), (SELECT count(*) FROM g2) FROM g1 GROUP BY k`, 3},
	// The enclosing query is a WHOLE-TABLE aggregate (no GROUP BY) that already
	// has an aggregate of its own, so no outward hoist is involved: the call
	// simply joins the item's accumulator list.
	{`SELECT sum(v), (SELECT sum(g1.v) FROM g2) FROM g1`, 1},
	// GROUP BY with an ORDER BY over the hoisted value.
	{`SELECT k, (SELECT sum(g1.v) FROM g2) AS s FROM g1 GROUP BY k ORDER BY s`, 3},
	// The body's own HAVING -- subquery.test 3.4.1's clause, in its plainest
	// form. cgo: (1,1),(2,1),(3,1).
	{`SELECT k, (SELECT w FROM g2 GROUP BY w HAVING sum(g1.v) > w) FROM g1 GROUP BY k`, 3},
	// A COMPOUND arm's select list: the arm is an independent SELECT, and the
	// scalar subquery still takes the FIRST arm's row. cgo: (1,0),(2,0),(3,0).
	{`SELECT k, (SELECT 0 FROM g2 UNION ALL SELECT sum(g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	// Written one body DEEPER than the one the item names: the inner body is
	// over the empty e1, so the whole thing is NULL while the accumulator still
	// has to be the enclosing group's.
	{`SELECT k, (SELECT (SELECT sum(g1.v) FROM e1) FROM g2) FROM g1 GROUP BY k`, 3},
	// DISTINCT and FILTER ride on the hoisted call, and sameAggCall keeps them
	// apart from the plain form. cgo for the FILTER: (1,20),(2,30),(3,40).
	{`SELECT k, (SELECT sum(DISTINCT g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	{`SELECT k, (SELECT sum(g1.v) FILTER (WHERE g1.v>15) FROM g2) FROM g1 GROUP BY k`, 3},
	// A WINDOW call is NOT an aggregate and is NEVER re-associated: "sum(g1.v)
	// OVER ()" keeps the ordinary anchor-row reading, and cgo answers
	// (1,20),(2,60),(3,80) -- twice the group's FIRST v, summed over g2's two
	// rows -- not twice the group's sum.
	{`SELECT k, (SELECT sum(g1.v) OVER () FROM g2) FROM g1 GROUP BY k`, 3},
}

// aggGroupedHoistStaysLocal: the mirror direction. The inner aggregate's
// argument DOES name a column the body's own FROM supplies, so SQLite leaves it
// where it is written and this engine must NOT hoist it. Getting this wrong
// swaps one group's value for another's, silently.
var aggGroupedHoistStaysLocal = []struct {
	sql      string
	wantRows int
}{
	{`SELECT k, (SELECT sum(w) FROM g2) FROM g1 GROUP BY k`, 3},
	{`SELECT k, (SELECT count(*) FROM g2) FROM g1 GROUP BY k`, 3},
	// Mixed: the argument names BOTH a local column and an enclosing one, which
	// is sqlite3ReferencesSrcList's 1 -- it stays.
	{`SELECT k, (SELECT sum(w+g1.v) FROM g2) FROM g1 GROUP BY k`, 3},
	{`SELECT x, avg(y) FROM t34 GROUP BY x HAVING (SELECT count(*) FROM t34 AS b) > 3`, 2},
	// The inner subquery defines its OWN "avg1" alias, which must win over the
	// outer's same-named one (resolve.c's resolveAlias is tried at the
	// CLOSEST NameContext level first, resolve.c:648-692) -- so "avg1 > 4.2"
	// reads the inner b-group's own average, not the outer a-group's. Every
	// b-group's own average (4.5 for x=106) is checked regardless of which a-
	// group is current, so NOT EXISTS is false for every a-group: 0 rows. A
	// wrong hoist from the outer alias instead would answer 1 row (only
	// x=107's outer average, 4.0, fails ">4.2", so only that group's EXISTS
	// is false) -- discriminating, not a coincidental match.
	{`SELECT a.x, avg(a.y) AS avg1
              FROM t34 AS a
             GROUP BY a.x
             HAVING NOT EXISTS(SELECT b.x, avg(b.y) AS avg1
                                 FROM t34 AS b
                                GROUP BY b.x
                                HAVING avg1 > 4.2)`, 0},
}

// aggGroupedHoistDeclines: shapes the engine must keep declining, each with the
// oracle's own row count so the decline is provably covering a real answer
// rather than a statement C SQLite also rejects.
var aggGroupedHoistDeclines = []struct {
	sql      string
	wantRows int
	why      string
}{
	// A hoisted min()/max() is one of the enclosing query's aggregates, so it
	// is a candidate for the HONORED row every bare column and every
	// correlated subquery in the same item reads from -- and findHonoredMinMax
	// cannot see it, because it walks the select list without descending into
	// subqueries. cgo answers (1,20,20),(2,30,30),(3,40,40) for the first of
	// these: the bare v follows max's WINNING row, not the group's first. This
	// collector sees one item at a time and has no statement to reconcile
	// against (honoredWithHoisted's job for the outward hoist), so it declines.
	{`SELECT k, v, (SELECT max(g1.v) FROM g2) FROM g1 GROUP BY k`, 3,
		"hoisted max() would move the honored anchor row"},
	{`SELECT count(*), (SELECT max(g1.v) FROM g2) FROM g1`, 1,
		"hoisted max() would move the honored anchor row"},
	// GROUP BY and ORDER BY: C SQLite ACCEPTS an aggregate there (3 rows of
	// (k,1) for the first), but the substitution delivers the value as an
	// integer LITERAL, which either clause reads as an ORDINAL reference to a
	// result column. Declined rather than answered -- see the hoistable rule in
	// materializedOuterRefInAggArg.
	{`SELECT k, (SELECT w FROM g2 GROUP BY sum(g1.v)) FROM g1 GROUP BY k`, 3,
		"a hoisted value in GROUP BY would be read as a column ordinal"},
	{`SELECT k, (SELECT w FROM g2 ORDER BY sum(g1.v)) FROM g1 GROUP BY k`, 3,
		"a hoisted value in ORDER BY would be read as a column ordinal"},
	// The hoisted call would land in a body a WITH-clause CTE owns, which the
	// runtime substitution deliberately does not descend into.
	{`SELECT k, (SELECT * FROM (WITH c(n) AS (SELECT sum(g1.v)) SELECT n FROM c)) FROM g1 GROUP BY k`, 3,
		"hoist target inside a WITH-clause CTE body"},
	// aggnested-4.1, verbatim. Its two sum(out.i) calls are written inside a
	// FROM-item DERIVED TABLE, which neither this collector nor
	// rewriteSelectOuterRefs descends into -- so the call is reported at the
	// derived table's OWN compile, where the enclosing query is
	// "... FROM (SELECT out.j)" and does not supply out.i either. Placing it
	// needs the enclosing COMPILER chain (hoistOwnerFor), which planGroupItem
	// does not hold.
	{`WITH out(i, j, k) AS (
              VALUES(1234, 5678, 9012)
          )
          SELECT (
            SELECT (
              SELECT min(abc) = ( SELECT ( SELECT 1234 fROM (SELECT abc) ) )
              FROM (
                SELECT sum( out.i ) + ( SELECT sum( out.i ) ) AS abc FROM (SELECT out.j)
              )
            )
          ) FROM out`, 1,
		"hoist owner is only reachable through the enclosing compiler chain"},
}

// aggGroupedHoistMutualReject: C SQLite rejects these outright, so this
// engine must too -- never answer. The first two are the reason the collector
// refuses a call written in a WHERE or a JOIN ON: SQLite clears NC_AllowAgg
// there, so re-associating the call would answer a statement the oracle throws
// out. They were both live wrong answers in the first cut of this rule, found
// by TestAggGroupedHoistFuzz rather than by any hand-written case.
var aggGroupedHoistMutualReject = []string{
	`SELECT k, (SELECT w FROM g2 WHERE sum(g1.v) > w) FROM g1 GROUP BY k`,
	`SELECT k FROM g1 GROUP BY k HAVING NOT EXISTS(SELECT 1 FROM g2 WHERE sum(g1.v) > w)`,
	`SELECT k, (SELECT g2.w FROM g2 JOIN e1 ON sum(g1.v)=g2.w) FROM g1 GROUP BY k`,
	// An aggregate inside an aggregate's argument is a hard error at every
	// level, hoist or no hoist.
	`SELECT k, (SELECT sum(sum(g1.v)) FROM g2) FROM g1 GROUP BY k`,
	// outerAliasAggCall's chain walk (sql_group.go) must STOP at a level that
	// defines a same-named alias AT ALL, even when that alias is disqualified
	// (a window function) -- never fall through past it to a farther level's
	// own same-named alias, matching resolveAlias's unconditional splice
	// (resolve.c:69-101, cnt=1; goto lookupname_end regardless of what the
	// alias turns out to be). C SQLite errors "misuse of aliased window
	// function avg1" here: the middle level's own "avg1" (row_number() OVER
	// (...), a window function) shadows the outer aggregate's own same-named
	// "avg1" for the innermost level's bare reference. A version of
	// outerAliasAggCall that conflated "no alias here" with "alias here but
	// disqualified" (both then just resultAliasExpr returning (nil, false))
	// skipped straight past the middle level to the outer aggregate instead,
	// silently answering 0 rows rather than declining -- caught live against
	// the oracle, verified by mutation-testing the fix back out.
	`SELECT a.x, avg(a.y) AS avg1 FROM t34 AS a GROUP BY a.x
	 HAVING NOT EXISTS(SELECT x, row_number() OVER (ORDER BY x) AS avg1
	                     FROM t34 WHERE EXISTS(SELECT 1 WHERE avg1 > 1))`,
}

func TestAggGroupedHoist(t *testing.T) {
	path := buildAggGroupedHoistDB(t)

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
		}
	}

	for _, tc := range aggGroupedHoistServed {
		answer(tc.sql, tc.wantRows)
	}

	// The plan-order half of the anchor rule (anchorPlanOrderProvable,
	// engine/vdbe_agg_codegen.go): a correlated subquery in HAVING reads the
	// group's ANCHOR ROW exactly as a bare column does, and over an INDEXED
	// table C SQLite may take that row from an index scan where this engine
	// always scans in rowid order. Measured at 11/70 wrong for this shape
	// (an indexed table, a correlated HAVING subquery) before the decline, and
	// 0/78 with no index -- so these two agreed only because their own fixture
	// happened to land on the same row.
	//
	// Self-verifying, so lifting the decline cannot pass unnoticed: the oracle
	// must still answer, and the recorded row count must still hold.
	for _, tc := range aggGroupedHoistPlanOrderDeclined {
		cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", tc.sql, cErr)
			continue
		}
		_ = cCols
		if len(cRows) != tc.wantRows {
			t.Errorf("[%s] C SQLite returned %d row(s), this gate's recorded count is %d",
				tc.sql, len(cRows), tc.wantRows)
		}
		if _, _, vErr := p.QueryArgs(tc.sql, nil); vErr == nil {
			t.Errorf("[%s] declined on purpose (plan-order anchor over an indexed table); if it now compiles, verify it against the oracle and move it back to aggGroupedHoistServed", tc.sql)
		}
	}
	for _, tc := range aggGroupedHoistStaysLocal {
		answer(tc.sql, tc.wantRows)
	}

	for _, tc := range aggGroupedHoistDeclines {
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
	}

	for _, sqlText := range aggGroupedHoistMutualReject {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Errorf("[%s] C SQLite ACCEPTED this -- the rejection premise no longer holds", sqlText)
			continue
		}
		if _, rows, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] QueryVDBE answered %d row(s) where C SQLite rejects the statement", sqlText, len(rows))
		}
	}
}

// TestAggGroupedHoistFuzz is the randomized half of the gate above. The list
// there is deterministic, and a deterministic probe can only pin the shapes
// someone thought of: what this rule actually decides is WHICH GROUP's value a
// subquery body sees, which is a function of the data, so a fixed fixture can
// agree by coincidence (two groups with the same sum agree under any
// attribution). This builds a fresh random two-column table per iteration --
// random group count, random group sizes, random values including NULLs and
// negatives -- and compares every cell against the oracle for a matrix of
// hoisted-aggregate shapes.
func TestAggGroupedHoistFuzz(t *testing.T) {
	// Shapes, all of which put the hoisted call in a position whose value
	// depends on the group it is attributed to.
	shapes := []string{
		`SELECT k, (SELECT %s(f.v) FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, count(*), (SELECT %s(f.v) FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT %s(f.v)) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT %s(f.v) FROM w2 WHERE w=1) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT %s(f.v)+w FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT %s(f.v) FROM w2 WHERE w=f.k) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT (SELECT %s(f.v) FROM w2) FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT %s(DISTINCT f.v) FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT %s(f.v) FILTER (WHERE f.v>0) FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT %s(f.v) OVER () FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT 0 FROM w2 UNION ALL SELECT %s(f.v) FROM w2) FROM f GROUP BY k ORDER BY k`,
		`SELECT k FROM f GROUP BY k HAVING (SELECT %s(f.v) FROM w2) > 0 ORDER BY k`,
		`SELECT k FROM f GROUP BY k HAVING (SELECT %s(f.v)) > 0 ORDER BY k`,
		`SELECT k, (SELECT w FROM w2 GROUP BY w HAVING %s(f.v) > w) FROM f GROUP BY k ORDER BY k`,
		// Positions C SQLite rejects or reinterprets -- the engine must
		// decline or reject in lockstep, never answer.
		`SELECT k FROM f GROUP BY k HAVING NOT EXISTS(SELECT 1 FROM w2 WHERE %s(f.v) > w) ORDER BY k`,
		`SELECT k, (SELECT w FROM w2 WHERE %s(f.v) > w) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT w2.w FROM w2 JOIN w3 ON %s(f.v)=w2.w) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT w FROM w2 GROUP BY %s(f.v)) FROM f GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT w FROM w2 ORDER BY %s(f.v)) FROM f GROUP BY k ORDER BY k`,
		// The enclosing query is a whole-table aggregate rather than a GROUP BY.
		`SELECT %s(f.v), (SELECT %s(f.v) FROM w2) FROM f`,
		`SELECT count(*), (SELECT %s(f.v) FROM w2) FROM f`,
	}
	aggs := []string{"sum", "count", "avg", "total", "group_concat", "min", "max"}

	// compared counts the (statement, fixture) pairs where BOTH engines answered
	// and every cell was checked. Without a floor this test can go vacuous the
	// moment a decline widens -- every case would "pass" by being skipped -- so
	// the floor is asserted at the end. It is the measured count rounded well
	// down, not a target.
	compared := 0

	rnd := rand.New(rand.NewSource(20260802))
	for iter := 0; iter < 60; iter++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "fuzz.sqlite")
		db, err := engine.Create(path)
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		var stmts []string
		stmts = append(stmts,
			`CREATE TABLE f(k INTEGER, v INTEGER)`,
			`CREATE TABLE w2(w INTEGER)`,
			`CREATE TABLE w3(z INTEGER)`,
			`INSERT INTO w2 VALUES(1),(2)`,
			`INSERT INTO w3 VALUES(1)`)
		nGroups := 1 + rnd.Intn(4)
		for g := 1; g <= nGroups; g++ {
			for n := rnd.Intn(4); n >= 0; n-- {
				switch rnd.Intn(6) {
				case 0:
					stmts = append(stmts, fmt.Sprintf(`INSERT INTO f VALUES(%d,NULL)`, g))
				default:
					stmts = append(stmts, fmt.Sprintf(`INSERT INTO f VALUES(%d,%d)`, g, rnd.Intn(41)-20))
				}
			}
		}
		for _, s := range stmts {
			if err := db.Exec(s); err != nil {
				t.Fatalf("Exec(%s): %v", s, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatal(err)
		}
		p, err := engine.Open(path)
		if err != nil {
			cdb.Close()
			t.Fatal(err)
		}
		for _, shape := range shapes {
			for _, a := range aggs {
				q := shape
				for strings.Contains(q, "%s") {
					q = strings.Replace(q, "%s", a, 1)
				}
				cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)
				vCols, vVals, vErr := p.QueryArgs(q, nil)
				if cErr != nil {
					if vErr == nil {
						t.Errorf("iter %d [%s] this engine answered %d row(s) where C SQLite rejects: %v\n  fixture: %v",
							iter, q, len(vVals), cErr, stmts)
					}
					continue
				}
				if vErr != nil {
					continue // a clean decline is never wrong
				}
				if len(vVals) != len(cRows) {
					t.Errorf("iter %d [%s] wrong ROW COUNT: this engine %d, C SQLite %d\n  fixture: %v",
						iter, q, len(vVals), len(cRows), stmts)
					continue
				}
				if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
					t.Errorf("iter %d [%s] DIVERGES: %s\n  vdbe: %v\n  cgo:  %v\n  fixture: %v",
						iter, q, reason, engineRowsToStrings(vVals), cRows, stmts)
					continue
				}
				compared++
			}
		}
		p.Close()
		cdb.Close()
	}
	if compared < 3000 {
		t.Errorf("only %d statement/fixture pairs were actually COMPARED against the oracle -- this fuzz has gone (nearly) vacuous; a decline must have widened", compared)
	}
	t.Logf("compared %d statement/fixture pairs against the oracle", compared)
}

// aggGroupedHoistPlanOrderDeclined are hoist shapes C SQLite ANSWERS and this
// engine declines because their ANCHOR ROW is plan-dependent -- see the loop in
// TestAggGroupedHoist that consumes them. They belong to this file rather than
// to the anchor gate because the hoist is what puts the correlated aggregate in
// HAVING in the first place; drop the index on t4 and both are served again.
var aggGroupedHoistPlanOrderDeclined = []struct {
	sql      string
	wantRows int
}{}
