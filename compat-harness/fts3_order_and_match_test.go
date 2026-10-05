// FTS3/FTS4 rules: the "order=desc" option changes segment encoding, and MATCH
// with non-literal query strings varies per row.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestFts3OrderOptionDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// Doclist encoding in descending order.
		{"the doclist descends", []string{
			`CREATE VIRTUAL TABLE a1 USING fts4(x)`,
			`CREATE VIRTUAL TABLE d1 USING fts4(x, order=desc)`,
			`INSERT INTO a1(docid,x) VALUES(1,'aa'),(2,'aa'),(3,'aa')`,
			`INSERT INTO d1(docid,x) VALUES(1,'aa'),(2,'aa'),(3,'aa')`,
			`SELECT level, idx, quote(root) FROM a1_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM d1_segdir ORDER BY level, idx`,
			`SELECT docid FROM a1 WHERE a1 MATCH 'aa'`,
			`SELECT docid FROM d1 WHERE d1 MATCH 'aa'`,
			`INSERT INTO d1(d1) VALUES('integrity-check')`,
		}},
		// Single-document segments are byte-identical in both orders; verify row order differs.
		{"one document per segment is byte-identical", []string{
			`CREATE VIRTUAL TABLE a2 USING fts4(x)`,
			`CREATE VIRTUAL TABLE d2 USING fts4(x, order=desc)`,
			`INSERT INTO a2 VALUES('aa bb')`,
			`INSERT INTO a2 VALUES('aa cc')`,
			`INSERT INTO d2 VALUES('aa bb')`,
			`INSERT INTO d2 VALUES('aa cc')`,
			`SELECT level, idx, quote(root) FROM a2_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM d2_segdir ORDER BY level, idx`,
			`SELECT docid FROM a2 WHERE a2 MATCH 'aa'`,
			`SELECT docid FROM d2 WHERE d2 MATCH 'aa'`,
			`SELECT docid FROM d2`,
			`SELECT rowid, x FROM d2`,
		}},
		// Plain scan order, with and without explicit ORDER BY.
		{"a plain scan descends, and ORDER BY still wins", []string{
			`CREATE VIRTUAL TABLE d3 USING fts4(x, order=DESC)`,
			`INSERT INTO d3(docid,x) VALUES(1,'one'),(2,'two'),(3,'three')`,
			`SELECT docid FROM d3`,
			`SELECT rowid FROM d3`,
			`SELECT x FROM d3`,
			`SELECT docid FROM d3 ORDER BY docid`,
			`SELECT docid FROM d3 ORDER BY docid DESC`,
			`SELECT docid FROM d3 ORDER BY x`,
			`SELECT docid FROM d3 LIMIT 2`,
			`CREATE TABLE o3(k)`,
			`INSERT INTO o3 VALUES(1),(2),(3)`,
			// Checked with correlated subquery to maintain DESC scan order.
			`SELECT docid, (SELECT k FROM o3 WHERE k=d3.docid) FROM d3`,
			`SELECT docid FROM d3 WHERE docid>1`,
			`SELECT count(*) FROM d3`,
			`SELECT group_concat(docid) FROM d3`,
		}},
		// Spilled segments with multi-byte varints.
		{"a spilled segment and multi-byte deltas", []string{
			`CREATE VIRTUAL TABLE d4 USING fts4(x, order=desc)`,
			`INSERT INTO d4(docid,x) VALUES(3,'bb'),(200,'bb'),(70000,'bb')`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM d4_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM d4_segments ORDER BY blockid`,
			`SELECT docid FROM d4 WHERE d4 MATCH 'bb'`,
			`SELECT docid FROM d4`,
			`INSERT INTO d4(d4) VALUES('integrity-check')`,
		}},
		// Negative and zero docids.
		{"zero and negative docids", []string{
			`CREATE VIRTUAL TABLE d5 USING fts4(x, order=desc)`,
			`INSERT INTO d5(docid,x) VALUES(-9,'cc'),(0,'cc'),(4,'cc')`,
			`SELECT level, idx, quote(root) FROM d5_segdir ORDER BY level, idx`,
			`SELECT docid FROM d5 WHERE d5 MATCH 'cc'`,
			`SELECT docid FROM d5`,
			`INSERT INTO d5(d5) VALUES('integrity-check')`,
			`CREATE VIRTUAL TABLE d6 USING fts4(x, order=DESC)`,
			`INSERT INTO d6(docid,x) VALUES(-113382409004785664,'aa')`,
			`INSERT INTO d6(docid,x) VALUES(1,'ab')`,
			`SELECT rowid FROM d6 WHERE x MATCH 'a*' ORDER BY docid DESC`,
			`SELECT level, idx, quote(root) FROM d6_segdir ORDER BY level, idx`,
		}},
		// Optimize and rebuild with encoder/decoder round-tripping.
		{"optimize, rebuild and a delete marker", []string{
			`CREATE VIRTUAL TABLE d7 USING fts4(x, order=desc)`,
			`INSERT INTO d7 VALUES('aa bb')`,
			`INSERT INTO d7 VALUES('bb cc')`,
			`INSERT INTO d7 VALUES('cc aa')`,
			`SELECT level, idx, quote(root) FROM d7_segdir ORDER BY level, idx`,
			`INSERT INTO d7(d7) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM d7_segdir ORDER BY level, idx`,
			`SELECT docid FROM d7 WHERE d7 MATCH 'aa'`,
			`SELECT docid FROM d7 WHERE d7 MATCH 'bb'`,
			`INSERT INTO d7(d7) VALUES('integrity-check')`,
			`DELETE FROM d7 WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM d7_segdir ORDER BY level, idx`,
			`SELECT docid FROM d7 WHERE d7 MATCH 'bb'`,
			`INSERT INTO d7(d7) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM d7_segdir ORDER BY level, idx`,
			`INSERT INTO d7(d7) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM d7_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM d7_stat ORDER BY id`,
			`INSERT INTO d7(d7) VALUES('integrity-check')`,
			// Single-row case: delete and new terms share one segment.
			`CREATE VIRTUAL TABLE d8 USING fts4(a, order=DESC)`,
			`INSERT INTO d8(a) VALUES (0)`,
			`INSERT INTO d8(a) VALUES (0)`,
			`UPDATE d8 SET a = NULL WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM d8_segdir ORDER BY level, idx`,
			`SELECT docid, a FROM d8`,
			`INSERT INTO d8(d8) VALUES('integrity-check')`,
		}},
		// Transaction accumulation with descending re-encoding.
		{"a transaction's accumulating segment", []string{
			`CREATE VIRTUAL TABLE d9 USING fts4(x, order=desc)`,
			`BEGIN`,
			`INSERT INTO d9(docid,x) VALUES(1,'aa')`,
			`INSERT INTO d9(docid,x) VALUES(2,'aa bb')`,
			`SELECT docid FROM d9 WHERE d9 MATCH 'aa'`,
			`INSERT INTO d9(docid,x) VALUES(3,'bb')`,
			`COMMIT`,
			`SELECT level, idx, quote(root) FROM d9_segdir ORDER BY level, idx`,
			`SELECT docid FROM d9 WHERE d9 MATCH 'aa'`,
			`SELECT docid FROM d9 WHERE d9 MATCH 'bb'`,
			`SELECT docid FROM d9`,
			`INSERT INTO d9(d9) VALUES('integrity-check')`,
			`CREATE VIRTUAL TABLE t0 USING fts4(order=desc)`,
			`BEGIN`,
			`INSERT INTO t0(rowid, content) VALUES(5, 'abc')`,
			`INSERT INTO t0(rowid, content) VALUES(6, 'abc')`,
			`SELECT docid FROM t0 WHERE t0 MATCH 'abc'`,
			`SELECT docid FROM t0 WHERE t0 MATCH '"abc abc"'`,
			`COMMIT`,
			`SELECT docid FROM t0 WHERE t0 MATCH 'abc'`,
			`SELECT level, idx, quote(root) FROM t0_segdir ORDER BY level, idx`,
			`INSERT INTO t0(t0) VALUES('integrity-check')`,
		}},
		// Prefix indexes also descend.
		{"prefix indexes descend too", []string{
			`CREATE VIRTUAL TABLE t6 USING fts4(x, order=DESC, prefix=1)`,
			`INSERT INTO t6(docid,x) VALUES(1,'alpha'),(2,'alto'),(3,'beta')`,
			`SELECT level, idx, quote(root) FROM t6_segdir ORDER BY level, idx`,
			`SELECT docid FROM t6 WHERE t6 MATCH 'a*'`,
			`SELECT docid FROM t6 WHERE t6 MATCH 'al*'`,
			`SELECT docid FROM t6 WHERE t6 MATCH 'b*'`,
			`INSERT INTO t6(t6) VALUES('integrity-check')`,
			`INSERT INTO t6(t6) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM t6_segdir ORDER BY level, idx`,
			`SELECT docid FROM t6 WHERE t6 MATCH 'a*'`,
			`CREATE VIRTUAL TABLE ft USING fts4(c0, c1, order=DESC, prefix=1)`,
			`INSERT INTO ft(c0,c1) VALUES('one','two'),('three','four')`,
			`SELECT level, idx, quote(root) FROM ft_segdir ORDER BY level, idx`,
			`SELECT docid FROM ft WHERE ft MATCH 'o*'`,
			`SELECT rowid, c0, c1 FROM ft`,
		}},
		// Auxiliary functions reading descending doclists.
		{"matchinfo, offsets, snippet and fts4aux", []string{
			`CREATE VIRTUAL TABLE da USING fts4(x, y, order=desc)`,
			`INSERT INTO da(docid,x,y) VALUES(1,'aa bb','aa'),(2,'bb cc','aa bb'),(3,'aa','cc')`,
			`SELECT docid, quote(matchinfo(da)) FROM da WHERE da MATCH 'aa'`,
			`SELECT docid, offsets(da) FROM da WHERE da MATCH 'aa'`,
			`SELECT docid, snippet(da) FROM da WHERE da MATCH 'bb'`,
			`CREATE VIRTUAL TABLE daux USING fts4aux(da)`,
			`SELECT term, col, documents, occurrences FROM daux`,
			`SELECT docid, quote(size) FROM da_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM da_stat ORDER BY id`,
			`INSERT INTO da(da) VALUES('integrity-check')`,
		}},
		// Option grammar: case-insensitive, dequoted, last-one-wins.
		{"the option's grammar and last-one-wins", []string{
			`CREATE VIRTUAL TABLE g1 USING fts4(x, order=desc, order=asc)`,
			`CREATE VIRTUAL TABLE g2 USING fts4(x, order=asc, order=desc)`,
			`CREATE VIRTUAL TABLE g3 USING fts4(x, order=[desc])`,
			`CREATE VIRTUAL TABLE g4 USING fts4(x, order='desc')`,
			`CREATE VIRTUAL TABLE g5 USING fts4(x, order= desc)`,
			`CREATE VIRTUAL TABLE g6 USING fts4(x, order=descx)`,
			`CREATE VIRTUAL TABLE g7 USING fts4(x, order=DeSc)`,
			`CREATE VIRTUAL TABLE g8 USING fts3(x, order=desc)`,
			`SELECT type, name FROM sqlite_master WHERE name LIKE 'g_' ORDER BY name`,
			`INSERT INTO g1(docid,x) VALUES(1,'aa'),(2,'aa')`,
			`SELECT docid FROM g1`,
			`SELECT quote(root) FROM g1_segdir`,
			`INSERT INTO g2(docid,x) VALUES(1,'aa'),(2,'aa')`,
			`SELECT docid FROM g2`,
			`SELECT quote(root) FROM g2_segdir`,
			`INSERT INTO g3(docid,x) VALUES(1,'aa'),(2,'aa')`,
			`SELECT docid FROM g3`,
			`SELECT quote(root) FROM g3_segdir`,
			`INSERT INTO g7(docid,x) VALUES(1,'aa'),(2,'aa')`,
			`SELECT docid FROM g7`,
			// fts3 has no module options, so "order" is a column name.
			`INSERT INTO g8(docid,x) VALUES(1,'aa'),(2,'aa')`,
			`SELECT docid FROM g8`,
			`SELECT quote(root) FROM g8_segdir`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// DESC fts4 table with spilled segments and prefix index.
var ftsDescInterchangeWrite = []string{
	`CREATE VIRTUAL TABLE t USING fts4(a, b, order=desc, prefix=2)`,
	`INSERT INTO t(docid,a,b) VALUES(1,'hello world','one'),(2,'hello there','two'),(3,'cruel world','three')`,
	`INSERT INTO t(docid,a,b) VALUES(200,'bb cc','four'),(70000,'bb dd','five')`,
}

var ftsDescInterchangeRead = []string{
	`SELECT docid, a, b FROM t`,
	`SELECT docid FROM t WHERE t MATCH 'hello'`,
	`SELECT docid FROM t WHERE t MATCH 'world'`,
	`SELECT docid FROM t WHERE t MATCH 'bb'`,
	`SELECT docid FROM t WHERE t MATCH 'he*'`,
	`SELECT docid FROM t WHERE t MATCH 'hello' ORDER BY docid`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts3OrderDescFileInterchange tests file format interchange: each engine writes
// and reads with both, verifying format compatibility.
func TestFts3OrderDescFileInterchange(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "orderdesc.db")
			for i, r := range runWithDSN(t, writer, dsn, ftsDescInterchangeWrite) {
				if k, _ := r["kind"].(string); k == "error" {
					t.Fatalf("%s failed to write the DESC database at stmt %d (%s): %v", writer, i, ftsDescInterchangeWrite[i], r)
				}
			}
			var baseline string
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, ftsDescInterchangeRead))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s writes] readers disagree\n  %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}

// TestFts3OrderDescIsSearchableByCSQLite verifies that C SQLite can search
// descending segments written by this engine, with integrity checks passing.
func TestFts3OrderDescIsSearchableByCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "orderdesc.db")
	runWithDSN(t, "musql", dsn, ftsDescInterchangeWrite)

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
		t.Fatalf("C SQLite reports integrity_check = %q on a database this engine wrote", ic)
	}
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Fatalf("fts3's own integrity-check on a database this engine wrote: %v", err)
	}

	cases := []struct {
		query string
		want  string
	}{
		{`SELECT group_concat(docid) FROM t WHERE t MATCH 'hello'`, "2,1"},
		{`SELECT group_concat(docid) FROM t WHERE t MATCH 'world'`, "3,1"},
		{`SELECT group_concat(docid) FROM t WHERE t MATCH 'bb'`, "70000,200"},
		{`SELECT group_concat(docid) FROM t WHERE t MATCH 'he*'`, "2,1"},
		{`SELECT group_concat(docid) FROM t`, "70000,200,3,2,1"},
	}
	for _, c := range cases {
		var got sql.NullString
		if err := sdb.QueryRow(c.query).Scan(&got); err != nil {
			t.Errorf("cgo %s: %v", c.query, err)
			continue
		}
		if got.String != c.want {
			t.Errorf("cgo reading this engine's DESC database: %s = %q, want %q", c.query, got.String, c.want)
		}
	}
}

// TestFts3OrderDescMultiRowUpdateSplits gates multi-row UPDATE against order=desc tables.
// WHERE-less updates scan newest-first and flush between rows. Updates with WHERE clauses
// use cost-based plans that cannot be replicated and remain declined.
func TestFts3OrderDescMultiRowUpdateSplits(t *testing.T) {
	dump := func(tbl, col string) []string {
		return []string{
			`SELECT docid, ` + col + ` FROM ` + tbl + ` ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM ` + tbl + `_segdir ORDER BY level, idx`,
			`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('integrity-check')`,
		}
	}
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fts4merge.test 8.0 verbatim", append([]string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a, order=DESC)`,
			`INSERT INTO t1(a) VALUES (0)`,
			`INSERT INTO t1(a) VALUES (0)`,
			`UPDATE t1 SET a = NULL`,
		}, dump("t1", "a")...)},
		{"four rows, non-null before and after", append([]string{
			`CREATE VIRTUAL TABLE d USING fts4(x, order=desc)`,
			`INSERT INTO d(docid,x) VALUES(1,'p'),(2,'p'),(3,'p'),(4,'p')`,
			`UPDATE d SET x='q'`,
		}, dump("d", "x")...)},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}

	// Verify WHERE clause affects visit order, and decline WHERE-qualified updates.
	setup := []string{
		`CREATE VIRTUAL TABLE d USING fts4(x, order=desc)`,
		`INSERT INTO d(docid,x) VALUES(1,'p'),(2,'p'),(3,'p'),(4,'p')`,
	}
	segdir := `SELECT level, idx, quote(root) FROM d_segdir ORDER BY level, idx`
	cgoRows := func(stmts []string) []any {
		dsn := filepath.Join(t.TempDir(), "d.db")
		out := runWithDSN(t, "cgo", dsn, append(append([]string{}, setup...), stmts...))
		last, _ := out[len(out)-1]["rows"].([]any)
		return last
	}
	if got, want := len(cgoRows([]string{`UPDATE d SET x='q' WHERE docid IN (1,3)`, segdir})), 2; got != want {
		t.Errorf("C SQLite left %d %%_segdir rows after an IN-list UPDATE of a DESC table, want %d", got, want)
	}
	for _, where := range []string{`docid<3`, `docid IN (1,2)`} {
		if got, want := len(cgoRows([]string{`DELETE FROM d WHERE ` + where, segdir})), 2; got != want {
			t.Errorf("C SQLite left %d %%_segdir rows after DELETE ... WHERE %s on a DESC table, want %d", got, where, want)
		}
	}

	dsn := filepath.Join(t.TempDir(), "m.db")
	out := runWithDSN(t, "musql", dsn, append(append([]string{}, setup...), []string{
		`UPDATE d SET x='q' WHERE docid IN (1,3)`,
		`UPDATE d SET x='q' WHERE docid=2`,
		`DELETE FROM d WHERE docid<3`,
		segdir,
	}...))
	if k, _ := out[2]["kind"].(string); k != "error" {
		t.Errorf("this engine ACCEPTED a WHERE-qualified multi-row UPDATE of an order=desc table (%v); it must decline -- this write path cannot derive its real visit order", out[2])
	}
	for i := 3; i <= 4; i++ {
		if k, _ := out[i]["kind"].(string); k == "error" {
			t.Errorf("stmt %d must still be accepted on an order=desc table: %v", i, out[i])
		}
	}
}

