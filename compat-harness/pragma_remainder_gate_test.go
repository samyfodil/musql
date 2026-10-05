// Gates pragmas mined from the TCL corpus. Rules are pinned by comparing
// getter cells through the engine's read side against a live oracle, and by
// asserting the oracle's own refusals for deliberately declined shapes so
// any C SQLite changes are noticed rather than silently becoming stale.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	_ "github.com/samyfodil/musql/driver"
)

// ---- PRAGMA application_id ----

// TestPragmaApplicationID verifies the header-backed scalar at offset 68,
// including signed rendering, transactional behavior, and its side effect on
// auto_vacuum initialization.
func TestPragmaApplicationID(t *testing.T) {
	runPragmaTailScript(t, []pragmaTailStep{
		pstepRows(`PRAGMA application_id`),
		pstep(`CREATE TABLE t(x)`),
		pstepRows(`PRAGMA application_id`),
		pstep(`PRAGMA application_id = 1234`),
		pstepRows(`PRAGMA application_id`),
		// pragma.test's own spelling: the function form, mixed case.
		pstep(`PRAGMA Application_ID(12345)`),
		pstepRows(`PRAGMA application_id`),
		// Signed, both extremes of the int32 range.
		pstep(`PRAGMA application_id = -1`),
		pstepRows(`PRAGMA application_id`),
		pstep(`PRAGMA application_id = 2147483647`),
		pstepRows(`PRAGMA application_id`),
		pstep(`PRAGMA application_id = -2147483648`),
		pstepRows(`PRAGMA application_id`),
		pstep(`PRAGMA application_id = 0`),
		pstepRows(`PRAGMA application_id`),
		// It is ordinary transactional state: a ROLLBACK puts it back.
		pstep(`PRAGMA application_id = 55`),
		pstep(`BEGIN`),
		pstep(`PRAGMA application_id = 7`),
		pstepRows(`PRAGMA application_id`),
		pstep(`ROLLBACK`),
		pstepRows(`PRAGMA application_id`),
		// ...and a COMMIT keeps it.
		pstep(`BEGIN`),
		pstep(`PRAGMA application_id = 9`),
		pstep(`COMMIT`),
		pstepRows(`PRAGMA application_id`),
		// Nothing else moved: the neighbouring header scalars are untouched by
		// it (verified on the oracle -- neither schema_version nor data_version
		// changes when application_id is written).
		pstepRows(`PRAGMA user_version`),
		pstepRows(`PRAGMA data_version`),
		pstepRows(`PRAGMA page_size`),
	})
}

