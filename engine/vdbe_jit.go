package engine

import (
	"slices"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// The JITted VDBE: internal/jit compiles every instruction it has a native
// form for; run() enters that code at any pc and interprets instructions
// it cannot execute (unsupported opcodes, types, overflows, etc.).

// vmJIT is one program's native code.
type vmJIT struct {
	code   *jit.Code
	insns  *Instruction // the program it was compiled from: run() checks identity
	n      int
	nReg   int
	entry  []bool        // the pcs the VDBE enters native code at
	marked []Instruction // the program with OpJIT at each entry: what run() dispatches on
	native int           // how many of its instructions have native code (the JIT score's static share)
	// rows[pc] is whether native code entered at pc can reach an
	// OpResultRow (jit.VReachesResultRow): the entries that need a row
	// buffer. nil when it reaches none from anywhere.
	rows []bool

	// curs is the cursor numbers the native code reads or advances (VColumn,
	// VNext), and nCurs the table size it indexes (the largest + 1). gate[pc]
	// is the first of them an entry at pc reaches, or -1: the VDBE skips an
	// entry whose cursor native code could not use, which would only pay for
	// the call and exit at that instruction.
	curs  []int
	nCurs int
	// entryCurs[pc] is the part of curs that native code entered at pc can
	// reach before it exits: all an entry hands over and brings back.
	entryCurs [][]int
	gate      []int16
	// fill[pc] lists, for an entry at pc, the columns of each cursor that the
	// native code on the straight path from pc reads (VColumn): what the
	// engine copies out of a cursor it cannot hand over as a row list. Nil
	// for a pc with none.
	fill [][]curCols
	// light[pc] is how many cursors an entry at pc may copy rows out of and
	// still do enough besides its column reads to pay for them (copyLimit):
	// 0 or 1, or -1 for any; nil for no entry with a limit.
	light []int8

	// updAt is the pc of the OpUpdateRow whose record native code gathers
	// (gatherUpdate), or -1. Its rows are the ones a call hands back, and the
	// VDBE applies them as soon as the call returns (vdbe.applyGathered).
	updAt int

	// aggAt is the pc of the OpHashAggStep whose registers native code
	// gathers (gatherAggStep), or -1: entries before aggSort gather into
	// vdbe.aggRows, and the VDBE steps each row as soon as the call returns.
	// aggLo..aggHi are the registers gathered, aggKey the key's record and
	// aggKeyLo its registers' first.
	aggAt, aggSort, aggLo, aggHi, aggKey, aggKeyLo, aggKeyN int

	// updSkip is the gathered record's Skip (gatherSkip): the columns whose
	// registers may be stale when the record is made, which updateRow then
	// reads from the cursor's row instead.
	updSkip uint64
	updPlan *updatePlan // that OP_UpdateRow's

	// params is how many of the nReg registers, the last ones, hold the bound
	// parameters (Program.jitParams); enter fills them.
	params int

	// text is whether the code has TEXT arms: enter says whether TEXTs
	// compare converted, which VTextCmpJump leaves to the VDBE
	// (ProgArgs.UTF16), and that no bytes are checked yet (TextPtr).
	text bool
	// like is whether it has a VLike, which reads case_sensitive_like
	// (ProgArgs.LikeCS).
	like bool

	// seekCols is every column the native code reads of each cursor: what a
	// seek's rows are copied out with (seekSnap).
	seekCols []curCols

	// reopen is, per cursor number, the derived source whose opens native
	// code may make (reopenSources); nil when there is none.
	reopen []reopenSrc

	// segCap is, per cursor number, whether its VColumns read a segment in
	// place (segCapableCursors).
	segCap []bool

	// tabs is, per cursor number, the table tabFor last worked out for it.
	tabs []atomic.Pointer[vmTab]

	// safeExit is, per pc, the entry native code that stops there resumes at
	// keeping its cursors (vdbe.jDefer), or -1: the VDBE reaches that entry
	// running instructions that touch none and jump nowhere.
	safeExit []int32

	// idxSpecs is, per cursor number, the index seek native code makes
	// (lowerIdxSeeks), and idxs the index idxFor last worked out for it.
	idxSpecs []*idxSeekSpec
	idxs     []atomic.Pointer[vmIdx]

	// inSlots is the subquery cache slot of each VInSub's set, its B.
	inSlots []int

	// walkScan is, per cursor number, whether native code advances the
	// streamed scan (nativeStreamNexts): only then is it handed over a batch
	// at a time, and otherwise its current row with the columns an entry reads.
	walkScan []bool
}

// curCols is one cursor's columns an entry reads.
type curCols struct {
	cursor int
	cols   []int
}

// vmJITEnabled controls the whole-program JIT (off with WithoutJIT).
var vmJITEnabled = jitEnabled

// noVMJIT caches when a Program has no native code.
var noVMJIT = &vmJIT{}

// jitCode is prog's native code, compiled on its first run and cached on the
// Program -- which the plan cache keeps, so a prepared statement compiles once.
// nil when it has none.
//
// The cache is checked against prog.Insns rather than trusted: a Program
// copied by value (limit_offset_halt.go) and given new instructions carries
// the old pointer, and compiles its own.
func (prog *Program) jitCode() *vmJIT { return prog.jitCodeOn(nil) }

// jitCodeOn is jitCode for a machine that runs on p, whose segments inform
// which columns the first compile guesses hold REALs (vmRealLowering).
func (prog *Program) jitCodeOn(p *ReadOnlyPager) *vmJIT {
	if len(prog.Insns) == 0 {
		return nil
	}
	if jx := (*vmJIT)(atomic.LoadPointer(&prog.vmJIT)); jx != nil && (jx == noVMJIT || jx.insns == &prog.Insns[0] && jx.n == len(prog.Insns)) {
		if jx == noVMJIT {
			return nil
		}
		return jx
	}
	jx := compileVMJIT(prog, p)
	if jx == nil {
		atomic.StorePointer(&prog.vmJIT, unsafe.Pointer(noVMJIT))
		return nil
	}
	atomic.StorePointer(&prog.vmJIT, unsafe.Pointer(jx))
	return jx
}

// vmJITLoopFree compiles a program with no loop too. Only tests set it, to run
// the code generator over straight-line programs.
var vmJITLoopFree bool

// programLoops reports whether prog has a loop the VDBE runs: a cursor or
// sorter loop, a subroutine, or a jump back. With none, and none among the
// instructions native code runs (jit.VLoops), each instruction runs at most
// once (a FROM-less SELECT, a constant folded per run), which native code
// cannot pay back the compile for.
func programLoops(prog *Program) bool {
	for pc, in := range prog.Insns {
		switch in.Op {
		case OpNext, OpSorterNext, OpHashAggNext, OpRightJoinSweepNext, OpSegJoinNext, OpGosub:
			return true
		case OpGoto:
			if in.P2 <= pc {
				return true
			}
		}
	}
	return false
}

// maxJITParams bounds the parameters native code reads from registers of
// their own (jitParams); an OpVariable of a later one exits.
const maxJITParams = 32

// jitParams is how many registers past NReg a machine running prog keeps for
// its parameters: native code reads OpVariable's value from there, which
// vmJIT.enter fills from the bound parameters at every entry.
func (prog *Program) jitParams() int {
	if n := atomic.LoadInt32(&prog.paramRegs); n > 0 {
		return int(n - 1)
	}
	n := 0
	for i := range prog.Insns {
		if in := &prog.Insns[i]; in.Op == OpVariable && in.P1 <= maxJITParams {
			n = max(n, in.P1)
		}
	}
	atomic.StoreInt32(&prog.paramRegs, int32(n+1))
	return n
}

// regFile is regs as prog's register file: NReg long, with room past that for
// jitParams. keep says regs' old values may stay; otherwise they are cleared.
func (prog *Program) regFile(regs []Value, keep bool) []Value {
	if want := prog.NReg + prog.jitParams(); cap(regs) < want {
		return make([]Value, prog.NReg, want)
	}
	regs = regs[:prog.NReg]
	if !keep {
		clear(regs)
	}
	return regs
}

// writeJIT is jitCode for a top-level write program (runWrite), which is
// compiled only when it loops over a cursor (an OpNext): an UPDATE's or a
// DELETE's scan, or an INSERT ... SELECT's. A one-row INSERT ... VALUES would
// pay a compile and a mapping for each statement text and run each native
// instruction once.
func (prog *Program) writeJIT() *vmJIT {
	switch atomic.LoadInt32(&prog.writeLoop) {
	case 1:
		return nil
	case 0:
		loops := slices.ContainsFunc(prog.Insns, func(in Instruction) bool { return in.Op == OpNext })
		v := int32(1)
		if loops {
			v = 2
		}
		atomic.StoreInt32(&prog.writeLoop, v)
		if !loops {
			return nil
		}
	}
	return prog.jitCode()
}

var valueLayout = jit.ValueLayout{
	Size:     int(unsafe.Sizeof(Value{})),
	OffTyp:   int(unsafe.Offsetof(Value{}.Typ)),
	OffI:     int(unsafe.Offsetof(Value{}.I)),
	OffF:     int(unsafe.Offsetof(Value{}.F)),
	OffS:     int(unsafe.Offsetof(Value{}.S)),
	TypNull:  byte(Null),
	TypInt:   byte(Int),
	TypFloat: byte(Float),
	TypText:  byte(Text),
}

// compileVMJIT compiles prog, or returns nil: the JIT off, no emitter for this
// platform, or nothing in the program it could run natively. nil means the
// VDBE runs every instruction, which is always correct.
func compileVMJIT(prog *Program, p *ReadOnlyPager) *vmJIT {
	if !vmJITEnabled || len(prog.Insns) == 0 {
		return nil
	}
	vp := make([]jit.VInsn, len(prog.Insns))
	entry := make([]bool, len(prog.Insns))
	native := 0
	rewindable, walkable, streamed := scannableCursors(prog.Insns, prog.NCursors)
	// Whether the program has a pair the fusion pass works on, noted as it is
	// lowered (jit.VFusable), so a program with none skips the pass.
	fusable := false
	nParams := prog.jitParams()
	ipk := writeCursorIPK(prog)
	realVP, realOK := vmRealLowering(prog, p.segStoresReal)
	textVP, textOK := vmTextLowering(prog, p.segStoresText)
	reopen := reopenSources(prog)
	var inSlots []int
	for pc := range prog.Insns {
		vp[pc] = lowerVM(&prog.Insns[pc], prog.NReg)
		if in := &prog.Insns[pc]; (in.Op == OpOpenDerived || in.Op == OpClose) && in.P1 >= 0 && in.P1 < len(reopen) && reopen[in.P1].ds != nil {
			vp[pc] = jit.VInsn{Op: jit.VClose, A: in.P1}
			if in.Op == OpOpenDerived {
				vp[pc].Op = jit.VOpen
			}
		}
		if realOK != nil && realOK[pc] {
			vp[pc] = realVP[pc]
		} else if textOK != nil && textOK[pc] {
			vp[pc] = textVP[pc]
		}
		if in := &prog.Insns[pc]; in.Op == OpInSub && jit.VMTextOrder {
			// An uncorrelated IN of one column, which native code probes in the
			// set the VDBE builds on its first run (inHashSet.ints).
			if plan, isPlan := in.P4.(*inSubPlan); isPlan && !plan.correlated && !plan.shared && len(plan.affs) == 1 && plan.affs[0] != affText &&
				(plan.colls[0] == "" || strings.EqualFold(plan.colls[0], "BINARY")) && in.P1 >= 0 && in.P1 < prog.NReg && in.P2 >= 0 && in.P2 < prog.NReg && in.P3 >= 0 {
				vp[pc] = jit.VInsn{Op: jit.VInSub, A: in.P1, B: len(inSlots), C: in.P2}
				if plan.not {
					vp[pc].Imm = 1
				}
				inSlots = append(inSlots, in.P3)
			}
		}
		switch in := &prog.Insns[pc]; {
		case in.Op == OpVariable && in.P1 >= 1 && in.P1 <= nParams && in.P2 >= 0 && in.P2 < prog.NReg:
			// A shallow copy of the parameter, from the register past NReg
			// that enter fills with it (jitParams).
			vp[pc] = jit.VInsn{Op: jit.VCopy, A: prog.NReg + in.P1 - 1, B: in.P2, Imm: 1}
		case vp[pc].Op == jit.VColumn && vp[pc].A < len(ipk) && ipk[vp[pc].A] == vp[pc].B:
			// A rowid table's INTEGER PRIMARY KEY reads as the rowid.
			vp[pc] = jit.VInsn{Op: jit.VRowid, A: vp[pc].A, C: vp[pc].C}
		}
		if pc > 0 && !fusable && jit.VFusable(vp[pc-1], vp[pc]) {
			fusable = true
		}
		// A column of any cursor is read natively (a cursor native code cannot
		// walk is read from a copy of its current row); only a cursor that
		// materializes its rows is walked natively.
		if c := vp[pc]; !vmJITCursorOps && vmCursorOp(c.Op) {
			vp[pc] = jit.VInsn{Op: jit.VExit}
		} else if c.Op == jit.VRewind && (c.A >= len(rewindable) || !rewindable[c.A]) {
			vp[pc] = jit.VInsn{Op: jit.VExit}
		} else if c.Op == jit.VNext && (c.A >= len(walkable) || !walkable[c.A]) {
			vp[pc] = jit.VInsn{Op: jit.VExit}
		}
		if vp[pc].Op != jit.VExit {
			entry[pc] = true
			native++
		}
	}
	// A table cursor's columns are read from its segment in place when the
	// engine hands it over in segment mode (jx.segMode), from its rows when not.
	segCap := segCapableCursors(prog)
	var segTables map[int]*resolvedTable
	if segCap != nil {
		segTables = map[int]*resolvedTable{}
		for _, in := range prog.Insns {
			if t, isTbl := in.P4.(*resolvedTable); isTbl && in.Op == OpOpenRead {
				segTables[in.P1] = t
			}
		}
	}
	for pc := range vp {
		if vp[pc].Op == jit.VColumn && vp[pc].A < len(segCap) && segCap[vp[pc].A] {
			// The kind the table's segments read the column as, which the read
			// is specialised for (jit.VSegCol.Plain).
			vp[pc].Imm, vp[pc].T = 1, p.segColKind(segTables[vp[pc].A], vp[pc].B)
		}
		// A correlated rowid seek and the rewind that runs it, which native
		// code makes itself over the table's segments (vmJIT.tabFor).
		if in := &prog.Insns[pc]; in.Op == OpSeekRowidHint && in.P3 == 1 && in.P1 >= 0 && in.P1 < len(segCap) && segCap[in.P1] &&
			in.P2 >= 0 && in.P2 < prog.NReg && pc+1 < len(vp) {
			if rw := &prog.Insns[pc+1]; rw.Op == OpRewind && rw.P1 == in.P1 && rw.P4 == nil {
				vp[pc] = jit.VInsn{Op: jit.VSeekRowid, A: in.P1, B: in.P2, T: rw.P2}
				vp[pc+1] = jit.VInsn{Op: jit.VSeekRewind, A: in.P1, T: rw.P2}
				for _, q := range []int{pc, pc + 1} {
					if !entry[q] {
						entry[q] = true
						native++
					}
				}
			}
		}
	}
	// An integer equality index seek of a table cursor, which native code
	// makes itself over the table's one segment (vmJIT.idxFor).
	idxSpecs, idxPCs, idxInner := lowerIdxSeeks(prog, vp, segCap)
	for _, q := range idxPCs {
		if !entry[q] {
			entry[q] = true
			native++
		}
	}
	n, walkScan := nativeStreamNexts(prog.Insns, vp, streamed, segCap, prog.NReg)
	if n > 0 {
		for pc, in := range prog.Insns {
			if in.Op == OpNext && vp[pc].Op == jit.VNext && !entry[pc] {
				entry[pc] = true
			}
		}
		native += n
	}
	if !vmJITLoopFree && !programLoops(prog) && !jit.VLoops(vp) {
		return nil
	}
	updAt := gatherUpdate(prog, vp)
	if updAt >= 0 {
		entry[updAt-1] = true
		native++
	}
	agg := stepGather{at: -1}
	if updAt < 0 {
		agg = gatherAggStep(prog, vp)
	}
	for _, at := range agg.ats {
		// Where the VDBE goes on after stepping a row native code could not
		// gather.
		entry[at+1] = true
		native++
	}
	if native == 0 {
		return nil
	}
	// The VDBE enters only where the code would then do at least
	// vmJITMinWork before handing control back. Every block stays reachable
	// from native code either way.
	//
	// A program left with no entry at all gets no code: its run loop then pays
	// nothing for the JIT, not even the check.
	entries := 0
	for pc := range entry {
		if entry[pc] && !worthEntering(vp, pc) {
			entry[pc] = false
		}
		if entry[pc] {
			entries++
		}
	}
	// A Goto whose target is entered is entered too: the VDBE reaches one
	// after running an instruction native code exited at, and entering at it
	// is entering at its target one dispatch sooner.
	for pc, in := range vp {
		if in.Op == jit.VGoto && !entry[pc] && in.T >= 0 && in.T < len(entry) && entry[in.T] {
			entry[pc] = true
			entries++
		}
	}
	if updAt >= 0 && entry[updAt] {
		// Never at the OpUpdateRow: the VDBE reaches it only having run the
		// OpMakeRecord itself, and the jump past it would drop that record.
		entry[updAt] = false
		entries--
	}
	for _, q := range idxInner {
		// Nor past a native index seek: the VDBE that ran the seek runs the
		// rest of it, its OpAutoIndexOrder above all, which the gotos skip.
		if entry[q] {
			entry[q] = false
			entries--
		}
	}
	if entries == 0 {
		return nil
	}
	b, err := jit.EmitVMFused(vp, prog.NReg+nParams, valueLayout, entry, fusable)
	if err != nil {
		return nil
	}
	code, err := jit.Map(b)
	if err != nil {
		return nil
	}
	marked := append([]Instruction(nil), prog.Insns...)
	for pc, e := range entry {
		if e {
			marked[pc].Op = OpJIT
		}
	}
	jx := &vmJIT{code: code, insns: &prog.Insns[0], n: len(prog.Insns), nReg: prog.NReg + nParams, params: nParams, entry: entry, marked: marked, native: native, updAt: updAt,
		segCap: segCap, walkScan: walkScan, inSlots: inSlots, idxSpecs: idxSpecs,
		aggAt: agg.at, aggSort: agg.sort, aggLo: agg.lo, aggHi: agg.hi, aggKey: agg.key, aggKeyLo: agg.keyLo, aggKeyN: agg.keyN}
	if segCap != nil {
		jx.tabs = make([]atomic.Pointer[vmTab], len(segCap))
	}
	if idxSpecs != nil {
		jx.idxs = make([]atomic.Pointer[vmIdx], len(idxSpecs))
	}
	jx.safeExit = make([]int32, len(prog.Insns))
	for pc := range prog.Insns {
		jx.safeExit[pc] = -1
		if !cursorFreeOp(&prog.Insns[pc]) {
			continue
		}
		q := pc + 1
		for q < len(prog.Insns) && !entry[q] && cursorFreeOp(&prog.Insns[q]) {
			q++
		}
		if q < len(prog.Insns) && entry[q] {
			jx.safeExit[pc] = int32(q)
		}
	}
	if slices.ContainsFunc(vp, func(in jit.VInsn) bool { return in.Op == jit.VOpen || in.Op == jit.VClose }) {
		jx.reopen = reopen
	}
	jx.like = slices.ContainsFunc(vp, func(in jit.VInsn) bool { return in.Op == jit.VLike })
	jx.text = slices.ContainsFunc(vp, func(in jit.VInsn) bool {
		return in.Op == jit.VTextCmpJump || in.Op == jit.VTextCmpSet || in.Op == jit.VSubstr || in.Op == jit.VInText
	})
	if updAt >= 0 {
		jx.updSkip = vp[updAt-1].Skip
		jx.updPlan, _ = prog.Insns[updAt].P4.(*updatePlan)
	}
	if slices.ContainsFunc(vp, func(in jit.VInsn) bool { return in.Op == jit.VResultRow }) {
		jx.rows = jit.VReachesResultRow(vp)
	}
	jx.gate = make([]int16, len(vp))
	jx.fill = make([][]curCols, len(vp))
	seen := map[int]bool{}
	for pc := range vp {
		jx.gate[pc] = -1
		if !entry[pc] {
			continue
		}
		jx.gate[pc] = int16(firstCursor(vp, pc))
		jx.fill[pc] = fillColumns(vp, pc)
		if l := copyLimit(vp, pc); jx.fill[pc] != nil && l >= 0 {
			if jx.light == nil {
				jx.light = make([]int8, len(vp))
				for i := range jx.light {
					jx.light[i] = -1
				}
			}
			jx.light[pc] = int8(l)
		}
	}
	if len(vp) <= vmJITReachLimit {
		jx.entryCurs = make([][]int, len(vp))
		for pc := range vp {
			if !entry[pc] {
				continue
			}
			jx.entryCurs[pc] = []int{}
			for at, r := range jit.VReachFrom(vp, pc) {
				in := vp[at]
				if r && vmCursorOp(in.Op) &&
					!slices.Contains(jx.entryCurs[pc], in.A) {
					jx.entryCurs[pc] = append(jx.entryCurs[pc], in.A)
				}
			}
		}
	}
	for _, in := range vp {
		if vmCursorOp(in.Op) && !seen[in.A] {
			seen[in.A] = true
			jx.curs = append(jx.curs, in.A)
			jx.nCurs = max(jx.nCurs, in.A+1)
		}
		if in.Op == jit.VColumn {
			i := slices.IndexFunc(jx.seekCols, func(cc curCols) bool { return cc.cursor == in.A })
			if i < 0 {
				jx.seekCols = append(jx.seekCols, curCols{cursor: in.A})
				i = len(jx.seekCols) - 1
			}
			if !slices.Contains(jx.seekCols[i].cols, in.B) {
				jx.seekCols[i].cols = append(jx.seekCols[i].cols, in.B)
			}
		}
	}
	return jx
}

// writeCursorIPK is, per cursor number, the INTEGER PRIMARY KEY column of the
// rowid table the cursor reads, or -1: only for a cursor every open of which
// is an OpOpenWrite, whose rows are a row store's and whose IPK slot reads as
// the row's rowid.
func writeCursorIPK(prog *Program) []int {
	ipk := make([]int, prog.NCursors)
	for i := range ipk {
		ipk[i] = -2 // not opened yet
	}
	for i := range prog.Insns {
		in := &prog.Insns[i]
		if !isCursorOpen(in.Op) || in.P1 < 0 || in.P1 >= len(ipk) {
			continue
		}
		col := -1
		if tbl, ok := in.P4.(*tableMeta); ok && in.Op == OpOpenWrite && !tbl.withoutRowid && tbl.ipkIndex >= 0 {
			col = tbl.ipkIndex
		}
		if ipk[in.P1] != -2 && ipk[in.P1] != col {
			col = -1
		}
		ipk[in.P1] = col
	}
	for i := range ipk {
		ipk[i] = max(ipk[i], -1)
	}
	return ipk
}

// isCursorOpen reports whether op opens a table cursor (its P1).
func isCursorOpen(op OpCode) bool {
	return op == OpOpenRead || op == OpOpenDerived || op == OpOpenWrite
}

// vmCursorOp reports whether op reads or moves its cursor (A).
func vmCursorOp(op jit.VOp) bool {
	switch op {
	case jit.VColumn, jit.VRowid, jit.VNext, jit.VRewind, jit.VOpen, jit.VClose, jit.VSeekRowid, jit.VSeekIdx, jit.VSeekRewind:
		return true
	}
	return false
}

// reopenSrc is the derived source and run-once slot every open of a cursor
// names.
type reopenSrc struct {
	ds   *derivedSource
	slot int
}

// reopenSources is, per cursor number, the derived source every open of the
// cursor reads, when each is an OpOpenDerived of that one source into one
// slot, which always opens a slot-fill cursor (slotSource): native code may
// then close and reopen it (VOpen, VClose) while another open would give it
// the rows it holds (vdbeCursor.slotReopenable). nil when no cursor
// qualifies.
func reopenSources(prog *Program) []reopenSrc {
	var out []reopenSrc
	bad := make([]bool, prog.NCursors)
	for i := range prog.Insns {
		in := &prog.Insns[i]
		if !isCursorOpen(in.Op) && in.Op != OpIfEmpty || in.P1 < 0 || in.P1 >= prog.NCursors || bad[in.P1] {
			continue
		}
		if in.Op == OpIfEmpty {
			if _, opens := in.P4.(*resolvedTable); !opens {
				continue
			}
		}
		ds, _ := in.P4.(*derivedSource)
		if in.Op != OpOpenDerived || !slotSource(ds) || out != nil && out[in.P1].ds != nil && (out[in.P1].ds != ds || out[in.P1].slot != in.P2) {
			bad[in.P1] = true
			if out != nil {
				out[in.P1] = reopenSrc{}
			}
			continue
		}
		if out == nil {
			out = make([]reopenSrc, prog.NCursors)
		}
		out[in.P1] = reopenSrc{ds: ds, slot: in.P2}
	}
	return out
}

// slotSource reports whether OpOpenDerived always opens ds as a slot-fill
// cursor over run-once rows (openSlotFillCursor): no source its case takes
// first, and a CTE or a subquery it neither runs again at every rewind,
// streams, nor runs per outer row.
func slotSource(ds *derivedSource) bool {
	if ds == nil || ds.catalogScope != scopeAny || ds.vtabWrite != nil || ds.schemaWrite || ds.vtab != nil || ds.recSelf != nil {
		return false
	}
	if ds.cte != nil {
		b := ds.cte.binding
		return !ds.rerun || b.m10d != cteM10dNo && b.nUse >= 2
	}
	return ds.prog != nil && !ds.prog.Correlated && !ds.rerun && ds.stream == streamNever && ds.upfrom == nil && ds.viewUpfrom == nil
}

// The states handReopen hands a reopenable cursor over in (VCursor.Reopen).
const (
	reopenOpen   = 1 // open over its slot's rows
	reopenFresh  = 2 // opened, not yet rewound
	reopenClosed = 3
)

// handReopen hands cur, cursor c, over as one native code may close and
// reopen (VOpen, VClose), when it is one: another open would give it its
// slot's rows again (slotReopenable), and it is open over them, opened and not
// yet rewound, or closed. Its view holds those rows in each state.
func (jx *vmJIT) handReopen(cur *vdbeCursor, c int, jc *jit.VCursor) bool {
	if c >= len(jx.reopen) || jx.reopen[c].ds == nil || !cur.slotReopenable(jx.reopen[c].ds, jx.reopen[c].slot) {
		return false
	}
	sf := cur.slot
	switch {
	case cur.materialized && sameRows(cur.rows, sf.rows) && len(cur.rowids) == len(sf.ids) && &cur.rowids[0] == &sf.ids[0] && jitCursorOK(cur):
		jc.Ok, jc.Pos, jc.Reopen = 1, int64(cur.pos), reopenOpen
	case !cur.materialized && cur.fill != nil:
		// As the VDBE's advance before a rewind: no row (VOpen's position).
		jc.Ok, jc.Pos, jc.Reopen = 1, int64(len(sf.rows)), reopenFresh
	case cur.rows == nil && cur.fill == nil:
		jc.Reopen = reopenClosed
	default:
		return false
	}
	jc.Rows, jc.N, jc.Rowids = unsafe.Pointer(unsafe.SliceData(sf.rows)), int64(len(sf.ids)), unsafe.Pointer(unsafe.SliceData(sf.ids))
	jc.Adv, jc.Rewind, jc.Mask = 1, 1, jitCursorMask(cur)
	return true
}

// syncReopen brings a cursor handReopen handed over up to date with what
// native code did to it, as the VDBE's opens, rewinds, advances and closes of
// it would have left it.
func syncReopen(cur *vdbeCursor, jc *jit.VCursor, params []Value) {
	switch {
	case jc.State == 0 && jc.Reopen == reopenOpen:
		cur.pos = int(jc.Pos)
		if jc.Last >= 0 {
			cur.rowid, cur.rowVals, cur.rowRaw = cur.rowids[jc.Last], cur.rows[jc.Last], nil
		}
	case jc.Last >= 0:
		// Rewound since it was last opened, and on row Last or past it.
		cur.slotRewound()
		cur.params = params
		cur.pos = int(jc.Pos)
		cur.rowid, cur.rowVals = cur.rowids[jc.Last], cur.rows[jc.Last]
	case jc.State != 0 && cur.slot.ds.lazy:
		cur.slotOpened()
	case jc.State != 0:
		// The open rewinds a source that is not lazy.
		cur.slotRewound()
	}
	if jc.Ok == 0 {
		cur.close()
	}
}

// gatherUpdate lowers an UPDATE's OpMakeRecord and the OpUpdateRow right after
// it so that native code does not return to the VDBE for every row: the record
// becomes a gathered OP_ResultRow of its registers and the rowid register
// after them, and the OpUpdateRow a jump past itself. It returns that
// OpUpdateRow's pc, or -1 when prog has no such pair.
//
// The VDBE applies the gathered rows, in order, as soon as the call returns
// and before it runs anything else (vdbe.applyGathered), so every instruction
// the VDBE runs still sees each row stored that the program would have stored
// by then. Native code itself reads no row store, only its cursors' frozen
// rows and registers, which storing a row does not change.
//
// Only an UPDATE whose plan allows it (updatePlan.gather: the rowid register
// is the cursor's rowid, so a gathered row names the row it updates), and
// only in a program with no OP_ResultRow of its own, whose rows would be
// mixed with these.
func gatherUpdate(prog *Program, vp []jit.VInsn) int {
	at := -1
	for pc := range prog.Insns {
		in := &prog.Insns[pc]
		switch in.Op {
		case OpResultRow:
			return -1
		case OpUpdateRow:
			plan, _ := in.P4.(*updatePlan)
			if pc == 0 || plan == nil || !plan.gather || at >= 0 {
				return -1
			}
			mr := &prog.Insns[pc-1]
			if mr.Op != OpMakeRecord || mr.P3 != in.P2 || in.P3 != mr.P1+mr.P2 || mr.P1 < 0 || in.P3 >= prog.NReg {
				return -1
			}
			at = pc
		}
	}
	if at < 0 || at+1 >= len(prog.Insns) {
		return -1
	}
	mr := &prog.Insns[at-1]
	vp[at-1] = jit.VInsn{Op: jit.VResultRow, A: mr.P1, B: mr.P2 + 1, Skip: gatherSkip(prog, at)}
	vp[at] = jit.VInsn{Op: jit.VGoto, T: at + 1}
	return at
}

// gatherSkip is the registers a gathered UPDATE record may leave out
// (VInsn.Skip): the columns the UPDATE does not assign, which applyGathered
// never reads from a gathered row -- updateRow takes them from the stored row,
// or from the cursor's own (updSkip). Leaving them out lets the JIT drop the
// OP_Column that loads each, when nothing else reads it.
//
// Only in a program made of the plain instructions listed here, none of which
// names such a register except the OP_Column that loads it and the record
// itself: then a register left stale (its OP_Column dropped) is read by
// nothing but the record, which takes the column from elsewhere. Anything
// else -- a trigger, a constraint's substitution, a function's argument run
// -- keeps them all.
// stepGather is gatherAggStep's verdict: at is -1 when it lowered nothing.
type stepGather struct {
	at, sort, lo, hi, key, keyLo, keyN int
	ats                                []int // every step, at the first
}

// gatherAggStep lowers a hashed GROUP BY's one OpHashAggStep, when it reads
// nothing but registers (aggPlan.rowFree) or its row is its key's
// (aggPlan.rowFromKeys), to a VResultRow that gathers them:
// the key's and every register an aggregate steps from. Its OpMakeRecord is a
// view of the key's registers (recView), which native code skips; enter keeps
// the record pointing at them. Native code then goes on past many rows before
// the VDBE steps them in order (vdbe.stepGathered).
//
// Only where no entry can run both it and an OpResultRow: every OpResultRow
// follows the OpHashAggSort, which native code never runs, and no native jump
// crosses it, so the entries before it are the ones that gather.
func gatherAggStep(prog *Program, vp []jit.VInsn) stepGather {
	none := stepGather{at: -1}
	var ats []int
	sort := -1
	for pc := range prog.Insns {
		switch in := &prog.Insns[pc]; in.Op {
		case OpHashAggStep:
			// A LEFT JOIN's null row steps the same key and registers again.
			if len(ats) > 0 {
				if first := &prog.Insns[ats[0]]; in.P1 != first.P1 || in.P2 != first.P2 || in.P4 != first.P4 {
					return none
				}
			}
			ats = append(ats, pc)
		case OpHashAggSort:
			if sort >= 0 {
				return none
			}
			sort = pc
		case OpResultRow:
			if sort < 0 {
				return none
			}
		}
	}
	if len(ats) == 0 || sort < ats[len(ats)-1] || vp[sort].Op != jit.VExit {
		return none
	}
	for _, at := range ats {
		if vp[at].Op != jit.VExit {
			return none
		}
	}
	step := &prog.Insns[ats[0]]
	plan, _ := step.P4.(*aggPlan)
	if plan == nil || !plan.rowFree && !plan.rowFromKeys() {
		return none
	}
	var mrAts []int
	for pc := range sort {
		if in := &prog.Insns[pc]; in.Op == OpMakeRecord && in.P3 == step.P1 {
			_, view := in.P4.(recView)
			if !view || vp[pc].Op != jit.VExit || len(mrAts) > 0 && (in.P1 != prog.Insns[mrAts[0]].P1 || in.P2 != prog.Insns[mrAts[0]].P2) {
				return none
			}
			mrAts = append(mrAts, pc)
		}
	}
	if len(mrAts) == 0 {
		return none
	}
	mr := &prog.Insns[mrAts[0]]
	if !plan.rowFree && mr.P2 != len(plan.keyCols) {
		return none
	}
	lo, hi := mr.P1, mr.P1+mr.P2
	for _, it := range aggPlanTemplates(plan) {
		for which := range it.rowRegs {
			if it.rowExpr(which) != nil && it.rowRegs[which] <= 0 {
				return none
			}
		}
		if len(it.obRegs) != len(it.orderBy) {
			return none
		}
		for _, r := range it.obRegs {
			if r <= 0 {
				return none
			}
		}
		for _, r := range segJoinStepRegs(it) {
			lo, hi = min(lo, r), max(hi, r+1)
		}
	}
	if lo < 0 || hi > prog.NReg || hi-lo > 32 {
		return none
	}
	for pc, in := range vp {
		if in.Op == jit.VExit || in.T == 0 {
			continue
		}
		if pc < sort && in.T > sort || pc > sort && in.T < sort {
			return none
		}
	}
	for _, pc := range mrAts {
		vp[pc] = jit.VInsn{Op: jit.VGoto, T: pc + 1}
	}
	for _, pc := range ats {
		vp[pc] = jit.VInsn{Op: jit.VResultRow, A: lo, B: hi - lo}
	}
	return stepGather{at: ats[0], ats: ats, sort: sort, lo: lo, hi: hi, key: step.P1, keyLo: mr.P1, keyN: mr.P2}
}

// aggGathers is whether an entry at pc gathers OpHashAggStep's rows
// (gatherAggStep).
func (jx *vmJIT) aggGathers(pc int) bool { return jx.aggAt >= 0 && pc < jx.aggSort }

func gatherSkip(prog *Program, at int) uint64 {
	mr := &prog.Insns[at-1]
	borrow, _ := mr.P4.(recBorrow)
	if len(borrow) == 0 || len(borrow) > 63 {
		return 0
	}
	var skip uint64
	for c, b := range borrow {
		if b && c < mr.P2 {
			skip |= 1 << uint(c)
		}
	}
	isSkipped := func(r int) bool {
		i := r - mr.P1
		return i >= 0 && i < mr.P2 && skip&(1<<uint(i)) != 0
	}
	for pc := range prog.Insns {
		if pc == at || pc == at-1 {
			continue
		}
		in := &prog.Insns[pc]
		// Which of P1..P3 name registers; where unsure, all three, which
		// can only cost the skip.
		regs := [3]bool{true, true, true}
		switch in.Op {
		case OpColumn, OpRowid:
			// They read a cursor, not a register, and the one that loads a
			// skipped register is what may be dropped.
			continue
		case OpInit, OpGoto, OpOpenWrite, OpRewind, OpNext, OpHalt:
			continue // cursors, jump targets and constants only
		case OpIf, OpIfNot, OpIsNull, OpNotNull:
			regs = [3]bool{true, false, false} // P2 is the jump target
		case OpAffinity:
			if _, one := in.P4.(affinity); !one {
				return 0 // a run of registers
			}
			regs = [3]bool{true, false, false}
		case OpSeekRowidSetHint:
			for r := in.P2; r < in.P2+in.P3; r++ {
				if isSkipped(r) {
					return 0
				}
			}
			continue
		case OpSeekIndexHint:
			if h, _ := in.P4.(*indexSeekHint); h != nil && h.in > 0 {
				for r := in.P2; r < in.P2+h.in; r++ {
					if isSkipped(r) {
						return 0
					}
				}
				continue
			}
		case OpVariable, OpSeekRowidHint, OpSeekRowidRangeHint, OpInteger, OpInt64, OpNull, OpSCopy, OpCopy,
			OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight,
			OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpNot, OpNegative:
			if in.Op == OpNull && in.P3 > in.P2 {
				return 0 // a run of registers
			}
		default:
			return 0
		}
		if regs[0] && isSkipped(in.P1) || regs[1] && isSkipped(in.P2) || regs[2] && isSkipped(in.P3) {
			return 0
		}
	}
	return skip
}

// scannableCursors marks the cursor numbers native code may rewind and the
// ones it may advance: every instruction that opens the cursor opens a
// materializing kind. A table read that is not a streamed scan (OpOpenRead, P5
// bit 0) and a derived source are both. A write-path cursor (OpOpenWrite) may
// be advanced -- its OP_Next is advance() over its frozen row list, which the
// engine replays after the call (enter) -- but not rewound, which rebuilds
// the list. A cursor nothing opens is neither.
func scannableCursors(insns []Instruction, nCursors int) (rewind, next, streamed []bool) {
	rewind, next, streamed = make([]bool, nCursors), make([]bool, nCursors), make([]bool, nCursors)
	opened := make([]bool, nCursors)
	for i := range insns {
		in := &insns[i]
		var rew, adv bool
		switch in.Op {
		case OpOpenRead:
			// A scan that may stream (P5 1, or 4 when its rows are in its key's
			// order) is advanced natively only where nativeStreamNexts says.
			rew, adv = in.P5&1 == 0, in.P5&5 == 0
		case OpOpenDerived:
			rew, adv = true, true
		case OpOpenWrite:
			rew, adv = false, true
		default:
			continue
		}
		if in.P1 < 0 || in.P1 >= nCursors {
			continue
		}
		if !opened[in.P1] {
			opened[in.P1], rewind[in.P1], next[in.P1] = true, rew, adv
			streamed[in.P1] = in.Op == OpOpenRead && in.P5&5 != 0
			continue
		}
		rewind[in.P1] = rewind[in.P1] && rew
		next[in.P1] = next[in.P1] && adv
		streamed[in.P1] = false
	}
	return rewind, next, streamed
}

// segCapableCursors is, per cursor number, whether every open of it is an
// OpOpenRead of a table, so that it may be handed over in segment mode; nil
// where this build reads no segment in place (jit.VMSegCursor).
func segCapableCursors(prog *Program) []bool {
	if !jit.VMSegCursor || prog.NCursors == 0 {
		return nil
	}
	capable, other := make([]bool, prog.NCursors), make([]bool, prog.NCursors)
	for i := range prog.Insns {
		in := &prog.Insns[i]
		if !isCursorOpen(in.Op) || in.P1 < 0 || in.P1 >= prog.NCursors {
			continue
		}
		if _, ok := in.P4.(*resolvedTable); ok && in.Op == OpOpenRead {
			capable[in.P1] = true
		} else {
			other[in.P1] = true
		}
	}
	for c := range capable {
		capable[c] = capable[c] && !other[c]
	}
	return capable
}

// nativeStreamNexts lowers the OP_Next of a streamed scan whose loop body, from
// its top to the Next, native code runs whole: there the scan is walked a
// batch at a time (seekSnap, walk). In a loop that exits to the VDBE along
// the way, an entry at the Next would only pay for a batch and the call --
// except for a scan handed over in segment mode (segCap), which costs no copy:
// its Next is lowered too, and walks the segment in place.
func nativeStreamNexts(insns []Instruction, vp []jit.VInsn, streamed, segCap []bool, nReg int) (n int, walk []bool) {
	for pc := range insns {
		in := &insns[pc]
		if in.Op != OpNext || in.P1 < 0 || in.P1 >= len(streamed) || !streamed[in.P1] || in.P2 < 0 || in.P2 >= pc {
			continue
		}
		whole := !slices.ContainsFunc(vp[in.P2:pc], func(v jit.VInsn) bool { return v.Op == jit.VExit })
		if !whole && (in.P1 >= len(segCap) || !segCap[in.P1]) {
			continue
		}
		if vp[pc] = lowerVM(in, nReg); vp[pc].Op != jit.VExit {
			n++
			if whole {
				if walk == nil {
					walk = make([]bool, len(streamed))
				}
				walk[in.P1] = true
			}
		}
	}
	return n, walk
}

// fillColumns is the columns of each cursor the VColumn instructions on the
// straight path from pc (the path nativeRun weighs, a little longer) read.
func fillColumns(vp []jit.VInsn, pc int) []curCols {
	var out []curCols
	for steps := 0; steps < 48 && pc >= 0 && pc < len(vp) && vp[pc].Op != jit.VExit; steps++ {
		in := vp[pc]
		switch in.Op {
		case jit.VGoto:
			pc = in.T
			continue
		case jit.VResultRow:
			return out // native code returns to Go there
		case jit.VColumn:
			i := slices.IndexFunc(out, func(cc curCols) bool { return cc.cursor == in.A })
			if i < 0 {
				out = append(out, curCols{cursor: in.A})
				i = len(out) - 1
			}
			if !slices.Contains(out[i].cols, in.B) {
				out[i].cols = append(out[i].cols, in.B)
			}
		}
		pc++
	}
	return out
}

// firstCursor is the first cursor number a cursor instruction on the native
// run from pc (the same path nativeRun weighs) names, or -1.
func firstCursor(vp []jit.VInsn, pc int) int {
	for steps := 0; steps < 16 && pc >= 0 && pc < len(vp) && vp[pc].Op != jit.VExit; steps++ {
		switch vp[pc].Op {
		case jit.VGoto:
			pc = vp[pc].T
			continue
		case jit.VColumn, jit.VRowid, jit.VNext, jit.VRewind, jit.VOpen, jit.VClose:
			return vp[pc].A
		case jit.VResultRow:
			return -1
		}
		pc++
	}
	return -1
}

var (
	vmArith = map[OpCode]jit.VOp{OpAdd: jit.VAdd, OpSubtract: jit.VSub, OpMultiply: jit.VMul,
		OpDivide: jit.VDiv, OpRemainder: jit.VRem,
		OpBitAnd: jit.VAnd, OpBitOr: jit.VOr, OpShiftLeft: jit.VShl, OpShiftRight: jit.VShr}
	vmCond = map[OpCode]jit.Cond{OpEq: jit.CondE, OpNe: jit.CondNE, OpLt: jit.CondL,
		OpLe: jit.CondLE, OpGt: jit.CondG, OpGe: jit.CondGE}
)

// vmJITCursorOps is whether Column and Next have native forms: on, except in
// the benchmark that measures what they add.
var vmJITCursorOps = true

// vmJITMinWork is the least native work worth an entry from the VDBE, in
// nativeRun's units. An entry and its exit cost ~4.7ns natively (measured on
// a two-SCopy block), about what the VDBE spends interpreting two copies --
// so a run of copies and constants never pays, while one integer opcode, which
// the VDBE spends several ns on, does once anything rides along with it. The
// platform's figure is jit.VMMinWork: an entry on js/wasm crosses into the JS
// host and back, which costs more than a native call.
var vmJITMinWork = jit.VMMinWork

// vmJITReachLimit is the longest program whose entries each prepare only the
// cursors they reach (vmJIT.entryCurs): finding them walks the program once an
// entry.
const vmJITReachLimit = 2000

// vmJITCursorEntryWork is what an entry whose run reads or advances a cursor
// costs beyond a plain one, in nativeRun's units: the engine fills the native
// view of each cursor before the call and brings the cursor up to date after.
const vmJITCursorEntryWork = 5

// worthEntering reports whether an entry at pc does enough native work to pay
// for itself: vmJITMinWork, plus vmJITCursorEntryWork when the run touches a
// cursor.
func worthEntering(vp []jit.VInsn, pc int) bool {
	w, cursor, _ := nativeRun(vp, pc, vmJITMinWork+vmJITCursorEntryWork)
	if cursor {
		return w >= vmJITMinWork+vmJITCursorEntryWork
	}
	return w >= vmJITMinWork
}

// vmJITCopyWork is the native work, past its column reads, that an entry must
// do to pay for copying cursors' rows out for it (enter's jitSnap): the
// engine reads those columns itself, one call each, so the native reads save
// nothing, and the entry's bookkeeping is several times a plain one's.
// Measured on joins whose inner seek is read in place: a run of two column
// reads and a test made a count over a join 28% slower than the VDBE alone,
// and a join that copied both its cursors' rows for a comparison and a row 47%
// slower. A scan copying its one row is paid for sooner (vmJITCopyWorkOne):
// a filter and a projection over a lazily read segment ran 1.4-1.7x faster.
const (
	vmJITCopyWork    = 24
	vmJITCopyWorkOne = 8
)

// vmJITMathWork is a math kernel's weight in nativeRun, and a substr's: the
// VDBE's call of the same function costs about as much as a dozen of its plain
// steps.
const vmJITMathWork = 24

// copyLimit is how many cursors an entry at pc may have copied out and still
// be entered (light): 0 when it does less than vmJITCopyWorkOne besides
// reading columns, 1 when less than vmJITCopyWork, -1 when it pays for any.
func copyLimit(vp []jit.VInsn, pc int) int {
	w, _, reads := nativeRun(vp, pc, vmJITCopyWork+64)
	switch {
	case w-reads < vmJITCopyWorkOne:
		return 0
	case w-reads < vmJITCopyWork:
		return 1
	}
	return -1
}

// nativeRun weighs the native instructions from pc along the fall-through and
// unconditional-jump path, up to limit: a copy or a constant is 1, a jump
// nothing, anything that computes or tests 2. It reports whether the run
// includes a cursor instruction, and the weight of its column and rowid reads.
func nativeRun(vp []jit.VInsn, pc, limit int) (w int, cursor bool, reads int) {
	for steps := 0; w < limit && steps < 64 && pc >= 0 && pc < len(vp) && vp[pc].Op != jit.VExit; steps++ {
		switch vp[pc].Op {
		case jit.VGoto:
			pc = vp[pc].T
			continue
		case jit.VCopy, jit.VInt:
			w++
		case jit.VColumn, jit.VRowid:
			w += 2
			reads += 2
			cursor = true
		case jit.VNext, jit.VRewind, jit.VOpen, jit.VClose:
			w += 2
			cursor = true
		case jit.VResultRow:
			return w + 2, cursor, reads // native code returns to Go there
		case jit.VMath, jit.VSubstr:
			w += vmJITMathWork
		default:
			w += 2
		}
		pc++
	}
	return w, cursor, reads
}

// lowerVM is in's native form, or VExit. Each case is the INTEGER arm of the
// VDBE's own case for that opcode (vdbe.go), which the VDBE also tests first;
// every other operand type exits to it.
func lowerVM(in *Instruction, nReg int) jit.VInsn {
	exit := jit.VInsn{Op: jit.VExit}
	ok := func(regs ...int) bool {
		for _, r := range regs {
			if r < 0 || r >= nReg {
				return false
			}
		}
		return true
	}
	switch in.Op {
	case OpInit, OpGoto:
		return jit.VInsn{Op: jit.VGoto, T: in.P2}
	case OpInteger:
		if ok(in.P2) {
			return jit.VInsn{Op: jit.VInt, A: in.P2, Imm: int64(in.P1)}
		}
	case OpInt64:
		if v, isInt := in.P4.(int64); isInt && ok(in.P2) {
			return jit.VInsn{Op: jit.VInt, A: in.P2, Imm: v}
		}
	case OpSCopy:
		// A shallow copy of any Value: a string's pointer is shared.
		if ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VCopy, A: in.P1, B: in.P2, Imm: 1}
		}
	case OpCopy:
		// copyValue differs from a plain copy only for a string or blob, whose
		// bytes it copies; native code refuses those.
		if ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VCopy, A: in.P1, B: in.P2}
		}
	case OpResultRow:
		// OpResultRow's copy of the row; the engine hands it over (run's
		// OpJIT case).
		if in.P2 >= 0 && in.P1 >= 0 && in.P1+in.P2 <= nReg {
			return jit.VInsn{Op: jit.VResultRow, A: in.P1, B: in.P2}
		}
	case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		// Divide and Remainder: intArith's integer arm, for a divisor native
		// code checks is neither 0 nor -1.
		if ok(in.P1, in.P2, in.P3) {
			return jit.VInsn{Op: vmArith[in.Op], A: in.P1, B: in.P2, C: in.P3}
		}
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		// Two TEXTs stand as they are under no affinity or TEXT affinity, and
		// are equal under BINARY when their bytes are; two INTEGERs answer
		// before affinity (below). Native code takes those two pairs for = and
		// <> and exits on any other.
		aff := affinity(in.P5 & p5AffMask)
		if coll, _ := in.P4.(string); (in.Op == OpEq || in.Op == OpNe) && (aff == affNone || aff == affText) &&
			(coll == "" || strings.EqualFold(coll, "BINARY")) {
			if in.P5&p5StoreP2 == 0 && ok(in.P1, in.P3) {
				return jit.VInsn{Op: jit.VTextCmpJump, A: in.P1, B: in.P3, T: in.P2, Cond: vmCond[in.Op]}
			}
			if in.P5&p5StoreP2 != 0 && ok(in.P1, in.P3, in.P2) {
				return jit.VInsn{Op: jit.VTextCmpSet, A: in.P1, B: in.P3, C: in.P2, Cond: vmCond[in.Op]}
			}
		}
		// Any other comparison under TEXT affinity is one of text values -- a
		// TEXT column against a string -- which native code would only enter
		// to exit from. The VDBE answers it from the start.
		if aff == affText {
			break
		}
		// Two INTEGERs answer before NULL handling, affinity or collation
		// (compareOp's first arm, vdbe.c:2288), so P4 and P5 matter only for
		// the store-the-result form, which stays the VDBE's.
		if in.P5&p5StoreP2 == 0 && ok(in.P1, in.P3) {
			return jit.VInsn{Op: jit.VCmpJump, A: in.P1, B: in.P3, T: in.P2, Cond: vmCond[in.Op]}
		}
		// ...and in the store form, the same two integers give 0 or 1 in r[P2]
		// (deliverCompare).
		if in.P5&p5StoreP2 != 0 && ok(in.P1, in.P3, in.P2) {
			return jit.VInsn{Op: jit.VCmpSet, A: in.P1, B: in.P3, C: in.P2, Cond: vmCond[in.Op]}
		}
	case OpFunction:
		// substr(x, y, z) of a TEXT whose bytes to y-1+z are ASCII and none NUL
		// is those bytes from y-1, which fnSubstr shares as well.
		if name, _ := in.P4.(string); (name == "substr" || name == "substring") && in.P2 == 3 && ok(in.P1, in.P1+2, in.P3) {
			return jit.VInsn{Op: jit.VSubstr, A: in.P1, C: in.P3}
		}
	case OpIf, OpIfNot:
		if ok(in.P1) {
			want := int64(0)
			if in.Op == OpIf {
				want = 1
			}
			return jit.VInsn{Op: jit.VIf, A: in.P1, T: in.P2, C: in.P3, Imm: want}
		}
	case OpIsNull, OpNotNull:
		if ok(in.P1) {
			want := int64(0)
			if in.Op == OpIsNull {
				want = 1
			}
			return jit.VInsn{Op: jit.VNullJump, A: in.P1, T: in.P2, Imm: want}
		}
	case OpNull:
		if ok(in.P2) {
			return jit.VInsn{Op: jit.VNull, C: in.P2}
		}
	case OpNegative:
		if ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VNeg, A: in.P1, C: in.P2}
		}
	case OpNot:
		// An INTEGER's NOT is boolValue(!isTruthy): 1 for 0, else 0.
		if ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VNot, A: in.P1, C: in.P2}
		}
	case OpAffinity:
		// The identity on an INTEGER (affinityIsIdentity): native code goes on
		// past one and leaves any other value to the VDBE.
		aff, one := in.P4.(affinity)
		if affs, ok := in.P4.([]affinity); ok && len(affs) == 1 {
			aff, one = affs[0], true
		}
		if one && (aff == affInteger || aff == affNumeric) && ok(in.P1) {
			return jit.VInsn{Op: jit.VIntGuard, A: in.P1}
		}
		return exit
	case OpRowid:
		if ok(in.P2) && in.P1 >= 0 {
			return jit.VInsn{Op: jit.VRowid, A: in.P1, C: in.P2}
		}
		return exit
	case OpInList:
		// The search of the run's integers, which OpInList keeps from P3 on
		// once it has built them; any other set, or operand, exits to it.
		if plan, _ := in.P4.(*inListPlan); plan != nil && plan.aff != affText && ok(in.P1, in.P2, in.P3) &&
			in.P3+1+len(plan.lits)+len(plan.params) <= nReg {
			return jit.VInsn{Op: jit.VInSet, A: in.P1, B: in.P3 + 1, C: in.P2, Imm: int64(in.P3)}
		}
		// A list of TEXTs matched as bytes, against the plan's own copy of
		// their addresses.
		if plan, _ := in.P4.(*inListPlan); plan != nil && jit.VMTextOrder && len(plan.texts) > 0 && ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VInText, A: in.P1, B: len(plan.texts), C: in.P2, Imm: int64(uintptr(unsafe.Pointer(&plan.texts[0])))}
		}
	case OpColumn:
		// Native code reads a cursor's column from its row list; whether the
		// cursor is one it may (scannableCursors) is checked by the caller, and
		// whether it is right now, at every entry (jitCursorOK).
		if ok(in.P3) && in.P1 >= 0 && in.P1 < 512 && in.P2 >= 0 && in.P2 < 64 {
			return jit.VInsn{Op: jit.VColumn, A: in.P1, B: in.P2, C: in.P3}
		}
	case OpRewind:
		// Only a cursor the engine has already materialized and may re-walk
		// (jitCursorOK): its rewind is a reset of the position, which the
		// native code does; the first rewind and a re-seek take the VDBE's.
		if in.P1 >= 0 && in.P1 < 512 && in.P4 == nil {
			return jit.VInsn{Op: jit.VRewind, A: in.P1, T: in.P2}
		}
	case OpNext:
		if in.P1 >= 0 && in.P1 < 512 {
			return jit.VInsn{Op: jit.VNext, A: in.P1, T: in.P2}
		}
	case OpGosub:
		if ok(in.P1) {
			return jit.VInsn{Op: jit.VGosub, A: in.P1, T: in.P2}
		}
	case OpReturn:
		if ok(in.P1) {
			return jit.VInsn{Op: jit.VReturn, A: in.P1}
		}
	}
	return exit
}

