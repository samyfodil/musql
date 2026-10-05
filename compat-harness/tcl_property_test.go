// Property-tier comparison for statements with nondeterministic values.
// Compares shape (column count/names) and row counts even when values differ.
//
//	  so they must be the random ones" would explain away exactly the
//	  divergences this tier exists to catch.
//	- the row ORDER, when the ORDER BY sorts by visible, deterministic keys --
//	  with the same tie tolerance the strict compare has (tclOrderByTieOK),
//	  because SQL leaves the order WITHIN a tie group unspecified.
//
// The tier is only ever reached AFTER the strict compare has already failed
// (runTCLSegment's !ok arm), so it can never weaken a statement that matches
// exactly today: such a statement passes on its own merits and never enters
// this file. A statement whose PROPERTIES disagree is WRONG and is reported as
// wrong, not absorbed.
package compat

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/samyfodil/musql/engine"
)

// tclColCompare is how strongly one OUTPUT COLUMN can be compared.
type tclColCompare int

const (
	// tclColStrict: the column's value does not depend on the RNG at all --
	// compare it exactly, like the strict compare does.
	tclColStrict tclColCompare = iota
	// tclColClass: only the storage class is fixed by the statement.
	tclColClass
	// tclColClassLen: the storage class AND the byte length are fixed.
	tclColClassLen
	// tclColNothing: nothing about this cell is derivable.
	tclColNothing
)

// tclPropPlan is what a statement's own text says is comparable about its
// result. Every field is derived from the STATEMENT (and, for a tainted read,
// from which tables earlier statements filled with random content) -- never
// from the two results being compared.
type tclPropPlan struct {
	cols []tclColCompare // one entry per output column
	// rowCount reports whether the number of rows is fixed by the statement.
	rowCount bool
	// rowsAlign reports whether the ROWS can be matched up one-for-one at all
	// (i.e. the row SET, not just its size, is fixed).
	rowsAlign bool
	// orderStable reports whether the row ORDER is fixed too, and orderKeys
	// holds the output-column indices of the ORDER BY keys that fix it (empty
	// unless orderStable). SQL leaves the order WITHIN a tie group
	// unspecified, so the keys are needed to tell a legitimate tie reorder
	// from a genuinely wrong order -- exactly the distinction tclOrderByTieOK
	// draws for the strict compare.
	orderStable bool
	orderKeys   []int
	// why names the reason this statement needs the tier, for the tally log.
	why string
}

// tclPropPlanFor builds the plan for stmt, or reports ok=false when the
// statement has no nondeterministic input at all -- in which case a failed
// strict compare is a plain WRONG answer and must stay one.
func tclPropPlanFor(stmt string, outCols []string, tainted tclRandomTainted) (tclPropPlan, bool) {
	ncols := len(outCols)
	switch {
	case tainted.reads(stmt):
		return tclTaintedPlan(stmt, ncols, tainted), true
	case tclCallsNondeterministicFunc(stmt):
		return tclStatementNondetPlan(stmt, outCols), true
	}
	return tclPropPlan{}, false
}

// tclTaintedPlan is the plan for a query reading a table an earlier statement
// filled with random content (vacuum3.test's "UPDATE t1 SET d =
// randomblob(1000)" and then a plain "SELECT * FROM t1"). The taint is tracked
// per TABLE, not per column, so no output cell can be claimed comparable --
// but the columns themselves still are, and so is the row count whenever the
// tainted CONTENT cannot have decided which rows come back.
func tclTaintedPlan(stmt string, ncols int, tainted tclRandomTainted) tclPropPlan {
	p := tclPropPlan{cols: make([]tclColCompare, ncols), why: "tainted-read"}
	for i := range p.cols {
		p.cols[i] = tclColNothing
	}
	// The row COUNT survives only when (a) every tainted table this query
	// reads was tainted in its VALUES rather than in its ROWS (see
	// tclTaintKind) and (b) nothing in the statement can filter, group or
	// truncate on those values. Both halves are needed: "SELECT * FROM t1"
	// over a randomblob-filled t1 returns the same rows on both sides, while
	// "SELECT * FROM t1 WHERE d > 'x'" does not.
	if !tainted.readsRowTainted(stmt) && tclNoRowFilteringClauses(stmt) {
		p.rowCount = true
	}
	return p
}

