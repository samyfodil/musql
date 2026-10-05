// This file makes fts3/fts4 DML work inside an explicit transaction. Each rule
// is what C fts3 does to %_segdir, and most are not what "flush at COMMIT"
// alone predicts.
//
// # C accumulates pending terms and flushes once...
//
//	BEGIN; INSERT 'alpha'; INSERT 'beta'; COMMIT;
//	  -> ONE %_segdir row (in autocommit the same two INSERTs leave TWO)
//
// # ...but only while every statement is a single-row one
//
//	BEGIN; INSERT VALUES('a1'),('b2'); INSERT VALUES('c3'),('d4'); COMMIT;
//	  -> TWO rows: [0|0 = a1,b2] and [0|1 = c3,d4]
//
// The cause is the statement sub-transaction. OP_Transaction opens a statement
// journal when "p->usesStmtJournal && pOp->p2 && (db->autoCommit==0 ||
// db->nVdbeRead>1)", which calls sqlite3VtabSavepoint(SAVEPOINT_BEGIN) ->
// fts3SavepointMethod -> 'flush', writing out what earlier statements left
// pending. It runs before the statement's own writes, so the seam falls
// between statements.
//
// usesStmtJournal is "isMultiWrite && mayAbort"; for a vtab sqlite3MayAbort is
// unconditional, so it reduces to isMultiWrite, which sqlite3Insert sets from
// "pSelect || pTrigger" after collapsing a single-row VALUES:
//
//	if( pSelect && (pSelect->selFlags & SF_Values)!=0 && pSelect->pPrior==0 ){
//	  pList = pSelect->pEList; ... pSelect = 0;
//	}
//	sqlite3BeginWriteOperation(pParse, pSelect || pTrigger, iDb);
//
// A multi-row VALUES is a compound (pPrior!=0) and survives. So it is the
// syntax, not the row count:
//
//	INSERT INTO ft(docid,x) VALUES(40,'d40')             no flush
//	INSERT INTO ft(docid,x) VALUES(40,'a'),(41,'b')      flush
//	INSERT INTO ft(docid,x) SELECT 40,'d40'              flush
//	INSERT INTO ft(docid,x) SELECT a,b FROM empty_table  flush (zero rows still)
//	INSERT OR REPLACE INTO ft(docid,x) VALUES(40,'d40')  no flush
//	INSERT INTO ft(docid,x) VALUES((SELECT 40),'d40')    no flush (SF_Values)
//
// sqlite3VtabSavepoint walks all of db->aVTrans, so the flush seals every fts3
// table in the transaction; hence fts3TxnSeal, not fts3TxnSealTable.
//
// # ...and a backwards docid forces a flush first
//
// The pending doclist encoding is docid-delta based, so a document sorting
// before one already added needs a new segment:
//
//	BEGIN; INSERT docid 9; INSERT docid 2; INSERT docid 5; COMMIT;
//	  -> TWO rows: {9} and {2,5}
//	BEGIN; INSERT docid 2; INSERT docid 5; INSERT docid 9; COMMIT;
//	  -> ONE row
//
// A DELETE's docid counts the same way.
//
// # A SAVEPOINT seals the segment
//
//	BEGIN; INSERT 'alpha'; SAVEPOINT sp; INSERT 'beta';
//	ROLLBACK TO sp; INSERT 'gamma'; COMMIT;
//	  -> TWO rows, and the table holds alpha and gamma
//
// ROLLBACK likewise discards everything pending.
//
// # Implementation: rewrite, not defer
//
// Deferring the write to COMMIT would leave %_segdir empty mid-transaction,
// and it is an ordinary table queries read directly, as a MATCH does. So the
// transaction's segment is rewritten in place after every statement: its old
// row and blocks are dropped and the whole accumulated term set re-encoded.
// The final bytes equal what one flush would have produced.
//
// # Not modelled
//
// The statement sub-transaction belongs to the statement, so any statement
// can seal the segment. Only the fts table's own DML is modelled
// (fts3InsertOpensStmtSubTxn, fts3MutationOpensStmtSubTxn). These also flush
// in C and do not here:
//
//	a multi-row INSERT into any table with a constraint that can ABORT
//	  (INTEGER PRIMARY KEY, UNIQUE, NOT NULL, CHECK -- not a plain index or
//	   an unconstrained table: mayAbort stays clear)
//	CREATE TABLE / INDEX / VIEW / VIRTUAL TABLE, DROP TABLE / INDEX, ALTER
//	  TABLE, ANALYZE, REINDEX (not CREATE TRIGGER, DROP VIEW, a no-op IF [NOT]
//	  EXISTS, PRAGMA user_version=N, or SELECT)
//	a statement that fires a trigger (pTrigger sets isMultiWrite)
//	"DELETE/UPDATE ... WHERE docid = <expr naming a column>", which
//	  fts3MutationOpensStmtSubTxn answers "do not know"
//
// Sealing has no safe direction, so these stay until a statement-level
// "would this open a statement journal" classifier exists. A trigger body
// writing an fts3 table is declined, so that row is unreachable. The same gap
// is why optimize() declines mid-transaction once tainted (fts3_optimize.go):
// its answer is the segment count.
package engine

