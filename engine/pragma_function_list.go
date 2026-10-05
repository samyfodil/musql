// This file implements "PRAGMA function_list" 's eponymous table-valued form,
// "pragma_function_list": the SQL functions a connection can call, one row per
// registered (name, nArg) definition.
//
// # The oracle shape (pragma.c:1462, PragTyp_FUNCTION_LIST)
//
//	for(i=0; i<SQLITE_FUNC_HASH_SZ; i++){
//	  for(p=sqlite3BuiltinFunctions.a[i]; p; p=p->u.pHash){
//	    pragmaFunclistLine(v, p, 1, showInternFunc);   // isBuiltin=1
//	  }
//	}
//	for(j=sqliteHashFirst(&db->aFunc); j; j=sqliteHashNext(j)){
//	  p = (FuncDef*)sqliteHashData(j);
//	  pragmaFunclistLine(v, p, 0, showInternFunc);     // isBuiltin=0
//	}
//
// mkpragmatab.tcl gives function_list "FLAG: Result0" and "COLS: name builtin
// type enc narg flags" -- Result0, so pragmaVtabConnect (pragma.c:2843)
// appends NEITHER "arg HIDDEN" NOR "schema HIDDEN": the declared table is
// exactly its six columns, and "SELECT * FROM pragma_function_list('x')" is
// "too many arguments on pragma_function_list() - max 0", which this engine's
// generic table-valued-call machinery produces once Connect declares zero
// hidden columns (buildVtabConstraints, vtab.go).
//
// pragmaFunclistLine (pragma.c:332) is where each row's other five columns
// come from: type is "w" for anything with xValue (every built-in aggregate is
// also a window function, so no row reports "a"), enc is azEnc[] of the
// encoding bits, and flags is "(p->funcFlags & mask) ^ SQLITE_INNOCUOUS" over
// DETERMINISTIC|DIRECTONLY|SUBTYPE|INNOCUOUS|SQLITE_FUNC_INTERNAL.
//
// # The rows are the oracle's own, in the oracle's own ORDER
//
// An unfiltered "SELECT * FROM pragma_function_list" is ordered by the two
// loops above: the 23 buckets of sqlite3BuiltinFunctions (SQLITE_FUNC_HASH,
// sqliteInt.h:1640, keyed on the first character plus the length of the name),
// each bucket's chain pushed at its head in registration order and each
// same-name overload threaded through pNext (sqlite3InsertBuiltinFuncs,
// callback.c:358-379), and then db->aFunc, the per-connection hash (hash.c), in
// its element-list order. Both orders are fixed properties of the oracle's
// build -- the registration order of the amalgamation, and the order in which
// sqlite3_open_v2 and mattn/go-sqlite3's connection hook register the rest --
// so they are reproduced as the literal output of
//
//	SELECT name, builtin, type, enc, narg, flags FROM pragma_function_list
//
// against mattn/go-sqlite3 3.53.3 in both of this project's oracle builds,
// rather than re-derived: the flags bit arithmetic (SQLITE_FUNC_UNSAFE shares
// SQLITE_INNOCUOUS's bit with the opposite meaning for built-ins) and hash.c's
// element-list order would each be a formula nothing here could check.
//
// The BUILT-IN half is identical in both builds (131 rows). The CONNECTION half
// is where they differ: the default build registers 16 rows -- the fts3 and
// rtree extensions' functions, match() (sqlite3_overload_function), and the
// five authenticate/auth_* functions mattn/go-sqlite3 registers on every
// connection it opens (its sqlite3.go) -- and -tags sqlite_fts5 adds
// fts5's seven, which land INTERLEAVED in that hash's order, not appended.
// Which half applies is decided the way pragma_module_list.go decides it:
// whether this process has fts5 registered.
//
// Every row names a function this engine resolves. The ones whose CALLS are not
// reproduced in every case are documented where they are implemented; listing
// them is what the oracle does, and a name's presence here is not a claim about
// any one call.
package engine

import "fmt"

// pragmaFunctionListRow is one row of the table.
type pragmaFunctionListRow struct {
	name    string
	builtin int64
	typ     string
	enc     string
	narg    int64
	flags   int64
}

