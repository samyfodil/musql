// FROM-clause resolution: resolving each FromItem (sql_ast.go) to its columns,
// desugaring NATURAL/USING joins, and planning join order and WHERE pushdown
// for the VDBE's join codegen (vdbe_join_codegen.go).
//
// ON vs WHERE for a LEFT JOIN: only ON decides match vs NULL-extension; WHERE
// filters the already-decided row. So "LEFT JOIN t2 ON a=a AND t2.b>10" still
// NULL-extends when the only candidate has b<=10, while "... ON a=a WHERE
// t2.b>10" drops the row.
//
// Pushdown: a WHERE conjunct runs as soon as every table it references is
// bound (planJoinPushdown), except conjuncts touching a LEFT-joined table or
// later, or containing a correlated subquery, which wait for the final row.
// Without it, long comma joins (sqllogictest select4/select5, up to 64 tables)
// are infeasible.
//
// RIGHT/FULL JOIN is supported anywhere in a flat FROM chain. Its match
// decision is global over the right table's own rows across the whole
// preceding search space: C runs the ordinary nested loop in FROM order, then
// a second scan of the right table emitting one NULL-extended row (NULLing
// every preceding table) per never-matched row. FULL is that sweep added to
// LEFT's inline extension. The codegen does this with an OpRightJoinMark
// bitmap and an appended sweep per right-outer source (emitRightOuterSweep).
// Because the decision is global, a FROM with a RIGHT/FULL item is never
// reordered and no WHERE conjunct is pushed down: pruning a combination early
// could turn a real match into a spurious NULL-extension. A parenthesized
// sub-join tree containing an outer join is declined at parse time
// (checkFlattenSafe, sql_parser.go).
package engine

import (
	"errors"
	"fmt"
	"sort"
)

// joinedTable is one resolved+materialized FROM item: its scope name (alias,
// or its own table name if unaliased), table metadata, every one of its rows
// (already normalizeRow'd), its column offset within a joined row's
// concatenated Value slice, and -- for every item but the first -- how it's
// joined onto the tables before it (left/on).
type joinedTable struct {
	name string
	// tableName is the real underlying table name (FromItem.Table), carried
	// through to tableScope.tableName for full_column_names result naming --
	// see that field's doc comment. Equals the alias for a
	// derived table, "" only for an unaliased derived table.
	tableName string
	tbl       *resolvedTable
	rows      [][]Value
	rowids    []int64 // rowids[i] is the b-tree rowid of rows[i] -- parallel to rows, same length, same order; feeds the rowid/oid/_rowid_ pseudo-column (see resolveColumn/evalCtx.rowids)
	offset    int
	left      bool // true if this is a LEFT JOIN target (needs NULL-extension when on has no match)
	on        Expr // join condition; nil for the base item, a comma/CROSS join, and a NATURAL join with no common columns

	// rightOuter is true for a RIGHT or FULL JOIN target, which needs the
	// global match tracking and appended sweep described in the package doc,
	// on top of `left` (false for RIGHT, true for FULL).
	rightOuter bool

	// coalesced is this table's tableScope.coalesced: the USING/NATURAL
	// coalescing info desugarJoinItem computed, or nil. buildScopes installs it.
	coalesced map[string]int

	// coalesceFallback is this table's tableScope.coalesceFallback: each later
	// RIGHT/FULL target whose USING/NATURAL chose this table as representative
	// appends its index, in FROM order (installCoalesceFallback).
	coalesceFallback map[string][]int

	// derived is true for a derived table (FromItem.Subquery): tbl is a
	// synthetic *resolvedTable with only the output columns, and its scope
	// exposes no rowid pseudo-column (tableScope.noRowid).
	derived bool

	// nestedNonLeading mirrors FromItem.NestedNonLeading, for buildScopes.
	nestedNonLeading bool

	// cross and wherePlanOK are read only by the ported planner
	// (computeExecOrder, where_plan.go). cross is FromItem.CrossKeyword (a
	// reorder barrier in SQLite); wherePlanOK is markWherePlanEligibility's
	// verdict that the port covers this FROM clause.
	cross       bool
	wherePlanOK bool
	// colUsed/colUsedOK are SrcItem.colUsed (see joinSource's copy and
	// wherePlanAutoIndexKey). onToWhere marks an ON clause bucketed as a WHERE
	// conjunct, as sqlite3ProcessJoin does (joinSource.onToWhere).
	colUsed   uint64
	colUsedOK bool
	onToWhere bool
	// noAutoIndex is "PRAGMA automatic_index = OFF" (see sqliteExecOrder).
	noAutoIndex bool

	// idxOrderKey is joinSource.idxOrderKey: the index the planner scans a
	// single-table FROM through.
	idxOrderKey *autoIndexKey

	// isFtsVtab is joinSource.isFtsVtab, read only by ftsMatchForcedOrder.
	isFtsVtab bool

	// groupLen mirrors FromItem.GroupLen: > 0 marks the first item of a
	// parenthesized join group spanning groupLen items. The item's other fields
	// describe how the whole group attaches to what precedes it.
	groupLen int

	// origIdx is this item's absolute position in the statement's flat FROM list
	// (its index into the scopes built by buildScopes), which survives when a
	// group is folded into one entry.
	origIdx int

	// groupSpan is 1 for an ordinary item, or for a synthetic group entry the
	// number of original tables it spans (origIdx..origIdx+groupSpan-1).
	groupSpan int

	// groupRowids is non-nil only for a synthetic group entry: groupRowids[ri]
	// holds the rowid pseudo-column of each of the group's member tables for row
	// ri, parallel to rows[ri]. nil for an ordinary item.
	groupRowids [][]Value

	// tvfArgs is a table-valued function's argument list, whose column
	// references are dependencies of its binding position
	// (placeAfterTvfArgs); nil for every other source.
	tvfArgs []Expr
}