// TestPragmaApplicationIDFixesAutoVacuum verifies that application_id setter
// on a fresh database fixes the auto_vacuum flag just as user_version does
// (because both are header-scalar WRITE operations), unlike page_size.
func TestPragmaApplicationIDFixesAutoVacuum(t *testing.T) {
	// The oracle's own rule, re-derived every run: if a header-scalar setter
	// ever stopped fixing the flag, this engine's wroteHeader would be wrong.
	for _, tc := range []struct{ first, wantAutoVacuum string }{
		{`PRAGMA application_id = 7`, `I:0`},
		{`PRAGMA user_version = 7`, `I:0`},
		{`PRAGMA page_size = 8192`, `I:1`},
	} {
		cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
		if err != nil {
			t.Fatalf("sql.Open(sqlite3): %v", err)
		}
		if _, err := cgodb.Exec(tc.first); err != nil {
			cgodb.Close()
			t.Fatalf("oracle %q: %v", tc.first, err)
		}
		if _, err := cgodb.Exec(`PRAGMA auto_vacuum = 1`); err != nil {
			cgodb.Close()
			t.Fatalf("oracle auto_vacuum=1 after %q: %v", tc.first, err)
		}
		rows, err := tclRunCGOQuery2(cgodb, `PRAGMA auto_vacuum`)
		cgodb.Close()
		if err != nil || len(rows) != 1 || rows[0][0] != tc.wantAutoVacuum {
			t.Fatalf("oracle: after %q then auto_vacuum=1, auto_vacuum = %v (err %v), want [[%s]]", tc.first, rows, err, tc.wantAutoVacuum)
		}
	}

	// This engine: application_id must behave exactly as user_version does.
	outcome := func(first string) int64 {
		path := filepath.Join(t.TempDir(), "go.db")
		godb, err := engine.Create(path)
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		if _, _, err := godb.ExecArgs(first, nil); err != nil {
			t.Fatalf("engine %q: %v", first, err)
		}
		if _, _, err := godb.ExecArgs(`PRAGMA auto_vacuum = 1`, nil); err != nil {
			t.Fatalf("engine auto_vacuum=1 after %q: %v", first, err)
		}
		// Make the mode real, then read what the FILE carries -- the same
		// question the oracle half above asks.
		if _, _, err := godb.ExecArgs(`CREATE TABLE t(a)`, nil); err != nil {
			t.Fatalf("engine CREATE after %q: %v", first, err)
		}
		if err := godb.Close(); err != nil {
			t.Fatalf("engine Close after %q: %v", first, err)
		}
		rp, err := engine.Open(path)
		if err != nil {
			t.Fatalf("engine reopen after %q: %v", first, err)
		}
		defer rp.Close()
		_, rows, qerr := rp.Query(`PRAGMA auto_vacuum`)
		if qerr != nil || len(rows) != 1 {
			t.Fatalf("engine PRAGMA auto_vacuum after %q: %v (err %v)", first, rows, qerr)
		}
		return rows[0][0].I
	}
	appID, userVer, pageSize := outcome(`PRAGMA application_id = 7`), outcome(`PRAGMA user_version = 7`), outcome(`PRAGMA page_size = 8192`)
	if appID != userVer {
		t.Errorf("after application_id=7 auto_vacuum reads %d, but after user_version=7 it reads %d -- the two are the same kind of header write in C SQLite, so application_id is not marking DB.wroteHeader", appID, userVer)
	}
	if appID == pageSize {
		t.Errorf("application_id=7 left auto_vacuum at %d, the same as page_size=8192 -- but page_size is NOT a write in C SQLite and application_id is, so wroteHeader is not being set", appID)
	}
	// ...and both match the oracle's own values, measured above.
	if appID != 0 || pageSize != 1 {
		t.Errorf("auto_vacuum after application_id=7 is %d (want 0) and after page_size=8192 is %d (want 1)", appID, pageSize)
	}
}

// TestPragmaApplicationIDSurvivesAMusqlWrite verifies that application_id
// persists after the engine writes to the database and exports it for the
// oracle to read.
func TestPragmaApplicationIDSurvivesAMusqlWrite(t *testing.T) {
	dir := t.TempDir()
	cPath := filepath.Join(dir, "shared.db")

	// C SQLite creates the database and stamps an application id.
	cgodb, err := sql.Open("sqlite3", cPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	cgodb.SetMaxOpenConns(1)
	for _, s := range []string{`CREATE TABLE t(x)`, `INSERT INTO t VALUES(1)`, `PRAGMA application_id = 1234605616`} {
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo %q: %v", s, err)
		}
	}
	if err := cgodb.Close(); err != nil {
		t.Fatalf("cgo Close: %v", err)
	}

	// This engine imports it (RULE #3: it runs on its own format), writes
	// something completely unrelated, and closes...
	path := filepath.Join(dir, "shared.musq")
	if err := sqliteconv.Import(cPath, path, sqliteconv.ImportOptions{}); err != nil {
		t.Fatalf("ImportSQLite: %v", err)
	}
	godb, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	if _, _, err := godb.ExecArgs(`INSERT INTO t VALUES(2)`, nil); err != nil {
		t.Fatalf("engine INSERT: %v", err)
	}
	if err := godb.Close(); err != nil {
		t.Fatalf("engine Close: %v", err)
	}

	// ...and hands it back. Header offset 68 is big-endian, and 1234605616 ==
	// 0x49969630.
	exported := filepath.Join(dir, "exported.db")
	if err := sqliteconv.Export(path, exported, 0); err != nil {
		t.Fatalf("ExportSQLite: %v", err)
	}
	raw, err := os.ReadFile(exported)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := fmt.Sprintf("% x", raw[68:72]); got != "49 96 96 30" {
		t.Errorf("header[68:72] after a musql write = %s, want 49 96 96 30 (the application id C SQLite stored)", got)
	}

	// ...and the oracle, the only reader that matters, agrees.
	cgodb2, err := sql.Open("sqlite3", exported)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3) reopen: %v", err)
	}
	defer cgodb2.Close()
	cgodb2.SetMaxOpenConns(1)
	rows, err := tclRunCGOQuery2(cgodb2, `PRAGMA application_id`)
	if err != nil {
		t.Fatalf("cgo re-read: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != "I:1234605616" {
		t.Errorf("C SQLite reads application_id = %v after a musql write, want [[1234605616]]", rows)
	}

	// The other direction: this engine writes the id, C SQLite reads it.
	other := filepath.Join(t.TempDir(), "go-authored.db")
	gdb, err := engine.Create(other)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{`CREATE TABLE u(y)`, `PRAGMA application_id = -1`} {
		if _, _, err := gdb.ExecArgs(s, nil); err != nil {
			t.Fatalf("engine %q: %v", s, err)
		}
	}
	if err := gdb.Close(); err != nil {
		t.Fatalf("engine Close: %v", err)
	}
	cgodb3, err := sql.Open("sqlite3", exportedForOracle(t, other))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3) go-authored: %v", err)
	}
	defer cgodb3.Close()
	cgodb3.SetMaxOpenConns(1)
	rows, err = tclRunCGOQuery2(cgodb3, `PRAGMA application_id`)
	if err != nil {
		t.Fatalf("cgo read of a go-authored file: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != "I:-1" {
		t.Errorf("C SQLite reads application_id = %v from a musql-authored file, want [[-1]] (the field is signed)", rows)
	}
	if rows, err := tclRunCGOQuery2(cgodb3, `PRAGMA integrity_check`); err != nil || len(rows) != 1 || rows[0][0] != "T:ok" {
		t.Errorf("C SQLite integrity_check on the musql-authored file = %v (err %v), want [[ok]]", rows, err)
	}
}

