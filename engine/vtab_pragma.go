// This file implements SQLite's eponymous "pragma_<name>(...)" table-valued
// functions:
//
//	SELECT * FROM pragma_table_info('t1')
//	SELECT name FROM pragma_table_info WHERE arg='t1'
//	SELECT * FROM pragma_index_xinfo('i1') ORDER BY seqno
//
// Each is a vtab module whose schema is the pragma's own result columns plus
// two hidden inputs, "arg" and "schema", in that order, so a call's first
// argument binds arg and its second schema. Rows come from queryPragmaStmt,
// the same entry point the bare PRAGMA uses, so the two forms agree by
// construction.
//
//   - "SELECT *" shows only the pragma's columns. arg echoes the argument;
//     schema is NULL when not supplied (not 'main').
//   - No argument, a NULL, or a missing table is zero rows, not an error, with
//     the column names still reported.
//   - An explicit schema works; an unattached one errors (queryPragmaStmt's
//     qualifier check).
//   - A parameterless pragma is still a TVF (pragma_user_version) and rejects
//     an argument.
//   - pragma_integrity_check/quick_check take a table name: the argument is
//     quoted into the pragma, so "pragma_integrity_check(1)" is "no such table:
//     1" (the bare pragma's 1 is a max-error count); NULL or none checks
//     everything.
//
// Which pragmas get a wrapper is deliberate: a bare PRAGMA's rows are not
// compared by the differential corpus, but a TVF is a SELECT whose rows are,
// so only pragmas whose bare form is verified exact (generated columns,
// partial/expression indexes, WITHOUT ROWID, automatic indexes, temp,
// absent/NULL targets) are wrapped. table_xinfo of a virtual table reports the
// declared column list (vtabDeclaredColumnsAsSQLiteReportsThem). A compound
// view answers through compoundColumnTypes' fold unless its arms are not
// analyzable.
//
// Not wrapped here: page_count, freelist_count and database_list
// (database_list's file column is the DSN path, never comparable).
// collation_list, function_list and module_list answer through their own code
// (queryPragmaStmt, pragma_function_list.go, pragma_module_list.go).
//
// table_list and foreign_key_check are wrapped (pragma_table_list.go,
// pragma_fkcheck.go) with two differences:
//
//   - table_list declares only "arg" (its first column is already "schema"),
//     so a second argument is an arity error, matching "too many arguments on
//     pragma_table_list() - max 1" (pragmaVtabSingleArgOnly).
//   - with no argument, both list everything rather than return zero rows
//     (pragmaVtabAbsentArgListsAll). Their unordered row order is C's hash
//     iteration and not reproduced, so gates use ORDER BY (fkey5.test 13.12);
//     foreign_key_check's content does not depend on order
//     (fkCheckWholeDatabaseRows), except a "foreign key mismatch" whose text
//     would, which declines.
package engine

import "fmt"

// pragmaVtabColumns is each wrapped pragma's own result columns, in order. It
// restates what the queryPragmaStmt implementations return because Connect must
// declare a schema before any pager exists to ask. pragmaVtabRows asserts the
// two agree on every call, so a future change to a pragma's shape fails loudly
// here instead of silently reporting the wrong column names.
var pragmaVtabColumns = map[string][]string{
	"table_info":        {"cid", "name", "type", "notnull", "dflt_value", "pk"},
	"table_xinfo":       {"cid", "name", "type", "notnull", "dflt_value", "pk", "hidden"},
	"index_list":        {"seq", "name", "unique", "origin", "partial"},
	"index_info":        {"seqno", "cid", "name"},
	"index_xinfo":       {"seqno", "cid", "name", "desc", "coll", "key"},
	"foreign_key_list":  {"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"},
	"foreign_key_check": {"table", "rowid", "parent", "fkid"},
	"table_list":        {"schema", "name", "type", "ncol", "wr", "strict"},
	"database_list":     {"seq", "name", "file"},
	"user_version":      {"user_version"},
	"application_id":    {"application_id"},
	"encoding":          {"encoding"},
	"integrity_check":   {"integrity_check"},
	"quick_check":       {"quick_check"},
	// cache_size's single column is named after the PRAGMA itself, and that is
	// not a convention -- it is pragmaVtabConnect's own i==0 fallback
	// (pragma.c): mkpragmatab.tcl gives cache_size no "COLS:" line, so
	// nPragCName is 0 and Connect emits "(\"%s\"" over pPragma->zName.
	"cache_size": {"cache_size"},
}

