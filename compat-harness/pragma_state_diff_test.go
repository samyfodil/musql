// This file tests the CONNECTION/DATABASE-STATE pragmas (journal_mode, locking_mode,
// writable_schema, wal_checkpoint) against C SQLite through the full driver stack.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// journalModeCase is a journal_mode pragma statement and whether it should
// decline on file-backed or memory-backed databases.
type journalModeCase struct {
	sql           string
	declineOnFile bool
	declineOnMem  bool
}

// Accept/decline is derived from the oracle: getters always answer, setters answer
// when C SQLite would switch or when the engine implements the mode.
var journalModeCases = []journalModeCase{
	{sql: `PRAGMA journal_mode`},
	{sql: `PRAGMA main.journal_mode`},
	{sql: `PRAGMA journal_mode=delete`, declineOnFile: false, declineOnMem: false},
	{sql: `PRAGMA journal_mode=DELETE`},
	{sql: `PRAGMA journal_mode='delete'`},
	{sql: `PRAGMA journal_mode(delete)`},
	// Unrecognized mode names: ignored, current mode reported.
	{sql: `PRAGMA journal_mode=xxx`},
	{sql: `PRAGMA journal_mode=persistent`}, // Not a real mode
	{sql: `PRAGMA journal_mode=invalid`},
	// WAL mode: setter switches on file, ignored on memory.
	{sql: `PRAGMA journal_mode=wal`},
	{sql: `PRAGMA journal_mode=WAL`},
	// Other rollback modes: no-ops on memory, both sides answer "memory".
	{sql: `PRAGMA journal_mode=truncate`},
	{sql: `PRAGMA journal_mode=persist`},
	{sql: `PRAGMA journal_mode=memory`},
	{sql: `PRAGMA journal_mode=TRUNCATE`},
	// "off" mode: changes ROLLBACK behavior, accepted on both file and memory.
	{sql: `PRAGMA journal_mode=off`, declineOnFile: false, declineOnMem: false},

	// locking_mode: declined on memory-backed databases.
	{sql: `PRAGMA locking_mode`, declineOnMem: true},
	{sql: `PRAGMA main.locking_mode`, declineOnMem: true},
	// temp.locking_mode: constant "exclusive", independent of main.
	{sql: `PRAGMA temp.locking_mode`, declineOnMem: true},
	{sql: `PRAGMA locking_mode=normal`, declineOnMem: true},
	{sql: `PRAGMA locking_mode=NORMAL`, declineOnMem: true},
	{sql: `PRAGMA locking_mode=xyz`, declineOnMem: true}, // unrecognized: ignored
	// exclusive mode: served on file, declined on memory.
	{sql: `PRAGMA locking_mode=exclusive`, declineOnMem: true},
}

// singleValuePragmaCases are single-row getter pragmas split out because setters differ.
var singleValuePragmaCases = []journalModeCase{
	{sql: `PRAGMA writable_schema`},
	{sql: `PRAGMA main.writable_schema`},
	// wal_checkpoint: omitted, returns fixed 0/-1/-1 and fails SQLITE_LOCKED.
}

// writableSchemaSetterCases test writable_schema setter, which returns empty result
// set with no columns, unlike journal_mode/locking_mode.
var writableSchemaSetterCases = []struct {
	sql     string
	decline bool
}{
	{sql: `PRAGMA writable_schema=0`},
	{sql: `PRAGMA writable_schema=off`},
	{sql: `PRAGMA writable_schema=no`},
	{sql: `PRAGMA writable_schema=false`},
	{sql: `PRAGMA writable_schema=1`},
	{sql: `PRAGMA writable_schema=on`},
	{sql: `PRAGMA writable_schema=yes`},
	{sql: `PRAGMA writable_schema=true`},
	{sql: `PRAGMA writable_schema=reset`},
	{sql: `PRAGMA writable_schema=RESET`},
	{sql: `PRAGMA writable_schema=2`},
	{sql: `PRAGMA writable_schema=xxx`},
	{sql: `PRAGMA writable_schema=-1`},
	{sql: `PRAGMA writable_schema=0x10`},
}

