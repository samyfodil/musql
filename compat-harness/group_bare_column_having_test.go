// Tests bare columns (not aggregates or GROUP BY keys) in HAVING and ORDER BY
// clauses. The key behavior is that bare columns reference the anchor row of
// the group, which can move if min/max aggregates are evaluated.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

var gbhSetup = []string{
	`CREATE TABLE t(k,v,w)`,
	`INSERT INTO t VALUES(1,50,'z'),(1,10,'a'),(2,1,'m'),(2,20,'b'),(3,30,'k')`,
	`CREATE TABLE u(g, n, s)`,
	`INSERT INTO u VALUES('a',3,'x'),('a',1,'y'),('b',2,'z'),('b',9,'w'),('c',4,'v')`,
}

func TestGroupBareColumnInHaving(t *testing.T) {
	for _, q := range []string{
		// Bare column is the group's first row
		`SELECT k, v, w FROM t GROUP BY k HAVING v>5 ORDER BY k`,
		`SELECT k FROM t GROUP BY k HAVING v=50 ORDER BY k`,
		`SELECT k FROM t GROUP BY k HAVING w='z' ORDER BY k`,
		`SELECT k FROM t GROUP BY k HAVING v>5 AND w<'n' ORDER BY k`,
		`SELECT k FROM t GROUP BY k HAVING v>5 OR w='m' ORDER BY k`,
		// Min/max aggregates move anchor, affecting bare column value
		`SELECT k, min(v) FROM t GROUP BY k HAVING v=10 ORDER BY k`,
		`SELECT k, max(v) FROM t GROUP BY k HAVING v=50 ORDER BY k`,
		`SELECT k, min(v), w FROM t GROUP BY k HAVING w='a' ORDER BY k`,
		`SELECT k, max(v), w FROM t GROUP BY k HAVING w='z' ORDER BY k`,
		// Min/max aggregates in HAVING clause
		`SELECT k, w FROM t GROUP BY k HAVING max(v)>5 AND w='z' ORDER BY k`,
		`SELECT k, w FROM t GROUP BY k HAVING min(v)=10 ORDER BY k`,
		// Bare column with aggregate and group key
		`SELECT k, count(*), v FROM t GROUP BY k HAVING count(*)>1 AND v>5 ORDER BY k`,
		// ORDER BY with bare columns
		`SELECT k FROM t GROUP BY k ORDER BY v, k`,
		`SELECT k FROM t GROUP BY k ORDER BY w DESC, k`,
		`SELECT k FROM t GROUP BY k ORDER BY v, w, k`,
		`SELECT k, min(v) FROM t GROUP BY k ORDER BY w, k`,
		// HAVING and ORDER BY with bare columns
		`SELECT k FROM t GROUP BY k HAVING v>5 ORDER BY w, k`,
		// Second fixture with different anchor/min/max positions
		`SELECT g, n, s FROM u GROUP BY g HAVING n>1 ORDER BY g`,
		`SELECT g, min(n), s FROM u GROUP BY g HAVING s='y' ORDER BY g`,
		`SELECT g FROM u GROUP BY g ORDER BY s, g`,
		// Control cases: select list, group keys, aggregates only
		`SELECT k, v, w FROM t GROUP BY k ORDER BY k`,
		`SELECT k FROM t GROUP BY k HAVING k>1 ORDER BY k`,
		`SELECT k FROM t GROUP BY k HAVING max(v)>5 ORDER BY k`,
		`SELECT k FROM t GROUP BY k HAVING count(*)>1 ORDER BY k`,
	} {
		differ(t, q, append(append([]string{}, gbhSetup...), q))
	}
}

// TestGroupBareColumnInHavingFuzz randomizes the fixture and the clause, since
// which group survives depends entirely on which row the anchor lands on --
// and that moves with the data, with whether a min/max is present, and with
// which of min or max it is.
func TestGroupBareColumnInHavingFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260803))
	aggs := []string{"", ", min(v)", ", max(v)", ", count(*)", ", min(v), max(v)"}
	preds := []string{"v>5", "v<20", "w='z'", "w>'k'", "v>5 AND w<'z'", "v=1 OR w='k'", "v IS NOT NULL"}
	// Every ORDER BY ends in the group key: a bare column can TIE across
	// groups, and SQL leaves a tie's relative order unspecified, so an
	// order-sensitive compare over one would fail on a legitimate
	// difference rather than on this rule.
	ords := []string{"v, k", "w, k", "w DESC, k", "v, w, k", "k, v"}
	for i := 0; i < 90; i++ {
		i := i
		t.Run(fmt.Sprintf("h%02d", i), func(t *testing.T) {
			var rows []string
			for r := 0; r < 4+rng.Intn(5); r++ {
				k := 1 + rng.Intn(3)
				v := rng.Intn(60)
				w := string(rune('a' + rng.Intn(6)))
				rows = append(rows, fmt.Sprintf("(%d,%d,'%s')", k, v, w))
			}
			stmts := []string{
				`CREATE TABLE t(k,v,w)`,
				`INSERT INTO t VALUES` + strings.Join(rows, ","),
			}
			agg := aggs[rng.Intn(len(aggs))]
			pred := preds[rng.Intn(len(preds))]
			ord := ords[rng.Intn(len(ords))]
			stmts = append(stmts,
				fmt.Sprintf(`SELECT k%s FROM t GROUP BY k HAVING %s ORDER BY k`, agg, pred),
				fmt.Sprintf(`SELECT k, v, w%s FROM t GROUP BY k HAVING %s ORDER BY k`, agg, pred),
				fmt.Sprintf(`SELECT k%s FROM t GROUP BY k ORDER BY %s`, agg, ord),
				fmt.Sprintf(`SELECT count(*) FROM (SELECT k FROM t GROUP BY k HAVING %s)`, pred))
			differ(t, fmt.Sprintf("h%02d %s | %s | %s", i, agg, pred, ord), stmts)
		})
	}
}
