// Tests WAL-mode database interchange: mode flags, integrity, checkpoints,
// side files, and journal_mode changes within transactions.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// walWriterProgram creates a schema and INSERT/DELETE/UPDATE statements for WAL testing.
func walWriterProgram(rows int) []string {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE w(id INTEGER PRIMARY KEY, s TEXT, b BLOB)`,
		`CREATE INDEX wi ON w(s)`,
	}
	for i := 0; i < rows; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO w VALUES(%d,'row-%d',x'%02x%02x')`, i, i, i%256, (i*7)%256))
	}
	return append(stmts,
		`DELETE FROM w WHERE id %% 3 = 0`,
		`UPDATE w SET s = s || '!' WHERE id %% 5 = 0`,
	)
}

const walSelectSQL = `SELECT id,s,quote(b) FROM w ORDER BY id`

// TestWALWrittenByMusqlIsReadableByCSQLite tests WAL interchange both directions.
func TestWALWrittenByMusqlIsReadableByCSQLite(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "wal.db")
			wrote := runWithDSN(t, writer, dsn, append(walWriterProgram(120), walSelectSQL))
			baseline := wrote[len(wrote)-1]
			if kind, _ := baseline["kind"].(string); kind != "rows" {
				t.Fatalf("writer %s: final SELECT did not return rows: %v", writer, baseline)
			}
			for _, reader := range engineOrder {
				reader := reader
				t.Run("read-by-"+reader, func(t *testing.T) {
					got := runWithDSN(t, reader, pathForReader(t, reader, dsn), []string{
						`PRAGMA journal_mode`,
						`PRAGMA integrity_check`,
						walSelectSQL,
					})
					assertOneRowCell(t, reader+": journal_mode", got[0], "T:wal")
					assertOneRowCell(t, reader+": integrity_check", got[1], "T:ok")
					if fmt.Sprint(got[2]) != fmt.Sprint(baseline) {
						t.Errorf("[%s writes, %s reads] WAL content mismatch\n  writer saw: %v\n  reader saw: %v",
							writer, reader, baseline, got[2])
					}
				})
			}
		})
	}
}

// TestWALCheckpointFoldsTheLogBack tests checkpoint behavior and log reset.
func TestWALCheckpointFoldsTheLogBack(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "ck.db")
	// walDefaultAutoCheckpoint is 1000 frames; one commit per INSERT appends at
	// least one, so this crosses the threshold several times over.
	const rows = 2600
	wrote := runWithDSN(t, "musql", dsn, append(walWriterProgram(rows), walSelectSQL, `PRAGMA wal_checkpoint`))
	baseline := wrote[len(wrote)-2]
	if kind, _ := baseline["kind"].(string); kind != "rows" {
		t.Fatalf("writer: final SELECT did not return rows: %v", baseline)
	}
	got := runWithDSN(t, "cgo", exportedForOracle(t, dsn), []string{`PRAGMA integrity_check`, walSelectSQL})
	assertOneRowCell(t, "cgo: integrity_check after checkpoints", got[0], "T:ok")
	if fmt.Sprint(got[1]) != fmt.Sprint(baseline) {
		t.Errorf("content mismatch after crossing the checkpoint threshold\n  musql saw: %v\n  cgo saw:    %v", baseline, got[1])
	}
}

// TestWALModeSurvivesReopenAndCanBeLeft tests WAL mode persistence and side file cleanup.
func TestWALModeSurvivesReopenAndCanBeLeft(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "mode.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", importedForMusql(t, dsn))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		return db
	}
	exec := func(db *sql.DB, stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	mode := func(db *sql.DB) string {
		t.Helper()
		var m string
		if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&m); err != nil {
			t.Fatalf("PRAGMA journal_mode: %v", err)
		}
		return m
	}

	db := open()
	exec(db, `CREATE TABLE t(a)`, `PRAGMA journal_mode=WAL`, `INSERT INTO t VALUES(1),(2),(3)`)
	if m := mode(db); m != "wal" {
		t.Fatalf("after entering: journal_mode = %q, want wal", m)
	}
	db.Close()

	// A FRESH connection must see wal mode -- i.e. the header really is in the
	// database file, not only in this session's memory or in the log.
	db = open()
	if m := mode(db); m != "wal" {
		t.Fatalf("after reopen: journal_mode = %q, want wal", m)
	}
	exec(db, `INSERT INTO t VALUES(4)`)
	exec(db, `PRAGMA journal_mode=DELETE`)
	if m := mode(db); m != "delete" {
		t.Fatalf("after leaving: journal_mode = %q, want delete", m)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("after leaving wal mode: count=%d err=%v, want 4", n, err)
	}
	db.Close()
	for _, suffix := range []string{walSuffixForTest, "-shm"} {
		if _, err := os.Stat(dsn + suffix); err == nil {
			t.Errorf("%s survived leaving wal mode", suffix)
		}
	}
	// ...and C SQLite agrees the file is a sound rollback-journal database.
	got := runWithDSN(t, "cgo", exportedForOracle(t, dsn), []string{`PRAGMA journal_mode`, `PRAGMA integrity_check`, `SELECT count(*) FROM t`})
	assertOneRowCell(t, "cgo: journal_mode after leaving wal", got[0], "T:delete")
	assertOneRowCell(t, "cgo: integrity_check after leaving wal", got[1], "T:ok")
	assertOneRowCell(t, "cgo: row count after leaving wal", got[2], "I:4")
}

