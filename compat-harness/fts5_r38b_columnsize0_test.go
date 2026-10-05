//go:build sqlite_fts5

// Tests fts5 CONTENTLESS tables with columnsize=0. Such a table has no
// separate content or docsize storage, so token counts must be recovered from
// postings. Also tests that queries without a MATCH clause are properly rejected.
// All tests verify agreement with the C SQLite oracle.
package compat

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// r38bSchemas varies the column count, UNINDEXED columns, prefix indexes, and tokenizer.
var r38bSchemas = []struct {
	name   string
	create string
	nCol   int
	// scans is whether contentless_unindexed=1 with an UNINDEXED column can scan.
	scans bool
}{
	{"one column", `CREATE VIRTUAL TABLE t USING fts5(a, columnsize=0, content='')`, 1, false},
	{"two columns", `CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0, content='')`, 2, false},
	{"three columns", `CREATE VIRTUAL TABLE t USING fts5(a, b, c, columnsize=0, content='')`, 3, false},
	{"content first", `CREATE VIRTUAL TABLE t USING fts5(content='', a, b, columnsize=0)`, 2, false},
	{"quoted zero", `CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize='0', content='')`, 2, false},
	{"prefix", `CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0, content='', prefix='1 3')`, 2, false},
	{"ascii", `CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0, content='', tokenize=ascii)`, 2, false},
	{"trigram", `CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0, content='', tokenize=trigram)`, 2, false},
	{"unindexed middle", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, c, columnsize=0, content='')`, 3, false},
	// contentless_unindexed=1 with NO unindexed column stays FTS5_CONTENT_NONE.
	{"contentless_unindexed=1 no UNINDEXED col", `CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0, content='', contentless_unindexed=1)`, 2, false},
	{"contentless_unindexed=1", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, c, columnsize=0, content='', contentless_unindexed=1)`, 3, true},
}

// r38bRows is test data with empty and NULL columns, repeated and shared terms, and non-contiguous rowids.
var r38bRows = []struct {
	rowid int
	vals  []string
}{
	{1, []string{"'shared alpha alpha'", "'shared beta'", "'gamma shared'"}},
	{2, []string{"'alpha only'", "'nothing here'", "''"}},
	{3, []string{"'zulu'", "'zulu zulu zulu'", "NULL"}},
	{5, []string{"'lonely'", "''", "'delta delta'"}},
	{9, []string{"'shared'", "'alpha beta gamma'", "'delta'"}},
}

func r38bFillOne(nCol int) string {
	cols := []string{"a", "b", "c"}[:nCol]
	var vals []string
	for _, r := range r38bRows {
		vals = append(vals, fmt.Sprintf("(%d,%s)", r.rowid, strings.Join(r.vals[:nCol], ",")))
	}
	return fmt.Sprintf("INSERT INTO t(rowid,%s) VALUES%s", strings.Join(cols, ","), strings.Join(vals, ","))
}

// r38bFillMany is the same rows as SEPARATE statements. Real fts5 appends a
// segment per statement, so the file the oracle writes this way holds several --
// the shape the reader has to merge and the one this engine never produces.
func r38bFillMany(nCol int) []string {
	cols := []string{"a", "b", "c"}[:nCol]
	var out []string
	for _, r := range r38bRows {
		out = append(out, fmt.Sprintf("INSERT INTO t(rowid,%s) VALUES(%d,%s)",
			strings.Join(cols, ","), r.rowid, strings.Join(r.vals[:nCol], ",")))
	}
	return out
}

