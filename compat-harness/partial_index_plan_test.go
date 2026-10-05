// This file tests query planning with partial indexes, ensuring correct scan order
// and result ordering.
package compat

import (
	"encoding/json"
	"testing"
)

var partialGridSchema = []string{
	`CREATE TABLE t(a INTEGER PRIMARY KEY, x INT, y TEXT, z)`,
	`INSERT INTO t VALUES(1,9,'e','p'),(2,1,NULL,'q'),(3,7,'c','r'),(4,3,'a','s'),
	                     (5,5,'d','t'),(6,5,'b','u'),(7,2,NULL,'v'),(8,8,'a','w')`,
	`CREATE INDEX tx ON t(x) WHERE x>2`,
	`CREATE TABLE s(a INTEGER PRIMARY KEY, y TEXT, w INT)`,
	`INSERT INTO s VALUES(1,'m',1),(2,NULL,2),(3,'c',3),(4,'x',4),(5,'c',5),(6,NULL,6),(7,'a',7)`,
	`CREATE INDEX sy ON s(y) WHERE y IS NOT NULL`,
	`CREATE TABLE e(a INTEGER PRIMARY KEY, x INT, y TEXT, v BLOB)`,
	`INSERT INTO e VALUES(1,5,'q',1),(2,4,'b',2),(3,5,'k',3),(4,5,'a',4),(5,6,'z',5),(6,5,'k',6)`,
	`CREATE INDEX ey ON e(y) WHERE x=5`,
	`CREATE TABLE f(a INTEGER PRIMARY KEY, x INT, y TEXT)`,
	`INSERT INTO f VALUES(1,4,'d'),(2,9,'i'),(3,1,'a'),(4,6,'f'),(5,6,'F'),(6,3,'c')`,
	`CREATE INDEX fx ON f(x)`,
	`CREATE INDEX fxd ON f(x DESC, y) WHERE x>2 AND y IS NOT NULL`,
	`CREATE TABLE g(k INT, n TEXT)`,
	`INSERT INTO g VALUES(5,'five'),(9,'nine'),(3,'three'),(1,'one'),(7,'seven')`,
	`CREATE TABLE h(a INTEGER PRIMARY KEY, x INT, y TEXT)`,
	`INSERT INTO h VALUES(1,8,'b'),(2,1,'z'),(3,6,'a'),(4,9,'y'),(5,2,'c'),(6,7,'x')`,
	`CREATE INDEX hx ON h(x) WHERE x>5`,
	`CREATE INDEX hy ON h(y)`,
	// u: a partial UNIQUE index, a condition on the INTEGER PRIMARY KEY, a
	// COLLATE, a negative literal and a text literal in conditions.
	`CREATE TABLE u(a INTEGER PRIMARY KEY, w INT, y TEXT COLLATE NOCASE, c TEXT)`,
	`INSERT INTO u VALUES(1,30,'B','k'),(2,10,'a','m'),(3,20,'C','k'),(4,40,NULL,'z'),(5,50,'d','k'),(6,-5,'e','q')`,
	`CREATE UNIQUE INDEX uw ON u(w) WHERE y IS NOT NULL`,
	`CREATE INDEX ua ON u(c) WHERE a>2`,
	`CREATE INDEX uy ON u(y) WHERE y>'b'`,
	`CREATE INDEX un ON u(c, w) WHERE w>-1`,
	`CREATE INDEX uc ON u(w) WHERE c='k'`,
	`CREATE INDEX u1 ON u(c) WHERE 1`, // only a column-free term implies it: never usable
	// v: an IS NOT NULL partial index on an INT column, with a NOT NULL one
	// beside it -- whose "IS NOT NULL" resolve.c:1075 folds to 1 in a WHERE.
	`CREATE TABLE v(a INTEGER PRIMARY KEY, x INT, k INT NOT NULL)`,
	`INSERT INTO v VALUES(1,5,3),(2,NULL,1),(3,2,2),(4,9,5),(5,1,4)`,
	`CREATE INDEX vx ON v(x) WHERE x IS NOT NULL`,
	`CREATE INDEX vk ON v(k) WHERE k IS NOT NULL`,
	// nn: a condition only exprAnalyze's own TERM_VNULL "x>NULL" could match,
	// which whereUsablePartialIndex refuses to count.
	`CREATE TABLE nn(a INTEGER PRIMARY KEY, x INT, k INT)`,
	`INSERT INTO nn VALUES(1,5,3),(2,NULL,1),(3,2,2),(4,9,5),(5,1,4)`,
	`CREATE INDEX nnk ON nn(k) WHERE x>NULL`,
	// fl: a bare column as the condition.
	`CREATE TABLE fl(a INTEGER PRIMARY KEY, b INT, k INT)`,
	`INSERT INTO fl VALUES(1,1,3),(2,0,1),(3,2,2),(4,NULL,5),(5,1,4)`,
	`CREATE INDEX flk ON fl(k) WHERE b`,
	// dq: a partial UNIQUE index over a NOT NULL column, which
	// isDistinctRedundant would take for proof of DISTINCT if it asked.
	`CREATE TABLE dq(a INTEGER PRIMARY KEY, w INT NOT NULL, y TEXT, z INT)`,
	`INSERT INTO dq VALUES(1,40,'a',1),(2,10,'b',2),(3,30,NULL,3),(4,20,'c',4),(5,30,'d',5)`,
	`CREATE UNIQUE INDEX dqw ON dq(w) WHERE y IS NOT NULL`,
	`CREATE INDEX dqz ON dq(z DESC)`,
}

