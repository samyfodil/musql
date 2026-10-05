// CREATE TRIGGER / DROP TRIGGER, trigger metadata, and the eager validation a
// firing statement runs over its triggers. A trigger's SQL is stored verbatim
// in sqlite_schema (type='trigger', rootpage=0) and re-parsed on open.
//
// Semantics, each checked against C SQLite:
//
//   - BEFORE fires before the row write, AFTER after; omitted timing is
//     BEFORE. Several triggers on one (table, event, timing) fire in REVERSE
//     creation order. Each row's BEFORE -> write -> AFTER completes before the
//     next row starts.
//   - UPDATE OF col-list fires whenever a named column is in SET, even if its
//     value does not change.
//   - DELETE has only OLD, INSERT only NEW. Referencing the missing side is a
//     fire-time "no such column: NEW.x", raised once per firing statement even
//     when it matches zero rows (validateTriggerExprsOnce).
//   - An unqualified reference in a body/WHEN never resolves against OLD/NEW
//     (tableScope.unqualifiedHidden, as for UPSERT's "excluded").
//   - With recursive_triggers OFF (the default) only the SAME trigger is kept
//     from re-entering itself; distinct triggers cascade freely. ON lifts that
//     guard (recursion is then bounded by SQLITE_MAX_TRIGGER_DEPTH) and makes a
//     REPLACE victim's delete fire DELETE triggers
//     (compileReplaceVictimDeletePlans, vdbe_write.go).
//   - A trigger's name shares no namespace with tables, views or indexes.
//   - DROP TABLE removes the table's triggers (drop_write.go).
//
// A bare SELECT body statement, or INSERT ... SELECT, is accepted for an INSERT
// trigger (which always fires at least once); for UPDATE/DELETE only shapes
// this file can validate eagerly are accepted, since C compiles the body at
// prepare time and rejects a bad reference even for a zero-row firing.
//
// Eager validation covers only the directly-fired triggers; an error two or
// more trigger levels deep surfaces only once a row reaches that cascade,
// where C would report it at prepare time.
package engine

import (
	"errors"
	"fmt"
)

// ---- RAISE() ----
//
// A trigger body/WHEN "RAISE(...)" (RaiseExpr, legal only inside a trigger
// program) yields no value; it returns one of these sentinels:
//
//   - RAISE(ABORT, msg): undo the whole statement and return msg (default).
//   - RAISE(FAIL, msg): return msg but keep changes made so far.
//   - RAISE(ROLLBACK, msg): unwind the enclosing explicit transaction (a later
//     COMMIT errors "cannot commit - no transaction is active"); ABORT with
//     none active.
//   - RAISE(IGNORE): no error; abandon the current row: its remaining
//     triggers do not fire and, from BEFORE, its write is skipped.
//
// The message is evaluated at fire time and coerced to text; NULL yields
// "constraint failed".
type raiseError struct {
	action conflictAction // conflictIgnore, or conflictAbort/conflictFail/conflictRollback
	msg    string         // meaningful only when action != conflictIgnore
}

func (e *raiseError) Error() string {
	if e.action == conflictIgnore {
		return "engine: RAISE(IGNORE) escaped its trigger program" // internal; caught at the DML row loop, never surfaced
	}
	return "engine: " + e.msg
}

