package compat

// Tests VDBE compound SELECT compilation for UNION, UNION ALL, INTERSECT, and EXCEPT.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBECompoundDB creates test tables with overlapping rows, duplicates, and NULL rows.
func buildVDBECompoundDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_compound.sqlite"
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

	exec(`CREATE TABLE ca (a INTEGER, b TEXT)`)
	exec(`INSERT INTO ca (a,b) VALUES (1,'x')`)
	exec(`INSERT INTO ca (a,b) VALUES (1,'x')`) // exact duplicate within ca
	exec(`INSERT INTO ca (a,b) VALUES (2,NULL)`)
	exec(`INSERT INTO ca (a,b) VALUES (NULL,NULL)`)
	exec(`INSERT INTO ca (a,b) VALUES (NULL,NULL)`) // all-NULL duplicate within ca
	exec(`INSERT INTO ca (a,b) VALUES (3,'y')`)
	exec(`INSERT INTO ca (a,b) VALUES (4,'z')`) // only in ca, not cb

	exec(`CREATE TABLE cb (a INTEGER, b TEXT)`)
	exec(`INSERT INTO cb (a,b) VALUES (1,'x')`)
	exec(`INSERT INTO cb (a,b) VALUES (2,NULL)`)
	exec(`INSERT INTO cb (a,b) VALUES (5,'w')`) // only in cb, not ca
	exec(`INSERT INTO cb (a,b) VALUES (NULL,NULL)`)
	exec(`INSERT INTO cb (a,b) VALUES (3,'y')`)
	exec(`INSERT INTO cb (a,b) VALUES (3,'y')`) // exact duplicate within cb

	exec(`CREATE TABLE cc (a INTEGER, b TEXT)`)
	exec(`INSERT INTO cc (a,b) VALUES (4,'z')`)
	exec(`INSERT INTO cc (a,b) VALUES (6,'v')`)

	exec(`CREATE TABLE wide_a (a INTEGER)`)
	exec(`INSERT INTO wide_a (a) VALUES (1)`)
	exec(`CREATE TABLE wide_b (a INTEGER, b INTEGER)`)
	exec(`INSERT INTO wide_b (a,b) VALUES (1,2)`)

	// ou/oo: a two-table JOIN, for a compound arm that itself joins (the
	// same column shape as ca/cb -- INTEGER, TEXT -- so it combines cleanly
	// with them).
	exec(`CREATE TABLE ou (id INTEGER PRIMARY KEY, name TEXT)`)
	exec(`INSERT INTO ou (id,name) VALUES (1,'p')`)
	exec(`INSERT INTO ou (id,name) VALUES (2,'q')`)
	exec(`INSERT INTO ou (id,name) VALUES (3,'r')`)
	exec(`CREATE TABLE oo (uid INTEGER, tag TEXT)`)
	exec(`INSERT INTO oo (uid,tag) VALUES (1,'x')`)
	exec(`INSERT INTO oo (uid,tag) VALUES (2,'q')`)
	exec(`INSERT INTO oo (uid,tag) VALUES (99,'z')`) // no matching ou.id -- INNER JOIN drops it

	exec(`CREATE TABLE empty_c (a INTEGER, b TEXT)`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeCompoundCorpus is the compound-SELECT corpus: every statement compiles
// to bytecode (QueryVDBE must not decline it) and must agree with both
// oracles.
var vdbeCompoundCorpus = []subCase{
	// --- UNION ALL: plain concatenation, duplicates preserved. ---
	{"SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb ORDER BY a, b", true},

	// --- UNION: dedup across both operands, NULL-equal (the all-NULL row
	// appears twice in ca and once in cb -- must collapse to one). ---
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a, b", true},
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb", false},

	// --- INTERSECT: rows present in both, deduped -- including the all-NULL
	// row, present in both. ---
	{"SELECT a, b FROM ca INTERSECT SELECT a, b FROM cb ORDER BY a, b", true},

	// --- EXCEPT: not commutative. ---
	{"SELECT a, b FROM ca EXCEPT SELECT a, b FROM cb ORDER BY a, b", true},
	{"SELECT a, b FROM cb EXCEPT SELECT a, b FROM ca ORDER BY a, b", true},

	// --- Single-column projection: NULL-equal dedup still applies. ---
	{"SELECT b FROM ca UNION SELECT b FROM cb ORDER BY b", true},

	// --- Three-arm chains, left-to-right associativity (never re-grouped). ---
	{"SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb EXCEPT SELECT a, b FROM cc ORDER BY a, b", true},
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb EXCEPT SELECT a, b FROM cc ORDER BY a, b", true},
	{"SELECT a, b FROM ca EXCEPT SELECT a, b FROM cb UNION SELECT a, b FROM cc ORDER BY a, b", true},
	{"SELECT a, b FROM ca INTERSECT SELECT a, b FROM cb UNION ALL SELECT a, b FROM cc ORDER BY a, b", true},

	// --- ORDER BY on a compound: by name, by ordinal, ASC/DESC. ---
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a", true},
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a DESC, b ASC", true},
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY 1, 2", true},
	{"SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb ORDER BY a, b", true},
	{"SELECT a AS x, b FROM ca UNION SELECT a, b FROM cb ORDER BY x, b", true},

	// --- LIMIT/OFFSET applied to the combined (and, if present, ordered)
	// result -- never to a single arm. ---
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a, b LIMIT 3", true},
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a, b LIMIT 3 OFFSET 2", true},
	{"SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb ORDER BY a, b LIMIT 5", true},
	{"SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a, b LIMIT 0", true},

	// --- Aggregate arms: each arm is its own one-row aggregate result. ---
	{"SELECT count(*) FROM ca UNION SELECT count(*) FROM cb", false},
	{"SELECT count(*) FROM ca UNION ALL SELECT count(*) FROM cb ORDER BY 1", true},

	// --- A JOIN arm (INNER JOIN, no matching row for oo.uid=99 is dropped)
	// combined with a plain arm: exercises compileSubProgram routing an
	// arm through the join codegen (vdbe_join_codegen.go), not just the
	// single-table scan compiler. ---
	{"SELECT ou.id, ou.name FROM ou JOIN oo ON oo.uid = ou.id UNION SELECT a, b FROM ca ORDER BY 1, 2", true},
	{"SELECT ou.id, ou.name FROM ou JOIN oo ON oo.uid = ou.id UNION ALL SELECT a, b FROM ca ORDER BY 1, 2", true},
	{"SELECT ou.id, ou.name FROM ou JOIN oo ON oo.uid = ou.id INTERSECT SELECT a, b FROM ca", false},

	// --- GROUP BY arm combined with another GROUP BY arm (same column
	// shape: a grouping column and an aggregate). Ordered by BOTH columns
	// (not just a): ca and cb each contribute their own a=NULL group, and
	// UNION's row-level dedup does not collapse them (their counts differ),
	// so "a" alone is a tie between the two surviving NULL rows -- their
	// relative order is otherwise implementation-defined (see
	// vdbe_sort_test.go's own tie-stability caveat), which the ordinal
	// "ORDER BY a, 2" resolves deterministically (a compound's ORDER BY only
	// supports a result-column position or bare name -- "count(*)" is
	// neither, see resolveCompoundOrderIndex). ---
	{"SELECT a, count(*) FROM ca GROUP BY a UNION SELECT a, count(*) FROM cb GROUP BY a ORDER BY a, 2", true},

	// --- WHERE narrows each arm before combining. ---
	{"SELECT a, b FROM ca WHERE a > 1 UNION SELECT a, b FROM cb WHERE a > 1 ORDER BY a, b", true},

	// --- A DISTINCT arm: compileSelectScan's row-mode DISTINCT support
	// (vdbe_scan.go/vdbe_sort_codegen.go) applies within a single arm just
	// like a top-level statement -- ca's own exact-duplicate (1,'x') row
	// collapses before the UNION with cb even sees it. ---
	{"SELECT DISTINCT a FROM ca UNION SELECT a FROM cb ORDER BY a", true},
	{"SELECT DISTINCT a, b FROM ca UNION ALL SELECT a, b FROM cb ORDER BY a, b", true},

	// --- A FROM-less (constant) arm combined with a table-backed arm. ---
	{"SELECT 4, 'z' UNION SELECT a, b FROM ca ORDER BY 1, 2", true},
	{"SELECT 99, 'q' UNION ALL SELECT a, b FROM ca ORDER BY 1, 2", true},

	// --- Empty arms. ---
	{"SELECT a, b FROM ca UNION SELECT a, b FROM empty_c ORDER BY a, b", true},
	{"SELECT a, b FROM empty_c UNION SELECT a, b FROM ca ORDER BY a, b", true},
	{"SELECT a, b FROM ca INTERSECT SELECT a, b FROM empty_c", false},
	{"SELECT a, b FROM ca EXCEPT SELECT a, b FROM empty_c ORDER BY a, b", true},
	{"SELECT a, b FROM empty_c EXCEPT SELECT a, b FROM ca", false},
	{"SELECT a, b FROM empty_c UNION ALL SELECT a, b FROM empty_c", false},
}

// TestVDBECompoundResultParity is the hard gate: every corpus statement must
// produce identical results through the VDBE and C SQLite.
func TestVDBECompoundResultParity(t *testing.T) {
	path := buildVDBECompoundDB(t)

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
	for _, tc := range vdbeCompoundCorpus {
		total++
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error (a compilable compound must not decline)\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)

		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("VDBE compound result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE compound parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBECompoundDualModeIntegration exercises the VDBEMode=dual integration
// path (tryVDBECompound, engine/vdbe_run.go) through the ordinary QueryArgs
// entry point for the whole corpus, confirming every statement the direct
// QueryVDBE gate above proves correct also runs clean on the driver path --
// where a compile decline would surface as an error, not as a fallback.
func TestVDBECompoundDualModeIntegration(t *testing.T) {
	path := buildVDBECompoundDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeCompoundCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] VDBEMode=dual: %v", tc.sql, err)
		}
	}
}