// resolveFrom resolves every table named in a FROM clause, computes each
// table's column offset in a joined row, and returns the flattened column
// metadata (every table's columns in FROM order) needed for "*" expansion and
// plan-time affinity lookups.
func (p *ReadOnlyPager) resolveFrom(items []FromItem, params []Value) ([]joinedTable, []columnInfo, error) {
	if err := p.checkDerivedJoinSupported(items); err != nil {
		return nil, nil, err
	}
	if err := checkSelfJoinGroupSupported(items); err != nil {
		return nil, nil, err
	}
	jts := make([]joinedTable, len(items))
	var flatCols []columnInfo
	offset := 0
	var state joinDesugarState
	for i, it := range items {
		var tbl *resolvedTable
		var rows [][]Value
		var rowids []int64
		derived := it.Subquery != nil

		// A database qualifier must resolve locally (qualifierResolvesLocally,
		// schema_qualifier.go), else "no such table". A resolved one is stripped,
		// and a schema-qualified name never binds to a CTE.
		//
		// rp is the pager that owns this item's table: p, unless attachedReaders
		// (SetAttachedReaders, cross_db.go) routes the name to an attached
		// database. CTE, temp-catalog and derived-table resolution stay on p.
		schemaQualified := it.Schema != ""
		rp := p
		if !derived {
			if len(p.attachedReaders) > 0 {
				owner, oerr := p.itemOwner(it)
				if oerr != nil {
					return nil, nil, oerr
				}
				rp = owner
			} else if it.Schema != "" {
				ok, serr := p.qualifierResolvesLocally(it.Schema)
				if serr != nil {
					return nil, nil, serr
				}
				if !ok {
					return nil, nil, fmt.Errorf("engine: no such table: %s.%s", it.Schema, it.Table)
				}
			}
		}

		if derived {
			// A derived table: run its uncorrelated subquery once with a nil outer
			// scope (no LATERAL, as in C). The schema-only retry instead derives
			// the columns from the subquery's expression list without running it
			// (derivedSchemaCols, ReadOnlyPager.derivedSchemaOnly).
			cols, ok := p.derivedSchemaCols(it.Subquery)
			var subRows [][]Value
			if !ok {
				var err error
				cols, subRows, err = p.resolveDerivedRows(it.Subquery, params)
				if err != nil {
					return nil, nil, err
				}
			}
			tbl = &resolvedTable{cols: cols, ipkIndex: -1}
			rows = subRows
			rowids = make([]int64, len(subRows)) // parallel-length; unused (noRowid scope)
		} else if cteBind, ok := p.lookupCTE(it.Table); ok && !schemaQualified {
			// An in-scope CTE shadows a real table or view of the same name
			// (select.c:6028/6036); it desugars into the same derived-table row
			// source as a subquery or view (cte.go).
			// INDEXED BY on a CTE is "no such index": sqlite3IndexedByLookup walks
			// the resolved table's index chain (select.c:5487-5490), which a CTE
			// lacks. itemHasNoIndexChain enforces the same on the compiled path.
			if it.IndexedBy != "" {
				return nil, nil, fmt.Errorf("engine: no such index: %s", it.IndexedBy)
			}
			// A recursive CTE is resolved schema-only, by compiling its initial arm
			// (cteSchemaCols, vdbe_join_codegen.go): resolveFrom's callers only need
			// column metadata, and running the recursion here would be wasted work
			// (and unbounded for a non-terminating CTE). Its rows come from the
			// compiled queue program (compileRecursiveCTE).
			var cols []columnInfo
			var crows [][]Value
			var cerr error
			// Under the schema-only retry (resolveFromSchemaOnly) a plain CTE is
			// resolved by the same compile-only path: its columns, never its rows.
			if cteBind.recursive != nil || p.derivedSchemaOnly {
				cols, cerr = p.cteSchemaCols(it.Table, cteBind)
			} else {
				cols, crows, cerr = p.resolveCTERows(it.Table, cteBind, params)
			}
			if cerr != nil {
				return nil, nil, cerr
			}
			tbl = &resolvedTable{cols: cols, ipkIndex: -1}
			rows = crows
			rowids = make([]int64, len(crows)) // parallel-length; unused (noRowid scope)
			derived = true
		} else if isTempSchemaCatalogName(it.Table) && !schemaQualified {
			// sqlite_temp_master/sqlite_temp_schema: serve the filtered row
			// source temp_catalog.go builds, the same one the VDBE read path uses,
			// so main's rows are never reported under temp's name.
			if it.IndexedBy != "" {
				return nil, nil, fmt.Errorf("engine: unsupported: INDEXED BY on sqlite_temp_master")
			}
			crows, rerr := p.schemaCatalogRows(scopeTemp)
			if rerr != nil {
				return nil, nil, rerr
			}
			tbl = &resolvedTable{cols: sqliteSchemaCatalogColumns(), ipkIndex: -1}
			rows = crows
			rowids = make([]int64, len(crows)) // parallel-length; unused (noRowid scope)
			derived = true
		} else if isV, verr := rp.isVtabItem(it); verr != nil {
			return nil, nil, verr
		} else if isV {
			// A virtual table (table-valued function or persisted CREATE VIRTUAL
			// TABLE), intercepted before resolveTable since a persisted vtab's
			// schema row has type "table". It keeps a rowid pseudo-column, so
			// derived stays false. outer is nil: this resolver only produces
			// column scopes; execution goes through the VDBE's runVtabOnce.
			vcols, vrows, vrowids, verr := rp.materializeVtab(it, params, nil, nil, nil, nil)
			if verr != nil && errors.Is(verr, errVtabArgNotConstant) {
				// A table-valued function whose argument names the trigger row
				// (json_each(NEW.x) in a trigger body): only the columns are needed
				// here, so fall back to the module's declared columns rather than
				// failing on "no such table: NEW". Other errors, including a bad
				// argument count, still surface.
				if scols, serr := rp.vtabScopeColumns(it); serr == nil {
					vcols, vrows, vrowids, verr = scols, nil, nil, nil
				}
			}
			if verr != nil {
				return nil, nil, verr
			}
			tbl = &resolvedTable{cols: vcols, ipkIndex: -1}
			rows = vrows
			rowids = vrowids
		} else {
			t, terr := rp.resolveTableIn(fromItemScope(it), it.Table)
			if terr == nil {
				tbl = t
				if it.IndexedBy != "" {
					if err := rp.checkIndexExists(it.IndexedBy, it.Table, fromItemScope(it)); err != nil {
						return nil, nil, err
					}
				}
				seq, errFn := rp.ScanTable(tbl.root)
				for rowid, vals := range seq {
					rows = append(rows, normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rowid, vals))
					rowids = append(rowids, int64(rowid))
				}
				if err := errFn(); err != nil {
					return nil, nil, err
				}
			} else if cscope, isCat, cerr := rp.schemaCatalogSourceScope(it); cerr != nil {
				return nil, nil, cerr
			} else if isCat {
				// A schema catalog resolveTableIn refused (see
				// validateFromTablesExist, insert_write.go): serve temp_catalog.go's
				// filtered rows. Reached only after that failure, so a connection
				// with no temp object keeps the raw-scan path.
				if it.IndexedBy != "" {
					return nil, nil, fmt.Errorf("engine: unsupported: INDEXED BY on %s", it.Table)
				}
				crows, rerr := rp.schemaCatalogRows(cscope)
				if rerr != nil {
					return nil, nil, rerr
				}
				tbl = &resolvedTable{cols: sqliteSchemaCatalogColumns(), ipkIndex: -1}
				rows = crows
				rowids = make([]int64, len(crows)) // parallel-length; unused (noRowid scope)
				derived = true
			} else {
				// Not a table: try a view (view.go), which desugars into the same
				// derived-table row source as an inline subquery, sourced from the
				// view's stored SELECT. resolveViewRowsGuarded guards view cycles;
				// viewOutputRows applies C's column renames and error-text fixups.
				// INDEXED BY on a view is "no such index", as in C.
				pcv, ok, verr := rp.resolveViewByNameIn(fromItemScope(it), it.Table)
				if verr != nil {
					return nil, nil, verr
				}
				if !ok {
					// A BARE reference to an eponymous virtual-table module
					// (vtab.go), resolved only AFTER an ordinary table and a
					// view both fail -- so a real table/view of the same name
					// always shadows the module (SQLite's eponymous rule).
					if isEponymousVtabName(it.Table) {
						vcols, vrows, vrowids, mverr := rp.materializeVtab(it, params, nil, nil, nil, nil)
						if mverr != nil {
							return nil, nil, mverr
						}
						tbl = &resolvedTable{cols: vcols, ipkIndex: -1}
						rows = vrows
						rowids = vrowids
					} else {
						return nil, nil, terr // preserve the original "no such table" error
					}
				} else {
					if it.IndexedBy != "" {
						// C SQLite's own wording: a view has no b-tree, so
						// the hint can never name a usable index, and it says
						// "no such index" even when an index of that name
						// exists on another table (indexedby.test).
						return nil, nil, fmt.Errorf("engine: no such index: %s", it.IndexedBy)
					}
					// &items[i], not &it: resolveViewRowsGuarded needs the AST node's stable
					// address to recognize a repeat ask about the same FromItem.
					// The schema-only retry takes a view's columns without running its body
					// (viewColumnInfos).
					var cols []columnInfo
					var vrows [][]Value
					var rerr error
					if p.derivedSchemaOnly {
						cols, rerr = rp.viewColumnInfos(it.Table, pcv)
					} else {
						cols, vrows, rerr = rp.resolveViewRowsGuarded(&items[i], it.Table, pcv, params)
					}
					if rerr != nil {
						return nil, nil, rerr
					}
					tbl = &resolvedTable{cols: cols, ipkIndex: -1}
					rows = vrows
					rowids = make([]int64, len(vrows)) // parallel-length; unused (noRowid scope)
					derived = true
				}
			}
		}

		name := it.Alias
		if name == "" {
			name = it.Table // "" for a derived table with no alias
		}
		// tableName is the REAL underlying table name (it.Table), used only for
		// full_column_names result naming (tableScope.tableName). For a derived
		// table (it.Table == "") it falls back to the alias (name), which is how
		// SQLite qualifies a derived-table column; "" only for an unaliased
		// derived table, where full_column_names naming declines downstream.
		tableName := it.Table
		if tableName == "" {
			tableName = name
		}
		onExpr, hidden, derr := desugarJoinItem(&state, i, it, name, tbl.cols)
		if derr != nil {
			return nil, nil, derr
		}
		jts[i] = joinedTable{
			name:             name,
			tableName:        tableName,
			tbl:              tbl,
			rows:             rows,
			rowids:           rowids,
			offset:           offset,
			left:             it.Join == JoinLeft || it.Join == JoinFull,
			rightOuter:       it.Join == JoinRight || it.Join == JoinFull,
			on:               onExpr,
			coalesced:        hidden,
			derived:          derived,
			groupLen:         it.GroupLen,
			origIdx:          i,
			groupSpan:        1,
			nestedNonLeading: it.NestedNonLeading,
		}
		// If i is an internal (non-connector) member of some enclosing
		// parenthesized join GROUP, its onExpr/hidden -- just computed
		// above by desugarJoinItem's single, group-unaware flat walk --
		// may reach OUTSIDE that group's own bounds; fix that up BEFORE
		// installCoalesceFallback (right below) ever reads hidden, so it
		// never installs a fallback on the wrong (too-far-outside, and at
		// this point not-yet-bound) table in the first place -- see
		// fixItemGroupScoping's doc comment.
		fixedHidden, ferr := fixItemGroupScoping(jts, i, hidden)
		if ferr != nil {
			return nil, nil, ferr
		}
		jts[i].coalesced = fixedHidden
		if err := installCoalesceFallback(jts, i, it.Join, fixedHidden); err != nil {
			return nil, nil, err
		}
		flatCols = append(flatCols, tbl.cols...)
		offset += len(tbl.cols)
	}
	// A group-starting item that acquired its OWN coalesceFallback from a
	// LATER item WITHIN its own group (e.g. "t2 RIGHT JOIN t3 USING(a)": t3
	// installs its fallback onto t2, its own group's representative) needs
	// its OWN CONNECTOR condition's "own side" read (t2.a in "t1.a=t2.a", the
	// group's connector to whatever precedes it) to consult that fallback
	// too -- see markGroupConnectorCoalesceAware's doc comment for exactly
	// why and why the ordinary UsingReprOwnItem ordering guard doesn't apply
	// here.
	for i := range jts {
		if jts[i].groupLen > 1 && jts[i].coalesced != nil && jts[i].coalesceFallback != nil && jts[i].on != nil {
			jts[i].on = markGroupConnectorCoalesceAware(jts[i].on, jts[i].name, jts[i].coalesceFallback)
		}
	}
	// No RIGHT/FULL shape validation here -- see this file's package doc
	// comment (RIGHT/FULL section): the VDBE codegen (vdbe_join_codegen.go's
	// emitJoinLoops/emitRightOuterSweep) handles any placement or count of
	// RIGHT/FULL items in a flat FROM clause -- no narrower gate remains.
	return jts, flatCols, nil
}

// groupConnectorCoalesceSentinel is the ColumnExpr.UsingReprOwnItem for a
// group connector's own-side read marked by markGroupConnectorCoalesceAware.
// Being larger than any real item index, it makes resolveColumnEx's
// "ownerIdx < usingReprOwnItem" guard always true, which is safe because the
// group is fully resolved before its connector is evaluated.
const groupConnectorCoalesceSentinel = 1 << 30

// markGroupConnectorCoalesceAware returns e (a group connector's
// USING/NATURAL condition, built only from ColumnExpr/BinaryExpr) with every
// own-side ColumnExpr qualified with ownName whose name is a key of fallback
// switched to UsingRepr with UsingReprOwnItem = groupConnectorCoalesceSentinel,
// so resolveColumnEx's RIGHT/FULL coalesce fallback applies to it.
//
// In "t1 JOIN (t2 RIGHT JOIN t3 USING(a)) USING(a)" the connector reads t2.a,
// which is NULL for t3's unmatched row although the group has a real "a" via
// t3. C's USING coalescing exposes t3's value; without this the outer join
// lost that row (joinB.test). The ordering guard is bypassed because the group
// is fully resolved before its connector runs.
func markGroupConnectorCoalesceAware(e Expr, ownName string, fallback map[string][]int) Expr {
	switch x := e.(type) {
	case ColumnExpr:
		if !x.UsingRepr && equalFoldName(x.Qualifier, ownName) {
			if _, ok := fallback[r33sFoldIdent(x.Name)]; ok {
				x.UsingRepr = true
				x.UsingReprOwnItem = groupConnectorCoalesceSentinel
			}
		}
		return x
	case BinaryExpr:
		x.L = markGroupConnectorCoalesceAware(x.L, ownName, fallback)
		x.R = markGroupConnectorCoalesceAware(x.R, ownName, fallback)
		return x
	default:
		return e
	}
}

// resolveDerivedRows executes a derived table's uncorrelated subquery once
// with a nil outer scope (no LATERAL) and returns its column metadata and rows.
// Column names come from the subquery's result columns; affinities are
// best-effort (derivedColumnAffinities). A compound's columns carry the
// leftmost arm's collation, as in SQLite (derivedColumnCollations).
func (p *ReadOnlyPager) resolveDerivedRows(sub *SelectStmt, params []Value) ([]columnInfo, [][]Value, error) {
	subCols, subRows, err := p.execSelect(sub, nil, params, "")
	if err != nil {
		return nil, nil, err
	}
	// derivedColumnInfos re-derives each output column's origin from the AST,
	// so it must run inside the subquery's OWN WITH -- selectExpander binds a
	// result expression to its source exactly once, and it does so between
	// sqlite3WithPush(p->pWith) (select.c:6000) and the matching pop. Without
	// the push a CTE that shadows a real table is invisible HERE only, so the
	// rows come from the CTE while the column metadata (declared type,
	// collation, origin table) comes from the shadowed base table.
	popSubCTEs := func() {}
	if len(sub.CTEs) > 0 {
		popSubCTEs = p.pushCTEScope(sub.CTEs)
	}
	cols := p.derivedColumnInfos(sub, subCols)
	popSubCTEs()
	for _, r := range subRows {
		if len(r) != len(cols) {
			return nil, nil, fmt.Errorf("engine: internal: derived table row has %d values, expected %d", len(r), len(cols))
		}
	}
	return cols, subRows, nil
}

// checkDerivedJoinSupported declines NATURAL/USING with an unaliased derived
// table when the FROM also contains a parenthesized join group
// (FromItem.GroupLen; see checkFlattenSafe, sql_parser.go).
//
// An unaliased derived table has no scope name, so its side of the desugared
// condition is addressed by FROM-item index (ColumnExpr.UsingPinned). That
// index is assigned in the group's own span-local resolveJoinSources call and
// is consistent when the group is the sole top-level item with no nested
// sub-group. Nesting breaks it: nested groups rebase owner chains by their own
// colBases, but UsingPinnedItem is not rebased, e.g.
//
//	SELECT * FROM (t0_a RIGHT JOIN (t0_c RIGHT JOIN (SELECT * FROM t2
//	  LEFT JOIN t0_b) USING (c0)) USING (c0))
//
// returned the right values under the wrong column name ("c0:1" vs "c0:3").
// A sibling top-level item risks a spurious "ambiguous column name"; also
// declined.
//
// Aliased derived tables, views and CTEs are addressed by name and compose
// (compat-harness/derived_natural_join_diff_test.go).
func (p *ReadOnlyPager) checkDerivedJoinSupported(items []FromItem) error {
	hasUsing, hasGroup := false, false
	for _, it := range items {
		if it.Natural || it.Using != nil {
			hasUsing = true
		}
		// See checkSelfJoinGroupSupported's identical widening, just above,
		// for why NestedGroupSpan != nil is included defensively here too.
		if it.GroupLen > 0 || it.NestedGroupSpan != nil {
			hasGroup = true
		}
	}
	if !hasUsing || !hasGroup {
		return nil
	}
	hasDerived := false
	for _, it := range items {
		// fromItemIsDerived (view.go) itself now treats a NestedGroupSpan
		// connector as derived -- conservatively, regardless of what its own
		// nested span actually contains -- so this scan does not need to
		// look inside it separately.
		if p.fromItemIsDerived(it) {
			hasDerived = true
			break
		}
	}
	if !hasDerived {
		return nil
	}
	// Safe carve-out: the group spans the whole FROM and no member starts a
	// nested group. A NestedGroupSpan connector at items[0] is excluded: it
	// wraps a span this function never inspected.
	if len(items) > 0 && items[0].GroupLen == len(items) && items[0].NestedGroupSpan == nil {
		nested := false
		for _, it := range items[1:] {
			if it.GroupLen > 0 || it.NestedGroupSpan != nil {
				nested = true
				break
			}
		}
		if !nested {
			return nil
		}
	}
	return fmt.Errorf("engine: unsupported: NATURAL/USING join combined with a derived table inside a parenthesized join group")
}

