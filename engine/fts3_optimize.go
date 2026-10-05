// This file implements fts3/fts4's optimize() function: merges index segments
// and reports 'Index optimized' (SQLITE_OK) or 'Index already optimal' (SQLITE_DONE).
// The merge writes the live session so subsequent calls see updated segdir.
//
// # What it does NOT move
//
// changes() is unchanged across an optimize() -- it still reports whatever the
// previous statement set (verified: 1 either side). total_changes() DOES move,
// by the number of shadow-table rows the merge wrote (10 -> 13 for a two-into-
// one merge, and not at all for a no-op), which is a module internal this
// engine does not reproduce -- so a merge marks total_changes() opaque, exactly
// as an fts3 INSERT already does (vtab_write.go).
//
// # The one shape this declines, and why
//
// Real fts3 holds a transaction's writes as PENDING TERMS and flushes them into
// a segment at defined points; this engine writes the transaction's segment
// EAGERLY and rewrites it in place after every statement (fts3_txn.go). At a
// COMMIT the two agree byte for byte, which is what makes the eager model
// sound. What optimize() needs is stronger than that: its whole answer is the
// SEGMENT COUNT, so the two have to agree MID-transaction as well, on every
// path that could have got here.
//
// They now agree for every statement written against the fts table itself --
// fts3_txn.go models all four of C fts3's flush points, including the
// statement sub-transaction that splits
//
//	BEGIN; INSERT INTO ft VALUES('a1'),('b2'); INSERT INTO ft VALUES('c3'),('d4');
//
// into two segments. What is NOT modelled is a statement in the same
// transaction that opens a statement journal WITHOUT touching the fts table:
// most DDL, a multi-row INSERT into any table with an abortable constraint, and
// anything that fires a trigger (the list, with its oracle evidence, is in
// fts3_txn.go). Real fts3 flushes there too, so after one of those this engine
// holds ONE accumulating segment where C fts3 has one flushed segment plus
// pending terms -- and optimize() would answer 'Index already optimal' where
// the oracle answers 'Index optimized'.
//
// Telling that case apart EXACTLY -- deciding whether a given statement really
// would have opened a C fts3 statement journal -- needs a classifier that is
// exact for every statement kind, which is what fts3_txn.go's own sealing logic
// already has to be (over-sealing there would split a segment C fts3 never
// split, "a NEW wrong answer" per that file's comment). optimize() does not
// need that precision: it only needs to know whether it is SAFE to answer, and
// answering "no" too often just costs an unnecessary decline. So
// db.fts3TxnTaint (writer.go) classifies the OTHER way around, by inclusion:
// db.fts3TxnMaybeTaint (fts3_txn.go), run after every top-level statement,
// clears only the shapes it can positively prove are already modelled here --
// this table's own INSERT/UPDATE/DELETE/REPLACE -- and taints on everything
// else, DDL and an unrelated table's write included. optimize() declines only
// when BOTH this table has an accumulating segment open AND that flag is set;
// outside a transaction there is no accumulating segment at all (each
// statement flushes its own, exactly as C fts3 does there), and inside one,
// a table this transaction has not written is still served untainted, because
// its %_segdir is the committed one both engines share.
package engine

import (
	"fmt"
	"strings"
)

// fts3OptimizeFuncName reports whether name is fts3/fts4's optimize(). It is
// kept apart from fts3AuxFuncName (offsets/matchinfo/snippet) because those
// three report on the CURRENT ROW of a MATCH and this one does not read the row
// at all -- it is a whole-table command wearing a function's clothes.
func fts3OptimizeFuncName(name string) bool { return strings.EqualFold(name, "optimize") }

// fts3OptimizeInfo is OpFts3Optimize's P4 payload: the fts3 table the call
// names, resolved at compile time.
type fts3OptimizeInfo struct {
	table string
}

// compileFts3Optimize compiles "optimize(<fts3 table>)". ok is false when the
// argument does not name an fts3 table at all, leaving compileFunc to reject
// the name the ordinary way -- which is what C SQLite does too, in its own
// wording ("unable to use function optimize in the requested context" for a
// column of an ordinary table, "no such column: t" for a FROM-less call).
func (c *compiler) compileFts3Optimize(x FuncExpr) (int, bool, error) {
	scopes := make([]tableScope, len(c.scopes))
	for i, s := range c.scopes {
		scopes[i] = s.tableScope
	}
	if len(x.Args) != 1 {
		// Same rule as compileFts3Aux's: an arity error must look identical
		// whether or not the argument would have named an fts3 table, because
		// C SQLite raises it before ever looking at the argument.
		for i := range scopes {
			if _, isFts3 := fts3ScopeInfo(c.pager, &scopes[i]); isFts3 {
				return 0, true, fmt.Errorf("engine: wrong number of arguments to function %s()", x.Name)
			}
		}
		return 0, false, nil
	}
	ms, ok := fts3ResolveMatchTarget(c.pager, scopes, x.Args[0])
	if !ok {
		return 0, false, nil
	}
	if ms.iDefault != len(ms.cols) {
		// A COLUMN rather than the table's hidden self-column. C SQLite's
		// own wording, verified: "illegal first argument to optimize".
		return 0, true, fmt.Errorf("engine: illegal first argument to %s", x.Name)
	}
	if ms.dbIdx != 0 {
		// The table lives in an ATTACHed database; the write session behind
		// this snapshot is the LOCAL one, so there is nothing to merge into.
		// C SQLite optimizes it normally (verified against an attached
		// ":memory:"), so this is a decline, not an error both engines share.
		return 0, true, fmt.Errorf("%w: optimize() over a table in an attached database (the write session behind this read snapshot is the local one)", errVDBEUnsupported)
	}
	if c.pager == nil || c.pager.writeSession == nil {
		// A pager opened from a file has no session to write through. This is
		// the autocommit READ path: the driver routes a statement that calls
		// optimize() into a write session instead (driver/conn.go).
		return 0, true, fmt.Errorf("%w: optimize() over a read-only snapshot (it MUTATES the index it names, so it needs the write session the snapshot was taken from)", errVDBEUnsupported)
	}
	d := c.alloc()
	c.emit(Instruction{Op: OpFts3Optimize, P2: d, P4: &fts3OptimizeInfo{table: ms.table}})
	return d, true, nil
}

