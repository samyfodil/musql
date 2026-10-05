// This file holds the compound SELECT helpers -- "SELECT ... (UNION [ALL] |
// INTERSECT | EXCEPT) SELECT ..." -- used by the compound codegen
// (vdbe_compound_codegen.go): combining arms' rows pairwise, left to right
// (combineCompound), dedup and sort order, and resolving the compound's ORDER
// BY. OrderBy/Limit/Offset live on the whole statement, not an arm (see
// SelectStmt.Compound).
package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
)

// combineCompound folds the next arm's rows into the accumulated left-hand
// result via one compound operator, left to right as SQLite associates them.
// UNION ALL concatenates, keeping duplicates. UNION, INTERSECT and EXCEPT
// deduplicate with NULL-equal row equality (as DISTINCT and GROUP BY do),
// under each column's collation from compoundColumnCollations. nil collations
// means all BINARY.
func combineCompound(op CompoundOp, left, right [][]Value, collations []string, enc TextEncoding) ([][]Value, error) {
	switch op {
	case "UNION ALL":
		out := make([][]Value, 0, len(left)+len(right))
		out = append(out, left...)
		out = append(out, right...)
		return out, nil

	case "UNION":
		return sortCompoundRows(unionMerge(dedupRows(left, collations, enc), dedupRows(right, collations, enc), collations, enc), collations, enc), nil

	case "INTERSECT":
		var out [][]Value
		for _, l := range dedupRows(left, collations, enc) {
			if rowsContain(right, l, collations, enc) {
				out = append(out, l)
			}
		}
		return sortCompoundRows(out, collations, enc), nil

	case "EXCEPT":
		var out [][]Value
		for _, l := range dedupRows(left, collations, enc) {
			if !rowsContain(right, l, collations, enc) {
				out = append(out, l)
			}
		}
		return sortCompoundRows(out, collations, enc), nil

	default:
		return nil, fmt.Errorf("engine: unsupported compound operator %q", op)
	}
}

// sortCompoundRows orders a deduplicating compound's result by its whole row,
// as C does: UNION, INTERSECT and EXCEPT dedup through an ephemeral b-tree
// keyed on the row, so output comes in key order:
//
//	SELECT a FROM u UNION SELECT 9      -> 1 2 3 9   (insertion order: 3 1 2 9)
//	SELECT a FROM u INTERSECT SELECT a FROM u -> 1 2 3
//	SELECT 'b' UNION SELECT 'a'         -> a b
//
// UNION ALL stays in arm order. Without ORDER BY the order is unspecified but
// observable (LIMIT, a bare column under an aggregate -- with1.test's "WITH
// c(i) AS (VALUES(5) UNION SELECT 0) SELECT min(1)-i FROM c" is 1, not -4).
func sortCompoundRows(rows [][]Value, collations []string, enc TextEncoding) [][]Value {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		for k := range a {
			if k >= len(b) {
				break
			}
			if c := compareValuesCollatedEnc(a[k], b[k], atOrEmpty(collations, k), enc); c != 0 {
				return c < 0
			}
		}
		return false
	})
	return rows
}

// keysEqualCollated is keysEqual's collation-aware sibling: elementwise
// compareValuesCollatedEnc equality under collations[i] (still NULL-equal). A
// short or nil collations list means BINARY for the missing columns.
func keysEqualCollated(a, b []Value, collations []string, enc TextEncoding) bool {
	for i := range a {
		if compareValuesCollatedEnc(a[i], b[i], atOrEmpty(collations, i), enc) != 0 {
			return false
		}
	}
	return true
}

// dedupRows removes duplicate rows (keysEqualCollated row-equality, i.e.
// NULLs equal to each other and each column compared via its own
// collations[i]), preserving first-occurrence order -- the same contract as
// DISTINCT's own (BINARY-only) dedup in execSelect's row-mode path. rows is
// compacted in place (safe here: every caller passes a freshly built slice
// it no longer reads independently afterward), mirroring execSelect's own
// "kept := results[:0]" idiom.
func dedupRows(rows [][]Value, collations []string, enc TextEncoding) [][]Value {
	kept := rows[:0]
	for _, r := range rows {
		dup := false
		for _, k := range kept {
			if keysEqualCollated(k, r, collations, enc) {
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, r)
		}
	}
	return kept
}

