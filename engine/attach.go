// ATTACH and DETACH:
//
//  1. primitives the driver Conn uses to implement ATTACH/DETACH and route a
//     schema-qualified statement to the owning database's file
//     (ParseAttachStmt/ParseDetachStmt, ReferencedTables,
//     StatementTargetSchema/StripDDLTargetSchema, TableNames); and
//  2. ATTACH/DETACH as engine statements (execAttach/execDetach, via OpDdl's
//     ddlAttach/ddlDetach) for a caller driving a *DB directly.
//
// The two never coexist on one session: the driver intercepts ATTACH/DETACH
// (handleAttachDetach), so a driver-owned *DB has an empty db.attached.
//
// ATTACH opens each database as a read pager wired onto db.attachedReaders,
// which SnapshotPager copies onto every read, so cross_db.go resolves
// foreign-qualified and attached-only tables. Writes into an attachment are
// delegated to its own write session (attach_write.go).
//
// Semantics, checked against C SQLite:
//
//	ATTACH 'p' AS x   /  ATTACH DATABASE 'p' AS x     both accepted
//	AS 'x' / AS "x" / AS [x]                          all accepted; name unquoted
//	re-ATTACH of a live name x     -> "database x is already in use"
//	AS main / AS temp              -> "database main|temp is already in use"
//	the SAME FILE under two names  -> accepted
//	an 11th attachment             -> "too many attached databases - max 10"
//	ATTACH ':memory:' / ATTACH ''  -> a private empty database, discarded on
//	                                  DETACH
//	ATTACH NULL AS x / ATTACH DATABASE ? AS ?  (? unbound)
//	                                -> NULL becomes "", so this is ATTACH ''
//	ATTACH x AS y  (bare, unquoted) -> attaches a file literally named "x"
//	ATTACH 0123 AS x                -> the numeric VALUE: attaches "123"
//	ATTACH of a nonexistent path   -> succeeds, and the file is created
//	DETACH x / DETACH DATABASE x   both accepted, as is DETACH 'x'
//	DETACH of an unattached name   -> "no such database: x"
//	DETACH main                    -> "cannot detach database main"
//	DETACH temp                    -> "no such database: temp" (C says "cannot
//	                                  detach database temp" once a temp table
//	                                  exists; only the first is reproduced)
//
// The PATH is evaluated as an expression (parseAttachPath). Still declined:
// a NAME beyond a single literal token ("AS 'a'||'b'"), and a trailing "KEY
// <expr>" clause. Single tokens resolve as attach.c's resolveAttachExpr does
// (attachExprLiteral), not as expression evaluation would: a bare or quoted
// identifier is its own spelling, NULL is "", a number is its value's text,
// and an unbound parameter is NULL (ATTACH/DETACH always go through Exec;
// attach3.test section 12).
//
// "file:" URIs (resolveAttachURIPath) accept only parameters this engine can
// honor: mode (rw/rwc/memory; ro declined), 8_3_names (a no-op off Windows),
// cache=private, cache=shared with mode=memory (memdb_registry.go), and
// vfs=memdb. This is stricter than C, which ignores unknown parameters, but
// nolock, immutable and cache=shared on a real file change semantics this
// engine cannot reproduce (shared BtShared, btree.c:2594-2649), so they are
// declined rather than ignored.

package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// TableRef is one database-qualified table reference: Schema is the database
// qualifier as written ("" if unqualified), Table the bare table name.
type TableRef struct {
	Schema string
	Table  string
}

// ParseAttachStmt recognizes "ATTACH [DATABASE] <path-expr> AS <name-expr>".
// ok=false with nil error means "not an ATTACH". An ATTACH this parser cannot
// handle (a complex name, a missing AS, a KEY clause) returns ok=true with an
// error. path is the resolved value (':memory:' handled by the caller); name is
// resolved as in attachExprLiteral.
func ParseAttachStmt(sqlText string) (path, name string, ok bool, err error) {
	toks, lerr := lex(strings.TrimSpace(sqlText))
	if lerr != nil {
		return "", "", false, nil
	}
	if len(toks) == 0 || toks[0].kind != tkIdent || toks[0].upper() != "ATTACH" {
		return "", "", false, nil
	}
	p := newParser(sqlText, toks)
	p.next() // ATTACH
	p.consumeKeyword("DATABASE")
	pathText, perr := parseAttachPath(p)
	if perr != nil {
		return "", "", true, perr
	}
	if !p.consumeKeyword("AS") {
		return "", "", true, fmt.Errorf("engine: ATTACH: expected AS")
	}
	nt := p.next()
	name, err = attachSchemaName(nt)
	if err != nil {
		return "", "", true, err
	}
	if p.peek().kind != tkEOF {
		return "", "", true, fmt.Errorf("engine: ATTACH: trailing tokens (a KEY clause is not supported)")
	}
	// A "file:" path is a SQLite URI, not a filename: the oracle resolves
	// ATTACH 'file:uritest.db' to the file "uritest.db" (verified directly),
	// so taking it literally here would let two ATTACHes SQLite maps onto ONE
	// file map onto two -- a wrong answer rather than a missing feature.
	//
	// The check is deliberately case-SENSITIVE, matching sqlite3ParseUri's own
	// memcmp (not a case-folding compare): "FILE:x.db" is not recognized as a
	// URI by C SQLite either, and is attached as a plain file literally
	// named "FILE:x.db" (verified directly: the byte-for-byte-named file
	// appears on disk, not "x.db").
	if strings.HasPrefix(pathText, "file:") {
		resolved, uerr := resolveAttachURIPath(pathText)
		if uerr != nil {
			return "", "", true, uerr
		}
		return resolved, name, true, nil
	}
	return pathText, name, true, nil
}

// parseAttachPath resolves ATTACH's PATH argument, consuming its tokens so the
// caller lands on AS.
//
// attachExprLiteral's single-token subset is tried first and kept when AS
// follows, because it differs from evaluation: a bare identifier is its own
// spelling ("ATTACH x AS y" attaches "x"), not a column.
//
// Anything else is an expression compiled with no row context, as codeAttach
// does (sqlite3ExprCode, attach.c:403; called as a function, attach.c:409), via
// selectExprValue (expr_value.go). auth.test's "ATTACH ':mem' || 'ory:' AS
// test1" attaches a private database. A FROM-less subquery works ("ATTACH
// (SELECT 'q.db') AS y") even with no pager (selectNeedsNoRowSource). A
// subquery reading a table is still declined: it needs the connection's
// snapshot, which this shared text->path entry point is not given.
//
// attachExprLiteral's syntax error is not terminal here for keywords that can
// open an expression (attachExprOpeningKeywords): "ATTACH CASE WHEN 1 THEN
// '<path>' END AS x" is valid in C.
//
// The value uses TEXT's NULL -> "" rule (valueToText); "" means a private
// database.
func parseAttachPath(p *parser) (string, error) {
	start := p.pos
	t := p.next()
	text, ok, err := attachExprLiteral(t)
	if err != nil && !(t.kind == tkIdent && !t.quoted && attachExprOpeningKeywords[t.upper()]) {
		return "", err
	}
	if ok && p.peekIsKeyword("AS") {
		return text, nil
	}
	p.pos = start
	e, perr := p.parseExpr()
	if perr != nil {
		return "", fmt.Errorf("engine: ATTACH: %w", perr)
	}
	// Resolve under the zeroed NameContext codeAttach gives its arguments
	// (attach.c:373, :377-379), so an aggregate, FILTER or OVER in the path
	// is a resolve-time error as in C ("FILTER may not be used with
	// non-aggregate ltrim()") rather than parsed and ignored
	// (requireZeroedNameContext, expr_value.go; i4_compiled_expr_test.go).
	if err := requireZeroedNameContext(e); err != nil {
		return "", err
	}
	v, verr := (*ReadOnlyPager)(nil).selectExprValue(e)
	if verr != nil {
		return "", fmt.Errorf("engine: ATTACH: database path expression: %w", verr)
	}
	return valueToText(v), nil
}

