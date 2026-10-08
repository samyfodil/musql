// Query and QueryArgs: the read entry points. A SELECT is parsed (sql_parser.go),
// compiled to a VDBE program (vdbe_scan.go and friends) and run against the read
// pager. Table metadata comes from the stored CREATE TABLE text
// (parseCreateTableColumnsAndAutoIndexes), including the INTEGER PRIMARY KEY rowid alias whose
// NULL record slot is replaced with the rowid.
package engine

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// resolvedTable is a table's metadata as needed to execute a query against
// it: where its b-tree root is, its columns in declaration order (name,
// affinity, rowid-alias flag), and which column (if any) is the rowid alias.
type resolvedTable struct {
	// name is the table's own name, used only to build the one-table scope a
	// generated column's expression resolves against (computeGeneratedInto),
	// so a qualified self-reference such as "AS (t.a * 2)" binds. Empty for a
	// synthetic/derived resolvedTable, which never carries generated columns.
	name     string
	root     uint32
	cols     []columnInfo
	ipkIndex int // index into cols of the INTEGER PRIMARY KEY column, or -1

	// withoutRowid/pkColIdx describe a WITHOUT ROWID table (see
	// schema_write.go's tableMeta.withoutRowid/pkIndex doc comments for the
	// write side's identical concept): withoutRowid is true, ipkIndex is
	// always -1 (there is no rowid at all to alias), and pkColIdx holds the
	// PRIMARY KEY column indices into cols, in PRIMARY KEY clause order --
	// the order its record stores them in. false/nil for an ordinary rowid table.
	withoutRowid bool
	pkColIdx     []int
	// pkDesc is parallel to pkColIdx: that key column was declared DESC, so the
	// clustered order runs the other way for it. A WITHOUT ROWID table IS its
	// PRIMARY KEY index, so this is what its SCAN ORDER is -- see
	// vdbeCursor.orderByWithoutRowidPK (vdbe_cursor.go), which is where a
	// segment-backed one gets that order rather than the order rows were written.
	pkDesc []bool

	// segPlans caches segColPlansFor: the per-column read rules a lazy segment
	// read applies, which are properties of the table alone.
	segPlans atomic.Pointer[[]segColPlan]
}

// sqlTextTableIsWithoutRowid reports whether a CREATE TABLE ends in a top-level
// "WITHOUT ROWID". Unlike parseTableTailClauses it never errors (STRICT,
// AUTOINCREMENT and malformed text included), since the read path uses it on any
// table it may open.
func sqlTextTableIsWithoutRowid(createSQL string) bool {
	toks, err := lex(createSQL)
	if err != nil {
		return false
	}
	depth := 0
	seenOpen := false
	for i, t := range toks {
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case "(":
			depth++
			seenOpen = true
		case ")":
			depth--
			if seenOpen && depth == 0 {
				tail := toks[i+1:]
				for j := 0; j+1 < len(tail); j++ {
					if tail[j].kind == tkIdent && tail[j].upper() == "WITHOUT" &&
						tail[j+1].kind == tkIdent && tail[j+1].upper() == "ROWID" {
						return true
					}
				}
				return false
			}
		}
	}
	return false
}

// isMainSchemaCatalogName reports whether name is "sqlite_master" or
// "sqlite_schema" (case-insensitive), the main catalog's always-valid FROM
// target. sqlite_temp_master/sqlite_temp_schema are separate
// (isTempSchemaCatalogName).
func isMainSchemaCatalogName(name string) bool {
	return equalFoldName(name, "sqlite_master") || equalFoldName(name, "sqlite_schema")
}

// isTempSchemaCatalogName reports whether name is "sqlite_temp_master" or
// "sqlite_temp_schema". It is always a valid FROM target with C's 5-column
// shape, empty when no temp objects exist. With a temp database open it routes
// to that database's own catalog (itemOwner); otherwise to the empty
// materialized source in temp_catalog.go.
func isTempSchemaCatalogName(name string) bool {
	return equalFoldName(name, "sqlite_temp_master") || equalFoldName(name, "sqlite_temp_schema")
}

// vtabPrivateStoreModules names virtual-table modules whose rows this engine
// keeps in its own b-tree instead of C's shadow tables. Empty: fts3/fts4, fts5
// and rtree all write real shadow tables. Kept for a future module without one.
var vtabPrivateStoreModules = map[string]bool{}

// schemaHasPrivateStoreVtabRow reports whether the live schema holds a virtual
// table from one of those modules.
func (p *ReadOnlyPager) schemaHasPrivateStoreVtabRow() (string, bool, error) {
	rows, err := p.Schema()
	if err != nil {
		return "", false, err
	}
	for _, r := range rows {
		if r.Type != "table" || !isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		if _, mod, _, _, perr := parseCreateVirtualTableStmt(r.SQL); perr == nil &&
			vtabPrivateStoreModules[r33sFoldIdent(mod)] {
			return r33sFoldIdent(mod), true, nil
		}
	}
	return "", false, nil
}

// sqliteSchemaCatalogColumns is sqlite_master's fixed 5-column shape (type,
// name, tbl_name, rootpage, sql; declared TEXT/TEXT/TEXT/INT/TEXT). None is a
// rowid alias, so the rowid is only reachable as the pseudo-column.
func sqliteSchemaCatalogColumns() []columnInfo {
	return []columnInfo{
		{Name: "type", DeclType: "TEXT", Aff: typeAffinity("TEXT"), Collation: "BINARY"},
		{Name: "name", DeclType: "TEXT", Aff: typeAffinity("TEXT"), Collation: "BINARY"},
		{Name: "tbl_name", DeclType: "TEXT", Aff: typeAffinity("TEXT"), Collation: "BINARY"},
		{Name: "rootpage", DeclType: "INT", Aff: typeAffinity("INT"), Collation: "BINARY"},
		{Name: "sql", DeclType: "TEXT", Aff: typeAffinity("TEXT"), Collation: "BINARY"},
	}
}

// resolveTable looks up name in sqlite_schema (case-insensitive) and parses its
// CREATE TABLE for column metadata. sqlite_master/sqlite_schema has no row
// describing itself, so it is special-cased first and resolved to the schema
// b-tree with sqliteSchemaCatalogColumns, scanned like any table.
func (p *ReadOnlyPager) resolveTable(name string) (*resolvedTable, error) {
	return p.resolveTableIn(scopeAny, name)
}

// resolveTableIn is resolveTable scoped to one catalog: scopeMain/scopeTemp
// match only that schema's own objects, while scopeAny (an unqualified name)
// searches TEMP FIRST and then main -- C SQLite's documented resolution
// order, verified directly (with both a temp and a main "tbl", a bare "SELECT
// * FROM tbl" reads the temp one). See temp_schema.go.
func (p *ReadOnlyPager) resolveTableIn(scope schemaScope, name string) (*resolvedTable, error) {
	// Memoize the parsed table by name: a ReadOnlyPager is an immutable snapshot,
	// so the sqlite_schema lookup + CREATE-statement reparse below yields the same
	// *resolvedTable every time. This is what a warm pager reused across autocommit
	// statements (driver read cache) relies on to avoid re-parsing CREATE SQL
	// on every point lookup. Keyed lower-cased (resolution below is
	// case-insensitive) and by scope, since the same name can now name a
	// different table in each catalog. The catalog-name branch is left
	// unmemoized: it is a fixed synthetic descriptor built without any scan.
	key := scopeCacheKey(scope, name)
	if p != nil && !isMainSchemaCatalogName(name) {
		if rt, ok := p.resolvedTables[key]; ok {
			return rt, nil
		}
	}
	if p != nil && equalFoldName(p.localSchema, "temp") && (isMainSchemaCatalogName(name) || isTempSchemaCatalogName(name)) {
		// The TEMP database's own catalog, under either spelling: this pager IS
		// aDb[1] (temp_store.go), so its sqlite_schema is page 1 of its own
		// file, exactly as main's is of main's.
		return &resolvedTable{root: schemaRootPage, cols: sqliteSchemaCatalogColumns(), ipkIndex: -1}, nil
	}
	if isMainSchemaCatalogName(name) {
		// "temp.sqlite_master" names the TEMP DATABASE's own schema table, not
		// main's: it is served by that database's own pager (itemOwner,
		// cross_db.go) or, on a connection that never opened one, by the empty
		// materialized source (temp_catalog.go). Reaching main's b-tree here
		// would report main's rows under temp's name -- a WRONG answer.
		if scope == scopeTemp {
			return nil, fmt.Errorf("engine: no such table: %s", name)
		}
		return &resolvedTable{root: schemaRootPage, cols: sqliteSchemaCatalogColumns(), ipkIndex: -1}, nil
	}
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	// sqlite_sequence is per database in C (a TEMP AUTOINCREMENT table's row lives
	// in temp's copy), and each database has its own here too, so each pager's table
	// is the answer.
	found := findSchemaTableRow(rows, scope, name)
	if found == nil {
		return nil, fmt.Errorf("engine: no such table: %s", name)
	}
	if found.RootPage == 0 && !isCreateVirtualTableSQL(found.SQL) {
		// An ordinary table whose catalog row names NO b-tree: only a direct
		// catalog write leaves one, and every read or write of it is
		// SQLITE_CORRUPT there -- sqlite3InitOne loads the row and the cursor
		// open then fails, so the table is listed and unusable. Measured
		// against 3.53.3: "INSERT INTO sqlite_schema VALUES('table','u','u',0,
		// 'CREATE TABLE u(x)')" then "SELECT count(*) FROM u" is "database
		// disk image is malformed", while every other table still answers.
		return nil, fmt.Errorf("engine: database disk image is malformed")
	}
	withoutRowid := sqlTextTableIsWithoutRowid(found.SQL)
	cols, _, specs, err := parseCreateTableColumnsAndAutoIndexes(found.SQL, withoutRowid)
	if err != nil {
		return nil, fmt.Errorf("engine: table %s: %w", name, err)
	}
	ipk := -1
	for i, c := range cols {
		if c.IsRowidAlias {
			ipk = i
			break
		}
	}
	var pkColIdx []int
	var pkDesc []bool
	if withoutRowid {
		for _, s := range specs {
			if s.kind != "pk" {
				continue
			}
			pkColIdx = make([]int, len(s.cols))
			pkDesc = make([]bool, len(s.cols))
			for i, cname := range s.cols {
				if i < len(s.desc) {
					pkDesc[i] = s.desc[i]
				}
				for j, c := range cols {
					if equalFoldName(c.Name, cname) {
						pkColIdx[i] = j
						break
					}
				}
			}
			break
		}
	}
	rt := &resolvedTable{name: name, root: found.RootPage, cols: cols, ipkIndex: ipk, withoutRowid: withoutRowid, pkColIdx: pkColIdx, pkDesc: pkDesc}
	if p != nil {
		if p.resolvedTables == nil {
			p.resolvedTables = make(map[string]*resolvedTable)
		}
		p.resolvedTables[key] = rt
	}
	return rt, nil
}

// checkIndexExists errors "no such index: <name>" unless name is an index on the
// hinted table, for INDEXED BY. scope is the FROM item's catalog: C walks the
// resolved table's own index chain (select.c:5480-5490), so with a TEMP t
// shadowing MAIN t, "FROM temp.t INDEXED BY onlymain" is an error.
func (p *ReadOnlyPager) checkIndexExists(name, table string, scope schemaScope) error {
	_, err := p.hintedIndexRow(name, table, scope)
	return err
}

// hintedIndexRow is checkIndexExists returning the index's sqlite_schema row,
// for a caller that needs its SQL (indexedByNoSolution, where_plan_indexed_by.go).
func (p *ReadOnlyPager) hintedIndexRow(name, table string, scope schemaScope) (*SchemaRow, error) {
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	// Which catalog the hinted TABLE resolved in decides which catalog's
	// indexes are even candidates. A table this pager cannot see at all leaves
	// the match unscoped: reporting "no such index" for what is really "no such
	// table" would be the wrong error, and the caller resolves the table for
	// itself.
	tblTemp, tblKnown := false, false
	if table != "" {
		if r := findSchemaTableRow(rows, scope, table); r != nil {
			tblTemp, tblKnown = r.Temp, true
		}
	}
	for i := range rows {
		r := &rows[i]
		if r.Type != "index" || !equalFoldName(r.Name, name) {
			continue
		}
		// The hint must name an index ON THIS TABLE: an index of that name on
		// some OTHER table is still "no such index" -- verified directly
		// against C SQLite, "SELECT * FROM t1 INDEXED BY i3" where i3 is
		// an index on t2 reports "no such index: i3" (indexedby.test).
		if table != "" && !equalFoldName(r.TblName, table) {
			continue
		}
		if tblKnown && r.Temp != tblTemp {
			continue
		}
		return r, nil
	}
	return nil, fmt.Errorf("engine: no such index: %s", name)
}

// outputColumn is one resolved result-column: its expression and its
// (already-decided) result name.
type outputColumn struct {
	expr Expr
	name string
}