// TestFts3NonLiteralMatchDiff gates MATCH with non-literal query strings.
// The query is evaluated per xFilter call and can come from table columns or expressions.
func TestFts3NonLiteralMatchDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"the query is another table's column", []string{
			`CREATE VIRTUAL TABLE ft1 USING fts4(x)`,
			`INSERT INTO ft1 VALUES('aaa bbb')`,
			`INSERT INTO ft1 VALUES('aaa aaa')`,
			`INSERT INTO ft1 VALUES('bbb bbb')`,
			`CREATE TABLE t1(id, y)`,
			`INSERT INTO t1 VALUES(1, 'aaa')`,
			`INSERT INTO t1 VALUES(2, 'bbb')`,
			`SELECT docid FROM ft1, t1 WHERE ft1 MATCH y AND id=1 ORDER BY docid`,
			`SELECT docid FROM ft1, t1 WHERE ft1 MATCH y AND id=2 ORDER BY docid`,
			`SELECT docid, id FROM ft1, t1 WHERE ft1 MATCH y ORDER BY docid, id`,
			`SELECT docid FROM ft1, t1 WHERE ft1 MATCH y||'x' ORDER BY docid`,
			`SELECT docid FROM ft1, t1 WHERE x MATCH y AND id=2 ORDER BY docid`,
		}},
		{"both sides are fts tables", []string{
			`CREATE VIRTUAL TABLE ft2 USING fts4(x)`,
			`CREATE VIRTUAL TABLE ft3 USING fts4(y)`,
			`INSERT INTO ft2 VALUES('abc')`,
			`INSERT INTO ft2 VALUES('def')`,
			`INSERT INTO ft3 VALUES('ghi')`,
			`INSERT INTO ft3 VALUES('abc')`,
			`SELECT * FROM ft2, ft3 WHERE x MATCH y`,
			`SELECT * FROM ft2, ft3 WHERE y MATCH x`,
			`SELECT * FROM ft3, ft2 WHERE x MATCH y`,
			`SELECT * FROM ft3, ft2 WHERE y MATCH x`,
			`SELECT * FROM ft3, ft2 WHERE y MATCH x AND x MATCH y`,
		}},
		{"the value conversion, and NULL", []string{
			`CREATE VIRTUAL TABLE ft USING fts4(x)`,
			`INSERT INTO ft(docid,x) VALUES(1,'aaa bbb'),(2,'123'),(3,'bbb')`,
			`CREATE TABLE q(k, v)`,
			`INSERT INTO q VALUES(1,'aaa'),(2,NULL),(3,123),(4,x'616161')`,
			`SELECT docid FROM ft WHERE ft MATCH CAST(x'616161' AS TEXT) ORDER BY docid`,
			`SELECT docid FROM ft WHERE ft MATCH 'a'||'aa' ORDER BY docid`,
			`SELECT docid FROM ft WHERE ft MATCH (SELECT v FROM q WHERE k=1) ORDER BY docid`,
			`SELECT docid FROM ft WHERE ft MATCH (SELECT v FROM q WHERE k=2) ORDER BY docid`,
			`SELECT docid FROM ft WHERE ft MATCH (SELECT v FROM q WHERE k=3) ORDER BY docid`,
			`SELECT docid FROM ft WHERE ft MATCH (SELECT v FROM q WHERE k=4) ORDER BY docid`,
			`SELECT docid, k FROM ft, q WHERE ft MATCH v ORDER BY docid, k`,
			`SELECT docid FROM ft WHERE ft MATCH nullif('aaa','aaa') ORDER BY docid`,
		}},
		{"a malformed non-literal query is an error", []string{
			`CREATE VIRTUAL TABLE ft USING fts4(x)`,
			`INSERT INTO ft VALUES('aaa')`,
			`CREATE TABLE bad(y)`,
			`SELECT docid FROM ft, bad WHERE ft MATCH y`,
			`INSERT INTO bad VALUES('AND')`,
			`SELECT docid FROM ft, bad WHERE ft MATCH y`,
			`SELECT docid FROM ft WHERE ft MATCH (SELECT y FROM bad)`,
			`SELECT docid FROM ft WHERE ft MATCH 'aaa' ORDER BY docid`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestFts3NonLiteralMatchOnEmptyTable gates non-literal MATCH over empty fts table.
// C SQLite parses the query per outer row and errors on malformed queries;
// this engine must decline to avoid silently answering no rows.
func TestFts3NonLiteralMatchOnEmptyTable(t *testing.T) {
	stmts := []string{
		`CREATE VIRTUAL TABLE ft USING fts4(x)`,
		`CREATE TABLE outer1(y)`,
		`INSERT INTO outer1 VALUES('AND')`,
		`SELECT docid FROM ft, outer1 WHERE ft MATCH y`,
	}
	dsn := filepath.Join(t.TempDir(), "e.db")
	cgo := runWithDSN(t, "cgo", dsn, stmts)
	if k, _ := cgo[3]["kind"].(string); k != "error" {
		t.Fatalf("premise broken: C SQLite did NOT error on a malformed non-literal MATCH over an EMPTY fts table (%v) -- the decline below is then unnecessary", cgo[3])
	}
	mine := runWithDSN(t, "musql", filepath.Join(t.TempDir(), "m.db"), stmts)
	if k, _ := mine[3]["kind"].(string); k != "error" {
		t.Errorf("this engine ANSWERED a non-literal MATCH over an empty fts table (%v); it must decline -- C SQLite parses the query per outer row and errors", mine[3])
	}
	ok := runWithDSN(t, "musql", filepath.Join(t.TempDir(), "o.db"), []string{
		`CREATE VIRTUAL TABLE ft USING fts4(x)`,
		`INSERT INTO ft VALUES('aaa')`,
		`CREATE TABLE outer1(y)`,
		`INSERT INTO outer1 VALUES('aaa')`,
		`SELECT docid FROM ft, outer1 WHERE ft MATCH y`,
	})
	if k, _ := ok[4]["kind"].(string); k == "error" {
		t.Errorf("the non-empty case must still be served: %v", ok[4])
	}
}
