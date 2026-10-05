// Compiles a FROM clause into nested cursor loops, one per table, so joins
// compose with WHERE, ORDER BY, aggregates and LIMIT the same way a single-table
// scan does. The body codegen (vdbe_scan.go, vdbe_sort_codegen.go,
// vdbe_agg_codegen.go) calls emitJoinLoops.
//
// Join semantics follow join.go:
//
//   - INNER/CROSS (including comma): the ON condition filters inline at its
//     level, with no NULL-extension. WHERE is applied at the innermost body.
//   - LEFT JOIN t ON <cond>: a per-left-row match flag starts at 0 and is set
//     when a right row satisfies <cond>. If it is still 0 after the right loop
//     (including an empty right table), the right cursor is put in the
//     OpNullRow state and the body runs once NULL-extended. WHERE is applied
//     afterwards, which is what makes "LEFT JOIN ... WHERE t2.b IS NULL" an
//     anti-join.
//
// Cursors are opened in FROM order (their numbers key every OpColumn), but the
// loops are nested in the order join.go's computeExecOrder returns (via
// joinPushdownPlan), the same plan that drives WHERE pushdown. Reordering does
// not change which rows come out, but it does change the order of an unordered
// result, so the exact order must be reproduced.
package engine

import (
	"errors"
	"fmt"
	"strings"
)

// joinSource is one FROM item ready for codegen: its resolved table, its
// compileScope (name/cols/colIndex/cursor -- tableScope.offset is set to this
// item's running column offset across every source, exactly like join.go's
// resolveFrom/buildScopes compute, since the aggregate
// planner (planNoGroupAggregate/planGroupByStmt) and its resulting rewritten
// expression trees need a real evalCtx with correctly offset-adjusted
// tables/vals -- see vdbe_agg.go), and how it's joined onto the sources
// before it.
type joinSource struct {
	tbl   *resolvedTable
	scope compileScope
	left  bool // LEFT JOIN: needs match-flag + NullRow handling (see this file's package doc comment)
	on    Expr // join condition; nil for a comma/CROSS join with no ON

	// notIndexed is FromItem.NotIndexed: the "NOT INDEXED" hint was written.
	// whereLoopAddBtree chains a table's real indexes onto its fake sPk only
	// when "pSrc->fg.notIndexed==0" (where.c:4070), so the hint takes every
	// SECONDARY index away -- the INTEGER PRIMARY KEY stays, which is why only
	// the index seek consults this and detectRowidSeekKey does not. The
	// where_plan gate already honours it (wherePlanIndexList); the seek
	// detectors are a separate, older mechanism that did not.
	notIndexed bool

	// cross is FromItem.CrossKeyword: the CROSS keyword was written. Semantic
	// no-op, planner barrier -- see that field's doc comment and where_plan.go.
	cross bool

	// updateTarget is FromItem.UpdateTarget: this source is the synthetic,
	// comma-joined stand-in update_from.go/view_trigger.go append LAST for
	// UPDATE...FROM's own target table (or an INSTEAD OF UPDATE trigger's
	// view). See that field's doc comment.
	updateTarget bool

	// flattened is FromItem.flattened: this table came up out of a view, CTE or
	// FROM-subquery body, and its OpOpenRead carries P5 bit 2.
	flattened bool

	// wherePlanOK marks a source the ported planner (where_plan.go) reasons about
	// exactly; set only by markWherePlanEligibility, so other paths keep the
	// default loop order. colUsed/colUsedOK are SrcItem.colUsed: the columns the
	// statement references, which decide an automatic index's covering columns
	// (wherePlanAutoIndexKey). colUsedOK false declines that index's key.
	wherePlanOK bool
	colUsed     uint64
	colUsedOK   bool

	// noAutoIndex is "PRAGMA automatic_index is OFF" -- whereLoopAddBtree's
	// "(pParse->db->flags & SQLITE_AutoIndex)!=0" guard, the one input the ported
	// planner takes from the CONNECTION rather than from the statement. Set by
	// markWherePlanEligibility (the only place holding the pager) on every item
	// alike, and consumed by sqliteExecOrder, which owns the rule.
	noAutoIndex bool

	// idxOrderKey is the key of the real index C scans this single-table source
	// through, so rows arrive in index order (markWherePlanIndexEligibility,
	// wherePlanSingleIndexKey). nil when the port declined or chose the table
	// scan. Delivered to emitJoinLoops as plan.autoIdxKeys[0], the same
	// OpAutoIndexOrder channel as the automatic index.
	idxOrderKey *autoIndexKey

	// onToWhere marks an INNER item whose ON clause was moved into the WHERE, as
	// sqlite3ProcessJoin does before planning. Its conjuncts are bucketed by
	// planJoinPushdown at the earliest depth binding every table they name, which
	// lets the planner bind this item before a table its ON references. Set only
	// for items the ported planner governs. `on` stays set: the planner builds its
	// term list from it in C's order.
	onToWhere bool

	// rightOuter is true for a RIGHT or FULL JOIN target -- see join.go's
	// joinedTable.rightOuter (the same semantics, mirrored here for the
	// VDBE codegen) and this file's emitRightOuterSweep.
	rightOuter bool

	// derived, when non-nil, marks a derived table (FromItem.Subquery): the
	// compiled sub-Program whose rows are this source's. emitJoinLoops emits
	// OpOpenDerived, which runs it once (derivedSlot cache) and feeds an in-memory
	// cursor (vdbe_cursor.go). tbl is synthetic, with the output columns only.
	derived     *Program
	derivedSlot int

	// keepSubtype, for a subquery, view or CTE source, is the result columns
	// whose subtype crosses into this query (flattenedSubtypeKeep); nil
	// clears every one.
	keepSubtype []bool

	// derivedOrderProvable is set by resolveDerivedSource when the subquery is
	// "SELECT ... FROM <one plain base table>" with nothing else, so its row order
	// is exactly what wherePlanSingleTableIndexOrder decided for that table. It is
	// decided inside the CTE scope window, never re-derived from a bare name later.
	// Read only by anchorNoIndexInPlay.
	//
	// derivedOrderNeedsOuterSafety is set when that proof rests on a real index: C
	// flattens the subquery (select.c:4290) and substitutes outer WHERE/ON/GROUP
	// BY/ORDER BY references into it (select.c:4572-4704), which can change the
	// chosen index. anchorNoIndexInPlay must then check that no outer clause folds
	// a predicate in.
	derivedOrderProvable         bool
	derivedOrderNeedsOuterSafety bool

	// vtabItem, when non-nil, marks a virtual-table source: a table-valued function
	// call, a persisted CREATE VIRTUAL TABLE, or an eponymous module. Exclusive
	// with derived; opened through OpOpenDerived's run-once slot, whose vtab branch
	// calls materializeVtab (runVtabOnce). Unlike a derived table it exposes a
	// real rowid, so noRowid stays false and the cursor gets materializeVtab's
	// rowids.
	vtabItem *FromItem

	// vtabDrivesBestIndex reports whether the rows really come from the module's
	// BestIndex/Filter cursor rather than a row source materializeVtab serves first
	// (fts shadow tables, rtree's b-tree). False when vtabItem is nil. Read by
	// annotateVtabCorrelations, which must not push a constraint the source would
	// ignore.
	vtabDrivesBestIndex bool

	// vtabCorr, when non-empty, marks this virtual-table source as CORRELATED:
	// the listed WHERE conjuncts' right-hand sides name tables bound strictly
	// earlier in the loop nesting, so the module is driven once per outer row
	// from inside the join (emitVtabCorrelatedOpen) instead of once per
	// execution above it. Set only by annotateVtabCorrelations
	// (vtab_correlated.go); nil for every source that engine ever emitted
	// before that file existed.
	vtabCorr []vtabCorrTerm

	// vtabOmitCandidate is 1 + the index into vtabItem.tvfWhere of the conjunct the
	// module says BestIndex will consume (omittingVtabModule), 0 otherwise (all but
	// fts3tokenize). Only a candidate: compileScanAttempt decides (vtabOmitConjuncts).
	// Computed here against the item's owning pager.
	vtabOmitCandidate int

	// isFtsVtab reports whether vtabItem is specifically a PERSISTED fts3/
	// fts4/fts5 virtual table reference (isFtsVtabSource, vdbe_agg_codegen.go
	// -- never a table-valued function call or any other vtab module),
	// computed once at resolveVtabSource time against the item's own owning
	// catalog. Always false when vtabItem is nil. Read by join.go's
	// ftsMatchForcedOrder/computeExecOrder to recognize the one JOIN LOOP
	// NESTING shape C SQLite's MATCH-constraint prerequisite mask FORCES
	// rather than costs -- see that function's doc comment.
	isFtsVtab bool

	// catalogScope, when not scopeAny, marks a MATERIALIZED SCHEMA CATALOG
	// source -- the two states a plain b-tree scan cannot answer: an empty temp
	// catalog (no temp database open) and a CREATE TABLE ... AS SELECT's own
	// reserved row. See resolveSchemaCatalogSource (temp_catalog.go).
	catalogScope schemaScope

	// cteItem, when non-nil, marks a CTE reference, opened through OpOpenDerived:
	// a recursive CTE opens its queue program (cteRef.queue) and a recursive arm's
	// self-reference the current row (cteRef.recSelf); an ordinary CTE calls
	// resolveCTERows (runCTEOnce). Its schema is resolved at compile time
	// (resolveCTESource). Exclusive with derived/vtabItem; no rowid.
	cteItem *cteRef

	// seekKeyExpr, when non-nil, is a column-free expression E such that a WHERE
	// conjunct pins this rowid table's rowid to E: emitJoinLoops evaluates it once
	// after OpOpenRead and emits OpSeekRowidHint. Set by detectRowidSeekKey for at
	// most one source per compile.
	seekKeyExpr Expr

	// idxSeek, when non-nil, requests a SECONDARY-INDEX leading-column equality
	// seek for this (ordinary rowid) table: emitJoinLoops evaluates its
	// keyExpr once, right after this source's OpOpenRead, and emits an
	// OpSeekIndexHint so the cursor materializes only the rows whose indexed
	// leading column equals the key (found via the index b-tree) instead of
	// full-scanning. Set only by compileScanPlain's detectIndexSeekKey, and only
	// when seekKeyExpr (the strictly-cheaper single-row rowid lookup) did NOT
	// apply to this same source. See indexSeekPlan (vdbe_scan.go).
	idxSeek *indexSeekPlan

	// joinSeekKeyExpr / joinIdxSeek are the correlated (inner join side) versions of
	// seekKeyExpr / idxSeek: the key comes from tables bound earlier and is
	// re-evaluated per outer row, just before this level's OpRewind, with the
	// re-seek flag (P3==1). Set by annotateJoinSeeks for INNER or LEFT sources (a
	// LEFT one keys only off its ON conjuncts, as disableTerm does). The join
	// condition is still re-checked per row, so it only restricts candidates, and
	// rows come in rowid order. The rowid key wins when both apply.
	joinSeekKeyExpr Expr
	joinIdxSeek     *indexSeekPlan

	// memberScopes, non-nil only for a materialized parenthesized join group
	// (resolveGroupSource), lists its leaf member scopes in FROM order, each on the
	// group's one derived cursor with its own columns (offset via colBase). scope is
	// memberScopes[0], the connector, so single-scope consumers (open/close, ON
	// match, NullRow, coalesce install) are unchanged; name-resolving consumers use
	// sourceScopes(s). nil for an ordinary source.
	memberScopes []compileScope

	// dbIdx is the pager this source's rows come from (dbIndexOf encoding): 0 for
	// the compile's own pager, i+1 for p.attachedReaders[i] when itemOwner routed
	// the item there. A base table passes it as OpOpenRead's P3. A view from a
	// foreign owner passes it as derivedSource.dbIdx, since its sub-Program was
	// compiled against that pager and must run against it. Ordinary derived
	// tables, CTEs and groups always use 0.
	dbIdx int
}

// sourceScopes returns s's own column-resolution scope(s): its memberScopes
// (one per LEAF member, in FROM order) when s is a materialized
// parenthesized join GROUP, or simply []compileScope{s.scope} for an
// ordinary (non-group) source. Every consumer that flattens srcs into a
// name-indexed scope list (joinScopes/tableScopesOf, emitJoinLevel's
// per-level ON-scoping, emitJoinSeekHint's seek-key scoping) must go through
// this rather than reading s.scope directly, or a reference to a group's
// OWN deep member (anything but its connector) would fail to resolve.
func sourceScopes(s joinSource) []compileScope {
	if len(s.memberScopes) > 0 {
		return s.memberScopes
	}
	// Carry dbIdx down onto the scope -- the one thing an expression compiler
	// needs to know about WHICH database a source came from (see
	// tableScope.dbIdx). A materialized join GROUP never contains a virtual
	// table (resolveGroupSource declines one), so its member scopes need none.
	sc := s.scope
	sc.dbIdx = s.dbIdx
	return []compileScope{sc}
}

// groupPresent reports whether any source is a materialized join group. Callers
// then use conservative strategies, e.g. buildJoinPlan skips reordering and
// pushdown, as join.go does with groups.
func groupPresent(srcs []joinSource) bool {
	for _, s := range srcs {
		if len(s.memberScopes) > 0 {
			return true
		}
	}
	return false
}

// identityOrder returns the identity permutation [0, 1, ..., n-1] -- the
// execOrder a parenthesized join GROUP's own internal evaluation always
// uses (compileGroupBody) and buildJoinPlan's group-present fallback uses
// (never reordered: see FromItem.GroupLen's doc comment and join.go's
// hasGroups-disables-reordering rule).
func identityOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	return order
}

// buildJoinPlan computes the join plan for where: joinPushdownPlan's reordering
// and per-level pushdown, except when a join group is present, where it returns
// identity order with everything deferred to the innermost body (as join.go
// does). planJoinPushdown assumes one scope per source, which groups break.
// Both are optimizations, so this is never wrong.
func buildJoinPlan(srcs []joinSource, where Expr) joinPlan {
	if groupPresent(srcs) {
		return joinPlan{
			execOrder: identityOrder(len(srcs)),
			buckets:   make([][]Expr, len(srcs)),
			deferred:  splitTopLevelAnd(where),
		}
	}
	return joinPushdownPlan(srcs, tableScopesOf(srcs), where)
}