// unionMerge folds newRows into accumulated (both already self-deduped, where
// dedupRows keeps the first occurrence within an arm) as UNION's cross-arm tie
// behaves when a collation makes two byte-different rows equal: the new row
// replaces the old one, so the rightmost arm's bytes win a cross-arm tie --
// the opposite of the within-arm rule. This is undocumented C behaviour; a
// three-arm tie is won by the last arm. Folding pairwise left to right
// reproduces that for any arm count. Unmatched rows are appended.
func unionMerge(accumulated, newRows [][]Value, collations []string, enc TextEncoding) [][]Value {
	out := accumulated
	for _, r := range newRows {
		replaced := false
		for i, k := range out {
			if keysEqualCollated(k, r, collations, enc) {
				out[i] = r
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, r)
		}
	}
	return out
}

// rowsContain reports whether rows contains a row equal to target under
// keysEqualCollated (NULL-equal, per-column collated) row-equality -- used
// by combineCompound's INTERSECT/EXCEPT paths as a set-membership test
// against the untouched operand (right), so it deliberately does not dedup
// or mutate rows. A recursive CTE's UNION dedup (rowKeySet, below) follows
// the same equality under the recursion's own per-column collations
// (recursiveCTECollations): a recursive CTE's body IS a compound SELECT, so
// its dedup follows exactly the same leftmost-arm-with-an-opinion rule.
func rowsContain(rows [][]Value, target []Value, collations []string, enc TextEncoding) bool {
	for _, r := range rows {
		if keysEqualCollated(r, target, collations, enc) {
			return true
		}
	}
	return false
}

// rowKeySet is rowsContain with a map: the same NULL-equal collated row
// equality (keysEqualCollated), keyed on collatedKeyBytes. Used where
// membership is tested per produced row against all rows so far -- a UNION
// recursive CTE's queue (recQueue), which was quadratic with a linear scan.
// It relies on collatedKeyBytes' contract, gated by
// TestRowKeySetMatchesKeysEqualCollated. INTERSECT/EXCEPT keep rowsContain.
type rowKeySet struct {
	seen       map[string]struct{}
	collations []string
	enc        TextEncoding

	// bytes is the key bytes held, plus a string header per key -- what a
	// recursive CTE's queue counts against its memory cap
	// (recQueue.residentBytes).
	bytes int64
}

// newRowKeySet creates an empty set comparing under collations (nil == every
// column BINARY) in database encoding enc.
func newRowKeySet(collations []string, enc TextEncoding) *rowKeySet {
	return &rowKeySet{seen: map[string]struct{}{}, collations: collations, enc: enc}
}

// add remembers row and reports whether it was NEWLY added -- false means an
// equal row (keysEqualCollated) was already present, in which case the set is
// unchanged, so the FIRST occurrence is the one that survives, matching the
// linear rowsContain-then-append idiom it replaces.
func (s *rowKeySet) add(row []Value) bool {
	k := string(collatedKeyBytes(row, s.collations, s.enc))
	if _, dup := s.seen[k]; dup {
		return false
	}
	s.seen[k] = struct{}{}
	s.bytes += int64(len(k)) + 16
	return true
}

// collatedKeyBytes canonically encodes a row so two rows encode identically iff
// keysEqualCollated calls them equal. It generalizes hashAggKeyBytes (which is
// this with no collations and UTF-8).
//
// Each value is a 1-byte class tag, an 8-byte big-endian length and the body,
// so no field can be misread as another. Int and an equal Float share a tag
// (floatAsCanonicalInt), as compareNumeric equates them; TEXT goes through
// collatedTextKey.
func collatedKeyBytes(vals []Value, collations []string, enc TextEncoding) []byte {
	return appendCollatedKeyBytes(make([]byte, 0, len(vals)*16), vals, collations, enc)
}

