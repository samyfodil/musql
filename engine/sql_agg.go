// Aggregate functions: the accumulator (aggItem) every aggregate path drives
// (whole-table, GROUP BY, window), its step/finalize rules ported from func.c,
// and the planning shared by those paths (association, argument analysis).
//
// An aggregate's argument list may start with DISTINCT, which dedups that
// aggregate's inputs before accumulation (see step). The 2+-argument scalar
// min/max is an ordinary function; isAggregateCall decides which a call is.
package engine

import (
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"
)

// isAggregateCall reports whether fc is an aggregate call rather than a
// scalar one. count/sum/total/avg/group_concat/string_agg have no scalar form,
// so any call to them is aggregate (string_agg is C's 2-argument alias for
// group_concat; string_agg(X) is "wrong number of arguments"), and a wrong
// argument count is still an aggregate call that planning rejects. min/max are
// aggregate with one argument and scalar with two or more, and the scalar
// form must not make a query an aggregate query.
func isAggregateCall(fc FuncExpr) bool {
	// A window-function call ("<fn>(...) OVER ...") is never a plain aggregate:
	// it has one-row-per-input window semantics handled entirely by
	// sql_window.go, and must not flip a query into GROUP-BY-less aggregate
	// mode (which would collapse the result to a single row).
	if fc.Over != nil {
		return false
	}
	name := r33sFoldIdent(fc.Name)
	switch name {
	case "count", "sum", "total", "avg", "group_concat", "string_agg":
		return true
	case "min", "max":
		return !fc.Star && len(fc.Args) == 1
	case "json_group_array", "json_group_object", "jsonb_group_array", "jsonb_group_object":
		// Real arity (1 for json_group_array, 2 for json_group_object) is
		// validated by planAggregateCall, exactly like count/sum/... above --
		// a wrong argument count is still an aggregate call (just one
		// planAggregateCall rejects), not a reason to fall back to row mode.
		return !fc.Star
	}
	return false
}

