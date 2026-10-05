//go:build sqlite_fts5

// Gate for fts5 answers and index interchange with C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// fts5Exec runs stmts against dsn through driver drv, returning the first
// error (with its statement) if any.
func fts5Exec(t *testing.T, drv, dsn string, stmts []string) error {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %q: %w", drv, s, err)
		}
	}
	return nil
}

// fts5Query runs one statement and renders its result as a single string, in
// the same "storage class tagged" spirit as the rest of this harness so a
// TEXT '1' can never compare equal to an INTEGER 1.
func fts5Query(t *testing.T, drv, dsn, stmt string) (string, error) {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	rows, err := db.Query(stmt)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(strings.Join(cols, "|"))
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		b.WriteString("\n")
		for i, c := range cells {
			if i > 0 {
				b.WriteString("|")
			}
			b.WriteString(tclNormalizeCGOCell(c))
		}
	}
	return b.String(), rows.Err()
}

func TestFts5AnswerDiff(t *testing.T) {
	cases := []struct {
		name   string
		setup  []string
		probes []string
		// allowDecline marks a group whose shapes this engine currently
		// DECLINES: a decline is a legitimate outcome under this project's
		// rules, so it is logged rather than failed -- but a decline that ever
		// turns into an ANSWER is still compared, so it can never turn into a
		// wrong one unnoticed.
		allowDecline bool
	}{
		{
			name: "MATCH placement in a join",
			setup: []string{
				`CREATE TABLE t1(a, b, rank)`,
				`INSERT INTO t1 VALUES('a', 'hello', '')`,
				`INSERT INTO t1 VALUES('b', 'world', '')`,
				`CREATE VIRTUAL TABLE ft USING fts5(a)`,
				`INSERT INTO ft VALUES('b')`,
				`INSERT INTO ft VALUES('y')`,
			},
			probes: []string{
				// fts5misc.test's ticket [7c0e06b16] case: with the fts5 table
				// written SECOND, the MATCH used to be tested before its
				// cursor was positioned, so an empty document matched nothing
				// and the query silently returned no rows.
				`SELECT * FROM t1 NATURAL JOIN ft WHERE ft MATCH('b')`,
				`SELECT * FROM ft NATURAL JOIN t1 WHERE ft MATCH('b')`,
				`SELECT t1.a, ft.a FROM t1, ft WHERE ft MATCH 'b' ORDER BY t1.a`,
				`SELECT t1.a, ft.a FROM t1 JOIN ft ON t1.a=ft.a WHERE ft MATCH 'b'`,
				`SELECT count(*) FROM t1, ft WHERE ft MATCH 'nosuchterm'`,
			},
		},
		{
			name: "core MATCH forms",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('one two three', 'alpha beta')`,
				`INSERT INTO t VALUES('two four', 'beta gamma')`,
				`INSERT INTO t VALUES('five', 'delta')`,
			},
			probes: []string{
				`SELECT rowid, a, b FROM t ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'two'`,
				`SELECT rowid FROM t WHERE t MATCH 'two AND beta'`,
				`SELECT rowid FROM t WHERE t MATCH 'four OR delta'`,
				`SELECT rowid FROM t WHERE t MATCH 'two NOT four'`,
				`SELECT rowid FROM t WHERE t MATCH '"one two"'`,
				`SELECT rowid FROM t WHERE t MATCH 'a:two'`,
				`SELECT rowid FROM t WHERE t MATCH 'b:two'`,
				`SELECT rowid FROM t WHERE t MATCH 'thr*'`,
				`SELECT rowid FROM t WHERE t MATCH 'NEAR(one three, 2)'`,
				`SELECT rowid FROM t WHERE t MATCH 'NEAR(one three, 0)'`,
				`SELECT count(*) FROM t WHERE t MATCH 'nosuchterm'`,
			},
		},
		{
			// engine/fts5_aux.go and fts5_snippet.go implement bm25(),
			// snippet(), highlight() and the "rank" hidden column; they are
			// wired into the VDBE compiler by engine/fts5_vdbe_aux.go (see
			// that file's package comment for the rules pinned against this
			// oracle, and compat-harness/fts5_aux_test.go for the broader
			// differential coverage: multiple weights, OR/NOT/NEAR queries,
			// no-MATCH behavior, and every decline boundary).
			name: "auxiliary functions and rank",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('the quick brown fox', 'jumps over')`,
				`INSERT INTO t VALUES('quick quick', 'fox fox fox')`,
			},
			probes: []string{
				`SELECT rowid, highlight(t, 0, '[', ']') FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid, snippet(t, 0, '<', '>', '...', 4) FROM t WHERE t MATCH 'brown' ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'quick' ORDER BY rank`,
				`SELECT rowid, round(bm25(t), 6) FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
			},
		},
		{
			// fts5's command channel. 'optimize' and 'rebuild' both mean "make
			// the index what the content implies", which is this engine's
			// invariant after every write, so both are answered; the rest are
			// declined by name (engine/fts5_shadow.go's fts5CommandInsert).
			name: "the command channel",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t(rowid,a,b) VALUES(1,'one two','x y')`,
				`INSERT INTO t(rowid,a,b) VALUES(2,'two three','y z')`,
				`INSERT INTO t(rowid,a,b) VALUES(3,'three four','z w')`,
				`INSERT INTO t(t) VALUES('optimize')`,
				`DELETE FROM t WHERE rowid=2`,
				`INSERT INTO t(t) VALUES('rebuild')`,
			},
			probes: []string{
				`SELECT rowid, a, b FROM t ORDER BY rowid`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'two' ORDER BY rowid)`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'three' ORDER BY rowid)`,
				`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
				`SELECT k, quote(v) FROM t_config ORDER BY k`,
			},
		},
		{
			// The shared insertIntoVtab -> vtabInsertRowValues path; rtree
			// gates it in the default build too (rtree_diff_test.go's
			// TestVtabInsertSelect). What is fts5-specific here is that the
			// inserted rows must also be INDEXED, which the MATCH probes check.
			name: "INSERT ... SELECT",
			setup: []string{
				`CREATE TABLE src(x, y)`,
				`INSERT INTO src VALUES('hello world','alpha'),('goodbye world','beta'),('three','gamma')`,
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t SELECT x, y FROM src WHERE x<>'three'`,
				`INSERT INTO t(b, a) SELECT y, x FROM src WHERE x='three'`,
				`INSERT INTO t(rowid, a, b) SELECT rowid+100, upper(x), y FROM src ORDER BY x DESC LIMIT 2`,
				`INSERT INTO t SELECT a, b FROM t WHERE rowid=1`,
			},
			probes: []string{
				`SELECT rowid, a, b FROM t ORDER BY rowid`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'world' ORDER BY rowid)`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'gamma' ORDER BY rowid)`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'GOODBYE' ORDER BY rowid)`,
				`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
			},
		},
		{
			name: "writes and rowid handling",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t(rowid, a) VALUES(7, 'seven text')`,
				`INSERT INTO t(a) VALUES('eight text')`,
				`UPDATE t SET a='seven changed' WHERE rowid=7`,
				`DELETE FROM t WHERE rowid=8`,
				`INSERT INTO t(a) VALUES('ninth text')`,
			},
			probes: []string{
				`SELECT rowid, a FROM t ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'changed'`,
				`SELECT rowid FROM t WHERE t MATCH 'eight'`,
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], c.setup); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			for _, p := range c.probes {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], p)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], p)
				switch {
				case goErr != nil && cgoErr != nil:
					// Both decline: agreement, not a divergence.
				case goErr != nil && c.allowDecline:
					t.Logf("declined (tracked, not wrong): %s\n  err: %v", p, goErr)
				case goErr != nil:
					t.Errorf("%s: this engine declined a query C fts5 answers\n  sql: %s\n  err: %v\n  cgo: %s", c.name, p, goErr, cgoOut)
				case cgoErr != nil:
					t.Errorf("%s: this engine ACCEPTED a query C fts5 rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", c.name, p, goOut, cgoErr)
				case goOut != cgoOut:
					t.Errorf("%s DIVERGES from C fts5\n  sql: %s\n  go:  %q\n  cgo: %q", c.name, p, goOut, cgoOut)
				}
			}
		})
	}
}

// fts5ShadowDump is every shadow table's raw content, plus the schema rows the
// five of them add. Those bytes ARE the file format: an engine that writes
// them differently writes a database C SQLite reads differently.
//
// Both dumps are taken through the CGo driver, over the two engines' FILES --
// not through each engine's own reader. That is the claim being made here, and
// it is also the only way to read %_data at all from this side: this engine
// DECLINES a query against %_data/%_idx, because their row set is its segment
// layout rather than its postings (engine/fts5_shadow.go's
// fts5SegmentShadowQueryGuard). The single-statement tables below are exactly
// the case where the two layouts agree, which is why the comparison is scoped
// to them.
var fts5ShadowDump = []string{
	`SELECT id, quote(block) FROM t_data ORDER BY id`,
	`SELECT quote(segid), quote(term), quote(pgno) FROM t_idx ORDER BY segid, term`,
	`SELECT * FROM t_content ORDER BY id`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT k, v FROM t_config`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts5ShadowLayoutDiff pins the bytes themselves for the case where the
// two engines agree exactly: a table built by ONE statement. Real fts5 appends
// a segment per statement and merges them later; this engine re-encodes the
// whole index from the live rows each time (engine/fts5_index.go), so after
// several statements the two hold the same postings in a different number of
// segments -- a legal difference, and the reason the byte comparison is scoped
// here and the behavioural one below is not.
func TestFts5ShadowLayoutDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"empty table", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		}},
		{"one row, two columns", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
		}},
		{"one statement, several rowids, shared terms", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a)`,
			`INSERT INTO t(rowid,a) VALUES(1,'x y'),(2,'x'),(5,'y x'),(100,'x')`,
		}},
		{"a negative rowid", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(-3,'neg','x')`,
		}},
		{"non-text column values are indexed through their text rendering", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1, 123, NULL), (2, 4.5, x'6869')`,
		}},
		{"a multi-page segment, split mid-doclist", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a)`,
			fts5BulkInsert(`INSERT INTO t(rowid,a) VALUES`, 3000, func(i int) string {
				return fmt.Sprintf("(%d,'word%04d qqq')", i+1, i)
			}),
		}},
		{"a multi-page segment over two columns", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 3000, func(i int) string {
				return fmt.Sprintf("(%d,'qqq word%04d','col %d')", i+1, i, i)
			}),
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], c.stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5ShadowDump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES\n  sql: %s\n%s", c.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// fts5BulkInsert builds one INSERT with n rows, so the whole table lands in a
// single segment on both engines.
func fts5BulkInsert(prefix string, n int, row func(i int) string) string {
	vals := make([]string, n)
	for i := range vals {
		vals[i] = row(i)
	}
	return prefix + strings.Join(vals, ",")
}

