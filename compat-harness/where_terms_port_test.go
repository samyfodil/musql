package compat

import (
	"fmt"
	"testing"
)

// WHERE TERMS THE PORTED PLANNER USED TO REFUSE. A refused term made it decline
// the whole statement, the scan fell back to rowid order, and every
// order-sensitive output was wrong wherever C walks an index: over t(a,b,c,d)
// indexed on c, "SELECT group_concat(a) FROM t WHERE c > 3 AND unlikely(d = 1)"
// is 25,22,... in C and 1,4,... there. Each case below is one whose scan order
// the term itself can decide, run against the oracle.
func TestWhereTermsPortedMatchC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX td ON t(d)", "CREATE INDEX tbd ON t(b, d)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t SELECT n, n % 5, 41 - n, n % 7 FROM s",
	}
	// likelihood()/likely()/unlikely(): whereClauseInsert's truthProb, which
	// whereLoopOutputAdjust and whereRangeAdjust then price.
	likelihood := []string{
		"c > 3 AND unlikely(d = 1)",
		"c > 3 AND likely(d = 1)",
		"likelihood(c > 3, 0.001) AND d = 1",
		"likelihood(c > 3, 0.999) AND d = 1",
		"likelihood(c > 3, 0.5) AND likelihood(c < 30, 0.5) AND d > 1",
		"c BETWEEN 3 AND 30 AND unlikely(d > 2)",
		"likely(c BETWEEN 3 AND 30) AND d = 2",
		"likely(c > 3 AND d = 2)",
		"unlikely(c > 3) AND unlikely(d > 2)",
		"likelihood(d = 1, 0.0) AND c > 10",
		"likelihood(d = 1, 1.0) AND c > 10",
		"c > 3 AND unlikely(b = 1) AND d > 0",
	}
	for i, w := range likelihood {
		for _, out := range []string{"SELECT group_concat(a) FROM t WHERE %s", "SELECT a FROM t WHERE %s LIMIT 3"} {
			differ(t, fmt.Sprintf("likelihood %d/%s", i, out[:12]), append(append([]string{}, base...), fmt.Sprintf(out, w)))
		}
	}

	// LIKE and GLOB: the pattern-length heuristic (where.c:3103) and the LIKE
	// optimization's prefix range (whereexpr.c:1376), which needs a TEXT column
	// under the right collation -- NOCASE for a case-insensitive LIKE, BINARY for
	// GLOB or under case_sensitive_like.
	lk := []string{
		"CREATE TABLE w(a INTEGER, s TEXT, n TEXT COLLATE NOCASE, u, k)",
		"CREATE INDEX ws ON w(s)", "CREATE INDEX wn ON w(n)", "CREATE INDEX wu ON w(u)", "CREATE INDEX wk ON w(k)",
		"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<60) INSERT INTO w SELECT i, " +
			"char(65 + (i % 7)) || char(97 + (i % 5)) || i, char(97 + (i % 6)) || char(65 + (i % 4)) || i, " +
			"(i % 9) || 'x' || i, 61 - i FROM r",
	}
	for i, w := range []string{
		"s LIKE 'Ba%'", "s LIKE 'ba%'", "n LIKE 'bA%'", "n LIKE 'ba%'", "s GLOB 'Ba*'", "n GLOB 'bA*'",
		"s LIKE 'B_a%'", "s LIKE 'Ba%' AND k > 10", "k > 10 AND s LIKE 'Babcdefghij%'", "k > 50 AND s LIKE 'B%'",
		"u LIKE '3x%'", "u LIKE '3%'", "u GLOB '3x*'", "n LIKE 'b@%'", "n LIKE 'bA_' ", "s LIKE 'Ba%' ESCAPE '!'",
		"s LIKE 'B!%%' ESCAPE '!'", "s NOT LIKE 'Ba%' AND k > 40", "like('Ba%', s)", "glob('Ba*', s) AND k < 30",
		"s LIKE 'Ba%' OR k = 3", "(s LIKE 'Ba%' OR s LIKE 'Ca%') AND k > 2",
	} {
		for _, pre := range [][]string{nil, {"PRAGMA case_sensitive_like = ON"}} {
			for _, out := range []string{"SELECT group_concat(a) FROM w WHERE %s", "SELECT a FROM w WHERE %s LIMIT 3"} {
				differ(t, fmt.Sprintf("like %d csl=%v/%s", i, pre != nil, out[:12]),
					append(append(append([]string{}, lk...), pre...), fmt.Sprintf(out, w)))
			}
		}
	}

	// The cost details that decide only between near-equal plans -- a JOIN
	// ORDER or an ORDER BY walk against a sort -- so each needs a tie to show:
	// the LIKE pattern heuristic on a term no loop uses (where.c:3103), a
	// LIKE whose prefix range is not its whole meaning and so is no parent of
	// the pair -- a pattern that goes on past its % or whose NOCASE prefix
	// ends in '@' (whereexpr.c:1420) -- and a likelihood() the index alone can
	// evaluate before its table lookups (where.c:4257-4276).
	pair := []string{
		"CREATE TABLE w(a INTEGER, s TEXT, n TEXT COLLATE NOCASE, k)", "CREATE INDEX ws ON w(s)", "CREATE INDEX wn ON w(n)", "CREATE INDEX wk ON w(k)",
		"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<60) INSERT INTO w SELECT i, " +
			"char(65 + (i % 7)) || char(97 + (i % 5)) || i, char(97 + (i % 6)) || char(65 + (i % 4)) || i, i % 13 FROM r",
		"INSERT INTO w VALUES(201, 'Bq1', 'b@1', 3), (202, 'Bq2', 'B@2', 4)",
		"CREATE TABLE v(a INTEGER, s TEXT, n TEXT COLLATE NOCASE, k)", "CREATE INDEX vs ON v(s)", "CREATE INDEX vn ON v(n)", "CREATE INDEX vk ON v(k)",
		"INSERT INTO v SELECT a + 100, s, n, k FROM w",
		"CREATE TABLE t2(a, b, e, f)", "CREATE INDEX t2be ON t2(b, e)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<50) INSERT INTO t2 SELECT n, n % 4, 50 - n, n % 3 FROM s",
	}
	for i, q := range []string{
		"SELECT group_concat(w.a || '-' || v.a) FROM w, v WHERE w.k = v.k AND v.s LIKE '%bc%' AND w.s LIKE '%a%'",
		"SELECT group_concat(w.a || '-' || v.a) FROM v, w WHERE w.k = v.k AND v.n LIKE 'B%' AND w.n LIKE 'B_%'",
		"SELECT group_concat(w.a || '-' || v.a) FROM w, v WHERE w.k = v.k AND v.n LIKE 'B_%' AND w.n LIKE 'B%'",
		"SELECT group_concat(w.a || '-' || v.a) FROM v, w WHERE w.k = v.k AND v.n LIKE 'b@%' AND w.n LIKE 'B%'",
		"SELECT group_concat(w.a || '-' || v.a) FROM w, v WHERE w.k = v.k AND v.n LIKE 'B%' AND w.n LIKE 'b@%'",
		"SELECT a FROM t2 WHERE likelihood(e > 20, 0.001) ORDER BY b LIMIT 8",
		"SELECT group_concat(a) FROM (SELECT a FROM t2 WHERE likelihood(e > 20, 0.01) ORDER BY b)",
	} {
		differ(t, fmt.Sprintf("tie %d", i), append(append([]string{}, pair...), q))
	}

	// "x IN (SELECT ...)": nIn 46 (where.c:3337), whatever b-tree
	// sqlite3FindInIndex hands the IN loop -- a DESC one included, which
	// codeINTerm walks backwards (wherecode.c:720) -- a compound RHS, whose
	// affinity is its RIGHTMOST arm's, and an OR disjunct holding one.
	rhs := [][]string{
		{"CREATE TABLE u(y)"},
		{"CREATE TABLE u(y UNIQUE)"},
		{"CREATE TABLE u(y)", "CREATE INDEX uy ON u(y DESC)"},
		{"CREATE TABLE u(y)", "CREATE UNIQUE INDEX uy ON u(y DESC)"},
		{"CREATE TABLE u(y INTEGER PRIMARY KEY)"},
		{"CREATE TABLE u(y TEXT COLLATE NOCASE UNIQUE)"},
	}
	for r, schema := range rhs {
		seed := append(append(append([]string{}, base...), schema...), "INSERT INTO u VALUES(3), (1), (4), (0)")
		for i, w := range []string{
			"d IN (SELECT y FROM u)", "d IN (SELECT y FROM u) AND c > 3", "c > 30 AND d IN (SELECT y FROM u)",
			"d IN (SELECT y FROM u WHERE y > 0)", "d IN (SELECT DISTINCT y FROM u)", "d IN (SELECT y + 0 FROM u)",
			"d IN (SELECT y FROM u UNION SELECT 6)", "d IN (SELECT y COLLATE binary FROM u)",
			"d IN (SELECT y FROM u) OR c = 5", "d NOT IN (SELECT y FROM u) AND c > 30",
			"d IN (SELECT y FROM u) AND b IN (SELECT y FROM u)", "d IN (SELECT y FROM u WHERE y <> t.b)",
			"b IN (SELECT y FROM u) AND d > 2", "rowid IN (SELECT y FROM u)",
			// Against a K-value list on another index: 46 prices the subquery
			// as 25 values, so the list's index wins for K < 25.
			"d IN (SELECT y FROM u) AND c IN (1,2,3,4,5,6,7,8,9,10,11,12,13,14,15)",
			"d IN (SELECT y FROM u) AND c IN (1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33)",
		} {
			for _, out := range []string{"SELECT group_concat(a) FROM t WHERE %s", "SELECT a FROM t WHERE %s LIMIT 3",
				"SELECT group_concat(t.a || ':' || v.a) FROM t, t AS v WHERE v.c = t.a AND %s"} {
				differ(t, fmt.Sprintf("in-select %d/%d/%s", r, i, out[:12]), append(append([]string{}, seed...), fmt.Sprintf(out, w)))
			}
		}
	}
}

