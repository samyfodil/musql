// This file wires the VDBE into the engine: the pager-free entry points the
// compat-harness uses to gate the bytecode path against C SQLite.
//
// The VDBE is the SOLE executor, read and write alike. A statement the
// compiler cannot handle is a hard "VDBE-only" error (query.go's
// execSelect/execNoFrom for a SELECT, compileWriteProgram for a write), never
// a fallback. There is no mode knob and no second execution strategy to select.
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
)

// tryVDBENoFrom is execNoFrom's bytecode hook (see query.go). It compiles the
// statement; if compilation fails (a construct outside the compiler's
// scope), it returns handled=false so execNoFrom reports the hard
// "VDBE-only" error -- there is nothing left to fall back to. On a
// successful compile it runs the program and returns its result directly.
// outer, when non-nil, is a LIVE enclosing-row evalCtx -- the NEW/OLD row of a
// firing trigger (execTriggerBodySelect, trigger.go; the INSERT ... SELECT
// source of a body statement, insert_write.go/view_trigger.go), or the row a
// write clause's subquery is being evaluated for. It is threaded into the
// compile as compiler.rowOuter, whose doc comment (vdbe_codegen.go) explains
// why resolving through it and baking the result in as a literal is sound.
// Ignoring it used to make every FROM-less trigger-body step that names
// "new.x"/"old.x" -- "SELECT RAISE(IGNORE) WHERE ...", "SELECT CASE WHEN
// old.a=1 THEN RAISE(IGNORE) END" -- decline with "no such table: new".
//
// p IS threaded through as the compile's pager (compileSelectNoFromPager,
// vdbe_codegen.go) -- unlike the pager-free RunNoFrom/DisassembleNoFrom
// entry points (which exist to compile a FROM-less statement with no open
// database at all, and so must keep pager nil), this hook always has a real
// open database available (it is a method on *ReadOnlyPager, called from
// execNoFrom with that same p). Threading it in is what lets a subquery
// embedded in a top-level FROM-less statement's WHERE/select-list compile at
// all: a scalar subquery, [NOT] EXISTS, "X IN (SELECT ...)", and -- the
// common case -- SQLite's documented "X [NOT] IN <table-name>" form, which
// the parser desugars to "X IN (SELECT * FROM <table-name>)" (see
// parseInRHS, sql_parser.go).
func (p *ReadOnlyPager) tryVDBENoFrom(stmt *SelectStmt, outer *evalCtx, params []Value) (cols []string, rows [][]Value, handled bool, err error) {
	prog, cerr := compileSelectNoFromRow(p, stmt, outer)
	if cerr != nil {
		if errors.Is(cerr, errVDBESemantic) {
			// A genuine C-SQLite compile error (e.g. a subquery column-count
			// or missing-table violation). Surface it directly.
			return nil, nil, true, cerr
		}
		// Not compilable. Hand the REASON back, exactly as tryVDBEScan and
		// tryVDBECompound already do (see tryVDBEScan's comment): the caller
		// still reports the hard error, but names this cause in it. Dropping it
		// here made every FROM-less gap -- a missing builtin, a double-quoted
		// string, a correlated alias -- collapse into one undifferentiated
		// conformance-histogram bucket that named a SHAPE and no construct.
		return nil, nil, false, cerr
	}
	// execOuter, not exec: this function already RECEIVES the enclosing row's
	// evalCtx and hands it to the compiler, so dropping it at run time left a
	// FROM-less program reached as a subquery body with m.outer == nil and
	// nothing able to see the enclosing row at all. tryVDBEScan has
	// always threaded it (vdbe_run.go's own scan hook). Pre-existing and wider
	// than any one feature -- "SELECT count(*), (SELECT a + row_number() OVER
	// ()) FROM t1" is 3|2 in C SQLite and was "no such column: a" here, with
	// no aggregate in the subquery body at all. Found by round twenty-four's
	// window stream, which verified the fix and could not land it.
	rows, err = prog.execOuter(p, outer, params)
	return prog.ColNames, rows, true, err
}

