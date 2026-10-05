// INSERT INTO with target aliases: the alias replaces the table name in
// ON CONFLICT...DO UPDATE SET/WHERE, but NOT in RETURNING.
package compat

import "testing"

func TestInsertTargetAlias(t *testing.T) {
	differ(t, "insert target alias in DO UPDATE", []string{
		`CREATE TABLE t1(a INT PRIMARY KEY, b, c DEFAULT 0)`,
		`INSERT INTO t1 VALUES(1,2,3)`,
		`INSERT INTO t1 AS t2(a,b) VALUES(1,8) ON CONFLICT(a) DO UPDATE SET b=excluded.b, c=t2.c+1`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
		`INSERT INTO t1 AS z(a,b) VALUES(1,77) ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE z.c<100`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
		`INSERT INTO t1 AS z(a,b) VALUES(1,78) ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE z.c>100`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
		`INSERT INTO t1 AS z(a,b) VALUES(1,88) ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE t1.c<100`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
	})
	differ(t, "insert target alias without an upsert", []string{
		`CREATE TABLE t1(a INT PRIMARY KEY, b, c DEFAULT 0)`,
		`INSERT INTO t1 AS x(a,b) VALUES(5,6)`,
		`INSERT INTO t1 AS y VALUES(9,9,9)`,
		`INSERT INTO t1 AS x DEFAULT VALUES`,
		`CREATE TABLE t9(k)`,
		`INSERT INTO t9 AS q SELECT 5`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
		`SELECT k FROM t9`,
	})
	differ(t, "insert target alias frees the excluded pseudo-table", []string{
		`CREATE TABLE excluded(a INT PRIMARY KEY, b, c DEFAULT 0)`,
		`INSERT INTO excluded VALUES(1,2,3)`,
		`INSERT INTO excluded AS base(a,b) VALUES(1,7) ON CONFLICT(a) DO UPDATE SET c=excluded.c+1`,
		`SELECT a,b,c FROM excluded`,
		`INSERT INTO excluded AS base(a,b) VALUES(1,7) ON CONFLICT(a) DO UPDATE SET c=base.c+10`,
		`SELECT a,b,c FROM excluded`,
	})
	differ(t, "insert target alias spellings", []string{
		`CREATE TABLE t1(a INT PRIMARY KEY, b)`,
		`INSERT INTO t1 x VALUES(1,2)`,
		`INSERT INTO t1 AS "my alias"(a,b) VALUES(2,3) ON CONFLICT(a) DO UPDATE SET b=1`,
		`INSERT INTO t1 AS t1(a,b) VALUES(2,9) ON CONFLICT(a) DO UPDATE SET b=t1.b+1`,
		`SELECT a,b FROM t1 ORDER BY a`,
	})
	differ(t, "insert target alias does not reach RETURNING", []string{
		`CREATE TABLE t1(a INT PRIMARY KEY, b, c DEFAULT 0)`,
		`INSERT INTO t1 AS x VALUES(1,2,3) RETURNING x.a, x.b`,
		`INSERT INTO t1 AS x VALUES(2,2,3) RETURNING t1.a`,
		`INSERT INTO t1 AS x VALUES(3,2,3) RETURNING a`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
	})
}

// TestUpsertReturningNoLongerSilentlyDropped tests that invalid RETURNING
// columns are rejected at prepare time, not silently ignored.
func TestUpsertReturningNoLongerSilentlyDropped(t *testing.T) {
	differ(t, "upsert with an invalid RETURNING column", []string{
		`CREATE TABLE t1(a INT PRIMARY KEY, b, c DEFAULT 0)`,
		`INSERT INTO t1 VALUES(1,2,3)`,
		`INSERT INTO t1(a,b) VALUES(1,9) ON CONFLICT(a) DO UPDATE SET c=t1.c+1 RETURNING zzz.a`,
		`INSERT INTO t1 AS x(a,b) VALUES(1,9) ON CONFLICT(a) DO UPDATE SET c=x.c+1 RETURNING zzz.a`,
		`SELECT a,b,c FROM t1`,
	})
	differ(t, "upsert with a valid RETURNING column", []string{
		`CREATE TABLE t1(a INT PRIMARY KEY, b, c DEFAULT 0)`,
		`INSERT INTO t1 VALUES(1,2,3)`,
		`INSERT INTO t1(a,b) VALUES(1,9) ON CONFLICT(a) DO UPDATE SET c=t1.c+1 RETURNING t1.a`,
		`SELECT a,b,c FROM t1`,
	})
	differ(t, "aliased upsert with a valid RETURNING column", []string{
		`CREATE TABLE t1(a INT PRIMARY KEY, b, c DEFAULT 0)`,
		`INSERT INTO t1 VALUES(1,2,3)`,
		`INSERT INTO t1 AS x(a,b) VALUES(1,9) ON CONFLICT(a) DO UPDATE SET c=x.c+1 RETURNING a`,
		`SELECT a,b,c FROM t1`,
	})
}
