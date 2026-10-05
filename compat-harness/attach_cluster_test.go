// Tests ATTACH database routing and error rules.
package compat

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestAttachQuotedSchemaQualifierRoutes tests that schema qualifiers can be
// string literals, not just identifiers, for routing to attached databases.
func TestAttachQuotedSchemaQualifierRoutes(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE seed(v)")
	goKw, cgoKw := buildAuxPair(t, "kw", "CREATE TABLE seed(v)")
	p := newAttachPair(t, []string{goAux, goKw}, []string{cgoAux, cgoKw})

	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("ATTACH '{1}' AS 'ON'")

	// String-literal qualifier routes to attached database.
	p.agreeExec("CREATE TABLE 'aux'.t1(a, b, c)")
	p.agreeExec("INSERT INTO 'aux'.t1 VALUES(1, 2, 3)")
	p.agreeExec("UPDATE 'aux'.t1 SET c = 9 WHERE a = 1")
	p.agreeQuery("SELECT a, b, c FROM aux.t1")
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name = 't1'")

	// Reserved keyword as database name: only quoted form accepted.
	p.agreeExec("CREATE TABLE 'ON'.t2(x)")
	p.agreeExec("INSERT INTO 'ON'.t2 VALUES(7)")
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name = 't2'")

	// Bare keyword still rejected on both sides.
	if goErr, cgoErr := p.exec("CREATE TABLE ON.t3(x)"); goErr == nil || cgoErr == nil {
		t.Errorf("CREATE TABLE ON.t3(x): expected both engines to reject; go=%v cgo=%v", goErr, cgoErr)
	}
	// A TARGET name C SQLite refuses to parse must stay refused. Splicing the
	// qualifier out hands the delegated session a different statement, and this
	// engine's own table-name parser is more permissive than SQLite's, so the
	// same nonIdentifierKeywords gate binds the target too. Verified against
	// 3.53.3 with aux attached: "CREATE TABLE aux.ON(a,b,c)" and
	// "CREATE TABLE aux.ORDER(a)" are syntax errors, while the quoted
	// "aux.\"ON\"(a)" and the identifier-legal keywords "aux.KEY(a)" /
	// "aux.ABORT(a)" are accepted. alter.test's own case is the first of these.
	for _, q := range []string{
		"CREATE TABLE 'ON'.ON(a, b, c)",
		"CREATE TABLE aux.ON(a, b, c)",
		"CREATE TABLE aux.ORDER(a)",
	} {
		if goErr, cgoErr := p.exec(q); goErr == nil || cgoErr == nil {
			t.Errorf("%s: expected both engines to reject; go=%v cgo=%v", q, goErr, cgoErr)
		}
	}
	// ...while a keyword that IS a legal identifier still routes.
	p.agreeExec(`CREATE TABLE aux.KEY(a)`)
	p.agreeExec(`CREATE TABLE aux."ON"(a)`)
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name IN ('KEY', 'ON')")

	// ...and a quoted qualifier naming nothing is still an unknown database.
	if goErr, cgoErr := p.exec("CREATE TABLE 'nope'.t4(x)"); goErr == nil || cgoErr == nil {
		t.Errorf("CREATE TABLE 'nope'.t4(x): expected both engines to reject; go=%v cgo=%v", goErr, cgoErr)
	}
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master")
}

