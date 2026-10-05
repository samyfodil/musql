package compat

import "testing"

// TestAlterRenameStringSpelledNames tests ALTER RENAME when table names are
// spelled as string literals. String literals in table name positions are
// recognized as such, but strings in value positions are not.
func TestAlterRenameStringSpelledNames(t *testing.T) {
	schema := func(extra ...string) []string {
		return append([]string{
			"CREATE TABLE a(x, y)",
			"CREATE TABLE b(a, p)",
			"CREATE INDEX ai ON a(x)",
		}, extra...)
	}
	dump := "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name"

	differ(t, "a value that merely looks like the table name", append(schema(
		"CREATE TRIGGER tr AFTER INSERT ON b BEGIN INSERT INTO a(x,y) VALUES(new.p, 'a'); END",
		"ALTER TABLE a RENAME TO c"), dump))
	differ(t, "a trigger body naming the table as a string", append(schema(
		"CREATE TRIGGER tr AFTER INSERT ON b BEGIN INSERT INTO 'a'(x,y) VALUES(new.p, 1); END",
		"ALTER TABLE a RENAME TO c"), dump))
	differ(t, "a column rename through a string-named target", append(schema(
		"CREATE TRIGGER tr AFTER INSERT ON b BEGIN INSERT INTO 'a'(x,y) VALUES(new.p, 1); END",
		"ALTER TABLE a RENAME COLUMN x TO z"), dump))
	differ(t, "a view naming the table as a string", append(schema(
		"CREATE VIEW v AS SELECT x FROM 'a'",
		"ALTER TABLE a RENAME TO c"), dump))
	differ(t, "a column rename through a string-named view FROM", append(schema(
		"CREATE VIEW v AS SELECT x FROM 'a'",
		"ALTER TABLE a RENAME COLUMN x TO z"), dump))
	differ(t, "a string name and a same-named column on another table", append(schema(
		"CREATE TRIGGER tr AFTER INSERT ON b BEGIN INSERT INTO 'a'(x,y) VALUES(new.p, 1); END",
		"ALTER TABLE b RENAME COLUMN a TO q"), dump))
}
