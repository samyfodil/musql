// The instruction set of the engine's bytecode VM (VDBE), a pure-Go
// reimplementation of SQLite's register machine and the engine's only executor:
// SQL compiles to a program, or the statement is an error (RULE #1). See vdbe.go
// for the loop, vdbe_codegen.go for the compiler, vdbe_disasm.go for EXPLAIN.
//
// Opcodes are named after SQLite's (vdbe.c) and keep their operand meanings and
// semantics, but not their numeric values or P4/P5 packing. The operand
// directions are copied exactly:
//
//   - Arithmetic / concat / bitwise (OpAdd, OpSubtract, ..., OpConcat,
//     OpBitAnd, ...): r[P3] = r[P2] OP r[P1]; OpSubtract computes r[P2]-r[P1].
//   - Comparisons (OpEq, OpNe, OpLt, OpLe, OpGt, OpGe) compare r[P3] against
//     r[P1] ("if reg(P3)<reg(P1) then jump to P2") and jump to P2 when true;
//     with p5StoreP2 they store the boolean (or NULL) in r[P2] instead. P5 also
//     carries the comparison affinity and the JUMPIFNULL / NULLEQ flags; P4
//     carries the collation name.
package engine

import "unsafe"

type OpCode uint8

const (
	// Control flow.
	OpInit      OpCode = iota // P2: jump to the program's real start (addr P2).
	OpGoto                    // P2: unconditional jump to addr P2.
	OpHalt                    // stop execution.
	OpResultRow               // P1, P2: emit r[P1 .. P1+P2-1] as one result row.
	OpIf                      // P1, P2, P3: jump to P2 if r[P1] is true; if r[P1] is NULL, jump iff P3!=0.
	OpIfNot                   // P1, P2, P3: jump to P2 if r[P1] is false; if r[P1] is NULL, jump iff P3!=0.
	OpIsNull                  // P1, P2: jump to P2 if r[P1] is NULL.
	OpNotNull                 // P1, P2: jump to P2 if r[P1] is NOT NULL.

	// Constant / value loads.
	OpInteger  // P1, P2: r[P2] = int64(P1) (small integers).
	OpInt64    // P4(int64), P2: r[P2] = P4 (full-width integers).
	OpReal     // P4(float64), P2: r[P2] = P4.
	OpString8  // P4(string), P2: r[P2] = TEXT P4.
	OpNull     // P2: r[P2] = NULL.
	OpBlob     // P4([]byte), P2: r[P2] = BLOB P4.
	OpVariable // P1, P2: r[P2] = the bound parameter at 1-based index P1.
	OpCopy     // P1, P2: r[P2] = deep copy of r[P1].
	OpSCopy    // P1, P2: r[P2] = shallow copy of r[P1].

	// Arithmetic / string (r[P3] = r[P2] OP r[P1]).
	OpAdd
	OpSubtract
	OpMultiply
	OpDivide
	OpRemainder
	OpConcat

	// Bitwise (r[P3] = r[P2] OP r[P1], bitwiseBinaryValue) and OpBitNot
	// (P1, P2: r[P2] = ~r[P1], bitNotValueUnary -- NULL-preserving,
	// otherwise always an INTEGER result; SQLite unary "~", compileUnary's
	// "~" case).
	// OpWindowAppend (P1: record register) appends one scanned row's
	// [cols.., rowids..] payload to the window batch; OpWindowFinal
	// (P4: *windowPlan) resolves the whole batch into the query's result rows
	// at once. See vdbe_window.go -- a window function is defined over a
	// PARTITION of the result set, which does not exist until the scan ends.
	OpWindowAppend
	OpWindowFinal

	// OpComputeGenerated (P1: the row's first register, P4: *tableMeta):
	// derives every generated column's value into its own register within
	// the row's full-width register block, via computeGeneratedInto
	// (query.go). Emitted right after a row's registers are populated and
	// BEFORE the CHECK constraints / OpMakeRecord that consume them, so a
	// CHECK over a generated column sees its real value and a STORED
	// column's value reaches the record. A no-op for a table with no
	// generated column.
	OpComputeGenerated

	// OpTypeCheck (P1: the row's first register, P4: *typeCheckPlan): a STRICT
	// table's per-column datatype check (checkStrictColumnTypes), C's
	// OP_TypeCheck (vdbe.c:3273-3275). The affinity half is the OpAffinity
	// block each write emitter already runs, so this is only the
	// storage-class check.
	//
	// Emitted after the NOT NULL tests and before CHECK, as C's lands
	// (sqlite3TableAffinity, insert.c:179, called at insert.c:2078-2081,
	// 2406-2409 or 2714-2717, all after the NOT NULL loop ending at 2058), and
	// before a BEFORE trigger fires (insert.c:1491/1495; update.c:983/984):
	// "INSERT INTO t VALUES(x'01','a')" into a STRICT t(i INT, b TEXT) fails
	// even when a BEFORE trigger would RAISE(IGNORE).
	//
	// It raises a plain error, never a conflictHalt: C raises
	// SQLITE_CONSTRAINT_DATATYPE from the opcode (vdbe.c:3391), so OR IGNORE /
	// REPLACE / FAIL still fail; writeCtx.resolveHalt treats it as ABORT.
	OpTypeCheck

	OpBitAnd
	OpBitOr
	OpShiftLeft
	OpShiftRight
	OpBitNot

	// Comparisons (compare r[P3] vs r[P1]; jump to P2 or, with p5StoreP2,
	// store the result in r[P2]).
	OpEq
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe

	// Logical.
	OpNot      // P1, P2: r[P2] = NOT r[P1] (NULL-preserving three-valued negation).
	OpNegative // P1, P2: r[P2] = -r[P1] (SQLite unary minus, negateValueUnary -- preserves IEEE negative zero, unlike a "0 - x" subtraction).

	// Affinity / cast. OpAffinity and OpRealAffinity MUTATE their target
	// register(s) in place (a permanent change), exactly like SQLite's
	// OP_Affinity/OP_RealAffinity -- distinct from the comparison-time,
	// non-mutating coercion the comparison opcodes apply via their P5 affinity
	// byte. OpCast likewise mutates r[P1] in place (SQLite's OP_Cast).
	OpAffinity     // P1, P4(affinity): r[P1] = applyAffinity(r[P1], P4).
	OpRealAffinity // P1: if r[P1] is an integer, convert it to REAL in place.
	OpCast         // P1, P4(string type): r[P1] = CAST(r[P1] AS P4).

	// Function call. P1: first argument register; P2: argument count; P3:
	// destination register; P4: the (lower-cased) function name, or the
	// *ScalarFunction itself (resolved when the program was built).
	OpFunction

	// Connection-state function. P3: destination register; P4: the
	// (lower-cased) name -- one of sqlite_version, changes, total_changes,
	// last_insert_rowid. It takes no arguments and no registers: its value
	// comes from the CONNECTION, read at RUN time from the write session or
	// the snapshot the run has (conn_state.go's connStateValue). Separate from
	// OpFunction because scalar functions have no connection to consult, and
	// reading at run time lets a trigger body's changes() see its previous
	// body statement.
	OpConnState

	// String-matching (LIKE). P1: subject reg; P3: pattern reg; P2: destination
	// reg; p5 carries the "NOT" flag. P4: the ESCAPE clause's register as an
	// int, or -1 when the LIKE has no ESCAPE clause (compileLike's sentinel).
	// Uses likeMatch/likeMatchEscape and likeEscapeRune (pattern_match.go),
	// which own the ESCAPE-operand validation, including its NULL-propagates /
	// error-takes-precedence-over-NULL ordering -- see likeEscapeRune's doc
	// comment. (IN (value-list) is compiled as a small jump chain over the
	// comparison opcodes plus a NULL-tracking flag register -- see
	// compileIn -- so it needs no dedicated opcode of its own.)
	OpLike

	// String-matching (GLOB). Same operand shape as OpLike (P1 subject, P3
	// pattern, P2 destination, p5GlobNot for NOT GLOB) but uses globMatch
	// instead of likeMatch (pattern_match.go) -- case-sensitive,
	// '*'/'?'/'[...]' wildcards, no ESCAPE.
	OpGlob

	// "X MATCH Pattern" for an fts5 table (fts5_match.go's evalMatch). P2:
	// destination. P4: *matchCompileInfo{expr, scopes, cursors}: the MatchExpr
	// and the scopes/cursors needed to gather the current row (gatherScopesRow,
	// vdbe.go). MATCH needs a whole row's column text (buildFts5Doc tokenizes
	// every column), so the opcode gathers the row itself instead of taking
	// compiled operand registers.
	OpMatch

	// An fts3/fts4 AUXILIARY function call -- "offsets(<tab>)" or
	// "matchinfo(<tab>[,<format>])" (fts3_search.go). P2: destination reg.
	// P4: *fts3AuxCompileInfo{fn, format, ms, query, scopes, cursors}. Like
	// OpMatch (and for the same reason) it
	// gathers the CURRENT ROW itself rather than taking pre-compiled operand
	// registers: an auxiliary function reports byte offsets INTO the row's own
	// column text, so it needs every column of the matched table plus the
	// MATCH query the cursor was opened with -- which is a property of the
	// statement, not of the call's arguments.
	OpFts3Aux

	// An fts3/fts4 optimize() call -- "optimize(<tab>)" (fts3_optimize.go).
	// P2: destination reg. P4: *fts3OptimizeInfo{table}. Alone among this
	// engine's read-side opcodes it WRITES: it merges the named table's
	// segments in the live write session the snapshot was taken from
	// (ReadOnlyPager.writeSession), which is exactly what C fts3's
	// optimize() does. It takes no operand registers because its one argument
	// names a TABLE, resolved at compile time, and it reads nothing from the
	// current row -- the row only decides HOW MANY TIMES it runs.
	OpFts3Optimize

	// The fts5 "rank" hidden column or an fts5 auxiliary function
	// (bm25/highlight/snippet, fts5_vdbe_aux.go). P2: destination. P4:
	// *fts5AuxCompileInfo{fn, state, argRegs, scopes, cursors}: state is the
	// corpus bm25 statistics resolved once at compile time (buildFts5AuxState);
	// argRegs are the call's trailing arguments, compiled (empty for "rank").
	// Like OpMatch, it gathers the current row itself.
	OpFts5Aux

	// Table cursor opcodes (vdbe_cursor.go implements the cursor itself: a
	// thin pull-based wrapper over the existing ScanTable b-tree scan --  no
	// new b-tree walking logic). These mirror SQLite's OP_OpenRead/OP_Rewind/
	// OP_Next/OP_Column/OP_Rowid/OP_Close (src/vdbe.c) operand-for-operand;
	// see compileSelectScan (vdbe_scan.go) for the canonical scan skeleton
	// that emits them (OpenRead; Rewind->end; loop: ...; Next->loop; end:
	// Close; Halt).
	OpOpenRead // P1: cursor number. P2: root page. P3: db index in dbIndexOf's encoding (cross_db.go) -- 0 for the running Program's own pager (m.pager; the overwhelmingly common case, and the ONLY case for a single-database compile), or i+1 for m.pager.attachedReaders[i] (a cross-database read whose FROM item was routed to an ATTACHed database by resolveJoinSources -- see joinSource.dbIdx, vdbe_join_codegen.go). Opens a read cursor on that pager's table b-tree (root is a page number within THAT pager, never m.pager's, whenever P3>0); P4 carries the *resolvedTable the cursor needs to normalize each row it reads (IPK rowid-alias substitution, REAL-affinity restore -- see normalizeRow).
	OpRewind   // P1: cursor. P2: jump target. Moves the cursor to its first row; if the table is empty, jump to P2 (past the loop). Otherwise fall through into the loop body.
	OpNext     // P1: cursor. P2: jump target. Advances the cursor; if another row exists, jump to P2 (the loop's top). Otherwise fall through (the scan is exhausted).
	OpColumn   // P1: cursor. P2: column index. P3: destination reg. r[P3] = the cursor's current row's column P2 (already IPK/REAL-affinity normalized).
	OpRowid    // P1: cursor. P2: destination reg. r[P2] = the cursor's current row's 64-bit rowid.
	OpClose    // P1: cursor. Ends the cursor's underlying scan.

	// OpClearSubtype drops a register's function subtype, the serialization
	// loss this engine's row block does not otherwise have (OpSorterData's P3
	// does it per record).
	//
	// C feeds a grouped query's aggregate steps from a sorter record, read back
	// with OP_Column over a pseudo-table (select.c:8601-8607,
	// expr.c:4995-4997); the Mem is rebuilt from a serial type
	// (sqlite3VdbeSerialGet, vdbeaux.c:4123), which has no subtype. So the
	// leaves of a grouped aggregate's argument lose the subtype. See
	// clearJSONSubtype (vdbe_agg.go); emitted only by emitColumnRead for a
	// hash-grouped plan's argument block, whose leaves come off live cursors.
	OpClearSubtype // P1: register. Drops r[P1]'s function subtype (Value.Subtype, record.go), leaving the value itself untouched.

	// OpNullRow: P1: cursor. Puts the cursor into a synthetic "NULL row" state
	// -- every subsequent OpColumn against it reads NULL, and OpRowid reads
	// NULL too (a row that doesn't exist has no rowid either) -- exactly
	// SQLite's own OP_NullRow, used for an unmatched LEFT JOIN row's
	// NULL-extension (join.go's package doc). Emitted by the join
	// codegen (vdbe_join_codegen.go) once, after a LEFT-joined cursor's
	// Rewind/Next loop finds no ON-match at all, to run the body exactly once
	// more with that side NULL-extended. A subsequent OpRewind on the same
	// cursor clears this state (see vdbeCursor.rewind).
	OpNullRow

	// RIGHT/FULL JOIN (emitRightOuterSweep, vdbe_join_codegen.go; semantics in
	// join.go's package doc): an ordinary nested loop in FROM order yielding
	// only real ON matches, then one second scan of the right-outer cursor,
	// after the main loop, emitting one NULL-extended row (every other cursor
	// NULL) per row that never matched any combination.
	//
	// OpRightJoinMark: P1: cursor. Marks the cursor's current row as having
	// satisfied its ON condition, emitted right after that test (WHERE does
	// not affect it). The mark bitmap is allocated on first use, sized to the
	// materialized rows, and lives as long as the cursor; only the sweep
	// opcodes read it.
	OpRightJoinMark

	// OpRightJoinSweepRewind: P1: the right-outer cursor (materialized here if
	// the main pass never reached it). P2: jump target when no unmatched row
	// exists. P4: []int, every other cursor. Positions P1 at its first unmarked
	// row and puts the P4 cursors into the OpNullRow state; jumps to P2 if none
	// (OpRewind's convention).
	OpRightJoinSweepRewind

	// OpRightJoinSweepNext: P1: cursor. P2: jump target (the sweep loop's
	// top -- mirrors OpNext's own convention: jump on FOUND, fall through
	// otherwise). P4: []int, the same OTHER-cursor list as the paired
	// OpRightJoinSweepRewind. Advances P1 to its NEXT never-marked row
	// (continuing the linear scan OpRightJoinSweepRewind started) and
	// re-nulls every cursor in P4; if none remains, falls through (the
	// sweep, and the whole RIGHT/FULL JOIN loop, is exhausted).
	OpRightJoinSweepNext

	// OpOuterColumn / OpOuterRowid read an enclosing query's live cursor, which
	// is how a correlated subquery runs. Its sub-Program executes in a vdbe
	// chained to the enclosing one (execWithParent / vdbe.parent); an outer
	// reference resolves at compile time to the ancestor cursor
	// (compileColumn's outer-chain walk), and P5 is how many frames up it is
	// (1 = immediate parent). The ancestor cursor is positioned on the current
	// enclosing row because the subquery runs within that row's evaluation.
	// Emitted only against ancestors whose cursors are live
	// (compiler.rowLive).
	OpOuterColumn // P1: cursor number (in the ancestor frame P5 levels up). P2: column index. P3: destination reg. P5: parent-frame levels up (>=1). r[P3] = that ancestor cursor's current row's column P2 (IPK/REAL-affinity normalized).
	OpOuterRowid  // P1: cursor number (in the ancestor frame P5 levels up). P2: destination reg. P5: parent-frame levels up (>=1). r[P2] = that ancestor cursor's current row's 64-bit rowid.

	// OpOuterAggReg reads a register of an ancestor frame: an aggregate-result
	// placeholder inside a subquery body, whose value (a finalized accumulator,
	// a GROUP BY key, or an anchor-row column/rowid) lives in the item program
	// compileAggItemProgram built.
	//
	// C has one flat program, so the subquery reads the same AggInfo registers
	// ("return AggInfoColumnReg(pAggInfo, pExpr->iAgg);", expr.c:4996, after
	// analyzeAggregate rewrites the reference inside the body, walker.c:82,
	// expr.c:7458-7469). Here the body is a separate Program, so the read
	// crosses a frame, with OpOuterColumn's P5 convention.
	OpOuterAggReg // P1: register number (in the ancestor frame P5 levels up). P3: destination reg. P5: parent-frame levels up (>=1). r[P3] = that ancestor frame's r[P1].

	// Sorter (ORDER BY) opcodes, after OP_SorterOpen/OP_MakeRecord/
	// OP_SorterInsert/OP_SorterSort/OP_SorterData/OP_SorterNext: a stable
	// multi-key sort of a scan's rows. See vdbe_sorter.go and
	// vdbe_sort_codegen.go.
	//
	// Rows stay native []Value in memory rather than serialized records, so:
	//
	//   - a record (sort keys then payload, built by OpMakeRecord) lives in its
	//     own record register space rr[], since Value has no record storage
	//     class; OpMakeRecord's P3 and OpSorterData's P2 address rr[];
	//   - there is no pseudo-cursor: OpRecordColumn reads rr[P1][P2] directly.
	OpSorterOpen   // P1: sorter number. P4(*sorterKeyInfo): the ORDER BY key's column count and each column's DESC flag. Opens sorter[P1] empty.
	OpMakeRecord   // P1: first source register. P2: register count (the ORDER BY key columns, then the payload/output columns, contiguous in r[]). P3: destination record register (rr[P3]). Packs r[P1..P1+P2-1] into one record, deep-copied so it survives past this loop iteration.
	OpSorterCheck  // P1: sorter number. P2: jump target. P3: first key register (r[P3..P3+nKey-1], the SAME range OpMakeRecord is about to pack as the record's leading key columns). Emitted only for a BOUNDED sorter (ORDER BY .. LIMIT, sorterKeyInfo.bound): if a row with this key would be discarded again by OpSorterInsert (vdbeSorter.loses), jump to P2, skipping the row's OpMakeRecord entirely. This is pushOntoSorter's own OP_IfNotZero/OP_Last/OP_IdxLE guard, which likewise runs BEFORE the record is built (select.c:832-859).
	OpSorterInsert // P1: sorter number. P2: source record register (rr[P2]). Appends that record to sorter[P1] as one new entry, split into its key (the leading columns, per sorter[P1]'s KeyInfo) and its payload (the rest).
	OpSorterSort   // P1: sorter number. P2: jump target. Stably sorts sorter[P1]'s entries by key (per-column DESC honored; a tie falls through to the next key column; a total tie preserves original insertion order) using the same value-level comparator (compareValues) every other comparison in this engine uses. If the sorter has no entries, jump to P2 (past the drain loop, exactly like OpRewind over an empty table); otherwise fall through with the first (now sorted) entry current.
	// OpSorterData's P3 asks for the serialization loss C's sorter has: its
	// drain reads columns with OP_Column over a pseudo-table
	// (select.c:8601-8607, expr.c:4995-4997), rebuilding each Mem from a serial
	// type (sqlite3VdbeSerialGet, vdbeaux.c:4123), so subtypes are dropped.
	// This sorter keeps them, so a grouped drain requests the drop
	// (clearJSONSubtype, vdbe_agg.go; aggDrainRow, vdbe_agg_codegen.go).
	OpSorterData // P1: sorter number. P2: destination record register (rr[P2]). P3: how many LEADING values of that record lose the JSON subtype (0: none). Copies the CURRENT sorted entry's payload columns into rr[P2].
	OpRecordColumn // P1: record register (rr[P1]). P2: column index within it. P3: destination register. r[P3] = rr[P1][P2].
	OpSorterNext   // P1: sorter number. P2: jump target. Advances to the next sorted entry; if one exists, jump to P2 (the drain loop's top, exactly like OpNext); otherwise fall through (the drain is exhausted).

	// DISTINCT opcodes: SELECT DISTINCT's dedup, done incrementally as each
	// output row is produced (whether a row duplicates an earlier one never
	// depends on later rows). The set (vdbe_distinct.go) keeps native []Value
	// rows and compares with keysEqual, the NULL-equal row equality GROUP BY
	// and compounds use, where C uses an ephemeral index probed with
	// OP_Found/OP_IdxInsert. Emitted by compileScanPlain and compileScanSorted.
	OpDistinctOpen // P1: distinct-set number. Opens distinctSets[P1] empty.
	OpDistinct     // P1: distinct-set number. P2: jump target. P3: record register (rr[P3]) holding the row's dedup key (its already-evaluated select-list output columns, packed by OpMakeRecord). If rr[P3] has been seen before (keysEqual against every key in distinctSets[P1]), jump to P2 (skip this row -- a duplicate); otherwise remember it and fall through (a new, distinct row).

	// Aggregate / GROUP BY opcodes, after OP_AggStep0/OP_AggStep1/OP_AggFinal.
	// They drive the engine's one accumulator implementation
	// (aggItem.step/finalize, sql_agg.go) over the item trees rewriteGroupExpr
	// produces, each compiled against registers holding the group's
	// accumulators, key and anchor row (compileAggItemProgram). The VM only
	// sequences: init accumulators, feed rows, detect group boundaries,
	// finalize and emit. See vdbe_agg.go and vdbe_agg_codegen.go.
	//
	// Accumulators are not Values, so like C's aggregate context they live in
	// VM state (vdbe.aggAccs), referenced by the *aggPlan / *aggResultInfo P4
	// payloads. Group keys live in record registers (rr[]).
	OpAggReset  // P4(*aggPlan): (re)initialize m.aggAccs -- a fresh set of per-group accumulator instances cloned from the plan's item templates. Emitted once before a whole-table aggregate scan, and once per group (at each group's first row) for GROUP BY.
	OpAggStep   // P1: row source. P2: STEP SEGMENT -- only the accumulators whose aggItem.stepSeg is P2 advance here, so a body emits one of these per segment, each preceded by that segment's own argument registers (updateAccumulator's one-OP_AggStep-per-aggregate shape, select.c:6942 inside the loop at :6824). A plan with nothing to separate has ONE segment (P2==0) and emits exactly one. Segment 0 also runs the min()/max() census and the anchor snapshot (magnetWalk). P3: source mode (0 = table cursor P1; 1 = record register rr[P1], whose LEADING block is [col0..colN-1, rowid..] -- the GROUP BY sorter payload, whose own trailing scan-order sequence number and group-key columns this opcode does not read; see aggRowSplit). P4(*aggPlan): builds the row's evalCtx and feeds it to every accumulator in m.aggAccs (aggItem.step), advancing each aggregate over this one row.
	OpAggResult // P1: destination register. P3: group-key record register rr[P3] holding this group's GROUP BY key tuple (or -1 for a whole-table aggregate, whose key is nil). P4(*aggResultInfo): finalizes the selected item's accumulators (aggItem.finalize) into a register block -- select.c:6733/:6786's OP_AggFinal into AggInfoFuncReg -- and RUNS that item's compiled program over it (itemPlan.prog), leaving one select-list, HAVING, or ORDER BY value for the group in r[P1]. An item the compiler cannot lower is the STATEMENT's error, never a second route; see aggItemLowerable.
	OpGroupSame // P1, P3: two group-key record registers rr[P1], rr[P3]. P2: jump target. P4([]string, optional): each key column's collating sequence, when the GROUP BY groups under a non-BINARY one. Jumps to P2 iff the two key tuples are equal under GROUP BY's grouping semantics (keysEqualGrouping: compareValues elementwise, so two NULLs compare EQUAL -- unlike "="; nil P4 is every column BINARY); falls through on a boundary (different group).
	OpRecCopy   // P1: source record register rr[P1]. P2: destination record register rr[P2]. Deep-copies rr[P1] into rr[P2] (used to remember the current group's key tuple across the drain loop).

	// GROUP BY + DISTINCT opcodes (vdbe_group_distinct.go). DISTINCT over GROUP
	// BY dedups the finalized, HAVING-filtered rows by output tuple, but which
	// of several groups with the same output survives (and so whose GROUP BY
	// and ORDER BY keys feed the final sort) is first-seen in scan order, not
	// the key order the sorted drain visits. So every group's output, key,
	// first-seen proxy and ORDER BY keys are collected here, then resolved in
	// one pass: dedup, sort, LIMIT/OFFSET, emit. Single-table only (see
	// compileScanGroupBy).
	OpGroupBatchAppend // P1: record register rr[P1] holding one group's collected entry ([minRowid, groupKey.., out.., orderKeys..], see groupBatchPlan). Appends it to the VM's group batch (vdbe.groupBatch).
	OpGroupBatchFinal  // P4(*groupBatchPlan): resolves the whole collected batch -- sort by first-seen order (the collected min-rowid), DISTINCT-dedup by output tuple, sort survivors (by ORDER BY keys if present, else by group key), slice by LIMIT/OFFSET, and append each surviving row directly to the program's result rows. Terminal for the groups it covers: unlike OpResultRow, it may append any number of rows (including zero) in one step.

	// GROUP BY hash aggregation (vdbe_hashagg.go), compiled by
	// compileScanGroupByHash instead of the sort-based scan+drain when a GROUP
	// BY has no DISTINCT and no trailing ORDER BY: one hash bucket per group,
	// in scan order. Bucket equality (hashAggKeyBytes) matches GROUP BY's
	// (keysEqual/compareValues: an Int and an equal Float share a bucket), and
	// buckets drain in key order (keyTupleLess), the order the sorter would
	// produce, so rows and their order match. A current bucket is finalized by
	// the existing OpAggResult.
	OpHashAggStep // P1: this row's group-key record register rr[P1]. P2: STEP SEGMENT, exactly OpAggStep's (see there) -- the bucket lookup and the row gather are idempotent for a row, so a multi-segment body repeating them advances only the accumulators the segment names. P4(*aggPlan): finds or creates (newAggAccs) the hash bucket for that key (remembering the key tuple on first creation), makes it the live accumulator set (m.aggAccs), and steps this row (gathered from plan.cursors, exactly OpAggStep's P3==0 mode) into it.
	OpHashAggSort // P2: jump target. Finalizes the VM's hash-aggregate buckets into group-key-ascending order (keyTupleLess). If there are no buckets at all, jump to P2 (past the drain loop, exactly like OpSorterSort over an empty sorter); otherwise fall through with the first (sorted) bucket current.
	// OpSegFilterCount is the JIT path's filter opcode: it counts rows of a
	// columnar table (segment_read.go) satisfying a conjunction of integer
	// comparisons into r[P1]. P1: destination. P2: cursor. P4(*segFilterPlan):
	// the predicates, resolved at compile time.
	//
	// It replaces a whole Rewind/Column/compare/AggStep/Next loop and is
	// introduced by a peephole (segment_peephole.go), not the compiler: the
	// compiler alone decides what a query means; this only changes how it runs,
	// and leaves anything it does not recognise alone.
	OpSegFilterCount

	// OpSegOrderLimit answers "ORDER BY <int columns> LIMIT n" from a COLUMNAR
	// table, materializing the winning rows for OpSegEmitRow to hand out.
	//
	// P1: base register the emitted row lands in. P2: cursor number.
	// P3: jump target -- the emit loop -- taken only when it SERVES.
	// P4(*segOrderPlan): key columns and directions, output columns, limit.
	//
	// Like OpSegFilterCount it is a GUARD: it declines by falling through to the
	// ordinary sorter loop, which the peephole leaves in the program precisely
	// so a decline has somewhere to go.
	OpSegOrderLimit

	// OpSegEmitRow hands out the next row OpSegOrderLimit materialized, copying
	// it into r[P1..P1+P3) and falling through; when they are exhausted it
	// jumps to P2 instead. It is the loop counterpart of OpSorterNext, and it
	// exists because the segment fast paths produce a row SET where
	// OpSegFilterCount produced a single value.
	OpSegEmitRow

	// OpSegHashAgg drives a whole GROUP BY scan from a columnar table into the
	// same hash buckets the ordinary loop fills, leaving the drain unchanged.
	// P1: cursor. P2: jump target (the draining OpHashAggSort), taken only when
	// it serves. P4(*segGroupPlan): which columns feed which registers. Per row
	// it writes the key and aggregate arguments into the scan body's registers
	// and calls hashAggStepRow, so no aggregate is reimplemented
	// (segment_group.go).
	// OpSegProgram runs a compiled loop body over a columnar table: the
	// recogniser lowered the opcodes between Rewind and AggStep into the JIT's
	// IR, and this runs the machine code. P1: destination. P2: cursor. P3:
	// jump target on success. P4(*segProgPlan): the lowered body. It is the
	// general form of OpSegFilterCount.
	OpSegProgram

	OpSegHashAgg

	OpHashAggData // P2: destination record register rr[P2]. Copies the CURRENT bucket's group-key tuple into rr[P2] and makes that bucket's accumulators the live set (m.aggAccs), so a following OpAggResult finalizes THIS group.
	OpHashAggNext // P2: jump target. Advances to the next bucket in sorted order; if one exists, jump to P2 (the drain loop's top, exactly like OpSorterNext); otherwise fall through (the drain is exhausted).

	// Subquery opcodes. Each runs a subquery compiled into its own Program (P4).
	// It is uncorrelated or correlated (p5Correlated for OpSubquery/OpExists,
	// inSubPlan.correlated for OpInSub), as decided when the sub-Program bound
	// or did not bind an outer column:
	//
	//   - uncorrelated: run once and cache the rows in a per-execution slot
	//     (vdbe.subCache, P2/P3), as SQLite's OP_Once does;
	//   - correlated: re-run on every evaluation, in a vdbe chained to the
	//     enclosing frame (execWithParent) so OpOuterColumn/OpOuterRowid read
	//     the current row.
	OpSubquery // P1: base of the dest register block. P2: sub-cache slot (uncorrelated only). P3: block WIDTH (0/1 = the ordinary scalar form; >1 = a ROW-VALUE subquery operand, emitRowSubqueryProbe). P4(*Program): the subquery. r[P1..P1+width-1] = the first result row's first width columns, all NULL if it produced no rows. Uncorrelated: run once (cached). Correlated (p5Correlated): re-run per evaluation against the enclosing frame. The compiler guarantees the sub-Program's result-column count equals width (else it falls back).
	OpExists   // P1: dest reg. P2: sub-cache slot (uncorrelated only). P3: NOT flag (0/1). P4(*Program): [NOT] EXISTS. r[P1] = 1 if the sub-Program produced >=1 row else 0, inverted when P3!=0. Never NULL. Uncorrelated: run once (cached). Correlated (p5Correlated): re-run per evaluation.
	OpInSub    // P1: base of the probe (X) register block, len(plan.affs) wide. P2: dest reg. P3: sub-cache slot (uncorrelated only). P4(*inSubPlan): "X [NOT] IN (SELECT ...)", scalar or ROW-VALUE. r[P2] = the three-valued IN result (finalizeIn), the per-column LHS-vs-element affinity/collation carried in the plan. Uncorrelated: run once (cached). Correlated (plan.correlated): re-run per evaluation. The compiler guarantees the sub-Program's result-column count equals the probe arity (else it declines).
	OpRowSub   // P1: base of the row-value operand's register block, len(plan.affs) wide. P2: dest reg. P3: sub-cache slot (uncorrelated only). P4(*rowSubPlan): "(a,b) <op> (SELECT x,y ...)" -- a ROW-VALUE comparison whose OTHER operand is a multi-column subquery. r[P2] = rowSubCompare's three-valued result over the subquery's FIRST row (an all-NULL row when it produced none). Uncorrelated: run once (cached). Correlated (plan.correlated): re-run per evaluation. The compiler guarantees the sub-Program's result-column count equals the row value's arity (else the statement is rejected, exactly as C SQLite rejects it).

	// OpSubCacheReset clears run-once cache slots P1..P1+P2-1 so their
	// subqueries run again.
	//
	// In C, OP_Program gives each invocation a fresh frame with a zeroed OP_Once
	// bitmap (vdbe.c:7581-7582), so trigger-body subqueries re-run per firing.
	// RETURNING is coded inline, not as a sub-program (trigger.c:1015-1017); C
	// gets per-row re-evaluation by marking a RETURNING subquery that reads the
	// modified table SF_Correlated (sqlite3ProcessReturningSubqueries,
	// trigger.c:998) and then EP_VarSelect (trigger.c:953-956), which suppresses
	// the OP_Once wrapper (expr.c:3889). Lacking EP_VarSelect on an opcode, this
	// resets the slots at the top of the RETURNING block instead.
	//
	// Emitted only by emitReturning, for a RETURNING block executed more than
	// once per statement (a scan loop's, not an unrolled VALUES tuple's).
	OpSubCacheReset // P1: first cache slot. P2: slot COUNT. Clears subCache[P1..P1+P2-1] so those slots' subquery opcodes re-run.

	// WRITE opcodes (vdbe_write.go). These mutate a *DB write handle's row
	// store (m.wctx.db). Expression evaluation (VALUES tuples, SET right-hand
	// sides, WHERE) runs as ordinary bytecode; the mutation opcodes delegate to
	// the row-store and schema helpers that own the mutation rules. Only write
	// programs contain them.
	OpOpenWrite     // P1: cursor number. P4(*tableMeta): opens a row-store cursor (openRowStoreCursor) over the table's CURRENT logical rows (materialized ascending-rowid, normalizeRow'd), for the OpColumn/OpRowid/OpRewind/OpNext scan an UPDATE/DELETE WHERE drives.
	OpHaltError     // P4(string): halts the program with that error unconditionally -- SQLite's OP_Halt with a non-zero error code and P4 error text (how a failed CHECK constraint aborts).
	OpHaltIfNull    // P3: register. P4(string): error text. Halts the program with that error if r[P3] is NULL -- SQLite's OP_HaltIfNull, how a NOT NULL constraint is enforced.
	OpMustBeInt     // P1: register, P2: jump address. Falls through when r[P1] holds an INTEGER, jumps to P2 otherwise -- SQLite's OP_MustBeInt (which converts where it can; this engine treats any non-INTEGER as "no explicit rowid given" and jumps, matching decideRowid's own rule).
	OpNewRowid      // P2: destination register. P4(*tableMeta): allocates the table's next rowid (AUTOINCREMENT sequence, else max+1) -- SQLite's OP_NewRowid.
	OpMemMax        // P1: rowid register. P4(*tableMeta): raises the AUTOINCREMENT table's sequence to r[P1] -- SQLite's OP_MemMax (autoIncStep, insert.c:520).
	OpRaise         // P1: RAISE action (int conflictAction; conflictIgnore is a skip-the-firing-row signal, else abort/fail/rollback). P2: message register (ignored for IGNORE). Raises the trigger RAISE(...) error -- SQLite's OP_Halt with the RAISE code, or the IGNORE control signal.
	OpParam         // P1: 0=OLD, 1=NEW, 2=an upsert's EXCLUDED, 3=a RETURNING clause's affected row. P2: column index (or -1 for the rowid pseudo-column). P3: destination register. Reads the trigger body's OLD/NEW row value -- SQLite's OP_Param reading the parent frame -- or, for P1>=2, the pseudo-row OpPseudoRow snapshotted.
	OpLimitCounter  // P1: register holding a just-coded LIMIT/OFFSET expression, P2: 1 for an OFFSET, 0 for a LIMIT. Coerces r[P1] to INTEGER or raises "datatype mismatch" -- SQLite's OP_MustBeInt with no jump target (select.c:2549/2558) -- and, for an OFFSET, clamps a negative to 0, which is codeOffset's OP_IfPos never firing on one (select.c:2492).
	OpPseudoRow     // P1: which pseudo-row (2 = an upsert's EXCLUDED, 3 = a RETURNING clause's affected row -- OpParam's own P1 numbering). P2: the row's rowid register, or -1. P4([]int): the registers holding its columns, in column order. Snapshots that row onto the machine (vdbe.trigExcluded / vdbe.trigReturn) so a SUB-program compiled inside the block can read it through OpParam. No C counterpart: lookupName rewrites both to a TK_REGISTER naming the block directly (resolve.c:572-576 for excluded, :587-593 for RETURNING) because C codes the subquery into the SAME register file.
	OpFireTriggers  // P1: NEW-row base register (or -1 if the event has no NEW row). P2: NEW rowid register. P3: OLD-row base register (or -1). P4(*triggerFirePlan): fires every matching trigger's body (each a compiled sub-program) for this row -- SQLite's OP_Program per trigger.
	// OpTriggerBodyRouted is a TEMP trigger's body statement whose unqualified
	// target resolves only inside an ATTACHed database. See
	// vdbe_trigger_routed.go for the whole mechanism and its C citations; it is
	// a whole sub-program on its own, never mixed with other instructions.
	OpTriggerBodyRouted // P4(*routedTriggerBody): compiles the body statement against the attachment's own write session at FIRE time and runs it there, with this machine's OLD/NEW row bound.
	OpUpsertFind        // P1: candidate row base register, P2: no-conflict jump address, P3: existing-row dest base register. P4(*upsertPlan): probes the target constraint for the candidate; jumps to P2 when there is no conflict (plain insert), errors on a conflict with a NON-target constraint, else loads the conflicting row into P3.. (+ its rowid into the plan's rowid reg) and falls through to the DO UPDATE / DO NOTHING block.
	OpUpsertStore   // P1: new record register, P2: existing-rowid register, P3: new-rowid register. P4(*updatePlan): re-stores the conflicting row as an UPDATE (delete existing rowid, insert new record under P3) -- the storage half of an upsert's DO UPDATE, journaled like OpUpdateRow.
	OpInsert        // P1: cursor, P2: record register, P3: rowid register. P4(*insertPlan): probes the unique indexes + rowid, resolves any conflict per plan.action (SQLite's ON CONFLICT baked into the insert), stores the record, journals the undo, updates last_insert_rowid and the affected-row count.
	OpDelete        // P1: cursor. P4(*tableMeta): deletes the row the cursor currently points at, immediately -- SQLite's OP_Delete ("Delete the record at which the P1 cursor is currently pointing"). Counts one affected row.
	OpClearTable    // P4(*tableMeta): empties the table's b-tree and every index's in one go, counting the rows it held -- SQLite's OP_Clear (vdbe.c:5896) over sqlite3BtreeClearTable, which delete.c:471-492 emits for a DELETE with no WHERE clause, no trigger and no foreign key. The TRUNCATE OPTIMISATION.
	// OpSkipIfRowGone guards the gap between an UPDATE's BEFORE triggers and
	// its store: a BEFORE UPDATE trigger is free to DELETE the very row that
	// was about to be updated, and when it does, C SQLite abandons that
	// row entirely -- no store, and the AFTER triggers never fire. Verified
	// directly: over a single row with "BEFORE UPDATE ... DELETE FROM t1" and
	// an AFTER trigger logging a row, "UPDATE t1 SET b=3" leaves BOTH the
	// table and the log empty (SQLite's own conflict3.test 13.3.*), whereas a
	// BEFORE trigger deleting some OTHER row leaves the update to proceed
	// normally. SQLite spells this OP_NotExists on the re-seeked cursor.
	OpSkipIfRowGone // P1: register holding the OLD rowid. P2: jump target (the row's end). P4(*tableMeta): the table. Jumps if that rowid no longer exists in the row store.
	OpUpdateRow     // P1: cursor (positioned on the OLD row), P2: record register holding the NEW row image, P3: register holding the NEW rowid. P4(*updatePlan): re-stores the row -- SQLite's OP_Delete+OP_Insert pair with OPFLAG_ISUPDATE. Records an undo entry so a later constraint failure rolls the whole statement back.
	// OpVInsert is the INSERT arm of SQLite's OP_VUpdate -- the spelling
	// insert.c:1561 emits ("sqlite3VdbeAddOp4(v, OP_VUpdate, 1, pTab->nCol+2,
	// regIns, pVTab, P4_VTAB);", with P1==1 meaning "publish
	// last_insert_rowid()"). See vdbe_vtab_write.go for the two places its
	// operands deliberately differ from the C's apVal block and why.
	OpVInsert // P1: base register of the first VALUES tuple's value block. P2: values per tuple (blocks are contiguous, tuple t occupying r[P1+t*P2 .. P1+(t+1)*P2-1]). P3: tuple count, or -1 for "the rows are the ones OpVInsertRow collected" (the SELECT source). P4(*vtabInsertPlan): hands those already-evaluated rows to the virtual table's writer (insertIntoVtab, this engine's xUpdate) -- SQLite's OP_VUpdate, whose own body likewise only gathers registers and calls the module ("apArg[i] = pX; ... rc = pModule->xUpdate(pVtab, nArg, apArg, &rowid);", vdbe.c:8739-8743).
	// OpVInsertRow is the SELECT source's COLLECT step, and it stands to
	// OpVInsert exactly as OpVWriteRow (below) stands to OpVWrite. The C splits
	// an INSERT ... SELECT into a collect and a replay too, and for a virtual
	// table the split is forced: a view or vtab target always has a trigger, so
	// "if( pTrigger || readsTable(pParse, iDb, pTab) ){ useTempTable = 1; }"
	// (insert.c:1167-1169) picks template 4, which drains the SELECT coroutine
	// into an ephemeral table ("OP_OpenEphemeral, srcTab, nColumn" then
	// OP_MakeRecord/OP_NewRowid/OP_Insert, insert.c:1189-1193) BEFORE the
	// insertion loop reads it back with "OP_Column, srcTab, k" (insert.c:1424).
	// The one deviation is the same one OpVInsert's note above records: the
	// replay is ONE instruction rather than one OP_VUpdate per row.
	OpVInsertRow // P1: base register of this source row's value block. P2: values per row. Appends a COPY of that block to the statement's collected row list (vdbe.vtabInsert) -- insert.c:1191-1193's MakeRecord+Insert into srcTab.
	// OpVWriteRow / OpVWrite are the UPDATE and DELETE arms of OP_VUpdate,
	// split as C splits them: updateVirtualTable collects one record per
	// changed row into an ephemeral (update.c:1320-1327) and a second loop
	// replays them into OP_VUpdate (update.c:1343-1348); DELETE collects rowids
	// into a RowSet (delete.c:582) replayed into OP_VUpdate (delete.c:626,
	// 644). OpVWriteRow collects; OpVWrite replays.
	//
	// The replay is one instruction for the whole statement because the module
	// side (deleteVtab/updateVtab, deleteFromFts3/updateFts3) keeps a
	// statement's fts3 pending terms locally; one call per row would write one
	// index segment per row. The per-row xUpdate calls happen inside it.
	OpVWriteRow     // P1: base register of this row's evaluated SET values (statement order; ignored when P3==0). P2: register holding the row's rowid, read from the scan cursor with OpRowid. P3: how many SET values (0 for a DELETE). Records the row as SELECTED by the WHERE -- update.c:1320-1327 / delete.c:582.
	OpVWrite        // P4(*vtabWritePlan): hands every row OpVWriteRow recorded to the virtual table's writer (deleteVtab/updateVtab, this engine's xUpdate) -- update.c:1348 / delete.c:644.
	// OpSchemaWritePre / OpSchemaWriteRow / OpSchemaWrite are the catalog-write
	// opcodes: DML targeting sqlite_master under "PRAGMA writable_schema=ON".
	// In C the flag only stops tabIsReadOnly (delete.c:103-107) from refusing,
	// and the ordinary codegen runs (vdbe_schema_write.go).
	//
	// OpSchemaWritePre comes first because what it checks is prepare-time in
	// C: sqlite3IsReadOnly (insert.c:1009, update.c:411, delete.c:388), SET /
	// IDLIST resolution (update.c:500) and VALUES arity (insert.c:1249-1252).
	// It runs as an opcode rather than at compile time because each answer
	// depends on connection state (writable_schema, db.wsEdits) that the
	// write-program cache key (cachedWriteProgram) does not cover.
	OpSchemaWritePre // P4(*schemaWritePlan): the catalog write's prepare-time checks -- the writable_schema flag, the shapes this overlay cannot reproduce (writableSchemaPreflight), the SET/IDLIST column names, and an INSERT's VALUES arity. Raises; records nothing.
	OpSchemaWriteRow // P1: base register of this row's evaluated SET values (statement order; ignored when P3==0). P2: register holding the row's 1-based catalog rowid, read from the scan cursor with OpRowid. P3: how many SET values (0 for a DELETE). Records the row as SELECTED by the WHERE -- delete.c:582's OP_RowSetAdd, update.c:1320-1327's ephemeral collect.
	OpSchemaWrite    // P1/P2/P3: an INSERT's tuple register blocks, laid out exactly like OpVInsert's (base, values per tuple, tuple count); unused by UPDATE/DELETE. P4(*schemaWritePlan): applies the whole statement to the writable_schema overlay (db.wsEdits, schema_write_direct.go) in one call.
	OpDdl           // P4(*ddlPlan): delegates a DDL or utility statement -- CREATE TABLE/INDEX/VIEW/TRIGGER/VIRTUAL TABLE, DROP INDEX/TABLE/VIEW/TRIGGER, ALTER, PRAGMA, ANALYZE, REINDEX, VACUUM, ATTACH, DETACH -- to the existing implementation of that statement kind (db.CreateTable/.../db.execPragma/db.execAttach). rowsAffected stays 0.
	OpTxn           // P4(*txnPlan): runs one transaction-control verb (BEGIN/COMMIT/END/ROLLBACK/SAVEPOINT/RELEASE/ROLLBACK TO), whose verb and savepoint name are parsed at COMPILE time -- exactly where SQLite parses them, emitting OP_AutoCommit (vdbe.c:4013) or OP_Savepoint (vdbe.c:3823) with the name already in P4.

	// OpOpenDerived: P1: cursor. P2: run-once cache slot. P4(*derivedSource):
	// opens a cursor over a derived table (FromItem.Subquery) or a view
	// (resolveViewSource). The sub-Program runs once, its rows cached in
	// subCache[P2] (both are uncorrelated), and installed in an in-memory
	// cursor over P4.tbl. It runs against P4.dbIdx's pager (dbIndexOf: 0 = this
	// Program's pager, i+1 = attachedReaders[i]), non-zero only for a
	// foreign-qualified view, which is compiled against its owning pager.
	// Later OpRewind/OpNext/OpColumn read the rows like a base table's (see
	// emitJoinLoops).
	OpOpenDerived

	// OpSeekRowidHint: P1: cursor. P2: key register. Makes cursor P1's next
	// OpRewind fetch only the row whose rowid == r[P2] (SeekRowidSegments,
	// segment_seek.go), when r[P2] is an INTEGER; any other key leaves a full
	// scan. Emitted by emitJoinLoops after OpOpenRead when detectRowidSeekKey
	// found "rowid = <const/param>" (or the IPK column). The conjunct is still
	// evaluated in the loop, so this only restricts the rows scanned.
	//
	// P3==1 marks a correlated (join inner-side) seek: emitted inside the outer
	// loop (emitJoinSeekHint, vdbe_join_seek.go) before the inner OpRewind, it
	// sets vdbeCursor.seekReseek so the cursor re-materializes on every rewind.
	// A non-integer key then does a full inner scan for that iteration.
	OpSeekRowidHint

	// OpSeekIndexHint: P1: cursor. P2: key register. P4: *indexSeekHint (index
	// root, key affinity, leading-column collation). Makes cursor P1's next
	// OpRewind fetch only rows whose indexed leading column equals r[P2], via
	// the posting list (SeekIndexRowidsSegments) and rowid fetches. The key is
	// coerced with P4.aff and compared under P4.coll, which detectIndexSeekKey
	// verified match the WHERE comparison. The conjunct is still evaluated in
	// the loop. P3==1 is a correlated seek, as for OpSeekRowidHint.
	OpSeekIndexHint

	// OpAutoIndexOrder: P1: cursor. P4: *autoIndexKey. Materializes cursor P1 if
	// needed and reorders its rows into the order SQLite's transient automatic
	// index would yield (constructAutomaticIndex's key: equality columns, then
	// covering columns, then rowid); see autoIndexKey (where_plan.go). It only
	// reorders; every conjunct is still evaluated in the loop. A cursor it
	// cannot reorder (no b-tree, or seek-driven) keeps its order.
	OpAutoIndexOrder

	// OpNotExists: P1 cursor, P4 the table. Re-seeks the cursor to its current
	// row: if the row is gone, jump to P2; otherwise re-read its current content
	// and fall through. With P5 != 0 the row is named by the key in register
	// P3, as C's OP_NotExists always is (vdbe.c:5521/5524); compileUpdateStmt's
	// UPDATE ... FROM loop needs that form (update.c:861-864).
	//
	//	rc = sqlite3BtreeTableMoveto(pCrsr, iKey, 0, &res);
	//	...
	//	pC->cacheStatus = CACHE_STALE;
	//	                                              -- vdbe.c:5536/5540
	//
	// The re-read matters: OLD.* loads after this guard see a row a cascade
	// rewrote (reseekRowStore, vdbe_cursor.go).
	//
	// sqlite3GenerateRowDelete places it so:
	//
	//	/* Seek cursor iCur to the row to delete. If this row no longer exists
	//	** (this can happen if a trigger program has already deleted it), do
	//	** not attempt to delete it or fire any DELETE triggers.  */
	//	iLabel = sqlite3VdbeMakeLabel(pParse);
	//	opSeek = HasRowid(pTab) ? OP_NotExists : OP_NotFound;
	//	if( eMode==ONEPASS_OFF ){
	//	  sqlite3VdbeAddOp4Int(v, opSeek, iDataCur, iLabel, iPk, nPk);
	//	                                              -- delete.c:768-774
	//
	// iLabel is at the end, so the jump skips OLD.*, BEFORE, the delete and
	// AFTER. compileDeleteStmt emits it only when triggers fire: bComplex
	// (delete.c:358) clears WHERE_ONEPASS_MULTIROW (delete.c:498), and a
	// one-pass plan needs no guard. It is emitted again after the BEFORE
	// program (delete.c:817-823). update.c does the same (update.c:877, and
	// :998 after BEFORE, which compileUpdateStmt answers with OpSkipIfRowGone).
	OpNotExists

	// The recursive-CTE QUEUE opcodes: generateWithRecursiveQuery
	// (select.c:2666-2836) as a program of its own, one per reference to a
	// recursive CTE. compileRecursiveCTE (vdbe_recursive_cte.go) lays them out
	// line for line against that function; see its doc comment for the listing
	// and for what each C instruction became.
	//
	// OpRecQueueOpen: P4(*recQueueSpec). Creates this machine's queue --
	// select.c:2733-2766's OP_OpenPseudo (the current row), OP_OpenEphemeral
	// (the queue, FIFO or keyed on the ORDER BY) and, for UNION, the iDistinct
	// ephemeral -- and loads the LIMIT/OFFSET counters computeLimitRegisters
	// (select.c:2703) put in registers.
	OpRecQueueOpen
	// OpRecQueueFill: P1: 0 for the setup query, 1 for a recursive step. P4(*
	// Program): runs that already-compiled SELECT to completion and pushes every
	// row it returns onto the queue (select.c:2791's sqlite3Select(pSetup,
	// &destQueue) and :2825's sqlite3Select(p, &destQueue)), deduplicating
	// against every row ever queued for UNION (SRT_DistFifo/SRT_DistQueue,
	// select.c:1328/:1475). A recursive step reads the row most recently popped
	// through OpOpenDerived's recSelf source.
	OpRecQueueFill
	// OpRecQueuePop: P2: jump target when the queue is empty. P3: base register
	// of the popped row. select.c:2796-2805: OP_Rewind queue, OP_NullRow
	// current, OP_RowData/OP_Column into regCurrent, OP_Delete.
	OpRecQueuePop
	// OpRecQueueOffset: P2: jump target while an OFFSET is still skipping
	// rows. codeOffset (select.c:879-888) at select.c:2809: OP_IfPos on the
	// OFFSET counter, so a skipped row is still EXPANDED.
	OpRecQueueOffset
	// OpRecQueueLimit: P2: jump target once the LIMIT is reached.
	// select.c:2812-2815's OP_DecrJumpZero, BEFORE the recursive step: it jumps
	// only when the counter reaches exactly zero (vdbe.c:7788-7794), so a
	// negative LIMIT never stops the recursion.
	OpRecQueueLimit
	// OpUpsertReload: P1: existing-rowid register. P4(*updatePlan): update.c's
	// After-BEFORE-trigger-reload-loop (:1000-1017) for an upsert's DO UPDATE:
	// every column the SET list does not assign is re-read from the live row
	// into plan.rowRegs, because a BEFORE UPDATE program may have edited it.
	// It runs before the constraint checks, which read those registers.
	OpUpsertReload

	// OpMultiOrTag: P1: cursor number. P2: register holding a pass number.
	// Files the cursor's CURRENT row under that pass of a multi-table
	// WHERE_MULTI_OR level -- the first sub-scan of the case-5 arm
	// (wherecode.c:2409) that would emit it for the outer rows now bound. See
	// multiOrOrder.passes and emitMultiOrGroups (vdbe_join_codegen.go).
	OpMultiOrTag
	// OpMultiOrSort: P1: cursor number. P4: *multiOrOrder. Permutes the
	// cursor's materialized rows into (pass, that pass's sub-scan key) order,
	// using the pass numbers OpMultiOrTag filed every row under -- the order the
	// sub-scans emit them in, each row once (OP_RowSetTest, wherecode.c:2450).
	// Only the ORDER changes: the loop body still tests every term.
	OpMultiOrSort

	// Subroutines: OP_Gosub and OP_Return (vdbe.c:1119, 1152). OpGosub P1, P2:
	// r[P1] = this instruction's address, then jump to P2. OpReturn P1, P3: if
	// r[P1] is an INTEGER, continue after the instruction at that address --
	// the one after the OpGosub that set it; otherwise fall through when P3 is
	// set, and fail when it is not (C asserts).
	OpGosub
	OpReturn

	// OpJIT never appears in a compiled program. vdbe_jit.go puts it, in a
	// COPY of the program that only run() dispatches on, at each pc where the
	// VDBE enters the program's native code -- so an instruction with no
	// native entry pays nothing for the JIT. Its operands are the original
	// instruction's, which run() executes instead when native code hands that
	// pc back.
	OpJIT

	numOpCodes // sentinel: count of opcodes, for the disassembler's name table.
)

