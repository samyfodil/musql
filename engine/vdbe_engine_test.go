package engine

import (
	"strings"
	"testing"
)

// TestVDBEHandBuiltProgram executes a Program assembled by hand (not via the
// codegen) to exercise the VM core directly: constant loads, arithmetic
// operand direction (r[P3]=r[P2]-r[P1]), and OpResultRow's register range.
func TestVDBEHandBuiltProgram(t *testing.T) {
	// Compute (10 - 3) and (3 * 4) into a two-column result row.
	prog := &Program{
		NReg:       5,
		NResultCol: 2,
		ColNames:   []string{"diff", "prod"},
		Insns: []Instruction{
			{Op: OpInteger, P1: 10, P2: 2},
			{Op: OpInteger, P1: 3, P2: 3},
			{Op: OpSubtract, P1: 3, P2: 2, P3: 0}, // r[0] = r[2] - r[3] = 10 - 3
			{Op: OpInteger, P1: 4, P2: 4},
			{Op: OpMultiply, P1: 3, P2: 4, P3: 1}, // r[1] = r[3] * r[4] = 3 * 4
			{Op: OpResultRow, P1: 0, P2: 2},
			{Op: OpHalt},
		},
	}
	rows, err := prog.exec(nil, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("shape: %v", rows)
	}
	if rows[0][0].Typ != Int || rows[0][0].I != 7 {
		t.Errorf("diff: got %+v want Int 7", rows[0][0])
	}
	if rows[0][1].Typ != Int || rows[0][1].I != 12 {
		t.Errorf("prod: got %+v want Int 12", rows[0][1])
	}
}

// TestVDBEAffinityMutationTiming pins down the design-review pitfall: OpAffinity
// MUTATES its register permanently, while a comparison opcode's P5 affinity is
// applied only to copies for the compare and must NOT mutate the operand
// registers.
func TestVDBEAffinityMutationTiming(t *testing.T) {
	// OpAffinity permanently turns TEXT '5' into INTEGER 5.
	mut := &Program{
		NReg: 1, NResultCol: 1, ColNames: []string{"t"},
		Insns: []Instruction{
			{Op: OpString8, P2: 0, P4: "5"},
			{Op: OpAffinity, P1: 0, P4: affNumeric},
			{Op: OpResultRow, P1: 0, P2: 1},
			{Op: OpHalt},
		},
	}
	rows, err := mut.exec(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].Typ != Int || rows[0][0].I != 5 {
		t.Errorf("OpAffinity should mutate TEXT '5' -> INTEGER 5, got %+v", rows[0][0])
	}

	// A comparison with numeric affinity compares '5' == 5 as TRUE but leaves
	// register 0 as the original TEXT '5'.
	cmp := &Program{
		NReg: 3, NResultCol: 3, ColNames: []string{"x", "y", "eq"},
		Insns: []Instruction{
			{Op: OpString8, P2: 0, P4: "5"},
			{Op: OpInteger, P1: 5, P2: 1},
			{Op: OpEq, P1: 1, P2: 2, P3: 0, P5: uint16(affNumeric) | p5StoreP2},
			{Op: OpResultRow, P1: 0, P2: 3},
			{Op: OpHalt},
		},
	}
	rows, err = cmp.exec(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].Typ != Text || string(rows[0][0].S) != "5" {
		t.Errorf("comparison affinity must not mutate operand register: got %+v", rows[0][0])
	}
	if rows[0][2].Typ != Int || rows[0][2].I != 1 {
		t.Errorf("'5' == 5 under numeric affinity should be TRUE, got %+v", rows[0][2])
	}
}

// TestVDBEDisassemble checks the EXPLAIN-style listing renders the expected
// structure for a representative statement (arithmetic operand direction,
// ResultRow range, Halt).
func TestVDBEDisassemble(t *testing.T) {
	d, err := DisassembleNoFrom("SELECT 2+3*4")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Init", "Integer", "Multiply", "Add", "ResultRow", "Halt",
		"r[4]=r[2]*r[3]", // 3*4 with SQLite operand direction
	} {
		if !strings.Contains(d, want) {
			t.Errorf("disassembly missing %q:\n%s", want, d)
		}
	}
}

