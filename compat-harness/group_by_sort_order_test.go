package compat

import (
	"fmt"
	"testing"
)

// This file tests that GROUP BY terms take the ASC/DESC direction of
// corresponding ORDER BY terms when the lists have the same length.
func TestGroupBySortOrderFollowsOrderBy(t *testing.T) {
	setup := []string{
		`CREATE TABLE u(g,k,s COLLATE NOCASE)`,
		`INSERT INTO u VALUES(1,1,'a'),(2,1,'B'),(3,2,'A'),(4,2,'b'),(5,3,'c'),(6,3,'C')`,
		`CREATE TABLE t1(g,b)`,
		`INSERT INTO t1 VALUES(1,'b'),(2,'A'),(3,'a'),(4,'B')`,
	}
	qs := []string{
		// One GROUP BY term, one ORDER BY term: the copy fires.
		`SELECT g FROM u GROUP BY g ORDER BY k`,
		`SELECT g FROM u GROUP BY g ORDER BY k DESC`,
		`SELECT g FROM t1 GROUP BY g ORDER BY length(b)`,
		`SELECT g FROM t1 GROUP BY g ORDER BY length(b) DESC`,
		`SELECT g FROM t1 GROUP BY g ORDER BY (g>2) DESC`,
		`SELECT g FROM t1 GROUP BY g ORDER BY 'k' DESC`,
		`SELECT g FROM t1 GROUP BY g ORDER BY NULL DESC`,
		`SELECT g, count(*) FROM u GROUP BY g ORDER BY count(*) DESC`,
		`SELECT g FROM u GROUP BY g HAVING 1 ORDER BY k DESC`,
		// Different lengths: no copy, so the grouping stays ascending.
		`SELECT g FROM u GROUP BY g,k ORDER BY k DESC`,
		`SELECT g FROM u GROUP BY g ORDER BY k DESC, g ASC`,
		`SELECT g FROM u GROUP BY g ORDER BY k DESC, g DESC`,
		// Two and two: both bits are copied, positionally.
		`SELECT g,k FROM u GROUP BY k,g ORDER BY k DESC, 1 ASC`,
		`SELECT g,k FROM u GROUP BY g,k ORDER BY 2 DESC, 1 DESC`,
		`SELECT k FROM u GROUP BY k,g ORDER BY 1 ASC, 1 DESC`,
		// The GROUP BY and ORDER BY being the SAME expression is the case the
		// call site is actually about (orderByGrp), so it must not regress.
		`SELECT g FROM u GROUP BY g ORDER BY g DESC`,
		`SELECT g FROM u GROUP BY g ORDER BY g`,
		`SELECT k, count(*) FROM u GROUP BY k ORDER BY k DESC`,
		// DISTINCT takes the batch path, which sorts the groups itself.
		`SELECT DISTINCT k FROM u GROUP BY g ORDER BY k DESC`,
		`SELECT DISTINCT k FROM u GROUP BY g ORDER BY k`,
	}
	for _, q := range qs {
		q := q
		t.Run(q, func(t *testing.T) {
			differ(t, "groupsortorder/"+q, append(append([]string{}, setup...), q))
		})
	}
}

// TestGroupBySortOrderWithCollation tests the same rule with COLLATE NOCASE keys.
func TestGroupBySortOrderWithCollation(t *testing.T) {
	setup := []string{
		`CREATE TABLE m(k COLLATE NOCASE, v)`,
		`INSERT INTO m VALUES('A',1),('a',2),('B',3),('b',4),('C',5)`,
	}
	var qs []string
	for _, ord := range []string{"", " DESC", " ASC"} {
		for _, term := range []string{"v", "k", "1", "length(k)", "k COLLATE BINARY"} {
			qs = append(qs, fmt.Sprintf("SELECT k, count(*) FROM m GROUP BY k ORDER BY %s%s", term, ord))
		}
	}
	for _, q := range qs {
		q := q
		t.Run(q, func(t *testing.T) {
			differ(t, "groupsortcoll/"+q, append(append([]string{}, setup...), q))
		})
	}
}
