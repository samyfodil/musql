// This package tests FTS3 matchinfo() handling with self-references and
// literal NULL format arguments.
package compat

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// TestFts3MatchinfoSelfReferenceFormat tests matchinfo() with a self-reference
// over a hand-corrupted shadow table.
func TestFts3MatchinfoSelfReferenceFormat(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t0 USING fts3(col0 INTEGER PRIMARY KEY,col1 VARCHAR(8),col2 BINARY,col3 BINARY)`,
		`INSERT INTO t0_content VALUES(0,NULL,NULL,NULL,NULL)`,
		`INSERT INTO t0_segdir VALUES(0,0,0,0,'0 42',X'00013103010200010332333405010201ba00000461616161050101020200000462626262050101030200')`,
	}
	query := `SELECT matchinfo(t0, t0) IS NULL FROM t0 WHERE t0 MATCH '1*'`

	path := t.TempDir() + "/fts3_matchinfo_selfref.sqlite"
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

	cpath := t.TempDir() + "/fts3_matchinfo_selfref_c.sqlite"
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

	vCols, vVals, vErr := p.QueryArgs(query, nil)
	if vErr != nil {
		if strings.Contains(vErr.Error(), "t0_content does not hold") {
			t.Fatalf("REGRESSION: still declines on the content-existence check: %v", vErr)
		}
		if strings.Contains(vErr.Error(), "non-literal format string") {
			t.Fatalf("REGRESSION: still declines on the matchinfo() format-string check: %v", vErr)
		}
		t.Fatalf("engine declined the mined statement: %v", vErr)
	}
	vRows := engineRowsToStrings(vVals)

	cCols, cRows, cErr := cgoSelect(t, cdb, query, nil)
	if cErr != nil {
		t.Fatalf("oracle rejected its own corpus statement: %v", cErr)
	}
	if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
		t.Fatalf("DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", reason, vRows, cRows)
	}
	// fts3corrupt4.test's own do_execsql_test 44.2 pins this result as {0}
	// (the mined statement is column 1 of a 1-column, 1-row result).
	if len(vRows) != 1 || len(vRows[0]) != 1 || vRows[0][0] != "I:0" {
		t.Fatalf("got %v, want the single row {0} fts3corrupt4.test 44.2 pins", vRows)
	}
}

// TestFts3MatchinfoLiteralNullFormat tests matchinfo() with a literal NULL
// format argument.
func TestFts3MatchinfoLiteralNullFormat(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t USING fts3(a)`,
		`INSERT INTO t VALUES('one two three')`,
	}
	query := `SELECT matchinfo(t, NULL) = matchinfo(t) FROM t WHERE t MATCH 'one'`

	path := t.TempDir() + "/fts3_matchinfo_null.sqlite"
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

	cpath := t.TempDir() + "/fts3_matchinfo_null_c.sqlite"
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

	vCols, vVals, vErr := p.QueryArgs(query, nil)
	if vErr != nil {
		t.Fatalf("engine declined: %v", vErr)
	}
	vRows := engineRowsToStrings(vVals)

	cCols, cRows, cErr := cgoSelect(t, cdb, query, nil)
	if cErr != nil {
		t.Fatalf("oracle rejected: %v", cErr)
	}
	if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
		t.Fatalf("DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", reason, vRows, cRows)
	}
	if len(vRows) != 1 || len(vRows[0]) != 1 || vRows[0][0] != "I:1" {
		t.Fatalf("got %v, want {1} (a literal NULL format reads back as the default)", vRows)
	}
}
