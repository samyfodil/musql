// This file tests ATTACH with non-literal paths: expressions, identifiers,
// NULL, numeric literals, and bound parameters.
package compat

import (
	"path/filepath"
	"testing"
)

// TestEngineAttachExpressionPath tests ATTACH with various expression forms.
func TestEngineAttachExpressionPath(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux")
	// A second, per-side path for the expression cases below. It has to be
	// ABSOLUTE and under t.TempDir(): a relative one would create a database
	// beside this package's source, which AGENTS.md forbids outright, and
	// DETACH does not delete a real (non-':memory:') attachment's file.
	dir := t.TempDir()
	p := newAttachPair(t,
		[]string{goAux, filepath.Join(dir, "go-expr.db2")},
		[]string{cgoAux, filepath.Join(dir, "cgo-expr.db2")})

	// ATTACH's path is a general EXPRESSION in SQLite's grammar, and a
	// scalar function call is legal there -- the oracle evaluates it at
	// RUNTIME (attach.c's codeAttach compiles it as ordinary VDBE bytecode;
	// verified directly against mattn/go-sqlite3 3.53.3: "ATTACH
	// printf('exprpath.db2') AS aux1" creates a file literally named
	// "exprpath.db2"). parseAttachPath evaluates it the same way, against an
	// empty evalCtx -- which is exactly the "no row context" attach.c compiles
	// it with -- so these agree rather than declining.
	p.agreeExec("ATTACH printf('%s', '{1}') AS aux1")
	p.agreeExec("DETACH aux1")
	p.agreeExec("ATTACH '{1}' || '' AS aux1")
	p.agreeExec("DETACH aux1")
	// ':memory:' assembled by concatenation is auth.test's own case, and the
	// whole reason this cannot be left as a literal: the oracle attaches a
	// PRIVATE database, not a file called ":mem' || 'ory:".
	p.agreeExec("ATTACH ':mem' || 'ory:' AS memexpr")
	p.agreeQuery("SELECT count(*) FROM memexpr.sqlite_master")
	p.agreeExec("DETACH memexpr")
	// A path expression that needs a ROW is the boundary that remains: there
	// is no FROM clause for it to bind against, so C SQLite rejects it too
	// ("no such column: t.c", verified directly).
	p.declineExec("ATTACH t.c AS aux1")

	// A bare, UNQUOTED identifier is C SQLite's OTHER special case here:
	// resolveAttachExpr rebinds it straight to its own spelling as a
	// string, never as a column reference (there is no FROM clause for one
	// to bind against; verified directly: "ATTACH DATABASE x AS y" attaches
	// a file literally named "x"). Not exercised as a differential exec
	// HERE, because a bare word is necessarily a path relative to the
	// process's CWD (it cannot contain a path separator and still lex as
	// one token) -- writing there would violate AGENTS.md's no-stray-dbs
	// rule even with an immediate DETACH, since DETACH does not delete a
	// real (non-':memory:') attachment's file. TestParseAttachStmtLiteralForms
	// (engine package) pins the resolved TEXT at the parser level instead,
	// with no file ever created.
	//
	// A bound parameter ("?") is the same question one level further out.
	// ParseAttachStmt is also driver's own ATTACH parser, but NEITHER
	// call path currently threads a bound VALUE this far (driver's
	// handleAttachDetach receives only the SQL text, never args -- see
	// engine/attach.go's package comment), so every parameter reaching
	// attachExprLiteral is, today, genuinely unbound -- and an unbound host
	// parameter's value is NULL by SQLite's own C-API contract (not a
	// driver artifact): verified directly against mattn/go-sqlite3 3.53.3,
	// via Exec -- the ONLY way ATTACH/DETACH ever reaches the oracle here,
	// never Query, whose own stricter database/sql arg-count precheck does
	// not apply to Exec and would otherwise mislead this into looking like
	// a mutual reject. Both "ATTACH DATABASE ? AS ?" and "ATTACH DATABASE
	// '' AS ?" attach a private database named "" (pragma_database_list
	// shows name="" file=""), exactly "ATTACH '' AS ''"'s own semantics,
	// and exactly what attach3.test's own do_test expects (section 12).
	p.agreeExec("ATTACH DATABASE ? AS ?")
	p.agreeExec("DETACH ?")
	p.agreeExec("ATTACH DATABASE '' AS ?")
	p.agreeExec("DETACH ''")
}
