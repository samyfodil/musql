package compat

import "testing"

// TestSortedSeekIntConstAsC: TPC-DS q61's shape, an inner table sought on one
// INTEGER constant for every outer row, answers as C does: a TEXT column
// whose keys are '-5.00' and '-5', a column of no type holding INTEGERs,
// REALs and TEXTs, a constant no row holds, in a LEFT JOIN and under a GROUP
// BY.
func TestSortedSeekIntConstAsC(t *testing.T) {
	const q = `SELECT count(*), sum(q) FROM f, cd, st WHERE f.cd = cd.k AND f.s = st.sk AND cd.g = 'M' AND `
	differ(t, "sorted-seek-int-const", []string{
		`CREATE TABLE cd(k INTEGER, g TEXT)`,
		`CREATE TABLE st(sk INTEGER, off VARCHAR, mix)`,
		`CREATE TABLE f(cd INTEGER, s INTEGER, q INTEGER)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 3000) INSERT INTO cd SELECT v, CASE v % 3 WHEN 0 THEN 'M' ELSE 'F' END FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 40) INSERT INTO st SELECT v,
			CASE v % 4 WHEN 0 THEN '-5.00' WHEN 1 THEN NULL WHEN 2 THEN '-5' ELSE '-6' END,
			CASE v % 5 WHEN 0 THEN -5 WHEN 1 THEN '-5' WHEN 2 THEN -5.0 WHEN 3 THEN NULL ELSE 'x' END FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 30000) INSERT INTO f SELECT v % 3001, v % 41, v % 13 FROM g`,
		`ANALYZE`,
		`VACUUM`,
		q + `st.off = -5`,
		q + `st.off = -6`,
		q + `st.off = 7`,
		q + `st.mix = -5`,
		q + `st.mix = 5 - 10`,
		q + `st.off = -5 AND st.mix = -5`,
		`SELECT count(*), count(st.sk), sum(q) FROM f, cd LEFT JOIN st ON f.s = st.sk AND st.off = -5 WHERE f.cd = cd.k AND cd.g = 'M'`,
		`SELECT st.off, count(*) FROM cd, st WHERE cd.g = 'M' AND st.off = -6 AND cd.k % 7 = st.sk % 7 GROUP BY 1 ORDER BY 1`,
		`SELECT count(*) FROM (SELECT f.q FROM f, cd, st WHERE f.cd = cd.k AND f.s = st.sk AND st.off = -5 AND cd.g = 'M')`,
	})
}
