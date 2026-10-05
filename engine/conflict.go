// This file implements the ON CONFLICT machinery shared by INSERT and UPDATE:
// the five resolution algorithms (lang_conflict.html), per-row conflict
// detection (findRowConflicts, over the UNIQUE index metadata), and UPSERT
// target matching.
//
//   - IGNORE skips a NOT NULL, CHECK, UNIQUE or rowid violation on that row and
//     continues.
//   - REPLACE deletes every existing row the candidate conflicts with (across
//     every UNIQUE index and/or the rowid) before storing it. A NOT NULL or
//     CHECK violation under REPLACE behaves like ABORT (a NOT NULL column with
//     a DEFAULT declines instead).
//   - ABORT (the default) undoes the current statement's rows and errors;
//     earlier statements in the transaction stand.
//   - FAIL errors but keeps the rows the statement already applied.
//   - ROLLBACK errors and undoes the whole explicit transaction, ending it (a
//     later COMMIT is "no transaction is active"); without one it is ABORT.
package engine

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// conflictAction is which of C SQLite's five ON CONFLICT resolution
// algorithms an INSERT/UPDATE statement's OR-clause names (or, with none
// given, the implicit default: conflictAbort).
type conflictAction int

const (
	conflictAbort conflictAction = iota
	conflictIgnore
	conflictReplace
	conflictFail
	conflictRollback
)

// parseConflictAction parses the resolution keyword following "INSERT OR"/
// "UPDATE OR": IGNORE, REPLACE, ABORT, FAIL, or ROLLBACK.
func parseConflictAction(p *parser) (conflictAction, error) {
	switch {
	case p.consumeKeyword("IGNORE"):
		return conflictIgnore, nil
	case p.consumeKeyword("REPLACE"):
		return conflictReplace, nil
	case p.consumeKeyword("ABORT"):
		return conflictAbort, nil
	case p.consumeKeyword("FAIL"):
		return conflictFail, nil
	case p.consumeKeyword("ROLLBACK"):
		return conflictRollback, nil
	default:
		return 0, fmt.Errorf("engine: expected IGNORE/REPLACE/ABORT/FAIL/ROLLBACK after OR, got %q", p.tokenDesc(p.peek()))
	}
}

// conflictHit is one row, currently stored in a table, that conflicts with a
// candidate row about to be inserted or updated into it: either the SAME
// rowid (target == []string{"rowid"} -- an INTEGER PRIMARY KEY/rowid
// collision) or agreement, on every indexed column, with some UNIQUE index
// registered against the table (target == that index's own column names,
// exactly as written in its CREATE TABLE/CREATE INDEX text). target is what
// upsertMatchingHitsByCols/validateUpsertTarget compare an ON CONFLICT(cols) list
// against.
type conflictHit struct {
	rowid  uint64
	target []string

	// idxName is set when the violated index has an EXPRESSION key, which has
	// no column to name: C SQLite words that one "UNIQUE constraint failed:
	// index 't5x'" -- quoted, and with no table. A partial index over plain
	// COLUMNS keeps the column form in target, so the split is on the key kind
	// and not on partial-ness (both verified against 3.53.3).
	idxName string
}

// targetKey renders cols as a case-insensitive, order-independent set key --
// C SQLite's own ON CONFLICT target resolution matches an index's column
// SET, not its exact written order (verified directly: CREATE TABLE
// t(a,b,UNIQUE(a,b)) rejects a conflicting row under "ON CONFLICT(b,a)" no
// differently than "ON CONFLICT(a,b)").
func targetKey(cols []string) string {
	lc := make([]string, len(cols))
	for i, c := range cols {
		lc[i] = r33sFoldIdent(c)
	}
	sort.Strings(lc)
	return strings.Join(lc, ",")
}

// hitTargetKey is targetKey for one conflictHit, resolving the rowid-
// collision sentinel target ({"rowid"}) to the table's own INTEGER PRIMARY
// KEY column name when it has one (verified directly: "ON CONFLICT(a) ..."
// against a table with "a INTEGER PRIMARY KEY" matches a rowid collision,
// not just a same-named ordinary UNIQUE column) or the literal "rowid" name
// for a table with no IPK column at all.
func hitTargetKey(tbl *tableMeta, hit conflictHit) string {
	if len(hit.target) == 1 && hit.target[0] == "rowid" {
		if tbl.ipkIndex >= 0 {
			return targetKey([]string{tbl.cols[tbl.ipkIndex].Name})
		}
		return targetKey([]string{"rowid"})
	}
	return targetKey(hit.target)
}

