package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSegWriteFilterNeverDropsARow holds segWriteFilterPeephole to the
// pre-filter's one obligation on the write side: a row its WHERE matches must
// still be written. Two identical databases with rows in all three places a
// row can be -- the segments, the delta, and the session's own overlay -- run
// the same UPDATE/DELETE sequence, one with the filter and one without, and
// every table state is compared after every statement.
func TestSegWriteFilterNeverDropsARow(t *testing.T) {
	setup := func(t *testing.T) *Session {
		t.Helper()
		path := filepath.Join(t.TempDir(), "w.musq")
		buildDB(t, path, append([]string{
			`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, n INTEGER, s TEXT)`,
			`CREATE INDEX t_k ON t(k)`,
		}, segRowWrites(1, 3000)...)...)
		execDB(t, path, `VACUUM`) // every row into segments
		execDB(t, path, segRowWrites(3001, 3400)...)
		execDB(t, path, `UPDATE t SET v = v + 1 WHERE id % 50 = 0`, `DELETE FROM t WHERE id % 97 = 0`,
			`INSERT INTO t VALUES(-5, 3, 10, NULL, 'neg')`) // the delta: puts, kills, a negative rowid
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.Discard() })
		for _, s := range []string{`UPDATE t SET n = 1 WHERE id % 31 = 0`, `INSERT INTO t VALUES(5000, 7, 7, 7, 'over')`} {
			if err := n.Exec(s); err != nil { // the overlay, left uncommitted
				t.Fatal(err)
			}
		}
		return n
	}
	ref, got := setup(t), setup(t)
	var stmts []string
	for _, k := range []int{-1, 0, 3, 7, 16} {
		stmts = append(stmts,
			fmt.Sprintf(`UPDATE t SET v = v + 1 WHERE k = %d`, k),
			fmt.Sprintf(`UPDATE t SET k = k + 1 WHERE v > %d AND k < 9`, k*60000),
			fmt.Sprintf(`UPDATE t SET s = 'n' WHERE n > %d OR k = 2`, k*100),
			fmt.Sprintf(`DELETE FROM t WHERE k = %d AND v < 500000`, k+1),
			fmt.Sprintf(`DELETE FROM t WHERE n <= %d`, k*500),
		)
	}
	stmts = append(stmts, `UPDATE t SET v = 0 WHERE id < 0`, `DELETE FROM t WHERE n IS NULL AND k = 5`)
	dump := `SELECT id, k, v, n, s FROM t ORDER BY id`
	fired := 0
	for _, s := range stmts {
		segPeepholesOffForTest = true
		rerr := ref.Exec(s)
		segPeepholesOffForTest = false
		SegRowsSelectedForTest()
		gerr := got.Exec(s)
		if SegRowsSelectedForTest() > 0 {
			fired++
		}
		if (rerr == nil) != (gerr == nil) {
			t.Fatalf("%s: reference err=%v filtered err=%v", s, rerr, gerr)
		}
		_, want, _ := ref.Query(dump, nil)
		_, have, _ := got.Query(dump, nil)
		if fmt.Sprint(want) != fmt.Sprint(have) {
			t.Fatalf("%s: tables differ (%d rows reference, %d filtered)", s, len(want), len(have))
		}
	}
	if fired == 0 {
		t.Fatal("the write filter never fired -- this test proves nothing")
	}
	t.Logf("%d of %d statements went through the write filter", fired, len(stmts))
}

func segRowWrites(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		n := "NULL"
		if i%3 != 0 {
			n = fmt.Sprint(i * 7)
		}
		out = append(out, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,%s,'s%d')`, i, i%17, (i*7919)%1000000, n, i))
	}
	return out
}