// TestVDBECompileExpr confirms representative expressions across the whole
// FROM-less grammar compile to a runnable program (no unsupported error) and
// that unsupported constructs are cleanly rejected rather than mis-compiled.
func TestVDBECompileExpr(t *testing.T) {
	compiles := []string{
		"SELECT 1", "SELECT 1+2*3-4/2%3", "SELECT -5, +5, NOT 0",
		"SELECT 'a'||'b'", "SELECT 1<2 AND 3>2 OR 0", "SELECT 5 IS 5, 5 IS NOT 6",
		"SELECT 5 IS NULL, 5 IS NOT NULL", "SELECT 3 BETWEEN 1 AND 5",
		"SELECT 2 IN (1,2,3), 9 NOT IN (1,2)", "SELECT 'hi' LIKE 'h%'",
		"SELECT CASE WHEN 1 THEN 'a' ELSE 'b' END", "SELECT CASE 1 WHEN 1 THEN 'a' END",
		"SELECT CAST('7x' AS INTEGER)", "SELECT abs(-5), coalesce(NULL,1)",
		"SELECT ?1, ?2+1",
		// A scalar subquery whose body reads no table needs no snapshot to
		// run against, so it compiles even on this PAGER-LESS entry point --
		// see selectNeedsNoRowSource (vdbe_codegen.go). It is how ATTACH's
		// path argument reaches the compiler ("ATTACH (SELECT 'q.db') AS y",
		// which C SQLite attaches; attach.c:403 codes that argument like
		// any other expression). One that DOES read a table still rejects
		// below.
		"SELECT (SELECT 1)",
	}
	for _, sql := range compiles {
		stmt, err := ParseSelect(sql)
		if err != nil {
			t.Errorf("parse %q: %v", sql, err)
			continue
		}
		if _, err := compileSelectNoFrom(stmt); err != nil {
			t.Errorf("compile %q: %v", sql, err)
		}
	}

	rejects := []string{
		"SELECT a FROM t",          // column ref / has FROM
		"SELECT count(*)",          // aggregate
		"SELECT (SELECT a FROM t)", // subquery reading a table: no snapshot here to read it from
		"SELECT * ",                // star
		"SELECT 1 UNION SELECT 2",  // compound
	}
	for _, sql := range rejects {
		stmt, err := ParseSelect(sql)
		if err != nil {
			continue // a parse error is also an acceptable "not for the VDBE" outcome
		}
		if _, err := compileSelectNoFrom(stmt); err == nil {
			t.Errorf("compile %q: expected unsupported error, got nil", sql)
		}
	}
}

