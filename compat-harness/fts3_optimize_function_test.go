// Differential gate for fts3/fts4's optimize() function: a scalar function that
// side-effects the index. Verified via shadow tables %_segdir and %_segments.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// fts3OptShadowDump is the full index image of a one-column fts4 table. Every
// case ends with it, so a merge that wrote different bytes for the same
// sentence fails here rather than passing on the string alone.
var fts3OptShadowDump = []string{
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT rowid, level, idx FROM t_segdir ORDER BY rowid`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(c0a) FROM t_content ORDER BY docid`,
}

func TestFts3OptimizeFunctionDiff(t *testing.T) {
	cases := []struct {
		name string
		// stmts is the script; setup runs first and every statement of it must
		// succeed on BOTH engines (nAccepted below), so a case can never pass
		// vacuously by failing identically on both sides.
		stmts     []string
		nAccepted int
	}{
		// The plain merge, and the sentence either side of it. Three level-0
		// segments become one; the second call has nothing left to do.
		{"three segments merge, then already optimal", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta two')`,
			`INSERT INTO t(a) VALUES('gamma three')`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`SELECT docid FROM t WHERE t MATCH 'two'`,
		}, fts3OptShadowDump...), 8},

		// fts3d.test 4.5's own shape, the mined statement this rule was closed
		// for: three INSERTs and a DELETE leave FOUR level-0 segments (the
		// delete markers are their own), and the merge drops the markers.
		{"merge drops delete markers", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid, a) VALUES (1, 'This is a test')`,
			`INSERT INTO t(docid, a) VALUES (2, 'That was a test')`,
			`INSERT INTO t(docid, a) VALUES (3, 'This is a test')`,
			`DELETE FROM t WHERE docid IN (1,3)`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`SELECT offsets(t) FROM t WHERE t MATCH 'this OR that OR was OR a OR is OR test' ORDER BY docid`,
		}, fts3OptShadowDump...), 8},

		// The output level is the GREATEST level present, not 0 -- and
		// fts3d.test 5.1 reaches it by editing %_segdir by hand, so the level
		// need not be one this engine's own writes would produce.
		{"output lands at the greatest level present", func() []string {
			s := []string{`CREATE VIRTUAL TABLE t USING fts4(a)`}
			for i := 0; i < 18; i++ {
				s = append(s, `INSERT INTO t(a) VALUES('word`+string(rune('a'+i))+` common')`)
			}
			return append(append(s,
				`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
				`SELECT optimize(t) FROM t LIMIT 1`,
				`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
				`UPDATE t_segdir SET level = 2 WHERE level = 1 AND idx = 0`,
				`SELECT optimize(t) FROM t LIMIT 1`,
			), fts3OptShadowDump...)
		}(), 24},

		// bSeenDone is the OR over every (langid, index), so a table whose
		// PREFIX index holds one segment answers 'Index already optimal' WHILE
		// its term index really does merge three rows into one. The sentence
		// therefore cannot be derived from "did the call write".
		{"prefix index makes it already optimal while the term index merges", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a, prefix=5)`,
			`INSERT INTO t(a) VALUES('ab')`,
			`INSERT INTO t(a) VALUES('cd')`,
			`INSERT INTO t(a) VALUES('abcdef')`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
			`SELECT optimize(t) FROM t LIMIT 1`,
		}, fts3OptShadowDump...), 8},

		// ...and the mirror, which is what makes the rule "nSegment==1", not
		// "nSegment<=1": the same prefix= table holding only terms too short to
		// index leaves the prefix index with ZERO segments, and a zero-segment
		// merge falls out of fts3SegmentMerge with rc still SQLITE_OK, so the
		// answer is 'Index optimized'.
		{"a prefix index with no segments does not make it already optimal", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a, prefix=5)`,
			`INSERT INTO t(a) VALUES('x')`,
			`INSERT INTO t(a) VALUES('y')`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
			`SELECT optimize(t) FROM t LIMIT 1`,
		}, fts3OptShadowDump...), 5},

		// It runs ONCE PER ROW SCANNED, not once per statement: the first call
		// merges and every later one reads what it wrote.
		{"once per row, and the later rows see the merge", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('a one')`,
			`INSERT INTO t(a) VALUES('b one')`,
			`INSERT INTO t(a) VALUES('c one')`,
			`SELECT docid, optimize(t) FROM t WHERE t MATCH 'one'`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
		}, 6},

		// An EMPTY table returns NO ROWS -- the function never runs at all --
		// and a LIMIT 0 does the same over a populated one.
		{"an empty table and a LIMIT 0 never call it", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`INSERT INTO t(a) VALUES('one')`,
			`INSERT INTO t(a) VALUES('two')`,
			`SELECT optimize(t) FROM t LIMIT 0`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
			`SELECT * FROM (SELECT optimize(t) FROM t LIMIT 1)`,
		}, fts3OptShadowDump...), 7},

		// "PRAGMA query_only=1" refuses it even though the statement carrying
		// it is a SELECT, and the index is left untouched.
		{"query_only refuses it and changes nothing", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta two')`,
			`PRAGMA query_only=1`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`PRAGMA query_only=0`,
		}, fts3OptShadowDump...), 4},

		// Inside a transaction that has NOT written this table, %_segdir is the
		// committed one both engines share, so the merge is served -- and a
		// later write to the same table opens a FRESH segment rather than
		// folding back into the one the merge produced.
		{"in a transaction that did not write this table", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`CREATE VIRTUAL TABLE u USING fts4(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta two')`,
			`BEGIN`,
			`INSERT INTO u(a) VALUES('other')`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`INSERT INTO t(a) VALUES('gamma three')`,
			`COMMIT`,
		}, fts3OptShadowDump...), 9},

		// ROLLBACK takes the merge with it.
		{"a rolled back transaction discards the merge", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta two')`,
			`BEGIN`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`ROLLBACK`,
		}, fts3OptShadowDump...), 6},

		// A "content=" table has no %_content of its own; optimize() only ever
		// reads %_segdir and %_segments, so it works there too.
		{"a content= table", []string{
			`CREATE TABLE src(id INTEGER PRIMARY KEY, x)`,
			`INSERT INTO src VALUES(1,'aa'),(2,'bb')`,
			`CREATE VIRTUAL TABLE ct USING fts4(content=src, x)`,
			`INSERT INTO ct(ct) VALUES('rebuild')`,
			`INSERT INTO ct(docid,x) VALUES(3,'cc')`,
			`SELECT optimize(ct) FROM ct LIMIT 1`,
			`SELECT optimize(ct) FROM ct LIMIT 1`,
			`SELECT level, idx, quote(root) FROM ct_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM ct_segments ORDER BY blockid`,
		}, 9},

		// The three argument shapes C SQLite rejects, each with its own
		// error, plus the FROM-less call. Both engines must refuse all four.
		{"the rejected argument shapes", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('one')`,
			`SELECT optimize(a) FROM t`,
			`SELECT optimize(t,1) FROM t`,
			`SELECT optimize(t)`,
			`CREATE TABLE ord(x)`,
			`INSERT INTO ord VALUES(1)`,
			`SELECT optimize(ord) FROM ord`,
			`SELECT optimize(x) FROM ord`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
		}, 2},

		// A fts3 (not fts4) table, and the command channel and the function
		// form reaching the same index in one script -- the merge core is
		// shared, so this pins that sharing it did not change either one.
		{"fts3, and both spellings against one index", append([]string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta two')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`INSERT INTO t(a) VALUES('gamma three')`,
			`SELECT optimize(t) FROM t LIMIT 1`,
			`SELECT optimize(t) FROM t LIMIT 1`,
		}, fts3OptShadowDump...), 8},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, c.stmts, c.nAccepted)
		})
	}
}

// fts3OptPendingScript is fts3f.test's own mined segment, verbatim (its
// second BEGIN is an error on both engines, exactly as mined). Real fts3
// holds these writes as pending terms and flushes them into their own
// segment before merging; this engine writes the transaction's segment
// eagerly instead (engine/fts3_txn.go), and optimize()'s whole answer is the
// SEGMENT COUNT, so it can only be served once the two models agree on that
// count for every way the transaction could have reached the call.
//
// They agree for THIS script -- the statement sub-transaction that
// engine/fts3_txn.go models splits the two multi-row INSERTs into two
// segments on both engines, and nothing else in the script is an untracked
// statement (fts3TxnMaybeTaint) -- so this engine now serves optimize() here
// instead of declining it (see TestFts3OptimizeFunctionUntaintedMatches). A
// transaction that ALSO runs a statement whose journal this engine does not
// model (most DDL, a write to any other table, anything firing a trigger; the
// list is fts3TxnMaybeTaint's own doc comment) still declines --
// TestFts3OptimizeFunctionTaintedStillDeclines pins that half.
var fts3OptPendingScript = []string{
	`CREATE VIRTUAL TABLE ft USING fts3(x)`,
	`BEGIN`,
	`INSERT INTO ft VALUES('a one'), ('b one'), ('c one')`,
	`SELECT docid FROM ft WHERE ft MATCH 'one'`,
	`INSERT INTO ft VALUES('a one'), ('b one'), ('c one')`,
}

// TestFts3OptimizeFunctionUntaintedMatches pins the fixed half: fts3f.test's
// own mined script now answers 'Index optimized' on BOTH engines, matching
// byte for byte, instead of this engine declining a shape C SQLite
// genuinely serves.
func TestFts3OptimizeFunctionUntaintedMatches(t *testing.T) {
	const call = `SELECT docid, optimize(ft) FROM ft WHERE ft MATCH 'one'`
	stmts := append(append([]string{}, fts3OptPendingScript...), call)

	cgo := run(t, "cgo", stmts)
	for i := range fts3OptPendingScript {
		// The second INSERT is the load-bearing one: without it the pending set
		// is a single segment and the answer would agree by accident.
		if i == 1 || i == 3 {
			continue // BEGIN and the plain SELECT are shape, not setup
		}
		if kind, _ := cgo[i]["kind"].(string); kind == "error" {
			t.Fatalf("cgo REJECTED a setup statement, so this gate would be vacuous: stmt #%d %s", i, stmts[i])
		}
	}
	last := cgo[len(cgo)-1]
	if kind, _ := last["kind"].(string); kind != "rows" {
		t.Fatalf("cgo did not answer optimize() at all (%v) -- this gate assumes C SQLite serves this shape", last)
	}
	rows, _ := last["rows"].([]any)
	if len(rows) == 0 {
		t.Fatalf("cgo answered no rows for %s; expected six", call)
	}
	first := fmt.Sprint(rows[0])
	if !strings.Contains(first, "Index optimized") {
		t.Fatalf("cgo's first row is %s, not 'Index optimized' -- the pending-terms flush this test targets is not happening, so re-derive the rule before trusting this assertion", first)
	}

	mush := run(t, "musql", stmts)
	mushLast := mush[len(mush)-1]
	if kind, _ := mushLast["kind"].(string); kind != "rows" {
		t.Fatalf("musql DECLINED optimize() (%v) for a script nothing untracked interfered with -- see engine/fts3_txn.go's fts3TxnMaybeTaint", mushLast)
	}
	mushRows, _ := mushLast["rows"].([]any)
	if len(mushRows) == 0 || fmt.Sprint(mushRows[0]) != first {
		t.Errorf("optimize() sentence DIVERGES\n  cgo:  %v\n  mush: %v", rows, mushRows)
	}
}

// TestFts3OptimizeFunctionTaintedStillDeclines pins the other half:
// fts3OptPendingScript's own shape, but with ONE extra statement this engine
// does not track (a write to a DIFFERENT table, well within
// fts3TxnMaybeTaint's own documented taint list) inserted before optimize()
// runs. C SQLite still answers cleanly (its pending-terms model does not
// care what else the transaction touched); this engine must keep declining,
// since its own accumulated segment count is no longer positively known to
// match once something untracked has run.
func TestFts3OptimizeFunctionTaintedStillDeclines(t *testing.T) {
	stmts := append(append([]string{}, fts3OptPendingScript...),
		`CREATE TABLE untracked(x)`,
		`SELECT docid, optimize(ft) FROM ft WHERE ft MATCH 'one'`,
	)

	cgo := run(t, "cgo", stmts)
	last := cgo[len(cgo)-1]
	if kind, _ := last["kind"].(string); kind != "rows" {
		t.Fatalf("cgo did not answer optimize() at all (%v) -- this gate assumes C SQLite serves this shape even with the untracked statement present", last)
	}

	mush := run(t, "musql", stmts)
	if kind, _ := mush[len(mush)-1]["kind"].(string); kind != "error" {
		t.Errorf("musql ANSWERED optimize() (%v) after an untracked statement (a CREATE TABLE) ran in the same transaction -- fts3TxnMaybeTaint should have tainted on it", mush[len(mush)-1])
	}
}

// fts3OptInterchangeWrite builds a database whose index was produced BY an
// optimize() call, with enough text to spill past the root node into
// %_segments -- the only place a merge's block numbering is visible.
func fts3OptInterchangeWrite() []string {
	s := []string{`CREATE VIRTUAL TABLE t USING fts4(a)`}
	for i := 0; i < 12; i++ {
		s = append(s, fmt.Sprintf(
			`INSERT INTO t(a) VALUES('doc%02d %s common trailing words to push this segment past one node')`,
			i, strings.Repeat("pad"+string(rune('a'+i))+" ", 12)))
	}
	return append(s,
		`SELECT optimize(t) FROM t LIMIT 1`,
		`DELETE FROM t WHERE docid IN (3,7)`,
		`INSERT INTO t(a) VALUES('late arrival common')`,
		`SELECT optimize(t) FROM t LIMIT 1`)
}

var fts3OptInterchangeRead = []string{
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
	`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'doc05' ORDER BY docid)`,
	`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'doc03' ORDER BY docid)`,
	`SELECT count(*) FROM t`,
}

// TestFts3OptimizeFileInterchange is the load-bearing claim: a database whose
// index an optimize() call PRODUCED must read back identically under both
// engines, whichever one wrote it. A merge that agreed on every sentence and
// laid its blocks out differently fails here.
func TestFts3OptimizeFileInterchange(t *testing.T) {
	write := fts3OptInterchangeWrite()
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "fts.db")
			wrote := runWithDSN(t, writer, dsn, write)
			for i, r := range wrote {
				if kind, _ := r["kind"].(string); kind == "error" {
					t.Fatalf("%s could not run the write script (the comparison would be vacuous): stmt #%d %s", writer, i, write[i])
				}
			}
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, fts3OptInterchangeRead))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s wrote it with optimize()] readers disagree\n  cgo reads:    %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}

// TestFts3OptimizeIsIntactToCSQLite closes the loop the other way: this
// engine does every optimize(), and C SQLite -- which never saw any of it
// happen -- has to integrity_check the FILE, integrity-check the INVERTED INDEX
// through fts3's own command (which re-tokenizes %_content and compares it
// against the live index, so a merge that dropped or duplicated a posting fails
// here where a MATCH would not), and go on writing.
func TestFts3OptimizeIsIntactToCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fts.db")
	write := fts3OptInterchangeWrite()
	wrote := runWithDSN(t, "musql", dsn, write)
	for i, r := range wrote {
		if kind, _ := r["kind"].(string); kind == "error" {
			t.Fatalf("musql could not run the write script: stmt #%d %s", i, write[i])
		}
	}

	sdb, err := sql.Open("sqlite3", exportedForOracle(t, dsn))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	var ic string
	if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "ok" {
		t.Fatalf("C SQLite reports the musql-optimized file as corrupt: %s", ic)
	}
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Fatalf("fts3's own integrity-check rejects the musql-optimized index: %v", err)
	}
	// ...and its own optimize() over what this engine merged must be a no-op
	// that still reports the DONE sentence.
	var sentence string
	if err := sdb.QueryRow(`SELECT optimize(t) FROM t LIMIT 1`).Scan(&sentence); err != nil {
		t.Fatalf("C SQLite could not optimize() the musql-optimized index: %v", err)
	}
	if sentence != "Index already optimal" {
		t.Errorf("C SQLite answers %q over an index musql already merged; want \"Index already optimal\" (it found more than one segment, so the merge left something behind)", sentence)
	}
	if _, err := sdb.Exec(`INSERT INTO t(a) VALUES('written by real sqlite after the merge')`); err != nil {
		t.Fatalf("C SQLite cannot go on writing to the musql-optimized index: %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Fatalf("fts3's integrity-check fails after C SQLite wrote to the musql-optimized index: %v", err)
	}
}

// fts3OptFuzzWords is a small vocabulary with deliberate prefix overlap, so a
// prefix= table's own index is exercised and terms really do collide across
// segments (which is what makes a merge's doclist ordering observable).
var fts3OptFuzzWords = []string{
	"alpha", "alphabet", "alp", "beta", "betamax", "be",
	"gamma", "gam", "delta", "del", "epsilon", "eps",
}

// TestFts3OptimizeFunctionFuzz randomizes what the merge actually sees --
// how many statements wrote segments, which documents were deleted or
// overwritten first, whether a prefix index exists, and whether the call is
// made before or after the level cascade -- because the sentence and the
// merged bytes are both VALUE dependent and a handful of hand-written cases
// that agree prove much less than they look like they do.
func TestFts3OptimizeFunctionFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	for i := 0; i < 40; i++ {
		i := i
		name := fmt.Sprintf("p%02d", i)
		t.Run(name, func(t *testing.T) {
			var s []string
			create := `CREATE VIRTUAL TABLE t USING fts4(a)`
			switch rng.Intn(3) {
			case 1:
				create = `CREATE VIRTUAL TABLE t USING fts4(a, prefix=3)`
			case 2:
				create = `CREATE VIRTUAL TABLE t USING fts3(a)`
			}
			s = append(s, create)

			nDoc := 1 + rng.Intn(20)
			for d := 1; d <= nDoc; d++ {
				var w []string
				for k := 0; k < 1+rng.Intn(4); k++ {
					w = append(w, fts3OptFuzzWords[rng.Intn(len(fts3OptFuzzWords))])
				}
				s = append(s, fmt.Sprintf(`INSERT INTO t(docid,a) VALUES(%d,'%s')`, d, strings.Join(w, " ")))
			}
			// A few mutations, which is what puts delete markers into the
			// segments the merge then has to drop.
			for k := 0; k < rng.Intn(4); k++ {
				d := 1 + rng.Intn(nDoc)
				if rng.Intn(2) == 0 {
					s = append(s, fmt.Sprintf(`DELETE FROM t WHERE docid=%d`, d))
				} else {
					s = append(s, fmt.Sprintf(`UPDATE t SET a='%s' WHERE docid=%d`,
						fts3OptFuzzWords[rng.Intn(len(fts3OptFuzzWords))], d))
				}
			}
			nSetup := len(s)
			s = append(s,
				`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
				`SELECT optimize(t) FROM t LIMIT 1`,
				`SELECT optimize(t) FROM t LIMIT 1`,
				`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
				`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid)`,
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'be*' ORDER BY docid)`,
				`SELECT count(*) FROM t`,
			)
			differAllAccepted(t, name, s, nSetup)
		})
	}
}