import (
	"fmt"
	"strings"
)

// fts3TxnSegment is the ONE segment PER INDEX a transaction is currently
// accumulating into for one fts3/fts4 table. A fresh set is opened when the
// transaction starts writing, when a docid goes backwards, when the statement's
// LANGUAGE ID differs from the accumulating segment's (fts3PendingTermsDocid's
// third flush point, "p->iPrevLangid != iLangid" -- see fts3_langid.go), after
// a SAVEPOINT, and when a statement opens a STATEMENT SUB-TRANSACTION (see
// fts3InsertOpensStmtSubTxn).
//
// "Per index" because a prefix= table has more than one (fts3_prefix.go), and
// C fts3 flushes them together: one accumulating segment each, all sealed at
// the same points.
type fts3TxnSegment struct {
	pt    *fts3PendingSet
	slots []fts3TxnSlot

	lastDocid int64
	haveDocid bool
}

// fts3TxnSlot is where one index's accumulating segment currently lives.
// segIdx is the %_segdir idx it occupies and segdirRowid the row it was last
// written to (0 before the first write); firstBlock and lastBlock bound the
// %_segments rows it owns, so a rewrite can drop exactly those. nLeafData is
// the leaf-byte size the LAST fts3TxnRewrite wrote for this slot -- see
// fts3TxnSeal's own comment for why that, and not any earlier rewrite's, is
// the size a promotion check here must use.
type fts3TxnSlot struct {
	segIdx      int64
	segdirRowid uint64
	firstBlock  int64
	lastBlock   int64
	nLeafData   int64
}

// fts3TxnFor returns name's accumulating segment, creating it if needed.
// db.fts3Txn is nil outside a transaction: in autocommit each statement flushes its own segment, which is
// what C fts3 does there too, so nothing is carried.
func (db *DB) fts3TxnFor(name string, prefixes []int, langid int64, desc bool) *fts3TxnSegment {
	if db.fts3Txn == nil {
		db.fts3Txn = map[string]*fts3TxnSegment{}
	}
	st := db.fts3Txn[name]
	if st == nil {
		st = &fts3TxnSegment{pt: newFts3PendingSet(prefixes, desc), slots: make([]fts3TxnSlot, len(prefixes)+1)}
		st.pt.langid = langid
		for i := range st.slots {
			st.slots[i].segIdx = -1
		}
		db.fts3Txn[name] = st
	}
	return st
}

// fts3TxnDiscard throws away every accumulated segment's pending terms -- what
// ROLLBACK and ROLLBACK TO do. The %_segdir/%_segments rows themselves are
// restored by the transaction snapshot (txn.go), so there is nothing to undo
// here beyond forgetting the terms.
func (db *DB) fts3TxnDiscard() {
	db.fts3Txn = nil
	db.fts3TxnTaint = false // see fts3TxnTaint's own doc comment (writer.go).
}