// containsAggregate reports whether e's expression tree contains an
// aggregate-function call anywhere -- used at plan time to decide
// aggregate-mode vs. row-mode for the whole query, and to reject an
// aggregate nested inside another aggregate's argument or inside a
// select-list item that isn't itself a top-level aggregate call.
func containsAggregate(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return false
	case FuncExpr:
		if isAggregateCall(x) {
			return true
		}
		for _, a := range x.walkArgs() {
			if containsAggregate(a) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return containsAggregate(x.X)
	case BinaryExpr:
		return containsAggregate(x.L) || containsAggregate(x.R)
	case IsNullExpr:
		return containsAggregate(x.X)
	case InExpr:
		if containsAggregate(x.X) {
			return true
		}
		for _, a := range x.List {
			if containsAggregate(a) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return containsAggregate(x.X) || containsAggregate(x.Lo) || containsAggregate(x.Hi)
	case LikeExpr:
		return containsAggregate(x.X) || containsAggregate(x.Pattern) || containsAggregate(x.Escape)
	case GlobExpr:
		return containsAggregate(x.X) || containsAggregate(x.Pattern)
	case CollateExpr:
		return containsAggregate(x.X)
	case CastExpr:
		return containsAggregate(x.X)
	case CaseExpr:
		if x.Base != nil && containsAggregate(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if containsAggregate(w.When) || containsAggregate(w.Then) {
				return true
			}
		}
		return x.Else != nil && containsAggregate(x.Else)
	// SubqueryExpr is deliberately NOT handled here (falls to default,
	// false): a subquery is its own independent scope, so an aggregate
	// call inside it (e.g. "(SELECT avg(c) FROM t1)") does not make the
	// *enclosing* select-list item an aggregate of the outer query's rows.
	default:
		return false
	}
}

// selectIsAggregateQuery reports whether stmt is an aggregate query in
// SQLite's sense, the SF_Aggregate flag resolveSelectStep (resolve.c) sets
// when "pGroupBy || (sNC.ncFlags & NC_HasAgg)!=0" after resolving the result
// set: a GROUP BY, or an aggregate call written in the select list. Not
// counted (each gives "HAVING clause on a non-aggregate query"):
//
//	SELECT 1 FROM t1 HAVING max(a)>0          -- agg in HAVING alone: no
//	SELECT (SELECT count(*)) FROM t1 HAVING 1 -- agg inside a subquery: no
//	SELECT count(*) OVER () FROM t1 HAVING 1  -- a WINDOW call: no
//	SELECT * FROM t1 HAVING count(*)>0        -- "*" is not an aggregate: no
//
// containsAggregate handles the last two (a windowed call is not an
// aggregate call; Star carries no expression).
func selectIsAggregateQuery(stmt *SelectStmt) bool {
	if len(stmt.GroupBy) != 0 {
		return true
	}
	for _, sc := range stmt.Columns {
		if !sc.Star && containsAggregate(sc.Expr) {
			return true
		}
	}
	return false
}

// requireAggregateForHaving reproduces C SQLite's "HAVING clause on a
// non-aggregate query" rejection for a HAVING clause on a query that has no
// GROUP BY and no aggregate in its result set.
//
// This lives at COMPILE time, not parse time, deliberately: SQLite raises it
// while RESOLVING the statement, so "CREATE VIEW v AS SELECT a FROM t1
// HAVING count(*)>1" is accepted at CREATE time and only errors when the view
// is actually used (verified directly). errVDBESemantic, not
// errVDBEUnsupported: this is a genuine C-SQLite rejection that must surface
// as the statement's own error, never be mistaken for a capability gap.
func requireAggregateForHaving(stmt *SelectStmt) error {
	if stmt.Having == nil || selectIsAggregateQuery(stmt) {
		return nil
	}
	return semanticf("engine: HAVING clause on a non-aggregate query")
}

// aggKind identifies which accumulation rule an aggItem implements.
type aggKind int

const (
	aggCount           aggKind = iota // count(expr): rows where expr is non-NULL
	aggCountStar                      // count(*): all surviving rows
	aggSum                            // sum(expr)
	aggTotal                          // total(expr)
	aggAvg                            // avg(expr)
	aggMin                            // min(expr)
	aggMax                            // max(expr)
	aggGroupConcat                    // group_concat(expr[, sep])
	aggJSONGroupArray                 // json_group_array(expr) -- see engine/json_funcs.go
	aggJSONGroupObject                // json_group_object(key, value) -- see engine/json_funcs.go
)

// aggItem is one aggregate call's execution plan and running state: an
// accumulator fed one value per surviving row by step, then read out by
// finalize.
type aggItem struct {
	kind     aggKind
	expr     Expr // the aggregate's argument (nil for aggCountStar)
	sepExpr  Expr // aggGroupConcat's optional separator expression; nil => default ","
	distinct bool // DISTINCT was written inside the aggregate's argument list
	jsonb    bool // the jsonb_group_array/jsonb_group_object spelling of a JSON kind
	// srcCall is the FuncExpr this template was planned from (planAggregateCall),
	// a PLAN-time-only field: it lets an aggregate that turns out to belong to an
	// ENCLOSING query be re-planned there verbatim. See vdbe_agg_hoist.go.
	srcCall FuncExpr

	// Running state, mutated by step as rows are scanned. Only the fields
	// relevant to kind are used.
	cnt         int64 // non-NULL contributing rows (count/sum/total/avg/group_concat)
	rSum        float64
	rErr        float64 // Kahan-Babuska-Neumaier error term (see kbnStep)
	iSum        int64
	sumApprox   bool // true once any non-integer value has contributed (see finalizeSum)
	sumOverflow bool // true once the int64 running sum has overflowed (see finalizeSum)
	sumTopExp   int  // the exponent window of the values summed: |v| < 2^sumTopExp
	sumUnitExp  int  // and every v is a multiple of 2^sumUnitExp. Maintained only
	sumWinSet   bool // when orderStrict is set -- see sumWatch/sumOrderFree
	best        Value
	haveBest    bool
	gcBuf       strings.Builder
	gcHave      bool
	gcFirstVal  string // the first contribution's text/separator, kept only when
	gcFirstSep  string // orderStrict is set -- see orderRisk

	// orderStrict is armed by the compiler when this query's ROW ORDER is not
	// provably the order C SQLite feeds its own accumulators
	// (anchorPlanOrderProvable, vdbe_agg_codegen.go) AND this accumulator is
	// order-sensitive (orderSensitive). finalize then refuses to answer unless
	// the accumulation TURNED OUT not to depend on the order after all --
	// orderRisk is the per-kind evidence, latched by step.
	orderStrict bool
	orderRisk   bool

	// minMaxLastWins makes a tie keep the later value, as a window
	// min()/max() over a moving frame does. The plain accumulator (minmaxStep,
	// func.c) replaces only on strict improvement, so the first of a tie wins.
	// A frame not starting UNBOUNDED PRECEDING instead uses an ephemeral index
	// keyed (value, counter), value DESC for min() (window.c:1428-1449), read
	// with OP_Last (window.c:1787), so the last row added wins a tie.
	//
	// Exceptions are C's two guards: an UNBOUNDED PRECEDING start, and an
	// EXCLUDE clause (regStartRowid, window.c:1416-1419). Over
	// p(i,v COLLATE NOCASE) = (1,'a'),(2,'B'),(3,'c'),(4,'A'),(5,'b'),(6,'C'):
	//
	//	min(v) OVER (ORDER BY i ROWS BETWEEN CURRENT ROW AND UNBOUNDED
	//	             FOLLOWING)   row 1 -> 'A', the LATER of the a/A tie
	//	min(v) OVER (ORDER BY i)  row 6 -> 'a', the EARLIER (unbounded start)
	minMaxLastWins bool

	// filter is a "FILTER (WHERE <cond>)" modifier's condition, or nil when
	// the call has none. SQLite evaluates it per candidate row BEFORE the
	// value reaches the accumulator, so a filtered-out row contributes
	// nothing at all -- not even to count(*).
	filter Expr

	// seen holds every distinct (per compareValues) non-NULL value already
	// fed into this accumulator, in first-seen order -- only populated/
	// consulted when distinct is true. Verified against the reference engine
	// that DISTINCT dedup happens BEFORE a value reaches the kind-specific
	// accumulation logic (e.g. sum(DISTINCT x) over integer 2 and TEXT '2'
	// contributes both, since they're a different storage class and hence
	// not compareValues-equal, but a second integer 2 contributes only
	// once) -- see step's shared pre-dispatch dedup check below.
	seen []Value

	// argColl caches the collating sequence of the aggregate's ARGUMENT --
	// see argCollation. Resolved on the first contributing row (planning has
	// no evalCtx to resolve a bare column's declared collation against) and
	// reused for the rest of this accumulator's life; the argument expression
	// is fixed, so the answer cannot change row to row.
	argColl    string
	argCollSet bool

	// rowRegs are the registers a compiled scan body left this accumulator's
	// per-row values in, indexed by aggExprFilter/aggExprArg/aggExprSep and
	// biased by one so zero means "not lowered". This ports updateAccumulator,
	// which codes the FILTER with sqlite3ExprIfFalse (select.c:6855) and the
	// arguments, separator included, with sqlite3ExprCodeExprList
	// (select.c:6902-6906) into the range OP_AggStep takes (select.c:6942).
	//
	// The compiler stamps them on the template (planAggArgRegs/emitAggArgRegs)
	// and cloneAggTemplates copies them into each group's clone.
	rowRegs [3]int

	// stepSeg is which of the scan body's step opcodes advances this
	// accumulator (planAggArgRegs/emitAggSteps). C's loop over the aggregates
	// (select.c:6824) codes each one's arguments (:6906) and its OP_AggStep
	// (:6942) before the next aggregate's arguments, so a later aggregate's
	// argument is never evaluated before an earlier one's step, which is
	// observable when both can raise. A compile-time fact copied into every
	// clone; zero (one step opcode per row) is the common case.
	stepSeg int

	// orderBy is the call's own ORDER BY. C gives such an aggregate an ephemeral
	// index (AggInfo_func.iOBTab, expr.c:7509-7534) that updateAccumulator inserts
	// rows into instead of stepping (select.c:6857-6925), and finalizeAggFunctions
	// steps through in key order before OP_AggFinal (select.c:6742-6786). Here
	// that index is obBuf, sorted at finalize.
	//
	// nil for min()/max(): expr.c:7509-7511 sets no iOBTab for a
	// SQLITE_FUNC_NEEDCOLL function, so its ORDER BY is never coded (but its
	// names still resolve; planAggArgRegs checks them).
	orderBy []OrderTerm
	// obUnique is C's bOBUnique (expr.c:7525-7528): DISTINCT, one argument and
	// one ORDER BY term that IS that argument. The index then has no sequence
	// column and its KEY is the dedup, so a later arrival equal to an earlier
	// one OVERWRITES it: over 1 then 1.0, "group_concat(DISTINCT x ORDER BY
	// x)" is '1.0' in 3.53.3, where the payload spelling ("... ORDER BY y")
	// keeps the first arrival like every other DISTINCT.
	obUnique bool
	// obRegs are the ORDER BY keys' registers, biased by one like rowRegs and
	// stamped by planAggArgRegs; a key that IS the argument (C's bOBPayload==0,
	// expr.c:7522-7526) shares the argument's register, since C codes it once.
	obRegs []int
	// obColls is each key's collating sequence -- sqlite3KeyInfoFromExprList
	// over the ORDER BY list (select.c:6716) -- resolved on the first row the
	// way argColl is.
	obColls   []string
	obCollSet bool
	// obBuf holds the admitted rows in ARRIVAL order, which is C's OP_Sequence
	// column (select.c:6886-6888) breaking ties among equal keys.
	obBuf []aggOBRow
	// obCtx is the context the rows were buffered under, for the replay's
	// encoding; nothing else of it is read.
	obCtx *evalCtx
	// obDone marks the buffer as replayed, so a second finalize of the same
	// accumulator reads the accumulated state rather than stepping it again.
	obDone bool
}

// aggOBRow is one entry of an ORDER BY aggregate's buffer: its sort keys and
// the argument values OP_AggStep will be handed (select.c:6766-6779).
type aggOBRow struct {
	keys     []Value
	arg, sep Value
}

// The three per-row expressions an aggItem can have, and rowRegs' index space.
const (
	aggExprFilter = iota // the FILTER (WHERE ...) condition
	aggExprArg           // the aggregate's own argument (it.expr)
	aggExprSep           // group_concat's separator / json_group_object's VALUE (it.sepExpr)
)

// rowExpr is the expression rowRegs' index WHICH names, and the one place the
// mapping lives: the compiler stamps a register for rowExpr(which)
// (planAggArgRegs / emitAggArgRegs, vdbe_agg_codegen.go) and rowValue reads
// that register back for the same which, so the two cannot drift apart.
func (it *aggItem) rowExpr(which int) Expr {
	switch which {
	case aggExprFilter:
		return it.filter
	case aggExprSep:
		return it.sepExpr
	}
	return it.expr
}

// rowValue returns one of this accumulator's per-row values:
// regs[rowRegs[which]-1], computed by a compiled body. An unstamped register is
// an internal inconsistency (the compiler declines what it cannot lower) and
// is reported as an error.
//
// The callers' evaluation order is C's: a filtered-out row evaluates no
// argument (select.c:6855 jumps past :6902-6906), and an admitted row evaluates
// every argument, separator included, even when the value is NULL or a
// DISTINCT duplicate (compat-harness/agg_second_arg_eager_test.go).
//
// regs is whichever slice holds the row's values:
//   - the aggregate opcodes pass the VM's registers (planAggArgRegs);
//   - the window path passes the batch entry's operand block
//     (stampWindowAggRegs);
//   - the sorted GROUP BY drain passes the VM's registers, filled from the
//     sorter record (aggDrainRow; select.c:8704 with useSortingIdx,
//     select.c:8607).
//
// ctx is unused.
func (it *aggItem) rowValue(which int, ctx *evalCtx, regs []Value) (Value, error) {
	if r := it.rowRegs[which]; r > 0 && r <= len(regs) {
		return regs[r-1], nil
	}
	return Value{}, fmt.Errorf("engine: internal: aggregate %s per-row expression %d was never lowered into a register", it.srcCall.Name, which)
}

// orderSensitive reports whether the order rows reach step can change this
// accumulator's finalized bytes (gated like the bare-column anchor, by
// armOrderSensitive / anchorPlanOrderProvable).
//
// C's own flag is SQLITE_FUNC_ANYORDER (count/min/max, sqliteInt.h:2059),
// inverted into NC_OrderAgg (resolve.c:1369-1371; sqliteInt.h:3554), which
// keeps the flattener from dropping a subquery's ORDER BY (select.c:7840).
// Per kind:
//
//   - count: countStep (func.c:2064) only increments; DISTINCT's cardinality
//     does not depend on order. Insensitive.
//   - sum/total/avg: sumStep (func.c:1929) uses order-dependent
//     Kahan-Babuska-Neumaier rounding and an overflow latch a later
//     non-integer clears (func.c:1958); sum over 1e308,1e308,-1e308 is +Inf
//     in one order and 1e308 in another.
//   - group_concat/string_agg: appends in order (func.c:2191), with the
//     separator from the current row (func.c:2217).
//   - json_group_array/json_group_object: the same append (json.c).
//   - min/max: ANYORDER in C, but minmaxStep (func.c:2104) keeps the first of
//     a compare-equal run, and equal is not byte-equal (1 vs 1.0, or texts
//     under NOCASE). Whether a tie occurs depends on the data
//     (compat-harness/aggorder_r29_test.go).
func (it *aggItem) orderSensitive() bool {
	switch it.kind {
	case aggCount, aggCountStar:
		return false
	}
	return true
}

// orderRiskErr is the run-time half: orderSensitive asks whether a kind can
// depend on order; this asks whether the rows actually seen did. Asked only
// when the compiler could not prove the order (orderStrict); only a latched
// risk declines:
//
//   - sum/total/avg: sumOrderFree proves the accumulation exact in any order.
//   - min/max: a tie with the running best that is not byte-identical.
//     Exact: once a member of the final tie group is best nothing displaces
//     it, and an earlier tie can only over-report.
//   - group_concat/string_agg: two contributions differing in value or
//     separator.
//   - json_group_array/json_group_object: the same, on the appended bytes
//     (stepJSONWatched).
//   - any kind: a DISTINCT duplicate dropped that is not byte-identical to
//     the survivor.
func (it *aggItem) orderRiskErr() error {
	if !it.orderStrict {
		return nil
	}
	risky := it.orderRisk
	switch it.kind {
	case aggSum, aggTotal, aggAvg:
		// With its own ORDER BY the accumulation ran in KEY order, so only
		// what obReplay/obBuffer latched -- a tie group, or a DISTINCT
		// survivor -- is left to arrival.
		if it.orderBy == nil {
			risky = !it.sumOrderFree()
		}
	}
	if !risky {
		return nil
	}
	return fmt.Errorf("%w: %s over rows whose ARRIVAL ORDER is not provably C SQLite's (see anchorPlanOrderProvable), and whose accumulation turned out to depend on it", errVDBEUnsupported, aggKindName(it.kind))
}

// aggKindName names a kind for orderRiskErr's message -- the decline bucket is
// read per aggregate, so "group_concat" and "sum" must not share one line.
func aggKindName(k aggKind) string {
	switch k {
	case aggSum:
		return "sum()"
	case aggTotal:
		return "total()"
	case aggAvg:
		return "avg()"
	case aggMin:
		return "min()"
	case aggMax:
		return "max()"
	case aggGroupConcat:
		return "group_concat()/string_agg()"
	case aggJSONGroupArray:
		return "json_group_array()"
	case aggJSONGroupObject:
		return "json_group_object()"
	}
	return "an order-sensitive aggregate"
}

// valuesIdentical reports whether two values are INDISTINGUISHABLE, not merely
// compareValues-equal: same storage class and same bytes. It is the question
// minmaxStep's tie (func.c:2131) leaves open -- sqlite3MemCompare calls INTEGER
// 1 and REAL 1.0 equal, and 'abc' and 'ABC' equal under NOCASE, while the two
// halves of each pair are different ANSWERS. Floats are compared by BIT so that
// 0.0 and -0.0, which "==" calls equal and %g prints differently, count as the
// distinct answers they are.
func valuesIdentical(a, b Value) bool {
	if a.Typ != b.Typ {
		return false
	}
	switch a.Typ {
	case Int:
		return a.I == b.I
	case Float:
		return math.Float64bits(a.F) == math.Float64bits(b.F)
	case Text, Blob:
		return string(a.S) == string(b.S)
	}
	return true // two NULLs
}

// argCollation is the collation governing this aggregate's argument
// comparisons: an explicit "min(x COLLATE nocase)" or a bare column's declared
// collation (topExprCollation's rule). Over ('abc'),('ABC'),('BCD'):
//
//	min(y COLLATE nocase)          -> 'abc'  (plain min(y) is 'ABC')
//	min(z), z TEXT COLLATE nocase  -> 'abc'
//	count(DISTINCT y COLLATE nocase) -> 2    (plain count(DISTINCT y) is 3)
//	count(DISTINCT z)              -> 2
//
// so DISTINCT dedup in any aggregate is collation-aware. "" means BINARY.
func (it *aggItem) argCollation(ctx *evalCtx) string {
	if !it.argCollSet {
		it.argColl, _ = topExprCollation(ctx, it.expr)
		it.argCollSet = true
	}
	return it.argColl
}

// obBuffer is updateAccumulator's ORDER BY arm (select.c:6857-6925) for one
// admitted row: the keys and the argument values go into the buffer, in
// arrival order, instead of into the accumulation. A DISTINCT is applied HERE,
// on the way in, exactly where C applies it -- codeDistinct against iDistinct
// before the OP_IdxInsert (select.c:6912-6916) -- so the replay never
// deduplicates again.
func (it *aggItem) obBuffer(ctx *evalCtx, regs []Value) error {
	keys := make([]Value, len(it.orderBy))
	for i := range it.orderBy {
		r := 0
		if i < len(it.obRegs) {
			r = it.obRegs[i]
		}
		if r <= 0 || r > len(regs) {
			return fmt.Errorf("engine: internal: aggregate ORDER BY term %d was never lowered into a register", i)
		}
		keys[i] = regs[r-1]
	}
	arg, err := it.rowValue(aggExprArg, ctx, regs)
	if err != nil {
		return err
	}
	var sep Value
	if it.sepExpr != nil {
		if sep, err = it.rowValue(aggExprSep, ctx, regs); err != nil {
			return err
		}
	}
	if !it.obCollSet {
		it.obColls = make([]string, len(it.orderBy))
		for i, t := range it.orderBy {
			it.obColls[i], _ = topExprCollation(ctx, t.Expr)
		}
		it.obCollSet = true
		it.obCtx = ctx
		it.argCollation(ctx) // cached for the replay, which has no row context
	}
	if it.distinct {
		coll := it.argCollation(ctx)
		for i := range it.obBuf {
			prev := &it.obBuf[i]
			if compareValuesCollatedEnc(arg, prev.arg, coll, ctx.encoding()) != 0 {
				continue
			}
			if it.obUnique {
				// The index's own key overwrites (see obUnique): the LATER
				// spelling survives, in the same position.
				if it.orderStrict && !valuesIdentical(arg, prev.arg) {
					it.orderRisk = true
				}
				prev.arg, prev.keys = arg, keys
				return nil
			}
			// iDistinct keeps the FIRST arrival, and its keys with it, so a
			// duplicate that differs from the survivor in either makes the
			// answer an arrival-order question.
			if it.orderStrict && (!valuesIdentical(arg, prev.arg) || !valuesAllIdentical(keys, prev.keys)) {
				it.orderRisk = true
			}
			return nil
		}
	}
	it.obBuf = append(it.obBuf, aggOBRow{keys: keys, arg: arg, sep: sep})
	return nil
}

// obReplay is finalizeAggFunctions' ORDER BY loop (select.c:6742-6786): the
// buffer in key order -- each term's collation, direction and NULLS placement
// (orderTermLess), ties in arrival order, which is what the OP_Sequence column
// in C's key gives -- stepped into the ordinary accumulation, before the
// ordinary finalize reads it. The replay goes through step itself with the
// FILTER, DISTINCT and ORDER BY already applied switched off, over a two-slot
// register block holding the entry's argument and separator.
//
// Where the ARRIVAL order is not provably C's (orderStrict), the key order
// still is, so only a tie group can let arrival reach the answer: two tied
// entries whose argument or separator differ latch orderRisk.
func (it *aggItem) obReplay() error {
	it.obDone = true
	enc := it.obCtx.encoding()
	less := func(a, b aggOBRow) (bool, bool) {
		for k, t := range it.orderBy {
			coll := ""
			if k < len(it.obColls) {
				coll = it.obColls[k]
			}
			if l, eq := orderTermLess(t, a.keys[k], b.keys[k], coll, enc); !eq {
				return l, false
			}
		}
		return false, true
	}
	sort.SliceStable(it.obBuf, func(i, j int) bool {
		l, _ := less(it.obBuf[i], it.obBuf[j])
		return l
	})
	if it.orderStrict {
		for i := 1; i < len(it.obBuf); i++ {
			a, b := it.obBuf[i-1], it.obBuf[i]
			if _, tie := less(a, b); tie && (!valuesIdentical(a.arg, b.arg) || !valuesIdentical(a.sep, b.sep)) {
				it.orderRisk = true
				break
			}
		}
	}
	orderBy, filter, distinct, strict, rowRegs := it.orderBy, it.filter, it.distinct, it.orderStrict, it.rowRegs
	it.orderBy, it.filter, it.distinct, it.orderStrict = nil, nil, false, false
	it.rowRegs = [3]int{0, 1, 2} // aggExprArg -> regs[0], aggExprSep -> regs[1]
	defer func() {
		it.orderBy, it.filter, it.distinct, it.orderStrict, it.rowRegs = orderBy, filter, distinct, strict, rowRegs
	}()
	regs := make([]Value, 2)
	for _, row := range it.obBuf {
		regs[0], regs[1] = row.arg, row.sep
		if err := it.step(it.obCtx, regs); err != nil {
			return err
		}
	}
	it.obBuf = nil
	return nil
}

// aggOBKeyIsArg is C's bOBPayload==0 (expr.c:7522-7526): one ORDER BY term
// that is the one argument, so the key is coded once and doubles as it.
func aggOBKeyIsArg(it *aggItem) bool {
	return len(it.orderBy) == 1 && it.sepExpr == nil && it.expr != nil && exprEqual(it.orderBy[0].Expr, it.expr)
}

// valuesAllIdentical is valuesIdentical over two equal-length tuples.
func valuesAllIdentical(a, b []Value) bool {
	for i := range a {
		if !valuesIdentical(a[i], b[i]) {
			return false
		}
	}
	return true
}

// planAggregateCall validates fc's arity and argument expressions and
// builds the matching aggItem. Argument expressions are validated with the
// ordinary row-mode checkExprSupported: they're evaluated per contributing
// row against that row's full column set exactly like a WHERE clause, so
// bare column references are fine (checkExprSupported already allows them)
// while a nested aggregate call is rejected (none of sql_eval.go's
// supportedFuncs are aggregate names, so checkExprSupported already refuses
// e.g. sum(count(x)) with an "unsupported function" error).
func planAggregateCall(fc FuncExpr) (*aggItem, error) {
	it, err := planAggregateCallKind(fc)
	if err != nil {
		return nil, err
	}
	// The ORDER BY terms are resolved in the call's own NameContext right after
	// its arguments (resolve.c:1322-1330), so they get the argument's checks --
	// "group_concat(x ORDER BY count(*))" is "misuse of aggregate function
	// count()", resolve.c's wording -- min()/max() included.
	for _, ob := range fc.orderByExprs() {
		if err := checkExprSupported(ob); err != nil {
			return nil, err
		}
	}
	// FILTER (WHERE ...) rides on the accumulator; step applies it per row.
	it.filter = fc.Filter
	// ORDER BY rides on it too, except on min()/max() (see aggItem.orderBy).
	if len(fc.OrderBy) > 0 && it.kind != aggMin && it.kind != aggMax {
		it.orderBy = fc.OrderBy
		// bOBPayload==0 (expr.c:7522-7526): one term that IS the one argument.
		// sqlite3ExprCompare compares RESOLVED expressions, so "x" and "t.x"
		// are the same there and different in this unresolved AST; the only
		// place that difference could change an answer is bOBUnique's
		// last-arrival-wins dedup, so that spelling declines.
		noPayload := len(fc.OrderBy) == 1 && len(fc.Args) == 1 && exprEqual(fc.OrderBy[0].Expr, fc.Args[0])
		if !noPayload && fc.Distinct && len(fc.OrderBy) == 1 && len(fc.Args) == 1 {
			if a, ok := fc.Args[0].(ColumnExpr); ok {
				if b, ok := fc.OrderBy[0].Expr.(ColumnExpr); ok && equalFoldName(a.Name, b.Name) {
					return nil, fmt.Errorf("%w: DISTINCT aggregate whose ORDER BY names its argument's column spelled differently (bOBUnique is decided on resolved expressions)", errVDBEUnsupported)
				}
			}
		}
		it.obUnique = noPayload && fc.Distinct
	}
	// srcCall is the call AS WRITTEN, kept only so an aggregate this query
	// turns out not to own can be re-planned against the query that does (see
	// hoistOwnerFor, vdbe_agg_hoist.go). Nothing at run time reads it --
	// cloneAggTemplates deliberately does not copy it.
	it.srcCall = fc
	return it, nil
}

func planAggregateCallKind(fc FuncExpr) (*aggItem, error) {
	name := r33sFoldIdent(fc.Name)
	if fc.Star {
		if name != "count" {
			return nil, fmt.Errorf("engine: unsupported use of * in %s()", fc.Name)
		}
		if fc.Distinct {
			// Defensive: unreachable via the parser today (a bare "*" right
			// after DISTINCT fails to parse as an expression -- see
			// sql_parser.go's function-call-arg parsing), but kept as a
			// direct error rather than silently ignoring DISTINCT, matching
			// the reference engine's rejection of "count(DISTINCT *)".
			return nil, fmt.Errorf("engine: unsupported use of DISTINCT with *")
		}
		return &aggItem{kind: aggCountStar}, nil
	}
	// DISTINCT aggregates must have exactly one argument (verified against
	// the reference engine: group_concat(DISTINCT x, sep) -- otherwise a
	// valid 2-argument call -- is rejected with exactly this message once
	// DISTINCT is present). count/sum/total/avg/min/max already require
	// exactly one argument regardless of DISTINCT, so this check is only
	// ever reachable (today) via group_concat, but is applied uniformly as
	// the general rule SQLite itself enforces.
	if fc.Distinct && len(fc.Args) != 1 {
		return nil, fmt.Errorf("engine: DISTINCT aggregates must have exactly one argument")
	}
	switch name {
	case "count":
		// count() with NO arguments at all is C SQLite's OWN spelling of
		// count(*): it registers count with both nArg 0 and nArg 1, and its
		// countStep bumps the counter unconditionally when argc==0 (func.c).
		// Verified directly against the reference engine over (1,NULL),(1,2),
		// (NULL,3): "SELECT count() FROM t1" is 3, exactly like count(*), and
		// so is the per-group / windowed / FILTERed spelling. It is the ONLY
		// zero-argument aggregate SQLite accepts -- sum()/avg()/total()/min()/
		// max()/group_concat() with no argument all error there too (probed,
		// all six agree with this package already), which is why this case is
		// widened and the others below are not.
		if len(fc.Args) == 0 {
			if fc.Distinct {
				// Unreachable via the parser (DISTINCT needs an expression
				// after it), and C SQLite rejects "count(DISTINCT)" as a
				// syntax error; kept explicit rather than silently dropped.
				return nil, fmt.Errorf("engine: unsupported use of DISTINCT with count()")
			}
			return &aggItem{kind: aggCountStar}, nil
		}
		if len(fc.Args) != 1 {
			return nil, fmt.Errorf("engine: count() takes exactly 1 argument (or count(*))")
		}
		if err := checkExprSupported(fc.Args[0]); err != nil {
			return nil, err
		}
		return &aggItem{kind: aggCount, expr: fc.Args[0], distinct: fc.Distinct}, nil

	case "sum", "total", "avg":
		if len(fc.Args) != 1 {
			return nil, fmt.Errorf("engine: %s() takes exactly 1 argument", name)
		}
		if err := checkExprSupported(fc.Args[0]); err != nil {
			return nil, err
		}
		kind := aggSum
		switch name {
		case "total":
			kind = aggTotal
		case "avg":
			kind = aggAvg
		}
		return &aggItem{kind: kind, expr: fc.Args[0], distinct: fc.Distinct}, nil

	case "min", "max":
		// isAggregateCall already only routes the 1-argument form here (a
		// 2+-argument call is the SCALAR form -- see sql_eval.go's
		// fnMinMaxScalar -- and never reaches planAggregateCall at all), so
		// this arity check should be unreachable via that path, but is kept
		// as a direct, defensive error rather than a panic if that
		// invariant is ever violated.
		if len(fc.Args) != 1 {
			return nil, fmt.Errorf("engine: internal: aggregate %s() called with %d arguments (expected exactly 1)", name, len(fc.Args))
		}
		if err := checkExprSupported(fc.Args[0]); err != nil {
			return nil, err
		}
		kind := aggMin
		if name == "max" {
			kind = aggMax
		}
		// DISTINCT is semantically a no-op for min/max (the least/greatest
		// of a set of values equals the least/greatest of its distinct
		// values), verified against the reference engine, but the dedup
		// bookkeeping (it.seen) is harmless overhead either way, so it isn't
		// specially suppressed here.
		return &aggItem{kind: kind, expr: fc.Args[0], distinct: fc.Distinct}, nil

	case "group_concat", "string_agg":
		// string_agg(X,Y) is C SQLite's exact alias for the 2-argument
		// group_concat(X,Y) (identical accumulation -- aggGroupConcat kind --
		// including a NULL separator meaning "no separator"), but is registered
		// ONLY with 2 arguments: string_agg(X) and string_agg(X,Y,Z) both error
		// "wrong number of arguments" where group_concat accepts 1-or-2 args.
		// Verified directly against mattn/go-sqlite3. Enforce the exact-2 arity
		// for string_agg so a 1-argument call is declined (never wrongly
		// accepted); the result column NAME is taken from the verbatim source
		// text (expandSelectList), so an unaliased "string_agg(...)" still
		// reports its own name, not "group_concat(...)".
		if name == "string_agg" {
			if len(fc.Args) != 2 {
				return nil, fmt.Errorf("engine: string_agg() takes exactly 2 arguments")
			}
		} else if len(fc.Args) < 1 || len(fc.Args) > 2 {
			// resolve.c:1293's own wording for a wrong argument count, and its
			// own prepare-time rejection: "wrong number of arguments to
			// function %#T()".
			return nil, semanticf("engine: wrong number of arguments to function %s()", "group_concat")
		}
		if err := checkExprSupported(fc.Args[0]); err != nil {
			return nil, err
		}
		var sep Expr
		if len(fc.Args) == 2 {
			if err := checkExprSupported(fc.Args[1]); err != nil {
				return nil, err
			}
			sep = fc.Args[1]
		}
		return &aggItem{kind: aggGroupConcat, expr: fc.Args[0], sepExpr: sep, distinct: fc.Distinct}, nil

	case "json_group_array", "jsonb_group_array":
		if len(fc.Args) != 1 {
			return nil, fmt.Errorf("engine: wrong number of arguments to function %s()", name)
		}
		if err := checkExprSupported(fc.Args[0]); err != nil {
			return nil, err
		}
		// jsonb_group_array is the same WAGGREGATE with JSON_BLOB user data
		// (json.c:5701), which only its result computation reads.
		return &aggItem{kind: aggJSONGroupArray, expr: fc.Args[0], distinct: fc.Distinct, jsonb: name == "jsonb_group_array"}, nil

	case "json_group_object", "jsonb_group_object":
		if len(fc.Args) != 2 {
			return nil, fmt.Errorf("engine: wrong number of arguments to function %s()", name)
		}
		if err := checkExprSupported(fc.Args[0]); err != nil {
			return nil, err
		}
		if err := checkExprSupported(fc.Args[1]); err != nil {
			return nil, err
		}
		// sepExpr reused as the VALUE expression here -- see
		// stepJSONGroupObject's (engine/json_funcs.go) doc comment.
		return &aggItem{kind: aggJSONGroupObject, expr: fc.Args[0], sepExpr: fc.Args[1], distinct: fc.Distinct, jsonb: name == "jsonb_group_object"}, nil
	}
	return nil, fmt.Errorf("engine: unsupported aggregate function %s()", fc.Name)
}

// step feeds one surviving row's values into its accumulator.
//
// With it.distinct, a non-NULL value reaches the kind-specific logic only the
// first time it is seen (compareValues equality, as GROUP BY groups), before
// any kind-specific processing: sum(DISTINCT x) over INTEGER 2 and TEXT '2'
// counts both, but a second INTEGER 2 once. group_concat(DISTINCT x) emits a
// separator only between surviving values.
func (it *aggItem) step(ctx *evalCtx, regs []Value) error {
	// FILTER (WHERE ...) gates the whole row out of this ONE aggregate --
	// checked before the kind switch so it applies to count(*) too (which
	// otherwise counts every row unconditionally), and evaluated against the
	// same per-row ctx the argument expressions use. A false OR NULL
	// condition skips the row, matching WHERE's own truthiness rule.
	if it.filter != nil {
		v, err := it.rowValue(aggExprFilter, ctx, regs)
		if err != nil {
			return err
		}
		if !isTruthy(v) {
			return nil
		}
	}
	if it.orderBy != nil {
		return it.obBuffer(ctx, regs)
	}
	switch it.kind {
	case aggCountStar:
		it.cnt++
		return nil

	case aggJSONGroupArray:
		// Unlike every aggregate above/below, a NULL row DOES contribute
		// (as the JSON literal null) -- handled entirely by
		// stepJSONGroupArray (engine/json_funcs.go), which must run BEFORE the
		// generic "NULL never contributes"/distinct-dedup logic just below,
		// not share it.
		return it.stepJSONWatched(ctx, regs, it.stepJSONGroupArray)

	case aggJSONGroupObject:
		return it.stepJSONWatched(ctx, regs, it.stepJSONGroupObject)
	}

	v, err := it.rowValue(aggExprArg, ctx, regs)
	if err != nil {
		return err
	}
	// The separator is argv[1], and the whole argument list is evaluated
	// before the step function runs (select.c:6902-6906, :6942), so every
	// admitted row evaluates it, even when the value is NULL or a DISTINCT
	// duplicate, which groupConcatStep ignores (func.c:2191, 2201):
	//
	//	SELECT group_concat(x, abs(y)) FROM t  -- t(x,y) = (NULL, min-int64)
	//	    "integer overflow", not NULL
	//
	// (json_group_object's twin is in stepJSONGroupObject.) Only evaluation
	// moves here; the BLOB->text conversion stays with the concatenation
	// below, an unobservable reordering since it cannot raise.
	var sepVal Value
	if it.kind == aggGroupConcat && it.sepExpr != nil {
		sepVal, err = it.rowValue(aggExprSep, ctx, regs)
		if err != nil {
			return err
		}
	}
	if v.Typ == Null {
		return nil
	}
	coll := it.argCollation(ctx)
	if it.distinct {
		for _, s := range it.seen {
			if compareValuesCollatedEnc(v, s, coll, ctx.encoding()) == 0 {
				// The dedup keeps the FIRST spelling of a value, so when the two
				// are not byte-identical -- 'abc'/'ABC' under NOCASE, INTEGER 1
				// and REAL 1.0 -- WHICH one every downstream kind then sees is an
				// arrival-order question, decided here rather than in the kind
				// branches below because they never see the loser at all.
				// count(DISTINCT x) is unaffected either way and is never armed.
				if it.orderStrict && !valuesIdentical(v, s) {
					it.orderRisk = true
				}
				return nil
			}
		}
		it.seen = append(it.seen, v)
	}

	switch it.kind {
	case aggCount:
		it.cnt++
		return nil

	case aggSum, aggTotal, aggAvg:
		it.cnt++
		asInt, i, f := aggNumericContribution(v)
		if it.orderStrict {
			it.sumWatch(asInt, i, f)
		}
		it.sumStep(asInt, i, f)
		return nil

	case aggMin, aggMax:
		if !it.haveBest {
			it.best, it.haveBest = v, true
			return nil
		}
		c := compareValuesCollatedEnc(v, it.best, coll, ctx.encoding())
		if (it.kind == aggMin && c < 0) || (it.kind == aggMax && c > 0) ||
			(it.minMaxLastWins && c == 0) {
			it.best = v
			return nil
		}
		// A value that ties the running best but is not IDENTICAL to it makes
		// which one survives an arrival-order question -- see orderSensitive's
		// min/max paragraph, and orderRisk for why testing against the RUNNING
		// best (rather than against the whole tie group) is exact.
		if it.orderStrict && c == 0 && !valuesIdentical(v, it.best) {
			it.orderRisk = true
		}
		return nil

	case aggGroupConcat:
		// group_concat's arguments are TEXT, so C SQLite converts a BLOB from
		// the database encoding first -- see coerceUTF16BlobValue (utf16.go),
		// which windowC.test's "group_concat(<blob>, <sep>)" is the corpus case
		// for. Both the VALUE and the SEPARATOR are converted, independently.
		v, cerr := coerceUTF16BlobValue("group_concat", v, ctx.encoding())
		if cerr != nil {
			return cerr
		}
		sep := ","
		if it.sepExpr != nil {
			// A NULL separator (verified against the reference engine)
			// contributes no text between items, exactly like NULL
			// anywhere else valueToText is used for concatenation.
			sv, serr := coerceUTF16BlobValue("group_concat", sepVal, ctx.encoding())
			if serr != nil {
				return serr
			}
			sep = valueToText(sv)
		}
		txt := valueToText(v)
		if it.orderStrict {
			// N copies of ONE (value, separator) pair concatenate to the same
			// string in any order; two contributions that differ in either half
			// do not. The first row's separator is never emitted, but it is
			// compared anyway: in another order it would be.
			switch {
			case !it.gcHave:
				it.gcFirstVal, it.gcFirstSep = txt, sep
			case txt != it.gcFirstVal || sep != it.gcFirstSep:
				it.orderRisk = true
			}
		}
		if it.gcHave {
			it.gcBuf.WriteString(sep)
		}
		it.gcBuf.WriteString(txt)
		it.gcHave = true
		return nil
	}
	return fmt.Errorf("engine: internal: unknown aggregate kind %d", it.kind)
}

// stepJSONWatched runs one of the two JSON aggregate steps (engine/json_funcs.go, and
// this file deliberately does not duplicate their value rules) and answers
// group_concat's order question about it from the OUTSIDE: both append their
// contribution -- one array element, or one "key":value pair -- to the same
// gcBuf, preceded by a ',' from the second one on, so the bytes appended by
// this call ARE the contribution, and N identical ones concatenate to the same
// JSON in any order. Only armed accumulators pay for it (see orderRiskErr).
func (it *aggItem) stepJSONWatched(ctx *evalCtx, regs []Value, jstep func(*evalCtx, []Value) error) error {
	if !it.orderStrict {
		return jstep(ctx, regs)
	}
	had, before := it.gcHave, it.gcBuf.Len()
	if err := jstep(ctx, regs); err != nil {
		return err
	}
	if it.gcBuf.Len() == before {
		// Nothing contributed: a NULL key in json_group_object, which is
		// order-free, or a DISTINCT duplicate, which is not -- the dedup keeps
		// the FIRST spelling and its own `seen` comparison is inside json_funcs.go, so
		// this cannot tell whether the loser was byte-identical. Latch.
		it.orderRisk = it.orderRisk || it.distinct
		return nil
	}
	chunk := it.gcBuf.String()[before:]
	if had {
		chunk = chunk[1:] // the ',' this call wrote before its own contribution
	}
	switch {
	case !had:
		it.gcFirstVal = chunk
	case chunk != it.gcFirstVal:
		it.orderRisk = true
	}
	return nil
}

// magnetStep advances one min()/max() census site (magnetPlan) over the row
// and reports what it does to the shared magnet register M, which decides
// whether the row becomes the group's bare-column anchor
// (aggAccumulators.magnetWalk). touched==false means the site was skipped and
// M keeps its previous value; otherwise M becomes hit.
//
// It is separate from step because it reproduces the three C sites that write
// the magnet, in order (updateAccumulator):
//
//	pFilter    -> OP_If jumps past everything below   (touched=false)
//	iDistinct  -> codeDistinct jumps past it likewise (touched=false)
//	OP_CollSeq -> sets M := 0 unconditionally
//	OP_AggStep -> minmaxStep calls sqlite3SkipAccumulatorLoad (M := 1) iff
//	              the accumulator did NOT change: a NULL argument with a best
//	              already recorded, or a value that does not beat it (a TIE
//	              counts as "did not beat" -- the first row holding the
//	              extremum wins).
//
// Unlike step, codeDistinct dedups the raw argument through an index where
// NULL==NULL, and a NULL argument still reaches minmaxStep. Over
// t(a,b,c)=(1,NULL,1),(2,NULL,5), "SELECT min(c), max(DISTINCT b), a FROM t"
// answers a=1.
func (it *aggItem) magnetStep(ctx *evalCtx, regs []Value) (touched, hit bool, err error) {
	if it.filter != nil {
		fv, ferr := it.rowValue(aggExprFilter, ctx, regs)
		if ferr != nil {
			return false, false, ferr
		}
		if !isTruthy(fv) {
			return false, false, nil
		}
	}
	v, err := it.rowValue(aggExprArg, ctx, regs)
	if err != nil {
		return false, false, err
	}
	coll := it.argCollation(ctx)
	if it.distinct {
		for _, s := range it.seen {
			if s.Typ == Null && v.Typ == Null {
				return false, false, nil
			}
			if s.Typ != Null && v.Typ != Null &&
				compareValuesCollatedEnc(v, s, coll, ctx.encoding()) == 0 {
				return false, false, nil
			}
		}
		it.seen = append(it.seen, v)
	}
	if v.Typ == Null {
		return true, it.haveBest, nil
	}
	if !it.haveBest {
		it.best, it.haveBest = v, true
		return true, false, nil
	}
	c := compareValuesCollatedEnc(v, it.best, coll, ctx.encoding())
	if (it.kind == aggMin && c < 0) || (it.kind == aggMax && c > 0) {
		it.best = v
		return true, false, nil
	}
	return true, true, nil
}

// sumStep folds one classified contribution (aggNumericContribution) into the
// sum()/avg()/total() state, a port of sumStep (func.c:1929). The integer
// total is exact until a non-integer arrives; only then is the float total
// seeded (kahanBabuskaNeumaierInit) and accumulated with an error term:
//
//	(9007199254740992, 1, 1)   avg   3.0023997515803315e15
//	                           total 9.007199254740994e15
//	(9007199254740992, 1, 1, 0.5) sum 9.007199254740994e15
//	(1.0, 1.0e17, -1.0e17)     sum/total 1.0
//
// The overflow latch is C's, including its clear on a later non-integer
// (func.c:1958): sum() raises "integer overflow" only if still latched at
// finalize, so sum over (max-int64, 1, 2.5) is a REAL.
func (it *aggItem) sumStep(asInt bool, i int64, f float64) {
	if !it.sumApprox {
		if !asInt {
			it.kbnInit(it.iSum)
			it.sumApprox = true
			it.kbnStep(f)
			return
		}
		if s, ok := addInt64(it.iSum, i); ok {
			it.iSum = s
			return
		}
		it.sumOverflow = true
		it.kbnInit(it.iSum)
		it.sumApprox = true
		it.kbnStepInt64(i)
		return
	}
	if asInt {
		it.kbnStepInt64(i)
		return
	}
	it.sumOverflow = false
	it.kbnStep(f)
}

// sumWatch records the EXPONENT WINDOW of the values a sum()/avg()/total() saw
// -- the largest magnitude, the coarsest common unit, and (through cnt) how many
// there were. Only called on an armed accumulator; see sumOrderFree for what the
// three prove. Every value is reduced to two integer exponents so no float
// rounding enters the proof itself: |v| < 2^top, and v is an integer multiple of
// 2^unit.
func (it *aggItem) sumWatch(asInt bool, i int64, f float64) {
	var top, unit int
	switch {
	case asInt:
		if i == 0 {
			return // adding zero is exact in any order
		}
		u := uint64(i)
		if i < 0 {
			u = -u // wraps to the magnitude, MinInt64 included
		}
		top, unit = bits.Len64(u), bits.TrailingZeros64(u)
	case f == 0:
		return
	case math.IsNaN(f) || math.IsInf(f, 0):
		// Nothing to bound: an infinity's own order-dependence (Inf + -Inf) is
		// not something to reason about.
		it.orderRisk = true
		return
	default:
		// f == frac * 2^exp with 0.5 <= |frac| < 1, so |f| < 2^exp and the
		// mantissa frac*2^53 is an exact integer whose trailing zeros say how
		// coarse f is: f == odd * 2^(exp-53+tz).
		frac, exp := math.Frexp(f)
		m := uint64(math.Abs(frac) * (1 << 53))
		top, unit = exp, exp-53+bits.TrailingZeros64(m)
	}
	if !it.sumWinSet {
		it.sumTopExp, it.sumUnitExp, it.sumWinSet = top, unit, true
		return
	}
	if top > it.sumTopExp {
		it.sumTopExp = top
	}
	if unit < it.sumUnitExp {
		it.sumUnitExp = unit
	}
}

// sumOrderFree reports whether this sum()/avg()/total() accumulates the same
// in every order. Every value is an integer multiple of 2^unit with |v| <
// 2^top, and there are at most 2^k of them (k = cnt's bit length), so every
// partial sum in every order is a multiple of 2^unit below 2^(top+k):
//
//   - approx==0: pure int64 addition, so order can only change whether it
//     overflows; top+k <= 63 says none can. (sum over (max-i64, -max-i64,
//     max-i64) is not free: another order overflows.)
//   - otherwise: top+k-unit <= 53 makes every partial sum an exact double, so
//     nothing rounds, including any int64 prefix (kahanBabuskaNeumaierInit's
//     split is exact). The overflow error cannot differ either: ovrfl is only
//     set while approx==0, and the non-integer either preceded the overflow
//     or cleared it (func.c:1958).
func (it *aggItem) sumOrderFree() bool {
	if it.orderRisk || it.sumOverflow {
		return false
	}
	if it.cnt <= 1 || !it.sumWinSet {
		// Nothing, one value, or only zeros: there is no addition to reorder.
		// The cnt<=1 case is not a nicety -- a GROUP BY over a key that is
		// nearly unique makes most groups singletons, and the fuzz corpus's
		// favourite values (+-2^63, 1e308) all fail the window test below.
		return true
	}
	// ceil(log2(cnt)): 2^k is an upper bound on how many values were added.
	k := bits.Len64(uint64(it.cnt - 1))
	if !it.sumApprox {
		return it.sumTopExp+k <= 63
	}
	// top+k <= 1023 keeps every partial sum FINITE, which the mantissa test
	// alone does not: 1e308+1e308-1e308 needs only a handful of significant
	// bits, but the order that adds the two positives first overflows to +Inf
	// where the other answers 1e308 (this engine served that, wrongly, until the
	// bound was written down -- compat-harness/aggorder_r29_test.go's sum-kbn).
	return it.sumTopExp+k <= 1023 && it.sumTopExp+k-it.sumUnitExp <= 53
}

// kbnStep is one Kahan-Babuska-Neumaier step (kahanBabuskaNeumaierStep,
// func.c:1873). The C marks its locals volatile to defeat x87 excess precision;
// Go's float64 arithmetic is already exactly IEEE-754 double, so the ordinary
// spelling is the faithful one.
func (it *aggItem) kbnStep(r float64) {
	s := it.rSum
	t := s + r
	if math.Abs(s) > math.Abs(r) {
		it.rErr += (s - t) + r
	} else {
		it.rErr += (r - t) + s
	}
	it.rSum = t
}

// kbnStepInt64 adds a (possibly large) integer to the running float total,
// splitting it so no single step loses bits (kahanBabuskaNeumaierStepInt64,
// func.c:1890). 4503599627370496 is 2^52.
func (it *aggItem) kbnStepInt64(v int64) {
	if v <= -4503599627370496 || v >= 4503599627370496 {
		sm := v % 16384
		it.kbnStep(float64(v - sm))
		it.kbnStep(float64(sm))
		return
	}
	it.kbnStep(float64(v))
}

// kbnInit seeds the float total from the exact integer one at the moment the
// accumulation stops being exact (kahanBabuskaNeumaierInit, func.c:1905).
func (it *aggItem) kbnInit(v int64) {
	if v <= -4503599627370496 || v >= 4503599627370496 {
		sm := v % 16384
		it.rSum, it.rErr = float64(v-sm), float64(sm)
		return
	}
	it.rSum, it.rErr = float64(v), 0
}

// kbnTotal is the float total as sumFinalize/avgFinalize/totalFinalize read it:
// rSum plus the error term, unless that term is itself NaN/Inf (sqlite3IsOverflow,
// util.c:75), in which case rSum alone.
func (it *aggItem) kbnTotal() float64 {
	if math.IsNaN(it.rErr) || math.IsInf(it.rErr, 0) {
		return it.rSum
	}
	return it.rSum + it.rErr
}

// aggNumericContribution classifies a non-NULL value as sum()/avg()/total()'s
// step does:
//
//   - INTEGER contributes exactly as int64 (asInt=true).
//   - REAL contributes as float64.
//   - TEXT that is entirely a well-formed number (parseFullNumeric, the
//     strict affinity rule, not arithmetic's prefix rule) contributes as that
//     number's type: '5' is the integer 5.
//   - anything else (other TEXT, any BLOB) contributes as float64 via the
//     leading-numeric-prefix rule ('5abc' is 5.0; no prefix is 0.0).
//
// f is always set; i only when asInt.
func aggNumericContribution(v Value) (asInt bool, i int64, f float64) {
	switch v.Typ {
	case Int:
		return true, v.I, float64(v.I)
	case Float:
		return false, 0, v.F
	case Text:
		if isF, ii, ff, ok := parseFullNumeric(string(v.S)); ok {
			if isF {
				return false, 0, ff
			}
			return true, ii, float64(ii)
		}
		// The one exception, and it runs the OPPOSITE way to arithmetic's: TEXT
		// whose integer prefix ends at a NUL contributes as an exact INTEGER even
		// though it is not a well-formed number, so "sum(cast(x'3100' as text))"
		// is the integer 1 where "sum('1x')" is the real 1.0. See
		// numericPrefixEndsAtNUL for the byte-level rule and the
		// table of what disagrees. BLOB does NOT get this -- "sum(x'3100')" is
		// real on both engines -- which is why the check sits in this Text arm
		// rather than being shared with the Blob one below.
		if numericPrefixEndsAtNUL(v) {
			if isF, ii, _, ok := parseNumericPrefix(string(v.S)); ok && !isF {
				return true, ii, float64(ii)
			}
		}
		isF, ii, ff := toNumericLoose(v)
		return false, 0, floatOf(isF, ii, ff)
	case Blob:
		isF, ii, ff := toNumericLoose(v)
		return false, 0, floatOf(isF, ii, ff)
	default: // Null: callers never invoke this for a NULL value
		return false, 0, 0
	}
}

// finalize computes it's result Value once the scan is done. It takes no
// context: every kind below reads only the running state step accumulated, the
// way C SQLite's xFinalize reads only sqlite3_aggregate_context (func.c's
// sumFinalize/minMaxFinalize/groupConcatFinalize all take just the
// sqlite3_context).
func (it *aggItem) finalize() (Value, error) {
	if it.orderBy != nil && !it.obDone {
		if err := it.obReplay(); err != nil {
			return Value{}, err
		}
	}
	if err := it.orderRiskErr(); err != nil {
		return Value{}, err
	}
	switch it.kind {
	case aggCountStar, aggCount:
		return Value{Typ: Int, I: it.cnt}, nil

	case aggSum:
		return finalizeSum(it)

	case aggTotal:
		// total() is always REAL and, unlike sum(), never NULL (0 non-NULL
		// inputs leaves the running total at 0.0) and never errors on
		// integer overflow (verified against the reference engine: the
		// same all-integer, overflowing input that makes sum() raise
		// "integer overflow" makes total() return a plain REAL).
		return Value{Typ: Float, F: it.sumFloat()}, nil

	case aggAvg:
		if it.cnt == 0 {
			return Value{Typ: Null}, nil
		}
		// The running total divided by the count, even when the
		// (unused-by-avg) integer running sum overflowed -- verified
		// against the reference engine: avg() never raises sum()'s
		// "integer overflow" error.
		return Value{Typ: Float, F: it.sumFloat() / float64(it.cnt)}, nil

	case aggMin, aggMax:
		if !it.haveBest {
			return Value{Typ: Null}, nil
		}
		return it.best, nil

	case aggGroupConcat:
		if !it.gcHave {
			return Value{Typ: Null}, nil
		}
		return Value{Typ: Text, S: []byte(it.gcBuf.String())}, nil

	case aggJSONGroupArray:
		return it.finalizeJSONGroupArray(), nil

	case aggJSONGroupObject:
		return it.finalizeJSONGroupObject(), nil
	}
	return Value{}, fmt.Errorf("engine: internal: unknown aggregate kind %d", it.kind)
}

// finalizeSum implements sum()'s result:
//
//   - no non-NULL inputs: NULL.
//   - any non-exact-integer contribution (sumApprox): the float total, even
//     if the integer total overflowed earlier (sum over (max-int64, 1, 2.5)
//     is 9223372036854775809.0).
//   - otherwise, an overflowed integer total raises "integer overflow"
//     (unlike + and unary -, which promote to REAL; avg() and total() never
//     raise it).
//   - otherwise the int64 total.
func finalizeSum(it *aggItem) (Value, error) {
	if it.cnt == 0 {
		return Value{Typ: Null}, nil
	}
	if it.sumApprox {
		if it.sumOverflow {
			return Value{}, fmt.Errorf("engine: integer overflow")
		}
		return Value{Typ: Float, F: it.kbnTotal()}, nil
	}
	return Value{Typ: Int, I: it.iSum}, nil
}

// sumFloat is avg()/total()'s reading of the running total: the EXACT integer
// one while the accumulation is still exact, the Kahan-Babuska-Neumaier one
// after (avgFinalize/totalFinalize, func.c:2020/2034).
func (it *aggItem) sumFloat() float64 {
	if it.sumApprox {
		return it.kbnTotal()
	}
	return float64(it.iSum)
}

// planNoGroupAggregate resolves a no-GROUP-BY aggregate SELECT's select list
// into one itemPlan per output column (aggregate calls rewritten to
// groupAggExpr placeholders, rewriteGroupExpr) plus every accumulator
// template. Used by compileScanAggregate; the caller handles "*", ORDER BY,
// WHERE and the scan.
//
// magnet/usesGroupBare mirror groupByPlan's fields: the min()/max() census and
// its S flag (magnetPlan), and whether any item rewrote a bare column against
// the anchor row.
//
// outer is the enclosing query's context (nil at top level), used only by the
// association check. outerCompiler is the live compile chain around stmt (nil
// when none), passed to planGroupItem so an escaping aggregate buried in a
// subquery can be escalated further out (recordSpec). Either may be nil.
func planNoGroupAggregate(stmt *SelectStmt, scopes []tableScope, outer *evalCtx, mode colNameMode, outerCompiler *compiler) (outCols []outputColumn, items []*itemPlan, allAggs []*aggItem, magnet *magnetPlan, usesGroupBare bool, err error) {
	outCols, err = expandSelectList(stmt.Columns, scopes, mode)
	if err != nil {
		return nil, nil, nil, nil, false, err
	}

	magnet = magnetPlanFor(stmt, false)

	// aggregateOwnedHere (see checkItemSubqueries' own doc, sql_group.go):
	// stmt is the SAME AST on both compileScanAggregate's first attempt and
	// any hoistOwnerFor retry (only its c.hoisted differs between them, never
	// stmt itself), and equally on compileNoFromAggregate's one and only
	// attempt (a FROM-less aggregate never retries), so this stays correct
	// -- false exactly when THIS query became aggregate solely via a hoisted
	// mover -- across every caller.
	aggregateOwnedHere := selectIsAggregateQuery(stmt)

	items = make([]*itemPlan, len(outCols))
	for i, oc := range outCols {
		// No pager on hand at this level (this function's signature carries
		// none), so the SELECT-LIST items of a whole-table aggregate never get
		// sql_group.go's outer-alias hoist (outerAliasAggCall) -- only its
		// HAVING clause does, planned separately by compileScanAggregate with
		// c.pager threaded through. Passing nil here simply keeps that hoist
		// from ever firing, same as before it existed. outerCompiler IS
		// threaded, independent of the pager gap above: it unlocks a DIFFERENT
		// hoist (recordSpec's escalation to an enclosing compiler), not
		// outerAliasAggCall's pager-gated one.
		it, ierr := planGroupItem(oc.expr, nil, nil, nil, scopes, true, nil, nil, nil, aggregateOwnedHere, outerCompiler, true)
		if ierr != nil {
			return nil, nil, nil, nil, false, ierr
		}
		items[i] = it
		allAggs = append(allAggs, it.aggTemplates...)
		if it.usesGroupBare {
			usesGroupBare = true
		}
	}

	if err := checkAggregateAssociation(allAggs, scopes, outer, outerCompiler); err != nil {
		return nil, nil, nil, nil, false, err
	}
	return outCols, items, allAggs, magnet, usesGroupBare, nil
}

// checkAggregateAssociation applies SQLite's aggregate-association rule to
// every aggregate the query owns, declining those that belong to an enclosing
// query. From resolve.c's TK_AGG_FUNCTION arm:
//
//	pNC2 = pNC;
//	while( pNC2 && sqlite3ReferencesSrcList(pParse, pExpr, pNC2->pSrcList)==0 ){
//	  pExpr->op2 += (1 + pNC2->nNestedSelect); pNC2 = pNC2->pNext; }
//	if( pNC2 && pDef ) pNC2->ncFlags |= NC_HasAgg | ...;
//
// sqlite3ReferencesSrcList returns 1 when the arguments name a table of that
// FROM, 0 when they name only other tables, -1 when they name none (or only
// their own subqueries' tables). So the aggregate belongs to the closest
// SELECT whose FROM supplies a referenced column, and stays put when nothing
// outward is referenced. Cases, in test order:
//
//   - a referenced column resolves locally: it is this query's, and any
//     outer column in it is a per-row constant ("(SELECT group_concat(b1,a1)
//     FROM t2) FROM t1").
//   - no column at all (sum(1), count(*), "total((SELECT b FROM x1))"): -1,
//     it stays.
//   - only outer columns: SQLite moves it outward, changing the outer row
//     count ("SELECT (SELECT sum(a1) FROM t2) FROM t1" is one row). Not
//     reproduced; declined.
//
// outer is used only to phrase the decline; with none, an unresolvable column
// reports its own "no such column".
//
// Every aggregate owner must call this: the select list
// (planNoGroupAggregate), a whole-table aggregate's HAVING
// (compileScanAggregate), and a GROUP BY query's select list, HAVING and
// ORDER BY (planGroupByStmt).
//
// outerCompiler, when non-nil, resolves a name that binds nothing locally but
// is not "no column": a double-quoted identifier or TRUE/FALSE falls back to a
// literal only after every NameContext fails (resolve.c:719-745), so if an
// enclosing FROM binds it the aggregate moves ("SELECT (SELECT max("a") FROM
// (SELECT 1 AS b)) FROM t" is one row).
func checkAggregateAssociation(aggs []*aggItem, scopes []tableScope, outer *evalCtx, outerCompiler *compiler) error {
	var (
		movers []*aggItem
		first  error
	)
	for _, agg := range aggs {
		exprs := aggArgExprs(agg)
		if len(exprs) == 0 {
			continue
		}
		hasLocal := false
		for _, e := range exprs {
			if aggArgHasLocalColumnRef(e, scopes) {
				hasLocal = true
				break
			}
		}
		if hasLocal {
			continue
		}
		// A PSEUDO-ROW argument -- NEW./OLD. in a trigger body, or an upsert's
		// "excluded" -- is neither local nor a mover, so this whole question
		// does not apply to it and asking anyway declines a statement the
		// oracle answers ("INSERT INTO tlog VALUES((SELECT sum(new.a)))").
		//
		// C reaches the same place by never counting one: analyzeAggregate's
		// argument walk associates an aggregate by matching TK_COLUMN cursor
		// numbers (expr.c), and a NEW./OLD. reference is TK_TRIGGER, not
		// TK_COLUMN, so it contributes to no SrcList and moves nothing.
		if aggArgsAreAllPseudoRow(exprs) {
			continue
		}
		localErr := validateColumnRefsAll(&evalCtx{tables: scopes}, exprs...)
		if localErr == nil {
			// validateColumnRefsAll cannot see a column buried inside a
			// FROM-less subquery (SubqueryExpr is deliberately not handled
			// there), so it just reported "no column referenced" -- but that is
			// only true for a BARE column; a subquery-buried one still needs
			// this second look before the "-1, stays" verdict is safe. See
			// aggArgHasFromlessSubqueryColumnRef's doc comment for the
			// verified example (sum((SELECT t1.y)) is a MOVER, not "-1").
			buried := false
			for _, e := range exprs {
				if aggArgHasFromlessSubqueryColumnRef(e) {
					buried = true
					break
				}
			}
			outward := !buried && fallbackNameBindsOutward(exprs, scopes, outerCompiler)
			if !buried && !outward {
				continue
			}
			localErr = fmt.Errorf("engine: unsupported: aggregate argument names a column reachable only through a from-less nested subquery")
			if outward {
				localErr = fmt.Errorf("engine: unsupported: aggregate argument names an enclosing query's column through an identifier with a literal fallback")
			}
		}
		movers = append(movers, agg)
		if first != nil {
			continue
		}
		if outer != nil && validateColumnRefsAll(&evalCtx{tables: scopes, outer: outer}, exprs...) == nil {
			first = fmt.Errorf("engine: unsupported: aggregate argument correlated to an enclosing query is not supported by this engine: %w", localErr)
			continue
		}
		first = localErr
	}
	if first == nil {
		return nil
	}
	// The error TEXT is exactly the first offender's, unchanged -- what is new
	// is that the whole set of re-associating aggregates rides along, so a
	// caller holding the enclosing compile chain can hoist them instead of
	// declining (hoistOwnerFor, vdbe_agg_hoist.go). Every other caller sees an
	// ordinary error.
	return &aggAssociationError{movers: movers, err: first}
}

// fallbackNameBindsOutward reports whether an unqualified identifier with a
// literal fallback (ColumnExpr.FallbackLiteral) in exprs names no column of
// scopes but one of an enclosing query's FROM. See checkAggregateAssociation.
// It does not enter subqueries, like aggArgHasLocalColumnRef.
func fallbackNameBindsOutward(exprs []Expr, scopes []tableScope, outer *compiler) bool {
	if outer == nil {
		return false
	}
	found := false
	var walk func(Expr)
	walk = func(e Expr) {
		if found || e == nil {
			return
		}
		if ce, ok := e.(ColumnExpr); ok {
			if ce.FallbackLiteral == nil || ce.Qualifier != "" {
				return
			}
			if _, _, _, _, err := resolveColumn(&evalCtx{tables: scopes}, "", ce.Name); err == nil {
				return
			}
			for oc := outer; oc != nil; oc = oc.outer {
				if _, _, _, _, err := resolveColumn(&evalCtx{tables: compileTableScopes(oc.scopes)}, "", ce.Name); err == nil {
					found = true
					return
				}
			}
			return
		}
		walkExprOperands(e, walk)
	}
	for _, e := range exprs {
		walk(e)
	}
	return found
}

// aggArgExprs is the expression list sqlite3ReferencesSrcList walks for one
// aggregate: its argument, group_concat's separator, and its FILTER, shared by
// checkAggregateAssociation and the hoist owner walk. The FILTER is part of
// the same TK_AGG_FUNCTION node. Over t1(a1)=(1,2,3), t2(b1,x)=(4,1),(5,0):
//
//	(SELECT count(a1) FILTER(WHERE x) FROM t2) FROM t1     -> 3 rows of 1
//	(SELECT count(a1) FROM t2) FROM t1                     -> ONE row, 3
//	(SELECT count(*) FILTER(WHERE a1>1) FROM t2) FROM t1   -> ONE row, 2
//	(SELECT count(b1) FILTER(WHERE a1>1) FROM t2) FROM t1  -> 3 rows: 0,2,2
//
// (filter1.test 6.1 is the first.)
func aggArgExprs(agg *aggItem) []Expr {
	var exprs []Expr
	if agg.expr != nil {
		exprs = append(exprs, agg.expr)
	}
	if agg.sepExpr != nil {
		exprs = append(exprs, agg.sepExpr)
	}
	if agg.filter != nil {
		exprs = append(exprs, agg.filter)
	}
	// The call's own ORDER BY, from the call as written: sqlite3ReferencesSrcList
	// walks pLeft for every aggregate (expr.c:7215-7220), min()/max() included,
	// whose aggItem.orderBy is nil. Without it "(SELECT group_concat(n.i ORDER
	// BY m.i) FROM m)" was handed to the OUTER query for naming only n.i.
	exprs = append(exprs, agg.srcCall.orderByExprs()...)
	return exprs
}

// aggAssociationError is checkAggregateAssociation's verdict when at least one
// aggregate does NOT belong to the query it is written in: err is the decline
// exactly as it always read, and movers lists every offending accumulator (in
// select-list order) so a caller that CAN place them -- the compiler, which
// holds the enclosing chain -- may hoist instead. Unwrap keeps errors.Is on
// the original error working for everyone else.
type aggAssociationError struct {
	movers []*aggItem
	err    error
}

func (e *aggAssociationError) Error() string { return e.err.Error() }

func (e *aggAssociationError) Unwrap() error { return e.err }

// aggArgHasLocalColumnRef reports whether e's expression tree contains, at
// any depth (but NOT descending into an independent SubqueryExpr/ExistsExpr
// scope -- exactly like validateColumnRefs/containsAggregate's identical
// treatment), at least one ColumnExpr that resolves against scopes ALONE,
// i.e. without needing to walk out to any enclosing (correlated) scope at
// all. Used only by planNoGroupAggregate's aggregate-argument association
// check above.
func aggArgHasLocalColumnRef(e Expr, scopes []tableScope) bool {
	localCtx := &evalCtx{tables: scopes}
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return false
	case ColumnExpr:
		_, _, _, _, err := resolveColumn(localCtx, x.Qualifier, x.Name)
		return err == nil
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if aggArgHasLocalColumnRef(a, scopes) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return aggArgHasLocalColumnRef(x.X, scopes)
	case BinaryExpr:
		return aggArgHasLocalColumnRef(x.L, scopes) || aggArgHasLocalColumnRef(x.R, scopes)
	case IsNullExpr:
		return aggArgHasLocalColumnRef(x.X, scopes)
	case InExpr:
		if aggArgHasLocalColumnRef(x.X, scopes) {
			return true
		}
		for _, a := range x.List {
			if aggArgHasLocalColumnRef(a, scopes) {
				return true
			}
		}
		return r35dFromlessSubHasLocalRef(x.Sub, scopes)
	case SubqueryExpr:
		return r35dFromlessSubHasLocalRef(x.Stmt, scopes)
	case ExistsExpr:
		return r35dFromlessSubHasLocalRef(x.Stmt, scopes)
	case BetweenExpr:
		return aggArgHasLocalColumnRef(x.X, scopes) || aggArgHasLocalColumnRef(x.Lo, scopes) || aggArgHasLocalColumnRef(x.Hi, scopes)
	case LikeExpr:
		return aggArgHasLocalColumnRef(x.X, scopes) || aggArgHasLocalColumnRef(x.Pattern, scopes) || aggArgHasLocalColumnRef(x.Escape, scopes)
	case GlobExpr:
		return aggArgHasLocalColumnRef(x.X, scopes) || aggArgHasLocalColumnRef(x.Pattern, scopes)
	case CollateExpr:
		return aggArgHasLocalColumnRef(x.X, scopes)
	case CastExpr:
		return aggArgHasLocalColumnRef(x.X, scopes)
	case CaseExpr:
		if x.Base != nil && aggArgHasLocalColumnRef(x.Base, scopes) {
			return true
		}
		for _, w := range x.Whens {
			if aggArgHasLocalColumnRef(w.When, scopes) || aggArgHasLocalColumnRef(w.Then, scopes) {
				return true
			}
		}
		return x.Else != nil && aggArgHasLocalColumnRef(x.Else, scopes)
	default:
		return false
	}
}

