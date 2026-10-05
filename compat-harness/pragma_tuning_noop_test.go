package compat

// Session-tuning pragmas must be accepted as no-ops and not change later
// statements; value spellings C SQLite rejects must still be refused.

import (
	"encoding/json"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// pragmaTuningBattery tests that must stay identical with and without tuning pragmas set.
var pragmaTuningBattery = []string{
	"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REAL CHECK(c IS NULL OR c > 0))",
	"CREATE INDEX tb ON t(b)",
	"CREATE TABLE ch(x INTEGER REFERENCES t(a))",
	"CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO ch VALUES(NEW.a); END",
	"INSERT INTO t VALUES(1,'x',1.5),(2,'y',2.5),(3,'z',NULL)",
	"BEGIN",
	"INSERT INTO t VALUES(4,'w',4.0)",
	"COMMIT",
	"UPDATE t SET b='q' WHERE a=2",
	"DELETE FROM t WHERE a=3",
	"SELECT a,b,c,typeof(a),typeof(b),typeof(c) FROM t ORDER BY a",
	"SELECT x FROM ch ORDER BY x",
	"SELECT count(*), sum(a), group_concat(b,'|') FROM (SELECT a,b FROM t ORDER BY a)",
	"ANALYZE",
	"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx",
	"ALTER TABLE t ADD COLUMN d DEFAULT 9",
	"SELECT a,d FROM t ORDER BY a",
	"PRAGMA integrity_check",
	"PRAGMA table_info(t)",
	"PRAGMA page_size",
	"PRAGMA encoding",
	"PRAGMA user_version",
	"INSERT INTO t VALUES(1,'dup',1.0)",
	"INSERT INTO t(a,c) VALUES(9,-5)",
	"SELECT a FROM t WHERE b='q'",
	"VACUUM",
	"SELECT count(*) FROM t",
}

// pragmaTuningSetters are all accepted pragma spellings from the corpus.
var pragmaTuningSetters = []string{
	"PRAGMA synchronous=OFF", "PRAGMA synchronous=NORMAL", "PRAGMA synchronous=FULL",
	"PRAGMA synchronous=0", "PRAGMA synchronous=1", "PRAGMA synchronous=2", "PRAGMA synchronous",
	"PRAGMA fullfsync=1", "PRAGMA checkpoint_fullfsync=1",
	"PRAGMA journal_size_limit=1024", "PRAGMA journal_size_limit=-1",
	"PRAGMA cache_spill=0", "PRAGMA cache_spill=1", "PRAGMA cache_spill=1000",
	"PRAGMA default_cache_size=500", "PRAGMA default_cache_size=-2000",
	"PRAGMA soft_heap_limit=1048576", "PRAGMA soft_heap_limit=0",
	"PRAGMA shrink_memory",
	"PRAGMA mmap_size=0", "PRAGMA mmap_size=268435456",
	"PRAGMA temp_store=MEMORY", "PRAGMA temp_store=FILE", "PRAGMA temp_store=DEFAULT",
	"PRAGMA temp_store=0", "PRAGMA temp_store=1", "PRAGMA temp_store=2",
	"PRAGMA threads=4", "PRAGMA busy_timeout=1000", "PRAGMA read_uncommitted=1",
	"PRAGMA empty_result_callbacks=1", "PRAGMA legacy_file_format=1",
	"PRAGMA vdbe_listing=1", "PRAGMA vdbe_trace=0",
	"PRAGMA lock_proxy_file=':auto:'",
	"PRAGMA cache_size=-4000", "PRAGMA automatic_index=0", "PRAGMA page_size=4096",
}

// TestPragmaTuningNoEffect verifies that each tuning pragma has no effect:
// the whole battery must match between engines when the pragma is set.
func TestPragmaTuningNoEffect(t *testing.T) {
	for _, set := range pragmaTuningSetters {
		stmts := append([]string{set}, pragmaTuningBattery...)
		mine := run(t, "musql", stmts)
		if mine[0]["kind"] == "error" {
			t.Errorf("expected acceptance, got an error for: %s", set)
			continue
		}
		got := marshalAfterFirst(t, mine)
		want := marshalAfterFirst(t, run(t, "cgo", stmts))
		if got != want {
			t.Errorf("battery diverged with %q set\n  cgo:    %s\n  musql: %s", set, want, got)
		}
	}
}

func marshalAfterFirst(t *testing.T, res []map[string]any) string {
	t.Helper()
	if len(res) == 0 {
		t.Fatal("worker returned no results")
	}
	b, err := json.Marshal(res[1:])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPragmaTuningRejectedValues pins rejected values; reserved words like NULL,
// SELECT, TABLE must be refused; keywords that are legal identifiers must be accepted.
func TestPragmaTuningRejectedValues(t *testing.T) {
	for _, name := range []string{"synchronous", "mmap_size", "temp_store", "cache_size", "page_size"} {
		for _, v := range []string{"NULL", "SELECT", "TABLE", "INDEX", "ADD", "NOT", "DISTINCT"} {
			q := fmt.Sprintf("PRAGMA %s=%s", name, v)
			res := run(t, "musql", []string{q})
			if res[0]["kind"] != "error" {
				t.Errorf("expected a decline (C SQLite refuses to parse it), got %v for: %s", res[0], q)
			}
		}
		if name == "page_size" {
			continue
		}
		for _, v := range []string{"KEY", "ABORT", "ON", "DELETE", "DEFAULT"} {
			q := fmt.Sprintf("PRAGMA %s=%s", name, v)
			res := run(t, "musql", []string{q})
			if res[0]["kind"] == "error" {
				t.Errorf("expected acceptance (C SQLite parses it), got an error for: %s", q)
			}
		}
	}
}

// TestPragmaTuningStillDeclined pins pragmas deliberately left out of the
// no-op set because they have real effects on later statements.
func TestPragmaTuningStillDeclined(t *testing.T) {
	for _, q := range []string{
		"PRAGMA hard_heap_limit=1048576",
		"PRAGMA temp_store_directory='/nonexistent-dir-for-this-test'",
	} {
		res := run(t, "musql", []string{q})
		if res[0]["kind"] != "error" {
			t.Errorf("expected a clean decline, got %v for: %s", res[0], q)
		}
	}
	differ(t, "tuningserved", []string{
		"PRAGMA analysis_limit", "PRAGMA analysis_limit=1", "PRAGMA analysis_limit",
		"PRAGMA analysis_limit=0", "PRAGMA analysis_limit",
		"PRAGMA cell_size_check", "PRAGMA cell_size_check=1", "PRAGMA cell_size_check",
		"PRAGMA cell_size_check=0", "PRAGMA cell_size_check",
	})
}

// TestPragmaTuningInTransaction pins pragmas refused inside transactions:
// synchronous by any transaction, temp_store when temp database is open.
func TestPragmaTuningInTransaction(t *testing.T) {
	for _, q := range []string{"PRAGMA synchronous=OFF", "PRAGMA synchronous=2"} {
		res := run(t, "musql", []string{"CREATE TABLE t(a)", "BEGIN", "INSERT INTO t VALUES(1)", q, "COMMIT"})
		if res[3]["kind"] != "error" {
			t.Errorf("expected a decline inside a transaction, got %v for: %s", res[3], q)
		}
	}
	for _, q := range []string{"PRAGMA temp_store=1", "PRAGMA temp_store=MEMORY"} {
		res := run(t, "musql", []string{"CREATE TABLE t(a)", "CREATE TEMP TABLE tt(b)",
			"BEGIN", "INSERT INTO t VALUES(1)", q, "COMMIT"})
		if res[4]["kind"] != "error" {
			t.Errorf("expected a decline with the temp database open, got %v for: %s", res[4], q)
		}
	}
	for _, q := range []string{"PRAGMA synchronous", "PRAGMA temp_store",
		"PRAGMA mmap_size=65536", "PRAGMA cache_spill=0", "PRAGMA journal_size_limit=1024",
		"PRAGMA default_cache_size=500", "PRAGMA soft_heap_limit=1048576",
		"PRAGMA threads=4", "PRAGMA busy_timeout=1000", "PRAGMA fullfsync=1"} {
		res := run(t, "musql", []string{"CREATE TABLE t(a)", "BEGIN", "INSERT INTO t VALUES(1)", q, "COMMIT"})
		if res[3]["kind"] == "error" {
			t.Errorf("expected acceptance inside a transaction, got an error for: %s", q)
		}
	}
	for _, q := range []string{"PRAGMA synchronous=OFF", "PRAGMA temp_store=1"} {
		res := run(t, "musql", []string{q})
		if res[0]["kind"] == "error" {
			t.Errorf("expected acceptance outside a transaction, got an error for: %s", q)
		}
	}
}

// TestPragmaTuningQualifiedStillDeclined pins qualified pragma forms (e.g.,
// aux.temp_store) that still decline because they have per-database effects.
func TestPragmaTuningQualifiedStillDeclined(t *testing.T) {
	for _, name := range []string{"temp_store"} {
		q := fmt.Sprintf("PRAGMA aux.%s = 0", name)
		res := run(t, "musql", []string{"ATTACH ':memory:' AS aux", q})
		if res[1]["kind"] != "error" {
			t.Errorf("expected a decline for the qualified form, got %v for: %s", res[1], q)
		}
	}
}