// partialGridWheres are the implying and non-implying conditions, per table.
// The comments name the sqlite3ExprImpliesExpr arm that decides each one.
var partialGridWheres = []struct{ tab, col, where string }{
	{"t", "y", `x>2`},               // sqlite3ExprCompare == 0
	{"t", "y", `2<x`},               // commuted in place by exprAnalyze: x>2
	{"t", "y", `x>2 AND z<>'q'`},    // one implying conjunct is enough
	{"t", "y", `z<>'q' AND x>2`},    // in either position
	{"t", "y", `x>3`},               // implies it in fact, not by the proof: skipped
	{"t", "y", `x>=3`},              // skipped
	{"t", "y", `x>+2`},              // TK_UPLUS: a different tree, skipped
	{"t", "y", `x>2.0`},             // TK_FLOAT vs EP_IntValue: skipped
	{"t", "y", `x>02`},              // EP_IntValue 2 either way: usable
	{"t", "y", `x>0x2`},             // and a hex spelling of it
	{"t", "y", `x BETWEEN 3 AND 8`}, // skipped: the virtual x>=3 is not x>2
	{"t", "y", `x IN (5,7)`},        // skipped
	{"t", "y", `x>2 OR x<0`},        // skipped: the OR term is not x>2
	{"t", "y", `z>'a'`},             // nothing to imply it from
	{"t", "z", `x>2 AND y IS NOT NULL`},
	{"t", "y", `t.x>2`},
	{"t", "y", `(x>2)`},
	{"t", "y", `x>2 COLLATE NOCASE`}, // a COLLATE under the comparison: skipped
	{"t", "y", `likely(x>2)`},        // whereClauseInsert skips the likely()
	{"t", "y", `iif(x>2, 1, 0)`},     // sqlite3ExprIsIIF: implies x>2
	{"t", "y", `iif(x>2, 1)`},
	{"t", "y", `CASE WHEN x>2 THEN 1 END`},
	{"t", "y", `CASE WHEN x>2 THEN 1 ELSE NULL END`},
	{"t", "y", `iif(x>2, 1, 5)`}, // not an IIF by its third argument
	{"t", "y", `x>2 AND x<9`},

	{"s", "w", `y IS NOT NULL`}, // compare == 0
	{"s", "w", `y NOTNULL`},
	{"s", "w", `y='c'`}, // exprImpliesNotNull: an operand of =
	{"s", "w", `'c'=y`},
	{"s", "w", `y>'b'`},
	{"s", "w", `y<>'q'`},
	{"s", "w", `y LIKE 'c%'`},    // only its LIKE-optimization virtual term
	{"s", "w", `y GLOB 'c*'`},    // likewise, BINARY
	{"s", "w", `y IN ('a','c')`}, // TK_IN: its left operand
	{"s", "w", `y IN (SELECT n FROM g)`},
	{"s", "w", `y NOT IN ('a','q')`}, // TK_NOT, then TK_IN
	{"s", "w", `y||'z'='cz'`},        // TK_CONCAT under =
	{"s", "w", `y IS 'c'`},           // TK_IS is not in exprImpliesNotNull: skipped
	{"s", "w", `(y IS 'zz') = 0`},    // nor under an operator that recurses
	{"s", "w", `length(y)>0`},        // TK_FUNCTION: skipped
	{"s", "w", `y BETWEEN 'a' AND 'n'`},
	{"s", "w", `NOT (y='zz')`}, // TK_NOT sets seenNot
	{"s", "w", `-y<0`},
	{"s", "w", `y IS TRUE`}, // TK_TRUTH, op2 TK_IS
	{"s", "w", `w>2`},
	{"s", "w", `y IS NOT NULL OR w>100`},

	{"e", "y", `x=5`}, // usable, and x is covered by the condition
	{"e", "y", `5=x`},
	{"e", "y", `x==5`},
	{"e", "y", `x IN (5)`}, // parse.y: x = +5, which is not x=5
	{"e", "y", `x=5 AND y>'b'`},
	{"e", "y", `x=4`},
	{"e", "v", `x=5`}, // v is not covered: a table lookup

	{"f", "y", `x>2 AND y IS NOT NULL`},
	{"f", "y", `y IS NOT NULL AND x>2`},
	{"f", "y", `x>2`},           // only half of the condition: skipped
	{"f", "y", `y IS NOT NULL`}, // the other half: skipped
	{"f", "y", `x>2 AND y>'a'`},

	{"h", "y", `x>5 OR y='a'`}, // WHERE_MULTI_OR: the first disjunct's sub-scan
	{"h", "y", `y='a' OR x>5`},
	{"h", "y", `x>5 OR x<2`},
	{"h", "a", `(x>5 AND y>'a') OR y='c'`},

	{"u", "c", `y IS NOT NULL`},
	{"u", "c", `w=30 AND y IS NOT NULL`}, // whereShortCut skips a partial UNIQUE index
	{"u", "y", `a>2`},
	{"u", "y", `rowid>2`}, // the rowid and its alias are one column
	{"u", "a", `y>'b'`},   // NOCASE: 'B' and 'b' are not in it
	{"u", "a", `y>'B'`},   // a different token: skipped
	{"u", "a", `w>-1`},    // TK_UMINUS on both sides
	{"u", "a", `w>-+1`},   // parse.y:1457 turns the TK_UPLUS into the TK_UMINUS: usable
	{"u", "a", `w>+-1`},   // but not the other way round
	{"u", "a", `w>-1 AND c>'a'`},
	{"u", "a", `c='k'`},
	{"u", "w", `c='k'`},
	{"u", "a", `c>'a' AND 1`},

	{"nn", "a", `x IS NOT NULL`},
	{"nn", "a", `x>NULL`},

	{"fl", "a", `b`},
	{"fl", "a", `b AND k>0`},
	{"fl", "a", `b=1`},
	{"fl", "a", `NOT b`},
	{"fl", "a", `iif(b, 1, 0)`},
	{"fl", "a", `b IS TRUE`},                     // TK_TRUTH is not the column: skipped
	{"fl", "a", `CASE WHEN b THEN 1 ELSE 0 END`}, // but this is an iif()
	{"fl", "a", `b IS NOT FALSE`},

	{"v", "a", `x IS NOT NULL`},
	{"v", "a", `x IS TRUE`},      // TK_TRUTH, op2 TK_IS: its operand
	{"v", "a", `x IS NOT FALSE`}, // op2 TK_ISNOT: not
	{"v", "a", `x IS FALSE`},
	{"v", "a", `x>1`},
	{"v", "a", `x IN (SELECT k FROM v)`},
	{"v", "a", `k>1`}, // "k IS NOT NULL" holds for every row
}

