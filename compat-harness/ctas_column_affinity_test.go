// This file tests CREATE TABLE AS SELECT's derived column affinity.
// Affinity is derived from the output expression, not just the column type.
// Values inserted after creation must store correctly according to affinity.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ctasSetup covers one column per affinity and non-standard type names.
var ctasSetup = []string{
	`CREATE TABLE t(i INTEGER, r REAL, s TEXT, b BLOB, n, v VARCHAR(9), f FOO)`,
	`INSERT INTO t VALUES(1,2.5,'x',x'00',7,'v','f')`,
}

// ctasProbe returns schema and table_info queries for verification.
func ctasProbe(name string) []string {
	return []string{
		fmt.Sprintf(`SELECT sql FROM sqlite_master WHERE name='%s'`, name),
		fmt.Sprintf(`SELECT name, type FROM pragma_table_info('%s')`, name),
	}
}

func TestCTASColumnAffinity(t *testing.T) {
	for _, ctas := range []string{
		`CREATE TABLE u AS SELECT cast('9' AS integer) AS k, cast(2 AS text) AS m`,
		`CREATE TABLE u AS SELECT cast(i AS blob) AS a, cast(i AS numeric) AS b,
		   cast(i AS real) AS c, cast(i AS wibble) AS d FROM t`,
		`CREATE TABLE u AS SELECT i COLLATE nocase AS a, (SELECT s FROM t) AS b,
		   coalesce(i,r) AS c FROM t`,
		`CREATE TABLE u AS SELECT rowid AS a, t.rowid+1 AS b FROM t`,
		`CREATE TABLE u AS SELECT * FROM (SELECT cast(i AS text) AS z FROM t)`,
		`CREATE TABLE u AS SELECT i,r,s,b,n,v,f FROM t`,
		`CREATE TABLE u AS SELECT 1+1 AS n, 'x' AS o, 3 AS p, 2.5 AS q, x'00' AS r, NULL AS s`,
		`CREATE TABLE u AS SELECT i+0 AS a, -i AS b, abs(i) AS c, upper(s) AS e FROM t`,
		`CREATE TABLE u AS SELECT max(i) AS a, count(*) AS b, sum(r) AS c FROM t`,
		// Compound SELECT affinity (left-to-right, first-wins, with demotion).
		`CREATE TABLE u AS SELECT i FROM t UNION ALL SELECT s FROM t`,
		`CREATE TABLE u AS SELECT 1 AS a UNION ALL SELECT cast(1 AS text)`,
		`CREATE TABLE u AS SELECT cast(i AS text) AS a FROM t UNION SELECT cast(r AS text) FROM t`,
		`CREATE TABLE u AS SELECT i AS a FROM t UNION ALL SELECT x'00' FROM t`,
		`CREATE TABLE u AS SELECT cast(i AS integer) AS a FROM t UNION ALL SELECT x'00' FROM t`,
		`CREATE TABLE u AS SELECT i AS a FROM t UNION ALL SELECT x'00' FROM t UNION ALL SELECT x'01' FROM t`,
		`CREATE TABLE u AS SELECT i AS a FROM t UNION ALL SELECT abs(i) FROM t`,
		`CREATE TABLE u AS SELECT abs(i) AS a FROM t UNION ALL SELECT i FROM t`,
	} {
		stmts := append(append([]string{}, ctasSetup...), ctas)
		stmts = append(stmts, ctasProbe("u")...)
		differ(t, ctas, stmts)
	}
}

// ctasStorageProbe inserts all storage classes and verifies correct storage according to affinity.
func ctasStorageProbe(nCols int) []string {
	var cols, rows []string
	for c := 0; c < nCols; c++ {
		cols = append(cols, fmt.Sprintf("typeof(c%d)||':'||quote(c%d)", c, c))
	}
	for _, lit := range []string{`5`, `5.0`, `'5'`, `x'35'`, `NULL`, `'abc'`, `2.5`} {
		var vals []string
		for c := 0; c < nCols; c++ {
			vals = append(vals, lit)
		}
		rows = append(rows, "("+strings.Join(vals, ",")+")")
	}
	return []string{
		`INSERT INTO u VALUES` + strings.Join(rows, ","),
		`SELECT ` + strings.Join(cols, ", ") + ` FROM u ORDER BY rowid`,
	}
}

