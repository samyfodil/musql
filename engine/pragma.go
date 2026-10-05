// PRAGMA: parsing (ParsePragma) and the pragmas this engine answers exactly as
// C SQLite does. Schema introspection reads through a ReadOnlyPager, so it sees
// a held transaction's uncommitted state as well as committed state.
//
// A pragma this engine does not implement is declined for a query, never
// answered with a guessed value. On Exec, only a curated set of pragmas with no
// observable effect are accepted as no-ops (execPragmaSafeNoop); a pragma that
// changes later behaviour is either implemented or declined, since accepting it
// silently would make later statements diverge from C.
package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PragmaStmt is one parsed "PRAGMA [schema.]name [= value | (value)]"
// statement (see ParsePragma). HasValue/ValueText/ValueIsString describe the
// "= value" or "(value)" payload uniformly -- SQLite accepts either spelling
// interchangeably for every pragma, so callers never need to know which form
// the original SQL text used.
type PragmaStmt struct {
	Schema        string // "" if unqualified
	Name          string // lower-cased
	HasValue      bool
	ValueText     string // raw spelling (identifier/number text, or a string literal's DECODED contents)
	ValueIsString bool
}

// DatabasePath is the file this session writes, which a holder that carries
// per-DATABASE state across sessions needs in order to key it. driver keys
// "PRAGMA max_page_count" by it: the ceiling is per database in C SQLite
// (each aDb entry has its own pager and its own mxPgno), and a driver that opens
// one session per statement has to put the right one back on each.
func (db *DB) DatabasePath() string {
	if db == nil {
		return ""
	}
	return db.path
}

// ErrMsgDiskFull is C SQLite's wording for SQLITE_FULL, which is what a
// statement that would push the database past "PRAGMA max_page_count" reports
// -- verified against mattn/go-sqlite3 3.53.3 for CREATE TABLE, INSERT and
// UPDATE alike.
const ErrMsgDiskFull = "database or disk is full"

// headerScalarValue parses a setter value for user_version, schema_version or
// application_id. C renders these 32-bit fields signed ("= -1" reads back -1),
// and parses the value with sqlite3Atoi (pragma.c:2340): a leading integer
// prefix, "0x10" as 16, and 0 for anything that does not fit
// (pragmaGetInt32, pragma_tuning.go).
func headerScalarValue(stmt *PragmaStmt) (uint32, error) {
	if !stmt.HasValue {
		return 0, fmt.Errorf("engine: PRAGMA %s: missing value", stmt.Name)
	}
	return uint32(int32(pragmaGetInt32(stmt.ValueText))), nil
}

// pragmaDefaultOptimizeLimit is SQLITE_DEFAULT_OPTIMIZE_LIMIT, the analysis
// limit PRAGMA optimize passes to its ANALYZEs when mask bit 0x10 is set. Tables
// with at most this many rows are scanned in full, so it only changes stat1 for
// larger tables.
const pragmaDefaultOptimizeLimit = 2000

// pragmaOptimizeAnalysisFloor is the floor pragma.c's scaling clamps at.
const pragmaOptimizeAnalysisFloor = 100

// r35aOptimizeAnalysisLimit is the per-ANALYZE row limit this statement would
// impose, or 0 for none. Ported from pragma.c's PragTyp_OPTIMIZE:
//
//	if( (opMask & 0x10)==0 ){ nLimit = 0; }
//	else if( db->nAnalysisLimit>0
//	      && db->nAnalysisLimit<SQLITE_DEFAULT_OPTIMIZE_LIMIT ){ nLimit = 0; }
//	else{ nLimit = SQLITE_DEFAULT_OPTIMIZE_LIMIT; }
//	...
//	if( nLimit>0 && nBtree>100 ){
//	  nLimit = 100*nLimit/nBtree;  if( nLimit<100 ) nLimit = 100;
//	}
//
// With nLimit 0 each ANALYZE uses the connection's analysis_limit
// (pragma.c:2618), which is modelled. nBtree counts only tables optimize will
// analyze (TF_MaybeReanalyze, not decidable here), so an upper bound is used;
// that can only scale the limit down, i.e. decline where C might agree.
func (db *DB) r35aOptimizeAnalysisLimit(mask int64) int {
	if mask&0x10 == 0 {
		return 0
	}
	nBtree := 0
	for _, t := range db.tables {
		if t.isTemp || strings.HasPrefix(r33sFoldIdent(t.name), "sqlite_") {
			continue
		}
		nBtree++
		for _, ix := range db.indexes {
			if !ix.isTemp && equalFoldName(ix.table, t.name) {
				nBtree++
			}
		}
	}
	n := pragmaDefaultOptimizeLimit
	if nBtree > 100 {
		n = 100 * n / nBtree
		if n < pragmaOptimizeAnalysisFloor {
			n = pragmaOptimizeAnalysisFloor
		}
	}
	return n
}

// execOptimize implements PRAGMA optimize: for each ordinary main table with an
// index (pragma.c:2580), decide whether C would re-ANALYZE it and, if so, run
// analyzeScoped (analyze_write.go).
//
// The rule (pragma.c:2485-2511): reanalyze iff (4b: an index lacks a stat1 row,
// or 4c: the planner used stat1 for this table this session, TF_MaybeReanalyze)
// and (5a: as 4b, or 5b: the row count moved more than 10x since stat1).
//
// 4c has two sources. The single-table WHERE test (whereLoopAddBtreeIndex) is
// tracked by markMaybeReanalyze (pragma_optimize_track.go). The bloom-filter
// check (where.c:6605) needs a solved multi-table loop nest, so once a session
// has run a join or subquery (sawJoinOrSubquery) the whole database falls back to
// stat1MatchesFreshAnalyze. 5b compares against stat1Baseline, this
// connection's cached view of stat1, not its raw content.
func (db *DB) execOptimize(stmt *PragmaStmt) error {
	mask := int64(0xfffe) // pragma.c's default when no argument is given
	if stmt.HasValue {
		// sqlite3Atoi, which is what pragma.c parses the mask with -- so
		// "0x10002" is hex and "12abc" is 12. See pragmaGetInt32.
		mask = pragmaGetInt32(strings.TrimSpace(stmt.ValueText))
	}
	if mask&0x02 == 0 {
		return nil
	}
	if mask&0x01 != 0 {
		return fmt.Errorf("%w: PRAGMA optimize(0x%x) with the 0x01 DEBUG bit set (it answers one text row per ANALYZE it would have run, and which those are depends on SQLite's TF_MaybeReanalyze -- set by a query planner that consulted an index's sqlite_stat1 row, which this engine's planner never does)", errVDBEUnsupported, uint32(mask))
	}
	if len(db.attached) > 0 {
		return fmt.Errorf("%w: PRAGMA optimize while databases are ATTACHed (C SQLite walks every attached schema but temp, and this session cannot compute an attachment's statistics)", errVDBEUnsupported)
	}
	if db.queryOnly || db.writeLock != "" {
		// C SQLite lets the pragma itself through query_only (its check is
		// in OP_Transaction, and the pragma's own program opens no write
		// transaction), so the sub-ANALYZE is where a refusal would surface --
		// on one side only, since this engine would have run nothing.
		return fmt.Errorf("%w: PRAGMA optimize under PRAGMA query_only (C SQLite still codes the ANALYZE statements, whose write transaction is what query_only refuses)", errVDBEUnsupported)
	}
	limit := db.r35aOptimizeAnalysisLimit(mask)
	// See table_load.go's package doc comment: PRAGMA optimize inspects
	// every table's row count (this loop, and the second pass below)
	// regardless of which table a later ANALYZE ends up running against --
	// C SQLite's own single top-to-bottom codegen loop (this function's
	// own doc comment) does the same, so gating this costs nothing PRAGMA
	// optimize did not already pay.
	for _, t := range db.tables {
		if t.isTemp || strings.HasPrefix(r33sFoldIdent(t.name), "sqlite_") {
			continue
		}
		if err := db.ensureTableLoaded(t); err != nil {
			return err
		}
		if limit > 0 && t.rows.len() > limit {
			return fmt.Errorf("%w: PRAGMA optimize with a table of %d rows (above this statement's own analysis limit of %d its ANALYZE samples rather than scans, so it can write a sqlite_stat1 row a full ANALYZE would not -- see r35aOptimizeAnalysisLimit)", errVDBEUnsupported, t.rows.len(), limit)
		}
	}
	if db.sawJoinOrSubquery {
		// The solved-join-order-dependent site (where.c:6605-6644) might have
		// marked any table this session -- fall back to the original,
		// database-wide proof rather than guess which ones.
		if !db.stat1MatchesFreshAnalyze() {
			return fmt.Errorf("%w: PRAGMA optimize with sqlite_stat1 out of date (C SQLite re-ANALYZEs exactly the tables whose statistics the QUERY PLANNER has used and whose size has since moved; this engine's planner never reads sqlite_stat1, so it cannot tell which those are, and a no-op accept would leave stale statistics where C SQLite rewrites them)", errVDBEUnsupported)
		}
		return nil
	}
	// Decide every table first, then act: C bakes each table's threshold in at
	// compile time before any ANALYZE runs, so a later table's decision never sees
	// an earlier table's new stat1. Deciding first also means an undecidable table
	// declines before anything is written.
	var toAnalyze []string
	for _, t := range db.tables {
		if t.isTemp || strings.HasPrefix(r33sFoldIdent(t.name), "sqlite_") {
			continue
		}
		if !db.tableHasAnyIndex(t.name) {
			continue
		}
		if t.rows.len() == 0 {
			// pragma.c:2606-2610: an empty table's OP_Rewind jumps straight
			// past the ANALYZE, unconditionally -- whatever condition 4/5
			// would otherwise have said.
			continue
		}
		if db.tableMissingStat1Index(t) {
			// Condition 4b/5a, both satisfied at once by the same fact.
			toAnalyze = append(toAnalyze, t.name)
			continue
		}
		if !db.maybeReanalyzed(t.name) {
			// No WHERE match against t's index was recorded on this *DB, but the driver's
			// autocommit opens a fresh *DB per statement, so an earlier statement may have
			// set TF_MaybeReanalyze unseen. Use stat1's on-disk row count
			// (stat1DiskRowCount) only to prove the growth check is negative either way;
			// outside that window, decline.
			disk, ok := db.stat1DiskRowCount(t.name)
			if !ok || stat1GrowthOutsideWindow(disk, int64(t.rows.len())) {
				return fmt.Errorf("%w: PRAGMA optimize on %s, whose sqlite_stat1 size has moved and which this *DB never itself queried (this engine cannot tell a genuinely never-queried table from one queried on an earlier autocommit statement's own throwaway session, which C SQLite's own TF_MaybeReanalyze would still remember)", errVDBEUnsupported, t.name)
			}
			continue
		}
		baseline, ok := db.stat1BaselineRowCount(t.name)
		if !ok {
			// maybeReanalyzed can only become true via a WHERE match against
			// an index this table's OWN sqlite_stat1 rows exist for
			// (tableMissingStat1Index above would otherwise have already
			// caught it), and every such row is created by an ANALYZE that
			// also populates stat1Baseline (refreshStat1Baseline) -- so this
			// is believed unreachable. Declined rather than guessed if it
			// ever is.
			return fmt.Errorf("%w: PRAGMA optimize needs %s's own sqlite_stat1 baseline, which this session has no record of", errVDBEUnsupported, t.name)
		}
		if stat1GrowthOutsideWindow(baseline, int64(t.rows.len())) {
			// Condition 5b.
			toAnalyze = append(toAnalyze, t.name)
		}
	}
	// The act pass: every table above already survived every decline check,
	// so a failure from here on is a genuine write-path error (analyzeScoped
	// itself), never a "could this table have been declined" question.
	for _, name := range toAnalyze {
		if err := db.analyzeScoped(name); err != nil {
			return err
		}
	}
	return nil
}

// stat1MatchesFreshAnalyze reports whether sqlite_stat1's stored content is
// already exactly what a full ANALYZE of this database would compute -- the
// condition that makes any ANALYZE PRAGMA optimize might run a no-op. Compared
// as a MULTISET of (tbl, idx, stat): optimize rewrites one table's rows at a
// time, so what matters is that every row it could delete is one it would
// immediately write back, and no row it would write is missing.
func (db *DB) stat1MatchesFreshAnalyze() bool {
	want := db.computeStat1Rows()
	t := db.findTableMetaIn(scopeMain, "sqlite_stat1")
	if t == nil {
		// No table at all: only an empty computation matches, and an ANALYZE
		// would otherwise CREATE it (a schema change, not just a row change).
		return len(want) == 0
	}
	if t.rows.len() != len(want) {
		return false
	}
	key := func(tbl string, idx Value, stat string) string {
		who := "\x00" // idx IS NULL, the table-level row
		if idx.Typ == Text {
			who = string(idx.S)
		}
		return tbl + "\x01" + who + "\x01" + stat
	}
	have := make(map[string]int, t.rows.len())
	for _, row := range t.rows.all() {
		if len(row) != 3 || row[0].Typ != Text || row[2].Typ != Text {
			return false // hand-written content this cannot compare
		}
		if row[1].Typ != Text && row[1].Typ != Null {
			return false
		}
		have[key(string(row[0].S), row[1], string(row[2].S))]++
	}
	for _, r := range want {
		k := key(r.tbl, r.idx, r.stat)
		if have[k] == 0 {
			return false
		}
		have[k]--
	}
	return true
}

// tempPragmaAnswersMain reports whether "PRAGMA temp.<name>" answers exactly
// what the unqualified form does, so the qualifier can be dropped before
// declineFileScopedTempPragma. Only data_version: always 1 for main here, and
// always 1 for temp in C too, since a temp database has one connection.
func tempPragmaAnswersMain(name string) bool {
	return equalFoldName(strings.TrimSpace(name), "data_version")
}

// isPragmaStmt reports whether trimmed (already whitespace-trimmed SQL text)
// is a PRAGMA statement, from its already-lexed tokens -- the same leading-
// keyword sniff every other top-level dispatcher in this package uses (see
// e.g. Exec's own token-switch in insert_write.go).
func isPragmaStmt(toks []token) bool {
	return len(toks) > 0 && toks[0].kind == tkIdent && toks[0].upper() == "PRAGMA"
}

// ParsePragma parses "PRAGMA [schema.]name [= value | (value)]": SQLite's own
// grammar for the statement (see https://www.sqlite.org/pragma.html#syntax).
// Only a single scalar value (identifier, signed number, or string literal)
// is accepted as the payload -- every pragma this package implements needs at
// most one.
func ParsePragma(sqlText string) (*PragmaStmt, error) {
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil {
		return nil, err
	}
	i := 0
	if !(i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "PRAGMA") {
		return nil, fmt.Errorf("engine: PRAGMA: expected PRAGMA")
	}
	i++
	// Both names are parse.y's nm, which is an identifier OR a string literal
	// (parse.y:339-340, the PRAGMA rules at 1714-1719): "PRAGMA 'main'.page_size"
	// is how rtree.c's own getNodeSize asks (rtree.c:3577).
	nm := func() (string, bool) {
		if i < len(toks) && toks[i].kind == tkIdent {
			return toks[i].text, true
		}
		if i < len(toks) && toks[i].kind == tkString {
			return toks[i].str, true
		}
		return "", false
	}
	first, ok := nm()
	if !ok {
		// A bare "PRAGMA" ran out of input, which parse.y:44-51's %syntax_error
		// action reports as "incomplete input" whatever the grammar wanted; a
		// non-identifier token is an ordinary syntax error at that token.
		if i >= len(toks) || toks[i].text == "" {
			return nil, fmt.Errorf("engine: incomplete input")
		}
		return nil, fmt.Errorf("engine: near %q: syntax error", toks[i].text)
	}
	i++
	schema, name := "", first
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		second, ok := nm()
		if !ok {
			return nil, fmt.Errorf("engine: PRAGMA: expected name after schema qualifier")
		}
		schema = first
		name = second
		i++
	}
	stmt := &PragmaStmt{Schema: schema, Name: r33sFoldIdent(name)}

	readValue := func() error {
		if i >= len(toks) {
			return fmt.Errorf("engine: PRAGMA %s: expected a value", stmt.Name)
		}
		t := toks[i]
		switch {
		case t.kind == tkPunct && (t.text == "-" || t.text == "+") && i+1 < len(toks) && toks[i+1].kind == tkNumber:
			sign := ""
			if t.text == "-" {
				sign = "-"
			}
			stmt.ValueText = sign + toks[i+1].text
			i += 2
		case t.kind == tkNumber:
			stmt.ValueText = t.text
			i++
		case t.kind == tkString:
			stmt.ValueText = t.str
			stmt.ValueIsString = true
			i++
		case t.kind == tkIdent:
			stmt.ValueText = t.text
			i++
		default:
			return fmt.Errorf("engine: PRAGMA %s: unexpected value token %q", stmt.Name, t.text)
		}
		stmt.HasValue = true
		return nil
	}

	switch {
	case i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "=":
		i++
		if err := readValue(); err != nil {
			return nil, err
		}
	case i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "(":
		i++
		if err := readValue(); err != nil {
			return nil, err
		}
		if !(i < len(toks) && toks[i].kind == tkPunct && toks[i].text == ")") {
			return nil, fmt.Errorf("engine: PRAGMA %s: expected ')'", stmt.Name)
		}
		i++
	}
	if !(i < len(toks) && toks[i].kind == tkEOF) {
		return nil, fmt.Errorf("engine: PRAGMA %s: unexpected trailing input", stmt.Name)
	}
	return stmt, nil
}

// ---- write side: setters + the blanket no-op accept ----

