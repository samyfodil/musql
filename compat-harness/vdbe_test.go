package compat

// Tests the VDBE bytecode VM correctness for FROM-less SELECT statements.

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// vdbeGateCorpus is a broad set of FROM-less SELECTs spanning the compiled
// grammar: arithmetic (int/real division, modulo, overflow), concat, all
// comparison ops incl. NULL three-valued, AND/OR/NOT short-circuit, CASE both
// forms, IN(list) with NULLs, BETWEEN, LIKE, CAST, the supported scalar
// functions, big ints near the int64 limits, reals, and blobs.
var vdbeGateCorpus = []string{
	"SELECT 1+2, 3-4, 5*6, 7/2, 7.0/2, 7%3, -7%3, 7%-3",
	"SELECT 10/0, 10%0, 10.0/0, 5/2.0, -5/2, 5.5+2, 2*3.5",
	"SELECT 9223372036854775807+1, -9223372036854775807-2, 5*5",
	"SELECT -5, +5, - -5, -'10', +'10', -3.5",
	"SELECT 'a'||'b'||'c', 1||2, 'x'||NULL, NULL||'y', 1.5||'z'",
	"SELECT 1<2, 2<=2, 3>2, 4>=5, 5=5, 6!=7, 8<>8",
	"SELECT NULL=NULL, NULL<1, 1>NULL, NULL<>NULL, 'a'<'b', 'B'<'a'",
	"SELECT 5='5', 5=5.0, '10'<'9', 10<'9'",
	"SELECT 1 IS 1, 1 IS NULL, NULL IS NULL, 'a' IS 'a', 1 IS NOT 2, NULL IS NOT NULL",
	"SELECT 5 IS NULL, 5 IS NOT NULL, NULL ISNULL, 5 NOTNULL",
	"SELECT 1 AND 1, 1 AND 0, 0 AND 1, NULL AND 0, NULL AND 1, 1 OR 0, 0 OR 0, NULL OR 1, NULL OR 0",
	"SELECT NOT 1, NOT 0, NOT NULL, NOT (1 AND 0), NOT 'abc', NOT ''",
	"SELECT CASE WHEN 1 THEN 'a' WHEN 1 THEN 'b' ELSE 'c' END",
	"SELECT CASE WHEN 0 THEN 'a' WHEN NULL THEN 'b' ELSE 'z' END",
	"SELECT CASE 3 WHEN 1 THEN 'x' WHEN 3 THEN 'y' ELSE 'z' END",
	"SELECT CASE NULL WHEN NULL THEN 'a' ELSE 'b' END, CASE 2 WHEN 1 THEN 'x' END",
	"SELECT 2 IN (1,2,3), 9 IN (1,2,3), NULL IN (1,2), 2 IN (1,NULL,2), 9 IN (1,NULL,2)",
	"SELECT 5 NOT IN (1,2), 5 NOT IN (1,NULL), 5 IN ()",
	"SELECT 3 BETWEEN 1 AND 5, 9 BETWEEN 1 AND 5, 3 NOT BETWEEN 1 AND 5, NULL BETWEEN 1 AND 5",
	"SELECT 'hello' LIKE 'h%', 'hello' LIKE 'H_LLO', 'abc' LIKE 'a%c', 'abc' NOT LIKE 'x%', NULL LIKE 'a', 'a' LIKE NULL",
	"SELECT CAST('123abc' AS INTEGER), CAST('3.14' AS REAL), CAST(3.99 AS INTEGER), CAST(65 AS TEXT), CAST('xyz' AS NUMERIC)",
	"SELECT CAST('5' AS INTEGER)='5', CAST('5' AS TEXT)=5, CAST(5 AS REAL), typeof(CAST(5 AS REAL))",
	"SELECT abs(-5), abs(-3.5), length('héllo'), length(x'00ff'), lower('AbÇ'), upper('AbÇ')",
	"SELECT substr('hello',2,3), substr('hello',-2), coalesce(NULL,NULL,7), ifnull(NULL,'d'), nullif(3,3), nullif(3,4)",
	"SELECT typeof(1), typeof(1.5), typeof('x'), typeof(x'00'), typeof(NULL), hex(x'deadbeef'), hex('AB')",
	"SELECT 9223372036854775807, -9223372036854775808, 0, 2.5e-3",
	"SELECT x'', x'00', x'deadbeef', typeof(x'01')",
	"SELECT 1 WHERE 1=1", "SELECT 1 WHERE 1=0", "SELECT 1 WHERE NULL",
	"SELECT 42 LIMIT 0", "SELECT 42 LIMIT 1", "SELECT 42 LIMIT 1 OFFSET 0",
	"SELECT 1 AS a, 2 b, 3+4 AS sum, 'x'",
	"SELECT (1+2)*3 = 9 AND 'a'||'b' = 'ab', CASE WHEN 5 IN (1,2,3,4,5) THEN abs(-1) ELSE 0 END",
}

