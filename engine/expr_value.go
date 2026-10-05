// This file holds the ONE seam that turns a standalone SQL expression -- one
// with no row behind it -- into a Value, and it does so by COMPILING the
// expression rather than walking it (AGENTS.md Rule 1).
//
// The mechanism is a FROM-less "SELECT <expr>" run through this package's
// single executor: execSelect -> execNoFrom -> tryVDBENoFrom (query.go),
// whose own contract is that a statement the bytecode compiler cannot lower is
// a HARD ERROR with no fallback of any kind. So every expression form these
// callers accept arrives as opcodes, and everything else declines cleanly.
//
// This is C SQLite's own shape for each of the three arguments served here.
// RULE ZERO citations, with the text that is actually on the line:
//
//	attach.c:403   "  sqlite3ExprCode(pParse, pFilename, regArgs);"
//	               ATTACH's PATH is coded into a register, then handed to
//	               attachFunc as an ordinary function call
//	               (attach.c:409, "    sqlite3VdbeAddFunctionCall(pParse, 0,
//	               regArgs+3-pFunc->nArg, regArgs+3,").
//
//	vacuum.c:130   "      sqlite3ExprCode(pParse, pInto, iIntoReg);"
//	               VACUUM INTO's TARGET is coded into a register and OP_Vacuum
//	               reads it as an sqlite3_value (vacuum.c:132,
//	               "    sqlite3VdbeAddOp2(v, OP_Vacuum, iDb, iIntoReg);";
//	               sqlite3RunVacuum's own "sqlite3_value *pOut" parameter).
//
//	fts5_main.c:1211  "    char *zSql = sqlite3Fts5Mprintf(&rc, \"SELECT %s\", zRankArgs);"
//	                  fts5's rank= ARGUMENTS are COMPILED and STEPPED --
//	                  fts5_main.c:1214 "      rc = sqlite3_prepare_v3(pConfig->db, zSql, -1,",
//	                  fts5_main.c:1219 "        if( SQLITE_ROW==sqlite3_step(pStmt) ){",
//	                  fts5_main.c:1227 "              pCsr->apRankArg[i] = sqlite3_column_value(pStmt, i);".
//
// NOT ONE of them is sqlite3ValueFromExpr (vdbemem.c:1978, "int
// sqlite3ValueFromExpr("). That matters, because the fts5 rank site had been
// recorded as TERMINAL-by-RULE-#1 on the grounds that its arguments are
// literal tokens. They ARE -- fts5's own rank grammar admits nothing else
// (fts5ConfigSkipLiteral, fts5_config.c:69, "static const char
// *fts5ConfigSkipLiteral(const char *pIn){") -- but C fts5 compiles them
// anyway, so RULE #1's carve-out does not cover the site and it is an ordinary
// compile.
package engine

import "fmt"

// selectExprValue evaluates e as the single result column of a FROM-less
// SELECT and returns that column's value: the one seam in this package for an
// expression that has to be evaluated with no row context behind it.
//
// The receiver may be nil. A FROM-less select needs no row source at all, and
// ATTACH's path is resolved by a parser that holds no pager (ParseAttachStmt,
// attach.go, which driver shares). A scalar subquery inside the expression
// still compiles
// there as long as ITS body reads no table either (selectNeedsNoRowSource,
// vdbe_codegen.go); one that does read a table needs a real pager, which
// VACUUM INTO has and passes ("VACUUM INTO (SELECT name FROM t2)") and which
// ATTACH's shared text->path entry point does not.
func (p *ReadOnlyPager) selectExprValue(e Expr) (Value, error) {
	return p.selectExprValueIn(e, nil, nil)
}

// selectExprValueIn is selectExprValue with the two pieces of surrounding
// statement state an expression may legitimately read even though it has no
// row of its own: outer, the enclosing frame a correlated reference binds
// against, and params, the statement's bound host parameters.
//
// It is what a MATCH's RIGHT operand needs when it arrives as an expression
// rather than as a register. C SQLite
// codes that operand into a
// register inside the enclosing statement -- wherecode.c:1584,
// "        codeExprOrVector(pParse, pRight, iTarget, 1);" -- so it sees the
// enclosing frame's registers and the statement's parameters, and NOT the
// MATCHed table's own row: whereexpr.c:1543's "(prereqExpr & prereqColumn)==0"
// is exactly the rule that a query string reading the MATCHed table's columns
// is never lifted out as a constraint at all. Passing outer rather than the
// row-bearing context reproduces that boundary.
func (p *ReadOnlyPager) selectExprValueIn(e Expr, outer *evalCtx, params []Value) (Value, error) {
	row, err := p.selectExprRowIn([]SelectColumn{{Expr: e}}, outer, params)
	if err != nil {
		return Value{}, err
	}
	return row[0], nil
}

