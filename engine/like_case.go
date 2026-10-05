package engine

// This file holds the resolution of "PRAGMA case_sensitive_like". It is
// CONNECTION state plumbing, not expression evaluation, which is why it sits
// in a file of its own. C SQLite keeps the same fact on the connection too,
// and even more emphatically: the pragma RE-REGISTERS like() itself
// (pragma.c:1660 -> sqlite3RegisterLikeFunctions, func.c:2358, which swaps
// likeInfoNorm for likeInfoAlt), so nothing that looks at the expression ever
// has to know.

// likeCaseSensitive reports this evaluation context's active "PRAGMA
// case_sensitive_like" setting.
//
// It reads the query's pager first, which SnapshotPager stamped from the
// connection's DB.caseSensitiveLike, and falls back to the flag carried on the
// context: a context with no pager (e.g. a RETURNING capture with no subquery)
// must still honour the pragma, so "INSERT INTO t VALUES('AbC') RETURNING x
// LIKE 'ab%'" is 0 with case_sensitive_like ON. Neither set means false,
// SQLite's case-insensitive default.
func (ctx *evalCtx) likeCaseSensitive() bool {
	if ctx == nil {
		return false
	}
	if ctx.pager != nil {
		return ctx.pager.caseSensitiveLike
	}
	return ctx.caseSensitiveLike
}
