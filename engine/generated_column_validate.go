package engine

// A generated column's expression must be validated at CREATE TABLE time.
// Validation includes: no subqueries, parameters, "." operator, or unknown
// functions; no PRIMARY KEY membership; not all columns generated; no cycles.
// These rules must match C SQLite's resolution to avoid schema divergence.

import (
	"fmt"
	"sort"
)

// validateGeneratedColumns validates one CREATE TABLE's finished column list.
// specs carries PRIMARY KEY/UNIQUE constraints. checkCycle is false for
// ALTER TABLE ADD COLUMN (schema reload, no cycle check at that time).
func validateGeneratedColumns(tblName string, cols []columnInfo, specs []autoIndexSpec, checkCycle bool) error {
	if !hasGeneratedCols(cols) {
		return nil
	}
	nNonGenerated := 0
	for _, c := range cols {
		if !c.IsGenerated() {
			nNonGenerated++
		}
	}
	if nNonGenerated == 0 {
		return fmt.Errorf("engine: must have at least one non-generated column")
	}
	// A generated column may be UNIQUE (verified against 3.53.3: "d AS (a)
	// UNIQUE" and "UNIQUE(d)" both create), but never part of the PRIMARY
	// KEY -- in either spelling, and even alongside a real column.
	for _, sp := range specs {
		if sp.kind != "pk" {
			continue
		}
		for _, cn := range sp.cols {
			if i := colIndexByName(cols, cn); i >= 0 && cols[i].IsGenerated() {
				return fmt.Errorf("engine: generated columns cannot be part of the PRIMARY KEY")
			}
		}
	}
	// A rowid-alias column is a PRIMARY KEY with no spec of its own.
	for _, c := range cols {
		if c.IsRowidAlias && c.IsGenerated() {
			return fmt.Errorf("engine: generated columns cannot be part of the PRIMARY KEY")
		}
	}

	// The scope is compileGeneratedColumns' own (noRowid: resolve.c:626
	// excludes NC_GenCol from the rowid match, so "y AS (rowid*2)" is
	// "no such column: rowid"), so validation and compilation cannot
	// disagree about what a bare name means.
	idx := buildColIndex(cols)
	refs := make([][]int, len(cols))
	for i := range cols {
		if !cols[i].IsGenerated() {
			continue
		}
		e, err := cachedGeneratedExpr(cols[i].GeneratedExpr)
		if err != nil {
			return fmt.Errorf("engine: CREATE TABLE %s: column %s: %w", tblName, cols[i].Name, err)
		}
		seen := map[int]bool{}
		if err := checkGeneratedColumnExpr(e, idx, seen); err != nil {
			return err
		}
		for j := range seen {
			refs[i] = append(refs[i], j)
		}
	}
	if !checkCycle {
		return nil
	}
	return checkGeneratedColumnCycle(cols, refs)
}

// colIndexByName does case-insensitive column lookup before buildColIndex's map.
func colIndexByName(cols []columnInfo, name string) int {
	for i := range cols {
		if equalFoldName(cols[i].Name, name) {
			return i
		}
	}
	return -1
}