// writableSchemaFlagSequence is the setter/getter ROUND TRIP, which the shape
// test above cannot see: a setter whose shape is right but whose effect is
// wrong still passes there. Every step's getter is compared against real
// SQLite's, so the value each spelling maps to comes from the oracle rather
// than from this engine's own idea of a boolean -- in particular "=reset",
// which is OFF plus a schema reload and NOT a third state, and the survival of
// the flag across COMMIT and ROLLBACK (3.53.3 does not clear it at either,
// whatever older versions did).
var writableSchemaFlagSequence = []string{
	`PRAGMA writable_schema`,
	`PRAGMA writable_schema=ON`,
	`PRAGMA writable_schema`,
	`PRAGMA main.writable_schema`,
	`PRAGMA writable_schema=off`,
	`PRAGMA writable_schema`,
	`PRAGMA writable_schema=1`,
	`PRAGMA writable_schema`,
	`PRAGMA writable_schema=reset`,
	`PRAGMA writable_schema`,
	`PRAGMA writable_schema=true`,
	`PRAGMA writable_schema`,
	`BEGIN`,
	`PRAGMA writable_schema`,
	`COMMIT`,
	`PRAGMA writable_schema`,
	`BEGIN`,
	`PRAGMA writable_schema=no`,
	`ROLLBACK`,
	`PRAGMA writable_schema`,
	`CREATE TABLE wsflag(x)`,
	`PRAGMA writable_schema`,
}

// TestWritableSchemaFlagRoundTripMatchesCSQLite replays that sequence
// against both engines on ONE connection each and compares every getter.
func TestWritableSchemaFlagRoundTripMatchesCSQLite(t *testing.T) {
	pureDB, mattnDB := openJournalModePair(t, false)
	for i, sqlText := range writableSchemaFlagSequence {
		mattnCols, mattnRows, mattnErr := queryAll(mattnDB, sqlText)
		pureCols, pureRows, pureErr := queryAll(pureDB, sqlText)
		if (mattnErr == nil) != (pureErr == nil) {
			t.Fatalf("step %d %q: real err=%v, pure err=%v", i, sqlText, mattnErr, pureErr)
		}
		if mattnErr != nil {
			continue
		}
		if strings.Join(pureCols, ",") != strings.Join(mattnCols, ",") || fmt.Sprint(pureRows) != fmt.Sprint(mattnRows) {
			t.Fatalf("step %d %q diverges: pure=%v%v real=%v%v", i, sqlText, pureCols, pureRows, mattnCols, mattnRows)
		}
	}
}

// TestPragmaSetterRowShapeMatchesCSQLite gates the SETTER result SHAPE,
// which the row-value comparison above cannot see: a setter that wrongly
// reports a row (or wrongly reports none) still "agrees" on every value it
// does return. This is the exact blind spot that makes PRAGMA work dangerous
// -- the corpus never compares a PRAGMA's rows at all.
func TestPragmaSetterRowShapeMatchesCSQLite(t *testing.T) {
	for _, tc := range writableSchemaSetterCases {
		t.Run(tc.sql, func(t *testing.T) {
			pureDB, mattnDB := openJournalModePair(t, false)

			mattnCols, mattnRows, mattnErr := queryShape(mattnDB, tc.sql)
			if mattnErr != nil {
				t.Fatalf("%s: C SQLite errored: %v", tc.sql, mattnErr)
			}
			pureCols, pureRows, pureErr := queryShape(pureDB, tc.sql)
			if tc.decline {
				if pureErr == nil {
					t.Fatalf("%s: expected this engine to DECLINE, got cols=%v rows=%d", tc.sql, pureCols, pureRows)
				}
				return
			}
			if pureErr != nil {
				t.Fatalf("%s: expected this engine to ANSWER, got error: %v", tc.sql, pureErr)
			}
			if len(pureCols) != len(mattnCols) || pureRows != mattnRows {
				t.Errorf("%s: setter shape diverges: pure cols=%v rows=%d, real cols=%v rows=%d",
					tc.sql, pureCols, pureRows, mattnCols, mattnRows)
			}
		})
	}
}

