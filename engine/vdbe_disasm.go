// This file renders a compiled Program as an EXPLAIN-style listing -- the same
// addr / opcode / p1 / p2 / p3 / p4 / p5 / comment columns SQLite's "EXPLAIN
// <sql>" prints -- for debugging the codegen and for eyeballing structural
// correspondence against C SQLite's bytecode (see the bytecode oracle in
// the compat-harness).
package engine

import (
	"fmt"
	"strings"
)

// opNames maps each opcode to its mnemonic (matching SQLite's OP_* names, sans
// the "OP_" prefix).
var opNames = [numOpCodes]string{
	OpGosub:                "Gosub",
	OpReturn:               "Return",
	OpJIT:                  "JIT",
	OpInit:                 "Init",
	OpSegOrderLimit:        "SegOrderLimit",
	OpSegHashAgg:           "SegHashAgg",
	OpSegProgram:           "SegProgram",
	OpSegEmitRow:           "SegEmitRow",
	OpSegDistinct:          "SegDistinct",
	OpGoto:                 "Goto",
	OpHalt:                 "Halt",
	OpResultRow:            "ResultRow",
	OpIf:                   "If",
	OpIfNot:                "IfNot",
	OpIsNull:               "IsNull",
	OpNotNull:              "NotNull",
	OpInteger:              "Integer",
	OpInt64:                "Int64",
	OpReal:                 "Real",
	OpString8:              "String8",
	OpNull:                 "Null",
	OpBlob:                 "Blob",
	OpVariable:             "Variable",
	OpCopy:                 "Copy",
	OpSCopy:                "SCopy",
	OpAdd:                  "Add",
	OpSubtract:             "Subtract",
	OpMultiply:             "Multiply",
	OpDivide:               "Divide",
	OpRemainder:            "Remainder",
	OpConcat:               "Concat",
	OpWindowAppend:         "WindowAppend",
	OpWindowFinal:          "WindowFinal",
	OpComputeGenerated:     "ComputeGenerated",
	OpTypeCheck:            "TypeCheck",
	OpBitAnd:               "BitAnd",
	OpBitOr:                "BitOr",
	OpShiftLeft:            "ShiftLeft",
	OpShiftRight:           "ShiftRight",
	OpBitNot:               "BitNot",
	OpEq:                   "Eq",
	OpNe:                   "Ne",
	OpLt:                   "Lt",
	OpLe:                   "Le",
	OpGt:                   "Gt",
	OpGe:                   "Ge",
	OpNot:                  "Not",
	OpNegative:             "Negative",
	OpAffinity:             "Affinity",
	OpRealAffinity:         "RealAffinity",
	OpCast:                 "Cast",
	OpFunction:             "Function",
	OpConnState:            "ConnState",
	OpLike:                 "Like",
	OpGlob:                 "Glob",
	OpMatch:                "Match",
	OpOpenRead:             "OpenRead",
	OpOpenDerived:          "OpenDerived",
	OpSeekRowidHint:        "SeekRowidHint",
	OpSeekIndexHint:        "SeekIndexHint",
	OpAutoIndexOrder:       "AutoIndexOrder",
	OpNotExists:            "NotExists",
	OpRewind:               "Rewind",
	OpNext:                 "Next",
	OpColumn:               "Column",
	OpRowid:                "Rowid",
	OpClose:                "Close",
	OpClearSubtype:         "ClearSubtype",
	OpNullRow:              "NullRow",
	OpRightJoinMark:        "RightJoinMark",
	OpRightJoinSweepRewind: "RJSweepRewind",
	OpRightJoinSweepNext:   "RJSweepNext",
	OpOuterColumn:          "OuterColumn",
	OpOuterRowid:           "OuterRowid",
	OpSorterOpen:           "SorterOpen",
	OpMakeRecord:           "MakeRecord",
	OpSorterCheck:          "SorterCheck",
	OpSorterInsert:         "SorterInsert",
	OpSorterSort:           "SorterSort",
	OpSorterData:           "SorterData",
	OpRecordColumn:         "RecordColumn",
	OpSorterNext:           "SorterNext",
	OpDistinctOpen:         "DistinctOpen",
	OpDistinct:             "Distinct",
	OpAggReset:             "AggReset",
	OpAggStep:              "AggStep",
	OpAggResult:            "AggResult",
	OpGroupSame:            "GroupSame",
	OpRecCopy:              "RecCopy",
	OpGroupBatchAppend:     "GroupBatchAppend",
	OpGroupBatchFinal:      "GroupBatchFinal",
	OpHashAggStep:          "HashAggStep",
	OpHashAggSort:          "HashAggSort",
	OpHashAggData:          "HashAggData",
	OpHashAggNext:          "HashAggNext",
	OpOuterAggReg:          "OuterAggReg",
	OpSubquery:             "Subquery",
	OpExists:               "Exists",
	OpInSub:                "InSub",
	OpRowSub:               "RowSub",
	OpSubCacheReset:        "SubCacheReset",
	OpOpenWrite:            "OpenWrite",
	OpHaltError:            "HaltError",
	OpHaltIfNull:           "HaltIfNull",
	OpMustBeInt:            "MustBeInt",
	OpNewRowid:             "NewRowid",
	OpMemMax:               "MemMax",
	OpRaise:                "Raise",
	OpParam:                "Param",
	OpPseudoRow:            "PseudoRow",
	OpLimitCounter:         "LimitCounter",
	OpFireTriggers:         "FireTriggers",
	OpTriggerBodyRouted:    "TriggerBodyRouted",
	OpUpsertFind:           "UpsertFind",
	OpUpsertStore:          "UpsertStore",
	OpInsert:               "Insert",
	OpDelete:               "Delete",
	OpClearTable:           "Clear",
	OpUpdateRow:            "UpdateRow",
	OpVInsert:              "VInsert",
	OpVInsertRow:           "VInsertRow",
	OpVWriteRow:            "VWriteRow",
	OpVWrite:               "VWrite",
	OpSchemaWritePre:       "SchemaWritePre",
	OpSchemaWriteRow:       "SchemaWriteRow",
	OpSchemaWrite:          "SchemaWrite",
	OpDdl:                  "Ddl",
	OpTxn:                  "Txn",
	OpRecQueueOpen:         "RecQueueOpen",
	OpRecQueueFill:         "RecQueueFill",
	OpRecQueuePush:         "RecQueuePush",
	OpRecQueueCheck:        "RecQueueCheck",
	OpRecQueuePop:          "RecQueuePop",
	OpRecQueueOffset:       "RecQueueOffset",
	OpRecQueueLimit:        "RecQueueLimit",
	OpUpsertReload:         "UpsertReload",
	OpMultiOrTag:           "MultiOrTag",
	OpMultiOrSort:          "MultiOrSort",
}

