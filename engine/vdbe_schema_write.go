package engine

import "fmt"

// This file lowers the three DIRECT SCHEMA-CATALOG writes into the write VM:
// "INSERT INTO / UPDATE / DELETE FROM sqlite_master" run against the catalog
// itself, which is what "PRAGMA writable_schema=ON" exists to unlock.
//
// # What the C does
//
// It does nothing special at all, and that is the whole design of this file.
// A catalog write is not a statement kind in SQLite: the flag's ONLY effect on
// the write path is that tabIsReadOnly (delete.c:98-108) stops reporting the
// table whose root page is 1 -- the one build.c:2674 sets TF_Readonly on --
// as read-only,
//
//	if( (pTab->tabFlags & TF_Readonly)!=0 ){
//	  return sqlite3WritableSchema(db)==0 && pParse->nested==0;
//	}                                            -- delete.c:105-107
//
// after which sqlite3IsReadOnly (delete.c:119-123) stops raising "table %s may
// not be modified" and the statement falls through to the SAME
// sqlite3Insert/sqlite3Update/sqlite3DeleteFrom codegen every other table's
// write gets: the WHERE into sqlite3WhereBegin (delete.c:526, update.c:742),
// each SET right-hand side into a register inside that scan (update.c:955),
// each VALUES expression into its own register (insert.c:1430-1431).
//
// And nothing follows it. Grepping insert.c/update.c/delete.c for ParseSchema
// finds NO occurrence: a direct catalog write emits no OP_ParseSchema and no
// schema-cookie bump, which is exactly the probed behaviour
// schema_write_direct.go's own package comment records -- the connection that
// made the write keeps running on its already-loaded schema, and only a REOPEN
// sees the corruption. There is no reload for this file to model.
//
// So the split here is the C's own. The WHERE, the SET list and the VALUES
// tuples are CODED, and the MUTATION -- which overlay row each value lands on,
// what the affinities do to it, which session flags the removal sets -- stays
// with schema_write_direct.go's overlay, in one call, the way OpVWrite leaves
// the mutation with the virtual-table module.
//
// # Why this route codes its own WHERE and SET list
//
// The shorter lowering would have been to hand the row-at-a-time seams
// (writeRowSelected / writeApplySetList, vtab_write.go) a new opcode and let
// them decide the WHERE and produce the SET values. This route deliberately
// does not call them: the WHERE compiles to opcodes over a cursor and the SET
// list compiles to a register block, exactly as the virtual-table
// UPDATE/DELETE promotion (vdbe_vtab_write.go) did one batch earlier -- and
// the ONLY half of writeApplySetList this route still uses is
// writeApplySetListPre, its already-COMPILED twin, which evaluates nothing.
// Both of those seams are decline stubs today for exactly that reason: every
// live caller now arrives with values the compiled scan already produced.
//
// # Three opcodes, and why the prepare-time half is one of them
//
// OpSchemaWritePre runs FIRST in all three programs and asks only what the C
// asks before its VM exists -- sqlite3IsReadOnly, the SET/IDLIST name
// resolution (update.c:500) and the VALUES arity check (insert.c:1249-1252).
// It is an OPCODE rather than a compile-time check because every one of those
// answers depends on connection state no write-program cache key covers
// (cachedWriteProgram keys on the SQL text, invalidated only by schemaGen and
// txGen): the writable_schema flag itself, which "PRAGMA writable_schema"
// moves without touching either generation, so a statement compiled and
// cached while it was ON would run again after it went OFF. Asking at run time
// cannot go stale, and it costs nothing: these are a few map/slice walks over
// a five-column table.
//
// # What is DECLINED, and what that means
//
// A decline here is a HARD ERROR (errVDBEUnsupported, RULE #1): there is no
// second route for the statement to land on, so each shape below is declined
// because this lowering does not model it, never because something else does.
//
//   - the clauses no catalog write has ever modelled (a FROM/RETURNING/OR
//     clause on the UPDATE, RETURNING on the DELETE, a SELECT source /
//     RETURNING / DEFAULT VALUES / OR / UPSERT on the INSERT);
//   - a leading WITH, an "INDEXED BY", an "AS alias" on the target. The scope
//     a catalog write resolves against is sqlite_master's own fixed
//     five-column shape and nothing else (see beginSchemaWriteScan), so there
//     is no CTE namespace to consult, no index for the hint to name, and no
//     alias for a qualified reference to use;
//   - a trigger body (trig != nil), which keeps this lowering's blast radius
//     to top-level statements -- and is also what makes OpSchemaWritePre's
//     prepare-failure marking correct without threading a trigger-depth flag
//     through it;
//   - RAGGED VALUES tuples on the INSERT, whose register block has no single
//     stride. Every such statement is an ARITY ERROR anyway: the tuples cannot
//     all match the target's five columns.
//
// A subquery is NOT declined. It compiles against the pre-statement snapshot
// (db.SnapshotPager(), through writeSubqueryPager), and carrying it as
// Program.WritePager is what stops cachedWriteProgram reusing this program --
// see compileViewDeleteStmt's identical arrangement.

