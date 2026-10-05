// This file implements schema (database) qualifier resolution: the shared
// logic that decides whether a "schema.table" / "schema.table.column"
// reference -- or a schema-qualified INSERT/UPDATE/DELETE target -- resolves
// against THIS engine instance's own single database, or must be declined.
//
// This engine has exactly one database per open file (see writer.go /
// pager.go) and no cross-file join machinery. Its TEMP catalog lives in that
// same file (temp_schema.go), so a "temp." qualifier is LOCAL; only an ATTACHed
// database is foreign. The multi-database (ATTACH) surface lives one layer up, in
// the driver Conn, which ROUTES a statement that references an ATTACHed
// database to that database's own file before it ever reaches the engine, and
// sets the engine instance's localSchema to the routed file's attach name. By
// the time a qualifier is checked here, the only ones that can legitimately
// resolve are:
//
//   - "" (unqualified) -- always local;
//   - the instance's own localSchema (default "main") -- local;
//   - "temp." -- local: this engine holds a real TEMP catalog alongside main
//     (temp_schema.go), so the qualifier SELECTS one of the two rather than
//     naming a database it cannot reach. Which catalog a name then resolves in
//     is decided by the lookup itself (resolveTableIn / findTableMetaIn), not
//     here.
//
// Everything else -- a foreign database name the Conn did not route here -- is
// declined (the caller turns that into a "no such table" style error, matching
// SQLite, which likewise rejects a reference into a database it cannot
// resolve).

package engine

import (
	"fmt"
)

// localSchemaOr returns the effective local schema name, defaulting to "main"
// for the empty (unset) case.
func localSchemaOr(ls string) string {
	if ls == "" {
		return "main"
	}
	return ls
}

// isTempSchemaQualifier reports whether q names the TEMP database (its schema
// name is "temp", with "temp" the only spelling SQLite accepts as a database
// qualifier -- the sqlite_temp_master catalog name is handled separately).
func isTempSchemaQualifier(q string) bool {
	return equalFoldName(q, "temp")
}

// qualifierResolvesLocally decides whether a written schema qualifier q on a
// table or column reference resolves against this pager's own (single) schema.
// See this file's package comment for the full contract. Returns ok=false (not
// an error) for a qualifier that simply does not resolve locally -- the caller
// converts that into the appropriate "no such table" error.
func (p *ReadOnlyPager) qualifierResolvesLocally(q string) (ok bool, err error) {
	if q == "" {
		return true, nil
	}
	local := localSchemaOr(p.localSchema)
	if equalFoldName(q, local) {
		return true, nil
	}
	// "temp." is main's own only while this connection has no TEMP database.
	// Once it has one it is a database of its own -- its own file, its own
	// catalog (temp_store.go) -- and reaches the reader search below like any
	// other, which is where its pager is.
	if equalFoldName(local, "main") && isTempSchemaQualifier(q) {
		return !p.hasTempReader(), nil
	}
	return false, nil
}

// OwnsTempDatabase is DB.OwnsTempDatabase for the read side: a snapshot taken
// from a write session already reaches that session's TEMP database, whose
// reader carries the current statement's uncommitted pages too.
func (p *ReadOnlyPager) OwnsTempDatabase() bool {
	if p == nil {
		return false
	}
	return p.hasTempReader() || (p.writeSession != nil && p.writeSession.OwnsTempDatabase())
}

// hasTempReader reports whether this snapshot carries the TEMP database as a
// reader of its own (SnapshotPager, writer.go).
func (p *ReadOnlyPager) hasTempReader() bool {
	if p == nil {
		return false
	}
	for _, ar := range p.attachedReaders {
		if ar.name == "temp" {
			return true
		}
	}
	return false
}

// SetLocalSchema sets the database-qualifier name this pager answers to (see
// ReadOnlyPager.localSchema). The driver Conn calls it after opening an
// ATTACHed database's file, so a qualifier written in the routed SQL matches.
func (p *ReadOnlyPager) SetLocalSchema(name string) { p.localSchema = name }