// pragmaVtabTakesArg is the wrapped pragmas that accept an "arg" at all. For the
// rest an argument is an error, matching the oracle ("pragma_integrity_check(1)"
// errors); their arg column still exists so the schema shape is uniform.
var pragmaVtabTakesArg = map[string]bool{
	"integrity_check":   true,
	"quick_check":       true,
	"table_info":        true,
	"table_xinfo":       true,
	"index_list":        true,
	"index_info":        true,
	"index_xinfo":       true,
	"foreign_key_list":  true,
	"foreign_key_check": true,
	"table_list":        true,
}

// pragmaVtabAbsentArgListsAll is table_list's own exception to "a missing arg
// answers zero rows" (pragmaVtabRows below): C SQLite's
// "SELECT * FROM pragma_table_list" (no call argument, no WHERE) lists EVERY
// table/view/virtual/shadow row instead -- verified directly against
// mattn/go-sqlite3 3.53.3 ("SELECT count(*) FROM pragma_table_list" answers
// the live count, not 0, on a database with ordinary tables). Every other
// arg-taking wrapped pragma keeps the zero-rows rule.
var pragmaVtabAbsentArgListsAll = map[string]bool{
	"table_list":        true,
	"foreign_key_check": true,
	"integrity_check":   true,
	"quick_check":       true,
}

// pragmaVtabSingleArgOnly is the wrapped pragmas whose Connect declares ONLY
// the "arg" hidden column, never "schema" -- just table_list, whose own FIRST
// result column is already named "schema" (so a second hidden input column of
// the same name would collide) and whose real pragma has no schema argument
// at all: "SELECT * FROM pragma_table_list('t1','main')" is "too many
// arguments on pragma_table_list() - max 1" (verified directly against
// mattn/go-sqlite3 3.53.3), which is exactly what this engine's own generic
// table-valued-function call-argument-count check (buildVtabConstraints,
// vtab.go) already raises once only one hidden column is declared here --
// nothing pragma-specific has to reproduce that wording.
var pragmaVtabSingleArgOnly = map[string]bool{
	"table_list": true,
}

// pragmaVtabSchemaOnlyHidden is the wrapped pragmas whose Connect declares only
// the "schema" hidden column. pragmaVtabConnect emits "arg HIDDEN" for
// PragFlg_Result1 and "schema HIDDEN" for SchemaOpt|SchemaReq, and cache_size
// is "NeedSchema Result0 SchemaReq NoColumns1" (mkpragmatab.tcl), so a call
// argument binds to schema: "pragma_cache_size('temp')" asks the temp
// database.
//
// The echoed column is always NULL: pragmaVtabFilter stores arguments from
// azArg[1] up without Result1, while pragmaVtabColumn reads azArg[0], never
// written. "SELECT cache_size, schema FROM pragma_cache_size('main')" is
// (-2000, NULL).
//
// So a schema constraint declines: C sets omit=1 in pragmaVtabBestIndex so
// "WHERE schema='temp'" is never re-tested, while this engine re-applies the
// WHERE and would either drop the row or report a non-NULL schema. vtab_omit.go
// could now omit a consumed conjunct (omittingVtabModule), but the two-input
// pragma modules have not been proven against the oracle for it.
var pragmaVtabSchemaOnlyHidden = map[string]bool{
	"cache_size": true,
}

func init() {
	for name := range pragmaVtabColumns {
		RegisterVtabModule("pragma_"+name, pragmaVtabModule{pragma: name})
	}
}

// Hidden-column positions, relative to the end of the declared schema.
const (
	pragmaVtabHasArg    = 1 << 0
	pragmaVtabHasSchema = 1 << 1
)

// pragmaVtabModule is the VtabModule for one wrapped pragma.
type pragmaVtabModule struct{ pragma string }