// appendCollatedKeyBytes is collatedKeyBytes writing into a caller-supplied
// buffer instead of a fresh one, so a caller that only ever uses the bytes as a
// lookup key can hand back the SAME buffer every row (hashAggStep). The bytes
// appended are identical either way -- collatedKeyBytes is now literally this
// function over a fresh slice -- so the encoding contract above is unchanged.
func appendCollatedKeyBytes(buf []byte, vals []Value, collations []string, enc TextEncoding) []byte {
	const (
		tagNull        = 0
		tagNumericInt  = 1 // Int, or an integer-valued Float (canonicalized to its int64)
		tagNumericReal = 2 // a Float with no exact int64 equivalent
		tagText        = 3
		tagBlob        = 4
	)
	// lenBuf is the length-prefix scratch space, kept deliberately SEPARATE
	// from any numeric encoding buffer below: appendField writes the
	// length prefix into lenBuf, never into the same backing array as the
	// body slice it was passed (a numeric body aliasing the length buffer
	// would get silently clobbered by the length write before being
	// appended -- exactly the bug this separation avoids).
	var lenBuf [8]byte
	appendField := func(tag byte, body []byte) {
		buf = append(buf, tag)
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(body)))
		buf = append(buf, lenBuf[:]...)
		buf = append(buf, body...)
	}
	for i, v := range vals {
		switch v.Typ {
		case Null:
			appendField(tagNull, nil)
		case Int:
			var numBuf [8]byte
			binary.BigEndian.PutUint64(numBuf[:], uint64(v.I))
			appendField(tagNumericInt, numBuf[:])
		case Float:
			var numBuf [8]byte
			if n, ok := floatAsCanonicalInt(v.F); ok {
				binary.BigEndian.PutUint64(numBuf[:], uint64(n))
				appendField(tagNumericInt, numBuf[:])
			} else {
				binary.BigEndian.PutUint64(numBuf[:], math.Float64bits(v.F))
				appendField(tagNumericReal, numBuf[:])
			}
		case Text:
			appendField(tagText, collatedTextKey(atOrEmpty(collations, i), v.S, enc))
		case Blob:
			appendField(tagBlob, v.S)
		default:
			appendField(tagNull, nil)
		}
	}
	return buf
}

// collatedTextKey canonicalizes one TEXT value so two values encode identically
// iff collatedTextCompareEnc calls them equal, mirroring its switch:
//
//   - NOCASE folds ASCII case and returns (nocaseCompare compares the UTF-8
//     bytes in every encoding);
//   - RTRIM strips trailing ASCII spaces and falls through;
//   - BINARY is memcmp -- over the encoded bytes in a UTF-16 database, which
//     utf16CompareUTF8 computes unit by unit from the UTF-8, so the key is that
//     unit sequence. Raw bytes would over-split: two different invalid UTF-8
//     sequences both decode to U+FFFD, and a non-BMP rune contributes only its
//     high surrogate (nextUTF16Unit).
func collatedTextKey(collation string, s []byte, enc TextEncoding) []byte {
	switch strings.ToUpper(collation) {
	case "NOCASE":
		out := make([]byte, len(s))
		for i, c := range s {
			out[i] = asciiLowerByte(c)
		}
		return out
	case "RTRIM":
		s = rtrimTrailingSpaces(s)
	}
	if !isUTF16(enc) {
		return s
	}
	out := make([]byte, 0, len(s))
	for len(s) > 0 {
		u, n := nextUTF16Unit(s)
		out = append(out, byte(u>>8), byte(u))
		s = s[n:]
	}
	return out
}