// findRowConflicts reports every stored row of tbl that conflicts with a
// candidate row: candidateRowid is the rowid it would occupy (already decided
// by the caller) and candidateFull its affinity-coerced column slice in
// tbl.rows' stored shape (IPK column NULLed; see indexColumnValue).
//
// A rowid collision is checked first, then every UNIQUE index on tbl. A NULL
// in any of an index's columns exempts it ("all NULL values are considered
// different"). The row being replaced or updated must already be absent from
// tbl.rows; opInsert checks before storing and opUpdateRow removes the old
// row first, so there is no exclude parameter.
func findRowConflicts(db *DB, tbl *tableMeta, candidateRowid uint64, candidateFull []Value) []conflictHit {
	var hits []conflictHit
	if _, exists := tbl.rows.get(candidateRowid); exists {
		hits = append(hits, conflictHit{rowid: candidateRowid, target: []string{"rowid"}})
	}
	// db.indexes is append-only, so it holds a table's indexes oldest
	// first. C's pTab->pIndex is newest first (sqlite3CreateIndex prepends,
	// build.c:4485-4486) and sqlite3GenerateConstraintChecks walks it as is
	// (insert.c:1895). Walking in reverse reproduces which conflict an ABORT
	// reports first and which victim a REPLACE deletes first -- visible
	// once a DELETE trigger has a side effect.
	for i := len(db.indexes) - 1; i >= 0; i-- {
		idx := db.indexes[i]
		if !idx.unique || !indexBelongsTo(idx, tbl) {
			continue
		}
		// An exprOrPartial index cannot be read by the column-keyed path below:
		// its colIdx/cols name the columns its expressions REFERENCE, not its
		// key (see indexMeta.exprOrPartial), and only WHERE-matching rows are
		// in it at all. It gets its own pass, which is what lets a conflict
		// CLAUSE -- OR IGNORE, OR REPLACE -- resolve against one.
		if idx.exprOrPartial {
			hits = append(hits, exprIndexConflicts(db, tbl, idx, candidateRowid, candidateFull)...)
			continue
		}
		cand := make([]Value, len(idx.colIdx))
		hasNull := false
		for i, ci := range idx.colIdx {
			v := indexColumnValue(tbl, candidateRowid, candidateFull, ci)
			if v.Typ == Null {
				hasNull = true
				break
			}
			cand[i] = v
		}
		if hasNull {
			continue
		}
		// Ask the store's conflict index (row_store_uniqindex.go) which rows MAY
		// hold this key, the way C asks the index, rather than reading every row
		// of the table: this runs once per row written, so the scan below made a
		// bulk INSERT or UPDATE against a UNIQUE index quadratic. Each candidate
		// is re-checked exactly as the scan checks it, so the index only prunes.
		if rowids, ok := tbl.rows.uniqConflictCandidates(tbl, idx, cand, db.encoding()); ok {
			if len(rowids) > 1 {
				rowids = slices.Clone(rowids)
				slices.SortFunc(rowids, func(a, b uint64) int { return cmp.Compare(int64(a), int64(b)) })
			}
			for _, rowid := range rowids {
				if rvals, have := tbl.rows.get(rowid); have && uniqueKeyMatches(db, tbl, idx, cand, rowid, rvals) {
					hits = append(hits, conflictHit{rowid: rowid, target: append([]string(nil), idx.cols...)})
				}
			}
			continue
		}
		hits = scanUniqueConflicts(db, tbl, idx, cand, hits)
	}
	return hits
}

// scanUniqueConflicts is findRowConflicts' full-table fallback for one unique
// index: every row whose key equals cand, appended to hits. Its own function
// because the range-over-func loop body is a closure, and a closure capturing
// findRowConflicts' hits moved hits to the heap on EVERY call -- one allocation
// per row written, whether or not the table has a unique index at all.
func scanUniqueConflicts(db *DB, tbl *tableMeta, idx *indexMeta, cand []Value, hits []conflictHit) []conflictHit {
	for rowid, rvals := range tbl.rows.all() {
		if uniqueKeyMatches(db, tbl, idx, cand, rowid, rvals) {
			hits = append(hits, conflictHit{rowid: rowid, target: append([]string(nil), idx.cols...)})
		}
	}
	return hits
}

