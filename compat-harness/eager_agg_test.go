package compat

import "testing"

// TestEagerAggregateAsC: #400. A fanning-out join under count/min/max, which
// musql joins per key (engine/eager_agg.go), answers as C's join does: NULL
// keys and values, a dim with duplicate keys, a TEXT-keyed dim, a NOCASE
// column whose min/max differ from BINARY's, an indexed fact, HAVING, ORDER
// BY and LIMIT over the rebuilt aggregates, three facts, and every shape the
// rewrite refuses (sum/avg/total/group_concat, DISTINCT, FILTER, an outer
// join, a cross-table inequality, an untyped key meeting a TEXT one, a NOCASE
// key, a window, a selective star join, a missing column), and a dim larger
// than the facts' key count.
// The issue's own tables are scaled down to 20,000 rows.
func TestEagerAggregateAsC(t *testing.T) {
	stmts := []string{
		`CREATE TABLE fa(k INTEGER, x INTEGER, s TEXT COLLATE NOCASE, r REAL)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 2000) INSERT INTO fa SELECT CASE WHEN value % 97 = 0 THEN NULL ELSE value % 53 END, CASE WHEN value % 7 = 0 THEN NULL ELSE value % 11 END, CASE WHEN value % 26 < 13 THEN char(97 + value % 26) ELSE char(65 + value % 26) END, (value % 100) / 8.0 FROM g`,
		`CREATE TABLE fb(k INT, y REAL, t TEXT)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 2000) INSERT INTO fb SELECT value % 47, (value * 7 % 100) / 3.0, nullif(char(97 + value % 5), 'c') FROM g`,
		`CREATE TABLE fc(k INTEGER, z INTEGER)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 1500) INSERT INTO fc SELECT value % 31, CASE WHEN value % 5 = 0 THEN NULL ELSE value % 13 END FROM g`,
		`CREATE TABLE d(k INTEGER, g TEXT, h INT)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 66) INSERT INTO d SELECT value % 60, 'g' || (value % 7), value % 5 FROM g`,
		`CREATE TABLE dt(k TEXT, g TEXT)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 60) INSERT INTO dt SELECT value % 60, 'g' || (value % 4) FROM g`,
		`CREATE TABLE fu(k, v)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 2000) INSERT INTO fu SELECT CASE WHEN value % 9 = 0 THEN (value % 53) * 1.0 ELSE value % 53 END, value FROM g`,
		`CREATE TABLE fu2(k, v)`,
		`INSERT INTO fu2 SELECT * FROM fu`,
		`CREATE TABLE fn(k TEXT COLLATE NOCASE, v)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 2000) INSERT INTO fn SELECT CASE WHEN value % 2 THEN 'k' ELSE 'K' END || (value % 50), value FROM g`,
		`CREATE TABLE dn(k TEXT, g TEXT)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 49) INSERT INTO dn SELECT 'k' || value, 'g' || (value % 3) FROM g`,
		`CREATE TABLE fia(k INTEGER, x INTEGER)`,
		`INSERT INTO fia SELECT k, x FROM fa`,
		`CREATE INDEX fia_k ON fia(k)`,
		`CREATE TABLE fib(k INTEGER, y REAL)`,
		`INSERT INTO fib SELECT k, y FROM fb`,
		`CREATE INDEX fib_k ON fib(k)`,
		`CREATE TABLE d2(k INTEGER, g TEXT)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 2000) INSERT INTO d2 SELECT value % 60, 'g' || (value % 3) FROM g`,
	}
	queries := []string{
		`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) AS n, count(a.x), count(b.t), max(b.y), min(b.y), max(a.s), min(a.s), max(a.r), min(d.h), count(d.h) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.h, count(*), max(a.x) FROM fa a, d, fb b WHERE a.k = d.k AND b.k = d.k AND a.x > 5 AND b.y < 30 GROUP BY d.h HAVING count(*) > 10 ORDER BY 2 DESC, 1`,
		`SELECT d.g, d.h, count(*) c FROM d JOIN fa a ON a.k = d.k JOIN fb b ON b.k = a.k GROUP BY d.g, d.h ORDER BY c DESC, d.g, d.h LIMIT 5 OFFSET 2`,
		`SELECT dt.g, count(*), max(b.y) FROM fa a JOIN dt USING (k) JOIN fb b USING (k) GROUP BY dt.g`,
		`SELECT d.g, count(*), count(c.z), max(c.z), min(a.s) FROM fa a JOIN d USING (k) JOIN fb b USING (k) JOIN fc c USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE d.h > 2 GROUP BY d.g`,
		`SELECT upper(d.g) || '!', count(*) + 1, max(b.y) * 2 FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING max(b.y) > 1 AND d.g <> 'g3'`,
		`SELECT d.g, count(*) FROM fia a JOIN d USING (k) JOIN fib b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.k IS NOT NULL AND b.t IS NOT NULL GROUP BY d.g ORDER BY count(*), d.g`,
		`SELECT d.g, max(a.x), count(*) FROM d JOIN fa a ON a.k = d.k AND a.x BETWEEN 2 AND 8 JOIN fb b ON b.k = d.k AND b.t IN ('a', 'b') GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.r > 100 GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fa a JOIN d ON a.k = d.k JOIN fb b ON b.k = d.h GROUP BY d.g`,
		`SELECT d.g AS gg, count(*) AS n, max(b.y) AS m FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING n > 1000 AND gg <> 'g1' ORDER BY m, gg`,
		`SELECT d.h, d.g, count(*) FROM fa JOIN d ON fa.k = d.k JOIN fb ON fb.k = fa.k GROUP BY d.g`,
		`SELECT d.g, count(*), avg(b.y) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, sum(a.x) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, total(b.y), group_concat(b.t) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(DISTINCT a.x) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) FILTER (WHERE a.x > 3) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, max(a.x + 1) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, max(a.s COLLATE BINARY) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) FROM d LEFT JOIN fa a USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.x < d.h GROUP BY d.g`,
		`SELECT dt.g, count(*) FROM dt JOIN fu ON fu.k = dt.k JOIN fu2 ON fu2.k = dt.k GROUP BY dt.g`,
		`SELECT dn.g, count(*) FROM fn x JOIN dn USING (k) JOIN fn y USING (k) GROUP BY dn.g`,
		`SELECT d.g, count(*) OVER () FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT DISTINCT d.h, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fia a JOIN d USING (k) JOIN fib b USING (k) WHERE d.h = 1 GROUP BY d.g`,
		`SELECT d2.g, count(*) FROM fa a JOIN d2 USING (k) JOIN fb b USING (k) GROUP BY d2.g`,
		`SELECT a.x, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY a.x`,
		`SELECT count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k)`,
		`SELECT k, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY k`,
		`SELECT d.g, count(a.nope) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE abs(a.x) > 2 GROUP BY d.g`,
		`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING (SELECT 1)`,
		`SELECT d.g, count(*) FROM fa a JOIN d ON a.k = d.k JOIN fb b ON b.k = a.x GROUP BY d.g`,
		`SELECT d.g, count(*) AS n FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING nope > 1`,
	}
	for _, q := range queries {
		differ(t, "eager-agg", append(append([]string{}, stmts...), q))
	}
}

