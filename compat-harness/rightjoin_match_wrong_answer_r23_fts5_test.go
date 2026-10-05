//go:build sqlite_fts5

// This file gates RIGHT/FULL JOIN with MATCH for fts5. fts5 already correctly
// handles this case and requires no fix.
package compat

import "testing"

func r23Fts5Setup() []string {
	return []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(value)`,
		`INSERT INTO t10(rowid,value) VALUES (1,'apple'),(2,'banana'),(3,'cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
	}
}

// TestR23Fts5RightJoinOnClauseAlreadyCorrect is the fts3/4 bug repro
// (TestR23RightJoinOnClauseCorrelatedBugRepro) mapped onto fts5. musql
// already refuses here (measured "no query solution", fts5's own
// SQLITE_CONSTRAINT-from-xBestIndex path -- fts5_main.c:657-661 -- a
// DIFFERENT real-SQLite mechanism than fts3's sqlite3InvalidFunction
// placeholder, but the same observable outcome: both engines refuse).
func TestR23Fts5RightJoinOnClauseAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, r23Fts5Setup())
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM t10 RIGHT JOIN x ON t10 MATCH x.term`)
}

// TestR23Fts5FullJoinOnClauseAlreadyCorrect is the FULL JOIN mirror.
func TestR23Fts5FullJoinOnClauseAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, r23Fts5Setup())
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM t10 FULL JOIN x ON t10 MATCH x.term`)
}

// TestR23Fts5RightJoinWhereClauseCorrelatedAlreadyCorrect is the WHERE-
// clause spelling of the same shape.
func TestR23Fts5RightJoinWhereClauseCorrelatedAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, r23Fts5Setup())
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM t10 RIGHT JOIN x ON 1=1 WHERE t10 MATCH x.term`)
}

// TestR23Fts5RightJoinWhereClauseLiteralAlreadyCorrect is the LITERAL-
// pattern WHERE-clause finding from the fts3/4 side, mapped onto fts5: here
// musql's compileFts5Match reaches the SPECIFIC "unable to use function
// MATCH" error (fts5MatchGood correctly withholds this scope), matching the
// exact mechanism -- unlike the ON-clause shapes above, which are refused
// earlier, during planning ("no query solution").
func TestR23Fts5RightJoinWhereClauseLiteralAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, r23Fts5Setup())
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM t10 RIGHT JOIN x ON 1=1 WHERE t10 MATCH 'apple'`)
}

// TestR23Fts5VtabAsRightJoinTargetWhereClauseAlreadyCorrect is the vtab-as-
// the-RIGHT-target WHERE-clause literal shape (the fts3/4 side's SECOND,
// independent bug this round's fix also closed there) -- fts5 already
// refuses it too.
func TestR23Fts5VtabAsRightJoinTargetWhereClauseAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, r23Fts5Setup())
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM x RIGHT JOIN t10 ON 1=1 WHERE t10 MATCH 'apple'`)
}

// TestR23Fts5VtabAsRightJoinTargetOnClauseOverRefusalGap is a DELIBERATE,
// documented over-refusal, not a wrong answer: the vtab as the RIGHT JOIN's
// own right-hand item, MATCHed in that SAME join's own ON clause, is
// EP_OuterON-safe and the oracle answers 3 rows (confirmed live) -- the
// fts3/4 side answers this correctly (TestR23VtabAsRightJoinTargetOnClause
// StillUsable), but fts5 still refuses ("no query solution"), most likely
// because this shape never reaches fts5MatchBindings/compileFts5Match at
// all (some earlier, unrelated RIGHT-JOIN-plus-vtab planning gate declines
// it first) rather than a wrong verdict inside fts5's own placement logic.
// "Never wrong" makes this a SAFE gap to leave: musql simply answers less
// than it could here, never more. Not this round's job to close (a coverage
// gap, not a bug) -- if this ever starts passing, delete this r39dGap entry
// and fold the query into a real r39dAgree pin instead.
func TestR23Fts5VtabAsRightJoinTargetOnClauseOverRefusalGap(t *testing.T) {
	edb, cdb := r39dOpen(t, r23Fts5Setup())
	r39dGap(t, edb, cdb,
		`SELECT x.tag, t10.value FROM x RIGHT JOIN t10 ON t10 MATCH x.term`,
		"give fts5-vtab-as-RIGHT-JOIN-target its own forced-order/placement support, mirroring the fts3/4 side's idx==x EP_OuterON-safe case")
}
