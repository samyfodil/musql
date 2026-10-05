// Virtual tables: a module interface (roughly xConnect / xBestIndex / xOpen /
// xFilter / xNext / xColumn / xRowid / xEof), a module registry, CREATE VIRTUAL
// TABLE / DROP TABLE persistence, and the read-side row source.
//
// A virtual table is read by instantiating the module, driving its cursor
// (Filter once, then Next/Column/Rowid/Eof) and materializing the (columns,
// rows, rowids) a cursor over it scans (OpOpenDerived, runVtabOnce), so WHERE,
// ORDER BY, joins and aggregation treat it like a table with those rows.
//
// Constraint pushdown: a table-valued function such as generate_series is
// unbounded without its inputs, which arrive as call arguments
// ("generate_series(1,10)") or equality constraints on hidden columns ("FROM
// generate_series WHERE start=1 AND stop=10"). Both become candidate
// constraints for BestIndex, whose chosen values go to Filter as argv, as in C.
// The engine still re-applies the whole WHERE, harmless for a module whose
// Column() echoes the input. A module that transforms it (fts3tokenize reports
// "input" as text, so "WHERE input = 123" would drop its own row) opts into
// having the conjunct removed from the residual WHERE (vtab_omit.go).
package engine

import (
	"errors"
	"fmt"
	"strings"
)

// ---- module interface ----

// VtabColumn is one column of a virtual table's declared schema. Type is the
// declared type text (may be ""), which drives column affinity exactly like a
// real CREATE TABLE column's type. Hidden marks a HIDDEN column (columnInfo.
// Hidden): usable in WHERE and in an explicit reference, and carried in every
// row, but omitted from "*" -- SQLite's semantics for a table-valued
// function's input columns (generate_series' start/stop/step).
type VtabColumn struct {
	Name   string
	Type   string
	Hidden bool
	// Unindexed marks an fts5 column declared "<name> UNINDEXED" (vtab_fts5.go).
	// Such a column is still STORED and still returned by SELECT; it simply
	// contributes no tokens to the full-text index, so nothing in it can be
	// MATCHed. Always false for every other module.
	Unindexed bool
	// NumericAffinity gives a column NUMERIC affinity instead of
	// typeAffinity(Type)'s BLOB-for-empty-type, for a column C declares as a bare
	// "<name> HIDDEN". sqlite3AddColumn takes the no-type BLOB default only when
	// the type-name token count is zero (build.c:1490, 1567); a trailing "HIDDEN"
	// is first read as the type name, so the affinity comes from
	// sqlite3AffinityType("HIDDEN"), which matches none of its rules and returns
	// SQLITE_AFF_NUMERIC (build.c:1653, 1716). The later pass recognizing HIDDEN
	// clears the reported type but not the affinity (table_xinfo shows type "").
	//
	// Set only where verified: fts4aux's languageid (vtab_fts3aux.go) and the
	// fts3/fts4 table's "languageid=" column, both declared "%Q HIDDEN"
	// (fts3.c:653-654). A column with an explicit Type (fts3's "docid INTEGER
	// HIDDEN") does not need it. Check each module's own schema string; a missed
	// one is a silent wrong answer for a TEXT-literal comparison.
	NumericAffinity bool
}

// VtabModule is a registered virtual-table module: SQLite's xConnect/xCreate.
// Connect declares the table's column schema from the CREATE VIRTUAL TABLE
// module arguments (the raw strings inside "USING module(arg, arg, ...)";
// empty for an eponymous access) and returns a VirtualTable ready to open.
type VtabModule interface {
	Connect(args []string) (cols []VtabColumn, tab VirtualTable, err error)
}

// catalogVtabModule is a module whose DECLARED COLUMNS can depend on ANOTHER
// object in the same database, so Connect alone cannot answer for it. fts4's
// "content=xxx" is the only one: a table declaring no columns of its own
// borrows the content table's (fts3_content.go). Every call site that holds a
// database goes through vtabConnect below so those tables resolve; the ones
// that do not (a schema-only probe) still get Connect's own clean error.
type catalogVtabModule interface {
	ConnectIn(cat fts3Catalog, args []string) ([]VtabColumn, VirtualTable, error)
}

// vtabConnect is mod.Connect with cat offered to a module that can use it.
func vtabConnect(cat fts3Catalog, mod VtabModule, args []string) ([]VtabColumn, VirtualTable, error) {
	if cm, ok := mod.(catalogVtabModule); ok {
		return cm.ConnectIn(cat, args)
	}
	return mod.Connect(args)
}

// VirtualTable is a connected virtual table instance: SQLite's sqlite3_vtab.
type VirtualTable interface {
	// BestIndex is offered the candidate constraints (each a column + operator
	// the engine could feed to Filter) and chooses which to consume by setting
	// info.Usage[i].ArgvIndex to a 1-based position in Filter's argv slice. It
	// may also set info.IdxNum / info.IdxStr as an opaque plan its own Filter
	// understands. A module that consumes nothing (leaving every ArgvIndex 0)
	// is scanned in full.
	BestIndex(info *VtabIndexInfo) error
	// Open returns a fresh cursor over this table.
	Open() (VtabCursor, error)
}

// VtabCursor is a scan cursor over a virtual table: SQLite's
// sqlite3_vtab_cursor. Filter is called once to start the scan (positioning on
// the first row); then the engine loops Column/Rowid + Next until Eof.
type VtabCursor interface {
	Filter(idxNum int, idxStr string, argv []Value) error
	Next() error
	Eof() bool
	Column(i int) (Value, error)
	Rowid() (int64, error)
	Close() error
}

// VtabOp identifies a constraint operator, mirroring SQLITE_INDEX_CONSTRAINT_*.
type VtabOp int

const (
	VtabEQ VtabOp = iota
	VtabGT
	VtabLE
	VtabLT
	VtabGE
)

// VtabConstraint is one candidate constraint offered to BestIndex: column
// Column compared with operator Op. Usable is false for a constraint whose
// right-hand side this engine cannot supply a value for where the virtual
// table is materialized -- typically a reference to another FROM item's
// column -- mirroring sqlite3_index_constraint.usable, which where.c:4389-4394
// clears for a term whose prereqRight is not yet in scope for the loop order
// being costed. Such a constraint is still OFFERED: see buildVtabConstraints
// for why withholding it was a wrong answer.
type VtabConstraint struct {
	Column int
	Op     VtabOp
	Usable bool

	// Omitted reports that this constraint's WHERE conjunct was already removed
	// from the residual WHERE, so rows will not be re-tested against it
	// (aConstraintUsage[].omit, honored). It is an input to BestIndex, decided at
	// compile time by omittingVtabModule where the residual WHERE is built, so a
	// module that must decline when its constraint is re-applied (fts3tokenize)
	// keeps declining unless it sees it: a missed omission is a decline, never a
	// dropped row. True for a call argument; otherwise false except for a module
	// that opts in. VtabConstraintUsage.Omit is the advisory output half.
	Omitted bool
}

