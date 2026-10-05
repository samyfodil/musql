// This file tests fts3/fts4 doclist delimitation: how position lists end
// and docid deltas begin.
package compat

import (
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// fts3Leaf renders a one-term segment leaf node: the leading byte doubles as
// the height (0 == leaf) and as that first term's nPrefix, then nSuffix, the
// term bytes, nDoclist, and the doclist.
func fts3Leaf(term string, doclist []byte) []byte {
	node := []byte{0x00, byte(len(term))}
	node = append(node, term...)
	node = append(node, byte(len(doclist)))
	return append(node, doclist...)
}

// fts3SegdirRow is the INSERT that replaces a table's whole index with one
// hand-written root leaf. end_block carries fts3's "<block> <nContent>" text
// form, whose second field is the root's own byte length.
func fts3SegdirRow(tbl string, node []byte) string {
	return fmt.Sprintf("INSERT INTO %s_segdir VALUES(0,0,0,0,'0 %d',X'%X')", tbl, len(node), node)
}

// r26eFts3Case is one hand-written doclist and the queries to ask of it.
type r26eFts3Case struct {
	name    string
	doclist []byte
	// declines is true for a doclist whose shape this engine's postings
	// representation cannot express (see the byte-length note on the second
	// case). Such a case is SELF-VERIFYING: the oracle is still replayed, and
	// if this engine ever ANSWERS it, the answer must match -- the gate fails
	// only on a wrong answer, never on the decline itself.
	declines bool
	queries  []string
}

var r26eFts3Cases = []r26eFts3Case{
	{
		// The recorded shape: one spare 0x00 between the terminator and the
		// next docid delta.
		name:    "one skipped zero",
		doclist: []byte{0x01, 0x02, 0x00, 0x00, 0x03, 0x02, 0x00},
		queries: []string{
			`SELECT docid FROM t WHERE t MATCH 'xx'`,
			`SELECT docid, offsets(t) FROM t WHERE t MATCH 'xx'`,
			`SELECT docid, matchinfo(t,'x') FROM t WHERE t MATCH 'xx'`,
			`SELECT docid FROM t WHERE t MATCH 'a:xx'`,
			`SELECT docid FROM t WHERE t MATCH 'b:xx'`,
			`SELECT count(*) FROM t WHERE t MATCH 'xx'`,
		},
	},
	{
		// THREE spare zeros: the skip is a loop, not a single step.
		name:    "three skipped zeros",
		doclist: []byte{0x01, 0x02, 0x00, 0x00, 0x00, 0x00, 0x03, 0x02, 0x00},
		queries: []string{
			`SELECT docid FROM t WHERE t MATCH 'xx'`,
			`SELECT docid FROM t WHERE t MATCH 'a:xx'`,
		},
	},
	{
		// A MULTI-BYTE position varint (0x82 0x01 == 130, position 128) whose
		// second byte has 0x80 clear: the scan must not stop inside it, and
		// must still find the terminator after it.
		name:    "multi-byte position varint",
		doclist: []byte{0x01, 0x82, 0x01, 0x00, 0x03, 0x02, 0x00},
		queries: []string{
			`SELECT docid FROM t WHERE t MATCH 'xx'`,
			`SELECT docid FROM t WHERE t MATCH 'a:xx'`,
		},
	},
	{
		// A DELETE MARKER (empty position list) followed by a live document:
		// the marker is byte length ZERO, which the scan produces directly.
		name:    "delete marker then a live document",
		doclist: []byte{0x01, 0x00, 0x03, 0x02, 0x00},
		queries: []string{
			`SELECT docid FROM t WHERE t MATCH 'xx'`,
			`SELECT docid FROM t WHERE t MATCH 'a:xx'`,
		},
	},
	{
		// The scan stops MID-VARINT: docid 1's position list is the single
		// byte 0x01 -- a POS_COLUMN header whose column argument was the
		// terminator. fts3 counts a document as a hit by its sublist's byte
		// LENGTH, so this document matches "t MATCH 'xx'" (oracle: 1 and 6)
		// while being invisible to "t MATCH 'a:xx'" (oracle: 6 alone).
		// docid -> column -> positions cannot express that, so it is declined.
		name:     "position list with bytes but no position",
		doclist:  []byte{0x01, 0x01, 0x00, 0x05, 0x02, 0x00},
		declines: true,
		queries: []string{
			`SELECT docid FROM t WHERE t MATCH 'xx'`,
			`SELECT docid FROM t WHERE t MATCH 'a:xx'`,
			`SELECT docid FROM t WHERE t MATCH 'b:xx'`,
		},
	},
}

// buildR26eFts3DB creates an fts3 table with a hand-written index leaf.
func buildR26eFts3DB(t *testing.T, doclist []byte) (string, *sql.DB) {
	t.Helper()
	setup := []string{`CREATE VIRTUAL TABLE t USING fts3(a,b)`}
	for d := 1; d <= 8; d++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO t(docid,a,b) VALUES(%d,'p','q')", d))
	}
	setup = append(setup, `DELETE FROM t_segdir`, fts3SegdirRow("t", fts3Leaf("xx", doclist)))

	path := t.TempDir() + "/r26e_fts3.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			db.Close()
			t.Fatalf("engine Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cpath := t.TempDir() + "/r26e_fts3_c.sqlite"
	cdb, err := sql.Open("sqlite3", cpath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range setup {
		if _, err := cdb.Exec(s); err != nil {
			cdb.Close()
			t.Fatalf("cgo Exec(%s): %v", s, err)
		}
	}
	return path, cdb
}

func TestFts3DoclistDelimiting(t *testing.T) {
	for _, tc := range r26eFts3Cases {
		t.Run(tc.name, func(t *testing.T) {
			path, cdb := buildR26eFts3DB(t, tc.doclist)
			defer cdb.Close()

			p, err := engine.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			meaningful := false
			for _, q := range tc.queries {
				vCols, vVals, vErr := p.QueryArgs(q, nil)
				cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)
				if cErr == nil && len(cRows) > 0 {
					meaningful = true
				}
				if vErr != nil && cErr != nil {
					continue // mutual rejection
				}
				if vErr != nil {
					if tc.declines {
						// Self-verifying decline: the oracle's answer is
						// recorded so the cost of the decline is visible.
						t.Logf("DECLINED [%s]: %v (oracle: %v)", q, vErr, cRows)
						continue
					}
					t.Errorf("[%s] engine DECLINED where C SQLite answers %v: %v", q, cRows, vErr)
					continue
				}
				if cErr != nil {
					t.Errorf("[%s] engine ANSWERED %v where C SQLite rejects it (%v)",
						q, engineRowsToStrings(vVals), cErr)
					continue
				}
				// Reached whether or not the case is marked declining: an
				// engine that starts ANSWERING one must answer CORRECTLY.
				vRows := engineRowsToStrings(vVals)
				if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
					t.Errorf("[%s] DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", q, reason, vRows, cRows)
				}
			}
			if !meaningful {
				t.Fatalf("fixture is wrong: C SQLite returned no rows for any query, so this case tests nothing")
			}
		})
	}
}

