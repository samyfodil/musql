package compat

import "testing"

// TestRealTextArithAsC: TPC-DS q04's year_total, arithmetic over VARCHAR
// prices, answers as C does: REAL spellings, INTEGER ones, '-0.00', spaces,
// exponents, more digits than a double holds, NULLs, a column of no type, and
// a divide by a zero spelled as TEXT.
func TestRealTextArithAsC(t *testing.T) {
	differ(t, "real-text-arith", []string{
		`CREATE TABLE ss(c INTEGER, lp VARCHAR, wc VARCHAR, dc VARCHAR, sp VARCHAR, mix)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 20000) INSERT INTO ss SELECT v % 50,
			printf('%d.%02d', v % 300, v % 100), printf('%d.%02d', v % 40, v % 97), printf('%d.%02d', v % 7, v % 13),
			printf('%d.%02d', v % 90, v % 11), CASE v % 6 WHEN 0 THEN 2 WHEN 1 THEN 2.5 WHEN 2 THEN '3.25' ELSE NULL END FROM g`,
		`INSERT INTO ss VALUES (1, '12', '3', '1', '2', '7'), (2, '-0.00', '0.00', '-0.0', '0.', '-0.00'),
			(3, ' 1.5', '1.5e2', '1.000000000000000000000001', '9007199254740993.5', 'x'),
			(4, NULL, '1.5', NULL, '2.5', NULL), (5, '.5', '-.25', '1.', '-3.', '.5')`,
		`ANALYZE`,
		`VACUUM`,
		`SELECT c, sum(((lp - wc) - dc) + sp / 2), count(*) FROM ss GROUP BY c ORDER BY c`,
		`SELECT sum(lp * wc), sum(lp - mix), sum(mix * 2), sum(wc / dc) FROM ss`,
		`SELECT c, lp - wc, lp + 0, 0 - lp, lp / sp, typeof(lp - wc) FROM ss WHERE c < 6 AND rowid > 19990 OR rowid > 20000 ORDER BY rowid`,
		`SELECT sum(CASE WHEN lp - wc > 100 THEN 1 ELSE 0 END), max(lp - wc), min(dc * sp) FROM ss`,
		`SELECT 1 / (lp - lp), lp / '0.00', -0.0 = lp - 0, lp - 0 FROM ss WHERE rowid > 20000`,
		`SELECT c, CASE WHEN sum(wc) > 0 THEN sum(lp - wc) / sum(wc) END FROM ss GROUP BY c ORDER BY 2 DESC, 1 LIMIT 5`,
	})
}
