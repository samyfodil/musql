// This file tests that compound query arms resolve column names from the schema
// without executing the subquery.
package compat

import "testing"

// TestCompoundArmDerivedSchemaOnly tests compound arm schema resolution without execution.
func TestCompoundArmDerivedSchemaOnly(t *testing.T) {
	setup := []string{
		`CREATE TABLE abc(a, b, c)`,
		`CREATE TABLE one(x)`,
		`INSERT INTO one VALUES(1)`,
	}
	q := func(sql string) []string { return append(append([]string{}, setup...), sql) }

	answering := []string{
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1, 'fault', 3 FROM (SELECT DISTINCT 2147483648, 'hardware' UNION ALL SELECT -2147483648, 'experiments' ORDER BY 2147483648 LIMIT 1 OFFSET 123456789.1234567899) UNION SELECT 'The','The',2147483649 UNION ALL SELECT DISTINCT 'hardware','first','experiments' ORDER BY 'hardware' LIMIT 123456789.1234567899 OFFSET -2147483647)) FROM abc`,
		`SELECT EXISTS(SELECT 1 FROM (SELECT 'k' FROM (SELECT 1 AS z UNION ALL SELECT 2 LIMIT 1 OFFSET 56.1) UNION ALL SELECT 'm' ORDER BY 'k')) FROM abc`,
		`SELECT randomblob(min(max(coalesce(EXISTS (SELECT 1 FROM ( SELECT (SELECT 2147483647) NOT IN (SELECT 2147483649 UNION ALL SELECT DISTINCT -1) IN (SELECT 2147483649), 'fault', (SELECT ALL -1 INTERSECT SELECT 'experiments') IN (SELECT ALL 56.1 ORDER BY 'experiments' DESC) FROM (SELECT DISTINCT 2147483648, 'hardware' UNION ALL SELECT -2147483648, 'experiments' ORDER BY 2147483648 LIMIT 1 OFFSET 123456789.1234567899) GROUP BY (SELECT ALL 0 INTERSECT SELECT 'in') IN (SELECT DISTINCT 'experiments' ORDER BY zeroblob(1000) LIMIT 56.1 OFFSET -456) HAVING EXISTS (SELECT 'fault' EXCEPT    SELECT DISTINCT 56.1) UNION SELECT 'The', 'The', 2147483649 UNION ALL SELECT DISTINCT 'hardware', 'first', 'experiments' ORDER BY 'hardware' LIMIT 123456789.1234567899 OFFSET -2147483647)) NOT IN (SELECT (SELECT DISTINCT (SELECT 'The') FROM abc ORDER BY EXISTS (SELECT -1 INTERSECT SELECT ALL NULL) ASC) IN (SELECT DISTINCT EXISTS (SELECT ALL 123456789.1234567899 ORDER BY 1 ASC, NULL DESC) FROM sqlite_master INTERSECT SELECT 456)), (SELECT ALL 'injection' UNION ALL SELECT ALL (SELECT DISTINCT 'first' UNION     SELECT DISTINCT 'The') FROM (SELECT 456, 'in', 2147483649))),1), 500)), 'first', EXISTS (SELECT DISTINCT 456 FROM abc ORDER BY 'experiments' DESC) FROM abc`,
	}
	for _, sql := range answering {
		differ(t, sql, q(sql))
		res := run(t, "musql", q(sql))
		if last := res[len(res)-1]; last["kind"] == "error" {
			t.Errorf("still declining: %s -> %v", sql, last)
		}
	}

	for _, sql := range []string{
		`SELECT EXISTS(SELECT 1 FROM (SELECT 'k' FROM (SELECT 1 AS z UNION ALL SELECT 2 LIMIT 1 OFFSET 56.1) UNION ALL SELECT 'm' ORDER BY 'k')) FROM one`,
	} {
		differ(t, sql, q(sql))
		res := run(t, "musql", q(sql))
		if last := res[len(res)-1]; last["kind"] != "error" {
			t.Errorf("stepped body must raise: %s -> %v", sql, last)
		}
	}

	differ(t, "ordinary compound arms over derived tables", []string{
		`CREATE TABLE t(a, b)`,
		`INSERT INTO t VALUES(3,'z'),(1,'y'),(2,'x')`,
		`SELECT * FROM (SELECT a, b FROM (SELECT * FROM t) UNION ALL SELECT 9, 'w' ORDER BY b)`,
		`SELECT * FROM (SELECT a FROM (SELECT a FROM t ORDER BY a LIMIT 2) UNION ALL SELECT 9 ORDER BY a)`,
		`SELECT * FROM (SELECT a AS k FROM t UNION ALL SELECT 9 ORDER BY k)`,
		`SELECT * FROM (SELECT b FROM (SELECT b FROM t) UNION SELECT 'z' ORDER BY 1)`,
	})
}
