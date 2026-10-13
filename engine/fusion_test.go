package engine

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestFusionAnswersAsUnfused runs the statements the program JIT serves three
// ways -- the plain VDBE, compiled programs fused, compiled programs unfused
// (WithoutFusion) -- and requires the same rows from all three. Fusion only
// rewrites how a program is emitted, so any difference is a fusion bug.
func TestFusionAnswersAsUnfused(t *testing.T) {
	if !JITEnabled() {
		t.Skip("no JIT on this machine")
	}
	defer Configure()
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w INTEGER, s TEXT)`}
	rng := rand.New(rand.NewSource(11))
	for i := 1; i <= 1500; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,%d,'%s%d')`,
			i, rng.Intn(10), rng.Intn(2000)-1000, rng.Intn(50), []string{"ab", "Abc", "xyz", "q"}[rng.Intn(4)], i%17))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT count(*) FROM t WHERE v > 0`,
		`SELECT count(*) FROM t WHERE v > 0 AND k < 5`,
		`SELECT count(*) FROM t WHERE v > 0 AND k <> 3 AND w <= 20`,
		`SELECT count(*) FROM t WHERE v > 500 OR k = 2`,
		`SELECT count(*) FROM t WHERE k = 1 OR k = 2 OR k = 7`,
		`SELECT count(*) FROM t WHERE v BETWEEN -100 AND 100`,
		`SELECT count(*) FROM t WHERE NOT (v < 0)`,
		`SELECT count(*) FROM t WHERE v + w > 10`,
		`SELECT count(*) FROM t WHERE v * 2 < w - 5`,
		`SELECT count(*) FROM t WHERE k = w % 10`,
		`SELECT count(*) FROM t WHERE abs(v) < 100`,
		`SELECT sum(v) FROM t WHERE k > 4`,
		`SELECT sum(v) FROM t WHERE v > 0 OR w = 7`,
		`SELECT sum(w) FROM t WHERE v BETWEEN 0 AND 300 AND k < 8`,
		`SELECT min(v) FROM t WHERE k = 3`,
		`SELECT max(v) FROM t WHERE w > 25 AND k <> 9`,
		`SELECT min(w), max(w) FROM t WHERE v < -500`,
		`SELECT count(*) FROM t WHERE s LIKE 'ab%'`,
		`SELECT count(*) FROM t WHERE s LIKE '%c1%' AND v > 0`,
		`SELECT count(*) FROM t WHERE length(s) > 3`,
		`SELECT id FROM t WHERE v > 900 AND k < 3 ORDER BY id`,
		`SELECT count(*) FROM t WHERE id < 700 AND v > 0`,
	}
	render := func(rows [][]Value) string { return fmt.Sprint(rows) }
	jit.FusedStepsForTest()
	for _, q := range queries {
		want := render(p.mustPlain(q))
		Configure()
		_, fused, err := p.fast(q)
		if err != nil {
			t.Fatalf("%s fused: %v", q, err)
		}
		Configure(WithoutFusion())
		_, unfused, err := p.fast(q)
		if err != nil {
			t.Fatalf("%s unfused: %v", q, err)
		}
		if got := render(fused); got != want {
			t.Errorf("%s: fused %s, VDBE %s", q, got, want)
		}
		if got := render(unfused); got != want {
			t.Errorf("%s: unfused %s, VDBE %s", q, got, want)
		}
	}
	if jit.FusedStepsForTest() == 0 {
		t.Fatal("fusion removed no step from any program: this compared unfused with itself")
	}
}
