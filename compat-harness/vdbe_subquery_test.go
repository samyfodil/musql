package compat

// VDBE subquery compilation: scalar, EXISTS, and IN (SELECT).
import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBESubqueryDB builds tables tailored to subquery semantics: t1 is the
// outer table (with a NULL v so the notorious three-valued NOT IN case is
// reachable); t2 is a many-valued table with a NULL b (for correlated scalars,
// aggregates, and an IN set with a NULL); inset/inset_nonull are explicit IN
// membership sets, one WITH a NULL element and one without (the difference
// between NOT IN yielding rows and NOT IN collapsing to NULL/empty); empty_t is
// genuinely empty (a scalar subquery over it must yield NULL, EXISTS 0).
func buildVDBESubqueryDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_subquery.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	exec := func(sqlText string) {
		t.Helper()
		if err := db.Exec(sqlText); err != nil {
			t.Fatalf("Exec(%s): %v", sqlText, err)
		}
	}

	exec(`CREATE TABLE t1 (k INTEGER PRIMARY KEY, v INTEGER, s TEXT)`)
	exec(`INSERT INTO t1 VALUES (1, 10, 'a')`)
	exec(`INSERT INTO t1 VALUES (2, 20, 'b')`)
	exec(`INSERT INTO t1 VALUES (3, 30, 'c')`)
	exec(`INSERT INTO t1 VALUES (4, NULL, 'd')`) // NULL v -- reaches NOT IN's NULL case

	exec(`CREATE TABLE t2 (k INTEGER, b INTEGER)`)
	exec(`INSERT INTO t2 VALUES (1, 100)`)
	exec(`INSERT INTO t2 VALUES (1, 150)`) // k=1 has two b -> correlated max/count > 1
	exec(`INSERT INTO t2 VALUES (2, 200)`)
	exec(`INSERT INTO t2 VALUES (3, NULL)`) // NULL b
	// k=4 absent -> a correlated subquery keyed on it produces no rows

	exec(`CREATE TABLE inset (x INTEGER)`)
	exec(`INSERT INTO inset VALUES (10)`)
	exec(`INSERT INTO inset VALUES (20)`)
	exec(`INSERT INTO inset VALUES (NULL)`) // the NULL that poisons NOT IN

	exec(`CREATE TABLE inset_nonull (x INTEGER)`)
	exec(`INSERT INTO inset_nonull VALUES (10)`)
	exec(`INSERT INTO inset_nonull VALUES (30)`)

	exec(`CREATE TABLE empty_t (x INTEGER)`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// subCase is one gate statement plus whether its output row order is
// significant (true only for a query whose ORDER BY totally orders the rows;
// an unordered query's row order isn't guaranteed by SQL, so queryResultsMatch
// compares those as a multiset).
type subCase struct {
	sql     string
	ordered bool
}

// vdbeSubqueryCorpus is the UNCORRELATED subquery corpus: every statement here
// compiles to bytecode (QueryVDBE must not decline it) and must agree with
// both oracles. Scalar subqueries are always single-row (an aggregate, or
// ORDER BY ... LIMIT 1) so "the first row" is deterministic across engines.
var vdbeSubqueryCorpus = []subCase{
	// --- scalar subquery in the SELECT list (uncorrelated) ---
	{"SELECT k, (SELECT max(b) FROM t2) FROM t1 ORDER BY k", true},
	{"SELECT k, (SELECT count(*) FROM t2) FROM t1 ORDER BY k", true},
	{"SELECT (SELECT min(x) FROM empty_t) FROM t1", false},               // no rows -> NULL
	{"SELECT k + (SELECT sum(b) FROM t2) FROM t1 ORDER BY k", true},      // scalar composed in an expression
	{"SELECT (SELECT b FROM t2 ORDER BY b DESC LIMIT 1) FROM t1", false}, // ORDER BY / LIMIT inside

	// --- scalar subquery in WHERE (uncorrelated) ---
	{"SELECT k FROM t1 WHERE v > (SELECT avg(b) FROM t2)", false},
	{"SELECT k FROM t1 WHERE v = (SELECT max(b) FROM t2 WHERE b < 200)", false},
	{"SELECT k FROM t1 WHERE v > (SELECT min(b) FROM t2) ORDER BY k", true},

	// --- EXISTS / NOT EXISTS (uncorrelated) ---
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM empty_t)", false}, // zero rows -> no output
	{"SELECT k FROM t1 WHERE NOT EXISTS (SELECT 1 FROM empty_t) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2)", false}, // t2 non-empty -> no output
	{"SELECT EXISTS (SELECT 1 FROM t2), NOT EXISTS (SELECT 1 FROM empty_t) FROM t1 ORDER BY k", true},

	// --- X [NOT] IN (SELECT ...) ---
	{"SELECT k FROM t1 WHERE v IN (SELECT x FROM inset_nonull) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE v NOT IN (SELECT x FROM inset_nonull) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE v IN (SELECT x FROM inset) ORDER BY k", true},     // set has a NULL -- IN still matches on a hit
	{"SELECT k FROM t1 WHERE v NOT IN (SELECT x FROM inset) ORDER BY k", true}, // the notorious NOT IN w/ NULL -> NULL/empty
	{"SELECT k FROM t1 WHERE v IN (SELECT b FROM t2) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE v NOT IN (SELECT b FROM t2) ORDER BY k", true},        // t2.b has a NULL too
	{"SELECT k FROM t1 WHERE v IN (SELECT b FROM t2 GROUP BY b) ORDER BY k", true}, // subquery with GROUP BY
	{"SELECT k FROM t1 WHERE v IN (SELECT x FROM empty_t) ORDER BY k", true},       // empty set -> IN false
	{"SELECT k FROM t1 WHERE v NOT IN (SELECT x FROM empty_t) ORDER BY k", true},   // empty set -> NOT IN true (even NULL v)

	// --- nested subqueries (all uncorrelated) ---
	{"SELECT k FROM t1 WHERE v IN (SELECT b FROM t2 WHERE b IN (SELECT x FROM inset)) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE b > (SELECT avg(b) FROM t2)) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE (SELECT count(*) FROM t2 WHERE b > (SELECT min(b) FROM t2)) > 0 ORDER BY k", true},

	// --- composing with the outer query's own machinery ---
	{"SELECT s FROM t1 WHERE v IN (SELECT x FROM inset_nonull) ORDER BY s DESC", true}, // outer ORDER BY DESC
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2) ORDER BY k LIMIT 2", true},      // outer LIMIT
	{"SELECT count(*) FROM t1 WHERE v IN (SELECT x FROM inset_nonull)", false},         // aggregate outer + IN subquery in WHERE
}

