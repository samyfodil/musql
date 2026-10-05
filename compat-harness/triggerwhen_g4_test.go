// This file gates trigger WHEN guards, verifying they read connection state
// consistently (changes(), total_changes(), last_insert_rowid()).
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// triggerWhenG4Setup leaves the connection in a state where the three counters
// are all DIFFERENT, so a guard reading the wrong one is visible:
//
//	changes()           = 3   (the three-row INSERT into s)
//	last_insert_rowid() = 33  (that INSERT's last explicit rowid)
//	total_changes()     = 4   (one row into t, then three into s)
func triggerWhenG4Setup() []string {
	return []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE s(a)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t(rowid,a) VALUES(1,1)`,
		`INSERT INTO s(rowid,a) VALUES(11,1),(22,2),(33,3)`,
	}
}

// triggerWhenG4Run creates the trigger, runs dml, and reports how many rows the
// trigger body logged -- or the statement's error, which is itself the answer
// being compared (the oracle never errors on any of these).
func triggerWhenG4Run(t *testing.T, driver, dsn, trigger, dml string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s: open: %v", driver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range append(triggerWhenG4Setup(), trigger) {
		if _, serr := db.Exec(s); serr != nil {
			t.Fatalf("%s: setup %q: %v", driver, s, serr)
		}
	}
	if _, derr := db.Exec(dml); derr != nil {
		return "error: " + derr.Error()
	}
	var n int
	if serr := db.QueryRow(`SELECT count(*) FROM log`).Scan(&n); serr != nil {
		t.Fatalf("%s: readback: %v", driver, serr)
	}
	if n > 0 {
		return "fired"
	}
	return "not fired"
}

func TestTriggerWhenG4ConnState(t *testing.T) {
	// route is the fragment that used to decide which of the two evaluators
	// ran. "" keeps the statement fully lowered, so the guard is a sub-program
	// -- that half is the CONTROL, and it passed before this fix as it does
	// after. " OF a" is an UPDATE OF column list, which the write compiler did
	// not model when this was written, so the whole statement declined and its
	// guard was evaluated with no pager -- that half is the GATE, and all six
	// of its cases fail if the guard stops carrying the write session (verified
	// by reverting the assignment and re-running). One fragment is the only
	// difference between the two halves, which is exactly why they have to
	// agree.
	//
	// "UPDATE OF" COMPILES today -- the OF list is applied as a filter over the
	// matched trigger list (compileUpdateTriggerFirePlan, engine/vdbe_trigger.go)
	// -- so both halves now take the compiled route, and the pair gates the OF
	// list's own answer rather than two evaluators against each other.
	for _, route := range []struct{ name, of string }{
		{"compiled", ""},
		{"declined-by-UPDATE-OF", " OF a"},
	} {
		for _, tc := range []struct {
			name     string
			when     string
			wantFire bool
		}{
			{"changes-hit", `changes() = 3`, true},
			{"changes-miss", `changes() = 0`, false},
			{"last_insert_rowid-hit", `last_insert_rowid() = 33`, true},
			{"last_insert_rowid-miss", `last_insert_rowid() = 0`, false},
			{"total_changes-hit", `total_changes() = 4`, true},
			{"total_changes-miss", `total_changes() = 0`, false},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				trigger := `CREATE TRIGGER tr AFTER UPDATE` + route.of +
					` ON t WHEN ` + tc.when + ` BEGIN INSERT INTO log VALUES(1); END`
				const dml = `UPDATE t SET a=2`
				want := triggerWhenG4Run(t, "sqlite3", filepath.Join(t.TempDir(), "oracle.sqlite"), trigger, dml)
				got := triggerWhenG4Run(t, "sqlite", filepath.Join(t.TempDir(), "musql.sqlite"), trigger, dml)

				// Non-vacuity first: if the oracle no longer does what the case
				// was built to observe, the differential below proves nothing.
				wantOracle := "not fired"
				if tc.wantFire {
					wantOracle = "fired"
				}
				if want != wantOracle {
					t.Fatalf("the ORACLE answered %q for WHEN %s, not the expected %q --\n"+
						"  this case no longer discriminates, so fix the constant rather than\n"+
						"  leaving a gate that passes whatever the engine does",
						want, tc.when, wantOracle)
				}
				if got != want {
					t.Errorf("WHEN %s on %s AFTER UPDATE%s\n  oracle: %s\n  engine: %s\n"+
						"  a trigger's WHEN guard must read the live connection state on BOTH\n"+
						"  of this engine's trigger routes -- see engine/trigger.go's guard ctx",
						tc.when, route.name, route.of, want, got)
				}
			})
		}
	}
}

// The other shape that declines: a SUBQUERY in the WHEN clause, which
// compileTriggerFirePlan refuses outright (engine/vdbe_trigger.go:111).
//
// READ THIS BEFORE TRUSTING IT AS A GATE: it is NOT one for the guard's db
// assignment, and passes with that assignment reverted. Verified by
// instrumenting the guard -- these cases do reach it, but with a
// snapshot pager already installed (the one the subquery needs), and a pager
// carries a connection-state copy of its own (ReadOnlyPager.connState,
// engine/conn_state.go:282), so they were answered before the fix and are
// answered after it.
//
// What they DO pin is the invariant that makes preferring the live session
// safe on this route: SnapshotPager stamps the counters at the moment it is
// built (engine/writer.go:3137-3138) and the guard builds its pager
// immediately before evaluating, so the frozen copy and the live session must
// agree here. Break that -- stamp a stale count, or take the snapshot earlier
// -- and these cases separate. The gate for the fix itself was the
// declined-by-UPDATE-OF half of TestTriggerWhenG4ConnState above, whose guard
// ran with NO pager at all.
func TestTriggerWhenG4ConnStateUnderSubquery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		when     string
		wantFire bool
	}{
		{"changes-hit", `changes() = 3 AND (SELECT count(*) FROM s) = 3`, true},
		{"changes-miss", `changes() = 0 AND (SELECT count(*) FROM s) = 3`, false},
		{"total_changes-hit", `total_changes() = 4 AND (SELECT count(*) FROM s) = 3`, true},
		{"total_changes-miss", `total_changes() = 0 AND (SELECT count(*) FROM s) = 3`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trigger := `CREATE TRIGGER tr AFTER UPDATE ON t WHEN ` + tc.when +
				` BEGIN INSERT INTO log VALUES(1); END`
			const dml = `UPDATE t SET a=2`
			want := triggerWhenG4Run(t, "sqlite3", filepath.Join(t.TempDir(), "oracle.sqlite"), trigger, dml)
			got := triggerWhenG4Run(t, "sqlite", filepath.Join(t.TempDir(), "musql.sqlite"), trigger, dml)

			wantOracle := "not fired"
			if tc.wantFire {
				wantOracle = "fired"
			}
			if want != wantOracle {
				t.Fatalf("the ORACLE answered %q for WHEN %s, not the expected %q -- "+
					"this case no longer discriminates", want, tc.when, wantOracle)
			}
			if got != want {
				t.Errorf("WHEN %s\n  oracle: %s\n  engine: %s\n"+
					"  a subquery in the guard installs a snapshot pager; the LIVE session\n"+
					"  still owns changes()/total_changes()/last_insert_rowid()",
					tc.when, want, got)
			}
		})
	}
}
