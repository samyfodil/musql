// LIMIT/OFFSET and DEFAULT expressions compiled to bytecode, not just declined.
package engine

import (
	"strings"
	"testing"
)

// TestLimitOffsetFoldIsCompiled verifies LIMIT/OFFSET expressions compile.
func TestLimitOffsetFoldIsCompiled(t *testing.T) {
	for _, c := range []struct {
		sql    string
		opcode string
	}{
		{"SELECT ?", "Variable"},
		{"SELECT 1+1", "Add"},
		{"SELECT abs(-2)", "Function"},
	} {
		asm, err := DisassembleNoFrom(c.sql)
		if err != nil {
			t.Fatalf("%s: compile: %v", c.sql, err)
		}
		if !strings.Contains(asm, c.opcode) {
			t.Errorf("%s: LIMIT/OFFSET fold program has no %s opcode -- it is not being compiled:\n%s",
				c.sql, c.opcode, asm)
		}
	}

	// The values, through the seam execSelect actually calls. A bound
	// parameter must reach OpVariable with the statement's own args, which is
	// the half a nil args list would silently break.
	for _, c := range []struct {
		name string
		e    Expr
		args []Value
		want int64
	}{
		{"param", ParamExpr{Index: 1}, []Value{{Typ: Int, I: 7}}, 7},
		{"arith", BinaryExpr{Op: "+", L: LiteralExpr{Val: Value{Typ: Int, I: 1}}, R: LiteralExpr{Val: Value{Typ: Int, I: 1}}}, nil, 2},
		{"func", FuncExpr{Name: "abs", Args: []Expr{LiteralExpr{Val: Value{Typ: Int, I: -3}}}}, nil, 3},
		{"text-affinity", LiteralExpr{Val: Value{Typ: Text, S: []byte("2")}}, nil, 2},
	} {
		got, err := foldLimitOffsetExpr(nil, c.e, c.args)
		if err != nil {
			t.Errorf("%s: foldLimitOffsetExpr: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}

	// A column reference must DECLINE, not resolve: resolve.c:1900-1903
	// resolves LIMIT/OFFSET against a freshly memset NameContext, so the
	// clause can name nothing. Resolving it against a live row context would
	// answer it instead -- exactly the wrong answer this fold replaced.
	if _, err := foldLimitOffsetExpr(nil, ColumnExpr{Name: "a"}, nil); err == nil {
		t.Error("foldLimitOffsetExpr resolved a bare column reference; the clause must decline it")
	}
	if _, err := foldLimitOffsetExpr(nil, ColumnExpr{Qualifier: "t", Name: "a"}, nil); err == nil {
		t.Error("foldLimitOffsetExpr resolved a qualified column reference; the clause must decline it")
	}
}

// TestDefaultFoldIsCompiled asserts foldDefaultValue evaluates a DEFAULT clause
// by running a compiled program: shapes that need real opcodes (arithmetic, a
// scalar function call, CAST, a string operator) all fold, and a shape the
// codegen cannot lower folds to ok=false rather than being evaluated some
// other way.
//
// Every want below was measured against the 3.53.3 oracle through
// compat-harness/i2 DEFAULT battery (CREATE TABLE + an INSERT omitting the
// column + typeof()).
func TestDefaultFoldIsCompiled(t *testing.T) {
	for _, c := range []struct {
		sql  string // the DEFAULT clause's parenthesized expression text
		want Value
	}{
		{"1+2", Value{Typ: Int, I: 3}},
		{"abs(-3)", Value{Typ: Int, I: 3}},
		{"upper('ab')", Value{Typ: Text, S: []byte("AB")}},
		{"'a' || 'b'", Value{Typ: Text, S: []byte("ab")}},
		{"CAST('12ab' AS INTEGER)", Value{Typ: Int, I: 12}},
		{"5 & 3", Value{Typ: Int, I: 1}},
		{"1/0", Value{Typ: Null}},
		{"nullif(1,1)", Value{Typ: Null}},
		{"CASE WHEN 1 THEN 'y' ELSE 'n' END", Value{Typ: Text, S: []byte("y")}},
	} {
		e, perr := parseCheckExprText(c.sql)
		if perr != nil {
			t.Fatalf("%s: parse: %v", c.sql, perr)
		}
		got, ok := foldDefaultValue(e)
		if !ok {
			t.Errorf("DEFAULT (%s) did NOT fold -- the compiled fold declined a shape it must lower", c.sql)
			continue
		}
		if !valuesIdentical(got, c.want) {
			t.Errorf("DEFAULT (%s): got %v, want %v", c.sql, got, c.want)
		}
	}

	// A subquery and an aggregate are refused deliberately (foldDefaultValue's
	// doc comment): the parser has no pager and no accumulator, and
	// compileSelectNoFrom compiles against no database.
	for _, sql := range []string{"(SELECT 1)", "count(*)"} {
		e, perr := parseCheckExprText(sql)
		if perr != nil {
			t.Fatalf("%s: parse: %v", sql, perr)
		}
		if _, ok := foldDefaultValue(e); ok {
			t.Errorf("DEFAULT (%s) folded; it must decline", sql)
		}
	}
}
