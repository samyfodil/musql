package compat

// Distinct aggregates in GROUP BY with index ordering.
//
//	stmt17: SELECT a, count(DISTINCT b) FROM t1 GROUP BY a
//	  (t1 carries "CREATE INDEX t1a ON t1(a)") -- CLOSED by this fix.
//	stmt6:  SELECT COUNT(DISTINCT TRUE) FROM v1 GROUP BY likelihood(v3, 0.1)
//	  (v1(v2 UNIQUE, v3 AS(TYPEOF(NULL)) UNIQUE)) -- CLOSED, but by a SEPARATE
//	  fix (wherePlanColUsed's own FallbackLiteral gap, same file), not this
//	  one: this statement's actual blocker was never the generated-column
//	  auto-index at all -- wherePlanIndexList reconstructs v3's UNIQUE
//	  constraint correctly (C SQLite's own build.c builds a PLAIN
//	  column-keyed autoindex for it too, verified directly against the
//	  oracle's "PRAGMA index_xinfo": cid=1, not an expression column -- a
//	  UNIQUE constraint can never declare an expression key, only a bare
//	  CREATE INDEX can). The bare "TRUE" in "COUNT(DISTINCT TRUE)" parses as
//	  a ColumnExpr carrying FallbackLiteral (sql_ast.go: a real column always
//	  wins, so resolution is attempted first) -- and wherePlanColUsed's own
//	  column walk, unlike every other resolver in this codebase
//	  (compileColumn, resolveColumnEx, ...), did not know that an unqualified
//	  ColumnExpr with no matching column and a non-nil FallbackLiteral
//	  contributes NO colUsed bit at all (resolve.c:718-747's
//	  sqlite3ExprIdToTrueFalse rewrites it to a literal before where.c's own
//	  colUsed walk ever runs), so it declined the WHOLE table outright before
//	  wherePlanIndexList was ever reached. A prior investigation of this exact
//	  statement mis-attributed the decline to the generated-column index
//	  question above without tracing which gate actually returned false --
//	  confirmed wrong by instrumenting wherePlanSingleTableIndexOrder's own
//	  call chain directly.
import "testing"

