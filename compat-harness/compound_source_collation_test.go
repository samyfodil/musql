// Tests collation inheritance in compound selects used as sources.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

var cscSetup = []string{
	`CREATE TABLE t1(a COLLATE NOCASE, b COLLATE NOCASE)`,
	`INSERT INTO t1 VALUES('abc','BbB'),('ABC','ccc')`,
	`CREATE TABLE t2(c, d)`,
	`INSERT INTO t2 VALUES('xyz','QqQ'),('XYZ','bbb')`,
	`CREATE TABLE t3(e COLLATE RTRIM, f)`,
	`INSERT INTO t3 VALUES('r  ','s'),('r','t')`,
}

func TestCompoundSourceCollation(t *testing.T) {
	for _, q := range []string{
		// collate5.test's own shape: the leftmost arm's declared collation
		// governs the exposed column.
		`SELECT count(*) FROM (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2) WHERE b='BbB'`,
		`SELECT count(*) FROM (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2) WHERE b='bbb'`,
		`SELECT count(*) FROM (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2) WHERE a='ABC'`,
		// ...and the other way round, where the leftmost arm is the UNCOLLATED
		// one, so the exposed column must compare BINARY even though a later
		// arm is NOCASE. This is the case a "just use any arm" rule gets wrong.
		`SELECT count(*) FROM (SELECT c,d FROM t2 UNION ALL SELECT a,b FROM t1) WHERE d='BbB'`,
		`SELECT count(*) FROM (SELECT c,d FROM t2 UNION ALL SELECT a,b FROM t1) WHERE d='bbb'`,
		// every compound operator
		`SELECT count(*) FROM (SELECT a FROM t1 UNION SELECT c FROM t2) WHERE a='ABC'`,
		`SELECT count(*) FROM (SELECT a FROM t1 INTERSECT SELECT a FROM t1) WHERE a='ABC'`,
		`SELECT count(*) FROM (SELECT a FROM t1 EXCEPT SELECT c FROM t2) WHERE a='ABC'`,
		// RTRIM, whose difference is trailing blanks rather than case
		`SELECT count(*) FROM (SELECT e,f FROM t3 UNION ALL SELECT c,d FROM t2) WHERE e='r'`,
		// the compound's OWN dedup must stay collation-aware too
		`SELECT count(*) FROM (SELECT a FROM t1 UNION SELECT 'ABC')`,
		`SELECT count(*) FROM (SELECT c FROM t2 UNION SELECT 'XYZ')`,
		// Through CTE and VIEW.
		`WITH u AS (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2) SELECT count(*) FROM u WHERE b='BbB'`,
		`SELECT count(*) FROM (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2) x WHERE x.b='BbB'`,
		// ORDER BY over the exposed column.
		`SELECT b FROM (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2) ORDER BY b, a`,
		// GROUP BY and DISTINCT over the column.
		`SELECT count(*) FROM (SELECT DISTINCT b FROM (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2))`,
		// Explicit COLLATE in arm select lists.
		`SELECT count(*) FROM (SELECT a COLLATE BINARY AS z FROM t1 UNION ALL SELECT c FROM t2) WHERE z='ABC'`,
		`SELECT count(*) FROM (SELECT c COLLATE NOCASE AS z FROM t2 UNION ALL SELECT a FROM t1) WHERE z='XYZ'`,
	} {
		differ(t, q, append(append([]string{}, cscSetup...), q))
	}
}

// TestCompoundSourceCollationView tests view bodies with compound collations.
func TestCompoundSourceCollationView(t *testing.T) {
	for _, body := range []string{
		`SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2`,
		`SELECT c,d FROM t2 UNION ALL SELECT a,b FROM t1`,
		`SELECT a,b FROM t1 UNION SELECT c,d FROM t2`,
	} {
		stmts := append(append([]string{}, cscSetup...),
			`CREATE VIEW v AS `+body,
			`SELECT count(*) FROM v WHERE b='BbB'`,
			`SELECT count(*) FROM v WHERE b='bbb'`,
			`SELECT count(*) FROM v WHERE a='ABC'`,
			`SELECT a,b FROM v ORDER BY a,b`)
		differ(t, body, stmts)
	}
}

var cscColls = []string{"", " COLLATE NOCASE", " COLLATE RTRIM"}
var cscVals = []string{`'a'`, `'A'`, `'a '`, `'b'`, `'B  '`, `NULL`, `1`}
var cscOps = []string{" UNION ALL ", " UNION ", " EXCEPT ", " INTERSECT "}

// TestCompoundSourceCollationFuzz randomizes collations, operators and data to test leftmost-arm rule.
func TestCompoundSourceCollationFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260803))
	for k := 0; k < 80; k++ {
		k := k
		t.Run(fmt.Sprintf("s%02d", k), func(t *testing.T) {
			var stmts []string
			nArms := 2 + rng.Intn(2)
			var arms []string
			for a := 0; a < nArms; a++ {
				tn := fmt.Sprintf("z%d", a)
				stmts = append(stmts, fmt.Sprintf(`CREATE TABLE %s(p%s, q%s)`,
					tn, cscColls[rng.Intn(len(cscColls))], cscColls[rng.Intn(len(cscColls))]))
				var rows []string
				for r := 0; r < 2+rng.Intn(3); r++ {
					rows = append(rows, fmt.Sprintf("(%s,%s)",
						cscVals[rng.Intn(len(cscVals))], cscVals[rng.Intn(len(cscVals))]))
				}
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO %s VALUES%s`, tn, strings.Join(rows, ",")))
				arms = append(arms, fmt.Sprintf(`SELECT p,q FROM %s`, tn))
			}
			body := strings.Join(arms, cscOps[rng.Intn(len(cscOps))])
			probe := cscVals[rng.Intn(len(cscVals))]
			stmts = append(stmts,
				fmt.Sprintf(`SELECT count(*) FROM (%s) WHERE p=%s`, body, probe),
				fmt.Sprintf(`SELECT count(*) FROM (%s) WHERE q=%s`, body, probe),
				fmt.Sprintf(`SELECT quote(p)||'/'||quote(q) FROM (%s) ORDER BY 1`, body),
				fmt.Sprintf(`SELECT count(*) FROM (SELECT DISTINCT p FROM (%s))`, body))
			differ(t, fmt.Sprintf("s%02d %s", k, body), stmts)
		})
	}
}
