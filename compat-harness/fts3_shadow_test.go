// Differential tests for fts3/fts4 virtual tables. Tests verify shadow table
// byte-identity, file interchange with C SQLite, and proper rejection of
// unsupported operations.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestFts3ShadowLayoutDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fts4 schema and first segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT rowid, a FROM t ORDER BY rowid`,
			`SELECT docid, c0a, c1b FROM t_content ORDER BY docid`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`SELECT count(*) FROM t_segments`,
			`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
		}},
		{"INSERT ... SELECT builds ONE segment, like a multi-row VALUES list", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`CREATE TABLE src(x,y)`,
			`INSERT INTO src VALUES('alpha beta','A B'),('gamma delta','C D'),('epsilon','E')`,
			`INSERT INTO t(a,b) SELECT x,y FROM src`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT a FROM t WHERE t MATCH 'gamma' ORDER BY docid`,
			`SELECT a FROM t WHERE t MATCH 'C' ORDER BY docid`,
			// Shadow bytes verify segmentation matches C fts3 exactly.
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, c0a, c1b FROM t_content ORDER BY docid`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`SELECT count(*) FROM t_segments`,
		}},
		{"INSERT ... SELECT: no column list, an expression, a filter, an empty source", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`CREATE TABLE src(x)`,
			`INSERT INTO src VALUES('one'),('two'),('three')`,
			`INSERT INTO t SELECT upper(x) FROM src WHERE x LIKE 't%'`,
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT a FROM t WHERE t MATCH 'TWO'`,
			`INSERT INTO t(a) SELECT x FROM src WHERE 0`,
			`SELECT count(*) FROM t`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
		}},
		{"INSERT ... SELECT with explicit ascending docids, either column order", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`CREATE VIRTUAL TABLE u USING fts4(x)`,
			`CREATE TABLE src(v)`,
			`INSERT INTO src VALUES('aa'),('bb')`,
			`INSERT INTO t(docid,x) SELECT rowid+10, v FROM src ORDER BY rowid`,
			`INSERT INTO u(x,docid) SELECT v, rowid+20 FROM src ORDER BY rowid`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT docid, x FROM u ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM u_segdir ORDER BY level, idx`,
		}},
		{"INSERT ... SELECT sourcing the fts table itself", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(x) VALUES('seed one')`,
			`INSERT INTO t(x) SELECT x FROM t`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT x FROM t WHERE t MATCH 'seed' ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
		}},
		{"INSERT ... SELECT of a non-text value indexes its TEXT rendering", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t(x) SELECT 7`,
			`SELECT x, typeof(x) FROM t`,
			`SELECT x FROM t WHERE t MATCH '7'`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		}},
		{"an fts table inside a subquery FROM", []string{
			// FTS table columns are declared by the module's Connect.
			`CREATE VIRTUAL TABLE ft USING fts4(c)`,
			`INSERT INTO ft(c) VALUES('x'),('q')`,
			`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)`,
			`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`,
			`SELECT a,b FROM t1 WHERE b IN (SELECT c FROM ft) ORDER BY a`,
			`SELECT a,b FROM t1 WHERE b NOT IN (SELECT c FROM ft) ORDER BY a`,
			`SELECT (SELECT count(*) FROM ft)`,
			`SELECT count(*) FROM t1 WHERE EXISTS(SELECT 1 FROM ft)`,
			`SELECT a FROM t1 WHERE b IN (SELECT c FROM ft WHERE ft MATCH 'x') ORDER BY a`,
		}},
		{"fts3 has only three shadow tables", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a,b)`,
			`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
			`INSERT INTO t(a,b) VALUES('hello there','baz')`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		}},
		{"no argument list declares a content column", []string{
			`CREATE VIRTUAL TABLE t2 USING fts3`,
			`CREATE VIRTUAL TABLE t3 USING fts4()`,
			`INSERT INTO t2 VALUES('one two')`,
			`INSERT INTO t3 VALUES('one two')`,
			`SELECT docid, content FROM t2`,
			`SELECT docid, content FROM t3`,
			`SELECT sql FROM sqlite_master WHERE name IN ('t2_content','t3_content') ORDER BY name`,
		}},
		{"prefix compression across terms", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t VALUES('abc abcd abcde xyz')`,
			`SELECT quote(root) FROM t_segdir`,
		}},
		{"multi-row VALUES is a single segment", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t VALUES('p'),('q'),('r')`,
			`SELECT count(*) FROM t_segdir`,
			`SELECT level, idx, quote(root) FROM t_segdir`,
			`SELECT docid, a FROM t ORDER BY docid`,
		}},
		{"explicit docid, a large docid gap, and the little-endian varint", []string{
			// The docid delta 200-3 == 197 encodes as the two bytes C5 01
			// only under FTS3's LITTLE-endian varint; the file-format varint
			// in engine/varint.go would write 01 45 and read back 8833.
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(docid,a) VALUES(3,'aa bb'),(9,'aa cc aa'),(200,'bb')`,
			`SELECT quote(root) FROM t_segdir`,
			`SELECT docid, a FROM t ORDER BY docid`,
		}},
		{"the simple tokenizer folds ASCII only", []string{
			// "Ab_cD 12x e-u ,, x1" with two raw UTF-8 bytes sequences: an
			// underscore and a hyphen are delimiters, ASCII A-Z folds, and
			// bytes >= 0x80 are token characters passed through unchanged.
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t VALUES('Ab_cD 12x ` + "é-ü" + ` ,, x1')`,
			`SELECT quote(root) FROM t_segdir`,
		}},
		{"non-text values index through their TEXT rendering", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t VALUES(NULL,NULL)`,
			`INSERT INTO t VALUES(1.5, x'414243')`,
			`INSERT INTO t VALUES(-0.0, 1e300)`,
			`SELECT docid, quote(a), quote(b) FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT id, quote(value) FROM t_stat`,
			// An all-NULL document indexes no term at all, so it produces NO
			// %_segdir row -- it is still counted in %_stat's document total.
			`SELECT count(*) FROM t_segdir`,
		}},
		{"three columns: docsize, stat and the column-change marker", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b,c)`,
			`INSERT INTO t VALUES('one two','three','four five six')`,
			`INSERT INTO t VALUES('one','','')`,
			`SELECT quote(root) FROM t_segdir ORDER BY idx`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT quote(value) FROM t_stat`,
		}},
		{"quoted and bracketed column names", []string{
			`CREATE VIRTUAL TABLE t USING fts4([x y], "zz")`,
			`INSERT INTO t VALUES('p','q')`,
			`SELECT sql FROM sqlite_master WHERE name='t_content'`,
			`SELECT docid, "x y", zz FROM t`,
		}},
		{"DROP TABLE takes the shadow tables with it", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t VALUES('x','y')`,
			`SELECT count(*) FROM sqlite_master`,
			`DROP TABLE t`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			// The name is free again, which it would not be if a shadow table
			// had been left behind.
			`CREATE VIRTUAL TABLE t USING fts4(q)`,
			`SELECT sql FROM sqlite_master WHERE name='t_content'`,
		}},
		// fts3DestroyMethod is FIVE unconditional "DROP TABLE IF EXISTS", one
		// per shadow name, whatever the table actually owns -- so the DROP takes
		// a same-named USER table with it, including the %_docsize and %_stat a
		// plain fts3 table never had. Verified against the oracle: with both
		// hand-made, "DROP TABLE t3" leaves sqlite_master EMPTY.
		{"DROP takes all five shadow names, even the two fts3 lacks", []string{
			`CREATE VIRTUAL TABLE t3 USING fts3(a, b)`,
			`CREATE TABLE t3_docsize(x)`,
			`CREATE TABLE t3_stat(y)`,
			`INSERT INTO t3 VALUES('one','two')`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`DROP TABLE t3`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`SELECT x FROM t3_docsize`,
		}},
		{"a taken shadow name fails the whole CREATE", []string{
			`CREATE TABLE t_segdir(x)`,
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		{"duplicate docid is a constraint failure", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(5,'x')`,
			`INSERT INTO t(docid,a) VALUES(5,'y')`,
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT count(*) FROM t_segdir`,
		}},
		{"rowid is a spelling of docid", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(rowid,a) VALUES(11,'x')`,
			`SELECT rowid, docid, a FROM t`,
		}},
		{"a duplicate column name and a column named docid are rejected", []string{
			// The leading real CREATE is not decoration: a statement that
			// fails as the very FIRST one against a not-yet-created file
			// leaves this driver's session unusable (a pre-existing
			// driver artifact, unrelated to fts3), which would mask what
			// this case is actually pinning.
			`CREATE TABLE keep(x)`,
			`CREATE VIRTUAL TABLE t4 USING fts4(a, a)`,
			`CREATE VIRTUAL TABLE t5 USING fts4(docid)`,
			`SELECT count(*) FROM sqlite_master`,
		}},
		{"fifteen separate statements are fifteen level-0 segments", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t VALUES('w0')`, `INSERT INTO t VALUES('w1')`,
			`INSERT INTO t VALUES('w2')`, `INSERT INTO t VALUES('w3')`,
			`INSERT INTO t VALUES('w4')`, `INSERT INTO t VALUES('w5')`,
			`INSERT INTO t VALUES('w6')`, `INSERT INTO t VALUES('w7')`,
			`INSERT INTO t VALUES('w8')`, `INSERT INTO t VALUES('w9')`,
			`INSERT INTO t VALUES('w10')`, `INSERT INTO t VALUES('w11')`,
			`INSERT INTO t VALUES('w12')`, `INSERT INTO t VALUES('w13')`,
			`INSERT INTO t VALUES('w14')`, `INSERT INTO t VALUES('w15')`,
			`SELECT level, idx, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a FROM t ORDER BY docid`,
		}},
		{"a docid-only row indexes nothing", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid) VALUES(-5)`,
			`SELECT docid, quote(a), quote(b) FROM t`,
			`SELECT count(*) FROM t_segdir`,
			`SELECT docid, quote(size) FROM t_docsize`,
			`SELECT quote(value) FROM t_stat`,
		}},
		{"an explicit docid then an auto one continues from it", []string{
			// Real fts3 writes each %_content row as it goes, so the second
			// row's auto docid is 6, not 1.
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(5,'x'),(NULL,'y')`,
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT quote(root) FROM t_segdir`,
		}},
		{"a negative docid delta is a ten-byte varint", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(docid,a) VALUES(-5,'zz')`,
			`SELECT quote(root) FROM t_segdir`,
			`SELECT docid, a FROM t`,
		}},
		// A join whose ON/WHERE equates an ordinary table's INTEGER PRIMARY
		// KEY with a VIRTUAL table's rowid used to configure a rowid seek on
		// the virtual table's pre-materialized cursor and dereference its nil
		// pager -- a panic, the conformance gate's hardest failure. rtree
		// reproduced it identically, so it predates fts3.
		{"join seeking a virtual table by rowid", []string{
			`CREATE VIRTUAL TABLE t1 USING fts3(c)`,
			`CREATE TABLE t2(id INTEGER PRIMARY KEY AUTOINCREMENT, weight INTEGER UNIQUE)`,
			`INSERT INTO t2 VALUES(1,10)`,
			`INSERT INTO t2 VALUES(2,5)`,
			`INSERT INTO t1(docid,c) VALUES(1,'This is a test')`,
			`INSERT INTO t1(docid,c) VALUES(2,'That was a test')`,
			`SELECT t1.rowid, weight FROM t1, t2 WHERE t2.weight>5 AND t2.id = t1.rowid ORDER BY weight`,
			`SELECT docid, weight FROM t1, t2 WHERE t2.id = t1.docid ORDER BY weight`,
			`SELECT docid FROM t1 WHERE docid IN (1, 2, 10)`,
		}},
		{"end_block is TEXT, not an integer", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t VALUES('one two three')`,
			`SELECT typeof(end_block), end_block, length(root) FROM t_segdir`,
		}},
		{"reopen: the shadow tables survive a close", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t VALUES('alpha beta','gamma')`,
			`INSERT INTO t VALUES('alpha delta','gamma')`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY idx`,
			`SELECT quote(value) FROM t_stat`,
		}},
		// --- DELETE and UPDATE (engine/fts3_write.go) ---
		{"a DELETE appends a segment of delete markers", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa bb','cc')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'bb dd','ee')`,
			`DELETE FROM t WHERE docid=1`,
			`SELECT level, idx, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT quote(value) FROM t_stat`,
			`SELECT docid FROM t WHERE t MATCH 'bb' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'aa' ORDER BY docid`,
		}},
		{"a multi-row DELETE is still ONE segment, in term order", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa bb','p')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'aa cc','q')`,
			`INSERT INTO t(docid,a,b) VALUES(3,'bb cc','r')`,
			`INSERT INTO t(docid,a,b) VALUES(4,'dd','s')`,
			// Written descending on purpose: SQLite still visits the rows in
			// ascending docid order, so both engines write one segment whose
			// shared terms ("aa", "bb") each carry TWO delete markers.
			`DELETE FROM t WHERE docid IN (3,1)`,
			`SELECT level, idx, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'aa' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'bb' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'cc' ORDER BY docid`,
		}},
		{"deleting the last row wipes every shadow table", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa','p')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'bb','q')`,
			`DELETE FROM t`,
			`SELECT count(*) FROM t_segdir`,
			`SELECT count(*) FROM t_content`,
			`SELECT count(*) FROM t_docsize`,
			`SELECT count(*) FROM t_segments`,
			`SELECT id, quote(value) FROM t_stat`,
			`SELECT docid FROM t WHERE t MATCH 'aa'`,
			// And the table is usable again afterwards, from %_segdir idx 0.
			`INSERT INTO t(docid,a,b) VALUES(5,'zz','yy')`,
			`SELECT level, idx, quote(root) FROM t_segdir`,
			`SELECT docid FROM t WHERE t MATCH 'zz'`,
		}},
		{"a DELETE that matches nothing changes nothing", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa','p')`,
			`DELETE FROM t WHERE docid=99`,
			`DELETE FROM t WHERE a='nosuch'`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY idx`,
			`SELECT quote(value) FROM t_stat`,
			`SELECT docid, a, b FROM t`,
		}},
		{"an UPDATE folds its delete and insert into one segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa bb','cc')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'bb dd','ee')`,
			`UPDATE t SET a='bb ff' WHERE docid=2`,
			`SELECT level, idx, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT quote(value) FROM t_stat`,
			`SELECT docid FROM t WHERE t MATCH 'dd' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'ff' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'bb' ORDER BY docid`,
		}},
		{"an UPDATE that changes nothing rewrites the same postings", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa','p')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'bb cc','q')`,
			`UPDATE t SET a=a WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT quote(value) FROM t_stat`,
			`SELECT docid FROM t WHERE t MATCH 'bb' ORDER BY docid`,
		}},
		{"updating a one-row table wipes and restarts at idx 0", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'aa bb')`,
			`INSERT INTO t(docid,a) VALUES(2,'bb cc')`,
			`DELETE FROM t WHERE docid=1`,
			`UPDATE t SET a='dd' WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a FROM t`,
			`SELECT docid FROM t WHERE t MATCH 'dd'`,
			`SELECT docid FROM t WHERE t MATCH 'bb'`,
		}},
		{"a multi-row UPDATE, NULL and non-text values", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa','p')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'bb','q')`,
			`INSERT INTO t(docid,a,b) VALUES(3,'cc','r')`,
			`UPDATE t SET a=NULL WHERE docid=1`,
			`UPDATE t SET a=7, b=upper(b) WHERE docid>=2`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, quote(a), quote(b) FROM t ORDER BY docid`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT quote(value) FROM t_stat`,
			`SELECT docid FROM t WHERE t MATCH '7' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'aa' ORDER BY docid`,
			`SELECT docid, offsets(t) FROM t WHERE t MATCH 'q OR r' ORDER BY docid`,
		}},
		{"delete then re-insert the same docid", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'aa')`,
			`INSERT INTO t(docid,a) VALUES(2,'bb')`,
			`DELETE FROM t WHERE docid=1`,
			`INSERT INTO t(docid,a) VALUES(1,'cc aa')`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'aa' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'cc' ORDER BY docid`,
			`SELECT quote(value) FROM t_stat`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}