// vmJITCursorSyncs counts the entries after which native code had advanced a
// cursor and the engine brought the cursor up to date -- what a test reads to
// know the cursor instructions really ran natively -- and
// vmJITWriteCursorSyncs those of a write-path cursor (native OP_Next over an
// UPDATE's or DELETE's row list). Counted only while vmJITCountSyncs, which a
// test sets: two locked adds an entry were a measurable share of a short one.
var (
	vmJITCursorSyncs, vmJITWriteCursorSyncs atomic.Int64
	vmJITSeekSnaps                          atomic.Int64
	// vmJITReopens counts the entries in which native code reopened a
	// derived cursor (VOpen).
	vmJITReopens atomic.Int64
	// vmJITSegWalks counts the entries that walked a cursor in segment mode
	// (segSync), and vmJITTabSeeks those that seeked one natively (seekSync).
	vmJITSegWalks, vmJITTabSeeks atomic.Int64
	// vmJITIdxSeeks counts the index seeks native code made (VSeekIdx) and
	// vmJITIdxSyncs the times the VDBE took such a cursor back (syncIdxSeek).
	vmJITIdxSeeks, vmJITIdxSyncs atomic.Int64
	// vmJITAggGathered counts the hashed GROUP BY steps native code gathered
	// and the VDBE replayed (gatherAggStep).
	vmJITAggGathered atomic.Int64
	vmJITCountSyncs  bool

	// vmJITCursorEnters counts entries into native code that reads or walks a
	// cursor, while vmJITCountCursorEnters is set.
	vmJITCursorEnters      atomic.Int64
	vmJITCountCursorEnters bool
)

