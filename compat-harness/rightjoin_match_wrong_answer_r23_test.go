// Gate for RIGHT/FULL JOIN on-clause MATCH constraints, which should be rejected like LEFT JOIN.
package compat

import "testing"

func r23Setup() []string {
	return []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
	}
}

// TestR23RightJoinOnClauseCorrelatedBugRepro tests RIGHT JOIN with ON-clause MATCH.
func TestR23RightJoinOnClauseCorrelatedBugRepro(t *testing.T) {
	differ(t, "fts4 RIGHT JOIN, ON-clause correlated MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM t10 RIGHT JOIN x ON t10 MATCH x.term GROUP BY x.tag`,
	))
}

// TestR23FullJoinOnClauseCorrelatedBugRepro tests FULL JOIN with ON-clause MATCH.
func TestR23FullJoinOnClauseCorrelatedBugRepro(t *testing.T) {
	differ(t, "fts4 FULL JOIN, ON-clause correlated MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM t10 FULL JOIN x ON t10 MATCH x.term GROUP BY x.tag`,
	))
}

// TestR23RightJoinWhereClauseCorrelatedBugRepro is the WHERE-clause spelling
// of the same shape ("ON 1=1 WHERE t10 MATCH x.term" instead of the ON
// clause) -- the mirror check the LEFT JOIN round always paired with its own
// ON-clause finding, applied here for RIGHT JOIN.
func TestR23RightJoinWhereClauseCorrelatedBugRepro(t *testing.T) {
	differ(t, "fts4 RIGHT JOIN, WHERE-clause correlated MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM t10 RIGHT JOIN x ON 1=1 WHERE t10 MATCH x.term`,
	))
}

// TestR23RightJoinWhereClauseLiteralBugRepro is the round's OWN new finding:
// unlike the LEFT JOIN WHERE-clause rule (pattern-gated -- a literal MATCH
// needing no correlation at all stays usable, see the LEFT control test
// below), a RIGHT/FULL join's own JT_LTORJ tagging makes ANY plain
// WHERE-clause MATCH on the earlier item unusable, literal pattern or not
// (constraintCompatibleWithOuterJoin, where.c:819-852 -- see
// ftsMatchLeftJoinUnusable's own doc comment). Verified live before writing
// any code: the oracle errors here too, not just on the correlated form.
func TestR23RightJoinWhereClauseLiteralBugRepro(t *testing.T) {
	differ(t, "fts4 RIGHT JOIN, WHERE-clause LITERAL MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM t10 RIGHT JOIN x ON 1=1 WHERE t10 MATCH 'apple'`,
	))
}

// TestR23FullJoinWhereClauseLiteralBugRepro is the FULL JOIN mirror of the
// literal-WHERE finding above.
func TestR23FullJoinWhereClauseLiteralBugRepro(t *testing.T) {
	differ(t, "fts4 FULL JOIN, WHERE-clause LITERAL MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM t10 FULL JOIN x ON 1=1 WHERE t10 MATCH 'apple'`,
	))
}

// TestR23LeftJoinWhereClauseLiteralStillUsable is the control this round
// must NOT regress: a LEFT JOIN's WHERE-clause MATCH stays pattern-gated, so
// a literal pattern (no correlation needed at all) is perfectly usable --
// unlike the RIGHT/FULL case immediately above. Both engines answer rows.
func TestR23LeftJoinWhereClauseLiteralStillUsable(t *testing.T) {
	differ(t, "fts4 LEFT JOIN, WHERE-clause LITERAL MATCH (control, stays usable)", append(r23Setup(),
		`SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON 1=1 WHERE t10 MATCH 'apple'`,
	))
}

// TestR23VtabAsRightJoinTargetOnClauseStillUsable is the mirror shape round
// 21 already proved safe for LEFT JOIN: the vtab AS the join step's own
// right-hand (joined-in) item, MATCHed in that SAME join's own ON clause, is
// EP_OuterON-safe regardless of join kind -- both engines answer rows here,
// unaffected by this round's fix (the early bailout this round removed
// already produced the same "not unusable" verdict for this shape, just for
// the wrong reason -- see ftsMatchLeftJoinUnusable's own doc comment).
func TestR23VtabAsRightJoinTargetOnClauseStillUsable(t *testing.T) {
	differ(t, "fts4 vtab AS the RIGHT JOIN target, ON-clause correlated MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM x RIGHT JOIN t10 ON t10 MATCH x.term`,
	))
}