// r38bShadowDump has no %_docsize line: its ABSENCE is carried by the
// sqlite_master dump, which is exactly what this schema is about.
var r38bShadowDump = []string{
	`SELECT id, quote(block) FROM t_data ORDER BY id`,
	`SELECT quote(segid), quote(term), quote(pgno) FROM t_idx ORDER BY segid, term`,
	`SELECT k, v FROM t_config`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestR38BColumnsize0ShadowBytes: for a table built by ONE statement the two
// engines' shadow tables must be byte-identical, including the absence of both
// %_content and %_docsize.
func TestR38BColumnsize0ShadowBytes(t *testing.T) {
	for _, sc := range r38bSchemas {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			stmts := []string{sc.create, r38bFillOne(sc.nCol)}
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range r38bShadowDump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES\n  sql: %s\n%s", sc.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// r38bProbes are read back over the SAME file by both engines. Every one of
// them carries a MATCH, because a query without one is an ERROR on such a table
// -- that half is r38bScanProbes.
var r38bProbes = []string{
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'zulu' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'delta' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alph*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared AND alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared NOT alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '"shared alpha"' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'NEAR(shared alpha, 1)' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '^alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'nosuchterm')`,
	// The columns themselves must all come back NULL for a contentless table
	// (fts5_main.c's fts5ColumnMethod), which only a MATCH can reach here.
	`SELECT ifnull(group_concat(coalesce(quote(a),'X')),'') FROM (SELECT a FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	// The TABLE-VALUED spelling reaches the module by a different route.
	`SELECT ifnull(group_concat(rowid),'') FROM t('alpha')`,
	`SELECT count(*) FROM t('shared')`,
	// bm25 scores off the per-column token counts, which on this table exist
	// ONLY as the postings' offsets -- so a mis-sized column shows up here even
	// when the row set is right.
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rank, rowid)`,
	`SELECT ifnull(group_concat(round(rank,6)),'') FROM (SELECT rank FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(round(r,6)),'') FROM (SELECT bm25(t) AS r FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(quote(h)),'') FROM (SELECT highlight(t,0,'[',']') AS h FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// r38bScanProbes are queries without a MATCH clause that should be refused.
var r38bScanProbes = []string{
	`SELECT rowid FROM t`,
	`SELECT rowid FROM t WHERE rowid=2`,
	`SELECT rowid FROM t WHERE rowid BETWEEN 1 AND 3`,
	`SELECT count(*) FROM t`,
	`SELECT a FROM t ORDER BY rowid`,
	`SELECT count(*) FROM t GROUP BY a`,
	`SELECT rowid FROM (SELECT rowid FROM t)`,
	`SELECT max(rowid) OVER () FROM t`,
	`SELECT rowid FROM t UNION ALL SELECT rowid FROM t`,
}

// r38bPlacementProbes tests MATCH placement when not converted to a constraint.
var r38bPlacementProbes = []string{
	`SELECT rowid FROM t WHERE t MATCH 'alpha' OR rowid=3`,
}

// TestR38BColumnsize0Interchange is the load-bearing test: whichever engine
// wrote the file, both must read the same answers out of it, and C SQLite
// must report integrity_check "ok" over the one this engine wrote.
func TestR38BColumnsize0Interchange(t *testing.T) {
	for _, sc := range r38bSchemas {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			for _, writer := range []string{"sqlite", "sqlite3"} {
				for _, fill := range []string{"one statement", "one per row"} {
					dsn := filepath.Join(t.TempDir(), "fts5.db")
					stmts := []string{sc.create}
					if fill == "one statement" {
						stmts = append(stmts, r38bFillOne(sc.nCol))
					} else {
						stmts = append(stmts, r38bFillMany(sc.nCol)...)
					}
					if err := fts5Exec(t, writer, dsn, stmts); err != nil {
						t.Fatalf("[%s/%s] %s: %v", sc.name, writer, fill, err)
					}
					label := fmt.Sprintf("%s written by %s, %s", sc.name, writer, fill)
					for _, q := range r38bProbes {
						if sc.nCol < 3 && strings.Contains(q, "delta") {
							continue // 'delta' lives in column c
						}
						goOut, goErr := fts5Query(t, "sqlite", musqlPathFor(t, writer, dsn), q)
						cgoOut, cgoErr := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), q)
						if (goErr == nil) != (cgoErr == nil) {
							t.Errorf("[%s] %s accept/reject disagrees\n  this engine: %v\n  cgo:         %v", label, q, goErr, cgoErr)
							continue
						}
						if goErr == nil && goOut != cgoOut {
							t.Errorf("[%s] %s DIVERGES\n%s", label, q, fts5DiffLines(goOut, cgoOut))
						}
					}
					for _, q := range r38bScanProbes {
						goOut, goErr := fts5Query(t, "sqlite", musqlPathFor(t, writer, dsn), q)
						cgoOut, cgoErr := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), q)
						if sc.scans {
							// zContent is NOT null here, so the oracle serves
							// these and so must this engine, byte for byte.
							if cgoErr != nil {
								t.Fatalf("[%s] %s: the ORACLE refused it, but this schema was recorded as scanning (zContent non-NULL): %v", label, q, cgoErr)
							}
							if goErr != nil {
								t.Errorf("[%s] %s: this engine REFUSED a scan C fts5 serves: %v -- fts5ContentlessScanGuard is too wide", label, q, goErr)
							} else if goOut != cgoOut {
								t.Errorf("[%s] %s DIVERGES\n%s", label, q, fts5DiffLines(goOut, cgoOut))
							}
							continue
						}
						if cgoErr == nil {
							t.Fatalf("[%s] %s: the ORACLE served it -- fts5_main.c:1623 was expected to refuse a query with no MATCH over a columnsize=0 contentless table; re-derive the rule before widening this engine's guard", label, q)
						}
						if goErr == nil {
							t.Errorf("[%s] %s: this engine SERVED a scan C fts5 refuses with %q -- fts5ContentlessScanGuard is too narrow", label, q, cgoErr)
						}
					}
					for _, q := range r38bPlacementProbes {
						goOut, goErr := fts5Query(t, "sqlite", musqlPathFor(t, writer, dsn), q)
						cgoOut, cgoErr := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), q)
						if (goErr == nil) != (cgoErr == nil) {
							t.Errorf("[%s] %s accept/reject disagrees\n  this engine: %v\n  cgo:         %v", label, q, goErr, cgoErr)
						} else if goErr == nil && goOut != cgoOut {
							t.Errorf("[%s] %s DIVERGES\n%s", label, q, fts5DiffLines(goOut, cgoOut))
						}
					}
					if out, err := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), `INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
						t.Errorf("[%s] C SQLite's integrity-check over this file: %v (%s)", label, err, out)
					}
				}
			}
		})
	}
}