// TestFts3DoclistValidData is the other half: the byte scan must change
// NOTHING for doclists fts3 and this engine actually write. It drives the
// table past FTS3_MERGE_COUNT so the LEVEL MERGE -- which decodes doclists
// through the very same function -- runs too, with deletes and updates in the
// mix, because getting the delete-marker rule wrong there silently RESURRECTS
// deleted rows rather than erroring.
func TestFts3DoclistValidData(t *testing.T) {
	var setup []string
	setup = append(setup, `CREATE VIRTUAL TABLE t USING fts3(a,b)`)
	// 40 inserts: past 16 flushes, so level-0 merges into level-1 and then
	// cascades.
	for d := 1; d <= 40; d++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO t(docid,a,b) VALUES(%d,'common w%d','tail%d')", d, d, d%7))
	}
	setup = append(setup,
		`DELETE FROM t WHERE docid=3`,
		`UPDATE t SET a='common changed' WHERE docid=5`,
		`DELETE FROM t WHERE docid=17`,
	)
	for d := 41; d <= 60; d++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO t(docid,a,b) VALUES(%d,'common w%d','tail%d')", d, d, d%7))
	}
	// a document with MANY tokens, so a position varint exceeds one byte
	long := ""
	for i := 0; i < 200; i++ {
		long += fmt.Sprintf("t%d ", i)
	}
	setup = append(setup, fmt.Sprintf("INSERT INTO t(docid,a,b) VALUES(61,'%s','common')", long))

	path := t.TempDir() + "/r26e_fts3_valid.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			db.Close()
			t.Fatalf("engine Exec(%.60s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong := 0
	for _, q := range []string{
		`SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid`,
		`SELECT count(*) FROM t WHERE t MATCH 'common'`,
		`SELECT docid FROM t WHERE t MATCH 'changed'`,
		`SELECT docid FROM t WHERE t MATCH 'w3'`,
		`SELECT docid FROM t WHERE t MATCH 'w17'`,
		`SELECT docid FROM t WHERE t MATCH 'w5'`,
		`SELECT docid FROM t WHERE t MATCH 'a:common' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'b:common' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'tail3' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '"common w41"'`,
		`SELECT docid FROM t WHERE t MATCH 'w4*' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 't150'`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH 't150'`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH 'changed'`,
		`SELECT docid, matchinfo(t,'x') FROM t WHERE t MATCH 'common' ORDER BY docid`,
	} {
		vCols, vVals, vErr := p.QueryArgs(q, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)
		if cErr != nil {
			t.Fatalf("[%s] fixture is wrong: C SQLite rejected it (%v)", q, cErr)
		}
		if vErr != nil {
			wrong++
			t.Errorf("[%s] engine DECLINED valid fts3 data: %v", q, vErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
			wrong++
			t.Errorf("[%s] DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", q, reason, vRows, cRows)
		}
	}
	if wrong != 0 {
		t.Fatalf("fts3 valid-data gate FAILED: wrong=%d (must be 0)", wrong)
	}
}