func TestAnchorOrderDistinctAggGroupBy(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{
			"stmt17-index-on-group-col",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1a ON t1(a)`,
				`CREATE INDEX t1bc ON t1(b, c)`,
				`INSERT INTO t1 VALUES (1, 1, 1)`,
				`INSERT INTO t1 VALUES (2, 2, 2)`,
				`INSERT INTO t1 VALUES (3, 3, 3)`,
				`INSERT INTO t1 VALUES (4, 1, 4)`,
				`INSERT INTO t1 VALUES (5, 2, 1)`,
				`INSERT INTO t1 VALUES (5, 3, 2)`,
				`INSERT INTO t1 VALUES (4, 1, 3)`,
				`INSERT INTO t1 VALUES (3, 2, 4)`,
				`INSERT INTO t1 VALUES (2, 3, 1)`,
				`INSERT INTO t1 VALUES (1, 1, 2)`,
				`SELECT a, count(DISTINCT b) FROM t1 GROUP BY a`,
			},
		},
		// stmt6, the mined statement itself -- CLOSED, but by the
		// wherePlanColUsed FallbackLiteral fix described above, not the
		// wherePlanSortCtlFor DISTINCT-aggregate narrowing this file's other
		// cases exercise. v1's own UNIQUE(v3) constraint on a VIRTUAL
		// generated column whose expression (TYPEOF(NULL)) is the same value
		// for every row means the table can hold at most one row, so this
		// case only proves the statement no longer declines -- see
		// TestAnchorOrderDistinctAggGeneratedColumnIndex below for the same
		// mechanism exercised with actual data (multiple distinct rows) over
		// a generated-column index.
		{
			"stmt6-mined-statement",
			[]string{
				`CREATE TABLE v1 ( v2 UNIQUE, v3 AS( TYPEOF ( NULL ) ) UNIQUE )`,
				`SELECT COUNT ( DISTINCT TRUE ) FROM v1 GROUP BY likelihood ( v3 , 0.100000 )`,
			},
		},
		//
		// DESC-index control: the exact fixture the groupEmissionOrderProvable
		// doc comment measures against C SQLite -- groups descend by a
		// there, not ascend. Confirms the fix does not silently paper over that
		// case with a wrong ascending answer; it must still be handled/declined
		// correctly (groupsArriveOutOfKeyOrder / anchorLoopOrderProvable).
		{
			"desc-index-on-group-col",
			[]string{
				`CREATE TABLE t(a, b, c)`,
				`CREATE INDEX i1 ON t(a DESC)`,
				`INSERT INTO t VALUES(3,'x',10)`,
				`INSERT INTO t VALUES(1,'Y',20)`,
				`INSERT INTO t VALUES(2,'x',30)`,
				`INSERT INTO t VALUES(2,'z',40)`,
				`INSERT INTO t VALUES(NULL,'w',50)`,
				`INSERT INTO t VALUES(1.0,'X',60)`,
				`SELECT count(DISTINCT a) FROM t GROUP BY a`,
			},
		},
		// No usable index at all: must fall back to this engine's ascending
		// sorter, matching the oracle's own GROUP BY sort.
		{
			"no-index-on-group-col",
			[]string{
				`CREATE TABLE t(a, b, c)`,
				`INSERT INTO t VALUES(3,'x',10)`,
				`INSERT INTO t VALUES(1,'Y',20)`,
				`INSERT INTO t VALUES(2,'x',30)`,
				`INSERT INTO t VALUES(2,'z',40)`,
				`INSERT INTO t VALUES(NULL,'w',50)`,
				`INSERT INTO t VALUES(1.0,'X',60)`,
				`SELECT count(DISTINCT a) FROM t GROUP BY a`,
			},
		},
		// Multiple DISTINCT aggregates -- nFunc != 1 in select.c, so distFlag is
		// 0 there regardless; still must decide the same as the single-agg case.
		{
			"two-distinct-aggs-index",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES (1, 1, 1)`,
				`INSERT INTO t1 VALUES (2, 2, 2)`,
				`INSERT INTO t1 VALUES (3, 3, 3)`,
				`INSERT INTO t1 VALUES (2, 1, 4)`,
				`INSERT INTO t1 VALUES (1, 2, 1)`,
				`SELECT a, count(DISTINCT b), count(DISTINCT c) FROM t1 GROUP BY a`,
			},
		},
		// WHERE-filtered variant over the same index, to exercise the WHERE-term
		// path through the now-unblocked planner.
		{
			"where-filtered-index",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES (1, 1, 1)`,
				`INSERT INTO t1 VALUES (2, 2, 2)`,
				`INSERT INTO t1 VALUES (3, 3, 3)`,
				`INSERT INTO t1 VALUES (2, 1, 4)`,
				`INSERT INTO t1 VALUES (1, 2, 1)`,
				`SELECT a, count(DISTINCT b) FROM t1 WHERE a >= 2 GROUP BY a`,
			},
		},
		// Whole-table (no GROUP BY) aggregate with a DISTINCT argument -- the
		// fix deliberately does not touch this arm (select.c hands it
		// pMinMaxOrderBy, NULL here since count() isn't min/max, so pResultSet
		// DOES drive path selection there -- a case this port does not model).
		// It passes anyway, for an unrelated reason: with no GROUP BY there is
		// only one output row, so groupEmissionOrderProvable never applies
		// (len(gp.groupExprs)==0 is a whole different code path), and
		// count(DISTINCT x) is order-independent (aggItem.step, sql_agg.go:
		// "count(DISTINCT x) is unaffected either way and is never armed") --
		// so no order question exists here at all. Included as a regression
		// guard on that separate reasoning, not because this fix changed it.
		{
			"whole-table-distinct-agg-with-index",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES (1, 1, 1)`,
				`INSERT INTO t1 VALUES (2, 2, 2)`,
				`INSERT INTO t1 VALUES (3, 3, 3)`,
				`SELECT count(DISTINCT a) FROM t1`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}

	// join-distinct-agg-groupby is deliberately NOT in the differ() battery
	// above: a 2-table join with a DISTINCT aggregate argument stays declined
	// on purpose (whereOmitNoopJoin's suppression, where.c:7174/7172, is a
	// real, unmodelled difference for nLevel>=2, so the fix's gate requires
	// len(jts)==1). Asserting agreement here would be a permanently-red test
	// for a known, out-of-scope limitation, not a regression signal.
	// Verified (git-stash A/B, see the commit that introduced this file) that
	// it declined identically before this fix, so nothing here regressed.
}

