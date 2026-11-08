package compat

import (
	"fmt"
	"testing"
)

// TestGroupJoinNeverOrderedServed: a join's GROUP BY with a DISTINCT
// aggregate and no ORDER BY, grouped by columns of a table that no join order
// can deliver in order (TPC-H q16 without its ORDER BY), is served and emits
// its groups as C does -- sorted. C's planner stops at the first loop that is
// not order-distinct, and no table here can be one as the outermost loop.
func TestGroupJoinNeverOrderedServed(t *testing.T) {
	schema := []string{
		`CREATE TABLE part(p_partkey INTEGER PRIMARY KEY, p_brand TEXT NOT NULL, p_type TEXT NOT NULL, p_size INTEGER NOT NULL)`,
		`CREATE TABLE partsupp(ps_partkey INTEGER NOT NULL, ps_suppkey INTEGER NOT NULL, ps_cost REAL, PRIMARY KEY(ps_partkey, ps_suppkey))`,
		`CREATE TABLE supplier(s_suppkey INTEGER PRIMARY KEY, s_comment TEXT)`,
	}
	brands := []string{"Brand#5", "Brand#1", "Brand#4", "Brand#2", "Brand#3"}
	types := []string{"STEEL", "COPPER", "BRASS"}
	for i := 1; i <= 40; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO part VALUES(%d, '%s', '%s', %d)`, i, brands[(i*7)%5], types[(i*5)%3], (i*11)%4))
	}
	for i := 1; i <= 10; i++ {
		c := "fine"
		if i%4 == 0 {
			c = "Customer Complaints"
		}
		schema = append(schema, fmt.Sprintf(`INSERT INTO supplier VALUES(%d, '%s')`, i, c))
	}
	for i := 1; i <= 40; i++ {
		for j := 0; j < 4; j++ {
			schema = append(schema, fmt.Sprintf(`INSERT INTO partsupp VALUES(%d, %d, %d.5)`, (i*13)%40+1, (i+j*3)%10+1, i+j))
		}
	}
	for _, q := range []string{
		`SELECT p_brand, p_type, p_size, count(DISTINCT ps_suppkey) FROM partsupp, part
		 WHERE p_partkey = ps_partkey AND p_brand <> 'Brand#4' AND p_size IN (0, 1, 3)
		   AND ps_suppkey NOT IN (SELECT s_suppkey FROM supplier WHERE s_comment LIKE '%Complaints%')
		 GROUP BY p_brand, p_type, p_size`,
		`SELECT p_type, count(DISTINCT ps_suppkey) FROM part JOIN partsupp ON ps_partkey = p_partkey GROUP BY p_type`,
		`SELECT p_brand, count(DISTINCT s_suppkey) FROM part, partsupp, supplier
		 WHERE ps_partkey = p_partkey AND s_suppkey = ps_suppkey GROUP BY p_brand`,
	} {
		differ(t, "group-join-never-ordered", append(append([]string{}, schema...), q))
	}
}