// resolveCompoundOrderIndex resolves one ORDER BY term of a compound to a
// 0-based result column, reproducing resolveCompoundOrderBy. A term is
// matched, in order, as
//
//	(1) a literal positive-integer ordinal; else
//	(2..N) walking the arms left to right, trying for each
//	    (a) resolveAsName: a bare reference to that arm's output names/aliases,
//	    (b) resolveOrderByTermToExprList: the term resolved in that arm's FROM
//	        scope and matched by column identity (or structural equality for a
//	        non-column term) against that arm's select list.
//	The first arm that resolves it wins; the matched position is the key.
//
// So "... UNION ALL SELECT a,b,c FROM t1 ORDER BY a,b,c" resolves against the
// right arm's names, and "ORDER BY t1.a" or a right-arm alias resolve too.
// Within an arm the alias match wins over an expression match (tkt2822-3.4).
//
// The term's collation is a separate question (resolveCompoundOrderCollation),
// keyed by the resolved position, independent of which arm matched.
//
// arms is the left-to-right list (resolveAllArmOutputs). If an arm did not
// resolve (arm.ok false), this declines after trying a bare output-name match
// against cols, rather than risk attributing a later arm's match.
func resolveCompoundOrderIndex(e Expr, cols []string, arms []armOutput, term int) (int, error) {
	// stripOrderCollate (query.go) lets any attached "COLLATE name" (including
	// a stack of them) pass through the ordinal/name/expression forms below
	// unchanged -- verified directly (see query.go's stripOrderCollate doc
	// comment) that C SQLite still recognizes every special form with a
	// COLLATE attached; resolveCompoundOrderCollation separately recovers the
	// collation from the ORIGINAL (unstripped) term.
	//
	// orderByOrdinal (query.go) also recognizes the signed forms ("+2"/
	// "-1") C SQLite treats as ordinal references too -- see its doc
	// comment.
	stripped := stripOrderCollate(e)
	if n, ok := orderByOrdinal(stripped); ok {
		if n < 1 || int(n) > len(cols) {
			return 0, semanticf("%s ORDER BY term out of range - should be between 1 and %d",
				sqliteOrdinalWord(term), len(cols))
		}
		return int(n) - 1, nil
	}
	ncols := len(cols)
	bareName, isBare := "", false
	if col, ok := stripped.(ColumnExpr); ok && col.Qualifier == "" {
		bareName, isBare = col.Name, true
	}
	for _, arm := range arms {
		if !arm.ok {
			// Can't reproduce SQLite's per-arm name resolution against an arm
			// we couldn't resolve. The one leftmost match still safe without a
			// scope is a bare name against cols (the actual result-column
			// names, == the leftmost arm's own output names); anything else is
			// declined rather than risk mis-attributing a later-arm match.
			if isBare {
				for i, c := range cols {
					if equalFoldName(c, bareName) {
						return i, nil
					}
				}
			}
			return 0, errCompoundOrderUnsupported
		}
		// (a) resolveAsName: bare name against this arm's own output names/
		// aliases (alias-first, so an alias beats an underlying-expression
		// match at a different position, exactly like SQLite).
		if isBare {
			for i := 0; i < ncols && i < len(arm.outCols); i++ {
				if equalFoldName(arm.outCols[i].name, bareName) {
					return i, nil
				}
			}
		}
		// (b) resolveOrderByTermToExprList: name-resolve the term against this
		// arm's FROM and match it against one of this arm's own select-list
		// expressions -- by resolved column IDENTITY for a column reference
		// (so a qualified "t.col" matches a bare "col" resolving to the same
		// physical column), else by structural equality.
		if idx, ok := matchArmSelectExpr(stripped, arm, ncols); ok {
			return idx, nil
		}
	}
	// A column-reference term that matched no arm is the term C rejects
	// (resolve.c:1699-1702, "%r ORDER BY term does not match any column in
	// the result set"). Only a column reference: a non-column expression C
	// might still match (its comparison is broader than this structural
	// test), so it keeps the decline.
	if _, isCol := stripped.(ColumnExpr); isCol {
		return 0, semanticf("engine: %s ORDER BY term does not match any column in the result set", sqliteOrdinalWord(term))
	}
	return 0, errCompoundOrderUnsupported
}

// errCompoundOrderUnsupported is the single decline resolveCompoundOrderIndex
// raises for an ORDER BY term it cannot resolve to a result-column position
// against any arm -- either a genuinely unsupported term shape or one real
// SQLite itself rejects ("Nth ORDER BY term does not match any column in the
// result set"), in which case the differential harness confirms the mutual
// rejection. Declining is always safe (a rejection is never a wrong answer).
var errCompoundOrderUnsupported = fmt.Errorf("engine: unsupported ORDER BY term on a compound SELECT (only a result-column position or name is supported)")