// Query parses and executes a single read-only SELECT statement (see the
// package-level grammar summary above) via a full table scan, returning
// result column names and rows in SQL-visible Value form. It returns a
// descriptive error -- never a panic -- for anything outside the supported
// grammar or semantics.
func (p *ReadOnlyPager) Query(sqlText string) (cols []string, rows [][]Value, err error) {
	return p.QueryArgs(sqlText, nil)
}

// QueryArgs is Query with bound parameters: args[i] binds parameter i+1 (see
// ParamExpr). A parameter beyond len(args) is NULL, as in C. ParseSelect(sql).Params
// reports a statement's parameters; BindByName builds args from names.
func (p *ReadOnlyPager) QueryArgs(sqlText string, args []Value) (cols []string, rows [][]Value, err error) {
	// Result rows from a segment mapping are sub-slices of mapped memory, which a
	// caller might read after the pager is closed (a SIGSEGV), so copy each result
	// cell's bytes here, at the boundary. Rows from Go-memory page images need no
	// copy.
	if p != nil && p.segs != nil {
		defer func() {
			if err == nil {
				rows = detachResultRows(rows)
			}
		}()
	}
	// A read's text reaches its write session here, so resolve the wal-index state
	// it deposited (resolveWalIndexRead): "SELECT * FROM t" opens the Wal and
	// "SELECT 1" does not, which later decides whether locking_mode=normal can leave
	// exclusive mode. Runs on the text before compiling, whether or not the
	// statement is supported, and before the plan cache.
	// The same classification for a pager with no write session (the driver's
	// autocommit read path); see pragma_defer_fk_read_pager.go.
	if derr := p.deferFKPagerReadGuard(sqlText); derr != nil {
		return nil, nil, derr
	}
	defer func() { p.deferFKPagerReadApply(sqlText, err) }()
	if serr := p.schemaLoadCorruptRefuses(sqlText); serr != nil {
		return nil, nil, serr
	}
	if p.writeSession != nil {
		p.writeSession.resolveWalIndexRead(sqlText)
		// "PRAGMA defer_foreign_keys" set OUTSIDE a transaction needs this
		// READ classified too -- a top-level query never reaches ExecArgs at
		// all, so deferFKAutocommitGuard's own placement there would miss it
		// entirely. It is the same classification the pager's own read path
		// uses (deferFKStatementClears, pragma_defer_fk_read_pager.go). A no-op
		// the moment the flag was never set outside a transaction, which is
		// every ordinary query.
		if p.writeSession.deferFKAutocommitActive() {
			clears, known := p.deferFKStatementClears(sqlText)
			if !known {
				return nil, nil, fmt.Errorf("%w: PRAGMA defer_foreign_keys is ON with no transaction open, and this READ is outside the shapes this engine can classify for whether C SQLite would then clear it (see engine/pragma_defer_fk_read_pager.go) -- declined rather than guessed: %s", errVDBEUnsupported, sqlText)
			}
			if clears {
				// C clears it at the autocommit Halt of any program that coded
				// OP_Transaction, whether that statement then succeeded or not
				// (vdbeaux.c:3434 / main.c:1530) -- but one that failed to
				// PREPARE never had a program ("SELECT nosuchcolumn FROM t" keeps
				// the flag), which is what errVDBESemantic marks. The pager's copy
				// is the one the driver reads back.
				defer func() {
					if !errors.Is(err, errVDBESemantic) {
						p.writeSession.SetDeferForeignKeys(false)
						p.deferFKs = false
					}
				}()
			}
		}
	}
	// Compiled-plan fast path: a repeated query skips lex+parse+compile entirely
	// (see the planCache field). Checked BEFORE the pragma lex below because a
	// cached entry is by construction a compilable SELECT, never a PRAGMA, so the
	// lex would be pure overhead on the hottest (already-seen) statements.
	if prog := p.cachedPlan(sqlText); prog != nil {
		rows, err = prog.execOuter(p, nil, args)
		return prog.ColNames, rows, err
	}
	trimmed := strings.TrimSpace(sqlText)
	if toks, lerr := lex(trimmed); lerr == nil && isPragmaStmt(toks) {
		return p.queryPragma(trimmed)
	}
	// "EXPLAIN <stmt>" / "EXPLAIN QUERY PLAN <stmt>" describe the compiled
	// program instead of running it (explain.go). Ahead of ParseSelect, which
	// has no EXPLAIN production of its own, and after the plan cache, which
	// only ever holds a runnable statement's text.
	if inner, mode := splitExplain(trimmed); mode != explainNone {
		return p.explainQuery(inner, mode)
	}
	stmt, err := ParseSelect(sqlText)
	if err != nil {
		return nil, nil, err
	}
	// PRAGMA optimize's own session-tracking hook (pragma_optimize_track.go):
	// a nil p.writeSession (no live write session behind this snapshot, e.g.
	// an ATTACHed reader queried on its own) is a no-op inside it, exactly
	// like resolveWalIndexRead above tolerates the same nil. Placed after the
	// plan-cache fast path already returned above, matching that function's
	// own documented limitation (a repeated cached query is tracked on its
	// first run only -- see markMaybeReanalyze's doc comment for why that is
	// safe).
	p.writeSession.markMaybeReanalyze(stmt)
	return p.execSelect(stmt, nil, args, sqlText)
}

// planCacheMax bounds the per-pager compiled-plan cache (see the planCache
// field): generous enough for any realistic distinct-statement working set on
// one connection, while capping the worst case (a workload that never repeats a
// query) at a fixed overhead -- once full it stops admitting new plans.
const planCacheMax = 256

// sharedPlanCache is the compiled-plan cache owned by a session rather than one
// pager: a held connection rebuilds its pager per statement, so a per-pager cache
// made prepared statements re-parse and re-compile every time. Keyed on the
// schema cookie and commit generation; a commit clears it, which costs a
// recompile but cannot be unsound.
type sharedPlanCache struct {
	m      map[string]*Program
	cookie uint32
	gen    uint64
	schema uint64
	// The column-name flags are part of the key: a Program carries its ColNames, so
	// two identical SELECTs either side of "PRAGMA full_column_names=ON" must not
	// share a plan (the per-pager drop in SetFullColumnNames cannot reach this
	// cache).
	fullCols     bool
	shortColsOff bool
	// The fts5 %_config generation too: a program reading "rank" bakes in which
	// auxiliary function it is (fts5RankFunction), and that lives in a shadow table
	// row that moves neither the cookie nor the fingerprint.
	fts5Gen uint64
	// ...and the sqlite_stat1 snapshot, which decides plans and moves nothing
	// above when an ANALYZE rewrites an existing table's rows.
	stat1 *planStat1
	// ...and the connection flags a compile reads: trusted_schema decides
	// whether a view body's MATCH is refused (trusted_schema.go), automatic_index
	// whether the planner may build a transient index, case_sensitive_like the
	// LIKE range bounds (where_plan.go). C expires every compiled statement when
	// a flag pragma changes (pragma.c:1190, "Many of the flag-pragmas modify the
	// code generated by the SQL compiler"); a plan compiled under the old value
	// answered "SELECT * FROM v" after "PRAGMA trusted_schema=OFF" where C
	// reports "unsafe use of MATCH()".
	untrusted, noAutoIndex, caseLike bool
}

// schemaFingerprint folds every object's name, owner and CREATE text into one
// number: the plan cache's real key. A plan bakes in its column list ("SELECT *"
// expands at compile time), and the cookie and generation miss some changes
// (ROLLBACK TO restoring an older t3, savepoint.test). Hashing the schema cannot
// miss a mutation site. One pass over already-loaded schema rows per statement.
func (p *ReadOnlyPager) schemaFingerprint() uint64 {
	// MEMOISED PER PAGER, because a pager's schema rows do not change under it and
	// two callers want this per statement: the plan cache's validity check and, on
	// a segment session, the read source's cache key. The hash walks every string
	// of every schema row, which measured at 1.9% of all CPU on a point lookup
	// when it ran twice.
	if p.schemaFP == 0 {
		p.schemaFP = schemaRowsFingerprint(p.schemaRows)
	}
	return p.schemaFP
}

// schemaRowsFingerprint folds a catalog into one number. Shared by the plan
// cache and by the rendered-catalog cache (segmentPager), which want the same
// question answered: is this the same schema as last time?
func schemaRowsFingerprint(rows []SchemaRow) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	mix := func(b string) {
		for i := 0; i < len(b); i++ {
			h ^= uint64(b[i])
			h *= prime64
		}
		h ^= 0xff
		h *= prime64
	}
	for i := range rows {
		r := &rows[i]
		mix(r.Type)
		mix(r.Name)
		mix(r.TblName)
		mix(r.SQL)
		mix(r.AliasOf)
		h ^= uint64(r.RootPage)
		h *= prime64
		h ^= uint64(r.Rowid)
		h *= prime64
		if r.Temp {
			h ^= 1
			h *= prime64
		}
	}
	// Never 0, because schemaFingerprint memoises on a zero field meaning "not
	// computed yet" and FNV-1a can land on 0 for some input. Setting one bit costs
	// nothing and removes the case rather than leaving it to be hit once.
	return h | 1
}

// planCacheFor returns the map to read and write, clearing it first when the
// schema it was compiled against has moved.
func (p *ReadOnlyPager) planCacheFor() map[string]*Program {
	c := p.sharedPlans
	// Not across statements while a database is attached: an attachment carries
	// per-statement state a plan cannot see (which alias holds the write lock, hence
	// "database is locked"). The per-pager cache still works within a statement.
	if c != nil && len(p.attachedReaders) > 0 {
		c = nil
	}
	if c == nil {
		// An unstamped pager keeps the old per-pager behaviour: correct, and no
		// worse than it was.
		if p.planCache == nil {
			p.planCache = make(map[string]*Program)
		}
		return p.planCache
	}
	if c.m == nil || c.cookie != p.meta.schemaCookie || c.gen != p.schemaCorruptGen ||
		c.schema != p.schemaFingerprint() ||
		c.fullCols != p.fullColumnNames || c.shortColsOff != p.shortColumnNamesOff ||
		c.fts5Gen != p.fts5ConfigGen || c.stat1 != p.planStats() ||
		c.untrusted != p.untrustedSchema || c.noAutoIndex != p.noAutoIndex || c.caseLike != p.caseSensitiveLike {
		c.m = make(map[string]*Program)
		c.cookie, c.gen, c.schema = p.meta.schemaCookie, p.schemaCorruptGen, p.schemaFingerprint()
		c.fullCols, c.shortColsOff = p.fullColumnNames, p.shortColumnNamesOff
		c.fts5Gen = p.fts5ConfigGen
		c.stat1 = p.planStats()
		c.untrusted, c.noAutoIndex, c.caseLike = p.untrustedSchema, p.noAutoIndex, p.caseSensitiveLike
	}
	return c.m
}

// cachedPlan returns the compiled Program memoized for sqlText, or nil if none
// is cached (the caller then compiles it and offers it back via cachePlan).
func (p *ReadOnlyPager) cachedPlan(sqlText string) *Program {
	if p.sharedPlans == nil && p.planCache == nil {
		return nil
	}
	return p.planCacheFor()[sqlText]
}

// cachePlan memoizes prog as sqlText's compiled Program (bounded by
// planCacheMax). Called by tryVDBEScan after a successful top-level compile
// (cacheKey != ""); a zero cacheKey -- every reentrant subquery / write-path
// execSelect call -- never caches.
func (p *ReadOnlyPager) cachePlan(sqlText string, prog *Program) {
	if sqlText == "" || prog == nil {
		return
	}
	m := p.planCacheFor()
	if _, ok := m[sqlText]; !ok && len(m) >= planCacheMax {
		return
	}
	m[sqlText] = prog
}

