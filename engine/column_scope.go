// Column name resolution: the scopes a reference is resolved against, and the
// lookup itself.
//
// tableScope is one joined table's namespace within an evalCtx -- its scope
// name, its column metadata, and where its columns begin in the concatenated
// value slice -- and resolveColumnEx is the lookup that walks them, applying
// SQLite's own rules: a qualified name selects its table first, an unqualified
// one takes the left-most match, and a USING/NATURAL duplicate is skipped
// rather than counted as an ambiguity (resolve.c:438-449).
//
// These are the COMPILER's, not an evaluator's: nothing here walks a tree to
// produce a value.
package engine

import (
	"fmt"
	"math"
)

// tableScope is one joined table's column namespace within a single evalCtx:
// its scope name (alias, or its own table name if unaliased), column
// metadata/index, and where its columns begin in the evalCtx's (possibly
// multi-table, concatenated) vals slice. See join.go's resolveFrom/
// buildScopes, which build one of these per FROM item.
type tableScope struct {
	name string
	// planOuter, set only on the ported planner's own copy of a statement's
	// scopes (and only on its first), is what a name the FROM clause does not
	// supply resolves against there. See wherePlanBoundOuter.
	planOuter *evalCtx
	// planCaseSensitiveLike and planUTF16LE ride with planOuter: "PRAGMA
	// case_sensitive_like" and a UTF-16LE text encoding, which decide the LIKE
	// optimization (whereLikeRange).
	planCaseSensitiveLike, planUTF16LE bool
	// planPager is the compile's pager, which resolving the names inside a
	// subquery term needs (wherePlanSubqueryUsage).
	planPager *ReadOnlyPager
	// fromExists marks the FROM item existsToJoin made of a WHERE EXISTS, which
	// only the planner ever sees (where_plan_exists.go), and existsColUsed is
	// what its subquery's result list and ORDER BY credit its colUsed with.
	fromExists    bool
	existsColUsed uint64
	// tableName is the underlying object's real name (not the alias), used only to
	// build the "<table>.<column>" result name under "PRAGMA full_column_names=ON"
	// (expandSelectList, columnRefNameParts): "SELECT a.f1 FROM test1 a" is named
	// "test1.f1", while "SELECT * FROM test1 a" with full_column_names=ON and
	// short_column_names=OFF uses the alias, "a.f1" (from name). A derived table
	// uses its alias; an unaliased one leaves "" and full_column_names naming
	// declines rather than reproduce "subquery-N". "" for scopes built outside the
	// SELECT FROM path.
	tableName string
	// groupAlias is the alias written on the PARENTHESIZED JOIN GROUP this
	// FROM item belongs to, if any -- FromItem.GroupAlias carried onto the
	// scope. Unlike name it is shared by every member of the group, so it is
	// consulted only AFTER no scope's own name matched a qualifier: it can
	// turn a "no such column" into an answer, never redirect a reference that
	// already resolved. See FromItem.GroupAlias (sql_ast.go) for the oracle
	// evidence and for the two shapes that never get one.
	groupAlias string
	cols       []columnInfo
	colIndex   map[string]int // lower-cased column name -> index into cols, and (offset+that) into vals
	offset     int

	// dbIdx is WHICH database this FROM item's rows come from, in dbIndexOf's
	// encoding (cross_db.go): 0 for the pager compiling the statement, i+1 for
	// its attachedReaders[i]. It is joinSource.dbIdx carried down onto the
	// scope, and the only expression compiler that needs it is the fts3/fts4
	// MATCH resolution -- an fts table's shadow tables live in ITS OWN
	// database, so "SELECT rowid FROM two.t3 WHERE t3 MATCH 'hello'"
	// (fts3aj.test) must load two.t3's index and not main's same-named one,
	// which answered 0 rows where C SQLite answers 1. Resolve it back to a
	// pager with ReadOnlyPager.forDB, never by closing over a pointer: a
	// compiled Program is re-run under other pagers (see dbIndexOf).
	dbIdx int

	// noRowid is true for a DERIVED TABLE's scope (a subquery in FROM -- see
	// FromItem.Subquery, sql_ast.go): a derived table exposes NO implicit
	// rowid/oid/_rowid_ pseudo-column, unlike an ordinary rowid table. A
	// reference to one of those names against a derived scope must therefore
	// resolve only if the derived table has a REAL output column of that name
	// (an ordinary colIndex hit), never fall through to the pseudo-rowid --
	// exactly matching C SQLite, which rejects "SELECT rowid FROM (SELECT
	// 1)" with "no such column: rowid". resolveColumn (below) and resolveInScopes
	// (vdbe_codegen.go) both consult this before ever treating a name as a
	// pseudo-rowid on this scope. false for every ordinary table scope.
	noRowid bool

	// coalesced maps a lower-cased column name that is a USING/NATURAL right-hand
	// duplicate on this table to the FROM-item index of its representative
	// (desugarJoinItem); nil otherwise. An unqualified reference skips this table
	// for that name, so the representative resolves it and the pair is never
	// "ambiguous" (resolveColumnEx, resolveInScopes). expandSelectList skips it in
	// a bare "*" ("t.*" shows every column). A qualified reference ("t2.a") always
	// reads this table's own column.
	coalesced map[string]int

	// coalesceFallback maps a lower-cased name that is this table's
	// (representative) copy of a USING/NATURAL common column to the FROM-item
	// indices, in FROM order, of every later RIGHT/FULL JOIN target carrying the
	// same column (installCoalesceFallback). Only a RIGHT/FULL join can make the
	// representative's copy NULL while a later copy is real. More than one entry
	// arises when several RIGHT/FULL items coalesce onto the same representative
	// ("t1 RIGHT JOIN t2 USING(a) RIGHT JOIN t3 USING(a)").
	//
	// An unqualified or "*" reference, or a qualified one naming this
	// representative, reads COALESCE(own, fallback[0], fallback[1], ...)
	// (resolveCoalesceChain; resolveInScopes mirrors it). That is exact: on any
	// row at most one source is genuine and the rest are NULL or equal to it
	// (join2.test; joinB.test 21-23). The qualified case matters because
	// desugarJoinItem builds every USING/NATURAL condition as a qualified
	// reference to the representative, so a later join's condition must see the
	// coalesced value ("t1 FULL JOIN t2 USING(a) LEFT JOIN t3 USING(a)"). nil for
	// other join shapes.
	coalesceFallback map[string][]int

	// unqualifiedHidden marks a scope reachable only through an explicit
	// qualifier: the UPSERT "excluded" scope, and a trigger's OLD/NEW. C's grammar
	// makes "excluded" reachable only as "excluded.col"; a bare "col" resolves
	// against the target table. Without this, "SET n = n + excluded.n" would be
	// "ambiguous column name".
	unqualifiedHidden bool

	// nestedNonLeading mirrors FromItem.NestedNonLeading (sql_ast.go) -- see
	// that field's own doc comment for exactly what it means (a table
	// reached through a parenthesized join subtree written anywhere but the
	// FROM clause's own leading position) and why it exists (a real-SQLite
	// rowid/oid/_rowid_ pseudo-column DISPLAY-NAME quirk this package
	// declines rather than mis-reproduce). Consulted only by query.go's
	// columnRefNameParts; every other use of a scope (row-value
	// evaluation, WHERE/ORDER BY/GROUP BY, an ordinary declared-column
	// reference) is entirely unaffected. false for every scope built outside
	// resolveFrom/resolveJoinSources (join.go/vdbe_join_codegen.go), which
	// are the only two places that ever set it from a real FromItem.
	nestedNonLeading bool

	// nestedRowidDisplayName, when non-empty, is the literal name ("_ROWID_",
	// "ROWID" or "OID"; sqlite3RowidAlias, expr.c:3042) C gives a qualified rowid
	// reference through this scope when nestedNonLeading is set. Written only by
	// annotateNestedRowidNames/rowidAliasCandidate (vdbe_join_codegen.go), for a
	// single (NestFromWrapDepth==1) unaliased join group whose flat member order
	// matches SQLite's rebuilt list. "" means decline (columnRefNameParts): deeper
	// nesting, an aliased group, or a name collision C would ":N"-suffix.
	nestedRowidDisplayName string

	// nestedColNames, parallel to cols, is each column's name in an ALIASED
	// parenthesized join group's rebuilt column list, where duplicates carry
	// SQLite's ":N" suffix (annotateNestedColNames, vdbe_join_codegen.go). A
	// reference qualified by this member ("f1.b" in "(f1 JOIN f2 USING(b)) AS
	// gq") is named by it; nil leaves every name as declared.
	nestedColNames []string

	// nestedStarNames, also parallel to cols, is what a bare "*" calls each of
	// those columns -- which is NOT always the same list. selectExpander emits
	// one SYNTHETIC entry per USING/NATURAL common column ahead of the members'
	// own columns (select.c:6194-6212), and that entry takes the UNSUFFIXED
	// name while both members' own copies get ":N"; "*" then shows the
	// synthetic one and hides the copies (COLFLAG_NOEXPAND). So over
	// "(t3 LEFT JOIN t4 USING(a))" a bare "*" says a, b, b:1 while "t3.a" says
	// "a:1". nil, like nestedColNames, leaves every name as declared.
	nestedStarNames []string

	// isFts5 marks a scope backed by an fts5 virtual table. It lets evalMatch
	// (fts5_match.go) recognize the fts5 target on the WRITE path (deleteVtab/
	// updateVtab, vtab_write.go), whose evalCtx carries no ReadOnlyPager to look
	// the table up in; the READ path leaves it false and detects fts5 via the
	// pager instead. false for every ordinary table scope.
	isFts5 bool

	// fts5Tok is that same write-path scope's TOKENIZER (nil = fts5's default
	// unicode61, which is what an ordinary scope's zero value means too). Only
	// read when isFts5 is set; the read path resolves the tokenizer from the
	// snapshot's schema instead (fts5SchemaTok, fts5_match.go).
	fts5Tok *fts5Tokenizer

	// fts5Detail is that same scope's detail= MODE (fts5_detail.go), carried
	// for the same reason and read the same way: fts5DetailFull -- the zero
	// value, and fts5's own default -- for an ordinary scope. It decides which
	// MATCH queries C fts5 refuses over this table, so a write-path DELETE/
	// UPDATE whose WHERE holds a MATCH needs it as much as a SELECT does.
	fts5Detail fts5Detail
}

