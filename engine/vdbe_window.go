// WINDOW functions ("<fn>(...) OVER (...)") in the VDBE.
//
// The compiled scan runs the ordinary join/WHERE skeleton and appends every
// surviving row to a batch (OpWindowAppend); one OpWindowFinal then resolves
// the whole batch, since a window function is defined over a partition of the
// result, which exists only after the scan (the same shape as DISTINCT over
// GROUP BY, vdbe_group_distinct.go).
//
// Per window function, windowFinal:
//
//  1. partitions the batch by PARTITION BY (compareValues equality, NULLs
//     together, as GROUP BY);
//  2. orders each partition by the spec's ORDER BY, stably, so ties keep scan
//     order (as SQLite's sorter does);
//  3. walks each partition assigning values, honoring peer groups where the
//     function's definition requires.
//
// The default frame is SQLite's: with an ORDER BY, RANGE BETWEEN UNBOUNDED
// PRECEDING AND CURRENT ROW; without, the whole partition. Explicit frames are
// in vdbe_window_frame.go.
package engine

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
)

// windowCall is one "<fn>(...) OVER (...)" call site in the select list,
// paired with the window it is computed over. idx is its position in
// windowPlan.calls, which is also its slot in evalCtx.windowVals -- what the
// windowResultExpr placeholder left behind by rewriteWindowCalls reads.
type windowCall struct {
	name     string // lower-cased function name
	args     []Expr
	distinct bool
	star     bool // "count(*) OVER (...)" -- carried so planAggregateCall sees aggCountStar
	filter   Expr // FILTER (WHERE ...) modifier, applied per row by aggItem.step
	spec     *WindowSpec

	// The batch-entry COLUMNS holding this call's lowered OPERANDS, one per
	// spec.PartitionBy / spec.OrderBy / args entry, and -1 for an operand the
	// compiler left un-lowered. They are what makes the window machinery read
	// values instead of evaluating expressions: C SQLite appends every
	// one of these to its generated sub-select's expression list
	// (sqlite3WindowRewrite, window.c:1029-1047) and reads them back with
	// OP_Column (window.c:1681/:1952/:1975). See planWindowOperands
	// (vdbe_window_codegen.go) for what is lowered and what is not.
	partCols  []int
	orderCols []int
	argCols   []int

	// filterCol is the batch-entry column holding this call's FILTER (WHERE
	// ...) condition, or -1. SQLite appends a window function's FILTER to the
	// same sub-select expression list its arguments go into
	// (window.c:1049-1051) and reads it back with OP_Column right before the
	// step (window.c:1694), so it is an operand like any other.
	filterCol int

	// The 1-BIASED registers holding this call's frame-bound offsets, or 0
	// when the bound carries none -- SQLite's regStart/regEnd, coded once by
	// sqlite3ExprCode (window.c:2940/:2944). See planWindowFrameOffsets.
	startOffReg int
	endOffReg   int

	// stepArgs is the compiled program for a SUBTYPE aggregate's ARGUMENTS --
	// the one operand family SQLite deliberately does not buffer, re-coding it
	// at STEP time instead (pWin->bExprArgs, window.c:1041-1044 and :1728-1745).
	// nil for every other call, and for one this compiler could not lower; see
	// compileWindowStepArgs (vdbe_window_codegen.go) and windowAggregate, its
	// only caller.
	stepArgs *windowProjection
}

// funcExpr rebuilds the call's FuncExpr for isAggregateCall/planAggregateCall.
func (w windowCall) funcExpr() FuncExpr {
	return FuncExpr{Name: w.name, Args: w.args, Distinct: w.distinct, Star: w.star, Filter: w.filter}
}

// atCol is cols[j], or -1 when the compiler left that operand un-lowered (or
// never reached it at all).
func atCol(cols []int, j int) int {
	if j >= 0 && j < len(cols) {
		return cols[j]
	}
	return -1
}

func (w windowCall) partCol(j int) int  { return atCol(w.partCols, j) }
func (w windowCall) orderCol(j int) int { return atCol(w.orderCols, j) }
func (w windowCall) argCol(j int) int   { return atCol(w.argCols, j) }

// windowPlan is the OpWindowFinal P4 payload: everything needed to turn the
// collected batch into the query's result rows.
type windowPlan struct {
	scopes  []tableScope
	nCols   int // width of the [cols..] block in each batch entry
	nRowids int // number of trailing rowid slots (one per joined table)
	// nOps is the width of the OPERAND block that follows the rowids in each
	// batch entry: one column per lowered PARTITION BY / ORDER BY key and per
	// lowered window-function argument (planWindowOperands,
	// vdbe_window_codegen.go). This is SQLite's pMWin->nBufferCol tail -- the
	// operands sqlite3WindowRewrite appends to its sub-select's expression
	// list (window.c:1029-1047) so the window code can read them with
	// OP_Column instead of evaluating anything.
	nOps  int
	calls []windowCall
	outs  []Expr // select-list expressions, window calls already rewritten
	// Outer ORDER BY, evaluated over the same per-row context as outs (so it
	// may itself reference a window function, already rewritten).
	orderExprs []Expr
	// orderOrdinal[i] is the 0-based OUTPUT column an ORDER BY ordinal names,
	// or -1 when term i is an ordinary expression (orderExprs[i]).
	orderOrdinal []int
	// orderTerms keeps each ORDER BY term as written, for collation lookup.
	orderTerms []OrderTerm
	orderDesc  []bool
	orderNulls []NullsOrder
	limit      *int64
	offset     *int64
	distinct   bool
	// distinctColls is the collation SELECT DISTINCT dedups each output column
	// under, or nil when all are BINARY (OpDistinct's and OpGroup's convention),
	// computed by distinctCollations (vdbe_scan.go).
	//
	// C builds the dedup index's KeyInfo from the outer expression list
	// (select.c:8269), after sqlite3WindowRewrite has turned each outer column into
	// a TK_COLUMN on the sub-select's ephemeral table (window.c:805-819), whose
	// columns carry the source expression's collation (select.c:2426-2429), read
	// back by sqlite3ExprCollSeq (expr.c:262). So the rewrite is
	// collation-transparent, and topExprCollation's rule applies. Over "c TEXT
	// COLLATE NOCASE" holding apple/APPLE/pear/PEAR/fig/Fig/date:
	//
	//	SELECT DISTINCT c, count(*) OVER () FROM w           -- 4 rows
	//	SELECT DISTINCT c, count(*) OVER () FROM w LIMIT 2   -- apple,pear
	//
	// (a duplicate surviving the dedup would also consume a LIMIT slot).
	distinctColls []string

	// projExprs is THE list windowFinal projects per batch row: outs, then
	// every orderExprs entry that is not nil (an ORDER BY term naming an output
	// ORDINAL or ALIAS takes its value from the projected row instead). ONE
	// list, built once, with the compiled program (proj) as its only consumer.
	// Keeping it one list is what makes "which expressions a row projects, and
	// in what order" a single fact rather than two that could disagree (an
	// expression can fail).
	//
	// projOrder[j] is where ORDER BY term j's value lands in that list, or -1
	// when orderOrdinal[j] names an output column instead.
	projExprs []Expr
	projOrder []int
	// projLifts are the column references the COMPILED projection BUFFERED as
	// batch operand columns 0..len-1, in that column order: names of a query
	// ENCLOSING this one, which no register of this query's own row can hold
	// (windowBufCols, vdbe_window_codegen.go). planWindowOperands puts them at
	// the FRONT of the operand block, so those column numbers are the ones the
	// compiled program was built against.
	//
	// A lift that fails to compile in the scan body FAILS THE STATEMENT
	// (emitWindowOperands): the compiled projection reads these columns by
	// number and there is nothing else that can compute them.
	projLifts []Expr
	// projNQL/projNQLKnown are the nQueryLoop the projection compiles under:
	// the window query's own pre-loop value (compileScanWindow).
	projNQL      logEst
	projNQLKnown bool
	// proj is the COMPILED projection, and is never nil for a plan
	// compileScanWindow built -- compileWindowProjection is total
	// (vdbe_window_codegen.go), and windowFinal dereferences this.
	proj *windowProjection
	// orderStrict is armed by compileScanWindow when the order rows ARRIVE in
	// the batch is not provably C SQLite's -- see window_peer_order.go,
	// which owns the whole guard, and which windowPeerAmbiguous then turns
	// into an actual decline only for the rows that really did tie.
	orderStrict bool
	// loopOnlyStrict narrows that: orderStrict is set and the ONE thing left
	// unproven is which order the join loops are nested in, every source's own
	// access path having been proved. That is the case windowPeerLoopImmune
	// can still answer at run time from the rows themselves.
	loopOnlyStrict bool
	// srcColOfs is the prefix-sum of each source's column count within the
	// [cols..] block, len(srcs)+1 long, so srcColOfs[i]:srcColOfs[i+1] is
	// source i's own slice of a batch entry and nCols+i is its rowid slot.
	// windowPeerLoopImmune is the only reader.
	srcColOfs []int
}

// rewriteWindowCalls replaces every "... OVER ..." call in e with a
// windowResultExpr placeholder, appending each call to *calls. The returned
// tree is evaluated per row against an evalCtx whose windowVals holds the
// precomputed value for each call.
//
// Window calls are matched by IDENTITY of call site, not by structure: two
// textually identical calls get two slots. That costs one redundant
// computation and can never be wrong, whereas de-duplicating would have to
// prove the specs equivalent too.
func rewriteWindowCalls(e Expr, calls *[]windowCall) Expr {
	rw := func(x Expr) Expr { return rewriteWindowCalls(x, calls) }
	switch x := e.(type) {
	case nil:
		return nil
	case FuncExpr:
		if x.Over != nil {
			idx := len(*calls)
			*calls = append(*calls, windowCall{
				name: r33sFoldIdent(x.Name), args: x.Args, distinct: x.Distinct, star: x.Star, filter: x.Filter, spec: x.Over,
			})
			return windowResultExpr{idx: idx}
		}
		out := x
		out.Args = make([]Expr, len(x.Args))
		for i, a := range x.Args {
			out.Args[i] = rw(a)
		}
		return out
	case UnaryExpr:
		x.X = rw(x.X)
		return x
	case BinaryExpr:
		x.L, x.R = rw(x.L), rw(x.R)
		return x
	case IsNullExpr:
		x.X = rw(x.X)
		return x
	case InExpr:
		x.X = rw(x.X)
		for i, it := range x.List {
			x.List[i] = rw(it)
		}
		return x
	case BetweenExpr:
		x.X, x.Lo, x.Hi = rw(x.X), rw(x.Lo), rw(x.Hi)
		return x
	case LikeExpr:
		x.X, x.Pattern, x.Escape = rw(x.X), rw(x.Pattern), rw(x.Escape)
		return x
	case GlobExpr:
		x.X, x.Pattern = rw(x.X), rw(x.Pattern)
		return x
	case CollateExpr:
		x.X = rw(x.X)
		return x
	case CastExpr:
		x.X = rw(x.X)
		return x
	case CaseExpr:
		x.Base = rw(x.Base)
		whens := make([]WhenClause, len(x.Whens))
		for i, w := range x.Whens {
			whens[i] = WhenClause{When: rw(w.When), Then: rw(w.Then)}
		}
		x.Whens = whens
		x.Else = rw(x.Else)
		return x
	default:
		return e
	}
}

