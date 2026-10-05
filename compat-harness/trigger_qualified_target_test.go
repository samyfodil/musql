package compat

import "testing"

// TestTempTriggerQualifiedStepTarget pins where a trigger body's DML target may
// carry a schema qualifier: only in a trigger that lives in TEMP, which one ON a
// temp table does without the TEMP keyword (trigger.c:162-171, :478-485), and
// the qualifier then decides the target past a same-named temp table.
func TestTempTriggerQualifiedStepTarget(t *testing.T) {
	differ(t, "qualified trigger targets", []string{
		"CREATE TABLE t1(a, b)",
		"INSERT INTO t1 VALUES(0, 0)",
		"CREATE TEMP TABLE t4(x)",
		"CREATE TEMP TABLE t1(a, b)",
		"INSERT INTO temp.t1 VALUES(9, 9)",
		"CREATE TRIGGER tr1 AFTER DELETE ON t4 BEGIN UPDATE main.t1 SET a=1, b=2; END",
		"CREATE TEMP TRIGGER tr2 AFTER INSERT ON t4 BEGIN INSERT INTO main.t1 VALUES(5, 5); DELETE FROM temp.t1; END",
		"CREATE TRIGGER tr3 AFTER INSERT ON main.t1 BEGIN UPDATE main.t1 SET a=3; END",
		"CREATE TRIGGER main.tr4 AFTER INSERT ON t4 BEGIN UPDATE main.t1 SET a=4; END",
		"SELECT name, tbl_name FROM sqlite_temp_schema WHERE type='trigger' ORDER BY name",
		"INSERT INTO t4 VALUES(1)",
		"DELETE FROM t4",
		"SELECT * FROM main.t1 ORDER BY a",
		"SELECT * FROM temp.t1",
	})
}