// matchArmSelectExpr matches term against arm's select-list expressions --
// resolveOrderByTermToExprList's half (the alias half is resolveAsName, done
// by the caller). A column reference is matched by identity
// (resolveColumnIndex in arm's scope) against output columns resolving to the
// same column, so "ORDER BY t1.a" and "ORDER BY a" both match output "a"; a
// name that does not resolve in this arm matches nothing. A non-column term
// falls back to structural equality ("ORDER BY CAST(b AS TEXT)"), which C is
// guaranteed to accept too.
func matchArmSelectExpr(term Expr, arm armOutput, ncols int) (int, bool) {
	scopes := arm.scopes()
	if ce, ok := term.(ColumnExpr); ok {
		id, ok := armColumnIdentity(scopes, ce)
		if !ok {
			return 0, false
		}
		for i := 0; i < ncols && i < len(arm.outCols); i++ {
			oce, ok := arm.outCols[i].expr.(ColumnExpr)
			if !ok {
				continue
			}
			if oid, ok := armColumnIdentity(scopes, oce); ok && oid == id {
				return i, true
			}
		}
		return 0, false
	}
	for i := 0; i < ncols && i < len(arm.outCols); i++ {
		if exprEqual(term, arm.outCols[i].expr) {
			return i, true
		}
	}
	return 0, false
}

// armColumnIdentity is resolveColumnIndex plus the one equivalence SQLite
// applies when matching a compound ORDER BY term against an arm's select list:
// a rowid reference and the table's INTEGER PRIMARY KEY are the same column.
// resolveColumnIndex keeps them apart (rowidIdentityBase-N vs the column's
// offset), which is right everywhere else. Over t1(a INTEGER PRIMARY KEY,b)
// and t2(c INTEGER PRIMARY KEY,d):
//
//	SELECT a FROM t1 UNION ALL SELECT c FROM t2 ORDER BY rowid    -> 1,2,3
//	SELECT b FROM t1 UNION ALL SELECT c FROM t2 ORDER BY rowid    -> 2,x,y
//	SELECT rowid FROM t1 UNION ALL SELECT c FROM t2 ORDER BY a    -> 1,2,3
//
// Where no arm holds its rowid, C still rejects and this still declines
// (where9.test).
func armColumnIdentity(scopes []tableScope, ce ColumnExpr) (int, bool) {
	ctx := &evalCtx{tables: scopes}
	_, idx, col, rowidTableIdx, err := resolveColumn(ctx, ce.Qualifier, ce.Name)
	if err != nil {
		return 0, false
	}
	if rowidTableIdx >= 0 {
		return rowidIdentityBase - rowidTableIdx, true
	}
	if col != nil && col.IsRowidAlias {
		// The INTEGER PRIMARY KEY column IS the rowid: hand back the identity a
		// pseudo-rowid reference to the same scope would get. The owning scope
		// is located by the flattened-offset range idx falls in, exactly as
		// columnRefNameParts (query.go) locates it; a miss (only reachable via
		// the RIGHT/FULL coalesce fallback's redirected offset) falls through
		// to the ordinary identity rather than guessing at a scope.
		for i := range ctx.tables {
			ts := &ctx.tables[i]
			if idx >= ts.offset && idx < ts.offset+len(ts.cols) {
				return rowidIdentityBase - i, true
			}
		}
	}
	return idx, true
}

// armOutput is one compound arm's select list, already expanded ("*"/"t.*"
// resolved into concrete per-column ColumnExpr items, exactly like an
// ordinary SELECT's own outCols -- see expandSelectList) against its own
// FROM scope, plus the evalCtx (schema-only: no row data) that scope
// resolves against. ok is false when the arm's FROM/select list couldn't be
// resolved at all -- left for the real execSelect call to report properly
// (see resolveArmOutputs); every reader of an armOutput must check it
// before indexing outCols.
type armOutput struct {
	ctx     *evalCtx
	outCols []outputColumn
	ok      bool
}

// scopes is the arm's schema-only FROM scope (one tableScope per FROM item),
// or nil for a FROM-less arm -- used by matchArmSelectExpr to resolve an
// ORDER BY term's column identity against this arm exactly as an ordinary
// column reference in this arm's own body would resolve.
func (a armOutput) scopes() []tableScope {
	if a.ctx == nil {
		return nil
	}
	return a.ctx.tables
}