// windowOperand reads one batch row's operand (a PARTITION BY / ORDER BY key
// or a positional function's argument) from batch-entry column col, as C
// reads every buffered operand with OP_Column (window.c:1029-1047; :1681,
// :1952, :1975). Nothing in the window code evaluates an expression.
//
// An operand the compiler could not lower is declined at compile time
// (planWindowOperands, emitWindowOperands). Out of range is an error, never a
// panic: every operand column is written for every row, so a short entry is
// an internal inconsistency.
func (m *vdbe) windowOperand(plan *windowPlan, entry []Value, col int) (Value, error) {
	i := plan.nCols + plan.nRowids + col
	if col < 0 || i < 0 || i >= len(entry) {
		return Value{}, fmt.Errorf("vdbe: window operand column %d is outside a %d-value batch entry", col, len(entry))
	}
	return entry[i], nil
}

// windowRowCtx builds the evalCtx for one batch entry, splitting it into its
// column block and per-table rowids exactly like aggRowSplit does for the
// aggregate opcodes. The operand block planWindowOperands appends after the
// rowids is deliberately outside both slices: it is read by column
// (windowOperand), never resolved by name.
func (p *windowPlan) windowRowCtx(m *vdbe, entry []Value, winVals []Value) *evalCtx {
	return &evalCtx{
		tables:     p.scopes,
		vals:       entry[:p.nCols],
		rowids:     entry[p.nCols : p.nCols+p.nRowids],
		windowVals: winVals,
		outer:      m.outer,
		pager:      m.pager,
		params:     m.params,
	}
}

// projectWindowRow computes one batch row's projection (plan.projExprs: the
// outer select list, then each ORDER BY term that is not an output ordinal) by
// running the compiled program. compileWindowProjection is total, so there is
// always one.
func (m *vdbe) projectWindowRow(plan *windowPlan, pm *vdbe, entry, winVals []Value) ([]Value, error) {
	return plan.proj.run(pm, entry, winVals)
}

// windowFinal resolves the collected batch into the query's result rows.

// windowLevels returns, for each distinct window spec, the index in calls of
// the first call using it, in first-appearance order (select list, then ORDER
// BY), the order SQLite's resolver links them in. Call indices rather than
// specs, because a level needs that call's lowered key columns
// (partCols/orderCols).
//
// sqlite3WindowLink links a window into the SELECT's list only if it is first
// or identical to the head; any other spec is lifted by
// selectWindowRewriteExprCb into the generated sub-select, which runs its own
// sqlite3WindowRewrite. So N distinct specs are N nested window queries, the
// first-written outermost (see computeWindowLevels).
func windowLevels(calls []windowCall) []int {
	var levels []int
	for ci, c := range calls {
		seen := false
		for _, li := range levels {
			// Two calls that BOTH omit a spec share the nil pointer, which the
			// identity test catches; sameWindowSpec deliberately declines a
			// nil operand.
			if s := calls[li].spec; s == c.spec || sameWindowSpec(s, c.spec) {
				seen = true
				break
			}
		}
		if !seen {
			levels = append(levels, ci)
		}
	}
	return levels
}

// computeWindowLevels fills vals[row][call] and returns the order rows are
// emitted in. It walks the levels innermost first: each level partitions and
// orders the rows as the level below left them, computes its calls, and
// passes its order outward. Over t(a,k,z)=(1,9,'c'),(2,9,'a'),(3,9,'b') (k
// tied):
//
//	SELECT a, sum(a) OVER (ORDER BY k ROWS 1 PRECEDING),
//	          count(*) OVER (ORDER BY z)              FROM t
//	  -> (2,2,1), (3,5,2), (1,4,3)
//
// so the outer sum() runs over the inner level's z order, which changes its
// values. Swapping the two calls gives (2,1,3), (3,2,5), (1,3,1).
//
// Each level's partitions come out in PARTITION BY key order, since the
// sub-select is ordered by PARTITION BY then ORDER BY: "SELECT b, sum(a) OVER
// (PARTITION BY a%2 ORDER BY a) FROM t" over (1,A)(2,B)(3,C)(3,D)(5,E)(7,F)
// emits B first.
//
// Specs differing only in frame share a level here, where SQLite
// (sqlite3WindowCompare) opens two; that is sound because stable sorts by K,
// anything, then K again equal one sort by K over the middle. Each call still
// uses its own frame.
func (m *vdbe) computeWindowLevels(plan *windowPlan, batch [][]Value, vals [][]Value) ([]int, error) {
	order := make([]int, len(batch))
	for i := range order {
		order[i] = i
	}
	levels := windowLevels(plan.calls)
	for li := len(levels) - 1; li >= 0; li-- {
		lvl := plan.calls[levels[li]]
		spec := lvl.spec
		parts, err := m.partitionAndOrder(plan, lvl, batch, vals, order)
		if err != nil {
			return nil, err
		}
		// The PEER-ORDER guard (window_peer_order.go). Asked per LEVEL,
		// because a level's peer groups are what its own calls read, and
		// asked only when the compiler could not prove the arrival order --
		// windowPeerEnds is O(n) per partition and an unarmed plan must not
		// pay for it.
		peerAmbiguous := false
		if plan.orderStrict {
			peerAmbiguous, err = m.windowPeerAmbiguous(plan, lvl, parts, batch, vals)
			if err != nil {
				return nil, err
			}
		}
		for ci, call := range plan.calls {
			cs := call.spec
			if cs == nil {
				cs = spec
			}
			if cs != spec && !sameWindowSpec(cs, spec) {
				continue
			}
			if err := m.computeWindowCall(plan, call, ci, parts, batch, vals, peerAmbiguous); err != nil {
				return nil, err
			}
			clearWindowCallSubtype(call.name, ci, parts, vals)
		}
		if perr := m.sortPartitionsByKey(plan, lvl, parts, batch, vals); perr != nil {
			return nil, perr
		}
		next := make([]int, 0, len(batch))
		for _, part := range parts {
			next = append(next, part...)
		}
		if len(next) != len(batch) {
			return nil, fmt.Errorf("%w: window partitioning lost rows", errVDBEUnsupported) // defensive: never emit a partial batch
		}
		order = next
	}
	return order, nil
}

// sortPartitionsByKey orders whole partitions by their own PARTITION BY key,
// ascending under each key expression's collating sequence -- see
// computeWindowLevels, which is the only caller (the VALUES a window computes
// never depend on partition order, only the emitted ROW order does).
func (m *vdbe) sortPartitionsByKey(plan *windowPlan, lvl windowCall, parts [][]int, batch, vals [][]Value) error {
	spec := lvl.spec
	if spec == nil || len(spec.PartitionBy) == 0 || len(parts) < 2 {
		return nil
	}
	colls := m.windowKeyCollations(plan, spec.PartitionBy, batch, vals)
	keys := make([][]Value, len(parts))
	for i, part := range parts {
		ri := part[0] // every row of a partition shares its key by construction
		k := make([]Value, len(spec.PartitionBy))
		for j := range spec.PartitionBy {
			v, err := m.windowOperand(plan, batch[ri], lvl.partCol(j))
			if err != nil {
				return err
			}
			k[j] = v
		}
		keys[i] = k
	}
	idx := make([]int, len(parts))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return compareIndexRec(keys[idx[a]], keys[idx[b]], colls, nil, m.encoding()) < 0
	})
	sorted := make([][]int, len(parts))
	for i, j := range idx {
		sorted[i] = parts[j]
	}
	copy(parts, sorted)
	return nil
}

// sameWindowSpec reports whether two window specs are the SAME sort order for
// windowLevels' purposes: structurally identical PARTITION BY / ORDER BY
// expressions and directions. Frame differences do not affect the sort -- see
// computeWindowLevels for why sharing a level between them is exact. Structural
// equality is deliberately conservative -- two specs it cannot prove equal just
// become two levels sorting by the same keys, which a stable sort makes
// idempotent.
func sameWindowSpec(a, b *WindowSpec) bool {
	if a == nil || b == nil {
		return false
	}
	if len(a.PartitionBy) != len(b.PartitionBy) || len(a.OrderBy) != len(b.OrderBy) {
		return false
	}
	for i := range a.PartitionBy {
		if !reflect.DeepEqual(a.PartitionBy[i], b.PartitionBy[i]) {
			return false
		}
	}
	for i := range a.OrderBy {
		if a.OrderBy[i].Desc != b.OrderBy[i].Desc || a.OrderBy[i].Nulls != b.OrderBy[i].Nulls {
			return false
		}
		if !reflect.DeepEqual(a.OrderBy[i].Expr, b.OrderBy[i].Expr) {
			return false
		}
	}
	return true
}

