// Tests that a derived source with no index of its own is correctly recognized
// for DISTINCT aggregates, even when other unrelated tables have indexes.
package compat

import "testing"

func TestDerivedSourceAnchorNoIndexRealValue(t *testing.T) {
	differ(t, "derived_anchor_noindex_real_value", []string{
		`CREATE TABLE t(x,y)`,
		`CREATE TABLE t_other(z)`,
		`CREATE INDEX i_other ON t_other(z)`,
		`INSERT INTO t VALUES(3,'a')`,
		`INSERT INTO t VALUES(1,'b')`,
		`INSERT INTO t VALUES(2,'c')`,
		`SELECT group_concat(DISTINCT x) FROM (SELECT x FROM t)`,
	})
}

// TestDerivedSourceAnchorNoIndexCTEShadowStaysSafe is the exact wrong-answer
// trap from a PRIOR reverted attempt at a similar optimization: a CTE
// SHADOWS a same-named REAL table that carries an index. This must never
// treat the shadowed (indexed) table's identity as the derived subquery's
// own -- p.lookupCTE, checked inside resolveDerivedSource's own
// pushCTEScope window (the correct time), must see the CTE and decline to
// prove "no index" here, falling back to the conservative noIndexAnywhere
// path instead.
func TestDerivedSourceAnchorNoIndexCTEShadowStaysSafe(t *testing.T) {
	differ(t, "derived_anchor_noindex_cte_shadow_safe", []string{
		`CREATE TABLE t1(x TEXT COLLATE NOCASE, y INTEGER)`,
		`CREATE INDEX ix ON t1(x)`,
		`INSERT INTO t1 VALUES('ABC', 7)`,
		`SELECT count(*) FROM (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1) WHERE x='ABC'`,
	})
}

// TestDerivedSourceAnchorNoIndexGenuineIndexNowProvable: when this test was
// first written (predating derivedSubqueryScanOrderProvable's
// wherePlanSingleTableIndexOrder fallback, and predating the
// indexed-anchor-remainder widening that lets a persisted view's own
// self-qualified FROM item reach this proof at all), a derived source whose
// table DOES carry an index had no proof and stayed declined unconditionally
// -- verified against the actual mined repro's own schema (minmax4.test's
// v0, whose body reads a table with an implicit UNIQUE index).
//
// Both of those since shipped, and hasIndex now defers to
// wherePlanSingleTableIndexOrder rather than refusing outright -- so this
// exact shape is servable when that function can prove the chosen access
// path's order (see derivedSubqueryScanOrderProvable, vdbe_join_codegen.go).
// Re-verified directly against the real oracle (not just "some answer,
// declined or not"): byte-exact agreement here, AND on two harder adversarial
// variants confirmed by hand while investigating this test's failure after
// the indexed-anchor-remainder merge -- a multi-row case with no WHERE
// (musql and cgo both return rowid/insertion order, not index order: no
// predicate favors the index, so neither planner uses it), and a range-scan
// case selective enough that cgo's own planner DOES switch to the index for
// access (musql matches cgo's resulting index-ORDER output exactly, not
// just its row set). This is no longer "Class A, out of scope" -- it is
// exactly the mechanism's intended proof, now reaching one more FROM-item
// shape than it used to.
func TestDerivedSourceAnchorNoIndexGenuineIndexNowProvable(t *testing.T) {
	differ(t, "derived_anchor_noindex_genuine_index_now_provable", []string{
		`CREATE TABLE t0(c0 UNIQUE, c1)`,
		`INSERT INTO t0(c1) VALUES (0)`,
		`INSERT INTO t0(c0) VALUES (0)`,
		`CREATE VIEW v0(c0, c1) AS SELECT t0.c1, t0.c0 FROM t0 WHERE CAST(t0.rowid AS INT) = 1`,
		`SELECT v0.c0, group_concat(DISTINCT v0.c1) FROM v0`,
	})
}
