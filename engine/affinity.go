// Type affinity: the declared affinity of an expression, and the conversions
// it forces.
//
// exprAffinity answers what SQLite's sqlite3ExprAffinity does (expr.c:45) --
// walking through TK_REGISTER via op2 so that a rewritten reference keeps its
// column identity, and returning zero for one that has none. comparisonAffinity
// then applies sqlite3CompareAffinity's rule (expr.c:355-372), including the
// arm that hands a comparison the OTHER operand's affinity when the
// first side has none at all.
package engine

// isNumericAffinity reports whether a is one of the three "numeric" family
// affinities (INTEGER, REAL, NUMERIC), which share identical comparison
// behavior per SQLite's affinity rules.
func isNumericAffinity(a affinity) bool {
	return a == affInteger || a == affReal || a == affNumeric
}

// exprAffinity returns e's type affinity for the purpose of the comparison
// rules (see comparisonAffinity): a bare column reference has its column's affinity;
// CAST(... AS type) has the affinity implied by type; every other
// expression (literals, function calls, arithmetic, ...) has no affinity.
func exprAffinity(ctx *evalCtx, e Expr) affinity {
	switch x := e.(type) {
	case ColumnExpr:
		if _, _, col, _, err := resolveColumn(ctx, x.Qualifier, x.Name); err == nil {
			return col.Aff
		}
		return affNone
	case CastExpr:
		switch x.Type {
		case "TEXT":
			return affText
		case "INTEGER":
			return affInteger
		case "REAL":
			return affReal
		case "NUMERIC":
			return affNumeric
		default: // BLOB
			return affNone
		}
	case groupKeyExpr:
		// A GROUP BY query's rewritten expression tree (sql_group.go):
		// carries the affinity the matched GROUP BY expression itself would
		// have had (its column's affinity, or none for a computed
		// expression), computed once at plan time -- so e.g. a HAVING clause
		// comparing a grouped INTEGER-affinity column against a TEXT literal
		// still gets SQLite's usual affinity-coercion treatment, exactly as
		// if the original column expression were being compared directly.
		return x.aff
	case groupBareColExpr:
		// Same reasoning as groupKeyExpr above: a bare column resolved via
		// the bare-column anchor extension is still, physically, a real
		// table column -- carry its own affinity so a comparison elsewhere
		// in the SAME select-list item (e.g. "b > 5") gets the usual
		// coercion treatment.
		return x.aff
	case affExpr:
		// A synthetic node standing in for a subquery result column (see
		// subquery_validate.go): report its precomputed affinity directly.
		return x.aff
	case LiteralExpr:
		// affNone for a literal written in SQL (sqlite3ExprAffinity's
		// "return pExpr->affExpr", which is 0 for one), and the substituted
		// COLUMN's own affinity for a materialized correlated reference --
		// the two land in different branches of sqlite3CompareAffinity, which
		// is the whole reason LiteralExpr carries the field.
		return x.aff
	case SubqueryExpr:
		// A scalar subquery carries the affinity of its single result column,
		// exactly like C SQLite's sqlite3ExprAffinity, which recurses into
		// the subquery's first result expression.
		if ci, ok := scalarSubqueryColumn(ctx, x.Stmt); ok {
			return ci.Aff
		}
		return affNone
	case CollateExpr:
		// An explicit "X COLLATE name" never changes X's own affinity --
		// C SQLite's sqlite3ExprAffinity explicitly skips a COLLATE
		// wrapper (sqlite3ExprSkipCollateAndLikely) before computing
		// affinity, and this is verified directly against C SQLite
		// (mattn/go-sqlite3): "intcol COLLATE NOCASE = '5'" still applies
		// intcol's own INTEGER affinity to '5' (coercing it to 5, matching
		// the COLLATE-free "intcol = '5'"), exactly as if the COLLATE
		// wrapper were not there. Without this case, a CollateExpr fell
		// through to the default (affNone) -- before compileExpr had a
		// CollateExpr case at all (vdbe_codegen.go), a comparison operand
		// could never actually BE a CollateExpr here, so this gap was
		// unreachable; now that compileExpr compiles one as a pass-through,
		// this must mirror that transparency for affinity too, or a
		// comparison like the one above would wrongly skip the numeric
		// coercion and compare unequal.
		return exprAffinity(ctx, x.X)
	default:
		return affNone
	}
}

// comparisonAffinity returns the single affinity a bytecode comparison opcode
// (OpEq/OpNe/OpLt/OpLe/OpGt/OpGe) carries in its P5 and applies to BOTH
// operands -- including the isMaterializedRef guard (whereB.test) and the
// "numeric family collapses to generic affNumeric, never affReal/affInteger"
// rule (affinity2.test). Applying it to both operands is idempotent on the one
// already carrying that storage class. It never coerces a NONE-affinity
// materialized column against a TEXT column, and never applies REAL affinity
// that would force an integer literal into a lossy float.
func comparisonAffinity(ctx *evalCtx, lexpr, rexpr Expr) affinity {
	la := exprAffinity(ctx, lexpr)
	ra := exprAffinity(ctx, rexpr)
	switch {
	case isNumericAffinity(la) && !isNumericAffinity(ra):
		return affNumeric
	case isNumericAffinity(ra) && !isNumericAffinity(la):
		return affNumeric
	case la == affText && ra == affNone && !isMaterializedRef(ctx, rexpr):
		return affText
	case ra == affText && la == affNone && !isMaterializedRef(ctx, lexpr):
		return affText
	}
	return affNone
}