// String gives an opcode its mnemonic (used in error messages and disassembly).
func (op OpCode) String() string {
	if int(op) < len(opNames) && opNames[op] != "" {
		return opNames[op]
	}
	return fmt.Sprintf("Op(%d)", int(op))
}

// affinityName renders an affinity as SQLite's single-letter code, for the P4
// column of a comparison/affinity instruction.
func affinityName(a affinity) string {
	switch a {
	case affText:
		return "TEXT"
	case affNumeric:
		return "NUMERIC"
	case affInteger:
		return "INTEGER"
	case affReal:
		return "REAL"
	default:
		return "BLOB"
	}
}

// Disassemble renders prog as a multi-line EXPLAIN-style table. It is stable
// and side-effect free, suitable for test golden output and logs.
func (prog *Program) Disassemble() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-4s %-12s %-5s %-5s %-5s %-18s %-3s %s\n",
		"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment")
	for addr, in := range prog.Insns {
		p4 := formatP4(in)
		fmt.Fprintf(&b, "%-4d %-12s %-5d %-5d %-5d %-18s %-3d %s\n",
			addr, in.Op.String(), in.P1, in.P2, in.P3, p4, in.P5, disasmComment(prog, addr, in))
	}
	return b.String()
}

// formatP4 renders the P4 operand for the disassembly's p4 column.
func formatP4(in Instruction) string {
	switch in.Op {
	case OpInt64:
		if v, ok := in.P4.(int64); ok {
			return fmt.Sprintf("%d", v)
		}
	case OpReal:
		if v, ok := in.P4.(float64); ok {
			return fmt.Sprintf("%g", v)
		}
	case OpString8:
		if v, ok := in.P4.(string); ok {
			return fmt.Sprintf("%q", v)
		}
	case OpBlob:
		if v, ok := in.P4.([]byte); ok {
			return fmt.Sprintf("x'%x'", v)
		}
	case OpFunction, OpCast, OpConnState:
		if v, ok := in.P4.(string); ok {
			return v
		}
	case OpAffinity:
		if v, ok := in.P4.(affinity); ok {
			return affinityName(v)
		}
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		if v, ok := in.P4.(string); ok {
			return "(" + v + ")"
		}
	case OpOpenRead:
		return "" // P4 is a *resolvedTable, not meaningfully renderable here.
	case OpSorterOpen:
		if ki, ok := in.P4.(*sorterKeyInfo); ok {
			return formatKeyInfo(ki)
		}
	case OpAggReset, OpAggStep, OpAggResult, OpHashAggStep:
		return "" // P4 is an *aggPlan / *aggResultInfo, described in the comment column.
	case OpGroupBatchFinal:
		return "" // P4 is a *groupBatchPlan, described in the comment column.
	case OpSubquery, OpExists, OpInSub, OpRowSub:
		return "" // P4 is a compiled sub-*Program / *inSubPlan / *rowSubPlan, described in the comment column.
	case OpRecQueueOpen, OpRecQueueFill:
		return "" // P4 is a *recQueueSpec / the setup or recursive-step *Program.
	}
	if in.P4 == nil {
		return ""
	}
	// Everything else is rendered only when it has a SHORT, STABLE rendering.
	// A compiler-internal struct does not: "%v" on one prints its whole
	// contents including heap ADDRESSES, which is both unreadable and
	// different on every run -- and this column is EXPLAIN's p4 (explain.go),
	// which a caller may diff. C SQLite prints a name there (a table, an
	// index, a collation) or nothing at all, never a structure dump.
	switch v := in.P4.(type) {
	case string:
		return v
	case *tableMeta:
		return v.name
	case *indexMeta:
		return v.name
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	}
	return ""
}

