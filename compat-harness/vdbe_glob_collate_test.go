// This file tests GLOB operator and expression-level COLLATE.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// globCollateScalarSupportedCases are standalone, FROM-less "SELECT ..."
// expressions the VDBE fully compiles (no CollateExpr node anywhere),
// pinning the exact GLOB semantics verified directly against C SQLite
// (mattn/go-sqlite3) while this feature was implemented: GLOB's
// case-sensitivity (unlike LIKE), '*'/'?'/'[...]' wildcards (including 'a-z'
// ranges, '^' -- not '!' -- negation, a leading ']'/'-' as a literal class
// member, and an unterminated '[' failing the WHOLE match), UTF-8
// rune-at-a-time matching, NULL propagation both directions, the numeric
// operand TEXT-rendering rule (no comparison-affinity coercion, just like
// LIKE), and the glob(pattern, x) function's argument order. Also included:
// a plain (COLLATE-free) trailing-space string comparison, confirming BINARY
// remains the untouched default.
var globCollateScalarSupportedCases = []string{
	// ---- GLOB: basic wildcards and case-sensitivity ----
	`SELECT 'abc' GLOB 'a*'`,
	`SELECT 'ABC' GLOB 'a*'`,
	`SELECT 'abc' GLOB 'A*'`,
	`SELECT 'abc' GLOB 'a?c'`,
	`SELECT 'abc' GLOB 'a??'`,
	`SELECT 'abc' GLOB 'abc'`,
	`SELECT 'abc' GLOB 'abd'`,
	`SELECT 'abc' NOT GLOB 'a*'`,
	`SELECT 'abc' NOT GLOB 'x*'`,
	`SELECT '' GLOB '*'`,
	`SELECT '' GLOB ''`,
	`SELECT 'a' GLOB ''`,

	// ---- GLOB: character classes ----
	`SELECT 'abc' GLOB '[ab]bc'`,
	`SELECT 'abc' GLOB '[a-c]bc'`,
	`SELECT 'abc' GLOB '[^a-c]bc'`,
	`SELECT 'xbc' GLOB '[^a-c]bc'`,
	`SELECT 'abc' GLOB '[!a-c]bc'`, // '!' is a LITERAL class member, not negation
	`SELECT 'a]b' GLOB 'a[]]b'`,    // leading ']' right after '[' is literal
	`SELECT 'a-b' GLOB 'a[-x]b'`,   // leading '-' right after '[' is literal
	`SELECT 'abc[' GLOB 'abc['`,    // unterminated class: whole match fails
	`SELECT 'a' GLOB '[a'`,         // unterminated class: whole match fails
	`SELECT 'abcx' GLOB 'abc[xy'`,  // unterminated class: whole match fails

	// ---- GLOB: UTF-8 (rune, not byte, matching) ----
	`SELECT 'café' GLOB 'caf?'`,
	`SELECT 'café' GLOB 'caf[é]'`,

	// ---- GLOB: NULL propagation ----
	`SELECT NULL GLOB 'a*'`,
	`SELECT 'abc' GLOB NULL`,
	`SELECT NULL GLOB NULL`,

	// ---- GLOB: non-text operands (TEXT-rendered, no affinity coercion) ----
	`SELECT 1 GLOB '1*'`,
	`SELECT 1 GLOB '1'`,
	`SELECT 1.5 GLOB '1.5'`,

	// ---- glob(pattern, x) function form: glob(X,Y) == "Y GLOB X" ----
	`SELECT glob('a*', 'abc')`,
	`SELECT glob('abc', 'a*')`, // reversed order: NOT the same as the above
	`SELECT glob('a*', NULL)`,
	`SELECT glob(NULL, 'abc')`,

	// ---- plain (COLLATE-free) comparison: BINARY default is unaffected ----
	`SELECT 'abc' = 'abc   '`, // no RTRIM: BINARY, unequal

	// ---- COLLATE: explicit-operand precedence ----
	`SELECT 'abc' = 'ABC' COLLATE NOCASE`,
	`SELECT 'ABC' COLLATE NOCASE = 'abc'`,
	`SELECT 'abc' COLLATE NOCASE = 'ABC' COLLATE BINARY`, // left wins -> NOCASE
	`SELECT 'ABC' COLLATE BINARY = 'abc' COLLATE NOCASE`, // left wins -> BINARY
	`SELECT 'a' COLLATE nocase = 'A'`,                    // lower-case collation name
	`SELECT 'a' COLLATE NOCASE = 'A' COLLATE NOCASE`,

	// ---- COLLATE: RTRIM (trailing ASCII space ONLY) ----
	`SELECT 'abc  ' = 'abc' COLLATE RTRIM`,
	`SELECT 'abc ' = 'abc' COLLATE RTRIM`,
	`SELECT 'abc' = 'abc' COLLATE RTRIM`,

	// ---- COLLATE: BINARY explicit is a no-op ----
	`SELECT 'abc' = 'ABC' COLLATE BINARY`,

	// ---- COLLATE: numeric/BLOB operands never consult collation ----
	`SELECT 1 COLLATE NOCASE = 1`,
	`SELECT 5 = 5 COLLATE NOCASE`,
	`SELECT x'4142' = x'4142' COLLATE NOCASE`,
	`SELECT x'4142' = x'6162' COLLATE NOCASE`, // "AB" vs "ab" as BLOBs: still BINARY

	// ---- COLLATE: NOCASE is ASCII-only ----
	`SELECT 'É' = 'é' COLLATE NOCASE`,

	// ---- COLLATE: propagates through CONCAT (left-then-right descent) ----
	`SELECT 'a' || 'b' COLLATE NOCASE = 'AB'`,
	`SELECT ('a'||'b') COLLATE NOCASE = 'AB'`,

	// ---- COLLATE: bare expression evaluates to the plain (uncollated) value ----
	`SELECT 'a' COLLATE NOCASE`,
	`SELECT typeof('a' COLLATE NOCASE)`,

	// ---- COLLATE: IS / IS NOT ----
	`SELECT 'abc' IS 'ABC' COLLATE NOCASE`,
	`SELECT 'abc' IS NOT 'ABC' COLLATE NOCASE`,
}