// pragmaFunctionListBuiltinRows is the sqlite3BuiltinFunctions half, in the
// oracle's bucket order. See this file's doc comment.
var pragmaFunctionListBuiltinRows = []pragmaFunctionListRow{
	{"group_concat", 1, "w", "utf8", 1, 2097152},
	{"group_concat", 1, "w", "utf8", 2, 2097152},
	{"json_type", 1, "s", "utf8", 1, 2099200},
	{"json_type", 1, "s", "utf8", 2, 2099200},
	{"jsonb_set", 1, "s", "utf8", -1, 3147776},
	{"julianday", 1, "s", "utf8", -1, 2099200},
	{"ntile", 1, "w", "utf8", 1, 2097152},
	{"nullif", 1, "s", "utf8", 2, 2099200},
	{"sqlite_compileoption_get", 1, "s", "utf8", 1, 2097152},
	{"json_valid", 1, "s", "utf8", 1, 2099200},
	{"json_valid", 1, "s", "utf8", 2, 2099200},
	{"json_quote", 1, "s", "utf8", 1, 3147776},
	{"json_patch", 1, "s", "utf8", 2, 2099200},
	{"->", 1, "s", "utf8", 2, 2099200},
	{"json_array", 1, "s", "utf8", -1, 3147776},
	{"current_timestamp", 1, "s", "utf8", 0, 2097152},
	{"sqlite_compileoption_used", 1, "s", "utf8", 1, 2097152},
	{"json_remove", 1, "s", "utf8", -1, 2099200},
	{"json_pretty", 1, "s", "utf8", 1, 2099200},
	{"json_pretty", 1, "s", "utf8", 2, 2099200},
	{"jsonb_patch", 1, "s", "utf8", 2, 2099200},
	{"json_object", 1, "s", "utf8", -1, 3147776},
	{"json_insert", 1, "s", "utf8", -1, 3147776},
	{"->>", 1, "s", "utf8", 2, 2099200},
	{"jsonb_array", 1, "s", "utf8", -1, 3147776},
	{"sum", 1, "w", "utf8", 1, 2097152},
	{"quote", 1, "s", "utf8", 1, 2099200},
	{"printf", 1, "s", "utf8", -1, 2099200},
	{"likelihood", 1, "s", "utf8", 2, 2099200},
	{"json_replace", 1, "s", "utf8", -1, 3147776},
	{"jsonb_remove", 1, "s", "utf8", -1, 2099200},
	{"jsonb_object", 1, "s", "utf8", -1, 3147776},
	{"jsonb_insert", 1, "s", "utf8", -1, 3147776},
	{"json_extract", 1, "s", "utf8", -1, 2099200},
	{"last_value", 1, "w", "utf8", 1, 2097152},
	{"rank", 1, "w", "utf8", 0, 2097152},
	{"sign", 1, "s", "utf8", 1, 2099200},
	{"round", 1, "s", "utf8", 1, 2099200},
	{"round", 1, "s", "utf8", 2, 2099200},
	{"rtrim", 1, "s", "utf8", 1, 2099200},
	{"rtrim", 1, "s", "utf8", 2, 2099200},
	{"jsonb_replace", 1, "s", "utf8", -1, 3147776},
	{"jsonb_extract", 1, "s", "utf8", -1, 2099200},
	{"nth_value", 1, "w", "utf8", 2, 2097152},
	{"random", 1, "s", "utf8", 0, 2097152},
	{"trim", 1, "s", "utf8", 1, 2099200},
	{"trim", 1, "s", "utf8", 2, 2099200},
	{"time", 1, "s", "utf8", -1, 2099200},
	{"total", 1, "w", "utf8", 1, 2097152},
	{"substr", 1, "s", "utf8", 2, 2099200},
	{"substr", 1, "s", "utf8", 3, 2099200},
	{"replace", 1, "s", "utf8", 3, 2099200},
	{"unhex", 1, "s", "utf8", 1, 2099200},
	{"unhex", 1, "s", "utf8", 2, 2099200},
	{"upper", 1, "s", "utf8", 1, 2099200},
	{"subtype", 1, "s", "utf8", 1, 3147776},
	{"typeof", 1, "s", "utf8", 1, 2099200},
	{"load_extension", 1, "s", "utf8", 1, 524288},
	{"load_extension", 1, "s", "utf8", 2, 524288},
	{"json_group_array", 1, "w", "utf8", 1, 3147776},
	{"avg", 1, "w", "utf8", 1, 2097152},
	{"unistr", 1, "s", "utf8", 1, 2099200},
	{"abs", 1, "s", "utf8", 1, 2099200},
	{"octet_length", 1, "s", "utf8", 1, 2099200},
	{"json_group_object", 1, "w", "utf8", 2, 3147776},
	{"jsonb_group_array", 1, "w", "utf8", 1, 3147776},
	{"json_array_length", 1, "s", "utf8", 1, 2099200},
	{"json_array_length", 1, "s", "utf8", 2, 2099200},
	{"json_array_insert", 1, "s", "utf8", -1, 3147776},
	{"strftime", 1, "s", "utf8", -1, 2099200},
	{"substring", 1, "s", "utf8", 2, 2099200},
	{"substring", 1, "s", "utf8", 3, 2099200},
	{"randomblob", 1, "s", "utf8", 1, 2097152},
	{"unicode", 1, "s", "utf8", 1, 2099200},
	{"jsonb_group_object", 1, "w", "utf8", 2, 3147776},
	{"jsonb_array_insert", 1, "s", "utf8", -1, 3147776},
	{"timediff", 1, "s", "utf8", 2, 2099200},
	{"percent_rank", 1, "w", "utf8", 0, 2097152},
	{"row_number", 1, "w", "utf8", 0, 2097152},
	{"string_agg", 1, "w", "utf8", 2, 2097152},
	{"last_insert_rowid", 1, "s", "utf8", 0, 2097152},
	{"sqlite_log", 1, "s", "utf8", 2, 2099200},
	{"unlikely", 1, "s", "utf8", 1, 2099200},
	{"json_error_position", 1, "s", "utf8", 1, 2099200},
	{"char", 1, "s", "utf8", -1, 2099200},
	{"unixepoch", 1, "s", "utf8", -1, 2099200},
	{"count", 1, "w", "utf8", 0, 2097152},
	{"count", 1, "w", "utf8", 1, 2097152},
	{"date", 1, "s", "utf8", -1, 2099200},
	{"concat", 1, "s", "utf8", -3, 2099200},
	{"total_changes", 1, "s", "utf8", 0, 2097152},
	{"changes", 1, "s", "utf8", 0, 2097152},
	{"unistr_quote", 1, "s", "utf8", 1, 2099200},
	{"sqlite_version", 1, "s", "utf8", 0, 2097152},
	{"if", 1, "s", "utf8", -4, 2099200},
	{"coalesce", 1, "s", "utf8", -4, 2099200},
	{"glob", 1, "s", "utf8", 2, 2099200},
	{"zeroblob", 1, "s", "utf8", 1, 2099200},
	{"hex", 1, "s", "utf8", 1, 2099200},
	{"iif", 1, "s", "utf8", -4, 2099200},
	{"sqlite_source_id", 1, "s", "utf8", 0, 2097152},
	{"concat_ws", 1, "s", "utf8", -4, 2099200},
	{"format", 1, "s", "utf8", -1, 2099200},
	{"datetime", 1, "s", "utf8", -1, 2099200},
	{"cume_dist", 1, "w", "utf8", 0, 2097152},
	{"instr", 1, "s", "utf8", 2, 2099200},
	{"json", 1, "s", "utf8", 1, 2099200},
	{"dense_rank", 1, "w", "utf8", 0, 2097152},
	{"ifnull", 1, "s", "utf8", 2, 2099200},
	{"jsonb", 1, "s", "utf8", 1, 2099200},
	{"current_date", 1, "s", "utf8", 0, 2097152},
	{"current_time", 1, "s", "utf8", 0, 2097152},
	{"lag", 1, "w", "utf8", 1, 2097152},
	{"lag", 1, "w", "utf8", 3, 2097152},
	{"lag", 1, "w", "utf8", 2, 2097152},
	{"like", 1, "s", "utf8", 2, 2099200},
	{"like", 1, "s", "utf8", 3, 2099200},
	{"max", 1, "s", "utf8", -3, 2099200},
	{"max", 1, "w", "utf8", 1, 2097152},
	{"min", 1, "s", "utf8", -3, 2099200},
	{"min", 1, "w", "utf8", 1, 2097152},
	{"lead", 1, "w", "utf8", 1, 2097152},
	{"lead", 1, "w", "utf8", 3, 2097152},
	{"lead", 1, "w", "utf8", 2, 2097152},
	{"lower", 1, "s", "utf8", 1, 2099200},
	{"ltrim", 1, "s", "utf8", 1, 2099200},
	{"ltrim", 1, "s", "utf8", 2, 2099200},
	{"first_value", 1, "w", "utf8", 1, 2097152},
	{"length", 1, "s", "utf8", 1, 2099200},
	{"likely", 1, "s", "utf8", 1, 2099200},
	{"json_set", 1, "s", "utf8", -1, 3147776},
}

