// An fts5 MATCH beginning with "*" is a special query, not full-text search.
// Directives: "*reads" and "*id" return one row (rowid=0, columns=NULL) with
// the table's hidden column holding the special value.
//
//	SELECT rowid,x1    FROM x1 WHERE x1 MATCH '*'        -> error, unknown special query:
//
// -- so it fires on ANY column's MATCH, is case-insensitive in the directive,
// and needs the "*" as the pattern's very first byte.
//
// NEITHER value is a quantity two independent implementations can agree on:
//
//   - "*reads" is Fts5Index.nRead (fts5_index.c:373), "Total number of blocks
//     read", incremented once per %_data block fetched through sqlite3_blob_read
//     (fts5DataRead, fts5_index.c:904) and never reset -- a cumulative count of
//     C's own segment-traversal I/O over the connection's life. Measured on the
//     oracle: 0 after a bare CREATE, 1 after one "pgsz" config write, 4 after two
//     one-row INSERTs, and 7 after one further MATCH over those two rows. It is
//     undocumented (it appears nowhere in ext/fts5/fts5.h) and exists for fts5's
//     own test suite to assert I/O behaviour. This engine does not read postings
//     block-by-block at all, so it has no corresponding number to report and may
//     not invent one.
//   - "*id" is Fts5Cursor.iCsrId, handed out by "pCsr->iCsrId = ++pGlobal->iNextId"
//     (fts5_main.c:801) -- a per-CONNECTION counter over every fts5 cursor opened
//     so far, whatever table it was on. Measured incrementing 1, 2, 3 across three
//     identical statements on one connection. It is useful only as the first
//     argument to an auxiliary function, and depends on the answering engine's
//     cursor-open schedule rather than on anything the SQL says.
//
// So these are declined in the same category, and with the same phrase, as an
// fts5 table's segment layout (fts5SegmentShadowQueryGuard, fts5_shadow.go) and
// the build's own compile-option list: not a gap this engine could close, but a
// value that is the answering build's own instrumentation. An UNRECOGNISED
// directive is left alone -- C SQLite errors on it too, so both engines
// already agree by rejecting it.
package engine

import (
	"fmt"
	"strings"
)

// fts5SpecialDirective returns the directive word of a special-query pattern,
// and reports whether pat is one at all. It is fts5FilterMethod's "zText[0]=='*'"
// test followed by fts5SpecialMatch's own scan, verbatim: the "*" must be the
// first byte, spaces after it are skipped, and the word runs to the next space.
func fts5SpecialDirective(pat string) (string, bool) {
	if !strings.HasPrefix(pat, "*") {
		return "", false
	}
	z := strings.TrimLeft(pat[1:], " ")
	if i := strings.IndexByte(z, ' '); i >= 0 {
		z = z[:i]
	}
	return z, true
}

// fts5SpecialQueryGuard declines a statement whose fts5 MATCH asks for one of
// the two internal parameters above. It runs from execSelect (query.go) AFTER
// the table-valued and "=" rewrites, so "FROM x1('*reads')" and
// "WHERE x1 = '*reads'" are already the MatchExpr this looks for.
func (p *ReadOnlyPager) fts5SpecialQueryGuard(stmt *SelectStmt) error {
	if p == nil || stmt == nil {
		return nil
	}
	// Cheap syntactic pre-filter: the schema is only consulted for a statement
	// that actually spells a "*"-leading MATCH pattern.
	target, dir, found := fts5SpecialInConjuncts(stmt.Where)
	for i := 0; !found && i < len(stmt.From); i++ {
		target, dir, found = fts5SpecialInConjuncts(stmt.From[i].On)
	}
	if !found || !p.fts5SpecialTargetsFts5(stmt, target) {
		return nil
	}
	switch strings.ToLower(dir) {
	case "reads":
		return fmt.Errorf("engine: unsupported: fts5's \"MATCH '*reads'\" reports the answering build's OWN count of %%_data blocks read (Fts5Index.nRead, fts5_index.c:373, incremented in fts5DataRead at fts5_index.c:904 and never reset), which is not reproducible against C SQLite: this engine does not traverse postings block by block, so it has no such number and will not invent one")
	case "id":
		return fmt.Errorf("engine: unsupported: fts5's \"MATCH '*id'\" reports the answering connection's OWN fts5 cursor counter (Fts5Cursor.iCsrId, assigned by \"++pGlobal->iNextId\" at fts5_main.c:801), which is not reproducible against C SQLite: it counts cursors this engine opens on its own schedule rather than anything the SQL states")
	}
	return nil
}

