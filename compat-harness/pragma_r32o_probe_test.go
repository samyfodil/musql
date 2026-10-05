// Tests PRAGMA rejection safety by asserting pragmas cannot reach assignment
// inside a rolled-back savepoint.
package compat

import (
	"encoding/json"
	"os"
	"testing"
)

// r32oProbeWrap is tclCGOExecAlsoRejects' savepoint scaffolding, as statements.
func r32oProbeWrap(stmt string) []string {
	return []string{
		`SAVEPOINT __musql_probe`, stmt,
		`ROLLBACK TO __musql_probe`, `RELEASE __musql_probe`,
	}
}

// r32oUnchanged runs stmts on the ORACLE and fails unless the last result
// equals the one at index want -- the "read the setting back" shape every case
// below uses.
func r32oUnchanged(t *testing.T, name string, want int, stmts []string) {
	t.Helper()
	res := run(t, "cgo", stmts)
	before, after := r32oRender(res[want]), r32oRender(res[len(res)-1])
	if before != after {
		t.Errorf("%s changed the oracle across a rolled-back probe: %s -> %s", name, before, after)
	}
}

// TestR32OPragmaProbeLeavesOracleAlone runs each admitted spelling the way the
// probe does (SAVEPOINT, exec, ROLLBACK TO, RELEASE) and checks the oracle's own
// answer to the corresponding getter is unchanged. It goes through the cgo
// worker, so what it exercises is C SQLite's behaviour, not a model of it.
func TestR32OPragmaProbeLeavesOracleAlone(t *testing.T) {
	// encoding: a name outside encnames[] must leave the encoding alone.
	// pragma.c reaches sqlite3ErrorMsg only on a miss, and skips the branch
	// entirely once DBFLAG_EncodingFixed is set -- both are no-ops here.
	r32oUnchanged(t, "PRAGMA encoding='bogus'", 1, append(append(
		[]string{`CREATE TABLE t1(a)`, `PRAGMA encoding`},
		r32oProbeWrap(`PRAGMA encoding='bogus'`)...), `PRAGMA encoding`))

	// temp_store_directory: a path that is not a directory fails sqlite3OsAccess
	// before sqlite3_temp_directory is freed and replaced.
	r32oUnchanged(t, "PRAGMA temp_store_directory=<non-directory>", 1, append(append(
		[]string{`CREATE TABLE t1(a)`, `PRAGMA temp_store_directory`},
		r32oProbeWrap(`PRAGMA temp_store_directory='/NON/EXISTENT/PATH/FOOBAR'`)...),
		`PRAGMA temp_store_directory`))

	// compile_options with a trailing token: a parse error runs nothing at all,
	// so read back a pragma the statement could not have touched anyway.
	r32oUnchanged(t, "PRAGMA compile_options *", 1, append(append(
		[]string{`CREATE TABLE t1(a)`, `PRAGMA count_changes`},
		r32oProbeWrap(`PRAGMA compile_options *`)...), `PRAGMA count_changes`))

	// synchronous: inside a transaction the assignment is an "else if" after the
	// error, so nothing is assigned. The probe has to run inside a REAL
	// transaction for the condition to hold, which is why this one is not
	// wrapped by r32oProbeWrap alone.
	r32oUnchanged(t, "PRAGMA synchronous=0 inside a transaction", 1, []string{
		`CREATE TABLE t1(a)`, `PRAGMA synchronous`, `BEGIN`, `INSERT INTO t1 VALUES(1)`,
		`SAVEPOINT __musql_probe`, `PRAGMA synchronous=0`,
		`ROLLBACK TO __musql_probe`, `RELEASE __musql_probe`,
		`COMMIT`, `PRAGMA synchronous`,
	})
}

