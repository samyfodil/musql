package compat

// Differential tests for PRAGMA lock_proxy_file (macOS only).
// Tests platform differences: on Linux it's unknown, on macOS it's real.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestPragmaLockProxyFile is the differential fixture.
func TestPragmaLockProxyFile(t *testing.T) {
	dir := t.TempDir()
	// Use absolute paths in temp dir to avoid stray lock files.
	t.Chdir(dir)
	p1 := filepath.Join(dir, "proxy1")
	p2 := filepath.Join(dir, "proxy2")

	// The getter before anything has been set: C SQLite's proxyFileControl
	// hands back NULL for a file that is not proxy-style (os_unix.c:8243) and
	// returnSingleText then emits no row at all -- but the column is still
	// named (pragma.c:198/225).
	differ(t, "lock_proxy_file unset getter", []string{
		`PRAGMA lock_proxy_file`,
		`PRAGMA main.lock_proxy_file`,
	})

	// Turning it on with an explicit path, and reading it back verbatim.
	differ(t, "lock_proxy_file set and read", []string{
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA lock_proxy_file`,
	})

	// Re-setting the SAME path is proxyFileControl's own short-circuit
	// (os_unix.c:8266): SQLITE_OK, nothing changes.
	differ(t, "lock_proxy_file re-set same path", []string{
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA lock_proxy_file`,
	})

	// A DIFFERENT path with no lock held: switchLockProxyPath takes it.
	differ(t, "lock_proxy_file switch unlocked", []string{
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA lock_proxy_file="` + p2 + `"`,
		`PRAGMA lock_proxy_file`,
	})

	// Empty string is a no-op off, error on.
	differ(t, "lock_proxy_file empty off", []string{
		`PRAGMA lock_proxy_file=""`,
		`PRAGMA lock_proxy_file`,
	})
	differ(t, "lock_proxy_file empty on", []string{
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA lock_proxy_file=""`,
		`PRAGMA lock_proxy_file`,
	})

	// Switch under held read lock is refused.
	differ(t, "lock_proxy_file switch under a held read lock", []string{
		`CREATE TABLE t(x)`,
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`BEGIN`,
		`SELECT * FROM t`,
		`PRAGMA lock_proxy_file="` + p2 + `"`,
		`COMMIT`,
		`PRAGMA lock_proxy_file`,
	})

	// Transform under held read lock is refused.
	differ(t, "lock_proxy_file transform under a held read lock", []string{
		`CREATE TABLE t(x)`,
		`BEGIN`,
		`SELECT * FROM t`,
		`PRAGMA lock_proxy_file="mine"`,
		`COMMIT`,
		`PRAGMA lock_proxy_file`,
	})

	// No-change set under held read lock succeeds.
	differ(t, "lock_proxy_file no-change set under a held read lock", []string{
		`CREATE TABLE t(x)`,
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`BEGIN`,
		`SELECT * FROM t`,
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA lock_proxy_file=":auto:"`,
		`COMMIT`,
		`PRAGMA lock_proxy_file`,
	})

	// Set is refused under WAL.
	differ(t, "lock_proxy_file under WAL", []string{
		`CREATE TABLE t(x)`,
		`PRAGMA journal_mode=WAL`,
		`SELECT * FROM t`,
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA lock_proxy_file`,
	})

	// Value is taken verbatim as a path.
	differ(t, "lock_proxy_file numeric value", []string{
		`PRAGMA lock_proxy_file=1`,
		`PRAGMA lock_proxy_file`,
	})
	differ(t, "lock_proxy_file bare identifier value", []string{
		`PRAGMA lock_proxy_file=mine`,
		`PRAGMA lock_proxy_file`,
	})
	// temp. is an error.
	differ(t, "lock_proxy_file temp", []string{
		`PRAGMA temp.lock_proxy_file`,
		`PRAGMA temp.lock_proxy_file="` + p1 + `"`,
		`PRAGMA temp.lock_proxy_file=""`,
		`PRAGMA temp.lock_proxy_file`,
	})
	// Unknown schema is an error on both sides.
	differ(t, "lock_proxy_file unknown schema", []string{
		`PRAGMA nosuch.lock_proxy_file`,
	})
	// Call syntax is a setter.
	differ(t, "lock_proxy_file call syntax", []string{
		`PRAGMA lock_proxy_file("` + p1 + `")`,
		`PRAGMA lock_proxy_file`,
	})
}