// fts3TxnSeal ends every accumulating segment without discarding what was
// written -- what a SAVEPOINT and a COMMIT do. The next write opens a fresh
// segment at a fresh idx.
//
// It is also the one point promotion (fts3_promote.go) runs for a
// transaction's segments. C's flush calls fts3PromoteSegments right after
// writing the pending segment (fts3_write.c:3330-3331), with that flush's size
// -- which here is what the last fts3TxnRewrite wrote (fts3TxnSlot.nLeafData).
// Promoting on every intermediate rewrite would compare against a smaller,
// still-growing size and could flip the 1.5x decision.
//
// writer.go's Close also calls this; see there for why.
func (db *DB) fts3TxnSeal() {
	for name, st := range db.fts3Txn {
		segdir := db.findTableMeta(name + "_segdir")
		if segdir == nil {
			continue // the table was dropped mid-transaction: nothing to promote.
		}
		for i := range st.slots {
			sl := &st.slots[i]
			if sl.segdirRowid == 0 {
				continue // this index never wrote a segment this transaction.
			}
			fts3PromoteSegments(segdir, st.pt.levelOf(i), sl.nLeafData)
		}
	}
	db.fts3Txn = nil
	db.fts3TxnTaint = false // see fts3TxnTaint's own doc comment (writer.go).
}

// fts3TxnStmtSeal applies the STATEMENT SUB-TRANSACTION flush point on its own,
// for a statement that opened a statement journal and then turned out to have
// nothing of its own to write. Real fts3 flushed at OP_Transaction either way,
// which is directly visible: "DELETE FROM ft WHERE docid>=999" matching no row
// still splits the transaction's segment, while "WHERE docid=999" -- the
// single-docid plan, which opens no statement journal -- does not.
func (db *DB) fts3TxnStmtSeal(stmtSubTxn bool) {
	if stmtSubTxn {
		db.fts3TxnSeal()
	}
}

// fts3TxnSealTable seals ONE table's accumulating segment. A command on the
// fts3 command channel (fts3_command.go) consumes that table's pending terms
// and no other's, so sealing every table's would wrongly split a segment a
// concurrent write to a DIFFERENT fts table is still filling.
func (db *DB) fts3TxnSealTable(name string) { delete(db.fts3Txn, name) }

// fts3TxnMaybeTaint sets db.fts3TxnTaint the first time sqlText -- a statement
// that just ran -- is not positively known to be a shape this file models.
// Called from ExecArgs after every top-level statement.
//
// Its bar is the opposite of fts3TxnSeal's: sealing must be exact, but taint
// only feeds a decline (optimize()'s guard), so a false taint costs an
// unneeded decline while a missed one gives optimize() a wrong segment count.
// It clears only a plain INSERT/UPDATE/DELETE/REPLACE naming a table already
// accumulating, and taints on everything else (DDL, other tables, possible
// triggers, anything it cannot classify) -- a superset of "not modelled".
func (db *DB) fts3TxnMaybeTaint(sqlText string) {
	if len(db.fts3Txn) == 0 || db.fts3TxnTaint {
		return // nothing accumulating to protect, or already tainted.
	}
	trimmed := strings.TrimSpace(sqlText)
	toks, err := lex(trimmed)
	if err != nil {
		db.fts3TxnTaint = true
		return
	}
	var table, schema string
	// definite reports whether an UPDATE/DELETE's WHERE classified every
	// conjunct as Yes or No under fts3ClassifyDocidEq (always true for
	// INSERT/REPLACE). fts3MutationOpensStmtSubTxn treats Unknown as
	// no-seal; here an Unknown means the seal decision was uncertain, so
	// the state is not positively known and taint stays.
	definite := true
	switch writeDispatchKeyword(trimmed, toks) {
	case "INSERT", "REPLACE":
		if stmt, perr := parseInsertStmt(trimmed); perr == nil {
			table, schema = stmt.table, stmt.schema
		}
	case "UPDATE":
		if stmt, perr := parseUpdateStmt(trimmed); perr == nil {
			table, schema = stmt.table, stmt.schema
			definite = fts3WhereClauseDefinite(stmt.where)
		}
	case "DELETE":
		if stmt, perr := parseDeleteStmt(trimmed); perr == nil {
			table, schema = stmt.table, stmt.schema
			definite = fts3WhereClauseDefinite(stmt.where)
		}
	}
	// A schema-qualified reference ("aux.ft") can never be THIS session's own
	// local table, however its bare name compares to something accumulating
	// here -- db.fts3Txn is keyed by the LOCAL table's own name, and a
	// statement naming a different database's table is never that table's
	// own DML no matter what db.findVtabMeta(table) would otherwise resolve
	// to. This matters because a write ROUTED to an ATTACHed database's own
	// session (execRoutedToAttached, insert_write.go) still calls this same
	// function on the ORIGINATING session with the statement's own original
	// (still schema-qualified) text -- see that call site's own comment for
	// why that write can still taint this session's accumulating segments.
	if table != "" && schema == "" && definite {
		if vm := db.findVtabMeta(table); vm != nil {
			if _, ok := db.fts3Txn[vm.name]; ok {
				return // this table's own DML: already modelled exactly.
			}
		}
	}
	db.fts3TxnTaint = true
}