// TestPragmaApplicationIDDeclinedShapes verifies the oracle's own behavior
// for three deliberately declined shapes: out-of-range values, temp.application_id,
// and operation under query_only mode.
func TestPragmaApplicationIDDeclinedShapes(t *testing.T) {
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)
	if _, err := cgodb.Exec(`CREATE TABLE t(x)`); err != nil {
		t.Fatalf("cgo CREATE: %v", err)
	}
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if _, _, err := godb.ExecArgs(`CREATE TABLE t(x)`, nil); err != nil {
		t.Fatalf("engine CREATE: %v", err)
	}

	// (1) sqlite3Atoi's out-of-range result, straight from the oracle -- and
	// this engine now STORES it rather than declining. sqlite3GetInt32
	// (util.c) fails on anything past 2147483647 (10 digits is its ceiling and
	// "v-neg>2147483647" its bound), and a failed sqlite3GetInt32 leaves
	// sqlite3Atoi's result at 0, which is what PragTyp_HEADER_VALUE writes.
	for _, tc := range []struct{ set, want string }{
		{`4294967295`, `I:0`},
		{`2147483648`, `I:0`},
		{`-2147483649`, `I:0`},
		{`0x7fffffff`, `I:2147483647`}, // the hex branch, at its own ceiling
		{`0x80000000`, `I:0`},          // ...and one past it: the sign bit FAILS
	} {
		if _, err := cgodb.Exec(`PRAGMA application_id = ` + tc.set); err != nil {
			t.Fatalf("cgo set %s: %v", tc.set, err)
		}
		rows, err := tclRunCGOQuery2(cgodb, `PRAGMA application_id`)
		if err != nil || len(rows) != 1 || rows[0][0] != tc.want {
			t.Fatalf("oracle: PRAGMA application_id = %s reads back %v (err %v), want [[%s]] -- the premise of this engine's decline no longer holds", tc.set, rows, err, tc.want)
		}
		execErr, panicked, panicVal := tclSafeExecArgs(godb, `PRAGMA application_id = `+tc.set)
		if panicked {
			t.Fatalf("engine PANICKED on application_id = %s: %v", tc.set, panicVal)
		}
		if execErr != nil {
			t.Errorf("engine DECLINED application_id = %s: %v -- C SQLite stores %s", tc.set, execErr, tc.want)
			continue
		}
		_, got, gerr, gpanicked, gpv := tclSafeGoQuery(godb, `PRAGMA application_id`)
		if gpanicked {
			t.Fatalf("engine PANICKED reading application_id back after %s: %v", tc.set, gpv)
		}
		if gerr != nil || len(got) != 1 || got[0][0] != tc.want {
			t.Errorf("engine: PRAGMA application_id = %s reads back %v (err %v), want [[%s]]", tc.set, got, gerr, tc.want)
		}
	}

	// (2) the temp database really does have its own application id.
	if _, err := cgodb.Exec(`PRAGMA application_id = 100`); err != nil {
		t.Fatalf("cgo set main: %v", err)
	}
	if _, err := cgodb.Exec(`PRAGMA temp.application_id = 200`); err != nil {
		t.Fatalf("cgo set temp: %v", err)
	}
	mainRows, _ := tclRunCGOQuery2(cgodb, `PRAGMA main.application_id`)
	tempRows, _ := tclRunCGOQuery2(cgodb, `PRAGMA temp.application_id`)
	if len(mainRows) != 1 || mainRows[0][0] != "I:100" || len(tempRows) != 1 || tempRows[0][0] != "I:200" {
		t.Fatalf("oracle: main=%v temp=%v, want [[100]] and [[200]] -- if the temp database no longer has its own header, declineFileScopedTempPragma's reason is stale", mainRows, tempRows)
	}
	// Both halves are answerable now: the TEMP database has a header of its own
	// (engine/temp_store.go), so "PRAGMA temp.application_id" reads and writes
	// that file's rather than main's.
	if err, panicked, pv := tclSafeExecArgs(godb, `PRAGMA temp.application_id`); panicked {
		t.Fatalf("engine PANICKED on the temp application_id getter: %v", pv)
	} else if err != nil {
		t.Errorf("engine DECLINED \"PRAGMA temp.application_id\", which C SQLite answers: %v", err)
	}
	if err, panicked, pv := tclSafeExecArgs(godb, `PRAGMA temp.application_id = 3`); panicked {
		t.Fatalf("engine PANICKED on the temp application_id setter: %v", pv)
	} else if err != nil {
		t.Errorf("engine DECLINED \"PRAGMA temp.application_id = 3\", which C SQLite applies: %v", err)
	}

	// (3) query_only refuses the setter on both engines, and allows the getter.
	if _, err := cgodb.Exec(`PRAGMA query_only = 1`); err != nil {
		t.Fatalf("cgo query_only: %v", err)
	}
	if _, err := cgodb.Exec(`PRAGMA application_id = 5`); err == nil {
		t.Fatalf("oracle ACCEPTED application_id=5 under query_only -- the premise of this engine's refusal is stale")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("oracle refused application_id=5 under query_only with %q, want a readonly error", err)
	}
	if _, _, err := godb.ExecArgs(`PRAGMA query_only = 1`, nil); err != nil {
		t.Fatalf("engine query_only: %v", err)
	}
	if _, _, err := godb.ExecArgs(`PRAGMA application_id = 5`, nil); err == nil {
		t.Errorf("engine ACCEPTED application_id=5 under query_only; C SQLite raises SQLITE_READONLY there")
	}
	if _, _, err := godb.ExecArgs(`PRAGMA application_id`, nil); err != nil {
		t.Errorf("engine refused the application_id GETTER under query_only: %v (C SQLite allows it)", err)
	}
}

