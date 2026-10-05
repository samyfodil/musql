package engine

import (
	"fmt"
	"strings"
)

// ---- "PRAGMA defer_foreign_keys" issued outside a transaction ----
//
// The setter is accepted in autocommit by tracking what C's autocommit boundary
// needs: whether the next top-level statement's Vdbe would set bIsReader.
// Every membership below is a C citation or a row measured in
// compat-harness/pragma_r25_defer_fk_table_test.go.
//
// ---- the C mechanism ----
//
// sqlite3VdbeHalt (vdbeaux.c:3344) skips its commit/rollback cleanup --
// SQLITE_DeferFKs clear included -- unless p->bIsReader, fixed at prepare by
// resolveP2Values (vdbeaux.c:871-910) when the program has OP_Transaction,
// OP_AutoCommit, OP_Savepoint, OP_Checkpoint, OP_Vacuum or OP_JournalMode on
// any database (unlike mainReadTxnOf, which is main-only). A prepare failure
// has no Vdbe and never sets it.
//
// The clear is further gated on "db->autoCommit && db->nVdbeWrite==(p->
// readOnly==0)" (vdbeaux.c:3401-3405). BEGIN turns autoCommit off while
// running, so its own Halt skips the clear and the flag is kept;
// COMMIT/ROLLBACK/END turn it back on, so they clear -- even an erroring one
// with no transaction open, which still codes OP_AutoCommit(1).
//
// With both conditions met the clear happens on success (vdbeaux.c:3432-3434)
// and on failure via sqlite3RollbackAll (main.c:1495, 1530-1532). So a
// statement failing at prepare keeps the flag; one failing at run time has
// cleared it.
//
// ---- the classifier ----
//
// deferFKAutocommitClearOf(sqlText) answers, for one top-level statement,
// whether C's program would set bIsReader: deferFKCleared, deferFKKept, or
// deferFKUnknown -- which deferFKAutocommitGuard turns into a decline, since a
// wrong guess either way is a wrong answer (a late violation report, or
// RESTRICT re-enabled too early).
type deferFKClear uint8

const (
	deferFKKept deferFKClear = iota
	deferFKCleared
	deferFKUnknown
)

// deferFKAutocommitActive reports whether this session is inside the narrow
// window PRAGMA defer_foreign_keys' autocommit acceptance opens: the flag is
// ON and there is no transaction of EITHER kind open. db.heldTransaction on
// its own (a driver BEGIN with no engine-level transaction underneath)
// is excluded on purpose but is not really a second case in practice --
// execPragma's own setter declines there instead of turning the flag on
// (fkDeferredUnmodelled's own gap, fk.go), so this can only become true right
// after the setter accepted the flag with NO transaction at all outstanding.
func (db *DB) deferFKAutocommitActive() bool {
	return db != nil && db.deferFKs && !db.txActive && !db.heldTransaction
}

// deferFKAutocommitGuard is ExecArgs' hook into the deferFKAutocommitActive
// window, called before the statement is routed or compiled (like
// queryOnlyRefusesWrite). Outside the window it is one bool read.
//
// topLevel excludes trigger body steps, which never get their own Vdbe in C.
// A Kept verdict changes nothing; a Cleared verdict is applied after the
// statement runs (deferFKAutocommitApply), so the classified setter/getter
// itself is not stomped; Unknown declines the statement.
func (db *DB) deferFKAutocommitGuard(sqlText string, topLevel bool) error {
	if !topLevel || !db.deferFKAutocommitActive() {
		return nil
	}
	if db.deferFKAutocommitClearOf(sqlText) == deferFKUnknown {
		return fmt.Errorf("%w: PRAGMA defer_foreign_keys is ON with no transaction open, and this statement is outside the table this engine has measured for whether C SQLite would then clear it (see engine/pragma_defer_fk_autocommit.go) -- declined rather than guessed, because guessing wrong in either direction is a wrong answer: %s", errVDBEUnsupported, sqlText)
	}
	return nil
}

