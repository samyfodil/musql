//go:build sqlite_fts5

// This file tests fts5's COMMAND CHANNEL against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// fts5CommandBase is the initial table setup for command tests.
var fts5CommandBase = []string{
	`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
	`INSERT INTO t(rowid,a,b) VALUES(1,'one two','x y')`,
	`INSERT INTO t(rowid,a,b) VALUES(2,'two three','y z')`,
	`INSERT INTO t(rowid,a,b) VALUES(3,'three four','z w')`,
}

// fts5CommandProbes are the shadow-table reads compared between engines.
var fts5CommandProbes = []string{
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT id, quote(c0), quote(c1) FROM t_content ORDER BY id`,
	`SELECT count(*) FROM t`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'two' ORDER BY rowid)`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts5CommandDiff replays one command per case against both engines and
// requires them to agree on whether it is accepted and on everything readable
// afterwards. A statement BOTH reject is agreement, and is logged rather than
// failed -- but one this engine accepts and C fts5 does not (or the other
// way round) is a hard failure, because that is a wrong answer.
func TestFts5CommandDiff(t *testing.T) {
	cmds := []string{
		// The four non-configuration commands, with and without a value.
		`('optimize')`, `('rebuild')`, `('integrity-check')`, `('merge')`,
		`('optimize', 5)`, `('rebuild', 5)`, `('integrity-check', 0)`, `('integrity-check', 1)`,
		`('integrity-check', 'x')`,
		// 'merge' takes any value at all, including none and NULL, and never
		// errors: negative means "merge until done", which is the state this
		// engine is permanently in.
		`('merge', 1)`, `('merge', 5)`, `('merge', 500)`, `('merge', -1)`, `('merge', -10)`,
		`('merge', -16)`, `('merge', 0)`, `('merge', NULL)`, `('merge', 'x')`, `('merge', 5.5)`,
		// pgsz: the corpus's own values, then both bounds and past them.
		`('pgsz', 32)`, `('pgsz', 40)`, `('pgsz', 64)`, `('pgsz', 100)`, `('pgsz', 128)`,
		`('pgsz', 4050)`, `('pgsz', 65536)`, `('pgsz', 31)`, `('pgsz', 65537)`, `('pgsz', 0)`,
		`('pgsz', -5)`, `('pgsz')`, `('pgsz', NULL)`, `('pgsz', 'abc')`, `('pgsz', 32.7)`,
		`('pgsz', 32.0)`, `('pgsz', x'3332')`, `('pgsz', 999999)`,
		// The value's own conversion rule, on a key whose range is wide.
		`('pgsz', '64')`, `('pgsz', ' 64')`, `('pgsz', '64 ')`, `('pgsz', '+64')`,
		`('pgsz', '064')`, `('pgsz', '64.0')`, `('pgsz', '6e1')`, `('pgsz', '0x40')`,
		`('pgsz', '')`, `('pgsz', '64abc')`, `('pgsz', '6 4')`, `('pgsz', '9223372036854775808')`,
		`('pgsz', '99999999999999999999')`, `('pgsz', 9223372036854775807)`,
		// The range check runs on the value's 32-bit truncation, not the value.
		`('pgsz', 4294967328)`, `('pgsz', 4294967296)`, `('pgsz', -4294967264)`,
		// secure-delete, automerge, usermerge, crisismerge, deletemerge,
		// hashsize, insttoken: each key's own bounds.
		`('secure-delete', 0)`, `('secure-delete', 1)`, `('secure-delete', 2)`,
		`('secure-delete', 100)`, `('secure-delete', -1)`, `('secure-delete')`,
		`('secure-delete', 'x')`, `('secure-delete', 4294967295)`, `('secure-delete', 4294967296)`,
		`('automerge', 0)`, `('automerge', 1)`, `('automerge', 4)`, `('automerge', 16)`,
		`('automerge', 64)`, `('automerge', 65)`, `('automerge', -1)`, `('automerge')`,
		`('automerge', 4294967360)`, `('automerge', 4294967361)`,
		`('usermerge', 1)`, `('usermerge', 2)`, `('usermerge', 4)`, `('usermerge', 16)`,
		`('usermerge', 17)`, `('usermerge', -1)`, `('usermerge')`,
		`('crisismerge', 0)`, `('crisismerge', 1)`, `('crisismerge', 2)`, `('crisismerge', 16)`,
		`('crisismerge', 100)`, `('crisismerge', 2147483647)`, `('crisismerge', 4294967296)`,
		`('crisismerge', -1)`, `('crisismerge')`,
		`('deletemerge', 0)`, `('deletemerge', 10)`, `('deletemerge', 100)`, `('deletemerge', -1)`,
		`('deletemerge', '55')`, `('deletemerge', 'x')`, `('deletemerge', NULL)`, `('deletemerge')`,
		`('hashsize', 1)`, `('hashsize', 1024)`, `('hashsize', 2147483647)`,
		`('hashsize', 2147483648)`, `('hashsize', 4294967297)`, `('hashsize', 0)`,
		`('hashsize', -1)`, `('hashsize')`,
		`('insttoken', 0)`, `('insttoken', 1)`, `('insttoken', 2)`, `('insttoken', -1)`,
		`('insttoken', '1')`, `('insttoken', 'x')`, `('insttoken')`, `('insttoken', 4294967295)`,
		// The command NAME folds case; the %_config key it stores does not.
		`('OPTIMIZE')`, `('Optimize')`, `('PGSZ', 64)`, `('Secure-Delete', 1)`,
		// Neither engine takes these: a padded name, an unknown one, and
		// 'delete-all', which C fts5 refuses on a content-bearing table.
		`(' pgsz ', 64)`, `('nosuchcommand')`, `('nosuchcommand', 1)`, `('delete-all')`,
		// And the two C fts5 DOES take that this engine declines by design.
		`('rank', 'bm25()')`, `(NULL)`,
	}
	// declined marks the shapes this engine refuses on purpose: 'rank' names a
	// default rank function, and rank is not reachable here at all; a NULL
	// command is C fts5's way of spelling "insert an all-NULL row", which
	// this engine does not reproduce (engine/fts5_shadow.go says why for each).
	// A decline is a legitimate outcome under this project's rules, so it is
	// logged -- but the day either turns into an ANSWER it is compared like
	// every other, so it can never quietly become a wrong one.
	declined := map[string]bool{`('rank', 'bm25()')`: true, `(NULL)`: true}
	for _, c := range cmds {
		c := c
		t.Run(strings.NewReplacer("'", "", "(", "", ")", "", " ", "_", ",", "").Replace(c), func(t *testing.T) {
			cols := "t"
			if strings.Contains(c, ",") {
				cols = "t, rank"
			}
			stmt := fmt.Sprintf(`INSERT INTO t(%s) VALUES%s`, cols, c)
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			errs := map[string]error{}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], fts5CommandBase); err != nil {
					t.Fatalf("%s setup: %v", drv, err)
				}
				errs[drv] = fts5Exec(t, drv, dsn[drv], []string{stmt})
			}
			switch {
			case errs["sqlite"] != nil && errs["sqlite3"] != nil:
				t.Logf("BOTH REJECT %s\n  go:  %v\n  cgo: %v", stmt, errs["sqlite"], errs["sqlite3"])
				return
			case errs["sqlite"] != nil:
				if declined[c] {
					t.Logf("DECLINED BY DESIGN %s\n  go: %v", stmt, errs["sqlite"])
					return
				}
				t.Errorf("this engine REJECTS a command C fts5 accepts\n  sql: %s\n  err: %v", stmt, errs["sqlite"])
				return
			case errs["sqlite3"] != nil:
				t.Errorf("this engine ACCEPTS a command C fts5 rejects (a wrong answer)\n  sql: %s\n  cgo err: %v", stmt, errs["sqlite3"])
				return
			}
			for _, q := range fts5CommandProbes {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  go:  %v\n  cgo: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES after %s\n  sql: %s\n%s", "state", stmt, q, fts5DiffLines(goOut, cgoOut))
				}
			}
			// Whatever the command did to this engine's file, C SQLite --
			// which never saw it being written -- must still find the index
			// consistent with the content.
			if ic, err := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), `PRAGMA integrity_check`); err != nil || ic != "integrity_check\nT:ok" {
				t.Errorf("after %s C SQLite reports integrity_check = %q (%v)", stmt, ic, err)
			}
		})
	}
}

// fts5SecureDeleteCases are the boundaries of the one rule 'secure-delete'
// carries outside %_data: which statements move %_config's "version" row from
// 4 to 5, and which leave it alone.
var fts5SecureDeleteCases = []struct {
	name  string
	stmts []string
}{
	{"nothing removed", nil},
	{"delete with secure-delete off", []string{`DELETE FROM t WHERE rowid=2`}},
	{"secure-delete alone", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`}},
	{"secure-delete then insert", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `INSERT INTO t(rowid,a,b) VALUES(9,'q','r')`}},
	{"secure-delete then delete", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `DELETE FROM t WHERE rowid=2`}},
	{"secure-delete then update", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `UPDATE t SET a='zzz' WHERE rowid=2`}},
	{"secure-delete then delete of no rows", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `DELETE FROM t WHERE rowid=99`}},
	{"secure-delete then update of no rows", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `UPDATE t SET a='zzz' WHERE rowid=99`}},
	{"secure-delete then delete every row", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `DELETE FROM t`}},
	{"secure-delete 0 then delete", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',0)`, `DELETE FROM t WHERE rowid=2`}},
	{"secure-delete 2 then delete", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',2)`, `DELETE FROM t WHERE rowid=2`}},
	{"delete then secure-delete", []string{`DELETE FROM t WHERE rowid=2`, `INSERT INTO t(t,rank) VALUES('secure-delete',1)`}},
	{"the bump is sticky", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `DELETE FROM t WHERE rowid=2`, `INSERT INTO t(t,rank) VALUES('secure-delete',0)`, `DELETE FROM t WHERE rowid=3`}},
	{"secure-delete then optimize", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `INSERT INTO t(t) VALUES('optimize')`}},
	{"secure-delete then rebuild", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `INSERT INTO t(t) VALUES('rebuild')`}},
	{"secure-delete then merge", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `INSERT INTO t(t,rank) VALUES('merge',-16)`}},
	{"capitalised, then delete", []string{`INSERT INTO t(t,rank) VALUES('Secure-Delete',1)`, `DELETE FROM t WHERE rowid=2`}},
	{"two deletes", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `DELETE FROM t WHERE rowid=2`, `DELETE FROM t WHERE rowid=3`}},
}

