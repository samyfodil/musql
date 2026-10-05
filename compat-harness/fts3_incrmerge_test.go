// Differential tests of fts3/fts4's "merge=A,B" command channel
// for no-op merges. Real-work merges are still declined.
package compat

import "testing"

// fts3ShadowDump reads back everything a merge could have touched.
func fts3ShadowDump(tbl string) []string {
	return []string{
		"SELECT name FROM sqlite_master ORDER BY name",
		"SELECT level, idx, start_block, leaves_end_block, end_block, typeof(end_block), quote(root) FROM " + tbl + "_segdir ORDER BY level, idx",
		"SELECT blockid, quote(block) FROM " + tbl + "_segments ORDER BY blockid",
	}
}

func TestFts3IncrmergeNoWork(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// One segment per level cannot reach MAX(2,nMin), so nothing merges.
		{"fts4 one segment, nothing to merge", append([]string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a)`,
			`INSERT INTO t1 VALUES('one two three')`,
			`INSERT INTO t1(t1) VALUES('merge=1000,2')`,
			`SELECT id, quote(value) FROM t1_stat ORDER BY id`,
		}, fts3ShadowDump("t1")...)},
		// fts3DoIncrmerge creates %_stat on an fts3 table before merging --
		// "if( !p->bHasStat ) sqlite3Fts3CreateStatTable(p)" -- so t2_stat
		// appears in sqlite_master even though not one segment moved.
		{"fts3 merge= creates %_stat", append([]string{
			`CREATE VIRTUAL TABLE t2 USING fts3(a)`,
			`INSERT INTO t2 VALUES('alpha')`,
			`SELECT name FROM sqlite_master ORDER BY name`,
			`INSERT INTO t2(t2) VALUES('merge=100,2')`,
			`SELECT id, quote(value) FROM t2_stat ORDER BY id`,
			`SELECT count(*) FROM t2_stat`,
		}, fts3ShadowDump("t2")...)},
		// A second merge= over %_stat is still a no-op.
		{"fts3 merge= twice", append([]string{
			`CREATE VIRTUAL TABLE t3 USING fts3(a, b)`,
			`INSERT INTO t3 VALUES('x y', 'z')`,
			`INSERT INTO t3(t3) VALUES('merge=10,4')`,
			`INSERT INTO t3(t3) VALUES('merge=10,4')`,
			`SELECT id, quote(value) FROM t3_stat ORDER BY id`,
		}, fts3ShadowDump("t3")...)},
		// merge=0 is a no-op.
		{"merge=0 does nothing even with mergeable levels", append([]string{
			`CREATE VIRTUAL TABLE t4 USING fts4(a)`,
			`INSERT INTO t4 VALUES('one')`,
			`INSERT INTO t4 VALUES('two')`,
			`SELECT count(*) FROM t4_segdir`,
			`INSERT INTO t4(t4) VALUES('merge=0,2')`,
		}, fts3ShadowDump("t4")...)},
		// An EMPTY table has no %_segdir rows at all.
		{"empty table", append([]string{
			`CREATE VIRTUAL TABLE t5 USING fts4(a)`,
			`INSERT INTO t5(t5) VALUES('merge=1000,2')`,
			`SELECT id, quote(value) FROM t5_stat ORDER BY id`,
		}, fts3ShadowDump("t5")...)},
		// merge= inside a transaction does not flush pending terms.
		{"merge= does not seal the transaction's segment", append([]string{
			`CREATE VIRTUAL TABLE t6 USING fts4(a)`,
			`BEGIN`,
			`INSERT INTO t6 VALUES('one')`,
			`INSERT INTO t6(t6) VALUES('merge=1000,2')`,
			`INSERT INTO t6 VALUES('two')`,
			`COMMIT`,
			`SELECT count(*) FROM t6_segdir`,
		}, fts3ShadowDump("t6")...)},
		// Malformed parameters are errors.
		{"malformed parameters are errors", append([]string{
			`CREATE VIRTUAL TABLE t7 USING fts4(a)`,
			`INSERT INTO t7 VALUES('one')`,
			`INSERT INTO t7(t7) VALUES('merge=x')`,
			`INSERT INTO t7(t7) VALUES('merge=2x')`,
			`INSERT INTO t7(t7) VALUES('merge=1,2,3')`,
			`INSERT INTO t7(t7) VALUES('merge=10,1')`,
			`INSERT INTO t7(t7) VALUES('merge=10,')`,
			`INSERT INTO t7(t7) VALUES('merge= 10,2')`,
			`INSERT INTO t7(t7) VALUES('merge=')`,
			`SELECT count(*) FROM t7`,
		}, fts3ShadowDump("t7")...)},
		// merge= is case-insensitive and ignores extra columns.
		{"case and extra columns", append([]string{
			`CREATE VIRTUAL TABLE t8 USING fts4(a)`,
			`INSERT INTO t8 VALUES('one')`,
			`INSERT INTO t8(t8, a) VALUES('MERGE=1000,2', 'ignored')`,
			`SELECT count(*) FROM t8`,
			`SELECT changes()`,
		}, fts3ShadowDump("t8")...)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}

// TestFts3IncrmergeRealWorkAgreesWithOracle verifies real merges that do work.
func TestFts3IncrmergeRealWorkAgreesWithOracle(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// Two level-0 segments merge into one at level 1.
		{"two segments, nMin=2", append([]string{
			`CREATE VIRTUAL TABLE m1 USING fts4(a)`,
			`INSERT INTO m1 VALUES('one')`,
			`INSERT INTO m1 VALUES('two')`,
			`INSERT INTO m1(m1) VALUES('merge=1000,2')`,
		}, fts3ShadowDump("m1")...)},
		// Eight segments reach the default merge threshold.
		{"eight segments, default nMin", append([]string{
			`CREATE VIRTUAL TABLE m2 USING fts4(a)`,
			`INSERT INTO m2 VALUES('a1')`,
			`INSERT INTO m2 VALUES('a2')`,
			`INSERT INTO m2 VALUES('a3')`,
			`INSERT INTO m2 VALUES('a4')`,
			`INSERT INTO m2 VALUES('a5')`,
			`INSERT INTO m2 VALUES('a6')`,
			`INSERT INTO m2 VALUES('a7')`,
			`INSERT INTO m2 VALUES('a8')`,
			`INSERT INTO m2(m2) VALUES('merge=1000')`,
		}, fts3ShadowDump("m2")...)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}

// TestFts3IncrmergeStillDeclinesRealWork verifies that unsupported merges error.
func TestFts3IncrmergeStillDeclinesRealWork(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		merge string
	}{
		// A %_stat hint row names a level to resume merging.
		{"a %_stat hint alone is work", []string{
			`CREATE VIRTUAL TABLE m3 USING fts4(a)`,
			`INSERT INTO m3 VALUES('one')`,
			`INSERT INTO m3_stat VALUES(1, x'0002')`,
		}, `INSERT INTO m3(m3) VALUES('merge=1000,2')`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			stmts := append(append([]string{}, c.setup...), c.merge)
			res := run(t, "musql", stmts)
			if len(res) != len(stmts) {
				t.Fatalf("got %d results for %d statements", len(res), len(stmts))
			}
			for i, r := range res[:len(res)-1] {
				if r["kind"] == "error" {
					t.Fatalf("setup statement %d (%s) errored -- this gate would pass vacuously", i, stmts[i])
				}
			}
			if got := res[len(res)-1]["kind"]; got != "error" {
				t.Errorf("%s: pure engine answered %q, want error -- a merge= with segments to merge must be declined, never silently accepted as a no-op", c.merge, got)
			}
		})
	}
}
