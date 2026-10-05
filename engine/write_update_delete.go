// This file implements UPDATE and DELETE. Both mutate only the target table's
// row store (tableMeta.rows); the commit appends what changed
// (segment_write.go).
//
// WHERE and SET expressions compile like a SELECT's, subqueries included. The
// subquery opcodes run against a snapshot of this session's content taken once
// per statement before any row changes (writeSubqueryPager), which is what C
// gives a WHERE (the match set is resolved against the pre-update image) and
// an uncorrelated SET subquery. A correlated SET subquery that scans the
// target table sees partially updated rows in C, which this cannot mirror, so
// it is declined (setSubqueryReadsTable, setSubqueryCorrelatesToRow).
// UPDATE ... FROM is update_from.go; RETURNING is returning_write.go.
package engine

import (
	"fmt"
)

// ---- DELETE ----

// deleteStmt is a parsed "DELETE FROM table [AS alias] [indexed-hint]
// [WHERE expr] [RETURNING ...]".
type deleteStmt struct {
	table     string
	schema    string // database qualifier ("main"/"temp"/attached name), "" if unqualified
	alias     string // "AS alias" on the target, "" if none -- see parseWriteTargetSuffix
	indexedBy string // "INDEXED BY <name>" hint, "" if none (a "NOT INDEXED" hint leaves it "")
	// notIndexed is the "NOT INDEXED" hint, which the planner has to see: it
	// keeps the scan off every index (DB.updateOnePassOrder).
	notIndexed bool
	where      Expr
	returning  []SelectColumn // RETURNING result columns, nil if no RETURNING clause
	params     ParamInfo
	// ctes are the definitions of a leading "WITH [RECURSIVE] <cte-list>"
	// clause, which C SQLite allows in front of DELETE exactly as in front
	// of SELECT. They are pushed onto the subquery pager's own CTE scope stack
	// for the duration of the COMPILE (pushCTEScope, cte.go) so a WHERE
	// subquery resolves them -- see compileDeleteStmt, vdbe_write.go.
	ctes []CTEDef
}

// parseWriteTargetSuffix parses the two optional clauses SQLite's
// qualified_table_name allows after an UPDATE/DELETE target: "AS <alias>",
// then "INDEXED BY <name>" or "NOT INDEXED" (only in that order).
//
// The alias replaces the table's name, as in a FROM clause: "UPDATE t1 AS q
// SET b=b+1 WHERE t1.a=1" is "no such column: t1.a". A bare (AS-less) alias is
// a syntax error here, unlike in FROM.
//
// The index hint's name is validated at exec time ("no such index:
// nosuchidx"), as for SELECT. Neither hint is legal inside a trigger body:
// "the INDEXED BY clause is not allowed on UPDATE or DELETE statements within
// triggers" (and the NOT INDEXED counterpart) -- trigger1.test. An alias is
// allowed there.
func parseWriteTargetSuffix(p *parser, kind string) (alias, indexedBy string, notIndexed bool, err error) {
	if p.consumeKeyword("AS") {
		a := p.next()
		name, ok := aliasTokenText(a)
		if !ok {
			return "", "", false, fmt.Errorf("engine: %s: expected alias after AS, got %q", kind, p.tokenDesc(a))
		}
		alias = name
	}
	switch {
	case p.consumeKeyword("INDEXED"):
		if !p.consumeKeyword("BY") {
			return "", "", false, fmt.Errorf("engine: %s: expected BY after INDEXED, got %q", kind, p.tokenDesc(p.peek()))
		}
		idx := p.next()
		if idx.kind != tkIdent {
			return "", "", false, fmt.Errorf("engine: %s: expected index name after INDEXED BY, got %q", kind, p.tokenDesc(idx))
		}
		if p.inTriggerBody {
			return "", "", false, fmt.Errorf("engine: the INDEXED BY clause is not allowed on UPDATE or DELETE statements within triggers")
		}
		indexedBy = idx.text
	case p.peekIsKeyword("NOT"):
		// "NOT INDEXED", but "NOT" also legitimately starts nothing else
		// here, so only consume the pair.
		save := p.pos
		p.next()
		if !p.consumeKeyword("INDEXED") {
			p.pos = save
			break
		}
		if p.inTriggerBody {
			return "", "", false, fmt.Errorf("engine: the NOT INDEXED clause is not allowed on UPDATE or DELETE statements within triggers")
		}
		notIndexed = true
	}
	return alias, indexedBy, notIndexed, nil
}

// checkWriteIndexHint validates an UPDATE/DELETE "INDEXED BY <name>" as C does:
// it must be an index of the resolved target table, else "no such index:
// <name>" -- sqlite3IndexedByLookup (select.c:5480), from sqlite3SrcListLookup
// (delete.c:41), shared by UPDATE (update.c:362) and DELETE (delete.c:345),
// and run before delete.c:460's truncate special case.
//
// select.c:5487-5490 walks the chosen table's pTab->pIndex, not a cross-catalog
// name lookup, so "DELETE FROM main.t INDEXED BY px" must not see a temp px.
// It checks this session's live index catalog, which includes indexes created
// earlier in the session.
//
// The planner consequence is separate: INDEXED BY narrows candidates to that
// index (where.c:4031-4034), so a partial index the WHERE does not imply
// leaves no loop and "no query solution" (where.c:6169) --
// writeIndexedByNoSolution, which each caller runs next.
func (db *DB) checkWriteIndexHint(name string, tbl *tableMeta) error {
	if name == "" || tbl == nil {
		return nil
	}
	if db.findTableIndexMeta(tbl, name) == nil {
		return fmt.Errorf("engine: no such index: %s", name)
	}
	return nil
}

