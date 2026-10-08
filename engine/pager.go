package engine

import (
	"fmt"
	"io"
)

// ReadOnlyPager is a read handle on one database: the segment file and its
// delta (segs), and what every query runs against.
type ReadOnlyPager struct {
	closer io.Closer
	meta   pagerMeta

	// storedIndexEntries is IntegrityCheckOptions.StoredIndexEntries, for the
	// one check it was handed to.
	storedIndexEntries func(index string) ([][]Value, bool)

	// segEncodingDeclared is "this SEGMENT file's catalog names a text encoding",
	// which it does only once the database has a schema (ConvertedCatalog.Encoding).
	// It is what stands in for nPages>0 when ATTACH asks whether the file it just
	// opened has an encoding that could disagree with the connection's -- a segment
	// file has no pages to count, and the SQLite reader's own empty-file case
	// stamps UTF-8 into the header it synthesizes, so neither of those can answer
	// the question. See attach.go's agreement check.
	segEncodingDeclared bool

	// segAutoVacuum is "PRAGMA auto_vacuum" as this SEGMENT file records it
	// (ConvertedCatalog.AutoVacuumPlus1), which is where the mode lives on a
	// format with no header.
	segAutoVacuum int

	// segs is the segment file this snapshot reads (segment_read.go).
	segs *segSource

	// viewExpansion is the stack of view names currently being expanded on
	// the current call path (resolveFrom -> resolveViewRowsGuarded ->
	// viewOutputRows -> resolveDerivedRows -> execSelect -> resolveFrom ->
	// ...), used to detect a view that (directly, or through a chain of
	// other views) references itself -- see view.go's
	// resolveViewRowsGuarded. Empty except while a view's own SELECT is
	// actively being resolved.
	viewExpansion []string

	//	// viewRefCount counts how many times each view has been expanded in
	//	// the current top-level statement, which the viewExpansion stack cannot
	//	// see. view3.test's doubling chain (v2 AS SELECT * FROM v1 UNION SELECT
	//	// * FROM v1, ... up to v32768) has no cycle, yet expands v1 2^16 times.
	//	// C caps this with `too many references to "v1": max 65535`, and so does
	//	// this -- otherwise it hangs. Reset per top-level statement.
	viewRefCount map[string]int

	//	// viewBodyFacts memoizes a view's resolved output columns and rows
	//	// (resolveViewRowsGuarded's result) per distinct view (viewBodyFactsKey),
	//	// not per occurrence. Lazily allocated, never reset: a ReadOnlyPager is a
	//	// fixed snapshot, so a view's body cannot change under it.
	//	//
	//	// C computes a view's column facts once and caches them on the schema
	//	// Table (sqlite3ViewGetColumnNames, build.c:3210-3212):
	//	//
	//	//	if( !IsVirtual(pTable) && pTable->nCol>0 ) return 0;
	//	//	return viewGetColumnNames(pParse, pTable);
	//	//
	//	// Without this, each helper asking about a view (subqueryColumnNames,
	//	// derivedColumnAffinities, derivedColumnCollations, ...) re-executed its
	//	// body, compounding at every level of a nested view chain.
	//	//
	//	// countViewReference still runs before this cache is consulted, so the
	//	// reference cap fires where C's nTabRef++ (select.c:6044) would. Only the
	//	// redundant work is skipped; the per-occurrence walk (select.c:6072-6080)
	//	// still runs via resolveViewColumns' compileSubProgram.
	viewBodyFacts map[viewBodyFactsKey]viewBodyFactsEntry

	//	// viewItemFacts is viewBodyFacts' per-occurrence twin, keyed by the
	//	// *FromItem pointer. Several helpers (derivedColumnCollations,
	//	// derivedColumnComputed, applyViewColumnNames) re-resolve the same
	//	// FromItem, and each would call countViewReference again for what is
	//	// one occurrence. A hit means this exact occurrence was already resolved
	//	// (every real occurrence is its own AST node), so it skips the count too:
	//	// select.c:6044 increments once per FROM item, not per caller asking.
	viewItemFacts map[*FromItem]viewBodyFactsEntry

	//	// cteScopes is the stack of WITH frames in dynamic scope, one map per
	//	// SelectStmt with CTEs whose compile has not returned, pushed by
	//	// pushCTEScope (cte.go). Nested statements are reached by reentering on
	//	// the same call stack, so an outer WITH stays visible to everything
	//	// inside it without threading a parameter. lookupCTE searches from the
	//	// end, so an inner WITH shadows an outer one.
	cteScopes []map[string]*cteBinding

	//	// cteExpansion is the stack of CTE definitions currently being expanded
	//	// (resolveCTERows), the CTE analogue of viewExpansion, used to reject
	//	// "circular reference: %s" when a CTE would reference itself outside the
	//	// recognized recursive shape (detectRecursiveShape). A recursive CTE's
	//	// own name in its recursive arms (cteBinding.selfRef) bypasses it.
	//	//
	//	// Keyed on the binding, not the name: an inner WITH shadows an outer one
	//	// of the same name, so "WITH RECURSIVE t21(a,b) AS (WITH t21(x) AS
	//	// (VALUES(1)) SELECT x,x FROM t21 ...)" reads the inner t21 and is not a
	//	// cycle (with1.test). pushCTEScope builds a fresh binding per frame.
	cteExpansion []cteExpansionFrame

	// fromBodyDepth counts the view, CTE and FROM-subquery bodies being
	// compiled on the current call path (enterFromBody) -- the one context in
	// which an unusable INDEXED BY partial index is a decline and not C's
	// "no query solution" (indexedByNoSolution, where_plan_indexed_by.go).
	fromBodyDepth int

	// r41ArmsHeld is non-zero while compileCompound compiles the arms of a
	// compound this package does not combine the way SQLite does -- an ORDER
	// BY (multiSelectByMerge) or an operator other than UNION ALL -- so that
	// compileSubProgram leaves those arms' plans alone. See
	// r38cFlattenCompoundArms (flatten_limit_r35c.go) for the measured reason.
	r41ArmsHeld int

	//	// derivedSchemaOnly makes resolveFrom's derived-table branch derive the
	//	// subquery's columns without running it (derivedSchemaCols), falling
	//	// back to materializing when it cannot. It is only turned on as a retry
	//	// by resolveArmOutputsOuter after the materializing path failed, via
	//	// resolveFromSchemaOnly (the only writer).
	//	//
	//	// resolveFrom's callers only want column metadata, but its derived-table
	//	// branch executes the subquery to get it. C never runs anything to learn
	//	// a FROM subquery's columns (sqlite3ExpandSubquery, select.c:5885):
	//	//
	//	//	select.c:5901   while( pSel->pPrior ){ pSel = pSel->pPrior; }
	//	//	select.c:5902   sqlite3ColumnsFromExprList(pParse, pSel->pEList,&pTab->nCol,&pTab->aCol);
	//	//
	//	// so a body whose LIMIT/OFFSET fails at run time still resolves names
	//	// (misc1.test misc1-26.0).
	derivedSchemaOnly bool

	//	// ctasPlaceholder marks a running CREATE TABLE ... AS SELECT whose new
	//	// table's schema row is reserved, so a catalog read by that SELECT shows
	//	// one extra all-NULL row, last, in ctasPlaceholderScope's catalog -- as C
	//	// does:
	//	//
	//	//	CREATE TABLE c1 AS SELECT rowid,type,name,tbl_name,rootpage,sql
	//	//	                     FROM sqlite_master;
	//	//	  1|'table'|'m1' |'m1'|2|'CREATE TABLE m1(x,y,z)'
	//	//	  2|'index'|'m1x'|'m1'|3|'CREATE INDEX m1x ON m1(x)'
	//	//	  3|NULL   |NULL |NULL|NULL|NULL        <- the reserved row
	//	//
	//	// Only in the new table's own catalog (temp or main), and every read of
	//	// it within the statement sees it, nested subqueries included.
	ctasPlaceholder      bool
	ctasPlaceholderScope schemaScope

	// fts5Names caches the fts5 virtual tables (by lower-cased name) present in
	// this read snapshot's schema, each with the TOKENIZER its CREATE VIRTUAL
	// TABLE spells (fts5SchemaTok), computed lazily on the first MATCH
	// evaluation (see isFts5Table, fts5_match.go). nil until first consulted;
	// safe to cache because a ReadOnlyPager is a fixed read snapshot whose schema
	// does not change during its lifetime.
	fts5Names map[string]fts5SchemaTok

	//	// fts3Tables caches this snapshot's fts3/fts4 tables by lower-cased name
	//	// with their columns (fts3TableInfo); fts3Matches memoizes the docid set
	//	// of one MATCH (table + default column + query; fts3MatchDocids);
	//	// fts3DocStats memoizes an FTS4 table's %_stat and %_docsize rows for
	//	// matchinfo(). Safe because the snapshot cannot change, and it keeps a
	//	// per-row MATCH from re-decoding the index per row.
	fts3Tables   map[string]*fts3TableMeta
	fts3Matches  map[string]*fts3MatchResult
	fts3DocStats map[string]*fts3DocStats

	// schemaRows / schemaLoaded memoize the full sqlite_schema scan (see Schema),
	// and resolvedTables memoizes the parsed columns/root of a base table by name
	// (see resolveTable). Both are safe for the exact reason fts5Names is: a
	// ReadOnlyPager is a fixed read snapshot whose sqlite_schema content cannot
	// change during its lifetime, so a scan/parse done once holds for every later
	// call on the same pager. This is what makes a warm pager reused across
	// autocommit statements (driver Conn's read cache) skip the b-tree scan +
	// CREATE-statement reparse that otherwise dominates a point lookup, and it
	// also collapses the several Schema() scans a single query already does
	// (resolveTable, checkIndexExists, view/fts lookups) into one.
	schemaRows     []SchemaRow
	// schemaFP memoises schemaRows' fingerprint for this pager's lifetime; 0 means
	// not computed yet. See ReadOnlyPager.schemaFingerprint.
	schemaFP uint64

	// fromColumnsOnly makes resolveFrom resolve a base table's columns without
	// reading its rows, for a caller that needs only the column metadata
	// (derivedSelectScope).
	fromColumnsOnly bool
	// fts5ConfigGen is DB.fts5ConfigGen as of this pager, stamped by stampPager.
	fts5ConfigGen uint64
	schemaLoaded   bool
	resolvedTables map[string]*resolvedTable

	// schemaRowsEdited is SnapshotPager's (writer.go) overlay-APPLIED sibling
	// of schemaRows: DB.writableSchemaEditedRows(), non-nil only while a
	// direct sqlite_master write is outstanding. schemaCatalogRows
	// (temp_catalog.go) -- the FILTERED sqlite_master/sqlite_temp_master row
	// source -- reads from this instead of calling Schema() whenever it is
	// set, so a filtered catalog read reflects the edit exactly like an
	// unfiltered raw-b-tree scan already does, while Schema()/schemaRows
	// above keep serving the PRE-edit list ordinary table resolution needs.
	// See writableSchemaEditedRows' own doc comment (schema_write_direct.go).
	schemaRowsEdited []SchemaRow

	//	// planCache memoizes the compiled Program for a top-level read, keyed by
	//	// its exact SQL text (QueryArgs), skipping lex/parse/compile -- the fixed
	//	// cost that dominates a point lookup. Safe because the snapshot's schema
	//	// is immutable, a Program depends only on text plus schema (parameters
	//	// are supplied at run time), and a run only reads the Program. Bounded by
	//	// planCacheMax entries.
	planCache map[string]*Program

	// sharedPlans is the SESSION's plan cache, when a session stamped this pager.
	// It outlives the pager, which is the whole point: a held connection builds a
	// new pager per statement. See sharedPlanCache.
	sharedPlans *sharedPlanCache

	// stat1 is the sqlite_stat1 snapshot plans over this pager are priced
	// with, and stat1Set whether anything decided it yet. See planStats.
	stat1    *planStat1
	stat1Set bool

	// caseSensitiveLike mirrors the connection's "PRAGMA case_sensitive_like"
	// flag at the moment this pager was materialized (see DB.SnapshotPager,
	// which copies DB.caseSensitiveLike here). It is consulted by LIKE
	// evaluation through both contexts that reach it -- a schema-time row
	// program reads it via evalCtx.pager (likeCaseSensitive, like_case.go), and
	// a statement's VDBE (OpLike/OpFunction, vdbe.go) via machine.pager, which
	// vdbe.likeCaseSensitive resolves the same way -- so a "SELECT ... LIKE ..."
	// run against this snapshot folds ASCII case (false, the default) or
	// compares case-sensitively (true). Pagers opened directly from disk
	// (Open/NewReadOnlyPager) leave it false: an on-disk read has no connection
	// carrying a pragma setting.
	caseSensitiveLike bool

	// deferFKs mirrors the connection's "PRAGMA defer_foreign_keys" at the
	// moment this pager was materialized, which is the only place its getter
	// can read it from: the flag is per-CONNECTION and lives for one
	// transaction (sqlite3VdbeHalt clears SQLITE_DeferFKs when the statement
	// leaves autocommit on), so there is nothing on disk to answer from.
	deferFKs bool

	// untrustedSchema mirrors the connection's "PRAGMA trusted_schema" at the
	// moment this pager was materialized, stored INVERTED so the zero value is
	// SQLite's ON default. It is read by exactly the two DDL-origin sites this
	// engine can reach -- a view body and a trigger body -- see
	// trusted_schema.go, which owns the rule.
	untrustedSchema bool

	// rtreeConns is the connection's r-tree registry (rtree_conn.go).
	rtreeConns *RtreeConnections

	// noAutoIndex mirrors the connection's "PRAGMA automatic_index" at the
	// moment this pager was materialized, stored INVERTED so the zero value is
	// SQLite's ON default -- which also makes a pager opened straight from a file
	// (Open/NewReadOnlyPager) behave like a fresh connection. It is read by
	// markWherePlanEligibility (where_plan_gate.go), which owns the rule: with
	// the flag clear, whereLoopAddBtree generates no automatic-index loop at all,
	// so the ported planner has nothing to reproduce and declines.
	noAutoIndex bool

	//	// writeSession is the live write session this snapshot was taken from
	//	// (SnapshotPager), nil for a pager opened from a file. It exists only for
	//	// fts3/fts4's optimize(), a scalar function whose purpose is a side effect
	//	// on the index (fts3_optimize.go). It writes the session's row store,
	//	// which no pager field aliases, and becomes visible at the next
	//	// SnapshotPager. Nothing else may use it to mutate: the caches here rely
	//	// on the snapshot not changing.
	writeSession *DB

	// foreignKeys mirrors the connection's "PRAGMA foreign_keys" flag for the
	// read side, which is the only place that pragma's GETTER is answered
	// (SnapshotPager copies DB.fkEnforce here; see fk.go). Enforcement itself
	// is entirely a write-path concern -- this is purely so reading the pragma
	// back reports what was set rather than a hardcoded 0.
	foreignKeys bool

	// writableSchema mirrors the connection's "PRAGMA writable_schema" flag,
	// for the same reason foreignKeys is here: the read side is where that
	// pragma's GETTER is answered, and it must report what the connection set
	// rather than a hardcoded 0. SnapshotPager copies DB.writableSchema here.
	writableSchema bool

	// schemaCorrupt is the session's shared, cookie-keyed verdict for
	// schemaLoadCorruptRefuses. A pager no session stamped gets a private one on
	// first use. See DB.schemaCorrupt.
	schemaCorrupt *schemaCorruptVerdict
	// schemaCorruptGen is the session's commit generation at the moment this pager
	// was built -- the second half of that cache's key.
	schemaCorruptGen uint64

	// changes / totalChanges / lastInsertRowid mirror the connection's
	// sqlite3_changes() / sqlite3_total_changes() / sqlite3_last_insert_rowid()
	// at the moment this pager was materialized, so a SELECT run over this
	// snapshot can answer changes()/total_changes()/last_insert_rowid() (see
	// conn_state.go). totalChangesOpaque / lastRowidOpaque carry the *DB fields
	// of the same names with them. A pager opened directly from disk
	// (Open/NewReadOnlyPager) leaves all five zero, which is exactly what a
	// freshly opened connection reports -- a driver whose autocommit reads run
	// on such a pager stamps its own carried values with SetConnState.
	changes            int64
	totalChanges       int64
	lastInsertRowid    int64
	totalChangesOpaque bool
	lastRowidOpaque    bool

	// secureDelete mirrors the connection's "PRAGMA secure_delete" setting (0,
	// 1 or 2) for the read side, which is where that pragma's GETTER is
	// answered. The setting changes nothing this engine does -- see
	// SecureDeleteResult -- so this is purely so reading it back reports what
	// was set rather than a hardcoded 0.
	secureDelete int

	// lockingDefault / lockingMain mirror the write session's two "PRAGMA
	// locking_mode" values (DB.lockingDefault / DB.lockingMain) for the read
	// side, which is where that pragma's GETTER is answered over a held
	// transaction's snapshot. The lock itself lives on the write session's
	// descriptor -- these are purely so reading the mode back reports what was
	// entered. See lockingModeResult.
	lockingDefault bool
	lockingMain    bool

	// walAutoCheckpointPlus1 is this CONNECTION's "PRAGMA wal_autocheckpoint"
	// threshold, pushed in by the driver so the read side can answer the getter
	// (the write session the setter ran on is long gone -- see
	// SetWalAutoCheckpoint). Stored BIASED BY ONE so the zero value means "this
	// connection never set one", which a plain int could not distinguish from a
	// deliberate 0 -- and 0 is meaningful: it DISABLES automatic checkpointing.
	walAutoCheckpointPlus1 int

	//	// fullColumnNames / shortColumnNamesOff mirror the connection's legacy
	//	// "PRAGMA full_column_names" / "short_column_names" when this pager was
	//	// materialized. They change only result column names (expandSelectList).
	//	// Their zero values are SQLite's defaults, so a pager opened from disk
	//	// uses the default rule. See colNameMode.
	fullColumnNames     bool
	shortColumnNamesOff bool

	// localSchema is the database-qualifier name this pager answers to when a
	// schema-qualified reference ("schema.table") is resolved -- "" means the
	// default "main". The driver Conn sets it (via SetLocalSchema) to an
	// ATTACHed database's name when it routes a statement referencing that
	// database to the database's own file, so a qualifier written in the SQL
	// ("aux.t") matches the file it was routed to. See qualifierResolvesLocally
	// (schema_qualifier.go). Pagers opened directly from disk leave it "".
	localSchema string

	// mainPath is THIS database's own file, and tempOpen whether the connection
	// has opened its TEMP database. Both exist for "PRAGMA database_list",
	// which reports one row per OPEN database (pragma.c:1436-1447) -- see
	// pragma_database_list.go.
	mainPath string
	tempOpen bool

	// inMemory marks this database as one with no durable backing of its own:
	// a ":memory:"/"mode=memory" DSN, or an ATTACH ':memory:'. This engine has
	// no memory-backed pager -- driver hands it an ordinary temp FILE
	// (driver/memdb.go, attach.go) -- so the file itself cannot be told
	// apart from a real database, and only the opener knows. Set via
	// SetInMemory; read only by PRAGMA journal_mode (pragma.go), which real
	// SQLite answers "memory" for such a database.
	inMemory bool

	// journalModeName is the read-side counterpart of DB.journalModeName: the
	// ROLLBACK journal mode the CONNECTION is in ("" == delete). It is here for
	// the same reason foreignKeys is -- the mode is connection state that real
	// SQLite keeps nowhere on disk, so a snapshot cannot recover it from the
	// file and a driver whose autocommit model opens a fresh session per
	// statement must carry it in (SetJournalMode). WAL, which the header DOES
	// carry, still wins over it -- see journalMode.
	journalModeName string

	// tempStore is the read-side counterpart of DB.tempStore: the value
	// "PRAGMA temp_store" last set on this CONNECTION (0 default, 1 file,
	// 2 memory). Here for journalModeName's reason -- C SQLite keeps
	// db->temp_store per connection and nowhere in any file, so only the
	// connection can supply it (SetTempStore).
	tempStore uint8

	// tuning is the read-side counterpart of DB.tuning: the per-database
	// "PRAGMA synchronous / cache_size / journal_size_limit / mmap_size" values
	// the connection has set, so a getter over this snapshot answers them
	// rather than the defaults. See pragma_tuning.go.
	pragmaState *PragmaConnState

	// tempJournalMode is the read-side counterpart of DB.tempJournalMode: the
	// TEMP database's own journal mode, which C SQLite likewise keeps only
	// on the connection. See tempJournalModeResult (pragma.go).
	tempJournalMode string

	//	// attachedReaders are the other attached databases this pager can
	//	// resolve a base-table FROM item against (set by SetAttachedReaders).
	//	// resolveFrom routes each item to its owner: this pager for local names,
	//	// or the matching attachedReader for a foreign qualifier or a name that
	//	// lives only in an attachment (this pager first, then attach order). See
	//	// cross_db.go.
	attachedReaders []attachedReader

	// originReadersBefore is nonzero ONLY on a DELEGATED cross-database write
	// session's snapshot (attach_write.go): it counts how many leading
	// attachedReaders come BEFORE this database in the ORIGINATING connection's
	// unqualified search order, so itemOwner searches those first instead of
	// letting this database shadow them. Zero everywhere else, which is exactly
	// today's "primary first" order. Nonzero also MARKS the whole reader set as
	// the originating connection's, which a stored VIEW body must not see --
	// see itemOwner.
	originReadersBefore int
}

