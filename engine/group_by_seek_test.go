package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// TestGroupBySeeksMatchScan: a GROUP BY whose WHERE an index or the rowid can
// seek now does, and every form must answer what the same query does with no
// seek -- group_concat's order inside a group included -- over segments, a
// delta and a session's own rows, through the hash drain and the sorted one.
func TestGroupBySeeksMatchScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.musq")
	stmts := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, a INTEGER, s TEXT)`,
		`CREATE INDEX ta ON t(a)`,
	}
	for i := -30; i <= 3000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %d, 's%d')`, i, (i+30)%7, (i+30)%400, (i+30)%13))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	clean, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()

	queries := []string{
		`SELECT k, count(*) FROM t WHERE a = 7 GROUP BY k`,
		`SELECT k, count(*), group_concat(id) FROM t WHERE a IN (9, 3, 250) GROUP BY k`,
		`SELECT s, sum(a), group_concat(id, ':') FROM t WHERE a IN (5, 6) GROUP BY s ORDER BY s DESC`,
		`SELECT k, max(s), min(id) FROM t WHERE id IN (17, 4, -30, 2999, 2000, 3000, 5000) GROUP BY k`,
		`SELECT k, count(*) FROM t WHERE a = 11 GROUP BY k HAVING count(*) > 1`,
		`SELECT k, a, count(*) FROM t WHERE a IN (1, 2) GROUP BY k, a`,
		`SELECT count(*) FROM t WHERE a IN (2, 3) GROUP BY s`,
		`SELECT k, count(*) FROM t WHERE k = 3 GROUP BY k`,
		`SELECT k, group_concat(s) FROM t WHERE a IN (` + inListLong() + `) GROUP BY k`,
	}
	check := func(t *testing.T, name string, query func(string) ([][]Value, error)) {
		t.Helper()
		for _, q := range queries {
			indexSeeksOffForTest, rowidInOffForTest = true, true
			want, werr := query(q + " -- reference")
			indexSeeksOffForTest, rowidInOffForTest = false, false
			got, gerr := query(q)
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("%s %s: error %v, reference %v", name, q, gerr, werr)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s:\n got %v\nwant %v", name, q, got, want)
			}
		}
	}
	check(t, "segments", func(q string) ([][]Value, error) {
		_, rows, err := clean.QueryArgs(q, nil)
		return rows, err
	})
	execDB(t, path, `INSERT INTO t VALUES(5000, 1, 7, 'new')`, `UPDATE t SET a = 9 WHERE id % 17 = 0`, `DELETE FROM t WHERE id % 11 = 0`)
	dirty, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dirty.Close()
	check(t, "delta", func(q string) ([][]Value, error) {
		_, rows, err := dirty.QueryArgs(q, nil)
		return rows, err
	})
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`UPDATE t SET a = 3, s = 'mine' WHERE id % 23 = 0`); err != nil {
		t.Fatal(err)
	}
	check(t, "session", func(q string) ([][]Value, error) {
		_, rows, err := n.Query(q, nil)
		return rows, err
	})
}

// TestGroupBySeekOrWalk: the columnar GROUP BY walk gives way to a seek that
// reads a sliver of the table, and keeps a key that matches much of it. A
// WHERE the walk cannot test (an IN list) leaves the loop, which seeks.
func TestGroupBySeekOrWalk(t *testing.T) {
	if !JITEnabled() {
		t.Skip("the columnar walk needs the JIT")
	}
	path := filepath.Join(t.TempDir(), "w.musq")
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, a INTEGER, b INTEGER)`, `CREATE INDEX ta ON t(a)`}
	for i := 1; i <= 20000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %d, %d)`, i, i%5, i%2000, i%3))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range []struct {
		sql  string
		want string // "seek", "walk" or "loop"
	}{
		{`SELECT k, count(*) FROM t WHERE a = 7 GROUP BY k`, "seek"},           // 10 rows of 20,000
		{`SELECT k, count(*) FROM t WHERE b = 1 AND a = 7 GROUP BY k`, "seek"}, // the narrower key decides
		{`SELECT k, count(*) FROM t WHERE b = 1 GROUP BY k`, "walk"},           // a third of the table
		{`SELECT k, count(*) FROM t WHERE k = 2 GROUP BY k`, "walk"},           // a fifth of the table
		{`SELECT k, count(*) FROM t WHERE a > 1990 GROUP BY k`, "walk"},        // a range: no seek
		{`SELECT k, count(*) FROM t WHERE a IN (1, 2, 3) GROUP BY k`, "loop"},
	} {
		ResetSegFilterCountersForTest()
		if _, _, err := p.QueryArgs(c.sql, nil); err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		got := "loop"
		if served, _ := SegFilterCountersForTest(); served > 0 {
			got = "walk"
		}
		if segGroupSeeksTaken.Load() > 0 {
			got = "seek"
		}
		if got != c.want {
			t.Errorf("%s: %s, want %s", c.sql, got, c.want)
		}
	}
}