// TestEagerAggregateIssueAsC: #400's tables, scaled to 20,000 rows: the
// count(*)/max form is joined per key, also as a subquery's body; the avg
// form is not.
func TestEagerAggregateIssueAsC(t *testing.T) {
	stmts := []string{
		`CREATE TABLE fact_a AS WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 20000) SELECT cast(min(399, FLOOR(400 * POW(value*1.0 / 20000, 3.74))) as int) AS keyid FROM g`,
		`CREATE TABLE fact_b AS WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 20000) SELECT cast(min(399, FLOOR(400 * POW(value*1.0 / 20000, 3.74))) as int) AS keyid, (((value * 13) % 1000)*1.0 / 7.0) AS measure FROM g`,
		`CREATE TABLE dim AS WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 400) SELECT value AS keyid, (value % 100) AS bucket FROM g`,
	}
	for _, q := range []string{
		`SELECT d.bucket, count(*) AS cnt, max(b.measure) FROM fact_a a JOIN dim d USING (keyid) JOIN fact_b b USING (keyid) GROUP BY d.bucket`,
		`SELECT d.bucket, count(*) AS cnt, min(b.measure), count(b.measure) FROM fact_a a JOIN dim d USING (keyid) JOIN fact_b b USING (keyid) GROUP BY d.bucket HAVING cnt > 1000 ORDER BY cnt DESC`,
		`SELECT d.bucket, count(*) AS cnt, avg(b.measure) FROM fact_a a JOIN dim d USING (keyid) JOIN fact_b b USING (keyid) GROUP BY d.bucket`,
		`SELECT count(*), max(cnt), min(m) FROM (SELECT d.bucket, count(*) AS cnt, max(b.measure) AS m FROM fact_a a JOIN dim d USING (keyid) JOIN fact_b b USING (keyid) GROUP BY d.bucket)`,
		`SELECT (SELECT max(cnt) FROM (SELECT d.bucket, count(*) AS cnt FROM fact_a a JOIN dim d USING (keyid) JOIN fact_b b USING (keyid) GROUP BY d.bucket)) AS v`,
	} {
		differ(t, "eager-agg-issue", append(append([]string{}, stmts...), q))
	}
}

