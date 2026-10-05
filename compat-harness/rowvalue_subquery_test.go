// This file tests row-value comparisons against multi-column subqueries.
// Such comparisons use only the subquery's first row and treat empty subqueries
// as NULL rows, not FALSE.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// rvsGrid is the operand alphabet for testing row-value comparisons.
var rvsGrid = []string{"NULL", "0", "1", "'a'", "'b'", "2.5"}

// rvsOps is every comparison operator for row values.
var rvsOps = []string{"=", "==", "!=", "<>", "IS", "IS NOT", "<", "<=", ">", ">="}

// rvsSchema is the test database schema.
var rvsSchema = []string{
	`CREATE TABLE tt(a, b)`,
	`INSERT INTO tt VALUES(1,2),(3,4)`,
	`CREATE TABLE ee(a, b)`,
	`CREATE TABLE nn(a, b)`,
	`INSERT INTO nn VALUES(1,NULL)`,
	`CREATE TABLE hh(a TEXT COLLATE NOCASE, b TEXT, c TEXT)`,
	`INSERT INTO hh VALUES('ABC','x','hit')`,
	`CREATE TABLE bb(a TEXT, b TEXT, c TEXT)`,
	`INSERT INTO bb VALUES('abc','x','miss')`,
	`CREATE TABLE hi(a TEXT COLLATE NOCASE, b TEXT, c TEXT)`,
	`INSERT INTO hi VALUES('ABC','x','hit')`,
	`CREATE INDEX hi_ab ON hi(a,b)`,
	// mm is the mirror of hh: its column a carries NO declared collation (so
	// BINARY governs unless the SUBQUERY side contributes one), and it is
	// indexed on (a,b) -- the exact shape rowvalue.test's own 6.2 pins and the
	// rowvalue-0-rowvalue-eq-subquery-collation candidate mined.
	`CREATE TABLE mm(a, b, c)`,
	`INSERT INTO mm VALUES('abc',1,'i'),('ABC',1,'ii'),('def',2,'iii'),('DEF',2,'iv'),('GHI',3,'v'),('ghi',3,'vi')`,
	`CREATE INDEX mm_ab ON mm(a, b)`,
	// side is a plain second table with no relation to mm/hh's own columns,
	// used purely to force a LEFT JOIN -- the shape that routes a WHERE-
	// position row-value/subquery conjunct through jplan.deferred instead of a
	// per-level bucket (planJoinPushdown's mentionsSubquery forceLast rule,
	// join.go), and so through compileScanAggregate/GroupBy/GroupByHash's and
	// compileScanWindow's own `deferredWhere` body rather than emitJoinLevel's
	// emitBucket.
	`CREATE TABLE side(x)`,
	`INSERT INTO side VALUES(1),(2)`,
	// gg mirrors mm but with a DECLARED NOCASE collation on its GROUP BY key --
	// anyNonBinaryCollation(gp.groupColls) forces compileScanGroupBy's SORTED
	// path rather than compileScanGroupByHash's, so the two GROUP BY dispatch
	// targets (both compileExpr(deferredWhere) sites) each get their own
	// exercised, independently-confirmed regression coverage.
	`CREATE TABLE gg(a TEXT COLLATE NOCASE, b, c)`,
	`INSERT INTO gg VALUES('abc',1,'i'),('ABC',1,'ii'),('def',2,'iii')`,
	`CREATE TABLE one(k)`,
	`INSERT INTO one VALUES(1)`,
	`CREATE TABLE ii(i INTEGER, t TEXT)`,
	`INSERT INTO ii VALUES(5,'5')`,
	`CREATE TABLE d1(c, a)`,
	`INSERT INTO d1 VALUES(1,2),(3,4)`,
	`CREATE TABLE d2(x, y)`,
	`INSERT INTO d2 VALUES(1,2),(9,9)`,
}

// rvsHarness compares queries between the engine and C SQLite.
type rvsHarness struct {
	t *testing.T
	p *engine.ReadOnlyPager
	c *sql.DB
}

func newRVSHarness(t *testing.T) *rvsHarness {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	for _, s := range rvsSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	return &rvsHarness{t: t, p: p, c: cdb}
}

// cmp runs q on both engines and reports divergences.
func (h *rvsHarness) cmp(q string) (declined bool, diverged string) {
	h.t.Helper()
	_, ev, eerr := h.p.QueryArgs(q, nil)
	cc, cr, cerr := cgoSelect(h.t, h.c, q, nil)
	if eerr != nil {
		return true, ""
	}
	if cerr != nil {
		return false, fmt.Sprintf("engine accepted what C SQLite rejects (%v)", cerr)
	}
	eRows := engineRowsToStrings(ev)
	cols := make([]string, len(cc))
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
		return false, fmt.Sprintf("%s\n  engine: %v\n  cgo:    %v", reason, eRows, cr)
	}
	return false, ""
}

