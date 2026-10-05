// This file tests FTS4's content= module option.
// Both contentless (content="") and external content (content=<table>)
// variants are tested against the oracle.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestFts3ContentOptionDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// The shadow set, the borrowed column list and the plain (non-MATCH)
		// read, which comes from the content table by rowid.
		{"external content: schema and read", []string{
			`CREATE TABLE t1(a, b, c)`,
			`INSERT INTO t1 VALUES('w x', 'x y', 'y z')`,
			`CREATE VIRTUAL TABLE ft1 USING fts4(content=t1)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`PRAGMA table_info(ft1)`,
			`SELECT *, rowid FROM ft1`,
			`SELECT a, c FROM ft1 WHERE rowid=1`,
			`INSERT INTO ft1(ft1) VALUES('rebuild')`,
			`SELECT rowid FROM ft1 WHERE ft1 MATCH 'x'`,
			`SELECT rowid FROM ft1 WHERE ft1 MATCH 'a'`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM ft1_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM ft1_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM ft1_stat ORDER BY id`,
			`INSERT INTO ft1(ft1) VALUES('integrity-check')`,
		}},
		// Declared columns are used verbatim and the content table's extra ones
		// are simply not read; the INTEGER PRIMARY KEY of a derived list IS an
		// fts column, and reads back as the docid.
		{"external content: declared vs derived columns", []string{
			`CREATE TABLE t1(a, b, c)`,
			`INSERT INTO t1 VALUES('w x', 'x y', 'y z')`,
			`CREATE VIRTUAL TABLE ft1 USING fts4(content=t1, b)`,
			`PRAGMA table_info(ft1)`,
			`SELECT *, rowid FROM ft1`,
			`CREATE TABLE p1(id INTEGER PRIMARY KEY, x TEXT, y)`,
			`INSERT INTO p1 VALUES(5,'hello','world')`,
			`CREATE VIRTUAL TABLE fp1 USING fts4(content=p1)`,
			`PRAGMA table_info(fp1)`,
			`SELECT rowid, * FROM fp1`,
			`INSERT INTO fp1(fp1) VALUES('rebuild')`,
			`SELECT rowid FROM fp1 WHERE fp1 MATCH 'hello'`,
			`SELECT rowid FROM fp1 WHERE fp1 MATCH '5'`,
			`SELECT id, quote(value) FROM fp1_stat`,
			// A declared column the content table does not have breaks every
			// read of the table ("SQL logic error" there).
			`CREATE TABLE w1(a)`,
			`CREATE VIRTUAL TABLE fw1 USING fts4(content=w1, b, c)`,
			`SELECT * FROM fw1`,
		}},
		// A content table that must be consulted at CREATE time and is not
		// there fails the CREATE, leaving nothing behind; one whose columns are
		// declared does not consult it at all.
		{"external content: missing content table", []string{
			`CREATE VIRTUAL TABLE ftz USING fts4(content=)`,
			`CREATE VIRTUAL TABLE ftz2 USING fts4(content=nosuch)`,
			`CREATE VIRTUAL TABLE ft8 USING fts4(content=nosuchtable, x)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`INSERT INTO ft8(docid, x) VALUES(13, 'U O N X G')`,
			`INSERT INTO ft8(docid, x) VALUES(15, 'N J Y G X')`,
			`SELECT id, quote(value) FROM ft8_stat`,
			`SELECT docid, quote(size) FROM ft8_docsize ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM ft8_segdir ORDER BY level, idx`,
			`SELECT * FROM ft8`,
		}},
		// A write never touches the content table: the docid must be supplied
		// and be an INTEGER, and the index/%_docsize/%_stat move alone.
		{"external content: writes leave the content table alone", []string{
			`CREATE TABLE t3(x, y)`,
			`CREATE VIRTUAL TABLE ft3 USING fts4(content=t3)`,
			`INSERT INTO ft3 VALUES('a b c','d e f')`,
			`INSERT INTO ft3(x,y) VALUES('a b c','d e f')`,
			`INSERT INTO ft3(rowid,x,y) VALUES(NULL,'a b c','d e f')`,
			`INSERT INTO ft3(docid,x,y) VALUES(21,'a b c','d e f')`,
			`SELECT level, idx, quote(root) FROM ft3_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM ft3_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM ft3_stat ORDER BY id`,
			`SELECT rowid FROM ft3 WHERE ft3 MATCH 'a b c'`,
			`SELECT rowid, * FROM ft3 WHERE ft3 MATCH 'a b c'`,
			`SELECT * FROM t3`,
			// The DELETE is driven by the content table, which holds nothing:
			// a complete no-op, not a wipe.
			`DELETE FROM ft3`,
			`SELECT rowid FROM ft3 WHERE ft3 MATCH 'a b c'`,
			`SELECT id, quote(value) FROM ft3_stat ORDER BY id`,
			`SELECT docid, quote(size) FROM ft3_docsize ORDER BY docid`,
			`SELECT count(*) FROM ft3_segdir`,
			// With the row present it deletes -- and, because fts3IsEmpty
			// answers "never empty" for a content= table, leaves a SECOND
			// segment of delete markers rather than an emptied index.
			`INSERT INTO t3(rowid,x,y) VALUES(21,'a b c','d e f')`,
			`DELETE FROM ft3`,
			`SELECT rowid FROM ft3 WHERE ft3 MATCH 'a b c'`,
			`SELECT level, idx, quote(root) FROM ft3_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM ft3_stat ORDER BY id`,
			`SELECT docid, quote(size) FROM ft3_docsize ORDER BY docid`,
			`SELECT rowid FROM t3`,
		}},
		// UPDATE, whose old values also come from the content table.
		{"external content: UPDATE", []string{
			`CREATE TABLE t3(x, y)`,
			`CREATE VIRTUAL TABLE ft3 USING fts4(content=t3)`,
			`INSERT INTO ft3(rowid, x, y) VALUES(0, 'R T M S M', 'A F O K H')`,
			`INSERT INTO ft3(rowid, x, y) VALUES(1, 'C Z J O X', 'U S Q D K')`,
			`INSERT INTO ft3(rowid, x, y) VALUES(2, 'N G H P O', 'N O P O C')`,
			`INSERT INTO t3(rowid, x, y) VALUES(0, 'R T M S M', 'A F O K H')`,
			`INSERT INTO t3(rowid, x, y) VALUES(1, 'C Z J O X', 'U S Q D K')`,
			`UPDATE ft3 SET x = y, y = x`,
			`SELECT level, idx, quote(root) FROM ft3_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM ft3_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM ft3_stat ORDER BY id`,
			`SELECT rowid, x, y FROM t3 ORDER BY rowid`,
			`SELECT rowid, x, y FROM ft3 ORDER BY rowid`,
			`SELECT rowid FROM ft3 WHERE ft3 MATCH 'y:O'`,
		}},
		// CONTENTLESS. The shadow set is the same minus %_content, the index
		// bytes are an ordinary table's, and every column read is refused.
		{"contentless", []string{
			`CREATE VIRTUAL TABLE ft9 USING fts4(content=, x)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`PRAGMA table_info(ft9)`,
			`INSERT INTO ft9(docid,x) VALUES(13,'U O N X G')`,
			`INSERT INTO ft9(docid,x) VALUES(14,'C J J U B')`,
			`INSERT INTO ft9(docid,x) VALUES(15,'N J Y G X')`,
			`SELECT level, idx, quote(root) FROM ft9_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM ft9_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM ft9_stat ORDER BY id`,
			`SELECT * FROM ft9`,
			`SELECT x FROM ft9`,
			`INSERT INTO ft9(ft9) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM ft9_segdir ORDER BY level, idx`,
			// The commands that need a content table, and the two statements
			// whose row source would have been one.
			`INSERT INTO ft9(ft9) VALUES('rebuild')`,
			`INSERT INTO ft9(ft9) VALUES('integrity-check')`,
			`DELETE FROM ft9`,
			`DELETE FROM ft9 WHERE docid=13`,
			`UPDATE ft9 SET x='q'`,
			`SELECT id, quote(value) FROM ft9_stat ORDER BY id`,
		}},
		// The docid rules, which have no %_content row to conflict with: not
		// an integer is "constraint failed", and the SAME docid twice simply
		// lands in the index twice.
		{"contentless: docid rules", []string{
			`CREATE VIRTUAL TABLE c1 USING fts4(content=, x)`,
			`INSERT INTO c1(docid,x) VALUES(5,'aa bb')`,
			`INSERT INTO c1(docid,x) VALUES(5,'cc dd')`,
			`INSERT INTO c1(rowid,x) VALUES(9,'ee')`,
			`INSERT INTO c1(docid,x) VALUES('notint','zz')`,
			`INSERT INTO c1(docid,x) VALUES(2.5,'zz')`,
			`INSERT INTO c1(x) VALUES('zz')`,
			`SELECT level, idx, quote(root) FROM c1_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM c1_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM c1_stat ORDER BY id`,
			// (A MATCH over a table with NO readable content table is the one
			// shape this engine declines -- see engine/fts3_content.go.)
			`CREATE TABLE src(x)`,
			`INSERT INTO src VALUES('m n'),('o p')`,
			`CREATE VIRTUAL TABLE c3 USING fts4(content=, x)`,
			`INSERT INTO c3(rowid,x) SELECT rowid,x FROM src`,
			`SELECT level, idx, quote(root) FROM c3_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM c3_stat ORDER BY id`,
		}},
		// DROP leaves a same-named user table alone, because that one drop is
		// COMMENTED OUT of fts3DestroyMethod for a content= table.
		{"DROP keeps <name>_content", []string{
			`CREATE TABLE t5(a, b, c, d)`,
			`CREATE VIRTUAL TABLE ft5 USING fts4(content=t5)`,
			`INSERT INTO t5 VALUES('a','b','c','d')`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`DROP TABLE ft5`,
			`SELECT * FROM t5`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`CREATE VIRTUAL TABLE ft5 USING fts4(content=t5)`,
			`CREATE TABLE t5_content(a, b)`,
			`DROP TABLE ft5`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		// "prefix=" and "content=" together: 'rebuild' has to fill every index.
		{"content= with prefix=", []string{
			`CREATE TABLE t10(a, b)`,
			`INSERT INTO t10 VALUES('abasia abasic abask', 'Abassin abastardize abatable')`,
			`INSERT INTO t10 VALUES('abate abatement abater', 'abatis abatised abaton')`,
			`INSERT INTO t10 VALUES('abator abattoir Abatua', 'abature abave abaxial')`,
			`CREATE VIRTUAL TABLE ft10 USING fts4(content=t10, prefix="2,4", a, b)`,
			`SELECT * FROM ft10 WHERE a MATCH 'ab*'`,
			`INSERT INTO ft10(ft10) VALUES('rebuild')`,
			`SELECT rowid FROM ft10 WHERE a MATCH 'ab*'`,
			`SELECT rowid FROM ft10 WHERE b MATCH 'abav*'`,
			`SELECT rowid FROM ft10 WHERE ft10 MATCH 'abas*'`,
			`SELECT level, idx, quote(root) FROM ft10_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM ft10_stat ORDER BY id`,
			`SELECT docid, quote(size) FROM ft10_docsize ORDER BY docid`,
			`INSERT INTO ft10(ft10) VALUES('integrity-check')`,
		}},
		// The content table's schema may change UNDER the fts table, and both
		// engines follow it -- an ALTER reconnects the vtab there. (A DROP does
		// NOT, which is why this engine declines that one; see
		// engine/fts3_content.go's fts3ContentDependent.)
		{"ALTER of the content table", []string{
			`CREATE TABLE t7(one, two, three)`,
			`CREATE VIRTUAL TABLE ft7 USING fts4(content=t7)`,
			`INSERT INTO t7 VALUES('A B','B A','C C')`,
			`SELECT * FROM ft7`,
			`ALTER TABLE t7 DROP COLUMN three`,
			`SELECT * FROM ft7`,
			`PRAGMA table_info(ft7)`,
			`ALTER TABLE t7 RENAME COLUMN one TO uno`,
			`SELECT * FROM ft7`,
			`PRAGMA table_info(ft7)`,
			`INSERT INTO ft7(ft7) VALUES('rebuild')`,
			`SELECT rowid FROM ft7 WHERE ft7 MATCH 'A'`,
			`SELECT level, idx, quote(root) FROM ft7_segdir ORDER BY level, idx`,
		}},
		// An fts table cannot be its own content table, nor two each other's:
		// both CREATE, both INSERT, and every read of them fails.
		{"circular references", []string{
			`CREATE VIRTUAL TABLE x1 USING fts4(content=x1)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`CREATE VIRTUAL TABLE t1 USING fts4(a, content=t1 )`,
			`INSERT INTO t1(rowid, a) VALUES(1, 'abc')`,
			`SELECT * FROM t1`,
			`SELECT count(*) FROM t1`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		// fts3 has no module options, so "content=t1" DECLARES a column there
		// -- and the table keeps its own %_content like any other fts3 table.
		{"fts3 declares a column instead", []string{
			`CREATE TABLE t1(a)`,
			`CREATE VIRTUAL TABLE f3 USING fts3(a, content=t1)`,
			`SELECT sql FROM sqlite_master WHERE name='f3_content'`,
			`PRAGMA table_info(f3)`,
			`INSERT INTO f3 VALUES('one','two')`,
			`SELECT docid, a, content FROM f3`,
			`SELECT quote(root) FROM f3_segdir`,
			`SELECT docid FROM f3 WHERE f3 MATCH 'two'`,
		}},
		// A WITHOUT ROWID content table has no rowid for fts3's first output
		// column, so the CREATE succeeds and every READ of the fts table fails.
		{"WITHOUT ROWID content table", []string{
			`CREATE TABLE wr(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
			`INSERT INTO wr VALUES('k1','hello world')`,
			`CREATE VIRTUAL TABLE fw USING fts4(content=wr)`,
			`PRAGMA table_info(fw)`,
			`SELECT * FROM fw`,
			`SELECT rowid, * FROM fw`,
			`INSERT INTO fw(fw) VALUES('rebuild')`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		// The option value is DEQUOTED and NOT TRIMMED, exactly like every
		// other fts4 option (vtab_fts3.go's parseSchema).
		{"content= value quoting", []string{
			`CREATE TABLE t1(a, b)`,
			`INSERT INTO t1 VALUES('one','two')`,
			`CREATE VIRTUAL TABLE q1 USING fts4(content="t1")`,
			`CREATE VIRTUAL TABLE q2 USING fts4(content='t1')`,
			`CREATE VIRTUAL TABLE q3 USING fts4(content=[t1])`,
			`CREATE VIRTUAL TABLE q4 USING fts4(content= t1)`,
			`CREATE VIRTUAL TABLE q5 USING fts4(content =t1)`,
			`CREATE VIRTUAL TABLE q6 USING fts4(content=t1, content=)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`SELECT * FROM q1`,
			`SELECT * FROM q2`,
			`SELECT * FROM q3`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// ftsContentInterchangeWrite builds an external-content and a contentless fts4
// table with a real index in each. Nothing here may depend on which engine
// runs it, which is what makes the read below a byte comparison.
var ftsContentInterchangeWrite = []string{
	`CREATE TABLE src(a, b)`,
	`INSERT INTO src(rowid, a, b) VALUES(1, 'hello world', 'tail one')`,
	`INSERT INTO src(rowid, a, b) VALUES(2, 'abcd abcde 7', 'tail two')`,
	`INSERT INTO src(rowid, a, b) VALUES(200, 'bb cc', 'tail three')`,
	`CREATE VIRTUAL TABLE t USING fts4(content=src)`,
	`INSERT INTO t(t) VALUES('rebuild')`,
	`CREATE VIRTUAL TABLE u USING fts4(content=, x)`,
	`INSERT INTO u(docid, x) VALUES(4, 'hello world')`,
	`INSERT INTO u(docid, x) VALUES(9, 'abcd abcde')`,
	`INSERT INTO u(docid, x) VALUES(11, 'bb cc')`,
}

var ftsContentInterchangeRead = []string{
	`SELECT rowid, a, b FROM t ORDER BY rowid`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM u_segdir ORDER BY level, idx`,
	`SELECT docid, quote(size) FROM u_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM u_stat ORDER BY id`,
	`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts3ContentFileInterchange writes the two content= shapes with each
// engine and reads the result back with both. A "content=" table has no
// %_content of its own, so every byte either reader sees is a segment,
// %_docsize or %_stat byte -- there is nothing else left to agree about.
func TestFts3ContentFileInterchange(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "ftscontent.db")
			runWithDSN(t, writer, dsn, ftsContentInterchangeWrite)
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, ftsContentInterchangeRead))
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

// TestFts3ContentIsSearchableByCSQLite is the sharpest form of the claim:
// this engine writes the database, and C SQLite -- which never saw it
// being written -- searches BOTH tables through the index it found on disk,
// runs fts3's own 'integrity-check' against the content table, and keeps
// writing to them. A segment blob that decoded to anything but exactly the
// right postings shows up as a missing or extra row; one whose PAGE STRUCTURE
// is wrong shows up in integrity_check, which a MATCH alone would not catch.
func TestFts3ContentIsSearchableByCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "ftscontent.db")
	runWithDSN(t, "musql", dsn, ftsContentInterchangeWrite)

	sdb, err := sql.Open("sqlite3", exportedForOracle(t, dsn))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	// PRAGMA integrity_check validates every fts4 table's inverted index
	// against its content table. A CONTENTLESS one has none, so C SQLite
	// reports "unable to validate" for it -- on a database IT wrote too
	// (verified), which is why only the external-content table's verdict is
	// asserted here. It must be silent: nothing may be said about main.t.
	assertNoFtsComplaintAbout(t, sdb, "main.t", "a content= database this engine wrote")

	cases := []struct {
		query string
		want  string
	}{
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'hello' ORDER BY docid)`, "1"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'abc*' ORDER BY docid)`, "2"},
		// The value 7 was indexed through its TEXT rendering, from the CONTENT
		// table rather than a %_content of its own.
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH '7' ORDER BY docid)`, "2"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'bb' ORDER BY docid)`, "200"},
		{`SELECT group_concat(rowid) FROM (SELECT rowid FROM t ORDER BY rowid)`, "1,2,200"},
		{`SELECT offsets(t) FROM t WHERE t MATCH 'abcde'`, "0 0 5 5"},
		// And the contentless one, whose columns can never be read at all.
		{`SELECT group_concat(docid) FROM (SELECT docid FROM u WHERE u MATCH 'hello' ORDER BY docid)`, "4"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM u WHERE u MATCH 'abcd' ORDER BY docid)`, "9"},
		{`SELECT count(*) FROM u WHERE u MATCH 'bb'`, "1"},
	}
	for _, c := range cases {
		var got string
		if err := sdb.QueryRow(c.query).Scan(&got); err != nil {
			t.Errorf("C SQLite failed on %q over a database this engine wrote: %v", c.query, err)
			continue
		}
		if got != c.want {
			t.Errorf("C SQLite disagrees about this engine's own content= index\n  query: %s\n  got:   %q\n  want:  %q", c.query, got, c.want)
		}
	}

	// fts3's OWN integrity-check re-tokenizes the content table and compares
	// it against the segments -- the check PRAGMA integrity_check cannot make
	// for a contentless table and makes only implicitly for the other. It must
	// PASS on the index this engine built from src, and then FAIL once a src
	// row goes away behind it, which is the one thing it exists to detect.
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Errorf("C SQLite's own 'integrity-check' rejected an index this engine built from that very content table: %v", err)
	}
	if _, err := sdb.Exec(`DELETE FROM src WHERE rowid=200`); err != nil {
		t.Fatalf("C SQLite could not delete the content row: %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err == nil {
		t.Errorf("C SQLite's own 'integrity-check' passed on an index deliberately out of step with its content table")
	}
	// And that same state is what a MATCH must still answer from the INDEX,
	// with the columns it cannot look up coming back NULL rather than as an
	// error -- the behaviour only a content= table has.
	for _, c := range [][2]string{
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'bb' ORDER BY docid)`, "200"},
		{`SELECT ifnull(group_concat(a),'') FROM (SELECT a FROM t WHERE t MATCH 'bb')`, ""},
		{`SELECT group_concat(rowid) FROM (SELECT rowid FROM t ORDER BY rowid)`, "1,2"},
	} {
		var got string
		if err := sdb.QueryRow(c[0]).Scan(&got); err != nil {
			t.Errorf("C SQLite failed on %q: %v", c[0], err)
		} else if got != c[1] {
			t.Errorf("C SQLite disagrees\n  query: %s\n  got:   %q\n  want:  %q", c[0], got, c[1])
		}
	}

	// And it can keep WRITING: a row C SQLite adds must be findable next to
	// one this engine wrote, which only works if it parsed the segments it
	// found.
	if _, err := sdb.Exec(`INSERT INTO u(docid,x) VALUES(50,'hello again')`); err != nil {
		t.Fatalf("C SQLite could not insert into a contentless table this engine created: %v", err)
	}
	var got string
	if err := sdb.QueryRow(`SELECT group_concat(docid) FROM (SELECT docid FROM u WHERE u MATCH 'hello' ORDER BY docid)`).Scan(&got); err != nil {
		t.Fatalf("MATCH after a real-SQLite insert: %v", err)
	}
	if got != "4,50" {
		t.Errorf("after C SQLite appended a row, MATCH 'hello' returned %q, want \"4,50\"", got)
	}
	// The b-tree pages C SQLite just wrote sit on top of this engine's, so
	// the structural half of integrity_check must still be silent about u.
	assertNoFtsComplaintAbout(t, sdb, "u_", "a contentless table after a real-SQLite write")
}

// assertNoFtsComplaintAbout fails unless every PRAGMA integrity_check line is
// silent about the named table. It is not "== ok": a database holding a
// CONTENTLESS fts4 table can never be, since integrity_check tries to validate
// that table's inverted index against a content table it does not have and
// reports "unable to validate" -- on a database C SQLite wrote itself just
// the same (verified against mattn/go-sqlite3 3.53.3).
func assertNoFtsComplaintAbout(t *testing.T, sdb *sql.DB, table, what string) {
	t.Helper()
	rows, err := sdb.Query(`PRAGMA integrity_check`)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("integrity_check scan: %v", err)
		}
		if line != "ok" && strings.Contains(line, table) {
			t.Fatalf("C SQLite reports integrity_check %q about %s on %s", line, table, what)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
}
