package compat

import "testing"

// Tests connector view cases in various JOIN and GROUP contexts.
// own group_connector_view_test.go convention) to isolate genuine ROW-VALUE
// correctness from the separate, pre-existing ":N" duplicate-column-name
// disambiguation gap (sqlite3ColumnsFromExprList, select.c:2295-2307) this
// package already deliberately serves without for an ORDINARY (non-view)
// group connector too -- confirmed identical for a plain base-table
// connector during this review, so it is not something this bucket's view
// support changes.

func TestReviewA_ViewConnectorExprWhereCoercion(t *testing.T) {
	differ(t, "review_a", []string{
		"CREATE TABLE t0(c0 INTEGER, c1 TEXT)",
		"CREATE TABLE t1(k INTEGER, v TEXT)",
		"INSERT INTO t0 VALUES(1,'1'),(2,'2'),(3,'x')",
		"INSERT INTO t1 VALUES(1,'one'),(2,'two')",
		"CREATE VIEW rva(kk, label) AS SELECT k+0, upper(v) FROM t1 WHERE k > 0",
		"SELECT * FROM t0 JOIN (rva CROSS JOIN t0 AS t0b) ON rva.kk = t0.c0",
	})
}

func TestReviewB_TempViewConnector(t *testing.T) {
	differ(t, "review_b", []string{
		"CREATE TABLE t2(w INTEGER)",
		"INSERT INTO t2 VALUES(7),(9)",
		"CREATE TEMP VIEW rvb(rvb_a) AS SELECT w FROM t2",
		"CREATE TABLE t3(a INTEGER, z TEXT)",
		"INSERT INTO t3 VALUES(7,'m'),(11,'n')",
		"SELECT * FROM t3 JOIN (rvb CROSS JOIN t3 AS t3b) ON rvb.rvb_a = t3.a",
	})
}

func TestReviewC_SchemaQualifiedViewConnector(t *testing.T) {
	differ(t, "review_c", []string{
		"ATTACH ':memory:' AS aux1",
		"CREATE TABLE aux1.t9(w INTEGER)",
		"INSERT INTO aux1.t9 VALUES(5),(6)",
		"CREATE VIEW aux1.rvc(rvc_a) AS SELECT w FROM t9",
		"CREATE TABLE t10(a INTEGER, z TEXT)",
		"INSERT INTO t10 VALUES(5,'p'),(8,'q')",
		"SELECT * FROM t10 JOIN (aux1.rvc CROSS JOIN t10 AS t10b) ON aux1.rvc.rvc_a = t10.a",
	})
}

func TestReviewD_ViewConnectorGroupOnLeftJoinRightSide(t *testing.T) {
	differ(t, "review_d", []string{
		"CREATE TABLE t4(a INTEGER)",
		"INSERT INTO t4 VALUES(1),(2),(3)",
		"CREATE TABLE t5(w INTEGER)",
		"INSERT INTO t5 VALUES(2)",
		"CREATE VIEW rvd(rvd_a) AS SELECT w FROM t5",
		"SELECT * FROM t4 LEFT JOIN (rvd CROSS JOIN t4 AS t4b) ON rvd.rvd_a = t4.a",
	})
}

func TestReviewE_NestedGroupInnerViewConnector(t *testing.T) {
	differ(t, "review_e", []string{
		"CREATE TABLE t6(a INTEGER)",
		"INSERT INTO t6 VALUES(1),(2)",
		"CREATE TABLE t7(w INTEGER)",
		"INSERT INTO t7 VALUES(2),(3)",
		"CREATE VIEW rve(rve_a) AS SELECT w FROM t7",
		"CREATE TABLE t8(t8_a INTEGER, z TEXT)",
		"INSERT INTO t8 VALUES(2,'m')",
		"CREATE TABLE t6c(t6c_a INTEGER)",
		"INSERT INTO t6c VALUES(1),(2)",
		"SELECT * FROM t6 JOIN ((rve JOIN t8 ON rve.rve_a = t8.t8_a) CROSS JOIN t6c) ON rve.rve_a = t6.a",
	})
}

// TestReviewF_ViewConnectorGroupByAggregate: GROUP BY over a parenthesized
// join group is declined REGARDLESS of connector kind -- confirmed via a
// direct probe with an ordinary base-table connector in the identical
// shape, same decline text ("vdbe: unsupported: GROUP BY over a
// parenthesized join group"). Not a view-connector-specific gap, and not
// exercised by differ() (which requires agreement, not decline) -- this is
// intentionally an ACCEPTANCE-only smoke check that the decline still fires
// cleanly (errVDBEUnsupported, not a crash/wrong value) for a view connector
// specifically, since resolveGroupSource's view branch is new code on this
// path.
func TestReviewF_ViewConnectorGroupByAggregateDeclinesCleanly(t *testing.T) {
	differAllowingDeclines(t, "review_f", []string{
		"CREATE TABLE t11(g TEXT, v INTEGER)",
		"INSERT INTO t11 VALUES('a',1),('a',2),('b',3)",
		"CREATE VIEW rvf(g2) AS SELECT g FROM t11",
		"CREATE TABLE t12(g2 TEXT)",
		"INSERT INTO t12 VALUES('a'),('b'),('b')",
		"SELECT rvf.g2, count(*) FROM t12 JOIN (rvf CROSS JOIN t12 AS t12b) ON rvf.g2 = t12.g2 GROUP BY rvf.g2 ORDER BY rvf.g2",
	})
}

func TestReviewG_ViewConnectorBodyWithSubquery(t *testing.T) {
	differ(t, "review_g", []string{
		"CREATE TABLE t13(a INTEGER)",
		"INSERT INTO t13 VALUES(1),(2),(3)",
		"CREATE TABLE t14(a INTEGER)",
		"INSERT INTO t14 VALUES(2),(3)",
		"CREATE VIEW rvg(rvg_a) AS SELECT a FROM t13 WHERE a IN (SELECT a FROM t14)",
		"CREATE TABLE t15(a INTEGER, z TEXT)",
		"INSERT INTO t15 VALUES(2,'x'),(3,'y'),(9,'z')",
		"SELECT * FROM t15 JOIN (rvg CROSS JOIN t15 AS t15b) ON rvg.rvg_a = t15.a",
	})
}

func TestReviewH_ViewConnectorMixedAffinityOrderBy(t *testing.T) {
	differ(t, "review_h", []string{
		"CREATE TABLE t16(x)",
		"INSERT INTO t16 VALUES(1), ('2'), (3.5), (NULL), (x'ab')",
		"CREATE VIEW rvh(x) AS SELECT x FROM t16",
		"CREATE TABLE t17(y INTEGER)",
		"INSERT INTO t17 VALUES(1)",
		"SELECT * FROM t17 JOIN (rvh CROSS JOIN t17 AS t17b) ON 1 ORDER BY rvh.x",
	})
}