func (m *vdbe) windowFinal(plan *windowPlan) ([][]Value, error) {
	batch := m.windowBatch
	n := len(batch)
	vals := make([][]Value, n) // vals[row][call]
	for i := range vals {
		vals[i] = make([]Value, len(plan.calls))
	}

	// C SQLite emits rows in the WINDOW's own sort order -- it computes the
	// window over a sorter and reads that sorter back -- so "SELECT x, count(*)
	// OVER (ORDER BY x) FROM t1" comes out x-ascending even though the query
	// never says so. Verified directly against C SQLite (window1.test).
	// computeWindowLevels does that per NESTING LEVEL, which is also what
	// decides each level's values; a top-level ORDER BY below then re-sorts
	// STABLY on top of it, so the emit order still breaks that ORDER BY's ties
	// exactly as SQLite's sorter does.
	emitOrder, eerr := m.computeWindowLevels(plan, batch, vals)
	if eerr != nil {
		return nil, eerr
	}

	// Project the select list per row, then order/dedup/slice.
	type outRow struct {
		out   []Value
		order []Value
		seq   int
	}
	rows := make([]outRow, 0, n)
	pm := plan.proj.machine(m)
	for _, i := range emitOrder {
		entry := batch[i]
		proj, err := m.projectWindowRow(plan, pm, entry, vals[i])
		if err != nil {
			return nil, err
		}
		o := proj[:len(plan.outs):len(plan.outs)]
		var ok []Value
		if len(plan.orderExprs) > 0 {
			ok = make([]Value, len(plan.orderExprs))
			for j := range plan.orderExprs {
				if plan.orderOrdinal[j] >= 0 {
					ok[j] = o[plan.orderOrdinal[j]]
					continue
				}
				ok[j] = proj[plan.projOrder[j]]
			}
		}
		rows = append(rows, outRow{out: o, order: ok, seq: i})
	}

	if plan.distinct {
		kept := rows[:0]
		for _, r := range rows {
			dup := false
			for _, k := range kept {
				// keysEqualGrouping, not keysEqual: DISTINCT compares under
				// each output column's own collating sequence. See
				// windowPlan.distinctColls for C's KeyInfo (select.c:8269) and
				// for the wrong answers a BINARY dedup produced here.
				if keysEqualGrouping(k.out, r.out, plan.distinctColls, m.encoding()) {
					dup = true
					break
				}
			}
			if !dup {
				kept = append(kept, r)
			}
		}
		rows = kept
	}

	if len(plan.orderExprs) > 0 {
		// Each ORDER BY term compares under its own COLLATING SEQUENCE, the
		// same rule an ordinary (window-free) ORDER BY follows: with
		// "color TEXT COLLATE NOCASE", "... FROM fruits ORDER BY color" is
		// green, RED, yellow, YELLOW (window9.test 1.5, verified directly),
		// not the BINARY order this path used to produce.
		orderColls := m.outerOrderCollations(plan, batch, vals)
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i].order, rows[j].order
			for k := range a {
				ot := OrderTerm{Desc: plan.orderDesc[k], Nulls: plan.orderNulls[k]}
				less, equal := orderTermLess(ot, a[k], b[k], atOrEmpty(orderColls, k), m.encoding())
				if equal {
					continue
				}
				return less
			}
			return false
		})
	}

	start, end := limitOffsetRange(len(rows), plan.limit, plan.offset)
	out := make([][]Value, 0, end-start)
	for _, r := range rows[start:end] {
		out = append(out, r.out)
	}
	return out, nil
}

// orderTermExprs pulls the expressions out of a window spec's ORDER BY terms.
func orderTermExprs(terms []OrderTerm) []Expr {
	out := make([]Expr, len(terms))
	for i, t := range terms {
		out[i] = t.Expr
	}
	return out
}

// exprHasAggregate reports whether e's tree calls an aggregate function
// (window calls of their own excluded -- those are handled as nested windows,
// which this slice declines separately).
func exprHasAggregate(e Expr) bool {
	_, ok := firstBareAggregate(e)
	return ok
}

// firstBareAggregate is exprHasAggregate with the call itself, for the one
// caller that has to NAME it: C SQLite's "misuse of aggregate: %s()"
// (disallowAggregatesInOrderByCb, window.c:942-949) reports the offending
// function's own token.
func firstBareAggregate(e Expr) (FuncExpr, bool) {
	var found FuncExpr
	ok := false
	walkExprShallow(e, func(fc FuncExpr) bool {
		if fc.Over == nil && isAggregateCall(fc) {
			found, ok = fc, true
		}
		return !ok
	})
	return found, ok
}

// outputAliasIndex returns the 0-based output column a bare ORDER BY name
// refers to by ALIAS, or -1. Only an unqualified name can be an alias, and a
// name that also resolves as a source column would have been rewritten by the
// caller's own expression path, which runs first.
func outputAliasIndex(outCols []outputColumn, e Expr) int {
	col, ok := e.(ColumnExpr)
	if !ok || col.Qualifier != "" {
		return -1
	}
	for i, oc := range outCols {
		if equalFoldName(oc.name, col.Name) {
			return i
		}
	}
	return -1
}

// outerOrderCollations returns each OUTER ORDER BY term's effective collating
// sequence. An ordinal term ("ORDER BY 2") takes the collation of the OUTPUT
// expression it names -- exactly what an ordinary ORDER BY does -- and any
// other term is resolved as written. Resolved once against any row's scope.
func (m *vdbe) outerOrderCollations(plan *windowPlan, batch [][]Value, vals [][]Value) []string {
	if len(plan.orderTerms) == 0 || len(batch) == 0 {
		return nil
	}
	ctx := plan.windowRowCtx(m, batch[0], vals[0])
	out := make([]string, len(plan.orderTerms))
	for i, ot := range plan.orderTerms {
		e := ot.Expr
		if plan.orderOrdinal[i] >= 0 {
			// The ordinal itself carries no collation; an explicit COLLATE on
			// the term does ("ORDER BY 2 COLLATE nocase"), and otherwise the
			// named output column's own applies.
			if name, ok := topExprCollation(ctx, e); ok {
				out[i] = name
				continue
			}
			if plan.orderOrdinal[i] < len(plan.outs) {
				e = plan.outs[plan.orderOrdinal[i]]
			}
		}
		if name, ok := topExprCollation(ctx, e); ok {
			out[i] = name
		}
	}
	return out
}

// partitionAndOrder groups the row indices in `order` by the spec's PARTITION
// BY keys (first-seen partition order, which only affects nothing observable --
// each partition is written back into its own rows' slots) and orders each
// partition by the spec's ORDER BY, stably.
//
// `order` is the sequence this level SEES its rows in, which is the previous
// (inner) level's emitted order rather than the raw scan order -- see
// computeWindowLevels. Both sorts here are STABLE, so that incoming order is
// what breaks a tie under this spec's own keys, exactly as it does for real
// SQLite's nested sub-selects.
func (m *vdbe) partitionAndOrder(plan *windowPlan, lvl windowCall, batch [][]Value, vals [][]Value, order []int) ([][]int, error) {
	spec := lvl.spec
	if spec == nil {
		spec = &WindowSpec{}
	}
	keyOf := func(i int, exprs []Expr, cols []int) ([]Value, error) {
		if len(exprs) == 0 {
			return nil, nil
		}
		k := make([]Value, len(exprs))
		for j := range exprs {
			v, err := m.windowOperand(plan, batch[i], atCol(cols, j))
			if err != nil {
				return nil, err
			}
			k[j] = v
		}
		return k, nil
	}

	// PARTITION BY and ORDER BY both compare under each key expression's own
	// COLLATING SEQUENCE, not BINARY: over "CREATE TABLE fruits(name TEXT
	// COLLATE NOCASE, ...)" holding apple/APPLE/pear/PEAR,
	// "dense_rank() OVER (ORDER BY name)" is 1,1,2,2 -- 'apple' and 'APPLE'
	// are PEERS -- and the rows come out in that NOCASE order (window9.test,
	// verified directly). Derived once from any row's scope; an empty batch
	// needs none.
	partColls := m.windowKeyCollations(plan, spec.PartitionBy, batch, vals)
	var parts [][]int
	var partKeys [][]Value
	for _, i := range order {
		pk, err := keyOf(i, spec.PartitionBy, lvl.partCols)
		if err != nil {
			return nil, err
		}
		found := -1
		for j, existing := range partKeys {
			if keysEqualCollated(existing, pk, partColls, m.encoding()) {
				found = j
				break
			}
		}
		if found < 0 {
			partKeys = append(partKeys, pk)
			parts = append(parts, []int{i})
			continue
		}
		parts[found] = append(parts[found], i)
	}

	if len(spec.OrderBy) > 0 {
		orderExprs := make([]Expr, len(spec.OrderBy))
		for i, ot := range spec.OrderBy {
			orderExprs[i] = ot.Expr
		}
		orderColls := m.windowKeyCollations(plan, orderExprs, batch, vals)
		keys := make(map[int][]Value, len(order))
		for _, i := range order {
			k, err := keyOf(i, orderExprs, lvl.orderCols)
			if err != nil {
				return nil, err
			}
			keys[i] = k
		}
		for _, part := range parts {
			var sortErr error
			sort.SliceStable(part, func(a, b int) bool {
				ka, kb := keys[part[a]], keys[part[b]]
				for k := range ka {
					less, equal := orderTermLess(spec.OrderBy[k], ka[k], kb[k], atOrEmpty(orderColls, k), m.encoding())
					if equal {
						continue
					}
					return less
				}
				return false
			})
			if sortErr != nil {
				return nil, sortErr
			}
		}
	}
	return parts, nil
}

// windowKeyCollations returns each key expression's effective collating
// sequence, resolved once against any row's scope (they are all the same
// scope). A nil/empty result degrades every comparison to BINARY, the previous
// behavior.
func (m *vdbe) windowKeyCollations(plan *windowPlan, exprs []Expr, batch [][]Value, vals [][]Value) []string {
	if len(exprs) == 0 || len(batch) == 0 {
		return nil
	}
	ctx := plan.windowRowCtx(m, batch[0], vals[0])
	out := make([]string, len(exprs))
	for i, e := range exprs {
		if name, ok := topExprCollation(ctx, e); ok {
			out[i] = name
		}
	}
	return out
}

// windowPeerEnds returns, for one already-ordered partition, the index (within
// the partition) one past the last row tied with each position under the
// window's ORDER BY. With no ORDER BY every row is a peer of every other, so
// the whole partition is one group -- which is exactly what makes a frameless
// aggregate window span the entire partition.
func (m *vdbe) windowPeerEnds(plan *windowPlan, call windowCall, part []int, batch [][]Value, vals [][]Value) ([]int, error) {
	spec := call.spec
	np := len(part)
	ends := make([]int, np)
	if spec == nil || len(spec.OrderBy) == 0 {
		for i := range ends {
			ends[i] = np
		}
		return ends, nil
	}
	orderExprs := make([]Expr, len(spec.OrderBy))
	for j, ot := range spec.OrderBy {
		orderExprs[j] = ot.Expr
	}
	// Peer grouping uses the ORDER BY keys' own collations, exactly like the
	// ordering itself (see partitionAndOrder).
	colls := m.windowKeyCollations(plan, orderExprs, batch, vals)
	keys := make([][]Value, np)
	for i, ri := range part {
		k := make([]Value, len(spec.OrderBy))
		for j := range spec.OrderBy {
			v, err := m.windowOperand(plan, batch[ri], call.orderCol(j))
			if err != nil {
				return nil, err
			}
			k[j] = v
		}
		keys[i] = k
	}
	i := 0
	for i < np {
		j := i + 1
		for j < np && keysEqualCollated(keys[i], keys[j], colls, m.encoding()) {
			j++
		}
		for k := i; k < j; k++ {
			ends[k] = j
		}
		i = j
	}
	return ends, nil
}

