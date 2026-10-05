// Package engine holds the set of pragma names C SQLite recognizes. An
// unknown name is inert (no-op, not an error). Named pragmas that SQLite
// recognizes are either implemented or declined with reason. The set below is
// read from pragma_pragma_list and is load-bearing for both the acceptance
// rule and for unknownPragmaIsNoop.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// sqlitePragmaNames is the list of pragma names SQLite recognizes.
var sqlitePragmaNames = map[string]bool{
	"analysis_limit":            true,
	"application_id":            true,
	"auto_vacuum":               true,
	"automatic_index":           true,
	"busy_timeout":              true,
	"cache_size":                true,
	"cache_spill":               true,
	"case_sensitive_like":       true,
	"cell_size_check":           true,
	"checkpoint_fullfsync":      true,
	"collation_list":            true,
	"compile_options":           true,
	"count_changes":             true,
	"data_version":              true,
	"database_list":             true,
	"defer_foreign_keys":        true,
	"empty_result_callbacks":    true,
	"encoding":                  true,
	"foreign_key_check":         true,
	"foreign_key_list":          true,
	"foreign_keys":              true,
	"freelist_count":            true,
	"full_column_names":         true,
	"fullfsync":                 true,
	"function_list":             true,
	"hard_heap_limit":           true,
	"ignore_check_constraints":  true,
	"incremental_vacuum":        true,
	"index_info":                true,
	"index_list":                true,
	"index_xinfo":               true,
	"integrity_check":           true,
	"journal_mode":              true,
	"journal_size_limit":        true,
	"legacy_alter_table":        true,
	"locking_mode":              true,
	"max_page_count":            true,
	"mmap_size":                 true,
	"module_list":               true,
	"optimize":                  true,
	"page_count":                true,
	"page_size":                 true,
	"pragma_list":               true,
	"query_only":                true,
	"quick_check":               true,
	"read_uncommitted":          true,
	"recursive_triggers":        true,
	"reverse_unordered_selects": true,
	"schema_version":            true,
	"secure_delete":             true,
	"short_column_names":        true,
	"shrink_memory":             true,
	"soft_heap_limit":           true,
	"synchronous":               true,
	"table_info":                true,
	"table_list":                true,
	"table_xinfo":               true,
	"temp_store":                true,
	"temp_store_directory":      true,
	"threads":                   true,
	"trusted_schema":            true,
	"user_version":              true,
	"wal_autocheckpoint":        true,
	"wal_checkpoint":            true,
	"writable_schema":           true,
}

// unknownPragmaIsNoop reports whether name is unknown to C SQLite (both
// setter and getter are no-ops). Names are compared case-insensitively.
func unknownPragmaIsNoop(name string) bool {
	return !sqlitePragmaNames[r33sFoldIdent(strings.TrimSpace(name))]
}

// ---- PRAGMA pragma_list, and its eponymous pragma_pragma_list() wrapper ----

// pragmaListRows returns one TEXT column "name" with one row per recognized pragma.
func pragmaListRows() ([]string, [][]Value) {
	names := make([]string, 0, len(sqlitePragmaNames))
	for n := range sqlitePragmaNames {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := make([][]Value, len(names))
	for i, n := range names {
		rows[i] = []Value{{Typ: Text, S: []byte(n)}}
	}
	return []string{"name"}, rows
}

// pragmaListVtabModule is the eponymous "pragma_pragma_list" table-valued
// function. It has only a static "name" column, no hidden input columns.
type pragmaListVtabModule struct{}

func init() { RegisterVtabModule("pragma_pragma_list", pragmaListVtabModule{}) }

func (pragmaListVtabModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		// Eponymous only, exactly like every other pragma vtab.
		return nil, nil, fmt.Errorf("engine: pragma_pragma_list takes no module arguments")
	}
	return []VtabColumn{{Name: "name"}}, pragmaListVtabTable{}, nil
}

// pragmaListVtabTable is the connected instance.
type pragmaListVtabTable struct{}

// BestIndex consumes nothing (no hidden input columns).
func (pragmaListVtabTable) BestIndex(*VtabIndexInfo) error { return nil }

func (pragmaListVtabTable) Open() (VtabCursor, error) {
	_, rows := pragmaListRows()
	return &pragmaListVtabCursor{rows: rows}, nil
}

// pragmaListVtabCursor walks pragma rows, numbering them from 1.
type pragmaListVtabCursor struct {
	rows [][]Value
	i    int
}

func (c *pragmaListVtabCursor) Filter(int, string, []Value) error { c.i = 0; return nil }
func (c *pragmaListVtabCursor) Next() error                       { c.i++; return nil }
func (c *pragmaListVtabCursor) Eof() bool                         { return c.i >= len(c.rows) }
func (c *pragmaListVtabCursor) Rowid() (int64, error)             { return int64(c.i + 1), nil }
func (c *pragmaListVtabCursor) Close() error                      { return nil }

func (c *pragmaListVtabCursor) Column(i int) (Value, error) {
	if c.i >= len(c.rows) || i != 0 {
		return Value{}, fmt.Errorf("engine: pragma_pragma_list has no column %d", i)
	}
	return c.rows[c.i][0], nil
}
