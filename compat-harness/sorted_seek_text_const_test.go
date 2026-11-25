package compat

import "testing"

// TestSortedSeekTextConstAsC: TPC-DS q27's shape, an inner table sought on
// one TEXT constant for every outer row, answers as C does: under BINARY and
// NOCASE, for a constant no row holds, one a numeric column takes as an
// INTEGER, in a LEFT JOIN and under a GROUP BY.
func TestSortedSeekTextConstAsC(t *testing.T) {
	const q = `SELECT count(*), sum(q) FROM f, cd, st WHERE f.cd = cd.k AND f.s = st.sk AND cd.g = 'M' AND `
	differ(t, "sorted-seek-text-const", []string{
		`CREATE TABLE cd(k INTEGER, g TEXT)`,
		`CREATE TABLE st(sk INTEGER, state TEXT, nc TEXT COLLATE NOCASE, num INTEGER)`,
		`CREATE TABLE f(cd INTEGER, s INTEGER, q INTEGER)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 3000) INSERT INTO cd SELECT v, CASE v % 3 WHEN 0 THEN 'M' ELSE 'F' END FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 40) INSERT INTO st SELECT v,
			CASE v % 4 WHEN 0 THEN 'TN' WHEN 1 THEN NULL WHEN 2 THEN 'tn' ELSE 'GA' END,
			CASE v % 4 WHEN 0 THEN 'TN' WHEN 1 THEN NULL WHEN 2 THEN 'tn' ELSE 'GA' END, v % 5 FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 30000) INSERT INTO f SELECT v % 3001, v % 41, v % 13 FROM g`,
		`ANALYZE`,
		`VACUUM`,
		q + `st.state = 'TN'`,
		q + `st.nc = 'TN'`,
		q + `st.state = 'XX'`,
		q + `st.num = '3'`,
		q + `st.state = 'TN' AND st.nc = 'tn'`,
		`SELECT count(*), count(st.sk), sum(q) FROM f, cd LEFT JOIN st ON f.s = st.sk AND st.state = 'TN' WHERE f.cd = cd.k AND cd.g = 'M'`,
		`SELECT st.state, count(*) FROM cd, st WHERE cd.g = 'M' AND st.state = 'GA' AND cd.k % 7 = st.sk % 7 GROUP BY 1 ORDER BY 1`,
	})
}