// attachedReader binds one attached database's name to its own read snapshot,
// so a cross-database read can route a FROM item to the file that owns it. name
// is lower-cased for case-insensitive matching; pager is a ReadOnlyPager over
// that database's file, with its own localSchema set to the attach name.
type attachedReader struct {
	name  string
	pager *ReadOnlyPager
	// path and inMemory are what "PRAGMA database_list" reports for this
	// database: sqlite3BtreeGetFilename's value (pragma.c:1445), which is the
	// ABSOLUTE file name, or "" for one with no durable file of its own.
	path     string
	inMemory bool
}

// noteAttachedTouched marks attachment q as touched by the transaction open
// on this snapshot's session, so a later DETACH is refused as C refuses it
// (attachedDB.touchedInTxn). A plain read-only pager marks nothing.
//
// It also refuses a read through a vfs=memdb alias while a sibling alias of
// the same store holds this transaction's write lock: memdb.c refuses a
// SHARED request while any handle holds RESERVED (memdb.c:383-389):
//
//	case SQLITE_LOCK_SHARED: {
//	  if( p->nWrLock>0 ){
//	    rc = SQLITE_BUSY;
//
// A unix file instead lets the reader see stale committed pages
// (refreshAttachedWriteReaders). A handle already holding SHARED (read or
// written earlier in the transaction, or under locking_mode=exclusive)
// makes no new request, so nothing is refused.
func (p *ReadOnlyPager) noteAttachedTouched(q string) error {
	if p == nil || p.writeSession == nil {
		return nil
	}
	db := p.writeSession
	if !db.inTransaction() && len(db.savepoints) == 0 {
		return nil
	}
	ad := db.attachedNamed(q)
	if ad == nil {
		return nil
	}
	holdsShared := ad.touchedInTxn || ad.reservedInTxn || (ad.wdb != nil && ad.wdb.lockingHeld)
	if ad.isMemdb && !holdsShared {
		for _, sib := range db.attached {
			if sib != ad && sib.isMemdb && sib.memdbName == ad.memdbName && sib.reservedInTxn {
				return fmt.Errorf("%w: reading ATTACHed database %s: it is the same in-memory store as %s, which holds this transaction's write lock (C SQLite: memdb refuses a new SHARED lock while another handle holds RESERVED)", ErrBusy, ad.name, sib.name)
			}
		}
	}
	ad.touchedInTxn = true
	return nil
}

