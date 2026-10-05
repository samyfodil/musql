package compat

// Tests for general RIGHT [OUTER] JOIN and FULL [OUTER] JOIN: multi-way,
// any placement. Verifies correctness against C SQLite at two page sizes.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildRightFullJoinDB builds, at pageSize, the rf1..rf4 fixture described
// above.
func buildRightFullJoinDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("rightfull_%d.sqlite", pageSize))
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

	exec(`CREATE TABLE rf1 (id1 INTEGER PRIMARY KEY, k INTEGER, v1 TEXT)`)
	exec(`INSERT INTO rf1 VALUES (1, 1, 'a1')`)
	exec(`INSERT INTO rf1 VALUES (2, 2, 'a2')`)
	exec(`INSERT INTO rf1 VALUES (3, 2, 'a2dup')`) // duplicate k=2
	exec(`INSERT INTO rf1 VALUES (4, NULL, 'aNull')`)
	exec(`INSERT INTO rf1 VALUES (5, 9, 'a9')`) // unmatched everywhere else

	exec(`CREATE TABLE rf2 (id2 INTEGER PRIMARY KEY, k INTEGER, v2 TEXT)`)
	exec(`INSERT INTO rf2 VALUES (1, 1, 'b1')`)
	exec(`INSERT INTO rf2 VALUES (2, 2, 'b2')`)
	exec(`INSERT INTO rf2 VALUES (3, NULL, 'bNull')`)
	exec(`INSERT INTO rf2 VALUES (4, 8, 'b8')`) // unmatched elsewhere

	exec(`CREATE TABLE rf3 (id3 INTEGER PRIMARY KEY, k INTEGER, v3 TEXT)`)
	exec(`INSERT INTO rf3 VALUES (1, 1, 'c1')`)
	exec(`INSERT INTO rf3 VALUES (2, 3, 'c3')`) // unmatched in rf1/rf2
	exec(`INSERT INTO rf3 VALUES (3, NULL, 'cNull')`)
	exec(`INSERT INTO rf3 VALUES (4, 2, 'c2')`)
	exec(`INSERT INTO rf3 VALUES (5, 2, 'c2dup')`) // duplicate k=2 on BOTH sides

	exec(`CREATE TABLE rf4 (id4 INTEGER PRIMARY KEY, k INTEGER, v4 TEXT)`)
	exec(`INSERT INTO rf4 VALUES (1, 1, 'd1')`)
	exec(`INSERT INTO rf4 VALUES (2, 4, 'd4')`) // unmatched
	exec(`INSERT INTO rf4 VALUES (3, NULL, 'dNull')`)

	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	return path
}

