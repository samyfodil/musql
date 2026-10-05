// The PEER-ORDER guard for window functions: the window-side twin of the
// aggregate ANCHOR guard (anchorPlanOrderProvable, vdbe_agg_codegen.go).
//
// A window function is computed over rows sorted by the window's PARTITION BY
// and ORDER BY keys. That sort does not resolve TIES: rows with equal keys are
// PEERS, and the order they end up in is the order they ARRIVED in. Real
// SQLite's arrival order is whatever plan its cost model picked --
// sqlite3WindowRewrite (window.c:1002-1013) moves the whole FROM/WHERE into a
// sub-select whose ORDER BY is the concatenation of the window's PARTITION BY
// and ORDER BY clauses, and sqlite3WhereBegin then serves that ORDER BY from
// whichever access path won, so a covering-index scan feeds the window in INDEX
// order. This engine always feeds it in ascending rowid order (even a secondary
// index seek is sorted back into it -- SeekIndexRowidsSegments, segment_seek.go).
//
// When the two differ, a peer group's internal order differs, and a window
// function that reads a row's POSITION within its peers reports different
// values. MEASURED, and the bug this file was written for:
//
//	CREATE TABLE t(a,b,c);
//	INSERT INTO t VALUES(3,'z',1),(1,'x',2),(2,'y',3),(1,'w',4),(3,'v',5);
//	SELECT lead(a) OVER (ORDER BY EXISTS(SELECT 1 FROM (SELECT 1))) FROM t
//	  ORDER BY 1;
//
// The window ORDER BY is a constant, so every row is a peer of every other and
// the arrival order is the whole answer. With no index both engines say
// NULL,1,1,2,3. After "CREATE INDEX ta ON t(a)" C SQLite says NULL,1,2,3,3
// -- a covering-index scan over ta delivers a ascending (1,1,2,3,3), so lead(a)
// is 1,2,3,3,NULL -- and this engine still said NULL,1,1,2,3. A different
// MULTISET, surviving the outer ORDER BY: a wrong answer, not a row-order
// difference.
//
// The guard is in two halves, exactly like the aggregate one:
//
//   - COMPILE time (windowPlan.orderStrict, armed in compileScanWindow): is the
//     arrival order provably C SQLite's? This asks anchorPlanOrderProvable,
//     the same question and the same proof the anchor guard asks -- the ported
//     planner decided this statement's access path and loop order, or no index
//     exists for either engine to walk.
//
//   - RUN time (windowPeerAmbiguous, below): did the rows actually PRODUCE a
//     tie? An unproven order costs nothing at all unless two rows really are
//     peers, which is a property of the stored values and cannot be decided
//     from the statement text. This is the same "arm a run-time test rather
//     than decline outright" shape armOrderSensitive uses, and for the same
//     reason: declining every window query over an indexed table at compile
//     time would cost whole common shapes that cannot diverge.
package engine

import "fmt"

// windowPeerAmbiguous reports whether this LEVEL's sorted partitions contain a
// peer group whose internal order is not forced -- two rows the window's ORDER
// BY calls equal (or, with no window ORDER BY at all, any two rows of the same
// partition, since then the whole partition is one peer group) that are not
// interchangeable.
//
// "Interchangeable" is the whole batch entry, rowid slots included, and that is
// deliberate rather than lazy: a projection may name rowid, so two rows whose
// COLUMNS agree can still produce different output rows when swapped. Peers
// that are byte-identical everywhere permute invisibly and are not a risk.
//
// parts must already be sorted by partitionAndOrder -- peers are adjacent only
// after that sort. windowPeerEnds is the same peer-boundary computation
// computeWindowCall itself uses (same keys, same collations), so the runs
// walked here are exactly the peer groups the functions will see.
func (m *vdbe) windowPeerAmbiguous(plan *windowPlan, lvl windowCall, parts [][]int, batch, vals [][]Value) (bool, error) {
	for _, part := range parts {
		ends, err := m.windowPeerEnds(plan, lvl, part, batch, vals)
		if err != nil {
			return false, err
		}
		for i := 0; i < len(part); {
			j := ends[i]
			// Identity is transitive, so comparing every member of the run
			// against its first is enough to decide the whole run.
			ambiguous := false
			for k := i + 1; k < j; k++ {
				if !windowEntriesIdentical(batch[part[i]], batch[part[k]]) {
					ambiguous = true
					break
				}
			}
			// The LOOP-ORDER escape: a run whose rows differ can still be
			// immune to which nesting won. See windowPeerLoopImmune.
			if ambiguous && plan.loopOnlyStrict && m.windowPeerLoopImmune(plan, batch, part[i:j]) {
				ambiguous = false
			}
			if ambiguous {
				return true, nil
			}
			i = j
		}
	}
	return false, nil
}