// TestEagerAggregateCollationAsC: min/max of a NOCASE or RTRIM fact column
// compare under its collation, but their result carries none, so HAVING and
// ORDER BY compare it under BINARY. And the per-key join reaches min/max in an
// order of its own, so compare-equal values with different bytes (texts under
// NOCASE, 1 and 1.0, the integer and the REAL -2^63, 0.0 and -0.0) decline
// rather than take whichever came first. A REAL column without -0.0 is joined
// per key.
func TestEagerAggregateCollationAsC(t *testing.T) {
	stmts := []string{
		`CREATE TABLE d(k INTEGER, g TEXT, u, n TEXT COLLATE NOCASE)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 19) INSERT INTO d SELECT value, 'g' || (value % 3), CASE WHEN value % 2 THEN 1 ELSE 1.0 END, CASE WHEN value % 2 THEN 'x' ELSE 'X' END FROM g`,
		`CREATE TABLE fa(k INTEGER, x INTEGER, s TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM, v, i INTEGER, w, z REAL)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 399) INSERT INTO fa SELECT value % 20, value, CASE WHEN value % 20 < 7 THEN 'a' ELSE 'B' END, CASE WHEN value % 3 = 0 THEN 'b ' ELSE 'b' END, CASE WHEN value % 3 THEN 5 ELSE 5.0 END, CASE WHEN value % 3 THEN -9223372036854775808 ELSE -9223372036854775808.0 END, CASE WHEN value % 3 THEN 0.0 ELSE -0.0 END, (value % 7) / 2.0 - 1 FROM g`,
		`CREATE TABLE fm(k INTEGER, s TEXT COLLATE NOCASE)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 399) INSERT INTO fm SELECT value % 20, char(CASE value % 3 WHEN 0 THEN 97 WHEN 1 THEN 65 ELSE 98 END) FROM g`,
		`CREATE TABLE fb(k INTEGER, y)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 399) INSERT INTO fb SELECT value % 20, value FROM g`,
	}
	for _, q := range []string{
		`SELECT d.k, min(a.s) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k HAVING min(a.s) < 'B'`,
		`SELECT d.k, min(a.s) AS m FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k HAVING m < 'B'`,
		`SELECT d.k, min(a.s) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k ORDER BY min(a.s), d.k`,
		`SELECT d.k, max(a.s) AS m FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k ORDER BY m DESC, d.k`,
		`SELECT d.g, max(a.s) FROM fm a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING max(a.s) = 'B'`,
		`SELECT d.g, max(a.s), min(a.s) FROM fm a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING min(a.s) = 'A'`,
		`SELECT d.g, max(a.r) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING max(a.r) = 'b'`,
		`SELECT d.g, max(a.v), typeof(max(a.v)) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, min(a.i), typeof(min(a.i)) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, min(d.u), typeof(min(d.u)), count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.u, typeof(d.u), count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.u`,
		`SELECT d.n, count(*), max(b.y) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.n`,
		`SELECT d.n, count(*) FROM d JOIN fa a USING (k) JOIN fb b USING (k) GROUP BY d.n`,
		`SELECT d.g, max(a.w), min(a.w), count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.k, min(a.z), max(a.z), count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k`,
	} {
		differ(t, "eager-agg-collation", append(append([]string{}, stmts...), q))
	}
}

// TestEagerAggregateFanoutAsC: facts with many keys, or a key per row, over a
// small dim are left to the join; facts with few keys are joined per key, also
// against a dim with more rows than they have keys.
func TestEagerAggregateFanoutAsC(t *testing.T) {
	stmts := []string{
		`CREATE TABLE d(k INTEGER, g TEXT)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 99) INSERT INTO d SELECT value * 7, 'g' || (value % 10) FROM g`,
		`CREATE TABLE ma(k INTEGER, x INTEGER)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 19999) INSERT INTO ma SELECT value % 5000, value FROM g`,
		`CREATE TABLE mb(k INTEGER, y INTEGER)`,
		`INSERT INTO mb SELECT * FROM ma`,
		`CREATE TABLE ua(k INTEGER, x INTEGER)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 19999) INSERT INTO ua SELECT value, value FROM g`,
		`CREATE TABLE ub(k INTEGER, y INTEGER)`,
		`INSERT INTO ub SELECT * FROM ua`,
		`CREATE TABLE fa(k INTEGER, x INTEGER)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 3999) INSERT INTO fa SELECT value % 100 * 7, value FROM g`,
		`CREATE TABLE fb(k INTEGER, y INTEGER)`,
		`INSERT INTO fb SELECT * FROM fa`,
		`CREATE TABLE big(k INTEGER, g TEXT)`,
		`WITH RECURSIVE g(value) AS (SELECT 0 UNION ALL SELECT value + 1 FROM g WHERE value < 3999) INSERT INTO big SELECT value % 700, 'g' || (value % 10) FROM g`,
	}
	for _, q := range []string{
		`SELECT d.g, count(*), max(b.y) FROM d JOIN ma a USING (k) JOIN mb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*), max(b.y) FROM ma a JOIN d USING (k) JOIN mb b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*), max(b.y) FROM ua a JOIN d USING (k) JOIN ub b USING (k) GROUP BY d.g`,
		`SELECT d.g, count(*), max(b.y) FROM d JOIN fa a USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT big.g, count(*), max(b.y), min(a.x) FROM fa a JOIN big USING (k) JOIN fb b USING (k) GROUP BY big.g`,
	} {
		differ(t, "eager-agg-fanout", append(append([]string{}, stmts...), q))
	}
}
