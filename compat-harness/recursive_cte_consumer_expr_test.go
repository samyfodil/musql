// This file gates recursive CTEs with computed select lists, verifying that
// LIMIT bounds are correctly applied to the consumer's output.
//
//   - an AGGREGATE call: for a non-grouped aggregate query sqlite3WhereEnd
//     finishes the WHOLE source scan and finalizeAggFunctions runs BEFORE
//     selectInnerLoop is called even once (select.c:8891-8911), so the LIMIT
//     bounds the aggregated OUTPUT and never the source;
//   - a WINDOW call: the FROM clause moves into a synthesized sub-query while
//     "ORDER BY, LIMIT and OFFSET remain part of the parent query"
//     (window.c:43-46, "SELECT REWRITING"), so the sub-query scanning the CTE
//     carries no LIMIT at all;
//
// Every negative case below runs over a recursion that TERMINATES on its own
// (ten rows), so a wrongly published bound is not a hang or a decline -- it is
// a visibly short answer this file's comparison catches. Both forms were
// mutation-tested: dropping the aggregate check makes nine of these queries
// diverge and dropping the window check makes four.
//
// The SUBQUERY cases at the end of that list are the one exception, and they
// are here as documentation rather than as a gate: a select-list subquery is
// compiled by its own compiler, so it never inherits the outer bound, and with
// no WHERE/ORDER BY/aggregate the outer scan stops at limit+offset rows
// whatever the select list says. Admitting subqueries in consumerItemIsPerRow
// diverges on NOTHING here -- verified by mutation. They stay refused for the
// reason that function's comment gives; these cases record the measurement.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func rcteConsumerPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	cdb.SetMaxOpenConns(1)
	return edb, cdb
}

// rcteConsumerExec runs each statement on both engines and fails on any
// accept/reject disagreement -- the only check available for a write, which
// produces no rows to compare. The rows themselves are compared afterwards by
// querying the table on both sides.
func rcteConsumerExec(t *testing.T, edb *engine.Session, cdb *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("accept/reject disagrees: %s\n  engine: %v\n  cgo:    %v", s, eErrOrNil(eerr), cerr)
		}
	}
}

// rcteInf is the deliberately non-terminating recursion both incrcorrupt.test
// statements use: nothing but the consumer's LIMIT ever ends it.
const rcteInf = "WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c) "

// rcteInf2 is the same with a second, TEXT column, so a truncation shows in the
// values as well as in the row count. Its payload is deliberately of BOUNDED
// width ("a1", "a2", ...) rather than the growing "y||'b'" the sibling
// r35c_recursive_cte_limit_test.go uses: should the bound ever stop being
// published for these shapes, the recursion falls back to running out to
// cteRecursionRowCap, and at 50,000 rows a growing payload is ~1.25GB of
// string -- a ten-minute thrash instead of the eight-second decline this file
// is meant to fail with. Measured: with the old bare-column-only rule
// restored, the growing form took this package past its 600s test timeout.
const rcteInf2 = "WITH c(x,y) AS (VALUES(1,'a') UNION ALL SELECT x+1, 'a'||(x+1) FROM c) "

// rcteFin TERMINATES on its own after ten rows. Every negative case uses it:
// over it, a bound this engine must NOT publish shows up as a short answer
// rather than as a decline or a hang, which is what makes those cases gates.
const rcteFin = "WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<10) "

// TestRecursiveCTEConsumerComputedSelectList is the shape incrcorrupt.test
// needs: a non-terminating recursion whose consumer computes something per
// row. Every query here is one C SQLite answers instantly.
func TestRecursiveCTEConsumerComputedSelectList(t *testing.T) {
	edb, cdb := rcteConsumerPair(t)
	for _, q := range []string{
		rcteInf + "SELECT x*2 FROM c LIMIT 4",
		rcteInf + "SELECT x, x||'y' FROM c LIMIT 3",
		rcteInf + "SELECT 1 FROM c LIMIT 3",
		rcteInf + "SELECT -x, NOT x, x IS NULL FROM c LIMIT 3",
		rcteInf + "SELECT abs(-x), cast(x AS TEXT), x IN (1,2,3) FROM c LIMIT 4",
		rcteInf + "SELECT x BETWEEN 1 AND 3, x LIKE '1', x GLOB '2' FROM c LIMIT 4",
		rcteInf + "SELECT CASE WHEN x%2=0 THEN 'e' ELSE 'o' END FROM c LIMIT 5",
		// A 2-argument min() is SQLite's ordinary SCALAR min, not the aggregate
		// (isAggregateCall, engine/sql_agg.go), so it is per-row and bounded.
		rcteInf + "SELECT min(x,99), max(x,-1) FROM c LIMIT 3",
		// Nondeterministic per-row, so only its LENGTH is comparable -- but it
		// is the very function incrcorrupt.test writes.
		rcteInf + "SELECT length(hex(randomblob(4))) FROM c LIMIT 3",
		// OFFSET is part of the bound: limit+offset rows are pulled.
		rcteInf + "SELECT x*2 FROM c LIMIT 3 OFFSET 2",
		rcteInf + "SELECT x||'!' FROM c LIMIT 0",
		rcteInf2 + "SELECT y, length(y) FROM c LIMIT 4",
		rcteInf2 + "SELECT y||'z' FROM c AS q LIMIT 3",
		// A computed item mixed with "*", and with a qualified star.
		rcteInf2 + "SELECT *, x+100 FROM c LIMIT 3",
		rcteInf2 + "SELECT c.*, length(y) FROM c LIMIT 3",
	} {
		flCompareQuery(t, "rcteconsumer", edb, cdb, q)
	}
}