// schemaWritePlan is the P4 of all three catalog-write opcodes: which verb,
// which spelling of the target the statement used (for byte-identical error
// text), and the statement itself.
//
// The statement is held whole for vtabInsertPlan's reason -- the overlay half
// (schema_write_direct.go) reads its IDLIST, its SET target names and its
// table name, and splitting those out here would mean maintaining a second
// copy of a mapping that has to agree with it. No EXPRESSION in it is ever
// EVALUATED on this route: the compilers below decline every shape whose
// values are not the register block, which is what makes this a promotion
// rather than a hand-off under another name.
type schemaWritePlan struct {
	verb  string // "INSERT INTO" / "UPDATE" / "DELETE FROM", for error text
	table string // the statement's own spelling of the target
	ins   *insertStmt
	upd   *updateStmt
	del   *deleteStmt
}

// preflightCols is the assigned-column list writableSchemaPreflight inspects
// (schema_write_direct.go): an UPDATE's SET targets, or an INSERT's column
// list. A DELETE assigns nothing.
func (p *schemaWritePlan) preflightCols() []string {
	switch {
	case p.upd != nil:
		cols := make([]string, 0, len(p.upd.sets))
		for _, a := range p.upd.sets {
			cols = append(cols, a.col)
		}
		return cols
	case p.del != nil:
		return nil
	default:
		return p.ins.cols
	}
}

// schemaWriteState is what a running catalog-write program accumulates: the
// catalog row set its scan cursor was opened over (so a selected rowid can be
// turned back into the wsCatalogKey the overlay is keyed by), and the rows the
// WHERE has selected so far. It is SQLite's RowSet (delete.c:582) / ephemeral
// table (update.c:1320-1327) under another name -- per-EXECUTION state on the
// machine, never anything the (cacheable) Program carries.
type schemaWriteState struct {
	rows []catalogRow
	sel  []schemaWriteSelected
}

// schemaWriteSelected is one row the WHERE selected: its 1-based catalog
// rowid, and its SET right-hand sides already evaluated, in STATEMENT order
// (nil for a DELETE).
type schemaWriteSelected struct {
	rowid int64
	vals  []Value
}

