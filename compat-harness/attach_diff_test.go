// Tests ATTACH/DETACH statements against the C SQLite oracle.
// Both engines must accept or both reject, except for cross-database writes
// which the engine deliberately doesn't implement. Each engine uses its own
// attached files to avoid testing the filesystem.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// attachPair is one differential session: a pure-Go engine write session and a
// cgo connection over their own main files, plus a per-side path substitution
// so the identical statement TEXT can name each side's own attached file.
type attachPair struct {
	t    *testing.T
	godb *engine.Session
	cgo  *sql.DB
	// paths[i] is substituted for "{i}" in a statement, per side.
	goPaths  []string
	cgoPaths []string
}

// stmt renders a statement template for one side: "{0}", "{1}", ... are
// replaced by that side's own file paths.
func (p *attachPair) stmt(tmpl string, paths []string) string {
	out := tmpl
	for i, path := range paths {
		out = strings.ReplaceAll(out, fmt.Sprintf("{%d}", i), path)
	}
	return out
}

func newAttachPair(t *testing.T, goPaths, cgoPaths []string) *attachPair {
	t.Helper()
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go-main.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { godb.Discard() })
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo-main.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	// One logical connection: attached set is connection-scoped.
	cgodb.SetMaxOpenConns(1)
	t.Cleanup(func() { cgodb.Close() })
	return &attachPair{t: t, godb: godb, cgo: cgodb, goPaths: goPaths, cgoPaths: cgoPaths}
}

// exec runs one statement on both sides and returns each side's error.
func (p *attachPair) exec(tmpl string) (goErr, cgoErr error) {
	p.t.Helper()
	_, _, goErr = p.godb.ExecArgs(p.stmt(tmpl, p.goPaths), nil)
	_, cgoErr = p.cgo.Exec(p.stmt(tmpl, p.cgoPaths))
	return
}

// agreeExec requires both engines to accept, or both to reject, one
// non-query statement.
func (p *attachPair) agreeExec(tmpl string) {
	p.t.Helper()
	goErr, cgoErr := p.exec(tmpl)
	switch {
	case goErr == nil && cgoErr != nil:
		p.t.Errorf("%q: engine ACCEPTED, C SQLite rejected: %v", tmpl, cgoErr)
	case goErr != nil && cgoErr == nil:
		p.t.Errorf("%q: engine rejected (%v), C SQLite ACCEPTED", tmpl, goErr)
	}
}

// declineExec requires the engine to reject a deliberately unimplemented shape.
func (p *attachPair) declineExec(tmpl string) {
	p.t.Helper()
	goSQL := p.stmt(tmpl, p.goPaths)
	if _, _, err := p.godb.ExecArgs(goSQL, nil); err == nil {
		p.t.Errorf("%q: engine ACCEPTED a statement this write path must decline", goSQL)
	}
	// The oracle side is deliberately not run; the databases stay in lockstep.
}

// query runs one query on both sides, returning normalized results.
func (p *attachPair) query(tmpl string) (goCols []string, goRows [][]string, goErr error, cgoCols []string, cgoRows [][]string, cgoErr error) {
	p.t.Helper()
	pager, err := p.godb.SnapshotPager()
	if err != nil {
		goErr = err
	} else {
		defer pager.Close()
		cols, vals, qerr := pager.QueryArgs(p.stmt(tmpl, p.goPaths), nil)
		if qerr != nil {
			goErr = qerr
		} else {
			goCols = cols
			goRows = make([][]string, len(vals))
			for i, row := range vals {
				cells := make([]string, len(row))
				for j, v := range row {
					cells[j] = normalizeEngineValue(v)
				}
				goRows[i] = cells
			}
		}
	}
	cgoCols, cgoRows, cgoErr = tclRunCGOQuery(p.cgo, p.stmt(tmpl, p.cgoPaths))
	return
}

