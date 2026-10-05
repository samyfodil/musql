package compat

// Tests VDBE SELECT DISTINCT deduplication.
// DISTINCT dedup uses the same row-equality as GROUP BY/UNION (NULL-equal --
// see engine/sql_group.go's keysEqual and engine/vdbe_distinct.go), reused
// verbatim rather than reimplemented, so a correct VDBE compile agrees with
// GROUP BY's and UNION's own dedup by construction.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEDistinctDB builds the tables this gate needs: a duplicate-heavy
// table with NULLs in both columns (including an all-NULL row duplicated
// verbatim -- NULL-equal dedup), a table with every SQL storage class packed
// into one NONE-affinity column (some exact duplicates within a class), two
// join tables whose join produces duplicate combinations on the dedup target
// column, a genuinely empty table, and a many-row low-cardinality multi-page
// table (a real multi-page scan, not a single leaf).
func buildVDBEDistinctDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vdbe_distinct.sqlite")
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

	// dt: duplicate rows (exact and partial), NULLs in both columns
	// (including an all-NULL row duplicated verbatim), every storage class in
	// c, and a signed-zero REAL.
	exec(`CREATE TABLE dt (a INTEGER, b TEXT, c REAL)`)
	exec(`INSERT INTO dt VALUES (1, 'x', 1.5)`)
	exec(`INSERT INTO dt VALUES (1, 'x', 1.5)`) // exact duplicate
	exec(`INSERT INTO dt VALUES (1, 'y', 1.5)`) // same a, different b -- not a dup
	exec(`INSERT INTO dt VALUES (2, 'x', 2.5)`)
	exec(`INSERT INTO dt VALUES (NULL, NULL, NULL)`)
	exec(`INSERT INTO dt VALUES (NULL, NULL, NULL)`) // all-NULL duplicate
	exec(`INSERT INTO dt VALUES (3, NULL, -0.0)`)
	exec(`INSERT INTO dt VALUES (NULL, 'z', 3.5)`)
	exec(`INSERT INTO dt VALUES (2, 'x', 2.5)`) // duplicate of an earlier row

	// mx: every SQL storage class in one NONE-affinity column, with an exact
	// duplicate integer and an exact duplicate NULL.
	exec(`CREATE TABLE mx (id INTEGER PRIMARY KEY, m)`)
	exec(`INSERT INTO mx (m) VALUES (7)`)
	exec(`INSERT INTO mx (m) VALUES (7)`) // duplicate integer
	exec(`INSERT INTO mx (m) VALUES (7.5)`)
	exec(`INSERT INTO mx (m) VALUES ('seven')`)
	exec(`INSERT INTO mx (m) VALUES (x'0007')`)
	exec(`INSERT INTO mx (m) VALUES (NULL)`)
	exec(`INSERT INTO mx (m) VALUES (NULL)`) // duplicate NULL

	// jt1/jt2: a join key with duplicates on both sides, so the joined output
	// column has repeated combinations for DISTINCT to collapse.
	exec(`CREATE TABLE jt1 (id INTEGER PRIMARY KEY, k INTEGER)`)
	exec(`INSERT INTO jt1 VALUES (1, 10)`)
	exec(`INSERT INTO jt1 VALUES (2, 10)`)
	exec(`INSERT INTO jt1 VALUES (3, 20)`)
	exec(`INSERT INTO jt1 VALUES (4, NULL)`)
	exec(`CREATE TABLE jt2 (id INTEGER PRIMARY KEY, k INTEGER, val TEXT)`)
	exec(`INSERT INTO jt2 VALUES (1, 10, 'p')`)
	exec(`INSERT INTO jt2 VALUES (2, 10, 'q')`)
	exec(`INSERT INTO jt2 VALUES (3, 20, 'r')`)
	exec(`INSERT INTO jt2 VALUES (4, NULL, 'n')`)

	exec(`CREATE TABLE empty_d (x INTEGER, y TEXT)`) // genuinely empty

	// big: many rows, low-cardinality "tag", at a 512-byte page size -- forces
	// a real multi-page scan with a lot of DISTINCT duplication to collapse.
	exec(`CREATE TABLE big (id INTEGER PRIMARY KEY, tag INTEGER, v INTEGER)`)
	for i := 1; i <= 400; i++ {
		exec(fmt.Sprintf(`INSERT INTO big VALUES (%d, %d, %d)`, i, i%7, i))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeDistinctCorpus is the DISTINCT gate corpus: single/multiple columns, an
// expression, NULL-equal dedup, WHERE, ORDER BY (by column and ordinal,
// ASC/DESC), LIMIT/OFFSET applied to the deduped (and, if present, ordered)
// set, a join, every storage class in one column, an empty result, and a
// multi-page table.
var vdbeDistinctCorpus = []subCase{
	// --- single column, multiple columns, an expression ---
	{"SELECT DISTINCT a FROM dt", false},
	{"SELECT DISTINCT a, b FROM dt", false},
	{"SELECT DISTINCT a, b, c FROM dt", false},
	{"SELECT DISTINCT a + 1 FROM dt", false},
	{"SELECT DISTINCT a > 1 FROM dt", false},
	{"SELECT DISTINCT upper(b) FROM dt", false},

	// --- NULLs: NULL-equal dedup, including the all-NULL row ---
	{"SELECT DISTINCT a FROM dt WHERE a IS NULL OR a = 1", false},
	{"SELECT DISTINCT a, b FROM dt WHERE a IS NULL", false}, // (NULL,NULL) x2 and (NULL,'z') -> 2 rows

	// --- DISTINCT + WHERE ---
	{"SELECT DISTINCT a FROM dt WHERE a IS NOT NULL", false},
	{"SELECT DISTINCT a, b FROM dt WHERE c > 1", false},

	// --- DISTINCT + ORDER BY: by column name, by ordinal, ASC and DESC ---
	{"SELECT DISTINCT a FROM dt ORDER BY a", true},
	{"SELECT DISTINCT a FROM dt ORDER BY a DESC", true},
	{"SELECT DISTINCT a, b FROM dt ORDER BY a, b", true},
	{"SELECT DISTINCT a, b FROM dt ORDER BY 2, 1", true},
	{"SELECT DISTINCT a, b FROM dt ORDER BY 1 DESC, 2 DESC", true},

	// --- DISTINCT + LIMIT/OFFSET, with and without ORDER BY (the
	// ORDER-BY-less compileScanPlain path and the sorted compileScanSorted
	// path both apply LIMIT/OFFSET to the DEDUPED set, never the raw scan) ---
	{"SELECT DISTINCT a FROM dt LIMIT 2", false},
	{"SELECT DISTINCT a FROM dt ORDER BY a LIMIT 2", true},
	{"SELECT DISTINCT a FROM dt ORDER BY a LIMIT 2 OFFSET 1", true},
	{"SELECT DISTINCT a FROM dt ORDER BY a LIMIT 0", true},
	{"SELECT DISTINCT a FROM dt ORDER BY a LIMIT -1 OFFSET 1", true},

	// --- DISTINCT over a join: duplicate combinations on both the comma-join
	// and explicit-JOIN forms collapse; a NULL join key never matches. ---
	{"SELECT DISTINCT jt1.k FROM jt1, jt2 WHERE jt1.k = jt2.k", false},
	{"SELECT DISTINCT jt1.k FROM jt1 JOIN jt2 ON jt1.k = jt2.k", false},
	{"SELECT DISTINCT jt1.k FROM jt1 JOIN jt2 ON jt1.k = jt2.k ORDER BY jt1.k", true},
	{"SELECT DISTINCT jt1.k, jt2.val FROM jt1 JOIN jt2 ON jt1.k = jt2.k", false},
	{"SELECT DISTINCT jt2.val FROM jt1 LEFT JOIN jt2 ON jt1.k = jt2.k", false}, // includes the NULL-extended row for jt1.k=NULL

	// --- every storage class (and NULL) in one column ---
	{"SELECT DISTINCT m FROM mx", false},
	{"SELECT DISTINCT m FROM mx ORDER BY m", true},
	{"SELECT DISTINCT typeof(m) FROM mx", false},

	// --- empty result ---
	{"SELECT DISTINCT x, y FROM empty_d", false},
	{"SELECT DISTINCT a FROM dt WHERE a > 100000", false},

	// --- multi-page table ---
	{"SELECT DISTINCT tag FROM big", false},
	{"SELECT DISTINCT tag FROM big ORDER BY tag", true},
	{"SELECT DISTINCT tag FROM big ORDER BY tag DESC LIMIT 3", true},

	// --- DISTINCT + GROUP BY (single table): the grouped rows themselves are
	// deduped by their select-list OUTPUT tuple, exactly like row-mode
	// DISTINCT, but over the finalized/HAVING-filtered group rows rather than
	// raw scan rows -- see compileScanGroupBy's doc comment
	// (engine/vdbe_agg_codegen.go) for why this compiles the whole group set
	// as one batch instead of emitting groups one at a time. "big" GROUP BY
	// tag already has one row per distinct tag value, so DISTINCT is a
	// structural no-op there; "dt" GROUP BY a, b collapses groups that tie on
	// "a" alone when SELECT DISTINCT a is combined with GROUP BY a, b. ---
	{"SELECT DISTINCT tag FROM big GROUP BY tag", false},
	{"SELECT DISTINCT tag FROM big GROUP BY tag ORDER BY tag", true},
	{"SELECT DISTINCT a FROM dt GROUP BY a, b", false},
	{"SELECT DISTINCT a FROM dt GROUP BY a, b ORDER BY a", true},
	{"SELECT DISTINCT a FROM dt GROUP BY a, b ORDER BY a DESC LIMIT 2", true},
	{"SELECT DISTINCT count(*) FROM dt GROUP BY a, b", false},
	{"SELECT DISTINCT a FROM dt GROUP BY a, b HAVING count(*) >= 1", false},

	// --- DISTINCT + GROUP BY over a JOIN: now compiles over any join (see
	// engine/vdbe_agg_codegen.go's compileScanGroupBy doc comment and
	// TestVDBEDistinctStillFallsBack's updated comment below). jt1/jt2's
	// duplicate join-key rows (k=10 matches twice on each side) produce
	// duplicate groups for DISTINCT to collapse. ---
	{"SELECT DISTINCT jt1.k FROM jt1, jt2 WHERE jt1.k = jt2.k GROUP BY jt1.k, jt2.k", false},
	{"SELECT DISTINCT jt1.k FROM jt1 JOIN jt2 ON jt1.k = jt2.k GROUP BY jt1.k, jt2.k", false},
	{"SELECT DISTINCT jt1.k FROM jt1 JOIN jt2 ON jt1.k = jt2.k GROUP BY jt1.k, jt2.k ORDER BY jt1.k", true},
	{"SELECT DISTINCT jt1.k FROM jt1 JOIN jt2 ON jt1.k = jt2.k GROUP BY jt1.k, jt2.k HAVING count(*) > 0 ORDER BY jt1.k LIMIT 1", true},
}

// TestVDBEDistinctResultParity is the hard gate: every corpus statement must
// produce identical results through the VDBE and C SQLite.
func TestVDBEDistinctResultParity(t *testing.T) {
	path := buildVDBEDistinctDB(t)

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
	for _, tc := range vdbeDistinctCorpus {
		total++
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error (a compilable DISTINCT query must not decline)\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)

		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("VDBE DISTINCT result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE DISTINCT parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEDistinctIntegratedQueryPath runs the whole DISTINCT corpus through
// the ordinary QueryArgs entry point -- the same code path a real driver
// connection uses -- rather than through QueryVDBE directly, so a statement the
// gate above serves but the integrated path does not is still caught.
func TestVDBEDistinctIntegratedQueryPath(t *testing.T) {
	path := buildVDBEDistinctDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeDistinctCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] QueryArgs: %v", tc.sql, err)
		}
	}
}

// Note: this file previously had a TestVDBEDistinctStillFallsBack pinning
// SELECT DISTINCT combined with its own GROUP BY over more than one joined
// table as a compile-time decline. Reproducing which of several groups
// sharing an output tuple survives the dedup needs a first-seen-scan-order
// proxy, which used to be a single table's minimum rowid (only valid for a
// single-table scan). That proxy is now a genuine scan-order sequence number
// (see compileScanGroupBy's doc comment, engine/vdbe_agg_codegen.go), valid
// over any join, so this shape compiles and runs on the VDBE now too -- see
// vdbeDistinctCorpus's "DISTINCT + GROUP BY over a JOIN" block above, which
// covers it as an ordinary passing case instead.