// resolveJoinSources resolves every FROM item into a joinSource, allocating one
// cursor per item in FROM order. USING/NATURAL conditions are desugared by the
// same desugarJoinItem and joinDesugarState join.go uses. A shape this compiler
// does not handle is errVDBEUnsupported; a real C rejection (missing table,
// INDEXED BY a missing index, a USING column absent from one side) is
// errVDBESemantic.
func resolveJoinSources(p *ReadOnlyPager, c *compiler, items []FromItem) ([]joinSource, error) {
	if p == nil {
		// A nil pager is reachable: a write program's subquery uses whatever snapshot
		// the statement installed, and one with a FROM needs a pager or p.attachedReaders
		// below would panic (seen from a trigger body). Decline instead.
		return nil, fmt.Errorf("%w: a subquery reading a table has no database snapshot to read from in this write-path expression", errVDBEUnsupported)
	}
	if err := p.checkDerivedJoinSupported(items); err != nil {
		return nil, declineOrSemantic(err)
	}
	if err := checkSelfJoinGroupSupported(items); err != nil {
		return nil, declineOrSemantic(err)
	}
	// absToSrcs maps the absolute FROM index that starts each top-level source (an
	// item, or a join group's connector) to its position in srcs, which is no longer
	// 1:1 with items once a group collapses its span. desugarJoinItem names
	// representatives by absolute index, so srcs[repr] lookups go through this map.
	absToSrcs := make(map[int]int, len(items))
	var srcs []joinSource
	offset := 0
	var state joinDesugarState
	for i := 0; i < len(items); {
		it := items[i]
		// NestedGroupSpan is an extra trigger alongside GroupLen > 0: an item can be both
		// (x in "((t1 JOIN t2) AS x JOIN t2x ON x.b=t2x.b) AS y"). GroupLen is 0 with a
		// NestedGroupSpan when resolveGroupSource re-enters the connector (it clears
		// GroupLen) or for a lone re-wrapped group. Either way one item slot is
		// consumed.
		if it.GroupLen > 0 || it.NestedGroupSpan != nil {
			gs, gerr := p.resolveGroupSource(c, &state, i, items)
			if gerr != nil {
				return nil, gerr
			}
			// Member scopes come back with offsets within the group's own row; rebase them
			// into this join's flat row, which tableScope.offset consumers (the aggregate
			// drain above all) index. Otherwise a non-leading group overwrote earlier
			// sources' slots ("u JOIN (t JOIN w USING(a))", "GROUP BY u.c").
			if len(gs.memberScopes) > 0 && offset > 0 {
				ms := make([]compileScope, len(gs.memberScopes))
				copy(ms, gs.memberScopes)
				for k := range ms {
					ms[k].offset += offset
				}
				gs.memberScopes = ms
				gs.scope.offset += offset
			}
			// GroupRebuildID, not just "is a group": the parser stamps it where C rebuilds
			// the group as an SF_NestedFrom sub-select (aliased or non-leading,
			// parse.y:777-816). A leading unaliased group keeps declared names even when
			// materialized; stamping every materialized group renamed columns C does not
			// (selectD.test).
			if it.GroupLen > 0 && it.GroupRebuildID != 0 && len(gs.memberScopes) == it.GroupLen {
				annotateMaterializedGroupColNames(items[i:i+it.GroupLen], gs.memberScopes)
			}
			absToSrcs[i] = len(srcs)
			srcs = append(srcs, gs)
			// See the base-table branch's identical RIGHT/FULL coalesce-
			// fallback wiring below: a group's own CONNECTOR can itself be a
			// USING/NATURAL RIGHT/FULL JOIN target (e.g. "t1 RIGHT JOIN (t2
			// JOIN t3 USING(a)) USING(a)"), and gs.scope.coalesced (set by
			// resolveGroupSource to the connector's own outer-facing hidden
			// map, exactly like an ordinary item's) drives it identically.
			if it.Join == JoinRight || it.Join == JoinFull {
				installSrcCoalesceFallback(srcs, absToSrcs, gs.scope.coalesced, i)
			}
			offset += len(gs.tbl.cols)
			consumed := it.GroupLen
			if consumed == 0 {
				consumed = 1
			}
			i += consumed
			continue
		}
		// "INDEXED BY <name>" on anything but an ordinary table is "no such index": a
		// CTE, derived table or table-valued function has an empty index chain
		// (select.c:5487). Checked before the branch dispatch, since those branches
		// return early. "NOT INDEXED" never sets IndexedBy.
		if it.IndexedBy != "" && itemHasNoIndexChain(p, it) {
			return nil, semanticf("engine: no such index: %s", it.IndexedBy)
		}
		// A table-valued function call ("generate_series(1,10)", vtab.go)
		// resolves to a materialized virtual-table row source -- see
		// resolveVtabSource and joinSource.vtabItem's doc comment. TableFunc
		// items never carry a database qualifier of their own (FromItem.Schema
		// is meaningless on one, mirroring the derived-subquery branch just
		// below), so p itself is always the owning pager.
		if it.TableFunc {
			vs, verr := p.resolveVtabSource(c, &state, i, it, offset)
			if verr != nil {
				return nil, verr
			}
			absToSrcs[i] = len(srcs)
			srcs = append(srcs, vs)
			if it.Join == JoinRight || it.Join == JoinFull {
				installSrcCoalesceFallback(srcs, absToSrcs, vs.scope.coalesced, i)
			}
			offset += len(vs.tbl.cols)
			i++
			continue
		}
		if it.Subquery != nil {
			// A derived table is always compiled (and later run) against THIS
			// SAME pager p -- a subquery FROM item can never carry a database
			// qualifier of its own (FromItem.Schema is meaningless on it), so
			// there is no owner other than p to route it to; a cross-database
			// read INSIDE the derived subquery's own FROM is handled by that
			// nested resolveJoinSources call seeing the SAME p.attachedReaders
			// (see resolveDerivedSource -> compileSubProgram(p, ...)).
			ds, derr := p.resolveDerivedSource(c, &state, i, it, offset)
			if derr != nil {
				return nil, derr
			}
			ds.keepSubtype = flattenedSubtypeKeep(c.flatOuter, items, i, it.Subquery, len(ds.tbl.cols))
			absToSrcs[i] = len(srcs)
			srcs = append(srcs, ds)
			// Mirror the base-table branch's RIGHT/FULL coalesce-fallback wiring
			// below (a derived table can be a USING/NATURAL RIGHT/FULL JOIN
			// target too), reading the desugared coalescing off the source's
			// own scope.
			if it.Join == JoinRight || it.Join == JoinFull {
				installSrcCoalesceFallback(srcs, absToSrcs, ds.scope.coalesced, i)
			}
			offset += len(ds.tbl.cols)
			i++
			continue
		}
		// Resolve which pager owns this item: p for a local reference, or for a
		// cross-database read the attached database owning a foreign-qualified or
		// attached-only name (itemOwner, as join.go's resolveFrom does). rp is used for
		// every lookup below and dbIdx is recorded for the VM.
		//
		// A CTE exists only on p and is checked first, so it shadows a same-named
		// attached table; a schema-qualified name is never a CTE (select.c:5697).
		// resolveCTESource builds an OpOpenDerived source and returns immediately.
		schemaQualified := it.Schema != ""
		if !schemaQualified {
			if cteBind, ok := p.lookupCTE(it.Table); ok {
				cs, cerr := p.resolveCTESource(c, &state, i, it, cteBind, offset)
				if cerr != nil {
					return nil, cerr
				}
				// A MATERIALIZED CTE is an optimization fence (select.c:7797).
				if cteBind.recursive == nil && cteBind.m10d != cteM10dYes {
					cs.keepSubtype = flattenedSubtypeKeep(c.flatOuter, items, i, cteBind.core, len(cs.tbl.cols))
				}
				absToSrcs[i] = len(srcs)
				srcs = append(srcs, cs)
				if it.Join == JoinRight || it.Join == JoinFull {
					installSrcCoalesceFallback(srcs, absToSrcs, cs.scope.coalesced, i)
				}
				offset += len(cs.tbl.cols)
				i++
				continue
			}
		}
		rp := p
		dbIdx := 0
		if schemaQualified || len(p.attachedReaders) > 0 {
			owner, oerr := p.itemOwner(it)
			if oerr != nil {
				// A genuine C-SQLite rejection (an unresolvable database
				// qualifier) -- errVDBESemantic, exactly like the resolveTable
				// failure below, so it PROPAGATES as the statement's real
				// error instead of being masked behind the generic
				// "not compilable to bytecode" fallback message.
				return nil, semanticf("%v", oerr)
			}
			rp = owner
			dbIdx = p.dbIndexOf(owner)
		}

		// INDEXED BY changes no plan here, but the index must exist on the owning pager
		// (checkIndexExists), as errVDBESemantic.
		if it.IndexedBy != "" {
			if ierr := rp.checkIndexExists(it.IndexedBy, it.Table, fromItemScope(it)); ierr != nil {
				return nil, semanticf("%v", ierr)
			}
		}

		// A SCHEMA CATALOG read. Checked before resolveTable, which has no row
		// for sqlite_temp_master at all and deliberately declines sqlite_master
		// while a temp object exists: this engine keeps both catalogs in ONE
		// schema b-tree (temp_schema.go), so the only exact answer is the
		// filtered row source resolveSchemaCatalogSource builds
		// (temp_catalog.go).
		if cscope, isCat, terr := rp.schemaCatalogSourceScope(it); terr != nil {
			return nil, semanticf("%v", terr)
		} else if isCat {
			cs, cserr := rp.resolveSchemaCatalogSource(c, &state, i, it, offset, cscope)
			if cserr != nil {
				return nil, cserr
			}
			cs.dbIdx = dbIdx
			srcs = append(srcs, cs)
			absToSrcs[i] = len(srcs) - 1
			offset += len(cs.tbl.cols)
			i++
			continue
		}

		// A persisted CREATE VIRTUAL TABLE (vtab.go) resolves to a materialized
		// virtual-table row source, exactly like the TableFunc branch above --
		// see resolveVtabSource. Checked before resolveTable so its type="table"
		// schema row is never mis-parsed as an ordinary CREATE TABLE.
		if isV, verr := rp.isVtabItem(it); verr != nil {
			return nil, declineOrSemantic(verr)
		} else if isV {
			vs, vserr := rp.resolveVtabSource(c, &state, i, it, offset)
			if vserr != nil {
				return nil, vserr
			}
			vs.dbIdx = dbIdx
			absToSrcs[i] = len(srcs)
			srcs = append(srcs, vs)
			if it.Join == JoinRight || it.Join == JoinFull {
				installSrcCoalesceFallback(srcs, absToSrcs, vs.scope.coalesced, i)
			}
			offset += len(vs.tbl.cols)
			i++
			continue
		}
		// A view resolves to a derived source from its stored SELECT, against its owning
		// pager rp, so its unqualified names bind in its own database. Checked before
		// resolveTable, which would report "no such table" for a view.
		if pcv, ok, verr := rp.resolveViewByNameIn(fromItemScope(it), it.Table); verr == nil && ok {
			// A VIEW has no b-tree, so an "INDEXED BY" hint on one can never
			// name a usable index -- C SQLite reports "no such index: <n>"
			// even when an index of that name exists on some other table.
			// Verified directly (indexedby.test).
			if it.IndexedBy != "" {
				return nil, fmt.Errorf("engine: no such index: %s", it.IndexedBy)
			}
			vs, vserr := rp.resolveViewSource(c, &state, i, it, pcv, offset)
			if vserr != nil {
				return nil, vserr
			}
			vs.keepSubtype = flattenedSubtypeKeep(c.flatOuter, items, i, pcv.selectStmt, len(vs.tbl.cols))
			// dbIdx tells OpOpenDerived which pager to run this view's own
			// compiled sub-Program against -- it was compiled with rp (via
			// resolveViewSource's compileSubProgram(p==rp, ...) call), whose
			// own internal OpOpenRead db-indices are 0-relative to RP, so the
			// sub-Program must also be EXECUTED with rp, not the enclosing
			// statement's pager (see this file's joinSource.dbIdx doc
			// comment and OpOpenDerived's body, vdbe.go).
			vs.dbIdx = dbIdx
			absToSrcs[i] = len(srcs)
			srcs = append(srcs, vs)
			// Mirror the derived-subquery branch's RIGHT/FULL coalesce-fallback
			// wiring above: a view CAN be a USING/NATURAL RIGHT/FULL JOIN target
			// ("t1 NATURAL RIGHT JOIN v", gated in
			// compat-harness/derived_natural_join_diff_test.go), so
			// vs.scope.coalesced is genuinely populated here and this wiring is
			// load-bearing, not merely symmetric.
			if it.Join == JoinRight || it.Join == JoinFull {
				installSrcCoalesceFallback(srcs, absToSrcs, vs.scope.coalesced, i)
			}
			offset += len(vs.tbl.cols)
			i++
			continue
		}
		tbl, terr := rp.resolveTableIn(fromItemScope(it), it.Table)
		if terr != nil {
			// A BARE reference to an eponymous virtual-table module (vtab.go),
			// resolved only after an ordinary table AND a view (checked above)
			// both fail to match -- so a real table/view of the same name
			// always shadows the module, mirroring resolveFrom (join.go)
			// exactly.
			if isEponymousVtabName(it.Table) {
				vs, vserr := rp.resolveVtabSource(c, &state, i, it, offset)
				if vserr != nil {
					return nil, vserr
				}
				vs.dbIdx = dbIdx
				absToSrcs[i] = len(srcs)
				srcs = append(srcs, vs)
				if it.Join == JoinRight || it.Join == JoinFull {
					installSrcCoalesceFallback(srcs, absToSrcs, vs.scope.coalesced, i)
				}
				offset += len(vs.tbl.cols)
				i++
				continue
			}
			// resolveTable's error is normally a genuine "no such table", so it is
			// errVDBESemantic and reaches the user as the statement's error.
			return nil, semanticf("%v", terr)
		}
		name := it.Alias
		if name == "" {
			name = it.Table
		}
		// tableName: real underlying table name for full_column_names naming
		// (tableScope.tableName) -- mirrors join.go's buildScopes wiring.
		tableName := it.Table
		if tableName == "" {
			tableName = name
		}
		onExpr, hidden, derr := desugarJoinItem(&state, i, it, name, tbl.cols)
		if derr != nil {
			return nil, declineOrSemantic(derr)
		}
		cursor := c.allocCursor()
		// noRowid: tbl.withoutRowid -- see join.go's identical wiring
		// (buildScopes) and query.go's resolvedTable.withoutRowid doc
		// comment: a WITHOUT ROWID table has no rowid/oid/_rowid_
		// pseudo-column at all.
		ts := tableScope{name: name, tableName: tableName, cols: tbl.cols, colIndex: buildColIndex(tbl.cols), offset: offset, coalesced: hidden, nestedNonLeading: it.NestedNonLeading, noRowid: tbl.withoutRowid}
		thisSrc := joinSource{
			tbl:          tbl,
			scope:        compileScope{tableScope: ts, cursor: cursor},
			left:         it.Join == JoinLeft || it.Join == JoinFull,
			rightOuter:   it.Join == JoinRight || it.Join == JoinFull,
			cross:        it.CrossKeyword,
			notIndexed:   it.NotIndexed,
			on:           onExpr,
			dbIdx:        dbIdx,
			updateTarget: it.UpdateTarget,
			flattened:    it.flattened,
		}
		absToSrcs[i] = len(srcs)
		srcs = append(srcs, thisSrc)
		// See join.go's installCoalesceFallback: a RIGHT/FULL-joined
		// source's own USING/NATURAL representative(s) (repr, in hidden --
		// always an EARLIER, already-built source) need this source's own
		// index recorded as their coalesce fallback, so an unqualified
		// reference to the common column reads COALESCE(representative,
		// this source) instead of the representative's raw (possibly NULL)
		// value alone -- see resolveInScopes/emitColumnReadCoalesce.
		if it.Join == JoinRight || it.Join == JoinFull {
			installSrcCoalesceFallback(srcs, absToSrcs, hidden, i)
		}
		offset += len(tbl.cols)
		i++
	}
	// RIGHT/FULL JOIN is supported in any position and number in a flat FROM (see
	// emitJoinLoops and emitRightOuterSweep). A join group's alias is stamped onto
	// its members' scopes here in one pass; memberScopes carries tableScope by
	// value from the group's own resolution.
	for i, it := range items {
		if it.GroupAlias == "" {
			continue
		}
		if k, ok := absToSrcs[i]; ok {
			srcs[k].scope.groupAlias = it.GroupAlias
		}
	}
	if err := checkRebuiltJoinGroups(items, itemScopes(items, absToSrcs, srcs), c.groupSpanRecursion); err != nil {
		return nil, err
	}
	annotateNestedRowidNames(items, srcs, absToSrcs)
	annotateNestedColNames(items, srcs, absToSrcs)
	return srcs, nil
}

// annotateNestedColNames computes tableScope.nestedColNames for members of an
// aliased parenthesized join group. C rebuilds it as an SF_NestedFrom sub-select
// whose list selectExpander builds per member (select.c:6193-6217): the next
// member's USING columns, this member's columns, then its rowid alias; names
// are ":N"-suffixed by sqlite3ColumnsFromExprList. So over "(f1(b,a) JOIN
// f2(b,g) USING(b)) AS gq" the list is b, b:1, a, rowid, b:2, g, rowid:1 and
// "SELECT f1.b" is named "b:1". Only groups whose members are wrapped once,
// individually addressable and not NATURAL are stamped.
func annotateNestedColNames(items []FromItem, srcs []joinSource, absToSrcs map[int]int) {
	for i := 0; i < len(items); i++ {
		id := items[i].GroupRebuildID
		if id == 0 {
			continue
		}
		end := i + 1
		for end < len(items) && items[end].GroupRebuildID == id {
			end++
		}
		annotateOneNestedColGroup(items[i:end], i, srcs, absToSrcs)
		i = end - 1
	}
}

func annotateOneNestedColGroup(members []FromItem, base int, srcs []joinSource, absToSrcs map[int]int) {
	// An ALIAS is only one of the two things that makes SQLite rebuild a group
	// (being written anywhere but the leading position is the other), and
	// GroupRebuildID is stamped for both -- so this used to require an alias and
	// left every unaliased rebuilt group naming its columns as declared.
	// Re-probed against 3.53.3: "SELECT w.a FROM u JOIN (t JOIN w ON t.a=w.a)"
	// is "a:1" with no alias anywhere, exactly as the aliased spelling is.
	scopes := make([]*tableScope, len(members))
	for k, m := range members {
		si, ok := absToSrcs[base+k]
		if !ok || si < 0 || si >= len(srcs) || len(srcs[si].memberScopes) > 0 ||
			m.NestFromWrapDepth != 1 {
			return
		}
		scopes[k] = &srcs[si].scope.tableScope
	}
	nestedColNamesInto(members, scopes)
}

// annotateMaterializedGroupColNames applies the same naming to a group this
// engine materializes as one joinSource ("SELECT t.a FROM u JOIN (t JOIN w
// USING(a))" is "a:1"). Members it cannot place one-to-one (a nested group, a
// subquery or table function, NATURAL) keep declared names.
func annotateMaterializedGroupColNames(members []FromItem, ms []compileScope) {
	if len(members) == 0 || len(members) != len(ms) {
		return
	}
	scopes := make([]*tableScope, len(ms))
	for k := range ms {
		m := members[k]
		if m.NestedGroupSpan != nil || m.Subquery != nil || m.TableFunc || m.Table == "" {
			return
		}
		scopes[k] = &ms[k].tableScope
	}
	nestedColNamesInto(members, scopes)
}

