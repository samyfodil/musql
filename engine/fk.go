// FOREIGN KEY enforcement ("PRAGMA foreign_keys = ON"). Each rule was checked
// against C SQLite; several contradict the obvious model.
//
// An IMMEDIATE foreign key is checked at the end of the statement, not per row:
//
//	CREATE TABLE t(id INTEGER PRIMARY KEY, other REFERENCES t(id));
//	INSERT INTO t VALUES(1,2),(2,1);      -- SUCCEEDS (either row order)
//	INSERT INTO t SELECT 4,5 UNION ALL SELECT 5,4;  -- SUCCEEDS
//	INSERT INTO t VALUES(9,99);           -- FOREIGN KEY constraint failed
//
// and a trigger repairing the violation later in the statement passes. So
// enforcement is a counter accumulated across the statement, as in fkey.c.
//
// A decrement is skipped when the counter is already zero (OP_FkIfZero), so a
// pre-existing violation (made with foreign_keys OFF) neither blocks an
// unrelated statement nor cancels a new one:
//
//	-- with p empty and c holding the orphan y=1:
//	DELETE FROM c              -- ok            (decrement skipped, net 0)
//	UPDATE c SET y=2 WHERE y=1 -- FK failed     (old -1 skipped, new +1)
//	UPDATE c SET y=y           -- FK failed     (same)
//	UPDATE c SET x='b'         -- ok            (the FK column is not ASSIGNED)
//
// The child side checks only when a child column of the key is in the SET list
// (value equality is not the test; a rowid-only change does not count), so
// every mutation site passes the assigned-column mask. The parent side keys off
// values: "UPDATE p SET id=id" leaves an ON UPDATE SET NULL child alone, "SET
// id=2" nulls it.
//
// Actions: CASCADE, SET NULL and SET DEFAULT fire on the parent row's delete or
// key change. SET DEFAULT's result is checked like any value (no parent 77 for
// "y DEFAULT 77" fails; no DEFAULT gives NULL, which passes). An action's write
// goes through the child's constraints (NOT NULL, CHECK). RESTRICT fails at the
// parent operation itself, not at statement end. A REPLACE's implicit delete
// fires ON DELETE actions; an UPSERT's DO UPDATE fires ON UPDATE ones. DROP
// TABLE of a parent acts as an implicit DELETE without the parent's delete
// triggers (fkBeforeDropTable).
//
// Resolution errors are raised for any DML on the child, including a DELETE and
// all-NULL values, as C code-generation failures:
//
//	REFERENCES nosuch              -> no such table: main.nosuch
//	REFERENCES p(a) with p(a,b)    -> foreign key mismatch - "d" referencing "p"
//	REFERENCES p   with p having no PRIMARY KEY -> the same mismatch
//	REFERENCES pv  (a VIEW)        -> the same mismatch
//	REFERENCES p(rowid)            -> the same mismatch (rowid is not a column)
//
// The parent resolves in the child's own catalog (a TEMP child referencing p
// reports "no such table: temp.p"). Child values compare to the parent key by
// an index seek's rules: the parent column's affinity and collation
// (fkCheckParentHas, pragma_fkcheck.go).
//
// DEFERRED keys ("DEFERRABLE INITIALLY DEFERRED" only; "DEFERRABLE",
// "DEFERRABLE INITIALLY IMMEDIATE" and "NOT DEFERRABLE INITIALLY DEFERRED" are
// immediate) use a transaction-level counter checked at COMMIT (DB.fkDeferred,
// db->nDeferredCons) with the same rules; their actions still fire
// immediately, and RESTRICT is still immediate.
//
//   - A COMMIT failing on a deferred violation leaves the transaction open
//     (BEGIN then says "cannot start a transaction within a transaction");
//     the same COMMIT succeeds once a statement resolves the violation.
//   - ROLLBACK TO restores the counter; RELEASE of an inner savepoint keeps
//     it; RELEASE of the outermost savepoint is a commit and runs the check,
//     changing nothing if it fails.
//   - In autocommit a deferred key acts as immediate.
//
// Declined: a deferred key in a transaction a driver holds without a commit
// check (fkDeferredUnmodelled); PRAGMA defer_foreign_keys in autocommit
// (deferFKAutocommitGuard).
//
// PRAGMA defer_foreign_keys (DB.deferFKs, DB.fkDeferredImm) needs a second
// counter, db->nDeferredImmCons: the flag picks the bucket an increment lands
// in (OP_FkCounter), while OP_FkIfZero reads the constraint's own counter too
// (fkCounterFor, fkMayDecrement).
//
//	getter 1 row (0/1), setter 0 rows; "=2" is on, "=bogus" off
//	(sqlite3GetBoolean). It takes effect inside a transaction and works with
//	foreign_keys OFF.
//
//	Cleared at every COMMIT/ROLLBACK of the outermost transaction, including
//	an autocommit statement's, but not by SAVEPOINT, inner RELEASE, ROLLBACK
//	TO, or a COMMIT that fails the check.
//
//	Setting it to 0 zeroes BOTH deferred counters ("if( mask==SQLITE_DeferFKs
//	){ db->nDeferredImmCons=0; db->nDeferredCons=0; }"), so it can wipe a
//	DEFERRABLE INITIALLY DEFERRED violation.
//
//	COMMIT checks the SUM of the two counters
//	(sqlite3VdbeCheckFkDeferred), so a decrement in one can cancel the other.
//
//	While on, RESTRICT is disabled (fkActionTrigger returns nothing for
//	OE_Restrict; fkey6.test 3.2/3.3).
//
// It is not carried on driver.Conn like foreign_keys: the driver opens a fresh
// engine.DB per autocommit statement, where the setter is declined.
package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The ON DELETE / ON UPDATE actions, spelled as parseFKAction canonicalizes
// them (pragma_parse.go).
const (
	fkNoAction   = "NO ACTION"
	fkRestrict   = "RESTRICT"
	fkCascade    = "CASCADE"
	fkSetNull    = "SET NULL"
	fkSetDefault = "SET DEFAULT"
)

// fkErrViolation is the exact error C SQLite reports for an immediate (or
// RESTRICT) foreign key violation: no table name, no constraint name. The
// "engine: " prefix is this package's own convention; the substring after it
// is what compat-harness/tcl_test.go classifies on.
func fkErrViolation() error {
	return fmt.Errorf("engine: FOREIGN KEY constraint failed")
}

// boolToInt64 renders a Go bool as the 0/1 integer a boolean-valued pragma
// getter reports.
func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// fkConstraint is one foreign key of one child table, resolved against its
// parent. resolveErr non-nil means the resolution FAILED and any DML on the
// child must report exactly that error (see this file's doc comment); the
// remaining fields are then unset.
type fkConstraint struct {
	child      *tableMeta
	childIdx   []int  // indices into child.cols, one per FK column
	parentName string // as written in the child's CREATE TABLE text
	parent     *tableMeta
	// parentIdx indexes parent.cols, one per key column; the sentinel -1
	// means "the parent's rowid" (a foreign key onto an INTEGER PRIMARY KEY,
	// which C SQLite seeks with OP_SeekRowid and which therefore can never
	// be a "foreign key mismatch" for want of an index).
	parentIdx  []int
	affs       []affinity
	colls      []string
	onDelete   string
	onUpdate   string
	deferred   bool
	resolveErr error
	// parentMissing narrows resolveErr to the one unresolvable shape real
	// SQLite treats differently from a plain mismatch: the parent TABLE does
	// not exist at all. See fkBeforeDropTable, where a missing parent moves the
	// foreign key counter and a mismatch does not.
	parentMissing bool
}

// fkSchema is every foreign key in this session's schema, resolved once per
// schema generation. Keyed by *tableMeta pointer, which is stable exactly as
// long as both guards below hold -- the same two guards cachedWriteProgram
// uses for the identical reason (see DB.txGen).
type fkSchema struct {
	cookie   uint32
	txGen    uint32
	byChild  map[*tableMeta][]*fkConstraint
	byParent map[*tableMeta][]*fkConstraint
	// byParentName indexes every foreign key by the parent name it WRITES,
	// lower-cased and per catalog -- including the ones that failed to resolve,
	// which byParent cannot hold. fkRequireResolvable needs those: a foreign key
	// naming a table it cannot resolve against is exactly the error C SQLite
	// raises when it compiles an action program over that table.
	byParentName map[string][]*fkConstraint
	empty        bool // no table in the schema declares a foreign key at all
}

// fkEvent is one row mutation recorded for end-of-statement foreign key
// accounting. old is nil for an insert, new is nil for a delete; both are
// NORMALIZED (an INTEGER PRIMARY KEY column carries its rowid rather than the
// stored NULL) and owned copies. mask, non-nil only for an UPDATE, reports
// which columns the statement ASSIGNED -- the child side's filter (see this
// file's doc comment).
type fkEvent struct {
	tbl  *tableMeta
	old  []Value
	new  []Value
	mask []bool
	// parent is the PARENT-side work for this mutation, resolved AT THE MOMENT
	// it happened rather than at the end of the statement. That timing is
	// load-bearing for two rules: RESTRICT fails on the child rows that exist
	// WHEN the parent row is touched (so "DELETE FROM t" on a self-referencing
	// RESTRICT table fails, even though by the end of the statement every
	// referencing row is gone too), and an action must act on those same rows.
	parent []fkParentWork
	// noChild suppresses child-side accounting for a mutation whose own
	// accounting was already done in full elsewhere -- DROP TABLE's implicit
	// delete (fkBeforeDropTable), whose child rewrites must not then be
	// re-checked against a parent table that no longer exists.
	noChild bool
}

// fkParentWork is one foreign key's parent-side view of one mutation, captured
// at mutation time: the key the row carried before and after, and which child
// rows referenced each.
type fkParentWork struct {
	fk      *fkConstraint
	oldKey  []Value // nil when the row had no key (an insert, or a NULL key)
	newKey  []Value // nil when the row has none afterwards (a delete, or NULL)
	oldKids []uint64
	newKids int
}

// fkStmtState is one statement's foreign key bookkeeping.
type fkStmtState struct {
	events []fkEvent
	// counter is the immediate-constraint violation counter (fkey.c's
	// FkCounter): incremented by a violation this statement created,
	// decremented -- but never below zero, see the doc comment -- by one it
	// resolved. A positive value at the end of the statement is the error.
	counter int
	// insertedInto is the set of tables this statement's OWN execution STORED a
	// row into. It exists for one ordering rule fkParentSide enforces: real
	// SQLite runs an FK action as a trigger program at the moment the parent
	// row is touched, so a REPLACE's victim delete cascades BEFORE the
	// replacement row lands, while this engine runs a row's actions once the
	// write opcode that stored it returns (fkDrain) -- after the replacement.
	// Those two orders agree unless the action acts on a table the statement
	// also inserted into -- see fkParentSide.
	insertedInto map[*tableMeta]bool
	// restrict is set the moment a RESTRICT action is violated. It is REPORTED
	// by the drain after that row's write opcode (fkDrain) rather than thrown
	// from the mutation site, which has no error return on every path.
	restrict bool
	// next is how far fkDrain has processed events, and draining guards it
	// against itself: an action it applies records more events, which the same
	// loop picks up rather than a nested one.
	next     int
	draining bool
	// deferredBefore/deferredImmBefore are the transaction's deferred counters
	// when the statement began -- C's p->nStmtDefCons/nStmtDefImmCons -- put
	// back when the statement is rolled back (writeCtx.rollback), because a
	// drain moves them while the statement is still running.
	deferredBefore, deferredImmBefore int
}

