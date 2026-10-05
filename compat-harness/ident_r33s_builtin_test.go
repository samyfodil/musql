// Tests for identifier folding with builtin names and values. Unicode folding
// must not affect ASCII-only builtins or collation comparisons.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// The four runes that Unicode-fold onto an ASCII letter.
const (
	r33sKelvin  = "K" // KELVIN SIGN      -> ToLower "k"
	r33sDotI    = "İ" // I WITH DOT ABOVE -> ToLower "i"
	r33sLongS   = "ſ" // LATIN SMALL LONG S -> EqualFold "s", ToUpper "S"
	r33sDotless = "ı" // DOTLESS I          -> ToUpper "I"
)

// TestR33SIdentFoldBuiltinNames verifies that Unicode-folded names don't match
// builtins.
func TestR33SIdentFoldBuiltinNames(t *testing.T) {
	cases := [][]string{
		{`SELECT min(1,2)`},
		{`SELECT MIN(1,2)`},
		{`SELECT M` + r33sDotI + `N(1,2)`},
		{`SELECT SUM(1)`},
		{`SELECT ` + r33sLongS + `UM(1)`},
		{`SELECT likely(1)`},
		{`SELECT LIKELY(1)`},
		{`SELECT LI` + r33sKelvin + `ELY(1)`},
		{`SELECT length('ab')`},
		{`SELECT LENGTH('ab')`},
		{`SELECT IFNULL(NULL,1)`},
		{`SELECT ` + r33sDotI + `FNULL(NULL,1)`},
		{`SELECT INSTR('abc','b')`},
		{`SELECT IN` + r33sLongS + `TR('abc','b')`},
		{`SELECT L` + r33sDotless + `KELY(1)`},
		{`SELECT 'a' = 'A' COLLATE NOCASE`},
		{`SELECT 'a' = 'A' COLLATE nocase`},
		{`SELECT 'a' = 'A' COLLATE NOCA` + r33sLongS + `E`},
		{`SELECT 'a' = 'A' COLLATE noca` + r33sLongS + `e`},
		{`SELECT 'a ' = 'a' COLLATE RTRIM`},
		{`SELECT 'a ' = 'a' COLLATE RTR` + r33sDotI + `M`},
		{`SELECT 'a' = 'a' COLLATE BINARY`},
		{`SELECT 'a' = 'a' COLLATE B` + r33sDotless + `NARY`},
		{`CREATE TABLE cc(x TEXT COLLATE NOCASE)`, `PRAGMA table_info(cc)`},
		{`CREATE TABLE cd(x TEXT COLLATE NOCA` + r33sLongS + `E)`, `PRAGMA table_info(cd)`},
		{`CREATE TABLE r1(a)`, `INSERT INTO r1 VALUES(7)`, `SELECT rowid FROM r1`},
		{`CREATE TABLE r2(a)`, `INSERT INTO r2 VALUES(7)`, `SELECT ROWID FROM r2`},
		{`CREATE TABLE r3(a)`, `INSERT INTO r3 VALUES(7)`, `SELECT ROW` + r33sDotI + `D FROM r3`},
		{`CREATE TABLE r4(a)`, `INSERT INTO r4 VALUES(7)`, `SELECT OID FROM r4`},
		{`CREATE TABLE r5(a)`, `INSERT INTO r5 VALUES(7)`, `SELECT O` + r33sDotI + `D FROM r5`},
		{`CREATE TABLE r6(a)`, `INSERT INTO r6 VALUES(7)`, `SELECT _ROWID_ FROM r6`},
		{`CREATE TABLE r7(a)`, `INSERT INTO r7 VALUES(7)`, `SELECT _ROW` + r33sDotI + `D_ FROM r7`},
		{`CREATE TABLE "SQLITE_zz"(x)`},
		{`CREATE TABLE "SQL` + r33sDotI + `TE_zz"(x)`, `SELECT name FROM sqlite_master`},
		{`CREATE TABLE "sql` + r33sDotless + `te_zz"(x)`, `SELECT name FROM sqlite_master`},
		{`CREATE TABLE q(x)`, `INSERT INTO q VALUES(1)`, `SELECT * FROM main.q`},
		{`CREATE TABLE q(x)`, `INSERT INTO q VALUES(1)`, `SELECT * FROM MAIN.q`},
		{`CREATE TABLE q(x)`, `INSERT INTO q VALUES(1)`, `SELECT * FROM "MA` + r33sDotI + `N".q`},
		{`CREATE TABLE q(x)`, `INSERT INTO q VALUES(1)`, `SELECT * FROM "ma` + r33sDotless + `n".q`},
		{`CREATE TABLE q(x)`, `INSERT INTO q VALUES(1)`, `SELECT * FROM "TEMP".q`},
	}
	for i, stmts := range cases {
		differ(t, fmt.Sprintf("r33s builtin #%d", i), stmts)
	}
}

