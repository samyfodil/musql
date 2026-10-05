// Gates a wrong-answer bug where an fts4 MATCH in a LEFT JOIN's ON or WHERE
// clause was silently answered instead of raising "unable to use function MATCH
// in the requested context". Three independent C rules block consuming such
// constraints. fts5 is already correct.
package compat

import "testing"

// TestR21LeftJoinMatchBugRepro reproduces the bug: an fts4 vtab is the LEFT
// item with an ON-clause MATCH correlated to the right table. Must error.
func TestR21LeftJoinMatchBugRepro(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH correlated to the right table", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestR21LeftVtabLiteralOnClauseStillErrors verifies that an fts4 LEFT-vtab
// ON-clause MATCH with a literal pattern also errors, proving the rule is
// side-dependent, not correlation-dependent.
func TestR21LeftVtabLiteralOnClauseStillErrors(t *testing.T) {
	differ(t, "fts4 LEFT-vtab ON-clause MATCH, purely literal pattern", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON t10 MATCH 'apple' AND x.term = t10.value GROUP BY x.tag`,
	})
}

// TestR21WhereClauseVariantAlsoErrors verifies the WHERE-clause variant where
// MATCH on the LEFT-vtab is correlated to the right table and also errors.
func TestR21WhereClauseVariantAlsoErrors(t *testing.T) {
	differ(t, "fts4 LEFT-vtab WHERE-clause MATCH correlated to the right table", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM t10 LEFT JOIN x ON 1=1 WHERE t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestR21InnerJoinCorrelatedUnaffected is a control: an INNER-join
// MATCH-correlated case must continue working as before.
func TestR21InnerJoinCorrelatedUnaffected(t *testing.T) {
	differ(t, "fts4 INNER-join MATCH correlated (unaffected control)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM t10, x WHERE t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestR21RightSideVtabLiteralUnaffected is a control: an fts4 RIGHT-side
// vtab with a literal ON-clause MATCH must continue working.
func TestR21RightSideVtabLiteralUnaffected(t *testing.T) {
	differ(t, "fts4 RIGHT-side vtab, ON-clause literal MATCH (unaffected control)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE r(id INTEGER, tag INTEGER)`,
		`INSERT INTO r VALUES (1,10)`,
		`SELECT r.tag, t10.value FROM r LEFT JOIN t10 ON t10 MATCH 'apple OR banana' GROUP BY r.tag`,
	})
}

// TestR21RightSideVtabCorrelatesLeftUnaffected is a control: an fts4 RIGHT-side
// vtab with an ON-clause MATCH correlated to the LEFT item must continue working.
func TestR21RightSideVtabCorrelatesLeftUnaffected(t *testing.T) {
	differ(t, "fts4 RIGHT-side vtab, ON-clause MATCH correlated to the LEFT item (unaffected control)", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM x LEFT JOIN t10 ON t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestR21RightSideVtabWhereClauseAlwaysErrors verifies that an fts4 RIGHT-side
// vtab WHERE-clause MATCH also errors, independent of correlation.
func TestR21RightSideVtabWhereClauseAlwaysErrors(t *testing.T) {
	differ(t, "fts4 RIGHT-side vtab, WHERE-clause MATCH correlated to the LEFT item", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM x LEFT JOIN t10 ON 1=1 WHERE t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestR21RightSideVtabWhereClauseLiteralAlsoErrors verifies the WHERE-clause
// rule is side-independent: RIGHT-side with a literal pattern also errors.
func TestR21RightSideVtabWhereClauseLiteralAlsoErrors(t *testing.T) {
	differ(t, "fts4 RIGHT-side vtab, WHERE-clause MATCH, purely literal pattern", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE x(term TEXT, tag INTEGER)`,
		`INSERT INTO x VALUES ('banana',1),('apple',1)`,
		`SELECT x.tag, t10.value FROM x LEFT JOIN t10 ON 1=1 WHERE t10 MATCH 'apple'`,
	})
}