// beginSchemaWriteScan is the prologue compileSchemaCatalogUpdate and
// compileSchemaCatalogDelete share: the scope, the preflight opcode, and the
// OpInit/OpOpenDerived/OpRewind head of the loop plus the per-row rowid read.
//
// The scope is sqlite_master's own fixed five-column shape
// (sqliteSchemaCatalogColumns, query.go) named for the table. noRowid stays
// FALSE, which is what keeps "WHERE rowid=2" resolving, and the corpus uses it
// (pragma.test's "UPDATE sqlite_schema SET rootpage=3 WHERE rowid=2").
// Whether that reference is ALLOWED is a separate, state-dependent question
// OpSchemaWritePre asks (wsGuardedColumnDecline).
//
// The scope is named for the STATEMENT'S OWN SPELLING of the catalog, not for
// the fixed "sqlite_master" the overlay's own rows carry, and that choice is
// measured. It makes a qualified reference in the WHERE resolve exactly as it
// does on the READ path -- both self-consistent spellings work ("UPDATE
// sqlite_schema ... WHERE sqlite_schema.name" and the sqlite_master pair),
// which is what C SQLite answers (verified: all four combinations succeed on
// 3.53.3; pinned in compat-harness/writable_schema_write_test.go's "both
// catalog spellings" case). It also RETIRES a divergence: naming the scope for
// the fixed "sqlite_master" instead would answer "no such table:
// sqlite_schema" to "UPDATE sqlite_schema SET sql='x' WHERE
// sqlite_schema.name='t'", where the oracle updates the row.
//
// The CROSS spellings ("UPDATE sqlite_master ... WHERE sqlite_schema.name")
// are still an error, and that is a pre-existing, engine-WIDE gap this batch
// neither widens nor narrows: C SQLite accepts
// them through lookupName's "if( pTab->tnum!=1 ) continue;" escape hatch
// (resolve.c:427-430) into isValidSchemaTableName (resolve.c:228-249), which
// this engine has no port of anywhere -- its read path answers "no such table:
// sqlite_schema" for "SELECT sqlite_schema.name FROM sqlite_master" too.
// Naming the scope for the statement's spelling makes the write path agree
// with the read path about which half of that gap works, rather than adding a
// second, differently-shaped one.
//
// No OLD row is read into registers, unlike the view path's twin: nothing
// downstream needs one. The WHERE and every SET right-hand side read the
// columns they name straight off the cursor (compileExpr emits its own
// OpColumn), and the overlay half re-reads the row it is editing from the very
// same source. The rowid IS read per row, because it is the row's identity in
// that source -- delete.c:552's "sqlite3ExprCodeGetColumnOfTable(v, pTab,
// iTabCur, -1, iKey);" into the RowSet, and update.c:861's "OP_Rowid iEph,
// regOldRowid".
func (db *DB) beginSchemaWriteScan(plan *schemaWritePlan, subq bool) (*compiler, int, int, int, *ReadOnlyPager, error) {
	cols := sqliteSchemaCatalogColumns()
	scope := compileScope{
		tableScope: tableScope{name: plan.table, cols: cols, colIndex: buildColIndex(cols), offset: 0},
		cursor:     0,
	}
	c := &compiler{scopes: []compileScope{scope}, nCursor: 1}
	var writePgr *ReadOnlyPager
	if subq {
		// The pre-statement image -- writeSubqueryPager (vdbe_write.go) is
		// db.SnapshotPager(), the one frozen snapshot every write program's
		// subqueries read. Declaring it as the program's
		// WritePager is what stops cachedWriteProgram reusing this program
		// across a changed database; see compileViewDeleteStmt's own note for
		// the live bug that arrangement exists to prevent.
		sp, perr := db.writeSubqueryPager()
		if perr != nil {
			return nil, 0, 0, 0, nil, declineOrSemantic(perr)
		}
		c.pager = sp
		writePgr = sp
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpSchemaWritePre, P4: plan})
	src := &derivedSource{schemaWrite: true, tbl: &resolvedTable{cols: cols, ipkIndex: -1}}
	c.emit(Instruction{Op: OpOpenDerived, P1: 0, P4: src})
	rewind := c.emit(Instruction{Op: OpRewind, P1: 0})
	loopTop := c.here()
	rowidReg := c.allocN(1)
	c.emit(Instruction{Op: OpRowid, P1: 0, P2: rowidReg})
	return c, loopTop, rewind, rowidReg, writePgr, nil
}

// emitSchemaWhere emits the statement's WHERE over the row the scan cursor is
// on, returning the jump to patch to the row's end (or -1 when there is no
// WHERE). Identical to emitVtabWhere, including the inWhereConjunct flag: this
// is the statement's own top-level WHERE, handed to sqlite3WhereBegin exactly
// as a SELECT's is (delete.c:526, update.c:742).
func emitSchemaWhere(c *compiler, where Expr) (int, error) {
	if where == nil {
		return -1, nil
	}
	c.inWhereConjunct = true
	wReg, werr := c.compileExpr(where)
	if werr != nil {
		return -1, werr
	}
	jump := c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
	c.inWhereConjunct = false
	return jump, nil
}