// r35dFromlessSubHasLocalRef is aggArgHasLocalColumnRef's subquery case,
// following sqlite3ReferencesSrcList (expr.c:7200): it descends into
// subqueries, excluding only the tables their own FROMs define
// (selectRefEnter/selectRefLeave), and any column resolving to this query's
// FROM sets bit 1 (exprRefToSrcList). A FROM-less subquery excludes nothing
// (selectRefEnter returns early on nSrc==0), so its columns count as if
// written directly: aggnested-4.1's "SELECT (SELECT sum(x+(SELECT y)) FROM
// bb) FROM aa" is one row, 579, sum() staying with bb.
//
// The descent is taken only where the exclude list is provably empty for the
// arm (r35dNoFromAnywhere). Aliases and CTEs disqualify a body (an alias is
// visible from the body's own WHERE).
//
// Each compound arm is gated independently, as sqlite3WalkSelect pushes each
// arm's own SrcList: in "SELECT x, (SELECT max(1) OVER(PARTITION BY
// sum((SELECT y FROM t1 UNION SELECT x ORDER BY 1)))) FROM t1", the FROM-less
// "SELECT x" arm's outer x counts despite its sibling's FROM (one row).
func r35dFromlessSubHasLocalRef(stmt *SelectStmt, scopes []tableScope) bool {
	return r35dSelectHasLocalRef(stmt, scopes)
}