// selectExprRow evaluates a whole result LIST in ONE compiled statement and
// returns its single row -- fts5FindRankFunction's own shape, which prepares
// "SELECT <args>" once and reads sqlite3_column_value(pStmt, i) for each i.
//
// A FROM-less select always produces exactly one row, so anything else is an
// internal inconsistency rather than an empty answer; C fts5 treats it the
// same way, turning a step that does not return SQLITE_ROW into an error
// (fts5_main.c:1232, "          rc = sqlite3_finalize(pStmt);", reached only
// from the else arm of the SQLITE_ROW test).
func (p *ReadOnlyPager) selectExprRow(cols []SelectColumn) ([]Value, error) {
	return p.selectExprRowIn(cols, nil, nil)
}

// selectExprRowIn is selectExprRow carrying the enclosing frame and the bound
// parameters -- see selectExprValueIn for what each one is for.
func (p *ReadOnlyPager) selectExprRowIn(cols []SelectColumn, outer *evalCtx, params []Value) ([]Value, error) {
	_, rows, err := p.execSelect(&SelectStmt{Columns: cols}, outer, params, "")
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != len(cols) {
		return nil, fmt.Errorf("engine: FROM-less expression list produced no row")
	}
	return rows[0], nil
}

// ---------------------------------------------------------------------------
// The POSITION's own name-resolution restriction.
//
// A row-less expression is not only EVALUATED somewhere, it is RESOLVED there,
// and the position decides what may appear in it. Compiling one as a FROM-less
// "SELECT <expr>" gives it a FROM-less SELECT's OWN NameContext -- where an
// aggregate and a window function are both perfectly legal ("SELECT min('a')"
// is a valid one-row query) -- which is MORE PERMISSIVE than the resolver two
// of this file's callers model. That gap is what requireZeroedNameContext
// closes, and it deliberately sits BESIDE the seam rather than inside it: the
// fts5 rank= site's counterpart really IS an ordinary prepared "SELECT <args>"
// (fts5_main.c:1211), so an aggregate there is legal and must stay legal.
// ---------------------------------------------------------------------------

// constantDefaultValue is sqlite3ValueFromExpr (vdbemem.c:1795-1965), and it is
// the ONE AST->value function RULE #1 (AGENTS.md Rule 1) permits: its
// contract is literal tokens, unary +/- and CAST of a literal, and it runs at
// DDL time only -- never during statement execution.
//
// It exists because ALTER TABLE ADD COLUMN's "constant default" test IS a call
// to that function and nothing else (alter.c:386-397):
//
//	rc = sqlite3ValueFromExpr(db, pDflt, SQLITE_UTF8, SQLITE_AFF_BLOB, &pVal);
//	...
//	if( !pVal ){
//	  sqlite3ErrorIfNotEmpty(pParse, zDb, zTab,
//	     "Cannot add a column with non-constant default");
//
// so "constant" here does NOT mean "has no free variables" -- (1+2) has none
// and is still refused. It means "this expression shape folds without an
// evaluator". Measured against 3.53.3 over a NON-EMPTY table, which is the
// only state in which the refusal fires at all:
//
//	(-(-9223372036854775808))  ACCEPT   real 9.223372036854776e+18
//	(-9223372036854775808)     ACCEPT   integer -9223372036854775808
//	(CAST('7' AS INTEGER))     ACCEPT   integer 7
//	42                         ACCEPT   integer 42
//	(1+2)                      REJECT
//	(abs(-3))                  REJECT
//	CURRENT_TIME               REJECT
//
// The nested unary minus is the case worth keeping: C folds the inner one to
// INTEGER minimum and then negating that overflows, which is why the column
// comes back REAL. negateValueUnary already carries exactly that promotion
// (value_arith.go), so the value falls out rather than being special-cased.
//
// Parentheses are transparent because this engine's parser does not build a
// node for them, exactly as SQLite's does not.
func constantDefaultValue(e Expr, enc TextEncoding) (Value, bool) {
	switch x := e.(type) {
	case LiteralExpr:
		return x.Val, true
	case UnaryExpr:
		// C strips TK_UPLUS in a loop and folds TK_UMINUS; both recurse here,
		// which is what admits "-(-<literal>)".
		v, ok := constantDefaultValue(x.X, enc)
		if !ok {
			return Value{}, false
		}
		switch x.Op {
		case "+":
			return v, true
		case "-":
			return negateValueUnary(v), true
		}
		return Value{}, false
	case CastExpr:
		// valueFromExpr recurses through TK_CAST with the cast's own affinity
		// (vdbemem.c:1820-1824).
		v, ok := constantDefaultValue(x.X, enc)
		if !ok {
			return Value{}, false
		}
		return castValueEnc(v, x.Type, enc), true
	}
	return Value{}, false
}
