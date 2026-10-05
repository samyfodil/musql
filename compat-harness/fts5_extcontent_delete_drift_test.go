//go:build sqlite_fts5

// FTS5 external-content 'delete' command tests verify that the engine handles
// drift in token counts when delete commands do not match actual row content,
// and that excluded edge cases correctly decline.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// engineSession holds one continuous write session on both engines paired together.
type engineSession struct {
	t     *testing.T
	godb  *engine.Session
	cgodb *sql.DB
}

func newEngineSession(t *testing.T) *engineSession {
	t.Helper()
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { godb.Discard() })
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	t.Cleanup(func() { cgodb.Close() })
	cgodb.SetMaxOpenConns(1)
	if _, err := cgodb.Exec("PRAGMA synchronous=OFF"); err != nil {
		t.Fatalf("cgo PRAGMA synchronous=OFF: %v", err)
	}
	return &engineSession{t: t, godb: godb, cgodb: cgodb}
}

// agreeExec runs stmt on both engines and requires BOTH to succeed.
func (s *engineSession) agreeExec(stmt string) {
	s.t.Helper()
	goErr, panicked, panicVal := tclSafeExecArgs(s.godb, stmt)
	if panicked {
		s.t.Fatalf("PANIC executing %q: %v", stmt, panicVal)
	}
	if goErr != nil {
		s.t.Fatalf("this engine DECLINES a statement expected to succeed\n  sql: %s\n  err: %v", stmt, goErr)
	}
	if _, cgoErr := s.cgodb.Exec(stmt); cgoErr != nil {
		s.t.Fatalf("oracle rejects a statement expected to succeed\n  sql: %s\n  cgo err: %v", stmt, cgoErr)
	}
}

// bothErr runs stmt on both engines and requires BOTH to fail, the cgo side
// with an error containing wantCgoSubstr.
func (s *engineSession) bothErr(stmt, wantCgoSubstr string) {
	s.t.Helper()
	goErr, panicked, panicVal := tclSafeExecArgs(s.godb, stmt)
	if panicked {
		s.t.Fatalf("PANIC executing %q: %v", stmt, panicVal)
	}
	if goErr == nil {
		s.t.Errorf("this engine ACCEPTS a statement C fts5 refuses\n  sql: %s", stmt)
	}
	_, cgoErr := s.cgodb.Exec(stmt)
	if cgoErr == nil {
		s.t.Fatalf("test fixture assumption wrong: oracle ACCEPTS %q", stmt)
	}
	if !strings.Contains(cgoErr.Error(), wantCgoSubstr) {
		s.t.Fatalf("test fixture assumption wrong: oracle's error for %q is %q, wanted it to contain %q", stmt, cgoErr.Error(), wantCgoSubstr)
	}
}

// declineOK runs stmt only on musql and requires it to fail -- the
// never-wrong claim for an excluded class: a decline is always a safe
// answer, regardless of what the oracle does with the same statement.
func (s *engineSession) declineOK(stmt string) {
	s.t.Helper()
	goErr, panicked, panicVal := tclSafeExecArgs(s.godb, stmt)
	if panicked {
		s.t.Fatalf("PANIC executing %q: %v", stmt, panicVal)
	}
	if goErr == nil {
		s.t.Errorf("this engine ACCEPTS a statement expected to stay declined\n  sql: %s", stmt)
	}
}

// oracleAccepts reports whether stmt succeeds against the CGo oracle,
// without touching musql at all -- used to confirm an excluded-class
// fixture is a REAL gap (the oracle actually serves it) rather than a
// statement that would have mutually rejected anyway.
func (s *engineSession) oracleAccepts(stmt string) bool {
	s.t.Helper()
	_, err := s.cgodb.Exec(stmt)
	return err == nil
}

// agreeQuery runs a query on both engines and requires identical results.
func (s *engineSession) agreeQuery(stmt string) {
	s.t.Helper()
	gCols, gRows, gErr, panicked, panicVal := tclSafeGoQuery(s.godb, stmt)
	if panicked {
		s.t.Fatalf("PANIC querying %q: %v", stmt, panicVal)
	}
	cCols, cRows, cErr := tclRunCGOQuery(s.cgodb, stmt)
	if gErr != nil || cErr != nil {
		s.t.Fatalf("query expected to agree failed\n  sql: %s\n  go err: %v\n  cgo err: %v", stmt, gErr, cErr)
	}
	if len(gCols) != len(cCols) {
		s.t.Fatalf("column count DIVERGES\n  sql: %s\n  go: %v\n  cgo: %v", stmt, gCols, cCols)
	}
	for i := range gCols {
		if gCols[i] != cCols[i] {
			s.t.Errorf("column name DIVERGES at %d\n  sql: %s\n  go: %q cgo: %q", i, stmt, gCols[i], cCols[i])
		}
	}
	if len(gRows) != len(cRows) {
		s.t.Fatalf("row count DIVERGES\n  sql: %s\n  go: %v\n  cgo: %v", stmt, gRows, cRows)
	}
	for i := range gRows {
		for j := range gRows[i] {
			if gRows[i][j] != cRows[i][j] {
				s.t.Errorf("cell DIVERGES at row %d col %d\n  sql: %s\n  go: %q cgo: %q", i, j, stmt, gRows[i][j], cRows[i][j])
			}
		}
	}
}

