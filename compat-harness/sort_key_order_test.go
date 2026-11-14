package compat

import (
	"fmt"
	"testing"
)

// TestTextKeyOrders pins the orders a hash GROUP BY drains in and an
// automatic index puts a join level's rows in, over keys mixing NULLs,
// INTEGERs, TEXTs (one a prefix of another) and BLOBs with zero bytes, under
// BINARY and NOCASE, ascending and descending, with no ORDER BY to hide the
// order.
func TestTextKeyOrders(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(a, b TEXT, c, v INT)",
		"CREATE TABLE u(b TEXT, c, w INT)",
	}
	vals := []string{"NULL", "1", "-5", "'a'", "'ab'", "'A'", "'b'", "''", "X'00'", "X'0001'", "X''", "'a b'"}
	for i := range 60 {
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%s, %s, %s, %d)", vals[i%len(vals)], vals[(i*5)%len(vals)], vals[(i*7)%len(vals)], i))
	}
	for i := range 40 {
		setup = append(setup, fmt.Sprintf("INSERT INTO u VALUES(%s, %s, %d)", vals[(i*5)%len(vals)], vals[(i*3)%len(vals)], i))
	}
	queries := []string{
		"SELECT b, c, count(*), sum(v) FROM t GROUP BY b, c",
		"SELECT b, c, count(*) FROM t GROUP BY b, c ORDER BY b DESC, c",
		"SELECT a, b, max(v) FROM t GROUP BY a, b ORDER BY a, b DESC",
		"SELECT b COLLATE NOCASE, count(*) FROM t GROUP BY 1",
		"SELECT t.v, u.w FROM t JOIN u ON u.b = t.b AND u.c = t.c",
		"SELECT t.v, u.w FROM t JOIN u ON u.b = t.b",
		"SELECT t.v, group_concat(u.w) FROM t JOIN u ON u.b = t.b GROUP BY t.v",
		"SELECT t.v, u.w FROM t JOIN u ON u.b = t.b COLLATE NOCASE",
	}
	for _, q := range queries {
		differ(t, q, append(append([]string(nil), setup...), q))
	}
}
