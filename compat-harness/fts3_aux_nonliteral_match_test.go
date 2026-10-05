// This file gates FTS3 auxiliary functions with non-literal MATCH queries.
//     "matchinfo(t)"/"offsets(t)"/"snippet(t,...)"'s first argument names the
//     TABLE itself (its hidden table-named column) the same way "t MATCH q"
//     does -- collectTableRefs already has a considerMatchTable special case
//     for MATCH's own operand (with a doc comment describing this exact
//     failure shape), but it was never extended to the three auxiliary
//     functions. An unrecognized dependency reports an EMPTY table-reference
//     set, which the pushdown planner then buckets at the OUTERMOST loop level
//     -- read before the fts cursor is positioned at all, not merely before
//     the WRONG row. Reproduced against baseline (pre-fix) musql with a
//     LITERAL MATCH and no correlated pattern at all: "CREATE VIRTUAL TABLE
//     t10 USING fts4(idx, value); ... SELECT docId, t10.* FROM t10, x WHERE
//     t10 MATCH 'one' AND matchinfo(t10) not null" already read t10's row
//     before OpMatch positioned it, erroring "unable to use function
//     matchinfo in the requested context" -- this predates and is independent
//     of the non-literal-query fix above; TestFts3AuxAsWhereClauseLoopLevel
//     pins it on its own, literal-only.
package compat

import "testing"

// TestFts3AuxMatchinfoNonLiteralQuery is fts3matchinfo.test's own "10.1"
// mined verbatim (see this file's package doc comment for the byte-identical
// upstream source). Oracle answer: {1 1 one 2 2 two 3 3 three}.
func TestFts3AuxMatchinfoNonLiteralQuery(t *testing.T) {
	differ(t, "fts3matchinfo.test 10.1: matchinfo() over a correlated (non-literal) MATCH", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(idx, value)`,
		`INSERT INTO t10 values (1, 'one'),(2, 'two'),(3, 'three')`,
		`SELECT docId, t10.*
    FROM t10
    JOIN (SELECT 1 AS idx UNION SELECT 2 UNION SELECT 3) AS x
   WHERE t10 MATCH x.idx
     AND matchinfo(t10) not null
   GROUP BY docId
   ORDER BY 1`,
	})
}

// TestFts3AuxOffsetsSnippetNonLiteralQuery broadens the SAME machinery
// (fts3AuxCompileInfo.patReg, the per-row query register) to offsets() and
// snippet(), which compileFts3Aux's non-literal branch serves identically to
// matchinfo() -- neither is exercised by the mined statement above, which
// only calls matchinfo().
func TestFts3AuxOffsetsSnippetNonLiteralQuery(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(idx, value)`,
		`INSERT INTO t10 values (1, 'one'),(2, 'two'),(3, 'three')`,
	}
	cases := []struct {
		name string
		stmt string
	}{
		{"offsets() over a correlated MATCH (select list)", `SELECT docId, offsets(t10)
    FROM t10 JOIN (SELECT 1 AS idx UNION SELECT 2 UNION SELECT 3) AS x
   WHERE t10 MATCH x.idx ORDER BY 1`},
		{"snippet() over a correlated MATCH (select list)", `SELECT docId, snippet(t10)
    FROM t10 JOIN (SELECT 1 AS idx UNION SELECT 2 UNION SELECT 3) AS x
   WHERE t10 MATCH x.idx ORDER BY 1`},
		{"offsets() over a correlated MATCH, as a WHERE residual", `SELECT docId
    FROM t10 JOIN (SELECT 1 AS idx UNION SELECT 2 UNION SELECT 3) AS x
   WHERE t10 MATCH x.idx AND offsets(t10) <> '' ORDER BY 1`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, append(append([]string{}, setup...), c.stmt))
		})
	}
}

// TestFts3AuxAsWhereClauseLoopLevel pins the join.go collectTableRefs fix on
// its own: a LITERAL MATCH, so compileFts3Aux's ALREADY-existing literal
// branch handles the query resolution -- only the WHERE-clause loop-level
// bucketing bug this file's package doc comment describes is exercised here.
func TestFts3AuxAsWhereClauseLoopLevel(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(idx, value)`,
		`INSERT INTO t10 values (1, 'one'),(2, 'two'),(3, 'three')`,
		`CREATE TABLE side(k)`,
		`INSERT INTO side VALUES(1),(2)`,
	}
	cases := []struct {
		name string
		stmt string
	}{
		{"matchinfo() WHERE residual, fts table FIRST in FROM", `SELECT docId, k FROM t10, side WHERE t10 MATCH 'one' AND matchinfo(t10) not null ORDER BY 1,2`},
		{"matchinfo() WHERE residual, fts table SECOND in FROM", `SELECT docId, k FROM side, t10 WHERE t10 MATCH 'two' AND matchinfo(t10) not null ORDER BY 1,2`},
		{"offsets() WHERE residual", `SELECT docId, k FROM t10, side WHERE t10 MATCH 'three' AND offsets(t10) <> '' ORDER BY 1,2`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, append(append([]string{}, setup...), c.stmt))
		})
	}
}
