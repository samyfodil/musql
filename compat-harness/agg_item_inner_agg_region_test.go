// This file tests that aggregate queries can read outer aggregate values
// from inside inner aggregate subqueries.
package compat

import "testing"

// TestAggItemInnerAggRegion tests reading outer aggregate values from within
// inner aggregate queries.
func TestAggItemInnerAggRegion(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k INTEGER, v INTEGER)`,
		`INSERT INTO t VALUES(1,10),(1,20),(2,30),(2,40),(3,50)`,
		`CREATE TABLE u(k INTEGER, w INTEGER)`,
		`INSERT INTO u VALUES(1,7),(2,8),(2,9),(3,10)`,
	}
	flLockstep(t, "inner-agg-region", setup,
		`SELECT k, count(*), (SELECT count(*) FROM u GROUP BY u.k HAVING u.k = t.k) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT max(u.w + t.v) FROM u GROUP BY u.k ORDER BY 1 LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT u.w FROM u GROUP BY u.k ORDER BY abs(u.k - t.k) LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT t.k, avg(t.v) AS avg1 FROM t GROUP BY t.k HAVING NOT EXISTS(SELECT u.k, avg(u.w) AS avg2 FROM u GROUP BY u.k HAVING avg1 > avg2) ORDER BY 1`,
		`SELECT t.k, avg(t.v) FROM t GROUP BY t.k HAVING NOT EXISTS(SELECT u.k, avg(u.w) FROM u GROUP BY u.k HAVING avg(t.v) > avg(u.w)) ORDER BY 1`,
		`SELECT k, (SELECT (SELECT count(*) FROM u AS z GROUP BY z.k HAVING z.k = t.k) FROM u GROUP BY u.k LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT count(*) FROM u GROUP BY u.k HAVING u.k <= t.rowid) FROM t GROUP BY k ORDER BY k`,
	)
}

// TestAggItemAggArgRegion tests aggregate function arguments with outer aggregate references.
func TestAggItemAggArgRegion(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(id1 INTEGER PRIMARY KEY, value1 INTEGER)`,
		`INSERT INTO t1 VALUES(4469,2),(4476,1)`,
		`CREATE TABLE t2(id2 INTEGER PRIMARY KEY, value2 INTEGER)`,
		`INSERT INTO t2 VALUES(0,1),(2,2)`,
	}
	flLockstep(t, "agg-arg-region", setup,
		`SELECT max(value1), (SELECT sum(value2=value1) FROM t2) FROM t1 GROUP BY id1 ORDER BY 1`,
		`SELECT (SELECT sum(value2=value1) FROM t2), max(value1) FROM t1 GROUP BY id1 ORDER BY 1`,
		`SELECT max(t1.value1), (SELECT sum(value2=t1.value1) FROM t2) FROM t1 GROUP BY id1 ORDER BY 1`,
		`SELECT id1, max(value1) FROM t1 GROUP BY id1 HAVING (SELECT sum(value2=max(value1)) FROM t2) > 0 ORDER BY 1`,
		`SELECT max(value1), (SELECT group_concat(value2, CAST(value1 AS TEXT)) FROM t2) FROM t1 GROUP BY id1 ORDER BY 1`,
		`SELECT max(value1), (SELECT count(*) FILTER (WHERE value2 = value1) FROM t2) FROM t1 GROUP BY id1 ORDER BY 1`,
		`SELECT id1, (SELECT max(value2 + t1.value1) FROM t2 GROUP BY id2 ORDER BY 1 LIMIT 1) FROM t1 GROUP BY id1 ORDER BY 1`,
		`SELECT id1, (SELECT max(value2 + value1) FROM t2 GROUP BY id2 ORDER BY 1 LIMIT 1) FROM t1 GROUP BY id1 ORDER BY 1`,
	)
}

// TestAggItemCompoundArmScope tests compound queries with unscoped terms that reference outer aggregates.
func TestAggItemCompoundArmScope(t *testing.T) {
	setup := []string{
		`CREATE TABLE abc(a INTEGER, c INTEGER)`,
		`INSERT INTO abc VALUES(1,10),(2,10),(3,20)`,
		`CREATE TABLE t2(a INTEGER)`,
		`INSERT INTO t2 VALUES(1),(2),(2),(3)`,
	}
	flLockstep(t, "compound-arm-scope", setup,
		`SELECT c FROM abc GROUP BY c HAVING EXISTS (SELECT a UNION SELECT 123) ORDER BY 1`,
		`SELECT (SELECT avg(a) UNION SELECT min(a) OVER ()) FROM t2 GROUP BY a ORDER BY 1`,
		`SELECT c, (SELECT 0 UNION ALL SELECT a ORDER BY 1 DESC LIMIT 1) FROM abc GROUP BY c ORDER BY 1`,
	)
}