// indexSeekHint is the P4 payload of OpSeekIndexHint: everything the runtime
// needs, beyond the key register, to configure a secondary-index leading-column
// equality seek on a cursor -- the index b-tree root page, the affinity the
// WHERE comparison applies to the key (pre-computed by detectIndexSeekKey via
// comparisonAffinity, so the runtime coerces the key identically to the
// comparison it is standing in for), and the index's leading-column collating
// sequence (BINARY/NOCASE/RTRIM). See OpSeekIndexHint's doc comment.
type indexSeekHint struct {
	root uint32
	aff  affinity
	coll string
	// leadingCol is the TABLE column the index leads on -- what a SEGMENT seek
	// needs in place of an index b-tree. See indexSeekPlan.leadingCol.
	leadingCol int
}

// multiOrOrder is the autoIndexKey variant OpAutoIndexOrder applies when the
// winning loop is a WHERE_MULTI_OR union of sub-scans (where_plan_multior_r37a.go).
//
// sqlite3WhereCodeOneLoopStart's case-5 arm (wherecode.c:2404) runs one whole
// sub-WHERE per disjunct, in clause order, and each sub-scan's body starts with
// an OP_RowSetTest that emits the row only the FIRST time that rowid is seen. So
// a row is emitted during the pass of the LOWEST-numbered disjunct it satisfies,
// in that sub-scan's own index order -- which is the two-level sort key
// multiOrOrderCursor (vdbe_cursor.go) applies. Only the ORDER changes: rows
// satisfying no disjunct at all sort last and are filtered by the loop body's
// WHERE exactly as before.
type multiOrOrder struct {
	// disjuncts are the OR's subterms in clause order, each compiled against a
	// row-in-registers block (compileSelfRowExpr); scopes is the FROM scope
	// they were compiled against. The runtime asks each program which
	// disjunct a materialized row satisfies (selfRowExpr.eval).
	//
	// C compiles each disjunct into a sub-scan (sqlite3WhereBegin with
	// WHERE_OR_SUBCLAUSE, wherecode.c:2431-2432) gated by OP_RowSetTest
	// (wherecode.c:2450). Here the rows are already on one cursor and only
	// their order is being reconstructed, so the nearest compiled form is C's
	// own for evaluating against a held row: a register block with the rowid
	// just below it (pParse->iSelfTab, expr.c:5047-5068), i.e. selfRowExpr.
	disjuncts []*selfRowExpr
	scopes    []tableScope
	// keys[i] is the key of the index disjunct i's own sub-scan walks, or nil for
	// that sub-scan's plain ascending-rowid table scan.
	keys []*autoIndexKey

	// passes, when set, makes this the MULTI-TABLE form, and disjuncts/scopes are
	// then unused: passes[i] is sub-scan i's entry of the OR's own clause, which
	// may name a table bound further OUT -- "t2.rowid<='a' OR t1.c0<=t2.c0" --
	// so which sub-scan a row belongs to changes with the outer row. The join
	// codegen compiles the passes into the level itself, right before its own
	// OpRewind (emitMultiOrGroups, vdbe_join_codegen.go): an OpMultiOrTag loop
	// files every row under the first pass it satisfies, and OpMultiOrSort then
	// orders the cursor by (pass, that pass's key). That is once per entry into
	// the level, exactly where the case-5 arm empties its RowSet.
	passes []Expr
}