// execNoFrom executes a FROM-less SELECT through the VDBE: the select list is
// evaluated over one synthetic row. An aggregate list compiles via
// compileNoFromAggregate, a window query via compileNoFromWindow.
func (p *ReadOnlyPager) execNoFrom(stmt *SelectStmt, outer *evalCtx, params []Value) (cols []string, rows [][]Value, err error) {
	if len(stmt.GroupBy) > 0 {
		trimmed, gerr := p.validateNoFromGroupBy(stmt, outer, params)
		if gerr != nil {
			return nil, nil, gerr
		}
		stmt = trimmed
	}

	// A window call is never an aggregate call (isAggregateCall, sql_agg.go),
	// so a windowed select list must NOT be routed to the aggregate compiler
	// here -- compileSelectNoFromTrig dispatches it to compileNoFromWindow.
	hasWindow := selectHasWindow(stmt) || orderByHasWindow(stmt)
	hasAgg := false
	for _, c := range stmt.Columns {
		if c.Star {
			return nil, nil, fmt.Errorf("engine: no tables specified")
		}
		if containsAggregate(c.Expr) {
			hasAgg = true
		}
	}
	if hasAgg && !hasWindow {
		cols, rows, handled, verr := p.tryVDBENoFromAggregate(stmt, outer, params)
		if handled {
			return cols, rows, verr
		}
		// A genuine C-SQLite error is the STATEMENT's error, not evidence the
		// compiler is missing a shape: errVDBESemantic's own contract says it
		// "must be surfaced to the caller as the statement's real error", and
		// wrapping it in "not compilable to bytecode ... (no fallback)" both
		// buried the real diagnosis and put text in front of it that real
		// SQLite has no counterpart for. See subquery_validate.go.
		if errors.Is(verr, errVDBESemantic) {
		return nil, nil, verr
		}
		if verr != nil {
			return nil, nil, fmt.Errorf("engine: VDBE-only: FROM-less aggregate statement not compilable to bytecode: %w (no fallback)", verr)
		}
		return nil, nil, fmt.Errorf("engine: VDBE-only: FROM-less aggregate statement not compilable to bytecode (no fallback)")
	}

	// VDBE-ONLY: the bytecode compiler is the sole executor. A FROM-less
	// statement it cannot compile is a HARD ERROR -- never a silent fall-back to
	// execNoFromRowMode, which has been removed. The error
	// names the gap so the conformance gate surfaces exactly what the compiler
	// still owes.
	cols, rows, handled, verr := p.tryVDBENoFrom(stmt, outer, params)
	if handled {
		return cols, rows, verr
	}
	// A genuine C-SQLite error is the STATEMENT's error, not evidence the
	// compiler is missing a shape: errVDBESemantic's own contract says it
	// "must be surfaced to the caller as the statement's real error", and
	// wrapping it in "not compilable to bytecode ... (no fallback)" both
	// buried the real diagnosis and put text in front of it that real
	// SQLite has no counterpart for. See subquery_validate.go.
	if errors.Is(verr, errVDBESemantic) {
	return nil, nil, verr
	}
	if verr != nil {
		return nil, nil, fmt.Errorf("engine: VDBE-only: FROM-less statement not compilable to bytecode: %w (no fallback)", verr)
	}
	return nil, nil, fmt.Errorf("engine: VDBE-only: FROM-less statement not compilable to bytecode (no fallback)")
}

// validateNoFromGroupBy validates a FROM-less GROUP BY and returns stmt without
// it: one row is one group, so the clause changes no answer:
//
//	SELECT 986 AS x GROUP BY X ORDER BY X    986      (orderby1.test)
//	SELECT 1 GROUP BY random()               1        (not per-row grouping)
//	SELECT 1 AS a GROUP BY a HAVING a>5      no rows  (HAVING still applies)
//	SELECT 1 GROUP BY 1 HAVING count(*)>1    no rows  (the group holds ONE row)
//
// Only errors are observable:
//
//	SELECT 1 GROUP BY 2           1st GROUP BY term out of range - should be between 1 and 1
//	SELECT count(*) GROUP BY 1    aggregate functions are not allowed in the GROUP BY clause
//	SELECT 1 GROUP BY nosuchcol   no such column: nosuchcol
//
// The aggregate rule applies through an alias ("SELECT count(*) AS c GROUP BY
// c"). Terms are resolved by compiling them as a throwaway select list.
func (p *ReadOnlyPager) validateNoFromGroupBy(stmt *SelectStmt, outer *evalCtx, params []Value) (*SelectStmt, error) {
	if stmt.Having != nil {
		// Dropping the clause would take HAVING's own group with it: this
		// engine accepts HAVING only on an aggregate query, and the FROM-less
		// GROUP BY is what makes this one aggregate. C SQLite answers
		// "SELECT 1 AS a GROUP BY a HAVING a>5" with no rows and
		// "GROUP BY 1 HAVING count(*)>1" with no rows either (the single group
		// holds ONE row), so serving it means running the HAVING against that
		// group rather than removing it. Declined until then.
		return nil, fmt.Errorf("engine: GROUP BY without FROM combined with HAVING is not supported")
	}
	var probe []SelectColumn
	for i, g := range stmt.GroupBy {
		resolved, viaSelectList := g, false
		if n, ok := orderByOrdinal(g); ok {
			if n < 1 || n > int64(len(stmt.Columns)) {
				return nil, fmt.Errorf("engine: %s GROUP BY term out of range - should be between 1 and %d", ordinalWord(i+1), len(stmt.Columns))
			}
			resolved, viaSelectList = stmt.Columns[n-1].Expr, true
		} else if c, ok := g.(ColumnExpr); ok && c.Qualifier == "" && c.Schema == "" {
			for _, sc := range stmt.Columns {
				if sc.Alias != "" && equalFoldName(sc.Alias, c.Name) {
					resolved, viaSelectList = sc.Expr, true
					break
				}
			}
		}
		if containsAggregate(resolved) {
			return nil, fmt.Errorf("engine: aggregate functions are not allowed in the GROUP BY clause")
		}
		if !viaSelectList {
			// Not resolved through the select list, so it still has to name
			// something. Evaluating it is the check.
			probe = append(probe, SelectColumn{Expr: g})
		}
	}
	if len(probe) > 0 {
		check := *stmt
		check.Columns = probe
		check.GroupBy = nil
		check.Having = nil
		check.OrderBy = nil
		check.Limit, check.Offset = nil, nil
		check.LimitParam, check.OffsetParam = nil, nil
		if _, _, err := p.execNoFrom(&check, outer, params); err != nil {
			return nil, err
		}
	}
	out := *stmt
	out.GroupBy = nil
	return &out, nil
}

// ordinalWord renders 1/2/3 as "1st"/"2nd"/"3rd" for the out-of-range messages,
// matching C SQLite's wording.
func ordinalWord(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return fmt.Sprintf("%dth", n)
	case n%10 == 1:
		return fmt.Sprintf("%dst", n)
	case n%10 == 2:
		return fmt.Sprintf("%dnd", n)
	case n%10 == 3:
		return fmt.Sprintf("%drd", n)
	}
	return fmt.Sprintf("%dth", n)
}

// selectTreeMentionsTable reports whether stmt or anything reachable from it
// (compound arms, CTE bodies, derived tables) has a FROM item match accepts.
// match sees the whole FromItem, so a caller can tell "sqlite_master" from
// "aux.sqlite_master".
func selectTreeMentionsTable(stmt *SelectStmt, match func(FromItem) bool) bool {
	if stmt == nil {
		return false
	}
	for _, it := range stmt.From {
		if it.Table != "" && match(it) {
			return true
		}
		if it.Subquery != nil && selectTreeMentionsTable(it.Subquery, match) {
			return true
		}
		if exprTreeMentionsTable(it.On, match) {
			return true
		}
	}
	for _, cte := range stmt.CTEs {
		if selectTreeMentionsTable(cte.Select, match) {
			return true
		}
	}
	for _, c := range stmt.Columns {
		if exprTreeMentionsTable(c.Expr, match) {
			return true
		}
	}
	if exprTreeMentionsTable(stmt.Where, match) || exprTreeMentionsTable(stmt.Having, match) {
		return true
	}
	for _, e := range stmt.GroupBy {
		if exprTreeMentionsTable(e, match) {
			return true
		}
	}
	for _, ot := range stmt.OrderBy {
		if exprTreeMentionsTable(ot.Expr, match) {
			return true
		}
	}
	for _, arm := range stmt.Compound {
		if selectTreeMentionsTable(arm.Stmt, match) {
			return true
		}
	}
	return false
}

// exprTreeMentionsTable is selectTreeMentionsTable's expression-level
// counterpart: true if e contains a scalar/EXISTS/IN subquery
// (SubqueryExpr/ExistsExpr/InExpr.Sub) whose OWN statement names such a table
// in a FROM clause, checked via selectTreeMentionsTable recursively. Mirrors
// the exhaustive Expr-type switch of containsAggregate/exprContainsSubquery
// (sql_agg.go/ sql_ast.go).
func exprTreeMentionsTable(e Expr, match func(FromItem) bool) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return false
	case UnaryExpr:
		return exprTreeMentionsTable(x.X, match)
	case BinaryExpr:
		return exprTreeMentionsTable(x.L, match) || exprTreeMentionsTable(x.R, match)
	case IsNullExpr:
		return exprTreeMentionsTable(x.X, match)
	case InExpr:
		if selectTreeMentionsTable(x.Sub, match) {
			return true
		}
		if exprTreeMentionsTable(x.X, match) {
			return true
		}
		for _, a := range x.List {
			if exprTreeMentionsTable(a, match) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return exprTreeMentionsTable(x.X, match) || exprTreeMentionsTable(x.Lo, match) || exprTreeMentionsTable(x.Hi, match)
	case LikeExpr:
		return exprTreeMentionsTable(x.X, match) || exprTreeMentionsTable(x.Pattern, match) || exprTreeMentionsTable(x.Escape, match)
	case GlobExpr:
		return exprTreeMentionsTable(x.X, match) || exprTreeMentionsTable(x.Pattern, match)
	case CollateExpr:
		return exprTreeMentionsTable(x.X, match)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if exprTreeMentionsTable(a, match) {
				return true
			}
		}
		return false
	case CastExpr:
		return exprTreeMentionsTable(x.X, match)
	case CaseExpr:
		if x.Base != nil && exprTreeMentionsTable(x.Base, match) {
			return true
		}
		for _, w := range x.Whens {
			if exprTreeMentionsTable(w.When, match) || exprTreeMentionsTable(w.Then, match) {
				return true
			}
		}
		return x.Else != nil && exprTreeMentionsTable(x.Else, match)
	case SubqueryExpr:
		return selectTreeMentionsTable(x.Stmt, match)
	case ExistsExpr:
		return selectTreeMentionsTable(x.Stmt, match)
	default:
		return false
	}
}

// selectTreeMentionsColumn reports whether stmt (select list, WHERE, GROUP BY,
// HAVING, ORDER BY, and everything reachable) references a column named colName,
// qualified or not. "*" counts as every column except the rowid. Name-only, so an
// unrelated table's column also matches; callers only decline on it.
func selectTreeMentionsColumn(stmt *SelectStmt, colName string) bool {
	if stmt == nil {
		return false
	}
	for _, c := range stmt.Columns {
		if c.Star && !isRowidAliasName(colName) {
			return true
		}
		if exprTreeMentionsColumn(c.Expr, colName) {
			return true
		}
	}
	for _, it := range stmt.From {
		if it.Subquery != nil && selectTreeMentionsColumn(it.Subquery, colName) {
			return true
		}
		if exprTreeMentionsColumn(it.On, colName) {
			return true
		}
	}
	for _, cte := range stmt.CTEs {
		if selectTreeMentionsColumn(cte.Select, colName) {
			return true
		}
	}
	if exprTreeMentionsColumn(stmt.Where, colName) || exprTreeMentionsColumn(stmt.Having, colName) {
		return true
	}
	for _, e := range stmt.GroupBy {
		if exprTreeMentionsColumn(e, colName) {
			return true
		}
	}
	for _, ot := range stmt.OrderBy {
		if exprTreeMentionsColumn(ot.Expr, colName) {
			return true
		}
	}
	for _, arm := range stmt.Compound {
		if selectTreeMentionsColumn(arm.Stmt, colName) {
			return true
		}
	}
	return false
}

// exprTreeMentionsColumn is selectTreeMentionsColumn's expression-level
// counterpart, mirroring the same exhaustive Expr-type switch as
// exprTreeMentionsSchemaCatalog above (see its doc comment).
func exprTreeMentionsColumn(e Expr, colName string) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return false
	case ColumnExpr:
		return equalFoldName(x.Name, colName)
	case UnaryExpr:
		return exprTreeMentionsColumn(x.X, colName)
	case BinaryExpr:
		return exprTreeMentionsColumn(x.L, colName) || exprTreeMentionsColumn(x.R, colName)
	case IsNullExpr:
		return exprTreeMentionsColumn(x.X, colName)
	case InExpr:
		if selectTreeMentionsColumn(x.Sub, colName) {
			return true
		}
		if exprTreeMentionsColumn(x.X, colName) {
			return true
		}
		for _, a := range x.List {
			if exprTreeMentionsColumn(a, colName) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return exprTreeMentionsColumn(x.X, colName) || exprTreeMentionsColumn(x.Lo, colName) || exprTreeMentionsColumn(x.Hi, colName)
	case LikeExpr:
		return exprTreeMentionsColumn(x.X, colName) || exprTreeMentionsColumn(x.Pattern, colName) || exprTreeMentionsColumn(x.Escape, colName)
	case GlobExpr:
		return exprTreeMentionsColumn(x.X, colName) || exprTreeMentionsColumn(x.Pattern, colName)
	case CollateExpr:
		return exprTreeMentionsColumn(x.X, colName)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if exprTreeMentionsColumn(a, colName) {
				return true
			}
		}
		return false
	case CastExpr:
		return exprTreeMentionsColumn(x.X, colName)
	case CaseExpr:
		if x.Base != nil && exprTreeMentionsColumn(x.Base, colName) {
			return true
		}
		for _, w := range x.Whens {
			if exprTreeMentionsColumn(w.When, colName) || exprTreeMentionsColumn(w.Then, colName) {
				return true
			}
		}
		return x.Else != nil && exprTreeMentionsColumn(x.Else, colName)
	case SubqueryExpr:
		return selectTreeMentionsColumn(x.Stmt, colName)
	case ExistsExpr:
		return selectTreeMentionsColumn(x.Stmt, colName)
	default:
		return false
	}
}