// TestVDBECompoundColumnCountMismatch confirms that a compound SELECT whose
// arms return different numbers of result columns is rejected -- by the VDBE
// compiler up front (QueryVDBE returns an error) and, because there is no
// fallback, by the integrated QueryArgs path too -- never silently combined
// (e.g. truncating/padding one side).
func TestVDBECompoundColumnCountMismatch(t *testing.T) {
	path := buildVDBECompoundDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cases := []string{
		`SELECT a FROM wide_a UNION SELECT a, b FROM wide_b`,
		`SELECT a, b FROM wide_b UNION ALL SELECT a FROM wide_a`,
		`SELECT a FROM wide_a INTERSECT SELECT a, b FROM wide_b`,
		`SELECT a FROM wide_a EXCEPT SELECT a, b FROM wide_b`,
	}
	for _, sqlText := range cases {
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] QueryVDBE: expected column-count-mismatch error, got nil", sqlText)
		}
		// QueryVDBE may either decline (handled=false-equivalent: an error
		// here) or, if it does compile, must raise the identical mismatch
		// error rather than guess -- either way, QueryArgs (the integrated
		// path) must end up erroring, never silently returning rows.
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] QueryArgs: expected column-count-mismatch error, got nil", sqlText)
		}
	}
}

// TestVDBECompoundUnsupportedOrderByTerm confirms an ORDER BY term on a
// compound SELECT that isn't a result-column position or name (an arbitrary
// recomputed expression) is rejected -- QueryVDBE declines it as a
// compile-time decline (compileCompound validates every ORDER BY term up front
// against resolveCompoundOrderIndex, sql_compound.go), and the integrated path
// raises that error rather than answering.
func TestVDBECompoundUnsupportedOrderByTerm(t *testing.T) {
	path := buildVDBECompoundDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	sqlText := `SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a + 1`
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Errorf("[%s] QueryVDBE: expected an unsupported/compile error, got nil", sqlText)
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Errorf("[%s] QueryArgs: expected the ORDER BY error, got nil", sqlText)
	}
}