// inSubPlan is the P4 payload of OpInSub: the compiled membership sub-Program,
// the (compile-time-constant) affinity applied to each set element before
// comparing it against the probe value X, and the NOT flag.
//
// That affinity COMBINES both sides -- comparisonAffinity (affinity.go) over
// X[i] and the subquery's i'th output column -- and is deliberately NOT the
// IN-LIST form's asymmetric X-only rule: "x IN (SELECT a FROM t3)" with a TEXT
// x and an INTEGER a must coerce under NUMERIC to match C SQLite
// (subquery.test). See compileInSubquery's comment (vdbe_codegen.go) for the
// measurement.
type inSubPlan struct {
	prog *Program
	// affs and colls are PER COLUMN, parallel to each other, and their shared
	// length is the membership test's ARITY: 1 for an ordinary "X IN (SELECT
	// y ...)", N for a ROW VALUE ("(a,b) IN (SELECT x,y ...)"), whose columns
	// are compared independently. OpInSub's P1 is the base of that many
	// consecutive probe registers.
	//
	// Each affs[i] is X[i]'s affinity COMBINED with the subquery's i'th result
	// column's, and each colls[i] the collating sequence resolveCompareCollation
	// picks for that same pair -- see inSubCollation / subqueryOutputCollations
	// (subquery_validate.go) for why the subquery side contributes both, and for
	// the oracle evidence.
	affs  []affinity
	colls []string
	not   bool
	// correlated marks a subquery that references an outer-scope column and so
	// must be RE-RUN (against the enclosing frame) on every evaluation rather
	// than materialized once and cached -- see the OpInSub doc comment above.
	correlated bool
}