// derivedSelectScope is the one resolution of a subquery's FROM clause and
// expanded select list that derivedColumnInfos' per-column derivations share.
// Resolving it once per derivation made nested CTEs cost 3^depth.
type derivedSelectScope struct {
	ctx     *evalCtx
	outCols []outputColumn
	ok      bool // false when the shape could not be resolved -- every caller then keeps its own default
}

func (p *ReadOnlyPager) derivedSelectScope(sub *SelectStmt, n int) derivedSelectScope {
	if sub == nil || len(sub.From) == 0 {
		return derivedSelectScope{}
	}
	jts, _, err := p.resolveFrom(sub.From, nil)
	if err != nil {
		return derivedSelectScope{}
	}
	scopes := buildScopes(jts)
	outCols, err := expandSelectList(sub.Columns, scopes, defaultColNameMode)
	if err != nil || len(outCols) != n {
		return derivedSelectScope{}
	}
	return derivedSelectScope{ctx: &evalCtx{tables: scopes}, outCols: outCols, ok: true}
}

// derivedColumnInfos builds a derived table's []columnInfo from its subquery's
// result-column names and its best-effort per-column affinities
// (derivedColumnAffinities). Shared by both resolvers (resolveFrom here,
// resolveJoinSources in vdbe_join_codegen.go) so the derived table's column
// namespace is identical in each.
func (p *ReadOnlyPager) derivedColumnInfos(sub *SelectStmt, names []string) []columnInfo {
	names = p.subqueryColumnNames(sub, names)
	// A repeated name is ":N"-renamed here, as sqlite3ColumnsFromExprList
	// does. Past the fifth repeat SQLite uses sqlite3_randomness
	// (select.c:2308-2310); r32mFinishUniqueColumnNames keeps counting
	// deterministically instead (compat-harness normalizes this with
	// tclNormalizeRandomColumnSuffix).
	names = r32mFinishUniqueColumnNames(names)
	ds := p.derivedSelectScope(sub, len(names))
	affs := p.derivedColumnAffinities(sub, len(names), ds)
	colls := derivedColumnCollations(len(names), ds)
	computed := derivedColumnComputed(len(names), ds)
	// A COMPOUND column's affinity is folded across its arms
	// (sqlite3SubqueryColumnTypes, select.c:2377-2385): it is AFF_NONE -- this
	// package's NoAffinity -- only when NO arm gives it one, so a leftmost
	// literal followed by a column arm is not computed at all, and a leftmost
	// computed expression followed by a column is not either. The leftmost arm
	// alone (derivedColumnComputed) answered both wrong: "a IN c" over
	// "c(x) AS (SELECT 123 UNION SELECT b FROM x2)" (view.test 31.1).
	if sub != nil && len(sub.Compound) > 0 {
		if types, ok := p.compoundColumnTypes(sub, false); ok && len(types) == len(names) {
			for i, t := range types {
				computed[i] = t.aff == affNone && !t.blob
			}
		}
	}
	cols := make([]columnInfo, len(names))
	for i := range names {
		cols[i] = columnInfo{Name: names[i], Aff: affs[i], Collation: colls[i], NoAffinity: computed[i]}
	}
	return cols
}

// resolveFromSchemaOnly is resolveFrom with derived tables' columns taken from
// their expression lists rather than by running them, as sqlite3ExpandSubquery
// (select.c:5885) does. It is only a retry: its one caller
// (resolveArmOutputsOuter, sql_compound.go) uses it after resolveFrom fails.
func (p *ReadOnlyPager) resolveFromSchemaOnly(items []FromItem) ([]joinedTable, error) {
	saved := p.derivedSchemaOnly
	p.derivedSchemaOnly = true
	defer func() { p.derivedSchemaOnly = saved }()
	jts, _, err := p.resolveFrom(items, nil)
	if err != nil {
		return nil, err
	}
	return jts, nil
}

// derivedSchemaCols builds a FROM subquery's column list without running it,
// or returns (nil, false) when the schema-only retry is inactive or the body
// does not compile.
//
// Names come from the body's compiled program, matching
// sqlite3ColumnsFromExprList over the leftmost arm (select.c:5901-5902);
// derivedColumnInfos adds affinity/collation signals. Like cteSchemaCols, it
// pushes the body's own CTE scope first, since derivedColumnInfos re-resolves
// the body's FROM.
func (p *ReadOnlyPager) derivedSchemaCols(sub *SelectStmt) ([]columnInfo, bool) {
	if !p.derivedSchemaOnly || sub == nil {
		return nil, false
	}
	prog, err := compileSubProgram(p, sub, nil)
	if err != nil || prog == nil {
		return nil, false
	}
	popSubCTEs := func() {}
	if len(sub.CTEs) > 0 {
		popSubCTEs = p.pushCTEScope(sub.CTEs)
	}
	cols := p.derivedColumnInfos(sub, prog.ColNames)
	popSubCTEs()
	return cols, true
}

// subqueryColumnNames corrects an inline derived table's (or CTE's) column
// names. sqlite3ColumnsFromExprList peels COLLATE from an unaliased item before
// testing for a column reference; generateColumnNames does not:
//
//	SELECT g COLLATE nocase FROM t9                  named "g COLLATE nocase"
//	SELECT * FROM (SELECT g COLLATE nocase FROM t9)  named "g"
//
// Only COLLATE: selectExpander builds an inline subquery's list before
// resolution, so likely() is still a function and "oid" a bare identifier. A
// view's list is built after resolution (sqlite3ResultSetOfSelect) and peels
// both (colNameMode.subqueryCols):
//
//	CREATE VIEW w AS SELECT unlikely(h) FROM t9; SELECT * FROM w   -> "h"
//	SELECT * FROM (SELECT unlikely(h) FROM t9)                     -> "unlikely(h)"
//	CREATE VIEW w AS SELECT oid FROM k; PRAGMA table_info(w)       -> "id"
//	SELECT * FROM (SELECT oid FROM k)                              -> "oid"
//
// Anything this cannot analyze leaves names untouched.
func (p *ReadOnlyPager) subqueryColumnNames(sub *SelectStmt, names []string) []string {
	if sub == nil || len(names) == 0 || len(sub.Columns) != len(names) {
		return names
	}
	stripped := make([]SelectColumn, len(sub.Columns))
	copy(stripped, sub.Columns)
	peeledAny := false
	for i := range stripped {
		// A "*" item expands to several outputs, so one peeled item would
		// shift every later position: bail rather than misalign. A star never
		// needs the peel anyway (it is already a bare column name).
		if stripped[i].Star {
			return names
		}
		if stripped[i].HasAlias {
			continue
		}
		if e, ok := skipCollateOnly(stripped[i].Expr); ok {
			stripped[i].Expr = e
			peeledAny = true
		}
	}
	if !peeledAny {
		return names
	}
	var scopes []tableScope
	if len(sub.From) > 0 {
		jts, _, err := p.resolveFrom(sub.From, nil)
		if err != nil {
			return names
		}
		scopes = buildScopes(jts)
	}
	// colNameMode's default (not defaultColNameMode): the peel is already
	// applied above, and this must otherwise name exactly like the result set
	// it is correcting.
	peeled, perr := expandSelectList(stripped, scopes, colNameMode{short: true})
	plain, verr := expandSelectList(sub.Columns, scopes, colNameMode{short: true})
	if perr != nil || verr != nil || len(peeled) != len(names) || len(plain) != len(names) {
		return names
	}
	var out []string
	for i := range names {
		// Correct a name ONLY while it is still the raw result-set one. A
		// caller that already applied the post-resolution rule -- a view's own
		// column list (view.go, view_trigger.go), which peels likely() and
		// resolves a rowid alias too -- has a name this must not undo.
		if names[i] != plain[i].name || plain[i].name == peeled[i].name {
			continue
		}
		if out == nil {
			out = append(out, names...)
		}
		out[i] = peeled[i].name
	}
	if out == nil {
		return names
	}
	return out
}

// skipCollateOnly peels every COLLATE wrapper off e, reporting whether there
// was one. See subqueryColumnNames for why likely()/unlikely()/likelihood()
// are deliberately NOT peeled with it here.
func skipCollateOnly(e Expr) (Expr, bool) {
	peeled := false
	for {
		c, isColl := e.(CollateExpr)
		if !isColl {
			return e, peeled
		}
		e, peeled = c.X, true
	}
}

// derivedColumnComputed reports, per output column, whether its defining

// derivedColumnComputed reports, per output column, whether its expression is
// computed rather than a column/CAST reference: SQLite's AFF_NONE vs AFF_BLOB,
// which decides whether the column defends its storage class in a comparison
// (columnInfo.NoAffinity). It reuses armColClassify's stop bit. An
// unclassifiable shape leaves false.
func derivedColumnComputed(n int, ds derivedSelectScope) []bool {
	out := make([]bool, n)
	if !ds.ok {
		return out
	}
	for i, oc := range ds.outCols {
		if _, stop, _, ok := armColClassify(ds.ctx, oc.expr); ok && !stop {
			out[i] = true
		}
	}
	return out
}

// derivedColumnCollations gives each derived table's (or view's) output column
// the collation of its defining expression, so a bare reference to a
// NOCASE column keeps NOCASE in the outer query:
//
//	SELECT * FROM (SELECT a AS z FROM t4) WHERE z='THIS'   -- t4.a NOCASE
//
// topExprCollation is SQLite's rule (explicit COLLATE wins, a bare column
// gives its declared collation, else none). It never errors; an unanalyzable
// shape leaves "" (BINARY).
func derivedColumnCollations(n int, ds derivedSelectScope) []string {
	colls := make([]string, n)
	if !ds.ok {
		return colls
	}
	for i, oc := range ds.outCols {
		if name, ok := topExprCollation(ds.ctx, oc.expr); ok {
			colls[i] = name
		}
	}
	return colls
}

// derivedColumnAffinities computes a derived table's per-column affinity at
// plan time: a column reference gets its source's affinity, a computed
// expression none. Any unanalyzable shape gives affNone for every column, which
// only ever suppresses a coercion. For a compound, sub.Columns/sub.From are the
// first arm's, which is SQLite's leftmost-arm rule.
func (p *ReadOnlyPager) derivedColumnAffinities(sub *SelectStmt, n int, ds derivedSelectScope) []affinity {
	affs := make([]affinity, n) // affNone (zero value) for every column by default
	if sub == nil {
		return affs
	}
	// A COMPOUND SELECT's output-column affinity is NOT the first arm's alone:
	// SQLite folds every arm's affinity together by a defined rule (see
	// compoundOutputAffinities). Reproduce it faithfully when analyzable; a
	// shape that isn't falls through to the first-arm best effort below (an
	// unanalyzable compound VIEW whose arms disagree is separately declined by
	// declineIfCompoundAffinityUnreproducible, view.go).
	if len(sub.Compound) > 0 {
		if caffs, ok := p.compoundOutputAffinities(sub); ok && len(caffs) == n {
			return caffs
		}
	}
	if !ds.ok {
		return affs
	}
	for i, oc := range ds.outCols {
		affs[i] = exprAffinity(ds.ctx, oc.expr)
	}
	return affs
}

