package compat

import "testing"

// TestCreateTableAsAgrees pins CREATE TABLE AS against the oracle: the rows and
// their types whether the table is filled straight into segments or row by
// row, and last_insert_rowid(), which C's SRT_Table insert leaves alone.
func TestCreateTableAsAgrees(t *testing.T) {
	src := "SELECT 1 AS i, 'x' AS s, 2.5 AS r, NULL AS n, CAST('7' AS INTEGER) AS c UNION ALL SELECT 2, 'y', 3, x'00', '8'"
	setup := []string{
		"CREATE TABLE a(x)",
		"INSERT INTO a VALUES (5)",
		"CREATE TABLE direct AS " + src,
		"BEGIN",
		"CREATE TABLE inTx AS " + src,
		"COMMIT",
		"CREATE TABLE u AS VALUES(CAST(1 AS REAL)),(CAST(1 AS REAL)) UNION ALL SELECT NULL",
	}
	for _, q := range []string{
		"SELECT last_insert_rowid(), changes(), total_changes()",
		"SELECT rowid, *, typeof(i), typeof(s), typeof(r), typeof(n), typeof(c), hex(n) FROM direct",
		"SELECT rowid, *, typeof(i), typeof(s), typeof(r), typeof(n), typeof(c), hex(n) FROM inTx",
		"SELECT typeof(column1), quote(column1) FROM u",
		"SELECT sql FROM sqlite_schema WHERE name IN ('direct', 'u') ORDER BY name",
	} {
		aggOrderCheck(t, aggOrderCase{q, append(append([]string(nil), setup...), q), true})
	}
}
