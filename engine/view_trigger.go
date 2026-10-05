// This file implements INSTEAD OF triggers on views and the view-DML routing
// they need. A DML statement against a VIEW (INSERT INTO v / UPDATE v / DELETE
// FROM v) is not a base-table write: SQLite rejects it outright UNLESS the view
// has a matching INSTEAD OF trigger, in which case the trigger's body runs
// INSTEAD of touching the view, with OLD/NEW bound to the view's own row image.
// vdbe_view_write.go, which lowers a view write into the write VM, routes
// here for the schema-only pieces once the target resolves to a view rather
// than a table.
//
// Every behavior below was verified directly against C SQLite (the CGo
// mattn/go-sqlite3 oracle, SQLite 3.53.3) before being implemented:
//
//   - No matching INSTEAD OF trigger: every write verb against a view errors
//     "cannot modify <view> because it is a view" (viewModifyError, view.go) --
//     unchanged from before this feature; this file only intercepts the case
//     where a matching trigger DOES exist.
//   - INSTEAD OF INSERT: NEW is the row being inserted, its columns mapped onto
//     the VIEW's own column layout (an explicit "(col,...)" insert list, or
//     positional). OLD is unavailable ("no such column: OLD.x"). The view's
//     column names/count come from ANALYZING (not executing) its SELECT -- so a
//     view whose SELECT names a missing table still errors "no such table:
//     main.<t>" even for INSERT (verified), while a per-row runtime error in
//     the view's SELECT body is NOT hit (INSERT never materializes the view).
//     The trigger body fires once per inserted row (INSERT ... VALUES(...),(...)
//     or INSERT ... SELECT), most-recently-created trigger first.
//   - INSTEAD OF DELETE: the view is MATERIALIZED (its SELECT executed), the
//     statement's WHERE is evaluated against each view row (OLD), and the
//     trigger body fires once per matching row -- all matching rows are
//     collected up front (verified: a body that DELETEs the base table on the
//     first fire still fires for every originally-matching row).
//   - INSTEAD OF UPDATE: like DELETE, but NEW is OLD with the SET assignments
//     applied -- every SET right-hand side evaluated against the OLD (current
//     view) row, simultaneously ("SET x=y, y=x" swaps), columns not named in
//     SET keeping their OLD value. SET targets and WHERE/RHS references resolve
//     against the view's columns ("no such column: <c>" otherwise). The
//     assembled NEW row then takes the VIEW's OWN per-column affinities, which
//     update.c applies UNGUARDED (sqlite3TableAffinity, update.c:983) -- the
//     opposite of the INSERT path, where insert.c:1490 skips it for a view.
//     So "UPDATE v SET t=5" on a TEXT-affinity view column gives NEW.t the
//     TEXT '5', and a WHEN clause reading it sees the converted value.
//   - WHEN: evaluated per row against OLD/NEW, gating the body exactly like a
//     table trigger's WHEN.
//   - Multiple matching INSTEAD OF triggers all fire, most-recently-created
//     first (matchingInsteadOfTriggers) -- identical to table-trigger ordering.
//   - An OR clause on the statement ("INSERT OR IGNORE INTO v ...", "REPLACE
//     INTO v ...", "UPDATE OR REPLACE v ...") governs the trigger body, exactly
//     as it does for a table target. An INSTEAD OF trigger is coded by the very
//     call that codes a BEFORE one -- insert.c:1495 / update.c:984 both pass
//     the statement's own onError to sqlite3CodeRowTrigger -- and
//     codeTriggerProgram (trigger.c:1137) then imposes it on every body step.
//     A DELETE is the exception in both spellings: delete.c:651 hands
//     OE_Default, so an enclosing policy stops there.
//     Consequences worth stating, because they are what a statement snapshot
//     gets wrong: OR FAIL keeps the rows the fires BEFORE the failing one wrote
//     (vdbe.c:1268, "Do not rollback if P2==OE_Fail"), while OR ABORT undoes
//     them; and total_changes() keeps every body step that COMPLETED even when
//     the statement then aborts, because trigger.c emits an OP_ResetCount after
//     each step (trigger.c:1156/1167/1175) and vdbe.c:6012's
//     "sqlite3VdbeSetChanges(db, p->nChange)" folds that step's count into
//     db->nTotalChange there and then -- a total that has no undo path.
//   - An UPSERT ("... ON CONFLICT(...) DO ...") against a view is real
//     SQLite's OWN error, "cannot UPSERT a view" (insert.c:1296), raised at
//     PREPARE time -- see vdbe_view_write.go's decline list, which keeps the
//     refusal a COMPILE-time one so changes() is left alone.
//   - A view DML statement reports 0 rows affected and does not advance
//     last_insert_rowid (both verified: changes()==0, and last_insert_rowid()
//     reverts to its pre-statement value after the trigger program) -- the
//     latter falls out for free from the save/restore of db.lastInsertRowid
//     every trigger fire already does (vdbe_trigger.go).
//   - DROP VIEW cascades to the view's INSTEAD OF triggers (removeView, view.go).
//
// Declined cleanly (never risked wrong), each an "unsupported" self-description
// the differential harness skips on both sides rather than a wrong answer:
//   - INSERT/UPDATE/DELETE ... RETURNING against a view. C SQLite SERVES
//     this (the RETURNING pseudo-trigger is a TRIGGER_AFTER trigger,
//     build.c:1465, and insert.c's AFTER-trigger call is outside the
//     `if( !isView )` guard), returning the NEW row for INSERT/UPDATE and the
//     OLD row for DELETE -- but returningPlan is keyed on a *tableMeta
//     (returning_write.go), which a view has none of, so it is declined here
//     rather than answered against the wrong row image.
//   - UPDATE ... FROM against a view whose join MULTI-MATCHES -- two join rows
//     carrying the same view row. The plain form is SERVED
//     (vdbe_view_update_from.go); only that ambiguity is refused, by
//     checkViewUpfromRows, whose doc comment carries the measured evidence
//     for why.
//   - A NEW/OLD reference reaching a view row this path cannot compute (a view
//     whose materialization itself is declined by the read path) surfaces that
//     read-path decline unchanged.
package engine

