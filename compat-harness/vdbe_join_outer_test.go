package compat

// Tests RIGHT and FULL OUTER JOIN correctness, comparing VDBE output against
// C SQLite. Row order is significant and checked positionally.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// vdbeOuterJoinCorpus reuses buildVDBEJoinDB's schema (vdbe_join_test.go):
// t1/t2/t3 share join key "a" with duplicates and NULLs on both sides, plus
// an unmatched value on each side; empty_j is genuinely empty.
var vdbeOuterJoinCorpus = []joinCase{
	// --- RIGHT JOIN ON: matches (including duplicates on either side) plus
	// non-matches -> t1 NULL, exactly the mirror of LEFT's own NULL side ---
	{"SELECT t1.name, t2.val FROM t1 RIGHT JOIN t2 ON t1.a = t2.a", true},
	{"SELECT t1.name, t2.val FROM t1 RIGHT OUTER JOIN t2 ON t1.a = t2.a", true},
	{"SELECT t2.val, t1.name FROM t1 RIGHT JOIN t2 ON t1.a = t2.a", true}, // select-list order independent of join column order

	// --- FULL JOIN ON: LEFT's own inline NULL-extension (t1 unmatched ->
	// t2 NULL) PLUS RIGHT's appended second pass (t2 unmatched -> t1 NULL) ---
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a", true},
	{"SELECT t1.name, t2.val FROM t1 FULL OUTER JOIN t2 ON t1.a = t2.a", true},

	// --- RIGHT/FULL composed with USING and NATURAL (desugarJoinItem, shared
	// with LEFT/INNER -- no RIGHT/FULL-specific desugaring logic exists, so
	// this also exercises that the shared desugar path is unaffected) ---
	{"SELECT t1.name, t2.val FROM t1 RIGHT JOIN t2 USING (a)", true},
	{"SELECT t1.name, t2.val FROM t1 NATURAL RIGHT JOIN t2", true},
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 USING (a)", true},
	{"SELECT t1.name, t2.val FROM t1 NATURAL FULL JOIN t2", true},

	// --- RIGHT JOIN ... WHERE t1.col IS NULL: the anti-join direction (only
	// t2's never-matched rows survive) ---
	{"SELECT t1.name, t2.val FROM t1 RIGHT JOIN t2 ON t1.a = t2.a WHERE t1.id IS NULL", true},
	{"SELECT t2.val FROM t1 RIGHT JOIN t2 ON t1.a = t2.a WHERE t1.name IS NULL", true},

	// --- FULL JOIN anti-join BOTH directions at once ---
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a WHERE t1.id IS NULL OR t2.id IS NULL", true},
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a WHERE t1.id IS NULL", true},
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a WHERE t2.id IS NULL", true},

	// --- empty table on either side ---
	{"SELECT * FROM t1 RIGHT JOIN empty_j ON t1.a = empty_j.a", true}, // right side empty -> zero rows (RIGHT guarantees every RIGHT row, and there are none)
	{"SELECT * FROM empty_j RIGHT JOIN t1 ON t1.a = empty_j.a", true}, // left side empty -> every t1 row NULL-extended
	{"SELECT * FROM t1 FULL JOIN empty_j ON t1.a = empty_j.a", true},  // FULL, right side empty -> every t1 row NULL-extended (no t2-side rows to append)
	{"SELECT * FROM empty_j FULL JOIN t1 ON t1.a = empty_j.a", true},  // FULL, left side empty -> every t1 row appears via the second pass, NULL-extending empty_j

	// --- NULL join keys never match, on either side of a RIGHT/FULL join ---
	{"SELECT t1.name, t2.val FROM t1 RIGHT JOIN t2 ON t1.a = t2.a", true}, // carol(NULL)/n(NULL) never pair; both still surface via their own unmatched handling
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a", true},

	// --- RIGHT/FULL + ORDER BY: nails a TOTAL order regardless of scan order ---
	{"SELECT t1.name, t2.id, t2.val FROM t1 RIGHT JOIN t2 ON t1.a = t2.a ORDER BY t2.id", true},
	{"SELECT t1.name, t2.val FROM t1 RIGHT JOIN t2 ON t1.a = t2.a ORDER BY t1.name, t2.val", true},
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a ORDER BY t2.val, t1.name", true},

	// --- RIGHT/FULL + its own extra WHERE (beyond the anti-join shape above) ---
	{"SELECT t1.name, t2.val FROM t1 RIGHT JOIN t2 ON t1.a = t2.a WHERE t2.val <> 'z'", true},
	{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a WHERE t1.name <> 'bob'", true},

	// --- RIGHT/FULL + aggregate ---
	{"SELECT count(*) FROM t1 RIGHT JOIN t2 ON t1.a = t2.a", true},
	{"SELECT count(*) FROM t1 FULL JOIN t2 ON t1.a = t2.a", true},
	{"SELECT t2.a, count(*) FROM t1 RIGHT JOIN t2 ON t1.a = t2.a GROUP BY t2.a", false}, // GROUP BY row order isn't otherwise specified
	{"SELECT t2.a, count(*) FROM t1 RIGHT JOIN t2 ON t1.a = t2.a GROUP BY t2.a ORDER BY t2.a", true},

	// --- column order of SELECT *: t1's columns then t2's, unaffected by
	// which side is guaranteed-present (RIGHT) or by NULL-extension (FULL) ---
	{"SELECT * FROM t1 RIGHT JOIN t2 ON t1.a = t2.a", true},
	{"SELECT * FROM t1 FULL JOIN t2 ON t1.a = t2.a", true},

	// --- 3-table: RIGHT/FULL as the LAST item, preceded by a comma join, a
	// plain INNER JOIN, and a LEFT JOIN (the "simple" shapes this package
	// supports -- see join.go's rightOuterIndex) ---
	{"SELECT t1.name, t2.val, t3.tag FROM t1, t2 RIGHT JOIN t3 ON t2.a = t3.a", true},
	{"SELECT t1.name, t2.val, t3.tag FROM t1 JOIN t2 ON t1.a = t2.a RIGHT JOIN t3 ON t2.a = t3.a", true},
	{"SELECT t1.name, t2.val, t3.tag FROM t1 LEFT JOIN t2 ON t1.a = t2.a RIGHT JOIN t3 ON t2.a = t3.a", true},
	{"SELECT t1.name, t2.val, t3.tag FROM t1, t2 FULL JOIN t3 ON t2.a = t3.a", true},
	{"SELECT t1.name, t2.val, t3.tag FROM t1 JOIN t2 ON t1.a = t2.a FULL JOIN t3 ON t2.a = t3.a", true},
}

// TestVDBEOuterJoinResultParity is the hard gate: every corpus statement
// must produce identical results (including exact row ORDER -- see this
// file's package doc comment) through the VDBE and C SQLite.
func TestVDBEOuterJoinResultParity(t *testing.T) {
	path := buildVDBEJoinDB(t)

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
	for _, tc := range vdbeOuterJoinCorpus {
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
	t.Logf("VDBE RIGHT/FULL JOIN result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE RIGHT/FULL JOIN parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEOuterJoinDualModeIntegration exercises the VDBEMode=dual
// integration path (tryVDBEScan) through the ordinary QueryArgs entry point
// for the whole RIGHT/FULL corpus, confirming every case answers there
// without error.
func TestVDBEOuterJoinDualModeIntegration(t *testing.T) {
	path := buildVDBEJoinDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeOuterJoinCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] VDBEMode=dual: %v", tc.sql, err)
		}
	}
}

// TestVDBESupportsMultiWayRightFull pins that the VDBE codegen now compiles
// (and correctly evaluates, against C SQLite) the general multi-way
// RIGHT/FULL shape that used to be declined outright: a RIGHT/FULL-joined
// table that isn't the LAST FROM item, and more than one RIGHT/FULL-joined
// table in the same FROM clause -- see engine/vdbe_join_codegen.go's
// emitJoinLoops/emitRightOuterSweep, which reproduce the RIGHT/FULL fold
// directly (join.go's package doc comment, RIGHT/FULL section, is where that
// fold is stated). This used to be TestVDBEDeclinesMultiWayRightFull (pinning
// the OPPOSITE: a clean decline here, which QueryArgs then papered over by
// answering the statement another way); vdbe_rightfull_join_test.go is the
// broader, dedicated
// correctness gate for these shapes' VALUES -- this test just confirms,
// alongside it, that QueryVDBE itself (not merely the integrated QueryArgs
// path) now accepts and correctly answers them.
func TestVDBESupportsMultiWayRightFull(t *testing.T) {
	path := buildVDBEJoinDB(t)

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
		// RIGHT JOIN not the last FROM item (another JOIN follows it).
		"SELECT * FROM t1 x RIGHT JOIN t1 y ON x.id = y.id JOIN t2 ON 1 = 1 ORDER BY x.id, y.id, t2.id",
		// FULL JOIN not the last FROM item.
		"SELECT * FROM t1 x FULL JOIN t1 y ON x.id = y.id JOIN t2 ON 1 = 1 ORDER BY x.id, y.id, t2.id",
		// Two RIGHT JOINs chained in the same FROM clause.
		"SELECT * FROM t1 x RIGHT JOIN t1 y ON x.a = y.a RIGHT JOIN t1 z ON y.a = z.a ORDER BY z.id, y.id, x.id",
		// A RIGHT JOIN followed by a comma join (the comma item comes after).
		"SELECT * FROM t1 x RIGHT JOIN t1 y ON x.id = y.id, t2 ORDER BY x.id, y.id, t2.id",
	} {
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("QueryVDBE(%q): %v", sqlText, vErr)
			continue
		}
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] cgo: %v", sqlText, cErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, true); !ok {
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
			t.Errorf("QueryArgs(%q): %v", sqlText, err)
		}
	}
}