// sweep tests a query template over all operand combinations.
func (h *rvsHarness) sweep(name string, tmpl func(op, a, b, x, y string) string) {
	h.t.Helper()
	n, declined := 0, 0
	for _, op := range rvsOps {
		for _, a := range rvsGrid {
			for _, b := range rvsGrid {
				for _, x := range rvsGrid {
					for _, y := range rvsGrid {
						n++
						q := tmpl(op, a, b, x, y)
						dec, why := h.cmp(q)
						if dec {
							declined++
							continue
						}
						if why != "" {
							h.t.Errorf("[%s] DIVERGES: %s", q, why)
						}
					}
				}
			}
		}
	}
	if declined != 0 {
		h.t.Errorf("%s: %d/%d declined; all must be supported", name, declined, n)
	}
	h.t.Logf("%s: %d row-value/subquery comparisons compared against C SQLite", name, n)
}

// TestRowValueSubqueryParity tests all operators against all operand grids.
func TestRowValueSubqueryParity(t *testing.T) {
	h := newRVSHarness(t)
	h.sweep("subquery on the right", func(op, a, b, x, y string) string {
		return fmt.Sprintf("SELECT (%s,%s) %s (SELECT %s,%s)", a, b, op, x, y)
	})
}

// TestRowValueSubqueryOnLeftParity tests comparisons with the subquery on the left.
func TestRowValueSubqueryOnLeftParity(t *testing.T) {
	h := newRVSHarness(t)
	h.sweep("subquery on the left", func(op, a, b, x, y string) string {
		return fmt.Sprintf("SELECT (SELECT %s,%s) %s (%s,%s)", x, y, op, a, b)
	})
}

// TestRowValueSubqueryEmptyParity verifies that empty subqueries behave as
// NULL rows, not FALSE.
func TestRowValueSubqueryEmptyParity(t *testing.T) {
	h := newRVSHarness(t)
	n, declined := 0, 0
	for _, op := range rvsOps {
		for _, a := range rvsGrid {
			for _, b := range rvsGrid {
				// Two spellings of "no rows": a FROM-less SELECT filtered away,
				// and a scan of a genuinely empty table.
				for _, sub := range []string{
					fmt.Sprintf("SELECT %s,%s WHERE 0", a, b),
					"SELECT a,b FROM ee",
				} {
					n++
					q := fmt.Sprintf("SELECT (%s,%s) %s (%s)", a, b, op, sub)
					dec, why := h.cmp(q)
					if dec {
						declined++
						continue
					}
					if why != "" {
						t.Errorf("[%s] DIVERGES: %s", q, why)
					}
				}
			}
		}
	}
	if declined != 0 {
		t.Errorf("empty subquery: %d/%d declined; all must be supported", declined, n)
	}
	t.Logf("empty subquery: %d comparisons compared against C SQLite", n)
}