// declineQueryOK runs stmt as a query only on musql and requires it to
// decline -- the read-side never-wrong claim, paired with declineOK above.
func (s *engineSession) declineQueryOK(stmt string) {
	s.t.Helper()
	_, _, gErr, panicked, panicVal := tclSafeGoQuery(s.godb, stmt)
	if panicked {
		s.t.Fatalf("PANIC querying %q: %v", stmt, panicVal)
	}
	if gErr == nil {
		s.t.Errorf("this engine ANSWERS a query expected to stay declined\n  sql: %s", stmt)
	}
}

// TestFts5ExtDeleteWriteTimeCorrupt checks that a delete against an empty
// table correctly reports corruption rather than drifting.
func TestFts5ExtDeleteWriteTimeCorrupt(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE TABLE test (id INTEGER PRIMARY KEY, name TEXT, value TEXT)`)
	s.agreeExec(`CREATE VIRTUAL TABLE test_idx USING fts5(name, content=test, content_rowid=id)`)
	s.bothErr(`INSERT INTO test_idx (test_idx, rowid, name) VALUES('delete', 1, 'quick')`, "malformed")
}

// TestFts5ExtDeleteRedundantEngineDirect checks that redundant deletes
// allow index searches to still work correctly despite corrupted statistics.
func TestFts5ExtDeleteRedundantEngineDirect(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE TABLE test (id INTEGER PRIMARY KEY, name TEXT, value TEXT)`)
	s.agreeExec(`CREATE VIRTUAL TABLE test_idx USING fts5(name, content=test, content_rowid=id)`)
	s.agreeExec(`INSERT INTO test_idx(rowid, name) VALUES(123, 'one one one')`)
	s.agreeExec(`INSERT INTO test_idx(rowid, name) VALUES(124, 'two two two')`)
	s.agreeExec(`INSERT INTO test_idx(rowid, name) VALUES(125, 'two two two')`)
	s.agreeExec(`INSERT INTO test_idx (test_idx, rowid, name) VALUES('delete', 123, 'one')`)
	s.agreeExec(`INSERT INTO test_idx (test_idx, rowid, name) VALUES('delete', 123, 'one')`)
	s.agreeExec(`INSERT INTO test_idx (test_idx, rowid, name) VALUES('delete', 123, 'one')`)

	// Integrity check does not fail for external-content tables with statistics drift.
	s.agreeExec(`INSERT INTO test_idx(test_idx) VALUES('integrity-check')`)

	// Decline MATCH queries on tables without content table rows populated.
	s.declineQueryOK(`SELECT rowid FROM test_idx WHERE test_idx MATCH 'two' ORDER BY rowid`)

	// Rank queries with drifted statistics should error on the oracle.
	if _, err := s.cgodb.Exec(`SELECT rowid FROM test_idx WHERE test_idx MATCH 'two' ORDER BY rank`); err == nil {
		t.Fatalf("test fixture assumption wrong: oracle answers the rank query normally despite the drift")
	} else if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("test fixture assumption wrong: oracle's rank-query error is %q, not the expected corruption", err.Error())
	}
	s.declineQueryOK(`SELECT rowid FROM test_idx WHERE test_idx MATCH 'two' ORDER BY rank`)
}

