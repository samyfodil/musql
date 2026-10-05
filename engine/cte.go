// WITH clauses (common table expressions). A FROM item naming a CTE resolves
// to the same derived-table row source a subquery or view uses (resolveFrom,
// view.go), never a separate execution path, except a recursive CTE, which is a
// queue program of its own (compileRecursiveCTE, vdbe_recursive_cte.go).
//
// Parsing is in sql_parser.go (parseWithClause); SelectStmt.CTEs and CTEDef
// (sql_ast.go) are the parsed shape. pushCTEScope/popCTEScope make a
// statement's CTEs visible to everything reached while it compiles or runs
// (ReadOnlyPager.cteScopes), and resolveCTERows is the row source for a
// non-recursive reference.
//
// Semantics, as in C:
//
//   - a CTE shadows a real table or view of the same name in its statement;
//   - CTEs in one WITH see each other in either order; only a real cycle
//     (outside the recognized recursive shape) is "circular reference: %s";
//   - a rename list of the wrong length is "table %s has %d values for %d
//     columns" (actual count first; unlike CREATE VIEW's wording);
//   - "no such table"/"no such column" inside a CTE body is not
//     "main."-qualified, unlike a stored view's (rewriteViewNoSuchTable).
package engine

import (
	"fmt"
)

// CTEMaterialization is one CTE's "AS [NOT] MATERIALIZED" hint -- SQLite's
// CteUse.eM10d. Only cteM10dYes is observable to this package: select.c:7797
// skips flattening (and ORDER BY elimination) for it outright, calling a
// MATERIALIZED CTE "an optimization fence". The other two behave alike for the
// flattener; they differ only in fromClauseTermCanBeCoroutine's rule (2b),
// which picks a coroutine over a materialized transient table and changes no
// answer. Exported only because CTEDef (sql_ast.go) carries it.
type CTEMaterialization uint8

const (
	cteM10dAny CTEMaterialization = iota // no keyword written (M10d_Any)
	cteM10dYes                           // "AS MATERIALIZED" (M10d_Yes)
	cteM10dNo                            // "AS NOT MATERIALIZED" (M10d_No)
)

// cteBinding is one CTE's resolved-enough-to-execute shape, built once (by
// newCTEBinding) when its WITH clause's scope frame is pushed, and reused
// for every FROM-item reference to it for as long as that frame stays on
// ReadOnlyPager.cteScopes.
type cteBinding struct {
	name     string
	colNames []string // explicit "(col, ...)" rename list; nil if none was given
	core     *SelectStmt

	// m10d is the "AS [NOT] MATERIALIZED" hint as written; cteM10dYes is what
	// stops the flattener (r38cExpandCTERef, flatten_limit_r35c.go).
	m10d CTEMaterialization

	// recursive is non-nil iff core matches the one recursive shape this
	// package recognizes (detectRecursiveShape) -- a compound whose TRAILING
	// arms, one or more of them and all joined by the same UNION or UNION ALL,
	// each reference this CTE's own name exactly once in their own top-level
	// FROM and nowhere else. nil means this CTE is resolved exactly like a
	// view: one execSelect over core as a whole.
	recursive *recursiveCTEShape

	// multiSelfRef is the ONE unrecognized recursive shape that has its own
	// error in C SQLite rather than a generic one: a recursive arm whose top
	// level FROM names this CTE MORE THAN ONCE. select.c:5786-5792 raises
	// "multiple references to recursive table: %s" the moment a second FROM item
	// matches while SF_Recursive is already set on that arm. Without this the
	// shape fell through to the generic circularity guard and reported
	// "circular reference", which is select.c:5802's separate zCteErr and the
	// wrong diagnosis.
	multiSelfRef bool

	// selfRef, when non-nil, is this RECURSIVE CTE's name bound to its own
	// current row, for the length of the compile of its recursive arms
	// (compileRecursiveCTE, vdbe_recursive_cte.go) -- SQLite binds the
	// recursive table to the iCurrent pseudo-cursor the same way
	// (select.c:2821-2825). It is compile-time only: resolveCTESource turns the
	// arm's FROM item into a one-row source over that row, and cteSchemaCols
	// answers the columns from it without re-entering the circularity guard.
	selfRef *recSelfRef

	// scopeDepth is how many WITH-clause frames were in force where this CTE
	// was DEFINED (its own frame included). A CTE's body is resolved in THAT
	// scope, not in the caller's: an inner WITH that redefines a name the body
	// references must not reach it. Verified directly (with3.test 2.0):
	//
	//	WITH x1 AS (SELECT 10), x2 AS (SELECT 11),
	//	     x3 AS (SELECT * FROM x1 UNION ALL SELECT * FROM x2),
	//	     x4 AS (WITH x1 AS (SELECT 12), x2 AS (SELECT 13) SELECT * FROM x3)
	//	SELECT * FROM x4;   -- 10, 11 (NOT 12, 13)
	scopeDepth int

	// frame is the scope frame this binding was created in (the very map it
	// is a member of), so resolveCTERows can put it BACK in scope when it is
	// no longer there. That happens for a subquery's own WITH clause: it is
	// pushed for the duration of that subquery's COMPILE (compileSubProgram,
	// vdbe_codegen.go), which is when a CTE FROM item is bound, but a
	// non-recursive CTE body -- including one that names a SIBLING from the
	// same clause -- runs LATER, out of the compiled program (runCTEOnce), by
	// which time that frame has been popped. When recursive CTEs still ran that
	// way too, the recursive arm's own self-reference resolved to nothing:
	// "y IN (WITH ss(x) AS (VALUES(7) UNION ALL SELECT x+7 FROM ss WHERE
	// x<49) SELECT x FROM ss)" failed with "no such table: ss".
	frame map[string]*cteBinding
}