// evalCtx is the per-row evaluation context: the joined tables' column
// metadata (shared across rows -- see tables) and the current row's decoded,
// concatenated values (with each table's own INTEGER PRIMARY KEY
// rowid-alias substitution already applied; see normalizeRow).
type evalCtx struct {
	// checkConstraintTable, when set, names the table a CHECK constraint is
	// being validated FOR. Inside a CHECK, C SQLite ignores the SCHEMA part
	// of a three-part reference entirely: "CHECK( main.t810.a>0 )" and even
	// "CHECK( xyzzy.t811.b BETWEEN 5 AND 10 )" -- naming a database that does
	// not exist -- both create AND enforce (verified against 3.53.3), which is
	// looser than the rule anywhere else (qualifierResolves). validateColumnRefs
	// skips its schema check for a reference qualified by this table's own name.
	checkConstraintTable string

	tables []tableScope // one entry per FROM item in this scope; nil for a FROM-less scope
	vals   []Value      // concatenated across tables per each one's offset; nil where no row context exists

	// rowids holds, parallel to tables, each table's current rowid pseudo-column
	// value (isRowidAliasName). A LEFT JOIN's NULL-extended table has a NULL entry.
	// nil wherever no row context exists, or for a schema-only probe ctx
	// (resolveColumnIndex, exprAffinity).
	rowids []Value

	// outer is the enclosing query's evalCtx when this ctx belongs to a
	// subquery, or nil for a top-level query. ColumnExpr resolution
	// (resolveColumn) walks this chain outward when a name/qualifier isn't
	// found in the current scope, which is what makes a correlated
	// subquery -- one referencing the enclosing row's columns -- work.
	outer *evalCtx

	// pager is the snapshot subqueries run against. It is the same *ReadOnlyPager for every ctx
	// in a given Query() call, at every scope-chain depth.
	pager *ReadOnlyPager

	// caseSensitiveLike is "PRAGMA case_sensitive_like" for a context with no
	// pager to read it from. See likeCaseSensitive (like_case.go).
	caseSensitiveLike bool

	// db is the WRITE session this ctx is evaluating for, and exists for one
	// reason: a connection-state function (conn_state.go's changes(),
	// total_changes(), last_insert_rowid()) must read the LIVE counters, and
	// pager alone cannot serve them -- a write-path pager is a snapshot frozen
	// at statement start, which inside a trigger body is already stale. nil on
	// every read path, where pager answers instead. A write-path ctx that
	// forgets to set it makes those three DECLINE ("unsupported function
	// changes()"), never answer wrong.
	db *DB

	// groupKey, groupAggVals, hoistedAggs, groupBareVals/groupBareRowids and
	// groupKeyVals are remnants of the deleted row-at-a-time aggregate
	// evaluator: nothing populates them, and the remaining reads (row_scope.go,
	// vdbe_codegen.go) are inert. Aggregate results, group keys and the
	// bare-column anchor row now live in item-program registers
	// (compileAggItemProgram, OpOuterAggReg).
	groupKey     []Value
	groupAggVals []Value
	hoistedAggs  []hoistedAggVal

	groupBareVals   []Value
	groupBareRowids []Value
	groupKeyVals    map[int]Value

	// windowVals[i] is the i'th window function's result for the current row
	// (set by vdbe_window.go). nil otherwise.
	windowVals []Value

	// fts5Aux is the per-query fts5 auxiliary context (the MATCH query's
	// phrases and corpus bm25 statistics, buildFts5AuxState) for a query using
	// "rank" or bm25/snippet/highlight under a MATCH; nil otherwise, where an
	// fts5 aux reference declines.
	fts5Aux *fts5AuxState

	// params holds the whole statement's positional bound-parameter values
	// (QueryArgs/ExecArgs' args, or nil for an unparameterized Query/Exec):
	// params[i] is the value bound to parameter index i+1 (see ParamExpr,
	// sql_ast.go). It is the SAME slice for every evalCtx built anywhere
	// during one top-level statement's execution, including every nested
	// scope (a subquery's own ctx, a per-group/per-aggregate finalize ctx,
	// join.go's per-row ctx, ...) -- exactly SQLite's own rule that a
	// subquery shares its enclosing statement's single bind array, never a
	// separate one of its own.
	params []Value
}