// omittingVtabModule is a module whose BestIndex consumes one WHERE conjunct
// and guarantees its rows satisfy it, C's "aConstraintUsage[i].omit = 1"
// (fts3tokBestIndexMethod, fts3_tokenize_vtab.c:248), needed when Column()
// transforms the constrained value. OmittedConjunct is asked at compile time
// from the AST and must name the conjunct BestIndex consumes at run time, or a
// filter would be dropped that nothing applied. The one implementation
// (fts3TokModule) answers only for a literal right-hand side, which always
// folds and so is always BestIndex's first usable EQ. -1 ("nothing") is always
// safe. conj is the item's tvfWhere; scopeName the qualifier that belongs to
// this item.
type omittingVtabModule interface {
	OmittedConjunct(conj []Expr, scopeName string) int
}

// VtabConstraintUsage is BestIndex's per-constraint decision, parallel to
// VtabIndexInfo.Constraints: ArgvIndex is the 1-based slot this constraint's
// value occupies in Filter's argv (0 = not used).
//
// Omit is C's aConstraintUsage[].omit, ported for completeness and read by
// nothing. This engine's omission is decided on the INPUT side instead, at
// COMPILE time and therefore BEFORE BestIndex runs at all, and reported back
// as VtabConstraint.Omitted -- see vtab_omit.go for why that inversion is what
// makes it safe. Setting Omit here changes no behaviour; reading
// VtabConstraint.Omitted is what tells a module what actually happened.
type VtabConstraintUsage struct {
	ArgvIndex int
	Omit      bool
}

// VtabIndexInfo is BestIndex's in/out parameter: SQLite's
// sqlite3_index_info.
type VtabIndexInfo struct {
	Constraints []VtabConstraint      // input: candidate constraints
	Usage       []VtabConstraintUsage // output: parallel to Constraints
	IdxNum      int                   // output: opaque plan number for Filter
	IdxStr      string                // output: opaque plan string for Filter
}

// ---- registry ----

// vtabModules is the package-level virtual-table module registry: module name
// (lower-cased) -> module. A built-in module registers here from an init()
// (see vtab_series.go's generate_series). This mirrors the driver's existing
// singleton registration model: registrations are global and
// apply to every connection.
var vtabModules = map[string]VtabModule{}

// RegisterVtabModule registers m under name (case-insensitively). A later
// registration under the same name replaces the earlier one.
func RegisterVtabModule(name string, m VtabModule) {
	vtabModules[r33sFoldIdent(name)] = m
}

// lookupVtabModule returns the module registered under name (case-insensitive).
func lookupVtabModule(name string) (VtabModule, bool) {
	m, ok := vtabModules[r33sFoldIdent(name)]
	return m, ok
}

// unregisterVtabModule removes the module registered under name (used by
// UnregisterFTS5 to keep an opt-in module out of processes that did not ask
// for it).
func unregisterVtabModule(name string) {
	delete(vtabModules, r33sFoldIdent(name))
}

// withVtabWhere returns stmt.From, or a shallow copy whose items carry the
// WHERE's top-level conjuncts in tvfWhere, so a virtual-table source
// (materializeVtab) can push hidden-column equalities into BestIndex/Filter.
// The copy leaves the parsed statement unmodified; base tables ignore tvfWhere.
// It also stamps fts3ContentUnneeded from fts3StmtContentUnneeded, the same
// predicate compiler.fts3ContentUnneeded uses, so the two cannot disagree.
func withVtabWhere(p *ReadOnlyPager, stmt *SelectStmt) []FromItem {
	from := stmt.From
	if len(from) == 0 {
		return from
	}
	// NON-NIL even for a statement with no WHERE at all, so that tvfWhere ==
	// nil means "this FROM item never passed through here" rather than "this
	// statement had no conjuncts". fts3_content.go needs to tell those apart:
	// whether a "content=" table's row source is its content table or its
	// INDEX turns on whether the statement offered a MATCH, and answering
	// "it did not" for an item nobody ever handed the WHERE to would be a
	// wrong answer rather than a decline.
	conj := []Expr{}
	if stmt.Where != nil {
		conj = splitTopLevelAnd(stmt.Where)
	}
	out := make([]FromItem, len(from))
	copy(out, from)
	for i := range out {
		out[i].tvfWhere = conj
		out[i].fts3ContentUnneeded, out[i].fts3ContentWanted = fts3StmtContentUnneeded(p, stmt, out[i].Table, out[i].Alias)
	}
	return out
}

// ---- read-path integration (join.go's resolveFrom calls these) ----

// isVtabItem reports whether FROM item it must be resolved as a virtual table
// rather than an ordinary base table -- i.e. it is a table-valued function
// call ("generate_series(1,10)") or a persisted CREATE VIRTUAL TABLE. Both
// must be intercepted BEFORE resolveTable (a persisted vtab's sqlite_schema
// row has type "table", which resolveTable would otherwise try to parse as an
// ordinary CREATE TABLE). A BARE eponymous-module reference is deliberately
// NOT covered here: it is resolved only AFTER resolveTable fails, so a real
// table of the same name always wins (SQLite's eponymous-module shadowing
// rule).
func (p *ReadOnlyPager) isVtabItem(it FromItem) (bool, error) {
	if it.TableFunc {
		return true, nil
	}
	if it.Subquery != nil {
		return false, nil
	}
	// A schema-qualified reference counts, including the qualifier a view's
	// expansion puts on its body's FROM items ("CREATE VIEW v AS SELECT ...
	// FROM <vtab>"; fts3aux1.test). The receiver is already the item's
	// owning pager, and the scope keeps "temp." from matching a main vtab.
	_, _, ok, err := p.createdVtabDefIn(fromItemScope(it), it.Table)
	return ok, err
}

// eponymousOnlyVtabModule reports whether a module can ONLY be named in a FROM
// clause and never CREATEd. C SQLite registers such a module with a NULL
// xCreate, and sqlite3VtabCallCreate's constructor path gives it the SAME error
// an unregistered module gets -- "no such module: %s" (vtab.c:789-790, whose
// test is `pMod==0 || pMod->pModule->xCreate==0 || pMod->pModule->xDestroy==0`).
// Verified against 3.53.3: "CREATE VIRTUAL TABLE g1 USING generate_series" and
// "... USING pragma_table_list" both answer "no such module: <name>", while
// fts3tokenize (which does have an xCreate) succeeds.
func eponymousOnlyVtabModule(name string) bool {
	n := r33sFoldIdent(name)
	return n == "generate_series" || strings.HasPrefix(n, "pragma_")
}

// isEponymousVtabName reports whether name is a registered virtual-table
// module usable eponymously (by its own name, with no CREATE). Consulted by
// resolveFrom only after resolveTable and the view lookup both fail.
func isEponymousVtabName(name string) bool {
	_, ok := lookupVtabModule(name)
	return ok
}

// vtabColumnAffinity is typeAffinity(c.Type), except for a column carrying
// VtabColumn's own NumericAffinity flag (see its doc comment) -- the columns
// this comparison-affinity fix could safely be verified for. This is the
// SOLE difference from the ordinary type-name-driven rule.
func vtabColumnAffinity(c VtabColumn) affinity {
	if c.NumericAffinity {
		return affNumeric
	}
	return typeAffinity(c.Type)
}

