// Tests the FTS4 "languageid=" module option against C SQLite 3.53.3.
// Language ID determines which shadow table levels store a row's terms.
// MATCH queries without a langid constraint search language 0 only.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestFts3LanguageidDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// The shape of the thing: %_content gains a trailing column literally
		// named "langid", the declared name is a HIDDEN column, and a row's
		// terms land at level langid*nIndex*1024.
		{"the shadow tables and the stored value", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a, b, languageid=lang_id)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`PRAGMA table_info(t1)`,
			`INSERT INTO t1(a,b) VALUES('alpha beta','gamma')`,
			`INSERT INTO t1(a,b,lang_id) VALUES('alpha beta','delta', 5)`,
			`SELECT docid, a, b, lang_id, typeof(lang_id) FROM t1 ORDER BY docid`,
			`SELECT * FROM t1 ORDER BY rowid`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t1_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t1_stat ORDER BY id`,
			`SELECT docid, quote(size) FROM t1_docsize ORDER BY docid`,
			`INSERT INTO t1(t1) VALUES('integrity-check')`,
		}},
		// sqlite3_value_int, not sqlite3_value_int64 and not the text: NULL and
		// 'xyz' are 0, 2.7 truncates to 2, and 2^32 wraps to 0 rather than
		// saturating. A NEGATIVE one is SQLITE_CONSTRAINT before anything is
		// written -- the row must not appear.
		{"the value is sqlite3_value_int", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, languageid=lid)`,
			`INSERT INTO t(a,lid) VALUES('one', NULL)`,
			`INSERT INTO t(a,lid) VALUES('two', 'xyz')`,
			`INSERT INTO t(a,lid) VALUES('three', 2.7)`,
			`INSERT INTO t(a,lid) VALUES('four', 4294967296)`,
			`INSERT INTO t(a,lid) VALUES('five', -3)`,
			`SELECT docid, a, lid, typeof(lid) FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}},
		// The query-side trap. An unconstrained MATCH sees language 0 alone; an
		// equality constraint moves it; a NON-equality one leaves it at 0, so
		// the filter then removes every row the search did find.
		{"a MATCH with no constraint searches language 0", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, languageid=lid)`,
			`INSERT INTO t(a,lid) VALUES('alpha', 0)`,
			`INSERT INTO t(a,lid) VALUES('bravo', 1)`,
			`INSERT INTO t(a,lid) VALUES('charlie', 2)`,
			`SELECT docid FROM t WHERE t MATCH 'alpha'`,
			`SELECT docid FROM t WHERE t MATCH 'bravo'`,
			`SELECT docid FROM t WHERE t MATCH 'bravo' AND lid=1`,
			`SELECT docid FROM t WHERE lid=1 AND t MATCH 'bravo'`,
			`SELECT docid FROM t WHERE t MATCH 'charlie' AND lid=2`,
			`SELECT docid FROM t WHERE t MATCH 'charlie' AND lid=1`,
			`SELECT docid FROM t WHERE t MATCH 'alpha' AND lid=0`,
			`SELECT docid FROM t WHERE lid=1`,
			`SELECT docid, a, lid FROM t ORDER BY docid`,
			`SELECT quote(matchinfo(t)) FROM t WHERE t MATCH 'bravo' AND lid=1`,
			`SELECT offsets(t) FROM t WHERE t MATCH 'bravo' AND lid=1`,
			`SELECT snippet(t) FROM t WHERE t MATCH 'bravo' AND lid=1`,
		}},
		// getAbsoluteLevel multiplies by nIndex, so a prefix= table's languages
		// are 1024*nIndex apart. Language 1's TERM index is at 3072 here, and
		// its 2-byte prefix index at 4096.
		{"the language multiplies by nIndex", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, languageid=lid, prefix="2,3")`,
			`SELECT sql FROM sqlite_master WHERE name='t_content'`,
			`INSERT INTO t(a,lid) VALUES('alpha', 0)`,
			`INSERT INTO t(a,lid) VALUES('bravo', 1)`,
			`INSERT INTO t(a,lid) VALUES('charlie', 7)`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid FROM t WHERE t MATCH 'al*'`,
			`SELECT docid FROM t WHERE t MATCH 'br*'`,
			`SELECT docid FROM t WHERE t MATCH 'br*' AND lid=1`,
			`SELECT docid FROM t WHERE t MATCH 'ch*' AND lid=7`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}},
		// optimize and integrity-check iterate (language, index) pairs, and
		// rebuild re-scans %_content flushing at every language change.
		{"optimize, rebuild and integrity-check are per language", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, languageid=lid)`,
			`INSERT INTO t(a,lid) VALUES('alpha one', 3)`,
			`INSERT INTO t(a,lid) VALUES('alpha two', 3)`,
			`INSERT INTO t(a,lid) VALUES('beta one', 4)`,
			`INSERT INTO t(a,lid) VALUES('beta two', 4)`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`INSERT INTO t(t) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid FROM t WHERE t MATCH 'alpha' AND lid=3`,
			`SELECT docid FROM t WHERE t MATCH 'beta' AND lid=4`,
			`INSERT INTO t(t) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`SELECT docid FROM t WHERE t MATCH 'beta' AND lid=4`,
		}},
		// A DELETE writes its markers under the language the ROW was indexed
		// under, and emptying the table still wipes everything.
		{"DELETE writes markers under the row's own language", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, languageid=lid)`,
			`INSERT INTO t(a,lid) VALUES('alpha', 2)`,
			`INSERT INTO t(a,lid) VALUES('beta', 2)`,
			`INSERT INTO t(a,lid) VALUES('gamma', 0)`,
			`DELETE FROM t WHERE docid=1`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid FROM t WHERE t MATCH 'alpha' AND lid=2`,
			`SELECT docid FROM t WHERE t MATCH 'beta' AND lid=2`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`UPDATE t SET a='delta' WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid, a, lid FROM t ORDER BY docid`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
			`DELETE FROM t`,
			`SELECT count(*) FROM t_segdir`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		}},
		// A transaction accumulates one segment per language: the accumulating
		// one is SEALED when the next statement's language differs, which is
		// fts3PendingTermsDocid's "p->iPrevLangid != iLangid" flush point.
		{"a language change seals the transaction's segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x, languageid=l)`,
			`BEGIN`,
			`INSERT INTO t(x,l) VALUES('one',3)`,
			`INSERT INTO t(x,l) VALUES('two',3)`,
			`INSERT INTO t(x,l) VALUES('three',4)`,
			`INSERT INTO t(x,l) VALUES('four',3)`,
			`COMMIT`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT docid FROM t WHERE t MATCH 'one' AND l=3`,
			`SELECT docid FROM t WHERE t MATCH 'four' AND l=3`,
			`SELECT docid FROM t WHERE t MATCH 'three' AND l=4`,
			`INSERT INTO t(t) VALUES('integrity-check')`,
		}},
		// The names that make the CREATE fail, because all five go into one
		// sqlite3_declare_vtab; the repeat that is last-one-wins; and the one
		// that names no column at all, which is fine.
		{"the names that collide, and the one that does not", []string{
			`CREATE VIRTUAL TABLE tA USING fts4(a, languageid=a)`,
			`CREATE VIRTUAL TABLE tB USING fts4(a, languageid=docid)`,
			`CREATE VIRTUAL TABLE tC USING fts4(a, languageid=tC)`,
			`CREATE VIRTUAL TABLE tD USING fts4(a, languageid=x, languageid=y)`,
			`CREATE VIRTUAL TABLE tE USING fts4(a, languageid="quoted")`,
			`CREATE VIRTUAL TABLE tF USING fts4(a, languageid=nosuchcol)`,
			// fts3IsSpecialColumn dequotes but does NOT trim, so a leading
			// space is part of the name (a trailing one was already stripped
			// from the whole argument).
			`CREATE VIRTUAL TABLE tG USING fts4(a, languageid= spaced)`,
			`CREATE VIRTUAL TABLE tH USING fts4(a, languageid=trailing )`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`INSERT INTO tD(a,y) VALUES('q',3)`,
			`SELECT docid, a, y FROM tD`,
			`INSERT INTO tE(a,quoted) VALUES('r',4)`,
			`SELECT docid, a, quoted FROM tE`,
			`INSERT INTO tF(a,nosuchcol) VALUES('s',5)`,
			`SELECT docid, a, nosuchcol FROM tF`,
			`INSERT INTO tG(a," spaced") VALUES('t',6)`,
			`SELECT docid, a, " spaced" FROM tG`,
			`SELECT level FROM tG_segdir`,
			`INSERT INTO tH(a,trailing) VALUES('u',7)`,
			`SELECT level FROM tH_segdir`,
			// fts3 has no module options at all, so the same argument declares
			// a column named "languageid".
			`CREATE VIRTUAL TABLE t3 USING fts3(a, languageid=lid)`,
			`SELECT sql FROM sqlite_master WHERE name='t3_content'`,
			`INSERT INTO t3 VALUES('one','two')`,
			`SELECT docid FROM t3 WHERE t3 MATCH 'two'`,
		}},
		// fts4aux reads LANGUAGE 0's term index and nothing else.
		{"fts4aux sees language 0 only", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b, languageid=l)`,
			`INSERT INTO t(a,b,l) VALUES('one two','three',0)`,
			`INSERT INTO t(a,b,l) VALUES('four','five',1)`,
			`CREATE VIRTUAL TABLE taux USING fts4aux(t)`,
			`SELECT term, col, documents, occurrences FROM taux`,
			`SELECT term, documents, occurrences FROM taux WHERE col='*'`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// ftsLangidInterchangeWrite builds a multi-language fts4 table, and
// ftsLangidInterchangeRead reads back everything about it that could differ:
// the rows, both languages' MATCH answers, and every shadow table's bytes.
var ftsLangidInterchangeWrite = []string{
	`CREATE VIRTUAL TABLE t USING fts4(a, b, languageid=lid, prefix=2)`,
	`INSERT INTO t(a,b,lid) VALUES('hello world','one',0)`,
	`INSERT INTO t(a,b,lid) VALUES('hello there','two',1)`,
	`INSERT INTO t(a,b,lid) VALUES('cruel world','three',1)`,
	`INSERT INTO t(docid,a,b,lid) VALUES(200,'bb cc','four',5)`,
}

var ftsLangidInterchangeRead = []string{
	`SELECT docid, a, b, lid FROM t ORDER BY docid`,
	`SELECT docid FROM t WHERE t MATCH 'hello' ORDER BY docid`,
	`SELECT docid FROM t WHERE t MATCH 'hello' AND lid=1 ORDER BY docid`,
	`SELECT docid FROM t WHERE t MATCH 'world' AND lid=1 ORDER BY docid`,
	`SELECT docid FROM t WHERE t MATCH 'bb' AND lid=5 ORDER BY docid`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts3LanguageidFileInterchange writes a multi-language fts4 database with
// each engine and reads it back with both. MATCH agreement alone would not
// prove the format right -- this engine answers a non-MATCH query from
// %_content, which is trivially portable -- so the segment blobs and their
// LEVELS are compared directly, in both directions.
func TestFts3LanguageidFileInterchange(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "langid.db")
			runWithDSN(t, writer, dsn, ftsLangidInterchangeWrite)
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, ftsLangidInterchangeRead))
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