// resolveAttachURIPath resolves a "file:" ATTACH path as SQLite's URI layer
// does, for the bounded set of forms this engine can honor (see the file doc).
// raw starts with the case-sensitive "file:". The result is opened like a
// plain filename; "" means a private database.
//
//	file:test.db2               -> "test.db2"
//	file://localhost/abs/path    -> "/abs/path"
//	file:///abs/path             -> "/abs/path"
//	file:/abs/path                -> "/abs/path"
//	file://baduser/abs/path      -> "invalid uri authority: baduser"
//	file:test.db2?mode=rw, file missing   -> "unable to open database: <raw>",
//	                                          file not created
//	file:test.db2?mode=rw, file exists    -> opened
//	file:test.db2?mode=rwc (or no mode)   -> created if missing
//	file:X?mode=memory                    -> a private database (two such
//	                                          ATTACHes do not share)
//	file:X?mode=memory&cache=shared       -> a named shared in-memory
//	                                          database keyed by X, via
//	                                          memdb_registry.go's
//	                                          sharedMemPathSentinel
//	file:X?cache=shared (mode != memory)  -> declined (shared page cache,
//	                                          btree.c:2594-2649)
//	file:X?cache=bogus                    -> "no such cache mode: bogus"
//	file:X?mode=bogus                     -> "no such access mode: bogus"
//	file:./test2.db?8_3_names=1|0         -> a no-op (8_3_names.test)
//	file:X?vfs=unix|unix-none|            -> declined: per-VFS locking is
//	     unix-dotfile|unix-excl              not modelled
//	file:/name?vfs=memdb                  -> a process-wide shared in-RAM
//	                                          database (memdb_registry.go)
//	file:X?vfs=memdb (no leading slash)   -> a private database
//	file:X?vfs=tvfs2 (or anything else)   -> "no such vfs: tvfs2" (uri.test)
func resolveAttachURIPath(raw string) (string, error) {
	rest := raw[len("file:"):]
	// A "#" fragment, and everything after it, is dropped before path/query
	// splitting even begins -- sqlite3ParseUri's own decode loop stops at '#'.
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}

	// Authority: "//" introduces one, terminated by the next "/". Only ""
	// and exactly "localhost" (case-sensitive) are legal -- sqlite3ParseUri's
	// own memcmp, not a case-folding compare.
	if strings.HasPrefix(rest, "//") {
		afterSlashes := rest[2:]
		end := strings.IndexByte(afterSlashes, '/')
		if end < 0 {
			end = len(afterSlashes)
		}
		authority := afterSlashes[:end]
		if authority != "" && authority != "localhost" {
			return "", fmt.Errorf("engine: invalid uri authority: %s", authority)
		}
		rest = afterSlashes[end:] // keeps the leading "/" of the path, if any
	}

	pathPart, queryPart, hasQuery := strings.Cut(rest, "?")
	if strings.ContainsRune(pathPart, '%') || strings.ContainsRune(queryPart, '%') {
		// C SQLite decodes a %HH escape in either the path or a query
		// name/value while copying it (sqlite3ParseUri's own loop) -- not a
		// shape the mined corpus's own ATTACH URIs use, so left declined
		// rather than guessed at, per this file's package comment.
		return "", fmt.Errorf("%w: ATTACH URI %q (a percent-encoded path or query component is not supported)", errVDBEUnsupported, raw)
	}

	mode, vfs, cache := "", "", ""
	if hasQuery {
		for _, kv := range strings.Split(queryPart, "&") {
			if kv == "" {
				continue
			}
			key, val, _ := strings.Cut(kv, "=")
			switch key {
			case "mode":
				mode = val
			case "vfs":
				vfs = val
			case "cache":
				cache = val
			case "8_3_names":
				// A Windows-only 8.3-filename-mangling toggle; this engine, like
				// the oracle's own non-Windows VFS, never consults it -- a
				// verified no-op (8_3_names.test, crashM.test).
			default:
				return "", fmt.Errorf("%w: ATTACH URI parameter %q (only mode=, cache= and 8_3_names= are supported; see resolveAttachURIPath)", errVDBEUnsupported, key)
			}
		}
	}

	// sqlite3ParseUri's OWN vfs lookup (main.c:3321-3325) runs unconditionally
	// near the end of the function, regardless of mode= -- *ppVfs =
	// sqlite3_vfs_find(zVfs); a miss is "no such vfs: %s", checked here before
	// mode's own switch so it is not skipped by mode=memory's early return.
	if vfs != "" {
		switch vfs {
		case "unix", "unix-none", "unix-dotfile", "unix-excl":
			// Legitimate VFS names a bare Linux build always registers
			// (os_unix.c:8501-8524's aVfs[], excluding the
			// SQLITE_ENABLE_LOCKING_STYLE-only ones) -- C SQLite ACCEPTS
			// the ATTACH with one of these. This engine does not model the
			// per-VFS locking-strategy differences (unix-none/unix-dotfile/
			// unix-excl trade fcntl locks for no locking / a dotfile lock /
			// an exclusive-only lock), so declining is the safe "never
			// wrong" call rather than silently ignoring the choice. Likely
			// narrower than memdb's own gap (below) -- these three only
			// change WHICH lock primitive guards a real file that still
			// exists on disk -- but not investigated to the same depth.
			return "", fmt.Errorf("%w: ATTACH URI vfs=%s (a VFS name C SQLite accepts, but this engine does not model per-VFS locking-strategy differences)", errVDBEUnsupported, vfs)
		case "memdb":
			// vfs=memdb is served by memdb_registry.go (share-by-name, refcounted
			// free on last close). mode= together with vfs= is declined: C applies
			// them independently (main.c:3321-3325), but the vfs switch here
			// returns before mode is consulted, so a mode would be dropped.
			if mode != "" {
				return "", fmt.Errorf("%w: ATTACH URI vfs=memdb combined with mode=%s (this engine applies vfs= and mode= as one branch, not independently; see resolveAttachURIPath's memdb case)", errVDBEUnsupported, mode)
			}
			// memdbOpen's gate (memdb.c:556-557): a name longer than one character
			// starting with '/' or '\\' is shared, looked up by exact string;
			// anything else is connection-private with no locking (MemStore's doc
			// comment), the same as ATTACH ':memory:', so it is routed through the
			// "" convention.
			if len(pathPart) > 1 && (pathPart[0] == '/' || pathPart[0] == '\\') {
				return memdbPathSentinel + pathPart, nil
			}
			return "", nil
		default:
			// Anything else is what a plain, unmodified Linux build of real
			// SQLite ALSO rejects with exactly this message -- verified
			// directly against the oracle this repo's own harness runs
			// (mattn/go-sqlite3 v1.14.48): unix/unix-none/unix-dotfile/
			// unix-excl/memdb succeed; unix-posix/unix-flock
			// (SQLITE_ENABLE_LOCKING_STYLE-only) and any unknown name fail
			// with this text. Not a decline: this engine AGREES with the
			// oracle here, byte-exact -- the same "no such access mode"
			// precedent mode's own switch uses below.
			return "", fmt.Errorf("engine: no such vfs: %s", vfs)
		}
	}

	// "cache" is validated here, before mode's own switch, mirroring where the
	// vfs check above already runs: sqlite3ParseUri accumulates cache's
	// SQLITE_OPEN_SHAREDCACHE/PRIVATECACHE bits in the SAME parameter loop as
	// mode (main.c:3255-3266), independent of mode's own switch there -- but
	// this function has a real counterpart for only ONE combination (mode=
	// memory, handled inside the "memory" case below via the process-wide
	// registry vfs=memdb already uses), so every other one is declined
	// outright here.
	switch cache {
	case "", "private":
		// The default (SQLITE_OPEN_PRIVATECACHE, or no shared-cache bit at all
		// when shared-cache mode is not globally enabled -- main.c:3392-3396):
		// every attachment in this engine already opens its own exclusive
		// pager, which already IS private-cache behavior, so there is nothing
		// to do -- a verified no-op (e_uri.test, uri.test).
	case "shared":
		if mode != "memory" {
			// A real FILE's shared cache is a different feature this engine
			// has no counterpart for at all: two connections attaching the
			// SAME real file with cache=shared read/write through ONE shared
			// BtShared/pager (btree.c:2594-2649), where this engine always
			// opens one exclusive pager per attachment, per session (see this
			// file's package comment). Declined, not approximated -- unlike
			// mode=memory below, which has a real counterpart.
			return "", fmt.Errorf("%w: ATTACH URI cache=shared combined with mode=%s (only a named IN-MEMORY shared-cache database is supported; see resolveAttachURIPath's cache=shared case)", errVDBEUnsupported, mode)
		}
		// mode=="memory": handled in the "memory" case below, the one
		// combination this engine can honor byte-for-byte.
	default:
		// C SQLite's own message (main.c:3294, zModeType="cache") for any
		// value besides "shared"/"private" -- not a decline, this engine
		// AGREES with the oracle here, the same "no such access mode"
		// precedent mode's own switch below uses.
		return "", fmt.Errorf("engine: no such cache mode: %s", cache)
	}

	switch mode {
	case "", "rwc":
		// The default: create-if-missing, exactly like a non-URI ATTACH --
		// execAttach's own ensureDatabaseFile call does this unchanged.
	case "memory":
		if cache == "shared" && pathPart != "" {
			// A named shared in-memory database: two ATTACHes of the same
			// "file:X?mode=memory&cache=shared", even from separate connections,
			// share one database (btree.c:2594-2649, keyed by zFilename). It uses
			// the memdb registry under a namespaced key (sharedMemPathSentinel).
			//
			// pathPart != "" matters: an empty filename is isTempDb
			// (btree.c:2548), never a shared-cache candidate (btree.c:2594), so it
			// gets a private database. Without the guard every empty-path
			// cache=shared ATTACH in the process would share one store.
			return sharedMemPathSentinel + pathPart, nil
		}
		// A private, non-durable database, regardless of the path text.
		// execAttach's isMem branch triggers on an empty path, so returning
		// "" reuses it exactly -- in BOTH callers of this parser, since
		// driver's own attach() applies the identical convention.
		return "", nil
	case "rw":
		// Unlike rwc, C SQLite does NOT create a missing file here -- and
		// the failure surfaces at ATTACH time, before execAttach's own
		// ensureDatabaseFile ever runs, so it must be checked here rather
		// than left to that call's own create-on-missing behavior.
		if _, err := os.Stat(pathPart); err != nil {
			// attachFunc's own message names the RAW argument, not the
			// resolved path (verified: the oracle's text echoes the whole
			// "file:...?mode=rw" literal) -- reproduced verbatim since it
			// costs nothing extra here.
			return "", fmt.Errorf("engine: unable to open database: %s", raw)
		}
	case "ro":
		// Declined, not approximated: honoring it means refusing every WRITE
		// into the attachment, which needs a read-only flag threaded through
		// attachedDB and attachedWriteSession's single choke point in
		// attach_write.go -- and since ParseAttachStmt is also driver's
		// own ATTACH parser (this file's package comment), a fifth return
		// value carrying that flag would have to cross that boundary too.
		// Not worth it for a mode the mined corpus never actually asks for
		// (only "mode=rw" appears, uri.test#336); mode=rw and mode=rwc need
		// no such tracking at all, since a write into them is just a write.
		return "", fmt.Errorf("%w: ATTACH URI mode=ro (read-only enforcement on a delegated write session is not implemented)", errVDBEUnsupported)
	default:
		return "", fmt.Errorf("engine: no such access mode: %s", mode)
	}
	return pathPart, nil
}

