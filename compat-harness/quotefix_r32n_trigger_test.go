package compat

import "testing"

// TestR32NQuotefixTrigger verifies trigger quoting in ALTER TABLE RENAME.
func TestR32NQuotefixTrigger(t *testing.T) {
	differ(t, "r32n-trig-when-and-values", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 WHEN new.a <> "zz" BEGIN INSERT INTO log VALUES("lit"); END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-update-step", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(p, q)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN UPDATE t2 SET q = "p" WHERE q = "zz"; END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-delete-step", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(p, q)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN DELETE FROM t2 WHERE "p" = "zz" AND "q" IS NOT NULL; END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-insert-select-step", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE t2(p, q)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log SELECT "p" FROM t2 WHERE "zz" IS NULL; END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-insert-column-list-untouched", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log("m") VALUES("lit"); END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-select-step", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN SELECT RAISE(ABORT, "boom") WHERE "zz" IS NULL; END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-temp-trigger", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE log(m)`,
		`CREATE TEMP TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES("lit"); END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT sql FROM temp.sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-update-of-list", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tr AFTER UPDATE OF a ON t1 WHEN "zz" IS NULL BEGIN INSERT INTO log VALUES(old.b); END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-drop-column", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES("lit"); END`,
		`ALTER TABLE t1 DROP COLUMN b`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r32n-trig-fires-after-quotefix", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 WHEN new.a <> "zz" BEGIN INSERT INTO log VALUES("lit"); END`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`INSERT INTO t1 VALUES('x', 1, 2)`,
		`INSERT INTO t1 VALUES('zz', 1, 2)`,
		`SELECT m FROM log`,
	})
}