func (m pragmaVtabModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		// Eponymous only: "CREATE VIRTUAL TABLE ... USING pragma_table_info(...)"
		// is not a thing, and neither is a module argument.
		return nil, nil, fmt.Errorf("engine: pragma_%s takes no module arguments", m.pragma)
	}
	own := pragmaVtabColumns[m.pragma]
	singleArg := pragmaVtabSingleArgOnly[m.pragma]
	schemaOnly := pragmaVtabSchemaOnlyHidden[m.pragma]
	cols := make([]VtabColumn, 0, len(own)+2)
	for _, c := range own {
		cols = append(cols, VtabColumn{Name: c})
	}
	// The two hidden columns are independent in pragmaVtabConnect (pragma.c):
	// "arg" comes from PragFlg_Result1 and "schema" from SchemaOpt|SchemaReq, so
	// a pragma can have either, both, or -- cache_size -- only the second.
	if !schemaOnly {
		cols = append(cols, VtabColumn{Name: "arg", Hidden: true})
	}
	if !singleArg {
		cols = append(cols, VtabColumn{Name: "schema", Hidden: true})
	}
	return cols, pragmaVtabTable{pragma: m.pragma, nOwn: len(own), singleArg: singleArg, schemaOnly: schemaOnly}, nil
}

// pragmaVtabTable is the connected instance. It carries no state beyond the
// pragma's identity: the rows are produced by pragmaVtabRows against the pager,
// which the cursor path never sees.
type pragmaVtabTable struct {
	pragma string
	nOwn   int
	// singleArg mirrors pragmaVtabSingleArgOnly: Connect declared only the
	// "arg" hidden column for this pragma, never "schema".
	singleArg bool
	// schemaOnly mirrors pragmaVtabSchemaOnlyHidden: Connect declared only the
	// "schema" hidden column, never "arg" -- so a call's first argument is the
	// SCHEMA. See that map for the C, and for why a schema constraint is then
	// declined rather than served.
	schemaOnly bool
}

// BestIndex consumes an equality on arg and/or schema, assigning argv slots in
// that fixed order so Filter/pragmaVtabRows can read them positionally. This is
// also what makes the CALL form work: a table-valued call's arguments arrive as
// equality constraints on the hidden columns in declaration order.
func (t pragmaVtabTable) BestIndex(info *VtabIndexInfo) error {
	type target struct {
		col int
		bit int
	}
	// Declaration order, which is what a table-valued call's arguments bind to
	// positionally: arg (unless schemaOnly), then schema (unless singleArg).
	var targets []target
	next := t.nOwn
	if !t.schemaOnly {
		targets = append(targets, target{next, pragmaVtabHasArg})
		next++
	}
	if !t.singleArg {
		targets = append(targets, target{next, pragmaVtabHasSchema})
	}
	argv := 0
	idxNum := 0
	for _, target := range targets {
		for i, c := range info.Constraints {
			if c.Usable && c.Op == VtabEQ && c.Column == target.col && info.Usage[i].ArgvIndex == 0 {
				argv++
				info.Usage[i].ArgvIndex = argv
				info.Usage[i].Omit = false // the engine re-applies WHERE; never omit
				idxNum |= target.bit
				break
			}
		}
	}
	// An unusable equality on a hidden input rejects the whole plan, without
	// asking whether a usable one already supplied it -- stricter than
	// json_each's and generate_series':
	//
	//	pragma.c:2902  if( pConstraint->iColumn < pTab->iHidden ) continue;
	//	pragma.c:2903  if( pConstraint->op!=SQLITE_INDEX_CONSTRAINT_EQ ) continue;
	//	pragma.c:2904  if( pConstraint->usable==0 ) return SQLITE_CONSTRAINT;
	//
	// Constraints on the pragma's own columns are left to the WHERE. See
	// buildVtabConstraints for why an unresolvable right side is offered
	// unusable.
	for _, c := range info.Constraints {
		if !c.Usable && c.Op == VtabEQ && c.Column >= t.nOwn {
			return fmt.Errorf("%w: pragma_%s(): its argument depends on another row source, which this engine materializes a virtual table before",
				errVDBEUnsupported, t.pragma)
		}
	}
	info.IdxNum = idxNum
	return nil
}

// Open exists to satisfy VirtualTable. It is never reached: materializeVtab
// routes a pragmaVtabModule through pragmaVtabRows instead, because the rows
// need the pager and a VtabCursor never sees one.
func (t pragmaVtabTable) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("engine: pragma_%s rows come from the pager, not a cursor", t.pragma)
}