// ParseDetachStmt recognizes "DETACH [DATABASE] <name>". Like ParseAttachStmt,
// ok=false means "not a DETACH statement"; ok=true with a non-nil error means a
// malformed DETACH the caller should decline.
func ParseDetachStmt(sqlText string) (name string, ok bool, err error) {
	toks, lerr := lex(strings.TrimSpace(sqlText))
	if lerr != nil {
		return "", false, nil
	}
	if len(toks) == 0 || toks[0].kind != tkIdent || toks[0].upper() != "DETACH" {
		return "", false, nil
	}
	p := newParser(sqlText, toks)
	p.next() // DETACH
	p.consumeKeyword("DATABASE")
	nt := p.next()
	name, err = attachSchemaName(nt)
	if err != nil {
		return "", true, err
	}
	if p.peek().kind != tkEOF {
		return "", true, fmt.Errorf("engine: DETACH: trailing tokens")
	}
	return name, true, nil
}

// attachExprLiteral decodes one token standing for an ATTACH/DETACH PATH or
// NAME. attach.c resolves both through resolveAttachExpr, which special-cases
// these shapes before any expression evaluation:
//
//   - a bare or quoted identifier is its own spelling as a string ("ATTACH
//     DATABASE x AS y" attaches a file named "x").
//   - an unquoted word that cannot start an expression
//     (nonIdentifierKeywords, sql_parser.go) is a syntax error: "ATTACH
//     'test3.db' AS ON" is `near "ON": syntax error` (alter.test). Quoted, it
//     is a name.
//   - CAST and RAISE open an expression although they are legal column
//     names, so they are reported unresolved (attachIncompleteKeywords); C
//     says "incomplete input" for "AS CAST". CASE/EXISTS/NOT are caught as
//     syntax errors, which is right for a NAME; the PATH retries them
//     (attachExprOpeningKeywords).
//   - CURRENT_DATE/TIME/TIMESTAMP evaluate, and the value is bound: "ATTACH
//     'x' AS CURRENT_DATE" names the schema after today's date. Lowered to
//     date()/time()/datetime() as parsePrimary does.
//   - NULL is "" ("DETACH null" is "no such database: ").
//   - a number is its value's text ("DETACH 12.5", "ATTACH 0123 AS x"
//     attaches "123"; stat.test).
//   - an unbound parameter is NULL, since no caller threads bound values
//     here.
//
// ok is false with nil error for anything else (an expression), leaving the
// caller to report it; err is set only for the syntax-error case.
func attachExprLiteral(t token) (text string, ok bool, err error) {
	switch t.kind {
	case tkIdent:
		if !t.quoted && t.upper() == "NULL" {
			return "", true, nil
		}
		if !t.quoted && nonIdentifierKeywords[t.upper()] {
			return "", false, fmt.Errorf("engine: near %q: syntax error", t.text)
		}
		if !t.quoted && attachIncompleteKeywords[t.upper()] {
			// ok=false, not an error: this token OPENS an expression, so the
			// PATH position must still get to try parsing one ("ATTACH
			// CAST(7 AS TEXT) AS y" is a real, accepted statement). Erroring
			// here instead broke exactly that case. Where the word really is
			// the whole argument, both callers then reject it -- the path via
			// parseExpr, the name via attachSchemaName -- which is the
			// agreement that matters; the oracle's own wording there is
			// "incomplete input".
			return "", false, nil
		}
		if !t.quoted {
			if kind, isCTime := attachCTimeKeywords[t.upper()]; isCTime {
				v, _, verr := fnDateTimeFamily(kind, nil)
				if verr != nil {
					return "", false, verr
				}
				return string(v.S), true, nil
			}
		}
		return t.text, true, nil
	case tkString:
		return t.str, true, nil
	case tkNumber:
		if i, ierr := strconv.ParseInt(t.text, 10, 64); ierr == nil {
			return strconv.FormatInt(i, 10), true, nil
		}
		if f, rc := sqliteAtoF([]byte(t.text)); rc > 0 {
			return formatFloatText(f), true, nil
		}
		return "", false, nil
	case tkParam:
		return "", true, nil
	}
	return "", false, nil
}