func TestFts5CommandSecureDeleteVersion(t *testing.T) {
	for _, c := range fts5SecureDeleteCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], append(append([]string(nil), fts5CommandBase...), c.stmts...)); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5CommandProbes {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  go:  %v\n  cgo: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES\n  sql: %s\n%s", c.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
			// A version-5 file is still one C SQLite reads and checks.
			if ic, err := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), `PRAGMA integrity_check`); err != nil || ic != "integrity_check\nT:ok" {
				t.Errorf("C SQLite reports integrity_check = %q (%v) over this engine's file", ic, err)
			}
		})
	}
}

// TestFts5CommandSecureDeleteInTransaction gates the one shape of the version
// rule this engine still DECLINES -- an UPDATE's removal inside an explicit
// transaction -- and confirms a DELETE's, which used to be declined too, is
// now DEFERRED to the next real flush point instead (engine/fts5_txn.go):
// C fts5 keeps an in-memory pending-write hash and only bumps %_config's
// 'version' row when that hash is flushed to disk (xSync/xRelease/
// xSavepoint), and this engine now models exactly that one %_config write
// (not the whole hash -- see fts5_txn.go's header for why the row removal
// itself never needed deferring).
//
// Each case runs the same statements on both engines and reports which
// statement, if any, this engine refused. A decline is the expected outcome
// only where "declines" says so -- everywhere else the transaction must go
// through and leave the same %_config, which is what keeps the guard from
// quietly widening into "no DELETE inside a transaction".
func TestFts5CommandSecureDeleteInTransaction(t *testing.T) {
	cases := []struct {
		name     string
		stmts    []string
		declines bool
	}{
		{"delete in a transaction", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `DELETE FROM t WHERE rowid=2`, `COMMIT`}, false},
		// UPDATE stays declined: C fts5's per-write flush trigger for it
		// runs through sqlite3Fts5IndexBeginWrite's rowid-ordering rule
		// (fts5_index.c:6788-6810), not only the txn-boundary callbacks a bare
		// DELETE needs, and this engine has no per-write hash to place that
		// rule against (fts5_config.go's fts5SecureDeleteGuard).
		{"update in a transaction", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `UPDATE t SET a='q' WHERE rowid=2`, `COMMIT`}, true},
		{"secure-delete off", []string{`BEGIN`, `DELETE FROM t WHERE rowid=2`, `COMMIT`}, false},
		{"secure-delete explicitly 0", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',0)`, `BEGIN`, `DELETE FROM t WHERE rowid=2`, `COMMIT`}, false},
		{"removing nothing", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `DELETE FROM t WHERE rowid=99`, `COMMIT`}, false},
		{"inserting in a transaction", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `INSERT INTO t(rowid,a,b) VALUES(9,'q','r')`, `COMMIT`}, false},
		// Once the bump has already happened in autocommit, the version row is
		// where it will stay, so a later transaction has nothing to defer.
		{"already at version 5", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `DELETE FROM t WHERE rowid=2`, `BEGIN`, `DELETE FROM t WHERE rowid=3`, `COMMIT`}, false},
		// Two deletes in one transaction (fts5secure3.test's own mined shape):
		// both ascending, literal rowids, so neither self-flushes the other
		// mid-transaction (verified against the oracle) -- the bump lands once,
		// at COMMIT.
		{"two deletes, one transaction", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `DELETE FROM t WHERE rowid=2`, `DELETE FROM t WHERE rowid=3`, `COMMIT`}, false},
		// A SAVEPOINT flushes unconditionally (C fts5's xSavepointMethod),
		// so the bump lands there instead of waiting for COMMIT.
		{"delete then savepoint", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `DELETE FROM t WHERE rowid=2`, `SAVEPOINT s1`, `COMMIT`}, false},
		// ROLLBACK discards the deferred bump outright -- nothing was ever
		// written for it, so there is nothing left to leave behind.
		{"delete then rollback", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `DELETE FROM t WHERE rowid=2`, `ROLLBACK`}, false},
		// The savepoint's own flush happens BEFORE the pager-level savepoint
		// mark C SQLite takes for it (vdbe.c's OP_Savepoint calls
		// sqlite3VtabSavepoint before creating the Savepoint struct), so a
		// LATER "ROLLBACK TO" that same savepoint must NOT undo the bump it
		// flushed -- verified directly against the oracle (fts5_txn.go's
		// openSavepoint comment).
		{"delete, savepoint (flushes), rollback to it stays flushed", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `DELETE FROM t WHERE rowid=2`, `SAVEPOINT s1`, `ROLLBACK TO s1`, `COMMIT`}, false},
		// Opening the savepoint BEFORE the delete means the delete's own bump
		// is pending AFTER the mark, so rolling back to that savepoint takes
		// the row (and the still-unflushed bump) with it -- this is
		// fts5version.test's own 2.1-2.3 shape.
		{"savepoint, delete, rollback to it discards both", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `SAVEPOINT s1`, `DELETE FROM t WHERE rowid=2`, `ROLLBACK TO s1`, `COMMIT`}, false},
		// An UNRELATED DDL statement mid-transaction is C fts5's OTHER
		// flush trigger (sqlite3VtabSavepoint's statement-sub-transaction
		// mechanism, not a txn-boundary callback) -- verified directly against
		// the oracle that it DOES bump %_config here. This engine does not
		// model it (fts5_txn.go's fts5TxnPendingDDLGuard) and declines cleanly
		// instead of leaving a stale version behind.
		{"delete then unrelated DDL", []string{`INSERT INTO t(t,rank) VALUES('secure-delete',1)`, `BEGIN`, `DELETE FROM t WHERE rowid=2`, `CREATE TABLE unrelated_ddl(x)`, `COMMIT`}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			errs := map[string]error{}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], fts5CommandBase); err != nil {
					t.Fatalf("%s setup: %v", drv, err)
				}
				errs[drv] = fts5Exec(t, drv, dsn[drv], c.stmts)
			}
			if errs["sqlite3"] != nil {
				t.Fatalf("C fts5 rejected the setup itself: %v", errs["sqlite3"])
			}
			if c.declines {
				if errs["sqlite"] == nil {
					t.Errorf("this engine ACCEPTED a removal it cannot time correctly -- if that is now reproduced, gate its %%_config here instead of declining it")
				} else {
					t.Logf("DECLINED BY DESIGN: %v", errs["sqlite"])
				}
				return
			}
			if errs["sqlite"] != nil {
				t.Fatalf("the guard fired on a statement it should not have: %v", errs["sqlite"])
			}
			for _, q := range fts5CommandProbes {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  go:  %v\n  cgo: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES\n  sql: %s\n%s", c.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// fts5ExecSameConn runs stmts in order on ONE already-open connection,
// returning the first error. Unlike fts5Exec/fts5Query (which each open and
// close their own connection), this is what a mid-transaction probe needs:
// a driver-HELD transaction's state (driver's Conn.tx) lives on the
// CONNECTION, not the database file, so BEGIN and a later COMMIT/SELECT must
// share one to see the same held transaction at all.
func fts5ExecSameConn(t *testing.T, db *sql.DB, stmts []string) error {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s, err)
		}
	}
	return nil
}