// TestFts5ExtDeleteNoOverlapEngineDirect checks that deletes of non-existent
// tokens or rows cause drift without affecting search results.
func TestFts5ExtDeleteNoOverlapEngineDirect(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE VIRTUAL TABLE t1 USING fts5(a, b, content=x1)`)
	s.agreeExec(`CREATE TABLE x1(rowid INTEGER PRIMARY KEY, a, b)`)
	s.agreeExec(`INSERT INTO x1 VALUES (1, 'hello world', 'today xyz'), (2, 'not the day', 'crunch crumble and chomp'), (3, 'one', 'two')`)
	s.agreeExec(`INSERT INTO t1(t1) VALUES('rebuild')`)
	s.agreeExec(`INSERT INTO t1(t1, rank) VALUES('secure-delete', 1)`)

	// Delete a non-existent rowid.
	s.agreeExec(`INSERT INTO t1(t1, rowid, a, b) VALUES('delete', 4, 'nosuchtoken', '')`)
	s.agreeExec(`INSERT INTO t1(t1) VALUES('integrity-check')`)

	// Delete tokens not present in actual rows.
	s.agreeExec(`INSERT INTO t1(t1, rowid, a, b) VALUES('delete', 1, 'crunch', '')`)
	s.agreeExec(`INSERT INTO t1(t1, rowid, a, b) VALUES('delete', 3, 'crunch', '')`)
	s.agreeExec(`INSERT INTO t1(t1) VALUES('integrity-check')`)

	// Verify searches still find the original rows.
	s.agreeQuery(`SELECT rowid FROM t1 WHERE t1 MATCH 'hello' ORDER BY rowid`)
	s.agreeQuery(`SELECT rowid FROM t1 WHERE t1 MATCH 'one' ORDER BY rowid`)
	s.agreeQuery(`SELECT rowid FROM t1 WHERE t1 MATCH 'crunch' ORDER BY rowid`)
}

// TestFts5ExtDeleteMixedColumnDeclines checks that a delete covering only
// one column of a multi-column row is declined.
func TestFts5ExtDeleteMixedColumnDeclines(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`)
	s.agreeExec(`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`)
	s.agreeExec(`INSERT INTO src VALUES(1, 'alpha', 'beta')`)
	s.agreeExec(`INSERT INTO t(rowid, a, b) VALUES(1, 'alpha', 'beta')`)

	stmt := `INSERT INTO t(t, rowid, a, b) VALUES('delete', 1, 'alpha', '')`
	if !s.oracleAccepts(stmt) {
		t.Fatalf("test fixture assumption wrong: oracle refuses the mixed-column delete")
	}
	s.declineOK(stmt)
}

// TestFts5ExtDeletePartialWithinColumnDeclines checks that a delete naming
// only some terms within a single column is declined.
func TestFts5ExtDeletePartialWithinColumnDeclines(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE TABLE src(id INTEGER PRIMARY KEY, a)`)
	s.agreeExec(`CREATE VIRTUAL TABLE t USING fts5(a, content=src, content_rowid=id)`)
	s.agreeExec(`INSERT INTO src VALUES(1, 'one two three')`)
	s.agreeExec(`INSERT INTO t(rowid, a) VALUES(1, 'one two three')`)

	stmt := `INSERT INTO t(t, rowid, a) VALUES('delete', 1, 'one')`
	if !s.oracleAccepts(stmt) {
		t.Fatalf("test fixture assumption wrong: oracle refuses the partial-column delete")
	}
	s.declineOK(stmt)
}

// TestFts5ContentlessDeletePartialIsAGhost checks that partial deletes on
// contentless tables create ghost rows visible to searches but not scans.
func TestFts5ContentlessDeletePartialIsAGhost(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE VIRTUAL TABLE tt USING fts5(a, content="")`)
	s.agreeExec(`INSERT INTO tt VALUES('c d c g g f')`)
	s.agreeExec(`INSERT INTO tt VALUES('c d g b f d')`)
	s.agreeExec(`INSERT INTO tt VALUES('c c f d e d')`)
	s.agreeExec(`INSERT INTO tt(tt, rowid, a) VALUES('delete', 3, 'c d g b f d')`)

	s.agreeQuery(`SELECT rowid FROM tt ORDER BY rowid`)
	s.agreeQuery(`SELECT count(*) FROM tt`)
	s.agreeQuery(`SELECT id FROM tt_docsize ORDER BY id`)
	s.agreeQuery(`SELECT rowid FROM tt WHERE tt MATCH 'e' ORDER BY rowid`)
	s.agreeQuery(`SELECT rowid FROM tt WHERE tt MATCH 'c' ORDER BY rowid`)
	s.agreeQuery(`SELECT rowid FROM tt WHERE tt MATCH 'd AND e' ORDER BY rowid`)
	s.agreeQuery(`SELECT rowid FROM tt WHERE rowid=3`)

	// Verify engines agree on a clean table after delete-all.
	s.agreeExec(`INSERT INTO tt(tt) VALUES('delete-all')`)
	s.agreeExec(`INSERT INTO tt VALUES('x y z')`)
	s.agreeQuery(`SELECT rowid FROM tt WHERE tt MATCH 'x'`)
}