// vdbeParityCorpus is a broad set of FROM-less SELECTs exercising the whole
// compiled grammar. Reused by both the engine-level smoke test here and, in
// the compat-harness, the gate that answers the same corpus from real C
// SQLite.
var vdbeParityCorpus = []string{
	// Arithmetic incl. int/real division, modulo, overflow, unary.
	"SELECT 1+2, 3-4, 5*6, 7/2, 7.0/2, 7%3, -7%3, 7%-3",
	"SELECT 10/0, 10%0, 10.0/0, 5/2.0, -5/2, 5.5+2, 2*3.5",
	"SELECT -9223372036854775807-2, 9223372036854775807+1, -(-9223372036854775807-1)",
	"SELECT -5, +5, - -5, -'10', +'10', -3.5",
	// Concatenation.
	"SELECT 'a'||'b'||'c', 1||2, 'x'||NULL, NULL||'y', 1.5||'z'",
	// Comparisons incl. NULL three-valued and cross-class ordering.
	"SELECT 1<2, 2<=2, 3>2, 4>=5, 5=5, 6!=7, 8<>8",
	"SELECT NULL=NULL, NULL<1, 1>NULL, NULL<>NULL, 'a'<'b', 'B'<'a'",
	"SELECT 5='5', 5=5.0, '10'<'9', 10<'9', x'01'<x'02'",
	// IS / IS NOT / IS NULL / NOTNULL.
	"SELECT 1 IS 1, 1 IS NULL, NULL IS NULL, 'a' IS 'a', 1 IS NOT 2, NULL IS NOT NULL",
	"SELECT 5 IS NULL, 5 IS NOT NULL, NULL ISNULL, 5 NOTNULL",
	// AND/OR/NOT short-circuit and three-valued.
	"SELECT 1 AND 1, 1 AND 0, 0 AND 1, NULL AND 0, NULL AND 1, 1 OR 0, 0 OR 0, NULL OR 1, NULL OR 0",
	"SELECT NOT 1, NOT 0, NOT NULL, NOT (1 AND 0), NOT 'abc', NOT ''",
	// CASE both forms.
	"SELECT CASE WHEN 1 THEN 'a' WHEN 1 THEN 'b' ELSE 'c' END",
	"SELECT CASE WHEN 0 THEN 'a' WHEN NULL THEN 'b' ELSE 'z' END",
	"SELECT CASE 3 WHEN 1 THEN 'x' WHEN 3 THEN 'y' ELSE 'z' END",
	"SELECT CASE NULL WHEN NULL THEN 'a' ELSE 'b' END, CASE 2 WHEN 1 THEN 'x' END",
	// IN(list) with NULLs, BETWEEN.
	"SELECT 2 IN (1,2,3), 9 IN (1,2,3), NULL IN (1,2), 2 IN (1,NULL,2), 9 IN (1,NULL,2)",
	"SELECT 5 NOT IN (1,2), 5 NOT IN (1,NULL), 5 IN ()",
	"SELECT 3 BETWEEN 1 AND 5, 9 BETWEEN 1 AND 5, 3 NOT BETWEEN 1 AND 5, NULL BETWEEN 1 AND 5",
	// LIKE.
	"SELECT 'hello' LIKE 'h%', 'hello' LIKE 'H_LLO', 'abc' LIKE 'a%c', 'abc' NOT LIKE 'x%', NULL LIKE 'a', 'a' LIKE NULL",
	// CAST.
	"SELECT CAST('123abc' AS INTEGER), CAST('3.14' AS REAL), CAST(3.99 AS INTEGER), CAST(65 AS TEXT), CAST('xyz' AS NUMERIC)",
	"SELECT CAST('5' AS INTEGER)='5', CAST('5' AS TEXT)=5, CAST(5 AS REAL), typeof(CAST(5 AS REAL))",
	// Scalar functions.
	"SELECT abs(-5), abs(-3.5), length('héllo'), length(x'00ff'), lower('AbÇ'), upper('AbÇ')",
	"SELECT substr('hello',2,3), substr('hello',-2), coalesce(NULL,NULL,7), ifnull(NULL,'d'), nullif(3,3), nullif(3,4)",
	"SELECT typeof(1), typeof(1.5), typeof('x'), typeof(x'00'), typeof(NULL), hex(x'deadbeef'), hex('AB')",
	// Big ints, reals, blobs, literals.
	"SELECT 9223372036854775807, -9223372036854775808, 0, 1.7976931348623157e308, 2.5e-3",
	"SELECT x'', x'00', x'deadbeef', typeof(x'01')",
	// WHERE and LIMIT/OFFSET over the single synthetic row.
	"SELECT 1 WHERE 1=1", "SELECT 1 WHERE 1=0", "SELECT 1 WHERE NULL",
	// "SELECT 42 OFFSET 1" (OFFSET with no LIMIT) is deliberately omitted:
	// it's a parse-time rejection (OFFSET must follow LIMIT), not something
	// that ever reaches the VDBE, so it belongs in the parser's own test
	// coverage rather than this compile/exec smoke corpus.
	"SELECT 42 LIMIT 0", "SELECT 42 LIMIT 1", "SELECT 42 LIMIT 1 OFFSET 0",
	// Multiple columns / aliases (names checked too).
	"SELECT 1 AS a, 2 b, 3+4 AS sum, 'x'",
	// Nested / mixed.
	"SELECT (1+2)*3 = 9 AND 'a'||'b' = 'ab', CASE WHEN 5 IN (1,2,3,4,5) THEN abs(-1) ELSE 0 END",
}

// TestVDBEExpressionCorpusRuns runs the whole corpus through the VDBE and
// asserts each compiles and executes without error. It used to also cross-check
// every answer against a second, in-package evaluator; that evaluator is gone,
// and the real-C-SQLite oracle in the compat-harness module is the actual
// correctness gate for this corpus now. Kept as a broad VDBE smoke test: a
// compile/exec error here across this much expression-shape coverage is a real
// regression.
func TestVDBEExpressionCorpusRuns(t *testing.T) {
	for _, sql := range vdbeParityCorpus {
		if _, _, err := RunNoFrom(sql, nil); err != nil {
			t.Errorf("VDBE %q: unexpected error: %v", sql, err)
		}
	}
}

// TestVDBEParams checks parameter binding through OpVariable executes without
// error, including an unbound (NULL) parameter. (This used to also cross-check
// against a second evaluator; see TestVDBEExpressionCorpusRuns' doc comment.)
func TestVDBEParams(t *testing.T) {
	type tc struct {
		sql  string
		args []Value
	}
	cases := []tc{
		{"SELECT ?1 + ?2", []Value{{Typ: Int, I: 10}, {Typ: Int, I: 20}}},
		{"SELECT ?1, ?2, ?3", []Value{{Typ: Text, S: []byte("hi")}, {Typ: Float, F: 2.5}, {Typ: Null}}},
		{"SELECT ?1 IN (1,2,3), ?2 LIKE 'a%'", []Value{{Typ: Int, I: 2}, {Typ: Text, S: []byte("abc")}}},
		{"SELECT ?5", nil}, // unbound -> NULL
	}
	for _, c := range cases {
		if _, _, err := RunNoFrom(c.sql, c.args); err != nil {
			t.Errorf("param %q: unexpected error: %v", c.sql, err)
		}
	}
}
