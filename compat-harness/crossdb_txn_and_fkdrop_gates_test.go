// This file tests cross-database write transactions and DROP TABLE foreign key handling.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// auxUserVersion reads the user_version from an attached database's file.
func auxUserVersion(t *testing.T, goPath, cgoPath string) (int64, int64) {
	t.Helper()
	rp, err := engine.Open(goPath)
	if err != nil {
		t.Fatalf("engine.Open(%s): %v", goPath, err)
	}
	defer rp.Close()
	_, rows, err := rp.QueryArgs("PRAGMA user_version", nil)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("engine PRAGMA user_version on %s: rows=%v err=%v", goPath, rows, err)
	}
	cdb, err := sql.Open("sqlite3", cgoPath)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", cgoPath, err)
	}
	defer cdb.Close()
	var cv int64
	if err := cdb.QueryRow("PRAGMA user_version").Scan(&cv); err != nil {
		t.Fatalf("cgo PRAGMA user_version on %s: %v", cgoPath, err)
	}
	return rows[0][0].I, cv
}

// wantAuxUserVersion verifies both sides have matching user_version values.
func wantAuxUserVersion(t *testing.T, goPath, cgoPath string, want int64, when string) {
	t.Helper()
	gv, cv := auxUserVersion(t, goPath, cgoPath)
	if gv != want || cv != want {
		t.Errorf("aux user_version %s: engine=%d cgo=%d, want %d", when, gv, cv, want)
	}
}

// TestCrossDatabaseWriteInTransaction tests that cross-database writes in
// transactions commit or rollback correctly with the main database.
func TestCrossDatabaseWriteInTransaction(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "txn")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t4(a, b, c)")
	p.agreeExec("CREATE TABLE aux.t4(a, b, c)")
	// Autocommit, i.e. already committed by C SQLite when it ran. Nothing
	// below may take these back.
	p.agreeExec("INSERT INTO aux.t4 VALUES(7, 8, 9)")
	p.agreeExec("PRAGMA aux.user_version = 3")

	// ---- COMMIT keeps the cross-database write ----
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO main.t4 VALUES(1, 2, 3)")
	p.agreeExec("INSERT INTO aux.t4 VALUES(4, 5, 6)")
	p.agreeExec("CREATE TABLE aux.committed(x)")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")
	p.agreeQuery("SELECT count(*) FROM aux.sqlite_master WHERE name = 'committed'")
	p.agreeQuery("SELECT a, b, c FROM main.t4 ORDER BY a")

	// ---- ROLLBACK undoes it, in the same breath as main's own ----
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO main.t4 VALUES(21, 22, 23)")
	p.agreeExec("INSERT INTO aux.t4 VALUES(24, 25, 26)")
	p.agreeExec("CREATE TABLE aux.rolledback(x)")
	p.agreeExec("PRAGMA aux.user_version = 10")
	// ...and the write is VISIBLE to a read inside the transaction that made it.
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")
	p.agreeExec("ROLLBACK")

	// This is the gate: every one of those four must be gone, and the
	// autocommit row and user_version from before the transaction must not be.
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")
	p.agreeQuery("SELECT count(*) FROM aux.sqlite_master WHERE name = 'rolledback'")
	p.agreeQuery("SELECT count(*) FROM aux.sqlite_master WHERE name = 'committed'")
	p.agreeQuery("SELECT a, b, c FROM main.t4 ORDER BY a")
	// Verify the rolled-back pragma is undone.
	wantAuxUserVersion(t, goAux, cgoAux, 3, "after ROLLBACK")

	// Verify the attachment is still writable after rollback.
	p.agreeExec("INSERT INTO aux.t4 VALUES(31, 32, 33)")
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")

	// Test SAVEPOINT and ROLLBACK TO with cross-database writes.
	p.agreeExec("BEGIN")
	p.agreeExec("SAVEPOINT s1")
	p.agreeExec("INSERT INTO aux.t4 VALUES(41, 42, 43)")
	p.agreeExec("PRAGMA aux.user_version = 99")
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")
	p.agreeExec("ROLLBACK TO s1")
	// Verify the rolled-back savepoint is undone.
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT a, b, c FROM aux.t4 ORDER BY a")
	wantAuxUserVersion(t, goAux, cgoAux, 3, "after the rolled-back savepoint writes")
}

