package engine

// The bodies of opcodes that two executors run: the VDBE's run loop, and the
// service blocks a compiled kernel hands back to Go (segment_service.go). One
// implementation each, so the two cannot drift.

// opFunction is OP_Function: r[P3] = P4(r[P1 .. P1+P2-1]).
func (m *vdbe) opFunction(op *Instruction) error {
	args := growValues(&m.fnArgBuf, op.P2) // vdbe.c:8866; see fnArgBuf
	copy(args, m.regs[op.P1:op.P1+op.P2])
	var v Value
	var ferr error
	switch f := op.P4.(type) {
	case *ScalarFunction:
		// Resolved when the program was built, as C's OP_Function
		// carries its FuncDef (vdbe.c:8850): no lookup per call.
		v, ferr = f.call(args)
	case string:
		if f == "rtreecheck" {
			v, ferr = m.rtreecheck(args) // reads the database; see rtree_check.go
		} else {
			v, ferr = callScalarFuncEnc(f, args, m.likeCaseSensitive(), m.encoding(), op.P5)
		}
	}
	if ferr != nil {
		return ferr
	}
	m.regs[op.P3] = v
	return nil
}

// opLike is OP_Like: r[P2] = r[P1] [NOT] LIKE r[P3] [ESCAPE r[P4]].
func (m *vdbe) opLike(op *Instruction) error {
	x, pat := m.regs[op.P1], m.regs[op.P3]
	escReg, _ := op.P4.(int)
	var (
		esc     rune
		hasEsc  bool
		escNull bool
	)
	if escReg >= 0 {
		r, isNull, eerr := likeEscapeRune(m.regs[escReg])
		if eerr != nil {
			return eerr
		}
		if isNull {
			escNull = true
		} else {
			esc, hasEsc = r, true
		}
	}
	if escNull || x.Typ == Null || pat.Typ == Null {
		m.regs[op.P2] = Value{Typ: Null}
	} else {
		cs := m.likeCaseSensitive()
		var matched bool
		if lp := m.likePlanFor(pat); !hasEsc && x.Typ == Text && lp != nil && lp.ok {
			matched = lp.match(x.S, cs)
		} else if hasEsc {
			matched = likeMatchEscape(valueToText(pat), valueToText(x), esc, cs)
		} else {
			matched = likeMatch(valueToText(pat), valueToText(x), cs)
		}
		if op.P5&p5LikeNot != 0 {
			matched = !matched
		}
		m.regs[op.P2] = boolValue(matched)
	}
	return nil
}

// opGlob is OP_Glob: r[P2] = r[P1] [NOT] GLOB r[P3].
func (m *vdbe) opGlob(op *Instruction) {
	x, pat := m.regs[op.P1], m.regs[op.P3]
	if x.Typ == Null || pat.Typ == Null {
		m.regs[op.P2] = Value{Typ: Null}
	} else {
		matched := globMatch(valueToText(pat), valueToText(x))
		if op.P5&p5GlobNot != 0 {
			matched = !matched
		}
		m.regs[op.P2] = boolValue(matched)
	}
}
