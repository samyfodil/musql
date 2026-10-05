package compat

import "testing"

// TestAlterRenameStringLiteralName verifies that ALTER TABLE RENAME correctly
// handles string literals used as table names and updates all REFERENCES.
func TestAlterRenameStringLiteralName(t *testing.T) {
	differ(t, "alter-rename-string-literal-name", []string{
		`CREATE TABLE 'p 1 "parent one"'(a REFERENCES 'p 1 "parent one"', b, PRIMARY KEY(b))`,
		`CREATE TABLE c1(c, d REFERENCES 'p 1 "parent one"' ON UPDATE CASCADE)`,
		`CREATE TABLE c2(e, f, FOREIGN KEY(f) REFERENCES 'p 1 "parent one"' ON UPDATE CASCADE)`,
		`INSERT INTO 'p 1 "parent one"' VALUES(1, 1)`,
		`ALTER TABLE 'p 1 "parent one"' RENAME TO p`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`INSERT INTO p VALUES(2, 2)`,
		`SELECT * FROM p ORDER BY b`,
	})
}
