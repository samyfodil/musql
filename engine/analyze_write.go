// This file implements ANALYZE: it creates and fills sqlite_stat1 exactly as C
// does (computeStat1Rows/storeStat1), and the planner reads it back
// (where_plan_stat1.go). sqlite_stat1 is an ordinary table, created verbatim as
// "CREATE TABLE sqlite_stat1(tbl,idx,stat)", so hand-seeded stats ("DELETE
// FROM sqlite_stat1; INSERT INTO sqlite_stat1 VALUES(...)") work as in C.
//
// sqlite_stat4 is not implemented (its sampling is not reproducible), and
// nothing creates it.
//
// Every accepted ANALYZE that targets main creates sqlite_stat1 first if
// absent, even forms that then write no row:
//
//	ANALYZE                 -- on a database with no tables
//	ANALYZE main            -- likewise
//	ANALYZE sqlite_master   -- also sqlite_schema / main.sqlite_master /
//	                           main.sqlite_schema
//	ANALYZE <view>          -- a view has nothing of its own to analyze
//
// "ANALYZE sqlite_master" does nothing else: it neither writes nor erases
// rows, so the schema cookie moves only on the create.
//
// TEMP-targeting forms ("ANALYZE temp", "ANALYZE temp.<x>", "ANALYZE
// sqlite_temp_master", a temp object) are a no-op here, where C creates and
// fills a temp sqlite_stat1. A main-schema ANALYZE records only main objects,
// as C does (computeStat1Rows' isTemp skip).
//
// Argument acceptance must match C's exactly: bare ANALYZE; "ANALYZE main" /
// "ANALYZE temp" (always valid); "ANALYZE <table/index/view>" for one that
// exists, including the four schema-catalog names; and "ANALYZE schema.name".
// "ANALYZE nosuchtable" is "no such table: nosuchtable" and "ANALYZE
// nosuchschema.x" is "unknown database nosuchschema".
//
// "ANALYZE schema.name" resolves the name in that schema:
//
//	main.sqlite_master  main.sqlite_schema           accepted (main's catalog)
//	temp.sqlite_master  temp.sqlite_schema           accepted (temp's own)
//	temp.sqlite_temp_master temp.sqlite_temp_schema  accepted
//	main.sqlite_temp_master main.sqlite_temp_schema  REJECTED, "no such table"
//	main.t1 main.ix main.v1  temp.tt                 accepted
//	temp.t1 temp.ix temp.v1  main.tt                 REJECTED, "no such table"
//
// Accepting what C rejects would be a wrong answer; rejecting what C accepts
// is only a decline. So anything not confidently recognized is rejected.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// isAnalyzeSchemaName reports whether name (case-insensitive) is one of the
// two schema names ANALYZE accepts bare, with no table/index/view lookup at
// all -- "main" and "temp" are always valid schema names in C SQLite,
// regardless of whether this session has ever ATTACHed anything or created a
// TEMP object (verified directly: "ANALYZE temp" succeeds against a
// freshly-opened database with no temp objects whatsoever).
func isAnalyzeSchemaName(name string) bool {
	return equalFoldName(name, "main") || equalFoldName(name, "temp")
}

// analyzeTargetScope resolves an ANALYZE argument under qualifier scope
// (scopeAny, scopeMain or scopeTemp) to the catalog its target lives in, or
// ok=false when nothing of that name resolves there -- exactly what C
// accepts. A schema-catalog name is always valid and names its own database:
// "temp.sqlite_master" is the temp catalog, while "sqlite_temp_master" names
// temp specifically and so resolves nowhere under "main.".
func (db *DB) analyzeTargetScope(scope schemaScope, name string) (schemaScope, bool) {
	if isTempSchemaCatalogName(name) {
		if scope == scopeMain {
			return scopeAny, false // "main.sqlite_temp_master": no such table
		}
		return scopeTemp, true
	}
	if isMainSchemaCatalogName(name) {
		if scope == scopeTemp {
			return scopeTemp, true // "temp.sqlite_master": the TEMP catalog
		}
		return scopeMain, true
	}
	if t := db.findTableMetaIn(scope, name); t != nil {
		return createScope(t.isTemp), true
	}
	if ix := db.findIndexMetaIn(scope, name); ix != nil {
		return createScope(ix.isTemp), true
	}
	if v := db.findViewMetaIn(scope, name); v != nil {
		return createScope(v.isTemp), true
	}
	if vt := db.findVtabMetaIn(scope, name); vt != nil {
		// sqlite3Analyze resolves the name with sqlite3LocateTable, which
		// finds a VIRTUAL table too (analyze.c:1440); analyzeOneTable then
		// walks its index list, which is empty, so the statement succeeds and
		// writes no sqlite_stat1 row for it. Verified against 3.53.3:
		// "ANALYZE <fts4 table>" is OK and stat1 gains rows only for the
		// SHADOW tables a bare ANALYZE would have covered anyway.
		return createScope(vt.isTemp), true
	}
	return scopeAny, false
}

