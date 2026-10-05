// Differential gates for PRAGMA writable_schema=RESET rootpage reloading:
// the object's own root (case A, no-op reload), out-of-range root (case B,
// fails load), and page alias to another b-tree (case C).
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

// wsrpStep is one statement of a fixture with what is asserted about it.
type wsrpStep struct {
	sql           string
	query         bool   // run through READ path, compare full result; else exec only
	wantErrSubstr string // both engines must reject with this substring
	wantGoDecline string // this engine declines while oracle accepts (ends replay)
}

func TestWritableSchemaResetRootpageSelfRootIsServed(t *testing.T) {
	// corruptN.test 6.0, mined as corruptN.test#4 -- statement 8 of that
	// segment is the RESET, and it was the whole file's last unsupported
	// statement until this shape was served. "rootpage=3 WHERE rowid=2" names
	// t2's own root at this page size (both engines: t1|2 t2|3 t1tr|0), so
	// the reload re-derives the identical object from the identical page.
	wsrpRun(t, 4096, []wsrpStep{
		{sql: `PRAGMA auto_vacuum = 0`},
		{sql: `CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`},
		{sql: `INSERT INTO t1(b) VALUES(zeroblob(300)),(zeroblob(300)),(zeroblob(300)),(zeroblob(300))`},
		{sql: `CREATE TABLE t2(a)`},
		{sql: `CREATE TRIGGER t1tr BEFORE UPDATE ON t1 BEGIN DELETE FROM t2; END`},
		// Rootpage witness: query only valid for schema with no DROP/CREATE churn.
		{sql: `SELECT name, rootpage FROM sqlite_schema`, query: true},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET rootpage=3 WHERE rowid=2`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `SELECT count(*) FROM t2`, query: true},
		{sql: `SELECT count(*) FROM t1`, query: true},
		{sql: `SELECT name, rootpage FROM sqlite_schema`, query: true},
		{sql: `PRAGMA integrity_check`, query: true},
		{sql: `INSERT INTO t2 VALUES('active'),('boomer'),('atom'),('atomic'), ('alpha channel backup abandon test aback boomer atom alpha active')`},
		{sql: `SELECT count(*) FROM t2`, query: true},
		{sql: `SELECT a FROM t2 ORDER BY 1`, query: true},
	})
}

// TestWritableSchemaResetRootpageOutOfRangeIsServed tests out-of-range rootpage:
// C's answer depends on physical layout, which segment files don't have.
func TestWritableSchemaResetRootpageOutOfRangeIsServed(t *testing.T) {
	wsrpRun(t, 4096, []wsrpStep{
		{sql: `PRAGMA auto_vacuum = 0`},
		{sql: `CREATE TABLE t1(x)`},
		{sql: `WITH s(i) AS ( SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<500 ) INSERT INTO t1 SELECT zeroblob(300) FROM s`},
		{sql: `CREATE TABLE t2(y)`},
		{sql: `CREATE TRIGGER tr BEFORE UPDATE ON t1 BEGIN DELETE FROM t2; END`},
		{sql: `PRAGMA writable_schema = ON`},
		{sql: `UPDATE sqlite_schema SET rootpage = 137 WHERE name='t2'`},
		{sql: `PRAGMA writable_schema = RESET`, wantGoDecline: "not reproducible against C SQLite"},
	})
}

func TestWritableSchemaResetRootpageAliasIsServed(t *testing.T) {
	// Autoindex row aliased to a sibling's b-tree: the schema load fails with
	// sqlite3IndexHasDuplicateRootPage, matching C SQLite's behavior.
	wsrpRun(t, 4096, []wsrpStep{
		{sql: `PRAGMA auto_vacuum = 0`},
		{sql: `CREATE TABLE x1(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)`},
		{sql: `INSERT INTO x1 VALUES(1, 1, 2)`},
		{sql: `INSERT INTO x1 VALUES(2, 2, 3)`},
		{sql: `INSERT INTO x1 VALUES(3, 3, 4)`},
		{sql: `INSERT INTO x1 VALUES(4, 5, 6)`},
		{sql: `PRAGMA writable_schema = 1`},
		{sql: `UPDATE sqlite_schema SET rootpage = (SELECT rootpage FROM sqlite_schema WHERE name = 'sqlite_autoindex_x1_2') WHERE name = 'sqlite_autoindex_x1_1'`},
		{sql: `PRAGMA writable_schema = RESET`},
		{sql: `SELECT count(*) FROM x1`, query: true, wantErrSubstr: "malformed database schema (sqlite_autoindex_x1_2) - invalid rootpage"},
		{sql: `INSERT INTO x1 VALUES(9, 9, 9)`, wantErrSubstr: "malformed database schema (sqlite_autoindex_x1_2) - invalid rootpage"},
		{sql: `PRAGMA writable_schema = ON`},
		{sql: `SELECT count(*) FROM x1`, query: true},
	})
}

func TestWritableSchemaResetRootpageTableAliasIsServed(t *testing.T) {
	// Table aliased to an interior page of another table's tree at page size 1024.
	// C's layout decides; segment files have no physical layout, so RESET declines.
	wsrpRun(t, 1024, []wsrpStep{
		{sql: `PRAGMA page_size = 1024`},
		{sql: `PRAGMA auto_vacuum = 0`},
		{sql: `CREATE TABLE t1(x)`},
		{sql: `WITH s(i) AS ( SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<500 ) INSERT INTO t1 SELECT zeroblob(300) FROM s`},
		{sql: `CREATE TABLE t2(y)`},
		{sql: `CREATE TRIGGER tr BEFORE UPDATE ON t1 BEGIN DELETE FROM t2; END`},
		{sql: `PRAGMA writable_schema = ON`},
		{sql: `UPDATE sqlite_schema SET rootpage = 137 WHERE name='t2'`},
		{sql: `PRAGMA writable_schema = RESET`, wantGoDecline: "not reproducible against C SQLite"},
	})
}

// wsrpRun replays steps against a fresh engine.DB and a fresh cgo connection
// in lockstep, asserting agreement at every step.
func wsrpRun(t *testing.T, pageSize int, steps []wsrpStep) {
	t.Helper()
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1) // writable_schema is per-CONNECTION state
	if pageSize != 4096 {
		if _, err := cgodb.Exec("PRAGMA page_size=" + itoaWSRP(pageSize)); err != nil {
			t.Fatal(err)
		}
	}

	for i, st := range steps {
		gotCols, gotRows, gerr := wsrpOne(godb, st, true)
		cgoCols, cgoRows, cerr := wsrpOne2(cgodb, st)
		switch {
		case st.wantGoDecline != "":
			if gerr == nil {
				t.Fatalf("step %d %q: this engine ACCEPTED it -- this is a RECORDED DECLINE. If the mechanism it stands in for has been built, replace this step with the positive assertions; if not, this is a regression", i, st.sql)
			}
			if cerr != nil {
				t.Fatalf("step %d %q: C SQLite REJECTED it (%v) -- the decline's premise (a shape the oracle answers) has moved", i, st.sql, cerr)
			}
			if !strings.Contains(gerr.Error(), st.wantGoDecline) {
				t.Fatalf("step %d %q: this engine's decline is not the one recorded:\n  got:  %v\n  want a message containing: %q", i, st.sql, gerr, st.wantGoDecline)
			}
			return // out of lockstep from here on; see wsrpStep.wantGoDecline
		case st.wantErrSubstr != "":
			if gerr == nil {
				t.Fatalf("step %d %q: this engine ACCEPTED it; C SQLite rejects it (%v)", i, st.sql, cerr)
			}
			if cerr == nil {
				t.Fatalf("step %d %q: C SQLite ACCEPTED it, this engine rejected it (%v) -- the fixture's recorded oracle behaviour has moved", i, st.sql, gerr)
			}
			if !strings.Contains(gerr.Error(), st.wantErrSubstr) {
				t.Fatalf("step %d %q: this engine's error is not the one recorded:\n  got:  %v\n  want a message containing: %q", i, st.sql, gerr, st.wantErrSubstr)
			}
			if !strings.Contains(cerr.Error(), st.wantErrSubstr) {
				t.Fatalf("step %d %q: C SQLite's error is not the one recorded:\n  got:  %v\n  want a message containing: %q", i, st.sql, cerr, st.wantErrSubstr)
			}
			continue
		default:
			if gerr != nil {
				t.Fatalf("step %d %q: this engine REJECTED it: %v (C SQLite: %v)", i, st.sql, gerr, cerr)
			}
			if cerr != nil {
				t.Fatalf("step %d %q: C SQLite REJECTED it: %v -- this shape is a mutual rejection, not a served one", i, st.sql, cerr)
			}
		}
		if !st.query {
			continue
		}
		if got, want := wsReloadAnswer(gotCols, gotRows, nil), wsReloadAnswer(cgoCols, cgoRows, nil); got != want {
			t.Fatalf("step %d %q DIVERGES:\n  this engine: %s\n  C SQLite: %s", i, st.sql, got, want)
		}
	}
}

// wsrpOne runs one step against this engine. Unlike wsGoRun (whose wsIsQuery
// reads the leading keyword) a step says for itself which path it takes, so a
// PRAGMA that returns rows is compared as the query it is.
func wsrpOne(godb *engine.Session, st wsrpStep, _ bool) (cols []string, rows [][]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errorfWSRP("PANIC: %v", r)
		}
	}()
	if !st.query {
		_, _, e := godb.ExecArgs(st.sql, nil)
		return nil, nil, e
	}
	p, e := godb.SnapshotPager()
	if e != nil {
		return nil, nil, e
	}
	defer p.Close()
	c, v, e := p.QueryArgs(st.sql, nil)
	if e != nil {
		return nil, nil, e
	}
	out := make([][]string, len(v))
	for i, r := range v {
		cells := make([]string, len(r))
		for j, val := range r {
			cells[j] = normalizeEngineValue(val)
		}
		out[i] = cells
	}
	return c, out, nil
}

func wsrpOne2(cgodb *sql.DB, st wsrpStep) ([]string, [][]string, error) {
	if !st.query {
		_, err := cgodb.Exec(st.sql)
		return nil, nil, err
	}
	return tclRunCGOQuery(cgodb, st.sql)
}

func itoaWSRP(n int) string { return strconv.Itoa(n) }

func errorfWSRP(format string, a ...any) error { return fmt.Errorf(format, a...) }

// wsReloadAnswer flattens a step's result -- rows, or the error -- into one
// comparable string. An ERROR is a legitimate answer: one of the fixtures
// leaves the schema so corrupt that reading it back at all is "malformed
// database schema".
func wsReloadAnswer(cols []string, rows [][]string, err error) string {
	if err != nil {
		return "ERROR: " + err.Error()
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "cols=%v rows=[", cols)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(" ")
		}
		fmt.Fprintf(&sb, "{%s}", strings.Join(r, "|"))
	}
	sb.WriteString("]")
	return sb.String()
}
