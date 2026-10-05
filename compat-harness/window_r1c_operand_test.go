// This file tests window function operands (PARTITION BY / ORDER BY keys
// and positional function arguments), comparing compiled results against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// r1cFixture creates test tables with peer groups, NULLs, mixed types, and collations.
var r1cFixture = []string{
	`CREATE TABLE w(a, b TEXT, c TEXT COLLATE NOCASE, d)`,
	`INSERT INTO w VALUES(1,'A','apple',10),(2,'B','APPLE',20),(3,'C','pear',30),` +
		`(3,'D','PEAR',30),(5,'E','fig',50),(7,'F','Fig',70),(NULL,'G','date',NULL)`,
	`CREATE TABLE u(k, v)`,
	`INSERT INTO u VALUES(1,100),(2,200),(9,900)`,
}

func r1cParity(t *testing.T, name string, probes []string) {
	t.Helper()
	driverParity(t, name, append(append([]string(nil), r1cFixture...), probes...))
}

// TestR1CWindowOperandKeys drives the PARTITION BY / ORDER BY operands, which
// SQLite appends to its sub-select's expression list at window.c:1029-1030 and
// this engine now computes into batch columns. Every key here is an
// EXPRESSION, not a bare column, so a lowering that silently changed the value
// would move rows between partitions or between peer groups.
func TestR1CWindowOperandKeys(t *testing.T) {
	r1cParity(t, "keys-expressions", []string{
		`SELECT b, sum(d) OVER (PARTITION BY a%2 ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (PARTITION BY a IS NULL ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (PARTITION BY CASE WHEN a>2 THEN 'hi' ELSE 'lo' END ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY CAST(a AS TEXT)) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY b||a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY -a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a*1.0/2) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (PARTITION BY a%2, a>2 ORDER BY a, b) FROM w ORDER BY b`,
		`SELECT b, count(*) OVER (PARTITION BY (a IS NOT NULL) ORDER BY b DESC) FROM w ORDER BY b`,
	})

	r1cParity(t, "keys-collate-and-nulls", []string{
		// A key's COLLATION decides peer grouping, so 'apple'/'APPLE' are one
		// peer group under NOCASE and two under BINARY.
		`SELECT b, dense_rank() OVER (ORDER BY c) FROM w ORDER BY b`,
		`SELECT b, dense_rank() OVER (ORDER BY c COLLATE BINARY) FROM w ORDER BY b`,
		`SELECT b, dense_rank() OVER (ORDER BY b COLLATE NOCASE) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY c COLLATE NOCASE, b) FROM w ORDER BY b`,
		// Explicit NULLS placement moves the NULL-keyed row, which the RANGE
		// path's non-NULL run (rangeKeys.nnLo/nnHi) depends on.
		`SELECT b, sum(d) OVER (ORDER BY a NULLS FIRST) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a NULLS LAST) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a DESC NULLS FIRST) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a DESC NULLS LAST) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a NULLS LAST RANGE BETWEEN UNBOUNDED PRECEDING AND 2 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a NULLS LAST RANGE BETWEEN 2 FOLLOWING AND UNBOUNDED FOLLOWING) FROM w ORDER BY b`,
	})

	r1cParity(t, "keys-subqueries", []string{
		// An UNCORRELATED subquery key is a per-row constant; a CORRELATED one
		// reads the scanned row. Both are compiled operands now (or roll back
		// together), and both must still agree.
		`SELECT b, sum(d) OVER (ORDER BY (SELECT max(v) FROM u)) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY (SELECT v FROM u WHERE k=w.a)) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (PARTITION BY (SELECT count(*) FROM u WHERE k<w.a) ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a IN (SELECT k FROM u), a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY EXISTS(SELECT 1 FROM u WHERE k=w.a), a) FROM w ORDER BY b`,
	})
}