// TestR32OPragmaProbeCounterExample pins the fact that makes
// tclR32OPragmaRejectedAtPrepare spelling-conditional instead of a blanket
// probe: a PragTyp_FLAG pragma writes db->flags inside sqlite3Pragma, so it
// takes effect during code generation and SURVIVES the savepoint rollback the
// probe wraps everything in. While this is true, widening the gate would
// corrupt the oracle mid-segment; if it ever stops being true, this says so.
//
// The demonstration is deliberately count_changes and NOT foreign_keys, and
// that is a correction rather than a preference. foreign_keys is the obvious
// candidate and is the ONE bit this cannot be shown with, because pragma.c's
// same arm masks it out while db->autoCommit==0:
//
//	u64 mask = pPragma->iArg;
//	if( db->autoCommit==0 ){ mask &= ~(SQLITE_ForeignKeys); }
//
// and SAVEPOINT clears autoCommit. So "SAVEPOINT; PRAGMA foreign_keys=ON;
// ROLLBACK TO; RELEASE" really does leave the oracle at 0 -- measured, both
// directions -- while the same wrapper around "PRAGMA count_changes=1" leaves it
// at 1. The hazard is real; only the example had to change.
func TestR32OPragmaProbeCounterExample(t *testing.T) {
	res := run(t, "cgo", append(append(
		[]string{`CREATE TABLE t1(a)`, `PRAGMA count_changes`},
		r32oProbeWrap(`PRAGMA count_changes=1`)...), `PRAGMA count_changes`))
	before, after := r32oRender(res[1]), r32oRender(res[len(res)-1])
	if before == after {
		t.Errorf("PRAGMA count_changes=1 no longer survives a savepoint rollback (%s -> %s)"+
			" -- tclR32OPragmaRejectedAtPrepare's gate could be widened", before, after)
	}
	// ...and the foreign_keys exception itself, so the correction above cannot
	// silently rot: it must stay at 0 across the very same wrapper.
	r32oUnchanged(t, "PRAGMA foreign_keys=ON (masked while autoCommit==0)", 1, append(append(
		[]string{`CREATE TABLE t1(a)`, `PRAGMA foreign_keys`},
		r32oProbeWrap(`PRAGMA foreign_keys=ON`)...), `PRAGMA foreign_keys`))
}

// TestR32OPragmaRejectedAtPrepareSpellings pins which spellings the predicate
// admits. The ones it must NOT admit are the assigning halves of the very same
// names, which is where a widened gate would do its damage.
func TestR32OPragmaRejectedAtPrepareSpellings(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		stmt string
		want bool
	}{
		{`PRAGMA compile_options *`, true},
		{`PRAGMA compile_options`, false},     // the ordinary getter
		{`PRAGMA compile_options = 1`, false}, // grammatical, and ignored
		{`PRAGMA encoding='bogus'`, true},     // not in encnames[]
		{`pragma encoding=bogus`, true},       // the corpus's own spelling
		{`PRAGMA encoding = 'utf16'`, false},  // encnames[], case-folded
		{`PRAGMA encoding = "UTF-16le"`, false},
		{`PRAGMA main.encoding = UTF8`, false}, // the qualifier is stripped
		{`PRAGMA encoding`, false},             // the getter assigns nothing
		{`PRAGMA temp_store_directory='/NON/EXISTENT/PATH/FOOBAR'`, true},
		{`PRAGMA temp_store_directory=''`, false},            // skips sqlite3OsAccess, ASSIGNS
		{`PRAGMA temp_store_directory='` + cwd + `'`, false}, // a real directory
		{`PRAGMA temp_store_directory`, false},
		{`PRAGMA synchronous = OFF`, false}, // transaction-conditional, not textual
		{`PRAGMA foreign_keys=ON`, false},   // THE counter-example
		{`PRAGMA journal_mode = memory`, false},
		{`SELECT 1`, false},
	} {
		if got := tclR32OPragmaRejectedAtPrepare(c.stmt); got != c.want {
			t.Errorf("tclR32OPragmaRejectedAtPrepare(%q) = %v, want %v", c.stmt, got, c.want)
		}
	}
}

// r32oRender is one worker result as its comparable JSON. The worker already
// normalizes every cell, so this is the same comparison differ makes.
func r32oRender(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}