// CountVMJITCursorEntersForTest turns counting the whole-program JIT's entries
// that read rows on or off.
func CountVMJITCursorEntersForTest(on bool) { vmJITCountCursorEnters = on }

// VMJITCursorEntersForTest returns how many such entries ran since the last
// call.
func VMJITCursorEntersForTest() int64 { return vmJITCursorEnters.Swap(0) }

// jitCursorOK reports whether native code may advance cur right now: a
// materialized row list advance() walks by position -- no streamed or segment
// source, no NULL row, no masked row. A write-path cursor qualifies: its
// OP_Next is the same walk of its frozen list, and the engine replays what
// advance() does with the row after the call (enter).
func jitCursorOK(cur *vdbeCursor) bool {
	return cur != nil && cur.materialized && cur.sub == nil && cur.vt == nil && cur.segSrc == nil && cur.segCur == nil &&
		cur.rowRaw == nil && !cur.rowidNull &&
		cur.streamErrPending == nil && len(cur.rows) >= len(cur.rowids)
}

// rewindsInVDBE reports whether in, where an entry starts, is an OP_Rewind
// native code would only exit at: VRewind rewinds a cursor walking its own
// row list (jitCursorOK, with the Rewind flag enter sets) or one it reopens,
// and no other.
func (jx *vmJIT) rewindsInVDBE(m *vdbe, in *Instruction) bool {
	if in.Op != OpRewind || in.P1 < 0 || in.P1 >= len(m.cursors) {
		return false
	}
	if in.P1 < len(jx.reopen) && jx.reopen[in.P1].ds != nil {
		return false
	}
	cur := m.cursors[in.P1]
	return cur == nil || !jitCursorOK(cur) || cur.seekReseek || cur.rowStore != nil || cur.refillEach
}

