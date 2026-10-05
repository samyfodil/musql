// This file tests MATCH queries against fts3/fts4 tables in ATTACHed databases.
// FTS tables store their shadow tables in their own file, so resolution must use
// the correct database's pager. Tests use attached databases with same-named
// tables holding different data to verify correct pager selection.
package compat

import "testing"

func TestFts3MatchInAttachedDatabase(t *testing.T) {
	// Build auxiliary database with multiple fts tables.
	goAux, cgoAux := buildAuxPair(t, "ftsaux",
		`CREATE VIRTUAL TABLE t3 USING fts3(a)`,
		`INSERT INTO t3 VALUES('hello world')`,
		`INSERT INTO t3 VALUES('nothing here')`,
		`CREATE VIRTUAL TABLE t2 USING fts4(a, b)`,
		`INSERT INTO t2 VALUES('hello world', 'x')`,
		`INSERT INTO t2 VALUES('other', 'hello')`,
		`CREATE VIRTUAL TABLE t1 USING fts3(a, b, c)`,
		`INSERT INTO t1 VALUES('song', 'two', 'nine')`,
		`INSERT INTO t1 VALUES('other', 'song', 'ten')`,
		`CREATE VIRTUAL TABLE t4 USING fts4(a)`,
		`INSERT INTO t4 VALUES('one two three')`,
		`INSERT INTO t4 VALUES('two three four')`,
		`CREATE VIRTUAL TABLE t5 USING fts4(a)`,
		`INSERT INTO t5 VALUES('x')`,
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	// Main database gets same-named tables with different data.
	p.agreeExec(`CREATE VIRTUAL TABLE t3 USING fts3(a)`)
	p.agreeExec(`INSERT INTO t3 VALUES('goodbye world')`)
	p.agreeExec(`CREATE VIRTUAL TABLE t1 USING fts3(a, b, c)`)
	p.agreeExec(`INSERT INTO t1 VALUES('one', 'two', 'three')`)
	p.agreeExec(`ATTACH DATABASE '{0}' AS two`)

	// Schema-qualified and unqualified MATCH queries.
	p.agreeQuery(`SELECT rowid, a FROM two.t3 WHERE t3 MATCH 'hello'`)
	p.agreeQuery(`SELECT rowid, a FROM two.t3 WHERE two.t3 MATCH 'world' ORDER BY rowid`)
	p.agreeQuery(`SELECT rowid, a FROM two.t3 WHERE t3 MATCH 'goodbye'`)
	p.agreeQuery(`SELECT rowid, a FROM main.t3 WHERE t3 MATCH 'goodbye'`)
	p.agreeQuery(`SELECT rowid, a FROM main.t3 WHERE t3 MATCH 'hello'`)
	p.agreeQuery(`SELECT rowid, a FROM t3 WHERE t3 MATCH 'goodbye'`)

	// Unqualified queries resolve to the attached database.
	p.agreeQuery(`SELECT rowid FROM t2 WHERE t2 MATCH 'hello' ORDER BY rowid`)
	p.agreeQuery(`SELECT rowid FROM t2 WHERE b MATCH 'hello'`)
	p.agreeQuery(`SELECT rowid FROM t2 WHERE a MATCH 'hello'`)
	p.agreeQuery(`SELECT rowid FROM t2 WHERE t2 MATCH 'nosuchterm'`)

	// Column-specific MATCH queries.
	p.agreeQuery(`SELECT a, b, c FROM two.t1 WHERE a MATCH 'song'`)
	p.agreeQuery(`SELECT a, b, c FROM two.t1 WHERE b MATCH 'song' ORDER BY a`)
	p.agreeQuery(`SELECT a, b, c FROM two.t1 WHERE c MATCH 'two'`)
	p.agreeQuery(`SELECT a, b, c FROM main.t1 WHERE c MATCH 'three'`)
	p.agreeQuery(`SELECT a, b, c FROM main.t1 WHERE c MATCH 'two'`)

	// FTS auxiliary functions (offsets, snippet, matchinfo).
	p.agreeQuery(`SELECT offsets(t4) FROM two.t4 WHERE t4 MATCH 'two' ORDER BY docid`)
	p.agreeQuery(`SELECT snippet(t4) FROM two.t4 WHERE t4 MATCH 'three' ORDER BY docid`)
	p.agreeQuery(`SELECT quote(matchinfo(t4)) FROM two.t4 WHERE t4 MATCH 'two' ORDER BY docid`)
	p.agreeQuery(`SELECT quote(matchinfo(t4, 'pcnxals')) FROM two.t4 WHERE t4 MATCH 'two' ORDER BY docid`)

	// Joins spanning multiple databases.
	p.agreeExec(`CREATE TABLE plain(k)`)
	p.agreeExec(`INSERT INTO plain VALUES(1),(2)`)
	p.agreeQuery(`SELECT k, a FROM plain, two.t4 WHERE t4 MATCH 'four' ORDER BY k`)
	p.agreeQuery(`SELECT k, a FROM two.t4, plain WHERE t4 MATCH 'four' ORDER BY k`)

	// Malformed and valid MATCH queries.
	p.agreeQuery(`SELECT rowid FROM two.t5 WHERE t5 MATCH 'AND'`)
	p.agreeQuery(`SELECT rowid FROM two.t5 WHERE t5 MATCH 'x'`)

	// Queries fail after DETACH.
	p.agreeExec(`DETACH DATABASE two`)
	p.agreeQuery(`SELECT rowid, a FROM two.t3 WHERE t3 MATCH 'hello'`)
	p.agreeQuery(`SELECT rowid FROM t2 WHERE t2 MATCH 'hello'`)
}