// TestR1CWindowOperandArgs drives the arguments of the six POSITIONAL window
// functions -- the ones whose values the window machinery reads back itself
// (window.c:1942/:1952/:1966/:1975/:1982). They are the arguments this batch
// lowers; an ordinary aggregate's argument is deliberately left to
// aggItem.rowValue and is covered by the aggregate cases below.
func TestR1CWindowOperandArgs(t *testing.T) {
	r1cParity(t, "args-positional", []string{
		`SELECT b, first_value(d*2) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, last_value(b||'!') OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, nth_value(d+1, 2) OVER (ORDER BY a) FROM w ORDER BY b`,
		// nth_value's N is evaluated at the CURRENT row, not once per partition.
		`SELECT b, nth_value(b, a) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, lead(d*3) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, lag(CASE WHEN a>2 THEN b ELSE c END) OVER (ORDER BY a) FROM w ORDER BY b`,
		// lead/lag's OFFSET and DEFAULT are per-row too. (The offset is read
		// at a row where it is a plain integer; a NULL or TEXT offset is a
		// PRE-EXISTING divergence of its own -- C SQLite gives the offset
		// numeric affinity and adds it to a rowid, window.c:1966-1982, where
		// this engine requires an integer -- and is not this batch's to fix.)
		`SELECT b, lead(b, a) OVER (ORDER BY a) FROM w WHERE a IS NOT NULL ORDER BY b`,
		`SELECT b, lag(b, 1, b||'?') OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, lead(b, 2, upper(c)) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, lag(d, -1, -1) OVER (ORDER BY a) FROM w ORDER BY b`,
		// ntile's N comes from the partition's FIRST row.
		`SELECT b, ntile(a) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, ntile(2+1) OVER (PARTITION BY a%2 ORDER BY a) FROM w ORDER BY b`,
		// The arity/domain errors these functions raise must still be raised.
		`SELECT ntile(0) OVER (ORDER BY a) FROM w`,
		`SELECT ntile(NULL) OVER (ORDER BY a) FROM w`,
		`SELECT nth_value(b, 0) OVER (ORDER BY a) FROM w`,
		`SELECT nth_value(b, a-1) OVER (ORDER BY a) FROM w`,
		`SELECT row_number(a) OVER (ORDER BY a) FROM w`,
	})

	r1cParity(t, "args-subquery", []string{
		`SELECT b, first_value((SELECT max(v) FROM u)) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, lag((SELECT v FROM u WHERE k=w.a)) OVER (ORDER BY a) FROM w ORDER BY b`,
	})
}