// recursiveCTEShape is a CTE's body decomposed into its "initial" part
// (every arm before the first recognized recursive one, combined exactly as
// written -- itself a normal, possibly-compound SelectStmt with no
// self-reference) and its "recursive" arms (the CTE's own trailing compound
// arms, each referencing the CTE exactly once in its own top-level FROM) --
// see detectRecursiveShape, which builds this, and compileRecursiveCTE
// (vdbe_recursive_cte.go), which compiles the queue program the two together
// define.
type recursiveCTEShape struct {
	initial *SelectStmt
	// recursiveArms is every self-referencing arm, in written order. SQLite
	// allows MORE THAN ONE ("... UNION <rec1> UNION <rec2>", with5.test's
	// bidirectional "closure" walks): each popped row is expanded by every arm
	// in turn, and what they produce is appended to the one shared queue.
	recursiveArms []*SelectStmt
	unionAll      bool // true for "... UNION ALL <arm>"; false for "... UNION <arm>" (dedup against the whole accumulated set)

	// orderCols/orderTerms describe an ORDER BY inside the CTE, which C uses as
	// the recursion's queue discipline: FIFO (breadth-first) by default, while
	// "ORDER BY <expr> DESC" expands the largest pending row first, turning a
	// graph walk depth-first (with1.test's edge table: 0..9 plain, 0,3,7,6,2,5,9,
	// 1,4,8 with "ORDER BY 2 DESC"). orderCols holds each term's output-column
	// index (ordinals and bare CTE column names only; detectRecursiveShape
	// declines anything else), orderColl each term's explicit COLLATE or ""
	// (recursiveOrderColumn).
	orderCols  []int
	orderTerms []OrderTerm
	orderColl  []string

	// limit/offset are a LIMIT written inside the CTE, which bounds the
	// RECURSION's own output. Verified directly: the 10-row walk above
	// becomes 0,1,2,3 under "LIMIT 4" and 2,3,4,5 under "LIMIT 4 OFFSET 2"
	// -- OFFSET skips output rows while expansion continues, which is why 4
	// and 5 still appear. nil when none was written.
	limit  *int64
	offset *int64
}

// newCTEBinding builds def's cteBinding, precomputing its recursive shape
// (if any) once so every FROM-item reference to it doesn't re-analyze the
// same AST repeatedly.
func newCTEBinding(def CTEDef, scopeDepth int) *cteBinding {
	return &cteBinding{
		name:       def.Name,
		colNames:   def.ColNames,
		core:       def.Select,
		m10d:       def.Materialized,
		recursive:  detectRecursiveShape(def.Name, def.Select),
		multiSelfRef: recursiveArmHasMultipleSelfRefs(def.Name, def.Select),
		scopeDepth: scopeDepth,
	}
}

// cteFrameVisible reports whether b is the binding name currently resolves to
// -- i.e. whether b's own scope frame is still on p.cteScopes. See
// cteBinding.frame.
func (p *ReadOnlyPager) cteFrameVisible(name string, b *cteBinding) bool {
	cur, ok := p.lookupCTE(name)
	return ok && cur == b
}

// enterCTEBodyScope switches p.cteScopes to the scope b's body resolves in and
// returns the restore func:
//
//   - the body resolves where the CTE was defined: later frames are truncated,
//     so an inner WITH redefining a name cannot reach it (cteBinding.scopeDepth,
//     with3.test);
//   - if the defining frame is gone (a subquery's own WITH, live only for its
//     compile), it is pushed back (cteBinding.frame).
//
// resolveCTERows and cteSchemaCols both use it, so rows and column names cannot
// come from different same-named CTEs (with3.test 2.0).
func (p *ReadOnlyPager) enterCTEBodyScope(name string, b *cteBinding) func() {
	saved := p.cteScopes
	switch {
	case !p.cteFrameVisible(name, b) && b.frame != nil:
		p.cteScopes = append(p.cteScopes[:len(p.cteScopes):len(p.cteScopes)], b.frame)
	case b.scopeDepth > 0 && b.scopeDepth <= len(p.cteScopes):
		p.cteScopes = p.cteScopes[:b.scopeDepth]
	}
	return func() { p.cteScopes = saved }
}