// compileSchemaCatalogDelete compiles "DELETE FROM sqlite_master [WHERE ...]"
// into: scan the catalog, and for each row whose values satisfy the WHERE,
// record its rowid; one OpSchemaWrite then dooms the whole set in the overlay.
// That is sqlite3DeleteFrom's own shape -- the WHERE coded into
// sqlite3WhereBegin (delete.c:526) and the rowid alone collected into a RowSet
// (delete.c:582), replayed after the scan.
//
// Note delete.c:474's "&& !IsVirtual(pTab)" has a sibling that matters here:
// the TRUNCATE optimisation additionally requires no triggers and no FKs, and
// this engine's catalog overlay has no truncate primitive at all, so a
// WHERE-less "DELETE FROM sqlite_master" is the same scan.
func (db *DB) compileSchemaCatalogDelete(stmt *deleteStmt, trig *trigCompileCtx) (*Program, error) {
	if trig != nil || stmt.returning != nil || len(stmt.ctes) > 0 || stmt.indexedBy != "" || stmt.alias != "" {
		return nil, fmt.Errorf("%w: direct sqlite_master DELETE: shape not lowered", errVDBEUnsupported)
	}
	if writableSchemaTargetIsTemp(stmt.schema, stmt.table) {
		// The scan below reads MAIN's catalog rows (wsCurrentCatalog), so a
		// temp-catalog row is not one of them. Only an INSERT is routed there.
		return nil, fmt.Errorf("%w: direct sqlite_temp_master write: only INSERT is lowered", errVDBEUnsupported)
	}
	plan := &schemaWritePlan{verb: "DELETE FROM", table: stmt.table, del: stmt}
	c, loopTop, rewind, rowidReg, writePgr, serr := db.beginSchemaWriteScan(plan, containsSubquery(stmt.where))
	if serr != nil {
		return nil, serr
	}
	whereJump, werr := emitSchemaWhere(c, stmt.where)
	if werr != nil {
		return nil, werr
	}
	c.emit(Instruction{Op: OpSchemaWriteRow, P2: rowidReg})
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}
	c.emit(Instruction{Op: OpNext, P1: 0, P2: loopTop})
	c.patch(rewind, c.here())
	c.emit(Instruction{Op: OpSchemaWrite, P4: plan})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: writePgr}, nil
}

// compileSchemaCatalogUpdate compiles "UPDATE sqlite_master SET c=expr[,...]
// [WHERE ...]" into: scan the catalog, and for each row the WHERE selects,
// evaluate every SET right-hand side against that row's OLD values and record
// them beside its rowid; one OpSchemaWrite then applies the whole set to the
// overlay. That is sqlite3Update's own shape -- sqlite3WhereBegin at
// update.c:742, each changed column coded INSIDE the scan at update.c:955.
//
// Every right-hand side sees the OLD row, because compileExpr reads the
// columns it names off the scan cursor, which is still positioned on the
// pre-update row. That is update.c's rule: :955 codes the expression while
// the cursor holds the old row, so "SET a=b, b=a" swaps rather than chains.
func (db *DB) compileSchemaCatalogUpdate(stmt *updateStmt, trig *trigCompileCtx) (*Program, error) {
	if trig != nil || stmt.returning != nil || stmt.from != nil || stmt.explicitOr ||
		len(stmt.ctes) > 0 || stmt.indexedBy != "" || stmt.alias != "" {
		return nil, fmt.Errorf("%w: direct sqlite_master UPDATE: shape not lowered", errVDBEUnsupported)
	}
	if writableSchemaTargetIsTemp(stmt.schema, stmt.table) {
		// The scan below reads MAIN's catalog rows (wsCurrentCatalog), so a
		// temp-catalog row is not one of them. Only an INSERT is routed there.
		return nil, fmt.Errorf("%w: direct sqlite_temp_master write: only INSERT is lowered", errVDBEUnsupported)
	}
	// The two shapes this format has no spelling for -- an assignment to
	// rootpage, and a match on the catalog's own rowid. Refused HERE so the
	// statement fails before it records anything; see segment_schema_write.go.
	plan := &schemaWritePlan{verb: "UPDATE", table: stmt.table, upd: stmt}
	c, loopTop, rewind, rowidReg, writePgr, serr := db.beginSchemaWriteScan(plan, containsSubquery(stmt.where) || wsSetsContainSubquery(stmt.sets))
	if serr != nil {
		return nil, serr
	}
	whereJump, werr := emitSchemaWhere(c, stmt.where)
	if werr != nil {
		return nil, werr
	}
	// One contiguous block per row, in STATEMENT order -- which is also the
	// order OpSchemaWrite's writeApplySetListPre (vtab_write.go) assigns them
	// in, so a repeated target still lands on its LAST assignment
	// (update.c:492's "aXRef[j] = i;" inside its own loop over pChanges).
	setBase := c.allocN(len(stmt.sets))
	for i, a := range stmt.sets {
		r, eerr := c.compileExpr(a.expr)
		if eerr != nil {
			return nil, eerr
		}
		if r != setBase+i {
			c.emit(Instruction{Op: OpSCopy, P1: r, P2: setBase + i})
		}
	}
	c.emit(Instruction{Op: OpSchemaWriteRow, P1: setBase, P2: rowidReg, P3: len(stmt.sets)})
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}
	c.emit(Instruction{Op: OpNext, P1: 0, P2: loopTop})
	c.patch(rewind, c.here())
	c.emit(Instruction{Op: OpSchemaWrite, P4: plan})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: writePgr}, nil
}