// attachIncompleteKeywords are bare keywords C shifts into an expression in an
// ATTACH/DETACH argument and then reports "incomplete input" for. They are not
// in nonIdentifierKeywords, which answers "is this a legal bare column name"
// (both are). attachExprLiteral reports them unresolved since they open an
// expression: "ATTACH CAST(7 AS TEXT) AS y" attaches "7". Derived from the
// oracle keyword by keyword; compat-harness/attach_r26_alias_keyword_test.go
// re-derives it so a SQLite upgrade that moves one fails there.
var attachIncompleteKeywords = map[string]bool{"CAST": true, "RAISE": true}

// attachExprOpeningKeywords are keywords in nonIdentifierKeywords (a syntax
// error in the NAME position) that can open an expression, so in the PATH
// position the statement is valid in C. Derived by trying every
// nonIdentifierKeywords entry in the PATH position as a prefix operator, CASE
// and parenthesized form; these three attached. Re-derived by
// TestI4AttachPathKeywordClass.
var attachExprOpeningKeywords = map[string]bool{"CASE": true, "EXISTS": true, "NOT": true}

// attachCTimeKeywords maps SQLite's three CTIME_KW keywords to the date/time
// function each is defined as. In an ATTACH/DETACH argument they EVALUATE --
// the schema name bound is the value's text, not the word -- exactly as they
// do in ordinary expression position (parsePrimary, sql_parser.go, lowers
// them to the identical zero-argument calls).
var attachCTimeKeywords = map[string]string{
	"CURRENT_DATE": "date", "CURRENT_TIME": "time", "CURRENT_TIMESTAMP": "datetime",
}

// attachSchemaName is attachExprLiteral for the NAME position specifically,
// where an unresolved token is always a terminal error (there is no
// position-specific fallback message to give, unlike ParseAttachStmt's PATH,
// which has its own "only a string-literal database path is supported").
func attachSchemaName(nt token) (string, error) {
	text, ok, err := attachExprLiteral(nt)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("engine: ATTACH/DETACH: expected a database name")
	}
	return text, nil
}

// attachedDB is one live ATTACH binding on a write session (DB.attached).
// pager is the read-only view of that database, opened by execAttach and held
// for the life of the binding; it is what DB.attachedReaders points at.
type attachedDB struct {
	name  string // attach schema name, case preserved (matched case-insensitively)
	path  string // backing file
	isMem bool   // ATTACH ':memory:' / '': a private temp file, removed on DETACH/Close

	// touchedInTxn marks this attachment read or written by the open transaction,
	// which makes C refuse to DETACH it:
	//
	//	BEGIN; DETACH aux                          ok  (never touched)
	//	BEGIN; SELECT x FROM aux.s; DETACH aux     database aux is locked
	//	BEGIN; INSERT INTO aux.s ...; DETACH aux   database aux is locked
	//	BEGIN; SELECT a FROM m; DETACH aux         ok  (only MAIN was read)
	//	DETACH aux with no transaction             ok
	//
	// Set by write routing (enterTxnAttachedWrite) and by a read resolving here
	// (ReadOnlyPager.itemOwner), cleared with the transaction. Over-marking only
	// over-refuses.
	touchedInTxn bool
	pager        *ReadOnlyPager

	// reservedInTxn is the RESERVED lock C SQLite's write through this
	// handle takes and holds until the transaction ends: set when a WRITE
	// through it is admitted inside an explicit transaction
	// (attachedWriteSessionFor), cleared with the transaction (clearTxnState).
	// It is what the same-file alias rules below key on -- see
	// attachedWriteSessionFor and attachedCommitLockCheck. touchedInTxn is the
	// SHARED half: a read (or a write) through the handle inside the
	// transaction.
	//
	// It deliberately SURVIVES a statement that fails after taking it, as C's
	// lock does: a statement rollback restores pages, never locks.
	reservedInTxn bool

	// isMemdb and memdbName mark a "file:/name?vfs=memdb" SHARED attachment
	// (see resolveAttachURIPath's memdb case and memdb_registry.go): path is
	// the real temp file the process-wide registry handed back for
	// memdbName, and close() must release the registry's reference on it
	// (memdbRelease) instead of the isMem branch's unconditional os.Remove,
	// since another attachment -- in this session or another -- may still
	// hold the same name.
	isMemdb   bool
	memdbName string

	// isSharedMem and sharedMemName mark a "file:X?mode=memory&cache=shared"
	// attachment (see resolveAttachURIPath's cache=shared case and
	// memdb_registry.go's sharedMemPathSentinel): path is the real temp file
	// the SAME process-wide registry vfs=memdb uses handed back for
	// sharedMemRegistryKey(sharedMemName), and close() must release the
	// registry's reference on it (memdbRelease) instead of the isMem branch's
	// unconditional os.Remove, exactly mirroring isMemdb/memdbName just above.
	isSharedMem   bool
	sharedMemName string

	// wdb is this attachment's own WRITE session, opened lazily the first time a
	// statement writes into it and committed by COMMIT/Close -- see
	// attach_write.go, which explains why a cross-database write is routed to a
	// second single-file session rather than taught to the first.
	wdb *DB

	// wsess is that write session's SEGMENT owner, non-nil for a database in our
	// own format -- which is every database this engine executes against.
	//
	// wdb is the ROW STORE and the Session is what persists it (Session.Commit
	// appends a delta batch). Every OTHER site keeps using wdb -- schema
	// lookups, savepoints, the per-connection setters -- so this field is
	// consulted at exactly the two lifecycle points where the persist happens,
	// through closeWrite and discardWrite below.
	wsess *Session

	// wroteData records that this attachment's session really mutated the file,
	// as opposed to wdb merely existing, which also happens for a read routed here
	// ("PRAGMA t2.integrity_check"). Conflating them made a second alias of a file
	// only ever read report "already written into" (pragma.test 3.9c/3.15).
	//
	// It is sticky for the attachment's life: attachedWriteSession needs "this
	// alias may hold a lock a sibling's write conflicts with" until COMMIT/Close,
	// and staying true can only over-decline. Set by execRoutedToAttached
	// (attachedRoutedStatementWrites), execVacuumAttached and
	// opTriggerBodyRouted.
	wroteData bool

	// journalMode is the journal mode this attachment's session should open in
	// ("" == delete). The session opens lazily, so an unqualified "PRAGMA
	// journal_mode = <mode>" (execJournalMode) records it here for attachments
	// without a session yet. It is not seeded from the connection: a database
	// attached after the switch keeps "delete", as in C.
	journalMode string
}

