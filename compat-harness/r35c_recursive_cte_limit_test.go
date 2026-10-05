// Recursive CTE with LIMIT: outer bound stops the recursion.
//
//	SELECT * FROM (SELECT x FROM c LIMIT 3) ORDER BY 1 DESC   -- hangs
//	SELECT a FROM t WHERE a IN (SELECT x FROM c LIMIT 5)      -- hangs
//
// The first is the query flattener moving the subquery's LIMIT out past the
// ORDER BY (flattenSubquery, select.c:4695), which makes the recursion
// unbounded again. Those two are deliberately absent below: an oracle that
// cannot answer is not an oracle.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

func r35cPair(t *testing.T) (*engine.Session, *sql.DB) {
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

// r35cExec runs each statement on both engines and fails on any accept/reject
// disagreement -- the only way an INSERT-from-a-recursive-CTE can be checked at
// all, since a decline there produces no rows to compare.
func r35cExec(t *testing.T, edb *engine.Session, cdb *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("accept/reject disagrees: %s\n  engine: %v\n  cgo:    %v", s, eErrOrNil(eerr), cerr)
		}
	}
}

const (
	r35cInf  = "WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c) "
	r35cInf2 = "WITH RECURSIVE c(x,y) AS (VALUES(1,'a') UNION ALL SELECT x+1, y||'b' FROM c) "
	r35cInfU = "WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c) "
	// A recursion that DOES terminate, for the two-reference cases: a bound
	// leaking onto the shared binding shows up as a truncated second read.
	r35cFin = "WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<10) "
)

// TestR35cRecursiveCTEOuterLimit is the whole point: a recursion with no bound
// of its own, terminated by what consumes it. Every query here is one real
// SQLite answers instantly.
func TestR35cRecursiveCTEOuterLimit(t *testing.T) {
	edb, cdb := r35cPair(t)
	r35cExec(t, edb, cdb,
		"CREATE TABLE t(a)",
		"INSERT INTO t VALUES(1),(2),(3)",
		"CREATE TABLE link(aa,bb)",
		"INSERT INTO link VALUES(1,2),(2,3),(3,1),(3,4)",
	)
	for _, q := range []string{
		// The top-level consumer.
		r35cInf + "SELECT x FROM c LIMIT 3",
		r35cInf + "SELECT x FROM c LIMIT 0",
		r35cInf + "SELECT x FROM c LIMIT 1 OFFSET 4",
		r35cInf + "SELECT x FROM c LIMIT 3 OFFSET 0",
		r35cInf + "SELECT * FROM c LIMIT 4",
		r35cInf + "SELECT x FROM c AS q LIMIT 3",
		r35cInf + "SELECT q.x FROM c AS q LIMIT 3",
		r35cInf2 + "SELECT x,y FROM c LIMIT 4",
		r35cInf2 + "SELECT * FROM c LIMIT 4 OFFSET 2",
		r35cInfU + "SELECT x FROM c LIMIT 5",
		// The consumer NESTED where execSelect never sees it: a derived table,
		// a scalar subquery, an EXISTS, a correlated select-list subquery, and
		// a join arm. These are the shapes the compile-time publish added.
		r35cInf + "SELECT * FROM (SELECT x FROM c LIMIT 3)",
		r35cInfU + "SELECT * FROM (SELECT x FROM c LIMIT 5)",
		r35cInf + "SELECT (SELECT x FROM c LIMIT 1)",
		r35cInf + "SELECT (SELECT x FROM c LIMIT 1 OFFSET 6)",
		r35cInf + "SELECT EXISTS(SELECT x FROM c LIMIT 1)",
		r35cInf + "SELECT a, (SELECT x FROM c LIMIT 1 OFFSET 2) FROM t",
		r35cInf + "SELECT * FROM t JOIN (SELECT x FROM c LIMIT 2) ORDER BY 1,2",
		r35cInf + "SELECT * FROM t, (SELECT x FROM c LIMIT 2) ORDER BY 1,2",
		// A CTE-internal LIMIT and a consumer LIMIT together: the tighter wins,
		// in both directions.
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c LIMIT 7) SELECT x FROM c LIMIT 3",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c LIMIT 3) SELECT x FROM c LIMIT 7",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c LIMIT 5 OFFSET 2) SELECT x FROM c LIMIT 2 OFFSET 1",
		// An ORDER BY inside the CTE picks the recursion's QUEUE DISCIPLINE, so
		// the consumer's LIMIT truncates a different sequence.
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c ORDER BY 1 DESC) SELECT x FROM c LIMIT 4",
		// A cyclic graph walk that terminates only because UNION dedups.
		"WITH RECURSIVE w(x) AS (VALUES(1) UNION SELECT bb FROM link JOIN w ON aa=x) SELECT x FROM w LIMIT 2",
	} {
		flCompareQuery(t, "r35couter", edb, cdb, q)
	}
}

// TestR35cRecursiveCTENoPointlessMaterialization guards the OTHER half of the
// eager-evaluation gap: resolveFrom (engine/join.go) is a SCHEMA resolver --
// every caller uses only buildScopes(jts) -- yet its CTE branch used to run the
// whole fixed-point loop to get column names. For a derived table over a
// non-terminating recursion that meant FIVE separate runs to
// cteRecursionRowCap's 50,000 rows, one per select-list metadata helper.
//
// Measured directly on this box: 15.2s before, ~1ms after, against 1ms for the
// cgo oracle. The bound below is ~8000x the fixed cost and half the broken one,
// so it separates the two without being a wall-clock race. If this fires,
// something reintroduced row materialization on a schema-only path -- do not
// raise the bound.
func TestR35cRecursiveCTENoPointlessMaterialization(t *testing.T) {
	edb, cdb := r35cPair(t)
	r35cExec(t, edb, cdb, "CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2),(3)")
	for _, q := range []string{
		r35cInf + "SELECT * FROM (SELECT x FROM c LIMIT 3)",
		r35cInf + "SELECT * FROM t, (SELECT x FROM c LIMIT 2) ORDER BY 1,2",
	} {
		start := time.Now()
		flCompareQuery(t, "r35cnomat", edb, cdb, q)
		if d := time.Since(start); d > 8*time.Second {
			t.Errorf("%s took %s -- a schema-only resolution is materializing the recursion again (engine/join.go's CTE branch of resolveFrom must use cteSchemaCols)", q, d.Round(time.Millisecond))
		}
	}
}