// armColSig carries, for one compound-arm output column, what
// sqlite3SubqueryColumnTypes needs to fold its affinity across arms:
//
//   - aff: the expression's affinity (affNone also for a BLOB column, hence
//     stop);
//   - stop: whether the left-to-right scan stops here: true for a column or
//     CAST (affinity > AFF_NONE, including AFF_BLOB), false for a literal or
//     computed value;
//   - dt: the sqlite3ExprDataType bitmask (0x01 numeric, 0x02 text, 0x04
//     blob), used to drop a TEXT/numeric affinity on a conflicting class;
//   - cast: whether the unpeeled node is literally a CAST ("p->op==TK_CAST");
//     only arm 0's matters, to raise numeric to SQLITE_AFF_FLEXNUM.
type armColSig struct {
	aff  affinity
	stop bool
	dt   int
	cast bool
}

// compoundOutputAffinities computes each output column's affinity for a
// compound SELECT by sqlite3SubqueryColumnTypes' rule: the first arm with a
// real affinity wins, unless it is TEXT and another arm may hold a number (or
// numeric and another may hold text), in which case there is none. ok is false
// for a shape it cannot analyze (unresolvable FROM, column-count mismatch,
// unclassifiable expression).
func (p *ReadOnlyPager) compoundOutputAffinities(sub *SelectStmt) ([]affinity, bool) {
	types, ok := p.compoundColumnTypes(sub, false)
	if !ok {
		return nil, false
	}
	out := make([]affinity, len(types))
	for i, t := range types {
		out[i] = t.aff
	}
	return out, true
}

// compoundColumnType is one compound output column's folded type, as
// sqlite3SubqueryColumnTypes leaves it. aff is this package's affinity, in
// which SQLite's AFF_NONE and AFF_BLOB are both affNone -- blob and flexnum
// carry the two bits that distinction and SQLITE_AFF_FLEXNUM would otherwise
// lose. Neither affects a COERCION (AFF_BLOB and AFF_NONE both leave a value
// alone; FLEXNUM applies numeric affinity exactly like AFF_NUMERIC), which is
// why compoundOutputAffinities can ignore both -- but both are visible in the
// declared TYPE PRAGMA table_info reports for a view (pragmaViewInfo).
type compoundColumnType struct {
	aff     affinity
	blob    bool // SQLite's AFF_BLOB rather than AFF_NONE
	flexnum bool // SQLITE_AFF_FLEXNUM: a surviving numeric affinity whose arm 0 is a CAST
}

// compoundColumnTypes folds every output column of a COMPOUND SELECT through
// sqlite3SubqueryColumnTypes. defaultBlob is that routine's own "char aff"
// parameter: SQLITE_AFF_BLOB for CREATE TABLE ... AS SELECT, SQLITE_AFF_NONE
// (false) for a view or any other FROM-clause subquery. ok is false for any
// shape this can't faithfully analyze -- see compoundArmSignals.
func (p *ReadOnlyPager) compoundColumnTypes(sub *SelectStmt, defaultBlob bool) ([]compoundColumnType, bool) {
	sigs, ok := p.compoundArmSignals(sub)
	if !ok {
		return nil, false
	}
	out := make([]compoundColumnType, len(sigs[0]))
	for i := range out {
		out[i] = foldCompoundColumnType(sigs, i, defaultBlob)
	}
	return out, true
}

// compoundArmSignals returns one armColSig row per LOGICAL compound arm of
// sub -- which is NOT one per Compound entry once a multi-row VALUES clause is
// involved: sqlite3MultiValues folds such a clause into a single arm (see
// valuesFold, sql_ast.go), so "SELECT g FROM t2 UNION ALL VALUES('x'),('z')"
// has two arms here even though the parser stored three cores.
func (p *ReadOnlyPager) compoundArmSignals(sub *SelectStmt) ([][]armColSig, bool) {
	var sigs [][]armColSig
	add := func(s []armColSig, ok bool) bool {
		if !ok || len(s) == 0 || (len(sigs) > 0 && len(s) != len(sigs[0])) {
			return false
		}
		sigs = append(sigs, s)
		return true
	}
	rest := sub.Compound
	if sub.ValuesArms > 0 {
		rows, tail, ok := multiRowValuesRows(sub)
		if !ok {
			return nil, false
		}
		rest = tail
		switch sub.ValuesFold {
		case valuesFoldCoroutine:
			if !add(coroutineValuesSig(len(sub.Columns)), true) {
				return nil, false
			}
		case valuesFoldUnionAll:
			// In the LEADING (or bare) position the fallback's arms splice
			// straight into the enclosing chain: parse.y only wraps a compound
			// RIGHT operand in a derived table.
			for _, r := range rows {
				if !add(p.armColumnSignals(r)) {
					return nil, false
				}
			}
		default:
			return nil, false // valuesFoldUnanalyzable
		}
	} else if !add(p.armColumnSignals(sub)) {
		return nil, false
	}
	for _, arm := range rest {
		if arm.Stmt == nil || !add(p.compoundArmSignal(arm.Stmt)) {
			return nil, false
		}
	}
	return sigs, true
}

// multiRowValuesRows splits a core that starts a multi-row VALUES clause into
// the cores holding its rows and the ordinary compound arms written after it.
func multiRowValuesRows(head *SelectStmt) (rows []*SelectStmt, rest []CompoundArm, ok bool) {
	if head.ValuesArms <= 0 || head.ValuesArms > len(head.Compound) {
		return nil, nil, false
	}
	rows = append(rows, head)
	for _, a := range head.Compound[:head.ValuesArms] {
		if a.Stmt == nil {
			return nil, nil, false
		}
		rows = append(rows, a.Stmt)
	}
	return rows, head.Compound[head.ValuesArms:], true
}

// coroutineValuesSig is what a co-routine-collapsed multi-row VALUES clause
// contributes as ONE arm: SQLite reads it as a TK_COLUMN over an AFF_NONE
// pseudo-table, so it has no affinity, does not stop the left-to-right scan,
// and constrains no storage class.
func coroutineValuesSig(n int) []armColSig {
	s := make([]armColSig, n)
	for i := range s {
		s[i] = armColSig{aff: affNone, dt: exprDataAny}
	}
	return s
}

// compoundArmSignal is armColumnSignals for an arm to the right of a compound
// operator. A multi-row VALUES there is one arm even when sqlite3MultiValues
// fell back to UNION ALL, because parse.y wraps a compound right operand in a
// derived table, so the arm is a column over that subquery's folded type. Over
// t2(f NUMERIC, g VARCHAR(9)):
//
//	CREATE VIEW v AS SELECT g FROM t2 UNION ALL VALUES('x'),('z')
//	  -> "BLOB": the wrapped rows fold to AFF_NONE, whose 0x07 data type
//	     demotes g's TEXT.
//	CREATE VIEW v AS SELECT g FROM t2 UNION ALL VALUES(CAST('x' AS TEXT)),('z')
//	  -> "VARCHAR(9)".
func (p *ReadOnlyPager) compoundArmSignal(core *SelectStmt) ([]armColSig, bool) {
	if core.ValuesArms == 0 {
		return p.armColumnSignals(core)
	}
	rows, rest, ok := multiRowValuesRows(core)
	if !ok || len(rest) != 0 {
		return nil, false // a compound arm never carries arms of its own
	}
	switch core.ValuesFold {
	case valuesFoldCoroutine:
		return coroutineValuesSig(len(core.Columns)), true
	case valuesFoldUnionAll:
		inner := make([][]armColSig, 0, len(rows))
		for _, r := range rows {
			s, ok := p.armColumnSignals(r)
			if !ok || len(s) == 0 || (len(inner) > 0 && len(s) != len(inner[0])) {
				return nil, false
			}
			inner = append(inner, s)
		}
		out := make([]armColSig, len(inner[0]))
		for i := range out {
			// A FROM-clause subquery's own default affinity is AFF_NONE.
			out[i] = derivedColumnSig(foldCompoundColumnType(inner, i, false))
		}
		return out, true
	}
	return nil, false // valuesFoldUnanalyzable
}

// derivedColumnSig is the armColSig a TK_COLUMN over a subquery column of the
// given folded type contributes: sqlite3ExprAffinity reports the column's own
// affinity (so the scan stops unless that is AFF_NONE), and
// sqlite3ExprDataType's TK_COLUMN case derives the storage classes from it.
func derivedColumnSig(t compoundColumnType) armColSig {
	dt := exprDataAny
	switch {
	case t.aff >= affNumeric:
		dt = exprDataNumeric | exprDataBlob
	case t.aff == affText:
		dt = exprDataText | exprDataBlob
	}
	return armColSig{aff: t.aff, stop: t.blob || t.aff != affNone, dt: dt}
}

// armColumnSignals resolves one select-core's FROM into scopes, expands its
// select list, and returns each output column's armColSig -- exactly the
// per-arm computation compoundOutputAffinities folds together. ok is false for
// any shape it can't analyze (an unresolvable FROM, an un-expandable select
// list, or an output expression armColClassify can't classify), matching
// derivedColumnAffinities' own "never error, just decline to analyze" contract.
func (p *ReadOnlyPager) armColumnSignals(core *SelectStmt) ([]armColSig, bool) {
	if core == nil {
		return nil, false
	}
	var scopes []tableScope
	if len(core.From) > 0 {
		jts, _, err := p.resolveFrom(core.From, nil)
		if err != nil {
			return nil, false
		}
		scopes = buildScopes(jts)
	}
	outCols, err := expandSelectList(core.Columns, scopes, defaultColNameMode)
	if err != nil {
		return nil, false
	}
	ctx := &evalCtx{tables: scopes, pager: p} // pager: a scalar-subquery arm resolves through it
	sigs := make([]armColSig, len(outCols))
	for i, oc := range outCols {
		aff, stop, dt, ok := armColClassify(ctx, oc.expr)
		if !ok {
			return nil, false
		}
		_, isCast := oc.expr.(CastExpr) // unpeeled: SQLite tests p->op==TK_CAST
		sigs[i] = armColSig{aff: aff, stop: stop, dt: dt, cast: isCast}
	}
	return sigs, true
}

