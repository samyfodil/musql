package compat

// This file tests "WHERE <col> IN (<values>)" over indexed columns.

import (
	"fmt"
	"strings"
	"testing"
)

// r36cInFixture creates test data with different orderings by rowid, column, and index.
var r36cInFixture = []string{
	"CREATE TABLE t(a, b, c)",
	"INSERT INTO t VALUES(3,'x',10)",
	"INSERT INTO t VALUES(1,'Y',20)",
	"INSERT INTO t VALUES(2,'x',30)",
	"INSERT INTO t VALUES(2,'z',40)",
	"INSERT INTO t VALUES(NULL,'w',50)",
	"INSERT INTO t VALUES(1.0,'X',60)",
	"INSERT INTO t VALUES(4,'y',70)",
}

var r36cSchemas = []struct{ name, ddl string }{
	{"noidx", ""},
	{"idx-a", "CREATE INDEX i1 ON t(a)"},
	{"idx-a-desc", "CREATE INDEX i1 ON t(a DESC)"},
	{"idx-ab", "CREATE INDEX i1 ON t(a,b)"},
	{"idx-ab-desc", "CREATE INDEX i1 ON t(a,b DESC)"},
	{"idx-ba", "CREATE INDEX i1 ON t(b,a)"},
	{"idx-cover", "CREATE INDEX i1 ON t(a,b,c)"},
	{"idx-a-nocase", "CREATE INDEX i1 ON t(a COLLATE NOCASE)"},
	{"idx-b-nocase", "CREATE INDEX i1 ON t(b COLLATE NOCASE)"},
	{"uniq-c", "CREATE UNIQUE INDEX i1 ON t(c)"},
	{"two-idx", "CREATE INDEX i1 ON t(a); CREATE INDEX i2 ON t(b)"},
	{"idx-c-a", "CREATE INDEX i1 ON t(c,a)"},
}

// r36cInWheres are the IN spellings. The list ORDER, the duplicate value, the
// NULL element and the mixed storage classes are each their own question about
// the ephemeral RHS set.
var r36cInWheres = []string{
	"WHERE a IN (1,2,3)",
	"WHERE a IN (3,2,1)",
	"WHERE a IN (2)",
	"WHERE a IN (1,1,3)",
	"WHERE a IN (NULL,2,3)",
	"WHERE a IN (1.0,2)",
	"WHERE a IN ('2',3)",
	"WHERE a NOT IN (1,2)",
	"WHERE b IN ('x','z')",
	"WHERE b IN ('X','x')",
	"WHERE c IN (10,30,60)",
	"WHERE a IN (1,2,3) AND b > 'W'",
	"WHERE a IN (1,2,3) AND c = 20",
	"WHERE a IN (1,2,3) AND b IN ('x','X')",
	"WHERE a IN ()",
}

// KNOWN OPEN, deliberately NOT in the battery above -- both are still served in
// this engine's rowid order where the oracle walks an index, and both are a
// DIFFERENT rule from the WO_IN arm this file gates:
//
//	WHERE a IN (1,2) OR a IN (3,4)
//	    exprAnalyzeOrTerm's case 3: the oracle builds a MULTI-INDEX OR loop
//	    (verified by EXPLAIN QUERY PLAN) whose rows arrive subterm by subterm
//	    through a RowSet, which is not a single index order and so is not
//	    expressible as an autoIndexKey at all. whereLoopAddOr is not ported.
//	WHERE a IN (SELECT a FROM t WHERE a > 1)
//	    sqlite3FindInIndex's IN_INDEX_EPH/IN_INDEX_INDEX_ASC choice and the
//	    subquery's own prerequisites; wherePlanTermsFrom leaves a subquery IN
//	    operator-less, so the whole statement declines to rowid order.

// r36cReads/r36cTails vary the READOUT and the tail, because the same plan read
// through a different select list is a different observation (round 31) and a
// tail that totally orders the rows hides the scan order entirely.
var r36cReads = []string{
	"a, b",
	"*",
	"a, typeof(a)",
	"quote(a), quote(b)",
	"rowid, a",
	"group_concat(b)",
	"count(*), min(a), max(a)",
	"a, count(*)",
	"a, b, max(a)",
}

var r36cTails = []string{
	"",
	"LIMIT 2",
	"ORDER BY a",
	"ORDER BY a DESC",
	"ORDER BY b",
	"GROUP BY a",
	"GROUP BY b",
	"ORDER BY a LIMIT 2 OFFSET 1",
}

