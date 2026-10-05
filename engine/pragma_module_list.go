// This file implements PRAGMA module_list: the list of registered virtual-table modules.
// Lists: fts3, fts3tokenize, fts4, fts4aux, rtree, pragma_module_list, and fts5 if enabled.
// depending on unknowable host state), this one is safe to hardcode.
//
// C SQLite does NOT list json_each/json_tree/generate_series here -- those
// are built-in table-valued functions implemented a different way
// (sqlite3VtabEponymousTableInit's own separate registry, not
// sqlite3_create_module()) -- so this is a CURATED list, not a projection of
// this engine's own vtabModules map (which registers all of those, plus every
// pragma_* eponymous table, under one unified mechanism with no such
// distinction).
package engine

import "fmt"

// pragmaModuleListNames is the base set every build reports, in the exact
// order the oracle returns them (already alphabetical).
var pragmaModuleListNames = []string{
	"fts3",
	"fts3tokenize",
	"fts4",
	"fts4aux",
	"pragma_module_list",
	"rtree",
	"rtree_i32",
}

// pragmaModuleListFts5Names is spliced in, in order, when this build has fts5
// registered -- see pragmaModuleListRows.
var pragmaModuleListFts5Names = []string{"fts5", "fts5vocab"}

// pragmaModuleListRows builds module_list's rows: the base set, plus the fts5
// pair exactly when this process has fts5 registered (lookupVtabModule,
// vtab.go -- vtab_fts5.go's own init/UnregisterFTS5 is what makes that
// reflect the build tag), merged back into alphabetical order the same way
// C SQLite's own sqlite3_str output naturally is.
func pragmaModuleListRows() []string {
	names := make([]string, len(pragmaModuleListNames))
	copy(names, pragmaModuleListNames)
	if _, ok := lookupVtabModule("fts5"); ok {
		// Insertion point: alphabetically "fts5"/"fts5vocab" both sort right
		// after "fts4aux" and before "pragma_module_list" -- index 4 in
		// pragmaModuleListNames (0-based: fts3, fts3tokenize, fts4, fts4aux).
		merged := make([]string, 0, len(names)+len(pragmaModuleListFts5Names))
		merged = append(merged, names[:4]...)
		merged = append(merged, pragmaModuleListFts5Names...)
		merged = append(merged, names[4:]...)
		names = merged
	}
	return names
}

// pragmaModuleListVtabModule is the eponymous "pragma_module_list" table-
// valued function, modelled on pragmaListVtabModule (pragma_unknown.go) --
// see its own doc comment for why this lives outside vtab_pragma.go's generic
// wrapper: module_list's declared shape, like pragma_list's, is exactly
// CREATE TABLE x("name") with no hidden input columns at all (mkpragmatab.tcl
// gives it neither PragFlg_Result1 nor SchemaOpt|SchemaReq).
type pragmaModuleListVtabModule struct{}

func init() { RegisterVtabModule("pragma_module_list", pragmaModuleListVtabModule{}) }

func (pragmaModuleListVtabModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		return nil, nil, fmt.Errorf("engine: pragma_module_list takes no module arguments")
	}
	return []VtabColumn{{Name: "name"}}, pragmaModuleListVtabTable{}, nil
}

type pragmaModuleListVtabTable struct{}

func (pragmaModuleListVtabTable) BestIndex(*VtabIndexInfo) error { return nil }

func (pragmaModuleListVtabTable) Open() (VtabCursor, error) {
	return &pragmaModuleListVtabCursor{names: pragmaModuleListRows()}, nil
}

type pragmaModuleListVtabCursor struct {
	names []string
	i     int
}

func (c *pragmaModuleListVtabCursor) Filter(int, string, []Value) error { c.i = 0; return nil }
func (c *pragmaModuleListVtabCursor) Next() error                       { c.i++; return nil }
func (c *pragmaModuleListVtabCursor) Eof() bool                         { return c.i >= len(c.names) }
func (c *pragmaModuleListVtabCursor) Rowid() (int64, error)             { return int64(c.i + 1), nil }
func (c *pragmaModuleListVtabCursor) Close() error                      { return nil }

func (c *pragmaModuleListVtabCursor) Column(i int) (Value, error) {
	if c.i >= len(c.names) || i != 0 {
		return Value{}, fmt.Errorf("engine: pragma_module_list has no column %d", i)
	}
	return Value{Typ: Text, S: []byte(c.names[c.i])}, nil
}
