// This file implements fts4's "languageid=" module option: separate full-text
// indexes, one per language ID, with each row carrying the language ID.
//
// Key rules:
// - The %_content column is named "langid", not the declared option name.
// - Languages share the %_segdir/%_segments pair, partitioned by level.
// - Level calculation: level = ((langid * nIndex) + iIndex) * 1024 + iLevel.
// - %_stat stays global across languages; %_docsize is unchanged.
// - The stored langid is sqlite3_value_int (32-bit); negative values are rejected.
// - A langid change flushes pending segments (same point as a backwards docid).
// langid writes MORE THAN ONE segment: verified,
// "VALUES('alpha','beta',1),('gamma','delta',0)" on a one-index table leaves
// a level-0 segment holding gamma/delta and a level-1024 one holding
// alpha/beta. An UPDATE that changes a row's langid is the same shape (the
// delete half goes to the OLD language -- langidFromSelect re-reads it from
// %_content, fts3_write.c:1102 -- the insert half to the NEW one,
// fts3_write.c:5816): verified, it leaves delete markers at the old langid's
// levels AND a fresh segment at the new one's.
//
// Both INSERT (vtab_fts3.go's own per-row midFlushed loop, keyed on langid
// alone since an INSERT's docids are already required ascending across the
// whole statement) and UPDATE (fts3_txn.go's fts3MidFlush, generalized to
// carry a langid field alongside its docid one -- fts3_write.go's
// noteMutationOp/updateFts3) reproduce this by sealing whatever pending terms
// have accumulated so far into their own segment the moment a transition
// fires, and resuming fresh under the new language. Only DELETE still
// declines a statement whose rows do not all share one langid
// (deleteFromFts3): a DELETE's rows are already required strictly ascending
// with no per-operation split of its own, so there is no existing mid-flush
// seam to generalize the way UPDATE's delete-then-insert pair provided one.
//
// Between STATEMENTS the flush point is reproduced rather than declined: a
// transaction's accumulating segment (fts3_txn.go) is sealed when the next
// statement's langid differs, which is exactly what iPrevLangid does.
//
// # The query-side trap: no constraint means language 0 ONLY
//
// fts3BestIndexMethod picks up an EQUALITY constraint on the langid column and
// fts3FilterMethod reads it with "pCsr->iLangid = 0; if( pLangid )
// pCsr->iLangid = sqlite3_value_int(pLangid)". So a MATCH with no langid
// constraint searches LANGUAGE 0 and nothing else -- "MATCH 'gamma'" finds
// nothing for a langid-5 row while "MATCH 'gamma' AND lid=5" finds it. Both
// verified.
//
// The constraint is NOT omitted (fts3BestIndexMethod sets argvIndex but leaves
// aConstraintUsage[].omit at 0), so the outer query still applies it as an
// ordinary filter over the rows the module returned. That is why a NON-equality
// constraint answers nothing: "MATCH 'bravo' AND lid>0" searches language 0,
// finds no 'bravo' there, and returns no row -- verified. This engine
// reproduces the equality case and DECLINES every other reference to the langid
// column from a WHERE/ON clause that also holds a MATCH, rather than guess
// which constraint C SQLite's planner would have picked up.
//
// fts4aux reads language 0's term index (levels 0..1023) ABSENT any
// constraint on its own hidden languageid column -- verified, a langid-1
// row's terms are simply absent from an unconstrained "SELECT * FROM terms".
// An EQUALITY constraint on that column ("WHERE languageid=1") picks the
// OTHER language's term index instead, the same "iCol==4, op==EQ" case
// fts3auxBestIndexMethod carries alongside its own term constraints
// (fts3_aux.c:184-186) -- see vtab_fts3aux.go's BestIndex/fts3AuxRows.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fts3LangidValue is sqlite3_value_int applied to an INSERT's langid slot: a
// 32-bit truncation, with NULL and any non-numeric text reading 0 (see this
// file's comment). ok is false for a NEGATIVE result, which C fts3 refuses
// with SQLITE_CONSTRAINT before writing anything.
func fts3LangidValue(v Value) (langid int64, ok bool) {
	n := int64(int32(valueToInt64Trunc(v)))
	if n < 0 {
		return 0, false
	}
	return n, true
}

// fts3LevelBase is getAbsoluteLevel(p, langid, iIndex, 0): the first %_segdir
// level belonging to index iIndex of language langid.
func fts3LevelBase(langid int64, nIndex, iIndex int) int64 {
	return (langid*int64(nIndex) + int64(iIndex)) * fts3SegdirMaxLevel
}