// ---- PRAGMA foreign_keys plumbing ----

// SetForeignKeys is the write side of "PRAGMA foreign_keys = <boolean>" (see
// execPragma). It is connection-level and NOT transactional -- C SQLite
// never resets it on ROLLBACK, and a SETTER issued INSIDE a transaction is
// silently ignored (verified: "PRAGMA foreign_keys=ON; BEGIN; PRAGMA
// foreign_keys=OFF; PRAGMA foreign_keys" answers 1), which is why the caller
// checks for an open transaction rather than this function.
func (db *DB) SetForeignKeys(on bool) { db.fkEnforce = on }

// ForeignKeys reports the current "PRAGMA foreign_keys" setting.
func (db *DB) ForeignKeys() bool { return db.fkEnforce }

// inTransaction reports whether a transaction is open around the current
// statement -- either one this engine itself started (BEGIN, txn.go) or one a
// driver is holding open across statements (MarkHeldTransaction). Callers: the
// PRAGMA foreign_keys / journal_mode setter declines (pragma.go), the
// cross-database write decline (attach_write.go), and fkBeforeDropTable.
func (db *DB) inTransaction() bool { return db.txActive || db.heldTransaction }

// fkDeferredUnmodelled reports whether a DEFERRED constraint's check has
// nowhere to happen: a transaction a DRIVER is holding open across statements
// (driver's Conn.tx) rather than one this engine started with BEGIN,
// AND whose holder has not promised to run the check itself.
//
// The driver commits by CLOSING the session, so there is no COMMIT statement
// for fkCommitCheck to run at. A holder that calls MarkDeferredFKCommitter
// takes that on -- it runs CheckDeferredForeignKeys before it commits, and
// keeps the transaction OPEN when that reports a violation, which is what
// C SQLite's failed commit does. db.txActive alone is the served case,
// autocommit included (there the statement IS the transaction).
func (db *DB) fkDeferredUnmodelled() bool {
	return db.heldTransaction && !db.txActive && !db.deferredFKCommitter
}

// transactionHasACommitCheck reports whether a deferred violation this
// statement leaves behind has a later COMMIT to be reported at, so that
// fkFinishStatement must NOT report it now: an engine-level BEGIN (commitTxn
// runs fkCommitCheck, txn.go), or a driver-held transaction whose holder
// has promised to run the check at its own commit. Without one -- autocommit
// -- the statement IS the transaction, and sqlite3VdbeHalt checks the
// deferred counters right there.
func (db *DB) transactionHasACommitCheck() bool {
	return db.txActive || (db.heldTransaction && db.deferredFKCommitter)
}

// MarkDeferredFKCommitter promises that this session's holder will call
// CheckDeferredForeignKeys before committing it, and will leave the
// transaction open if that reports a violation. See fkDeferredUnmodelled.
func (db *DB) MarkDeferredFKCommitter() { db.deferredFKCommitter = true }

// CheckDeferredForeignKeys is sqlite3VdbeCheckFkDeferred for a holder that
// commits a session rather than running a COMMIT statement (fkCommitCheck's
// own caller is commitTxn, txn.go). The holder MUST NOT commit when this
// reports an error: C SQLite's COMMIT leaves the transaction open and
// SQLITE_CONSTRAINT_FOREIGNKEY is raised by the COMMIT itself.
func (db *DB) CheckDeferredForeignKeys() error { return db.fkCommitCheck() }

// fkCommitCheck is the DEFERRED foreign key check C SQLite runs at COMMIT
// (sqlite3VdbeCheckFkDeferred). Its caller must report the error
// WITHOUT ending the transaction -- see commitTxn.
//
// It tests the SUM of the two deferred counters against ZERO, exactly as
// sqlite3VdbeCheckFkDeferred does ("(db->nDeferredCons+db->nDeferredImmCons)==0
// return SQLITE_OK"). The sum -- and "!= 0" rather than "> 0" -- matters
// because "PRAGMA defer_foreign_keys" redirects an IMMEDIATE key's increments
// into fkDeferredImm while the never-decrement-from-zero test reads BOTH
// counters (fkMayDecrement), so a decrement can legitimately push one negative
// and cancel the other.
func (db *DB) fkCommitCheck() error {
	// Belt-and-suspenders alongside fkFinishStatement's identical check
	// (db.pendingLoadErr's own doc comment): every statement already reports this
	// at its own end, well before COMMIT, but this catches it too if some
	// path ever reaches COMMIT without going through fkFinishStatement first.
	if db.pendingLoadErr != nil {
		err := db.pendingLoadErr
		db.pendingLoadErr = nil
		return err
	}
	if db.fkDeferred+db.fkDeferredImm != 0 {
		return fkErrViolation()
	}
	return nil
}

// SetDeferForeignKeys is the write side of "PRAGMA defer_foreign_keys" (see
// execPragma, which is where the accept/decline rule lives). Turning it OFF
// zeroes BOTH deferred counters -- sqlite3Pragma's
//
//	if( mask==SQLITE_DeferFKs ){ db->nDeferredImmCons = 0;
//	                             db->nDeferredCons = 0; }
//
// which is wider than the pragma's own bucket and is directly observable: with
// the flag NEVER turned on, "BEGIN; DELETE FROM p" orphaning a DEFERRABLE
// INITIALLY DEFERRED child then a bare "PRAGMA defer_foreign_keys=0" makes the
// COMMIT SUCCEED, the violation simply gone. Turning it ON zeroes nothing.
func (db *DB) SetDeferForeignKeys(on bool) {
	db.deferFKs = on
	if !on {
		db.fkDeferredImm = 0
		db.fkDeferred = 0
	}
}

// DeferForeignKeys reports the current "PRAGMA defer_foreign_keys" setting.
// Nil-safe: a driver asks it of the session it is HOLDING, which is nil
// whenever there is no transaction open -- and no transaction means the flag
// is off, which is what C SQLite reports there.
func (db *DB) DeferForeignKeys() bool { return db != nil && db.deferFKs }

// fkRestrictDisabled reports whether a RESTRICT action must be SKIPPED, which
// is what "PRAGMA defer_foreign_keys" does to it: fkey.c's fkActionTrigger
// returns no trigger at all for OE_Restrict once SQLITE_DeferFKs is set, so the
// key falls through to ordinary (now deferred) counting. fkey6.test 3.2/3.3 is
// exactly this, and it is directly verified: with "c2(y REFERENCES p2 ON DELETE
// RESTRICT ON UPDATE RESTRICT)" holding y=1, "BEGIN; UPDATE p2 SET a=a-1" is
// FOREIGN KEY constraint failed with the pragma off and succeeds -- COMMIT
// included, since the shifted key still resolves -- with it on.
func (db *DB) fkRestrictDisabled() bool { return db.deferFKs }

// ---- resolution ----

// fkSchemaOf returns this session's resolved foreign keys, rebuilding them
// when the schema generation moved.
func (db *DB) fkSchemaOf() *fkSchema {
	// db.schemaGen, not the file's schemaCookie: a TEMP table can carry a
	// foreign key and does not move the cookie (bumpSchema, schema_write.go).
	if db.fkCache != nil && db.fkCache.cookie == db.schemaGen && db.fkCache.txGen == db.txGen {
		return db.fkCache
	}
	fs := &fkSchema{
		cookie:       db.schemaGen,
		txGen:        db.txGen,
		byChild:      map[*tableMeta][]*fkConstraint{},
		byParent:     map[*tableMeta][]*fkConstraint{},
		byParentName: map[string][]*fkConstraint{},
		empty:        true,
	}
	for _, t := range db.tables {
		// A CREATE TABLE text with no REFERENCES keyword anywhere cannot
		// declare a foreign key, in either the column-level or the
		// table-level spelling -- so the overwhelmingly common table costs
		// one substring search rather than a full re-parse.
		if !strings.Contains(strings.ToUpper(t.sql), "REFERENCES") {
			continue
		}
		def, err := parsePragmaTableDef(t.sql)
		if err != nil || len(def.fks) == 0 {
			continue
		}
		for _, fk := range def.fks {
			c := db.resolveFK(t, def, fk)
			fs.empty = false
			fs.byChild[t] = append(fs.byChild[t], c)
			if c.parent != nil {
				fs.byParent[c.parent] = append(fs.byParent[c.parent], c)
			}
			key := fkParentNameKey(t.isTemp, c.parentName)
			fs.byParentName[key] = append(fs.byParentName[key], c)
		}
	}
	db.fkCache = fs
	return fs
}

// fkOf returns the foreign keys declared BY tbl (tbl is the child).
func (db *DB) fkOf(tbl *tableMeta) []*fkConstraint { return db.fkSchemaOf().byChild[tbl] }

// fkReferencing returns the foreign keys that point AT tbl (tbl is the parent).
func (db *DB) fkReferencing(tbl *tableMeta) []*fkConstraint {
	return db.fkSchemaOf().byParent[tbl]
}

// fkNamingParent returns every foreign key whose WRITTEN parent name is tbl's,
// resolved or not -- see fkSchema.byParentName.
func (db *DB) fkNamingParent(tbl *tableMeta) []*fkConstraint {
	return db.fkSchemaOf().byParentName[fkParentNameKey(tbl.isTemp, tbl.name)]
}

// fkParentNameKey is byParentName's key: the catalog plus the lower-cased name.
func fkParentNameKey(isTemp bool, name string) string {
	if isTemp {
		return "temp." + r33sFoldIdent(name)
	}
	return "main." + r33sFoldIdent(name)
}

