package compat

import "testing"

// TestTriggerBodySelectAnyShape pins that CREATE TRIGGER takes any SELECT as a
// body step, whatever the event: C resolves a body only when the FIRING
// statement is prepared (codeRowTrigger), so a bad reference fails that
// statement -- even one that fires nothing -- and every body runs as coded.
// sessionC.test's RAISE guard is the corpus shape; the aggregate ones name
// NEW/OLD from an aggregate's result list and HAVING.
func TestTriggerBodySelectAnyShape(t *testing.T) {
	differ(t, "trigger body SELECT shapes", []string{
		"CREATE TABLE p(a PRIMARY KEY, b)",
		"CREATE TABLE c(d, e)",
		"CREATE TABLE log(x)",
		"INSERT INTO p VALUES(1,1),(2,2),(3,3)",
		"INSERT INTO c VALUES(10,1),(20,2),(30,2)",
		"CREATE TRIGGER restrict_trig BEFORE DELETE ON p BEGIN SELECT raise(ABORT, 'error!') FROM c WHERE e=old.a; END",
		"DELETE FROM p WHERE a=3",
		"DELETE FROM p WHERE a=1",
		"DELETE FROM p WHERE 0",
		"DROP TRIGGER restrict_trig",
		"CREATE TRIGGER bad AFTER UPDATE ON p BEGIN SELECT x FROM nosuchtable; END",
		"UPDATE p SET b=5 WHERE 0",
		"DROP TRIGGER bad",
		"CREATE TRIGGER bad2 AFTER UPDATE ON p BEGIN SELECT nosuchcol FROM c; END",
		"UPDATE p SET b=5 WHERE 0",
		"DROP TRIGGER bad2",
		"CREATE TRIGGER agg AFTER UPDATE ON p BEGIN INSERT INTO log SELECT count(*)+new.a FROM c GROUP BY e HAVING e=new.a; SELECT DISTINCT d FROM c UNION SELECT a FROM p LIMIT 1; WITH w AS (SELECT 1) SELECT * FROM w, c; INSERT INTO log SELECT max(d)*old.b FROM c; END",
		"UPDATE p SET b=b+10",
		"SELECT * FROM log ORDER BY rowid",
		"SELECT * FROM p ORDER BY a",
		"CREATE TRIGGER shapes AFTER DELETE ON c BEGIN SELECT d FROM (SELECT * FROM c WHERE e > 0); SELECT d FROM (SELECT abs(e) AS d FROM c); SELECT d FROM (SELECT * FROM (SELECT * FROM c)); SELECT d FROM (SELECT c.d FROM c JOIN c AS c2 ON c.d = c2.d); SELECT d FROM (SELECT * FROM c ORDER BY e); SELECT d FROM (SELECT * FROM c LIMIT 1); SELECT raise(IGNORE) FROM c WHERE d=old.d+10; END",
		"DELETE FROM c WHERE d=10",
		"DELETE FROM c WHERE d=30",
		"SELECT * FROM c ORDER BY d",
		"SELECT type, name FROM sqlite_schema WHERE type='trigger' ORDER BY name",
	})
}