// partialGridQueries turns each condition into its three readouts, and adds the
// shapes that read the order some other way.
func partialGridQueries() []string {
	var qs []string
	for _, w := range partialGridWheres {
		qs = append(qs,
			`SELECT group_concat(`+w.col+`,',') FROM `+w.tab+` WHERE `+w.where,
			`SELECT `+w.col+` FROM `+w.tab+` WHERE `+w.where+` LIMIT 2`,
			`SELECT a, `+w.col+` FROM `+w.tab+` WHERE `+w.where,
		)
	}
	return append(qs,
		// ORDER BY whose ties the scan order breaks, and one it satisfies.
		`SELECT a FROM t WHERE x>2 ORDER BY x`,
		`SELECT a FROM t WHERE x>2 ORDER BY y`,
		`SELECT a FROM t WHERE x>2 ORDER BY x DESC LIMIT 3`,
		`SELECT a FROM s WHERE y IS NOT NULL ORDER BY y`,
		`SELECT a FROM e WHERE x=5 ORDER BY x`,
		`SELECT a FROM f WHERE x>2 AND y IS NOT NULL ORDER BY x`,
		`SELECT a FROM u WHERE y IS NOT NULL ORDER BY w`,
		// DISTINCT and GROUP BY over the indexed column.
		`SELECT DISTINCT x FROM t WHERE x>2`,
		`SELECT x, count(*) FROM t WHERE x>2 GROUP BY x`,
		`SELECT y, group_concat(a) FROM s WHERE y>'b' GROUP BY y`,
		`SELECT DISTINCT w FROM u WHERE y IS NOT NULL`,
		`SELECT DISTINCT w FROM dq WHERE y IS NOT NULL`,
		`SELECT DISTINCT w FROM dq WHERE y IS NOT NULL AND z>0`,
		`SELECT DISTINCT w, z FROM dq WHERE y>'a'`,
		`SELECT DISTINCT w FROM dq WHERE w>0 AND y IS NOT NULL`,
		`SELECT group_concat(a) FROM dq WHERE w=30 AND y IS NOT NULL`,
		`SELECT group_concat(a) FROM dq WHERE w=30`,
		`SELECT min(x), max(x) FROM t WHERE x>2`,
		// JOINs: the WHERE or an INNER ON may imply it; an OUTER join's own
		// ON may imply it for the table it NULL-extends, and nothing else may.
		`SELECT group_concat(t.y||g.n) FROM t, g WHERE t.x=g.k AND t.x>2`,
		`SELECT group_concat(t.y||g.n) FROM g JOIN t ON t.x=g.k AND t.x>2`,
		`SELECT group_concat(t.y||g.n) FROM g JOIN t ON t.x=g.k WHERE t.x>2`,
		`SELECT group_concat(g.n||coalesce(t.y,'-')) FROM g LEFT JOIN t ON t.x=g.k AND t.x>2`,
		`SELECT group_concat(g.n||coalesce(t.y,'-')) FROM g LEFT JOIN t ON t.x=g.k WHERE t.x>2`,
		`SELECT group_concat(coalesce(t.y,'-')||g.n) FROM t LEFT JOIN g ON g.k=t.x WHERE t.x>2`,
		`SELECT group_concat(g.n||coalesce(s.w,'-')) FROM g LEFT JOIN s ON s.w=g.k AND s.y IS NOT NULL`,
		`SELECT group_concat(g.n||coalesce(t.y,'-')) FROM g LEFT JOIN t ON t.x=g.k WHERE iif(t.x>2, 1, 0)`,
		`SELECT group_concat(g.n||coalesce(t.y,'-')) FROM g LEFT JOIN t ON t.z<>g.n WHERE iif(t.x>2, 1, 0)`,
		`SELECT group_concat(g.n||coalesce(t.y,'-')) FROM g LEFT JOIN t ON t.z<>g.n WHERE coalesce(t.x>2, 1)`,
		`SELECT group_concat(t.y||coalesce(s.w,'-')) FROM g JOIN t ON t.x=g.k LEFT JOIN s ON s.w=g.k AND t.x>2`,
		`SELECT group_concat(t.y||coalesce(s.w,'-')) FROM g JOIN t ON t.x+0=g.k LEFT JOIN s ON s.w=g.k AND t.x>2`,
		`SELECT t.y, g.n FROM t, g WHERE t.x>2 AND g.k>0 LIMIT 3`,
		`SELECT group_concat(a.y||b.y) FROM t a, t b WHERE a.x>2 AND b.a=a.a`,
		`SELECT group_concat(a.y||b.y) FROM t a, t b WHERE b.x>2 AND b.a=a.a`,
		`SELECT group_concat(t.y) FROM t WHERE EXISTS (SELECT 1 FROM g WHERE g.k=t.x) AND x>2`,
		// A subquery's own WHERE, correlated or not.
		`SELECT (SELECT group_concat(y) FROM t WHERE x>2)`,
		`SELECT n, (SELECT group_concat(y) FROM t WHERE x>2 AND x<=g.k) FROM g`,
		`SELECT n FROM g WHERE k IN (SELECT x FROM t WHERE x>2) LIMIT 2`,
		`SELECT group_concat(y) FROM (SELECT y FROM t WHERE x>2)`,
		`SELECT y FROM (SELECT y, x FROM t WHERE z<>'q' LIMIT 100) WHERE x>2`,
		`SELECT group_concat(y) FROM t WHERE x>2 AND a IN (SELECT a FROM t WHERE x>2)`,
		// HAVING, and a WHERE sqlite3ExprAnd does not fold.
		`SELECT x, group_concat(y) FROM t GROUP BY x HAVING x>2`,
		`SELECT group_concat(y) FROM t WHERE x>2 AND 1`,
		`SELECT group_concat(y) FROM t WHERE (x>2 AND 1) AND z<>'q'`,
		`SELECT group_concat(y) FROM t WHERE x>2 AND 0`,
		`SELECT group_concat(y) FROM t WHERE x IS NOT NULL AND x>2`,
		`SELECT count(*), count(y) FROM t WHERE x>2`,
		`WITH c AS (SELECT y FROM t WHERE x>2) SELECT group_concat(y) FROM c`,
		// INDEXED BY names the partial index outright; NOT INDEXED hides it.
		`SELECT group_concat(y) FROM t INDEXED BY tx WHERE x>2`,
		`SELECT group_concat(y) FROM t INDEXED BY tx WHERE x>3`,
		`SELECT group_concat(y) FROM t NOT INDEXED WHERE x>2`,
	)
}

