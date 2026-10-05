// Tests that nested view chains correctly count and memoize view references.
// A view's column facts are computed once per view and reused, avoiding
// redundant execution and reference over-counting.
package compat

import "testing"

// TestView3DoublingChainRefcount verifies that a deeply doubled view chain
// is rejected when it exceeds the reference count limit.
func TestView3DoublingChainRefcount(t *testing.T) {
	differ(t, "view3_doubling_chain_refcount", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(5)`,
		`CREATE VIEW v1 AS SELECT x*2 FROM t1`,
		`CREATE VIEW v2 AS SELECT * FROM v1 UNION SELECT * FROM v1`,
		`CREATE VIEW v4 AS SELECT * FROM v2 UNION SELECT * FROM v2`,
		`CREATE VIEW v8 AS SELECT * FROM v4 UNION SELECT * FROM v4`,
		`CREATE VIEW v16 AS SELECT * FROM v8 UNION SELECT * FROM v8`,
		`CREATE VIEW v32 AS SELECT * FROM v16 UNION SELECT * FROM v16`,
		`CREATE VIEW v64 AS SELECT * FROM v32 UNION SELECT * FROM v32`,
		`CREATE VIEW v128 AS SELECT * FROM v64 UNION SELECT * FROM v64`,
		`CREATE VIEW v256 AS SELECT * FROM v128 UNION SELECT * FROM v128`,
		`CREATE VIEW v512 AS SELECT * FROM v256 UNION SELECT * FROM v256`,
		`CREATE VIEW v1024 AS SELECT * FROM v512 UNION SELECT * FROM v512`,
		`CREATE VIEW v2048 AS SELECT * FROM v1024 UNION SELECT * FROM v1024`,
		`CREATE VIEW v4096 AS SELECT * FROM v2048 UNION SELECT * FROM v2048`,
		`CREATE VIEW v8192 AS SELECT * FROM v4096 UNION SELECT * FROM v4096`,
		`CREATE VIEW v16384 AS SELECT * FROM v8192 UNION SELECT * FROM v8192`,
		`CREATE VIEW v32768 AS SELECT * FROM v16384 UNION SELECT * FROM v16384`,
		`SELECT * FROM v32768 UNION SELECT * FROM v32768`,
	})
}

// TestView3ModerateChainNowAnswers verifies that a single reference to a
// moderately deep view chain succeeds.
func TestView3ModerateChainNowAnswers(t *testing.T) {
	differ(t, "view3_moderate_chain_answers", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(5)`,
		`CREATE VIEW v1 AS SELECT x*2 FROM t1`,
		`CREATE VIEW v2 AS SELECT * FROM v1 UNION SELECT * FROM v1`,
		`CREATE VIEW v4 AS SELECT * FROM v2 UNION SELECT * FROM v2`,
		`CREATE VIEW v8 AS SELECT * FROM v4 UNION SELECT * FROM v4`,
		`CREATE VIEW v16 AS SELECT * FROM v8 UNION SELECT * FROM v8`,
		`CREATE VIEW v32 AS SELECT * FROM v16 UNION SELECT * FROM v16`,
		`CREATE VIEW v64 AS SELECT * FROM v32 UNION SELECT * FROM v32`,
		`CREATE VIEW v128 AS SELECT * FROM v64 UNION SELECT * FROM v64`,
		`CREATE VIEW v256 AS SELECT * FROM v128 UNION SELECT * FROM v128`,
		`CREATE VIEW v512 AS SELECT * FROM v256 UNION SELECT * FROM v256`,
		`CREATE VIEW v1024 AS SELECT * FROM v512 UNION SELECT * FROM v512`,
		`SELECT * FROM v1024`,
		`SELECT count(*) FROM v1024`,
	})
}