// disasmComment produces the human-readable synopsis column, echoing the
// operand-direction and affinity/flag conventions SQLite's own EXPLAIN
// annotates.
func disasmComment(prog *Program, addr int, in Instruction) string {
	switch in.Op {
	case OpInit:
		return fmt.Sprintf("start at %d", in.P2)
	case OpGoto:
		return fmt.Sprintf("goto %d", in.P2)
	case OpResultRow:
		return fmt.Sprintf("output=r[%d..%d]", in.P1, in.P1+in.P2-1)
	case OpInteger, OpInt64:
		return fmt.Sprintf("r[%d]=%s", in.P2, p4OrP1Int(in))
	case OpReal, OpString8, OpBlob:
		return fmt.Sprintf("r[%d]=%s", in.P2, formatP4(in))
	case OpNull:
		return fmt.Sprintf("r[%d]=NULL", in.P2)
	case OpVariable:
		return fmt.Sprintf("r[%d]=parameter(%d)", in.P2, in.P1)
	case OpCopy, OpSCopy:
		return fmt.Sprintf("r[%d]=r[%d]", in.P2, in.P1)
	case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat:
		return fmt.Sprintf("r[%d]=r[%d]%sr[%d]", in.P3, in.P2, synopsisOp(in.Op), in.P1)
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		return compareComment(in)
	case OpNot:
		return fmt.Sprintf("r[%d]=NOT r[%d]", in.P2, in.P1)
	case OpNegative:
		return fmt.Sprintf("r[%d]=-r[%d]", in.P2, in.P1)
	case OpIf:
		return fmt.Sprintf("if r[%d] goto %d", in.P1, in.P2)
	case OpIfNot:
		return fmt.Sprintf("if !r[%d] goto %d", in.P1, in.P2)
	case OpIsNull:
		return fmt.Sprintf("if r[%d]==NULL goto %d", in.P1, in.P2)
	case OpNotNull:
		return fmt.Sprintf("if r[%d]!=NULL goto %d", in.P1, in.P2)
	case OpCast:
		return fmt.Sprintf("r[%d]=CAST(r[%d] AS %v)", in.P1, in.P1, in.P4)
	case OpAffinity:
		return fmt.Sprintf("affinity(r[%d])", in.P1)
	case OpRealAffinity:
		return fmt.Sprintf("realaffinity(r[%d])", in.P1)
	case OpFunction:
		return fmt.Sprintf("r[%d]=%v(r[%d..%d])", in.P3, in.P4, in.P1, in.P1+in.P2-1)
	case OpConnState:
		return fmt.Sprintf("r[%d]=%v()", in.P3, in.P4)
	case OpLike:
		neg := ""
		if in.P5&p5LikeNot != 0 {
			neg = "NOT "
		}
		if escReg, _ := in.P4.(int); escReg >= 0 {
			return fmt.Sprintf("r[%d]=r[%d] %sLIKE r[%d] ESCAPE r[%d]", in.P2, in.P1, neg, in.P3, escReg)
		}
		return fmt.Sprintf("r[%d]=r[%d] %sLIKE r[%d]", in.P2, in.P1, neg, in.P3)
	case OpGlob:
		neg := ""
		if in.P5&p5GlobNot != 0 {
			neg = "NOT "
		}
		return fmt.Sprintf("r[%d]=r[%d] %sGLOB r[%d]", in.P2, in.P1, neg, in.P3)
	case OpMatch:
		if info, ok := in.P4.(*matchCompileInfo); ok {
			return fmt.Sprintf("r[%d]=MATCH(%v)", in.P2, info.expr)
		}
		return fmt.Sprintf("r[%d]=MATCH", in.P2)
	case OpHalt:
		return "end"
	case OpOpenRead:
		if in.P3 != 0 {
			return fmt.Sprintf("cursor(%d)=table root=%d db=%d", in.P1, in.P2, in.P3)
		}
		return fmt.Sprintf("cursor(%d)=table root=%d", in.P1, in.P2)
	case OpNotExists:
		return fmt.Sprintf("cursor(%d): if the row is gone goto %d", in.P1, in.P2)
	case OpRewind:
		return fmt.Sprintf("cursor(%d): rewind; if empty goto %d", in.P1, in.P2)
	case OpNext:
		return fmt.Sprintf("cursor(%d): next; if row goto %d", in.P1, in.P2)
	case OpColumn:
		return fmt.Sprintf("r[%d]=cursor(%d).column(%d)", in.P3, in.P1, in.P2)
	case OpRowid:
		return fmt.Sprintf("r[%d]=cursor(%d).rowid", in.P2, in.P1)
	case OpOuterColumn:
		return fmt.Sprintf("r[%d]=outer[%d].cursor(%d).column(%d)", in.P3, in.P5, in.P1, in.P2)
	case OpOuterRowid:
		return fmt.Sprintf("r[%d]=outer[%d].cursor(%d).rowid", in.P2, in.P5, in.P1)
	case OpClose:
		return fmt.Sprintf("cursor(%d): close", in.P1)
	case OpClearSubtype:
		return fmt.Sprintf("r[%d]: drop the JSON subtype", in.P1)
	case OpNullRow:
		return fmt.Sprintf("cursor(%d): null row", in.P1)
	case OpRightJoinMark:
		return fmt.Sprintf("cursor(%d): mark current row matched (RIGHT/FULL JOIN)", in.P1)
	case OpRightJoinSweepRewind:
		return fmt.Sprintf("cursor(%d): sweep first unmatched row; if none goto %d", in.P1, in.P2)
	case OpRightJoinSweepNext:
		return fmt.Sprintf("cursor(%d): sweep next unmatched row; if found goto %d", in.P1, in.P2)
	case OpSorterOpen:
		if ki, ok := in.P4.(*sorterKeyInfo); ok {
			return fmt.Sprintf("sorter(%d)=open %s", in.P1, formatKeyInfo(ki))
		}
		return fmt.Sprintf("sorter(%d)=open", in.P1)
	case OpMakeRecord:
		return fmt.Sprintf("rr[%d]=record(r[%d..%d])", in.P3, in.P1, in.P1+in.P2-1)
	case OpSorterCheck:
		return fmt.Sprintf("sorter(%d): if bounded and r[%d..] loses goto %d", in.P1, in.P3, in.P2)
	case OpSorterInsert:
		return fmt.Sprintf("sorter(%d): insert rr[%d]", in.P1, in.P2)
	case OpSorterSort:
		return fmt.Sprintf("sorter(%d): sort; if empty goto %d", in.P1, in.P2)
	case OpSorterData:
		if in.P3 > 0 {
			return fmt.Sprintf("rr[%d]=sorter(%d).data (first %d lose the JSON subtype)", in.P2, in.P1, in.P3)
		}
		return fmt.Sprintf("rr[%d]=sorter(%d).data", in.P2, in.P1)
	case OpRecordColumn:
		return fmt.Sprintf("r[%d]=rr[%d][%d]", in.P3, in.P1, in.P2)
	case OpSorterNext:
		return fmt.Sprintf("sorter(%d): next; if row goto %d", in.P1, in.P2)
	case OpDistinctOpen:
		return fmt.Sprintf("distinct(%d)=open", in.P1)
	case OpDistinct:
		return fmt.Sprintf("if distinct(%d).seen(rr[%d]) goto %d else remember", in.P1, in.P3, in.P2)
	case OpAggReset:
		return "agg: reset accumulators"
	case OpAggStep:
		if in.P3 == 0 {
			return fmt.Sprintf("agg: step segment %d over joined cursor(s) row", in.P2)
		}
		return fmt.Sprintf("agg: step segment %d over rr[%d] row", in.P2, in.P1)
	case OpAggResult:
		if info, ok := in.P4.(*aggResultInfo); ok {
			return fmt.Sprintf("r[%d]=agg %s (groupkey=rr[%d])", in.P1, aggSetName(info.set, info.idx), in.P3)
		}
		return fmt.Sprintf("r[%d]=agg result", in.P1)
	case OpGroupSame:
		return fmt.Sprintf("if rr[%d]==rr[%d] goto %d", in.P1, in.P3, in.P2)
	case OpRecCopy:
		return fmt.Sprintf("rr[%d]=rr[%d]", in.P2, in.P1)
	case OpGroupBatchAppend:
		return fmt.Sprintf("group batch: append rr[%d]", in.P1)
	case OpGroupBatchFinal:
		return "group batch: dedup+sort+limit -> result rows"
	case OpHashAggStep:
		return fmt.Sprintf("hashagg: step segment %d over joined cursor(s) row (key=rr[%d])", in.P2, in.P1)
	case OpHashAggSort:
		return fmt.Sprintf("hashagg: sort buckets; if empty goto %d", in.P2)
	case OpHashAggData:
		return fmt.Sprintf("rr[%d]=hashagg bucket key (accs live)", in.P2)
	case OpHashAggNext:
		return fmt.Sprintf("hashagg: next bucket; if row goto %d", in.P2)
	case OpSubquery:
		if in.P5&p5Correlated != 0 {
			return fmt.Sprintf("r[%d]=scalar subquery (correlated, per-row)", in.P1)
		}
		return fmt.Sprintf("r[%d]=scalar subquery (once, cache %d)", in.P1, in.P2)
	case OpExists:
		neg := ""
		if in.P3 != 0 {
			neg = "NOT "
		}
		if in.P5&p5Correlated != 0 {
			return fmt.Sprintf("r[%d]=%sEXISTS subquery (correlated, per-row)", in.P1, neg)
		}
		return fmt.Sprintf("r[%d]=%sEXISTS subquery (once, cache %d)", in.P1, neg, in.P2)
	case OpInSub:
		neg := ""
		correlated := false
		if plan, ok := in.P4.(*inSubPlan); ok {
			neg = ""
			if plan.not {
				neg = "NOT "
			}
			correlated = plan.correlated
		}
		if correlated {
			return fmt.Sprintf("r[%d]=r[%d] %sIN subquery (correlated, per-row)", in.P2, in.P1, neg)
		}
		return fmt.Sprintf("r[%d]=r[%d] %sIN subquery (once, cache %d)", in.P2, in.P1, neg, in.P3)
	case OpRowSub:
		op, correlated := "?", false
		if plan, ok := in.P4.(*rowSubPlan); ok {
			op, correlated = plan.op, plan.correlated
			if plan.subOnLeft {
				if correlated {
					return fmt.Sprintf("r[%d]=subquery row %s r[%d] (correlated, per-row)", in.P2, op, in.P1)
				}
				return fmt.Sprintf("r[%d]=subquery row %s r[%d] (once, cache %d)", in.P2, op, in.P1, in.P3)
			}
		}
		if correlated {
			return fmt.Sprintf("r[%d]=r[%d] %s subquery row (correlated, per-row)", in.P2, in.P1, op)
		}
		return fmt.Sprintf("r[%d]=r[%d] %s subquery row (once, cache %d)", in.P2, in.P1, op, in.P3)
	case OpSubCacheReset:
		return fmt.Sprintf("clear sub-cache [%d,%d) -- re-run those subqueries this pass", in.P1, in.P1+in.P2)
	}
	return ""
}