// analyzeObject performs "ANALYZE <name>" for a target already resolved to the
// catalog it lives in (analyzeTargetScope).
func (db *DB) analyzeObject(target schemaScope, name string) error {
	if target == scopeTemp {
		// A temp sqlite_stat1 is not implemented; see the file comment.
		return nil
	}
	if isMainSchemaCatalogName(name) {
		// "ANALYZE sqlite_master" recomputes nothing and erases nothing -- it
		// only creates sqlite_stat1 if absent (verified; see the doc comment)
		// -- but it does LOAD it: analyzeTable ends in loadAnalysis for a
		// table it skips (analyze.c:1442), which is why the test suite's own
		// idiom for "use the rows I just wrote" is exactly this statement.
		if err := db.ensureStat1Table(); err != nil {
			return err
		}
		db.loadAnalysis()
		return nil
	}
	return db.analyzeScoped(name)
}

// execAnalyze parses and "executes" (validates, then does nothing -- see
// this file's package doc comment) a single ANALYZE statement.
//
// The sqlite_stat1 rows it writes go in through the ordinary InsertArgs path,
// which moves last_insert_rowid() -- and C SQLite's ANALYZE does not
// (verified: after "INSERT INTO t VALUES(8)" it still reports 8, where this
// engine reported the last stat row's rowid). ANALYZE is the engine writing
// SQLite's own internal table, not the connection inserting a row, so the
// counter is put back. changes()/total_changes() need no such guard: an
// ANALYZE program is not change-counting (Program.CountsChanges), and nothing
// on this path publishes them.
func (db *DB) execAnalyze(sqlText string) error {
	savedLastInsertRowid := db.lastInsertRowid
	defer func() { db.lastInsertRowid = savedLastInsertRowid }()
	toks, err := lex(sqlText)
	if err != nil {
		return err
	}
	p := newParser(sqlText, toks)
	if !p.consumeKeyword("ANALYZE") {
		return fmt.Errorf("engine: ANALYZE: expected ANALYZE, got %q", p.tokenDesc(p.peek()))
	}

	if p.peek().kind == tkIdent {
		first := p.next().text
		if p.peekIsPunct(".") {
			p.next()
			t := p.peek()
			if t.kind != tkIdent {
				return fmt.Errorf("engine: ANALYZE: expected name after %q., got %q", first, p.tokenDesc(t))
			}
			p.next()
			second := t.text
			if err := expectDropTrailer(p, "engine: ANALYZE"); err != nil {
				return err
			}
			scope, ok := scopeOfQualifier(first)
			if !ok {
				// Not "main"/"temp": it may be an ATTACHed database, whose
				// objects C SQLite's ANALYZE reaches through
				// sqlite3TwoPartName exactly like main's (analyze.c:1486).
				if db.attachedNamed(first) != nil {
					return db.analyzeAttached(first, second, scopeMain)
				}
				return fmt.Errorf("engine: unknown database %s", first)
			}
			target, ok := db.analyzeTargetScope(scope, second)
			if !ok {
				return fmt.Errorf("engine: no such table: %s.%s", first, second)
			}
			return db.analyzeObject(target, second)
		}
		if err := expectDropTrailer(p, "engine: ANALYZE"); err != nil {
			return err
		}
		// A bare "main"/"temp" names a whole SCHEMA, not an object -- and does
		// so even if a table of that name exists, exactly as before.
		if equalFoldName(first, "main") {
			// "ANALYZE main" analyzes the main schema ALONE -- unlike a bare
			// ANALYZE, which covers every attachment too (analyze.c:1475-1483
			// are two different arms).
			return db.analyzeMainSchema()
		}
		if isAnalyzeSchemaName(first) {
			// "ANALYZE temp" populates a TEMP sqlite_stat1 in C SQLite; not
			// implemented here (see the file comment).
			return nil
		}
		// A bare ATTACHed database NAME is form 2 as well, and it wins over a
		// same-named table for the same reason "main" does: sqlite3Analyze
		// tests "sqlite3FindDb(db, pName1)>=0" BEFORE it ever looks for an
		// object (analyze.c:1481).
		if db.attachedNamed(first) != nil {
			return db.analyzeAttached(first, "", scopeAny)
		}
		target, ok := db.analyzeTargetScope(scopeAny, first)
		if !ok {
			return fmt.Errorf("engine: no such table: %s", first)
		}
		return db.analyzeObject(target, first)
	}

	if err := expectDropTrailer(p, "engine: ANALYZE"); err != nil {
		return err
	}
	// Bare "ANALYZE": every database except TEMP (analyze.c:1475-1480).
	return db.analyzeEveryDatabase()
}