// maxAttachedDatabases mirrors SQLITE_MAX_ATTACHED's default (and the oracle
// build's actual limit, verified: the 11th ATTACH fails with exactly the
// message reproduced in execAttach).
const maxAttachedDatabases = 10

// execAttach runs "ATTACH [DATABASE] '<path>' AS <name>", adding a binding to
// db.attached. The database is opened immediately, so a file that is not ours
// fails here (C defers that to first access). A missing or zero-length file is
// created empty first, as C creates it at ATTACH.
func (db *DB) execAttach(trimmed string) error {
	path, name, ok, err := ParseAttachStmt(trimmed)
	if !ok {
		// Unreachable: compileWriteProgram only emits ddlAttach for an ATTACH.
		return fmt.Errorf("engine: ATTACH: not an ATTACH statement: %q", trimmed)
	}
	if err != nil {
		return err
	}
	if equalFoldName(name, "main") || equalFoldName(name, "temp") || equalFoldName(name, localSchemaOr(db.localSchema)) {
		return fmt.Errorf("engine: database %s is already in use", name)
	}
	for _, a := range db.attached {
		if equalFoldName(a.name, name) {
			return fmt.Errorf("engine: database %s is already in use", name)
		}
	}
	if len(db.attached) >= maxAttachedDatabases {
		return fmt.Errorf("engine: too many attached databases - max %d", maxAttachedDatabases)
	}
	ad := &attachedDB{name: name, path: path}
	if memdbName, isMemdb := IsMemdbAttachName(path); isMemdb {
		// A second ATTACH of one shared memdb name is accepted (unlike
		// cache=shared below). The aliases are two handles on one MemStore,
		// governed by memdb's lock arbitration (memdb.c:368-419), above all
		// that a new SHARED is refused while another handle holds RESERVED:
		//
		//	INSERT INTO m1.t ...; SELECT * FROM m2.t   -> sees the insert
		//	BEGIN; INSERT INTO m1.t ...; SELECT * FROM m2.t
		//	                                           -> "database is locked"
		//	COMMIT; SELECT * FROM m2.t                 -> sees both rows
		//
		// Those rules live with the other same-file alias rules
		// (attachedWriteSessionFor, noteAttachedTouched, refreshAttachedWriteReaders,
		// attachedCommitLockCheck); fts5misc.test 14.0.
		acquired, aerr := memdbAcquire(memdbName)
		if aerr != nil {
			return aerr
		}
		ad.path = acquired
		ad.isMemdb = true
		ad.memdbName = memdbName
	} else if sharedName, isSharedMem := IsSharedMemAttachName(path); isSharedMem {
		// A second ATTACH of one cache=shared name in one connection is an
		// error in C: sqlite3BtreeOpen refuses the same BtShared twice
		// (btree.c:2631-2640):
		//
		//	for(iDb=db->nDb-1; iDb>=0; iDb--){
		//	  Btree *pExisting = db->aDb[iDb].pBt;
		//	  if( pExisting && pExisting->pBt==pBt ){
		//	    ... return SQLITE_CONSTRAINT;
		//
		// and attachFunc reports it (attach.c:200-202):
		//
		//	if( rc==SQLITE_CONSTRAINT ){
		//	  rc = SQLITE_ERROR;
		//	  zErrDyn = sqlite3MPrintf(db, "database is already attached");
		for _, a := range db.attached {
			if a.isSharedMem && a.sharedMemName == sharedName {
				return errors.New("engine: database is already attached")
			}
		}
		acquired, aerr := memdbAcquire(sharedMemRegistryKey(sharedName))
		if aerr != nil {
			return aerr
		}
		ad.path = acquired
		ad.isSharedMem = true
		ad.sharedMemName = sharedName
	} else if path == "" || equalFoldName(path, ":memory:") {
		// A private, non-durable database. A temp file (under os.TempDir(), so
		// never the source tree) reproduces every OBSERVABLE property -- this
		// session sees it, nothing else can name it, DETACH throws it away --
		// while reusing the ordinary file machinery. Mirrors driver's own
		// ':memory:' attachment (driver/attach.go).
		f, ferr := os.CreateTemp("", "musql-attach-mem-*.db")
		if ferr != nil {
			return fmt.Errorf("engine: ATTACH ':memory:': %w", ferr)
		}
		f.Close()
		ad.path = f.Name()
		ad.isMem = true
	}
	// Attaching this session's OWN file is refused. C SQLite allows it (a
	// second handle onto the same file, with its own read transaction), but this
	// engine holds the whole database in memory and rewrites the file at Close,
	// so an attached reader of it would serve the PRE-session content under a
	// name the caller reasonably expects to track main -- and, worse, the
	// empty-file materialization below would truncate a file this very session
	// is about to rewrite.
	if samePath(ad.path, db.path) {
		ad.cleanupOnFailedAttach()
		return fmt.Errorf("engine: ATTACH: cannot attach this session's own database file (%s)", ad.path)
	}
	// Aliasing a file this session already wrote is accepted, as in C:
	// attachFunc always calls sqlite3BtreeOpen (attach.c:195), whose
	// same-file dedup only applies with SQLITE_OPEN_SHAREDCACHE
	// (btree.c:2595), off by default (global.c:265, main.c:3392-3396), so
	// the second ATTACH gets an independent pager (btree.c:2662, 2682). It
	// reads what the first alias wrote; conflicts surface only when both
	// write in one transaction (attachedWriteSessionFor; attach.test 9.1-9.3).
	// The new alias's pager is refreshed from a sibling's live session at
	// the end of this function (refreshAttachedWriteReaders).
	ferr := ensureDatabaseFile(ad.path, int(db.pageSize), db.encoding())
	if ferr != nil {
		ad.cleanupOnFailedAttach()
		return fmt.Errorf("engine: ATTACH: %w", ferr)
	}
	// The ENGINE's statement path, which the mined corpus runs through; the
	// DRIVER's own ATTACH opens held sessions.
	rp, oerr := openAttachedRead(ad.path)
	if oerr != nil {
		ad.cleanupOnFailedAttach()
		return fmt.Errorf("engine: ATTACH: %w", oerr)
	}
	// Every database on a connection must share a text encoding: "attached
	// databases must use the same text encoding as main database"
	// (attach2.test section 5), since BINARY comparison order differs
	// between encodings (utf16.go). An empty database has no encoding yet
	// and cannot disagree, as in C (sqlite3ReadSchema leaves ENC(db) alone
	// for an empty file); "has content" is the catalog's encoding field
	// being declared (ConvertedCatalog.Encoding).
	if rp.segEncodingDeclared && rp.encoding() != db.encoding() {
		rp.Close()
		ad.cleanupOnFailedAttach()
		return fmt.Errorf("engine: attached databases must use the same text encoding as main database")
	}
	// The pager must answer to the qualifier it was attached under, so a
	// reference written as "<name>.t" resolves on it (see itemOwner).
	rp.SetLocalSchema(ad.name)
	ad.pager = rp
	db.attached = append(db.attached, ad)
	// A database attached while the connection's default locking mode is
	// EXCLUSIVE inherits it: attachFunc seeds the new pager from
	// db->dfltLockMode (attach.c:215), not main's current mode, hence
	// db.lockingDefault rather than db.lockingMain (they differ after
	// "PRAGMA main.locking_mode=normal").
	//
	// sqlite3PagerLockingMode (pager.c:7374-7384) only sets a flag; the lock
	// appears at the next acquire. attachFunc then reads the schema
	// (sqlite3Init), taking SHARED, which exclusive mode never releases
	// (pager.c:5228/5405), matching holdLockingModeLock's persistent lock.
	if db.lockingDefault {
		w, werr := db.attachedWriteSession(ad.name)
		if werr != nil {
			// Undo the append above, mirroring this function's other
			// failure-path cleanup: a failed ATTACH must leave db.attached
			// exactly as it was before this call. ad.pager (rp above) is
			// already open at this point -- unlike every EARLIER failure
			// branch in this function, which all run before "ad.pager = rp"
			// -- so it needs an explicit Close here too, the same as the
			// text-encoding mismatch check just above does.
			ad.pager.Close()
			db.attached = db.attached[:len(db.attached)-1]
			ad.cleanupOnFailedAttach()
			return werr
		}
		w.lockingMain = true
		if lerr := w.holdLockingModeLock(); lerr != nil {
			// Another connection already holds a conflicting lock on the
			// newly-attached file. C SQLite would surface exactly this
			// BUSY conflict here too -- sqlite3Init's own schema read is what
			// actually takes the lock (see this block's citation above) -- so
			// this is a clean, honest decline rather than an ATTACH that
			// silently reports "exclusive" without ever holding the lock that
			// claims. w (== ad.wdb, attachedWriteSession's own bookkeeping)
			// is a second, independent open handle on the same file as
			// ad.pager -- Discard releases it without writing anything,
			// mirroring Discard's own "ROLLBACK counterpart to Close" doc
			// comment, since this attachment never got far enough to matter.
			w.Discard()
			ad.pager.Close()
			db.attached = db.attached[:len(db.attached)-1]
			ad.cleanupOnFailedAttach()
			return lerr
		}
	}
	// refreshAttachedWriteReaders also covers a fresh alias of a file a
	// sibling holds a live write session on: this alias's pager sees only
	// the disk, and writes are deferred to COMMIT where C's autocommit has
	// already flushed them. attach.test 9.1 reads aux1's row through aux2
	// with no COMMIT in between.
	return db.refreshAttachedWriteReaders()
}