// rvsTableQ are the shapes an operand grid cannot reach: the FIRST-ROW rule,
// correlation, per-column affinity and per-column collation, a row value
// inside a JOIN's WHERE and inside a derived table, and 3-element arity. Each
// is answered by both engines -- a decline here is a failure, not a pass.
var rvsTableQ = []string{
	// Rule 1: only the FIRST row counts, and ORDER BY / LIMIT / OFFSET decide
	// which row that is.
	`SELECT (1,2) = (SELECT a,b FROM tt)`,
	`SELECT (1,2) = (SELECT a,b FROM tt ORDER BY a DESC)`,
	`SELECT (3,4) = (SELECT a,b FROM tt ORDER BY a DESC)`,
	`SELECT (1,2) = (SELECT a,b FROM tt LIMIT 1 OFFSET 1)`,
	`SELECT (3,4) = (SELECT a,b FROM tt LIMIT 1 OFFSET 1)`,
	`SELECT (1,2) = (SELECT 3,4 UNION ALL SELECT 1,2)`,
	`SELECT (1,2) = (SELECT max(a),max(b) FROM tt)`,
	`SELECT (3,4) = (SELECT max(a),max(b) FROM tt)`,
	// "IN (VALUES ...)": the row-value IN whose right-hand side is written as
	// a VALUES clause rather than a SELECT. SQLite's grammar spells VALUES out
	// as a select-statement there, so it is the SUBQUERY form of IN and shares
	// this same path -- it used to be declined only because the parser read the
	// bare VALUES as a one-element value list. See parseInClause.
	`SELECT (1,2) IN (VALUES(1,2))`,
	`SELECT (1,2) IN (VALUES(3,4),(1,2))`,
	`SELECT (1,2) IN (VALUES(3,4))`,
	`SELECT (1,2) NOT IN (VALUES(1,2))`,
	`SELECT (a,b) IN (VALUES(1,2)) FROM tt`,
	// A row from a table, so the subquery side carries real column affinity
	// rather than literal affinity.
	`SELECT (1,NULL) = (SELECT a,b FROM nn)`,
	`SELECT (1,NULL) IS (SELECT a,b FROM nn)`,
	`SELECT (1,NULL) IS NOT (SELECT a,b FROM nn)`,
	`SELECT (2,2) < (SELECT a,b FROM nn)`,
	`SELECT (0,0) < (SELECT a,b FROM nn)`,
	// CORRELATED: the subquery names an outer column, so it must re-run per
	// outer row instead of being materialized once.
	`SELECT rowid FROM d1 WHERE (c,a) = (SELECT x,y FROM d2 WHERE d2.rowid=d1.rowid) ORDER BY rowid`,
	`SELECT c, (c,a) = (SELECT x,y FROM d2 WHERE d2.rowid=d1.rowid) FROM d1 ORDER BY rowid`,
	`SELECT c, (c,a) < (SELECT x,y FROM d2 WHERE x>=c) FROM d1 ORDER BY rowid`,
	// PER-COLUMN COLLATION, resolved left-operand-first, in every case where
	// the ROW-VALUE side is the one that resolves it. hh.a declares NOCASE and
	// bb.a plain BINARY over the same letters in different case, so the two
	// spellings must disagree with each other and agree with SQLite. (The
	// cases where the SUBQUERY side is what resolves it are answered too --
	// see rvsCollationFromSubqueryAnsweredQ below, including the WHERE-position
	// split "=" / "==" / "IS" carry.)
	`SELECT c FROM hh WHERE (a,b) = (SELECT 'abc','x')`,
	`SELECT c FROM hh WHERE (a,b) = (SELECT a,b FROM bb)`,
	`SELECT c FROM bb WHERE (a,b) = (SELECT a,b FROM hh)`,
	`SELECT (a,b) = (SELECT a,b FROM bb) FROM hh`,
	`SELECT (a,b) = (SELECT a,b FROM hh) FROM bb`,
	`SELECT (a,b) < (SELECT a,b FROM bb) FROM hh`,
	`SELECT (a,b) <= (SELECT a,b FROM bb) FROM hh`,
	`SELECT (a,b) IS (SELECT a,b FROM bb) FROM hh`,
	// An EXPLICIT COLLATE on a row-value element outranks both sides' declared
	// collations, so it too is resolved from the row-value side.
	`SELECT c FROM hh WHERE (a COLLATE BINARY, b) = (SELECT 'abc','x')`,
	`SELECT c FROM bb WHERE (a COLLATE NOCASE, b) = (SELECT 'ABC','x')`,
	`SELECT c FROM hh WHERE (a COLLATE BINARY, b) IS NOT (SELECT 'abc','x')`,
	`SELECT (a COLLATE BINARY, b) < (SELECT 'abc','x') FROM hh`,
	// PER-COLUMN AFFINITY: ii mixes INTEGER and TEXT, so each position coerces
	// on its own.
	`SELECT ('5','5') = (SELECT i,t FROM ii)`,
	`SELECT (5,5) = (SELECT t,i FROM ii)`,
	`SELECT ('5',5) < (SELECT i,t FROM ii)`,
	`SELECT (5,'5') >= (SELECT i,t FROM ii)`,
	`SELECT (i,t) = (SELECT '5','5') FROM ii`,
	// The NULL-safe pair coerces under the SAME per-column affinity as "=",
	// which is not obvious from "IS is NULL-safe": "(i,t) IS (SELECT '5','5')"
	// over (5,'5') is 1, and "('5',5) IS NOT (...)" is 0.
	`SELECT (i,t) IS (SELECT '5','5') FROM ii`,
	`SELECT ('5','5') IS (SELECT i,t FROM ii)`,
	`SELECT ('5',5) IS NOT (SELECT i,t FROM ii)`,
	`SELECT ('5',NULL) = (SELECT i,t FROM ii)`,
	`SELECT ('5',NULL) IS (SELECT i,t FROM ii)`,
	// Storage classes the operand grid does not carry: BLOB, and an
	// INTEGER/REAL pair that must compare numerically equal.
	`SELECT (x'0a',1) = (SELECT x'0a',1)`,
	`SELECT (x'0a',1) < (SELECT x'0b',1)`,
	`SELECT (x'0a',1) = (SELECT '0a',1)`,
	`SELECT (1,2) = (SELECT 1.0,2.0)`,
	`SELECT (1,2) IS (SELECT 1.0,2.0)`,
	// Three elements, where the lexicographic walk has a middle position to
	// stop at and a NULL can sit either before or after the deciding one.
	`SELECT (1,2,3) = (SELECT 1,2,3)`,
	`SELECT (1,2,3) < (SELECT 1,2,4)`,
	`SELECT (1,NULL,3) < (SELECT 1,2,4)`,
	`SELECT (1,2,NULL) >= (SELECT 1,2,4)`,
	// Inside a JOIN's WHERE (including the outer-join forms the corpus uses),
	// a derived table, an aggregate query, and combined with other operators.
	`SELECT tt.a FROM ee RIGHT JOIN tt ON (ee.a=tt.a) WHERE (tt.b,4)=(SELECT 3,4)`,
	`SELECT tt.a FROM ee LEFT JOIN tt ON (ee.a=tt.a) WHERE (tt.b,4) IS (SELECT 3,4)`,
	`SELECT tt.a FROM tt LEFT JOIN ee ON (ee.a=tt.a) WHERE (tt.a,tt.b)=(SELECT 1,2)`,
	`SELECT z.a FROM (SELECT tt.a FROM tt WHERE (987,tt.b)=(SELECT 987,2)) AS z`,
	`SELECT max(a) FROM tt WHERE (a,b) = (SELECT 1,2)`,
	`SELECT count(*) FROM tt WHERE (a,b) < (SELECT 3,4)`,
	`SELECT ((1,2) = (SELECT 1,2)) AND ((3,4) = (SELECT 3,4))`,
	`SELECT NOT ((1,2) = (SELECT 1,2))`,
	`SELECT a FROM tt WHERE (a,b) = (SELECT 1,2) OR a=3 ORDER BY a`,
	// Both orders in ONE statement, which is how rowvalue2.test spells it.
	`SELECT (SELECT +b,1) >= (a,1), (a,1) <= (SELECT +b,1) FROM tt ORDER BY a`,
	// Through a GROUP BY select list. This was carried in rvsDeclineQ (as
	// "sqlite: 1 then 0", i.e. expected to error) until this session, but it
	// turns out already correctly answered, unrelated to the WHERE-position
	// collation fix above: rewriteGroupExpr's own RowExpr case (sql_group.go)
	// rewrites a row value's ELEMENTS exactly like any other bare column
	// reference in the select list (each resolves against the group's key/
	// anchor row), while the SubqueryExpr side is left as its own independent
	// scope -- so compileRowSubCompare runs per output group against already
	// GROUP-BY-resolved operands, same as any other expression here.
	`SELECT (a,b) = (SELECT 1,2) FROM tt GROUP BY a`, // sqlite: 1 then 0
}