// rowSubPlan is the P4 payload of OpRowSub: a ROW-VALUE comparison one of
// whose operands is a multi-column SUBQUERY -- "(a,b) = (SELECT x,y FROM t)"
// and every other comparison operator, in either operand order. Unlike every
// row-value shape the PARSER handles (desugarRowCompare and friends,
// sql_parser.go), this one cannot be rewritten into a scalar boolean tree: the
// subquery must be evaluated ONCE and its whole first row compared, which is
// what rowSubCompare (vdbe.go) does with the fields below.
type rowSubPlan struct {
	prog *Program
	// op is the comparison as written: "=" "==" "!=" "<>" "IS" "IS NOT" (the
	// equality-shaped family) or "<" "<=" ">" ">=" (the lexicographic one).
	op string
	// subOnLeft records which operand the subquery was, because the comparison
	// is NOT symmetric: the lexicographic operators invert, and the collating
	// sequence is resolved left-operand-first (resolveCompareCollation).
	subOnLeft bool
	// affs and colls are PER COLUMN, parallel to each other, and their shared
	// length is the row value's ARITY -- the width of OpRowSub's P1 register
	// block. Each is what a SCALAR comparison at that position would use:
	// affs[i] combines the row value's i'th element with the subquery's i'th
	// result column (comparisonAffinity, which is symmetric), and colls[i] is
	// resolveCompareCollation over the same pair IN SOURCE ORDER -- which the
	// compiler only ever fills in when the ROW-VALUE side is what resolves it
	// (see compileRowSubCompare for the case it declines instead).
	affs  []affinity
	colls []string
	// correlated marks a subquery that references an outer-scope column and so
	// must be RE-RUN on every evaluation rather than materialized once and
	// cached -- see the OpInSub doc comment above.
	correlated bool
}