// resolveAllArmOutputs builds every compound arm's armOutput in LEFT-TO-RIGHT
// order: stmt's own leftmost arm first (resolveArmOutputs on stmt, which reads
// only its Columns/From), then each stmt.Compound arm. resolveCompoundOrderIndex
// walks this slice to reproduce SQLite's leftmost-first ORDER BY term
// resolution; arms[0] is also the leftmost arm every collation/dedup rule
// consults (compoundColumnCollations/resolveCompoundOrderCollation).
func (p *ReadOnlyPager) resolveAllArmOutputs(stmt *SelectStmt) []armOutput {
	return p.resolveAllArmOutputsOuter(stmt, nil)
}

// resolveAllArmOutputsOuter is resolveAllArmOutputs with the ENCLOSING query's
// schema-only scope chain, so an arm's CORRELATED column reference resolves the
// same way the compiled arm itself resolves it (compileColumn walks outward
// through compiler.outer). Only the collation rule reads it -- an arm's own
// output expression that is a bare reference to an enclosing table's column
// carries that column's DECLARED collation, exactly as sqlite3ExprCollSeq
// reads any other TK_COLUMN's (c:151568's multiSelectCollSeq calls it on the
// arm expression whatever scope it resolved against). nil outer is the
// ordinary top-level case and behaves exactly as before.
func (p *ReadOnlyPager) resolveAllArmOutputsOuter(stmt *SelectStmt, outer *evalCtx) []armOutput {
	arms := make([]armOutput, 0, len(stmt.Compound)+1)
	arms = append(arms, p.resolveArmOutputsOuter(stmt, outer))
	for _, arm := range stmt.Compound {
		arms = append(arms, p.resolveArmOutputsOuter(arm.Stmt, outer))
	}
	return arms
}

// resolveArmOutputs builds sel's armOutput: a schema-only evalCtx from sel's
// own FROM, and sel's select list expanded through it via expandSelectList --
// the same star-expansion every ordinary SELECT's own execSelect applies,
// reused here so a "*"/"t.*" item in a compound arm is treated exactly like
// the concrete per-column reference it stands for.
func (p *ReadOnlyPager) resolveArmOutputs(sel *SelectStmt) armOutput {
	return p.resolveArmOutputsOuter(sel, nil)
}

// resolveArmOutputsOuter is resolveArmOutputs with an ENCLOSING schema-only
// scope chained beneath sel's own, so a CORRELATED column reference in sel's
// select list resolves (and so contributes its DECLARED collation) exactly as
// the compiled arm itself resolves it.
func (p *ReadOnlyPager) resolveArmOutputsOuter(sel *SelectStmt, outer *evalCtx) armOutput {
	var scopes []tableScope
	if len(sel.From) > 0 {
		jts, _, err := p.resolveFrom(sel.From, nil)
		if err != nil {
			// resolveFrom materializes a derived table to learn its columns, so a
			// body that fails only at run time would sink this arm's name
			// resolution, while C resolves names without running anything
			// (sqlite3ExpandSubquery, select.c:5885). Retry schema-only before
			// giving up (ReadOnlyPager.derivedSchemaOnly). misc1.test misc1-26.0
			// is the case: a run-time "datatype mismatch" OFFSET C never reaches.
			jts, err = p.resolveFromSchemaOnly(sel.From)
			if err != nil {
				return armOutput{}
			}
		}
		scopes = buildScopes(jts)
	}
	// Always a real ctx, and always carrying the pager: an arm's expression
	// may need a SCHEMA even with no FROM clause of its own, which is exactly
	// what a scalar subquery in the select list is (exprAffinity resolves its
	// single output column through derivedColumnInfos). Before this, only an
	// arm WITH a FROM clause got a ctx and none got a pager, so
	// compoundColumnAffinity read "(SELECT s FROM t)" in any arm as having no
	// affinity -- which then DEMOTED the whole column, the last 7 of the 400
	// two-arm pairs to disagree with the oracle. Handing out a ctx with an
	// empty scope list changes nothing for the other consumer
	// (topExprCollation resolves no column either way).
	ctx := &evalCtx{tables: scopes, outer: outer, pager: p}
	outCols, err := expandSelectList(sel.Columns, scopes, p.colNameMode())
	if err != nil {
		return armOutput{}
	}
	return armOutput{ctx: ctx, outCols: outCols, ok: true}
}