// pragmaFunctionListConnRows is the db->aFunc half for the default oracle
// build, in the oracle's hash order.
var pragmaFunctionListConnRows = []pragmaFunctionListRow{
	{"auth_enabled", 0, "s", "utf8", 0, 2048},
	{"auth_user_change", 0, "s", "utf8", 3, 2048},
	{"auth_user_add", 0, "s", "utf8", 3, 2048},
	{"rtreecheck", 0, "s", "utf8", -1, 0},
	{"rtreenode", 0, "s", "utf8", 2, 0},
	{"optimize", 0, "s", "utf8", 1, 0},
	{"matchinfo", 0, "s", "utf8", 2, 0},
	{"matchinfo", 0, "s", "utf8", 1, 0},
	{"authenticate", 0, "s", "utf8", 2, 2048},
	{"match", 0, "s", "utf8", 2, 0},
	{"fts3_tokenizer", 0, "s", "utf8", 2, 524288},
	{"fts3_tokenizer", 0, "s", "utf8", 1, 524288},
	{"snippet", 0, "s", "utf8", -1, 0},
	{"auth_user_delete", 0, "s", "utf8", 1, 2048},
	{"rtreedepth", 0, "s", "utf8", 1, 0},
	{"offsets", 0, "s", "utf8", 1, 0},
}