// jitCursorMask is the columns native code may read from cur's row list:
// those where rows[pos] holds what OpColumn reads, now and after any advance.
//
// A row store's rows kept as stored (lazyNorm) differ from what advance()
// makes current in two places only, normalizeRowInto's: the INTEGER PRIMARY
// KEY's slot and the REAL-affinity columns. Every other column reads the same
// from the stored row. And a write-path cursor's current row is the list's
// only until reseekRowStore refreshes it apart from the list; then it has
// none.
func jitCursorMask(cur *vdbeCursor) uint64 {
	mask := ^uint64(0)
	if cur.lazyNorm {
		if !cur.jitLazyMaskOK {
			tbl := cur.rowStore
			if tbl.ipkIndex >= 0 && tbl.ipkIndex < 64 {
				mask &^= 1 << uint(tbl.ipkIndex)
			}
			for i, c := range tbl.cols[:min(len(tbl.cols), 64)] {
				if c.Aff == affReal {
					mask &^= 1 << uint(i)
				}
			}
			cur.jitLazyMask, cur.jitLazyMaskOK = mask, true
		}
		mask = cur.jitLazyMask
	}
	if cur.hasGeneratedCols() {
		// A VIRTUAL generated column's slot may hold its error (valueGenErr),
		// which only the VDBE's read raises.
		cols := cur.columns()
		for i, c := range cols[:min(len(cols), 64)] {
			if c.IsGenerated() && !c.GeneratedStored {
				mask &^= 1 << uint(i)
			}
		}
	}
	if cur.rowStore == nil || cur.pos < 0 || cur.pos >= len(cur.rowids) {
		return mask
	}
	if sameRow(cur.rowVals, cur.rows[cur.pos]) || cur.lazyNorm && sameRow(cur.rowVals, cur.normBuf) {
		return mask
	}
	return 0
}