// execPragma implements Exec's PRAGMA case: implemented setters mutate
// connection or catalog state, a curated set of side-effect-free names
// (execPragmaSafeNoop) are no-ops, and everything else is declined. A malformed
// PRAGMA is always an error.
//
// This is not a blanket accept, although C ignores unknown pragmas: accepting a
// pragma that changes later behaviour without implementing it makes later
// statements silently diverge from C.
func (db *DB) execPragma(sqlText string) error {
	stmt, err := ParsePragma(sqlText)
	if err != nil {
		return err
	}
	// A failed schema load refuses every pragma but "writable_schema" itself,
	// which is the one that LIFTS it (see schemaCorruptRefusesStatement's own
	// doc comment, schema_reload_rootpage.go, for the measured split and why
	// this engine's is deliberately the coarser one). This sits at the very
	// top, ahead of every other rule, exactly as C SQLite's own
	// sqlite3ReadSchema failure does: it happens during PREPARE, before the
	// pragma's body is ever reached.
	if stmt.Name != "writable_schema" {
		if cerr := db.schemaCorruptErr(); cerr != nil {
			return cerr
		}
	}
	// A reserved word as the value is a syntax error for every pragma
	// (parse.y: "nmnum ::= plus_num | nm | ON | DELETE | DEFAULT"), so
	// "PRAGMA foreign_keys=NULL" fails 'near "NULL"'. Checked first so the answer
	// does not depend on which arm handles the name.
	if serr := pragmaValueSpellingSupported(stmt); serr != nil {
		return serr
	}
	// The tuning pragmas are answered from this session's own per-database
	// values, qualified form included -- see execTuningPragma, which must run
	// BEFORE the scope switch below would decline that form.
	if handled, terr := db.execTuningPragma(stmt); handled {
		return terr
	}
	// A qualifier that names an ATTACHed database is sorted into one of the
	// three buckets attachedPragmaScope describes; the ROUTED one never gets
	// here (ExecArgs delegated it to that database's own session first).
	switch attachedPragmaScope(stmt.Name) {
	case pragmaScopeConnection:
		// The qualifier changes nothing at all -- drop it and answer here.
		if db.attachedNamed(stmt.Schema) != nil {
			stmt.Schema = ""
		}
	case pragmaScopeRouted:
		if db.attachedNamed(stmt.Schema) != nil {
			// Unreachable through ExecArgs (execRoutedToAttached ran first), and
			// a hard error rather than a fallthrough so a future caller that
			// bypasses the routing cannot silently answer for MAIN instead.
			return fmt.Errorf("engine: PRAGMA %s.%s was not routed to that database's own session (see attach_write.go)", stmt.Schema, stmt.Name)
		}
	case pragmaScopeDeclined:
		if db.attachedNamed(stmt.Schema) != nil {
			return fmt.Errorf("%w: PRAGMA %s.%s (this pragma's qualified form is neither per-database nor per-connection here -- see attachedPragmaScope)", errVDBEUnsupported, strings.ToLower(stmt.Schema), stmt.Name)
		}
	}
	// "PRAGMA <db>.<name>" against a database that is not attached is an
	// error in C SQLite ("unknown database aux" -- alter3/alter4.test),
	// not a silently-ignored qualifier: before this check the qualifier was
	// dropped and the pragma answered for main, which is a WRONG answer
	// rather than a decline. Anything other than this engine's own two
	// catalogs (temp_schema.go) cannot resolve; "temp." itself resolves only
	// for the object-scoped pragmas -- see declineFileScopedTempPragma.
	if err := db.checkWriteSchemaQualifier(stmt.Schema); err != nil {
		return err
	}
	// A name C SQLite does not recognize is INERT there -- no rows, no
	// error, no state touched (pragma_unknown.go). It sits AFTER the qualifier
	// check, because "PRAGMA aux.bogus" against an unattached aux is "unknown
	// database aux" on both sides, and BEFORE every name-keyed rule below,
	// because those all presuppose a pragma that exists: without this,
	// "PRAGMA temp.sql_trace=1" was declined for reading a per-database header
	// that sql_trace has nothing to do with.
	if unknownPragmaIsNoop(stmt.Name) {
		return nil
	}
	if !tempPragmaAnswersMain(stmt.Name) {
		// page_count/freelist_count are getters whose row Exec discards, and C's getter
		// never rejects (pragma.c:663, 2324), so accept the temp-qualified form here.
		// The query path keeps its decline. A temp setter is not accepted: this engine
		// does not write the temp header.
		if r35aEmptyTempPragmaName(stmt.Name) && isTempSchemaQualifier(stmt.Schema) && !stmt.HasValue {
			return nil
		}
		// A GETTER over the TEMP database's own header is answerable now: that
		// database has a header of its own (temp_store.go). An Exec discards
		// the row, so reaching this point is enough -- what matters is not
		// refusing a statement C SQLite runs.
		if isTempSchemaQualifier(stmt.Schema) && !stmt.HasValue && pragmaReadsHeader(stmt.Name) {
			return nil
		}
		if err := declineFileScopedTempPragma(stmt.Schema, stmt.Name); err != nil {
			return err
		}
	}
	switch stmt.Name {
	case "user_version":
		if stmt.HasValue {
			n, err := headerScalarValue(stmt)
			if err != nil {
				return err
			}
			if isTempSchemaQualifier(stmt.Schema) {
				// The TEMP database has a header of its own (temp_store.go),
				// and C SQLite writes this value there -- opening that
				// database to do it. Main's stays untouched.
				return db.setTempHeaderScalar("user_version", uint32(n))
			}
			db.userVersion = n
		}
		return nil
	case "schema_version":
		if stmt.HasValue {
			n, err := headerScalarValue(stmt)
			if err != nil {
				return err
			}
			if isTempSchemaQualifier(stmt.Schema) {
				return db.setTempHeaderScalar("schema_version", uint32(n))
			}
			db.schemaCookie = n
			// A schema_version write forces a full schema reload (altertab.test 22.0): C
			// records a stale cookie (vdbe.c:4262) so the next statement reloads. This
			// engine reloads here instead, and only while a direct sqlite_master edit is
			// outstanding, the only time it is observable (schema_reload_image.go). The
			// reload is failure-atomic; ROLLBACK undoes it (txn.go).
			if db.writableSchemaEditsActive() {
				db.reloadSchemaFromSegmentCatalog(true)
			}
		}
		return nil
	case "application_id":
		// application_id is a header scalar handled like user_version: the setter
		// stores it, the commit writes it to the catalog, the getter reads it. The
		// setter returns no rows, "application_id(N)" is the same setter, a qualified
		// form routes to that database (attachedPragmaScope), ROLLBACK reverts it.
		if stmt.HasValue {
			n, err := headerScalarValue(stmt)
			if err != nil {
				return err
			}
			if isTempSchemaQualifier(stmt.Schema) {
				return db.setTempHeaderScalar("application_id", uint32(n))
			}
			db.applicationID = n
		}
		return nil
	case "foreign_keys":
		// The setter turns FK enforcement on or off (fk.go). It returns no rows, and
		// inside a transaction it is silently ignored, as in C. Values parse as
		// sqlite3GetBoolean does (pragmaGetBoolean): "=2" is on, "=bogus" and "=-1" off.
		if stmt.HasValue {
			b := pragmaGetBoolean(stmt.ValueText, false)
			if db.inTransaction() {
				return nil // a no-op in C SQLite too -- see above
			}
			db.fkEnforce = b
			// The resolved-foreign-key cache is keyed on the schema
			// generation, not on this flag, and resolution errors are only
			// ever raised while enforcing -- so nothing to invalidate here.
			return nil
		}
		return nil
	case "query_only":
		// A real behaviour switch -- every later WRITE fails -- so it is
		// faithfully implemented rather than no-op'd. See queryOnlyRefusesWrite
		// for the enforcement and its oracle evidence. Plain connection state:
		// the setter takes effect inside a transaction and is still set after
		// the COMMIT (verified), so there is no transaction guard. A
		// non-canonical boolean is declined rather than guessed.
		if !stmt.HasValue {
			return nil // a getter; its single row is discarded by an Exec
		}
		b := pragmaGetBoolean(stmt.ValueText, false)
		db.queryOnly = b
		return nil
	case "recursive_triggers":
		// recursive_triggers decides whether a trigger already on the stack may fire
		// again (triggerEnter) and whether a REPLACE victim's delete fires DELETE
		// triggers (compileReplaceVictimDeletePlans). See SetRecursiveTriggers. It
		// takes effect inside a transaction; non-canonical booleans decline.
		if !stmt.HasValue {
			return nil // a getter; its single row is discarded by an Exec
		}
		b := pragmaGetBoolean(stmt.ValueText, false)
		db.SetRecursiveTriggers(b)
		return nil
	case "automatic_index":
		// automatic_index switches the transient automatic index, whose key decides
		// the inner loop's row order (visible to group_concat, bare columns, LIMIT);
		// see markWherePlanEligibility. From pragma.c's PragTyp_FLAG: default ON
		// (stored inverted in DB.noAutoIndex), getter one INTEGER row, setter none,
		// per connection so a qualifier is ignored, no transaction guard.
		// Non-canonical booleans decline.
		if !stmt.HasValue {
			return nil // a getter; its single row is discarded by an Exec
		}
		b := pragmaGetBoolean(stmt.ValueText, false)
		db.SetAutomaticIndex(b)
		return nil
	case "defer_foreign_keys":
		// defer_foreign_keys makes every FK deferred for the rest of the transaction
		// (fk.go). Getter one row, setter none; it works inside a transaction and with
		// foreign_keys off. Inside a transaction it lasts until the outermost COMMIT,
		// ROLLBACK or committing RELEASE (clearTxnState); setting 0 zeroes the counter.
		//
		// Outside a transaction, C clears it in sqlite3VdbeHalt when the statement was
		// a reader and left autocommit on (vdbeaux.c:3344, 3401); BEGIN turns autocommit
		// off first, so the flag survives it (fkey6.test 1.8). See
		// pragma_defer_fk_autocommit.go. A driver-held transaction is served only when
		// the holder runs the deferred check at its own commit
		// (MarkDeferredFKCommitter).
		if !stmt.HasValue {
			return nil // a getter; its single row is discarded by an Exec
		}
		if db.fkDeferredUnmodelled() {
			return fmt.Errorf("%w: PRAGMA defer_foreign_keys inside a driver-held transaction whose holder does not run the COMMIT check (see MarkDeferredFKCommitter, fk.go)", errVDBEUnsupported)
		}
		db.SetDeferForeignKeys(pragmaGetBoolean(stmt.ValueText, false))
		return nil
	case "trusted_schema":
		// trusted_schema: ON is the default. Per connection (a qualifier is ignored),
		// no transaction restriction, getter one row, setter none. With OFF a schema
		// object may not use a non-innocuous function or virtual table; see
		// trusted_schema.go. Non-canonical booleans decline.
		if !stmt.HasValue {
			return nil // a getter; its single row is discarded by an Exec
		}
		b := pragmaGetBoolean(stmt.ValueText, false)
		db.SetTrustedSchema(b)
		return nil
	case "legacy_alter_table":
		// legacy_alter_table changes what a later "ALTER TABLE ... RENAME TO" rewrites
		// (see PragmaConnState.LegacyAlterTable). renameTableTo does not read it yet, so
		// ExecArgs refuses RENAME TO while it is on. Getter one INTEGER row, setter
		// none, qualifier ignored.
		if !stmt.HasValue {
			return nil
		}
		b := pragmaGetBoolean(stmt.ValueText, false)
		db.SetLegacyAlterTable(b)
		return nil
	case "ignore_check_constraints":
		// ignore_check_constraints: per connection; see SetIgnoreCheckConstraints.
		// Getter one INTEGER row, setter none, qualifier ignored.
		if !stmt.HasValue {
			return nil
		}
		b := pragmaGetBoolean(stmt.ValueText, false)
		db.SetIgnoreCheckConstraints(b)
		return nil
	case "optimize":
		// PRAGMA optimize: see execOptimize for how the set of tables C would
		// re-analyze is decided.
		return db.execOptimize(stmt)
	case "journal_mode":
		// The row this produces is discarded on the exec path -- a driver's Exec
		// of a row-returning PRAGMA drops it, exactly as C SQLite's does --
		// but unlike the query side this one can actually SWITCH the mode, which
		// is what execJournalMode does. See its doc comment for the accept/
		// decline split and why it is wider here than on the read side.
		if isTempSchemaQualifier(stmt.Schema) {
			// The TEMP database's own mode is separate connection state with its
			// own rules -- see tempJournalModeResult.
			return db.execTempJournalMode(stmt)
		}
		return db.execJournalMode(stmt)
	case "auto_vacuum":
		return db.execAutoVacuum(stmt)
	case "secure_delete":
		// The row this produces is discarded on the exec path, like
		// journal_mode's -- what matters here is that the setting is really
		// recorded, so every later getter reports it. See SecureDeleteResult.
		// Per-DATABASE now (PragmaConnState.SecureDelete): a qualified setter
		// touches one database where a bare one sets them all, which is what
		// the qualified form used to be declined for.
		if err := db.checkWriteSchemaQualifier(stmt.Schema); err != nil {
			return err
		}
		if db.pragmaState == nil {
			db.pragmaState = &PragmaConnState{}
		}
		if _, _, err := SecureDeleteStateResult(stmt, db.pragmaState); err != nil {
			return err
		}
		// DB.secureDelete stays MAIN's value, for the older single-value
		// plumbing the pager copy and the driver's push still use.
		db.secureDelete = SecureDeleteMain(db.pragmaState)
		return nil
	case "incremental_vacuum":
		// This format keeps no freelist, so there is nothing to reclaim.
		return nil
	case "lock_status":
		// Nothing to do and nothing to report -- see lockStatusResult.
		return nil
	case "page_count", "freelist_count":
		// Getters; their rows are discarded by an Exec, and neither changes
		// anything. (C SQLite's "PRAGMA page_count = N" is not a setter
		// either -- the value is ignored.)
		return nil
	case "max_page_count":
		// The getter's row is discarded here and answered by queryPragma. A
		// setter has nothing to count on this format: PragmaHasNoMeaningHere owns
		// the wording and the reason.
		if !stmt.HasValue {
			return nil
		}
		return PragmaHasNoMeaningHere(stmt.Name)
	case "wal_checkpoint":
		// A CHECKPOINT is a write: it folds the log into the database file. Its
		// row is discarded on the exec path, exactly like journal_mode's, but the
		// action must still happen here -- and its accept/decline split must
		// match the query side's, which rejects an invalid mode name.
		_, _, err := db.walCheckpointPragma(stmt)
		return err
	case "wal_autocheckpoint":
		_, _, err := db.walAutoCheckpointPragma(stmt)
		return err
	case "foreign_key_check":
		// foreign_key_check's rows are discarded on Exec, but it must still raise the
		// errors C raises (no such table, foreign key mismatch), so run it against a
		// snapshot of this session's state and keep only the error.
		p, err := db.SnapshotPager()
		if err != nil {
			return err
		}
		scope, _ := scopeOfQualifier(stmt.Schema)
		// "PRAGMA temp.foreign_key_check" checks the TEMP DATABASE's tables,
		// which are in its own file (temp_store.go) and so in its own pager's
		// schema -- this one's holds main's alone. The QUERY path routes the
		// same way (queryPragmaStmt); without this the Exec silently checked
		// nothing and accepted a statement C SQLite fails with "foreign key
		// mismatch".
		if isTempSchemaQualifier(stmt.Schema) {
			if owner, found := p.attachedReaderNamed("temp"); found {
				p = owner
			}
		}
		if !stmt.HasValue {
			// The whole-database form. Its ROWS are in a schema-hash order this
			// engine cannot reproduce, which is why the QUERY side still
			// declines it -- but an Exec discards them, so what is left is
			// exactly the error, and that IS reproducible. See
			// fkCheckWholeDatabase.
			return p.fkCheckWholeDatabase(scope)
		}
		_, _, err = p.pragmaForeignKeyCheck(scope, stmt)
		return err
	case "locking_mode":
		// The row this produces is discarded on the exec path, like
		// journal_mode's -- what matters here is that the mode is really
		// ENTERED (the lock taken and kept), so every later getter reports it
		// and another connection genuinely sees it. See execLockingMode.
		_, _, err := db.execLockingMode(stmt)
		return err
	case "writable_schema":
		// The setter's empty result set is discarded on the exec path; what
		// matters here is that the flag is really RECORDED, so every later
		// getter reports it and the sqlite_master write path
		// (schema_write_direct.go) is really unlocked.
		if rerr := db.writableSchemaResetDecline(stmt); rerr != nil {
			return rerr
		}
		_, _, next, err := writableSchemaResult(stmt, db.writableSchema)
		if err != nil {
			return err
		}
		db.writableSchema = next
		// A DIRECT sqlite_schema WRITE is the one way a malformed catalog row
		// arrives without the schema cookie moving -- and it is exactly what this
		// pragma is turned on to do. So entering or leaving the mode invalidates
		// the cached corruption verdict (schemaCorruptVerdict). Without it a clean
		// verdict cached before the edit outlived it, and the engine happily used a
		// table whose stored SQL was the word "nonsense".
		db.schemaCorruptGen++
		if _, isReset := writableSchemaValue(stmt.ValueText); isReset && stmt.HasValue {
			db.rtreeConns.reset() // pragma.c:1182's sqlite3ResetAllSchemasOfConnection
		}
		if next {
			// Turning it ON is what LIFTS a failed schema load, because
			// the reload C SQLite retries at the next prepare then takes
			// corruptSchema's silent arm (prepare.c:45-46) instead of the
			// message-formatting one. Measured; see clearSchemaCorrupt
			// (schema_reload_rootpage.go).
			db.clearSchemaCorrupt()
		}
		return nil
	case "case_sensitive_like":
		// Setter: switch this connection's LIKE case sensitivity (operator and like());
		// GLOB is unaffected. Non-canonical values decline.
		if stmt.HasValue {
			b := pragmaGetBoolean(stmt.ValueText, false)
			db.caseSensitiveLike = b
			return nil
		}
		// The bare form is a no-op in C: no rows, setting unchanged.
		return nil
	case "full_column_names", "short_column_names":
		// short_column_names / full_column_names change result column naming, applied
		// in expandSelectList (query.go). The getter is answered by queryPragmaStmt
		// from the same flags. Non-canonical values decline.
		if stmt.HasValue {
			b := pragmaGetBoolean(stmt.ValueText, false)
			if stmt.Name == "full_column_names" {
				db.fullColumnNames = b
			} else {
				db.shortColumnNamesOff = !b
			}
			return nil
		}
		// The GETTER's row is produced by the read side (queryPragmaStmt) and
		// discarded here, exactly as locking_mode's and journal_mode's are:
		// nothing on the exec path changes, so "do nothing, successfully" is
		// the byte-exact behavior. It used to be declined for want of that
		// read-side reporter, which now exists.
		return nil
	case "integrity_check", "quick_check":
		// A non-numeric argument names a table that must exist ("no such table:
		// nope"); an index name is rejected too. A numeric argument is an error cap.
		if stmt.HasValue {
			// A QUOTED numeral is a NAME, not a count -- "PRAGMA
			// quick_check('3')" is "no such table: 3" while the bare
			// "quick_check(3)" is a max-error count (verified against 3.53.3).
			// Only a bare integer literal escapes the name lookup, which is
			// why ValueIsString is tested FIRST.
			numeric := false
			if !stmt.ValueIsString {
				_, perr := strconv.ParseInt(stmt.ValueText, 10, 64)
				numeric = perr == nil
			}
			if !numeric &&
				!equalFoldName(stmt.ValueText, "sqlite_master") &&
				!equalFoldName(stmt.ValueText, "sqlite_schema") &&
				db.findTableMeta(stmt.ValueText) == nil &&
				// A VIEW qualifies too: C SQLite answers "ok" for one,
				// while an INDEX name -- a real schema object, but not a
				// table -- is rejected like any other unknown name.
				db.findViewMeta(stmt.ValueText) == nil &&
				// A virtual table counts too: sqlite3LocateTable finds any Table*
				// (pragma.c:1725, build.c:337). Vtabs live in db.vtabs, not db.tables.
				db.findVtabMeta(stmt.ValueText) == nil {
				return fmt.Errorf("engine: no such table: %s", stmt.ValueText)
			}
		}
		// The rows are the read side's; what reaches the exec side is whether
		// the pragma FAILS, and the r-tree pass can fail it (rtree_check.go's
		// integrityCheckRtrees). Main only: an attached or temp r-tree is not
		// reached from here.
		if stmt.Schema == "" || strings.EqualFold(stmt.Schema, "main") {
			if len(db.vtabs) > 0 {
				p, perr := db.SnapshotPager()
				if perr != nil {
					return perr
				}
				_, only := integrityCheckArgs(stmt)
				if _, verr := p.integrityCheckRtrees(scopeMain, only); verr != nil {
					return verr
				}
			}
		}
		return nil
	case "encoding":
		// "PRAGMA encoding = <name>" takes effect only on a database with no schema
		// yet; otherwise C silently ignores it, including unrecognized names. A UTF-16
		// request on an empty database is declined: it changes the encoding of every
		// text value the connection produces, built-in results included, not just
		// stored rows.
		if stmt.HasValue {
			want, known := textEncodingByName(stmt.ValueText)
			switch {
			case db.hasSchemaObjects():
				// Too late to change it, whatever the name says -- verified for
				// an unrecognized name too.
				return nil
			case known && want == db.encoding():
				return nil // already that encoding; a genuine no-op
			case known:
				// A still-EMPTY database really switches -- see utf16.go for the
				// three seams that makes observable. The encoding is a catalog
				// field (ConvertedCatalog.Encoding), so a reopen reads it back.
				db.textEncoding = want
				return nil
			case !known:
				// On a still-EMPTY database an unrecognized name is an ERROR
				// there, not the silent no-op it becomes once a schema exists
				// (verified: "PRAGMA encoding='bogus'" errors on a fresh
				// database and is ignored after a CREATE TABLE).
				return fmt.Errorf("engine: unsupported PRAGMA encoding=%s (C SQLite rejects an unrecognized encoding name on a database that has no schema yet, and ignores it once one exists)", stmt.ValueText)
			}
		}
		return nil
	}
	if execPragmaSafeNoop[stmt.Name] || execPragmaTuningNoop[stmt.Name] {
		if err := pragmaValueSpellingSupported(stmt); err != nil {
			return err
		}
		if err := pragmaTransactionRestriction(db, stmt); err != nil {
			return err
		}
		// temp_store is a no-op for everything this engine does EXCEPT one
		// thing: asking for MEMORY makes C SQLite's temp database
		// memory-backed, which its "PRAGMA temp.journal_mode" then reports and
		// follows different rules for. Record that so the getter declines
		// instead of answering "delete" -- see tempJournalModeResult.
		if stmt.Name == "temp_store" && stmt.HasValue && tempStoreAsksForMemory(stmt.ValueText) {
			db.tempJournalMode = tempJournalModeUnmodelled
		}
		// ...and page_size is a no-op for everything this engine does EXCEPT one
		// thing: C SQLite DEFERS the request to the next VACUUM INTO, whose
		// copy then comes out at the requested size. Record it so execVacuumInto
		// can build the copy at that size -- notePageSizeRequest (writer.go)
		// owns the rule and the oracle evidence.
		if stmt.Name == "page_size" && stmt.HasValue {
			db.notePageSizeRequest(stmt.ValueText)
			// The page size a database was created at is a catalog field
			// (ConvertedCatalog.PageSize). The setter takes on a database with no schema
			// yet and is ignored afterwards, as in C. fts3 derives its segment-spill budget
			// from it, so it is not cosmetic.
			if !db.hasSchemaObjects() {
				if n := db.pageSizeRequest; n >= 512 && n <= 65536 && isPowerOfTwo(uint32(n)) {
					db.pageSize, db.usable = uint32(n), uint32(n)
				}
			}
		}
		return nil
	}
	return fmt.Errorf("engine: unsupported PRAGMA %s", stmt.Name)
}

// ---- PRAGMA full_column_names / short_column_names ----

// SetFullColumnNames / SetShortColumnNames are the write side of the two
// column-naming flags (pragma.c PragTyp_FLAG). The driver opens a fresh DB per
// autocommit statement, so it pushes the flags into each session
// (Conn.fullColumnNames). The short flag is stored inverted so the zero value is
// C's default (ShortColNames on, FullColNames off).
func (db *DB) SetFullColumnNames(on bool) { db.fullColumnNames = on }

// SetShortColumnNames is SetFullColumnNames' counterpart for
// SQLITE_ShortColNames; see it for the model and for the inversion.
func (db *DB) SetShortColumnNames(on bool) { db.shortColumnNamesOff = !on }

// SetFullColumnNames pushes the connection flag onto a pager opened from disk
// (the driver's autocommit reads). Nil-tolerant. Drops planCache: a compiled
// Program carries its ColNames, and the driver reuses one warm pager.
func (p *ReadOnlyPager) SetFullColumnNames(on bool) {
	if p == nil || p.fullColumnNames == on {
		return
	}
	p.fullColumnNames = on
	p.planCache = nil
}

// SetShortColumnNames is SetFullColumnNames' counterpart; see it, including for
// why a real change drops the compiled-plan cache.
func (p *ReadOnlyPager) SetShortColumnNames(on bool) {
	if p == nil || p.shortColumnNamesOff == !on {
		return
	}
	p.shortColumnNamesOff = !on
	p.planCache = nil
}

// ---- PRAGMA legacy_alter_table ----

// SetLegacyAlterTable / LegacyAlterTable let the driver push the connection
// flag into every session it opens. It lives in PragmaConnState, which
// SnapshotPager carries to the read snapshot where the getter is answered.
func (db *DB) SetLegacyAlterTable(on bool) {
	if db.pragmaState == nil {
		if !on {
			return // the default; nothing to record
		}
		db.pragmaState = &PragmaConnState{}
	}
	db.pragmaState.LegacyAlterTable = on
}

// LegacyAlterTable reports this connection's "PRAGMA legacy_alter_table". It is
// what "ALTER TABLE ... RENAME TO" has to consult: with it ON, C SQLite
// rewrites the renamed table's OWN row (and an index's, and a trigger's ON
// clause) and leaves every REFERENCE to it alone -- the trigger body, another
// table's REFERENCES (unless "PRAGMA foreign_keys" is separately ON), and a
// view's body. See PragmaConnState.LegacyAlterTable for the measured table,
// and renameTableTo/renameVtabTo (alter_write.go/alter_vtab_write.go) for
// where this is read.
func (db *DB) LegacyAlterTable() bool {
	return db != nil && db.pragmaState != nil && db.pragmaState.LegacyAlterTable
}

// ---- PRAGMA ignore_check_constraints ----

// SetIgnoreCheckConstraints is the write side of "PRAGMA
// ignore_check_constraints" (SQLITE_IgnoreChecks): per connection, default OFF,
// not in the file, not transactional. It suppresses CHECK enforcement in
// sqlite3GenerateConstraintChecks (every write path) and in integrity_check.
// NOT NULL, UNIQUE, PK, FKs, STRICT and RAISE are untouched. Readers:
// checkTableChecks (insert_write.go), emitCheckConstraintsAction
// (vdbe_write.go), integrityCheckViolations.
func (db *DB) SetIgnoreCheckConstraints(on bool) {
	if db.pragmaState == nil {
		if !on {
			return // the default; nothing to record
		}
		db.pragmaState = &PragmaConnState{}
	}
	if db.pragmaState.IgnoreCheckConstraints == on {
		return
	}
	db.pragmaState.IgnoreCheckConstraints = on
	// CHECKs are compiled into the write program and the plan cache is keyed on
	// text, so drop it or a re-executed INSERT keeps the old flag's plan.
	db.writePlans = nil
}

// IgnoreCheckConstraints reports this connection's "PRAGMA
// ignore_check_constraints". Nil-safe: a nil *DB is reachable on the compile
// path, and the flag's default is OFF.
func (db *DB) IgnoreCheckConstraints() bool {
	return db != nil && db.pragmaState != nil && db.pragmaState.IgnoreCheckConstraints
}

// IgnoreCheckConstraints is the read side's view of the same flag, carried onto
// the snapshot by SnapshotPager (which copies db.pragmaState) or pushed in by a
// driver that opens a fresh read pager per statement. integrity_check is what
// consults it.
func (p *ReadOnlyPager) IgnoreCheckConstraints() bool {
	return p != nil && p.pragmaState != nil && p.pragmaState.IgnoreCheckConstraints
}

// SetIgnoreCheckConstraints pushes the connection flag onto a pager opened
// straight from disk -- driver's Conn, whose autocommit reads never went
// through SnapshotPager. It allocates its own PragmaConnState rather than
// writing through a shared one, so it is only ever called on a pager the caller
// owns (never on a SnapshotPager, whose state is the writer's own struct).
func (p *ReadOnlyPager) SetIgnoreCheckConstraints(on bool) {
	if p.pragmaState == nil {
		if !on {
			return
		}
		p.pragmaState = &PragmaConnState{}
	}
	p.pragmaState.IgnoreCheckConstraints = on
}

// ---- PRAGMA query_only enforcement ----

// SetMaxSize is the driver's "PRAGMA max_size": a commit that would leave main's
// file (segments and delta) larger than n bytes fails with ErrDiskFull instead,
// and nothing of it is written. 0 is no limit. It is the connection's, not the
// file's -- the caller keeps it -- so the driver stamps it on every session it
// opens, as it does query_only. C's nearest is max_page_count, which this format
// declines (PragmaHasNoMeaningHere): its pages are not ours.
func (db *DB) SetMaxSize(n int64) { db.maxSize = n }

// MaxSize reports SetMaxSize's limit.
func (db *DB) MaxSize() int64 { return db.maxSize }

// ErrDiskFull is SQLITE_FULL, C's "database or disk is full" (ErrMsgDiskFull).
var ErrDiskFull = errors.New("engine: " + ErrMsgDiskFull)

// SetQueryOnly is the write side of "PRAGMA query_only" (see execPragma).
// QueryOnly reads it back; both exist so the driver, which owns this
// connection-level flag across its one-session-per-statement model, can push it
// into every session it opens (driver's Conn.queryOnly).
func (db *DB) SetQueryOnly(on bool) { db.queryOnly = on }

// QueryOnly reports the current "PRAGMA query_only" setting.
func (db *DB) QueryOnly() bool { return db.queryOnly }

// SetWriteLock refuses this session's writes exactly as "PRAGMA query_only"
// does, with reason as the error, until it is set back to "". Unlike the
// pragma it is not the SQL's to lift: the driver sets it on a connection that
// must not write the file at all (replication's databases take writes only
// through the connections replication made).
func (db *DB) SetWriteLock(reason string) { db.writeLock = reason }

// CaptureGuard is main's ConvertedCatalog.CaptureGuard: empty, or the error a
// write from a connection that does not capture its changes gets.
func (db *DB) CaptureGuard() string { return db.captureGuard }

// SetCaptureGuard sets main's capture guard to reason; the commit records it in
// the file. Enforcing it is the driver's job, since only the driver knows
// whether a connection captures (driver.Conn.SetCaptureHooks).
func (db *DB) SetCaptureGuard(reason string) { db.captureGuard = reason }

// refusedWrite is the error for a write the session refuses: the write lock's
// reason, or C's query_only message.
func (db *DB) refusedWrite() error {
	if db.writeLock != "" && !db.queryOnly {
		return errors.New(db.writeLock)
	}
	return errQueryOnlyWrite()
}

// errQueryOnlyWrite is C SQLite's own message for a write refused by
// "PRAGMA query_only": SQLITE_READONLY, raised from OP_Transaction ("Writes
// prohibited by the PRAGMA query_only=TRUE statement").
func errQueryOnlyWrite() error {
	return fmt.Errorf("engine: attempt to write a readonly database")
}