// aggSetName names an OpAggResult's target item set for the disassembly.
func aggSetName(set, idx int) string {
	switch set {
	case aggSetHaving:
		return "having"
	case aggSetOrder:
		return fmt.Sprintf("order[%d]", idx)
	default:
		return fmt.Sprintf("out[%d]", idx)
	}
}

// formatKeyInfo renders a sorter's key shape for the disassembly's p4/comment
// columns: how many leading record columns are key columns and each one's
// ASC/DESC direction, e.g. "keyinfo(nKey=2 ASC,DESC)".
func formatKeyInfo(ki *sorterKeyInfo) string {
	dirs := make([]string, ki.nKey)
	for i := 0; i < ki.nKey; i++ {
		if i < len(ki.desc) && ki.desc[i] {
			dirs[i] = "DESC"
		} else {
			dirs[i] = "ASC"
		}
	}
	return fmt.Sprintf("keyinfo(nKey=%d %s)", ki.nKey, strings.Join(dirs, ","))
}

func p4OrP1Int(in Instruction) string {
	if in.Op == OpInt64 {
		return formatP4(in)
	}
	return fmt.Sprintf("%d", in.P1)
}

func synopsisOp(op OpCode) string {
	switch op {
	case OpAdd:
		return "+"
	case OpSubtract:
		return "-"
	case OpMultiply:
		return "*"
	case OpDivide:
		return "/"
	case OpRemainder:
		return "%"
	case OpConcat:
		return "||"
	}
	return "?"
}

// compareComment describes a comparison instruction, noting its store-vs-jump
// mode, its comparison affinity, and any NULLEQ flag -- the same facts SQLite's
// EXPLAIN synopsis carries in its P5/comment for OP_Eq..OP_Ge.
func compareComment(in Instruction) string {
	sym := map[OpCode]string{OpEq: "==", OpNe: "!=", OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">="}[in.Op]
	var flags []string
	if aff := affinity(in.P5 & p5AffMask); aff != affNone {
		flags = append(flags, affinityName(aff))
	}
	if in.P5&p5NullEq != 0 {
		flags = append(flags, "NULLEQ")
	}
	if in.P5&p5JumpIfNull != 0 {
		flags = append(flags, "JUMPIFNULL")
	}
	suffix := ""
	if len(flags) > 0 {
		suffix = " (" + strings.Join(flags, ",") + ")"
	}
	if in.P5&p5StoreP2 != 0 {
		return fmt.Sprintf("r[%d]=(r[%d]%sr[%d])%s", in.P2, in.P3, sym, in.P1, suffix)
	}
	return fmt.Sprintf("if r[%d]%sr[%d] goto %d%s", in.P3, sym, in.P1, in.P2, suffix)
}