// asRaiseError reports whether err is (or wraps) a *raiseError. errors.As is
// used so the classification survives any %w-wrapping between the RAISE
// evaluation and the statement boundary (in practice a RAISE error propagates
// unwrapped, but this is robust either way).
func asRaiseError(err error) (*raiseError, bool) {
	var re *raiseError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

// isRaiseIgnore reports whether err is a RAISE(IGNORE) signal.
func isRaiseIgnore(err error) bool {
	re, ok := asRaiseError(err)
	return ok && re.action == conflictIgnore
}

// ErrorKeepsAutocommitChanges reports whether err halted a statement the FAIL
// way: RAISE(FAIL) or an "OR FAIL" conflict. FAIL keeps the changes already
// made, so the driver must COMMIT an autocommit statement's state on this error
// rather than discard it (e.g. "INSERT OR FAIL INTO t VALUES(1),(2),(3),(4)"
// over t holding 3 keeps 1,2,3). ABORT/ROLLBACK leave the state already
// reverted.
func ErrorKeepsAutocommitChanges(err error) bool {
	if isConflictFailErr(err) {
		return true
	}
	re, ok := asRaiseError(err)
	return ok && re.action == conflictFail
}

// ErrorRollsBackTransaction reports whether err is a RAISE(ROLLBACK) trigger
// error, whose defining behavior is that the ENTIRE enclosing transaction is
// unwound (a following COMMIT then errors "cannot commit - no transaction is
// active"). When a database/sql driver models a transaction as a held engine
// session (as driver does) rather than the engine's own SQL BEGIN/COMMIT --
// so the engine's internal txActive is not set and its own transaction-wide
// rollback in beginStatementSnapshot does not fire -- the driver consults this
// to Discard the whole held session, matching C SQLite.
func ErrorRollsBackTransaction(err error) bool {
	// Two distinct sources roll the whole transaction back: an explicit
	// RAISE(ROLLBACK) in a trigger body, and an ON CONFLICT ROLLBACK
	// constraint violation the engine could not unwind itself because no
	// engine-level transaction was active (ErrConflictRollback, vdbe_write.go).
	if errors.Is(err, ErrConflictRollback) {
		return true
	}
	re, ok := asRaiseError(err)
	return ok && re.action == conflictRollback
}

var errRaiseIgnore error = &raiseError{action: conflictIgnore}

// ---- data model ----

type triggerTiming int

const (
	triggerBefore triggerTiming = iota
	triggerAfter
)

type triggerEventKind int

const (
	triggerInsert triggerEventKind = iota
	triggerUpdate
	triggerDelete
)

// triggerBodyStmt is one parsed statement of a trigger body: exactly one of
// insert/update/delete/sel is set. A bare SELECT body (sel) is executed and its
// result discarded, but its runtime errors still abort the firing statement.
// An INSERT ... SELECT is held in insert. See the package doc for which
// triggers accept the SELECT-sourced shapes.
type triggerBodyStmt struct {
	insert *insertStmt
	update *updateStmt
	delete *deleteStmt
	sel    *SelectStmt
}

// triggerMeta is what this writer needs to remember about a trigger --
// whether created this session (CreateTrigger) or recovered from an existing
// database (OpenWrite) -- to fire it and, at Close, re-emit its verbatim
// CREATE TRIGGER text into sqlite_schema (see view.go's viewMeta, which this
// mirrors: a trigger has no b-tree of its own either).
type triggerMeta struct {
	// schemaSeq is this object's creation rank, the order its sqlite_schema row
	// appears in (C appends each new object's row). materialize (writer.go) sorts
	// by it so a rebuilt schema keeps creation order. Assigned by DB.nextSchemaSeq,
	// recovered from row position on open, and unchanged by ALTER TABLE.
	schemaSeq uint64

	// isTemp is this trigger's own catalog; tableIsTemp is the catalog of the
	// TABLE (or view) it fires on, which need not be the same one: "CREATE TEMP
	// TRIGGER trig1 AFTER INSERT ON main.t4" is a temp trigger on a main table,
	// and must fire for main.t4's inserts only -- not for a temp t4 of the same
	// name (trigger1.test 10.x). Every trigger lookup matches on BOTH the table
	// name and tableIsTemp. See temp_schema.go.
	isTemp      bool
	tableIsTemp bool

	// tableAttachName is the attached database the target table lives in, when
	// non-empty (tableIsTemp is then false). C keeps the trigger's own schema and
	// its target's separately (pSchema/pTabSchema, trigger.c:268-275), and only a
	// TEMP trigger may target an attachment (sqlite3FixSrcList,
	// attach.c:485-527, 533-555; see resolveCreateTriggerAttachedTarget).
	//
	// Every local trigger match (matchingTriggers,
	// tableHasTriggers, matchingInsteadOfTriggers, triggersCompilable, DROP TABLE's
	// cascade) must require this empty, or a same-named local table would steal it
	// (trigger1.test 10.x has a t4 in main, temp and an attachment). It is a name,
	// re-resolved via db.attachedNamed on every use, never a cached binding that
	// could go stale across DETACH/re-ATTACH.
	tableAttachName string

	name  string
	sql   string // verbatim CREATE TRIGGER text, stored in sqlite_schema at Close
	table string

	timing triggerTiming
	event  triggerEventKind

	// insteadOf is true for an INSTEAD OF trigger (view-only): its timing is
	// irrelevant (INSTEAD OF has no BEFORE/AFTER), and instead of firing around
	// a base-table row write it REPLACES a DML statement against the view --
	// see view_trigger.go for the view-DML routing and OLD/NEW-for-view
	// semantics. A table trigger always has insteadOf == false.
	insteadOf bool

	// updateCols is the "OF col[,col...]" list for an UPDATE trigger, or nil
	// when the trigger applies to any UPDATE regardless of which columns are
	// assigned (including every non-UPDATE trigger). See filterUpdateTriggers.
	updateCols []string

	when Expr // WHEN clause, or nil if none was given
	body []triggerBodyStmt
}

// matchingTriggers returns every trigger registered against table for the
// given (event, timing), in REVERSE creation order -- see this file's
// package doc comment: same-timing triggers on the same table+event fire
// most-recently-created first, verified directly against C SQLite.
//
// See triggerMeta.tableAttachName for why it must be empty to match here.
func (db *DB) matchingTriggers(table string, tableIsTemp bool, event triggerEventKind, timing triggerTiming) []*triggerMeta {
	var out []*triggerMeta
	for i := len(db.triggers) - 1; i >= 0; i-- {
		tr := db.triggers[i]
		if !tr.insteadOf && tr.event == event && tr.timing == timing && tr.tableAttachName == "" && tr.tableIsTemp == tableIsTemp && equalFoldName(tr.table, table) {
			out = append(out, tr)
		}
	}
	// foreignTempTriggers (writer.go) are matched without the
	// tableAttachName/tableIsTemp checks, after this session's own matches.
	// That ordering between local and foreign triggers for the same
	// event/timing is a deterministic choice, not one verified against C.
	for i := len(db.foreignTempTriggers) - 1; i >= 0; i-- {
		tr := db.foreignTempTriggers[i]
		if !tr.insteadOf && tr.event == event && tr.timing == timing && equalFoldName(tr.table, table) {
			out = append(out, tr)
		}
	}
	return out
}

// matchingInsteadOfTriggers returns every INSTEAD OF trigger registered
// against view for the given event, in REVERSE creation order -- exactly like
// matchingTriggers' verified same-timing ordering for table triggers (probed
// directly: two INSTEAD OF INSERT triggers on one view fire most-recently-
// created first). INSTEAD OF has no BEFORE/AFTER timing, so there is no timing
// filter. See triggerMeta.tableAttachName for why it must be empty to match
// here.
func (db *DB) matchingInsteadOfTriggers(view string, viewIsTemp bool, event triggerEventKind) []*triggerMeta {
	var out []*triggerMeta
	for i := len(db.triggers) - 1; i >= 0; i-- {
		tr := db.triggers[i]
		if tr.insteadOf && tr.event == event && tr.tableAttachName == "" && tr.tableIsTemp == viewIsTemp && equalFoldName(tr.table, view) {
			out = append(out, tr)
		}
	}
	return out
}

// tableHasTriggers reports whether table has ANY trigger (BEFORE or AFTER)
// registered for event -- the gate vdbe_write.go's compileInsertWrite/
// compileUpdateWrite/compileDeleteWrite use to decide whether a write needs
// trigger fire plans compiled around it at all, and the gate insert_write.go
// uses to keep the xfer optimization off a triggered destination. See
// triggerMeta.tableAttachName for why it must be empty to match here.
func (db *DB) tableHasTriggers(table string, tableIsTemp bool, event triggerEventKind) bool {
	for _, tr := range db.triggers {
		if !tr.insteadOf && tr.event == event && tr.tableAttachName == "" && tr.tableIsTemp == tableIsTemp && equalFoldName(tr.table, table) {
			return true
		}
	}
	// foreignTempTriggers (writer.go) count here too -- this is the gate
	// vdbe_write.go's compileInsertWrite/compileUpdateWrite/
	// compileDeleteWrite use to decide whether to even LOOK for a fire plan
	// at all; missing a foreign match here would silently store the row
	// with NO trigger firing whatsoever, worse than any decline.
	for _, tr := range db.foreignTempTriggers {
		if !tr.insteadOf && tr.event == event && equalFoldName(tr.table, table) {
			return true
		}
	}
	return false
}

// tableHasAnyAttachedTrigger reports whether this session holds a trigger
// bound to attachName's table (any event or timing). execRoutedToAttached
// (attach_write.go) uses it to refuse delegating a write or DDL it could not
// fire or cascade correctly through; the attachment's own trigger cascade runs
// in a separate *DB session (build.c:3387-3411 sqlite3CodeDropTable, alter.c's
// rename cascade).
func (db *DB) tableHasAnyAttachedTrigger(attachName, tableName string) bool {
	for _, tr := range db.triggers {
		if tr.tableAttachName != "" && equalFoldName(tr.tableAttachName, attachName) && equalFoldName(tr.table, tableName) {
			return true
		}
	}
	return false
}

// attachedTriggersFor returns every trigger this session bound to
// attachName's own copy of tableName (tableAttachName == attachName, table
// == tableName), any event/timing -- tableHasAnyAttachedTrigger's collecting
// twin. Used by attach_write.go's routeAttachedStatement to check each
// candidate's attachedTriggerMirrorSafe individually before deciding whether
// a routed DML statement against tableName may proceed.
func (db *DB) attachedTriggersFor(attachName, tableName string) []*triggerMeta {
	var out []*triggerMeta
	for _, tr := range db.triggers {
		if tr.tableAttachName != "" && equalFoldName(tr.tableAttachName, attachName) && equalFoldName(tr.table, tableName) {
			out = append(out, tr)
		}
	}
	return out
}

// attachedMirrorSafeTriggers returns every trigger this session bound to
// attachName (any target table) that attachedTriggerMirrorSafe clears for
// firing by MIRRORING it onto attachName's own delegated write session --
// what SetForeignTempTriggers installs there after every routed write
// (attach_write.go's routeAttachedStatement), so that session's own LOCAL
// trigger machinery can fire them as if they were genuinely its own.
func (db *DB) attachedMirrorSafeTriggers(attachName string) []*triggerMeta {
	var out []*triggerMeta
	for _, tr := range db.triggers {
		if tr.tableAttachName != "" && equalFoldName(tr.tableAttachName, attachName) && db.attachedTriggerMirrorSafe(tr, attachName) {
			out = append(out, tr)
		}
	}
	return out
}

// SetForeignTempTriggers installs trs as this session's foreignTempTriggers.
// routeAttachedStatement (attach_write.go) calls it on every routed write,
// never caching it, so a CREATE/DROP TRIGGER in between is seen by the next.
func (db *DB) SetForeignTempTriggers(trs []*triggerMeta) { db.foreignTempTriggers = trs }

// attachedTriggerMirrorSafe reports whether tr, a TEMP trigger bound to
// attachName's table, can be fired by mirroring it onto attachName's delegated
// write session (SetForeignTempTriggers) and letting that session's ordinary
// trigger machinery run it.
//
// C fires the body in the originating connection's full search order (TEMP,
// MAIN, then attachments in order: sqlite3FindTable, build.c:373-385).
// Mirroring resolves every name inside attachName, which is correct only when
// that search would find the same objects; resolveUnqualifiedAcrossAttached
// (attach_write.go) is that search. E.g. trigger1.test 10.9-10.10: once
// insert_log moves from main into aux, the trigger on aux.t4 becomes
// mirror-safe. Checked on every routed write.
//
// Refused outright (attachedTriggerBodyTargets): a body with a SELECT source
// or any subquery (collectSelectTables cannot enumerate every reachable
// name), and BEFORE/INSTEAD OF triggers (no pre-store hooks here).
func (db *DB) attachedTriggerMirrorSafe(tr *triggerMeta, attachName string) bool {
	tables, ok := attachedTriggerBodyTargets(tr)
	if !ok {
		return false
	}
	kinds := []string{"table", "view"}
	for _, table := range tables {
		winner, ok := db.resolveUnqualifiedAcrossAttached(table, kinds)
		if !ok || !equalFoldName(winner, attachName) {
			return false
		}
	}
	return true
}

// attachedTriggerOriginatingSafe is the opposite case: tr, a TEMP trigger on
// an attached table, can be fired by this session itself
// (compileTriggerFirePlanFor + firePlanForRow) given the row the delegated
// write produced, when every table its body reaches resolves to this session's
// local catalog. trigger1.test 10.3-10.8 (trig3 on aux.t4 writing
// main.insert_log) is that shape. The two predicates are mutually exclusive
// (see routeAttachedStatement) and share attachedTriggerBodyTargets'
// preconditions.
func (db *DB) attachedTriggerOriginatingSafe(tr *triggerMeta) bool {
	tables, ok := attachedTriggerBodyTargets(tr)
	if !ok {
		return false
	}
	kinds := []string{"table", "view"}
	for _, table := range tables {
		if _, ok := db.resolveUnqualifiedAcrossAttached(table, kinds); ok {
			return false // resolves to SOME attachment, not this session's own catalog
		}
	}
	return true
}

// attachedTriggerBodyTargets lists the table each of tr's body statements
// targets, or ok=false when the body is not analyzable: BEFORE or INSTEAD OF,
// a SELECT source or bare SELECT step, or a subquery in WHEN or a body
// expression. Shared by both attached-trigger predicates.
func attachedTriggerBodyTargets(tr *triggerMeta) ([]string, bool) {
	if tr.timing != triggerAfter || tr.insteadOf {
		return nil, false
	}
	if containsSubquery(tr.when) {
		return nil, false
	}
	var tables []string
	for _, bs := range tr.body {
		var table string
		var exprs []Expr
		switch {
		case bs.insert != nil:
			if bs.insert.selectStmt != nil {
				return nil, false // INSERT ... SELECT: source tables not enumerable here
			}
			table = bs.insert.table
			for _, row := range bs.insert.rows {
				exprs = append(exprs, row...)
			}
		case bs.update != nil:
			table = bs.update.table
			exprs = append(exprs, bs.update.where)
			for _, a := range bs.update.sets {
				exprs = append(exprs, a.expr)
			}
		case bs.delete != nil:
			table = bs.delete.table
			exprs = append(exprs, bs.delete.where)
		default:
			return nil, false // a bare-SELECT body step: same reason as INSERT...SELECT
		}
		for _, e := range exprs {
			if containsSubquery(e) {
				return nil, false
			}
		}
		tables = append(tables, table)
	}
	return tables, true
}

// filterUpdateTriggers keeps only the triggers in trs whose "OF col-list" (if
// any) overlaps setCols -- see this file's package doc comment: UPDATE OF
// fires whenever the named column is included in the UPDATE's SET clause,
// regardless of whether its value actually changes. A trigger with no OF
// list (updateCols == nil) always passes.
func filterUpdateTriggers(trs []*triggerMeta, setCols []string) []*triggerMeta {
	var out []*triggerMeta
	for _, tr := range trs {
		if tr.updateCols == nil {
			out = append(out, tr)
			continue
		}
		for _, oc := range tr.updateCols {
			matched := false
			for _, sc := range setCols {
				if equalFoldName(oc, sc) {
					matched = true
					break
				}
			}
			if matched {
				out = append(out, tr)
				break
			}
		}
	}
	return out
}

// ---- parsing ----

type parsedCreateTrigger struct {
	// isTemp puts this trigger in the TEMP catalog (a spelled TEMP keyword or
	// a "temp." qualifier on its NAME). nameScope is the qualifier written on
	// the NAME and tableScope the one written on the ON clause (both scopeAny
	// when absent) -- see triggerTargetScope, which combines them into the
	// catalog the target table resolves in. A trigger on a TEMP table is itself
	// temp whatever it was spelled -- see CreateTrigger and temp_schema.go.
	isTemp     bool
	nameScope  schemaScope
	tableScope schemaScope
	name       string
	ifNotExi   bool
	insteadOf  bool
	timing     triggerTiming
	event      triggerEventKind
	updateCols []string
	table      string
	when       Expr
	body       []triggerBodyStmt

	// tableSchemaText is the ON-clause's own qualifier when scopeOfQualifier
	// could not classify it as ""/main/temp -- i.e. it names, or claims to
	// name, an ATTACHed database. tableScope stays scopeAny in that case (it
	// is NOT "unqualified": CreateTrigger's resolveCreateTriggerAttachedTarget
	// must see this and skip the ordinary local temp-first/main lookup
	// entirely, or a same-named LOCAL table would silently steal the trigger
	// -- see that function's doc comment).
	tableSchemaText string

	// unsafeRaiseIgnore is set when a body INSERT/UPDATE/DELETE statement
	// embedded a RAISE(IGNORE) whose unwind scope this engine cannot match
	// (see parser.raiseIgnoreDeclined); CreateTrigger declines such a trigger.
	unsafeRaiseIgnore bool
}

// isCreateTriggerStmt reports whether toks (a lexed statement already known
// to start with CREATE) is a CREATE [TEMP|TEMPORARY] TRIGGER, as opposed to
// CREATE TABLE/[UNIQUE] INDEX/VIEW.
func isCreateTriggerStmt(toks []token) bool {
	i := 1
	if i < len(toks) && toks[i].kind == tkIdent && (toks[i].upper() == "TEMP" || toks[i].upper() == "TEMPORARY") {
		i++
	}
	return i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "TRIGGER"
}

// parseCreateTriggerStmt parses "CREATE [TEMP|TEMPORARY] TRIGGER [IF NOT
// EXISTS] [schema.]name [BEFORE|AFTER|INSTEAD OF] {DELETE|INSERT|UPDATE [OF
// col[,col...]]} ON [schema.]table [FOR EACH ROW] [WHEN expr] BEGIN
// <stmt>; [<stmt>;]... END". This is the SAME parser CreateTrigger (write
// path) and OpenWrite's reload both use, so a trigger's in-session
// definition and its materialized-then-reread definition can never disagree
// (mirrors view.go's parseCreateViewStmt).
func parseCreateTriggerStmt(sqlText string) (*parsedCreateTrigger, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	if !p.consumeKeyword("CREATE") {
		return nil, fmt.Errorf("engine: CREATE TRIGGER: expected CREATE, got %q", p.tokenDesc(p.peek()))
	}
	isTemp := p.consumeKeyword("TEMP") || p.consumeKeyword("TEMPORARY")
	if !p.consumeKeyword("TRIGGER") {
		return nil, fmt.Errorf("engine: CREATE TRIGGER: expected TRIGGER, got %q", p.tokenDesc(p.peek()))
	}
	stmt := &parsedCreateTrigger{}
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("NOT") || !p.consumeKeyword("EXISTS") {
			return nil, fmt.Errorf("engine: CREATE TRIGGER: expected NOT EXISTS after IF")
		}
		stmt.ifNotExi = true
	}

	// objectNameToken, not a bare tkIdent test: every name position in SQLite's
	// grammar takes a single-quoted STRING as the name too (its "nm" production
	// -- see that function's doc comment). Verified for CREATE TRIGGER's own
	// three positions against 3.53.3: "CREATE TRIGGER 'trig4' AFTER INSERT ON
	// 't8' BEGIN ... END" and "CREATE TRIGGER trig3 AFTER INSERT ON main.'t8'
	// BEGIN ... END" are both accepted, and the trigger really fires on a write
	// to t8 (alter.test 2/3).
	t := p.peek()
	tName, tOK := objectNameToken(t)
	if !tOK {
		return nil, fmt.Errorf("engine: CREATE TRIGGER: expected trigger name, got %q", p.tokenDesc(t))
	}
	p.next()
	stmt.name = tName
	if p.peekIsPunct(".") {
		p.next()
		t2 := p.peek()
		t2Name, t2OK := objectNameToken(t2)
		if !t2OK {
			return nil, fmt.Errorf("engine: CREATE TRIGGER: expected trigger name after schema qualifier")
		}
		p.next()
		// A TEMP trigger's name may not be schema-qualified at all -- not
		// even "main." or "temp." Verified directly against C SQLite:
		// "CREATE TEMP TRIGGER main.r1 ..." is "temporary trigger may not
		// have qualified name", while the same statement without TEMP is
		// fine and "CREATE TEMP TRIGGER r1 ..." is fine (trigger7.test).
		if isTemp {
			return nil, fmt.Errorf("engine: temporary trigger may not have qualified name")
		}
		scope, ok := scopeOfQualifier(tName)
		if !ok {
			return nil, fmt.Errorf("engine: CREATE TRIGGER: unknown database %s", tName)
		}
		isTemp = scope == scopeTemp
		stmt.nameScope = scope
		stmt.name = t2Name
	}
	stmt.isTemp = isTemp

	timingSet := false
	switch {
	case p.consumeKeyword("BEFORE"):
		stmt.timing = triggerBefore
		timingSet = true
	case p.consumeKeyword("AFTER"):
		stmt.timing = triggerAfter
		timingSet = true
	case p.peekIsKeyword("INSTEAD"):
		p.next()
		if !p.consumeKeyword("OF") {
			return nil, fmt.Errorf("engine: CREATE TRIGGER: expected OF after INSTEAD")
		}
		stmt.insteadOf = true
	}
	if !timingSet && !stmt.insteadOf {
		// Verified directly against C SQLite: omitting BEFORE/AFTER/INSTEAD
		// OF entirely fires the trigger as BEFORE -- see this file's package
		// doc comment.
		stmt.timing = triggerBefore
	}

	switch {
	case p.consumeKeyword("DELETE"):
		stmt.event = triggerDelete
	case p.consumeKeyword("INSERT"):
		stmt.event = triggerInsert
	case p.consumeKeyword("UPDATE"):
		stmt.event = triggerUpdate
		if p.consumeKeyword("OF") {
			for {
				ct := p.peek()
				if ct.kind != tkIdent {
					return nil, fmt.Errorf("engine: CREATE TRIGGER: expected column name after OF, got %q", p.tokenDesc(ct))
				}
				p.next()
				stmt.updateCols = append(stmt.updateCols, ct.text)
				if p.peekIsPunct(",") {
					p.next()
					continue
				}
				break
			}
		}
	default:
		return nil, fmt.Errorf("engine: CREATE TRIGGER: expected DELETE, INSERT, or UPDATE, got %q", p.tokenDesc(p.peek()))
	}

	if !p.consumeKeyword("ON") {
		return nil, fmt.Errorf("engine: CREATE TRIGGER: expected ON, got %q", p.tokenDesc(p.peek()))
	}
	tt := p.peek()
	ttName, ttOK := objectNameToken(tt)
	if !ttOK {
		return nil, fmt.Errorf("engine: CREATE TRIGGER: expected table name, got %q", p.tokenDesc(tt))
	}
	p.next()
	stmt.table = ttName
	if p.peekIsPunct(".") {
		p.next()
		t2 := p.peek()
		t2Name, t2OK := objectNameToken(t2)
		if !t2OK {
			return nil, fmt.Errorf("engine: CREATE TRIGGER: expected table name after schema qualifier")
		}
		p.next()
		scope, ok := scopeOfQualifier(ttName)
		if ok {
			stmt.tableScope = scope
		} else {
			// An unknown qualifier may name an ATTACHed database: C resolves a
			// schema-qualified ON table via sqlite3SrcListLookup before deciding
			// whether the trigger may reference it (trigger.c:167, :176-180).
			// That needs db.attached, so defer to CreateTrigger
			// (resolveCreateTriggerAttachedTarget).
			stmt.tableSchemaText = ttName
		}
		stmt.table = t2Name
	}
	// The ON-clause table name may be a STRING literal like any "nm"
	// (parse.y:340): "CREATE TRIGGER trig3 AFTER INSERT ON main.'t8' ...".
	// RENAME splices that token too (locateTriggerOnTableToken), as
	// renameTableFunc does (alter.c:1852).

	if p.consumeKeyword("FOR") {
		if !p.consumeKeyword("EACH") || !p.consumeKeyword("ROW") {
			return nil, fmt.Errorf("engine: CREATE TRIGGER: expected EACH ROW after FOR")
		}
	}

	// From here on (WHEN clause + BEGIN...END body) a "RAISE(...)" expression
	// is legal -- see parser.inTriggerBody. This *parser parses only this one
	// CREATE TRIGGER statement, so the flag never needs restoring.
	p.inTriggerBody = true

	if p.consumeKeyword("WHEN") {
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.when = expr
	}

	if !p.consumeKeyword("BEGIN") {
		return nil, fmt.Errorf("engine: CREATE TRIGGER: expected BEGIN, got %q", p.tokenDesc(p.peek()))
	}
	body, err := parseTriggerBody(p, stmt.event)
	if err != nil {
		return nil, err
	}
	stmt.body = body
	stmt.unsafeRaiseIgnore = p.sawUnsafeRaiseIgnore

	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: CREATE TRIGGER: unexpected trailing input near %q (multi-statement Exec is not supported by this write path)", p.tokenDesc(p.peek()))
	}
	return stmt, nil
}