// compoundColumnCollations returns the collation combineCompound's dedup uses
// for each of n output columns, from the leftmost arm (arms[0]) only: a
// declared collation (a bare column reference, possibly through CAST/unary
// "+") or an explicit "expr COLLATE name" on its output governs; otherwise
// BINARY. A collation on any other arm has no effect: "SELECT a FROM t1 UNION
// SELECT a FROM t2" with only t2.a NOCASE keeps 'abc' and 'ABC'. An unresolved
// leftmost arm degrades to all BINARY (a defensive fallback; callers have
// already resolved it).
func compoundColumnCollations(arms []armOutput, n int) []string {
	result := make([]string, n)
	for i := range result {
		result[i] = compoundArmCollation(arms, i)
	}
	return result
}

// compoundArmCollation returns the collation result column idx carries: from
// the first arm, left to right, whose expression at idx has an opinion. Usually
// the leftmost, but "a||''" has no collation (a declared collation does not
// propagate through a concatenation), so a later arm's applies:
//
//	SELECT a||'' FROM tst UNION ALL SELECT b COLLATE nocase FROM tst ORDER BY 1
//	  -> A,a,B,b,C,c   (NOCASE, from the SECOND arm)
//	SELECT a      FROM tst UNION ALL SELECT b COLLATE nocase FROM tst ORDER BY 1
//	  -> A,B,C,a,b,c   (BINARY: the first arm's bare column HAS an opinion)
//
// The same rule drives UNION's dedup.
func compoundArmCollation(arms []armOutput, idx int) string {
	for _, arm := range arms {
		if !arm.ok || idx < 0 || idx >= len(arm.outCols) {
			continue
		}
		if name, ok := topExprCollation(arm.ctx, arm.outCols[idx].expr); ok {
			if !equalFoldName(name, "BINARY") {
				return name
			}
			// An explicit/declared BINARY is still an OPINION: it settles the
			// column and stops the search, which is why a first arm naming a
			// plain (BINARY) column beats a later arm's explicit NOCASE.
			return "BINARY"
		}
	}
	return "BINARY"
}

// resolveCompoundOrderCollation returns the collation ORDER BY term ot,
// resolved to position idx, sorts by. An explicit COLLATE on the term wins
// (topExprCollation with ctx=nil is safe: only the explicit search can fire on
// a term naming a result column). Otherwise the leftmost arm at idx decides,
// as for dedup.
func resolveCompoundOrderCollation(arms []armOutput, idx int, ot OrderTerm) string {
	if n, ok := topExprCollation(nil, ot.Expr); ok {
		return n
	}
	return compoundArmCollation(arms, idx)
}

// compoundOrderByCollateNameQuirk reports whether stmt hits a C quirk this
// package does not reproduce: an unaliased select item's column name is
// normally its source text ("a COLLATE nocase"), but when the compound has an
// ORDER BY term with its own explicit COLLATE (whatever column it targets) C
// names it "a" instead -- for every item shaped that way. Such statements
// decline rather than report a wrong column name. Without such an ORDER BY
// term this returns false.
func compoundOrderByCollateNameQuirk(stmt *SelectStmt, cols []string) bool {
	hasExplicitOrderCollate := false
	for _, ot := range stmt.OrderBy {
		if _, ok := topExprCollation(nil, ot.Expr); ok {
			hasExplicitOrderCollate = true
			break
		}
	}
	if !hasExplicitOrderCollate {
		return false
	}
	for _, c := range cols {
		if strings.Contains(strings.ToUpper(c), "COLLATE") {
			return true
		}
	}
	return false
}

// compoundHasNonUnionAllOp reports whether stmt's compound chain has any
// operator other than UNION ALL -- the trigger convertCompoundSelectToSubquery
// (select.c:5534) tests ("p->op!=TK_ALL && p->op!=TK_SELECT") before wrapping
// the compound in "SELECT * FROM (...)". A pure UNION ALL chain keeps its
// source-text column names, so compoundOrderByCollateNameQuirk's renaming only
// applies when this is true.
func compoundHasNonUnionAllOp(stmt *SelectStmt) bool {
	for _, arm := range stmt.Compound {
		if arm.Op != "UNION ALL" {
			return true
		}
	}
	return false
}