// TestTCLIsolateAttachRewrite tests ATTACH path isolation for tcl corpus.
func TestTCLIsolateAttachRewrite(t *testing.T) {
	dir := "/tmp/oracle-attach"
	for _, c := range []struct{ in, want string }{
		// Relative filenames isolated; memory databases untouched.
		{"ATTACH 'test2.db' AS aux", "ATTACH '/tmp/oracle-attach/test2.db' AS aux"},
		{"ATTACH DATABASE 'test.db2' AS two", "ATTACH DATABASE '/tmp/oracle-attach/test.db2' AS two"},
		{"attach\n  'test3.db'\n  as three", "attach\n  '/tmp/oracle-attach/test3.db'\n  as three"},
		{"ATTACH ':memory:' AS aux", "ATTACH ':memory:' AS aux"},
		{"ATTACH DATABASE '' AS aux", "ATTACH DATABASE '' AS aux"},
		// Absolute paths and URIs preserved or relocated selectively.
		{"ATTACH '/nodir/nofile.x' AS aux", "ATTACH '/nodir/nofile.x' AS aux"},
		{"ATTACH 'file:test.db2?vfs=tvfs2' AS aux", "ATTACH 'file:" + dir + "/test.db2?vfs=tvfs2' AS aux"},
		// Non-ATTACH or non-literal: untouched.
		{"DETACH DATABASE two", "DETACH DATABASE two"},
		{"ATTACH 'a'||'b' AS aux", "ATTACH '/tmp/oracle-attach/a'||'b' AS aux"},
		{"SELECT 'test2.db'", "SELECT 'test2.db'"},
	} {
		if got := tclIsolateAttach(c.in, dir); got != c.want {
			t.Errorf("tclIsolateAttach(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// TestTCLSegmentAttachIsolationCommits verifies that isolated attachment
// paths allow both engines to operate independently.
func TestTCLSegmentAttachIsolationCommits(t *testing.T) {
	const rel = "test.db2"
	goDir, cgoDir := t.TempDir(), t.TempDir()
	// The relative path below must never resolve against the SOURCE TREE, which
	// is where this package runs from -- including when tclIsolateAttach is
	// neutered to check that this test still fails without it. runTCLSegment
	// gives each segment its own working directory for the same reason.
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prevWD)
	p := newAttachPair(t, []string{goDir}, []string{cgoDir})

	for _, s := range []string{
		"ATTACH '" + rel + "' AS aux",
		"CREATE TABLE aux.t4(x, y)",
		"INSERT INTO aux.t4 VALUES('A', 'B')",
		"BEGIN",
		"COMMIT",
	} {
		goSQL, cgoSQL := tclIsolateAttach(s, goDir), tclIsolateAttach(s, cgoDir)
		if _, _, err := p.godb.ExecArgs(goSQL, nil); err != nil {
			t.Fatalf("engine rejected %q: %v", goSQL, err)
		}
		if _, err := p.cgo.Exec(cgoSQL); err != nil {
			t.Fatalf("C SQLite rejected %q: %v", cgoSQL, err)
		}
	}
	p.agreeQuery("SELECT x, y FROM aux.t4")

	// ...and the isolation is real: each engine wrote its OWN file, and neither
	// file is the other's.
	goFile, cgoFile := filepath.Join(goDir, rel), filepath.Join(cgoDir, rel)
	for _, f := range []string{goFile, cgoFile} {
		// The engine's file is a segment; C reads its export (pathForReader).
		db, err := sql.Open("sqlite3", pathForReader(t, "cgo", f))
		if err != nil {
			t.Fatalf("sql.Open(%s): %v", f, err)
		}
		var n int
		err = db.QueryRow("SELECT count(*) FROM t4").Scan(&n)
		db.Close()
		if err != nil {
			t.Fatalf("%s: reading back t4: %v", f, err)
		}
		if n != 1 {
			t.Errorf("%s: t4 has %d rows, want 1", f, n)
		}
	}
}

// TestTCLOracleHasDatabase tests that statements naming non-existent attached
// databases are rejected identically by both engines.
func TestTCLOracleHasDatabase(t *testing.T) {
	for _, c := range []struct{ stmt, want string }{
		{"DETACH DATABASE two", "two"},
		{"DETACH two", "two"},
		{"DETACH 'two'", "two"},
		{"DETACH [two]", "two"},
		{`DETACH "two"`, "two"},
		{"detach database  aux ;", "aux"},
		{"PRAGMA aux.integrity_check", "aux"},
		{"pragma aux.locking_mode = normal", "aux"},
		{"PRAGMA 'aux'.user_version", "aux"},
		{"VACUUM aux", "aux"},
		{"ANALYZE nosuch.t1", "nosuch"},
		{"REINDEX nosuch.t1", "nosuch"},
		// main and temp never claimed; no database name to extract.
		{"DETACH main", ""},
		{"DETACH temp", ""},
		{"PRAGMA main.journal_mode", ""},
		{"PRAGMA temp.default_cache_size", ""},
		{"PRAGMA journal_mode", ""},
		{"VACUUM", ""},
		{"VACUUM INTO 'x.db'", ""},
		{"VACUUM aux INTO 'x.db'", ""},
		{"ANALYZE t1", ""},
		{"REINDEX", ""},
		{"ATTACH 'x.db' AS aux", ""},
		{"SELECT 1", ""},
		{"INSERT INTO aux.t VALUES(1)", ""},
	} {
		if got := tclStatementDatabaseName(c.stmt); got != c.want {
			t.Errorf("tclStatementDatabaseName(%q) = %q, want %q", c.stmt, got, c.want)
		}
	}

	dir := t.TempDir()
	p := newAttachPair(t, nil, nil)
	if _, err := p.cgo.Exec("CREATE TABLE t1(a)"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DETACH DATABASE two",
		"DETACH aux",
		"PRAGMA aux.integrity_check",
		"PRAGMA aux.locking_mode = normal",
		"PRAGMA aux.default_cache_size = 10",
		"PRAGMA aux.journal_mode = PERSIST",
		"PRAGMA aux.synchronous",
		"VACUUM aux",
		"ANALYZE nosuch.t1",
		"REINDEX nosuch.t1",
	} {
		if tclOracleHasDatabase(p.cgo, tclStatementDatabaseName(q)) {
			t.Fatalf("%q: setup attached a database it must not have", q)
		}
		if _, err := p.cgo.Exec(q); err == nil {
			t.Errorf("%q: C SQLite ACCEPTED it with no such database attached -- the rule's premise is false", q)
		}
	}
	// The converse: once aux really IS attached the rule must not fire, so a Go
	// decline of one of these stays the visible coverage gap it is.
	if _, err := p.cgo.Exec("ATTACH '" + filepath.Join(dir, "a.db") + "' AS aux"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"PRAGMA aux.integrity_check", "VACUUM aux", "DETACH aux"} {
		if !tclOracleHasDatabase(p.cgo, tclStatementDatabaseName(q)) {
			t.Errorf("%q: aux IS attached, but the rule would still claim the oracle rejects it", q)
		}
	}
}

// TestAttachAutocommitWriteSurvivesRollback checks that rolling back a
// transaction on main leaves attached session writes intact.
func TestAttachAutocommitWriteSurvivesRollback(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "rb")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t4(a, b, c)")
	p.agreeExec("CREATE TABLE aux.t4(a, b, c)")
	p.agreeExec("INSERT INTO aux.t4 VALUES(7, 8, 9)")

	// Transaction on main only; attachment unaffected by rollback.
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO main.t4 VALUES(1, 2, 3)")
	p.agreeExec("ROLLBACK")

	// Autocommit writes to attachment persist.
	p.agreeExec("INSERT INTO aux.t4 VALUES(17, 18, 19)")
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")
	// Main's rollback still worked.
	p.agreeQuery("SELECT count(*) FROM main.t4")
}

// TestAttachDetachCommitsItsWrites checks that DETACH commits attached
// SQLite committed each of those statements when it ran, and a DETACH cannot
// unwrite them. Dropping the session left the file as it was before the
// attachment, so re-ATTACHing found an empty database.
//
// tkt1873.test's own sequence, and the second bug the shared attachment file
// had been hiding: with both engines pointed at ONE test2.db, the oracle's
// autocommit had really created t2 there, so this engine's re-ATTACH read back
// a table it had never written.
func TestAttachDetachCommitsItsWrites(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "det")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE aux.t2(x, y)")
	p.agreeExec("INSERT INTO aux.t2 VALUES(5, 6)")
	p.agreeExec("DETACH aux")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeQuery("SELECT x, y FROM aux.t2")

	// A ':memory:' attachment is the deliberate exception: DETACH discards that
	// database outright, so a re-ATTACH of the same name starts empty -- which
	// is what C SQLite does too.
	p.agreeExec("ATTACH ':memory:' AS mem")
	p.agreeExec("CREATE TABLE mem.m1(z)")
	p.agreeExec("DETACH mem")
	p.agreeExec("ATTACH ':memory:' AS mem")
	if goErr, cgoErr := p.exec("SELECT z FROM mem.m1"); goErr == nil || cgoErr == nil {
		t.Errorf("a re-ATTACHed ':memory:' database must start empty; go=%v cgo=%v", goErr, cgoErr)
	}
}