// TestPragmaApplicationIDQualifiedIsRouted verifies that qualified pragmas are
// per-database, so "PRAGMA aux.application_id" affects only aux.
func TestPragmaApplicationIDQualifiedIsRouted(t *testing.T) {
	dir := t.TempDir()
	goMain, goAux := filepath.Join(dir, "go.db"), filepath.Join(dir, "go-aux.db")
	cgoAux := filepath.Join(dir, "cgo-aux.db")

	godb, err := engine.Create(goMain)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	for _, s := range []string{
		`CREATE TABLE t(x)`,
		`ATTACH '%AUX%' AS aux`,
		`CREATE TABLE aux.u(y)`,
		`PRAGMA aux.application_id = 42`,
	} {
		gerr, panicked, pv := tclSafeExecArgs(godb, strings.ReplaceAll(s, "%AUX%", goAux))
		if panicked {
			t.Fatalf("engine PANICKED on %q: %v", s, pv)
		}
		_, cerr := cgodb.Exec(strings.ReplaceAll(s, "%AUX%", cgoAux))
		if (gerr != nil) != (cerr != nil) {
			t.Fatalf("%q: accept/reject disagreement\n  go:  %v\n  cgo: %v", s, gerr, cerr)
		}
		if gerr != nil {
			t.Fatalf("%q: declined by both engines (%v); the routing this test pins never ran", s, gerr)
		}
	}
	// A qualifier names a database this engine's read side has no handle on --
	// a SnapshotPager is main's alone -- so the two databases are compared by
	// opening the FILE the routing wrote, which is where the value had to land.
	if err := godb.Close(); err != nil {
		t.Fatalf("engine Close: %v", err)
	}
	for _, tc := range []struct{ file, oracleGetter string }{
		{goAux, `PRAGMA aux.application_id`},
		{goMain, `PRAGMA main.application_id`},
	} {
		p, err := engine.Open(tc.file)
		if err != nil {
			t.Fatalf("engine.Open(%s): %v", tc.file, err)
		}
		goCols, goRows, qerr := p.QueryArgs(`PRAGMA application_id`, nil)
		p.Close()
		if qerr != nil {
			t.Fatalf("engine read of %s: %v", tc.file, qerr)
		}
		cgoCols, cgoRows, cqerr := tclRunCGOQuery(cgodb, tc.oracleGetter)
		if cqerr != nil {
			t.Fatalf("oracle %q: %v", tc.oracleGetter, cqerr)
		}
		if ok, reason := queryResultsMatch(goCols, tclStringRows(goRows), cgoCols, cgoRows, true); !ok {
			t.Errorf("routed application_id: %s\n  go (%s):  cols=%v rows=%v\n  cgo (%s): cols=%v rows=%v",
				reason, tc.file, goCols, goRows, tc.oracleGetter, cgoCols, cgoRows)
		}
	}
	// The whole point of "routed": aux moved and main did NOT. If C SQLite
	// ever made the qualifier per-connection instead, attachedPragmaScope's
	// routing entry would be wrong, and this is what says so.
	rows, err := tclRunCGOQuery2(cgodb, `PRAGMA main.application_id`)
	if err != nil || len(rows) != 1 || rows[0][0] != "I:0" {
		t.Fatalf("oracle: main.application_id = %v (err %v) after setting aux's, want [[I:0]] -- if the qualifier is no longer per-database, attachedPragmaScope's routing entry is wrong", rows, err)
	}
}

