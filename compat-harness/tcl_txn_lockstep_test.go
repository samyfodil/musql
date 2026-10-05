// Tests transaction lockstep between the Go engine and oracle: ensures both
// agree on whether an explicit transaction is open after each statement,
// particularly with ROLLBACK conflict policies.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// txnLockstepGoInTransaction checks if the Go engine has an open transaction.
func txnLockstepGoInTransaction(t *testing.T, godb *engine.Session) bool {
	t.Helper()
	if _, _, err := godb.ExecArgs("BEGIN", nil); err != nil {
		return true
	}
	if _, _, err := godb.ExecArgs("ROLLBACK", nil); err != nil {
		t.Fatalf("ROLLBACK of a just-opened empty transaction failed: %v", err)
	}
	return false
}

func TestTCLProbeReportsLostTransaction(t *testing.T) {
	setup := []string{
		"CREATE TABLE tbl (a primary key, b, c)",
		"CREATE TRIGGER ai_tbl AFTER INSERT ON tbl BEGIN INSERT OR IGNORE INTO tbl values (new.a, 0, 0); END",
		"BEGIN",
		"INSERT INTO tbl values (1, 2, 3)",
	}
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	for _, s := range setup {
		if _, cerr := cgodb.Exec(s); cerr != nil {
			t.Fatalf("cgo setup %q: %v", s, cerr)
		}
		// The engine declines the trigger-carrying INSERT; the corpus mirrors
		// that as a no-op, so only what it accepts is replayed here too.
		godb.ExecArgs(s, nil)
	}
	if !txnLockstepGoInTransaction(t, godb) {
		t.Fatal("setup: the Go engine should be inside the BEGIN")
	}

	// rejects is TRUE here, and that is the reporting rule rather than a claim
	// that the oracle refused the statement at prepare: it prepares and starts,
	// then the trigger's conflicting row aborts it. Losing the probe savepoint
	// says where the oracle's STATE ended up, not whether it accepted; the
	// statement error still answers the question, so it is reported.
	//
	// This moves statements from "unsupported" (a musql gap) to
	// "mutualReject", i.e. it IMPROVES the headline number, which is exactly
	// the kind of change to distrust. It is justified because the specific
	// statements are named and checkable: trigger3.test's three "Trigger
	// rollback"/"View rollback" cases are rejected by the oracle in the SAME
	// words this engine uses. If that ever stops being true, a real gap will be
	// hidden here -- so the containment assertions below are what keep this
	// honest, not the flag itself.
	rejects, txnLost := tclCGOExecAlsoRejects(cgodb, "INSERT OR ROLLBACK INTO tbl values (3, 2, 3)")
	if !rejects {
		t.Errorf("rejects=false: the statement error is what answers the question, and it must be reported")
	}
	if !txnLost {
		t.Fatal("txnLost=false, but the ROLLBACK conflict policy aborts the whole transaction -- the probe cannot contain it")
	}
	if tclCGOInTransaction(cgodb) {
		t.Error("the oracle should be back in autocommit after a conflict rollback")
	}
	// The repair runTCLSegment performs must land the Go engine there too.
	tclSafeExecArgs(godb, "ROLLBACK")
	if txnLockstepGoInTransaction(t, godb) {
		t.Error("after the repair the Go engine should be in autocommit, matching the oracle")
	}
}

// TestTCLTxnStmtProbeMatchesOracle re-derives tclCGOTxnStmtAlsoRejects' answer
// from the oracle rather than trusting it: for each of the two transaction
// states, it asks the helper what C SQLite would do with a BEGIN and with a
// COMMIT, then actually runs them on a throwaway connection in the same state
// and compares. A drift in either direction (SQLite changing when it rejects,
// or the helper's rule being wrong) fails here rather than silently
// misclassifying a whole corpus bucket.
func TestTCLTxnStmtProbeMatchesOracle(t *testing.T) {
	for _, inTxn := range []bool{false, true} {
		for _, tc := range []struct {
			stmt string
			kind engine.TxnKind
		}{
			{"BEGIN", engine.TxnBegin},
			{"COMMIT", engine.TxnCommit},
			{"ROLLBACK", engine.TxnRollback},
		} {
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "o.db"))
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			db.SetMaxOpenConns(1)
			if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if inTxn {
				if _, err := db.Exec("BEGIN"); err != nil {
					t.Fatalf("setup BEGIN: %v", err)
				}
			}
			predicted := tclCGOTxnStmtAlsoRejects(db, tc.kind)
			_, actualErr := db.Exec(tc.stmt)
			if got := actualErr != nil; got != predicted {
				t.Errorf("inTxn=%v %s: tclCGOTxnStmtAlsoRejects said rejects=%v, oracle actually gave err=%v",
					inTxn, tc.stmt, predicted, actualErr)
			}
			db.Close()
		}
	}
}