// ftsInterchangeWrite is the program whose FILE both engines must agree on.
// Kept deliberately varied: several segments, an explicit docid far past the
// others (exercising a multi-byte docid delta), a prefix-compressible term
// run, and a non-text column value.
var ftsInterchangeWrite = []string{
	`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
	`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	`INSERT INTO t VALUES('abc abcd abcde xyz', 7)`,
	`INSERT INTO t(docid,a,b) VALUES(200,'bb',NULL)`,
}

// ftsInterchangeRead re-reads everything a reader can see WITHOUT the fts
// query language: the table's own rows and every shadow table's raw bytes.
var ftsInterchangeRead = []string{
	`SELECT docid, a, b, typeof(a), typeof(b) FROM t ORDER BY docid`,
	`SELECT rowid, a FROM t ORDER BY rowid`,
	`SELECT docid, c0a, c1b FROM t_content ORDER BY docid`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts3FileInterchange writes an fts4 database with each engine and reads
// it back with both. Everything either reader sees must be identical, which
// for the shadow tables means byte-identical segment blobs.
func TestFts3FileInterchange(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "fts.db")
			runWithDSN(t, writer, dsn, ftsInterchangeWrite)
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, ftsInterchangeRead))
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

// TestFts3IndexIsSearchableByCSQLite is the sharpest form of the
// interchange claim: this engine writes the database, and C SQLite --
// which never saw it being written -- searches it THROUGH THE FTS INDEX it
// found on disk. A segment blob that decoded to anything other than exactly
// the right postings would show up here as a missing or extra row, and a
// malformed one as a corrupt-database error.
func TestFts3IndexIsSearchableByCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fts.db")
	runWithDSN(t, "musql", dsn, ftsInterchangeWrite)

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

	cases := []struct {
		query string
		want  string
	}{
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'hello' ORDER BY docid)`, "1"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'abcd' ORDER BY docid)`, "2"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'abc*' ORDER BY docid)`, "2"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'bb' ORDER BY docid)`, "200"},
		// The integer column value 7 was indexed through its TEXT rendering.
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH '7' ORDER BY docid)`, "2"},
		// A term nobody indexed.
		{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'nosuchterm')`, ""},
		// offsets() decodes the POSITIONS out of the doclist, not just the
		// docids: "abcde" is the third token of column 0, at byte offset 9.
		{`SELECT offsets(t) FROM t WHERE t MATCH 'abcde'`, "0 0 9 5"},
		// A two-term implicit-AND query, which intersects two doclists.
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'hello world' ORDER BY docid)`, "1"},
		{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'hello abcd')`, ""},
	}
	for _, c := range cases {
		var got string
		if err := sdb.QueryRow(c.query).Scan(&got); err != nil {
			t.Errorf("C SQLite failed on %q over a database this engine wrote: %v", c.query, err)
			continue
		}
		if got != c.want {
			t.Errorf("C SQLite disagrees about this engine's own index\n  query: %s\n  got:   %q\n  want:  %q", c.query, got, c.want)
		}
	}

	// And C SQLite can keep WRITING to it: a row it adds must be findable
	// by a query that also matches a row this engine wrote, which only works
	// if it could parse the segments already there.
	if _, err := sdb.Exec(`INSERT INTO t(a,b) VALUES('hello again','tail')`); err != nil {
		t.Fatalf("C SQLite could not insert into a table this engine created: %v", err)
	}
	var got string
	if err := sdb.QueryRow(`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'hello' ORDER BY docid)`).Scan(&got); err != nil {
		t.Fatalf("MATCH after a real-SQLite insert: %v", err)
	}
	if got != "1,201" {
		t.Errorf("after C SQLite appended a row, MATCH 'hello' returned %q, want \"1,201\"", got)
	}
	if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
		t.Errorf("integrity_check after a real-SQLite write: %q (%v)", ic, err)
	}
}

