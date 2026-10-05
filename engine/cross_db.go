// This file implements the engine-side of a genuine CROSS-DATABASE read: a
// single SELECT/JOIN (or a DML statement's source SELECT/subquery) whose FROM
// items live in two or more attached databases -- e.g.
//
//	SELECT * FROM main.a JOIN aux.b ON a.id = b.id
//
// The engine is otherwise strictly one database per open file (see
// pager.go / writer.go). The multi-database surface is assembled one layer up
// in the driver Conn, which -- for a cross-database read -- opens a
// ReadOnlyPager per referenced database and wires the extra ones onto the
// PRIMARY pager (the one execSelect runs on) via SetAttachedReaders below.
//
// From then on the whole existing single-file query pipeline is reused
// verbatim: resolveFrom (join.go) MATERIALIZES each base-table FROM item into
// rows, and the ONLY change cross-database support needs is that each item's
// rows are scanned from the pager that OWNS that table rather than assuming the
// primary. Once every item is materialized, the join / WHERE / aggregate /
// ORDER BY / subquery pipeline operates purely over the combined in-memory
// rows and is entirely pager-agnostic -- a row does not carry which file it
// came from. A nested subquery reenters execSelect on the SAME primary pager,
// so it too routes its own FROM items across the attached set; a correlated
// reference resolves through the outer-row chain, which is likewise
// file-independent. This is what makes a cross-database JOIN byte-exact with
// mattn/go-sqlite3 while touching only the row-source resolution step.
//
// What routes here, and where it resolves:
//   - a schema qualifier that names an attached database (aux.t) -> that
//     database's pager;
//   - a schema qualifier that resolves locally (""/main/temp/localSchema) ->
//     the primary pager, exactly as before;
//   - an UNqualified name -> the primary pager if it owns a CTE / temp-catalog
//     name / table / view / vtab of that name (SQLite's search order puts the
//     primary -- "main" -- first and lets it SHADOW an attached same-named
//     table), else the first attachedReader (in attach order) that has a table
//     or view of that name, else the primary (which then raises the same "no
//     such table" error it would have with no attachment at all).
//
// A foreign-qualified VIEW is resolved on its owning pager too, so its stored
// SELECT runs entirely within that database's own schema (matching SQLite's
// rule that a view's unqualified references bind in the view's schema); a view
// body that itself crosses databases has no attachedReaders on that owning
// pager and so declines cleanly there rather than answering wrong.

package engine

import (
	"fmt"
)

// SetAttachedReaders wires the OTHER attached databases a cross-database read
// may resolve a FROM item against onto this (primary) pager. names[i] is the
// attach schema name of pagers[i]; the two slices are parallel and in attach
// order (which is the search order for an unqualified name after this primary).
// The primary pager is the one execSelect runs on; it represents the database a
// "main."/local/unqualified-here reference resolves against. Passing empty
// slices (or never calling this) leaves the pager in its ordinary single-file
// mode. Each attached pager should already have its own localSchema set to its
// attach name (SetLocalSchema) so a schema-qualified reference resolved on it
// matches.
func (p *ReadOnlyPager) SetAttachedReaders(names []string, pagers []*ReadOnlyPager) {
	dbs := make([]AttachedDatabase, len(names))
	for i := range names {
		dbs[i] = AttachedDatabase{Name: names[i], Pager: pagers[i]}
	}
	p.SetAttachedDatabases(dbs)
}

// AttachedDatabase is one database wired onto a reader: the pager that answers
// for it, plus what "PRAGMA database_list" has to report about it (its file, or
// "" when it has none). Path and InMemory are optional -- a caller that only
// needs routing can leave them zero, and database_list then reports "".
type AttachedDatabase struct {
	Name     string
	Pager    *ReadOnlyPager
	Path     string
	InMemory bool
}