import (
	"fmt"
)

// viewColumnInfos resolves a view's output column layout (names + affinities)
// WITHOUT executing its SELECT's per-row projection -- the schema-only analysis
// INSTEAD OF INSERT needs (SQLite maps NEW.<col> onto these, and materializes
// nothing). It mirrors viewOutputRows' column half (resolveFrom + expandSelectList
// + derivedColumnInfos + the explicit "(col,...)" rename), and routes a
// missing-table error through rewriteViewNoSuchTable so an INSERT into a view
// over a not-yet-existing table reports "no such table: main.<t>" exactly like
// a query against the same view does (verified directly against C SQLite).
func (p *ReadOnlyPager) viewColumnInfos(viewName string, pcv *parsedCreateView) ([]columnInfo, error) {
	core := pcv.selectStmt
	if core == nil {
		return nil, fmt.Errorf("engine: view %s has no SELECT", viewName)
	}
	// The same deferred check viewOutputRows makes (view_collate.go), for the
	// column-only path an INSTEAD OF trigger takes: naming a view's columns
	// still means resolving its body, so an unrecognized COLLATE name
	// anywhere in it must be caught here too, before NEW.<col> gets mapped
	// onto columns whose declared collation was never actually resolved.
	if name := firstUnknownViewCollation(core); name != "" {
		return nil, errUnknownViewCollation(name)
	}
	// The same DDL-origin check viewOutputRows makes, for the column-only path
	// an INSTEAD OF trigger takes: naming a view's columns still means resolving
	// its body, so with trusted_schema=OFF an INSERT into a view whose body uses
	// something non-innocuous is refused before any trigger fires -- verified
	// directly against the oracle. See trusted_schema.go.
	if err := p.checkTrustedSchemaSelect(pcv.isTemp, core); err != nil {
		return nil, err
	}
	// A view body sees its OWN WITH and nothing else, which is TWO independent
	// facts SQLite carries in one place: select.c:5991 pushes the body's WITH
	// and, when the body has none, synthesises an empty one purely to carry the
	// bView flag -- whose only job is to stop searchWith's outward walk
	// (select.c:5627, "if( p->bView ) break;"). Both halves are needed here
	// because resolveFrom/expandSelectList/derivedColumnInfos below all
	// RE-DERIVE from the AST, and SQLite binds each result expression to its
	// source exactly once, inside that push (select.c:6000) -- it never
	// re-resolves. Without the push, a CTE shadowing a same-named table is
	// invisible and an INSTEAD OF trigger's NEW.<col> maps onto the SHADOWED
	// base table's columns; without the hide, the body would see an ENCLOSING
	// statement's CTE that C SQLite keeps out of it. Same fix as
	// viewOutputRows (view.go) and subqueryScopes (subquery_validate.go).
	defer p.hideCTEScopes()()
	defer p.pushCTEScope(core.CTEs)()
	var scopes []tableScope
	if len(core.From) > 0 {
		jts, _, err := p.resolveFrom(core.From, nil)
		if err != nil {
			return nil, rewriteViewNoSuchTable(err)
		}
		scopes = buildScopes(jts)
	}
	outCols, err := expandSelectList(core.Columns, scopes, defaultColNameMode)
	if err != nil {
		return nil, rewriteViewNoSuchTable(err)
	}
	names := make([]string, len(outCols))
	for i, oc := range outCols {
		names[i] = oc.name
	}
	cols := p.derivedColumnInfos(core, names)
	if pcv.colNames != nil {
		if len(pcv.colNames) != len(cols) {
			return nil, fmt.Errorf("engine: expected %d columns for '%s' but got %d", len(pcv.colNames), viewName, len(cols))
		}
		renamed := make([]columnInfo, len(cols))
		copy(renamed, cols)
		for i, n := range pcv.colNames {
			renamed[i].Name = n
		}
		cols = renamed
	}
	return cols, nil
}