// checkWriteSchemaQualifier returns nil if a schema qualifier q on an
// INSERT/UPDATE/DELETE target resolves against this write session's own schema
// (the write-path counterpart of qualifierResolvesLocally), else a declining
// error. Only the exact-success/exact-failure distinction is compat-relevant
// (the harness never compares error text), so the wording here is for humans.
//
// A qualifier naming a database this session ATTACHed (attach.go) is still
// DECLINED -- a write session materializes exactly one file, so it can read an
// attached database but never write into it -- just with its own message, so
// "you asked for something out of scope" reads differently from "that database
// does not exist". Note the asymmetry with the READ path's qualifierResolves
// (cross_db.go), which DOES accept an attached name: reads route to the owning
// pager, writes have nowhere to route to.
func (db *DB) checkWriteSchemaQualifier(q string) error {
	if q == "" {
		return nil
	}
	local := localSchemaOr(db.localSchema)
	if equalFoldName(q, local) || (equalFoldName(local, "main") && isTempSchemaQualifier(q)) {
		return nil
	}
	if db.attachedNamed(q) != nil {
		return fmt.Errorf("engine: cannot write into ATTACHed database %s (this write path materializes a single file; route the statement to that database's own session)", q)
	}
	return fmt.Errorf("engine: unknown database %s", q)
}

// SetLocalSchema sets the database-qualifier name this write session answers to
// (see DB.localSchema).
func (db *DB) SetLocalSchema(name string) { db.localSchema = name }

// validateSelectSchemaQualifiers walks a whole SELECT statement (recursively
// through its FROM items, every expression, subqueries, compound arms, CTEs
// and window specs) and validates EVERY database qualifier it carries -- each
// FromItem.Schema and each three-part ColumnExpr.Schema -- against this pager's
// own schema (qualifierResolvesLocally). It returns the first "no such table"
// decline for a qualifier that does not resolve locally.
//
// This is the single, mode-independent choke point run once at the top of
// execSelect (query.go), BEFORE the row-mode / GROUP-BY / aggregate / window
// dispatch. Doing it here -- rather than in each dispatch path's own
// column-resolution code -- is what makes the guarantee airtight: no matter
// which execution mode runs (and no matter that some of them resolve columns
// through code that carries no pager, e.g. the single-table WHERE push-down or
// GROUP-BY key resolution), an invalid qualifier is already rejected, so those
// paths can safely resolve a validated three-part reference as its plain
// two-part form. See compileColumn's ColumnExpr handling (vdbe_codegen.go).
func (p *ReadOnlyPager) validateSelectSchemaQualifiers(stmt *SelectStmt) error {
	return p.validateSelectSchemaQualifiersIn(stmt, nil)
}

func (p *ReadOnlyPager) validateSelectSchemaQualifiersIn(stmt *SelectStmt, from []FromItem) error {
	if stmt == nil {
		return nil
	}
	// A three-part "schema.table.column" reference is validated against THIS
	// statement's own FROM items (see validateExprSchemaQualifiersIn), so the
	// enclosing scope's list is replaced, not extended, once this SELECT has
	// one of its own.
	if len(stmt.From) > 0 {
		from = stmt.From
	}
	for i := range stmt.From {
		it := &stmt.From[i]
		if it.Schema != "" {
			ok, err := p.qualifierResolves(it.Schema)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("engine: no such table: %s.%s", it.Schema, it.Table)
			}
		}
		if it.Subquery != nil {
			if err := p.validateSelectSchemaQualifiersIn(it.Subquery, from); err != nil {
				return err
			}
		}
		if err := p.validateExprSchemaQualifiersIn(it.On, from); err != nil {
			return err
		}
	}
	for _, c := range stmt.Columns {
		if err := p.validateExprSchemaQualifiersIn(c.Expr, from); err != nil {
			return err
		}
	}
	if err := p.validateExprSchemaQualifiersIn(stmt.Where, from); err != nil {
		return err
	}
	for _, g := range stmt.GroupBy {
		if err := p.validateExprSchemaQualifiersIn(g, from); err != nil {
			return err
		}
	}
	if err := p.validateExprSchemaQualifiersIn(stmt.Having, from); err != nil {
		return err
	}
	for _, ot := range stmt.OrderBy {
		if err := p.validateExprSchemaQualifiersIn(ot.Expr, from); err != nil {
			return err
		}
	}
	for _, w := range stmt.Windows {
		if err := p.validateWindowSchemaQualifiersIn(w.Spec, from); err != nil {
			return err
		}
	}
	for _, arm := range stmt.Compound {
		if err := p.validateSelectSchemaQualifiersIn(arm.Stmt, from); err != nil {
			return err
		}
	}
	for _, cte := range stmt.CTEs {
		if err := p.validateSelectSchemaQualifiersIn(cte.Select, from); err != nil {
			return err
		}
	}
	return nil
}

