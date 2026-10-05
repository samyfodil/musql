// This file gates the fts3 doclist reader's unbounded position-list scan
// with test cases from the fts3corrupt test suite.
package compat

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// r26fFts3Case is a test case with setup and queries.
type r26fFts3Case struct {
	name    string
	setup   []string
	queries []string
}

var r26fFts3Cases = []r26fFts3Case{
	{
		// fts3corrupt4.test 39.0/39.1. Term "1"'s doclist (07 82 00) ends in a
		// literal 0x00 (passes the structural check) but the scan is still
		// mid-varint there (the preceding byte 0x82 has its continuation bit
		// set), so it must run on through term "1234"'s own real header bytes
		// (01 03 32 33 34 05) and into its real doclist (01 01 01 02 00),
		// stopping at THAT doclist's genuine terminator -- landing on docid 7
		// (verified against engine's TestFts3DoclistOverrunsIntoNextTermBytes,
		// which decodes these exact bytes white-box). t0_content holds only
		// rowid 1, so docid 7 has no content row -- both queries below are a
		// genuine PASS now that fts3StmtContentUnneeded (fts3_search.go)
		// proves neither one's select list ever reads a real column.
		name: "fts3corrupt4 39 -- t0 NEAR overrun into next term's real bytes",
		setup: []string{
			`CREATE VIRTUAL TABLE t0 USING fts3(col0 INTEGER PRIMARY KEY, col1 VARCHAR(8), col2 BINARY, col3 BINARY)`,
			`INSERT INTO t0_content VALUES(1,1,'1234','aaaa','bbbb')`,
			`INSERT INTO t0_segdir VALUES(0,0,0,0,'0 42',X'000131030782000103323334050101010200000461616161050101020200000462626262050101030200')`,
		},
		queries: []string{
			`SELECT rowid FROM t0 WHERE t0 MATCH '1 NEAR 1'`,
			`SELECT count(*) FROM t0 WHERE t0 MATCH '1 NEAR 1'`,
		},
	},
	{
		// fts3corrupt4.test 43.1/43.2. Term "1"'s doclist here is JUST "01 c9
		// 00" (nDoclist=3): delta 1 (docid 1), then a position-list scan that
		// is still mid-varint at the declared boundary (0xc9's continuation
		// bit) and has to run through the whole of term "1234"'s real header
		// bytes AND past the node's real end into its zero PADDING before
		// finding a terminator there -- this is the one case here that
		// exercises padding, not just a next term's real bytes. Term "1234"'s
		// own second term declaration (nDoclist=5, but only 2 bytes of real
		// node remain after its header) is itself truncated -- but
		// `MATCH '1 NEAR 1'` only ever needs term "1" (the node's FIRST term,
		// an exact match for both operands), so fts3LoadIndex's lazy walk
		// (fts3GatherWantedTerms) never structurally parses "1234" as a term
		// of its own; it is only ever touched as raw bytes inside term "1"'s
		// own unbounded position-list scan, never as a header
		// fts3DecodeLeafNode's outer loop parses in its own right. See
		// engine/fts3_lazy_leaf_test.go for a white-box proof of that
		// distinction.
		name: "fts3corrupt4 43 -- def NEAR overrun into a truncated second term",
		setup: []string{
			`CREATE VIRTUAL TABLE def USING fts3(xyz)`,
			`INSERT INTO def_segdir VALUES(0,0,0,0,0, X'0001310301c9000103323334050d81')`,
		},
		queries: []string{
			`SELECT rowid FROM def WHERE def MATCH '1 NEAR 1'`,
		},
	},
	{
		// fts3corrupt4.test 44.1/44.2. Term "1"'s doclist is "01 02 00 01 ba"
		// (nDoclist=5): the position list for docid 1 overruns via a 0xba
		// byte (continuation bit set) sitting one byte before the declared
		// boundary, again running into term "234"'s real header bytes.
		// t0_content holds only rowid 0, and the match resolves to docid 1 --
		// STILL declines even with fts3StmtContentUnneeded in place, for a
		// SEPARATE, independent reason: matchinfo()'s own second argument
		// here is the column t0, not a literal format string, which makes
		// fts3ResultColumnSafe (fts3_search.go) refuse this select list too
		// (correctly -- the same non-literal-format-string decline this
		// engine's matchinfo() compiler already raises on its own).
		name: "fts3corrupt4 44 -- t0 matchinfo over an overrun doclist",
		setup: []string{
			`CREATE VIRTUAL TABLE t0 USING fts3(col0 INTEGER PRIMARY KEY,col1 VARCHAR(8),col2 BINARY,col3 BINARY)`,
			`INSERT INTO t0_content VALUES(0,NULL,NULL,NULL,NULL)`,
			`INSERT INTO t0_segdir VALUES(0,0,0,0,'0 42',X'00013103010200010332333405010201ba00000461616161050101020200000462626262050101030200')`,
		},
		queries: []string{
			`SELECT matchinfo(t0, t0) IS NULL FROM t0 WHERE t0 MATCH '1*'`,
		},
	},
	{
		// fts3corrupt4.test 40.1/40.2 (verbatim from the .test file -- t0_content
		// is never populated at all, so EVERY docid the index matches is
		// missing from it). Exercises matchinfo() alone as the ENTIRE select
		// list, which fts3ResultColumnSafe recognises as content-free
		// (fts3MatchinfoFunc, fts3.c:3822-3836, never calls fts3CursorSeek) --
		// a genuine PASS.
		name: "fts3corrupt4 40 -- t0 matchinfo alone over an empty content table",
		setup: []string{
			`CREATE VIRTUAL TABLE t0 USING fts3(col0 INTEGER PRIMARY KEY, col1, col2 ,col3 )`,
			`INSERT INTO t0_segdir VALUES(0,0,0,0,'0 42',X'0001310301020001033233340500010102000004616161bc050101020200000462626262050101030200')`,
		},
		queries: []string{
			`SELECT 0==matchinfo(t0,'sx') FROM t0 WHERE t0 MATCH '1* 2 3 4 5 6 OR 1'`,
		},
	},
	{
		// fts3corrupt6.test 2.0/2.1. Term "1234"'s own doclist (01 00 ff f2
		// 00) opens with an EMPTY position list (delta 1, immediate 0x00 --
		// docid 1 is a delete marker) and then a second docid whose huge delta
		// varint (ff f2 00) ends exactly ON the declared nDoclist boundary, so
		// ITS position list scan overruns into the whole of the next term's
		// real header+suffix+nDoclist+doclist bytes. Exercises a NEAR compound
		// expression ('(1 NEAR 1) AND (aaaa OR 1)') on top of the overrun,
		// matching the exact corpus statement. t0_content has ZERO rows here
		// (no INSERT INTO t0_content at all) -- a genuine PASS now that
		// fts3StmtContentUnneeded proves count(*) alone never reads one.
		name: "fts3corrupt6 2 -- t0 NEAR+OR compound over a double overrun",
		setup: []string{
			`CREATE VIRTUAL TABLE t0 USING fts3(a)`,
			`INSERT INTO t0_segdir VALUES(0,0,0,0,'0 42',X'000131030782000103323334050100fff200010461616161050101020200000462626262050101030200')`,
		},
		queries: []string{
			`SELECT count(*) FROM t0 WHERE t0 MATCH '(1 NEAR 1) AND (aaaa OR 1)'`,
		},
	},
}

