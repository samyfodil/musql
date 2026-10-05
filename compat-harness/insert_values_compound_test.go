package compat

import "testing"

// TestInsertValuesAsSelect pins INSERT sources that are a VALUES list and yet
// a SELECT (engine/insert_write.go): one followed by a compound operator,
// which is only the compound's first arm, and one with a row that is not
// constant, which C codes as a UNION ALL of one-row SELECTs (values.test 1.2.x
// and 3.x). Rows land in rowid order, so the SELECT after each shows the order
// they were inserted in, not just which.
func TestInsertValuesAsSelect(t *testing.T) {
	differ(t, "INSERT VALUES as a SELECT", []string{
		"CREATE TABLE x1(a, b, c)",
		"INSERT INTO x1 VALUES(1, 1, 1), (2, 2, 2), (3, 3, 3) UNION ALL SELECT 4, 4, 4",
		"SELECT rowid, * FROM x1",
		"DELETE FROM x1",
		"INSERT INTO x1 VALUES(3, 3, 3), (1, 1, 1), (3, 3, 3) UNION SELECT 2, 2, 2",
		"SELECT rowid, * FROM x1",
		"DELETE FROM x1",
		"INSERT INTO x1 VALUES(3, 3, 3), (1, 1, 1) UNION ALL SELECT 2, 2, 2 ORDER BY 1 DESC LIMIT 2",
		"SELECT rowid, * FROM x1",
		"DELETE FROM x1",
		"INSERT INTO x1 VALUES(1, 1, 1), (2, 2, 2) EXCEPT SELECT 1, 1, 1",
		"INSERT INTO x1 VALUES(5, 5, 5) UNION ALL VALUES(6, 6, 6), (7, 7, 7)",
		"SELECT rowid, * FROM x1",
		"CREATE TABLE y1(x, y)",
		"INSERT INTO y1 VALUES(1, 2), (3, 4), (row_number() OVER (), 5)",
		"INSERT INTO y1 VALUES(1, 2), (3, 4), (row_number() OVER (), 6), (row_number() OVER (), 7)",
		"INSERT INTO y1 VALUES(row_number() OVER (), 8)",
		"INSERT INTO y1 VALUES(row_number() OVER (), 12), (13, 14)",
		"INSERT INTO y1 VALUES(1, 2), (count(*), 9)",
		"INSERT INTO y1(y, x) VALUES(10, 1), (11, sum(1) OVER ())",
		"SELECT rowid, * FROM y1",
		"INSERT INTO y1 VALUES(1, 2), (3, 4) UNION ALL SELECT 5",
	})
}
