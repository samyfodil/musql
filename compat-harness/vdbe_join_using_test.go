package compat

// This file gates JOIN ... USING and NATURAL JOIN, verifying that common columns
// are coalesced correctly so they appear exactly once in "SELECT *" output.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEJoinUsingDB builds the tables the USING/NATURAL gate needs:
//
//   - j1/j2/j3 share BOTH an "id" INTEGER PRIMARY KEY (each table's own,
//     unrelated to the others') AND an "a" INTEGER join-key column --
//     deliberately two common columns, so a NATURAL join between them
//     exercises a MULTI-column common-column condition (id AND a both), while
//     an explicit "USING(a)" isolates the single-column case (id stays a
//     visible, un-coalesced duplicate on both sides, proving coalescing only
//     hides the NAMED column, never every same-named one). "a" carries
//     duplicate values (fan-out), a NULL (must never match), and unmatched
//     values on both sides (LEFT JOIN NULL-extension coverage).
//   - empty_ju is a genuinely empty table sharing (id, a) with j1 -- LEFT
//     JOIN/NATURAL LEFT JOIN against zero rows.
//   - disjoint_l/disjoint_r share NO column name at all -- NATURAL JOIN
//     between them must degenerate to a true, condition-less cross join
//     (SQLite's own documented fallback for zero common columns).
func buildVDBEJoinUsingDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_join_using.sqlite"
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

	exec(`CREATE TABLE j1 (id INTEGER PRIMARY KEY, a INTEGER, name TEXT)`)
	exec(`INSERT INTO j1 VALUES (1, 10, 'alice')`)
	exec(`INSERT INTO j1 VALUES (2, 20, 'bob')`)
	exec(`INSERT INTO j1 VALUES (3, NULL, 'carol')`) // NULL join key -- never matches
	exec(`INSERT INTO j1 VALUES (4, 20, 'dave')`)    // duplicate a=20 -> fan-out
	exec(`INSERT INTO j1 VALUES (5, 30, 'eve')`)     // a=30 -- unmatched in j2

	exec(`CREATE TABLE j2 (id INTEGER PRIMARY KEY, a INTEGER, val TEXT)`)
	exec(`INSERT INTO j2 VALUES (1, 10, 'x')`)
	exec(`INSERT INTO j2 VALUES (2, 20, 'y')`)
	exec(`INSERT INTO j2 VALUES (3, 20, 'z')`)   // duplicate a=20 -> fan-out
	exec(`INSERT INTO j2 VALUES (4, NULL, 'n')`) // NULL join key -- never matches
	exec(`INSERT INTO j2 VALUES (5, 40, 'w')`)   // a=40 -- unmatched in j1

	exec(`CREATE TABLE j3 (id INTEGER PRIMARY KEY, a INTEGER, tag TEXT)`)
	exec(`INSERT INTO j3 VALUES (1, 10, 'red')`)
	exec(`INSERT INTO j3 VALUES (2, 20, 'blue')`)
	exec(`INSERT INTO j3 VALUES (3, 30, 'green')`)

	exec(`CREATE TABLE empty_ju (id INTEGER PRIMARY KEY, a INTEGER)`)

	exec(`CREATE TABLE disjoint_l (x INTEGER PRIMARY KEY, lbl TEXT)`)
	exec(`INSERT INTO disjoint_l VALUES (1, 'L1')`)
	exec(`INSERT INTO disjoint_l VALUES (2, 'L2')`)

	exec(`CREATE TABLE disjoint_r (y INTEGER PRIMARY KEY, rbl TEXT)`)
	exec(`INSERT INTO disjoint_r VALUES (1, 'R1')`)
	exec(`INSERT INTO disjoint_r VALUES (2, 'R2')`)
	exec(`INSERT INTO disjoint_r VALUES (3, 'R3')`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeJoinUsingCorpus is the USING/NATURAL parity gate corpus.
var vdbeJoinUsingCorpus = []joinCase{
	// --- USING(a): single named common column (the coincidental shared "id"
	// stays a visible, un-coalesced duplicate on both sides) ---
	{"SELECT j1.name, j2.val FROM j1 JOIN j2 USING(a)", false},
	{"SELECT * FROM j1 JOIN j2 USING(a)", false},
	{"SELECT a, name, val FROM j1 JOIN j2 USING(a)", false}, // unqualified common column -- not ambiguous
	{"SELECT j1.a, j2.a FROM j1 JOIN j2 USING(a)", false},   // qualified refs still distinguish each side
	{"SELECT * FROM j1 JOIN j2 USING(a) ORDER BY a, j1.id, j2.id", true},
	{"SELECT * FROM j1 JOIN j2 USING(a) WHERE val <> 'z'", false},
	{"SELECT a, count(*) FROM j1 JOIN j2 USING(a) GROUP BY a ORDER BY a", true},
	{"SELECT a, count(*), group_concat(val) FROM j1 JOIN j2 USING(a) GROUP BY a ORDER BY a", true},
	{"SELECT DISTINCT a FROM j1 JOIN j2 USING(a) ORDER BY a", true},
	{"SELECT * FROM j1 JOIN j2 USING(a) ORDER BY a, j1.id, j2.id LIMIT 2", true},

	// --- LEFT JOIN USING: NULL-extension. The common column is ALWAYS the
	// LEFT side's value, matched or not -- verified directly against real
	// SQLite here, not just asserted in this package's doc comments. ---
	{"SELECT * FROM j1 LEFT JOIN j2 USING(a) ORDER BY j1.id, j2.id", true},
	{"SELECT a, name, val FROM j1 LEFT JOIN j2 USING(a) ORDER BY j1.id, j2.id", true},
	{"SELECT * FROM j1 LEFT JOIN empty_ju USING(a) ORDER BY j1.id", true}, // right always empty -> a stays j1's value throughout

	// --- NATURAL JOIN: common columns computed from schema -- here BOTH "id"
	// and "a" (j1/j2 share both names), a genuine multi-column condition. ---
	{"SELECT * FROM j1 NATURAL JOIN j2", false},
	{"SELECT * FROM j1 NATURAL JOIN j2 ORDER BY id", true},
	{"SELECT * FROM j1 NATURAL LEFT JOIN j2 ORDER BY id", true},
	{"SELECT * FROM j1 NATURAL LEFT JOIN empty_ju ORDER BY id", true},
	{"SELECT id, name FROM j1 NATURAL JOIN j2 ORDER BY id", true}, // unqualified "id" -- common to both, not ambiguous

	// --- NATURAL JOIN with NO common columns at all -> a true, condition-less
	// cross join (SQLite's own documented fallback). ---
	{"SELECT * FROM disjoint_l NATURAL JOIN disjoint_r ORDER BY x, y", true},
	{"SELECT count(*) FROM disjoint_l NATURAL JOIN disjoint_r", false},
	{"SELECT lbl, rbl FROM disjoint_l NATURAL LEFT JOIN disjoint_r ORDER BY x, y", true},

	// --- 3-table chains mixing USING/NATURAL with an ordinary ON, and two
	// USING joins re-using the same column name (chained coalescing: the
	// unqualified "a" must always resolve to j1.a, the leftmost/original
	// representative, never j2.a). ---
	{"SELECT * FROM j1 JOIN j2 USING(a) JOIN j3 ON j2.a = j3.a ORDER BY j1.id, j2.id, j3.id", true},
	{"SELECT a, name, val, tag FROM j1 JOIN j2 USING(a) JOIN j3 USING(a) ORDER BY j1.id, j2.id, j3.id", true},
	{"SELECT * FROM j1 JOIN j2 USING(a) JOIN j3 USING(a) ORDER BY j1.id, j2.id, j3.id", true},
	{"SELECT * FROM j1 NATURAL JOIN j3 ORDER BY id", true},
	{"SELECT j1.name, j3.tag FROM j1 JOIN j2 USING(a) LEFT JOIN j3 ON j2.a = j3.a ORDER BY j1.id, j2.id", true},
}

// TestVDBEJoinUsingNaturalResultParity is the hard gate: every corpus
// statement must produce identical results through the VDBE and real C
// SQLite.
func TestVDBEJoinUsingNaturalResultParity(t *testing.T) {
	path := buildVDBEJoinUsingDB(t)

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
	for _, tc := range vdbeJoinUsingCorpus {
		total++
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)

		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("VDBE JOIN USING/NATURAL result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE JOIN USING/NATURAL parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEJoinUsingDualModeIntegration runs the whole USING/NATURAL corpus
// through the ORDINARY QueryArgs entry point rather than QueryVDBE, which is
// the dispatch every caller outside this file takes (tryVDBEScan and its
// siblings, engine/vdbe_run.go). Every statement must come back without an
// error there too, not only on the direct path the gate above uses.
func TestVDBEJoinUsingDualModeIntegration(t *testing.T) {
	path := buildVDBEJoinUsingDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeJoinUsingCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] via QueryArgs: %v", tc.sql, err)
		}
	}
}