// resolveFK resolves one parsed foreign key of child against its parent,
// following exactly the rules in this file's doc comment. It never returns
// nil: an unresolvable foreign key comes back carrying its resolveErr, which
// is what every DML on the child must then report.
func (db *DB) resolveFK(child *tableMeta, def *pragmaTableDef, fk pragmaFK) *fkConstraint {
	c := &fkConstraint{
		child:      child,
		parentName: fk.toTable,
		onDelete:   fk.onDelete,
		onUpdate:   fk.onUpdate,
		deferred:   fk.deferred,
	}
	mismatch := func() *fkConstraint {
		c.resolveErr = fkCheckMismatch(child.name, fk.toTable)
		c.parent, c.parentIdx = nil, nil
		return c
	}
	for _, name := range fk.fromCols {
		ci := columnIndexByName(child.cols, name)
		if ci < 0 {
			return mismatch()
		}
		c.childIdx = append(c.childIdx, ci)
	}
	// A foreign key's parent is resolved in the CHILD's own catalog -- a TEMP
	// child cannot reach a main table (see this file's doc comment).
	scope := createScope(child.isTemp)
	parent := db.findTableMetaIn(scope, fk.toTable)
	if parent == nil {
		// A VIEW or virtual table of that name is a MISMATCH (neither has a
		// unique index to seek), not a missing table -- verified directly.
		if db.findViewMetaIn(scope, fk.toTable) != nil || db.findVtabMeta(fk.toTable) != nil {
			return mismatch()
		}
		qual := "main"
		if child.isTemp {
			qual = "temp"
		}
		c.resolveErr = fmt.Errorf("engine: no such table: %s.%s", qual, fk.toTable)
		c.parentMissing = true
		return c
	}
	c.parent = parent
	keyNames := fk.toCols
	if len(keyNames) == 0 {
		// No explicit column list: the parent's PRIMARY KEY. A parent with
		// none at all is a mismatch.
		pdef, err := parsePragmaTableDef(parent.sql)
		if err != nil {
			return mismatch()
		}
		if pdef.rowidAlias != "" {
			keyNames = []string{pdef.rowidAlias}
		} else if keyNames = pragmaPrimaryKeyColumns(pdef); len(keyNames) == 0 {
			return mismatch()
		}
	}
	if len(keyNames) != len(c.childIdx) {
		return mismatch()
	}
	for _, kn := range keyNames {
		pi := columnIndexByName(parent.cols, kn)
		if pi < 0 {
			return mismatch()
		}
		c.parentIdx = append(c.parentIdx, pi)
	}
	// The parent key columns must be EXACTLY the columns of a unique index
	// (in any order -- "FOREIGN KEY(y,x) REFERENCES p2(b,a)" against
	// "UNIQUE(a,b)" resolves, while a strict SUBSET does not), or the
	// parent's single rowid-alias column.
	if len(c.parentIdx) == 1 && c.parentIdx[0] == parent.ipkIndex && parent.ipkIndex >= 0 {
		c.parentIdx[0] = -1
		c.affs = []affinity{affInteger}
		c.colls = []string{"BINARY"}
		return c
	}
	idx := db.fkParentUniqueIndex(parent, keyNames)
	if idx == nil {
		return mismatch()
	}
	for _, pi := range c.parentIdx {
		col := parent.cols[pi]
		c.affs = append(c.affs, col.Aff)
		c.colls = append(c.colls, fkIndexCollation(idx, pi, col))
	}
	return c
}

// fkParentUniqueIndex finds a UNIQUE index on parent whose key columns are
// exactly keyNames, each with that column's default collation (including a
// WITHOUT ROWID table's PK index, indexMeta.isTablePK). nil is C's "foreign
// key mismatch". The collation test is e_fkey.test cases 5 and 6:
//
//	p4(a PRIMARY KEY, b);  CREATE UNIQUE INDEX p4i ON p4(b COLLATE nocase)
//	  -> c4(c REFERENCES p4(b)) is a MISMATCH (column b's default is BINARY)
//	p5(a PRIMARY KEY, b COLLATE nocase); UNIQUE INDEX p5i ON p5(b COLLATE binary)
//	  -> c5(c REFERENCES p5(b)) is a MISMATCH (the other way round)
//	p8(a PRIMARY KEY, b COLLATE nocase); UNIQUE INDEX p8i ON p8(b)
//	  -> c8(c REFERENCES p8(b)) RESOLVES, and the seek folds case
func (db *DB) fkParentUniqueIndex(parent *tableMeta, keyNames []string) *indexMeta {
	for _, ix := range db.indexes {
		if !ix.unique || ix.exprOrPartial || ix.isTemp != parent.isTemp || !equalFoldName(ix.table, parent.name) {
			continue
		}
		if !sameColumnSet(ix.cols, keyNames) {
			continue
		}
		if !fkIndexUsesDefaultCollations(ix, parent) {
			continue
		}
		return ix
	}
	return nil
}

// fkIndexUsesDefaultCollations reports whether every key column of ix is
// indexed under that column's own declared collating sequence -- see
// fkParentUniqueIndex for why a foreign key requires it.
func fkIndexUsesDefaultCollations(ix *indexMeta, parent *tableMeta) bool {
	for k, ci := range ix.colIdx {
		if ci < 0 || ci >= len(parent.cols) {
			return false
		}
		want := parent.cols[ci].Collation
		if want == "" {
			want = "BINARY"
		}
		got := "BINARY"
		if k < len(ix.colCollation) && ix.colCollation[k] != "" {
			got = ix.colCollation[k]
		}
		if !equalFoldName(got, want) {
			return false
		}
	}
	return true
}

// fkIndexCollation is the collating sequence a seek of idx uses for the
// parent column at table-column index pi: the index's own recorded collation
// for that key column, falling back to the column's declared one.
func fkIndexCollation(idx *indexMeta, pi int, col columnInfo) string {
	for k, ci := range idx.colIdx {
		if ci == pi && k < len(idx.colCollation) && idx.colCollation[k] != "" {
			return idx.colCollation[k]
		}
	}
	if col.Collation != "" {
		return col.Collation
	}
	return "BINARY"
}

// columnIndexByName returns the position of the column named name in cols
// (case-insensitively), or -1.
func columnIndexByName(cols []columnInfo, name string) int {
	for i := range cols {
		if equalFoldName(cols[i].Name, name) {
			return i
		}
	}
	return -1
}

// ---- per-statement accounting ----

// fkDMLTargetName extracts the target's schema and table name from a lexed
// INSERT/UPDATE/DELETE ("INSERT [OR <action>] INTO [<db>.]<name>", "UPDATE [OR
// <action>] [<db>.]<name>", "DELETE FROM [<db>.]<name>") for Program.FKTargets;
// "" if unrecognized. The "<db>." matters: write compilers resolve a qualifier
// (sqlite3SrcListLookup, delete.c:31-46), so a qualified write must still get
// FKTargets.
//
// verbAt is the verb's index in toks (writeDispatchKeywordAt), since a leading
// WITH shifts it; reading toks[1] picked up a CTE's name and skipped
// fkCheckTargets. In C a WITH is only a sqlite3WithPush (parse.y:1955-1957,
// 991/1040/1082), and the same FK code is generated either way (delete.c:781,
// update.c:560).
func fkDMLTargetName(kw string, toks []token, verbAt int) (schema, table string) {
	i := verbAt + 1
	if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "OR" {
		i += 2 // OR <action>
	}
	switch kw {
	case "INSERT", "REPLACE":
		// "REPLACE INTO t" is "INSERT OR REPLACE INTO t" spelled short
		// (parse.y:1116 "insert_cmd ::= REPLACE."), so its target sits behind
		// the same INTO. Without this arm the walk hands back "INTO" as the
		// table name, findTableMetaIn misses, and the statement carries no
		// FKTargets at all -- the exact skipped-fkCheckTargets bug this
		// function's doc comment above describes twice already. It became
		// reachable the moment compileWriteProgram started compiling REPLACE
		// itself and dispatching it here.
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "INTO" {
			i++
		}
	case "DELETE":
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "FROM" {
			i++
		}
	}
	if i >= len(toks) || toks[i].kind != tkIdent {
		return "", ""
	}
	if i+2 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." && toks[i+2].kind == tkIdent {
		return toks[i].text, toks[i+2].text
	}
	return "", toks[i].text
}

// fkCheckTargets raises an unresolvable (or out-of-scope) foreign key of a
// write program's target table BEFORE the program runs. See Program.FKTargets.
func (db *DB) fkCheckTargets(prog *Program) error {
	if !db.fkEnforce || prog == nil {
		return nil
	}
	for _, tbl := range prog.FKTargets {
		if err := db.fkRequireResolvable(tbl, prog.FKReach); err != nil {
			return err
		}
	}
	return nil
}

// fkReach is which FK ACTIONS a statement can set off, and so how far
// fkRequireResolvable has to walk. C SQLite resolves a foreign key only
// where it actually GENERATES code for it, so this is not "everything nearby":
// a plain INSERT into a parent generates nothing at all (fkey.c: "Inserting a
// single row into a parent table cannot cause an immediate foreign key
// violation. So do nothing in this case."), which is why the same schema
// accepts "INSERT INTO t1 VALUES(9)" and rejects "DELETE FROM t1".
type fkReach uint8

const (
	fkReachDelete fkReach = 1 << iota // the statement can delete a parent key: ON DELETE actions
	fkReachUpdate                     // the statement can change one: ON UPDATE actions
)

// fkStatementReach maps a lexed write statement to the actions it can set off:
//
//	DELETE FROM t1          -> ON DELETE   (errors on a mismatch two hops away)
//	UPDATE t1 SET x=x       -> ON UPDATE   (accepted: t0's ON UPDATE is NO ACTION)
//	REPLACE INTO t1 SELECT..-> ON DELETE   (the victim delete's actions are compiled)
//	INSERT INTO t1 VALUES(9)-> nothing     (accepted, mismatch and all)
//
// verbAt is the verb's index (fkDMLTargetName); scanning from it keeps a WITH
// clause's body from being read as "INSERT OR REPLACE".
func fkStatementReach(kw string, toks []token, verbAt int) fkReach {
	switch kw {
	case "DELETE":
		return fkReachDelete
	case "UPDATE":
		return fkReachUpdate
	case "REPLACE":
		return fkReachDelete
	case "INSERT":
	default:
		return 0
	}
	var r fkReach
	// "OR REPLACE" can only be the two tokens after the verb
	// (parse.y:1114, 492, 495). Scanning for an OR/REPLACE pair matched
	// "... OR replace(a,'x','y')" (replace is a function) and widened the
	// reach, inventing a resolution error the oracle does not raise.
	if verbAt+2 < len(toks) &&
		toks[verbAt+1].kind == tkIdent && toks[verbAt+1].upper() == "OR" &&
		toks[verbAt+2].kind == tkIdent && toks[verbAt+2].upper() == "REPLACE" {
		// An inserted row can evict an existing one, whose ON DELETE actions
		// C SQLite then compiles.
		r |= fkReachDelete
	}
	for i := verbAt + 1; i < len(toks); i++ {
		if toks[i].kind != tkIdent {
			continue
		}
		switch toks[i].upper() {
		case "DO":
			// "ON CONFLICT ... DO UPDATE": an upsert can change a parent key.
			if i+1 < len(toks) && toks[i+1].kind == tkIdent && toks[i+1].upper() == "UPDATE" {
				r |= fkReachUpdate
			}
		}
	}
	return r
}