// SetAttachedDatabases is SetAttachedReaders with the per-database metadata
// database_list needs, set in ONE call so a caller cannot wire the readers and
// forget the paths.
func (p *ReadOnlyPager) SetAttachedDatabases(dbs []AttachedDatabase) {
	// THE CATALOG-CORRUPTION VERDICT IS ABOUT EVERY DATABASE A STATEMENT CAN SEE
	// (schemaLoadCatalogs), so changing the set invalidates it. Its other key is
	// MAIN's schema cookie, which attaching a database does not move -- so without
	// this, attaching one whose catalog holds a malformed row after a clean verdict
	// was cached would miss it, and a missed corruption is a wrong answer.
	if p != nil && p.schemaCorrupt != nil {
		p.schemaCorrupt.valid = false
	}
	p.attachedReaders = p.attachedReaders[:0]
	p.originReadersBefore = 0
	for i := range dbs {
		name := r33sFoldIdent(dbs[i].Name)
		p.attachedReaders = append(p.attachedReaders, attachedReader{
			name:     name,
			pager:    dbs[i].Pager,
			path:     dbs[i].Path,
			inMemory: dbs[i].InMemory,
		})
		// The TEMP database is searched BEFORE the primary for an unqualified
		// name -- C SQLite's own order (sqlite3LocateTable walks db->aDb
		// with iDb=1 first, build.c:352-378) -- and originReadersBefore is how
		// that position is spelled here. It is passed first by every caller
		// that has one (driver's wireAttachedReaders, engine
		// SnapshotPager).
		if name == "temp" && len(p.attachedReaders) == 1 {
			p.originReadersBefore = 1
		}
	}
	// TEMP is the one schema whose objects may reference another database (a
	// TEMP view over a main table, a TEMP trigger on one), so the temp pager
	// reaches back: this pager under its own name, plus every attachment.
	for i, ar := range p.attachedReaders {
		if ar.name != "temp" {
			continue
		}
		back := make([]attachedReader, 0, len(p.attachedReaders))
		back = append(back, attachedReader{name: r33sFoldIdent(localSchemaOr(p.localSchema)), pager: p})
		for j, other := range p.attachedReaders {
			if j != i {
				back = append(back, other)
			}
		}
		ar.pager.attachedReaders = back
	}
}

// SetAttachedReaders wires the OTHER attached databases a cross-database DML
// statement's source SELECT / subquery may read, onto this write session (see
// DB.attachedReaders). names[i] is the attach schema name of pagers[i], in
// attach order. The readers are copied onto every ReadOnlyPager the session
// materializes for a read (SnapshotPager), so the write path's own execSelect
// routes a foreign table to the reader that owns it. Passing empty slices
// leaves the session in ordinary single-database mode.
func (db *DB) SetAttachedReaders(names []string, pagers []*ReadOnlyPager) {
	dbs := make([]AttachedDatabase, len(names))
	for i := range names {
		dbs[i] = AttachedDatabase{Name: names[i], Pager: pagers[i]}
	}
	db.SetAttachedDatabases(dbs)
}

// SetAttachedDatabases is the write session's SetAttachedDatabases: the same
// metadata, carried onto every ReadOnlyPager this session materializes.
func (db *DB) SetAttachedDatabases(dbs []AttachedDatabase) {
	db.attachedReaders = db.attachedReaders[:0]
	for i := range dbs {
		db.attachedReaders = append(db.attachedReaders, attachedReader{
			name:     r33sFoldIdent(dbs[i].Name),
			pager:    dbs[i].Pager,
			path:     dbs[i].Path,
			inMemory: dbs[i].InMemory,
		})
	}
}

// attachedReaderNamed returns the attached-database pager whose attach name
// equals q (case-insensitive), or (nil, false) if q names no attached database.
func (p *ReadOnlyPager) attachedReaderNamed(q string) (*ReadOnlyPager, bool) {
	lq := r33sFoldIdent(q)
	for _, ar := range p.attachedReaders {
		if ar.name == lq {
			return ar.pager, true
		}
	}
	return nil, false
}

// qualifierResolves is qualifierResolvesLocally widened to accept a qualifier
// that names one of this pager's attachedReaders (a cross-database read). It is
// the check the schema-qualifier VALIDATION passes use (validateSelectSchemaQualifiers,
// validateExprSchemaQualifiersIn, validateColumnRefs), so a qualifier pointing at
// an attached database is accepted there and later routed by resolveFrom, while
// a qualifier that resolves nowhere is still declined. With no attachedReaders
// it is exactly qualifierResolvesLocally -- the single-file behavior is unchanged.
func (p *ReadOnlyPager) qualifierResolves(q string) (bool, error) {
	ok, err := p.qualifierResolvesLocally(q)
	if err != nil || ok {
		return ok, err
	}
	if _, found := p.attachedReaderNamed(q); found {
		return true, nil
	}
	return false, nil
}