// colNameMode is the resolved legacy column-naming state for one query --
// the two "PRAGMA full_column_names" / "PRAGMA short_column_names" flags in
// their natural (non-inverted) sense, so short defaults to true. It is the
// single input expandSelectList (query.go) consults to decide a result
// column's reported name. defaultColNameMode is SQLite's out-of-the-box
// state and is passed by every INTERNAL expandSelectList caller (derived-
// table resolution, CREATE TABLE AS, view definition, subquery validation),
// whose column names C SQLite computes WITHOUT applying these pragmas.
type colNameMode struct {
	full  bool // PRAGMA full_column_names (default false)
	short bool // PRAGMA short_column_names (default true)

	//	// subqueryCols selects sqlite3ColumnsFromExprList's naming rule instead
	//	// of generateColumnNames's. Building a table's columns (a view, FROM
	//	// subquery, CTE, CREATE TABLE AS) peels COLLATE and likely()/unlikely()/
	//	// likelihood() off an unaliased item before asking whether it is a
	//	// column reference; naming a result set does not. Over t9(g TEXT):
	//	//
	//	//	SELECT g COLLATE nocase FROM t9            -> "g COLLATE nocase"
	//	//	SELECT * FROM (SELECT g COLLATE nocase ..) -> "g"
	//	//	CREATE TABLE u AS SELECT g COLLATE nocase FROM t9 -> u(g TEXT)
	//	//	CREATE VIEW v AS SELECT likely(g) FROM t9; table_info -> "g"
	subqueryCols bool
}