// fts5SpecialTargetsFts5 reports whether the MATCH's left operand names an fts5
// table in stmt's FROM. Only fts5 has special queries -- fts3/fts4's MATCH has
// no "*" form at all -- so a pattern that merely LOOKS like one must be left to
// decline for its own reason rather than claimed here.
//
// Three shapes are decided, and nothing else:
//
//   - a bare name equal to an fts5 FROM item's TABLE name: that is the
//     table-named hidden column, and fts5 forbids a column sharing its table's
//     name (fts5RewriteEqMatch makes the same argument);
//   - a qualifier equal to an fts5 FROM item's alias or table name;
//   - a bare name with exactly ONE FROM item, which is that fts5 table's -- the
//     "x MATCH '*reads'" form, which reaches C's check too since it precedes
//     any column handling. With a second FROM item the name could be its, so
//     this is not decided.
func (p *ReadOnlyPager) fts5SpecialTargetsFts5(stmt *SelectStmt, target ColumnExpr) bool {
	if target.Schema != "" {
		return false
	}
	for _, it := range stmt.From {
		if it.Subquery != nil || it.Table == "" || !p.isFts5Table(it.Table) {
			continue
		}
		if target.Qualifier != "" {
			if equalFoldName(target.Qualifier, it.Alias) || equalFoldName(target.Qualifier, it.Table) {
				return true
			}
			continue
		}
		// "rank" is the OTHER hidden column fts5 declares (fts5_config.c:765),
		// and a MATCH against it is not the special-query dispatch: probed
		// against the oracle, "WHERE rank MATCH '*reads'" answers rows rather
		// than reporting nRead. fts5RewriteEqMatch excludes it from the "="
		// rewrite for the same kind of reason.
		if equalFoldName(target.Name, "rank") {
			continue
		}
		if equalFoldName(target.Name, it.Table) || len(stmt.From) == 1 {
			return true
		}
	}
	return false
}

// fts5SpecialInConjuncts finds a special-query MATCH among e's top-level AND
// conjuncts, returning its left operand and the directive. It deliberately does
// not descend into OR or NOT, matching fts5EqMatchInConjuncts
// (fts5_tablefunc.go).
//
// That is NARROWER than C, which was probed rather than assumed: SQLite's
// OR-optimization splits "x1 MATCH '*reads' OR rowid=1" into two index scans
// and hands fts5 the MATCH after all, so the oracle answers it (2 rows) where
// this scan ignores it. The narrowness is safe in the only direction that
// matters -- an unclaimed special query still DECLINES, for its own reason
// ("fts5: expected a search term") rather than with the non-reproducible
// phrase -- while widening it would risk claiming a query that is not one. A
// "NOT MATCH" is not offered as a constraint at all.
func fts5SpecialInConjuncts(e Expr) (ColumnExpr, string, bool) {
	switch x := e.(type) {
	case BinaryExpr:
		if !strings.EqualFold(x.Op, "AND") {
			return ColumnExpr{}, "", false
		}
		if c, d, ok := fts5SpecialInConjuncts(x.L); ok {
			return c, d, true
		}
		return fts5SpecialInConjuncts(x.R)
	case MatchExpr:
		// "NOT MATCH" is not offered to xBestIndex as a MATCH constraint, so
		// it never reaches the special-query dispatch.
		if x.Not {
			return ColumnExpr{}, "", false
		}
		ce, isCol := x.X.(ColumnExpr)
		lit, isLit := x.Pattern.(LiteralExpr)
		if !isCol || !isLit || lit.Val.Typ != Text {
			return ColumnExpr{}, "", false
		}
		d, ok := fts5SpecialDirective(string(lit.Val.S))
		return ce, d, ok
	}
	return ColumnExpr{}, "", false
}