// TestPragmaLockProxyFileDisablesWAL ensures proxy locking disables WAL.
func TestPragmaLockProxyFileDisablesWAL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("no lock_proxy_file in this C build (SQLITE_ENABLE_LOCKING_STYLE)")
	}
	dir := t.TempDir()
	p1 := filepath.Join(dir, "proxy1")
	differ(t, "proxy locking disables WAL, silently", []string{
		`CREATE TABLE t(x)`,
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`PRAGMA journal_mode=WAL`,
		`INSERT INTO t VALUES(1)`,
		`SELECT * FROM t`,
		`PRAGMA journal_mode`,
	})
	// Also via ":auto:".
	differ(t, "proxy locking via :auto: disables WAL too", []string{
		`CREATE TABLE t2(x)`,
		`PRAGMA lock_proxy_file=":auto:"`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA journal_mode`,
	})
	// Control: WAL works without proxy locking.
	differ(t, "CONTROL: WAL is still reachable without proxy locking", []string{
		`CREATE TABLE t3(x)`,
		`PRAGMA journal_mode=WAL`,
		`INSERT INTO t3 VALUES(1)`,
		`SELECT * FROM t3`,
		`PRAGMA journal_mode`,
	})
}

func TestPragmaLockProxyFileAutoGetterIsDeclined(t *testing.T) {
	stmts := []string{`PRAGMA lock_proxy_file=":auto:"`, `PRAGMA lock_proxy_file`}
	mine := run(t, "musql", stmts)
	oracle := run(t, "cgo", stmts)

	if runtime.GOOS != "darwin" {
		// No pragma in non-Apple C build.
		got, _ := json.Marshal(mine)
		want, _ := json.Marshal(oracle)
		if string(got) != string(want) {
			t.Errorf("off darwin the pragma must be absent on both sides\n  cgo:    %s\n  musql: %s", want, got)
		}
		return
	}

	if mine[0]["kind"] != "rows" {
		t.Fatalf(`PRAGMA lock_proxy_file=":auto:" must be ACCEPTED: %v`, mine[0])
	}
	if mine[1]["kind"] != "error" {
		t.Errorf("the :auto: getter must decline, got %v", mine[1])
	}
	// Oracle answers the generated path.
	rows, _ := oracle[1]["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected the oracle to answer one row: %v", oracle[1])
	}
	cell := rows[0].([]any)[0].(string)
	if !strings.HasSuffix(cell, ":auto:") || !strings.Contains(cell, "sqliteplocks") {
		t.Errorf("oracle's :auto: path looks unlike proxyGetLockPath's: %q", cell)
	}
}

// TestPragmaLockProxyFileConservativeRefusals records where this engine is stricter than C SQLite.
func TestPragmaLockProxyFileConservativeRefusals(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("no lock_proxy_file in this C build (SQLITE_ENABLE_LOCKING_STYLE)")
	}
	dir := t.TempDir()
	p1 := filepath.Join(dir, "proxy1")
	p2 := filepath.Join(dir, "proxy2")
	stmts := []string{
		`PRAGMA lock_proxy_file="` + p1 + `"`,
		`BEGIN`, // deferred: C SQLite takes no lock until a statement runs
		`PRAGMA lock_proxy_file="` + p2 + `"`,
		`ROLLBACK`,
	}
	if oracle := run(t, "cgo", stmts); oracle[2]["kind"] != "rows" {
		t.Fatalf("premise gone: the oracle now refuses a set after a bare BEGIN: %v", oracle[2])
	}
	if mine := run(t, "musql", stmts); mine[2]["kind"] != "error" {
		t.Errorf("expected this engine's conservative refusal after a bare BEGIN, got %v", mine[2])
	}

	// ...and the SECOND deliberate over-refusal: merely entering WAL holds no
	// lock on the database file. PragmaTuningDB.LockHeld carries the WAL bit
	// unconditionally, so a set is refused here where the oracle accepts it.
	//
	// This one was previously undocumented AND untested, and worse, LockHeld's
	// doc comment asserted the opposite -- that the oracle refuses after WAL.
	// It does not. The refusal the original fixture appeared to observe came
	// from a "SELECT * FROM t" between the two pragmas, the first statement
	// that actually opens the WAL and takes SHARED. Pinning it here makes the
	// gap a measured fact, and makes the day it is closed a visible change
	// rather than a silent one.
	wal := []string{
		`CREATE TABLE t(x)`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA lock_proxy_file="` + p1 + `"`, // nothing has opened the WAL yet
	}
	if oracle := run(t, "cgo", wal); oracle[2]["kind"] == "error" {
		t.Fatalf("premise gone: the oracle now refuses a set after entering WAL with nothing read: %v", oracle[2])
	}
	if mine := run(t, "musql", wal); mine[2]["kind"] != "error" {
		t.Errorf("expected this engine's conservative refusal after entering WAL, got %v -- "+
			"if this now ACCEPTS, LockHeld learned to distinguish \"in WAL\" from \"the WAL has been "+
			"opened\", so close the gap in its doc comment too", mine[2])
	}
}