// sameRow reports whether a and b are the same row: one array, one length.
func sameRow(a, b []Value) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}

// copies is how many cursors an entry at pc would copy a row out of: those it
// reads that native code cannot walk, and a point seek, whose one row
// seekSnap copies for this entry alone.
func (jx *vmJIT) copies(m *vdbe, pc int) int {
	n := 0
	for _, cc := range jx.fill[pc] {
		if cc.cursor >= len(m.cursors) {
			continue
		}
		if cc.cursor < len(jx.reopen) && jx.reopen[cc.cursor].ds != nil {
			continue // handed over whole when it can be (handReopen)
		}
		if cur := m.cursors[cc.cursor]; !jitCursorOK(cur) && (cur == nil || !jx.seekWalkable(cc.cursor, cur) || cur.segSrc.point) &&
			(cur == nil || !jx.segModeOK(cc.cursor, cur) && jx.tabFor(cc.cursor, cur) == nil) {
			n++
		}
	}
	return n
}

// jitSnap is the one-row list a cursor native code can read but not walk is
// handed over as: the row's columns copied out through readColumn, in a list
// of one row. The header array is part of the struct so its address is fixed.
type jitSnap struct {
	row []Value
	hdr [1][]Value
	// segLive is whether this entry handed the cursor over in segment mode
	// (vmJIT.segMode), at row segEntry of its segment.
	segLive  bool
	segEntry int
	// rowCur and the rest name the segment row row copies while it is kept
	// past the call, and rowMask its columns copied so far.
	rowCur  *vdbeCursor
	rowSeg  *segment
	rowIdx  int
	rowPos  int
	rowGen  uint64
	rowMask uint64

	// seek is how many rows seekSnap copied out of a seek, 0 when none; rows,
	// rids and slab hold them. They are kept across entries while the cursor
	// stays on that seek (owner, gen): rows[k] is the list's row base+k. live
	// is whether this entry handed them over, at row entry.
	live  bool
	entry int
	seek  int
	// refs is where each row is in its segment, width a row's length in the
	// slab; win a scan's batch size, and more whether the scan has rows past
	// it.
	refs  []segAheadRow
	width int
	win   int
	more  bool
	rows  [][]Value
	rids  []uint64
	slab  []Value
	mask  uint64
	owner *vdbeCursor
	gen   uint64
	base  int
}