// applyAffinityToValue converts v the way storing it into a column of
// affinity aff would. This mirrors C SQLite's applyAffinity/
// vdbeMemRenderNum closely enough to
// matter for two DIFFERENT reasons depending on aff:
//
//   - TEXT affinity NEVER changes the value's storage class -- it just
//     stringifies whatever numeric representation is already there
//     (verified directly: even a value SQLite would otherwise fold to
//     INTEGER for NUMERIC/INTEGER/REAL affinity stringifies via its
//     original REAL formatting for TEXT affinity, e.g. as
//     "-9.223372036854776e+18" rather than "-9223372036854775808" -- see
//     _applyAffinity's SQLITE_AFF_TEXT branch, which calls
//     sqlite3VdbeMemStringify on the UNCHANGED Mem). formatFloatText
//     already reproduces FpDecode's own zero-sign quirk (a value that is
//     exactly zero always renders "+", regardless of sign bit), so no
//     extra folding is needed here at all.
//   - NUMERIC/INTEGER/REAL affinity parse a TEXT value that is, in its
//     entirety (aside from surrounding whitespace), a well-formed number
//     (anything that doesn't qualify is returned unconverted); an ALREADY
//     Int/Float value additionally gets the "is this integer-valued REAL
//     also expressible as INTEGER" fold-to-Int treatment intFoldWide
//     (this is C SQLite's actual per-affinity behavior too: NUMERIC,
//     INTEGER, and REAL all funnel an already-numeric Mem through the SAME
//     sqlite3VdbeIntegerAffinity check in _applyAffinity's dispatcher --
//     REAL affinity keeps the RESULT typed as real even when it folds, an
//     "invisible" storage optimization, while NUMERIC/INTEGER make the fold
//     visible via typeof()). A TEXT-sourced value that parsed as a Float
//     gets the narrower intFoldNarrow bound instead, matching
//     applyNumericAffinity's own "-alsoAnInt" check.
func applyAffinityToValue(v Value, aff affinity) Value {
	if affinityIsIdentity(v.Typ, aff) {
		return v
	}
	return applyAffinitySlow(v, aff)
}

// applyAffinitySlow is applyAffinityToValue without the identity shortcut --
// the rule itself, which affinityIsIdentity's claims are tested against.
func applyAffinitySlow(v Value, aff affinity) Value {
	switch aff {
	case affText:
		switch v.Typ {
		case Int, Float:
			return Value{Typ: Text, S: []byte(valueToText(v))}
		}
		return v
	case affNumeric, affInteger, affReal:
		switch v.Typ {
		case Text:
			if isF, i, f, ok := parseFullNumeric(string(v.S)); ok {
				if isF {
					return foldAlreadyNumeric(Value{Typ: Float, F: f}, aff, intFoldNarrow)
				}
				return foldAlreadyNumeric(Value{Typ: Int, I: i}, aff, intFoldNarrow)
			}
			return v
		case Int, Float:
			return foldAlreadyNumeric(v, aff, intFoldWide)
		}
		return v
	default:
		return v
	}
}

// affinityIsIdentity reports whether applyAffinityToValue(v, aff) is provably
// v itself, given only v's type: an Int under NUMERIC or INTEGER affinity
// reaches foldAlreadyNumeric's Int case, which returns it untouched for every
// affinity except REAL. Kept tiny and typ-only (rather than taking the whole
// Value) so it inlines and costs no 48-byte Value copy -- the point is to let a
// per-row caller skip the non-inlinable applyAffinityToValue call entirely.
// See compareOp (vdbe.go), which runs this twice for every row of a filtered
// scan.
//
// NULL and BLOB are identities under every affinity (no arm above touches
// them), and TEXT under TEXT affinity -- which with the Int case covers an
// INTEGER or TEXT value bound to a column of its own type, the common INSERT:
// OpAffinity runs this once per column per row.
func affinityIsIdentity(typ ValueType, aff affinity) bool {
	switch typ {
	case Int:
		return aff == affNumeric || aff == affInteger
	case Text:
		return aff == affText
	case Null, Blob:
		return true
	}
	return false
}

// foldAlreadyNumeric applies NUMERIC/INTEGER/REAL affinity's storage rule to
// v (already Int or Float, never TEXT -- see applyAffinityToValue's two
// callers, which pick foldFn per how v became numeric in the first place):
// a Float that qualifies per foldFn is folded to that integer -- visibly,
// for NUMERIC/INTEGER affinity (typeof() reports 'integer'); invisibly
// (still typeof() 'real', but with the int64-reconstructed float64 value --
// which for -0.0 has lost its sign, exactly matching C SQLite) for REAL
// affinity. Conversely, an Int value is forced into floating-point
// representation for REAL affinity specifically (SQLite's own documented
// rule: "a column with REAL affinity... forces integer values into
// floating point representation", applied via a separate OP_RealAffinity
// step in C SQLite's codegen rather than through this same fold, but
// with an identical net effect).
func foldAlreadyNumeric(v Value, aff affinity, foldFn func(float64) (int64, bool)) Value {
	switch v.Typ {
	case Float:
		if i, ok := foldFn(v.F); ok {
			if aff == affNumeric || aff == affInteger {
				return Value{Typ: Int, I: i}
			}
			return Value{Typ: Float, F: float64(i)} // affReal: stays real, sign/precision re-derived from the integer
		}
		return v
	case Int:
		if aff == affReal {
			return Value{Typ: Float, F: float64(v.I)}
		}
		return v
	default:
		return v
	}
}