// vtabScopeColumns returns the DECLARED columns of FROM item it read as a
// virtual table, in the same columnInfo shape materializeVtab builds for its
// scan -- so a schema-only analyzer (subqueryScopes, subquery_validate.go) can
// resolve names against a vtab without materializing a single row. It errors
// when it names no registered module, which is the caller's signal that this is
// an ordinary missing table.
func (p *ReadOnlyPager) vtabScopeColumns(it FromItem) ([]columnInfo, error) {
	mod, modArgs, err := p.resolveVtabModuleForItem(it)
	if err != nil {
		return nil, err
	}
	vcols, _, err := vtabConnect(p.fts3Catalog(), mod, modArgs)
	if err != nil {
		return nil, err
	}
	cols := make([]columnInfo, len(vcols))
	for i, c := range vcols {
		cols[i] = columnInfo{Name: c.Name, DeclType: c.Type, Aff: vtabColumnAffinity(c), Hidden: c.Hidden, Unindexed: c.Unindexed}
	}
	return cols, nil
}

// vtabDrivesBestIndex reports whether a FROM item resolving to module mod gets
// its rows from the module's BestIndex/Filter/cursor, i.e. reaches
// materializeVtab's tail, rather than one of the persisted row sources ahead of
// it (fts5's %_content via materializeFts5, a writable vtab's store via
// materializeWritableVtab, fts3/fts4's shadow tables via materializeFts3Item).
// The checks mirror materializeVtab's dispatch in order and must stay so.
// annotateVtabCorrelations reads it at compile time so it does not arrange a
// per-outer-row constraint for a source that never consults one.
func vtabDrivesBestIndex(mod VtabModule, it FromItem) bool {
	if it.TableFunc {
		return true
	}
	if _, isFts5 := mod.(fts5Module); isFts5 {
		return false
	}
	if _, isWritable := mod.(writableVtabModule); isWritable {
		return false
	}
	if _, isFts3 := mod.(fts3Module); isFts3 {
		return false
	}
	return true
}

// materializeVtab resolves FROM item it as a virtual table and drives its
// cursor to completion, returning (columns, rows, rowids). params are the bound
// parameters for call arguments and pushed constraint right-hand sides; outer
// is the enclosing trigger row they may reference (buildVtabConstraints).
// trig/from are the compiled way an argument names the firing row: the trigger
// context the source was resolved under (derivedSource.vtabTrig) and the
// machine holding the trigger registers; nil off the trigger path. corr carries
// constraint right-hand sides a correlated source computed from its outer row
// (vtab_correlated.go); nil otherwise.
func (p *ReadOnlyPager) materializeVtab(it FromItem, params []Value, outer *evalCtx, trig *trigCompileCtx, from *vdbe, corr []vtabCorrValue) ([]columnInfo, [][]Value, []int64, error) {
	mod, modArgs, err := p.resolveVtabModuleForItem(it)
	if err != nil {
		return nil, nil, nil, err
	}
	vcols, tab, err := vtabConnect(p.fts3Catalog(), mod, modArgs)
	if err != nil {
		return nil, nil, nil, err
	}
	cols := make([]columnInfo, len(vcols))
	for i, c := range vcols {
		cols[i] = columnInfo{Name: c.Name, DeclType: c.Type, Aff: vtabColumnAffinity(c), Hidden: c.Hidden, Unindexed: c.Unindexed}
	}

	// A WRITABLE virtual table (rtree) persists its rows under its own name
	// (vtab_write.go): read them by table name, presenting each row as its full
	// declared columns [id(=rowid), coord0, ...]. This is what makes a writable
	// vtab's data visible to a pager that did not write it. Connect's throwaway
	// store above supplied only the column schema.
	// An fts5 table is writable but keeps its rows in %_content instead, like
	// fts3 -- see fts5_shadow.go's materializeFts5. Checked first, because it
	// has no b-tree of its own for materializeWritableVtab to read.
	if _, isFts5 := mod.(fts5Module); isFts5 && !it.TableFunc {
		return p.materializeFts5(it, cols)
	}
	// An r-tree is writable but keeps its rows in %_node, like fts5 in
	// %_content -- see rtree_shadow.go. Checked before the generic writable
	// arm, which would read a b-tree it no longer has.
	if rm, isRtree := mod.(rtreeModule); isRtree && !it.TableFunc {
		return p.materializeRtree(rm, modArgs, it.Table, cols)
	}
	if _, isWritable := mod.(writableVtabModule); isWritable && !it.TableFunc {
		return p.materializeWritableVtab(it.Table, cols)
	}
	// An fts3/fts4 table's rows live in its %_content SHADOW table, not under
	// a rootpage of its own -- see vtab_fts3.go's materializeFts3. A
	// "content=" table's live somewhere else again, which is why the schema
	// (and the statement's own WHERE conjuncts) go with it (fts3_content.go).
	if fm, isFts3 := mod.(fts3Module); isFts3 && !it.TableFunc {
		sch, serr := fm.parseSchemaWith(modArgs, p.fts3Catalog())
		if serr != nil {
			return nil, nil, nil, serr
		}
		return p.materializeFts3Item(it, it.Table, cols, sch)
	}

	constraints, rhsVals, err := p.buildVtabConstraints(it, cols, params, outer, trig, from, corr)
	if err != nil {
		return nil, nil, nil, err
	}
	info := &VtabIndexInfo{Constraints: constraints, Usage: make([]VtabConstraintUsage, len(constraints))}
	if err := tab.BestIndex(info); err != nil {
		return nil, nil, nil, err
	}
	maxArgv := 0
	for _, u := range info.Usage {
		if u.ArgvIndex > maxArgv {
			maxArgv = u.ArgvIndex
		}
	}
	argv := make([]Value, maxArgv)
	for i, u := range info.Usage {
		if u.ArgvIndex >= 1 && u.ArgvIndex <= maxArgv {
			argv[u.ArgvIndex-1] = rhsVals[i]
		}
	}

	// A module whose rows come from the DATABASE this pager reads, rather than
	// from the module alone, is served here instead of through a cursor: a
	// VtabCursor never sees a pager. The eponymous pragma_* table-valued
	// functions (vtab_pragma.go) are that case -- they run the wrapped PRAGMA
	// against p -- and they still go through BestIndex above, because their
	// hidden arg/schema inputs arrive as ordinary constraints.
	if pt, isPragma := tab.(pragmaVtabTable); isPragma {
		rows, rowids, perr := pt.pragmaVtabRows(p, info.IdxNum, argv)
		if perr != nil {
			return nil, nil, nil, perr
		}
		return cols, rows, rowids, nil
	}
	// fts4aux is the other one: it reads the TARGET fts3/fts4 table's term
	// index out of the same database (vtab_fts3aux.go).
	if at, isAux := tab.(fts3AuxTable); isAux {
		rows, rowids, aerr := at.fts3AuxRows(p, info.IdxNum, argv)
		if aerr != nil {
			return nil, nil, nil, aerr
		}
		return cols, rows, rowids, nil
	}
	// fts5vocab is fts4aux's fts5 counterpart: it reads the TARGET fts5
	// table's %_content and re-tokenizes it (vtab_fts5vocab.go).
	if vt, isVocab := tab.(fts5VocabTable); isVocab {
		rows, rowids, verr := vt.fts5VocabRows(p, info.IdxNum, argv)
		if verr != nil {
			return nil, nil, nil, verr
		}
		return cols, rows, rowids, nil
	}

	cur, err := tab.Open()
	if err != nil {
		return nil, nil, nil, err
	}
	defer cur.Close()
	if ea, ok := cur.(vtabEncodingAware); ok {
		ea.setTextEncoding(p.encoding())
	}
	if err := cur.Filter(info.IdxNum, info.IdxStr, argv); err != nil {
		return nil, nil, nil, err
	}
	var rows [][]Value
	var rowids []int64
	for {
		eof := cur.Eof()
		if eof {
			break
		}
		row := make([]Value, len(cols))
		for i := range cols {
			v, cerr := cur.Column(i)
			if cerr != nil {
				return nil, nil, nil, cerr
			}
			row[i] = v
		}
		rid, rerr := cur.Rowid()
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		rows = append(rows, row)
		rowids = append(rowids, rid)
		if nerr := cur.Next(); nerr != nil {
			return nil, nil, nil, nerr
		}
	}
	return cols, rows, rowids, nil
}

