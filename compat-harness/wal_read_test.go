// Tests that the engine's read path correctly interprets WAL (write-ahead log)
// files. The engine must return the same rows as C SQLite for databases with
// committed changes in the WAL file that have not yet been checkpointed.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// walQuery is the read-back used throughout this gate.
const walQuery = "SELECT id, v FROM t ORDER BY id"

// walOpenConn opens path with C SQLite as a single connection (so every
// PRAGMA below applies to, and stays applied on, that one underlying
// sqlite3 connection -- go-sqlite3 pragmas like wal_autocheckpoint and
// cache_size are per-connection, and a pool that silently opened a second
// connection would lose them), in WAL mode with auto-checkpointing
// disabled: exactly the "keep a connection open so the -wal persists, and
// nothing auto-folds it back" setup this gate needs (a normal Close would
// checkpoint, and typically empties or removes the -wal).
func walOpenConn(t *testing.T, path string, pageSize int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	walStmt(t, db, fmt.Sprintf("PRAGMA page_size=%d", pageSize)) // no-op once the db is non-empty
	walStmt(t, db, "PRAGMA journal_mode=WAL")
	walStmt(t, db, "PRAGMA wal_autocheckpoint=0")
	return db
}

// walStmt runs sqlText, trying Query first (PRAGMAs return a result row, and
// database/sql's Exec is not guaranteed to like that) and falling back to
// Exec, the technique worker/main.go uses to run arbitrary SQL against any
// driver. Rows must be drained with Next() before Close(): go-sqlite3 only
// steps a statement to completion as Next() is called, so a statement with
// no result columns (CREATE TABLE, INSERT, BEGIN, ...) run through Query and
// closed WITHOUT ever calling Next() is left un-executed -- confirmed by a
// CREATE TABLE that reported no error yet left the table absent.
func walStmt(t *testing.T, db *sql.DB, sqlText string) {
	t.Helper()
	rows, err := db.Query(sqlText)
	if err != nil {
		if _, eerr := db.Exec(sqlText); eerr != nil {
			t.Fatalf("%s: query err=%v, exec err=%v", sqlText, err, eerr)
		}
		return
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	rows.Close()
}

// walCRows reads walQuery via db (C SQLite) and formats "id|v" lines in
// query order -- the oracle every scenario below compares the pure-Go
// engine's Query result against.
func walCRows(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(walQuery)
	if err != nil {
		t.Fatalf("C query: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("%d|%s", id, v))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

// walGoRows opens path fresh with the pure-Go engine and formats walQuery's
// result identically to walCRows, so the two are directly comparable.
// NeedsRecovery must be false: this gate is specifically about the case
// where the pure-Go reader IS able to make sense of the WAL.
//
// The engine reads its OWN format (RULE #3), so what it sees of a live C WAL
// database is what ImportSQLite converts: the base file with every committed
// frame of the -wal laid over it.
func walGoRows(t *testing.T, path string) string {
	t.Helper()
	seg := filepath.Join(t.TempDir(), "imported.musq")
	if err := sqliteconv.Import(path, seg, sqliteconv.ImportOptions{}); err != nil {
		t.Fatalf("ImportSQLite(%s): %v", path, err)
	}
	p, err := engine.Open(seg)
	if err != nil {
		t.Fatalf("engine.Open(%s): %v", seg, err)
	}
	defer p.Close()
	_, rows, err := p.Query(walQuery)
	if err != nil {
		t.Fatalf("engine Query: %v", err)
	}
	var lines []string
	for _, r := range rows {
		id := r[0].I
		var v string
		if r[1].Typ == engine.Text || r[1].Typ == engine.Blob {
			v = string(r[1].S)
		}
		lines = append(lines, fmt.Sprintf("%d|%s", id, v))
	}
	return strings.Join(lines, "\n")
}

// walAssertMatches is the core gate assertion: at this instant (no writer
// active), the pure-Go engine's read of path must be EXACTLY what C SQLite,
// through the still-open connection db, reports.
func walAssertMatches(t *testing.T, db *sql.DB, path, label string) {
	t.Helper()
	want := walCRows(t, db)
	got := walGoRows(t, path)
	if got != want {
		t.Fatalf("%s: pure-Go engine rows != C SQLite rows\n  C SQLite:\n%s\n  pure-Go:\n%s", label, want, got)
	}
}

// walParseRows turns walGoRows/walCRows's "id|v" lines into a map, so a
// scenario can assert about one specific row without the ambiguity of
// substring-matching formatted lines (e.g. "1|X" is a substring of
// "21|X").
func walParseRows(s string) map[int64]string {
	m := map[int64]string{}
	if s == "" {
		return m
	}
	for _, line := range strings.Split(s, "\n") {
		i := strings.IndexByte(line, '|')
		id, err := strconv.ParseInt(line[:i], 10, 64)
		if err != nil {
			continue
		}
		m[id] = line[i+1:]
	}
	return m
}

func TestWALRead(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wal.db")

			db := walOpenConn(t, path, pageSize)

			// Base state, spanning several pages, committed via one
			// transaction.
			walStmt(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
			walStmt(t, db, "BEGIN")
			for i := 1; i <= 200; i++ {
				walStmt(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,'orig-%d-padding-text-xxxxx')", i, i))
			}
			walStmt(t, db, "COMMIT")

			// Fold the base state into the main file and empty the -wal, so
			// everything from here on is layered, via the WAL, on top of
			// REAL base-file pages (not the entire database).
			walStmt(t, db, "PRAGMA wal_checkpoint(TRUNCATE)")

			// Scenario: a WAL-mode db that WAS checkpointed (-wal
			// empty/absent) reads correctly from the base file alone.
			if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > walHeaderSizeForTest {
				t.Fatalf("expected an empty/truncated -wal right after checkpoint, got size %d", info.Size())
			}
			walAssertMatches(t, db, path, "after checkpoint (no WAL overlay)")

			// Commit 1: overrides an existing page (UPDATE of early rows,
			// which were part of the checkpointed base file), deletes an
			// existing row, and inserts a brand-new row that exists ONLY in
			// the WAL.
			walStmt(t, db, "BEGIN")
			walStmt(t, db, "UPDATE t SET v='WAL-OVERRIDE' WHERE id BETWEEN 1 AND 5")
			walStmt(t, db, "DELETE FROM t WHERE id=10")
			walStmt(t, db, "INSERT INTO t(id,v) VALUES(9990,'wal-only-row')")
			walStmt(t, db, "COMMIT")
			walAssertMatches(t, db, path, "commit 1: page override + delete + wal-only row")

			rows := walParseRows(walGoRows(t, path))
			if rows[1] != "WAL-OVERRIDE" {
				t.Fatalf("row 1: got %q, want WAL-OVERRIDE (page override not visible)", rows[1])
			}
			if _, ok := rows[10]; ok {
				t.Fatalf("row 10 should have been deleted by the WAL commit, but is present")
			}
			if rows[9990] != "wal-only-row" {
				t.Fatalf("row 9990 (WAL-only insert): got %q, want wal-only-row", rows[9990])
			}

			// Commit 2: a SECOND, later commit overriding the SAME rows
			// again -- proves the reader tracks the LATEST committed frame
			// per page, not merely the first one it finds.
			walStmt(t, db, "UPDATE t SET v='WAL-OVERRIDE-2' WHERE id BETWEEN 1 AND 5")
			walAssertMatches(t, db, path, "commit 2: same page overridden again (latest commit wins)")
			rows = walParseRows(walGoRows(t, path))
			if rows[1] != "WAL-OVERRIDE-2" {
				t.Fatalf("row 1 after second commit: got %q, want WAL-OVERRIDE-2 (latest WAL commit did not win)", rows[1])
			}

			// Commit 3: extend the database with enough new rows to force
			// brand-new pages beyond the checkpointed base file's own page
			// count.
			walStmt(t, db, "BEGIN")
			for i := 0; i < 300; i++ {
				walStmt(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,'extend-%d-padding-text-xxxxx')", 20000+i, i))
			}
			walStmt(t, db, "COMMIT")
			walAssertMatches(t, db, path, "commit 3: db extended by the WAL with new pages")

			// Sanity: the WAL genuinely did extend the db past the
			// checkpointed base file's page count -- otherwise "extended by
			// WAL" was never actually exercised.
			// C's own page count, which reads through the WAL -- this checks the
			// FIXTURE, so the oracle is the one to ask.
			var walPages uint32
			if err := db.QueryRow("PRAGMA page_count").Scan(&walPages); err != nil {
				t.Fatalf("PRAGMA page_count: %v", err)
			}
			baseInfo, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat base file: %v", err)
			}
			basePages := uint32(baseInfo.Size()) / uint32(pageSize)
			if walPages <= basePages {
				t.Fatalf("WAL snapshot page count (%d) <= checkpointed base file page count (%d); WAL extension was not exercised", walPages, basePages)
			}

			// Uncommitted trailing frame: a SECOND connection opens an
			// explicit transaction, forces its dirty pages to spill into
			// the WAL file (a tiny cache_size makes SQLite write them out
			// well before any commit), and then ROLLS BACK instead of
			// committing. Those spilled frames are real, checksummed bytes
			// physically sitting in the -wal by the time we read -- and
			// MUST be entirely invisible to both C SQLite (read through the
			// first connection, which never opened that transaction) and
			// the pure-Go engine.
			second := walOpenConn(t, path, pageSize)
			walStmt(t, second, "PRAGMA cache_size=2")
			walStmt(t, second, "BEGIN")
			for i := 0; i < 200; i++ {
				walStmt(t, second, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,'UNCOMMITTED-%d')", 90000+i, i))
			}
			walAssertMatches(t, db, path, "uncommitted trailing frame must be ignored")
			if got := walGoRows(t, path); strings.Contains(got, "UNCOMMITTED") {
				t.Fatalf("pure-Go engine leaked rows from an uncommitted transaction:\n%s", got)
			}
			walStmt(t, second, "ROLLBACK")
			second.Close()
			walAssertMatches(t, db, path, "after rollback of the second connection's transaction")

			// Final scenario: a normal Close() (C SQLite's ordinary
			// shutdown path) checkpoints and typically empties/removes the
			// -wal entirely. A fresh reader -- both C SQLite's and the
			// pure-Go engine's -- must still agree afterward.
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			oracle, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer oracle.Close()
			want := walCRows(t, oracle)
			got := walGoRows(t, path)
			if got != want {
				t.Fatalf("after Close(): pure-Go engine rows != C SQLite rows\n  C SQLite:\n%s\n  pure-Go:\n%s", want, got)
			}
		})
	}
}

// walHeaderSizeForTest mirrors engine's walHeaderSize (32): a truncated/
// checkpointed -wal file is expected to be at or below this, never a real
// frame's worth of additional bytes. Kept as a small local constant rather
// than importing engine's unexported one.
const walHeaderSizeForTest = 32
