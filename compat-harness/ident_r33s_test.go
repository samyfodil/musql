// TestR33SIdentFold gates identifier case folding: SQLite folds identifiers
// ASCII-only; this engine initially used Unicode folding, which treats
// non-ASCII case variants as distinct.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r33sPairs are identifier pairs that fold together under Unicode rules and are
// DISTINCT under sqlite3UpperToLower. Each entry is (a, b, why).
//
// The last two are controls: "A"/"a" must still collapse (the ASCII rule is the
// whole of the rule), and "fi-ligature"/"fi" must stay distinct under both.
var r33sPairs = []struct{ a, b, why string }{
	{"Ä", "ä", "A-umlaut vs a-umlaut"},                       // 2 bytes each
	{"xÖy", "xöy", "O-umlaut embedded"},                      // non-ASCII in the middle
	{"İ", "i", "dotted capital I vs i (2 bytes vs 1)"},       // ToLower shortens
	{"K", "k", "Kelvin sign vs k (3 bytes vs 1)"},            // ToLower shortens
	{"Σ", "σ", "capital sigma vs small sigma"},               //
	{"ς", "σ", "final sigma vs small sigma"},                 // EqualFold-only pair
	{"ſ", "s", "long s vs s"},                                // EqualFold-only pair
	{"ẞ", "ß", "capital sharp s vs sharp s"},                 //
	{"ı", "i", "dotless i vs i (ToUpper-only pair)"},         //
	{"A", "a", "CONTROL: ASCII pair, must still collapse"},   //
	{"ﬁ", "fi", "CONTROL: fi ligature, distinct under both"}, //
}

// r33sQuote renders an identifier as a double-quoted SQL name.
func r33sQuote(s string) string { return `"` + s + `"` }

// TestR33SIdentFoldDerivedColumns tests a derived table with result columns
// that differ only by non-ASCII case.
func TestR33SIdentFoldDerivedColumns(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		src := fmt.Sprintf(`SELECT 1 AS %s, 2 AS %s`, qa, qb)
		for j, readout := range []string{
			`SELECT * FROM (%[1]s)`,
			`SELECT %[2]s FROM (%[1]s)`,
			`SELECT %[3]s FROM (%[1]s)`,
			`SELECT %[2]s, %[3]s FROM (%[1]s)`,
			`SELECT s.%[2]s, s.%[3]s FROM (%[1]s) AS s`,
			`SELECT * FROM (%[1]s) WHERE %[3]s = 2`,
			`SELECT %[2]s FROM (%[1]s) GROUP BY %[2]s`,
			`SELECT %[2]s FROM (%[1]s) ORDER BY %[3]s`,
			`SELECT count(*) FROM (%[1]s) WHERE %[2]s = 1`,
			`SELECT (SELECT %[3]s FROM (%[1]s))`,
		} {
			differ(t, fmt.Sprintf("r33s derived pair#%d(%s) readout#%d", i, p.why, j),
				[]string{fmt.Sprintf(readout, src, qa, qb)})
		}
	}
}

// TestR33SIdentFoldBaseTableColumns tests a base table with columns that
// differ only by non-ASCII case.
func TestR33SIdentFoldBaseTableColumns(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		stmts := []string{
			fmt.Sprintf(`CREATE TABLE t(%s INTEGER, %s TEXT)`, qa, qb),
			`PRAGMA table_info(t)`,
			`INSERT INTO t VALUES(1,'x')`,
			fmt.Sprintf(`SELECT %s, %s FROM t`, qa, qb),
			fmt.Sprintf(`SELECT * FROM t WHERE %s='x'`, qb),
			fmt.Sprintf(`SELECT %s FROM t GROUP BY %s`, qa, qa),
			fmt.Sprintf(`SELECT %s FROM t ORDER BY %s`, qb, qb),
			fmt.Sprintf(`CREATE INDEX ix ON t(%s)`, qb),
			`PRAGMA index_info(ix)`,
			fmt.Sprintf(`UPDATE t SET %s = 9`, qa),
			`SELECT * FROM t`,
		}
		differ(t, fmt.Sprintf("r33s basecol pair#%d(%s)", i, p.why), stmts)
	}
}

