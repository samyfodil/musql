package compat

import (
	"fmt"
	"testing"
)

// TestSortedSeekNarrowsAsC: TPC-DS q88's shape, ANALYZEd: a count over a
// join whose innermost table is probed on three columns, with runs of 100
// and of 600 rows on the first, NULLs in the second, and in two layouts a
// TEXT in the third. Every probe's rows are those C's automatic index finds,
// for keys that are an INTEGER, a REAL, a TEXT, and a REAL that is no
// integer.
func TestSortedSeekNarrowsAsC(t *testing.T) {
	for _, keys := range []int{600, 100} {
		for _, text := range []bool{false, true} {
			x := "(v / 7) % 5"
			if text {
				x = "CASE WHEN v % 41 = 0 THEN 'x' ELSE (v / 7) % 5 END"
			}
			gen := `WITH RECURSIVE g(v) AS (SELECT 0 UNION ALL SELECT v + 1 FROM g WHERE v < %d) `
			schema := []string{
				`CREATE TABLE ss(tsk INTEGER, hd INTEGER, st INTEGER, v INTEGER)`,
				`CREATE TABLE tm(tsk INTEGER, hour INTEGER)`,
				`CREATE TABLE hd(hdsk INTEGER, dep INTEGER)`,
				`CREATE TABLE st(stsk INTEGER, name TEXT)`,
				fmt.Sprintf(gen+`INSERT INTO ss SELECT v %% %d, CASE WHEN v %% 53 = 0 THEN NULL ELSE (v / %d * 13) %% 4000 END, `+x+`, v FROM g WHERE v > 0`,
					60000, keys, keys),
				fmt.Sprintf(gen+`INSERT INTO tm SELECT v, v * 24 / %d FROM g`, keys-1, keys),
				fmt.Sprintf(gen+`INSERT INTO hd SELECT v, v %% 7 FROM g`, 3999),
				`INSERT INTO st VALUES (0, 'other'), (1, 'other'), (2, 'ese'), (3, 'other'), (4, 'other')`,
				`ANALYZE`, `VACUUM`,
			}
			const q = `SELECT count(*) FROM ss, hd, tm, st
WHERE ss.tsk = tm.tsk AND ss.hd = %s AND ss.st = %s AND tm.hour = 8 AND hd.dep = 3 AND st.name = 'ese'`
			var qs []string
			for _, k := range [][2]string{{`hd.hdsk`, `st.stsk`}, {`hd.hdsk * 1.0`, `st.stsk`}, {`hd.hdsk`, `CAST(st.stsk AS TEXT)`}, {`hd.hdsk + 0.5`, `st.stsk`}} {
				qs = append(qs, fmt.Sprintf(q, k[0], k[1]))
			}
			differ(t, fmt.Sprintf("sorted-seek-narrow-%d-%v", keys, text), append(schema, qs...))
		}
	}
}
