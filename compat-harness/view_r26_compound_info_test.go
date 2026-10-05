// PRAGMA table_info/xinfo/table_list over COMPOUND views.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

var cviSetup = []string{
	`CREATE TABLE t1(a INTEGER, b TEXT)`,
	`CREATE TABLE t2(f NUMERIC, g VARCHAR(9), h DoUbLe)`,
	`CREATE TABLE t3(m MYBLOB, n)`,
	`INSERT INTO t1 VALUES(1,'p')`,
	`INSERT INTO t2 VALUES(1,'5',2.0)`,
	`INSERT INTO t3 VALUES(x'00',7)`,
}

func cviProbe() []string {
	return []string{
		// "SELECT *", not a written column list: "notnull" is a KEYWORD in
		// SQLite's expression grammar, so naming that column makes the whole
		// statement a syntax error there (and not here -- a separate,
		// pre-existing laxity this gate must not depend on).
		`SELECT * FROM pragma_table_info('v')`,
		`SELECT * FROM pragma_table_xinfo('v')`,
		`SELECT * FROM pragma_table_list WHERE name='v'`,
	}
}

// TestCompoundViewInfo pins the shapes the rule turns on, one at a time.
func TestCompoundViewInfo(t *testing.T) {
	for _, body := range []string{
		// The pairings the reverted attempt's matrix was built from, kept as
		`SELECT a FROM t1 UNION ALL SELECT a FROM t1`,
		`SELECT a FROM t1 UNION ALL SELECT b FROM t1`,
		`SELECT a FROM t1 UNION ALL SELECT a+1 FROM t1`,
		`SELECT g FROM t2 UNION ALL SELECT f FROM t2`,
		`SELECT m FROM t3 UNION ALL SELECT g FROM t2`,
		`SELECT n FROM t3 UNION ALL SELECT a FROM t1`,
		`SELECT +g FROM t2 UNION ALL SELECT 44`,
		`SELECT +a FROM t1 UNION ALL SELECT 'q'`,
		`SELECT -a FROM t1 UNION ALL SELECT 'q'`,
		`SELECT 44 UNION ALL SELECT +g FROM t2`,
		`SELECT g COLLATE nocase FROM t2 UNION ALL SELECT g FROM t2`,
		`SELECT CAST(a AS INT) COLLATE binary FROM t1 UNION ALL SELECT a FROM t1`,
		`SELECT g FROM t2 UNION ALL SELECT g COLLATE nocase FROM t2`,
		`SELECT a,b FROM t1 UNION ALL SELECT 2,2`,
		`SELECT g FROM t2 UNION ALL SELECT g FROM t2`,
		`SELECT h FROM t2 UNION ALL SELECT h FROM t2`,
		`SELECT m FROM t3 UNION ALL SELECT m FROM t3`,
		`SELECT CAST(a AS INT) FROM t1 UNION ALL SELECT x'00'`,
		`SELECT CAST(a AS REAL) FROM t1 UNION ALL SELECT x'00'`,
		`SELECT a FROM t1 UNION ALL SELECT x'00'`,
		`SELECT m FROM t3 UNION ALL SELECT 1`,
		`SELECT m FROM t3 UNION ALL SELECT 'q'`,
		`SELECT NULL UNION ALL SELECT m FROM t3 UNION ALL SELECT 'q'`,
		`SELECT NULL UNION ALL SELECT NULL UNION ALL SELECT a FROM t1`,
		`SELECT a FROM t1 UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 'q'`,
		`SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3`,
		`SELECT g FROM t2 UNION SELECT a FROM t1`,
		`SELECT g FROM t2 INTERSECT SELECT a FROM t1`,
		`SELECT g FROM t2 EXCEPT SELECT a FROM t1`,
		`SELECT a FROM t1 UNION ALL SELECT * FROM (SELECT g FROM t2 UNION ALL SELECT 1)`,
		`SELECT (SELECT g FROM t2) UNION ALL SELECT a FROM t1`,
		`SELECT a FROM t1 UNION ALL SELECT (SELECT g FROM t2)`,
		`SELECT max(a) FROM t1 UNION ALL SELECT g FROM t2`,
		`SELECT CASE WHEN a>0 THEN a ELSE b END FROM t1 UNION ALL SELECT g FROM t2`,
		`SELECT g||'q' FROM t2 UNION ALL SELECT a FROM t1`,
		// Multi-column, so a per-column fold cannot be papered over by a
		// whole-row one.
		`SELECT a,b,f FROM t1, t2 UNION ALL SELECT g,f,h FROM t2 UNION ALL SELECT 1,2,'q'`,
	} {
		stmts := append(append([]string{}, cviSetup...), `CREATE VIEW v AS `+body)
		stmts = append(stmts, cviProbe()...)
		// The CTAS of the same body, whose ONLY difference is the default
		// affinity SQLite passes (BLOB rather than NONE).
		stmts = append(stmts,
			`CREATE TABLE u AS `+body,
			`SELECT sql FROM sqlite_master WHERE name='u'`)
		differ(t, body, stmts)
	}
}