// agreeQuery requires both engines to reject the query, or both to accept it
// with identical column names and rows (order-sensitive: every query here
// carries its own ORDER BY, or returns a single row).
func (p *attachPair) agreeQuery(tmpl string) {
	p.t.Helper()
	goCols, goRows, goErr, cgoCols, cgoRows, cgoErr := p.query(tmpl)
	switch {
	case goErr != nil && cgoErr != nil:
		return
	case goErr != nil:
		p.t.Errorf("%q: engine rejected (%v), C SQLite returned cols=%v rows=%v", tmpl, goErr, cgoCols, cgoRows)
		return
	case cgoErr != nil:
		p.t.Errorf("%q: engine ACCEPTED (cols=%v rows=%v), C SQLite rejected: %v", tmpl, goCols, goRows, cgoErr)
		return
	}
	if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
		p.t.Errorf("%q: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", tmpl, reason, goCols, goRows, cgoCols, cgoRows)
	}
}

// buildAuxPair creates two byte-identical auxiliary databases -- one per engine
// -- by running the same setup statements against each with its OWN writer, and
// returns the two paths. This is what lets the differential body below use one
// statement template for both sides.
func buildAuxPair(t *testing.T, tag string, setup ...string) (goPath, cgoPath string) {
	t.Helper()
	dir := t.TempDir()
	goPath = filepath.Join(dir, "go-"+tag+".db")
	cgoPath = filepath.Join(dir, "cgo-"+tag+".db")

	godb, err := engine.Create(goPath)
	if err != nil {
		t.Fatalf("engine.Create(%s): %v", goPath, err)
	}
	for _, s := range setup {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			godb.Discard()
			t.Fatalf("engine aux setup %q: %v", s, err)
		}
	}
	if err := godb.Close(); err != nil {
		t.Fatalf("engine aux Close: %v", err)
	}

	cgodb, err := sql.Open("sqlite3", cgoPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s): %v", cgoPath, err)
	}
	defer cgodb.Close()
	for _, s := range setup {
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo aux setup %q: %v", s, err)
		}
	}
	return goPath, cgoPath
}

// TestEngineAttachDetachStatements pins the ATTACH/DETACH statement surface
// itself: which forms are accepted, and -- the part that actually matters --
// that every rejection is one C SQLite makes too.
func TestEngineAttachDetachStatements(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux")
	goAux2, cgoAux2 := buildAuxPair(t, "aux2")
	p := newAttachPair(t, []string{goAux, goAux2}, []string{cgoAux, cgoAux2})

	// Both spellings of the statement, and every quoting of the name.
	p.agreeExec("ATTACH DATABASE '{0}' AS aux")
	p.agreeExec("DETACH DATABASE aux")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("DETACH aux")
	p.agreeExec("ATTACH '{0}' AS 'aux'")
	p.agreeExec("DETACH 'aux'")
	p.agreeExec("ATTACH '{0}' AS \"aux\"")
	p.agreeExec("DETACH \"aux\"")
	p.agreeExec("ATTACH '{0}' AS [aux]")
	p.agreeExec("DETACH [aux]")

	// Name is an expression; unquoted keywords are syntax errors but quoted ones work.
	p.agreeExec("ATTACH '{0}' AS ON")
	p.agreeExec("ATTACH '{0}' AS SELECT")
	p.agreeExec("ATTACH '{0}' AS 'on'")
	p.agreeExec("DETACH 'on'")

	// A live name cannot be reused, and neither can main/temp.
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("ATTACH '{1}' AS aux")    // already in use
	p.agreeExec("ATTACH '{1}' AS main")   // already in use
	p.agreeExec("ATTACH '{1}' AS temp")   // already in use
	p.agreeExec("ATTACH '{1}' AS AUX")    // case-insensitive
	p.agreeExec("ATTACH '{0}' AS auxdup") // same file under a second name

	// DETACH's three outcomes.
	p.agreeExec("DETACH auxdup")
	p.agreeExec("DETACH nosuchdatabase") // no such database
	p.agreeExec("DETACH main")           // cannot detach
	p.agreeExec("DETACH temp")           // no such database (temp is not attached)
	p.agreeExec("DETACH AUX")            // case-insensitive
	p.agreeExec("DETACH aux")            // already gone: no such database

	// SQLITE_MAX_ATTACHED: the 11th attachment fails on both.
	for i := 0; i < 12; i++ {
		p.agreeExec(fmt.Sprintf("ATTACH '{0}' AS lim%d", i))
	}
}

