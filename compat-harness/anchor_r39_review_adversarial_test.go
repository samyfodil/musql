package compat

import "testing"

// Adversarial coverage for aggregate order and anchor in nested queries.
// Tests shapes exercised in scalar subqueries and compound queries that verify
// aggregate order detection does not incorrectly trust index plans.
func TestR39ReviewAdversarial(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// --- withFromPlanTrust: compileScanAggregate's own 3 call sites ---
		//
		// Each of these embeds the target whole-table-aggregate/GROUP BY query
		// as a SELECT-LIST scalar-subquery expression, itself nested inside
		// ANOTHER select-list scalar subquery, at a plain top-level statement
		// (no VIEW). This matters mechanically: a DERIVED TABLE in FROM goes
		// through derivedOuterBarrier (vdbe_join_codegen.go), which returns a
		// NIL barrier -- reproducing an unnested outer==nil compile -- whenever
		// its own caller c has c.outer==nil AND c.rowOuter==nil, i.e. whenever
		// the derived table sits directly in a TRUE top-level FROM clause. A
		// scalar-subquery EXPRESSION has no such barrier -- compileSubProgram
		// is threaded c unconditionally -- so ONE layer of scalar-subquery
		// nesting around a plain top-level statement is not enough to reach
		// c.outer!=nil for a FROM-item at all; it takes two (confirmed by
		// mutation below: a single-layer derived-table-in-FROM variant of each
		// of these cases passed even with withFromPlanTrust's restore
		// temporarily neutered, proving it never exercised the nested path).
		//
		// Each also carries a "WHERE a > ''" term on the inner aggregate: with
		// no WHERE at all, wherePlanIndexesProvablyInert (where_plan_gate.go)
		// ALREADY proves t4x cannot produce a covering-scan loop worth
		// considering here regardless of withFromPlanTrust, so anchorPlanOrder
		// Provable's fallback path alone would still serve it correctly --
		// mutation-CONFIRMED to make no observable difference (verified by
		// hand: printf-instrumenting anchorPlanOrderProvable showed
		// wherePlanIndexesProvablyInert==true independent of the mutation for
		// the no-WHERE form). A range term on the indexed column is what makes
		// the index genuinely non-inert, forcing the actual cost-based
		// wherePlanIndexOrderDecided verdict -- confirmed by hand, with the
		// SAME instrumentation, that this form flips from
		// wherePlanIndexOrderDecided=true/served under the real fix to
		// wherePlanIndexesProvablyInert=false/DECLINED under a
		// withFromPlanTrust that no-ops its restore.
		{
			"agg-anchor-bare-column-no-view",
			[]string{
				`CREATE TABLE t4(a TEXT, b INT)`,
				`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2)`,
				`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
				`SELECT (SELECT a FROM (SELECT count(*) AS n, a FROM t4 WHERE a > '')) AS anchor`,
			},
		},
		{
			"agg-order-sensitive-groupconcat-no-view",
			[]string{
				`CREATE TABLE t4(a TEXT, b INT)`,
				`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2)`,
				`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
				// A whole-table aggregate (no GROUP BY) with an order-sensitive
				// aggregate and NO explicit ORDER BY inside the aggregate call --
				// aggPlanOrderProvable's arm, via compileScanAggregate's second
				// withFromPlanTrust site.
				`SELECT (SELECT s FROM (SELECT group_concat(a) AS s FROM t4 WHERE a > '')) AS anchor`,
			},
		},
		{
			"agg-having-order-sensitive-no-view",
			[]string{
				`CREATE TABLE t4(a TEXT, b INT)`,
				`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2)`,
				`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
				// HAVING carries its own order-sensitive aggregate distinct from
				// the select list's -- compileScanAggregate's THIRD
				// withFromPlanTrust site (hp.aggTemplates).
				`SELECT (SELECT n FROM (SELECT count(*) AS n FROM t4 WHERE a > '' HAVING group_concat(a) IS NOT NULL)) AS anchor`,
			},
		},
		// --- withFromPlanTrust: compileScanGroupBy's remaining 2 sites ---
		{
			"groupby-order-sensitive-aggregate-no-view",
			[]string{
				`CREATE TABLE t4(a TEXT, b INT)`,
				`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2),('abc',3)`,
				`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
				// GROUP BY b (not a -- so the anchor-bare-column check does not
				// fire), with group_concat(a) order-sensitive and no ORDER BY:
				// exercises compileScanGroupBy's OWN order-sensitive-aggregate
				// withFromPlanTrust call independent of the anchor-read one.
				`SELECT (SELECT group_concat(a) FROM (SELECT * FROM t4 WHERE a > '') GROUP BY b LIMIT 1) AS anchor`,
			},
		},
		{
			"groupby-emission-order-no-bare-column",
			[]string{
				`CREATE TABLE t4(a TEXT, b INT)`,
				`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2),('abc',3)`,
				`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
				// GROUP BY a with no ORDER BY and no bare-column/subquery anchor
				// read at all (every select item is either the key or a proper
				// aggregate) -- exercises ONLY groupEmissionOrderProvable's own
				// withFromPlanTrust call, nested one level.
				`SELECT (SELECT a FROM (SELECT * FROM t4) GROUP BY a LIMIT 1) AS anchor`,
			},
		},
		// --- withFromPlanTrust: nesting depth 2 (VIEW-based) ---
		{
			"double-nested-groupby-two-levels",
			[]string{
				`CREATE TABLE t4(a TEXT, b INT)`,
				`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2)`,
				`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
				`CREATE VIEW t5 AS SELECT 1 AS b WHERE (SELECT a FROM (SELECT count(*) n, a FROM t4))`,
				`SELECT * FROM t5`,
			},
		},
		// --- fromlessArmOrderProvable: dedup, INTERSECT/EXCEPT, mixed arms ---
		{
			"compound-union-dedup-collision-with-join",
			[]string{
				`CREATE TABLE t2(c PRIMARY KEY, v)`,
				`INSERT INTO t2 VALUES(1,10),(2,20)`,
				// Two arms collide under plain UNION's dedup (1 appears twice);
				// GROUP BY reads a bare column from the REAL (indexed) side too,
				// so the anchor spans BOTH the compound's dedup order and t2's
				// real index.
				`SELECT * FROM (SELECT 1 AS x UNION SELECT 2 UNION SELECT 1 UNION SELECT 2)
				   LEFT JOIN (SELECT c, v AS y FROM t2) ON 1=1
				  GROUP BY x`,
			},
		},
		{
			"compound-intersect-except-fromless",
			[]string{
				`CREATE TABLE t2(c PRIMARY KEY, v)`,
				`INSERT INTO t2 VALUES(1,10),(2,20),(3,30)`,
				// INTERSECT/EXCEPT, not UNION/UNION ALL -- restriction (17a) means
				// C SQLite's flattenSubquery never even considers folding this
				// into the outer plan (it only flattens UNION ALL compounds), so
				// this must be immune to t2's index for an independent reason
				// (the isolated-execution argument) than the flattening
				// restriction itself.
				`SELECT * FROM (SELECT 1 AS x UNION SELECT 2 UNION SELECT 3
				                EXCEPT SELECT 2
				                INTERSECT SELECT 1 UNION ALL SELECT 3)
				   LEFT JOIN (SELECT c, v AS y FROM t2) ON 1=1
				  GROUP BY x`,
			},
		},
		// compound-mixed-fromless-and-real-arm is handled separately, below,
		// via differAllowingDeclines: musql declines it both before and
		// after this change (confirmed by reverting engine/vdbe_agg_codegen.go
		// and engine/vdbe_join_codegen.go to HEAD~1 and re-running this exact
		// case), so it is a PRE-EXISTING gap, not something this fix should
		// have widened or could have regressed -- fromlessArmOrderProvable
		// correctly refuses to mark a MIXED compound (one FROM-less arm, one
		// real-table arm) provable, only a uniformly FROM-less compound
		// qualifies.
		{
			"compound-fromless-with-where-guard",
			[]string{
				`CREATE TABLE t2(c PRIMARY KEY, v)`,
				`INSERT INTO t2 VALUES(1,10),(2,20)`,
				// Each arm keeps its own (constant-only) WHERE guard -- still
				// FROM-less per arm, so still immune, but exercises
				// fromlessArmOrderProvable against a non-trivial arm body rather
				// than a bare "SELECT <const>".
				`SELECT * FROM (SELECT 111 AS x WHERE 1=1 UNION ALL SELECT 222 WHERE 2>1)
				   LEFT JOIN (SELECT c, v AS y FROM t2) ON 1=1
				  GROUP BY x`,
			},
		},
		{
			"compound-fromless-four-arms-aggregate-outer",
			[]string{
				`CREATE TABLE t3(c PRIMARY KEY, v)`,
				`INSERT INTO t3 VALUES(1,7),(2,8),(3,9)`,
				// A whole-table aggregate (no GROUP BY) over the join, reading a
				// bare column from the compound side -- compileScanAggregate's
				// anchor check (not compileScanGroupBy's) combined with fix #2.
				`SELECT count(*), x FROM (SELECT 1 AS x UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4)
				   LEFT JOIN (SELECT c, v AS y FROM t3) ON 1=1`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}

	t.Run("compound-mixed-fromless-and-real-arm", func(t *testing.T) {
		differAllowingDeclines(t, "compound-mixed-fromless-and-real-arm", []string{
			`CREATE TABLE t1(c PRIMARY KEY, v)`,
			`INSERT INTO t1 VALUES(5,50),(6,60)`,
			`CREATE TABLE t2(c PRIMARY KEY, v)`,
			`INSERT INTO t2 VALUES(1,10),(2,20)`,
			`SELECT * FROM (SELECT 111 AS x UNION ALL SELECT c FROM t1)
			   LEFT JOIN (SELECT c, v AS y FROM t2) ON 1=1
			  GROUP BY x`,
		})
	})
}
