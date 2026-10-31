package compat

import "testing"

// A VIRTUAL column added by ALTER TABLE ADD COLUMN whose expression raises over
// a row written before it (json_extract of a value that is not JSON). C
// computes the column only where a statement reads it: the read raises, every
// other statement answers, and no row is lost. musql used to drop the row from
// every scan, and VACUUM or a compaction then dropped it for good.

var gencolRaiseSetup = []string{
	"CREATE TABLE i(value INT, j TEXT)",
	"INSERT INTO i VALUES(1,'bad'),(7,'1')",
	"ALTER TABLE i ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL",
}

func gencolRaiseCase(stmts ...string) []string {
	return append(append([]string{}, gencolRaiseSetup...), stmts...)
}

func TestGeneratedVirtualRaiseReadsOnlyItsColumn(t *testing.T) {
	for name, stmts := range map[string][]string{
		"reads": gencolRaiseCase(
			"SELECT value, j FROM i",
			"SELECT count(*) FROM i",
			"SELECT sum(value) FROM i",
			"SELECT max(j), min(value) FROM i",
			"SELECT total(value), avg(value), group_concat(j) FROM i",
			"SELECT count(g) FROM i",
			"SELECT min(g) FROM i",
			"SELECT g FROM i",
			"SELECT * FROM i",
			"SELECT value FROM i WHERE g > 0",
			"SELECT value FROM i WHERE value = 7 AND g > 0",
			"SELECT value FROM i ORDER BY g",
			"SELECT value, j FROM i ORDER BY j",
			"SELECT DISTINCT j FROM i",
			"SELECT j, count(*) FROM i GROUP BY j",
			"SELECT a.value, b.value FROM i a JOIN i b ON a.value = b.value",
			"SELECT value FROM i WHERE rowid = 1",
			"SELECT g FROM i WHERE rowid = 2",
			"SELECT value FROM i UNION SELECT value FROM i",
			"SELECT value FROM (SELECT * FROM i)",
			"WITH c AS (SELECT * FROM i) SELECT value FROM c",
			"WITH c AS MATERIALIZED (SELECT * FROM i) SELECT value FROM c",
			"SELECT value FROM i WHERE value IN (SELECT value FROM i)",
			"SELECT EXISTS (SELECT * FROM i)",
			"SELECT (SELECT g FROM i WHERE value = 7)",
			"PRAGMA integrity_check",
			"PRAGMA quick_check",
		),
		"views": gencolRaiseCase(
			"CREATE VIEW v AS SELECT * FROM i",
			"SELECT value FROM v",
			"SELECT g FROM v",
		),
		"copy": gencolRaiseCase(
			"CREATE TABLE t2(a, b, c)",
			"INSERT INTO t2 SELECT * FROM i",
			"INSERT INTO t2 SELECT value, j, 0 FROM i",
			"SELECT * FROM t2",
			"CREATE TABLE t3 AS SELECT * FROM i",
			"CREATE TABLE t4 AS SELECT value, j FROM i",
			"SELECT * FROM t4",
		),
		"update": gencolRaiseCase(
			"UPDATE i SET value = value + 100",
			"SELECT value, j FROM i",
			"UPDATE i SET value = value + 100 WHERE value = 7",
			"UPDATE i SET j = j WHERE value = 1",
			"UPDATE i SET j = '3' WHERE value = 1",
			"SELECT value, j, g FROM i",
		),
		"update-returning": gencolRaiseCase(
			"UPDATE i SET j = '4' WHERE value = 1 RETURNING value",
			"UPDATE i SET j = '5' WHERE value = 1 RETURNING g",
			"SELECT value, j FROM i",
		),
		"replace": gencolRaiseCase(
			"REPLACE INTO i(rowid, value, j) VALUES(1, 5, '5')",
			"SELECT value, j, g FROM i",
		),
		"delete": gencolRaiseCase(
			"DELETE FROM i WHERE value = 1 RETURNING value",
			"DELETE FROM i WHERE value = 7 RETURNING g",
			"SELECT value, j FROM i",
			"DELETE FROM i WHERE value = 1",
			"SELECT value, j FROM i",
		),
		"trigger-reads-g": gencolRaiseCase(
			"CREATE TABLE log(x)",
			"CREATE TRIGGER tu BEFORE UPDATE ON i BEGIN INSERT INTO log VALUES(old.g); END",
			"UPDATE i SET j = '3' WHERE value = 1",
			"CREATE TRIGGER td BEFORE DELETE ON i BEGIN INSERT INTO log VALUES(old.g); END",
			"DELETE FROM i WHERE value = 1",
			"SELECT * FROM log",
			"SELECT value, j FROM i",
		),
		"ddl": gencolRaiseCase(
			"ALTER TABLE i ADD COLUMN h AS (g || 'x')",
			"SELECT value FROM i",
			"SELECT h FROM i",
			"ALTER TABLE i RENAME COLUMN j TO k",
			"ALTER TABLE i RENAME TO i2",
			"SELECT value, k FROM i2",
			"ALTER TABLE i2 DROP COLUMN value",
			"SELECT k FROM i2",
			"ALTER TABLE i2 DROP COLUMN g",
			"SELECT * FROM i2",
		),
		"index": gencolRaiseCase(
			"CREATE INDEX ij ON i(j)",
			"CREATE INDEX ie ON i(value) WHERE g > 0",
			"CREATE INDEX ix ON i(json_extract(j,'$'))",
			"SELECT name FROM sqlite_schema ORDER BY name",
			"REINDEX",
			"SELECT value FROM i WHERE j = 'bad'",
			"UPDATE i SET j = '9' WHERE j = 'bad'",
			"SELECT value, j, g FROM i",
		),
		"txn": {
			"CREATE TABLE i(value INT, j TEXT)",
			"BEGIN",
			"INSERT INTO i VALUES(1,'bad'),(7,'1')",
			"ALTER TABLE i ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL",
			"SELECT value, j FROM i",
			"SELECT g FROM i",
			"COMMIT",
			"SELECT value, j FROM i",
			"BEGIN",
			"INSERT INTO i(value, j) VALUES(8, '8')",
			"SELECT g FROM i WHERE value = 8",
			"ROLLBACK",
			"SELECT value, j FROM i",
		},
		"vacuum": gencolRaiseCase(
			"VACUUM",
			"SELECT value, j FROM i",
			"SELECT g FROM i WHERE value = 7",
			"SELECT g FROM i",
		),
		"ipk": {
			"CREATE TABLE i(id INTEGER PRIMARY KEY, j TEXT)",
			"INSERT INTO i VALUES(1,'bad'),(7,'1')",
			"ALTER TABLE i ADD COLUMN g AS (json_extract(j,'$') + id)",
			"SELECT id, j FROM i",
			"SELECT g FROM i WHERE id = 7",
			"SELECT g FROM i WHERE id = 1",
			"UPDATE i SET j = '2' WHERE id = 1",
			"SELECT id, g FROM i",
		},
		"typed": {
			"CREATE TABLE i(value INT, j TEXT)",
			"INSERT INTO i VALUES(1,'bad'),(7,'1')",
			"ALTER TABLE i ADD COLUMN g INT AS (json_extract(j,'$')) VIRTUAL",
			"PRAGMA integrity_check",
			"PRAGMA quick_check",
			"PRAGMA integrity_check(i)",
			"SELECT value FROM i",
		},
		"chain": {
			"CREATE TABLE i(value INT, j TEXT, v AS (value * 2), s AS (value + 1) STORED)",
			"INSERT INTO i(value, j) VALUES(1,'bad'),(7,'1')",
			"ALTER TABLE i ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL",
			"ALTER TABLE i ADD COLUMN h AS (v + 1)",
			"ALTER TABLE i ADD COLUMN k AS (g + 1)",
			"SELECT value, v, s, h FROM i",
			"SELECT v FROM i WHERE h > 0",
			"SELECT k FROM i",
			"SELECT * FROM i",
		},
	} {
		differ(t, "gencol-raise "+name, stmts)
	}
}