// tclNoRowFilteringClauses reports whether stmt's row COUNT is decided purely
// by the tables it reads -- no WHERE/HAVING/DISTINCT/GROUP BY/LIMIT/OFFSET and
// no join, any of which could consult a tainted value. Parsed rather than
// grepped so a WHERE inside a string literal or a subquery's own LIMIT is not
// mistaken for the statement's.
func tclNoRowFilteringClauses(stmt string) bool {
	ast, err := engine.ParseSelect(stmt)
	if err != nil {
		return false
	}
	if ast.Distinct || ast.Where != nil || ast.Having != nil ||
		len(ast.GroupBy) > 0 || len(ast.Compound) > 0 ||
		ast.Limit != nil || ast.Offset != nil ||
		ast.LimitParam != nil || ast.OffsetParam != nil {
		return false
	}
	if len(ast.From) != 1 {
		return false
	}
	for _, c := range ast.Columns {
		// An aggregate collapses the row count to something the tainted
		// values can decide (count(DISTINCT d)); a plain column cannot.
		if !c.Star {
			if _, ok := c.Expr.(engine.ColumnExpr); !ok {
				return false
			}
		}
	}
	return true
}

// tclStatementNondetPlan is the plan for a query that calls random()/
// randomblob() itself.
func tclStatementNondetPlan(stmt string, outCols []string) tclPropPlan {
	ncols := len(outCols)
	p := tclPropPlan{why: "nondeterministic-call"}
	ast, err := engine.ParseSelect(stmt)
	if err != nil {
		// Outside this engine's own SELECT grammar: claim nothing but the
		// columns, which are compared unconditionally by tclPropertyMatch.
		p.cols = tclAllNothing(ncols)
		return p
	}
	cols, mapped := tclArmColCompare(ast, ncols)
	if !mapped {
		p.cols = tclAllNothing(ncols)
		return p
	}
	p.cols = cols
	// Does the nondeterminism stay INSIDE the result-column expressions? Count
	// the calls in the whole statement and in each result column: a call the
	// columns do not account for is in a WHERE, a FROM subquery, a CTE, an
	// ORDER BY or a LIMIT, any of which can decide which rows come back.
	// Counting can only ever OVER-count the statement (a "random(" inside a
	// string literal), which errs toward claiming less.
	escaped := tclCountNondet(stmt) > tclColumnNondetCount(ast)
	anyNondet := false
	for _, c := range cols {
		if c != tclColStrict {
			anyNondet = true
		}
	}
	// DISTINCT and GROUP BY collapse duplicate draws, so the number of rows
	// then depends on which values were drawn.
	collapses := ast.Distinct || len(ast.GroupBy) > 0
	p.rowCount = !escaped && !(anyNondet && collapses)
	// A LIMIT plus an ORDER BY that can see a random value chooses a random
	// SUBSET, not just a random order, so the rows themselves stop lining up.
	truncates := ast.Limit != nil || ast.Offset != nil || ast.LimitParam != nil || ast.OffsetParam != nil
	p.rowsAlign = p.rowCount && !(truncates && anyNondet)
	// The ORDER BY fixes the row order only when every one of its keys is a
	// VISIBLE output column and none of them can see a random draw. A key
	// that is not in the output leaves a tie group's order unjudgeable from
	// the result at all (tclOrderByKeyHidden's own case), so the projection
	// is compared as a multiset instead.
	if p.rowsAlign && tclHasTopLevelOrderBy(stmt) && !tclOrderByTouchesNondet(ast, cols) {
		if keys, ok := tclOrderByKeyIndices(stmt, outCols); ok {
			p.orderStable, p.orderKeys = true, keys
		}
	}
	return p
}

func tclAllNothing(n int) []tclColCompare {
	out := make([]tclColCompare, n)
	for i := range out {
		out[i] = tclColNothing
	}
	return out
}

// tclArmColCompare maps a compound SELECT's arms onto ncols output columns,
// taking the WEAKEST comparability any arm contributes to each position. It
// reports mapped=false when a "*" makes the mapping ambiguous (two or more
// stars in one arm), in which case the caller claims nothing per cell.
func tclArmColCompare(ast *engine.SelectStmt, ncols int) ([]tclColCompare, bool) {
	out := make([]tclColCompare, ncols)
	arms := []*engine.SelectStmt{ast}
	for _, a := range ast.Compound {
		arms = append(arms, a.Stmt)
	}
	for _, arm := range arms {
		got, ok := tclOneArmColCompare(arm, ncols)
		if !ok {
			return nil, false
		}
		for i := range out {
			if got[i] > out[i] {
				out[i] = got[i]
			}
		}
	}
	return out, true
}