// queryOnlyRefusesWrite reports the error a statement gets under query_only. C
// checks in OP_Transaction when the write flag is set, so a statement is refused
// iff it asks for a write transaction, at run time (a compile error still wins).
// Measured:
//
//	refused: INSERT/UPDATE/DELETE (including "WHERE 0", which still opens the
//	         transaction), CREATE/DROP/ALTER of anything, CREATE TEMP TABLE and
//	         any write to a temp table, VACUUM, ANALYZE, REINDEX (every form),
//	         BEGIN IMMEDIATE, BEGIN EXCLUSIVE,
//	         PRAGMA user_version=N, PRAGMA schema_version=N,
//	         PRAGMA application_id=N, PRAGMA incremental_vacuum
//	allowed: SELECT, BEGIN, BEGIN DEFERRED, COMMIT, ROLLBACK, SAVEPOINT,
//	         RELEASE, ATTACH, DETACH, DROP TABLE IF EXISTS <nonexistent>,
//	         CREATE TABLE IF NOT EXISTS <existing>, PRAGMA integrity_check,
//	         PRAGMA optimize, PRAGMA wal_checkpoint, PRAGMA journal_mode=<any>,
//	         PRAGMA max_page_count (get AND set),
//	         PRAGMA auto_vacuum=N, PRAGMA secure_delete, PRAGMA locking_mode,
//	         PRAGMA writable_schema, PRAGMA foreign_keys, and every tuning
//	         pragma -- plus PRAGMA query_only=0 itself
//
// Deliberate over-refusals (safe direction): IF EXISTS / IF NOT EXISTS no-ops
// and REINDEX with nothing to rebuild, which C allows but this token-level gate
// cannot tell apart without the schema.
func (db *DB) queryOnlyRefusesWrite(sqlText string) error {
	if !db.queryOnly && db.writeLock == "" {
		return nil
	}
	trimmed := strings.TrimSpace(sqlText)
	verb, ok := LeadingStatementVerb(trimmed)
	if !ok {
		return nil // does not even lex: let the parser report it
	}
	switch verb {
	case "SELECT", "VALUES", "EXPLAIN",
		"COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE", "ATTACH", "DETACH":
		return nil
	case "BEGIN":
		// A plain (DEFERRED) BEGIN opens no transaction of any kind yet;
		// IMMEDIATE and EXCLUSIVE both ask for a write one straight away.
		up := strings.ToUpper(trimmed)
		if strings.Contains(up, "IMMEDIATE") || strings.Contains(up, "EXCLUSIVE") {
			return db.refusedWrite()
		}
		return nil
	case "PRAGMA":
		stmt, perr := ParsePragma(trimmed)
		if perr != nil {
			return nil // let ParsePragma's own error surface
		}
		switch stmt.Name {
		case "user_version", "schema_version", "application_id":
			if stmt.HasValue {
				return db.refusedWrite()
			}
		case "incremental_vacuum":
			return db.refusedWrite()
		}
		return nil
	}
	return db.refusedWrite()
}

// pragmaTransactionRestriction declines the tuning setters C refuses inside an
// open transaction. synchronous is refused in any open transaction ("Safety
// level may not be changed inside a transaction"). temp_store is refused only
// once the temp database has been opened, even if its objects were since
// dropped; that state is DB.tempDatabaseOpened, so temp_store goes to
// r35aTempStoreSetter. Getters are always fine.
func pragmaTransactionRestriction(db *DB, stmt *PragmaStmt) error {
	if !stmt.HasValue {
		return nil
	}
	if stmt.Name == "temp_store" {
		return db.r35aTempStoreSetter(stmt)
	}
	if !db.inTransaction() {
		return nil
	}
	return pragmaSetterInTransactionError(stmt.Name)
}

// r35aTempStoreSetter is "PRAGMA temp_store = <v>", ported from pragma.c's
// changeTempStorage/invalidateTempStorage:
//
//   - the value already in force: no-op;
//   - temp database not open: no-op, even inside a transaction;
//   - open, inside a transaction: "temporary storage cannot be changed from
//     within a transaction";
//   - open, in autocommit: the temp database is closed and every temp object
//     goes with it (DropTempObjects, temp_schema.go), along with its journal
//     mode and sqlite_sequence state. Only schemaGen is bumped, not main's
//     schema cookie.
//
// Still declined: sqlite3ResetAllSchemasOfConnection also reloads main's
// schema, which over an outstanding direct sqlite_master write would need a
// writable_schema=RESET style reload.
func (db *DB) r35aTempStoreSetter(stmt *PragmaStmt) error {
	ts := r35aTempStoreValue(stmt.ValueText)
	if ts == db.tempStore {
		return nil // (1)
	}
	// C refuses only once the temp database is open (invalidateTempStorage's
	// "db->aDb[1].pBt != 0"), so check that first.
	if !db.tempDatabaseOpened {
		db.tempStore = ts // (0)
		return nil
	}
	if db.inTransaction() {
		return pragmaSetterInTransactionError("temp_store") // (2)
	}
	if db.writableSchemaEditsActive() {
		return fmt.Errorf("%w: PRAGMA temp_store=%s while this session holds a direct sqlite_master write (invalidateTempStorage calls sqlite3ResetAllSchemasOfConnection, which RELOADS main's schema out of the edited page 1 -- the same reload PRAGMA writable_schema=RESET performs, and this engine has none)", errVDBEUnsupported, stmt.ValueText)
	}
	db.DropTempObjects() // (3)
	db.tempDatabaseHeldObject = false
	db.tempJournalMode = ""
	db.tempStore = ts
	db.tempDatabaseOpened = false
	return nil
}

// pragmaSetterInTransactionError is the rule itself, split out so the two
// callers cannot drift: this path, and PragmaTuningResult, which answers
// synchronous from connection state and so never reaches execPragma.
func pragmaSetterInTransactionError(name string) error {
	switch name {
	case "synchronous":
		return fmt.Errorf("engine: Safety level may not be changed inside a transaction")
	case "temp_store":
		return fmt.Errorf("engine: temporary storage cannot be changed from within a transaction")
	}
	return nil
}

// pragmaValueSpellingSupported rejects a value spelling C's grammar rejects
// ("nmnum ::= plus_num | nm | ON | DELETE | DEFAULT"), so a no-op accept never
// accepts what C refuses. Rejected: nonIdentifierKeywords (sql_parser.go) minus
// those three; KEY and ABORT are still identifiers, and any quoted spelling is
// an nm.
//
// PragmaStmt records only whether the value was a string literal, so a quoted
// identifier like `"null"` is declined though C accepts it; over-declining is
// the safe direction.
func pragmaValueSpellingSupported(stmt *PragmaStmt) error {
	if !stmt.HasValue || stmt.ValueIsString {
		return nil
	}
	up := strings.ToUpper(stmt.ValueText)
	switch up {
	case "ON", "DELETE", "DEFAULT":
		return nil
	}
	if nonIdentifierKeywords[up] {
		return fmt.Errorf("engine: PRAGMA %s=%s: %s is a reserved word and not a usable pragma value (C SQLite's grammar admits a signed number, an identifier or string, or ON/DELETE/DEFAULT)",
			stmt.Name, stmt.ValueText, stmt.ValueText)
	}
	return nil
}

// execPragmaSafeNoop is the set of pragma names whose Exec form is accepted as a
// no-op: none affects any other statement's rows or errors.
//
// Each tuning entry was checked with a fixed battery run on C with and without
// the pragma, but "same output" is necessary, not sufficient: every name here
// affects only fsync policy, memory, threading, file layout or diagnostics.
// "Affects only the plan" is not on that list; porting more of the planner can
// make a plan pragma observable (automatic_index was).
//
// Deliberately absent: anything that changes whether a statement errors or what
// it returns, e.g. ignore_check_constraints, defer_foreign_keys, query_only,
// max_page_count, analysis_limit, optimize, hard_heap_limit, cell_size_check,
// temp_store_directory (C errors on an unusable directory). Several of those are
// implemented elsewhere.
var execPragmaSafeNoop = map[string]bool{
	"data_version":   true,
	"collation_list": true,
	// pragma_list is a pure REPORTER over a compile-time list, exactly like
	// collation_list: its rows come from queryPragmaStmt (pragmaListRows,
	// pragma_unknown.go) and are discarded here. pragma.c's
	// PragTyp_PRAGMA_LIST arm touches no state and never reads zRight, so
	// there is nothing for the exec path to do or to refuse.
	"pragma_list": true,
	// function_list is the same: a pure REPORTER whose rows come from
	// queryPragmaStmt (pragmaFunctionListValueRows, pragma_function_list.go)
	// and are discarded here. PragTyp_FUNCTION_LIST touches no state and never
	// reads zRight.
	"function_list":    true,
	"page_size":        true,
	"cache_size":       true,
	"table_info":       true,
	"table_xinfo":      true,
	"index_list":       true,
	"index_info":       true,
	"index_xinfo":      true,
	"foreign_key_list": true,
	// table_list is a pure reporter. Its Exec form cannot error for anything the
	// query side declines; its real errors (too many arguments, unknown schema) are
	// caught before this map is consulted.
	"table_list": true,

	// database_list is a pure reporter, so Exec discards its rows. The query side
	// still declines: the temp row is listed once the temp database has been opened,
	// even after all temp objects are dropped, which the schema alone cannot tell.
	// A qualifier is ignored in C.
	"database_list": true,
}

// execPragmaTuningNoop is the session-tuning half of the no-op set, kept apart
// because these (synchronous, cache_spill, journal_size_limit, mmap_size,
// default_cache_size) are per-database in C, so only the unqualified form is
// accepted (TestAttachedPragmaAcceptDeclineSplit).
var execPragmaTuningNoop = map[string]bool{
	// fsync policy and journal/WAL file handling -- durability, never results.
	// fullfsync and checkpoint_fullfsync have MOVED out of this map: they are
	// still accepted and still inert, but their VALUE is now tracked and reported
	// back (pragmaInertConnFlagNames, pragma_tuning.go), because a no-op accept
	// left the getter answering zero rows where C SQLite answers a row.
	"synchronous":        true,
	"journal_size_limit": true,

	// Memory and cache sizing.
	"default_cache_size": true,
	"shrink_memory":      true,
	"mmap_size":          true,

	// Where temporary material lives -- memory versus a file. The rows are
	// identical either way (verified for both MEMORY and FILE).
	"temp_store": true,

	// Threading. read_uncommitted, busy_timeout, threads and soft_heap_limit are
	// served elsewhere (pragmaInertConnFlagNames, pragmaBusyTimeout,
	// pragma_resource_limits.go) because C answers their getters with a row.

	// No-ops in C itself: legacy_file_format does nothing, vdbe_* tracing only
	// works in SQLITE_DEBUG builds, multiplex_truncate needs a VFS shim that is not
	// loaded.
	"legacy_file_format": true,
	"vdbe_listing":       true,
	"vdbe_trace":         true,
	"vdbe_addoptrace":    true,
	"vdbe_debug":         true,
	"parser_trace":       true,
	"multiplex_truncate": true,

	// A pure REPORTER with no state of its own: its rows are discarded on the
	// exec path exactly as table_info's are, and its query form stays declined.
	"compile_options": true,
}

// ---- "PRAGMA <attached>.<name>": which database answers ----

// pragmaScope is what a database qualifier naming an ATTACHed database means
// for one pragma name. The three values are a partition of every pragma name,
// and each membership below was pinned by RUNNING the statement against
// mattn/go-sqlite3 (3.53.3) with an aux database attached, never reasoned out.
type pragmaScope int

const (
	// pragmaScopeRouted: the pragma is about the database itself and the qualifier
	// picks which one. Delegated to that attachment's own session with the
	// qualifier stripped (attach_write.go).
	pragmaScopeRouted pragmaScope = iota

	// pragmaScopeConnection: C ignores the qualifier because the setting is per
	// connection. The execPragmaSafeNoop names ride here too, since this engine
	// keeps no per-database state for them.
	pragmaScopeConnection

	// pragmaScopeDeclined: everything else, mostly names already declined
	// unqualified. "encoding" is the exception: a qualified encoding assignment is a
	// silent no-op in C even on an empty attached database, which neither routing
	// nor answering reproduces.
	pragmaScopeDeclined
)

// attachedPragmaScope classifies name for a qualifier that names an ATTACHed
// database. See pragmaScope's constants for the oracle evidence behind each
// bucket; the routed set is exactly the names whose execPragma case does
// something to -- or validates against -- the database it is asked about.
func attachedPragmaScope(name string) pragmaScope {
	switch name {
	case "user_version", "schema_version", "journal_mode", "auto_vacuum",
		"secure_delete", "wal_checkpoint", "wal_autocheckpoint",
		"foreign_key_check", "locking_mode", "integrity_check", "quick_check",
		// application_id is header state exactly as user_version is, and the
		// qualifier picks the file: with aux attached, "PRAGMA
		// aux.application_id=42" reads back 42 for aux and leaves main at 0
		// (verified against mattn/go-sqlite3 3.53.3).
		"application_id",
		// max_page_count lives in the PAGER, so the qualifier picks the file:
		// with main's ceiling at 9, "PRAGMA aux.max_page_count" answers
		// 4294967294 and "PRAGMA aux.max_page_count = 4" answers 4 while
		// "PRAGMA main.max_page_count" still answers 9 -- and it is aux's own 4
		// that then refuses "CREATE TABLE aux.q4(z)" with "database or disk is
		// full" (verified against mattn/go-sqlite3 3.53.3).
		"max_page_count":
		return pragmaScopeRouted
	case "foreign_keys", "writable_schema", "case_sensitive_like",
		"full_column_names", "short_column_names", "recursive_triggers",
		"query_only", "defer_foreign_keys", "trusted_schema",
		// automatic_index is named here explicitly now that it is really
		// implemented; it used to reach pragmaScopeConnection through the
		// execPragmaSafeNoop fallthrough below. Same verified rule as the rest of
		// this list -- it is a db->flags bit, so the qualifier is ignored.
		"automatic_index",
		// Named per-database by C SQLite but backed by nothing here, so the
		// qualifier selects between two identical nothings: incremental_vacuum
		// is a no-op (this format keeps no freelist), lock_status reports
		// nothing, and page_count/freelist_count are getters whose rows an Exec
		// discards.
		"incremental_vacuum", "lock_status", "page_count", "freelist_count":
		return pragmaScopeConnection
	}
	if execPragmaSafeNoop[name] {
		return pragmaScopeConnection
	}
	return pragmaScopeDeclined
}

// pragmaReadsHeader reports whether a pragma answers out of a database's file
// header. The temp database has its own (temp_store.go), so "temp." reads that.
func pragmaReadsHeader(name string) bool {
	switch name {
	case "user_version", "application_id", "schema_version", "page_count",
		"freelist_count", "auto_vacuum", "data_version", "page_size",
		"encoding", "max_page_count":
		return true
	}
	return false
}

func pragmaReadsSchema(name string) bool {
	switch name {
	case "table_list", "integrity_check", "quick_check", "foreign_key_check":
		return true
	}
	return pragmaObjectScoped[name]
}

// ---- read side: schema introspection + header-backed scalars ----

// boolToInt renders a Go bool as the 0/1 Value SQLite itself uses for a
// boolean-shaped pragma result column (notnull, unique, partial, desc, key).
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// queryPragma implements ReadOnlyPager.QueryArgs's PRAGMA case: an implemented
// pragma or a clean decline.
//
// data_version is always 1: every connection this engine hands out has observed
// no other connection's commit, which is what C reports for a fresh connection.
func (p *ReadOnlyPager) queryPragma(sqlText string) (cols []string, rows [][]Value, err error) {
	stmt, err := ParsePragma(sqlText)
	if err != nil {
		return nil, nil, err
	}
	return p.queryPragmaStmt(stmt)
}

// queryPragmaStmt is queryPragma over an ALREADY-PARSED statement, so a caller
// that has the pieces rather than the text can reach the identical
// implementation instead of re-serializing them into SQL and re-parsing. The
// eponymous "pragma_<name>(...)" table-valued functions (vtab_pragma.go) are
// that caller: routing them through here is what makes a pragma TVF's rows
// identical to the bare PRAGMA's by construction, rather than by two
// implementations agreeing.
func (p *ReadOnlyPager) queryPragmaStmt(stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	// See execPragma's identical check: an unattached schema qualifier is an
	// error, not a no-op.
	if stmt.Schema != "" {
		// "PRAGMA temp.<name>" asks the TEMP database, which is a database of
		// its own now (temp_store.go): anything that reads a SCHEMA -- an
		// object's columns, its indexes, the file's own counters -- is answered
		// by that pager. A CONNECTION-level pragma ignores the qualifier
		// entirely (attachedPragmaScope), so it stays here.
		if isTempSchemaQualifier(stmt.Schema) && (pragmaReadsSchema(stmt.Name) || pragmaReadsHeader(stmt.Name)) {
			if owner, found := p.attachedReaderNamed("temp"); found {
				// The qualifier RIDES ALONG: on that pager "temp" is its own
				// local schema, so the lookup stays scoped to the temp catalog
				// instead of falling through to main (which the temp pager can
				// also reach -- see SetAttachedReaders). "PRAGMA
				// temp.table_info(m)" over a MAIN-only m must answer zero rows,
				// which is what C SQLite does.
				return owner.queryPragmaStmt(stmt)
			}
		}
		ok, qerr := p.qualifierResolvesLocally(stmt.Schema)
		if qerr != nil {
			return nil, nil, qerr
		}
		if !ok && isTempSchemaQualifier(stmt.Schema) {
			// A "temp." qualifier that is NOT one of the routed classes above
			// is a CONNECTION-level pragma (journal_mode, locking_mode, the
			// tuning getters): C SQLite answers those from the connection
			// whatever the qualifier says, and this pager is where that state
			// is. Answering here is what it did before the TEMP database
			// became a reader of its own.
			ok = true
		}
		if !ok {
			// An attached qualifier routes to that database's pager only for table_list and
			// foreign_key_check (fkey5.test 13.2), which then resolves locally there. Other
			// names answer from connection state an attached pager does not mirror, so they
			// keep declining (pragmaRoutableToAttached).
			if pragmaRoutableToAttached[stmt.Name] {
				if owner, found := p.attachedReaderNamed(stmt.Schema); found {
					return owner.queryPragmaStmt(stmt)
				}
			}
			return nil, nil, fmt.Errorf("engine: unknown database %s", stmt.Schema)
		}
	}
	// A name C SQLite does not recognize answers ZERO COLUMNS and zero rows
	// there, whatever the qualifier -- see pragma_unknown.go, and execPragma's
	// identical placement: after the qualifier check, before every name-keyed
	// rule, because those all presuppose a pragma that exists.
	if unknownPragmaIsNoop(stmt.Name) {
		return nil, nil, nil
	}
	if stmt.Schema != "" && !tempPragmaAnswersMain(stmt.Name) && !equalFoldName(p.localSchema, "temp") {
		// The temp database's own page_count/freelist_count ARE answerable
		// while it has never held an object -- both are 0 there. See
		// r35aEmptyTempPragmaResult (temp_schema.go).
		if cols, rows, ok := p.r35aEmptyTempPragmaResult(stmt); ok {
			return cols, rows, nil
		}
		if err := declineFileScopedTempPragma(stmt.Schema, stmt.Name); err != nil {
			return nil, nil, err
		}
	}
	// An object-scoped pragma resolves its argument across every attached database,
	// main first, as C does; when the object is not here, the pragma runs against its
	// owner. "Not here", not "empty answer" (pragmaObjectOwner, attach_write.go).
	// Covers both PRAGMA and the pragma_*() table-valued forms.
	if stmt.Schema == "" && stmt.HasValue && pragmaReadsSchema(stmt.Name) &&
		!isMasterSchemaCatalogAlias(stmt.ValueText) {
		// sqlite_master / sqlite_temp_master name a catalog every pager answers to, so
		// they are not routed; pragmaTableListOne tags them main / temp. Everything else
		// resolves temp first, C's order for an unqualified name (build.c:352).
		if owner, found := p.attachedReaderNamed("temp"); found && owner.pagerHasName(stmt.ValueText) {
			return owner.queryPragmaStmt(stmt)
		}
		if owner := p.pragmaObjectOwner(stmt.ValueText); owner != nil {
			return owner.queryPragmaStmt(stmt)
		}
	}
	// A "main."/"temp." qualifier scopes the object lookup to that catalog,
	// exactly like it does on a FROM item: "PRAGMA main.table_info(tt)" over a
	// temp-only tt answers ZERO rows while the unqualified form (temp first)
	// answers its columns -- verified directly. See temp_schema.go.
	scope, _ := scopeOfQualifier(stmt.Schema)
	switch stmt.Name {
	case "table_info":
		return p.pragmaTableInfo(scope, stmt.ValueText, false)
	case "table_xinfo":
		return p.pragmaTableInfo(scope, stmt.ValueText, true)
	case "index_list":
		return p.pragmaIndexList(scope, stmt.ValueText)
	case "index_info":
		return p.pragmaIndexInfo(scope, stmt.ValueText, false)
	case "index_xinfo":
		return p.pragmaIndexInfo(scope, stmt.ValueText, true)
	case "foreign_key_list":
		return p.pragmaForeignKeyList(scope, stmt.ValueText)
	case "database_list":
		return p.pragmaDatabaseList()
	case "foreign_key_check":
		return p.pragmaForeignKeyCheck(scope, stmt)
	case "table_list":
		return p.pragmaTableList(scope, stmt)
	case "user_version":
		// SIGNED: C SQLite renders this 32-bit header field as an int32, so
		// "PRAGMA user_version=-1" reads back -1 and not 4294967295 (verified
		// against mattn/go-sqlite3 3.53.3). See headerScalarValue for the
		// setter's half of the same rule.
		return []string{"user_version"}, [][]Value{{{Typ: Int, I: int64(int32(p.meta.userVersion))}}}, nil
	case "schema_version":
		// Signed for the same reason user_version is, and verified the same
		// way: "PRAGMA schema_version=-1" reads back -1.
		return []string{"schema_version"}, [][]Value{{{Typ: Int, I: int64(int32(p.meta.schemaCookie))}}}, nil
	case "application_id":
		// Read back signed, as C does. An assignment is declined here and left to the
		// write path: the setter answers zero rows.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA application_id=%s on the read side (the assignment is a write-path statement, and answers no rows there -- see execPragma)", errVDBEUnsupported, stmt.ValueText)
		}
		return []string{"application_id"}, [][]Value{{{Typ: Int, I: int64(int32(p.meta.applicationID))}}}, nil
	case "collation_list":
		// The collations this engine implements (knownCollations), newest first, as a
		// default C build lists them: RTRIM, NOCASE, BINARY. No setter.
		return []string{"seq", "name"}, [][]Value{
			{{Typ: Int, I: 0}, {Typ: Text, S: []byte("RTRIM")}},
			{{Typ: Int, I: 1}, {Typ: Text, S: []byte("NOCASE")}},
			{{Typ: Int, I: 2}, {Typ: Text, S: []byte("BINARY")}},
		}, nil
	case "pragma_list":
		// The list of pragma names this engine's own acceptance rule is keyed
		// on -- see pragmaListRows (pragma_unknown.go) for the port and for why
		// this one enumerate-a-feature-set pragma is answerable. There is no
		// setter: pragma.c gives pragma_list only PragFlg_Result0, and its arm
		// never reads zRight at all -- "PRAGMA pragma_list=anything" still
		// answers the whole list -- so an argument is IGNORED here for the same
		// reason, not declined.
		cols, rows := pragmaListRows()
		return cols, rows, nil
	case "function_list":
		// The same rows as the pragma_function_list table-valued form (one
		// implementation in C, pragma.c:1462); see pragma_function_list.go. An argument
		// is ignored. module_list stays declined: C's order comes from a hash.
		return pragmaFunctionListCols, pragmaFunctionListValueRows(), nil
	case "full_column_names", "short_column_names":
		// Getter from the snapshot's flags (colNameMode, which the setter's effect also
		// reads). One INTEGER row named after the pragma; a fresh connection has
		// short=1, full=0. An assignment is routed to the write path: a read snapshot
		// cannot record connection state.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA %s=%s on the read side (the assignment is a write-path statement -- see execPragma)", errVDBEUnsupported, stmt.Name, stmt.ValueText)
		}
		mode := p.colNameMode()
		on := mode.short
		if stmt.Name == "full_column_names" {
			on = mode.full
		}
		return []string{stmt.Name}, [][]Value{{{Typ: Int, I: boolToInt64(on)}}}, nil
	case "data_version":
		return []string{"data_version"}, [][]Value{{{Typ: Int, I: 1}}}, nil
	case "legacy_alter_table":
		// The GETTER, answered from the flag this snapshot was taken with (the
		// driver pushes it in -- SetLegacyAlterTable). An ASSIGNMENT is declined
		// here for the reason secure_delete's is: a read snapshot has nowhere to
		// record it, and accepting it would drop the write.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA legacy_alter_table=%s on the read side (the assignment is a write-path statement -- see execPragma)", errVDBEUnsupported, stmt.ValueText)
		}
		on := int64(0)
		if p.pragmaState != nil && p.pragmaState.LegacyAlterTable {
			on = 1
		}
		return []string{stmt.Name}, [][]Value{{{Typ: Int, I: on}}}, nil
	case "ignore_check_constraints":
		// Getter from the snapshot's flag. A setter to the current value is answerable
		// (empty result); one that would change it is declined, since a read snapshot
		// is immutable. The driver answers both forms from Conn state.
		if stmt.HasValue {
			b := pragmaGetBoolean(stmt.ValueText, false)
			if b != p.IgnoreCheckConstraints() {
				return nil, nil, fmt.Errorf("%w: PRAGMA ignore_check_constraints=%s on the read side (a read snapshot cannot record connection state; run it through the write path)", errVDBEUnsupported, stmt.ValueText)
			}
			return nil, nil, nil
		}
		return []string{stmt.Name}, [][]Value{{{Typ: Int, I: boolToInt64(p.IgnoreCheckConstraints())}}}, nil
	case "foreign_keys":
		return []string{"foreign_keys"}, [][]Value{{{Typ: Int, I: boolToInt64(p.foreignKeys)}}}, nil
	case "automatic_index":
		// Getter: one INTEGER row "automatic_index", 1 on a fresh connection. An
		// assignment is routed to the write path.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA automatic_index=%s on the read side (the assignment is a write-path statement, and answers no rows there -- see execPragma)", errVDBEUnsupported, stmt.ValueText)
		}
		return []string{"automatic_index"}, [][]Value{{{Typ: Int, I: boolToInt64(p.AutomaticIndex())}}}, nil
	case "temp_store":
		// Getter: one INTEGER row "temp_store", 0 by default (pragma.c getTempStore: 0
		// default, 1 file, 2 memory). An assignment is routed to the write path, which
		// may discard the temp database.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA temp_store=%s on the read side (the assignment is a write-path statement -- see r35aTempStoreSetter)", errVDBEUnsupported, stmt.ValueText)
		}
		return []string{"temp_store"}, [][]Value{{{Typ: Int, I: int64(p.tempStore)}}}, nil
	case "trusted_schema":
		// Getter from the snapshot's flag; per connection, so a qualifier needs no
		// scope. An assignment is routed to the write path (the setter returns zero
		// rows).
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA trusted_schema=%s on the read side (the assignment is a write-path statement, and answers no rows there -- see execPragma)", errVDBEUnsupported, stmt.ValueText)
		}
		return []string{"trusted_schema"}, [][]Value{{{Typ: Int, I: boolToInt64(p.TrustedSchema())}}}, nil
	case "defer_foreign_keys":
		// The GETTER reports this snapshot's own flag, stamped from the
		// connection (SnapshotPager) or pushed in by a driver that opens a
		// fresh read session per statement. An ASSIGNMENT is a write-path
		// statement and answers no rows, so it is declined here and routed --
		// exactly as trusted_schema's is.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA defer_foreign_keys=%s on the read side (the assignment is a write-path statement, and answers no rows there -- see execPragma)", errVDBEUnsupported, stmt.ValueText)
		}
		return []string{"defer_foreign_keys"}, [][]Value{{{Typ: Int, I: boolToInt64(p.DeferForeignKeys())}}}, nil
	case "page_size":
		return []string{"page_size"}, [][]Value{{{Typ: Int, I: int64(p.meta.pageSize)}}}, nil
	case "encoding":
		return []string{"encoding"}, [][]Value{{{Typ: Text, S: []byte(textEncodingName(p.meta.encoding))}}}, nil
	case "integrity_check", "quick_check":
		// A bare integer is an error cap; anything else names an object that must
		// exist ("no such table: nope"), matching execPragma.
		if err := p.checkIntegrityCheckTarget(scope, stmt); err != nil {
			return nil, nil, err
		}
		// The per-row findings, then the virtual tables', on one shared
		// error-count budget -- see integrityCheckViolations' doc comment for
		// why the cap is shared.
		maxErr, only := integrityCheckArgs(stmt)
		// A WHOLE-database check over a catalog with an ALIAS (two tables naming
		// one storage) answers in PAGES in C -- "2nd reference to page N" for
		// every page of the shared tree, "Page N: never used" for every page of
		// the orphaned one (btree.c's checkTreePage / checkRef) -- which depends on
		// C's layout of both trees. Declined; the single-object form, which skips
		// page coverage (btree.c:11144-11170), is served.
		if only == "" && p.segs != nil {
			for _, r := range p.schemaRows {
				if r.AliasOf != "" {
					return nil, nil, fmt.Errorf("%w: whole-database PRAGMA %s over a table that shares another's storage is not reproducible against C SQLite on this format (C reports the shared and orphaned b-trees page by page)", errVDBEUnsupported, stmt.Name)
				}
			}
		}
		// C's STRUCTURAL half (OP_IntegrityCk, pragma.c:1783) checks pages,
		// which this format does not have: the whole budget goes to the rows.
		var rows [][]Value
		if remaining := maxErr; remaining > 0 {
			bad, cerr := p.integrityCheckViolations(scope, stmt, only, remaining)
			if cerr != nil {
				return nil, nil, cerr
			}
			rows = append(rows, bad...)
			// The virtual-table pass runs last, on what is left of the budget;
			// a row past it is never reached, C having halted (pragma.c:2186's
			// integrityCheckResultRow).
			if remaining -= len(bad); remaining > 0 {
				vrows, verr := p.integrityCheckRtrees(scope, only)
				if verr != nil {
					return nil, nil, verr
				}
				for _, v := range vrows[:min(len(vrows), remaining)] {
					rows = append(rows, []Value{v})
				}
			}
		}
		if len(rows) > 0 {
			return []string{stmt.Name}, rows, nil
		}
		return []string{stmt.Name}, [][]Value{{{Typ: Text, S: []byte("ok")}}}, nil
	case "journal_mode":
		if isTempSchemaQualifier(stmt.Schema) {
			return tempJournalModeResult(p.tempJournalMode, stmt)
		}
		// A ReadOnlyPager's database may be in WAL mode (its catalog records it)
		// -- see journalModeResult.
		return journalModeResult(p.inMemory, p.pragmaState.lockProxyOn(), p.journalMode(), stmt)
	case "locking_mode":
		return lockingModeResult(p.inMemory, p.lockingDefault, p.lockingMain, stmt)
	case "wal_autocheckpoint":
		// The value the DRIVER pushed in, or the built-in default when this
		// connection never set one. The setter itself is a write-session action
		// (see the exec path), which is why only the value arrives here.
		n := walDefaultAutoCheckpoint
		if p.walAutoCheckpointPlus1 > 0 {
			n = p.walAutoCheckpointPlus1 - 1
		}
		return []string{"wal_autocheckpoint"}, [][]Value{{{Typ: Int, I: int64(n)}}}, nil
	case "wal_checkpoint":
		// Read side: report state, never checkpoint. The driver runs the action
		// through a write session first. An unrecognized mode is not an error (see
		// walCheckpointModeOf). A non-empty log declines, as segmentWALCheckpoint does.
		isWAL := p.journalMode() == journalModeWAL
		if isWAL && segLogNonEmpty(p.mainPath) {
			return nil, nil, walCheckpointDecline(walCheckpointModeOf(stmt))
		}
		cols, rows := walCheckpointRows(isWAL)
		return cols, rows, nil
	case "lock_status":
		return lockStatusResult()
	case "auto_vacuum":
		// The mode this database records in its catalog
		// (ConvertedCatalog.AutoVacuumPlus1).
		return autoVacuumResult(stmt, p.segAutoVacuum)
	case "secure_delete":
		// The GETTER, answered from this connection's own value (the driver
		// pushes it in -- SetSecureDelete). An ASSIGNMENT is declined here for
		// the reason autoVacuumResult's is: a read snapshot cannot record it,
		// and accepting it would drop the write.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA secure_delete=%s on the read side (the assignment is a write-path statement -- see execPragma)", errVDBEUnsupported, stmt.ValueText)
		}
		st := p.pragmaState
		if st == nil {
			// A snapshot taken before any secure_delete statement carries no
			// state; the single value it did carry is main's.
			st = &PragmaConnState{SecureDeleteDefault: p.secureDelete}
		}
		cols, rows, err := SecureDeleteStateResult(stmt, st)
		return cols, rows, err
	case "incremental_vacuum":
		// Zero columns and zero rows: what C answers for a database with no
		// freelist pages to reclaim, which on this format is every one -- a
		// rewrite gives back everything churn retained (verified against
		// mattn/go-sqlite3 3.53.3: on an auto_vacuum=0 database it answers no
		// rows with or without an argument).
		return []string{}, [][]Value{}, nil
	case "page_count", "freelist_count":
		// Both are answered about this file: the segment and delta in their recorded
		// page size (segFilePages), and a freelist of zero, since a rewrite returns all
		// retained space and the delta is append-only. The numbers differ from C's for
		// the same data, which is an implementation detail like rootpage; the harness
		// blanks them on both sides.
		var n int64
		if stmt.Name == "page_count" && p.segs != nil {
			n = int64(p.segFilePages())
		}
		return []string{stmt.Name}, [][]Value{{{Typ: Int, I: n}}}, nil
	case "max_page_count":
		// The getter is OP_MaxPgcnt with 0, which reports the pager's mxPgno
		// (pragma.c:671-677): SQLITE_MAX_PAGE_COUNT (sqliteLimit.h:249) until a
		// setter lowers it -- and a setter is declined on this format, so that
		// default is always the answer.
		if stmt.HasValue {
			return nil, nil, PragmaHasNoMeaningHere(stmt.Name)
		}
		return []string{"max_page_count"}, [][]Value{{{Typ: Int, I: 0xfffffffe}}}, nil
	case "writable_schema":
		// Getter from the snapshot's flag. A setter is declined: the snapshot cannot
		// record it. The exec side owns the flag.
		if stmt.HasValue {
			return nil, nil, fmt.Errorf("%w: PRAGMA writable_schema=%s on the read side (a read snapshot cannot record connection state; run it through the write path)", errVDBEUnsupported, stmt.ValueText)
		}
		cols, rows, _, err := writableSchemaResult(stmt, p.writableSchema)
		return cols, rows, err
	default:
		// The tuning pragmas' getters answer from the values SnapshotPager
		// carried over from the writer -- see queryTuningPragma.
		if cols, rows, handled, terr := p.queryTuningPragma(stmt); handled {
			return cols, rows, terr
		}
		// An unrecognized name answers ZERO COLUMNS and zero rows there, not an
		// error -- see pragma_unknown.go. Nil cols, not an empty slice: that is
		// what "PRAGMA bogus" reports on the oracle.
		if unknownPragmaIsNoop(stmt.Name) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("engine: unsupported PRAGMA %s", stmt.Name)
	}
}