// materializeWritableVtab reads a writable virtual table's rows from the real
// b-tree persisted under its own rootpage (keyed by table name), presenting
// each as its full declared columns [id(=rowid), coord0, ...]. The stored
// record's first slot is NULL (the id is carried by the rowid key, matching an
// ordinary INTEGER-PRIMARY-KEY table), so it is filled in from the rowid here.
func (p *ReadOnlyPager) materializeWritableVtab(name string, cols []columnInfo) ([]columnInfo, [][]Value, []int64, error) {
	rowids, records, err := p.Rows(name)
	if err != nil {
		return nil, nil, nil, err
	}
	rows := make([][]Value, len(records))
	outRowids := make([]int64, len(records))
	for i := range records {
		row := make([]Value, len(cols))
		row[0] = Value{Typ: Int, I: int64(rowids[i])}
		for j := 1; j < len(cols) && j < len(records[i]); j++ {
			row[j] = records[i][j]
		}
		rows[i] = row
		outRowids[i] = int64(rowids[i])
	}
	return cols, rows, outRowids, nil
}

// resolveVtabModuleForItem finds the module and its CREATE arguments for FROM
// item it: a table-valued function call resolves the module by it.Table (no
// CREATE args); a persisted CREATE VIRTUAL TABLE resolves the module named in
// its stored USING clause (with its stored args); a bare name falls back to an
// eponymous module. It errors "no such table" when none matches.
func (p *ReadOnlyPager) resolveVtabModuleForItem(it FromItem) (VtabModule, []string, error) {
	if it.TableFunc {
		m, ok := lookupVtabModule(it.Table)
		if !ok {
			return nil, nil, fmt.Errorf("engine: no such table-valued function: %s()", it.Table)
		}
		return m, nil, nil
	}
	moduleName, args, ok, err := p.createdVtabDef(it.Table)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		m, found := lookupVtabModule(moduleName)
		if !found {
			return nil, nil, fmt.Errorf("engine: no such module: %s", moduleName)
		}
		return m, args, nil
	}
	if m, found := lookupVtabModule(it.Table); found {
		return m, nil, nil
	}
	return nil, nil, fmt.Errorf("engine: no such table: %s", it.Table)
}

// errVtabArgNotConstant tags the one buildVtabConstraints failure that is a
// missing ROW CONTEXT rather than a bad statement: a table-valued function's
// call argument that could not be evaluated where it was reached. resolveFrom
// (join.go) retries such an item as a columns-only resolution, because it has
// no row context to offer and does not need one -- see the branch there.
var errVtabArgNotConstant = errors.New("engine: table-valued function argument needs a row context")

// buildVtabConstraints builds the candidate constraints offered to BestIndex,
// with their right-hand-side values: the call arguments bound to the module's
// hidden columns, then every pushed WHERE conjunct "col <op> rhs" on this
// table's columns. An unevaluable call argument is an error (TVF arguments may
// not reference the surrounding query); an unevaluable WHERE right-hand side is
// offered unusable.
//
// Each right-hand side is compiled and run (foldVtabInputValue), as C codes it
// into a register (codeExprOrVector, wherecode.c:1584) after turning TVF
// arguments into "hidden-column = TK_UPLUS(arg)" terms
// (sqlite3WhereTabFuncArgs, whereexpr.c:1902).
//
// Unusable, not dropped: "FROM t, json_each e WHERE e.json = t.j" offers the
// term with Usable false, as C does (where.c:4389-4390). Dropping it gave
// json_each no input and returned zero rows. A module that needs the input
// rejects the plan (json.c:5493; series.c:867; pragma.c:2904); C then tries
// another loop order, but this engine materializes the vtab once, standalone
// (runVtabOnce), so it declines. Modules that do not need it (fts4aux,
// fts5vocab: fts3_aux.c:173, fts5_vocab.c:284) scan and the WHERE applies.
//
// outer is the enclosing trigger row, the one thing an argument may reference
// ("json_each(NEW.x)" in a body, attach.test 5.10), coded against OLD/NEW as C
// codes any body expression.
//
// corr removes one unusable case: a right-hand side naming a table bound
// earlier in the join is coded per outer row in C (wherecode.c:1584) with
// xFilter re-run. When the compiler arranged that (vtab_correlated.go), the
// value arrives computed and the constraint is usable (where.c:4390).
func (p *ReadOnlyPager) buildVtabConstraints(it FromItem, cols []columnInfo, params []Value, outer *evalCtx, trig *trigCompileCtx, from *vdbe, corr []vtabCorrValue) ([]VtabConstraint, []Value, error) {
	var cs []VtabConstraint
	var vals []Value

	hidden := hiddenColIndices(cols)
	for ai, argExpr := range it.TableFuncArgs {
		if ai >= len(hidden) {
			return nil, nil, fmt.Errorf("engine: %s(): too many arguments (module declares %d hidden input columns)", it.Table, len(hidden))
		}
		v, ok := vtabCorrArgFor(corr, ai, hidden[ai])
		if !ok {
			var err error
			if v, err = p.foldVtabInputValue(argExpr, params, outer, trig, from); err != nil {
				return nil, nil, fmt.Errorf("%w: engine: %s(): argument %d is not a constant: %w", errVtabArgNotConstant, it.Table, ai+1, err)
			}
		}
		// Omitted: an argument is not a WHERE conjunct here, so nothing
		// re-tests the module's rows against it -- the state C reaches by
		// honouring omit=1 on the "hidden = arg" term it synthesizes.
		cs = append(cs, VtabConstraint{Column: hidden[ai], Op: VtabEQ, Usable: true, Omitted: true})
		vals = append(vals, v)
	}

	name := it.Alias
	if name == "" {
		name = it.Table
	}
	for ci2, conj := range it.tvfWhere {
		ci, op, rhs, ok := matchVtabConstraint(conj, cols, name)
		if !ok {
			continue
		}
		// The conjunct the scan compiler actually DROPPED from the residual WHERE
		// for this item, if any -- FromItem.tvfOmitPlus1, stamped only for the
		// sources whose WHERE was really rewritten. See VtabConstraint.Omitted.
		omitted := it.tvfOmitPlus1 != 0 && it.tvfOmitPlus1-1 == ci2
		if cv, ok := vtabCorrValueFor(corr, ci2, ci, op); ok {
			// Already computed off the outer row, in a register, before this
			// level's loop started -- see vtab_correlated.go. The (column,
			// operator) pair had to match the one re-derived here, so this
			// value belongs to exactly this constraint.
			cs = append(cs, VtabConstraint{Column: ci, Op: op, Usable: true, Omitted: omitted})
			vals = append(vals, cv)
			continue
		}
		v, err := p.foldVtabInputValue(rhs, params, outer, trig, from)
		if err != nil {
			// Offered UNUSABLE with a placeholder value, keeping the two
			// slices parallel. BestIndex never assigns an unusable constraint
			// an ArgvIndex, so the placeholder is never read.
			cs = append(cs, VtabConstraint{Column: ci, Op: op, Usable: false, Omitted: omitted})
			vals = append(vals, Value{})
			continue
		}
		cs = append(cs, VtabConstraint{Column: ci, Op: op, Usable: true, Omitted: omitted})
		vals = append(vals, v)
	}
	return cs, vals, nil
}

