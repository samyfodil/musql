package engine

import (
	"fmt"
	"testing"
)

// TestSegJoinArgsOnPassingRows: an aggregate's argument over a level whose
// own terms let few rows through is computed on those rows alone, and
// answers as the loop does: INTEGERs and REALs mixed, NULLs, and an argument
// that raises an error on a row the terms drop.
func TestSegJoinArgsOnPassingRows(t *testing.T) {
	stmts := []string{
		`CREATE TABLE li (pk INTEGER, price, disc, ship TEXT, big INTEGER)`,
		`CREATE TABLE pt (pk INTEGER, ty TEXT)`,
	}
	for i := 0; i < 3000; i++ {
		price := fmt.Sprintf("%d.25", 100+i%97)
		if i%5 == 0 {
			price = fmt.Sprint(100 + i%97)
		}
		disc := "0.05"
		switch {
		case i%7 == 0:
			disc = "0"
		case i%11 == 0:
			disc = "NULL"
		}
		big := "1"
		if i%12 == 11 {
			big = "-9223372036854775808" // abs() of it overflows, in December, which no query keeps
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO li VALUES (%d, %s, %s, '1995-%02d-%02d', %s)`, i%200, price, disc, 1+i%12, 1+i%28, big))
	}
	for i := 0; i < 200; i++ {
		ty := "STANDARD"
		if i%3 == 0 {
			ty = "PROMO BRUSHED"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO pt VALUES (%d, '%s')`, i, ty))
	}
	p := newSegPair(t, stmts...)
	for _, q := range []string{
		`SELECT sum(CASE WHEN ty LIKE 'PROMO%' THEN price * (1 - disc) ELSE 0 END), sum(price * (1 - disc)), count(*)
 FROM li, pt WHERE li.pk = pt.pk AND ship >= '1995-09-01' AND ship < '1995-10-01'`,
		`SELECT ty, sum(price * (1 - disc)), avg(price + disc), min(price * 2), max(disc * 3), total(abs(big)) FROM li, pt
 WHERE li.pk = pt.pk AND ship >= '1995-03-01' AND ship < '1995-03-04' GROUP BY ty ORDER BY ty`,
	} {
		before := segJoinArgsSparse.Load()
		got := p.mustFast(q)
		if columnarOn() && segJoinArgsSparse.Load() == before {
			t.Errorf("%s: no argument computed on the passing rows alone", q)
		}
		want := p.mustPlain(q)
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n fast  %.300s\n plain %.300s", q, g, w)
		}
	}
}