// checkGeneratedColumnExpr walks a generated column's expression, rejecting what
// NC_GenCol rejects and recording column indices in seen. Declines on unknown nodes.
func checkGeneratedColumnExpr(e Expr, idx map[string]int, seen map[int]bool) error {
	var walk func(Expr) error
	walkAll := func(list ...Expr) error {
		for _, x := range list {
			if err := walk(x); err != nil {
				return err
			}
		}
		return nil
	}
	walk = func(e Expr) error {
		switch x := e.(type) {
		case nil, LiteralExpr:
			return nil
		case ParamExpr:
			return fmt.Errorf("engine: parameters prohibited in generated columns")
		case SubqueryExpr, ExistsExpr:
			return fmt.Errorf("engine: subqueries prohibited in generated columns")
		case ColumnExpr:
			// A qualifier is refused even when it names the table itself.
			if x.Qualifier != "" {
				return fmt.Errorf(`engine: the "." operator prohibited in generated columns`)
			}
			i, ok := idx[r33sFoldIdent(x.Name)]
			if !ok {
				// A DOUBLE-QUOTED name that resolves to no column degrades to
				// the STRING LITERAL it spells -- SQLite's
				// double-quoted-string misfeature, which
				// ColumnExpr.FallbackLiteral already carries and which the
				// VDBE's own compileColumn honors. So
				// "v AS (x || \"rowid\")" is 'rowid' the string, not
				// "no such column: rowid" (quote.test's
				// r32n-gencol-rowid-never-resolves, verified against 3.53.3).
				// It contributes no column reference, exactly like the literal
				// it now is. The index-expression validator has the same arm.
				if x.FallbackLiteral != nil {
					return nil
				}
				return fmt.Errorf("engine: no such column: %s", x.Name)
			}
			seen[i] = true
			return nil
		case FuncExpr:
			if isAggregateCall(x) {
				return fmt.Errorf("engine: misuse of aggregate function %s()", x.Name)
			}
			if x.CurrentTimeKw {
				// CURRENT_DATE/TIME/TIMESTAMP are not SQLITE_FUNC_CONSTANT
				// either, so resolve.c:1228 refuses them here too, in the
				// same words. See FuncExpr.CurrentTimeKw.
				return fmt.Errorf("engine: non-deterministic functions prohibited in generated columns")
			}
			if !indexExprFuncAllowed(x.Name) {
				// indexExprFuncAllowed is the same allow-list an index
				// expression uses, and for the same reason: resolve.c's
				// non-deterministic rule names NC_IdxExpr, NC_PartIdx and
				// NC_GenCol in ONE mask (resolve.c:1228), so the two sites
				// refuse exactly the same functions. An unknown name is a
				// different error, and checkExprSupported below reports it.
				if supportedFuncs[r33sFoldIdent(x.Name)] {
					return fmt.Errorf("engine: non-deterministic functions prohibited in generated columns")
				}
			}
			return walkAll(x.Args...)
		case UnaryExpr:
			return walk(x.X)
		case BinaryExpr:
			return walkAll(x.L, x.R)
		case IsNullExpr:
			return walk(x.X)
		case CollateExpr:
			return walk(x.X)
		case CastExpr:
			return walk(x.X)
		case BetweenExpr:
			return walkAll(x.X, x.Lo, x.Hi)
		case LikeExpr:
			return walkAll(x.X, x.Pattern, x.Escape)
		case CaseExpr:
			if err := walkAll(x.Base, x.Else); err != nil {
				return err
			}
			for _, w := range x.Whens {
				if err := walkAll(w.When, w.Then); err != nil {
					return err
				}
			}
			return nil
		case InExpr:
			if x.Sub != nil {
				return fmt.Errorf("engine: subqueries prohibited in generated columns")
			}
			if err := walk(x.X); err != nil {
				return err
			}
			return walkAll(x.List...)
		case RowExpr:
			return walkAll(x.Elems...)
		default:
			return fmt.Errorf("%w: CREATE TABLE: a generated column expression this write path cannot validate (%T)", errVDBEUnsupported, e)
		}
	}
	if err := walk(e); err != nil {
		return err
	}
	// checkExprSupported owns the "no such function" and
	// unsupported-construct answers, which are the same on every schema
	// expression; running it last keeps the NC_GenCol-specific messages above
	// from being pre-empted by it.
	return checkExprSupported(e)
}

// checkGeneratedColumnCycle is expr.c's COLFLAG_BUSY walk: a generated column
// whose expression reaches itself, directly or through other generated
// columns, is "generated column loop on \"X\"". A generated column that
// merely READS another one is fine -- "d AS (e), e AS (a), f AS (d)" creates.
//
// The name reported is the column the depth-first walk finds already BUSY,
// exactly as C reports pCol->zCnName at expr.c:4450. The walk starts from the
// LAST generated column, which is what reproduces the oracle's own choice on
// the two shapes measured against 3.53.3: "d AS (d)" is 'loop on "d"' and
// "d AS (e), e AS (d)" is 'loop on "e"'. C's own order is its code
// generator's, so this is the observed order rather than a derived one; the
// SET of rejected schemas does not depend on it.
func checkGeneratedColumnCycle(cols []columnInfo, refs [][]int) error {
	busy := make([]bool, len(cols))
	done := make([]bool, len(cols))
	var visit func(int) error
	visit = func(i int) error {
		if busy[i] {
			return fmt.Errorf("engine: generated column loop on %q", cols[i].Name)
		}
		if done[i] {
			return nil
		}
		busy[i] = true
		for _, j := range refs[i] {
			if !cols[j].IsGenerated() {
				continue
			}
			if err := visit(j); err != nil {
				return err
			}
		}
		busy[i] = false
		done[i] = true
		return nil
	}
	for i := len(cols) - 1; i >= 0; i-- {
		if !cols[i].IsGenerated() {
			continue
		}
		if err := visit(i); err != nil {
			return err
		}
	}
	return nil
}

// generatedExprDeps is the set of column indexes e reads, as
// compileGeneratedColumns stamps it onto columnInfo.genDeps. It is the
// dependency edge computeGeneratedInto walks so a generated column that reads
// ANOTHER one is evaluated after it.
//
// It reuses the validator's own walk, which records exactly that set as it
// goes, so the two cannot disagree about what an expression reads. The walk's
// ERROR is discarded here: validateGeneratedColumns has already refused such a
// schema at CREATE time, and a file that somehow carries one must still LOAD.
func generatedExprDeps(e Expr, idx map[string]int) []int {
	seen := map[int]bool{}
	checkGeneratedColumnExpr(e, idx, seen)
	if len(seen) == 0 {
		return nil
	}
	out := make([]int, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}