// evalFts3Optimize runs one optimize() call against the live write session
// behind p. It is OpFts3Optimize's whole body.
func evalFts3Optimize(p *ReadOnlyPager, info *fts3OptimizeInfo) (Value, error) {
	if p == nil || p.writeSession == nil {
		return Value{}, fmt.Errorf("%w: optimize() over a read-only snapshot", errVDBEUnsupported)
	}
	db := p.writeSession
	vm := db.findVtabMeta(info.table)
	if vm == nil {
		return Value{}, fmt.Errorf("engine: unable to use function optimize in the requested context")
	}
	m, isFts3 := fts3ModuleOf(vm)
	if !isFts3 {
		return Value{}, fmt.Errorf("engine: unable to use function optimize in the requested context")
	}
	if db.queryOnly || db.writeLock != "" {
		// optimize() WRITES, so "PRAGMA query_only=1" refuses it even though the
		// statement carrying it is a SELECT -- verified against the oracle,
		// which answers "attempt to write a readonly database".
		return Value{}, db.refusedWrite()
	}
	accumulating := db.fts3Txn[vm.name] != nil
	if accumulating && db.fts3TxnTaint {
		// This transaction is still accumulating into a segment C fts3 would
		// be holding as pending terms, AND has run a statement (DDL, a write to
		// some OTHER table, a trigger) fts3_txn.go's model does not account for
		// -- see fts3TxnMaybeTaint. Declined only for THAT combination: when
		// nothing untracked has happened, this engine's eager per-statement
		// rewrite already keeps %_segdir holding exactly what C fts3's own
		// sqlite3Fts3PendingTermsFlush (fts3DoOptimize's first act,
		// fts3_write.c:3549) would have just written, so the merge below sees
		// the same segment count either way.
		return Value{}, fmt.Errorf("%w: optimize() on %s while this transaction is still writing to it AND has run a statement this engine cannot yet classify as flushing C fts3's pending terms or not (DDL, a write to another table, or a trigger)", errVDBEUnsupported, vm.name)
	}
	wrote, seenDone, err := db.fts3Optimize(vm, m)
	if err != nil {
		return Value{}, err
	}
	if accumulating {
		// Real fts3's sqlite3Fts3PendingTermsFlush (fts3DoOptimize's first act)
		// empties pendingTerms unconditionally, so a LATER statement against
		// this table in the same transaction opens a FRESH segment rather than
		// extending the one the merge above just consumed -- exactly the seal
		// fts3RunCommand already applies for the command-channel spelling
		// ("INSERT INTO t(t) VALUES('optimize')", fts3_command.go). Without
		// this, fts3TxnRewrite's next call would try to drop %_segdir/
		// %_segments rows this merge already deleted.
		db.fts3TxnSealTable(vm.name)
	}
	if wrote {
		// The merge's own shadow-table row counts are module internals this
		// engine does not reproduce -- the same split vtab_write.go already
		// applies to an fts3 INSERT.
		db.totalChangesOpaque = true
	}
	if seenDone {
		return Value{Typ: Text, S: []byte("Index already optimal")}, nil
	}
	return Value{Typ: Text, S: []byte("Index optimized")}, nil
}

// StatementCallsFts3Optimize reports whether sqlText calls a one-argument
// function named "optimize" anywhere in its text. It is the driver's test for
// "this SELECT is really a WRITE" (driver/conn.go), the same role
// StatementHasReturning plays for a RETURNING DML -- and like that one it is
// deliberately cheap and conservative: it lexes rather than parses, so a string
// literal or a quoted identifier spelling "optimize(" never matches, and a
// false positive costs only a write session for a statement that turns out not
// to need one.
func statementCallsFts3OptimizeUncached(sqlText string) bool {
	toks, err := lex(sqlText)
	if err != nil {
		return false
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind == tkIdent && !toks[i].quoted && strings.EqualFold(toks[i].text, "optimize") &&
			toks[i+1].kind == tkPunct && toks[i+1].text == "(" {
			return true
		}
	}
	return false
}
