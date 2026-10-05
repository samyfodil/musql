// This file implements "PRAGMA trusted_schema=OFF", which prevents DDL-origin
// expressions from calling functions not tagged SQLITE_INNOCUOUS. It checks
// VIEW and TRIGGER bodies, which are the only sites reachable by unsafe virtuals
// (fts3/fts4/fts5 MATCH, etc.) in this engine.
// MAIN view does not, and so does a TEMP INSTEAD OF trigger whose body MATCHes.
// The exemption is per OBJECT, not per statement: a TEMP view whose body reads a
// MAIN view that MATCHes still fails, because the MAIN view's own body is
// checked when it is reached. Checking each object as it is used, rather than
// checking the outermost statement, is what makes that fall out for free.
//
// # Why the walk default-DENIES
//
// The check is STATIC over the object's whole parsed body, which is where real
// SQLite's is too (name resolution, not evaluation): a MATCH in a CASE branch
// that is never taken still fails there. An expression node shape this walk does
// not recognize therefore DECLINES (errVDBEUnsupported) instead of passing --
// the same default-deny fts3CountMatchExpr uses. A decline costs the statement;
// silently answering a statement C SQLite refuses is the wrong answer this
// whole flag exists to avoid. Nothing can reach that decline today (all 18 Expr
// node types are handled), and it is off by construction for the ON default.
package engine

import (
	"fmt"
)

// SetTrustedSchema sets this connection's "PRAGMA trusted_schema". ON (true) is
// the default, so the field is stored INVERTED: every *DB and *ReadOnlyPager
// built anywhere in this package is trusted by its zero value, and only an
// explicit OFF has to be carried.
func (db *DB) SetTrustedSchema(on bool) { db.untrustedSchema = !on }

// TrustedSchema reports this connection's "PRAGMA trusted_schema".
func (db *DB) TrustedSchema() bool { return db == nil || !db.untrustedSchema }

// SetTrustedSchema is SetTrustedSchema for a read snapshot, which is how the
// driver carries the connection flag into a read-only session (its
// autocommit model opens a fresh one per statement).
//
// A change DROPS the pager's plan cache. The walk that refuses a view body's
// unsafe function runs while the view is compiled into the statement's
// Program, so a Program cached under one setting carries that setting's
// verdict: "SELECT * FROM v" run once with the flag ON and again, on the same
// warm pager, after "PRAGMA trusted_schema=OFF" answered from the cached plan
// where 3.53.3 reports "unsafe use of MATCH()" (measured over an fts4 view).
func (p *ReadOnlyPager) SetTrustedSchema(on bool) {
	if p.untrustedSchema == on {
		p.planCache = nil
	}
	p.untrustedSchema = !on
}

// TrustedSchema reports the flag a read snapshot was stamped with.
func (p *ReadOnlyPager) TrustedSchema() bool { return p == nil || !p.untrustedSchema }

// unsafeSchemaUseErr is the error C SQLite reports for a non-innocuous
// FUNCTION used from a DDL-origin expression. The wording (and the "()" even
// for the MATCH operator) is byte-for-byte what the oracle produces.
func unsafeSchemaUseErr(fn string) error {
	return fmt.Errorf("engine: unsafe use of %s()", fn)
}

// unsafeSchemaVtabErr is the same for a non-innocuous virtual-table MODULE,
// which C SQLite names by the TABLE's name rather than the module's.
func unsafeSchemaVtabErr(table string) error {
	return fmt.Errorf("engine: unsafe use of virtual table %q", table)
}

// vtabModuleInnocuous reports whether module is tagged SQLITE_VTAB_INNOCUOUS,
// i.e. usable from a schema object with trusted_schema=OFF. Every name here was
// probed directly against the oracle except generate_series, which mattn's
// build does not compile in -- upstream series.c calls sqlite3_vtab_config(db,
// SQLITE_VTAB_INNOCUOUS), so it is listed as innocuous and is the one entry no
// differential gate here can pin.
//
// The default is NOT innocuous, matching SQLite (a module is unsafe unless it
// opts in), which is what puts fts4aux, fts3tokenize, fts5vocab and every
// pragma_* table-valued function on the unsafe side.
func vtabModuleInnocuous(module string) bool {
	switch r33sFoldIdent(module) {
	case "fts3", "fts4", "fts5", "rtree", "rtree_i32", "json_each", "json_tree",
		"jsonb_each", "jsonb_tree", "generate_series":
		return true
	}
	return false
}

