// Tests the fts4 "matchinfo=fts3" option, which omits the %_docsize shadow table
// while keeping the full %_stat table and affecting certain matchinfo() directives.
package compat

import "testing"

func TestFts3MatchinfoOptionDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"no %_docsize table is created", []string{
			`CREATE VIRTUAL TABLE mi USING fts4(a, b, matchinfo=fts3)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`SELECT count(*) FROM sqlite_master WHERE name='mi_docsize'`,
			`SELECT * FROM mi_docsize`,
		}},
		{"the index and %_stat are an ordinary fts4 table's", []string{
			`CREATE VIRTUAL TABLE mi USING fts4(a, b, matchinfo=fts3)`,
			`CREATE VIRTUAL TABLE ord USING fts4(a, b)`,
			`INSERT INTO mi VALUES('one two','three four')`,
			`INSERT INTO ord VALUES('one two','three four')`,
			`INSERT INTO mi VALUES('two three','four five')`,
			`INSERT INTO ord VALUES('two three','four five')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM mi_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM mi_stat ORDER BY id`,
			`SELECT id, quote(value) FROM ord_stat ORDER BY id`,
			`SELECT docid, a, b FROM mi ORDER BY docid`,
			`SELECT docid FROM mi WHERE mi MATCH 'two'`,
			`SELECT docid FROM mi WHERE mi MATCH 'b:four'`,
		}},
		{"matchinfo directives", []string{
			`CREATE VIRTUAL TABLE mi USING fts4(a, b, matchinfo=fts3)`,
			`INSERT INTO mi VALUES('one two','three four')`,
			`INSERT INTO mi VALUES('two three','four five')`,
			`SELECT quote(matchinfo(mi)) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'pcx')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'n')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'a')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'na')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'y')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'b')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'l')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'pcxnal')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT quote(matchinfo(mi,'nl')) FROM mi WHERE mi MATCH 'two'`,
			`SELECT offsets(mi) FROM mi WHERE mi MATCH 'two'`,
			`SELECT snippet(mi) FROM mi WHERE mi MATCH 'two'`,
		}},
		// DELETE and UPDATE keep %_stat in step with no %_docsize to drive it,
		// and emptying the table still throws every shadow table away.
		{"DELETE, UPDATE and the empty-table wipe", []string{
			`CREATE VIRTUAL TABLE mi USING fts4(a, b, matchinfo=fts3)`,
			`INSERT INTO mi VALUES('one two','three four')`,
			`INSERT INTO mi VALUES('two three','four five')`,
			`INSERT INTO mi VALUES('six','seven')`,
			`DELETE FROM mi WHERE docid=1`,
			`SELECT level, idx, quote(root) FROM mi_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM mi_stat ORDER BY id`,
			`UPDATE mi SET a='eight nine' WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM mi_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM mi_stat ORDER BY id`,
			`SELECT docid, a, b FROM mi ORDER BY docid`,
			`SELECT docid FROM mi WHERE mi MATCH 'eight'`,
			`DELETE FROM mi`,
			`SELECT count(*) FROM mi_segdir`,
			`SELECT id, quote(value) FROM mi_stat ORDER BY id`,
		}},
		// The command channel: optimize/rebuild/integrity-check must not look
		// for the table that is not there.
		{"the command channel", []string{
			`CREATE VIRTUAL TABLE mi USING fts4(a, b, matchinfo=fts3)`,
			`INSERT INTO mi VALUES('one two','three four')`,
			`INSERT INTO mi VALUES('two three','four five')`,
			`INSERT INTO mi(mi) VALUES('integrity-check')`,
			`INSERT INTO mi(mi) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM mi_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM mi_stat ORDER BY id`,
			`INSERT INTO mi(mi) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM mi_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM mi_stat ORDER BY id`,
			`INSERT INTO mi(mi) VALUES('integrity-check')`,
			`SELECT docid FROM mi WHERE mi MATCH 'five'`,
		}},
		// The VALUE is matched case-insensitively and "fts3" is the only one.
		// "matchinfo" with a space before the '=' is a key of its own and so is
		// "unrecognized parameter", exactly as it is for notindexed.
		{"accepted and refused spellings", []string{
			`CREATE VIRTUAL TABLE m1 USING fts4(a, b, MatchInfo=FTS3)`,
			`CREATE VIRTUAL TABLE m2 USING fts4(a, b, matchinfo=fts4)`,
			`CREATE VIRTUAL TABLE m3 USING fts4(a, b, matchinfo=fts5)`,
			`CREATE VIRTUAL TABLE m4 USING fts4(a, b, matchinfo=fs3)`,
			`CREATE VIRTUAL TABLE m5 USING fts4(a, b, matchinfo=)`,
			`CREATE VIRTUAL TABLE m6 USING fts4(a, b, matchinfo = fts3)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`INSERT INTO m1 VALUES('one two','three four')`,
			`SELECT id, quote(value) FROM m1_stat ORDER BY id`,
			`SELECT quote(matchinfo(m1,'l')) FROM m1 WHERE m1 MATCH 'two'`,
		}},
		// fts3 has no module options at all, so the same argument DECLARES a
		// column there -- and that table has neither %_docsize nor %_stat.
		{"fts3 declares a column instead", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a, b, matchinfo=fts3)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`INSERT INTO t VALUES('one','two','three')`,
			`SELECT quote(root) FROM t_segdir`,
			`SELECT docid FROM t WHERE t MATCH 'three'`,
			`SELECT quote(matchinfo(t,'n')) FROM t WHERE t MATCH 'three'`,
			`SELECT quote(matchinfo(t,'l')) FROM t WHERE t MATCH 'three'`,
		}},
		// The DROP still names %_docsize (fts3DestroyMethod is five
		// unconditional "DROP TABLE IF EXISTS" -- see fts3_shadow_test.go), so
		// it takes a USER table of that name with it even though this table
		// never had one.
		{"DROP still names the %_docsize this table lacks", []string{
			`CREATE VIRTUAL TABLE mi USING fts4(a, b, matchinfo=fts3)`,
			`CREATE TABLE mi_docsize(x)`,
			`INSERT INTO mi_docsize VALUES(42)`,
			`INSERT INTO mi VALUES('one two','three four')`,
			`DROP TABLE mi`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`SELECT x FROM mi_docsize`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}
