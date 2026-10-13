package compat

import "testing"

// TestAlterRevalidateSweep crosses RENAME COLUMN / DROP COLUMN with the bodies
// a view or trigger can hold that name the column: renameTestSchema prepares
// every one afterwards, so a name the rewrite could not follow fails the ALTER.
func TestAlterRevalidateSweep(t *testing.T) {
	selects := []string{
		"SELECT b FROM s",
		"SELECT * FROM s",
		"SELECT * FROM s ORDER BY b",
		"SELECT c1 AS b FROM s ORDER BY b",
		"SELECT c1 AS b FROM s WHERE b",
		"SELECT c1 AS b FROM s GROUP BY b",
		"SELECT c1 FROM s ORDER BY b",
		"SELECT c1 FROM s ORDER BY b DESC, c2",
		"SELECT c1 FROM s GROUP BY b",
		"SELECT EXISTS(SELECT 1 FROM s WHERE b)",
		"SELECT EXISTS(SELECT * FROM s ORDER BY b)",
		"SELECT EXISTS(SELECT 1 FROM s ORDER BY b)",
		"SELECT (SELECT max(b) FROM s)",
		"SELECT x FROM o WHERE x IN (SELECT b FROM s)",
		"SELECT s.b FROM s",
		"SELECT t1.b FROM s AS t1",
		"SELECT b FROM (SELECT b FROM s)",
		"SELECT b FROM s UNION SELECT b FROM s",
		"WITH w AS (SELECT b FROM s) SELECT * FROM w",
		"SELECT b FROM prev",
		"SELECT * FROM prev",
	}
	ops := []string{"ALTER TABLE s RENAME COLUMN b TO bb", "ALTER TABLE s DROP COLUMN b"}
	pre := []string{"CREATE TABLE s(c1, c2, b)", "CREATE TABLE o(x)", "CREATE TABLE d(n)", "CREATE VIEW prev AS SELECT c1, b FROM s"}
	with := func(stmts ...string) []string { return append(append([]string(nil), pre...), stmts...) }
	var cases [][]string
	for _, sel := range selects {
		for _, op := range ops {
			cases = append(cases, with("CREATE VIEW vv AS "+sel, op,
				"SELECT sql FROM sqlite_master WHERE name IN ('vv','prev','s') ORDER BY name", "SELECT count(*) FROM vv"))
			for _, step := range []string{
				sel + ";",
				"INSERT INTO o " + sel + ";",
				"UPDATE o SET x=(" + sel + ");",
				"DELETE FROM o WHERE x IN (" + sel + ");",
			} {
				cases = append(cases, with("CREATE TRIGGER tg AFTER INSERT ON d BEGIN "+step+" END", op,
					"SELECT sql FROM sqlite_master WHERE name IN ('tg','prev','s') ORDER BY name", "INSERT INTO d VALUES(1)"))
			}
		}
	}
	for _, when := range []string{"WHEN new.n > (SELECT max(b) FROM s)", "WHEN EXISTS(SELECT 1 FROM s WHERE b)", "WHEN main.s.b > 0", "WHEN s.b > 0"} {
		for _, op := range ops {
			cases = append(cases, with("CREATE TRIGGER tg AFTER INSERT ON s "+when+" BEGIN SELECT 1; END", op,
				"SELECT sql FROM sqlite_master WHERE name IN ('tg','s') ORDER BY name", "INSERT INTO s VALUES(1,2,3)"))
		}
	}
	sweepAll(t, cases)
}
