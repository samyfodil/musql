package compat

// Gate for VDBE aggregate and GROUP BY compilation, comparing against C SQLite.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEAggDB builds the tables the aggregate/GROUP BY gate needs: a broad
// grouping table with low-cardinality integer/text keys, NULLs scattered
// through both the grouping keys and the aggregated columns, every storage
// class (including a NONE-affinity "mixed" column), an INTEGER PRIMARY KEY
// (stable rowid for the ORDER BY min-rowid tiebreak); a genuinely empty table
// (empty-input semantics); and a many-row, multi-page table (a real multi-page
// grouped scan, not a single leaf).
func buildVDBEAggDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vdbe_agg.sqlite")
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

	exec(`CREATE TABLE g (id INTEGER PRIMARY KEY, k INTEGER, k2 TEXT, v INTEGER, r REAL, s TEXT, mixed)`)
	// k/k2 are the grouping keys (with NULLs and cross-type collisions); v/r/s
	// are aggregated (with NULLs); mixed holds a different storage class per row.
	rows := []string{
		`INSERT INTO g VALUES (1, 1, 'a', 10, 1.5, 'apple',  7)`,
		`INSERT INTO g VALUES (2, 1, 'a', 20, 2.5, 'avocado', 'hi')`,
		`INSERT INTO g VALUES (3, 2, 'b', 5,  NULL, NULL,    2.0)`,
		`INSERT INTO g VALUES (4, 2, 'b', 5,  3.5, 'berry',  x'ab')`,
		`INSERT INTO g VALUES (5, 3, NULL, NULL, -1.0, 'cherry', NULL)`,
		`INSERT INTO g VALUES (6, 1, 'a', 30, 9.0, 'apricot', 100)`,
		`INSERT INTO g VALUES (7, NULL, 'b', 7, 0.0, 'blueberry', -3.25)`,
		`INSERT INTO g VALUES (8, 2, 'b', NULL, 4.5, NULL,   'x')`,
		`INSERT INTO g VALUES (9, NULL, NULL, 1, NULL, 'z', NULL)`,
		`INSERT INTO g VALUES (10, 3, 'c', 100, 100.0, 'zucchini', 0)`,
	}
	for _, r := range rows {
		exec(r)
	}

	exec(`CREATE TABLE e (x INTEGER, k INTEGER, v INTEGER)`) // genuinely empty

	// Many rows, at a 512-byte page size, to force a multi-page grouped scan.
	exec(`CREATE TABLE big (id INTEGER PRIMARY KEY, bucket INTEGER, v INTEGER)`)
	for i := 1; i <= 400; i++ {
		exec(fmt.Sprintf(`INSERT INTO big VALUES (%d, %d, %d)`, i, i%7, i))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// aggCase is one gate statement plus whether its output row order is
// significant (true only when it has an ORDER BY that totally orders the rows).
type aggCase struct {
	sql     string
	ordered bool
}

var vdbeAggCorpus = []aggCase{
	// --- whole-table aggregates (no GROUP BY): always exactly one row ---
	{"SELECT count(*) FROM g", false},
	{"SELECT count(v), count(*), count(k) FROM g", false},
	{"SELECT sum(v), avg(v), min(v), max(v), total(v) FROM g", false},
	{"SELECT sum(r), avg(r), min(r), max(r) FROM g", false},
	{"SELECT min(s), max(s), group_concat(s) FROM g", false},
	{"SELECT group_concat(s, '|') FROM g", false},
	{"SELECT count(DISTINCT k), sum(DISTINCT v), count(DISTINCT mixed) FROM g", false},
	{"SELECT count(*)+1, sum(v)*2, coalesce(max(k),-1) FROM g WHERE v > 5", false},
	{"SELECT min(mixed), max(mixed), count(mixed) FROM g", false},
	{"SELECT count(*) FROM g WHERE k IS NULL", false},
	{"SELECT count(*) FROM g WHERE 1=0", false},       // all filtered -> count 0, one row
	{"SELECT sum(v), max(v) FROM g WHERE 1=0", false}, // all filtered -> NULLs, one row
	// whole-table aggregate over an empty table: still exactly one row.
	{"SELECT count(*), count(v), sum(v), avg(v), min(v), max(v), total(v), group_concat(v) FROM e", false},
	{"SELECT count(*)*10 + 1 FROM e", false},
	// whole-table aggregate with LIMIT/OFFSET on the single row.
	{"SELECT count(*) FROM g LIMIT 0", false},
	{"SELECT count(*) FROM g LIMIT 1", false},
	{"SELECT count(*) FROM g LIMIT 1 OFFSET 1", false},
	// multi-page table.
	{"SELECT count(*), sum(v), avg(v), min(v), max(v) FROM big", false},

	// --- GROUP BY (order-insensitive; compared as a set) ---
	{"SELECT k, count(*) FROM g GROUP BY k", false},
	{"SELECT k, count(*), sum(v), avg(v), min(v), max(v), total(v) FROM g GROUP BY k", false},
	{"SELECT k2, count(*), group_concat(s) FROM g GROUP BY k2", false},
	{"SELECT k, k2, count(*), sum(v) FROM g GROUP BY k, k2", false},
	{"SELECT k, count(DISTINCT v) FROM g GROUP BY k", false},
	{"SELECT mixed, count(*) FROM g GROUP BY mixed", false},
	{"SELECT k, count(*) FROM g WHERE v IS NOT NULL GROUP BY k", false},
	{"SELECT k, count(*) FROM g GROUP BY k HAVING count(*) > 1", false},
	{"SELECT k, sum(v) FROM g GROUP BY k HAVING sum(v) > 20", false},
	{"SELECT k, count(*) FROM g GROUP BY k HAVING count(*) > 1 AND max(v) > 10", false},
	{"SELECT k2, count(*) FROM g GROUP BY k2 HAVING k2 IS NOT NULL", false},
	{"SELECT k || '!', count(*) FROM g WHERE k IS NOT NULL GROUP BY k", false},
	// empty input + GROUP BY -> zero rows.
	{"SELECT k, count(*) FROM e GROUP BY k", false},
	{"SELECT k, count(*), sum(v) FROM e GROUP BY k HAVING count(*) > 0", false},
	// multi-page grouped scan.
	{"SELECT bucket, count(*), sum(v), min(v), max(v) FROM big GROUP BY bucket", false},

	// --- GROUP BY + ORDER BY (total order: a grouping key is the final term) ---
	{"SELECT k, count(*) FROM g GROUP BY k ORDER BY k", true},
	{"SELECT k, count(*) FROM g GROUP BY k ORDER BY k DESC", true},
	{"SELECT k, count(*) FROM g GROUP BY k ORDER BY count(*) DESC, k", true},
	{"SELECT k, sum(v) FROM g GROUP BY k ORDER BY sum(v), k", true},
	{"SELECT k, k2, count(*) FROM g GROUP BY k, k2 ORDER BY k, k2", true},
	{"SELECT k2, count(*) FROM g GROUP BY k2 ORDER BY count(*) DESC, k2", true},
	// GROUP BY + ORDER BY + LIMIT/OFFSET.
	{"SELECT k, count(*) FROM g GROUP BY k ORDER BY k LIMIT 2", true},
	{"SELECT k, count(*) FROM g GROUP BY k ORDER BY count(*) DESC, k LIMIT 2 OFFSET 1", true},
	{"SELECT k, sum(v) FROM g GROUP BY k ORDER BY k DESC LIMIT 1", true},
	{"SELECT bucket, count(*) FROM big GROUP BY bucket ORDER BY count(*) DESC, bucket LIMIT 3", true},
	// ORDER BY an output alias / ordinal.
	{"SELECT k, count(*) AS n FROM g GROUP BY k ORDER BY n DESC, k", true},
	{"SELECT k, count(*) FROM g GROUP BY k ORDER BY 2 DESC, 1", true},

	// --- GROUP BY + DISTINCT (single table): the finalized, HAVING-filtered
	// group rows are deduped by their select-list OUTPUT tuple -- collapsing
	// groups that tie on the DISTINCT-ed expression even though they differ
	// on the rest of the GROUP BY key (k2 here) -- exactly like
	// sql_group.go's queryGroupBy itself, reproduced via the batch-collect
	// path in compileScanGroupBy/vdbe_group_distinct.go rather than emitted
	// incrementally. See the GROUP BY + DISTINCT over a JOIN block further
	// below for the multi-table version of this same shape. ---
	{"SELECT DISTINCT k FROM g GROUP BY k, k2", false},
	{"SELECT DISTINCT k FROM g GROUP BY k, k2 ORDER BY k", true},
	{"SELECT DISTINCT k FROM g GROUP BY k, k2 ORDER BY k DESC", true},
	{"SELECT DISTINCT k FROM g GROUP BY k, k2 HAVING count(*) > 0", false},
	{"SELECT DISTINCT count(*) FROM g GROUP BY k, k2", false},
	{"SELECT DISTINCT k FROM g GROUP BY k, k2 ORDER BY k LIMIT 2", true},
	{"SELECT DISTINCT k FROM g GROUP BY k, k2 ORDER BY k LIMIT 2 OFFSET 1", true},
	{"SELECT DISTINCT bucket % 2 FROM big GROUP BY bucket", false},
	// empty input + GROUP BY + DISTINCT -> zero rows.
	{"SELECT DISTINCT k FROM e GROUP BY k", false},

	// --- GROUP BY + ORDER BY over a JOIN (2 and 3 tables): the drain's
	// first-seen-scan-order tiebreak (curMinSeqReg, a genuine scan-order
	// sequence number -- see compileScanGroupBy's doc comment,
	// engine/vdbe_agg_codegen.go) is now valid over any join, not just a
	// single table. g self-joined on k produces duplicate combinations (k=1
	// has two matching rows on each side, k=2 has two, k=3 one, k=NULL never
	// matches), exercising real ties. ---
	{"SELECT g.k, count(*) FROM g, g AS g2 WHERE g.k = g2.k GROUP BY g.k ORDER BY g.k", true},
	{"SELECT g.k, count(*) FROM g JOIN g AS g2 ON g.k = g2.k GROUP BY g.k ORDER BY count(*) DESC, g.k", true},
	{"SELECT g.k, count(*) FROM g JOIN g AS g2 ON g.k = g2.k GROUP BY g.k ORDER BY g.k LIMIT 1", true},
	{"SELECT g.k, count(*) FROM g LEFT JOIN g AS g2 ON g.k = g2.k GROUP BY g.k ORDER BY g.k", true},
	{"SELECT g.k, g2.k2, count(*) FROM g, g AS g2 WHERE g.k = g2.k GROUP BY g.k, g2.k2 ORDER BY g.k, g2.k2", true},
	{"SELECT g.k, count(*) FROM g, g AS g2, g AS g3 WHERE g.k = g2.k AND g2.k = g3.k GROUP BY g.k ORDER BY g.k", true},
	{"SELECT g.k, count(*) FROM g, g AS g2, g AS g3 WHERE g.k = g2.k AND g2.k = g3.k GROUP BY g.k ORDER BY count(*) DESC, g.k LIMIT 2 OFFSET 1", true},

	// --- SELECT DISTINCT + GROUP BY over a JOIN (2 and 3 tables): the
	// batch-collect+dedup path (vdbe_group_distinct.go's groupBatchFinal) is
	// likewise now valid over any join. ---
	{"SELECT DISTINCT g.k FROM g, g AS g2 WHERE g.k = g2.k GROUP BY g.k, g2.k", false},
	{"SELECT DISTINCT g.k FROM g JOIN g AS g2 ON g.k = g2.k GROUP BY g.k, g2.k ORDER BY g.k", true},
	{"SELECT DISTINCT g.k FROM g JOIN g AS g2 ON g.k = g2.k GROUP BY g.k, g2.k ORDER BY g.k DESC LIMIT 1", true},
	{"SELECT DISTINCT g.k FROM g, g AS g2 WHERE g.k = g2.k GROUP BY g.k, g2.k HAVING count(*) > 0", false},
	{"SELECT DISTINCT g.k FROM g LEFT JOIN g AS g2 ON g.k = g2.k GROUP BY g.k, g2.k", false},
	{"SELECT DISTINCT g.k FROM g, g AS g2, g AS g3 WHERE g.k = g2.k AND g2.k = g3.k GROUP BY g.k, g2.k, g3.k", false},
	{"SELECT DISTINCT g.k FROM g, g AS g2, g AS g3 WHERE g.k = g2.k AND g2.k = g3.k GROUP BY g.k, g2.k, g3.k ORDER BY g.k LIMIT 2", true},
}

// TestVDBEAggResultParity is the hard gate: every corpus statement must produce
// identical results through the VDBE and C SQLite.
func TestVDBEAggResultParity(t *testing.T) {
	path := buildVDBEAggDB(t)

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
	for _, tc := range vdbeAggCorpus {
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
	t.Logf("VDBE aggregate/GROUP BY result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE aggregate parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEAggDualModeIntegration exercises the compile-and-run path
// (tryVDBEScan, engine/vdbe_run.go) through the ordinary QueryArgs entry point
// for the whole aggregate corpus, confirming every statement in it runs there
// and not only through the explicit QueryVDBE entry point the gate above uses.
func TestVDBEAggDualModeIntegration(t *testing.T) {
	path := buildVDBEAggDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeAggCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] VDBEMode=dual: %v", tc.sql, err)
		}
	}
}

// TestVDBEAggStillFallsBack pins the one aggregate-adjacent shape that must
// still produce an error (not a compile decline -- see below): a GROUP BY
// referencing an unqualified column name present in more than one joined
// table (both g and e have a column named k) is a genuine "ambiguous column
// name" error C SQLite raises too, not a "multi-table
// GROUP BY is unsupported" limitation -- comma/CROSS/[INNER]/LEFT-joined
// GROUP BY, with or without its own ORDER BY, and SELECT DISTINCT combined
// with GROUP BY, all now compile and run on the VDBE over any join (see
// vdbeAggCorpus's GROUP BY + ORDER BY / DISTINCT + GROUP BY over a JOIN
// blocks above, and compileScanGroupBy's doc comment, engine/vdbe_agg_
// codegen.go); this statement merely happens to also be invalid SQL on its
// own terms, independent of how it is run. QueryVDBE must report an
// error rather than run wrong.
func TestVDBEAggStillFallsBack(t *testing.T) {
	path := buildVDBEAggDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		"SELECT k, count(*) FROM g, e GROUP BY k", // ambiguous column "k" (both g.k and e.k exist) -> real error
	} {
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("QueryVDBE(%q): expected an unsupported/compile error, got nil", sqlText)
		}
	}
}