// findTableIndexMeta is select.c:5487-5490's own loop: the index named name
// among THIS table's indexes -- see indexBelongsTo (index_write.go) for the
// same-catalog-and-same-table pair that defines pTab->pIndex membership.
func (db *DB) findTableIndexMeta(tbl *tableMeta, name string) *indexMeta {
	for _, ix := range db.indexes {
		if indexBelongsTo(ix, tbl) && equalFoldName(ix.name, name) {
			return ix
		}
	}
	return nil
}

// writeScopeName is the name an UPDATE's/DELETE's own WHERE/SET expressions
// resolve a qualified column reference against: the statement's "AS alias"
// when it has one, else the table's own name. See parseWriteTargetSuffix.
func writeScopeName(alias string, tbl *tableMeta) string {
	if alias != "" {
		return alias
	}
	return tbl.name
}

func parseDeleteStmt(sqlText string) (*deleteStmt, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	stmt, err := parseDeleteStmtTokens(p)
	if err != nil {
		return nil, err
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: DELETE: unexpected trailing input near %q", p.tokenDesc(p.peek()))
	}
	stmt.params = p.params.info()
	return stmt, nil
}

// parseDeleteStmtTokens parses a DELETE statement's grammar off of p, stopping
// right after the statement's own WHERE clause (if any) -- WITHOUT requiring
// a trailing ";"/EOF and WITHOUT snapshotting p.params.info(). See
// parseInsertStmtTokens's identical doc comment (insert_write.go): this is
// the ONE grammar both a top-level DELETE (parseDeleteStmt) and a trigger
// body's DELETE statement (parseTriggerBody, trigger.go) share.
func parseDeleteStmtTokens(p *parser) (*deleteStmt, error) {
	// A leading "WITH [RECURSIVE] <cte-list>" (parseWithClause), allowed in
	// front of DELETE as in front of SELECT/INSERT/UPDATE. The CTEs are
	// pushed onto the subquery pager's scope for compileDeleteStmt, so
	// "WITH dset AS (SELECT 2 UNION ALL SELECT 4) DELETE FROM t1 WHERE x IN
	// dset" (with1.test) works. An unused one is a no-op, as in C.
	var leadingCTEs []CTEDef
	if p.peekIsKeyword("WITH") {
		var err error
		leadingCTEs, err = p.parseWithClause()
		if err != nil {
			return nil, err
		}
	}
	if !p.consumeKeyword("DELETE") {
		return nil, fmt.Errorf("engine: DELETE: expected DELETE, got %q", p.tokenDesc(p.peek()))
	}
	if !p.consumeKeyword("FROM") {
		// parse.y's cmd ::= with DELETE FROM xfullname ... has no alternative without
		// FROM, so C reports the offending token itself rather than what it wanted.
		return nil, fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
	}
	// objectNameToken, not a bare tkIdent check: SQLite's "nm" production also
	// accepts a single-quoted STRING as a name ("DELETE FROM 'table1'"), see
	// its doc comment (sql_parser.go).
	t := p.peek()
	name, ok := objectNameToken(t)
	if !ok {
		return nil, fmt.Errorf("engine: DELETE: expected table name, got %q", p.tokenDesc(t))
	}
	p.next()
	stmt := &deleteStmt{table: name, ctes: leadingCTEs}
	// Optional database qualifier: "DELETE FROM schema.table ...".
	if p.peekIsPunct(".") {
		p.next() // the "."
		t2 := p.peek()
		n2, ok := objectNameToken(t2)
		if !ok {
			return nil, fmt.Errorf("engine: DELETE: expected table name after schema qualifier, got %q", p.tokenDesc(t2))
		}
		p.next()
		stmt.schema = name
		stmt.table = n2
	}
	alias, indexedBy, notIndexed, err := parseWriteTargetSuffix(p, "DELETE")
	if err != nil {
		return nil, err
	}
	stmt.alias, stmt.indexedBy, stmt.notIndexed = alias, indexedBy, notIndexed

	if p.peekIsPunct(",") || p.peekIsKeyword("USING") {
		return nil, fmt.Errorf("engine: DELETE: multi-table DELETE is not supported by this write path")
	}
	if p.consumeKeyword("WHERE") {
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.where = expr
	}
	ret, err := parseReturningClause(p)
	if err != nil {
		return nil, err
	}
	stmt.returning = ret
	return stmt, nil
}