// dropStat1Rows removes every sqlite_stat1 row whose col ("tbl" or "idx")
// equals name -- what DROP INDEX and DROP TABLE must do, since C SQLite
// deletes a dropped object's stats along with it (verified directly against
// analyze.test's own DROP sequence). A no-op when sqlite_stat1 does not
// exist, which is the common case.
func (db *DB) dropStat1Rows(col, name string) error {
	if db.findTableMeta("sqlite_stat1") == nil {
		return nil
	}
	c := stat1ColumnIndex(col)
	if c < 0 {
		return fmt.Errorf("engine: dropStat1Rows: unknown sqlite_stat1 column %q", col)
	}
	return db.clearStat1Rows(c, name)
}

// analyzeScoped implements "ANALYZE <table>" and "ANALYZE <index>": it
// recomputes and replaces only that object's sqlite_stat1 rows
// (analyze.test). A view target writes no row but still creates sqlite_stat1
// (ensureStat1Table). Only main objects reach here, so lookups are scopeMain
// and "ANALYZE ix" cannot pick a same-named temp index.
func (db *DB) analyzeScoped(target string) error {
	all := db.computeStat1Rows()
	if idx := db.findIndexMetaIn(scopeMain, target); idx != nil {
		var keep []stat1Row
		for _, r := range all {
			if r.idx.Typ == Text && equalFoldName(string(r.idx.S), target) {
				keep = append(keep, r)
			}
		}
		return db.storeStat1Scoped(keep, stat1ColIdx, target)
	}
	if db.findTableMetaIn(scopeMain, target) == nil {
		// A view: nothing of its own to analyze, but sqlite_stat1 is still
		// created (see this function's doc comment), and loaded as for
		// "ANALYZE sqlite_master".
		if err := db.ensureStat1Table(); err != nil {
			return err
		}
		db.loadAnalysis()
		return nil
	}
	var keep []stat1Row
	for _, r := range all {
		if equalFoldName(r.tbl, target) {
			keep = append(keep, r)
		}
	}
	return db.storeStat1Scoped(keep, stat1ColTbl, target)
}

// stat1Row is one computed sqlite_stat1 row: the table name, the index name
// (a Text Value, or a Null Value for the table-level "no full index" row),
// and the space-separated stat string exactly as C SQLite's ANALYZE writes
// it (see computeStat1Rows).
type stat1Row struct {
	tbl  string
	idx  Value
	stat string
}

// analyzeMainSchema recomputes the whole main schema's sqlite_stat1 content
// and stores it -- the effect of a bare "ANALYZE" (or "ANALYZE main") in real
// SQLite: create sqlite_stat1 if it does not exist, clear it, then insert one
// row per index (and one idx=NULL row per table with no full index). See
// storeStat1.
func (db *DB) analyzeMainSchema() error {
	return db.storeStat1(db.computeStat1Rows())
}