// unsafeSchemaFunc reports whether a function call named fn is non-innocuous,
// i.e. reaches this engine only as a virtual table's xFindFunction overload.
// None of these exists as an ordinary scalar here, so a call that does NOT bind
// to a virtual table errors either way ("no such function") and flagging it
// costs nothing but the wording.
func unsafeSchemaFunc(fn string) bool {
	switch r33sFoldIdent(fn) {
	case "offsets", "snippet", "matchinfo", "optimize", // fts3/fts4
		"bm25", "highlight", // fts5 (snippet is shared with fts3)
		"fts5",                                  // fts5_main.c:3821, no SQLITE_INNOCUOUS
		"rtreecheck", "rtreedepth", "rtreenode", // rtree
		"load_extension", "fts3_tokenizer", // also SQLITE_DIRECTONLY
		"authenticate", "auth_user_add", "auth_user_change", "auth_user_delete", "auth_enabled":
		return true
	}
	return false
}

// directOnlySchemaFunc reports whether fn is tagged SQLITE_DIRECTONLY, which
// sqlite3ExprFunctionUsable (expr.c:1286) refuses from a schema object whether
// or not the schema is trusted: load_extension is an SFUNCTION (func.c:3290,
// sqliteInt.h:2144) and fts3_tokenizer is registered SQLITE_DIRECTONLY
// (fts3_tokenizer.c:480).
func directOnlySchemaFunc(fn string) bool {
	switch r33sFoldIdent(fn) {
	case "load_extension", "fts3_tokenizer":
		return true
	}
	return false
}

// trustedSchemaWalk is the walk itself, parameterised only by how a FROM-item
// name resolves to its virtual-table MODULE ("" when it is not one). The two
// callers differ in nothing else: a read snapshot answers from its schema rows,
// a live write session from its in-memory vtab metas.
type trustedSchemaWalk struct {
	// moduleOf resolves a FROM-item name against the schema. known is false
	// when NOTHING of that name exists there, which is what tells a BARE
	// eponymous-module reference apart from an ordinary table (SQLite resolves
	// one only AFTER the table lookup fails -- its module-shadowing rule).
	// module is "" for a name that exists but is not a virtual table.
	moduleOf func(table string) (module string, known bool)
	// cte counts the names bound by WITH clauses currently in scope, so a CTE
	// that happens to share an eponymous module's name shadows it here exactly
	// as it does in C SQLite -- an over-fire would refuse what the oracle
	// allows, which is as wrong as the miss it is guarding against.
	cte map[string]int
	// directOnly is set when the schema IS trusted: only a SQLITE_DIRECTONLY
	// function is then refused, nothing is declined, and moduleOf is never
	// consulted.
	directOnly bool
}

// checkTrustedSchemaSelect is the guard the VIEW-body site uses: it returns nil
// unless this connection turned trusted_schema OFF and the object being used
// lives outside the TEMP catalog, and otherwise walks sel for the first
// non-innocuous use.
func (p *ReadOnlyPager) checkTrustedSchemaSelect(isTemp bool, sel *SelectStmt) error {
	if isTemp || sel == nil {
		return nil
	}
	w := p.trustedSchemaWalk()
	w.directOnly = p == nil || !p.untrustedSchema
	return w.walkTrustedSchemaSelect(sel)
}

func (p *ReadOnlyPager) trustedSchemaWalk() trustedSchemaWalk {
	return trustedSchemaWalk{
		cte: map[string]int{},
		moduleOf: func(table string) (string, bool) {
			if module, _, ok, err := p.createdVtabDef(table); err == nil && ok {
				return module, true
			}
			rows, err := p.Schema()
			if err != nil {
				return "", true // unreadable schema: say nothing rather than guess
			}
			for i := range rows {
				if equalFoldName(rows[i].Name, table) {
					return "", true
				}
			}
			return "", false
		},
	}
}

// checkTrustedSchemaTrigger is the guard the TRIGGER-body site uses. A trigger
// body is not one SelectStmt: it is a WHEN clause plus a list of INSERT/UPDATE/
// DELETE/SELECT steps, each of which is walked in full.
func (db *DB) checkTrustedSchemaTrigger(tr *triggerMeta) error {
	if db == nil || tr == nil || tr.isTemp {
		return nil
	}
	w := trustedSchemaWalk{
		directOnly: !db.untrustedSchema,
		cte:        map[string]int{},
		moduleOf: func(table string) (string, bool) {
			if v := db.findVtabMeta(table); v != nil {
				return v.module, true
			}
			if db.findTableMeta(table) != nil || db.findViewMeta(table) != nil {
				return "", true
			}
			return "", false
		},
	}
	if err := w.walkTrustedSchemaExpr(tr.when); err != nil {
		return err
	}
	for _, bs := range tr.body {
		if err := w.walkTrustedSchemaBodyStmt(bs); err != nil {
			return err
		}
	}
	return nil
}