// nestedColNamesInto stamps tableScope.nestedColNames on each of a rebuilt
// group's member scopes, following selectExpander's own SF_NestedFrom list
// construction (select.c:6193-6217): the NEXT member's USING columns first,
// then this member's own columns, then its rowid alias when it has a visible
// rowid, with sqlite3ColumnsFromExprList's ":N" disambiguation over the whole
// list (select.c:2293-2306).
func nestedColNamesInto(members []FromItem, scopes []*tableScope) {
	var names []string
	start := make([]int, len(members))
	for k, ts := range scopes {
		if k+1 < len(members) {
			names = append(names, groupJoinCommonNames(members[k+1], scopes[:k+1], scopes[k+1])...)
		}
		start[k] = len(names)
		for _, col := range ts.cols {
			if col.Hidden {
				// Hidden columns are left out of the list (select.c:6245)
				// and so shift every later position; not modelled.
				return
			}
			names = append(names, col.Name)
		}
		if !ts.noRowid {
			if alias := rowidAliasCandidate(ts.cols); alias != "" {
				names = append(names, alias)
			}
		}
	}
	uniq, _ := r32mUniqueColumnNames(names)
	common := map[string]bool{}
	for k := range members {
		if k+1 < len(members) {
			for _, n := range groupJoinCommonNames(members[k+1], scopes[:k+1], scopes[k+1]) {
				common[r33sFoldIdent(n)] = true
			}
		}
	}
	for k, ts := range scopes {
		ts.nestedColNames = uniq[start[k] : start[k]+len(ts.cols)]
		// A common column's UNSUFFIXED name belongs to the synthetic entry,
		// which is the one a bare "*" shows -- see tableScope.nestedStarNames.
		star := make([]string, len(ts.cols))
		for i, col := range ts.cols {
			if common[r33sFoldIdent(col.Name)] {
				star[i] = col.Name
			} else {
				star[i] = ts.nestedColNames[i]
			}
		}
		ts.nestedStarNames = star
	}
}


// groupJoinCommonNames is the column names the join between members k and k+1
// makes common: USING's list, or for NATURAL every name k+1 shares with any
// member to its left (select.c:379). selectExpander adds one entry per name
// ahead of the member's columns (select.c:6194), counting toward ":N".
func groupJoinCommonNames(it FromItem, left []*tableScope, right *tableScope) []string {
	if len(it.Using) != 0 {
		return it.Using
	}
	if !it.Natural {
		return nil
	}
	has := make(map[string]bool, len(right.cols))
	for _, c := range right.cols {
		has[r33sFoldIdent(c.Name)] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, ts := range left {
		for _, c := range ts.cols {
			l := r33sFoldIdent(c.Name)
			if has[l] && !seen[l] {
				seen[l] = true
				out = append(out, c.Name)
			}
		}
	}
	return out
}

// annotateNestedRowidNames computes tableScope.nestedRowidDisplayName for
// every eligible member of a parenthesized join GROUP (FromItem.
// GroupRebuildID > 0) -- see that field's doc comment for the narrow,
// verified-safe case it covers, and columnRefNameParts (query.go) for the
// consumer. Groups are found by the same contiguous-GroupRebuildID scan
// checkRebuiltJoinGroups uses; anything this function is not sure about it
// simply leaves alone (nestedRowidDisplayName stays "", i.e. still decline).
func annotateNestedRowidNames(items []FromItem, srcs []joinSource, absToSrcs map[int]int) {
	for i := 0; i < len(items); i++ {
		id := items[i].GroupRebuildID
		if id == 0 {
			continue
		}
		end := i + 1
		for end < len(items) && items[end].GroupRebuildID == id {
			end++
		}
		annotateOneNestedRowidGroup(items[i:end], i, srcs, absToSrcs)
		i = end - 1
	}
}

// annotateOneNestedRowidGroup handles one GroupRebuildID span, which can mix
// wrap depths ("q1 JOIN (q2 JOIN (q3 JOIN q4))"): C exposes q2's rowid as a
// literal while q3/q4's do not resolve, so naming is decided per member
// (NestFromWrapDepth == 1). The whole span bails if a member has no individual
// joinSource (a materialized group). Aliased groups are skipped: their rowid
// does not resolve here anyway.
func annotateOneNestedRowidGroup(members []FromItem, base int, srcs []joinSource, absToSrcs map[int]int) {
	if members[0].HasGroupAlias {
		return
	}
	scopes := make([]*tableScope, len(members))
	for k := range members {
		si, ok := absToSrcs[base+k]
		if !ok || si < 0 || si >= len(srcs) || len(srcs[si].memberScopes) > 0 {
			return
		}
		scopes[k] = &srcs[si].scope.tableScope
	}
	// seen: names already placed by earlier members (columns, then a depth-1
	// member's rowid alias; select.c ~6228 does not re-expose a nested rowid). A
	// later member colliding with one would get a ":N" suffix in C, so that member
	// is declined.
	seen := make(map[string]bool)
	for k, m := range members {
		ts := scopes[k]
		for _, col := range ts.cols {
			seen[r33sFoldIdent(col.Name)] = true
		}
		if ts.noRowid || m.NestFromWrapDepth != 1 {
			continue
		}
		cand := rowidAliasCandidate(ts.cols)
		if cand == "" {
			continue
		}
		lcand := r33sFoldIdent(cand)
		if !seen[lcand] {
			ts.nestedRowidDisplayName = cand
		}
		seen[lcand] = true
	}
}

// rowidAliasCandidate returns the literal token C SQLite's
// sqlite3RowidAlias (expr.c:3042-3062, sqlite3IsRowid immediately above it)
// would pick for a VisibleRowid table whose declared columns are cols: the
// first of "_ROWID_", "ROWID", "OID" (in that order) that does not equal,
// case-insensitively, any of cols' own names. "" if all three are shadowed
// (impossible for a real CREATE TABLE -- one table cannot declare all
// three -- but sqlite3RowidAlias itself returns NULL there, so this mirrors
// that rather than assume it can't happen).
func rowidAliasCandidate(cols []columnInfo) string {
	for _, opt := range [3]string{"_ROWID_", "ROWID", "OID"} {
		shadowed := false
		for _, c := range cols {
			if equalFoldName(c.Name, opt) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			return opt
		}
	}
	return ""
}

// installSrcCoalesceFallback records ownerItem, a RIGHT/FULL-joined item's
// absolute index, as a coalesce fallback owner on the representative its
// USING/NATURAL chose for each coalesced column (see
// tableScope.coalesceFallback). It must write both joinSource.scope and, for a
// materialized group, memberScopes[0]: they are struct copies that share the
// map only once it exists, so creating it through one left the other nil
// ("(t4 LEFT JOIN t5 USING(a)) RIGHT JOIN t1 USING(a)" read NULL).
func installSrcCoalesceFallback(srcs []joinSource, absToSrcs map[int]int, coalesced map[string]int, ownerItem int) {
	for lname, repr := range coalesced {
		k := absToSrcs[repr]
		if srcs[k].scope.coalesceFallback == nil {
			srcs[k].scope.coalesceFallback = make(map[string][]int, len(coalesced))
		}
		srcs[k].scope.coalesceFallback[lname] = append(srcs[k].scope.coalesceFallback[lname], ownerItem)
		if len(srcs[k].memberScopes) > 0 {
			srcs[k].memberScopes[0].coalesceFallback = srcs[k].scope.coalesceFallback
		}
	}
}

// itemScopes maps every FROM item index to its own scope, including members
// inside a group's span (absToSrcs stops at the span's start). A
// NestedGroupSpan slot contributes several leaves, so nestedGroupLeafCount /
// combinedMemberScope chunk memberScopes in resolveGroupSource's widths. A
// mismatch falls back to the group's first member scope, which can only make
// checkRebuiltJoinGroups decline less, never answer wrong.
func itemScopes(items []FromItem, absToSrcs map[int]int, srcs []joinSource) map[int]compileScope {
	out := make(map[int]compileScope, len(items))
	for i := 0; i < len(items); {
		k, ok := absToSrcs[i]
		if !ok {
			i++ // an item strictly inside a group span; its own start filled it in
			continue
		}
		gl := items[i].GroupLen
		ms := srcs[k].memberScopes
		if len(ms) == 0 {
			// An ordinary item, OR (gl==0, NestedGroupSpan!=nil) a
			// re-wrapped group's own opaque connector resolved as ONE
			// top-level slot by resolveGroupSource's own gl==0 branch
			// (finishGroupSource's doc comment) -- either way srcs[k] IS
			// this one position's own resolution.
			out[i] = srcs[k].scope
			i++
			continue
		}
		if gl == 0 {
			// The gl==0-but-materialized-group case: this ONE slot's own
			// declared columns are its WHOLE flattened member set, not just
			// ms[0] alone (mirrors C SQLite's own SF_NestedFrom rebuild,
			// which treats a nested-from subquery's flattened column list as
			// that ONE SrcItem's own declared columns) -- see
			// combinedMemberScope's own doc comment.
			out[i] = combinedMemberScope(ms)
			i++
			continue
		}
		consumed := 0
		for m := 0; m < gl && i+m < len(items); m++ {
			n := nestedGroupLeafCount(items[i+m])
			if n <= 0 || consumed+n > len(ms) {
				out[i+m] = ms[0] // this function's own long-standing safety net
				continue
			}
			out[i+m] = combinedMemberScope(ms[consumed : consumed+n])
			consumed += n
		}
		i += gl
	}
	return out
}

// nestedGroupLeafCount reports how many leaf member scopes an item contributes:
// 1 normally, or the recursive sum over a re-wrap stand-in's nested span. An
// item with GroupLen > 0 inside the same span still counts 1 (a pre-existing
// approximation; see itemScopes).
func nestedGroupLeafCount(it FromItem) int {
	if it.NestedGroupSpan == nil {
		return 1
	}
	n := 0
	for _, m := range it.NestedGroupSpan {
		n += nestedGroupLeafCount(m)
	}
	if n == 0 {
		return 1 // never observed (NestedGroupSpan is never empty); guards a 0-count fall-through above
	}
	return n
}

// combinedMemberScope merges the leaf scopes of one outer slot into one
// compileScope spanning all their columns, as C treats a nested-from subquery's
// flattened list as that one item's columns. Duplicates are kept so an
// enclosing census sees collisions; colIndex keeps the first. scopes is never
// empty.
func combinedMemberScope(scopes []compileScope) compileScope {
	if len(scopes) == 1 {
		return scopes[0]
	}
	out := scopes[0]
	cols := make([]columnInfo, 0, len(scopes))
	idx := make(map[string]int, len(scopes))
	for _, sc := range scopes {
		for _, col := range sc.cols {
			ln := r33sFoldIdent(col.Name)
			if _, dup := idx[ln]; !dup {
				idx[ln] = len(cols)
			}
			cols = append(cols, col)
		}
	}
	out.cols = cols
	out.colIndex = idx
	return out
}

// checkRebuiltJoinGroups declines a parenthesized join group whose column list
// C rebuilds (FromItem.GroupRebuildID: non-leading or aliased) in a way this
// compiler does not reproduce. This engine emits a group's columns in FROM
// order, which matches only the spliced (leading, unaliased) form. Measured
// over ta(a,b), td(b,g), tc(z), u1(b,p), u2(b,q), zc(m,n), zb(q,_ROWID_):
//
// (A) USING/NATURAL coalescing moves the surviving copy to the front, and
// stacked joins add a column:
//
//	SELECT * FROM (ta JOIN td USING(b))        -> cols  a b g   (spliced)
//	SELECT * FROM tc, (ta JOIN td USING(b))    -> cols  z b a g (rebuilt)
//	SELECT * FROM anchor, (o1 LEFT JOIN o2 USING(x) FULL JOIN o3 USING(y))
//	                                           -> 6 cols zz x y y:1 p r
//
// Two distinct coalesced names decline. One that does not already lead the
// group declines only where a "*" expands over the group
// (GroupRebuildStarred).
//
// (A2) A coalesced name that a member outside that USING also declares makes C
// refuse the statement ("ambiguous column name: k"), so it declines.
//
// (B) An aliased rebuilt group's repeated name is an error in C:
//
//	SELECT * FROM (u1 JOIN u2 ON 1)         -> cols b p b q
//	SELECT * FROM (u1 JOIN u2 ON 1) AS j    -> ambiguous column name: b
//
// (C) An aliased group whose members declare rowid/oid/_rowid_ collides with
// C's invisible rowid alias entries:
//
//	SELECT * FROM (zc JOIN zb ON 1)         -> cols m n q _ROWID_
//	SELECT * FROM (zc JOIN zb ON 1) AS g    -> no such column: _ROWID_:1
//
// declined in any member position.
//
// Not declined: an unaliased, uncoalesced repeated name, which C ":N"-renames
// and this engine names plainly (as errIfDuplicateOutputNames does); values,
// order and count match.
//
// deferFrontMove is true only for a group's internal resolution recursion
// (compiler.groupSpanRecursion); see checkOneRebuiltGroup.
func checkRebuiltJoinGroups(items []FromItem, scopes map[int]compileScope, deferFrontMove bool) error {
	// Members of one group are contiguous, so one pass in FROM order groups
	// them; ids are only ever compared for equality.
	for i := 0; i < len(items); i++ {
		id := items[i].GroupRebuildID
		if id == 0 {
			continue
		}
		end := i + 1
		for end < len(items) && items[end].GroupRebuildID == id {
			end++
		}
		if err := checkOneRebuiltGroup(items[i:end], scopes, i, deferFrontMove); err != nil {
			return err
		}
		i = end - 1
	}
	return nil
}

// checkOneRebuiltGroup applies (A), (A2), (B) and (C) to one group: members are
// items[0:], with absolute indexes from base. deferFrontMove skips (A): the
// recursive call has no outward connector context, and the outer call always
// runs afterwards with it (frontMoveUnobservable).
func checkOneRebuiltGroup(members []FromItem, scopes map[int]compileScope, base int, deferFrontMove bool) error {
	// (A). A member's USING/NATURAL always coalesces against an EARLIER member
	// of its own group -- the group's own connector, which attaches the WHOLE
	// group to what precedes it and could name a column outside, sits on the
	// group's FIRST member, which is why that one is skipped. ("SELECT * FROM
	// ta JOIN (td JOIN te ON 1) USING(b)" is served: the group itself
	// coalesces nothing.) A NATURAL join with NO column in common likewise
	// coalesces nothing and stays served.
	var coalesced []string
	// coalesceCount[n] is how many of the group's OWN joins coalesce n -- i.e.
	// how many copies of n the rebuilt list drops. (A2) below needs it to tell
	// a fully-coalesced name from one a further member still declares.
	coalesceCount := make(map[string]int)
	// firstConnectorCoalesces/onlyFirstConnector record which internal connectors
	// coalesce. C inserts a coalescing connector's representative before the left
	// member of that pair (select.c ~6193), which is the group's front only when
	// the first connector is the coalescing one; frontMoveUnobservable assumes
	// that.
	firstConnectorCoalesces := false
	onlyFirstConnector := true
	for k := 1; k < len(members); k++ {
		kCoalesces := false
		for _, u := range members[k].Using {
			coalesced = appendUniqueFold(coalesced, u)
			coalesceCount[r33sFoldIdent(u)]++
			kCoalesces = true
		}
		if members[k].Natural {
			for _, col := range scopes[base+k].cols {
				for e := 0; e < k; e++ {
					if _, shared := scopes[base+e].colIndex[r33sFoldIdent(col.Name)]; shared {
						coalesced = appendUniqueFold(coalesced, col.Name)
						coalesceCount[r33sFoldIdent(col.Name)]++
						kCoalesces = true
						break
					}
				}
			}
		}
		if !kCoalesces {
			continue
		}
		if k == 1 {
			firstConnectorCoalesces = true
		} else {
			onlyFirstConnector = false
		}
	}
	// (A2): a coalesced name also declared by a member outside that USING/NATURAL
	// survives the rebuild twice and C refuses the statement, though the leading
	// spliced spelling answers:
	//
	//	SELECT p, r FROM (g1 JOIN g2 USING(k) JOIN g3 ON 1)      -> answers
	//	SELECT p, r FROM tc, (g1 JOIN g2 USING(k) JOIN g3 ON 1)  -> ERROR
	//	                                          "ambiguous column name: k"
	for _, n := range coalesced {
		ln := r33sFoldIdent(n)
		declaring := 0
		for k := range members {
			if _, ok := scopes[base+k].colIndex[ln]; ok {
				declaring++
			}
		}
		if declaring > coalesceCount[ln]+1 {
			return fmt.Errorf("%w: parenthesized join group in a position SQLite rebuilds the column list for, whose coalesced column %q is also declared by a member outside that USING/NATURAL (SQLite reports \"ambiguous column name\" there)", errVDBEUnsupported, n)
		}
	}
	if len(coalesced) > 0 {
		// The rebuilt list puts coalesced columns first, matching FROM order only when
		// the one coalesced column already leads. Two distinct coalesced names produce
		// an extra column (zz,x,y,y:1,p,r), so they decline. Stacking USING on the same
		// name matches.
		if len(coalesced) > 1 {
			return fmt.Errorf("%w: parenthesized join group coalescing %s in a position SQLite rebuilds the column list for (two distinct coalesced names add a copy this engine has no column for, and the second name is then ambiguous)", errVDBEUnsupported, strings.Join(coalesced, ","))
		}
		first := ""
		if cols := scopes[base].cols; len(cols) > 0 {
			first = r33sFoldIdent(cols[0].Name)
		}
		if r33sFoldIdent(coalesced[0]) != first && members[0].GroupRebuildStarred && !deferFrontMove {
			// Unless deferred, decline unless the first connector is the only coalescing one
			// and frontMoveUnobservable proves the move invisible; otherwise the insertion
			// point is mid-list and the check would test the wrong column.
			if !firstConnectorCoalesces || !onlyFirstConnector || !frontMoveUnobservable(members[0], scopes, base, first) {
				return fmt.Errorf("%w: parenthesized join group coalescing %s in a position SQLite rebuilds the column list for, expanded by a \"*\" (the coalesced column moves to the front of that list)", errVDBEUnsupported, strings.Join(coalesced, ","))
			}
			// Else: the group's own OUTWARD USING/NATURAL connector coalesces
			// on `first` too (frontMoveUnobservable's own doc comment), so
			// `first` never survives to a flat "*" expansion in EITHER
			// ordering -- the internal front-move is invisible.
		}
	}
	if !members[0].HasGroupAlias {
		return nil
	}
	// (B) and (C). A coalesced name counts as a repeat in the census, but two cases
	// are exempt (TestR26RebuiltGroupCoalescedAnswers):
	//
	//  1. No "*" touches the group (GroupRebuildStarred false): qualified
	//     references resolve through the group-alias and coalesce-fallback pass,
	//     never the rebuilt list, so the census checks nothing observable.
	//  2. Only a bare "*" touches it (not GroupRebuildQualStarred): a duplicate that
	//     is the group's coalesced column is exempt. C keeps right-hand USING columns
	//     in the rebuilt list but marks them COLFLAG_NOEXPAND (select.c ~6309),
	//     which only a bare "*" skips (~6250); after the checks above that leaves
	//     exactly what expandSelectList already produces (joinH.test 14.x).
	//
	// A qualified star bypasses NOEXPAND and reaches the ":N" copy, which C itself
	// rejects ("SELECT f1.* FROM (f1 JOIN f2 USING(b)) AS gq": "no such column:
	// b:1"), so it stays declined, as do accidental duplicates.
	coalescedSet := make(map[string]bool, len(coalesced))
	for _, n := range coalesced {
		coalescedSet[r33sFoldIdent(n)] = true
	}
	seen := make(map[string]bool)
	for k := range members {
		for _, col := range scopes[base+k].cols {
			lname := r33sFoldIdent(col.Name)
			switch lname {
			case "rowid", "oid", "_rowid_":
				return fmt.Errorf("%w: parenthesized join group aliased %q declares the column name %q (SQLite's rebuilt list collides it with the group's own invisible rowid alias and then cannot resolve it)", errVDBEUnsupported, members[0].GroupAlias, col.Name)
			}
			if !members[0].GroupRebuildStarred {
				continue
			}
			if !members[0].GroupRebuildQualStarred && coalescedSet[lname] {
				continue
			}
			if seen[lname] {
				return fmt.Errorf("%w: parenthesized join group aliased %q repeats the column name %q (SQLite rebuilds an aliased group's column list, making that ambiguous)", errVDBEUnsupported, members[0].GroupAlias, col.Name)
			}
			seen[lname] = true
		}
	}
	return nil
}

// frontMoveUnobservable reports whether case (A)'s front move cannot be seen:
// the group's connector (members[0], carrying its outward join) coalesces on
// first, the column the rebuild would move to the front. The outward join then
// drops first from "*" anyway (select.c:6260; expandSelectList's coalesced
// hiding), so its position does not matter (join2.test: "t1 NATURAL LEFT OUTER
// JOIN (t2 NATURAL JOIN t3)"). base==0 is never eligible: a leading group has no
// outward connector. The caller restricts this to groups whose first internal
// connector is the only coalescing one; otherwise C's insertion point is
// mid-list.
func frontMoveUnobservable(conn FromItem, scopes map[int]compileScope, base int, first string) bool {
	if base == 0 || first == "" {
		return false
	}
	for _, u := range conn.Using {
		if r33sFoldIdent(u) == first {
			return true
		}
	}
	if !conn.Natural {
		return false
	}
	for e := 0; e < base; e++ {
		if _, shared := scopes[e].colIndex[first]; shared {
			return true
		}
	}
	return false
}

// appendUniqueFold appends name to names unless a case-insensitive equal is
// already there, keeping first-seen order (which is FROM order here).
func appendUniqueFold(names []string, name string) []string {
	for _, n := range names {
		if equalFoldName(n, name) {
			return names
		}
	}
	return append(names, name)
}

// resolveDerivedSource resolves a derived-table FROM item into a joinSource
// whose rows come from a compiled sub-Program. A body the VDBE cannot build is
// errVDBEUnsupported. Column metadata comes from derivedColumnInfos, matching
// C's column namespace.
//
// The body compiles against derivedOuterBarrier(c), which enforces two rules:
// it cannot see its own query's FROM items (no LATERAL: "SELECT * FROM t1,
// (SELECT t1.a)" is "no such column"), but it can see an enclosing query's
// columns when the surrounding subquery is correlated (subquery2.test,
// tkt3346.test). The barrier occupies a level so OpOuterColumn's P5 counts the
// same hops as the runtime parent chain (the body runs one frame below c), and
// c must then re-run per enclosing row. A compound's exposed column takes the
// leftmost arm's collation (derivedColumnCollations).
func (p *ReadOnlyPager) resolveDerivedSource(c *compiler, state *joinDesugarState, i int, it FromItem, offset int) (joinSource, error) {
	leave := p.enterFromBody()
	prog, perr := compileSubProgram(p, it.Subquery, derivedOuterBarrier(c))
	leave()
	if perr != nil {
		return joinSource{}, perr // already wrapped errVDBEUnsupported
	}
	if prog.Correlated {
		// The body bound a column of a query enclosing c, reading it THROUGH c's
		// own frame. compileColumn's marking loop walks the barrier, not c (the
		// barrier stands in for it), so c would otherwise stay uncorrelated and
		// its program be run once and cached (runSubOnce) -- serving every
		// enclosing row the materialization built for the first one.
		c.correlated = true
	}
	// derivedColumnInfos re-resolves the body's FROM to read affinity and
	// collation, so the body's own WITH must be in scope again here. C binds each
	// result expression once inside sqlite3WithPush (select.c:6000, 6028) and
	// sqlite3SubqueryColumnTypes (select.c:2346) just follows the binding; this
	// engine re-derives from the AST and must recreate the scope, or a CTE that
	// shadows a table would take the table's NOCASE/affinity while returning the
	// CTE's rows.
	popSubCTEs := func() {}
	if it.Subquery != nil && len(it.Subquery.CTEs) > 0 {
		popSubCTEs = p.pushCTEScope(it.Subquery.CTEs)
	}
	cols := p.derivedColumnInfos(it.Subquery, prog.ColNames)
	orderProvable, needsOuterSafety := p.derivedSubqueryScanOrderProvable(c, it.Subquery)
	popSubCTEs()
	name := it.Alias // "" for a derived table with no alias
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, cols)
	if derr != nil {
		return joinSource{}, declineOrSemantic(derr)
	}
	tbl := &resolvedTable{cols: cols, ipkIndex: -1}
	cursor := c.allocCursor()
	slot := c.allocSub()
	// A derived table has no real table name; full_column_names qualifies its
	// columns with the alias (tableScope.tableName == name), or declines when
	// unaliased (name == "").
	ts := tableScope{name: name, tableName: name, cols: cols, colIndex: buildColIndex(cols), offset: offset, coalesced: hidden, noRowid: true}
	return joinSource{
		tbl:                          tbl,
		scope:                        compileScope{tableScope: ts, cursor: cursor},
		left:                         it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:                   it.Join == JoinRight || it.Join == JoinFull,
		on:                           onExpr,
		derived:                      prog,
		derivedSlot:                  slot,
		derivedOrderProvable:         orderProvable,
		derivedOrderNeedsOuterSafety: needsOuterSafety,
	}, nil
}