// SetInMemory marks this pager's database as having no durable backing of its
// own -- a ":memory:"/"mode=memory" DSN, or an ATTACH ':memory:'. The
// driver Conn calls it right after opening the temp FILE it backs such a
// database with (driver/memdb.go, driver/attach.go), exactly as it
// calls SetLocalSchema for an attached database's name: this engine has no
// memory-backed pager, so nothing about the file on disk distinguishes one,
// and only the opener knows. See journalModeResult, the only reader.
func (p *ReadOnlyPager) SetInMemory(v bool) { p.inMemory = v }

// SetInMemory is the write-path counterpart of ReadOnlyPager.SetInMemory.
func (db *DB) SetInMemory(v bool) { db.inMemory = v }

// SetTextEncoding pins the encoding a write session should stamp into the file
// it creates. It exists for the same reason SetForeignKeys does: a driver whose
// autocommit model opens a fresh session per statement owns the state that
// "PRAGMA encoding" set on an earlier one (driver's Conn.textEncoding).
// OpenWrite reads the encoding out of the header instead and ignores this.
func (db *DB) SetTextEncoding(enc TextEncoding) { db.textEncoding = enc }

// TextEncoding reports this session's text encoding.
func (db *DB) TextEncoding() TextEncoding { return db.encoding() }

// SetForeignKeys carries the connection's "PRAGMA foreign_keys" setting onto a
// read-only view, so its GETTER reports what the connection set (enforcement
// itself is a write-path concern -- see fk.go). Needed because a driver whose
// autocommit model opens a fresh session per statement owns that state itself
// (driver's Conn.foreignKeys).
func (p *ReadOnlyPager) SetForeignKeys(v bool) { p.foreignKeys = v }

// SetWritableSchema carries the connection's "PRAGMA writable_schema" flag onto
// a write session / a read-only view, for exactly the reason SetForeignKeys
// exists: a driver whose autocommit model opens a fresh session per statement
// owns that connection state itself (driver's Conn.writableSchema), and
// both the flag's own getter and the sqlite_master write path
// (schema_write_direct.go) have to see it.
// SetWritableSchema sets this DB's "PRAGMA writable_schema" flag.
func (db *DB) SetWritableSchema(v bool) {
	db.writableSchema = v
	db.schemaCorruptGen++ // see execPragma's own note on why
}

// SetWritableSchema is SetWritableSchema's read-snapshot half; see above.
func (p *ReadOnlyPager) SetWritableSchema(v bool) { p.writableSchema = v }

// PragmaWritableSchemaValue exposes writableSchemaValue's parse to driver,
// which answers this pragma's setter form at the CONNECTION level. Reported
// separately from PragmaBooleanValue because "reset" is a spelling only this
// pragma has (it means OFF plus a schema reload).
func PragmaWritableSchemaValue(text string) (on, isReset bool) {
	return writableSchemaValue(text)
}

// SetWalAutoCheckpoint records this connection's "PRAGMA wal_autocheckpoint"
// threshold on a read snapshot, so its GETTER can be answered here. The setter
// runs on a write session that this driver throws away after one statement, so
// without this the getter would always report the built-in default.
func (p *ReadOnlyPager) SetWalAutoCheckpoint(n int) {
	if n < 0 {
		n = 0
	}
	p.walAutoCheckpointPlus1 = n + 1
}

// Every journal mode name SQLite knows. All but "off" are modes this engine can
// truthfully report and really be in; "off" is listed because journalModeResult
// and execJournalMode must tell "a real mode this engine declines" apart from
// "a name SQLite does not recognize at all", which behave differently -- an
// unrecognized name is silently ignored, a declined one is an error.
const (
	journalModeDelete   = "delete"
	journalModeWAL      = "wal"
	journalModeMemory   = "memory"
	journalModeOff      = "off"
	journalModeTruncate = "truncate"
	journalModePersist  = "persist"

	lockingModeNormal    = "normal"
	lockingModeExclusive = "exclusive"
)

var journalModeNames = map[string]bool{
	journalModeDelete: true, journalModeWAL: true,
	journalModeMemory: true, journalModeOff: true,
	journalModeTruncate: true, journalModePersist: true,
}

// journalModeResult implements PRAGMA [db.]journal_mode, getter and setter,
// against cur, the mode in effect. C answers one TEXT row "journal_mode" with
// the resulting mode in lower case and never errors: an unrecognized name is
// ignored.
//
// The mode is connection state, so cur is a parameter. On this read side a
// setter is answerable only when it cannot change the mode: an unrecognized
// name, "= <cur>", or on an in-memory database anything other than memory/off,
// which C ignores. Everything else is routed to the write path
// (execJournalMode).
func journalModeResult(inMemory bool, lockProxyOn bool, cur string, stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	if stmt.HasValue {
		want := strings.ToLower(strings.TrimSpace(stmt.ValueText))
		switch {
		case !journalModeNames[want], want == cur:
			// A no-op in C SQLite, so echoing cur back is not a guess --
			// it is the whole behavior.
		case lockProxyOn && want == journalModeWAL:
			// Proxy locking disables WAL silently: its IO methods have no xShmMap
			// (os_unix.c:5936), so sqlite3PagerWalSupported is false (pager.c:7595) and
			// OP_JournalMode keeps the old mode (vdbe.c:8087). C still allows WAL
			// in exclusive locking mode, which this check does not model. lock_proxy_file is darwin-only, so LockProxyOn is false
			// elsewhere.
		case inMemory && want != journalModeOff:
			// Memory-BACKED: unreachable mode, silently ignored. ("=memory"
			// itself already matched want == cur above.) The test is inMemory
			// rather than cur == "memory" because a FILE-backed connection can
			// now be in memory mode too, and there every other mode IS reachable.
		default:
			return nil, nil, fmt.Errorf("engine: unsupported PRAGMA journal_mode=%s over a read-only snapshot (this database is in %q mode; a mode change is a write and belongs on the write path -- see execJournalMode)", want, cur)
		}
	}
	return []string{"journal_mode"}, [][]Value{{{Typ: Text, S: []byte(cur)}}}, nil
}

// tempStoreAsksForMemory reports whether a "PRAGMA temp_store" value asks for a
// memory-backed temp database. C SQLite's own spellings for that are the
// numeral 2 and the name MEMORY (0/DEFAULT and 1/FILE both leave it file-backed
// in this oracle build, whose compile-time SQLITE_TEMP_STORE is 1 -- verified:
// "PRAGMA temp_store=FILE" leaves "PRAGMA temp.journal_mode" at "delete").
func tempStoreAsksForMemory(text string) bool {
	return r35aTempStoreValue(text) == 2
}

// The two non-mode values of DB.tempJournalMode, each a state that
// tempJournalModeResult declines:
//
//   - tempJournalModeUnmodelled: temp_store made the temp database
//     memory-backed.
//   - tempJournalModeUnopened: an unqualified journal_mode setter ran while it
//     was unknown whether the temp database was open.
//
// "" means neither: it reads "delete" and an unqualified setter does not reach
// it.
const (
	tempJournalModeUnmodelled = "!memory-backed"
	tempJournalModeUnopened   = "!open-state-unknown"
)

// tempJournalModeOf is the mode "PRAGMA temp.journal_mode" reports, given the
// raw connection state ("" being the untouched default).
func tempJournalModeOf(cur string) string {
	if cur == "" {
		return journalModeDelete
	}
	return cur
}

// tempJournalModeResult implements "PRAGMA temp.journal_mode". The temp
// database's mode is connection state with its own default, measured on C:
//
//	PRAGMA temp.journal_mode                  delete   (the default, and it is
//	                                                    NOT main's -- see below)
//	PRAGMA journal_mode = persist   persist   the UNQUALIFIED setter moves EVERY
//	PRAGMA main.journal_mode        persist   database, temp and any attachment
//	PRAGMA temp.journal_mode        persist   included
//	PRAGMA main.journal_mode = truncate       ...while a QUALIFIED one moves only
//	PRAGMA temp.journal_mode        persist   the database it names
//
//	PRAGMA temp.journal_mode = memory  memory  the temp-qualified setter moves
//	PRAGMA journal_mode                delete  temp alone
//	PRAGMA temp.journal_mode = wal     delete  WAL is refused for temp, and an
//	PRAGMA temp.journal_mode = off     off     unrecognized name likewise: the
//	PRAGMA temp.journal_mode = wal     off     current mode is reported back
//
// The unqualified setter reaches temp only if it is already open, and temp
// opens lazily (any temp-qualified pragma, a temp object even after DROP,
// reading sqlite_temp_master; not temp_store or a sorter). After a
// temp-qualified journal_mode statement the mode is known; an unqualified
// setter before that records tempJournalModeUnopened and declines, except
// "= delete", which reads the same either way.
//
// After temp_store=MEMORY the temp database is memory-backed and later
// transitions are not modelled, so tempJournalModeUnmodelled declines.
func tempJournalModeResult(cur string, stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	switch cur {
	case tempJournalModeUnmodelled:
		return nil, nil, fmt.Errorf("%w: PRAGMA temp.journal_mode after PRAGMA temp_store made the temp database memory-backed (C SQLite then answers \"memory\" and stops following the unqualified setter, and a switch back to temp_store=FILE returns it to \"delete\" rather than to main's mode -- state this engine does not model)", errVDBEUnsupported)
	case tempJournalModeUnopened:
		return nil, nil, fmt.Errorf("%w: PRAGMA temp.journal_mode after an unqualified PRAGMA journal_mode setter on a connection that had not yet opened the temp database (C SQLite's unqualified setter reaches only the databases that are OPEN, and this engine does not track whether the temp one is -- run a temp-qualified pragma first and the mode is known exactly)", errVDBEUnsupported)
	}
	mode := tempJournalModeOf(cur)
	if stmt.HasValue {
		want := strings.ToLower(strings.TrimSpace(stmt.ValueText))
		if journalModeNames[want] && want != journalModeWAL && want != mode {
			// A real switch: only the write path may record it (this is the read
			// side, which has nowhere to put it -- journalModeResult's own rule).
			return nil, nil, fmt.Errorf("%w: PRAGMA temp.journal_mode=%s over a read-only snapshot (the temp database is in %q mode; recording a switch is a write-path statement -- see execTempJournalMode)", errVDBEUnsupported, want, mode)
		}
		// Everything else -- an unrecognized name, "= <cur>", and "= wal",
		// which the temp database always refuses -- is a no-op in C SQLite
		// too, so reporting the unchanged mode back IS the whole behaviour.
	}
	return []string{"journal_mode"}, [][]Value{{{Typ: Text, S: []byte(mode)}}}, nil
}

// execTempJournalMode is tempJournalModeResult's write half: the only side that
// can record a switch. Its accept/decline split is the read side's, inverted --
// see that function for every rule and its oracle evidence.
func (db *DB) execTempJournalMode(stmt *PragmaStmt) error {
	switch db.tempJournalMode {
	case tempJournalModeUnmodelled, tempJournalModeUnopened:
		_, _, err := tempJournalModeResult(db.tempJournalMode, stmt)
		return err
	}
	// Running this pragma at all OPENS the temp database (verified -- see
	// tempJournalModeResult's opener table), so record the mode explicitly: from
	// here on an unqualified setter really does reach it.
	db.tempJournalMode = tempJournalModeOf(db.tempJournalMode)
	if !stmt.HasValue {
		return nil // a getter; its single row is discarded by an Exec
	}
	want := strings.ToLower(strings.TrimSpace(stmt.ValueText))
	if !journalModeNames[want] || want == journalModeWAL {
		return nil // silently ignored in C SQLite; the mode stays put
	}
	db.tempJournalMode = want
	return nil
}

// lockStatusResult implements PRAGMA [db.]lock_status, which in a build without
// SQLITE_DEBUG answers NOTHING AT ALL: zero columns and zero rows, in every form
// -- the bare getter, the schema-qualified one, and even "lock_status=1"
// (verified against mattn/go-sqlite3 3.53.3, which is such a build). So matching
// it needs no lock state of any kind; it is not an accept-and-guess.
func lockStatusResult() (cols []string, rows [][]Value, err error) {
	return []string{}, [][]Value{}, nil
}

// lockingModeRequest is what a PRAGMA locking_mode statement ASKS FOR: the mode
// named by its value, or "neither" for the value-less getter AND for a value
// C SQLite does not recognize -- pragma.c's getLockingMode() folds those two
// together, which is why "PRAGMA locking_mode=xyz" is a pure QUERY rather than
// an error or a reset (verified: "=xyz" answers "exclusive" on a connection
// already in exclusive mode and "normal" on one that is not).
type lockingModeRequest int

const (
	lockingModeQuery lockingModeRequest = iota
	lockingModeSetNormal
	lockingModeSetExclusive
)

func lockingModeRequestOf(stmt *PragmaStmt) lockingModeRequest {
	if !stmt.HasValue {
		return lockingModeQuery
	}
	switch strings.ToLower(strings.TrimSpace(stmt.ValueText)) {
	case lockingModeNormal:
		return lockingModeSetNormal
	case lockingModeExclusive:
		return lockingModeSetExclusive
	}
	return lockingModeQuery
}

// LockingModeRequested reports the mode stmt ASKS for, and whether it asks for
// one at all: getLockingMode (pragma.c:667) recognizes only "exclusive" and
// "normal", so every other right-hand side -- "=xyz" included -- is a pure
// query and reports ok=false. A "temp." qualifier is NOT considered here (that
// database is pinned exclusive); IsTempSchemaQualifier is its own test.
// Exported for driver, which has to know whether a form asks for a real
// change of lock lifetime before it routes one to a session.
func LockingModeRequested(stmt *PragmaStmt) (exclusive, ok bool) {
	switch lockingModeRequestOf(stmt) {
	case lockingModeSetExclusive:
		return true, true
	case lockingModeSetNormal:
		return false, true
	}
	return false, false
}

// IsTempSchemaQualifier reports whether q names the TEMP database, for a caller
// outside this package that has to apply one of the schema-qualified rules
// itself (driver's locking_mode routing).
func IsTempSchemaQualifier(q string) bool { return isTempSchemaQualifier(q) }

// lockingModeRow is the single row every form of this pragma answers: one TEXT
// column named "locking_mode", lower-cased whatever the argument's spelling.
func lockingModeRow(exclusive bool) (cols []string, rows [][]Value, err error) {
	mode := lockingModeNormal
	if exclusive {
		mode = lockingModeExclusive
	}
	return []string{"locking_mode"}, [][]Value{{{Typ: Text, S: []byte(mode)}}}, nil
}

