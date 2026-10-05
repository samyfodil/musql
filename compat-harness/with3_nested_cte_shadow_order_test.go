// cteOrderProvable (engine/vdbe_join_codegen.go) reused
// derivedSubqueryScanOrderProvable to prove a CTE reference's own row order,
// but omitted pushing the CTE body's OWN nested WITH clause before that call
// -- the same "SQLite never re-resolves" bug class AGENTS.md's read-the-source section
// section is written from (select.c:6000 sqlite3WithPush). An unqualified
// name inside the body that a nested CTE shadows resolved against whatever
// the OUTER scope had by that name instead.
package compat

import "testing"

func TestNestedCTEShadowsOuterTableForOrderProof(t *testing.T) {
	differ(t, "outer CTE's local WITH shadows an outer table of the same name", []string{
		`CREATE TABLE y(v)`,
		`CREATE TABLE a(k INTEGER)`,
		`CREATE TABLE b(k INTEGER)`,
		`INSERT INTO a VALUES(1),(2)`,
		`INSERT INTO b VALUES(10),(20)`,
		`INSERT INTO y VALUES(999)`,
		`WITH outer_cte AS ( WITH y(k) AS (SELECT a.k FROM a, b) SELECT k FROM y ) SELECT count(*), k FROM outer_cte`,
		// A GROUP BY over the CTE reference so an order-sensitive aggregate
		// path (anchorNoIndexInPlay, engine/vdbe_agg_codegen.go) is
		// actually exercised, not just a plain scalar count.
		`WITH outer_cte AS ( WITH y(k) AS (SELECT a.k FROM a, b) SELECT k FROM y ) SELECT k, count(*) FROM outer_cte GROUP BY k ORDER BY k`,
	})
}
