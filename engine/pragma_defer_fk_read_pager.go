package engine

import (
	"errors"
	"fmt"
	"strings"
)

// ---- "PRAGMA defer_foreign_keys" and a READ with no write session ----
//
// Whether C SQLite would clear SQLITE_DeferFKs after a read: its own bIsReader
// flag (vdbeaux.c:871-910). The verdict does not depend on the query's outcome
// (bIsReader is fixed at PREPARE). A prepare failure keeps the flag since no
// Vdbe is created to clear anything.

// deferFKReadClears classifies one top-level READ for whether C SQLite would
// clear SQLITE_DeferFKs after it: clears is the answer, known is whether this
// engine can give one. An unknown verdict declines the statement rather than
// guess -- see deferFKPagerReadGuard.
func (p *ReadOnlyPager) deferFKReadClears(sqlText string) (clears, known bool) {
	if p == nil {
		return false, false
	}
	if p.tempOpen {
		// A temp object can be read without naming a schema, so "every FROM
		// item is a main table" stops being decidable from the text. The *DB
		// twin gives up for exactly this reason (holdsAnyTempObject).
		return false, false
	}
	stmt, err := ParseSelect(sqlText)
	if err != nil {
		// Parse failure: flag survives since no Vdbe is built.
		return false, true
	}
	if len(stmt.CTEs) != 0 {
		return false, false
	}
	if len(stmt.From) == 0 {
		// No FROM, and no subquery to hide a cursor in ("SELECT 1"): provably no
		// reader. A subquery makes it one ("SELECT (SELECT count(*) FROM t)"),
		// so that falls through to unknown.
		if selectHasAnySubquery(stmt) {
			return false, false
		}
		return false, true
	}
	for _, it := range stmt.From {
		if it.Subquery != nil || it.TableFunc || it.Table == "" {
			return false, false
		}
		if it.Schema != "" && !equalFoldName(it.Schema, "main") {
			return false, false
		}
		if isMainSchemaCatalogName(it.Table) {
			continue // sqlite_master / sqlite_schema: a real cursor
		}
		if _, rerr := p.resolveTable(it.Table); rerr != nil {
			// No such table: C's PREPARE fails, so no Vdbe and no clear.
			return false, true
		}
	}
	return true, true
}

// selectHasAnySubquery reports whether stmt's own level holds a subquery
// anywhere a cursor could hide -- the select list, WHERE, HAVING, GROUP BY or
// ORDER BY. Used only to keep a FROM-less SELECT's "no reader" verdict honest.
func selectHasAnySubquery(stmt *SelectStmt) bool {
	if stmt == nil {
		return false
	}
	if len(stmt.Compound) != 0 {
		return true // an arm can have its own FROM; not worth classifying here
	}
	for _, sc := range stmt.Columns {
		if containsSubquery(sc.Expr) {
			return true
		}
	}
	if containsSubquery(stmt.Where) || containsSubquery(stmt.Having) {
		return true
	}
	for _, g := range stmt.GroupBy {
		if containsSubquery(g) {
			return true
		}
	}
	for _, o := range stmt.OrderBy {
		if containsSubquery(o.Expr) {
			return true
		}
	}
	return false
}

// deferFKPagerReadGuard is the guard half: it declines a read this engine
// cannot classify while the flag is on, exactly as the *DB twin does, and
// otherwise returns nil. A no-op the moment the flag is off, which is every
// ordinary query.
// deferFKStatementClears classifies one top-level READ for "PRAGMA
// defer_foreign_keys" in autocommit: a PRAGMA by DeferFKPragmaClears' measured
// table, a SELECT by deferFKReadClears. The write session's read path and this
// pager's own ask the same question, so they share the answer.
func (p *ReadOnlyPager) deferFKStatementClears(sqlText string) (clears, known bool) {
	if toks, err := lex(strings.TrimSpace(sqlText)); err == nil && isPragmaStmt(toks) {
		return DeferFKPragmaClears(sqlText)
	}
	return p.deferFKReadClears(sqlText)
}

func (p *ReadOnlyPager) deferFKPagerReadGuard(sqlText string) error {
	if p == nil || !p.deferFKs || p.writeSession != nil {
		return nil
	}
	if _, known := p.deferFKReadClears(sqlText); known {
		return nil
	}
	return fmt.Errorf("%w: PRAGMA defer_foreign_keys is ON with no transaction open, and this READ is outside the shapes this engine can classify for whether C SQLite would then clear it (see engine/pragma_defer_fk_read_pager.go) -- declined rather than guessed: %s", errVDBEUnsupported, sqlText)
}

// deferFKPagerReadApply is the other half, called AFTER the read whatever its
// outcome -- see this file's doc comment for why the outcome plays no part. The
// caller (a driver carrying the flag across sessions) reads the pager's value
// back afterwards.
func (p *ReadOnlyPager) deferFKPagerReadApply(sqlText string, err error) {
	if p == nil || !p.deferFKs || p.writeSession != nil {
		return
	}
	if errors.Is(err, errVDBESemantic) {
		return // failed to PREPARE: no program, so nothing cleared it in C
	}
	if clears, known := p.deferFKReadClears(sqlText); known && clears {
		p.deferFKs = false
	}
}

// DeferFKPragmaClears applies deferFKReadClears to a PRAGMA statement,
// exported for drivers that answer pragmas themselves. A PRAGMA that codes a
// read transaction clears the flag; one that does not keeps it.
func DeferFKPragmaClears(sqlText string) (clears, known bool) {
	stmt, err := ParsePragma(sqlText)
	if err != nil || stmt == nil {
		return false, false
	}
	switch {
	case mainReadTxnPragmaYes[stmt.Name]:
		return true, true
	case mainReadTxnPragmaNo[stmt.Name]:
		return false, true
	}
	return false, false
}