// Paths that read a key column of every row: a foreign key's scan of its child
// table and the fill of a new index compute a VIRTUAL key column for each row,
// so a row whose column raises fails the statement there, as in C.
func TestGeneratedVirtualRaiseKeyScans(t *testing.T) {
	for name, stmts := range map[string][]string{
		"fk": {
			"PRAGMA foreign_keys = ON",
			"CREATE TABLE p(id INTEGER PRIMARY KEY, v)",
			"INSERT INTO p VALUES(1, 'a')",
			"CREATE TABLE c(x TEXT)",
			"INSERT INTO c VALUES('bad'),('1')",
			"ALTER TABLE c ADD COLUMN g AS (json_extract(x,'$')) REFERENCES p(id)",
			"SELECT x FROM c",
			"PRAGMA foreign_key_check",
			"DELETE FROM p WHERE id = 1",
			"INSERT INTO p VALUES(2, 'b')",
			"UPDATE p SET v = 'c' WHERE id = 2",
			"UPDATE p SET id = 3 WHERE id = 2",
			"DELETE FROM p WHERE id = 2",
			"SELECT id, v FROM p",
			"UPDATE c SET x = '2' WHERE x = 'bad'",
			"SELECT x, g FROM c",
			"DELETE FROM c WHERE x = '1'",
			"SELECT x FROM c",
		},
		"fk-child-delete": {
			"PRAGMA foreign_keys = ON",
			"CREATE TABLE p(id INTEGER PRIMARY KEY)",
			"INSERT INTO p VALUES(1)",
			"CREATE TABLE c(x TEXT, y)",
			"INSERT INTO c VALUES('bad', 1),('1', 2)",
			"ALTER TABLE c ADD COLUMN g AS (json_extract(x,'$')) REFERENCES p(id)",
			"UPDATE c SET y = 3 WHERE x = 'bad'",
			"DELETE FROM c WHERE x = 'bad'",
			"SELECT x, y FROM c",
			"PRAGMA foreign_keys = OFF",
			"DELETE FROM c WHERE x = 'bad'",
			"SELECT x, y FROM c",
		},
		"index": gencolRaiseCase(
			"CREATE INDEX ig ON i(g)",
			"CREATE INDEX iv ON i(value, g)",
			"CREATE UNIQUE INDEX iu ON i(g)",
			"CREATE INDEX ip ON i(g) WHERE value > 5",
			"CREATE INDEX ip2 ON i(g) WHERE value < 5",
			"CREATE INDEX ie ON i(g, lower(j))",
			"SELECT name FROM sqlite_schema ORDER BY name",
			"SELECT value FROM i WHERE g = 1",
		),
	} {
		differ(t, "gencol-raise "+name, stmts)
	}
}