// fts3WhereClauseDefinite reports whether where -- an UPDATE/DELETE's own
// WHERE clause -- classifies EVERY top-level AND conjunct as a DEFINITE
// fts3ClassifyDocidEq verdict (Yes or No), with none falling through to
// fts3DocidEqUnknown. A nil WHERE (fts3AndConjuncts' own nil case) is
// trivially definite -- there is nothing left to be unsure about.
func fts3WhereClauseDefinite(where Expr) bool {
	for _, c := range fts3AndConjuncts(where) {
		if fts3ClassifyDocidEq(c) == fts3DocidEqUnknown {
			return false
		}
	}
	return true
}

// needsSealFor reports whether adding docid to st's segment would go
// backwards, i.e. whether the segment must be sealed first.
func (st *fts3TxnSegment) needsSealFor(docid int64) bool {
	return st.haveDocid && docid <= st.lastDocid
}

// noteDocid records docid as the segment's newest once the caller has
// committed to adding it.
func (st *fts3TxnSegment) noteDocid(docid int64) {
	st.lastDocid, st.haveDocid = docid, true
}

// fts3TxnRewrite re-encodes st's whole accumulated term set and replaces the
// %_segdir row and %_segments blocks it previously occupied. Called after every
// statement inside a transaction, so the shadow tables always describe every
// row written so far.
func (db *DB) fts3TxnRewrite(st *fts3TxnSegment, m fts3Module, segdir, segments *tableMeta) error {
	// Drop what these segments previously wrote, ALL of them first, so the
	// re-encode below both reuses their %_segdir idx values and gets the same
	// block ids handed back in the same index order.
	for i := range st.slots {
		sl := &st.slots[i]
		if sl.segdirRowid == 0 {
			continue
		}
		segdir.dropRow(sl.segdirRowid)
		sl.segdirRowid = 0
		if segments != nil {
			for b := sl.firstBlock; b <= sl.lastBlock; b++ {
				segments.dropRow(uint64(b))
			}
		}
	}
	for i, pt := range st.pt.pts {
		if pt.empty() {
			continue
		}
		sl := &st.slots[i]
		level := st.pt.levelOf(i)
		if sl.segIdx < 0 {
			// The cascade leaf count fts3AllocateSegdirIdx can also report here
			// is deliberately unused: this write path's fts3-automerge trigger
			// (engine/fts3_automerge.go) does not run inside an explicit
			// transaction (see its own file comment for why), so nothing needs
			// this slot's own nLeafAdd contribution yet.
			idx, _, err := db.fts3AllocateSegdirIdx(segdir, segments, m, level, 0, st.pt.desc)
			if err != nil {
				return err
			}
			sl.segIdx = idx
		}
		img := pt.encodeSegment(int(db.pageSize)-fts3NodeOverhead, fts3NextBlockID(segments))
		if err := fts3StoreSegment(segdir, segments, level, sl.segIdx, img); err != nil {
			return err
		}
		rowid, ok := maxRowidOfTable(segdir)
		if !ok {
			return fmt.Errorf("engine: fts3: %%_segdir row vanished after being written")
		}
		sl.segdirRowid = rowid
		sl.firstBlock = img.firstBlock
		sl.lastBlock = img.firstBlock + int64(len(img.blocks)) - 1
		sl.nLeafData = img.nLeafData
	}
	return nil
}