// TestPragmaInsideWriteTransactionMatchesCSQLite pins the hazard that made
// PRAGMA wal_checkpoint unanswerable: C SQLite fails some pragmas
// SQLITE_LOCKED once the connection holds a WRITE lock, and this engine, which
// models no such lock ladder, would answer anyway. The mined corpus caught
// exactly that as a WRONG result in fallocate.test.
//
// The three pragmas this package DOES answer were checked against the same
// state and do not have the hazard. This test keeps it that way -- if any of
// them ever starts depending on lock state, or if wal_checkpoint is added back
// without a lock model, it fails here rather than in a 15-chunk corpus sweep.
func TestPragmaInsideWriteTransactionMatchesCSQLite(t *testing.T) {
	// A deferred BEGIN that has WRITTEN, and a bare BEGIN IMMEDIATE, are the
	// two states C SQLite treats as holding the write lock -- verified
	// directly: "BEGIN" alone is NOT enough, "BEGIN; INSERT" and
	// "BEGIN IMMEDIATE" both are.
	for _, opener := range [][]string{
		{`BEGIN`, `INSERT INTO jm VALUES(1)`},
		{`BEGIN IMMEDIATE`},
	} {
		t.Run(strings.Join(opener, ";"), func(t *testing.T) {
			pureDB, mattnDB := openJournalModePair(t, false)
			for _, stmt := range opener {
				if _, err := pureDB.Exec(stmt); err != nil {
					t.Fatalf("pure %q: %v", stmt, err)
				}
				if _, err := mattnDB.Exec(stmt); err != nil {
					t.Fatalf("mattn %q: %v", stmt, err)
				}
			}
			for _, sqlText := range []string{
				`PRAGMA journal_mode`, `PRAGMA journal_mode=delete`,
				`PRAGMA locking_mode`, `PRAGMA locking_mode=normal`,
				`PRAGMA writable_schema`,
			} {
				mattnCols, mattnRows, mattnErr := queryAll(mattnDB, sqlText)
				pureCols, pureRows, pureErr := queryAll(pureDB, sqlText)
				if (mattnErr == nil) != (pureErr == nil) {
					t.Errorf("%s under a write lock: real err=%v, pure err=%v", sqlText, mattnErr, pureErr)
					continue
				}
				if mattnErr != nil {
					continue
				}
				if strings.Join(pureCols, ",") != strings.Join(mattnCols, ",") || fmt.Sprint(pureRows) != fmt.Sprint(mattnRows) {
					t.Errorf("%s under a write lock diverges: pure=%v%v real=%v%v", sqlText, pureCols, pureRows, mattnCols, mattnRows)
				}
			}
		})
	}
}

// queryShape runs sqlText and reports the result set's column names and row
// COUNT, without caring about cell values -- what the setter cases assert.
func queryShape(db *sql.DB, sqlText string) (cols []string, nRows int, err error) {
	rows, err := db.Query(sqlText)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	cols, err = rows.Columns()
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		nRows++
	}
	return cols, nRows, rows.Err()
}

func TestPragmaJournalModeMatchesCSQLite(t *testing.T) {
	for _, mem := range []bool{false, true} {
		name := "file"
		if mem {
			name = "memory"
		}
		t.Run(name, func(t *testing.T) {
			cases := append(append([]journalModeCase(nil), journalModeCases...), singleValuePragmaCases...)
			for _, tc := range cases {
				t.Run(tc.sql, func(t *testing.T) {
					decline := tc.declineOnFile
					if mem {
						decline = tc.declineOnMem
					}
					runJournalModeCase(t, mem, tc.sql, decline)
				})
			}
		})
	}
}

// runJournalModeCase runs one statement against a FRESH pair of databases --
// fresh because a setter C SQLite accepts changes the oracle's mode for
// every later statement on that connection, which would make the cases
// order-dependent.
func runJournalModeCase(t *testing.T, mem bool, sqlText string, wantDecline bool) {
	t.Helper()
	pureDB, mattnDB := openJournalModePair(t, mem)

	mattnCols, mattnRows, mattnErr := queryAll(mattnDB, sqlText)
	pureCols, pureRows, pureErr := queryAll(pureDB, sqlText)

	// C SQLite never errors on any of these, whatever the argument.
	if mattnErr != nil {
		t.Fatalf("%s: C SQLite errored, which it never does here: %v", sqlText, mattnErr)
	}
	if wantDecline {
		if pureErr == nil {
			t.Fatalf("%s: expected this engine to DECLINE, but it answered %v (C SQLite: %v)", sqlText, pureRows, mattnRows)
		}
		return
	}
	if pureErr != nil {
		t.Fatalf("%s: expected this engine to ANSWER %v, got error: %v", sqlText, mattnRows, pureErr)
	}
	if strings.Join(pureCols, ",") != strings.Join(mattnCols, ",") {
		t.Errorf("%s: column names diverge: pure=%v real=%v", sqlText, pureCols, mattnCols)
	}
	if fmt.Sprint(pureRows) != fmt.Sprint(mattnRows) {
		t.Errorf("%s: rows diverge: pure=%v real=%v", sqlText, pureRows, mattnRows)
	}
}