// windowBuiltinArity maps the built-in window functions (sqlite3WindowFunctions'
// aWindowFuncs, "built-in window functions that are not also aggregates",
// window.c:609-624, all SQLITE_FUNC_WINDOW, window.c:584/592/601) to their
// permitted argument counts. Membership also decides the word in resolve.c's
// "misuse of %s function %#T()" (resolve.c:1269-1273): these are "window"
// functions even without OVER. Keys are lower-cased (r33sFoldIdent).
var windowBuiltinArity = map[string][2]int{
	"row_number":   {0, 0},
	"rank":         {0, 0},
	"dense_rank":   {0, 0},
	"percent_rank": {0, 0},
	"cume_dist":    {0, 0},
	"ntile":        {1, 1},
	"first_value":  {1, 1},
	"last_value":   {1, 1},
	"nth_value":    {2, 2},
	"lead":         {1, 3},
	"lag":          {1, 3},
}

// checkWindowFuncArity enforces the built-in window functions' argument counts,
// which C SQLite reports as "wrong number of arguments to function <name>()"
// -- verified directly for row_number(a), rank(a), dense_rank(a,b),
// percent_rank(a) and cume_dist(a), all of which take NONE (windowerr.test).
// A function not listed here is an ordinary aggregate used as a window
// function, whose own arity check lives with the aggregate.
func checkWindowFuncArity(name string, nArgs int) error {
	rng, ok := windowBuiltinArity[name]
	if !ok {
		return nil
	}
	if nArgs >= rng[0] && nArgs <= rng[1] {
		return nil
	}
	return fmt.Errorf("engine: wrong number of arguments to function %s()", name)
}

// clearWindowCallSubtype drops the JSON subtype from a window call's results:
// C spills window material into an ephemeral table and reads it back with
// OP_Column, losing the subtype (record.go), so json_array(first_value(json(1))
// OVER ()) is ["1"]. json_group_array/json_group_object are exempt: their
// xValue/xFinal set JSON_SUBTYPE again (jsonArrayCompute,
// SQLITE_RESULT_SUBTYPE).
func clearWindowCallSubtype(name string, ci int, parts [][]int, vals [][]Value) {
	switch r33sFoldIdent(name) {
	case "json_group_array", "json_group_object", "jsonb_group_array", "jsonb_group_object":
		return
	}
	for _, part := range parts {
		for _, ri := range part {
			vals[ri][ci].Subtype = 0
		}
	}
}

// computeWindowCall fills vals[row][ci] for every row, per call's function.
// parts is this call's LEVEL's partitioning (computeWindowLevels): every call
// sharing one specification shares one partition/order pass.
func (m *vdbe) computeWindowCall(plan *windowPlan, call windowCall, ci int, parts [][]int, batch [][]Value, vals [][]Value, peerAmbiguous bool) error {
	if err := checkWindowFuncArity(call.name, len(call.args)); err != nil {
		return err
	}
	// The PEER-ORDER guard's decline half: this call reads a row's POSITION
	// among its peers and the level really did produce a tie this engine's
	// arrival order broke on its own authority. See window_peer_order.go.
	if peerAmbiguous && windowCallPeerSensitive(call) {
		return errWindowPeerOrder(call.name)
	}
	if call.distinct {
		// DISTINCT is never allowed in a window function, for ANY aggregate and
		// with or without an ORDER BY -- C SQLite rejects "sum(DISTINCT b)
		// OVER ()", and likewise count/avg/group_concat/max, while the same
		// aggregates WITHOUT OVER accept DISTINCT normally (all verified
		// directly). This engine used to compute an answer for every one of
		// them, which is a wrong answer rather than a gap: the statement is one
		// C SQLite refuses to run at all.
		return semanticf("DISTINCT is not supported for window functions")
	}
	for _, part := range parts {
		ends, err := m.windowPeerEnds(plan, call, part, batch, vals)
		if err != nil {
			return err
		}
		// frameSpans is computed LAZILY, only for the functions that actually
		// read the frame: C SQLite does not even validate the frame's
		// offsets for one that doesn't, so "row_number() OVER (ORDER BY a ROWS
		// BETWEEN -1 PRECEDING AND CURRENT ROW)" succeeds where the same frame
		// under sum() is "frame starting offset must be a non-negative
		// integer" (verified directly).
		frameSpans := func() ([]windowFrameSpan, error) {
			return m.windowFrameSpans(plan, call, part, ends, batch, vals)
		}
		switch call.name {
		case "row_number":
			for k, ri := range part {
				vals[ri][ci] = Value{Typ: Int, I: int64(k + 1)}
			}
		case "rank":
			// Every row in a peer group takes the group's FIRST position.
			start := 0
			for k := range part {
				if k == 0 || ends[k-1] != ends[k] {
					start = k
				}
				vals[part[k]][ci] = Value{Typ: Int, I: int64(start + 1)}
			}
		case "dense_rank":
			d := 0
			for k := range part {
				if k == 0 || ends[k-1] != ends[k] {
					d++
				}
				vals[part[k]][ci] = Value{Typ: Int, I: int64(d)}
			}
		case "cume_dist":
			for k := range part {
				vals[part[k]][ci] = Value{Typ: Float, F: float64(ends[k]) / float64(len(part))}
			}
		case "percent_rank":
			start := 0
			for k := range part {
				if k == 0 || ends[k-1] != ends[k] {
					start = k
				}
				if len(part) == 1 {
					vals[part[k]][ci] = Value{Typ: Float, F: 0}
					continue
				}
				vals[part[k]][ci] = Value{Typ: Float, F: float64(start) / float64(len(part)-1)}
			}
		case "ntile":
			if err := m.windowNtile(plan, call, ci, part, batch, vals); err != nil {
				return err
			}
		case "lead", "lag":
			if err := m.windowLeadLag(plan, call, ci, part, batch, vals); err != nil {
				return err
			}
		case "first_value", "last_value", "nth_value":
			spans, serr := frameSpans()
			if serr != nil {
				return serr
			}
			if err := m.windowFrameValue(plan, call, ci, part, spans, batch, vals); err != nil {
				return err
			}
		default:
			// An ordinary aggregate used as a window function: accumulate over
			// this row's FRAME -- the explicit one if the window declared it,
			// else SQLite's default (through the current row's last peer, or
			// the whole partition when the window has no ORDER BY). See
			// vdbe_window_frame.go.
			spans, serr := frameSpans()
			if serr != nil {
				return serr
			}
			if err := m.windowAggregate(plan, call, ci, part, spans, batch, vals, peerAmbiguous); err != nil {
				return err
			}
		}
	}
	return nil
}

// windowAggregate computes an aggregate window function over each row's own
// FRAME (spans, from windowFrameSpans) for one ordered partition.
func (m *vdbe) windowAggregate(plan *windowPlan, call windowCall, ci int, part []int, spans []windowFrameSpan, batch [][]Value, vals [][]Value, peerAmbiguous bool) error {
	fc := call.funcExpr()
	if !isAggregateCall(fc) {
		return fmt.Errorf("%w: window function %s()", errVDBEUnsupported, call.name)
	}
	if len(part) == 0 {
		return nil
	}
	// The template is planned ONCE per partition rather than once per row: it
	// carries no running state (cloneAggTemplates makes that per row below), and
	// stamping it once is what lets every clone inherit the batch columns its
	// per-row expressions were compiled into -- rowRegs rides along through the
	// clone exactly as it does for a GROUP BY's per-group accumulators.
	tmpl, err := planAggregateCall(fc)
	if err != nil {
		return declineOrSemantic(err)
	}
	stampWindowAggRegs(tmpl, call)
	// The PEER-ORDER guard's ARMING half (window_peer_order.go): a peer-based
	// frame is the same set of rows in either arrival order, but the order they
	// STEP in is not, and group_concat appends in that order while sum's
	// rounding depends on it. orderStrict hands the question to the
	// accumulator's own per-value risk latch (aggItem.orderRiskErr,
	// sql_agg.go), which declines only the frames that really did depend on it
	// -- the same trade armOrderSensitive makes for a non-window aggregate.
	tmpl.orderStrict = tmpl.orderStrict || peerAmbiguous
	// See aggItem.minMaxLastWins: a window min()/max() over a frame that does
	// not start at UNBOUNDED PRECEDING reads its answer off an ephemeral
	// index whose tie-break is the LAST value added, not the first -- unless
	// an EXCLUDE clause puts the frame back on the plain accumulator.
	tmpl.minMaxLastWins = windowMinMaxLastWins(call.spec.Frame)
	// A SUBTYPE aggregate's arguments are computed HERE, per stepped row, by a
	// program of their own -- see stepArgs and windowStepArgsLowerable. The
	// machine is built once per partition (the batch can be large) exactly as
	// windowFinal builds the projection's.
	var (
		stepPM   *vdbe
		stepRegs []Value
	)
	if call.stepArgs != nil {
		stepPM = call.stepArgs.machine(m)
		stampWindowStepArgRegs(tmpl, plan, call)
	}
	// Recomputing the accumulator per row is O(n^2) in a partition; acceptable
	// here (and always correct), since the alternative -- an incremental
	// accumulator -- cannot express every aggregate's removal step, and a frame
	// that moves BACKWARD (an EXCLUDE hole, a "<N> FOLLOWING" start) has no
	// removal step to make.
	// ponytail: O(n^2) per partition, revisit if window queries get hot.
	for k := range part {
		acc := cloneAggTemplates([]*aggItem{tmpl})[0]
		for j := spans[k].lo; j < spans[k].hi; j++ {
			if !frameRowIncluded(spans[k], call.spec.Frame, k, j) {
				continue
			}
			ri := part[j]
			regs := plan.operandVals(batch[ri])
			if stepPM != nil {
				// C's windowAggStep tests the BUFFERED filter column first and
				// its OP_IfNot jumps past the whole argument coding
				// (window.c:1694-1696, landed at window.c:1758), so a
				// filtered-out row never evaluates the argument list. Skipping
				// the row entirely is the same thing: aggItem.step reads the
				// very same column and would skip it too.
				if call.filterCol >= 0 {
					fv, ferr := m.windowOperand(plan, batch[ri], call.filterCol)
					if ferr != nil {
						return ferr
					}
					if !isTruthy(fv) {
						continue
					}
				}
				av, aerr := call.stepArgs.run(stepPM, batch[ri], nil)
				if aerr != nil {
					return aerr
				}
				stepRegs = append(append(stepRegs[:0], regs...), av...)
				regs = stepRegs
			}
			ctx := plan.windowRowCtx(m, batch[ri], vals[ri])
			if err := acc.step(ctx, regs); err != nil {
				return err
			}
		}
		v, err := acc.finalize()
		if err != nil {
			return err
		}
		vals[part[k]][ci] = v
	}
	return nil
}

