// This file implements the compile-time validation C applies to a subquery in
// "X [NOT] IN (SELECT ...)", "(SELECT ...)" or "[NOT] EXISTS (SELECT ...)" --
// checks C raises at prepare time however many outer rows there are. A check
// that fired only while evaluating ("sub-select returns N columns - expected
// 1", "no such table") would never fire over an empty outer table, so the
// statement would wrongly succeed (TCL in-12.*, func6).
//
// A violation is errVDBESemantic, propagated as the statement's own error; a
// shape the validators cannot analyze is errVDBEUnsupported.
package engine

import (
	"errors"
	"fmt"
)

// errVDBESemantic marks a compile-time error that C SQLite ALSO raises
// (wrong subquery column count, a missing table named only inside a subquery,
// a compound SELECT whose arms disagree on column count). Unlike
// errVDBEUnsupported -- which means "the compiler cannot lower this shape at
// all" and surfaces as "not compilable to bytecode" (query.go) -- an
// errVDBESemantic must be surfaced to the caller as the statement's real
// error text, because that text is what C SQLite raises unconditionally at
// prepare time and the conformance gate compares against.
var errVDBESemantic = errors.New("vdbe: semantic error")

// semanticError carries errVDBESemantic without putting its text in front of
// the message: errors.Is(err, errVDBESemantic) holds, and Error() is exactly
// the error C SQLite raises. Wrapping with "%w: %v" used to leak "vdbe:
// semantic error: " into every such message, so "INSERT INTO nonexistent
// VALUES(1)" read "vdbe: semantic error: engine: no such table: nonexistent"
// where C SQLite says "no such table: nonexistent".
type semanticError struct{ msg string }

func (e *semanticError) Error() string        { return e.msg }
func (e *semanticError) Is(target error) bool { return target == errVDBESemantic }

// semanticf is fmt.Errorf for errVDBESemantic; see semanticError.
func semanticf(format string, args ...any) error {
	return &semanticError{msg: fmt.Sprintf(format, args...)}
}

// subqueryScopes resolves a subquery's own FROM tables into schema-only
// tableScopes (no cursors, no row values -- enough for "*" expansion and
// column-affinity resolution). A missing table is a genuine C-SQLite error
// (errVDBESemantic: "no such table"); a FROM item this engine's grammar never
// produces (a derived table has no name at all) is un-analyzable and yields
// errVDBEUnsupported so the caller falls back.
func (p *ReadOnlyPager) subqueryScopes(from []FromItem) ([]tableScope, error) {
	var scopes []tableScope
	offset := 0
	for _, it := range from {
		if it.Table == "" {
			return nil, fmt.Errorf("%w: subquery over a derived table", errVDBEUnsupported)
		}
		// Resolve the owning pager as resolveJoinSources does (itemOwner): p
		// for a local name, or the attachment owning a foreign-qualified name
		// or an unqualified name that lives only there -- otherwise "id IN
		// (SELECT aid FROM aux.b)" would report "no such table".
		//
		// A CTE shadowing a same-named table wins unconditionally:
		// selectExpander tries resolveFromTermToCte first (select.c:6028) and
		// sqlite3LocateTableItem is the else (6036). Resolving the table first
		// let its declared collation decide the comparison:
		//
		//	CREATE TABLE u(x TEXT COLLATE NOCASE); INSERT INTO u VALUES('ABC');
		//	UPDATE u SET x='zz'
		//	  WHERE 'abc' IN (WITH u(x) AS (SELECT 'ABC') SELECT x FROM u)
		//	  -- C leaves the row alone
		//
		// A schema-qualified term is never a CTE (select.c:5698). The decline
		// is errVDBEUnsupported, which for subqueryOutputCollations/Affinities
		// means "no RHS contribution" -- BINARY here, C's answer.
		if it.Schema == "" {
			if _, ok := p.lookupCTE(it.Table); ok {
				return nil, fmt.Errorf("%w: subquery FROM references a CTE", errVDBEUnsupported)
			}
		}
		rp := p
		if it.Schema != "" || len(p.attachedReaders) > 0 {
			owner, oerr := p.itemOwner(it)
			if oerr != nil {
				return nil, semanticf("%v", oerr)
			}
			rp = owner
		}
		tbl, err := rp.resolveTableIn(fromItemScope(it), it.Table)
		if err != nil {
			// A CTE or view is a valid subquery FROM item, but a schema-only scope
			// for one needs resolveFrom's derived-table machinery, so this
			// analyzer declines (errVDBEUnsupported) and leaves it to the compiler.
			// The CTE check comes first, since a CTE shadows a same-named view too,
			// and both live only in the local schema.
			if _, ok := p.lookupCTE(it.Table); ok {
				return nil, fmt.Errorf("%w: subquery FROM references a CTE", errVDBEUnsupported)
			}
			if _, ok, verr := p.resolveViewByNameIn(fromItemScope(it), it.Table); verr == nil && ok {
				return nil, fmt.Errorf("%w: subquery FROM references a view", errVDBEUnsupported)
			}
			// A virtual table's columns are declared by its module's Connect (what
			// materializeVtab uses), so the scope is built from those. Without
			// this, resolveTableIn cannot see a vtab and "a IN (SELECT value FROM
			// generate_series(1,3))" failed with "no such table".
			if vcols, verr := p.vtabScopeColumns(it); verr == nil {
				name := it.Alias
				if name == "" {
					name = it.Table
				}
				scopes = append(scopes, tableScope{
					name:     name,
					cols:     vcols,
					colIndex: buildColIndex(vcols),
					offset:   offset,
				})
				offset += len(vcols)
				continue
			}
			// A SCHEMA CATALOG read, which resolveTableIn likewise cannot see:
			// sqlite_temp_master is not a table here at all, and sqlite_master
			// deliberately declines there while a temp object exists (query.go).
			// resolveJoinSources compiles both into a filtered row source
			// (temp_catalog.go), and this analyzer only needs the catalog's
			// fixed five-column shape -- exactly the vtab case above. Without
			// it, "... WHERE x IN (SELECT name FROM sqlite_temp_master)" failed
			// with a hard "no such table" for a catalog the compiler CAN serve.
			if _, isCat := catalogItemScope(it); isCat {
				cols := sqliteSchemaCatalogColumns()
				name := it.Alias
				if name == "" {
					name = it.Table
				}
				scopes = append(scopes, tableScope{
					name:     name,
					cols:     cols,
					colIndex: buildColIndex(cols),
					offset:   offset,
				})
				offset += len(cols)
				continue
			}
			return nil, semanticf("%v", err)
		}
		name := it.Alias
		if name == "" {
			name = it.Table
		}
		scopes = append(scopes, tableScope{
			name:     name,
			cols:     tbl.cols,
			colIndex: buildColIndex(tbl.cols),
			offset:   offset,
		})
		offset += len(tbl.cols)
	}
	return scopes, nil
}