// r35dSelectHasLocalRef asks aggArgHasLocalColumnRef of every expression an
// r35dNoFromAnywhere-eligible ARM holds, then always recurses into compound
// siblings regardless of whether THIS arm was eligible -- see
// r35dFromlessSubHasLocalRef's doc comment for why gating is per-arm.
func r35dSelectHasLocalRef(stmt *SelectStmt, scopes []tableScope) bool {
	if stmt == nil {
		return false
	}
	if r35dNoFromAnywhere(stmt) {
		hit := false
		r35dArmExprs(stmt, func(e Expr) {
			if !hit && aggArgHasLocalColumnRef(e, scopes) {
				hit = true
			}
		})
		if hit {
			return true
		}
	}
	for _, a := range stmt.Compound {
		if r35dSelectHasLocalRef(a.Stmt, scopes) {
			return true
		}
	}
	return false
}

// r35dArmExprs calls fn for every expression of arm whose bare column
// references are judged at the enclosing level when arm is a FROM-less body
// inside an aggregate's argument. Two clauses are excluded:
//
// A compound's own ORDER BY: resolveCompoundOrderBy turns each term into an
// integer (resolve.c:1671-1683) or rejects the statement (resolve.c:1700-1703),
// before the association walk (resolve.c:1423, then :1356-1360):
//
//	SELECT (SELECT sum((SELECT 7 AS x UNION SELECT 8 ORDER BY x))) FROM t1
//	  -> TWO rows of 7: nothing outer is referenced, so sum() stays put.
//
// A bare name in WHERE / GROUP BY / HAVING / LIMIT / OFFSET / a non-compound
// ORDER BY that names the body's own alias: lookupName tries the FROM, then the
// result-set aliases (resolve.c:639-651), before the enclosing context
// (resolve.c:702), and resolveAlias (resolve.c:69-101) splices in the aliased
// expression, whose own references still count:
//
//	SELECT (SELECT sum((SELECT 5 AS c WHERE c=x))) FROM t1  -> ONE row
//
// The select list itself cannot bind a sibling alias (resolved first, see
// resolve.c:645):
//
//	SELECT (SELECT sum((SELECT x AS x))) FROM t1           -> ONE row, 3
func r35dArmExprs(sel *SelectStmt, fn func(Expr)) {
	if sel == nil {
		return
	}
	for _, c := range sel.Columns {
		// A "*" item carries no expression of its own (SelectColumn.Star), and
		// selectHasAnyColumnRef always skipped one explicitly; fn would see a
		// nil either way, so the skip is kept for clarity, not for behaviour.
		if c.Star {
			continue
		}
		fn(c.Expr)
	}
	// A bare name in one of the clauses below binds to arm's OWN select-list
	// alias before it reaches the enclosing query, and BECOMES that alias's
	// expression (resolveAlias, resolve.c:69-101). substituteAliasRefs is this
	// engine's port of exactly that splice; passing nil scopes is what makes it
	// the FROM-less case, where nothing local can win the name first.
	clause := func(e Expr) {
		if e == nil {
			return
		}
		changed := false
		fn(substituteAliasRefs(e, sel.Columns, nil, true, true, &changed, nil))
	}
	for _, e := range []Expr{sel.Where, sel.Having, sel.LimitParam, sel.OffsetParam} {
		clause(e)
	}
	for _, g := range sel.GroupBy {
		clause(g)
	}
	if len(sel.Compound) == 0 {
		for _, o := range sel.OrderBy {
			clause(o.Expr)
		}
	}
}