// analyzeEveryDatabase is a bare "ANALYZE": every database except TEMP
// (sqlite3Analyze, analyze.c:1475-1480):
//
//	if( pName1==0 ){
//	  /* Form 1:  Analyze everything */
//	  for(i=0; i<db->nDb; i++){
//	    if( i==1 ) continue;  /* Do not analyze the TEMP database */
//	    analyzeDatabase(pParse, i);
//
// Each attachment is analyzed through its own write session, the routing
// every cross-database write uses, so it inherits that lock ladder: an
// attachment a sibling alias holds SHARED under locking_mode=exclusive
// declines with ErrBusy, C's "database is locked". db.attached never contains
// TEMP, so C's "i==1" skip needs no counterpart.
func (db *DB) analyzeEveryDatabase() error {
	// Every attachment's write lock is taken before anything is written, so a
	// refused ANALYZE leaves nothing behind -- C's net effect, since it rolls
	// the whole statement back (main's stats included) when a lock fails.
	//
	// Locks a refused ANALYZE did get are kept, as C's are: a statement
	// rollback restores pages, not locks. attachedWriteSessionFor records each
	// (attachedDB.reservedInTxn) so the following COMMIT sees them
	// (fts5misc.test 14.0).
	type target struct {
		name, path string
		w          *DB
	}
	var targets []target
	for i := 0; i < len(db.attached); i++ {
		ad := db.attached[i]
		// Two aliases of ONE file written by ONE statement conflict even in
		// autocommit, where the statement is its own implicit transaction: the
		// second handle's RESERVED request meets the first's. Inside an explicit
		// transaction attachedWriteSessionFor reaches the same answer from the
		// first alias's recorded lock; this covers the implicit one.
		for _, t := range targets {
			if samePath(t.path, ad.path) {
				return fmt.Errorf("%w: ANALYZE writes into ATTACHed database %s, whose file is also attached as %s, which this statement is already writing (C SQLite: a second handle's RESERVED request on one file conflicts with the first's)", ErrBusy, ad.name, t.name)
			}
		}
		w, err := db.attachedWriteSessionFor(ad.name, true)
		if err != nil {
			return err
		}
		if w == nil {
			return fmt.Errorf("engine: unknown database %s", ad.name)
		}
		targets = append(targets, target{name: ad.name, path: ad.path, w: w})
	}
	if err := db.analyzeMainSchema(); err != nil {
		return err
	}
	for _, t := range targets {
		if err := t.w.analyzeMainSchema(); err != nil {
			return err
		}
		if err := db.analyzeAttachedDone(t.name); err != nil {
			return err
		}
	}
	return nil
}

// analyzeAttached analyzes ATTACHed database q: its whole schema when name is
// "" ("ANALYZE aux1", analyze.c:1481-1483), or the one object name resolves to
// there ("ANALYZE aux1.t", analyze.c:1484-1498). scope is the qualifier the
// object was written under, scopeAny for the whole-schema form.
//
// An attachment's own session sees its file as MAIN, so the object lookup and
// the stat1 write are the ordinary main-schema ones run against that session.
func (db *DB) analyzeAttached(q, name string, scope schemaScope) error {
	w, err := db.attachedWriteSessionFor(q, true)
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("engine: unknown database %s", q)
	}
	if name == "" {
		if err := w.analyzeMainSchema(); err != nil {
			return err
		}
		return db.analyzeAttachedDone(q)
	}
	target, ok := w.analyzeTargetScope(scope, name)
	if !ok {
		return fmt.Errorf("engine: no such table: %s.%s", q, name)
	}
	if err := w.analyzeObject(target, name); err != nil {
		return err
	}
	return db.analyzeAttachedDone(q)
}

// analyzeAttachedDone is the tail every routed write to an attachment runs
// (routeAttachedStatement, attach_write.go): mark the attachment as
// having genuinely MUTATED -- which is what the alias lock ladder keys on, not
// the mere existence of a write session -- and re-point the read pagers of
// every same-path attachment at the new content, so the very next
// "SELECT ... FROM aux1.sqlite_stat1" sees the rows ANALYZE just wrote.
// Without the refresh the write lands in the file and the read answers "no
// such table: sqlite_stat1" from a snapshot taken before it.
func (db *DB) analyzeAttachedDone(q string) error {
	if ad := db.attachedNamed(q); ad != nil {
		ad.wroteData = true
	}
	return db.refreshAttachedWriteReaders()
}