// parseTriggerBody parses the "<stmt>; [<stmt>;]... END" tail of a trigger body
// (BEGIN already consumed), reusing the top-level statement parsers on the same
// token stream; each stops at its own statement end.
//
// A bare SELECT/WITH body and an INSERT ... SELECT body are gated by event
// (see the package doc); outside the gate they decline as unsupported.
// errTriggerReturning is C's parse-time rejection of RETURNING in a trigger.
var errTriggerReturning = fmt.Errorf("engine: CREATE TRIGGER: RETURNING is not allowed in a trigger body")

func parseTriggerBody(p *parser, event triggerEventKind) ([]triggerBodyStmt, error) {
	eventGate := func(what string) error {
		if event != triggerInsert {
			return fmt.Errorf("engine: CREATE TRIGGER: %s in a non-INSERT trigger body is not supported by this write path", what)
		}
		return nil
	}
	var body []triggerBodyStmt
	for {
		if p.consumeKeyword("END") {
			break
		}
		var bs triggerBodyStmt
		switch {
		case p.peekIsKeyword("INSERT"), p.peekIsKeyword("REPLACE"):
			// A RAISE(IGNORE) buried in a body INSERT/UPDATE/DELETE has an
			// unwind scope this engine cannot match (see raiseIgnoreDeclined);
			// flag it so CreateTrigger declines the whole trigger cleanly.
			p.raiseIgnoreDeclined = true
			st, err := parseInsertStmtTokens(p, true)
			p.raiseIgnoreDeclined = false
			if err != nil {
				return nil, err
			}
			if st.selectStmt != nil {
				// An INSERT's SOURCE select is held to the same rule a BARE
				// body SELECT is (below): outside an INSERT trigger it must be
				// a shape the eager pass can fully validate, so a body
				// reference C SQLite rejects at prepare time cannot slip
				// through a firing that never happens. It used to be declined
				// outright for every non-INSERT trigger, which also refused the
				// simple shapes the bare form already accepted.
				if event != triggerInsert &&
					!isTrivialBodySelect(st.selectStmt) &&
					!isSimpleValidatableInsertSource(st.selectStmt) {
					if err := eventGate("INSERT ... SELECT of this shape"); err != nil {
						return nil, err
					}
				}
			}
			if st.returning != nil {
				return nil, errTriggerReturning
			}
			bs.insert = st
		case p.peekIsKeyword("UPDATE"):
			p.raiseIgnoreDeclined = true
			st, err := parseUpdateStmtTokens(p)
			p.raiseIgnoreDeclined = false
			if err != nil {
				return nil, err
			}
			if st.returning != nil {
				return nil, errTriggerReturning
			}
			bs.update = st
		case p.peekIsKeyword("DELETE"):
			p.raiseIgnoreDeclined = true
			st, err := parseDeleteStmtTokens(p)
			p.raiseIgnoreDeclined = false
			if err != nil {
				return nil, err
			}
			if st.returning != nil {
				return nil, errTriggerReturning
			}
			bs.delete = st
		case p.peekIsKeyword("SELECT"), p.peekIsKeyword("WITH"), p.peekIsKeyword("VALUES"):
			// A bare VALUES is a legal body statement (returning1.test 13). A
			// single tuple parses as a FROM-less select core, so it gets the same
			// eager check as a FROM-less SELECT; a multi-tuple VALUES is a compound
			// and stays declined outside an INSERT trigger.
			sel, err := p.parseSelectStmt()
			if err != nil {
				return nil, err
			}
			// A bare SELECT body runs and discards its result at fire time. Any
			// shape is accepted for an INSERT trigger. For UPDATE/DELETE (which may
			// fire zero times) only shapes validated eagerly at firing time are
			// accepted: a FROM-less SELECT, or a simple FROM-bearing one
			// (isSimpleValidatableBodySelect / validateSimpleBodySelectEager /
			// isSchemaSafeDerivedSubquery). Anything more complex could accept a
			// reference C rejects at prepare time, so it stays declined.
			bs.sel = sel
		default:
			return nil, fmt.Errorf("engine: CREATE TRIGGER: expected INSERT, UPDATE, DELETE, or END, got %q", p.tokenDesc(p.peek()))
		}
		if err := p.expectPunct(";"); err != nil {
			return nil, err
		}
		body = append(body, bs)
	}
	return body, nil
}