// isRowidAliasName reports whether name (case-insensitively) is one of the
// three names SQLite reserves, on every ordinary (rowid) table, as an alias
// for that table's 64-bit rowid: "rowid", "oid", "_rowid_". This is a pure
// name check with no table context -- resolveColumn only treats it as
// meaningful for a given table once that table has no REAL column of the
// same name (see resolveColumn's doc comment: a real column always shadows
// the pseudo-column of the same name, checked first).
func isRowidAliasName(name string) bool {
	switch r33sFoldIdent(name) {
	case "rowid", "oid", "_rowid_":
		return true
	default:
		return false
	}
}

// rowidColumnInfo is the shared, immutable columnInfo describing the
// rowid/oid/_rowid_ pseudo-column resolveColumn hands back (as col) whenever
// a reference resolves to it rather than a real column: INTEGER affinity
// (exprAffinity needs this for correct comparison-affinity coercion, e.g.
// "WHERE rowid = '5'"), and IsRowidAlias set for the same reason an INTEGER
// PRIMARY KEY column's own columnInfo has it (both alias the same 64-bit
// value). It's never entered into any tableScope.cols/colIndex -- it exists
// purely as resolveColumn's return value -- so it never appears in "*"
// expansion or any other real-column enumeration.
var rowidColumnInfo = &columnInfo{Name: "rowid", Aff: affInteger, IsRowidAlias: true}

