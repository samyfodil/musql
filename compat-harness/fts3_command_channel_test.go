// This file tests fts3/fts4 command channel ("INSERT INTO t(t) VALUES('optimize')")
// against C SQLite, comparing shadow table bytes.
package compat

import "testing"

// fts3CmdDump queries the fts4 shadow tables.
var fts3CmdDump = []string{
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT rowid, level, idx FROM t_segdir ORDER BY rowid`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(c0a) FROM t_content ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
}

func TestFts3CommandChannelDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"optimize merges every segment into one at level 0", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta two')`,
			`INSERT INTO t(a) VALUES('gamma three')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`SELECT docid FROM t WHERE t MATCH 'two'`,
		}, fts3CmdDump...)},
		// The load-bearing case for the merge rule: 'optimize' drops the delete
		// markers it reads (nothing older survives them), and the term whose
		// only document was deleted disappears entirely rather than staying as
		// an empty doclist.
		{"optimize drops delete markers and emptied terms", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta one')`,
			`INSERT INTO t(a) VALUES('gamma one')`,
			`DELETE FROM t WHERE docid=2`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`SELECT docid, a FROM t WHERE t MATCH 'one' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'beta'`,
		}, fts3CmdDump...)},
		// A table with one segment (or none) is left completely alone -- fts3's
		// SQLITE_DONE shortcut, which the INSERT form reports as success.
		{"optimize on an empty and a one-segment table changes nothing", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`INSERT INTO t(a) VALUES('only one')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`INSERT INTO t(t) VALUES('OPTIMIZE')`,
		}, fts3CmdDump...)},
		// The output level is the greatest level PRESENT, not 0: 18 inserts
		// leave L0 idx 0,1 plus the L1 segment the 17th merged, and optimize
		// puts the result at L1 idx 0.
		{"optimize lands at the greatest level present", func() []string {
			s := []string{`CREATE VIRTUAL TABLE t USING fts4(a)`}
			for i := 0; i < 18; i++ {
				s = append(s, `INSERT INTO t(a) VALUES('word`+string(rune('a'+i))+` common')`)
			}
			s = append(s,
				`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
				`INSERT INTO t(t) VALUES('optimize')`,
				`SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid`)
			return append(s, fts3CmdDump...)
		}()},
		// A spilling optimize over inputs that themselves spilled: the output's
		// %_segments blocks are numbered from what the table holds BEFORE the
		// inputs are freed, and the next ordinary INSERT continues from there.
		{"optimize spills into %_segments", func() []string {
			s := []string{`PRAGMA page_size=512`, `CREATE VIRTUAL TABLE t USING fts4(a)`}
			for i := 0; i < 6; i++ {
				txt := ""
				for j := 0; j < 60; j++ {
					txt += "tok" + string(rune('a'+i)) + string(rune('a'+j/26)) + string(rune('a'+j%26)) + " "
				}
				s = append(s, `INSERT INTO t(a) VALUES('`+txt+`')`)
			}
			s = append(s,
				`INSERT INTO t(t) VALUES('optimize')`,
				`INSERT INTO t(a) VALUES('zzz')`,
				`SELECT docid FROM t WHERE t MATCH 'tokaaa' ORDER BY docid`,
				`SELECT docid FROM t WHERE t MATCH 'tokfbh' ORDER BY docid`)
			return append(s, fts3CmdDump...)
		}()},
		{"rebuild recomputes the index, %_docsize and %_stat", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('one two')`,
			`INSERT INTO t VALUES('four')`,
			`DELETE FROM t WHERE docid=1`,
			`INSERT INTO t(t) VALUES('rebuild')`,
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'four'`,
			`SELECT docid FROM t WHERE t MATCH 'one'`,
		}, fts3CmdDump...)},
		// rebuild REPAIRS an index corrupted behind fts3's back: %_segdir is
		// rebuilt from %_content, so the MATCH that found nothing finds the row.
		{"rebuild repairs a wiped index", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('x y')`,
			`DELETE FROM t_segdir`,
			`SELECT docid FROM t WHERE t MATCH 'x'`,
			`INSERT INTO t(t) VALUES('rebuild')`,
			`SELECT docid FROM t WHERE t MATCH 'x'`,
		}, fts3CmdDump...)},
		{"rebuild on an empty table still writes %_stat", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(t) VALUES('rebuild')`,
		}, fts3CmdDump...)},
		// integrity-check: the two failures, and the two near-misses that must
		// NOT fire (%_stat and %_docsize are not part of the check).
		{"integrity-check", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('alpha one')`,
			`INSERT INTO t VALUES('beta two')`,
			`DELETE FROM t WHERE docid=1`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`INSERT INTO t_content(docid, c0a) VALUES(50,'orphan')`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`DELETE FROM t_content WHERE docid=50`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`UPDATE t_stat SET value=X'FF' WHERE id=0`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`DELETE FROM t_docsize`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`DELETE FROM t_segdir`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`INSERT INTO t(t) VALUES('rebuild')`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}},
		// The command NAME is matched case-insensitively with NO trimming, a
		// NULL in the slot is an ordinary all-NULL row rather than a command,
		// and every other column of a command INSERT is ignored.
		{"command forms", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(t) VALUES('nosuchcommand')`,
			`INSERT INTO t(t) VALUES(' optimize')`,
			`INSERT INTO t(t) VALUES('optimize ')`,
			`INSERT INTO t(t) VALUES(7)`,
			`INSERT INTO t(t) VALUES(NULL)`,
			`SELECT count(*) FROM t`,
			`SELECT docid, quote(a) FROM t ORDER BY docid`,
			`INSERT INTO t(t,a) VALUES('optimize','x')`,
			`INSERT INTO t(a,t) VALUES('x','optimize')`,
			`INSERT INTO t(t,docid) VALUES('optimize',99)`,
			`INSERT INTO t(t) VALUES('optimize'),('optimize')`,
			`INSERT INTO t(t) SELECT 'optimize'`,
			`SELECT count(*) FROM t`,
		}, fts3CmdDump...)},
		// A command inside a transaction consumes the segment the transaction
		// was filling, so the next statement opens a fresh one: two %_segdir
		// rows at COMMIT, not one.
		{"a command inside a transaction seals the segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('alpha')`,
			`INSERT INTO t VALUES('beta')`,
			`BEGIN`,
			`INSERT INTO t VALUES('gamma')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`INSERT INTO t VALUES('delta')`,
			`COMMIT`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'delta'`,
		}},
		{"fts3 has the same command channel", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(a) VALUES('alpha one')`,
			`INSERT INTO t(a) VALUES('beta two')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`INSERT INTO t(t) VALUES('rebuild')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`SELECT type,name FROM sqlite_master ORDER BY name`,
		}},
		// The disambiguation the whole channel rests on: C fts3 declares a
		// hidden column named after the table, so a DECLARED one of that name
		// makes the CREATE fail on both engines.
		{"a column named after the table is refused", []string{
			`CREATE VIRTUAL TABLE tt USING fts4(tt)`,
			`CREATE VIRTUAL TABLE uu USING fts3(uu)`,
			`CREATE VIRTUAL TABLE vv USING fts4(a, VV)`,
			`SELECT type,name FROM sqlite_master ORDER BY name`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestFts3CommandsAgree covers the two commands this file once pinned as
// DELIBERATE DECLINES. They are declines no longer: "merge=X,Y" performs a
// real incremental merge (engine/fts3_incrmerge.go) and
// "automerge=N" wires the real automatic-merge trigger
// (engine/fts3_automerge.go). The assertion is therefore inverted
// -- from "this engine must refuse it" to "this engine must agree with the
// oracle, down to the shadow bytes", which is this file's own standard.
//
// The old test outlived the features by 138 commits, asserting a decline the
// engine had stopped issuing, and was RED on main that entire time (verified
// by checking it out at 40d6b8f, the automerge commit itself). Its stated
// reason -- "it switches on automatic incremental merging for every LATER
// write, which this engine does not do" -- had simply stopped being true.
//
// The trailing write and MATCH are the point: a command that desynchronized
// the two databases would not show up on the command statement itself, only
// on what came after it. That is also the shape of the bug this test's
// replacement caught for real (see fts3_automerge_verbatim_test.go: writes
// made under automerge were being discarded at Close).
func TestFts3CommandsAgree(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"merge=", `INSERT INTO t(t) VALUES('merge=1000,2')`},
		{"merge= with defaults", `INSERT INTO t(t) VALUES('merge=2,2')`},
		{"automerge=", `INSERT INTO t(t) VALUES('automerge=4')`},
	}
	for _, c := range cases {
		differ(t, "fts3 command agrees: "+c.name, []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(a) VALUES('one')`,
			`INSERT INTO t(a) VALUES('two')`,
			c.cmd,
			`SELECT docid FROM t WHERE t MATCH 'one' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'two' ORDER BY docid`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, typeof(end_block), quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			// What the command did to LATER writes is the half a decline test
			// could never have checked.
			`INSERT INTO t(a) VALUES('three')`,
			`SELECT docid FROM t WHERE t MATCH 'three' ORDER BY docid`,
			`SELECT level, idx FROM t_segdir ORDER BY level, idx`,
			`PRAGMA integrity_check`,
		})
	}
}