// CreateTrigger parses and registers a single "CREATE TRIGGER ..." statement.
// INSTEAD OF on a table and BEFORE/AFTER on a view are rejected with C's
// wording. The target must already exist at CREATE time.
func (db *DB) CreateTrigger(sqlText string) error {
	stmt, err := parseCreateTriggerStmt(sqlText)
	if err != nil {
		return err
	}

	// viewName is set when the trigger targets a view: INSTEAD OF is supported
	// there (view_trigger.go); BEFORE/AFTER is rejected with C's wording.
	//
	// The local lookup is skipped when the ON clause carried an unrecognized
	// qualifier (stmt.tableSchemaText != ""), else a same-named local object
	// would steal a trigger that names an attached database (trigger1.test
	// 10.x).
	targetScope := triggerTargetScope(stmt.nameScope, stmt.tableScope)
	var tbl *tableMeta
	var vm *viewMeta
	if stmt.tableSchemaText == "" {
		tbl = db.findTableMetaIn(targetScope, stmt.table)
		if tbl == nil {
			vm = db.findViewMetaIn(targetScope, stmt.table)
		}
	}
	var viewName string
	var targetIsTemp bool
	var targetAttachName string
	switch {
	case tbl != nil:
		if stmt.insteadOf {
			return fmt.Errorf("engine: cannot create INSTEAD OF trigger on table: %s", stmt.table)
		}
		targetIsTemp = tbl.isTemp
	case vm != nil:
		if !stmt.insteadOf {
			timingWord := "BEFORE"
			if stmt.timing == triggerAfter {
				timingWord = "AFTER"
			}
			return fmt.Errorf("engine: cannot create %s trigger on view: %s", timingWord, stmt.table)
		}
		viewName = vm.name
		targetIsTemp = vm.isTemp
	default:
		ad, aerr := db.resolveCreateTriggerAttachedTarget(stmt)
		if aerr != nil {
			return aerr
		}
		targetAttachName = ad.name
	}
	// A trigger on a TEMP object belongs to the temp catalog whether or not it
	// was spelled TEMP -- verified directly: "CREATE TEMP TABLE q(a); CREATE
	// TRIGGER trm AFTER INSERT ON q ..." lands trm in sqlite_temp_master. See
	// temp_schema.go. An ATTACHment-bound target never widens this: only a
	// TEMP trigger can reach one at all (resolveCreateTriggerAttachedTarget),
	// so isTemp is already true whenever targetAttachName is set.
	isTemp := stmt.isTemp || targetIsTemp

	// A body INSERT/UPDATE/DELETE target may carry a schema qualifier only in a
	// trigger that lives in TEMP (triggerStepAllocate, trigger.c:478-485, tests
	// pNew->pSchema) -- which a trigger ON a temp table does even without the
	// TEMP keyword (sqlite3BeginTrigger, trigger.c:162-171): e_update.test's
	// "CREATE TRIGGER tr1 AFTER DELETE ON t4 BEGIN UPDATE main.t1 ... END"
	// over a temp t4. A qualifier in a body SELECT's FROM is always fine.
	if !isTemp {
		for _, bs := range stmt.body {
			if bs.insert != nil && bs.insert.schema != "" || bs.update != nil && bs.update.schema != "" ||
				bs.delete != nil && bs.delete.schema != "" {
				return fmt.Errorf("engine: qualified table names are not allowed on INSERT, UPDATE, and DELETE statements within triggers")
			}
		}
	}

	if err := db.checkReservedObjectName(stmt.name); err != nil {
		return err
	}
	// A trigger body may not reference another database -- C SQLite:
	// "trigger r5 cannot reference objects in database orig" (attach.test).
	// Same rule (and the same walker) as CreateView's own check; a body DML
	// target carrying ANY qualifier is already rejected outright, so this
	// covers the body SELECTs, where a "main." qualifier is legal. A trigger
	// that lives in TEMP is not checked at all: the fixer runs with bTemp set
	// for iDb==1 and skips every source (attach.c:495, :547), so a temp
	// trigger's body may read "aux.a1" (temptrigger.test).
	local := localSchemaOr(db.localSchema)
	foreign := func(schema string) error {
		if schema == "" || isTemp {
			return nil
		}
		return fmt.Errorf("engine: trigger %s cannot reference objects in database %s", stmt.name, schema)
	}
	if err := foreign(exprForeignSchema(stmt.when, local, isTemp)); err != nil {
		return err
	}
	for _, bs := range stmt.body {
		// EVERY expression a body statement carries can reach another database
		// through a subquery, not just a source SELECT: a body INSERT's VALUES
		// tuple and a body DELETE/UPDATE's WHERE are rejected the same way
		// (attach.test 5.8/5.9, verified directly).
		var exprs []Expr
		var sel *SelectStmt
		switch {
		case bs.sel != nil:
			sel = bs.sel
		case bs.insert != nil:
			sel = bs.insert.selectStmt
			for _, row := range bs.insert.rows {
				exprs = append(exprs, row...)
			}
		case bs.update != nil:
			exprs = append(exprs, bs.update.where)
			for _, a := range bs.update.sets {
				exprs = append(exprs, a.expr)
			}
		case bs.delete != nil:
			exprs = append(exprs, bs.delete.where)
		}
		if err := foreign(selectForeignSchema(sel, local, isTemp)); err != nil {
			return err
		}
		for _, e := range exprs {
			if err := foreign(exprForeignSchema(e, local, isTemp)); err != nil {
				return err
			}
		}
	}
	if existing := db.findTriggerMetaIn(createScope(isTemp), stmt.name); existing != nil {
		if stmt.ifNotExi {
			return nil
		}
		return fmt.Errorf("engine: trigger %s already exists", stmt.name)
	}

	if stmt.unsafeRaiseIgnore {
		// A RAISE(IGNORE) embedded in a body INSERT/UPDATE/DELETE (as opposed
		// to a WHEN clause or a bare-SELECT body step) has an unwind scope this
		// engine does not reproduce -- declined cleanly rather than risked
		// wrong. See parser.raiseIgnoreDeclined.
		return fmt.Errorf("engine: CREATE TRIGGER: RAISE(IGNORE) inside a body INSERT/UPDATE/DELETE statement is not supported by this write path")
	}

	// The registered target name is the base table's canonical name, the
	// view's canonical name (for an INSTEAD OF trigger), or -- for an
	// ATTACHment-bound target, which has no LOCAL tableMeta/viewMeta to take
	// a canonical name from -- the bare name as written on the ON-clause
	// (comparisons against it are always case-insensitive, so this is enough
	// to match by).
	targetName := stmt.table
	switch {
	case tbl != nil:
		targetName = tbl.name
	case viewName != "":
		targetName = viewName
	}
	db.triggers = append(db.triggers, &triggerMeta{
		schemaSeq:       db.nextSchemaSeq(isTemp),
		isTemp:          isTemp,
		tableIsTemp:     targetIsTemp,
		tableAttachName: targetAttachName,
		name:            stmt.name,
		sql:             storedSchemaSQL("TRIGGER", sqlText, isTemp),
		table:           targetName,
		timing:          stmt.timing,
		event:           stmt.event,
		insteadOf:       stmt.insteadOf,
		updateCols:      stmt.updateCols,
		when:            stmt.when,
		body:            stmt.body,
	})
	db.bumpSchema(isTemp)
	return nil
}

// resolveCreateTriggerAttachedTarget is CreateTrigger's fallback once the ON
// table matched no local table or view; it re-resolves db.attachedNamed every
// time.
//
//   - An explicit unrecognized qualifier is checked against db.attached. C
//     resolves it first (trigger.c:167), then pins a non-TEMP trigger's ON
//     clause to its own database (sqlite3FixSrcList, trigger.c:176-180),
//     rejecting anything else with "trigger %s cannot reference objects in
//     database %s" (attach.c:498-501). A TEMP trigger skips the pin
//     (attach.c:533-555). "CREATE TRIGGER aux.trig ... ON aux.t" never gets
//     here: routeAttachedStatement delegates it to aux's session.
//   - An unqualified ON table can reach an attachment only for a TEMP trigger,
//     via the full TEMP/MAIN/ATTACHED search (build.c:373-385).
func (db *DB) resolveCreateTriggerAttachedTarget(stmt *parsedCreateTrigger) (*attachedDB, error) {
	if stmt.tableSchemaText != "" {
		ad := db.attachedNamed(stmt.tableSchemaText)
		if ad == nil {
			return nil, fmt.Errorf("engine: CREATE TRIGGER: unknown database %s", stmt.tableSchemaText)
		}
		if !stmt.isTemp {
			return nil, fmt.Errorf("engine: trigger %s cannot reference objects in database %s", stmt.name, stmt.tableSchemaText)
		}
		if !db.attachedHasTable(ad, stmt.table) {
			return nil, fmt.Errorf("engine: no such table: %s.%s", stmt.tableSchemaText, stmt.table)
		}
		return ad, nil
	}
	if stmt.isTemp {
		if ad := db.firstAttachedTable(stmt.table); ad != nil {
			return ad, nil
		}
	}
	return nil, fmt.Errorf("engine: no such table: main.%s", stmt.table)
}

// removeTrigger removes tr from db.triggers, mirroring removeView (view.go)
// and removeTableAndIndexes (drop_write.go).
func (db *DB) removeTrigger(tr *triggerMeta) {
	for i, x := range db.triggers {
		if x == tr {
			db.triggers = append(db.triggers[:i], db.triggers[i+1:]...)
			break
		}
	}
	db.bumpSchema(tr.isTemp)
}

// DropTrigger parses and executes "DROP TRIGGER [IF EXISTS] [schema.]name",
// mirroring DropView/DropTable (drop_write.go): dropQualifiedName/
// expectDropTrailer are the SAME shared helpers those use.
func (db *DB) DropTrigger(sqlText string) error {
	toks, err := lex(sqlText)
	if err != nil {
		return err
	}
	p := newParser(sqlText, toks)
	errPrefix := "engine: DROP TRIGGER"
	if !p.consumeKeyword("DROP") {
		return fmt.Errorf("%s: expected DROP, got %q", errPrefix, p.tokenDesc(p.peek()))
	}
	if !p.consumeKeyword("TRIGGER") {
		return fmt.Errorf("%s: expected TRIGGER, got %q", errPrefix, p.tokenDesc(p.peek()))
	}
	ifExists := false
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("EXISTS") {
			return fmt.Errorf("%s: expected EXISTS after IF", errPrefix)
		}
		ifExists = true
	}
	bare, display, scope, unknownSchema, nerr := dropQualifiedName(p, errPrefix)
	if nerr != nil {
		return nerr
	}
	if terr := expectDropTrailer(p, errPrefix); terr != nil {
		return terr
	}

	if aerr := db.checkDropSchemaQualifier(display, unknownSchema); aerr != nil {
		return aerr
	}
	if !unknownSchema {
		if tr := db.findTriggerMetaIn(scope, bare); tr != nil {
			db.removeTrigger(tr)
			return nil
		}
	}
	if ifExists {
		return nil
	}
	return fmt.Errorf("engine: no such trigger: %s", display)
}

