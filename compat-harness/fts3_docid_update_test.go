// Tests UPDATE statements that change an fts3/fts4 table's docid/rowid.
// Docid changes are implemented as DELETE followed by INSERT, and segment
// creation depends on whether the docid moves up or down in sort order.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestFts3DocidUpdateDiff(t *testing.T) {
	cases := []struct {
		name      string
		stmts     []string
		nAccepted int
	}{
		// The mined shape itself, and the control that the two-agent
		// investigation left unverified: moving the docid UP stays in ONE
		// segment (an ordinary delete-marker-then-insert doclist entry, same
		// as an in-place same-docid UPDATE), where DOWN needs two.
		{"docid moves up: one segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(docid,x) VALUES(1,'alpha beta')`,
			`INSERT INTO t(docid,x) VALUES(9,'gamma delta')`,
			`UPDATE t SET docid=2 WHERE docid=1`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}, 4},
		{"docid moves down: two segments", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(docid,x) VALUES(1,'alpha beta')`,
			`INSERT INTO t(docid,x) VALUES(9,'gamma delta')`,
			`UPDATE t SET docid=2 WHERE docid=9`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}, 4},

		// The wipe rule's own control: an UPDATE of the table's SOLE row's
		// docid must NOT wipe, unlike deleteRow's ordinary path.
		{"docid change over the table's only row does not wipe", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(docid,x) VALUES(7,'alpha beta')`,
			`UPDATE t SET docid=3 WHERE docid=7`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}, 3},
		{"docid change over the table's only row, fts3 (no %_docsize/%_stat)", []string{
			`CREATE VIRTUAL TABLE t USING fts3(x)`,
			`INSERT INTO t(docid,x) VALUES(7,'alpha beta')`,
			`UPDATE t SET docid=3 WHERE docid=7`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, x FROM t ORDER BY docid`,
		}, 3},

		// A SAME-docid UPDATE reached through this new code path (the SET
		// clause names docid, but the computed value equals the row's own)
		// must still merge into ONE ordinary doclist entry, not a marker
		// plus a second one -- and must NOT skip the wipe check the way a
		// genuinely-changing docid does, since the row never actually moves.
		{"docid=docid is an ordinary in-place update, wipe included", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(docid,x) VALUES(4,'alpha beta')`,
			`UPDATE t SET docid=docid, x='alpha gamma' WHERE docid=4`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}, 3},

		// A multi-row UPDATE whose rows interleave backward AMONG
		// THEMSELVES (not just within one row's own two halves) -- fts3conf's
		// own T4 shape, minus the OR clause and the conflict it strikes
		// there (bucket 3, a different file). Two rows moving to fresh,
		// non-conflicting docids in a way that undercuts the FIRST row's own
		// new docid: two segments, one per source row's term.
		{"a multi-row UPDATE with an inter-row backward transition", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(docid,x) VALUES(1,'w1')`,
			`INSERT INTO t(docid,x) VALUES(2,'w2')`,
			`INSERT INTO t(docid,x) VALUES(5,'w5')`,
			`UPDATE t SET docid = CASE WHEN docid=1 THEN 4 ELSE 3 END WHERE docid<=2`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}, 5},

		// The mid-statement flush point folds into (and seals) whatever the
		// TRANSACTION is already accumulating for this table, the same as
		// fts3_txn.go's insert-side rule -- checked strictly AFTER COMMIT,
		// since this engine's eager mid-transaction rewrite is a known,
		// pre-existing, out-of-scope gap from what C fts3 shows at that
		// exact point (this file's own header comment).
		{"the flush folds into the transaction's own accumulating segment", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(x)`,
			`INSERT INTO t1(docid,x) VALUES(5,'w5')`,
			`BEGIN`,
			`INSERT INTO t1(docid,x) VALUES(10,'w10')`,
			`UPDATE t1 SET docid=99 WHERE docid=5`,
			`COMMIT`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t1_segdir ORDER BY level, idx`,
			`SELECT docid, x FROM t1 ORDER BY docid`,
			`INSERT INTO t1(t1) VALUES('integrity-check')`,
		}, 6},

		// "rowid"/"oid"/"_rowid_" are NOT rowid synonyms for a SET target on
		// an fts3/fts4 table (this file's own header comment): each is a
		// silent no-op, its RHS never evaluated, and the row's docid is
		// untouched -- unlike "docid" itself, the one spelling that reaches
		// fts3 at all.
		{"rowid= is a no-op, RHS never evaluated", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'x','y')`,
			`UPDATE t SET rowid=1/0 WHERE docid=1`,
			`SELECT docid,a,b FROM t ORDER BY docid`,
		}, 3},
		{"rowid= alongside a real column change", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'x','y')`,
			`UPDATE t SET rowid='hello', a='z' WHERE docid=1`,
			`SELECT docid,a,b FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
		}, 3},
		{"oid= is a no-op too", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'x')`,
			`UPDATE t SET oid=9 WHERE docid=1`,
			`SELECT docid,a FROM t ORDER BY docid`,
		}, 3},
		{"_rowid_= is a no-op too", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'x')`,
			`UPDATE t SET _rowid_=9 WHERE docid=1`,
			`SELECT docid,a FROM t ORDER BY docid`,
		}, 3},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, c.stmts, c.nAccepted)
		})
	}
}