// TestView3FullDepthChainSingleReference verifies that a single reference to
// a fully deep view chain (32768 references) is accepted.
func TestView3FullDepthChainSingleReference(t *testing.T) {
	differ(t, "view3_full_depth_chain_single_reference", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(5)`,
		`CREATE VIEW v1 AS SELECT x*2 FROM t1`,
		`CREATE VIEW v2 AS SELECT * FROM v1 UNION SELECT * FROM v1`,
		`CREATE VIEW v4 AS SELECT * FROM v2 UNION SELECT * FROM v2`,
		`CREATE VIEW v8 AS SELECT * FROM v4 UNION SELECT * FROM v4`,
		`CREATE VIEW v16 AS SELECT * FROM v8 UNION SELECT * FROM v8`,
		`CREATE VIEW v32 AS SELECT * FROM v16 UNION SELECT * FROM v16`,
		`CREATE VIEW v64 AS SELECT * FROM v32 UNION SELECT * FROM v32`,
		`CREATE VIEW v128 AS SELECT * FROM v64 UNION SELECT * FROM v64`,
		`CREATE VIEW v256 AS SELECT * FROM v128 UNION SELECT * FROM v128`,
		`CREATE VIEW v512 AS SELECT * FROM v256 UNION SELECT * FROM v256`,
		`CREATE VIEW v1024 AS SELECT * FROM v512 UNION SELECT * FROM v512`,
		`CREATE VIEW v2048 AS SELECT * FROM v1024 UNION SELECT * FROM v1024`,
		`CREATE VIEW v4096 AS SELECT * FROM v2048 UNION SELECT * FROM v2048`,
		`CREATE VIEW v8192 AS SELECT * FROM v4096 UNION SELECT * FROM v4096`,
		`CREATE VIEW v16384 AS SELECT * FROM v8192 UNION SELECT * FROM v8192`,
		`CREATE VIEW v32768 AS SELECT * FROM v16384 UNION SELECT * FROM v16384`,
		`SELECT * FROM v32768`,
	})
}

// view3FullChainDDL is the base schema for the full depth chain tests.
var view3FullChainDDL = []string{
	`CREATE TABLE t1(x)`,
	`INSERT INTO t1 VALUES(5)`,
	`CREATE VIEW v1 AS SELECT x*2 FROM t1`,
	`CREATE VIEW v2 AS SELECT * FROM v1 UNION SELECT * FROM v1`,
	`CREATE VIEW v4 AS SELECT * FROM v2 UNION SELECT * FROM v2`,
	`CREATE VIEW v8 AS SELECT * FROM v4 UNION SELECT * FROM v4`,
	`CREATE VIEW v16 AS SELECT * FROM v8 UNION SELECT * FROM v8`,
	`CREATE VIEW v32 AS SELECT * FROM v16 UNION SELECT * FROM v16`,
	`CREATE VIEW v64 AS SELECT * FROM v32 UNION SELECT * FROM v32`,
	`CREATE VIEW v128 AS SELECT * FROM v64 UNION SELECT * FROM v64`,
	`CREATE VIEW v256 AS SELECT * FROM v128 UNION SELECT * FROM v128`,
	`CREATE VIEW v512 AS SELECT * FROM v256 UNION SELECT * FROM v256`,
	`CREATE VIEW v1024 AS SELECT * FROM v512 UNION SELECT * FROM v512`,
	`CREATE VIEW v2048 AS SELECT * FROM v1024 UNION SELECT * FROM v1024`,
	`CREATE VIEW v4096 AS SELECT * FROM v2048 UNION SELECT * FROM v2048`,
	`CREATE VIEW v8192 AS SELECT * FROM v4096 UNION SELECT * FROM v4096`,
	`CREATE VIEW v16384 AS SELECT * FROM v8192 UNION SELECT * FROM v8192`,
	`CREATE VIEW v32768 AS SELECT * FROM v16384 UNION SELECT * FROM v16384`,
}

// TestView3OtherShapesAgreeAtFullDepth verifies that various query shapes
// work correctly on fully deep chains.
func TestView3OtherShapesAgreeAtFullDepth(t *testing.T) {
	for _, shape := range []string{
		`SELECT count(*) FROM v32768`,
		`SELECT * FROM v32768 WHERE "x*2" > 5`,
	} {
		stmts := append(append([]string{}, view3FullChainDDL...), shape)
		differ(t, "view3_full_depth_shape", stmts)
	}
}

// TestView3TwoDistinctChainsNoCacheAliasing verifies that two unrelated view
// chains cannot be confused by the memoization cache.
func TestView3TwoDistinctChainsNoCacheAliasing(t *testing.T) {
	differ(t, "view3_two_distinct_chains", []string{
		`CREATE TABLE ta(x)`,
		`INSERT INTO ta VALUES(3)`,
		`CREATE VIEW a1 AS SELECT x*2 FROM ta`,
		`CREATE VIEW a2 AS SELECT * FROM a1 UNION SELECT * FROM a1`,
		`CREATE VIEW a4 AS SELECT * FROM a2 UNION SELECT * FROM a2`,
		`CREATE VIEW a8 AS SELECT * FROM a4 UNION SELECT * FROM a4`,
		`CREATE TABLE tb(y)`,
		`INSERT INTO tb VALUES(100)`,
		`CREATE VIEW b1 AS SELECT y+1 FROM tb`,
		`CREATE VIEW b2 AS SELECT * FROM b1 UNION SELECT * FROM b1`,
		`CREATE VIEW b4 AS SELECT * FROM b2 UNION SELECT * FROM b2`,
		`CREATE VIEW b8 AS SELECT * FROM b4 UNION SELECT * FROM b4`,
		`SELECT * FROM a8, b8`,
		`SELECT (SELECT * FROM a8) AS av, (SELECT * FROM b8) AS bv`,
	})
}

// TestView3TempShadowsMainNoCacheAliasing verifies that TEMP and MAIN views
// with the same name are distinct in the cache.
func TestView3TempShadowsMainNoCacheAliasing(t *testing.T) {
	differ(t, "view3_temp_shadows_main", []string{
		`CREATE VIEW v1 AS SELECT 111 AS n`,
		`CREATE TEMP VIEW v1 AS SELECT 222 AS n`,
		`CREATE VIEW wrap_main AS SELECT * FROM main.v1`,
		`CREATE VIEW wrap_temp AS SELECT * FROM v1`,
		`SELECT * FROM wrap_main, wrap_temp`,
		`SELECT (SELECT * FROM wrap_main) AS m, (SELECT * FROM wrap_temp) AS t`,
	})
}