// groupBatchPlan is OpGroupBatchFinal's P4 payload: the static shape of every
// collected batch entry ([minRowid, groupKey[0..nGroup), out[0..nOut),
// orderKeys[0..nOrder) if hasOrder]) plus the final ordering/slicing rule --
// everything groupBatchFinal (vdbe_group_distinct.go) needs for its
// DISTINCT dedup -> sort -> LIMIT/OFFSET pass. limit/offset are read directly from the statement (already
// resolved to literal values by the time this compiles, exactly like
// compileScanGroupBy's own emitLimitOffsetCounters), not from registers --
// this opcode's whole body runs in Go over the materialized batch, so there
// is nothing for a runtime register to add.
type groupBatchPlan struct {
	nGroup     int
	nOut       int
	nOrder     int
	hasOrder   bool
	orderDesc  []bool       // len == nOrder; per-column DESC flag, parallel to the orderKeys span
	orderNulls []NullsOrder // len == nOrder; per-column NULLS placement, parallel to orderDesc
	orderColl  []string     // len == nOrder; per-column collating sequence ("" == BINARY), parallel to orderDesc
	// outColl is each OUTPUT column's collating sequence ("" == BINARY), what
	// a SELECT DISTINCT dedups the finished rows by -- see
	// groupByPlan.outColls for why it is the RESULT column's collation and
	// never the GROUP BY key's. nil for a non-DISTINCT batch.
	outColl []string
	// scanOrder is select.c's groupBySort == 0 for a SELECT DISTINCT batch: the
	// survivors already left step 1 in SCAN order, so groupBatchFinal must NOT
	// re-sort them by their group-key tuple. See scanOrderGroups
	// (vdbe_agg_codegen.go), which is where this is decided.
	scanOrder bool
	limit     *int64
	offset    *int64
}