// fkRequireResolvable reports the error a DML statement on tbl must raise
// before running, walking as far as the statement's reach allows.
//
// Level 0, tbl's own keys, raises C's error unconditionally (a code-generation
// failure for any DML on the child).
//
// Deeper keys are reached through actions: C compiles an action as a program on
// the child, which resolves every key pointing at it, so a mismatch two hops
// away aborts the statement (fkey2.test 20150416-100):
//
//	CREATE TABLE t1(x PRIMARY KEY);
//	CREATE TABLE t(y REFERENCES t0(x) ON DELETE SET DEFAULT);  -- t0 has no x
//	CREATE TABLE t0(y REFERENCES t1 ON DELETE SET NULL);
//	DELETE FROM t1;   -- foreign key mismatch - "t" referencing "t0"
//
// Those are declined rather than raised: whether C raises depends on exactly
// which action programs it generates, which this approximates from the verb.
func (db *DB) fkRequireResolvable(tbl *tableMeta, reach fkReach) error {
	if !db.fkEnforce || tbl == nil {
		return nil
	}
	for _, fk := range db.fkOf(tbl) {
		if fk.resolveErr != nil {
			return fk.resolveErr
		}
		if fk.deferred && db.fkDeferredUnmodelled() {
			return fkDeferredDeclined(fk)
		}
	}
	if reach == 0 {
		return nil
	}
	seen := map[*tableMeta]fkReach{tbl: reach}
	queue := []*tableMeta{tbl}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		at := seen[u]
		for _, fk := range db.fkNamingParent(u) {
			if fk.resolveErr != nil {
				return fmt.Errorf("%w: %v -- reached from this statement through a foreign key action, where C SQLite raises it only for the action programs the statement actually generates",
					errVDBEUnsupported, fk.resolveErr)
			}
			if fk.deferred && db.fkDeferredUnmodelled() {
				return fkDeferredDeclined(fk)
			}
			// An ON DELETE CASCADE deletes child rows (so the child's own ON
			// DELETE actions follow); SET NULL/SET DEFAULT and every ON UPDATE
			// action rewrite them (so its ON UPDATE actions follow).
			var next fkReach
			if at&fkReachDelete != 0 {
				switch fk.onDelete {
				case fkCascade:
					next |= fkReachDelete
				case fkSetNull, fkSetDefault:
					next |= fkReachUpdate
				}
			}
			if at&fkReachUpdate != 0 && fkActionPerformsDML(fk.onUpdate) {
				next |= fkReachUpdate
			}
			if next == 0 || fk.child == nil {
				continue
			}
			if have, ok := seen[fk.child]; ok && have|next == have {
				continue
			}
			seen[fk.child] |= next
			queue = append(queue, fk.child)
		}
	}
	return nil
}

// fkActionPerformsDML reports whether an action rewrites child rows (and so
// makes C SQLite compile a program over the child table).
func fkActionPerformsDML(action string) bool {
	switch action {
	case fkCascade, fkSetNull, fkSetDefault:
		return true
	}
	return false
}

// fkBeginStatement resets the foreign key bookkeeping for one top-level write
// statement. A no-op unless enforcement is on.
func (db *DB) fkBeginStatement() {
	if !db.fkEnforce {
		db.fkStmt = nil
		return
	}
	db.fkStmt = &fkStmtState{deferredBefore: db.fkDeferred, deferredImmBefore: db.fkDeferredImm}
}

// fkRowMutated records one logical row mutation for foreign key accounting,
// which fkDrain applies after the write opcode that made it. It is called from every site that also calls noteRowChange
// (rowhook.go) -- the two together are the engine's complete set of logical
// row mutations. old/new are the row store's own slices (an INTEGER PRIMARY
// KEY column holding NULL); they are normalized against their OWN rowid and
// copied here, which is why both rowids are passed: an UPDATE that moves a row
// to a new rowid changes the very key the parent side compares. mask is the
// UPDATE's assigned-column mask, nil for an insert, a delete, or an update
// this engine treats as assigning everything.
func (db *DB) fkRowMutated(tbl *tableMeta, oldRowid, newRowid uint64, old, new []Value, mask []bool) {
	if db.fkStmt == nil || db.fkSchemaOf().empty {
		return
	}
	// Only a table that participates in some foreign key can affect the
	// counter, so an ordinary table in an FK-carrying schema stays free.
	childFKs, parentFKs := db.fkOf(tbl), db.fkReferencing(tbl)
	if len(childFKs) == 0 && len(parentFKs) == 0 {
		return
	}
	ev := fkEvent{
		tbl: tbl,
		old: copyHookRow(tbl, oldRowid, old),
		new: copyHookRow(tbl, newRowid, new),
		// A GENERATED column counts as assigned when its generator reads a
		// column the SET list assigned -- see fkWidenGeneratedMask.
		mask: fkWidenGeneratedMask(tbl, mask),
		// fkSuppressChild is set only while fkBeforeDropTable runs: that pass
		// does its OWN complete accounting before the parent table is removed,
		// and re-checking its child rewrites afterwards would try to resolve
		// the very foreign key whose parent has just ceased to exist.
		noChild: db.fkSuppressChild,
	}
	for _, fk := range parentFKs {
		if fk.resolveErr != nil {
			continue // a broken foreign key has no resolvable children to act on
		}
		w := fkParentWork{fk: fk}
		if k, ok := fkParentKeyOf(fk, ev.old); ok {
			w.oldKey = k
			w.oldKids = db.fkChildRowsMatching(fk, k)
		}
		if k, ok := fkParentKeyOf(fk, ev.new); ok {
			w.newKey = k
			w.newKids = len(db.fkChildRowsMatching(fk, k))
		}
		// RESTRICT is checked HERE, not at the end of the statement: it is the
		// one action C SQLite refuses to defer (see fkStmtState.restrict).
		if w.oldKey != nil && len(w.oldKids) > 0 && !fkKeysEqualOrNil(fk, w.oldKey, w.newKey, db.encoding()) {
			action := fk.onDelete
			if ev.new != nil {
				action = fk.onUpdate
			}
			if action == fkRestrict && !db.fkRestrictDisabled() {
				db.fkStmt.restrict = true
			}
		}
		ev.parent = append(ev.parent, w)
	}
	if ev.old == nil && ev.new != nil {
		if db.fkStmt.insertedInto == nil {
			db.fkStmt.insertedInto = map[*tableMeta]bool{}
		}
		db.fkStmt.insertedInto[tbl] = true
	}
	db.fkStmt.events = append(db.fkStmt.events, ev)
}

// fkKeysEqualOrNil is fkKeysEqual tolerating a nil (absent) second key, which
// never equals a present one.
func fkKeysEqualOrNil(fk *fkConstraint, a, b []Value, enc TextEncoding) bool {
	if b == nil {
		return false
	}
	return fkKeysEqual(fk, a, b, enc)
}

// fkColMask turns assigned column indices into fkRowMutated's mask. Its result
// is always non-nil: fkColMask(n, nil) is "this UPDATE assigned nothing", not
// the nil "not an UPDATE, check every key". SQLite draws the same line:
// sqlite3FkCheck's aChange is NULL for an insert (insert.c:1573) and the SET
// map for an update (update.c:1049), and a key is skipped only for a non-NULL
// aChange that does not name it (fkey.c:925, 1024).
// fkWidenGeneratedMask marks, in an UPDATE's mask, every generated column whose
// generator reads an assigned column, since that decides whether a key on the
// generated column is checked. The SET list can never name one, so C widens
// explicitly before sqlite3FkRequired (update.c:532-550):
//
//	do{
//	  bProgress = 0;
//	  for(i=0; i<pTab->nCol; i++){
//	    if( aXRef[i]>=0 ) continue;
//	    if( (pTab->aCol[i].colFlags & COLFLAG_GENERATED)==0 ) continue;
//	    if( sqlite3ExprReferencesUpdatedColumn(
//	            sqlite3ColumnExpr(pTab, &pTab->aCol[i]), aXRef, chngRowid) ){
//	      aXRef[i] = 99999;
//	      bProgress = 1;
//	    }
//	  }
//	}while( bProgress );
//
// a fixpoint, since one generated column may read another. Over "c(k INTEGER
// PRIMARY KEY, x, y AS (x*2) REFERENCES p(k))" with p holding 2 and 4, "UPDATE
// c SET x = 3" fails. Marking every generated column would be wrong the other
// way.
func fkWidenGeneratedMask(tbl *tableMeta, mask []bool) []bool {
	if len(mask) == 0 || !hasGeneratedCols(tbl.cols) {
		return mask
	}
	idx := buildColIndex(tbl.cols)
	out, copied := mask, false
	for progress := true; progress; {
		progress = false
		for i, c := range tbl.cols {
			if i >= len(out) || out[i] || !c.IsGenerated() {
				continue
			}
			expr, err := cachedGeneratedExpr(c.GeneratedExpr)
			if err != nil {
				continue // an unparsable generator is reported by whoever evaluates it
			}
			reads := false
			// wherePlanWalkColumns is TOTAL: a node it does not recognise
			// reports itself as ColumnExpr{} with an EMPTY name. A generated
			// column's expression cannot hold a subquery (SQLite rejects one
			// at CREATE TABLE), so that is unreachable here -- but if it ever
			// is reached, treating it as a read is the direction that keeps
			// the check running rather than silently dropping it.
			wherePlanWalkColumns(expr, func(ce ColumnExpr) bool {
				if ce.Name == "" {
					reads = true
					return false
				}
				if j, ok := idx[r33sFoldIdent(ce.Name)]; ok && j < len(out) && out[j] {
					reads = true
					return false
				}
				return true
			})
			if !reads {
				continue
			}
			if !copied {
				out, copied = append([]bool(nil), mask...), true
			}
			out[i] = true
			progress = true
		}
	}
	return out
}

func fkColMask(ncols int, colIdx []int) []bool {
	m := make([]bool, ncols)
	for _, ci := range colIdx {
		if ci >= 0 && ci < ncols {
			m[ci] = true
		}
	}
	return m
}

