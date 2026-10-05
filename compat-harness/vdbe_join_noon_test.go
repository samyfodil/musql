package compat

// This file tests JOINs with no constraint (INNER/LEFT/RIGHT/FULL JOIN without ON).
// forEachJoinedRow/execRightFullJoin and engine/vdbe_join_codegen.go's
// emitJoinLevel already treat a nil join condition as "always matches" for
// EVERY join kind, unconditionally -- a case a NATURAL join with no common
// columns already exercised (see engine/join.go's desugarJoinItem doc
// comment). The only change was engine/sql_parser.go's parseFromClause
// accepting a missing ON/USING/NATURAL on a non-CROSS join instead of
// raising "expected ON after JOIN".
//
// Follows vdbe_rightfull_join_test.go's table-driven parity shape: every
// case is checked at BOTH page sizes 512 and 4096, through
//
//	(a) the integrated engine path -- (*engine.ReadOnlyPager).Query (the VDBE)
//	(b) C SQLite               -- github.com/mattn/go-sqlite3 (ground truth)
//
// against each other, ordered (every case carries an explicit ORDER BY, so
// an ordered comparison is meaningful). QueryVDBE itself is checked too, but
// only as bonus coverage when it happens to succeed: a RIGHT/FULL-joined
// item still has to satisfy vdbeRightOuterIndex's single-item-last-position
// shape to compile directly, same as an ON-bearing RIGHT/FULL JOIN already
// does elsewhere.
//
// Schema (buildJoinNoOnDB, below): nj1 (2 rows) and nj2 (3 rows) give a
// non-trivial, easily-eyeballed cross product (2x3=6 rows); nj3 (1 row)
// extends that to a 3-way chain with no constraint anywhere. emptyL and
// emptyR are same-shaped but genuinely empty tables, isolating the
// empty-side edge case on each side of LEFT/RIGHT/FULL independently of the
// other.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildJoinNoOnDB builds, at pageSize, the nj1/nj2/nj3/emptyL/emptyR fixture
// described above.
func buildJoinNoOnDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("joinnoon_%d.sqlite", pageSize))
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

	exec(`CREATE TABLE nj1 (id1 INTEGER PRIMARY KEY, v1 TEXT)`)
	exec(`INSERT INTO nj1 VALUES (1, 'a1')`)
	exec(`INSERT INTO nj1 VALUES (2, 'a2')`)

	exec(`CREATE TABLE nj2 (id2 INTEGER PRIMARY KEY, v2 TEXT)`)
	exec(`INSERT INTO nj2 VALUES (1, 'b1')`)
	exec(`INSERT INTO nj2 VALUES (2, 'b2')`)
	exec(`INSERT INTO nj2 VALUES (3, 'b3')`)

	exec(`CREATE TABLE nj3 (id3 INTEGER PRIMARY KEY, v3 TEXT)`)
	exec(`INSERT INTO nj3 VALUES (1, 'c1')`)

	exec(`CREATE TABLE emptyL (idL INTEGER PRIMARY KEY, vL TEXT)`)
	exec(`CREATE TABLE emptyR (idR INTEGER PRIMARY KEY, vR TEXT)`)

	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	return path
}

// joinNoOnCorpus is the matrix of no-join-constraint statements this gate
// checks -- every join operator, no ON/USING/NATURAL at all, plus the
// empty-side edge cases and a WHERE-filtered variant.
var joinNoOnCorpus = []string{
	// --- plain/INNER JOIN, no constraint: cross product ---
	`SELECT id1, v1, id2, v2 FROM nj1 JOIN nj2 ORDER BY id1, id2`,
	`SELECT id1, v1, id2, v2 FROM nj1 INNER JOIN nj2 ORDER BY id1, id2`,
	// 3-way chain, no constraint anywhere
	`SELECT id1, id2, id3 FROM nj1 JOIN nj2 JOIN nj3 ORDER BY id1, id2, id3`,
	`SELECT id1, id2, id3 FROM nj1 INNER JOIN nj2 INNER JOIN nj3 ORDER BY id1, id2, id3`,

	// --- LEFT JOIN, no constraint, right side NON-EMPTY: cross product, no
	//     NULL-extension (every right row matches every left row) ---
	`SELECT id1, v1, id2, v2 FROM nj1 LEFT JOIN nj2 ORDER BY id1, id2`,
	`SELECT id1, v1, id2, v2 FROM nj1 LEFT OUTER JOIN nj2 ORDER BY id1, id2`,

	// --- LEFT JOIN, no constraint, right side EMPTY: every left row still
	//     emitted once, NULL-extended for the right table's columns ---
	`SELECT id1, v1, idR, vR FROM nj1 LEFT JOIN emptyR ORDER BY id1`,

	// --- RIGHT JOIN, no constraint ---
	`SELECT id1, id2 FROM nj1 RIGHT JOIN nj2 ORDER BY id2, id1`,
	// left side EMPTY: every right row still emitted once, NULL-extended left
	`SELECT idL, vL, id2, v2 FROM emptyL RIGHT JOIN nj2 ORDER BY id2`,
	// right side EMPTY: no rows at all (nothing to be "every row of" on the right)
	`SELECT id1, idR FROM nj1 RIGHT JOIN emptyR ORDER BY id1`,

	// --- FULL JOIN, no constraint ---
	`SELECT id1, id2 FROM nj1 FULL JOIN nj2 ORDER BY id1, id2`,
	`SELECT id1, id2 FROM nj1 FULL OUTER JOIN nj2 ORDER BY id1, id2`,
	// left side EMPTY: right's own never-matched-row pass NULL-pads left
	`SELECT idL, vL, id2, v2 FROM emptyL FULL JOIN nj2 ORDER BY id2`,
	// right side EMPTY: left's own inline NULL-extension covers every left row
	`SELECT id1, v1, idR, vR FROM nj1 FULL JOIN emptyR ORDER BY id1`,
	// both sides EMPTY: no rows at all
	`SELECT idL, idR FROM emptyL FULL JOIN emptyR`,

	// --- mixed with WHERE ---
	`SELECT id1, id2 FROM nj1 JOIN nj2 WHERE v1 = 'a1' ORDER BY id2`,
	`SELECT id1, id2 FROM nj1 INNER JOIN nj2 WHERE id2 = 2 ORDER BY id1`,
	`SELECT id1, v1, id2, v2 FROM nj1 LEFT JOIN nj2 WHERE v2 = 'b1' ORDER BY id1`,
	`SELECT id1, v1, idR, vR FROM nj1 LEFT JOIN emptyR WHERE id1 = 1`,
	`SELECT id1, id2 FROM nj1 RIGHT JOIN nj2 WHERE id1 = 1 ORDER BY id2`,
}

// TestJoinNoConstraintTableParity is the hard gate: every joinNoOnCorpus
// statement must produce identical, identically-ordered results through the
// integrated engine path and C SQLite -- at both page sizes.
func TestJoinNoConstraintTableParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildJoinNoOnDB(t, pageSize)

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
			for _, sqlText := range joinNoOnCorpus {
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

				// QueryVDBE itself is bonus coverage only: a RIGHT/FULL item
				// still must satisfy the VDBE's own single-item-last-position
				// fast path (vdbeRightOuterIndex) to compile directly here,
				// same as an ON-bearing RIGHT/FULL JOIN already requires.
				if vCols, vVals, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
					vRows := engineRowsToStrings(vVals)
					if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, true); !ok {
						wrong++
						t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
							sqlText, reason, vCols, vRows, cCols, cRows)
					}
				}
			}

			t.Logf("JOIN-with-no-constraint parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("JOIN-with-no-constraint parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}
