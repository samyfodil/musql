// This file implements the RETURNING clause for INSERT/UPDATE/DELETE
// (parseReturningClause). RETURNING turns a write into a result set; the
// clause is compiled into the write program (emitReturning), and
// ExecReturningArgs is the entry point that hands back (cols, rows). Writes
// that collect rows rather than coding them in line (the vtab writes) capture
// them into a *returningState; others pass nil.
//
// Visibility matches C: INSERT and UPDATE RETURNING see the post-change row
// (with its assigned rowid and defaults), DELETE RETURNING the pre-delete row.
// Column naming uses expandSelectList over the target's scope, so "RETURNING
// *" expands to the declared columns and "RETURNING a, b+1" names "a" and
// "b+1". Row order is unspecified in SQLite; rows come out in a deterministic
// order and callers must sort if they care.
//
// A subquery in a RETURNING expression sees the database after that row's own
// write but before later rows' (over t1(a,b)=(1,10), "INSERT INTO t1(a,b)
// VALUES(2,20),(3,30) RETURNING a, (SELECT count(*) FROM t1)" is
// (2,2),(3,3)), so one frozen snapshot cannot answer it; where emitReturning
// cannot serve one (see returningSite) it declines.
//
// A BEFORE trigger's write to the same row shows in the captured row; an
// AFTER trigger's writes never add or change captured rows; a RAISE(IGNORE)
// skips the row. RETURNING inside a trigger body is an error in C, and is
// declined here.
package engine

import (
	"strings"
)

// returningState carries a resolved RETURNING clause into a write executor and
// collects the affected rows' evaluated expression values back out. nil at
// every call site that isn't a top-level RETURNING statement.
type returningState struct {
	plan *returningPlan
	rows [][]Value
}

// returningPlan is a RETURNING clause resolved against one target table: the
// output columns (expanded, named, and validated once) plus the table scope
// their expressions evaluate against.
type returningPlan struct {
	// caseSensitiveLike is the connection's "PRAGMA case_sensitive_like",
	// captured at PREPARE time (buildReturningPlan is a *DB method) so capture
	// can hand it to an evalCtx that has no pager. See evalCtx.caseSensitiveLike.
	caseSensitiveLike bool

	outCols []outputColumn
	// exprs is outCols' expressions, in order, resolved once as
	// codeReturningTrigger does (trigger.c:1090, 1095). Nothing reads it now:
	// emitReturning codes the list itself.
	exprs []Expr
	names []string
	tbl   *tableMeta
	// hasSubquery is true when any output expression contains one: computed
	// once so the per-row capture loop only pays for a fresh snapshot pager
	// (returningSubqueryPager) when a subquery could actually need to read
	// one. Mirrors vtabReturningPlan.hasSubquery (vtab_write.go).
	hasSubquery bool
}

// buildReturningPlan resolves ret against tbl: it expands "*"/"t.*", derives
// each output column's name (expandSelectList, as for SELECT), and checks every
// expression resolves only against tbl's columns, returning a decline or "no
// such column" rather than a wrong column. A subquery is not declined here;
// the caller (emitReturning) decides.
func (db *DB) buildReturningPlan(tbl *tableMeta, ret []SelectColumn) (*returningPlan, error) {
	scope := tableScope{
		name:      tbl.name,
		tableName: tbl.name,
		cols:      tbl.cols,
		colIndex:  buildColIndex(tbl.cols),
		offset:    0,
		noRowid:   tbl.withoutRowid,
	}
	mode := colNameMode{full: db.fullColumnNames, short: !db.shortColumnNamesOff}
	outCols, err := expandSelectList(ret, []tableScope{scope}, mode)
	if err != nil {
		return nil, err
	}
	planCtx := &evalCtx{tables: []tableScope{scope}}
	names := make([]string, len(outCols))
	exprs := make([]Expr, len(outCols))
	hasSubquery := false
	for i, oc := range outCols {
		if err := checkExprSupported(oc.expr); err != nil {
			return nil, err
		}
		if containsSubquery(oc.expr) {
			hasSubquery = true
		}
		if err := validateColumnRefs(oc.expr, planCtx); err != nil {
			// semanticf, so it stays the STATEMENT's error: RETURNING's own
			// expressions resolve against the target row like any others, so an
			// unresolvable name is C's prepare-time "no such column: x"
			// (resolve.c:785), and as a plain error it reached the caller
			// wrapped as a decline ("unsupported: engine: no such column: x").
			// The message is passed through verbatim -- it is already C's.
			return nil, semanticf("%s", err)
		}
		names[i] = oc.name
		exprs[i] = oc.expr
	}
	return &returningPlan{outCols: outCols, exprs: exprs, names: names, tbl: tbl, hasSubquery: hasSubquery,
		caseSensitiveLike: db.caseSensitiveLike}, nil
}

// ExecReturningArgs runs one INSERT/UPDATE/DELETE ... RETURNING and returns its
// result set (column names + rows), as QueryArgs does for a SELECT -- the
// entry point the driver routes a RETURNING Query to. ExecArgs runs the same
// statement and reports only the count, so a plain Exec works too. A
// statement without RETURNING returns nil rows and no error; callers check
// HasReturning first.
func (db *DB) ExecReturningArgs(sqlText string, args []Value) (cols []string, rows [][]Value, err error) {
	// A RETURNING statement is an INSERT/UPDATE/DELETE, so "PRAGMA query_only"
	// refuses it exactly as ExecArgs does -- see queryOnlyRefusesWrite.
	if qerr := db.queryOnlyRefusesWrite(sqlText); qerr != nil {
		return nil, nil, qerr
	}
	return db.execReturningViaVM(sqlText, args)
}

// LastInsertRowid returns this *DB session's current sqlite3_last_insert_rowid()
// value -- the rowid of the most recent successful INSERT (0 until the first).
// ExecArgs already returns this alongside its count; this accessor exposes the
// same value to a caller (driver) that ran an INSERT ... RETURNING through
// ExecReturningArgs, which returns rows rather than the count/last-id pair.
func (db *DB) LastInsertRowid() int64 { return db.lastInsertRowid }

// StatementHasReturning reports whether sqlText is an INSERT/UPDATE/DELETE
// carrying a RETURNING clause, so a driver can decide up front to route it
// through ExecReturningArgs (a result set) rather than ExecArgs (a count). A
// parse error here is reported as "no RETURNING" (false, nil): the statement
// will re-parse and surface the same error through whichever execution path
// runs it, exactly as an ordinary malformed statement does.
func statementHasReturningUncached(sqlText string) bool {
	trimmed := strings.TrimSpace(sqlText)
	toks, err := lex(trimmed)
	if err != nil {
		return false
	}
	switch writeDispatchKeyword(trimmed, toks) {
	case "INSERT", "REPLACE":
		stmt, err := parseInsertStmt(trimmed)
		return err == nil && stmt.returning != nil
	case "UPDATE":
		stmt, err := parseUpdateStmt(trimmed)
		return err == nil && stmt.returning != nil
	case "DELETE":
		stmt, err := parseDeleteStmt(trimmed)
		return err == nil && stmt.returning != nil
	}
	return false
}