// r36cCase runs one (schema, where, read, tail) cell against the oracle.
//
// ddl is SPLIT on ';': the worker runs one statement per list entry, and a
// two-index schema handed over whole was executed differently by the two
// drivers -- which reported all 16 of the two-idx cells as divergences with no
// planner involved at all. A gate that manufactures its own divergence is worse
// than no gate.
func r36cCase(t *testing.T, name, ddl, wh, rd, tl string) {
	t.Helper()
	stmts := append([]string(nil), r36cInFixture...)
	for _, d := range strings.Split(ddl, ";") {
		if d = strings.TrimSpace(d); d != "" {
			stmts = append(stmts, d)
		}
	}
	q := "SELECT " + rd + " FROM t " + wh
	if tl != "" {
		q += " " + tl
	}
	differ(t, name, append(stmts, q))
}

// TestR36CInIndexOrder walks schema x IN-spelling with the readout and tail
// ROTATED rather than squared: each cell of the full 12x17x9x8 product costs two
// subprocess runs (~0.24s), so the square is a 20-minute gate. Rotating still
// pairs every schema and every IN spelling with a different readout and tail,
// which is what the shape-space report says the interactions are 2-way in.
func TestR36CInIndexOrder(t *testing.T) {
	n := 0
	for si, sc := range r36cSchemas {
		for wi, wh := range r36cInWheres {
			rd := r36cReads[(si+wi)%len(r36cReads)]
			tl := r36cTails[(si+2*wi)%len(r36cTails)]
			n++
			r36cCase(t, fmt.Sprintf("r36c-in/%s/%d", sc.name, n), sc.ddl, wh, rd, tl)
		}
	}
	// The full readout x tail product, over the schemas the report named (a
	// composite index and its reversal) plus the DESC one -- a DESC index is
	// where the IN column's "is it constant across the loop?" question becomes
	// observable, because a wrong answer there re-sorts the descending scan
	// order into ascending (autoIndexKey.nEq / whereIdxNSkippedEq).
	for _, sc := range []string{
		"CREATE INDEX i1 ON t(a,b)",
		"CREATE INDEX i1 ON t(a DESC)",
	} {
		for _, wh := range []string{"WHERE a IN (1,2,3)", "WHERE a IN (3,2,1)"} {
			for _, rd := range r36cReads {
				for _, tl := range r36cTails {
					n++
					r36cCase(t, fmt.Sprintf("r36c-in-sq/%d", n), sc, wh, rd, tl)
				}
			}
		}
	}
	t.Logf("r36c IN battery: %d cases", n)
}

// TestR36CInJoinOrder puts the IN term inside a multi-table FROM clause, where
// the planner's answer decides the LOOP ORDER as well as the index -- a
// different code path (wherePlanMultiTableOrder) from the single-table one, and
// the one that used to decline the whole statement on sight of an IN.
func TestR36CInJoinOrder(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b)",
		"CREATE TABLE u(x, y)",
		"INSERT INTO t VALUES(3,'p'),(1,'q'),(2,'r'),(2,'s'),(NULL,'t')",
		"INSERT INTO u VALUES(2,'A'),(1,'B'),(3,'C'),(2,'D')",
	}
	schemas := []string{
		"",
		"CREATE INDEX ti ON t(a)",
		"CREATE INDEX ui ON u(x)",
		"CREATE INDEX ti ON t(a,b); CREATE INDEX ui ON u(x,y)",
		"CREATE INDEX ti ON t(b,a)",
	}
	queries := []string{
		"SELECT t.a, t.b, u.x, u.y FROM t, u WHERE t.a = u.x AND t.a IN (1,2)",
		"SELECT t.a, u.y FROM t JOIN u ON t.a = u.x WHERE u.x IN (2,3)",
		"SELECT t.b, u.y FROM t LEFT JOIN u ON t.a = u.x WHERE t.a IN (1,2,3)",
		"SELECT group_concat(t.b), group_concat(u.y) FROM t, u WHERE t.a IN (1,2) AND u.x IN (1,2)",
		"SELECT t.a, count(*) FROM t, u WHERE t.a = u.x AND u.x IN (1,2,3) GROUP BY t.a",
		"SELECT t.a, u.y FROM t, u WHERE t.a IN (1,2,3) AND u.x = t.a LIMIT 3",
	}
	n := 0
	for si, sc := range schemas {
		for qi, q := range queries {
			stmts := append([]string(nil), base...)
			for _, d := range strings.Split(sc, ";") {
				if d = strings.TrimSpace(d); d != "" {
					stmts = append(stmts, d)
				}
			}
			stmts = append(stmts, q)
			n++
			differ(t, fmt.Sprintf("r36c-injoin/%d/%d", si, qi), stmts)
		}
	}
	t.Logf("r36c IN join battery: %d cases", n)
}