// parseReturningClause parses an optional trailing "RETURNING <result-column>
// [, ...]" clause off of p (an INSERT/UPDATE/DELETE's last clause), returning
// nil when no RETURNING keyword is present. The result-column grammar is
// exactly a SELECT's (parseSelectList) -- "*", "table.*", or an expression
// with an optional alias -- so RETURNING output-column naming reuses the SAME
// expandSelectList machinery a SELECT uses (see execReturningRows). C SQLite
// forbids a bare "RETURNING *" from resolving table.* against anything but the
// single table being written, which is exactly the single-table scope this
// path evaluates the clause against.
func parseReturningClause(p *parser) ([]SelectColumn, error) {
	if !p.consumeKeyword("RETURNING") {
		return nil, nil
	}
	cols, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	return cols, nil
}

// Delete parses and executes a single DELETE FROM statement: every row of
// the named table for which WHERE evaluates truthy (or, with no WHERE,
// every row) is removed from the table's logical row store. It returns the
// number of rows deleted.
func (db *DB) Delete(sqlText string) (int, error) {
	return db.DeleteArgs(sqlText, nil)
}

// DeleteArgs is Delete extended with bound-parameter support: args[i] binds
// SQL parameter index i+1 (see ParamExpr, sql_ast.go and QueryArgs' doc
// comment in query.go, which this mirrors for the write path) wherever a
// placeholder appears in WHERE. A placeholder beyond len(args) evaluates to
// NULL, matching C SQLite's own unbound-parameter behavior.
func (db *DB) DeleteArgs(sqlText string, args []Value) (int, error) {
	// Delegates to ExecArgs, which compiles the statement and runs the program
	// (compileDeleteWrite, vdbe_write.go). These exported helpers were the last
	// thing keeping the old AST write drivers reachable -- nothing outside
	// package engine uses them, not driver, not replication, not compat-harness --
	// so routing them here is what let that whole ~90-function cluster be
	// deleted.
	//
	// The parse is KEPT rather than folded into ExecArgs: it is what makes
	// "DeleteArgs" reject a statement of the wrong VERB, which delegating alone
	// would silently accept.
	if _, err := parseDeleteStmt(sqlText); err != nil {
		return 0, err
	}
	n, _, err := db.ExecArgs(sqlText, args)
	return int(n), err
}

// ---- UPDATE ----

// assignment is one "col = expr" item of an UPDATE's SET clause.
type assignment struct {
	col  string
	expr Expr
}

// updateStmt is a parsed "UPDATE [OR <action>] table [AS alias]
// [indexed-hint] SET col=expr[,...] [WHERE expr]".
type updateStmt struct {
	table     string
	schema    string // database qualifier ("main"/"temp"/attached name), "" if unqualified
	alias     string // "AS alias" on the target, "" if none -- see parseWriteTargetSuffix
	indexedBy string // "INDEXED BY <name>" hint, "" if none (a "NOT INDEXED" hint leaves it "")
	// notIndexed: see deleteStmt.notIndexed.
	notIndexed bool
	sets       []assignment
	where      Expr
	from       []FromItem     // FROM-clause join sources for "UPDATE ... FROM ...", nil for a plain single-table UPDATE
	returning  []SelectColumn // RETURNING result columns, nil if no RETURNING clause
	params     ParamInfo

	// orAction is the resolution algorithm named by an "UPDATE OR ..."
	// clause, or the default conflictAbort when none was given -- see
	// conflict.go.
	orAction conflictAction
	// explicitOr is true when this statement itself gave an explicit
	// "UPDATE OR <action>" clause -- see insertStmt.explicitOr's identical
	// doc comment (insert_write.go) for why this is needed alongside
	// orAction: a table constraint's own declared "ON CONFLICT <action>"
	// default only governs a violation of THAT constraint when the
	// statement itself named none.
	explicitOr bool

	// ctes are a leading "WITH [RECURSIVE] <cte-list>" clause's definitions --
	// see deleteStmt.ctes' doc comment.
	ctes []CTEDef
}

func parseUpdateStmt(sqlText string) (*updateStmt, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	stmt, err := parseUpdateStmtTokens(p)
	if err != nil {
		return nil, err
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: UPDATE: unexpected trailing input near %q", p.tokenDesc(p.peek()))
	}
	stmt.params = p.params.info()
	return stmt, nil
}