// fts3InsertOpensStmtSubTxn reports whether an INSERT into an fts3/fts4 table
// opens a STATEMENT SUB-TRANSACTION, and so flushes every fts table's pending
// terms before it writes anything of its own (this file's second rule).
//
// It is sqlite3Insert's "pSelect || pTrigger" read back off the parse tree: a
// VALUES list of more than one row is a compound SELECT that survives the
// single-row collapse, an "INSERT ... SELECT" is one outright, and a one-row
// VALUES list -- however complicated the expressions inside it -- is not. The
// pTrigger half cannot arise here: no trigger can be attached to a virtual
// table, and a trigger BODY that writes one is declined by this write path.
func fts3InsertOpensStmtSubTxn(stmt *insertStmt) bool {
	return stmt.selectStmt != nil || len(stmt.rows) > 1
}

// fts3MutationOpensStmtSubTxn is fts3InsertOpensStmtSubTxn for DELETE and
// UPDATE, where the answer comes from the query plan, not syntax.
//
// Both call sqlite3MayAbort unconditionally for a vtab and sqlite3MultiWrite
// for everything but ONEPASS_SINGLE (the vtab DELETE branch even clears
// isMultiWrite at top-level ONEPASS_SINGLE). So the condition is
// "eOnePass != ONEPASS_SINGLE", and sqlite3WhereBegin reaches ONEPASS_SINGLE
// for a vtab only through WHERE_ONEROW, from SQLITE_INDEX_SCAN_UNIQUE.
// fts3BestIndexMethod sets that only for FTS3_DOCID_SEARCH -- the first usable
// == constraint on rowid/docid -- and whereLoopAddVirtualOne clears it for IN.
//
//	WHERE docid=20 / rowid=20 / oid=20 / _rowid_=20 / ft.docid=20      no flush
//	WHERE main.ft.docid=20 / 20=docid / docid==20 / (docid=20)         no flush
//	WHERE docid=20+0 / abs(-20) / CAST(20 AS INTEGER) / NULL           no flush
//	WHERE docid COLLATE BINARY = 20 / docid = 20 COLLATE BINARY        no flush
//	WHERE docid IN (20)                     no flush (one-element IN is an ==)
//	WHERE docid=20 AND x<>'q' / AND 1 / AND docid=30 / docid>=20 AND docid=20
//	                                        no flush (the first == wins)
//	WHERE docid=999 matching no row         no flush (the plan, not the rows)
//	WHERE docid IN (20,30) / docid>=20 / docid BETWEEN 20 AND 20       FLUSH
//	WHERE docid IS 20 / NOT docid<>20 / docid<>20 / docid NOT IN (20)  FLUSH
//	WHERE +docid=20 / 20=+docid / likely(docid)=20 / unlikely / likelihood
//	                                                                   FLUSH
//	WHERE x='d20' / WHERE 0 / WHERE 1 / no WHERE at all                FLUSH
//
// likely() and friends are stripped only after exprAnalyze decides a term is a
// constraint, so wrapping the column defeats the docid plan; COLLATE does not.
//
// The walk defaults to unknown, and unknown means do not seal -- over-sealing
// would be a new wrong answer.
func fts3MutationOpensStmtSubTxn(where Expr) bool {
	seals := true
	for _, c := range fts3AndConjuncts(where) {
		switch fts3ClassifyDocidEq(c) {
		case fts3DocidEqYes:
			return false // FTS3_DOCID_SEARCH: ONEPASS_SINGLE, no statement journal
		case fts3DocidEqUnknown:
			seals = false
		}
	}
	return seals
}

