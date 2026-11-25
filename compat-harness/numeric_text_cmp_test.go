package compat

import "testing"

// TestNumericTextCmpAsC: TPC-DS q16's d_date BETWEEN '2002-02-01' AND
// CAST('2002-04-02' AS date) answers as C does, with CASTs of literals to
// NUMERIC, INTEGER and REAL, a CAST whose operand a CASE chooses, and numbers
// against TEXTs under numeric affinity and none: TEXTs that are numbers,
// TEXTs that are not, REALs, NULLs and BLOBs, the number on either side.
func TestNumericTextCmpAsC(t *testing.T) {
	differ(t, "numeric-text-cmp", []string{
		`CREATE TABLE dd(sk INTEGER, d date, txt TEXT, num INTEGER, r REAL, any_)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 20000) INSERT INTO dd SELECT v,
			printf('%04d-%02d-%02d', 1998 + v % 8, 1 + v % 12, 1 + v % 28),
			CASE v % 9 WHEN 0 THEN '12' WHEN 1 THEN ' 7 ' WHEN 2 THEN '1e3' WHEN 3 THEN '-.5' WHEN 4 THEN 'abc'
				WHEN 5 THEN NULL WHEN 6 THEN '2002-03-01' WHEN 7 THEN X'3132' ELSE '' END,
			CASE v % 7 WHEN 0 THEN NULL ELSE v % 2005 END, v / 3.0,
			CASE v % 5 WHEN 0 THEN '12' WHEN 1 THEN 12 WHEN 2 THEN 'x' WHEN 3 THEN 2.5 ELSE NULL END FROM g`,
		`INSERT INTO dd VALUES (20001, 2002, '2002', 2002, 2002.0, '2002'), (20002, NULL, NULL, NULL, NULL, NULL),
			(20003, X'00', '9 9', 1, -0.0, X'00')`,
		`ANALYZE`,
		`SELECT count(*), sum(sk) FROM dd WHERE d BETWEEN '2002-02-01' AND CAST('2002-04-02' AS date)`,
		`SELECT count(*), sum(sk) FROM dd WHERE d <= CAST('2002-04-02' AS date)`,
		`SELECT count(*) FROM dd WHERE d > CAST('2001.5' AS NUMERIC) AND d < CAST('2003x' AS INTEGER)`,
		`SELECT count(*) FROM dd WHERE txt >= CAST('5' AS REAL)`,
		`SELECT count(*) FROM dd WHERE any_ < CAST('7' AS date)`,
		`SELECT count(*) FROM dd WHERE num = CAST(CASE WHEN sk % 2 = 0 THEN '1' ELSE '2' END AS INTEGER)`,
		`SELECT count(*), sum(txt < num), sum(txt >= num), sum(num > txt), sum(txt = num), sum(txt <> r), sum(r <= txt) FROM dd`,
		`SELECT count(*), sum(any_ < num), sum(any_ > r), sum(num <= any_), sum(any_ = 12) FROM dd`,
		`SELECT sk, txt < num, txt > r, d >= 2002, any_ > 3 FROM dd WHERE sk % 997 = 0 OR sk > 20000 ORDER BY sk`,
	})
}