// parseUpdateStmtTokens parses an UPDATE statement's grammar off of p,
// stopping right after the statement's own WHERE clause (if any) -- WITHOUT
// requiring a trailing ";"/EOF and WITHOUT snapshotting p.params.info(). See
// parseInsertStmtTokens's identical doc comment (insert_write.go): this is
// the ONE grammar both a top-level UPDATE (parseUpdateStmt) and a trigger
// body's UPDATE statement (parseTriggerBody, trigger.go) share.
func parseUpdateStmtTokens(p *parser) (*updateStmt, error) {
	// A leading WITH clause: see parseDeleteStmtTokens' identical doc comment
	// just above -- the same reasoning applies verbatim to UPDATE's SET/WHERE,
	// which reach their subqueries through the same writeSubqueryPager.
	var leadingCTEs []CTEDef
	if p.peekIsKeyword("WITH") {
		var err error
		leadingCTEs, err = p.parseWithClause()
		if err != nil {
			return nil, err
		}
	}
	if !p.consumeKeyword("UPDATE") {
		return nil, fmt.Errorf("engine: UPDATE: expected UPDATE, got %q", p.tokenDesc(p.peek()))
	}
	stmt := &updateStmt{ctes: leadingCTEs}
	if p.consumeKeyword("OR") {
		act, err := parseConflictAction(p)
		if err != nil {
			return nil, err
		}
		stmt.orAction = act
		stmt.explicitOr = true
	}
	// See parseDeleteStmtTokens' identical objectNameToken note.
	t := p.peek()
	name, ok := objectNameToken(t)
	if !ok {
		return nil, fmt.Errorf("engine: UPDATE: expected table name, got %q", p.tokenDesc(t))
	}
	p.next()
	stmt.table = name
	// Optional database qualifier: "UPDATE schema.table SET ...".
	if p.peekIsPunct(".") {
		p.next() // the "."
		t2 := p.peek()
		n2, ok := objectNameToken(t2)
		if !ok {
			return nil, fmt.Errorf("engine: UPDATE: expected table name after schema qualifier, got %q", p.tokenDesc(t2))
		}
		p.next()
		stmt.schema = name
		stmt.table = n2
	}
	alias, indexedBy, notIndexed, err := parseWriteTargetSuffix(p, "UPDATE")
	if err != nil {
		return nil, err
	}
	stmt.alias, stmt.indexedBy, stmt.notIndexed = alias, indexedBy, notIndexed

	if !p.consumeKeyword("SET") {
		return nil, fmt.Errorf("engine: UPDATE: expected SET, got %q", p.tokenDesc(p.peek()))
	}
	for {
		// "SET (col, col, ...) = <row-value>" -- SQLite's column-list
		// assignment. parseSetColumnList expands it into the ordinary
		// one-column-per-assignment form this write path already runs; see
		// its doc comment for the exact equivalence.
		if p.peekIsPunct("(") {
			sets, err := parseSetColumnList(p, "UPDATE")
			if err != nil {
				return nil, err
			}
			stmt.sets = append(stmt.sets, sets...)
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
		ct := p.peek()
		colName, ok := objectNameToken(ct)
		if !ok {
			return nil, fmt.Errorf("engine: UPDATE: expected column name, got %q", p.tokenDesc(ct))
		}
		p.next()
		if err := p.expectPunct("="); err != nil {
			return nil, err
		}
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.sets = append(stmt.sets, assignment{col: colName, expr: expr})
		if p.peekIsPunct(",") {
			p.next()
			continue
		}
		break
	}

	if p.consumeKeyword("FROM") {
		// UPDATE ... SET ... FROM <join-source> [WHERE ...]: the join-based
		// (multi-table) UPDATE form. The FROM clause reuses the SELECT
		// FROM-clause grammar verbatim (parseFromClause) -- a comma/JOIN list of
		// tables, derived tables, and their ON/USING conditions -- so every
		// join shape a SELECT accepts is accepted here too. Execution
		// (update_from.go) declines the genuinely-nondeterministic multi-match
		// shape; see that file's doc comment.
		items, err := p.parseFromClause(false)
		if err != nil {
			return nil, err
		}
		stmt.from = items
	}
	if p.consumeKeyword("WHERE") {
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.where = expr
	}
	ret, err := parseReturningClause(p)
	if err != nil {
		return nil, err
	}
	stmt.returning = ret
	return stmt, nil
}

// parseSetColumnList parses one "(col, col, ...) = <row-value>" SET element,
// positioned on the "(", and returns the equivalent plain assignments:
//
//	SET (c,d) = (a,b)                -> c=a, d=b
//	SET (c) = 99                     -> c=99      (a 1-column list takes a plain scalar)
//	SET (c,d) = (SELECT y,z FROM t2 WHERE w=a)
//	                                 -> c=(SELECT y FROM t2 WHERE w=a),
//	                                    d=(SELECT z FROM t2 WHERE w=a)
//
// An arity mismatch is C's "N columns assigned M values"; an unknown column
// surfaces later as "no such column". A repeated column is accepted, as in C.
// Every SET value is computed from the row's pre-update image, which is why
// splitting a tuple is exact: "SET (a,b) = (b,a)" swaps.
func parseSetColumnList(p *parser, kind string) ([]assignment, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var cols []string
	for {
		ct := p.peek()
		colName, ok := objectNameToken(ct)
		if !ok {
			return nil, fmt.Errorf("engine: %s: expected column name, got %q", kind, p.tokenDesc(ct))
		}
		p.next()
		cols = append(cols, colName)
		if p.peekIsPunct(",") {
			p.next()
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	if err := p.expectPunct("="); err != nil {
		return nil, err
	}
	rhs, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	switch r := rhs.(type) {
	case RowExpr:
		if len(cols) != len(r.Elems) {
			return nil, fmt.Errorf("engine: %s: %d columns assigned %d values", kind, len(cols), len(r.Elems))
		}
		out := make([]assignment, len(cols))
		for i, c := range cols {
			out[i] = assignment{col: c, expr: r.Elems[i]}
		}
		return out, nil
	case SubqueryExpr:
		return expandRowSubqueryAssign(cols, r, kind)
	default:
		// A single parenthesized expression is not a row value at all
		// (parsePrimary unwraps it), so this is the "(c) = <scalar>" form --
		// legal only for a one-column list.
		if len(cols) != 1 {
			return nil, fmt.Errorf("engine: %s: %d columns assigned 1 values", kind, len(cols))
		}
		return []assignment{{col: cols[0], expr: rhs}}, nil
	}
}

// expandRowSubqueryAssign expands "(c1, ..., cN) = (SELECT e1, ..., eN ...)"
// into N assignments, each the same subquery narrowed to one expression. They
// share FROM/WHERE/GROUP BY/LIMIT, so they scan in the same order and stop on
// the same row, as C's single row-value evaluation does -- including no row,
// where each yields NULL.
//
// Declined, because narrowing would change which row comes first:
//
//   - a compound (UNION dedups on the full tuple);
//   - SELECT DISTINCT (same reason);
//   - ORDER BY (an ordinal term could fall out of range).
//
// A non-deterministic call anywhere in the body is declined (the N scans could
// disagree), as is "*" (its width needs the schema).
func expandRowSubqueryAssign(cols []string, sub SubqueryExpr, kind string) ([]assignment, error) {
	s := sub.Stmt
	switch {
	case len(s.Compound) != 0:
		return nil, fmt.Errorf("engine: %s: a compound SELECT as a column-list assignment's value is not supported by this write path", kind)
	case s.Distinct:
		return nil, fmt.Errorf("engine: %s: SELECT DISTINCT as a column-list assignment's value is not supported by this write path", kind)
	case len(s.OrderBy) != 0:
		return nil, fmt.Errorf("engine: %s: ORDER BY in a column-list assignment's value is not supported by this write path", kind)
	case selectCallsNondeterministicFunc(s):
		return nil, fmt.Errorf("engine: %s: a non-deterministic column-list assignment value is not supported by this write path", kind)
	}
	for _, sc := range s.Columns {
		if sc.Star {
			return nil, fmt.Errorf("engine: %s: \"*\" in a column-list assignment's value is not supported by this write path", kind)
		}
	}
	if len(cols) != len(s.Columns) {
		// Against a SELECT the count is NOT checked while parsing:
		// sqlite3ExprListAppendVector skips its "%d columns assigned %d
		// values" when the right side is TK_SELECT (expr.c:2113), and the
		// mismatch surfaces when the statement is coded. So CREATE TRIGGER
		// accepts such a body (altertab.test 33.0) and the FIRING statement
		// is what fails. A poisoned literal defers the error to exactly that
		// point (LiteralExpr.deferredErr).
		bad := LiteralExpr{deferredErr: fmt.Sprintf("%d columns assigned %d values", len(cols), len(s.Columns))}
		out := make([]assignment, len(cols))
		for i, c := range cols {
			out[i] = assignment{col: c, expr: bad}
		}
		return out, nil
	}
	out := make([]assignment, len(cols))
	for i, c := range cols {
		narrowed := *s
		narrowed.Columns = []SelectColumn{s.Columns[i]}
		out[i] = assignment{col: c, expr: SubqueryExpr{Stmt: &narrowed}}
	}
	return out, nil
}

// pendingUpdate is one matched row's outcome, computed against a snapshot of
// every matching row's OLD values before any row in the batch is actually
// mutated -- see Update's doc comment for why.
type pendingUpdate struct {
	oldRowid uint64
	newRowid uint64
	newVals  []Value
}

// rowidFromValue converts an UPDATE's IPK/rowid SET value, or an INSERT's
// explicit rowid column value (decideRowid, for a table with no IPK column),
// into a rowid: an integer as is, a real with no fractional part accepted.
// Anything else, including NULL in UPDATE, is C's bare "datatype mismatch".
// (decideRowid never passes NULL; there NULL means auto-assign.)
func rowidFromValue(v Value) (uint64, error) {
	switch v.Typ {
	case Int:
		return uint64(v.I), nil
	case Float:
		if v.F == float64(int64(v.F)) {
			return uint64(int64(v.F)), nil
		}
	}
	return 0, fmt.Errorf("datatype mismatch")
}

// Update parses and executes a single UPDATE and returns the number of rows
// updated. Every SET value is computed from the row's pre-update values, as in
// C ("SET a=b, b=a" swaps, "SET n=n+1" reads the current n); the row is
// re-stored under its rowid, or a new one if the INTEGER PRIMARY KEY was
// assigned. Rows are applied and constraint-checked one at a time in
// ascending old-rowid order, matching C's order-dependent behaviour on a
// UNIQUE column.
func (db *DB) Update(sqlText string) (int, error) {
	return db.UpdateArgs(sqlText, nil)
}

// UpdateArgs is Update extended with bound-parameter support: args[i] binds
// SQL parameter index i+1 (see ParamExpr, sql_ast.go) wherever a placeholder
// appears in SET or WHERE. A placeholder beyond len(args) evaluates to NULL,
// matching C SQLite's own unbound-parameter behavior.
func (db *DB) UpdateArgs(sqlText string, args []Value) (int, error) {
	// Delegates to ExecArgs, which compiles the statement and runs the program
	// (compileUpdateWrite, vdbe_write.go). See DeleteArgs' identical comment
	// for why these exported helpers route here rather than executing anything
	// of their own.
	//
	// The parse is KEPT rather than folded into ExecArgs: it is what makes
	// "UpdateArgs" reject a statement of the wrong VERB, which delegating alone
	// would silently accept.
	if _, err := parseUpdateStmt(sqlText); err != nil {
		return 0, err
	}
	n, _, err := db.ExecArgs(sqlText, args)
	return int(n), err
}

// ---- shared WHERE/SET evaluation support ----

// rowEvalCtx builds the single-table evalCtx for one row of tbl: one
// tableScope named after the table, with vals normalized as the read side
// does (normalizeRow), so a column sees what a SELECT would. pager, when
// non-nil, is a snapshot for expressions that read the database; outer chains
// an enclosing scope for outward resolution.
func rowEvalCtx(tbl *tableMeta, rowid uint64, vals []Value, params []Value, outer *evalCtx, pager *ReadOnlyPager) *evalCtx {
	return rowEvalCtxAs(tbl, "", rowid, vals, params, outer, pager)
}

// rowEvalCtxAs is rowEvalCtx with an explicit scope NAME: an UPDATE's/DELETE's
// own "AS alias" replaces the table's name for column resolution (see
// parseWriteTargetSuffix), so a caller evaluating in the scope of such a
// statement passes the statement's alias; rowEvalCtx, the only caller left,
// passes "" for the table's own name. The value row is
// still normalized against the REAL table name -- normalizeRow uses it only to
// compute generated columns, which are defined on the table, not the alias.
func rowEvalCtxAs(tbl *tableMeta, alias string, rowid uint64, vals []Value, params []Value, outer *evalCtx, pager *ReadOnlyPager) *evalCtx {
	scope := tableScope{name: writeScopeName(alias, tbl), cols: tbl.cols, colIndex: buildColIndex(tbl.cols), offset: 0, noRowid: tbl.withoutRowid}
	return &evalCtx{
		tables: []tableScope{scope},
		pager:  pager,
		vals:   normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rowid, vals),
		// rowids exposes this table's own rowid/oid/_rowid_ pseudo-column
		// (see resolveColumn, column_scope.go) so "DELETE FROM t WHERE
		// rowid=?"/"UPDATE t SET a=1 WHERE _rowid_=?" work exactly like the
		// read-side query engine already does -- this write path evaluates
		// WHERE/SET against exactly one table's own row, so a single-entry
		// slice (matching the single-entry tables above) always suffices.
		rowids: []Value{{Typ: Int, I: int64(rowid)}},
		params: params,
		outer:  outer,
	}
}

// setSubqueryReadsTable reports whether any subquery in e (a SET right-hand
// side) reads table, at any nesting depth (derived table, compound arm,
// nested subquery, CTE body).
//
// C evaluates a correlated SET subquery per row against the live, partially
// updated table: "UPDATE t SET x = x + (SELECT count(*) FROM t t2 WHERE
// t2.x<=t.x)" over 1,2,3 gives 2,4,5, while this path's pre-update snapshot
// would give 2,4,6. An uncorrelated one sees the pre-update image ("SET
// x=(SELECT sum(x) FROM t)" gives 6,6,6), which the snapshot reproduces;
// setSubqueryCorrelatesToRow separates the two. Subqueries reading only other
// tables are always safe, correlated or not. WHERE subqueries need no guard:
// C resolves the whole match set first.
func setSubqueryReadsTable(e Expr, table string) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return false
	case SubqueryExpr:
		return selectReferencesTableFrom(x.Stmt, table)
	case ExistsExpr:
		return selectReferencesTableFrom(x.Stmt, table)
	case RowExpr:
		// A row value's elements are ordinary expressions with no special
		// resolution barrier -- SQLite's resolver has no TK_VECTOR-specific
		// case in resolve.c, so "(a,b)" walks exactly like any other
		// sub-expression list. checkExprSupported's own InExpr case
		// (expr_supported.go) already treats a RowExpr's Elems this way when
		// deciding support; without this case here, a RowExpr fell to the
		// default arm below (return false), which is the WRONG direction for
		// this function specifically when one of its elements is itself a
		// subquery reading table -- e.g. "(a, (SELECT max(x) FROM t)) IN
		// (SELECT p,q FROM o)" would have silently reported "doesn't read
		// table" and skipped the decline this whole function exists to gate.
		for _, el := range x.Elems {
			if setSubqueryReadsTable(el, table) {
				return true
			}
		}
		return false
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if setSubqueryReadsTable(a, table) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return setSubqueryReadsTable(x.X, table)
	case BinaryExpr:
		return setSubqueryReadsTable(x.L, table) || setSubqueryReadsTable(x.R, table)
	case IsNullExpr:
		return setSubqueryReadsTable(x.X, table)
	case InExpr:
		if x.Sub != nil && selectReferencesTableFrom(x.Sub, table) {
			return true
		}
		if setSubqueryReadsTable(x.X, table) {
			return true
		}
		for _, a := range x.List {
			if setSubqueryReadsTable(a, table) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return setSubqueryReadsTable(x.X, table) || setSubqueryReadsTable(x.Lo, table) || setSubqueryReadsTable(x.Hi, table)
	case LikeExpr:
		return setSubqueryReadsTable(x.X, table) || setSubqueryReadsTable(x.Pattern, table) || setSubqueryReadsTable(x.Escape, table)
	case GlobExpr:
		return setSubqueryReadsTable(x.X, table) || setSubqueryReadsTable(x.Pattern, table)
	case CollateExpr:
		return setSubqueryReadsTable(x.X, table)
	case CastExpr:
		return setSubqueryReadsTable(x.X, table)
	case CaseExpr:
		if x.Base != nil && setSubqueryReadsTable(x.Base, table) {
			return true
		}
		for _, w := range x.Whens {
			if setSubqueryReadsTable(w.When, table) || setSubqueryReadsTable(w.Then, table) {
				return true
			}
		}
		return x.Else != nil && setSubqueryReadsTable(x.Else, table)
	default:
		return false
	}
}

// selectReferencesTableFrom reports whether sel, recursively through every
// nested subquery expression, derived table, compound arm, CTE body, and join
// constraint, has any FROM item that names table (case-insensitively). It
// intentionally over-approximates -- a CTE or derived table that merely
// shadows the name still counts -- because it only ever gates a decline (see
// setSubqueryReadsTable), where a false positive costs a cleanly-declined
// statement, never a wrong answer.
func selectReferencesTableFrom(sel *SelectStmt, table string) bool {
	if sel == nil {
		return false
	}
	for _, f := range sel.From {
		if f.Table != "" && equalFoldName(f.Table, table) {
			return true
		}
		if selectReferencesTableFrom(f.Subquery, table) {
			return true
		}
		if setSubqueryReadsTable(f.On, table) {
			return true
		}
	}
	for _, c := range sel.Columns {
		if setSubqueryReadsTable(c.Expr, table) {
			return true
		}
	}
	if setSubqueryReadsTable(sel.Where, table) {
		return true
	}
	for _, g := range sel.GroupBy {
		if setSubqueryReadsTable(g, table) {
			return true
		}
	}
	if setSubqueryReadsTable(sel.Having, table) {
		return true
	}
	for _, o := range sel.OrderBy {
		if setSubqueryReadsTable(o.Expr, table) {
			return true
		}
	}
	for _, arm := range sel.Compound {
		if selectReferencesTableFrom(arm.Stmt, table) {
			return true
		}
	}
	for _, cte := range sel.CTEs {
		if selectReferencesTableFrom(cte.Select, table) {
			return true
		}
	}
	return false
}

// containsSubquery reports whether e's expression tree contains a
// SubqueryExpr, ExistsExpr, or the subquery form of InExpr anywhere --
// constructs that can only be evaluated against a live ReadOnlyPager, which
// this write-only path's rowEvalCtx does not always have (see its doc
// comment). Mirrors checkExprSupported's traversal shape.
func containsSubquery(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return false
	case SubqueryExpr, ExistsExpr:
		return true
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if containsSubquery(a) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return containsSubquery(x.X)
	case BinaryExpr:
		return containsSubquery(x.L) || containsSubquery(x.R)
	case IsNullExpr:
		return containsSubquery(x.X)
	case InExpr:
		if x.Sub != nil {
			return true
		}
		if containsSubquery(x.X) {
			return true
		}
		for _, a := range x.List {
			if containsSubquery(a) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return containsSubquery(x.X) || containsSubquery(x.Lo) || containsSubquery(x.Hi)
	case LikeExpr:
		return containsSubquery(x.X) || containsSubquery(x.Pattern) || containsSubquery(x.Escape)
	case GlobExpr:
		return containsSubquery(x.X) || containsSubquery(x.Pattern)
	case CollateExpr:
		return containsSubquery(x.X)
	case CastExpr:
		return containsSubquery(x.X)
	case CaseExpr:
		if x.Base != nil && containsSubquery(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if containsSubquery(w.When) || containsSubquery(w.Then) {
				return true
			}
		}
		return x.Else != nil && containsSubquery(x.Else)
	default:
		return false
	}
}

// setSubqueryCorrelatesToRow reports whether any subquery in e (a SET
// right-hand side) can reach the row being updated -- what makes a SET
// subquery over the target table unreproducible here. It lets the mainstream
// uncorrelated shape ("UPDATE t SET c = (SELECT max(c) FROM t)") through:
//
//	UPDATE u SET b=(SELECT sum(b) FROM u)              30,30,30   pre-update image
//	UPDATE v SET b=(SELECT count(*) FROM v WHERE b>15) 2,2,2      pre-update image
//	UPDATE t SET x=x+(SELECT count(*) FROM t t2
//	                  WHERE t2.x<=t.x)  over 1,2,3     2,4,5      LIVE, row by row
//
// The one correlation vector is a qualifier the subquery's own FROM does not
// introduce ("t.x" above, where t is aliased t2 inside). So the visible set
// starts empty: seeding it with the target's name would make that reference
// look local. An unqualified reference cannot escape, since the subquery's
// FROM contains the target table and binds every such name. Anything the
// walker cannot account for counts as correlated.
func setSubqueryCorrelatesToRow(e Expr, visible []string) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return false
	case ColumnExpr:
		return x.Qualifier != "" && !containsFold(visible, x.Qualifier)
	case SubqueryExpr:
		return selectCorrelatesToRow(x.Stmt, visible)
	case ExistsExpr:
		return selectCorrelatesToRow(x.Stmt, visible)
	case RowExpr:
		// A row value's elements are ordinary expressions with no resolution
		// barrier, so each gets the same qualifier check as any position; an
		// escaping "(t.x,t.y) IN (...)" is still caught, while "(a,b) IN
		// (SELECT x,y FROM t)" over unqualified columns is not falsely
		// correlated (in7.test).
		for _, el := range x.Elems {
			if setSubqueryCorrelatesToRow(el, visible) {
				return true
			}
		}
		return false
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if setSubqueryCorrelatesToRow(a, visible) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return setSubqueryCorrelatesToRow(x.X, visible)
	case BinaryExpr:
		return setSubqueryCorrelatesToRow(x.L, visible) || setSubqueryCorrelatesToRow(x.R, visible)
	case IsNullExpr:
		return setSubqueryCorrelatesToRow(x.X, visible)
	case InExpr:
		if x.Sub != nil && selectCorrelatesToRow(x.Sub, visible) {
			return true
		}
		if setSubqueryCorrelatesToRow(x.X, visible) {
			return true
		}
		for _, a := range x.List {
			if setSubqueryCorrelatesToRow(a, visible) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return setSubqueryCorrelatesToRow(x.X, visible) || setSubqueryCorrelatesToRow(x.Lo, visible) ||
			setSubqueryCorrelatesToRow(x.Hi, visible)
	case LikeExpr:
		return setSubqueryCorrelatesToRow(x.X, visible) || setSubqueryCorrelatesToRow(x.Pattern, visible) ||
			setSubqueryCorrelatesToRow(x.Escape, visible)
	case GlobExpr:
		return setSubqueryCorrelatesToRow(x.X, visible) || setSubqueryCorrelatesToRow(x.Pattern, visible)
	case CollateExpr:
		return setSubqueryCorrelatesToRow(x.X, visible)
	case CastExpr:
		return setSubqueryCorrelatesToRow(x.X, visible)
	case CaseExpr:
		if x.Base != nil && setSubqueryCorrelatesToRow(x.Base, visible) {
			return true
		}
		for _, w := range x.Whens {
			if setSubqueryCorrelatesToRow(w.When, visible) || setSubqueryCorrelatesToRow(w.Then, visible) {
				return true
			}
		}
		return x.Else != nil && setSubqueryCorrelatesToRow(x.Else, visible)
	default:
		// An expression node this walker does not model: assume it could carry
		// a reference out. See this function's doc comment.
		return true
	}
}