// fts3AndConjuncts flattens e over top-level AND, which is the only operator
// sqlite3WhereSplit breaks a WHERE clause on. A nil WHERE yields none, and a
// statement with no constraint at all therefore seals.
func fts3AndConjuncts(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(BinaryExpr); ok && b.Op == "AND" {
		return append(fts3AndConjuncts(b.L), fts3AndConjuncts(b.R)...)
	}
	return []Expr{e}
}

type fts3DocidEqKind int

const (
	fts3DocidEqNo      fts3DocidEqKind = iota // definitely not a usable == on the docid
	fts3DocidEqYes                            // definitely one
	fts3DocidEqUnknown                        // a shape this walk does not decide
)

// fts3ClassifyDocidEq decides whether ONE conjunct is the usable "== on the
// rowid/docid" that makes fts3's plan FTS3_DOCID_SEARCH.
func fts3ClassifyDocidEq(e Expr) fts3DocidEqKind {
	switch x := e.(type) {
	case BinaryExpr:
		if x.Op != "=" && x.Op != "==" {
			// Every other operator is a different constraint op (IS, <>, the
			// inequalities) or no constraint at all (OR, the arithmetic ones).
			return fts3DocidEqNo
		}
		if fts3IsDocidRef(x.L) {
			return fts3DocidEqRHS(x.R)
		}
		if fts3IsDocidRef(x.R) {
			return fts3DocidEqRHS(x.L)
		}
		return fts3DocidEqNo
	case InExpr:
		// "x IN (<one expression>)" is rewritten to "x == <expression>" and so
		// reaches xBestIndex as an EQ; any other IN keeps WO_IN, which
		// whereLoopAddVirtualOne strips SQLITE_INDEX_SCAN_UNIQUE for.
		if x.Not || x.Sub != nil || len(x.List) != 1 || !fts3IsDocidRef(x.X) {
			return fts3DocidEqNo
		}
		return fts3DocidEqRHS(x.List[0])
	case LiteralExpr, ParamExpr, ColumnExpr, UnaryExpr, IsNullExpr, CollateExpr,
		CastExpr, BetweenExpr, LikeExpr, GlobExpr, MatchExpr, CaseExpr, FuncExpr:
		return fts3DocidEqNo
	}
	return fts3DocidEqUnknown
}

// fts3DocidEqRHS classifies the OTHER side of a "docid = ..." term. where.c
// only accepts a term as a constraint on a table when the other side does not
// depend on that table's own cursor, and a DELETE/UPDATE against an fts3 table
// has exactly one table in scope (a subquery there is already declined), so
// "mentions no column" IS "does not depend on the table". A column reference is
// reported UNKNOWN rather than "no" because a bare identifier can also be a
// constant keyword, which C SQLite does not count as a dependency.
func fts3DocidEqRHS(e Expr) fts3DocidEqKind {
	n, known := fts3CountColumnRefs(e)
	switch {
	case !known:
		return fts3DocidEqUnknown
	case n > 0:
		return fts3DocidEqUnknown
	}
	return fts3DocidEqYes
}

// fts3IsDocidRef reports whether e is a bare reference to the fts3 table's
// rowid, under any of its spellings. exprAnalyze skips COLLATE before deciding
// a term names a column ("docid COLLATE BINARY = 20" is still the docid plan),
// but nothing else -- "+docid" and "likely(docid)" are both verified NOT to be.
//
// A qualifier is not checked against the table name: a DELETE/UPDATE against an
// fts3 table has one table in scope, so any qualifier that resolves at all
// resolves to it, and one that does not makes the statement fail before this
// runs.
func fts3IsDocidRef(e Expr) bool {
	for {
		c, ok := e.(CollateExpr)
		if !ok {
			break
		}
		e = c.X
	}
	col, ok := e.(ColumnExpr)
	if !ok {
		return false
	}
	switch strings.ToUpper(col.Name) {
	case "DOCID", "ROWID", "OID", "_ROWID_":
		return true
	}
	return false
}