// schemaCatalogQueryGuard declines a statement referencing sqlite_master /
// sqlite_schema that this engine cannot answer as C does. One rule remains: any
// reference while the schema holds a virtual table from a module with a private
// row store (schemaHasPrivateStoreVtabRow), since C's catalog would list its
// shadow tables. vtabPrivateStoreModules is empty today, so this never fires.
func (p *ReadOnlyPager) schemaCatalogQueryGuard(stmt *SelectStmt) error {
	// sqlite_temp_master falls under ONE rule, the private-store vtab one: the
	// TEMP database's catalog is its own page 1 now (temp_store.go), so every
	// other rule below is about the database this pager IS -- but an rtree in
	// TEMP leaves that catalog missing the three shadow-table rows C SQLite
	// lists there, exactly as it does in main.
	if !selectTreeMentionsTable(stmt, func(it FromItem) bool { return isMainSchemaCatalogName(it.Table) }) {
		if selectTreeMentionsTable(stmt, func(it FromItem) bool { return isTempSchemaCatalogName(it.Table) }) {
			if tp, found := p.attachedReaderNamed("temp"); found {
				return tp.privateStoreVtabCatalogDecline()
			}
		}
		return nil
	}
	// A catalog qualified by an attached database ("aux.sqlite_master") is checked
	// against that database's own schema by running this guard on its pager (one
	// level deep: attached readers have no attachments).
	if owner, ok := p.foreignCatalogOwner(stmt); ok {
		return owner.schemaCatalogQueryGuard(stmt)
	}
	// sqlite_schema.rowid used to be declined here on our format; each object
	// keeps C's rowid now (DB.nextSchemaSeq).
	return p.privateStoreVtabCatalogDecline()
}

// privateStoreVtabCatalogDecline is the one catalog rule that is about this
// database's CONTENT rather than about the statement: a module this engine
// backs with its own private row store instead of the module's real shadow
// tables (rtree) leaves the catalog's ROW SET -- not merely one column of it --
// different from C SQLite's, which lists one row per shadow table.
func (p *ReadOnlyPager) privateStoreVtabCatalogDecline() error {
	if mod, has, verr := p.schemaHasPrivateStoreVtabRow(); verr != nil {
		return verr
	} else if has {
		return fmt.Errorf("engine: unsupported: sqlite_master/sqlite_schema cannot be answered while the schema holds a %q virtual table: C SQLite's %s creates shadow tables (which appear as rows here) and this engine stores that table's rows in a b-tree of its own instead", mod, mod)
	}
	return nil
}

// foreignCatalogOwner returns the attached pager whose catalog is the only one
// stmt reads, or false when it reads this database's catalog too, names several
// attachments, or names a qualifier that is not an attachment
// (catalogItemScope decides "this database").
func (p *ReadOnlyPager) foreignCatalogOwner(stmt *SelectStmt) (*ReadOnlyPager, bool) {
	var qualifiers []string
	readsLocal := selectTreeMentionsTable(stmt, func(it FromItem) bool {
		if !isMainSchemaCatalogName(it.Table) && !isTempSchemaCatalogName(it.Table) {
			return false
		}
		if _, ok := catalogItemScope(it); ok {
			return true // a LOCAL catalog: stop the walk, this is not a foreign-only read
		}
		qualifiers = append(qualifiers, r33sFoldIdent(it.Schema))
		return false
	})
	if readsLocal || len(qualifiers) == 0 {
		return nil, false
	}
	for _, q := range qualifiers[1:] {
		if q != qualifiers[0] {
			return nil, false // two different attachments: no single owner to delegate to
		}
	}
	return p.attachedReaderNamed(qualifiers[0])
}

// limitOffsetPseudoRowRefs substitutes into e the one kind of reference a
// LIMIT/OFFSET may make: new.x / old.x in a trigger body. Ordinary correlated
// references and excluded.x are refused, and the result compiles standalone
// (foldLimitOffsetExpr) or declines.
//
// C resolves the clause with a zeroed NameContext (resolve.c:1900-1903):
//
//	/* Resolve the expressions in the LIMIT and OFFSET clauses. These
//	** are not allowed to refer to any names, so pass an empty NameContext.
//	*/
//	memset(&sNC, 0, sizeof(sNC));
//
// so "UPDATE t SET c = (SELECT b FROM u LIMIT t.a)" is "no such column: t.a"
// (resolving it outward here once wrote or deleted rows). In the pseudo-row
// block the two arms are gated differently:
//
//	resolve.c:525   if( pParse->pTriggerTab!=0 ){
//	resolve.c:537   }else if( op!=TK_DELETE && zTab && sqlite3StrICmp("new",zTab) == 0 ){
//	resolve.c:540   }else if( op!=TK_INSERT && zTab && sqlite3StrICmp("old",zTab)==0 ){
//	resolve.c:547   if( (pNC->ncFlags & NC_UUpsert)!=0 && zTab!=0 ){
//	resolve.c:549     if( pUpsert && sqlite3StrICmp("excluded",zTab)==0 ){
//
// The trigger arm reads a parse-level field the memset cannot clear, so new./old.
// resolve; the upsert arm reads a NameContext flag the memset clears, so
// "... DO UPDATE SET c=(SELECT 7 LIMIT excluded.c)" is "no such column" and must
// not overwrite the row.
//
// Pseudo-row values are substituted as literals (as rewriteSelectOuterRefs does).
// The local set passed below is every reachable scope except the trigger
// pseudo-rows, a whitelist: a scope must be unqualifiedHidden and named
// "new"/"old" (a real table named new is not substituted). Unqualified names are
// never substituted ("LIMIT a" is "no such column").
func limitOffsetPseudoRowRefs(e Expr, outer *evalCtx) Expr {
	if e == nil || outer == nil {
		return e
	}
	local := map[string]bool{}
	for c := outer; c != nil; c = c.outer {
		for _, ts := range c.tables {
			n := r33sFoldIdent(ts.name)
			if n == "" || (ts.unqualifiedHidden && (n == "new" || n == "old")) {
				continue
			}
			local[n] = true
		}
	}
	changed := false
	syn := rewriteSelectOuterRefs(
		&SelectStmt{Columns: []SelectColumn{{Expr: e}}},
		outer, local, &changed, false)
	if !changed || syn == nil || len(syn.Columns) != 1 {
		return e
	}
	return syn.Columns[0].Expr
}

// execSelect executes a parsed SelectStmt; it is the reentrant entry point,
// called with outer == nil for a top-level statement and with the enclosing
// row's evalCtx for a subquery that needs it.
//
// cacheKey, when non-empty, is the exact SQL text of a TOP-LEVEL read (QueryArgs
// passes it; every reentrant subquery / write-path caller passes "") whose
// compiled Program tryVDBEScan may memoize on the pager for reuse -- see
// ReadOnlyPager.planCache. It is threaded rather than derived from outer==nil so
// only a genuine top-level QueryArgs read ever caches, never an uncorrelated
// subquery that also happens to run with outer==nil.
func (p *ReadOnlyPager) execSelect(stmt *SelectStmt, outer *evalCtx, params []Value, cacheKey string) (cols []string, rows [][]Value, err error) {
	// Validate every database qualifier in the whole statement tree once, at the top
	// level (outer == nil), before any dispatch, so no path resolves an invalid
	// qualifier as its two-part form (validateSelectSchemaQualifiers).
	if outer == nil {
		if err := p.validateSelectSchemaQualifiers(stmt); err != nil {
			return nil, nil, err
		}
	}
	if err := p.schemaCatalogQueryGuard(stmt); err != nil {
		return nil, nil, err
	}
	if err := p.fts5SegmentShadowQueryGuard(stmt); err != nil {
		return nil, nil, err
	}
	// "FROM <fts5 table>('query')" is fts5's table-valued call form; rewrite it
	// into the plain scan + MATCH it means (fts5_tablefunc.go) before anything
	// below sees it. Returns stmt unchanged in every other case.
	stmt = p.fts5RewriteTableFunc(stmt)
	// "<fts5 table> = <expr>" is a full-text match spelled with "="
	// (fts5_main.c:653-655); a no-op for every statement without one.
	stmt = p.fts5RewriteEqMatch(stmt)
	// "JOIN <fts5 table> USING (<that name>)" is the same match through the
	// join's own equality; a no-op for every statement without one.
	stmt = p.fts5RewriteUsingMatch(stmt)
	// "MATCH '*reads'" / "MATCH '*id'" are fts5's internal-parameter requests,
	// not full-text queries (fts5_main.c:1515-1520). Both report the answering
	// build's own instrumentation; see fts5_special_query.go. Runs AFTER the
	// rewrites above so the table-valued and "=" spellings are already a MATCH.
	if err := p.fts5SpecialQueryGuard(stmt); err != nil {
		return nil, nil, err
	}
	// Push this statement's WITH CTEs onto p.cteScopes for the length of this call,
	// so everything reached while it runs (its FROM, compound arms, nested
	// subqueries) sees them; see ReadOnlyPager.cteScopes. Done before the VDBE
	// dispatch so resolveJoinSources can consult it.
	if len(stmt.CTEs) > 0 {
		pop := p.pushCTEScope(stmt.CTEs)
		defer pop()
	}

	// Resolve LimitParam/OffsetParam into a concrete Limit/Offset once, before
	// dispatch, on a shallow copy (the AST may be reentered). Resolution compiles the
	// expression (foldLimitOffsetExpr), as C codes it into registers
	// (computeLimitRegisters, select.c:2517-2556). Only trigger pseudo-rows from
	// outer are reachable (limitOffsetPseudoRowRefs).
	//
	// A folded bound parameter must not be plan-cached: the cache is keyed on text
	// and the folded value would be reused ("LIMIT ?" stepped with 1, 6, 0 answered
	// one row every time). So cacheKey is cleared. C never has this problem because
	// it folds only integer literals and codes everything else into the counter
	// register; emitLimitOffsetReg already does that for some bodies, and extending
	// it to the rest (window, GROUP BY with HAVING, compound) would let these back
	// into the cache.
	if stmt.LimitParam != nil || stmt.OffsetParam != nil {
		if exprHasParam(stmt.LimitParam) || exprHasParam(stmt.OffsetParam) {
			cacheKey = ""
		}
		resolved := *stmt
		if stmt.LimitParam != nil {
			n, ferr := foldLimitOffsetExpr(p, limitOffsetPseudoRowRefs(stmt.LimitParam, outer), params)
			if ferr != nil {
				return nil, nil, ferr
			}
			resolved.Limit = &n
			resolved.LimitParam = nil
		}
		if stmt.OffsetParam != nil {
			n, ferr := foldLimitOffsetExpr(p, limitOffsetPseudoRowRefs(stmt.OffsetParam, outer), params)
			if ferr != nil {
				return nil, nil, ferr
			}
			resolved.Offset = &n
			resolved.OffsetParam = nil
		}
		stmt = &resolved
	}

	// A RECURSIVE CTE consumed by nothing but a LIMIT stops early: the bound is
	// published by the COMPILER, onto the reference that consumes it -- see
	// compileScanAttempt (vdbe_scan.go) and cteRef.outerRowCapPlus1
	// (vdbe_join_codegen.go). It used to be published here instead, which
	// reached only a top-level statement: the identical shape nested inside a
	// derived table or a scalar subquery is compiled into a sub-Program that
	// execSelect never sees, and declined.

	// A compound SELECT (UNION/UNION ALL/INTERSECT/EXCEPT) is compiled whole
	// (tryVDBECompound). This must be checked before the FROM-less dispatch
	// below, since a compound's first arm may itself be FROM-less ("SELECT 1
	// UNION SELECT 2").
	if len(stmt.Compound) > 0 {
		// The flattener runs per SELECT-CORE, so a derived table / CTE / view
		// read from inside a compound ARM is merged into that arm exactly as one
		// read from a lone SELECT is -- and the dispatch below never reaches
		// r37cFlattenTransparent's own call site further down. See
		// r38cFlattenCompoundArms (flatten_limit_r35c.go); a no-op for every
		// statement whose arms hold no such FROM item.
		stmt = p.r38cFlattenCompoundArms(stmt)
		// A compound the compiler cannot handle is an error. outer is threaded, since
		// this is reentered with a live outer for a compound subquery body (e.g. the
		// write path's rowEvalCtx); dropping it left "HAVING EXISTS (SELECT a UNION
		// SELECT 123)" declining where the qualified form answered. See tryVDBECompound.
		cols, rows, handled, verr := p.tryVDBECompound(stmt, outer, params)
		if handled {
			return cols, rows, verr
		}
		// A genuine C-SQLite error is the STATEMENT's error, not evidence the
		// compiler is missing a shape: errVDBESemantic's own contract says it
		// "must be surfaced to the caller as the statement's real error", and
		// wrapping it in "not compilable to bytecode ... (no fallback)" both
		// buried the real diagnosis and put text in front of it that real
		// SQLite has no counterpart for. See subquery_validate.go.
		if errors.Is(verr, errVDBESemantic) {
		return nil, nil, verr
		}
		if verr != nil {
			return nil, nil, fmt.Errorf("engine: VDBE-only: compound SELECT not compilable to bytecode (no fallback): %w", verr)
		}
		return nil, nil, fmt.Errorf("engine: VDBE-only: compound SELECT not compilable to bytecode (no fallback)")
	}
	if len(stmt.From) == 0 {
		return p.execNoFrom(stmt, outer, params)
	}

	// C flattens a FROM subquery into its parent before planning, so the parent's
	// WHERE can drive an index and rows arrive in index order; materializing it
	// instead gives a different row order. The one case where flattening changes
	// only the plan ("SELECT * FROM <ordinary table>") is rewritten here
	// (flatten_limit_r35c.go); it needs the pager to recognize an ordinary table.
	stmt = p.r37cFlattenTransparent(stmt)
	// ...and a body that is a PROJECTION of one ordinary table, which needs
	// flattenSubquery's substitution of every parent reference (substExpr,
	// select.c:3797). See flatten_projection.go.
	stmt = p.r41FlattenProjection(stmt)
	if len(stmt.Compound) > 0 {
		// A UNION ALL body flattened the parent into a compound
		// (r41FlattenCompound), which the compound dispatch above runs. Its WITH
		// is already in scope, pushed by this call.
		cp := *stmt
		cp.CTEs = nil
		return p.execSelect(&cp, outer, params, cacheKey)
	}
	// Every FROM-clause subquery left unflattened then gets the parent's
	// single-item WHERE terms copied into it (pushDownWhereTerms,
	// select.c:5134). See pushdown_where.go.
	stmt = p.r41PushDown(stmt)

	// VDBE-ONLY: the single-/multi-table scan compiler is the sole executor. A
	// statement it cannot compile is a HARD ERROR. The error names
	// the gap so the conformance gate shows exactly which shapes the scan
	// compiler still owes (see vdbe_scan.go's errVDBEUnsupported reasons).
	cols, rows, handled, verr := p.tryVDBEScan(stmt, outer, params, cacheKey)
	if handled {
		return cols, rows, verr
	}
	// A genuine C-SQLite error is the STATEMENT's error, not evidence the
	// compiler is missing a shape: errVDBESemantic's own contract says it
	// "must be surfaced to the caller as the statement's real error", and
	// wrapping it in "not compilable to bytecode ... (no fallback)" both
	// buried the real diagnosis and put text in front of it that real
	// SQLite has no counterpart for. See subquery_validate.go.
	if errors.Is(verr, errVDBESemantic) {
	return nil, nil, verr
	}
	if verr != nil {
		return nil, nil, fmt.Errorf("engine: VDBE-only: %w (no fallback)", verr)
	}
	return nil, nil, fmt.Errorf("engine: VDBE-only: statement not compilable to bytecode (no fallback)")
}

