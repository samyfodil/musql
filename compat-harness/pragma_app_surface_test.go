package compat

import (
	"strings"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestPragmaAppSurface tests pragmas an application might use: getters and
// setters compared against C SQLite. Declines are reported, not tallied.
// function_list came off that list too: its rows were already the oracle's own,
// read out of it rather than re-derived (pragma_function_list.go), and only the
// STATEMENT spelling was missing where the eponymous pragma_function_list()
// already served them -- pragma.c:1462 gives the two forms one implementation.
//
// threads, soft_heap_limit, defer_foreign_keys=ON, max_page_count=<n> and
// temp_store=<v> were all on that gap list and are now served in full, each with
// its own gate: the first two per C's own scopes (pragma_resource_limits.go), the
// next two carried as connection state through the driver, and temp_store's
// SETTER routed to the write path where the engine implements it
// (temp_store_setter_test.go -- it used to succeed through Exec and decline
// through Query, so whether an application's pragma worked depended on which
// database/sql method it happened to call).
//
// A decline is an ERROR, never a wrong answer, so a swap that meets one fails
// loudly at that statement rather than drifting. That is the property this test
// protects: if one of the listed declines starts being SERVED, the assertion
// below says so and the case moves to the agreeing set with its oracle evidence.
func TestPragmaAppSurface(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE INDEX i ON t(b)`,
		`INSERT INTO t VALUES(1,'x')`,
	}
	// PRAGMA database_list is deliberately absent: its "file" column is the
	// database's own path, which differs by construction when each engine opens
	// its own copy. pragma_database_list_test.go gates it against one shared
	// file instead.
	agree := []string{
		`PRAGMA journal_mode`, `PRAGMA journal_mode=DELETE`,
		// WAL is served on this format: the delta already IS a write-ahead log, and
		// the mode is a catalog field (journal_mode_segment_test.go).
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous`, `PRAGMA synchronous=NORMAL`,
		`PRAGMA foreign_keys`, `PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout`, `PRAGMA busy_timeout=5000`,
		`PRAGMA cache_size`, `PRAGMA cache_size=-2000`,
		`PRAGMA temp_store`,
		`PRAGMA page_size`, `PRAGMA auto_vacuum`, `PRAGMA auto_vacuum=INCREMENTAL`,
		`PRAGMA wal_autocheckpoint`, `PRAGMA wal_autocheckpoint=100`,
		`PRAGMA locking_mode`,
		`PRAGMA user_version`, `PRAGMA user_version=7`,
		`PRAGMA application_id`, `PRAGMA application_id=3`,
		`PRAGMA secure_delete`, `PRAGMA secure_delete=ON`,
		`PRAGMA recursive_triggers`, `PRAGMA recursive_triggers=ON`,
		`PRAGMA ignore_check_constraints=ON`,
		`PRAGMA query_only`, `PRAGMA query_only=ON`,
		`PRAGMA read_uncommitted`,
		`PRAGMA mmap_size`, `PRAGMA mmap_size=0`,
		`PRAGMA analysis_limit`, `PRAGMA analysis_limit=400`,
		`PRAGMA cell_size_check`, `PRAGMA legacy_alter_table`, `PRAGMA trusted_schema`,
		`PRAGMA writable_schema`, `PRAGMA case_sensitive_like=ON`,
		`PRAGMA encoding`, `PRAGMA journal_size_limit`, `PRAGMA journal_size_limit=1000`,
		`PRAGMA fullfsync`, `PRAGMA checkpoint_fullfsync`,
		`PRAGMA optimize`, `PRAGMA shrink_memory`, `PRAGMA incremental_vacuum`,
		`PRAGMA integrity_check`, `PRAGMA quick_check`, `PRAGMA foreign_key_check`,
		`PRAGMA table_info(t)`, `PRAGMA table_xinfo(t)`, `PRAGMA index_list(t)`,
		`PRAGMA index_info(i)`, `PRAGMA index_xinfo(i)`, `PRAGMA table_list`,
		`PRAGMA collation_list`, `PRAGMA pragma_list`, `PRAGMA function_list`,
		`PRAGMA locking_mode=EXCLUSIVE`,
		`PRAGMA data_version`,
		`PRAGMA schema_version`, `PRAGMA wal_checkpoint(PASSIVE)`, `PRAGMA vdbe_trace`,
		// The RESOURCE limits, served out of tracked state since
		// pragma_resource_limits.go: threads per connection with the build's
		// clamp, soft_heap_limit PROCESS-global exactly as C keeps it, and
		// hard_heap_limit's getter.
		`PRAGMA threads`, `PRAGMA threads=2`, `PRAGMA threads=100`,
		`PRAGMA threads=-1`, `PRAGMA threads=0x3`, `PRAGMA threads='4abc'`,
		`PRAGMA soft_heap_limit`, `PRAGMA soft_heap_limit=1024`,
		`PRAGMA soft_heap_limit=0x400`, `PRAGMA soft_heap_limit=-5`,
		`PRAGMA soft_heap_limit=0`, `PRAGMA hard_heap_limit`,
		// defer_foreign_keys in AUTOCOMMIT, whose whole lifetime is now carried
		// on the connection -- see defer_foreign_keys_pragma_test.go for the
		// sequences that pin it, including the one the old decline existed for.
		`PRAGMA defer_foreign_keys`, `PRAGMA defer_foreign_keys=ON`,
		`PRAGMA defer_foreign_keys=2`, `PRAGMA defer_foreign_keys=bogus`,
		// temp_store's SETTER, which the driver now routes to the write path.
		`PRAGMA temp_store=MEMORY`, `PRAGMA temp_store=FILE`,
		`PRAGMA temp_store=DEFAULT`, `PRAGMA temp_store=2`, `PRAGMA temp_store=junk`,
	}
	declines := []string{
		`PRAGMA module_list`, `PRAGMA compile_options`,
		// A ceiling in pages, the only page-shaped pragma this format declines
		// (docs/segment-format-compatibility.md).
		`PRAGMA max_page_count`, `PRAGMA max_page_count=100000`, `PRAGMA max_page_count=0`,
	}
	// "PRAGMA hard_heap_limit = N" is declined too, and it is asserted WITHOUT
	// asking the oracle: sqlite3_hard_heap_limit64 is PROCESS-global and really
	// enforced, so one cgo set of it makes every later allocation in this test
	// binary fail with SQLITE_NOMEM -- measured, and it took out the rest of a
	// probe run. The decline's own reason is in pragma_resource_limits.go.
	t.Run("decline-hard_heap_limit-set", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if got := renderQuery(db, `PRAGMA hard_heap_limit=5000`); got != "ERR" {
			t.Errorf("PRAGMA hard_heap_limit=5000: served now (%s) -- if the enforcement is real, move it to the agreeing set", got)
		}
		if got := renderQuery(db, `PRAGMA hard_heap_limit`); got != "[hard_heap_limit][0]" {
			t.Errorf("the refused set must leave the limit at 0, got %s", got)
		}
		db.Close()
	})
	run := func(q string) (cgo, mus string) {
		var out [2]string
		for i, drv := range []string{"sqlite3", "sqlite"} {
			db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			for _, s := range setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			out[i] = renderQuery(db, q)
			db.Close()
		}
		return out[0], out[1]
	}
	for qi, q := range agree {
		qi, q := qi, q
		t.Run(fmt.Sprintf("agree-%02d", qi), func(t *testing.T) {
			cgo, mus := run(q)
			if cgo != mus {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cgo, mus)
			}
		})
	}
	// ...and the two COUNTS both engines answer with their own storage's number,
	// which is not comparable and is not a gap either: see storageOnlyColumns
	// (harness_test.go). Asserted as ANSWERED-BY-BOTH rather than compared.
	for _, q := range []string{`PRAGMA page_count`, `PRAGMA freelist_count`} {
		q := q
		t.Run("answers-"+strings.ReplaceAll(q, " ", "_"), func(t *testing.T) {
			cgo, mus := run(q)
			if cgo == "" || strings.Contains(mus, "unsupported") || strings.Contains(mus, "error") {
				t.Errorf("%s must be ANSWERED by both\n  cgo: %s\n  mus: %s", q, cgo, mus)
			}
		})
	}
	for qi, q := range declines {
		qi, q := qi, q
		t.Run(fmt.Sprintf("decline-%02d", qi), func(t *testing.T) {
			cgo, mus := run(q)
			if cgo == "ERR" {
				t.Errorf("%s: the ORACLE refuses it too -- this case no longer measures a decline", q)
				return
			}
			if mus != "ERR" {
				t.Errorf("%s: served now (%s) -- move it to the agreeing set with its oracle evidence", q, mus)
			}
		})
	}
}
