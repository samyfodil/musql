package engine

import (
	"fmt"
	"testing"
	"time"
)

// TestSegJoinClockNotKept: a join filter that reads the clock is computed
// again by the next statement, whose 'now' has moved; a join's filters are
// not kept with its table when one of them does.
func TestSegJoinClockNotKept(t *testing.T) {
	db := memdbTestDB(t)
	stmts := []string{`CREATE TABLE a(id INTEGER PRIMARY KEY, ts REAL, k INTEGER)`, `CREATE TABLE b(id INTEGER PRIMARY KEY, k INTEGER)`}
	for i := 0; i < 2000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO a VALUES(%d, NULL, %d)`, i, i%50), fmt.Sprintf(`INSERT INTO b VALUES(%d, %d)`, i, i%50))
	}
	// One second from now.
	stmts = append(stmts, `UPDATE a SET ts = julianday('now') + 1.0 / 86400`)
	for _, q := range stmts {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	run := func(q string) string {
		t.Helper()
		p, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		_, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	qs := []string{
		`SELECT count(*) FROM a, b WHERE a.k = b.k AND a.ts > julianday('now')`,
		`SELECT count(*) FROM a, b WHERE a.k = b.k AND a.ts > CAST(julianday('now') AS REAL)`,
		`SELECT count(*) FROM a, b WHERE a.k = b.k AND a.ts > julianday()`,
	}
	for _, q := range qs {
		if got := run(q); got != "[1:80000|]" {
			t.Fatalf("%s, before its time: %s", q, got)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	for _, q := range qs {
		if got := run(q); got != "[1:0|]" {
			t.Errorf("%s, a run later: %s, want [1:0|]", q, got)
		}
	}
}

// TestSegJoinFiltersAgree: a columnar join's filters, computed only on the
// rows the ones before them let through (one-column filters once per value,
// a column read on those rows only when most fail) and kept for the next run
// while the rows stay the same, give the loop's answer -- on a first run, a
// second, and after the rows change. So do aggregates of one column, computed
// once per value: l.m holds the same bytes as TEXT and as a BLOB.
func TestSegJoinFiltersAgree(t *testing.T) {
	stmts := []string{
		"CREATE TABLE o(ok INTEGER PRIMARY KEY, pri TEXT)",
		"CREATE TABLE l(id INTEGER PRIMARY KEY, ok INTEGER, mode TEXT, sd TEXT, cd TEXT, rd TEXT, q INTEGER, d REAL, m)",
	}
	modes := []string{"MAIL", "SHIP", "AIR", "RAIL", "TRUCK", "FOB", "REG AIR"}
	for i := 1; i <= 400; i++ {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO o VALUES(%d, '%d-PRI')", i, i%5+1))
	}
	for i := 1; i <= 4000; i++ {
		mode := fmt.Sprintf("'%s'", modes[i%len(modes)])
		if i%23 == 0 {
			mode = "NULL"
		}
		// m holds the same bytes as TEXT and as a BLOB, which a filter of
		// it tells apart.
		m := []string{"'ab'", "x'6162'", "97", "'AB'"}[i%4]
		stmts = append(stmts, fmt.Sprintf("INSERT INTO l VALUES(%d, %d, %s, '1994-%02d-%02d', '1994-%02d-%02d', '199%d-%02d-%02d', %d, %d.5, %s)",
			i, i%400+1, mode, i%12+1, i%28+1, (i+3)%12+1, (i*7)%28+1, 3+i%3, (i*5)%12+1, (i*3)%28+1, i%9, i%4, m))
	}
	p := newSegPair(t, stmts...)
	filters := []string{
		"l.mode IN ('MAIL', 'SHIP')",
		"l.cd < l.rd",
		"l.sd < l.cd",
		"l.rd >= date('1994-01-01')",
		"l.rd < date('1994-01-01', '+1 year')",
		"l.mode = 'AIR' OR l.q > 5",
		"CASE WHEN l.mode = 'RAIL' THEN 1 ELSE l.q > 3 END",
		"l.mode LIKE '%AIR'",
		"l.q / (l.q - l.q) IS NULL",
		"l.d > 1",
		"l.mode IS NULL OR l.mode <> 'FOB'",
		"CASE l.mode WHEN 'RAIL' THEN 1 WHEN 'AIR' THEN 1 ELSE 0 END = 1",
		"l.m IN ('ab', 'zz')",
		"l.m IN (x'6162', 5)",
	}
	var qs []string
	for i := range filters {
		for j := i + 1; j < len(filters); j++ {
			qs = append(qs, fmt.Sprintf("SELECT l.mode, count(*), sum(l.q), sum(CASE WHEN o.pri = '1-PRI' THEN 1 ELSE 0 END), max(l.rd), sum(l.m = 'ab'), sum(typeof(l.m) = 'blob') FROM o, l WHERE o.ok = l.ok AND %s AND %s GROUP BY l.mode ORDER BY l.mode", filters[i], filters[j]))
		}
	}
	restricted, reused := segJoinFilterRestricted.Load(), segJoinPassReuses.Load()
	check := func(label string) {
		t.Helper()
		n, err := OpenWrite(p.path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		for _, q := range qs {
			want := fmt.Sprint(typedRows(p.mustPlain(q)))
			for run := 1; run <= 2; run++ {
				_, rows, err := n.Query(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				if got := fmt.Sprint(typedRows(rows)); got != want {
					t.Errorf("%s, run %d: %s\n fast  %s\n plain %s", label, run, q, got, want)
				}
			}
		}
	}
	check("segments")
	p.delta("INSERT INTO l VALUES(9000, 3, 'MAIL', '1994-01-02', '1994-02-02', '1994-03-02', 9, 2.5, 'ab')",
		"UPDATE l SET mode = 'SHIP' WHERE id % 5 = 0", "DELETE FROM l WHERE id % 7 = 0")
	check("rows changed")
	if segJoinFilterRestricted.Load() == restricted || segJoinPassReuses.Load() == reused {
		t.Errorf("filters computed on some rows: %d, levels read back: %d", segJoinFilterRestricted.Load()-restricted, segJoinPassReuses.Load()-reused)
	}
}