// fts5DiffLines reports the first differing line and byte offset, because the
// blobs compared above run to thousands of hex digits.
func fts5DiffLines(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] == w[i] {
			continue
		}
		j := 0
		for j < len(g[i]) && j < len(w[i]) && g[i][j] == w[i][j] {
			j++
		}
		lo := j - 32
		if lo < 0 {
			lo = 0
		}
		win := func(s string) string {
			hi := j + 32
			if hi > len(s) {
				hi = len(s)
			}
			return s[lo:hi]
		}
		return fmt.Sprintf("  line %d differs at byte %d (len go=%d cgo=%d)\n  go:  ...%s...\n  cgo: ...%s...", i, j, len(g[i]), len(w[i]), win(g[i]), win(w[i]))
	}
	return fmt.Sprintf("  line counts differ: go=%d cgo=%d\n  go:  %q\n  cgo: %q", len(g), len(w), got, want)
}

// fts5InterchangeCases are databases whose SHAPE the byte comparison above
// cannot cover -- several statements, deletes, updates -- but whose meaning
// both engines must still agree on completely.
var fts5InterchangeCases = []struct {
	name  string
	stmts []string
}{
	{"one row per statement", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
		`INSERT INTO t(a,b) VALUES('second row','baz')`,
		`INSERT INTO t(rowid,a,b) VALUES(9,'hello again','foo')`,
	}},
	{"deletes leave no trace of the removed rows", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 500, func(i int) string {
			return fmt.Sprintf("(%d,'alpha%d beta shared','gamma %d')", i+1, i, i)
		}),
		`DELETE FROM t WHERE rowid % 3 = 0`,
	}},
	{"updates rewrite a row's postings", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 500, func(i int) string {
			return fmt.Sprintf("(%d,'alpha%d beta shared','gamma %d')", i+1, i, i)
		}),
		`UPDATE t SET a='rewritten text here' WHERE rowid % 5 = 0`,
	}},
	{"emptying the table leaves an empty index", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
		`DELETE FROM t`,
	}},
	{"folded and multi-byte text", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(rowid,a) VALUES(1,'café NAÏVE Ünïcödé'),(2,'ÀÉÎÕÜ mixed'),(3,'日本語 テスト')`,
	}},
	{"optimize and rebuild leave a searchable index", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 500, func(i int) string {
			return fmt.Sprintf("(%d,'alpha%d beta shared','gamma %d')", i+1, i, i)
		}),
		`DELETE FROM t WHERE rowid % 7 = 0`,
		`INSERT INTO t(t) VALUES('optimize')`,
		`INSERT INTO t(rowid,a,b) VALUES(9001,'hello world','beta')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
}