// fts5QuerySameConn is fts5Query over an already-open connection -- see
// fts5ExecSameConn.
func fts5QuerySameConn(t *testing.T, db *sql.DB, stmt string) (string, error) {
	t.Helper()
	rows, err := db.Query(stmt)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(strings.Join(cols, "|"))
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		b.WriteString("\n")
		for i, c := range cells {
			if i > 0 {
				b.WriteString("|")
			}
			b.WriteString(tclNormalizeCGOCell(c))
		}
	}
	return b.String(), rows.Err()
}

// TestFts5CommandChannelFlushesDeferredSecureDelete gates the OTHER flush/
// discard points a deferred 'secure-delete' bump has, beyond the plain
// txn-boundary ones TestFts5CommandSecureDeleteInTransaction already covers:
// the command channel itself. 'optimize', 'merge', 'flush', 'rank' and any
// configuration command (including 'secure-delete' being reconfigured) flush
// the ISSUING table's own pending bump before they run their own work;
// 'rebuild' and 'delete-all' discard it instead, since both reinitialize the
// index from scratch and leave nothing for a deferred removal to describe
// (engine/fts5_txn.go's fts5TxnFlushTable/fts5TxnDiscardTable, wired into
// engine/fts5_shadow.go's fts5CommandInsert).
//
// Each case reads %_config's version TWICE inside the same still-open
// transaction -- once right after the DELETE (must read 4, since nothing has
// flushed yet) and once right after the command under test (must match
// wantAfterCmd) -- and then once more after COMMIT, which every one of these
// reaches at 5 except 'rebuild'/'delete-all', which discarded the only bump
// there ever was.
func TestFts5CommandChannelFlushesDeferredSecureDelete(t *testing.T) {
	cases := []struct {
		name         string
		cmd          string
		wantAfterCmd string // %_config version right after cmd, still mid-transaction
	}{
		{"optimize", `INSERT INTO t(t) VALUES('optimize')`, "5"},
		{"merge", `INSERT INTO t(t,rank) VALUES('merge',-16)`, "5"},
		{"flush", `INSERT INTO t(t) VALUES('flush')`, "5"},
		{"rank", `INSERT INTO t(t,rank) VALUES('rank','bm25()')`, "5"},
		{"pgsz (a configuration command)", `INSERT INTO t(t,rank) VALUES('pgsz',64)`, "5"},
		{"secure-delete (reconfigured)", `INSERT INTO t(t,rank) VALUES('secure-delete',2)`, "5"},
		{"rebuild", `INSERT INTO t(t) VALUES('rebuild')`, "4"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			conns := map[string]*sql.DB{}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				db, err := sql.Open(drv, filepath.Join(dir, drv+".db"))
				if err != nil {
					t.Fatalf("open %s: %v", drv, err)
				}
				db.SetMaxOpenConns(1)
				t.Cleanup(func() { db.Close() })
				conns[drv] = db
			}
			versionQ := `SELECT quote(v) FROM t_config WHERE k='version'`
			for _, drv := range []string{"sqlite", "sqlite3"} {
				db := conns[drv]
				if err := fts5ExecSameConn(t, db, fts5CommandBase); err != nil {
					t.Fatalf("%s setup: %v", drv, err)
				}
				if err := fts5ExecSameConn(t, db, []string{
					`INSERT INTO t(t,rank) VALUES('secure-delete',1)`,
					`BEGIN`,
					`DELETE FROM t WHERE rowid=2`,
				}); err != nil {
					t.Fatalf("%s: pending-bump setup: %v", drv, err)
				}
				if out, err := fts5QuerySameConn(t, db, versionQ); err != nil || out != "quote(v)\nT:4" {
					t.Fatalf("%s: version before %s = %q (%v), want still-deferred 4", drv, c.name, out, err)
				}
				if err := fts5ExecSameConn(t, db, []string{c.cmd}); err != nil {
					t.Fatalf("%s: %s: %v", drv, c.cmd, err)
				}
			}
			wantMid := "quote(v)\nT:" + c.wantAfterCmd
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if out, err := fts5QuerySameConn(t, conns[drv], versionQ); err != nil || out != wantMid {
					t.Errorf("%s: version right after %s = %q (%v), want %q", drv, c.name, out, err, wantMid)
				}
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5ExecSameConn(t, conns[drv], []string{`COMMIT`}); err != nil {
					t.Fatalf("%s: COMMIT: %v", drv, err)
				}
			}
			for _, q := range fts5CommandProbes {
				goOut, goErr := fts5QuerySameConn(t, conns["sqlite"], q)
				cgoOut, cgoErr := fts5QuerySameConn(t, conns["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  go:  %v\n  cgo: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES after COMMIT\n  sql: %s\n%s", c.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// TestFts5CommandDeleteAllDiscardsDeferredSecureDelete is
// TestFts5CommandChannelFlushesDeferredSecureDelete's 'delete-all' case,
// kept separate because 'delete-all' needs a CONTENTLESS table (fts5CommandBase
// is an ordinary one) and its own way of removing a row -- the 'delete'
// command, not a SQL DELETE (fts5_extcontent.go's fts5ExtDeleteCommand also
// reaches fts5NoteRowsRemoved, so the pending bump forms identically).
func TestFts5CommandDeleteAllDiscardsDeferredSecureDelete(t *testing.T) {
	dir := t.TempDir()
	versionQ := `SELECT quote(v) FROM ct_config WHERE k='version'`
	base := []string{
		`CREATE VIRTUAL TABLE ct USING fts5(col, content='')`,
		`INSERT INTO ct(rowid, col) VALUES(1, 'data for the table')`,
		`INSERT INTO ct(rowid, col) VALUES(2, 'more of the same')`,
		`INSERT INTO ct(ct, rank) VALUES('secure-delete', 1)`,
		`BEGIN`,
		`INSERT INTO ct(ct, rowid, col) VALUES('delete', 1, 'data for the table')`,
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		db, err := sql.Open(drv, filepath.Join(dir, drv+".db"))
		if err != nil {
			t.Fatalf("open %s: %v", drv, err)
		}
		db.SetMaxOpenConns(1)
		defer db.Close()
		if err := fts5ExecSameConn(t, db, base); err != nil {
			t.Fatalf("%s setup: %v", drv, err)
		}
		if out, err := fts5QuerySameConn(t, db, versionQ); err != nil || out != "quote(v)\nT:4" {
			t.Fatalf("%s: version before delete-all = %q (%v), want still-deferred 4", drv, out, err)
		}
		if err := fts5ExecSameConn(t, db, []string{`INSERT INTO ct(ct) VALUES('delete-all')`, `COMMIT`}); err != nil {
			t.Fatalf("%s: delete-all/COMMIT: %v", drv, err)
		}
		if out, err := fts5QuerySameConn(t, db, versionQ); err != nil || out != "quote(v)\nT:4" {
			t.Errorf("%s: version after delete-all and COMMIT = %q (%v), want discarded 4", drv, out, err)
		}
	}
}

// fts5PageSizeDump is fts5ShadowDump with the DOCLIST INDEX taken out of the
// two shadow tables that carry it.
//
// A doclist index is fts5's seek accelerator for a term whose doclist has run
// past FTS5_MIN_DLIDX_SIZE leaf pages carrying no term of their own: an extra
// %_data page in the dlidx address space (bit 36 of the id), flagged by the
// LOW BIT of the %_idx pgno that covers it. This engine never writes one, at
// any page size -- engine/fts5_index.go says why, and the flag it leaves clear
// is exactly what tells C fts5 not to look for one.
//
// Small page sizes are simply the first place that omission becomes REACHABLE:
// at the default 4050 a doclist has to be enormous before four consecutive
// leaf pages hold no term, which is why TestFts5ShadowLayoutDiff's 3000-row
// cases compare %_data in full and still agree. At pgsz=32 a 40-row table
// already gets there. What is being gated here is that everything ELSE is
// byte-identical -- every leaf page, the structure record, the averages
// record, %_docsize, %_config -- and that C SQLite's own integrity_check
// still passes over the result, which is the check that the omission is
// self-consistent rather than a corrupt file.
var fts5PageSizeDump = []string{
	`SELECT id, quote(block) FROM t_data WHERE (id>>36)&1 = 0 ORDER BY id`,
	`SELECT quote(segid), quote(term), quote(pgno>>1) FROM t_idx ORDER BY segid, term`,
	`SELECT * FROM t_content ORDER BY id`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT k, v FROM t_config`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts5CommandPageSize is the byte gate for 'pgsz'. Each case sets a page
// size and then writes the whole table in ONE statement, which is the shape
// where the two engines' segment layouts agree exactly (see
// TestFts5ShadowLayoutDiff) -- so at a page size C fts5 splits 200 ways,
// every split point has to land on the same byte.
func TestFts5CommandPageSize(t *testing.T) {
	body := func(n int) string {
		vals := make([]string, n)
		for i := range vals {
			vals[i] = fmt.Sprintf("(%d,'word%04d qqq shared','col%d beta')", i+1, i, i)
		}
		return `INSERT INTO t(rowid,a,b) VALUES` + strings.Join(vals, ",")
	}
	cases := []struct {
		pgsz, rows int
	}{
		{32, 2}, {32, 40}, {32, 200}, {40, 200}, {64, 200}, {100, 200}, {128, 300},
		{1000, 400}, {4050, 400}, {65536, 400},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("pgsz=%d/rows=%d", c.pgsz, c.rows), func(t *testing.T) {
			stmts := []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				fmt.Sprintf(`INSERT INTO t(t, rank) VALUES('pgsz', %d)`, c.pgsz),
				body(c.rows),
			}
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			// This engine writes no doclist index, at any page size (see
			// engine/fts5_index.go), so the exclusions in fts5PageSizeDump
			// below cannot be hiding a divergence on THIS side.
			for _, q := range []string{
				`SELECT count(*) FROM t_data WHERE (id>>36)&1 = 1`,
				`SELECT count(*) FROM t_idx WHERE pgno&1 = 1`,
			} {
				if got, err := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q); err != nil || got != "count(*)\nI:0" {
					t.Fatalf("this engine's file has doclist-index state it should never write\n  sql: %s\n  got: %q (%v)", q, got, err)
				}
			}
			// Both dumps through the CGo driver, over the two FILES -- the only
			// way to read %_data from this side at all (this engine declines a
			// query against it), and the claim actually being made.
			for _, q := range fts5PageSizeDump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("pgsz=%d DIVERGES\n  sql: %s\n%s", c.pgsz, q, fts5DiffLines(goOut, cgoOut))
				}
			}
			// The bytes agreeing is the strong claim; this is the one that
			// would catch a page size C SQLite still reads and searches
			// correctly but whose checksum does not add up.
			if ic, err := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), `PRAGMA integrity_check`); err != nil || ic != "integrity_check\nT:ok" {
				t.Errorf("pgsz=%d: C SQLite reports integrity_check = %q (%v)", c.pgsz, ic, err)
			}
			// And this engine must read back C SQLite's file at that page
			// size, through the index rather than around it.
			for _, q := range []string{
				`SELECT count(*) FROM t`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'word0001' ORDER BY rowid)`,
				`SELECT k, quote(v) FROM t_config ORDER BY k`,
			} {
				goOut, goErr := fts5Query(t, "sqlite", importedForMusql(t, dsn["sqlite3"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  go over cgo's file: %v\n  cgo: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("pgsz=%d: this engine reads C SQLite's file differently\n  sql: %s\n  go:  %q\n  cgo: %q", c.pgsz, q, goOut, cgoOut)
				}
			}
		})
	}
}
