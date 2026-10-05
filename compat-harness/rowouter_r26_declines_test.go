package compat

import (
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var declR26Schema = []string{
	"CREATE TABLE t2(a INTEGER, b INTEGER)",
	"INSERT INTO t2 VALUES(1,10),(2,20),(2,30)",
	"CREATE TABLE t0(c0 INTEGER, c1 INTEGER)",
	"INSERT INTO t0 VALUES(0,0),(1,1)",
	"CREATE TABLE one(x,y)",
	"INSERT INTO one VALUES(1,7)",
	"CREATE TABLE p1(a INTEGER,b INTEGER)",
	"INSERT INTO p1 VALUES(1,100),(2,202),(2,200)",
	"CREATE INDEX px ON p1(a,b)",
}

// declR26Cases used to hold six still-declined shapes (R1's reassociated avg()
// plus five R2 RowExpr-in-aggregate cases); all six are now served and AGREE
// with the oracle -- r35dNoFromAnywhere's per-arm compound fix (sql_agg.go)
// makes checkAggregateAssociation correctly re-associate the FROM-less
// aggregate to the enclosing GROUP BY query in every one of them, which routes
// them through the already-correct reassociation/row-value machinery instead
// of the declining one. Moved to TestR26RowOuterR2Upgraded below per this
// file's own established convention (TestR26RowOuterUpgraded, round 28).

// TestR26RowOuterUpgraded is the six R1 shapes that were entries in
// declR26Cases above until round 28 threaded the enclosing row through the three
// call sites named in this file's header. Each one is now ANSWERED, and asserted
// cell-for-cell against the oracle rather than merely observed to run -- an
// "upgrade" that answered something else would be the wrong answer this whole
// class produced twice before.
//
// The first three go through the COMPOUND route (tryVDBECompound), the last three
// through the FROM-less AGGREGATE one (tryVDBENoFromAggregate); in each the
// aggregate itself names no table, so sqlite3ReferencesSrcList answers -1 and
// SQLite's association rule keeps the call in the subquery -- it was only ever
// the bare column beside it that failed to bind. The WRITE case ends with its own
// read-back so a silently-skipped UPDATE shows up as the row it failed to change;
// it is safe through differ() because neither engine errors on any of its
// statements (see the worker's "Query, then Exec if that failed" retry).
func TestR26RowOuterUpgraded(t *testing.T) {
	for _, q := range []string{
		"SELECT a, count(*) FROM t2 GROUP BY a HAVING EXISTS (SELECT a UNION SELECT 123) ORDER BY a",
		"SELECT a, (SELECT a UNION SELECT 99 ORDER BY 1 LIMIT 1) FROM t2 GROUP BY a ORDER BY a",
		"SELECT a, (SELECT count(*)+a) FROM t2 GROUP BY a ORDER BY a",
		"SELECT a, (SELECT count(*) WHERE a>1) FROM t2 GROUP BY a ORDER BY a",
		"SELECT a, (SELECT total(1)+b) FROM t2 GROUP BY a ORDER BY a",
	} {
		if !differ(t, "r26rowouter-upgraded", append(append([]string(nil), declR26Schema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
	if !differ(t, "r26rowouter-upgraded", append(append([]string(nil), declR26Schema...),
		"UPDATE t2 SET b=99 WHERE EXISTS (SELECT a INTERSECT SELECT 1)",
		"SELECT a,b FROM t2 ORDER BY a,b")) {
		t.Error("diverged on the unqualified compound WRITE")
	}
}

// TestR26RowOuterR2Upgraded is the six shapes declR26Cases used to pin as
// still-declined (see its own doc comment): a FROM-less avg() re-associating
// to the enclosing GROUP BY query (R1's own trap), plus five R2 RowExpr-in-
// aggregate shapes that only become reachable once that re-association fires
// correctly instead of the association verdict silently staying "-1" (the
// mixed-arm-compound-adjacent gap r35dNoFromAnywhere's per-arm fix closes).
// Each is asserted cell-for-cell against the oracle, matching this file's
// TestR26RowOuterUpgraded convention for the round-28 upgrades.
func TestR26RowOuterR2Upgraded(t *testing.T) {
	for _, q := range []string{
		"SELECT (SELECT avg(b)) FROM t2 GROUP BY a ORDER BY 1",
		"SELECT count(*), (c0,c1) IN (SELECT x,y FROM one) FROM t0",
		"SELECT count(*) FROM t0 HAVING (c0,c1) IN (SELECT x,y FROM one)",
		"SELECT max((c0,c1) IN (SELECT x,y FROM one)) FROM t0",
		"SELECT count(*), (min(c0),max(c1)) IN (SELECT x,y FROM one) FROM t0",
		"SELECT (0,0) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0",
	} {
		if !differ(t, "r26rowouter-r2-upgraded", append(append([]string(nil), declR26Schema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestR26RowOuterServedControls is the other half: the spellings that ALREADY
// work and must keep working. Each is the QUALIFIED (or desugared) twin of a
// declined case above, which is what makes the decline an inconsistency rather
// than a missing feature -- and what would make a regression in the substitution
// path visible here rather than only in the corpus.
func TestR26RowOuterServedControls(t *testing.T) {
	for _, q := range []string{
		"SELECT a, count(*) FROM t2 GROUP BY a HAVING EXISTS (SELECT t2.a UNION SELECT 123) ORDER BY a",
		"SELECT a, (SELECT t2.a UNION SELECT 99 ORDER BY 1 LIMIT 1) FROM t2 GROUP BY a ORDER BY a",
		"SELECT a, (SELECT count(*)+t2.a) FROM t2 GROUP BY a ORDER BY a",
		"SELECT (SELECT avg(t2.b)) FROM t2 GROUP BY a ORDER BY 1",
		"SELECT (SELECT sum(t2.b) UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM t2 GROUP BY a ORDER BY 1",
		"SELECT (0,1) IN (SELECT 0,1)",
		"SELECT count(*), (c0,c1) IN ((1,7)) FROM t0",
	} {
		if !differ(t, "r26rowouter-control", append(append([]string(nil), declR26Schema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
	if !differ(t, "r26rowouter-control", append(append([]string(nil), declR26Schema...),
		"UPDATE t2 SET b=99 WHERE EXISTS (SELECT t2.a INTERSECT SELECT 1)",
		"SELECT a,b FROM t2 ORDER BY a,b")) {
		t.Error("diverged on the qualified compound WRITE")
	}
}

// TestR26AnchorOutranksTheCompoundGap pins the ORDER the two rules must be
// applied in. R1's own refutation was a compound subquery in HAVING over an
// INDEXED table: closing the compound gap without the anchor plan-order decline
// (engine/vdbe_agg_codegen.go's anchorPlanOrderProvable) produced a PHANTOM ROW,
// because the anchor a bare column in the subquery reads is the first row of the
// chosen PLAN's scan order and this engine always scans by rowid.
//
// R1 LANDED in round 28 and this decline DID survive it, verified by this test
// running green over the fix: anchorPlanOrderProvable is consulted by the
// ENCLOSING aggregate compile, one level above anything the row threading
// touches, so serving the subquery body never reaches the shapes below.
//
// The decline has since been lifted: the ported planner now proves the covering
// index's order for this plan, so the anchor is the first row in THAT order and
// both shapes answer exactly as C SQLite does. The test compares with the
// oracle, and still asserts the oracle's answer is NOT what a rowid-order anchor
// produces, so a regression to rowid order shows up as the phantom row it is.
func TestR26AnchorOutranksTheCompoundGap(t *testing.T) {
	for _, c := range []struct{ q, phantom string }{
		// p1 = (1,100),(2,202),(2,200) with an index on (a,b): the covering
		// index visits b=200 before b=202, so the group a=2 anchors on 200 and
		// the INTERSECT finds nothing. A rowid-order anchor finds 202 and emits
		// the group.
		{"SELECT a,count(*) FROM p1 GROUP BY a HAVING EXISTS (SELECT b INTERSECT SELECT 202)", `"I:2","I:2"`},
		{"SELECT a,count(*) FROM p1 GROUP BY a HAVING EXISTS (SELECT p1.b INTERSECT SELECT 202)", `"I:2","I:2"`},
	} {
		stmts := append(append([]string(nil), declR26Schema...), c.q)
		differ(t, "r26 anchor outranks the compound gap", stmts)
		oracle, _ := json.Marshal(run(t, "cgo", stmts)[len(stmts)-1])
		if strings.Contains(string(oracle), c.phantom) {
			t.Errorf("%q: the oracle now agrees with a rowid-order anchor (%s) -- this shape no longer needs the decline: %s",
				c.q, c.phantom, oracle)
		}
	}
}
