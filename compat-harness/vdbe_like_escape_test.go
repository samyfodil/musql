// Gate for LIKE's ESCAPE clause, verified against C SQLite for escape handling,
// NULL propagation, invalid escape values, and interactions with type coercion.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// likeEscapeScalarCases are standalone expressions testing LIKE...ESCAPE and like() semantics.
var likeEscapeScalarCases = []string{
	// ---- ESCAPE: escaping %, _, and the escape character itself ----
	`SELECT 'a%b' LIKE 'a\%b' ESCAPE '\'`,
	`SELECT 'axb' LIKE 'a\%b' ESCAPE '\'`,
	`SELECT 'a_b' LIKE 'a\_b' ESCAPE '\'`,
	`SELECT 'axb' LIKE 'a\_b' ESCAPE '\'`,
	`SELECT 'a\b' LIKE 'a\\b' ESCAPE '\'`,
	`SELECT 'a\\b' LIKE 'a\\b' ESCAPE '\'`,
	`SELECT 'a\\%b' LIKE 'a\\\%b' ESCAPE '\'`,

	// ---- ESCAPE: escape followed by an ORDINARY character is still
	// literal, and that literal comparison is STILL ASCII-case-folded ----
	`SELECT 'ac' LIKE 'a\c' ESCAPE '\'`,
	`SELECT 'a\c' LIKE 'a\c' ESCAPE '\'`,
	`SELECT 'ac' LIKE 'a\C' ESCAPE '\'`,
	`SELECT 'aC' LIKE 'a\C' ESCAPE '\'`,

	// ---- ESCAPE: a trailing lone escape (nothing follows it) never
	// matches anything, even the text it looks identical to ----
	`SELECT 'a' LIKE 'a\' ESCAPE '\'`,
	`SELECT 'a\' LIKE 'a\' ESCAPE '\'`,

	// ---- ESCAPE: the TRIGGER check is case-SENSITIVE, unlike ordinary
	// LIKE character comparison (which folds ASCII case) ----
	`SELECT 'aXb' LIKE 'axXb' ESCAPE 'x'`,
	`SELECT 'aXb' LIKE 'aXXb' ESCAPE 'x'`,
	`SELECT 'axb' LIKE 'aXXb' ESCAPE 'x'`,

	// ---- ESCAPE: an escape char equal to a wildcard disables that
	// wildcard entirely -- every occurrence is then a trigger, never a
	// wildcard ----
	`SELECT 'abc%' LIKE 'abc%%' ESCAPE '%'`,
	`SELECT 'abc%%' LIKE 'abc%%' ESCAPE '%'`,
	`SELECT 'abc_' LIKE 'abc__' ESCAPE '_'`,
	`SELECT 'abc__' LIKE 'abc__' ESCAPE '_'`,
	`SELECT 'x' LIKE '%' ESCAPE '_'`,

	// ---- ESCAPE: non-ASCII (multi-byte UTF-8), single-RUNE escape
	// character ----
	`SELECT 'aéb' LIKE 'aé_' ESCAPE 'é'`,
	`SELECT 'a%b' LIKE 'aé%b' ESCAPE 'é'`,

	// ---- ESCAPE: NULL ESCAPE operand propagates NULL -- NOT an error ----
	`SELECT 'a' LIKE 'a' ESCAPE NULL`,
	`SELECT 'a' LIKE 'a%' ESCAPE NULL`,
	`SELECT like('a%', 'a%', NULL)`,

	// ---- ESCAPE: NULL X/Pattern still propagates NULL when ESCAPE is
	// itself valid ----
	`SELECT NULL LIKE 'a' ESCAPE '\'`,
	`SELECT 'a' LIKE NULL ESCAPE '\'`,

	// ---- ESCAPE: an invalid (not exactly one character) ESCAPE value is a
	// runtime error that takes precedence over an X/Pattern NULL and over a
	// WHERE that would otherwise discard the row ----
	`SELECT 'a' LIKE 'a' ESCAPE '\ab'`,
	`SELECT 'a' LIKE 'a' ESCAPE ''`,
	`SELECT NULL LIKE 'a' ESCAPE '\ab'`,
	`SELECT 'a' LIKE NULL ESCAPE '\ab'`,
	`SELECT 1 WHERE 'a' LIKE 'a' ESCAPE '\ab'`,
	`SELECT like('a', 'a', 'ab')`,

	// ---- ESCAPE: a computed (non-literal) escape expression, evaluated
	// per row rather than required to be a constant ----
	`SELECT 'a%b' LIKE 'a' || '\' || '%b' ESCAPE '\'`,
	`SELECT 'a%b' LIKE 'a\%b' ESCAPE ('\' || '')`,

	// ---- NOT LIKE with ESCAPE ----
	`SELECT 'a%b' NOT LIKE 'a\%b' ESCAPE '\'`,
	`SELECT 'axb' NOT LIKE 'a\%b' ESCAPE '\'`,

	// ---- like(pattern, x[, escape]) function form: like(X,Y) == "Y LIKE
	// X"; like(X,Y,Z) == "Y LIKE X ESCAPE Z" ----
	`SELECT like('a\%b', 'a%b', '\')`,
	`SELECT like('a%b', 'a%b')`,
	`SELECT like('a\%b', 'axb', '\')`,
	`SELECT like('a*', 'a*')`,

	// ---- plain LIKE (no ESCAPE): ASCII-only case-insensitivity, NULL
	// operands -- unaffected by adding ESCAPE support ----
	`SELECT 'ABC' LIKE 'abc'`,
	`SELECT 'É' LIKE 'é'`,
	`SELECT NULL LIKE 'a%'`,
	`SELECT 'a' LIKE NULL`,

	// ---- numeric / BLOB operand coercion is unaffected by an ESCAPE
	// clause (still TEXT-rendered, no affinity coercion) ----
	`SELECT 123 LIKE '1_3' ESCAPE '\'`,
	`SELECT 1.5 LIKE '1.5' ESCAPE '\'`,
	`SELECT 5 LIKE '5' ESCAPE 5`,   // numeric ESCAPE operand: "5" renders as 1 char
	`SELECT 5 LIKE '5' ESCAPE 5.0`, // "5.0" renders as 3 chars -> error
	`SELECT x'616263' LIKE 'abc' ESCAPE '\'`,
	`SELECT 'abc' LIKE x'616263' ESCAPE '\'`,
	`SELECT x'616263' LIKE x'616263' ESCAPE '\'`,
}