// r35dArmRefsResolve reports whether every bare column reference r35dArmExprs
// enumerates for sel (and inside its FROM-less subqueries) resolves against
// scopes. It is the precondition for hoisting the aggregate holding sel:
// hoisting replaces the argument with a value, so its names are never resolved
// again, while C resolves them all before association (resolve.c:1423, then
// :1356-1360). "SELECT (SELECT sum((SELECT x ORDER BY c))) FROM t1" is "no
// such column: c" in C.
func r35dArmRefsResolve(sel *SelectStmt, scopes []tableScope) bool {
	ctx := &evalCtx{tables: scopes}
	ok := true
	var check func(*SelectStmt)
	var descend func(Expr)
	descend = func(e Expr) {
		if !ok || e == nil {
			return
		}
		switch t := e.(type) {
		case SubqueryExpr:
			if r35dNoFromAnywhere(t.Stmt) {
				check(t.Stmt)
			}
			return
		case ExistsExpr:
			if r35dNoFromAnywhere(t.Stmt) {
				check(t.Stmt)
			}
			return
		case InExpr:
			if t.Sub != nil && r35dNoFromAnywhere(t.Sub) {
				check(t.Sub)
			}
		}
		walkExprOperands(e, descend)
	}
	check = func(cur *SelectStmt) {
		if !ok || cur == nil {
			return
		}
		for _, arm := range append([]*SelectStmt{cur}, compoundArmStmts(cur)...) {
			r35dArmExprs(arm, func(e Expr) {
				if !ok {
					return
				}
				if err := validateColumnRefsAll(ctx, e); err != nil {
					ok = false
					return
				}
				descend(e)
			})
		}
	}
	check(sel)
	return ok
}