// lockingModeResult is the read side of PRAGMA [db.]locking_mode: it reports
// the state the write session recorded and cannot change it, since a read
// pager holds no descriptor to lock, so a switching setter declines. The write
// side is execLockingMode.
//
// A memory-backed database declines outright: there C pins main exclusive
// while the unqualified getter reports the connection default, and this engine
// backs ":memory:" with a temp file it cannot pin.
func lockingModeResult(inMemory bool, dflt, main bool, stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	if inMemory {
		return nil, nil, fmt.Errorf("engine: unsupported PRAGMA locking_mode on a memory-backed database (there the unqualified form reports a connection-wide default while the schema-qualified form reports the database's own pinned \"exclusive\" mode -- two pieces of state this engine does not model)")
	}
	if isTempSchemaQualifier(stmt.Schema) {
		return lockingModeRow(true) // pinned exclusive -- see execLockingMode
	}
	req := lockingModeRequestOf(stmt)
	if req == lockingModeQuery {
		if stmt.Schema == "" {
			return lockingModeRow(dflt) // the bare getter reports the DEFAULT
		}
		return lockingModeRow(main)
	}
	if (req == lockingModeSetExclusive) != main {
		return nil, nil, fmt.Errorf("engine: unsupported PRAGMA locking_mode=%s from a caller with no long-lived write session (entering or leaving exclusive locking mode is a lock LIFETIME change -- the lock has to be held across statements on one descriptor, which only such a session owns; see execLockingMode)", strings.ToLower(strings.TrimSpace(stmt.ValueText)))
	}
	return lockingModeRow(main)
}

// LockingModeResult answers the pragma for a caller that carries the two modes
// itself -- driver, which keeps them on the Conn (its lockingModePragma)
// because it owns the connection's state across its sessions. Exported for the
// same reason SecureDeleteResult is: the
// driver has to answer this pragma itself, and it must answer it with exactly
// the engine's own shape and decline rules. dflt is the connection-wide
// default, main this database's own mode; both false is a connection that has
// never entered the mode.
func LockingModeResult(inMemory bool, dflt, main bool, stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	return lockingModeResult(inMemory, dflt, main, stmt)
}

// LockingModes reports this session's connection-wide default and main
// database's own locking mode, for a driver that has to carry them onto the
// NEXT statement's session (SetLockingModes). See execLockingMode for the two.
func (db *DB) LockingModes() (dflt, main bool) { return db.lockingDefault, db.lockingMain }

// SetLockingModes seeds them, the mirror of LockingModes. The LOCK itself is
// the session's segment lock (holdLockingModeLock); this is the state that
// decides whether it is held.
func (db *DB) SetLockingModes(dflt, main bool) { db.lockingDefault, db.lockingMain = dflt, main }

// execLockingMode is the write side of PRAGMA [db.]locking_mode, which owns the
// descriptor the lock lives on.
//
// Every form answers one TEXT row "locking_mode" and never errors. There are
// two pieces of state: the connection default (dfltLockMode, written only by
// the unqualified setter, inherited by later ATTACHes) and each database's own
// mode. The bare getter reports the default; every other form reports the named
// (or main) database's mode. temp is pinned exclusive.
//
// Entering the mode takes a real byte-range lock on this session's descriptor
// and keeps it across statements (holdLockingModeLock, lock.go). Declined,
// because the lock could not be held as claimed:
//
//   - leaving on a WAL database while the wal-index state is unknown (see
//     walExclusiveLeaveDecline);
//   - an attachment this session was delegated to;
//   - the unqualified "=exclusive" while attachments exist (C locks every
//     file); ATTACH while exclusive is declined by execAttach, so exclusive
//     mode and attachments never coexist.
//
// Unrecognized values and every getter are answered as C answers them.
func (db *DB) execLockingMode(stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	if db.inMemory {
		return lockingModeResult(true, db.lockingDefault, db.lockingMain, stmt)
	}
	if isTempSchemaQualifier(stmt.Schema) {
		// Pinned exclusive; a setter naming it is a silent no-op there too.
		return lockingModeRow(true)
	}
	req := lockingModeRequestOf(stmt)
	bare := stmt.Schema == ""
	if req == lockingModeQuery {
		if bare {
			return lockingModeRow(db.lockingDefault)
		}
		return lockingModeRow(db.lockingMain)
	}
	want := req == lockingModeSetExclusive
	if !want && db.lockingMain && db.inWALMode() {
		switch db.walIndexState {
		case walIndexHeap:
			// The pager refuses (sqlite3WalHeapMemory) and main stays exclusive, but
			// pragma.c writes db->dfltLockMode first, unconditionally: the setter answers
			// "exclusive", the bare getter then "normal", "main.locking_mode" "exclusive"
			// (wal2.test).
			if bare {
				db.lockingDefault = false
			}
			return lockingModeRow(true)
		case walIndexNotOpened, walIndexShm:
			// Leavable: either no Wal was ever opened, or it was opened while
			// the pager was still NORMAL, so its wal-index is a real "-shm"
			// file rather than heap memory. Falls through to the ordinary path.
		default:
			return nil, nil, walExclusiveLeaveDecline()
		}
	}
	if want && !db.lockingMain {
		if isAttachedSession(db.localSchema) {
			return nil, nil, fmt.Errorf("engine: unsupported PRAGMA %s.locking_mode=exclusive (this engine holds the cross-statement lock on the session's own database; an attachment's session is opened lazily, so the unqualified setter could not reach every attached file the way C SQLite does)", strings.ToLower(db.localSchema))
		}
		if bare && len(db.attached) > 0 {
			return nil, nil, fmt.Errorf("engine: unsupported PRAGMA locking_mode=exclusive while databases are ATTACHed (C SQLite's unqualified setter locks every attached file as well, which this engine would have to materialize each attachment to do)")
		}
	}
	if bare {
		db.lockingDefault = want
		if !want {
			// The unqualified setter reaches every attached database too (pragma.c:700-714),
			// in both directions. Only leaving is implemented: entering would have to take
			// and atomically unwind OS locks on every file, because holdLockingModeLock
			// locks at the pragma itself; the bare "=exclusive" with attachments stays
			// declined above. exclusive.test only exercises leaving.
			for _, ad := range db.attached {
				w, werr := db.attachedWriteSession(ad.name)
				if werr != nil {
					return nil, nil, werr
				}
				// A memory-backed attachment is skipped: C's sweep is already a no-op for it
				// (pager.c:7382, "!pPager->tempFile"), and w.execLockingMode would decline the
				// whole statement over it.
				if w.inMemory {
					continue
				}
				// Reuse the routed "PRAGMA aux.locking_mode=normal" path so w's leave gets
				// every execLockingMode guard, including the WAL heap-memory refusal. Leaving
				// acquires nothing; the OS unlock happens when w releases its lock at commit
				// (releaseSegmentLockingModeLock).
				leaveStmt, perr := ParsePragma(`PRAGMA locking_mode = normal`)
				if perr != nil {
					return nil, nil, perr // unreachable: a literal, well-formed pragma
				}
				if _, _, lerr := w.execLockingMode(leaveStmt); lerr != nil {
					// w's leave is uncertain (walIndexUnknown); decline. Earlier attachments and
					// db.lockingDefault may already have moved: a partial decline, never a wrong
					// value.
					return nil, nil, lerr
				}
			}
			// Refresh each attachment's read snapshot, which copies lockingMain; this
			// sweep bypasses the routed path that normally refreshes it.
			if rerr := db.refreshAttachedWriteReaders(); rerr != nil {
				return nil, nil, rerr
			}
		}
	}
	db.lockingMain = want
	if !want {
		// Back to normal: give the held lock up, or the connection keeps excluding
		// everyone else after saying it stopped.
		db.releaseSegmentLockingModeLock()
	}
	if want {
		if err := db.holdLockingModeLock(); err != nil {
			// Another connection holds a conflicting lock. C SQLite reports
			// that at the first access rather than at the pragma, but reporting
			// it here is a clean decline, never a wrong answer -- and it is
			// strictly better than answering "exclusive" without the lock.
			db.lockingMain, db.lockingDefault = false, false
			return nil, nil, err
		}
	}
	return lockingModeRow(db.lockingMain)
}

// walExclusiveLeaveDecline is the error for "PRAGMA locking_mode=normal" on an
// exclusive WAL connection whose wal-index state is walIndexUnknown; the other
// states are served (execLockingMode).
//
// pager.c refuses the change while sqlite3WalHeapMemory, which wal.c sets at
// sqlite3WalOpen only when the pager was already exclusive. So the mode is
// unleavable exactly when the WAL was opened while exclusive. Whether a
// statement opened it (coded OP_Transaction on main) is mainReadTxnOf's
// measured classification; unknown declines. Leaving WAL mode closes the Wal
// and clears the flag.
func (db *DB) inWALMode() bool { return db.segWAL }

func walExclusiveLeaveDecline() error {
	return fmt.Errorf("engine: unsupported PRAGMA locking_mode=normal on a WAL-mode database already in exclusive locking mode (C SQLite can leave the mode unless the WAL was OPENED while the pager was ALREADY exclusive, and a statement this session could not classify has run since -- see walIndexKind; leave WAL mode first and the setter is served)")
}

// ---- where the wal-index lives, and how this session tracks it ----

// walIndexKind is WHERE this connection's wal-index lives, which is the ONE
// thing that decides whether "PRAGMA locking_mode=normal" can leave exclusive
// locking mode -- see walExclusiveLeaveDecline for the pager.c rule and
// noteWalIndexStatement for how this is maintained.
//
// The state is per-CONNECTION and per-Wal: entering or leaving WAL journal mode
// opens or closes the Wal, and resets it to walIndexNotOpened.
type walIndexKind uint8

const (
	// walIndexNotOpened: no statement has opened the Wal since this database
	// entered WAL mode, so there is no wal-index at all and the mode is
	// freely leavable.
	walIndexNotOpened walIndexKind = iota
	// walIndexShm: the Wal was opened while the pager was NORMAL, so the
	// wal-index is a real "-shm" file and the mode stays freely leavable --
	// for the whole life of that Wal, even after exclusive mode is entered.
	walIndexShm
	// walIndexHeap: the Wal was opened while the pager was ALREADY exclusive,
	// so pagerOpenWal put the wal-index in heap memory and
	// sqlite3PagerLockingMode() refuses to leave the mode.
	walIndexHeap
	// walIndexUnknown: a statement ran that this session could not classify,
	// so which of the three above holds is not known. Declined rather than
	// guessed -- guessing either way is a wrong answer, not a gap.
	walIndexUnknown
	// walIndexUnknownRead: a top-level READ SNAPSHOT was taken and the query it
	// was taken for has not run yet, so its text is not known YET. Identical to
	// walIndexUnknown for every reader of this state (execLockingMode's switch
	// declines on both), but PROVISIONAL: ReadOnlyPager.QueryArgs sees that text
	// a moment later and resolves it -- see resolveWalIndexRead. Anything else
	// that happens first hardens it to walIndexUnknown, because a snapshot whose
	// query never arrived really is unclassifiable.
	walIndexUnknownRead
)

// mainReadTxn is whether a statement's program codes an OP_Transaction on the
// MAIN database -- i.e. whether it is the statement that OPENS the Wal.
type mainReadTxn uint8

const (
	mainReadTxnNo mainReadTxn = iota
	mainReadTxnYes
	mainReadTxnUnknown
)

// noteWalIndexStatement folds one completed top-level statement into the
// wal-index state. Only the first statement that opens the Wal matters. UNKNOWN
// here is the read path's provisional walIndexUnknownRead (from SnapshotPager);
// ExecArgs uses noteWalIndexOpaque instead. A provisional unknown still
// outstanding is hardened first.
func (db *DB) noteWalIndexStatement(k mainReadTxn) {
	if !db.inWALMode() {
		return
	}
	if db.walIndexState == walIndexUnknownRead {
		db.walIndexState = walIndexUnknown
	}
	if db.walIndexState != walIndexNotOpened {
		return
	}
	switch k {
	case mainReadTxnYes:
		if db.lockingMain {
			db.walIndexState = walIndexHeap
		} else {
			db.walIndexState = walIndexShm
		}
	case mainReadTxnUnknown:
		db.walIndexState = walIndexUnknownRead
	}
}

// noteWalIndexOpaque records that something this session cannot classify AT ALL
// has run -- today, a top-level statement that returned an error, where it
// failed deciding whether it ever coded an OP_Transaction. Unlike
// noteWalIndexStatement's provisional unknown, nothing can resolve this one.
func (db *DB) noteWalIndexOpaque() {
	if !db.inWALMode() {
		return
	}
	if db.walIndexState == walIndexNotOpened || db.walIndexState == walIndexUnknownRead {
		db.walIndexState = walIndexUnknown
	}
}

// resolveWalIndexRead resolves a top-level snapshot's provisional unknown once
// its query text arrives (ReadOnlyPager.QueryArgs). Only the first query on a
// snapshot resolves it.
func (db *DB) resolveWalIndexRead(sqlText string) {
	if db == nil {
		return
	}
	// Every top-level READ arrives here, whether or not the wal-index state is
	// still provisional, which makes this the read half of the pair that keeps
	// DB.tempDatabaseOpened current -- "SELECT * FROM sqlite_temp_master" is an
	// opener (temp_schema.go). The write half is mainReadTxnOf, below.
	db.r35aNoteTempDatabaseOpen(sqlText)
	if db.walIndexState != walIndexUnknownRead {
		return
	}
	k := db.mainReadTxnOf(sqlText)
	if k == mainReadTxnUnknown {
		db.walIndexState = walIndexUnknown
		return
	}
	db.walIndexState = walIndexNotOpened
	db.noteWalIndexStatement(k)
}

// mainReadTxnOf classifies whether a top-level statement opens a read
// transaction on main (and so the Wal). Every entry was measured on C with:
//
//	CREATE TABLE t(a); INSERT INTO t VALUES(1);
//	PRAGMA journal_mode=wal; PRAGMA locking_mode=exclusive;
//	<statement>;
//	PRAGMA locking_mode=normal      -> "normal" = NO, "exclusive" = YES
//
//	NO   SELECT 1; BEGIN; BEGIN DEFERRED; BEGIN+COMMIT; BEGIN+ROLLBACK;
//	     SAVEPOINT; SAVEPOINT+RELEASE; SAVEPOINT+ROLLBACK TO;
//	     ATTACH; ATTACH+DETACH; REINDEX (with no index to rebuild);
//	     CREATE TEMP TABLE, and an INSERT into or SELECT from one;
//	     PRAGMA locking_mode / journal_mode / database_list / synchronous /
//	     cache_size / page_size / auto_vacuum / encoding / wal_autocheckpoint /
//	     lock_status / foreign_keys (get AND set) / defer_foreign_keys /
//	     cache_spill / temp_store / mmap_size / journal_size_limit / threads /
//	     busy_timeout / shrink_memory / collation_list / case_sensitive_like /
//	     recursive_triggers / trusted_schema / query_only / legacy_alter_table /
//	     ignore_check_constraints / analysis_limit / secure_delete /
//	     writable_schema / foreign_key_check (over a schema with no FK)
//
//	YES  SELECT * FROM t; SELECT count(*) FROM sqlite_master;
//	     SELECT * FROM sqlite_master; INSERT; UPDATE; DELETE; REPLACE;
//	     CREATE TABLE / INDEX / VIEW / TRIGGER; DROP TABLE IF EXISTS <absent>;
//	     CREATE TABLE IF NOT EXISTS <present>; ALTER TABLE RENAME;
//	     BEGIN IMMEDIATE; BEGIN EXCLUSIVE; VACUUM; ANALYZE;
//	     PRAGMA max_page_count / user_version (get AND set) / page_count /
//	     freelist_count / schema_version / application_id / data_version /
//	     integrity_check / quick_check / table_info / table_list / index_list /
//	     wal_checkpoint / optimize / incremental_vacuum
//
// Anything else is UNKNOWN, which declines. TEMP statements are NO (they open
// the temp pager), so DML/DDL is YES only while the session holds no temp
// object; mainReadTxnAfter re-asks after the statement runs.
func (db *DB) mainReadTxnOf(sqlText string) mainReadTxn {
	// ExecArgs calls this for every top-level statement it runs, which makes it
	// the write half of the pair that keeps DB.tempDatabaseOpened current (the
	// read half is resolveWalIndexRead, above). The bit is a fact about what
	// C SQLite's PARSER would have done with the text, so noting it here --
	// before any classification, and whether or not the statement then succeeds
	// -- is exactly the right moment.
	db.r35aNoteTempDatabaseOpen(sqlText)
	trimmed := strings.TrimSpace(sqlText)
	verb, ok := db.leadingVerb(trimmed)
	if !ok {
		return mainReadTxnUnknown
	}
	switch verb {
	case "SELECT":
		return db.mainReadTxnOfSelect(trimmed)
	case "BEGIN":
		up := strings.ToUpper(trimmed)
		if strings.Contains(up, "IMMEDIATE") || strings.Contains(up, "EXCLUSIVE") {
			return mainReadTxnYes
		}
		return mainReadTxnNo
	case "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE", "ATTACH", "DETACH":
		return mainReadTxnNo
	case "PRAGMA":
		stmt, perr := ParsePragma(trimmed)
		if perr != nil {
			return mainReadTxnUnknown
		}
		// A name C SQLite does not recognize is INERT there -- it never
		// even reaches a program, let alone a transaction (pragma_unknown.go).
		if unknownPragmaIsNoop(stmt.Name) {
			return mainReadTxnNo
		}
		if mainReadTxnPragmaYes[stmt.Name] {
			return mainReadTxnYes
		}
		if mainReadTxnPragmaNo[stmt.Name] {
			return mainReadTxnNo
		}
		return mainReadTxnUnknown
	}
	if mainReadTxnTempSensitiveVerb[verb] {
		if db.holdsAnyTempObject() {
			return mainReadTxnUnknown
		}
		return mainReadTxnYes
	}
	return mainReadTxnUnknown
}

// mainReadTxnTempSensitiveVerb is the set of leading verbs whose classification
// turns on whether this session holds a TEMP object -- mainReadTxnOf's DML/DDL
// case. It is a named set rather than a switch case because that question has to
// be asked TWICE, before and after the statement: see mainReadTxnAfter.
var mainReadTxnTempSensitiveVerb = map[string]bool{
	"INSERT": true, "REPLACE": true, "UPDATE": true, "DELETE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "VACUUM": true, "ANALYZE": true,
}

// mainReadTxnAfter re-asks the TEMP question after the statement ran: the
// statement creating the session's first temp object (CREATE TEMP
// TABLE/VIEW/TRIGGER) is NO in C, which the before-check cannot see. It becomes
// UNKNOWN, since telling it apart would need target resolution. Measured YES
// pragmas stay YES beside a temp object.
func (db *DB) mainReadTxnAfter(sqlText string, k mainReadTxn) mainReadTxn {
	if k != mainReadTxnYes || !db.holdsAnyTempObject() {
		return k
	}
	if verb, ok := db.leadingVerb(strings.TrimSpace(sqlText)); ok && mainReadTxnTempSensitiveVerb[verb] {
		return mainReadTxnUnknown
	}
	return k
}

// leadingVerb is LeadingStatementVerb with a one-entry memo; exact, since the
// verb is a pure function of the text. It saves re-lexing the same prepared
// statement on every execution (most of mainReadTxnOf's cost on bulk INSERT). A
// plain scan of the first identifier is not equivalent: the lexer reports
// ok=false for text that does not lex at all, which becomes UNKNOWN.
func (db *DB) leadingVerb(sqlText string) (string, bool) {
	if db.lastVerbText == sqlText {
		return db.lastVerb, db.lastVerbOK
	}
	verb, ok := LeadingStatementVerb(sqlText)
	db.lastVerbText, db.lastVerb, db.lastVerbOK = sqlText, verb, ok
	return verb, ok
}

// mainReadTxnOfSelect is mainReadTxnOf's SELECT case, called from
// resolveWalIndexRead. Measured with the same probe:
//
//	NO   SELECT 1; SELECT abs(-1); SELECT date('now'); VALUES(1);
//	     SELECT 1 UNION ALL SELECT 2; SELECT 1 ORDER BY 1;
//	     SELECT 1 WHERE EXISTS(SELECT 1); SELECT * FROM (SELECT 1);
//	     WITH x(a) AS (VALUES(1)) SELECT * FROM x;
//	     SELECT * FROM sqlite_temp_master; SELECT * FROM tt (a TEMP table)
//
//	YES  SELECT * FROM t -- and the same with WHERE 0, LIMIT 0, ORDER BY,
//	     count(*), max(a), a self-JOIN or a UNION ALL;
//	     SELECT * FROM sqlite_master; SELECT count(*) FROM sqlite_master;
//	     SELECT * FROM v, a view -- even v0 whose whole body is "SELECT 1";
//	     SELECT (SELECT a FROM t); SELECT 1 WHERE EXISTS(SELECT 1 FROM t);
//	     WITH x AS (SELECT a FROM t) SELECT * FROM x;
//	     SELECT * FROM t, tt (main beside a temp table);
//	     SELECT * FROM pragma_table_info('t');
//	     SELECT * FROM nosuchtable and SELECT nosuchcolumn FROM t
//
// Only the YES end is classified: a FROM whose every item is an unqualified (or
// main.) name resolving in this catalog, with no temp object in the session, no
// CTEs, and a non-empty FROM. Everything else is UNKNOWN.
func (db *DB) mainReadTxnOfSelect(sqlText string) mainReadTxn {
	if db.holdsAnyTempObject() {
		return mainReadTxnUnknown
	}
	stmt, err := ParseSelect(sqlText)
	if err != nil || len(stmt.From) == 0 || len(stmt.CTEs) != 0 {
		return mainReadTxnUnknown
	}
	for _, it := range stmt.From {
		if it.Subquery != nil || it.TableFunc || it.Table == "" {
			return mainReadTxnUnknown
		}
		if it.Schema != "" && !equalFoldName(it.Schema, "main") {
			return mainReadTxnUnknown
		}
		if isMainSchemaCatalogName(it.Table) {
			// sqlite_master / sqlite_schema, measured YES. The temp spellings
			// are NOT, and isMainSchemaCatalogName excludes them.
			continue
		}
		if db.findTableMeta(it.Table) != nil {
			continue
		}
		if db.findViewMeta(it.Table) != nil {
			continue
		}
		return mainReadTxnUnknown
	}
	return mainReadTxnYes
}

// mainReadTxnPragmaYes / mainReadTxnPragmaNo are the measured pragma halves of
// mainReadTxnOf's table; see it for the probe and the full evidence. A name in
// NEITHER map is UNKNOWN, which declines -- foreign_key_check is deliberately
// absent, because whether it codes a transaction depends on whether the schema
// has a foreign key at all.
var mainReadTxnPragmaYes = map[string]bool{
	"max_page_count": true, "user_version": true, "page_count": true,
	"freelist_count": true, "schema_version": true, "application_id": true,
	"data_version": true, "integrity_check": true, "quick_check": true,
	"table_info": true, "table_xinfo": true, "table_list": true,
	"index_list": true, "index_info": true, "index_xinfo": true,
	"wal_checkpoint": true, "optimize": true, "incremental_vacuum": true,
}

var mainReadTxnPragmaNo = map[string]bool{
	"locking_mode": true, "journal_mode": true, "database_list": true,
	"synchronous": true, "cache_size": true, "page_size": true,
	"auto_vacuum": true, "encoding": true, "wal_autocheckpoint": true,
	"foreign_keys": true, "defer_foreign_keys": true, "cache_spill": true,
	"temp_store": true, "mmap_size": true, "journal_size_limit": true,
	"threads": true, "busy_timeout": true, "shrink_memory": true,
	"collation_list": true, "case_sensitive_like": true,
	"recursive_triggers": true, "trusted_schema": true, "query_only": true,
	"legacy_alter_table": true, "ignore_check_constraints": true,
	"analysis_limit": true, "secure_delete": true, "writable_schema": true,
}

// holdsAnyTempObject reports whether this session's TEMP catalog holds anything
// at all -- see mainReadTxnOf, whose DML/DDL case needs it: a statement whose
// target is a temp object opens the TEMP pager and never touches main.
func (db *DB) holdsAnyTempObject() bool {
	for _, t := range db.tables {
		if t.isTemp {
			return true
		}
	}
	for _, v := range db.views {
		if v.isTemp {
			return true
		}
	}
	for _, i := range db.indexes {
		if i.isTemp {
			return true
		}
	}
	for _, tr := range db.triggers {
		if tr.isTemp {
			return true
		}
	}
	for _, vt := range db.vtabs {
		if vt.isTemp {
			return true
		}
	}
	return false
}

// isAttachedSession reports whether localSchema names an ATTACHed database --
// i.e. this *DB is the delegated session attach_write.go opened for one, rather
// than a connection's own main database. "" and "main" are the main session.
func isAttachedSession(localSchema string) bool {
	return localSchema != "" && !equalFoldName(localSchema, "main")
}

// writableSchemaValue parses PRAGMA writable_schema's argument into the flag
// it asks for, plus whether it was the RESET spelling (SQLite 3.35+), which
// turns the flag OFF *and* reloads the schema.
//
// Verified directly against mattn/go-sqlite3 3.53.3, reading the flag back
// after each: "=reset"/"=RESET" leave it 0 -- RESET is not a third state, it
// is OFF with a reload attached.
func writableSchemaValue(text string) (on, isReset bool) {
	if strings.EqualFold(strings.TrimSpace(text), "reset") {
		return false, true
	}
	return pragmaGetBoolean(text, false), false
}

// WritableSchemaEditsActive reports whether this session holds a direct
// sqlite_master write the live schema has not taken on. Exported for the
// driver, which must ask the held session before serving
// "writable_schema=RESET".
func (db *DB) WritableSchemaEditsActive() bool {
	return db != nil && db.writableSchemaEditsActive()
}