// inRHSCore is the select-core whose result expressions "X IN (SELECT ...)"
// combines X's affinity and collation with: exprINAffinity (expr.c:3477) and
// sqlite3CodeRhsOfIN (expr.c:3750-3755) read pExpr->x.pSelect->pEList, which
// for a compound is the rightmost arm (parse.y:624-643 chains the left as
// pPrior). So "a IN (SELECT b FROM x2 UNION SELECT 123)" compares under a's
// TEXT affinity and "a IN (SELECT 123 UNION SELECT b FROM x2)" under none.
//
// A multi-row VALUES list is not analyzed (ok false): its shape depends on
// sqlite3MultiValues, and the fallback (X's affinity alone) is what every
// all-literal list gives.
func inRHSCore(stmt *SelectStmt) (*SelectStmt, bool) {
	n := len(stmt.Compound)
	if n == 0 {
		return stmt, true
	}
	if stmt.ValuesArms > 0 || stmt.Compound[n-1].Stmt == nil {
		return nil, false
	}
	return stmt.Compound[n-1].Stmt, true
}

// subqueryAnalysisScopes is subqueryScopes for the IN-comparison analyses
// above, which also need a CTE's or a view's columns: sqlite3SubqueryColumnTypes
// gives such a column an affinity and a collation (select.c:2377-2398) that the
// comparison combines with X's, and leaving it out -- X alone -- converted
// '123' against a compound CTE column C holds at AFF_BLOB (view.test 31.1).
// Those are resolved by resolveFromSchemaOnly, which compiles each body for its
// column types and runs none of them.
func (p *ReadOnlyPager) subqueryAnalysisScopes(from []FromItem) ([]tableScope, error) {
	scopes, err := p.subqueryScopes(from)
	if err == nil || !errors.Is(err, errVDBEUnsupported) {
		return scopes, err
	}
	jts, serr := p.resolveFromSchemaOnly(from)
	if serr != nil {
		return nil, err
	}
	return buildScopes(jts), nil
}

// armColumnCount returns the number of result columns one select-core (a
// compound arm, or a non-compound SELECT) produces, resolving "*" against its
// own FROM scope. A missing table surfaces as errVDBESemantic (via
// subqueryScopes); an un-analyzable select list yields errVDBEUnsupported.
func (p *ReadOnlyPager) armColumnCount(stmt *SelectStmt) (int, error) {
	scopes, err := p.subqueryScopes(stmt.From)
	if err != nil {
		return 0, err
	}
	outCols, err := expandSelectList(stmt.Columns, scopes, defaultColNameMode)
	if err != nil {
		return 0, declineOrSemantic(err)
	}
	return len(outCols), nil
}