// pragmaFunctionListConnRowsFts5 is the same half for the -tags sqlite_fts5
// build, whose seven extra rows interleave with the default ones.
var pragmaFunctionListConnRowsFts5 = []pragmaFunctionListRow{
	{"auth_enabled", 0, "s", "utf8", 0, 2048},
	{"auth_user_change", 0, "s", "utf8", 3, 2048},
	{"auth_user_add", 0, "s", "utf8", 3, 2048},
	{"fts5_locale", 0, "s", "utf8", 2, 3145728},
	{"rtreecheck", 0, "s", "utf8", -1, 0},
	{"rtreenode", 0, "s", "utf8", 2, 0},
	{"fts5_source_id", 0, "s", "utf8", 0, 2099200},
	{"bm25", 0, "s", "utf8", -1, 0},
	{"optimize", 0, "s", "utf8", 1, 0},
	{"fts5_insttoken", 0, "s", "utf8", 1, 2097152},
	{"fts5", 0, "s", "utf8", 1, 0},
	{"matchinfo", 0, "s", "utf8", 2, 0},
	{"matchinfo", 0, "s", "utf8", 1, 0},
	{"authenticate", 0, "s", "utf8", 2, 2048},
	{"match", 0, "s", "utf8", 2, 0},
	{"highlight", 0, "s", "utf8", -1, 0},
	{"fts3_tokenizer", 0, "s", "utf8", 2, 524288},
	{"fts3_tokenizer", 0, "s", "utf8", 1, 524288},
	{"snippet", 0, "s", "utf8", -1, 0},
	{"auth_user_delete", 0, "s", "utf8", 1, 2048},
	{"rtreedepth", 0, "s", "utf8", 1, 0},
	{"fts5_get_locale", 0, "s", "utf8", -1, 0},
	{"offsets", 0, "s", "utf8", 1, 0},
}

// pragmaFunctionListRows is the whole table for this process: the built-in
// half, then the connection half of whichever oracle build this process
// matches (fts5 registered or not -- see pragmaModuleListRows).
func pragmaFunctionListRows() []pragmaFunctionListRow {
	conn := pragmaFunctionListConnRows
	if _, ok := lookupVtabModule("fts5"); ok {
		conn = pragmaFunctionListConnRowsFts5
	}
	rows := make([]pragmaFunctionListRow, 0, len(pragmaFunctionListBuiltinRows)+len(conn))
	rows = append(rows, pragmaFunctionListBuiltinRows...)
	return append(rows, conn...)
}

// pragmaFunctionListCols is the row shape both forms report.
var pragmaFunctionListCols = []string{"name", "builtin", "type", "enc", "narg", "flags"}