// fts3SegdirLangids is SQL_SELECT_ALL_LANGID ("SELECT ?1 UNION SELECT level /
// (1024 * ?2) FROM %_segdir", bound with iPrevLangid and nIndex): every
// language 'optimize' and 'integrity-check' iterate over, ascending. Language 0
// is always in the set because a fresh connection's iPrevLangid is 0.
//
// A level a merge could never have written (a negative one, from a hand-built
// shadow table) is left out: C fts3's integer division would place it in a
// negative language, and getAbsoluteLevel asserts langid >= 0.
func fts3SegdirLangids(rows []fts3SegdirRow, nIndex int) []int64 {
	seen := map[int64]bool{0: true}
	for _, r := range rows {
		if r.level < 0 {
			continue
		}
		seen[r.level/(fts3SegdirMaxLevel*int64(nIndex))] = true
	}
	out := make([]int64, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// fts3LangidTargetSlot is the INSERT target index of the langid column, which
// insertIntoFts3 lays out one past the command-channel slot.
func fts3LangidTargetSlot(nCol int) int { return nCol + 2 }

// fts3ResolveMatchLangid decides which language a MATCH searches, from the
// statement's top-level WHERE conjuncts.
//
// It returns an error rather than a language whenever the langid column is
// mentioned in a way this engine cannot map onto fts3BestIndexMethod's own
// choice: only "<langidcol> = <integer literal>" as a top-level AND-conjunct is
// an equality constraint the planner hands to the module, and guessing wrong
// would search the wrong index -- a wrong answer, not a missing feature. The
// LAST such conjunct wins, mirroring fts3BestIndexMethod's own reassignment of
// iLangidCons.
func fts3ResolveMatchLangid(ms fts3MatchScope, conjuncts []Expr, elsewhere []Expr) (int64, error) {
	if ms.langidCol == "" {
		return 0, nil
	}
	langid := int64(0)
	for _, cj := range conjuncts {
		if bin, ok := cj.(BinaryExpr); ok && bin.Op == "=" {
			if n, ok := fts3LangidEquality(ms, bin); ok {
				langid = n
				continue
			}
		}
		if fts3ExprMentionsLangid(ms, cj) {
			return 0, fmt.Errorf("%w: a MATCH against an fts4 table whose %q language-id column is constrained by anything but \"%s = <integer>\"",
				errVDBEUnsupported, ms.langidCol, ms.langidCol)
		}
	}
	for _, e := range elsewhere {
		if fts3ExprMentionsLangid(ms, e) {
			return 0, fmt.Errorf("%w: a MATCH against an fts4 table whose %q language-id column is constrained outside the WHERE clause",
				errVDBEUnsupported, ms.langidCol)
		}
	}
	return langid, nil
}

// fts3LangidEquality reports whether bin is "<ms's langid column> = <integer
// literal>" (either way round) and, if so, the language it names.
func fts3LangidEquality(ms fts3MatchScope, bin BinaryExpr) (int64, bool) {
	for _, pair := range [2][2]Expr{{bin.L, bin.R}, {bin.R, bin.L}} {
		ce, isCol := pair[0].(ColumnExpr)
		if !isCol || !fts3IsLangidRef(ms, ce) {
			continue
		}
		lit, isLit := pair[1].(LiteralExpr)
		if !isLit || lit.Val.Typ != Int || lit.Val.I < 0 {
			continue
		}
		return lit.Val.I, true
	}
	return 0, false
}

// fts3ExprMentionsLangid reports whether e references ms's langid column
// anywhere. It DEFAULT-DENIES, exactly as fts3CountMatchExpr does: an
// expression node this walker does not know is reported as a mention, so the
// statement declines rather than search the wrong language.
func fts3ExprMentionsLangid(ms fts3MatchScope, e Expr) bool {
	any := func(es ...Expr) bool {
		for _, sub := range es {
			if fts3ExprMentionsLangid(ms, sub) {
				return true
			}
		}
		return false
	}
	switch x := e.(type) {
	case nil:
		return false
	case LiteralExpr, ParamExpr, RaiseExpr:
		return false
	case ColumnExpr:
		return fts3IsLangidRef(ms, x)
	case MatchExpr:
		return any(x.X, x.Pattern)
	case UnaryExpr:
		return any(x.X)
	case BinaryExpr:
		return any(x.L, x.R)
	case IsNullExpr:
		return any(x.X)
	case CollateExpr:
		return any(x.X)
	case CastExpr:
		return any(x.X)
	case BetweenExpr:
		return any(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return any(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return any(x.X, x.Pattern)
	case InExpr:
		return any(append([]Expr{x.X}, x.List...)...)
	case FuncExpr:
		if x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 {
			return true
		}
		return any(x.Args...)
	case CaseExpr:
		es := []Expr{x.Base, x.Else}
		for _, w := range x.Whens {
			es = append(es, w.When, w.Then)
		}
		return any(es...)
	case RowExpr:
		return any(x.Elems...)
	case SubqueryExpr, ExistsExpr:
		// A correlated subquery could constrain the langid column from
		// outside anything this walker can classify; default-deny.
		return true
	}
	return true
}

// fts3IsLangidRef reports whether ce names ms's langid column, qualified with
// the MATCH target's own name or not at all.
func fts3IsLangidRef(ms fts3MatchScope, ce ColumnExpr) bool {
	if !strings.EqualFold(ce.Name, ms.langidCol) {
		return false
	}
	if ce.Qualifier == "" {
		return true
	}
	if ms.scope == nil {
		return false
	}
	return strings.EqualFold(ce.Qualifier, ms.scope.name)
}

// fts3LangidContext splits a SELECT into the two lists fts3ResolveMatchLangid
// needs: stmt.Where's top-level AND-conjuncts (where an equality constraint the
// planner can hand the module may appear) and every OTHER expression that can
// constrain a row (a JOIN's ON clause, and HAVING). The select list, GROUP BY
// and ORDER BY are deliberately absent: mentioning the langid column there
// selects or sorts by it, it does not choose an index.
func fts3LangidContext(stmt *SelectStmt) (conjuncts, elsewhere []Expr) {
	conjuncts = splitTopLevelAnd(stmt.Where)
	for _, it := range stmt.From {
		if it.On != nil {
			elsewhere = append(elsewhere, it.On)
		}
	}
	if stmt.Having != nil {
		elsewhere = append(elsewhere, stmt.Having)
	}
	return conjuncts, elsewhere
}

// fts3WithLangid resolves which language ms's MATCH searches from this
// compile's statement, and returns ms carrying it. A table without
// "languageid=" is returned unchanged.
func (c *compiler) fts3WithLangid(ms fts3MatchScope) (fts3MatchScope, error) {
	if ms.langidCol == "" {
		return ms, nil
	}
	langid, err := fts3ResolveMatchLangid(ms, c.fts3Conjuncts, c.fts3Elsewhere)
	if err != nil {
		return ms, err
	}
	ms.langid = langid
	return ms, nil
}
