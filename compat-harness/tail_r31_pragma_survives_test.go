// Tests that PRAGMA setters survive across statements through the driver.
// Each settable pragma is set, verified, then tested across a write+read
// to ensure it persists and is not silently ignored.
package compat

import (
	"fmt"
	"strings"
	"testing"
)

// tailR31SettablePragmas lists every settable pragma with non-default values.
var tailR31SettablePragmas = []struct{ name, value string }{
	{"analysis_limit", "100"},
	{"application_id", "17"},
	{"auto_vacuum", "1"},
	{"automatic_index", "0"},
	{"busy_timeout", "1234"},
	{"cache_size", "-500"},
	{"cache_spill", "0"},
	{"cell_size_check", "1"},
	{"checkpoint_fullfsync", "1"},
	{"count_changes", "1"},
	{"defer_foreign_keys", "1"},
	{"empty_result_callbacks", "1"},
	{"foreign_keys", "1"},
	{"full_column_names", "1"},
	{"fullfsync", "1"},
	{"hard_heap_limit", "1000000"},
	{"ignore_check_constraints", "1"},
	{"journal_mode", "memory"},
	{"journal_size_limit", "4096"},
	{"legacy_alter_table", "1"},
	{"locking_mode", "exclusive"},
	{"max_page_count", "100"},
	{"mmap_size", "65536"},
	{"query_only", "1"},
	{"read_uncommitted", "1"},
	{"recursive_triggers", "1"},
	{"reverse_unordered_selects", "1"},
	{"secure_delete", "1"},
	{"short_column_names", "0"},
	{"soft_heap_limit", "1000000"},
	{"synchronous", "0"},
	{"temp_store", "2"},
	{"threads", "4"},
	{"trusted_schema", "0"},
	{"user_version", "42"},
	{"wal_autocheckpoint", "500"},
	{"writable_schema", "1"},
}

// tailR31SetterKnownOpen is the tracked backlog: names this sweep found still
// diverging, with the reason. A name in here is REPORTED, not failed -- but the
// list is self-verifying in the useful direction, so a name that starts
// agreeing fails the test asking to be removed, and a name NOT in it that
// diverges fails outright. That is what keeps it a measurement and a gate at
// once.
var tailR31SetterKnownOpen = map[string]string{
	// Accepted and then really ignored -- the failure mode AGENTS.md names. The
	// bit genuinely changes what a LATER statement returns, so the getter is
	// deliberately NOT reported back either (engine's
	// pragmaInertConnFlagNames says why): with reverse_unordered_selects ON an
	// unordered scan runs backwards.
	//
	// count_changes was the other one and is CLOSED (round 32): its flag is
	// carried on the connection (engine's pragmaCallerConnFlagNames) and the
	// DML's change-count row is produced by driver's Conn.countChangesRow,
	// which is the only layer with a result set to put it on. The one shape
	// still open there -- an upsert whose DO UPDATE branch fires -- is tracked
	// in pragma_r32o_flags_test.go, where the oracle answer is pinned.

	// Accepted no-ops whose value is not tracked, so the getter answers zero
	// rows where the oracle answers one. Each has a real effect this engine does
	// not reproduce, which is why they were not taken with the four inert flag
	// bits: threads and soft_heap_limit are resource limits with their own
	// clamping rules (sqlite3_limit / sqlite3_soft_heap_limit64, where a
	// NEGATIVE value queries rather than sets), and temp_store decides where
	// temporary material lives.
	//
	// busy_timeout was the fourth and is CLOSED (round 32, engine's
	// pragmaBusyTimeout): both forms answer one row of a column named "timeout",
	// and the unset default is this engine's own lock wait -- which is 5000ms,
	// the same value mattn/go-sqlite3 sets on every connection it opens. The
	// stored value still does not shorten that wait; see pragmaBusyTimeout.
	// threads, soft_heap_limit and temp_store were all here and are CLOSED: the
	// first two are served out of tracked state with C's own scopes
	// (pragma_resource_limits.go -- threads per connection with the build's
	// clamp, soft_heap_limit PROCESS-global), and temp_store's setter is routed
	// to the write path by the driver (temp_store_setter_test.go).
	//
	// hard_heap_limit takes their place, and for a reason no amount of tracking
	// could remove: sqlite3_hard_heap_limit64 is PROCESS-global and really
	// ENFORCED, so accepting the setter would answer where C raises
	// SQLITE_NOMEM (pragmaHardHeapLimit). The getter divergence logged here is
	// that decline's consequence -- and it is only visible at all because an
	// EARLIER cgo test in this same binary set the limit for the whole process.
	"hard_heap_limit": "setter declined by design (C enforces it process-wide); see pragmaHardHeapLimit",

	// DECLINED setters whose knock-on the getter then shows. The decline is
	// deliberate and documented; the getter divergence is its consequence, not a
	// separate bug.
	//
	// max_page_count was here and is CLOSED: the ceiling is carried per database
	// FILE on the Conn (max_page_count_driver_test.go).
	//
	// locking_mode was here and is CLOSED too: the lock is held across
	// statements on the connection's one long-lived descriptor and every other
	// lock it would take stands down while it holds that one
	// (engine.Session.HoldLockingMode; locking_mode_exclusive_driver_test.go
	// runs the whole measured behaviour table against both engines).

	// defer_foreign_keys was here and is CLOSED: its whole lifetime is carried on
	// the Conn now, including C's own rule that sqlite3VdbeHalt clears the flag
	// when an autocommit statement's Vdbe was a reader
	// (defer_foreign_keys_pragma_test.go).

	// wal_autocheckpoint was the last entry here and is CLOSED in both shapes: it
	// is carried on the Conn, so it survives the next statement AND an explicit
	// transaction. The transaction half was tracked separately under "txn/<name>"
	// -- the keys are split precisely so a name fixed in one shape and open in the
	// other cannot hide behind the other's entry, which is what this very name did
	// on the first run -- and the split is what made the closure visible: the map
	// fails the test when a name in it starts agreeing, and this one did.
}