// TestCompoundViewInfoDeclined pins the ONE family this still declines rather
// than answers: a view whose LEFTMOST arm reads from a derived table, another
// view or a CTE. Resolving arm 0's column through it is what columnType would
// have to do, and columnTypeImpl's TK_COLUMN branch takes the RIGHTMOST arm of
// whatever subquery it lands in (pSubq->pSelect is the top of the pPrior
// chain) while sqlite3SubqueryColumnTypes itself walks to the LEFTMOST -- so a
// view's own reported type and the type an outer compound derives from it
// legitimately disagree, and getting that direction wrong is a wrong answer,
// not a gap. The oracle's answers, for whoever lifts this:
//
//	CREATE VIEW vtxt AS SELECT g AS y FROM t2 UNION ALL SELECT b FROM t1;
//	  vtxt.y                                          -> "VARCHAR(9)"
//	  CREATE VIEW v AS SELECT y FROM vtxt UNION ALL SELECT b FROM t1
//	  v.y                                             -> "TEXT"
//	CREATE VIEW v AS SELECT * FROM (SELECT a FROM t1 UNION ALL SELECT g FROM t2)
//	                 UNION ALL SELECT b FROM t1;  v.a -> "BLOB"
//
// The gate is on the SIDE this engine is on: the oracle must still ANSWER and
// this engine must still DECLINE. A merge that starts serving these will fail
// here rather than silently ship whichever direction it guessed.
func TestCompoundViewInfoDeclined(t *testing.T) {
	for _, c := range []struct{ setup, body string }{
		{"", `SELECT * FROM (SELECT a FROM t1 UNION ALL SELECT g FROM t2) UNION ALL SELECT b FROM t1`},
		{`CREATE VIEW vtxt AS SELECT g AS y FROM t2 UNION ALL SELECT b FROM t1`, `SELECT y FROM vtxt UNION ALL SELECT b FROM t1`},
		{`CREATE VIEW vtxt AS SELECT g AS y FROM t2 UNION ALL SELECT a FROM t1`, `SELECT y FROM vtxt UNION ALL SELECT a FROM t1`},
		{`CREATE VIEW vtxt AS SELECT a AS y FROM t1`, `SELECT y FROM vtxt UNION ALL SELECT g FROM t2`},
		{`CREATE VIEW vtxt AS SELECT a AS y FROM t1 UNION ALL SELECT 1`, `SELECT y FROM vtxt`},
		{`CREATE VIEW vtxt AS SELECT a AS y FROM t1`, `WITH cte(y) AS (SELECT y FROM vtxt) SELECT y FROM cte UNION ALL SELECT g FROM t2`},
	} {
		stmts := append([]string{}, cviSetup...)
		if c.setup != "" {
			stmts = append(stmts, c.setup)
		}
		stmts = append(stmts, `CREATE VIEW v AS `+c.body, `SELECT * FROM pragma_table_info('v')`)
		last := len(stmts) - 1
		cgo := run(t, "cgo", stmts)
		mus := run(t, "musql", stmts)
		if cgo[last]["kind"] != "rows" {
			t.Errorf("[%s] the oracle no longer answers PRAGMA table_info: %v", c.body, cgo[last])
		}
		if mus[last]["kind"] != "error" {
			t.Errorf("[%s] this engine now ANSWERS a shape it declined; move it into TestCompoundViewInfo and assert the type: %v",
				c.body, mus[last])
		}
	}
}

// cviArms are the per-arm expressions. Deliberately not a column x column
// matrix: that is exactly what produced the rule this replaced.
var cviArms = []string{
	`a`, `b`, `f`, `g`, `h`, `m`, `n`,
	`CAST(a AS TEXT)`, `CAST(a AS INT)`, `CAST(a AS REAL)`, `CAST(a AS NUMERIC)`,
	`CAST(a AS BLOB)`, `CAST(g AS wibble)`, `CAST(a AS INT) COLLATE binary`,
	`1`, `'x'`, `2.5`, `NULL`, `x'00'`,
	`a+1`, `-a`, `+a`, `+g`, `abs(a)`, `upper(b)`, `g||'q'`, `a COLLATE nocase`,
	`g COLLATE nocase`, `a=1`, `max(a)`, `count(*)`,
	`(SELECT g FROM t2)`, `(SELECT a FROM t1)`, `(SELECT n FROM t3)`,
	`CASE WHEN a>0 THEN a ELSE g END`, `CASE WHEN a>0 THEN 'q' END`,
}

var cviOps = []string{" UNION ALL ", " UNION ", " EXCEPT ", " INTERSECT "}

func TestCompoundViewInfoFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260807))
	for k := 0; k < 160; k++ {
		k := k
		name := fmt.Sprintf("c%03d", k)
		t.Run(name, func(t *testing.T) {
			var parts []string
			for a := 0; a < 2+rng.Intn(3); a++ {
				sel := "SELECT " + cviArms[rng.Intn(len(cviArms))]
				if a == 0 {
					sel += " AS z"
				}
				parts = append(parts, sel+" FROM t1, t2, t3")
			}
			body := strings.Join(parts, cviOps[rng.Intn(len(cviOps))])
			stmts := append(append([]string{}, cviSetup...), `CREATE VIEW v AS `+body)
			stmts = append(stmts, cviProbe()...)
			stmts = append(stmts,
				`CREATE TABLE u AS `+body,
				`SELECT sql FROM sqlite_master WHERE name='u'`,
			)
			differ(t, name+" "+body, stmts)
		})
	}
}
