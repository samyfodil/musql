// This file gates join groups whose leading member is a VIEW instead of a table.
package compat

import "testing"

func TestGroupConnectorView(t *testing.T) {
	differ(t, "group_connector_view", []string{
		`CREATE TABLE t0(c0, c1)`,
		`CREATE TABLE t1(x)`,
		`CREATE VIEW v1(vc) AS SELECT x FROM t1`,
		`CREATE VIEW v2(wc) AS SELECT vc FROM v1`,
		`INSERT INTO t0 VALUES(1,2)`,
		`INSERT INTO t0 VALUES(5,1)`,
		`INSERT INTO t1 VALUES(9)`,
		`SELECT * FROM v1 INNER JOIN (v2 CROSS JOIN t0) ON (t0.c0 < t0.c1)`,
	})
}

// TestGroupConnectorViewAliased checks aliased view connectors resolve identically.
func TestGroupConnectorViewAliased(t *testing.T) {
	differ(t, "group_connector_view_aliased", []string{
		`CREATE TABLE t0(c0, c1)`,
		`CREATE TABLE t2(w)`,
		`CREATE VIEW v3(vc) AS SELECT w FROM t2`,
		`INSERT INTO t0 VALUES(1,2)`,
		`INSERT INTO t0 VALUES(5,1)`,
		`INSERT INTO t2 VALUES(7)`,
		`SELECT * FROM t0 JOIN (v3 AS vv CROSS JOIN t0 AS t0b) ON vv.vc < t0.c1`,
	})
}

// TestGroupConnectorViewInternalUsing checks view connectors with USING joins.
func TestGroupConnectorViewInternalUsing(t *testing.T) {
	differ(t, "group_connector_view_internal_using", []string{
		`CREATE TABLE t2(w)`,
		`CREATE VIEW v4(a) AS SELECT w FROM t2`,
		`CREATE TABLE t3(a, z)`,
		`INSERT INTO t2 VALUES(7)`,
		`INSERT INTO t3 VALUES(7,9)`,
		`INSERT INTO t3 VALUES(11,13)`,
		`SELECT * FROM (v4 RIGHT JOIN t3 USING(a))`,
	})
}

// TestGroupConnectorViewInternalNaturalLeft checks view connectors with NATURAL LEFT JOINs.
func TestGroupConnectorViewInternalNaturalLeft(t *testing.T) {
	differ(t, "group_connector_view_internal_natural_left", []string{
		`CREATE TABLE t2(w)`,
		`CREATE VIEW v5(a) AS SELECT w FROM t2`,
		`CREATE TABLE t3(a, z)`,
		`INSERT INTO t2 VALUES(7)`,
		`INSERT INTO t3 VALUES(7,9)`,
		`INSERT INTO t3 VALUES(11,13)`,
		`SELECT * FROM (v5 NATURAL LEFT JOIN t3)`,
	})
}

// TestGroupConnectorViewIndexedByDeclines checks INDEXED BY fails for views.
func TestGroupConnectorViewIndexedByDeclines(t *testing.T) {
	differ(t, "group_connector_view_indexedby_declines", []string{
		`CREATE TABLE t0(a, c1)`,
		`CREATE TABLE t2(w)`,
		`CREATE INDEX i_t0 ON t0(a)`,
		`CREATE VIEW v6(a) AS SELECT w FROM t2`,
		`INSERT INTO t0 VALUES(1,2)`,
		`INSERT INTO t2 VALUES(7)`,
		`SELECT * FROM t0 JOIN (v6 INDEXED BY i_t0 CROSS JOIN t2 AS t2b) USING(a)`,
	})
}