// fkFinishStatement drains whatever the per-row drain has not reached
// (fkStatementPass below) and then applies whichever of the two checks this statement is due:
// the IMMEDIATE one always, and the DEFERRED one only in autocommit, where the
// statement is its own transaction and its implicit commit happens right here.
// Inside an explicit BEGIN the deferred counter simply carries forward to
// commitTxn.
//
// Any error means the statement is UNWOUND, so the deferred counter -- which
// is transaction state, not statement state -- is put back exactly as it was
// found. C SQLite does the same through its statement journal
// (vdbeCloseStatement restores nStmtDefCons).
func (db *DB) fkFinishStatement() error {
	// See db.pendingLoadErr's own doc comment: a parent/child table this
	// statement's own FK checks needed (not its own DML target, already
	// gated at its own choke point) failed to load -- report that now rather
	// than trusting whatever fkParentHasKey/fkChildRowsMatching computed
	// against a table they could not actually read.
	if db.pendingLoadErr != nil {
		err := db.pendingLoadErr
		db.pendingLoadErr = nil
		return err
	}
	st := db.fkStmt
	if st == nil {
		return nil
	}
	if err := db.fkStatementPass(st); err != nil {
		db.fkDeferred, db.fkDeferredImm = st.deferredBefore, st.deferredImmBefore
		return err
	}
	if db.transactionHasACommitCheck() {
		return nil // checked at COMMIT instead -- fkCommitCheck
	}
	// AUTOCOMMIT: the implicit transaction ends with this statement, so the
	// deferred counters are checked and cleared here -- sqlite3VdbeHalt does
	// exactly this, and only, when the statement leaves autocommit ON.
	// fkDeferredImm can only be non-zero inside a transaction of some kind
	// ("PRAGMA defer_foreign_keys"' setter is declined in autocommit, see
	// execPragma), but it is summed and cleared with its sibling regardless,
	// since sqlite3VdbeHalt makes no distinction either.
	pending := db.fkDeferred + db.fkDeferredImm
	db.fkDeferred, db.fkDeferredImm = 0, 0
	if pending != 0 {
		return fkErrViolation()
	}
	return nil
}

// fkStatementPass is fkFinishStatement's body: it walks the recorded mutations
// in order, fires the parent-side ACTIONS (whose own row mutations join the
// same queue, which is what makes a cascade chain and a self-referencing
// cascade converge), maintains the violation counters, and finally reports a
// violation if the IMMEDIATE one came out positive.
//
// Any error it returns means the statement must be UNWOUND -- including the
// action mutations this function itself applied, which is why they are pushed
// onto the same statement journal.
func (db *DB) fkStatementPass(st *fkStmtState) error {
	if err := db.fkDrain(st); err != nil {
		return err
	}
	if st.counter > 0 {
		return fkErrViolation()
	}
	return nil
}

// fkDrain processes every event recorded since the last drain: parent-side
// actions (whose own mutations join the queue, so cascade chains and
// self-references converge) and both sides' counters.
//
// It runs after each row a write opcode stores, as C codes a row's FK checks
// around the write (update.c:1049, :1093) and its actions right after
// ("sqlite3FkActions(...)", update.c:1107), before the row's AFTER triggers
// (update.c:1118):
//
//	CREATE TABLE ch(id INTEGER PRIMARY KEY, pk REFERENCES p(k) ON UPDATE CASCADE);
//	CREATE TRIGGER tp AFTER UPDATE ON p BEGIN INSERT INTO lg SELECT count(*) FROM ch WHERE pk > 10; END;
//	UPDATE p SET k = k + 10;     -- over p(1,2,3), ch pointing at each
//
// logs 1,2,3. Only the immediate counter's verdict waits for statement end
// (fkStatementPass), as C checks at OP_Halt (sqlite3VdbeCheckFkImmediate).
func (db *DB) fkDrain(st *fkStmtState) error {
	if st == nil || st.draining {
		return nil
	}
	st.draining = true
	defer func() { st.draining = false }()
	// ponytail: the event queue is bounded only by the schema's own cascade
	// structure. A cycle terminates because each pass removes or NULLs rows,
	// but a pathological schema could still grow it without bound, and a
	// runaway is a hang rather than a wrong answer -- so it is capped and
	// declined. Raise the cap if a real workload ever reaches it.
	const maxEvents = 1 << 20
	if st.restrict {
		return fkErrViolation()
	}
	for ; st.next < len(st.events); st.next++ {
		if st.next >= maxEvents {
			return fmt.Errorf("%w: foreign key actions did not converge within %d row mutations", errVDBEUnsupported, maxEvents)
		}
		ev := st.events[st.next]
		if err := db.fkParentSide(st, ev); err != nil {
			return err
		}
		if st.restrict {
			return fkErrViolation()
		}
		if !ev.noChild {
			if err := db.fkChildSide(st, ev); err != nil {
				return err
			}
		}
	}
	return nil
}

// fkCounterFor returns the counter fk's violations belong in: the statement's
// (immediate) or the transaction's (deferred), as OP_FkCounter chooses; its
// OP_FkIfZero reads the same counter. They must be separate even in
// autocommit, or a deferred decrement could cancel an immediate violation.
//
// "PRAGMA defer_foreign_keys" adds a third bucket, nested so:
//
//	if( pOp->p1 ){                              // the CONSTRAINT is deferred
//	  db->nDeferredCons += pOp->p2;
//	}else if( db->flags & SQLITE_DeferFKs ){
//	  db->nDeferredImmCons += pOp->p2;
//	}else{
//	  p->nFkConstraint += pOp->p2;
//	}
//
// so a DEFERRABLE INITIALLY DEFERRED key still counts in nDeferredCons with the
// flag on; the flag redirects only immediate keys.
func (db *DB) fkCounterFor(st *fkStmtState, fk *fkConstraint) *int {
	if fk.deferred {
		return &db.fkDeferred
	}
	if db.deferFKs {
		return &db.fkDeferredImm
	}
	return &st.counter
}

// fkMayDecrement is OP_FkIfZero: it permits the decrement when the
// constraint's own counter (the statement's, or nDeferredCons) or
// nDeferredImmCons is non-zero:
//
//	p1  ->  jump (skip) if db->nDeferredCons==0 && db->nDeferredImmCons==0
//	!p1 ->  jump (skip) if p->nFkConstraint==0 && db->nDeferredImmCons==0
//
// With defer_foreign_keys off, fkDeferredImm is 0 and this is the plain
// single-counter test.
func (db *DB) fkMayDecrement(st *fkStmtState, fk *fkConstraint) bool {
	own := st.counter
	if fk.deferred {
		own = db.fkDeferred
	}
	return own != 0 || db.fkDeferredImm != 0
}

// fkChildSide accounts one mutation of a CHILD table: the new row image's key
// must resolve to a parent row, and removing an old image that did not
// resolve gives the counter back (never below zero -- see the doc comment).
func (db *DB) fkChildSide(st *fkStmtState, ev fkEvent) error {
	for _, fk := range db.fkOf(ev.tbl) {
		if fk.resolveErr != nil {
			return fk.resolveErr
		}
		if fk.deferred && db.fkDeferredUnmodelled() {
			return fkDeferredDeclined(fk)
		}
		// An UPDATE consults a foreign key only when it ASSIGNS one of that
		// key's own child columns -- not when the VALUES happen to change.
		if ev.mask != nil && !fkAssignsChildCols(fk, ev.mask) {
			continue
		}
		counter := db.fkCounterFor(st, fk)
		if ev.old != nil && db.fkMayDecrement(st, fk) {
			if key, ok := fkKeyOf(fk.childIdx, ev.old); ok && !db.fkParentHasKey(fk, key) {
				*counter--
			}
		}
		if ev.new != nil {
			if key, ok := fkKeyOf(fk.childIdx, ev.new); ok && !db.fkParentHasKey(fk, key) {
				*counter++
			}
		}
	}
	return nil
}

// fkParentSide applies one mutation's parent-side work: it counts the child
// rows a removed or moved parent key orphaned, for every action kind, and then
// fires the action, whose child mutations decrement through fkChildSide.
//
// That is fkey.c's order: sqlite3GenerateRowDelete counts (sqlite3FkCheck),
// deletes, then runs sqlite3FkActions. With SET NULL, SET DEFAULT (default 77,
// no parent 77) and SET DEFAULT (no default) children of one parent row,
// deleting it counts 3, the actions give back 3, and 77 adds 1: the statement
// fails. Counting only NO ACTION keys would let the zero rule swallow it.
func (db *DB) fkParentSide(st *fkStmtState, ev fkEvent) error {
	for _, w := range ev.parent {
		fk := w.fk
		if fk.deferred && db.fkDeferredUnmodelled() {
			return fkDeferredDeclined(fk)
		}
		counter := db.fkCounterFor(st, fk)
		// A key the mutation did not change acts on nothing and counts nothing
		// -- fkey.c's +1 for the old key and -1 for the new one cancel exactly.
		// Verified: "UPDATE p SET id=id", "UPDATE p SET id=1" (the value it
		// already had) and "UPDATE p SET v='b'" all leave an ON UPDATE SET NULL
		// child alone.
		if w.oldKey != nil && !fkKeysEqualOrNil(fk, w.oldKey, w.newKey, db.encoding()) && len(w.oldKids) > 0 {
			*counter += len(w.oldKids)
			action := fk.onDelete
			if ev.new != nil {
				action = fk.onUpdate
			}
			if action == fkRestrict && db.fkRestrictDisabled() {
				// "PRAGMA defer_foreign_keys" turns RESTRICT into ordinary
				// deferred counting -- fkActionTrigger generates no trigger for
				// it at all. The +len(oldKids) above has already been booked
				// into fkDeferredImm, which is what the COMMIT then checks.
				action = fkNoAction
			}
			switch action {
			case fkRestrict:
				st.restrict = true
				return nil
			case fkCascade, fkSetNull, fkSetDefault:
				// C fires an action the moment the parent row is touched, so an
				// "INSERT OR REPLACE" whose victim delete cascades does so before the
				// replacement row is stored; here actions run after the write opcode
				// (fkDrain). The two differ only when the action acts on a table the
				// statement also inserted into:
				//
				//	CREATE TABLE t11(x INTEGER PRIMARY KEY,
				//	                 parent REFERENCES t11 ON DELETE CASCADE);
				//	INSERT INTO t11 VALUES(1,NULL),(2,1),(3,2);
				//	INSERT OR REPLACE INTO t11 VALUES(2,3);
				//
				// C fails FOREIGN KEY constraint failed (fkey1-5.2); declined here.
				if st.insertedInto[fk.child] {
					return fmt.Errorf("%w: foreign key action %s on %s, which this statement also inserts into (C SQLite fires the action before the new row is stored; this engine runs actions after the write that stores the new row)",
						errVDBEUnsupported, action, fk.child.name)
				}
				if err := db.fkApplyAction(fk, w.oldKids, action, ev.new); err != nil {
					return err
				}
			}
		}
		// A key the mutation BROUGHT INTO EXISTENCE resolves outstanding
		// violations -- never below zero. For a DEFERRED constraint this is
		// what lets a later statement in the transaction repair an earlier
		// one's violation: verified directly, "BEGIN; INSERT an orphan child;
		// COMMIT" fails, and inserting the missing PARENT row after that
		// failed COMMIT makes the very next COMMIT succeed -- while inserting
		// an UNRELATED parent key does not.
		if w.newKey != nil && w.newKids > 0 && db.fkMayDecrement(st, fk) {
			*counter -= w.newKids
			if *counter < 0 {
				*counter = 0
			}
		}
	}
	return nil
}