// WritableSchema reports this session's writable_schema flag. Exported for the
// driver: a RESET applied inside a held transaction flips it through the engine,
// so the Conn mirror reads it back.
func (db *DB) WritableSchema() bool {
	return db != nil && db.writableSchema
}

// writableSchemaResetDecline handles "PRAGMA writable_schema=RESET" while a
// direct sqlite_master edit is outstanding. RESET is OFF plus a whole-schema
// reload (pragma.c:1182), which re-derives every object from the edited rows
// and takes each root from the row's rootpage (prepare.c:135, 174). The reload
// serves sql-text edits, deleted rows and the rootpage shapes
// (schema_reload_rootpage.go). Still declined: an index whose rootpage aliases
// another b-tree (C answers covered columns from that index's record), and
// name/type edits (C matches autoindex rows by name and can leave an index
// rootless; this engine matches by position).
func (db *DB) writableSchemaResetDecline(stmt *PragmaStmt) error {
	if !stmt.HasValue || !db.writableSchemaEditsActive() {
		return nil
	}
	if _, isReset := writableSchemaValue(stmt.ValueText); !isReset {
		return nil
	}
	// Run sqlite3InitCallback's row rules first (prepare.c:145-200): a RESET re-runs
	// the schema load and a row it refuses fails the whole load (corruptSchema).
	// Only an inserted catalog row can be such a row here (wsInsertedRowCorrupt).
	if obj, detail, bad := db.wsInsertedRowCorrupt(); bad {
		db.latchSchemaCorrupt(obj, detail)
		return nil
	}
	// ...and sqlite3CheckObjectName's cross-check on an EDITED row, which is the
	// same rule for the other half of the overlay: a row whose type/name/tbl_name
	// no longer agrees with what its own SQL creates does not decline the reload,
	// it CORRUPTS the schema (build.c:1031-1052 into corruptSchema). See
	// wsEditedRowDisagreesWithItsSQL.
	if obj, bad := db.wsEditedRowDisagreesWithItsSQL(); bad {
		db.latchSchemaCorrupt(obj, "")
		return nil
	}
	if db.reloadSchemaFromEdits() {
		return nil
	}
	// ...and, last, the GENERAL reload: re-derive every object from the edited
	// catalog rows (segment_schema_write.go). That is prepare.c's sqlite3InitOne
	// verbatim rather than a per-shape patch, and it is what serves every
	// sql-text edit the narrower route above does not -- a rewritten column
	// list, an index whose CREATE INDEX text gained UNIQUE, a view's or
	// trigger's body, or a row simply deleted.
	if db.reloadSchemaFromSegmentCatalog(false) {
		return nil
	}
	if db.segmentRootpageEditDeclined() {
		return errSegmentRootpageNotReproducible
	}
	// What reaches this line is an edit the reload declines by design --
	// a name or type edit, an inserted row naming storage, a reload inside a
	// transaction (schema_reload_image.go lists each with its reason) -- so the
	// message names those rather than one example.
	return fmt.Errorf("engine: unsupported PRAGMA writable_schema=RESET while this session holds a direct sqlite_master write this engine cannot reload from (RESET is OFF plus a WHOLE-SCHEMA reload, pragma.c:1182, which re-derives every object from the EDITED catalog rows and takes each one's b-tree root from the row's own rootpage column, prepare.c:135-137/174-183 into build.c:2673/4393. Every sql-text edit and deleted row reloads, and a rootpage naming the object's own table or another live table is served; what is left is a name or type edit, an inserted row that names storage, or a reload inside an open transaction -- see schema_reload_image.go)")
}

// writableSchemaResult implements PRAGMA [db.]writable_schema over cur, the
// connection's flag, and returns the value it holds afterwards. The getter is
// one INTEGER row "writable_schema"; the setter is an empty result with no
// columns. Values parse as sqlite3GetBoolean (pragmaGetBoolean); "=reset" is
// 0. The flag is connection state and survives COMMIT and ROLLBACK. What it
// unlocks is in schema_write_direct.go.
func writableSchemaResult(stmt *PragmaStmt, cur bool) (cols []string, rows [][]Value, next bool, err error) {
	if stmt.HasValue {
		on, _ := writableSchemaValue(stmt.ValueText)
		return nil, nil, on, nil // setter: empty result set, no columns
	}
	return []string{"writable_schema"}, [][]Value{{{Typ: Int, I: boolToInt64(cur)}}}, cur, nil
}

// journalMode reports this session's journal mode, the value "PRAGMA
// journal_mode" answers. A memory-backed database is "memory" and a WAL
// database "wal" whatever was set, except that MEMDB also accepts "off"
// (sqlite3PagerSetJournalMode). journalOffUndoDisabled relies on that.
func (db *DB) journalMode() string {
	switch {
	case db.inMemory && db.journalModeName == journalModeOff:
		return journalModeOff
	case db.inMemory:
		return journalModeMemory
	case db.segWAL:
		return journalModeWAL
	case db.journalModeName != "":
		return db.journalModeName
	}
	return journalModeDelete
}

// journalOffUndoDisabled reports whether db is in journal_mode=off inside a
// transaction (explicit BEGIN or a driver-held session). There C has no
// per-statement or per-savepoint undo (sqlite3PagerOpenSavepoint opens no
// sub-journal), while a whole ROLLBACK still works. Callers guard one
// statement's applied rows (rollbackToSavepoint, wc.rollback), never
// rollbackTxn.
func (db *DB) journalOffUndoDisabled() bool {
	return db.journalMode() == journalModeOff && db.inTransaction()
}

// SetJournalMode carries a connection's rollback journal mode onto a write
// session. It is needed for the reason SetForeignKeys is: the mode is
// per-CONNECTION state that C SQLite keeps nowhere on disk (verified: a
// second connection to a file another one put in truncate mode reads back
// "delete"), and a driver whose autocommit model opens a fresh session per
// statement would otherwise lose it after the pragma that set it. "" is the
// default, delete.
func (db *DB) SetJournalMode(m string) { db.journalModeName = m }

// TempStore is this session's "PRAGMA temp_store" value (DB.tempStore), and
// SetTempStore seeds it. They exist for the same reason the journal-mode pair
// does: C SQLite keeps db->temp_store per CONNECTION and nowhere in any
// file, while this driver opens a throwaway session per autocommit statement,
// so the value has to be carried on the Conn across them or every statement
// would start from the compile-time default again -- which is also what made
// the GETTER unanswerable.
func (db *DB) TempStore() uint8     { return db.tempStore }
func (db *DB) SetTempStore(v uint8) { db.tempStore = v }

// SetCaseSensitiveLike / CaseSensitiveLike are the connection round trip for
// "PRAGMA case_sensitive_like", for exactly the reason SetForeignKeys is one:
// the flag is per-CONNECTION state kept nowhere on disk, and a driver whose
// autocommit model opens a fresh session per statement would otherwise lose it
// the moment the pragma that set it returned.
func (db *DB) SetCaseSensitiveLike(on bool) { db.caseSensitiveLike = on }

// CaseSensitiveLike is the read-back half of SetCaseSensitiveLike: the driver
// seeds a throwaway session with it and reads it out again afterwards, so a
// setter the session applied survives the session.
func (db *DB) CaseSensitiveLike() bool { return db.caseSensitiveLike }

// SetAutomaticIndex / AutomaticIndex are the connection round trip for "PRAGMA
// automatic_index", for exactly the reason SetCaseSensitiveLike is one -- and it
// matters more here than for most: the flag's whole effect belongs to a LATER
// statement (the join whose loop order and inner visit order it decides), so a
// value that died with the throwaway session that set it would be no
// implementation at all. See DB.noAutoIndex for the flag's own rules; the read
// side's counterpart is ReadOnlyPager.SetAutomaticIndex.
func (db *DB) SetAutomaticIndex(on bool) { db.noAutoIndex = !on }

// AutomaticIndex reports the flag, ON on a fresh session as in C SQLite.
func (db *DB) AutomaticIndex() bool { return db == nil || !db.noAutoIndex }

// JournalMode reports the connection's own rollback mode as SetJournalMode set
// or a journal_mode setter left it, not the composite journalMode(). The driver
// seeds and reads it back across its throwaway sessions.
func (db *DB) JournalMode() string { return db.journalModeName }

// SetTempJournalMode / TempJournalMode are the same round trip for the TEMP
// database's own journal mode, which C SQLite likewise keeps only on the
// connection. The driver needs both halves for the reason it needs
// SetJournalMode's: an autocommit statement runs on a throwaway session, so
// without them "PRAGMA temp.journal_mode=memory" would be forgotten by the next
// statement. See tempJournalModeResult for what the value means.
func (db *DB) SetTempJournalMode(m string) { db.tempJournalMode = m }

// TempJournalMode reports this session's temp-database journal mode.
func (db *DB) TempJournalMode() string { return db.tempJournalMode }

// attachedPagersProvablyClean reports whether every attached database's pager
// would let an unqualified journal_mode setter move it, per OP_JournalMode's
// per-pager loop (vdbe.c:8054, pragma.c:762):
//
//   - sqlite3PagerOkToChangeJournalMode (pager.c:7506) refuses once a page is
//     dirty; !touchedInTxn proves it is not (conservatively: a read-only touch
//     also counts).
//   - a mid-transaction switch into or out of WAL errors the whole statement
//     (vdbe.c:8083); the caller excludes want == WAL, and this requires no
//     attachment is currently in WAL.
func attachedPagersProvablyClean(db *DB) bool {
	for _, a := range db.attached {
		if a.touchedInTxn {
			return false
		}
		var cur string
		switch {
		case a.wdb != nil:
			cur = a.wdb.journalMode()
		case a.pager != nil:
			cur = a.pager.journalMode()
		default:
			return false // no session to read a mode off of -- don't guess
		}
		if cur == journalModeWAL {
			return false
		}
	}
	return true
}

// execJournalMode implements the write side of "PRAGMA journal_mode [= mode]",
// the only side that can switch a mode. Rollback modes are connection state
// (DB.journalModeName); WAL is a property of the file. An unrecognized name or
// the current mode is a no-op, a memory-backed database only takes "off" and
// "memory", and in autocommit every switch takes. Inside a transaction see the
// dirty-page cases below.
//
// "off" is accepted like any mode; what differs is undo afterwards, as in C
// (sqlite3PagerOpenSavepoint opens nothing under OFF): ROLLBACK TO keeps the
// rolled-back work and a failed statement keeps the rows it applied (with
// changes() still 0), while a full ROLLBACK still undoes everything. See
// journalOffUndoDisabled and its callers.
func (db *DB) execJournalMode(stmt *PragmaStmt) error {
	cur := db.journalMode()
	if !stmt.HasValue {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(stmt.ValueText))
	if want == journalModeWAL && db.pragmaState.lockProxyOn() {
		// Proxy locking disables WAL, and OP_JournalMode downgrades the request
		// back to the current mode SILENTLY rather than erroring -- see
		// journalModeResult's arm for the citation chain (os_unix.c:5936-5945 ->
		// pager.c:7595-7599 -> vdbe.c:8087-8094). The write path has to refuse
		// the move for the same reason the read path refuses to report it:
		// otherwise the mode really changes here and every later getter is
		// wrong, which is worse than the read-side gap alone.
		return nil
	}
	// An unqualified setter moves each database separately, so temp follows even
	// when main does not (e.g. main pinned at "memory"). Temp refuses WAL, and a
	// declined mode must not move it. It reaches temp only while temp is open,
	// which is provable only after a temp-qualified journal_mode statement; until
	// then the state is unknown, except "= delete", which reads the same either way.
	// See tempJournalModeResult.
	moveTemp := func() {
		if stmt.Schema != "" || want == journalModeWAL {
			return
		}
		switch db.tempJournalMode {
		case tempJournalModeUnmodelled, tempJournalModeUnopened:
			return
		case "":
			if want != journalModeDelete {
				db.tempJournalMode = tempJournalModeUnopened
			}
			return
		}
		db.tempJournalMode = want
	}
	// applyMemBackedTarget is MEMDB's restriction (sqlite3PagerSetJournalMode): only
	// "memory" or "off" ever replaces the current mode; other targets are ignored,
	// in or out of a clean transaction.
	applyMemBackedTarget := func() {
		if want == journalModeMemory {
			db.journalModeName = ""
		}
		moveTemp()
	}
	switch {
	case !journalModeNames[want]:
		return nil
	case want == cur:
		moveTemp()
		return nil
	case db.inTransaction():
		// Inside a transaction the outcome depends on whether a page is dirty
		// (sqlite3PagerOkToChangeJournalMode). Measured over all 30 mode pairs:
		//
		//	BEGIN, or BEGIN + SELECT:
		//	  rollback mode -> rollback mode   takes
		//	  ... -> wal    "cannot change into wal mode from within a transaction"
		//	  wal -> ...    "cannot change out of wal mode from within a transaction"
		//	BEGIN + INSERT:
		//	  every pair is silently ignored
		//
		// DB.txMayHaveDirtiedMain proves the clean side (set by statement kind; see
		// noteTransactionMayHaveDirtiedMain). Not provably clean keeps declining, which
		// leaves both engines' modes unchanged.
		if db.txMayHaveDirtiedMain {
			// The dirty side is provable too (DB.txDirtiedMain): every pair is ignored for
			// main. Temp is its own pager and still takes the setter when untouched, which
			// moveTemp models. Attachments are served when attachedPagersProvablyClean.
			// want != WAL applies only with attachments: with none, C's dirty-page gate
			// ignores "=wal" before the WAL-transition error is reached (pager.c:7506,
			// vdbe.c:8095).
			if db.txDirtiedMain && stmt.Schema == "" && (len(db.attached) == 0 || want != journalModeWAL) && attachedPagersProvablyClean(db) {
				switch db.tempJournalMode {
				case "", tempJournalModeUnmodelled, tempJournalModeUnopened:
					moveTemp()
					// Mirror the fully-clean common tail's own attached-database
					// loop below (pragma.go, after this switch) exactly -- the
					// per-attachment update is identical, only the precondition
					// that lets a DIRTIED-MAIN transaction reach it differs.
					for _, a := range db.attached {
						a.journalMode = want
						if a.wdb != nil {
							a.wdb.SetJournalMode(want)
						}
						if a.pager != nil {
							a.pager.SetJournalMode(want)
						}
					}
					return nil
				}
			}
			return fmt.Errorf("engine: unsupported PRAGMA journal_mode=%s inside a transaction that has already written (C SQLite silently ignores it or takes it depending on whether a PAGE was really dirtied -- a zero-row UPDATE is on the other side of that line -- which is pager state this engine does not model)", want)
		}
		if db.inMemory && want != journalModeOff {
			// MEMDB's target restriction applies even in a clean transaction, and before
			// the WAL-transition errors below: "=wal" on :memory: mid-transaction is
			// ignored, not an error.
			applyMemBackedTarget()
			return nil
		}
		if want == journalModeWAL {
			return fmt.Errorf("engine: cannot change into wal mode from within a transaction")
		}
		if cur == journalModeWAL {
			return fmt.Errorf("engine: cannot change out of wal mode from within a transaction")
		}
		// A rollback mode to another rollback mode, with the pager still clean:
		// C SQLite TAKES it, reports the new mode, and it survives the
		// COMMIT. Nothing is on disk yet here either (this engine writes the
		// file at commit), so it is exactly the switch an autocommit assignment
		// makes -- break out of the switch and let that common tail run,
		// attached databases and the temp database included.
	case db.inMemory && want != journalModeOff:
		// Not in a transaction at all (db.inTransaction() above would have
		// caught it otherwise) -- the autocommit mirror of the clean half of
		// that case's own memory check, see applyMemBackedTarget.
		applyMemBackedTarget()
		return nil
	default:
		// On this format no mode touches a journal file; a commit appends one batch to
		// the delta (segment_delta.go).
		//
		//   - delete, truncate, persist and memory differ in C only in the
		//     "<db>-journal" file, so they are taken and reported.
		//   - off changes in-transaction undo, which journalOffUndoDisabled routes.
		//     TestTCLCorpusTxnLockstep catches mistakes here.
		//   - wal is a property of the file; the delta is a write-ahead log, so the
		//     mode is a catalog field (ConvertedCatalog.JournalWAL), and the catalog
		//     change rewrites the file, folding the log in.
		if want == journalModeWAL {
			db.segWAL = true
			db.journalModeName = ""
			// A brand-new Wal is not open yet: C opens it at the first read
			// transaction after this, which is what decides its wal-index. See
			// walIndexKind.
			db.walIndexState = walIndexNotOpened
			return nil
		}
		if db.segWAL {
			db.segWAL = false
		}
		db.journalModeName = want
		moveTemp()
		return nil
	}
	db.journalModeName = want
	moveTemp()
	// An UNQUALIFIED assignment reaches every database attached RIGHT NOW too
	// (see attachedWriteSession for the oracle evidence, and attachedDB
	// .journalMode for why a LATER attachment must not inherit it). A QUALIFIED
	// one never gets here on this session -- ExecArgs delegates it to that
	// attachment's OWN session, whose attached list is empty, so the loop is a
	// no-op there and main is correctly left alone.
	for _, a := range db.attached {
		a.journalMode = want // the seed for a session not yet opened
		if a.wdb != nil {
			a.wdb.SetJournalMode(want)
		}
		if a.pager != nil {
			a.pager.SetJournalMode(want)
		}
	}
	return nil
}

// journalModeOf reports the mode a database is in FROM THE DATABASE ALONE:
// memory-backed, or WAL as its catalog records it (ConvertedCatalog.JournalWAL).
// Which of the four rollback modes a connection chose is not in the file (see
// DB.journalModeName), so this bottoms out at "delete"; journalMode() layers the
// connection's own mode on top.
func journalModeOf(inMemory, wal bool) string {
	switch {
	case inMemory:
		return journalModeMemory
	case wal:
		return journalModeWAL
	default:
		return journalModeDelete
	}
}

// journalMode is the read-side counterpart of DB.journalMode: the mode "PRAGMA
// journal_mode" answers over this snapshot. Same precedence -- memory-backed
// and WAL are properties of the database and outrank whatever the connection
// set (see ReadOnlyPager.journalModeName) -- except "off", which is the one
// mode a memory-backed connection can genuinely be in per DB.journalMode's own
// doc comment; journalModeOf itself stays untouched (its other callers, like
// the wal_checkpoint isWAL check, need WAL-vs-not, not this distinction).
func (p *ReadOnlyPager) journalMode() string {
	if p.inMemory && p.journalModeName == journalModeOff {
		return journalModeOff
	}
	if m := journalModeOf(p.inMemory, p.meta.wal); m != journalModeDelete {
		return m
	}
	if p.journalModeName != "" {
		return p.journalModeName
	}
	return journalModeDelete
}

// SetJournalMode carries a connection's rollback journal mode onto a read-only
// view, so its GETTER reports what the connection set. The write-path
// counterpart is DB.SetJournalMode; see there for why the driver owns this.
func (p *ReadOnlyPager) SetJournalMode(m string) { p.journalModeName = m }

// SetTempStore stamps the connection's "PRAGMA temp_store" value onto this
// snapshot so the GETTER answers from the connection rather than from a freshly
// opened file's default. See DB.TempStore.
func (p *ReadOnlyPager) SetTempStore(v uint8) { p.tempStore = v }

// SetCaseSensitiveLike carries the connection's case_sensitive_like onto a
// read-only view, for the driver's standalone Open() pager an autocommit SELECT
// gets. SnapshotPager stamps it already. Counterpart: DB.SetCaseSensitiveLike.
func (p *ReadOnlyPager) SetCaseSensitiveLike(on bool) { p.caseSensitiveLike = on }

// SetAutomaticIndex / AutomaticIndex carry the connection's automatic_index
// onto a read-only view, for the same reason as SetCaseSensitiveLike; losing it
// changes join row order. Nil-tolerant; ON is the default.
//
// A change invalidates planCache: the flag is read at compile time and baked
// into the Program, and the driver reuses one warm pager across autocommit
// reads.
func (p *ReadOnlyPager) SetAutomaticIndex(on bool) {
	if p.noAutoIndex == on { // the flag is really changing
		p.planCache = nil
	}
	p.noAutoIndex = !on
}

// AutomaticIndex reports the flag; see SetAutomaticIndex.
func (p *ReadOnlyPager) AutomaticIndex() bool { return p == nil || !p.noAutoIndex }

// ReverseUnorderedSelects reports the connection's reverse_unordered_selects
// flag. It rides in PragmaConnState (copied by SnapshotPager, stamped by the
// driver via SetPragmaTuningValues). Read at compile time (where.c:7126; see
// where_plan_gate.go). Nil-tolerant; OFF by default.
func (p *ReadOnlyPager) ReverseUnorderedSelects() bool {
	return p != nil && p.pragmaState.ReverseUnorderedSelects()
}

// SetTempJournalMode carries the connection's TEMP-database journal mode onto a
// read-only view, so "PRAGMA temp.journal_mode" answers what the connection set.
// The write-path counterpart is DB.SetTempJournalMode.
func (p *ReadOnlyPager) SetTempJournalMode(m string) { p.tempJournalMode = m }

// textEncodingByName parses "PRAGMA encoding"'s value as C SQLite's own
// pragma.c does: the eight spellings it recognizes, case-insensitively and with
// the surrounding quotes of a string literal stripped, with the bare "UTF-16"
// meaning NATIVE byte order (little-endian everywhere this engine builds --
// verified: "PRAGMA encoding='UTF-16'" then the getter answers "UTF-16le").
// known=false for anything else, which C SQLite silently ignores.
func textEncodingByName(text string) (enc TextEncoding, known bool) {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(text), `"'`)) {
	case "utf8", "utf-8":
		return UTF8, true
	case "utf16", "utf-16", "utf16le", "utf-16le":
		return UTF16LE, true
	case "utf16be", "utf-16be":
		return UTF16BE, true
	}
	return UTF8, false
}

// hasSchemaObjects reports whether this session's database holds any schema
// object at all -- the condition "PRAGMA encoding" and (in C SQLite) several
// other one-shot pragmas key off. sqlite_sequence is deliberately counted: it
// is a real table in the schema, and its presence means an AUTOINCREMENT table
// existed.
func (db *DB) hasSchemaObjects() bool {
	return len(db.tables) > 0 || len(db.indexes) > 0 || len(db.views) > 0 ||
		len(db.vtabs) > 0 || len(db.triggers) > 0
}

// textEncodingName renders a TextEncoding value as
// PRAGMA encoding's own text spelling, matching C SQLite's exact strings
// (verified directly against mattn/go-sqlite3 for the UTF8 case, the only one
// this engine's own write path ever produces -- see this file's package doc
// comment). UTF16LE/UTF16BE are handled defensively for an externally
// created database this engine's read path happened to open, even though
// nothing in this engine ever WRITES either value itself.
func textEncodingName(enc TextEncoding) string {
	switch enc {
	case UTF16LE:
		return "UTF-16le"
	case UTF16BE:
		return "UTF-16be"
	default:
		return "UTF-8"
	}
}

// findSchemaRowIn finds a schema row by name in one catalog: scopeAny prefers a
// TEMP row over a main one of the same name (unqualified resolution order),
// scopeMain/scopeTemp match only their own. See temp_schema.go.
func findSchemaRowIn(rows []SchemaRow, scope schemaScope, typ, name string) *SchemaRow {
	var main *SchemaRow
	for i := range rows {
		if rows[i].Type != typ || !equalFoldName(rows[i].Name, name) || !scope.accepts(rows[i].Temp) {
			continue
		}
		if rows[i].Temp {
			return &rows[i]
		}
		if main == nil {
			main = &rows[i]
		}
	}
	return main
}