// compileSchemaCatalogInsert compiles "INSERT INTO sqlite_master [(cols)]
// VALUES (...)" into: every tuple's expressions coded into one contiguous
// register block per tuple, and one OpSchemaWrite handing those blocks to the
// overlay. That is sqlite3Insert's own coding loop
// ("int y = sqlite3ExprCodeTarget(pParse, pX, iRegStore);", insert.c:1430-1431)
// with the store replaced by the overlay's own, the way insert.c:1558-1564
// replaces it with a single OP_VUpdate for a virtual table.
//
// There is no cursor and no scan: an INSERT reads no existing catalog row.
func (db *DB) compileSchemaCatalogInsert(stmt *insertStmt, trig *trigCompileCtx) (*Program, error) {
	if trig != nil || stmt.selectStmt != nil || stmt.upsert != nil ||
		stmt.explicitOr || len(stmt.ctes) > 0 || len(stmt.rows) == 0 {
		return nil, fmt.Errorf("%w: direct sqlite_master INSERT: shape not lowered", errVDBEUnsupported)
	}
	// One uniform stride, like compileVtabInsertStmt's. A statement whose
	// tuples disagree can never satisfy the catalog's fixed five-column arity
	// for all of them, so every such statement is an ARITY ERROR whichever way
	// it is worded -- declining costs no statement that could have run.
	width := len(stmt.rows[0])
	for _, row := range stmt.rows {
		if len(row) != width {
			return nil, fmt.Errorf("%w: direct sqlite_master INSERT with ragged VALUES tuples", errVDBEUnsupported)
		}
	}
	plan := &schemaWritePlan{verb: "INSERT INTO", table: stmt.table, ins: stmt}
	c := &compiler{}
	var writePgr *ReadOnlyPager
	if wsRowsContainSubquery(stmt.rows) {
		sp, perr := db.writeSubqueryPager()
		if perr != nil {
			return nil, declineOrSemantic(perr)
		}
		c.pager = sp
		writePgr = sp
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpSchemaWritePre, P4: plan})
	// ALL tuples are coded BEFORE the single OpSchemaWrite, and that ordering
	// is what makes a two-row VALUES all-or-nothing: nothing reaches the
	// overlay until every tuple has evaluated.
	base := c.allocN(width * len(stmt.rows))
	for r, row := range stmt.rows {
		for i, e := range row {
			reg, eerr := c.compileExpr(e)
			if eerr != nil {
				return nil, eerr
			}
			dst := base + r*width + i
			if reg != dst {
				c.emit(Instruction{Op: OpSCopy, P1: reg, P2: dst})
			}
		}
	}
	c.emit(Instruction{Op: OpSchemaWrite, P1: base, P2: width, P3: len(stmt.rows), P4: plan})
	colNames, rerr := c.emitSchemaCatalogReturning(db, stmt, base, width)
	if rerr != nil {
		return nil, rerr
	}
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: writePgr, ColNames: colNames}, nil
}