// execDetach runs a "DETACH [DATABASE] <name>" statement, dropping the binding
// and releasing its pager. The error texts mirror the oracle exactly (verified
// directly): "main" is a distinct "cannot detach" case, while "temp" -- which
// is never IN the attached set -- reports the ordinary "no such database".
func (db *DB) execDetach(trimmed string) error {
	name, ok, err := ParseDetachStmt(trimmed)
	if !ok {
		return fmt.Errorf("engine: DETACH: not a DETACH statement: %q", trimmed)
	}
	if err != nil {
		return err
	}
	if equalFoldName(name, "main") || equalFoldName(name, localSchemaOr(db.localSchema)) {
		return fmt.Errorf("engine: cannot detach database %s", name)
	}
	// Inside a transaction C refuses to detach a database the transaction
	// touched, reads included ("database aux is locked"); see
	// attachedDB.touchedInTxn.
	if ad := db.attachedNamed(name); db.txActive && ad != nil && ad.touchedInTxn {
		return fmt.Errorf("engine: database %s is locked", name)
	}
	for i, a := range db.attached {
		if !equalFoldName(a.name, name) {
			continue
		}
		// Commit this attachment's session first rather than drop it: a
		// touched-in-transaction attachment cannot be detached (above), so what
		// it holds is autocommit work C already committed:
		//
		//	ATTACH 'test2.db' AS aux; CREATE TABLE aux.t2(x,y);
		//	DETACH aux; ATTACH 'test2.db' AS aux; SELECT * FROM t2;
		//	  C SQLite: t2 is there
		//
		// (tkt1873.test). A ':memory:' attachment still loses its content, as
		// DETACH discards it.
		// A held locking-mode lock is released here, at DETACH, not in
		// closeWrite (called on every commit, which would drop it early) or
		// close (by then ad.wdb is nil).
		wdb := a.wdb
		if err := a.closeWrite(); err != nil {
			return fmt.Errorf("engine: DETACH %s: committing its writes: %w", a.name, err)
		}
		if wdb != nil {
			wdb.releaseSegmentLockingModeLock()
		}
		a.close()
		db.attached = append(db.attached[:i], db.attached[i+1:]...)
		db.refreshAttachedReaders()
		return nil
	}
	return fmt.Errorf("engine: no such database: %s", name)
}

// close releases one binding's pager and, for a ':memory:' attachment, the
// private temp file backing it -- or, for a shared "vfs=memdb" or
// "cache=shared" attachment, this session's reference on the process-wide
// registry's store, which frees its backing temp file only once every
// reference (from every attachment, in every session in this process) has
// released (see memdbRelease).
func (a *attachedDB) close() {
	if a.pager != nil {
		a.pager.Close()
		a.pager = nil
	}
	if a.isMem {
		os.Remove(a.path)
	} else if a.isMemdb {
		memdbRelease(a.memdbName)
	} else if a.isSharedMem {
		memdbRelease(sharedMemRegistryKey(a.sharedMemName))
	}
}

// cleanupOnFailedAttach undoes whatever ad.path/ad.isMem/ad.isMemdb/
// ad.isSharedMem above already allocated, for an ATTACH that fails AFTER that
// allocation but before the binding is ever added to db.attached (so close()
// -- which walks db.attached -- will never reach it). Same split as close()
// itself, for the same reason.
func (ad *attachedDB) cleanupOnFailedAttach() {
	if ad.isMem {
		os.Remove(ad.path)
	} else if ad.isMemdb {
		memdbRelease(ad.memdbName)
	} else if ad.isSharedMem {
		memdbRelease(sharedMemRegistryKey(ad.sharedMemName))
	}
}

// refreshAttachedReaders rebuilds db.attachedReaders from db.attached, in
// attach order (which is the search order an unqualified name follows after
// this session's own database -- see itemOwner). Called after every ATTACH and
// DETACH; SnapshotPager copies the result onto each read it materializes.
func (db *DB) refreshAttachedReaders() {
	// A FRESH slice, never a truncate-and-refill: SnapshotPager hands the same
	// backing array to every read it materializes, so rewriting it in place
	// would mutate a pager an outstanding read still holds.
	readers := make([]attachedReader, 0, len(db.attached))
	for _, a := range db.attached {
		readers = append(readers, attachedReader{
			name:  r33sFoldIdent(a.name),
			pager: a.pager,

			// ...and what "PRAGMA database_list" reports for it
			// (pragma_database_list.go). A memory-backed attachment has a real
			// temp file underneath but no file to REPORT, exactly as real
			// SQLite reports "" for ATTACH ':memory:'.
			path:     a.path,
			inMemory: a.isMem || a.isMemdb || a.isSharedMem,
		})
	}
	db.attachedReaders = readers

	// Drop every cached write Program. A compiled program bakes the db-INDEX of
	// each foreign FROM item into its opcodes (dbIndexOf, cross_db.go), and
	// those indexes are POSITIONS in the very slice just rebuilt: after a DETACH
	// one can name a database that is gone, or a different one that shifted down
	// into its slot. No shape compiled TODAY puts such an index in a cacheable
	// program (a write program with a subquery carries a WritePager and is never
	// cached at all -- see cachedWriteProgram), so this is currently belt and
	// braces; it is here because cachedWriteProgram's three existing guards --
	// schemaCookie, txGen, WritePager -- have no way whatsoever to observe the
	// attached set changing, and this cache has already produced wrong answers
	// twice for exactly that reason.
	db.writePlans = nil
}

