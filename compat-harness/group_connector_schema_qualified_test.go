package compat

// Schema-qualified connectors in parenthesized join groups, including
// attached databases and CTE shadowing.

import "testing"

func TestGroupConnectorSchemaQualified(t *testing.T) {
	differ(t, "group_connector_schema_qualified", []string{
		`CREATE TABLE x1(p)`,
		`CREATE TABLE q1(a)`,
		`CREATE TABLE q2(b)`,
		`INSERT INTO x1 VALUES(1)`,
		`INSERT INTO q1 VALUES(2)`,
		`INSERT INTO q2 VALUES(3)`,
		`SELECT * FROM x1 JOIN (main.q1 JOIN q2 ON 1) ON 1`,
	})
}

func TestGroupConnectorSchemaQualifiedAliased(t *testing.T) {
	differ(t, "group_connector_schema_qualified_aliased", []string{
		`CREATE TABLE t4(a)`,
		`CREATE TABLE t1(p)`,
		`CREATE TABLE t2(q)`,
		`INSERT INTO t4 VALUES(1)`,
		`INSERT INTO t1 VALUES(2)`,
		`INSERT INTO t2 VALUES(3)`,
		`SELECT x.a, t1.p, t2.q FROM t1 JOIN (t2 JOIN main.t4 x ON x.a=t2.q+1) ON t2.q=t1.p+1`,
	})
}

// TestGroupConnectorAttached: connector in an attached database.
func TestGroupConnectorAttached(t *testing.T) {
	differ(t, "group_connector_attached", []string{
		`ATTACH ':memory:' AS aux1`,
		`CREATE TABLE t1(p)`,
		`CREATE TABLE aux1.t4(w)`,
		`CREATE TABLE t2(q)`,
		`INSERT INTO t1 VALUES(1)`,
		`INSERT INTO aux1.t4 VALUES(2)`,
		`INSERT INTO t2 VALUES(3)`,
		`SELECT * FROM t1 JOIN (aux1.t4 JOIN t2 ON t2.q=t4.w+1) ON t2.q=t1.p+1`,
	})
}

// TestGroupConnectorSchemaQualifiedNotCTE: a schema-qualified connector cannot be shadowed by a CTE.
func TestGroupConnectorSchemaQualifiedNotCTE(t *testing.T) {
	differ(t, "group_connector_schema_qualified_not_cte", []string{
		`CREATE TABLE t4(a)`,
		`CREATE TABLE t1(p)`,
		`CREATE TABLE t2(q)`,
		`INSERT INTO t4 VALUES(1)`,
		`INSERT INTO t1 VALUES(2)`,
		`INSERT INTO t2 VALUES(3)`,
		`WITH t4(a) AS (SELECT 99) SELECT x.a, t1.p, t2.q FROM t1 JOIN (t2 JOIN main.t4 x ON x.a=t2.q+1) ON t2.q=t1.p+1`,
	})
}