// TestVDBEJoinUsingStarCoalescing is the explicit, hand-verified "SELECT *"
// coalescing check the task's own doc comment calls out as "where bugs
// hide": rather than only trusting cross-engine agreement (which a shared
// bug could in principle survive), this asserts the EXACT column list and
// row values for a handful of USING/NATURAL "*" queries, computed by hand
// against buildVDBEJoinUsingDB's known data. Checked through both the VDBE
// and -- via cgoSelect -- C SQLite, so any disagreement between them
// also surfaces here, not just a wrong-vs-reference mismatch.
func TestVDBEJoinUsingStarCoalescing(t *testing.T) {
	path := buildVDBEJoinUsingDB(t)

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

	type starCase struct {
		sql      string
		wantCols []string
		// wantRows is pre-sorted to match ORDER BY in sql, one storage-class-
		// tagged string per cell -- the SAME "N"/"I:"/"F:"/"T:"/"X:" scheme
		// normalizeAny/normalizeEngineValue produce (see cellsEqual), since
		// queryResultsMatch compares tagged cells, not raw values.
		wantRows [][]string
	}

	cases := []starCase{
		// USING(a): only "a" is coalesced (shown once, in j1's position);
		// "id" is NOT named in USING, so BOTH j1.id and j2.id survive as two
		// separate (same-named) output columns -- exactly SQLite's rule that
		// coalescing is scoped to the NAMED columns only.
		{
			sql:      "SELECT * FROM j1 JOIN j2 USING(a) ORDER BY j1.id, j2.id",
			wantCols: []string{"id", "a", "name", "id", "val"},
			wantRows: [][]string{
				{"I:1", "I:10", "T:alice", "I:1", "T:x"},
				{"I:2", "I:20", "T:bob", "I:2", "T:y"},
				{"I:2", "I:20", "T:bob", "I:3", "T:z"},
				{"I:4", "I:20", "T:dave", "I:2", "T:y"},
				{"I:4", "I:20", "T:dave", "I:3", "T:z"},
			},
		},
		// LEFT JOIN USING(a): j1's a=30 ("eve") has no j2 match -- NULL-
		// extended, but the coalesced "a" column still shows 30 (j1's own
		// value), never NULL, and j1's a=NULL ("carol") never matches either
		// (also NULL-extended, "a" stays NULL -- it IS NULL either way here).
		{
			sql:      "SELECT * FROM j1 LEFT JOIN j2 USING(a) ORDER BY j1.id, j2.id",
			wantCols: []string{"id", "a", "name", "id", "val"},
			wantRows: [][]string{
				{"I:1", "I:10", "T:alice", "I:1", "T:x"},
				{"I:2", "I:20", "T:bob", "I:2", "T:y"},
				{"I:2", "I:20", "T:bob", "I:3", "T:z"},
				{"I:3", "N", "T:carol", "N", "N"}, // NULL-extended: a stays NULL (it already was)
				{"I:4", "I:20", "T:dave", "I:2", "T:y"},
				{"I:4", "I:20", "T:dave", "I:3", "T:z"},
				{"I:5", "I:30", "T:eve", "N", "N"}, // NULL-extended: a stays 30 (j1's own value), not NULL
			},
		},
		// NATURAL JOIN: j1/j2 share BOTH "id" and "a" -- both coalesced, so
		// j2's copies of both are hidden; only j1.id, j1.a, j1.name, j2.val
		// remain (4 columns, not 5). Only id/a pairs that match on BOTH
		// columns survive (id=1/a=10 and id=2/a=20); id=3's NULL a, id=4's
		// mismatched a, and id=5's unmatched a all drop out.
		{
			sql:      "SELECT * FROM j1 NATURAL JOIN j2 ORDER BY id",
			wantCols: []string{"id", "a", "name", "val"},
			wantRows: [][]string{
				{"I:1", "I:10", "T:alice", "T:x"},
				{"I:2", "I:20", "T:bob", "T:y"},
			},
		},
		// NATURAL JOIN with no common columns at all: a true cross join,
		// every disjoint_l row paired with every disjoint_r row, no column
		// hidden or renamed at all (4 columns: x, lbl, y, rbl).
		{
			sql:      "SELECT * FROM disjoint_l NATURAL JOIN disjoint_r ORDER BY x, y",
			wantCols: []string{"x", "lbl", "y", "rbl"},
			wantRows: [][]string{
				{"I:1", "T:L1", "I:1", "T:R1"},
				{"I:1", "T:L1", "I:2", "T:R2"},
				{"I:1", "T:L1", "I:3", "T:R3"},
				{"I:2", "T:L2", "I:1", "T:R1"},
				{"I:2", "T:L2", "I:2", "T:R2"},
				{"I:2", "T:L2", "I:3", "T:R3"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
			if cErr != nil {
				t.Fatalf("cgoSelect: %v", cErr)
			}
			if ok, reason := queryResultsMatch(tc.wantCols, tc.wantRows, cCols, cRows, true); !ok {
				t.Fatalf("hand-computed expectation itself disagrees with C SQLite (fix the test): %s\n  want: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
					reason, tc.wantCols, tc.wantRows, cCols, cRows)
			}

			vCols, vVals, vErr := p.QueryArgs(tc.sql, nil)
			if vErr != nil {
				t.Fatalf("QueryVDBE: %v", vErr)
			}
			vRows := engineRowsToStrings(vVals)
			if ok, reason := queryResultsMatch(vCols, vRows, tc.wantCols, tc.wantRows, true); !ok {
				t.Errorf("VDBE coalescing mismatch: %s\n  vdbe: cols=%v rows=%v\n  want: cols=%v rows=%v",
					reason, vCols, vRows, tc.wantCols, tc.wantRows)
			}
		})
	}
}

// TestVDBEJoinUsingNaturalRejectsCombinations pins the parse-time rejections
// C SQLite also applies -- NATURAL combined with an explicit ON or USING
// on the same join is a parse error -- consistently across C SQLite and
// the VDBE-integrated QueryArgs path.
func TestVDBEJoinUsingNaturalRejectsCombinations(t *testing.T) {
	path := buildVDBEJoinUsingDB(t)

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

	for _, sqlText := range []string{
		"SELECT * FROM j1 NATURAL JOIN j2 ON j1.a = j2.a",
		"SELECT * FROM j1 NATURAL JOIN j2 USING (a)",
		"SELECT * FROM j1 JOIN j2 USING (no_such_column)",
	} {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Fatalf("[%s] expected C SQLite to reject this, got nil", sqlText)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] expected the integrated QueryArgs path to reject this, got nil", sqlText)
		}
	}
}

