package compat

// VDBE multi-table JOIN compiler correctness gate: comma/CROSS/INNER/LEFT
// joins entirely in bytecode, with WHERE, ORDER BY, GROUP BY, LIMIT/OFFSET.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEJoinDB builds test tables: t1/t2/t3 with duplicates, NULLs,
// unmatched values, and empty_j; t4..t7 for deep joins.
func buildVDBEJoinDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_join.sqlite"
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

	exec(`CREATE TABLE t1 (id INTEGER PRIMARY KEY, a INTEGER, name TEXT)`)
	exec(`INSERT INTO t1 VALUES (1, 10, 'alice')`)
	exec(`INSERT INTO t1 VALUES (2, 20, 'bob')`)
	exec(`INSERT INTO t1 VALUES (3, NULL, 'carol')`) // NULL join key -- never matches
	exec(`INSERT INTO t1 VALUES (4, 20, 'dave')`)    // duplicate a=20 -> multiple matches
	exec(`INSERT INTO t1 VALUES (5, 30, 'eve')`)     // a=30 -- unmatched in t2

	exec(`CREATE TABLE t2 (id INTEGER PRIMARY KEY, a INTEGER, val TEXT)`)
	exec(`INSERT INTO t2 VALUES (1, 10, 'x')`)
	exec(`INSERT INTO t2 VALUES (2, 20, 'y')`)
	exec(`INSERT INTO t2 VALUES (3, 20, 'z')`)   // duplicate a=20 -> multiple matches
	exec(`INSERT INTO t2 VALUES (4, NULL, 'n')`) // NULL join key -- never matches
	exec(`INSERT INTO t2 VALUES (5, 40, 'w')`)   // a=40 -- unmatched in t1

	exec(`CREATE TABLE t3 (id INTEGER PRIMARY KEY, a INTEGER, tag TEXT)`)
	exec(`INSERT INTO t3 VALUES (1, 10, 'red')`)
	exec(`INSERT INTO t3 VALUES (2, 20, 'blue')`)
	exec(`INSERT INTO t3 VALUES (3, 30, 'green')`)

	exec(`CREATE TABLE empty_j (id INTEGER PRIMARY KEY, a INTEGER)`)

	exec(`CREATE TABLE t4 (id INTEGER PRIMARY KEY, a INTEGER, note TEXT)`)
	exec(`INSERT INTO t4 VALUES (1, 10, 'four-a')`)
	exec(`INSERT INTO t4 VALUES (2, 20, 'four-b')`)
	exec(`INSERT INTO t4 VALUES (3, 20, 'four-c')`) // duplicate a=20
	exec(`INSERT INTO t4 VALUES (4, 50, 'four-d')`) // a=50 -- unmatched elsewhere

	exec(`CREATE TABLE t5 (id INTEGER PRIMARY KEY, a INTEGER, note TEXT)`)
	exec(`INSERT INTO t5 VALUES (1, 10, 'five-a')`)
	exec(`INSERT INTO t5 VALUES (2, 20, 'five-b')`)
	exec(`INSERT INTO t5 VALUES (3, 60, 'five-c')`) // a=60 -- unmatched elsewhere

	exec(`CREATE TABLE t6 (id INTEGER PRIMARY KEY, a INTEGER, note TEXT)`)
	exec(`INSERT INTO t6 VALUES (1, 10, 'six-a')`)
	exec(`INSERT INTO t6 VALUES (2, 20, 'six-b')`)
	exec(`INSERT INTO t6 VALUES (3, 20, 'six-c')`) // duplicate a=20

	exec(`CREATE TABLE t7 (id INTEGER PRIMARY KEY, a INTEGER, note TEXT)`)
	exec(`INSERT INTO t7 VALUES (1, 10, 'sev-a')`)
	exec(`INSERT INTO t7 VALUES (2, 20, 'sev-b')`)
	exec(`INSERT INTO t7 VALUES (3, 70, 'sev-c')`) // a=70 -- unmatched elsewhere

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// joinCase is a statement plus whether its output row order is significant.
type joinCase struct {
	sql     string
	ordered bool
}

