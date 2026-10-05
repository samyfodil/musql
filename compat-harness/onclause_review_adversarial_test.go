package compat

import "testing"

// Independent adversarial review of ON clause to WHERE promotion guards.

// TestReviewOnClauseGroupMemberReferencesOutsideRightwardAlias tests a
// parenthesized join group where members reference aliases outside the group.
func TestReviewOnClauseGroupMemberReferencesOutsideRightwardAlias(t *testing.T) {
	differAllowingDeclines(t, "review_group_member_rightward_outside", []string{
		`CREATE TABLE t0(a)`, `CREATE TABLE t1(b)`, `CREATE TABLE t2(c)`, `CREATE TABLE t3(d)`,
		`INSERT INTO t0 VALUES(1)`, `INSERT INTO t1 VALUES(1)`,
		`INSERT INTO t2 VALUES(1)`, `INSERT INTO t3 VALUES(1)`,
		// t1's ON references t3, which is introduced by a LATER plain INNER
		// join outside the parenthesized group (t0 JOIN (t1 JOIN t2 ...)).
		`SELECT * FROM t0 JOIN (t1 JOIN t2 ON t1.b=t2.c) ON (t3.d NOTNULL) JOIN t3 ON 1`,
	})
}

// TestReviewOnClauseInnerRightwardWithRightJoinBeforeIt tests an INNER join
// with ON referencing a rightward alias, combined with a RIGHT JOIN.
func TestReviewOnClauseInnerRightwardWithRightJoinBeforeIt(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t0(a)`, `CREATE TABLE t1(b)`, `CREATE TABLE t2(c)`, `CREATE TABLE t3(d)`,
		// t0 JOIN t1: plain INNER, ON references t3 (introduced later).
		// t1 RIGHT JOIN t2: a RIGHT join sits between them in the FROM clause.
		`SELECT * FROM t0 JOIN t1 ON (t3.d NOTNULL) RIGHT JOIN t2 ON 1 JOIN t3 ON 1`,
	}
	oracleRes := run(t, "cgo", stmts)
	musqlRes := run(t, "musql", stmts)
	oracleErr := oracleRes[len(stmts)-1]["kind"] == "error"
	musqlErr := musqlRes[len(stmts)-1]["kind"] == "error"
	if oracleErr != musqlErr {
		t.Fatalf("agreement mismatch: oracle error=%v (%v) musql error=%v (%v)",
			oracleErr, oracleRes[len(stmts)-1], musqlErr, musqlRes[len(stmts)-1])
	}
}

// A FULL JOIN combined with a plain INNER join elsewhere in the same FROM
// clause, same shape as above but with FULL OUTER JOIN.
func TestReviewOnClauseInnerRightwardWithFullJoinBeforeIt(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t0(a)`, `CREATE TABLE t1(b)`, `CREATE TABLE t2(c)`, `CREATE TABLE t3(d)`,
		`SELECT * FROM t0 JOIN t1 ON (t3.d NOTNULL) FULL JOIN t2 ON 1 JOIN t3 ON 1`,
	}
	oracleRes := run(t, "cgo", stmts)
	musqlRes := run(t, "musql", stmts)
	oracleErr := oracleRes[len(stmts)-1]["kind"] == "error"
	musqlErr := musqlRes[len(stmts)-1]["kind"] == "error"
	if oracleErr != musqlErr {
		t.Fatalf("agreement mismatch: oracle error=%v (%v) musql error=%v (%v)",
			oracleErr, oracleRes[len(stmts)-1], musqlErr, musqlRes[len(stmts)-1])
	}
}