// walkTrustedSchemaBodyStmt walks one trigger-body step. Every expression a
// step can hold is covered: an INSERT's VALUES tuples, its source SELECT and
// its upsert clause; an UPDATE's SET list, FROM items and WHERE; a DELETE's
// WHERE; a bare SELECT step. RETURNING is not reachable from a trigger body
// (the parser rejects it there, exactly as C SQLite does).
func (w trustedSchemaWalk) walkTrustedSchemaBodyStmt(bs triggerBodyStmt) error {
	switch {
	case bs.insert != nil:
		for _, row := range bs.insert.rows {
			for _, e := range row {
				if err := w.walkTrustedSchemaExpr(e); err != nil {
					return err
				}
			}
		}
		if err := w.walkTrustedSchemaSelect(bs.insert.selectStmt); err != nil {
			return err
		}
		for u := bs.insert.upsert; u != nil; u = u.next {
			for _, a := range u.sets {
				if err := w.walkTrustedSchemaExpr(a.expr); err != nil {
					return err
				}
			}
			if err := w.walkTrustedSchemaExpr(u.where); err != nil {
				return err
			}
		}
		return nil
	case bs.update != nil:
		for _, a := range bs.update.sets {
			if err := w.walkTrustedSchemaExpr(a.expr); err != nil {
				return err
			}
		}
		for i := range bs.update.from {
			if err := w.walkTrustedSchemaFrom(bs.update.from[i]); err != nil {
				return err
			}
		}
		return w.walkTrustedSchemaExpr(bs.update.where)
	case bs.delete != nil:
		return w.walkTrustedSchemaExpr(bs.delete.where)
	case bs.sel != nil:
		return w.walkTrustedSchemaSelect(bs.sel)
	}
	return nil
}

func (w trustedSchemaWalk) walkTrustedSchemaSelect(sel *SelectStmt) error {
	if sel == nil {
		return nil
	}
	// Names this statement's WITH clause binds shadow any eponymous module of
	// the same name, for as long as it is in scope.
	for _, cte := range sel.CTEs {
		w.cte[r33sFoldIdent(cte.Name)]++
	}
	defer func() {
		for _, cte := range sel.CTEs {
			w.cte[r33sFoldIdent(cte.Name)]--
		}
	}()
	for _, c := range sel.Columns {
		if err := w.walkTrustedSchemaExpr(c.Expr); err != nil {
			return err
		}
	}
	for i := range sel.From {
		if err := w.walkTrustedSchemaFrom(sel.From[i]); err != nil {
			return err
		}
	}
	for _, e := range []Expr{sel.Where, sel.Having, sel.LimitParam, sel.OffsetParam} {
		if err := w.walkTrustedSchemaExpr(e); err != nil {
			return err
		}
	}
	for _, e := range sel.GroupBy {
		if err := w.walkTrustedSchemaExpr(e); err != nil {
			return err
		}
	}
	for _, o := range sel.OrderBy {
		if err := w.walkTrustedSchemaExpr(o.Expr); err != nil {
			return err
		}
	}
	for _, nw := range sel.Windows {
		if nw.Spec == nil {
			continue
		}
		if err := w.walkTrustedSchemaWindow(*nw.Spec); err != nil {
			return err
		}
	}
	for _, cte := range sel.CTEs {
		if err := w.walkTrustedSchemaSelect(cte.Select); err != nil {
			return err
		}
	}
	for _, arm := range sel.Compound {
		if err := w.walkTrustedSchemaSelect(arm.Stmt); err != nil {
			return err
		}
	}
	return nil
}

