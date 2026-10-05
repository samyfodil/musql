package compat

// Tests VDBE single-table cursor-driven SELECT compilation.
//
//	(a) the VDBE            -- (*engine.ReadOnlyPager).QueryVDBE
//	(b) C SQLite       -- github.com/mattn/go-sqlite3 (the ground-truth oracle)

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEScanDB drives the pure-Go engine writer to build a small database
// covering the shapes the gate below needs: a plain (no INTEGER PRIMARY KEY,
// auto-assigned rowid) table with NULLs and every storage class, an INTEGER
// PRIMARY KEY table (rowid aliasing), an empty table, and a many-row table
// (at a small page size, to force a real multi-page/interior-node scan, not
// just a single leaf).
func buildVDBEScanDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vdbe_scan.sqlite")
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

	// plain: no IPK -- rowid auto-assigned 1,2,3,... NULLs and every storage
	// class (integer, real, text, blob, null), including a signed-zero REAL.
	exec(`CREATE TABLE plain (a INTEGER, b TEXT, c REAL)`)
	exec(`INSERT INTO plain VALUES (1, 'one', 1.5)`)
	exec(`INSERT INTO plain VALUES (2, 'two', NULL)`)
	exec(`INSERT INTO plain VALUES (NULL, NULL, 3.5)`)
	exec(`INSERT INTO plain VALUES (4, 'four', -0.0)`)
	exec(`INSERT INTO plain VALUES (5, x'deadbeef', 5.5)`)
	exec(`INSERT INTO plain VALUES (-3, 'neg', -2.25)`)

	// ipk: INTEGER PRIMARY KEY, shuffled explicit rowids -- exercises the
	// rowid/IPK-alias substitution over a real b-tree scan (not just the
	// single synthetic row the FROM-less path has).
	exec(`CREATE TABLE ipk (id INTEGER PRIMARY KEY, name TEXT)`)
	exec(`INSERT INTO ipk VALUES (100, 'a')`)
	exec(`INSERT INTO ipk VALUES (5, 'b')`)
	exec(`INSERT INTO ipk VALUES (50, 'c')`)
	exec(`INSERT INTO ipk VALUES (7, NULL)`)

	// empty: genuinely zero rows.
	exec(`CREATE TABLE empty_t (x INTEGER)`)

	// big: enough rows, at a 512-byte page size, to force multiple leaf pages
	// and grow an interior b-tree level -- a real multi-page scan.
	exec(`CREATE TABLE big (id INTEGER PRIMARY KEY, v INTEGER, tag TEXT)`)
	for i := 1; i <= 400; i++ {
		exec(fmt.Sprintf(`INSERT INTO big VALUES (%d, %d, 'tag-%d')`, i, i*i, i%7))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeScanCorpus is a broad set of single-table SELECTs exercising: "*" and
// explicit column lists, computed/function select-list items, WHERE
// (comparisons, AND/OR, NULL, IN(list), BETWEEN, LIKE), WHERE/SELECT on the
// rowid pseudo-column (both a plain table's auto-rowid and an INTEGER PRIMARY
// KEY table's real alias), a table qualifier/alias, LIMIT, LIMIT+OFFSET,
// LIMIT 0, an empty table, an all-filtered-out WHERE, NULLs/every storage
// class, and a multi-page table.
var vdbeScanCorpus = []string{
	"SELECT * FROM plain",
	"SELECT a, b FROM plain",
	"SELECT a+c, upper(b) FROM plain",
	"SELECT rowid, a, b, c FROM plain",
	"SELECT * FROM plain WHERE a > 1",
	"SELECT * FROM plain WHERE a > 1 AND c < 10",
	"SELECT * FROM plain WHERE a > 1 OR b = 'two'",
	"SELECT * FROM plain WHERE a IS NULL",
	"SELECT * FROM plain WHERE a IS NOT NULL",
	"SELECT * FROM plain WHERE a IN (1,2,4)",
	"SELECT * FROM plain WHERE a NOT IN (1,2,4)",
	"SELECT * FROM plain WHERE c BETWEEN 1 AND 4",
	"SELECT * FROM plain WHERE c NOT BETWEEN 1 AND 4",
	"SELECT * FROM plain WHERE b LIKE '%o%'",
	"SELECT * FROM plain WHERE b NOT LIKE '%o%'",
	"SELECT * FROM plain WHERE rowid = 2",
	"SELECT * FROM plain WHERE rowid > 3",
	"SELECT p.a, p.rowid FROM plain p WHERE p.a > 0",
	"SELECT * FROM plain LIMIT 2",
	"SELECT * FROM plain LIMIT 2 OFFSET 1",
	"SELECT * FROM plain LIMIT 0",
	"SELECT * FROM plain LIMIT -1",
	"SELECT * FROM plain WHERE a > 1000",         // all filtered out
	"SELECT * FROM plain WHERE a > 1000 LIMIT 5", // all filtered out + LIMIT
	"SELECT * FROM empty_t",
	"SELECT * FROM empty_t LIMIT 5",
	"SELECT * FROM ipk",
	"SELECT rowid, name FROM ipk",
	"SELECT id, name FROM ipk WHERE id > 10",
	"SELECT * FROM ipk WHERE rowid = 50",
	"SELECT * FROM ipk WHERE id IS NULL", // id is the IPK -- never NULL
	"SELECT * FROM ipk WHERE name IS NULL",
	"SELECT * FROM big WHERE v > 100000 LIMIT 5",
	"SELECT * FROM big LIMIT 10 OFFSET 390",
	"SELECT id FROM big WHERE id BETWEEN 100 AND 110",
	"SELECT id, tag FROM big WHERE tag = 'tag-3' LIMIT 3",
	"SELECT * FROM big WHERE id = 1 OR id = 400",
	// SELECT DISTINCT: single/multiple columns, an expression, NULLs
	// (NULL-equal dedup), WHERE, ORDER BY (by column and by ordinal, ASC and
	// DESC), LIMIT/OFFSET applied to the deduped set, an empty table, and a
	// multi-page table. See vdbe_distinct_test.go for the dedicated gate.
	"SELECT DISTINCT a FROM plain",
	"SELECT DISTINCT b, c FROM plain",
	"SELECT DISTINCT a > 0 FROM plain",
	"SELECT DISTINCT a FROM plain WHERE a > 1",
	"SELECT DISTINCT a FROM plain ORDER BY a",
	"SELECT DISTINCT a FROM plain ORDER BY a DESC",
	"SELECT DISTINCT a FROM plain ORDER BY 1",
	"SELECT DISTINCT a FROM plain LIMIT 2",
	"SELECT DISTINCT a FROM plain ORDER BY a LIMIT 2 OFFSET 1",
	"SELECT DISTINCT * FROM empty_t",
	"SELECT DISTINCT tag FROM big",
	"SELECT DISTINCT tag FROM big ORDER BY tag",
}

// TestVDBEScanResultParity is the hard gate: every corpus statement must
// produce identical results through the VDBE and C SQLite.
func TestVDBEScanResultParity(t *testing.T) {
	path := buildVDBEScanDB(t)

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
	for _, sqlText := range vdbeScanCorpus {
		total++

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)

		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("VDBE scan result-parity gate: %d single-table SELECT statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE scan parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEScanFallback checks that a statement outside the VDBE compiler's
// current scope is cleanly reported as unsupported by QueryVDBE -- the same
// named reason tryVDBEScan (engine/vdbe_run.go) hands execSelect, which turns
// it into a hard error -- rather than executed wrong or panicking. Comma/CROSS/
// [INNER]/LEFT JOIN are now compiled by the VDBE (see vdbe_join_test.go), so
// those two shapes moved out of this list; SELECT DISTINCT (row-mode) is now
// also compiled by the VDBE (see vdbe_distinct_test.go), so it too moved out
// of this list; NATURAL JOIN and USING(...) are now supported too (see
// vdbe_join_using_test.go); RIGHT JOIN and FULL JOIN are ALSO now supported,
// in the "last FROM item" shape (see vdbe_join_outer_test.go for their own
// dedicated gate, and vdbe_join_using_test.go's
// TestVDBEJoinUsingStillFallsBackForRightFullMidChain / this package's
// TestOuterJoinStillFallsBack for the wider shapes that still fall back).
// TestVDBEScanFallback used to pin "SELECT * FROM plain JOIN ipk ON plain.a =
// (SELECT id FROM ipk LIMIT 1)" (a subquery inside a JOIN's ON condition) as
// declining cleanly. compileExpr now DOES
// compile a subquery inside an ON condition (emitJoinLevel's compileOn,
// vdbe_join_codegen.go, no longer preemptively declines it -- see that
// function's own doc comment for why it's safe), so this is now a positive
// QueryVDBE-vs-real-C-SQLite parity check instead -- see
// vdbe_join_gap_fix_test.go for this shape's own dedicated, wider gate.
func TestVDBEScanFallback(t *testing.T) {
	path := buildVDBEScanDB(t)

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

	const sqlText = "SELECT * FROM plain JOIN ipk ON plain.a = (SELECT id FROM ipk LIMIT 1)"

	vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
	if vErr != nil {
		t.Fatalf("QueryVDBE(%q): %v", sqlText, vErr)
	}
	cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
	if cErr != nil {
		t.Fatalf("cgo(%q): %v", sqlText, cErr)
	}
	vRows := engineRowsToStrings(vVals)
	if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
		t.Errorf("QueryVDBE(%q) DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
			sqlText, reason, vCols, vRows, cCols, cRows)
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
		t.Errorf("QueryArgs(%q): %v", sqlText, err)
	}
}

// TestVDBEScanDualModeIntegration drives the whole scan corpus through the
// ordinary QueryArgs entry point -- the same code path a real driver
// connection uses, reaching the compiler via tryVDBEScan (engine/vdbe_run.go)
// -- and asserts every statement in it still compiles and runs. There is no
// second executor to absorb a shape that stopped compiling, so a regression
// surfaces here as a hard error rather than a silent detour.
func TestVDBEScanDualModeIntegration(t *testing.T) {
	path := buildVDBEScanDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range vdbeScanCorpus {
		if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
			t.Errorf("[%s] QueryArgs: %v", sqlText, err)
		}
	}
}
