package compat

// This file contains fast regression pins for DISTINCT aggregate ordering.

import "testing"

// r37dTieTable holds INTEGER 1 and REAL 1.0 -- compare-equal, so the ephemeral
// index a DISTINCT argument is deduped through (codeDistinct, select.c:933)
// calls them duplicates, and WHICH SPELLING SURVIVES is decided by arrival
// order. b holds 'x' twice and 'Y'/'X', which a NOCASE index makes equal too.
var r37dTieTable = []string{
	"CREATE TABLE t(a,b,c)",
	"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
}

func r37dCase(name string, schema []string, sql string, mustAnswer bool) aggOrderCase {
	stmts := append(append([]string(nil), r37dTieTable...), schema...)
	return aggOrderCase{name, append(stmts, sql), mustAnswer}
}

func TestR37DDistinctAggOrderPins(t *testing.T) {
	descIdx := []string{"CREATE INDEX i1 ON t(a DESC)"}
	aIdx := []string{"CREATE INDEX i1 ON t(a)"}
	baIdx := []string{"CREATE INDEX i1 ON t(b,a)"}

	for _, c := range []aggOrderCase{
		// MUST KEEP ANSWERING. count's cardinality does not depend on which
		// duplicate arrived first (SQLITE_FUNC_ANYORDER, func.c:3368), and with
		// no index at all there is nothing for either engine to walk.
		r37dCase("count-distinct-indexed", aIdx, "SELECT count(DISTINCT a) FROM t", true),
		r37dCase("count-distinct-desc-idx", descIdx, "SELECT count(DISTINCT a) FROM t", true),
		r37dCase("concat-distinct-noidx", nil, "SELECT group_concat(DISTINCT b) FROM t", true),
		r37dCase("sum-distinct-noidx", nil, "SELECT sum(DISTINCT a) FROM t", true),
		r37dCase("min-distinct-noidx", nil, "SELECT min(DISTINCT a), typeof(min(DISTINCT a)) FROM t", true),
		// The two CONTROLS. Neither carries DISTINCT, so the ported planner
		// decides the scan and the index order reaches the accumulator here
		// exactly as it does there. Both measured 0 wrong over all nine index
		// shapes of the r37d battery, and they are pinned so the arming above
		// cannot start declining them.
		r37dCase("concat-plain-indexed", baIdx, "SELECT group_concat(b) FROM t", true),
		r37dCase("concat-sep-indexed", baIdx, "SELECT group_concat(b,'-') FROM t", true),

		// AGREE OR DECLINE. Each was a WRONG ANSWER before r37d.
		//
		// The index on a DESC, walked backwards to satisfy "ORDER BY a", visits
		// REAL 1.0 before INTEGER 1, so C's dedup keeps the REAL: 1.0 there,
		// INTEGER 1 here.
		r37dCase("min-distinct-tie-desc", descIdx, "SELECT min(DISTINCT a) FROM t", false),
		r37dCase("max-distinct-tie-desc", descIdx, "SELECT max(DISTINCT a) FROM t", false),
		r37dCase("sum-distinct-group-desc", descIdx, "SELECT sum(DISTINCT a) FROM t GROUP BY a ORDER BY a", false),
		// A covering index on (b,a) delivers b in BINARY order, so the
		// concatenation is 'X,Y,w,x,z' there and was 'x,Y,z,w,X' here.
		r37dCase("concat-distinct-covering", baIdx, "SELECT group_concat(DISTINCT b) FROM t", false),
		r37dCase("json-distinct-covering", baIdx, "SELECT json_group_array(DISTINCT b) FROM t", false),
		// The EMISSION-ORDER half: same rows, opposite order. C takes select.c's
		// groupBySort==0 arm because the DESC index already delivers the
		// grouping, and emits 3,2,1,NULL where this engine emitted NULL,1,2,3.
		r37dCase("count-distinct-group-desc", descIdx, "SELECT count(DISTINCT a) FROM t GROUP BY a", false),
		r37dCase("count-distinct-b-group-desc", descIdx, "SELECT count(DISTINCT b) FROM t GROUP BY a", false),
		// ...and the shape the round-36 shape space actually flagged, whose two
		// items are a DISTINCT argument and group_concat's separator form. The
		// battery says the separator form is innocent: it is the DISTINCT
		// argument that makes the whole statement's plan undecided.
		r37dCase("concat-agg-r36-cell", baIdx, "SELECT group_concat(b,'-'), count(DISTINCT a) FROM t", false),
	} {
		aggOrderCheck(t, c)
	}
}