// TestEngineAttachMemory covers ATTACH ':memory:' -- a private, non-durable
// database that DETACH throws away, so a re-ATTACH of the same name starts
// empty again.
func TestEngineAttachMemory(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("ATTACH ':memory:' AS m1")
	p.agreeQuery("SELECT count(*) FROM m1.sqlite_master")
	p.agreeExec("DETACH m1")
	p.agreeExec("ATTACH ':memory:' AS m1")
	p.agreeQuery("SELECT count(*) FROM m1.sqlite_master")
	p.agreeExec("DETACH m1")
}

// TestEngineAttachRead is the read half: with an attached database that
// actually has content, schema-qualified reads, joins across the two files,
// and SQLite's unqualified search order (main shadows an attached same-named
// table) must all agree with the oracle.
func TestEngineAttachRead(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO t VALUES(1,'aux-one'),(2,'aux-two')",
		"CREATE TABLE shadowed(v TEXT)",
		"INSERT INTO shadowed VALUES('from-aux')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	p.agreeExec("CREATE TABLE m(id INTEGER PRIMARY KEY, tag TEXT)")
	p.agreeExec("INSERT INTO m VALUES(1,'main-one'),(3,'main-three')")
	p.agreeExec("CREATE TABLE shadowed(v TEXT)")
	p.agreeExec("INSERT INTO shadowed VALUES('from-main')")
	p.agreeExec("ATTACH '{0}' AS aux")

	// Schema-qualified reads of the attached database.
	p.agreeQuery("SELECT a, b FROM aux.t ORDER BY a")
	p.agreeQuery("SELECT count(*) FROM aux.t")
	p.agreeQuery("SELECT count(*) FROM aux.sqlite_master")
	p.agreeQuery("SELECT type, name FROM aux.sqlite_master ORDER BY name")

	// An unqualified name main does not have resolves in the attached
	// database; one main DOES have is shadowed by main.
	p.agreeQuery("SELECT a, b FROM t ORDER BY a")
	p.agreeQuery("SELECT v FROM shadowed ORDER BY v")
	p.agreeQuery("SELECT v FROM aux.shadowed ORDER BY v")
	p.agreeQuery("SELECT v FROM main.shadowed ORDER BY v")

	// A join spanning both files, and a correlated subquery reading across.
	p.agreeQuery("SELECT m.id, m.tag, aux.t.b FROM m JOIN aux.t ON aux.t.a = m.id ORDER BY m.id")
	p.agreeQuery("SELECT id, (SELECT b FROM aux.t WHERE a = m.id) FROM m ORDER BY id")
	p.agreeQuery("SELECT id FROM m WHERE id IN (SELECT a FROM aux.t) ORDER BY id")

	// An UNQUALIFIED schema catalog is always THIS database's, never an
	// attached one's -- the catalog is not a row-store table, so the search
	// order's "does the primary own this name" test has to name it explicitly
	// or the query silently reads the wrong database's schema.
	p.agreeQuery("SELECT type, name FROM sqlite_master ORDER BY name")
	p.agreeQuery("SELECT count(*) FROM sqlite_master")
	p.agreeQuery("SELECT name FROM sqlite_master WHERE name='t'")

	// A qualifier naming nothing must still be rejected on both sides.
	p.agreeQuery("SELECT * FROM nosuchdb.t")

	// Reading a detached database is an error again.
	p.agreeExec("DETACH aux")
	p.agreeQuery("SELECT a FROM aux.t ORDER BY a")
	p.agreeQuery("SELECT a, b FROM t ORDER BY a") // aux.t no longer in the search path
}