// deferFKAutocommitApply is deferFKAutocommitGuard's other half, called from
// ExecArgs AFTER the statement has run -- success or failure alike, see this
// file's own doc comment for why the RUN outcome plays no part. Only a
// deferFKCleared verdict does anything; deferFKKept is by definition a no-op,
// and deferFKUnknown never reaches here because the guard above already
// declined the statement before it ran.
func (db *DB) deferFKAutocommitApply(sqlText string, topLevel bool) {
	if !topLevel || !db.deferFKAutocommitActive() {
		return
	}
	if db.deferFKAutocommitClearOf(sqlText) == deferFKCleared {
		// SetDeferForeignKeys(false) is exactly sqlite3VdbeHalt's own clear
		// (vdbeaux.c:3432-3434 / main.c:1530-1532): both zero the deferred
		// counters AND clear the flag, never just the flag alone.
		db.SetDeferForeignKeys(false)
	}
}

// deferFKAutocommitClearOf classifies ONE top-level statement's text -- see
// this file's doc comment for the exact question it answers.
func (db *DB) deferFKAutocommitClearOf(sqlText string) deferFKClear {
	trimmed := strings.TrimSpace(sqlText)
	verb, ok := LeadingStatementVerb(trimmed)
	if !ok {
		return deferFKUnknown
	}
	switch verb {
	case "BEGIN":
		// Every spelling -- plain, DEFERRED, IMMEDIATE, EXCLUSIVE -- sets
		// bIsReader but leaves autoCommit at 0 by the time its own Halt call
		// looks; see this file's doc comment. Measured for all four.
		return deferFKKept
	case "SAVEPOINT":
		return deferFKKept
	case "COMMIT", "END":
		// Measured even when there is nothing to commit: it errors AND clears
		// (OP_AutoCommit(1) still runs before the "no transaction" check
		// fires).
		return deferFKCleared
	case "ROLLBACK":
		// Only the PLAIN form is measured. "ROLLBACK TO <savepoint>" cannot
		// even be reached inside deferFKAutocommitActive's window (no
		// transaction is open, so no savepoint exists to roll back to) and is
		// left unclassified rather than guessed.
		if strings.Contains(strings.ToUpper(trimmed), "TO") {
			return deferFKUnknown
		}
		return deferFKCleared
	case "ATTACH":
		return deferFKCleared
	case "INSERT", "REPLACE", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "VACUUM", "ANALYZE":
		return db.deferFKAutocommitClearOfDML(trimmed)
	case "PRAGMA":
		return db.deferFKAutocommitClearOfPragma(trimmed)
	}
	// DETACH, RELEASE, REINDEX (whose own clear/keep genuinely depends on
	// whether there is an index to rebuild -- pragma_r25's own note, not
	// reproduced here), and everything else this table never measured.
	return deferFKUnknown
}

// deferFKAutocommitClearOfDML is deferFKAutocommitClearOf's case for a
// statement whose prepare either allocates a Vdbe (which touches a database:
// Cleared, whatever happens at run time) or fails (no Vdbe: Kept).
//
// It compiles through db.cachedWriteProgram, the same cache tryVDBEWrite reads
// next, so no second parse is paid. A compile failure is Kept, as C's prepare
// failure is -- including a "DROP <non-object-kind>" this path cannot run.
//
// With any ATTACHed database it is Unknown: a statement targeting the
// attachment is delegated whole to its own session (execRoutedToAttached), so
// compiling it against this catalog could resolve the wrong table, and the
// flag is connection-wide in C.
func (db *DB) deferFKAutocommitClearOfDML(trimmed string) deferFKClear {
	if len(db.attached) != 0 {
		return deferFKUnknown
	}
	if _, err := db.cachedWriteProgram(trimmed); err != nil {
		return deferFKKept
	}
	return deferFKCleared
}