// pushCTEScope builds one scope frame (a name->*cteBinding map, case-
// insensitive keys) for defs and pushes it onto p.cteScopes; the returned
// func pops it again. Called once, at the very top of execSelect
// (query.go), for any SelectStmt whose own CTEs is non-empty -- see
// ReadOnlyPager.cteScopes' doc comment (pager.go) for why one push/pop per
// execSelect call is enough to make these CTEs visible to everything
// (FROM, nested subqueries, compound arms) reached while THIS statement
// runs, without threading a scope parameter through every intermediate
// function.
func (p *ReadOnlyPager) pushCTEScope(defs []CTEDef) func() {
	frame := make(map[string]*cteBinding, len(defs))
	depth := len(p.cteScopes) + 1 // this frame's own index, +1 (it is visible to its members)
	for _, def := range defs {
		b := newCTEBinding(def, depth)
		b.frame = frame
		frame[r33sFoldIdent(def.Name)] = b
	}
	p.cteScopes = append(p.cteScopes, frame)
	return func() {
		p.cteScopes = p.cteScopes[:len(p.cteScopes)-1]
	}
}

// hideCTEScopes makes every WITH clause currently in scope invisible until the
// returned function is called. A stored VIEW's body resolves against the
// SCHEMA alone -- a CTE in the statement that queries the view must not shadow
// a table the view names. Verified directly: with "CREATE TABLE t1(x,y)"
// holding (1,2) and "CREATE VIEW v2 AS SELECT * FROM t1",
// "WITH t1(a,b) AS (SELECT 3,4) SELECT * FROM v2" returns t1's own row 1,2
// under the names x,y -- the CTE is not consulted at all (view2.test).
func (p *ReadOnlyPager) hideCTEScopes() func() {
	if p == nil {
		return func() {}
	}
	saved := p.cteScopes
	p.cteScopes = nil
	return func() { p.cteScopes = saved }
}

// lookupCTE searches p.cteScopes from the END (the most recently pushed,
// i.e. innermost, WITH clause) backward for name (case-insensitive),
// so an inner WITH clause's CTE shadows an outer one of the same name.
func (p *ReadOnlyPager) lookupCTE(name string) (*cteBinding, bool) {
	// A nil receiver is reachable: the WRITE path compiles/evaluates
	// expressions (a trigger's WHEN clause, a CHECK) against an evalCtx that
	// carries no pager until one is snapshotted, and a subquery inside such an
	// expression walks straight into the read compiler. There are no CTE
	// scopes in that case by definition, so report "not found" rather than
	// dereferencing -- the engine must never panic.
	if p == nil {
		return nil, false
	}
	key := r33sFoldIdent(name)
	for i := len(p.cteScopes) - 1; i >= 0; i-- {
		if b, ok := p.cteScopes[i][key]; ok {
			return b, true
		}
	}
	return nil, false
}

// snapshotCTEScopes returns an independent copy of p.cteScopes' current
// stack -- see Program.CTEScopeSnapshot (vdbe_op.go) for the one caller
// that needs this and why. append(nil, ...) always allocates a fresh backing
// array (Go's append semantics for a nil destination), so the returned slice
// can never alias p.cteScopes' own backing array: a LATER pushCTEScope on p
// (e.g. an unrelated statement reusing the same pager after this one
// returns) can never retroactively corrupt an already-taken snapshot.
func (p *ReadOnlyPager) snapshotCTEScopes() []map[string]*cteBinding {
	if p == nil || len(p.cteScopes) == 0 {
		return nil
	}
	return append([]map[string]*cteBinding(nil), p.cteScopes...)
}

// resolveCTERows is the run-once row source for a non-recursive CTE reference
// (runCTEOnce, resolveFrom): b's columns and rows, renamed per its column list.
// A recursive CTE is a compiled queue (compileRecursiveCTE) with its schema from
// cteSchemaCols, so reaching here with one is an internal error. p.cteExpansion
// (view.go's viewExpansion analogue) rejects a reference cycle as "circular
// reference: %s".
func (p *ReadOnlyPager) resolveCTERows(name string, b *cteBinding, params []Value) ([]columnInfo, [][]Value, error) {
	if b.recursive != nil {
		return nil, nil, fmt.Errorf("engine: internal: recursive CTE %s reached the run-time materializer", name)
	}
	if b.multiSelfRef {
		return nil, nil, fmt.Errorf("engine: multiple references to recursive table: %s", name)
	}
	for _, seen := range p.cteExpansion {
		if seen.b == b {
			return nil, nil, fmt.Errorf("engine: circular reference: %s", name)
		}
	}
	p.cteExpansion = append(p.cteExpansion, cteExpansionFrame{name: name, b: b})
	// Resolve the body in the scope where it was DEFINED (see
	// cteBinding.scopeDepth): frames pushed since then are invisible to it.
	defer p.enterCTEBodyScope(name, b)()
	cols, rows, err := p.resolveDerivedRows(b.core, params)
	p.cteExpansion = p.cteExpansion[:len(p.cteExpansion)-1]
	if err != nil {
		return nil, nil, err
	}
	return applyCTEColNames(name, b.colNames, cols, rows)
}