// TestEngineAttachCrossDatabaseWrite tests writes to attached databases.
// Statements that still decline name a database rather than an object.
func TestEngineAttachCrossDatabaseWrite(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO t VALUES(1,'aux-one')",
		"CREATE INDEX ix ON t(b)",
		"CREATE VIEW vv AS SELECT a FROM t",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE m(id INTEGER PRIMARY KEY)")
	p.agreeExec("ATTACH '{0}' AS aux")

	// DDL into the attachment.
	p.agreeExec("CREATE TABLE aux.newtab(x)")
	p.agreeExec("CREATE TABLE IF NOT EXISTS aux.newtab(x)")
	p.agreeExec("CREATE TABLE aux.ctas AS SELECT 1 AS x")
	p.agreeExec("CREATE INDEX aux.i2 ON newtab(x)")
	p.agreeExec("CREATE VIEW aux.v AS SELECT 1 AS one")
	p.agreeQuery("SELECT type, name FROM aux.sqlite_master ORDER BY name")

	// DML into the attachment with verification reads.
	p.agreeExec("INSERT INTO aux.t VALUES(2,'two')")
	p.agreeQuery("SELECT a, b FROM aux.t ORDER BY a")
	p.agreeExec("REPLACE INTO aux.t VALUES(2,'two-replaced')")
	p.agreeQuery("SELECT a, b FROM aux.t ORDER BY a")
	p.agreeExec("UPDATE aux.t SET b='updated' WHERE a=1")
	p.agreeQuery("SELECT a, b FROM aux.t ORDER BY a")
	p.agreeExec("DELETE FROM aux.t WHERE a=2")
	p.agreeQuery("SELECT a, b FROM aux.t ORDER BY a")
	p.agreeQuery("SELECT x FROM aux.newtab")
	p.agreeQuery("SELECT one FROM aux.v")

	// main is untouched throughout -- exactly one file per statement moved.
	p.agreeQuery("SELECT type, name FROM sqlite_master ORDER BY name")
	p.agreeQuery("SELECT count(*) FROM m")

	// ALTER and DROP into the attachment.
	p.agreeExec("ALTER TABLE aux.t ADD COLUMN c")
	p.agreeQuery("SELECT a, b, c FROM aux.t ORDER BY a")
	// Drop the view before RENAME since renaming a table a view references is declined.
	p.agreeExec("DROP VIEW aux.vv")
	p.agreeExec("ALTER TABLE aux.t RENAME TO t2")
	p.agreeQuery("SELECT a, b FROM aux.t2 ORDER BY a")
	p.agreeExec("DROP INDEX aux.ix")
	p.agreeExec("DROP TABLE aux.newtab")
	p.agreeExec("DROP TABLE IF EXISTS aux.nosuch")
	p.agreeExec("DROP TRIGGER IF EXISTS aux.notrigger")
	p.agreeQuery("SELECT type, name FROM aux.sqlite_master ORDER BY name")

	// ANALYZE aux is now supported on attached databases.
	p.agreeExec("ANALYZE aux")
	p.agreeQuery("SELECT tbl, idx, stat FROM aux.sqlite_stat1 ORDER BY tbl, idx")
	// The bare ANALYZE form covers all attachments.
	p.agreeExec("ANALYZE")
	p.agreeQuery("SELECT tbl, idx, stat FROM aux.sqlite_stat1 ORDER BY tbl, idx")
	p.agreeQuery("SELECT count(*) FROM sqlite_master WHERE name='sqlite_stat1'")

	// Still out of scope: REINDEX names a database, not an object.
	p.declineExec("REINDEX aux")
	// VACUUM aux is now supported; see TestEngineVacuumAttached for details.
}