// rowOuterCompile wraps a LIVE enclosing-row evalCtx as the one-level enclosing
// *compiler* the sub-compilers already know how to walk, so a compile entered
// with a row but WITHOUT an enclosing compile (tryVDBECompound,
// tryVDBENoFromAggregate below) reaches it through exactly the same last resort
// every nested compile does -- compileColumn's c.outerRowCtx() and
// compiler.affCtx's rowOuter tail (vdbe_codegen.go), and bindOuterAggRef's
// (vdbe_agg_codegen.go).
//
// This is a WRAPPER, not a new resolution rule, and that is the point: the
// order an unqualified name binds in is then the one those functions already
// implement, which is resolve.c's lookupName order (SQLite 3.53.3, manifest
// d4c0e51e...782c62) -- this compile's own FROM first, then each enclosing
// compile's, and the live row only when the whole walk has failed. It cannot
// preempt an arm's own FROM item, because resolveInScopes runs first.
//
// A nil row yields a nil compiler, so a TOP-LEVEL statement compiles with
// outer == nil exactly as before (which also keeps markWherePlanEligibility,
// where_plan_gate.go, eligible for it -- it declines any nested compile).
func rowOuterCompile(p *ReadOnlyPager, row *evalCtx) *compiler {
	if row == nil {
		return nil
	}
	return &compiler{pager: p, rowOuter: row}
}

// tryVDBENoFromAggregate is execNoFrom's bytecode hook for a FROM-less SELECT
// whose select-list contains an aggregate call (see query.go's execNoFrom
// and compileNoFromAggregate, vdbe_agg_codegen.go). It mirrors
// tryVDBENoFrom exactly, just against that dedicated aggregate compiler
// instead of compileSelectNoFromPager (which declines any aggregate select-
// list item outright).
//
// outer is threaded exactly as tryVDBENoFrom threads its own: into the compile
// (as the enclosing row, via rowOuterCompile) and into the run (execOuter, whose
// m.outer is what the aggregate opcodes evaluate an item's non-aggregate parts
// against -- aggResult's finalizeCtx, vdbe_agg.go). Dropping it made an
// UNQUALIFIED correlated reference beside the aggregate decline where the
// qualified spelling was answered: "SELECT a,(SELECT count(*)+a) FROM t2 GROUP
// BY a" is 1|2,2|3 in C SQLite and was "column reference in a FROM-less
// aggregate" here, while "(SELECT count(*)+t2.a)" already worked (only the
// qualified form reaches rewriteSelectOuterRefs).
//
// A RE-ASSOCIATED aggregate is unaffected: checkAggregateAssociation (sql_agg.go,
// run by planNoGroupAggregate before any of this) consults the LOCAL scopes only,
// so "SELECT (SELECT avg(b)) FROM t2 GROUP BY a" -- which C SQLite computes
// over the whole GROUP -- still declines rather than averaging the one anchor row.
func (p *ReadOnlyPager) tryVDBENoFromAggregate(stmt *SelectStmt, outer *evalCtx, params []Value) (cols []string, rows [][]Value, handled bool, err error) {
	prog, cerr := compileNoFromAggregate(p, stmt, rowOuterCompile(p, outer))
	if cerr != nil {
		if errors.Is(cerr, errVDBESemantic) {
			return nil, nil, true, cerr // see tryVDBENoFrom
		}
		return nil, nil, false, cerr // see tryVDBENoFrom: the reason is the deliverable
	}
	rows, err = prog.execOuter(p, outer, params)
	return prog.ColNames, rows, true, err
}