// foldCompoundColumnType folds column i's per-arm signals into the type
// sqlite3SubqueryColumnTypes assigns: phase one takes the first arm with a
// real affinity (stop bit set), phase two drops a TEXT or numeric affinity to
// AFF_BLOB if another arm's storage class conflicts.
//
// defaultBlob is that routine's "aff" parameter, used when no arm has an
// affinity: SQLITE_AFF_BLOB for CREATE TABLE AS, SQLITE_AFF_NONE for a view or
// FROM subquery.
func foldCompoundColumnType(sigs [][]armColSig, i int, defaultBlob bool) compoundColumnType {
	numArms := len(sigs)
	aff := sigs[0][i].aff
	stop := sigs[0][i].stop
	m := 0
	j := 0
	for !stop && j < numArms-1 {
		m |= sigs[j][i].dt
		j++
		aff = sigs[j][i].aff
		stop = sigs[j][i].stop
	}
	if !stop {
		// No arm carried an affinity at all, so the caller's default applies.
		return compoundColumnType{aff: affNone, blob: defaultBlob}
	}
	if aff == affNone {
		// The deciding arm stopped the scan without an affinity of its own,
		// which in SQLite is AFF_BLOB (a typeless COLUMN, or a CAST to one) --
		// strictly greater than AFF_NONE, so the default does NOT apply and
		// phase two, which needs AFF_TEXT or better, does not run either.
		return compoundColumnType{aff: affNone, blob: true}
	}
	// Phase two, under SQLite's own "(pS2->pNext || pS2!=pSelect)" guard --
	// always true for a written compound, false for the single arm a
	// co-routine-collapsed multi-row VALUES can leave behind.
	if j == 0 && j+1 >= numArms {
		return compoundColumnType{aff: aff}
	}
	for k := j + 1; k < numArms; k++ {
		m |= sigs[k][i].dt
	}
	if aff == affText && m&exprDataNumeric != 0 {
		return compoundColumnType{aff: affNone, blob: true} // TEXT, but some other arm may hold a number
	}
	if aff >= affNumeric && m&exprDataText != 0 {
		return compoundColumnType{aff: affNone, blob: true} // numeric, but some other arm may hold text
	}
	if aff >= affNumeric && sigs[0][i].cast {
		// SQLITE_AFF_FLEXNUM. It coerces exactly like AFF_NUMERIC; the only
		// visible difference is that a view reports it as "NUM" even when the
		// surviving affinity was INTEGER or REAL.
		return compoundColumnType{aff: aff, flexnum: true}
	}
	return compoundColumnType{aff: aff}
}

// armColClassify returns the affinity, scan-stop flag, and storage-class
// data-type bitmask SQLite's sqlite3ExprAffinity / sqlite3ExprDataType derive
// from one arm output expression (see armColSig). ok is false for an expression
// shape it can't faithfully classify -- a scalar subquery, whose SQLite
// affinity and data type would require resolving the subquery's own first
// output column -- so the whole compound is treated as unanalyzable rather than
// guessed at.
func armColClassify(ctx *evalCtx, e Expr) (aff affinity, stop bool, dt int, ok bool) {
	// sqlite3ExprAffinity peels only COLLATE; sqlite3ExprDataType also peels
	// TK_UPLUS. So unary "+" hides its operand's affinity but not its storage
	// class:
	//
	//	CREATE VIEW v AS SELECT +g FROM t2 UNION ALL SELECT 44;  -- type ""
	//
	// Peel COLLATE for both; peel unary "+" for the data type only.
	for {
		if c, isColl := e.(CollateExpr); isColl {
			e = c.X
			continue
		}
		break
	}
	if u, isUnary := e.(UnaryExpr); isUnary && u.Op == "+" {
		_, _, d, ok := armColClassify(ctx, u.X)
		return affNone, false, d, ok
	}
	switch x := e.(type) {
	case ColumnExpr, CastExpr:
		// A CAST, and a reference to a real table's column, always has a
		// SQLite affinity > AFF_NONE (even a no-type column is AFF_BLOB), so
		// the affinity scan stops here. exprAffinity reports affNone for the
		// BLOB case, which folds to "no affinity" exactly as SQLite's own rule
		// does. A DERIVED table's column is the exception: one whose defining
		// expression was computed really is AFF_NONE, and does NOT stop the
		// scan -- columnInfo.NoAffinity is exactly that bit.
		a := exprAffinity(ctx, e)
		stop := true
		if col, isCol := e.(ColumnExpr); isCol && a == affNone {
			if _, _, ci, _, err := resolveColumn(ctx, col.Qualifier, col.Name); err == nil && ci != nil && ci.NoAffinity {
				stop = false
			}
		}
		d := 0x07
		if a >= affNumeric {
			d = 0x05
		} else if a == affText {
			d = 0x06
		}
		return a, stop, d, true
	case LiteralExpr:
		switch x.Val.Typ {
		case Null:
			return affNone, false, 0x00, true
		case Text:
			return affNone, false, 0x02, true
		case Blob:
			return affNone, false, 0x04, true
		default: // Int, Float
			return affNone, false, 0x01, true
		}
	case BinaryExpr:
		if x.Op == "||" {
			return affNone, false, 0x06, true // TK_CONCAT: text or blob
		}
		return affNone, false, 0x01, true // arithmetic/comparison/logical: numeric
	case FuncExpr, ParamExpr:
		return affNone, false, 0x07, true // any storage class
	case CaseExpr:
		d, cok := caseDataType(ctx, x)
		if !cok {
			return affNone, false, 0, false
		}
		return affNone, false, d, true
	case SubqueryExpr:
		// sqlite3ExprAffinity's TK_SELECT case recurses into the subquery's
		// own first result expression -- of pExpr->x.pSelect, which for a
		// COMPOUND subquery is the TOP of the pPrior chain, i.e. the RIGHTMOST
		// arm, NOT the folded type derivedColumnInfos computes. Only the
		// non-compound case is safe to answer here; a compound one stays a
		// gray zone rather than a guess in the wrong direction.
		if x.Stmt == nil || len(x.Stmt.Compound) != 0 {
			return affNone, false, 0, false
		}
		ci, ok := scalarSubqueryColumn(ctx, x.Stmt)
		if !ok {
			return affNone, false, 0, false
		}
		d := 0x07
		if ci.Aff >= affNumeric {
			d = 0x05
		} else if ci.Aff == affText {
			d = 0x06
		}
		return ci.Aff, !ci.NoAffinity, d, true
	default:
		// IS/IN/BETWEEN/LIKE/GLOB/IS NULL/EXISTS/unary-minus/... -- SQLite's
		// sqlite3ExprDataType default: numeric storage class, no affinity.
		return affNone, false, 0x01, true
	}
}

// caseDataType returns the storage-class bitmask SQLite's sqlite3ExprDataType
// assigns a CASE expression: the OR of its THEN branches' bitmasks and its ELSE
// branch's (the WHEN conditions do not contribute). ok is false if any branch
// is itself unclassifiable.
func caseDataType(ctx *evalCtx, x CaseExpr) (int, bool) {
	res := 0
	for _, w := range x.Whens {
		_, _, d, ok := armColClassify(ctx, w.Then)
		if !ok {
			return 0, false
		}
		res |= d
	}
	if x.Else != nil {
		_, _, d, ok := armColClassify(ctx, x.Else)
		if !ok {
			return 0, false
		}
		res |= d
	}
	return res, true
}

// installCoalesceFallback records, on each of jts[idx]'s USING/NATURAL
// representatives (earlier items, from hidden), that idx is a fallback source
// for that column when idx is a RIGHT/FULL JOIN target: a RIGHT/FULL join's
// representative can be NULL, so an unqualified reference is really
// COALESCE(repr.c, idx.c). E.g. join2.test's "t2 NATURAL JOIN t3 NATURAL RIGHT
// JOIN t1" shows t1.b exactly when t2.b is NULL. No-op for other join kinds.
//
// tableScope.coalesceFallback holds a list per representative+column, appended
// in FROM order, so "t1 RIGHT JOIN t2 USING(a) RIGHT JOIN t3 USING(a)" gives
// COALESCE(t1.a, t2.a, t3.a). Resolution takes the first non-NULL entry; on any
// row at most one source is the genuine one, so the order of skipped NULLs
// does not matter.
func installCoalesceFallback(jts []joinedTable, idx int, kind JoinKind, hidden map[string]int) error {
	if kind != JoinRight && kind != JoinFull {
		return nil
	}
	for lname, repr := range hidden {
		if jts[repr].coalesceFallback == nil {
			jts[repr].coalesceFallback = make(map[string][]int, len(hidden))
		}
		jts[repr].coalesceFallback[lname] = append(jts[repr].coalesceFallback[lname], idx)
	}
	return nil
}

// hasRightOuter reports whether any of jts is a RIGHT/FULL JOIN target --
// see this file's package doc comment (RIGHT/FULL section) for why, when
// true, computeExecOrder disables its leading-prefix reordering and
// planJoinPushdown disables ALL WHERE-conjunct pushdown, for the whole FROM
// clause, not just the rightOuter item's own level.
func hasRightOuter(jts []joinedTable) bool {
	for _, jt := range jts {
		if jt.rightOuter {
			return true
		}
	}
	return false
}

// hasGroups reports whether any of jts starts a parenthesized join GROUP
// (FromItem.GroupLen/joinedTable.groupLen's doc comments) -- see
// computeExecOrder/planJoinPushdown, both of which -- exactly like
// hasRightOuter's own early-return gate -- must treat the WHOLE FROM clause
// conservatively (never reordered, never bucket-pushed) once true, since a
// group is an opaque, atomically-evaluated relation as far as any of that
// per-item planning is concerned.
func hasGroups(jts []joinedTable) bool {
	for _, jt := range jts {
		if jt.groupLen > 0 {
			return true
		}
	}
	return false
}