// An upsert's DO UPDATE reads the conflicting row: C computes its VIRTUAL
// column only where SET or WHERE reads it, and the new row's from the values
// the update leaves.
func TestGeneratedVirtualRaiseUpsert(t *testing.T) {
	setup := []string{
		"CREATE TABLE u(k INT UNIQUE, j TEXT, x)",
		"INSERT INTO u VALUES(1,'bad',0),(7,'1',0)",
		"ALTER TABLE u ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL",
		"CREATE TABLE log(x)",
	}
	tail := "SELECT k, j, g FROM u WHERE k <> 1 ORDER BY k"
	for name, stmts := range map[string][]string{
		"set-reads-g":      {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = g", "SELECT k, j FROM u ORDER BY k"},
		"set-reads-g-expr": {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = coalesce(g, 'n')", "SELECT k, j FROM u ORDER BY k"},
		"set-x-reads-g":    {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = excluded.j, x = g", "SELECT k, j, x FROM u ORDER BY k"},
		"set-x-reads-g-op": {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = excluded.j, x = typeof(g) || (g IS NULL)", "SELECT k, j, x FROM u ORDER BY k"},
		"where-reads-g":    {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = excluded.j WHERE g IS NULL", "SELECT k, j FROM u ORDER BY k"},
		"where-skips-g":    {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = '3' WHERE k > 100 AND g > 0", "SELECT k, j FROM u ORDER BY k"},
		"set-excluded":     {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = excluded.j", "SELECT k, j, g FROM u ORDER BY k"},
		"good-row-reads-g": {"INSERT INTO u(k,j) VALUES(7,'2') ON CONFLICT(k) DO UPDATE SET j = g + 1", "SELECT k, j, g FROM u ORDER BY k"},
		"returning":        {"INSERT INTO u(k,j) VALUES(1,'9') ON CONFLICT(k) DO UPDATE SET j = excluded.j RETURNING k, g", tail},
		"new-row-raises":   {"INSERT INTO u(k,j) VALUES(1,'x') ON CONFLICT(k) DO UPDATE SET k = 2", "SELECT k, j FROM u ORDER BY k"},
		"subquery-reads-g": {"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = (SELECT g)", "SELECT k, j FROM u ORDER BY k"},
		"trigger-old-k": {
			"CREATE TRIGGER tr AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(old.k); END",
			"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = excluded.j",
			"SELECT x FROM log", "SELECT k, j, g FROM u ORDER BY k",
		},
		"trigger-old-g": {
			"CREATE TRIGGER tr AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(old.g); END",
			"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = excluded.j",
			"SELECT x FROM log", "SELECT k, j FROM u ORDER BY k",
		},
		"before-trigger": {
			"CREATE TRIGGER tr BEFORE UPDATE ON u BEGIN INSERT INTO log VALUES(new.k); END",
			"INSERT INTO u(k,j) VALUES(1,'5') ON CONFLICT(k) DO UPDATE SET j = excluded.j",
			"SELECT x FROM log", "SELECT k, j, g FROM u ORDER BY k",
		},
		"replace": {"INSERT OR REPLACE INTO u(k,j) VALUES(1,'5')", "SELECT k, j, g FROM u ORDER BY k"},
	} {
		differ(t, "gencol-raise upsert "+name, append(append([]string{}, setup...), stmts...))
	}
}

// ALTER TABLE ADD COLUMN runs quick_check over the table when the new column
// has a CHECK, is a NOT NULL generated column, or the table is STRICT
// (alter.c). quick_check computes a VIRTUAL column only where the column is
// NOT NULL or its type is checked, so only then can its expression fail the
// ALTER, and in a STRICT table a value not of the column's type fails it too.
// The ALTER is Exec'd: C reports the nested quick_check's columns on a Query.
func TestGeneratedVirtualAddColumnChecks(t *testing.T) {
	strict := []string{"CREATE TABLE s(a INT, j TEXT) STRICT"}
	plain := []string{"CREATE TABLE c(a INT, j TEXT)", "INSERT INTO c VALUES(1,'bad'),(7,'1')"}
	with := func(base []string, more ...string) []string {
		return append(append([]string{}, base...), more...)
	}
	for _, tc := range []struct {
		setup    []string
		q, table string
	}{
		{with(strict, "INSERT INTO s VALUES(1,'x'),(2,'5')"), "ALTER TABLE s ADD COLUMN g INT AS (j) VIRTUAL", "s"},
		{with(strict, "INSERT INTO s VALUES(1,'bad'),(2,'5')"), "ALTER TABLE s ADD COLUMN g INT AS (json_extract(j,'$')) VIRTUAL", "s"},
		{with(strict, "INSERT INTO s VALUES(1,'7'),(2,'5')"), "ALTER TABLE s ADD COLUMN g INT AS (j) VIRTUAL", "s"},
		{with(strict, "INSERT INTO s VALUES(1,'bad'),(2,'5')"), "ALTER TABLE s ADD COLUMN g ANY AS (json_extract(j,'$')) VIRTUAL", "s"},
		{with(strict, "INSERT INTO s VALUES(1,'bad'),(2,'5')", "ALTER TABLE s ADD COLUMN g ANY AS (json_extract(j,'$')) VIRTUAL"), "ALTER TABLE s ADD COLUMN h INT", "s"},
		{with(plain, "ALTER TABLE c ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL"), "ALTER TABLE c ADD COLUMN h INT CHECK (h IS NULL OR h > 0)", "c"},
		{with(plain, "ALTER TABLE c ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL"), "ALTER TABLE c ADD COLUMN h INT CHECK (g IS NOT NULL OR h IS NULL)", "c"},
		{with(plain, "ALTER TABLE c ADD COLUMN g INT AS (json_extract(j,'$')) VIRTUAL"), "ALTER TABLE c ADD COLUMN h INT CHECK (h IS NULL)", "c"},
		{[]string{"CREATE TABLE n(a INT, j TEXT)", "INSERT INTO n VALUES(1,NULL),(7,'1')"}, "ALTER TABLE n ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL NOT NULL", "n"},
	} {
		eoqDiffer(t, tc.setup, tc.q, "SELECT count(*) FROM pragma_table_xinfo('"+tc.table+"')")
	}
}

// A write whose VIRTUAL column raises fails with the expression's own message,
// as in C, which names neither the table nor the column.
func TestGeneratedVirtualRaiseErrorText(t *testing.T) {
	for _, q := range []string{
		"UPDATE i SET value = value + 100",
		"UPDATE i SET j = 'bad2' WHERE value = 7",
		"INSERT INTO i(value, j) VALUES(3, 'bad')",
	} {
		c, m := boundPair(t, gencolRaiseSetup)
		_, cerr := c.Exec(q)
		_, merr := m.Exec(q)
		if cerr == nil || merr == nil || cerr.Error() != merr.Error() {
			t.Errorf("%s\n  cgo: %v\n  mus: %v", q, cerr, merr)
		}
	}
}