// rightFullJoinCorpus is the matrix of statements this gate checks: every
// shape the task's own acceptance gate calls out explicitly -- a RIGHT
// mid-chain (A LEFT JOIN B RIGHT JOIN C), a RIGHT then FULL chain, a plain
// two-table FULL JOIN, RIGHT/FULL composed with USING and with NATURAL,
// RIGHT/FULL with an extra WHERE filter (including one that would be UNSAFE
// to push down early -- a filter on the null-supplying side), a 4-way
// LEFT-RIGHT-FULL chain (joinD.test's own shape), more than one RIGHT JOIN
// chained, and row-multiplicity (a match that fans out to several rows).
// Every case is ORDER BY-pinned (RIGHT/FULL's own row order is otherwise
// unspecified beyond "literal FROM order plus an appended sweep" -- see
// join.go's package doc comment -- so an explicit ORDER BY is what makes an
// ordered comparison meaningful here, exactly like vdbe_join_outer_test.go's
// single-item gate already does for its own corpus).
var rightFullJoinCorpus = []string{
	// --- two-table FULL JOIN, plain ON ---
	`SELECT rf1.v1, rf2.v2 FROM rf1 FULL JOIN rf2 ON rf1.k = rf2.k ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2`,
	`SELECT rf1.v1, rf2.v2 FROM rf1 FULL OUTER JOIN rf2 ON rf1.k = rf2.k ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2`,

	// --- RIGHT/FULL + USING / NATURAL (coalesced "k") ---
	`SELECT k, v1, v2 FROM rf1 RIGHT JOIN rf2 USING (k) ORDER BY rf2.id2, rf1.id1`,
	`SELECT k, v1, v2 FROM rf1 NATURAL RIGHT JOIN rf2 ORDER BY rf2.id2, rf1.id1`,
	`SELECT k, v1, v2 FROM rf1 FULL JOIN rf2 USING (k) ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2`,
	`SELECT k, v1, v2 FROM rf1 NATURAL FULL JOIN rf2 ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2`,

	// --- row multiplicity: k=2 matches on both sides fan out to 2x2 rows ---
	`SELECT rf1.v1, rf3.v3 FROM rf1 RIGHT JOIN rf3 ON rf1.k = rf3.k ORDER BY rf3.id3, rf1.id1`,
	`SELECT rf1.v1, rf3.v3 FROM rf1 FULL JOIN rf3 ON rf1.k = rf3.k ORDER BY coalesce(rf1.k, rf3.k), rf1.id1, rf3.id3`,

	// --- 3-way: RIGHT mid-chain -- "A LEFT JOIN B RIGHT JOIN C": the RIGHT
	// JOIN's "left side" is the WHOLE accumulated (A,B), NULL-padding both on
	// a never-matched C row, not merely B ---
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k ORDER BY rf3.id3, rf2.id2, rf1.id1`,
	// Same shape, but the ON of the RIGHT JOIN references A directly (not B) --
	// still must scope against the WHOLE accumulated (A,B) tuple, not "just A".
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf1.k = rf3.k ORDER BY rf3.id3, rf2.id2, rf1.id1`,

	// --- 3-way: RIGHT then FULL chained ---
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 RIGHT JOIN rf2 ON rf1.k = rf2.k FULL JOIN rf3 ON rf2.k = rf3.k ORDER BY coalesce(rf2.k, rf3.k), rf1.id1, rf2.id2, rf3.id3`,

	// --- 3-way: comma join then RIGHT (the pre-existing single-item shape,
	// re-verified here alongside the general matrix) ---
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1, rf2 RIGHT JOIN rf3 ON rf2.k = rf3.k ORDER BY rf3.id3, rf2.id2, rf1.id1`,

	// --- more than one RIGHT JOIN chained in the same FROM clause ---
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 RIGHT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k ORDER BY rf3.id3, rf2.id2, rf1.id1`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM rf1 RIGHT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k RIGHT JOIN rf4 ON rf3.k = rf4.k ORDER BY rf4.id4, rf3.id3, rf2.id2, rf1.id1`,

	// --- 4-way LEFT-RIGHT-FULL chain (joinD.test's own shape) ---
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM rf1 LEFT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k FULL JOIN rf4 ON rf3.k = rf4.k ORDER BY coalesce(rf3.k, rf4.k), rf1.id1, rf2.id2, rf3.id3, rf4.id4`,

	// --- WHERE after a mid-chain RIGHT: filters the fully-padded row, never
	// pushed down early (a filter on the null-supplying side -- rf1/rf2 --
	// must still see every RIGHT-guaranteed rf3 row before being applied) ---
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k WHERE rf1.v1 IS NULL ORDER BY rf3.id3, rf2.id2, rf1.id1`,
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k WHERE rf2.v2 IS NOT NULL ORDER BY rf3.id3, rf2.id2, rf1.id1`,
	// A WHERE naming only the LEFT-most (frequently NULL-padded) table: real
	// SQLite applies this AFTER padding, so a NULL-padded rf1 row is dropped
	// (NULL = 'a1' is never true), never converted into a "still matches"
	// row by evaluating the filter before padding took effect.
	`SELECT rf1.v1, rf3.v3 FROM rf1 RIGHT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k WHERE rf1.v1 = 'a1' ORDER BY rf3.id3, rf2.id2, rf1.id1`,
	// WHERE on the FULL-joined table itself -- must filter post-padding, not
	// prune the branch before FULL's own second pass gets to run.
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN rf2 ON rf1.k = rf2.k FULL JOIN rf3 ON rf2.k = rf3.k WHERE rf3.k > 1 OR rf3.k IS NULL ORDER BY coalesce(rf2.k, rf3.k), rf1.id1, rf2.id2, rf3.id3`,

	// --- SELECT * column layout: unaffected by which side is
	// guaranteed-present (RIGHT) or NULL-padded (FULL) on either side ---
	`SELECT * FROM rf1 RIGHT JOIN rf2 ON rf1.k = rf2.k ORDER BY rf2.id2, rf1.id1`,
	`SELECT * FROM rf1 FULL JOIN rf2 ON rf1.k = rf2.k ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2`,

	// --- aggregate over a general RIGHT/FULL chain ---
	`SELECT count(*) FROM rf1 LEFT JOIN rf2 ON rf1.k = rf2.k RIGHT JOIN rf3 ON rf2.k = rf3.k`,
	`SELECT count(*) FROM rf1 RIGHT JOIN rf2 ON rf1.k = rf2.k FULL JOIN rf3 ON rf2.k = rf3.k`,
}