// tryVDBEScan is execSelect's bytecode hook for a single-table (no JOIN)
// SELECT (see query.go). It mirrors tryVDBENoFrom exactly, just against
// compileSelectScan (the single-/multi-table scan compiler, vdbe_scan.go)
// instead of compileSelectNoFromPager.
func (p *ReadOnlyPager) tryVDBEScan(stmt *SelectStmt, outer *evalCtx, params []Value, cacheKey string) (cols []string, rows [][]Value, handled bool, err error) {
	// outer is threaded into the COMPILE as compiler.rowOuter, exactly as
	// tryVDBENoFrom already threads its own -- see compileSelectScanRow
	// (vdbe_scan.go) for what that buys (an UNQUALIFIED correlated reference)
	// and why resolving it inside compileColumn is the safe place. Only for a
	// subquery/write-path compile: cacheKey is non-empty ONLY for a top-level
	// QueryArgs read, whose outer is nil anyway, and baking an enclosing row's
	// value in as a literal is sound solely because the Program is never reused
	// against a different row. The cacheKey guard states that invariant rather
	// than relying on it.
	var rowOuter *evalCtx
	if cacheKey == "" {
		rowOuter = outer
	}
	prog, cerr := compileSelectScanRow(p, stmt, nil, rowOuter)
	if cerr != nil {
		if errors.Is(cerr, errVDBESemantic) {
			return nil, nil, true, cerr // see tryVDBENoFrom
		}
		// Not compilable. Hand the REASON back so the caller's hard error names
		// the actual gap (compileSelectScan's errVDBEUnsupported text) instead
		// of a generic "not compilable": the conformance gate buckets these
		// messages into its unsupported-feature histogram, and a single
		// undifferentiated message collapses every distinct gap into one
		// unactionable bucket.
		return nil, nil, false, cerr
	}
	// Memoize this top-level read's compiled Program for reuse (cacheKey is the
	// SQL text for a QueryArgs call, "" for every subquery / write-path caller
	// -- see execSelect's cacheKey doc and the planCache field).
	p.cachePlan(cacheKey, prog)
	rows, err = prog.execOuter(p, outer, params)
	return prog.ColNames, rows, true, err
}


// tryVDBECompound is execSelect's bytecode hook for a compound SELECT
// (UNION/UNION ALL/INTERSECT/EXCEPT). It mirrors tryVDBEScan/tryVDBENoFrom
// exactly: compileCompound returning a non-semantic error means handled=false
// (the caller reports the hard "VDBE-only" error -- there is nothing left to
// fall back to); on a successful compile, it returns the bytecode result
// directly.
//
// outer, when non-nil, is the LIVE enclosing row a compound reached as a
// SUBQUERY BODY is being evaluated for (rowEvalCtx, write_update_delete.go,
// builds one). It is threaded exactly as tryVDBENoFrom
// threads its own: into the compile via rowOuterCompile, so each ARM resolves an
// UNQUALIFIED correlated reference through compileColumn's own last resort, and
// into the run via execOuter. Dropping it made "SELECT a,(SELECT a UNION SELECT
// 99 ORDER BY 1 LIMIT 1) FROM t2 GROUP BY a" decline as "no such column: a"
// while the QUALIFIED "(SELECT t2.a UNION ...)" was already answered -- the
// substitution that ran ahead of this call reached only the qualified spelling.
func (p *ReadOnlyPager) tryVDBECompound(stmt *SelectStmt, outer *evalCtx, params []Value) (cols []string, rows [][]Value, handled bool, err error) {
	cp, cerr := compileCompound(p, stmt, rowOuterCompile(p, outer))
	if cerr != nil {
		if errors.Is(cerr, errVDBESemantic) {
			return nil, nil, true, cerr // see tryVDBENoFrom
		}
		// The compiler's own reason is RETURNED rather than dropped: the caller
		// still reports the hard error, but names this cause in it. Without
		// that, every compound decline collapses into one opaque bucket in the
		// corpus histogram -- and that histogram is the classification this
		// project's method runs on.
		return nil, nil, false, cerr
	}
	cols, rows, err = cp.execOuter(p, outer, params)
	return cols, rows, true, err
}

// DisassembleScan compiles a single-table SELECT against p's schema and
// returns its VDBE program's EXPLAIN-style disassembly.
func DisassembleScan(p *ReadOnlyPager, sqlText string) (string, error) {
	stmt, err := ParseSelect(sqlText)
	if err != nil {
		return "", err
	}
	prog, err := compileSelectScan(p, stmt, nil)
	if err != nil {
		return "", err
	}
	return prog.Disassemble(), nil
}

// valuesEqualExact reports whether two Values are identical in storage class
// and payload -- used by tests that compare two VDBE runs of the same
// statement cell-for-cell (no INTEGER/REAL storage-optimization tolerance is
// needed here -- that tolerance only applies across the disk-record
// boundary).
func valuesEqualExact(a, b Value) bool {
	if a.Typ != b.Typ {
		return false
	}
	switch a.Typ {
	case Null:
		return true
	case Int:
		return a.I == b.I
	case Float:
		return a.F == b.F
	case Text, Blob:
		return bytes.Equal(a.S, b.S)
	}
	return false
}