// compoundArmStmts is sel's compound arms, in order (sel itself is the first
// arm and is NOT included).
func compoundArmStmts(sel *SelectStmt) []*SelectStmt {
	if sel == nil || len(sel.Compound) == 0 {
		return nil
	}
	out := make([]*SelectStmt, 0, len(sel.Compound))
	for _, a := range sel.Compound {
		out = append(out, a.Stmt)
	}
	return out
}

// r35dNoFromAnywhere reports whether stmt ITSELF -- not its compound arms,
// which r35dSelectHasLocalRef/selectHasAnyColumnRef gate one at a time -- and
// every subquery reachable from ITS OWN expressions, has NO FROM clause: the
// condition under which sqlite3ReferencesSrcList's exclude list stays empty
// for stmt's own arm. A WITH clause disqualifies it, and so does a select-list
// alias that could CAPTURE one of stmt's own bare names:
// see r35dFromlessSubHasLocalRef.
func r35dNoFromAnywhere(stmt *SelectStmt) bool {
	if stmt == nil {
		return false
	}
	if len(stmt.From) != 0 || len(stmt.CTEs) != 0 {
		return false
	}
	ok := true
	var walk func(Expr)
	walk = func(e Expr) {
		if !ok || e == nil {
			return
		}
		switch x := e.(type) {
		case SubqueryExpr:
			ok = ok && r35dNoFromAnywhere(x.Stmt)
		case ExistsExpr:
			ok = ok && r35dNoFromAnywhere(x.Stmt)
		case InExpr:
			if x.Sub != nil {
				ok = ok && r35dNoFromAnywhere(x.Sub)
			}
			walk(x.X)
			for _, it := range x.List {
				walk(it)
			}
		case FuncExpr:
			for _, a := range x.Args {
				walk(a)
			}
			walk(x.Filter)
			for _, ob := range x.orderByExprs() {
				walk(ob)
			}
		case UnaryExpr:
			walk(x.X)
		case BinaryExpr:
			walk(x.L)
			walk(x.R)
		case IsNullExpr:
			walk(x.X)
		case BetweenExpr:
			walk(x.X)
			walk(x.Lo)
			walk(x.Hi)
		case LikeExpr:
			walk(x.X)
			walk(x.Pattern)
			walk(x.Escape)
		case GlobExpr:
			walk(x.X)
			walk(x.Pattern)
		case MatchExpr:
			walk(x.X)
			walk(x.Pattern)
		case CollateExpr:
			walk(x.X)
		case CastExpr:
			walk(x.X)
		case CaseExpr:
			walk(x.Base)
			for _, w := range x.Whens {
				walk(w.When)
				walk(w.Then)
			}
			walk(x.Else)
		}
	}
	for _, c := range stmt.Columns {
		walk(c.Expr)
	}
	for _, e := range []Expr{stmt.Where, stmt.Having, stmt.LimitParam, stmt.OffsetParam} {
		walk(e)
	}
	for _, g := range stmt.GroupBy {
		walk(g)
	}
	for _, o := range stmt.OrderBy {
		walk(o.Expr)
	}
	return ok
}