// applyCTEColNames applies a CTE's explicit "(col, ...)" rename list (if
// any) to cols/rows, or -- absent one -- rejects a case-insensitive
// duplicate output-column name exactly like view.go's viewOutputRows does
// for the identical reason: C SQLite disambiguates such a name with a
// ":N" suffix this package does not reproduce, so it declines cleanly
// rather than expose two same-named output columns. An explicit rename
// list is exempt (verified directly against C SQLite: "WITH c(x,x) AS
// ..." is accepted with no error, both column named "x" verbatim by this
// package -- matching CREATE VIEW's identical, already-accepted policy).
func applyCTEColNames(name string, colNames []string, cols []columnInfo, rows [][]Value) ([]columnInfo, [][]Value, error) {
	if colNames != nil {
		if len(colNames) != len(cols) {
			return nil, nil, fmt.Errorf("engine: table %s has %d values for %d columns", name, len(cols), len(colNames))
		}
		renamed := make([]columnInfo, len(cols))
		copy(renamed, cols)
		for i, n := range colNames {
			renamed[i].Name = n
		}
		// withExpand (select.c) substitutes the CTE's own column list for the
		// body's -- "pEList = pCte->pCols;" -- and then runs the SAME
		// sqlite3ColumnsFromExprList over it, so an explicit list is
		// ":N"-uniquified exactly like a derived one. "WITH c(x,x) AS
		// (SELECT 1,2) SELECT * FROM c" is accepted, as recorded above, but
		// 3.53.3 names the columns x and "x:1" and answers 1|2; left verbatim
		// both were x, the star bound both to the first, and the answer was
		// 1|1 -- a wrong VALUE.
		if uniqNames, uniqOK := r32mUniqueColumnNames(colNames); uniqOK {
			for i := range renamed {
				renamed[i].Name = uniqNames[i]
			}
		}
		return renamed, rows, nil
	}
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		lname := r33sFoldIdent(c.Name)
		if seen[lname] {
			return nil, nil, fmt.Errorf("engine: unsupported: duplicate result-column name %q in CTE %s (SQLite's ':N' disambiguation is not reproduced)", c.Name, name)
		}
		seen[lname] = true
	}
	return cols, rows, nil
}

// cteRecursionRowCap and cteRecursionByteCap stop a non-terminating recursive
// CTE's queue program (recQueueFill) cleanly. C has no such limit: its queue
// and UNION dedup set are ephemeral tables (select.c:2740, 2761) spilled to temp
// files, so a runaway recursion ends in SQLITE_FULL on disk. Here they live in
// RAM, so the bound is on memory:
//
//   - cteRecursionByteCap bounds content produced: 24 bytes per row slice plus
//     48 per Value plus TEXT/BLOB payload, counted logically, since a blob
//     passed through unchanged is shared here but copied per row by the
//     consumer.
//   - cteRecursionRowCap bounds time for narrow rows, where the byte cap is
//     millions of rows away. 300,000 is the largest incrementing bound in the
//     corpus (bigsort.test 1.0; then 100,000 in temptable2.test,
//     vacuummem.test, fts5unicode3.test). An unbounded "WITH c(x) AS
//     (VALUES(1) UNION ALL SELECT x+1 FROM c) INSERT INTO t SELECT x FROM c"
//     declines in about 1.7s.
//
// Neither applies when a finite bound is in force (the CTE's own LIMIT or the
// consumer's, outerRowCapPlus1): such a recursion stops after offset+limit
// rounds (recQueueFill), e.g. closure01.test's "... LIMIT 131072".
//
// A recursion feeding a bounded co-routine INSERT (streamSink.bounded,
// insert_stream_bounded.go) streams into the table's spilling row store
// (row_store_spill.go), so there cteRecursionByteCap bounds only what the queue
// holds (frontier plus UNION seen set, recQueue.residentBytes), and
// cteRecursionStreamByteCap the content produced (time and disk): bigsort.test
// 1.0 produces 2.83 GiB at a 462 MiB heap peak.
const (
	cteRecursionRowCap        = 300_000
	cteRecursionByteCap       = 512 << 20
	cteRecursionStreamByteCap = 4 << 30
)

// recursiveRowFootprint is one produced row's contribution to
// cteRecursionByteCap: the slice header, the Values themselves (48 bytes each,
// see Value's own doc comment in record.go), and every TEXT/BLOB payload byte.
func recursiveRowFootprint(row []Value) int64 {
	n := int64(24 + 48*len(row))
	for i := range row {
		n += int64(len(row[i].S))
	}
	return n
}

// recursiveCTECollations resolves the collation a UNION recursive CTE's dedup
// uses per output column. The body is a compound, and C keys the dedup
// ephemeral with the compound's collation (multiSelectCollSeq: leftmost arm
// with an opinion wins), so this is compoundColumnCollations over b.core's
// arms. Over 'a','A','b','B':
//
//	tt.k COLLATE NOCASE, "SELECT k FROM tt UNION SELECT s FROM c WHERE 0"
//	                                            -> 2 rows (NOCASE)
//	tb.k plain,          "SELECT k FROM tb UNION ..."           -> 4 (BINARY)
//	tb.k plain,   "SELECT k COLLATE NOCASE FROM tb UNION ..."   -> 2 (NOCASE)
//	tb.k plain,   "SELECT k FROM tb UNION SELECT upper(s) COLLATE NOCASE
//	               FROM c"                      -> 4: the leftmost arm's bare
//	                                               column IS an opinion (BINARY)
//	                                               and stops the walk
//	tb.k plain,   "SELECT k||'' FROM tb UNION SELECT upper(s) COLLATE NOCASE
//	               FROM c"                      -> 2: "||" has no opinion, so
//	                                               the RECURSIVE arm's governs
//
// b.selfRef is set to cols (no queue) meanwhile, so the recursive arms' FROM,
// which names this CTE, resolves schema-only instead of hitting the circularity
// guard; the binding carries no spec, so it never becomes a row source.
func (p *ReadOnlyPager) recursiveCTECollations(b *cteBinding, cols []columnInfo) []string {
	saved := b.selfRef
	b.selfRef = &recSelfRef{cols: cols, expansionDepth: -1}
	defer func() { b.selfRef = saved }()
	return compoundColumnCollations(p.resolveAllArmOutputs(b.core), len(cols))
}

