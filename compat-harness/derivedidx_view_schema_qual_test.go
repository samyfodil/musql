package compat

// This file gates view schema qualification in order provability checks.
// Views are self-qualified but should not be treated as opaque by index analysis.

import "testing"

// minmax4Fixture is the minmax4.test schema.
func minmax4Fixture() []string {
	return []string{
		"CREATE TABLE t0(c0 UNIQUE, c1)",
		"INSERT INTO t0(c1) VALUES (0)",
		"INSERT INTO t0(c0) VALUES (0)",
		"CREATE VIEW v0(c0, c1) AS SELECT t0.c1, t0.c0 FROM t0 WHERE CAST(t0.rowid AS INT) = 1",
	}
}

// TestViewSchemaQualCorpusShape checks the mined corpus statement serves.
func TestViewSchemaQualCorpusShape(t *testing.T) {
	stmts := append(minmax4Fixture(), "SELECT v0.c0, MIN(v0.c1) FROM v0")
	differ(t, "minmax4-6.2.2", stmts)
}

// TestViewSchemaQualExplicitIndex checks explicit indices work the same way.
func TestViewSchemaQualExplicitIndex(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t0(c0, c1)",
		"CREATE INDEX t0c0 ON t0(c0)",
		"INSERT INTO t0(c1) VALUES (0)",
		"INSERT INTO t0(c0) VALUES (0)",
		"CREATE VIEW v0(c0, c1) AS SELECT t0.c1, t0.c0 FROM t0 WHERE CAST(t0.rowid AS INT) = 1",
		"SELECT v0.c0, MIN(v0.c1) FROM v0",
	}
	differ(t, "minmax4-explicit-index", stmts)
}

// TestViewSchemaQualGroupByUnindexedElsewhereIndexed reaches the identical
// guard through the GROUP BY aggregate compiler (compileScanGroupBy), over a
// view whose own base table (t0) carries NO index -- but the SCHEMA holds an
// unrelated indexed table (t1) elsewhere. Before this fix, EVERY view was
// opaque to the order proof, so anchorNoIndexInPlay fell back to
// noIndexAnywhere(p): true here would require NO index existing ANYWHERE in
// the whole schema, which t1 alone was enough to defeat -- even though
// nothing about t1 has any bearing on how t0's own (index-free) rows are
// ordered. Chosen specifically so the old whole-schema fallback could not
// accidentally still mask the fix by chance.
func TestViewSchemaQualGroupByUnindexedElsewhereIndexed(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t0(x, y, z)",
		"INSERT INTO t0 VALUES(1, 10, 100), (1, 20, 200), (2, 30, 300)",
		"CREATE TABLE t1(a UNIQUE)", // indexed, but never referenced by the query below
		"CREATE VIEW v0 AS SELECT x, y, z FROM t0",
		"SELECT x, y, MIN(z) FROM v0 GROUP BY x",
	}
	differ(t, "minmax4-groupby-unindexed-view", stmts)
}

// TestViewSchemaQualOuterUnsafe mirrors derivedidx_r40_test.go's
// TestDerivedIdxOrderOuterUnsafe for a VIEW: the view's exposed column y is
// real-indexed (t1.c is PRIMARY KEY), and the outer WHERE references y
// directly, so C SQLite's flattening substitution (select.c:4572-4704) can
// fold "y > 200" in as a new predicate on t1 before costing the index. It was
// pinned as a decline; it answers now and matches the oracle, and is compared
// with it.
func TestViewSchemaQualOuterUnsafe(t *testing.T) {
	differ(t, "view-outer-where", append(derivedIdxFixture(),
		"CREATE VIEW v1 AS SELECT c+222 AS y FROM t1",
		"SELECT * FROM (SELECT 111 AS x) LEFT JOIN v1 WHERE y > 200 GROUP BY 1",
	))
}

// TestViewSchemaQualTempShadowSafety is the correctness check this fix's own
// design explicitly requires shipping WITH, not as a follow-up: resolving a
// schema-qualified FROM item now consults fromItemScope(it) instead of
// leaving it unscoped, specifically so a same-named TEMP table created AFTER
// the view cannot be mistaken for -- or supply the census/index for -- the
// view's own MAIN table. t0 is recreated as an UNINDEXED TEMP table after
// v0 already exists; since qualifyUnqualifiedFromItems permanently pinned
// v0's body to MAIN at CREATE VIEW time (view.go's own documented rule,
// e_dropview.test), the query must still read MAIN's t0 -- unique index,
// two rows, "0 {}" answer -- and must NOT pick up the newer, unindexed,
// TEMP t0 (which would silently change the answer: a WRONG VALUE, not
// merely an unnecessary decline, if resolveTableIn were ever handed the
// wrong scope here).
func TestViewSchemaQualTempShadowSafety(t *testing.T) {
	stmts := append(minmax4Fixture(),
		"CREATE TEMP TABLE t0(c0, c1)",
		"SELECT v0.c0, MIN(v0.c1) FROM v0",
	)
	differ(t, "minmax4-temp-shadow", stmts)
}
