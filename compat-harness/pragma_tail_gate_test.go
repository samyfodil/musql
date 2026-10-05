// Gate for pragma acceptance/rejection, comparing against C SQLite in lockstep.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// pragmaTailStep is one statement of a lockstep script. A PRAGMA is not a
// "query" by tclIsQuery's leading-verb rule (the corpus runs every one of them
// through Exec, whose rows are discarded), so a step that must compare CELLS
// says so explicitly.
type pragmaTailStep struct {
	sql string
	// rows, when true, additionally compares the statement's result set
	// cell-for-cell against the oracle's -- run through the engine's read side,
	// which is the surface a driver's Query reaches.
	rows bool
}

func pstep(sqlText string) pragmaTailStep     { return pragmaTailStep{sql: sqlText} }
func pstepRows(sqlText string) pragmaTailStep { return pragmaTailStep{sql: sqlText, rows: true} }

// runPragmaTailScript replays steps on a fresh engine.DB and a fresh cgo
// connection, requiring identical accept/reject decisions throughout and
// identical cells for every step marked with pstepRows.
func runPragmaTailScript(t *testing.T, steps []pragmaTailStep) {
	t.Helper()
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1) // one logical connection: connection state must span statements

	for i, step := range steps {
		if !tclIsQuery(step.sql) {
			execErr, panicked, panicVal := tclSafeExecArgs(godb, step.sql)
			if panicked {
				t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, step.sql, panicVal)
			}
			_, cerr := cgodb.Exec(step.sql)
			if (execErr != nil) != (cerr != nil) {
				t.Fatalf("stmt #%d %q: exec error disagreement\n  go:  %v\n  cgo: %v", i, step.sql, execErr, cerr)
			}
			if !step.rows || execErr != nil {
				continue
			}
		}
		// The read side, which is the surface a driver's Query reaches: for a
		// PRAGMA this is a SECOND run of the same statement, which is exactly
		// what makes it a getter check rather than a re-application.
		p, err := godb.SnapshotPager()
		if err != nil {
			t.Fatalf("stmt #%d %q: SnapshotPager: %v", i, step.sql, err)
		}
		goCols, goRows, qerr := p.QueryArgs(step.sql, nil)
		cgoCols, cgoRows, cqerr := tclRunCGOQuery(cgodb, step.sql)
		if (qerr != nil) != (cqerr != nil) {
			t.Fatalf("stmt #%d %q: query error disagreement\n  go:  %v\n  cgo: %v", i, step.sql, qerr, cqerr)
		}
		if qerr != nil {
			continue
		}
		if ok, reason := queryResultsMatch(goCols, tclStringRows(goRows), cgoCols, cgoRows, true); !ok {
			t.Fatalf("stmt #%d %q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v",
				i, step.sql, reason, goCols, goRows, cgoCols, cgoRows)
		}
	}
}

// ---- PRAGMA temp.cache_size ----