// pushSubqueryCTEs makes stmt's own WITH clause visible for the returned
// func's scope. The validators resolve FROM items from the schema
// (subqueryScopes) before compileSubProgram pushes its own, so "X IN (WITH c
// AS (...) SELECT ... FROM c)" would otherwise be "no such table: c". A no-op
// without a WITH.
func (p *ReadOnlyPager) pushSubqueryCTEs(stmt *SelectStmt) func() {
	if p == nil || len(stmt.CTEs) == 0 {
		return func() {}
	}
	return p.pushCTEScope(stmt.CTEs)
}

// subqueryResultColumns returns how many result columns a (possibly compound)
// subquery produces, validating as C does at prepare time that every
// referenced table exists and that compound arms agree on column count. Those
// failures are errVDBESemantic (resolveTable's message shape); an
// unanalyzable shape is errVDBEUnsupported.
func (p *ReadOnlyPager) subqueryResultColumns(stmt *SelectStmt) (int, error) {
	defer p.pushSubqueryCTEs(stmt)()
	n, err := p.armColumnCount(stmt)
	if err != nil {
		return 0, err
	}
	for i, arm := range stmt.Compound {
		an, err := p.armColumnCount(arm.Stmt)
		if err != nil {
			return 0, err
		}
		if an != n && i < stmt.ValuesArms {
			return 0, semanticf("all VALUES must have the same number of terms") // select.c:3076-3078
		}
		if an != n {
			return 0, semanticf("SELECTs to the left and right of %s do not have the same number of result columns", arm.Op)
		}
	}
	return n, nil
}

// subqueryOutputAffinity returns the declared affinity of a single-column
// (non-compound) subquery's one result column, plus whether that column is a
// materialized reference (a real table column) rather than a computed value --
// the two facts comparisonAffinity needs to combine BOTH operands'
// affinities for the "X IN (SELECT y ...)" rule (SQLite compares X against y
// with their COMBINED affinity, unlike the IN-list form, which coerces only by
// X's own affinity). A shape it can't analyze (compound, missing table, more
// than one column) yields (affNone, false) -- the neutral value that leaves
// comparisonAffinity to decide from X alone, matching the prior behavior.
func (p *ReadOnlyPager) subqueryOutputAffinity(stmt *SelectStmt, outer *evalCtx) (affinity, bool) {
	affs, mats, ok := p.subqueryOutputAffinities(stmt, outer)
	if !ok || len(affs) != 1 {
		return affNone, false
	}
	return affs[0], mats[0]
}

// subqueryOutputAffinities is subqueryOutputAffinity for EVERY result column of
// a (non-compound) subquery, in select-list order -- what a ROW-VALUE membership
// test needs, since "(a,b) IN (SELECT x,y ...)" combines each pair's affinities
// independently ("('5','5') IN (SELECT i,t FROM ta)" over "ta(i INTEGER, t
// TEXT)" holding (5,'5') is TRUE: column 0 coerces numerically because of i,
// column 1 compares as text because of t -- verified against the oracle).
func (p *ReadOnlyPager) subqueryOutputAffinities(stmt *SelectStmt, outer *evalCtx) (affs []affinity, mats []bool, ok bool) {
	defer p.pushSubqueryCTEs(stmt)()
	core, cok := inRHSCore(stmt)
	if !cok {
		return nil, nil, false
	}
	scopes, err := p.subqueryAnalysisScopes(core.From)
	if err != nil {
		return nil, nil, false
	}
	outCols, err := expandSelectList(core.Columns, scopes, defaultColNameMode)
	if err != nil || len(outCols) == 0 {
		return nil, nil, false
	}
	ctx := &evalCtx{tables: scopes, outer: outer}
	affs = make([]affinity, len(outCols))
	mats = make([]bool, len(outCols))
	for i, oc := range outCols {
		affs[i], mats[i] = exprAffinity(ctx, oc.expr), isMaterializedRef(ctx, oc.expr)
	}
	return affs, mats, true
}