// statColValue returns the value of table t's column colIdx for the row keyed
// by rowid (whose stored payload is row). For an ordinary rowid table's own
// INTEGER PRIMARY KEY column, the stored payload holds NULL and the true value
// is the rowid itself (tableMeta.rows' doc comment), so it is substituted here
// -- exactly what an index over that column would key on.
func statColValue(t *tableMeta, rowid uint64, row []Value, colIdx int) Value {
	if colIdx == t.ipkIndex {
		return Value{Typ: Int, I: int64(rowid)}
	}
	if colIdx >= 0 && colIdx < len(row) {
		return row[colIdx]
	}
	return Value{Typ: Null}
}

// computeStat1Rows computes the sqlite_stat1 rows C's ANALYZE would write for
// the current main-schema content:
//
//   - sqlite_-prefixed tables are skipped.
//   - An empty table contributes nothing, not even an idx=NULL row.
//   - Each index on a non-empty table gives (tbl, idx, stat): the row count,
//     then per key prefix k the average rows sharing the first k values,
//     ceil(nRow / nDistinct_k), distinct under the index's collations
//     (compareIndexRec). As in statGet, a result of 2 is pulled back to 1
//     when nRow*10 <= nDistinct*11 (analyze.c:873, "if I is between 1.0 and
//     1.1 ... then do not round up").
//   - A table with no non-partial index gets one idx=NULL row holding the row
//     count (C's needTableCnt).
//
// An expression or partial index is analyzed through its own keys and
// predicate (indexMeta.keys/where, as indexEntryOf keys entries). Its stat
// counts only rows its WHERE admits (ordinary truthiness), and a partial index
// does not clear needTableCnt while a non-partial expression index does:
//
//	CREATE INDEX t1a ON t1(a) WHERE a IS NOT NULL, 3 rows, one NULL
//	                                     -> t1a "2 1", plus the idx=NULL row
//	CREATE INDEX i ON t1(a+b, b), rows (1,1),(2,2),(1,1)
//	                                     -> i "3 2 2", and NO idx=NULL row
//	CREATE INDEX i ON t1(a) WHERE a>100, no row qualifies
//	                                     -> i "0 0", plus the idx=NULL row
//
// A WITHOUT ROWID table's PRIMARY KEY is an index (indexMeta.isTablePK), so it
// yields a stat row and clears needTableCnt. It is recorded under the table's
// own name, since that PK has no sqlite_schema row.

// tableHasPartialIndex reports whether t has at least one PARTIAL index (one
// whose stored CREATE INDEX text carries a WHERE) -- the only kind ANALYZE
// records for an empty table. An expression index with no WHERE does not count.
func (db *DB) tableHasPartialIndex(t *tableMeta) bool {
	for _, idx := range db.indexes {
		if idx.isTemp != t.isTemp || !equalFoldName(idx.table, t.name) {
			continue // see computeStat1Rows' matching guard
		}
		if idx.exprOrPartial && idx.where != nil {
			return true
		}
	}
	return false
}