// resolveColumn finds the scope and (offset-adjusted) vals index a
// ColumnExpr(qualifier, name) resolves to, walking ctx, ctx.outer, ...
// outward. Unqualified: within one scope it must match exactly one table (more
// is "ambiguous column name"), and only a scope with no match falls through to
// the outer one. Qualified: matched case-insensitively against table names;
// once a table matches, the column must exist there (no fall-through, no
// ambiguity check). col is the column's metadata.
//
// rowidTableIdx is -1 for an ordinary column, or, when name resolves to a
// table's rowid pseudo-column, that table's index into foundCtx.tables and
// foundCtx.rowids (callers read foundCtx.rowids[rowidTableIdx]; idx is 0).
// A real column named rowid/oid/_rowid_ wins over the pseudo-column, as in C;
// unqualified, each table contributes one candidate for the name, so a bare
// "rowid" over two tables is ambiguous.
func resolveColumn(ctx *evalCtx, qualifier, name string) (foundCtx *evalCtx, idx int, col *columnInfo, rowidTableIdx int, err error) {
	return resolveColumnEx(ctx, ColumnExpr{Qualifier: qualifier, Name: name})
}

// resolveColumnEx is resolveColumn taking the whole ColumnExpr, so it sees the
// join-desugaring fields (ColumnExpr.UsingRepr/UsingReprOwnItem/UsingPinned):
//
//   - UsingRepr: a desugarJoinItem USING/NATURAL condition against its
//     representative, which gets the RIGHT/FULL coalesce fallback an
//     unqualified reference gets, but only from items strictly before
//     UsingReprOwnItem (otherwise the condition would compare against itself).
//   - UsingPinned: bind to one FROM item by index, the only way to address an
//     unaliased derived table.
//
// resolveColumn passes neither.
func resolveColumnEx(ctx *evalCtx, ce ColumnExpr) (foundCtx *evalCtx, idx int, col *columnInfo, rowidTableIdx int, err error) {
	qualifier, name := ce.Qualifier, ce.Name
	usingRepr, usingReprOwnItem := ce.UsingRepr, ce.UsingReprOwnItem
	lname := r33sFoldIdent(name)
	// A PINNED reference (ColumnExpr.UsingPinned): bound by FROM-item index in
	// THIS scope level only -- a desugared join condition never reaches an
	// outer query's FROM -- and otherwise treated exactly like the qualified
	// branch below, coalesce fallback included. Mirrors resolveInScopes'
	// identical branch (vdbe_codegen.go) so the two engines can never disagree
	// on what a USING/NATURAL condition over an unnamed derived table means.
	if ce.UsingPinned {
		if ctx == nil || ce.UsingPinnedItem < 0 || ce.UsingPinnedItem >= len(ctx.tables) {
			return nil, 0, nil, -1, fmt.Errorf("engine: internal: USING/NATURAL join condition pinned to FROM item %d, out of range", ce.UsingPinnedItem)
		}
		ts := ctx.tables[ce.UsingPinnedItem]
		colIdx, ok := ts.colIndex[lname]
		if !ok {
			return nil, 0, nil, -1, fmt.Errorf("engine: no such column: %s", name)
		}
		foundOffset, foundIdx := ts.offset, colIdx
		foundCol := &ts.cols[colIdx]
		if usingRepr && ts.coalesceFallback != nil {
			foundOffset, foundIdx, foundCol = resolveCoalesceChain(ctx.vals, ctx.tables, lname, foundOffset, foundIdx, foundCol, ts.coalesceFallback[lname], usingReprOwnItem, false)
		}
		return ctx, foundOffset + foundIdx, foundCol, -1, nil
	}
	for c := ctx; c != nil; c = c.outer {
		if qualifier != "" {
			for i, ts := range c.tables {
				if !equalFoldName(ts.name, qualifier) {
					continue
				}
				if colIdx, ok := ts.colIndex[lname]; ok {
					// An UNALIASED SELF-JOIN exposing this column through BOTH
					// copies: lookupName counts every FROM item whose name
					// matches the qualifier, and a second one carrying the
					// column -- unless its own USING list names it -- lets cnt
					// pass 1, which is "ambiguous column name"
					// (resolve.c:436-447, :785). "SELECT t.b FROM t JOIN t
					// USING (a) GROUP BY a" is that error in C SQLite; taking
					// the first match here answered it with the left copy's b.
					// The VDBE resolver has the same rule
					// (qualifiedScopeAmbiguous, vdbe_codegen.go); this is the
					// resolver the GROUP BY planner reads through.
					if sameNamedScopeAlsoExposes(c.tables, &c.tables[i], name) {
						return nil, 0, nil, -1, fmt.Errorf("engine: ambiguous column name: %s.%s", qualifier, name)
					}
					foundOffset, foundIdx := ts.offset, colIdx
					foundCol := &ts.cols[colIdx]
					// The coalesce fallback applies to this qualified read only for a
					// desugared representative reference (usingRepr) whose own join item
					// is strictly later than the installing item, so a later join's
					// condition sees the coalesced value. A user-written "t1.a" reads the
					// raw value, and a RIGHT/FULL join's own condition must not use its
					// own fallback (ColumnExpr.UsingReprOwnItem).
					if usingRepr && ts.coalesceFallback != nil {
						foundOffset, foundIdx, foundCol = resolveCoalesceChain(c.vals, c.tables, lname, foundOffset, foundIdx, foundCol, ts.coalesceFallback[lname], usingReprOwnItem, false)
					}
					return c, foundOffset + foundIdx, foundCol, -1, nil
				}
				if isRowidAliasName(lname) && !ts.noRowid {
					// Two FROM items share this qualifier (an unaliased self-join, or
					// one alias written twice), both offering a rowid that coalescing
					// never hides: "SELECT t1.rowid FROM t1 NATURAL JOIN t1" is
					// "ambiguous column name: t1.rowid" in C (while "SELECT t1.b ..."
					// answers, b being coalesced away on the second copy).
					for _, other := range c.tables[i+1:] {
						if equalFoldName(other.name, qualifier) && !other.noRowid {
							return nil, 0, nil, -1, fmt.Errorf("engine: ambiguous column name: %s.%s", qualifier, name)
						}
					}
					return c, 0, rowidColumnInfo, i, nil
				}
				return nil, 0, nil, -1, fmt.Errorf("engine: no such column: %s.%s", qualifier, name)
			}
			// No FROM item is named the qualifier; it may be a join group's alias
			// (tableScope.groupAlias), naming every member. Reached only after the
			// name loop failed, so it can only turn "no such column" into an
			// answer. A group-alias read applies the group's coalesce fallback,
			// as C resolves it against the group's rebuilt column list, whose
			// entry for a coalesced name is the coalesce() (see resolveInScopes).
			for _, ts := range c.tables {
				if !equalFoldName(ts.groupAlias, qualifier) {
					continue
				}
				if colIdx, ok := ts.colIndex[lname]; ok {
					foundOffset, foundIdx := ts.offset, colIdx
					foundCol := &ts.cols[colIdx]
					if ts.coalesceFallback != nil {
						foundOffset, foundIdx, foundCol = resolveCoalesceChain(c.vals, c.tables, lname, foundOffset, foundIdx, foundCol, ts.coalesceFallback[lname], math.MaxInt, true)
					}
					return c, foundOffset + foundIdx, foundCol, -1, nil
				}
			}
			continue
		}
		foundIdx, foundOffset := -1, -1
		var foundCol *columnInfo
		var foundFallback map[string][]int
		foundRowidTable := -1
		for i, ts := range c.tables {
			if ts.unqualifiedHidden {
				continue
			}
			if colIdx, ok := ts.colIndex[lname]; ok {
				if ts.coalesced != nil {
					if _, hidden := ts.coalesced[lname]; hidden {
						// A USING/NATURAL right-hand duplicate: this table's
						// own copy doesn't count as a (candidate-for-
						// ambiguity) match at all -- the representative
						// table's own entry (found elsewhere in this same
						// loop) is what resolves the name instead.
						continue
					}
				}
				if foundIdx != -1 || foundRowidTable != -1 {
					return nil, 0, nil, -1, fmt.Errorf("engine: ambiguous column name: %s", name)
				}
				foundIdx, foundOffset = colIdx, ts.offset
				foundCol = &ts.cols[colIdx]
				foundFallback = ts.coalesceFallback
				continue
			}
			if isRowidAliasName(lname) && !ts.noRowid {
				if foundIdx != -1 || foundRowidTable != -1 {
					return nil, 0, nil, -1, fmt.Errorf("engine: ambiguous column name: %s", name)
				}
				foundRowidTable = i
			}
		}
		if foundRowidTable != -1 {
			return c, 0, rowidColumnInfo, foundRowidTable, nil
		}
		if foundIdx != -1 {
			// RIGHT/FULL JOIN coalesce fallback (tableScope.coalesceFallback's
			// doc comment): if this (representative) table's own value is
			// NULL for the CURRENT row, read the first (in FROM order) of its
			// fallback owners whose own value isn't -- exactly COALESCE
			// (representative, fallback[0], fallback[1], ...), computed fresh
			// per row (never a static, compile-time-only choice).
			if foundFallback != nil {
				foundOffset, foundIdx, foundCol = resolveCoalesceChain(c.vals, c.tables, lname, foundOffset, foundIdx, foundCol, foundFallback[lname], math.MaxInt, true)
			}
			return c, foundOffset + foundIdx, foundCol, -1, nil
		}
	}
	if qualifier != "" {
		// resolve.c:785-796 -- an unknown QUALIFIER is cnt==0 exactly like an
		// unknown column, so C reports "no such column: <qualifier>.<name>".
		return nil, 0, nil, -1, fmt.Errorf("engine: no such column: %s.%s", qualifier, name)
	}
	return nil, 0, nil, -1, fmt.Errorf("engine: no such column: %s", name)
}