// subqueryOutputCollation is subqueryOutputAffinity's collation twin: the
// collation a single-column, non-compound subquery's result column contributes
// to "X [NOT] IN (SELECT y ...)", and whether it is explicit (a COLLATE in the
// select list) or a bare column's declared one -- resolveCompareCollation ranks
// explicit on either side above declared on either side. Over
// tc(x TEXT COLLATE NOCASE, y TEXT) holding ('ABC','abc'):
//
//	'abc' IN (SELECT x FROM tc)                 -> 1  (x's DECLARED NOCASE)
//	x IN (SELECT y COLLATE RTRIM FROM tc)       -> 0  (the subquery's EXPLICIT
//	                                                   RTRIM outranks x's
//	                                                   declared NOCASE)
//	'ABC' IN (SELECT y COLLATE NOCASE FROM tc)  -> 1
//	x COLLATE RTRIM IN (SELECT y FROM tc)       -> 0  (X's explicit wins)
//	x IN (SELECT y FROM tc)                     -> 1  (X's declared wins over
//	                                                   the right's BINARY)
//	'abc' IN (SELECT upper(x) FROM tc)          -> 0  (a computed column
//	                                                   carries no collation)
//	'abc' IN (SELECT x FROM tc UNION ALL ...)   -> 0  (a compound loses it)
//
// An unanalyzable shape yields ok=false, leaving the collation to X alone.
func (p *ReadOnlyPager) subqueryOutputCollation(stmt *SelectStmt, outer *evalCtx) (name string, explicit, ok bool) {
	rhs, cok := p.subqueryOutputCollations(stmt, outer)
	if !cok || len(rhs) != 1 {
		return "", false, false
	}
	ce, is := rhs[0].(collExpr)
	if !is {
		return "", false, false
	}
	return ce.name, ce.explicit, true
}

// subqueryOutputCollations is subqueryOutputCollation for EVERY result column,
// in select-list order, as the RIGHT-hand operand resolveCompareCollation wants:
// element i is a collExpr when that column contributes a collating sequence, and
// nil when it contributes none (nil, not a zero collExpr -- an empty name would
// otherwise be resolved as the literal collation ""). A ROW-VALUE membership
// test resolves each pair independently: "('abc','DEF') IN (SELECT x,y FROM tc)"
// over "tc(x TEXT COLLATE NOCASE, y TEXT)" holding ('ABC','DEF') is TRUE, while
// swapping the subquery's columns makes it FALSE (verified against the oracle).
func (p *ReadOnlyPager) subqueryOutputCollations(stmt *SelectStmt, outer *evalCtx) (rhs []Expr, ok bool) {
	defer p.pushSubqueryCTEs(stmt)()
	core, cok := inRHSCore(stmt)
	if !cok {
		return nil, false
	}
	scopes, err := p.subqueryAnalysisScopes(core.From)
	if err != nil {
		return nil, false
	}
	outCols, err := expandSelectList(core.Columns, scopes, defaultColNameMode)
	if err != nil || len(outCols) == 0 {
		return nil, false
	}
	ctx := &evalCtx{tables: scopes, outer: outer}
	rhs = make([]Expr, len(outCols))
	for i, oc := range outCols {
		switch n, found := exprCollation(oc.expr); {
		case found:
			rhs[i] = collExpr{name: n, explicit: true}
		default:
			if n, found := declaredColumnCollation(ctx, oc.expr); found {
				rhs[i] = collExpr{name: n, explicit: false}
			}
		}
	}
	return rhs, true
}

// collExpr is a synthetic expression node carrying a precomputed collating
// sequence, the collation counterpart of affExpr: it never evaluates, and only
// exprCollation/declaredColumnCollation consult it, so subqueryOutputCollation's
// answer can be fed to resolveCompareCollation as the IN comparison's RIGHT
// operand while keeping its explicit-vs-declared rank.
type collExpr struct {
	name     string
	explicit bool
}

func (collExpr) exprNode() {}

// inSubCollation is the collating sequence "X [NOT] IN (SELECT y ...)" compares
// under: resolveCompareCollation over X and a collExpr standing in for the
// subquery's result column. The single source of truth for compileInSubquery,
// so the collation an IN comparison uses is decided in exactly one place.
func inSubCollation(p *ReadOnlyPager, ctx *evalCtx, x Expr, sub *SelectStmt) string {
	var rhs Expr
	if p != nil {
		if n, explicit, ok := p.subqueryOutputCollation(sub, ctx); ok {
			rhs = collExpr{name: n, explicit: explicit}
		}
	}
	return resolveCompareCollation(ctx, x, rhs)
}

// inSubRowCollations is inSubCollation per column for a ROW-VALUE membership
// test: xs[i] paired with the subquery's i'th result column. A nil rhs slice (an
// unanalyzable subquery) leaves every column to be decided from its own xs[i].
func inSubRowCollations(ctx *evalCtx, xs []Expr, rhs []Expr) []string {
	out := make([]string, len(xs))
	for i := range xs {
		var r Expr
		if i < len(rhs) {
			r = rhs[i]
		}
		out[i] = resolveCompareCollation(ctx, xs[i], r)
	}
	return out
}

// affExpr is a synthetic expression node carrying a precomputed affinity and
// materialization flag. It never evaluates -- only exprAffinity/isMaterialized
// Ref consult it -- and exists to feed a subquery result column's affinity
// into comparisonAffinity (affinity.go) for the
// asymmetric-vs-combined "X IN (SELECT y ...)" affinity rule (subquery.test's
// x-IN-integer-subquery-vs-text-column case).
type affExpr struct {
	aff          affinity
	materialized bool
}

func (affExpr) exprNode() {}