// uniqueKeyMatches reports whether stored row rvals carries cand's key in idx:
// every key column non-NULL and equal under its collation. The one comparison
// both the scan and the index-pruned probe make.
func uniqueKeyMatches(db *DB, tbl *tableMeta, idx *indexMeta, cand []Value, rowid uint64, rvals []Value) bool {
	if len(rvals) < len(tbl.cols) {
		rvals = padStoredRow(tbl.cols, rvals) // reads as the column DEFAULT
	}
	for i, ci := range idx.colIdx {
		if ci < 0 || ci >= len(rvals) {
			return false
		}
		ev := indexColumnValue(tbl, rowid, rvals, ci)
		if ev.Typ == Null || compareValuesCollatedEnc(ev, cand[i], effectiveCollation(atOrEmpty(idx.colCollation, i)), db.encoding()) != 0 {
			return false
		}
	}
	return true
}

// exprIndexConflicts is findRowConflicts' pass for a UNIQUE expression or
// partial index: keys are evaluated, and a row outside the partial WHERE is
// not in the index. A NULL key never conflicts, so (NULL,1) and (NULL,2) fit
// an index on a+b.
//
// The WHERE and keys are lowered by compileIndexExprs, the same seam
// indexEntryOf and validateUniqueIndex use, so "what conflicts" and "what the
// index holds" have one evaluator. A shape the compiler declines declines for
// all three.
func exprIndexConflicts(db *DB, tbl *tableMeta, idx *indexMeta, candidateRowid uint64, candidateFull []Value) []conflictHit {
	whereProg, keyProgs := compileIndexExprs(tbl, idx)
	inIndex := func(rowid uint64, vals []Value) ([]Value, bool) {
		ctx := rowEvalCtx(tbl, rowid, vals, nil, nil, nil)
		if idx.where != nil {
			w, err := whereProg.eval(ctx)
			if err != nil || !isTruthy(w) {
				return nil, false
			}
		}
		key := make([]Value, len(idx.keys))
		for i, k := range idx.keys {
			var v Value
			if k.colIdx >= 0 {
				v = indexColumnValue(tbl, rowid, vals, k.colIdx)
			} else {
				ev, err := keyProgs[i].eval(ctx)
				if err != nil {
					return nil, false
				}
				v = ev
			}
			if v.Typ == Null {
				return nil, false
			}
			key[i] = v
		}
		return key, true
	}
	cand, ok := inIndex(candidateRowid, candidateFull)
	if !ok {
		return nil
	}
	// An expression key has no column to name; a partial index over plain
	// columns still reports them (uniqueConflictError's own split).
	var target []string
	idxName := ""
	for _, k := range idx.keys {
		if k.colIdx < 0 {
			idxName = idx.name
			target = nil
			break
		}
		target = append(target, tbl.cols[k.colIdx].Name)
	}
	var hits []conflictHit
	for rowid, rvals := range tbl.rows.all() {
		if rowid == candidateRowid {
			continue
		}
		other, ok := inIndex(rowid, rvals)
		if !ok {
			continue
		}
		match := true
		for i := range cand {
			if compareValuesCollatedEnc(other[i], cand[i], effectiveCollation(idx.keys[i].collation), db.encoding()) != 0 {
				match = false
				break
			}
		}
		if match {
			hits = append(hits, conflictHit{rowid: rowid, target: target, idxName: idxName})
		}
	}
	return hits
}

// dedupRowids returns the distinct rowids named across hits, in the order
// each first appears -- REPLACE deletes each conflicting row exactly once
// even when it was found via more than one violated constraint (e.g. a
// candidate row conflicting with the SAME existing row on two different
// UNIQUE columns at once).
func dedupRowids(hits []conflictHit) []uint64 {
	seen := make(map[uint64]bool, len(hits))
	out := make([]uint64, 0, len(hits))
	for _, h := range hits {
		if !seen[h.rowid] {
			seen[h.rowid] = true
			out = append(out, h.rowid)
		}
	}
	return out
}

