package engine

import (
	"fmt"
	"testing"
)

// TestSegJoinTextBoundsMatchLoop: a join level's comparison of a TEXT or BLOB
// column with a constant, the constant a literal or a date function's value
// computed once, answers as the loop does: under TEXT affinity and under a
// DATE column's NUMERIC affinity, with a bound a date function makes NULL or
// reads off the clock, over a column holding NULLs or an INTEGER, a BLOB
// against a BLOB and a TEXT against a BLOB, a TEXT cell that NUMERIC affinity
// reads as a number (a subquery's bound brings that affinity to a TEXT
// column), in UTF-8 and in UTF-16. The packed cells answer most of them.
func TestSegJoinTextBoundsMatchLoop(t *testing.T) {
	setup := []string{
		`CREATE TABLE o(id INTEGER PRIMARY KEY, pri TEXT)`,
		`CREATE TABLE l(id INTEGER PRIMARY KEY, ok INTEGER, rd TEXT, dd DATE, sn TEXT, mx TEXT, bl BLOB, tx TEXT)`,
		`CREATE TABLE n(d NUMERIC)`,
		`INSERT INTO n VALUES('!')`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 50) INSERT INTO o SELECT i, (i % 5 + 1) || '-PRI' FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 3000) INSERT INTO l SELECT i, i % 50 + 1,
			printf('%d-%02d-%02d', 1990 + i % 9, i % 12 + 1, i % 28 + 1),
			printf('%d-%02d-%02d', 1990 + i % 9, i % 12 + 1, i % 28 + 1),
			CASE WHEN i % 7 = 0 THEN NULL ELSE 'm' || (i % 6) END,
			CASE WHEN i % 500 = 0 THEN i ELSE 'm' || (i % 6) END,
			CAST(printf('b%d', i % 4) AS BLOB),
			CASE i % 4 WHEN 0 THEN '9' WHEN 1 THEN 'abc' WHEN 2 THEN '12' ELSE ' ' END
			FROM c`,
	}
	queries := []string{
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.rd >= '1994-01-01' AND l.rd < '1995-01-01' GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.dd >= date('1994-01-01') AND l.dd < date('1994-01-01', '1 years') GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.rd >= date('1994-01-01') AND l.rd <= strftime('%Y-%m-%d', '1996-06-30') GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.dd > '1997' GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.rd < date('garbage') GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.rd < date('now') GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.sn > 'm2' GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.mx > 'm2' GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.bl > x'6231' GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.rd < x'00' GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.tx > (SELECT d FROM n) GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.tx > '!' GROUP BY o.pri`,
		`SELECT count(*), sum(l.id) FROM o, l WHERE o.id = l.ok AND l.rd BETWEEN '1991-03-01' AND date('1991-09-30') AND o.pri <> '1-PRI'`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.rd < date('now') AND l.mx LIKE '%3' GROUP BY o.pri`,
		`SELECT o.pri, count(*) FROM o, l WHERE o.id = l.ok AND l.rd < '1996' AND l.mx LIKE '%3' GROUP BY o.pri`,
	}
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		p := newSegPair(t, append([]string{"PRAGMA encoding = '" + enc + "'"}, setup...)...)
		db, err := OpenWrite(p.path)
		if err != nil {
			t.Fatal(err)
		}
		segJoinBytesPasses.Store(0)
		for _, q := range queries {
			want := fmt.Sprint(p.plain(q))
			for run := 1; run <= 2; run++ {
				if got := fmt.Sprint(db.Query(q, nil)); got != want {
					t.Errorf("%s, %s, run %d:\n  loop   %.300s\n  kernel %.300s", enc, q, run, want, got)
				}
			}
		}
		db.Discard()
		if n := segJoinBytesPasses.Load(); enc == "UTF-8" && jitEnabled && n < 8 {
			t.Errorf("%d level terms compared on packed cells, want at least 8", n)
		} else if enc != "UTF-8" && n != 0 {
			t.Errorf("%s: %d level terms compared on packed cells, want none", enc, n)
		}
	}
}