// TestAnchorOrderDistinctAggGeneratedColumnIndex exercises stmt6's actual
// mechanism (wherePlanColUsed's FallbackLiteral gap, see this file's own
// header comment) with real, multi-row data over a table whose ONLY index
// is on a VIRTUAL generated column -- v1's own UNIQUE(v3) in the mined
// statement cannot be exercised this way (TYPEOF(NULL) is the same value
// for every row, so the table can hold at most one), so this uses a
// non-constant generated expression (v2+1) instead, isolating the
// generated-column-index question the prior investigation of this bucket
// raised (and got wrong -- see the header comment) from the constant-value
// degenerate shape the corpus happened to mine.
func TestAnchorOrderDistinctAggGeneratedColumnIndex(t *testing.T) {
	differ(t, "generated-col-index-distinct-true", []string{
		`CREATE TABLE v4 ( v2 UNIQUE, v3 AS( v2 + 1 ) UNIQUE, v5 )`,
		`INSERT INTO v4(v2, v5) VALUES (3, 'z')`,
		`INSERT INTO v4(v2, v5) VALUES (1, 'x')`,
		`INSERT INTO v4(v2, v5) VALUES (2, 'y')`,
		`SELECT v5, COUNT(DISTINCT TRUE) FROM v4 GROUP BY likelihood(v3, 0.1)`,
	})
}

