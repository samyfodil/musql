// Tests the aggregate FILTER (WHERE ...) modifier, which filters rows into
// each aggregate.
package compat

import (
	"fmt"
	"testing"
)

var filterSetup = []string{
	`CREATE TABLE t(g TEXT, v INTEGER)`,
	`INSERT INTO t VALUES('a',1),('a',2),('a',NULL),('b',10),('b',20)`,
}

var filterCorpus = []string{
	`SELECT count(*) FILTER (WHERE v>1) FROM t`,
	`SELECT count(v) FILTER (WHERE v>1) FROM t`,
	`SELECT sum(v) FILTER (WHERE v>1) FROM t`,
	`SELECT avg(v) FILTER (WHERE v>1) FROM t`,
	`SELECT min(v) FILTER (WHERE v>1), max(v) FILTER (WHERE v<20) FROM t`,
	`SELECT count(*) FILTER (WHERE v IS NULL) FROM t`,
	// A false or NULL condition skips the row, matching WHERE's truthiness.
	`SELECT count(*) FILTER (WHERE 0) FROM t`,
	`SELECT count(*) FILTER (WHERE NULL) FROM t`,
	// Per-group, and alongside an unfiltered aggregate over the same rows.
	`SELECT g, count(*) FILTER (WHERE v>1), count(*) FROM t GROUP BY g ORDER BY g`,
	`SELECT g, sum(v) FILTER (WHERE v>5) FROM t GROUP BY g ORDER BY g`,
	`SELECT group_concat(v) FILTER (WHERE v>1) FROM t`,
	`SELECT count(DISTINCT v) FILTER (WHERE v>1) FROM t`,
	`SELECT total(v) FILTER (WHERE v>100) FROM t`,
	`SELECT g, count(*) FROM t GROUP BY g HAVING count(*) FILTER (WHERE v>1) > 0 ORDER BY g`,
	// FILTER on a window aggregate.
	`SELECT v, count(*) FILTER (WHERE v>1) OVER (ORDER BY v) FROM t ORDER BY v`,
	`SELECT v, sum(v) FILTER (WHERE v>1) OVER (PARTITION BY g ORDER BY v) FROM t ORDER BY v`,
}

func TestFilterClauseMatchesCSQLite(t *testing.T) {
	wrong := 0
	for _, q := range filterCorpus {
		stmts := append(append([]string{}, filterSetup...), q)
		c := run(t, "cgo", stmts)
		m := run(t, "musql", stmts)
		ci, mi := c[len(c)-1], m[len(m)-1]
		if fmt.Sprint(ci) != fmt.Sprint(mi) {
			wrong++
			t.Errorf("[%s] DIVERGES\n  cgo:    %v\n  musql: %v", q, ci, mi)
		}
	}
	t.Logf("FILTER parity: %d statements, wrong=%d", len(filterCorpus), wrong)
	if wrong != 0 {
		t.Fatalf("FILTER parity FAILED: wrong=%d (must be 0)", wrong)
	}
}