// unqualifiedColumnNotFoundErr reports whether err is exactly the "no such
// column: <name>" resolveColumnEx raises for an unqualified reference that
// matched nothing in any scope, as opposed to "ambiguous column name" or a
// qualified-reference error. columnRowValue and validateColumnRefs use it to
// apply ColumnExpr.FallbackLiteral (TRUE/FALSE) only then: an ambiguous real
// column named "true" must still report ambiguity.
func unqualifiedColumnNotFoundErr(err error, name string) bool {
	return err != nil && err.Error() == "engine: no such column: "+name
}

// resolveCoalesceChain implements tableScope.coalesceFallback's
// COALESCE(representative, fallback[0], ...) read. fallback is the
// representative's owner list for lname in FROM order; offset/idx/col are the
// representative's own read, returned unchanged if non-NULL or with no
// fallback. Otherwise the first owner strictly before limit whose current
// value is non-NULL wins. limit is math.MaxInt for an unqualified/"*" read and
// the condition's own join-item index for a qualified condition read (so a hop
// never coalesces against itself); the list is in ascending index order, so
// the loop stops at limit. All NULL returns the representative's NULL.
//
// transitive follows nested join groups: fixItemGroupScoping redirects each
// level's representative to its own enclosing group's start, so fallbacks form
// a chain of single hops (t1 -> t2 -> t3); with transitive, a still-NULL owner
// is followed into its own fallback list. Only the final unqualified/"*" read
// passes it, so join conditions (which rows match) are unaffected. Order does
// not matter (every non-NULL source in a row is equal); indices increase along
// links so the walk ends, and visited guards against cycles.
func resolveCoalesceChain(vals []Value, tables []tableScope, lname string, offset, idx int, col *columnInfo, fallback []int, limit int, transitive bool) (int, int, *columnInfo) {
	if fallback == nil || offset+idx >= len(vals) || vals[offset+idx].Typ != Null {
		return offset, idx, col
	}
	var visited map[int]bool
	var walk func(fb []int) (int, int, *columnInfo, bool)
	walk = func(fb []int) (int, int, *columnInfo, bool) {
		for _, ownerIdx := range fb {
			if ownerIdx >= limit {
				break
			}
			if visited[ownerIdx] {
				continue
			}
			owner := tables[ownerIdx]
			ownerColIdx, ok := owner.colIndex[lname]
			if !ok {
				continue
			}
			if vals[owner.offset+ownerColIdx].Typ != Null {
				return owner.offset, ownerColIdx, &owner.cols[ownerColIdx], true
			}
			if transitive && owner.coalesceFallback != nil {
				if visited == nil {
					visited = map[int]bool{}
				}
				visited[ownerIdx] = true
				if o, i2, c2, found := walk(owner.coalesceFallback[lname]); found {
					return o, i2, c2, true
				}
			}
		}
		return 0, 0, nil, false
	}
	if o, i2, c2, found := walk(fallback); found {
		return o, i2, c2
	}
	return offset, idx, col
}