// fromlessArmOrderProvable reports whether arm has no FROM and no aggregate, so
// it yields at most one row: there is no scan to order and nothing for an
// outer predicate to fold into.
func fromlessArmOrderProvable(arm *SelectStmt) bool {
	return arm != nil && len(arm.From) == 0 && !selectIsAggregateQuery(arm)
}

// derivedSubqueryScanOrderProvable reports whether stmt is "SELECT ... FROM <one
// plain base table>" (no compound, aggregate, DISTINCT, ORDER BY, LIMIT or
// OFFSET), so its row order is exactly what wherePlanSingleTableIndexOrder
// decides for that table, letting anchorNoIndexInPlay trust it. Must be called
// with the CTE scope that resolved stmt's FROM active.
//
// A locally resolving schema qualifier is allowed: qualifyUnqualifiedFromItems
// stamps one on every persisted view body, as C's fixSelectCb does
// (attach.c:507). A foreign qualifier declines.
//
// With a real index, C flattens this shape (select.c:4290) and plans it like a
// top-level item, which is the question wherePlanSingleTableIndexOrder answers;
// the sub-Program compiles through the same compileSelectScan and gets the same
// key, so its rows already come in that order. needsOuterSafety is true in that
// case, since outer predicates substituted by flattening could change the
// choice; the caller must rule that out.
func (p *ReadOnlyPager) derivedSubqueryScanOrderProvable(c *compiler, stmt *SelectStmt) (orderProvable, needsOuterSafety bool) {
	if stmt == nil {
		return false, false
	}
	if len(stmt.Compound) != 0 {
		// A compound whose every arm is FROM-less is a short literal row sequence, with
		// no table anywhere for an index to walk, so it is provable too; any arm reading
		// a table falls through to false like other compounds.
		if fromlessArmOrderProvable(stmt) {
			for _, arm := range stmt.Compound {
				if !fromlessArmOrderProvable(arm.Stmt) {
					return false, false
				}
			}
			return true, false
		}
		return false, false
	}
	if stmt.Distinct || len(stmt.OrderBy) != 0 ||
		stmt.Limit != nil || stmt.Offset != nil || selectIsAggregateQuery(stmt) {
		return false, false
	}
	// A FROM-less select yields at most one row, so it is provable regardless. This
	// matters because anchorNoIndexInPlay's opaque flag is shared across sources:
	// one unproven constant side would force the schema-wide noIndexAnywhere check
	// for the whole statement.
	if len(stmt.From) == 0 {
		return true, false
	}
	if len(stmt.From) != 1 {
		return false, false
	}
	it := stmt.From[0]
	if it.Table == "" || it.Subquery != nil || it.TableFunc {
		return false, false
	}
	// scope is the catalog this FROM item's own compile would resolve it
	// in -- fromItemScope (temp_schema.go) is the SAME function
	// resolveJoinSources itself calls to resolve this exact item
	// (vdbe_join_codegen.go's own resolveViewSource/resolveJoinSources
	// call sites), so the census and resolveTableIn call below can never
	// disagree with what the real compile actually reads. For an
	// unqualified item it is scopeAny, identical to this function's
	// previous unscoped behavior.
	scope := fromItemScope(it)
	if it.Schema != "" {
		ok, lerr := p.qualifierResolvesLocally(it.Schema)
		if lerr != nil || !ok {
			return false, false // a genuinely foreign qualifier -- not provable here
		}
		// A schema-qualified name is never a CTE (join.go's schemaQualified
		// gate skips the CTE branch entirely for one; verified this mirrors
		// resolveJoinSources exactly), so the lookupCTE shadow check just
		// below only applies to the unqualified case.
	} else if _, ok := p.lookupCTE(it.Table); ok {
		return false, false // a CTE shadows this name -- not a base table at all
	}
	rows, err := p.Schema()
	if err != nil {
		return false, false
	}
	sawTable, hasIndex := false, false
	for i := range rows {
		if !scope.accepts(rows[i].Temp) {
			continue
		}
		if rows[i].Type == "index" && equalFoldName(rows[i].TblName, it.Table) {
			hasIndex = true
		}
		if rows[i].Type == "table" && equalFoldName(rows[i].Name, it.Table) {
			sawTable = true
		}
	}
	if !sawTable {
		return false, false
	}
	if !hasIndex {
		// No index anywhere on this table: the rowid scan is the only
		// candidate loop, with or without any predicate flattening could add
		// (a predicate on an unindexed column only ever filters output rows
		// AFTER the same full scan either engine would run -- it cannot
		// create a competing access path that did not exist before). Immune
		// to outer folding.
		return true, false
	}
	tbl, terr := p.resolveTableIn(scope, it.Table)
	if terr != nil {
		return false, false
	}
	srcs := []joinSource{{
		tbl: tbl,
		scope: compileScope{tableScope: tableScope{
			name: it.Table, tableName: it.Table, cols: tbl.cols, colIndex: buildColIndex(tbl.cols),
		}},
	}}
	_, _, ok := wherePlanSingleTableIndexOrder(p, c, srcs, stmt)
	return ok, ok
}

// derivedOuterBarrier builds the enclosing compiler a derived table's body
// compiles against: c's place in the chain with no scopes of its own (see
// resolveDerivedSource). nil in, nil out. c.rowOuter is carried too: a subquery
// compiled with a live enclosing row and no enclosing compiler would otherwise
// lose its only enclosing query ("SELECT sum((SELECT 1 FROM (SELECT 2 WHERE x
// IS NULL) WHERE 0)) FROM t1", select4.test). Such compiles are never cached,
// so baking in the row value is sound.
func derivedOuterBarrier(c *compiler) *compiler {
	if c == nil || (c.outer == nil && c.rowOuter == nil) {
		return nil
	}
	barrier := &compiler{pager: c.pager, outer: c.outer, rowOuter: c.rowOuter}
	// A derived table's body is populated BEFORE its containing query's own
	// WHERE loop begins (materialized as a coroutine/ephemeral table ahead of
	// sqlite3WhereBegin), so its seed is c's GRANDPARENT's ambient nQueryLoop --
	// c.outer's, exactly like the barrier already skips c itself for outer.
	barrier.nQueryLoop, barrier.nQueryLoopKnown = inheritedNQueryLoop(c.outer)
	return barrier
}

// resolveVtabSource resolves a table-valued function, persisted virtual table
// or eponymous module into a joinSource whose rows come from materializeVtab,
// run once through OpOpenDerived (derivedSlot). Callers have confirmed it is a
// vtab and pass the owning pager. The column schema comes from a throwaway
// Connect, the same schema materializeVtab re-derives at run time. Unlike a
// derived table it keeps a real rowid (openMaterializedCursor).
func (p *ReadOnlyPager) resolveVtabSource(c *compiler, state *joinDesugarState, i int, it FromItem, offset int) (joinSource, error) {
	mod, modArgs, merr := p.resolveVtabModuleForItem(it)
	if merr != nil {
		// A genuine C-SQLite rejection ("no such table"/"no such module"),
		// not a construct outside this compiler's scope -- wrapped
		// errVDBESemantic so the caller (tryVDBEScan, vdbe_run.go) PROPAGATES
		// it as the statement's real error, exactly like the ordinary
		// resolveTable failure branch just past this function's own call
		// site.
		return joinSource{}, semanticf("%v", merr)
	}
	vcols, _, cerr := vtabConnect(p.fts3Catalog(), mod, modArgs)
	if cerr != nil {
		return joinSource{}, semanticf("%v", cerr)
	}
	cols := make([]columnInfo, len(vcols))
	for ci, vc := range vcols {
		cols[ci] = columnInfo{Name: vc.Name, DeclType: vc.Type, Aff: vtabColumnAffinity(vc), Hidden: vc.Hidden, Unindexed: vc.Unindexed}
	}
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	tableName := it.Table
	if tableName == "" {
		tableName = name
	}
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, cols)
	if derr != nil {
		return joinSource{}, declineOrSemantic(derr)
	}
	tbl := &resolvedTable{cols: cols, ipkIndex: -1}
	cursor := c.allocCursor()
	slot := c.allocSub()
	ts := tableScope{name: name, tableName: tableName, cols: cols, colIndex: buildColIndex(cols), offset: offset, coalesced: hidden}
	itemCopy := it
	js := joinSource{
		tbl:         tbl,
		scope:       compileScope{tableScope: ts, cursor: cursor},
		left:        it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:  it.Join == JoinRight || it.Join == JoinFull,
		on:          onExpr,
		derivedSlot: slot,
		vtabItem:    &itemCopy,
		// Whether this source's rows come through the module's BestIndex/
		// Filter pair at all -- see joinSource.vtabDrivesBestIndex and
		// vtabDrivesBestIndex (vtab.go), which reads the same three type
		// assertions materializeVtab's own dispatch does.
		vtabDrivesBestIndex: vtabDrivesBestIndex(mod, it),
	}
	// The module's own claim on one of this statement's WHERE conjuncts, if it
	// makes one -- see joinSource.vtabOmitCandidate. Never beside a call
	// argument: C appends the argument's "hidden = arg" term AFTER the WHERE's
	// own (sqlite3WhereSplit at where.c:6941, sqlite3WhereTabFuncArgs at
	// :6975), so a module keeping the last equality keeps the argument and the
	// WHERE conjunct is re-applied there too.
	if om, ok := mod.(omittingVtabModule); ok && len(it.tvfWhere) > 0 && len(it.TableFuncArgs) == 0 {
		if j := om.OmittedConjunct(it.tvfWhere, name); j >= 0 && j < len(it.tvfWhere) {
			js.vtabOmitCandidate = j + 1
		}
	}
	// isFtsVtab is computed against p, the item's owning pager, where p.forDB(0) is
	// right whatever js.dbIdx becomes. ftsMatchForcedOrder and computeExecOrder
	// read it to recognize the MATCH-forced nesting.
	js.isFtsVtab = isFtsVtabSource(p, js)
	return js, nil
}

