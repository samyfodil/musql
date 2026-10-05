// Connection counters (last_insert_rowid, total_changes) across failed
// CREATE VIRTUAL TABLE statements. Failed creates must not modify counters;
// successful creates still hide the counter.
package compat

import "testing"

func TestFailedVirtualTableCreateKeepsConnCounters(t *testing.T) {
	// The vtab1.test 7 shape: an unknown module, then the counters.
	differ(t, "a create naming an unknown module leaves the counters alone", []string{
		`CREATE TABLE real_abc(a PRIMARY KEY, b, c)`,
		`INSERT INTO real_abc VALUES(9,9,9)`,
		`SELECT last_insert_rowid() AS r`,
		`CREATE VIRTUAL TABLE echo_abc USING echo(real_abc)`,
		`SELECT last_insert_rowid() AS r`,
		`SELECT total_changes() AS tc`,
		`INSERT INTO echo_abc VALUES(1,2,3)`,
		`SELECT last_insert_rowid() AS r`,
	})
	// A module that EXISTS but rejects its arguments: rtree validates the
	// column count before writing anything, so nothing moves there either.
	differ(t, "a create with bad module arguments leaves the counters alone", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`SELECT last_insert_rowid() AS r`,
		`CREATE VIRTUAL TABLE bad USING rtree(id)`,
		`SELECT last_insert_rowid() AS r`,
		`SELECT total_changes() AS tc`,
	})
	// A create refused for the NAME -- reserved prefix, and an already-taken
	// one -- is refused before any xCreate.
	differ(t, "a create refused by name leaves the counters alone", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`CREATE VIRTUAL TABLE sqlite_stat1 USING rtree(id,x0,x1)`,
		`SELECT last_insert_rowid() AS r`,
		`CREATE TABLE taken(x)`,
		`CREATE VIRTUAL TABLE taken USING rtree(id,x0,x1)`,
		`SELECT last_insert_rowid() AS r`,
		`SELECT total_changes() AS tc`,
	})
	// IF NOT EXISTS over an existing name runs no xCreate at all.
	differ(t, "IF NOT EXISTS over an existing table leaves the counters alone", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS t USING rtree(id,x0,x1)`,
		`SELECT last_insert_rowid() AS r`,
		`SELECT total_changes() AS tc`,
	})
}

// TestSuccessfulVtabCreateStillHidesTheCounter verifies that successful
// virtual table creates still hide the last_insert_rowid counter.
func TestSuccessfulVtabCreateStillHidesTheCounter(t *testing.T) {
	res := run(t, "musql", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`CREATE VIRTUAL TABLE rt USING rtree(id,x0,x1)`,
		`SELECT last_insert_rowid() AS r`,
	})
	if res[2]["kind"] == "error" {
		t.Fatalf("the create itself should succeed, got %v", res[2])
	}
	if res[3]["kind"] != "error" {
		t.Errorf("expected last_insert_rowid() to decline after a successful rtree create, got %v", res[3])
	}
}