// RunNoFrom compiles and executes a FROM-less SELECT through the VDBE
// bytecode VM and returns its column names and rows. It is pager-free: a
// FROM-less query touches no table, so no open database is required. This is
// the harness's gate entry point for such statements, run against real C
// SQLite.
//
// It returns a wrapped errVDBEUnsupported error for any statement outside
// the compiler's current scope (e.g. a column reference, which a FROM-less
// statement can never resolve).
func RunNoFrom(sqlText string, args []Value) (cols []string, rows [][]Value, err error) {
	stmt, err := ParseSelect(sqlText)
	if err != nil {
		return nil, nil, err
	}
	if err := resolveNoFromLimitOffset(nil, stmt, args); err != nil {
		return nil, nil, err
	}
	prog, cerr := compileSelectNoFrom(stmt)
	if cerr != nil {
		return nil, nil, cerr
	}
	rows, rerr := prog.exec(nil, args)
	if rerr != nil {
		return nil, nil, rerr
	}
	return prog.ColNames, rows, nil
}

// resolveNoFromLimitOffset resolves a LIMIT/OFFSET expression into a concrete
// Limit/Offset before its statement is compiled, mirroring execSelect's own
// one-time resolution so that the one VDBE entry point that does not go through
// execSelect -- RunNoFrom, the compat-harness gate's pager-free entry point --
// handles "LIMIT ?" the same way the driver does.
//
// It used to serve QueryVDBE as well. That was a SECOND door onto the same
// executor: it parsed, compiled and ran a SELECT while skipping the plan cache,
// PRAGMA/EXPLAIN dispatch, the CTE scope push, schema-qualifier validation, the
// wal-index read classification, the deferred-FK guards, the frozen-schema
// check, the fts5 table-function rewrite and both flatten passes -- everything
// QueryArgs does around the same compile. Its name also asserted a VDBE /
// non-VDBE distinction that stopped existing when the AST interpreter was
// deleted: execSelect is VDBE-only and hard-errors (query.go), so "run this via
// the VDBE" and "run this" are the same sentence. Deleted; its ~260 call sites
// are QueryArgs now. RunNoFrom is the remaining second door and is a narrower
// one -- it is pager-free, which QueryArgs is not.
//
// The resolution is COMPILED, through the same foldLimitOffsetExpr
// (vdbe_codegen.go) resolveSubProgramLimitOffset uses -- the FROM-less codegen
// over a synthetic one-column SELECT, run once with this statement's own bound
// parameters, so "LIMIT ?" lands on OpVariable and "LIMIT 1+1" on the ordinary
// arithmetic opcodes. It used to walk the AST here instead, which made this the
// SECOND evaluator of a clause the compiler already owns everywhere else.
//
// That is what C SQLite does with the clause too: computeLimitRegisters
// (select.c:2517) codes both halves into registers with sqlite3ExprCode --
//
//	select.c:2548   sqlite3ExprCode(pParse, pLimit->pLeft, iLimit);
//	select.c:2556   sqlite3ExprCode(pParse, pLimit->pRight, iOffset);
//
// -- once, before the scan, exactly like this one-time resolution.
//
// pager is the statement's own, and it is load-bearing rather than plumbing:
// "LIMIT (SELECT a FROM t5)" is a shape the grammar allows and parseLimitTerm
// exists to accept, and it compiles into a sub-Program only against a real
// database. A caller with a live pager passes it for exactly that; RunNoFrom
// passes nil because that route is pager-free by construction (a FROM-less
// opens no database), and compileSelectNoFromPager(nil, ...) is precisely
// compileSelectNoFrom -- so a LIMIT expression needing a row source declines
// there rather than compiling against no database.
func resolveNoFromLimitOffset(pager *ReadOnlyPager, stmt *SelectStmt, args []Value) error {
	if stmt.LimitParam == nil && stmt.OffsetParam == nil {
		return nil
	}
	if stmt.LimitParam != nil {
		n, err := foldLimitOffsetExpr(pager, stmt.LimitParam, args)
		if err != nil {
			return err
		}
		stmt.Limit = &n
		stmt.LimitParam = nil
	}
	if stmt.OffsetParam != nil {
		n, err := foldLimitOffsetExpr(pager, stmt.OffsetParam, args)
		if err != nil {
			return err
		}
		stmt.Offset = &n
		stmt.OffsetParam = nil
	}
	return nil
}