// normalizeRow turns a raw decoded record into the values SQL sees: it puts the
// rowid into the INTEGER PRIMARY KEY column (stored as NULL), and restores REAL
// for a REAL-affinity column stored as an integer (datatype3.html; typeof()
// reports "real"). It copies vals only when a correction applies.
func normalizeRow(tblName string, cols []columnInfo, ipkIndex int, rowid uint64, vals []Value) []Value {
	if hasGeneratedCols(cols) {
		out := expandStoredRow(cols, vals)
		if &out[0] == &vals[0] { // no widening happened; must not mutate the caller's slice
			out = append([]Value(nil), vals...)
		}
		normalizeRowInto(out, cols, ipkIndex, rowid)
		// A generated expression failing (a bad schema this engine accepted)
		// leaves the computed columns NULL rather than failing the read.
		_ = computeGeneratedInto(tblName, cols, out)
		return out
	}
	// A record narrower than the column list (a row written before an
	// ALTER TABLE ADD COLUMN) is widened first -- every loop below indexes by
	// column position. See padStoredRow.
	if len(vals) < len(cols) {
		return normalizeRowPadded(padStoredRow(cols, vals), cols, ipkIndex, rowid)
	}
	needsCopy := ipkIndex >= 0 && vals[ipkIndex].Typ == Null
	if !needsCopy {
		for i, c := range cols {
			if c.Aff == affReal && vals[i].Typ == Int {
				needsCopy = true
				break
			}
		}
	}
	if !needsCopy {
		return vals
	}
	out := append([]Value(nil), vals...)
	normalizeRowInto(out, cols, ipkIndex, rowid)
	return out
}

// normalizeRowInPlace is normalizeRow for a caller that owns vals (the cursor
// path, whose vals is a fresh decodeRecord slice): it applies the same rowid
// alias and REAL fix-ups in place, saving one allocation per scanned row. It only
// rewrites Value structs, never the page bytes TEXT/BLOB values point into.
// Callers passing a shared slice (the write path's row store) must use
// normalizeRow. hasGen must be hasGeneratedCols(cols), passed in by the per-row
// caller that already knows it.
func normalizeRowInPlace(tblName string, cols []columnInfo, ipkIndex int, rowid uint64, vals []Value, hasGen bool) []Value {
	if hasGen {
		// Widening cannot happen in place; expandStoredRow allocates only
		// when the table actually has a VIRTUAL column.
		vals = expandStoredRow(cols, vals)
		normalizeRowInto(vals, cols, ipkIndex, rowid)
		_ = computeGeneratedInto(tblName, cols, vals)
		return vals
	}
	// See normalizeRow's identical widening: padStoredRow already allocates a
	// fresh slice, so applying the fix-ups to it stays in-place-safe.
	vals = padStoredRow(cols, vals)
	normalizeRowInto(vals, cols, ipkIndex, rowid)
	return vals
}

// normalizeRowPadded applies the ordinary fix-ups to an already-widened row
// that padStoredRow freshly allocated, so no second defensive copy is made.
func normalizeRowPadded(row []Value, cols []columnInfo, ipkIndex int, rowid uint64) []Value {
	normalizeRowInto(row, cols, ipkIndex, rowid)
	return row
}

// normalizeRowInto applies the IPK-rowid-alias substitution and REAL-affinity
// int->float fix-up to row in place -- the shared body of normalizeRow (over a
// fresh copy) and normalizeRowInPlace (over the caller's own slice).
func normalizeRowInto(row []Value, cols []columnInfo, ipkIndex int, rowid uint64) {
	if ipkIndex >= 0 && row[ipkIndex].Typ == Null {
		row[ipkIndex] = Value{Typ: Int, I: int64(rowid)}
	}
	for i, c := range cols {
		if c.Aff == affReal && row[i].Typ == Int {
			row[i] = Value{Typ: Float, F: float64(row[i].I)}
		}
	}
}

// limitOffsetRange computes the [start, end) slice bounds LIMIT/OFFSET
// select out of a length-n result: offset is clamped into [0, n] (a negative
// offset behaves as 0; one past the end yields no rows) and a non-negative
// limit caps the count from there; a nil or negative limit means unlimited,
// matching SQLite's own "LIMIT -1" convention.
func limitOffsetRange(n int, limit, offset *int64) (start, end int) {
	start, end = 0, n
	if offset != nil {
		o := *offset
		switch {
		case o < 0:
			o = 0
		case int(o) > n:
			o = int64(n)
		}
		start = int(o)
	}
	if limit != nil && *limit >= 0 {
		if l := start + int(*limit); l < end {
			end = l
		}
	}
	return start, end
}

// validateColumnRefs walks e for a ColumnExpr that does not resolve against
// ctx and returns resolveColumn's error ("no such column", "no such table",
// "ambiguous column name"). A plan-time pass, because a per-row read only finds a
// bad reference when some row reaches it, while C validates at prepare time.
// ctx must carry the same outer chain execution uses, so correlated references
// resolve. Subquery bodies are not descended into; they are validated when
// compiled against their own context.
func validateColumnRefs(e Expr, ctx *evalCtx) error {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return nil
	case ColumnExpr:
		// The fts5 "rank" hidden column resolves to the row's bm25() score
		// when this query carries an fts5 auxiliary context (see fts5AuxState /
		// columnRowValue's matching shortcut, row_scope.go); it is not a real column, so
		// skip ordinary column-existence validation for it.
		if ctx.fts5Aux != nil && x.Schema == "" && equalFoldName(x.Name, "rank") && ctx.fts5Aux.matchesRankQualifier(x.Qualifier) {
			return nil
		}
		// Validate a three-part reference's database qualifier here, the one plan-time
		// point holding the pager (the row read ignores it). A non-resolving qualifier,
		// or no pager to check with, declines.
		if x.Schema != "" && !(ctx.checkConstraintTable != "" && equalFoldName(x.Qualifier, ctx.checkConstraintTable)) {
			if ctx.pager == nil {
				return fmt.Errorf("engine: no such table: %s.%s", x.Schema, x.Qualifier)
			}
			ok, serr := ctx.pager.qualifierResolves(x.Schema)
			if serr != nil {
				return serr
			}
			if !ok {
				return fmt.Errorf("engine: no such table: %s.%s", x.Schema, x.Qualifier)
			}
		}
		_, _, _, _, err := resolveColumn(ctx, x.Qualifier, x.Name)
		if err != nil && x.FallbackLiteral != nil && unqualifiedColumnNotFoundErr(err, x.Name) {
			return nil
		}
		return err
	case FuncExpr:
		if ctx.fts5Aux != nil && fts5AuxFuncName(x.Name) {
			// The first argument is the fts5 table name (not a column); only
			// the trailing marker/weight arguments are ordinary expressions.
			if len(x.Args) >= 1 {
				return validateColumnRefsAll(ctx, x.Args[1:]...)
			}
			return nil
		}
		return validateColumnRefsAll(ctx, x.walkArgs()...)
	case UnaryExpr:
		return validateColumnRefs(x.X, ctx)
	case BinaryExpr:
		return validateColumnRefsAll(ctx, x.L, x.R)
	case IsNullExpr:
		return validateColumnRefs(x.X, ctx)
	case InExpr:
		if err := validateColumnRefs(x.X, ctx); err != nil {
			return err
		}
		return validateColumnRefsAll(ctx, x.List...)
	case BetweenExpr:
		return validateColumnRefsAll(ctx, x.X, x.Lo, x.Hi)
	case LikeExpr:
		return validateColumnRefsAll(ctx, x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return validateColumnRefsAll(ctx, x.X, x.Pattern)
	case CollateExpr:
		return validateColumnRefs(x.X, ctx)
	case CastExpr:
		return validateColumnRefs(x.X, ctx)
	case CaseExpr:
		if x.Base != nil {
			if err := validateColumnRefs(x.Base, ctx); err != nil {
				return err
			}
		}
		for _, w := range x.Whens {
			if err := validateColumnRefs(w.When, ctx); err != nil {
				return err
			}
			if err := validateColumnRefs(w.Then, ctx); err != nil {
				return err
			}
		}
		if x.Else != nil {
			return validateColumnRefs(x.Else, ctx)
		}
		return nil
	case RaiseExpr:
		// RAISE(IGNORE) carries no message; RAISE(ABORT|FAIL|ROLLBACK, msg)'s
		// message expression is compiled by C SQLite at prepare time, so a
		// bad column reference in it surfaces eagerly (even for a zero-row
		// firing) -- validate it against the same OLD/NEW scope.
		return validateColumnRefs(x.Msg, ctx)
	case SubqueryExpr, ExistsExpr:
		return nil
	default:
		return nil
	}
}

func validateColumnRefsAll(ctx *evalCtx, es ...Expr) error {
	for _, e := range es {
		if err := validateColumnRefs(e, ctx); err != nil {
			return err
		}
	}
	return nil
}