// TestRecursiveCTEConsumerAggregateOrWindowNotCapped is the half that keeps
// the widening honest. Each select list here makes the consumer read the WHOLE
// CTE despite its LIMIT, so publishing a bound for it would truncate the
// recursion and answer short. The recursion terminates on its own, so both
// engines answer and the comparison is exact.
func TestRecursiveCTEConsumerAggregateOrWindowNotCapped(t *testing.T) {
	edb, cdb := rcteConsumerPair(t)
	for _, q := range []string{
		// Aggregates: select.c:8891-8911.
		rcteFin + "SELECT count(*) FROM c LIMIT 3",
		rcteFin + "SELECT count(*) FROM c LIMIT 3 OFFSET 0",
		rcteFin + "SELECT max(x), min(x), sum(x) FROM c LIMIT 3",
		rcteFin + "SELECT total(x), avg(x) FROM c LIMIT 3",
		rcteFin + "SELECT group_concat(x) FROM c LIMIT 3",
		rcteFin + "SELECT count(*) FILTER (WHERE x>5) FROM c LIMIT 3",
		// An aggregate BURIED inside an otherwise per-row expression: the
		// whitelist has to recurse, not just look at the top node.
		rcteFin + "SELECT 1 + count(*) FROM c LIMIT 3",
		rcteFin + "SELECT CASE WHEN count(*)>3 THEN 'many' ELSE 'few' END FROM c LIMIT 3",
		rcteFin + "SELECT cast(sum(x) AS TEXT) FROM c LIMIT 3",
		rcteFin + "SELECT abs(-sum(x)) FROM c LIMIT 3",
		// Window functions: window.c:43-46.
		rcteFin + "SELECT x, sum(x) OVER () FROM c LIMIT 3",
		rcteFin + "SELECT x, count(*) OVER () FROM c LIMIT 3",
		rcteFin + "SELECT x, max(x) OVER (ORDER BY x DESC) FROM c LIMIT 3",
		rcteFin + "SELECT 1 + sum(x) OVER () FROM c LIMIT 3",
		// Subqueries, in each of their three spellings.
		rcteFin + "SELECT x, (SELECT count(*) FROM c) FROM c LIMIT 3",
		rcteFin + "SELECT x, EXISTS(SELECT 1 FROM c WHERE x=9) FROM c LIMIT 3",
		rcteFin + "SELECT x, x IN (SELECT x FROM c WHERE x>8) FROM c LIMIT 3",
	} {
		flCompareQuery(t, "rctenocap", edb, cdb, q)
	}
}

// TestRecursiveCTEConsumerInsertSelectComputed replays incrcorrupt.test 1.0
// and 2.1 verbatim -- the two corpus statements this widening closes -- and
// then compares what actually landed in the table. randomblob(600) is
// nondeterministic, so the comparison is over count/sum/length rather than the
// blob bytes, which is exactly what the .test file itself checks (its own
// assertion is a page_count).
func TestRecursiveCTEConsumerInsertSelectComputed(t *testing.T) {
	for _, autoVacuum := range []string{"2", "1"} {
		edb, cdb := rcteConsumerPair(t)
		rcteConsumerExec(t, edb, cdb,
			"PRAGMA auto_vacuum = "+autoVacuum,
			"CREATE TABLE t1(a PRIMARY KEY, b)",
			"WITH data(i) AS ( SELECT 1 UNION ALL SELECT i+1 FROM data ) "+
				"INSERT INTO t1 SELECT i, randomblob(600) FROM data LIMIT 20",
		)
		flCompareQuery(t, "rcteinsert", edb, cdb,
			"SELECT count(*), sum(a), sum(length(b)), typeof(b) FROM t1")
		flCompareQuery(t, "rcteinsert", edb, cdb, "SELECT a FROM t1 ORDER BY a")
		flCompareQuery(t, "rcteinsert", edb, cdb, "PRAGMA page_count")
	}

	// The same shape with an OFFSET, and with the recursion feeding a plain
	// INSERT ... SELECT of a computed pair.
	edb, cdb := rcteConsumerPair(t)
	rcteConsumerExec(t, edb, cdb,
		"CREATE TABLE t2(a, b)",
		rcteInf+"INSERT INTO t2 SELECT x*10, x||'-' FROM c LIMIT 4 OFFSET 3",
	)
	flCompareQuery(t, "rcteinsert", edb, cdb, "SELECT a, b FROM t2 ORDER BY a")
}