// pragmaFunctionListValueRows is pragmaFunctionListRows rendered as Values, for
// the statement form ("PRAGMA function_list", queryPragmaStmt). The cursor
// below renders the identical fields for the table-valued form.
func pragmaFunctionListValueRows() [][]Value {
	src := pragmaFunctionListRows()
	rows := make([][]Value, len(src))
	for i, r := range src {
		rows[i] = []Value{
			{Typ: Text, S: []byte(r.name)},
			{Typ: Int, I: r.builtin},
			{Typ: Text, S: []byte(r.typ)},
			{Typ: Text, S: []byte(r.enc)},
			{Typ: Int, I: r.narg},
			{Typ: Int, I: r.flags},
		}
	}
	return rows
}

// pragmaFunctionListVtabModule is the eponymous "pragma_function_list"
// table-valued function, modelled on pragmaModuleListVtabModule
// (pragma_module_list.go) and pragmaListVtabModule (pragma_unknown.go) -- like
// both, function_list's mkpragmatab.tcl entry is "FLAG: Result0" with no
// SchemaOpt/SchemaReq either, so its declared shape is exactly its six own
// columns and NO hidden ones, and it lives outside vtab_pragma.go's generic
// wrapper for the same reason they do. Both forms share this file's rows:
// queryPragmaStmt's "function_list" arm (pragma.go) renders them through
// pragmaFunctionListValueRows, exactly as pragma.c:1462 gives the two forms one
// implementation.
type pragmaFunctionListVtabModule struct{}

func init() { RegisterVtabModule("pragma_function_list", pragmaFunctionListVtabModule{}) }

func (pragmaFunctionListVtabModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		return nil, nil, fmt.Errorf("engine: pragma_function_list takes no module arguments")
	}
	cols := []VtabColumn{{Name: "name"}, {Name: "builtin"}, {Name: "type"}, {Name: "enc"}, {Name: "narg"}, {Name: "flags"}}
	return cols, pragmaFunctionListVtabTable{}, nil
}

// pragmaFunctionListVtabTable is the connected instance. It holds no state:
// the rows are pragmaFunctionListRows' fixed table.
type pragmaFunctionListVtabTable struct{}

// BestIndex consumes nothing: like pragma_module_list and pragma_pragma_list,
// there is no hidden input column to constrain (pragmaVtabBestIndex's own
// "if( pTab->nHidden==0 ) return SQLITE_OK" early exit, pragma.c). A WHERE
// clause on "name" (equality OR the mined corpus's own LIKE pattern) is
// applied generically by the engine over whatever rows Open below produces,
// exactly like scanning any ordinary table.
func (pragmaFunctionListVtabTable) BestIndex(*VtabIndexInfo) error { return nil }

func (pragmaFunctionListVtabTable) Open() (VtabCursor, error) {
	return &pragmaFunctionListVtabCursor{rows: pragmaFunctionListRows()}, nil
}

// pragmaFunctionListVtabCursor walks pragmaFunctionListRows, numbered from 1
// like every other cursor-backed pragma vtab in this file family.
type pragmaFunctionListVtabCursor struct {
	rows []pragmaFunctionListRow
	i    int
}

func (c *pragmaFunctionListVtabCursor) Filter(int, string, []Value) error { c.i = 0; return nil }
func (c *pragmaFunctionListVtabCursor) Next() error                       { c.i++; return nil }
func (c *pragmaFunctionListVtabCursor) Eof() bool                         { return c.i >= len(c.rows) }
func (c *pragmaFunctionListVtabCursor) Rowid() (int64, error)             { return int64(c.i + 1), nil }
func (c *pragmaFunctionListVtabCursor) Close() error                      { return nil }

func (c *pragmaFunctionListVtabCursor) Column(i int) (Value, error) {
	if c.i >= len(c.rows) {
		return Value{}, fmt.Errorf("engine: pragma_function_list: no current row")
	}
	r := c.rows[c.i]
	switch i {
	case 0:
		return Value{Typ: Text, S: []byte(r.name)}, nil
	case 1:
		return Value{Typ: Int, I: r.builtin}, nil
	case 2:
		return Value{Typ: Text, S: []byte(r.typ)}, nil
	case 3:
		return Value{Typ: Text, S: []byte(r.enc)}, nil
	case 4:
		return Value{Typ: Int, I: r.narg}, nil
	case 5:
		return Value{Typ: Int, I: r.flags}, nil
	}
	return Value{}, fmt.Errorf("engine: pragma_function_list has no column %d", i)
}
