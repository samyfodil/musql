package compat

import "testing"

// TestParseR31AlterEmptyInOperand tests ALTER TABLE RENAME COLUMN with
// empty IN() operands and different expression types (CAST, COLLATE).
func TestParseR31AlterEmptyInOperand(t *testing.T) {
	differ(t, "r31-alterin-check-cast", []string{
		`CREATE TABLE t1(a, b, CHECK(CAST(b AS INT) IN ()))`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1 VALUES(1,2)`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r31-alterin-check-collate", []string{
		`CREATE TABLE t1(a, b, CHECK(abs(b) COLLATE nocase NOT IN ()))`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1 VALUES(1,2)`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r31-alterin-check-plain-func", []string{
		`CREATE TABLE t1(a, b, CHECK(abs(b) NOT IN ()))`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1 VALUES(1,2)`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r31-alterin-check-like", []string{
		`CREATE TABLE t1(a, b, CHECK((b LIKE 'x') NOT IN ()))`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1 VALUES(1,2)`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r31-alterin-check-glob-regexp", []string{
		`CREATE TABLE t1(a, b, CHECK((b GLOB 'x') NOT IN ()))`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
	})
	differ(t, "r31-alterin-index-cast", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE INDEX i1 ON t1(a) WHERE CAST(b AS INT) IN ()`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-alterin-index-collate", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE INDEX i1 ON t1(a) WHERE abs(b) COLLATE nocase IN ()`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-alterin-index-plain", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE INDEX i1 ON t1(a) WHERE b IN ()`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	// "cast(b)" with no AS is a CALL, so the operand survives and IS rewritten.
	differ(t, "r31-alterin-check-cast-as-function", []string{
		`CREATE TABLE t1(a, b, CHECK(nosuchfn(b) IN ()))`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
	})
}
