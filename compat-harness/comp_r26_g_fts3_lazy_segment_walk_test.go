// Tests lazy segment decoding for fts3: decode only the terms a MATCH query
// needs, rather than all terms in the segment node. This prevents rejection on
// corrupt terms the query never references.
package compat

import (
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// r26gSetupAndCompare creates identical fts3 tables in both engines and runs a query,
// returning results from each side.
func r26gSetupAndCompare(t *testing.T, name string, setup []string, query string) (vCols []string, vRows [][]string, vErr error, cCols []string, cRows [][]string, cErr error) {
	t.Helper()
	path := t.TempDir() + "/r26g_" + name + ".sqlite"
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

	cpath := t.TempDir() + "/r26g_" + name + "_c.sqlite"
	cdb, err := sql.Open("sqlite3", cpath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	for _, s := range setup {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup Exec(%.80s): %v", s, err)
		}
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	_, vVals, vErr := p.QueryArgs(query, nil)
	vRows = engineRowsToStrings(vVals)
	cCols, cRows, cErr = cgoSelect(t, cdb, query, nil)
	return nil, vRows, vErr, cCols, cRows, cErr
}

// TestFts3LazySegmentWalkClosesCorpusCase tests that lazy decoding fixes a
// "corrupt segment node" rejection on a hand-crafted corpus case.
func TestFts3LazySegmentWalkClosesCorpusCase(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE def USING fts3(xyz)`,
		`INSERT INTO def_segdir VALUES(0,0,0,0,0, X'0001310301c9000103323334050d81')`,
	}
	query := `SELECT rowid FROM def WHERE def MATCH '1 NEAR 1'`
	_, vRows, vErr, _, cRows, cErr := r26gSetupAndCompare(t, "corpus", setup, query)

	if vErr != nil && strings.Contains(vErr.Error(), "corrupt segment node") {
		t.Fatalf("REGRESSION: still declines with the pre-fix reason: %v", vErr)
	}
	// Check that if both engines succeed or both error, they agree.
	if vErr == nil && cErr == nil {
		if ok, reason := queryResultsMatch(nil, vRows, nil, cRows, false); !ok {
			t.Fatalf("DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", reason, vRows, cRows)
		}
	}
	if vErr == nil && cErr != nil {
		t.Fatalf("engine ANSWERED %v where C SQLite rejects it (%v)", vRows, cErr)
	}
	t.Logf("engine: rows=%v err=%v; cgo: rows=%v err=%v", vRows, vErr, cRows, cErr)
}

// TestFts3LazySegmentWalkStillFindsRealCorruption tests that lazy decoding
// still detects genuine corruption in segment nodes that lies on the path to
// needed terms, and does not skip past reachable corruption.
func TestFts3LazySegmentWalkStillFindsRealCorruption(t *testing.T) {
	// Hand-built node with three terms: "1" and "2" well-formed, "9" corrupt (overrun).
	node, err := hex.DecodeString("000131030502000001320306020000013905")
	if err != nil {
		t.Fatal(err)
	}
	setup := []string{
		`CREATE VIRTUAL TABLE t9 USING fts3(a)`,
		`INSERT INTO t9_content VALUES(5, 'apple')`,
		`INSERT INTO t9_content VALUES(6, 'banana')`,
		`INSERT INTO t9_segdir VALUES(0,0,0,0,0, X'` + hex.EncodeToString(node) + `')`,
	}

	t.Run("MATCH '9' walks past two well-formed terms into the real corruption", func(t *testing.T) {
		_, vRows, vErr, _, cRows, cErr := r26gSetupAndCompare(t, "t9_nine", setup, `SELECT rowid FROM t9 WHERE t9 MATCH '9'`)
		if vErr == nil {
			t.Fatalf("expected a decline (term \"9\"'s header genuinely overruns the node): engine answered %v (oracle: rows=%v err=%v)", vRows, cRows, cErr)
		}
		if !strings.Contains(vErr.Error(), "corrupt segment node") {
			t.Fatalf("declined for an unexpected reason (want \"corrupt segment node\"): %v", vErr)
		}
		if cErr == nil {
			// Oracle tolerates the corrupt shape.
			t.Logf("NOTE: oracle did NOT error on this shape (rows=%v) -- engine still correctly declined rather than guess", cRows)
		} else {
			t.Logf("mutual decline, as expected: engine=%v cgo=%v", vErr, cErr)
		}
	})

	// MATCH '1' is the first term and well-formed, so lazy decoding finds it
	// without parsing the corrupt term.
	t.Run("MATCH '1' never reaches the corrupt term and fully agrees with the oracle", func(t *testing.T) {
		_, vRows, vErr, _, cRows, cErr := r26gSetupAndCompare(t, "t9_one", setup, `SELECT rowid FROM t9 WHERE t9 MATCH '1'`)
		if vErr != nil {
			t.Fatalf("expected success (term \"1\" is well-formed and needs no traversal past itself): %v (oracle: rows=%v err=%v)", vErr, cRows, cErr)
		}
		if cErr != nil {
			t.Fatalf("engine answered %v but the oracle rejects the same query: %v", vRows, cErr)
		}
		if ok, reason := queryResultsMatch(nil, vRows, nil, cRows, false); !ok {
			t.Fatalf("DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", reason, vRows, cRows)
		}
		t.Logf("AGREES: %v", vRows)
	})
}