// pagerHasName reports whether this pager's OWN database can resolve the
// unqualified name as a base table, a view, a persisted virtual table, or an
// eponymous virtual-table module -- i.e. everything resolveFrom's base-table
// branches (join.go) would bind it to on this pager. It is used only by
// itemOwner (and fromItemIsDerived) to decide, for an UNqualified name, whether
// the primary owns it (and thus shadows any same-named attached table) or the
// name must be searched for among the attachedReaders. A schema read error is
// treated as "does not resolve here" -- conservative, never a wrong bind.
func (p *ReadOnlyPager) pagerHasName(name string) bool {
	if isV, err := p.isVtabItem(FromItem{Table: name}); err == nil && isV {
		return true
	}
	if _, err := p.resolveTable(name); err == nil {
		return true
	}
	if _, ok, err := p.resolveViewByName(name); err == nil && ok {
		return true
	}
	return isEponymousVtabName(name)
}

// dbIdxIsTemp reports whether a joinSource's dbIdx names this connection's TEMP
// database. It is a second FILE (temp_store.go), like an ATTACHed one, but it
// is this SESSION's own -- same connection state, same pager rules, opened and
// committed alongside main -- so a planner gate that declines a foreign
// database should still treat it as local. C SQLite draws the same line:
// whereReverseScanOrder (where.c:6727) sets revMask for EVERY FROM item, with
// no notion of which database the table lives in.
func (p *ReadOnlyPager) dbIdxIsTemp(dbIdx int) bool {
	return p != nil && dbIdx > 0 && dbIdx <= len(p.attachedReaders) &&
		p.attachedReaders[dbIdx-1].name == "temp"
}

// pagerForScope resolves a "main."/"temp." qualifier to the DATABASE it names.
// They are two files now (temp_store.go): the TEMP database rides as a reader
// on the primary, and the primary rides back on it (SetAttachedReaders), so
// either one can be reached from either side. Reports false when the named
// database is not open at all -- a "temp." qualifier on a connection that never
// made a temp object.
func (p *ReadOnlyPager) pagerForScope(scope schemaScope) (*ReadOnlyPager, bool) {
	isTemp := p != nil && equalFoldName(p.localSchema, "temp")
	if scope == scopeTemp {
		if isTemp {
			return p, true
		}
		if tp, found := p.attachedReaderNamed("temp"); found {
			return tp, true
		}
		return nil, false
	}
	if !isTemp {
		return p, true
	}
	if mp, found := p.attachedReaderNamed("main"); found {
		return mp, true
	}
	return nil, false
}

// dbIndexOf returns the db-index the VDBE join compiler (resolveJoinSources,
// vdbe_join_codegen.go) records for a FROM item resolved to owner by
// itemOwner: 0 when owner is this (primary) pager itself, or i+1 when owner is
// p.attachedReaders[i]. This is the VDBE analogue of what resolveFrom
// (join.go) does at PLAN time by just calling rp's own methods directly --
// the VDBE instead compiles ONCE and runs the resulting bytecode possibly many
// times, so it cannot close over a *ReadOnlyPager pointer inside an
// Instruction (P4 payloads are shared across every cursor the SAME compiled
// Program might later run under, e.g. a foreign view's own compiled body --
// see resolveViewSource/derivedSource); it stores this small integer instead
// and resolves it back to a pager at VM runtime via
// m.pager.attachedReaders[dbIdx-1].pager (OpOpenRead/OpOpenDerived, vdbe.go).
// Returns 0 (never found) only if owner is neither p nor one of its
// attachedReaders, which itemOwner never actually returns.
func (p *ReadOnlyPager) dbIndexOf(owner *ReadOnlyPager) int {
	if owner == p {
		return 0
	}
	for i, ar := range p.attachedReaders {
		if ar.pager == owner {
			return i + 1
		}
	}
	return 0
}