// TestR35cRecursiveCTETwoReferences is the case that decides WHERE the
// consumer's bound is allowed to live. Each statement reads the same recursive
// CTE twice under different bounds; a bound published onto the shared
// *cteBinding truncates whichever read comes second.
func TestR35cRecursiveCTETwoReferences(t *testing.T) {
	edb, cdb := r35cPair(t)
	r35cExec(t, edb, cdb, "CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2),(3)")
	for _, q := range []string{
		r35cFin + "SELECT (SELECT x FROM c LIMIT 1), (SELECT count(*) FROM c)",
		r35cFin + "SELECT (SELECT count(*) FROM c), (SELECT x FROM c LIMIT 1)",
		r35cFin + "SELECT (SELECT x FROM c LIMIT 1), (SELECT max(x) FROM c)",
		r35cFin + "SELECT (SELECT x FROM c LIMIT 2), (SELECT x FROM c LIMIT 5 OFFSET 3)",
		r35cFin + "SELECT * FROM (SELECT x FROM c LIMIT 2) JOIN (SELECT count(*) AS n FROM c)",
		r35cFin + "SELECT * FROM (SELECT x FROM c LIMIT 2), c ORDER BY 1,2",
		// The unbounded readouts on their own: a bound must never reach these.
		r35cFin + "SELECT count(*) FROM c",
		r35cFin + "SELECT x FROM c LIMIT 30",
		r35cFin + "SELECT x FROM c LIMIT 30 OFFSET 8",
		// Shapes recursiveCTEOuterCap deliberately does NOT bound (a computed
		// select-list item, a WHERE, an ORDER BY): they must still be right.
		r35cFin + "SELECT x*2 FROM c LIMIT 3",
		r35cFin + "SELECT x FROM c WHERE x>4 LIMIT 2",
		r35cFin + "SELECT x FROM c ORDER BY x DESC LIMIT 2",
		// A plain (non-recursive) CTE under the same shapes -- the control.
		"WITH p(x) AS (SELECT a FROM t) SELECT x FROM p LIMIT 2",
		"WITH p(x) AS (SELECT a FROM t) SELECT * FROM (SELECT x FROM p LIMIT 2)",
		"WITH p(x) AS (SELECT a FROM t) SELECT (SELECT x FROM p LIMIT 1), (SELECT count(*) FROM p)",
	} {
		flCompareQuery(t, "r35ctworef", edb, cdb, q)
	}
}

// TestR35cRecursiveCTEBoundedLimit is closure01.test's own statement. Its
// recursion never terminates on its own either, but it carries its OWN
// LIMIT 131072 -- a bound far above the engine's internal runaway cap, which
// used to fire first and decline a recursion C SQLite answers. A bounded
// loop needs no runaway cap at all: it pops one queue row per round and stops
// after offset+limit of them.
func TestR35cRecursiveCTEBoundedLimit(t *testing.T) {
	edb, cdb := r35cPair(t)
	r35cExec(t, edb, cdb,
		"CREATE TABLE t1(x INTEGER PRIMARY KEY, y)",
		"WITH RECURSIVE cnt(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM cnt LIMIT 131072) INSERT INTO t1(x, y) SELECT i, nullif(i,1)/2 FROM cnt",
		"CREATE TABLE t2(x)",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c) INSERT INTO t2 SELECT x FROM c LIMIT 5",
		"CREATE TABLE t3(x)",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c LIMIT 4) INSERT INTO t3 SELECT x FROM c",
	)
	flCompareQuery(t, "r35cbounded", edb, cdb, "SELECT count(*), sum(x), sum(y) FROM t1")
	flCompareQuery(t, "r35cbounded", edb, cdb, "SELECT count(*), group_concat(x) FROM t2")
	flCompareQuery(t, "r35cbounded", edb, cdb, "SELECT count(*), group_concat(x) FROM t3")
}

// TestR35cRecursiveCTELimitSweep varies the readout across every (connector,
// seed, LIMIT, OFFSET) combination -- LIMIT 0 and a duplicate seed under UNION
// are the two corners a single hand-written case keeps missing.
func TestR35cRecursiveCTELimitSweep(t *testing.T) {
	edb, cdb := r35cPair(t)
	for _, conn := range []string{"UNION ALL", "UNION"} {
		for _, body := range []string{
			"VALUES(1) %s SELECT x+1 FROM c",
			"VALUES(1),(1),(2) %s SELECT x+1 FROM c",
			"SELECT 1 %s SELECT x+2 FROM c",
		} {
			for _, lim := range []int{0, 1, 2, 5, 13} {
				for _, off := range []int{-1, 0, 1, 4} {
					q := fmt.Sprintf("WITH RECURSIVE c(x) AS (%s) SELECT x FROM c LIMIT %d",
						fmt.Sprintf(body, conn), lim)
					if off >= 0 {
						q += fmt.Sprintf(" OFFSET %d", off)
					}
					flCompareQuery(t, "r35csweep", edb, cdb, q)
					flCompareQuery(t, "r35csweep", edb, cdb, "SELECT * FROM ("+q+")")
				}
			}
		}
	}
}