func TestRowValueSubqueryTableParity(t *testing.T) {
	h := newRVSHarness(t)
	for _, q := range append(append([]string{}, rvsTableQ...), rvsCollationFromSubqueryAnsweredQ...) {
		dec, why := h.cmp(q)
		if dec {
			// Re-run to report the actual error text.
			_, _, eerr := h.p.QueryArgs(q, nil)
			t.Errorf("[%s] engine declined a supported row-value subquery shape: %v", q, eerr)
			continue
		}
		if why != "" {
			t.Errorf("[%s] DIVERGES: %s", q, why)
		}
	}
}

// rvsWriteStmts reach the same comparison through the WRITE path -- an
// UPDATE/DELETE WHERE and an INSERT ... SELECT WHERE. The read sweeps above
// cannot cover this: a write compiles through compileWrite, and "the write
// path compiles it too" is a separate claim. Each runs on both engines in
// order and the final SELECT must agree.
var rvsWriteStmts = []string{
	`CREATE TABLE w(a, b)`,
	`INSERT INTO w VALUES(1,2),(3,4),(5,6)`,
	`DELETE FROM w WHERE (a,b) = (SELECT 5,6)`,
	`UPDATE w SET b = 99 WHERE (a,b) = (SELECT a,b FROM tt)`,
	`UPDATE w SET b = 77 WHERE (a,b) < (SELECT 3,4)`,
	`INSERT INTO w SELECT a+10, b FROM tt WHERE (a,b) >= (SELECT 1,2)`,
	`DELETE FROM w WHERE (a,b) IS NOT (SELECT a,b FROM ee)`,
}

func TestRowValueSubqueryWritePathParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range append(append([]string{}, rvsSchema...), rvsWriteStmts...) {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	const q = `SELECT a, b FROM w ORDER BY a, b`
	_, ev, eerr := p.QueryArgs(q, nil)
	cc, cr, cerr := cgoSelect(t, cdb, q, nil)
	if eerr != nil || cerr != nil {
		t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
	}
	eRows := engineRowsToStrings(ev)
	cols := make([]string, len(cc))
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
		t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
	}
}

// TestRowValueSubqueryDeleteUpdateWherePositionCollation verifies that DELETE
// and UPDATE use the target table's collation for row-value comparisons in the
// WHERE clause, not the subquery's.
func TestRowValueSubqueryDeleteUpdateWherePositionCollation(t *testing.T) {
	schema := []string{
		`CREATE TABLE hh(a TEXT COLLATE NOCASE, b TEXT, c TEXT)`,
	}
	cases := []struct {
		name  string
		write string
	}{
		{"delete", `DELETE FROM hh WHERE (a,b) = (SELECT 'abc' COLLATE BINARY,'x')`},
		{"update", `UPDATE hh SET c = 'CHANGED' WHERE (a,b) = (SELECT 'abc' COLLATE BINARY,'x')`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer edb.Close()
			cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()
			stmts := append(append([]string{}, schema...), `INSERT INTO hh VALUES('ABC','x','hit')`, tc.write)
			for _, s := range stmts {
				if err := edb.Exec(s); err != nil {
					t.Fatalf("engine %s: %v", s, err)
				}
				if _, err := cdb.Exec(s); err != nil {
					t.Fatalf("cgo %s: %v", s, err)
				}
			}
			p, err := edb.SnapshotPager()
			if err != nil {
				t.Fatal(err)
			}
			const q = `SELECT a, b, c FROM hh ORDER BY a, b, c`
			_, ev, eerr := p.QueryArgs(q, nil)
			cc, cr, cerr := cgoSelect(t, cdb, q, nil)
			if eerr != nil || cerr != nil {
				t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
			}
			eRows := engineRowsToStrings(ev)
			cols := make([]string, len(cc))
			for i := range cols {
				cols[i] = fmt.Sprintf("c%d", i)
			}
			if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
				t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", tc.write, reason, eRows, cr)
			}
		})
	}
}

// rvsDeclineQ are the row-value/subquery shapes this engine still does NOT
// implement. The right-hand comment on each line is C SQLite's own verified
// behavior, so the split between "SQLite rejects it too" and "SQLite answers it
// and we decline" stays documented in one place. Either way the engine must
// produce an ERROR: declining a shape C SQLite supports costs coverage,
// answering it approximately would cost correctness.
var rvsDeclineQ = []string{
	// ARITY MISMATCH -- C SQLite rejects these, and so must this engine.
	`SELECT (1,2) = (SELECT 1)`,             // sqlite: error "row value misused"
	`SELECT (1,2,3) = (SELECT 1,2)`,         // sqlite: error "row value misused"
	`SELECT 1 = (SELECT 1,2)`,               // sqlite: error "row value misused"
	`SELECT (SELECT 1,2) = 1`,               // sqlite: error "row value misused"
	`SELECT (1,2) = (SELECT a FROM tt)`,     // sqlite: error "row value misused"
	`SELECT (1,2) < (SELECT a,b,1 FROM tt)`, // sqlite: error "row value misused"
	// BOTH operands a subquery. SQLite accepts it; OpRowSub carries ONE row
	// source, so this needs two and stays declined rather than guessed at.
	`SELECT (SELECT 1,2) = (SELECT 1,2)`, // sqlite: 1
	`SELECT (SELECT 1,2) < (SELECT 1,3)`, // sqlite: 1
	// BETWEEN over a row value and a subquery, in either arrangement. The
	// "X>=Lo AND X<=Hi" identity writes X twice, so a subquery X would RUN
	// twice; and a subquery BOUND leaves the other half a bare row-vs-row
	// comparison the parser did not desugar. See compileBetween.
	`SELECT (1,2) BETWEEN (SELECT 1,2) AND (3,4)`,     // sqlite: 1
	`SELECT (SELECT 1,2) BETWEEN (1,2) AND (3,4)`,     // sqlite: 1
	`SELECT (SELECT 1,2) NOT BETWEEN (1,2) AND (3,4)`, // sqlite: 0
	// A row value in a CASE (as the base or as a WHEN), which SQLite also
	// accepts. That is a separate feature -- CASE compares its base against
	// each WHEN itself, so it needs row-aware CASE codegen, not this opcode.
	// A row value in a scalar position is SQLite's own "row value misused"; a
	// subquery on the other side does not make it one.
	`SELECT (1,2) + (SELECT 1,2)`,  // sqlite: error "row value misused"
	`SELECT (SELECT 1,2) || (1,2)`, // sqlite: error "row value misused"
}