// recursiveOrderColumn maps one ORDER BY term inside a recursive CTE to an
// output-column index, or -1 unless it is an in-range 1-based ordinal or a bare
// name of a result column (see recursiveCTEShape.orderCols). A COLLATE is
// stripped first and returned separately, as resolveCompoundOrderIndex/
// resolveCompoundOrderCollation do ("ORDER BY 2, 3 COLLATE nocase", with1.test's
// scan_tree); the queue's order is the answer.
//
// Names are searched in every arm, as C resolves a compound ORDER BY: with1.test
// 10.7.3's "WITH t(a) AS (SELECT 1 AS b UNION ALL SELECT a+1 AS c FROM t WHERE
// a<5 ORDER BY c)" is 1..5 ("c" is the second arm's alias), while "ORDER BY a"
// (10.7.1) is an error though "a" is the CTE's declared column.
func recursiveOrderColumn(e Expr, core *SelectStmt, nOut int) (idx int, collation string) {
	collation = ""
	if n, ok := topExprCollation(nil, e); ok {
		collation = n
	}
	switch x := stripOrderCollate(e).(type) {
	case LiteralExpr:
		if x.Val.Typ == Int && x.Val.I >= 1 && int(x.Val.I) <= nOut {
			return int(x.Val.I) - 1, collation
		}
	case ColumnExpr:
		if x.Qualifier != "" {
			return -1, ""
		}
		lists := [][]SelectColumn{core.Columns}
		for _, arm := range core.Compound {
			if arm.Stmt != nil {
				lists = append(lists, arm.Stmt.Columns)
			}
		}
		for _, cols := range lists {
			for i, c := range cols {
				if i >= nOut {
					break
				}
				if c.Star {
					return -1, "" // "*" makes the output-column mapping non-obvious
				}
				if equalFoldName(c.Alias, x.Name) {
					return i, collation
				}
				if col, ok := c.Expr.(ColumnExpr); ok && c.Alias == "" && equalFoldName(col.Name, x.Name) {
					return i, collation
				}
			}
		}
	}
	return -1, ""
}

// recursiveArmHasMultipleSelfRefs reports select.c:5786-5792's shape: an arm
// whose top-level FROM names this CTE more than once. C sets SF_Recursive on
// the first match; a second is "multiple references to recursive table: %s"
// (:5788). Checked separately because detectRecursiveShape returns only a shape
// or nil, and this reason concerns exactly a shape it declines.
func recursiveArmHasMultipleSelfRefs(name string, core *SelectStmt) bool {
	if core == nil {
		return false
	}
	for _, c := range core.Compound {
		if c.Stmt == nil {
			continue
		}
		n := 0
		for _, it := range c.Stmt.From {
			if cteFromItemRefs(it, name) {
				n++
			}
		}
		if n > 1 {
			return true
		}
	}
	return false
}

