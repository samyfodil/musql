// Gates fts3MatchBindings fix: parenthesized JOIN groups in FROM collapse jts entries,
// breaking the onOwnerOK/onOwner shortcut. fromToJts projection translates both idx and onOwner.
// (gatherScopesRow, vdbe.go: a group member's row is read through its
// group's own single shared cursor, a different shape than that function
// assumes). fromToJts reports -1 for such a member (no top-level jts entry
// exists for it at all), and fts3MatchBindings now treats an unmappable
// scope -- subject OR pattern dependency -- as grounds to poison exactly
// like a genuine ftsMatchLeftJoinUnusable verdict, turning the crash into
// this file's ordinary "unable to use function MATCH" decline instead.
package compat

import "testing"

// TestR23UnrelatedGroupBeforeVtabPairBugRepro is the exact repro above.
func TestR23UnrelatedGroupBeforeVtabPairBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, unrelated paren-joined group BEFORE the pair", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`CREATE TABLE a(id INTEGER)`,
		`CREATE TABLE b(id INTEGER)`,
		`INSERT INTO a VALUES (1)`,
		`INSERT INTO b VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, (a LEFT JOIN b ON a.id=b.id), t10 LEFT JOIN x ON t10 MATCH x.term`,
	})
}

// TestR23UnrelatedGroupAfterVtabPairBugRepro moves the SAME unrelated group
// to AFTER the vtab pair -- confirms the fix doesn't depend on the group's
// position relative to the pair, only on it being present at all.
func TestR23UnrelatedGroupAfterVtabPairBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, unrelated paren-joined group AFTER the pair", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`CREATE TABLE a(id INTEGER)`,
		`CREATE TABLE b(id INTEGER)`,
		`INSERT INTO a VALUES (1)`,
		`INSERT INTO b VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH x.term, y, (a LEFT JOIN b ON a.id=b.id)`,
	})
}

// Unrelated group sandwiched between CROSS-joined table and vtab pair.
func TestR23UnrelatedGroupBetweenPairAndOwnerBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, unrelated CROSS-joined group directly before the pair", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`CREATE TABLE a(id INTEGER)`,
		`CREATE TABLE b(id INTEGER)`,
		`INSERT INTO a VALUES (1)`,
		`INSERT INTO b VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y CROSS JOIN (a LEFT JOIN b ON a.id=b.id) JOIN t10 LEFT JOIN x ON t10 MATCH x.term`,
	})
}

// WHERE-clause variant with three-item unrelated group.
// jts index for THIS particular arrangement (a different bug -- an
// accidental alias, not a proof of correctness -- masking the divergence);
// a 3-item group's wider shift breaks that coincidence and reproduces the
// real, general bug. Confirmed live pre-fix: cgo errors, musql answered 3
// rows for the identical 2-item-group SQL that a wider group DOES diverge
// on; both this file's own committed fix and a plain per-statement idx
// bounds check would otherwise miss it (idx==len(jts) here, so it never
// even reaches ftsMatchLeftJoinUnusable's own x-comparison loop pre-fix).
func TestR23UnrelatedGroupWhereClauseVariantBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab WHERE-clause MATCH, unrelated 3-item paren-joined group present", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`CREATE TABLE p(id INTEGER)`,
		`CREATE TABLE q(id INTEGER)`,
		`CREATE TABLE r(id INTEGER)`,
		`INSERT INTO p VALUES (1)`,
		`INSERT INTO q VALUES (1)`,
		`INSERT INTO r VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, (p LEFT JOIN q ON p.id=q.id JOIN r ON q.id=r.id), t10 LEFT JOIN x ON 1=1 WHERE t10 MATCH x.term`,
	})
}

// TestR23UnrelatedGroupRightSideVtabUnaffected is the "vtab as the LEFT
// JOIN's own right-hand item, MATCHed in that SAME join's own ON clause"
// control (never poisoned -- EP_OuterON safe) plus the identical unrelated
// group, confirming the per-conjunct fix still recognizes this shape
// correctly rather than over-declining once a group is anywhere in scope.
func TestR23UnrelatedGroupRightSideVtabUnaffected(t *testing.T) {
	differ(t, "fts4 RIGHT-side vtab, ON-clause MATCH, unrelated paren-joined group present (unaffected control)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`CREATE TABLE a(id INTEGER)`,
		`CREATE TABLE b(id INTEGER)`,
		`INSERT INTO a VALUES (1)`,
		`INSERT INTO b VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, (a LEFT JOIN b ON a.id=b.id), x LEFT JOIN t10 ON t10 MATCH x.term`,
	})
}