// viewBodyNoSuchTableRewrite applies rewriteViewNoSuchTable's "main."
// qualification to a "no such table" raised while compiling a view's own body,
// as C does. Only errVDBESemantic errors are rewritten (and re-wrapped);
// capability gaps pass through unchanged.
func viewBodyNoSuchTableRewrite(err error) error {
	if err == nil || !errors.Is(err, errVDBESemantic) {
		return err
	}
	rewritten := rewriteViewNoSuchTable(fmt.Errorf("%s", err.Error()))
	return semanticf("%s", rewritten.Error())
}

// resolveViewColumns computes a view's output columns (the compiled body's
// result columns, then its declared names' rename/":N" uniquify, or the
// duplicate-name guard without them), as resolveViewSource does. Factored out so
// resolveGroupSource can use it for a view connector without resolveViewSource's
// cursor allocation and desugarJoinItem call, which would double-register the
// item index.
func (p *ReadOnlyPager) resolveViewColumns(c *compiler, it FromItem, pcv *parsedCreateView) ([]columnInfo, *Program, bool, bool, error) {
	sub := pcv.selectStmt
	// An unknown COLLATE name in the view body is deferred until the view is used,
	// which is here (build.c:3115 sqlite3ViewGetColumnNames; view_collate.go), and
	// is errVDBESemantic.
	if name := firstUnknownViewCollation(sub); name != "" {
		return nil, nil, false, false, semanticf("%v", errUnknownViewCollation(name))
	}
	// A view body is one of the two DDL-origin sites "PRAGMA trusted_schema=OFF"
	// reaches, and this is the compile path that actually runs one (the VDBE is
	// the sole executor); see trusted_schema.go. A no-op with the ON default.
	// Wrapped errVDBESemantic so tryVDBEScan PROPAGATES the oracle's own wording
	// instead of masking it behind "not compilable to bytecode".
	if err := p.checkTrustedSchemaSelect(pcv.isTemp, sub); err != nil {
		if errors.Is(err, errVDBEUnsupported) {
			return nil, nil, false, false, err
		}
		return nil, nil, false, false, semanticf("%v", err)
	}
	for _, seen := range p.viewExpansion {
		if equalFoldName(seen, it.Table) {
			return nil, nil, false, false, fmt.Errorf("engine: view %s is circularly defined", it.Table)
		}
	}
	// The FAN-OUT guard, which the cycle stack above structurally cannot cover:
	// see countViewReference (view.go). This compile path is the one view3.test
	// actually reaches, and without it a doubling view chain hangs the compiler.
	if err := p.countViewReference(it.Table); err != nil {
		return nil, nil, false, false, err
	}
	p.viewExpansion = append(p.viewExpansion, it.Table)
	// A view's body sees its OWN WITH clause and nothing else: the caller's
	// clauses are hidden (a CTE in the querying statement must not shadow a
	// table the view names -- view2.test), and the body's own are pushed here
	// because compileSubProgram, unlike execSelect, does not push them itself.
	restoreCTEs := p.hideCTEScopes()
	popBodyCTEs := func() {}
	if len(sub.CTEs) > 0 {
		popBodyCTEs = p.pushCTEScope(sub.CTEs)
	}
	leave := p.enterFromBody()
	prog, perr := compileSubProgram(p, sub, nil)
	leave()
	// The view's column names and types are derived from the body's FROM too, so
	// they must be computed while the body's WITH is still pushed: C pushes it
	// (select.c:5991, 6000) and pops it only after the whole walk (sqlite3SelectPopWith,
	// :5866). Computed after the pops, a body CTE shadowing a table renamed the
	// view's columns to the table's:
	//
	//	CREATE TABLE t1(x,y); INSERT INTO t1 VALUES(1,2);
	//	CREATE VIEW v3 AS WITH t1(p,q) AS (SELECT 9,9) SELECT * FROM t1;
	//	SELECT * FROM v3   ->  cgo p|q = 9,9; this engine x|y = 9,9
	var cols []columnInfo
	var orderProvable, needsOuterSafety bool
	if perr == nil {
		cols = p.derivedColumnInfos(sub, prog.ColNames)
		// A VIEW's column list is built by sqlite3ResultSetOfSelect AFTER its
		// expressions are resolved, which peels likely()/unlikely()/likelihood()
		// off an unaliased item where an inline derived table's naming does not --
		// prog.ColNames is the inline answer. viewOutputRows (view.go) re-derives
		// them under the resolved rule and this mirror did not, so the two paths
		// disagreed: "CREATE VIEW v AS SELECT unlikely(x) FROM t9" reported its
		// column as "unlikely(x)" through a SELECT and as "x" through PRAGMA
		// table_info, where 3.53.3 says "x" for both.
		p.applyViewColumnNames(sub, cols)
		// Same reasoning as resolveDerivedSource's identical call (see that
		// function's derivedOrderProvable doc comment): must run in THIS exact
		// window, where the view body's OWN CTE scope is pushed and the
		// caller's is hidden (restoreCTEs/popBodyCTEs above), so a CTE
		// SHADOWING a same-named base table inside the view's own body is
		// detected correctly rather than re-derived later with no scope.
		orderProvable, needsOuterSafety = p.derivedSubqueryScanOrderProvable(c, sub)
	}
	popBodyCTEs()
	restoreCTEs()
	p.viewExpansion = p.viewExpansion[:len(p.viewExpansion)-1]
	if perr != nil {
		// Unsupported errors pass through; a semantic error from the view's own FROM
		// ("no such table") gets the "main." rewrite, as for materialized views.
		return nil, nil, false, false, viewBodyNoSuchTableRewrite(perr)
	}
	if pcv.colNames != nil {
		if len(pcv.colNames) != len(cols) {
			// C's own "expected N columns for '<view>' but got M", as errVDBESemantic.
			return nil, nil, false, false, semanticf("expected %d columns for '%s' but got %d", len(pcv.colNames), it.Table, len(cols))
		}
		renamed := make([]columnInfo, len(cols))
		copy(renamed, cols)
		for idx, n := range pcv.colNames {
			renamed[idx].Name = n
		}
		// The explicit list takes the ":N" uniquifier too -- build.c:3175's
		// sqlite3ViewGetColumnNames feeds it straight to
		// sqlite3ColumnsFromExprList. See viewOutputRows (view.go), which this
		// mirrors, for the wrong VALUE the un-renamed spelling produced.
		if uniqNames, uniqOK := r32mUniqueColumnNames(pcv.colNames); uniqOK {
			for idx := range renamed {
				renamed[idx].Name = uniqNames[idx]
			}
		}
		cols = renamed
	} else {
		seen := make(map[string]bool, len(cols))
		for _, cinfo := range cols {
			lname := r33sFoldIdent(cinfo.Name)
			if seen[lname] {
				return nil, nil, false, false, fmt.Errorf("%w: duplicate result-column name %q in view %s (SQLite's ':N' disambiguation is not reproduced)", errVDBEUnsupported, cinfo.Name, it.Table)
			}
			seen[lname] = true
		}
	}
	return cols, prog, orderProvable, needsOuterSafety, nil
}

// resolveViewSource resolves a FROM item naming a view into a joinSource over a
// run-once sub-Program compiling the view's stored SELECT, reusing
// resolveDerivedSource's wiring with the view's differences:
//
//  1. An unaliased view is still qualifiable by its own name ("SELECT v.x FROM
//     v").
//  2. A declared column list ("CREATE VIEW v(a,b)") renames the output.
//  3. Without one, duplicate output names are declined.
//  4. A circular view chain is caught here at compile time via
//     p.viewExpansion, returning "circularly defined" instead of recursing.
func (p *ReadOnlyPager) resolveViewSource(c *compiler, state *joinDesugarState, i int, it FromItem, pcv *parsedCreateView, offset int) (joinSource, error) {
	cols, prog, orderProvable, needsOuterSafety, err := p.resolveViewColumns(c, it, pcv)
	if err != nil {
		return joinSource{}, err
	}
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, cols)
	if derr != nil {
		return joinSource{}, declineOrSemantic(derr)
	}
	tbl := &resolvedTable{cols: cols, ipkIndex: -1}
	cursor := c.allocCursor()
	slot := c.allocSub()
	ts := tableScope{name: name, tableName: name, cols: cols, colIndex: buildColIndex(cols), offset: offset, coalesced: hidden, noRowid: true}
	return joinSource{
		tbl:                          tbl,
		scope:                        compileScope{tableScope: ts, cursor: cursor},
		left:                         it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:                   it.Join == JoinRight || it.Join == JoinFull,
		on:                           onExpr,
		derived:                      prog,
		derivedSlot:                  slot,
		derivedOrderProvable:         orderProvable,
		derivedOrderNeedsOuterSafety: needsOuterSafety,
	}, nil
}

// cteRef is joinSource.cteItem's payload and derivedSource's own P4 payload
// (vdbe.go) for a CTE row source: the CTE's name (for resolveCTERows' own
// circularity-guard bookkeeping, cte.go) and its already-resolved
// *cteBinding (built once, when its WITH clause's scope frame was pushed --
// pushCTEScope, cte.go -- and shared by every FROM-item reference to it).
type cteRef struct {
	name    string
	binding *cteBinding

	// outerRowCapPlus1, when non-zero, is one more than the rows this reference's
	// statement can read from the CTE (its LIMIT plus OFFSET, recursiveCTEOuterCap).
	// The recursive queue program stops there, so a recursion bounded only by an
	// outer LIMIT terminates as C's lazy co-routine does. It lives on the reference,
	// not the shared binding, because two references can have different bounds.
	// Plus-one encoded because LIMIT 0 is a real bound.
	outerRowCapPlus1 int64

	// orderProvable is joinSource.derivedOrderProvable for a CTE reference: its body
	// is simple enough (cteOrderProvable) to exclude it from anchorNoIndexInPlay's
	// schema-wide fallback. Computed once in resolveCTESource with the CTE's scope
	// active.
	orderProvable bool

	// queue is a RECURSIVE CTE reference's compiled queue program
	// (compileRecursiveCTE, vdbe_recursive_cte.go), opened as an ordinary
	// derived sub-Program. nil for every other CTE reference.
	queue *Program

	// recSelf, when non-nil, marks this reference as a recursive CTE's own name
	// inside one of its recursive arms: its one row is the queue row currently
	// being expanded (SQLite's iCurrent pseudo-cursor, select.c:2733). nil for
	// every other CTE reference.
	recSelf *recQueueSpec
}

// resolveCorrelatedCTESource resolves a non-recursive CTE reference as a
// correlated derived table: it compiles b.core against derivedOuterBarrier(c)
// and checks prog.Correlated. In C a CTE reference becomes an ordinary subquery
// FROM item before resolution (select.c:5754) and is resolved with the
// enclosing NameContext like any derived table (resolve.c:1929-1954). A view is
// different: its body is fixed at CREATE time and compiles with no outer.
//
// Returns ok=false, err=nil when it does not apply (no enclosing compiler, a
// reference cycle, a declined probe, or not correlated, which is almost every
// CTE), so the caller's path is unchanged. Once correlation is proven, errors
// propagate. The Program goes into joinSource.derived, so OpOpenDerived's
// existing prog/execWithParent branch runs it; nothing new is needed at run
// time.
func (p *ReadOnlyPager) resolveCorrelatedCTESource(c *compiler, state *joinDesugarState, i int, it FromItem, b *cteBinding, offset int) (joinSource, bool, error) {
	if c == nil {
		return joinSource{}, false, nil
	}
	cteName := it.Table
	// The SAME circularity guard resolveCTERows/cteSchemaCols use (cte.go's
	// own doc comment on ReadOnlyPager.cteExpansion): without it, a genuinely
	// self-referencing non-recursive body ("WITH c(x) AS (WITH d(y) AS
	// (SELECT x FROM c) SELECT y FROM d) SELECT * FROM c", already pinned by
	// cte_inner_with_shadowing_test.go) would recurse compileSubProgram ->
	// resolveCTESource -> this function -> compileSubProgram on the SAME
	// binding forever -- a Go call-stack overflow, this package's hardest
	// forbidden failure (AGENTS.md "never panic"), not merely a wrong answer.
	for _, seen := range p.cteExpansion {
		if seen.b == b {
			return joinSource{}, false, nil // let the existing guard below report it
		}
	}
	p.cteExpansion = append(p.cteExpansion, cteExpansionFrame{name: cteName, b: b})
	defer func() { p.cteExpansion = p.cteExpansion[:len(p.cteExpansion)-1] }()
	// The body resolves in the scope where THIS CTE was DEFINED, exactly like
	// cteSchemaCols/resolveCTERows -- see cteBinding.scopeDepth's own doc
	// comment (cte.go) for the with3.test evidence this guards.
	defer p.enterCTEBodyScope(cteName, b)()

	leave := p.enterFromBody()
	prog, perr := compileSubProgram(p, b.core, derivedOuterBarrier(c))
	leave()
	if perr != nil || !prog.Correlated {
		return joinSource{}, false, nil
	}
	// c's own program must now re-run per enclosing row too, not be cached --
	// see resolveDerivedSource's identical propagation and its own doc
	// comment for why: c is the frame this CTE body's OpOuterColumn reads
	// THROUGH, so c staying "uncorrelated" would let it run once and serve
	// every enclosing row the first row's materialization built.
	c.correlated = true

	// cols exactly mirrors cteSchemaCols' own tail: derivedColumnInfos under
	// the body's own nested WITH (if any) pushed for the length of the call
	// (see resolveDerivedSource's identical step for the wrong-VALUE bug this
	// guards -- a CTE-shadowed-table affinity leak), then the CTE's own
	// explicit "(col, ...)" rename list / duplicate-name rejection.
	popTargetCTEs := func() {}
	if len(b.core.CTEs) > 0 {
		popTargetCTEs = p.pushCTEScope(b.core.CTEs)
	}
	cols := p.derivedColumnInfos(b.core, prog.ColNames)
	popTargetCTEs()
	cols, _, err := applyCTEColNames(cteName, b.colNames, cols, nil)
	if err != nil {
		return joinSource{}, false, semanticf("%v", err)
	}

	name := it.Alias
	if name == "" {
		name = it.Table
	}
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, cols)
	if derr != nil {
		return joinSource{}, false, declineOrSemantic(derr)
	}
	tbl := &resolvedTable{cols: cols, ipkIndex: -1}
	cursor := c.allocCursor()
	slot := c.allocSub()
	// noRowid: true, exactly like an ordinary derived table/view and like
	// resolveCTESource's own unchanged path -- a CTE exposes no rowid
	// pseudo-column.
	ts := tableScope{name: name, tableName: name, cols: cols, colIndex: buildColIndex(cols), offset: offset, coalesced: hidden, noRowid: true}
	return joinSource{
		tbl:         tbl,
		scope:       compileScope{tableScope: ts, cursor: cursor},
		left:        it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:  it.Join == JoinRight || it.Join == JoinFull,
		on:          onExpr,
		derived:     prog,
		derivedSlot: slot,
	}, true, nil
}

// resolveCTESource resolves a FROM item naming an in-scope CTE into a source
// opened through OpOpenDerived:
//
//   - a recursive CTE compiles into its queue program (compileRecursiveCTE,
//     as C codes the recursion at prepare time, select.c:2666);
//   - its own name inside a recursive arm (b.selfRef) is a one-row source over
//     the queue's current row (resolveRecursiveSelfSource);
//   - an ordinary CTE is materialized once at open time (runCTEOnce); a
//     correlated one goes to resolveCorrelatedCTESource first.
//
// The output schema is resolved at compile time without running anything
// (cteSchemaCols); a result's column shape does not depend on parameter values.
func (p *ReadOnlyPager) resolveCTESource(c *compiler, state *joinDesugarState, i int, it FromItem, b *cteBinding, offset int) (joinSource, error) {
	cteName := it.Table
	// The recursive step's own reference to the CTE it belongs to -- but only
	// from the arm compile itself: a reference reached through any further CTE
	// expansion (a sibling body naming this one) is a cycle, which the guard in
	// cteSchemaCols/compileRecursiveCTE reports. See recSelfRef.
	if sr := b.selfRef; sr != nil && sr.spec != nil && sr.expansionDepth == len(p.cteExpansion) {
		return p.resolveRecursiveSelfSource(c, state, i, it, b, sr, offset)
	}

	// A non-recursive CTE whose body binds an enclosing query's column ("SELECT x
	// FROM t1 WHERE EXISTS (WITH t2(a) AS (VALUES(x)) SELECT * FROM t2 WHERE a>7)")
	// is resolved like a correlated derived table (resolveCorrelatedCTESource).
	// Uncorrelated references stay on the path below unchanged. A reference to an
	// enclosing select-list alias still declines, as it does without the CTE.
	if b.recursive == nil {
		if ds, ok, cerr := p.resolveCorrelatedCTESource(c, state, i, it, b, offset); cerr != nil {
			return joinSource{}, cerr
		} else if ok {
			return ds, nil
		}
	}

	cols, err := p.cteSchemaCols(cteName, b)
	if err != nil {
		return joinSource{}, err
	}
	// See cteOrderProvable's own doc comment: computed here, not later from
	// anchorNoIndexInPlay, for the identical reason cteSchemaCols' own scope
	// window matters.
	orderProvable := p.cteOrderProvable(c, cteName, b)
	// The consuming statement's own exact row bound, if its shape had one --
	// see cteRef.outerRowCapPlus1 and compileScanAttempt (vdbe_scan.go). The
	// name check is belt-and-braces: recursiveCTEOuterCap only ever publishes
	// for a single-FROM-item statement, so this IS that item.
	var outerCap int64
	if c.cteOuterCapPlus1 > 0 && equalFoldName(c.cteOuterCapName, cteName) {
		outerCap = c.cteOuterCapPlus1
	}
	var queue *Program
	if b.recursive != nil {
		queue, err = p.compileRecursiveCTE(cteName, b, cols, outerCap)
		if err != nil {
			return joinSource{}, err
		}
	}
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, cols)
	if derr != nil {
		return joinSource{}, declineOrSemantic(derr)
	}
	tbl := &resolvedTable{cols: cols, ipkIndex: -1}
	cursor := c.allocCursor()
	slot := c.allocSub()
	// noRowid: true, exactly like an ordinary derived table/view -- a CTE
	// exposes no rowid pseudo-column.
	ts := tableScope{name: name, tableName: name, cols: cols, colIndex: buildColIndex(cols), offset: offset, coalesced: hidden, noRowid: true}
	return joinSource{
		tbl:         tbl,
		scope:       compileScope{tableScope: ts, cursor: cursor},
		left:        it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:  it.Join == JoinRight || it.Join == JoinFull,
		on:          onExpr,
		derivedSlot: slot,
		cteItem:     &cteRef{name: cteName, binding: b, outerRowCapPlus1: outerCap, orderProvable: orderProvable, queue: queue},
	}, nil
}