// TestR1CWindowFrames re-drives the whole frame surface now that the bound
// offsets live in registers computed ahead of the scan rather than in a per-row
// expression evaluation.
func TestR1CWindowFrames(t *testing.T) {
	r1cParity(t, "frame-modes", []string{
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a GROUPS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a RANGE BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a GROUPS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS 2 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a GROUPS 1 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN 1 FOLLOWING AND 2 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN 3 FOLLOWING AND 4 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, count(d) OVER (ORDER BY a ROWS BETWEEN 3 FOLLOWING AND 4 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (PARTITION BY a%2 ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, group_concat(b) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
	})

	r1cParity(t, "frame-exclude", []string{
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE NO OTHERS) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE GROUP) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE TIES) FROM w ORDER BY b`,
		`SELECT b, first_value(b) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, last_value(b) OVER (ORDER BY a GROUPS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE GROUP) FROM w ORDER BY b`,
		`SELECT b, nth_value(b,2) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE TIES) FROM w ORDER BY b`,
	})
}

// TestR1CFrameOffsetConstants pins the CONSTANT-expression rule for a frame
// bound's offset. sqlite3WindowOffsetExpr (window.c:1163-1170) replaces a
// non-constant offset with a literal NULL at parse time, and windowCheckValue
// then reports "frame starting offset must be a non-negative integer"; this
// engine now reproduces that substitution at compile time, so the accept/reject
// split has to be SQLite's exactly.
func TestR1CFrameOffsetConstants(t *testing.T) {
	r1cParity(t, "offset-constant-accepted", []string{
		`SELECT b, sum(d) OVER (ORDER BY a ROWS 2 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS 2.0 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS '2' PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS (2) PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS 1+1 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS -(-2) PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS CAST('2' AS INTEGER) PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS 2||'' PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS CASE WHEN 1 THEN 2 ELSE 3 END PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a RANGE 1.5 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a RANGE BETWEEN '  3 ' PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		// A bare TRUE/FALSE is an IDENTIFIER that exprNodeIsConstant's TK_ID arm
		// rewrites into TK_TRUEFALSE and prunes as CONSTANT (expr.c:2568-2573),
		// so it is a legal offset. window1.test section 13 writes exactly the
		// RANGE spelling below, and it is what caught this engine classifying
		// the token as an ordinary column reference.
		`SELECT b, sum(d) OVER (ORDER BY a ROWS true PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS FALSE PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS true+1 PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a ROWS CASE WHEN 1 THEN true ELSE false END PRECEDING) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a RANGE BETWEEN 5.2 PRECEDING AND true PRECEDING) FROM w ORDER BY b`,
		`SELECT b, count(*) OVER win FROM w WINDOW win AS (PARTITION BY b ORDER BY a RANGE BETWEEN 5.2 PRECEDING AND true PRECEDING) ORDER BY b`,
	})

	// sqlite3WindowOffsetExpr runs at PARSE time, so a COLUMN named "true"
	// cannot shadow the keyword in a frame offset the way it does everywhere
	// else -- the identifier was already rewritten before resolution ran.
	driverParity(t, "offset-true-vs-column", []string{
		`CREATE TABLE tf(true, a, d)`,
		`INSERT INTO tf VALUES(5,1,10),(5,2,20),(5,3,30)`,
		`SELECT true FROM tf`,
		`SELECT a, sum(d) OVER (ORDER BY a ROWS true PRECEDING) FROM tf ORDER BY a`,
		`SELECT a, sum(d) OVER (ORDER BY a ROWS "true" PRECEDING) FROM tf ORDER BY a`,
	})

	r1cParity(t, "offset-non-constant-rejected", []string{
		// A COLUMN, a FUNCTION call and a SUBQUERY are all non-constant.
		`SELECT sum(d) OVER (ORDER BY a ROWS a PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS abs(2) PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS length('ab') PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS max(2,1) PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS coalesce(2,3) PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS nullif(2,3) PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS (SELECT 2) PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a RANGE (SELECT max(v) FROM u) PRECEDING) FROM w`,
		// LIKE/GLOB are FUNCTION calls in SQLite's grammar (parse.y:1363),
		// not operators, so they are non-constant there too.
		`SELECT sum(d) OVER (ORDER BY a ROWS ('a' LIKE 'a')+1 PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS ('a' GLOB 'a')+1 PRECEDING) FROM w`,
		// Negative / non-numeric constants are accepted by the grammar and
		// rejected at run time.
		`SELECT sum(d) OVER (ORDER BY a ROWS -1 PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS NULL PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS 'x' PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a RANGE 'x' PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS x'3132' PRECEDING) FROM w`,
		`SELECT sum(d) OVER (ORDER BY a ROWS 1.5 PRECEDING) FROM w`,
		// A function that does not READ the frame does not validate it.
		`SELECT b, row_number() OVER (ORDER BY a ROWS BETWEEN -1 PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, row_number() OVER (ORDER BY a ROWS abs(1) PRECEDING) FROM w ORDER BY b`,
	})

	// A CORRELATED column reference in the offset is non-constant too, so it is
	// the same NULL and the same error -- which the route this replaced
	// reached only by failing to resolve it, and this one reaches structurally
	// (window.c:1163-1170). Both spellings agreed before and after; they are
	// pinned so a future scope change cannot start ANSWERING them.
	//
	// The EXISTS spelling is deliberately absent: C SQLite never evaluates
	// the window at all there and answers rows where this engine reports the
	// offset error, which is a PRE-EXISTING planner divergence unrelated to the
	// offset's own rule.
	r1cParity(t, "offset-correlated-column", []string{
		`SELECT k, (SELECT sum(d) OVER (ORDER BY a ROWS u.k PRECEDING) FROM w LIMIT 1) FROM u ORDER BY k`,
		`SELECT k, (SELECT sum(d) OVER (ORDER BY a RANGE u.k PRECEDING) FROM w LIMIT 1) FROM u ORDER BY k`,
	})
}