// tclOneArmColCompare expands one arm's select list to ncols entries. A "*"
// contributes only deterministic columns (a star cannot name a function call),
// so the only thing it costs is the ability to place the columns after it --
// which is recoverable as long as exactly one star is present.
func tclOneArmColCompare(arm *engine.SelectStmt, ncols int) ([]tclColCompare, bool) {
	stars := 0
	for _, c := range arm.Columns {
		if c.Star {
			stars++
		}
	}
	if stars > 1 {
		return nil, false
	}
	out := make([]tclColCompare, 0, ncols)
	for _, c := range arm.Columns {
		if c.Star {
			expand := ncols - (len(arm.Columns) - 1)
			if expand < 0 {
				return nil, false
			}
			for i := 0; i < expand; i++ {
				out = append(out, tclColStrict)
			}
			continue
		}
		out = append(out, tclExprCompare(c.Expr, c.RawText))
	}
	if len(out) != ncols {
		return nil, false
	}
	return out, true
}

// tclExprCompare decides how strongly ONE result-column expression can be
// compared, from the expression alone.
//
// Every rule below is read off C SQLite's own implementation of the generator,
// because "it is random, so anything goes" is exactly the assumption that
// loses the signal:
//
//   - random() is sqlite3_result_int64 of a randomness draw (func.c:565-585,
//     randomFunc), so its storage class is INTEGER on every draw, on both
//     engines.
//   - randomblob(N) is sqlite3_result_blob of exactly N bytes, with N<1
//     clamped to 1 (func.c:591-608, randomBlob), so its storage class is BLOB
//     and its byte length is fixed by N -- which is comparable whenever N
//     itself does not depend on the RNG, even when this harness cannot compute
//     N (misc1.test calls randomblob() on a deeply nested constant
//     expression).
//   - hex(X) renders each byte of X as two ASCII characters (func.c:1343,
//     hexFunc), so hex(randomblob(N)) is TEXT of 2N characters.
//   - abs() of an INTEGER argument returns an INTEGER (func.c:194, absFunc,
//     the SQLITE_INTEGER case) -- and cannot overflow on a random() draw,
//     because randomFunc masks the sign bit precisely so that
//     -9223372036854775808 is never produced.
//   - the BITWISE operators go through sqlite3VdbeIntValue (vdbe.c:2046-2047)
//     and stamp MEM_Int, so they are INTEGER unless an operand is NULL.
//   - "%" is NOT one of them. It is OP_Remainder, in the ARITHMETIC group
//     (vdbe.c:1895), which does not call sqlite3VdbeIntValue and whose
//     integer path RETURNS NULL WHEN THE DIVISOR IS ZERO:
//     "if( iA==0 ) goto arithmetic_result_is_null;" (vdbe.c:1923). A random()
//     divisor CAN draw 0, so "<int> % random()" is INTEGER-or-NULL, not
//     INTEGER, and the divisor must be a non-zero literal for the claim to
//     hold. The odds are 1 in 2^64 -- the same order as the abs()/
//     SMALLEST_INT64 case randomFunc's own sign mask exists to prevent, which
//     is the argument for closing it rather than against.
//
// Anything else that contains a nondeterministic call yields tclColNothing:
// "CASE WHEN random()>0 THEN 1 END" is genuinely NULL-or-1 and
// "randomblob(0) - 1" is genuinely INTEGER-or-REAL (a random blob's leading
// bytes decide which), so asserting a class for either would manufacture a
// divergence rather than catch one.
func tclExprCompare(e engine.Expr, rawText string) tclColCompare {
	if !tclCallsNondeterministicFunc(rawText) {
		return tclColStrict
	}
	// More than one generator call in this column's source text means an
	// argument is itself nondeterministic, so none of the length rules hold.
	if tclCountNondet(rawText) != 1 {
		return tclColNothing
	}
	return tclExprCompareShape(e)
}