// stampWindowAggRegs points an aggregate window call's accumulator at the batch
// columns the scan body computed its argument and FILTER into, the same seam
// the grouped path uses (planAggArgRegs/aggItem.rowRegs). C reads them with
// OP_Column (window.c:1681, 1694) before OP_AggStep (window.c:1749).
//
// The mapping is positional: windowAggArgsLowerable allows only names for which
// planAggregateCallKind sets expr=Args[0] and (group_concat/string_agg)
// sepExpr=Args[1]. A non-lowered operand is -1, which +1 makes rowRegs' "not
// lowered" zero.
//
// The JSON subtype is not stripped here: a batch entry keeps it (OpMakeRecord
// copies with copyValue), unlike a grouped row through the sorter.
func stampWindowAggRegs(tmpl *aggItem, call windowCall) {
	tmpl.rowRegs = [3]int{}
	tmpl.rowRegs[aggExprArg] = call.argCol(0) + 1
	tmpl.rowRegs[aggExprSep] = call.argCol(1) + 1
	tmpl.rowRegs[aggExprFilter] = call.filterCol + 1
}

// stampWindowStepArgRegs points a SUBTYPE aggregate's accumulator at the two
// slots windowAggregate appends AFTER the batch entry's operand block: the
// values its stepArgs program computed for the row being stepped.
//
// The mapping is the same positional one stampWindowAggRegs uses and is
// provable for the same reason (windowStepArgsLowerable's allow-list, whose two
// names planAggregateCallKind gives "expr: fc.Args[0]" and -- for
// json_group_object -- "sepExpr: fc.Args[1]", refusing any other arity). The
// registers are 1-BIASED like every other rowRegs entry, so plan.nOps+1 names
// index plan.nOps, the first slot past operandVals' own width.
func stampWindowStepArgRegs(tmpl *aggItem, plan *windowPlan, call windowCall) {
	tmpl.rowRegs[aggExprArg] = plan.nOps + 1
	if len(call.args) > 1 {
		tmpl.rowRegs[aggExprSep] = plan.nOps + 2
	}
}

// operandVals is one batch entry's OPERAND block -- the columns
// planWindowOperands appended after the [cols.., rowids..] payload, indexed by
// the same operand column number vdbe.windowOperand reads.
func (p *windowPlan) operandVals(entry []Value) []Value {
	base := p.nCols + p.nRowids
	if base >= len(entry) {
		return nil
	}
	return entry[base:]
}

// compileNoFromWindow compiles a FROM-less window SELECT ("SELECT sum(44) OVER
// ()", or the correlated inner query of "SELECT (SELECT sum(a) OVER (ORDER BY
// a)) FROM t1"): compileScanWindow over zero sources, whose body runs once, so
// the batch holds one zero-column entry, a one-row partition.
//
// That is C's answer: "SELECT sum(44) OVER ()" is 44; count(*), row_number(),
// ntile(1), rank() OVER (ORDER BY 1) and cume_dist() are 1; percent_rank() is
// 0; lead(44) is NULL; lag(7,1,99) is 99; "WHERE 0" and "LIMIT 1 OFFSET 1" give
// no rows. Per outer row through a subquery: over t1(a)=1,2,3, "SELECT (SELECT
// sum(a) OVER (ORDER BY a)) FROM t1" is 1,2,3.
//
// A bare aggregate beside the window goes to compileFromlessAggWindow.
func compileNoFromWindow(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler, trig *trigCompileCtx, rowOuter *evalCtx) (*Program, error) {
	for _, sc := range stmt.Columns {
		if sc.Star {
			return nil, fmt.Errorf("%w: SELECT *", errVDBEUnsupported)
		}
	}
	if aggs := fromlessWindowAggregates(stmt); len(aggs) > 0 {
		return compileFromlessAggWindow(pager, stmt, aggs, outer, trig, rowOuter)
	}
	for _, ot := range stmt.OrderBy {
		if containsAggregate(ot.Expr) {
			return nil, fmt.Errorf("%w: aggregate combined with a window function without FROM", errVDBEUnsupported)
		}
	}
	if containsAggregate(stmt.Where) {
		return nil, fmt.Errorf("%w: aggregate combined with a window function without FROM", errVDBEUnsupported)
	}
	// Same result-set-alias rule, and same trigger-body exception, as the
	// non-window FROM-less compiler applies (compileSelectNoFromTrig).
	if trig == nil {
		stmt = substituteResultAliases(stmt, nil, nil, pager)
	}
	outCols, err := expandSelectList(stmt.Columns, nil, pager.colNameMode())
	if err != nil {
		return nil, err
	}
	c := &compiler{pager: pager, outer: outer, trig: trig, rowOuter: rowOuter}
	// A window function's codegen gathers into an ephemeral table and replays
	// it (sqlite3WindowCodeStep), not the plain "WHERE and select list share
	// one body window" shape compileScanPlain/compileScanSorted rely on -- so
	// nQueryLoop is left distrusted for anything compiled beneath this
	// compiler, exactly like an aggregate/GROUP BY compile (see
	// compiler.nQueryLoopKnown).
	c.nQueryLoopKnown = false
	return compileScanWindow(c, stmt, nil, nil, outCols)
}

// fromlessWindowAggregates collects every bare aggregate call in a FROM-less
// window statement's select list, including inside a window's own PARTITION BY
// / ORDER BY (which containsAggregate skips). Window calls are excluded
// (isAggregateCall), and so are aggregates inside subqueries (walkExprShallow).
// Under-collecting only declines; over-collecting would hoist an aggregate C
// leaves alone, so an unreferenced "WINDOW <name> AS (...)" is not walked.
func fromlessWindowAggregates(stmt *SelectStmt) []FuncExpr {
	var out []FuncExpr
	var walk func(Expr)
	walk = func(e Expr) {
		walkExprShallow(e, func(fc FuncExpr) bool {
			if fc.Over != nil {
				spec := fc.Over
				if spec.Ref != "" {
					r, err := resolveWindowSpec(spec, stmt.Windows, 0)
					if err != nil {
						// Leave the reference to compileScanWindow's own
						// resolve, which reports it with SQLite's wording.
						return true
					}
					spec = r
				}
				for _, pe := range spec.PartitionBy {
					walk(pe)
				}
				for _, ot := range spec.OrderBy {
					walk(ot.Expr)
				}
				return true
			}
			if isAggregateCall(fc) {
				out = append(out, fc)
			}
			return true
		})
	}
	for _, sc := range stmt.Columns {
		if !sc.Star {
			walk(sc.Expr)
		}
	}
	return out
}

// compileFromlessAggWindow compiles a FROM-less window SELECT that also has
// bare aggregates. With no FROM, SQLite's association rule
// (sqlite3ReferencesSrcList; checkAggregateAssociation, vdbe_agg_hoist.go)
// splits them:
//
//   - an aggregate naming no column stays here: compileScanGroupedWindow's
//     two-stage shape over the one synthetic row. Over
//     t1(x,y,z)=(1000,2000,3000),(7,8,9), "SELECT x, (SELECT sum(1) + count(*)
//     OVER ()) FROM t1" is two rows of 2.
//   - an aggregate naming a column belongs to an enclosing query, collapsing
//     it to one row: over t1(a)=(1,2,3), "SELECT (SELECT sum(a) + count(a)
//     OVER ()) FROM t1" is one row (7), and "SELECT a, (SELECT max(a) +
//     row_number() OVER ()) FROM t1" is 3|4. hoistAggregatesOutward records it
//     against the owning compile and fails this one; that compile restarts as
//     an aggregate query, rewriteExprOuterRefs substitutes the finalized
//     value, and this body comes back with no aggregate.
//
// At top level an aggregate naming a column surfaces "no such column".
//
// A subquery argument is classified by checkAggregateAssociation
// (aggArgHasFromlessSubqueryColumnRef): a column reached through a FROM-less
// subquery is outward. Over t1(x,y)=(1,10),(2,20), t2(b)=(5),(6):
//
//	SELECT x, (SELECT sum((SELECT t1.y))          FROM t2) FROM t1  -> ONE row
//	SELECT x, (SELECT sum((SELECT t1.y FROM t2))  FROM t2) FROM t1  -> ONE row
//	SELECT x, (SELECT sum((SELECT b     FROM t2)) FROM t2) FROM t1  -> two rows
//	SELECT x, (SELECT sum((SELECT t1.y + b))      FROM t2) FROM t1  -> two rows
//
// (rowvalue.test 30.1 hoists, or declines where hoisting cannot place it).
func compileFromlessAggWindow(pager *ReadOnlyPager, stmt *SelectStmt, aggs []FuncExpr, outer *compiler, trig *trigCompileCtx, rowOuter *evalCtx) (*Program, error) {
	items := make([]*aggItem, 0, len(aggs))
	for _, fc := range aggs {
		it, err := planAggregateCall(fc)
		if err != nil {
			return nil, declineOrSemantic(err)
		}
		items = append(items, it)
	}
	// scopes is nil because this query has no FROM: sqlite3ReferencesSrcList can
	// never answer 1 for it. rowOuter is passed only so the decline is phrased as
	// the correlated one when there is an enclosing row scope.
	err := checkAggregateAssociation(items, nil, rowOuter, outer)
	if err == nil {
		return compileScanGroupedWindow(pager, stmt, nil, nil, outer)
	}
	if ae, ok := asAggAssociationError(err); ok && trig == nil && hoistAggregatesOutward(outer, stmt, ae) {
		return nil, fmt.Errorf("%w: aggregate belongs to an enclosing query", errVDBEUnsupported)
	}
	return nil, declineOrSemantic(err)
}