func (p *ReadOnlyPager) validateWindowSchemaQualifiersIn(spec *WindowSpec, from []FromItem) error {
	if spec == nil {
		return nil
	}
	for _, e := range spec.PartitionBy {
		if err := p.validateExprSchemaQualifiersIn(e, from); err != nil {
			return err
		}
	}
	for _, ot := range spec.OrderBy {
		if err := p.validateExprSchemaQualifiersIn(ot.Expr, from); err != nil {
			return err
		}
	}
	return nil
}

// validateExprSchemaQualifiersIn walks one expression tree, validating every
// three-part ColumnExpr.Schema it carries and descending into any embedded
// subquery (which is validated as its own SELECT). Every Expr type that can
// hold a sub-expression or a sub-SELECT is enumerated so a qualifier can never
// hide from validation in an un-walked corner (a missed INVALID qualifier would
// otherwise be silently accepted as its two-part form -- a wrong answer).
func (p *ReadOnlyPager) validateExprSchemaQualifiersIn(e Expr, from []FromItem) error {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return nil
	case ColumnExpr:
		if x.Schema == "" {
			return nil
		}
		ok, err := p.qualifierResolves(x.Schema)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("engine: no such column: %s.%s.%s", x.Schema, x.Qualifier, x.Name)
		}
		// The written catalog must be the one the FROM item providing this
		// name actually lives in: "SELECT temp.t.a FROM t" is an error while
		// only a main t exists, and resolves to the temp t once one does
		// (verified directly). A qualifier naming an ALIAS is checked the same
		// way against the aliased item, which C SQLite also accepts
		// ("SELECT main.al.a FROM t AS al" works).
		if bad, is := p.columnQualifierCatalogMismatch(x, from); is {
			return bad
		}
		return nil
	case UnaryExpr:
		return p.validateExprSchemaQualifiersIn(x.X, from)
	case BinaryExpr:
		return firstErr(p.validateExprSchemaQualifiersIn(x.L, from), p.validateExprSchemaQualifiersIn(x.R, from))
	case IsNullExpr:
		return p.validateExprSchemaQualifiersIn(x.X, from)
	case InExpr:
		if err := p.validateExprSchemaQualifiersIn(x.X, from); err != nil {
			return err
		}
		for _, it := range x.List {
			if err := p.validateExprSchemaQualifiersIn(it, from); err != nil {
				return err
			}
		}
		return p.validateSelectSchemaQualifiersIn(x.Sub, from)
	case BetweenExpr:
		return firstErr(p.validateExprSchemaQualifiersIn(x.X, from), p.validateExprSchemaQualifiersIn(x.Lo, from), p.validateExprSchemaQualifiersIn(x.Hi, from))
	case LikeExpr:
		return firstErr(p.validateExprSchemaQualifiersIn(x.X, from), p.validateExprSchemaQualifiersIn(x.Pattern, from), p.validateExprSchemaQualifiersIn(x.Escape, from))
	case GlobExpr:
		return firstErr(p.validateExprSchemaQualifiersIn(x.X, from), p.validateExprSchemaQualifiersIn(x.Pattern, from))
	case CollateExpr:
		return p.validateExprSchemaQualifiersIn(x.X, from)
	case CastExpr:
		return p.validateExprSchemaQualifiersIn(x.X, from)
	case FuncExpr:
		for _, a := range x.Args {
			if err := p.validateExprSchemaQualifiersIn(a, from); err != nil {
				return err
			}
		}
		if err := p.validateExprSchemaQualifiersIn(x.Filter, from); err != nil {
			return err
		}
		for _, ob := range x.orderByExprs() {
			if err := p.validateExprSchemaQualifiersIn(ob, from); err != nil {
				return err
			}
		}
		return p.validateWindowSchemaQualifiersIn(x.Over, from)
	case RowExpr:
		for _, el := range x.Elems {
			if err := p.validateExprSchemaQualifiersIn(el, from); err != nil {
				return err
			}
		}
		return nil
	case CaseExpr:
		if err := p.validateExprSchemaQualifiersIn(x.Base, from); err != nil {
			return err
		}
		for _, w := range x.Whens {
			if err := firstErr(p.validateExprSchemaQualifiersIn(w.When, from), p.validateExprSchemaQualifiersIn(w.Then, from)); err != nil {
				return err
			}
		}
		return p.validateExprSchemaQualifiersIn(x.Else, from)
	case SubqueryExpr:
		return p.validateSelectSchemaQualifiersIn(x.Stmt, from)
	case ExistsExpr:
		return p.validateSelectSchemaQualifiersIn(x.Stmt, from)
	case RaiseExpr:
		return p.validateExprSchemaQualifiersIn(x.Msg, from)
	default:
		// An Expr type with no sub-expression and no schema qualifier of its
		// own carries nothing to validate.
		return nil
	}
}