// globCollateScalarBothDeclineCases reference an UNKNOWN collation name: real
// SQLite itself rejects these (an unregistered collation is a genuine SQL
// error, not a shape it supports), so compareOneScalar's ordinary "both
// engines must agree" contract applies unmodified -- both erroring (for
// different reasons: the engine because it never compiles a CollateExpr at
// all, C SQLite because the name doesn't resolve) is a pass, not a gap.
var globCollateScalarBothDeclineCases = []string{
	`SELECT 'a' = 'b' COLLATE BOGUSCOLLATION`,
	`SELECT 'a' = 'b' COLLATE NoSuchColl2`,
}

func TestGlobCollateScalarsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("globcollate_scalar_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()

			sdb, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()

			for _, s := range globCollateScalarSupportedCases {
				t.Run(s, func(t *testing.T) {
					compareOneScalar(t, db, sdb, s)
				})
			}
			for _, s := range globCollateScalarBothDeclineCases {
				t.Run(s, func(t *testing.T) {
					compareOneScalar(t, db, sdb, s)
				})
			}
		})
	}
}

// buildGlobCollateDB builds, at pageSize, a small on-disk table of strings
// covering the row shapes the table-driven corpus below needs: mixed-case
// duplicates (for NOCASE grouping/ordering), a NULL row, GLOB bracket-class
// edge cases (literal ']'/'-'), UTF-8 text, an empty string, and
// trailing-ASCII-space variants (for RTRIM).
func buildGlobCollateDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("globcollate_%d.sqlite", pageSize))
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

	exec(`CREATE TABLE strs (id INTEGER PRIMARY KEY, x TEXT)`)
	rows := []any{
		"abc", "ABC", "AbC", "abd", "a1c", "xyz",
		"a]b", "a-b", "café", "", "abc ", "ABC  ", nil,
	}
	for i, v := range rows {
		if v == nil {
			exec(fmt.Sprintf(`INSERT INTO strs VALUES (%d, NULL)`, i+1))
			continue
		}
		exec(fmt.Sprintf(`INSERT INTO strs VALUES (%d, '%s')`, i+1, v))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// globCollateTableCorpus are GLOB-only queries (no explicit COLLATE anywhere)
// whose result SET must match (row order irrelevant -- these are unordered
// WHERE-filters or a boolean projection). GLOB compiles fully to bytecode
// (compileGlob), so every one of these is a hard VDBE-vs-C-SQLite parity
// requirement, not a best-effort check.
var globCollateTableCorpus = []string{
	"SELECT x FROM strs WHERE x GLOB 'a*'",
	"SELECT x FROM strs WHERE x GLOB 'A*'",
	"SELECT x FROM strs WHERE x NOT GLOB 'a*'",
	"SELECT x FROM strs WHERE x GLOB '???'",
	"SELECT x FROM strs WHERE x GLOB '[a-c]*'",
	"SELECT x FROM strs WHERE x GLOB '[^a-c]*'",
	"SELECT x FROM strs WHERE x GLOB 'a[]]b'",
	"SELECT x FROM strs WHERE x GLOB 'a[-x]b'",
	"SELECT x FROM strs WHERE glob('a*', x)",
	"SELECT x, x GLOB 'a*' FROM strs",

	// ---- expression-level COLLATE: WHERE-predicate equality/IN, no ORDER BY
	// (result SET only -- row order irrelevant) ----
	"SELECT x FROM strs WHERE x = 'abc' COLLATE NOCASE",
	"SELECT x FROM strs WHERE x COLLATE NOCASE = 'abc'",
	"SELECT x FROM strs WHERE x COLLATE NOCASE = 'ABC'",
	"SELECT x FROM strs WHERE x = 'abc' COLLATE RTRIM",
	"SELECT x FROM strs WHERE x COLLATE NOCASE IN ('abc', 'xyz')",
}

// globCollateTableOrderedCorpus are queries whose result ROW ORDER must match
// exactly: an explicit "X COLLATE name" governing an ORDER BY term -- a
// result-column-name and an ordinal ORDER BY target under NOCASE (both of
// which must still resolve to the referenced OUTPUT COLUMN, not a constant,
// with a COLLATE attached -- see resolveOrderKeys' stripOrderCollate use,
// vdbe_sort_codegen.go), a plain column reference under NOCASE, and an
// explicit-BINARY ORDER BY term (a no-op override, still exercising the same
// codegen path).
var globCollateTableOrderedCorpus = []string{
	"SELECT x FROM strs WHERE x IS NOT NULL AND x GLOB '[Aa]*' ORDER BY x COLLATE NOCASE, x",
	"SELECT x AS y FROM strs WHERE x IS NOT NULL AND x GLOB '[Aa]*' ORDER BY y COLLATE NOCASE, y",
	"SELECT x FROM strs WHERE x IS NOT NULL AND x GLOB '[Aa]*' ORDER BY 1 COLLATE NOCASE, 1",
	"SELECT x FROM strs WHERE x IS NOT NULL ORDER BY x COLLATE BINARY",
}

// TestGlobCollateTableParity is the table-driven hard gate, at both page
// sizes 512 and 4096: every corpus statement (GLOB-only or carrying an
// explicit expression-level COLLATE) must produce identical results through
// the VDBE and C SQLite (byte-exact parity, including row ORDER for
// globCollateTableOrderedCorpus).
func TestGlobCollateTableParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildGlobCollateDB(t, pageSize)

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
			runSupportedCase := func(sqlText string, orderSensitive bool) {
				total++
				eCols, eVals, eErr := p.Query(sqlText)
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

				if eErr != nil || cErr != nil {
					wrong++
					t.Errorf("[%s] unexpected error\n  engine=%v\n  cgo=%v", sqlText, eErr, cErr)
					return
				}

				eRows := engineRowsToStrings(eVals)

				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, orderSensitive); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}

				vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
				if vErr != nil {
					wrong++
					t.Errorf("[%s] QueryVDBE: expected a GLOB-only statement to compile, got: %v", sqlText, vErr)
					return
				}
				vRows := engineRowsToStrings(vVals)
				if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, orderSensitive); !ok {
					wrong++
					t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
						sqlText, reason, vCols, vRows, cCols, cRows)
				}
			}

			for _, sqlText := range globCollateTableCorpus {
				runSupportedCase(sqlText, false)
			}
			for _, sqlText := range globCollateTableOrderedCorpus {
				runSupportedCase(sqlText, true)
			}

			t.Logf("GLOB/COLLATE table parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("GLOB/COLLATE table parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}