// resolveRecursiveSelfSource is resolveCTESource for a recursive CTE's own name
// inside one of its recursive arms, while compileRecursiveCTE compiles that arm
// (sr is b.selfRef): a one-row source over the queue row being expanded,
// SQLite's iCurrent pseudo-cursor (select.c:2733, bound to the recursive
// FROM term at select.c:2721-2725). Everything but where the row comes from is
// resolveCTESource's own: the renamed CTE columns, the USING/NATURAL desugar,
// noRowid, and a cteItem the planner treats as the CTE source it is.
func (p *ReadOnlyPager) resolveRecursiveSelfSource(c *compiler, state *joinDesugarState, i int, it FromItem, b *cteBinding, sr *recSelfRef, offset int) (joinSource, error) {
	cols := sr.cols
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, cols)
	if derr != nil {
		return joinSource{}, declineOrSemantic(derr)
	}
	tbl := &resolvedTable{cols: cols, ipkIndex: -1}
	cursor := c.allocCursor()
	slot := c.allocSub()
	ts := tableScope{name: name, tableName: name, cols: cols, colIndex: buildColIndex(cols), offset: offset, coalesced: hidden, noRowid: true}
	return joinSource{
		tbl:         tbl,
		scope:       compileScope{tableScope: ts, cursor: cursor},
		left:        it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:  it.Join == JoinRight || it.Join == JoinFull,
		on:          onExpr,
		derivedSlot: slot,
		cteItem:     &cteRef{name: it.Table, binding: b, recSelf: sr.spec},
	}, nil
}

// cteSchemaCols resolves b's output columns at compile time with no execution.
// p.cteExpansion, shared with resolveCTERows' runtime guard, catches a
// reference cycle among CTEs before compileSubProgram recurses forever ("circular
// reference: %s"); the recognized recursive self-reference is short-circuited
// by b.selfRef first.
func (p *ReadOnlyPager) cteSchemaCols(name string, b *cteBinding) ([]columnInfo, error) {
	if b.selfRef != nil {
		return b.selfRef.cols, nil
	}
	if b.multiSelfRef {
		// select.c:5786-5792 -- a recursive arm whose own FROM names this CTE
		// twice is "multiple references to recursive table", not the circular
		// reference the guard below would report. See cteBinding.multiSelfRef.
		return nil, semanticf("multiple references to recursive table: %s", name)
	}
	for _, seen := range p.cteExpansion {
		if seen.b == b {
			// A genuine C-SQLite rejection (C SQLite raises the identical
			// "circular reference: %s" text -- see cte.go's resolveCTERows,
			// whose runtime circularity guard reports the same wording for
			// the identical shape reached a different way), not a construct
			// outside this compiler's scope: wrapped errVDBESemantic so the
			// caller (tryVDBEScan, vdbe_run.go) PROPAGATES it as the
			// statement's real error instead of masking it behind the
			// generic "not compilable to bytecode" fallback message.
			return nil, semanticf("circular reference: %s", name)
		}
	}
	p.cteExpansion = append(p.cteExpansion, cteExpansionFrame{name: name, b: b})
	defer func() { p.cteExpansion = p.cteExpansion[:len(p.cteExpansion)-1] }()
	// The body's own CTE scope, exactly the one resolveCTERows will use when
	// it later RUNS this same body -- see enterCTEBodyScope (cte.go).
	defer p.enterCTEBodyScope(name, b)()

	target := b.core
	if b.recursive != nil {
		target = b.recursive.initial
	}
	if len(target.Compound) > 0 {
	}
	// Only the CTE's COLUMN NAMES are wanted here; its ROWS come from
	// resolveCTERows -> execSelect, which resolves an unresolved LIMIT/OFFSET
	// EXPRESSION (SelectStmt.LimitParam) properly. compileSubProgram declines
	// such a body -- rightly, since a MATERIALIZED sub-program would silently
	// drop the clause -- so clear it for this schema-only compile rather than
	// let a "WITH c AS (SELECT a FROM t LIMIT 3-1)" decline over a clause that
	// cannot change a single column name.
	if target.LimitParam != nil || target.OffsetParam != nil {
		schemaOnly := *target
		schemaOnly.LimitParam, schemaOnly.OffsetParam = nil, nil
		target = &schemaOnly
	}
	leave := p.enterFromBody()
	prog, perr := compileSubProgram(p, target, nil)
	leave()
	if perr != nil {
		return nil, perr // already wrapped errVDBEUnsupported/errVDBESemantic
	}
	// A CTE body may have its own WITH, and derivedColumnInfos re-resolves the
	// body's FROM, so push it here too, as resolveDerivedSource does:
	//
	//	WITH c AS (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1)
	//	SELECT count(*) FROM c WHERE x='ABC'   -->  cgo 0, this engine 1
	popTargetCTEs := func() {}
	if len(target.CTEs) > 0 {
		popTargetCTEs = p.pushCTEScope(target.CTEs)
	}
	cols := p.derivedColumnInfos(target, prog.ColNames)
	popTargetCTEs()
	cols, _, err := applyCTEColNames(name, b.colNames, cols, nil)
	if err != nil {
		return nil, semanticf("%v", err)
	}
	return cols, nil
}

// cteOrderProvable reports whether a CTE reference's row order is provably
// independent of any index, so anchorNoIndexInPlay can exclude it from the
// schema-wide fallback (an unrelated index otherwise declined with3.test's
// "WITH t1(a) AS (VALUES(1)) SELECT ... FROM t1 GROUP BY 1"). Must be called
// from resolveCTESource with the CTE's scope active.
//
// Narrower than derivedSubqueryScanOrderProvable: a recursive CTE is never
// provable (its order comes from the queue), and the real-index case is
// declined because a CTE's rows come from resolveCTERows, not the compiled
// sub-Program that proof reasons about.
func (p *ReadOnlyPager) cteOrderProvable(c *compiler, name string, b *cteBinding) bool {
	if b.recursive != nil {
		return false
	}
	target := b.core
	if target.LimitParam != nil || target.OffsetParam != nil {
		// An unresolved LIMIT/OFFSET expression is irrelevant to cteSchemaCols
		// (a result set's SHAPE never depends on it) but is NOT irrelevant here:
		// derivedSubqueryScanOrderProvable already refuses any stmt with a
		// (resolved) Limit/Offset outright, and a still-unresolved one governs
		// row order/count by the identical mechanism -- decline rather than
		// silently treat it as absent.
		return false
	}
	defer p.enterCTEBodyScope(name, b)()
	// Push target's own nested WITH before delegating, as resolveDerivedSource and
	// cteSchemaCols do, or a name it shadows resolves against the outer scope
	// (select.c:6000).
	popTargetCTEs := func() {}
	if len(target.CTEs) > 0 {
		popTargetCTEs = p.pushCTEScope(target.CTEs)
	}
	orderProvable, needsOuterSafety := p.derivedSubqueryScanOrderProvable(c, target)
	popTargetCTEs()
	if needsOuterSafety {
		return false // the "real index" sub-case -- see this function's own doc comment
	}
	return orderProvable
}

// resolveGroupSource materializes one parenthesized join group span,
// items[i : i+GroupLen], as a single joinSource over a run-once sub-Program
// (compileGroupBody) computing the group's own joins. Because the derived
// cursor's rows are the group's row combinations, RIGHT/FULL match tracking and
// sweeps (vdbeCursor.matched, emitRightOuterSweep) and whole-group
// NULL-extension (OpNullRow on the one cursor) work unchanged.
//
// The connector's own Join/On/Using/Natural (how the group attaches outward) is
// desugared like an ordinary item, against the outer state; a USING/NATURAL
// search only considers the connector's own columns, which is right because the
// group's internal joins already equate them. The internal span is resolved by
// a fresh recursive resolveJoinSources over a copy with the first element's
// attachment fields cleared, so an internal search can never find a
// representative outside the group.
func (p *ReadOnlyPager) resolveGroupSource(c *compiler, state *joinDesugarState, i int, items []FromItem) (joinSource, error) {
	it := items[i]
	gl := it.GroupLen
	// The connector must be a base table or view (or a nested group); a derived
	// table, vtab or CTE connector is declined. INDEXED BY and schema qualification
	// on the connector are handled below.
	if it.TableFunc {
		return joinSource{}, fmt.Errorf("%w: table-valued function as a parenthesized join group's connector", errVDBEUnsupported)
	}
	if isV, verr := p.isVtabItem(it); verr != nil {
		return joinSource{}, declineOrSemantic(verr)
	} else if isV {
		return joinSource{}, fmt.Errorf("%w: virtual table as a parenthesized join group's connector", errVDBEUnsupported)
	}
	if it.Subquery != nil {
		return joinSource{}, fmt.Errorf("%w: derived table as a parenthesized join group's connector", errVDBEUnsupported)
	}
	// A connector that is itself a materialized nested group (FromItem.NestedGroupSpan,
	// x in "((t1 JOIN t2) AS x JOIN t2x ON x.b=t2x.b) AS y") has no table; its
	// columns come from resolving its nested span, done as a throwaway pass
	// (resolveNestedGroupConnectorCols). It is resolved again for real by the
	// internal span recursion below.
	var tbl *resolvedTable
	if it.NestedGroupSpan != nil {
		cols, cerr := p.resolveNestedGroupConnectorCols(it.NestedGroupSpan)
		if cerr != nil {
			return joinSource{}, cerr
		}
		tbl = &resolvedTable{cols: cols, ipkIndex: -1}
		return p.finishGroupSource(c, state, i, items, it, gl, tbl)
	}
	// A schema-qualified connector resolves against its owning pager, as in
	// resolveJoinSources (a qualified name is never a CTE, select.c:5697).
	rp := p
	if it.Schema != "" || len(p.attachedReaders) > 0 {
		owner, oerr := p.itemOwner(it)
		if oerr != nil {
			return joinSource{}, semanticf("%v", oerr)
		}
		rp = owner
	}
	if it.IndexedBy != "" {
		if ierr := rp.checkIndexExists(it.IndexedBy, it.Table, fromItemScope(it)); ierr != nil {
			return joinSource{}, semanticf("%v", ierr)
		}
	}
	if it.Schema == "" {
		if _, ok := p.lookupCTE(it.Table); ok {
			return joinSource{}, fmt.Errorf("%w: CTE reference as a parenthesized join group's connector", errVDBEUnsupported)
		}
	}
	// A view connector (joinI.test: "v1 INNER JOIN (v2 CROSS JOIN t0)"), checked
	// before resolveTableIn, which only finds tables. resolveViewColumns gets its
	// columns without a second desugarJoinItem registration.
	if pcv, ok, verr := rp.resolveViewByNameIn(fromItemScope(it), it.Table); verr == nil && ok {
		// This call is columns-only; the connector is resolved again for real by the
		// internal recursion. Both walk the view's body, and countViewReference is a
		// persistent counter against C's 65535 cap, so roll back what this throwaway
		// call adds, or a view chain C accepts (view3.test doublings) would exceed it.
		savedRefCount := make(map[string]int, len(rp.viewRefCount))
		for k, v := range rp.viewRefCount {
			savedRefCount[k] = v
		}
		cols, _, _, _, cerr := rp.resolveViewColumns(c, it, pcv)
		rp.viewRefCount = savedRefCount
		if cerr != nil {
			return joinSource{}, cerr
		}
		tbl = &resolvedTable{cols: cols, ipkIndex: -1}
	} else {
		var terr error
		tbl, terr = rp.resolveTableIn(fromItemScope(it), it.Table)
		if terr != nil {
			// See the identical base-table branch's own comment above
			// (resolveJoinSources): a genuine "no such table" must PROPAGATE
			// (errVDBESemantic), never be masked behind the generic
			// "not compilable to bytecode" fallback message.
			return joinSource{}, semanticf("%v", terr)
		}
	}
	return p.finishGroupSource(c, state, i, items, it, gl, tbl)
}

