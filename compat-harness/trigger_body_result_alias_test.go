package compat

import "testing"

// TestTriggerBodyResultAlias tests select-list aliases in trigger bodies with
// WHERE, GROUP BY, and HAVING clauses.
func TestTriggerBodyResultAlias(t *testing.T) {
	t.Run("groupby-alias-over-indexed-table", func(t *testing.T) {
		flLockstep(t, "groupby-alias-over-indexed-table", []string{
			`CREATE TABLE t1(a UNIQUE, b)`,
			`CREATE TABLE t2(p, q)`,
			`INSERT INTO t1 VALUES(1,10),(2,20)`,
			`CREATE TRIGGER tr AFTER INSERT ON t2 BEGIN` +
				` INSERT INTO t1(a,b) SELECT b AS z, count(*) FROM t1 GROUP BY z HAVING z IS NOT NULL; END`,
			`INSERT INTO t2 VALUES(1,2)`,
		}, `SELECT a, b FROM t1 ORDER BY rowid`)
	})

	t.Run("where-alias", func(t *testing.T) {
		flLockstep(t, "where-alias", []string{
			`CREATE TABLE s(a, b)`,
			`CREATE TABLE fire(p)`,
			`CREATE TABLE dst(v)`,
			`INSERT INTO s VALUES(1,10),(2,20),(3,30)`,
			`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN` +
				` INSERT INTO dst SELECT a+b AS sum FROM s WHERE sum > 15; END`,
			`INSERT INTO fire VALUES(1)`,
		}, `SELECT v FROM dst ORDER BY v`)
	})

	t.Run("real-column-beats-alias", func(t *testing.T) {
		flLockstep(t, "real-column-beats-alias", []string{
			`CREATE TABLE s(a, b)`,
			`CREATE TABLE fire(p)`,
			`CREATE TABLE dst(v)`,
			`INSERT INTO s VALUES(1,10),(2,20),(3,30)`,
			`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN` +
				` INSERT INTO dst SELECT a+100 AS a FROM s WHERE a>1; END`,
			`INSERT INTO fire VALUES(1)`,
		}, `SELECT v FROM dst ORDER BY v`)
	})

	t.Run("alias-named-like-a-pseudo-row-column", func(t *testing.T) {
		flLockstep(t, "alias-named-like-a-pseudo-row-column", []string{
			`CREATE TABLE s(a)`,
			`CREATE TABLE fire(p)`,
			`CREATE TABLE dst(v, w)`,
			`INSERT INTO s VALUES(1),(2),(3)`,
			`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN` +
				` INSERT INTO dst SELECT a*10 AS p, new.p FROM s WHERE p > 15; END`,
			`INSERT INTO fire VALUES(7)`,
		}, `SELECT v, w FROM dst ORDER BY v`)
	})

	t.Run("groupby-alias-and-ordinal", func(t *testing.T) {
		flLockstep(t, "groupby-alias-and-ordinal", []string{
			`CREATE TABLE t1(a UNIQUE, b)`,
			`CREATE TABLE fire(p)`,
			`CREATE TABLE dst(k, n)`,
			`INSERT INTO t1 VALUES(1,10),(2,10),(3,20)`,
			`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN` +
				` INSERT INTO dst SELECT b AS z, count(*) FROM t1 GROUP BY z;` +
				` INSERT INTO dst SELECT b, count(*) FROM t1 GROUP BY 1; END`,
			`INSERT INTO fire VALUES(1)`,
		}, `SELECT k, n FROM dst ORDER BY rowid`)
	})
}