// detectRecursiveShape reports whether core is the recursive CTE shape this
// package compiles into a queue program (compileRecursiveCTE), or nil, in
// which case the CTE resolves like a derived table and a genuine
// self-reference is "circular reference: %s" (resolveCTERows' cteExpansion).
//
// The shape: a compound whose arms split into an initial prefix never naming
// the CTE and a non-empty suffix of arms that each reference it exactly once in
// their own top-level FROM (nowhere else) and hold no aggregate, all joined by
// the same operator, UNION or UNION ALL. The prefix is combined as written.
// Several recursive arms (with5.test's bidirectional walk) each expand every
// popped row in written order (one OpRecQueueFill per arm; select.c:2825).
//
// Both suffix constraints are C's: mixing UNION and UNION ALL among recursive
// arms, or a non-recursive arm after a recursive one, is "circular reference:
// c" there too. The operator also governs the seed rows: over seeds
// (1),(1),(2), UNION answers 1,2,11,101,12,102 and UNION ALL
// 1,1,2,11,101,11,101,12,102.
func detectRecursiveShape(name string, core *SelectStmt) *recursiveCTEShape {
	if core == nil || len(core.Compound) == 0 {
		return nil
	}
	// The recursive arms are the trailing run that mentions name. Walk from the
	// LAST arm backwards while each one still does, so firstRec ends up at the
	// first of them; an arm before that which mentions name is caught by the
	// selectReferencesTable(initial) check below, exactly as it always was.
	firstRec := len(core.Compound)
	for firstRec > 0 && topFromReferences(core.Compound[firstRec-1].Stmt, name) {
		firstRec--
	}
	if firstRec == len(core.Compound) {
		return nil // no self-referencing trailing arm -- not recursive at all
	}
	op := core.Compound[firstRec].Op
	if op != "UNION" && op != "UNION ALL" {
		return nil
	}

	recursiveArms := make([]*SelectStmt, 0, len(core.Compound)-firstRec)
	for _, c := range core.Compound[firstRec:] {
		// C SQLite refuses to mix the two operators across the recursive
		// arms, so one dedup policy always governs the whole recursion -- see
		// this function's doc comment for the two queries that pin it.
		if c.Op != op {
			return nil
		}
		recursiveArms = append(recursiveArms, c.Stmt)
	}

	for _, recursiveArm := range recursiveArms {
		if recursiveArm == nil {
			return nil
		}
		count := 0
		for _, it := range recursiveArm.From {
			if cteFromItemRefs(it, name) {
				count++
			}
		}
		if count != 1 {
			// count>1 is the "multiple references to recursive table" shape --
			// declined rather than guessing at C SQLite's own rejection text.
			// count==0 cannot happen for an arm topFromReferences selected, but
			// it is cheap to keep the invariant local.
			return nil
		}
		for _, c := range recursiveArm.Columns {
			if !c.Star && containsAggregate(c.Expr) {
				return nil // C SQLite: "recursive aggregate queries not supported"
			}
		}
		if recursiveArm.GroupBy != nil || containsAggregate(recursiveArm.Having) {
			return nil
		}
		if selectReferencesTableExcludingTopFrom(recursiveArm, name) {
			return nil // a second reference hiding in a nested subquery/WHERE/ON -- decline
		}
		// A LIMIT/OFFSET written as a bound PARAMETER inside the CTE is
		// declined: compileRecursiveCTE bakes the bound into the queue's spec
		// at prepare time, and a parameter's value is not known then.
		if recursiveArm.LimitParam != nil || recursiveArm.OffsetParam != nil {
			return nil
		}
	}
	lastArm := recursiveArms[len(recursiveArms)-1]
	if core.LimitParam != nil || core.OffsetParam != nil {
		return nil
	}
	// An ORDER BY inside the CTE selects the recursion's queue discipline
	// (see recursiveCTEShape.orderCols). Only ordinals and bare CTE column
	// names are recognized -- an arbitrary expression would have to be
	// evaluated against a CTE row whose scope this does not build, so it is
	// declined rather than silently ignored, which is what used to return
	// rows in the wrong order.
	orderBy := core.OrderBy
	if len(orderBy) == 0 {
		orderBy = lastArm.OrderBy
	}
	nOut := len(core.Columns)
	orderCols := make([]int, 0, len(orderBy))
	orderColl := make([]string, 0, len(orderBy))
	for _, ot := range orderBy {
		idx, coll := recursiveOrderColumn(ot.Expr, core, nOut)
		if idx < 0 {
			return nil
		}
		orderCols = append(orderCols, idx)
		orderColl = append(orderColl, coll)
	}

	initial := &SelectStmt{
		Columns:  core.Columns,
		Distinct: core.Distinct,
		From:     core.From,
		Where:    core.Where,
		GroupBy:  core.GroupBy,
		Having:   core.Having,
		Compound: core.Compound[:firstRec],
	}
	if selectReferencesTable(initial, name) {
		return nil // self-reference outside the recognized trailing-arm run -- C SQLite rejects this as circular; so does this package's ordinary (non-recursive) path
	}

	limit, offset := core.Limit, core.Offset
	if limit == nil && offset == nil {
		limit, offset = lastArm.Limit, lastArm.Offset
	}
	return &recursiveCTEShape{
		initial:       initial,
		recursiveArms: recursiveArms,
		unionAll:      op == "UNION ALL",
		orderCols:     orderCols,
		orderTerms:    orderBy,
		orderColl:     orderColl,
		limit:         limit,
		offset:        offset,
	}
}

// topFromReferences reports whether s names table in its OWN top-level FROM --
// the one position detectRecursiveShape recognizes a recursive self-reference
// in. A nil s (a malformed arm) references nothing.
func topFromReferences(s *SelectStmt, table string) bool {
	if s == nil {
		return false
	}
	for _, it := range s.From {
		if cteFromItemRefs(it, table) {
			return true
		}
	}
	return false
}

// cteFromItemRefs reports whether FROM item it references the CTE named name.
// A schema qualifier rules it out: a CTE lives in no database, so "main.t4"
// always names the table t4, as resolveJoinSources already decides (lookupCTE
// only when it.Schema == ""). "WITH t4(x) AS (VALUES(4) UNION ALL SELECT x+1
// FROM main.t4 WHERE x<10) SELECT * FROM t4" is not recursive and answers
// 4,5,8; "temp." behaves the same.
func cteFromItemRefs(it FromItem, name string) bool {
	return it.Subquery == nil && it.Schema == "" && equalFoldName(it.Table, name)
}

