package engine

// LIKE in the filtered count: "count(*) FROM t WHERE payload LIKE '%-7-payload'"
// matched every row through OpLike -- a column read into a Value, the opcode,
// a jump. Here the pattern is analysed once (likePlan) and each segment's TEXT
// cells are matched in its heap, with no row built. It accepts what the byte
// matcher accepts and a column that is a pure TEXT block; anything else
// declines to the loop, where OpLike answers.

// segPlanLike is "col [NOT] LIKE <literal or parameter>" with no ESCAPE.
type segPlanLike struct {
	col      int
	lit      Value
	paramIdx int // 1-based; 0 means lit
	not      bool
}

// segParseLikeGroup reads "Column cursor.col -> r; String8/Variable -> p;
// Like r, p -> dest; IfNot dest -> next".
func segParseLikeGroup(in []Instruction, pc, limit, cursor, nextAt int) (segPlanLike, int, bool) {
	var no segPlanLike
	if pc+3 >= limit {
		return no, 0, false
	}
	col, pat, like, test := in[pc], in[pc+1], in[pc+2], in[pc+3]
	esc, _ := like.P4.(int)
	if col.Op != OpColumn || col.P1 != cursor || like.Op != OpLike || like.P1 != col.P3 || like.P3 != pat.P2 || esc >= 0 ||
		test.Op != OpIfNot || test.P1 != like.P2 || test.P2 != nextAt || test.P3 != 1 {
		return no, 0, false
	}
	pl := segPlanLike{col: col.P2, not: like.P5&p5LikeNot != 0}
	switch pat.Op {
	case OpString8:
		s, ok := pat.P4.(string)
		if !ok {
			return no, 0, false
		}
		pl.lit = Value{Typ: Text, S: []byte(s)}
	case OpVariable:
		pl.paramIdx = pat.P1
	default:
		return no, 0, false
	}
	return pl, pc + 4, true
}

// segLike is a segPlanLike resolved for this run.
type segLike struct {
	col  int
	plan *likePlan
	cs   bool
	not  bool
}

// segLikes resolves each LIKE's pattern; false declines.
func (m *vdbe) segLikes(plan []segPlanLike) ([]segLike, bool) {
	out := make([]segLike, len(plan))
	for i, pl := range plan {
		v := pl.lit
		if pl.paramIdx != 0 {
			if pl.paramIdx < 1 || pl.paramIdx > len(m.params) {
				return nil, false
			}
			v = m.params[pl.paramIdx-1]
		}
		if v.Typ != Text {
			return nil, false // NULL, or a value OpLike would first render as text
		}
		lp := newLikePlan(v.S)
		if !lp.ok {
			return nil, false
		}
		out[i] = segLike{col: pl.col, plan: lp, cs: m.likeCaseSensitive(), not: pl.not}
	}
	return out, true
}
