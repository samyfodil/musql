package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestSegSortedSeekNarrowsWithoutOrder: a sorted seek keyed on further
// columns where no order can show (TPC-DS q88, ANALYZEd: every aggregate a
// count) has no automatic index order to follow, so it sorts each run by
// those columns, then the rowid, and takes only the stretch equal on them
// all: most probes then miss, and a filter is built, unless a column holds a
// TEXT. Runs of 100 rows were walked whole; runs of 600 were not served at
// all. The answers are the seek's on its first column alone. NULLs, a TEXT in
// an INTEGER column, and keys that are a REAL or a TEXT are among them.
func TestSegSortedSeekNarrowsWithoutOrder(t *testing.T) {
	for _, l := range []struct {
		keys int
		text bool
	}{{600, false}, {100, false}, {600, true}, {100, true}} {
		keys, x := l.keys, "(value / 7) %% 5"
		if l.text {
			x = "CASE WHEN value %% 41 = 0 THEN 'x' ELSE (value / 7) %% 5 END"
		}
		p := newSegPair(t,
			`CREATE TABLE ss(tsk INTEGER, hd INTEGER, st INTEGER, v INTEGER)`,
			`CREATE TABLE tm(tsk INTEGER, hour INTEGER)`,
			`CREATE TABLE hd(hdsk INTEGER, dep INTEGER)`,
			`CREATE TABLE st(stsk INTEGER, name TEXT)`,
			fmt.Sprintf(`INSERT INTO ss SELECT value %% %d, CASE WHEN value %% 53 = 0 THEN NULL ELSE (value / %d * 13) %% 4000 END,
			   `+x+`, value FROM generate_series(1, 60000)`, keys, keys),
			fmt.Sprintf(`INSERT INTO tm SELECT value, value * 24 / %d FROM generate_series(0, %d)`, keys, keys-1),
			`INSERT INTO hd SELECT value, value % 7 FROM generate_series(0, 3999)`,
			`INSERT INTO st SELECT value, CASE value WHEN 2 THEN 'ese' ELSE 'other' END FROM generate_series(0, 4)`,
			`ANALYZE`)
		const q = `SELECT count(*) FROM ss, hd, tm, st
WHERE ss.tsk = tm.tsk AND ss.hd = %s AND ss.st = %s AND tm.hour = 8 AND hd.dep = 3 AND st.name = 'ese'`
		for _, c := range []struct {
			hd, st string
			some   bool
		}{
			{`hd.hdsk`, `st.stsk`, true},
			{`hd.hdsk * 1.0`, `st.stsk`, true},
			{`hd.hdsk`, `CAST(st.stsk AS TEXT)`, true},
			{`hd.hdsk + 0.5`, `st.stsk`, false},
		} {
			seek := fmt.Sprintf(q, c.hd, c.st)
			one := strings.Replace(strings.Replace(seek, "ss.hd =", "ss.hd + 0 =", 1), "ss.st =", "+ss.st =", 1)
			segSortedSeeksServed.Store(0)
			segBloomsBuilt.Store(0)
			got := p.mustFast(seek)
			if segSortedSeeksServed.Load() == 0 {
				t.Errorf("runs of %d, text %v, %s: no seek served by the segments", 60000/keys, l.text, seek)
			}
			if !l.text && segBloomsBuilt.Load() == 0 {
				t.Errorf("runs of %d, text %v, %s: no filter answered its probes", 60000/keys, l.text, seek)
			}
			want := p.mustFast(one)
			if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
				t.Errorf("runs of %d, text %v, %s:\n seek %s\n one  %s", 60000/keys, l.text, seek, g, w)
			}
			if n := got[0][0].I; c.some == (n == 0) {
				t.Errorf("runs of %d, text %v, %s: count %d", 60000/keys, l.text, seek, n)
			}
		}
	}
}
