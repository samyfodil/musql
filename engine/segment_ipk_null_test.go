package engine

import (
	"fmt"
	"testing"
)

// TestRowidAliasIsNeverNull: an INTEGER PRIMARY KEY is stored as NULL in its
// column block and read as the rowid, so a kernel testing its NULL-ness must
// see every row as non-NULL. Reading the stored bitmap instead dropped every
// row from "WHERE a IS NOT NULL" (found by the R30 indexed-aggregate fuzz).
func TestRowidAliasIsNeverNull(t *testing.T) {
	p := newSegPair(t, `CREATE TABLE f(a INTEGER PRIMARY KEY, b INT, o TEXT NOT NULL)`,
		`INSERT INTO f VALUES(10,2,'r0'),(20,NULL,'r1'),(30,2,'r2'),(40,3,'r3'),(50,1,'r4'),(60,2,'r5')`)
	for _, q := range []string{
		`SELECT o FROM f WHERE a IS NOT NULL ORDER BY a`,
		`SELECT o FROM f WHERE a IS NOT NULL`,
		`SELECT group_concat(o) FROM f WHERE a IS NOT NULL`,
		`SELECT count(*) FROM f WHERE a IS NULL`,
		`SELECT count(*) FROM f WHERE a IS NOT NULL AND b IS NULL`,
		`SELECT sum(b) FROM f WHERE a NOTNULL`,
	} {
		_, want, err := p.plain(q)
		if err != nil {
			t.Fatalf("%s plain: %v", q, err)
		}
		_, got, err := p.fast(q)
		if err != nil {
			t.Fatalf("%s fast: %v", q, err)
		}
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s: kernel %s, loop %s", q, g, w)
		}
	}
}