// TestDropTableUnresolvableForeignKey tests DROP TABLE behavior with
// unresolvable foreign keys and missing parent tables.
func TestDropTableUnresolvableForeignKey(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("PRAGMA foreign_keys = ON")

	// Unresolvable parent keys are ignored during DROP.
	for _, child := range []string{
		"CREATE TABLE c1(a, b, FOREIGN KEY(a,b) REFERENCES p)",
		"CREATE TABLE c1(a, b, FOREIGN KEY(a,b) REFERENCES p ON DELETE CASCADE)",
		"CREATE TABLE c1(a, b, FOREIGN KEY(a,b) REFERENCES p ON DELETE SET NULL)",
	} {
		p.agreeExec("PRAGMA foreign_keys = OFF")
		p.agreeExec("CREATE TABLE p(x PRIMARY KEY)")
		p.agreeExec(child)
		p.agreeExec("INSERT INTO p VALUES(1)")
		p.agreeExec("INSERT INTO c1 VALUES(1, 2)")
		p.agreeExec("PRAGMA foreign_keys = ON")
		p.agreeExec("DROP TABLE p")
		p.agreeQuery("SELECT a, b FROM c1 ORDER BY a")
		p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name = 'p'")
		p.agreeExec("DROP TABLE c1")
	}

	// Missing parent tables are also ignored.
	p.agreeExec("CREATE TABLE q(x PRIMARY KEY)")
	p.agreeExec("CREATE TABLE d1(a, b, FOREIGN KEY(a,b) REFERENCES q)")
	p.agreeExec("DROP TABLE d1")
	p.agreeExec("CREATE TABLE d2(a REFERENCES nosuchtable)")
	p.agreeExec("INSERT INTO d2 VALUES(1)")
	p.agreeExec("DROP TABLE d2")
	p.agreeExec("DROP TABLE q")

	// Unresolvable keys don't suppress errors from resolvable ones.
	p.agreeExec("CREATE TABLE p(x PRIMARY KEY)")
	p.agreeExec("CREATE TABLE c1(a, b, FOREIGN KEY(a,b) REFERENCES p)")
	p.agreeExec("CREATE TABLE c2(a REFERENCES p)")
	p.agreeExec("INSERT INTO p VALUES(1)")
	p.agreeExec("INSERT INTO c2 VALUES(1)")
	if goErr, cgoErr := p.exec("DROP TABLE p"); cgoErr == nil || tclClassifyExecErr(goErr) != "constraint" {
		t.Errorf("DROP TABLE p with a resolvable referencing row must be a constraint failure on BOTH; go=%v cgo=%v", goErr, cgoErr)
	}
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name = 'p'")
	p.agreeExec("DELETE FROM c2")
	p.agreeExec("DROP TABLE p")
	p.agreeExec("DROP TABLE c1")
	p.agreeExec("DROP TABLE c2")

	// ---- a MISSING parent table moves the counter, and that fails the DROP ----
	// The implicit delete has to be generated for it to matter, which needs
	// something to reference the table being dropped.
	p.agreeExec("PRAGMA foreign_keys = OFF")
	p.agreeExec("CREATE TABLE dd(a REFERENCES nosuchtable)")
	p.agreeExec("CREATE TABLE rr(z REFERENCES dd)")
	p.agreeExec("INSERT INTO dd VALUES(1)")
	p.agreeExec("PRAGMA foreign_keys = ON")
	// A CONSTRAINT failure specifically, not a decline: the point of modelling
	// the counter at all is that the DROP is REFUSED here the way it is there.
	if goErr, cgoErr := p.exec("DROP TABLE dd"); cgoErr == nil || tclClassifyExecErr(goErr) != "constraint" {
		t.Errorf("DROP TABLE dd (missing parent, non-NULL key, referrer present) must be a constraint failure on BOTH; go=%v cgo=%v", goErr, cgoErr)
	}
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name = 'dd'")

	// A NULL key decrements nothing, so the same drop is then accepted.
	p.agreeExec("DELETE FROM dd")
	p.agreeExec("INSERT INTO dd VALUES(NULL)")
	p.agreeExec("DROP TABLE dd")
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name = 'dd'")
	p.agreeExec("DROP TABLE rr")

	// ...and with NOTHING referencing it, no delete is generated at all, so even
	// the non-NULL row drops cleanly.
	p.agreeExec("PRAGMA foreign_keys = OFF")
	p.agreeExec("CREATE TABLE dn(a REFERENCES nosuchtable)")
	p.agreeExec("INSERT INTO dn VALUES(1)")
	p.agreeExec("PRAGMA foreign_keys = ON")
	p.agreeExec("DROP TABLE dn")

	// ...and a MISMATCH is not a missing table: the parent exists, so that
	// branch only skips the key and no counter moves, non-NULL row or not.
	p.agreeExec("PRAGMA foreign_keys = OFF")
	p.agreeExec("CREATE TABLE mq(x PRIMARY KEY)")
	p.agreeExec("CREATE TABLE dm(a, b, FOREIGN KEY(a,b) REFERENCES mq)")
	p.agreeExec("CREATE TABLE rm(z REFERENCES dm)")
	p.agreeExec("INSERT INTO dm VALUES(1, 2)")
	p.agreeExec("PRAGMA foreign_keys = ON")
	p.agreeExec("DROP TABLE dm")
	p.agreeQuery("SELECT count(*) FROM main.sqlite_master WHERE name = 'dm'")

	// The rule is a DROP-only one: an ordinary DELETE or UPDATE on the same
	// schema still raises the mismatch on both engines.
	p.agreeExec("CREATE TABLE p(x PRIMARY KEY)")
	p.agreeExec("CREATE TABLE c(a, b, FOREIGN KEY(a,b) REFERENCES p)")
	p.agreeExec("INSERT INTO p VALUES(1)")
	for _, q := range []string{"DELETE FROM p", "UPDATE p SET x = 2"} {
		if goErr, cgoErr := p.exec(q); goErr == nil || cgoErr == nil {
			t.Errorf("%q must still raise the mismatch on BOTH; go=%v cgo=%v", q, goErr, cgoErr)
		}
	}
	p.agreeExec("DROP TABLE p")
}

