package compat

import (
	"fmt"
	"testing"
)

// TestWindowMinMaxTieKeepsTheLastFrameRow gates that window min()/max() over
// moving frames resolves ties to the later row.
func TestWindowMinMaxTieKeepsTheLastFrameRow(t *testing.T) {
	setup := []string{
		`CREATE TABLE p(i, v COLLATE NOCASE, n)`,
		`INSERT INTO p VALUES(1,'a',1),(2,'B',2.0),(3,'c',2),(4,'A',1.0),(5,'b',3),(6,'C',3.0)`,
	}
	frames := []string{
		"",
		"ORDER BY i",
		"ORDER BY i ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING",
		"ORDER BY i ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW",
		"ORDER BY i ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING",
		"ORDER BY i ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING",
		"ORDER BY i ROWS BETWEEN 2 PRECEDING AND CURRENT ROW",
		"ORDER BY i ROWS BETWEEN CURRENT ROW AND 2 FOLLOWING",
		"ORDER BY i RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING",
		"ORDER BY i RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING",
		"ORDER BY i RANGE BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING",
		"ORDER BY i GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING",
		"ORDER BY i GROUPS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING",
		"ORDER BY i GROUPS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING",
		"ORDER BY i ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING EXCLUDE NO OTHERS",
		"ORDER BY i ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW",
		"ORDER BY i ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE GROUP",
		"ORDER BY i ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE TIES",
		"PARTITION BY v ORDER BY i ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING",
	}
	for _, f := range frames {
		f := f
		t.Run(f, func(t *testing.T) {
			var stmts []string
			stmts = append(stmts, setup...)
			for _, col := range []string{"v", "n"} {
				for _, fn := range []string{"min", "max"} {
					stmts = append(stmts, fmt.Sprintf(
						"SELECT i, quote(%s(%s) OVER (%s)) FROM p ORDER BY i", fn, col, f))
				}
			}
			differ(t, "winminmaxtie/"+f, stmts)
		})
	}
}
