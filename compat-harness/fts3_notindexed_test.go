// This file gates the fts4 "notindexed=<col>" option, verifying that the
// index, docsize totals, and stats are correct for both indexed and non-indexed columns.
package compat

import "testing"

func TestFts3NotIndexedDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"one notindexed column of two", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b, notindexed=b)`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
			`INSERT INTO t(a,b) VALUES('one two','three four')`,
			`INSERT INTO t(a,b) VALUES('five','six')`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			// Notindexed column not found by match.
			`SELECT docid FROM t WHERE t MATCH 'three'`,
			`SELECT docid FROM t WHERE t MATCH 'b:one'`,
			`SELECT docid FROM t WHERE t MATCH 'one'`,
			`SELECT docid FROM t WHERE t MATCH 'a:five'`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`SELECT docid, quote(c0a), quote(c1b) FROM t_content ORDER BY docid`,
			`SELECT quote(matchinfo(t)) FROM t WHERE t MATCH 'five'`,
			`SELECT offsets(t) FROM t WHERE t MATCH 'five'`,
			`SELECT snippet(t) FROM t WHERE t MATCH 'one'`,
		}},
		// DELETE and UPDATE handle notindexed columns correctly.
		{"DELETE and UPDATE leave the notindexed column out", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b, notindexed=b)`,
			`INSERT INTO t(a,b) VALUES('one two','three four')`,
			`INSERT INTO t(a,b) VALUES('five','six')`,
			`INSERT INTO t(a,b) VALUES('seven','eight')`,
			`DELETE FROM t WHERE docid=1`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`UPDATE t SET b='nine ten' WHERE docid=2`,
			`UPDATE t SET a='eleven' WHERE docid=3`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'nine'`,
			`SELECT docid FROM t WHERE t MATCH 'eleven'`,
		}},
		// Multiple notindexed columns.
		{"two notindexed columns, including the first", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b, c, notindexed=a, notindexed=c)`,
			`SELECT sql FROM sqlite_master WHERE name='t_content'`,
			`INSERT INTO t VALUES('p','q','r')`,
			`INSERT INTO t VALUES('xx yy','zz','ww')`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`SELECT docid FROM t WHERE t MATCH 'zz'`,
			`SELECT docid FROM t WHERE t MATCH 'xx'`,
		}},
		// Forward reference, case-insensitive, quoted names.
		{"forward reference, case, quoting", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(notindexed=b, a, b)`,
			`CREATE VIRTUAL TABLE t2 USING fts4(a, b, notindexed=B)`,
			`CREATE VIRTUAL TABLE t3 USING fts4(a, b, notindexed="b")`,
			`CREATE VIRTUAL TABLE t4 USING fts4(abc, ab, a, notindexed=abc)`,
			`INSERT INTO t1 VALUES('one','two')`,
			`INSERT INTO t2 VALUES('one','two')`,
			`INSERT INTO t3 VALUES('one','two')`,
			`INSERT INTO t4 VALUES('one','two','three')`,
			`SELECT quote(root) FROM t1_segdir`,
			`SELECT quote(root) FROM t2_segdir`,
			`SELECT quote(root) FROM t3_segdir`,
			`SELECT quote(root) FROM t4_segdir`,
			`SELECT quote(value) FROM t4_stat`,
			`SELECT quote(size) FROM t4_docsize`,
		}},
		// Invalid notindexed spellings must fail.
		{"refused notindexed spellings", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a, notindexed=nosuch)`,
			`CREATE VIRTUAL TABLE t2 USING fts4(a, b, notindexed = b)`,
			`CREATE VIRTUAL TABLE t3 USING fts4(a, b, notindexed=)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		// fts3 has no module options; notindexed declares a column name.
		{"fts3 declares a column instead", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a, b, notindexed=b)`,
			`SELECT sql FROM sqlite_master WHERE name='t_content'`,
			`INSERT INTO t VALUES('one','two','three')`,
			`SELECT quote(root) FROM t_segdir`,
			`SELECT docid FROM t WHERE t MATCH 'two'`,
		}},
		// rebuild and integrity-check respect notindexed.
		{"rebuild and integrity-check honour notindexed", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b, notindexed=b)`,
			`INSERT INTO t(a,b) VALUES('one two','three four')`,
			`INSERT INTO t(a,b) VALUES('five','six')`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`INSERT INTO t(t) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`SELECT docid FROM t WHERE t MATCH 'five'`,
			`SELECT docid FROM t WHERE t MATCH 'six'`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}