const walSuffixForTest = "-wal"

// assertOneRowCell checks single-row, single-column results.
func assertOneRowCell(t *testing.T, what string, res map[string]any, want string) {
	t.Helper()
	rows, _ := res["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("%s: expected 1 row, got %v", what, res)
	}
	cells, _ := rows[0].([]any)
	if len(cells) != 1 || fmt.Sprint(cells[0]) != want {
		t.Errorf("%s: got %v, want [%s]", what, cells, want)
	}
}

// TestJournalModeChangeInsideTransactionIsDeclined tests journal_mode changes
// within transactions: WAL transitions error cleanly, rollback-to-rollback
// changes may take or be silently ignored depending on transaction dirtiness.
func TestJournalModeChangeInsideTransactionIsDeclined(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stmts   []string
		decline bool
	}{
		// A WAL transition from a CLEAN transaction is an error in real
		// SQLite, and an error here too -- the same outcome as the decline it
		// replaced, now for the oracle's own reason.
		{"into-wal-from-a-deferred-transaction", []string{`CREATE TABLE t(a)`, `BEGIN`, `PRAGMA journal_mode=wal`}, true},
		{"out-of-wal-from-a-transaction", []string{`CREATE TABLE t(a)`, `PRAGMA journal_mode=wal`, `BEGIN`, `PRAGMA journal_mode=delete`}, true},
		// ...while a rollback-to-rollback change from a clean transaction TAKES.
		{"into-truncate-from-a-deferred-transaction", []string{`CREATE TABLE t(a)`, `BEGIN`, `PRAGMA journal_mode=truncate`}, false},
		{"out-of-truncate-from-a-transaction", []string{`CREATE TABLE t(a)`, `PRAGMA journal_mode=truncate`, `BEGIN`, `PRAGMA journal_mode=memory`}, false},
		{"no-change-is-still-accepted", []string{`CREATE TABLE t(a)`, `PRAGMA journal_mode=wal`, `BEGIN`, `PRAGMA journal_mode=wal`}, false},
		{"delete-to-delete-is-still-accepted", []string{`CREATE TABLE t(a)`, `BEGIN`, `PRAGMA journal_mode=delete`}, false},
		{"truncate-to-truncate-is-still-accepted", []string{`CREATE TABLE t(a)`, `PRAGMA journal_mode=truncate`, `BEGIN`, `PRAGMA journal_mode=truncate`}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, "musql", tc.stmts)
			last := res[len(res)-1]
			gotErr := last["kind"] == "error"
			if gotErr != tc.decline {
				t.Errorf("%s: kind=%v, wanted decline=%v", tc.stmts[len(tc.stmts)-1], last["kind"], tc.decline)
			}
		})
	}
	// Once the transaction has provably written, the setter is the oracle's
	// silent no-op: it reports the OLD mode and the mode survives the COMMIT.
	// (Used to be asserted as a decline; upgraded when the dirty half of the
	// pager boundary became provable -- see the doc comment.)
	for _, to := range []string{"wal", "persist"} {
		to := to
		t.Run("into-"+to+"-after-writing-is-ignored", func(t *testing.T) {
			res := run(t, "musql", []string{
				`CREATE TABLE t(a)`,
				`BEGIN`,
				`INSERT INTO t VALUES(1)`,
				`PRAGMA journal_mode=` + to, // #3: ignored, reports delete
				`COMMIT`,
				`PRAGMA journal_mode`, // #5: still delete
			})
			assertOneRowCell(t, "ignored setter's own report", res[3], "T:delete")
			assertOneRowCell(t, "journal_mode after COMMIT", res[5], "T:delete")
		})
	}
}