// subCacheEntry is one run-once cache slot for a subquery opcode: the sub-
// Program's materialized result rows, filled the first time the opcode
// executes (done set) and reused on every later execution within the same
// vdbe run. See the subquery opcodes' doc comment above. rowids is used only
// by a VIRTUAL TABLE derived source (derivedSource.vtab, runVtabOnce,
// vdbe.go) -- nil (and never consulted) for an ordinary compiled sub-Program,
// which exposes no rowid pseudo-column at all.
type subCacheEntry struct {
	done   bool
	rows   [][]Value
	rowids []int64
}

// derivedSource is OpOpenDerived's P4. Exactly one of these is set:
//
//   - prog: the compiled sub-Program producing a derived table's (or view's)
//     rows, run once via runSubOnce and cached in subCache[P2].
//   - vtab: a virtual table source (resolveVtabSource): a table-valued
//     function, a persisted virtual table or an eponymous module, run once
//     via runVtabOnce (materializeVtab), with real rowids.
//   - vtabWrite: the write-side scan of a virtual table
//     (vdbe_vtab_write.go).
//   - cte: a non-recursive CTE (resolveCTESource), run once via runCTEOnce.
//     A recursive CTE's queue program is a prog.
//   - recSelf: a recursive CTE's own name inside a recursive arm, a one-row
//     cursor over the queue row being expanded (vdbe_recursive_cte.go).
//
// tbl describes the output columns (opening the cursor, sizing OpNullRow).
// dbIdx is the pager to run prog / materializeVtab against (dbIndexOf),
// non-zero only for a foreign-qualified view or vtab; a CTE is always 0.
type derivedSource struct {
	prog *Program

	// keepSubtype is joinSource.keepSubtype: the columns whose subtype the
	// cursor leaves in place (openDerivedCursor).
	keepSubtype []bool

	// pager, when non-nil, is the database this source must run against,
	// overriding the enclosing program's own (dbIdx). A WRITE program has no
	// pager of its own -- the write path compiles its source SELECT against a
	// snapshot of the session's current rows (compileInsertSelectWrite) and
	// carries it here.
	pager *ReadOnlyPager

	vtab *FromItem
	// vtabTrig is the trigger context vtab was resolved under (c.trig at the
	// emitting compile), nil outside a trigger body. A table-valued function's
	// arguments are compiled at run time (buildVtabConstraints /
	// foldVtabInputValue, vtab.go), so "json_each(NEW.x)" must compile under the
	// body's trigger context to read the trigger registers
	// (resolveTriggerParam -> OpParam). C turns each argument into a
	// "hidden-column = <arg>" WHERE term (sqlite3WhereTabFuncArgs,
	// whereexpr.c:1902) coded inside the trigger sub-program
	// (resolve.c:525-543). It is carried on the source because it is
	// compile-time schema; the values come from the machine.
	vtabTrig *trigCompileCtx

	// vtabCorr, when non-empty, makes this a CORRELATED virtual-table source:
	// the listed constraints' right-hand sides live in REGISTERS this opcode
	// reads, written by the outer loops it is nested inside, instead of being
	// folded standalone. It is what turns "one materialization per execution"
	// into "one xFilter per outer row", which is what C does unconditionally
	// (wherecode.c:1584 codes the value, wherecode.c:1610 runs OP_VFilter, both
	// inside the loop). Emitted only by emitVtabCorrelatedOpen; nil everywhere
	// else. See vtab_correlated.go.
	vtabCorr []vtabCorrTerm

	cte   *cteRef
	tbl   *resolvedTable
	dbIdx int

	// recSelf, when non-nil, is the recursive CTE whose CURRENT queue row this
	// source opens (vdbe.recCur) -- SQLite's OP_OpenPseudo iCurrent over
	// regCurrent (select.c:2733). No subCache slot: the row changes with every
	// pop, and each recursive step runs on a fresh machine anyway.
	recSelf *recQueueSpec

	// stream says whether prog may run as a CO-ROUTINE feeding this cursor one
	// row at a time instead of being materialized first (streamsDerived,
	// vdbe_stream.go). streamNever, the zero value, is every source's behaviour
	// before this field existed.
	stream streamMode

	// sinkPlan is the INSERT whose rows a streamAlways source feeds, handed to
	// the co-routine chain it starts (streamSink). nil for every other source.
	sinkPlan *insertPlan

	// catalogScope, when not scopeAny, marks a MATERIALIZED SCHEMA CATALOG
	// source: scope's catalog rendered into sqlite_master's five-column shape
	// (schemaCatalogRows, temp_catalog.go).
	// There is no sub-program and no module to drive -- OpOpenDerived reads the
	// pager's already-memoized Schema() directly.
	catalogScope schemaScope

	// upfrom, when non-nil, marks this source as an "UPDATE ... FROM" pass-one
	// destination -- SQLite's SRT_Upfrom ephemeral table (select.c:1355).
	// prog's rows are COLLAPSED onto one entry per target row, keyed by that
	// row's row-store key and ordered by it, before the cursor is opened. See
	// collapseUpfromRows (vdbe_update_from.go).
	upfrom *upfromDest
	// viewUpfrom, when non-nil, marks this source as an "UPDATE <view> ... FROM"
	// pass-one destination. It is a DIFFERENT destination from upfrom above and
	// the C says so: updateFromSelect's "else if( IsView(pTab) )" arm projects
	// the view's OWN columns (not a key) and uses SRT_Table, i.e. an
	// auto-keyed ephemeral with no collapse at all (update.c:243-247). So
	// prog's rows are opened AS THEY ARE; the only run-time work is the
	// multi-match refusal. See checkViewUpfromRows (vdbe_view_update_from.go).
	viewUpfrom *viewUpfromDest
	// vtabWrite, when non-nil, marks the WRITE-side scan of a virtual table:
	// the cursor a compiled vtab UPDATE/DELETE drives its row loop with, which
	// is SQLite's own "sqlite3WhereBegin over the virtual table" (update.c:1273
	// inside updateVirtualTable, delete.c:526). Its rows come from the module's
	// OWN write-side row source (vtabWriteRows, vdbe_vtab_write.go) at RUN
	// time, never from a pager: vtab is the READ path's source and answers from
	// a *ReadOnlyPager image, which the write path has none of. Both fields
	// open the same openMaterializedCursor, so a vtab's real rowid
	// pseudo-column (OpRowid) works on either.
	vtabWrite *vtabMeta

	// schemaWrite marks the write-side scan of the schema catalog, the cursor
	// a compiled "UPDATE/DELETE FROM sqlite_master" under writable_schema
	// loops over. In C that is an ordinary table write once tabIsReadOnly
	// (delete.c:103-107) allows it, with the usual sqlite3WhereBegin
	// (delete.c:526, update.c:742). Its rows come from db.wsCurrentCatalog at
	// run time, the same set the overlay's apply half keys against, not from
	// catalogScope (the read path's source). Rowids are 1..N over that list,
	// matching materialize's numbering for main's part of the catalog.
	schemaWrite bool
}