// ctasExprs are the output expressions the fuzz draws from, each paired with
// nothing but itself -- the affinity rule is per expression, so the fuzz's job
// is to mix shapes that HAVE an affinity with shapes that do not, in every
// position, and let the storage probe say whether the two engines agree.
var ctasExprs = []string{
	`i`, `r`, `s`, `b`, `n`, `v`, `f`,
	`cast(i AS text)`, `cast(i AS integer)`, `cast(i AS real)`,
	`cast(i AS numeric)`, `cast(i AS blob)`, `cast(s AS wibble)`,
	`i COLLATE nocase`, `s COLLATE binary`, `cast(i AS text) COLLATE nocase`,
	`(SELECT s FROM t)`, `(SELECT i FROM t)`, `(SELECT cast(r AS text) FROM t)`,
	`i+0`, `-i`, `abs(i)`, `upper(s)`, `1`, `'x'`, `2.5`, `x'00'`, `NULL`,
	`coalesce(i,s)`, `nullif(i,9)`, `iif(i>0,i,s)`, `rowid`, `t.rowid+1`,
	`case when i>0 then i else s end`,
	`(SELECT cast(i AS blob) FROM t)`,
}

func TestCTASColumnAffinityFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	for k := 0; k < 60; k++ {
		k := k
		name := fmt.Sprintf("c%02d", k)
		t.Run(name, func(t *testing.T) {
			n := 1 + rng.Intn(4)
			var sel []string
			for c := 0; c < n; c++ {
				sel = append(sel, fmt.Sprintf("%s AS c%d", ctasExprs[rng.Intn(len(ctasExprs))], c))
			}
			ctas := `CREATE TABLE u AS SELECT ` + strings.Join(sel, ", ") + ` FROM t`
			stmts := append(append([]string{}, ctasSetup...), ctas)
			stmts = append(stmts, ctasProbe("u")...)
			stmts = append(stmts, ctasStorageProbe(n)...)
			differ(t, name+" "+ctas, stmts)
		})
	}
}

// ctasCompoundArms are the per-arm expressions for the compound fuzz.
var ctasCompoundArms = []string{
	`i`, `s`, `r`, `f`, `b`, `n`, `v`,
	`cast(i AS text)`, `cast(i AS integer)`, `cast(i AS blob)`, `cast(i AS numeric)`,
	`1`, `'x'`, `2.5`, `NULL`, `x'00'`,
	`abs(i)`, `i||'q'`, `+i`, `-i`, `i COLLATE nocase`, `i+1`, `i=1`,
	`(SELECT s FROM t)`, `(SELECT cast(i AS blob) FROM t)`,
	`case when i>0 then i else s end`, `case when i>0 then 'a' end`,
	`max(i)`, `count(*)`,
}

var ctasCompoundOps = []string{" UNION ALL ", " UNION ", " EXCEPT ", " INTERSECT "}

// TestCTASCompoundAffinityFuzz tests compound affinity with randomized arms and operators.
func TestCTASCompoundAffinityFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	for k := 0; k < 120; k++ {
		k := k
		name := fmt.Sprintf("k%03d", k)
		t.Run(name, func(t *testing.T) {
			var parts []string
			for a := 0; a < 2+rng.Intn(3); a++ {
				e := ctasCompoundArms[rng.Intn(len(ctasCompoundArms))]
				if a == 0 {
					parts = append(parts, fmt.Sprintf(`SELECT %s AS z FROM t`, e))
				} else {
					parts = append(parts, fmt.Sprintf(`SELECT %s FROM t`, e))
				}
			}
			ctas := `CREATE TABLE u AS ` + strings.Join(parts, ctasCompoundOps[rng.Intn(len(ctasCompoundOps))])
			stmts := append(append([]string{}, ctasSetup...), ctas)
			stmts = append(stmts, ctasProbe("u")...)
			stmts = append(stmts,
				`INSERT INTO u VALUES(5),('5'),(5.0),(x'35'),(NULL)`,
				`SELECT typeof(z)||':'||quote(z) FROM u ORDER BY 1`)
			differ(t, name+" "+ctas, stmts)
		})
	}
}