// TestR33SNocaseAndLikeStayASCII verifies that NOCASE and LIKE remain ASCII-only.
func TestR33SNocaseAndLikeStayASCII(t *testing.T) {
	var eqs, likes string
	for i, p := range r33sPairs {
		if i > 0 {
			eqs += ", "
			likes += ", "
		}
		eqs += fmt.Sprintf("'%s'='%s' COLLATE NOCASE", p.a, p.b)
		likes += fmt.Sprintf("'%s' LIKE '%s'", p.a, p.b)
	}
	cases := [][]string{
		{`SELECT ` + eqs},
		{`SELECT ` + likes},
		{`SELECT lower('A` + r33sKelvin + `'), upper('a` + r33sLongS + `'), lower('` + r33sDotI + `'), upper('` + r33sDotless + `')`},
		// Ordering under NOCASE: ']' (0x5D) must still land between 'B' and
		// 'b', and the non-ASCII pairs must still sort as distinct bytes.
		{
			`CREATE TABLE n(x TEXT COLLATE NOCASE)`,
			`INSERT INTO n VALUES('` + r33sPairs[0].a + `'),('` + r33sPairs[0].b + `'),('A'),('a'),('a]b'),('ABC'),('` + r33sPairs[4].a + `'),('` + r33sPairs[4].b + `')`,
			`SELECT x FROM n ORDER BY x, rowid`,
			`SELECT count(DISTINCT x) FROM n`,
			`SELECT * FROM (SELECT x, count(*) AS c FROM n GROUP BY x) ORDER BY 1, 2`,
		},
		{
			`CREATE TABLE u(x TEXT COLLATE NOCASE UNIQUE)`,
			`INSERT INTO u VALUES('` + r33sPairs[0].a + `')`,
			`INSERT INTO u VALUES('` + r33sPairs[0].b + `')`,
			`INSERT INTO u VALUES('` + r33sPairs[4].a + `')`,
			`INSERT INTO u VALUES('` + r33sPairs[4].b + `')`,
			`INSERT INTO u VALUES('A')`,
			`INSERT INTO u VALUES('a')`,
			`SELECT x FROM u ORDER BY rowid`,
		},
		{
			`CREATE TABLE b("` + r33sPairs[0].a + `" TEXT COLLATE NOCASE, "` + r33sPairs[0].b + `" TEXT COLLATE NOCASE)`,
			`INSERT INTO b VALUES('` + r33sPairs[0].a + `','` + r33sPairs[0].b + `')`,
			`SELECT "` + r33sPairs[0].a + `"="` + r33sPairs[0].b + `", "` + r33sPairs[0].a + `"="` + r33sPairs[0].b + `" COLLATE NOCASE FROM b`,
			`SELECT "` + r33sPairs[0].a + `", "` + r33sPairs[0].b + `" FROM b`,
		},
	}
	for i, stmts := range cases {
		differ(t, fmt.Sprintf("r33s nocase #%d", i), stmts)
	}
}

// TestR33SIdentFoldWritePath verifies identifier folding in write statements.
func TestR33SIdentFoldWritePath(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s write pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE w(%s INTEGER, %s INTEGER, z INTEGER)`, qa, qb),
			fmt.Sprintf(`INSERT INTO w(%s,%s,z) VALUES(1,2,3)`, qa, qb),
			fmt.Sprintf(`INSERT INTO w(%s,z) VALUES(4,5)`, qb),
			`SELECT * FROM w ORDER BY rowid`,
			fmt.Sprintf(`UPDATE w SET %s = 99 WHERE z = 3`, qb),
			`SELECT * FROM w ORDER BY rowid`,
			fmt.Sprintf(`ALTER TABLE w RENAME COLUMN %s TO nz`, qb),
			`SELECT * FROM w ORDER BY rowid`,
			`PRAGMA table_info(w)`,
		})
		differ(t, fmt.Sprintf("r33s constraint pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE ch(%s INTEGER, %s INTEGER, CHECK(%s > 0))`, qa, qb, qb),
			`INSERT INTO ch VALUES(1,1)`,
			`INSERT INTO ch VALUES(1,0)`,
			`SELECT * FROM ch`,
			fmt.Sprintf(`CREATE TABLE uq(%s INTEGER, %s INTEGER, UNIQUE(%s))`, qa, qb, qb),
			`INSERT INTO uq VALUES(1,1)`,
			`INSERT INTO uq VALUES(2,1)`,
			fmt.Sprintf(`INSERT INTO uq VALUES(1,2) ON CONFLICT(%s) DO UPDATE SET %s = 77`, qb, qa),
			`SELECT * FROM uq ORDER BY rowid`,
		})
		differ(t, fmt.Sprintf("r33s trigger pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE tg(%s INTEGER, %s INTEGER)`, qa, qb),
			`CREATE TABLE log(v INTEGER)`,
			fmt.Sprintf(`CREATE TRIGGER trg AFTER INSERT ON tg BEGIN INSERT INTO log VALUES(NEW.%s); END`, qb),
			`INSERT INTO tg VALUES(1,2)`,
			`SELECT * FROM log`,
		})
	}
}