// closeAttached releases every binding this session's own ATTACH statements
// created. Called from Close and Discard; a driver-owned session has no
// bindings of its own and this is a no-op there (see DB.attached).
func (db *DB) closeAttached() {
	for _, a := range db.attached {
		a.close()
	}
	db.attached = nil
	db.attachedReaders = nil
}

// attachedNamed returns this session's ATTACH binding called q
// (case-insensitive), or nil. Used by checkWriteSchemaQualifier to tell a
// qualifier naming a REAL, attached database (a cross-database write, declined
// as unsupported) apart from one naming nothing at all.
func (db *DB) attachedNamed(q string) *attachedDB {
	for _, a := range db.attached {
		if equalFoldName(a.name, q) {
			return a
		}
	}
	return nil
}

// AttachedJournalModeForTest is the journal mode the named attachment's own
// session will commit under ("" for no such attachment) -- what "PRAGMA
// aux.journal_mode" answers through the driver, for an engine-direct test.
func (n *Session) AttachedJournalModeForTest(name string) string {
	if a := n.attachedNamed(name); a != nil {
		return a.journalMode
	}
	return ""
}

// checkDropSchemaQualifier declines a DROP TABLE/VIEW/INDEX/TRIGGER whose
// qualifier names an ATTACHed database. DROP resolves an unknown qualifier as
// "not found" (dropQualifiedName, as C does), which with IF EXISTS becomes a
// silent success; for an attached database that would claim a table was
// dropped that was not. display is the "schema.name" text, unknownSchema
// dropQualifiedName's fourth result. A no-op with nothing attached.
func (db *DB) checkDropSchemaQualifier(display string, unknownSchema bool) error {
	if !unknownSchema || len(db.attached) == 0 {
		return nil
	}
	q, _, ok := strings.Cut(display, ".")
	if !ok || db.attachedNamed(q) == nil {
		return nil
	}
	return fmt.Errorf("engine: cannot drop %s: dropping an object in ATTACHed database %s is not supported by this write path", display, q)
}

// samePath reports whether a and b name the same file, comparing them as
// absolute cleaned paths. It is deliberately syntactic (no stat, no symlink
// resolution): its one caller only needs to catch the ordinary "attach the file
// I am already writing" case, and a false negative there is no worse than not
// checking at all.
func samePath(a, b string) bool {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

// ensureDatabaseFile creates path as an empty (zero-byte) file if it is
// missing or empty, the state C leaves an ATTACH of a nonexistent path in. An
// existing non-empty file is untouched. pageSize and enc are not stamped:
// there is no header yet, and the first write transaction decides both.
func ensureDatabaseFile(path string, pageSize int, enc TextEncoding) error {
	fi, err := os.Stat(path)
	if err == nil && fi.Size() > 0 {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Zero bytes, as sqlite3_open leaves a file until a write commits.
	f, ferr := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if ferr != nil {
		return ferr
	}
	return f.Close()
}

// ReferencedTables reports the database-qualified base tables a
// SELECT/INSERT/UPDATE/DELETE statement references (understood=true). For any
// other statement kind (PRAGMA, CREATE/DROP/ALTER, ATTACH/DETACH, ...) it
// returns understood=false and no refs -- the caller routes those by other
// means (or to main). A parse error is returned as-is so the caller can decline.
func ReferencedTables(sqlText string) (refs []TableRef, understood bool, err error) {
	trimmed := strings.TrimSpace(sqlText)
	toks, lerr := lex(trimmed)
	if lerr != nil {
		return nil, false, lerr
	}
	if len(toks) == 0 || toks[0].kind != tkIdent {
		return nil, false, nil
	}
	kw := toks[0].upper()
	if kw == "WITH" {
		if verb, ok := verbAfterLeadingWith(trimmed, toks); ok {
			kw = verb
		}
	}
	var out []TableRef
	switch kw {
	case "SELECT":
		stmt, e := ParseSelect(trimmed)
		if e != nil {
			return nil, true, e
		}
		collectSelectRefs(stmt, map[string]bool{}, &out)
	case "INSERT", "REPLACE":
		stmt, e := parseInsertStmt(trimmed)
		if e != nil {
			return nil, true, e
		}
		out = append(out, TableRef{Schema: stmt.schema, Table: stmt.table})
		if stmt.selectStmt != nil {
			collectSelectRefs(stmt.selectStmt, map[string]bool{}, &out)
		}
		for _, row := range stmt.rows {
			for _, e := range row {
				collectExprSubqueryRefs(e, &out)
			}
		}
	case "UPDATE":
		stmt, e := parseUpdateStmt(trimmed)
		if e != nil {
			return nil, true, e
		}
		out = append(out, TableRef{Schema: stmt.schema, Table: stmt.table})
		cte := map[string]bool{}
		for i := range stmt.from {
			collectFromItemRefs(&stmt.from[i], cte, &out)
		}
		for _, a := range stmt.sets {
			collectExprSubqueryRefs(a.expr, &out)
		}
		collectExprSubqueryRefs(stmt.where, &out)
	case "DELETE":
		stmt, e := parseDeleteStmt(trimmed)
		if e != nil {
			return nil, true, e
		}
		out = append(out, TableRef{Schema: stmt.schema, Table: stmt.table})
		collectExprSubqueryRefs(stmt.where, &out)
	default:
		return nil, false, nil
	}
	return out, true, nil
}

// collectSelectRefs appends every base-table reference in a SELECT (recursively
// through its FROM items' subqueries, compound arms, CTE bodies, and any
// subquery embedded in an expression) to *out. cteNames holds the WITH-defined
// names in scope; an UNqualified FROM item naming one of them is a CTE
// reference, not a base table, and is skipped (a qualified reference never is).
func collectSelectRefs(stmt *SelectStmt, cteNames map[string]bool, out *[]TableRef) {
	if stmt == nil {
		return
	}
	for _, cte := range stmt.CTEs {
		cteNames[r33sFoldIdent(cte.Name)] = true
	}
	for _, cte := range stmt.CTEs {
		collectSelectRefs(cte.Select, cteNames, out)
	}
	for i := range stmt.From {
		collectFromItemRefs(&stmt.From[i], cteNames, out)
	}
	for _, c := range stmt.Columns {
		collectExprSubqueryRefs(c.Expr, out)
	}
	collectExprSubqueryRefs(stmt.Where, out)
	for _, g := range stmt.GroupBy {
		collectExprSubqueryRefs(g, out)
	}
	collectExprSubqueryRefs(stmt.Having, out)
	for _, ot := range stmt.OrderBy {
		collectExprSubqueryRefs(ot.Expr, out)
	}
	for _, arm := range stmt.Compound {
		collectSelectRefs(arm.Stmt, cteNames, out)
	}
}

func collectFromItemRefs(it *FromItem, cteNames map[string]bool, out *[]TableRef) {
	if it.Subquery != nil {
		collectSelectRefs(it.Subquery, cteNames, out)
		return
	}
	collectExprSubqueryRefs(it.On, out)
	if it.Table == "" {
		return
	}
	if it.Schema == "" && cteNames[r33sFoldIdent(it.Table)] {
		return // a CTE reference, not a base table
	}
	*out = append(*out, TableRef{Schema: it.Schema, Table: it.Table})
}

// collectExprSubqueryRefs descends an expression tree only far enough to reach
// any embedded subquery (SubqueryExpr, ExistsExpr, InExpr.Sub) and collect the
// base tables IT references -- a subquery can name a different database, which
// the caller must see to detect a cross-database statement. (An expression's
// own column references never name a database of their own; see this file's
// package comment.) It mirrors exprContainsSubquery's structure so no
// subquery-bearing expression form is missed.
func collectExprSubqueryRefs(e Expr, out *[]TableRef) {
	switch x := e.(type) {
	case nil:
		return
	case UnaryExpr:
		collectExprSubqueryRefs(x.X, out)
	case BinaryExpr:
		collectExprSubqueryRefs(x.L, out)
		collectExprSubqueryRefs(x.R, out)
	case IsNullExpr:
		collectExprSubqueryRefs(x.X, out)
	case InExpr:
		collectExprSubqueryRefs(x.X, out)
		for _, it := range x.List {
			collectExprSubqueryRefs(it, out)
		}
		collectSelectRefs(x.Sub, map[string]bool{}, out)
	case BetweenExpr:
		collectExprSubqueryRefs(x.X, out)
		collectExprSubqueryRefs(x.Lo, out)
		collectExprSubqueryRefs(x.Hi, out)
	case LikeExpr:
		collectExprSubqueryRefs(x.X, out)
		collectExprSubqueryRefs(x.Pattern, out)
		collectExprSubqueryRefs(x.Escape, out)
	case GlobExpr:
		collectExprSubqueryRefs(x.X, out)
		collectExprSubqueryRefs(x.Pattern, out)
	case CollateExpr:
		collectExprSubqueryRefs(x.X, out)
	case CastExpr:
		collectExprSubqueryRefs(x.X, out)
	case FuncExpr:
		for _, a := range x.Args {
			collectExprSubqueryRefs(a, out)
		}
		collectExprSubqueryRefs(x.Filter, out)
		for _, ob := range x.orderByExprs() {
			collectExprSubqueryRefs(ob, out)
		}
	case RowExpr:
		for _, el := range x.Elems {
			collectExprSubqueryRefs(el, out)
		}
	case CaseExpr:
		collectExprSubqueryRefs(x.Base, out)
		for _, w := range x.Whens {
			collectExprSubqueryRefs(w.When, out)
			collectExprSubqueryRefs(w.Then, out)
		}
		collectExprSubqueryRefs(x.Else, out)
	case SubqueryExpr:
		collectSelectRefs(x.Stmt, map[string]bool{}, out)
	case ExistsExpr:
		collectSelectRefs(x.Stmt, map[string]bool{}, out)
	case RaiseExpr:
		collectExprSubqueryRefs(x.Msg, out)
	default:
		return
	}
}

// ddlLeadingKeywords are the keyword tokens that can precede a CREATE / DROP /
// ALTER statement's TARGET object name (before its optional "schema." prefix).
var ddlLeadingKeywords = map[string]bool{
	"CREATE": true, "TEMP": true, "TEMPORARY": true, "UNIQUE": true,
	"TABLE": true, "INDEX": true, "VIEW": true, "TRIGGER": true,
	"DROP": true, "ALTER": true, "IF": true, "NOT": true, "EXISTS": true,
	"DATABASE": true, "VIRTUAL": true,
}

// StatementTargetSchema reports the database qualifier of a CREATE / DROP /
// ALTER statement's target object ("aux" for "CREATE TABLE aux.t (...)",
// "CREATE INDEX aux.i ON t(a)", "DROP TABLE aux.t", "ALTER TABLE aux.t ..."),
// or "" for an unqualified target. isDDL is true only for a CREATE/DROP/ALTER
// statement; false (schema "") for anything else. It is a deliberately small
// token scan -- the driver Conn uses it only to pick which attached
// database's file a DDL statement runs against; the engine on that file then
// validates the qualifier and the statement body as usual.
func StatementTargetSchema(sqlText string) (schema string, isDDL bool) {
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil || len(toks) == 0 || toks[0].kind != tkIdent {
		return "", false
	}
	switch toks[0].upper() {
	case "CREATE", "DROP", "ALTER":
	default:
		return "", false
	}
	// Skip leading keyword tokens; the first non-keyword identifier is the
	// target name. If it is immediately followed by ".", that identifier was
	// the schema qualifier.
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tkIdent {
			return "", true
		}
		if ddlLeadingKeywords[t.upper()] && !t.quoted {
			continue
		}
		// t is the target name (or its schema qualifier).
		if i+1 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." {
			return t.text, true
		}
		return "", true
	}
	return "", true
}