// TestR23VtabAsFullJoinTargetOnClauseStillUsable is the FULL JOIN mirror.
func TestR23VtabAsFullJoinTargetOnClauseStillUsable(t *testing.T) {
	differ(t, "fts4 vtab AS the FULL JOIN target, ON-clause correlated MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM x FULL JOIN t10 ON t10 MATCH x.term`,
	))
}

// TestR23VtabAsRightJoinTargetWhereClauseBugRepro is a SECOND, independent
// silent-wrong-answer bug this round's fix also closes (found while
// verifying the mirror shape, not assumed): the vtab as the RIGHT-joined
// item itself, MATCHed from a plain WHERE clause (no ON-clause tag at all).
// musql answered rows here before this round -- the WHERE-clause loop's old
// early bailout ("!jts[x].left || jts[x].rightOuter") skipped x==idx (a pure
// RIGHT target) entirely instead of reaching the idx==x case that already
// existed for LEFT.
func TestR23VtabAsRightJoinTargetWhereClauseBugRepro(t *testing.T) {
	differ(t, "fts4 vtab AS the RIGHT JOIN target, WHERE-clause literal MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM x RIGHT JOIN t10 ON 1=1 WHERE t10 MATCH 'apple'`,
	))
}

// TestR23VtabAsFullJoinTargetWhereClauseBugRepro is the FULL JOIN mirror.
func TestR23VtabAsFullJoinTargetWhereClauseBugRepro(t *testing.T) {
	differ(t, "fts4 vtab AS the FULL JOIN target, WHERE-clause literal MATCH", append(r23Setup(),
		`SELECT x.tag, t10.value FROM x FULL JOIN t10 ON 1=1 WHERE t10 MATCH 'apple'`,
	))
}

// TestR23InnerJoinWhereClauseCorrelatedStillUsable is the ordinary,
// unaffected control: a plain (INNER) join's WHERE-clause correlated MATCH,
// which this round must not touch at all.
func TestR23InnerJoinWhereClauseCorrelatedStillUsable(t *testing.T) {
	differ(t, "fts4 INNER JOIN, WHERE-clause correlated MATCH (control)", append(r23Setup(),
		`SELECT x.tag, t10.value FROM t10 JOIN x ON 1=1 WHERE t10 MATCH x.term`,
	))
}

// TestR23ThreeItemRightJoinBugRepro extends the round-22 3+-item precedent
// (an unrelated comma-joined table around the SAME two-item pairing) to
// RIGHT JOIN, confirming the per-x loop generalizes without needing any new
// width-tracking (musql's own rightOuterIndex invariant -- at most one
// RIGHT/FULL-joined item per FROM clause, always the LAST one -- means the
// single rightOuter x is already found by the existing loop).
func TestR23ThreeItemRightJoinBugRepro(t *testing.T) {
	setup := append(r23Setup(), `CREATE TABLE y(id INTEGER)`, `INSERT INTO y VALUES (1)`)
	differ(t, "fts4 RIGHT JOIN, 3-item FROM, unrelated comma-joined table before", append(setup,
		`SELECT y.id, x.tag, t10.value FROM y, t10 RIGHT JOIN x ON t10 MATCH x.term`,
	))
}

// TestR23ThreeItemRightJoinTrailingBugRepro moves the unrelated table to
// AFTER the RIGHT JOIN pair -- rightOuterIndex requires the rightOuter item
// to be the LAST item of the FROM clause, so this places y after x, not
// between t10 and x.
func TestR23ThreeItemRightJoinTrailingBugRepro(t *testing.T) {
	setup := append(r23Setup(), `CREATE TABLE y(id INTEGER)`, `INSERT INTO y VALUES (1)`)
	differ(t, "fts4 RIGHT JOIN, 3-item FROM, unrelated comma-joined table after", append(setup,
		`SELECT y.id, x.tag, t10.value FROM t10 RIGHT JOIN x ON t10 MATCH x.term, y`,
	))
}
