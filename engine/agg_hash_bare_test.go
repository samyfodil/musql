package engine

import "testing"

// TestHashGroupByServesBareColumns: a GROUP BY whose select list reads a bare
// column takes the hash path.
func TestHashGroupByServesBareColumns(t *testing.T) {
	p := argRegsFixture(t)
	for _, sql := range []string{
		`SELECT v, sum(id) FROM t GROUP BY k+1`,
		`SELECT k, max(v), s FROM t GROUP BY k`,
		`SELECT k, sum(v) FROM t GROUP BY k HAVING v > 0`,
	} {
		prog, _ := compiledAgg(t, p, sql)
		hash := false
		for _, in := range prog.Insns {
			hash = hash || in.Op == OpHashAggStep
		}
		if !hash {
			t.Errorf("%s: not on the hash path", sql)
		}
	}
}
