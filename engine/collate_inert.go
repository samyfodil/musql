// This file handles the one case where an unknown collation is not an error:
// a COLLATE expression in a SELECT result column (parseCollate, sql_parser.go).
// Re-parse with the check suppressed, and accept only if all unknown collations
// appear as outermost result columns with no other clauses consuming them.
// answers the first line above.
package engine

import "strings"

// unknownCollationErrText is parseCollate's message, matched to decide whether
// a failed parse is worth retrying. Matching the TEXT rather than a sentinel
// keeps the retry entirely outside the parser's own error paths: nothing else
// has to know this exists.
const unknownCollationErrText = "no such collation sequence: "

// isUnknownCollationErr reports whether err is parseCollate's rejection.
func isUnknownCollationErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), unknownCollationErrText)
}

// inertUnknownCollations counts the result columns of stmt whose whole
// expression is "<something> COLLATE <name>" with an unrecognized name -- the
// only position this file serves. ok is false when stmt carries ANY clause
// that could consume a collation, in which case the count is meaningless.
func inertUnknownCollations(stmt *SelectStmt) (n int, ok bool) {
	if stmt == nil {
		return 0, false
	}
	if stmt.From != nil || stmt.Where != nil || stmt.GroupBy != nil || stmt.Having != nil ||
		stmt.OrderBy != nil || stmt.Compound != nil || stmt.Distinct ||
		stmt.Limit != nil || stmt.Offset != nil ||
		stmt.LimitParam != nil || stmt.OffsetParam != nil ||
		len(stmt.CTEs) != 0 || len(stmt.Windows) != 0 {
		return 0, false
	}
	for _, c := range stmt.Columns {
		ce, isColl := c.Expr.(CollateExpr)
		if !isColl {
			continue
		}
		if !knownCollations[asciiFold(ce.Name, false)] {
			n++
		}
	}
	return n, true
}

// parseSelectAllowingInertCollation is ParseSelect's retry: re-parse sql with
// parseCollate's check suppressed and hand back the statement only if every
// suppressed name sits where nothing can consume it. Any other outcome reports
// ok == false and the caller returns its original error.
func parseSelectAllowingInertCollation(sql string) (*SelectStmt, bool) {
	toks, err := lex(sql)
	if err != nil {
		return nil, false
	}
	p := newParser(sql, toks)
	p.allowUnknownCollation = true
	stmt, err := p.parseSelectStmt()
	if err != nil {
		return nil, false
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, false
	}
	if p.unknownCollations == 0 {
		// The parse only succeeded the second time for some OTHER reason --
		// impossible today, but returning false keeps the first error rather
		// than serving a statement this retry never accounted for.
		return nil, false
	}
	n, ok := inertUnknownCollations(stmt)
	if !ok || n != p.unknownCollations {
		return nil, false
	}
	stmt.Params = p.params.info()
	return stmt, true
}