// viewMetaToParsed adapts a stored viewMeta into the parsedCreateView shape the
// read-path column/row resolvers (viewColumnInfos/viewOutputRows) consume.
func viewMetaToParsed(vm *viewMeta) *parsedCreateView {
	return &parsedCreateView{isTemp: vm.isTemp, name: vm.name, colNames: vm.colNames, selectStmt: vm.selectStmt}
}

// ---- OLD/NEW pseudo-table scopes over a VIEW's columns ----

// viewOldNewScopes builds the "old"/"new" tableScope pair a view INSTEAD OF
// trigger's WHEN/body resolves OLD./NEW. against, from the VIEW's own output
// columns -- the exact analogue of triggerOldNewScopes (trigger.go) for a view.
// Both scopes always exist so a qualified reference to either always finds SOME
// scope; the side the event doesn't provide (NEW for DELETE, OLD for INSERT) is
// built EMPTY so a reference into it fails "no such column: OLD.x"/"NEW.x"
// (verified directly). noRowid is set on both: a view exposes no rowid, so
// OLD.rowid/NEW.rowid never resolve. unqualifiedHidden is set exactly as for
// table triggers (an unqualified column reference never resolves against
// OLD/NEW).
func viewOldNewScopes(viewCols []columnInfo, event triggerEventKind) (oldTS, newTS tableScope) {
	hasOld := event == triggerUpdate || event == triggerDelete
	hasNew := event == triggerUpdate || event == triggerInsert

	if hasOld {
		oldTS = tableScope{name: "old", cols: viewCols, colIndex: buildColIndex(viewCols), offset: 0, noRowid: true, unqualifiedHidden: true}
	} else {
		oldTS = tableScope{name: "old", colIndex: map[string]int{}, noRowid: true, unqualifiedHidden: true}
	}
	newOffset := 0
	if hasOld {
		newOffset = len(viewCols)
	}
	if hasNew {
		newTS = tableScope{name: "new", cols: viewCols, colIndex: buildColIndex(viewCols), offset: newOffset, noRowid: true, unqualifiedHidden: true}
	} else {
		newTS = tableScope{name: "new", colIndex: map[string]int{}, noRowid: true, unqualifiedHidden: true}
	}
	return oldTS, newTS
}