// foldVtabInputValue computes one constraint right-hand side by compiling it as
// a one-column FROM-less Program and running it once against the bound
// parameters (as foldLimitOffsetExpr does). rowOuter is the live trigger row,
// so "json_each(NEW.x)" resolves through compiler.rowOuter.
//
// trig/from are the compiled route: trig makes NEW./OLD. resolve through
// resolveTriggerParam as an OpParam read (keeping declared affinity and
// collation, compiler.affCtx's trigPseudoRowScopes), and from is the machine
// whose trigNew/trigOld that read uses (execTrig). nil off the trigger path. C
// codes the rewritten "hidden-column = <arg>" term (whereexpr.c:1902) with
// sqlite3ExprCodeTarget (expr.c:4950) inside the trigger sub-program, where
// lookupName binds NEW./OLD. (resolve.c:525-543).
//
// A reference to another FROM item's column fails to compile here, which the
// callers treat as "not a constant".
func (p *ReadOnlyPager) foldVtabInputValue(e Expr, params []Value, rowOuter *evalCtx, trig *trigCompileCtx, from *vdbe) (Value, error) {
	prog, err := compileSelectNoFromTrig(p, &SelectStmt{Columns: []SelectColumn{{Expr: e}}}, nil, trig, rowOuter, nil)
	if err != nil {
		return Value{}, err
	}
	rows, err := prog.execTrig(p, params, from)
	if err != nil {
		return Value{}, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return Value{}, fmt.Errorf("%w: virtual-table constraint expression did not yield one value", errVDBEUnsupported)
	}
	return rows[0][0], nil
}

// hiddenColIndices returns the indices of cols that are Hidden, in declaration
// order -- the positional target of a table-valued function's call arguments.
func hiddenColIndices(cols []columnInfo) []int {
	var out []int
	for i, c := range cols {
		if c.Hidden {
			out = append(out, i)
		}
	}
	return out
}

// matchVtabConstraint recognizes a top-level WHERE conjunct of the form
// "col <op> rhs" (or "rhs <op> col") where col names one of cols (unqualified,
// or qualified with scopeName). It returns the matched column index, the
// operator oriented so col is on the left, and the rhs expression.
func matchVtabConstraint(conj Expr, cols []columnInfo, scopeName string) (int, VtabOp, Expr, bool) {
	be, ok := conj.(BinaryExpr)
	if !ok {
		return 0, 0, nil, false
	}
	op, ok := binaryToVtabOp(be.Op)
	if !ok {
		return 0, 0, nil, false
	}
	if ci, ok := vtabColMatch(be.L, cols, scopeName); ok {
		return ci, op, be.R, true
	}
	if ci, ok := vtabColMatch(be.R, cols, scopeName); ok {
		return ci, flipVtabOp(op), be.L, true
	}
	return 0, 0, nil, false
}

// vtabColMatch returns the index of the cols entry e refers to, if e is a
// ColumnExpr naming one of cols (unqualified, or qualified with scopeName).
func vtabColMatch(e Expr, cols []columnInfo, scopeName string) (int, bool) {
	ce, ok := e.(ColumnExpr)
	if !ok {
		return 0, false
	}
	if ce.Qualifier != "" && !equalFoldName(ce.Qualifier, scopeName) {
		return 0, false
	}
	for i, c := range cols {
		if equalFoldName(c.Name, ce.Name) {
			return i, true
		}
	}
	return 0, false
}

func binaryToVtabOp(op string) (VtabOp, bool) {
	switch op {
	case "=", "==":
		return VtabEQ, true
	case "<":
		return VtabLT, true
	case "<=":
		return VtabLE, true
	case ">":
		return VtabGT, true
	case ">=":
		return VtabGE, true
	}
	return 0, false
}

func flipVtabOp(op VtabOp) VtabOp {
	switch op {
	case VtabLT:
		return VtabGT
	case VtabLE:
		return VtabGE
	case VtabGT:
		return VtabLT
	case VtabGE:
		return VtabLE
	}
	return op // EQ is symmetric
}

// ---- CREATE VIRTUAL TABLE / DROP persistence ----

// vtabMeta is one virtual table registered this write session (via
// CreateVirtualTable) or recovered from an existing file (schema_load_objects.go). sql
// is the verbatim CREATE VIRTUAL TABLE text stored in sqlite_schema at Close
// (rootpage 0, exactly like a view). module and args are parsed out of that
// text for validation and for the read path (createdVtabDef re-parses the
// stored text rather than reading these).
type vtabMeta struct {
	// schemaSeq is this object's CREATION rank within its database: the order
	// its sqlite_schema row must appear in, which C SQLite gives by simply
	// appending each new object's row to the catalog b-tree. materialize
	// (writer.go) sorts every row it emits by this, so a schema this engine
	// rebuilds from scratch on every flush still reports its objects in the
	// order they were created rather than grouped by kind. Assigned by
	// DB.nextSchemaSeq at CREATE time, recovered from the row's own position at
	// OpenWrite, and carried unchanged through ALTER TABLE (which C SQLite
	// likewise applies in place, leaving the row's rowid alone).
	schemaSeq uint64

	name   string
	sql    string
	module string
	args   []string

	// isTemp is whether this vtab's own sqlite_schema row belongs to the
	// TEMP catalog rather than main (temp_schema.go) -- true for a
	// "CREATE VIRTUAL TABLE temp.x USING ..." (currently only ever set for
	// the fts3/fts4/fts4aux/fts3tokenize family; see CreateVirtualTable).
	// sql already carries the same fact as this engine's on-disk marker
	// (withTempKeywordIf), so this field is the in-session-only mirror of
	// it -- cheaper to consult than re-lexing sql everywhere isTemp is
	// needed (hasTempObject, DropTempObjects).
	isTemp bool

	// store is the live in-session backing store of a WRITABLE virtual table
	// (rtree -- see vtab_write.go / vtab_rtree.go), nil for a read-only module
	// (generate_series). When non-nil its rows are materialized into a real
	// table b-tree under this vtab's own sqlite_schema rootpage at Close
	// (writer.go) and recovered from it at OpenWrite (schema_load_objects.go), so a
	// writable vtab's data is persisted through the file exactly like an
	// ordinary table's; the read path (materializeVtab) reads that b-tree, and
	// the write path (insert_write.go/write_update_delete.go) routes DML through
	// this store's VtabUpdater.
	store vtabStore

	// loadErr is why store could not be read back from its shadow tables when
	// this session opened -- a damaged %_node blob, say. It fails a write that
	// reaches this table and nothing else: C SQLite reads an r-tree's or an
	// fts5 table's shadow tables only when a statement uses it (rtree.c's
	// nodeAcquire, fts5's own index reads), so one damaged table there leaves
	// every other write to the database working.
	loadErr error

	// rootPage is the routing root a store's rows are keyed by: recovered from
	// the catalog at open, and handed out by nextSegmentRoot for a table
	// created this session.
	rootPage uint32
}

