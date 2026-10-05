// Exhaustive differential tests for ROW VALUES tuple comparisons, lexicographic
// under three-valued logic with special NULL handling rules.
//
// TestRowValuePerPositionCollation pins the other half of the semantics: the
// collating sequence and the affinity are chosen PER COLUMN POSITION, by the
// same rules a scalar comparison at that position would use. And
// TestRowValueUnsupportedShapesDecline holds the "never wrong" line for
// everything still not implemented -- those must be clean errors, never a
// guessed answer.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// rvGrid is the operand alphabet for the exhaustive sweeps: NULL plus one
// value from each comparison-relevant storage class, in both orders relative
// to each other so a comparison can come out either way.
var rvGrid = []string{"NULL", "0", "1", "'a'", "'b'", "2.5"}

// rvSmallGrid trims rvGrid for the 3-element/BETWEEN sweeps, whose operand
// space is 6 elements wide (4^6 = 16384 grids per operator, already).
var rvSmallGrid = []string{"NULL", "0", "1", "'a'"}

// rvHarness pairs a fresh engine DB with a fresh cgo connection and compares
// one FROM-less scalar query on both. Used for the row-value sweeps, which are
// pure expression semantics and need no table at all.
type rvHarness struct {
	t *testing.T
	p *engine.ReadOnlyPager
	c *sql.DB
}

func newRVHarness(t *testing.T) *rvHarness {
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
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	return &rvHarness{t: t, p: p, c: cdb}
}

// cmp runs q on both engines and reports a divergence. An engine ERROR is not
// a divergence: this engine is allowed to decline a shape (that is the "never
// wrong" contract), it is only ever forbidden from answering DIFFERENTLY. The
// sweeps below assert separately that the declined count is zero for the
// shapes they cover, so a silent mass-decline cannot hide behind this.
func (h *rvHarness) cmp(q string) (declined bool, diverged string) {
	h.t.Helper()
	_, ev, eerr := h.p.QueryArgs(q, nil)
	_, cr, cerr := cgoSelect(h.t, h.c, q, nil)
	if eerr != nil {
		return true, ""
	}
	if cerr != nil {
		return false, fmt.Sprintf("engine accepted what C SQLite rejects (%v)", cerr)
	}
	eRows := engineRowsToStrings(ev)
	cols := make([]string, len(cr[0]))
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
		return false, fmt.Sprintf("%s\n  engine: %v\n  cgo:    %v", reason, eRows, cr)
	}
	return false, ""
}