// dbIdxForSchema resolves a WRITTEN schema qualifier (ColumnExpr.Schema, a
// three-part "schema.table.column" reference's database name) to the SAME
// dbIndexOf encoding a compiled scope's tableScope.dbIdx already carries --
// the compile-time mirror of resolve.c's lookupName, which translates zDb
// into a Schema* pointer ONCE, up front, by a linear scan of db->aDb[]
// (resolve.c:309-331, ~"Translate the schema name in zDb into a pointer to
// the corresponding schema"). ok is false for schema=="" (no qualifier was
// written -- the overwhelming common case, where matching stays NAME-only,
// unfiltered by database) or when pager is nil; every caller must treat
// ok==false as "apply no dbIdx filter", never as "filter to dbIdx 0".
//
// A schema that resolves to neither the local pager nor a named attachedReader
// cannot reach here in practice: every call site that hands resolveInScopes a
// non-empty x.Schema has already had it validated by qualifierResolves
// (compileColumn, vdbe_codegen.go, or validateExprSchemaQualifiersIn,
// schema_qualifier.go) before compilation gets this far. The zero value (0,
// false) is returned defensively rather than panicking or guessing.
func dbIdxForSchema(pager *ReadOnlyPager, schema string) (dbIdx int, ok bool) {
	if schema == "" || pager == nil {
		return 0, false
	}
	if local, err := pager.qualifierResolvesLocally(schema); err == nil && local {
		return 0, true
	}
	if ar, found := pager.attachedReaderNamed(schema); found {
		return pager.dbIndexOf(ar), true
	}
	return 0, false
}

// forDB is dbIndexOf's inverse: the pager a dbIdx names, resolved against this
// primary pager. It is the compile-time and expression-evaluation counterpart
// of the VM's own m.dbPager (vdbe.go) -- both exist because a dbIdx is the only
// safe way to record WHICH database something came from (see dbIndexOf).
//
// A dbIdx this pager cannot serve yields nil rather than an error or a panic:
// every caller already treats a nil pager as "this is not resolvable here" and
// declines, and a nil *ReadOnlyPager is legitimately reachable from the write
// path.
func (p *ReadOnlyPager) forDB(dbIdx int) *ReadOnlyPager {
	if p == nil || dbIdx == 0 {
		return p
	}
	if dbIdx-1 >= len(p.attachedReaders) {
		return nil
	}
	return p.attachedReaders[dbIdx-1].pager
}