// TestAnchorOrderDistinctAggGroupByDistinctaggFixture replays six more
// single-table, GROUP-BY-with-DISTINCT-argument statements straight out of
// distinctagg.test's own section 3 fixture (SQLite 3.53.3) -- same bucket,
// additional corpus coverage beyond the two statements the task named. Every
// one of these was a decline before this fix (verified by git-stash A/B, same
// method as TestAnchorOrderDistinctAggGroupBy above).
func TestAnchorOrderDistinctAggGroupByDistinctaggFixture(t *testing.T) {
	fixture := []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX t1a ON t1(a)`,
		`CREATE INDEX t1bc ON t1(b, c)`,
		`INSERT INTO t1 VALUES(1, 'A', 1)`,
		`INSERT INTO t1 VALUES(1, 'A', 1)`,
		`INSERT INTO t1 VALUES(2, 'A', 2)`,
		`INSERT INTO t1 VALUES(2, 'A', 2)`,
		`INSERT INTO t1 VALUES(1, 'B', 1)`,
		`INSERT INTO t1 VALUES(2, 'B', 2)`,
		`INSERT INTO t1 VALUES(3, 'B', 3)`,
		`INSERT INTO t1 VALUES(NULL, 'B', NULL)`,
		`INSERT INTO t1 VALUES(NULL, 'C', NULL)`,
		`INSERT INTO t1 VALUES('d', 'D', 'd')`,
		`CREATE TABLE t2(d, e, f)`,
		`CREATE INDEX t2def ON t2(d, e, f)`,
		`INSERT INTO t2 VALUES(1, 1, 'a')`,
		`INSERT INTO t2 VALUES(1, 1, 'a')`,
		`INSERT INTO t2 VALUES(1, 2, 'a')`,
		`INSERT INTO t2 VALUES(1, 2, 'a')`,
		`INSERT INTO t2 VALUES(1, 2, 'b')`,
		`INSERT INTO t2 VALUES(1, 3, 'b')`,
		`INSERT INTO t2 VALUES(1, 3, 'a')`,
		`INSERT INTO t2 VALUES(1, 3, 'b')`,
		`INSERT INTO t2 VALUES(2, 3, 'x')`,
		`INSERT INTO t2 VALUES(2, 3, 'y')`,
		`INSERT INTO t2 VALUES(2, 3, 'z')`,
	}
	stmts := []struct {
		name string
		sql  string
	}{
		{"count-distinct-c-group-b", `SELECT count(DISTINCT c) FROM t1 GROUP BY b`},
		{"count-distinct-a-group-b", `SELECT count(DISTINCT a) FROM t1 GROUP BY b`},
		{"count-distinct-a-group-expr", `SELECT count(DISTINCT a) FROM t1 GROUP BY b+c`},
		{"count-distinct-f-group-d-e", `SELECT count(DISTINCT f) FROM t2 GROUP BY d, e`},
		{"count-distinct-f-group-d", `SELECT count(DISTINCT f) FROM t2 GROUP BY d`},
		{"count-distinct-f-where-group-e", `SELECT count(DISTINCT f) FROM t2 WHERE d IS 1 GROUP BY e`},
	}
	for _, s := range stmts {
		t.Run(s.name, func(t *testing.T) {
			differ(t, s.name, append(append([]string{}, fixture...), s.sql))
		})
	}
}

// TestAnchorOrderDistinctAggFallbackLiteralOuterRefStaysDeclined is a
// review-caught regression: wherePlanColUsed's FallbackLiteral exception
// (see this file's own header comment) is exact only for a TOP-LEVEL
// (c.outer == nil) single-table GROUP BY -- compileColumn's own analogous
// rule (vdbe_codegen.go) is safe because it walks c.outer UNQUALIFIED
// first, and only falls through to its own FallbackLiteral check once that
// whole enclosing-scope search comes up empty (lookupName's own ordering,
// resolve.c's "while(pNC)" loop before the "cnt==0 && zTab==0" arm). But
// wherePlanOuterRef, sitting right next to this exception, recognizes only
// a QUALIFIED outer reference (its own doc comment) -- an unqualified bare
// "true"/"false" that genuinely correlates to a real, live outer column
// (SQLite permits an unquoted "true" as an ordinary column name) had no
// equivalent recognizer, so it silently fell through to the exception
// meant for the compile-time-constant case instead, producing a WRONG
// per-outer-row answer once the single-table GROUP BY/DISTINCT-aggregate
// order proof this whole fix unblocks ran. Verified live against the
// oracle: the exact statement below answers ONE row ([5, 1, 2]) on real
// SQLite; before the c.outer == nil guard, musql answered TWO wrong rows.
// Full correlated support (matching the oracle's own row) would need a
// deeper fix -- walking c.outer unqualified the way compileColumn does --
// deliberately out of scope for this narrow pass, so this only asserts the
// SAFE outcome (a clean decline, never a served wrong answer), via
// differAllowingDeclines rather than a strict differ().
func TestAnchorOrderDistinctAggFallbackLiteralOuterRefStaysDeclined(t *testing.T) {
	stmts := []string{
		`CREATE TABLE outer_t(true INTEGER, x INTEGER)`,
		`CREATE TABLE inner_t(y INTEGER, z INTEGER)`,
		`CREATE INDEX inner_y ON inner_t(y)`,
		`INSERT INTO outer_t VALUES (5,1),(7,2)`,
		`INSERT INTO inner_t VALUES (1,100),(1,200),(2,300),(2,300),(3,400)`,
		`SELECT true, x, (SELECT count(DISTINCT true) FROM inner_t GROUP BY y) FROM outer_t ORDER BY x`,
	}
	differAllowingDeclines(t, "correlated-unqualified-true-outer-ref", stmts)
}

// TestAnchorOrderDistinctAggFallbackLiteralOuterRefVariants pins the two
// sibling shapes the same review round confirmed diverge identically
// (group_concat instead of count, and a correlating WHERE clause) --
// making sure the c.outer == nil guard closes the whole class, not just
// the one exact reviewer-constructed statement. Same differAllowingDeclines
// reasoning as the sibling test above: a clean decline is the safe,
// expected outcome, not full oracle parity.
func TestAnchorOrderDistinctAggFallbackLiteralOuterRefVariants(t *testing.T) {
	base := []string{
		`CREATE TABLE outer_t2(true INTEGER, x INTEGER)`,
		`CREATE TABLE inner_t2(y INTEGER, z INTEGER)`,
		`CREATE INDEX inner_t2y ON inner_t2(y)`,
		`INSERT INTO outer_t2 VALUES (5,1),(7,2)`,
		`INSERT INTO inner_t2 VALUES (1,100),(1,200),(2,300),(2,300),(3,400)`,
	}
	cases := []struct {
		name string
		sql  string
	}{
		{"group-concat", `SELECT true, x, (SELECT group_concat(DISTINCT true) FROM inner_t2 GROUP BY y) FROM outer_t2 ORDER BY x`},
		{"correlating-where", `SELECT true, x, (SELECT count(DISTINCT z) FROM inner_t2 WHERE y=true GROUP BY y) FROM outer_t2 ORDER BY x`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differAllowingDeclines(t, c.name, append(append([]string{}, base...), c.sql))
		})
	}
}
