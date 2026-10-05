// Tests that FTS4 MATCH in ON clauses is rejected in 3+-item FROM lists,
// just as it is in 2-item lists.
package compat

import "testing"

// TestR22ThreeItemCommaJoinBugRepro reproduces the bug: an unrelated table
// comma-joined with the t10 LEFT JOIN x pairing should cause an error.
func TestR22ThreeItemCommaJoinBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, 3-item FROM, unrelated comma-joined table", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, t10 LEFT JOIN x ON t10 MATCH x.term`,
	})
}

// TestR22ThreeItemCrossJoinBugRepro tests the same case with explicit CROSS JOIN
// instead of comma-join to verify the fix works regardless of join type.
func TestR22ThreeItemCrossJoinBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, 3-item FROM, unrelated CROSS-joined table", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y CROSS JOIN t10 LEFT JOIN x ON t10 MATCH x.term`,
	})
}

// TestR22ThreeItemUnrelatedTableAfterBugRepro moves the unrelated table to
// after the LEFT JOIN pair to verify the fix is position-independent.
func TestR22ThreeItemUnrelatedTableAfterBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, 3-item FROM, unrelated table AFTER the join pair", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH x.term, y`,
	})
}

// TestR22ThreeItemWhereClauseVariantBugRepro is the WHERE-clause spelling
// of the same 3-item shape (an unrelated "ON 1=1" keeps the LEFT JOIN
// itself intact) -- exercising the WHERE-clause prereqRight/mUsable rule
// (where.c:4390), not just the ON-clause Ticket #3015 rule, in the presence
// of an unrelated third item.
func TestR22ThreeItemWhereClauseVariantBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab WHERE-clause MATCH, 3-item FROM, unrelated comma-joined table", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, t10 LEFT JOIN x ON 1=1 WHERE t10 MATCH x.term`,
	})
}

// TestR22TwoItemCaseStillFixed re-runs round 21's own original bug repro
// unchanged: the 3+-item generalization must not alter the verdict for the
// exact shape it already fixed.
func TestR22TwoItemCaseStillFixed(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH correlated to the right table (round 21 repro, unaffected)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestR22ThreeItemInnerJoinCorrelatedUnaffected is a 3-item FROM where the
// vtab is NOT LEFT-joined at all (an ordinary INNER-join MATCH correlation,
// exactly ftsMatchForcedOrder's own 2-item forced-order shape, plus an
// unrelated third table) -- must keep answering real rows, completely
// unaffected by this round's LEFT-JOIN-specific rule.
func TestR22ThreeItemInnerJoinCorrelatedUnaffected(t *testing.T) {
	differ(t, "fts4 INNER-join MATCH correlated, 3-item FROM, no LEFT JOIN (unaffected control)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, t10, x WHERE t10 MATCH x.term ORDER BY t10.value`,
	})
}

// TestR22ThreeItemRightSideVtabUnaffected is a 3-item FROM where the vtab
// IS the LEFT JOIN's own right-hand (joined-in) item, MATCHed in that SAME
// join's own ON clause -- the one shape ftsMatchLeftJoinUnusable must never
// poison (EP_OuterON safe) -- plus an unrelated third table, to confirm the
// generalized per-step rule still recognizes this control correctly.
func TestR22ThreeItemRightSideVtabUnaffected(t *testing.T) {
	differ(t, "fts4 RIGHT-side vtab, ON-clause MATCH, 3-item FROM (unaffected control)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, x LEFT JOIN t10 ON t10 MATCH x.term`,
	})
}

// TestR22ThreeItemAggregateStillDeclines confirms a 3+-item FROM with an
// aggregate/GROUP BY over the identical bug shape still declines today for
// an UNRELATED, already-existing reason (a multi-table JOIN LOOP ORDER
// decline this round does not touch) -- not silently answering wrong, and
// not newly affected by this round's fix either.
func TestR22ThreeItemAggregateStillDeclines(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, 3-item FROM, GROUP BY (unrelated pre-existing decline)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, t10 LEFT JOIN x ON t10 MATCH x.term GROUP BY x.tag`,
	})
}
