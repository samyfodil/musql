package compat

import "testing"

// TestCompoundMergeArmOrderAsC: the arms of a UNION, INTERSECT or EXCEPT are
// planned under the ORDER BY multiSelectByMerge invents, every result column
// under the compound's collating sequence, and answer as C does: TPC-DS q75's
// three-way UNION of joins, ANALYZEd, NOCASE and BINARY columns, a "*" arm,
// aggregate and GROUP BY arms, a UNION ALL run after a UNION, an EXISTS body,
// a correlated arm, and values equal under the compound's comparison but
// not alike (1 and 1.0, 'a' and 'A'), where which one a UNION keeps depends on
// the order the arm's rows arrive in.
func TestCompoundMergeArmOrderAsC(t *testing.T) {
	differ(t, "compound-merge-order", []string{
		`CREATE TABLE dd(d_sk INTEGER, d_year INTEGER)`,
		`CREATE TABLE it(i_sk INTEGER, i_brand INTEGER, i_cat TEXT, i_name TEXT COLLATE NOCASE)`,
		`CREATE TABLE cs(c_date INTEGER, c_item INTEGER, c_order INTEGER, c_qty, c_price REAL)`,
		`CREATE TABLE cr(r_item INTEGER, r_order INTEGER, r_qty INTEGER, r_amt REAL)`,
		`CREATE TABLE ws(w_date INTEGER, w_item INTEGER, w_order INTEGER, w_qty, w_price REAL)`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 6000) INSERT INTO dd SELECT value, 1998 + value % 5 FROM g WHERE value <= 400`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 6000) INSERT INTO it SELECT value, value % 7, CASE value % 3 WHEN 0 THEN 'Books' ELSE 'Music' END,
			CASE value % 4 WHEN 0 THEN 'a' WHEN 1 THEN 'A' WHEN 2 THEN 'b' ELSE 'B' END FROM g WHERE value <= 120`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 6000) INSERT INTO cs SELECT value % 400 + 1, value % 120 + 1, value, CASE value % 5 WHEN 0 THEN 1.0 ELSE value % 9 END, value / 4.0
			FROM g WHERE value <= 6000`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 6000) INSERT INTO cr SELECT value % 120 + 1, value * 7, value % 4, value / 8.0 FROM g WHERE value <= 900`,
		`WITH RECURSIVE g(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM g WHERE value < 6000) INSERT INTO ws SELECT value % 400 + 1, value % 120 + 1, value, CASE value % 4 WHEN 0 THEN 1 ELSE value % 9 END, value / 2.0
			FROM g WHERE value <= 3000`,
		`ANALYZE`,
		`SELECT d_year, i_brand, sum(q), count(*) FROM (
			SELECT d_year, i_brand, c_qty - COALESCE(r_qty, 0) AS q FROM cs JOIN it ON i_sk = c_item JOIN dd ON d_sk = c_date
				LEFT JOIN cr ON c_order = r_order AND c_item = r_item WHERE i_cat = 'Books'
			UNION SELECT d_year, i_brand, w_qty FROM ws JOIN it ON i_sk = w_item JOIN dd ON d_sk = w_date WHERE i_cat = 'Books'
			UNION SELECT d_year, i_brand, c_qty FROM cs JOIN it ON i_sk = c_item JOIN dd ON d_sk = c_date WHERE i_cat = 'Music'
		) GROUP BY 1, 2 ORDER BY 1, 2`,
		`SELECT i_name, c_qty FROM cs JOIN it ON i_sk = c_item WHERE c_order < 40
			UNION SELECT i_name, w_qty FROM ws JOIN it ON i_sk = w_item WHERE w_order < 40`,
		`SELECT typeof(q), q FROM (SELECT c_qty AS q FROM cs JOIN dd ON d_sk = c_date WHERE c_order % 50 = 0
			UNION SELECT w_qty FROM ws JOIN dd ON d_sk = w_date WHERE w_order % 50 = 0)`,
		`SELECT i_name FROM it JOIN cs ON c_item = i_sk WHERE c_order < 30 INTERSECT SELECT i_name FROM it JOIN ws ON w_item = i_sk`,
		`SELECT i_name, i_brand FROM it JOIN cs ON c_item = i_sk WHERE c_order < 300 EXCEPT SELECT i_name, i_brand FROM it JOIN ws ON w_item = i_sk WHERE w_order < 100`,
		`SELECT * FROM it JOIN dd ON d_sk = i_sk WHERE i_brand = 3 UNION SELECT * FROM it JOIN dd ON d_sk = i_sk + 1 WHERE i_brand = 4`,
		`SELECT d_year, count(*) FROM cs JOIN dd ON d_sk = c_date GROUP BY d_year UNION SELECT d_year, count(*) FROM ws JOIN dd ON d_sk = w_date GROUP BY d_year`,
		`SELECT max(c_price) FROM cs JOIN it ON i_sk = c_item UNION SELECT min(w_price) FROM ws JOIN it ON i_sk = w_item`,
		`SELECT i_brand FROM it JOIN cs ON c_item = i_sk WHERE c_order < 20 UNION SELECT i_brand FROM it JOIN ws ON w_item = i_sk WHERE w_order < 20
			UNION ALL SELECT d_year FROM dd WHERE d_sk < 4`,
		`SELECT i_brand, i_name FROM it JOIN cs ON c_item = i_sk WHERE c_order < 50 UNION SELECT i_brand, i_name FROM it JOIN ws ON w_item = i_sk WHERE w_order < 50 LIMIT 5`,
		`SELECT count(*) FROM it WHERE EXISTS (SELECT 1 FROM cs JOIN dd ON d_sk = c_date WHERE c_item = it.i_sk AND d_year = 2000
			UNION SELECT 1 FROM ws JOIN dd ON d_sk = w_date WHERE w_item = it.i_sk AND d_year = 2001)`,
		`SELECT i_sk, (SELECT count(*) FROM (SELECT c_qty FROM cs JOIN dd ON d_sk = c_date WHERE c_item = it.i_sk
			UNION SELECT w_qty FROM ws JOIN dd ON d_sk = w_date WHERE w_item = it.i_sk)) FROM it WHERE i_sk < 15`,
	})
}