func tclExprCompareShape(e engine.Expr) tclColCompare {
	switch x := e.(type) {
	case engine.FuncExpr:
		if x.Over != nil || x.Filter != nil || x.Distinct {
			return tclColNothing
		}
		switch strings.ToLower(x.Name) {
		case "random":
			if len(x.Args) == 0 {
				return tclColClass
			}
		case "randomblob":
			if len(x.Args) == 1 {
				return tclColClassLen
			}
		case "hex":
			// Only over randomblob: hex(random()) renders a decimal integer
			// whose digit count varies with the draw.
			if len(x.Args) == 1 {
				if inner, ok := x.Args[0].(engine.FuncExpr); ok &&
					strings.EqualFold(inner.Name, "randomblob") && len(inner.Args) == 1 {
					return tclColClassLen
				}
			}
		case "abs":
			if len(x.Args) == 1 && tclExprCompareShape(x.Args[0]) == tclColClass {
				return tclColClass
			}
		}
	case engine.UnaryExpr:
		// Unary +/-/~ of an integer stays an integer.
		switch x.Op {
		case "+", "-", "~":
			if tclExprCompareShape(x.X) == tclColClass {
				return tclColClass
			}
		}
	case engine.BinaryExpr:
		switch x.Op {
		case "&", "|", "<<", ">>":
			l, r := tclExprCompareShape(x.L), tclExprCompareShape(x.R)
			lInt := l == tclColClass || tclIsNonZeroIntLiteral(x.L)
			rInt := r == tclColClass || tclIsNonZeroIntLiteral(x.R)
			if lInt && rInt {
				return tclColClass
			}
		case "%":
			// Split out of the bitwise arm because its DIVISOR cannot be a
			// generator: OP_Remainder returns NULL for a zero divisor
			// (vdbe.c:1923), and random() can draw 0. The left operand may
			// still be a generator; only the right one must be a non-zero
			// literal.
			l := tclExprCompareShape(x.L)
			lInt := l == tclColClass || tclIsNonZeroIntLiteral(x.L)
			if lInt && tclIsNonZeroIntLiteral(x.R) {
				return tclColClass
			}
		}
	}
	return tclColNothing
}

// tclIsNonZeroIntLiteral reports whether e is a literal integer that is safe
// as the right operand of % (a zero divisor yields NULL, not an integer).
func tclIsNonZeroIntLiteral(e engine.Expr) bool {
	lit, ok := e.(engine.LiteralExpr)
	return ok && lit.Val.Typ == engine.Int && lit.Val.I != 0
}

// tclCountNondet counts random()/randomblob() calls in a piece of SQL text.
func tclCountNondet(sql string) int {
	return len(tclNondeterministicFuncRegex.FindAllString(sql, -1))
}

// tclColumnNondetCount sums the generator calls that sit inside a result
// column of any arm -- the ones that decide a VALUE rather than a row set.
func tclColumnNondetCount(ast *engine.SelectStmt) int {
	n := 0
	arms := []*engine.SelectStmt{ast}
	for _, a := range ast.Compound {
		arms = append(arms, a.Stmt)
	}
	for _, arm := range arms {
		for _, c := range arm.Columns {
			if !c.Star {
				n += tclCountNondet(c.RawText)
			}
		}
	}
	return n
}

// tclOrderByTouchesNondet reports whether any top-level ORDER BY term names a
// nondeterministic result column, by ordinal ("ORDER BY 1") or by its alias
// ("... AS r ... ORDER BY r"). A term that CALLS a generator itself is already
// covered by the escaped-call count above.
func tclOrderByTouchesNondet(ast *engine.SelectStmt, cols []tclColCompare) bool {
	nondetAlias := map[string]bool{}
	for i, c := range ast.Columns {
		if c.Star || i >= len(cols) || cols[i] == tclColStrict {
			continue
		}
		if c.HasAlias && c.Alias != "" {
			nondetAlias[strings.ToLower(c.Alias)] = true
		}
		nondetAlias[strings.ToLower(strings.TrimSpace(c.Text))] = true
	}
	for _, ot := range ast.OrderBy {
		switch x := ot.Expr.(type) {
		case engine.LiteralExpr:
			if x.Val.Typ == engine.Int {
				i := int(x.Val.I) - 1
				if i >= 0 && i < len(cols) && cols[i] != tclColStrict {
					return true
				}
			}
		case engine.ColumnExpr:
			if nondetAlias[strings.ToLower(x.Name)] {
				return true
			}
		}
	}
	return false
}