// TestR33SIdentFoldTableAndAlias tests object names: table names, aliases,
// and column qualifiers.
func TestR33SIdentFoldTableAndAlias(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s tablename pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE %s(x)`, qa),
			fmt.Sprintf(`CREATE TABLE %s(y)`, qb),
			fmt.Sprintf(`INSERT INTO %s VALUES(1)`, qa),
			fmt.Sprintf(`INSERT INTO %s VALUES(2)`, qb),
			fmt.Sprintf(`SELECT * FROM %s`, qa),
			fmt.Sprintf(`SELECT * FROM %s`, qb),
			`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`,
			fmt.Sprintf(`DROP TABLE %s`, qa),
			`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`,
		})
		differ(t, fmt.Sprintf("r33s alias pair#%d(%s)", i, p.why), []string{
			`CREATE TABLE t(x INTEGER, y INTEGER)`,
			`INSERT INTO t VALUES(1,2)`,
			fmt.Sprintf(`SELECT %s.x FROM t AS %s`, qa, qa),
			fmt.Sprintf(`SELECT %s.x FROM t AS %s`, qb, qa),
			fmt.Sprintf(`SELECT %s.x, %s.y FROM t AS %s JOIN t AS %s ON 1`, qa, qb, qa, qb),
		})
	}
}

// TestR33SIdentFoldCTE tests CTE names and explicit column lists.
func TestR33SIdentFoldCTE(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		for j, q := range []string{
			`WITH %[1]s AS (SELECT 1 AS v) SELECT * FROM %[1]s`,
			`WITH %[1]s AS (SELECT 1 AS v) SELECT * FROM %[2]s`,
			`WITH %[1]s AS (SELECT 1 AS v), %[2]s AS (SELECT 2 AS v) SELECT (SELECT v FROM %[1]s), (SELECT v FROM %[2]s)`,
			`WITH c(%[1]s,%[2]s) AS (SELECT 1,2) SELECT %[1]s, %[2]s FROM c`,
			`WITH c(%[1]s,%[2]s) AS (SELECT 1,2) SELECT * FROM c`,
			`WITH RECURSIVE %[1]s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM %[1]s WHERE n<3) SELECT * FROM %[1]s`,
		} {
			differ(t, fmt.Sprintf("r33s cte pair#%d(%s) q#%d", i, p.why, j),
				[]string{fmt.Sprintf(q, qa, qb)})
		}
	}
}

// TestR33SIdentFoldViewAndJoinKeys tests view column lists and join-key
// matching.
func TestR33SIdentFoldViewAndJoinKeys(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s view pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE VIEW v(%s,%s) AS SELECT 1,2`, qa, qb),
			`SELECT * FROM v`,
			`PRAGMA table_info(v)`,
			fmt.Sprintf(`SELECT %s FROM v`, qa),
			fmt.Sprintf(`SELECT %s FROM v`, qb),
		})
		differ(t, fmt.Sprintf("r33s using pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE l(%s INTEGER, m INTEGER)`, qa),
			fmt.Sprintf(`CREATE TABLE r(%s INTEGER, n INTEGER)`, qb),
			`INSERT INTO l VALUES(1,10),(2,20)`,
			`INSERT INTO r VALUES(1,100),(3,300)`,
			fmt.Sprintf(`SELECT * FROM l JOIN r USING(%s) ORDER BY 1`, qa),
			fmt.Sprintf(`SELECT * FROM l JOIN r USING(%s) ORDER BY 1`, qb),
			`SELECT * FROM l NATURAL JOIN r ORDER BY 1`,
			`SELECT * FROM l JOIN r ON 1 ORDER BY 1,2,3,4`,
		})
	}
}

// TestR33SIdentFoldGroupOrderByName tests name resolution in GROUP BY and
// ORDER BY.
func TestR33SIdentFoldGroupOrderByName(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s groupord pair#%d(%s)", i, p.why), []string{
			`CREATE TABLE g(x INTEGER, y INTEGER)`,
			`INSERT INTO g VALUES(1,5),(1,6),(2,7)`,
			fmt.Sprintf(`SELECT x AS %s, count(*) FROM g GROUP BY %s ORDER BY %s`, qa, qa, qa),
			fmt.Sprintf(`SELECT x AS %s, y AS %s FROM g ORDER BY %s, %s`, qa, qb, qb, qa),
			fmt.Sprintf(`SELECT x AS %s FROM g ORDER BY %s`, qa, qb),
			fmt.Sprintf(`SELECT x AS %s, sum(y) FROM g GROUP BY %s HAVING %s > 0 ORDER BY 1`, qa, qa, qa),
		})
	}
}