// buildColIndex maps each column name (lower-cased) to its position, keeping
// the first occurrence, as sqlite3ColumnIndex returns on the first match:
//
//	i = 0;
//	while( 1 ){
//	  if( aCol[i].hName==h && sqlite3StrICmp(aCol[i].zCnName, zCol)==0 ) return i;
//	  i++;
//	  if( i>=nCol ) break;
//	}
//
// Only a derived table can repeat a name: "SELECT a FROM (SELECT 1 AS a, 2 AS
// a)" is 1. C also ":N"-renames such duplicates when building the derived
// table (sqlite3ColumnsFromExprList), which this engine does not reproduce;
// errIfDuplicateOutputNames declines the "*" shapes that would show it.
func buildColIndex(cols []columnInfo) map[string]int {
	m := make(map[string]int, len(cols))
	for i, c := range cols {
		ln := r33sFoldIdent(c.Name)
		if _, dup := m[ln]; dup {
			continue
		}
		m[ln] = i
	}
	return m
}

// addFromScopeNames adds each of items' own scope names (its alias, or its
// own table name when unaliased -- a derived table with neither contributes
// nothing, matching the fact that it has no name a qualifier could ever
// target) to names, lower-cased for the same case-insensitive qualifier
// matching resolveColumn itself uses.
func addFromScopeNames(items []FromItem, names map[string]bool) {
	for _, it := range items {
		n := it.Alias
		if n == "" {
			n = it.Table
		}
		if n != "" {
			names[r33sFoldIdent(n)] = true
		}
	}
}