// ---- the header scalars are SIGNED ----

// TestPragmaHeaderScalarsAreSigned verifies that user_version and schema_version
// are rendered as signed int32, not unsigned.
func TestPragmaHeaderScalarsAreSigned(t *testing.T) {
	for _, name := range []string{"user_version", "schema_version", "application_id"} {
		t.Run(name, func(t *testing.T) {
			steps := []pragmaTailStep{pstep(`CREATE TABLE t(x)`)}
			for _, v := range []string{"-1", "-2147483648", "2147483647", "-2", "0", "7"} {
				steps = append(steps,
					pstep(`PRAGMA `+name+` = `+v),
					pstepRows(`PRAGMA `+name),
				)
			}
			runPragmaTailScript(t, steps)
		})
	}
}

// TestPragmaHeaderScalarOutOfRange verifies the oracle stores 0 for out-of-range
// values via sqlite3Atoi, so out-of-range declines are necessary.
func TestPragmaHeaderScalarOutOfRange(t *testing.T) {
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	for _, s := range []string{`CREATE TABLE t(x)`} {
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo %q: %v", s, err)
		}
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("engine %q: %v", s, err)
		}
	}
	for _, name := range []string{"user_version", "schema_version", "application_id"} {
		for _, v := range []string{"2147483648", "4294967295", "-2147483649"} {
			set := `PRAGMA ` + name + ` = ` + v
			if _, err := cgodb.Exec(set); err != nil {
				t.Fatalf("oracle %q: %v", set, err)
			}
			rows, err := tclRunCGOQuery2(cgodb, `PRAGMA `+name)
			if err != nil || len(rows) != 1 || rows[0][0] != "I:0" {
				t.Fatalf("oracle: %q reads back %v (err %v), want [[I:0]] -- sqlite3Atoi no longer stores 0 for an out-of-int32 value, so this engine's decline needs revisiting", set, rows, err)
			}
			// ...and this engine stores 0 there too now: sqlite3Atoi is
			// ported (pragmaGetInt32), so the value C SQLite would hold
			// is the value this one holds, and the decline that stood here
			// was pure loss.
			if execErr, panicked, pv := tclSafeExecArgs(godb, set); panicked {
				t.Fatalf("engine PANICKED on %q: %v", set, pv)
			} else if execErr != nil {
				t.Errorf("engine DECLINED %q: %v -- sqlite3Atoi stores 0 there and so should this", set, execErr)
			} else if _, grows, gerr, gpan, gpv := tclSafeGoQuery(godb, `PRAGMA `+name); gpan {
				t.Fatalf("engine PANICKED reading PRAGMA %s back: %v", name, gpv)
			} else if gerr != nil {
				t.Errorf("engine: PRAGMA %s after %q: %v", name, set, gerr)
			} else if len(grows) != 1 || len(grows[0]) != 1 || grows[0][0] != "I:0" {
				t.Errorf("engine: after %q, PRAGMA %s reads back %v, want [[I:0]] (the oracle's answer)", set, name, grows)
			}
		}
	}
}