// TestFts5ContentlessDeletePrefixIndexStillDeclines checks that partial
// deletes on tables with prefix indices are declined.
func TestFts5ContentlessDeletePrefixIndexStillDeclines(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE VIRTUAL TABLE tt USING fts5(a, content='', prefix='2')`)
	s.agreeExec(`INSERT INTO tt VALUES('abc abd')`)
	s.agreeExec(`INSERT INTO tt VALUES('abx zz')`)
	stmt := `INSERT INTO tt(tt, rowid, a) VALUES('delete', 1, 'abc xyz')`
	if !s.oracleAccepts(stmt) {
		t.Fatalf("test fixture assumption wrong: oracle refuses this contentless delete")
	}
	_, rows, err := tclRunCGOQuery(s.cgodb, `SELECT rowid FROM tt WHERE tt MATCH 'ab*'`)
	if err != nil || len(rows) != 1 || rows[0][0] != "I:2" {
		t.Fatalf("test fixture assumption wrong: oracle's MATCH 'ab*' after the delete is %v (%v), wanted only rowid 2", rows, err)
	}
	s.declineOK(stmt)
}

// TestFts5ContentlessGhostFileFromOracle checks that ghosted files from the
// oracle are served or refused correctly based on statistics consistency.
func TestFts5ContentlessGhostFileFromOracle(t *testing.T) {
	for _, tc := range []struct {
		del   string
		serve bool
	}{
		{`INSERT INTO tt(tt, rowid, a) VALUES('delete', 3, 'c d g b f d')`, true},
		{`INSERT INTO tt(tt, rowid, a) VALUES('delete', 3, 'c d')`, false},
	} {
		path := filepath.Join(t.TempDir(), "c.db")
		cgodb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`CREATE VIRTUAL TABLE tt USING fts5(a, content="")`,
			`INSERT INTO tt VALUES('c d c g g f')`,
			`INSERT INTO tt VALUES('c d g b f d')`,
			`INSERT INTO tt VALUES('c c f d e d')`,
			tc.del,
		} {
			if _, err := cgodb.Exec(q); err != nil {
				t.Fatalf("oracle %s: %v", q, err)
			}
		}
		// Open the file through the import path.
		godb, err := engine.OpenWrite(importedForMusql(t, path))
		if err != nil {
			t.Fatalf("engine.OpenWrite: %v", err)
		}
		for _, q := range []string{
			`SELECT rowid FROM tt ORDER BY rowid`,
			`SELECT rowid FROM tt WHERE tt MATCH 'e' ORDER BY rowid`,
			`SELECT rowid FROM tt WHERE tt MATCH 'c' ORDER BY rowid`,
		} {
			_, gRows, gErr, panicked, pv := tclSafeGoQuery(godb, q)
			if panicked {
				t.Fatalf("PANIC %s: %v", q, pv)
			}
			if !tc.serve {
				if gErr == nil {
					t.Errorf("%s: this engine SERVES a ghost whose averages record drifted\n  sql: %s\n  got: %v", tc.del, q, gRows)
				}
				continue
			}
			_, cRows, cErr := tclRunCGOQuery(cgodb, q)
			if gErr != nil || cErr != nil {
				t.Fatalf("%s\n  go err: %v\n  cgo err: %v", q, gErr, cErr)
			}
			if fmt.Sprint(gRows) != fmt.Sprint(cRows) {
				t.Errorf("DIVERGES %s\n  go:  %v\n  cgo: %v", q, gRows, cRows)
			}
		}
		godb.Discard()
		cgodb.Close()
	}
}

// TestFts5ContentlessDeleteCountsDifferStillDeclines checks that deletes
// naming fewer tokens than a document holds are declined.
func TestFts5ContentlessDeleteCountsDifferStillDeclines(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE VIRTUAL TABLE tt USING fts5(a, content="")`)
	s.agreeExec(`INSERT INTO tt VALUES('c c f d e d')`)
	stmt := `INSERT INTO tt(tt, rowid, a) VALUES('delete', 1, 'c d')`
	if !s.oracleAccepts(stmt) {
		t.Fatalf("test fixture assumption wrong: oracle refuses this contentless delete")
	}
	s.declineOK(stmt)
}

// TestFts5ExtDeleteRankDriftDeclines checks that rank queries on tables with
// drifted statistics are declined.
func TestFts5ExtDeleteRankDriftDeclines(t *testing.T) {
	s := newEngineSession(t)
	s.agreeExec(`CREATE VIRTUAL TABLE t1 USING fts5(a, b, content=x1)`)
	s.agreeExec(`CREATE TABLE x1(rowid INTEGER PRIMARY KEY, a, b)`)
	s.agreeExec(`INSERT INTO x1 VALUES (1, 'hello world', 'today xyz'), (2, 'not the day', 'crunch crumble and chomp'), (3, 'one', 'two')`)
	s.agreeExec(`INSERT INTO t1(t1) VALUES('rebuild')`)
	s.agreeExec(`INSERT INTO t1(t1, rowid, a, b) VALUES('delete', 4, 'nosuchtoken', '')`)

	rankQuery := `SELECT rowid FROM t1 WHERE t1 MATCH 'hello' ORDER BY rank`
	if !s.oracleAccepts(rankQuery) {
		t.Fatalf("test fixture assumption wrong: the single no-op delete above already corrupts the oracle's rank query")
	}
	s.declineQueryOK(rankQuery)
}