// matchCompileInfo is OpMatch's P4 payload (see OpMatch's own doc comment
// above): the parsed MatchExpr AST node plus the compile-time table scopes
// and their parallel cursor numbers -- exactly c.scopes at the MATCH
// expression's compile site, flattened -- needed to reassemble the per-row
// evalCtx (tables+vals) evalMatch (fts5_match.go) evaluates against, gathered
// fresh from the currently open cursors on every OpMatch execution
// (gatherScopesRow, vdbe.go).
type matchCompileInfo struct {
	expr    MatchExpr
	scopes  []tableScope
	cursors []int
	// colBases is compileScope.colBase per scopes[i] -- nil unless at least
	// one entry is a materialized parenthesized join GROUP's leaf member,
	// in which case it is set for every entry (0 for an ordinary source).
	// See gatherScopesRow's own doc comment (vdbe.go) for why this is what
	// keeps a group's cursor-sharing members from being copied past their
	// own slice of the group's wider shared row.
	colBases []int
	// langid is which LANGUAGE an fts4 "languageid=" MATCH searches, resolved
	// at COMPILE time from the statement's own WHERE clause (fts3_langid.go) --
	// the per-row re-resolution below sees only the scopes, not the WHERE, and
	// language 0 is not a safe default for a table that has other languages.
	langid int64

	// contentUnneeded is compiler.fts3ContentUnneeded for this MATCH's own
	// resolved scope, threaded down to evalFts3Match through
	// evalMatchExprLangid -- see fts3StmtContentUnneeded's doc comment
	// (fts3_search.go) for the rule. false (the strict/safe default) for
	// every fts5 MATCH, which never reaches evalFts3Match at all.
	contentUnneeded bool

	// patReg holds this MATCH's query value, coded by compileMatchExpr just
	// before the OpMatch reading it, so it is recomputed per execution, as C
	// codes a vtab constraint value in the loop body (wherecode.c:1584) and the
	// module reads it as a value (fts3.c:3365).
	//
	// patCompiled distinguishes unset from register 0, which is allocatable;
	// fts5's matchCompileInfo (compileFts5Match) leaves both zero and evaluates
	// its own pattern.
	patReg      int
	patCompiled bool
}

// P5 flag/affinity layout for the comparison opcodes. The low nibble holds the
// comparison affinity (one of the affinity enum values, affNone..affReal); the
// flag bits sit above it.
const (
	p5AffMask    uint16 = 0x0f
	p5StoreP2    uint16 = 0x10 // store boolean/NULL result in r[P2] instead of jumping.
	p5JumpIfNull uint16 = 0x20 // jump (or, with StoreP2, that path is unused) if either operand is NULL.
	p5NullEq     uint16 = 0x40 // NULLs compare equal; result is never NULL (IS / IS NOT).
	p5LikeNot    uint16 = 0x01 // OpLike: negate the match (NOT LIKE).
	p5GlobNot    uint16 = 0x01 // OpGlob: negate the match (NOT GLOB). Same bit as p5LikeNot -- disjoint opcodes, so no collision.
	p5Correlated uint16 = 0x80 // OpSubquery/OpExists: the sub-Program is correlated -- re-run per evaluation against the enclosing frame instead of run-once + cache.
)