// resolveGroupBoundary returns the start of the innermost parenthesized join
// group containing i as an internal (non-connector) member; in "t1 JOIN (t2
// JOIN (t3 LEFT JOIN (t4 JOIN t5)))" t4's is t3's group. ok is false when i is
// not an internal member of any group.
func resolveGroupBoundary(jts []joinedTable, i int) (start int, ok bool) {
	best := -1
	for g := 0; g < i; g++ {
		if gl := jts[g].groupLen; gl > 0 && g+gl > i {
			if g > best {
				best = g
			}
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

// fixItemGroupScoping repairs jts[i]'s just-computed on/hidden when they reach
// outside its innermost enclosing group (resolveGroupBoundary). It runs before
// installCoalesceFallback, for every item, and is a no-op outside groups.
//
// desugarJoinItem's representative search is a flat, group-unaware walk. For
// an internal group member, a representative outside the group names a table
// not yet bound when the group's rows are built, so the condition would see
// NULL. joinC.test's "t1 JOIN (t2 JOIN (t3 LEFT JOIN (t4 JOIN t5 USING(a))
// USING(a)) USING(a)) USING(a)" returned 0 rows instead of 2 because every
// USING(a) resolved back to t1. hidden must be fixed too, or
// installCoalesceFallback installs onto the wrong table (joinB.test lost a row).
//
//   - USING/NATURAL (hidden != nil): the representative is this engine's own
//     choice, so it is redirected to the innermost group's first item, which is
//     equal to the original by the INNER connectors in between.
//   - Explicit ON (hidden == nil): the user wrote the escaping reference;
//     SQLite rejects it ("no such column: x.a" for "t1 x JOIN (t1 y RIGHT JOIN
//     t2 ON x.a = y.a)"), and so does this.
func fixItemGroupScoping(jts []joinedTable, i int, hidden map[string]int) (map[string]int, error) {
	boundary, ok := resolveGroupBoundary(jts, i)
	if !ok || jts[i].on == nil {
		return hidden, nil
	}
	scopes := buildScopes(jts[:i+1]) // only tables 0..i are built so far -- sufficient, since every reference this checks is necessarily at an earlier position than i
	refs := map[int]bool{}
	collectTableRefs(jts[i].on, scopes, refs)
	escapes := false
	for idx := range refs {
		if idx < boundary {
			escapes = true
			break
		}
	}
	if !escapes {
		return hidden, nil
	}
	if hidden == nil {
		return nil, fmt.Errorf("engine: unsupported: JOIN ... ON inside a parenthesized join group referencing a table outside that group")
	}
	rewritten, err := rewriteQualifiedRefs(jts[i].on, scopes, boundary, jts[boundary].name, jts[boundary].tbl.cols)
	if err != nil {
		return nil, err
	}
	jts[i].on = rewritten
	fixedHidden := make(map[string]int, len(hidden))
	for lname, repr := range hidden {
		if repr < boundary {
			fixedHidden[lname] = boundary
		} else {
			fixedHidden[lname] = repr
		}
	}
	return fixedHidden, nil
}

// rewriteQualifiedRefs returns a copy of e (only ColumnExpr/BinaryExpr, the
// shapes desugarJoinItem builds) with every qualified ColumnExpr resolving to a
// scope before boundary redirected to boundaryName (see fixItemGroupScoping).
// boundaryCols is checked to confirm the boundary item has the column.
func rewriteQualifiedRefs(e Expr, scopes []tableScope, boundary int, boundaryName string, boundaryCols []columnInfo) (Expr, error) {
	hasCol := func(name string) bool {
		for _, c := range boundaryCols {
			if equalFoldName(c.Name, name) {
				return true
			}
		}
		return false
	}
	var walk func(Expr) (Expr, error)
	walk = func(e Expr) (Expr, error) {
		switch x := e.(type) {
		case ColumnExpr:
			if x.Qualifier == "" {
				return x, nil
			}
			for idx, ts := range scopes {
				if equalFoldName(ts.name, x.Qualifier) {
					if idx < boundary {
						if !hasCol(x.Name) {
							return nil, fmt.Errorf("engine: unsupported: parenthesized join group's own USING/NATURAL representative is not reachable from within the group")
						}
						x.Qualifier = boundaryName
					}
					break
				}
			}
			return x, nil
		case BinaryExpr:
			l, err := walk(x.L)
			if err != nil {
				return nil, err
			}
			r, err := walk(x.R)
			if err != nil {
				return nil, err
			}
			x.L, x.R = l, r
			return x, nil
		default:
			return e, nil
		}
	}
	return walk(e)
}

// onsOf extracts jts' effective join conditions (each already desugared from
// USING/NATURAL, if applicable, by resolveFrom) in FROM order -- what
// checkFromSupported and query.go's per-item validateColumnRefs loop check,
// in place of reading FromItem.On directly (which, for a USING/NATURAL item,
// is never populated -- the parser can't compute it; see desugarJoinItem).
func onsOf(jts []joinedTable) []Expr {
	ons := make([]Expr, len(jts))
	for i, jt := range jts {
		ons[i] = jt.on
	}
	return ons
}

// buildScopes builds the (shared, read-only, reused for every row) tableScope
// list an evalCtx needs to resolve bare/qualified column references across
// every table in jts (see resolveColumn).
func buildScopes(jts []joinedTable) []tableScope {
	scopes := make([]tableScope, len(jts))
	for i, jt := range jts {
		// noRowid: true for a derived table (jt.derived, no rowid pseudo-
		// column at all -- see joinedTable.derived's doc comment) OR a
		// WITHOUT ROWID base table (jt.tbl.withoutRowid -- see that field's
		// doc comment, query.go): C SQLite errors "no such column:
		// rowid"/"oid"/"_rowid_" against one just like it does for a
		// derived table, verified directly.
		scopes[i] = tableScope{name: jt.name, tableName: jt.tableName, cols: jt.tbl.cols, colIndex: buildColIndex(jt.tbl.cols), offset: jt.offset, coalesced: jt.coalesced, coalesceFallback: jt.coalesceFallback, noRowid: jt.derived || jt.tbl.withoutRowid, nestedNonLeading: jt.nestedNonLeading}
	}
	return scopes
}

// joinDesugarState accumulates, over a left-to-right walk of the FROM items,
// the visible "*"-expansion columns (those not coalesced away by an earlier
// USING/NATURAL) and each item's scope name. C computes a NATURAL/USING join's
// common columns against the join's output so far, not the raw tables.
//
// resolveFrom and resolveJoinSources (vdbe_join_codegen.go) both thread one
// state through desugarJoinItem, so they cannot desugar differently.
type joinDesugarState struct {
	visible    []visibleCol
	scopeNames []string // scopeNames[i] is FROM item i's own scope name (alias, or table name if unaliased), indexed by ABSOLUTE FROM-item index -- see recordScope
	nested     []bool   // nested[i] mirrors FROM item i's FromItem.NestedNonLeading, same indexing

	// sharedName records that some desugared USING/NATURAL condition side had
	// to be pinned by FROM-item index (ColumnExpr.UsingPinned) because its
	// scope NAME is not unique in this FROM clause -- an unaliased self-join.
	// It gates checkSharedScopeNames, whose declines exist only for that
	// shape: a FROM clause whose names are all distinct keeps exactly its
	// prior behavior, checks included.
	sharedName bool
}

// recordScope stores item idx's scope name and NestedNonLeading flag at
// position idx, padding skipped positions, so the slices stay indexable by
// absolute FROM-item index.
//
// resolveJoinSources skips a group's internal members (they are resolved by a
// recursive call with their own state); a plain append drifted by one per
// skipped member, reading the wrong item's name and, with two skipped members,
// running off the end of the slice.
func (state *joinDesugarState) recordScope(idx int, name string, nested bool) {
	for len(state.scopeNames) <= idx {
		state.scopeNames = append(state.scopeNames, "")
		state.nested = append(state.nested, false)
	}
	state.scopeNames[idx] = name
	state.nested[idx] = nested
}

// firstScopeNamed returns the FROM-item index of the FIRST scope named n,
// matched case-insensitively exactly as resolveColumn/resolveInScopes match a
// qualifier -- so a caller comparing this against its OWN index is asking "does
// a qualified reference spelled with my own name land on me?". -1 for n == ""
// (an unaliased derived table, which no qualifier can name at all), which no
// real item index ever equals, so that case pins too.
func (state *joinDesugarState) firstScopeNamed(n string) int {
	if n == "" {
		return -1
	}
	for i, s := range state.scopeNames {
		if equalFoldName(s, n) {
			return i
		}
	}
	return -1
}

// expose records FROM item idx's own output columns -- every one this item's
// USING/NATURAL coalescing did not hide -- as visible to later items, then
// re-checks the whole clause against checkSharedScopeNames.
func (state *joinDesugarState) expose(idx int, cols []columnInfo, hidden map[string]int) error {
	for _, c := range cols {
		if _, isHidden := hidden[r33sFoldIdent(c.Name)]; isHidden {
			continue
		}
		state.visible = append(state.visible, visibleCol{tableIdx: idx, name: c.Name})
	}
	return state.checkSharedScopeNames()
}

// checkSharedScopeNames declines the unaliased-self-join shape SQLite itself
// refuses: a same-named item inside a non-leading parenthesized FROM term
// (FromItem.NestedNonLeading). SQLite materializes such a term and the
// duplicated name makes its pseudo-rowid ambiguous: "SELECT * FROM t2, (t1
// NATURAL JOIN t1)" is "ambiguous column name: main.t1._ROWID_", while the
// leading spelling answers. It re-runs over the whole clause on every later
// item, since a third same-named item can arrive later.
//
// Ambiguity of an exposed column is per reference, not per statement, and is
// already reported by resolveInScopes and expandSelectList.
func (state *joinDesugarState) checkSharedScopeNames() error {
	if !state.sharedName {
		return nil
	}
	for i, n := range state.scopeNames {
		first := state.firstScopeNamed(n)
		if n == "" || first == i {
			continue
		}
		if state.nested[i] || state.nested[first] {
			return fmt.Errorf("engine: unsupported: NATURAL/USING self-join of %q inside a non-leading parenthesized FROM term (SQLite reports its pseudo-rowid as ambiguous)", n)
		}
	}
	return nil
}

// checkSelfJoinGroupSupported declines a NATURAL/USING join whose FROM names
// the same scope twice and also contains a parenthesized join group. Self-join
// sides are addressed by FROM-item index (ColumnExpr.UsingPinned), which stops
// lining up with scope positions once joinScopes/sourceScopes collapse a group;
// same reason as checkDerivedJoinSupported.
func checkSelfJoinGroupSupported(items []FromItem) error {
	hasUsing, hasGroup := false, false
	for _, it := range items {
		if it.Natural || it.Using != nil {
			hasUsing = true
		}
		// it.NestedGroupSpan != nil (FromItem.NestedGroupSpan's own doc
		// comment) is included defensively alongside the ordinary
		// GroupLen > 0 test: a LONE re-wrapped group with no further
		// wrapping carries GroupLen == 0 at this top level (checkFlattenSafe
		// never assigns one to a single-item elems), so GroupLen alone would
		// miss it here.
		if it.GroupLen > 0 || it.NestedGroupSpan != nil {
			hasGroup = true
		}
	}
	if !hasUsing || !hasGroup {
		return nil
	}
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		name := it.Alias
		if name == "" {
			name = it.Table
		}
		if name == "" {
			continue // an unaliased derived table: checkDerivedJoinSupported's case
		}
		lname := r33sFoldIdent(name)
		if seen[lname] {
			return fmt.Errorf("engine: unsupported: NATURAL/USING self-join of %q combined with a parenthesized join group", name)
		}
		seen[lname] = true
	}
	return nil
}

// visibleCol is one entry of joinDesugarState.visible: a column name (its
// original-case spelling, from whichever table contributed it) and the
// FROM-item index that supplies it.
type visibleCol struct {
	tableIdx int
	name     string
}

// desugarJoinItem computes FROM item idx's effective join condition and
// coalescing info from its parsed Join/On/Using/Natural, scope name and
// columns, and adds its columns to state's visible list.
//
// Returns:
//   - onExpr: it.On for a plain item; for NATURAL/USING the synthesized
//     "<repr>.<c> = <name>.<c> AND ...", or nil for a NATURAL join with no
//     common columns (a cross join; with LEFT, nil means "always matches").
//   - hidden: for NATURAL/USING with common columns, lower-cased name ->
//     representative's FROM-item index; nil otherwise. Installed as
//     tableScope.coalesced, it makes an unqualified reference resolve to the
//     representative and "*" show the column once.
//
// The representative is the earliest earlier item whose column of that name
// is not already coalesced away, so "t1 JOIN t2 USING(a) JOIN t3 USING(a)"
// resolves "a" to t1.a. It is always on the left side of every pairing, never
// the NULL-extended side of the join that introduced it.
func desugarJoinItem(state *joinDesugarState, idx int, it FromItem, name string, cols []columnInfo) (onExpr Expr, hidden map[string]int, err error) {
	state.recordScope(idx, name, it.NestedNonLeading)

	if !it.Natural && it.Using == nil {
		if err := state.expose(idx, cols, nil); err != nil {
			return nil, nil, err
		}
		return it.On, nil, nil
	}

	var names []string
	if it.Natural {
		rightHas := make(map[string]bool, len(cols))
		for _, c := range cols {
			rightHas[r33sFoldIdent(c.Name)] = true
		}
		seen := make(map[string]bool, len(state.visible))
		for _, vc := range state.visible {
			lname := r33sFoldIdent(vc.name)
			if rightHas[lname] && !seen[lname] {
				names = append(names, vc.name)
				seen[lname] = true
			}
		}
	} else {
		names = it.Using
	}

	if len(names) == 0 {
		// NATURAL with no common columns at all: a true cross join, no
		// condition -- SQLite's own documented fallback. (USING's own column
		// list is never empty syntactically -- parseUsingList requires at
		// least one -- so this branch is only ever reached for NATURAL.)
		if err := state.expose(idx, cols, nil); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}

	rightIdx := buildColIndex(cols)
	hidden = make(map[string]int, len(names))
	for _, n := range names {
		lname := r33sFoldIdent(n)
		if _, ok := rightIdx[lname]; !ok {
			// select.c:588-591's own wording, and its own PREPARE-time
			// rejection: semanticf so it propagates as the statement's error
			// instead of reaching the caller inside "VDBE-only: ... (no
			// fallback)".
			return nil, nil, semanticf("engine: cannot join using column %s - column not present in both tables", n)
		}
		// The common column appears more than once in this item's own columns
		// (only a derived table or view). C coalesces only the first and keeps
		// later ones as ':N' output columns ("SELECT * FROM t1 JOIN (SELECT a, a
		// FROM t2) AS d USING(a)" has three columns); hidden is keyed by name and
		// would hide both, so decline, as errIfDuplicateOutputNames does for
		// ':N' naming elsewhere.
		dups := 0
		for _, c := range cols {
			if equalFoldName(c.Name, n) {
				dups++
			}
		}
		if dups > 1 {
			return nil, nil, fmt.Errorf("engine: unsupported: duplicate column name %q on the USING/NATURAL-coalesced side of a join (SQLite's ':N' disambiguation is not reproduced)", n)
		}
		// tableAndColumnIndex (select.c:379-409) returns the first earlier item
		// exposing the name, with no ambiguity check; both NATURAL synthesis
		// (select.c:544) and USING (select.c:583) use it. C's only USING
		// ambiguity error, "ambiguous reference to %s in USING()"
		// (select.c:619), fires inside a RIGHT/FULL join's multi-left coalesce
		// (JT_LTORJ, select.c:600), so the check here is scoped to it.Join's
		// own RIGHT/FULL.
		repr, ambiguous := -1, false
		for _, vc := range state.visible {
			if equalFoldName(vc.name, n) {
				if repr == -1 {
					repr = vc.tableIdx
				} else if vc.tableIdx != repr {
					ambiguous = true
				}
			}
		}
		if repr == -1 {
			// select.c:588-591's own wording, and its own PREPARE-time
			// rejection: semanticf so it propagates as the statement's error
			// instead of reaching the caller inside "VDBE-only: ... (no
			// fallback)".
			return nil, nil, semanticf("engine: cannot join using column %s - column not present in both tables", n)
		}
		if ambiguous && (it.Join == JoinRight || it.Join == JoinFull) {
			return nil, nil, fmt.Errorf("engine: ambiguous column name in NATURAL/USING join: %s", n)
		}
		hidden[lname] = repr

		// Each side is addressed by scope name, unless the name cannot single
		// it out; then by FROM-item index (ColumnExpr.UsingPinned), because a
		// qualified reference resolves to the first scope bearing the name:
		//
		//   - an unaliased derived table has no scope name;
		//   - an unaliased self-join ("t1 JOIN t1 USING(a)", "t1 x JOIN t2 x
		//     USING(a)") would collapse to the tautology "t1.a = t1.a". This
		//     also covers "t2 JOIN t1 USING(a) JOIN t1 USING(a)", where the
		//     representative is a third table.
		//
		// Pins are used only when needed. The pinned side's Qualifier is
		// cleared so plan-time walkers that read Qualifier
		// (collectTableRefs, markGroupConnectorCoalesceAware) treat it
		// conservatively as unqualified.
		lref := ColumnExpr{Qualifier: state.scopeNames[repr], Name: n, UsingRepr: true, UsingReprOwnItem: idx}
		if state.firstScopeNamed(lref.Qualifier) != repr {
			state.sharedName = state.sharedName || lref.Qualifier != ""
			lref.Qualifier = ""
			lref.UsingPinned, lref.UsingPinnedItem = true, repr
		}
		rref := ColumnExpr{Qualifier: name, Name: n}
		if state.firstScopeNamed(name) != idx {
			state.sharedName = state.sharedName || name != ""
			rref.Qualifier = ""
			rref.UsingPinned, rref.UsingPinnedItem = true, idx
		}
		cond := Expr(BinaryExpr{Op: "=", L: lref, R: rref})
		if onExpr == nil {
			onExpr = cond
		} else {
			onExpr = BinaryExpr{Op: "AND", L: onExpr, R: cond}
		}
	}

	if err := state.expose(idx, cols, hidden); err != nil {
		return nil, nil, err
	}
	return onExpr, hidden, nil
}

// splitTopLevelAnd flattens a top-level chain of AND-BinaryExprs into its
// individual conjuncts -- e.g. "a AND b AND c" parses left-associatively as
// "(a AND b) AND c" and flattens back to [a, b, c] -- so each can potentially
// be pushed down to the earliest join level it's fully determined at (see
// planJoinPushdown). Anything that isn't a top-level AND (a single
// expression, an OR, ...) is returned as its own one-element slice. nil in
// yields nil out.
func splitTopLevelAnd(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(BinaryExpr); ok && b.Op == "AND" {
		return append(splitTopLevelAnd(b.L), splitTopLevelAnd(b.R)...)
	}
	return []Expr{e}
}

// mentionsSubquery reports whether e's tree contains a SubqueryExpr,
// ExistsExpr, or InExpr-with-a-subquery anywhere. Used to conservatively
// keep a WHERE conjunct from being pushed down earlier than the last join
// level: a correlated subquery inside it might reference any table in this
// FROM's scope via the outer-chain (resolveColumn), in a way
// conjunctMaxTableIdx -- which only walks this expression's own ColumnExpr
// nodes, never descending into a nested SelectStmt -- can't see.
func mentionsSubquery(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return false
	case SubqueryExpr, ExistsExpr:
		return true
	case UnaryExpr:
		return mentionsSubquery(x.X)
	case BinaryExpr:
		return mentionsSubquery(x.L) || mentionsSubquery(x.R)
	case IsNullExpr:
		return mentionsSubquery(x.X)
	case InExpr:
		if x.Sub != nil {
			return true
		}
		if mentionsSubquery(x.X) {
			return true
		}
		for _, a := range x.List {
			if mentionsSubquery(a) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return mentionsSubquery(x.X) || mentionsSubquery(x.Lo) || mentionsSubquery(x.Hi)
	case LikeExpr:
		return mentionsSubquery(x.X) || mentionsSubquery(x.Pattern) || mentionsSubquery(x.Escape)
	case GlobExpr:
		return mentionsSubquery(x.X) || mentionsSubquery(x.Pattern)
	case CollateExpr:
		return mentionsSubquery(x.X)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if mentionsSubquery(a) {
				return true
			}
		}
		return false
	case CastExpr:
		return mentionsSubquery(x.X)
	case CaseExpr:
		if x.Base != nil && mentionsSubquery(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if mentionsSubquery(w.When) || mentionsSubquery(w.Then) {
				return true
			}
		}
		return x.Else != nil && mentionsSubquery(x.Else)
	default:
		return false
	}
}

// collectTableRefs adds to out the index of every scope e references a column
// of; constants and outer references contribute nothing. A qualified reference
// matches the named table; an unqualified one matches every table with that
// column except one whose copy USING/NATURAL coalesced away (as lookupName
// skips it); ambiguity is left to resolveColumn.
//
// This feeds wherePlanTermsFrom's WhereTerm.prereqAll, which decides which
// loop orders the planner can build, so an over-estimate can change row
// order and an under-estimate tests a conjunct before its cursor is
// positioned. The RIGHT/FULL coalesce chain is therefore marked explicitly.
func collectTableRefs(e Expr, scopes []tableScope, out map[int]bool) {
	// "<tab> MATCH q" names the fts table itself, which considerColumn
	// never resolves. Evaluating MATCH reads the fts table's current row
	// (buildFts5Doc, fts5_match.go; fts3ScopeDocid, fts3_search.go), so a
	// missed dependency runs it against an unpositioned cursor: "SELECT
	// t1.a, ft.a FROM t1, ft WHERE ft MATCH 'b'" returned nothing or
	// errored (fts5misc.test ticket [7c0e06b16]).
	considerMatchTable := func(e Expr) {
		ce, ok := e.(ColumnExpr)
		if !ok || ce.Qualifier != "" {
			return
		}
		for i, ts := range scopes {
			if equalFoldName(ts.name, ce.Name) {
				out[i] = true
			}
		}
	}
	// markFallback adds the RIGHT/FULL JOIN coalesce chain a read of ts's own
	// column lname falls back to (tableScope.coalesceFallback): the
	// value is COALESCE(this table's, each owner's), so the read genuinely
	// depends on every owner too. Only ever non-empty when a JoinRight/JoinFull
	// item coalesced that name back onto ts (installCoalesceFallback).
	markFallback := func(ts tableScope, lname string) {
		for _, owner := range ts.coalesceFallback[lname] {
			if owner >= 0 && owner < len(scopes) {
				out[owner] = true
			}
		}
	}
	considerColumn := func(x ColumnExpr) {
		lname := r33sFoldIdent(x.Name)
		if x.UsingPinned {
			// A desugared USING/NATURAL condition's side, bound by FROM-item
			// INDEX because no scope name reaches it (ColumnExpr.UsingPinned,
			// sql_ast.go). Its Qualifier is empty, so the unqualified arm below
			// would report a dependency on every item that HAS the column --
			// for "t JOIN t USING(a)", both copies -- rather than on the one
			// item resolveColumnEx actually reads.
			if x.UsingPinnedItem >= 0 && x.UsingPinnedItem < len(scopes) {
				out[x.UsingPinnedItem] = true
				if x.UsingRepr {
					markFallback(scopes[x.UsingPinnedItem], lname)
				}
			}
			return
		}
		for i, ts := range scopes {
			if x.Qualifier != "" {
				if equalFoldName(ts.name, x.Qualifier) {
					out[i] = true
					if x.UsingRepr {
						markFallback(ts, lname)
					}
					return
				}
				continue
			}
			// lookupName (resolve.c) skips a column named in the item's own USING
			// clause, so an unqualified reference to a common column depends only
			// on the representative. Over-reporting it changed the planner's loop
			// order (and the row order of an unordered result).
			if _, hidden := ts.coalesced[lname]; hidden || ts.fromExists {
				continue // fromExists: see wherePlanColumnRef
			}
			if _, ok := ts.colIndex[lname]; ok {
				out[i] = true
				markFallback(ts, lname)
			}
		}
		if x.Qualifier == "" {
			return
		}
		// The qualifier names no FROM item; it may be a join group's alias
		// (tableScope.groupAlias), which names every member. Depend on all of
		// them: reporting none pushed the conjunct before the loop, where it
		// read an unopened cursor. Over-reporting can only delay a conjunct.
		for i, ts := range scopes {
			if equalFoldName(ts.groupAlias, x.Qualifier) {
				out[i] = true
			}
		}
	}
	var walk func(Expr)
	walk = func(e Expr) {
		switch x := e.(type) {
		case nil, LiteralExpr, ParamExpr:
		case ColumnExpr:
			considerColumn(x)
		case UnaryExpr:
			walk(x.X)
		case BinaryExpr:
			walk(x.L)
			walk(x.R)
		case IsNullExpr:
			walk(x.X)
		case InExpr:
			walk(x.X)
			for _, a := range x.List {
				walk(a)
			}
		case BetweenExpr:
			walk(x.X)
			walk(x.Lo)
			walk(x.Hi)
		case LikeExpr:
			walk(x.X)
			walk(x.Pattern)
			walk(x.Escape)
		case GlobExpr:
			walk(x.X)
			walk(x.Pattern)
		case MatchExpr:
			considerMatchTable(x.X)
			walk(x.X) // the "col MATCH q" form resolves as an ordinary column
			walk(x.Pattern)
		case CollateExpr:
			walk(x.X)
		case FuncExpr:
			// An fts3/fts4 auxiliary function's first argument names the table
			// itself (fts3AuxFuncName/fts3ResolveMatchTarget, fts3_search.go),
			// so it needs considerMatchTable just like MATCH; otherwise
			// matchinfo/offsets/snippet in WHERE read the fts cursor before it
			// is positioned.
			if len(x.Args) > 0 && fts3AuxFuncName(x.Name) {
				considerMatchTable(x.Args[0])
			}
			for _, a := range x.walkArgs() {
				walk(a)
			}
		case CastExpr:
			walk(x.X)
		case CaseExpr:
			if x.Base != nil {
				walk(x.Base)
			}
			for _, w := range x.Whens {
				walk(w.When)
				walk(w.Then)
			}
			if x.Else != nil {
				walk(x.Else)
			}
		}
	}
	walk(e)
}

// computeExecOrder computes the order the nested loop binds tables, as a
// permutation of jts' indices (jts' own order fixes the output column layout).
// Placing each table as soon as some conjunct is determined by it keeps the
// branch count small for long comma joins with pairwise links.
//
// Only a leading run of comma/CROSS items (On == nil, not LEFT) is reordered:
// inner joins commute, but a LEFT JOIN's NULL-extension and an explicit ON
// depend on textual position. The rest keeps its order after the prefix.
//
// It also returns the transient automatic index the ported planner builds at
// each level, nil where this function's own rule decided (autoIndexKey,
// where_plan.go).
func computeExecOrder(jts []joinedTable, scopes []tableScope, where Expr) ([]int, []*autoIndexKey) {
	order := make([]int, len(jts))
	for i := range order {
		order[i] = i
	}
	if hasRightOuter(jts) || hasGroups(jts) {
		// Never reorder with a RIGHT/FULL JOIN (C's row order there is FROM
		// order, and reordering could change it) or a parenthesized join group,
		// which is evaluated as one atomic relation.
		return order, nil
	}

	// The PORTED SQLite planner answers first, for the FROM clauses it can
	// reproduce exactly (markWherePlanEligibility decides which, and every
	// joinedTable built outside the read-side scan compiler leaves wherePlanOK
	// false). Row order is observable -- group_concat, a bare column in a
	// grouped aggregate, LIMIT without a totally-ordering ORDER BY -- so where
	// the algorithm can be reproduced it is reproduced rather than
	// approximated; the component heuristic below stays for everything else.
	// See where_plan.go.
	if planned, keys, ok := sqliteExecOrder(jts, scopes, where); ok {
		return planned, keys
	}

	// A two-item FROM where an fts vtab's MATCH correlates to exactly the
	// other item: C's nesting is forced to visit that item first
	// (ftsMatchForcedOrder, where_plan_fts_forced_order.go). The ported
	// solver never sees this shape since it excludes vtab sources.
	if planned, ok := ftsMatchForcedOrder(jts, scopes, where); ok {
		return planned, nil
	}

	prefixLen := 0
	for _, jt := range jts {
		if jt.on != nil || jt.left {
			break
		}
		prefixLen++
	}
	if prefixLen <= 1 {
		return order, nil
	}

	var tableSets []map[int]bool
	for _, cj := range splitTopLevelAnd(where) {
		set := map[int]bool{}
		collectTableRefs(cj, scopes, set)
		// Only conjuncts entirely within the reorderable prefix inform
		// reordering; anything touching a fixed-position table can't be
		// "completed" by placing more prefix tables, so it's irrelevant here
		// (it's still handled correctly, just not specially favored, by the
		// ordinary bucket/deferred assignment in planJoinPushdown).
		inPrefix := true
		for idx := range set {
			if idx >= prefixLen {
				inPrefix = false
				break
			}
		}
		if inPrefix && len(set) > 0 {
			tableSets = append(tableSets, set)
		}
	}

	// Group the prefix tables into connected components of the join graph
	// (a multi-table conjunct unions its tables; a single-table one marks
	// its table as filtered). A component with no filtered table can only
	// be pruned by joining its own tables, so place it first while the
	// branch count is small; filtered components are cheap anywhere and
	// go last.
	parent := make([]int, prefixLen)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	hasOwnFilter := make([]bool, prefixLen)
	for _, set := range tableSets {
		if len(set) == 1 {
			for idx := range set {
				hasOwnFilter[idx] = true
			}
			continue
		}
		first, started := 0, false
		for idx := range set {
			if !started {
				first, started = idx, true
				continue
			}
			union(first, idx)
		}
	}

	compTables := map[int][]int{}
	compHasFilter := map[int]bool{}
	for i := 0; i < prefixLen; i++ {
		r := find(i)
		compTables[r] = append(compTables[r], i)
		if hasOwnFilter[i] {
			compHasFilter[r] = true
		}
	}
	roots := make([]int, 0, len(compTables))
	for r := range compTables {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(i, j int) bool {
		fi, fj := compHasFilter[roots[i]], compHasFilter[roots[j]]
		if fi != fj {
			return !fi // no-own-filter components go first
		}
		if len(compTables[roots[i]]) != len(compTables[roots[j]]) {
			return len(compTables[roots[i]]) < len(compTables[roots[j]])
		}
		return roots[i] < roots[j]
	})

	const unset = 1 << 30
	placed := make([]int, 0, prefixLen)
	placedSet := make(map[int]bool, prefixLen)
	scoreFor := func(t int) int {
		best := unset
		for _, set := range tableSets {
			if !set[t] {
				continue
			}
			need := 0
			for idx := range set {
				if idx != t && !placedSet[idx] {
					need++
				}
			}
			if need < best {
				best = need
			}
		}
		return best
	}
	for _, r := range roots {
		remaining := append([]int(nil), compTables[r]...)
		for len(remaining) > 0 {
			bestPos, bestScore := 0, unset+1
			for i, t := range remaining {
				if s := scoreFor(t); s < bestScore {
					bestScore, bestPos = s, i
				}
			}
			t := remaining[bestPos]
			placed = append(placed, t)
			placedSet[t] = true
			remaining = append(remaining[:bestPos], remaining[bestPos+1:]...)
		}
	}

	execOrder := make([]int, len(jts))
	copy(execOrder, placeAfterTvfArgs(placed, jts, scopes))
	for i := prefixLen; i < len(jts); i++ {
		execOrder[i] = i
	}
	return execOrder, nil
}

// placeAfterTvfArgs keeps each table-valued function after the tables its
// arguments read. C adds every argument as a WHERE term on the function's
// hidden column (whereexpr.c:1926-1935), making those tables prerequisites;
// without this "json_each(t.a) j, t" put j outermost. Each step binds the first
// table whose dependencies are bound; an unsatisfiable dependency leaves placed
// unchanged and the argument then refuses to fold.
func placeAfterTvfArgs(placed []int, jts []joinedTable, scopes []tableScope) []int {
	deps := map[int]map[int]bool{}
	for _, t := range placed {
		for _, a := range jts[t].tvfArgs {
			if deps[t] == nil {
				deps[t] = map[int]bool{}
			}
			collectTableRefs(a, scopes, deps[t])
		}
		delete(deps[t], t)
	}
	if len(deps) == 0 {
		return placed
	}
	out := make([]int, 0, len(placed))
	bound := map[int]bool{}
	pending := append([]int(nil), placed...)
	for len(pending) > 0 {
		pick := -1
		for i, t := range pending {
			ready := true
			for d := range deps[t] {
				if !bound[d] {
					ready = false
					break
				}
			}
			if ready {
				pick = i
				break
			}
		}
		if pick < 0 {
			return placed
		}
		out = append(out, pending[pick])
		bound[pending[pick]] = true
		pending = append(pending[:pick], pending[pick+1:]...)
	}
	return out
}

// joinPlan is the plan for one run of a resolved FROM clause. execOrder is the
// binding order (computeExecOrder). buckets[i] holds the WHERE conjuncts to
// test right after the table at execution position i is bound; deferred holds
// those that must wait for the whole row (LEFT-joined tables onward, or a
// correlated subquery). autoIdxKeys[i], when non-nil, is the planner's
// transient automatic index at position i, whose key decides that level's row
// order (autoIndexKey, where_plan.go; emitJoinLoops, vdbe_join_codegen.go).
type joinPlan struct {
	execOrder   []int
	buckets     [][]Expr
	deferred    []Expr
	autoIdxKeys []*autoIndexKey
}

// planJoinPushdown computes the binding order and assigns each top-level WHERE
// conjunct to the earliest execution position at which all its tables are
// bound, deferring it if that is at or past the first LEFT-joined table (LEFT
// tables are never reordered) or it contains a correlated subquery.
func planJoinPushdown(where Expr, jts []joinedTable, scopes []tableScope) joinPlan {
	if hasRightOuter(jts) || hasGroups(jts) {
		// Defer every conjunct with a RIGHT/FULL item or a join group. A
		// RIGHT/FULL match decision is global, so pruning any combination early
		// could hide a match; a group is one atomic relation that no per-item
		// level can test against. Conservative, but correct.
		order, keys := computeExecOrder(jts, scopes, where)
		return joinPlan{
			execOrder:   order,
			buckets:     make([][]Expr, len(jts)),
			deferred:    splitTopLevelAnd(where),
			autoIdxKeys: keys,
		}
	}

	firstLeftIdx := len(jts)
	for i, jt := range jts {
		if jt.left {
			firstLeftIdx = i
			break
		}
	}

	execOrder, autoIdxKeys := computeExecOrder(jts, scopes, where)
	pos := make([]int, len(jts)) // pos[originalTableIdx] = execution position
	for p, origIdx := range execOrder {
		pos[origIdx] = p
	}

	// An INNER item marked onToWhere has had its ON clause MOVED into the WHERE
	// clause -- sqlite3ProcessJoin's own rewrite (select.c), which ANDs each ON
	// onto the end of the WHERE in FROM order. Its conjuncts are bucketed here
	// like any other WHERE conjunct, so emitJoinLevel tests them at the earliest
	// execution depth binding every table they name rather than at the item's own
	// depth. See joinSource.onToWhere for why the tree itself is left intact.
	conjuncts := splitTopLevelAnd(where)
	for i := range jts {
		if jts[i].onToWhere {
			conjuncts = append(conjuncts, splitTopLevelAnd(jts[i].on)...)
		}
	}

	plan := joinPlan{execOrder: execOrder, buckets: make([][]Expr, len(jts)), autoIdxKeys: autoIdxKeys}
	for _, cj := range conjuncts {
		refs := map[int]bool{}
		collectTableRefs(cj, scopes, refs)
		origMax := -1
		for idx := range refs {
			if idx > origMax {
				origMax = idx
			}
		}
		forceLast := mentionsSubquery(cj)
		checkIdx := origMax
		if forceLast && checkIdx < len(jts)-1 {
			checkIdx = len(jts) - 1
		}
		if checkIdx < 0 {
			checkIdx = 0
		}
		if checkIdx >= firstLeftIdx {
			plan.deferred = append(plan.deferred, cj)
			continue
		}

		execLvl := 0
		for idx := range refs {
			if p := pos[idx]; p > execLvl {
				execLvl = p
			}
		}
		if forceLast {
			execLvl = len(jts) - 1
		}
		plan.buckets[execLvl] = append(plan.buckets[execLvl], cj)
	}
	return plan
}

// checkFromSupported runs checkExprSupported over every FROM item's
// effective join condition (ons -- each already desugared from USING/NATURAL
// if applicable, by resolveFrom/resolveJoinSources; see onsOf/desugarJoinItem),
// exactly like WHERE/select-list/ORDER-BY are already checked at plan time
// (see execSelect) -- so an unsupported construct written in an ON clause (or
// synthesized from USING/NATURAL) is rejected up front rather than silently
// never being reached because an earlier join step's WHERE/ON filtered every
// row first.
func checkFromSupported(ons []Expr) error {
	for _, on := range ons {
		if err := checkExprSupported(on); err != nil {
			return fmt.Errorf("engine: JOIN ... ON: %w", err)
		}
	}
	return nil
}

// itemHasNoIndexChain reports whether it resolves to something with no index
// chain for "INDEXED BY" to name: a derived table, a table-valued function,
// or a CTE. Each is an ephemeral Table in C SQLite, so
// sqlite3IndexedByLookup's walk over pTab->pIndex finds nothing and the
// statement is "no such index: <name>". A VIEW is NOT here -- it is resolved
// through its own path, which already reports the same error.
func itemHasNoIndexChain(p *ReadOnlyPager, it FromItem) bool {
	if it.Subquery != nil || it.TableFunc {
		return true
	}
	if it.Table == "" || it.Schema != "" {
		return false // a schema-qualified FROM term can never be a CTE (select.c:5697)
	}
	_, isCTE := p.lookupCTE(it.Table)
	return isCTE
}