// TestLikeEscapeScalarsMatchCSQLite compares scalar ESCAPE results against C SQLite.
func TestLikeEscapeScalarsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("likeescape_scalar_%d.sqlite", pageSize))
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

			for _, s := range likeEscapeScalarCases {
				t.Run(s, func(t *testing.T) {
					compareOneScalar(t, db, sdb, s)
				})
			}
		})
	}
}

// buildLikeEscapeDB builds test tables with escape text patterns and per-row escape columns.
func buildLikeEscapeDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("likeescape_%d.sqlite", pageSize))
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

	exec(`CREATE TABLE eitems (id INTEGER PRIMARY KEY, x TEXT)`)
	rows := []any{
		"abcde", "ab%de", "a_cde", `a\b`, `a\\b`,
		"abc%", "abc%%", "abc_", "abc__",
		"ABCDE", "AbCdE", "aéb", "Xb", "aXb", "axXb",
		"", nil,
	}
	for i, v := range rows {
		if v == nil {
			exec(fmt.Sprintf(`INSERT INTO eitems VALUES (%d, NULL)`, i+1))
			continue
		}
		s := v.(string)
		s = strings.ReplaceAll(s, "'", "''")
		exec(fmt.Sprintf(`INSERT INTO eitems VALUES (%d, '%s')`, i+1, s))
	}

	exec(`CREATE TABLE eesc (id INTEGER PRIMARY KEY, x TEXT, esc TEXT)`)
	escRows := [][2]string{
		{"abcde", `\`},
		{"abc_de", `\`},
		{"abc\\de", "bogus"}, // multi-char ESCAPE -> runtime error for THIS row only
	}
	for i, r := range escRows {
		exec(fmt.Sprintf(`INSERT INTO eesc VALUES (%d, '%s', '%s')`, i+1, r[0], r[1]))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// likeEscapeTableCorpus are queries to test ESCAPE in WHERE clauses.
var likeEscapeTableCorpus = []string{
	`SELECT x FROM eitems WHERE x LIKE 'ab\%de' ESCAPE '\'`,
	`SELECT x FROM eitems WHERE x LIKE 'a\_cde' ESCAPE '\'`,
	`SELECT x FROM eitems WHERE x NOT LIKE 'ab\%de' ESCAPE '\'`,
	`SELECT x FROM eitems WHERE x LIKE 'abc%%' ESCAPE '%'`,
	`SELECT x FROM eitems WHERE x LIKE 'abc__' ESCAPE '_'`,
	`SELECT x FROM eitems WHERE x LIKE 'a\\b' ESCAPE '\'`,
	`SELECT x FROM eitems WHERE like('ab\%de', x, '\')`,
	`SELECT x FROM eitems WHERE x LIKE 'A%'`,
	`SELECT x, x LIKE 'ab\%de' ESCAPE '\' FROM eitems`,
	`SELECT x FROM eitems WHERE x LIKE 'aé_' ESCAPE 'é'`,
	`SELECT x FROM eitems WHERE x LIKE 'axXb' ESCAPE 'x'`,
	`SELECT x FROM eitems WHERE x IS NOT NULL AND x LIKE '%'`,
	// per-row (column-valued, non-literal) ESCAPE, including a row whose
	// ESCAPE is multi-character -- must error for exactly that row.
	`SELECT id FROM eesc WHERE x LIKE 'abc%de' ESCAPE esc`,
}

// TestLikeEscapeTableParity compares table results against C SQLite at different page sizes.
func TestLikeEscapeTableParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildLikeEscapeDB(t, pageSize)

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
			runCase := func(sqlText string) {
				total++
				eCols, eVals, eErr := p.Query(sqlText)
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

				if (eErr == nil) != (cErr == nil) {
					wrong++
					t.Errorf("[%s] error-shape mismatch\n  engine=%v\n  cgo=%v", sqlText, eErr, cErr)
					return
				}
				if cErr != nil {
					// Both declined/errored identically (e.g. the per-row
					// invalid-ESCAPE case) -- fine, never counted wrong.
					return
				}

				eRows := engineRowsToStrings(eVals)

				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, false); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}

				if vCols, vVals, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
					vRows := engineRowsToStrings(vVals)
					if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
						wrong++
						t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
							sqlText, reason, vCols, vRows, cCols, cRows)
					}
				}
			}

			for _, sqlText := range likeEscapeTableCorpus {
				runCase(sqlText)
			}

			t.Logf("LIKE ESCAPE table parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("LIKE ESCAPE table parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}
