package compat

import "testing"

// TestTextCastConstAsC: #393's d_date BETWEEN '2002-02-01' AND
// CAST('2002-04-01' AS varchar) answers as C does, with the other spellings
// of TEXT affinity, a cast whose operand a CASE chooses, a number spelled as
// TEXT against INTEGERs, TEXTs, NULLs and BLOBs, and the cast on either side.
func TestTextCastConstAsC(t *testing.T) {
	differ(t, "text-cast-const", []string{
		`CREATE TABLE dd(sk INTEGER, d varchar, n INTEGER, any_)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 20000) INSERT INTO dd SELECT v,
			printf('%04d-%02d-%02d', 1998 + v % 8, 1 + v % 12, 1 + v % 28), v % 50,
			CASE v % 5 WHEN 0 THEN '12' WHEN 1 THEN 12 WHEN 2 THEN 'x' WHEN 3 THEN 2.5 ELSE NULL END FROM g`,
		`INSERT INTO dd VALUES (20001, 12, 1, X'00'), (20002, NULL, 2, NULL), (20003, X'32303032', 3, '7'), (20004, '2002', 7, 7)`,
		`SELECT count(*), sum(sk) FROM dd WHERE d BETWEEN '2002-02-01' AND CAST('2002-04-01' AS varchar)`,
		`SELECT count(*) FROM dd WHERE d <= CAST('2002-04-01' AS TEXT) AND d > CAST('2000' AS CLOB)`,
		`SELECT count(*) FROM dd WHERE CAST('2001-06-15' AS char(10)) < d`,
		`SELECT count(*) FROM dd WHERE d = CAST('12' AS TEXT) OR n = CAST('7' AS TEXT)`,
		`SELECT count(*), sum(any_ = CAST('12' AS TEXT)), sum(any_ < CAST('7' AS varchar)), sum(CAST('x' AS TEXT) >= any_) FROM dd`,
		`SELECT count(*) FROM dd WHERE d < CAST(CASE WHEN sk % 2 = 0 THEN '2000-01-01' ELSE '2003-01-01' END AS TEXT)`,
		`SELECT sk, d, d < CAST('2002' AS TEXT), CAST('x' AS TEXT) || d, typeof(CAST('5' AS TEXT)) FROM dd WHERE sk % 1999 = 0 OR sk > 20000 ORDER BY sk`,
	})
}
