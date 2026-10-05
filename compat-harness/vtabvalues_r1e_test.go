// Virtual-table and FTS write primitives now take VALUES rather than AST expressions.
// Refactored to mirror C SQLite, asserted as invisible through differential testing.
package compat

import "testing"

// vtabValuesFtsCycle is one full mutation cycle over an fts3/fts4 table: every
// verb the write primitives serve, each followed by the MATCH read that would
// expose an index the mutation left wrong.
func vtabValuesFtsCycle(module string) []string {
	return []string{
		`CREATE VIRTUAL TABLE t USING ` + module + `(a, b)`,
		`INSERT INTO t(docid,a,b) VALUES(1,'alpha one','x y')`,
		`INSERT INTO t(docid,a,b) VALUES(2,'beta two','y z')`,
		`INSERT INTO t(docid,a,b) VALUES(3,'gamma three','z w')`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'two' ORDER BY docid`,
		`PRAGMA integrity_check`,

		// UPDATE: the SET right-hand sides go through writeApplySetList, the
		// WHERE through writeRowSelected.
		`UPDATE t SET a='delta four' WHERE docid=2`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'two' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'four' ORDER BY docid`,
		`PRAGMA integrity_check`,

		// A SET RHS that reads the OLD row (update.c:1282 codes it inside the
		// scan, so it sees the pre-update values), and a multi-row WHERE.
		`UPDATE t SET b = a || '!' WHERE docid > 1`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		`PRAGMA integrity_check`,

		// UPSERT, which insert.c:1291-1294 rejects for EVERY virtual table.
		`INSERT INTO t(docid,a,b) VALUES(1,'x','y') ON CONFLICT DO NOTHING`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`PRAGMA integrity_check`,

		// A WHERE that is NULL, not false: isTruthy must still reject it.
		`UPDATE t SET a='never' WHERE NULL`,
		`DELETE FROM t WHERE NULL`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`PRAGMA integrity_check`,

		// DELETE, then the MATCH that must no longer find the row.
		`DELETE FROM t WHERE docid=3`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'gamma' ORDER BY docid`,
		`PRAGMA integrity_check`,

		// A ROWID-MOVING update: "docid" is the one spelling that moves it.
		`UPDATE t SET docid=10 WHERE docid=1`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		`PRAGMA integrity_check`,

		// INSERT OR REPLACE onto an occupied docid.
		`INSERT OR REPLACE INTO t(docid,a,b) VALUES(10,'epsilon five','q')`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'epsilon' ORDER BY docid`,
		`PRAGMA integrity_check`,

		// The COMMAND CHANNEL, which fts3/fts4 have always served from values
		// (engine/fts3_command.go's fts3CommandInsert takes [][]Value) -- the
		// shape fts5's channel was moved onto in this batch.
		`INSERT INTO t(t) VALUES('optimize')`,
		`SELECT docid FROM t WHERE t MATCH 'epsilon' ORDER BY docid`,
		`PRAGMA integrity_check`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'five' ORDER BY docid`,
		`INSERT INTO t(t) VALUES('integrity-check')`,
		`PRAGMA integrity_check`,
	}
}

func TestVtabValuesR1EFtsCycle(t *testing.T) {
	for _, m := range []string{"fts3", "fts4"} {
		t.Run(m, func(t *testing.T) {
			differ(t, "R1E "+m+" cycle", vtabValuesFtsCycle(m))
		})
	}
}

// TestVtabValuesR1ERowidSynonymSetIsNoOp covers the SET target that resolves
// to no slot at all: rowid/oid/_rowid_ against an fts3/fts4 table, which real
// SQLite leaves alone (sqlite3Update's virtual-table branch never resolves the
// generic rowid synonyms against a vtab's declared columns the way it does for
// a real table) -- engine/fts3_write.go's fts3SetRowidSynonymNoOp, the
// NEGATIVE slot writeApplySetList skips.
//
// It is the mutation-sensitive half of the writeApplySetList change: the slot
// is -2, so a writeApplySetList that stopped honoring a negative entry does not
// merely answer differently, it writes newVals[-2] and takes the process down.
// "Never panic" makes that the loudest failure this harness has.
//
// Both halves of that no-op are pinned, because they are NOT the same claim:
// the ASSIGNMENT is dropped, but the RHS is still EVALUATED. C SQLite
// resolves the synonym to chngRowid with the expression held aside
// (update.c:494-498) and codes it into apVal[1] (update.c:1291); fts3's
// xUpdate then reads the new rowid from the explicit docid cell
// apVal[3+nColumn] and consults apVal[1] only when that is NULL
// (fts3_write.c:5762-5765), which on an UPDATE it never is. So an RHS that
// errors must still abort the whole statement. "1/0" cannot show that -- it
// is NULL in SQLite, not an error -- so the erroring RHS is pinned separately
// below.
func TestVtabValuesR1ERowidSynonymSetIsNoOp(t *testing.T) {
	for _, syn := range []string{"rowid", "oid", "_rowid_"} {
		t.Run(syn, func(t *testing.T) {
			differ(t, "R1E rowid-synonym SET "+syn, []string{
				`CREATE VIRTUAL TABLE t USING fts4(a)`,
				`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
				`INSERT INTO t(docid,a) VALUES(2,'beta')`,
				`UPDATE t SET ` + syn + `=1/0 WHERE docid=1`,
				`SELECT docid, a FROM t ORDER BY docid`,
				`SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
				`SELECT docid FROM t WHERE t MATCH 'beta' ORDER BY docid`,
				`PRAGMA integrity_check`,
				// The same statement with a REAL column beside the synonym: the
				// real column's RHS must still be evaluated and stored.
				`UPDATE t SET ` + syn + `=7, a='gamma' WHERE docid=2`,
				`SELECT docid, a FROM t ORDER BY docid`,
				`SELECT docid FROM t WHERE t MATCH 'gamma' ORDER BY docid`,
				`SELECT docid FROM t WHERE t MATCH 'beta' ORDER BY docid`,
				`PRAGMA integrity_check`,
			})
		})
	}
}

// TestVtabValuesR1ERowidSynonymSetStillEvaluates is the other half of the
// no-op above: the slotless target's VALUE is discarded, but its ERRORS are
// the statement's. C SQLite codes the expression (update.c:1291) even
// though fts3's xUpdate never reads the register it lands in
// (fts3_write.c:5762-5765), so "integer overflow" aborts the UPDATE and the
// real column beside it is NOT written.
//
// This was a live wrong answer before R1 batch E: writeApplySetList skipped a
// negative slot entirely, so the overflow went unraised and a='beta' was
// committed. The oracle raises and leaves a='alpha'.
func TestVtabValuesR1ERowidSynonymSetStillEvaluates(t *testing.T) {
	for _, syn := range []string{"rowid", "oid", "_rowid_"} {
		t.Run(syn, func(t *testing.T) {
			differ(t, "R1E rowid-synonym SET evaluates "+syn, []string{
				`CREATE VIRTUAL TABLE t USING fts4(a)`,
				`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
				// The whole statement must abort: no "integer overflow"
				// swallowed, and a stays 'alpha'.
				`UPDATE t SET ` + syn + `=abs(-9223372036854775807-1), a='beta' WHERE docid=1`,
				`SELECT docid, a FROM t ORDER BY docid`,
				`SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
				`SELECT docid FROM t WHERE t MATCH 'beta' ORDER BY docid`,
				`PRAGMA integrity_check`,
				// The synonym alone, with nothing else in the SET list.
				`UPDATE t SET ` + syn + `=abs(-9223372036854775807-1) WHERE docid=1`,
				`SELECT docid, a FROM t ORDER BY docid`,
				// Column RHS errors win over the synonym's regardless of the
				// order they are WRITTEN in, because SQLite codes every column
				// first (update.c:1282) and the rowid last (update.c:1291).
				`UPDATE t SET a=zeroblob(1000000000000), ` + syn + `=abs(-9223372036854775807-1) WHERE docid=1`,
				`UPDATE t SET ` + syn + `=abs(-9223372036854775807-1), a=zeroblob(1000000000000) WHERE docid=1`,
				`SELECT docid, a FROM t ORDER BY docid`,
				`PRAGMA integrity_check`,
			})
		})
	}
}

// TestVtabValuesR1EExternalContent covers the "content=" table, where the row
// the write path is handed contributes to the index alone and the documents
// live in an ORDINARY table resolved by name. This project has already shipped
// one silent wrong answer there (an fts index reporting zero matches while
// persisting the empty index), so every mutation is followed by the MATCH that
// would have caught it.
//
// PRAGMA integrity_check is asserted at the points where the two engines agree
// about it, and NOT in the middle of the cycle: an UPDATE/DELETE on a content=
// table deliberately leaves the index describing documents the content table
// no longer holds, and real fts4's integrity_check reports that as "malformed
// inverted index" where this engine's still reports ok. That gap is
// pre-existing (it reproduces byte for byte with this batch's engine changes
// reverted) and has nothing to do with which primitives take values, so it is
// reported rather than pinned here -- pinning it would freeze a wrong answer.
func TestVtabValuesR1EExternalContent(t *testing.T) {
	differ(t, "R1E fts4 content=", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'alpha one','x'),(2,'beta two','y'),(3,'gamma three','z')`,
		`CREATE VIRTUAL TABLE t USING fts4(content="src", a, b)`,
		`INSERT INTO t(docid,a,b) SELECT id,a,b FROM src`,
		`SELECT docid FROM t WHERE t MATCH 'two' ORDER BY docid`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`PRAGMA integrity_check`,
		// The index is updated on its own; the content table is not.
		`UPDATE t SET a='delta four' WHERE docid=2`,
		`SELECT docid FROM t WHERE t MATCH 'two' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'four' ORDER BY docid`,
		`DELETE FROM t WHERE docid=3`,
		`SELECT docid FROM t WHERE t MATCH 'gamma' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		// 'rebuild' re-indexes from the content table, which puts the two back
		// in step -- and integrity_check comparable again.
		`INSERT INTO t(t) VALUES('rebuild')`,
		`SELECT docid FROM t WHERE t MATCH 'two' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'gamma' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'four' ORDER BY docid`,
		`SELECT docid, a, b FROM t ORDER BY docid`,
		`PRAGMA integrity_check`,
	})
}

// TestVtabValuesR1ERtree runs the same verbs against the OTHER writable module
// this engine ships, whose store has no full-text index at all -- so a change
// to the shared vtab write path that only happened to work for fts would show
// up here.
func TestVtabValuesR1ERtree(t *testing.T) {
	differ(t, "R1E rtree cycle", []string{
		`CREATE VIRTUAL TABLE r USING rtree(id, minX, maxX, minY, maxY)`,
		`INSERT INTO r VALUES(1, 0.0, 1.0, 0.0, 1.0)`,
		`INSERT INTO r VALUES(2, 2.0, 3.0, 2.0, 3.0)`,
		`INSERT INTO r VALUES(3, 4.0, 5.0, 4.0, 5.0)`,
		`SELECT id, minX, maxX, minY, maxY FROM r ORDER BY id`,
		`SELECT id FROM r WHERE minX>=2.0 AND maxX<=3.0 ORDER BY id`,
		`PRAGMA integrity_check`,
		// A SET RHS reading the OLD row, under a WHERE.
		`UPDATE r SET maxX = maxX + 10.0 WHERE id=2`,
		`SELECT id, minX, maxX, minY, maxY FROM r ORDER BY id`,
		`PRAGMA integrity_check`,
		// A WHERE that is NULL, not false.
		`UPDATE r SET maxY = 99.0 WHERE NULL`,
		`DELETE FROM r WHERE NULL`,
		`SELECT id, minX, maxX, minY, maxY FROM r ORDER BY id`,
		// A ROWID-MOVING update: for rtree the id column IS the rowid
		// (rtreeUpdate takes aData[2]).
		`UPDATE r SET id=20 WHERE id=3`,
		`SELECT id, minX, maxX, minY, maxY FROM r ORDER BY id`,
		`SELECT rowid, id FROM r ORDER BY rowid`,
		`PRAGMA integrity_check`,
		`DELETE FROM r WHERE id=1`,
		`SELECT id, minX, maxX, minY, maxY FROM r ORDER BY id`,
		`PRAGMA integrity_check`,
	})
}

// TestVtabValuesR1EUpsertStaysRejected pins the one conflict shape C SQLite
// refuses for EVERY virtual table, module-independent: insert.c:1291-1294
// rejects UPSERT against a vtab in the parser/codegen, so both engines must
// reject it. It is here because insertIntoVtab's gate sits right beside the
// command-channel block this batch rewrote.
func TestVtabValuesR1EUpsertStaysRejected(t *testing.T) {
	differ(t, "R1E vtab upsert", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
		`INSERT INTO t(docid,a) VALUES(1,'beta') ON CONFLICT DO NOTHING`,
		`INSERT INTO t(docid,a) VALUES(1,'beta') ON CONFLICT(docid) DO UPDATE SET a='gamma'`,
		`SELECT docid, a FROM t ORDER BY docid`,
		`PRAGMA integrity_check`,
	})
}
