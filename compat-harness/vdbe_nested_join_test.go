package compat

import (
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// Correctness gate for parenthesized (nested) JOIN trees: "A JOIN (B JOIN C ON ...)
// ON ..." versus flat chains. Tests join grouping for OUTER joins, where grouping is
// significant: "A LEFT JOIN (B JOIN C ON p) ON q" differs from flat chains.
var nestedJoinCorpus = []string{
	// Transitive coalesce chains through group connectors.
	`SELECT coalesce(rf1.k, rf2.k, rf3.k) FROM rf1 FULL JOIN (rf2 FULL JOIN rf3 USING (k)) USING (k) ORDER BY 1`,
	`SELECT k FROM rf1 FULL JOIN (rf2 FULL JOIN rf3 USING (k)) USING (k) ORDER BY 1`,
	`SELECT k, v1, v2, v3 FROM rf1 FULL JOIN (rf2 FULL JOIN rf3 USING (k)) USING (k) ORDER BY 1,2,3,4`,
	`SELECT k FROM rf1 RIGHT JOIN (rf2 RIGHT JOIN rf3 USING (k)) USING (k) ORDER BY 1`,
	`SELECT k, v1, v2, v3 FROM rf1 RIGHT JOIN (rf2 FULL JOIN rf3 USING (k)) USING (k) ORDER BY 1,2,3,4`,

	// Pure INNER associative join (baseline: grouping is a no-op for INNER).
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf1.id1, rf2.id2, rf3.id3`,

	// Headline case: LEFT JOIN (B JOIN C ON p) ON q shows NULL together, never partial match.
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf1.id1, rf2.id2, rf3.id3`,
	// Connector condition references group's second member.
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf3.k ORDER BY rf1.id1, rf2.id2, rf3.id3`,

	// Deeper coverage: two sibling parenthesized groups.
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM (rf1 JOIN rf2 ON rf1.k = rf2.k) LEFT JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k ORDER BY rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM (rf1 LEFT JOIN rf2 ON rf1.k = rf2.k) LEFT JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf1.k = rf3.k ORDER BY rf1.id1, rf2.id2, rf3.id3, rf4.id4`,

	// USING against a sub-join.
	`SELECT k, v1, v2, v3 FROM rf1 JOIN (rf2 JOIN rf3 USING (k)) USING (k) ORDER BY rf1.id1, rf2.id2, rf3.id3`,
	`SELECT k, v1, v2, v3 FROM rf1 LEFT JOIN (rf2 JOIN rf3 USING (k)) USING (k) ORDER BY rf1.id1, rf2.id2, rf3.id3`,

	// NATURAL against a sub-join: bare "k" reference resolves unambiguously to single representative.
	`SELECT rf1.v1, rf2.v2, rf3.v3, k FROM rf1 NATURAL JOIN (rf2 JOIN rf3 USING (k)) ORDER BY rf1.id1, rf2.id2, rf3.id3`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, k FROM rf1 NATURAL LEFT JOIN (rf2 JOIN rf3 USING (k)) ORDER BY rf1.id1, rf2.id2, rf3.id3`,

	// RIGHT/FULL wrapping an INNER-only unit.
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 RIGHT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf2.id2, rf3.id3, rf1.id1`,
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 FULL JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY coalesce(rf1.k, rf2.k, rf3.k), rf1.id1, rf2.id2, rf3.id3`,
	// RIGHT/FULL wrapping a unit whose internal join is itself RIGHT.
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 RIGHT JOIN (rf2 RIGHT JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf3.id3, rf2.id2, rf1.id1`,

	// WHERE filter alongside nested group: applied post-grouping.
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k WHERE rf2.v2 IS NULL ORDER BY rf1.id1, rf2.id2, rf3.id3`,

	// Comma-joined table alongside nested group.
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM rf1, rf4 JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf4.k = rf2.k ORDER BY rf1.id1, rf4.id4, rf2.id2, rf3.id3`,

	// Truly nested: A op (B op (C op D)), 4 tables deep, mixed INNER/OUTER.
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM rf1 LEFT JOIN (rf2 JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM rf1 LEFT JOIN (rf2 RIGHT JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf3.id3, rf4.id4, rf2.id2, rf1.id1`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM rf1 JOIN (rf2 LEFT JOIN (rf3 RIGHT JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf1.id1, rf2.id2, rf4.id4, rf3.id3`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4 FROM rf1 FULL JOIN (rf2 JOIN (rf3 FULL JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY coalesce(rf1.k, rf2.k, rf3.k, rf4.k), rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	// Truly nested via USING, INNER connector outside, mixed RIGHT inside.
	`SELECT k, v1, v2, v3, v4 FROM rf1 JOIN (rf2 RIGHT JOIN (rf3 JOIN rf4 USING (k)) USING (k)) USING (k) ORDER BY rf3.id3, rf4.id4, rf2.id2, rf1.id1`,

	// Mixed INNER/LEFT/RIGHT/FULL inside and outside a single group.
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 LEFT JOIN (rf2 RIGHT JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf3.id3, rf2.id2, rf1.id1`,
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 RIGHT JOIN (rf2 FULL JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY rf2.id2, rf3.id3, rf1.id1`,
	`SELECT rf1.v1, rf2.v2, rf3.v3 FROM rf1 FULL JOIN (rf2 LEFT JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2, rf3.id3`,

	// --- RIGHT/FULL connector combined with USING/NATURAL inside the group
	// (the group's own connector -- rf2 -- has no coalesce fallback of its
	// own here, so no transitive chain is needed -- see
	// TestNestedJoinGroupDeclinedShapes for the shape that DOES need one) ---
	`SELECT rf1.v1, rf2.v2, rf3.v3, k FROM rf1 RIGHT JOIN (rf2 JOIN rf3 USING (k)) USING (k) ORDER BY rf2.id2, rf3.id3, rf1.id1`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, k FROM rf1 FULL JOIN (rf2 JOIN rf3 USING (k)) USING (k) ORDER BY coalesce(rf1.k, rf2.k), rf1.id1, rf2.id2, rf3.id3`,
	`SELECT rf1.v1, rf2.v2, rf3.v3, k FROM rf1 RIGHT JOIN (rf2 NATURAL JOIN rf3) USING (k) ORDER BY rf2.id2, rf3.id3, rf1.id1`,
	// Aggregates and GROUP BY over groups: member scopes now rebasedsay correctly.
	`SELECT count(*) FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k`,
	`SELECT rf1.id1, count(*) FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k GROUP BY rf1.id1 ORDER BY rf1.id1`,
	`SELECT rf1.id1, count(rf2.v2), count(rf3.v3) FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k GROUP BY rf1.id1 ORDER BY rf1.id1`,
	`SELECT rf2.v2, count(*) FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k GROUP BY rf2.v2 ORDER BY rf2.v2`,
	`SELECT sum(rf1.k), sum(rf2.k), sum(rf3.k) FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k`,
	`SELECT count(*) FROM rf1 JOIN (rf2 LEFT JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k`,
	`SELECT count(*) FROM rf1 LEFT JOIN (rf2 LEFT JOIN rf3 USING(k)) USING(k)`,
	`SELECT rf1.k, count(*) FROM rf1 LEFT JOIN (rf2 LEFT JOIN rf3 USING(k)) USING(k) GROUP BY rf1.k ORDER BY rf1.k`,
	`SELECT count(*) FROM rf1 RIGHT JOIN (rf2 JOIN rf3 ON rf2.k=rf3.k) ON rf1.k=rf2.k`,
	`SELECT rf3.v3, count(*) FROM rf1 RIGHT JOIN (rf2 JOIN rf3 ON rf2.k=rf3.k) ON rf1.k=rf2.k GROUP BY rf3.v3 ORDER BY rf3.v3`,
	`SELECT count(*) FROM rf1 FULL JOIN (rf2 JOIN rf3 ON rf2.k=rf3.k) ON rf1.k=rf2.k`,
	`SELECT max(rf3.v3), min(rf2.v2) FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k`,
	`SELECT count(*) FROM rf1 LEFT JOIN (rf2 JOIN (rf3 JOIN rf4 ON rf3.k=rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k`,
	`SELECT rf1.id1, group_concat(rf3.v3) FROM rf1 LEFT JOIN (rf2 JOIN rf3 ON rf2.k = rf3.k) ON rf1.k = rf2.k GROUP BY rf1.id1 ORDER BY rf1.id1`,
}

// TestNestedJoinTableParity: every corpus statement produces identical results
// through the engine and C SQLite at page sizes 512 and 4096.
func TestNestedJoinTableParity(t *testing.T) {
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
			for _, sqlText := range nestedJoinCorpus {
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

				// QueryVDBE is REQUIRED to succeed and match for every
				// statement in this corpus (unlike this file's earlier
				// form): resolveGroupSource (vdbe_join_codegen.go) compiles
				// every shape here, so a QueryVDBE failure now indicates a
				// real compiler regression, not an expected decline.
				vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
				if vErr != nil {
					wrong++
					t.Errorf("[%s] QueryVDBE unexpectedly declined: %v", sqlText, vErr)
					continue
				}
				vRows := engineRowsToStrings(vVals)
				if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, true); !ok {
					wrong++
					t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
						sqlText, reason, vCols, vRows, cCols, cRows)
				}
			}

			t.Logf("nested (parenthesized) JOIN parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("nested (parenthesized) JOIN parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}