// TestVDBEJoinUsingRightFullSupported pins that RIGHT JOIN and FULL JOIN --
// including composed with USING/NATURAL -- ARE now supported, in the
// "last FROM item" shape (see join.go's rightOuterIndex and
// vdbe_join_outer_test.go for their own dedicated result-parity gate): they
// reach QueryVDBE/QueryArgs and run cleanly, rather than being rejected by
// the parser.
func TestVDBEJoinUsingRightFullSupported(t *testing.T) {
	path := buildVDBEJoinUsingDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		"SELECT * FROM j1 RIGHT JOIN j2 ON j1.a = j2.a",
		"SELECT * FROM j1 FULL JOIN j2 ON j1.a = j2.a",
		"SELECT * FROM j1 RIGHT JOIN j2 USING(a)",
		"SELECT * FROM j1 NATURAL RIGHT JOIN j2",
		"SELECT * FROM j1 NATURAL FULL JOIN j2",
	} {
		if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
			t.Errorf("QueryVDBE(%q): %v", sqlText, err)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
			t.Errorf("QueryArgs(%q): %v", sqlText, err)
		}
	}
}

// TestVDBEJoinUsingStillFallsBackForRightFullMidChain pins that a mid-chain
// RIGHT JOIN composed with USING/NATURAL still cleanly declines (never
// silently mismatches) for BOTH corpus statements below -- though for a
// reason that has nothing to do with mid-chain RIGHT/FULL placement anymore:
// the VDBE codegen now compiles a RIGHT/FULL JOIN at any position (see
// vdbe_join_codegen.go's emitJoinLoops/emitRightOuterSweep and
// TestVDBESupportsMultiWayRightFull), so what actually declines here is real,
// GENUINE SQL ambiguity -- verified directly against C SQLite
// (mattn/go-sqlite3), which rejects both with the identical "ambiguous
// column name: a" -- because each corpus statement's TRAILING join ("JOIN j3
// ON j2.a = j3.a") is an ORDINARY (non-USING/NATURAL) join, so j3's own "a"
// is never hidden/coalesced the way j1/j2's shared "a" is: a bare/`*`
// reference to "a" then has two genuine candidates (the coalesced j1-or-j2
// value, and j3's separate own column), which is ambiguous in real SQL, not
// a shape gap. (A trailing USING/NATURAL join reusing the same column name
// instead -- see TestRightFullJoinLaterQualifiedRefUsesCoalesceFallback --
// has no such second candidate and resolves cleanly.)
func TestVDBEJoinUsingStillFallsBackForRightFullMidChain(t *testing.T) {
	path := buildVDBEJoinUsingDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		"SELECT * FROM j1 RIGHT JOIN j2 USING(a) JOIN j3 ON j2.a = j3.a",
		"SELECT * FROM j1 NATURAL RIGHT JOIN j2 JOIN j3 ON j2.a = j3.a",
	} {
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("QueryVDBE(%q): expected an unsupported/compile error, got nil", sqlText)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("QueryArgs(%q): expected RIGHT/FULL JOIN to still be rejected, got nil", sqlText)
		}
	}
}