// orderByOrdinal reports whether e is an ORDER BY term C treats as a 1-based
// result-column ordinal, and its signed value: an integer literal, optionally
// with one leading unary +/- ("ORDER BY +2" sorts by column 2, "ORDER BY -1" is
// "1st ORDER BY term out of range"). Anything else is an ordinary expression.
//
// The unnegated magnitude must fit int32: sqlite3ExprIsInteger (expr.c:2899)
// only reads a literal with EP_IntValue, set only when the token fits 32 bits
// (expr.c:924). So "ORDER BY -2147483648" sorts by a constant, while
// "ORDER BY -2147483647" is out of range.
func orderByOrdinal(e Expr) (n int64, ok bool) {
	switch x := e.(type) {
	case LiteralExpr:
		if x.Val.Typ == Int && x.Val.I >= 0 && x.Val.I <= math.MaxInt32 {
			return x.Val.I, true
		}
	case UnaryExpr:
		if lit, isLit := x.X.(LiteralExpr); isLit && lit.Val.Typ == Int &&
			lit.Val.I >= 0 && lit.Val.I <= math.MaxInt32 {
			switch x.Op {
			case "+":
				return lit.Val.I, true
			case "-":
				return -lit.Val.I, true
			}
		}
	}
	return 0, false
}

// stripOrderCollate removes every top-level COLLATE wrapper from e, so the
// ordinal and alias special forms still match ("ORDER BY 1 COLLATE NOCASE",
// even stacked COLLATEs). The sort collation is still taken from the original
// term (its outermost COLLATE).
func stripOrderCollate(e Expr) Expr {
	for {
		c, ok := e.(CollateExpr)
		if !ok {
			return e
		}
		e = c.X
	}
}

// r32mTrimDupSuffix cuts a trailing ":<digits>" off a column name, as
// sqlite3ColumnsFromExprList does before appending its counter
// (select.c:2300-2306):
//
//	for(j=nName-1; j>0 && sqlite3Isdigit(zName[j]); j--){}
//	if( zName[j]==':' ) nName = j;
//
// The j>0 guard keeps ":1"'s empty prefix, so it becomes ":2".
func r32mTrimDupSuffix(name string) string {
	if name == "" {
		return name
	}
	j := len(name) - 1
	for j > 0 && name[j] >= '0' && name[j] <= '9' {
		j--
	}
	if name[j] == ':' {
		return name[:j]
	}
	return name
}

// trueFalseColumnNames renames a derived column called "true" or "false" (any
// case) to "column<N>", its 1-based position, as sqlite3ColumnsFromExprList
// does before it de-duplicates (select.c:2287). So such a column of a derived
// table, view or CTE can never be named, and "x IS true" over one stays the
// truth test: lookupName finds nothing (resolve.c:747).
func trueFalseColumnNames(names []string) []string {
	out := names
	for i, n := range names {
		if !equalFoldName(n, "true") && !equalFoldName(n, "false") {
			continue
		}
		if &out[0] == &names[0] {
			out = append([]string(nil), names...)
		}
		out[i] = "column" + strconv.Itoa(i+1)
	}
	return out
}

// r32mUniqueColumnNames renames repeated result-column names as C does when it
// builds the table for a derived table, CTE or view (sqlite3ColumnsFromExprList):
//
//	cnt = 0;
//	while( zName && (pCollide = sqlite3HashFind(&ht, zName))!=0 ){
//	  nName = sqlite3Strlen30(zName);
//	  if( nName>0 ){
//	    for(j=nName-1; j>0 && sqlite3Isdigit(zName[j]); j--){}
//	    if( zName[j]==':' ) nName = j;
//	  }
//	  zName = sqlite3MPrintf(db, "%.*z:%u", nName, zName, ++cnt);
//	  if( cnt>3 ){ sqlite3_randomness(sizeof(cnt), &cnt); }
//	}
//
// Collisions are case-insensitive, the suffix goes on the repeat's own
// spelling ("a, A" gives a, A:1); an existing ":<digits>" tail is replaced; the
// counter restarts per column and skips taken names. Past the fourth bump C uses
// a random suffix, so ok is false there and callers keep the duplicate-name
// decline. A top-level statement's own columns are never renamed.
func r32mUniqueColumnNames(names []string) ([]string, bool) {
	names = trueFalseColumnNames(names)
	// Nothing repeats in the overwhelming majority of subqueries; don't
	// allocate a second slice for them.
	seen := make(map[string]bool, len(names))
	repeats := false
	for _, n := range names {
		ln := r33sFoldIdent(n)
		if seen[ln] {
			repeats = true
			break
		}
		seen[ln] = true
	}
	if !repeats {
		return names, true
	}
	out := make([]string, len(names))
	taken := make(map[string]bool, len(names))
	for i, n := range names {
		z := n
		cnt := 0
		for taken[r33sFoldIdent(z)] {
			// Cut back over a trailing ":<digits>" so the counter REPLACES an
			// existing suffix rather than stacking onto it. The j>0 guard is
			// SQLite's own: index 0 is never consumed as a digit, so ":1"
			// keeps an empty prefix and becomes ":2".
			keep := len(z)
			if keep > 0 {
				j := keep - 1
				for j > 0 && z[j] >= '0' && z[j] <= '9' {
					j--
				}
				if z[j] == ':' {
					keep = j
				}
			}
			cnt++
			if cnt > 4 {
				// SQLite randomized its counter after forming ":4"; a name
				// still colliding at that point gets an unreproducible one.
				return names, false
			}
			z = z[:keep] + ":" + strconv.Itoa(cnt)
		}
		taken[r33sFoldIdent(z)] = true
		out[i] = z
	}
	return out, true
}

// r32mFinishUniqueColumnNames is r32mUniqueColumnNames without giving up: past
// the fourth repeat, where C switches to random suffixes, it keeps counting
// (name:5, ...). Used only by CREATE TABLE AS SELECT (createTableAsSelect), whose
// column names are never compared with C's and must still be unique.
func r32mFinishUniqueColumnNames(names []string) []string {
	names = trueFalseColumnNames(names)
	out := make([]string, len(names))
	taken := make(map[string]bool, len(names))
	for i, n := range names {
		z := n
		cnt := 0
		for taken[r33sFoldIdent(z)] {
			keep := len(z)
			if keep > 0 {
				j := keep - 1
				for j > 0 && z[j] >= '0' && z[j] <= '9' {
					j--
				}
				if z[j] == ':' {
					keep = j
				}
			}
			cnt++
			z = z[:keep] + ":" + strconv.Itoa(cnt)
		}
		taken[r33sFoldIdent(z)] = true
		out[i] = z
	}
	return out
}

// errIfDuplicateOutputNames declines a row-mode SELECT whose result-column names
// cannot match C's: duplicate names (case-insensitive) where one FROM item's own
// column list repeats the name, i.e. a subquery C would ":N"-rename
// ("SELECT * FROM (SELECT * FROM t2 JOIN t3 ON t2.c=t3.c)" gives c,d,c:1,e).
// Duplicates across FROM items stay plain in C too and are fine. Only applies
// with a parenthesized FROM (stmt.FromParenthesized).
//
// A nested parenthesized join (stmt.FromNestedParenJoin) is never declined: C's
// ":N" names there are not even deterministic past a couple of levels, and
// SQLite calls such names unspecified, so the query runs with plain names (the
// harness relaxes them, tclRelaxUnspecifiedColNames). Single-level duplicates
// are declined because names are compared.
func errIfDuplicateOutputNames(stmt *SelectStmt, outCols []outputColumn, scopes []tableScope) error {
	if !stmt.FromParenthesized {
		return nil
	}
	// A duplicate inside a non-leading parenthesized join group is ":N"-renamed by C
	// (it becomes an SF_NestedFrom subquery). Checked before the per-scope gate
	// below, since the collision is across the group's members:
	//
	//	SELECT * FROM (t JOIN w ON t.a=w.a)        -> a b a   d   (leading)
	//	SELECT * FROM u JOIN (t JOIN w ON t.a=w.a) -> a c a b a:1 d
	//
	// See FromItem.NestedGroupID.
	if r45NameNestedGroupDuplicates(stmt, outCols, scopes) {
		return nil
	}
	// C renames inside a subquery's own column list (sqlite3ColumnsFromExprList),
	// never across the outer query's FROM items:
	//
	//	SELECT * FROM (t2) AS x JOIN t3 ON x.c=t3.c    -> c d c   e
	//	SELECT * FROM (t2 JOIN t3 ON t2.c=t3.c)        -> c d c   e
	//	SELECT * FROM (SELECT * FROM t2 JOIN t3 ON t2.c=t3.c)
	//	                                               -> c d c:1 e
	//	SELECT * FROM (SELECT 1 AS c, 2 AS c)          -> c   c:1
	//	SELECT * FROM (SELECT * FROM t2 JOIN t3 ON t2.c=t3.c) JOIN t4 ON t4.c=1
	//	                                               -> c d c:1 e c f
	//
	// So decline only when some FROM item's own column list is duplicated.
	if !anyScopeHasDuplicateColumnName(scopes) {
		return nil
	}
	if stmt.FromNestedParenJoin {
		return nil
	}
	seen := make(map[string]bool, len(outCols))
	for _, oc := range outCols {
		lname := r33sFoldIdent(oc.name)
		if seen[lname] {
			return fmt.Errorf("engine: unsupported: duplicate result-column name %q from a parenthesized/derived FROM (SQLite's ':N' disambiguation is not reproduced)", oc.name)
		}
		seen[lname] = true
	}
	return nil
}

// r45NameNestedGroupDuplicates renames outCols in place with C's ":N" spelling
// for each non-leading join group's duplicates, and reports whether it did the
// whole job. It runs only for a bare "*" with no USING/NATURAL and no derived
// items, when the scopes' column counts sum to len(outCols); otherwise false.
func r45NameNestedGroupDuplicates(stmt *SelectStmt, outCols []outputColumn, scopes []tableScope) bool {
	if len(stmt.Columns) != 1 || !stmt.Columns[0].Star || stmt.Columns[0].StarQualifier != "" {
		return false
	}
	if len(scopes) != len(stmt.From) || len(scopes) == 0 {
		return false
	}
	total, anyGroup := 0, false
	for i, it := range stmt.From {
		if it.Natural || len(it.Using) != 0 || it.Subquery != nil || it.TableFunc {
			return false
		}
		if it.NestedGroupID != 0 {
			anyGroup = true
		}
		total += len(scopes[i].cols)
	}
	if !anyGroup || total != len(outCols) {
		return false
	}
	// One counter per group, exactly as sqlite3ColumnsFromExprList keeps one
	// per subquery it builds a column list for.
	type key struct {
		group int
		name  string
	}
	seenIn := map[key]bool{}
	count := map[int]uint{}
	at := 0
	for i := range stmt.From {
		gid := stmt.From[i].NestedGroupID
		for range scopes[i].cols {
			oc := &outCols[at]
			at++
			if gid == 0 {
				continue
			}
			k := key{gid, r33sFoldIdent(oc.name)}
			if !seenIn[k] {
				seenIn[k] = true
				continue
			}
			count[gid]++
			// r32mTrimDupSuffix, not a plain append: sqlite3ColumnsFromExprList
			// cuts back over an existing ":<digits>" before adding its own
			// counter (select.c:2300-2306), so a name that arrives ALREADY
			// suffixed gets ":2", never ":1:2". A nested group is where that
			// arrives: its inner pass already named the sub-group's own
			// duplicate ("a:1"), and this pass then renames it again --
			// "SELECT * FROM u JOIN (t JOIN (w JOIN y ON w.a=y.a) ON t.a=w.a)"
			// was a, b, a:1, d, a:1:2, e where 3.53.3 says a:2.
			oc.name = fmt.Sprintf("%s:%d", r32mTrimDupSuffix(oc.name), count[gid])
			seenIn[key{gid, r33sFoldIdent(oc.name)}] = true
		}
	}
	// The rename only settles the GROUP's own duplicates; a collision with an
	// item outside it is C SQLite's too and is left alone, so re-check that
	// nothing is duplicated within any single group before claiming the job.
	perGroup := map[key]bool{}
	at = 0
	for i := range stmt.From {
		gid := stmt.From[i].NestedGroupID
		for range scopes[i].cols {
			k := key{gid, r33sFoldIdent(outCols[at].name)}
			at++
			if gid == 0 {
				continue
			}
			if perGroup[k] {
				return false
			}
			perGroup[k] = true
		}
	}
	return true
}

// anyScopeHasDuplicateColumnName reports whether any single FROM item's own
// column list repeats a (case-insensitive) name -- the one situation in which
// C SQLite ":N"-renames, and therefore the one in which this engine's plain
// names are wrong rather than merely duplicated. See errIfDuplicateOutputNames.
func anyScopeHasDuplicateColumnName(scopes []tableScope) bool {
	for _, sc := range scopes {
		if len(sc.cols) < 2 {
			continue
		}
		seen := make(map[string]bool, len(sc.cols))
		for _, ci := range sc.cols {
			lname := r33sFoldIdent(ci.Name)
			if seen[lname] {
				return true
			}
			seen[lname] = true
		}
	}
	return false
}