// TestRightFullJoinTableParity is the table-driven hard gate: every corpus
// statement must produce identical, exactly-ordered results through the
// integrated engine path and C SQLite, at both page sizes 512 and 4096.
func TestRightFullJoinTableParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildRightFullJoinDB(t, pageSize)

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
			for _, sqlText := range rightFullJoinCorpus {
				total++

				eCols, eVals, eErr := p.Query(sqlText)
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

				if eErr != nil || cErr != nil {
					wrong++
					t.Errorf("[%s] unexpected error\n  engine=%v\n  cgo=%v", sqlText, eErr, cErr)
					continue
				}

				eRows := engineRowsToStrings(eVals)

				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}

				// QueryVDBE itself is bonus coverage only: a mid-chain or
				// repeated RIGHT/FULL is EXPECTED to decline here -- see
				// vdbe_join_codegen.go's vdbeRightOuterIndex.
				if vCols, vVals, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
					vRows := engineRowsToStrings(vVals)
					if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, true); !ok {
						wrong++
						t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
							sqlText, reason, vCols, vRows, cCols, cRows)
					}
				}
			}

			t.Logf("RIGHT/FULL multi-way JOIN parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("RIGHT/FULL multi-way JOIN parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}

// multiHopCoalesceCorpus is the N-WAY COALESCE gate: every case has TWO OR
// MORE separate RIGHT/FULL items in the same chain USING/NATURAL-coalescing
// the IDENTICAL column name ("k") back onto the SAME earlier representative
// table (rf1) -- e.g. "rf1 RIGHT JOIN rf2 USING(k) RIGHT JOIN rf3 USING(k)":
// rf2's own "k" is hidden (never re-entered as a candidate representative),
// so rf3's own USING(k) ALSO chooses rf1 as its representative, exactly the
// shape join.go's installCoalesceFallback used to decline outright (see git
// history/CHANGELOG: "more than one RIGHT/FULL JOIN coalescing the same
// USING/NATURAL column onto the same representative table") rather than risk
// a wrong 2-way-only COALESCE. It's now answered as the real, general N-way
// COALESCE(rf1.k, rf2.k, rf3.k, ...) in FROM order C SQLite itself
// produces -- verified directly against C SQLite (mattn/go-sqlite3)
// below, at both page sizes, for 3-table (two hops) AND 4-table (three
// hops) chains, mixing RIGHT and FULL, USING and NATURAL, and including a
// WHERE that references the bare (coalesced) column -- exactly joinB.test's
// own joinB-21/22/23 shape (chained USING(a) with more than one trailing
// RIGHT/FULL).
var multiHopCoalesceCorpus = []string{
	// --- 3-way: two RIGHT hops onto the same representative (rf1) ---
	`SELECT k, rf1.k, rf2.k, rf3.k FROM rf1 RIGHT JOIN rf2 USING (k) RIGHT JOIN rf3 USING (k) ORDER BY rf3.id3, rf2.id2, rf1.id1`,
	// --- 3-way: RIGHT then FULL, same representative ---
	`SELECT k, v1, v2, v3 FROM rf1 RIGHT JOIN rf2 USING (k) FULL JOIN rf3 USING (k) ORDER BY k NULLS FIRST, rf1.id1, rf2.id2, rf3.id3`,
	// --- 3-way: FULL then FULL, same representative ---
	`SELECT k, v1, v2, v3 FROM rf1 FULL JOIN rf2 USING (k) FULL JOIN rf3 USING (k) ORDER BY k NULLS FIRST, rf1.id1, rf2.id2, rf3.id3`,
	// --- 3-way NATURAL variant of the same shape ---
	`SELECT k, v1, v2, v3 FROM rf1 NATURAL RIGHT JOIN rf2 NATURAL FULL JOIN rf3 ORDER BY k NULLS FIRST, rf1.id1, rf2.id2, rf3.id3`,
	// --- the originally-declined 4-way shape: LEFT (ordinary), then RIGHT,
	// then FULL, all coalescing "k" back onto rf1 (three total fallback
	// owners once both RIGHT and FULL install onto rf1) ---
	`SELECT k FROM rf1 LEFT JOIN rf2 USING (k) RIGHT JOIN rf3 USING (k) FULL JOIN rf4 USING (k) ORDER BY k NULLS FIRST`,
	// --- 4-way: three hops (RIGHT, RIGHT, FULL) all onto rf1 -- exercises a
	// fallback list longer than 2 ---
	`SELECT k, v1, v2, v3, v4 FROM rf1 RIGHT JOIN rf2 USING (k) RIGHT JOIN rf3 USING (k) FULL JOIN rf4 USING (k) ORDER BY k NULLS FIRST, rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	// --- 4-way: all FULL, same representative throughout ---
	`SELECT k, v1, v2, v3, v4 FROM rf1 FULL JOIN rf2 USING (k) FULL JOIN rf3 USING (k) FULL JOIN rf4 USING (k) ORDER BY k NULLS FIRST, rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	// --- WHERE referencing the bare (coalesced) column after a multi-hop
	// chain -- must filter the LOGICAL (COALESCE'd) value, not any one raw
	// (possibly NULL) side ---
	`SELECT k, v1, v2, v3 FROM rf1 RIGHT JOIN rf2 USING (k) RIGHT JOIN rf3 USING (k) WHERE k IS NOT NULL ORDER BY k, rf1.id1, rf2.id2, rf3.id3`,
	// --- A single RIGHT/FULL hop coalescing the same column (NOT a
	// collision -- pinned alongside the multi-hop cases so a regression that
	// broke the single-hop case while "fixing" multi-hop would surface here
	// too) ---
	`SELECT k FROM rf1 LEFT JOIN rf2 USING (k) RIGHT JOIN rf3 USING (k) ORDER BY k NULLS FIRST`,
}