// TestR1CFrameOffsetParameter drives "ROWS ? PRECEDING". A bound parameter is
// CONSTANT under sqlite3ExprIsConstant -- TK_VARIABLE falls through to
// WRC_Continue (expr.c:2599-2615), unlike the eCode==4 spelling a CREATE
// statement uses -- so the offset compiles into the register rather than
// becoming NULL. That makes it re-evaluated on every EXECUTION of one prepared
// statement, which a compile-time fold would silently get wrong, so the same
// statement is run repeatedly with different bindings.
func TestR1CFrameOffsetParameter(t *testing.T) {
	var want []string
	for _, drv := range []string{"sqlite3", "sqlite"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "p.sqlite")
		}
		db, err := sql.Open(drv, dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, s := range []string{
			`CREATE TABLE p(a,d)`,
			`INSERT INTO p VALUES(1,10),(2,20),(3,30),(4,40)`,
		} {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("[%s] %s: %v", drv, s, err)
			}
		}
		st, err := db.Prepare(`SELECT a, sum(d) OVER (ORDER BY a ROWS ? PRECEDING) FROM p ORDER BY a`)
		if err != nil {
			t.Fatalf("[%s] prepare: %v", drv, err)
		}
		var got []string
		for _, n := range []any{0, 1, 2, 10, "2", -1, nil} {
			rows, qerr := st.Query(n)
			if qerr != nil {
				got = append(got, "ERR")
				continue
			}
			out := ""
			for rows.Next() {
				var a, s any
				if serr := rows.Scan(&a, &s); serr != nil {
					out = "SCANERR"
					break
				}
				out += fmt.Sprintf(" %v/%v", a, s)
			}
			if rows.Err() != nil {
				out = "ERR"
			}
			rows.Close()
			got = append(got, out)
		}
		st.Close()
		db.Close()
		if want == nil {
			want = got
			continue
		}
		for i := range want {
			if want[i] != got[i] {
				t.Errorf("param #%d\n  engine: %s\n  cgo:    %s", i, got[i], want[i])
			}
		}
	}
}

// TestR1CWindowSpecsAndFunctions covers named windows, the multi-spec nesting
// rule, and every window function the engine implements.
func TestR1CWindowSpecsAndFunctions(t *testing.T) {
	r1cParity(t, "named-and-multi-spec", []string{
		`SELECT b, sum(d) OVER win FROM w WINDOW win AS (PARTITION BY a%2 ORDER BY a) ORDER BY b`,
		`SELECT b, sum(d) OVER (win ORDER BY a) FROM w WINDOW win AS (PARTITION BY a%2) ORDER BY b`,
		`SELECT b, sum(d) OVER w2 FROM w WINDOW w1 AS (ORDER BY a), w2 AS (w1 ROWS 1 PRECEDING) ORDER BY b`,
		// Two calls sharing ONE spec pointer must share their key columns
		// without either seeing the other's values.
		`SELECT b, sum(d) OVER win, count(*) OVER win, rank() OVER win FROM w WINDOW win AS (PARTITION BY a%2 ORDER BY a) ORDER BY b`,
		// Distinct specs are NESTED window queries, first-written outermost --
		// the emit order and the values both depend on getting that right.
		`SELECT b, sum(d) OVER (ORDER BY a), count(*) OVER (ORDER BY c) FROM w`,
		`SELECT b, count(*) OVER (ORDER BY c), sum(d) OVER (ORDER BY a) FROM w`,
		`SELECT a, sum(d) OVER (ORDER BY a ROWS 1 PRECEDING), sum(d) OVER (ORDER BY a RANGE UNBOUNDED PRECEDING) FROM w`,
		`SELECT b, row_number() OVER (PARTITION BY a%2 ORDER BY b), row_number() OVER (PARTITION BY a IS NULL ORDER BY b DESC) FROM w`,
		// ... and with DISTINCT / LIMIT / an outer ORDER BY on top.
		`SELECT DISTINCT a%2, sum(d) OVER (PARTITION BY a%2) FROM w`,
		`SELECT b, sum(d) OVER (ORDER BY a) AS s FROM w ORDER BY s, b LIMIT 4`,
		`SELECT b, sum(d) OVER (ORDER BY a) FROM w ORDER BY 2, 1 LIMIT 3 OFFSET 2`,
	})

	r1cParity(t, "every-window-function", []string{
		`SELECT b, row_number() OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, rank() OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, dense_rank() OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, percent_rank() OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, cume_dist() OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, ntile(3) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, lead(b) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, lag(b) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, first_value(b) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, last_value(b) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, nth_value(b,2) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, count(*) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, count(a) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, sum(d) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, total(d) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, avg(d) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, min(d) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, max(d) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, group_concat(b) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, group_concat(b,'-') OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, json_group_array(a) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, json_group_object(b,a) OVER (ORDER BY a) FROM w ORDER BY b`,
		// DISTINCT is never legal in a window function, for any aggregate.
		`SELECT sum(DISTINCT d) OVER (ORDER BY a) FROM w`,
	})
}