// expandSelectList turns a select list (possibly with "*") into named output
// columns as C names them under the given colNameMode (full_column_names /
// short_column_names; defaultColNameMode for internal callers). An alias always
// wins; a column reference is its declared name, "<realtable>.<column>" with
// full_column_names, or its source text with both pragmas off; a "*" column is
// its declared name, or "<source>.<column>" with full on and short off; an
// expression keeps its source text.
func expandSelectList(sel []SelectColumn, scopes []tableScope, mode colNameMode) ([]outputColumn, error) {
	// longNames is C SQLite's own gate for QUALIFYING a "*"-expanded
	// column ("<source>.<col>"): full_column_names ON *and* short_column_names
	// OFF -- distinct from an ordinary column reference, which qualifies under
	// full_column_names ON regardless of short_column_names (verified directly).
	longNames := mode.full && !mode.short
	// starName computes one "*"-expanded column's reported name under mode.
	// Under longNames it is "<ts.name>.<col>" (the FROM item's ALIAS, or its
	// table name when unaliased -- ts.name, NOT ts.tableName); otherwise the
	// bare declared column name (the default, unchanged). An unnamed source
	// (ts.name == "", an unaliased derived table) under longNames is declined:
	// SQLite names it "(subquery-N).col", a synthetic counter this engine does
	// not reproduce.
	starName := func(ts tableScope, colName string) (string, error) {
		if !longNames {
			return colName, nil
		}
		if ts.name == "" {
			return "", fmt.Errorf("engine: unsupported: PRAGMA full_column_names naming of '*' over an unnamed subquery (SQLite's 'subquery-N.%s' auto-name is not reproduced)", colName)
		}
		return ts.name + "." + colName, nil
	}
	var out []outputColumn
	for _, sc := range sel {
		if sc.Star {
			if sc.StarQualifier != "" {
				// "t1.*" expands only the FROM item an ordinary "t1.col" would resolve to, in
				// this statement's own scopes only: "SELECT (SELECT t1.* FROM t2) FROM t1" is
				// "no such table: t1".
				ts, ok := findTableScope(scopes, sc.StarQualifier)
				if !ok {
					return nil, fmt.Errorf("engine: no such table: %s", sc.StarQualifier)
				}
				// A qualified star matching several FROM items (an unaliased self-join) expands
				// all of them in C ("SELECT tt.*" over "tt JOIN tt USING(a,b,c)" gives six
				// columns); findTableScope takes the first only, so decline.
				if n := countScopesNamed(scopes, ts.name); n > 1 {
					return nil, fmt.Errorf("engine: unsupported: %q.* names %d FROM items (C SQLite expands every one of them)", ts.name, n)
				}
				for ci, c := range ts.cols {
					// A virtual-table HIDDEN column (columnInfo.Hidden) is
					// omitted from "*" expansion, exactly like SQLite (a
					// generate_series' start/stop/step never appear in
					// "SELECT t.* FROM generate_series(...) t").
					if c.Hidden {
						continue
					}
					// A qualified star coalesces RIGHT/FULL USING columns like a bare one: C's
					// expander rewrites a "*"-expanded USING column into coalesce(...) but leaves
					// hand-written references alone:
					//
					//	SELECT t1.a  ...  -> NULL   raw storage
					//	SELECT t1.*  ...  -> 3      the COALESCE'd column
					//
					// Except inside a non-leading join group, which C materializes as a subquery, so
					// the star reads the raw copy (FromItem.NestedNonLeading):
					//
					//	SELECT t1.* FROM (t1 RIGHT JOIN t2 USING(a))
					//	  -> 3     the COALESCE'd column   (group is leading)
					//	SELECT t1.* FROM t4, (t1 RIGHT JOIN t2 USING(a))
					//	  -> NULL  t1's raw storage        (group is a subquery)
					if !ts.nestedNonLeading && ts.coalesceFallback != nil {
						if _, hasFallback := ts.coalesceFallback[r33sFoldIdent(c.Name)]; hasFallback {
							if longNames {
								return nil, fmt.Errorf("engine: unsupported: PRAGMA full_column_names naming of a USING/NATURAL-coalesced '*' column %q", c.Name)
							}
							out = append(out, outputColumn{expr: ColumnExpr{Name: c.Name}, name: c.Name})
							continue
						}
					}
					nm, nerr := starName(*ts, c.Name)
					if nerr != nil {
						return nil, nerr
					}
					// A rebuilt group's member is named from the group's rebuilt column list
					// (tableScope.nestedColNames), ":N" and all ("SELECT t.* FROM u JOIN (t JOIN w
					// USING(a))" reports "a:1 b"). Default naming mode only.
					if !longNames && ts.nestedColNames != nil && ci < len(ts.nestedColNames) {
						nm = ts.nestedColNames[ci]
					}
					out = append(out, outputColumn{expr: ColumnExpr{Qualifier: ts.name, Name: c.Name}, name: nm})
				}
				continue
			}
			for si := range scopes {
				ts := scopes[si]
				for ci, c := range ts.cols {
					// A virtual-table HIDDEN column (columnInfo.Hidden) is
					// never part of a bare "*" -- see the qualified-star
					// branch above and columnInfo.Hidden's doc comment.
					if c.Hidden {
						continue
					}
					// A USING/NATURAL common column appears once, at its representative (earlier)
					// table; this table's copy is skipped (see tableScope.coalesced).
					if ts.coalesced != nil {
						if _, hidden := ts.coalesced[r33sFoldIdent(c.Name)]; hidden {
							continue
						}
					}
					// A RIGHT/FULL join can make even the representative's copy NULL, so "*" emits
					// an unqualified reference that resolves back to this table with the coalesce
					// fallback (join2.test); a no-op for other columns. An unaliased self-join can
					// make the column ambiguous (sameNamedScopeAlsoExposes), per reference: "SELECT
					// 123 FROM t1 JOIN t1 USING(x)" answers, "*" errors.
					if sameNamedScopeAlsoExposes(scopes, &scopes[si], c.Name) {
						return nil, fmt.Errorf("engine: ambiguous column name: %s.%s", ts.name, c.Name)
					}
					if ts.coalesceFallback != nil {
						if _, hasFallback := ts.coalesceFallback[r33sFoldIdent(c.Name)]; hasFallback {
							if longNames {
								// A USING/NATURAL RIGHT/FULL-coalesced "*" column
								// under full_column_names is not among the shapes
								// verified against C SQLite -- decline rather
								// than risk a wrong qualified name.
								return nil, fmt.Errorf("engine: unsupported: PRAGMA full_column_names naming of a USING/NATURAL-coalesced '*' column %q", c.Name)
							}
							out = append(out, outputColumn{expr: ColumnExpr{Name: c.Name}, name: c.Name})
							continue
						}
					}
					nm, nerr := starName(ts, c.Name)
					if nerr != nil {
						return nil, nerr
					}
					// A rebuilt group's own column list names what "*" shows,
					// which is not the declared name once the group repeats one
					// (tableScope.nestedStarNames). Default naming only: the
					// ":N"-plus-full_column_names combination is not verified.
					if !longNames && ts.nestedStarNames != nil && ci < len(ts.nestedStarNames) {
						nm = ts.nestedStarNames[ci]
					}
					// An unaliased derived table has no scope name, so an emitted unqualified
					// reference would resolve by name across all FROM items and could be ambiguous
					// ("SELECT * FROM t23 LEFT JOIN (SELECT * FROM t24)", join.test). Bind by FROM
					// index instead (ColumnExpr.UsingPinned); si indexes the same scopes list the
					// resolvers walk. Same for two same-named tables from different databases
					// ("main.t4 JOIN aux1.t4"): a two-part reference cannot tell them apart, while C
					// puts the database name into the entry (select.c:6165, 6304).
					if ts.name == "" || starQualifierAmbiguousAcrossDB(scopes, si) {
						out = append(out, outputColumn{
							expr: ColumnExpr{Name: c.Name, UsingPinned: true, UsingPinnedItem: si},
							name: nm,
						})
						continue
					}
					out = append(out, outputColumn{expr: ColumnExpr{Qualifier: ts.name, Name: c.Name}, name: nm})
				}
			}
			continue
		}
		name := sc.Alias
		if !sc.HasAlias {
			// An explicit empty alias ("AS ''") keeps its empty name (HasAlias); only no
			// alias at all falls back to the source text (misc1.test).
			name = sc.Text
			// An unaliased bare rowid/oid/_rowid_ is named after the table's INTEGER PRIMARY
			// KEY column if it has one, else "rowid" ("SELECT oid FROM t" is "rowid").
			// rowidOutputColumnName returns ok=false for other references (name stays
			// sc.Text) and an error for a rowid into a NestedNonLeading table, whose name is
			// not reproduced.
			// A table's columns (view, derived table, CTE, CTAS) are named by
			// sqlite3ColumnsFromExprList, which peels COLLATE and likely() first; a result
			// set's by generateColumnNames, which does not. See colNameMode.subqueryCols.
			nameExpr := sc.Expr
			if mode.subqueryCols {
				nameExpr = skipCollateAndLikely(nameExpr)
			}
			if colRef, ok := nameExpr.(ColumnExpr); ok {
				qualifier, display, isCol, rerr := columnRefNameParts(scopes, colRef)
				if rerr != nil {
					return nil, rerr
				}
				if isCol {
					switch {
					case mode.full:
						// full_column_names=ON: "<realtable>.<column>" (or the
						// rowid display name), using the referenced column's own
						// table name -- NOT its alias. Verified: "SELECT a.f1
						// FROM test1 a" -> "test1.f1", "SELECT rowid FROM t1"
						// (IPK "id") -> "t1.id". An empty qualifier (an unaliased
						// derived-table column) can't be reproduced (SQLite's
						// "subquery-N"), so decline.
						if qualifier == "" {
							return nil, fmt.Errorf("engine: unsupported: PRAGMA full_column_names naming of a column from an unnamed subquery")
						}
						name = qualifier + "." + display
					case mode.short:
						// Default (short_column_names=ON, full_column_names=OFF):
						// the referenced column's own DECLARED name (schema case),
						// not the identifier as spelled -- "SELECT consolehost
						// FROM host" (declared "consoleHost") -> "consoleHost",
						// "SELECT rowid FROM items" -> that table's IPK name.
						// This is the engine's prior, unconditional behavior.
						name = display
					default:
						// short_column_names=OFF AND full_column_names=OFF: the
						// verbatim source-text span exactly as typed (qualifier
						// and inter-token spacing preserved) -- "SELECT test1 . f1
						// FROM test1" -> "test1 . f1", "SELECT oid FROM t" ->
						// "oid". sc.RawText holds that span (sc.Text has the
						// qualifier stripped, so it can't be used here).
						name = sc.RawText
					}
				}
				// A reference falling back to a literal (FallbackLiteral) is named by its source
				// span with quotes, as C does:
				//
				//   SELECT "nosuch"   -> column named  "nosuch"  (with quotes)
				//   SELECT """"""""   -> column named  """"""""  (all 8)
				//   SELECT "a" FROM t -> column named  a  (it resolved)
				if !isCol && colRef.FallbackLiteral != nil {
					name = sc.RawText
				}
			}
		}
		out = append(out, outputColumn{expr: sc.Expr, name: name})
	}
	return out, nil
}

// sameNamedScopeAlsoExposes reports whether a FROM item other than starred, with
// the same scope name, also exposes column name: C's ambiguity condition. For an
// unaliased self-join "t1 JOIN t1 USING(x)", "SELECT t1.y", "*" and "t1.*" are
// "ambiguous column name: main.t1.y", while "SELECT x" and "SELECT t1.x" answer
// (x is coalesced away on the second copy). Per reference.
func sameNamedScopeAlsoExposes(scopes []tableScope, starred *tableScope, name string) bool {
	if starred == nil || starred.name == "" {
		return false
	}
	lname := r33sFoldIdent(name)
	for i := range scopes {
		s := &scopes[i]
		if s == starred || !equalFoldName(s.name, starred.name) {
			continue
		}
		// A same-named scope from another database is a different table ("main.t4 JOIN
		// aux1.t4"), not a self-join; C keeps them apart by database name (select.c:6165,
		// 6304). See starQualifierAmbiguousAcrossDB.
		if s.dbIdx != starred.dbIdx {
			continue
		}
		if _, ok := s.colIndex[lname]; !ok {
			continue
		}
		if s.coalesced != nil {
			if _, hidden := s.coalesced[lname]; hidden {
				continue
			}
		}
		return true
	}
	return false
}

// starQualifierAmbiguousAcrossDB reports whether a scope other than scopes[at]
// has the same name but a different database (tableScope.dbIdx). A two-part
// reference binds by name to the first such scope, so every column of every later
// one must be pinned by FROM index, whatever its name.
func starQualifierAmbiguousAcrossDB(scopes []tableScope, at int) bool {
	for i := range scopes {
		if i == at || !equalFoldName(scopes[i].name, scopes[at].name) {
			continue
		}
		if scopes[i].dbIdx != scopes[at].dbIdx {
			return true
		}
	}
	return false
}