// pragmaVtabRows runs the wrapped pragma and returns its rows widened with the
// echoed hidden arg/schema columns, plus 1-based rowids. idxNum/argv are
// BestIndex's own encoding (arg first, then schema).
//
// A missing arg for a pragma that needs one yields ZERO ROWS, never an error --
// the oracle's behavior, and the reason this cannot just refuse the shape.
func (t pragmaVtabTable) pragmaVtabRows(p *ReadOnlyPager, idxNum int, argv []Value) ([][]Value, []int64, error) {
	var argV, schemaV Value
	i := 0
	if idxNum&pragmaVtabHasArg != 0 {
		argV = argv[i]
		i++
	}
	if idxNum&pragmaVtabHasSchema != 0 {
		schemaV = argv[i]
	}
	// A schema-only-hidden pragma cannot serve a schema CONSTRAINT byte-exactly
	// without an omit this engine's BestIndex does not have -- see
	// pragmaVtabSchemaOnlyHidden for the C and for why either alternative is a
	// wrong answer. Declined, so the unconstrained form (which IS byte-exact,
	// echoed NULL and all) can still be served.
	if t.schemaOnly && idxNum&pragmaVtabHasSchema != 0 {
		return nil, nil, fmt.Errorf("%w: pragma_%s with a schema argument (its hidden \"schema\" column reads back NULL in C SQLite -- pragmaVtabFilter writes azArg[1] where pragmaVtabColumn reads azArg[0] -- which only works because pragmaVtabBestIndex omits the constraint, and this engine re-applies WHERE)", errVDBEUnsupported, t.pragma)
	}
	if argV.Typ != Null && !pragmaVtabTakesArg[t.pragma] {
		return nil, nil, fmt.Errorf("engine: pragma_%s takes no argument", t.pragma)
	}
	// No argument (absent, or an explicit NULL) for an arg-taking pragma: zero
	// rows. Asking the pragma itself with an empty target would answer zero rows
	// for most of them anyway, but not for all, so decide it here. table_list and
	// foreign_key_check are the two exceptions (pragmaVtabAbsentArgListsAll):
	// their own bare/NULL-argument form is the whole-database listing, not zero
	// rows -- verified directly against mattn/go-sqlite3 3.53.3 (fkey5.test
	// 13.12's "SELECT * FROM pragma_foreign_key_check" is what makes this
	// concrete: no call parens at all, still the two-row whole-database answer).
	if pragmaVtabTakesArg[t.pragma] && argV.Typ == Null && !pragmaVtabAbsentArgListsAll[t.pragma] {
		return nil, nil, nil
	}
	stmt := &PragmaStmt{Name: t.pragma}
	if argV.Typ != Null {
		stmt.HasValue = true
		stmt.ValueIsString = true
		stmt.ValueText = valueToText(argV)
	}
	if schemaV.Typ != Null {
		stmt.Schema = valueToText(schemaV)
	}
	// An OBJECT-scoped pragma resolves its object across every attached
	// database, main first -- C SQLite's own pragma resolution. That rule
	// lives in queryPragmaStmt (pragma.go), which both this wrapper and the bare
	// "PRAGMA index_list(...)" spelling go through, so the two cannot drift; it
	// was first needed here when cross-database writes landed (attach_write.go)
	// and the mined corpus caught it immediately (pragma4.test:
	// pragma_index_info('i2') answered ZERO rows for an index living in aux,
	// where C SQLite answers its row).
	cols, rows, err := p.queryPragmaStmt(stmt)
	if err != nil {
		return nil, nil, err
	}
	// The declared schema and the pragma's actual shape must agree; a mismatch
	// means pragmaVtabColumns has drifted from the implementation, which would
	// otherwise surface as silently mislabelled columns.
	own := pragmaVtabColumns[t.pragma]
	if len(cols) != len(own) {
		return nil, nil, fmt.Errorf("engine: pragma_%s declares %d columns but PRAGMA %s returned %d",
			t.pragma, len(own), t.pragma, len(cols))
	}
	for i, c := range cols {
		if c != own[i] {
			return nil, nil, fmt.Errorf("engine: pragma_%s declares column %d as %q but PRAGMA %s returned %q",
				t.pragma, i, own[i], t.pragma, c)
		}
	}
	out := make([][]Value, len(rows))
	rowids := make([]int64, len(rows))
	for r, row := range rows {
		full := make([]Value, 0, t.nOwn+2)
		full = append(full, row...)
		if !t.schemaOnly {
			full = append(full, argV)
		}
		if !t.singleArg {
			// schemaV is necessarily NULL for a schemaOnly pragma (a schema
			// constraint was declined above), which is also what the oracle echoes
			// there -- see pragmaVtabSchemaOnlyHidden.
			full = append(full, schemaV)
		}
		out[r] = full
		rowids[r] = int64(r + 1)
	}
	return out, rowids, nil
}