// selectCorrelatesToRow is setSubqueryCorrelatesToRow's per-SELECT half: it
// adds sel's own FROM-introduced names to the visible set and walks everything
// sel can hold. A FROM item shape it cannot name (a derived table with no
// alias, a table-valued function) makes the whole select count as correlated,
// for the reason the sibling's doc comment gives.
func selectCorrelatesToRow(sel *SelectStmt, visible []string) bool {
	if sel == nil {
		return true
	}
	inner := append([]string(nil), visible...)
	for _, it := range sel.From {
		switch {
		case it.Alias != "":
			inner = append(inner, it.Alias)
		case it.Subquery == nil && it.Table != "":
			inner = append(inner, it.Table)
		default:
			return true // unnamed derived table: nothing to bind a qualifier to
		}
	}
	for _, def := range sel.CTEs {
		inner = append(inner, def.Name)
	}
	for _, it := range sel.From {
		if it.Subquery != nil && selectCorrelatesToRow(it.Subquery, inner) {
			return true
		}
		if setSubqueryCorrelatesToRow(it.On, inner) {
			return true
		}
	}
	for _, c := range sel.Columns {
		if !c.Star && setSubqueryCorrelatesToRow(c.Expr, inner) {
			return true
		}
		if c.Star && c.StarQualifier != "" && !containsFold(inner, c.StarQualifier) {
			return true
		}
	}
	if setSubqueryCorrelatesToRow(sel.Where, inner) || setSubqueryCorrelatesToRow(sel.Having, inner) {
		return true
	}
	for _, g := range sel.GroupBy {
		if setSubqueryCorrelatesToRow(g, inner) {
			return true
		}
	}
	for _, ot := range sel.OrderBy {
		if setSubqueryCorrelatesToRow(ot.Expr, inner) {
			return true
		}
	}
	for _, def := range sel.CTEs {
		if selectCorrelatesToRow(def.Select, inner) {
			return true
		}
	}
	for _, arm := range sel.Compound {
		if selectCorrelatesToRow(arm.Stmt, visible) {
			return true
		}
	}
	return false
}