// emitSchemaCatalogReturning emits one RETURNING row per inserted catalog row,
// out of the five-column image the overlay stores (wsInsertRowFromValues): a
// column the statement gave, affinity-coerced as that write coerces it, and
// NULL for every other -- which is the whole row for "DEFAULT VALUES"
// (returning1.test 21.0). Nothing is emitted without a RETURNING clause.
//
// The row is reachable by the target's own name and by every other spelling of
// the catalog, which is what resolve.c:528-532 accepts (the trigger table's
// name, or isValidSchemaTableName); a reference to its ROWID is not, because
// the overlay numbers the row only when the catalog is next written.
func (c *compiler) emitSchemaCatalogReturning(db *DB, stmt *insertStmt, base, width int) ([]string, error) {
	if stmt.returning == nil {
		return nil, nil
	}
	colIdx, rerr := wsResolveCatalogColumns(stmt.table, stmt.cols)
	if rerr != nil {
		return nil, declineOrSemantic(rerr)
	}
	catCols := sqliteSchemaCatalogColumns()
	tbl := &tableMeta{name: stmt.table, cols: catCols, ipkIndex: -1}
	ret := make([]SelectColumn, len(stmt.returning))
	for i, sc := range stmt.returning {
		if !sc.Star {
			e, ok := requalifyCatalogRefs(sc.Expr, stmt.table)
			if !ok {
				return nil, fmt.Errorf("%w: direct sqlite_master INSERT: RETURNING expression not lowered", errVDBEUnsupported)
			}
			sc.Expr = e
		}
		ret[i] = sc
	}
	var colNames []string
	for r := 0; r < len(stmt.rows); r++ {
		rowBase := c.allocN(len(catCols))
		for i := range catCols {
			c.emit(Instruction{Op: OpNull, P2: rowBase + i})
		}
		for j := 0; j < width; j++ {
			target := j
			if colIdx != nil {
				target = colIdx[j]
			}
			c.emit(Instruction{Op: OpSCopy, P1: base + r*width + j, P2: rowBase + target})
			c.emit(Instruction{Op: OpAffinity, P1: rowBase + target, P4: catCols[target].Aff})
		}
		names, err := c.emitReturning(db, tbl, ret, rowRegsFor(rowBase, len(catCols)), -1, returningSiteNoSubquery)
		if err != nil {
			return nil, err
		}
		colNames = names
	}
	return colNames, nil
}

// requalifyCatalogRefs rewrites every catalog-named QUALIFIER in e to name,
// so "sqlite_master.x" and "sqlite_schema.x" both resolve against a scope
// named however the statement spelled its target -- isValidSchemaTableName's
// own acceptance (resolve.c:531). It reports false for a node kind it does not
// walk, which is where a qualified reference could hide unrewritten.
func requalifyCatalogRefs(e Expr, name string) (Expr, bool) {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return e, true
	case ColumnExpr:
		if x.Schema == "" && x.Qualifier != "" && (isMainSchemaCatalogName(x.Qualifier) || isTempSchemaCatalogName(x.Qualifier)) {
			x.Qualifier = name
		}
		return x, true
	case UnaryExpr:
		sub, ok := requalifyCatalogRefs(x.X, name)
		x.X = sub
		return x, ok
	case BinaryExpr:
		l, lok := requalifyCatalogRefs(x.L, name)
		r, rok := requalifyCatalogRefs(x.R, name)
		x.L, x.R = l, r
		return x, lok && rok
	case IsNullExpr:
		sub, ok := requalifyCatalogRefs(x.X, name)
		x.X = sub
		return x, ok
	case CollateExpr:
		sub, ok := requalifyCatalogRefs(x.X, name)
		x.X = sub
		return x, ok
	case CastExpr:
		sub, ok := requalifyCatalogRefs(x.X, name)
		x.X = sub
		return x, ok
	case FuncExpr:
		if x.Over != nil || x.Filter != nil {
			return e, false
		}
		args := make([]Expr, len(x.Args))
		ok := true
		for i, a := range x.Args {
			sub, sok := requalifyCatalogRefs(a, name)
			args[i], ok = sub, ok && sok
		}
		x.Args = args
		return x, ok
	default:
		return e, false
	}
}

// ---- the opcode handlers ----

// schemaWriteDB is the write session all three opcodes run against, reported
// rather than dereferenced when it is somehow absent (AGENTS.md invariant 2).
// Unreachable: these opcodes are emitted only into WRITE programs.
func (m *vdbe) schemaWriteDB() (*DB, error) {
	if m.wctx == nil || m.wctx.db == nil {
		return nil, fmt.Errorf("vdbe: schema-catalog write with no write session")
	}
	return m.wctx.db, nil
}