// outerTableScopeNames collects, from outer's scope chain, the names standing
// for a real SrcList table, the only references sqlite3ReferencesSrcList
// counts (exprRefToSrcList reacts to TK_COLUMN/TK_AGG_COLUMN only). A trigger's
// NEW/OLD resolves to TK_TRIGGER, and UPSERT's "excluded" and a RETURNING row to
// TK_REGISTER, so "sum(new.a)" names no table and the aggregate stays in its
// subquery: "CREATE TRIGGER tr AFTER INSERT ON src BEGIN INSERT INTO log
// VALUES((SELECT sum(new.a))); END" logs 3 for new.a = 3. Those are the scopes
// marked unqualifiedHidden, so they are skipped here.
func outerTableScopeNames(outer *evalCtx) map[string]bool {
	names := map[string]bool{}
	for c := outer; c != nil; c = c.outer {
		for _, ts := range c.tables {
			if !ts.unqualifiedHidden && ts.name != "" {
				names[r33sFoldIdent(ts.name)] = true
			}
		}
	}
	return names
}

// resolveColumnScopeName is a best-effort, schema-only lookup of which
// FROM-item scope name ce resolves to, used only by
// riskyCrossTableDeclaredCollationOr to tell same-table from cross-table
// operands. It covers only resolveColumnEx's common case; ok=false makes that
// check skip the pair, which is the safe direction.
func resolveColumnScopeName(ctx *evalCtx, ce ColumnExpr) (string, bool) {
	if ctx == nil {
		return "", false
	}
	lname := r33sFoldIdent(ce.Name)
	for c := ctx; c != nil; c = c.outer {
		for _, ts := range c.tables {
			if ce.Qualifier != "" && !equalFoldName(ts.name, ce.Qualifier) {
				continue
			}
			if _, ok := ts.colIndex[lname]; ok {
				return ts.name, true
			}
		}
	}
	return "", false
}