// TestInSelectRhsCollateStaysDeclined: with an explicit COLLATE on the RHS
// column and none on x, C's IN LOOP dedups the RHS under the RHS's collation
// (expr.c:3752) and seeks x's index under x's own, so walking the index answers
// a different ROW SET than the same query NOT INDEXED -- 3,4 against 1,2,3,4
// below. This engine realizes a plan as a walk plus the WHERE, which has only
// the second answer, so a winning loop using such a term declines
// (wherePlanInRhsRefuse); a plan that does not use it still answers.
func TestInSelectRhsCollateStaysDeclined(t *testing.T) {
	base := []string{"CREATE TABLE t(a, d TEXT)", "CREATE INDEX td ON t(d)",
		"INSERT INTO t VALUES(1,'b'),(2,'A'),(3,'a'),(4,'B'),(5,'c')",
		"CREATE TABLE u(y TEXT)", "INSERT INTO u VALUES('a'),('B')"}
	indexed := append(append([]string{}, base...),
		"SELECT group_concat(a) FROM t WHERE d IN (SELECT y COLLATE nocase FROM u)")
	if got := run(t, "cgo", indexed)[len(indexed)-1]; got["kind"] != "rows" {
		t.Fatalf("test premise wrong: oracle answered %v", got)
	}
	if got := run(t, "musql", indexed)[len(indexed)-1]["kind"]; got != "error" {
		t.Errorf("expected a decline for the indexed IN loop, got kind=%v", got)
	}
	differ(t, "in-collate not indexed", append(append([]string{}, base...),
		"SELECT group_concat(a) FROM t NOT INDEXED WHERE d IN (SELECT y COLLATE nocase FROM u)"))
	// A rowid is never TEXT, so no collation can merge two of its values.
	differ(t, "in-collate rowid", append(append([]string{}, base...), "INSERT INTO u VALUES('3'), ('1')",
		"SELECT group_concat(a) FROM t WHERE rowid IN (SELECT y COLLATE nocase FROM u)"))
	differ(t, "in-collate matching", append(append([]string{}, base...),
		"SELECT group_concat(a) FROM t WHERE d COLLATE nocase IN (SELECT y COLLATE nocase FROM u)"))
}
