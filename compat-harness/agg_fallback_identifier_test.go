package compat

import "testing"

// TestAggregateFallbackIdentifierAssociation compares with C SQLite an
// aggregate whose argument is a double-quoted identifier or TRUE/FALSE. Such a
// name falls back to a literal only when NO enclosing query binds it
// (resolve.c:719-745); when one does, it is that query's column and the
// aggregate re-associates there, changing its row count.
func TestAggregateFallbackIdentifierAssociation(t *testing.T) {
	differ(t, "aggregate over a double-quoted or TRUE/FALSE identifier", []string{
		`CREATE TABLE t(a, "true")`, `INSERT INTO t VALUES(1, 5),(2, 6)`,
		`CREATE TABLE u(a)`, `INSERT INTO u VALUES(10),(20),(30)`,
		`CREATE TABLE w(z)`, `INSERT INTO w VALUES(7)`,
		`SELECT (SELECT count(TRUE) FROM w AS x) FROM t`,
		`SELECT (SELECT max("a") FROM (SELECT 1 AS b)) FROM t`,
		`SELECT (SELECT max("a") FROM w) FROM t`,
		`SELECT (SELECT max("a") FROM u) FROM t`,
		`SELECT (SELECT sum("nosuch") FROM w) FROM t`,
		`SELECT (SELECT count("a") FROM w GROUP BY z) FROM t`,
		`SELECT (SELECT z FROM w GROUP BY z HAVING max("a")>1) FROM t`,
		`SELECT (SELECT (SELECT max("a") FROM w) FROM u LIMIT 1) FROM t`,
		`SELECT (SELECT (SELECT max("z") FROM (SELECT 1 AS q)) FROM w) FROM t`,
	})
}