// TestFts3LanguageidIsSearchableByCSQLite is the sharpest form of the claim:
// this engine writes the database and C SQLite, which never saw it being
// written, searches each LANGUAGE's index through the levels it found on disk.
// A language written to the wrong level block would answer no rows here, and a
// malformed segment would fail integrity_check.
func TestFts3LanguageidIsSearchableByCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "langid.db")
	runWithDSN(t, "musql", dsn, ftsLangidInterchangeWrite)

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
		// Language 0 alone, because the query names no language.
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'hello' ORDER BY docid)`, "1"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'hello' AND lid=1 ORDER BY docid)`, "2"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'world' AND lid=1 ORDER BY docid)`, "3"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'bb' AND lid=5 ORDER BY docid)`, "200"},
		// The prefix index of language 1, at its own level block ('there' in
		// docid 2's column a and 'three' in docid 3's column b).
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'th*' AND lid=1 ORDER BY docid)`, "2,3"},
	}
	for _, c := range cases {
		var got sql.NullString
		if err := sdb.QueryRow(c.query).Scan(&got); err != nil {
			t.Errorf("cgo %s: %v", c.query, err)
			continue
		}
		if got.String != c.want {
			t.Errorf("cgo %s = %q, want %q (this engine wrote the index)", c.query, got.String, c.want)
		}
	}

	// fts3's own integrity-check re-tokenizes %_content and compares it against
	// the index, per language -- the check this engine's writes have to survive
	// when run by the OTHER engine.
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Errorf("cgo integrity-check on a database this engine wrote: %v", err)
	}
}