// countScopesNamed counts the FROM items carrying name -- more than one means
// an unaliased self-join, where a qualified reference or star through that
// name means something different from the single-item case. See
// sameNamedScopeAlsoExposes and expandSelectList's qualified-star branch.
func countScopesNamed(scopes []tableScope, name string) int {
	n := 0
	for i := range scopes {
		if equalFoldName(scopes[i].name, name) {
			n++
		}
	}
	return n
}

// findTableScope returns the first scope whose name (alias, or table name)
// matches qualifier case-insensitively, the same first-match rule a qualified
// column reference uses.
func findTableScope(scopes []tableScope, qualifier string) (*tableScope, bool) {
	for i := range scopes {
		if equalFoldName(scopes[i].name, qualifier) {
			return &scopes[i], true
		}
	}
	return nil, false
}

// columnRefNameParts resolves an unaliased select-list column reference (schema
// only, no outer chain) into what expandSelectList needs: qualifier, the real
// table name for full_column_names ("" for an unaliased derived table), and
// display, the declared column name, or for a rowid reference the IPK column's
// name or "rowid". ok is false when ce is not exactly one column or rowid.
//
// err is set only for a rowid into a NestedNonLeading table, whose display name
// in C is an uppercase literal depending on which spelling is unshadowed
// ("_ROWID_"); that is declined. One wrap layer of nesting does resolve and is
// named deterministically (FromItem.NestFromWrapDepth; see
// annotateNestedRowidNames).
func columnRefNameParts(scopes []tableScope, ce ColumnExpr) (qualifier, display string, ok bool, err error) {
	ctx := &evalCtx{tables: scopes}
	foundCtx, idx, col, rowidTableIdx, rerr := resolveColumn(ctx, ce.Qualifier, ce.Name)
	if rerr != nil || foundCtx == nil {
		return "", "", false, nil // unresolved/ambiguous: leave to the validator
	}
	// A pseudo-rowid resolves with rowidTableIdx >= 0 (a SCOPE index into
	// foundCtx.tables) AND a non-nil col (rowidColumnInfo) -- so check it FIRST.
	if rowidTableIdx >= 0 {
		ts := foundCtx.tables[rowidTableIdx]
		if ts.nestedNonLeading {
			// tableScope.nestedRowidDisplayName, when set by
			// annotateNestedRowidNames (vdbe_join_codegen.go), is the exact
			// literal name C SQLite gives this specific reference --
			// verified directly against mattn/go-sqlite3 3.53.3 (see that
			// field's doc comment). Left "" for everything the narrow,
			// verified-safe case there does not cover, in which case this
			// still declines exactly as before that mechanism existed.
			if ts.nestedRowidDisplayName != "" {
				return ts.tableName, ts.nestedRowidDisplayName, true, nil
			}
			return "", "", false, fmt.Errorf("engine: unsupported: rowid/oid/_rowid_ pseudo-column %q referenced through a non-leading parenthesized join subtree (SQLite's own display name for it is not reproduced)", ce.Name)
		}
		display = "rowid"
		for _, c := range ts.cols {
			if c.IsRowidAlias {
				display = c.Name
				break
			}
		}
		return ts.tableName, display, true, nil
	}
	if col == nil {
		return "", "", false, nil
	}
	// An ordinary column: resolveColumn returns idx as a FLATTENED value offset
	// (ts.offset + column position), NOT a scope index -- locate the owning
	// scope by the offset range it falls in. A miss (idx outside every scope's
	// range -- only possible for the rare RIGHT/FULL coalesce fallback that
	// redirects the offset to another table) leaves naming to the verbatim
	// default rather than risk a wrong qualifier.
	for i := range foundCtx.tables {
		ts := &foundCtx.tables[i]
		if idx >= ts.offset && idx < ts.offset+len(ts.cols) {
			// A member of an aliased parenthesized join group, named through
			// the member itself: the group's rebuilt column list names it,
			// ":N" suffix and all (tableScope.nestedColNames).
			if ts.nestedColNames != nil && ce.Qualifier != "" && !equalFoldName(ce.Qualifier, ts.groupAlias) {
				return ts.tableName, ts.nestedColNames[idx-ts.offset], true, nil
			}
			return ts.tableName, col.Name, true, nil
		}
	}
	return "", "", false, nil
}

// ---- generated columns ----
//
// A "[GENERATED ALWAYS] AS (<expr>) [STORED|VIRTUAL]" column differs physically
// in one way: a VIRTUAL column has no slot in the stored record, a STORED one
// does. Everything above the record codec uses full-width rows, so the two
// conversions live here:
//
//   - expandStoredRow: record slots -> full-width row (read path)
//   - contractToStoredRow: full-width row -> record slots (write path)
//
// with computeGeneratedInto filling computed values.

// hasGeneratedCols reports whether cols contains any generated column. It is
// the fast bail-out every conversion below takes first, so a schema with no
// generated column pays nothing.
func hasGeneratedCols(cols []columnInfo) bool {
	for _, c := range cols {
		if c.IsGenerated() {
			return true
		}
	}
	return false
}

// hasVirtualCols reports whether cols contains a VIRTUAL generated column --
// i.e. whether the record is narrower than the column list at all. A table
// whose generated columns are all STORED needs no slot remapping.
func hasVirtualCols(cols []columnInfo) bool {
	for _, c := range cols {
		if c.IsGenerated() && !c.GeneratedStored {
			return true
		}
	}
	return false
}

// storedColumnSlots maps each column index to its slot in the on-disk record,
// or -1 for a VIRTUAL generated column (which has none). Slots are assigned
// in declaration order, skipping virtual columns in place.
func storedColumnSlots(cols []columnInfo) []int {
	slots := make([]int, len(cols))
	n := 0
	for i, c := range cols {
		if c.IsGenerated() && !c.GeneratedStored {
			slots[i] = -1
			continue
		}
		slots[i] = n
		n++
	}
	return slots
}

// expandStoredRow widens a decoded record (one value per stored column) to a
// full-width row, leaving virtual slots NULL for computeGeneratedInto and filling
// stored columns past the record's end with their DEFAULT (padStoredRow). With no
// virtual columns it is padStoredRow. Idempotent: a full-width row (len(rec) >=
// len(cols)) is returned as is, since write-path callers pass rows from the
// already-expanded row store; widening twice shifted every column after a virtual
// one.
func expandStoredRow(cols []columnInfo, rec []Value) []Value {
	if !hasVirtualCols(cols) {
		// No slot mapping to do -- but the record may still be SHORT, because
		// a table with generated columns can also have had a column added
		// after its rows were written. padStoredRow owns that rule.
		return padStoredRow(cols, rec)
	}
	if len(rec) >= len(cols) {
		return rec
	}
	out := make([]Value, len(cols))
	for i, slot := range storedColumnSlots(cols) {
		switch {
		case slot < 0 || slot < len(rec):
			if slot >= 0 {
				out[i] = rec[slot]
			}
		case cols[i].HasDefault && cols[i].DefaultKnown:
			// The record predates an "ALTER TABLE ... ADD COLUMN", so this
			// STORED column has no slot in it at all and reads its DEFAULT --
			// padStoredRow's rule, applied through the slot mapping because
			// the missing columns are not simply the trailing ones once a
			// VIRTUAL column sits earlier in the list.
			out[i] = applyAffinityToValue(cols[i].DefaultValue, cols[i].Aff)
		}
	}
	return out
}

// padStoredRow widens a record shorter than its table's columns, as rows written
// before ADD COLUMN are (C does not rewrite them): missing columns read as their
// DEFAULT, or NULL ("1|2|NULL|9" after adding c and "d DEFAULT 9"). Without it a
// short record panicked with index out of range. An unfoldable DEFAULT gives
// NULL (ADD COLUMN cannot produce one). Tables with virtual columns use
// expandStoredRow, which applies this too.
func padStoredRow(cols []columnInfo, vals []Value) []Value {
	if len(vals) >= len(cols) {
		return vals
	}
	out := make([]Value, len(cols))
	copy(out, vals)
	for i := len(vals); i < len(cols); i++ {
		if cols[i].HasDefault && cols[i].DefaultKnown {
			// sqlite3ColumnDefault reads the default through the column's
			// affinity (sqlite3ValueFromExpr, update.c:70-74).
			out[i] = applyAffinityToValue(cols[i].DefaultValue, cols[i].Aff)
		}
	}
	return out
}

// contractToStoredRow narrows a full-width row down to the values that
// actually go into the record, dropping every VIRTUAL generated column.
// Returns row unchanged when there are no virtual columns.
func contractToStoredRow(cols []columnInfo, row []Value) []Value {
	if !hasVirtualCols(cols) {
		return row
	}
	out := make([]Value, 0, len(cols))
	for i, c := range cols {
		if c.IsGenerated() && !c.GeneratedStored {
			continue
		}
		if i < len(row) {
			out = append(out, row[i])
		} else {
			out = append(out, Value{Typ: Null})
		}
	}
	return out
}

// computeGeneratedInto evaluates every generated column's expression against
// row (which must already be full-width, with all NON-generated values in
// place) and stores each result back into its own slot, with the column's
// declared affinity applied exactly as storing any other value would.
//
// Columns are computed in declaration order, so a generated column may
// reference an earlier generated one. tblName/scope are needed to bind the
// expression's column references to this same row.
func computeGeneratedInto(tblName string, cols []columnInfo, row []Value) error {
	if !hasGeneratedCols(cols) {
		return nil
	}
	// noRowid, matching compileGeneratedColumns' own scope (sql_parser.go) and
	// C: resolve.c:626 excludes NC_GenCol from the rowid match outright, so
	// "CREATE TABLE e(x INT, y AS (rowid*2))" is "no such column: rowid" in
	// 3.53.3 -- and this ctx carries no rowids to answer with anyway. Without
	// it the two halves disagreed about what "rowid" means here, which is the
	// one thing two readers of the same schema must never do.
	scope := tableScope{name: tblName, cols: cols, colIndex: buildColIndex(cols), noRowid: true}
	ctx := &evalCtx{tables: []tableScope{scope}, vals: row}
	// Compute in dependency order: a generated column may read another
	// ("t(a, d AS (e), e AS (a), f AS (d))"), and C computes each on demand
	// (expr.c:5070-5080). columnInfo.genDeps holds the edges.
	done := make([]bool, len(cols))
	busy := make([]bool, len(cols))
	var compute func(i int) error
	compute = func(i int) error {
		c := cols[i]
		if done[i] || !c.IsGenerated() {
			return nil
		}
		if busy[i] {
			// C's own answer for a cycle here (expr.c:5072). A schema that
			// reaches this was not written by validateGeneratedColumns.
			return fmt.Errorf("engine: generated column loop on %q", c.Name)
		}
		busy[i] = true
		defer func() { busy[i] = false }()
		for _, j := range c.genDeps {
			if err := compute(j); err != nil {
				return err
			}
		}
		done[i] = true
		expr, err := cachedGeneratedExpr(c.GeneratedExpr)
		if err != nil {
			return fmt.Errorf("engine: table %s: generated column %s: %w", tblName, c.Name, err)
		}
		// The compiled body, stamped on the column by the schema funnel
		// (compileGeneratedColumns, sql_parser.go) -- SQLite's own
		// sqlite3ComputeGeneratedColumns, which codes each generated column
		// under pParse->iSelfTab = -iRegStore (insert.c:285/353/372) rather
		// than evaluating anything. A cols list that did not come through that
		// funnel has none, and eval compiles the body itself (restamp).
		gp := c.genProg
		if gp == nil {
			gp = &selfRowExpr{expr: expr}
		}
		v, err := gp.eval(ctx)
		if err != nil {
			return fmt.Errorf("engine: table %s: generated column %s: %w", tblName, c.Name, err)
		}
		row[i] = applyAffinityToValue(v, c.Aff)
		return nil
	}
	for i := range cols {
		if err := compute(i); err != nil {
			return err
		}
	}
	return nil
}

// generatedExprCache memoizes parseCheckExprText per generated-column
// expression source. A generated column is evaluated once PER ROW, so parsing
// its text on every row would turn a scan into a parse loop; the text comes
// from the schema and is fixed for the life of the table, so the parsed AST is
// safely shared (nothing that reads this tree mutates it).
var generatedExprCache sync.Map // map[string]generatedExprEntry

type generatedExprEntry struct {
	expr Expr
	err  error
}

func cachedGeneratedExpr(text string) (Expr, error) {
	if v, ok := generatedExprCache.Load(text); ok {
		e := v.(generatedExprEntry)
		return e.expr, e.err
	}
	expr, err := parseCheckExprText(text)
	generatedExprCache.Store(text, generatedExprEntry{expr: expr, err: err})
	return expr, err
}

// detachResultRows copies every byte slice a result row holds, so the rows no
// longer point into a segment file's mapping. See QueryArgs.
func detachResultRows(rows [][]Value) [][]Value {
	for _, r := range rows {
		for i := range r {
			if r[i].S != nil {
				r[i].S = append([]byte(nil), r[i].S...)
			}
		}
	}
	return rows
}