// finishGroupSource is resolveGroupSource's connector-independent half: given
// the connector's columns (table, view or nested group), it desugars the
// outward attachment, resolves the group's internal span recursively, and builds
// the joinSource. gl is it.GroupLen: the span is items[i:i+gl]; gl == 0 only for
// a NestedGroupSpan connector (re-entered with GroupLen cleared, or a lone
// re-wrapped group).
func (p *ReadOnlyPager) finishGroupSource(c *compiler, state *joinDesugarState, i int, items []FromItem, it FromItem, gl int, tbl *resolvedTable) (joinSource, error) {
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, tbl.cols)
	if derr != nil {
		return joinSource{}, declineOrSemantic(derr)
	}

	// Neutralize a copy of the span's first element for the internal resolution:
	// only Join/On/Using/Natural/GroupLen are cleared, so the member keeps its
	// identity and a NestedGroupSpan connector still resolves its own group when
	// re-entered.
	var internalSpanSrc []FromItem
	if gl > 0 {
		internalSpanSrc = items[i : i+gl]
	} else {
		internalSpanSrc = it.NestedGroupSpan
	}
	span := make([]FromItem, len(internalSpanSrc))
	copy(span, internalSpanSrc)
	span[0].Join = JoinCross
	span[0].On = nil
	span[0].Using = nil
	span[0].Natural = false
	span[0].GroupLen = 0

	// groupSpanRecursion: this recursive resolveJoinSources call sees the
	// span with its own connector's OUTWARD attachment already neutralized
	// (span[0], above), so checkRebuiltJoinGroups' front-move census inside
	// it has no way to prove a mismatch unobservable -- see the field's own
	// doc comment (vdbe_codegen.go) and checkOneRebuiltGroup's (this file).
	subC := &compiler{pager: p, groupSpanRecursion: true}
	spanSrcs, serr := resolveJoinSources(p, subC, span)
	if serr != nil {
		return joinSource{}, serr
	}
	if err := checkFromSupported(onsOfSources(spanSrcs)); err != nil {
		return joinSource{}, declineOrSemantic(err)
	}
	subC.scopes = joinScopes(spanSrcs)
	subC.rowLive = true

	// The connector's ON read of its own column must also consult its internal
	// coalesce fallback, installed by a later member of the same group ("t2 RIGHT
	// JOIN t3 USING(a)" onto t2); see markGroupConnectorCoalesceAware. Gated on
	// hidden, the connector's outward coalesced map; its internal one is always nil
	// here.
	if hidden != nil && spanSrcs[0].scope.coalesceFallback != nil && onExpr != nil {
		onExpr = markGroupConnectorCoalesceAware(onExpr, spanSrcs[0].scope.name, spanSrcs[0].scope.coalesceFallback)
	}

	// The group's output row is every leaf member's every column, in FROM order,
	// never hidden by coalescing. Each column is pinned to its member's scope index
	// (ColumnExpr.UsingPinned) rather than addressed by name, since an unaliased
	// derived member has no name and would collide with same-named columns
	// elsewhere ("ambiguous", joinH.test 16.2.2).
	var outCols []outputColumn
	for i, sc := range subC.scopes {
		for _, col := range sc.cols {
			outCols = append(outCols, outputColumn{name: col.Name, expr: ColumnExpr{Name: col.Name, UsingPinned: true, UsingPinnedItem: i}})
		}
	}
	for _, oc := range outCols {
		if err := checkExprSupported(oc.expr); err != nil {
			return joinSource{}, declineOrSemantic(err)
		}
	}
	prog, perr := compileGroupBody(subC, spanSrcs, outCols)
	if perr != nil {
		return joinSource{}, perr
	}

	allCols := make([]columnInfo, 0, len(outCols))
	for _, sc := range subC.scopes {
		allCols = append(allCols, sc.cols...)
	}
	groupTbl := &resolvedTable{cols: allCols, ipkIndex: -1}
	cursor := c.allocCursor()
	slot := c.allocSub()

	// memberScopes: one compileScope per leaf member (subC.scopes), all on this
	// group's derived cursor, each with colBase at its start in the flattened row.
	// A nested group's own colBase is replaced, not added, since compileGroupBody
	// re-materialized its values into this row. noRowid is forced for every member.
	// colBases are computed first because resolving a member's coalesce fallback
	// may need another member's base.
	colBases := make([]int, len(subC.scopes))
	base := 0
	for k, sc := range subC.scopes {
		colBases[k] = base
		base += len(sc.cols)
	}

	memberScopes := make([]compileScope, len(subC.scopes))
	for k, sc := range subC.scopes {
		ts := sc.tableScope
		ts.noRowid = true
		// Re-address a member's coalesce fallback onto this group's cursor: an owner's
		// address is {cursor, colBases[ownerIdx] + its local column}. A nested group's
		// connector already has a resolved chain; it is shifted by colBases[connector]
		// rather than dropped, because the enclosing coalesce names the whole inner
		// chain (see coalesceOwnersCombined).
		var resolved map[string][]coalesceOwner
		rebase := func(lname string, shift int, owners []coalesceOwner) {
			if len(owners) == 0 {
				return
			}
			if resolved == nil {
				resolved = make(map[string][]coalesceOwner, 2)
			}
			for _, o := range owners {
				resolved[lname] = append(resolved[lname], coalesceOwner{cursor: cursor, colIdx: shift + o.colIdx})
			}
		}
		for lname, inner := range sc.resolvedFallback {
			rebase(lname, colBases[k], inner)
		}
		if ts.coalesceFallback != nil {
			for lname, ownerIdxs := range ts.coalesceFallback {
				for _, ownerIdx := range ownerIdxs {
					if ownerIdx < 0 || ownerIdx >= len(subC.scopes) {
						continue
					}
					ownerColIdx, ok := subC.scopes[ownerIdx].colIndex[lname]
					if !ok {
						continue
					}
					rebase(lname, colBases[ownerIdx], []coalesceOwner{{colIdx: ownerColIdx}})
					// The owner may ITSELF be a nested group's connector, whose
					// own exposed value is a coalesce over ITS members -- the
					// same one-ADD re-addressing, from the owner's own base.
					rebase(lname, colBases[ownerIdx], subC.scopes[ownerIdx].resolvedFallback[lname])
				}
			}
			// The owner indices are stale now, so walking consumers must find nothing, but
			// the keys must stay: expandSelectList's "*" decides between the coalesced and
			// raw column by key presence ("SELECT * FROM (t1 RIGHT JOIN t2 USING(a))" showed
			// t1's NULL when the map was cleared). Empty owner lists behave like nil. The
			// map must stay live so an enclosing RIGHT/FULL USING can append its owner after
			// the resolved chain.
			marker := make(map[string][]int, len(ts.coalesceFallback))
			for lname := range ts.coalesceFallback {
				marker[lname] = nil
			}
			ts.coalesceFallback = marker
		}
		memberScopes[k] = compileScope{tableScope: ts, cursor: cursor, colBase: colBases[k], resolvedFallback: resolved}
	}
	// The connector's own outer-facing coalesced map (hidden, from THIS
	// function's own desugarJoinItem call above) -- NOT whatever the
	// internal recursive resolution computed for span[0] (always nil: its
	// own Join/On/Using/Natural were cleared) -- is what an unqualified
	// reference from OUTSIDE the group must consult, exactly like an
	// ordinary item's tableScope.coalesced.
	memberScopes[0].coalesced = hidden

	// Members reached through a nested connector carry that nested group's alias
	// (x), which is not visible outside this group: "no such column: x.a", while
	// y.a works. So stamp every member with this group's own alias, including ""
	// for an unaliased outer wrap (C's rebuild makes the same anonymous subquery
	// either way, parse.y:810-814, and resolve.c:419 matches only a non-NULL
	// alias), so "x.b" stops resolving from outside. Only at gl > 0, the call
	// building this span: at gl == 0 the inner "x" must still be visible while
	// t2x's ON clause compiles.
	if gl > 0 && it.NestedGroupSpan != nil {
		for k := range memberScopes {
			memberScopes[k].groupAlias = it.GroupAlias
		}
	}

	return joinSource{
		tbl:          groupTbl,
		scope:        memberScopes[0],
		left:         it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:   it.Join == JoinRight || it.Join == JoinFull,
		on:           onExpr,
		derived:      prog,
		derivedSlot:  slot,
		memberScopes: memberScopes,
	}, nil
}

// resolveNestedGroupConnectorCols resolves a NestedGroupSpan far enough to learn
// its flattened columns, which the connector's outward desugar needs before the
// span is built. Like the view-connector branch it is a throwaway pass, so the
// view reference counts it adds are rolled back, on every pager a view inside
// the span could touch (p and its attached readers).
func (p *ReadOnlyPager) resolveNestedGroupConnectorCols(span []FromItem) ([]columnInfo, error) {
	type refSnapshot struct {
		pager *ReadOnlyPager
		saved map[string]int
	}
	snapshots := make([]refSnapshot, 0, 1+len(p.attachedReaders))
	snapshot := func(rp *ReadOnlyPager) {
		saved := make(map[string]int, len(rp.viewRefCount))
		for k, v := range rp.viewRefCount {
			saved[k] = v
		}
		snapshots = append(snapshots, refSnapshot{pager: rp, saved: saved})
	}
	snapshot(p)
	for _, ar := range p.attachedReaders {
		snapshot(ar.pager)
	}
	restore := func() {
		for _, s := range snapshots {
			s.pager.viewRefCount = s.saved
		}
	}

	// A disposable compiler and state; only gs.tbl.cols survives. span[0].GroupLen
	// is the real span length, so this reaches the ordinary connector dispatch.
	throwawayC := &compiler{pager: p}
	var throwawayState joinDesugarState
	gs, err := p.resolveGroupSource(throwawayC, &throwawayState, 0, span)
	restore()
	if err != nil {
		return nil, err
	}
	return gs.tbl.cols, nil
}

// compileGroupBody compiles a join group's resolved internal sources into a
// run-once sub-Program producing one outCols row per combination its own joins
// allow: emitJoinLoops in identity order with no WHERE, LIMIT or DISTINCT.
func compileGroupBody(subC *compiler, spanSrcs []joinSource, outCols []outputColumn) (*Program, error) {
	initAddr := subC.emit(Instruction{Op: OpInit})
	subC.patch(initAddr, 1)
	resultBase := subC.allocN(len(outCols))
	plan := joinPlan{execOrder: identityOrder(len(spanSrcs)), buckets: make([][]Expr, len(spanSrcs))}
	body := func() error {
		for i, oc := range outCols {
			reg, err := subC.compileExpr(oc.expr)
			if err != nil {
				return err
			}
			subC.emit(Instruction{Op: OpSCopy, P1: reg, P2: resultBase + i})
		}
		subC.emit(Instruction{Op: OpResultRow, P1: resultBase, P2: len(outCols)})
		return nil
	}
	if err := emitJoinLoops(subC, spanSrcs, plan, body); err != nil {
		return nil, err
	}
	subC.emit(Instruction{Op: OpHalt})
	cols := make([]string, len(outCols))
	for i, oc := range outCols {
		cols[i] = oc.name
	}
	return &Program{
		Insns:      subC.insns,
		NReg:       subC.nReg,
		NCursors:   subC.nCursor,
		NRecRegs:   subC.nRec,
		NSubCache:  subC.nSub,
		NDistinct:  subC.nDistinct,
		NResultCol: len(outCols),
		ColNames:   cols,
		Correlated: subC.correlated,
	}, nil
}

// onsOfSources extracts srcs' effective join conditions (each already
// desugared from USING/NATURAL, if applicable, by resolveJoinSources) in FROM
// order -- the []Expr shape checkFromSupported (join.go) takes, mirroring
// join.go's own onsOf for []joinedTable.
func onsOfSources(srcs []joinSource) []Expr {
	ons := make([]Expr, len(srcs))
	for i, s := range srcs {
		ons[i] = s.on
	}
	return ons
}

// joinScopes extracts srcs' compileScopes, in FROM order -- the c.scopes
// value a join compile installs so compileColumn/compAffinity/inAffinity
// resolve a bare/qualified column reference against every joined table. A
// materialized parenthesized join GROUP source (joinSource.memberScopes)
// contributes EVERY one of its own leaf members' scopes here, in FROM
// order, not just its own connector's -- see sourceScopes -- so a reference
// to any table inside a group resolves exactly as if the group had never
// been parenthesized at all.
func joinScopes(srcs []joinSource) []compileScope {
	var scopes []compileScope
	for _, s := range srcs {
		scopes = append(scopes, sourceScopes(s)...)
	}
	return scopes
}

// tableScopesOf extracts srcs' tableScopes (dropping the cursor number) --
// the []tableScope shape expandSelectList/planNoGroupAggregate/
// planGroupByStmt take. Flattened through
// sourceScopes exactly like joinScopes, for the same reason.
func tableScopesOf(srcs []joinSource) []tableScope {
	scopes := joinScopes(srcs)
	ts := make([]tableScope, len(scopes))
	for i, s := range scopes {
		ts[i] = s.tableScope
	}
	return ts
}

// totalCols returns the combined column count across every source -- the
// [cols..] block width the aggregate opcodes' row-gathering layout uses (see
// aggPlan.nCols, vdbe_agg.go).
func totalCols(srcs []joinSource) int {
	n := 0
	for _, s := range srcs {
		n += len(s.tbl.cols)
	}
	return n
}

// cursorsOf extracts srcs' cursor numbers, in FROM order -- aggPlan.cursors,
// for OpAggStep's P3==0 (whole-table, no GROUP BY) cursor-gather mode.
func cursorsOf(srcs []joinSource) []int {
	// Per LEAF SCOPE, not per source: this list is indexed alongside
	// aggPlan.scopes, which tableScopesOf flattens a materialized
	// parenthesized join GROUP into one entry per member. sourceScopes is what
	// keeps the two the same length and order; for an ordinary source it
	// returns the single scope this used to read directly.
	cs := make([]int, 0, len(srcs))
	for _, s := range srcs {
		for _, sc := range sourceScopes(s) {
			cs = append(cs, sc.cursor)
		}
	}
	return cs
}

// gathersOf describes the ROW COPY for an aggregate's cursor-gather mode: one
// entry per PHYSICAL source, at that source's own offset in the [cols..]
// block and as wide as the columns it exposes. A materialized join GROUP is
// ONE entry here even though it is several scopes -- its single cursor already
// carries every member's columns concatenated, in the same order the block
// expects them. See aggPlan.gathers.
func gathersOf(srcs []joinSource) []aggGather {
	gs := make([]aggGather, 0, len(srcs))
	for _, s := range srcs {
		gs = append(gs, aggGather{cursor: s.scope.cursor, offset: s.scope.offset, n: len(s.tbl.cols)})
	}
	return gs
}

// joinPushdownPlan computes the join plan by calling join.go's planJoinPushdown
// on a throwaway []joinedTable with the on/left fields it reads. It returns
// execOrder (the binding order, identical to computeExecOrder), buckets[pos] (WHERE
// conjuncts testable right after the table bound at that depth, emitted by
// emitJoinLevel), and deferred (conjuncts that need the full, possibly
// NULL-extended row, emitted by the caller's body). Reusing join.go's planner
// keeps row order and ON-vs-WHERE safety identical.
func joinPushdownPlan(srcs []joinSource, scopes []tableScope, where Expr) joinPlan {
	return planJoinPushdown(where, joinedTablesFor(srcs), scopes)
}

// joinedTablesFor projects a resolved FROM clause onto the []joinedTable that
// planJoinPushdown and the ported SQLite planner read. It is the ONE place the
// projection lives, so a caller that only wants to ASK the planner something
// (wherePlanDecidedOrder, where_plan_gate.go) cannot drift from the one that
// actually plans.
func joinedTablesFor(srcs []joinSource) []joinedTable {
	jts := make([]joinedTable, len(srcs))
	for i, s := range srcs {
		jts[i] = joinedTable{
			left: s.left, on: s.on, rightOuter: s.rightOuter,
			// tbl/cross/wherePlanOK are read only by the ported SQLite
			// planner (computeExecOrder -> wherePlanOrder, where_plan.go):
			// column affinities for its automatic-index test, the CROSS
			// reorder barrier, and whether this FROM clause is inside the
			// subset it reproduces exactly at all.
			tbl: s.tbl, cross: s.cross, wherePlanOK: s.wherePlanOK,
			colUsed: s.colUsed, colUsedOK: s.colUsedOK, onToWhere: s.onToWhere,
			noAutoIndex: s.noAutoIndex, idxOrderKey: s.idxOrderKey,
			isFtsVtab: s.isFtsVtab,
		}
		if s.vtabItem != nil && s.vtabItem.TableFunc {
			jts[i].tvfArgs = s.vtabItem.TableFuncArgs
		}
	}
	return jts
}

// andConjuncts recombines a conjunct list into a single left-associated AND
// expression (nil for an empty list) -- the inverse of splitTopLevelAnd
// (join.go), used to hand a join body the DEFERRED subset of WHERE (the
// conjuncts planJoinPushdown could not safely push earlier) as one Expr, so
// each body's existing single-Expr WHERE handling needs no structural change:
// it simply evaluates this deferred residue instead of the whole WHERE, the
// rest having already been tested (and pruned on) at earlier join levels.
func andConjuncts(cjs []Expr) Expr {
	if len(cjs) == 0 {
		return nil
	}
	e := cjs[0]
	for _, cj := range cjs[1:] {
		e = BinaryExpr{Op: "AND", L: e, R: cj}
	}
	return e
}