// TestVDBECompoundArmFallback used to confirm that a compound whose arm falls
// outside the VDBE's per-arm scan compiler -- an arm with a subquery inside a
// JOIN's ON condition -- is a clean, ALL-OR-NOTHING compile-time decline.
// compileExpr now DOES compile a subquery inside an ON
// condition (emitJoinLevel's compileOn, vdbe_join_codegen.go, no longer
// preemptively declines it -- see that function's own doc comment for why
// it's safe), so this arm now compiles like any other, and the whole compound
// COMPILES through the VDBE -- this is now a positive QueryVDBE-vs-real-C-
// SQLite parity check instead (see vdbe_join_gap_fix_test.go for this shape's
// own dedicated, wider gate, outside a compound). A DISTINCT arm, and a
// DISTINCT+GROUP BY arm (over a single table OR a join), no longer belong
// here either -- all are now compiled by the VDBE (see vdbeCompoundCorpus
// above, vdbe_distinct_test.go, and vdbe_agg_test.go).
func TestVDBECompoundArmFallback(t *testing.T) {
	path := buildVDBECompoundDB(t)

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

	sqlText := `SELECT ca.a FROM ca JOIN cb ON ca.a = (SELECT a FROM cc LIMIT 1) UNION SELECT a FROM cb ORDER BY a`
	vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
	if vErr != nil {
		t.Fatalf("[%s] QueryVDBE: %v", sqlText, vErr)
	}
	cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
	if cErr != nil {
		t.Fatalf("[%s] cgo: %v", sqlText, cErr)
	}
	vRows := engineRowsToStrings(vVals)
	if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, true); !ok {
		t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
			sqlText, reason, vCols, vRows, cCols, cRows)
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
		t.Errorf("[%s] QueryArgs: %v", sqlText, err)
	}
}