// windowEntriesIdentical reports whether two batch entries are byte-identical
// in every slot -- columns, rowids and lowered operands alike. valuesIdentical
// is the same test the aggregate order-risk latch uses (sql_agg.go): equal
// under sqlite3MemCompare is NOT enough, since INTEGER 1 and REAL 1.0 compare
// equal and report differently.
func windowEntriesIdentical(a, b []Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesIdentical(a[i], b[i]) {
			return false
		}
	}
	return true
}

// windowCallPeerSensitive reports whether this call's VALUE can change when two
// peers are swapped.
//
//   - rank/dense_rank/percent_rank/cume_dist are defined on the peer GROUP --
//     every member takes the group's first position (computeWindowCall), and
//     cume_dist takes the group's end -- so a permutation inside the group
//     cannot move any of them.
//   - row_number and ntile are pure POSITION within the partition.
//   - lead/lag read the row N places away in the partition ORDER.
//   - first_value/last_value/nth_value read a POSITION within the frame, which
//     for a tied frame edge is a peer -- unless the value they read there is a
//     constant, the same at every position (window1.test 73.3's
//     "nth_value(15,2) OVER()"): then only the frame's SIZE counts, which is
//     the aggregate rule below.
//   - an ordinary aggregate used as a window function reads its FRAME, and the
//     frame is peer-based in every mode but ROWS: RANGE bounds are decided by
//     ORDER BY VALUE comparisons and GROUPS bounds by peer-group counts
//     (window.c's windowCodeRangeTest / eStart handling), so peers share a
//     frame, while a ROWS frame counts physical rows and a swap moves the
//     boundary across the pair. EXCLUDE does not change that -- CURRENT ROW,
//     GROUP and TIES all remove rows by IDENTITY or by peer group, never by
//     position.
//
// A peer-invariant AGGREGATE still has an order-sensitive ACCUMULATION over the
// frame it shares (group_concat appends in arrival order, sum's rounding is
// order-dependent). That half is not decided here: windowAggregate arms the
// accumulator's own orderStrict instead, so the existing per-value risk latch
// (aggItem.orderRiskErr, sql_agg.go) declines only the frames that really did
// depend on the order.
func windowCallPeerSensitive(call windowCall) bool {
	switch call.name {
	case "rank", "dense_rank", "percent_rank", "cume_dist":
		return false
	case "row_number", "ntile", "lead", "lag":
		return true
	case "first_value", "last_value", "nth_value":
		if len(call.args) == 0 || !exprIsConstantValue(call.args[0]) {
			return true
		}
	}
	if call.spec == nil || call.spec.Frame == nil || call.spec.Frame.Mode != FrameRows {
		return false
	}
	// "ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING" counts
	// physical rows to a boundary that is the partition's own end either way,
	// so it is the WHOLE partition for every row -- the same set a peer-based
	// frame gives, arrived at by counting rather than by comparing. EXCLUDE
	// does not change that: CURRENT ROW removes the row itself, GROUP and TIES
	// remove its peer group, and all three are identity- or peer-keyed, never
	// positional. Measured: this is windowpushd.test 2.x's
	// "max(max(z)) OVER (PARTITION BY sum(y) ROWS BETWEEN UNBOUNDED PRECEDING
	// AND UNBOUNDED FOLLOWING)", three statements that have no positional
	// dependence at all.
	f := call.spec.Frame
	return !(f.Start.Type == FrameUnboundedPreceding && f.End.Type == FrameUnboundedFollowing)
}

// errWindowPeerOrder is the decline windowCallPeerSensitive produces. It names
// the function so the decline bucket is readable per call, and keeps a leading
// phrase shared with errAnchorPlanOrder's family so a census still groups the
// two together as one root cause.
func errWindowPeerOrder(name string) error {
	return fmt.Errorf("%w: %s() OVER a peer group whose internal order is not provably C SQLite's -- the window's own ORDER BY leaves these rows tied and the scan that broke the tie may be an index one (see windowPeerAmbiguous)", errVDBEUnsupported, name)
}