var defaultColNameMode = colNameMode{full: false, short: true, subqueryCols: true}

// colNameMode returns the pager's active legacy column-naming state (see the
// ReadOnlyPager.fullColumnNames / shortColumnNamesOff fields). A nil pager --
// the pager-free FROM-less path -- has no connection state and uses the
// default (full=OFF, short=ON), matching the engine's prior naming behavior.
func (p *ReadOnlyPager) colNameMode() colNameMode {
	if p == nil {
		return defaultColNameMode
	}
	return colNameMode{full: p.fullColumnNames, short: !p.shortColumnNamesOff}
}

// pagerMeta is the database values a read answers PRAGMAs from.
type pagerMeta struct {
	schemaCookie  uint32
	userVersion   uint32
	applicationID uint32
	encoding      TextEncoding
	pageSize      uint32
	wal           bool
	captureGuard  string // ConvertedCatalog.CaptureGuard
}

// newReadOnlyPager is a pager over an empty database, which a segment open
// (segment_open.go) or an attachment then fills.
func newReadOnlyPager() *ReadOnlyPager {
	return &ReadOnlyPager{meta: pagerMeta{encoding: UTF8, pageSize: 4096}}
}

// Close releases the database this pager reads, if it owns it.
func (p *ReadOnlyPager) Close() error {
	if p.closer != nil {
		return p.closer.Close()
	}
	return nil
}