// pragmaTableInfo implements PRAGMA table_info/table_xinfo. A name that
// doesn't resolve to an ordinary table (not found at all, or a view -- this
// engine has no per-column type/nullability story for a view's own computed
// output columns) answers zero rows, not an error, matching C SQLite's
// own "no such table" -> empty-result-set behavior for table_info (verified
// directly); only a genuinely malformed CREATE TABLE text (which should
// never occur for a table this engine itself created) surfaces as an error.
func (p *ReadOnlyPager) pragmaTableInfo(scope schemaScope, tableName string, extended bool) (cols []string, rows [][]Value, err error) {
	if extended {
		cols = []string{"cid", "name", "type", "notnull", "dflt_value", "pk", "hidden"}
	} else {
		cols = []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}
	}
	rows = [][]Value{}
	schemaRows, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	// The SCHEMA CATALOG itself is a legitimate target, and it never has a
	// schema row describing itself (see isMainSchemaCatalogName), so the lookup
	// below would always answer zero rows for it. C SQLite reports its fixed
	// five columns -- verified directly for sqlite_master, sqlite_schema,
	// sqlite_temp_master and sqlite_temp_schema alike, all with notnull=0, no
	// default, pk=0 and (table_xinfo) hidden=0.
	if isMainSchemaCatalogName(tableName) || isTempSchemaCatalogName(tableName) {
		for cid, c := range sqliteSchemaCatalogColumns() {
			row := []Value{
				{Typ: Int, I: int64(cid)},
				{Typ: Text, S: []byte(c.Name)},
				{Typ: Text, S: []byte(c.DeclType)},
				{Typ: Int, I: 0},
				{Typ: Null},
				{Typ: Int, I: 0},
			}
			if extended {
				row = append(row, Value{Typ: Int, I: 0})
			}
			rows = append(rows, row)
		}
		return cols, rows, nil
	}
	tr := findSchemaRowIn(schemaRows, scope, "table", tableName)
	if tr == nil {
		// A VIEW is a legitimate target: C SQLite reports the view's OWN
		// result columns. This engine used to answer zero rows for one, a wrong
		// answer nothing could see (a bare PRAGMA's rows are never compared by
		// the differential corpus) until pragma_table_info made it a SELECT.
		if vr := findSchemaRowIn(schemaRows, scope, "view", tableName); vr != nil {
			return p.pragmaViewInfo(vr, extended)
		}
		// A bare eponymous-only module (json_each, pragma_*) is a valid target: C
		// seeds such modules into the schema at open (sqlite3VtabEponymousTableInit). A
		// module that needs CREATE (rtree, fts3/4) answers zero rows there, which is
		// what a failed Connect below falls through to.
		if mod, ok := lookupVtabModule(tableName); ok {
			if vcols, _, cerr := vtabConnect(p.fts3Catalog(), mod, nil); cerr == nil {
				if extended {
					// table_xinfo would need the module's own HIDDEN columns
					// too (pragmaVirtualTableInfo declines the same shape for
					// a CREATEd virtual table, for the identical reason: this
					// engine's hidden-column sets are not verified against
					// the oracle's for every module). Declined rather than
					// silently answering the table_info-shaped subset.
					return nil, nil, fmt.Errorf("engine: unsupported PRAGMA table_xinfo on eponymous virtual table %s", tableName)
				}
				rows = [][]Value{}
				cid := 0
				for _, c := range vcols {
					if c.Hidden {
						continue
					}
					rows = append(rows, []Value{
						{Typ: Int, I: int64(cid)},
						{Typ: Text, S: []byte(c.Name)},
						{Typ: Text, S: []byte(c.Type)},
						{Typ: Int, I: 0},
						{Typ: Null},
						{Typ: Int, I: 0},
					})
					cid++
				}
				return cols, rows, nil
			}
		}
		return cols, rows, nil
	}
	// A VIRTUAL TABLE's schema row is a CREATE VIRTUAL TABLE, which the CREATE
	// TABLE parser below cannot read. It used to fall through to that parser and
	// come back as an error the caller turned into ZERO ROWS -- so "PRAGMA
	// table_info" on ANY virtual table (rtree, fts3/fts4, fts4aux) silently
	// reported no columns at all where C SQLite lists them. A bare PRAGMA's
	// rows are never compared by the differential corpus, which is why it went
	// unseen; pragma_table_info as a SELECT does compare them.
	if isCreateVirtualTableSQL(tr.SQL) {
		return p.pragmaVirtualTableInfo(tr, tableName, extended)
	}
	def, err := parsePragmaTableDef(tr.SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA table_info: %w", err)
	}
	// table_info omits generated columns and numbers cid contiguously over the
	// rest; table_xinfo reports all of them with hidden 0, 2 (VIRTUAL) or 3
	// (STORED).
	cid := 0
	for _, c := range def.cols {
		if c.generated && !extended {
			continue
		}
		dflt := Value{Typ: Null}
		if c.hasDefault {
			dflt = Value{Typ: Text, S: []byte(c.dflt)}
		}
		row := []Value{
			{Typ: Int, I: int64(cid)},
			{Typ: Text, S: []byte(c.name)},
			{Typ: Text, S: []byte(c.declType)},
			{Typ: Int, I: boolToInt(c.notNull)},
			dflt,
			{Typ: Int, I: int64(def.pkPos[r33sFoldIdent(c.name)])},
		}
		if extended {
			hidden := 0
			switch {
			case c.generated && c.generatedStored:
				hidden = 3
			case c.generated:
				hidden = 2
			}
			row = append(row, Value{Typ: Int, I: int64(hidden)})
		}
		rows = append(rows, row)
		cid++
	}
	return cols, rows, nil
}

// pragmaVirtualTableInfo reports a virtual table's columns from the module's
// declared schema: fts3/fts4 user columns with empty type, rtree's id INT and
// coordinates REAL, fts4aux's four columns; notnull/dflt/pk are 0/NULL/0.
// table_xinfo adds hidden columns in C's declared order
// (vtabDeclaredColumnsAsSQLiteReportsThem).
func (p *ReadOnlyPager) pragmaVirtualTableInfo(tr *SchemaRow, tableName string, extended bool) (cols []string, rows [][]Value, err error) {
	pragma := "table_info"
	if extended {
		pragma = "table_xinfo"
	}
	_, module, args, _, perr := parseCreateVirtualTableStmt(tr.SQL)
	if perr != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA %s: %w", pragma, perr)
	}
	mod, ok := lookupVtabModule(module)
	if !ok {
		return nil, nil, fmt.Errorf("engine: no such module: %s", module)
	}
	vcols, _, cerr := vtabConnect(p.fts3Catalog(), mod, args)
	if cerr != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA %s: %s: %w", pragma, tableName, cerr)
	}
	cols = []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}
	if extended {
		cols = append(cols, "hidden")
		declared, ok := vtabDeclaredColumnsAsSQLiteReportsThem(module, tableName, vcols)
		if !ok {
			return nil, nil, fmt.Errorf("engine: unsupported PRAGMA table_xinfo on virtual table %s (this engine has not verified module %q's declared column list against C SQLite's)", tableName, module)
		}
		vcols = declared
	}
	rows = [][]Value{}
	cid := 0
	for _, c := range vcols {
		if c.Hidden && !extended {
			continue
		}
		row := []Value{
			{Typ: Int, I: int64(cid)},
			{Typ: Text, S: []byte(c.Name)},
			{Typ: Text, S: []byte(c.Type)},
			{Typ: Int, I: 0},
			{Typ: Null},
			{Typ: Int, I: 0},
		}
		if extended {
			hidden := 0
			if c.Hidden {
				hidden = 1
			}
			row = append(row, Value{Typ: Int, I: int64(hidden)})
		}
		rows = append(rows, row)
		cid++
	}
	return cols, rows, nil
}

// vtabDeclaredColumnsAsSQLiteReportsThem maps a module's columns as this engine
// models them onto C's declared list, which table_xinfo reports. ok is false for
// an unverified module.
//
// fts3/fts4 and fts5 differ: this engine models a leading hidden rowid slot
// (vtabSyntheticRowidSlot) and no command column, while C declares user columns
// first:
//
//   - fts3/fts4 (fts3.c:651): "x(<cols>, <tablename> HIDDEN, docid HIDDEN,
//     <langid> HIDDEN)", <langid> being "languageid=" or "__langid".
//   - fts5 (fts5_config.c:760): "x(<cols>, <name> HIDDEN, rank HIDDEN)".
//
// rtree, fts4aux and fts3tokenize already match C.
func vtabDeclaredColumnsAsSQLiteReportsThem(module, tableName string, vcols []VtabColumn) ([]VtabColumn, bool) {
	hide := func(name string) VtabColumn { return VtabColumn{Name: name, Hidden: true} }
	switch strings.ToLower(module) {
	case "fts3", "fts4":
		// [docid HIDDEN, cols..., (langid HIDDEN)] -> C's order.
		if len(vcols) == 0 {
			return nil, false
		}
		user, langid := vcols[1:], "__langid"
		if n := len(user); n > 0 && user[n-1].Hidden {
			langid, user = user[n-1].Name, user[:n-1]
		}
		out := make([]VtabColumn, 0, len(user)+3)
		for _, c := range user {
			out = append(out, VtabColumn{Name: c.Name}) // C reports an EMPTY type for every fts3 column
		}
		return append(out, hide(tableName), hide("docid"), hide(langid)), true
	case "fts5":
		// [rowid HIDDEN, cols...] -> C's order.
		if len(vcols) == 0 {
			return nil, false
		}
		out := make([]VtabColumn, 0, len(vcols)+1)
		for _, c := range vcols[1:] {
			out = append(out, VtabColumn{Name: c.Name})
		}
		return append(out, hide(tableName), hide("rank")), true
	case "rtree", "rtree_i32", "fts4aux", "fts3tokenize":
		return vcols, true
	}
	return nil, false
}

// pragmaViewInfo is table_info/table_xinfo over a view: names from the view's
// column list, else its select list (duplicates as-is); notnull/dflt/pk and
// hidden are always 0/NULL/0/0; types from pragmaViewColumnType. A compound
// view folds its arms' types (compoundColumnTypes, as
// sqlite3SubqueryColumnTypes with SQLITE_AFF_NONE). A FROM over another view,
// CTE or derived table is declined.
func (p *ReadOnlyPager) pragmaViewInfo(vr *SchemaRow, extended bool) (cols []string, rows [][]Value, err error) {
	if extended {
		cols = []string{"cid", "name", "type", "notnull", "dflt_value", "pk", "hidden"}
	} else {
		cols = []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}
	}
	pcv, perr := parseCreateViewStmt(vr.SQL)
	if perr != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA table_info: view %s: %w", vr.Name, perr)
	}
	sel := pcv.selectStmt
	if sel == nil {
		return nil, nil, fmt.Errorf("%w: PRAGMA table_info over view %s", errVDBEUnsupported, vr.Name)
	}
	// The deferred half of parseCreateViewStmt's own permissiveness: an
	// unrecognized COLLATE name anywhere in this view's body is retained,
	// unresolved, until the view is genuinely referenced -- which naming its
	// columns is (C SQLite's build.c:3115-3183 sqlite3ViewGetColumnNames,
	// called here too). See view_collate.go: measured directly against real
	// SQLite, a bare "PRAGMA table_info(v)" over "CREATE VIEW v AS SELECT a
	// COLLATE bogus FROM t" already fails with no SELECT anywhere.
	if name := firstUnknownViewCollation(sel); name != "" {
		return nil, nil, fmt.Errorf("engine: PRAGMA table_info: view %s: %w", vr.Name, errUnknownViewCollation(name))
	}
	// Naming a view's columns resolves its body, so trusted_schema=OFF applies. C
	// caches resolved view columns per connection and then skips the check; this
	// engine has no cache, so it matches a fresh connection and declines where the
	// cache would answer.
	if err := p.checkTrustedSchemaSelect(vr.Temp, sel); err != nil {
		return nil, nil, err
	}
	scopes, serr := p.subqueryScopes(sel.From)
	if serr != nil {
		return nil, nil, fmt.Errorf("%w: PRAGMA table_info over view %s: %v", errVDBEUnsupported, vr.Name, serr)
	}
	mode := p.colNameMode()
	mode.subqueryCols = true // a view's columns are sqlite3ColumnsFromExprList's, not a result set's
	outCols, oerr := expandSelectList(sel.Columns, scopes, mode)
	if oerr != nil {
		return nil, nil, fmt.Errorf("%w: PRAGMA table_info over view %s: %v", errVDBEUnsupported, vr.Name, oerr)
	}
	if pcv.colNames != nil && len(pcv.colNames) != len(outCols) {
		// C SQLite rejects such a view at CREATE time, so this is defensive.
		return nil, nil, fmt.Errorf("engine: PRAGMA table_info: view %s has %d columns but its SELECT produces %d",
			vr.Name, len(pcv.colNames), len(outCols))
	}
	// A COMPOUND view's per-column type is folded across every arm; a simple
	// one's is just its own expression's. Both then run the same declared-type
	// tail (viewColumnTypeName) over the LEFTMOST arm's expression -- which,
	// for a compound, is sel's own select list.
	var folded []compoundColumnType
	if len(sel.Compound) != 0 {
		fold, ok := p.compoundColumnTypes(sel, false) // a VIEW's default is SQLITE_AFF_NONE
		if !ok || len(fold) != len(outCols) {
			return nil, nil, fmt.Errorf("%w: PRAGMA table_info over a COMPOUND view (%s) whose arms are not analyzable", errVDBEUnsupported, vr.Name)
		}
		folded = fold
	}
	// A case-insensitive duplicate among a view's column names is ":N"-renamed,
	// whether the names come from the body's result set or from an explicit
	// "(col, ...)" list: sqlite3ViewGetColumnNames (build.c:3175) hands
	// EITHER list to the one sqlite3ColumnsFromExprList, so table_info reports
	// "a","a:1" for "CREATE VIEW v AS SELECT 1 AS a, 2 AS a" and "x","x:1" for
	// "CREATE VIEW v(x,x) AS SELECT 1,2". This used to decline instead, on the
	// recorded grounds that the ":N" suffix was not reproduced.
	viewColNames := make([]string, len(outCols))
	for i, oc := range outCols {
		viewColNames[i] = oc.name
		if pcv.colNames != nil {
			viewColNames[i] = pcv.colNames[i]
		}
	}
	uniqNames, uniqOK := r32mUniqueColumnNames(viewColNames)
	if !uniqOK {
		// Six or more repeats: SQLite names the sixth from
		// sqlite3_randomness, so there is nothing to match.
		return nil, nil, fmt.Errorf("%w: PRAGMA table_info over view %s: a result-column name is repeated six or more times (SQLite names the sixth from sqlite3_randomness)", errVDBEUnsupported, vr.Name)
	}
	rows = [][]Value{}
	for i, oc := range outCols {
		name := uniqNames[i]
		typ := ""
		if folded != nil {
			typ = p.viewColumnTypeName(scopes, oc.expr, folded[i])
		} else {
			typ = p.pragmaViewColumnType(scopes, oc.expr)
		}
		row := []Value{
			{Typ: Int, I: int64(i)},
			{Typ: Text, S: []byte(name)},
			{Typ: Text, S: []byte(typ)},
			{Typ: Int, I: 0},
			{Typ: Null},
			{Typ: Int, I: 0},
		}
		if extended {
			row = append(row, Value{Typ: Int, I: 0})
		}
		rows = append(rows, row)
	}
	return cols, rows, nil
}

// pragmaViewColumnType is the "type" table_info reports for a view column, a
// port of sqlite3SubqueryColumnTypes' declared-type tail:
//
//	zType = columnType(expr)
//	if zType==0 || sqlite3AffinityType(zType) != affinity:
//	    zType = "NUM" for NUMERIC/FLEXNUM, else the FIRST of
//	            BLOB/INT/INTEGER/REAL/TEXT whose standard affinity matches,
//	            else nothing at all
//
// columnTypeImpl has two cases: a column reference (its declared type, or
// INTEGER for rowid) and a subquery (recurse into its first result). Anything
// else, including CAST and COLLATE, falls to the affinity name.
func (p *ReadOnlyPager) pragmaViewColumnType(scopes []tableScope, e Expr) string {
	ctx := &evalCtx{tables: scopes, pager: p}
	aff, blob := viewColumnAffinity(ctx, e)
	return p.viewColumnTypeName(scopes, e, compoundColumnType{aff: aff, blob: blob})
}

// viewColumnTypeName is sqlite3SubqueryColumnTypes' declared-type tail alone,
// given a column's already-folded type: the COMPOUND path (pragmaViewInfo)
// computes t across every arm and reuses this, since the tail itself is the
// same code in SQLite -- "zType = columnType(A0[i])" always names the LEFTMOST
// arm's expression, in the leftmost arm's own FROM scope.
func (p *ReadOnlyPager) viewColumnTypeName(scopes []tableScope, arm0 Expr, t compoundColumnType) string {
	if !t.flexnum {
		// SQLITE_AFF_FLEXNUM is 0x46, which sqlite3AffinityType can never
		// return, so a FLEXNUM column always fails this test and always ends
		// up reported as "NUM".
		if zType, ok := p.viewColumnDeclType(scopes, arm0, 0); ok {
			// sqlite3AffinityType never returns AFF_NONE either, so a declared
			// type can only agree with an expression whose own affinity is
			// real -- or, where this package's affNone stands for AFF_BLOB,
			// with a BLOB one.
			if a := typeAffinity(zType); a == t.aff && (t.aff != affNone || t.blob) {
				return zType
			}
		}
	}
	if t.flexnum {
		return "NUM"
	}
	if t.aff == affNone && !t.blob {
		return "" // AFF_NONE matches no standard type name
	}
	return affinityTypeName(t.aff)
}

// viewColumnAffinity is sqlite3ExprAffinity for one view/derived-table result
// expression, plus the bit this package's affinity enum cannot hold: whether
// the answer is SQLite's AFF_BLOB (a typeless COLUMN, or a CAST to a typeless
// name) rather than AFF_NONE (a computed value). The two are indistinguishable
// in a coercion and NOT in a reported type -- AFF_BLOB prints "BLOB", AFF_NONE
// prints nothing.
func viewColumnAffinity(ctx *evalCtx, e Expr) (affinity, bool) {
	// sqlite3ExprAffinity's EP_Skip loop peels COLLATE, and only COLLATE: a
	// unary "+" HIDES its operand's affinity there (see armColClassify).
	for {
		c, isColl := e.(CollateExpr)
		if !isColl {
			break
		}
		e = c.X
	}
	if aff := exprAffinity(ctx, e); aff != affNone {
		return aff, false
	}
	switch x := e.(type) {
	case CastExpr:
		return affNone, true // CAST to BLOB, or to any name with no other affinity
	case ColumnExpr:
		if _, _, col, _, err := resolveColumn(ctx, x.Qualifier, x.Name); err == nil && col != nil {
			// columnInfo.NoAffinity is exactly this distinction, already
			// computed for a derived table's columns by derivedColumnComputed.
			return affNone, !col.NoAffinity
		}
		if isRowidRefIn(ctx.tables, x) {
			return affInteger, false
		}
		return affNone, false
	case SubqueryExpr:
		if ci, ok := scalarSubqueryColumn(ctx, x.Stmt); ok {
			return affNone, !ci.NoAffinity
		}
	}
	return affNone, false
}

// viewColumnDeclType is columnTypeImpl (c:150913): the declared type text
// SQLite recovers for one result expression, and whether it found one at all.
// depth bounds the TK_SELECT recursion, which is otherwise unbounded on a
// mutually-referencing schema.
func (p *ReadOnlyPager) viewColumnDeclType(scopes []tableScope, e Expr, depth int) (string, bool) {
	if depth > 8 {
		return "", false
	}
	switch x := e.(type) {
	case ColumnExpr:
		for _, ts := range scopes {
			if x.Qualifier != "" && !equalFoldName(ts.name, x.Qualifier) {
				continue
			}
			idx, ok := ts.colIndex[r33sFoldIdent(x.Name)]
			if !ok || idx >= len(ts.cols) {
				continue
			}
			declared := strings.TrimSpace(ts.cols[idx].DeclType)
			if declared == "" {
				return "", false // sqlite3ColumnType's zDflt is 0 here
			}
			if canon, isStd := pragmaStdTypeNames[strings.ToUpper(declared)]; isStd {
				return canon, true
			}
			return declared, true
		}
		if isRowidRefIn(scopes, x) {
			// columnTypeImpl's "if( iCol<0 ) zType = "INTEGER"". Every
			// spelling, and whether or not the table has an INTEGER PRIMARY
			// KEY: "SELECT rowid FROM k" and "SELECT oid FROM k" both report
			// INTEGER (verified against 3.53.3).
			return "INTEGER", true
		}
		return "", false
	case SubqueryExpr:
		if x.Stmt == nil || len(x.Stmt.Columns) == 0 {
			return "", false
		}
		// columnTypeImpl's TK_SELECT case reads pExpr->x.pSelect, which for a
		// COMPOUND subquery is the TOP of the pPrior chain -- the RIGHTMOST
		// arm, not the leftmost sqlite3SubqueryColumnTypes itself walks to.
		core := x.Stmt
		if n := len(core.Compound); n != 0 {
			if core.Compound[n-1].Stmt == nil {
				return "", false
			}
			core = core.Compound[n-1].Stmt
		}
		subScopes, err := p.subqueryScopes(core.From)
		if err != nil {
			return "", false
		}
		outCols, oerr := expandSelectList(core.Columns, subScopes, defaultColNameMode)
		if oerr != nil || len(outCols) == 0 {
			return "", false
		}
		return p.viewColumnDeclType(subScopes, outCols[0].expr, depth+1)
	default:
		return "", false
	}
}

// isRowidRefIn reports whether x is a bare (or singly-qualified) rowid/oid/
// _rowid_ reference to one of scopes' real tables -- the shape columnTypeImpl
// sees as iCol<0.
func isRowidRefIn(scopes []tableScope, x ColumnExpr) bool {
	if !isRowidAliasName(x.Name) {
		return false
	}
	for _, ts := range scopes {
		if x.Qualifier != "" && !equalFoldName(ts.name, x.Qualifier) {
			continue
		}
		if _, shadowed := ts.colIndex[r33sFoldIdent(x.Name)]; shadowed {
			continue
		}
		return true
	}
	return false
}

// affinityTypeName is the five names SQLite reports an AFFINITY by -- the
// spellings PRAGMA table_info uses for a view column whose value comes from a
// CAST (see pragmaViewColumnType). Note NUMERIC's name is the abbreviated "NUM"
// and INTEGER's is "INT", neither of which matches the affinity's own
// identifier.
func affinityTypeName(a affinity) string {
	switch a {
	case affText:
		return "TEXT"
	case affNumeric:
		return "NUM"
	case affInteger:
		return "INT"
	case affReal:
		return "REAL"
	default:
		return "BLOB"
	}
}

// pragmaIndexList implements PRAGMA index_list. Row order matches real
// SQLite's own observed (not otherwise documented) behavior exactly: every
// explicit (CREATE INDEX-produced) index in REVERSE creation order, followed
// by every automatic (UNIQUE/PRIMARY KEY-derived) index in REVERSE
// declaration order -- verified directly against mattn/go-sqlite3 (real
// SQLite prepends each newly registered index to the table's own index
// list). A table name that doesn't resolve answers zero rows, not an error.
func (p *ReadOnlyPager) pragmaIndexList(scope schemaScope, tableName string) (cols []string, rows [][]Value, err error) {
	cols = []string{"seq", "name", "unique", "origin", "partial"}
	rows = [][]Value{}
	schemaRows, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	tr := findSchemaRowIn(schemaRows, scope, "table", tableName)
	if tr == nil {
		return cols, rows, nil
	}
	if isCreateVirtualTableSQL(tr.SQL) {
		// A VIRTUAL table has no Index list of its own -- pTab->pIndex is
		// null for one -- so PragTyp_INDEX_LIST's loop (pragma.c:1423) runs
		// zero times and the pragma answers its header and nothing else.
		// Its SHADOW tables are ordinary tables and answer normally.
		return cols, rows, nil
	}
	// An index row names its table only by tbl_name, so once BOTH catalogs
	// hold a table of this name there is no way to tell which of them a given
	// index belongs to -- the interchangeable file format has nowhere to record
	// it (see markTempSchemaRows, temp_schema.go). Declined rather than
	// answered with a list that may attribute the other catalog's indexes here.
	if findSchemaRowIn(schemaRows, scopeMain, "table", tableName) != nil &&
		findSchemaRowIn(schemaRows, scopeTemp, "table", tableName) != nil {
		return nil, nil, fmt.Errorf("engine: unsupported: PRAGMA index_list(%s): a temp and a main table share this name, and an index row records only its tbl_name -- which of the two it belongs to is not recoverable", tableName)
	}

	type item struct {
		name            string
		unique, partial bool
		origin          string
	}
	var explicit []item
	for _, r := range schemaRows {
		if r.Type == "index" && equalFoldName(r.TblName, tr.Name) && r.Temp == tr.Temp && r.SQL != "" {
			uniq, partial := parsePragmaIndexHeader(r.SQL)
			explicit = append(explicit, item{name: r.Name, unique: uniq, partial: partial, origin: "c"})
		}
	}
	reverseInPlace(explicit)

	def, err := parsePragmaTableDef(tr.SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA index_list: %w", err)
	}
	var autos []item
	for i, spec := range def.autoIdx {
		autos = append(autos, item{
			name:   fmt.Sprintf("sqlite_autoindex_%s_%d", tr.Name, i+1),
			unique: true,
			origin: spec.kind,
		})
	}
	reverseInPlace(autos)

	all := append(explicit, autos...)
	for seq, it := range all {
		rows = append(rows, []Value{
			{Typ: Int, I: int64(seq)},
			{Typ: Text, S: []byte(it.name)},
			{Typ: Int, I: boolToInt(it.unique)},
			{Typ: Text, S: []byte(it.origin)},
			{Typ: Int, I: boolToInt(it.partial)},
		})
	}
	return cols, rows, nil
}