// TestVDBECompoundAsSubqueryParity confirms the "compound as a subquery body"
// shape documented at the top of this file: compileSubProgram now routes a
// compound (UNION/INTERSECT/EXCEPT) subquery body -- in an IN list, an
// EXISTS, or a scalar-subquery position -- to compileSubProgramCompound, so
// QueryVDBE compiles and runs the WHOLE enclosing statement in bytecode, with
// no fallback of any kind. Every case below carries its own outer FROM clause
// (ca) so the outer statement itself isn't ALSO a FROM-less SELECT -- a
// separate, unrelated VDBE limitation (see query.go's execNoFrom) that would
// otherwise mask what this test is actually pinning down.
func TestVDBECompoundAsSubqueryParity(t *testing.T) {
	path := buildVDBECompoundDB(t)

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

	cases := []subCase{
		{"SELECT a FROM ca WHERE a IN (SELECT a FROM ca UNION SELECT a FROM cb) ORDER BY a", true},
		{"SELECT a FROM ca WHERE EXISTS (SELECT a FROM ca INTERSECT SELECT a FROM cc) ORDER BY a", true},
		{"SELECT a, (SELECT a FROM ca UNION ALL SELECT a FROM cb ORDER BY a LIMIT 1) FROM ca ORDER BY a", true},
	}
	for _, tc := range cases {
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE: expected the compound-as-subquery-body shape to compile, got: %v", sqlText, vErr)
			continue
		}
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] cgo: %v", sqlText, cErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
			t.Errorf("[%s] QueryArgs: %v", sqlText, err)
		}
	}
}

// TestVDBECompoundDualSubqueryFallback confirms that running the
// "compound-as-subquery-body" corpus in VDBEMode=dual raises no divergence:
// each of these compiles whole through compileSubProgramCompound (see this
// file's package doc comment), so the "masked divergence" failure mode -- a
// would-be pass turning into an error -- must not occur here.
func TestVDBECompoundDualSubqueryFallback(t *testing.T) {
	path := buildVDBECompoundDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cases := []string{
		"SELECT a FROM ca WHERE a IN (SELECT a FROM ca UNION SELECT a FROM cb) ORDER BY a",
		"SELECT EXISTS (SELECT a FROM ca INTERSECT SELECT a FROM cc)",
		"SELECT (SELECT a FROM ca UNION ALL SELECT a FROM cb ORDER BY a LIMIT 1)",
	}
	for _, sqlText := range cases {
		if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
			t.Errorf("[%s] VDBEMode=dual (compound-as-subquery fallback must not diverge): %v", sqlText, err)
		}
	}
}