// fts5InterchangeProbes are answered identically by both engines over either
// engine's file. Every one of them goes THROUGH the inverted index on the
// real-SQLite side, so a segment blob that decoded to anything but exactly the
// right postings shows up here as a missing or extra row.
var fts5InterchangeProbes = []string{
	`SELECT count(*) FROM t`,
	`SELECT group_concat(rowid) FROM (SELECT rowid FROM t ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'beta' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha1*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'beta AND shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '"hello world"' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'cafe' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'rewritten' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'nosuchterm')`,
	// The three shadow tables whose contents do NOT depend on the segment
	// layout, so both engines' readers must agree on them over either file.
	// (%_data and %_idx are the two that do -- see fts5ShadowDump.)
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts5FileInterchange is the load-bearing claim, and the inverse of what
// this file used to pin: an fts5 database is now interchangeable in BOTH
// directions. Whichever engine wrote it, both must give the same answers to
// every probe -- and C SQLite, which never saw the file being written,
// must also report integrity_check "ok", which is what checks the index
// against the content rather than merely parsing it.
func TestFts5FileInterchange(t *testing.T) {
	for _, c := range fts5InterchangeCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			for _, writer := range []string{"sqlite", "sqlite3"} {
				dsn := filepath.Join(t.TempDir(), "fts5.db")
				if err := fts5Exec(t, writer, dsn, c.stmts); err != nil {
					t.Fatalf("%s writes: %v", writer, err)
				}
				// Each engine reads its OWN format, with the converter in between where
				// the writer was the other one (convert_for_oracle_test.go).
				goPath, cgoPath := pathsForBothEngines(t, writer, dsn)

				for _, q := range fts5InterchangeProbes {
					goOut, goErr := fts5Query(t, "sqlite", goPath, q)
					cgoOut, cgoErr := fts5Query(t, "sqlite3", cgoPath, q)
					switch {
					case goErr != nil:
						t.Errorf("[%s wrote] this engine cannot read it back\n  sql: %s\n  err: %v", writer, q, goErr)
					case cgoErr != nil:
						t.Errorf("[%s wrote] C SQLite cannot read it\n  sql: %s\n  err: %v", writer, q, cgoErr)
					case goOut != cgoOut:
						t.Errorf("[%s wrote] the two engines read it differently\n  sql: %s\n  go:  %q\n  cgo: %q", writer, q, goOut, cgoOut)
					}
				}
				// fts5's own integrity check recomputes the index checksum
				// from %_content and compares it with the one it derives by
				// walking the segments -- so this fails on any postings,
				// position or %_docsize error, including ones every query
				// above happens to miss.
				ic, err := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), `PRAGMA integrity_check`)
				if err != nil {
					t.Fatalf("[%s wrote] integrity_check: %v", writer, err)
				}
				// "T:" is fts5Query's storage-class tag for TEXT.
				if ic != "integrity_check\nT:ok" {
					t.Errorf("[%s wrote] C SQLite reports integrity_check = %q", writer, ic)
				}
			}
		})
	}
}