// columnQualifierCatalogMismatch checks a three-part "schema.qualifier.column"
// reference against the FROM items in scope: the FROM item that provides
// qualifier (by table name or alias) must live in the catalog the reference
// names. Returns is=false whenever no item matches the qualifier at all --
// resolution is left to the ordinary column-resolution path, which reports its
// own error.
func (p *ReadOnlyPager) columnQualifierCatalogMismatch(x ColumnExpr, from []FromItem) (error, bool) {
	scope, ok := scopeOfQualifier(x.Schema)
	if !ok || scope == scopeAny {
		return nil, false
	}
	notFound := fmt.Errorf("engine: no such column: %s.%s.%s", x.Schema, x.Qualifier, x.Name)
	for _, it := range from {
		name := it.Alias
		if name == "" {
			name = it.Table
		}
		if !equalFoldName(name, x.Qualifier) {
			continue
		}
		if it.Schema != "" {
			// Both written: they must agree.
			if itScope, itOK := scopeOfQualifier(it.Schema); !itOK || itScope != scope {
				return notFound, true
			}
			return nil, false
		}
		if it.Subquery != nil || it.Table == "" {
			return nil, false // a derived table belongs to no catalog
		}
		// Unqualified item: it lives wherever its own name resolves. A TEMP
		// qualifier is asked of the TEMP database, which is where temp objects
		// are (temp_store.go) -- this pager's own catalog holds none.
		rp := p
		if scope == scopeTemp {
			if owner, found := p.attachedReaderNamed("temp"); found {
				rp = owner
			}
		}
		if _, err := rp.resolveTableIn(scope, it.Table); err != nil {
			// A VIEW is just as legitimate a source as a table.
			if _, found, verr := rp.resolveViewByNameIn(scope, it.Table); verr == nil && found {
				return nil, false
			}
			return notFound, true
		}
		return nil, false
	}
	return nil, false
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// targetSchemaName is the name of the schema a write TARGET lives in: the temp
// catalog for a TEMP table (see tableMeta.isTemp, temp_schema.go), otherwise
// this session's own database under whatever name localSchema gives it. It is
// what compileColumn (vdbe_codegen.go) compares a three-part reference's
// database qualifier against when it has no pager.
func (db *DB) targetSchemaName(tbl *tableMeta) string {
	if tbl != nil && tbl.isTemp {
		return "temp"
	}
	return localSchemaOr(db.localSchema)
}

// ownSchemaQualifier reports whether the database qualifier q names the schema
// `own` -- the schema a write compile's single scope (its target table) lives
// in. It exists because compileColumn must decide a three-part
// "schema.table.column" reference with no pager to resolve against, and the
// decision needs only a NAME.
//
// It is the two halves of lookupName's qualifier handling, run together. First
// resolve.c:319-330 turns zDb into a pSchema pointer by matching it against
// db->aDb[i].zDbSName, with resolve.c:330 still accepting "main" when the main
// database has been renamed via SQLITE_DBCONFIG_MAINDBNAME -- which is why the
// non-temp arm is an OR with "main" rather than an equality. Then, crucially,
// the SrcList walk only counts a table whose OWN schema is that pointer
// ("if( pTab->pSchema!=pSchema ) continue;", resolve.c:421), so a qualifier
// that names a REAL but DIFFERENT schema resolves to nothing and the reference
// is a plain "no such column".
//
// That second half is not a detail. Verified against the 3.53.3 oracle over
// main.t(a,b) holding (1,2): "UPDATE t SET a=9 WHERE temp.t.b=2" and
// "UPDATE t SET a=temp.t.b+1" both ERROR there. Accepting temp here merely
// because the session HAS a temp catalog let both statements run and rewrite
// the row -- a wrong answer, in a write. Hence the comparison is against the
// TARGET's schema, not against the set of schemas that exist.
//
// Deliberately narrower than the read path's qualifierResolves (cross_db.go),
// which also accepts an ATTACHed name: a write session cannot route there at
// all (checkWriteSchemaQualifier above), so an attached qualifier keeps
// compileColumn's decline rather than becoming an error here.
func ownSchemaQualifier(own, q string) bool {
	if q == "" {
		return true
	}
	if isTempSchemaQualifier(own) {
		return isTempSchemaQualifier(q)
	}
	return equalFoldName(q, own) || equalFoldName(q, "main")
}