// ftsMutateWrite is the DELETE/UPDATE version of ftsInterchangeWrite. It is
// arranged so the resulting index CANNOT be read correctly by an engine that
// ignores delete markers: docid 2's "gamma" survives only in an older segment
// (the marker that retires it is in a newer one), docid 3 is deleted outright,
// and docid 4 is updated so one of its terms is retired while another that the
// same statement re-inserts must NOT be.
var ftsMutateWrite = []string{
	`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
	`INSERT INTO t(docid,a,b) VALUES(1,'alpha beta','gamma')`,
	`INSERT INTO t(docid,a,b) VALUES(2,'beta gamma','delta')`,
	`INSERT INTO t(docid,a,b) VALUES(3,'gamma delta','epsilon')`,
	`INSERT INTO t(docid,a,b) VALUES(4,'zeta eta','theta')`,
	`DELETE FROM t WHERE docid=3`,
	`UPDATE t SET a='zeta iota' WHERE docid=4`,
	`UPDATE t SET b='kappa' WHERE docid=2`,
}

// ftsMutateRead re-reads a mutated table without the fts query language: its
// rows and every shadow table's raw bytes, so a delete marker written or
// decoded differently shows up as different bytes rather than as a missing row.
var ftsMutateRead = []string{
	`SELECT docid, a, b FROM t ORDER BY docid`,
	`SELECT docid, c0a, c1b FROM t_content ORDER BY docid`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
}

// ftsMutateMatch is what the index itself must answer afterwards -- the part
// only a reader that honours delete markers gets right.
var ftsMutateMatch = []struct{ query, want string }{
	// docid 2's "gamma" is retired and re-inserted by the SAME statement (the
	// UPDATE only touched column b, but fts3 re-indexes the whole row), so it
	// must survive: an engine that wrote a marker and a posting as two
	// separate doclist entries instead of folding them into one would drop it.
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'gamma' ORDER BY docid)`, "1,2"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'beta' ORDER BY docid)`, "1,2"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'delta' ORDER BY docid)`, ""},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'epsilon' ORDER BY docid)`, ""},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'kappa' ORDER BY docid)`, "2"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'zeta' ORDER BY docid)`, "4"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'eta' ORDER BY docid)`, ""},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'iota' ORDER BY docid)`, "4"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH '"zeta iota"' ORDER BY docid)`, "4"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'et*' ORDER BY docid)`, ""},
	{`SELECT offsets(t) FROM t WHERE t MATCH 'iota'`, "0 0 5 4"},
}

// TestFts3MutatedFileInterchange is the DELETE/UPDATE half of the interchange
// claim, and the load-bearing test for this engine's delete-marker encoding: a
// database each engine wrote AND THEN MUTATED must read back identically under
// both, down to the segment blobs.
func TestFts3MutatedFileInterchange(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "fts.db")
			runWithDSN(t, writer, dsn, ftsMutateWrite)
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, ftsMutateRead))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s writes+mutates] readers disagree\n  %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}

// TestFts3MutatedIndexIsSearchableByCSQLite closes the loop the other way
// round from the reader tests: this engine DELETEs and UPDATEs, and real C
// SQLite -- which never saw any of it happen -- has to integrity_check the
// file, resolve every delete marker in it, and go on writing to it.
func TestFts3MutatedIndexIsSearchableByCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fts.db")
	runWithDSN(t, "musql", dsn, ftsMutateWrite)

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
		t.Fatalf("C SQLite reports integrity_check = %q on a database this engine mutated", ic)
	}
	for _, c := range ftsMutateMatch {
		var got string
		if err := sdb.QueryRow(c.query).Scan(&got); err != nil {
			t.Errorf("C SQLite failed on %q over a database this engine mutated: %v", c.query, err)
			continue
		}
		if got != c.want {
			t.Errorf("C SQLite disagrees about this engine's own delete markers\n  query: %s\n  got:   %q\n  want:  %q", c.query, got, c.want)
		}
	}

	// C SQLite must be able to keep MUTATING it too: its own DELETE reads
	// this engine's segments (to tokenize the row it is retiring) and appends
	// markers of its own on top of them.
	if _, err := sdb.Exec(`DELETE FROM t WHERE docid=1`); err != nil {
		t.Fatalf("C SQLite could not delete from a table this engine mutated: %v", err)
	}
	if _, err := sdb.Exec(`UPDATE t SET a='lambda' WHERE docid=2`); err != nil {
		t.Fatalf("C SQLite could not update a table this engine mutated: %v", err)
	}
	var got string
	if err := sdb.QueryRow(`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'beta' ORDER BY docid)`).Scan(&got); err != nil {
		t.Fatalf("MATCH after real-SQLite mutations: %v", err)
	}
	if got != "" {
		t.Errorf("after C SQLite retired the last 'beta', MATCH returned %q, want \"\"", got)
	}
	if err := sdb.QueryRow(`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'lambda')`).Scan(&got); err != nil {
		t.Fatalf("MATCH after real-SQLite mutations: %v", err)
	}
	if got != "2" {
		t.Errorf("after C SQLite's own UPDATE, MATCH 'lambda' returned %q, want \"2\"", got)
	}
	if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
		t.Errorf("integrity_check after real-SQLite mutations: %q (%v)", ic, err)
	}
}

// TestFts3DeclinedShapes pins everything deliberately out of scope. Each
// statement must fail with a clean error in this engine -- C SQLite
// ACCEPTS all of them, so they can never be run through differ(), and an
// engine that quietly started accepting one would be returning a WRONG answer
// rather than an incomplete one.
func TestFts3DeclinedShapes(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		bad   string
	}{
		// MATCH itself is now ANSWERED (fts3_match_test.go gates it against
		// the oracle). What stays declined is a MALFORMED query -- and the
		// load-bearing form of that is an EMPTY table, where a per-row MATCH
		// check never runs at all, so this used to return zero rows and claim
		// success while C SQLite rejects the statement outright.
		// fts3expr.test contributed 14 such divergences.
		{"MATCH on an empty table", []string{`CREATE VIRTUAL TABLE t USING fts3(a,b,c)`},
			`SELECT * FROM t WHERE t MATCH 'example AND (hello OR world))'`},
		{"MATCH on an ordinary table", []string{`CREATE TABLE ord(x)`},
			`SELECT * FROM ord WHERE ord MATCH 'q'`},
		{"multi-row INSERT with descending docids", []string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			`INSERT INTO t(docid,a) VALUES(9,'aa'),(3,'aa bb')`},
		{"rowid and docid both named", []string{`CREATE VIRTUAL TABLE t USING fts3(c)`},
			`INSERT INTO t(rowid, docid, c) VALUES (14, 15, 'bad test')`},
		// offsets(), snippet() and now matchinfo() are all ANSWERED over a
		// cursor with no query at all -- TestFts3MatchinfoNoQuery
		// (fts3_match_test.go) gates matchinfo()'s own no-query blob (the
		// empty one, for every format string) against the oracle.
		// INSERT ... SELECT is now ANSWERED too (its rows go through the very
		// same staging loop a multi-row VALUES list uses, so the statement still
		// builds exactly ONE segment -- which is what C fts3 does per
		// statement). TestFts3ShadowLayoutDiff gates it on the shadow BYTES. What
		// stays out is the descending-docid form below, for the same reason the
		// VALUES spelling does.
		{"INSERT ... SELECT with descending docids", []string{`CREATE VIRTUAL TABLE t USING fts4(a)`, `CREATE TABLE src(x)`, `INSERT INTO src VALUES('p'),('q')`},
			`INSERT INTO t(docid,a) SELECT 10-rowid, x FROM src ORDER BY rowid`},
		// UPDATE and DELETE are now ANSWERED (fts3_write.go); a docid-changing
		// UPDATE is now ANSWERED too, including the mid-statement flush a
		// docid moving downwards needs (fts3_docid_update_test.go gates it on
		// the shadow BYTES). What stays out is a MATCH in the WHERE clause
		// (the write session has no index reader for one).
		{"DELETE with a MATCH in WHERE", []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`, `INSERT INTO t VALUES('x','y')`},
			`DELETE FROM t WHERE t MATCH 'x'`},
		{"UPDATE with a MATCH in WHERE", []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`, `INSERT INTO t VALUES('x','y')`},
			`UPDATE t SET a='z' WHERE t MATCH 'x'`},
		// INSERT OR REPLACE is now ANSWERED -- fts3 implements that one
		// conflict mode itself, as a delete-then-insert into one segment
		// (fts3_replace_conflict_test.go gates it on the shadow BYTES, in both
		// interchange directions and under a fuzz). Every OTHER conflict mode
		// still is not.
		{"INSERT OR IGNORE", []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`},
			`INSERT OR IGNORE INTO t VALUES('x','y')`},
		// 'optimize', 'rebuild' and 'integrity-check' are now ANSWERED
		// (engine/fts3_command.go, gated on the shadow BYTES by
		// fts3_command_channel_test.go). What stays declined there is
		// 'merge='/'automerge=', which that file's own TestFts3CommandDeclined
		// pins.
		{"tokenize= option", nil, `CREATE VIRTUAL TABLE t USING fts4(a, tokenize=icu)`},
		{"tokenize option, space spelling", nil, `CREATE VIRTUAL TABLE t USING fts3(a, tokenize icu)`},
		{"content= option", nil, `CREATE VIRTUAL TABLE t USING fts4(content=src)`},
		// notindexed=, matchinfo=fts3, prefix= and languageid= are now ANSWERED
		// (engine/vtab_fts3.go's parseSchema, engine/fts3_prefix.go,
		// engine/fts3_langid.go), gated on the shadow BYTES -- and on the
		// spellings C fts3 refuses -- by fts3_notindexed_test.go,
		// fts3_matchinfo_option_test.go, fts3_prefix_test.go and
		// fts3_languageid_test.go.
		// A declared column TYPE was declined here too. It is now implemented:
		// C fts3 keeps only the argument's first token and discards the rest,
		// so the column is untyped -- see fts3_module_args_test.go, which
		// compares the resulting column set and segment bytes against the oracle
		// rather than merely checking the statement is refused.
		// A "temp."-qualified fts3/fts4/fts4aux/fts3tokenize CREATE used to
		// land silently in main here -- a wrong answer rather than a missing
		// feature -- so it stayed in this file's DECLINED list even after
		// "temp." became an accepted qualifier for an ordinary table/view/
		// trigger. It is now ANSWERED for this family too (createShadowTables
		// puts each shadow table in the SAME catalog as the fts3/fts4 table
		// itself); see vtab_temp_catalog_test.go for the oracle evidence and
		// the catalog assertions. Every OTHER module (rtree, fts5, ...) keeps
		// this exact decline -- TestCreateVirtualTableTempNonFts3Declined.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openMusqlFts(t)
			for _, s := range c.setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if rows, err := db.Query(c.bad); err == nil {
				// A row-returning statement may only fail at iteration time.
				iterErr := rows.Err()
				for rows.Next() {
				}
				if iterErr == nil {
					iterErr = rows.Err()
				}
				rows.Close()
				if iterErr == nil {
					t.Fatalf("engine ACCEPTED an out-of-scope fts3/fts4 statement (C SQLite answers it differently): %s", c.bad)
				}
			}
		})
	}
}

// The two data-dependent declines this file used to gate are both gone. A
// segment too large for a single root node now SPILLS into %_segments
// byte-for-byte like C fts3 (fts3_spill_test.go), and the 16th level-0
// segment now MERGES into a level-1 one (engine/fts3_merge.go,
// fts3_merge_test.go) -- both gated by comparing the shadow tables' raw bytes
// against the oracle in both file directions, which is a far stronger claim
// than "the statement is refused". DML inside an explicit transaction was the
// third, now engine/fts3_txn.go.

func openMusqlFts(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fts.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, sqlText string) {
	t.Helper()
	if _, err := db.Exec(sqlText); err != nil {
		t.Fatalf("%q: %v", sqlText, err)
	}
}

func assertCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("%q: %v", query, err)
	}
	if got != want {
		t.Errorf("%q = %d, want %d", query, got, want)
	}
}