// compileScanWindow compiles a window SELECT: the join/WHERE scan skeleton,
// whose body appends each surviving row's [cols.., rowids..] record to the
// batch (OpWindowAppend), then one OpWindowFinal (windowFinal). Frames are in
// vdbe_window_frame.go. A window call anywhere but the select list or ORDER
// BY is declined.
//
// A window with GROUP BY is compiled by compileScanGroupedWindow
// (vdbe_window_group.go), which turns the groups into a derived table and
// calls this with it as the FROM; the guard below catches the leftover
// "HAVING without GROUP BY" and a caller that skipped the rewrite.
func compileScanWindow(c *compiler, stmt *SelectStmt, srcs []joinSource, scopes []tableScope, outCols []outputColumn) (*Program, error) {
	// OpWindowFinal's batch-then-resolve shape runs the window's own result
	// computation OUTSIDE the scan's WHERE loop body (after every row has been
	// appended), the same asymmetry compileScanAggregate/compileScanGroupBy
	// guard against. See compiler.nQueryLoopKnown.
	preNQL, preNQLKnown := c.nQueryLoop, c.nQueryLoopKnown
	c.nQueryLoopKnown = false
	if len(stmt.GroupBy) > 0 || stmt.Having != nil {
		return nil, fmt.Errorf("%w: window function combined with GROUP BY", errVDBEUnsupported)
	}

	// A bare aggregate in the window query's own ORDER BY is "misuse of
	// aggregate: <name>()" (disallowAggregatesInOrderByCb, window.c:942-949),
	// run when the query is not itself aggregate:
	//
	//	if( (p->selFlags & SF_Aggregate)==0 ){
	//	  w.xExprCallback = disallowAggregatesInOrderByCb;
	//	  w.xSelectCallback = 0;
	//	  sqlite3WalkExprList(&w, p->pOrderBy);
	//	}                                        -- window.c:987-991
	//
	// Every query here qualifies: SF_Aggregate comes only from result-set
	// aggregates or GROUP BY (resolve.c:1977-1984), both routed to
	// compileScanGroupedWindow, whose rewrite leaves no aggregates here.
	//
	// walkExprShallow skips subqueries, as C does with a null xSelectCallback
	// (walker.c:209), so "ORDER BY (SELECT sum(v) FROM u)" is fine in both. Unlike
	// C (walker.c:88-89), it also stops at an OVER clause; an aggregate inside a
	// window's own spec is delegated further down, not rejected. The FROM-less
	// spelling declines earlier in compileNoFromWindow.
	for _, ot := range stmt.OrderBy {
		if fc, ok := firstBareAggregate(ot.Expr); ok {
			return nil, semanticf("misuse of aggregate: %s()", fc.Name)
		}
	}

	plan := &windowPlan{
		scopes:   scopes,
		nCols:    totalCols(srcs),
		nRowids:  len(srcs),
		limit:    stmt.Limit,
		offset:   stmt.Offset,
		distinct: stmt.Distinct,
		// The SAME distinctCollations (vdbe_scan.go) the ordinary row-mode
		// scan gives OpDistinct's P4, over the SAME outCols. c.scopes is
		// already this query's FROM at every one of this function's three
		// call sites (vdbe_scan.go, vdbe_window_group.go, and the
		// FROM-less compiler's nil), so a column's DECLARED collation
		// resolves here exactly as it does there.
		distinctColls: distinctCollations(c, outCols),
	}
	for _, oc := range outCols {
		plan.outs = append(plan.outs, rewriteWindowCalls(oc.expr, &plan.calls))
	}
	for oi, ot := range stmt.OrderBy {
		// A bare ORDINAL ("ORDER BY 2") names an OUTPUT column, not the
		// constant 2 -- evaluating it as an expression yielded the same value
		// for every row and left the result unsorted (window1.test's
		// "SELECT a, sum(b) OVER (ORDER BY a) AS abc FROM t1 ORDER BY 2").
		if n, isOrd := orderByOrdinal(stripOrderCollate(ot.Expr)); isOrd {
			if n < 1 || int(n) > len(outCols) {
				return nil, semanticf("%s ORDER BY term out of range - should be between 1 and %d",
					sqliteOrdinalWord(oi+1), len(outCols))
			}
			plan.orderOrdinal = append(plan.orderOrdinal, int(n)-1)
			plan.orderExprs = append(plan.orderExprs, nil)
		} else if idx := outputAliasIndex(outCols, stripOrderCollate(ot.Expr)); idx >= 0 {
			// A bare ORDER BY name matching an output ALIAS names that output
			// column, which is the only way to sort by a window function's
			// result: "SELECT a, sum(b) OVER (ORDER BY a) AS abc FROM t1
			// ORDER BY abc" (verified directly) used to fail "no such column".
			plan.orderOrdinal = append(plan.orderOrdinal, idx)
			plan.orderExprs = append(plan.orderExprs, nil)
		} else {
			plan.orderOrdinal = append(plan.orderOrdinal, -1)
			plan.orderExprs = append(plan.orderExprs, rewriteWindowCalls(ot.Expr, &plan.calls))
		}
		plan.orderTerms = append(plan.orderTerms, ot)
		plan.orderDesc = append(plan.orderDesc, ot.Desc)
		plan.orderNulls = append(plan.orderNulls, ot.Nulls)
	}
	if len(plan.calls) == 0 {
		return nil, fmt.Errorf("%w: window function outside the select list / ORDER BY", errVDBEUnsupported)
	}
	for ci := range plan.calls {
		// Resolve each call's base-window REFERENCE ("OVER win", "OVER (win
		// ORDER BY ...)") against the statement's WINDOW clause FIRST, so
		// everything below -- the frame check, the emit-order spec comparison,
		// partitioning -- sees the effective spec. See resolveWindowSpec.
		spec, rerr := resolveWindowSpec(plan.calls[ci].spec, stmt.Windows, 0)
		if rerr != nil {
			return nil, semanticf("%v", rerr)
		}
		plan.calls[ci].spec = spec
	}
	for _, call := range plan.calls {
		if err := checkWindowFrame(call.spec); err != nil {
			return nil, semanticf("%v", err)
		}
		if !isWindowFunctionName(call.name) {
			if !isAggregateCall(call.funcExpr()) {
				return nil, fmt.Errorf("%w: window function %s()", errVDBEUnsupported, call.name)
			}
		}
		// An aggregate inside the window's own PARTITION BY / ORDER BY ("OVER
		// (ORDER BY sum(...))") is evaluated against the enclosing aggregate
		// query's groups first, a two-stage ordering not modelled here
		// (window1.test 42.x); declined.
		//
		// A per-row subquery there is different: exprHasAggregate does not
		// descend into it, so "ORDER BY (SELECT sum(y) FROM t2)" is a per-row
		// scalar key whose aggregate belongs to the subquery. A subquery nesting
		// another window is also a per-row scalar: sqlite3WindowRewrite appends
		// spec expressions verbatim to the sub-select (exprListAppendList,
		// pruning at nested Selects), and the inner window runs with its
		// subquery.
		//
		// What does matter is an outward aggregate inside such a subquery
		// (window1.test 42.4, t1(b, x) empty):
		//
		//	SELECT sum(b) OVER (ORDER BY (SELECT max(b) OVER (ORDER BY
		//	  sum((SELECT x AS c UNION SELECT 1234 ORDER BY c))) AS e
		//	  ORDER BY e)) FROM t1
		//
		// C answers one NULL row: the sum() names t1.x through a FROM-less
		// subquery, so it re-associates here and makes this an aggregate query,
		// which emits a row over an empty table. specOutwardAggInSubquery
		// detects that shape. compat-harness/window_r24_nested_spec_test.go
		// (mined statements, many spellings, 370 random nested-spec queries) and
		// window_r_correlated_where_test.go cover what is served, including spec
		// subqueries correlated two levels out.
		for _, e := range append(append([]Expr(nil), call.spec.PartitionBy...), orderTermExprs(call.spec.OrderBy)...) {
			if exprHasAggregate(e) {
				// A real FROM with no GROUP BY is a whole-table aggregate, the
				// single-group case of compileScanGroupedWindow's shape, so delegate:
				// "SELECT sum(b) OVER (ORDER BY sum(c)) FROM t" answers the group's
				// anchor-row sum, as sum(b) OVER () does. (The len check is defensive.)
				// Only at top level (no enclosing compiler, live outer row or trigger
				// context): the rewrite has not been verified for a correlated
				// aggregate argument reaching through this compile's outer chain
				// (compat-harness/fromless_agg_window_test.go).
				if len(stmt.GroupBy) == 0 && len(srcs) > 0 && c.outer == nil && c.rowOuter == nil && c.trig == nil {
					return compileScanGroupedWindow(c.pager, stmt, srcs, scopes, c.outer)
				}
				return nil, fmt.Errorf("%w: aggregate inside a window's own ORDER BY/PARTITION BY", errVDBEUnsupported)
			}
			if specOutwardAggInSubquery(c.pager, e, scopes) {
				// The same delegation, which also settles what specOutwardAggInSubquery
				// can only approximate: compileScanGroupedWindow lifts the spec term
				// into a derived query over this statement's own FROM/WHERE/GROUP BY/
				// HAVING (groupWindowHoist.specTerm), the sub-select C builds
				// (window.c:968-971, 1029-1030, 1068), so whether the subquery's
				// aggregate re-associates is decided by the ordinary compile, as
				// resolve.c decides it (resolve.c:1356-1360). Over an empty and a
				// two-row t1(b,x):
				//
				//	SELECT sum(b) OVER (ORDER BY (SELECT sum(x)))       FROM t1
				//	  -> 1 row (NULL / 10): the sum re-associates here
				//	SELECT sum(b) OVER (ORDER BY (SELECT sum(x) FROM t1)) FROM t1
				//	  -> 0 rows / 2 rows (30,30): the sum is the subquery's
				//
				// specOutwardAggInSubquery says yes to both; the rewrite gets both
				// right (window1.test 63.3).
				if len(stmt.GroupBy) == 0 && len(srcs) > 0 && c.outer == nil && c.rowOuter == nil && c.trig == nil {
					return compileScanGroupedWindow(c.pager, stmt, srcs, scopes, c.outer)
				}
				return nil, fmt.Errorf("%w: aggregate inside a subquery of a window's own ORDER BY/PARTITION BY that re-associates with this query", errVDBEUnsupported)
			}
		}
	}

	// bindAggOuterRefs marks this compile correlated for every outward
	// reference, so the enclosing sub-Program is not run once and cached
	// (runSubOnce has no parent frame for OpOuterColumn/OpOuterAggReg to
	// read). The projection now emits its own marking; this pass's
	// remaining job is the decline below, a clean compile-time error for a
	// reference no enclosing scope can serve.
	//
	// A failed bind declines: over o(a,b)=(1,NULL),(2,NULL), inr(x)=(1),(2),(2),
	//
	//	UPDATE o SET b = (SELECT sum(x) OVER (PARTITION BY o.a) FROM inr LIMIT 1)
	//
	// sets both rows to 5 in C, but the UPDATE holds its target row in
	// registers (compiler.regScopes), which bindOuterAggRef cannot walk, so
	// the program would fail at run time. The write path then takes a route
	// that can deliver it (TestAggFilterCorrelatedWrite). The read spelling
	// binds fine and answers 5,5.
	var refs []Expr
	refs = append(refs, plan.outs...)
	refs = append(refs, plan.orderExprs...)
	for _, call := range plan.calls {
		refs = append(refs, call.args...)
		refs = append(refs, call.filter)
		refs = append(refs, call.spec.PartitionBy...)
		refs = append(refs, orderTermExprs(call.spec.OrderBy)...)
	}
	// An AMBIGUOUS name (hardErr) is deliberately NOT a decline: C SQLite
	// rejects it too, and the run-time arm words it the way SQLite words it,
	// where a decline would reword it as a capability gap. Only a reference
	// that resolves NOWHERE this compile can reach declines -- see
	// bindAggOuterRefsReason.
	if ok, hardErr := bindAggOuterRefsReason(c, nil, true, refs...); !ok && !hardErr {
		return nil, fmt.Errorf("%w: window operand correlated to an enclosing query this compiler cannot reach", errVDBEUnsupported)
	}

	// The PEER-ORDER guard's COMPILE half (window_peer_order.go): is the order
	// rows ARRIVE in the batch provably the order C SQLite feeds its own
	// window sorter? Arming here costs nothing on its own -- windowPeerAmbiguous
	// decides at run time whether the rows actually tied.
	pathProvable := windowScanPathProvable(c.pager, c, srcs, scopes, stmt, plan.calls)
	plan.orderStrict = !(pathProvable && anchorLoopOrderProvable(srcs, scopes, stmt.Where))
	if plan.orderStrict && pathProvable && windowPlainInnerSources(srcs) {
		plan.loopOnlyStrict = true
		plan.srcColOfs = make([]int, len(srcs)+1)
		for i, s := range srcs {
			plan.srcColOfs[i+1] = plan.srcColOfs[i] + len(s.tbl.cols)
		}
	}

	// The outer projection (select list plus non-ordinal ORDER BY terms)
	// becomes one compiled program over a register block with the batch row
	// and window results (compileWindowProjection), as C codes both lists
	// against the ephemeral cursor (window.c:1021-1022) with each window node
	// being its regResult (expr.c:5358-5360). A projection that does not
	// compile is the statement's error (RULE #1).
	buildWindowProjList(plan)
	// The projection is C's selectInnerLoop, coded after window.c:3039's
	// sqlite3WhereEnd has restored the PRE-loop nQueryLoop (where.c:7881), so a
	// subquery in it is planned under this query's own starting value.
	plan.projNQL, plan.projNQLKnown = preNQL, preNQLKnown
	proj, projErr := compileWindowProjection(c, plan)
	if projErr != nil {
		return nil, projErr
	}
	plan.proj = proj

	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)

	// The frame bounds' offsets are coded ONCE, ahead of the scan: they are
	// constant expressions with no row in scope, which is exactly why real
	// SQLite codes them into registers of their own (window.c:2940/:2944)
	// rather than into the buffered row. See planWindowFrameOffsets.
	if err := planWindowFrameOffsets(c, plan); err != nil {
		return nil, err
	}

	// Every OTHER operand -- each spec's PARTITION BY / ORDER BY keys and each
	// positional function's arguments -- becomes a COLUMN of the batch entry,
	// which is sqlite3WindowRewrite's own shape (window.c:1029-1047). The
	// register block is reserved HERE, once, because emitJoinLoops emits the
	// body below more than once for a LEFT/FULL JOIN.
	opExprs, operr := planWindowOperands(c.pager, plan)
	if operr != nil {
		return nil, operr
	}

	colBase := c.allocN(plan.nCols)
	rowidBase := c.allocN(len(srcs))
	opBase := c.allocN(plan.nOps)
	recReg := c.allocRec()

	jplan := joinPushdownPlan(srcs, scopes, stmt.Where)
	annotateJoinSeeks(c, srcs, jplan)
	deferredWhere := andConjuncts(jplan.deferred)

	body := func() error {
		whereJump := -1
		if deferredWhere != nil {
			// deferredWhere is andConjuncts of plan.deferred -- itself a set of
			// genuine top-level WHERE AND-conjuncts (splitTopLevelAnd, join.go)
			// recombined into one AND tree -- see compileScanAggregate's
			// identical guard (vdbe_agg_codegen.go) and
			// compiler.inWhereConjunct's doc comment. A window query's own scan
			// loop is built by emitJoinLoops exactly like a plain/aggregate
			// scan's, so the same buckets-vs-deferred split applies: a LEFT/
			// RIGHT/FULL join or parenthesized group forces a row-value/
			// subquery WHERE conjunct here instead of into a per-level bucket
			// (planJoinPushdown's mentionsSubquery forceLast rule, join.go).
			c.inWhereConjunct = true
			wReg, werr := c.compileExpr(deferredWhere)
			if werr != nil {
				return werr
			}
			whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
		}
		for _, s := range srcs {
			for j := 0; j < len(s.tbl.cols); j++ {
				c.emit(Instruction{Op: OpColumn, P1: s.scope.cursor, P2: j, P3: colBase + s.scope.offset + j})
			}
		}
		for i, s := range srcs {
			c.emit(Instruction{Op: OpRowid, P1: s.scope.cursor, P2: rowidBase + i})
		}
		if operr := emitWindowOperands(c, opBase, opExprs, plan); operr != nil {
			return operr
		}
		c.emit(Instruction{Op: OpMakeRecord, P1: colBase, P2: plan.nCols + len(srcs) + plan.nOps, P3: recReg})
		c.emit(Instruction{Op: OpWindowAppend, P1: recReg})
		if whereJump >= 0 {
			c.patch(whereJump, c.here())
		}
		return nil
	}
	// The body runs with every cursor positioned on the current row, so a
	// correlated subquery beneath it may read them (compiler.rowLive), as
	// compileScanPlain, compileScanSorted and compileScanAggregate also
	// declare. Without it, e.g.
	//
	//	SELECT a, sum(a) OVER () FROM tx WHERE EXISTS (SELECT 1 FROM map WHERE v=a)
	//
	// declined (window_r_correlated_where_test.go). C has no separate window
	// scan: sqlite3WindowRewrite moves the WHERE (window.c:969, 995) into the
	// generated sub-select over the same FROM,
	//
	//	pSub = sqlite3SelectNew(
	//	    pParse, pSublist, pSrc, pWhere, pGroupBy, pHaving, pSort, 0, 0
	//	);                                            -- window.c:1068-1070
	//
	// coded by ordinary select codegen. Restored afterwards: OpWindowFinal's
	// batch resolution is not row-live.
	savedRowLive := c.rowLive
	c.rowLive = true
	jerr := emitJoinLoops(c, srcs, jplan, body)
	c.rowLive = savedRowLive
	if jerr != nil {
		return nil, jerr
	}

	// A SUBTYPE aggregate's arguments are the one operand family that is NOT a
	// batch column, here for the same reason it is not one in C: they are
	// re-coded at STEP time (pWin->bExprArgs, window.c:1728-1745).
	//
	// Compiled AFTER the loops because it is compiled against the same scan
	// body's register block. compileWindowStepArgs REFUSES a call whose FILTER
	// has no column, since windowAggregate reproduces C's "the filter test
	// jumps past the argument coding" (window.c:1694-1696) by reading exactly
	// that column.
	for ci := range plan.calls {
		plan.calls[ci].stepArgs = compileWindowStepArgs(c, plan, &plan.calls[ci])
	}
	// ...and only now is it known whether every ordinary aggregate's operands
	// are actually SERVED. aggItem.rowValue reads a register or errors
	// (sql_agg.go), so an operand with no column is a run-time failure --
	// see windowAggOperandsServed (vdbe_window_codegen.go), which owns the
	// rule and the three ways a slot goes missing.
	for ci := range plan.calls {
		if err := windowAggOperandsServed(&plan.calls[ci]); err != nil {
			return nil, err
		}
	}

	c.emit(Instruction{Op: OpWindowFinal, P4: plan})
	c.emit(Instruction{Op: OpHalt})

	return &Program{
		Insns:      c.insns,
		NReg:       c.nReg,
		NCursors:   c.nCursor,
		NRecRegs:   c.nRec,
		NSorters:   c.nSorter,
		NSubCache:  c.nSub,
		NResultCol: len(outCols),
		ColNames:   outColNames(outCols),
		Correlated: c.correlated,
	}, nil
}