// windowArrivalOrderProvable is the guard's COMPILE half: is the order rows
// reach OpWindowAppend in provably the order C SQLite feeds its own window
// sorter?
//
// It is anchorPlanOrderProvable (vdbe_agg_codegen.go) with its two PORTED-
// PLANNER disjuncts deliberately left out, because both of them model the
// pOrderBy select.c hands sqlite3WhereBegin for an ORDINARY statement, and a
// window statement's is a different list entirely: sqlite3WindowRewrite
// (window.c:1002-1013) moves the FROM/WHERE into a sub-select whose ORDER BY is
// the concatenation of the window's PARTITION BY and ORDER BY clauses --
//
//	pSort = exprListAppendList(pParse, 0, pMWin->pPartition, 1);
//	pSort = exprListAppendList(pParse, pSort, pMWin->pOrderBy, 1);
//
// -- and that is what decides which loop wins. The port says so itself:
// wherePlanSortCtlFor (where_plan_gate.go) declines "a WINDOW function,
// which routes to a different compiler here and to sqlite3WindowCodeStep
// there", so wherePlanIndexOrderDecided is false for every window statement
// anyway. wherePlanIndexesProvablyInert is the one that would be UNSOUND rather
// than merely useless: its indexMightHelpWithOrderBy half is computed only when
// the statement carries an outer ORDER BY / GROUP BY / DISTINCT / DISTINCT
// aggregate, so "SELECT lead(a) OVER (ORDER BY b) FROM t" with an index on b
// would be called inert while C walks that very index to satisfy pSort.
//
// What is left is the blunt, sound pair: no index for either engine to walk
// (anchorNoIndexInPlay), no WHERE_MULTI_OR loop whose rows arrive as a
// concatenation of sub-scans (anchorMultiOrInPlay), and a join loop order this
// engine reproduces (anchorLoopOrderProvable).
func windowArrivalOrderProvable(p *ReadOnlyPager, c *compiler, srcs []joinSource, scopes []tableScope,
	stmt *SelectStmt, calls []windowCall) bool {
	return windowScanPathProvable(p, c, srcs, scopes, stmt, calls) &&
		anchorLoopOrderProvable(srcs, scopes, stmt.Where)
}

// windowScanPathProvable is windowArrivalOrderProvable's ACCESS-PATH half on
// its own: every source is read by the path this engine reads it by, whatever
// order the loops around them end up nested in. Split out because the LOOP-ORDER
// half has a run-time escape the access-path half does not (see
// windowPeerLoopImmune), and that escape is only sound once this half holds.
func windowScanPathProvable(p *ReadOnlyPager, c *compiler, srcs []joinSource, scopes []tableScope,
	stmt *SelectStmt, calls []windowCall) bool {
	if p == nil {
		return false
	}
	// "PRAGMA reverse_unordered_selects" makes whereReverseScanOrder
	// (where.c:6727) walk EVERY loop backwards, and it fires exactly when the
	// statement asks no ordering question -- "if( pWInfo->pOrderBy==0 &&
	// (db->flags & SQLITE_ReverseOrder)!=0 )" (where.c:7126). For a window
	// sub-select pOrderBy is pSort, so this can only bite a window with neither
	// PARTITION BY nor ORDER BY ("OVER ()") -- where the whole partition is one
	// peer group and a reversed scan is exactly the divergence this guard is
	// about. Refused outright rather than reasoned about.
	if p.ReverseUnorderedSelects() {
		return false
	}
	if anchorMultiOrInPlay(p, srcs, scopes, stmt) {
		return false
	}
	if anchorNoIndexInPlay(p, srcs, scopes, stmt) {
		return true
	}
	// An index that yields no WhereLoop at all leaves SQLite's loop set
	// identical to the no-index one, and an index whose every key column is
	// CONSTANT inside a peer group leaves rowid as the only order it can
	// impose -- which is the order this engine already produces. The first is
	// wherePlanIndexesProvablyInert's own question; the second is handed to it
	// as a per-index exemption, because the two have to be answered TOGETHER:
	// windowpushd.test's v3 carries one of each ("CREATE INDEX i1 ON t1(a)",
	// inert for a query that names neither a nor an ordering it could help;
	// "CREATE INDEX i2 ON t1(b)", not inert at all but keyed on the very column
	// the window partitions by), and a statement is provable only when every
	// index is one or the other.
	opts, ok := windowInertOpts(srcs, scopes, calls)
	if !ok {
		return false
	}
	return wherePlanIndexesProvablyInert(p, c, srcs, stmt, opts)
}

