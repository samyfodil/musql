// Tests FTS3/FTS4 segment spilling: when flush terms outgrow a single node,
// segments spill into %_segments blocks with an interior-node tree over them.
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

// fts3ManyTerms returns n distinct, ascending terms as one space-separated
// document. The zero-padded numbering makes the terms share long prefixes, so
// the leaf encoding's prefix compression and the interior nodes' separator
// terms (a shared prefix plus one byte) are both exercised.
func fts3ManyTerms(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s%06d", prefix, i)
	}
	return b.String()
}

// fts3LongTerms is fts3ManyTerms with a padLen-byte shared prefix, so the
// interior nodes' separator terms are that long too -- see this file's doc
// comment for why that is what buys tree DEPTH cheaply.
func fts3LongTerms(padLen, n int) string {
	return fts3ManyTerms("t"+strings.Repeat("q", padLen-1), n)
}

func TestFts3SegmentSpillLayout(t *testing.T) {
	// The raw shadow-table probes every case ends with.
	dump := []string{
		`SELECT level, idx, start_block, leaves_end_block, end_block, typeof(end_block), length(root), quote(root) FROM t_segdir ORDER BY level, idx`,
		`SELECT blockid, length(block), quote(block) FROM t_segments ORDER BY blockid`,
		`SELECT count(*) FROM t_segments`,
	}
	cases := []struct {
		name  string
		stmts []string
	}{
		{"one leaf still fits in root", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("w", 400) + `')`,
			`SELECT docid FROM t WHERE t MATCH 'w000007'`,
		}},
		{"spills into two leaves", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("w", 700) + `')`,
			`SELECT docid FROM t WHERE t MATCH 'w000000'`,
			`SELECT docid FROM t WHERE t MATCH 'w000699'`,
			`SELECT count(*) FROM t WHERE t MATCH 'w00003*'`,
		}},
		{"spills into many leaves, one interior level", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("term", 5000) + `')`,
			`SELECT docid FROM t WHERE t MATCH 'term000000'`,
			`SELECT docid FROM t WHERE t MATCH 'term004999'`,
			`SELECT count(*) FROM t WHERE t MATCH 'term00019*'`,
		}},
		{"long terms: two interior levels", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('` + fts3LongTerms(200, 9000) + `')`,
			`SELECT count(*) FROM t WHERE t MATCH '` + fts3LongTerms(200, 1) + `'`,
			`SELECT level, idx, start_block, leaves_end_block FROM t_segdir`,
		}},
		{"two columns, both spilling", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("aa", 900) + `','` + fts3ManyTerms("bb", 900) + `')`,
			`SELECT docid FROM t WHERE t MATCH 'aa000010'`,
			`SELECT docid FROM t WHERE t MATCH 'bb000010'`,
			`SELECT docid FROM t WHERE t MATCH 'a:aa000010'`,
			`SELECT count(*) FROM t WHERE t MATCH 'b:aa000010'`,
		}},
		{"a second statement's blocks continue the numbering", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("p", 700) + `')`,
			`INSERT INTO t VALUES('` + fts3ManyTerms("q", 700) + `')`,
			`INSERT INTO t VALUES('small doc')`,
			`SELECT docid FROM t WHERE t MATCH 'p000050'`,
			`SELECT docid FROM t WHERE t MATCH 'q000050'`,
			`SELECT docid FROM t WHERE t MATCH 'small'`,
		}},
		{"multi-row insert spilling, with docsize and stat", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'` + fts3ManyTerms("m", 500) + `'),(2,'` + fts3ManyTerms("n", 500) + `')`,
			`SELECT docid FROM t WHERE t MATCH 'm000079' ORDER BY docid`,
			`SELECT docid FROM t WHERE t MATCH 'n000000' ORDER BY docid`,
			`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
			`SELECT quote(value) FROM t_stat`,
		}},
		{"deleting from a spilled table appends a marker segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'` + fts3ManyTerms("d", 700) + `')`,
			`INSERT INTO t(docid,a) VALUES(2,'keep this row')`,
			`DELETE FROM t WHERE docid=1`,
			`SELECT docid, substr(a,1,8) FROM t ORDER BY docid`,
			`SELECT count(*) FROM t WHERE t MATCH 'd000005'`,
			`SELECT docid FROM t WHERE t MATCH 'keep'`,
		}},
		{"updating a spilled row rewrites into a new segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'` + fts3ManyTerms("u", 700) + `')`,
			`INSERT INTO t(docid,a) VALUES(2,'other')`,
			`UPDATE t SET a='` + fts3ManyTerms("v", 700) + `' WHERE docid=1`,
			`SELECT count(*) FROM t WHERE t MATCH 'u000005'`,
			`SELECT docid FROM t WHERE t MATCH 'v000005'`,
			`SELECT docid, substr(a,1,8) FROM t ORDER BY docid`,
		}},
		{"deleting every row empties %_segments too", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'` + fts3ManyTerms("z", 700) + `')`,
			`DELETE FROM t WHERE docid=1`,
			`SELECT count(*) FROM t`,
			`INSERT INTO t(docid,a) VALUES(1,'` + fts3ManyTerms("z", 700) + `')`,
			`SELECT docid FROM t WHERE t MATCH 'z000005'`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, append(append([]string{}, c.stmts...), dump...))
		})
	}
}

// TestFts3SpillTreeDepth covers the deepest shape: an interior tree with TWO
// levels, so that the level below the root is itself written out to
// %_segments and the root's children are interior nodes rather than leaves.
// That is the part of the writer with real bookkeeping in it -- each interior
// node's leftmost-child block id follows the previous node's by its own entry
// count plus one -- and the case above cannot reach it at the default page
// size for less than several megabytes of text (an interior node's separator
// terms are prefix-compressed against each other too, so hundreds of leaves
// are needed).
//
// It runs at page_size=512, which brings the same shape within ~9000 short
// terms, and therefore compares the two engines IN PROCESS rather than through
// differ(): the "PRAGMA page_size=512" statement itself is answered with a row
// here and with none by C SQLite, which would otherwise be the only thing a
// differ() case reported. Everything after that pragma is compared exactly,
// including every raw block.
func TestFts3SpillTreeDepth(t *testing.T) {
	setup := []string{
		`PRAGMA page_size=512`,
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('` + fts3ManyTerms("w", 9000) + `')`,
		`INSERT INTO t VALUES('one small row')`,
		`INSERT INTO t VALUES('` + fts3ManyTerms("v", 4000) + `')`,
	}
	probes := []string{
		`SELECT level, idx, start_block, leaves_end_block, end_block, typeof(end_block), length(root), quote(root) FROM t_segdir ORDER BY level, idx`,
		`SELECT blockid, length(block), quote(block) FROM t_segments ORDER BY blockid`,
		`SELECT docid FROM t WHERE t MATCH 'w000000' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'w008999' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'v003999' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'small' ORDER BY docid`,
		`SELECT count(*) FROM t WHERE t MATCH 'w00450*'`,
		// The root node's first byte is its HEIGHT: 0 is a leaf, 1 an interior
		// node over leaves, 2 one over interior nodes. Reported so a case that
		// quietly stopped being deep fails the absolute check below rather
		// than passing on agreement alone.
		`SELECT hex(substr(root,1,1)) FROM t_segdir ORDER BY level, idx`,
	}

	out := map[string][]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := filepath.Join(t.TempDir(), drv+".db")
		if err := fts3SpillExec(t, drv, dsn, setup); err != nil {
			t.Fatalf("setup: %v", err)
		}
		for _, p := range probes {
			got, err := fts3SpillQuery(t, drv, dsn, p)
			if err != nil {
				t.Fatalf("[%s] %s: %v", drv, p, err)
			}
			out[drv] = append(out[drv], got)
		}
	}
	for i, p := range probes {
		if out["sqlite"][i] != out["sqlite3"][i] {
			t.Errorf("spilled segment DIVERGES from C SQLite\n  sql: %s\n  go:  %.400q\n  cgo: %.400q", p, out["sqlite"][i], out["sqlite3"][i])
		}
	}
	// The first %_segdir row's root must be a height-2 node, or this case is
	// no longer testing what it exists for.
	if heights := out["sqlite"][len(probes)-1]; !strings.Contains(heights, "\nT:02") {
		t.Errorf("no height-2 root node in %%_segdir (%q): this case no longer builds a two-level interior tree", heights)
	}
}