// fillFor is the columns of cursor c an entry reads (vmJIT.fill).
func fillFor(fill []curCols, c int) []int {
	for _, cc := range fill {
		if cc.cursor == c {
			return cc.cols
		}
	}
	return nil
}

// seekGens numbers seeks across every cursor, so a copy is never taken for
// another seek's, even by a cursor recycled at the same address.
var seekGens atomic.Uint64

// jitSeekRows is the most rows of a seek seekSnap copies out, and of a
// scan's batch; a scan's first batch is jitScanRows, each next one twice the
// last, so a scan that stops early decodes little it does not read.
const (
	jitSeekRows = 256
	jitScanRows = 8
)

// seekWalkable is whether seekSnap can hand cursor c's rows over for native
// code to walk.
func (jx *vmJIT) seekWalkable(c int, cur *vdbeCursor) bool {
	src := cur.segSrc
	if src == nil || cur.segCur == nil {
		return false
	}
	if !src.point && !src.list {
		return c < len(jx.walkScan) && jx.walkScan[c]
	}
	return !src.list || len(src.listRows)-src.listPos < jitSeekRows
}

// seekSnap hands native code the rows a rowid or index seek on a segment table
// has left (segPointSeek, segListSeek), or a batch of a scan's: the current
// one and those after it, each with the columns the program reads, in a list
// it may walk. false when cur has no such rows or too many to copy.
//
// The rows' columns are decoded as entries read them: need, this entry's, for
// every row handed over, joining those an earlier entry decoded.
func (jx *vmJIT) seekSnap(c int, cur *vdbeCursor, jc *jit.VCursor, sn *jitSnap, need []int) bool {
	src := cur.segSrc
	if !jx.seekWalkable(c, cur) {
		return false
	}
	at := 0
	switch {
	case src.list:
		at = src.listPos - 1
	case !src.point:
		at = cur.pos
	}
	if sn.seek == 0 || sn.owner != cur || sn.gen != cur.seekGen || at < sn.base || at >= sn.base+sn.seek {
		if !jx.seekCopy(c, cur, sn, at) {
			return false
		}
	}
	sn.decode(cur, need)
	if vmJITCountSyncs {
		vmJITSeekSnaps.Add(1)
	}
	sn.live, sn.entry = true, at-sn.base
	adv := int64(1)
	if sn.more {
		adv = 2
	}
	tab := jc.Tab
	defer func() { jc.Tab = tab }()
	*jc = jit.VCursor{Rows: unsafe.Pointer(unsafe.SliceData(sn.rows)), Rowids: unsafe.Pointer(unsafe.SliceData(sn.rids)),
		N: int64(sn.seek), Pos: int64(sn.entry), Last: -1, Ok: 1, Adv: adv, Mask: sn.mask}
	return true
}

// seekCopy copies the rows of cur's seek from list position at (the current
// row) to the end into sn; or, for a scan, the current row and a batch of
// those after it, which the source keeps for its next to serve (lookahead).
func (jx *vmJIT) seekCopy(c int, cur *vdbeCursor, sn *jitSnap, at int) bool {
	src := cur.segSrc
	scan := !src.point && !src.list
	var cols []int
	for _, cc := range jx.seekCols {
		if cc.cursor == c {
			cols = cc.cols
		}
	}
	if len(cols) == 0 {
		return false
	}
	n := 1
	var ahead []segAheadRow
	switch {
	case src.list:
		n = len(src.listRows) - at
	case scan:
		if sn.owner != cur || sn.gen != cur.seekGen {
			sn.win = jitScanRows
		} else {
			sn.win = min(2*sn.win, jitSeekRows)
		}
		ahead = src.lookahead(sn.win - 1)
		n = 1 + min(len(ahead), sn.win-1)
		sn.more = len(ahead) > n-1 || !src.drained
	}
	if n > jitSeekRows {
		return false
	}
	w := 0
	for _, col := range cols {
		if col >= 64 {
			return false
		}
		w = max(w, col+1)
	}
	slab := slices.Grow(sn.slab[:0], n*w)[:n*w]
	rows, rids := sn.rows[:0], sn.rids[:0]
	refs := append(sn.refs[:0], segAheadRow{cur.segCur, cur.segRow, cur.rowid})
	for k := 1; k < n; k++ {
		if scan {
			refs = append(refs, ahead[k-1])
		} else {
			s, row := src.listSegs[at+k], src.listRows[at+k]
			refs = append(refs, segAheadRow{s, row, s.Rowid(row)})
		}
	}
	for k := range n {
		rows, rids = append(rows, slab[k*w:(k+1)*w:(k+1)*w]), append(rids, refs[k].rowid)
	}
	sn.refs, sn.width = refs, w
	sn.seek, sn.rows, sn.rids, sn.slab, sn.mask = n, rows, rids, slab, 0
	sn.owner, sn.gen, sn.base = cur, cur.seekGen, at
	return true
}

// decode decodes the columns of cols not decoded yet for every row sn hands
// over, column by column over each run of consecutive rows of one segment.
func (sn *jitSnap) decode(cur *vdbeCursor, cols []int) {
	var want []int
	for _, col := range cols {
		if col >= 0 && col < sn.width && sn.mask&(1<<uint(col)) == 0 {
			want = append(want, col)
		}
	}
	if len(want) == 0 {
		return
	}
	n, w, refs := sn.seek, sn.width, sn.refs
	for k0 := 0; k0 < n; {
		s, row := refs[k0].s, refs[k0].row
		k1 := k0 + 1
		for k1 < n && refs[k1].s == s && refs[k1].row == row+k1-k0 {
			k1++
		}
		for _, col := range want {
			segColumnRun(cur.segSrcCols, s, col, row, sn.rids[k0:k1], sn.slab[k0*w+col:], w)
		}
		k0 = k1
	}
	for _, col := range want {
		sn.mask |= 1 << uint(col)
	}
}

// inSets publishes the sets the program's VInSubs probe: each one ready while
// the subquery's cached rows are still those its set was built from.
func (jx *vmJIT) inSets(m *vdbe) *jit.VInSubSet {
	if len(m.jinsub) < len(jx.inSlots) {
		m.jinsub = make([]jit.VInSubSet, len(jx.inSlots))
	}
	for k, slot := range jx.inSlots {
		v := &m.jinsub[k]
		var s *inHashSet
		if slot < len(m.subCache) {
			s = m.subCache[slot].inSet
		}
		if s == nil || !s.intsOK || !s.current(&m.subCache[slot]) {
			v.Ready = 0
			continue
		}
		keys := unsafe.Pointer(unsafe.SliceData(s.ints))
		if v.Keys != keys {
			v.Keys = keys
		}
		v.N, v.HasNull, v.Empty, v.Ready = int64(len(s.ints)), flag(s.hasNull), flag(s.empty), 1
	}
	return &m.jinsub[0]
}