// ---- OLD/NEW pseudo-table scopes ----

// triggerOldNewScopes builds the "old"/"new" scopes a trigger's WHEN and body
// resolve OLD./NEW. against. Both always exist; the side event lacks (NEW for
// DELETE, OLD for INSERT) is empty, so a reference fails with C's "no such
// column: OLD.x" rather than "no such table". Both set unqualifiedHidden: an
// unqualified reference never resolves against OLD/NEW.
func triggerOldNewScopes(tbl *tableMeta, event triggerEventKind) (oldTS, newTS tableScope) {
	hasOld := event == triggerUpdate || event == triggerDelete
	hasNew := event == triggerUpdate || event == triggerInsert

	if hasOld {
		oldTS = tableScope{name: "old", cols: tbl.cols, colIndex: buildColIndex(tbl.cols), offset: 0, unqualifiedHidden: true}
	} else {
		oldTS = tableScope{name: "old", colIndex: map[string]int{}, noRowid: true, unqualifiedHidden: true}
	}
	newOffset := 0
	if hasOld {
		newOffset = len(tbl.cols)
	}
	if hasNew {
		newTS = tableScope{name: "new", cols: tbl.cols, colIndex: buildColIndex(tbl.cols), offset: newOffset, unqualifiedHidden: true}
	} else {
		newTS = tableScope{name: "new", colIndex: map[string]int{}, noRowid: true, unqualifiedHidden: true}
	}
	return oldTS, newTS
}

// triggerSchemaCtx builds a SCHEMA-ONLY (no row values) old/new evalCtx for
// event against tbl -- used by validateTriggerExprsOnce to catch a bad
// column reference once per firing statement, before any row is processed
// (see that function's doc comment for why this must run eagerly).
func triggerSchemaCtx(tbl *tableMeta, event triggerEventKind) *evalCtx {
	oldTS, newTS := triggerOldNewScopes(tbl, event)
	return &evalCtx{tables: []tableScope{oldTS, newTS}}
}

// ---- eager (zero-row-safe) validation ----