// aggArgHasFromlessSubqueryColumnRef reports whether e contains, without
// crossing a subquery with its own FROM, a column reference inside a FROM-less
// subquery, whether or not it resolves. checkAggregateAssociation's fallback
// (validateColumnRefsAll) does not look inside subqueries, so such a column
// was invisible and the aggregate wrongly stayed local.
//
// This is sqlite3ReferencesSrcList's bit 0x02 (exprRefToSrcList,
// expr.c:7160): a column outside this FROM and the exclude list, which a
// FROM-less subquery never extends (expr.c:7131). Over t1(x,y)=(1,10),(2,20),
// t2(b)=(5),(6): "SELECT x, (SELECT sum((SELECT t1.y)) FROM t2) FROM t1" is
// one row, (1,30).
func aggArgHasFromlessSubqueryColumnRef(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr, ColumnExpr:
		return false
	case FuncExpr:
		for _, a := range x.Args {
			if aggArgHasFromlessSubqueryColumnRef(a) {
				return true
			}
		}
		for _, ob := range x.orderByExprs() {
			if aggArgHasFromlessSubqueryColumnRef(ob) {
				return true
			}
		}
		return aggArgHasFromlessSubqueryColumnRef(x.Filter)
	case UnaryExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.X)
	case BinaryExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.L) || aggArgHasFromlessSubqueryColumnRef(x.R)
	case IsNullExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.X)
	case InExpr:
		if aggArgHasFromlessSubqueryColumnRef(x.X) {
			return true
		}
		for _, a := range x.List {
			if aggArgHasFromlessSubqueryColumnRef(a) {
				return true
			}
		}
		return r35dFromlessSubHasAnyColumnRef(x.Sub)
	case SubqueryExpr:
		return r35dFromlessSubHasAnyColumnRef(x.Stmt)
	case ExistsExpr:
		return r35dFromlessSubHasAnyColumnRef(x.Stmt)
	case BetweenExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.X) || aggArgHasFromlessSubqueryColumnRef(x.Lo) || aggArgHasFromlessSubqueryColumnRef(x.Hi)
	case LikeExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.X) || aggArgHasFromlessSubqueryColumnRef(x.Pattern) || aggArgHasFromlessSubqueryColumnRef(x.Escape)
	case GlobExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.X) || aggArgHasFromlessSubqueryColumnRef(x.Pattern)
	case CollateExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.X)
	case CastExpr:
		return aggArgHasFromlessSubqueryColumnRef(x.X)
	case CaseExpr:
		if x.Base != nil && aggArgHasFromlessSubqueryColumnRef(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if aggArgHasFromlessSubqueryColumnRef(w.When) || aggArgHasFromlessSubqueryColumnRef(w.Then) {
				return true
			}
		}
		return x.Else != nil && aggArgHasFromlessSubqueryColumnRef(x.Else)
	default:
		return false
	}
}

