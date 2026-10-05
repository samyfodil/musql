// This file gates aggregates with subquery arguments, verifying that column
// references within the subquery argument are correctly resolved for association.
package compat

import "testing"

// TestAggFromlessSubqueryArgMined replays the two REAL mined statements this
// bucket was scoped from (tables renamed, see file comment); rowvalue.test
// 30.1's own UPDATE...FROM wrapper is exercised separately below.
func TestAggFromlessSubqueryArgMined(t *testing.T) {
	h := newRAWHarness(t)

	// rowvalue.test 30.1's own shape, non-UPDATE spelling: a window function's
	// PARTITION BY holds a bare aggregate whose OWN argument is a FROM-less
	// subquery correlated to the enclosing query's column. sum((SELECT w2.y))
	// is a MOVER (w2.y names w2's own column, and this window body has no
	// FROM of its own to claim it), so it re-associates with w2, collapsing
	// the two-row scan to ONE. Verified against 3.53.3 over
	// w2(x,y,z)=(1000,2000,3000),(7,8,9): (1000, 1000).
	h.exec(`CREATE TABLE w2(x, y, z)`)
	h.exec(`INSERT INTO w2 VALUES(1000,2000,3000),(7,8,9)`)
	h.cmpWant(`SELECT x, (SELECT max(w2.x) OVER( PARTITION BY sum( (SELECT w2.y) ) )) FROM w2`)

	// window9.test 8.4's own shape: a compound arm's window function argument
	// is sum(avg((SELECT x FROM wv1))). avg's argument subquery has its OWN
	// FROM (FROM wv1), so it is SQLite's "-1, stays" case (the column x
	// belongs to the SUBQUERY's own FROM, not this body's) -- this shape was
	// blocked purely by the old blanket bailout, unrelated to the mover-vs-
	// stays verdict itself. Verified against 3.53.3 over wv1's two rows of 0:
	// two rows of 0.0.
	h2 := newRAWHarness(t)
	h2.exec(`CREATE VIEW wv1 AS SELECT 0 AS x UNION SELECT count() OVER() FROM (SELECT 0) ORDER BY 1`)
	h2.cmpWant(`SELECT( SELECT x UNION SELECT sum( avg((SELECT x FROM wv1)) ) OVER() ) FROM wv1`)
}

// TestAggFromlessSubqueryArgUpdateFrom replays rowvalue.test 30.1 verbatim
// (its actual UPDATE...FROM spelling, with w1 holding exactly the one row the
// real test uses -- multiple w1 rows feeding a single w2 row through
// UPDATE...FROM is documented as an arbitrary/implementation-defined choice
// even in C SQLite, so this file does not try to pin that boundary; see
// TestAggFromlessSubqueryArgUpdateFromMultiRow below for what WAS checked
// there).
func TestAggFromlessSubqueryArgUpdateFrom(t *testing.T) {
	h := newRAWHarness(t)
	h.exec(`CREATE TABLE w1(x, y, z)`)
	h.exec(`CREATE TABLE w2(a, b)`)
	h.exec(`INSERT INTO w1 VALUES(1000, 2000, 3000)`)
	h.exec(`INSERT INTO w2 VALUES(NULL, NULL)`)
	h.exec(`UPDATE w2 SET (a,b)=( SELECT max( w1.x ) OVER( PARTITION BY sum( (SELECT w1.y) ) ), 2 ) FROM w1`)
	h.cmpWant(`SELECT * FROM w2`)
}

// TestAggFromlessSubqueryArgUpdateFromMultiRow widens rowvalue.test 30.1 to a
// multi-row w1 -- SQLite's docs call UPDATE...FROM's row selection
// "arbitrary" when more than one FROM row matches a single target row, but
// THIS build's actual VDBE plan is deterministic, and matches this engine's
// own choice for every shape tried (forward order, reversed order, and a
// two-row w2 checking the update does not fan out to every w2 row). Not a
// citable SQLite RULE -- an empirical pin against the concrete oracle build,
// same standard as the rest of this harness.
func TestAggFromlessSubqueryArgUpdateFromMultiRow(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"forward order, larger x last", []string{
			`CREATE TABLE w1(x, y, z)`,
			`CREATE TABLE w2(a, b)`,
			`INSERT INTO w1 VALUES(1000, 2000, 3000),(2500,7000,9000)`,
			`INSERT INTO w2 VALUES(NULL, NULL)`,
			`UPDATE w2 SET (a,b)=( SELECT max( w1.x ) OVER( PARTITION BY sum( (SELECT w1.y) ) ), 2 ) FROM w1`,
		}},
		{"reversed order, smaller x last", []string{
			`CREATE TABLE w1(x, y, z)`,
			`CREATE TABLE w2(a, b)`,
			`INSERT INTO w1 VALUES(2500,7000,9000),(1000, 2000, 3000)`,
			`INSERT INTO w2 VALUES(NULL, NULL)`,
			`UPDATE w2 SET (a,b)=( SELECT max( w1.x ) OVER( PARTITION BY sum( (SELECT w1.y) ) ), 2 ) FROM w1`,
		}},
		{"two w2 rows, three w1 rows", []string{
			`CREATE TABLE w1(x, y)`,
			`CREATE TABLE w2(a, b)`,
			`INSERT INTO w1 VALUES(10,1),(20,2),(30,3)`,
			`INSERT INTO w2 VALUES(NULL,NULL),(NULL,NULL)`,
			`UPDATE w2 SET (a,b)=( SELECT max( w1.x ) OVER( PARTITION BY sum( (SELECT w1.y) ) ), 2 ) FROM w1`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRAWHarness(t)
			for _, s := range c.stmts {
				h.exec(s)
			}
			h.cmpWant(`SELECT * FROM w2`)
		})
	}
}

// TestAggFromlessSubqueryArgNonWindow pins the general (non-window) case the
// SAME checkAggregateAssociation fix closes: an ordinary aggregate query's
// select-list item whose subquery holds an aggregate call, and THAT
// aggregate's own argument is itself a FROM-less subquery reaching an
// enclosing column. Not window-related at all -- this is
// planNoGroupAggregate's own call into checkAggregateAssociation
// (sql_agg.go), sharing the identical verdict fix.
//
// checkAggregateAssociation correctly calls this a MOVER (C SQLite answers
// ONE row, (1, 30), not two rows of a per-w1-row-correlated sum -- (1,20)|(2,40)
// -- which is what this engine silently answered, WRONG, before that fix).
//
// It then DECLINED for a while, because PLACING the hoisted value was a second,
// separate gap: materializedOuterRefInAggArgArm (sql_group.go) restarted its
// walk at the nested "(SELECT w1.y)", so the escaping reference arrived with
// here=false and BLOCKED the item instead of recording a hoist. That gap is
// closed -- walkTransparentBody walks a FROM-less body written inside an
// already-claimed aggregate as part of the SAME level, which is what
// sqlite3ReferencesSrcList itself does (selectRefEnter returns early when a
// nested SELECT has no SrcList to exclude, expr.c:7131) -- so this ANSWERS now,
// and cmpWant compares it to 3.53.3 cell for cell instead.
func TestAggFromlessSubqueryArgNonWindow(t *testing.T) {
	h := newRAWHarness(t)
	h.exec(`CREATE TABLE w1(x, y)`)
	h.exec(`CREATE TABLE w2(b)`)
	h.exec(`INSERT INTO w1 VALUES(1,10),(2,20)`)
	h.exec(`INSERT INTO w2 VALUES(5),(6)`)
	h.cmpWant(`SELECT x, (SELECT sum((SELECT w1.y)) FROM w2) FROM w1 ORDER BY x`)
}
