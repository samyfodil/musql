package compat

import "testing"

// TestAlterColumnNotNullAndAddCheck pins ALTER TABLE's 3.53 constraint forms
// (engine/alter_write.go) against the oracle: the stored text each one leaves
// (sqlite_drop_constraint / sqlite_add_constraint, alter.c:2519-2691), what is
// ENFORCED afterwards -- the reload re-derives NOT NULL and CHECK from that
// text -- and the errors in C's order.
func TestAlterColumnNotNullAndAddCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"set and drop not null", []string{
			"CREATE TABLE t(a, b INT, c TEXT DEFAULT 'x')",
			"INSERT INTO t VALUES(1, 2, 'y')",
			"PRAGMA schema_version",
			"ALTER TABLE t ALTER b SET NOT   NULL",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"PRAGMA schema_version",
			"PRAGMA table_info(t)",
			"INSERT INTO t VALUES(3, NULL, 'z')",
			"ALTER TABLE t ALTER COLUMN b DROP NOT NULL",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"INSERT INTO t VALUES(3, NULL, 'z')",
			"ALTER TABLE t ALTER b DROP NOT NULL",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"PRAGMA schema_version",
			"SELECT * FROM t ORDER BY a",
		}},
		{"set not null refuses an existing null", []string{
			"CREATE TABLE t(a, b)",
			"INSERT INTO t VALUES(1, NULL)",
			"ALTER TABLE t ALTER b SET NOT NULL",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"UPDATE t SET b=0",
			"ALTER TABLE t ALTER b SET NOT NULL ON CONFLICT IGNORE",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"INSERT INTO t VALUES(2, NULL)",
			"SELECT count(*) FROM t",
		}},
		{"a named not null", []string{
			"CREATE TABLE t(a CONSTRAINT nn NOT NULL, b)",
			"ALTER TABLE t DROP CONSTRAINT nn",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"INSERT INTO t VALUES(NULL, 1)",
			"ALTER TABLE t ALTER a SET NOT NULL",
			"DELETE FROM t",
			"ALTER TABLE t ALTER a SET NOT NULL /* kept */ -- dropped",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
		}},
		{"add check", []string{
			"CREATE TABLE t(a INT, b)",
			"INSERT INTO t VALUES(1, 1), (2, 5)",
			"ALTER TABLE t ADD CHECK (b < 5)",
			"ALTER TABLE t ADD CONSTRAINT c1 CHECK (b < 10) /* c */ -- gone",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"INSERT INTO t VALUES(3, 11)",
			"INSERT INTO t VALUES(3, 9)",
			"ALTER TABLE t ADD CONSTRAINT c1 CHECK (a > 0)",
			"ALTER TABLE t ADD CONSTRAINT C1 CHECK (a > 0)",
			"ALTER TABLE t ADD CONSTRAINT \"c2\" CHECK (a > 0) ON CONFLICT FAIL",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
			"ALTER TABLE t DROP CONSTRAINT c1",
			"INSERT INTO t VALUES(4, 11)",
			"SELECT * FROM t ORDER BY a",
		}},
		{"errors", []string{
			"CREATE TABLE t(a, b)",
			"CREATE VIEW v AS SELECT a FROM t",
			"CREATE VIRTUAL TABLE r USING rtree(id, x1, x2)",
			"ALTER TABLE t ALTER nosuch SET NOT NULL",
			"ALTER TABLE t ALTER rowid DROP NOT NULL",
			"ALTER TABLE v ALTER a SET NOT NULL",
			"ALTER TABLE r ALTER x1 SET NOT NULL",
			"ALTER TABLE sqlite_schema ALTER sql SET NOT NULL",
			"ALTER TABLE t ADD CHECK (nosuch > 0)",
			"ALTER TABLE t ADD CHECK ((SELECT 1) > 0)",
			"ALTER TABLE t ADD CHECK (random() > 0)",
			"ALTER TABLE t ADD CONSTRAINT CHECK (a > 0)",
			"ALTER TABLE t ALTER a SET NULL",
			"ALTER TABLE t ALTER a DROP NOT",
			"ALTER TABLE nosuch ALTER a SET NOT NULL",
			"SELECT sql FROM sqlite_schema WHERE name='t'",
		}},
		{"temp", []string{
			"CREATE TEMP TABLE tt(a, b)",
			"ALTER TABLE temp.tt ALTER a SET NOT NULL",
			"ALTER TABLE tt ADD CHECK (b != 0)",
			"SELECT sql FROM sqlite_temp_schema WHERE name='tt'",
			"INSERT INTO tt VALUES(NULL, 1)",
			"INSERT INTO tt VALUES(1, 0)",
			"INSERT INTO tt VALUES(1, 1)",
			"SELECT * FROM tt",
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "alter constraint: "+tc.name, tc.stmts) })
	}
}