// fkDeferredDeclined is the clean decline for a DEFERRABLE INITIALLY DEFERRED
// constraint inside a transaction a DRIVER is holding open -- see
// fkDeferredUnmodelled. An engine-level BEGIN and autocommit are both served.
func fkDeferredDeclined(fk *fkConstraint) error {
	return fmt.Errorf("%w: DEFERRABLE INITIALLY DEFERRED foreign key on %s inside a driver-held transaction (C SQLite checks it at COMMIT and a failed COMMIT leaves the transaction open, which this driver commits-by-closing-the-session; an engine-level BEGIN and autocommit are both enforced)",
		errVDBEUnsupported, fk.child.name)
}

// fkAssignsChildCols reports whether an UPDATE's assigned-column mask covers
// any of fk's own child columns.
func fkAssignsChildCols(fk *fkConstraint, mask []bool) bool {
	for _, ci := range fk.childIdx {
		if ci < len(mask) && mask[ci] {
			return true
		}
	}
	return false
}

// fkKeyOf extracts the key tuple at idx out of a row image, reporting ok
// false when ANY column is NULL -- MATCH SIMPLE, the only match mode SQLite
// implements, satisfies such a row vacuously.
func fkKeyOf(idx []int, row []Value) ([]Value, bool) {
	key := make([]Value, 0, len(idx))
	for _, ci := range idx {
		if ci >= len(row) || row[ci].Typ == Null {
			return nil, false
		}
		key = append(key, row[ci])
	}
	return key, true
}

// fkAnyRowHasFullKey reports whether any row of tbl carries a wholly non-NULL
// value for fk's CHILD key columns -- C SQLite's "row with non-NULL keys",
// the rows a missing parent table makes the implicit delete count (see
// fkBeforeDropTable). A partly-NULL key satisfies a foreign key vacuously and
// moves no counter, which is the same rule fkKeyOf already applies everywhere
// else.
func fkAnyRowHasFullKey(tbl *tableMeta, fk *fkConstraint) bool {
	for rid, raw := range tbl.rows.all() {
		if _, ok := fkKeyOf(fk.childIdx, normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, raw)); ok {
			return true
		}
	}
	return false
}

// fkParentKeyOf extracts a parent row image's key tuple, resolving the rowid
// sentinel (see fkConstraint.parentIdx). A normalized row image already
// carries the rowid in its INTEGER PRIMARY KEY slot, so the sentinel reads
// that column.
func fkParentKeyOf(fk *fkConstraint, row []Value) ([]Value, bool) {
	if row == nil {
		return nil, false
	}
	key := make([]Value, 0, len(fk.parentIdx))
	for _, pi := range fk.parentIdx {
		if pi < 0 {
			pi = fk.parent.ipkIndex
		}
		if pi < 0 || pi >= len(row) || row[pi].Typ == Null {
			return nil, false
		}
		key = append(key, row[pi])
	}
	return key, true
}

// fkParentKeyRaw is fkParentKeyOf without NULL rejection: the parent row's
// actual key values, NULL included. CASCADE writes those values into the child
// (ON UPDATE CASCADE propagates a NULL component and the child survives),
// while fkParentKeyOf's "any NULL means absent" rule is for matching (MATCH
// SIMPLE). A nil row means a real parent DELETE, where CASCADE deletes the
// child; fkParentKeyOf is nil in both cases, so it cannot tell them apart.
func fkParentKeyRaw(fk *fkConstraint, row []Value) []Value {
	if row == nil {
		return nil
	}
	key := make([]Value, len(fk.parentIdx))
	for i, pi := range fk.parentIdx {
		if pi < 0 {
			pi = fk.parent.ipkIndex
		}
		if pi < 0 || pi >= len(row) {
			key[i] = Value{Typ: Null}
			continue
		}
		key[i] = row[pi]
	}
	return key
}

// fkKeysEqual compares two PARENT key tuples under the parent key's own
// collating sequences -- the comparison that decides whether an ON UPDATE
// action fires at all.
func fkKeysEqual(fk *fkConstraint, a, b []Value, enc TextEncoding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if compareValuesCollatedEnc(a[i], b[i], fk.colls[i], enc) != 0 {
			return false
		}
	}
	return true
}

// fkParentHasKey reports whether the parent holds a row whose key equals the
// child's key, compared as an index seek: parent affinity applied to the child
// value, parent collation (fkCheckParentHas). C seeks by rowid or a UNIQUE
// index (sqlite3FkLocateIndex, fkey.c:183); fkParentCandidates narrows from
// the row store, each candidate re-checked by fkParentRowMatches, and only an
// unnarrowable key scans.
func (db *DB) fkParentHasKey(fk *fkConstraint, key []Value) bool {
	// See table_load.go's package doc comment, and db.pendingLoadErr's own doc
	// comment for why a load failure here is recorded rather than returned:
	// fk.parent is a DIFFERENT table than the statement's own DML target, the
	// one case a DML's own target-resolution choke point does not cover.
	if err := db.ensureTableLoaded(fk.parent); err != nil {
		if db.pendingLoadErr == nil {
			db.pendingLoadErr = err
		}
		return false
	}
	probe := make([]Value, len(key))
	for i := range key {
		probe[i] = applyAffinityToValue(key[i], fk.affs[i])
	}
	if cands, ok := db.fkParentCandidates(fk, probe); ok {
		for _, rid := range cands {
			if prow, have := fk.parent.rows.get(rid); have && fkParentRowMatches(fk, rid, prow, probe, db.encoding()) {
				return true
			}
		}
		return false
	}
	for rid, prow := range fk.parent.rows.all() {
		if fkParentRowMatches(fk, rid, prow, probe, db.encoding()) {
			return true
		}
	}
	return false
}

// fkParentCandidates narrows fkParentHasKey's search to the parent rows that
// may hold probe, or reports false when it cannot and the parent must be
// scanned. It only prunes; the caller re-checks every candidate.
//
// A probe with a NULL component is left to the scan, which is what decides it
// today (compareValuesCollatedEnc calls two NULLs equal).
func (db *DB) fkParentCandidates(fk *fkConstraint, probe []Value) ([]uint64, bool) {
	parent := fk.parent
	if parent.rows == nil || len(probe) != len(fk.parentIdx) {
		return nil, false
	}
	for _, v := range probe {
		if v.Typ == Null {
			return nil, false
		}
	}
	// The rowid: one lookup. Only an Int probe is keyed this way; anything else
	// (a Float, text) is left to the scan rather than reasoned about here.
	if len(probe) == 1 && !parent.withoutRowid && (fk.parentIdx[0] < 0 || fk.parentIdx[0] == parent.ipkIndex) {
		if probe[0].Typ != Int {
			return nil, false
		}
		return []uint64{uint64(probe[0].I)}, true
	}
	// A UNIQUE index over exactly the key's columns, in order, under the same
	// collations: its conflict index (row_store_uniqindex.go) keys equal values
	// alike under those collations, which is the only property pruning needs.
	for _, idx := range db.indexes {
		if !idx.unique || idx.exprOrPartial || !indexBelongsTo(idx, parent) || !slices.Equal(idx.colIdx, fk.parentIdx) {
			continue
		}
		same := true
		for i := range idx.colIdx {
			if !asciiEqualFold(effectiveCollation(atOrEmpty(idx.colCollation, i)), effectiveCollation(fk.colls[i])) {
				same = false
				break
			}
		}
		if !same {
			continue
		}
		return parent.rows.uniqConflictCandidates(parent, idx, probe, db.encoding())
	}
	return nil, false
}