// engineRowsToStrings normalizes an engine result (from RunNoFrom) into the
// storage-class-tagged string rows queryResultsMatch consumes.
func engineRowsToStrings(vals [][]engine.Value) [][]string {
	out := make([][]string, len(vals))
	for r, row := range vals {
		cells := make([]string, len(row))
		for c, v := range row {
			cells[c] = normalizeEngineValue(v)
		}
		out[r] = cells
	}
	return out
}

// cgoSelect runs sql through C SQLite (mattn) and returns its normalized
// columns and rows.
func cgoSelect(t *testing.T, db *sql.DB, sqlText string, args []any) ([]string, [][]string, error) {
	t.Helper()
	rows, err := db.Query(sqlText, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		norm := make([]string, len(cols))
		for i, c := range cells {
			norm[i] = normalizeAny(c)
		}
		out = append(out, norm)
	}
	return cols, out, rows.Err()
}

// TestVDBEResultParity is the hard gate: every corpus statement must produce
// identical results through the VDBE and C SQLite.
func TestVDBEResultParity(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	wrong := 0
	total := 0
	for _, sqlText := range vdbeGateCorpus {
		total++

		vCols, vVals, vErr := engine.RunNoFrom(sqlText, nil) // (a) VDBE
		cCols, cRows, cErr := cgoSelect(t, db, sqlText, nil) // (b) C SQLite oracle

		// Every corpus statement is expected to execute cleanly on both.
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
	t.Logf("VDBE result-parity gate: %d FROM-less SELECT statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEParamParity extends the gate to bound parameters (OpVariable),
// comparing the VDBE against C SQLite.
func TestVDBEParamParity(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := []struct {
		sql        string
		engineArgs []engine.Value
		cgoArgs    []any
	}{
		{"SELECT ?1 + ?2, ?2 * 2", []engine.Value{evInt(10), evInt(20)}, []any{int64(10), int64(20)}},
		{"SELECT ?1 IN (1,2,3), ?2 LIKE 'a%'", []engine.Value{evInt(2), evText("abc")}, []any{int64(2), "abc"}},
		{"SELECT ?1 || ?2, ?1 = ?2", []engine.Value{evText("a"), evText("b")}, []any{"a", "b"}},
		{"SELECT CASE WHEN ?1 THEN 'y' ELSE 'n' END", []engine.Value{evInt(0)}, []any{int64(0)}},
	}
	wrong := 0
	for _, c := range cases {
		vCols, vVals, vErr := engine.RunNoFrom(c.sql, c.engineArgs)
		cCols, cRows, cErr := cgoSelect(t, db, c.sql, c.cgoArgs)
		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error vdbe=%v cgo=%v", c.sql, vErr, cErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe=%v cgo=%v", c.sql, reason, vRows, cRows)
		}
	}
	t.Logf("VDBE param-parity gate: %d statements, wrong=%d", len(cases), wrong)
	if wrong != 0 {
		t.Fatalf("VDBE param parity gate FAILED: wrong=%d", wrong)
	}
}

// TestVDBEBytecodeOracle logs our disassembler output next to SQLite's own
// EXPLAIN bytecode for a handful of statements. Informational only (not a gate)
// this increment: SQLite's codegen folds constants and reuses registers, which
// our dumb, unfolded codegen does not yet do -- so an exact opcode-sequence
// match is a later convergence goal, captured here for eyeballing.
func TestVDBEBytecodeOracle(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, sqlText := range []string{
		"SELECT 2+3*4",
		"SELECT 5 < 'x'",
		"SELECT CASE WHEN 1>2 THEN 'a' ELSE 'b' END",
	} {
		ours, err := engine.DisassembleNoFrom(sqlText)
		if err != nil {
			t.Errorf("disassemble %q: %v", sqlText, err)
			continue
		}
		theirs := explainReference(t, db, sqlText)
		t.Logf("\n==== %s ====\n--- musql VDBE ---\n%s\n--- SQLite EXPLAIN ---\n%s", sqlText, ours, theirs)
	}
}

// explainReference runs "EXPLAIN <sql>" through C SQLite (mattn) and
// renders the addr/opcode/p1/p2/p3/p4/p5/comment rows as a table, the bytecode
// oracle for structural comparison against our disassembler.
func explainReference(t *testing.T, db *sql.DB, sqlText string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN " + sqlText)
	if err != nil {
		return "EXPLAIN error: " + err.Error()
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var b strings.Builder
	b.WriteString(strings.Join(cols, "\t") + "\n")
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "scan error: " + err.Error()
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = strings.TrimPrefix(strings.TrimPrefix(normalizeAny(c), "I:"), "T:")
		}
		b.WriteString(strings.Join(parts, "\t") + "\n")
	}
	return b.String()
}