// specOutwardAggInSubquery reports whether e, one PARTITION BY / ORDER BY key
// of a window spec, contains inside a subquery a bare aggregate whose argument
// names a column of this query's FROM (scopes). resolve.c then re-associates
// the aggregate here (sqlite3ReferencesSrcList answers 1), making the window
// query an aggregate query that emits a row even over an empty table, which
// this path does not model (window1.test 42.4).
//
// A name also in the subquery's own FROM binds there and the aggregate stays;
// aggShadow proves that for base tables, views and item-qualified references.
// For a CTE or derived table a name that merely could be this query's still
// counts (a decline where C would stay). A name not in this query's FROM
// cannot re-associate, so "ORDER BY (SELECT sum(y) FROM t2)" stays served.
//
// It fails safe: an unrecognized node answers yes. exprHasAggregate cannot be
// the leaf test since it stops at a SubqueryExpr.
func specOutwardAggInSubquery(pager *ReadOnlyPager, e Expr, scopes []tableScope) bool {
	return outwardAggExpr(e, scopes, &aggShadow{pager: pager})
}

func outwardAggExpr(e Expr, scopes []tableScope, sh *aggShadow) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr, windowResultExpr:
		return false
	case groupAggExpr, groupKeyExpr, groupBareColExpr:
		// An aggregate-result placeholder is already associated (its owner is
		// C's pExpr->pAggInfo, expr.c:4996/5342), so it cannot re-associate
		// here. Without this arm it hit the fail-safe "yes" and declined
		// window1.test#40, "SELECT (SELECT count(a) OVER (ORDER BY sum(a)) +
		// total(a) OVER()) FROM t1", whose sum(a) is hoisted to the enclosing
		// query.
		return false
	case SubqueryExpr:
		return outwardAggSelect(x.Stmt, scopes, sh)
	case ExistsExpr:
		return outwardAggSelect(x.Stmt, scopes, sh)
	case FuncExpr:
		if x.Over == nil && isAggregateCall(x) && aggArgNamesScopeColumn(x, scopes, sh) {
			return true
		}
		for _, a := range x.Args {
			if outwardAggExpr(a, scopes, sh) {
				return true
			}
		}
		if outwardAggExpr(x.Filter, scopes, sh) {
			return true
		}
		for _, ob := range x.orderByExprs() {
			if outwardAggExpr(ob, scopes, sh) {
				return true
			}
		}
		if x.Over != nil {
			// A window's OWN spec, which walkExprShallow (and so
			// exprHasAggregate) stops at -- and which is exactly where
			// window1.test 42.4 writes the re-associating aggregate. A "OVER
			// <name>" reference's spec lives in its statement's WINDOW clause,
			// walked by outwardAggSelect.
			for _, pe := range x.Over.PartitionBy {
				if outwardAggExpr(pe, scopes, sh) {
					return true
				}
			}
			for _, ot := range x.Over.OrderBy {
				if outwardAggExpr(ot.Expr, scopes, sh) {
					return true
				}
			}
		}
		return false
	case UnaryExpr:
		return outwardAggExpr(x.X, scopes, sh)
	case BinaryExpr:
		return outwardAggExpr(x.L, scopes, sh) || outwardAggExpr(x.R, scopes, sh)
	case IsNullExpr:
		return outwardAggExpr(x.X, scopes, sh)
	case InExpr:
		if x.Sub != nil && outwardAggSelect(x.Sub, scopes, sh) {
			return true
		}
		if outwardAggExpr(x.X, scopes, sh) {
			return true
		}
		for _, a := range x.List {
			if outwardAggExpr(a, scopes, sh) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return outwardAggExpr(x.X, scopes, sh) || outwardAggExpr(x.Lo, scopes, sh) || outwardAggExpr(x.Hi, scopes, sh)
	case LikeExpr:
		return outwardAggExpr(x.X, scopes, sh) || outwardAggExpr(x.Pattern, scopes, sh) || outwardAggExpr(x.Escape, scopes, sh)
	case GlobExpr:
		return outwardAggExpr(x.X, scopes, sh) || outwardAggExpr(x.Pattern, scopes, sh)
	case CollateExpr:
		return outwardAggExpr(x.X, scopes, sh)
	case CastExpr:
		return outwardAggExpr(x.X, scopes, sh)
	case CaseExpr:
		if x.Base != nil && outwardAggExpr(x.Base, scopes, sh) {
			return true
		}
		for _, w := range x.Whens {
			if outwardAggExpr(w.When, scopes, sh) || outwardAggExpr(w.Then, scopes, sh) {
				return true
			}
		}
		return x.Else != nil && outwardAggExpr(x.Else, scopes, sh)
	default:
		// Unrecognized node: assume it may hide one. See the doc comment.
		return true
	}
}