// walkTrustedSchemaFrom classifies one FROM item. A DERIVED table recurses; a
// table-valued function names its module directly; an ordinary name is looked up
// in this pager's schema and is only interesting when it turns out to be a
// persisted virtual table. A name that resolves to nothing at all -- a CTE
// reference, a missing table, another VIEW -- is left alone: the view's own
// resolution will check its own body when it is reached, which is what makes a
// chain of views behave like the oracle's.
func (w trustedSchemaWalk) walkTrustedSchemaFrom(it FromItem) error {
	if err := w.walkTrustedSchemaExpr(it.On); err != nil {
		return err
	}
	for _, a := range it.TableFuncArgs {
		if err := w.walkTrustedSchemaExpr(a); err != nil {
			return err
		}
	}
	if it.Subquery != nil {
		return w.walkTrustedSchemaSelect(it.Subquery)
	}
	if it.Table == "" || w.directOnly {
		return nil
	}
	if it.TableFunc {
		// "name(args)" names an EPONYMOUS module directly.
		if !vtabModuleInnocuous(it.Table) {
			return unsafeSchemaVtabErr(it.Table)
		}
		return nil
	}
	if w.cte[r33sFoldIdent(it.Table)] > 0 {
		return nil // bound by an enclosing WITH clause, not a schema object
	}
	module, known := w.moduleOf(it.Table)
	if known {
		if module != "" && !vtabModuleInnocuous(module) {
			return unsafeSchemaVtabErr(it.Table)
		}
		return nil
	}
	// Nothing of that name in the schema, so it may be a BARE eponymous-module
	// reference -- the paren-less "FROM pragma_table_info" form, which real
	// SQLite resolves only after the table lookup fails and which it refuses
	// with the flag off exactly like the "name(args)" one (verified directly).
	if _, ok := lookupVtabModule(it.Table); ok && !vtabModuleInnocuous(it.Table) {
		return unsafeSchemaVtabErr(it.Table)
	}
	return nil
}

func (w trustedSchemaWalk) walkTrustedSchemaWindow(spec WindowSpec) error {
	for _, e := range spec.PartitionBy {
		if err := w.walkTrustedSchemaExpr(e); err != nil {
			return err
		}
	}
	for _, o := range spec.OrderBy {
		if err := w.walkTrustedSchemaExpr(o.Expr); err != nil {
			return err
		}
	}
	if spec.Frame != nil {
		for _, e := range []Expr{spec.Frame.Start.Offset, spec.Frame.End.Offset} {
			if err := w.walkTrustedSchemaExpr(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkTrustedSchemaExpr walks one expression. It DEFAULT-DENIES: a node shape
// not listed here declines rather than passing, so a future Expr type cannot
// silently open a hole in the rule (see this file's doc comment).
func (w trustedSchemaWalk) walkTrustedSchemaExpr(e Expr) error {
	if e == nil {
		return nil
	}
	sub := func(es ...Expr) error {
		for _, x := range es {
			if err := w.walkTrustedSchemaExpr(x); err != nil {
				return err
			}
		}
		return nil
	}
	switch x := e.(type) {
	case LiteralExpr, ParamExpr, ColumnExpr:
		return nil
	case MatchExpr:
		// The operator itself, whatever it is applied to: C SQLite resolves
		// "X MATCH Y" as match(Y,X), which only ever exists as a virtual
		// table's overload -- so it is unsafe when it binds and "unable to use
		// function MATCH in the requested context" when it does not. Both are
		// errors, so flagging every one of them is exact on the axis that
		// matters.
		if w.directOnly {
			return sub(x.X, x.Pattern)
		}
		return unsafeSchemaUseErr("MATCH")
	case FuncExpr:
		if directOnlySchemaFunc(x.Name) || (!w.directOnly && unsafeSchemaFunc(x.Name)) {
			return unsafeSchemaUseErr(r33sFoldIdent(x.Name))
		}
		if err := sub(x.Args...); err != nil {
			return err
		}
		if err := sub(x.Filter); err != nil {
			return err
		}
		if err := sub(x.orderByExprs()...); err != nil {
			return err
		}
		if x.Over != nil {
			return w.walkTrustedSchemaWindow(*x.Over)
		}
		return nil
	case UnaryExpr:
		return sub(x.X)
	case BinaryExpr:
		return sub(x.L, x.R)
	case IsNullExpr:
		return sub(x.X)
	case InExpr:
		if err := sub(x.X); err != nil {
			return err
		}
		if err := sub(x.List...); err != nil {
			return err
		}
		return w.walkTrustedSchemaSelect(x.Sub)
	case BetweenExpr:
		return sub(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return sub(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return sub(x.X, x.Pattern)
	case CollateExpr:
		return sub(x.X)
	case CastExpr:
		return sub(x.X)
	case CaseExpr:
		if err := sub(x.Base, x.Else); err != nil {
			return err
		}
		for _, w := range x.Whens {
			if err := sub(w.When, w.Then); err != nil {
				return err
			}
		}
		return nil
	case SubqueryExpr:
		return w.walkTrustedSchemaSelect(x.Stmt)
	case ExistsExpr:
		return w.walkTrustedSchemaSelect(x.Stmt)
	case RowExpr:
		return sub(x.Elems...)
	case RaiseExpr:
		return sub(x.Msg)
	default:
		if w.directOnly {
			return nil
		}
		return fmt.Errorf("%w: PRAGMA trusted_schema=OFF over a %T inside a schema object (this rule's walk classifies every expression a schema object may hold, and declines rather than passing one it does not recognize)", errVDBEUnsupported, e)
	}
}