// TestR23UnrelatedGroupInnerJoinCorrelatedUnaffected is ftsMatchForcedOrder's
// own 2-item forced-order shape (no LEFT JOIN at all) plus the unrelated
// group -- must keep answering real rows.
func TestR23UnrelatedGroupInnerJoinCorrelatedUnaffected(t *testing.T) {
	differ(t, "fts4 INNER-join MATCH correlated, unrelated paren-joined group present (unaffected control)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`CREATE TABLE a(id INTEGER)`,
		`CREATE TABLE b(id INTEGER)`,
		`INSERT INTO a VALUES (1)`,
		`INSERT INTO b VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, (a LEFT JOIN b ON a.id=b.id), t10, x WHERE t10 MATCH x.term ORDER BY t10.value`,
	})
}

// TestR23VtabInteriorToGroupOutsideMatchNoLongerPanics is the bonus finding:
// a vtab as a NON-CONNECTOR member of a group's own span (reachable --
// resolveGroupSource only declines a vtab AS the connector), MATCHed from a
// WHERE clause entirely outside the group. This used to panic
// (gatherScopesRow, vdbe.go) rather than answer wrong; it must now decline
// cleanly, agreeing with the oracle's own error.
func TestR23VtabInteriorToGroupOutsideMatchNoLongerPanics(t *testing.T) {
	differ(t, "vtab interior to a group's span, MATCHed from an outside WHERE clause", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE a(id INTEGER)`,
		`INSERT INTO a VALUES (1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, t10.value FROM y, (a LEFT JOIN t10 ON a.id = t10.rowid) WHERE t10 MATCH 'apple'`,
	})
}

// TestR23VtabInteriorToGroupPatternRefNoLongerPanics is a variant of the
// SAME bonus finding where only the MATCH's PATTERN -- never the subject --
// reads a column interior to the group (the vtab itself sits outside it, as
// the LEFT JOIN's own right-hand item). This particular arrangement already
// happened to decline pre-fix too, via round 21's own unrelated onOwner<0
// idx==0 shortcut (coincidence, not proof: that shortcut never looks at the
// pattern at all) -- so it does not independently pin a divergence the way
// the sibling test above does. Kept anyway as a direct regression pin on
// fts3MatchBindings' own new "any UNMAPPABLE pattern dependency poisons the
// scope" rule, which is what actually guarantees this shape can never reach
// gatherScopesRow regardless of which other shortcut would otherwise apply.
func TestR23VtabInteriorToGroupPatternRefNoLongerPanics(t *testing.T) {
	differ(t, "vtab outside a group, MATCH pattern reads a column interior to it", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE a(id INTEGER)`,
		`INSERT INTO a VALUES (1)`,
		`CREATE TABLE w(term TEXT)`,
		`INSERT INTO w VALUES ('apple')`,
		`SELECT t10.value FROM t10 LEFT JOIN (a JOIN w ON a.id = 1) ON t10 MATCH w.term`,
	})
}

// TestR23TwoItemAndThreeItemCasesStillFixed re-runs round 21's and round
// 22's own repros unchanged, on top of round 23's fix -- the new
// fromToJts-based translation must not alter the verdict for either shape
// it replaces the logic under.
func TestR23TwoItemAndThreeItemCasesStillFixed(t *testing.T) {
	differ(t, "round 21 repro, unaffected by round 23", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH x.term GROUP BY x.tag`,
	})
	differ(t, "round 22 repro, unaffected by round 23", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`CREATE TABLE y(id INTEGER)`,
		`INSERT INTO y VALUES (1)`,
		`SELECT y.id, x.tag, t10.value FROM y, t10 LEFT JOIN x ON t10 MATCH x.term`,
	})
}