// TestPragmaTempCacheSize pins the rule engine/temp_schema.go's
// declineFileScopedTempPragma now applies to cache_size: the temp database
// really does have its own value in C SQLite (0 by default, against main's
// -2000), but the value is a page-cache memory hint that changes no statement's
// rows or errors -- which is exactly why the UNQUALIFIED form is already an
// accepted no-op. So the qualifier selects between two identical nothings.
func TestPragmaTempCacheSize(t *testing.T) {
	// The corpus's own uses, verbatim (pragma.test, pragma2.test,
	// temptable2.test, syscall.test), plus a battery that must stay unaffected.
	runPragmaTailScript(t, []pragmaTailStep{
		pstep(`PRAGMA main.cache_size = 10`),
		pstep(`PRAGMA temp.cache_size = 10`),
		pstep(`PRAGMA temp.cache_size = 1`),
		pstep(`PRAGMA temp.cache_size = 400`),
		pstep(`PRAGMA temp.cache_size=2000`),
		pstep(`PRAGMA temp.cache_size = 1000`),
		pstep(`PRAGMA temp.cache_size`),
		pstep(`PRAGMA temp.cache_size = -100`),
		pstep(`PRAGMA temp.cache_size = 0`),
		// The battery: rows, types, ordering, a temp table and integrity_check
		// must all read the same with the hint set as without it.
		pstep(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`),
		pstep(`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`),
		pstepRows(`SELECT a, b, typeof(a), typeof(b) FROM t ORDER BY a`),
		pstep(`CREATE TEMP TABLE tt(a, b)`),
		pstep(`INSERT INTO tt VALUES(1,2),(3,4)`),
		pstepRows(`SELECT * FROM tt ORDER BY a`),
		pstepRows(`SELECT count(*) FROM t`),
		pstepRows(`PRAGMA integrity_check`),
		pstepRows(`SELECT a FROM t ORDER BY a DESC`),
	})
}

// TestPragmaTempCacheSizeReportsWhatWasSet pins the getter, which used to
// decline here. The reason it declined was exact and worth keeping in view:
// "this engine tracks no value to report, and a hardcoded -2000 would lie the
// moment a script set one". Nothing is hardcoded now -- the value the setter
// stored is the value the getter reports, per database (pragma_tuning.go), so
// the condition that decline was protecting is satisfied rather than bypassed.
//
// The defaults it checks are the measured ones and they are NOT uniform: temp
// starts at 0 where main starts at -2000.
func TestPragmaTempCacheSizeReportsWhatWasSet(t *testing.T) {
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if _, _, err := godb.ExecArgs(`PRAGMA temp.cache_size = 10`, nil); err != nil {
		t.Fatalf("PRAGMA temp.cache_size = 10: expected an accept, got %v", err)
	}
	p, err := godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	// temp carries what was just set; main is untouched at its own default.
	for _, tc := range []struct {
		q    string
		want int64
	}{
		{`PRAGMA temp.cache_size`, 10},
		{`PRAGMA cache_size`, -2000},
		{`PRAGMA main.cache_size`, -2000},
	} {
		_, rows, err := p.QueryArgs(tc.q, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].I != tc.want {
			t.Fatalf("%s: want one row [%d], got %v", tc.q, tc.want, rows)
		}
	}
	// page_count over a temp database that has never held an object is 0, not
	// unanswerable -- C SQLite creates the temp database lazily and writes
	// nothing into it until an object needs a page, so sqlite3BtreeLastPage()
	// is 0 there. The oracle answers one row [0]; this path ACCEPTS the
	// statement (Exec discards the row).
	//
	// KNOWN GAP, deliberately not asserted here: the QUERY path still declines
	// it, because r35aEmptyTempPragmaResult needs a write session to know the
	// temp database never held an object and a read-only pager has none. That
	// is an over-decline (a gap), not a wrong answer -- but the two paths
	// disagreeing is itself a defect worth closing.
	if _, _, err := godb.ExecArgs(`PRAGMA temp.page_count`, nil); err != nil {
		t.Fatalf("PRAGMA temp.page_count: expected acceptance, got %v", err)
	}
}

// ---- PRAGMA temp.journal_mode ----

// TestPragmaTempJournalMode gates the rule that replaced this pragma's decline.
// Its premise ("reads this engine's single database header") was FALSE: real
// SQLite reads no header for it either -- the temp database's journal mode is
// pure connection state with its own default. See engine's
// tempJournalModeResult for the full table; this replays it in lockstep.
func TestPragmaTempJournalMode(t *testing.T) {
	t.Run("default-and-the-unqualified-setter", func(t *testing.T) {
		// jrnlmode.test's own 1.0/1.2/1.7/1.7.2 sequence: the UNQUALIFIED setter
		// moves temp along with main, and a QUALIFIED main setter does not.
		runPragmaTailScript(t, []pragmaTailStep{
			pstepRows(`PRAGMA temp.journal_mode`),
			pstepRows(`PRAGMA journal_mode`),
			pstepRows(`PRAGMA main.journal_mode`),
			pstep(`PRAGMA journal_mode = persist`),
			pstepRows(`PRAGMA journal_mode`),
			pstepRows(`PRAGMA main.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA journal_mode = delete`),
			pstepRows(`PRAGMA Temp.journal_mode`),
			pstep(`PRAGMA journal_mode = truncate`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA journal_mode = memory`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA main.journal_mode = persist`),
			pstepRows(`PRAGMA main.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode`),
		})
	})
	t.Run("the-temp-qualified-setter-moves-temp-alone", func(t *testing.T) {
		runPragmaTailScript(t, []pragmaTailStep{
			pstep(`PRAGMA temp.journal_mode = memory`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstepRows(`PRAGMA journal_mode`),
			pstepRows(`PRAGMA main.journal_mode`),
			pstep(`PRAGMA Temp.journal_mode = TRUNCATE`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA temp.journal_mode = 'persist'`),
			pstepRows(`PRAGMA TEMP.JOURNAL_MODE`),
			pstep(`PRAGMA journal_mode = delete`),
			pstepRows(`PRAGMA temp.journal_mode`),
		})
	})
	t.Run("wal-and-an-unrecognized-name-are-refused-silently", func(t *testing.T) {
		// walmode-5.3.1..5.3.4 verbatim: the temp database can never be WAL, and
		// the setter reports the mode it stayed in rather than erroring.
		runPragmaTailScript(t, []pragmaTailStep{
			pstepRows(`PRAGMA temp.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode = wal`),
			pstep(`BEGIN`),
			pstep(`CREATE TEMP TABLE t1(a, b)`),
			pstep(`INSERT INTO t1 VALUES(1, 2)`),
			pstep(`COMMIT`),
			pstep(`SELECT * FROM t1`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode = wal`),
			pstep(`INSERT INTO t1 VALUES(3, 4)`),
			pstep(`SELECT * FROM t1`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode = xyz`),
			pstep(`PRAGMA temp.journal_mode = off`),
			pstepRows(`PRAGMA temp.journal_mode = wal`),
			pstepRows(`PRAGMA temp.journal_mode`),
		})
	})
	t.Run("main-going-wal-leaves-temp-alone", func(t *testing.T) {
		runPragmaTailScript(t, []pragmaTailStep{
			pstep(`PRAGMA journal_mode=wal`),
			pstepRows(`PRAGMA journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA journal_mode=persist`),
			pstepRows(`PRAGMA journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode`),
		})
	})
	t.Run("with-attachments", func(t *testing.T) {
		// jrnlmode.test 1.9-1.13's own shape: the unqualified setter has to reach
		// main, every attachment AND temp, while each qualified one moves only
		// what it names. This is also the routing check -- a "temp."-qualified
		// pragma must NOT be mistaken for an attached database's.
		runPragmaTailScript(t, []pragmaTailStep{
			pstep(`ATTACH ':memory:' AS aux1`),
			// The temp-qualified getter first, exactly as jrnlmode.test's own
			// segment opens with it -- and that is load-bearing, not incidental:
			// it is what OPENS the temp database, and the unqualified setter
			// below reaches only open databases.
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA journal_mode = PERSIST`),
			pstepRows(`PRAGMA main.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA aux1.journal_mode = DELETE`),
			pstepRows(`PRAGMA main.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA journal_mode = delete`),
			pstepRows(`PRAGMA main.journal_mode`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`PRAGMA temp.journal_mode = truncate`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstepRows(`PRAGMA main.journal_mode`),
		})
	})
	t.Run("a-temp-table-does-not-move-it", func(t *testing.T) {
		runPragmaTailScript(t, []pragmaTailStep{
			pstep(`CREATE TEMP TABLE tt(x)`),
			pstep(`INSERT INTO tt VALUES(1)`),
			pstepRows(`PRAGMA temp.journal_mode`),
			pstep(`DROP TABLE tt`),
			pstepRows(`PRAGMA temp.journal_mode`),
		})
	})
}

// TestPragmaTempJournalModeOpenerRule gates the rule that makes the
// every-database setter conditional: an UNQUALIFIED "PRAGMA journal_mode = X"
// reaches only the databases that are OPEN, and SQLite opens the temp one
// lazily. The first half runs the oracle side of the opener table in
// tempJournalModeResult's doc comment, so the evidence stays live; the second
// pins what this engine does with it.
func TestPragmaTempJournalModeOpenerRule(t *testing.T) {
	for _, tc := range []struct {
		name, opener, want string
	}{
		{"nothing", ``, "delete"},
		{"the-getter-itself", `PRAGMA temp.journal_mode`, "persist"},
		{"another-temp-qualified-pragma", `PRAGMA temp.cache_size`, "persist"},
		{"a-temp-table", `CREATE TEMP TABLE tt(x)`, "persist"},
		{"a-temp-master-read", `SELECT count(*) FROM sqlite_temp_master`, "persist"},
		{"temp-store-is-not-an-opener", `PRAGMA temp_store = file`, "delete"},
	} {
		t.Run("oracle/"+tc.name, func(t *testing.T) {
			cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer cgodb.Close()
			cgodb.SetMaxOpenConns(1)
			if tc.opener != "" {
				if _, err := cgodb.Exec(tc.opener); err != nil {
					t.Fatalf("%s: %v", tc.opener, err)
				}
			}
			if _, err := cgodb.Exec(`PRAGMA journal_mode = persist`); err != nil {
				t.Fatalf("setter: %v", err)
			}
			_, rows, err := tclRunCGOQuery(cgodb, `PRAGMA temp.journal_mode`)
			if err != nil {
				t.Fatalf("getter: %v", err)
			}
			if len(rows) != 1 || rows[0][0] != "T:"+tc.want {
				t.Fatalf("after %q the oracle answers %v, want %s -- the opener rule has changed; re-derive tempJournalModeResult", tc.opener, rows, tc.want)
			}
		})
	}
	// This engine proves the temp database is open only after a temp-qualified
	// journal_mode statement of its own. Before that an unqualified setter
	// leaves the answer unknowable and it declines -- never guesses.
	t.Run("engine-declines-what-it-cannot-prove", func(t *testing.T) {
		godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		defer godb.Discard()
		if _, _, err := godb.ExecArgs(`PRAGMA journal_mode = persist`, nil); err != nil {
			t.Fatalf("setter: %v", err)
		}
		_, _, err = godb.ExecArgs(`PRAGMA temp.journal_mode`, nil)
		if err == nil {
			t.Fatalf("expected a decline: the setter may or may not have reached the temp database")
		}
		if !strings.Contains(err.Error(), "had not yet opened the temp database") {
			t.Fatalf("decline text %q does not carry the reason", err)
		}
	})
	// "= delete" is the exception: it leaves the reading at "delete" whether or
	// not the setter reached temp, so it stays answerable.
	t.Run("delete-stays-knowable", func(t *testing.T) {
		runPragmaTailScript(t, []pragmaTailStep{
			pstep(`PRAGMA journal_mode = delete`),
			pstepRows(`PRAGMA temp.journal_mode`),
		})
	})
}

// TestPragmaTempJournalModeMemoryStoreDeclined pins the one state this pragma
// still declines: "PRAGMA temp_store=MEMORY" makes C SQLite's temp database
// memory-backed, after which it answers "memory", stops following the
// unqualified setter, and returns to "delete" -- not to main's mode -- when
// temp_store goes back to FILE. temp_store itself is accepted here as a pure
// no-op, so answering "delete" from then on would be WRONG rather than
// approximate.
func TestPragmaTempJournalModeMemoryStoreDeclined(t *testing.T) {
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if _, _, err := godb.ExecArgs(`PRAGMA temp.journal_mode`, nil); err != nil {
		t.Fatalf("PRAGMA temp.journal_mode before temp_store: %v", err)
	}
	for _, s := range []string{`PRAGMA temp_store=MEMORY`, `PRAGMA temp_store=2`} {
		godb2, err := engine.Create(filepath.Join(t.TempDir(), s[:14]+".db"))
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		if _, _, err := godb2.ExecArgs(s, nil); err != nil {
			godb2.Discard()
			t.Fatalf("%s: %v", s, err)
		}
		_, _, err = godb2.ExecArgs(`PRAGMA temp.journal_mode`, nil)
		godb2.Discard()
		if err == nil {
			t.Fatalf("after %s, PRAGMA temp.journal_mode must decline (C SQLite answers \"memory\" there)", s)
		}
		if !strings.Contains(err.Error(), "memory-backed") {
			t.Fatalf("after %s, decline text %q does not carry the reason", s, err)
		}
	}
	// temp_store=FILE/0/1/DEFAULT leaves it file-backed, so the getter still
	// answers -- verified against the oracle, which reports "delete" there.
	godb3, err := engine.Create(filepath.Join(t.TempDir(), "file.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb3.Discard()
	for _, s := range []string{`PRAGMA temp_store=FILE`, `PRAGMA temp_store=1`, `PRAGMA temp.journal_mode`} {
		if _, _, err := godb3.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// TestPragmaTempJournalModeThroughDriver is the same rule across the driver's
// SESSION BOUNDARY -- an autocommit statement runs on a throwaway engine.DB, so
// this is what proves Conn.tempJournalMode carries the mode from the setter's
// session to the next statement's.
func TestPragmaTempJournalModeThroughDriver(t *testing.T) {
	godb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	defer godb.Close()
	godb.SetMaxOpenConns(1) // one connection: the mode is connection state
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	for _, s := range []string{
		`PRAGMA temp.journal_mode`,
		`PRAGMA temp.journal_mode = memory`,
		`PRAGMA temp.journal_mode`,
		`PRAGMA journal_mode`,
		`PRAGMA journal_mode = truncate`,
		`PRAGMA temp.journal_mode`,
		`PRAGMA temp.journal_mode = wal`,
		`PRAGMA temp.journal_mode`,
	} {
		goRows, gerr := tclRunCGOQuery2(godb, s)
		cgoRows, cerr := tclRunCGOQuery2(cgodb, s)
		if (gerr != nil) != (cerr != nil) {
			t.Fatalf("%s: error disagreement\n  go:  %v\n  cgo: %v", s, gerr, cerr)
		}
		if gerr != nil {
			continue
		}
		if ok, reason := queryResultsMatch([]string{"journal_mode"}, goRows, []string{"journal_mode"}, cgoRows, true); !ok {
			t.Fatalf("%s: %s\n  go:  %v\n  cgo: %v", s, reason, goRows, cgoRows)
		}
	}
}

// tclRunCGOQuery2 runs sqlText through any database/sql driver and returns its
// rows in the shared normalized form; a thin wrapper so the musql driver and
// the cgo one can be compared with the same call.
func tclRunCGOQuery2(db *sql.DB, sqlText string) ([][]string, error) {
	_, rows, err := tclRunCGOQuery(db, sqlText)
	return rows, err
}

// ---- PRAGMA trusted_schema ----

// TestPragmaTrustedSchemaOn gates the ON half: it is the default, and setting
// it ON changes nothing on EITHER connection. The OFF half is served too now --
// see trusted_schema_enforcement_test.go, which owns the rule's whole gate --
// so this file keeps only the ON script it always had.
func TestPragmaTrustedSchemaOn(t *testing.T) {
	runPragmaTailScript(t, []pragmaTailStep{
		pstepRows(`PRAGMA trusted_schema`),
		pstep(`PRAGMA trusted_schema=ON`),
		pstepRows(`PRAGMA trusted_schema`),
		pstep(`PRAGMA trusted_schema = 1`),
		pstep(`PRAGMA trusted_schema=On`),
		pstep(`PRAGMA trusted_schema=yes`),
		pstep(`PRAGMA trusted_schema=true`),
		pstepRows(`PRAGMA trusted_schema`),
		// The qualifier is ignored: it is a per-CONNECTION flag.
		pstep(`PRAGMA main.trusted_schema=1`),
		pstepRows(`PRAGMA main.trusted_schema`),
		pstep(`PRAGMA temp.trusted_schema=1`),
		pstepRows(`PRAGMA temp.trusted_schema`),
		// No transaction restriction (unlike synchronous/temp_store).
		pstep(`BEGIN`),
		pstep(`PRAGMA trusted_schema=1`),
		pstep(`COMMIT`),
		pstepRows(`PRAGMA trusted_schema`),
		// ...and a schema object that WOULD be refused with it off keeps
		// working with it on, on both engines. This is the shape that proves
		// the OFF decline is not over-caution: MATCH from inside a view.
		pstep(`CREATE TABLE t(a,b)`),
		pstep(`INSERT INTO t VALUES(1,2)`),
		pstep(`CREATE VIEW v AS SELECT a+b AS s FROM t`),
		pstep(`SELECT * FROM v`),
		pstep(`CREATE VIRTUAL TABLE ft USING fts4(x)`),
		pstep(`INSERT INTO ft VALUES('hello world')`),
		pstep(`CREATE VIEW vm AS SELECT * FROM ft WHERE x MATCH 'hello'`),
		pstep(`SELECT count(*) FROM vm`),
		pstepRows(`PRAGMA trusted_schema`),
	})
}

// TestPragmaTrustedSchemaOffValueParse pins what stays DECLINED about the OFF
// setter now that OFF itself is served (trusted_schema_enforcement_test.go owns
// the enforcement gate): a value spelling outside the canonical booleans, which
// is exactly the line foreign_keys draws -- except that line has moved: real
// SQLite's parse is sqlite3GetBoolean's ("=2" is ON, "=-1"/"=bogus" are OFF)
// and it is PORTED now (pragmaGetBoolean) rather than declined, so the second
// half of this test asserts the answers instead of the declines.
func TestPragmaTrustedSchemaOffValueParse(t *testing.T) {
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	for _, s := range []string{
		`PRAGMA trusted_schema=OFF`, `PRAGMA trusted_schema=off`,
		`PRAGMA trusted_schema=0`, `PRAGMA trusted_schema=Off`,
		`PRAGMA trusted_schema=no`, `PRAGMA trusted_schema=false`,
		`PRAGMA main.trusted_schema=0`, `PRAGMA temp.trusted_schema=0`,
	} {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: expected the setter to be served, got %v", s, err)
		}
	}
	// ...and the non-canonical spellings are SERVED now too, with
	// sqlite3GetBoolean's own answer (pragma.c:97): "=2" is ON, "=-1" and
	// "=bogus" are OFF, because "-" is not a digit and "bogus" is not one of
	// the six words. They were declined while that parse was described as
	// idiosyncratic rather than ported; pragmaGetBoolean ports it.
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{`PRAGMA trusted_schema=2`, true},
		{`PRAGMA trusted_schema=0x10`, true},
		{`PRAGMA trusted_schema=-1`, false},
		{`PRAGMA trusted_schema=bogus`, false},
		{`PRAGMA trusted_schema=256`, false}, // (u8)sqlite3Atoi truncates
	} {
		if _, _, err := godb.ExecArgs(tc.sql, nil); err != nil {
			t.Fatalf("%s: expected the setter to be served, got %v", tc.sql, err)
		}
		if got := godb.TrustedSchema(); got != tc.want {
			t.Errorf("%s left trusted_schema=%v, want %v", tc.sql, got, tc.want)
		}
	}
}

// tclStringRows renders engine rows the way tclSafeGoQuery does, so
// queryResultsMatch can compare them against cgo's.
func tclStringRows(rows [][]engine.Value) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			cells[i] = normalizeEngineValue(v)
		}
		out = append(out, cells)
	}
	return out
}