// rvsCollationFromSubqueryAnsweredQ are the shapes where the SUBQUERY side is
// what resolves the comparison's collating sequence -- the row value's own
// element carries neither an explicit COLLATE nor a declared collation of
// equal rank. C SQLite answers seven of the ten comparison operators the
// SAME way everywhere (the subquery's collation governs, in a select list or
// a WHERE alike); for the remaining three -- "=", "==", "IS" -- WHERE
// POSITION decides it. One statement shows both answers at once for the
// identical expression text over the same row,
//
//	sqlite> SELECT c, (a,b) = (SELECT 'abc' COLLATE BINARY,'x') FROM hh
//	   ...>   WHERE (a,b) = (SELECT 'abc' COLLATE BINARY,'x');
//	hit|0
//
// -- the WHERE copy said TRUE (hh.a's declared NOCASE) and the select-list
// copy said 0 (the subquery's explicit BINARY). This is a real, STATICALLY
// DETERMINABLE compile-time fact, not oracle ambiguity: SQLite's WHERE
// processing decomposes a vector "="/"IS" that is a direct top-level
// AND-conjunct of the WHERE clause into per-field scalar terms (a=X AND b=Y)
// -- tag-20220128a, whereexpr.c:1467-1489, guarded by "pWC->op==TK_AND" (the
// WhereClause a query's own WHERE terms are split into by sqlite3WhereSplit;
// an OR's operands get their own pWC->op==TK_OR sub-clause instead,
// exprAnalyzeOrTerm, whereexpr.c:721, so a vector term reached only through an
// OR is never decomposed). Each per-field replacement is built by
// sqlite3ExprForVectorField (expr.c:574-621): for a subquery vector it is a
// freshly synthesized, opaque TK_SELECT_COLUMN node carrying no EP_Collate, so
// sqlite3BinaryCompareCollSeq (expr.c:424-441) falls through to the LEFT
// operand's own collation instead. Everywhere else -- a select-list
// expression, CASE, HAVING, an OR-nested WHERE term, or any of the other 7
// comparison operators even inside WHERE (tag-20220128a's guard fires only for
// TK_EQ/TK_IS) -- the general vector-comparison codegen (exprVectorRegister/
// codeVectorCompare, expr.c:659-784) reads the subquery's real result-column
// expression directly, so its actual COLLATE governs. SQLite's own
// rowvalue.test pins the WHERE side of the "=" split at test 6.2, which
// expects one row from "(a,b) = (SELECT 'abc' COLLATE nocase, 1)" over a
// table holding both 'abc' and 'ABC' -- exactly what mm/TestRowValueSubqueryTableParity
// below re-derives.
//
// See engine/vdbe_codegen.go's compiler.inWhereConjunct for the mechanism
// this engine uses to reproduce the split: a compile-time flag, true only
// while compiling a direct top-level WHERE AND-conjunct (re-armed through
// nested ANDs, mirroring sqlite3WhereSplit's recursive descent; never through
// an OR), consulted by compileRowSubCompare only for these three operators.
//
// WHICH operators split was measured, not assumed: all ten, in both operand
// orders, with the subquery contributing either a DECLARED or an EXPLICIT
// collation, with and without an index on the row value's columns -- 60
// spelling pairs, each chosen so BINARY and NOCASE give different answers.
// Exactly "=", "==" and "IS" disagreed between the select-list and WHERE
// spellings (both spellings of each below, checked against the oracle); the
// other seven agreed in every pair, all taking the subquery's collation
// regardless of position.
//
// The scalar rule, for contrast, IS consistent: a subquery's own collation
// never propagates ("'abc' = (SELECT a FROM hh)" and "'abc' = (SELECT 'ABC'
// COLLATE NOCASE)" are both 0, in both positions), which is what this engine
// already does for "x = (SELECT y)".
var rvsCollationFromSubqueryAnsweredQ = []string{
	// The three WHERE-position-dependent operators, each spelling checked
	// against its own (different) oracle answer -- the case this file used to
	// decline entirely (rowvalue-0-rowvalue-eq-subquery-collation).
	`SELECT (a,b) = (SELECT 'abc' COLLATE BINARY,'x') FROM hh`,
	`SELECT c FROM hh WHERE (a,b) = (SELECT 'abc' COLLATE BINARY,'x')`,
	`SELECT (a,b) == (SELECT 'abc' COLLATE BINARY,'x') FROM hh`,
	`SELECT (a,b) IS (SELECT 'abc' COLLATE BINARY,'x') FROM hh`,
	`SELECT ('abc','x') = (SELECT a,b FROM hh)`,
	`SELECT ('abc','x') IS (SELECT a,b FROM hh)`,
	`SELECT ('abc','x') = (SELECT 'ABC' COLLATE NOCASE,'x')`,
	`SELECT (SELECT a,b FROM hh) = (a,b) FROM bb`,
	`SELECT (SELECT a,b FROM hh) IS (a,b) FROM bb`,
	`SELECT (SELECT 'ABC' COLLATE NOCASE,'x') = ('abc','x')`,
	// The mined statement's own exact shape: mm.a carries NO declared
	// collation (unlike hh.a's NOCASE above), so it is the SUBQUERY's explicit
	// NOCASE that would govern outside WHERE -- and, per tag-20220128a, is
	// silently dropped back to BINARY inside it, over an INDEXED column pair.
	// rowvalue.test's own 6.2 pins exactly this WHERE answer (one row, 'i').
	`SELECT c FROM mm WHERE (a,b) = (SELECT 'abc' COLLATE nocase, 1)`,
	`SELECT a, c, (a,b) = (SELECT 'abc' COLLATE nocase, 1) FROM mm ORDER BY c`,
	// hh.a is 'ABC' NOCASE, bb.a is 'abc' BINARY; the subquery side carries the
	// governing collation because the row-value element has none of equal rank.
	`SELECT ('abc','x') != (SELECT a,b FROM hh)`,
	`SELECT k FROM one WHERE ('abc','x') != (SELECT a,b FROM hh)`,
	`SELECT ('abc','x') <> (SELECT a,b FROM hh)`,
	`SELECT k FROM one WHERE ('abc','x') <> (SELECT a,b FROM hh)`,
	`SELECT ('abc','x') IS NOT (SELECT a,b FROM hh)`,
	`SELECT k FROM one WHERE ('abc','x') IS NOT (SELECT a,b FROM hh)`,
	`SELECT ('abc','w') < (SELECT a,b FROM hh)`,
	`SELECT k FROM one WHERE ('abc','w') < (SELECT a,b FROM hh)`,
	`SELECT ('abc','x') <= (SELECT a,b FROM hh)`,
	`SELECT k FROM one WHERE ('abc','x') <= (SELECT a,b FROM hh)`,
	`SELECT ('abc','w') > (SELECT a,b FROM hh)`,
	`SELECT k FROM one WHERE ('abc','w') > (SELECT a,b FROM hh)`,
	`SELECT ('abc','w') >= (SELECT a,b FROM hh)`,
	`SELECT k FROM one WHERE ('abc','w') >= (SELECT a,b FROM hh)`,
	// The mirrored operand order, and an EXPLICIT COLLATE inside the subquery
	// rather than a declared one on its column.
	`SELECT (SELECT a,b FROM hh) < ('abc','w') FROM bb`,
	`SELECT c FROM bb WHERE (SELECT a,b FROM hh) < ('abc','w')`,
	`SELECT (SELECT a,b FROM hh) >= ('abc','w')`,
	`SELECT k FROM one WHERE (SELECT a,b FROM hh) >= ('abc','w')`,
	`SELECT (a,b) != (SELECT 'abc' COLLATE BINARY,'x') FROM hh`,
	`SELECT c FROM hh WHERE (a,b) != (SELECT 'abc' COLLATE BINARY,'x')`,
	`SELECT (a,b) < (SELECT 'abc' COLLATE BINARY,'x') FROM hh`,
	`SELECT c FROM hh WHERE (a,b) < (SELECT 'abc' COLLATE BINARY,'x')`,
	`SELECT (a,b) >= (SELECT 'abc' COLLATE BINARY,'x') FROM hh`,
	`SELECT c FROM hh WHERE (a,b) >= (SELECT 'abc' COLLATE BINARY,'x')`,
	// And with an INDEX over the row value's columns, which is what would put
	// SQLite's WHERE processing on its index-search path if anything could.
	`SELECT c FROM hi WHERE (a,b) < (SELECT 'abc' COLLATE BINARY,'x')`,
	`SELECT c FROM hi WHERE (a,b) != (SELECT 'abc' COLLATE BINARY,'x')`,
	`SELECT c FROM hi WHERE ('abc','w') >= (SELECT a,b FROM hi)`,
	// The five sites a 2nd adversarial review round found this same WHERE-
	// position split still missing from, beyond the three (compileScanPlain/
	// compileScanSorted/emitJoinLevel's own emitBucket) the original fix armed
	// -- plus two more this session's own further sweep of every direct
	// compileExpr(WHERE-equivalent) call site in engine/*.go turned up. Every
	// one below is confirmed by DIRECT comparison against mattn/go-sqlite3
	// (not just declined-then-guessed): each was wrong before the site's own
	// c.inWhereConjunct = true was added, and matches now.
	//
	// compileScanAggregate/compileScanGroupBy/compileScanGroupByHash: a LEFT
	// JOIN forces this WHERE conjunct into jplan.deferred (planJoinPushdown's
	// mentionsSubquery forceLast rule, join.go) rather than a per-level bucket,
	// which is what let it bypass emitJoinLevel's already-patched emitBucket.
	// mm.a carries no declared collation, so BINARY governs; C SQLite
	// matches only the 'abc' row here (1), and this engine wrongly matched
	// 'ABC' too (2) via the subquery's own NOCASE before this.
	`SELECT count(*) FROM mm LEFT JOIN side ON side.x = mm.b WHERE (mm.a,mm.b) = (SELECT 'abc' COLLATE nocase, 1)`,
	// The two GROUP BY dispatch targets specifically (an aggregate with no
	// GROUP BY at all, like the count(*) case just above, never reaches
	// either): gg.a's declared NOCASE collation forces compileScanGroupBy's
	// SORTED path (anyNonBinaryCollation(gp.groupColls)), while mm.a's
	// undeclared collation qualifies for compileScanGroupByHash's O(n) path.
	// Confirmed independently for each: mutating only the sorted site's guard
	// changes gg's own answer from 2 (C SQLite: NOCASE governs in WHERE
	// position, both rows group together) to 1; mutating only the hash site's
	// splits mm's single 'abc' group into two ('abc' and 'ABC').
	`SELECT gg.a, count(*) FROM gg LEFT JOIN side ON side.x = gg.b WHERE (gg.a,gg.b) = (SELECT 'abc' COLLATE BINARY, 1) GROUP BY gg.a`,
	`SELECT mm.a, count(*) FROM mm LEFT JOIN side ON side.x = mm.b WHERE (mm.a,mm.b) = (SELECT 'abc' COLLATE nocase, 1) GROUP BY mm.a`,
	// compileScanWindow: the same deferred-vs-bucket split, with a window call
	// in the select list so the query dispatches to compileScanWindow instead
	// of compileScanAggregate.
	`SELECT mm.c, row_number() OVER (ORDER BY mm.c) FROM mm LEFT JOIN side ON side.x = mm.b WHERE (mm.a,mm.b) = (SELECT 'abc' COLLATE nocase, 1)`,
	// compileSelectNoFromTrig / compileNoFromAggregate: a FROM-less WHERE is
	// not exempt from tag-20220128a in C SQLite -- where.c:6941 splits
	// pWhere into the WhereClause BEFORE where.c:6945's "no FROM" special
	// case, and sqlite3WhereExprAnalyze (where.c:6990, which performs the
	// actual rewrite) runs unconditionally either way. hh has no bearing here
	// (this is a FROM-less query); the literal's own affinity is BINARY.
	`SELECT 1 WHERE ('abc','x') = (SELECT 'ABC' COLLATE nocase, 'x')`,
	`SELECT count(*) WHERE ('abc','x') = (SELECT 'ABC' COLLATE nocase, 'x')`,
	// emitJoinLevel's compileOn (the s.on path for a genuine, non-onToWhere-
	// promoted OUTER join's own ON clause): select.c:655-661 ANDs EVERY join's
	// ON into p->pWhere in C SQLite, EP_OuterON exactly like EP_InnerON,
	// before sqlite3WhereBegin ever runs -- so an outer join's ON is just as
	// much a top-level WHERE AND-conjunct as an ordinary WHERE term. mm.a has
	// no declared collation, so only the literal-cased 'abc' row matches in
	// C SQLite (2 output rows: side has 2 rows, both paired with the one
	// matching mm row), and every other mm row is a LEFT JOIN miss (1 row
	// each, NULL-extended) -- 7 rows total, not 8.
	`SELECT mm.c FROM mm LEFT JOIN side ON (mm.a,mm.b) = (SELECT 'abc' COLLATE nocase, 1)`,
}

func TestRowValueSubqueryUnsupportedShapesDecline(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	for _, s := range rvsSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range rvsDeclineQ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("[%s] PANIC: %v", q, r)
				}
			}()
			if _, _, err := p.QueryArgs(q, nil); err == nil {
				t.Errorf("[%s] accepted an unsupported row-value subquery shape; it must decline, never guess", q)
			}
		}()
	}
}
