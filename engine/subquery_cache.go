package engine

import (
	"encoding/binary"
	"math"
	"sync/atomic"
)

// subKeepHits counts the runs kept rows stood in for, so a test can tell
// they were.
var subKeepHits atomic.Int64

// A correlated scalar subquery runs again for every row of its enclosing
// query, though its answer depends only on the enclosing values it reads:
// "(SELECT avg(x) FROM t2 WHERE t2.k = t1.k)" reads t1.k alone. Its rows are
// kept by those values, so each distinct one runs it once. The values are
// what its OpOuterColumn, OpOuterRowid and OpOuterAggReg read (outerRead),
// recorded as the subquery compiles (outerReadSink). A subquery that reaches
// an enclosing row any other way, or calls a function that is not constant,
// is not kept.
//
// The rows it reads do not change while the statement runs: a write
// statement's subqueries read the image from before it, and one that must
// see the database as it stands (a live body) is not kept. Between two rows
// a statement hands back, other statements may change the database, so the
// kept rows are dropped there (vdbe.run).

// outerRead is one enclosing value a correlated subquery reads: what its
// OpOuterColumn (cursor p1's column p2), OpOuterRowid (cursor p1's rowid) or
// OpOuterAggReg (register p1) reads, level frames above the frame that runs
// the subquery (0: that frame itself).
type outerRead struct {
	op     OpCode
	level  int
	p1, p2 int
}

// outerReadSink collects, while a scalar subquery compiles, every read of an
// enclosing frame its code makes. opaque is set when it reaches one in a way
// that is not an outerRead, or calls a function that is not constant.
type outerReadSink struct {
	reads  []outerRead
	opaque bool
}

// emitOuterRead emits in, a read of the frame level frames above c's
// (in.P5), and records it in every sink above c that the frame is at or
// above.
func (c *compiler) emitOuterRead(in Instruction) {
	c.emit(in)
	level := int(in.P5)
	k := 1
	for q := c.outer; q != nil; q, k = q.outer, k+1 {
		if q.subSink != nil && level >= k {
			q.subSink.reads = append(q.subSink.reads, outerRead{op: in.Op, level: level - k, p1: in.P1, p2: in.P2})
		}
	}
}

// poisonSubSinks marks every sink at or above c opaque.
func (c *compiler) poisonSubSinks() {
	for q := c; q != nil; q = q.outer {
		if q.subSink != nil {
			q.subSink.opaque = true
		}
		if q.aggSkipped != nil && q.aggSkipped.subSink != nil {
			q.aggSkipped.subSink.opaque = true
		}
	}
}

// subKeepable reports whether nothing prog runs, nor any program it runs in
// turn, reaches an enclosing row except through its outer-read opcodes, or
// calls a function that is not constant. Code compiled off the compile chain
// records nothing in a sink: a virtual table's arguments, coded as it opens,
// and a window's projection; and the MATCH and fts auxiliary opcodes read the
// enclosing row through m.outer.
func subKeepable(prog *Program, seen map[*Program]bool) bool {
	if prog == nil || seen[prog] {
		return true
	}
	if prog.Compound != nil || prog.LiveSource != nil {
		return false
	}
	seen[prog] = true
	item := func(it *itemPlan) bool {
		return it == nil || it.prog == nil || subKeepable(it.prog.prog, seen)
	}
	for _, in := range prog.Insns {
		switch in.Op {
		case OpMatch, OpFts3Aux, OpFts5Aux, OpWindowAppend, OpWindowFinal, OpGroupBatchAppend, OpGroupBatchFinal:
			return false
		}
		switch p4 := in.P4.(type) {
		case *Program:
			if !subKeepable(p4, seen) {
				return false
			}
		case *inSubPlan:
			if !subKeepable(p4.prog, seen) {
				return false
			}
		case *rowSubPlan:
			if !subKeepable(p4.prog, seen) {
				return false
			}
		case *derivedSource:
			if p4.vtab != nil || !subKeepable(p4.prog, seen) {
				return false
			}
		case *aggPlan:
			if !item(p4.havingPlan) {
				return false
			}
			for _, it := range p4.outPlans {
				if !item(it) {
					return false
				}
			}
			for _, op := range p4.orderPlans {
				if !item(op.item) {
					return false
				}
			}
		case *ScalarFunction:
			return false
		case string:
			if in.Op == OpFunction && nonConstantFuncs[r33sFoldIdent(p4)] {
				return false
			}
		}
	}
	return true
}

// maxSubKeep bounds the distinct keys one subquery keeps rows for; past it,
// rows are kept no longer, nor where the keys seldom repeat.
const maxSubKeep = 1 << 16

// subKeep is a correlated subquery's kept rows, by key (subqueryKey).
type subKeep struct {
	rows       map[string][][]Value
	hits, runs int
	off        bool
}

// keptSubquery is OpSubquery's run of a correlated prog whose rows are kept
// in slot: those kept for the enclosing values it reads, or a run that keeps
// them.
func (m *vdbe) keptSubquery(slot int, prog *Program) ([][]Value, error) {
	e := &m.subCache[slot]
	if e.keep == nil {
		e.keep = &subKeep{rows: map[string][][]Value{}}
	}
	k := e.keep
	if k.off {
		return prog.execWithParent(m)
	}
	key, ok, err := m.subqueryKey(prog.outerReads)
	if err != nil {
		return nil, err
	}
	if !ok {
		return prog.execWithParent(m)
	}
	if rows, ok := k.rows[string(key)]; ok {
		k.hits++
		subKeepHits.Add(1)
		return rows, nil
	}
	rows, err := prog.execWithParent(m)
	if err != nil {
		return nil, err
	}
	k.runs++
	if len(k.rows) >= maxSubKeep || k.runs >= 1024 && k.hits < k.runs {
		k.off, k.rows = true, nil
		return rows, nil
	}
	kept := make([][]Value, len(rows))
	for i, r := range rows {
		kept[i] = make([]Value, len(r))
		for j, v := range r {
			if v.S != nil {
				v.S = append([]byte(nil), v.S...)
			}
			kept[i][j] = v
		}
	}
	k.rows[string(key)] = kept
	return kept, nil
}

// subqueryKey is the enclosing values reads read, as the subquery's own
// opcodes would read them now, each encoded whole: its type, subtype and
// value. false where a frame is not there to read.
func (m *vdbe) subqueryKey(reads []outerRead) ([]byte, bool, error) {
	key := m.subKeyBuf[:0]
	for _, rd := range reads {
		fr := m
		for range rd.level {
			if fr = fr.parent; fr == nil {
				return nil, false, nil
			}
		}
		var v Value
		switch rd.op {
		case OpOuterColumn:
			if err := fr.cursors[rd.p1].readColumn(rd.p2, &v); err != nil {
				return nil, false, err
			}
		case OpOuterRowid:
			if cur := fr.cursors[rd.p1]; !cur.rowidNull {
				v = Value{Typ: Int, I: int64(cur.rowid)}
			}
		case OpOuterAggReg:
			v = fr.regs[rd.p1]
		}
		key = append(key, byte(v.Typ), v.Subtype)
		if v.intReal {
			key = append(key, 1)
		} else {
			key = append(key, 0)
		}
		switch v.Typ {
		case Int:
			key = binary.LittleEndian.AppendUint64(key, uint64(v.I))
		case Float:
			key = binary.LittleEndian.AppendUint64(key, math.Float64bits(v.F))
		case Text, Blob:
			key = binary.AppendUvarint(key, uint64(len(v.S)))
			key = append(key, v.S...)
		}
	}
	m.subKeyBuf = key
	return key, true, nil
}