// findVtabMeta returns the vtabMeta for name (case-insensitive), or nil.
func (db *DB) findVtabMeta(name string) *vtabMeta {
	for _, v := range db.vtabs {
		if equalFoldName(v.name, name) {
			return v
		}
	}
	return nil
}

// CreateVirtualTable executes a "CREATE VIRTUAL TABLE [IF NOT EXISTS] name
// USING module[(args)]" statement: it validates that module is registered and
// that Connect accepts its arguments, then records the vtab so Close persists
// its sqlite_schema row (type "table", rootpage 0, verbatim SQL). The read
// path re-parses that stored row (createdVtabDef) and re-Connects the module,
// so a created vtab is queryable after the write is flushed.
func (db *DB) CreateVirtualTable(sqlText string) error {
	// See schema_write.go's CreateTable's identical comment: any DDL
	// disqualifies the incremental commit path.
	name, module, args, ifNotExists, isTemp, unqualifiedSQL, err := parseCreateVirtualTableStmtScoped(sqlText)
	if err != nil {
		return err
	}
	// The "sqlite_" prefix is reserved for a virtual table exactly as it is
	// for an ordinary one (C SQLite runs both through sqlite3StartTable):
	// "CREATE VIRTUAL TABLE sqlite_stat1 USING fts5(a)" is "object name
	// reserved for internal use: sqlite_stat1" there, and used to be ACCEPTED
	// here -- leaving a table C SQLite does not have, so every later
	// statement naming it diverged too (vtabK.test, found by the mined fts5
	// corpus). Checked before the already-exists tests below, which is the
	// order sqlite3StartTable applies them in.
	if err := db.checkReservedObjectName(name); err != nil {
		return err
	}
	// A new object collides only with its OWN catalog's objects -- exactly
	// createTable's rule (schema_write.go) via the same createScope helper
	// (temp_schema.go): "CREATE VIRTUAL TABLE temp.t USING fts3(a)" must not
	// be blocked by an unrelated main table also named t, and vice versa.
	scope := createScope(isTemp)
	if db.findVtabMetaIn(scope, name) != nil || db.findTableMetaIn(scope, name) != nil || db.findViewMetaIn(scope, name) != nil {
		if ifNotExists {
			return nil
		}
		return fmt.Errorf("engine: table %s already exists", name)
	}
	if db.findIndexMetaIn(scope, name) != nil {
		return fmt.Errorf("engine: there is already an index named %s", name)
	}
	mod, ok := lookupVtabModule(module)
	if !ok || eponymousOnlyVtabModule(module) {
		return fmt.Errorf("engine: no such module: %s", module)
	}
	// A TEMP virtual table needs no per-module permission: every module's
	// storage is addressed by the object's isTemp, so its bytes land in
	// the TEMP file.
	// fts4aux's two-argument form (naming the target's database) is
	// accepted by C only when fts4aux itself is created in TEMP:
	// "fts4aux(main, t)", "fts4aux(temp, t)" and "fts4aux(nosuchdb, t)"
	// all fail "invalid arguments to fts4aux constructor" when created
	// bare or in main, and all succeed (resolving at SELECT) in temp.
	// Checked here because fts3AuxModule.Connect has no schema context and
	// reruns on every read (vtab_fts3aux.go has the read-time half).
	if equalFoldName(module, "fts4aux") && len(args) == 2 && !isTemp {
		return fmt.Errorf("engine: CREATE VIRTUAL TABLE %s: fts4aux: invalid arguments to fts4aux constructor", name)
	}
	// fts5vocab's three-argument "fts5vocab(db, table, type)" form is legal
	// only when fts5vocab is created in TEMP: fts5_vocab.c:190 sets bDb
	// only for argv[1]=="temp" (the vtab's own database), and :192 fails
	// any other argc but 5 unless bDb ("wrong number of vtable arguments").
	// Checked here for fts4aux's reason (fts5VocabResolvePager has the
	// read-time half).
	if equalFoldName(module, "fts5vocab") && len(args) == 3 && !isTemp {
		return fmt.Errorf("engine: fts5vocab: wrong number of vtable arguments")
	}
	// A writable module (rtree) builds a live backing store now, validating its
	// arguments (column count/shape) in the process; a read-only module
	// (generate_series) is validated by Connect and keeps store nil.
	var store vtabStore
	if wm, isWritable := mod.(writableVtabModule); isWritable {
		st, cerr := wm.newWritableStore(name, args)
		if cerr != nil {
			return fmt.Errorf("engine: CREATE VIRTUAL TABLE %s: %w", name, cerr)
		}
		store = st
	} else if _, _, cerr := vtabConnect(db.fts3Catalog(), mod, args); cerr != nil {
		return fmt.Errorf("engine: CREATE VIRTUAL TABLE %s: %w", name, cerr)
	}
	// The virtual table's own creation rank is taken NOW, before any shadow
	// table exists: C SQLite writes the "CREATE VIRTUAL TABLE" row FIRST and
	// each shadow's row after it (fts4 reports ft, ft_content, ft_segments,
	// ft_segdir, sqlite_autoindex_ft_segdir_1, ft_docsize, ft_stat -- verified
	// against mattn/go-sqlite3), where this engine creates the shadows first and
	// registers the vtabMeta last. See DB.nextSchemaSeq.
	vtSeq := db.nextSchemaSeq(isTemp)
	// An fts3/fts4 table keeps its data in ORDINARY shadow tables alongside
	// itself rather than under a rootpage of its own -- see vtab_fts3.go. They
	// must exist before the vtabMeta is registered, so a failure here leaves
	// no half-created table behind. Each shadow belongs to the SAME catalog as
	// the fts3/fts4 table itself (createShadowTables' own isTemp comment).
	if fm, isFts3 := mod.(fts3Module); isFts3 {
		// An fts3/fts4 table's read path resolves its shadow tables by name
		// (p.Rows), so an identically named table in both catalogs would make
		// that lookup ambiguous, while C keeps main.t1 and temp.t1 fully
		// independent. Supporting it needs the read path to carry a resolved
		// catalog, so the colliding CREATE declines.
		other := scopeMain
		if scope == scopeMain {
			other = scopeTemp
		}
		if ov := db.findVtabMetaIn(other, name); ov != nil {
			if _, otherIsFts3 := fts3ModuleOf(ov); otherIsFts3 {
				return fmt.Errorf("engine: unsupported: CREATE VIRTUAL TABLE %s USING %s while a same-named fts3/fts4 table already exists in the other catalog (this engine's shadow-table read path cannot disambiguate %s_content/_segments/_segdir by name alone)", name, module, name)
			}
		}
		sch, cerr := fm.parseSchemaWith(args, db.fts3Catalog())
		if cerr != nil {
			return fmt.Errorf("engine: CREATE VIRTUAL TABLE %s: %w", name, cerr)
		}
		if cerr := fm.createShadowTables(db, name, sch, isTemp); cerr != nil {
			return fmt.Errorf("engine: CREATE VIRTUAL TABLE %s: %w", name, cerr)
		}
	}
	// An fts5 table keeps a store (its rows and the SQL-level semantics) but
	// persists it into the same shadow tables C fts5 uses, rather than
	// under a rootpage of its own -- see fts5_shadow.go.
	if fm, isFts5 := mod.(fts5Module); isFts5 {
		// The identical cross-catalog collision guard as fts3/fts4's just
		// above, and for the same reason: fts5's own READ path
		// (fts5SchemaTok, fts5_match.go) resolves an fts5 table's schema row
		// -- and its "_data"/"_idx"/"_content"/"_docsize"/"_config" shadows
		// (db.findTableMeta in fts5_shadow.go's fts5SyncShadows and
		// elsewhere) -- BY NAME, unscoped by catalog. A same-named fts5 table
		// in both main and temp would make every one of those lookups
		// ambiguous, so this declines the colliding CREATE outright rather
		// than risk the silent wrong answer an unscoped read would otherwise
		// produce.
		other := scopeMain
		if scope == scopeMain {
			other = scopeTemp
		}
		if ov := db.findVtabMetaIn(other, name); ov != nil {
			if _, otherIsFts5 := fts5ModuleOf(ov); otherIsFts5 {
				return fmt.Errorf("engine: unsupported: CREATE VIRTUAL TABLE %s USING %s while a same-named fts5 table already exists in the other catalog (this engine's shadow-table read path cannot disambiguate %s_data/_idx/_content/_docsize/_config by name alone)", name, module, name)
			}
		}
		if cerr := fm.createShadowTables(db, name, store.(*fts5Store), isTemp); cerr != nil {
			return fmt.Errorf("engine: CREATE VIRTUAL TABLE %s: %w", name, cerr)
		}
	}
	// An r-tree persists into the SAME three shadow tables C SQLite's own
	// module creates, rather than under a rootpage of its own -- see
	// rtree_shadow.go for why that is what makes such a database readable by
	// C SQLite at all.
	if rm, isRtree := mod.(rtreeModule); isRtree {
		if cerr := rm.createShadowTables(db, name, store.(*rtreeStore), isTemp); cerr != nil {
			return fmt.Errorf("engine: CREATE VIRTUAL TABLE %s: %w", name, cerr)
		}
	}
	// A successful create moves C's connection counters for some modules
	// (an rtree xCreate leaves last_insert_rowid()==1 and total_changes()
	// one higher), which this engine does not reproduce, so both counters
	// become unanswerable for the session (markConnStateOpaque). Marked only
	// on success: a failed create ("no such module: echo", rtree "Too few
	// columns", "table dup already exists") moves nothing in C.
	//
	// The fts3 family moves neither counter (its xCreate writes only DDL),
	// so it is not marked; fts3e.test's "INSERT INTO t1(docid,c)
	// VALUES(last_insert_rowid(),...)" depends on that.
	if !fts3FamilyModule(module) {
		db.markConnStateOpaque()
	}
	// sql is stored the same way storedSchemaSQL renders a TABLE/VIEW/TRIGGER:
	// qualifier stripped (unqualifiedSQL, matching what C SQLite itself
	// keeps) and, for a TEMP object, this engine's own on-disk TEMP marker
	// added back on -- the fact markTempSchemaRows/isCreateVirtualTableSQL
	// recover the catalog from at the next open, and temp_catalog.go's
	// withoutTempKeyword strips back off for an sqlite_temp_master read.
	db.vtabs = append(db.vtabs, &vtabMeta{schemaSeq: vtSeq, name: name, sql: withTempKeywordIf(unqualifiedSQL, isTemp), module: module, args: args, store: store, isTemp: isTemp})
	db.bumpSchema(false)
	if _, isRtree := mod.(rtreeModule); isRtree && !isTemp {
		if db.rtreeConns == nil {
			db.rtreeConns = NewRtreeConnections()
		}
		db.rtreeConns.noteCreated("main", name, db.schemaCookie)
	}
	return nil
}