func (db *DB) computeStat1Rows() []stat1Row {
	var out []stat1Row
	for _, t := range db.analyzeTableOrder() {
		if strings.HasPrefix(r33sFoldIdent(t.name), "sqlite_") {
			continue
		}
		// ANALYZE needs every table's rows. Best-effort: some callers
		// (pragma_optimize_track.go) thread no error, and a load can only
		// fail when another writer committed mid-session -- a session
		// already doomed at its own commit. The error is still recorded in
		// db.pendingLoadErr so storeStat1's callers report it.
		if err := db.ensureTableLoaded(t); err != nil {
			if db.pendingLoadErr == nil {
				db.pendingLoadErr = err
			}
			continue
		}
		// A TEMP table belongs to the temp DATABASE, whose stats C SQLite
		// keeps in its own temp.sqlite_stat1 -- verified directly: a bare
		// ANALYZE with a temp table present writes only the MAIN table's rows
		// into main.sqlite_stat1 and creates no temp.sqlite_stat1 at all. This
		// engine's ANALYZE writes main's sqlite_stat1 only (temp's own is not
		// built), so folding a temp table's stats in here reported a row real
		// SQLite never puts in main -- a wrong answer.
		if t.isTemp {
			continue
		}
		// An EMPTY table is skipped -- except that a PARTIAL index still gets
		// its row. That is SQLite's own rule (analyze.c: "always record an
		// entry for a partial index, even if it is empty"), verified
		// directly: with no rows at all, two partial indexes report "0 0"
		// each and there is no idx=NULL row, while a plain or UNIQUE index
		// reports nothing. index6/index7.test hit exactly this, because the
		// virtual table they populate from is out of scope here and leaves
		// the table empty on BOTH sides.
		nRow := t.rows.len()
		if nRow == 0 && !db.tableHasPartialIndex(t) {
			continue
		}
		rowids := make([]uint64, 0, nRow)
		for rid := range t.rows.all() {
			rowids = append(rowids, rid)
		}
		sort.Slice(rowids, func(i, j int) bool { return rowids[i] < rowids[j] })

		needTableCnt := true
		// analyzeIndexOrder matches by table NAME and catalog both: a TEMP
		// index's table is always TEMP (index_write.go rejects a TEMP index
		// on a non-TEMP table), and a main table may share its name with a
		// temp one. Same guard as drop_write.go's.
		for _, idx := range db.analyzeIndexOrder(t) {
			if nRow == 0 && !idx.exprOrPartial {
				// See the empty-table note above: only a partial index is
				// recorded for an empty table.
				continue
			}
			var (
				recs  [][]Value
				colls []string
				nKey  int
			)
			if idx.exprOrPartial {
				keys, where := idx.keys, idx.where
				nKey = len(keys)
				if nKey == 0 {
					continue
				}
				colls = make([]string, nKey)
				for c, k := range keys {
					colls[c] = k.collation
				}
				// The SAME compiled programs materializing, UNIQUE-validating
				// and conflict-checking this index already run -- see
				// compileIndexExprs (index_write.go), whose doc comment makes
				// the point this call site is the fourth instance of: every
				// consumer asking "is this row in this index, and with what
				// key?" must answer it with ONE evaluator, or they are two
				// implementations of one semantics. ANALYZE is exactly that
				// question, asked to count distinct key prefixes.
				whereProg, keyProgs := compileIndexExprs(t, idx)
				for _, rid := range rowids {
					row := t.rows.row(rid)
					ctx := rowEvalCtx(t, rid, row, nil, nil, nil)
					if where != nil {
						v, verr := whereProg.eval(ctx)
						if verr != nil {
							recs = nil
							break
						}
						if !isTruthy(v) {
							continue
						}
					}
					rec := make([]Value, nKey)
					bad := false
					for c, k := range keys {
						if k.colIdx >= 0 {
							rec[c] = statColValue(t, rid, row, k.colIdx)
							continue
						}
						v, verr := keyProgs[c].eval(ctx)
						if verr != nil {
							bad = true
							break
						}
						rec[c] = v
					}
					if bad {
						recs = nil
						break
					}
					recs = append(recs, rec)
				}
				if recs == nil && where == nil {
					continue // an evaluation failed: leave this index unanalyzed
				}
				if where == nil {
					// A non-partial (expression) index covers every row, so it
					// supplies the table count -- exactly like a plain index.
					needTableCnt = false
				}
			} else {
				nKey = len(idx.colIdx)
				if nKey == 0 {
					continue
				}
				needTableCnt = false
				colls = idx.colCollation
				recs = make([][]Value, nRow)
				for i, rid := range rowids {
					row := t.rows.row(rid)
					rec := make([]Value, nKey)
					for c := 0; c < nKey; c++ {
						rec[c] = statColValue(t, rid, row, idx.colIdx[c])
					}
					recs[i] = rec
				}
			}
			// Sorted ASCENDING regardless of any key column's DESC flag: this
			// only ever groups equal prefixes together so the loop below can
			// COUNT DISTINCT ones, and reversing a column changes neither
			// which records are equal nor how many distinct prefixes there
			// are. (The b-tree's own cell order does honour DESC -- see
			// indexMeta.colDesc.)
			sort.SliceStable(recs, func(i, j int) bool {
				return compareIndexRec(recs[i], recs[j], colls, nil, db.encoding()) < 0
			})

			// The scan itself, which "PRAGMA analysis_limit" can cut short:
			// see analysisLimitWalk (pragma_analysis_limit.go) for the
			// skip-ahead rule statPush encodes. nRow is what the scan
			// VISITED; the leading figure is the index's true size whenever
			// the scan skipped at all (statGet, analyze.c:869-870).
			enc := db.encoding()
			eqPrefix := func(a, b []Value, n int) bool {
				return compareIndexRec(a[:n], b[:n], colls[:n], nil, enc) == 0
			}
			nRowScanned, skipAhead, anDLt := analysisLimitWalk(recs, nKey, db.analysisLimit(), eqPrefix)
			nIdxRow := nRowScanned
			reported := nRowScanned
			if skipAhead > 0 {
				reported = len(recs)
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%d", reported)
			for k := 1; k <= nKey; k++ {
				if nIdxRow == 0 {
					// An index no row qualifies for reports 0 per key column,
					// not the ceil formula (verified: stat "0 0").
					sb.WriteString(" 0")
					continue
				}
				distinct := anDLt[k-1] + 1
				iVal := (nIdxRow + distinct - 1) / distinct
				// analyze.c:873, statGet's own comment just above it ("if I is
				// between 1.0 and 1.1 ... then do not round up but instead
				// keep the I value at 1.0"): a plain ceil(nRow/distinct) that
				// lands on EXACTLY 2 only because of the ceiling (i.e. the
				// true ratio is barely over 1.0) is pulled back down to 1.
				// Verified live against the built oracle (sqlite3 3.53.3,
				// SOURCE_ID d4c0e51e...82c62): 1001 rows over 1000 distinct
				// values (busy.test's own 1000-row growth over a table that
				// started with one row already at x=1) reports "1001 1", not
				// the "1001 2" plain ceil(1001/1000) gives.
				if iVal == 2 && nIdxRow*10 <= distinct*11 {
					iVal = 1
				}
				fmt.Fprintf(&sb, " %d", iVal)
			}
			// A WITHOUT ROWID table's PRIMARY KEY has no sqlite_schema row of
			// its own (the table's b-tree IS the index), and C SQLite
			// records its stat under the TABLE's name -- verified directly:
			// "CREATE TABLE t1(a,b,c PRIMARY KEY) WITHOUT rowid" with two
			// rows yields sqlite_stat1 row (t1, "t1", "2 1").
			statIdxName := idx.name
			if idx.isTablePK {
				statIdxName = t.name
			}
			out = append(out, stat1Row{tbl: t.name, idx: Value{Typ: Text, S: []byte(statIdxName)}, stat: sb.String()})
		}
		if needTableCnt && nRow > 0 {
			out = append(out, stat1Row{tbl: t.name, idx: Value{Typ: Null}, stat: fmt.Sprintf("%d", nRow)})
		}
	}
	return out
}

// ensureStat1Table creates sqlite_stat1 if it does not already exist, verbatim
// "CREATE TABLE sqlite_stat1(tbl,idx,stat)" so it appears in sqlite_master
// identically to C SQLite. This is the step EVERY accepted main-schema
// ANALYZE performs first, whether or not it goes on to write a row -- see this
// file's doc comment for the forms that do nothing else.
func (db *DB) ensureStat1Table() error {
	if db.findTableMeta("sqlite_stat1") != nil {
		return nil
	}
	// "sqlite_" is a reserved prefix for USER-created objects; this one is
	// the engine creating SQLite's own internal stat table, exactly as
	// C SQLite does with db->init.busy set. See DB.internalSchemaInit.
	db.internalSchemaInit = true
	err := db.CreateTable("CREATE TABLE sqlite_stat1(tbl,idx,stat)")
	db.internalSchemaInit = false
	return err
}

// storeStat1 writes rows into sqlite_stat1 as C SQLite's ANALYZE does:
// create the table (verbatim "CREATE TABLE sqlite_stat1(tbl,idx,stat)", so it
// appears in sqlite_master identically to C SQLite) if absent, DELETE all
// existing rows, then INSERT the freshly computed set. Once created it is a
// completely ordinary writable/queryable table -- the tests that hand-seed
// stats ("DELETE FROM sqlite_stat1; INSERT INTO sqlite_stat1 VALUES(...)")
// then drive it through the normal write path.
func (db *DB) storeStat1(rows []stat1Row) error {
	return db.storeStat1Scoped(rows, stat1ColAll, "")
}

// storeStat1Scoped is storeStat1 with a narrower clear step: delCol/delName
// select exactly the rows the caller is about to replace. A whole-schema
// ANALYZE clears everything; "ANALYZE <table>" clears that table's rows and
// "ANALYZE <index>" only that index's, leaving every other table's stats
// intact -- verified directly against C SQLite (analyze.test).
func (db *DB) storeStat1Scoped(rows []stat1Row, delCol int, delName string) error {
	if err := db.ensureStat1Table(); err != nil {
		return err
	}
	if err := db.clearStat1Rows(delCol, delName); err != nil {
		return err
	}
	t := db.findTableMeta("sqlite_stat1")
	if t == nil {
		return fmt.Errorf("engine: storeStat1Scoped: sqlite_stat1 missing after ensureStat1Table")
	}
	for _, r := range rows {
		rowid, rerr := nextRowidForTable(t)
		if rerr != nil {
			return rerr
		}
		t.putRow(rowid, []Value{
			{Typ: Text, S: []byte(r.tbl)},
			r.idx,
			{Typ: Text, S: []byte(r.stat)},
		})
	}
	// Every ANALYZE form funnels through here (analyzeMainSchema/
	// analyzeScoped, and execOptimize's own re-ANALYZE, pragma.go), so this
	// is the one choke point that must refresh *DB.stat1Baseline -- mirroring
	// analyze.c:1384's loadAnalysis, unconditionally emitted at the end of
	// every real ANALYZE. See refreshStat1Baseline's doc comment
	// (pragma_optimize_track.go).
	db.refreshStat1Baseline()
	// OP_LoadAnalysis (analyze.c:1387): the ANALYZE's own rows are what plans
	// use from here on.
	db.loadAnalysis()
	return nil
}


// sqlite_stat1's column positions, and the sentinel for "every row".
// "CREATE TABLE sqlite_stat1(tbl,idx,stat)" is verbatim what C SQLite
// creates (analyze.c's zSchema), so these are fixed by the format.
const (
	stat1ColTbl = 0
	stat1ColIdx = 1
	stat1ColAll = -1
)

// stat1ColumnIndex maps a sqlite_stat1 column NAME to its position, or -2 for
// one this file does not scope by. It exists only for dropStat1Rows' two
// callers, which pass "tbl"/"idx" as strings.
func stat1ColumnIndex(col string) int {
	switch col {
	case "tbl":
		return stat1ColTbl
	case "idx":
		return stat1ColIdx
	}
	return -2
}

// clearStat1Rows removes every sqlite_stat1 row whose column col equals name;
// stat1ColAll removes all of them.
//
// It writes the row store directly (putRow/dropRow), as storeStat1Scoped does
// for inserts, rather than running DELETE/INSERT: execAnalyze runs inside an
// opcode body, so ExecArgs here would nest a VM in the running VM. It also
// leaves last_insert_rowid() alone, as C's ANALYZE does. sqlite_stat1 has no
// indexes, triggers, FKs or CHECKs, so a compiled write would add nothing.
func (db *DB) clearStat1Rows(col int, name string) error {
	t := db.findTableMeta("sqlite_stat1")
	if t == nil {
		return nil
	}
	// The row store is loaded LAZILY, so a table reopened from disk holds no
	// rows in memory until asked. Iterating t.rows without this would silently
	// clear nothing.
	if err := db.ensureTableLoaded(t); err != nil {
		return err
	}
	var doomed []uint64
	for rowid, vals := range t.rows.all() {
		if col == stat1ColAll {
			doomed = append(doomed, rowid)
			continue
		}
		// "WHERE idx = ?" against a NULL idx (the table-level row) is NULL, not
		// true, so a NULL never matches -- which requiring Text preserves.
		if col < len(vals) && vals[col].Typ == Text && string(vals[col].S) == name {
			doomed = append(doomed, rowid)
		}
	}
	for _, rowid := range doomed {
		t.dropRow(rowid)
	}
	return nil
}
