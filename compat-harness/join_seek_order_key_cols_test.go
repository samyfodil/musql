package compat

import "testing"

// TestJoinSeekOrderKeyColumnsDecoded: a join level's index seek whose rows are
// built and sorted into the index's order (a key column the program never
// reads, b here, with a REAL and an INTEGER that compare equal ahead of it)
// decodes the key's columns, so the rows reach group_concat in the index's
// order, as C walks them.
func TestJoinSeekOrderKeyColumnsDecoded(t *testing.T) {
	differ(t, "join-seek-order-key-cols", []string{
		`CREATE TABLE t(a,b,c)`,
		`INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)`,
		`CREATE TABLE z(a,d)`,
		`INSERT INTO z VALUES(1,'p'),(2,'q'),(3,'r'),(2,'s'),(NULL,'u')`,
		`CREATE INDEX i1 ON t(a,b)`,
		`SELECT count(*), group_concat(t.c) FROM t JOIN z ON t.a = z.a`,
		`SELECT group_concat(t.c, '/') FROM z JOIN t ON t.a = z.a WHERE z.d <> 'q'`,
		`SELECT z.d, group_concat(t.c) FROM z JOIN t ON t.a = z.a GROUP BY z.d`,
	})
}