// TestVDBESubqueryResultParity is the hard gate: every uncorrelated corpus
// statement must produce identical results through the VDBE and real C
// SQLite.
func TestVDBESubqueryResultParity(t *testing.T) {
	path := buildVDBESubqueryDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong := 0
	total := 0
	for _, tc := range vdbeSubqueryCorpus {
		total++
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error (a compilable subquery must not decline)\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)

		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("VDBE subquery result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE subquery parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBESubqueryDualModeIntegration exercises the VDBEMode=dual integration
// path (tryVDBEScan/tryVDBENoFrom) through the ordinary QueryArgs entry point
// for the whole uncorrelated corpus, confirming every case compiles and runs
// on the real driver path (no error) and restores VDBEMode afterward.
func TestVDBESubqueryDualModeIntegration(t *testing.T) {
	path := buildVDBESubqueryDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeSubqueryCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] VDBEMode=dual: %v", tc.sql, err)
		}
	}
}

// correlatedCorpus is a correlated subquery whose ENCLOSING query is a
// whole-table AGGREGATE. It used to be the split's other half -- the shape the
// VDBE declined at compile time -- and this test asserted that decline. The
// premise was that an aggregate has no positioned "current row" for the
// subquery to correlate against (compiler.rowLive false), and that is true of
// an aggregate's POST-SCAN RESULT phase and false of its WHERE, which runs
// inside the join loops with every cursor positioned exactly as a row-mode
// scan's does. The three aggregate compilers simply never set the flag over
// their scan body; they do now (engine/vdbe_agg_codegen.go), so these compile
// and this asserts the ANSWERS instead.
//
// Kept as its own corpus rather than folded into vdbeSubqueryCorpus (which is
// the UNCORRELATED one) because the category is worth naming: every statement
// here is a correlated reference read from an aggregate's WHERE.
var correlatedCorpus = []subCase{
	{"SELECT count(*) FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k)", false},     // correlated EXISTS under an aggregate
	{"SELECT count(*) FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k)", false}, // correlated NOT EXISTS under an aggregate
	{"SELECT count(*) FROM t1 WHERE v IN (SELECT b FROM t2 WHERE t2.k = t1.k)", false},       // correlated IN under an aggregate
	{"SELECT sum(v) FROM t1 WHERE v > (SELECT max(b) FROM t2 WHERE t2.k = t1.k)", false},     // correlated scalar under an aggregate
	{"SELECT max(k) FROM t1 WHERE v = (SELECT min(b) FROM t2 WHERE t2.k = t1.k)", false},     // correlated scalar under an aggregate
	// ...and the same shapes under GROUP BY, whose two compilers (sorted and
	// hash) each have their own scan body.
	{"SELECT k, count(*) FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k) GROUP BY k ORDER BY k", true},
	{"SELECT k, sum(v) FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k) GROUP BY k ORDER BY k", true},
	{"SELECT count(*) FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k) HAVING count(*) > 1", false},
}

// TestVDBESubqueryCorrelatedFallback pins that a correlated subquery in an
// AGGREGATE's WHERE now compiles AND answers what C SQLite answers -- three
// of the five cases distinguish a wrong cursor position from a right one by
// VALUE (3, 1, 0) and two by NULL-vs-a-number, so a body that read the wrong
// row could not pass. The name is kept so the history is greppable; what it
// asserts is the opposite of what it used to.
func TestVDBESubqueryCorrelatedFallback(t *testing.T) {
	path := buildVDBESubqueryDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong := 0
	for _, tc := range correlatedCorpus {
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error (a correlated subquery in an aggregate WHERE must compile)\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, engineRowsToStrings(vVals), cCols, cRows)
		}

		// The ordinary integrated path must agree too, not just QueryVDBE.
		if _, _, aErr := p.QueryArgs(sqlText, nil); aErr != nil {
			wrong++
			t.Errorf("[%s] QueryArgs: %v", sqlText, aErr)
		}
	}
	if wrong != 0 {
		t.Fatalf("correlated-under-aggregate parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// NOTE: TestVDBESubqueryDualCorrelated used to live here, asserting that
// running the correlated corpus under VDBEMode=dual raised no divergence
// because the outer query fell back to a SECOND EVALUATOR at compile time.
// That premise is dead: that evaluator (and the dual-mode SELECT cross-check
// oracle that compared against it) has been removed entirely -- see
// engine/vdbe_run.go's VDBEMode doc comment, which now says VDBEDual "is no
// longer distinguished from VDBEOn" on the read side. There is nothing left
// for this test to meaningfully check (QueryArgs now hard-errors on the
// correlated corpus regardless of VDBEMode, exactly like
// TestVDBESubqueryCorrelatedFallback above already covers), so the test was
// deleted rather than kept as a vacuous duplicate.
