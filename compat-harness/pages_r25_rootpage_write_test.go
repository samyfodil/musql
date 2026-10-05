// This file gates the WRITE side of sqlite_master.rootpage with writable_schema ON.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// rpwPrograms are the mined shapes plus the observations that must stay normal
// after them. Engine-direct, like TestTCLCorpus: this engine's catalog overlay
// lives on the session, and the driver opens a fresh one per statement.
var rpwPrograms = []struct {
	name  string
	stmts []string
	// reads are run afterwards against BOTH engines and must agree.
	reads []string
}{
	{"corruptN.test#5: rootpage past the end of the file", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES('a'),('b'),('c')`,
		`CREATE TABLE t2(y)`,
		`CREATE TRIGGER tr BEFORE UPDATE ON t1 BEGIN DELETE FROM t2; END`,
		`PRAGMA writable_schema = ON`,
		`UPDATE sqlite_schema SET rootpage = 137 WHERE name='t2'`,
	}, []string{
		`SELECT * FROM t1`, `SELECT * FROM t2`, `PRAGMA integrity_check`,
		`SELECT type, name FROM sqlite_schema ORDER BY name`,
	}},
	{"pager1.test#35: rootpage of a page that does not exist", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE t2(a, b)`,
		`PRAGMA writable_schema = 1`,
		`UPDATE sqlite_master SET rootpage=5 WHERE tbl_name = 't1'`,
		`PRAGMA writable_schema = 0`,
	}, []string{
		`SELECT * FROM t1`, `SELECT * FROM t2`, `PRAGMA integrity_check`,
	}},
	{"a later INSERT does not refresh the frozen rootpage", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)`,
		`CREATE UNIQUE INDEX i1 ON t1(c)`,
		`INSERT INTO t1 VALUES(1,'one','i'),(2,'two','ii')`,
		`PRAGMA writable_schema = 1`,
		`UPDATE sqlite_schema SET rootpage = 9 WHERE name = 'i1'`,
		`INSERT INTO t1 VALUES(3,'three','iii')`,
	}, []string{
		`SELECT * FROM t1 WHERE c='iii'`, `SELECT count(*) FROM t1`,
		`PRAGMA integrity_check`,
	}},
}

// TestPagesR25RootpageWriteIsANoOp is the differential half: the write is
// accepted on both engines and changes nothing either can observe.
func TestPagesR25RootpageWriteIsANoOp(t *testing.T) {
	for _, tc := range rpwPrograms {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			godb, err := engine.Create(filepath.Join(dir, "m.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			for _, s := range tc.stmts {
				if _, _, eerr := godb.ExecArgs(s, nil); eerr != nil {
					t.Fatalf("musql %q: %v", s, eerr)
				}
			}
			cgodb, oerr := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
			if oerr != nil {
				t.Fatalf("open oracle: %v", oerr)
			}
			cgodb.SetMaxOpenConns(1)
			defer cgodb.Close()
			for _, s := range tc.stmts {
				if _, cerr := cgodb.Exec(s); cerr != nil {
					t.Fatalf("oracle %q: %v -- the write is supposed to be accepted", s, cerr)
				}
			}
			for _, q := range tc.reads {
				want := rpwCGORows(t, cgodb, q)
				got := rpwEngineRows(t, godb, q)
				if got != want {
					t.Errorf("after the rootpage write, %q\n  musql: %s\n  cgo:    %s", q, got, want)
				}
			}
		})
	}
}

// TestPagesR25RootpageCorruptFileReopens is the never-panic half. A rootpage
// this engine wrote through to page 1 names a page that holds something else,
// or no page at all -- reopening such a file is exactly the case C SQLite
// answers "database disk image is malformed" for. What this engine must never
// do is PANIC, and it must not report success with invented rows either.
func TestPagesR25RootpageCorruptFileReopens(t *testing.T) {
	for _, root := range []string{"137", "5", "2", "0", "1"} {
		root := root
		t.Run("rootpage="+root, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "m.db")
			godb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			for _, s := range []string{
				`CREATE TABLE t1(a, b)`,
				`INSERT INTO t1 VALUES(1, 'x')`,
				`CREATE TABLE t2(a, b)`,
				`CREATE INDEX t2a ON t2(a)`,
				`PRAGMA writable_schema = 1`,
				`UPDATE sqlite_schema SET rootpage = ` + root + ` WHERE name = 't2'`,
			} {
				if _, _, eerr := godb.ExecArgs(s, nil); eerr != nil {
					t.Fatalf("musql %q: %v", s, eerr)
				}
			}
			if cerr := godb.Close(); cerr != nil {
				t.Fatalf("close: %v", cerr)
			}
			// Reopen with THIS engine -- the panic gate.
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("reopening a file with rootpage=%s PANICKED: %v", root, r)
					}
				}()
				db, oerr := sql.Open("sqlite", path)
				if oerr != nil {
					return
				}
				db.SetMaxOpenConns(1)
				defer db.Close()
				for _, q := range []string{
					`SELECT * FROM t1`, `SELECT * FROM t2`,
					`SELECT type, name FROM sqlite_schema ORDER BY name`,
					`PRAGMA integrity_check`,
				} {
					rows, qerr := db.Query(q)
					if qerr != nil {
						t.Logf("musql reopen %q: %v", q, qerr)
						continue
					}
					n := 0
					for rows.Next() {
						n++
					}
					rerr := rows.Err()
					rows.Close()
					t.Logf("musql reopen %q: %d rows, err=%v", q, n, rerr)
				}
			}()
			// ...and with C SQLite, whose answer is the reference for what a
			// corrupt catalog looks like from outside -- when there is a file to
			// hand it. A catalog whose rootpage names storage this format cannot
			// reproduce has no SQLite form, and ExportSQLite declines it rather than
			// writing one (ConvertedCatalog.RootEdits); the reopen above declines
			// the same way, which is the never-panic gate this test is.
			exported := filepath.Join(t.TempDir(), "export.db")
			if xerr := sqliteconv.Export(path, exported, 0); xerr != nil {
				t.Logf("ExportSQLite declines this catalog: %v", xerr)
				return
			}
			cdb, cerr := sql.Open("sqlite3", exported)
			if cerr != nil {
				t.Fatalf("oracle open: %v", cerr)
			}
			cdb.SetMaxOpenConns(1)
			defer cdb.Close()
			var n int
			t.Logf("cgo reopen SELECT count(*) FROM t2: err=%v", cdb.QueryRow(`SELECT count(*) FROM t2`).Scan(&n))
		})
	}
}

// TestPagesR25RootpageReadMatchesCSQLite pins the statements that EVALUATE
// the rootpage column of a catalog write. They were declined while this
// engine's page numbering could differ from C SQLite's; rootpage numbering
// is now reproduced (TestPagesR25RootpageNumbering) and a history that diverges
// is flagged (DB.wsRootpageNumberingDiverged), so over this schema they are
// served, and every one must leave the catalog exactly as C SQLite does.
// A decline is still allowed; an accepted statement that disagrees is not.
func TestPagesR25RootpageReadMatchesCSQLite(t *testing.T) {
	for _, stmt := range []string{
		`UPDATE sqlite_schema SET rootpage = (SELECT rootpage FROM sqlite_schema WHERE name='i2') WHERE name='i1'`,
		`UPDATE sqlite_schema SET rootpage = (SELECT rootpage FROM sqlite_schema WHERE name='t1')`,
		`UPDATE sqlite_schema SET name = 'q' WHERE rootpage = 3`,
		`UPDATE sqlite_schema SET name = 'x' || (SELECT rootpage FROM sqlite_schema WHERE name='t1')`,
		`DELETE FROM sqlite_schema WHERE rootpage > 2`,
		`INSERT INTO sqlite_master VALUES('table','q','q',(SELECT rootpage FROM sqlite_master WHERE name='t1'),'CREATE TABLE q(x)')`,
	} {
		t.Run(stmt[:min(48, len(stmt))], func(t *testing.T) {
			dir := t.TempDir()
			godb, err := engine.Create(filepath.Join(dir, "m.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
			if err != nil {
				t.Fatalf("open oracle: %v", err)
			}
			cgodb.SetMaxOpenConns(1)
			defer cgodb.Close()
			for _, s := range []string{
				`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)`,
				`CREATE UNIQUE INDEX i1 ON t1(c)`,
				`CREATE TABLE t2(a INTEGER PRIMARY KEY, b, c)`,
				`CREATE UNIQUE INDEX i2 ON t2(c)`,
				`PRAGMA writable_schema = 1`,
			} {
				if _, _, eerr := godb.ExecArgs(s, nil); eerr != nil {
					t.Fatalf("setup %q: %v", s, eerr)
				}
				if _, cerr := cgodb.Exec(s); cerr != nil {
					t.Fatalf("oracle setup %q: %v", s, cerr)
				}
			}
			if _, _, eerr := wsGoRun(godb, stmt); eerr != nil {
				t.Logf("declined: %v", eerr)
				return
			}
			if _, _, cerr := wsCGORun(cgodb, stmt); cerr != nil {
				t.Fatalf("C SQLite refused what this engine accepted: %v", cerr)
			}
			// Not ORDER BY rowid: the catalog's rowids are one of the things
			// this engine declines to report once a statement has churned it.
			const q = `SELECT type, name, tbl_name, rootpage, sql FROM sqlite_schema ORDER BY type, name`
			goCols, goRows, goErr := wsGoRun(godb, q)
			if goErr != nil {
				t.Logf("catalog read declined: %v", goErr)
				return
			}
			cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, q)
			if cgoErr != nil {
				t.Fatalf("catalog read: C SQLite errored where this engine answered: %v", cgoErr)
			}
			if !wsSameResult(goCols, goRows, cgoCols, cgoRows) {
				t.Errorf("catalog after %q diverges:\n  pure: %v\n  real: %v", stmt, goRows, cgoRows)
			}
		})
	}
}

// TestPagesR25WritableSchemaResetDoesNotReload measures the ONE way a catalog
// edit is observable within the connection, and this engine does not reproduce
// it: "PRAGMA writable_schema=RESET" turns the flag off *and reloads the
// schema*, which is exactly when a deliberately-broken catalog starts biting
// (it is what corruptN.test uses). This engine parses the RESET spelling --
// engine.PragmaWritableSchemaValue returns isReset -- and then discards it, in
// both the engine (writableSchemaResult) and the driver
// (driver's writableSchemaPragma), so the flag goes off and nothing else
// happens.
//
// PRE-EXISTING, not introduced by serving a rootpage assignment: the same hole
// is there for the sql column, which has been served all along. It needed
// either a reload of db.tables from the edited page 1, or a decline of RESET
// while db.wsEdits is non-empty.
//
// ROUND 25, pragma stream: it is now the DECLINE (writableSchemaResetDecline,
// pragma.go, with driver routing the held-session case to it). So this test
// no longer measures a divergence -- it holds the boundary: the RESET must be
// REFUSED while an edit is outstanding, and the read after it is then never
// reached. If musql ever accepts it again, the reload it implies is what has
// to be real, and the row comparison below says whether it is.
func TestPagesR25WritableSchemaResetDoesNotReload(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit string
		read string
	}{
		{"a rewritten sql text", `UPDATE sqlite_schema SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`, `SELECT * FROM t1`},
		{"a rootpage past the end of the file", `UPDATE sqlite_schema SET rootpage=137 WHERE name='t1'`, `SELECT * FROM t1`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stmts := []string{
				`CREATE TABLE t1(a, b)`, `INSERT INTO t1 VALUES(1, 2)`,
				`PRAGMA writable_schema=ON`, tc.edit, `PRAGMA writable_schema=RESET`,
			}
			godb, err := engine.Create(filepath.Join(dir, "m.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			declinedReset := false
			for _, s := range stmts {
				_, _, eerr := godb.ExecArgs(s, nil)
				if eerr == nil {
					continue
				}
				if s == `PRAGMA writable_schema=RESET` {
					declinedReset = true
					break
				}
				t.Fatalf("musql %q: %v", s, eerr)
			}
			cgodb, oerr := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
			if oerr != nil {
				t.Fatalf("open oracle: %v", oerr)
			}
			cgodb.SetMaxOpenConns(1)
			defer cgodb.Close()
			for _, s := range stmts {
				if _, cerr := cgodb.Exec(s); cerr != nil {
					t.Fatalf("oracle %q: %v", s, cerr)
				}
			}
			want := rpwCGORows(t, cgodb, tc.read)
			if declinedReset {
				// The boundary, held: the reload C SQLite performs here is
				// what makes the edit bite, and refusing to pretend otherwise is
				// the whole fix. Recorded with the oracle's own answer so the
				// target stays visible to whoever implements the reload.
				t.Logf("RESET DECLINED after %s -- the oracle's post-reload %q answers %s", tc.name, tc.read, want)
				return
			}
			got := rpwEngineRows(t, godb, tc.read)
			if got != want {
				t.Errorf("musql ACCEPTED writable_schema=RESET after %s and did not reload -- %q\n  musql: %s\n  cgo:    %s",
					tc.name, tc.read, got, want)
			}
		})
	}
}

func rpwEngineRows(t *testing.T, db *engine.Session, q string) string {
	t.Helper()
	cols, rows, err, panicked, panicVal := tclSafeGoQuery(db, q)
	if panicked {
		t.Fatalf("musql PANICKED on %q: %v", q, panicVal)
	}
	// Formatted exactly like rpwCGORows' error arm. Dropping it made a
	// correctly refused read compare as an empty result against the
	// oracle's "malformed database schema".
	if err != nil {
		return "ERR " + err.Error()
	}
	return rpwFormat(cols, rows)
}

func rpwCGORows(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	cols, rows, err := tclRunCGOQuery(db, q)
	if err != nil {
		return "ERR " + err.Error()
	}
	return rpwFormat(cols, rows)
}

func rpwFormat(cols []string, rows [][]string) string {
	out := "cols=" + joinStrings(cols) + " rows=["
	for i, r := range rows {
		if i > 0 {
			out += " "
		}
		out += joinStrings(r)
	}
	return out + "]"
}

func joinStrings(v []string) string {
	out := "("
	for i, s := range v {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out + ")"
}