// A RIGHT JOIN sitting AFTER an otherwise-eligible plain INNER join whose ON
// clause references a rightward alias introduced between the RIGHT JOIN and
// the reference -- exercises the "groupOrRightOuter" guard's ANYWHERE-in-
// clause scope (not just "before" the referencing item).
func TestReviewOnClauseInnerRightwardBeforeRightJoinLater(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t0(a)`, `CREATE TABLE t1(b)`, `CREATE TABLE t2(c)`, `CREATE TABLE t3(d)`,
		// t0 JOIN t1 ON references t2 (rightward). t2 RIGHT JOIN t3 comes after.
		`SELECT * FROM t0 JOIN t1 ON (t2.c NOTNULL) JOIN t2 ON 1 RIGHT JOIN t3 ON 1`,
	}
	oracleRes := run(t, "cgo", stmts)
	musqlRes := run(t, "musql", stmts)
	oracleErr := oracleRes[len(stmts)-1]["kind"] == "error"
	musqlErr := musqlRes[len(stmts)-1]["kind"] == "error"
	if oracleErr != musqlErr {
		t.Fatalf("agreement mismatch: oracle error=%v (%v) musql error=%v (%v)",
			oracleErr, oracleRes[len(stmts)-1], musqlErr, musqlRes[len(stmts)-1])
	}
}

// Multiple plain INNER joins, each with its OWN ON clause referencing a
// DIFFERENT rightward alias, all in the same statement -- checks that the
// promotion loop in markWherePlanEligibility correctly handles more than one
// forward reference simultaneously (not just the single-reference case the
// implementer's own regression covers).
func TestReviewMultipleInnerJoinsEachWithOwnRightwardRef(t *testing.T) {
	differ(t, "review_multi_inner_rightward", []string{
		`CREATE TABLE t0(a INTEGER)`,
		`CREATE TABLE t1(b INTEGER)`,
		`CREATE TABLE t2(c INTEGER)`,
		`CREATE TABLE t3(d INTEGER)`,
		`CREATE TABLE t4(e INTEGER)`,
		`INSERT INTO t0 VALUES(1),(2)`,
		`INSERT INTO t1 VALUES(1),(3)`,
		`INSERT INTO t2 VALUES(1)`,
		`INSERT INTO t3 VALUES(2)`,
		`INSERT INTO t4 VALUES(1),(2)`,
		// t1's ON references t2 (2 hops right); t3's ON references t4 (1 hop
		// right) -- two independent forward references in one FROM clause.
		`SELECT t0.a,t1.b,t2.c,t3.d,t4.e FROM t0
		 JOIN t1 ON (t2.c IS NOT NULL AND t0.a=t1.b)
		 JOIN t2 ON t2.c=t0.a
		 JOIN t3 ON (t4.e IS NOT NULL)
		 JOIN t4 ON t4.e=t3.d
		 ORDER BY 1,2,3,4,5`,
	})
}

// An ON clause that references a rightward alias AND ALSO independently
// trips a genuine, unrelated decline (a correlated scalar subquery in the ON
// clause, which this port's compiler declines regardless of ON-scoping).
// Confirms it still declines for THAT reason -- an error, not a silent
// misresolution -- rather than the promotion accidentally making it "work"
// with a wrong answer.
func TestReviewRightwardOnAlsoHasUnrelatedDeclineReason(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t0(a INTEGER)`, `CREATE TABLE t1(b INTEGER)`, `CREATE TABLE t2(c INTEGER)`,
		`INSERT INTO t0 VALUES(1)`, `INSERT INTO t1 VALUES(1)`, `INSERT INTO t2 VALUES(1)`,
		// ON references t2 (rightward) AND contains a correlated subquery
		// referencing the outer t0 -- if musql answers rows here where the
		// oracle also answers rows, we must check the ROW VALUES match, not
		// just presence/absence of error.
		`SELECT * FROM t0 JOIN t1 ON (t2.c NOTNULL AND (SELECT count(*) FROM t2 WHERE t2.c=t0.a)>0) JOIN t2 ON 1`,
	}
	oracleRes := run(t, "cgo", stmts)
	last := oracleRes[len(stmts)-1]
	if last["kind"] == "error" {
		musqlRes := run(t, "musql", stmts)
		if musqlRes[len(stmts)-1]["kind"] != "error" {
			t.Fatalf("oracle declined (%v) but musql did NOT decline: %v", last, musqlRes[len(stmts)-1])
		}
		return
	}
	// If the oracle actually accepted it, the two engines' full output must
	// match byte for byte -- this is the "WRONG VALUE" tripwire.
	differ(t, "review_rightward_on_with_correlated_subquery", stmts)
}

// The exact mined distinct2.test statement (5-way self-join CTAS), re-typed
// independently from the task description rather than copied from the
// implementer's own test file, plus a simplified 2-table reduction for
// easier debugging.
func TestReviewDistinct2MinedStatementIndependent(t *testing.T) {
	differ(t, "review_distinct2_mined", []string{
		`CREATE TABLE t102 (i0 TEXT UNIQUE NOT NULL)`,
		`INSERT INTO t102 VALUES ('0'),('1'),('2')`,
		`CREATE TABLE t2 AS
		 SELECT DISTINCT *
		 FROM t102 AS t0
		 JOIN t102 AS t4 ON (t2.i0 IN t102)
		 NATURAL JOIN t102 AS t3
		 JOIN t102 AS t1 ON (t0.i0 IN t102)
		 JOIN t102 AS t2 ON (t2.i0=+t0.i0 OR (t0.i0<>500 AND t2.i0=t1.i0))`,
		`SELECT *, '|' FROM t2 ORDER BY 1,2,3,4,5`,
	})
}

// Simplified 2-3 table version of the same forward-reference-plus-range-
// rewrite shape, for isolating the mechanism without the 5-way self-join
// noise.
func TestReviewDistinct2SimplifiedTwoTable(t *testing.T) {
	differ(t, "review_distinct2_simplified", []string{
		`CREATE TABLE tt (i0 TEXT UNIQUE NOT NULL)`,
		`INSERT INTO tt VALUES ('0'),('1'),('2')`,
		// t4's ON is a bare-table IN ("x IN tbl"), which trips
		// wherePlanHasRangeRewrite and makes the ported solver decline; its ON
		// also references t2, introduced by the THIRD join term.
		`SELECT t0.i0,t4.i0,t2.i0 FROM tt AS t0
		 JOIN tt AS t4 ON (t2.i0 IN tt)
		 JOIN tt AS t2 ON (t2.i0=t0.i0)
		 ORDER BY 1,2,3`,
	})
}