// TestR38BColumnsize0Corpus tests the sequence of statements for columnsize=0 tables.
func TestR38BColumnsize0Corpus(t *testing.T) {
	flLockstep(t, "fts5columnsize-2", []string{
		`CREATE VIRTUAL TABLE t2 USING fts5(x, columnsize=0, content='')`,
		`INSERT INTO t2(rowid, x) VALUES(1, 'c d e f')`,
		`INSERT INTO t2(rowid, x) VALUES(2, 'c d e f g h')`,
		`INSERT INTO t2(rowid, x) VALUES(3, 'a b c d e f g h')`,
	},
		`SELECT rowid FROM t2 WHERE t2 MATCH 'b'`,
		`SELECT rowid FROM t2 WHERE t2 MATCH 'e' ORDER BY rowid`,
		`SELECT rowid FROM t2 WHERE t2 MATCH 'h' ORDER BY rowid`,
		`SELECT rowid FROM t2 WHERE t2 MATCH '"e f g"' ORDER BY rowid`,
		`SELECT rowid FROM t2`,
		`SELECT rowid FROM t2 WHERE rowid=2`,
		`SELECT rowid FROM t2 WHERE rowid BETWEEN 1 AND 3`,
	)
	// Test with an UNINDEXED column in the middle.
	flLockstep(t, "fts5columnsize-3.2", []string{
		`CREATE VIRTUAL TABLE t4 USING fts5(x, y UNINDEXED, z, columnsize=0, content='')`,
		`INSERT INTO t4(rowid, x, y, z) VALUES(1, 'a a', 'b b b', 'c')`,
		`INSERT INTO t4(rowid, x, y, z) VALUES(2, 'x a x', 'b b b y', '')`,
	},
		`SELECT rowid FROM t4 WHERE t4 MATCH 'a' ORDER BY rowid`,
		`SELECT rowid FROM t4 WHERE t4 MATCH 'b' ORDER BY rowid`,
		`SELECT rowid FROM t4 WHERE t4 MATCH 'c' ORDER BY rowid`,
		`SELECT rowid, round(bm25(t4),6) FROM t4 WHERE t4 MATCH 'a' ORDER BY rowid`,
	)
	// Test the 'delete' command over a columnsize=0 table.
	flLockstep(t, "fts5columnsize-delete", []string{
		`CREATE VIRTUAL TABLE t2 USING fts5(x, columnsize=0, content='')`,
		`INSERT INTO t2(rowid, x) VALUES(1, 'c d e f')`,
		`INSERT INTO t2(rowid, x) VALUES(2, 'c d e f g h')`,
		`INSERT INTO t2(rowid, x) VALUES(3, 'a b c d e f g h')`,
		`INSERT INTO t2(t2, rowid, x) VALUES('delete', 2, 'c d e f g h')`,
	},
		`SELECT rowid FROM t2 WHERE t2 MATCH 'b'`,
		`SELECT rowid FROM t2 WHERE t2 MATCH 'e' ORDER BY rowid`,
		`SELECT rowid FROM t2 WHERE t2 MATCH 'h' ORDER BY rowid`,
	)
}

// TestR38BColumnsize0EmptyDocument tests handling of rows with all indexed columns empty.
// Such rows leave no posting and with no docsize, this engine refuses them rather than
// silently encoding an incomplete index.
func TestR38BColumnsize0EmptyDocument(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fts5.db")
	stmts := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, columnsize=0, content='')`,
		`INSERT INTO t(rowid,a) VALUES(1,'alpha')`,
		`INSERT INTO t(rowid,a) VALUES(2,'')`,
		`INSERT INTO t(rowid,a) VALUES(3,'beta')`,
	}
	if err := fts5Exec(t, "sqlite3", dsn, stmts); err != nil {
		t.Fatalf("the oracle refused the setup: %v", err)
	}
	q := `SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha OR beta' ORDER BY rowid)`
	cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn, q)
	if cgoErr != nil {
		t.Fatalf("the oracle refused %s: %v", q, cgoErr)
	}
	goOut, goErr := fts5Query(t, "sqlite", importedForMusql(t, dsn), q)
	if goErr == nil {
		if goOut != cgoOut {
			t.Errorf("%s DIVERGES\n%s", q, fts5DiffLines(goOut, cgoOut))
		}
		t.Log("this engine now reads a columnsize=0 contentless table holding an all-empty document: drop the refusal branch above")
		return
	}
	if !strings.Contains(goErr.Error(), "averages record") {
		t.Errorf("expected the averages-record refusal for an unrecoverable document, got: %v", goErr)
	}
}