// fts3SpillExec runs stmts against dsn through driver drv.
func fts3SpillExec(t *testing.T, drv, dsn string, stmts []string) error {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %.60q: %w", drv, s, err)
		}
	}
	return nil
}

// fts3SpillQuery renders one statement's whole result as a string, storage
// class included (tclNormalizeCGOCell), so two engines' answers compare byte
// for byte.
func fts3SpillQuery(t *testing.T, drv, dsn, stmt string) (string, error) {
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

// fts3SpillWrite builds a table whose index has to spill into %_segments and
// therefore also carries an interior node over those leaves.
var fts3SpillWrite = []string{
	`CREATE VIRTUAL TABLE t USING fts4(a, b)`,
	`INSERT INTO t(docid,a,b) VALUES(1,'` + fts3ManyTerms("alpha", 3000) + `','tail one')`,
	`INSERT INTO t(docid,a,b) VALUES(2,'` + fts3ManyTerms("beta", 3000) + `','tail two')`,
	`INSERT INTO t(docid,a,b) VALUES(3,'small row','tail three')`,
}

// TestFts3SpillInterchange is the load-bearing half: the FILE has to cross
// engines. C SQLite -- which never saw this database being written --
// must integrity_check it and search its spilled index through the blocks it
// finds in %_segments, and vice versa.
func TestFts3SpillInterchange(t *testing.T) {
	probes := []struct{ query, want string }{
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'alpha000000' ORDER BY docid)`, "1"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'alpha002999' ORDER BY docid)`, "1"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'beta001500' ORDER BY docid)`, "2"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'tail' ORDER BY docid)`, "1,2,3"},
		{`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'small' ORDER BY docid)`, "3"},
		{`SELECT count(*) FROM t WHERE t MATCH 'alpha00299*'`, "1"},
		{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'nosuchterm')`, ""},
	}

	t.Run("this engine writes, C SQLite reads", func(t *testing.T) {
		dsn := filepath.Join(t.TempDir(), "spill.db")
		runWithDSN(t, "musql", dsn, fts3SpillWrite)

		sdb, err := sql.Open("sqlite3", exportedForOracle(t, dsn))
		if err != nil {
			t.Fatalf("sql.Open(sqlite3): %v", err)
		}
		defer sdb.Close()
		var ic string
		if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
			t.Fatalf("C SQLite reports integrity_check = %q (%v) on a database this engine wrote", ic, err)
		}
		var nBlock int
		if err := sdb.QueryRow(`SELECT count(*) FROM t_segments`).Scan(&nBlock); err != nil {
			t.Fatalf("counting %%_segments: %v", err)
		}
		if nBlock < 2 {
			t.Fatalf("this case is supposed to SPILL, but %%_segments holds %d block(s)", nBlock)
		}
		for _, p := range probes {
			var got string
			if err := sdb.QueryRow(p.query).Scan(&got); err != nil {
				t.Errorf("C SQLite failed on %q over a database this engine wrote: %v", p.query, err)
				continue
			}
			if got != p.want {
				t.Errorf("C SQLite disagrees about this engine's spilled index\n  query: %s\n  got:   %q\n  want:  %q", p.query, got, p.want)
			}
		}
		// And it can keep writing to it, which only works if it could parse
		// the interior nodes and blocks already there.
		if _, err := sdb.Exec(`INSERT INTO t(docid,a,b) VALUES(4,'alpha000000 late','tail four')`); err != nil {
			t.Fatalf("C SQLite could not insert into a spilled table this engine wrote: %v", err)
		}
		var got string
		if err := sdb.QueryRow(`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'alpha000000' ORDER BY docid)`).Scan(&got); err != nil {
			t.Fatalf("MATCH after a real-SQLite insert: %v", err)
		}
		if got != "1,4" {
			t.Errorf("after C SQLite appended a row, MATCH returned %q, want \"1,4\"", got)
		}
		if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
			t.Errorf("integrity_check after a real-SQLite write: %q (%v)", ic, err)
		}
	})

	t.Run("C SQLite writes, this engine reads", func(t *testing.T) {
		dsn := filepath.Join(t.TempDir(), "spill.db")
		runWithDSN(t, "cgo", dsn, fts3SpillWrite)

		gdb, err := sql.Open("sqlite", importedForMusql(t, dsn))
		if err != nil {
			t.Fatalf("sql.Open(sqlite): %v", err)
		}
		defer gdb.Close()
		for _, p := range probes {
			var got string
			if err := gdb.QueryRow(p.query).Scan(&got); err != nil {
				t.Errorf("this engine failed on %q over a database C SQLite wrote: %v", p.query, err)
				continue
			}
			if got != p.want {
				t.Errorf("this engine disagrees about C SQLite's spilled index\n  query: %s\n  got:   %q\n  want:  %q", p.query, got, p.want)
			}
		}
	})
}