var vdbeJoinCorpus = []joinCase{
	{"SELECT t1.name, t2.val FROM t1, t2 WHERE t1.a = t2.a", false},
	{"SELECT t1.name, t2.val FROM t1 CROSS JOIN t2 WHERE t1.a = t2.a", false},
	{"SELECT t1.name, t2.val FROM t1, t2", false},
	{"SELECT * FROM t1, empty_j", false},
	{"SELECT * FROM empty_j, t1", false},

	{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a", false},
	{"SELECT t1.name, t2.val FROM t1 INNER JOIN t2 ON t1.a = t2.a", false},
	{"SELECT t1.name, t2.val FROM t1 CROSS JOIN t2 ON t1.a = t2.a", false},
	{"SELECT t1.name, empty_j.a FROM t1 JOIN empty_j ON t1.a = empty_j.a", false},

	{"SELECT t1.name, t2.val, t3.tag FROM t1 JOIN t2 ON t1.a = t2.a JOIN t3 ON t2.a = t3.a", false},
	{"SELECT t1.name, t2.val, t3.tag FROM t1, t2, t3 WHERE t1.a = t2.a AND t2.a = t3.a", false},

	{"SELECT t1.name, t2.val FROM t1 LEFT JOIN t2 ON t1.a = t2.a", false},
	{"SELECT t1.name, t2.id, t2.val FROM t1 LEFT JOIN t2 ON t1.a = t2.a ORDER BY t1.name, t2.id", true},
	{"SELECT t1.name, t2.val FROM t1 LEFT OUTER JOIN t2 ON t1.a = t2.a", false},
	{"SELECT * FROM t1 LEFT JOIN empty_j ON t1.a = empty_j.a", false},

	{"SELECT t1.name FROM t1 LEFT JOIN t2 ON t1.a = t2.a WHERE t2.id IS NULL", false},
	{"SELECT t1.name FROM t1 LEFT JOIN t2 ON t1.a = t2.a AND t2.val = 'y' WHERE t2.id IS NULL", false},

	{"SELECT x.name, y.name FROM t1 x JOIN t1 y ON x.a = y.a AND x.id <> y.id", false},
	{"SELECT x.name, y.name FROM t1 AS x, t1 AS y WHERE x.a = y.a AND x.id < y.id", false},

	{"SELECT t1.name, t2.val, t3.tag FROM t1 LEFT JOIN t2 ON t1.a = t2.a JOIN t3 ON t2.a = t3.a", false},
	{"SELECT t1.name, t2.val, t3.tag FROM t1 LEFT JOIN t2 ON t1.a = t2.a LEFT JOIN t3 ON t2.a = t3.a", false},

	{"SELECT t1.a, t2.a FROM t1 JOIN t2 ON t1.a = t2.a", false},
	{"SELECT x.name AS xn, y.name AS yn FROM t1 x JOIN t1 y ON x.a = y.a WHERE x.id <> y.id", false},

	{"SELECT t1.name, t2.val FROM t1, t2 WHERE t1.a = t2.a AND t2.val <> 'z'", false},
	{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a WHERE t1.name <> 'bob'", false},

	{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a ORDER BY t1.name, t2.val", true},
	{"SELECT t1.name, t2.val FROM t1, t2 WHERE t1.a = t2.a ORDER BY t2.val DESC, t1.name", true},

	{"SELECT count(*) FROM t1, t2 WHERE t1.a = t2.a", false},
	{"SELECT t1.a, count(*) FROM t1 JOIN t2 ON t1.a = t2.a GROUP BY t1.a", false},
	{"SELECT t1.a, count(*), group_concat(t2.val) FROM t1 JOIN t2 ON t1.a = t2.a GROUP BY t1.a", false},
	{"SELECT t1.name, count(*) FROM t1 LEFT JOIN t2 ON t1.a = t2.a GROUP BY t1.name", false},
	{"SELECT t1.a, count(*), group_concat(t2.val) FROM t1 JOIN t2 ON t1.a = t2.a GROUP BY t1.a ORDER BY t1.a", true},
	{"SELECT t1.name, count(*) FROM t1 LEFT JOIN t2 ON t1.a = t2.a GROUP BY t1.name ORDER BY t1.name", true},
	{"SELECT t1.a, count(*) FROM t1, t2, t3 WHERE t1.a = t2.a AND t2.a = t3.a GROUP BY t1.a ORDER BY count(*) DESC, t1.a", true},

	{"SELECT t1.name, t2.val FROM t1, t2 WHERE t1.a = t2.a ORDER BY t1.name, t2.val LIMIT 2", true},
	{"SELECT t1.name, t2.val FROM t1, t2 WHERE t1.a = t2.a ORDER BY t1.name, t2.val LIMIT 2 OFFSET 1", true},

	{"SELECT DISTINCT t1.a FROM t1, t2 WHERE t1.a = t2.a", false},
	{"SELECT DISTINCT t1.a FROM t1 JOIN t2 ON t1.a = t2.a", false},
	{"SELECT DISTINCT t1.a FROM t1 JOIN t2 ON t1.a = t2.a ORDER BY t1.a", true},
	{"SELECT DISTINCT t2.val FROM t1 LEFT JOIN t2 ON t1.a = t2.a", false},

	{"SELECT t1.name, t2.val FROM t1, t2 WHERE t1.a = t2.a", false},
	{"SELECT t1.name FROM t1 WHERE t1.a IS NULL", false},

	{"SELECT t1.rowid, t2.rowid FROM t1 JOIN t2 ON t1.a = t2.a", false},
	{"SELECT t1.name, t2.rowid FROM t1 LEFT JOIN t2 ON t1.a = t2.a ORDER BY t1.name", true},

	{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON true WHERE t1.a = t2.a", false},
	{"SELECT t1.id, t1.name, t2.val FROM t1 LEFT JOIN t2 ON false ORDER BY t1.id", true},
	{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a AND t2.a IN (SELECT a FROM t3) ORDER BY t1.id, t2.id", true},
	{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a AND t2.a = (SELECT MIN(a) FROM t3 WHERE t3.a >= t1.a) ORDER BY t1.id, t2.id", true},
}

// vdbeDeepJoinCorpus is 4-, 5-, 6-, 7-table joins with per-conjunct WHERE pushdown.
var vdbeDeepJoinCorpus = []joinCase{
	{"SELECT t1.name, t2.val, t3.tag, t4.note FROM t1, t2, t3, t4 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note FROM t1 JOIN t2 ON t1.a = t2.a JOIN t3 ON t2.a = t3.a JOIN t4 ON t3.a = t4.a", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note, t5.note FROM t1, t2, t3, t4, t5 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a AND t4.a = t5.a", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note, t5.note, t6.note FROM t1, t2, t3, t4, t5, t6 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a AND t4.a = t5.a AND t5.a = t6.a", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note, t5.note, t6.note FROM t1 JOIN t2 ON t1.a = t2.a JOIN t3 ON t2.a = t3.a JOIN t4 ON t3.a = t4.a JOIN t5 ON t4.a = t5.a JOIN t6 ON t5.a = t6.a", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note, t5.note, t6.note, t7.note FROM t1, t2, t3, t4, t5, t6, t7 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a AND t4.a = t5.a AND t5.a = t6.a AND t6.a = t7.a", false},
	{"SELECT t1.name, t7.note FROM t1 JOIN t2 ON t1.a = t2.a JOIN t3 ON t2.a = t3.a JOIN t4 ON t3.a = t4.a JOIN t5 ON t4.a = t5.a JOIN t6 ON t5.a = t6.a JOIN t7 ON t6.a = t7.a", false},
	{"SELECT t1.name, t2.val, t4.note FROM t1, t2, t3, t4 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a AND t1.name <> 'bob' AND t2.val <> 'z'", false},
	{"SELECT t1.name, t5.note FROM t1, t2, t3, t4, t5 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a AND t4.a = t5.a AND t1.a >= 10 AND t5.note IS NOT NULL", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note FROM t1 LEFT JOIN t2 ON t1.a = t2.a LEFT JOIN t3 ON t2.a = t3.a LEFT JOIN t4 ON t3.a = t4.a", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note, t5.note FROM t1 LEFT JOIN t2 ON t1.a = t2.a LEFT JOIN t3 ON t2.a = t3.a LEFT JOIN t4 ON t3.a = t4.a LEFT JOIN t5 ON t4.a = t5.a", false},
	{"SELECT t1.name, t3.tag FROM t1, t2 LEFT JOIN t3 ON t2.a = t3.a WHERE t1.a = t2.a AND t1.name <> 'carol'", false},
	{"SELECT t1.name, t3.id FROM t1, t2 LEFT JOIN t3 ON t2.a = t3.a WHERE t1.a = t2.a AND t3.id IS NULL", false},
	{"SELECT t1.name, t2.val, t3.tag, t4.note FROM t1, t2, t3, t4 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a ORDER BY t1.name, t4.note", true},
	{"SELECT t1.name, t2.val, t3.tag, t4.note, t5.note FROM t1, t2, t3, t4, t5 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a AND t4.a = t5.a ORDER BY t1.a, t1.name, t5.note", true},
	{"SELECT t1.a, count(*) FROM t1, t2, t3, t4 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a GROUP BY t1.a", false},
	{"SELECT count(*) FROM t1, t2, t3, t4, t5 WHERE t1.a = t2.a AND t2.a = t3.a AND t3.a = t4.a AND t4.a = t5.a", false},
}

// TestVDBEDeepJoinResultParity gates 4..7-table joins through VDBE; must
// produce identical results to C SQLite and finish fast via WHERE pushdown.
func TestVDBEDeepJoinResultParity(t *testing.T) {
	path := buildVDBEJoinDB(t)
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
	for _, tc := range vdbeDeepJoinCorpus {
		sqlText := tc.sql

		start := time.Now()
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		elapsed := time.Since(start)
		if vErr != nil {
			wrong++
			t.Errorf("[%s] QueryVDBE (must compile+run a wide join now): %v", sqlText, vErr)
			continue
		}
		if elapsed > 5*time.Second {
			wrong++
			t.Errorf("[%s] QueryVDBE took %v -- pushdown should keep a wide join fast (no cross-product blowup)", sqlText, elapsed)
		}

		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error\n  cgo=%v", sqlText, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("VDBE deep-join result-parity gate: %d statements, wrong=%d", len(vdbeDeepJoinCorpus), wrong)
	if wrong != 0 {
		t.Fatalf("VDBE deep-join result-parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEJoinResultParity gates every corpus statement; VDBE and C SQLite
// must produce identical results.
func TestVDBEJoinResultParity(t *testing.T) {
	path := buildVDBEJoinDB(t)
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
	for _, tc := range vdbeJoinCorpus {
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
	t.Logf("VDBE JOIN result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE JOIN parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEJoinDualModeIntegration runs the corpus through QueryArgs (not
// QueryVDBE direct); every statement must answer without error via the
// integrated entry point.
func TestVDBEJoinDualModeIntegration(t *testing.T) {
	path := buildVDBEJoinDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeJoinCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] integrated QueryArgs path: %v", tc.sql, err)
		}
	}
}

// TestVDBEJoinAmbiguousColumn checks that ambiguous column references error
// consistently across both engines.
func TestVDBEJoinAmbiguousColumn(t *testing.T) {
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

	const sqlText = "SELECT a FROM t1, t2"

	_, _, cErr := cgoSelect(t, cdb, sqlText, nil)
	if cErr == nil {
		t.Fatalf("[%s] expected C SQLite to reject an ambiguous column, got nil", sqlText)
	}
	if _, _, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
		t.Errorf("[%s] expected QueryVDBE to reject an ambiguous column, got nil", sqlText)
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Errorf("[%s] expected the integrated QueryArgs path to reject an ambiguous column, got nil", sqlText)
	}
}

// NOTE on a shape this file used to pin as still-declined:
// "SELECT * FROM t1 JOIN t2 ON t1.a = (SELECT a FROM t3 LIMIT 1)" (a
// subquery inside a JOIN's ON condition) used to be the sole case in a
// TestVDBEJoinStillFallsBack gate here. compileExpr now DOES compile a
// subquery inside an ON condition (emitJoinLevel's compileOn,
// vdbe_join_codegen.go, no longer preemptively declines it -- see that
// function's own doc comment for why it's safe: c.rowLive is already true
// and c.scopes is correctly narrowed to exactly the sources bound so far by
// the time the ON's subquery compiles), so that gate was removed; see
// vdbeJoinCorpus's own "bare TRUE/FALSE ... and a subquery ... inside an ON
// condition" block above (covering both QueryVDBE-direct AND the integrated
// QueryArgs path, via TestVDBEJoinResultParity/TestVDBEJoinDualModeIntegration)
// and vdbe_join_gap_fix_test.go for this shape's own dedicated coverage. A
// GROUP BY with its own trailing ORDER BY over more than one joined table now
// COMPILES (see vdbeJoinCorpus's "GROUP BY + its own ORDER BY, over a join"
// block above, and compileScanGroupBy's doc comment, vdbe_agg_codegen.go, for
// the genuine scan-order sequence number that made this join-safe). A join up
// to maxJoinTables (8) wide now COMPILES thanks to WHERE pushdown, so a
// 4-table join is no longer here -- see vdbeDeepJoinCorpus/
// TestVDBEDeepJoinResultParity for the wide-join parity sweep. SELECT
// DISTINCT across a join is compiled by the VDBE too (see
// vdbe_distinct_test.go). NATURAL JOIN and USING(...) are now supported (see
// vdbe_join_using_test.go for their own dedicated gate). RIGHT JOIN and FULL
// JOIN are also now supported, in the "last FROM item" shape -- see
// vdbe_join_outer_test.go for their own dedicated gate (result parity AND
// the still-unsupported wider shapes, e.g. RIGHT/FULL mid-chain).

// buildVDBEWideChainJoinDB builds an n-table chain-join database replicating
// sqllogictest's select5.test shape, with linked rows by id.
func buildVDBEWideChainJoinDB(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("vdbe_wide_chain_%d.sqlite", n))
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
	for i := 1; i <= n; i++ {
		exec(fmt.Sprintf(`CREATE TABLE wc%d (id INTEGER PRIMARY KEY, link INTEGER, label TEXT)`, i))
		for r := 1; r <= 10; r++ {
			link := "NULL"
			if i < n {
				link = fmt.Sprintf("%d", r)
			}
			exec(fmt.Sprintf(`INSERT INTO wc%d VALUES (%d, %s, 'wc%d-row%d')`, i, r, link, i, r))
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// wideChainJoinSQL builds an n-table chain-join query with links and anchor.
func wideChainJoinSQL(n int, anchor int) string {
	var sb strings.Builder
	sb.WriteString("SELECT ")
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "wc%d.label", i)
	}
	sb.WriteString(" FROM ")
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "wc%d", i)
	}
	sb.WriteString(" WHERE ")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&sb, "wc%d.link = wc%d.id AND ", i, i+1)
	}
	fmt.Fprintf(&sb, "wc1.id = %d", anchor)
	return sb.String()
}

// TestVDBEWideJoinTiming gates n-table joins (8, 16, 32, 64) for maxJoinTables:
// must compile, match C SQLite, and finish fast via WHERE pushdown.
func TestVDBEWideJoinTiming(t *testing.T) {
	for _, n := range []int{8, 16, 32, 64} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			path := buildVDBEWideChainJoinDB(t, n)

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

			sqlText := wideChainJoinSQL(n, 5)

			start := time.Now()
			vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
			elapsed := time.Since(start)
			if vErr != nil {
				t.Fatalf("[n=%d] QueryVDBE (must compile+run a %d-table join now that maxJoinTables=64): %v", n, n, vErr)
			}
			if elapsed > 5*time.Second {
				t.Errorf("[n=%d] QueryVDBE took %v -- pushdown should keep a select5.test-shaped chain join fast", n, elapsed)
			}

			cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
			if cErr != nil {
				t.Fatalf("[n=%d] unexpected error\n  cgo=%v", n, cErr)
			}

			vRows := engineRowsToStrings(vVals)
			if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
				t.Errorf("[n=%d] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
					n, reason, vCols, vRows, cCols, cRows)
			}
			t.Logf("[n=%d] QueryVDBE took %v", n, elapsed)
		})
	}
}

// TestVDBEJoinBeyondCapFallsBack pins the maxJoinTables ceiling (64):
// 65-table joins must decline cleanly on both engines.
func TestVDBEJoinBeyondCapFallsBack(t *testing.T) {
	path := buildVDBEWideChainJoinDB(t, 65)

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

	sqlText := wideChainJoinSQL(65, 5)

	if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
		t.Fatalf("QueryVDBE(%q...): expected C SQLite to also reject a 65-table join (sanity check), got nil", sqlText[:40])
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Errorf("QueryVDBE(%q...): expected an unsupported/compile error for a 65-table join, got nil", sqlText[:40])
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Errorf("QueryArgs(%q...): expected a decline for a 65-table join (no fallback), got nil", sqlText[:40])
	}
}