// TestPragmaLockProxyFileAutoPathIsNotDerivable measures the two facts that
// make the ":auto:" getter unclosable, so the obvious "just compute the path"
// fix fails HERE instead of shipping a silent wrong answer.
//
// proxyGetLockPath (os_unix.c:7432) builds the auto-named path out of three
// pieces -- confstr(_CS_DARWIN_USER_TEMP_DIR) (os_unix.c:7442), the
// literal "sqliteplocks/", and the database's own path with every '/' turned
// into '_' -- which reads like something this engine could reproduce from
// os.TempDir() plus the DSN. It cannot, for two independent reasons, and both
// are measured below rather than argued:
//
//  1. The prefix is confstr(_CS_DARWIN_USER_TEMP_DIR), which is NOT $TMPDIR.
//     Go's os.TempDir() IS $TMPDIR, and confstr is a libSystem routine a
//     CGo-free engine cannot call at all. Measured: with TMPDIR pointed
//     somewhere else entirely, the oracle still answers out of the per-uid
//     /var/folders/<...>/T directory. So a path built from os.TempDir() is
//     right only by coincidence -- and wrong on any machine, script or CI job
//     that sets TMPDIR, which is exactly where a wrong answer would go
//     unnoticed.
//
//  2. Once a conch file exists and its host id matches, the getter reports the
//     path stored IN THE CONCH, whatever it is: proxyTakeConch copies it
//     straight out of the conch payload and jumps to end_takeconch without
//     ever calling proxyGetLockPath (os_unix.c:7844-7856). Measured: one
//     connection sets an EXPLICIT path, a later connection asks for ":auto:",
//     and the getter answers the first connection's explicit path -- which is
//     what SQLite's own pragma.test 16.2.1 asserts. The value is therefore not
//     a function of the database path at all; it is a function of a file this
//     engine never writes.
//
// The third state is not reproduced here because it needs a second process:
// when the conch cannot be taken, proxyFileControl reports the literal
// ":auto: (not held)" (os_unix.c:8241), which SQLite's own lock6.test 1.4
// pins. That one is a fact about a LOCK on that same file.
func TestPragmaLockProxyFileAutoPathIsNotDerivable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("no lock_proxy_file in this C build (SQLITE_ENABLE_LOCKING_STYLE)")
	}
	// Taken BEFORE TMPDIR moves, so everything this test writes still lands in
	// the test's own directory.
	dir := t.TempDir()

	// ---- 1. the prefix is confstr's, not $TMPDIR's ----
	alt := filepath.Join(dir, "alt-tmp")
	if err := os.MkdirAll(alt, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alt)
	got := lockProxyAutoPath(t, filepath.Join(dir, "auto.db"))
	if !strings.Contains(got, "/sqliteplocks/") || !strings.HasSuffix(got, ":auto:") {
		t.Fatalf("premise gone: the oracle's :auto: path is no longer proxyGetLockPath's shape: %q", got)
	}
	if strings.HasPrefix(got, alt) || strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("the oracle's :auto: path now comes from $TMPDIR (%q), which os.TempDir() can read: %q -- "+
			"if this is really true on every darwin, the \":auto:\" getter may be computable after all; "+
			"see engine/pragma_lock_proxy_file.go", alt, got)
	}

	// ---- 2. a conch that already carries a path wins over the generated one ----
	db := filepath.Join(dir, "conch.db")
	explicit := filepath.Join(dir, "myproxy")
	first, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	first.SetMaxOpenConns(1)
	if _, err := first.Exec(`PRAGMA lock_proxy_file="` + explicit + `"`); err != nil {
		t.Fatalf("set explicit proxy path: %v", err)
	}
	// Take the conch, which is what writes that path into it (os_unix.c:7905).
	if _, err := first.Exec(`SELECT * FROM sqlite_master`); err != nil {
		t.Fatalf("read on the first connection: %v", err)
	}
	first.Close()

	if second := lockProxyAutoPath(t, db); second != explicit {
		t.Errorf("a second connection asking for \":auto:\" answered %q, want the path the CONCH carries (%q) -- "+
			"pragma.test 16.2.1 asserts this, and it is why the :auto: getter is not a function of the database path",
			second, explicit)
	}
}

