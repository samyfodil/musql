//go:build sqlite_fts5

// Tests FTS5 auxiliary functions: rank, bm25, highlight, and snippet.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestFts5AuxAnswerDiff(t *testing.T) {
	cases := []struct {
		name         string
		setup        []string
		probes       []string
		allowDecline bool
	}{
		{
			// bm25's exact arithmetic (k1=1.2, b=0.75, IDF floored at 1e-06),
			// including the RAW value (not just round(bm25(),6)): a 3-row
			// corpus where "quick"/"fox" both have a negative raw IDF (2 of 3
			// rows contain each), so every score here exercises the floor.
			name: "bm25 arithmetic",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('the quick brown fox', 'jumps over')`,
				`INSERT INTO t VALUES('quick quick', 'fox fox fox')`,
				`INSERT INTO t VALUES('lazy dog sleeps', 'all day long')`,
			},
			probes: []string{
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
				`SELECT rowid, bm25(t, 10.0) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
				`SELECT rowid, bm25(t, 1.0, 10.0) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
				`SELECT rowid, bm25(t, 0.0) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
				`SELECT rowid, bm25(t, -1.0) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
				// more weights than columns: the extras are silently unused.
				`SELECT rowid, bm25(t,1.0,2.0,3.0,4.0) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
				`SELECT rowid, rank FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid, bm25(t), rank FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid, typeof(bm25(t)) FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'quick' ORDER BY rank`,
				`SELECT rowid FROM t WHERE t MATCH 'quick' ORDER BY rank DESC`,
				`SELECT t.rowid FROM t WHERE t MATCH 'quick' ORDER BY t.rank`,
				// single-column MATCH form combines fine with bm25/rank.
				`SELECT rowid, bm25(t) FROM t WHERE a MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE t.a MATCH 'quick' ORDER BY rowid`,
				// MATCH combined with an ordinary AND conjunct, either order.
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'quick' AND rowid > 0 ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE rowid > 0 AND t MATCH 'quick' ORDER BY rowid`,
			},
		},
		{
			// bm25/highlight/rank over a corpus with an empty and a NULL
			// column: NULL/'' both tokenize to zero tokens, so D and avgdl
			// both treat them identically.
			name: "empty and NULL columns don't skew docsize",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('one two three', '')`,
				`INSERT INTO t VALUES('', 'four five')`,
				`INSERT INTO t VALUES(NULL, NULL)`,
			},
			probes: []string{
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'two' ORDER BY rowid`,
			},
		},
		{
			// The connective-flattening rule: OR/AND/NEAR sum every phrase
			// using this row's own PLAIN occurrence count (no proximity
			// trimming, no OR-branch masking, a repeated phrase counted
			// twice); a NOT's excluded side contributes nothing, even for a
			// row where it is physically present but matched via an
			// unrelated sibling OR branch.
			name: "connective flattening: OR/AND/NOT/NEAR",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('alpha beta both here')`,
				`INSERT INTO t VALUES('alpha only present')`,
				`INSERT INTO t VALUES('beta only present')`,
			},
			probes: []string{
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'beta' ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'alpha OR beta' ORDER BY rowid`,
				`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'alpha OR beta' ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'alpha AND beta' ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'alpha NOT beta' ORDER BY rowid`,
				`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'alpha NOT beta' ORDER BY rowid`,
				// a repeated phrase across OR is double-counted, not deduped.
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'alpha OR alpha' ORDER BY rowid`,
			},
		},
		{
			name: "NOT's excluded side is invisible even inside a sibling OR branch",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('bravo charlie')`,
				`INSERT INTO t VALUES('charlie only')`,
				`INSERT INTO t VALUES('alpha only')`,
			},
			probes: []string{
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH '(alpha NOT bravo) OR charlie' ORDER BY rowid`,
				`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH '(alpha NOT bravo) OR charlie' ORDER BY rowid`,
			},
		},
		{
			name: "NEAR sums its phrases with no proximity trimming",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('quick brown fox jumps')`,
				`INSERT INTO t VALUES('quick fox')`,
			},
			probes: []string{
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'NEAR(quick fox)' ORDER BY rowid`,
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'NEAR(quick fox, 1)' ORDER BY rowid`,
				`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'NEAR(quick fox)' ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'NEAR(quick fox)' ORDER BY rank`,
			},
		},
		{
			// column-filtered phrases combine with OR exactly like an
			// unrestricted one -- each phrase keeps its own column filter.
			name: "column-filtered phrases under OR",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('quick fox', 'quick fox')`,
				`INSERT INTO t VALUES('quick only', 'nothing')`,
			},
			probes: []string{
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'a:quick OR b:fox' ORDER BY rowid`,
				`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'a:quick OR b:fox' ORDER BY rowid`,
				`SELECT rowid, highlight(t,1,'[',']') FROM t WHERE t MATCH 'a:quick OR b:fox' ORDER BY rowid`,
			},
		},
		{
			// No MATCH anywhere in the statement: bm25()/highlight()/
			// snippet() still answer (a phrase-less score/unmarked text),
			// but "rank" -- the hidden column -- is NULL, not bm25(t)'s -0.
			name: "no MATCH at all",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('aaa bbb ccc ddd eee fff ggg hhh', 'col b text')`,
			},
			probes: []string{
				`SELECT rowid, bm25(t), bm25(t, 5.0) FROM t`,
				`SELECT rowid, rank FROM t`,
				`SELECT rowid FROM t ORDER BY rank`,
				`SELECT rowid, highlight(t,0,'[',']') FROM t`,
				`SELECT rowid, snippet(t, 0, '[', ']', '...', 3) FROM t`,
				`SELECT rowid, snippet(t,0,'[',']','...',3) FROM t WHERE rowid=1`,
			},
		},
		{
			name: "highlight() exact output, including out-of-range columns",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('The quick brown fox jumps over the lazy dog', 'jumps over')`,
				`INSERT INTO t VALUES('quick quick', 'fox fox fox')`,
			},
			probes: []string{
				`SELECT rowid, highlight(t, 0, '[', ']') FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid, highlight(t, 1, '[', ']') FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
				`SELECT rowid, highlight(t, 5, '[', ']') FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
				`SELECT rowid, highlight(t, -1, '[', ']') FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
			},
		},
		{
			// snippet()'s window centering/clamping and the sentence-start
			// bonus, across a spread of token budgets and match positions
			// (start/middle/end of document, with and without punctuation).
			name: "snippet() window selection",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('aaa bbb ccc TARGET eee fff ggg hhh')`,
			},
			probes: []string{
				`SELECT snippet(t, 0, '[', ']', '...', 1) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, 0, '[', ']', '...', 2) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, 0, '[', ']', '...', 3) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, 0, '[', ']', '...', 4) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, 0, '[', ']', '...', 8) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, 0, '[', ']', '...', 20) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, -1, '[', ']', '...', 64) FROM t WHERE t MATCH 'TARGET'`,
			},
		},
		{
			name: "snippet() sentence-boundary bias",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('aaa bbb ccc ddd. eee fff TARGET ggg hhh iii.')`,
			},
			probes: []string{
				`SELECT snippet(t, 0, '[', ']', '...', 2) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, 0, '[', ']', '...', 3) FROM t WHERE t MATCH 'TARGET'`,
				`SELECT snippet(t, 0, '[', ']', '...', 4) FROM t WHERE t MATCH 'TARGET'`,
			},
		},
		{
			// Genuine C-SQLite rejections this engine also rejects (never
			// answered): both sides decline, which the runner below treats
			// as agreement with no allowDecline needed.
			name: "arity and malformed-argument errors (both engines reject)",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('quick fox', 'jumps over')`,
			},
			probes: []string{
				`SELECT bm25() FROM t WHERE t MATCH 'quick'`,
				`SELECT highlight(t,0,'[') FROM t WHERE t MATCH 'quick'`,
				`SELECT highlight(t,0,'[',']','x') FROM t WHERE t MATCH 'quick'`,
				`SELECT snippet(t,0,'[',']','...') FROM t WHERE t MATCH 'quick'`,
				// arg0 not naming the table itself: a real column ("no such
				// cursor" from C fts5), a name resolving to nothing ("no
				// such column"), a string literal (not an identifier).
				`SELECT bm25(a) FROM t WHERE t MATCH 'quick'`,
				`SELECT bm25(nosuchtable) FROM t WHERE t MATCH 'quick'`,
				`SELECT bm25('t') FROM t WHERE t MATCH 'quick'`,
			},
		},
		{
			// bm25/rank/highlight over a JOIN, with the fts5 table in the
			// SECOND (non-zero-offset) FROM position: the current row has to
			// be gathered from every open cursor (gatherRowScopes), not just
			// assumed to start at register/column offset 0.
			name: "bm25 and rank over a join, fts5 table not first",
			setup: []string{
				`CREATE TABLE side(id INTEGER PRIMARY KEY, tag)`,
				`INSERT INTO side VALUES(1,'x'),(2,'y')`,
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t(rowid,a) VALUES(1,'quick fox'),(2,'quick quick')`,
			},
			probes: []string{
				`SELECT side.id, t.rowid, bm25(t) FROM side, t WHERE t MATCH 'quick' AND side.id=t.rowid ORDER BY side.id`,
				`SELECT side.id, t.rowid FROM side JOIN t ON side.id=t.rowid WHERE t MATCH 'quick' ORDER BY rank`,
				`SELECT side.id, highlight(t,0,'[',']') FROM side, t WHERE t MATCH 'quick' AND side.id=t.rowid ORDER BY side.id`,
			},
		},
		{
			// Scope limits this engine declines that C fts5 actually
			// answers: a non-literal MATCH pattern (this engine bakes the
			// corpus scan into the compiled Program at prepare time, which a
			// per-execution bound value cannot safely feed -- see
			// fts5_vdbe_aux.go), and more than one fts5 table carrying an
			// aux reference (C fts5 resolves each bm25(t1)/bm25(t2) call
			// independently and even supports a QUALIFIED rank; this engine
			// declines the whole shape). Both need allowDecline: true since
			// the oracle does not reject these itself.
			name:         "scope limits: non-literal MATCH pattern, multiple fts5 tables",
			allowDecline: true,
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('quick fox')`,
				`CREATE VIRTUAL TABLE t2 USING fts5(a)`,
				`INSERT INTO t2 VALUES('quick fox')`,
			},
			probes: []string{
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH ('qu' || 'ick') ORDER BY rowid`,
				`SELECT t.rowid, bm25(t) FROM t, t2 WHERE t MATCH 'quick' AND t2 MATCH 'fox'`,
				`SELECT t.rowid FROM t, t2 WHERE t MATCH 'quick' AND t2 MATCH 'fox' ORDER BY rank`,
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

// TestFts5Bm25RawPrecision checks bm25()'s RAW (unrounded) float64 output
// against the oracle's, not just round(bm25(),6) -- reading the value
// through database/sql as a float64 directly (rather than through
// fts5Query's textual cell rendering) and comparing with Go's == on the
// float64 bits themselves, so a difference at any ULP would fail this test
// even if it happened to round-trip identically through string formatting.
func TestFts5Bm25RawPrecision(t *testing.T) {
	dir := t.TempDir()
	goDSN := filepath.Join(dir, "musql.db")
	cgoDSN := filepath.Join(dir, "cgo.db")
	setup := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('the quick brown fox', 'jumps over')`,
		`INSERT INTO t VALUES('quick quick', 'fox fox fox')`,
		`INSERT INTO t VALUES('lazy dog sleeps', 'all day long')`,
	}
	for _, dsn := range []string{goDSN, cgoDSN} {
		drv := "sqlite"
		if dsn == cgoDSN {
			drv = "sqlite3"
		}
		if err := fts5Exec(t, drv, dsn, setup); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	goDB, err := sql.Open("sqlite", goDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer goDB.Close()
	cgoDB, err := sql.Open("sqlite3", cgoDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer cgoDB.Close()

	for _, q := range []string{
		`SELECT bm25(t) FROM t WHERE t MATCH 'quick' ORDER BY rowid`,
		`SELECT bm25(t) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
		`SELECT bm25(t, 10.0) FROM t WHERE t MATCH 'fox' ORDER BY rowid`,
	} {
		goRows, err := goDB.Query(q)
		if err != nil {
			t.Fatalf("%s: musql: %v", q, err)
		}
		var goVals []float64
		for goRows.Next() {
			var v float64
			if err := goRows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			goVals = append(goVals, v)
		}
		goRows.Close()

		cgoRows, err := cgoDB.Query(q)
		if err != nil {
			t.Fatalf("%s: cgo: %v", q, err)
		}
		var cgoVals []float64
		for cgoRows.Next() {
			var v float64
			if err := cgoRows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			cgoVals = append(cgoVals, v)
		}
		cgoRows.Close()

		if len(goVals) != len(cgoVals) {
			t.Fatalf("%s: row count %d vs %d", q, len(goVals), len(cgoVals))
		}
		for i := range goVals {
			if goVals[i] != cgoVals[i] {
				t.Errorf("%s: row %d RAW bm25 differs: musql=%s cgo=%s", q, i, fmt.Sprintf("%.20g", goVals[i]), fmt.Sprintf("%.20g", cgoVals[i]))
			}
		}
	}
}