// fkChildRowsMatching returns the rowids of every child row whose foreign key
// value tuple equals the given PARENT key, in ascending rowid order (the
// order the child's own b-tree -- and so C SQLite's action loop -- visits
// them in).
func (db *DB) fkChildRowsMatching(fk *fkConstraint, parentKey []Value) []uint64 {
	// See fkParentHasKey's identical guard just above.
	if err := db.ensureTableLoaded(fk.child); err != nil {
		if db.pendingLoadErr == nil {
			db.pendingLoadErr = err
		}
		return nil
	}
	var out []uint64
	for rid, crow := range fk.child.rows.all() {
		row := normalizeRow(fk.child.name, fk.child.cols, fk.child.ipkIndex, rid, crow)
		key, ok := fkKeyOf(fk.childIdx, row)
		if !ok {
			continue
		}
		match := true
		for i := range key {
			if compareValuesCollatedEnc(applyAffinityToValue(key[i], fk.affs[i]), parentKey[i], fk.colls[i], db.encoding()) != 0 {
				match = false
				break
			}
		}
		if match {
			out = append(out, rid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return rowidLess(out[i], out[j]) })
	return out
}

// fkParentRowMatches compares one stored parent row against an
// already-affinity-applied probe key.
func fkParentRowMatches(fk *fkConstraint, rid uint64, prow []Value, probe []Value, enc TextEncoding) bool {
	for i, pi := range fk.parentIdx {
		var pv Value
		if pi < 0 {
			pv = Value{Typ: Int, I: int64(rid)}
		} else {
			if pi >= len(prow) {
				return false
			}
			pv = prow[pi]
			// A rowid table stores NULL in its INTEGER PRIMARY KEY slot; the
			// rowid carries the value (see tableMeta.rows).
			if pi == fk.parent.ipkIndex {
				pv = Value{Typ: Int, I: int64(rid)}
			}
		}
		if compareValuesCollatedEnc(probe[i], pv, fk.colls[i], enc) != 0 {
			return false
		}
	}
	return true
}

// ---- actions ----

// fkApplyAction rewrites the child rows a vanished parent key orphaned, per
// fk's ON DELETE / ON UPDATE action. Each write goes through the child's
// constraints (NOT NULL, CHECK, STRICT, UNIQUE), is journaled, and is recorded
// as a mutation for the counter.
//
// Rewritten rows count in total_changes() but not the statement's changes():
// C runs an action as a sub-program like a trigger body ("DELETE FROM p WHERE
// id=1" with two cascaded children: changes()==1, total_changes() +3).
// newParentRow is the parent's post-write row (ev.new), or nil for a DELETE;
// it is passed whole because a NULL key component and a delete need different
// actions (fkParentKeyRaw).
func (db *DB) fkApplyAction(fk *fkConstraint, kids []uint64, action string, newParentRow []Value) error {
	child := fk.child
	// newKey preserves NULL components -- unlike fkParentKeyOf's key, which
	// collapses to nil (indistinguishable from "no row at all") the moment any
	// column is NULL. CASCADE needs the actual per-column values to propagate.
	newKey := fkParentKeyRaw(fk, newParentRow)
	isDelete := action == fkCascade && newKey == nil
	// The child's own triggers fire around the implicit DELETE/UPDATE: in C an
	// action is a trigger program (fkActionTrigger) whose step is an ordinary
	// DELETE/UPDATE on the child. firePlanForRow takes OLD/NEW directly. Over
	// par(1,2) and ch(10->1, 11->1, 12->2) with ON DELETE/UPDATE CASCADE:
	//
	//	AFTER DELETE on ch,  "DELETE FROM par WHERE id=1"  -> fires twice,
	//	                     OLD.(id,pid) = (10,1) then (11,1)
	//	AFTER UPDATE on ch,  "UPDATE par SET id=99 ..."    -> fires twice,
	//	                     (OLD.pid,NEW.pid) = (1,99) both times
	//	ON DELETE SET NULL                                 -> fires the UPDATE
	//	                     triggers, NEW.pid NULL
	//	AFTER DELETE reading ch -> count(*) DECREASES between the two fires, so
	//	                     the action is per-row and the AFTER runs after that
	//	                     row's own write
	//
	// UPDATE triggers use the "UPDATE OF" gate with the key's child columns as the
	// SET list (compileUpdateTriggerFirePlan).
	memo := newTriggerPrgMemo()
	var beforePlan, afterPlan *triggerFirePlan
	var perr error
	if isDelete {
		if beforePlan, perr = db.compileTriggerFirePlan(child, triggerDelete, triggerBefore, memo); perr != nil {
			return perr
		}
		if afterPlan, perr = db.compileTriggerFirePlan(child, triggerDelete, triggerAfter, memo); perr != nil {
			return perr
		}
	} else {
		setCols := make([]string, 0, len(fk.childIdx))
		for _, ci := range fk.childIdx {
			setCols = append(setCols, child.cols[ci].Name)
		}
		if beforePlan, perr = db.compileUpdateTriggerFirePlan(child, triggerBefore, memo, conflictAbort, false, setCols); perr != nil {
			return perr
		}
		if afterPlan, perr = db.compileUpdateTriggerFirePlan(child, triggerAfter, memo, conflictAbort, false, setCols); perr != nil {
			return perr
		}
	}
	// One ordering is declined: C codes an action's nested actions before
	// the row's AFTER trigger (sqlite3GenerateRowDelete: sqlite3FkActions,
	// then the AFTER sqlite3CodeRowTrigger), so cascade chains interleave
	// depth-first. With par -> ch -> gc each carrying an AFTER DELETE
	// trigger, "DELETE FROM par WHERE id=1":
	//
	//	oracle   gc 100, ch 10, gc 101, ch 11     depth-first per child row
	//	here     ch 10, ch 11, gc 100, gc 101     this level, then the next
	//
	// The final state matches but the bodies' view does not. Fixing it needs
	// the nested level drained inline between the write and its AFTER fire.
	if afterPlan != nil {
		reach := fkReachDelete
		if !isDelete {
			reach = fkReachUpdate
		}
		for _, nested := range db.fkNamingParent(child) {
			act := nested.onDelete
			if reach == fkReachUpdate {
				act = nested.onUpdate
			}
			if fkActionPerformsDML(act) {
				return fmt.Errorf("%w: foreign key action %s on %s has an AFTER trigger and its own onward action to %s -- C SQLite runs the onward action BEFORE that trigger (sqlite3GenerateRowDelete codes sqlite3FkActions ahead of the AFTER block), which this engine drains one level at a time",
					errVDBEUnsupported, action, child.name, nested.child.name)
			}
		}
	}
	fire := func(plan *triggerFirePlan, newRow, oldRow []Value, rid uint64) error {
		if plan == nil {
			return nil
		}
		// NEW.rowid is the child's new key when that key is its INTEGER PRIMARY
		// KEY (see fkStoreChildRow).
		nrid := rid
		if child.ipkIndex >= 0 && child.ipkIndex < len(newRow) && newRow[child.ipkIndex].Typ == Int {
			nrid = uint64(newRow[child.ipkIndex].I)
		}
		return (&vdbe{wctx: db.activeWC}).firePlanForRow(plan, newRow, int64(nrid), oldRow, int64(rid))
	}
	if (beforePlan != nil || afterPlan != nil) && db.activeWC == nil {
		// firePlanForRow reads wctx unconditionally, and a nil one is a PANIC,
		// which AGENTS.md invariant 2 ranks as the hardest failure. Every
		// production caller is inside a statement's own write context; this says
		// so rather than trusting it.
		return fmt.Errorf("%w: foreign key action %s on %s fires a trigger with no active write context",
			errVDBEUnsupported, action, child.name)
	}
	nextRowFor := func(old []Value) []Value {
		next := append([]Value(nil), old...)
		for i, ci := range fk.childIdx {
			switch action {
			case fkCascade:
				next[ci] = applyAffinityToValue(newKey[i], child.cols[ci].Aff)
			case fkSetNull:
				next[ci] = Value{Typ: Null}
			case fkSetDefault:
				next[ci] = fkColumnDefault(child.cols[ci])
			}
		}
		return next
	}
	changed := 0
	defer func() { db.setChanges(int64(changed)) }()
	for _, rid := range kids {
		stored := child.rows.row(rid)
		if stored == nil {
			continue // already removed by an earlier action in this statement
		}
		old := normalizeRow(child.name, child.cols, child.ipkIndex, rid, stored)
		var next []Value
		if !isDelete {
			next = nextRowFor(old)
		}
		if err := fire(beforePlan, next, old, rid); err != nil {
			return err
		}
		// A BEFORE trigger can DELETE or rewrite the very row the action is about
		// to touch -- verified: a "BEFORE DELETE ON ch BEGIN DELETE FROM ch WHERE
		// id = OLD.id" still logs every cascaded row and leaves them gone -- so
		// the image is re-read rather than carried across the fire.
		if beforePlan != nil {
			if stored = child.rows.row(rid); stored == nil {
				changed++
				continue
			}
			old = normalizeRow(child.name, child.cols, child.ipkIndex, rid, stored)
			if !isDelete {
				next = nextRowFor(old)
			}
		}
		if isDelete {
			db.fkDeleteChildRow(child, rid, stored, old)
		} else if err := db.fkStoreChildRow(child, fk.childIdx, rid, stored, old, next); err != nil {
			return err
		}
		if err := fire(afterPlan, next, old, rid); err != nil {
			return err
		}
		changed++
	}
	return nil
}

// fkColumnDefault is the value ON DELETE/UPDATE SET DEFAULT writes: the
// column's DEFAULT clause folded value, or NULL when it has none (verified:
// "y REFERENCES p ON DELETE SET DEFAULT" with no DEFAULT clause becomes NULL
// and so satisfies the constraint vacuously).
func fkColumnDefault(col columnInfo) Value {
	if col.HasDefault && col.DefaultKnown {
		return applyAffinityToValue(col.DefaultValue, col.Aff)
	}
	return Value{Typ: Null}
}


// fkDeleteChildRow removes one child row on behalf of an ON DELETE CASCADE.
func (db *DB) fkDeleteChildRow(child *tableMeta, rid uint64, stored, old []Value) {
	db.noteRowChange(child, RowDelete, rid, stored, nil)
	child.dropRow(rid)
	db.fkJournal(func() { child.putRow(rid, stored) })
	// Recorded through the ordinary mutation seam, not a bare event: a
	// cascaded delete is itself a parent-side mutation for anything
	// referencing THIS table, which is what makes a cascade CHAIN (a -> b ->
	// c) and a self-referencing cascade reach the end.
	db.fkRowMutated(child, rid, rid, stored, nil, nil)
	_ = old
}

// fkStoreChildRow rewrites one child row on behalf of a SET NULL / SET
// DEFAULT / ON UPDATE CASCADE action, running the child's own constraints
// first (C SQLite reports "NOT NULL constraint failed: c.y" for an ON
// DELETE SET NULL onto a NOT NULL column, and "CHECK constraint failed: ..."
// for a CHECK -- both verified directly).
//
// childIdx is the action's assigned columns -- the foreign key's own child
// columns, and nothing else. C SQLite codes the action as an implicit
// UPDATE of exactly those, so its CHECKs follow the UPDATE rule (only a
// constraint naming one of them is re-evaluated); see checkChangeSet.
func (db *DB) fkStoreChildRow(child *tableMeta, childIdx []int, rid uint64, stored, old, next []Value) error {
	for i, c := range child.cols {
		if i == child.ipkIndex || c.IsGenerated() {
			continue
		}
		if c.NotNull && next[i].Typ == Null {
			return fmt.Errorf("engine: NOT NULL constraint failed: %s.%s", child.name, c.Name)
		}
	}
	if err := db.checkTableChecksChanged(child, child.name, rid, next, buildChangeSet(child, childIdx)); err != nil {
		return err
	}
	if err := checkStrictColumnTypes(child, next); err != nil {
		return err
	}
	if err := computeGeneratedInto(child.name, child.cols, next); err != nil {
		return err
	}
	// The action is an implicit UPDATE of the child, so a key that IS the
	// child's INTEGER PRIMARY KEY moves the row: OP_MustBeInt on the new rowid
	// (update.c:894), then sqlite3RowidConstraint on a collision (build.c).
	newRid := rid
	if child.ipkIndex >= 0 {
		v := applyAffinityToValue(next[child.ipkIndex], affInteger)
		if v.Typ == Float && v.F == float64(int64(v.F)) {
			v = Value{Typ: Int, I: int64(v.F)}
		}
		if v.Typ != Int {
			return fmt.Errorf("engine: datatype mismatch")
		}
		next[child.ipkIndex] = v
		newRid = uint64(v.I)
		if _, exists := child.rows.get(newRid); exists && newRid != rid {
			return fmt.Errorf("engine: UNIQUE constraint failed: %s.%s", child.name, child.cols[child.ipkIndex].Name)
		}
	}
	toStore := append([]Value(nil), next...)
	if child.ipkIndex >= 0 {
		toStore[child.ipkIndex] = Value{Typ: Null}
	}
	if newRid != rid {
		child.dropRow(rid)
	}
	child.putRow(newRid, toStore)
	db.fkJournal(func() {
		child.dropRow(newRid)
		child.putRow(rid, stored)
	})
	if err := db.checkUniqueIndexesForRow(child, newRid, toStore); err != nil {
		return err
	}
	db.noteRowUpdate(child, rid, newRid, stored, toStore)
	// See fkDeleteChildRow: recorded through the mutation seam so an ON
	// UPDATE CASCADE that moves a key propagates to ITS children too.
	db.fkRowMutated(child, rid, newRid, stored, toStore, nil)
	_ = old
	return nil
}

// fkJournal pushes an undo closure onto the statement journal of whichever
// write statement is running, so a later failure -- including this file's own
// end-of-statement violation -- unwinds an action's writes too. A nil active
// context means no statement journal exists (the FK paths a DDL statement
// drives, see fkBeforeDropTable), in which case the DDL's own unwind covers
// it.
func (db *DB) fkJournal(undo func()) {
	if db.activeWC != nil {
		db.activeWC.journal = append(db.activeWC.journal, undo)
	}
}

// ---- DROP TABLE ----

// fkBeforeDropTable applies DROP TABLE's implicit "DELETE FROM parent" for
// foreign key purposes: dropping a parent fires the children's ON DELETE
// actions and fails with FOREIGN KEY constraint failed if a child would be
// left orphaned. A SELF-reference is exempt (a self-referencing table drops
// cleanly, verified), and the parent's OWN delete triggers do NOT fire --
// only the child's, which is why an action that would fire one is declined
// exactly as everywhere else in this file.
func (db *DB) fkBeforeDropTable(tbl *tableMeta) error {
	if !db.fkEnforce {
		return nil
	}
	// C compiles the implicit delete, triggers included, even though it does
	// not fire the parent's delete triggers and even for an empty table, so a
	// trigger that no longer compiles makes the DROP fail (without_rowid3.test
	// #293, fkey2.test #304: "no such column: old.x"). So the decline is "a
	// trigger body does not compile", checked with checkSchemaObjectsResolve
	// (alter_write.go); a healthy trigger is accepted and does not fire
	// (e_fkey.test R-11078-03945, 57.1-57.5):
	//
	//	CREATE TRIGGER tt AFTER  DELETE ON p BEGIN INSERT INTO log ...; END
	//	                          DROP TABLE p -> ACCEPTED, log EMPTY, c emptied
	//
	// An unresolvable key naming this table as parent is a no-op for the drop:
	// C codes the DROP with pParse->disableTriggers (sqlite3FkDropTable), which
	// suppresses "foreign key mismatch" in sqlite3FkLocateIndex, skips the
	// parent check (isIgnoreErrors) and yields no action program:
	//
	//	c(a,b, FOREIGN KEY(a,b) REFERENCES p)  DELETE FROM p    -> mismatch
	//	                                       UPDATE p SET x=2 -> mismatch
	//	                                       DROP TABLE p     -> ACCEPTED
	//	... ON DELETE CASCADE, with a child row loaded FKs-off:
	//	                                       DROP TABLE p     -> ACCEPTED,
	//	                                         and the child row is NOT
	//	                                         cascaded away (no action ran)
	//
	// It is per key: a resolvable sibling with a row still fails. The orphan
	// pass below already skips unresolvable keys.
	if len(db.fkNamingParent(tbl)) > 0 {
		for _, tr := range db.triggers {
			if tr.tableIsTemp != tbl.isTemp || !equalFoldName(tr.table, tbl.name) || tr.event != triggerDelete {
				continue
			}
			if verr := db.validateTriggerExprsOnce(tbl, triggerDelete, []*triggerMeta{tr}); verr != nil {
				return fmt.Errorf("%w: DROP TABLE %s: it is a foreign key parent and its DELETE trigger %s does not compile (%v), which C SQLite raises from the implicit delete it codes the DROP as",
					errVDBEUnsupported, tbl.name, tr.name, verr)
			}
		}
	}
	// The implicit delete also covers the child side, so dropping a child
	// resolves the deferred violations its rows carried; C compiles that
	// delete only when one is outstanding (OP_FkIfZero, sqlite3FkDropTable).
	// For each deferred (or defer_foreign_keys) key this table holds as a
	// child, a row whose key does not resolve decrements the counter as
	// fkChildSide would, with fkMayDecrement and fkCounterFor unchanged.
	// Immediate keys have nothing outstanding. tkt-b1d3a2e531.test ("BEGIN;
	// DROP TABLE pp; DROP TABLE cc; COMMIT" succeeds), fkey6.test 2.4/2.6.
	//
	// A key may arrive with fk.parentMissing (its parent dropped a statement
	// earlier, fk.parent nil): treated as an empty parent, so a full key
	// never matches, without calling fkParentHasKey. A real mismatch
	// (resolveErr != nil) is skipped; no violation involving one can be
	// outstanding.
	if db.fkDeferred+db.fkDeferredImm > 0 {
		for _, fk := range db.fkOf(tbl) {
			if !(fk.deferred || db.deferFKs) || (fk.resolveErr != nil && !fk.parentMissing) {
				continue
			}
			for rid, raw := range tbl.rows.all() {
				key, ok := fkKeyOf(fk.childIdx, normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, raw))
				if !ok {
					continue
				}
				if !fk.parentMissing {
					has := db.fkParentHasKey(fk, key)
					// fkParentHasKey reaches fk.parent, a DIFFERENT table than
					// tbl (already loaded by DropTable itself before calling
					// here) -- a load failure there is recorded into
					// db.pendingLoadErr rather than returned (fkParentHasKey has
					// no error return), so it must be checked HERE, before this
					// function returns nil and DropTable proceeds straight to
					// its own irreversible db.removeTableAndIndexes(tbl) call.
					// Unlike fkFinishStatement/ExecArgs' identical check, this
					// one cannot wait until the statement's own end: by then the
					// mutation this bug guards against has already happened.
					if db.pendingLoadErr != nil {
						err := db.pendingLoadErr
						db.pendingLoadErr = nil
						return err
					}
					if has {
						continue
					}
				}
				if db.fkMayDecrement(db.fkStmt, fk) {
					counter := db.fkCounterFor(db.fkStmt, fk)
					*counter--
				}
			}
		}
	}
	if tbl.rows.len() == 0 {
		return nil
	}
	// One child-side effect can make the drop FAIL: a key whose parent table
	// does not exist is treated as an empty parent, and each row with a
	// non-NULL key decrements the counter (sqlite3FkCheck's isIgnoreErrors
	// branch: "behave as if it is empty"), leaving it at -N. With
	// "d(a REFERENCES nosuch)" and "r(z REFERENCES d)":
	//
	//	row (1)      -> DROP TABLE d: FOREIGN KEY constraint failed
	//	row (NULL)   -> accepted (a NULL key decrements nothing)
	//	no referrer  -> accepted (sqlite3FkDropTable emits no delete)
	//	parent EXISTS but the key mismatches -> accepted (that branch only
	//	  "continue"s -- no counter moves), even with a non-NULL row
	//
	// A deferred key of that shape fails at COMMIT instead; declined.
	if len(db.fkNamingParent(tbl)) > 0 {
		for _, fk := range db.fkOf(tbl) {
			if fk.parentMissing && fkAnyRowHasFullKey(tbl, fk) {
				if fk.deferred || db.deferFKs {
					return fmt.Errorf("%w: DROP TABLE %s, whose own key %s cannot find parent table %s and is DEFERRED (C SQLite's implicit delete moves the deferred counter, failing the COMMIT rather than the DROP)",
						errVDBEUnsupported, tbl.name, fk.child.name, fk.parentName)
				}
				return fkErrViolation()
			}
		}
	}
	db.fkSuppressChild = true
	defer func() { db.fkSuppressChild = false }()
	for _, fk := range db.fkReferencing(tbl) {
		if fk.resolveErr != nil || fk.child == tbl {
			continue
		}
		// A DEFERRABLE INITIALLY DEFERRED referrer inside a DRIVER-held
		// transaction (no engine-level BEGIN) is declined exactly like an
		// ordinary DELETE's identical shape -- fkChildSide/fkParentSide's own
		// fkDeferredUnmodelled guard -- because this engine's deferred-FK model
		// is COMMIT-shaped and a driver-held transaction has no engine COMMIT
		// to hang the check on -- unless its holder promised to run the check
		// itself (MarkDeferredFKCommitter), which driver does, and which
		// is also what lets "PRAGMA defer_foreign_keys" be accepted there.
		if fk.deferred && db.fkDeferredUnmodelled() {
			return fkDeferredDeclined(fk)
		}
		rids := make([]uint64, 0, tbl.rows.len())
		for rid := range tbl.rows.all() {
			rids = append(rids, rid)
		}
		sort.Slice(rids, func(i, j int) bool { return rowidLess(rids[i], rids[j]) })
		for _, rid := range rids {
			key, ok := fkParentKeyOf(fk, normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, tbl.rows.row(rid)))
			if !ok {
				continue
			}
			kids := db.fkChildRowsMatching(fk, key)
			// fkChildRowsMatching reaches fk.child, a DIFFERENT table than
			// tbl -- same reasoning as fkParentHasKey's identical check just
			// above: a load failure there must abort BEFORE this function
			// returns nil and DropTable calls its own irreversible
			// db.removeTableAndIndexes(tbl), not just at the statement's
			// eventual end.
			if db.pendingLoadErr != nil {
				err := db.pendingLoadErr
				db.pendingLoadErr = nil
				return err
			}
			if len(kids) == 0 {
				continue
			}
			switch fk.onDelete {
			case fkCascade, fkSetNull, fkSetDefault:
				if err := db.fkApplyAction(fk, kids, fk.onDelete, nil); err != nil {
					return err
				}
			default: // NO ACTION / RESTRICT
				action := fk.onDelete
				if action == fkRestrict && db.fkRestrictDisabled() {
					// "PRAGMA defer_foreign_keys" turns RESTRICT into ordinary
					// deferred counting here too -- see fkParentSide's
					// identical normalization for the rule and its evidence.
					action = fkNoAction
				}
				if action == fkRestrict {
					// RESTRICT is never deferred by DEFERRABLE alone -- it
					// fails at the moment of the parent operation, which for a
					// DROP is the DROP itself, not the eventual COMMIT.
					return fkErrViolation()
				}
				// A deferred (or defer_foreign_keys) referrer makes this drop's
				// orphans a COMMIT-time failure ("BEGIN; DROP TABLE p" succeeds,
				// COMMIT fails; dropping the child too resolves it,
				// tkt-b1d3a2e531.test 1.2/2.2). Counted through fkCounterFor like an
				// ordinary DELETE, so fkCommitCheck (or fkFinishStatement in
				// autocommit) reports it where a DELETE's would land.
				if fk.deferred || db.deferFKs {
					counter := db.fkCounterFor(db.fkStmt, fk)
					*counter += len(kids)
					continue
				}
				return fkErrViolation()
			}
		}
	}
	return nil
}

// PragmaBooleanValue exposes this package's boolean-pragma parse -- real
// SQLite's sqlite3GetBoolean, see pragmaGetBoolean -- to the driver layer,
// which owns the connection-level "PRAGMA foreign_keys" state (see
// driver's Conn.foreignKeys) and must agree with execPragma about what
// every spelling means.
func PragmaBooleanValue(text string) bool { return pragmaGetBoolean(text, false) }

// DeferForeignKeys reports the flag this snapshot was taken with. A driver
// that opens a fresh read session per statement has to carry the connection's
// flag in, the way SetTrustedSchema does (trusted_schema.go).
func (p *ReadOnlyPager) DeferForeignKeys() bool { return p != nil && p.deferFKs }

// SetDeferForeignKeys stamps the connection's flag onto this snapshot.
func (p *ReadOnlyPager) SetDeferForeignKeys(on bool) { p.deferFKs = on }