// opSchemaWritePre runs OpSchemaWritePre: the catalog write's prepare-time
// checks, in the C's own order. See this file's doc comment for why they are
// asked here rather than at compile time.
func (m *vdbe) opSchemaWritePre(op *Instruction) error {
	plan := op.P4.(*schemaWritePlan)
	db, derr := m.schemaWriteDB()
	if derr != nil {
		return derr
	}
	if err := db.writableSchemaPreflight(plan.verb, plan.table, plan.preflightCols()); err != nil {
		// The flag-off half of this is C SQLite's sqlite3IsReadOnly, raised
		// while GENERATING code (delete.c:121) -- before the VM exists, so the
		// statement never reaches RUN state and sqlite3VdbeHalt's changeCntOn
		// block never runs. See prepareFailed below for the measurement.
		//
		// ONLY the read-only refusal is marked. Every other error this
		// function can return is the OVERLAY's own "unsupported" decline --
		// this engine's, not the C's, raised where C SQLite SUCCEEDS -- so
		// there is no oracle behaviour for it to match and it keeps whatever
		// changes() behaviour it already had.
		if isSchemaCatalogNotWritable(err) {
			m.notePrepareFailed()
		}
		return err
	}
	// The other two prepare-time checks, in the C's own order after
	// sqlite3IsReadOnly: the SET/IDLIST name resolution (update.c:500) and the
	// VALUES arity (insert.c:1249-1252). Both are marked too, because both are
	// decided by the PARSER in C and leave changes() alone for the same reason
	// the refusal does.
	switch {
	case plan.upd != nil:
		names := make([]string, len(plan.upd.sets))
		for i, a := range plan.upd.sets {
			names[i] = a.col
		}
		if _, rerr := wsResolveCatalogColumns(plan.table, names); rerr != nil {
			m.notePrepareFailed()
			return rerr
		}
	case plan.ins != nil:
		if _, rerr := wsResolveCatalogColumns(plan.table, plan.ins.cols); rerr != nil {
			m.notePrepareFailed()
			return rerr
		}
		if aerr := wsCheckInsertArity(plan.ins); aerr != nil {
			m.notePrepareFailed()
			return aerr
		}
	}
	return nil
}

// notePrepareFailed records that this statement failed a check C SQLite
// makes at PREPARE time, so runWrite's deferred setChanges leaves
// changes()/total_changes() exactly where the previous statement left them
// (vdbe_write.go's writeCtx.prepareFailed).
//
// MEASURED against mattn/go-sqlite3 3.53.3, all three checks, in
// compat-harness/writable_schema_prepare_changes_test.go: after
// "INSERT INTO t VALUES(1),(2),(3)" leaves changes()/total_changes() at 3/3,
// every one of "UPDATE/DELETE FROM/INSERT INTO sqlite_master ..." with the
// flag OFF, "INSERT INTO sqlite_master VALUES(1,2,3,4)" and
// "UPDATE sqlite_master SET nosuchcol='x'" with it ON still reads 3/3.
//
// No topLevel test: the three compilers decline a trigger body outright, so a
// program carrying this opcode is always the outermost statement.
func (m *vdbe) notePrepareFailed() {
	if m.wctx != nil {
		m.wctx.prepareFailed = true
	}
}

// opSchemaWriteRow runs OpSchemaWriteRow: record the row the scan cursor is on
// as one the WHERE selected, together with its already-evaluated SET values.
// It is delete.c:582's OP_RowSetAdd and update.c:1320-1327's ephemeral collect.
func (m *vdbe) opSchemaWriteRow(op *Instruction) {
	if m.schemaWrite == nil {
		// Unreachable: OpOpenDerived's schemaWrite branch runs first in every
		// program that contains this opcode and always installs the state.
		m.schemaWrite = &schemaWriteState{}
	}
	var vals []Value
	if op.P3 > 0 {
		// COPIED out of the registers rather than aliased, for opVWriteRow's
		// reason: these outlive the loop iteration that produced them, because
		// every later row overwrites the same register block.
		vals = make([]Value, op.P3)
		copy(vals, m.regs[op.P1:op.P1+op.P3])
	}
	m.schemaWrite.sel = append(m.schemaWrite.sel, schemaWriteSelected{rowid: m.regs[op.P2].I, vals: vals})
}