// DisassembleNoFrom compiles a FROM-less SELECT and returns its VDBE program's
// EXPLAIN-style disassembly, for debugging and the bytecode-oracle comparison.
func DisassembleNoFrom(sqlText string) (string, error) {
	stmt, err := ParseSelect(sqlText)
	if err != nil {
		return "", err
	}
	prog, err := compileSelectNoFrom(stmt)
	if err != nil {
		return "", err
	}
	return prog.Disassemble(), nil
}

// eval evaluates one schema-time row expression against the row ctx carries --
// ctx.vals for the columns, ctx.rowids[0] for the rowid pseudo-column -- by
// SEEDING the register block compileSelfRowExpr laid out and running the
// program. See selfRowExpr (vdbe_codegen.go) for the four expression kinds
// this serves and for C SQLite's own iSelfTab register block it reproduces
// (expr.c:5047-5068).
//
// ctx is read for two things beyond the row: its pager, which is what makes
// "PRAGMA case_sensitive_like" and the database text encoding agree everywhere
// (evalCtx.likeCaseSensitive / evalCtx.encoding resolve through the same
// field); and its single table scope, whose COLUMN LIST decides whether this
// program still describes this row at all (runnable, below).
//
// NOTHING HERE INTERPRETS ANY MORE. A program that does not describe ctx's row
// is RE-COMPILED against it (restamp) -- a compile, then a run, which is RULE
// #1's own shape -- and an expression that will not lower is an error. The arm
// this replaced was the last un-compiled evaluation of a schema-time expression
// in the engine; see restamp for what was measured before it was deleted.
func (p *selfRowExpr) eval(ctx *evalCtx) (Value, error) {
	if ctx == nil {
		// Answered here rather than left to restamp, which reads ctx.tables
		// for the scope to re-compile against and would nil-dereference --
		// invariant 3 forbids that without exception. No caller produces this
		// -- every one builds a real evalCtx (rowEvalCtx, computeGeneratedInto)
		// -- so a clean error is both unreachable and the only sound answer:
		// there is no row here to evaluate anything against.
		return Value{}, errors.New("engine: schema-time expression evaluated with no row context")
	}
	q := p
	if !q.runnable(ctx) {
		var rerr error
		if q, rerr = p.restamp(ctx); rerr != nil {
			return Value{}, rerr
		}
	}
	// These run once per ROW of a scan or a write, so the machine and its
	// register file are pooled rather than allocated per evaluation: built
	// fresh each time, the seam measured 331ns/1192B for an expression that had
	// cost 150ns/0B before, which would have made compiling a
	// generated column a read-path regression rather than a win.
	m, _ := selfRowVMs.Get().(*vdbe)
	if m == nil {
		m = &vdbe{}
	}
	if cap(m.regs) < q.prog.NReg {
		m.regs = make([]Value, q.prog.NReg)
	} else {
		m.regs = m.regs[:q.prog.NReg]
		clear(m.regs) // a stale value must never be readable as a register
	}
	// The run-once cache slots a FROM-less subquery in this expression takes
	// (compileSelfRowExpr serves nSub; every other counter still declines).
	// Sized and CLEARED per evaluation rather than kept, for the two reasons
	// compileAggItemProgram's identical pooled machine gives
	// (vdbe_agg_item_codegen.go): the slot numbering belongs to ONE expression's
	// program and this machine is shared by every expression of every table, so
	// a kept cache would answer one CHECK out of another's slot; and clearing it
	// re-runs the body per row, which is what the arm it replaced did (it
	// cached nothing). Almost always a no-op -- NSubCache is 0 for
	// every ordinary CHECK, generated column, DEFAULT and index expression.
	if cap(m.subCache) < q.prog.NSubCache {
		m.subCache = make([]subCacheEntry, q.prog.NSubCache)
	} else {
		m.subCache = m.subCache[:q.prog.NSubCache]
		clear(m.subCache)
	}
	// Cursors and record registers, for the one caller that compiles WITH a
	// pager (compileSelfRowExprPaged): a virtual table's RETURNING list, whose
	// "RETURNING (SELECT b FROM t2)" is a real subquery over a real table and
	// therefore opens a cursor. Sized and CLEARED per evaluation for the same
	// reason subCache is: the numbering belongs to ONE expression's program and
	// this machine is shared by every expression of every table, so a kept
	// cursor would answer one expression out of another's slot.
	//
	// Both are 0 for every other caller -- a CHECK, a generated column, a
	// DEFAULT, an index key, a partial-index WHERE -- because those compile
	// with no pager and compileSelfRowExprPaged still refuses a cursor there.
	if cap(m.cursors) < q.prog.NCursors {
		m.cursors = make([]*vdbeCursor, q.prog.NCursors)
	} else {
		m.cursors = m.cursors[:q.prog.NCursors]
		clear(m.cursors)
	}
	if cap(m.recRegs) < q.prog.NRecRegs {
		m.recRegs = make([][]Value, q.prog.NRecRegs)
	} else {
		m.recRegs = m.recRegs[:q.prog.NRecRegs]
		clear(m.recRegs)
	}
	m.pager = ctx.pager
	// The statement's BOUND PARAMETERS. compileSelfRowExpr lowers a ParamExpr
	// to OpVariable (vdbe_codegen.go), whose body is paramAt(m.params, P1), so
	// leaving this nil answered NULL for every "?" in a compiled expression.
	//
	// Latent until a caller fed a USER-BOUND expression through here: every
	// earlier one is schema-time (a CHECK, a generated column, a DEFAULT, an
	// index key, a partial-index WHERE), and C SQLite forbids a parameter
	// in all of those -- "parameters prohibited in CHECK constraints"
	// (build.c) and its siblings. The RETURNING capture
	// (buildVtabReturningPlan, vtab_write.go) is the first, and it made this a
	// SILENT WRONG VALUE rather than an error:
	//
	//	CREATE VIRTUAL TABLE r USING rtree(id,x0,x1);
	//	INSERT INTO r VALUES(1,2,3) RETURNING id, ?            -- bound 42
	//	  -> "1, NULL" here, "1, 42" in the 3.53.3 oracle
	//	INSERT INTO r VALUES(2,2,3)
	//	  RETURNING CASE WHEN ? THEN 'yes' ELSE 'no' END       -- bound 1
	//	  -> "no" here, "yes" there
	//
	// Cleared below beside m.pager, for the same reason: this machine is POOLED
	// across every expression of every table, and a retained params slice would
	// let one statement's bound values be read by the next.
	m.params = ctx.params
	// Same reason as m.params: this pooled machine has neither a pager nor a
	// write context, so LIKE would read a false flag. See vdbe.likeCaseSensitive.
	m.caseSensitiveLike = ctx.likeCaseSensitive()
	if len(ctx.rowids) > 0 {
		m.regs[q.rowidReg] = ctx.rowids[0]
	}
	copy(m.regs[q.colBase:q.colBase+q.nCols], ctx.vals)
	_, err := m.run(q.prog.Insns)
	out := m.regs[q.resultReg]
	m.pager = nil
	m.params = nil
	m.caseSensitiveLike = false
	selfRowVMs.Put(m)
	if err != nil {
		return Value{}, err
	}
	return out, nil
}