// windowInertOpts builds the inertness test's inputs for a window statement:
// the REAL ordering list its base scan is planned against, and the per-index
// exemption for an index that cannot reorder a peer group.
//
// THE ORDERING LIST is the INNERMOST window level's PARTITION BY ++ ORDER BY,
// and nothing else. sqlite3WindowRewrite (window.c:1002-1013) builds pSort from
// pMWin alone and pushes every window function belonging to a DIFFERENT window
// down into the sub-select's expression list (selectWindowRewriteExprCb's
// TK_FUNCTION arm, window.c:775-786, which prunes only for a window that IS in
// pMWin's list and otherwise falls through to the append); that sub-select is
// then rewritten in its turn, so the select that finally reads the base table
// is the one for the LAST such window -- computeWindowLevels' "innermost"
// (vdbe_window.go), whose doc comment carries the direct 3.53.3 verification
// that it is the last-written spec. The statement's own ORDER BY, DISTINCT and
// LIMIT stay on OUTER selects that read an ephemeral table, never the base
// scan: sqlite3SelectNew is handed pSort as the sub-select's ORDER BY and 0,0
// for its limit/offset (window.c:1070-1072).
//
// PLUS the columns a WHERE term could be pushed down onto, because this
// statement may be a VIEW or sub-query body and the term is then not in
// stmt.Where at all. pushDownWhereTerms only pushes into a windowed sub-query
// when
//
//	if( pSubq->selFlags & (SF_Recursive|SF_MultiPart) ) return 0;  select.c:5146
//	if( pSubq->pWin && pSubq->pWin->pPartition==0 ) return 0;      select.c:5185
//
// and what it pushes is then restricted by pushDownWindowCheck to
// "sqlite3ExprIsConstantOrGroupBy(pParse, pExpr, pSubq->pWin->pPartition)"
// (select.c:5019) -- constants and copies of pMWin's OWN PARTITION BY
// expressions, nothing else. pMWin is the OUTERMOST level (the first window in
// select-list order; sqlite3WindowLink prepends, and only a window IDENTICAL to
// the head ever links, so the head's content is the first one written), and
// SF_MultiPart is set the moment a window that fails to link carries a
// DIFFERENT PARTITION BY (window.c:1342-1343). MEASURED, and it is what makes
// windowpushd.test's v2 provable: "SELECT a, c, max(c) OVER (PARTITION BY a),
// row_number() OVER () FROM t1" carries two incompatible partitions, so
// SF_MultiPart blocks push-down outright -- confirmed against 3.53.3 over a
// deliberately non-rowid-ordered t3, where "SELECT * FROM w3 WHERE a<'C'"
// reports row_number 2,3,4 (the positions among ALL five rows, in rowid order),
// which a pushed-down filter could not produce.
//
// ok == false when the shape is one this reasoning does not cover, which leaves
// the caller on anchorNoIndexInPlay alone.
func windowInertOpts(srcs []joinSource, scopes []tableScope, calls []windowCall) (*wherePlanInertOpts, bool) {
	levels := windowLevels(calls)
	if len(levels) == 0 {
		return nil, false
	}
	sortExprs := []Expr{}
	if inner := calls[levels[len(levels)-1]].spec; inner != nil {
		sortExprs = append(sortExprs, inner.PartitionBy...)
		for _, ot := range inner.OrderBy {
			sortExprs = append(sortExprs, ot.Expr)
		}
	}
	if main := calls[levels[0]].spec; main != nil && len(main.PartitionBy) > 0 && !windowMultiPart(levels, calls) {
		sortExprs = append(sortExprs, main.PartitionBy...)
	}
	return &wherePlanInertOpts{
		sortExprs: sortExprs,
		exempt:    windowPeerConstantIndexFn(srcs, scopes, calls),
	}, true
}