// ---- PRAGMA max_page_count ----

// TestPragmaMaxPageCountGetter verifies the baseline value for max_page_count
// when no setter has touched it.
func TestPragmaMaxPageCountGetter(t *testing.T) {
	runPragmaTailScript(t, []pragmaTailStep{
		// A fresh connection, before there is any page at all.
		pstepRows(`PRAGMA max_page_count`),
		pstep(`CREATE TABLE t(x)`),
		pstepRows(`PRAGMA page_count`),
		// ...and after: the limit does not track the page count.
		pstepRows(`PRAGMA max_page_count`),
		pstep(`INSERT INTO t VALUES(1),(2),(3)`),
		pstepRows(`PRAGMA page_count`),
		pstepRows(`PRAGMA max_page_count`),
		// Unaffected by a transaction, and by the qualifier C SQLite
		// resolves to main.
		pstep(`BEGIN`),
		pstepRows(`PRAGMA max_page_count`),
		pstep(`COMMIT`),
		pstepRows(`PRAGMA main.max_page_count`),
		// query_only allows both halves of this one (unlike user_version's).
		pstep(`PRAGMA query_only = 1`),
		pstepRows(`PRAGMA max_page_count`),
		pstep(`PRAGMA query_only = 0`),
	})
}

// ---- through the DRIVER, not just the engine ----

// TestPragmaApplicationIDThroughDriver verifies header scalars work through
// database/sql drivers and the driver's zeroRowSetterPragmas list.
func TestPragmaApplicationIDThroughDriver(t *testing.T) {
	godb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	defer godb.Close()
	godb.SetMaxOpenConns(1)
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	for _, s := range []string{
		`CREATE TABLE t(x)`,
		`PRAGMA application_id = 5`,
		`PRAGMA application_id`,
		`PRAGMA user_version = -3`,
		`PRAGMA user_version`,
		`PRAGMA application_id = -3`,
		`PRAGMA application_id`,
		// max_page_count is not here: it is the one page-shaped pragma this format
		// declines, and TestPragmaAppSurface's decline list gates exactly that.
	} {
		goRows, goErr := tclRunCGOQuery2(godb, s)
		cgoRows, cgoErr := tclRunCGOQuery2(cgodb, s)
		if (goErr != nil) != (cgoErr != nil) {
			t.Fatalf("%q: error disagreement through the driver\n  go:  %v\n  cgo: %v", s, goErr, cgoErr)
		}
		if goErr != nil {
			continue
		}
		if ok, reason := queryResultsMatch(nil, goRows, nil, cgoRows, true); !ok {
			t.Errorf("%q through the driver: %s\n  go:  %v\n  cgo: %v", s, reason, goRows, cgoRows)
		}
	}
}