// opSchemaWrite runs OpSchemaWrite: apply the whole statement to the
// writable_schema overlay in one call. It is the replay loop's OP_Insert /
// OP_Delete, with this engine's one-call-per-statement shape -- the overlay is
// a per-session map (db.wsEdits, schema_write_direct.go), not a b-tree cursor,
// so there is nothing per-row for a per-row opcode to hold.
func (m *vdbe) opSchemaWrite(op *Instruction) error {
	plan := op.P4.(*schemaWritePlan)
	db, derr := m.schemaWriteDB()
	if derr != nil {
		return derr
	}
	st := m.schemaWrite
	if st == nil {
		// An INSERT has no scan and so never installs one -- it also never
		// reads st. On the UPDATE/DELETE arms nil is unreachable, because
		// OpOpenDerived's schemaWrite branch runs unconditionally ahead of this
		// opcode; a nil dereference there is not a failure mode worth having
		// (AGENTS.md invariant 2).
		st = &schemaWriteState{}
	}
	// One statement, one selection -- the ephemeral table update.c:1357 drops.
	m.schemaWrite = nil
	var n int
	switch {
	case plan.del != nil:
		doomed := make([]wsCatalogKey, 0, len(st.sel))
		for _, s := range st.sel {
			key, kerr := st.keyOf(s.rowid)
			if kerr != nil {
				return kerr
			}
			doomed = append(doomed, key)
		}
		n = db.wsApplyCatalogDeletes(doomed)
	case plan.upd != nil:
		setIdx, rerr := wsUpdateSetIndexes(plan.upd)
		if rerr != nil {
			return rerr
		}
		pending := make([]wsPendingUpdate, 0, len(st.sel))
		for _, s := range st.sel {
			key, kerr := st.keyOf(s.rowid)
			if kerr != nil {
				return kerr
			}
			vals, verr := wsSetValuesFromRegisters(s.vals, setIdx)
			if verr != nil {
				return verr
			}
			pending = append(pending, wsPendingUpdate{key: key, vals: vals})
		}
		n = db.wsApplyCatalogUpdates(pending, setIdx)
	default:
		built := make([][]Value, 0, op.P3)
		colIdx, rerr := wsResolveCatalogColumns(plan.table, plan.ins.cols)
		if rerr != nil {
			return rerr
		}
		for t := 0; t < op.P3; t++ {
			raw := m.regs[op.P1+t*op.P2 : op.P1+(t+1)*op.P2]
			built = append(built, wsInsertRowFromValues(raw, plan.ins.cols, colIdx))
		}
		temp := writableSchemaTargetIsTemp(plan.ins.schema, plan.table)
		if temp {
			// Open the TEMP database NOW, as any other temp write does: its
			// file has to exist while the STATEMENT runs, because the driver
			// reads it off the session (DB.TempPath) before the commit that
			// would otherwise create it.
			if _, terr := db.tempDatabase(); terr != nil {
				return terr
			}
		}
		n = db.wsApplyCatalogInserts(built, temp)
	}
	// By ASSIGNMENT, not accumulation, for opVWrite's reason: one OpSchemaWrite
	// is one whole statement however many rows it turned out to touch. Unlike
	// opVWrite there is no "even when it errored" case to state -- the three
	// apply halves cannot fail (they are map writes), and every early return
	// above is an unreachable internal error that leaves the count alone.
	m.wctx.rowsAffected = n
	return nil
}

// keyOf turns a selected 1-based catalog rowid back into the wsCatalogKey the
// overlay is keyed by. The numbering is the one OpOpenDerived's schemaWrite
// branch assigned -- the position in db.wsCurrentCatalog()'s own list
// (schema_write_direct.go) -- so this is an index, not a search.
func (st *schemaWriteState) keyOf(rowid int64) (wsCatalogKey, error) {
	for _, r := range st.rows {
		if r.rowid == rowid {
			return r.key, nil
		}
	}
	// Unreachable: every rowid here came from OpRowid over the very cursor this
	// list opened. Reported rather than guessed at (AGENTS.md invariant 2).
	return wsCatalogKey{}, fmt.Errorf("engine: internal error: schema-catalog rowid %d is not one of the %d scanned rows", rowid, len(st.rows))
}
