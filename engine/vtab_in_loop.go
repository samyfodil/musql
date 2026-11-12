package engine

import (
	"fmt"
	"slices"
)

// A virtual table constrained by an IN it consumes is filtered once for every
// distinct value of the IN, as C's IN loop around OP_VFilter does (codeINTerm,
// wherecode.c:668): the values of an ephemeral index over the right-hand side,
// keyed with the IN's affinity and compared as a record under its collation,
// in ascending order, NULLs skipped. The module sees each as the constraint's one value and its
// rows are concatenated with no test of the IN between, since the module
// consumed it.
//
// The constraint reaches buildVtabConstraints as a call of vtabInLoopFn in the
// item's tvfWhere: its column, the key's affinity and collation, then the IN's
// list, or one SubqueryExpr for an uncorrelated IN (SELECT ...).
const vtabInLoopFn = "\x00vtab_in_loop"

// vtabInLoop is a consumed IN: argv[arg] takes each of vals in turn.
type vtabInLoop struct {
	arg  int
	vals []Value
}

// vtabInLoopCall is "col IN (rhs)" as buildVtabConstraints reads it.
func vtabInLoopCall(col int, aff affinity, coll string, list []Expr, sub *SelectStmt) FuncExpr {
	args := []Expr{LiteralExpr{Val: Value{Typ: Int, I: int64(col)}}, LiteralExpr{Val: Value{Typ: Int, I: int64(aff)}}, LiteralExpr{Val: Value{Typ: Text, S: []byte(coll)}}}
	if sub != nil {
		args = append(args, SubqueryExpr{Stmt: sub})
	} else {
		args = append(args, list...)
	}
	return FuncExpr{Name: vtabInLoopFn, Args: args}
}

// vtabInLoopValues is the column and the values call f loops over.
//
// A value that reads an enclosing query's row is the one corr holds for this
// conjunct (conj), computed where that row is current (enclosingRowTerms).
func (p *ReadOnlyPager) vtabInLoopValues(f FuncExpr, params []Value, outer *evalCtx, trig *trigCompileCtx, from *vdbe, corr []vtabCorrValue, conj int) (int, []Value, error) {
	col, aff, coll := int(f.Args[0].(LiteralExpr).Val.I), affinity(f.Args[1].(LiteralExpr).Val.I), string(f.Args[2].(LiteralExpr).Val.S)
	var vals []Value
	if s, isSub := f.Args[3].(SubqueryExpr); isSub && len(f.Args) == 4 {
		_, rows, err := p.execSelect(s.Stmt, nil, params, "")
		if err != nil {
			return 0, nil, err
		}
		for _, r := range rows {
			if len(r) != 1 {
				return 0, nil, fmt.Errorf("%w: an IN subquery of %d columns", errVDBEUnsupported, len(r))
			}
			vals = append(vals, r[0])
		}
	} else {
	values:
		for k, e := range f.Args[3:] {
			for _, cv := range corr {
				if cv.arg == 0 && cv.conj == conj && cv.elem == k+1 {
					vals = append(vals, cv.v)
					continue values
				}
			}
			v, err := p.foldVtabInputValue(e, params, outer, trig, from)
			if err != nil {
				return 0, nil, err
			}
			vals = append(vals, v)
		}
	}
	out := vals[:0]
	for _, v := range vals {
		if v.Typ != Null {
			out = append(out, applyAffinityToValue(v, aff))
		}
	}
	enc := p.encoding()
	cmp := func(a, b Value) int { return compareValuesCollatedEnc(a, b, coll, enc) }
	// A key equal to one already in the index replaces it.
	slices.SortStableFunc(out, cmp)
	keep := out[:0]
	for _, v := range out {
		if n := len(keep); n > 0 && cmp(keep[n-1], v) == 0 {
			keep[n-1] = v
			continue
		}
		keep = append(keep, v)
	}
	return col, keep, nil
}

// vtabInLoopCursor is a module's cursor filtered once for every combination of
// its loops' values, the first loop outermost, as C nests the IN loops in the
// order of the constraints' argv slots.
type vtabInLoopCursor struct {
	VtabCursor
	loops  []vtabInLoop
	at     []int
	idxNum int
	idxStr string
	argv   []Value
	done   bool
}

func newVtabInLoopCursor(cur VtabCursor, loops []vtabInLoop) VtabCursor {
	if len(loops) == 0 {
		return cur
	}
	return &vtabInLoopCursor{VtabCursor: cur, loops: loops, at: make([]int, len(loops))}
}

func (c *vtabInLoopCursor) Filter(idxNum int, idxStr string, argv []Value) error {
	c.idxNum, c.idxStr, c.argv = idxNum, idxStr, slices.Clone(argv)
	clear(c.at)
	c.done = slices.ContainsFunc(c.loops, func(l vtabInLoop) bool { return len(l.vals) == 0 })
	if c.done {
		return nil
	}
	if err := c.filter(); err != nil {
		return err
	}
	return c.settle()
}

// filter filters the module with the loops' current values.
func (c *vtabInLoopCursor) filter() error {
	for k, l := range c.loops {
		c.argv[l.arg] = l.vals[c.at[k]]
	}
	return c.VtabCursor.Filter(c.idxNum, c.idxStr, c.argv)
}

// settle moves on through the loops' values while the module has no row.
func (c *vtabInLoopCursor) settle() error {
	for c.VtabCursor.Eof() {
		k := len(c.at) - 1
		for ; k >= 0; k-- {
			if c.at[k]++; c.at[k] < len(c.loops[k].vals) {
				break
			}
			c.at[k] = 0
		}
		if k < 0 {
			c.done = true
			return nil
		}
		if err := c.filter(); err != nil {
			return err
		}
	}
	return nil
}

func (c *vtabInLoopCursor) Next() error {
	if c.done {
		return nil
	}
	if err := c.VtabCursor.Next(); err != nil {
		return err
	}
	return c.settle()
}

func (c *vtabInLoopCursor) Eof() bool { return c.done }