// uniqueConflictError renders hit as C SQLite's own "UNIQUE constraint
// failed: table.col[, table.col...]" wording -- verified directly against
// mattn/go-sqlite3 for a single-column UNIQUE index, a multi-column
// UNIQUE(a,b) index, an INTEGER PRIMARY KEY rowid collision (uses that
// column's own declared name), and a rowid collision on a table with no IPK
// column at all (uses the literal name "rowid").
func uniqueConflictError(tbl *tableMeta, hit conflictHit) error {
	if len(hit.target) == 1 && hit.target[0] == "rowid" {
		colName := "rowid"
		if tbl.ipkIndex >= 0 {
			colName = tbl.cols[tbl.ipkIndex].Name
		}
		return fmt.Errorf("UNIQUE constraint failed: %s.%s", tbl.name, colName)
	}
	if hit.idxName != "" {
		return fmt.Errorf("UNIQUE constraint failed: index '%s'", hit.idxName)
	}
	parts := make([]string, len(hit.target))
	for i, n := range hit.target {
		parts[i] = fmt.Sprintf("%s.%s", tbl.name, n)
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
}

// validateUpsertTarget confirms, once per statement (C rejects a bad target at
// prepare time even if nothing conflicts), that an explicit ON CONFLICT(cols)
// names the column set of a UNIQUE index on tbl or, as a single column, the
// rowid (its IPK name or a rowid spelling). An empty list matches any conflict
// and needs no check. Column order does not matter (targetKey). The error is
// C's own wording.
func validateUpsertTarget(db *DB, tbl *tableMeta, targetCols, targetCollations []string, targetWhere Expr) error {
	if len(targetCols) == 0 {
		return nil
	}
	// RESOLUTION COMES FIRST. sqlite3UpsertAnalyzeTarget resolves the target's
	// names before it looks at a single index -- "sqlite3ResolveExprListNames(
	// &sNC, pUpsert->pUpsertTarget)" at upsert.c:119, ahead of every match
	// attempt below it -- so a name that is no column of the table at all is
	// resolve.c:785's "no such column: x" and never this clause's own
	// complaint. "ON CONFLICT(nosuch)" read as "does not match any PRIMARY KEY
	// or UNIQUE constraint" here, which is true and is not what C says.
	for _, n := range targetCols {
		if _, ok := resolveColumnOrRowidTarget(tbl, n); !ok {
			return semanticf("engine: no such column: %s", n)
		}
	}
	key := targetKey(targetCols)
	if len(targetCols) == 1 && !tbl.withoutRowid {
		if tbl.ipkIndex >= 0 && equalFoldName(tbl.cols[tbl.ipkIndex].Name, targetCols[0]) {
			return nil
		}
		// The literal rowid spellings match the rowid target even on a table
		// with an INTEGER PRIMARY KEY: upsert.c:128-135 tests only
		// "HasRowid(pTab) && pTarget->nExpr==1 && pTerm->iColumn==XN_ROWID",
		// and resolve.c:564/567 maps rowid/oid/_rowid_ and the IPK name alike
		// to iColumn==-1. WITHOUT ROWID has no rowid (HasRowid false), which
		// the pre-resolution check above already reports.
		if isRowidAliasName(targetCols[0]) {
			return nil
		}
	}
	for _, idx := range db.indexes {
		if !idx.unique || !indexBelongsTo(idx, tbl) {
			continue
		}
		if idx.exprOrPartial {
			// C SQLite (upsert.c's sqlite3UpsertAnalyzeTarget) matches a
			// partial index ONLY when the target itself carries a WHERE that
			// is EXACTLY (sqlite3ExprCompare-equal to) the index's own
			// pPartIdxWhere -- a WHERE-less target never matches a partial
			// index, regardless of column overlap. cols/colIdx on an
			// exprOrPartial index name the columns its expression/WHERE
			// REFERENCE, not its key (see indexMeta.exprOrPartial), so the key
			// list to compare against targetCols is idx.keys instead -- and
			// only a plain-column key list can match at all, since this
			// engine's own conflict-target grammar accepts no expression
			// terms (parseUpsertClause).
			if targetWhere == nil || idx.where == nil || !exprEqual(targetWhere, idx.where) {
				continue
			}
			if len(idx.keys) != len(targetCols) {
				continue
			}
			cols := make([]string, len(idx.keys))
			for i, k := range idx.keys {
				if k.colIdx < 0 {
					cols = nil
					break
				}
				cols[i] = tbl.cols[k.colIdx].Name
			}
			if cols == nil || targetKey(cols) != key {
				continue
			}
			if !partialTargetCollationsMatch(tbl, idx, targetCols, targetCollations) {
				continue
			}
			return nil
		}
		// A target's WHERE is only ever COMPARED against a partial candidate
		// (the branch above) -- upsert.c's own check is nested inside
		// "if( pIdx->pPartIdxWhere )", so for an ORDINARY (non-partial) index
		// the target's WHERE is not consulted for matching at all and does
		// not rule this index out. Verified directly: "ON CONFLICT(b,c,d)
		// WHERE a!=0 DO NOTHING" against a plain (non-partial) UNIQUE(d,c,b)
		// matches and applies (upsert4.test 2.1.2.6).
		if targetKey(idx.cols) == key && targetCollationsMatch(idx, targetCols, targetCollations) {
			return nil
		}
	}
	// semanticf: C raises this at PREPARE time too (sqlite3UpsertAnalyzeTarget's
	// own tail, upsert.c), so it is the STATEMENT's error. As a plain error it
	// reached the caller as "unsupported: engine: ON CONFLICT clause does not
	// match ..." -- the right words wearing a wrapper C has no counterpart for.
	return semanticf("engine: ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint")
}

// partialTargetCollationsMatch is targetCollationsMatch for an exprOrPartial
// index, whose per-key collation lives in idx.keys[i].collation rather than
// the parallel idx.colCollation array a plain-column index uses.
func partialTargetCollationsMatch(tbl *tableMeta, idx *indexMeta, targetCols, targetCollations []string) bool {
	for i, tc := range targetCols {
		want := targetCollations[i]
		if want == "" {
			continue
		}
		for _, k := range idx.keys {
			if k.colIdx >= 0 && equalFoldName(tbl.cols[k.colIdx].Name, tc) {
				if !equalFoldName(k.collation, want) {
					return false
				}
				break
			}
		}
	}
	return true
}

// targetCollationsMatch reports whether idx's own EFFECTIVE per-column
// collation (indexMeta.colCollation) agrees with every EXPLICIT "COLLATE
// name" the conflict target gave. A target column with no COLLATE of its
// own matches any index -- exactly like the bare "ON CONFLICT(b)" case
// already does -- so this only ever NARROWS which index an explicitly
// collated target can match, never widens it. Verified directly: over
// "CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT UNIQUE)" (b's unique index
// is BINARY, the default), "ON CONFLICT(b COLLATE binary)" matches and
// applies the upsert, while "ON CONFLICT(b COLLATE nocase)" does NOT --
// C SQLite fails the whole INSERT, not merely this one candidate index.
func targetCollationsMatch(idx *indexMeta, targetCols, targetCollations []string) bool {
	for i, tc := range targetCols {
		want := targetCollations[i]
		if want == "" {
			continue
		}
		for j, ic := range idx.cols {
			if equalFoldName(ic, tc) {
				if !equalFoldName(idx.colCollation[j], want) {
					return false
				}
				break
			}
		}
	}
	return true
}

// upsertTargetKey is targetKey for the WRITTEN conflict target, normalized the
// same way hitTargetKey normalizes a HIT's: on a rowid table, a single
// rowid/oid/_rowid_ spelling names the ROWID target, which is the very target
// the INTEGER PRIMARY KEY column's own name names -- resolve.c:564/567
// collapses all four spellings to iColumn==-1 and upsert.c:128-135 matches on
// exactly that. Without this the two sides normalized differently: a hit on an
// IPK collision came back as the IPK's NAME while the written "ON
// CONFLICT(rowid)" stayed "rowid", so the upsert matched nothing and the
// statement raised UNIQUE where 3.53.3 applies the DO NOTHING/DO UPDATE.
func upsertTargetKey(tbl *tableMeta, targetCols []string) string {
	if len(targetCols) == 1 && !tbl.withoutRowid && isRowidAliasName(targetCols[0]) {
		if tbl.ipkIndex >= 0 {
			return targetKey([]string{tbl.cols[tbl.ipkIndex].Name})
		}
		return targetKey([]string{"rowid"})
	}
	return targetKey(targetCols)
}

// upsertMatchingHitsByCols filters hits to those matching the (validated)
// conflict target, or returns them unchanged when no target was given: a bare
// ON CONFLICT DO NOTHING / DO UPDATE matches any conflict.
func upsertMatchingHitsByCols(tbl *tableMeta, targetCols []string, hits []conflictHit) []conflictHit {
	if len(targetCols) == 0 {
		return hits
	}
	want := upsertTargetKey(tbl, targetCols)
	var out []conflictHit
	for _, h := range hits {
		if hitTargetKey(tbl, h) == want {
			out = append(out, h)
		}
	}
	return out
}

// declaredNotNullAction returns tbl.cols[idx]'s own declared NOT NULL "ON
// CONFLICT <action>" resolution (conflictAbort when none was declared, or
// idx is out of range) -- see columnInfo.NotNullConflict's own doc comment.
func declaredNotNullAction(tbl *tableMeta, idx int) conflictAction {
	if idx < 0 || idx >= len(tbl.cols) {
		return conflictAbort
	}
	return tbl.cols[idx].NotNullConflict
}

// declaredHitAction returns the ON CONFLICT action the constraint that
// produced hit itself declared: columnInfo.RowidConflict for a rowid
// collision (hit.target == []string{"rowid"} -- the INTEGER PRIMARY KEY
// column's own declared action when tbl has one, or conflictAbort for a
// table with no IPK column at all, which cannot declare one), or the
// matching indexMeta.onConflict for an index-backed UNIQUE/PRIMARY KEY hit.
// conflictAbort (C SQLite's own implicit default) when the constraint
// declared none.
func declaredHitAction(db *DB, tbl *tableMeta, hit conflictHit) conflictAction {
	if len(hit.target) == 1 && hit.target[0] == "rowid" {
		if tbl.ipkIndex >= 0 {
			return tbl.cols[tbl.ipkIndex].RowidConflict
		}
		return conflictAbort
	}
	if hit.idxName != "" {
		for _, idx := range db.indexes {
			if equalFoldName(idx.name, hit.idxName) {
				return idx.onConflict
			}
		}
		return conflictAbort
	}
	key := targetKey(hit.target)
	for _, idx := range db.indexes {
		// An exprOrPartial index is skipped here on purpose: its colIdx/cols
		// name the columns its expressions REFERENCE, not its key, and only
		// WHERE-matching rows are in it -- so this column-keyed conflict
		// machinery cannot read it. validateUniqueIndex enforces those after
		// the statement instead, and a non-ABORT conflict action on such a
		// table is declined rather than mis-resolved here.
		if !idx.unique || idx.exprOrPartial || !indexBelongsTo(idx, tbl) {
			continue
		}
		if targetKey(idx.cols) == key {
			return idx.onConflict
		}
	}
	return conflictAbort
}

// effectiveHitAction is the conflict-resolution action that ACTUALLY governs
// hit for this one statement: the statement's own EXPLICIT OR-clause
// (stmtAction), if it gave one (explicitOr), always wins uniformly over
// every constraint's own declared default -- otherwise the constraint that
// produced hit governs itself, via declaredHitAction. See
// insertStmt.explicitOr's doc comment for why "no clause at all" and "an
// explicit OR ABORT" must be told apart here (both leave stmtAction at the
// conflictAbort zero value).
func effectiveHitAction(db *DB, tbl *tableMeta, hit conflictHit, stmtAction conflictAction, explicitOr bool) conflictAction {
	if explicitOr {
		return stmtAction
	}
	return declaredHitAction(db, tbl, hit)
}

// effectiveNotNullAction is effectiveHitAction's NOT NULL counterpart: the
// statement's own explicit OR-clause wins uniformly when given, else the
// violating column's own declared NotNullConflict governs.
func effectiveNotNullAction(tbl *tableMeta, idx int, stmtAction conflictAction, explicitOr bool) conflictAction {
	if explicitOr {
		return stmtAction
	}
	return declaredNotNullAction(tbl, idx)
}

// tableHasDeclaredConflict reports whether ANY constraint registered against
// tbl (a NOT NULL column, the INTEGER PRIMARY KEY/rowid-alias column's own
// PRIMARY KEY, or any UNIQUE/PRIMARY KEY index) declared a non-default (not
// conflictAbort) "ON CONFLICT <action>" of its own. Consulted by
// vdbe_write.go's compileInsertWrite/compileUpdateWrite to decide whether a
// statement with NO explicit OR-clause of its own must still emit the per-row
// conflict-aware machinery instead of the plain, original
// insert-then-verify-once/apply-then-verify-once pair -- a table with no
// declared conflict action anywhere behaves EXACTLY as before this feature,
// so no pre-existing INSERT/UPDATE behavior can regress from adding it.
func (db *DB) tableHasDeclaredConflict(tbl *tableMeta) bool {
	for _, c := range tbl.cols {
		if c.NotNull && c.NotNullConflict != conflictAbort {
			return true
		}
	}
	if tbl.ipkIndex >= 0 && tbl.cols[tbl.ipkIndex].RowidConflict != conflictAbort {
		return true
	}
	for _, idx := range db.indexes {
		if idx.unique && !idx.exprOrPartial && indexBelongsTo(idx, tbl) && idx.onConflict != conflictAbort {
			return true
		}
	}
	return false
}

// classifyHits partitions one candidate row's hits (findRowConflicts) by each
// hit's effective action (actionOf), in C's processing order: every
// non-REPLACE hit is checked first, in findRowConflicts' order, and REPLACE is
// applied only if every hit is a REPLACE (conflict.test conflict-15.20: a row
// hitting a plain PRIMARY KEY and an ON CONFLICT REPLACE UNIQUE reports the PK
// failure). nonReplace and nonReplaceActions are parallel; replaceRowids is
// the deduplicated victim set, used only when nonReplace is empty.
func classifyHits(hits []conflictHit, actionOf func(conflictHit) conflictAction) (nonReplace []conflictHit, nonReplaceActions []conflictAction, replaceRowids []uint64) {
	var replaceHits []conflictHit
	for _, h := range hits {
		if actionOf(h) == conflictReplace {
			replaceHits = append(replaceHits, h)
			continue
		}
		nonReplace = append(nonReplace, h)
		nonReplaceActions = append(nonReplaceActions, actionOf(h))
	}
	replaceRowids = dedupRowids(replaceHits)
	return
}

// replaceRecheckHit reports the first hit of the recheck run right after a
// REPLACE victim's DELETE triggers fire (fireReplaceVictimDeleteRow) whose
// effective action would itself be REPLACE. C emits that recheck only for an
// index resolving to OE_Replace (insert.c:2601), so hits on other indexes are
// not reported, and a reported hit always aborts: C hardcodes the recheck's
// resolution to OE_Abort (insert.c:2199, 2663).
func replaceRecheckHit(db *DB, tbl *tableMeta, hits []conflictHit, stmtAction conflictAction, explicitOr bool) (conflictHit, bool) {
	for _, h := range hits {
		if effectiveHitAction(db, tbl, h, stmtAction, explicitOr) == conflictReplace {
			return h, true
		}
	}
	return conflictHit{}, false
}

// conflictFailErr tags an error whose ON CONFLICT action is FAIL, so a
// triggered write's per-statement snapshot (beginStatementSnapshot) does not
// undo anything -- the same exemption as RAISE(FAIL). Under FAIL everything
// before the offending row survives, including what BEFORE triggers wrote to
// other tables: "INSERT OR FAIL INTO t VALUES(1,'dup')" with a BEFORE INSERT
// trigger logging the row keeps the log row, while OR ABORT/ROLLBACK leaves
// the log empty. The message passes through unchanged and Unwrap keeps
// errors.Is/As working.
type conflictFailErr struct{ err error }

func (e *conflictFailErr) Error() string { return e.err.Error() }
func (e *conflictFailErr) Unwrap() error { return e.err }

// markConflictFail tags err per conflictFailErr when action is FAIL, and
// returns it untouched otherwise. Called at the write paths' own conflict
// resolution points, so the tag rides all the way out to the deferred
// snapshot restore.
func markConflictFail(action conflictAction, err error) error {
	if action != conflictFail || err == nil {
		return err
	}
	return &conflictFailErr{err: err}
}

// isConflictFailErr reports whether err is (or wraps) a FAIL-tagged conflict
// error -- errors.As so the classification survives any %w-wrapping between
// the resolution point and the statement boundary.
func isConflictFailErr(err error) bool {
	var cf *conflictFailErr
	return errors.As(err, &cf)
}

// haltCount is the row count a statement ending with err publishes to
// changes()/total_changes(): n on success or FAIL, else 0 -- sqlite3VdbeHalt's
// rule:
//
//	if( eStatementOp!=SAVEPOINT_ROLLBACK ){
//	  sqlite3VdbeSetChanges(db, p->nChange);
//	}else{
//	  sqlite3VdbeSetChanges(db, 0);
//	}
//
// The statement journal is unwound for ABORT and ROLLBACK and kept for FAIL.
func haltCount(n int, err error) int64 {
	if err != nil && !isConflictFailErr(err) {
		return 0
	}
	return int64(n)
}
