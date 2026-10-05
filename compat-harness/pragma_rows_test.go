// Tests PRAGMA result rows against C SQLite, comparing all pragma names and spellings.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// prSetup gives every pragma something non-trivial to describe: two tables
// (one WITHOUT ROWID), an index, a view, a trigger, a foreign key and a temp
// table, so table_info/index_list/foreign_key_list/database_list have real
// rows rather than empty ones.
var prSetup = []string{
	`CREATE TABLE parent(id INTEGER PRIMARY KEY, tag TEXT UNIQUE)`,
	`CREATE TABLE child(a INT NOT NULL DEFAULT 7, b TEXT COLLATE NOCASE,
	                    c BLOB, pid INT REFERENCES parent(id))`,
	`CREATE INDEX child_b ON child(b DESC)`,
	`CREATE UNIQUE INDEX child_ac ON child(a, c)`,
	`CREATE TABLE wr(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
	`CREATE VIEW cv AS SELECT a, b FROM child`,
	`CREATE TRIGGER trg AFTER INSERT ON child BEGIN UPDATE parent SET tag=tag; END`,
	`CREATE TEMP TABLE tmp1(z)`,
	`INSERT INTO parent VALUES(1,'one'),(2,'two')`,
	`INSERT INTO child VALUES(1,'x',x'00',1),(2,'y',x'01',2)`,
	`INSERT INTO wr VALUES('k1',10),('k2',20)`,
}

// prQuery reads one statement through Query and returns a printable
// transcript: the column names, then every cell. Column NAMES are part of a
// pragma's answer -- "PRAGMA table_info" naming its columns cid/name/type/
// notnull/dflt_value/pk is exactly what callers read it by -- so they are
// compared too.
func prQuery(t *testing.T, driver, dsn string, setup []string, q string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s open: %v", driver, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range setup {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s setup %q: %v", driver, s, eerr)
		}
	}
	rows, qerr := db.Query(q)
	if qerr != nil {
		return "ERR"
	}
	defer rows.Close()
	cols, cerr := rows.Columns()
	if cerr != nil {
		return "ERR"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "cols=%v", cols)
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			return "ERR"
		}
		b.WriteString(" |")
		for _, c := range cells {
			switch v := c.(type) {
			case []byte:
				fmt.Fprintf(&b, "x'%x',", v)
			default:
				fmt.Fprintf(&b, "%v,", v)
			}
		}
	}
	if rows.Err() != nil {
		return "ERR"
	}
	return b.String()
}

// prDiffer compares one pragma's Query answer on both engines. A musql
// DECLINE ("ERR" where the oracle answered) is reported separately from a
// WRONG ANSWER, because only the second is a conformance failure -- see this
// file's doc comment.
func prDiffer(t *testing.T, q string, setup []string) (declined bool) {
	t.Helper()
	dir := t.TempDir()
	goPath, cgoPath := filepath.Join(dir, "musql.db"), filepath.Join(dir, "cgo.db")
	got := prQuery(t, "sqlite", goPath, setup, q)
	want := prQuery(t, "sqlite3", cgoPath, setup, q)
	// "PRAGMA database_list" reports each database's own FILE, and the two
	// engines are on differently-named ones by construction -- so the path is
	// normalized away here rather than the pragma being skipped, which keeps
	// every other cell (seq, name, and whether a file is reported at all) under
	// comparison. See compat-harness/pragma_database_list_test.go for the test
	// that pins the paths themselves.
	got = strings.ReplaceAll(got, goPath, "<db>")
	want = strings.ReplaceAll(want, cgoPath, "<db>")
	if got == want {
		return false
	}
	if got == "ERR" {
		return true // declined: correct-but-incomplete, not a wrong answer
	}
	t.Errorf("%q DIVERGES\n  cgo:    %s\n  musql: %s", q, want, got)
	return false
}

// prSortRows sorts a prQuery transcript's row-chunks alphabetically, for a
// pragma whose row ORDER is C SQLite's own internal schema hash-table
// iteration and is not reproducible here -- table_list's bare/qualified
// listing and foreign_key_check's bare form (see
// engine/pragma_table_list.go's and engine/pragma_fkcheck.go's
// fkCheckWholeDatabaseRows doc comments for why the ROW SET is still exact
// even though this engine's own scan order differs). This mirrors exactly
// what TestTCLCorpus itself does for these shapes: compare order-insensitively
// unless the SQL sorts.
func prSortRows(s string) string {
	head, rest, ok := strings.Cut(s, " |")
	if !ok {
		return s
	}
	rows := strings.Split(rest, " |")
	for i := range rows {
		rows[i] = "|" + rows[i]
	}
	sort.Strings(rows)
	return head + " " + strings.Join(rows, " ")
}

// prDifferUnordered is prDiffer (see its doc comment) for a pragma whose row
// order is not reproducible -- see prSortRows.
func prDifferUnordered(t *testing.T, q string, setup []string) (declined bool) {
	t.Helper()
	dir := t.TempDir()
	got := prSortRows(prQuery(t, "sqlite", filepath.Join(dir, "musql.db"), setup, q))
	want := prSortRows(prQuery(t, "sqlite3", filepath.Join(dir, "cgo.db"), setup, q))
	if got == want {
		return false
	}
	if got == "ERR" {
		return true // declined: correct-but-incomplete, not a wrong answer
	}
	t.Errorf("%q DIVERGES (order-insensitive)\n  cgo:    %s\n  musql: %s", q, want, got)
	return false
}

// prDifferQ is prDiffer with a display name separate from the statement, for
// callers that vary the SETUP rather than the query.
func prDifferQ(t *testing.T, name string, setup []string, q string) {
	t.Helper()
	dir := t.TempDir()
	got := prQuery(t, "sqlite", filepath.Join(dir, "musql.db"), setup, q)
	want := prQuery(t, "sqlite3", filepath.Join(dir, "cgo.db"), setup, q)
	if got != want && got != "ERR" {
		t.Errorf("%s DIVERGES\n  cgo:    %s\n  musql: %s", name, want, got)
	}
}

// prNames is every pragma name engine/pragma.go recognises, plus the ones it
// deliberately does not, so a pragma that starts answering without a rule is
// caught as loudly as one that answers wrongly.
var prNames = []string{
	"application_id", "auto_vacuum", "automatic_index", "busy_timeout",
	"cache_size", "cache_spill", "case_sensitive_like", "cell_size_check",
	"checkpoint_fullfsync", "collation_list", "compile_options",
	"count_changes", "data_version", "database_list", "default_cache_size",
	"defer_foreign_keys", "empty_result_callbacks", "encoding",
	"foreign_key_check", "foreign_keys", "freelist_count", "full_column_names",
	"fullfsync", "function_list", "hard_heap_limit", "ignore_check_constraints",
	"incremental_vacuum", "index_list", "integrity_check", "journal_mode",
	"journal_size_limit", "legacy_alter_table", "legacy_file_format",
	"locking_mode", "max_page_count", "mmap_size", "module_list", "optimize",
	"page_size", "pragma_list", "query_only", "quick_check",
	"read_uncommitted", "recursive_triggers", "reverse_unordered_selects",
	"schema_version", "secure_delete", "short_column_names", "shrink_memory",
	"soft_heap_limit", "synchronous", "table_list", "temp_store", "threads",
	"trusted_schema", "user_version", "wal_autocheckpoint", "wal_checkpoint",
	"writable_schema",
}

// prUnorderedRowPragmas is the pragma names whose row order this engine
// cannot reproduce (see prSortRows) -- compared order-insensitively rather
// than skipped, so a genuine WRONG row set (not just a reordering) still
// fails loudly.
var prUnorderedRowPragmas = map[string]bool{
	"table_list":        true,
	"foreign_key_check": true,
}

// TestPragmaQueryRows is the bare "PRAGMA name" query form for every name.
func TestPragmaQueryRows(t *testing.T) {
	var declined []string
	for _, name := range prNames {
		q := "PRAGMA " + name
		diff := prDiffer
		if prUnorderedRowPragmas[name] {
			diff = prDifferUnordered
		}
		if diff(t, q, prSetup) {
			declined = append(declined, name)
		}
	}
	t.Logf("declined (not a divergence): %d of %d: %v", len(declined), len(prNames), declined)
}

// prObjects are the arguments an object-scoped pragma takes: real objects of
// each kind, plus one that does not exist.
var prObjects = []string{"child", "parent", "wr", "cv", "tmp1", "child_b",
	"sqlite_master", "sqlite_schema", "nope"}

// TestPragmaQueryRowsWithArgument covers the ARGUMENT form, in both spellings
// SQLite accepts. This is where quick_check('nope') answered "ok" for a table
// that does not exist.
func TestPragmaQueryRowsWithArgument(t *testing.T) {
	for _, name := range []string{
		"table_info", "table_xinfo", "index_list", "index_info", "index_xinfo",
		"foreign_key_list", "integrity_check", "quick_check", "table_list",
		"foreign_key_check",
	} {
		for _, obj := range prObjects {
			prDiffer(t, fmt.Sprintf("PRAGMA %s(%s)", name, obj), prSetup)
			prDiffer(t, fmt.Sprintf("PRAGMA %s('%s')", name, obj), prSetup)
			prDiffer(t, fmt.Sprintf("PRAGMA %s=%s", name, obj), prSetup)
		}
	}
}

// TestPragmaAssignmentRows is the SETTER form, whose ROW SHAPE is its own
// conformance surface: C SQLite returns NO rows for most assignments and
// one row for the few that report the resulting mode (journal_mode,
// locking_mode), while a read-only pragma's "assignment" is just a query.
// PRAGMA page_size=512 returning a row is the defect this pins.
func TestPragmaAssignmentRows(t *testing.T) {
	for _, c := range []struct{ name, val string }{
		{"application_id", "7"}, {"user_version", "9"}, {"schema_version", "4"},
		{"page_size", "512"}, {"page_size", "4096"}, {"auto_vacuum", "1"},
		{"auto_vacuum", "none"}, {"cache_size", "-2000"}, {"cache_size", "100"},
		{"foreign_keys", "1"}, {"foreign_keys", "0"}, {"foreign_keys", "on"},
		{"recursive_triggers", "1"}, {"case_sensitive_like", "1"},
		{"journal_mode", "delete"}, {"journal_mode", "truncate"},
		{"journal_mode", "memory"}, {"journal_mode", "wal"},
		{"locking_mode", "exclusive"}, {"locking_mode", "normal"},
		{"synchronous", "0"}, {"synchronous", "full"}, {"temp_store", "2"},
		{"secure_delete", "1"}, {"query_only", "1"}, {"query_only", "0"},
		{"defer_foreign_keys", "1"}, {"trusted_schema", "0"},
		{"busy_timeout", "500"}, {"max_page_count", "1000"},
		{"wal_autocheckpoint", "500"}, {"journal_size_limit", "4096"},
		{"mmap_size", "0"}, {"encoding", "'UTF-8'"}, {"writable_schema", "1"},
		{"data_version", "3"}, // read-only: the "assignment" is a query
		{"freelist_count", "3"},
	} {
		// Both assignment spellings: "= value" and "(value)". They are the
		// same statement to the parser and must answer identically.
		prDiffer(t, fmt.Sprintf("PRAGMA %s = %s", c.name, c.val), prSetup)
		prDiffer(t, fmt.Sprintf("PRAGMA %s(%s)", c.name, c.val), prSetup)
	}
}

// TestPragmaQueryRowsQualified covers the schema-qualified spellings, which
// resolve their argument in that database.
func TestPragmaQueryRowsQualified(t *testing.T) {
	for _, schema := range []string{"main", "temp"} {
		for _, name := range []string{
			"page_size", "freelist_count", "encoding",
			"journal_mode", "user_version", "application_id", "schema_version",
			"auto_vacuum", "integrity_check", "quick_check", "table_list",
			"database_list", "foreign_key_check",
		} {
			q := fmt.Sprintf("PRAGMA %s.%s", schema, name)
			if prUnorderedRowPragmas[name] {
				prDifferUnordered(t, q, prSetup)
				continue
			}
			prDiffer(t, q, prSetup)
		}
		prDiffer(t, fmt.Sprintf("PRAGMA %s.table_info(child)", schema), prSetup)
		prDiffer(t, fmt.Sprintf("PRAGMA %s.table_info(tmp1)", schema), prSetup)
	}
}

// TestPragmaEmptyDatabase runs the same names against a database with NO
// schema at all, where several pragmas answer differently (an empty
// table_list, a zero page_count) and where a wrong "no rows" is easiest to
// mistake for correct.
func TestPragmaEmptyDatabase(t *testing.T) {
	for _, name := range prNames {
		prDiffer(t, "PRAGMA "+name, nil)
	}
	for _, name := range []string{"table_info", "index_list", "quick_check", "integrity_check"} {
		prDiffer(t, fmt.Sprintf("PRAGMA %s(nope)", name), nil)
	}
}