// validateTriggerExprsOnce runs validateColumnRefs (query.go) over every WHEN
// and body expression of trs against tbl's schema-only OLD/NEW scopes, once per
// firing statement and before any row, because C reports these errors even
// when the statement matches zero rows.
func (db *DB) validateTriggerExprsOnce(tbl *tableMeta, event triggerEventKind, trs []*triggerMeta) error {
	if err := db.checkTriggerTempScope(trs); err != nil {
		return err
	}
	if err := db.checkTriggerBodyTables(trs); err != nil {
		return err
	}
	schemaOuter := triggerSchemaCtx(tbl, event)
	for _, tr := range trs {
		if tr.when != nil {
			if err := validateColumnRefs(tr.when, schemaOuter); err != nil {
				return err
			}
			// ...and the WHEN clause's own SUBQUERIES, which validateColumnRefs
			// does not descend into. renameResolveTrigger resolves pWhen like
			// every other trigger expression (alter.c), so "WHEN EXISTS(SELECT 1
			// FROM t1 WHERE b IS NOT NULL)" after "ALTER TABLE t1 DROP COLUMN b"
			// is "error in trigger tr after drop column: no such column: b" on
			// 3.53.3 -- and this engine performed the drop, for EVERY body shape,
			// because the gate was the WHEN clause and not the body at all.
			if err := db.r34vValidateExprScopes(tr.when, schemaOuter); err != nil {
				return err
			}
			if err := declineTriggerWhenSubqueryCollation(tbl, tr); err != nil {
				return err
			}
		}
		for _, bs := range tr.body {
			if err := db.validateTriggerBodyStmtExprsOnce(bs, schemaOuter); err != nil {
				return err
			}
			if bs.sel != nil {
				if err := db.validateTrivialBodySelectExprs(bs.sel, schemaOuter); err != nil {
					return err
				}
				if !isTrivialBodySelect(bs.sel) && isSimpleValidatableBodySelect(bs.sel) {
					if err := db.validateSimpleBodySelectEager(bs.sel, schemaOuter, false); err != nil {
						return err
					}
				}
			}
			// ...and an INSERT ... SELECT body statement's SOURCE, by the same
			// rule: that is what lets the shape be accepted outside an INSERT
			// trigger at all, since the error still lands on the CREATE TRIGGER
			// rather than on a firing that may never come.
			if bs.insert != nil && bs.insert.selectStmt != nil {
				if err := db.validateTrivialBodySelectExprs(bs.insert.selectStmt, schemaOuter); err != nil {
					return err
				}
				// orderBy=true: an INSERT's source runs with its ORDER BY,
				// so C resolves it even for a zero-row firing ("...
				// SELECT b FROM y2 ORDER BY nosuchcol" in an AFTER DELETE trigger
				// errors on a DELETE from an empty table).
				if !isTrivialBodySelect(bs.insert.selectStmt) && isSimpleValidatableInsertSource(bs.insert.selectStmt) {
					if err := db.validateSimpleBodySelectEager(bs.insert.selectStmt, schemaOuter, true); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// isTrivialBodySelect reports whether sel is a "SELECT <exprs> [WHERE <cond>]"
// with no FROM/JOIN, compound arm, CTE, GROUP BY/HAVING, ORDER BY, window, or
// LIMIT -- the only SELECT shape whose expressions reference nothing but
// OLD/NEW (and constants/parameters) and so can be validated EAGERLY against
// the OLD/NEW schema scope to reproduce C SQLite's prepare-time compile
// error even for a zero-row firing. Anything more complex is only accepted as
// an INSERT-trigger body (where the body always fires) and validated lazily.
func isTrivialBodySelect(sel *SelectStmt) bool {
	return len(sel.From) == 0 &&
		len(sel.Compound) == 0 &&
		len(sel.CTEs) == 0 &&
		len(sel.GroupBy) == 0 &&
		sel.Having == nil &&
		len(sel.OrderBy) == 0 &&
		len(sel.Windows) == 0 &&
		sel.Limit == nil && sel.LimitParam == nil
}

// validateTrivialBodySelectExprs eagerly validates a FROM-less body SELECT's
// result and WHERE expressions against the OLD/NEW scope, so "SELECT nosuchcol"
// errors even for a zero-row firing, as C's prepare-time compile does.
// It is a *DB method because a FROM-less SELECT may still contain a subquery
// ("SELECT 1, (SELECT max(b) FROM t1)"), which sqlite3SelectPrep resolves; DROP
// COLUMN b must then fail with "error in trigger tr after drop column".
func (db *DB) validateTrivialBodySelectExprs(sel *SelectStmt, outer *evalCtx) error {
	if !isTrivialBodySelect(sel) {
		return nil
	}
	for _, c := range sel.Columns {
		if c.Star {
			continue
		}
		if err := validateColumnRefs(c.Expr, outer); err != nil {
			return err
		}
		if err := db.r34vValidateExprScopes(c.Expr, outer); err != nil {
			return err
		}
	}
	if sel.Where != nil {
		if err := validateColumnRefs(sel.Where, outer); err != nil {
			return err
		}
		if err := db.r34vValidateExprScopes(sel.Where, outer); err != nil {
			return err
		}
	}
	return nil
}

// isSimpleValidatableBodySelect reports whether sel is a FROM-bearing body
// SELECT that validateSimpleBodySelectEager can fully validate, so its
// prepare-time errors surface even for a zero-row firing.
//
// Accepted: a flat SELECT with FROM, optional WHERE, ORDER BY, GROUP BY/HAVING,
// and compound arms that are themselves this shape; no CTE, window frame,
// DISTINCT or LIMIT/OFFSET. FROM items are named tables/views or a derived
// table narrow enough for isSchemaSafeDerivedSubquery. Nested subqueries must
// pass r34vExprValidatable.
//
// GROUP BY, HAVING and compound arms are in the shape because C name-resolves
// them even when the result is discarded, and r33rTriggersResolveAfter
// (alter_write.go) relies on this validator for DROP COLUMN:
//
//	BEGIN SELECT x FROM t1 GROUP BY nosuchcol; END
//	BEGIN SELECT x FROM t1 GROUP BY x HAVING nosuchcol>0; END
//	BEGIN SELECT x FROM t1 UNION ALL SELECT nosuchcol FROM t1; END
//
// all raise on a zero-row UPDATE. A leading FROM-less arm (a desugared
// multi-row VALUES) stays out.
func isSimpleValidatableBodySelect(sel *SelectStmt) bool {
	if len(sel.From) == 0 {
		return false
	}
	if len(sel.CTEs) != 0 ||
		sel.Distinct ||
		sel.Limit != nil || sel.LimitParam != nil ||
		sel.Offset != nil || sel.OffsetParam != nil {
		return false
	}
	for _, nw := range sel.Windows {
		if !r34vWindowSpecValidatable(nw.Spec) {
			return false
		}
	}
	for _, arm := range sel.Compound {
		if arm.Stmt == nil || len(arm.Stmt.Compound) != 0 || !isSimpleValidatableBodySelect(arm.Stmt) {
			return false
		}
	}
	for _, e := range sel.GroupBy {
		if exprContainsSubquery(e) {
			return false
		}
	}
	if sel.Having != nil && exprContainsSubquery(sel.Having) {
		return false
	}
	for _, it := range sel.From {
		if it.Subquery != nil && !isSchemaSafeDerivedSubquery(it.Subquery) {
			return false
		}
	}
	for _, c := range sel.Columns {
		if c.Star {
			continue
		}
		if !r34vExprValidatable(c.Expr) {
			return false
		}
	}
	if !r34vExprValidatable(sel.Where) {
		return false
	}
	for _, ot := range sel.OrderBy {
		if !r34vExprValidatable(ot.Expr) {
			return false
		}
	}
	for _, it := range sel.From {
		if it.On != nil && !r34vExprValidatable(it.On) {
			return false
		}
	}
	return true
}

// r34vExprValidatable reports whether every subquery and inline window spec
// nested in e is itself something the eager pass can name-check. Recursion
// threads the enclosing scope as outer context, as sqlite3SelectPrep resolves a
// subquery, so correlated references still resolve.
func r34vExprValidatable(e Expr) bool {
	var subs []*SelectStmt
	var wins []*WindowSpec
	if !r34vExprScopes(e, &subs, &wins) {
		return false
	}
	for _, s := range subs {
		if !isTrivialBodySelect(s) && !isSimpleValidatableBodySelect(s) {
			return false
		}
	}
	for _, w := range wins {
		if !r34vWindowSpecValidatable(w) {
			return false
		}
	}
	return true
}

// r34vWindowSpecValidatable reports whether spec is a plain PARTITION BY /
// ORDER BY with no frame (frame bounds need scope rules this pass lacks). A
// base-window reference ("OVER w") is fine: the named window is validated in
// its own right.
func r34vWindowSpecValidatable(spec *WindowSpec) bool {
	if spec == nil {
		return false
	}
	if spec.Frame != nil {
		return false
	}
	for _, e := range spec.PartitionBy {
		if !r34vExprValidatable(e) {
			return false
		}
	}
	for _, ot := range spec.OrderBy {
		if !r34vExprValidatable(ot.Expr) {
			return false
		}
	}
	return true
}

// r34vExprScopes appends every nested SELECT (a scalar/EXISTS/IN subquery body)
// and every inline "OVER (...)" window spec in e to subs/wins, and reports
// whether the walk understood everything it saw. It mirrors
// exprContainsSubquery's cases exactly (sql_ast.go); an expression form it does
// not know, or a FILTER clause (whose scope this pass does not model), answers
// false so the caller keeps the whole SELECT out of the validatable shape.
func r34vExprScopes(e Expr, subs *[]*SelectStmt, wins *[]*WindowSpec) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr, ColumnExpr:
		return true
	case SubqueryExpr:
		*subs = append(*subs, x.Stmt)
		return true
	case ExistsExpr:
		*subs = append(*subs, x.Stmt)
		return true
	case UnaryExpr:
		return r34vExprScopes(x.X, subs, wins)
	case BinaryExpr:
		return r34vExprScopes(x.L, subs, wins) && r34vExprScopes(x.R, subs, wins)
	case IsNullExpr:
		return r34vExprScopes(x.X, subs, wins)
	case InExpr:
		if x.Sub != nil {
			*subs = append(*subs, x.Sub)
		}
		if !r34vExprScopes(x.X, subs, wins) {
			return false
		}
		for _, it := range x.List {
			if !r34vExprScopes(it, subs, wins) {
				return false
			}
		}
		return true
	case BetweenExpr:
		return r34vExprScopes(x.X, subs, wins) && r34vExprScopes(x.Lo, subs, wins) && r34vExprScopes(x.Hi, subs, wins)
	case LikeExpr:
		return r34vExprScopes(x.X, subs, wins) && r34vExprScopes(x.Pattern, subs, wins) && r34vExprScopes(x.Escape, subs, wins)
	case GlobExpr:
		return r34vExprScopes(x.X, subs, wins) && r34vExprScopes(x.Pattern, subs, wins)
	case MatchExpr:
		return r34vExprScopes(x.X, subs, wins) && r34vExprScopes(x.Pattern, subs, wins)
	case CollateExpr:
		return r34vExprScopes(x.X, subs, wins)
	case CastExpr:
		return r34vExprScopes(x.X, subs, wins)
	case FuncExpr:
		if x.Filter != nil || len(x.OrderBy) > 0 {
			return false
		}
		if x.Over != nil {
			*wins = append(*wins, x.Over)
		}
		for _, a := range x.Args {
			if !r34vExprScopes(a, subs, wins) {
				return false
			}
		}
		return true
	case RowExpr:
		for _, el := range x.Elems {
			if !r34vExprScopes(el, subs, wins) {
				return false
			}
		}
		return true
	case CaseExpr:
		if x.Base != nil && !r34vExprScopes(x.Base, subs, wins) {
			return false
		}
		for _, w := range x.Whens {
			if !r34vExprScopes(w.When, subs, wins) || !r34vExprScopes(w.Then, subs, wins) {
				return false
			}
		}
		if x.Else != nil {
			return r34vExprScopes(x.Else, subs, wins)
		}
		return true
	default:
		return false
	}
}

// r34vValidateExprScopes name-checks every subquery and window spec nested in e
// against ctx, the scope e itself resolves in. A nested scope this pass cannot
// model is SKIPPED rather than refused -- the caller (a WHEN clause, a select
// list) has no shape to decline at that point, and skipping only costs coverage.
func (db *DB) r34vValidateExprScopes(e Expr, ctx *evalCtx) error {
	var subs []*SelectStmt
	var wins []*WindowSpec
	if !r34vExprScopes(e, &subs, &wins) {
		return nil
	}
	for _, w := range wins {
		if !r34vWindowSpecValidatable(w) {
			continue
		}
		for _, pe := range w.PartitionBy {
			if err := validateColumnRefs(pe, ctx); err != nil {
				return err
			}
		}
		for _, ot := range w.OrderBy {
			if err := validateColumnRefs(ot.Expr, ctx); err != nil {
				return err
			}
		}
		if err := db.r34vValidateExprScopesAll(ctx, w); err != nil {
			return err
		}
	}
	for _, s := range subs {
		switch {
		case isTrivialBodySelect(s):
			if err := db.validateTrivialBodySelectExprs(s, ctx); err != nil {
				return err
			}
		case isSimpleValidatableBodySelect(s):
			if err := db.validateSimpleBodySelectEager(s, ctx, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// r34vValidateExprScopesAll recurses into a window spec's own expressions for
// the subqueries THEY may contain.
func (db *DB) r34vValidateExprScopesAll(ctx *evalCtx, w *WindowSpec) error {
	for _, pe := range w.PartitionBy {
		if err := db.r34vValidateExprScopes(pe, ctx); err != nil {
			return err
		}
	}
	for _, ot := range w.OrderBy {
		if err := db.r34vValidateExprScopes(ot.Expr, ctx); err != nil {
			return err
		}
	}
	return nil
}

// isSimpleValidatableInsertSource is isSimpleValidatableBodySelect for an
// INSERT ... SELECT body's source, which also allows LIMIT/OFFSET. A bare body
// SELECT has its ORDER BY stripped (its result is discarded), so a LIMIT there
// would pick a different row; an INSERT source runs in full with its ORDER BY.
// The eager pass only resolves names, so LIMIT does not widen what it accepts
// (misc1.test, misc3.test).
func isSimpleValidatableInsertSource(sel *SelectStmt) bool {
	if sel.Limit == nil && sel.LimitParam == nil && sel.Offset == nil && sel.OffsetParam == nil {
		return isSimpleValidatableBodySelect(sel)
	}
	cp := *sel
	cp.Limit, cp.LimitParam, cp.Offset, cp.OffsetParam = nil, nil, nil, nil
	return isSimpleValidatableBodySelect(&cp)
}

// isSchemaSafeDerivedSubquery reports whether sub, a derived table in a
// trigger body SELECT, is narrow enough for eager validation. resolveFrom
// executes a derived table to materialize it, so anything that could raise a
// data-dependent error (WHERE, function calls, joins, nesting, GROUP BY,
// ORDER BY, LIMIT, compounds, CTEs, windows, DISTINCT) is refused; only a bare
// "SELECT <cols-or-star> FROM <table>" (altertab3.test 29.4) is accepted.
func isSchemaSafeDerivedSubquery(sub *SelectStmt) bool {
	if len(sub.From) != 1 || sub.From[0].Subquery != nil || sub.From[0].On != nil {
		return false
	}
	if len(sub.Compound) != 0 || len(sub.CTEs) != 0 || len(sub.GroupBy) != 0 ||
		sub.Having != nil || len(sub.OrderBy) != 0 || len(sub.Windows) != 0 ||
		sub.Distinct || sub.Where != nil ||
		sub.Limit != nil || sub.LimitParam != nil || sub.Offset != nil || sub.OffsetParam != nil {
		return false
	}
	for _, c := range sub.Columns {
		if c.Star {
			continue
		}
		if _, ok := c.Expr.(ColumnExpr); !ok {
			return false
		}
	}
	return true
}

// validateSimpleBodySelectEager validates a simple FROM-bearing body SELECT
// (isSimpleValidatableBodySelect) against the OLD/NEW scope (outer): every table
// exists and every column reference in the select list, WHERE and JOIN ON
// resolves. It reuses the top-level SELECT's validation (validateSelectTablesExist,
// resolveFrom+buildScopes, checkFromSupported, expandSelectList,
// validateColumnRefs), so error text matches. It evaluates nothing except
// resolveFrom's materialization of an isSchemaSafeDerivedSubquery item, which
// cannot raise a data-dependent error.
func (db *DB) validateSimpleBodySelectEager(sel *SelectStmt, outer *evalCtx, orderBy bool) error {
	pager, err := db.SnapshotPager()
	if err != nil {
		return err
	}
	if err := validateSelectTablesExist(pager, sel); err != nil {
		return err
	}
	// Validate a derived item's own body first, as sqlite3SelectPrep
	// recurses into a subquery. Otherwise resolveFrom reaches the error
	// wrapped as a VDBE decline, which r33rTriggersResolveAfter does not
	// recognize, and DROP COLUMN b over "SELECT a, b FROM (SELECT a, b
	// FROM t1)" succeeded where C errors.
	for _, it := range sel.From {
		if it.Subquery == nil {
			continue
		}
		if err := db.validateSimpleBodySelectEager(it.Subquery, outer, false); err != nil {
			return err
		}
	}
	jts, _, err := pager.resolveFrom(sel.From, nil)
	if err != nil {
		return err
	}
	scopes := buildScopes(jts)
	ons := onsOf(jts)
	if err := checkFromSupported(ons); err != nil {
		return err
	}
	planCtx := &evalCtx{outer: outer, tables: scopes, pager: pager}
	for _, on := range ons {
		if err := validateColumnRefs(on, planCtx); err != nil {
			return fmt.Errorf("engine: JOIN ... ON: %w", err)
		}
	}
	// expandSelectList validates each "*"/"t.*" qualifier resolves to a FROM
	// scope (its own "no such table" for a bad qualifier), matching a top-level
	// SELECT; a bare "*" is always fine. Its expansion is otherwise wanted only
	// for the ORDER BY check at the bottom.
	outCols, err := expandSelectList(sel.Columns, scopes, pager.colNameMode())
	if err != nil {
		return err
	}
	for _, c := range sel.Columns {
		if c.Star {
			continue
		}
		if err := validateColumnRefs(c.Expr, planCtx); err != nil {
			return err
		}
		if err := db.r34vValidateExprScopes(c.Expr, planCtx); err != nil {
			return err
		}
	}
	if sel.Where != nil {
		if err := validateColumnRefs(sel.Where, planCtx); err != nil {
			return err
		}
		if err := db.r34vValidateExprScopes(sel.Where, planCtx); err != nil {
			return err
		}
	}
	// A "WINDOW <name> AS (...)" clause's own PARTITION BY / ORDER BY resolve
	// against these same scopes, and are resolved whether or not the SELECT's
	// result is discarded -- "SELECT a, count(*) OVER w FROM t1 WINDOW w AS
	// (PARTITION BY b)" after "ALTER TABLE t1 DROP COLUMN b" is "error in trigger
	// tr after drop column: no such column: b" on 3.53.3, and this engine
	// performed the drop. An inline "OVER (...)" reaches the same check through
	// r34vValidateExprScopes above.
	for _, nw := range sel.Windows {
		if nw.Spec == nil {
			continue
		}
		for _, pe := range nw.Spec.PartitionBy {
			if err := validateColumnRefs(pe, planCtx); err != nil {
				return err
			}
		}
		for _, ot := range nw.Spec.OrderBy {
			if err := validateColumnRefs(ot.Expr, planCtx); err != nil {
				return err
			}
		}
		if err := db.r34vValidateExprScopesAll(planCtx, nw.Spec); err != nil {
			return err
		}
	}
	// GROUP BY and HAVING are resolved even for a discarded result, so they
	// are not gated on orderBy. Both see the result-column aliases
	// (NC_UEList, resolve.c), which validateColumnRefs does not model, so an
	// alias-naming term is skipped rather than wrongly rejected ("SELECT x
	// AS z, count(*) FROM t1 GROUP BY z HAVING z>0" is valid).
	for _, g := range sel.GroupBy {
		if orderTermNamesOutputColumn(g, sel.Columns, outCols) || r34vNamesOutputAlias(g, sel.Columns) {
			continue
		}
		if err := validateColumnRefs(g, planCtx); err != nil {
			return err
		}
	}
	if sel.Having != nil && !r34vNamesOutputAlias(sel.Having, sel.Columns) {
		if err := validateColumnRefs(sel.Having, planCtx); err != nil {
			return err
		}
	}
	// Every further compound arm carries its own FROM clause and so its own
	// scopes; recursing is what name-checks it at all (sqlite3ResolveSelectNames
	// walks pPrior the same way). An arm has no ORDER BY of its own.
	for _, arm := range sel.Compound {
		if err := db.validateSimpleBodySelectEager(arm.Stmt, outer, false); err != nil {
			return err
		}
	}
	// ORDER BY is validated only when the result is consumed (orderBy, an
	// INSERT ... SELECT source). A discarded SELECT's ORDER BY is never
	// name-resolved by C ("SELECT x FROM t1 ORDER BY nosuchcol" in a body is
	// not an error, even when it fires), but an INSERT source's is.
	if orderBy {
		for _, ot := range sel.OrderBy {
			// An ORDER BY term may name an output column instead of an input
			// one -- an ordinal ("ORDER BY 1") or a result alias ("SELECT b AS
			// z ... ORDER BY z") -- neither of which is in planCtx's scopes.
			// Validating those as column references would reject exactly what
			// C SQLite accepts, which is the failure this whole eager pass
			// exists to avoid in the other direction.
			if orderTermNamesOutputColumn(ot.Expr, sel.Columns, outCols) {
				continue
			}
			if err := validateColumnRefs(ot.Expr, planCtx); err != nil {
				return err
			}
		}
	}
	return nil
}

// r34vNamesOutputAlias reports whether e references one of this SELECT's
// explicit AS aliases, which lookupName resolves under NC_UEList (resolve.c;
// only ENAME_NAME, i.e. an AS, counts). validateSimpleBodySelectEager skips
// such GROUP BY/HAVING terms; over-matching only skips a check.
func r34vNamesOutputAlias(e Expr, cols []SelectColumn) bool {
	for _, c := range cols {
		if c.HasAlias && exprTreeMentionsColumn(e, c.Alias) {
			return true
		}
	}
	return false
}

// orderTermNamesOutputColumn reports whether an ORDER BY term names one of the
// SELECT's own OUTPUT columns rather than an input column: a bare integer
// ordinal, or an unqualified identifier matching a result alias or an expanded
// result-column name. Only those two forms; anything else (an expression, a
// qualified reference) resolves against the FROM scopes like any other
// expression.
func orderTermNamesOutputColumn(e Expr, cols []SelectColumn, out []outputColumn) bool {
	switch x := e.(type) {
	case LiteralExpr:
		return x.Val.Typ == Int
	case ColumnExpr:
		if x.Qualifier != "" || x.Schema != "" {
			return false
		}
		for _, c := range cols {
			if c.HasAlias && equalFoldName(c.Alias, x.Name) {
				return true
			}
		}
		for _, o := range out {
			if equalFoldName(o.name, x.Name) {
				return true
			}
		}
	}
	return false
}

// validateTriggerBodyStmtExprsOnce validates one body statement's expressions
// (INSERT VALUES and UPSERT SET/WHERE; UPDATE SET/WHERE; DELETE WHERE) against
// outer, the OLD/NEW schema-only context.
//
// UPDATE/DELETE also need their own target's scope (db.findTableMeta); if the
// target does not exist yet, validation is skipped and execution reports "no
// such table" if reached. An UPSERT's SET/WHERE needs the target's scope plus a
// schema-only "excluded" scope, matching the compiled upsert at
// fire time. VALUES tuples are validated against outer alone, since they cannot
// reference the row being inserted.
func (db *DB) validateTriggerBodyStmtExprsOnce(bs triggerBodyStmt, outer *evalCtx) error {
	switch {
	case bs.insert != nil:
		st := bs.insert
		for _, row := range st.rows {
			if err := validateColumnRefsAll(outer, row...); err != nil {
				return err
			}
		}
		if st.upsert != nil {
			if t2 := db.findTableMeta(st.table); t2 != nil {
				tblScope := tableScope{name: t2.name, cols: t2.cols, colIndex: buildColIndex(t2.cols), offset: 0}
				exclScope := tableScope{name: "excluded", cols: t2.cols, colIndex: buildColIndex(t2.cols), offset: 0, unqualifiedHidden: true}
				ctx := &evalCtx{tables: []tableScope{tblScope, exclScope}, outer: outer}
				for up := st.upsert; up != nil; up = up.next {
					for _, a := range up.sets {
						if err := validateColumnRefs(a.expr, ctx); err != nil {
							return err
						}
					}
					if err := validateColumnRefs(up.where, ctx); err != nil {
						return err
					}
				}
			}
		}
	case bs.update != nil:
		st := bs.update
		if st.from != nil {
			// UPDATE ... FROM: the FROM sources' columns are in scope for SET
			// and WHERE, which this schema-only context cannot model, so skip.
			// The compiled UPDATE ... FROM resolves every reference at fire
			// time even for a zero-row join, and C also reports such errors at fire time, not
			// at CREATE TRIGGER.
			return nil
		}
		t2 := db.findTableMeta(st.table)
		if t2 == nil {
			return nil
		}
		ctx := &evalCtx{tables: []tableScope{{name: t2.name, cols: t2.cols, colIndex: buildColIndex(t2.cols), offset: 0}}, outer: outer}
		for _, a := range st.sets {
			if err := validateColumnRefs(a.expr, ctx); err != nil {
				return err
			}
		}
		if err := validateColumnRefs(st.where, ctx); err != nil {
			return err
		}
	case bs.delete != nil:
		st := bs.delete
		t2 := db.findTableMeta(st.table)
		if t2 == nil {
			return nil
		}
		ctx := &evalCtx{tables: []tableScope{{name: t2.name, cols: t2.cols, colIndex: buildColIndex(t2.cols), offset: 0}}, outer: outer}
		if err := validateColumnRefs(st.where, ctx); err != nil {
			return err
		}
	}
	return nil
}

// beginStatementSnapshot captures a full-database snapshot before any row is
// touched and returns a restore func the caller defers with errp, its named
// error return. Its callers are the two sides of a delegated ATTACHed write
// (attach_write.go), which must succeed or fail together; the compiled write
// path uses its own journal (writeCtx.rollback, vdbe_write.go).
//
// The restore is skipped if db.txActive changed: a nested "OR ROLLBACK"
// (conflict.go) has already unwound the whole transaction, and re-applying
// this statement's narrower snapshot would undo that.
func (db *DB) beginStatementSnapshot(errp *error) func() {
	wasTxActive := db.txActive
	snap := db.captureSnapshot()
	return func() {
		if *errp == nil {
			return
		}
		// A RAISE(FAIL/ROLLBACK) error carries a conflict scope that differs
		// from the ordinary whole-statement undo (see raiseError's doc
		// comment); RAISE(ABORT) wants exactly the ordinary undo.
		if re, ok := asRaiseError(*errp); ok {
			switch re.action {
			case conflictFail:
				return
			case conflictRollback:
				// RAISE(ROLLBACK): unwind the ENTIRE enclosing explicit
				// transaction, exactly like an "OR ROLLBACK" conflict
				// (conflict.go). With no active transaction it degrades to
				// ABORT (this single statement IS the transaction), handled by
				// the whole-statement restore below.
				if db.txActive {
					db.restoreSnapshot(db.txSnapshot)
					db.clearTxnState() // savepoints inside it go with it -- see txn.go
					return
				}
			}
			// conflictAbort (and the no-transaction ROLLBACK degrade) fall
			// through to the ordinary whole-statement snapshot restore.
		}
		// An "OR FAIL" (or a per-constraint declared ON CONFLICT FAIL)
		// violation wants precisely the RAISE(FAIL) treatment above: undo
		// nothing. The conflict-aware row loops tag their error for exactly
		// this (conflict.go's conflictFailErr, which carries the verified
		// oracle evidence). FAIL is the ONLY conflict action needing a tag:
		// ABORT wants this restore, and an "OR ROLLBACK" that really unwound a
		// transaction already flipped db.txActive, which the check below sees.
		if isConflictFailErr(*errp) {
			return
		}
		if db.txActive == wasTxActive {
			db.restoreSnapshot(snap)
		}
	}
}

// ---- recursion / cascade bookkeeping ----

// triggerFireState carries recursion bookkeeping through one statement's
// trigger cascade (built lazily by writeCtx.fireState, vdbe_write.go). active
// is the chain of executing triggers by pointer identity: with
// recursive_triggers OFF the guard is "same trigger already active", which also
// bounds any cycle since a finite trigger set must revisit one. orconf/orconfSet
// carry the trigger program's conflict policy.
type triggerFireState struct {
	active    []*triggerMeta
	orconf    conflictAction
	orconfSet bool
}

// maxTriggerDepth is SQLITE_MAX_TRIGGER_DEPTH, reachable with
// recursive_triggers ON. C evaluates WHEN inside the pushed frame and
// OP_Program tests "nFrame >= limit" before pushing, so exactly 1000 frames may
// be live:
//
//	CREATE TRIGGER tr AFTER INSERT ON t WHEN new.x < N
//	  BEGIN INSERT INTO t VALUES(new.x+1); END;  INSERT INTO t VALUES(1)
//
//	N = 1000  ->  1000 rows
//	N = 1001  ->  "too many levels of trigger recursion", statement rolled back
const maxTriggerDepth = 1000

// triggerEnter reports whether tr should be skipped (already active, with
// recursive_triggers OFF; the caller's write still happens) or pushes it and
// returns skip=false for the caller to fire and later leave(). A non-nil err
// (the depth limit) must unwind the statement.
//
// With recursive_triggers ON the identity test is skipped, as OP_Program's p5
// ("bRecursive", sqlite3CodeRowTriggerDirect) is then 0. The depth limit is
// checked either way, after the identity test, in OP_Program's order.
func (db *DB) triggerEnter(fs *triggerFireState, tr *triggerMeta) (skip bool, err error) {
	if !db.recursiveTriggers {
		for _, a := range fs.active {
			if a == tr {
				return true, nil
			}
		}
	}
	if len(fs.active) >= maxTriggerDepth {
		return true, fmt.Errorf("engine: too many levels of trigger recursion")
	}
	fs.active = append(fs.active, tr)
	return false, nil
}

// ---- PRAGMA recursive_triggers ----

// SetRecursiveTriggers is the write side of "PRAGMA recursive_triggers". The
// getter returns 0/1 (default 0); the setter parses with sqlite3GetBoolean,
// takes effect inside an open transaction and is not transactional, and
// ignores a database qualifier.
//
// The flag has two observable effects in C:
//  1. it clears OP_Program's recursion guard (triggerEnter);
//  2. a REPLACE conflict's victim delete fires DELETE triggers
//     (sqlite3GenerateConstraintChecks; see compileReplaceVictimDeletePlans,
//     vdbe_write.go).
func (db *DB) SetRecursiveTriggers(on bool) {
	if db.recursiveTriggers == on {
		return
	}
	db.recursiveTriggers = on
	// Effect (2) is baked into compiled insert plans, and the write-plan cache
	// is keyed on statement text (cachedWriteProgram), so drop it: otherwise a
	// repeated statement would replay a plan compiled under the old flag.
	// Same hazard and fix as SetIgnoreCheckConstraints.
	db.writePlans = nil
}

// RecursiveTriggers reports the current "PRAGMA recursive_triggers" setting.
func (db *DB) RecursiveTriggers() bool { return db.recursiveTriggers }

// hasReplaceVictimDeleteTriggers reports whether tbl has a DELETE trigger while
// recursive_triggers is ON, sqlite3TriggersExist's condition as consulted by
// sqlite3GenerateConstraintChecks (insert.c:2220-2222):
//
//	if( db->flags&SQLITE_RecTriggers ){
//	  pTrigger = sqlite3TriggersExist(pParse, pTab, TK_DELETE, 0, 0);
//	  regTrigCnt = pTrigger!=0 || sqlite3FkRequired(pParse, pTab, 0, 0);
//	}
//
// A conflict-resolving INSERT/UPDATE may then fire them for the row its
// REPLACE deletes (compileReplaceVictimDeletePlans, vdbe_write.go).
func (db *DB) hasReplaceVictimDeleteTriggers(tbl *tableMeta) bool {
	if !db.recursiveTriggers || tbl == nil {
		return false
	}
	return len(db.matchingTriggers(tbl.name, tbl.isTemp, triggerDelete, triggerBefore)) > 0 ||
		len(db.matchingTriggers(tbl.name, tbl.isTemp, triggerDelete, triggerAfter)) > 0
}

// enterOrconfReplace sets the conflict policy for a REPLACE victim delete. Both insert.c
// sites that fire it (2340, IPK/rowid; 2613, UNIQUE index) pass the literal
// OE_Replace to sqlite3GenerateRowDelete, so codeTriggerProgram's
// "pParse->eOrconf = (orconf==OE_Default) ? pStep->orconf : orconf"
// (trigger.c:1137) installs REPLACE for the trigger body, overriding any outer
// policy. An ordinary DELETE passes OE_Default.
func (fs *triggerFireState) enterOrconfReplace() func() {
	prev, prevSet := fs.orconf, fs.orconfSet
	fs.orconf, fs.orconfSet = conflictReplace, true
	return func() { fs.orconf, fs.orconfSet = prev, prevSet }
}

func (fs *triggerFireState) leave() {
	fs.active = fs.active[:len(fs.active)-1]
}

// checkTriggerBodyTables rejects a trigger whose body names a table that does
// not exist. C compiles the trigger program at prepare time, so the error does
// not depend on how many rows match (fts3conf.test 4.4.2: "no such table:
// main.AFTS" even for an empty table). Virtual tables and views count as
// existing, as sqlite3LocateTable resolves all three (fts5content.test 2.1).
func (db *DB) checkTriggerBodyTables(trs []*triggerMeta) error {
	schema := localSchemaOr(db.localSchema)
	for _, tr := range trs {
		for _, name := range triggerBodyTableNames(tr) {
			if name == "" {
				continue
			}
			if db.findTableMeta(name) == nil && db.findViewMeta(name) == nil && db.findVtabMeta(name) == nil {
				// A TEMP trigger's unqualified body reference gets the full
				// TEMP/MAIN/ATTACHED search (sqlite3FindTable, build.c:373-385):
				// sqlite3FixInit sets bTemp = (iDb==1) (attach.c:539), so the
				// schema pin in fixSelectCb (attach.c:492) never applies to it,
				// whereas a MAIN trigger or view is pinned at CREATE
				// (trigger.c:346; build.c:3032). Same rule as
				// checkSchemaObjectsResolve (alter_write.go); only a base TABLE
				// counts. trigger1.test 10.10.
				if tr.isTemp && db.firstAttachedTable(name) != nil {
					continue
				}
				return fmt.Errorf("engine: no such table: %s.%s", schema, name)
			}
		}
	}
	return nil
}

// triggerTargetScope is the catalog a CREATE TRIGGER's ON-clause table
// resolves in: an explicit "main."/"temp." on the ON clause wins; otherwise a
// "main."-qualified trigger NAME pins it to main, and everything else resolves
// temp-first. Verified directly against mattn/go-sqlite3 with a main AND a temp
// t300 both present (triggerD.test's own 3.x case): "CREATE TRIGGER main.r300
// ... ON t300" fires for "INSERT INTO main.t300" (10003), while the same
// statement written "CREATE TRIGGER r300 ... ON t300" fires for "INSERT INTO
// temp.t300" (10004) instead.
func triggerTargetScope(nameScope, tableScope schemaScope) schemaScope {
	if tableScope != scopeAny {
		return tableScope
	}
	if nameScope == scopeMain {
		return scopeMain
	}
	return scopeAny
}

// checkTriggerTempScope rejects a MAIN-schema trigger whose body references a
// TEMP table: C's main trigger cannot reach the temp schema, so the INSERT
// fails at prepare time with "no such table: main.t2" (trigger1.test). The
// error is raised when the triggering statement is about to fire, as in C. A
// TEMP trigger reaching a temp table is fine.
func (db *DB) checkTriggerTempScope(trs []*triggerMeta) error {
	tempTables := make(map[string]bool)
	for _, t := range db.tables {
		if t.isTemp {
			tempTables[r33sFoldIdent(t.name)] = true
		}
	}
	if len(tempTables) == 0 {
		return nil
	}
	for _, tr := range trs {
		if tr.isTemp {
			continue
		}
		for _, name := range triggerBodyTableNames(tr) {
			if tempTables[r33sFoldIdent(name)] && db.findTableMetaIn(scopeMain, name) == nil {
				return fmt.Errorf("engine: no such table: main.%s", name)
			}
		}
	}
	return nil
}

// triggerBodyTableNames returns every table name a trigger's body statements
// name -- each DML target plus every table their source SELECTs read.
func triggerBodyTableNames(tr *triggerMeta) []string {
	var out []string
	for _, bs := range tr.body {
		switch {
		case bs.insert != nil:
			out = append(out, bs.insert.table)
			collectSelectTables(bs.insert.selectStmt, &out)
		case bs.update != nil:
			out = append(out, bs.update.table)
		case bs.delete != nil:
			out = append(out, bs.delete.table)
		case bs.sel != nil:
			collectSelectTables(bs.sel, &out)
		}
	}
	return out
}

// deferredLiteralErr returns the first LiteralExpr.deferredErr (sql_ast.go)
// found in e or any descendant wherePlanWalkChildren reaches, or "" if none.
// wherePlanWalkChildren does not descend into a subquery, so this only ever
// sees a poisoned literal sitting directly in e's own expression tree -- see
// triggerBodyStmtDeferredLiteralErr's doc comment for why that is the scope
// this needs.
func deferredLiteralErr(e Expr) string {
	if e == nil {
		return ""
	}
	if lit, ok := e.(LiteralExpr); ok {
		return lit.deferredErr
	}
	found := ""
	wherePlanWalkChildren(e, func(c Expr) {
		if found == "" {
			found = deferredLiteralErr(c)
		}
	})
	return found
}

// triggerBodyStmtDeferredLiteralErr returns the first held-open literal
// magnitude error (LiteralExpr.deferredErr) in bs's INSERT/UPDATE/DELETE
// expressions, or "". bs.sel is not checked: a bare body SELECT reaches
// compileExpr directly, which raises it in place. The write compiler has
// fast paths (seek keys, constant folding) that might read a literal's
// placeholder value, so callers raise before entering it
// (compileTriggerBodyStmtInner, vdbe_trigger.go).
func triggerBodyStmtDeferredLiteralErr(bs triggerBodyStmt) string {
	switch {
	case bs.insert != nil:
		for _, row := range bs.insert.rows {
			for _, e := range row {
				if s := deferredLiteralErr(e); s != "" {
					return s
				}
			}
		}
	case bs.update != nil:
		for _, a := range bs.update.sets {
			if s := deferredLiteralErr(a.expr); s != "" {
				return s
			}
		}
		return deferredLiteralErr(bs.update.where)
	case bs.delete != nil:
		return deferredLiteralErr(bs.delete.where)
	}
	return ""
}

// applyOrconfInsert / applyOrconfUpdate are the rule itself, taken as plain
// values so the compiler can apply it while BUILDING a trigger body's program
// -- pParse->eOrconf resolved at COMPILE time, once, rather than per firing
// (compileTriggerBodyStmtInner, vdbe_trigger.go; trigCompileCtx.orconf carries
// the pair).
func applyOrconfInsert(action conflictAction, set bool, st *insertStmt) *insertStmt {
	if !set || (st.explicitOr && st.orAction == action) {
		return st
	}
	cp := *st
	cp.orAction, cp.explicitOr = action, true
	return &cp
}

func applyOrconfUpdate(action conflictAction, set bool, st *updateStmt) *updateStmt {
	if !set || (st.explicitOr && st.orAction == action) {
		return st
	}
	cp := *st
	cp.orAction, cp.explicitOr = action, true
	return &cp
}

// ---- per-row, trigger-aware write primitives ----

// beforeInsertAutoRowid is the rowid a BEFORE INSERT program sees for an
// auto-assigned row: C allocates it after the BEFORE program, so NEW.rowid
// (and an INTEGER PRIMARY KEY aliasing it) reads -1 (insert3.test 2.2). The
// emitter is compiler.beforeInsertRowid (vdbe_codegen.go); this constant is
// unreferenced.
const beforeInsertAutoRowid = ^uint64(0) // int64(-1)

// declineTriggerWhenSubqueryCollation rejects a trigger WHEN clause containing
// a subquery while its table declares a non-BINARY collation. Inside such a
// subquery an OLD./NEW. reference loses its declared collation, so
//
//	CREATE TABLE t1(x COLLATE NOCASE PRIMARY KEY);
//	CREATE TRIGGER tt1 AFTER DELETE ON t1
//	  WHEN EXISTS (SELECT 1 FROM t2 WHERE old.x = y) ...
//
// would compare BINARY where C uses NOCASE. Declined rather than wrong; the
// fix is to carry the outer column's declared collation into the compiled
// comparison.
func declineTriggerWhenSubqueryCollation(tbl *tableMeta, tr *triggerMeta) error {
	if !exprContainsSubquery(tr.when) {
		return nil
	}
	for _, c := range tbl.cols {
		if coll := effectiveCollation(c.Collation); coll != "BINARY" {
			return fmt.Errorf("%w: trigger %s has a subquery in its WHEN clause while %s.%s declares COLLATE %s -- an OLD./NEW. reference inside a compiled subquery arrives without the column identity its declared collation comes from",
				errVDBEUnsupported, tr.name, tbl.name, c.Name, coll)
		}
	}
	return nil
}