// partialGridNeverWrong are shapes this port leaves DECLINED, each for a reason
// of its own; the assertion is only that the answer, when there is one, is C's.
//
//   - resolve.c:1075's NOT NULL strength reduction folds "k IS NOT NULL" on a
//     NOT NULL column to the integer 1 in a SELECT's WHERE -- but only when
//     every enclosing name context is a WHERE (NC_Where) and the column is not
//     EP_CanBeNull, which pcxConv does not track, so the term is unknown.
//   - a LEFT JOIN whose WHERE ORs over the NULL-extended table declines with or
//     without a partial index (an ordinary index on t.x does the same).
var partialGridNeverWrong = []string{

	`SELECT group_concat(a) FROM v WHERE k IS NOT NULL`,
	`SELECT group_concat(a) FROM v WHERE k IS NULL OR x>1`,
	`SELECT group_concat(g.n||coalesce(t.y,'-')) FROM g LEFT JOIN t ON t.x=g.k WHERE t.x>2 OR t.x IS NULL`,
}

// partialGridDML are statements whose result is the scan order itself: the
// rows RETURNING reports, in the order the one-pass loop visits them.
var partialGridDML = [][]string{
	{`DELETE FROM t WHERE x>2 RETURNING a, y`, `SELECT a FROM t`},
	{`UPDATE t SET z=z||'!' WHERE x>2 RETURNING a`, `SELECT group_concat(z) FROM t`},
	{`UPDATE s SET w=w+100 WHERE y>'b' RETURNING a, w`},
	{`INSERT INTO g SELECT x, y FROM t WHERE x>2`, `SELECT rowid, k, n FROM g`},
}

// TestPartialIndexPlan compares every readout, with and without sqlite_stat1,
// against the oracle. The SELECTs share one connection per mode; each DML
// shape gets its own.
func TestPartialIndexPlan(t *testing.T) {
	qs := partialGridQueries()
	for _, analyze := range []bool{false, true} {
		setup := append([]string{}, partialGridSchema...)
		name := "partial index"
		if analyze {
			setup = append(setup, `ANALYZE`)
			name += " analyzed"
		}
		stmts := append(append(append([]string{}, setup...), qs...), partialGridNeverWrong...)
		oracle := run(t, "cgo", stmts)
		got := run(t, "musql", stmts)
		for i := range stmts {
			ob, _ := json.Marshal(oracle[i])
			gb, _ := json.Marshal(got[i])
			declinable := i >= len(setup)+len(qs) && got[i]["kind"] == "error"
			if string(ob) != string(gb) && !declinable {
				t.Errorf("[%s] musql DIVERGES from C SQLite\n  sql:    %s\n  cgo:    %s\n  musql: %s",
					name, stmts[i], ob, gb)
			}
		}
		for _, dml := range partialGridDML {
			differ(t, name+" dml", append(append([]string{}, setup...), dml...))
		}
	}
}