// tclPropertyMatch is the weaker compare itself. It reports ok=false -- a
// WRONG answer -- when any property the plan says is fixed disagrees.
func tclPropertyMatch(p tclPropPlan, gCols []string, gRows [][]string, cCols []string, cRows [][]string) (bool, string) {
	// Columns are compared unconditionally, and exactly: no nondeterministic
	// value can change how many columns a query returns or what they are
	// called.
	if len(gCols) != len(cCols) {
		return false, fmt.Sprintf("column count: engine=%d cgo=%d", len(gCols), len(cCols))
	}
	for i := range gCols {
		if gCols[i] != cCols[i] {
			return false, fmt.Sprintf("column %d name: engine=%q cgo=%q", i, gCols[i], cCols[i])
		}
	}
	if !p.rowCount {
		return true, ""
	}
	if len(gRows) != len(cRows) {
		return false, fmt.Sprintf("row count: engine=%d cgo=%d", len(gRows), len(cRows))
	}
	if !p.rowsAlign {
		return true, ""
	}
	g := tclProject(p, gRows)
	c := tclProject(p, cRows)
	if !p.orderStable {
		g = sortedRowsCopy(g)
		c = sortedRowsCopy(c)
	}
	for r := range g {
		// Guard the width rather than trusting it. Both drivers build rows of
		// len(cols) so a short row is not reachable today, but every other
		// comparator in this harness guards it (queryResultsMatch,
		// tclOrderByTieOK, tclProjectionsTieOnly) and an index panic is this
		// project's hardest gate failure -- invariant 3, no exceptions.
		if len(g[r]) != len(c[r]) {
			return false, fmt.Sprintf("row %d column count: engine=%d cgo=%d", r, len(g[r]), len(c[r]))
		}
		for i := range g[r] {
			// A tclColStrict column is compared with cellsEqual, the SAME
			// comparator the strict path uses, so it carries the INTEGER/REAL
			// storage tolerance without canonicalCell's six-decimal precision
			// loss. Every other kind is already a rendered token ("class=S",
			// "class=S,len=16", "") where plain equality is what is meant.
			same := g[r][i] == c[r][i]
			if !same && i < len(p.cols) && p.cols[i] == tclColStrict {
				same = cellsEqual(g[r][i], c[r][i])
			}
			if !same {
				// Under a deterministic ORDER BY the two engines can still
				// legitimately order a TIE GROUP differently, so give the
				// same tolerance the strict compare gives (tclOrderByTieOK):
				// accept only when both sides agree on every ORDER BY KEY at
				// every position and hold the same rows overall.
				if p.orderStable && tclProjectionsTieOnly(p.orderKeys, g, c) {
					return true, ""
				}
				return false, fmt.Sprintf("row %d col %d (%s): engine=%s cgo=%s", r, i, gCols[i], g[r][i], c[r][i])
			}
		}
	}
	return true, ""
}

// tclProjectionsTieOnly reports whether two projections differ ONLY inside
// ORDER BY tie groups: every ORDER BY key agrees at every position (so both
// orderings honor the ORDER BY) and the two projections hold the same
// multiset. Modelled directly on tclOrderByTieOK, which does the same job for
// the strict compare; it takes the key INDICES rather than re-deriving them
// because the plan already resolved them against these very columns.
func tclProjectionsTieOnly(keys []int, g, c [][]string) bool {
	if len(keys) == 0 || len(g) != len(c) {
		return false
	}
	for r := range g {
		if len(g[r]) != len(c[r]) {
			return false
		}
		for _, ki := range keys {
			if ki >= len(g[r]) || g[r][ki] != c[r][ki] {
				return false
			}
		}
	}
	gs, cs := sortedRowsCopy(g), sortedRowsCopy(c)
	for r := range gs {
		for i := range gs[r] {
			if gs[r][i] != cs[r][i] {
				return false
			}
		}
	}
	return true
}

// tclProject replaces every cell with the strongest token the plan says is
// comparable: the value itself for a deterministic column, and otherwise the
// storage class (plus the byte length where the generator fixes it).
func tclProject(p tclPropPlan, rows [][]string) [][]string {
	out := make([][]string, len(rows))
	for r, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			k := tclColNothing
			if i < len(p.cols) {
				k = p.cols[i]
			}
			cells[i] = tclCellToken(cell, k)
		}
		out[r] = cells
	}
	return out
}

