// Enforces RULE #1: SQL executes only as a compiled VDBE program. There is no
// AST interpreter; a shape the compiler cannot lower is an error.
//
// C SQLite has one AST->value function, sqlite3ValueFromExpr (vdbemem.c:1978),
// limited to single constant tokens at prepare/DDL time. Everything else is
// compiled (sqlite3ExprCodeTarget, expr.c:4950; OP_Function, vdbe.c:8850).
//
// Every count below is a ratchet at 0. Do not raise it.
package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	maxEvalExprCallSites      = 0
	maxEvalExprFiles          = 0
	maxCompileStmtWriteRoutes = 0
)

// forbiddenASTDrivers are deleted AST-walking executors that must stay gone.
// Matched as declarations, so a comment naming one does not trip the gate.
var forbiddenASTDrivers = []string{
	"insertStmtExec",
	"updateStmtExec",
	"deleteStmtExec",
	"execStatement",
	"fireOneTrigger",
	"evalExpr",
	"evalSubquery",
	"evalExists",
	"evalIn",
	"updateFromExec",
	"execCompound",
	"queryAggregate",
	"execInsteadOfInsert",
	"execInsteadOfUpdate",
	"execInsteadOfDelete",
	"execInsteadOfUpdateFrom",
}

func engineNonTestSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read engine dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		out[n] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no non-test engine sources found -- the gate would pass vacuously")
	}
	return out
}

// TestRuleOneInterpreterRatchet fails if the AST interpreter grew.
func TestRuleOneInterpreterRatchet(t *testing.T) {
	src := engineNonTestSources(t)

	callSites, files := 0, 0
	for _, body := range src {
		n := strings.Count(body, "evalExpr(")
		if n > 0 {
			files++
			callSites += n
		}
	}
	check := func(what string, got, ratchet int, howToFix string) {
		t.Helper()
		switch {
		case got > ratchet:
			t.Errorf("RULE #1 REGRESSION: %s is %d, ratchet is %d.\n"+
				"The AST interpreter GREW. See AGENTS.md Rule 1 -- it is banned and under\n"+
				"removal; a shape the compiler cannot lower must be an ERROR, not something\n"+
				"handed to the interpreter. Do NOT raise the ratchet to make this pass.\n"+
				"%s", what, got, ratchet, howToFix)
		case got < ratchet:
			t.Logf("PROGRESS: %s is %d, below the ratchet of %d -- lower the constant in this "+
				"file in the same commit.", what, got, ratchet)
		}
	}

	check("evalExpr call sites", callSites, maxEvalExprCallSites,
		"Compile the expression to opcodes instead (SQLite: sqlite3ExprCodeTarget, expr.c:4950).")
	check("files calling evalExpr", files, maxEvalExprFiles,
		"An opcode body must own its value semantics, as SQLite's do -- not delegate to the interpreter.")
	// sql_eval.go was the interpreter; assert its absence, not its size.
	if _, ok := src["sql_eval.go"]; ok {
		t.Errorf("engine/sql_eval.go is back. It was the AST interpreter, and it was DELETED: "+
			"evalExpr no longer exists (see maxEvalExprCallSites = %d above). Value primitives "+
			"belong in a file named for what they DO -- value_arith.go, value_compare.go, "+
			"collate_resolve.go and the rest -- not in a file named for an evaluator this engine "+
			"does not have.", maxEvalExprCallSites)
	}

	routes := 0
	for _, body := range src {
		routes += strings.Count(body, "compileStmtWrite(")
	}
	check("routes into the OpStmt escape hatch (compileStmtWrite call sites)", routes, maxCompileStmtWriteRoutes,
		"compileWrite being \"TOTAL\" via this fallback is the BUG, not the guarantee. Give the shape real codegen.")
}

// TestRuleOneNoNewInterpreterEntryPoints fails if a deleted AST driver returns.
func TestRuleOneNoNewInterpreterEntryPoints(t *testing.T) {
	src := engineNonTestSources(t)
	var joined strings.Builder
	for _, body := range src {
		joined.WriteString(body)
		joined.WriteByte('\n')
	}
	all := joined.String()

	for _, fn := range forbiddenASTDrivers {
		for _, decl := range []string{"func " + fn + "(", "func (db *DB) " + fn + "("} {
			if strings.Contains(all, decl) {
				t.Errorf("RULE #1 REGRESSION: %q is BACK (%s). Every one of these walked an AST to "+
					"produce a value or run a statement, and all of them were deleted. SQL compiles to "+
					"a VDBE program and the program runs; a shape the compiler cannot lower is a hard "+
					"error, never a fallback to something that interprets it.", fn, decl)
			}
		}
	}
	if len(forbiddenASTDrivers) == 0 {
		t.Fatal("forbiddenASTDrivers is empty, so this test asserts nothing -- that is exactly the " +
			"tautology it was rewritten to remove. Put the names back.")
	}
}
