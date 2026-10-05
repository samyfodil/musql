// This file tests SELECT DISTINCT's collating sequence against C SQLite.
// DISTINCT is collation-sensitive: declared collations apply to dedup,
// explicit COLLATE overrides it, and expressions lose it.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// dcSetup tables with NOCASE, RTRIM, and default collations.
var dcSetup = []string{
	`CREATE TABLE t1(a COLLATE NOCASE, b)`,
	`INSERT INTO t1 VALUES('one',1),('ONE',2),('One',3),('two',4),('TWO',5)`,
	`CREATE TABLE t2(a COLLATE RTRIM, b)`,
	`INSERT INTO t2 VALUES('x  ',1),('x',2),('y',3),('y ',4)`,
	`CREATE TABLE t3(a COLLATE NOCASE, c COLLATE RTRIM, d)`,
	`INSERT INTO t3 VALUES('p','q  ',1),('P','q',2),('p','q ',3),('r','s',4)`,
	`CREATE TABLE t4(a, b)`,
	`INSERT INTO t4 VALUES('one',1),('ONE',2),(NULL,3),(NULL,4)`,
}

func TestDistinctCollation(t *testing.T) {
	for _, q := range []string{
		// Declared collations, explicit COLLATE overrides, per-column.
		`SELECT DISTINCT a FROM t1 ORDER BY b`,
		`SELECT DISTINCT a FROM t2 ORDER BY b`,
		`SELECT count(*) FROM (SELECT DISTINCT a FROM t1)`,
		`SELECT count(*) FROM (SELECT DISTINCT a FROM t2)`,
		`SELECT count(*) FROM (SELECT DISTINCT a COLLATE BINARY FROM t1)`,
		`SELECT count(*) FROM (SELECT DISTINCT a COLLATE NOCASE FROM t2)`,
		`SELECT count(*) FROM (SELECT DISTINCT a COLLATE RTRIM FROM t1)`,
		`SELECT count(*) FROM (SELECT DISTINCT a, b FROM t1)`,
		`SELECT count(*) FROM (SELECT DISTINCT a, a COLLATE BINARY FROM t1)`,
		`SELECT DISTINCT a, c FROM t3 ORDER BY d`,
		`SELECT count(*) FROM (SELECT DISTINCT a, c FROM t3)`,
		// Expressions and functions.
		`SELECT count(*) FROM (SELECT DISTINCT a||'' FROM t1)`,
		`SELECT count(*) FROM (SELECT DISTINCT upper(a) FROM t1)`,
		`SELECT count(*) FROM (SELECT DISTINCT +a FROM t1)`,
		`SELECT count(*) FROM (SELECT DISTINCT CAST(a AS TEXT) FROM t1)`,
		// NULLs, ORDER BY, LIMIT, IN.
		`SELECT count(*) FROM (SELECT DISTINCT a FROM t4)`,
		`SELECT DISTINCT a FROM t4 ORDER BY b`,
		`SELECT DISTINCT a FROM t1 ORDER BY a`,
		`SELECT DISTINCT a FROM t2 ORDER BY a`,
		`SELECT DISTINCT a FROM t3 ORDER BY a DESC`,
		`SELECT DISTINCT a FROM t1 ORDER BY b LIMIT 1`,
		`SELECT count(*) FROM t1 WHERE a IN (SELECT DISTINCT a FROM t1)`,
		`SELECT count(*) FROM t1 WHERE a COLLATE BINARY IN (SELECT DISTINCT a FROM t1)`,
	} {
		differ(t, q, append(append([]string{}, dcSetup...), q))
	}
}

// dcVals are the values the fuzz draws from: the pairs that separate BINARY
// from NOCASE from RTRIM, plus NULL and a numeric, since the dedup runs over
// whole rows and a collation applies only to TEXT.
var dcVals = []string{`'a'`, `'A'`, `'a '`, `'A  '`, `'b'`, `'B '`, `NULL`, `1`, `'1'`, `''`, `' '`}

var dcColls = []string{"", " COLLATE NOCASE", " COLLATE RTRIM", " COLLATE BINARY"}

// TestDistinctCollationFuzz randomizes the declared collations, the explicit
// ones, the column count and the DATA. The data axis is what matters: which
// rows collapse depends entirely on where the case and trailing-blank
// variants land, so a fixed fixture agrees on far more than the rule does.
func TestDistinctCollationFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260803))
	for k := 0; k < 90; k++ {
		k := k
		t.Run(fmt.Sprintf("d%02d", k), func(t *testing.T) {
			nCols := 1 + rng.Intn(3)
			var decls []string
			for c := 0; c < nCols; c++ {
				decls = append(decls, fmt.Sprintf("c%d%s", c, dcColls[rng.Intn(len(dcColls))]))
			}
			stmts := []string{fmt.Sprintf(`CREATE TABLE t(%s, ord)`, strings.Join(decls, ", "))}
			var rows []string
			for r := 0; r < 3+rng.Intn(5); r++ {
				var vals []string
				for c := 0; c < nCols; c++ {
					vals = append(vals, dcVals[rng.Intn(len(dcVals))])
				}
				rows = append(rows, fmt.Sprintf("(%s,%d)", strings.Join(vals, ","), r))
			}
			stmts = append(stmts, `INSERT INTO t VALUES`+strings.Join(rows, ","))

			// Generate select list with declared collations, explicit COLLATE, and expressions.
			var sel []string
			for c := 0; c < nCols; c++ {
				switch rng.Intn(5) {
				case 0:
					sel = append(sel, fmt.Sprintf("c%d COLLATE BINARY", c))
				case 1:
					sel = append(sel, fmt.Sprintf("c%d COLLATE NOCASE", c))
				case 2:
					sel = append(sel, fmt.Sprintf("c%d||''", c))
				default:
					sel = append(sel, fmt.Sprintf("c%d", c))
				}
			}
			list := strings.Join(sel, ", ")
			stmts = append(stmts,
				fmt.Sprintf(`SELECT count(*) FROM (SELECT DISTINCT %s FROM t)`, list),
				fmt.Sprintf(`SELECT quote(x0) FROM (SELECT DISTINCT %s AS x0 FROM t) ORDER BY 1`, sel[0]),
				fmt.Sprintf(`SELECT DISTINCT %s FROM t ORDER BY 1`, list))
			differ(t, fmt.Sprintf("d%02d %s", k, list), stmts)
		})
	}
}