// flag is b as native code's flag word.
func flag(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// resetVCursor is *jc = jit.VCursor{Last: -1}, storing a pointer field only
// where it holds one: a whole-struct store is a bulk write barrier on every
// entry while the collector runs.
func resetVCursor(jc *jit.VCursor) {
	if jc.Rows != nil {
		jc.Rows = nil
	}
	if jc.Rowids != nil {
		jc.Rowids = nil
	}
	if jc.Seg != nil {
		jc.Seg = nil
	}
	if jc.Tab != nil {
		jc.Tab = nil
	}
	if jc.Sel != nil {
		jc.Sel = nil
	}
	if jc.Idx != nil {
		jc.Idx = nil
	}
	jc.N, jc.Pos, jc.Last, jc.Ok, jc.Dirty, jc.Rewind, jc.Adv, jc.Mask, jc.Reopen, jc.State = 0, 0, -1, 0, 0, 0, 0, 0, 0, 0
	jc.Key, jc.Served = 0, 0
}

// segModeOK reports whether cursor c can be handed over in segment mode: its
// columns read in place (segCap), positioned on a segment row whose rows next,
// if any, run on in the same segment -- a point seek's one row, or a scan with
// no rows pulled ahead and no window of a pre-filter's selection.
func (jx *vmJIT) segModeOK(c int, cur *vdbeCursor) bool {
	if c >= len(jx.segCap) || !jx.segCap[c] {
		return false
	}
	src, s := cur.segSrc, cur.segCur
	switch {
	case src == nil || s == nil:
		return false
	case src.list:
		// A list in one segment, on its current entry: walked through its rows
		// (jit.VCursor.Sel).
		k := src.listPos - 1
		return s == src.listOne && k >= 0 && k < len(src.listRows) && cur.segRow == src.listRows[k]
	case src.point:
		return s == src.pointSeg && cur.segRow == src.pointRow
	}
	return src.selPrep == nil && src.aheadPos >= len(src.ahead) && src.si < len(src.segs) && src.segs[src.si] == s && cur.segRow == src.ri-1
}

// segMode hands cursor c over in segment mode: native code reads its current
// segment's column blocks in place (jit.VCursor.Seg) from its row, and walks
// on through the segment's rows the scan yields next. Off the segment's last
// row it exits for the VDBE's OP_Next when a segment follows.
func (jx *vmJIT) segMode(c int, cur *vdbeCursor, jc *jit.VCursor, sn *jitSnap) bool {
	if !jx.segModeOK(c, cur) {
		return false
	}
	src, s := cur.segSrc, cur.segCur
	if src.list {
		v := s.vmSegFor(cur.segSrcCols)
		k := src.listPos - 1
		jc.N, jc.Pos, jc.Last, jc.Ok, jc.Dirty, jc.Rewind, jc.Adv, jc.Mask = int64(len(src.listRows)), int64(k), -1, 1, 0, 0, 1, v.mask
		jc.Reopen, jc.State = 0, 0
		if seg := unsafe.Pointer(&v.seg); jc.Seg != seg {
			jc.Seg = seg
		}
		if rids := v.seg.Rowids; jc.Rowids != rids {
			jc.Rowids = rids
		}
		if sel := unsafe.Pointer(unsafe.SliceData(src.listRows)); jc.Sel != sel {
			jc.Sel = sel
		}
		if jc.Rows != nil {
			jc.Rows = nil
		}
		if jx.idxSpecs != nil && jx.idxSpecs[c] != nil && k >= 0 {
			// A seek native code may make again: the rows it walks on this
			// list first are counted from here (jit.VSeekIdx, State 5).
			jc.State, jc.Last, jc.Served = 5, int64(k), -int64(k+1)
		}
		sn.segLive, sn.segEntry = true, k
		return true
	}
	end, adv := cur.segRow+1, int64(1)
	if !src.point {
		end = src.end
		if src.si+1 < len(src.segs) {
			adv = 2
		}
	}
	v := s.vmSegFor(cur.segSrcCols)
	jc.N, jc.Pos, jc.Last, jc.Ok, jc.Dirty, jc.Rewind, jc.Adv, jc.Mask = int64(end), int64(cur.segRow), -1, 1, 0, 0, adv, v.mask
	jc.Reopen, jc.State = 0, 0
	if seg := unsafe.Pointer(&v.seg); jc.Seg != seg {
		jc.Seg = seg
	}
	if rids := v.seg.Rowids; jc.Rowids != rids {
		jc.Rowids = rids
	}
	if jc.Rows != nil {
		jc.Rows = nil
	}
	if jx.idxSpecs != nil && jx.idxSpecs[c] != nil {
		// As for a list above.
		jc.State, jc.Last, jc.Served = 5, int64(cur.segRow), -int64(cur.segRow+1)
	}
	sn.segLive, sn.segEntry = true, cur.segRow
	return true
}

// vmTab is a table's segments as VSeekRowid searches them (jit.VTab), kept
// on the cursor for the segments it was built from.
type vmTab struct {
	tab   jit.VTab
	segs  []jit.VTabSeg
	at    []*segment // the segment of each of segs
	plans []segColPlan
	// What the answer was worked out from, which it stands for while
	// unchanged: the pager's segments, and a session's store of the table with
	// its write counts. none is a table native code may not seek.
	src    *segSource
	root   uint32
	live   *rowStore
	writes uint64
	swaps  uint32
	none   bool
}

// current reports whether t is still the answer for the table at root over
// src.
func (t *vmTab) current(src *segSource, root uint32) bool {
	return t != nil && t.src == src && t.root == root && (t.live == nil || t.live.writes == t.writes && t.live.swaps == t.swaps)
}

// tabFor returns cursor c's table as VSeekRowid searches it, or nil where the
// segments are not the whole answer for every rowid: rows in the delta log or
// written by the session, a narrower segment, a generated column. The answer
// is kept on the cursor, and on the program for the cursors it opens next,
// while what it was worked out from is unchanged.
func (jx *vmJIT) tabFor(c int, cur *vdbeCursor) *vmTab {
	if c >= len(jx.segCap) || !jx.segCap[c] || cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil || cur.rowStore != nil {
		return nil
	}
	src, root := cur.pager.segs, cur.tbl.root
	t := cur.vmTab
	if !t.current(src, root) {
		if t = jx.tabs[c].Load(); !t.current(src, root) {
			t = buildVMTab(cur, src)
			jx.tabs[c].Store(t)
		}
		cur.vmTab = t
	}
	if t.none {
		return nil
	}
	return t
}

// buildVMTab works out tabFor's answer.
func buildVMTab(cur *vdbeCursor, src *segSource) *vmTab {
	root := cur.tbl.root
	t := &vmTab{src: src, root: root, none: true}
	if cur.tbl.withoutRowid || cursorHasNoBtree(cur) || cur.hasGeneratedCols() || cur.rowStore != nil {
		return t
	}
	var segs []*segment
	if src.isLive(root) {
		rows, have, err := src.liveRows(root)
		if err != nil || !have || rows == nil {
			return t
		}
		t.live, t.writes, t.swaps = rows, rows.writes, rows.swaps
		var ok bool
		if segs, ok = src.liveBaseSegments(root); !ok {
			return t
		}
	} else if src.cleanFor(root) {
		segs = src.byRoot[root]
	}
	t.plans = segColPlansFor(cur.tbl)
	for _, s := range segs {
		if s == nil || len(s.cols) != len(cur.tbl.cols) {
			return t
		}
		if s.nRows == 0 {
			continue
		}
		v := s.vmSegFor(t.plans)
		t.segs = append(t.segs, jit.VTabSeg{Lo: int64(s.Rowid(0)), Hi: int64(s.Rowid(s.nRows - 1)), Rows: int64(s.nRows),
			Seg: unsafe.Pointer(&v.seg), Mask: v.mask})
		t.at = append(t.at, s)
	}
	if len(t.segs) == 0 {
		return t
	}
	t.tab = jit.VTab{Segs: unsafe.Pointer(&t.segs[0]), N: int64(len(t.segs))}
	t.none = false
	return t
}

// seekSync brings a cursor native code seeked (VSeekRowid) up to date with
// its last seek, as the VDBE's OP_SeekRowidHint and OP_Rewind of that key
// leave it -- a point source on the row, or none -- and the advance off the
// row when native code made it.
func (t *vmTab) seekSync(cur *vdbeCursor, jc *jit.VCursor) {
	cur.seekConfigured, cur.seekNone, cur.seekKey, cur.seekReseek = true, false, jc.Key, true
	cur.segFilter, cur.sorted = nil, nil
	cur.materialized, cur.rows, cur.lazyNorm, cur.rowids, cur.matched = false, nil, false, nil, nil
	cur.seekGen = seekGens.Add(1)
	cur.streamErrPending, cur.rowidNull, cur.rowRaw = nil, false, nil
	cur.pos = -1
	cur.segSrc, cur.segCur = nil, nil
	segRowsServed.Add(jc.Served)
	if jc.State == 3 {
		return // no row has the key
	}
	k := slices.IndexFunc(t.segs, func(ts jit.VTabSeg) bool { return ts.Seg == jc.Seg })
	if k < 0 {
		return
	}
	s, row := t.at[k], int(jc.Last)
	cur.pointSrc = segRowSource{point: true, pointSeg: s, pointRow: row, pointDone: true, cols: t.plans}
	cur.segSrc, cur.segSrcCols = &cur.pointSrc, t.plans
	cur.pos, cur.rowid, cur.segCur, cur.segRow = 0, s.Rowid(row), s, row
	cur.segWidth = min(s.Width(row), len(t.plans))
	if jc.Pos >= jc.N {
		cur.segSrc, cur.segCur = nil, nil // off its one row
	}
}

// segSync brings a cursor segMode handed over up to date with the rows native
// code moved it over, as advance() would have left it.
func segSync(cur *vdbeCursor, jc *jit.VCursor, sn *jitSnap) {
	sn.segLive = false
	if jc.Dirty == 0 {
		return
	}
	if moved := int(jc.Last) - sn.segEntry; jc.Last >= 0 && moved > 0 {
		cur.pos += moved
		switch src := cur.segSrc; {
		case src.list:
			// Last is a position in the list, which names the row.
			cur.segRow, src.listPos = src.listRows[jc.Last], int(jc.Last)+1
		default:
			cur.segRow = int(jc.Last)
			if !src.point {
				src.ri = cur.segRow + 1
			}
		}
		cur.rowid = cur.segCur.Rowid(cur.segRow)
		cur.segWidth = min(cur.segCur.Width(cur.segRow), len(cur.segSrcCols))
		segRowsServed.Add(int64(moved))
	}
	if jc.Pos >= jc.N && jc.Adv == 1 {
		cur.segSrc, cur.segCur = nil, nil
	}
}

// seekSync brings a cursor seekSnap handed over up to date with the native
// advance, as advance() would have left it.
func seekSync(cur *vdbeCursor, jc *jit.VCursor, sn *jitSnap) {
	sn.live = false
	if jc.Dirty == 0 {
		return
	}
	if moved := int(jc.Last) - sn.entry; jc.Last >= 0 && moved > 0 {
		src, i := cur.segSrc, sn.base+int(jc.Last)
		cur.pos += moved
		r := sn.refs[jc.Last]
		cur.segCur, cur.segRow, cur.rowid = r.s, r.row, r.rowid
		if src.list {
			segRowsServed.Add(int64(moved))
			src.listPos = i + 1
		} else {
			// Batch row k is the source's ahead[k-1] (lookahead): the cursor on
			// row k has taken k of them.
			segRowsServed.Add(int64(int(jc.Last) - src.aheadPos))
			src.aheadPos = int(jc.Last)
		}
		cur.segWidth = min(cur.segCur.Width(cur.segRow), len(cur.segSrcCols))
	}
	if int(jc.Pos) >= sn.seek {
		cur.segSrc, cur.segCur = nil, nil
	}
}

// jitReadColumn is cur.readColumn for a cursor whose state is not known to
// hold a row: false, writing nothing, where the row has no column i.
func jitReadColumn(cur *vdbeCursor, i int, dst *Value) bool {
	switch {
	case cur.segCur != nil:
		*dst = cur.segColumn(i)
		return true
	case cur.rowRaw != nil:
		if !cur.colMask.has(i) {
			if cur.decodeFullRow() != nil {
				return false
			}
		}
	}
	if i < 0 || i >= len(cur.rowVals) || cur.rowVals[i].Typ >= valueGenErr {
		return false
	}
	*dst = cur.rowVals[i]
	return true
}

// enter runs native code from pc and returns the pc the VDBE runs next.
//
// rows is run's list of result rows. A machine that gathers them (no yield)
// lets native code append to it: each OpResultRow's row is carved from
// m.recChunk, as recAlloc would, and its header written into the list's spare
// capacity; enter takes both over after the call. n is then -1.
//
// A machine that hands each row over gets at most one row per call: n is
// then its width, copied into m.yieldRow's array when the machine reuses it
// (and rows is empty), else to the head of m.recChunk, and pc is the one after
// the OpResultRow. The caller hands it over (run's OpJIT case). n is -1 when
// native code ran no OpResultRow.
func (jx *vmJIT) enter(m *vdbe, pc int, rows *[][]Value) (next, n int) {
	regs := m.regs
	if cap(regs) < jx.nReg || jx.nReg == 0 {
		m.flushJIT()
		return pc, -1
	}
	resume := m.jDefer && m.jDeferJX == jx && m.jDeferPC == pc
	if !resume && m.jDefer {
		m.flushJIT()
	}
	args := &m.jargs
	if jx.params > 0 {
		// The bound parameters, where native code's OpVariable reads them.
		regs = regs[:jx.nReg]
		base := jx.nReg - jx.params
		for i := range jx.params {
			regs[base+i] = paramAt(m.params, i+1)
		}
	}
	args.Regs, args.PC, args.Cur = (*int64)(unsafe.Pointer(&regs[0])), int64(pc), nil
	if jx.text {
		args.UTF16, args.TextPtr = 0, 0
		if isUTF16(m.encoding()) {
			args.UTF16 = 1
		}
	}
	if jx.like {
		args.LikeCS = 0
		if m.likeCaseSensitive() {
			args.LikeCS = 1
		}
	}
	if len(jx.inSlots) > 0 {
		args.InSets = jx.inSets(m)
	}
	args.RowBuf, args.RowCap, args.RowUsed, args.RowsBuf, args.RowsCap, args.Yield = nil, -1, 0, nil, 0, 0
	gather := jx.updAt >= 0 || jx.aggGathers(pc)
	switch {
	case jx.rows == nil || !jx.rows[pc] || m.firstRow:
		// No OpResultRow native code runs; or the VDBE's stops the run with an
		// empty row.
	case !gather && m.yieldReuse && len(*rows) == 0:
		if c := cap(m.yieldRow); c > 0 {
			args.RowBuf, args.RowCap = (*int64)(unsafe.Pointer(unsafe.SliceData(m.yieldRow[:c]))), int64(c)
		} else {
			args.RowCap = 0
		}
	default:
		// An UPDATE's gathered rows (gatherUpdate) go to a buffer of their
		// own, which the VDBE stores out of and reuses (vdbe.gatherChunk).
		buf := m.recChunk
		if gather {
			buf = m.gatherChunk
		}
		if c := len(buf); c > 0 {
			args.RowBuf, args.RowCap = (*int64)(unsafe.Pointer(&buf[0])), int64(c)
		} else {
			args.RowCap = 0
		}
		// A step's rows are the VDBE's own, never handed over one by one.
		if !m.yield || jx.aggGathers(pc) {
			if cap(*rows) == len(*rows) {
				// Grown as append would grow it, so a subquery that returns one
				// row allocates for one; native code gathers rows only as far as
				// the capacity goes, and the VDBE's append past it grows the next.
				*rows = slices.Grow(*rows, 1)
			}
			spare := (*rows)[len(*rows):cap(*rows)]
			args.RowsBuf, args.RowsCap = (*int64)(unsafe.Pointer(unsafe.SliceData(spare))), int64(len(spare))
		}
	}
	curs := jx.curs
	if jx.entryCurs != nil {
		curs = jx.entryCurs[pc]
	}
	if jx.nCurs > 0 {
		if len(m.jcur) < jx.nCurs {
			m.jcur = make([]jit.VCursor, jx.nCurs)
			m.jsnap = make([]jitSnap, jx.nCurs)
		}
		if !resume {
			m.jLive = m.jLive[:0]
		}
		for _, c := range curs {
			if resume && slices.Contains(m.jLive, c) {
				continue // as native code left it
			}
			jx.handCursor(m, pc, c)
			m.jLive = append(m.jLive, c)
		}
		args.Cur = &m.jcur[0]
	}
	m.jDefer = false
	jx.code.Call2(args)
	if jx.nCurs > 0 {
		// Stopped at an instruction that touches no cursor, which the VDBE
		// runs before it enters again at the next: native code keeps them.
		if exit := int(args.PC); args.Yield == 0 && exit >= 0 && exit < len(jx.safeExit) && jx.safeExit[exit] >= 0 {
			m.jDefer, m.jDeferPC, m.jDeferJX = true, int(jx.safeExit[exit]), jx
		} else {
			for _, c := range m.jLive {
				jx.syncCursor(m, c, exit)
			}
			m.jLive = m.jLive[:0]
		}
	}
	if args.Yield > 0 {
		if args.RowsBuf != nil {
			// The rows native code gathered: their headers are in the list's
			// spare capacity, their Values the chunk's first RowUsed.
			k := int(args.Yield)
			*rows = (*rows)[:len(*rows)+k]
			if !gather {
				m.recChunk = m.recChunk[args.RowUsed:]
			}
			m.jitRows += k
			return int(args.PC), -1
		}
		return int(args.PC), int(args.Yield) - 1
	}
	return int(args.PC), -1
}

// handCursor hands cursor c over to native code entered at pc (m.jcur[c]).
func (jx *vmJIT) handCursor(m *vdbe, pc, c int) {
	jc := &m.jcur[c]
	resetVCursor(jc)
	var cur *vdbeCursor
	if c < len(m.cursors) {
		cur = m.cursors[c]
	}
	if jx.idxSpecs != nil && jx.idxSpecs[c] != nil && jx.idxSpecs[c].open >= 0 && (cur == nil || cur.closed || cur.tbl == nil) {
		// A seek cursor native code opens itself (VOpen), not open now: it
		// seeks the table its open names, as last worked out for an open
		// cursor while that still stands.
		if t := jx.openedTab(m, c); t != nil {
			jc.Tab = unsafe.Pointer(&t.tab)
			if x := jx.idxFor(c, t); x != nil {
				jc.Idx = unsafe.Pointer(&x.vidx[0])
			}
		}
		jc.Reopen = jit.VReopenSeek
		return
	}
	if cur == nil {
		return
	}
	if t := jx.tabFor(c, cur); t != nil {
		jc.Tab = unsafe.Pointer(&t.tab)
		if jx.idxSpecs != nil && jx.idxSpecs[c] != nil {
			if x := jx.idxFor(c, t); x != nil {
				jc.Idx = unsafe.Pointer(&x.vidx[0])
			}
		}
	}
	if jx.idxSpecs != nil && jx.idxSpecs[c] != nil && jx.idxSpecs[c].open >= 0 {
		// A seek cursor native code may open and close (VOpen, VClose).
		defer func() { jc.Reopen = jit.VReopenSeek }()
	}
	if jx.reopen != nil && jx.handReopen(cur, c, jc) {
		return
	}
	if jitCursorOK(cur) {
		jc.Rows = unsafe.Pointer(unsafe.SliceData(cur.rows))
		// The rowid list is the cursor's rowid only while its position
		// says so: a seek that sets the rowid apart from the list (the
		// UPDATE ... FROM target's, a WITHOUT ROWID key's) leaves pos
		// where it was. Out of the list VRowid exits anyway, and a
		// native advance keeps the two together.
		if p := cur.pos; len(cur.rowids) > 0 && (p < 0 || p >= len(cur.rowids) || cur.rowids[p] == cur.rowid) {
			jc.Rowids = unsafe.Pointer(unsafe.SliceData(cur.rowids))
		}
		jc.N, jc.Pos, jc.Ok, jc.Adv = int64(len(cur.rowids)), int64(cur.pos), 1, 1
		jc.Mask = jitCursorMask(cur)
		if !cur.seekReseek && cur.rowStore == nil && !cur.refillEach {
			// A re-seeking cursor rebuilds its rows at every rewind, and
			// so do a write-path one and an inner co-routine
			// (derivedSource.rerun).
			jc.Rewind = 1
		}
		return
	}
	if jx.segMode(c, cur, jc, &m.jsnap[c]) {
		return
	}
	if jx.seekSnap(c, cur, jc, &m.jsnap[c], fillFor(jx.fill[pc], c)) {
		return
	}
	// Not a cursor native code can walk: hand over a copy of its current
	// row, with the columns this entry reads, and let it read only
	// those. Nothing native moves it (Adv and Rewind are zero), so the
	// copy stays the cursor's row for as long as the code runs.
	for _, cc := range jx.fill[pc] {
		if cc.cursor != c {
			continue
		}
		sn := &m.jsnap[c]
		// A segment row's copy is kept while the cursor stays on it, so a
		// column is copied once however many entries read it.
		var mask uint64
		if cur.segCur != nil && sn.rowCur == cur && sn.rowSeg == cur.segCur && sn.rowIdx == cur.segRow &&
			sn.rowPos == cur.pos && sn.rowGen == cur.seekGen {
			mask = sn.rowMask
		}
		n := len(sn.row)
		if mask == 0 {
			n = 0
		}
		for _, col := range cc.cols {
			n = max(n, col+1)
		}
		if cap(sn.row) < n {
			sn.row = append(make([]Value, 0, n), sn.row...)
		}
		sn.row = sn.row[:n]
		for _, col := range cc.cols {
			if mask&(1<<uint(col)) == 0 && jitReadColumn(cur, col, &sn.row[col]) {
				mask |= 1 << uint(col)
			}
		}
		sn.rowCur = nil
		if cur.segCur != nil {
			sn.rowCur, sn.rowSeg, sn.rowIdx, sn.rowPos, sn.rowGen, sn.rowMask = cur, cur.segCur, cur.segRow, cur.pos, cur.seekGen, mask
		}
		if mask != 0 {
			sn.hdr[0] = sn.row
			jc.Rows, jc.N, jc.Pos, jc.Ok, jc.Mask = unsafe.Pointer(&sn.hdr[0]), 1, 0, 1, mask
		}
	}
}

// syncCursor brings cursor c up to date with what native code, which
// stopped at exit, did to it (m.jcur[c]).
func (jx *vmJIT) syncCursor(m *vdbe, c, exit int) {
	jc := &m.jcur[c]
	if jc.Reopen >= jit.VReopenSeek {
		if jx.syncSeekOpen(m, c, jc) {
			return
		}
	} else if jc.Reopen != 0 {
		if jc.Dirty != 0 {
			syncReopen(m.cursors[c], jc, m.params)
		}
		if vmJITCountSyncs && jc.State != 0 {
			vmJITReopens.Add(1)
		}
		return
	}
	if jc.Idx != nil && jc.State >= 2 && jc.State <= 4 {
		// Native code seeked the cursor through its index: the VDBE's
		// seek of the last key is run here, and walks on to where
		// native code left it -- unless what it stopped at closes or
		// reopens the cursor, which drops that state anyway.
		m.jsnap[c].segLive, m.jsnap[c].live = false, false
		// Served is the rows the seeks before the last served; the
		// last served up to the cursor's row, which the VDBE's walk
		// counts when it takes the cursor back.
		last, served := int64(0), jc.Served
		if jc.State != 3 {
			last = jc.Last + 1
		}
		if !jx.idxSpecs[c].listed {
			last, served = 0, 0
		}
		if vmJITCountSyncs {
			vmJITIdxSeeks.Add(max(jc.Served, 0) + 1)
		}
		if at := exit; at >= 0 && at < jx.n {
			if in := unsafe.Slice(jx.insns, jx.n)[at]; in.P1 == c && (in.Op == OpClose || isCursorOpen(in.Op)) {
				segRowsServed.Add(served + last)
				return
			}
		}
		segRowsServed.Add(served)
		if vmJITCountSyncs {
			vmJITIdxSyncs.Add(1)
		}
		if err := m.syncIdxSeek(jx, c, jc, jx.idxSpecs[c]); err != nil && m.jitErr == nil {
			m.jitErr = err
		}
		return
	}
	if jc.Tab != nil && jc.State >= 2 && jc.State <= 4 {
		// Native code seeked the cursor: the VDBE's seek of the last key
		// is run here, and leaves the cursor as native code did.
		m.jsnap[c].segLive, m.jsnap[c].live = false, false
		cur := m.cursors[c]
		cur.vmTab.seekSync(cur, jc)
		if vmJITCountSyncs {
			vmJITTabSeeks.Add(1)
		}
		return
	}
	if sn := &m.jsnap[c]; sn.segLive {
		segSync(m.cursors[c], jc, sn)
		if vmJITCountSyncs && jc.Dirty != 0 {
			vmJITSegWalks.Add(1)
		}
		return
	}
	if sn := &m.jsnap[c]; sn.live {
		seekSync(m.cursors[c], jc, sn)
		return
	}
	if sn := &m.jsnap[c]; len(sn.row) != 0 && sn.rowCur == nil {
		clear(sn.row) // the copy holds no row past the call
		sn.hdr[0] = nil
	}
	if jc.Dirty != 0 {
		// The native advance moved only the position; the cursor's
		// current row, rowid and the rest follow it here, as advance()
		// would have left them. Past the end advance leaves the last row
		// current, and so does this (Last).
		cur := m.cursors[c]
		if vmJITCountSyncs {
			vmJITCursorSyncs.Add(1)
			if cur.rowStore != nil {
				vmJITWriteCursorSyncs.Add(1)
			}
		}
		cur.pos = int(jc.Pos)
		if jc.Last >= 0 {
			cur.rowid = cur.rowids[jc.Last]
			cur.rowVals = cur.rows[jc.Last]
			if cur.lazyNorm {
				cur.rowVals = cur.normalizeStored(cur.rowVals)
			}
			cur.rowRaw = nil
		}
	}
}

// cursorFreeOp reports whether the VDBE's run of in reads or moves no cursor:
// it reads and writes registers only, so native code may keep its cursors
// across it.
func cursorFreeOp(in *Instruction) bool {
	switch in.Op {
	case OpMakeRecord, OpConcat, OpSCopy, OpCopy, OpInteger, OpInt64, OpReal, OpString8, OpNull, OpClearSubtype, OpLike, OpGlob, OpSorterInsert,
		OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight, OpBitNot, OpNegative:
		return true
	case OpFunction:
		switch name, _ := in.P4.(string); name {
		case "date", "time", "datetime", "julianday", "unixepoch", "strftime", "printf", "format",
			"upper", "lower", "length", "abs", "round", "substr", "substring", "replace", "trim", "ltrim", "rtrim",
			"instr", "hex", "typeof", "coalesce", "ifnull", "nullif", "concat", "concat_ws":
			return true
		}
	}
	return false
}