// windowMultiPart is SF_MultiPart (sqliteInt.h:3654, "Has multiple incompatible
// PARTITIONs"): sqlite3WindowLink links a window into pSel->pWin only when the
// list is empty or the new window is IDENTICAL to the head, and otherwise sets
// the flag whenever the two PARTITION BY lists differ
// ("if( sqlite3ExprListCompare(pWin->pPartition, pSel->pWin->pPartition,-1) )",
// window.c:1342-1343). levels[0] is the head (see windowInertOpts).
func windowMultiPart(levels []int, calls []windowCall) bool {
	head := calls[levels[0]].spec
	for _, li := range levels[1:] {
		s := calls[li].spec
		if sameWindowSpec(head, s) || (head == nil && s == nil) {
			continue
		}
		var hp, sp []Expr
		if head != nil {
			hp = head.PartitionBy
		}
		if s != nil {
			sp = s.PartitionBy
		}
		if !exprListEqual(hp, sp) {
			return true
		}
	}
	return false
}

// exprListEqual is sqlite3ExprListCompare's "identical" verdict over two bare
// expression lists, built on exprEqual (sql_group.go), this package's
// sqlite3ExprCompare.
func exprListEqual(a, b []Expr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !exprEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// windowPeerConstantIndexFn builds wherePlanIndexesInert's per-index exemption:
// an index whose every key column holds ONE value throughout any peer group,
// so the only order it can still impose inside that group is the trailing
// rowid's -- ascending, which is this engine's own scan order.
//
// The peer-constant columns are the INNERMOST window level's PARTITION BY and
// ORDER BY keys. Innermost is the one that matters because it is the level
// whose sub-select reads the base table: sqlite3WindowRewrite pushes every
// OTHER window function down into that sub-select's expression list
// (selectWindowRewriteExprCb, window.c:917-948), so only the innermost spec's
// keys become the pSort sqlite3WhereBegin plans against. Every level ABOVE it
// re-partitions rows whose order is already settled by the level below, so
// proving the innermost proves them all. (computeWindowLevels walks the levels
// innermost-first for the same reason, and its doc comment carries the direct
// 3.53.3 verification that the LAST-written spec is the innermost one.)
//
// Four conditions, each closing a way the residual order could stop being
// plain ascending rowid:
//
//   - every key of the spec is a BARE COLUMN of a source this statement scans.
//     An expression key cannot be matched against an index column here.
//   - no ORDER BY term of the spec is DESC. exprListAppendList copies the
//     window ORDER BY's sort order into pSort (window.c:1006, the trailing "1"
//     is bCopySortOrder), and a DESC pSort term is what lets
//     wherePathSatisfiesOrderBy walk the index BACKWARD -- which reverses the
//     rowid tie-break inside the peer group.
//   - every key column of the index is ASCending, for the same reason from the
//     other side: an index column declared DESC is walked backward to satisfy
//     an ASC ordering.
//   - the index column's collating sequence is the column's own declared one.
//     The peer grouping compares under the key expression's collation
//     (windowKeyCollations, vdbe_window.go, which for a bare column is the
//     declared one); an index built with an explicit different COLLATE could
//     call two rows equal that the peer grouping does not, so "constant across
//     the peer group" would no longer follow.
//
// A nil result (no usable peer keys) exempts nothing, which is the same answer
// as before this existed.
func windowPeerConstantIndexFn(srcs []joinSource, scopes []tableScope,
	calls []windowCall) func(int, *whereIndexInfo) bool {
	levels := windowLevels(calls)
	if len(levels) == 0 {
		return nil
	}
	spec := calls[levels[len(levels)-1]].spec
	if spec == nil || (len(spec.PartitionBy) == 0 && len(spec.OrderBy) == 0) {
		return nil
	}
	keys := append([]Expr(nil), spec.PartitionBy...)
	for _, ot := range spec.OrderBy {
		if ot.Desc {
			return nil
		}
		keys = append(keys, ot.Expr)
	}
	jts := make([]joinedTable, len(srcs))
	for i := range srcs {
		jts[i] = joinedTable{tbl: srcs[i].tbl}
	}
	peerConst := make([]uint64, len(srcs))
	for _, k := range keys {
		ce, ok := k.(ColumnExpr)
		if !ok {
			return nil
		}
		tab, col, ok := wherePlanColumnRef(jts, scopes, ce)
		if !ok || tab < 0 || tab >= len(srcs) || col < 0 || col >= 63 {
			return nil
		}
		peerConst[tab] |= uint64(1) << uint(col)
	}
	return func(i int, idx *whereIndexInfo) bool {
		if i < 0 || i >= len(srcs) || peerConst[i] == 0 || idx == nil || idx.nKeyCol == 0 {
			return false
		}
		tbl := srcs[i].tbl
		if tbl == nil {
			return false
		}
		for k := 0; k < idx.nKeyCol; k++ {
			if k >= len(idx.aiColumn) || k >= len(idx.azColl) || k >= len(idx.aSortOrder) {
				return false
			}
			col := idx.aiColumn[k]
			if col < 0 || col >= 63 || col >= len(tbl.cols) {
				return false
			}
			if peerConst[i]&(uint64(1)<<uint(col)) == 0 {
				return false
			}
			if idx.aSortOrder[k] {
				return false
			}
			if !equalFoldName(idx.azColl[k], collationOrBinary(tbl.cols[col].Collation)) {
				return false
			}
		}
		return true
	}
}

// collationOrBinary is a declared collation with the empty spelling folded to
// BINARY, which is what an index entry carries for an un-COLLATEd column.
func collationOrBinary(name string) string {
	if name == "" {
		return "BINARY"
	}
	return name
}

// windowPeerLoopImmune reports whether a peer group's internal order is the
// same under EVERY loop nesting C SQLite could have chosen -- the run-time
// escape for the LOOP-ORDER half of the arrival-order proof, and the reason
// windowArrivalOrderProvable splits that half out.
//
// The theorem: a FROM clause of N sources arrives lexicographically ordered by
// (position within loop 1, position within loop 2, ...) for whatever nesting
// won, and a WHERE clause only removes rows, never reorders them. So if every
// source but ONE holds the SAME ROW throughout the peer group, the surviving
// rows differ only in that one source's position, and every nesting -- inner
// first or outer first -- orders them by that position ascending. Which nesting
// won cannot be observed. With TWO sources varying it can: over A(a1,a2) and
// B(b1,b2), [A,B] arrives a1b1, a1b2, a2b1, a2b2 and [B,A] arrives a1b1, a2b1,
// a1b2, a2b2, which differ in the middle.
//
// MEASURED: this is windowC.test 2.x, the last shape the peer-order guard cost.
// "WITH separator(x) AS (VALUES(',a,'),(',bc,')), value(y) AS (VALUES(1),
// (x'5585...')) SELECT group_concat(y,x) OVER (ORDER BY x ROWS BETWEEN 1
// PRECEDING AND 1 PRECEDING) FROM separator, value" ties both rows of each
// x-peer-group, and the ported loop-order solver declines a FROM clause of two
// CTEs outright (wherePlanMultiTableSources) -- but inside each peer group the
// separator row is FIXED and only the value row moves, so the nesting is
// immaterial and the statement is answered rather than declined.
//
// Restricted to plain inner/cross sources: a LEFT/RIGHT/FULL join's
// null-extended rows are not a row of the source at all, and its nesting is
// constrained by the prerequisite mask rather than by cost, so the model above
// does not describe it. rowids are compared alongside the columns for the same
// reason windowEntriesIdentical compares them: a projection may name one.
func (m *vdbe) windowPeerLoopImmune(plan *windowPlan, batch [][]Value, run []int) bool {
	if len(plan.srcColOfs) < 2 || len(run) < 2 {
		return false
	}
	nSrc := len(plan.srcColOfs) - 1
	varying := 0
	first := batch[run[0]]
	for i := 0; i < nSrc; i++ {
		lo, hi := plan.srcColOfs[i], plan.srcColOfs[i+1]
		same := true
		for _, ri := range run[1:] {
			e := batch[ri]
			if !windowEntriesIdentical(first[lo:hi], e[lo:hi]) {
				same = false
				break
			}
			if r := plan.nCols + i; r < len(first) && r < len(e) && !valuesIdentical(first[r], e[r]) {
				same = false
				break
			}
		}
		if !same {
			varying++
			if varying > 1 {
				return false
			}
		}
	}
	return true
}

// windowPlainInnerSources reports whether every source of this FROM clause is a
// plain inner/cross one, the shape windowPeerLoopImmune's model describes. An
// OUTER join is excluded there for the reason that function's doc comment
// gives, and is excluded HERE rather than per peer group because it is a
// property of the statement, not of the rows.
func windowPlainInnerSources(srcs []joinSource) bool {
	if len(srcs) < 2 {
		return false // one source: the loop order was never in question
	}
	for i := range srcs {
		if srcs[i].left || srcs[i].rightOuter || srcs[i].tbl == nil {
			return false
		}
	}
	return true
}