// TestFts3OptimizeTaintSelfReferencingDeleteDeclines is a review-caught
// regression: a DELETE whose WHERE clause names the table's OWN column on
// BOTH sides of "docid=" ("docid=x", not a literal) is a shape
// fts3ClassifyDocidEq deliberately leaves fts3DocidEqUnknown (fts3_txn.go's
// own doc comment) -- fts3MutationOpensStmtSubTxn's DEFAULT-UNKNOWN handling
// treats that conservatively for ITS OWN purpose (whether to seal), but
// fts3TxnMaybeTaint must NOT read "already modelled" off the same uncertain
// classification: a real, non-literal WHERE clause forces a statement
// journal (usesStmtJournal) that this engine's own per-statement flush
// timing is not proven to match. Verified live against the oracle: real
// SQLite's DELETE with this WHERE opens a statement journal and flushes at
// OP_Transaction, splitting the pending set into two segments the SAME way
// an untracked statement would -- so a later optimize() answering "Index
// already optimal" here is a served wrong answer, not merely incomplete.
func TestFts3OptimizeTaintSelfReferencingDeleteDeclines(t *testing.T) {
	stmts := []string{
		`CREATE VIRTUAL TABLE ft USING fts3(x)`,
		`BEGIN`,
		`INSERT INTO ft(docid,x) VALUES(10,'d10')`,
		`DELETE FROM ft WHERE docid=x`,
		`INSERT INTO ft(docid,x) VALUES(15,'d15')`,
		`SELECT optimize(ft) FROM ft LIMIT 1`,
	}

	cgo := run(t, "cgo", stmts)
	last := cgo[len(cgo)-1]
	if kind, _ := last["kind"].(string); kind != "rows" {
		t.Fatalf("cgo did not answer optimize() at all (%v) -- this gate assumes C SQLite serves this shape", last)
	}
	rows, _ := last["rows"].([]any)
	if len(rows) == 0 || !strings.Contains(fmt.Sprint(rows[0]), "Index optimized") {
		t.Fatalf("cgo's answer is %v, not 'Index optimized' -- the two-segment premise this test targets is not happening, so re-derive before trusting this assertion", rows)
	}

	mush := run(t, "musql", stmts)
	if kind, _ := mush[len(mush)-1]["kind"].(string); kind != "error" {
		t.Errorf("musql ANSWERED optimize() (%v) after a DELETE with a genuinely UNKNOWN-classified WHERE clause -- fts3TxnMaybeTaint should have tainted rather than trust the docid-based seal decision", mush[len(mush)-1])
	}
}

// The ATTACHed-name-collision sibling of the review-caught regression above
// (fts3TxnMaybeTaint misreading a write to an attached database's
// same-named table as the local table's own DML) is gated at the ENGINE
// level instead of here: driver has its own connection-level routing
// that opens a fresh, direct session against the attached FILE for an
// autocommit schema-qualified statement, never reaching
// execRoutedToAttached/insert_write.go at all -- so a driver-level
// (database/sql, this file's own run()/differ() harness) test cannot
// exercise the code path the regression is actually in. See
// engine/fts3_txn_attach_test.go's TestFts3OptimizeTaintAttachedNameCollisionDeclines,
// which drives db.Exec() directly the same way
// TestCrossDatabaseWriteRoutesToTheAttachedSession (attach_write_test.go)
// does for the analogous reason.