// removeVtab removes v from db.vtabs, mirroring removeView (view.go): DropTable
// calls this once it finds v, and Close's materialize then never emits its
// sqlite_schema row again. A virtual table has no b-tree of its own (rootpage
// 0), so there is nothing else to free.
func (db *DB) removeVtab(v *vtabMeta) {
	for i, x := range db.vtabs {
		if x == v {
			db.vtabs = append(db.vtabs[:i], db.vtabs[i+1:]...)
			break
		}
	}
	db.bumpSchema(false)
}

// createdVtabDef reads the live schema and, if name is a persisted CREATE
// VIRTUAL TABLE, returns its module name and USING arguments. ok is false (nil
// error) when name is not a virtual table (an ordinary table, view, or absent).
func (p *ReadOnlyPager) createdVtabDef(name string) (module string, args []string, ok bool, err error) {
	return p.createdVtabDefIn(scopeAny, name)
}

// createdVtabDefIn is createdVtabDef restricted to one catalog, so a
// "temp."-qualified name cannot match a main-catalog virtual table.
func (p *ReadOnlyPager) createdVtabDefIn(scope schemaScope, name string) (module string, args []string, ok bool, err error) {
	rows, serr := p.Schema()
	if serr != nil {
		return "", nil, false, serr
	}
	for i := range rows {
		if rows[i].Type == "table" && equalFoldName(rows[i].Name, name) && scope.accepts(rows[i].Temp) {
			if !isCreateVirtualTableSQL(rows[i].SQL) {
				return "", nil, false, nil
			}
			_, mod, a, _, perr := parseCreateVirtualTableStmt(rows[i].SQL)
			if perr != nil {
				return "", nil, false, perr
			}
			return mod, a, true, nil
		}
	}
	return "", nil, false, nil
}

// isCreateVirtualTableStmt reports whether a lexed statement begins
// "CREATE VIRTUAL TABLE".
func isCreateVirtualTableStmt(toks []token) bool {
	return len(toks) >= 3 &&
		toks[0].kind == tkIdent && toks[0].upper() == "CREATE" &&
		toks[1].kind == tkIdent && toks[1].upper() == "VIRTUAL" &&
		toks[2].kind == tkIdent && toks[2].upper() == "TABLE"
}

// isCreateVirtualTableSQL reports whether sqlText (a stored schema row's SQL) is
// a CREATE VIRTUAL TABLE; a lex failure reports false. A TEMP vtab's stored text
// carries this engine's marker "CREATE TEMP VIRTUAL TABLE ..."
// (withTempKeywordIf), which C has no grammar for (a syntax error there), so a
// leading TEMP here can only be the marker, and is stripped.
func isCreateVirtualTableSQLUncached(sqlText string) bool {
	if isTempCreateSQL(sqlText) {
		sqlText = withoutTempKeyword(sqlText)
	}
	toks, err := lex(sqlText)
	if err != nil {
		return false
	}
	return isCreateVirtualTableStmt(toks)
}

