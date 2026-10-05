// Test multi-row INSERT into fts4 with languageid where rows have different language ids.
// Verify shadow table structure and fts4aux query behavior with language constraints.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// ftsLangidSplitDump returns queries to verify shadow tables and integrity.
func ftsLangidSplitDump(tbl, cols string) []string {
	return []string{
		`SELECT docid, ` + cols + ` FROM ` + tbl + ` ORDER BY docid`,
		`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM ` + tbl + `_segdir ORDER BY level, idx`,
		`SELECT blockid, quote(block) FROM ` + tbl + `_segments ORDER BY blockid`,
		`SELECT docid, quote(size) FROM ` + tbl + `_docsize ORDER BY docid`,
		`SELECT id, quote(value) FROM ` + tbl + `_stat ORDER BY id`,
		`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('integrity-check')`,
	}
}

func TestFtsLangidSplitMultiRowInsertDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
		dump  []string
	}{
		// Three rows with different language ids and fts4aux queries.
		{"fts3aux2.test verbatim (1.1-1.4.6)", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a, b, languageid=l)`,
			`INSERT INTO t1(a, b, l) VALUES ('zero zero', 'zero zero', 0), ('one two', 'three four', 1), ('five six', 'seven eight', 2)`,
			`CREATE VIRTUAL TABLE terms USING fts4aux(t1)`,
		}, []string{
			`SELECT term, documents, occurrences, languageid FROM terms WHERE col = '*'`,
			`SELECT * FROM terms`,
			`SELECT * FROM terms WHERE languageid=''`,
			`SELECT * FROM terms WHERE languageid=-1`,
			`SELECT * FROM terms WHERE languageid=9223372036854775807`,
			`SELECT * FROM terms WHERE languageid=-9223372036854775808`,
			`SELECT * FROM terms WHERE languageid=NULL`,
			`SELECT term, documents, occurrences, languageid FROM terms WHERE col = '*' AND languageid=1`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid=1`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid=1 AND term='zero'`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid='1' AND term='two'`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid='+1' AND term>'four'`,
			`SELECT term, documents, occurrences, languageid FROM terms WHERE col = '*' AND languageid=2`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid=2`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid=2 AND term='five'`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE term='five' AND languageid=2`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE term>='seven' AND languageid=2`,
			`SELECT term, col, documents, occurrences, languageid FROM terms WHERE term>='e' AND term<'seven' AND languageid=2`,
		}},

		// Language 0 reappears later; should flush separately with prefix indexes.
		{"a language reused in a later, separate run, with a prefix index", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a, languageid=l, prefix="2")`,
			`INSERT INTO t1(a, l) VALUES ('alpha beta', 0), ('gamma delta', 1), ('epsilon zeta', 0), ('eta theta', 2)`,
		}, append([]string{
			`SELECT docid FROM t1 WHERE t1 MATCH 'al*' AND l=0`,
			`SELECT docid FROM t1 WHERE t1 MATCH 'ep*' AND l=0`,
			`SELECT docid FROM t1 WHERE t1 MATCH 'ga*' AND l=1`,
			`SELECT docid FROM t1 WHERE t1 MATCH 'et*' AND l=2`,
		}, ftsLangidSplitDump("t1", "a, l")...)},

		// Auto-assigned docid counts across language splits.
		{"auto-assigned docid keeps counting across a language split", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a, languageid=l)`,
			`INSERT INTO t1(a, l) VALUES ('one', 5), ('two', 5), ('three', 9)`,
		}, ftsLangidSplitDump("t1", "a, l")},

		// Final language folds into transaction accumulator; mid-statement runs seal separately.
		{"a language split inside an explicit transaction", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a, languageid=l)`,
			`BEGIN`,
			`INSERT INTO t1(a, l) VALUES ('un', 3)`,
			`INSERT INTO t1(a, l) VALUES ('deux', 3), ('trois', 4), ('quatre', 3)`,
			`INSERT INTO t1(a, l) VALUES ('cinq', 3)`,
			`COMMIT`,
		}, append([]string{
			`SELECT docid FROM t1 WHERE t1 MATCH 'un' AND l=3`,
			`SELECT docid FROM t1 WHERE t1 MATCH 'quatre' AND l=3`,
			`SELECT docid FROM t1 WHERE t1 MATCH 'cinq' AND l=3`,
			`SELECT docid FROM t1 WHERE t1 MATCH 'trois' AND l=4`,
		}, ftsLangidSplitDump("t1", "a, l")...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, append(append([]string{}, c.stmts...), c.dump...), len(c.stmts))
		})
	}
}