// tclCellToken renders one cell down to what the plan permits comparing.
func tclCellToken(cell string, k tclColCompare) string {
	switch k {
	case tclColStrict:
		// The RAW cell. It used to be canonicalCell, on the reasoning that
		// that carries cellsEqual's INTEGER/REAL tolerance -- but canonicalCell
		// is a SORT key, and its own doc comment says so: it renders any
		// numeric as "N:" + FormatFloat(f, 'f', 6, 64). Six decimal places.
		// So abs(1000000000000000001) and ...002 both render "N:
		// 1000000000000000000.000000" and compared EQUAL, and the identical
		// divergence was reported WRONG when no random() column rode alongside
		// and ABSORBED when one did. A tier that claims a deterministic column
		// "compares exactly as it would strictly" has to actually do that.
		//
		// Sorting is unaffected: sortedRowsCopy sorts by rowSortKey, which
		// canonicalises every cell itself, so the raw value here still sorts
		// canonically. Only the COMPARISON changes, and it now goes through
		// cellsEqual -- the real comparator -- in tclPropertyMatch.
		return cell
	case tclColClass:
		return "class=" + string(tclCellClass(cell))
	case tclColClassLen:
		if n := tclCellBytes(cell); n >= 0 {
			return fmt.Sprintf("class=%c,len=%d", tclCellClass(cell), n)
		}
		return "class=" + string(tclCellClass(cell))
	}
	return "*"
}

// tclCellClass is a normalized cell's SQLite storage class. TEXT and BLOB are
// deliberately ONE class here: normalizeEngineValue and tclNormalizeCGOCell
// both render a blob whose bytes happen to be valid UTF-8 as "T:", so a
// randomblob(1) draw is "X:" on one side and "T:" on the other purely because
// of which byte came out. Its LENGTH still distinguishes them, and a genuine
// TEXT-vs-BLOB confusion in a fixed-length column is caught by that.
func tclCellClass(cell string) byte {
	tag, _, ok := splitTag(cell)
	if !ok {
		return '?'
	}
	switch tag {
	case 'T', 'X':
		return 'S'
	}
	return tag
}

// tclCellBytes is a normalized TEXT/BLOB cell's length in BYTES ("X:" carries
// two hex characters per byte), or -1 for a cell that has no byte length.
func tclCellBytes(cell string) int {
	tag, payload, ok := splitTag(cell)
	if !ok {
		return -1
	}
	switch tag {
	case 'X':
		return len(payload) / 2
	case 'T':
		return len(payload)
	}
	return -1
}

// ---- resource guard ----

// tclRandomMaterializationBudget bounds how much random blob content a
// statement may materialize before this harness declines to RUN it at all.
//
// This is a RESOURCE limit, not a comparability one: sort3.test's "WITH
// r(x,y) AS (SELECT 1, randomblob(1000) UNION ALL SELECT x+1, randomblob(1000)
// FROM r LIMIT 2200000) SELECT count(*), sum(length(y)) FROM r" has a
// perfectly deterministic result (2200000 and 2200000000) and would be a
// full PASS -- it just builds 2.2 GB of blob to get there, on BOTH engines,
// inside a 2G-capped test run. Two statements in the whole corpus trip this.
//
// MUSQL_NO_CAP=1 lifts it, so an uncapped run executes these two statements
// rather than dropping them; only 2G-capped runs keep the guard.
const tclRandomMaterializationCappedBudget = 32 << 20

var tclRandomMaterializationBudget int64 = func() int64 {
	if os.Getenv("MUSQL_NO_CAP") == "1" {
		return math.MaxInt64
	}
	return tclRandomMaterializationCappedBudget
}()

var (
	tclRandomblobArgRe = regexp.MustCompile(`(?i)\brandomblob\s*\(\s*(-?[0-9]+)\s*\)`)
	tclLimitCountRe    = regexp.MustCompile(`(?i)\blimit\s+([0-9]+)`)
)

// tclRandomMaterialization estimates the bytes of random blob a statement
// builds: the largest literal randomblob() size it names, times the largest
// literal LIMIT that could repeat it. Deliberately crude -- it only has to
// separate "megabytes" from "gigabytes".
func tclRandomMaterialization(stmt string) int64 {
	var blob, limit int64 = 0, 1
	for _, m := range tclRandomblobArgRe.FindAllStringSubmatch(stmt, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > blob {
			blob = n
		}
	}
	if blob == 0 {
		return 0
	}
	for _, m := range tclLimitCountRe.FindAllStringSubmatch(stmt, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > limit {
			limit = n
		}
	}
	return blob * limit
}