// viewTriggerSchemaCtx builds a schema-only (no row values) OLD/NEW evalCtx for
// a view trigger firing -- used by validateViewTriggerExprsOnce to catch a bad
// OLD/NEW column reference eagerly (once per statement, even when zero view rows
// ultimately match), mirroring triggerSchemaCtx (trigger.go).
func viewTriggerSchemaCtx(viewCols []columnInfo, event triggerEventKind) *evalCtx {
	oldTS, newTS := viewOldNewScopes(viewCols, event)
	return &evalCtx{tables: []tableScope{oldTS, newTS}}
}

// validateViewTriggerExprsOnce is validateTriggerExprsOnce (trigger.go) for a
// view trigger: it validates every candidate trigger's WHEN and body-statement
// expressions against the view's OLD/NEW schema-only scopes, eagerly, so a bad
// OLD/NEW reference surfaces even when zero view rows ultimately fire.
func (db *DB) validateViewTriggerExprsOnce(viewCols []columnInfo, event triggerEventKind, trs []*triggerMeta) error {
	schemaOuter := viewTriggerSchemaCtx(viewCols, event)
	for _, tr := range trs {
		if tr.when != nil {
			if err := validateColumnRefs(tr.when, schemaOuter); err != nil {
				return err
			}
		}
		for _, bs := range tr.body {
			if err := db.validateTriggerBodyStmtExprsOnce(bs, schemaOuter); err != nil {
				return err
			}
			if bs.sel != nil {
				if err := db.validateTrivialBodySelectExprs(bs.sel, schemaOuter); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---- INSTEAD OF INSERT ----

// viewInsertColIdx resolves an explicit "(col,...)" insert list to indices into
// viewCols, erroring "table <v> has no column named <c>" for an unknown name.
// Returns nil for a positional (no column list) insert.
func viewInsertColIdx(cols []string, viewName string, viewCols []columnInfo) ([]int, error) {
	if cols == nil {
		return nil, nil
	}
	ci := buildColIndex(viewCols)
	colIdx := make([]int, len(cols))
	for i, cn := range cols {
		idx, ok := ci[r33sFoldIdent(cn)]
		if !ok {
			return nil, fmt.Errorf("engine: table %s has no column named %s", viewName, cn)
		}
		colIdx[i] = idx
	}
	return colIdx, nil
}

// checkViewInsertArity validates one tuple's value count against the view's
// column layout, reproducing C SQLite's two distinct wordings.
func checkViewInsertArity(cols []string, viewName string, nCols int, colIdx []int, nVals int) error {
	if cols == nil {
		if nVals != nCols {
			return fmt.Errorf("engine: table %s has %d columns but %d values were supplied", viewName, nCols, nVals)
		}
		return nil
	}
	if nVals != len(colIdx) {
		return fmt.Errorf("engine: %d values for %d columns", nVals, len(colIdx))
	}
	return nil
}

// ---- INSTEAD OF DELETE / UPDATE (view materialization) ----

// returningContainsSubquery reports whether any RETURNING column holds a
// subquery.
func returningContainsSubquery(ret []SelectColumn) bool {
	for _, rc := range ret {
		if containsSubquery(rc.Expr) {
			return true
		}
	}
	return false
}

// setsContainSubquery reports whether any SET right-hand side holds a subquery
// (so a subquery pager must be built for an UPDATE-on-view's SET evaluation).
func setsContainSubquery(sets []assignment) bool {
	for _, a := range sets {
		if containsSubquery(a.expr) {
			return true
		}
	}
	return false
}