// TestTCLOracleAttachNameInUse gates tcl_test.go's tclOracleAttachNameInUse:
// the inverse of tclOracleHasDatabase, for the one statement kind in the
// no-probe set whose database name must be FREE rather than present.
//
// The first half pins the name extraction (including the spellings a database
// name has, since ATTACH's is an expression); the second RUNS every form
// against the oracle and requires the rule's premise to hold -- an ATTACH the
// rule claims C SQLite rejects really is rejected, and one it does not claim
// really is accepted.
func TestTCLOracleAttachNameInUse(t *testing.T) {
	for _, c := range []struct{ stmt, want string }{
		{"ATTACH 'x.db' AS aux", "aux"},
		{"ATTACH DATABASE 'x.db' AS aux", "aux"},
		{"attach\n  'x.db'\n  as  Aux ;", "Aux"},
		{"ATTACH 'x.db' AS 'aux'", "aux"},
		{`ATTACH 'x.db' AS "aux"`, "aux"},
		{"ATTACH 'x.db' AS [aux]", "aux"},
		// The path is an expression and may itself contain " AS ": the LAST one
		// is the binding.
		{"ATTACH 'a AS b' AS aux", "aux"},
		{"ATTACH 'a'||'b' AS aux", "aux"},
		// Not an ATTACH, or no AS name to speak of.
		{"DETACH aux", ""},
		{"PRAGMA aux.user_version", ""},
		{"ATTACH 'x.db'", ""},
		{"SELECT 'ATTACH x AS aux'", ""},
	} {
		got := ""
		if m := tclAttachAsRe.FindStringSubmatch(c.stmt); m != nil {
			got = tclUnquoteName(m[1])
		}
		if got != c.want {
			t.Errorf("attach name of %q = %q, want %q", c.stmt, got, c.want)
		}
	}

	dir := t.TempDir()
	p := newAttachPair(t, nil, nil)
	free := filepath.Join(dir, "free.db")
	taken := filepath.Join(dir, "taken.db")

	// main and temp are refused before a temp database exists...
	for _, q := range []string{
		"ATTACH '" + free + "' AS main",
		"ATTACH '" + free + "' AS temp",
		"ATTACH '" + free + "' AS TeMp",
	} {
		if !tclOracleAttachNameInUse(p.cgo, q) {
			t.Errorf("%q: the rule must claim the name is in use", q)
		}
		if _, err := p.cgo.Exec(q); err == nil {
			t.Errorf("%q: C SQLite ACCEPTED it -- the rule's premise is false", q)
		}
	}
	// ...and after one does.
	if _, err := p.cgo.Exec("CREATE TEMP TABLE tt(a)"); err != nil {
		t.Fatal(err)
	}
	if !tclOracleAttachNameInUse(p.cgo, "ATTACH '"+free+"' AS temp") {
		t.Error("temp must still be in use once a temp table exists")
	}

	// A free name is NOT claimed, so a Go decline of one stays the visible
	// coverage gap it is.
	if tclOracleAttachNameInUse(p.cgo, "ATTACH '"+taken+"' AS aux") {
		t.Fatal("aux is not attached yet; the rule must not claim it is in use")
	}
	if _, err := p.cgo.Exec("ATTACH '" + taken + "' AS aux"); err != nil {
		t.Fatal(err)
	}
	// ...and once taken it is, case-insensitively, while a second name for the
	// SAME FILE is still free (C SQLite allows the aliasing).
	for _, q := range []string{
		"ATTACH '" + free + "' AS aux",
		"ATTACH '" + free + "' AS AUX",
	} {
		if !tclOracleAttachNameInUse(p.cgo, q) {
			t.Errorf("%q: aux IS attached; the rule must claim it", q)
		}
		if _, err := p.cgo.Exec(q); err == nil {
			t.Errorf("%q: C SQLite ACCEPTED a re-ATTACH of a live name", q)
		}
	}
	if tclOracleAttachNameInUse(p.cgo, "ATTACH '"+taken+"' AS aux2") {
		t.Error("aux2 is free even though its FILE is already attached as aux")
	}
	if _, err := p.cgo.Exec("ATTACH '" + taken + "' AS aux2"); err != nil {
		t.Errorf("C SQLite rejected one file under two names: %v", err)
	}
	// A DETACHed name is free again.
	if _, err := p.cgo.Exec("DETACH aux"); err != nil {
		t.Fatal(err)
	}
	if tclOracleAttachNameInUse(p.cgo, "ATTACH '"+free+"' AS aux") {
		t.Error("aux was detached; the rule must not claim it is still in use")
	}
}