// parseCreateVirtualTableStmt parses
// "CREATE VIRTUAL TABLE [IF NOT EXISTS] [schema.]name USING module [(arg, ...)]".
// The module arguments are captured as raw, comma-separated, trimmed strings
// between the top-level parentheses (SQLite passes module arguments verbatim to
// xConnect/xCreate), or nil when the "(...)" is omitted.
//
// A thin wrapper around parseCreateVirtualTableStmtScoped, whose extra return
// values (the schema-scope this statement's own qualifier named, and the
// qualifier-stripped SQL) only CreateVirtualTable itself needs; every other
// caller here just wants the module and its arguments back out of a schema
// row it already resolved by some other means.
func parseCreateVirtualTableStmt(sqlText string) (name, module string, args []string, ifNotExists bool, err error) {
	name, module, args, ifNotExists, _, _, err = parseCreateVirtualTableStmtScoped(sqlText)
	return
}

// parseCreateVirtualTableStmtScoped is parseCreateVirtualTableStmt plus:
//
//   - isTemp: the statement names the TEMP catalog ("temp."-qualified, or this
//     engine's stored TEMP marker);
//   - unqualifiedSQL: sqlText without "[schema.]", which is what C stores in
//     sqlite_schema for either catalog ("CREATE VIRTUAL TABLE temp.x USING
//     m(a)" reads back "CREATE VIRTUAL TABLE x USING m(a)").
//     CreateVirtualTable re-marks TEMP (withTempKeywordIf), as storedSchemaSQL
//     does for TABLE/VIEW/TRIGGER.
//
// Only "main" and "temp" qualifiers are accepted; an attachment is an "unknown
// database" decline (the driver routes such statements to that database's
// session and strips the qualifier, StripDDLTargetSchema). The leading TEMP
// marker is accepted because it can only come from a stored row: fresh user
// text must begin "CREATE VIRTUAL TABLE" to reach here
// (isCreateVirtualTableStmt).
func parseCreateVirtualTableStmtScoped(sqlText string) (name, module string, args []string, ifNotExists, isTemp bool, unqualifiedSQL string, err error) {
	if isTempCreateSQL(sqlText) {
		isTemp = true
		sqlText = withoutTempKeyword(sqlText)
	}
	unqualifiedSQL = sqlText
	toks, lexErr := lex(sqlText)
	if lexErr != nil {
		return "", "", nil, false, false, "", lexErr
	}
	// The stored text starts at the CREATE token, not at the first byte the
	// caller handed over: C SQLite records the statement from
	// pParse->sNameToken's own span, so a leading comment or blank line is not
	// part of it ("-- lead\nCREATE VIRTUAL TABLE v USING fts5(o)" stores
	// "CREATE VIRTUAL TABLE v USING fts5(o)" -- verified against 3.53.3, and
	// the ordinary CREATE TABLE path here already agreed). Keeping it made
	// sqlite_schema.sql differ from C's, and -- once the schema load started
	// applying sqlite3InitCallback's own rules (schema_reload_image.go) -- a
	// row whose text does not begin "CR" is a row the load REFUSES, so the
	// whole database read as malformed.
	lead := 0
	if len(toks) > 0 {
		lead = toks[0].Start
	}
	p := newParser(sqlText, toks)
	if !p.consumeKeyword("CREATE") || !p.consumeKeyword("VIRTUAL") || !p.consumeKeyword("TABLE") {
		return "", "", nil, false, false, "", fmt.Errorf("engine: expected CREATE VIRTUAL TABLE, got %q", sqlText)
	}
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("NOT") || !p.consumeKeyword("EXISTS") {
			return "", "", nil, false, false, "", fmt.Errorf("engine: CREATE VIRTUAL TABLE: expected NOT EXISTS after IF")
		}
		ifNotExists = true
	}
	// "createkw VIRTUAL TABLE ifnotexists nm dbnm USING nm" (parse.y): both
	// names are nm, which takes a string literal too (parse.y:339-340).
	nt := p.next()
	first, ok := objectNameToken(nt)
	if !ok {
		return "", "", nil, false, false, "", fmt.Errorf("engine: CREATE VIRTUAL TABLE: expected table name, got %q", p.tokenDesc(nt))
	}
	name = first
	if p.peekIsPunct(".") { // [schema.]name -- keep only the bare name
		p.next()
		nt2 := p.next()
		second, ok := objectNameToken(nt2)
		if !ok {
			return "", "", nil, false, false, "", fmt.Errorf("engine: CREATE VIRTUAL TABLE: expected table name after schema qualifier, got %q", p.tokenDesc(nt2))
		}
		switch {
		case equalFoldName(first, "main"):
			// isTemp stays whatever the on-disk-marker check above already
			// decided (always false here: fresh user text naming "main."
			// never carries that marker).
		case equalFoldName(first, "temp"):
			isTemp = true
		default:
			return "", "", nil, false, false, "", fmt.Errorf("engine: CREATE VIRTUAL TABLE: unknown database %s (only main and temp are supported by this write path)", first)
		}
		name = second
		// Splice the "<schema>." prefix back out, leaving everything else --
		// spacing, quoting, the verbatim module-argument text -- byte-exact.
		// Mirrors StripDDLTargetSchema's identical splice for every OTHER DDL
		// kind (engine/attach.go); inlined here since this function already
		// holds the tokens.
		unqualifiedSQL = sqlText[lead:nt.Start] + sqlText[nt2.Start:]
	}
	if lead > 0 && unqualifiedSQL == sqlText {
		unqualifiedSQL = sqlText[lead:]
	}
	if !p.consumeKeyword("USING") {
		return "", "", nil, false, false, "", fmt.Errorf("engine: CREATE VIRTUAL TABLE: expected USING, got %q", p.tokenDesc(p.peek()))
	}
	mt := p.next()
	if mt.kind != tkIdent {
		return "", "", nil, false, false, "", fmt.Errorf("engine: CREATE VIRTUAL TABLE: expected module name, got %q", p.tokenDesc(mt))
	}
	module = mt.text
	if p.peekIsPunct("(") {
		// SQLite hands each module argument over as TEXT, but it cuts that text
		// by TOKEN: an argument runs from its first token to its last
		// (sqlite3VtabArgExtend, vtab.c:543-552), top-level commas separate
		// them, and an argument with no token at all is never added
		// (addArgumentToVtab, vtab.c:436-443). So a comment BETWEEN arguments
		// belongs to neither -- over "rtree( id, -- the key\n minX, ...)" the
		// second argument is "minX" -- while one INSIDE an argument stays in its
		// text. Cutting the raw text instead made that comment the start of
		// the next argument, and rtree read its column name as "--".
		p.next() // the "("
		depth := 1
		first, last := -1, -1
		flush := func() {
			if first >= 0 {
				args = append(args, sqlText[first:last])
			}
			first, last = -1, -1
		}
		for {
			t := p.next()
			if t.kind == tkEOF {
				return "", "", nil, false, false, "", fmt.Errorf("engine: CREATE VIRTUAL TABLE: unbalanced '(' in module arguments")
			}
			if t.kind == tkPunct && t.text == ")" {
				depth--
				if depth == 0 {
					flush()
					break
				}
			} else if t.kind == tkPunct && t.text == "(" {
				depth++
			} else if depth == 1 && t.kind == tkPunct && t.text == "," {
				flush()
				continue
			}
			if first < 0 {
				first = t.Start
			}
			last = t.End
		}
	}
	return name, module, args, ifNotExists, isTemp, unqualifiedSQL, nil
}