// Instruction is one VDBE instruction: an opcode plus SQLite's five operand
// slots. P1/P2/P3 are integers (register numbers, jump targets, small
// immediates, counts); P4 is a typed payload (a literal value, a function
// name, an affinity, a collation); P5 is a bit-packed flag/affinity word.
type Instruction struct {
	Op OpCode
	P1 int
	P2 int
	P3 int
	P4 any
	P5 uint16
}

// Program is a compiled VDBE program: a flat instruction stream plus the
// register-file size it needs and the number of result columns each
// OpResultRow emits.
type Program struct {
	Insns      []Instruction
	vmJIT      unsafe.Pointer // *vmJIT: the native code jitCode compiled for Insns, or noVMJIT
	// outerCtx is *outerCtxVerdict: outerCtxUnread's answer for Insns, checked
	// against them as vmJIT is, since a Program copied by value can be handed
	// different instructions.
	outerCtx unsafe.Pointer
	NReg       int      // registers to allocate (r[0..NReg-1]).
	NCursors   int      // table cursors to allocate (cursors[0..NCursors-1]); 0 for a cursor-free (FROM-less) program.
	NRecRegs   int      // record registers to allocate (rr[0..NRecRegs-1]); 0 for a program with no ORDER BY (see the sorter opcodes above).
	NSorters   int      // sorter cursors to allocate (sorters[0..NSorters-1]); 0 for a program with no ORDER BY.
	NSubCache  int      // run-once subquery cache slots to allocate (subCache[0..NSubCache-1]); 0 for a program with no compiled subquery. See the subquery opcodes (vdbe_op.go).
	NDistinct  int      // distinct-set trackers to allocate (distinctSets[0..NDistinct-1]); 0 for a program with no SELECT DISTINCT. See the DISTINCT opcodes (vdbe_op.go).
	NResultCol int      // columns per emitted result row.
	// OrderUnproven marks a SUBQUERY body whose row order the ported planner
	// could not prove is C SQLite's (anchorPlanOrderProvable): harmless to an
	// IN or EXISTS, but a scalar subquery answers with its FIRST row, so
	// OpSubquery declines when the rows it got would give a different first
	// row in another order.
	OrderUnproven bool
	ColNames   []string // result column names, in order.

	// WritePager, when non-nil, is the snapshot a WRITE program's subqueries
	// read against (a plain read program has none -- it runs against the VM's
	// own m.pager). The write path has no pager of its own, so a compiled write
	// program that contains a subquery carries the snapshot it was compiled
	// against here, and runWrite installs it as the VM's pager. See
	// compileUpdateWrite/compileDeleteWrite.
	WritePager *ReadOnlyPager

	// Ainc is the statement's pAinc list (insert.c:410-455): every
	// AUTOINCREMENT table it may insert into, directly or through a trigger,
	// whose sqlite_sequence row it reads when it starts (writeCtx.aincBegin).
	Ainc []*tableMeta

	// CountsChanges marks a program whose statement PUBLISHES changes() --
	// SQLite's Vdbe.changeCntOn, set for INSERT/UPDATE/DELETE (REPLACE
	// included) and for nothing else. It is what keeps a CREATE/DROP/ALTER,
	// VACUUM, ANALYZE, PRAGMA or transaction-control statement from resetting
	// the counter to its own zero row count: C SQLite leaves changes()
	// exactly as the last DML left it across all of those (verified -- after
	// an INSERT of one row, "CREATE TABLE u(x); CREATE INDEX i1 ON t(b);
	// DROP TABLE u; ALTER TABLE t ADD COLUMN c" all still report 1). See
	// runWrite and conn_state.go.
	CountsChanges bool

	// FKTargets is the table(s) a WRITE program mutates -- the DML target,
	// plus the target of any trigger body the compiler inlined. It exists for
	// one rule: a foreign key that cannot be RESOLVED is an error for ANY DML
	// on its child table, including a DELETE, including one that matches no
	// rows at all (C SQLite raises it while GENERATING code, so
	// "no such table: main.p" / "foreign key mismatch" fire even for an empty
	// table -- verified). Row-level accounting cannot see such a statement,
	// so runWrite checks these before running. nil for a read program and for
	// a DDL/utility one.
	FKTargets []*tableMeta

	// FKReach is how far the foreign key resolution walk may follow ACTIONS
	// from FKTargets -- see fkStatementReach (fk.go), which derives it from the
	// statement's verb.
	FKReach fkReach

	// Correlated marks a sub-Program that references at least one outer-scope
	// column (it emitted an OpOuterColumn/OpOuterRowid, or transitively
	// contains a subquery that references a scope outer to THIS one). The
	// enclosing compiler reads it to decide whether the OpSubquery/OpExists/
	// OpInSub referencing this sub-Program must re-run per evaluation
	// (correlated) or run once and cache (uncorrelated). Always false for a
	// top-level program (nothing outer to reference). See compileColumn and
	// compileScalarSubquery (vdbe_codegen.go).
	Correlated bool

	// Compound is non-nil only for a Program standing in for a compound SELECT
	// used as a subquery body (compileSubProgramCompound): only this field and
	// NResultCol/ColNames are set, with no Insns. exec/execOuter check it first
	// and run Compound.exec (vdbe_compound_codegen.go). Correlated may be set
	// (an arm may bind an outer column), so execWithParent hands each arm the
	// parent frame.
	Compound *compoundProgram

	// OuterFrames describes, per ENCLOSING compile level (index 0 = the
	// immediate parent), that level's table scopes and the cursor numbers
	// backing them. It exists for the opcode bodies that consume a whole ROW
	// through an evalCtx instead of pre-compiled operand registers -- the
	// aggregate opcodes (aggRowCtx, vdbe_agg.go) and OpMatch/OpFts3Aux/
	// OpFts5Aux (gatherScopesRow, vdbe.go): a CORRELATED column reference
	// reached that way resolves through evalCtx.outer rather than by an
	// OpOuterColumn, and execWithParent has to reconstruct that evalCtx chain
	// from the live parent frames. nil for every program compiled with no
	// enclosing compile in hand (outerFramesOf, vdbe_codegen.go). See
	// buildOuterEvalCtx (vdbe.go).
	OuterFrames []outerFrameInfo

	// CTEScopeSnapshot is the WITH scope stack (ReadOnlyPager.cteScopes) in
	// force when this Program was compiled, nil when none. Set by
	// compileSubProgram with OuterFrames. Some opcodes resolve names when they
	// run (the aggregate opcodes, OpMatch/OpFts3Aux/OpFts5Aux), after
	// compileSubProgram's pushCTEScope has popped, so a CTE visible only
	// lexically (with2.test 10.1) would be "no such table". Bindings are
	// immutable (newCTEBinding), so the compile-time map is still correct;
	// execOuter/execWithParent install it for the whole run.
	CTEScopeSnapshot []map[string]*cteBinding

	// LiveSource, when non-nil, is the SELECT this sub-Program was lowered
	// from, kept so the VM can lower it AGAIN -- against the database as it
	// stands at the instant the program actually runs, rather than against the
	// image the enclosing WRITE statement was compiled over. See
	// vdbe_live_read.go for the mechanism, the C that owns the behaviour
	// (OP_Program zeroes the frame's OP_Once bits on every firing, vdbe.c:7582)
	// and the self-contained restriction that makes the second lowering
	// provably identical to the first.
	//
	// nil for every read program and for every FROZEN write sub-program, which
	// is all of them but a trigger's WHEN guard and a trigger body statement.
	LiveSource *SelectStmt

	// LiveTrig is the trigger context LiveSource was lowered under, kept for
	// exactly one reason: the SECOND lowering must resolve NEW./OLD. the same
	// way the first did, or the two would disagree about what the program even
	// means. It is the whole of what a live sub-program may name outside
	// itself -- see compileLiveSubProgram's trig-only outer -- and it becomes
	// OpParam reads of the running machine's trigger row, never folded values.
	//
	// nil whenever LiveSource is, and also for a live sub-program compiled
	// outside a trigger body.
	LiveTrig *trigCompileCtx

	// LiveRet is the same thing for the OTHER context a live sub-program may
	// name outside itself: a RETURNING clause's AFFECTED ROW (resolve.c's
	// NC_UBaseReg arm, :528-536). Every word of LiveTrig's reason applies --
	// the second lowering must resolve the name the same way the first did, and
	// it resolves to an OpParam read of the row the machine carries
	// (OpPseudoRow), never to a folded value.
	//
	// nil whenever LiveSource is, and for every live sub-program outside a
	// RETURNING list.
	LiveRet *retRowCompileCtx
	// LiveNQueryLoop and LiveNQLKnown are the enclosing nQueryLoop LiveSource
	// was first lowered under (compileLiveSubProgram), so the run-time lowering
	// plans it the same way.
	LiveNQueryLoop logEst
	LiveNQLKnown   bool

	// LiveRow is the UPDATE row a live SET subquery is correlated to (see
	// liveRowCtx, vdbe_live_read.go). nil for every other program.
	LiveRow *liveRowCtx

	// LiveStmtImage marks a live sub-program of a TRIGGER BODY's UPDATE or
	// DELETE WHERE: it is lowered against the image taken when that body
	// statement began (vdbe.stmtImage), not the database as it stands -- C
	// evaluates such a WHERE in pass one, before the statement writes a row.
	// NeedsStmtImage marks the body program that holds one, so the executor
	// takes the image (runTriggerSub). See liveWhereSubSelect.
	LiveStmtImage  bool
	NeedsStmtImage bool
	// LiveFirstImage marks a live sub-program C plans with an AUTOMATIC INDEX:
	// it is lowered against the image taken the first time it runs in this
	// frame, which is when OP_Once builds C's index, and every later run reads
	// that image (vdbe.firstImages). See subqueryPlansAutoIndex.
	LiveFirstImage bool
}

// outerFrameInfo is one enclosing execution frame's shape: the table scopes
// that compile had in force and, parallel to them, the cursor numbers holding
// their rows. Together with the live *vdbe of that frame it reconstitutes the
// enclosing query's evalCtx -- see Program.OuterFrames.
type outerFrameInfo struct {
	scopes  []tableScope
	cursors []int
}
