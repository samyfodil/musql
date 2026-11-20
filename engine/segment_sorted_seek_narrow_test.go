package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestSegSortedSeekNarrowsLongRuns: a sorted seek keyed on a second column
// whose first column's runs are long is served from the segments when the
// automatic index orders a run by the second column next, each seek taking
// only the stretch of its run equal on both; it answers as the seek defeated
// by a CAST does.
func TestSegSortedSeekNarrowsLongRuns(t *testing.T) {
	stmts := []string{
		`CREATE TABLE d (sk BIGINT, id VARCHAR, yr BIGINT, qoy BIGINT, pad VARCHAR)`,
		`CREATE TABLE ss (dsk BIGINT, stsk BIGINT, net VARCHAR)`,
		`CREATE TABLE st (sk BIGINT, name VARCHAR, zip VARCHAR)`,
		`CREATE TABLE ca (ask BIGINT, zip VARCHAR)`,
	}
	for i := 0; i < 3000; i++ {
		yr := fmt.Sprint(1990 + (i/4)%10)
		if i%37 == 0 {
			yr = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO d VALUES (%d, 'D%d', %s, %d, 'p%d')`, i, i, yr, i%4+1, i))
	}
	for i := 0; i < 4000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO ss VALUES (%d, %d, '%d.5')`, (i*7)%3000, i%12, i))
	}
	for i := 0; i < 12; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO st VALUES (%d, 'store%d', '%d1234')`, i, i%5, 30+i%6))
	}
	for i := 0; i < 400; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO ca VALUES (%d, '%d%03d')`, i, 30+i%9, i%7))
	}
	p := newSegPair(t, stmts...)
	const q = `SELECT st.name, sum(ss.net), count(*) FROM ss, d, st,
  (SELECT zip FROM (SELECT substr(zip, 1, 5) zip FROM ca WHERE substr(zip, 1, 2) IN ('30', '31', '33', '35')
   INTERSECT SELECT zip FROM (SELECT substr(zip, 1, 5) zip, count(*) c FROM ca GROUP BY zip HAVING count(*) > 2))) v
WHERE ss.stsk = st.sk AND ss.dsk = d.sk AND d.qoy = 2 AND %s AND substr(st.zip, 1, 2) = substr(v.zip, 1, 2)
GROUP BY st.name ORDER BY st.name`
	for _, yr := range []string{`d.yr = 1995`, `d.yr = '1995'`, `d.yr = 1995.0`, `d.yr = 1980`, `d.yr = st.sk + 1990`} {
		seek := fmt.Sprintf(q, yr)
		scan := strings.Replace(strings.Replace(seek, "d.qoy = 2", "CAST(d.qoy AS INTEGER) = 2", 1), "d.yr =", "CAST(d.yr AS INTEGER) =", 1)
		segSortedSeeksServed.Store(0)
		got := p.mustFast(seek)
		if segSortedSeeksServed.Load() == 0 {
			t.Errorf("%s: no seek served by the segments", yr)
		}
		want := p.mustFast(scan)
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n seek %.300s\n scan %.300s", yr, g, w)
		}
	}
}
