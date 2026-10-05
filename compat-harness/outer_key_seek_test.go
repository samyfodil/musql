package compat

import (
	"fmt"
	"testing"
)

// TestCorrelatedEqualitySeeksWithTheOuterKey: a correlated subquery's
// "inner.col = outer.col" seeks the inner index with the outer value, as C's
// codeEqualityTerm does, instead of scanning the inner table once per outer
// row. The seek only narrows the candidates -- the equality is still tested --
// so what must hold is that it never drops a row the comparison accepts: across
// affinities, a collation the key brings to the comparison, NULLs, either
// operand order, and rows fresh in the log or compacted into segments.
func TestCorrelatedEqualitySeeksWithTheOuterKey(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b, bt TEXT, bn TEXT COLLATE NOCASE, bi INTEGER)",
		"INSERT INTO t VALUES(1,3,'3','X',3),(2,'3','03','x',7),(3,NULL,NULL,NULL,NULL),(4,7.0,'7','Y',11),(5,'x','x','y',3)",
		"CREATE TABLE u(x INTEGER PRIMARY KEY, y, yt TEXT, yn TEXT COLLATE NOCASE, yi INTEGER)",
		"CREATE INDEX uy ON u(y)", "CREATE INDEX uyt ON u(yt)", "CREATE INDEX uyn ON u(yn)", "CREATE INDEX uyi ON u(yi)",
		"INSERT INTO u VALUES(10,3,'3','x',3),(11,'3','3','X',3),(12,7,'7','y',7),(13,NULL,NULL,NULL,NULL),(14,'x','x','Y',11),(15,3.0,'3.0','x',3)",
	}
	pairs := [][2]string{
		{"u.y", "t.b"}, {"u.yt", "t.b"}, {"u.yi", "t.b"}, {"u.yn", "t.bn"}, {"u.yt", "t.bn"}, {"u.yn", "t.bt"},
		{"u.y", "t.bi"}, {"u.yi", "t.bt"}, {"u.y", "t.bt"},
	}
	var qs []string
	for _, p := range pairs {
		for _, cmp := range []string{p[0] + " = " + p[1], p[1] + " = " + p[0]} {
			qs = append(qs,
				fmt.Sprintf("SELECT a, (SELECT group_concat(x) FROM u WHERE %s) FROM t ORDER BY a", cmp),
				fmt.Sprintf("SELECT a FROM t WHERE EXISTS (SELECT 1 FROM u WHERE %s) ORDER BY a", cmp),
				fmt.Sprintf("SELECT a, (SELECT count(*) FROM u WHERE %s AND x > 10) FROM t ORDER BY a", cmp))
		}
	}
	for _, compact := range []string{"SELECT 1", "ANALYZE"} {
		for i, q := range qs {
			differ(t, fmt.Sprintf("outer-key %d %s", i, compact), append(append([]string{}, base...), compact, q))
		}
	}
}

// TestCorrelatedSeekRefusesTheKeysCollation: "t.bn = u.yt" compares under
// t.bn's NOCASE (the left operand's, expr.c:424), which u.yt's BINARY index
// cannot seek -- once the rows are compacted into segments, a seek answered
// "12" where C answers "10,12" (outerSeekCollationAgrees).
func TestCorrelatedSeekRefusesTheKeysCollation(t *testing.T) {
	for _, compact := range []string{"SELECT 1", "ANALYZE"} {
		for _, cmp := range []string{"t.bn = u.yt", "u.yt = t.bn", "u.yt = t.bn COLLATE nocase"} {
			differ(t, compact+" "+cmp, []string{
				"CREATE TABLE t(a INTEGER PRIMARY KEY, bn TEXT COLLATE NOCASE)", "INSERT INTO t VALUES(1,'X'),(2,'x'),(3,'Y')",
				"CREATE TABLE u(x INTEGER PRIMARY KEY, yt TEXT)", "CREATE INDEX uyt ON u(yt)", "INSERT INTO u VALUES(10,'x'),(11,'y'),(12,'X')",
				compact,
				"SELECT a, (SELECT group_concat(x) FROM u WHERE " + cmp + ") FROM t ORDER BY a",
			})
		}
	}
}