// StripDDLTargetSchema removes the database qualifier from a CREATE / DROP /
// ALTER statement's target object name ("CREATE TABLE aux.t (...)" ->
// "CREATE TABLE t (...)"), returning the rewritten SQL and changed=true. It is
// used by the driver Conn AFTER it has routed a DDL statement to an
// ATTACHed database's file: the engine's single-file DDL parsers accept only an
// unqualified (or "main"/"temp") target, so once the owning file is chosen the
// qualifier is redundant and is removed, leaving the exact same statement the
// engine would run to create the object in its own single database. The removal
// is a byte-exact splice using the tokens' source offsets, so nothing else in
// the statement text (comments, spacing, the body) changes. changed=false (and
// the original text) if there is no target qualifier to strip.
func StripDDLTargetSchema(sqlText string) (rewritten string, changed bool) {
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil || len(toks) == 0 || toks[0].kind != tkIdent {
		return sqlText, false
	}
	switch toks[0].upper() {
	case "CREATE", "DROP", "ALTER":
	default:
		return sqlText, false
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tkIdent {
			return sqlText, false
		}
		if ddlLeadingKeywords[t.upper()] && !t.quoted {
			continue
		}
		if i+1 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." {
			// Splice out the source bytes of the "<schema>." prefix: from the
			// start of the schema identifier up to (and including) the dot.
			dot := toks[i+1]
			return sqlText[:t.Start] + sqlText[dot.End:], true
		}
		return sqlText, false
	}
	return sqlText, false
}

// TableNames returns the names of every ordinary table in this database's
// schema catalog (type == "table"), used by the driver Conn to resolve an
// unqualified table name to the first database, in SQLite's search order, that
// actually contains it. sqlite_-prefixed internal tables are included as they
// are in the catalog; the caller matches case-insensitively.
func (p *ReadOnlyPager) TableNames() ([]string, error) {
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rows {
		if r.Type == "table" {
			names = append(names, r.Name)
		}
	}
	return names, nil
}

// SetAttachedReadersForTest wires src's ATTACH bindings onto p, so a test can
// run a cross-database read against a snapshot the way the driver Conn does
// (SetAttachedReaders). Test-only seam.
func (p *ReadOnlyPager) SetAttachedReadersForTest(src *Session) {
	p.attachedReaders = src.attachedReaders
}

// openAttachedRead opens a database file for ATTACH. Like every open, it reads
// this engine's own format and nothing else.
func openAttachedRead(path string) (*ReadOnlyPager, error) {
	// A MISSING or ZERO-LENGTH file is an empty database, and it has to be read
	// as one here rather than left to Open: ensureDatabaseFile creates a brand-new
	// attachment at zero bytes on purpose (C SQLite writes nothing until the first
	// commit), so "ATTACH 'newfile' AS aux" arrives with no header of any format to
	// recognise.
	if fi, serr := os.Stat(path); serr != nil || fi.Size() == 0 {
		if serr != nil && !os.IsNotExist(serr) {
			return nil, serr
		}
		return newReadOnlyPager(), nil
	}
	return Open(path)
}