// itemOwner returns the pager that owns base-table FROM item it -- this primary
// pager, or one of its attachedReaders (a cross-database read). It is consulted
// by resolveFrom ONLY when attachedReaders is non-empty; the single-file path
// never calls it. The caller guarantees it.Subquery == nil.
//
//   - A schema-qualified item resolves on the primary when the qualifier is
//     local (""/main/temp/localSchema, honoring TEMP taint via
//     qualifierResolvesLocally), on the matching attachedReader when it names an
//     attached database, and is declined ("no such table") when it names no
//     database this pager can serve.
//   - An UNqualified item stays on the primary when the primary owns it as a
//     CTE, a temp-catalog name, or a table/view/vtab (so the primary -- "main"
//     -- shadows any same-named attached table, matching SQLite's search
//     order); otherwise it routes to the first attachedReader, in attach order,
//     whose database has a table or view of that name; otherwise it falls back
//     to the primary, which then raises the ordinary "no such table" error.
//
// On a DELEGATED cross-database write session the primary is NOT "main" and so
// does not come first: originReadersBefore says how many readers precede it --
// see that field and attach_write.go.
func (p *ReadOnlyPager) itemOwner(it FromItem) (*ReadOnlyPager, error) {
	readers, before := p.attachedReaders, p.originReadersBefore
	// A stored VIEW's body binds strictly inside the view's OWN database: real
	// SQLite refuses to create a non-TEMP view that references another one
	// ("view vm cannot reference objects in database aux") and answers
	// "no such table: aux.m1" for a view IN aux whose body names main's m1
	// (both verified directly). So the ORIGINATING readers a delegated write
	// session carries are invisible while a body compiles -- viewExpansion is
	// non-empty for exactly that span (resolveViewSource). An ordinary session's
	// readers are the connection's own and keep whatever reach they had.
	if before > 0 && len(p.viewExpansion) > 0 {
		readers, before = nil, 0
	}
	// "sqlite_temp_master"/"sqlite_temp_schema" is the TEMP database's OWN
	// catalog, whichever database this pager is: it is the one name that always
	// means aDb[1]. With a temp database in play its rows are simply page 1 of
	// that file (temp_store.go); without one there is nothing to route to and
	// the empty filtered source answers, exactly as C SQLite's empty temp
	// catalog does.
	if isTempSchemaCatalogName(it.Table) && !equalFoldName(it.Schema, "main") {
		for _, ar := range readers {
			if ar.name == "temp" {
				return ar.pager, nil
			}
		}
	}
	if it.Schema != "" {
		ok, err := p.qualifierResolvesLocally(it.Schema)
		if err != nil {
			return nil, err
		}
		if ok {
			return p, nil
		}
		lq := r33sFoldIdent(it.Schema)
		for _, ar := range readers {
			if ar.name == lq {
				if err := p.noteAttachedTouched(ar.name); err != nil {
					return nil, err
				}
				return ar.pager, nil
			}
		}
		return nil, fmt.Errorf("engine: no such table: %s.%s", it.Schema, it.Table)
	}
	// Unqualified: the primary (main) has priority for a CTE, a temp-catalog
	// name, or any table/view/vtab it owns -- so it shadows a same-named
	// attached table exactly as SQLite's search order does.
	if _, ok := p.lookupCTE(it.Table); ok {
		return p, nil
	}
	// An unqualified SCHEMA CATALOG name always means MAIN's catalog, never a
	// later database's -- and never the TEMP database's, even though temp is
	// searched first for an ordinary name (C SQLite: "sqlite_master" is
	// aDb[0]'s, "sqlite_temp_master" is aDb[1]'s, and neither spelling ever
	// means the other): with "two" attached, real
	// SQLite answers "SELECT sql FROM sqlite_master WHERE name='v1'" out of
	// main's schema (verified in situ -- view.test segment 7, which is what
	// caught this). It has to be spelled out here because the catalog is not a
	// row-store table: pagerHasName below resolves it through resolveTable,
	// which does not know the name, so the primary would fail to claim it and
	// the search would fall through to the first attached database that does --
	// reading the WRONG database's schema and, in that test, quietly returning
	// no rows for a view that exists.
	if isMainSchemaCatalogName(it.Table) || isTempSchemaCatalogName(it.Table) {
		// The TEMP database never claims an unqualified catalog name: the temp
		// spelling was routed to it above, and "sqlite_master" is main's. A
		// DELEGATED write session's leading readers are its ORIGINATING
		// database, which does (see originReadersBefore).
		for _, ar := range readers[:before] {
			if ar.name != "temp" {
				return ar.pager, nil
			}
		}
		return p, nil
	}
	// An EPONYMOUS virtual table -- a "pragma_*" wrapper, generate_series,
	// json_each -- is registered on the CONNECTION, not in any database's
	// schema (sqlite3VtabEponymousTableInit, vtab.c:1289), so every pager here
	// would claim it and the search order would hand it to whichever comes
	// first. It stays on the primary, which is where a pragma wrapper's own
	// per-database routing lives (queryPragmaStmt, pragma.go).
	if isEponymousVtabName(it.Table) {
		return p, nil
	}
	for _, ar := range readers[:before] {
		if ar.pager.pagerHasName(it.Table) {
			if err := p.noteAttachedTouched(ar.name); err != nil {
					return nil, err
				}
			return ar.pager, nil
		}
	}
	if p.pagerHasName(it.Table) {
		return p, nil
	}
	for _, ar := range readers[before:] {
		if ar.pager.pagerHasName(it.Table) {
			if err := p.noteAttachedTouched(ar.name); err != nil {
					return nil, err
				}
			return ar.pager, nil
		}
	}
	return p, nil // let the primary raise the ordinary "no such table" error
}