// lockProxyAutoPath turns proxy locking on for path with ":auto:" and returns
// what the oracle's getter then reports. It opens the C driver directly rather
// than going through run(): the whole point is the value C SQLite computes
// for THAT database file, and run()'s worker chooses its own DSN.
func lockProxyAutoPath(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA lock_proxy_file=":auto:"`); err != nil {
		t.Fatalf("PRAGMA lock_proxy_file=\":auto:\" on %s: %v", path, err)
	}
	var got string
	if err := db.QueryRow(`PRAGMA lock_proxy_file`).Scan(&got); err != nil {
		t.Fatalf("PRAGMA lock_proxy_file on %s: %v", path, err)
	}
	return got
}

// TestPragmaLockProxyFileMinedLock6Segment replays lock6.test's second mined
// segment -- the corpus statements themselves, in the corpus's own order --
// and pins which of them this engine really cannot answer.
//
// The whole-corpus sweep used to book THREE of this file's statements as
// "unsupported", i.e. as a musql coverage gap. Not one of them is:
//
//	#2  PRAGMA lock_proxy_file        after ":auto:" -- a value no correct
//	                                  engine could match here, because it
//	                                  embeds the database file's own path
//	                                  (proxyGetLockPath, os_unix.c:7432) and
//	                                  the corpus gives the two engines
//	                                  different files. See
//	                                  TestPragmaLockProxyFileAutoPathIsNotDerivable
//	                                  for why computing it is not the missing
//	                                  piece either.
//	#8  PRAGMA lock_proxy_file="mine" under the read transaction #6/#7 opened
//	                                  -- switchLockProxyPath's SQLITE_BUSY
//	                                  (os_unix.c:8082-8084), which C SQLite
//	                                  reports as "failed to set lock proxy
//	                                  file" (pragma.c:1115). BOTH engines
//	                                  refuse it, in the same words. It is
//	                                  differential AGREEMENT.
//
// #8 was counted as a gap because tclExecProbeSafe never asks the oracle about
// a PRAGMA SETTER. tclOracleRefusesLockProxySet now asks -- not by making such
// a probe undoable in general, which a GETTER could not do (proxyTakeConch
// closes and REOPENS the database's file descriptor whenever the conch is not
// currently held, os_unix.c:7950-7956, which mid-transaction drops the very
// locks the replay depends on) but by asking only from a tracked state in
// which an unexpected ACCEPT is a plain path swap it can put back. This test
// keeps the underlying FACT measured; TestPragmaLockProxyFileCorpusBuckets
// keeps the bucket it lands in measured.
func TestPragmaLockProxyFileMinedLock6Segment(t *testing.T) {
	// lock6.test#1 as tclSegments mines it, verbatim.
	stmts := []string{
		`select * from sqlite_master`,
		`PRAGMA lock_proxy_file=":auto:"`,
		`PRAGMA lock_proxy_file`,
		`PRAGMA lock_proxy_file="notmine"`,
		`select * from sqlite_master`,
		`PRAGMA lock_proxy_file`,
		`BEGIN`,
		`SELECT * FROM sqlite_master`,
		`PRAGMA lock_proxy_file="mine"`,
		`select * from sqlite_master`,
	}
	// "notmine"/"mine" are RELATIVE paths, and the oracle's getter really
	// creates a lock file at one of them -- in the process's working
	// directory, which is this package's source tree. See
	// TestPragmaLockProxyFile's own note.
	t.Chdir(t.TempDir())
	mine := run(t, "musql", stmts)
	oracle := run(t, "cgo", stmts)

	// Off darwin there is no such pragma in this C build, so even #2 agrees
	// (both sides answer nothing); the loop at the end then covers the whole
	// segment. On darwin the two statements below are the whole story.
	skip := map[int]bool{}
	if runtime.GOOS != "darwin" {
		assertLockProxySegmentAgrees(t, stmts, mine, oracle, skip)
		return
	}
	skip[2] = true

	// #8: BOTH refuse. This is the statement the sweep miscounts.
	if oracle[8]["kind"] != "error" {
		t.Errorf("premise gone: the oracle now ACCEPTS %q under a held read lock: %v -- "+
			"if C SQLite stopped refusing here, this engine's refusal became a WRONG ANSWER", stmts[8], oracle[8])
	}
	if mine[8]["kind"] != "error" {
		t.Errorf("this engine must refuse %q under a held read lock, exactly as C SQLite does: %v", stmts[8], mine[8])
	}

	// #2: the ONE real gap in this segment.
	if mine[2]["kind"] != "error" {
		t.Errorf("the :auto: getter must still decline rather than guess: %v", mine[2])
	}
	if oracle[2]["kind"] != "rows" {
		t.Fatalf("premise gone: the oracle no longer answers the :auto: getter: %v", oracle[2])
	}

	// ...and every other lock_proxy_file statement agrees exactly, so #2 is
	// the whole of the difference rather than the one that happened to be
	// noticed.
	assertLockProxySegmentAgrees(t, stmts, mine, oracle, skip)
}

// assertLockProxySegmentAgrees compares the two engines statement for
// statement, minus the indexes skip names and minus the sqlite_master reads.
// Those reads are declined here because rootpage is not reproducible against
// C SQLite (engine/query.go) -- nothing to do with this pragma, and
// what the corpus books as outOfScope rather than as a gap.
func assertLockProxySegmentAgrees(t *testing.T, stmts []string, mine, oracle []map[string]any, skip map[int]bool) {
	t.Helper()
	for i := range stmts {
		if skip[i] || strings.Contains(strings.ToLower(stmts[i]), "sqlite_master") {
			continue
		}
		got, _ := json.Marshal(mine[i])
		want, _ := json.Marshal(oracle[i])
		if string(got) != string(want) {
			t.Errorf("stmt #%d %q DIVERGES\n  cgo:    %s\n  musql: %s", i, stmts[i], want, got)
		}
	}
}

// TestPragmaLockProxyFileCorpusBuckets is the ACCOUNTING gate for this pragma:
// it replays the three mined corpus segments that carry a lock_proxy_file
// statement through runTCLSegment -- the whole-corpus sweep's own scorer, not a
// paraphrase of it -- and asserts which bucket each one lands in.
//
// All four of these statements used to be booked "unsupported", i.e. as
// coverage gaps this engine owes work on. None of them is one:
//
//	lock6.test#0  stmt 2  PRAGMA lock_proxy_file   after ":auto:"  -> outOfScope
//	lock6.test#1  stmt 2  PRAGMA lock_proxy_file   after ":auto:"  -> outOfScope
//	pragma.test#28 stmt 1 PRAGMA lock_proxy_file   after ":auto:"  -> outOfScope
//	lock6.test#1  stmt 8  PRAGMA lock_proxy_file="mine"            -> mutualReject
//
// The three getters report a value no correct engine could match here, for the
// reason tclIsOutOfScope's own doc comment now spells out: proxyGetLockPath
// (os_unix.c:7432) mangles THE DATABASE FILE'S OWN PATH into it, and
// runTCLSegment hands the two engines different files. The setter is refused by
// BOTH engines -- switchLockProxyPath's SQLITE_BUSY (os_unix.c:8082-8084)
// surfacing as pragma.c:1115's "failed to set lock proxy file" -- which is
// differential agreement; see tclOracleRefusesLockProxySet for why asking the
// oracle is safe in exactly that state.
//
// Runs on every platform on purpose. Off darwin C has no such pragma
// (SQLITE_ENABLE_LOCKING_STYLE, os_unix.c:66-70) and this engine mirrors its
// unknown-pragma path, so every statement is an ordinary pass there -- and
// "unsupported == 0" still has to hold, which is what would catch a change that
// closed the darwin accounting by opening a linux gap.
func TestPragmaLockProxyFileCorpusBuckets(t *testing.T) {
	// "notmine"/"mine" are RELATIVE proxy paths and the ORACLE really creates a
	// lock file at one of them, in the process's working directory -- which is
	// this package's source tree unless it is moved. See TestPragmaLockProxyFile.
	t.Chdir(t.TempDir())

	segments := []struct {
		label string
		stmts []string
		// darwin-only expectations for the statements this file is about.
		wantMutualReject int
		wantOutOfScope   int
	}{
		{
			// lock6.test#0, as tclSegments mines it, verbatim.
			label: "lock6.test#0",
			stmts: []string{
				`PRAGMA lock_proxy_file=":auto:"`,
				`select * from sqlite_master`,
				`PRAGMA lock_proxy_file`,
				`pragma lock_status`,
			},
			// the getter, plus the sqlite_master read whose rootpage is not
			// reproducible either (engine/query.go's own decline).
			wantOutOfScope: 2,
		},
		{
			// lock6.test#1, as tclSegments mines it, verbatim.
			label: "lock6.test#1",
			stmts: []string{
				`select * from sqlite_master`,
				`PRAGMA lock_proxy_file=":auto:"`,
				`PRAGMA lock_proxy_file`,
				`PRAGMA lock_proxy_file="notmine"`,
				`select * from sqlite_master`,
				`PRAGMA lock_proxy_file`,
				`BEGIN`,
				`SELECT * FROM sqlite_master`,
				`PRAGMA lock_proxy_file="mine"`,
				`select * from sqlite_master`,
			},
			wantMutualReject: 1,
			wantOutOfScope:   5, // the :auto: getter + four sqlite_master reads
		},
		{
			// pragma.test#28, as tclSegments mines it, verbatim.
			label:          "pragma.test#28",
			stmts:          []string{`PRAGMA lock_proxy_file=":auto:"`, `PRAGMA lock_proxy_file`},
			wantOutOfScope: 1,
		},
	}

	for _, seg := range segments {
		t.Run(seg.label, func(t *testing.T) {
			got := runTCLSegment(t, seg.label, seg.stmts, map[string]int{})
			// The whole point: not one of these is a coverage gap.
			if got.unsupported != 0 {
				t.Errorf("%s: unsupported=%d, want 0 -- every lock_proxy_file statement in the corpus is either "+
					"non-comparable by construction or a mutual rejection; see this test's doc comment", seg.label, got.unsupported)
			}
			if got.wrong != 0 || got.panics != 0 {
				t.Errorf("%s: wrong=%d panics=%d, want 0/0", seg.label, got.wrong, got.panics)
			}
			if runtime.GOOS != "darwin" {
				// No such pragma in this C build: nothing below applies, and
				// the "unsupported == 0" check above is the whole assertion.
				return
			}
			if got.mutualReject != seg.wantMutualReject {
				t.Errorf("%s: mutualReject=%d, want %d", seg.label, got.mutualReject, seg.wantMutualReject)
			}
			if got.outOfScope != seg.wantOutOfScope {
				t.Errorf("%s: outOfScope=%d, want %d", seg.label, got.outOfScope, seg.wantOutOfScope)
			}
		})
	}
}

// TestPragmaLockProxyFileAutoGetterIsOutOfScope pins the CONTRACT between the
// engine's decline message and the harness's classifier, which is otherwise a
// phrase match nothing would notice breaking: tclIsOutOfScope recognises the
// ":auto:" getter's decline only because that message carries the words "not
// reproducible against C SQLite". Reword the engine's error without the
// phrase and this fails here, loudly, instead of silently moving three corpus
// statements back into the coverage-gap column.
func TestPragmaLockProxyFileAutoGetterIsOutOfScope(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("no lock_proxy_file in this C build (SQLITE_ENABLE_LOCKING_STYLE)")
	}
	godb, err := engine.Create(filepath.Join(t.TempDir(), "auto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()

	if execErr, panicked, pv := tclSafeExecArgs(godb, `PRAGMA lock_proxy_file=":auto:"`); execErr != nil || panicked {
		t.Fatalf(`PRAGMA lock_proxy_file=":auto:" must be accepted: err=%v panicked=%v (%v)`, execErr, panicked, pv)
	}
	const getter = `PRAGMA lock_proxy_file`
	execErr, panicked, pv := tclSafeExecArgs(godb, getter)
	if panicked {
		t.Fatalf("the :auto: getter panicked: %v", pv)
	}
	if execErr == nil {
		t.Fatalf("the :auto: getter must decline rather than guess a conch path")
	}
	if !tclIsOutOfScope(getter, execErr) {
		t.Errorf("tclIsOutOfScope must classify the :auto: getter's decline as non-comparable, got false for: %v", execErr)
	}
}