// fts3CountColumnRefs counts the column references in e, reporting known=false
// for a node kind it does not walk -- the same default-deny shape
// fts3CountMatchExpr uses, and for the same reason.
func fts3CountColumnRefs(e Expr) (int, bool) {
	sum := func(es ...Expr) (int, bool) {
		n, known := 0, true
		for _, sub := range es {
			c, k := fts3CountColumnRefs(sub)
			n += c
			known = known && k
		}
		return n, known
	}
	switch x := e.(type) {
	case nil:
		return 0, true
	case LiteralExpr, ParamExpr:
		return 0, true
	case ColumnExpr:
		return 1, true
	case UnaryExpr:
		return sum(x.X)
	case BinaryExpr:
		return sum(x.L, x.R)
	case IsNullExpr:
		return sum(x.X)
	case CollateExpr:
		return sum(x.X)
	case CastExpr:
		return sum(x.X)
	case BetweenExpr:
		return sum(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return sum(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return sum(x.X, x.Pattern)
	case InExpr:
		if x.Sub != nil {
			return 0, false
		}
		return sum(append([]Expr{x.X}, x.List...)...)
	case FuncExpr:
		if x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 {
			return 0, false
		}
		return sum(x.Args...)
	case CaseExpr:
		es := []Expr{x.Base, x.Else}
		for _, w := range x.Whens {
			es = append(es, w.When, w.Then)
		}
		return sum(es...)
	}
	return 0, false
}

// fts3TxnSegmentForInsert returns the accumulating segment an INSERT's terms
// should join, or nil outside a transaction (the statement flushes its own).
//
// It first applies the two sealing rules:
//
//   - stmtSubTxn: the statement opened a statement journal, so C flushed every
//     table at OP_Transaction.
//   - backwards docid: if the lowest docid to add is at or below the segment's
//     newest, seal and open a fresh one (fts3PendingTermsDocid). staged is
//     ascending, so its first row decides.
func (db *DB) fts3TxnSegmentForInsert(name string, prefixes []int, langid int64, desc bool, staged []fts3StagedRow, stmtSubTxn bool) *fts3TxnSegment {
	if !db.inTransaction() && len(db.savepoints) == 0 {
		return nil
	}
	if stmtSubTxn {
		db.fts3TxnSeal()
	}
	st := db.fts3TxnFor(name, prefixes, langid, desc)
	if (len(staged) > 0 && st.needsSealFor(staged[0].docid)) || st.pt.langid != langid {
		db.fts3TxnSeal()
		st = db.fts3TxnFor(name, prefixes, langid, desc)
	}
	for _, r := range staged {
		st.noteDocid(r.docid)
	}
	return st
}

// fts3TxnSegmentForMutation is fts3TxnSegmentForInsert for a DELETE/UPDATE,
// whose docids are known up front and whose statement sub-transaction is
// decided by the query plan (fts3MutationOpensStmtSubTxn) rather than the shape
// of a data source.
func (db *DB) fts3TxnSegmentForMutation(name string, prefixes []int, langid int64, desc bool, docids []int64, stmtSubTxn bool) *fts3TxnSegment {
	if !db.inTransaction() && len(db.savepoints) == 0 {
		return nil
	}
	if stmtSubTxn {
		db.fts3TxnSeal()
	}
	st := db.fts3TxnFor(name, prefixes, langid, desc)
	if (len(docids) > 0 && st.needsSealFor(docids[0])) || st.pt.langid != langid {
		db.fts3TxnSeal()
		st = db.fts3TxnFor(name, prefixes, langid, desc)
	}
	for _, d := range docids {
		st.noteDocid(d)
	}
	return st
}

// fts3TxnMergeAndRewrite folds this statement's pending terms into the
// transaction's accumulating segment and rewrites it in place.
func (db *DB) fts3TxnMergeAndRewrite(st *fts3TxnSegment, stmtTerms *fts3PendingSet, m fts3Module, segdir, segments *tableMeta) error {
	st.pt.merge(stmtTerms)
	return db.fts3TxnRewrite(st, m, segdir, segments)
}

// fts3MidFlush is fts3PendingTermsDocid's flush rule (fts3_write.c:888-909)
// applied per operation (a row's DELETE half, then its INSERT half), as a
// docid-changing UPDATE, a WHERE-less "order=DESC" UPDATE and a
// language-tracking UPDATE need:
//
//	iDocid<p->iPrevDocid || (iDocid==p->iPrevDocid && p->bPrevDelete==0)
//	  || p->iPrevLangid!=iLangid
//
// (The nPendingData>nMaxPendingData memory-pressure flush is not modelled.)
// Ordinary DELETEs and UPDATEs are ascending, same-docid and same-language, so
// fts3TxnSegmentForMutation's whole-statement check covers them.
type fts3MidFlush struct {
	docid     int64
	langid    int64
	have      bool
	wasDelete bool
}

// note applies the rule to one operation and reports whether the caller must
// seal off whatever it has accumulated so far and start a fresh segment
// BEFORE folding this operation's own terms in. The "insert half of the
// delete just recorded" exception is docid==the previous op's AND that op was
// the delete half AND this one is the insert half -- an ordinary same-docid
// update's own pair, verified never to flush (fts3_write.go) -- UNLESS the
// language also changed, which flushes regardless (C fts3's own OR-clause
// has no such exception; see fts3_langid.go's "A langid CHANGE splits the
// segment").
func (f *fts3MidFlush) note(docid int64, isDelete bool, langid int64) bool {
	flush := f.have && (docid <= f.docid && !(docid == f.docid && f.wasDelete && !isDelete) || langid != f.langid)
	f.docid, f.wasDelete, f.langid, f.have = docid, isDelete, langid, true
	return flush
}

// fts3TxnPeekLastDocid reads name's CURRENTLY accumulating segment's last
// docid without creating one -- what a docid-changing UPDATE's fts3MidFlush
// seeds from, so one that is not the first write of its transaction still
// flushes at the point C fts3's connection-wide iPrevDocid would: verified,
// a single-row INSERT opens no statement sub-transaction, so the segment it
// leaves accumulating is exactly what a LATER statement's own first operation
// must compare against. A table with nothing accumulating (outside a
// transaction, or the first write of one) answers unseeded, matching
// fts3TxnFor's own fresh-segment state.
func (db *DB) fts3TxnPeekLastDocid(name string) (docid int64, have bool) {
	st := db.fts3Txn[name]
	if st == nil {
		return 0, false
	}
	return st.lastDocid, st.haveDocid
}

// fts3TxnPeekLastLangid is fts3TxnPeekLastDocid's twin for iPrevLangid: the
// language of name's current accumulating segment, without creating one. A
// language-tracking UPDATE's fts3MidFlush must start from it, not zero, to
// match C's connection-wide iPrevLangid. A segment's pt.langid is always set
// at creation (0 without "languageid="), so have means only "a segment
// exists".
func (db *DB) fts3TxnPeekLastLangid(name string) (langid int64, have bool) {
	st := db.fts3Txn[name]
	if st == nil {
		return 0, false
	}
	return st.pt.langid, true
}

// fts3TxnFlushMid folds pt into name's transaction segment and SEALS it (or,
// outside a transaction, writes pt as its own independent segment via
// fts3FlushPendingSet, exactly as a standalone statement's own final segment
// does) -- a MID-STATEMENT flush point. The next write, whether later in the
// SAME statement or a later one, starts fresh.
func (db *DB) fts3TxnFlushMid(name string, prefixes []int, langid int64, desc bool, pt *fts3PendingSet, m fts3Module, segdir, segments *tableMeta) error {
	if !db.inTransaction() && len(db.savepoints) == 0 {
		// The leaf count is discarded here: a caller reaches a mid-statement
		// flush at all only when it has ALREADY declined the "more than one
		// %_segdir write of its own" combination with automerge active (see
		// vtab_fts3.go's insertIntoFts3 and fts3_write.go's fts3Mutation.commit),
		// so automerge is guaranteed off for name by the time this runs.
		_, err := db.fts3FlushPendingSet(m, segdir, segments, pt)
		return err
	}
	st := db.fts3TxnFor(name, prefixes, langid, desc)
	if err := db.fts3TxnMergeAndRewrite(st, pt, m, segdir, segments); err != nil {
		return err
	}
	db.fts3TxnSeal()
	return nil
}