func TestRowValueLexParity(t *testing.T) {
	h := newRVHarness(t)
	n, declined := 0, 0
	for _, op := range []string{"<", "<=", ">", ">="} {
		for _, a := range rvGrid {
			for _, b := range rvGrid {
				for _, c := range rvGrid {
					for _, d := range rvGrid {
						n++
						q := fmt.Sprintf("SELECT (%s,%s) %s (%s,%s)", a, b, op, c, d)
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
	}
	if declined != 0 {
		t.Errorf("%d/%d 2-element lexicographic comparisons declined; all must be supported", declined, n)
	}
	t.Logf("2-element lexicographic row comparison: %d grids compared against C SQLite", n)
}

func TestRowValueLex3Parity(t *testing.T) {
	h := newRVHarness(t)
	n, declined := 0, 0
	for _, op := range []string{"<", "<=", ">", ">="} {
		for _, a := range rvSmallGrid {
			for _, b := range rvSmallGrid {
				for _, c := range rvSmallGrid {
					for _, x := range rvSmallGrid {
						for _, y := range rvSmallGrid {
							for _, z := range rvSmallGrid {
								n++
								q := fmt.Sprintf("SELECT (%s,%s,%s) %s (%s,%s,%s)", a, b, c, op, x, y, z)
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
			}
		}
	}
	if declined != 0 {
		t.Errorf("%d/%d 3-element lexicographic comparisons declined; all must be supported", declined, n)
	}
	t.Logf("3-element lexicographic row comparison: %d grids compared against C SQLite", n)
}

func TestRowValueBetweenParity(t *testing.T) {
	h := newRVHarness(t)
	n, declined := 0, 0
	for _, not := range []string{"", "NOT "} {
		for _, a := range rvSmallGrid {
			for _, b := range rvSmallGrid {
				for _, lo1 := range rvSmallGrid {
					for _, lo2 := range rvSmallGrid {
						for _, hi1 := range rvSmallGrid {
							for _, hi2 := range rvSmallGrid {
								n++
								q := fmt.Sprintf("SELECT (%s,%s) %sBETWEEN (%s,%s) AND (%s,%s)",
									a, b, not, lo1, lo2, hi1, hi2)
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
			}
		}
	}
	if declined != 0 {
		t.Errorf("%d/%d row-value BETWEENs declined; all must be supported", declined, n)
	}
	t.Logf("row-value BETWEEN: %d grids compared against C SQLite", n)
}

// rvSchema is a table set built to make a per-position collation or affinity
// mistake VISIBLE: hh.a is TEXT COLLATE nocase (so 'ABC' and 'abc' compare
// equal at position 0 but not under binary), and tt stores digit strings in a
// TEXT column and in an INTEGER column, so '10' vs '9' orders one way as text
// and the other way as a number depending on which side supplies the affinity.
var rvSchema = []string{
	`CREATE TABLE hh(a TEXT COLLATE nocase, b INT, c)`,
	`INSERT INTO hh VALUES('ABC', 1, 'x'), ('abc', 2, 'y'), ('def', 1, 'z')`,
	`CREATE TABLE tt(x TEXT, y INTEGER)`,
	`INSERT INTO tt VALUES('10', '9'), ('9', '10')`,
	`CREATE TABLE ii(p INT, q INT)`,
	`INSERT INTO ii VALUES(1,1),(1,2),(1,NULL),(2,1),(NULL,1),(NULL,NULL),(3,3)`,
}

var rvCollateQ = []string{
	// The LEFT element's collation governs its own position, exactly as in a
	// scalar comparison: hh.a is COLLATE nocase, so 'ABC' < 'abd' there.
	`SELECT c FROM hh WHERE (a,b) < ('abd',1) ORDER BY c`,
	`SELECT c FROM hh WHERE (a,b) >= ('ABC',1) ORDER BY c`,
	`SELECT c FROM hh WHERE ('ABD',1) > (a,b) ORDER BY c`,
	`SELECT c FROM hh WHERE (a,b) BETWEEN ('ABC',1) AND ('abd',9) ORDER BY c`,
	// An explicit per-element COLLATE overrides, on either side, WITHOUT
	// leaking to the other position.
	`SELECT c FROM hh WHERE (a COLLATE binary, b) >= ('ABC',1) ORDER BY c`,
	`SELECT c FROM hh WHERE (a, b) >= ('ABC' COLLATE binary,1) ORDER BY c`,
	`SELECT c FROM hh WHERE (a COLLATE binary, b) < ('abd',1) ORDER BY c`,
	// Affinity is likewise per position: TEXT x vs an integer literal applies
	// TEXT affinity, INTEGER y vs a text literal applies NUMERIC.
	`SELECT x FROM tt WHERE (x,y) < (10, 10) ORDER BY x`,
	`SELECT x FROM tt WHERE (x,y) < ('10', '10') ORDER BY x`,
	`SELECT x FROM tt WHERE (y,x) < (10, 10) ORDER BY x`,
	`SELECT x FROM tt WHERE (y,x) BETWEEN (0,'') AND (10, '99') ORDER BY x`,
	// NULLs in stored columns, filtered by every lexicographic operator: a
	// WHERE that lets a NULL through would show up as an extra row.
	`SELECT p,q FROM ii WHERE (p,q) > (1,1) ORDER BY p,q`,
	`SELECT p,q FROM ii WHERE (p,q) >= (1,1) ORDER BY p,q`,
	`SELECT p,q FROM ii WHERE (p,q) < (2,1) ORDER BY p,q`,
	`SELECT p,q FROM ii WHERE (p,q) <= (1,2) ORDER BY p,q`,
	`SELECT p,q FROM ii WHERE (p,q) BETWEEN (1,1) AND (2,2) ORDER BY p,q`,
	`SELECT p,q FROM ii WHERE (p,q) NOT BETWEEN (1,1) AND (2,2) ORDER BY p,q`,
	`SELECT p,q FROM ii WHERE NOT ((p,q) < (2,1)) ORDER BY p,q`,
	// The row-value operands need not be bare columns.
	`SELECT p,q FROM ii WHERE (p+0, abs(q)) <= (2, 1) ORDER BY p,q`,
	`SELECT p,q FROM ii WHERE (CASE WHEN p IS NULL THEN 0 ELSE p END, q) > (0,0) ORDER BY p,q`,
	// Row comparison as a select-list value, not just a WHERE predicate --
	// this is where a NULL result is actually observable rather than merely
	// filtering.
	`SELECT p, q, (p,q) < (1,2), (p,q) >= (1,2), (p,q) BETWEEN (1,1) AND (2,2) FROM ii ORDER BY rowid`,
	// Interaction with the neighbouring precedence tiers: "<" binds tighter
	// than "=", and BETWEEN's bounds parse at the relational tier.
	`SELECT (1,2) < (1,3) = 1`,
	`SELECT (1,2) BETWEEN (1,1) AND (1,3) AND 1`,
}

func TestRowValuePerPositionCollation(t *testing.T) {
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
	for _, s := range rvSchema {
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
	for _, q := range rvCollateQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			t.Errorf("[%s] engine declined a supported row-value shape: %v", q, eerr)
			continue
		}
		if cerr != nil {
			t.Errorf("[%s] engine accepted what C SQLite rejects: %v", q, cerr)
			continue
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
}

// rvWriteStmts exercise the WRITE path. The desugaring happens in the parser,
// so a row-value WHERE reaches UPDATE/DELETE (and a trigger body, and an
// INSERT ... SELECT) through exactly the same rewrite the read path uses --
// but only the read path is swept exhaustively above, and "the write path
// compiles it too" is a separate claim worth its own gate. Each statement is
// run on both engines in order; the final SELECT below must agree.
var rvWriteStmts = []string{
	`DELETE FROM ii WHERE (p,q) > (2,2)`,
	`UPDATE ii SET q = 99 WHERE (p,q) < (1,2)`,
	`UPDATE ii SET q = 77 WHERE (p,q) BETWEEN (1,2) AND (2,1)`,
	`DELETE FROM ii WHERE (p,q) NOT BETWEEN (0,0) AND (50,50)`,
	`INSERT INTO ii SELECT p+10, q FROM ii WHERE (p,q) >= (1,1)`,
}

func TestRowValueWritePathParity(t *testing.T) {
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
	for _, s := range append(append([]string{}, rvSchema...), rvWriteStmts...) {
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
	const q = `SELECT p, q FROM ii ORDER BY p, q`
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

// A ROW-VALUE "IN (SELECT ...)" used to live in the lists below, pinned as a
// decline so the engine could not guess at it. It is implemented now -- see
// engine/vdbe_codegen.go's compileInSubquery and the differential gate in
// in_subquery_test.go, which compares its three-valued NULL logic, per-column
// affinity and per-column collation against C SQLite. So is the row-value
// COMPARISON against a subquery ("(a,b) = (SELECT x,y)" and every other
// operator, either operand order) -- compileRowSubCompare/OpRowSub, gated by
// rowvalue_subquery_test.go. Only the shapes that are still genuinely
// unimplemented (an arity mismatch, a row value in any other position) remain
// pinned here.
//
// rvDeclineQ are the row-value shapes this engine does NOT implement. Real
// SQLite splits them two ways -- it rejects the misuses outright with "row
// value misused" and it ACCEPTS the subquery forms -- but this engine must
// produce an ERROR for every one of them either way. That is the "never
// wrong" contract: declining a shape C SQLite supports costs coverage,
// answering it approximately would cost correctness.
//
// The right-hand comment on each line is C SQLite 3.x's own verified
// behavior (mattn/go-sqlite3), so the split stays documented in one place.
var rvDeclineQ = []string{
	`SELECT (1,2) < (1,2,3)`,              // sqlite: error "row value misused"
	`SELECT (1,2,3) >= (1,2)`,             // sqlite: error "row value misused"
	`SELECT (1,2) < 3`,                    // sqlite: error "row value misused"
	`SELECT 3 > (1,2)`,                    // sqlite: error "row value misused"
	`SELECT (1,2)`,                        // sqlite: error "row value misused"
	`SELECT (1,2) + 1`,                    // sqlite: error "row value misused"
	`SELECT -(1,2) < (1,2)`,               // sqlite: error "row value misused"
	`SELECT (1,2) < (1,2) COLLATE nocase`, // sqlite: error "row value misused"
	`SELECT (1,2) BETWEEN (1,2,3) AND (3,4)`,
	`SELECT (1,2) BETWEEN 1 AND (3,4)`,
	`SELECT (1,2) BETWEEN (1,2) AND 4`,
	`SELECT (1,2) IN ((1,2,3))`,        // sqlite: error, "IN(...) element has 3 terms - expected 2"
	`SELECT (1,2) IN (SELECT 1)`,       // sqlite: error, "sub-select returns 1 columns - expected 2"
	`SELECT (random(),1) < (1,2)`,      // sqlite: evaluates random() ONCE; the desugaring cannot
	`SELECT (1,2) < (randomblob(4),2)`, // ditto
}

// rvDeclineTableQ repeats the same shapes against a real table, so the decline
// is exercised on the ordinary table-scan compile path and not only on the
// FROM-less one (which has its own, earlier "not compilable to bytecode"
// gate and would otherwise be the only thing this test ever reached).
var rvDeclineTableQ = []string{
	`SELECT p FROM ii WHERE (p,q) < (1,2,3)`,
	`SELECT p FROM ii WHERE (p,q,1) >= (1,2)`,
	`SELECT p FROM ii WHERE (p,q) < 3`,
	`SELECT p FROM ii WHERE 3 > (p,q)`,
	`SELECT (p,q) FROM ii`,
	`SELECT p FROM ii WHERE (p,q) + 1`,
	`SELECT p FROM ii WHERE (p,q) < (1,2) COLLATE nocase`,
	`SELECT p FROM ii WHERE (p,q) BETWEEN (1,2,3) AND (3,4)`,
	`SELECT p FROM ii WHERE (p,q) BETWEEN 1 AND (3,4)`,
	`SELECT p FROM ii WHERE (p,q) BETWEEN (1,2) AND 4`,
	`SELECT p FROM ii WHERE (p,q) IN ((1,2,3))`,
	`SELECT p FROM ii WHERE (p,q) IN (SELECT p FROM ii)`,
	`SELECT p FROM ii WHERE (random(),q) < (1,2)`,
	`SELECT p FROM ii WHERE (p,q) < (randomblob(4),2)`,
	`SELECT p FROM ii WHERE (p,q) BETWEEN (random(),0) AND (9,9)`,
	`SELECT p FROM ii WHERE (random(),q) BETWEEN (0,0) AND (9,9)`,
}

func TestRowValueUnsupportedShapesDecline(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	for _, s := range rvSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append([]string{}, rvDeclineQ...), rvDeclineTableQ...) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("[%s] PANIC: %v", q, r)
				}
			}()
			if _, _, err := p.QueryArgs(q, nil); err == nil {
				t.Errorf("[%s] accepted an unsupported row-value shape; it must decline, never guess", q)
			}
		}()
	}
}
