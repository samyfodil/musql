package compat

import "testing"

// TestGroupConnectorViewDoesNotDoubleReferenceCount tests that a parenthesized
// join group doesn't double-count view reference depth.
func TestGroupConnectorViewDoesNotDoubleReferenceCount(t *testing.T) {
	differ(t, "group_connector_view_refcount", []string{
		"CREATE VIEW v1 AS SELECT 1 AS x",
		"CREATE VIEW v2 AS SELECT * FROM v1 UNION SELECT * FROM v1",
		"CREATE VIEW v4 AS SELECT * FROM v2 UNION SELECT * FROM v2",
		"CREATE VIEW v8 AS SELECT * FROM v4 UNION SELECT * FROM v4",
		"CREATE VIEW v16 AS SELECT * FROM v8 UNION SELECT * FROM v8",
		"CREATE VIEW v32 AS SELECT * FROM v16 UNION SELECT * FROM v16",
		"CREATE TABLE t(a)",
		"INSERT INTO t VALUES(1)",
		"SELECT * FROM t JOIN (v32 CROSS JOIN t AS t2) ON 1",
	})
}
