package compat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestFlattenJoinBody: a FROM subquery that joins several tables and computes
// named columns (TPC-H q07 and q09's shape) is flattened as C flattens it, so
// the parent's GROUP BY and ORDER BY run over the join itself. The plan C
// prints names the base tables, and every answer, ties included, is C's.
// Shapes the rewrite leaves alone answer too: a body column that may carry a
// subtype, an unnamed expression a "*" would name, and a parent holding a
// subquery.
func TestFlattenJoinBody(t *testing.T) {
	schema := []string{
		`CREATE TABLE n(nk INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE s(sk INTEGER PRIMARY KEY, nk INT)`,
		`CREATE TABLE o(ok INTEGER PRIMARY KEY, d TEXT)`,
		`CREATE TABLE l(ok INT, sk INT, price REAL, disc REAL, qty INT)`,
	}
	for i := 1; i <= 5; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO n VALUES(%d, 'N%d')`, i, i%3))
	}
	for i := 1; i <= 12; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO s VALUES(%d, %d)`, i, i%5+1))
	}
	for i := 1; i <= 30; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO o VALUES(%d, '199%d-0%d-1%d')`, i, 4+i%3, 1+i%9, i%10))
	}
	for i := 0; i < 200; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO l VALUES(%d, %d, %d.25, 0.0%d, %d)`, i%30+1, i%12+1, 10+i%17, i%9, i%7))
	}
	for _, q := range []string{
		`SELECT nation, yr, sum(amount) FROM (
		   SELECT n.name AS nation, strftime('%Y', o.d) AS yr, price * (1 - disc) - qty AS amount
		   FROM s, l, o, n WHERE s.sk = l.sk AND o.ok = l.ok AND n.nk = s.nk AND price > 12
		 ) AS profit GROUP BY nation, yr ORDER BY nation, yr DESC`,
		`SELECT nation, count(*) FROM (SELECT n.name AS nation, l.qty FROM n, s, l WHERE s.nk = n.nk AND l.sk = s.sk) GROUP BY nation`,
		`SELECT * FROM (SELECT n.name AS nation, l.qty FROM n, s, l WHERE s.nk = n.nk AND l.sk = s.sk AND l.qty = 3) ORDER BY 1`,
		`SELECT json_quote(j) FROM (SELECT json_array(l.qty) AS j FROM s, l WHERE l.sk = s.sk AND s.sk < 3) ORDER BY 1`,
		`SELECT * FROM (SELECT upper(n.name), l.qty FROM n, s, l WHERE s.nk = n.nk AND l.sk = s.sk AND l.qty = 6) ORDER BY 1, 2`,
		`SELECT nation, (SELECT count(*) FROM n WHERE name = nation) FROM (SELECT n.name AS nation FROM n, s WHERE s.nk = n.nk) ORDER BY 1`,
	} {
		differ(t, "flatten-join-body", append(append([]string{}, schema...), q))
	}

	// The plan scans the base tables as C's does. This engine prints no
	// temp b-tree line for any statement, so those of C's are left out.
	c, m := boundPair(t, schema)
	ctx := context.Background()
	cc, err := c.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	mc, err := m.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	q := `SELECT nation, yr, sum(amount) FROM (
	        SELECT n.name AS nation, strftime('%Y', o.d) AS yr, price * (1 - disc) - qty AS amount
	        FROM s, l, o, n WHERE s.sk = l.sk AND o.ok = l.ok AND n.nk = s.nk
	      ) AS profit GROUP BY nation, yr ORDER BY nation, yr DESC`
	want := slices.DeleteFunc(planDetails(t, cc, q), func(d string) bool { return strings.HasPrefix(d, "USE TEMP B-TREE") })
	if got := planDetails(t, mc, q); !slices.Equal(want, got) {
		t.Errorf("EXPLAIN QUERY PLAN:\n  cgo:   %q\n  musql: %q", want, got)
	}
}
