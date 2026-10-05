// Tests that generated columns with compound subqueries don't crash. C SQLite
// rejects subqueries in generated columns; musql previously crashed instead
// of declining. Verifies no panic occurs (oracle may differ on acceptance).
package compat

import "testing"

// gencolCompoundNilPagerCorpus tests UNION, INTERSECT, EXCEPT and multi-arm compounds.
var gencolCompoundNilPagerCorpus = []string{
	`CREATE TABLE t(x, y AS ((SELECT 1 UNION SELECT 2)))`,
	`INSERT INTO t(x) VALUES(1)`,
	`SELECT x, y FROM t`,
	`CREATE TABLE tv(x, y AS ((SELECT 1 UNION SELECT 2)) VIRTUAL)`,
	`INSERT INTO tv(x) VALUES(1)`,
	`SELECT x, y FROM tv`,
	`CREATE TABLE ti(x, y AS ((SELECT 5 INTERSECT SELECT 5)))`,
	`INSERT INTO ti(x) VALUES(1)`,
	`SELECT x, y FROM ti`,
	`CREATE TABLE te(x, y AS ((SELECT 5 EXCEPT SELECT 6)))`,
	`INSERT INTO te(x) VALUES(1)`,
	`SELECT x, y FROM te`,
	`CREATE TABLE t3(x, y AS ((SELECT 1 UNION SELECT 2 UNION SELECT 3)))`,
	`INSERT INTO t3(x) VALUES(1)`,
	`SELECT x, y FROM t3`,
}

// TestGeneratedColumnCompoundDefaultNoPagerPanic verifies the corpus runs
// without crashing.
func TestGeneratedColumnCompoundDefaultNoPagerPanic(t *testing.T) {
	differ(t, "generated column compound subquery", gencolCompoundNilPagerCorpus)
}
