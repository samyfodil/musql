//go:build sqlite_fts5

// Tests the fts5 equivalent of the leftjoin_match bug, verifying fts5's
// MATCH placement logic is correct.
package compat

import "testing"

// TestR21Fts5EquivalentAlreadyCorrect verifies fts5 correctly handles the
// leftjoin_match bug when a MATCH is placed in an ON clause.
func TestR21Fts5EquivalentAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(value)`,
		`INSERT INTO t10(rowid,value) VALUES (1,'apple'),(2,'banana'),(3,'cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
	})
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH x.term GROUP BY x.tag`)
}

// TestR21Fts5LiteralOnLeftAlreadyCorrect tests a literal value in the ON
// clause with fts5.
func TestR21Fts5LiteralOnLeftAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(value)`,
		`INSERT INTO t10(rowid,value) VALUES (1,'apple'),(2,'banana'),(3,'cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
	})
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH 'apple' AND x.term = t10.value GROUP BY x.tag`)
}

// TestR21Fts5WhereVariantAlreadyCorrect tests the WHERE-clause variant with
// fts5.
func TestR21Fts5WhereVariantAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(value)`,
		`INSERT INTO t10(rowid,value) VALUES (1,'apple'),(2,'banana'),(3,'cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
	})
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON 1=1 WHERE t10 MATCH x.term GROUP BY x.tag`)
}

// TestR21Fts5RightSideWhereClauseAlreadyCorrect is the broader, disableTerm-
// driven finding (TestR21RightSideVtabWhereClauseAlwaysErrors /
// TestR21RightSideVtabWhereClauseLiteralAlsoErrors), mapped onto fts5: a
// WHERE-clause MATCH on the RIGHT (joined-in) item of a LEFT JOIN, literal
// pattern, no correlation at all. fts5_match_placement.go's own
// fts5OuterJoinScopes/outerRight already marks exactly this scope and
// walkConjunct's "owner < 0 && outerRight[idx]" branch already poisons it
// (fts5_match_placement.go) -- landed well before this
// investigation, confirmed here rather than assumed.
func TestR21Fts5RightSideWhereClauseAlreadyCorrect(t *testing.T) {
	edb, cdb := r39dOpen(t, []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(value)`,
		`INSERT INTO t10(rowid,value) VALUES (1,'apple'),(2,'banana'),(3,'cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
	})
	r39dAgree(t, edb, cdb, `SELECT x.tag, t10.value FROM x LEFT JOIN t10 ON 1=1 WHERE t10 MATCH 'apple'`)
}