// queryAll runs sqlText and returns the column names plus every row rendered
// as strings -- so a multi-column pragma like wal_checkpoint (busy/log/
// checkpointed) is compared in full, not just its first column.
func queryAll(db *sql.DB, sqlText string) (cols []string, out [][]string, err error) {
	rows, err := db.Query(sqlText)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err = rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		rendered := make([]string, len(cols))
		for i, c := range cells {
			if b, ok := c.([]byte); ok {
				c = string(b)
			}
			rendered[i] = fmt.Sprintf("%v", c)
		}
		out = append(out, rendered)
	}
	return cols, out, rows.Err()
}

// openJournalModePair opens one database per driver, either file-backed or
// memory-backed. Both handles are pinned to a SINGLE connection: an in-memory
// database is per-connection in C SQLite, so a pooled second connection
// would silently be a DIFFERENT database.
func openJournalModePair(t *testing.T, mem bool) (pureDB, mattnDB *sql.DB) {
	t.Helper()
	pureDSN, mattnDSN := ":memory:", ":memory:"
	if !mem {
		dir := t.TempDir()
		pureDSN = filepath.Join(dir, "jm.pure.sqlite")
		mattnDSN = filepath.Join(dir, "jm.mattn.sqlite")
	}
	open := func(driver, dsn string) *sql.DB {
		db, err := sql.Open(driver, dsn)
		if err != nil {
			t.Fatalf("sql.Open(%s, %s): %v", driver, dsn, err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		// The "jm" table is a FIXTURE, not a workaround:
		// TestPragmaInsideWriteTransactionMatchesCSQLite below needs a table to
		// INSERT into. (It used to double as a workaround for a read-only FIRST
		// statement on a memory-backed database failing SQLITE_BUSY -- that bug is
		// fixed, and empty_db_read_first_test.go now gates the untouched-database
		// case directly.)
		if _, err := db.Exec(`CREATE TABLE jm(a)`); err != nil {
			t.Fatalf("%s: CREATE: %v", driver, err)
		}
		return db
	}
	return open(driver.DriverName, pureDSN), open("sqlite3", mattnDSN)
}

// TestPragmaSecureDeleteMatchesCSQLite gates "PRAGMA secure_delete", which
// this driver answers from CONNECTION state (driver's secureDeletePragma).
//
// The whole sequence runs on ONE pair of connections on purpose: the value is
// connection state, so what has to match is not any single statement but the
// running value every later getter reports. Both the column names and every
// cell are compared, plus the ROW COUNT -- secure_delete's setter reports the
// new value back, where the zero-row setters below return nothing, and that
// difference is invisible to the mined corpus (it routes every PRAGMA to the
// exec side, where rows are never compared).
func TestPragmaSecureDeleteMatchesCSQLite(t *testing.T) {
	pureDB, mattnDB := openJournalModePair(t, false)
	for _, sqlText := range []string{
		// The default, and that a qualified getter agrees with the bare one.
		`PRAGMA secure_delete`,
		`PRAGMA main.secure_delete`,
		`PRAGMA temp.secure_delete`,
		// Every accepted spelling, each followed by a read-back -- the assertion
		// that the assignment was APPLIED and not merely shape-matched.
		`PRAGMA secure_delete=1`,
		`PRAGMA secure_delete`,
		`PRAGMA secure_delete=0`,
		`PRAGMA secure_delete`,
		`PRAGMA secure_delete=on`,
		`PRAGMA secure_delete`,
		`PRAGMA secure_delete=off`,
		`PRAGMA secure_delete`,
		`PRAGMA secure_delete=yes`,
		`PRAGMA secure_delete=no`,
		`PRAGMA secure_delete=true`,
		`PRAGMA secure_delete=false`,
		// "fast" is its own third value (2), not a boolean.
		`PRAGMA secure_delete=fast`,
		`PRAGMA secure_delete`,
		`PRAGMA main.secure_delete`,
		`PRAGMA temp.secure_delete`,
		`PRAGMA secure_delete=FAST`,
		`PRAGMA secure_delete='fast'`,
		`PRAGMA secure_delete`,
		// The ARGUMENT spelling is a setter too -- PragmaStmt.HasValue cannot
		// tell the two apart, so this is where a blanket "HasValue means
		// argument" rule would show up.
		`PRAGMA secure_delete(1)`,
		`PRAGMA secure_delete`,
		`PRAGMA secure_delete(0)`,
		`PRAGMA secure_delete`,
		// ...and it survives a write and a transaction unchanged.
		`PRAGMA secure_delete=1`,
		`INSERT INTO jm VALUES(1)`,
		`DELETE FROM jm`,
		`PRAGMA secure_delete`,
	} {
		mattnCols, mattnRows, mattnErr := queryAll(mattnDB, sqlText)
		if mattnErr != nil {
			t.Fatalf("%s: C SQLite errored, which it never does here: %v", sqlText, mattnErr)
		}
		pureCols, pureRows, pureErr := queryAll(pureDB, sqlText)
		if pureErr != nil {
			t.Fatalf("%s: expected this engine to ANSWER %v, got error: %v", sqlText, mattnRows, pureErr)
		}
		if strings.Join(pureCols, ",") != strings.Join(mattnCols, ",") {
			t.Errorf("%s: column names diverge: pure=%v real=%v", sqlText, pureCols, mattnCols)
		}
		if fmt.Sprint(pureRows) != fmt.Sprint(mattnRows) {
			t.Errorf("%s: rows diverge: pure=%v real=%v", sqlText, pureRows, mattnRows)
		}
	}
}

// TestPragmaSecureDeleteDeclines pins the two secure_delete forms that stay
// REFUSED, both of which C SQLite accepts -- so a decline, not a mismatch,
// is the pass condition.
//
// A SCHEMA-QUALIFIED setter sets only that database where the bare form sets
// them all: verified, "PRAGMA main.secure_delete=ON" leaves the oracle's
// "PRAGMA temp.secure_delete" at 0, and securedel.test measures exactly that
// against an ATTACHed db2. That used to be declined here, because the
// connection kept ONE value for every database and a qualified setter would
// have made them disagree. It keeps a value PER DATABASE now, plus a default a
// later ATTACH inherits (PragmaConnState.SecureDelete) -- see
// secure_delete_per_db_test.go for the measured rules -- so the qualified
// setters below are accepted and only the VALUE spellings remain declined.
//
// The value spellings outside "fast" and the canonical booleans are SERVED
// now: C SQLite reads "2" and "3" as 1 but "-1" and "xyzzy" as 0, which is
// sqlite3GetBoolean's parse, and that is ported (pragmaGetBoolean) rather
// than guessed at. So this compares the RESULTING setting against the oracle
// instead of asserting a decline.
func TestPragmaSecureDeleteDeclines(t *testing.T) {
	for _, sqlText := range []string{
		`PRAGMA secure_delete=2`,
		`PRAGMA secure_delete=3`,
		`PRAGMA secure_delete=-1`,
		`PRAGMA secure_delete=xyzzy`,
	} {
		t.Run(sqlText, func(t *testing.T) {
			pureDB, mattnDB := openJournalModePair(t, false)
			mattnCols, mattnRows, mattnErr := queryAll(mattnDB, sqlText)
			if mattnErr != nil {
				t.Fatalf("%s: C SQLite errored, so this fixture no longer reaches the shape: %v", sqlText, mattnErr)
			}
			pureCols, pureRows, err := queryAll(pureDB, sqlText)
			if err != nil {
				t.Fatalf("%s: expected this engine to ANSWER, got %v", sqlText, err)
			}
			if fmt.Sprint(pureCols) != fmt.Sprint(mattnCols) || fmt.Sprint(pureRows) != fmt.Sprint(mattnRows) {
				t.Errorf("%s: pure=%v%v real=%v%v", sqlText, pureCols, pureRows, mattnCols, mattnRows)
			}
			_, mattnAfter, merr := queryAll(mattnDB, `PRAGMA secure_delete`)
			if merr != nil {
				t.Fatalf("C SQLite: PRAGMA secure_delete: %v", merr)
			}
			_, pureAfter, perr := queryAll(pureDB, `PRAGMA secure_delete`)
			if perr != nil {
				t.Fatalf("%s then PRAGMA secure_delete: %v", sqlText, perr)
			}
			if fmt.Sprint(pureAfter) != fmt.Sprint(mattnAfter) {
				t.Errorf("after %s: pure reads back %v, real %v", sqlText, pureAfter, mattnAfter)
			}
			return
		})
	}
}

func testPragmaSecureDeleteDeclinesUnused(t *testing.T) {
	for _, sqlText := range []string{`PRAGMA secure_delete=2`} {
		t.Run(sqlText, func(t *testing.T) {
			pureDB, mattnDB := openJournalModePair(t, false)
			if _, _, err := queryAll(mattnDB, sqlText); err != nil {
				t.Fatalf("%s: C SQLite errored, so this is not a decline case: %v", sqlText, err)
			}
			if cols, rows, err := queryAll(pureDB, sqlText); err == nil {
				t.Fatalf("%s: expected this engine to DECLINE, got cols=%v rows=%v", sqlText, cols, rows)
			}
			// ...and it declines WHOLE: the setting is exactly where it was, so
			// a corpus that mirrors the skip onto its oracle connection (which
			// is what makes a decline safe at all) stays in lockstep afterwards.
			// The mattnDB above deliberately DID run the statement, so it is not
			// the connection to compare against here.
			_, after, err := queryAll(pureDB, `PRAGMA secure_delete`)
			if err != nil {
				t.Fatalf("%s then PRAGMA secure_delete: %v", sqlText, err)
			}
			if fmt.Sprint(after) != "[[0]]" {
				t.Errorf("%s left secure_delete at %v, want the untouched default 0 -- a partial apply", sqlText, after)
			}
		})
	}
}

// TestPragmaSecureDeleteAcrossAttach replays securedel.test's own shape -- a
// BARE setter followed by a getter on an ATTACHed database -- which is what
// makes the qualified getter answerable at all: a bare setter sets every
// attached database at once, so one connection-wide value stays exact for it.
// The qualifier still has to RESOLVE: before the ATTACH both engines raise
// "unknown database db2".
//
// It is a separate test because each engine needs its own ATTACH path, which
// openJournalModePair's shared statement text cannot carry.
func TestPragmaSecureDeleteAcrossAttach(t *testing.T) {
	stmts := []string{
		`PRAGMA db2.secure_delete`, // before the ATTACH: an error on both
		`ATTACH`,                   // substituted per engine below
		`PRAGMA secure_delete`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA secure_delete=ON`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA secure_delete=FAST`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA secure_delete=OFF`,
		`PRAGMA db2.secure_delete`,
	}
	rendered := map[string][]string{}
	for _, drv := range []string{driver.DriverName, "sqlite3"} {
		dir := t.TempDir()
		db, err := sql.Open(drv, filepath.Join(dir, "sd.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE t(a)`); err != nil {
			t.Fatal(err)
		}
		for _, sqlText := range stmts {
			if sqlText == `ATTACH` {
				if _, err := db.Exec(`ATTACH '` + filepath.Join(dir, "db2.sqlite") + `' AS db2`); err != nil {
					t.Fatalf("%s: ATTACH: %v", drv, err)
				}
				continue
			}
			cols, rows, err := queryAll(db, sqlText)
			if err != nil {
				rendered[drv] = append(rendered[drv], sqlText+" -> error")
				continue
			}
			rendered[drv] = append(rendered[drv], fmt.Sprintf("%s -> %v%v", sqlText, cols, rows))
		}
	}
	for i, want := range rendered["sqlite3"] {
		if got := rendered[driver.DriverName][i]; got != want {
			t.Errorf("statement %d diverges:\n  pure: %s\n  real: %s", i, got, want)
		}
	}
}

// TestPragmaSecureDeleteIsConnectionState pins that the value is NOT stored in
// the file: a second connection onto the same database reads back the default,
// exactly as C SQLite does. Getting this wrong would be a wrong answer no
// single-connection test could see.
func TestPragmaSecureDeleteIsConnectionState(t *testing.T) {
	dir := t.TempDir()
	for _, drv := range []struct{ name, dsn string }{
		{driver.DriverName, filepath.Join(dir, "sd.pure.sqlite")},
		{"sqlite3", filepath.Join(dir, "sd.mattn.sqlite")},
	} {
		t.Run(drv.name, func(t *testing.T) {
			first, err := sql.Open(drv.name, drv.dsn)
			if err != nil {
				t.Fatal(err)
			}
			first.SetMaxOpenConns(1)
			defer first.Close()
			if _, err := first.Exec(`CREATE TABLE t(a)`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := queryAll(first, `PRAGMA secure_delete=1`); err != nil {
				t.Fatal(err)
			}
			second, err := sql.Open(drv.name, drv.dsn)
			if err != nil {
				t.Fatal(err)
			}
			second.SetMaxOpenConns(1)
			defer second.Close()
			var got int
			if err := second.QueryRow(`PRAGMA secure_delete`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != 0 {
				t.Errorf("a second connection reports secure_delete=%d, want 0 -- it is connection state, not file state", got)
			}
		})
	}
}

// TestPragmaZeroRowSetterShapeMatchesCSQLite pins the pragmas whose
// ASSIGNMENT form returns an EMPTY result set while their value-less getter
// returns one row -- and, just as importantly, that the assignment still takes
// EFFECT when it is run through Query rather than Exec.
//
// This was a KNOWN OPEN WRONG ANSWER: the setters answered a row, and the
// mined-TCL corpus structurally cannot see it (it routes every PRAGMA to the
// exec side, where rows are never compared). The tempting fix -- suppressing
// the row on the read side -- is worse than the bug, because that is also where
// the setter would stop being APPLIED: "PRAGMA user_version = 7" sent via Query
// would silently do nothing, trading a cosmetic row for a DROPPED WRITE. So the
// driver routes such an assignment through the write path and answers empty
// (driver's zeroRowSetterPragmas), and the readback below is what proves
// the write still happened.
//
// The list is per-NAME on purpose: data_version/journal_mode/locking_mode
// report the resulting value back instead, and PragmaStmt.HasValue is true for
// an ARGUMENT too (table_info(t), foreign_key_list(t)), which must keep its rows.
func TestPragmaZeroRowSetterShapeMatchesCSQLite(t *testing.T) {
	for _, tc := range []struct {
		set    string
		getter string
		want   string // the value the getter must report afterwards, "" to skip
	}{
		{set: `PRAGMA user_version = 7`, getter: `PRAGMA user_version`, want: "7"},
		{set: `PRAGMA user_version = 0`, getter: `PRAGMA user_version`, want: "0"},
		{set: `PRAGMA schema_version = 9`, getter: `PRAGMA schema_version`},
		{set: `PRAGMA foreign_keys = 1`, getter: `PRAGMA foreign_keys`, want: "1"},
		{set: `PRAGMA foreign_keys = 0`, getter: `PRAGMA foreign_keys`, want: "0"},
		{set: `PRAGMA encoding = 'UTF-8'`, getter: `PRAGMA encoding`, want: "UTF-8"},
		{set: `PRAGMA page_size = 4096`, getter: `PRAGMA page_size`, want: "4096"},
	} {
		tc := tc
		t.Run(tc.set, func(t *testing.T) {
			pureDB, mattnDB := openJournalModePair(t, false)

			mattnCols, mattnRows, mattnErr := queryShape(mattnDB, tc.set)
			if mattnErr != nil {
				t.Fatalf("%s: C SQLite errored: %v", tc.set, mattnErr)
			}
			if len(mattnCols) != 0 || mattnRows != 0 {
				t.Fatalf("%s: C SQLite is not a zero-row setter after all (cols=%v rows=%d) -- "+
					"the name does not belong in zeroRowSetterPragmas", tc.set, mattnCols, mattnRows)
			}
			pureCols, pureRows, pureErr := queryShape(pureDB, tc.set)
			if pureErr != nil {
				t.Fatalf("%s: this engine errored: %v", tc.set, pureErr)
			}
			if len(pureCols) != 0 || pureRows != 0 {
				t.Errorf("%s: setter shape diverges: this engine cols=%v rows=%d, C SQLite returns nothing",
					tc.set, pureCols, pureRows)
			}
			if tc.want == "" {
				return
			}
			// ...and it really took effect, which is the half a row-suppressing
			// fix on the read side would have broken.
			var got string
			if err := pureDB.QueryRow(tc.getter).Scan(&got); err != nil {
				t.Fatalf("%s then %s: %v", tc.set, tc.getter, err)
			}
			if got != tc.want {
				t.Errorf("%s then %s: got %q, want %q -- the assignment was dropped", tc.set, tc.getter, got, tc.want)
			}
		})
	}
}