// reverseInPlace reverses s in place; a small generic helper local to this
// file (pragmaIndexList's explicit/automatic index ordering).
func reverseInPlace[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// withoutRowidAutoPKOwner resolves "sqlite_autoindex_<table>_<N>" to its table
// when it names a WITHOUT ROWID table's PK index, which has no schema row in C
// (it is the table's b-tree) yet is listed and findable. Lets
// index_info/index_xinfo answer for it.
func withoutRowidAutoPKOwner(schemaRows []SchemaRow, scope schemaScope, idxName string) *SchemaRow {
	n := autoIndexSuffixNumber(idxName)
	if n < 1 {
		return nil
	}
	for i := range schemaRows {
		tr := &schemaRows[i]
		if tr.Type != "table" || !scope.accepts(tr.Temp) {
			continue
		}
		if _, ok := autoIndexNameSuffix(idxName, tr.Name); !ok {
			continue
		}
		if !sqlTextTableIsWithoutRowid(tr.SQL) {
			return nil
		}
		def, derr := parsePragmaTableDef(tr.SQL)
		if derr != nil || n > len(def.autoIdx) || def.autoIdx[n-1].kind != "pk" {
			return nil
		}
		return tr
	}
	return nil
}

// pragmaIndexInfo implements PRAGMA index_info/index_xinfo. An index name
// that doesn't resolve answers zero rows, not an error. index_xinfo always
// appends one trailing pseudo-column (cid -1, name NULL, key 0) representing
// the implicit rowid every rowid-table index's b-tree key ends with --
// verified directly against mattn/go-sqlite3, including for an index that
// already covers every one of its table's columns.
func (p *ReadOnlyPager) pragmaIndexInfo(scope schemaScope, idxName string, extended bool) (cols []string, rows [][]Value, err error) {
	if extended {
		cols = []string{"seqno", "cid", "name", "desc", "coll", "key"}
	} else {
		cols = []string{"seqno", "cid", "name"}
	}
	rows = [][]Value{}
	schemaRows, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	ir := findSchemaRowIn(schemaRows, scope, "index", idxName)
	// A WITHOUT ROWID table name describes its PK index, C's fallback in pragma.c:
	//
	//	pIdx = sqlite3FindIndex(db, zRight, zDb);
	//	if( pIdx==0 ){
	//	  pTab = sqlite3LocateTable(pParse, LOCATE_NOERR, zRight, zDb);
	//	  if( pTab && !HasRowid(pTab) ){ pIdx = sqlite3PrimaryKeyIndex(pTab); }
	//	}
	//
	// A rowid table still answers nothing.
	var pkOfTable *SchemaRow
	if ir == nil {
		tr := findSchemaRowIn(schemaRows, scope, "table", idxName)
		if tr == nil {
			// ...and the same index asked for by its own NAME. A WITHOUT
			// ROWID table's PRIMARY KEY index is the one index C SQLite
			// NAMES but writes no sqlite_schema row for, so findSchemaRowIn
			// cannot see it and the table-name fallback above does not fire
			// either. C has no such gap: sqlite3FindIndex reads the schema's
			// idxHash, which holds that index like any other.
			tr = withoutRowidAutoPKOwner(schemaRows, scope, idxName)
		}
		if tr == nil || !sqlTextTableIsWithoutRowid(tr.SQL) {
			return cols, rows, nil
		}
		pkOfTable = tr
	}
	tr := pkOfTable
	if tr == nil {
		tr = findSchemaRowIn(schemaRows, createScope(ir.Temp), "table", ir.TblName)
	}
	if tr == nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA index_info: index %s references unknown table %s", idxName, ir.TblName)
	}
	tblDef, err := parsePragmaTableDef(tr.SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA index_info: %w", err)
	}
	// An EXPRESSION key entry (parsePragmaIndexColumns reports it as the empty
	// name -- see indexKeyColumnName) is cid -2 with a NULL name, C SQLite's
	// own encoding for "this key is an expression, not a column".
	cidOf := func(name string) int64 {
		if name == "" {
			return -2
		}
		for i, c := range tblDef.cols {
			if equalFoldName(c.name, name) {
				return int64(i)
			}
		}
		return -1
	}

	// collOf is index_xinfo's "coll" column: the key's EFFECTIVE collating
	// sequence, reported with the letter case the source text used rather than
	// normalized. Every clause verified directly against mattn/go-sqlite3 over
	// "t(a, b TEXT collate nocase, c TEXT COLLATE RtRiM)":
	//
	//   - the key's OWN "COLLATE x" wins, spelled as written -- "ON t(b COLLATE
	//     rtrim)" reports "rtrim", and "ON t(a COLLATE BINARY)" over a NOCASE
	//     column reports "BINARY";
	//   - otherwise the COLUMN's declared COLLATE, also as written -- "ON t(b)"
	//     reports "nocase" and "ON t(c)" reports "RtRiM". Quoting the key name
	//     changes nothing ("ON t(\"b\")" still reports "nocase");
	//   - otherwise "BINARY";
	//   - and an EXPRESSION key is ALWAYS "BINARY", even when it writes its own
	//     COLLATE: "ON t(a+1 COLLATE NoCase)" reports "BINARY" (which is what
	//     index_write.go's own expression-key comment already recorded).
	//
	// An AUTOMATIC index's key carries its constraint's own COLLATE just as a
	// CREATE INDEX key does: C's sqlite3AddPrimaryKey hands the constraint's
	// list to sqlite3CreateIndex (build.c:1889), which takes a key's
	// TK_COLLATE before the column's (build.c:4252-4263) -- so
	// "PRIMARY KEY(a, b COLLATE NOCASE)" reports NOCASE for b, and enforces it.
	collOf := func(kc pragmaIndexCol) string {
		if kc.name == "" {
			return "BINARY"
		}
		if kc.coll != "" {
			return kc.coll
		}
		for _, c := range tblDef.cols {
			if equalFoldName(c.name, kc.name) && c.collateSrc != "" {
				return c.collateSrc
			}
		}
		return "BINARY"
	}

	// pkSpec is the table's PRIMARY KEY automatic-index entry, present for
	// every WITHOUT ROWID table (parsePragmaTableDef's own doc comment records
	// why it is reconstructed even though sqlite_schema has no row for it).
	pkSpec := func() *pragmaAutoIndexSpec {
		for i := range tblDef.autoIdx {
			if tblDef.autoIdx[i].kind == "pk" {
				return &tblDef.autoIdx[i]
			}
		}
		return nil
	}

	var keyCols []pragmaIndexCol
	if pkOfTable != nil {
		spec := pkSpec()
		if spec == nil {
			return nil, nil, fmt.Errorf("engine: PRAGMA index_info: WITHOUT ROWID table %s has no PRIMARY KEY", idxName)
		}
		for ci, nm := range spec.cols {
			keyCols = append(keyCols, pragmaIndexCol{name: nm, desc: ci < len(spec.desc) && spec.desc[ci], coll: atOrEmpty(spec.coll, ci)})
		}
	} else if ir.SQL != "" {
		names, descs, colls, _, perr := parsePragmaIndexColumns(ir.SQL)
		if perr != nil {
			return nil, nil, fmt.Errorf("engine: PRAGMA index_info: %w", perr)
		}
		for i, nm := range names {
			keyCols = append(keyCols, pragmaIndexCol{name: nm, desc: descs[i], coll: atOrEmpty(colls, i)})
		}
	} else {
		n := autoIndexSuffixNumber(ir.Name)
		if n < 1 || n > len(tblDef.autoIdx) {
			return nil, nil, fmt.Errorf("engine: PRAGMA index_info: cannot resolve automatic index %s", idxName)
		}
		spec := tblDef.autoIdx[n-1]
		for ci, nm := range spec.cols {
			// An automatic index keeps its constraint's own per-column DESC:
			// C SQLite reports desc=1 for "UNIQUE(a DESC)"'s
			// sqlite_autoindex too (verified via PRAGMA index_xinfo).
			keyCols = append(keyCols, pragmaIndexCol{name: nm, desc: ci < len(spec.desc) && spec.desc[ci], coll: atOrEmpty(spec.coll, ci)})
		}
	}

	for i, kc := range keyCols {
		nameVal := Value{Typ: Text, S: []byte(kc.name)}
		if kc.name == "" {
			nameVal = Value{Typ: Null}
		}
		row := []Value{
			{Typ: Int, I: int64(i)},
			{Typ: Int, I: cidOf(kc.name)},
			nameVal,
		}
		if extended {
			row = append(row, Value{Typ: Int, I: boolToInt(kc.desc)}, Value{Typ: Text, S: []byte(collOf(kc))}, Value{Typ: Int, I: 1})
		}
		rows = append(rows, row)
	}
	if extended {
		// index_xinfo's trailing key=0 rows are what identifies the table row: the
		// rowid (cid -1, NULL name) for a rowid table, the PK columns for a WITHOUT
		// ROWID table. The PK index itself (idxName named the table) is the table's
		// b-tree, so it carries every other column, in table order.
		var covered []pragmaIndexCol
		if sqlTextTableIsWithoutRowid(tr.SQL) {
			inKey := func(name string) bool {
				for _, kc := range keyCols {
					if equalFoldName(kc.name, name) {
						return true
					}
				}
				return false
			}
			if pkOfTable != nil {
				for _, c := range tblDef.cols {
					if !inKey(c.name) {
						covered = append(covered, pragmaIndexCol{name: c.name})
					}
				}
			} else if spec := pkSpec(); spec != nil {
				// A secondary index that already covers a PK column does not
				// repeat it -- sqlite3CreateIndex appends only the missing ones.
				for _, nm := range spec.cols {
					if !inKey(nm) {
						covered = append(covered, pragmaIndexCol{name: nm})
					}
				}
			}
		} else {
			covered = []pragmaIndexCol{{name: ""}} // the implicit rowid
		}
		for i, cc := range covered {
			seqno := int64(len(keyCols) + i)
			if cc.name == "" {
				rows = append(rows, []Value{
					{Typ: Int, I: seqno},
					{Typ: Int, I: -1},
					{Typ: Null},
					{Typ: Int, I: 0},
					{Typ: Text, S: []byte("BINARY")},
					{Typ: Int, I: 0},
				})
				continue
			}
			rows = append(rows, []Value{
				{Typ: Int, I: seqno},
				{Typ: Int, I: cidOf(cc.name)},
				{Typ: Text, S: []byte(cc.name)},
				{Typ: Int, I: 0},
				{Typ: Text, S: []byte(collOf(cc))},
				{Typ: Int, I: 0},
			})
		}
	}
	return cols, rows, nil
}

// pragmaForeignKeyList implements PRAGMA foreign_key_list. A table name that
// doesn't resolve, or one with no FOREIGN KEY clauses at all, both correctly
// answer zero rows (the latter is C SQLite's own behavior for any
// ordinary table, not a special case here). Row order/id numbering and the
// "to"/"match" columns match C SQLite's own observed behavior exactly --
// see this file's package doc comment.
func (p *ReadOnlyPager) pragmaForeignKeyList(scope schemaScope, tableName string) (cols []string, rows [][]Value, err error) {
	cols = []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"}
	rows = [][]Value{}
	schemaRows, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	tr := findSchemaRowIn(schemaRows, scope, "table", tableName)
	if tr == nil {
		return cols, rows, nil
	}
	if isCreateVirtualTableSQL(tr.SQL) {
		// "pTab && IsOrdinaryTable(pTab)" (pragma.c:1510): a virtual table
		// declares no foreign keys, so the pragma answers its header alone.
		return cols, rows, nil
	}
	def, err := parsePragmaTableDef(tr.SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA foreign_key_list: %w", err)
	}
	n := len(def.fks)
	for declIdx := n - 1; declIdx >= 0; declIdx-- {
		fk := def.fks[declIdx]
		id := int64(n - 1 - declIdx)
		for seq, fromCol := range fk.fromCols {
			toVal := Value{Typ: Null}
			if seq < len(fk.toCols) {
				toVal = Value{Typ: Text, S: []byte(fk.toCols[seq])}
			}
			rows = append(rows, []Value{
				{Typ: Int, I: id},
				{Typ: Int, I: int64(seq)},
				{Typ: Text, S: []byte(fk.toTable)},
				{Typ: Text, S: []byte(fromCol)},
				toVal,
				{Typ: Text, S: []byte(fk.onUpdate)},
				{Typ: Text, S: []byte(fk.onDelete)},
				{Typ: Text, S: []byte("NONE")},
			})
		}
	}
	return cols, rows, nil
}

// TextEncodingByName exposes this package's "PRAGMA encoding" value parse to the
// driver layer, which owns the connection-level request (see driver's
// Conn.textEncoding) and must agree with execPragma about which spellings mean
// what.
func TextEncodingByName(text string) (TextEncoding, bool) { return textEncodingByName(text) }

// execAutoVacuum implements "PRAGMA [db.]auto_vacuum [= MODE]": the getter is
// the database's mode; the setter follows sqlite3BtreeSetAutoVacuum plus
// pragma.c's meta[6] write, with four outcomes measured on C:
//
//  1. no page 1 yet: converts now;
//  2. already auto-vacuum and mode non-zero: full <-> incremental switches in
//     place;
//  3. otherwise a change is ignored but remembered for the next VACUUM (setting
//     it back to the current mode clears the request);
//  4. mode 0 on a non-auto-vacuum database: nothing.
func (db *DB) execAutoVacuum(stmt *PragmaStmt) error {
	if !stmt.HasValue {
		return nil // the getter; its row is queryPragma's
	}
	mode := autoVacuumModeOf(stmt.ValueText)
	// C's test is "page 1 was never written" (BTS_PAGESIZE_FIXED). Here: no schema
	// objects, and user_version, application_id and WAL mode untouched (each would
	// have written page 1 in C). The mode is a catalog field
	// (ConvertedCatalog.AutoVacuumPlus1).
	fresh := !db.hasSchemaObjects() && db.userVersion == 0 && db.applicationID == 0 && !db.segWAL
	switch {
	case db.autoVacuum != 0 && mode != 0:
		db.autoVacuum = mode          // case 2: switch full<->incremental in place
		db.autoVacuumPendingPlus1 = 0 // ...and drop any deferred request
	case db.autoVacuum != 0:
		// Turning it OFF is never immediate and never ambiguous: a database can
		// only BE auto-vacuum if it already has a page 1, so BTS_PAGESIZE_FIXED
		// is necessarily set and the setter is necessarily the remembered kind.
		db.autoVacuumPendingPlus1 = mode + 1 // case 3
	case mode == 0:
		db.autoVacuumPendingPlus1 = 0 // case 4
	case fresh:
		db.autoVacuum = mode // case 1: this session creates page 1
		db.autoVacuumPendingPlus1 = 0
	default:
		// Page 1 exists in C terms, so the setter is remembered for the next VACUUM
		// (case 3).
		db.autoVacuumPendingPlus1 = mode + 1 // case 3
	}
	return nil
}

// AutoVacuumPending reports this session's deferred auto_vacuum request, and
// SetAutoVacuumPending carries one in. They exist for the driver, whose
// autocommit model opens a fresh session per statement and so owns the
// connection-level state itself -- exactly like foreign_keys.
func (db *DB) AutoVacuumPending() int     { return db.autoVacuumPendingPlus1 }
func (db *DB) SetAutoVacuumPending(v int) { db.autoVacuumPendingPlus1 = v }

// secureDeleteValue resolves the pragma's payload to 0, 1 or 2 -- "fast" is
// the only spelling that is this pragma's own; everything else is
// sqlite3GetBoolean's (pragma.c:1164's PragTyp_FLAG default of 0).
func secureDeleteValue(text string) int {
	if strings.EqualFold(strings.TrimSpace(text), "fast") {
		return 2
	}
	if pragmaGetBoolean(text, false) {
		return 1
	}
	return 0
}

// SetSecureDelete carries the connection's "PRAGMA secure_delete" setting onto a
// session, and the ReadOnlyPager overload onto a read snapshot so the GETTER can
// be answered there. Both exist because the driver's autocommit model throws the
// session the setter ran on away after one statement.
func (db *DB) SetSecureDelete(v int)           { db.secureDelete = v }
func (p *ReadOnlyPager) SetSecureDelete(v int) { p.secureDelete = v }

// autoVacuumResult is the READ side of the same pragma. It answers the getter
// and declines every assignment, whatever the mode resolves to: a read snapshot
// cannot record the deferred request the write side keeps, so accepting one here
// would silently DROP it and let the next VACUUM through. Every driver path
// routes the assignment through the write side first (driver's
// zeroRowSetterPragmas), so nothing reaches this that should have been applied.
func autoVacuumResult(stmt *PragmaStmt, mode int) (cols []string, rows [][]Value, err error) {
	if stmt.HasValue {
		return nil, nil, fmt.Errorf("%w: PRAGMA auto_vacuum=%s on the read side (the assignment is a write-path statement -- see execAutoVacuum)", errVDBEUnsupported, stmt.ValueText)
	}
	return []string{"auto_vacuum"}, [][]Value{{{Typ: Int, I: int64(mode)}}}, nil
}

// autoVacuumModeOf resolves the setter's payload as C does: "none", "full",
// "incremental" case-insensitively, else the leading decimal integer, with
// anything outside 0..2 meaning none ("1.0" and "01" are 1; "3", "on", "-1" are
// 0). ParsePragma has already stripped quotes and a leading "+".
func autoVacuumModeOf(text string) int {
	s := strings.TrimSpace(text)
	switch strings.ToLower(s) {
	case "none":
		return 0
	case "full":
		return 1
	case "incremental":
		return 2
	}
	// Everything else is its LEADING decimal integer, with anything outside
	// 0..2 meaning none. A leading "-" is not a digit, so a negative value
	// stops the scan before it starts and lands on none -- which is what "-1"
	// really reads back -- and so does a word with no digits at all.
	n := 0
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
		if n > 2 {
			return 0
		}
	}
	return n
}

// checkIntegrityCheckTarget validates integrity_check/quick_check's argument:
//
//	(no argument)  (3)  (0)          -> ok      a max-error COUNT
//	(real1) ('real1') (v1) (tmp1)    -> ok      a table or a view
//	(sqlite_master) (sqlite_schema)  -> ok      the catalogs
//	(nope) ('nope') ("nope") ([nope])-> no such table: nope
//	(real1b)                         -> no such table: real1b   an INDEX
//	('3')                            -> no such table: 3        QUOTED
//
// An index does not qualify, and a quoted numeral is a name, so
// stmt.ValueIsString is tested before the integer parse.
func (p *ReadOnlyPager) checkIntegrityCheckTarget(scope schemaScope, stmt *PragmaStmt) error {
	if !stmt.HasValue {
		return nil
	}
	if !stmt.ValueIsString {
		if _, err := strconv.ParseInt(stmt.ValueText, 10, 64); err == nil {
			return nil // a max-error count
		}
	}
	name := stmt.ValueText
	if isMainSchemaCatalogName(name) || isTempSchemaCatalogName(name) {
		return nil
	}
	rows, err := p.Schema()
	if err != nil {
		return err
	}
	if findSchemaRowIn(rows, scope, "table", name) != nil ||
		findSchemaRowIn(rows, scope, "view", name) != nil {
		return nil
	}
	return fmt.Errorf("engine: no such table: %s", name)
}

// integrityCheckErrorMax is SQLITE_INTEGRITY_CHECK_ERROR_MAX: the number of
// messages PRAGMA integrity_check/quick_check reports before it stops. The
// argument form overrides it, and pragma.c maps a non-positive one back to this
// default ("if( mxErr<=0 ) mxErr = SQLITE_INTEGRITY_CHECK_ERROR_MAX"). Verified
// against mattn/go-sqlite3 3.53.3 over 300 violating rows: the bare form and
// "quick_check" each answer 100, "(200)" answers 156 (i.e. everything, since
// there were fewer than 200), and "(0)" is not a cap at all.
const integrityCheckErrorMax = 100

// integrityCheckArgs splits PRAGMA integrity_check/quick_check's optional
// argument into a max-error COUNT and a single-OBJECT name -- the same split
// checkIntegrityCheckTarget has already validated, and the same split real
// SQLite's own pragma.c makes (pragma.c:1717-1727, read directly: zRight
// parses as an int32 first, and only on failure does it become pObjTab's
// lookup name via sqlite3LocateTable).
func integrityCheckArgs(stmt *PragmaStmt) (maxErr int, only string) {
	maxErr = integrityCheckErrorMax
	if !stmt.HasValue {
		return maxErr, ""
	}
	if !stmt.ValueIsString {
		if n, perr := strconv.ParseInt(stmt.ValueText, 10, 32); perr == nil {
			if n > 0 {
				maxErr = int(n)
			}
			// A non-positive count is not a cap of zero: pragma.c puts the
			// default back. Verified: "PRAGMA integrity_check(0)" reported all
			// six messages a seeded database held.
			return maxErr, ""
		}
	}
	return maxErr, stmt.ValueText
}

// integrityCheckViolations is the per-row half of integrity_check / quick_check
// (pragma.c:1822-2155): rows against column constraints, CHECKs and indexes.
// The findings and their wording live in integrity_rowcheck.go; this decides
// which tables are visited and spends the shared error budget. The structural
// half runs first, as in C (pragma.c:1783 before 1822).
//
// C visits tables in schema hash order, which is not reproducible; this visits
// in schema order with the same messages. Within a table the order matches C.
// Since table order decides which messages survive the cap, a capped answer
// spanning two or more violating tables is declined.
func (p *ReadOnlyPager) integrityCheckViolations(scope schemaScope, stmt *PragmaStmt, only string, budget int) ([][]Value, error) {
	maxErr := budget
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	// quick_check omits the index half and nothing else (pragma.c:2058's
	// "Omit the remaining tests for quick_check"); the column and CHECK
	// arms above it run for both spellings.
	isQuick := strings.EqualFold(stmt.Name, "quick_check")
	type badTable struct {
		name string
		msgs []string
	}
	var bad []badTable
	// countMsgs is pragma.c:1794-1820's block, which checks every table's
	// index entry counts before the per-row loop visits any table.
	var countMsgs []string
	tablesWithFindings := 0
	total := 0
	for i := range rows {
		tr := &rows[i]
		if tr.Type != "table" || !scope.accepts(tr.Temp) {
			continue
		}
		if only != "" && !equalFoldName(tr.Name, only) {
			continue
		}
		// A VIRTUAL table has no CHECK constraints and no CREATE TABLE text to
		// parse; C SQLite's own loop skips everything that is not an ordinary
		// table. An internal "sqlite_*" table has none either. An EMPTY sql
		// column is skipped before anything reads it: writable_schema can put
		// one there, and parseCreateTableColumnsAndAutoIndexes indexes token 0
		// unconditionally -- a panic is this project's hardest failure.
		if strings.TrimSpace(tr.SQL) == "" || isCreateVirtualTableSQL(tr.SQL) ||
			strings.HasPrefix(r33sFoldIdent(tr.Name), "sqlite_") {
			continue
		}
		// Qualify only when the OTHER catalog holds a table of this name, which
		// is the only case a bare name would resolve to the wrong one (an
		// unqualified lookup is temp-first). Always qualifying would be worse:
		// "main." does not resolve on every pager this can run on.
		other := scopeTemp
		if tr.Temp {
			other = scopeMain
		}
		qualify := findSchemaRowIn(rows, other, "table", tr.Name) != nil
		counts, msgs, cerr := p.integrityRowFindings(tr, rows, qualify, isQuick)
		if cerr != nil {
			return nil, cerr
		}
		if len(counts)+len(msgs) > 0 {
			tablesWithFindings++
		}
		if len(counts) > 0 {
			countMsgs = append(countMsgs, counts...)
			total += len(counts)
		}
		if len(msgs) > 0 {
			bad = append(bad, badTable{tr.Name, msgs})
			total += len(msgs)
		}
	}
	// The cap is where the unreproducible table order stops being invisible:
	// truncating a list drawn from TWO tables keeps whichever C SQLite's hash
	// order put first, and this engine's schema order need not agree. Measured:
	// "PRAGMA integrity_check(2)" over a database with violations in t1, t2, tt
	// and wr answers wr,t1 there and t1,t1 here. Within ONE table the order is
	// fully determined (row order, then pragma.c's own per-row sequence -- see
	// integrity_rowcheck.go), so only the multi-table truncation is declined.
	if total > maxErr && tablesWithFindings > 1 {
		return nil, fmt.Errorf("%w: PRAGMA %s(%s) truncating %d row-level violations across %d tables (which ones survive is decided by C SQLite's schema hash-table iteration order, which this engine does not reproduce -- see integrityCheckViolations)", errVDBEUnsupported, stmt.Name, stmt.ValueText, total, tablesWithFindings)
	}
	var out [][]Value
	for _, msg := range countMsgs {
		if len(out) >= maxErr {
			break
		}
		out = append(out, []Value{{Typ: Text, S: []byte(msg)}})
	}
	for _, bt := range bad {
		for _, msg := range bt.msgs {
			if len(out) >= maxErr {
				break
			}
			out = append(out, []Value{{Typ: Text, S: []byte(msg)}})
		}
	}
	return out, nil
}

// PragmaValueSpellingError is pragmaValueSpellingSupported for the driver, which
// answers many pragmas itself and never reaches execPragma's check. Returns nil
// for text that is not a PRAGMA or has no value.
func PragmaValueSpellingError(sqlText string) error {
	stmt, err := ParsePragma(sqlText)
	if err != nil || stmt == nil {
		return nil
	}
	return pragmaValueSpellingSupported(stmt)
}