// TestEngineAttachCrossDatabaseSource tests writes to the main database with reads from attached databases.
func TestEngineAttachCrossDatabaseSource(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE src(a INTEGER, b TEXT)",
		"INSERT INTO src VALUES(1,'one'),(2,'two'),(3,'three')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE dst(a INTEGER, b TEXT)")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("INSERT INTO dst SELECT a, b FROM aux.src WHERE a < 3")
	p.agreeQuery("SELECT a, b FROM dst ORDER BY a")
	p.agreeExec("UPDATE dst SET b = (SELECT b FROM aux.src WHERE aux.src.a = dst.a + 1)")
	p.agreeQuery("SELECT a, b FROM dst ORDER BY a")
	p.agreeExec("DELETE FROM dst WHERE a IN (SELECT a FROM aux.src WHERE b = 'one')")
	p.agreeQuery("SELECT a, b FROM dst ORDER BY a")
	p.agreeQuery("SELECT count(*) FROM aux.src") // the source is untouched
}

// TestEngineAttachReattachOrder tests that statements are recompiled when the attached set changes.
func TestEngineAttachReattachOrder(t *testing.T) {
	goA, cgoA := buildAuxPair(t, "a",
		"CREATE TABLE s(a INTEGER)",
		"INSERT INTO s VALUES(1),(2)",
	)
	goB, cgoB := buildAuxPair(t, "b",
		"CREATE TABLE s(a INTEGER)",
		"INSERT INTO s VALUES(30),(40)",
	)
	p := newAttachPair(t, []string{goA, goB}, []string{cgoA, cgoB})
	p.agreeExec("CREATE TABLE dst(a INTEGER)")
	p.agreeExec("ATTACH '{0}' AS one")
	p.agreeExec("ATTACH '{1}' AS two")

	// Compile the statement while "two" is the SECOND attachment...
	p.agreeExec("INSERT INTO dst SELECT a FROM two.s")
	p.agreeQuery("SELECT a FROM dst ORDER BY a")

	// ...then remove the first one, so "two" moves into slot 1, and run the
	// IDENTICAL text again. A stale cached program would read the wrong file
	// or index past the end of the slice.
	p.agreeExec("DETACH one")
	p.agreeExec("INSERT INTO dst SELECT a FROM two.s")
	p.agreeQuery("SELECT a FROM dst ORDER BY a")

	// And with nothing attached at all it must simply fail on both sides.
	p.agreeExec("DETACH two")
	p.agreeExec("INSERT INTO dst SELECT a FROM two.s")
	p.agreeQuery("SELECT a FROM dst ORDER BY a")
}

// TestEngineAttachInTransaction tests ATTACH/DETACH inside transactions.
// ATTACH is not undone by ROLLBACK. DETACH is refused for databases touched in the transaction.
func TestEngineAttachInTransaction(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE s(a INTEGER)",
		"INSERT INTO s VALUES(7)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE dst(a INTEGER)")

	p.agreeExec("BEGIN")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeQuery("SELECT a FROM aux.s ORDER BY a")
	p.agreeExec("INSERT INTO dst SELECT a FROM aux.s")
	// Both engines reject this: transaction read aux so it is locked.
	p.agreeExec("DETACH aux")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT a FROM dst ORDER BY a")

	// A transaction that touched only MAIN leaves aux free. ROLLBACK undoes writes only.
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO dst VALUES(99)")
	p.agreeExec("DETACH aux")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT a FROM dst ORDER BY a")
	p.agreeExec("DETACH aux") // already detached: no such database, on both
}

// TestEngineAttachDeclinedForms tests ATTACH forms that the engine deliberately rejects.
func TestEngineAttachDeclinedForms(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.declineExec("ATTACH 'fo' || 'o.d' || t.c AS ex")   // path expression needing a ROW
	p.declineExec("ATTACH '{0}' AS 'e' || 'x'")          // expression NAME (only the path is evaluated)
	p.declineExec("ATTACH '{0}' AS k KEY 'secret'")      // KEY clause
	p.declineExec("ATTACH 'file:uri.db?vfs=tvfs2' AS u") // URI parameter outside the bounded set
	p.declineExec("ATTACH '{0}'")                        // no AS
	p.declineExec("DETACH")                              // no name
	p.declineExec("DETACH aux extra")                    // trailing tokens
}