// scalarSubqueryColumn resolves a scalar subquery's single result column to
// the same columnInfo a derived table's column would carry (its affinity, and
// whether it is computed rather than a materialized column reference). It
// needs a pager to resolve the subquery's FROM, so it reports false whenever
// one is not reachable -- leaving the caller with the previous, affinity-less
// answer rather than a guess.
func scalarSubqueryColumn(ctx *evalCtx, sub *SelectStmt) (columnInfo, bool) {
	if ctx == nil || ctx.pager == nil || sub == nil {
		return columnInfo{}, false
	}
	cols := ctx.pager.derivedColumnInfos(sub, []string{""})
	if len(cols) != 1 {
		return columnInfo{}, false
	}
	return cols[0], true
}

// isMaterializedRef reports whether e is an already-stored value (a table
// column, or a GROUP BY key placeholder for one) rather than a literal or
// computed expression, which decides SQLite's TEXT-affinity coercion: a
// no-affinity column compared with a TEXT column is not converted (t1.y untyped
// 99 vs t2.b TEXT '99' are unequal, whereB.test), while a TEXT column compared
// with a numeric literal converts the literal ("t.x = 1" matches '1'). A
// no-affinity value is coerced toward the other side's TEXT only when it is
// not a materialized reference.
func isMaterializedRef(ctx *evalCtx, e Expr) bool {
	switch x := e.(type) {
	case ColumnExpr:
		// A derived table's (or view's) computed column has AFF_NONE, not
		// AFF_BLOB, and does not defend its storage class
		// (columnInfo.NoAffinity). Over t0(c0 TEXT, c1) holding ('-1', 0):
		//
		//	SELECT c0<c1 FROM t0                             -> 0
		//	SELECT c0<c1 FROM (SELECT c0, c1 FROM t0)        -> 0
		//	SELECT c0<c1 FROM (SELECT c0, AVG(c1) AS c1 ...) -> 1
		//	SELECT c0<c1 FROM (SELECT c0, c1+0 AS c1 ...)    -> 1
		//
		// (view.test 27.*).
		// A bare TRUE/FALSE is a literal, not a column (C rewrites it to
		// TK_TRUEFALSE at resolve time, sqlite3ExprIdToTrueFalse), so it must
		// not defend a storage class: over tt(x TEXT) holding 1,'1',true,
		// "WHERE x = true" is 3. Resolution decides, as row_scope.go's
		// FallbackLiteral arm does, so a real column named "true" is
		// unaffected.
		if ctx != nil {
			_, _, col, _, err := resolveColumnEx(ctx, x)
			if err == nil && col != nil && col.NoAffinity {
				return false
			}
			if err != nil && x.Qualifier == "" && x.FallbackLiteral != nil {
				return false
			}
			return true
		}
		return x.Qualifier != "" || x.FallbackLiteral == nil
	case groupBareColExpr:
		// The same rule the ColumnExpr case above applies, carried on the
		// placeholder because there is no column reference left to resolve:
		// a COMPUTED derived-table column (NoAffinity) does not defend its
		// storage class. See groupBareColExpr.noAff.
		return !x.noAff
	case groupKeyExpr:
		return true
	case affExpr:
		// A subquery result column that is itself a real column reference is
		// materialized (defends its storage class); a computed one is not --
		// carried on the synthetic node (see subquery_validate.go).
		return x.materialized
	case LiteralExpr:
		// False for a literal written in SQL, and the substituted column's own
		// answer for a materialized correlated reference (LiteralExpr's doc
		// comment).
		return x.materialized
	case SubqueryExpr:
		// Same rule for a SCALAR subquery: it defends the storage class of the
		// column it selects. Verified directly over "CREATE TABLE c1(a
		// INTEGER, b TEXT); CREATE TABLE c2(x BLOB, y BLOB)" each holding
		// (1,1) -- so c1.b stores '1' and c2.y stores 1 --
		//
		//	SELECT c1.rowid FROM c1 WHERE b = (SELECT y FROM c2)   -> no rows
		//
		// i.e. the BLOB-affinity column reached through the subquery blocks
		// the TEXT coercion exactly as "b = c2.y" would (rowvalue9.test 3.5).
		if ci, ok := scalarSubqueryColumn(ctx, x.Stmt); ok {
			return !ci.NoAffinity
		}
		return false
	case CollateExpr:
		// An explicit "X COLLATE name" wrapping a materialized column
		// reference does not strip its storage-class defense -- verified
		// directly against C SQLite (mattn/go-sqlite3): "'5' = nonecol
		// COLLATE NOCASE" (a TEXT literal against a NONE-affinity column
		// wrapped in COLLATE) stays UNEQUAL, exactly like the COLLATE-free
		// "'5' = nonecol", i.e. the COLLATE wrapper is transparent to this
		// rule too, mirroring exprAffinity's own CollateExpr pass-through
		// just above (comparisonAffinity asks both of the same operand).
		return isMaterializedRef(ctx, x.X)
	default:
		return false
	}
}
