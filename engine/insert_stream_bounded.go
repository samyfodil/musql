package engine

// INSERT ... SELECT with a recursive source may exceed in-memory caps. The rows
// spill as they arrive: store past rowSpillThreshold, commit streams into delta,
// compaction streams into segments. Bounds time and disk, not heap.

// insertProgramArmable reports whether an INSERT ... SELECT touches only its
// source and builds the row: value opcodes, target cursor 0 (unread), source on
// srcCursor. Sub-programs, triggers, or target reads leave it unarmed.
func insertProgramArmable(insns []Instruction, tbl *tableMeta, plan *insertPlan, src *derivedSource, srcCursor int) bool {
	for i := range insns {
		in := &insns[i]
		switch in.Op {
		case OpInit, OpGoto, OpHalt, OpIf, OpIfNot, OpIsNull, OpNotNull, OpInteger, OpInt64, OpReal,
			OpString8, OpNull, OpBlob, OpVariable, OpCopy, OpSCopy, OpAdd, OpSubtract, OpMultiply,
			OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight, OpBitNot,
			OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpNot, OpNegative, OpAffinity, OpRealAffinity, OpCast,
			OpFunction, OpLike, OpGlob, OpClearSubtype, OpMakeRecord, OpHaltError, OpHaltIfNull,
			OpMustBeInt, OpTypeCheck:
			switch in.P4.(type) {
			case nil, string, int64, float64, int, []byte, affinity, *typeCheckPlan:
			default:
				return false
			}
		case OpComputeGenerated, OpNewRowid, OpMemMax:
			if t, ok := in.P4.(*tableMeta); !ok || t != tbl {
				return false
			}
		case OpInsert:
			if p, ok := in.P4.(*insertPlan); !ok || p != plan || in.P1 != 0 {
				return false
			}
		case OpOpenWrite:
			if t, ok := in.P4.(*tableMeta); !ok || t != tbl || in.P1 != 0 {
				return false
			}
		case OpOpenDerived:
			if d, ok := in.P4.(*derivedSource); !ok || d != src || in.P1 != srcCursor {
				return false
			}
		case OpRewind, OpNext, OpColumn:
			if in.P1 != srcCursor {
				return false
			}
		default:
			return false
		}
	}
	return true
}