// restamp re-compiles p's body against the scope ctx's row actually belongs to,
// for the cases runnable refuses: a schema object carrying no prepared program
// at all, and a program whose column list is no longer the one this row is laid
// out by (a savepoint clone, an ALTER that spliced the list). It covers what
// the deleted arm used to, done the way RULE #1 requires --
// compile, then run -- and it is why deleting that arm costs no answer that
// was reachable before: the stale-list case still yields the value it did.
//
// MEASURED before deleting the arm, over the whole 1,314-file TCL corpus with
// both halves of the seam instrumented (every chunk wrong=0 panics=0): the
// arm was reached 65 times, EVERY one of them because compileExpr had
// declined the expression -- not once for a stale list, a missing scope or a
// short row. The 65 came from exactly two roots, both now COMPILED rather than
// declined: a three-part reference inside a CHECK body or a partial index's
// WHERE (compiler.ignoreDbQualifier, C's resolve.c:316) and "rowid" inside a
// CHECK on a rowid table (checkProgramScope, C's resolve.c:564-568). So this
// function's own fallback path is measured at zero, which is also why it does
// not cache: a recompile it never performs needs no memo.
//
// An expression that will not lower even here is an ERROR. That is RULE #1's
// answer and it is not a capability loss: it is reached only where the old arm
// would have taken the very same un-lowerable tree, which the measurement
// above puts at zero occurrences corpus-wide.
func (p *selfRowExpr) restamp(ctx *evalCtx) (*selfRowExpr, error) {
	if p == nil || p.expr == nil {
		return nil, errors.New("engine: schema-time expression has no body to evaluate")
	}
	if len(ctx.tables) != 1 {
		return nil, fmt.Errorf("%w: a schema-time expression needs exactly one table scope", errVDBEUnsupported)
	}
	q := compileSelfRowExpr(ctx.tables[0], p.expr, p.ignoreDbQualifier, p.pureCtx)
	if !q.runnable(ctx) {
		return nil, fmt.Errorf("%w: this schema-time expression cannot be compiled against the row it is evaluated over", errVDBEUnsupported)
	}
	return q, nil
}