// TestFts4auxLangidConstraintDiff tests fts4aux language constraints with prefix indexes.
func TestFts4auxLangidConstraintDiff(t *testing.T) {
	stmts := []string{
		`CREATE VIRTUAL TABLE t1 USING fts4(a, languageid=l, prefix="2,3")`,
		`INSERT INTO t1(a, l) VALUES ('alpha ant', 0)`,
		`INSERT INTO t1(a, l) VALUES ('bravo bear', 1)`,
		`INSERT INTO t1(a, l) VALUES ('charlie cat', 4)`,
		`CREATE VIRTUAL TABLE terms USING fts4aux(t1)`,
	}
	dump := []string{
		// Language 4 with prefix indexes.
		`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid=4`,
		`SELECT term FROM terms WHERE languageid=4 AND term LIKE 'ch*' ESCAPE '\'`,
		// Language with no rows and TEXT-literal constraints.
		`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid=2`,
		`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid='1'`,
		`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid='x'`,
		// Language 0 and all results.
		`SELECT term, col, documents, occurrences, languageid FROM terms WHERE languageid=0`,
		`SELECT term, col, documents, occurrences, languageid FROM terms`,
		`INSERT INTO t1(t1) VALUES('integrity-check')`,
	}
	differAllAccepted(t, "fts4aux languageid constraint, prefix index + empty language + TEXT literal", append(append([]string{}, stmts...), dump...), len(stmts))
}

// ftsLangidSplitInterchangeWrite is a multi-row, multi-language INSERT for interchange testing.
var ftsLangidSplitInterchangeWrite = []string{
	`CREATE VIRTUAL TABLE t1 USING fts4(a, b, languageid=l, prefix=2)`,
	`INSERT INTO t1(a, b, l) VALUES ('zero zero', 'zero zero', 0), ('one two', 'three four', 1), ('five six', 'seven eight', 2), ('nine ten', 'more zero text', 0)`,
}

// TestFtsLangidSplitIsSearchableByCSQLite verifies C SQLite can search a file written by musql.
func TestFtsLangidSplitIsSearchableByCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "langidsplit.db")
	runWithDSN(t, "musql", dsn, ftsLangidSplitInterchangeWrite)

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
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t1 WHERE t1 MATCH 'zero' ORDER BY docid)`, "1,4"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t1 WHERE t1 MATCH 'zero' AND l=0 ORDER BY docid)`, "1,4"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t1 WHERE t1 MATCH 'one' AND l=1 ORDER BY docid)`, "2"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t1 WHERE t1 MATCH 'five' AND l=2 ORDER BY docid)`, "3"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t1 WHERE t1 MATCH 'ni*' AND l=0 ORDER BY docid)`, "4"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t1 WHERE t1 MATCH 'ze*' AND l=0 ORDER BY docid)`, "1,4"},
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

	// Run fts3's integrity check.
	if _, err := sdb.Exec(`INSERT INTO t1(t1) VALUES('integrity-check')`); err != nil {
		t.Errorf("cgo integrity-check on a database this engine wrote: %v", err)
	}
}

// TestFtsBaseTableLangidNumericAffinity tests base fts4 table languageid hidden column affinity.
func TestFtsBaseTableLangidNumericAffinity(t *testing.T) {
	differ(t, "base table languageid TEXT-literal equality", []string{
		`CREATE VIRTUAL TABLE t1 USING fts4(a, b, languageid=l)`,
		`INSERT INTO t1(docid, a, b, l) VALUES(5, 'x', 'y', 0)`,
		`SELECT count(*) FROM t1 WHERE l='0'`,
		`SELECT count(*) FROM t1 WHERE l=0`,
		`SELECT count(*) FROM t1 WHERE l='1'`,
	})
}