// TestTCLCorpusTxnLockstep is the corpus-wide form: replay every mined segment
// that uses explicit transactions through TestTCLCorpus' own protocol and, at
// every transaction-control statement, assert the two engines agree about
// whether a transaction is open. This is the sweep that found the probe
// containment failure above, kept as a gate so a future harness or engine
// change cannot reintroduce a silent drift.
func TestTCLCorpusTxnLockstep(t *testing.T) {
	files := tclAllFiles(t)
	if len(files) == 0 {
		t.Skip("no .test files found under " + tclCorpusDir)
	}
	if testing.Short() && len(files) > tclShortFileCount {
		files = files[:tclShortFileCount]
	}
	checked := 0
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(tclCorpusDir, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		// Cheap pre-filter: mining and replaying a file that cannot contain a
		// transaction statement at all is pure cost. Over-inclusive on purpose
		// (a trigger body's own BEGIN matches too) -- txnLockstepSegmentUsesTxns
		// makes the real decision per segment.
		if up := strings.ToUpper(string(src)); !strings.Contains(up, "BEGIN") &&
			!strings.Contains(up, "COMMIT") && !strings.Contains(up, "SAVEPOINT") &&
			!strings.Contains(up, "ROLLBACK") {
			continue
		}
		for si, stmts := range tclSegments(string(src)) {
			if !txnLockstepSegmentUsesTxns(stmts) {
				continue
			}
			checked += txnLockstepReplay(t, fmt.Sprintf("%s#%d", rel, si), stmts)
		}
	}
	t.Logf("checked %d transaction-control statements for engine/oracle lockstep", checked)
}

func txnLockstepSegmentUsesTxns(stmts []string) bool {
	for _, s := range stmts {
		if _, ok := engine.TxnStmtKind(s); ok {
			return true
		}
		if _, _, ok := engine.SavepointStmt(s); ok {
			return true
		}
	}
	return false
}

// txnLockstepReplay runs one segment through the same lockstep protocol
// runTCLSegment uses -- deliberately a trimmed copy rather than a shared helper,
// because this test judges only transaction STATE and must not be perturbed by
// the result-comparison machinery -- and returns how many transaction-control
// statements it checked.
func txnLockstepReplay(t *testing.T, label string, stmts []string) int {
	t.Helper()
	if len(stmts) > tclMaxStmtsPerFile {
		stmts = stmts[:tclMaxStmtsPerFile]
	}
	segDir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(segDir); err != nil {
		t.Fatalf("Chdir(%s): %v", segDir, err)
	}
	defer os.Chdir(prevWD)
	// The oracle's relative ATTACHes land in a directory of their own, exactly as
	// runTCLSegment moves them (tclIsolateAttach). This replay never did, so both
	// engines wrote ONE "test2.db" -- harmless while both wrote the SQLite format,
	// and a desync once the engine's own format became the storage: the oracle's
	// BEGIN IMMEDIATE read the engine's file and failed "file is not a database"
	// (jrnlmode.test#2 and #4, the only two desyncs this gate reported).
	cgoAttachDir := t.TempDir()

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
	cgodb.SetMaxOpenConns(1)
	cgodb.Exec("PRAGMA synchronous=OFF")

	checked := 0
	for i, stmt := range stmts {
		if tclIsQuery(stmt) {
			if tclCallsNondeterministicFunc(stmt) {
				continue
			}
			_, _, qerr, panicked, _ := tclSafeGoQuery(godb, stmt)
			if panicked || qerr != nil {
				continue
			}
			tclRunCGOQuery(cgodb, stmt)
			continue
		}
		execErr, panicked, _ := tclSafeExecArgs(godb, stmt)
		if panicked {
			continue
		}
		// cgoErr is kept only to be PRINTED by the desync report below. It was
		// discarded before, and that made the one desync this gate has ever
		// reported unreadable: "oracle inTransaction=false" after a
		// BEGIN IMMEDIATE the engine accepted says the oracle is not in a
		// transaction, and gives no hint whether its own BEGIN IMMEDIATE
		// failed (and with what) or succeeded and was then lost. It is NOT
		// asserted on -- an oracle error here is routine, and the two engines'
		// error TEXT is compared by other gates.
		var cgoErr error
		cgoStmt := tclIsolateAttach(stmt, cgoAttachDir)
		switch tclClassifyExecErr(execErr) {
		case "", "constraint":
			_, cgoErr = cgodb.Exec(cgoStmt)
		case "unsupported":
			if tclExecProbeSafe(stmt) {
				if _, txnLost := tclCGOExecAlsoRejects(cgodb, cgoStmt); txnLost {
					tclSafeExecArgs(godb, "ROLLBACK")
				}
			}
		}
		_, isTxn := engine.TxnStmtKind(stmt)
		if _, _, isSavepoint := engine.SavepointStmt(stmt); !isTxn && !isSavepoint {
			continue
		}
		checked++
		goIn := txnLockstepGoInTransaction(t, godb)
		cgoIn := tclCGOInTransaction(cgodb)
		if goIn != cgoIn {
			t.Errorf("TRANSACTION STATE DESYNC\n  segment: %s\n  after stmt #%d: %s\n  go engine inTransaction=%v, oracle inTransaction=%v\n  go engine err: %v\n  oracle err: %v\n  every later statement in this segment compares two databases that disagree about what is committed",
				label, i, strings.TrimSpace(stmt), goIn, cgoIn, execErr, cgoErr)
			return checked
		}
	}
	return checked
}