func TestTailR31PragmaSetterSurvivesTheNextStatement(t *testing.T) {
	for _, p := range tailR31SettablePragmas {
		t.Run(p.name, func(t *testing.T) {
			tailR31TrackedDiffer(t, p.name, "survives "+p.name, []string{
				`CREATE TABLE t1(a)`,
				fmt.Sprintf(`PRAGMA %s=%s`, p.name, p.value),
				`PRAGMA ` + p.name, // immediate readback: the same session
				`INSERT INTO t1 VALUES(1)`,
				`PRAGMA ` + p.name, // after a WRITE opened and closed a session
				`SELECT count(*) FROM t1`,
				`PRAGMA ` + p.name, // after a READ opened and closed a snapshot
			})
		})
	}
}

// tailR31TrackedDiffer is differAllowingDeclines with the backlog applied: a
// key in tailR31SetterKnownOpen only LOGS its divergence, and fails instead if
// it has none left to log. key is "txn/<name>" for the transaction shape and
// the bare name for the other, so an entry can never cover a shape it was not
// measured on.
// tailR31TxnShapeFixed names the pragmas whose "txn/<name>" shape AGREES with
// the oracle even though the bare statement shape does not, so the inheritance
// in tailR31TrackedDiffer must not excuse it. It is EMPTY now:
// defer_foreign_keys was its only member and is closed in both shapes, so there
// is no longer a name that is fixed across a COMMIT and open across a statement.
var tailR31TxnShapeFixed = map[string]bool{}

func tailR31TrackedDiffer(t *testing.T, key, label string, stmts []string) {
	t.Helper()
	reason, tracked := tailR31SetterKnownOpen[key]
	if !tracked {
		// A "txn/<name>" shape also inherits the bare name's entry, since a
		// setter that does not survive a statement cannot survive a COMMIT
		// either. The reverse is deliberately NOT true: an entry under
		// "txn/<name>" alone covers ONLY the transaction shape, so a name that
		// is fine across statements and broken across a COMMIT cannot be
		// excused in both. (wal_autocheckpoint is exactly that name.)
		if bare, ok := strings.CutPrefix(key, "txn/"); ok && !tailR31TxnShapeFixed[bare] {
			reason, tracked = tailR31SetterKnownOpen[bare]
		}
	}
	if tracked {
		sub := &testing.T{}
		differAllowingDeclines(sub, label, stmts)
		if !sub.Failed() {
			t.Errorf("%s no longer diverges (%s) -- remove it from tailR31SetterKnownOpen", key, reason)
			return
		}
		t.Logf("KNOWN OPEN %s: %s", key, reason)
		return
	}
	differAllowingDeclines(t, label, stmts)
}

// TestTailR31InertFlagPragmas pins the four PragTyp_FLAG bits this engine now
// tracks and reports back (engine's pragmaInertConnFlagNames) beyond the
// survives-a-statement question above: the fresh-connection default, the
// setter's empty result set, both directions, and that a SCHEMA QUALIFIER is
// ignored -- db->flags has one copy per connection and none per database, and
// pragma.c's PragTyp_FLAG arm never looks at iDb.
func TestTailR31InertFlagPragmas(t *testing.T) {
	for _, n := range []string{"fullfsync", "checkpoint_fullfsync", "empty_result_callbacks", "read_uncommitted"} {
		t.Run(n, func(t *testing.T) {
			differAllowingDeclines(t, "inert flag "+n, []string{
				`CREATE TABLE t1(a)`,
				`PRAGMA ` + n, // a fresh connection: 0
				`PRAGMA ` + n + `=1`,
				`PRAGMA ` + n,
				`PRAGMA ` + n + `=0`,
				`PRAGMA ` + n,
				`PRAGMA ` + n + `=on`,
				`PRAGMA ` + n,
				`PRAGMA ` + n + `=off`,
				`PRAGMA ` + n,
				`PRAGMA ` + n + `=true`,
				`PRAGMA ` + n,
				`PRAGMA main.` + n + `=0`, // the qualifier is ignored...
				`PRAGMA ` + n,
				`PRAGMA temp.` + n + `=1`, // ...even when it names the OTHER database
				`PRAGMA ` + n,
				`PRAGMA main.` + n,
				// The flag really is inert: the same statements answer the same
				// rows with it on and off.
				`INSERT INTO t1 VALUES(1),(2)`,
				`SELECT a FROM t1 ORDER BY a`,
				`PRAGMA ` + n + `=0`,
				`SELECT a FROM t1 ORDER BY a`,
				`PRAGMA integrity_check`,
			})
		})
	}
}

// TestTailR31PragmaSetterSurvivesATransaction asks the same question across an
// explicit transaction, which is the other place a per-statement session model
// can lose a flag: the setter runs on the HELD session and the flag must still
// be there once that session is thrown away at COMMIT.
func TestTailR31PragmaSetterSurvivesATransaction(t *testing.T) {
	for _, p := range tailR31SettablePragmas {
		t.Run(p.name, func(t *testing.T) {
			tailR31TrackedDiffer(t, "txn/"+p.name, "survives a transaction "+p.name, []string{
				`CREATE TABLE t1(a)`,
				`BEGIN`,
				fmt.Sprintf(`PRAGMA %s=%s`, p.name, p.value),
				`PRAGMA ` + p.name,
				`COMMIT`,
				`PRAGMA ` + p.name,
			})
		})
	}
}