// runnable reports whether p's compiled program may be run against ctx's row.
// A false answer sends eval to restamp, which re-compiles against ctx's own
// scope; only an expression that will not lower there is an error. Each guard
// is covered by its own test in schema_expr_test.go
// (TestSelfRowExprEvalGuards), because a guard nothing exercises is
// indistinguishable from one that does not work.
//
//   - a nil receiver, which no caller should produce (each one substitutes a
//     raw selfRowExpr where a schema object carries no prepared program).
//     Guarded rather than trusted: a nil dereference here would be a panic, and
//     AGENTS.md invariant 2 has no exceptions.
//   - prog == nil: compileExpr could not lower this expression, or the compile
//     needed VM state this program is not given (compileSelfRowExpr).
//   - ctx == nil, which eval answers before calling runnable at all: there is
//     no row there to evaluate anything against (see eval).
//   - exactly one table scope. It is the scope the row belongs to, and the one
//     whose column list the program was compiled against; anything else means
//     this evalCtx is not the shape every caller builds (rowEvalCtx,
//     computeGeneratedInto).
//   - the SAME column list the program was compiled against, by slice identity
//     -- see selfRowExpr.cols and sameColumnList (vdbe_codegen.go) for why an
//     EQUAL list is not good enough and an in-place rename still is.
//   - a row at least as wide as that list. eval copies ctx.vals into the
//     register block and the program reads register colBase+i for column i, so
//     a short row would answer from a cleared register rather than from the
//     column -- a wrong value, not a panic, which is why it is checked and not
//     left to copy's own clamping.
//   - a ROWID to seed, whenever the scope admits the pseudo-column at all. The
//     rowid register is seeded only when ctx carries one, so a scope that
//     resolves "rowid" paired with a ctx that has none would read a CLEARED
//     register -- NULL, silently, exactly the failure mode the width check
//     above exists to prevent. Every caller that admits the name does supply
//     one (rowEvalCtx, multiOrOrderCursor); the two that cannot -- a DEFAULT
//     clause and a generated column, neither of which C resolves "rowid" in
//     either (resolve.c:626 excludes NC_GenCol from the rowid match) -- set
//     noRowid and are unaffected.
func (p *selfRowExpr) runnable(ctx *evalCtx) bool {
	if p == nil || p.prog == nil || ctx == nil {
		return false
	}
	if len(ctx.tables) != 1 || !sameColumnList(p.cols, ctx.tables[0].cols) {
		return false
	}
	if !ctx.tables[0].noRowid && len(ctx.rowids) == 0 {
		return false
	}
	return len(ctx.vals) >= p.nCols
}

// selfRowVMs pools the register machines selfRowExpr.eval runs its programs on.
// A pooled machine carries its register slice and its run-once subquery cache:
// these programs have no cursors, no record registers, no sorters and no
// distinct sets -- compileSelfRowExpr DECLINES any compile that allocated one,
// so a program that reaches this pool provably needs none of them -- and eval
// clears both slices and re-seeds the pager on every use.
var selfRowVMs sync.Pool
