// Tests PRAGMA wal_checkpoint and PRAGMA wal_autocheckpoint. Frame counts are
// declined where the engine's materialization differs from SQLite's page tracking.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestWalPragmasMatchCSQLite(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"a non-WAL database answers -1", []string{
			`CREATE TABLE t(a)`,
			`INSERT INTO t VALUES(1)`,
			`PRAGMA wal_checkpoint`,
			`PRAGMA wal_checkpoint(PASSIVE)`,
			`PRAGMA wal_checkpoint(FULL)`,
			`PRAGMA wal_checkpoint(RESTART)`,
			`PRAGMA wal_checkpoint(TRUNCATE)`,
			`PRAGMA main.wal_checkpoint`,
			`SELECT * FROM t`,
			`PRAGMA journal_mode`,
		}},
		{"a WAL database with an empty log answers 0", []string{
			`PRAGMA journal_mode=wal`,
			`PRAGMA wal_checkpoint`,
			`PRAGMA wal_checkpoint(PASSIVE)`,
			`PRAGMA main.wal_checkpoint`,
			`PRAGMA journal_mode`,
		}},
		// TRUNCATE empties the log.
		{"TRUNCATE is exact whatever was written", []string{
			`PRAGMA journal_mode=wal`,
			`CREATE TABLE t(a)`,
			`INSERT INTO t VALUES(1)`,
			`INSERT INTO t VALUES(2)`,
			`PRAGMA wal_checkpoint(TRUNCATE)`,
			`SELECT count(*) FROM t`,
			`PRAGMA integrity_check`,
			`PRAGMA wal_checkpoint`,
			`PRAGMA wal_checkpoint(truncate)`,
			`INSERT INTO t VALUES(3)`,
			`PRAGMA wal_checkpoint(TRUNCATE)`,
			`SELECT count(*) FROM t ORDER BY 1`,
			`PRAGMA integrity_check`,
			`PRAGMA journal_mode=delete`,
			`SELECT count(*) FROM t`,
		}},
		// Unrecognized checkpoint mode is silently ignored.
		{"an unrecognized checkpoint mode is ignored", []string{
			`CREATE TABLE t(a)`,
			`PRAGMA wal_checkpoint(bogus)`,
			`PRAGMA wal_checkpoint(1)`,
			`PRAGMA wal_checkpoint('')`,
			`SELECT count(*) FROM t`,
			`PRAGMA journal_mode=wal`,
			`PRAGMA wal_checkpoint(bogus)`,
			`PRAGMA journal_mode`,
		}},
		// lock_status returns nothing in non-debug builds.
		{"lock_status answers nothing at all", []string{
			`CREATE TABLE t(a)`,
			`PRAGMA lock_status`,
			`PRAGMA main.lock_status`,
			`PRAGMA lock_status=1`,
			`SELECT count(*) FROM t`,
			`PRAGMA locking_mode`,
			`PRAGMA locking_mode=normal`,
			`PRAGMA locking_mode=NORMAL`,
			`PRAGMA locking_mode=xyz`,
			`PRAGMA locking_mode`,
		}},
		{"wal_autocheckpoint is connection state", []string{
			`PRAGMA wal_autocheckpoint`,
			`PRAGMA wal_autocheckpoint=100`,
			`PRAGMA wal_autocheckpoint`,
			`PRAGMA main.wal_autocheckpoint`,
			`PRAGMA wal_autocheckpoint=0`,
			`PRAGMA wal_autocheckpoint`,
			`PRAGMA wal_autocheckpoint=-5`,
			`PRAGMA wal_autocheckpoint`,
			`PRAGMA wal_autocheckpoint=1000`,
			`PRAGMA wal_autocheckpoint`,
		}},
		{"wal_autocheckpoint survives writes and a mode switch", []string{
			`PRAGMA wal_autocheckpoint=250`,
			`CREATE TABLE t(a)`,
			`INSERT INTO t VALUES(1)`,
			`PRAGMA wal_autocheckpoint`,
			`PRAGMA journal_mode=wal`,
			`INSERT INTO t VALUES(2)`,
			`PRAGMA wal_autocheckpoint`,
			`PRAGMA wal_checkpoint(TRUNCATE)`,
			`PRAGMA wal_autocheckpoint`,
			`SELECT count(*) FROM t`,
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "walpragma/"+tc.name, tc.stmts) })
	}
}

// TestWalCheckpointFrameCountDeclined verifies that frame count checkpoints
// are properly declined.
func TestWalCheckpointFrameCountDeclined(t *testing.T) {
	res := run(t, "musql", []string{
		`PRAGMA journal_mode=wal`,
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA wal_checkpoint`,
		`PRAGMA wal_checkpoint(PASSIVE)`,
		`PRAGMA wal_checkpoint(FULL)`,
		`PRAGMA wal_checkpoint(RESTART)`,
		// TRUNCATE must still work after declined checkpoints.
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`SELECT count(*) FROM t`,
		`PRAGMA integrity_check`,
	})
	for i := 3; i <= 6; i++ {
		if res[i]["kind"] != "error" {
			t.Errorf("statement %d: a checkpoint that would misreport the frame count must be declined, got %v", i, res[i])
		}
	}
	if got := res[7]; got["kind"] == "error" {
		t.Errorf("TRUNCATE must still work after a declined checkpoint: %v", got)
	}
	for _, i := range []int{8, 9} {
		if res[i]["kind"] == "error" {
			t.Errorf("statement %d must still work: %v", i, res[i])
		}
	}

	// Checkpoints inside a transaction with an open read are refused.
	res = run(t, "musql", []string{
		`PRAGMA journal_mode=wal`,
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`BEGIN`,
		`SELECT * FROM t`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`COMMIT`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`SELECT count(*) FROM t`,
	})
	if res[5]["kind"] != "error" {
		t.Errorf("a checkpoint with a read open must be declined, got %v", res[5])
	}
	if res[7]["kind"] == "error" || res[8]["kind"] == "error" {
		t.Errorf("outside the transaction it must work again: %v / %v", res[7], res[8])
	}
}

// TestLockingModeExclusiveServedInWAL tests locking_mode=exclusive in WAL mode,
// verifying that surrounding statements continue to work.
func TestLockingModeExclusiveServedInWAL(t *testing.T) {
	res := run(t, "musql", []string{
		`PRAGMA journal_mode=wal`,
		`CREATE TABLE t(a)`,
		`PRAGMA locking_mode=exclusive`,
		`PRAGMA locking_mode=EXCLUSIVE`,
		`PRAGMA main.locking_mode=exclusive`,
		`PRAGMA locking_mode`,
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*) FROM t`,
		`PRAGMA locking_mode=normal`,
		`SELECT count(*) FROM t`,
	})
	for i := range res {
		if res[i]["kind"] == "error" {
			t.Errorf("statement %d: %v", i, res[i])
		}
	}
}