// TestFts3DocidUpdateConflictErrors is the negative half of the conflict
// case above: the UPDATE itself must ERROR on both engines (differAllAccepted
// cannot express "this statement fails"), and the table must be provably
// unchanged afterward.
func TestFts3DocidUpdateConflictErrors(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		bad   string
		after []string
	}{
		{"a fresh two-row table", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(docid,x) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,x) VALUES(2,'beta')`,
		}, `UPDATE t SET docid=2 WHERE docid=1`, []string{
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
		}},
		// fts4onepass.test's own bracket -- verbatim mined SQL text -- which
		// is a conflict over ITS seed data for every OR-clause variant that
		// file tries (do_catchsql_test asserts "constraint failed" for all of
		// them), not only the default OR ABORT this decline covers.
		{"the exact fts4onepass.test bracket", []string{
			`CREATE VIRTUAL TABLE ft2 USING fts4`,
			`INSERT INTO ft2(rowid, content) VALUES(1, 'a b c')`,
			`INSERT INTO ft2(rowid, content) VALUES(2, 'a b d')`,
			`INSERT INTO ft2(rowid, content) VALUES(3, 'a b e')`,
		}, `UPDATE ft2 SET docid=2 WHERE docid=1`, []string{
			`SELECT rowid, content FROM ft2 ORDER BY rowid`,
			`SELECT level, idx, quote(root) FROM ft2_segdir ORDER BY level, idx`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name+": setup accepted", c.setup, len(c.setup))
			full := append(append(append([]string{}, c.setup...), c.bad), c.after...)
			if !differ(t, c.name+": conflicting UPDATE errors on both engines", full) {
				t.Fatal("engines disagree about the conflicting UPDATE or its aftermath")
			}
		})
	}
}

// TestFts3DocidUpdateFileInterchange is the load-bearing claim: a database
// whose segmentation a docid-changing UPDATE decided must read back
// identically under both engines, whichever one wrote it -- down to the
// segment blobs, and past C SQLite's own integrity_check and fts3
// integrity-check/optimize commands.
func TestFts3DocidUpdateFileInterchange(t *testing.T) {
	write := []string{
		`CREATE VIRTUAL TABLE t USING fts4(x)`,
		`INSERT INTO t(docid,x) VALUES(1,'alpha beta')`,
		`INSERT INTO t(docid,x) VALUES(9,'gamma delta')`,
		`INSERT INTO t(docid,x) VALUES(20,'alpha epsilon')`,
		`UPDATE t SET docid=2 WHERE docid=9`,   // downwards: two segments
		`UPDATE t SET docid=50 WHERE docid=20`, // upwards: one
		`BEGIN`,
		`INSERT INTO t(docid,x) VALUES(60,'zeta')`,
		`UPDATE t SET docid=3 WHERE docid=1`,
		`COMMIT`,
	}
	read := []string{
		`SELECT docid, x FROM t ORDER BY docid`,
		`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
		`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
		`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid)`,
		`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'zeta' ORDER BY docid)`,
	}
	fileInterchangeRoundTrip(t, "docid-update", write, read)
}

// fileInterchangeRoundTrip is the shared shape TestFts3DocidUpdateFileInterchange
// and this project's other fts3 file-interchange gates all use: write with
// each engine in turn, read back with both, and require agreement. Kept here
// rather than duplicated because this package's existing versions
// (fts3_stmt_subtxn_test.go, fts3_replace_conflict_test.go) are inlined
// per-file rather than shared.
func fileInterchangeRoundTrip(t *testing.T, name string, write, read []string) {
	t.Helper()
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := t.TempDir() + "/" + name + ".db"
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
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, read))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s wrote it] readers disagree\n  baseline: %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}

// TestFts3DocidUpdateFuzzDiff randomizes the write history -- how many seed
// rows exist, whether a docid-changing UPDATE lands inside an explicit
// transaction (mixed with an ordinary single-row INSERT ahead of it, so the
// mid-statement flush's cross-statement seed is exercised too), and whether
// an ordinary (non-docid) UPDATE is interleaved -- because the segmentation
// is history-dependent and a handful of hand-written scripts prove far less
// than they look like they do.
func TestFts3DocidUpdateFuzzDiff(t *testing.T) {
	nIter := 300
	if testing.Short() {
		nIter = 60
	}
	words := []string{"aa", "bb", "cc", "dd", "ee"}
	rng := rand.New(rand.NewSource(20260806))
	text := func() string {
		n := 1 + rng.Intn(2)
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, words[rng.Intn(len(words))])
		}
		return strings.Join(parts, " ")
	}
	for iter := 0; iter < nIter; iter++ {
		schema := `CREATE VIRTUAL TABLE t USING fts4(x)`
		switch rng.Intn(3) {
		case 1:
			schema = `CREATE VIRTUAL TABLE t USING fts3(x)`
		case 2:
			schema = `CREATE VIRTUAL TABLE t USING fts4(x, prefix="2")`
		}
		stmts := []string{schema}
		docid := int64(1)
		nSeed := 2 + rng.Intn(4)
		for k := 0; k < nSeed; k++ {
			stmts = append(stmts, fmt.Sprintf(`INSERT INTO t(docid,x) VALUES(%d,'%s')`, docid, text()))
			docid++
		}
		inTxn := rng.Intn(2) == 0
		if inTxn {
			stmts = append(stmts, `BEGIN`)
		}
		nOps := 1 + rng.Intn(3)
		for k := 0; k < nOps; k++ {
			old := 1 + rng.Int63n(docid-1)
			switch rng.Intn(3) {
			case 0, 1:
				newD := docid
				docid++
				stmts = append(stmts, fmt.Sprintf(`UPDATE t SET docid=%d WHERE docid=%d`, newD, old))
			default:
				stmts = append(stmts, fmt.Sprintf(`UPDATE t SET x='%s' WHERE docid=%d`, text(), old))
			}
		}
		if inTxn {
			stmts = append(stmts, `COMMIT`)
		}
		stmts = append(stmts,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT count(*) FROM t`,
		)
		for _, w := range words {
			stmts = append(stmts, fmt.Sprintf(`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH '%s' ORDER BY docid)`, w))
		}
		if !differ(t, fmt.Sprintf("docid-update-fuzz/%d", iter), stmts) {
			t.Fatalf("stopping at the first divergent history (iteration %d)", iter)
		}
	}
}