// r35dFromlessSubHasAnyColumnRef is aggArgHasFromlessSubqueryColumnRef's
// SUBQUERY case: true when stmt contains at least one bare column reference
// reachable through an r35dNoFromAnywhere-eligible ARM (own-level gate, no
// FROM/WITH/select-list-alias -- the SAME provable-safety gate
// r35dFromlessSubHasLocalRef uses) at any depth. Gated PER ARM, exactly like
// r35dSelectHasLocalRef -- see its doc comment for the mixed-arm-compound
// case this closes. Once r35dNoFromAnywhere holds for a given arm there is no
// nested FROM anywhere below THAT arm to own a column, so any name found
// there is unconditionally an outward-or-nonexistent reference --
// exprHasAnyColumnRef therefore recurses freely within an eligible arm, with
// no exclusion bookkeeping of its own.
func r35dFromlessSubHasAnyColumnRef(stmt *SelectStmt) bool {
	return selectHasAnyColumnRef(stmt)
}

// selectHasAnyColumnRef and exprHasAnyColumnRef report whether a subquery
// tree contains a bare column reference anywhere, scanning only through
// r35dNoFromAnywhere-eligible arms (see r35dFromlessSubHasAnyColumnRef) but
// always recursing into compound siblings regardless of THIS arm's own
// eligibility. Unlike aggArgHasLocalColumnRef's walk, this one does not stop
// at a nested SubqueryExpr/InExpr.Sub/ExistsExpr: within an eligible arm,
// there is no nested FROM anywhere below for a deeper subquery to own.
func selectHasAnyColumnRef(stmt *SelectStmt) bool {
	if stmt == nil {
		return false
	}
	if r35dNoFromAnywhere(stmt) {
		hit := false
		r35dArmExprs(stmt, func(e Expr) {
			if !hit && exprHasAnyColumnRef(e) {
				hit = true
			}
		})
		if hit {
			return true
		}
	}
	for _, a := range stmt.Compound {
		if selectHasAnyColumnRef(a.Stmt) {
			return true
		}
	}
	return false
}

func exprHasAnyColumnRef(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return false
	case ColumnExpr:
		return true
	case FuncExpr:
		for _, a := range x.Args {
			if exprHasAnyColumnRef(a) {
				return true
			}
		}
		for _, ob := range x.orderByExprs() {
			if exprHasAnyColumnRef(ob) {
				return true
			}
		}
		return exprHasAnyColumnRef(x.Filter)
	case UnaryExpr:
		return exprHasAnyColumnRef(x.X)
	case BinaryExpr:
		return exprHasAnyColumnRef(x.L) || exprHasAnyColumnRef(x.R)
	case IsNullExpr:
		return exprHasAnyColumnRef(x.X)
	case InExpr:
		if exprHasAnyColumnRef(x.X) {
			return true
		}
		for _, a := range x.List {
			if exprHasAnyColumnRef(a) {
				return true
			}
		}
		return selectHasAnyColumnRef(x.Sub)
	case SubqueryExpr:
		return selectHasAnyColumnRef(x.Stmt)
	case ExistsExpr:
		return selectHasAnyColumnRef(x.Stmt)
	case BetweenExpr:
		return exprHasAnyColumnRef(x.X) || exprHasAnyColumnRef(x.Lo) || exprHasAnyColumnRef(x.Hi)
	case LikeExpr:
		return exprHasAnyColumnRef(x.X) || exprHasAnyColumnRef(x.Pattern) || exprHasAnyColumnRef(x.Escape)
	case GlobExpr:
		return exprHasAnyColumnRef(x.X) || exprHasAnyColumnRef(x.Pattern)
	case MatchExpr:
		return exprHasAnyColumnRef(x.X) || exprHasAnyColumnRef(x.Pattern)
	case CollateExpr:
		return exprHasAnyColumnRef(x.X)
	case CastExpr:
		return exprHasAnyColumnRef(x.X)
	case CaseExpr:
		if x.Base != nil && exprHasAnyColumnRef(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if exprHasAnyColumnRef(w.When) || exprHasAnyColumnRef(w.Then) {
				return true
			}
		}
		return x.Else != nil && exprHasAnyColumnRef(x.Else)
	case RowExpr:
		for _, el := range x.Elems {
			if exprHasAnyColumnRef(el) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// aggArgsAreAllPseudoRow reports whether every column reference in exprs is a
// PSEUDO-ROW one -- qualified NEW./OLD. (a trigger body's firing row) or
// "excluded" (an upsert's proposed row). Those name no FROM item, so they can
// neither make an aggregate local to this query nor move it to an enclosing
// one; see checkAggregateAssociation's use.
//
// Conservative in the safe direction: an expression carrying no column
// reference at all answers false and takes the ordinary path, exactly as
// before, and a MIX of a pseudo-row and a real column answers false too, so the
// association question is still asked wherever it can have an answer.
func aggArgsAreAllPseudoRow(exprs []Expr) bool {
	any, ok := false, true
	var walk func(Expr)
	walk = func(e Expr) {
		if c, isCol := e.(ColumnExpr); isCol {
			any = true
			switch r33sFoldIdent(c.Qualifier) {
			case "new", "old", "excluded":
			default:
				ok = false
			}
			return
		}
		walkExprOperands(e, walk)
	}
	for _, e := range exprs {
		walk(e)
	}
	return any && ok
}