// emitJoinLoops emits OpOpenRead for every source in FROM order (cursor numbers
// follow FROM position), nested Rewind/Next loops in plan.execOrder around body
// (called once per joined row at the innermost level), then OpClose for each.
// Pushed-down conjuncts (plan.buckets) are tested per level by emitJoinLevel;
// plan.deferred is the caller's to test inside body.
func emitJoinLoops(c *compiler, srcs []joinSource, plan joinPlan, body func() error) error {
	// A virtual-table source whose constraint names a table bound earlier in
	// this nesting is driven from INSIDE the join instead of above it -- the
	// open below is skipped for it and emitJoinLevel emits its own. Decided
	// here, the single funnel every join-shaped codegen path reaches, so the
	// skip and the emission can never disagree. See vtab_correlated.go.
	annotateVtabCorrelations(c, srcs, plan)
	// The source bound at execOrder[0] is the OUTERMOST nested loop, whose
	// OpRewind runs exactly once per program execution (every deeper level
	// rewinds once per outer-row combination). A plain full scan over a rowid
	// table there -- with no seek hint and not itself a RIGHT/FULL JOIN sweep
	// target -- can therefore iterate the b-tree LAZILY instead of materializing
	// the whole table (see vdbeCursor.streamable). Deeper levels, and any
	// rightOuter/seek/withoutRowid/derived source, keep materializing.
	streamOuter := -1
	if len(plan.execOrder) > 0 {
		o := plan.execOrder[0]
		s := srcs[o]
		if s.derived == nil && s.vtabItem == nil && s.cteItem == nil && s.catalogScope == scopeAny &&
			!s.rightOuter && s.seekKeyExpr == nil && s.idxSeek == nil && !s.tbl.withoutRowid &&
			(len(plan.autoIdxKeys) == 0 || plan.autoIdxKeys[0] == nil) {
			// A level carrying an index-order key must MATERIALIZE: the key is
			// a permutation of the whole row set, which a one-row-at-a-time
			// b-tree pull has nothing to permute. See OpAutoIndexOrder.
			streamOuter = o
		}
	}
	// The same once-per-run outermost level, when it is a DERIVED source (a
	// subquery, a view, or a recursive CTE's queue): it may run as a
	// co-routine, fromClauseTermCanBeCoroutine's arrangement (select.c:7266),
	// but only while this program is itself one -- see streamIfYielding.
	streamDerived := -1
	if len(plan.execOrder) > 0 {
		o := plan.execOrder[0]
		s := srcs[o]
		if (s.derived != nil || (s.cteItem != nil && s.cteItem.queue != nil)) && !s.rightOuter &&
			(len(plan.autoIdxKeys) == 0 || plan.autoIdxKeys[0] == nil) {
			streamDerived = o
		}
	}
	for i, s := range srcs {
		if s.derived != nil {
			ds := &derivedSource{prog: s.derived, tbl: s.tbl, dbIdx: s.dbIdx, keepSubtype: s.keepSubtype}
			if i == streamDerived {
				ds.stream = streamIfYielding
			}
			c.emit(Instruction{Op: OpOpenDerived, P1: s.scope.cursor, P2: s.derivedSlot, P4: ds})
			continue
		}
		if s.catalogScope != scopeAny {
			c.emit(Instruction{Op: OpOpenDerived, P1: s.scope.cursor, P2: s.derivedSlot, P4: &derivedSource{catalogScope: s.catalogScope, tbl: s.tbl, dbIdx: s.dbIdx}})
			continue
		}
		if s.vtabItem != nil {
			if len(srcs[i].vtabCorr) > 0 {
				// Correlated: its rows depend on an outer row that does not
				// exist yet here, so its ONLY open is the one
				// emitVtabCorrelatedOpen emits inside the join. Leaving the
				// cursor nil until then is safe -- nothing reads it before its
				// own level's code runs, and OpClose tolerates a nil cursor,
				// which is what an outer loop that never produced a row leaves.
				continue
			}
			// c.trig travels with the item: a table-valued function's argument
			// is folded at RUN time, and that is the only channel by which
			// "json_each(NEW.x)" in a trigger body can reach the firing row.
			// See derivedSource.vtabTrig (vdbe_op.go).
			c.emit(Instruction{Op: OpOpenDerived, P1: s.scope.cursor, P2: s.derivedSlot, P4: &derivedSource{vtab: s.vtabItem, vtabTrig: c.trig, tbl: s.tbl, dbIdx: s.dbIdx}})
			continue
		}
		if s.cteItem != nil {
			ds := &derivedSource{cte: s.cteItem, tbl: s.tbl, dbIdx: s.dbIdx, keepSubtype: s.keepSubtype}
			switch {
			case s.cteItem.recSelf != nil:
				ds = &derivedSource{recSelf: s.cteItem.recSelf, tbl: s.tbl}
			case s.cteItem.queue != nil:
				ds = &derivedSource{prog: s.cteItem.queue, tbl: s.tbl, dbIdx: s.dbIdx}
				if i == streamDerived {
					ds.stream = streamIfYielding
				}
			}
			c.emit(Instruction{Op: OpOpenDerived, P1: s.scope.cursor, P2: s.derivedSlot, P4: ds})
			continue
		}
		var p5 uint16
		if i == streamOuter {
			p5 = 1
		}
		if s.flattened {
			p5 |= 2 // see returningSubqueryLifetimes
		}
		c.emit(Instruction{Op: OpOpenRead, P1: s.scope.cursor, P2: int(s.tbl.root), P3: s.dbIdx, P4: s.tbl, P5: p5})
		if s.seekKeyExpr != nil {
			// Evaluate the (column-free constant/parameter) seek key now --
			// before any Rewind, so no cursor row needs to be positioned --
			// and hand it to the cursor. See OpSeekRowidHint (vdbe_op.go) and
			// detectRowidSeekKey (vdbe_scan.go).
			keyReg, kerr := c.compileExpr(s.seekKeyExpr)
			if kerr != nil {
				return kerr
			}
			c.emit(Instruction{Op: OpSeekRowidHint, P1: s.scope.cursor, P2: keyReg})
		}
		if s.idxSeek != nil {
			// Evaluate the (column-free constant/parameter) index seek key now --
			// before any Rewind -- and hand it, with the index's root/affinity/
			// collation, to the cursor. See OpSeekIndexHint (vdbe_op.go) and
			// detectIndexSeekKey (vdbe_scan.go).
			keyReg, kerr := c.compileExpr(s.idxSeek.keyExpr)
			if kerr != nil {
				return kerr
			}
			c.emit(Instruction{Op: OpSeekIndexHint, P1: s.scope.cursor, P2: keyReg,
				P4: &indexSeekHint{root: s.idxSeek.root, aff: s.idxSeek.aff, coll: s.idxSeek.coll, leadingCol: s.idxSeek.leadingCol}})
		}
	}
	// The transient automatic index: C builds it once per run under OP_Once, from
	// the table alone, so here it is a one-time reordering of the inner cursor's
	// rows before any loop. Never for execOrder[0] (nRow 0 fails "nRow < 3"). See
	// OpAutoIndexOrder and autoIndexKey.
	for level, key := range plan.autoIdxKeys {
		if key == nil || key.multiOr != nil && key.multiOr.passes != nil {
			// A multi-table WHERE_MULTI_OR level is ordered inside its own loop,
			// once per outer row: see emitMultiOrGroups.
			continue
		}
		c.emit(Instruction{Op: OpAutoIndexOrder, P1: srcs[plan.execOrder[level]].scope.cursor, P4: key})
	}
	if err := c.emitJoinLevel(srcs, plan, 0, body); err != nil {
		return err
	}
	// RIGHT/FULL second passes, one per rightOuter source in FROM order (execOrder
	// is the identity whenever one exists). By the time source L's sweep runs, the
	// main loop and every earlier sweep have marked it, so its matched set is
	// complete. Its unmatched rows continue into the following levels, so they feed
	// later joins, including a later rightOuter's matching.
	order := plan.execOrder
	for level := 0; level < len(srcs); level++ {
		if srcs[order[level]].rightOuter {
			if err := c.emitRightOuterSweep(srcs, plan, order, level, body); err != nil {
				return err
			}
		}
	}
	for i := len(srcs) - 1; i >= 0; i-- {
		c.emit(Instruction{Op: OpClose, P1: srcs[i].scope.cursor})
	}
	return nil
}

// emitRightOuterSweep emits the RIGHT/FULL second pass for the source at
// physical level `level`: a loop over the rows OpRightJoinMark never marked,
// continuing into levels level+1.. for each, with every earlier cursor in the
// OpNullRow state. OpRightJoinSweepRewind/Next do the scan and NULLing. Called
// from emitJoinLoops after the main loop, never inside emitJoinLevel, where it
// would re-run per outer row.
func (c *compiler) emitRightOuterSweep(srcs []joinSource, plan joinPlan, order []int, level int, body func() error) error {
	cur := srcs[order[level]].scope.cursor
	others := make([]int, 0, level)
	for i := 0; i < level; i++ {
		others = append(others, srcs[order[i]].scope.cursor)
	}
	rewindJump := c.emit(Instruction{Op: OpRightJoinSweepRewind, P1: cur, P4: others})
	loopAddr := c.here()
	if err := c.emitJoinLevel(srcs, plan, level+1, body); err != nil {
		return err
	}
	c.emit(Instruction{Op: OpRightJoinSweepNext, P1: cur, P2: loopAddr, P4: others})
	c.patch(rewindJump, c.here()) // no unmatched rows at all -> past the sweep entirely
	return nil
}

// emitMultiOrGroups orders the level's cursor as a multi-table WHERE_MULTI_OR
// loop visits it, right before the level's OpRewind (when the case-5 arm empties
// its RowSet, wherecode.c:2339). A no-op for other levels. Each row belongs to
// the first pass it satisfies (OP_RowSetTest, wherecode.c:2450), decided by the
// compiled passes with outer cursors positioned:
//
//	Rewind cur -> done
//	top: g = 0; if pass0 goto tag; g = 1; if pass1 goto tag; ... g = n
//	tag: MultiOrTag cur, g
//	     Next cur -> top
//	done: MultiOrSort cur   -- by (g, pass g's sub-scan key)
//
// Only the order changes; a row satisfying no pass is filed last and fails the
// OR the body still tests.
func (c *compiler) emitMultiOrGroups(plan joinPlan, level, cur int) error {
	if level >= len(plan.autoIdxKeys) {
		return nil
	}
	key := plan.autoIdxKeys[level]
	if key == nil || key.multiOr == nil || key.multiOr.passes == nil {
		return nil
	}
	mo := key.multiOr
	rewind := c.emit(Instruction{Op: OpRewind, P1: cur})
	top := c.here()
	g := c.alloc()
	var hits []int
	for i, d := range mo.passes {
		c.emit(Instruction{Op: OpInteger, P1: i, P2: g})
		reg, err := c.compileExpr(d)
		if err != nil {
			return err
		}
		hits = append(hits, c.emit(Instruction{Op: OpIf, P1: reg})) // NULL does not jump
	}
	c.emit(Instruction{Op: OpInteger, P1: len(mo.passes), P2: g})
	for _, h := range hits {
		c.patch(h, c.here())
	}
	c.emit(Instruction{Op: OpMultiOrTag, P1: cur, P2: g})
	c.emit(Instruction{Op: OpNext, P1: cur, P2: top})
	c.patch(rewind, c.here())
	c.emit(Instruction{Op: OpMultiOrSort, P1: cur, P4: mo})
	return nil
}

// emitJoinLevel emits nested loop level `level` (a physical depth) binding
// srcs[plan.execOrder[level]], and calls body at the innermost level for every
// joined row. Rewind/Next targets are local to the level, which is re-entered
// once per outer combination.
//
// WHERE pushdown: after the cursor is positioned (and an INNER ON passes), the
// conjuncts planJoinPushdown assigned to this depth are tested, jumping to Next
// when false or NULL. Buckets are empty from the first LEFT join on, so a
// WHERE term never affects match-vs-NULL-extend.
//
// An ON expression compiles with c.scopes narrowed to the sources already
// bound at this depth: an ON may reference preceding tables only, and an
// unbound cursor has no row yet. Bucket conjuncts compile under the full scope,
// since every table they name is bound here.
func (c *compiler) emitJoinLevel(srcs []joinSource, plan joinPlan, level int, body func() error) error {
	order := plan.execOrder
	if level == len(srcs) {
		return body()
	}
	origIdx := order[level]
	s := srcs[origIdx]
	cur := s.scope.cursor

	compileOn := func() (int, error) {
		// A subquery in an ON condition compiles like one in WHERE, with this compiler
		// as its outer, while c.scopes is narrowed to the sources bound at this level.
		// So a correlated reference can name only tables already joined (SQL's ON
		// scoping) and every cursor it reads is positioned (rowLive is already set).
		saved := c.scopes
		var visible []compileScope
		for i := 0; i <= level; i++ {
			// sourceScopes, not .scope directly: a materialized parenthesized
			// join GROUP bound at physical level i contributes EVERY one of
			// its own leaf members here, not just its connector's -- an ON
			// condition at a later level (or this same group's own connector
			// on, tested via this same helper) may reference any of them.
			visible = append(visible, sourceScopes(srcs[order[i]])...)
		}
		c.scopes = visible
		// ...and while they are narrowed, this compiler is not a safe target for
		// the aggregate-association walk: a column its FULL FROM does supply
		// would look absent here, handing the aggregate to a query further out
		// and changing the wrong query's row count. See hoistOwnerFor
		// (vdbe_agg_hoist.go), which refuses to walk past a narrowed level.
		if c.hoist != nil {
			c.hoist.narrowed = true
			defer func() { c.hoist.narrowed = false }()
		}
		defer func() { c.scopes = saved }()
		// An outer join's ON is still a top-level WHERE AND-conjunct in C's sense:
		// sqlite3ProcessJoin ANDs every ON into the WHERE (select.c:655) before the
		// vector-equality rewrite runs (where.c:6941, 6990). So it compiles with
		// inWhereConjunct set ("mm LEFT JOIN side ON (mm.a,mm.b) = (SELECT 'abc'
		// COLLATE nocase, 1)" uses BINARY).
		c.inWhereConjunct = true
		return c.compileExpr(s.on)
	}

	// emitBucket compiles this level's pushed-down WHERE conjuncts as pruning
	// tests, returning the OpIfNot jump addresses for the caller to patch to
	// this level's Next. A conjunct that evaluates false/NULL skips straight
	// to this table's next row.
	emitBucket := func() ([]int, error) {
		var jumps []int
		for _, cj := range plan.buckets[level] {
			// cj is one of plan's top-level WHERE AND-conjuncts (splitTopLevelAnd,
			// join.go, plus any onToWhere-promoted ON conjunct -- indistinguishable
			// from an ordinary WHERE conjunct by the time sqlite3ProcessJoin's own
			// rewrite has run in C SQLite too), pushed to this execution level
			// by planJoinPushdown/joinPushdownPlan. It is exactly the WhereClause
			// membership whereexpr.c's tag-20220128a guards on ("pWC->op==TK_AND")
			// -- see compiler.inWhereConjunct's doc comment.
			c.inWhereConjunct = true
			reg, err := c.compileExpr(cj)
			if err != nil {
				return nil, err
			}
			jumps = append(jumps, c.emit(Instruction{Op: OpIfNot, P1: reg, P3: 1}))
		}
		return jumps, nil
	}

	// A CORRELATED virtual-table source's one and only open: right here, inside
	// every outer loop, with its constraint right-hand sides read off the
	// cursors this level's outer loops have positioned -- where C emits
	// OP_VFilter (wherecode.c:1610). A no-op unless annotateVtabCorrelations
	// recorded terms. It must precede emitJoinSeekHint below, which
	// dereferences this cursor. See vtab_correlated.go.
	if err := c.emitVtabCorrelatedOpen(srcs, order, level, s); err != nil {
		return err
	}

	if !s.left {
		// Correlated inner-side seek: right before this level's OpRewind (when
		// every outer cursor is positioned), evaluate the join key and configure
		// the cursor to re-seek its rowid/index instead of full-scanning. A no-op
		// unless annotateJoinSeeks recorded a plan for this source; the join
		// condition below is still evaluated in full, so this only prunes the
		// inner row source. See emitJoinSeekHint (vdbe_join_seek.go).
		if err := c.emitJoinSeekHint(srcs, order, level, s); err != nil {
			return err
		}
		if err := c.emitMultiOrGroups(plan, level, cur); err != nil {
			return err
		}
		rewindJump := c.emit(Instruction{Op: OpRewind, P1: cur})
		loopAddr := c.here()
		onJump := -1
		if s.on != nil && !s.onToWhere {
			// s.onToWhere: sqlite3ProcessJoin moved this INNER item's ON into the
			// WHERE clause, so planJoinPushdown already bucketed its conjuncts to
			// whichever depth binds every table they name -- emitBucket below
			// tests them there. Emitting the ON here too would be redundant, and
			// at an order that binds this item first it could not even resolve.
			onReg, err := compileOn()
			if err != nil {
				return err
			}
			onJump = c.emit(Instruction{Op: OpIfNot, P1: onReg, P3: 1}) // ON false/NULL -> Next (patched below)
		}
		if s.rightOuter {
			// RIGHT JOIN (left==false, rightOuter==true): record the global
			// match now -- ON alone decides match-vs-unmatched (see
			// emitRightOuterSweep's doc comment and join.go's package doc
			// comment), so this must run BEFORE the bucket test below, and
			// onJump (patched to skip straight past this whole block on ON
			// false/NULL) already ensures it never runs unless ON passed.
			c.emit(Instruction{Op: OpRightJoinMark, P1: cur})
		}
		bucketJumps, err := emitBucket()
		if err != nil {
			return err
		}
		if err := c.emitJoinLevel(srcs, plan, level+1, body); err != nil {
			return err
		}
		nextAddr := c.emit(Instruction{Op: OpNext, P1: cur, P2: loopAddr})
		if onJump >= 0 {
			c.patch(onJump, nextAddr)
		}
		for _, j := range bucketJumps {
			c.patch(j, nextAddr) // pushed-down WHERE false/NULL -> this table's next row
		}
		c.patch(rewindJump, c.here()) // empty -> past this level's own Next, contributing nothing
		return nil
	}

	// LEFT JOIN: match flag plus NullRow fallback. No bucket test (buckets are empty
	// from the first LEFT join on); s.on decides the match, and is nil only for a
	// NATURAL LEFT JOIN with no common columns, where every row matches. The
	// correlated seek is safe because the flag is set only after ON passes on a
	// fetched row; an empty seek falls through with the flag 0, as an exhausted scan
	// does. The seek keys off ON conjuncts only.
	if err := c.emitJoinSeekHint(srcs, order, level, s); err != nil {
		return err
	}
	matchReg := c.alloc()
	c.emit(Instruction{Op: OpInteger, P1: 0, P2: matchReg})
	if err := c.emitMultiOrGroups(plan, level, cur); err != nil {
		return err
	}
	rewindJump := c.emit(Instruction{Op: OpRewind, P1: cur})
	loopAddr := c.here()
	onJump := -1
	if s.on != nil {
		onReg, err := compileOn()
		if err != nil {
			return err
		}
		onJump = c.emit(Instruction{Op: OpIfNot, P1: onReg, P3: 1}) // no match -> Next
	}
	c.emit(Instruction{Op: OpInteger, P1: 1, P2: matchReg})
	if s.rightOuter {
		// FULL JOIN (left==true, rightOuter==true): feeds the SAME global
		// match bookkeeping RIGHT's branch above does -- see this file's
		// package doc comment and join.go's package doc comment (RIGHT/FULL
		// section): FULL is exactly LEFT's own inline per-combination
		// NULL-extension (unchanged, below) PLUS RIGHT's global second pass.
		c.emit(Instruction{Op: OpRightJoinMark, P1: cur})
	}
	if err := c.emitJoinLevel(srcs, plan, level+1, body); err != nil {
		return err
	}
	nextAddr := c.emit(Instruction{Op: OpNext, P1: cur, P2: loopAddr})
	if onJump >= 0 {
		c.patch(onJump, nextAddr)
	}
	c.patch(rewindJump, c.here()) // empty right table -> straight to the match check below, flag still 0

	skipNull := c.emit(Instruction{Op: OpIf, P1: matchReg}) // matched at least once -> skip NULL-extension
	c.emit(Instruction{Op: OpNullRow, P1: cur})
	if err := c.emitJoinLevel(srcs, plan, level+1, body); err != nil {
		return err
	}
	c.patch(skipNull, c.here())
	return nil
}