// TestR1CWindowWithAggregates covers a window over an aggregate, an aggregate
// over a window, and FILTER -- the shapes where the window's own operands and
// the AGGREGATE machinery's operands (aggItem.rowValue, which this batch
// deliberately does not touch) meet.
func TestR1CWindowWithAggregates(t *testing.T) {
	r1cParity(t, "window-over-aggregate", []string{
		`SELECT a%2 AS p, sum(d) AS s, sum(sum(d)) OVER (ORDER BY a%2) FROM w GROUP BY a%2 ORDER BY p`,
		`SELECT a%2 AS p, count(*) AS n, row_number() OVER (ORDER BY count(*) DESC, a%2) FROM w GROUP BY a%2 ORDER BY p`,
		`SELECT a%2 AS p, first_value(sum(d)) OVER (ORDER BY a%2) FROM w GROUP BY a%2 ORDER BY p`,
		`SELECT a%2 AS p, sum(d) OVER (PARTITION BY a%2) FROM w GROUP BY a%2 ORDER BY p`,
		`SELECT a%2 AS p, lead(sum(d)) OVER (ORDER BY a%2) FROM w GROUP BY a%2 ORDER BY p`,
	})

	r1cParity(t, "aggregate-over-window", []string{
		`SELECT sum(s) FROM (SELECT sum(d) OVER (ORDER BY a) AS s FROM w)`,
		`SELECT count(*), max(r) FROM (SELECT rank() OVER (ORDER BY a) AS r FROM w)`,
		`SELECT group_concat(v) FROM (SELECT first_value(b||a) OVER (PARTITION BY a%2 ORDER BY a) AS v FROM w)`,
	})

	r1cParity(t, "filter", []string{
		`SELECT b, sum(d) FILTER (WHERE a>2) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, count(*) FILTER (WHERE a IS NOT NULL) OVER (PARTITION BY a%2 ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, group_concat(b) FILTER (WHERE b<'D') OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d) FILTER (WHERE (SELECT count(*) FROM u WHERE k=w.a)>0) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, max(d) FILTER (WHERE a%2=1) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
	})
}

// TestR1CWindowLoweringRollback drives the shapes planWindowOperands refuses
// (or emitWindowOperands fails to compile). A refusal is ALL-OR-NOTHING for the
// WHOLE query -- there is no fallback, so the statement declines -- and that
// outcome must still agree with C SQLite's (queryString collapses either
// engine's failure to "ERR", so a shape SQLite also rejects agrees). If the
// refusal were PARTIAL -- some operands read as columns that were never written
// -- these would come out silently wrong rather than merely failing.
func TestR1CWindowLoweringRollback(t *testing.T) {
	r1cParity(t, "rollback-shapes", []string{
		// A window call written INSIDE another window's operand: compileFunc
		// would drop the OVER, so windowOperandLowerable refuses it outright.
		`SELECT b, sum(d) OVER (ORDER BY row_number() OVER (ORDER BY a)) FROM w`,
		`SELECT b, first_value(row_number() OVER (ORDER BY a)) OVER (ORDER BY a) FROM w`,
		// An aggregate inside a spec / inside a positional argument.
		`SELECT b, sum(d) OVER (ORDER BY sum(d)) FROM w`,
		`SELECT b, first_value(sum(d)) OVER (ORDER BY a) FROM w`,
		// A window inside a spec's SUBQUERY belongs to that subquery and is
		// served; it is the one nested-window shape that is not a rollback.
		`SELECT b, sum(d) OVER (ORDER BY (SELECT max(r) FROM (SELECT rank() OVER (ORDER BY k) r FROM u))) FROM w ORDER BY b`,
	})
}

// TestR1CWindowJoinsAndViews puts the lowered operands under a scan body that
// emitJoinLoops emits MORE THAN ONCE -- a LEFT/RIGHT/FULL join's NULL-extension
// pass -- which is the exact hazard planAggArgRegs' doc comment records for the
// aggregate half (distinctagg.test 6.1 answered 0 instead of 1 until the
// registers were hoisted out of the body).
func TestR1CWindowJoinsAndViews(t *testing.T) {
	r1cParity(t, "joins", []string{
		`SELECT w.b, u.v, sum(w.d) OVER (PARTITION BY u.k IS NULL ORDER BY w.a) FROM w LEFT JOIN u ON u.k=w.a ORDER BY w.b`,
		`SELECT w.b, u.v, first_value(u.v*2) OVER (ORDER BY w.a) FROM w LEFT JOIN u ON u.k=w.a ORDER BY w.b`,
		`SELECT w.b, u.k, lag(u.v,1,u.k) OVER (ORDER BY w.a, u.k) FROM w LEFT JOIN u ON u.k=w.a ORDER BY w.b`,
		`SELECT w.b, u.k, count(*) OVER (PARTITION BY u.k%2 ORDER BY w.a) FROM w RIGHT JOIN u ON u.k=w.a ORDER BY w.b, u.k`,
		`SELECT w.b, u.k, sum(u.v) OVER (ORDER BY w.a NULLS LAST, u.k) FROM w FULL JOIN u ON u.k=w.a ORDER BY w.b, u.k`,
		`SELECT w.b, u.v, sum(w.d) OVER (ORDER BY w.a ROWS 1 PRECEDING) FROM w JOIN u ON u.k=w.a ORDER BY w.b`,
	})

	r1cParity(t, "views-and-ctes", []string{
		`CREATE VIEW vw AS SELECT a, b, d FROM w WHERE a IS NOT NULL`,
		`SELECT b, sum(d) OVER (PARTITION BY a%2 ORDER BY a) FROM vw ORDER BY b`,
		`WITH x AS (SELECT a, b, d FROM w) SELECT b, first_value(d*2) OVER (ORDER BY a) FROM x ORDER BY b`,
		`SELECT b, ntile(a) OVER (ORDER BY a) FROM (SELECT * FROM w WHERE a IS NOT NULL) ORDER BY b`,
	})
}

// TestR1CWindowJSONSubtype guards the JSON subtype across the new register
// round trip: a lowered operand is copied into the batch record by
// OpMakeRecord exactly as a plain column is, so json()'s subtype has to survive
// (or be dropped) exactly as it does in C SQLite.
func TestR1CWindowJSONSubtype(t *testing.T) {
	driverParity(t, "json-subtype", []string{
		`CREATE TABLE j(x)`,
		`INSERT INTO j VALUES(1),(2),(3)`,
		`SELECT json_array(first_value(json(x)) OVER ()) FROM j`,
		`SELECT json_array(last_value(json_quote(x)) OVER (ORDER BY x)) FROM j`,
		`SELECT json_group_array(x) OVER (ORDER BY x) FROM j`,
		`SELECT json_array(lag(json('[1]'),1,json('[2]')) OVER (ORDER BY x)) FROM j`,
		`SELECT subtype(first_value(json(x)) OVER ()) FROM j`,
		`SELECT x, sum(x) OVER (ORDER BY json_extract(json_object('k',x),'$.k')) FROM j ORDER BY x`,
	})
}

// TestR1CWindowNoFrom covers the FROM-less window compiler, whose batch holds
// exactly one zero-column entry -- so an operand column is the ONLY thing in
// its record.
func TestR1CWindowNoFrom(t *testing.T) {
	driverParity(t, "no-from", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`SELECT sum(44) OVER ()`,
		`SELECT row_number() OVER (ORDER BY 1)`,
		`SELECT ntile(1) OVER ()`,
		`SELECT lag(7,1,99) OVER ()`,
		`SELECT first_value(1+1) OVER (ORDER BY 2)`,
		`SELECT nth_value(5,1) OVER (PARTITION BY 1)`,
		`SELECT (SELECT sum(a) OVER (ORDER BY a)) FROM t1`,
		`SELECT (SELECT row_number() OVER ()) FROM t1`,
		`SELECT (SELECT first_value(t1.a*2) OVER ()) FROM t1`,
		`SELECT sum(1) OVER () LIMIT 1 OFFSET 1`,
	})
}