// outwardAggSelect is specOutwardAggInSubquery's statement half: it reports
// whether stmt -- a subquery of a window's own spec, or one nested inside one --
// writes a bare aggregate that re-associates with the query holding scopes.
// It walks every expression position, and recurses through nested subqueries,
// compound arms, derived tables and CTE bodies, because resolve.c's outward
// walk reaches all of them.
func outwardAggSelect(stmt *SelectStmt, scopes []tableScope, sh *aggShadow) bool {
	if stmt == nil {
		return false
	}
	sh = sh.enter(stmt)
	visit := func(e Expr) bool { return outwardAggExpr(e, scopes, sh) }
	for _, c := range stmt.Columns {
		if !c.Star && visit(c.Expr) {
			return true
		}
	}
	for _, it := range stmt.From {
		if it.Subquery != nil && outwardAggSelect(it.Subquery, scopes, sh) {
			return true
		}
		if visit(it.On) {
			return true
		}
	}
	if visit(stmt.Where) || visit(stmt.Having) {
		return true
	}
	for _, g := range stmt.GroupBy {
		if visit(g) {
			return true
		}
	}
	for _, ot := range stmt.OrderBy {
		if visit(ot.Expr) {
			return true
		}
	}
	for _, nw := range stmt.Windows {
		if nw.Spec == nil {
			continue
		}
		for _, pe := range nw.Spec.PartitionBy {
			if visit(pe) {
				return true
			}
		}
		for _, ot := range nw.Spec.OrderBy {
			if visit(ot.Expr) {
				return true
			}
		}
	}
	for _, arm := range stmt.Compound {
		if outwardAggSelect(arm.Stmt, scopes, sh) {
			return true
		}
	}
	for _, cte := range stmt.CTEs {
		if outwardAggSelect(cte.Select, scopes, sh) {
			return true
		}
	}
	return false
}

// aggShadow is the FROM clauses of the subqueries between an aggregate and the
// window query, innermost last. A column reference one of them binds never
// reaches the window query: resolve.c's lookupName stops at the first
// NameContext that answers ("if( cnt ) break;", resolve.c:703, before
// "pNC = pNC->pNext;", :704), so "max(n)" in "(SELECT max(n) FROM b b2 ...)"
// is the subquery's own aggregate whatever the window query's FROM holds.
//
// An item whose columns this cannot learn -- a derived table, a table-valued
// function, a CTE -- still binds a QUALIFIED reference to its own name, but
// shadows no unqualified one: that name then counts as possibly the window
// query's, the declining direction the doc comment above describes.
type aggShadow struct {
	pager  *ReadOnlyPager
	levels [][]aggShadowItem
}

type aggShadowItem struct {
	name  string
	cols  []columnInfo
	rowid bool
}

// enter returns sh with stmt's FROM pushed as the innermost level.
func (sh *aggShadow) enter(stmt *SelectStmt) *aggShadow {
	level := make([]aggShadowItem, 0, len(stmt.From))
	for _, it := range stmt.From {
		item := aggShadowItem{name: it.Alias}
		if item.name == "" {
			item.name = it.Table
		}
		_, isCTE := sh.pager.lookupCTE(it.Table)
		for _, cte := range stmt.CTEs {
			isCTE = isCTE || equalFoldName(cte.Name, it.Table)
		}
		if it.Subquery == nil && !it.TableFunc && it.Table != "" && it.Schema == "" && !isCTE && sh.pager != nil {
			if rt, err := sh.pager.resolveTable(it.Table); err == nil {
				item.cols, item.rowid = rt.cols, !rt.withoutRowid
			} else if pcv, ok, verr := sh.pager.resolveViewByName(it.Table); verr == nil && ok {
				// A view's columns are its body's output names; it has no
				// rowid of its own to shadow.
				if vcols, cerr := sh.pager.viewColumnInfos(it.Table, pcv); cerr == nil {
					item.cols = vcols
				}
			}
		}
		level = append(level, item)
	}
	return &aggShadow{pager: sh.pager, levels: append(slices.Clone(sh.levels), level)}
}

// binds reports whether some level of sh answers ce before the window query
// could.
func (sh *aggShadow) binds(ce ColumnExpr) bool {
	for _, level := range sh.levels {
		for _, it := range level {
			if ce.Qualifier != "" {
				if equalFoldName(ce.Qualifier, it.name) {
					return true
				}
				continue
			}
			for _, c := range it.cols {
				if equalFoldName(c.Name, ce.Name) {
					return true
				}
			}
			if it.cols != nil && it.rowid && isRowidAliasName(ce.Name) {
				return true
			}
		}
	}
	return false
}

// aggArgNamesScopeColumn reports whether any column reference anywhere in fc's
// arguments (or its FILTER), INCLUDING inside their own subqueries, resolves
// against scopes. Descending into those subqueries is the whole point: the
// statement this rule exists for writes "sum((SELECT x AS c UNION SELECT 1234
// ORDER BY c))", where x is the only reference and it lives one subquery down.
// aggArgHasLocalColumnRef (sql_agg.go) stops at a SubqueryExpr and so cannot
// see it.
func aggArgNamesScopeColumn(fc FuncExpr, scopes []tableScope, sh *aggShadow) bool {
	local := &evalCtx{tables: scopes}
	found := false
	var walkE func(Expr)
	var walkS func(*SelectStmt)
	walkE = func(e Expr) {
		if found || e == nil {
			return
		}
		switch x := e.(type) {
		case ColumnExpr:
			if sh.binds(x) {
				break
			}
			if _, _, _, _, err := resolveColumn(local, x.Qualifier, x.Name); err == nil {
				found = true
			}
		case SubqueryExpr:
			walkS(x.Stmt)
		case ExistsExpr:
			walkS(x.Stmt)
		case InExpr:
			walkS(x.Sub)
			walkE(x.X)
			for _, it := range x.List {
				walkE(it)
			}
		case FuncExpr:
			for _, a := range x.Args {
				walkE(a)
			}
			walkE(x.Filter)
			for _, ob := range x.orderByExprs() {
				walkE(ob)
			}
			if x.Over != nil {
				for _, pe := range x.Over.PartitionBy {
					walkE(pe)
				}
				for _, ot := range x.Over.OrderBy {
					walkE(ot.Expr)
				}
			}
		case UnaryExpr:
			walkE(x.X)
		case BinaryExpr:
			walkE(x.L)
			walkE(x.R)
		case IsNullExpr:
			walkE(x.X)
		case BetweenExpr:
			walkE(x.X)
			walkE(x.Lo)
			walkE(x.Hi)
		case LikeExpr:
			walkE(x.X)
			walkE(x.Pattern)
			walkE(x.Escape)
		case GlobExpr:
			walkE(x.X)
			walkE(x.Pattern)
		case MatchExpr:
			walkE(x.X)
			walkE(x.Pattern)
		case CollateExpr:
			walkE(x.X)
		case CastExpr:
			walkE(x.X)
		case CaseExpr:
			walkE(x.Base)
			for _, w := range x.Whens {
				walkE(w.When)
				walkE(w.Then)
			}
			walkE(x.Else)
		case RowExpr:
			for _, it := range x.Elems {
				walkE(it)
			}
		case LiteralExpr, ParamExpr, windowResultExpr:
		default:
			// An unrecognized node might hold a reference this rule must see.
			// Report one, exactly as specOutwardAggInSubquery's own default
			// does: over-reporting costs a decline, under-reporting is wrong.
			found = true
		}
	}
	walkS = func(stmt *SelectStmt) {
		if found || stmt == nil {
			return
		}
		for _, c := range stmt.Columns {
			if !c.Star {
				walkE(c.Expr)
			}
		}
		for _, it := range stmt.From {
			walkE(it.On)
			walkS(it.Subquery)
		}
		walkE(stmt.Where)
		walkE(stmt.Having)
		for _, g := range stmt.GroupBy {
			walkE(g)
		}
		for _, ot := range stmt.OrderBy {
			walkE(ot.Expr)
		}
		for _, arm := range stmt.Compound {
			walkS(arm.Stmt)
		}
		for _, cte := range stmt.CTEs {
			walkS(cte.Select)
		}
	}
	for _, a := range fc.Args {
		walkE(a)
	}
	walkE(fc.Filter)
	for _, ob := range fc.orderByExprs() {
		walkE(ob)
	}
	return found
}

// isWindowFunctionName reports whether name is a ranking/positional function
// that exists only as a window function (as opposed to an aggregate used with
// OVER, which isAggregateCall recognizes).
func isWindowFunctionName(name string) bool {
	switch name {
	case "row_number", "rank", "dense_rank", "cume_dist", "percent_rank",
		"ntile", "lead", "lag", "first_value", "last_value", "nth_value":
		return true
	}
	return false
}

// windowMinMaxLastWins reports whether a min()/max() over this frame resolves
// a TIE to the later value. See aggItem.minMaxLastWins for the C and the
// measurement; the two guards here are window.c's own, "pWin->eStart !=
// TK_UNBOUNDED" (window.c:1428) and "pMWin->regStartRowid==0" (window.c:1700),
// the second of which is set only by an EXCLUDE clause (window.c:1416).
func windowMinMaxLastWins(f *WindowFrame) bool {
	if f == nil {
		return false // the default frame starts at UNBOUNDED PRECEDING
	}
	if f.Exclude {
		return false
	}
	return f.Start.Type != FrameUnboundedPreceding
}
