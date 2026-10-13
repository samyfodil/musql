package compat

import (
	"fmt"
	"testing"
)

// TestWindowMovingFrameSweep crosses the aggregates C inverts (sum, total, avg,
// count, group_concat) with the data that makes an incremental accumulator
// differ from a recomputed one (infinities, integer extremes, int/real
// transitions, NULLs, varying separators) and with every frame shape.
func TestWindowMovingFrameSweep(t *testing.T) {
	datasets := map[string]string{
		"ints":    "(1,1,1,'a','|'),(2,2,2,'b','|'),(3,3,3,'c','|'),(4,4,4,'d','|'),(5,5,5,'e','|')",
		"extreme": "(1,1,9223372036854775807,'a','|'),(2,2,5,'b','--'),(3,3,0,'c','|'),(4,4,-5,'d',''),(5,5,-9223372036854775807-1,'e','|')",
		"inf":     "(1,1,-1e999,'a','|'),(2,2,-1,'b','|'),(3,3,0,'c','--'),(4,4,2.5,'d','|'),(5,5,1e999,'e','|'),(6,6,1e999,'f','|')",
		"mixed":   "(1,1,1.25,'a','|'),(2,2,100,'b','|'),(3,3,200,'c','|'),(4,4,300,'d','|'),(5,5,0.1,'e','|')",
		"nulls":   "(1,1,NULL,'a','|'),(2,2,7,NULL,'|'),(3,3,NULL,'c',NULL),(4,4,9,'d','|'),(5,5,2,'e','|')",
		"big":     "(1,1,1e308,'a','|'),(2,2,1e308,'b','|'),(3,3,-1e308,'c','|'),(4,4,1e-5,'d','|'),(5,5,3,'e','|')",
		"ties":    "(1,1,1,'a','|'),(2,1,2,'b','--'),(3,2,3,'','|'),(4,2,4,'d','|'),(5,3,5,'e','|')",
		"text":    "(1,1,'1','a','|'),(2,2,'2.5','b','|'),(3,3,'x','c','|'),(4,4,4,'d','|'),(5,5,NULL,'e','|')",
	}
	aggs := []string{"sum(v)", "total(v)", "avg(v)", "count(v)", "count(*)", "group_concat(s)", "group_concat(s, sep)", "string_agg(s, '+')"}
	bounds := []struct{ lo, hi string }{
		{"1 PRECEDING", "CURRENT ROW"}, {"2 PRECEDING", "1 PRECEDING"}, {"1 PRECEDING", "1 FOLLOWING"},
		{"CURRENT ROW", "1 FOLLOWING"}, {"1 FOLLOWING", "2 FOLLOWING"}, {"1 PRECEDING", "UNBOUNDED FOLLOWING"},
		{"CURRENT ROW", "UNBOUNDED FOLLOWING"}, {"2 FOLLOWING", "UNBOUNDED FOLLOWING"}, {"3 PRECEDING", "2 PRECEDING"},
	}
	var cases [][]string
	for name, rows := range datasets {
		_ = name
		for _, agg := range aggs {
			for _, mode := range []string{"ROWS", "GROUPS", "RANGE"} {
				for _, b := range bounds {
					q := fmt.Sprintf("SELECT id, %s OVER (ORDER BY k %s BETWEEN %s AND %s) FROM d ORDER BY id", agg, mode, b.lo, b.hi)
					cases = append(cases, []string{"CREATE TABLE d(id INTEGER PRIMARY KEY, k, v, s, sep)", "INSERT INTO d VALUES " + rows, q})
				}
			}
		}
	}
	sweepAll(t, cases)
}