// selectReferencesTable reports whether stmt references a FROM-item (table
// name, not alias) matching name (case-insensitive) ANYWHERE within it: its
// own top-level FROM items (including recursing into a derived-table
// FROM-item's own subquery), every FROM item's ON condition, every
// select-list/WHERE/GROUP BY/HAVING expression (including any subquery
// nested inside one -- see exprReferencesTable), and every compound arm.
// Used by detectRecursiveShape to conservatively confirm the "initial" part
// of a candidate recursive CTE never references the CTE itself (which
// would make it a genuine reference cycle, not this package's recognized
// recursive shape).
func selectReferencesTable(stmt *SelectStmt, name string) bool {
	if stmt == nil {
		return false
	}
	for _, it := range stmt.From {
		if it.Subquery != nil {
			if selectReferencesTable(it.Subquery, name) {
				return true
			}
		} else if cteFromItemRefs(it, name) {
			return true
		}
		if exprReferencesTable(it.On, name) {
			return true
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star && exprReferencesTable(c.Expr, name) {
			return true
		}
	}
	if exprReferencesTable(stmt.Where, name) {
		return true
	}
	for _, g := range stmt.GroupBy {
		if exprReferencesTable(g, name) {
			return true
		}
	}
	if exprReferencesTable(stmt.Having, name) {
		return true
	}
	for _, arm := range stmt.Compound {
		if selectReferencesTable(arm.Stmt, name) {
			return true
		}
	}
	return false
}

// selectReferencesTableExcludingTopFrom is selectReferencesTable's
// counterpart for a candidate recursive arm: it does NOT count a plain
// top-level FROM-item reference to name (detectRecursiveShape's own loop
// already counted and validated exactly one of those separately) but still
// checks everything else -- a derived-table FROM-item's own subquery, every
// ON condition, every select-list/WHERE/GROUP BY/HAVING expression (and any
// subquery nested inside one) -- so a SECOND, hidden reference to the CTE
// (e.g. inside a WHERE-clause subquery, or a JOIN ON) is still caught and
// declined. A select-core (what a compound arm always is -- see
// CompoundArm's doc comment) never has its own Compound, so unlike
// selectReferencesTable this never needs to recurse into one.
func selectReferencesTableExcludingTopFrom(stmt *SelectStmt, name string) bool {
	if stmt == nil {
		return false
	}
	for _, it := range stmt.From {
		if it.Subquery != nil {
			if selectReferencesTable(it.Subquery, name) {
				return true
			}
		}
		if exprReferencesTable(it.On, name) {
			return true
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star && exprReferencesTable(c.Expr, name) {
			return true
		}
	}
	if exprReferencesTable(stmt.Where, name) {
		return true
	}
	for _, g := range stmt.GroupBy {
		if exprReferencesTable(g, name) {
			return true
		}
	}
	if exprReferencesTable(stmt.Having, name) {
		return true
	}
	return false
}

// exprReferencesTable is selectReferencesTable's expression-tree recursion,
// mirroring exprContainsSubquery's (sql_ast.go) exhaustive type switch over
// every Expr node kind, except it recurses INTO a nested subquery's own
// body (via selectReferencesTable) rather than stopping at its boundary --
// a self-reference hiding inside a scalar/EXISTS/IN subquery must still be
// found.
func exprReferencesTable(e Expr, name string) bool {
	if e == nil {
		return false
	}
	switch x := e.(type) {
	case LiteralExpr, ParamExpr, ColumnExpr:
		return false
	case UnaryExpr:
		return exprReferencesTable(x.X, name)
	case BinaryExpr:
		return exprReferencesTable(x.L, name) || exprReferencesTable(x.R, name)
	case IsNullExpr:
		return exprReferencesTable(x.X, name)
	case InExpr:
		if x.Sub != nil && selectReferencesTable(x.Sub, name) {
			return true
		}
		if exprReferencesTable(x.X, name) {
			return true
		}
		for _, it := range x.List {
			if exprReferencesTable(it, name) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return exprReferencesTable(x.X, name) || exprReferencesTable(x.Lo, name) || exprReferencesTable(x.Hi, name)
	case LikeExpr:
		return exprReferencesTable(x.X, name) || exprReferencesTable(x.Pattern, name) || exprReferencesTable(x.Escape, name)
	case GlobExpr:
		return exprReferencesTable(x.X, name) || exprReferencesTable(x.Pattern, name)
	case CollateExpr:
		return exprReferencesTable(x.X, name)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if exprReferencesTable(a, name) {
				return true
			}
		}
		return false
	case CastExpr:
		return exprReferencesTable(x.X, name)
	case CaseExpr:
		if x.Base != nil && exprReferencesTable(x.Base, name) {
			return true
		}
		for _, w := range x.Whens {
			if exprReferencesTable(w.When, name) || exprReferencesTable(w.Then, name) {
				return true
			}
		}
		return x.Else != nil && exprReferencesTable(x.Else, name)
	case SubqueryExpr:
		return selectReferencesTable(x.Stmt, name)
	case ExistsExpr:
		return selectReferencesTable(x.Stmt, name)
	default:
		return false
	}
}

// recursiveCTEOuterCap reports how many rows a recursive CTE may stop after,
// for a consuming statement whose only effect on it is a LIMIT (plus OFFSET),
// and names the CTE. A cap too low is a wrong answer while no cap merely
// declines, so anything that could read past the first limit+offset rows
// disqualifies:
//
//	WHERE / HAVING     filters, so row k of the CTE need not be row k of the output
//	ORDER BY           may need every row before the first output row is known
//	GROUP BY, DISTINCT the same, and both collapse rows
//	a JOIN or a second FROM item   multiplies rows, and can reference the CTE twice
//	a compound arm     the LIMIT belongs to the whole compound, not to this arm
//	a select-list item that is not computable from the pulled row alone --
//	                   a subquery (a second read of the CTE), an aggregate, or
//	                   a window function; see consumerItemIsPerRow
//
// leaving "SELECT <per-row expressions> FROM <cte> LIMIT n [OFFSET m]".
func recursiveCTEOuterCap(stmt *SelectStmt) (string, int64, bool) {
	if stmt == nil || stmt.Limit == nil || stmt.LimitParam != nil || stmt.OffsetParam != nil {
		return "", 0, false
	}
	if len(stmt.Compound) != 0 || stmt.Distinct || stmt.Where != nil ||
		len(stmt.GroupBy) != 0 || stmt.Having != nil || len(stmt.OrderBy) != 0 {
		return "", 0, false
	}
	if len(stmt.From) != 1 {
		return "", 0, false
	}
	it := stmt.From[0]
	if it.Subquery != nil || it.Schema != "" || it.Table == "" || it.On != nil ||
		len(it.Using) != 0 || it.Natural {
		return "", 0, false
	}
	// Every select-list item must be computable from the row the consumer has
	// already pulled -- see consumerItemIsPerRow. A "*" (bare or qualified)
	// always is: it expands to that row's own columns.
	for _, c := range stmt.Columns {
		if c.Star {
			continue
		}
		if c.Expr == nil || !consumerItemIsPerRow(c.Expr) {
			return "", 0, false
		}
	}
	limit := *stmt.Limit
	if limit < 0 {
		return "", 0, false
	}
	total := limit
	if stmt.Offset != nil {
		off := *stmt.Offset
		if off < 0 {
			return "", 0, false
		}
		total += off
		if total < limit {
			return "", 0, false // int64 overflow -- no cap rather than a wrong one
		}
	}
	return it.Table, total, true
}

// consumerItemIsPerRow reports whether one select-list expression of a bounded
// consumer can be computed from the pulled CTE row without reading past
// limit+offset rows. C runs the CTE as a co-routine (fromClauseTermCanBeCoroutine,
// select.c:7266, 8055) and decrements LIMIT at the bottom of selectInnerLoop
// (select.c:1522-1524), after the whole row is computed, so a scalar expression
// costs no extra pull ("SELECT i, randomblob(600) FROM data LIMIT 20" over a
// non-terminating data, incrcorrupt.test).
//
// Refused:
//   - an aggregate: the whole scan finishes before output (select.c:8891-8911),
//     so LIMIT bounds the output, not the source;
//   - a window call: the FROM moves into a sub-query without the LIMIT
//     (window.c:43-46);
//   - a subquery, conservatively: it could read the CTE again, and the bound
//     rides one reference (cteRef.outerRowCapPlus1). Admitting it changes no
//     answer here, but no corpus statement needs it and the fixture cannot
//     prove it (recursive_cte_consumer_expr_test.go).
//
// A whitelist: an unlisted form leaves the bound unset.
func consumerItemIsPerRow(e Expr) bool {
	switch x := e.(type) {
	case nil:
		// An absent optional operand (CaseExpr.Base/Else, LikeExpr.Escape),
		// reached only through the recursions below -- a select-list item that
		// is itself nil is refused by the caller.
		return true
	case LiteralExpr, ColumnExpr, ParamExpr:
		return true
	case UnaryExpr:
		return consumerItemIsPerRow(x.X)
	case BinaryExpr:
		return consumerItemIsPerRow(x.L) && consumerItemIsPerRow(x.R)
	case IsNullExpr:
		return consumerItemIsPerRow(x.X)
	case CollateExpr:
		return consumerItemIsPerRow(x.X)
	case CastExpr:
		return consumerItemIsPerRow(x.X)
	case BetweenExpr:
		return consumerItemIsPerRow(x.X) && consumerItemIsPerRow(x.Lo) && consumerItemIsPerRow(x.Hi)
	case LikeExpr:
		return consumerItemIsPerRow(x.X) && consumerItemIsPerRow(x.Pattern) && consumerItemIsPerRow(x.Escape)
	case GlobExpr:
		return consumerItemIsPerRow(x.X) && consumerItemIsPerRow(x.Pattern)
	case InExpr:
		if x.Sub != nil {
			return false
		}
		if !consumerItemIsPerRow(x.X) {
			return false
		}
		for _, li := range x.List {
			if !consumerItemIsPerRow(li) {
				return false
			}
		}
		return true
	case CaseExpr:
		if !consumerItemIsPerRow(x.Base) || !consumerItemIsPerRow(x.Else) {
			return false
		}
		for _, w := range x.Whens {
			if !consumerItemIsPerRow(w.When) || !consumerItemIsPerRow(w.Then) {
				return false
			}
		}
		return true
	case FuncExpr:
		// Over: a window call. Filter: "FILTER (WHERE ...)", which only ever
		// attaches to an aggregate or a window call, so refusing it outright
		// costs nothing that isAggregateCall would not already refuse.
		if x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 || isAggregateCall(x) {
			return false
		}
		for _, a := range x.Args {
			if !consumerItemIsPerRow(a) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// cteExpansionFrame is one entry of ReadOnlyPager.cteExpansion: the CTE
// DEFINITION currently being expanded, plus the name to report if it turns out
// to be a cycle. See that field's doc comment for why the identity that matters
// is the binding and not the name.
type cteExpansionFrame struct {
	name string
	b    *cteBinding
}