// TestRightFullJoinMultiHopSameUsingColumn is the correctness gate for the
// N-way COALESCE this package's RIGHT/FULL support needs: every statement in
// multiHopCoalesceCorpus must produce identical, exactly-ordered results
// through the integrated engine path against C SQLite, at both page sizes.
// QueryVDBE itself declines every one of these (more than one rightOuter item
// -- see vdbe_join_codegen.go's vdbeRightOuterIndex), and with no second
// executor to fall back to, the integrated engine path hard-errors on these
// too -- a real VDBE compiler gap this gate surfaces.
func TestRightFullJoinMultiHopSameUsingColumn(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildRightFullJoinDB(t, pageSize)

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
			for _, sqlText := range multiHopCoalesceCorpus {
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
				eCols, eVals, eErr := p.Query(sqlText)
				if cErr != nil || eErr != nil {
					wrong++
					t.Errorf("[%s] unexpected error\n  cgo=%v\n  engine=%v", sqlText, cErr, eErr)
					continue
				}

				eRows := engineRowsToStrings(eVals)
				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}

				// Bonus coverage only, same as TestRightFullJoinTableParity:
				// QueryVDBE is expected to decline every one of these (more
				// than one rightOuter item), but if it ever DOES succeed (e.g.
				// a future codegen widening), it must still match.
				if vCols, vVals, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
					vRows := engineRowsToStrings(vVals)
					if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, true); !ok {
						wrong++
						t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
							sqlText, reason, vCols, vRows, cCols, cRows)
					}
				}
			}
			if wrong != 0 {
				t.Fatalf("multi-hop same-USING-column COALESCE gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}

// TestRightFullJoinLaterQualifiedRefUsesCoalesceFallback pins a bug this
// package's RIGHT/FULL coalescing machinery used to get wrong: a LATER join
// item's own USING/NATURAL condition is desugared (join.go's
// desugarJoinItem) as a QUALIFIED equality against its representative table
// -- e.g. "t1.a = t3.a" for "... JOIN t3 USING(a)" -- purely as an
// implementation convenience, but that reference is semantically supposed
// to see the representative's COALESCED value when an EARLIER RIGHT/FULL
// JOIN in the chain already NULL-extended the representative's own raw copy
// -- exactly like a bare "a" reference already correctly does (see
// tableScope.coalesceFallback's doc comment). Before the fix
// (ColumnExpr.UsingRepr/UsingReprOwnItem, sql_ast.go), a qualified reference
// bypassed that fallback entirely, wrongly reading the representative's raw
// NULL and so wrongly failing to match rows that should -- verified
// directly against C SQLite via joinB.test (e.g. "t1 FULL JOIN t2
// USING(a) LEFT JOIN t3 USING(a) LEFT JOIN t4 USING(a) LEFT JOIN t5
// USING(a)": t3/t4's own USING match against t1's coalesced-from-t2 value
// dropped rows C SQLite keeps).
//
// This is deliberately distinct from TestRightFullJoinMultiHopSameUsingColumn
// above: that one pins the N-way COALESCE case of TWO OR MORE RIGHT/FULL
// JOINs coalescing onto the same representative; this one pins an ORDINARY
// (INNER/LEFT/NATURAL) later join reusing the column, exercising the SAME
// coalesceFallback machinery from its qualified (rather than N-way) angle.
//
// The fix also had to guard a subtler, second failure mode: applying the
// fallback to the RIGHT/FULL JOIN's OWN synthesized condition (rather than
// only a LATER item's) makes that condition self-referentially always true
// whenever the representative's raw value is NULL (COALESCE(t1.a, t2.a) =
// t2.a trivially holds whenever t1.a is NULL) -- verified directly against
// vdbe_join_outer_test.go's simpler two-table fixture ("t1 RIGHT JOIN t2
// USING (a)": t1's NULL-keyed "carol" row must match NO t2 row, not every
// one). This test's corpus exercises both directions at once: every case
// below has a RIGHT/FULL JOIN immediately followed by an ordinary join
// reusing the same USING/NATURAL column, so a regression in either
// direction (dropping a row that should match, or matching a row that
// shouldn't) would surface here.
func TestRightFullJoinLaterQualifiedRefUsesCoalesceFallback(t *testing.T) {
	corpus := []string{
		// RIGHT JOIN installs rf1's coalesce fallback (owner=rf2); the LATER
		// LEFT JOIN's own USING(k) condition against rf1 must use the
		// coalesced (rf1-or-rf2) value, not rf1's raw (possibly NULL, after
		// RIGHT's own NULL-extension) one.
		`SELECT k, v1, v2, v3 FROM rf1 RIGHT JOIN rf2 USING (k) LEFT JOIN rf3 USING (k) ORDER BY rf2.id2, rf1.id1, rf3.id3`,
		// Same shape with FULL instead of RIGHT.
		`SELECT k, v1, v2, v3 FROM rf1 FULL JOIN rf2 USING (k) LEFT JOIN rf3 USING (k) ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2, rf3.id3`,
		// The later item itself an INNER JOIN: must actually MATCH via the
		// coalesced value (not merely decide whether to NULL-pad).
		`SELECT k, v1, v2, v3 FROM rf1 FULL JOIN rf2 USING (k) INNER JOIN rf3 USING (k) ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2, rf3.id3`,
		// NATURAL variant of the same shape.
		`SELECT k, v1, v2, v3 FROM rf1 NATURAL FULL JOIN rf2 NATURAL LEFT JOIN rf3 ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2, rf3.id3`,
		// 4-way: two ordinary joins stacked after the RIGHT/FULL hop, each
		// re-referencing the same representative.
		`SELECT k, v1, v2, v3, v4 FROM rf1 FULL JOIN rf2 USING (k) LEFT JOIN rf3 USING (k) LEFT JOIN rf4 USING (k) ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	}

	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildRightFullJoinDB(t, pageSize)

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
			for _, sqlText := range corpus {
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
				if cErr != nil {
					wrong++
					t.Errorf("[%s] unexpected error\n  cgo=%v", sqlText, cErr)
					continue
				}

				// Check the integrated engine path (the VDBE).
				eCols, eVals, eErr := p.Query(sqlText)
				if eErr != nil {
					wrong++
					t.Errorf("[%s] p.Query unexpected error: %v", sqlText, eErr)
					continue
				}
				eRows := engineRowsToStrings(eVals)
				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}
			}
			if wrong != 0 {
				t.Fatalf("qualified-ref coalesce-fallback regression gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}