// deferFKAutocommitClearOfPragma is deferFKAutocommitClearOf's PRAGMA case.
func (db *DB) deferFKAutocommitClearOfPragma(trimmed string) deferFKClear {
	stmt, err := ParsePragma(trimmed)
	if err != nil {
		return deferFKUnknown
	}
	if stmt.Schema != "" {
		// A qualified pragma can be routed to an ATTACHed database's own
		// reader (queryPragmaStmt) or answered per-connection depending on the
		// name -- a second classification question this file does not answer.
		// Left unknown rather than guessed.
		return deferFKUnknown
	}
	if unknownPragmaIsNoop(stmt.Name) {
		// C SQLite never even resolves the name to a PragTyp, let alone
		// allocates a Vdbe that touches anything -- pragma_r25's own
		// "PRAGMA no_such_pragma_at_all" row.
		return deferFKKept
	}
	if deferFKAutocommitPragmaKept[stmt.Name] {
		return deferFKKept
	}
	if deferFKAutocommitPragmaCleared[stmt.Name] {
		return deferFKCleared
	}
	return deferFKUnknown
}

// deferFKAutocommitPragmaKept is every pragma name whose setter and getter
// never touch a database, so the Vdbe never sets bIsReader:
//
//   - every PragTyp_FLAG name (mkpragmatab.tcl): the setter emits OP_Expire
//     (pragma.c:1152-1194) and the getter a register load, never a cursor;
//   - the other names measured Kept: locking_mode, synchronous, cache_size,
//     cache_spill, database_list, collation_list, case_sensitive_like,
//     temp_store, busy_timeout, auto_vacuum, encoding, page_size,
//     wal_autocheckpoint, journal_size_limit, mmap_size, threads,
//     shrink_memory, secure_delete, analysis_limit, lock_status.
var deferFKAutocommitPragmaKept = map[string]bool{
	// PragTyp_FLAG (mkpragmatab.tcl), pragma.c:1152-1194:
	"full_column_names": true, "short_column_names": true,
	"count_changes": true, "empty_result_callbacks": true,
	"fullfsync": true, "checkpoint_fullfsync": true,
	"reverse_unordered_selects": true, "query_only": true,
	"automatic_index": true, "sql_trace": true, "vdbe_listing": true,
	"vdbe_trace": true, "vdbe_addoptrace": true, "vdbe_debug": true,
	"vdbe_eqp": true, "ignore_check_constraints": true,
	"writable_schema": true, "read_uncommitted": true,
	"recursive_triggers": true, "trusted_schema": true,
	"foreign_keys": true, "defer_foreign_keys": true,
	"cell_size_check": true, "parser_trace": true,
	"legacy_alter_table": true,
	// measured directly, pragma_r25_defer_fk_table_test.go:
	"locking_mode": true, "synchronous": true, "cache_size": true,
	"cache_spill": true, "database_list": true, "collation_list": true,
	"case_sensitive_like": true, "temp_store": true, "busy_timeout": true,
	"auto_vacuum": true, "encoding": true, "page_size": true,
	"wal_autocheckpoint": true, "journal_size_limit": true,
	"mmap_size": true, "threads": true, "shrink_memory": true,
	"secure_delete": true, "analysis_limit": true, "lock_status": true,
}

// deferFKAutocommitPragmaCleared is every pragma name measured Cleared: each
// opens a cursor (a header read, a schema scan, or journal_mode's
// OP_JournalMode) regardless of this engine's support for it.
//
// optimize, foreign_key_list and foreign_key_check are in neither map: they
// open a cursor only when the target has a foreign key (pragma.c:1506, 1541,
// inside "if(pFK)"), so the answer depends on the schema. They stay Unknown.
var deferFKAutocommitPragmaCleared = map[string]bool{
	"journal_mode": true, "user_version": true, "max_page_count": true,
	"page_count": true, "freelist_count": true, "schema_version": true,
	"application_id": true, "data_version": true, "integrity_check": true,
	"quick_check": true, "table_info": true, "table_xinfo": true,
	"table_list": true, "index_list": true, "index_info": true,
	"index_xinfo": true, "wal_checkpoint": true, "incremental_vacuum": true,
}