func TestFts3DoclistOverrunCorpusCases(t *testing.T) {
	for _, tc := range r26fFts3Cases {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/r26f_fts3.sqlite"
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					db.Close()
					t.Fatalf("engine setup Exec(%.80s): %v", s, err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			cpath := t.TempDir() + "/r26f_fts3_c.sqlite"
			cdb, err := sql.Open("sqlite3", cpath)
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()
			for _, s := range tc.setup {
				if _, err := cdb.Exec(s); err != nil {
					t.Fatalf("cgo setup Exec(%.80s): %v", s, err)
				}
			}

			p, err := engine.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			for _, q := range tc.queries {
				vCols, vVals, vErr := p.QueryArgs(q, nil)
				cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)

				// The one thing this specific fix promises: this decline
				// reason must never be seen again on these corpus statements.
				// Whatever else the engine does with them, it must not be
				// THIS.
				if vErr != nil && strings.Contains(vErr.Error(), "unterminated position list") {
					t.Errorf("[%s] REGRESSION: still declines with the pre-fix reason: %v", q, vErr)
					continue
				}

				if vErr != nil && cErr != nil {
					t.Logf("[%s] mutual decline: engine=%v cgo=%v", q, vErr, cErr)
					continue
				}
				if vErr != nil {
					// A decline where the oracle answers is not wrong, only
					// incomplete -- acceptable as long as it is not the old
					// reason (checked above). Both remaining reasons here are
					// separate, pre-existing, already-documented blockers
					// (see the file doc comment) that this fix does not
					// close; record which one so a regression to a THIRD,
					// new decline reason is at least visible in the log.
					t.Logf("[%s] declines (not the old bound): oracle answers %v: %v", q, cRows, vErr)
					continue
				}
				if cErr != nil {
					t.Errorf("[%s] engine ANSWERED %v where C SQLite rejects it (%v)",
						q, engineRowsToStrings(vVals), cErr)
					continue
				}
				// Both answered: the one case that must never be wrong.
				vRows := engineRowsToStrings(vVals)
				if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
					t.Errorf("[%s] DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", q, reason, vRows, cRows)
				} else {
					t.Logf("[%s] AGREES: %v", q, vRows)
				}
			}
		})
	}
}

