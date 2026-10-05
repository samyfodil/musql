// Tests the fts4 prefix index option. Prefix indexes are derived data, so
// tests verify the structure of index metadata, not query results.
package compat

import "testing"

func TestFts3PrefixDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// Extra %_segdir rows for prefix indexes at each specified length.
		{"two prefix indexes over two columns", []string{
			`CREATE VIRTUAL TABLE p USING fts4(a, b, prefix="2,3")`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`INSERT INTO p VALUES('one two','three four')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM p_stat ORDER BY id`,
			`SELECT docid, quote(size) FROM p_docsize ORDER BY docid`,
			`INSERT INTO p VALUES('onerous','threefold')`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT docid FROM p WHERE p MATCH 'on*'`,
			`SELECT docid FROM p WHERE p MATCH 'on'`,
			`SELECT docid FROM p WHERE p MATCH 'one'`,
			`SELECT docid FROM p WHERE p MATCH 'b:thr*'`,
			`SELECT quote(matchinfo(p)) FROM p WHERE p MATCH 'one'`,
			`INSERT INTO p(p) VALUES('integrity-check')`,
		}},
		// Prefix truncation is by byte count, not character count.
		{"the prefix is bytes, not characters", []string{
			`CREATE VIRTUAL TABLE p2 USING fts4(x, prefix=2)`,
			`CREATE VIRTUAL TABLE p3 USING fts4(x, prefix=3)`,
			`INSERT INTO p2 VALUES('éàx ab')`,
			`INSERT INTO p3 VALUES('éàx ab')`,
			`SELECT level, idx, quote(root) FROM p2_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM p3_segdir ORDER BY level, idx`,
			`INSERT INTO p2(p2) VALUES('integrity-check')`,
			`INSERT INTO p3(p3) VALUES('integrity-check')`,
		}},
		// A term SHORTER than the length is left out of that index; one exactly
		// that long is in.
		{"a term shorter than the prefix is skipped", []string{
			`CREATE VIRTUAL TABLE p USING fts4(x, prefix=5)`,
			`INSERT INTO p VALUES('ab abc abcd abcde abcdef')`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`INSERT INTO p VALUES('zz')`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM p_stat ORDER BY id`,
			`INSERT INTO p(p) VALUES('integrity-check')`,
		}},
		// Positional, not sorted and not deduplicated -- except for a zero,
		// which is dropped and COMPACTS the ones after it.
		{"the list is positional; a zero is dropped", []string{
			`CREATE VIRTUAL TABLE a1 USING fts4(x, prefix="3,3,2")`,
			`CREATE VIRTUAL TABLE a2 USING fts4(x, prefix="1,600,2")`,
			`CREATE VIRTUAL TABLE a3 USING fts4(x, prefix="0,2")`,
			`CREATE VIRTUAL TABLE a4 USING fts4(x, prefix="1,1")`,
			`CREATE VIRTUAL TABLE a5 USING fts4(x, prefix="0")`,
			`CREATE VIRTUAL TABLE a6 USING fts4(x, prefix="")`,
			`CREATE VIRTUAL TABLE a7 USING fts4(x, prefix=)`,
			`CREATE VIRTUAL TABLE a8 USING fts4(x, prefix="2147483647,2147483648,2147483649")`,
			// fts3GobbleInt reads a maximal DIGIT RUN and then skips one
			// character unexamined, so these are accepted, not errors.
			`CREATE VIRTUAL TABLE a9 USING fts4(x, prefix="1.5")`,
			`CREATE VIRTUAL TABLE aa USING fts4(x, prefix="1.5,2")`,
			`CREATE VIRTUAL TABLE ab USING fts4(x, prefix="12x3")`,
			`CREATE VIRTUAL TABLE ac USING fts4(x, prefix="3 ")`,
			`INSERT INTO a1 VALUES('abcdef')`,
			`INSERT INTO a2 VALUES('abcdef')`,
			`INSERT INTO a3 VALUES('abcdef')`,
			`INSERT INTO a4 VALUES('abcdef')`,
			`INSERT INTO a5 VALUES('abcdef')`,
			`INSERT INTO a6 VALUES('abcdef')`,
			`INSERT INTO a7 VALUES('abcdef')`,
			`INSERT INTO a8 VALUES('abcdef')`,
			`SELECT level, idx, quote(root) FROM a1_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM a2_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM a3_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM a4_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM a5_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM a6_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM a7_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM a8_segdir ORDER BY level, idx`,
			`INSERT INTO a9 VALUES('abcdefghijklmnopqrstuvwxyz')`,
			`INSERT INTO aa VALUES('abcdefghijklmnopqrstuvwxyz')`,
			`INSERT INTO ab VALUES('abcdefghijklmnopqrstuvwxyz')`,
			`INSERT INTO ac VALUES('abcdefghijklmnopqrstuvwxyz')`,
			`SELECT level, idx, quote(root) FROM a9_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM aa_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM ab_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM ac_segdir ORDER BY level, idx`,
		}},
		// The spellings C fts3 REFUSES; both engines must fail the CREATE
		// and leave nothing behind.
		{"refused prefix spellings", []string{
			`CREATE VIRTUAL TABLE b1 USING fts4(x, prefix="abc")`,
			`CREATE VIRTUAL TABLE b2 USING fts4(x, prefix="-1")`,
			`CREATE VIRTUAL TABLE b3 USING fts4(x, prefix="1, 2")`,
			`CREATE VIRTUAL TABLE b4 USING fts4(x, prefix="1,2,")`,
			`CREATE VIRTUAL TABLE b5 USING fts4(x, prefix=",1")`,
			`CREATE VIRTUAL TABLE b6 USING fts4(x, prefix="1x,2")`,
			`CREATE VIRTUAL TABLE b7 USING fts4(x, prefix="  2")`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		// A DELETE writes a marker segment into EVERY index, and an UPDATE
		// re-inserts into every index; emptying the table wipes them all.
		{"DELETE, UPDATE and the wipe reach every index", []string{
			`CREATE VIRTUAL TABLE p USING fts4(a, b, prefix=2)`,
			`INSERT INTO p VALUES('one two','three four')`,
			`INSERT INTO p VALUES('onerous','threefold')`,
			`INSERT INTO p VALUES('five','six')`,
			`DELETE FROM p WHERE docid=1`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM p_stat ORDER BY id`,
			`SELECT docid FROM p WHERE p MATCH 'one'`,
			`SELECT docid FROM p WHERE p MATCH 'on*'`,
			`UPDATE p SET a='eleven twelve' WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT docid, a, b FROM p ORDER BY docid`,
			`INSERT INTO p(p) VALUES('integrity-check')`,
			`DELETE FROM p`,
			`SELECT count(*) FROM p_segdir`,
			`SELECT id, quote(value) FROM p_stat ORDER BY id`,
		}},
		// optimize merges each index separately and swallows the SQLITE_DONE a
		// single-segment one returns; rebuild rebuilds them all.
		{"optimize is per index, and skips a single-segment one", []string{
			`CREATE VIRTUAL TABLE w USING fts4(x, prefix=5)`,
			`INSERT INTO w VALUES('ab')`,
			`INSERT INTO w VALUES('cd')`,
			`INSERT INTO w VALUES('abcdef')`,
			`SELECT level, idx, quote(root) FROM w_segdir ORDER BY level, idx`,
			`INSERT INTO w(w) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM w_segdir ORDER BY level, idx`,
			`INSERT INTO w(w) VALUES('integrity-check')`,
			`INSERT INTO w(w) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM w_segdir ORDER BY level, idx`,
			`SELECT blockid FROM w_segments ORDER BY blockid`,
			`INSERT INTO w(w) VALUES('integrity-check')`,
		}},
		{"optimize merges every index when each has several", []string{
			`CREATE VIRTUAL TABLE z USING fts4(x, prefix="2,3")`,
			`INSERT INTO z VALUES('alpha')`,
			`INSERT INTO z VALUES('alpine')`,
			`INSERT INTO z VALUES('beta')`,
			`INSERT INTO z(z) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM z_segdir ORDER BY level, idx`,
			`INSERT INTO z(z) VALUES('integrity-check')`,
			`SELECT docid FROM z WHERE z MATCH 'al*'`,
			`INSERT INTO z(z) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM z_segdir ORDER BY level, idx`,
		}},
		// fts4aux reads the TERM index only -- the prefix terms must not leak
		// into it, and neither must they into a MATCH.
		{"fts4aux and MATCH see the term index only", []string{
			`CREATE VIRTUAL TABLE z USING fts4(x, prefix="2,3")`,
			`INSERT INTO z VALUES('alpha')`,
			`INSERT INTO z VALUES('alpine')`,
			`INSERT INTO z VALUES('beta')`,
			`CREATE VIRTUAL TABLE zaux USING fts4aux(z)`,
			`SELECT term, col, documents, occurrences FROM zaux`,
			`SELECT docid FROM z WHERE z MATCH 'al'`,
			`SELECT docid FROM z WHERE z MATCH 'alp'`,
			`SELECT docid FROM z WHERE z MATCH 'be'`,
			`SELECT docid FROM z WHERE z MATCH 'beta'`,
			`SELECT quote(matchinfo(z)) FROM z WHERE z MATCH 'alpha'`,
			`SELECT offsets(z) FROM z WHERE z MATCH 'alpha'`,
		}},
		// A level MERGE has to fire per index too: the sixteenth flush merges
		// each index's level-0 segments into its own level-1 slot.
		{"the level merge is per index", []string{
			`CREATE VIRTUAL TABLE m USING fts4(x, prefix=2)`,
			`INSERT INTO m VALUES('aa common')`,
			`INSERT INTO m VALUES('bb common')`,
			`INSERT INTO m VALUES('cc common')`,
			`INSERT INTO m VALUES('dd common')`,
			`INSERT INTO m VALUES('ee common')`,
			`INSERT INTO m VALUES('ff common')`,
			`INSERT INTO m VALUES('gg common')`,
			`INSERT INTO m VALUES('hh common')`,
			`INSERT INTO m VALUES('ii common')`,
			`INSERT INTO m VALUES('jj common')`,
			`INSERT INTO m VALUES('kk common')`,
			`INSERT INTO m VALUES('ll common')`,
			`INSERT INTO m VALUES('mm common')`,
			`INSERT INTO m VALUES('nn common')`,
			`INSERT INTO m VALUES('oo common')`,
			`SELECT level, idx FROM m_segdir ORDER BY level, idx`,
			`INSERT INTO m VALUES('pp common')`,
			`SELECT level, idx, quote(root) FROM m_segdir ORDER BY level, idx`,
			`INSERT INTO m VALUES('qq common')`,
			`SELECT level, idx FROM m_segdir ORDER BY level, idx`,
			`SELECT docid FROM m WHERE m MATCH 'common'`,
			`SELECT docid FROM m WHERE m MATCH 'pp'`,
			`INSERT INTO m(m) VALUES('integrity-check')`,
		}},
		// A prefix index accumulates inside a transaction in lockstep with the
		// term index, and both are sealed at the same points.
		{"a transaction accumulates every index together", []string{
			`CREATE VIRTUAL TABLE w USING fts4(x, prefix=2)`,
			`INSERT INTO w VALUES('aa')`,
			`INSERT INTO w VALUES('bb')`,
			`BEGIN`,
			`INSERT INTO w VALUES('cc')`,
			`INSERT INTO w VALUES('dd')`,
			`COMMIT`,
			`SELECT level, idx, quote(root) FROM w_segdir ORDER BY level, idx`,
			`INSERT INTO w(w) VALUES('integrity-check')`,
			`SELECT docid FROM w WHERE w MATCH 'dd'`,
		}},
		// A prefix index SPILLS through the same encoder, and the %_segments
		// block ids must climb in INDEX order -- index 0's blocks first, then
		// index 1's -- which is fts3PendingTermsFlush's own loop.
		{"two indexes spilling in one statement", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, prefix=6)`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("w", 700) + `')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, length(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, length(block) FROM t_segments ORDER BY blockid`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("q", 700) + `')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, length(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, length(block) FROM t_segments ORDER BY blockid`,
			`SELECT docid FROM t WHERE t MATCH 'w000050'`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, length(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, length(block) FROM t_segments ORDER BY blockid`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}},
		// fts3 has no module options, so the same argument DECLARES a column.
		{"fts3 declares a column instead", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a, prefix="1,2")`,
			`SELECT sql FROM sqlite_master WHERE name='t_content'`,
			`INSERT INTO t VALUES('one','two')`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid FROM t WHERE t MATCH 'two'`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}
