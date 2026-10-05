// Tests sqlite_compileoption_used() behavior across oracle builds.
// Any answer that turns on membership is right in one build and wrong in the
// other.
//
// What makes the function servable anyway is that most CALLS do not turn on
// membership. sqlite3_compileoption_used is a prefix match against the option
// array, so a name that is not a compile option in ANY build answers 0
// everywhere, and a name whose option is pinned by the MODULE rather than by
// its host answers the same in both builds. engine/compile_options.go serves
// exactly those two and declines the rest -- see it for the 242-name catalogue
// that makes the first claim checkable.
//
// # What would make this WRONG rather than incomplete
//
// Answering for an option whose presence is the build's or the host's:
// ENABLE_FTS5 (0 here, 1 under the fts5 tag), COMPILER=gcc-12.4.0 (this
// machine's), MUTEX_PTHREADS (this OS's). Every one of those must keep
// declining, and TestBuildDependentCompileOptionsStillDecline asserts it --
// which is also why this file's differ cases contain none of them.
package compat

import "testing"

func TestCompileOptionUsed(t *testing.T) {
	// ctime.test's own calls. The THREADSAFE family exercises every branch of
	// the prefix match: bare name, exact value, wrong value, and the trailing
	// "=" that matches the name but not the terminator.
	differ(t, "sqlite_compileoption_used over a module-pinned option", []string{
		`SELECT sqlite_compileoption_used('SQLITE_THREADSAFE') AS a`,
		`SELECT sqlite_compileoption_used('THREADSAFE') AS a`,
		`SELECT sqlite_compileoption_used("THREADSAFE") AS a`,
		`SELECT sqlite_compileoption_used('threadsafe') AS a`,
		`SELECT sqlite_compileoption_used('THREADSAFE=0') AS a`,
		`SELECT sqlite_compileoption_used('THREADSAFE=1') AS a`,
		`SELECT sqlite_compileoption_used('THREADSAFE=2') AS a`,
		`SELECT sqlite_compileoption_used('THREADSAFE=') AS a`,
		// The other four mattn/go-sqlite3 pins, both spellings.
		`SELECT sqlite_compileoption_used('ENABLE_RTREE') AS a`,
		`SELECT sqlite_compileoption_used('SQLITE_ENABLE_FTS3') AS a`,
		`SELECT sqlite_compileoption_used('ENABLE_FTS3_PARENTHESIS') AS a`,
		`SELECT sqlite_compileoption_used('ENABLE_UPDATE_DELETE_LIMIT') AS a`,
		`SELECT sqlite_compileoption_used('OMIT_DEPRECATED') AS a`,
		`SELECT sqlite_compileoption_used('DEFAULT_WAL_SYNCHRONOUS=1') AS a`,
		`SELECT sqlite_compileoption_used('DEFAULT_WAL_SYNCHRONOUS=0') AS a`,
	})
	// Arguments that are not compile options at all: 0 on every build ever
	// compiled, which is what the catalogue proves. OMIT_COMPILEOPTION_DIAGS
	// is the sharpest -- define it and this function does not exist, so any
	// build able to answer answers 0.
	differ(t, "sqlite_compileoption_used over a non-option", []string{
		`SELECT sqlite_compileoption_used('SQLITE_OMIT_COMPILEOPTION_DIAGS') AS a`,
		`SELECT sqlite_compileoption_used('OMIT_COMPILEOPTION_DIAGS') AS a`,
		`SELECT sqlite_compileoption_used('') AS a`,
		`SELECT sqlite_compileoption_used("") AS a`,
		`SELECT sqlite_compileoption_used('0') AS a`,
		`SELECT sqlite_compileoption_used(0) AS a`,
		`SELECT sqlite_compileoption_used(1.0) AS a`,
		`SELECT sqlite_compileoption_used('SQLITE_') AS a`,
		`SELECT sqlite_compileoption_used('NOT_AN_OPTION_AT_ALL') AS a`,
		// A STRICT PREFIX of a real name: ctime.c's terminator test reads the
		// next character, a digit, so this does NOT match ENABLE_FTS3.
		`SELECT sqlite_compileoption_used('ENABLE_FTS') AS a`,
		`SELECT sqlite_compileoption_used('THREADSAF') AS a`,
	})
	// NULL in, NULL out -- not 0.
	differ(t, "sqlite_compileoption_used(NULL)", []string{
		`SELECT sqlite_compileoption_used(NULL) AS a`,
		`SELECT typeof(sqlite_compileoption_used(NULL)) AS a`,
		`SELECT sqlite_compileoption_used(NULL) IS NULL AS a`,
	})
	// Arity is a prepare-time rejection on both sides.
	differ(t, "sqlite_compileoption_used arity", []string{
		`SELECT sqlite_compileoption_used() AS a`,
		`SELECT sqlite_compileoption_used('A','B') AS a`,
		`SELECT 1 AS alive`,
	})
}

// TestBuildDependentCompileOptionsStillDecline is the wrong-answer boundary.
// Each of these has a real answer on the oracle that musql cannot know: the
// first differs between the two oracle BUILDS these gates run, the rest
// between HOSTS. Asserted one-sided, since the oracle answers all of them.
func TestBuildDependentCompileOptionsStillDecline(t *testing.T) {
	for _, q := range []string{
		// 0 in the plain build, 1 under "-tags sqlite_fts5".
		`SELECT sqlite_compileoption_used('ENABLE_FTS5')`,
		`SELECT sqlite_compileoption_used('SQLITE_ENABLE_FTS5')`,
		// The machine that compiled the oracle decides these.
		`SELECT sqlite_compileoption_used('COMPILER')`,
		`SELECT sqlite_compileoption_used('COMPILER=gcc-12.4.0')`,
		`SELECT sqlite_compileoption_used('ATOMIC_INTRINSICS=1')`,
		`SELECT sqlite_compileoption_used('MUTEX_PTHREADS')`,
		`SELECT sqlite_compileoption_used('SYSTEM_MALLOC')`,
		`SELECT sqlite_compileoption_used('MAX_ATTACHED=10')`,
		// ...and every index into the list is build-dependent, so an
		// in-range get() is declined whole. The COMPOSED call
		// used(get(0)) is the one exception and is gated the other way, in
		// compile_option_used_of_get_test.go: it is 1 on every build ever
		// compiled (mkctimec.tcl:388-396 makes sqlite3azCompileOpt[]
		// non-empty unconditionally), so the engine answers it.
		`SELECT sqlite_compileoption_get(0)`,
		`SELECT sqlite_compileoption_get(1)`,
	} {
		res := run(t, "musql", []string{q})
		if res[0]["kind"] != "error" {
			t.Errorf("expected a clean decline for %q, got %v", q, res[0])
		}
	}
}