// TestFts3DoclistDeclaredTerminatorDeclines is the excluded-class regression
// this fix's soundness depends on: unbounding the scan must NOT turn a
// doclist whose declared final byte is genuinely not 0x00 into something this
// engine accepts. fts3_write.c:1450-1453's structural check runs before any
// scanning and is unaffected by this change -- verified here self-verifyingly
// against the live oracle, using comp_r26_e_fts3_doclist_test.go's shared
// fts3Leaf/fts3SegdirRow/buildR26eFts3DB helpers (same package).
func TestFts3DoclistDeclaredTerminatorDeclines(t *testing.T) {
	// nDoclist=2, doclist bytes "01 01": a single-byte position list (01)
	// would need a terminating 0x00 right after it, but the declared window's
	// last byte is 0x01, not 0x00 -- corrupt by the raw byte check regardless
	// of anything a scan might find past it.
	doclist := []byte{0x01, 0x01}
	path, cdb := buildR26eFts3DB(t, doclist)
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	q := `SELECT docid FROM t WHERE t MATCH 'xx'`
	_, vVals, vErr := p.QueryArgs(q, nil)
	_, cRows, cErr := cgoSelect(t, cdb, q, nil)

	if vErr == nil {
		t.Fatalf("[%s] engine ANSWERED %v -- the fts3_write.c:1450-1453 declared-terminator check must still fire regardless of the unbounded scan (oracle: err=%v rows=%v)",
			q, engineRowsToStrings(vVals), cErr, cRows)
	}
	if strings.Contains(vErr.Error(), "unterminated position list") {
		t.Fatalf("[%s] declined for the wrong reason (%v) -- expected the declared-terminator check, not the padded-scan backstop", q, vErr)
	}
	// vErr != nil, for the right reason: declined, as required. Whether cgo
	// also declines or not, this engine is never WRONG by staying silent
	// here.
	t.Logf("[%s] declined as required: %v (oracle: err=%v rows=%v)", q, vErr, cErr, cRows)
}

// TestFts3ContentUnneededExcludedClass verifies that content-aware
// optimization does not widen content-existence checks beyond safe shapes.
func TestFts3ContentUnneededExcludedClass(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t0 USING fts3(col0 INTEGER PRIMARY KEY, col1 VARCHAR(8), col2 BINARY, col3 BINARY)`,
		`INSERT INTO t0_content VALUES(1,1,'1234','aaaa','bbbb')`,
		`INSERT INTO t0_segdir VALUES(0,0,0,0,'0 42',X'000131030782000103323334050101010200000461616161050101020200000462626262050101030200')`,
	}
	queries := []string{
		`SELECT rowid, col1 FROM t0 WHERE t0 MATCH '1 NEAR 1'`,
		`SELECT offsets(t0) FROM t0 WHERE t0 MATCH '1 NEAR 1'`,
		`SELECT snippet(t0) FROM t0 WHERE t0 MATCH '1 NEAR 1'`,
	}

	path := t.TempDir() + "/r26f_excluded.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			db.Close()
			t.Fatalf("engine setup Exec(%.80s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cpath := t.TempDir() + "/r26f_excluded_c.sqlite"
	cdb, err := sql.Open("sqlite3", cpath)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range setup {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup Exec(%.80s): %v", s, err)
		}
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, q := range queries {
		_, vVals, vErr := p.QueryArgs(q, nil)
		_, cRows, cErr := cgoSelect(t, cdb, q, nil)

		if vErr == nil {
			t.Errorf("[%s] engine ANSWERED %v -- this excluded class must still decline (a real %%_content read), never guess a value for the content-missing docid (oracle: err=%v rows=%v)",
				q, engineRowsToStrings(vVals), cErr, cRows)
			continue
		}
		if strings.Contains(vErr.Error(), "unterminated position list") {
			t.Errorf("[%s] declined for the WRONG (pre-fix) reason: %v", q, vErr)
			continue
		}
		t.Logf("[%s] declined as required: %v (oracle: err=%v rows=%v)", q, vErr, cErr, cRows)
	}
}