// TestOuterJoinParenthesizedStillFallsBack pins the genuinely different
// shape that stays declined even now that a parenthesized join subtree
// containing an outer join is otherwise supported (see join.go's
// collapseGroups/resolveGroupRelation and FromItem.GroupLen's doc comment,
// and vdbe_nested_join_test.go for the general case's own gate): a nested
// group's OWN internal join condition written to EXPLICITLY reference a
// table from OUTSIDE its own parentheses -- e.g. "(t1 y RIGHT JOIN t2 ON
// x.a = y.a)", whose "x.a" names a table outside the parenthesized group
// entirely. C SQLite itself rejects exactly this ("no such column:
// x.a", verified directly against mattn/go-sqlite3): standard SQL
// join-tree scoping means a nested joined-table's own join condition can
// only see its own children, never an outside sibling -- see join.go's
// fixGroupInternalScoping, which declines the identical shape rather than
// silently reading an always-NULL value for the escaped reference. (The
// OTHER, still-parenthesized query this test used to also pin --
// "t1 x RIGHT JOIN (t1 y LEFT JOIN t2 ON y.a = t2.a) ON x.a = y.a" -- has
// no such escaping reference (its "x.a = y.a" is the group's own CONNECTOR,
// legitimately evaluated outside the group) and now succeeds; it's covered
// by vdbe_nested_join_test.go instead.)
func TestOuterJoinParenthesizedStillFallsBack(t *testing.T) {
	path := buildVDBEJoinDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		// Parenthesized outer join whose OWN internal ON explicitly
		// references a table outside its own parentheses (x) -- declined
		// regardless of connector kind, matching C SQLite's rejection.
		"SELECT * FROM t1 x JOIN (t1 y RIGHT JOIN t2 ON x.a = y.a) ON 1 = 1",
	} {
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("QueryVDBE(%q): expected an unsupported/compile error, got nil", sqlText)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("QueryArgs(%q): expected a resolve error (join.go's fixGroupInternalScoping), got nil", sqlText)
		}
	}
}
