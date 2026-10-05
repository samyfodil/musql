// This file mines SQL statements from SQLite's TCL test suite (vendored under testdata/tcl/)
// and replays them in lockstep order against the Go engine and C SQLite, comparing results.
// Statements are segmented at reset_db/reconnect boundaries to match original test isolation.
// SQL is extracted from execsql, do_execsql_test, catchsql, and db eval constructs, skipping
// those with TCL substitutions ($-variables, [ ] command substitution). Queries (SELECT/WITH/VALUES)
// are run via SnapshotPager+QueryArgs; everything else via ExecArgs. Errors are classified as
// accepted (""), constraint violations, or unsupported. Panics are caught and reported separately.
package compat

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// tclCorpusDir holds SQLite's test suite, vendored in full (all 1474 *.test files,
// with no exclusions).
const tclCorpusDir = "testdata/tcl"

// tclFTS5ModuleRegex matches a CREATE VIRTUAL TABLE naming the fts5 module (or
// its fts5vocab companion). A corpus file containing one can only be replayed
// meaningfully when the CGo oracle was built with fts5 -- see
// fts5_disabled_test.go for the two-build arrangement.
var tclFTS5ModuleRegex = regexp.MustCompile(`(?i)\busing\s+fts5(vocab)?\b`)

// tclMaxStmtsPerFile bounds a single (pathological) file's statement count so
// one huge file can't blow up total runtime; essentially never hit by the
// vendored corpus (the largest mined files run a few hundred statements).
const tclMaxStmtsPerFile = 5000

// tclShortFileCount is how many (sorted, so deterministic) vendored files
// TestTCLCorpus exercises under `go test -short`.
const tclShortFileCount = 25

// tclKeywordRegex matches the four SQL-embedding TCL constructs this miner
// understands, as whole words/phrases -- \b keeps "execsql" from matching
// inside "do_execsql_test" (word-constituent '_' on both sides there defeats
// the boundary, so the two never double-match the same occurrence).
var tclKeywordRegex = regexp.MustCompile(`\bdo_execsql_test\b|\bexecsql\b|\bcatchsql\b|\bdb\s+eval\b`)

func isTCLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// findNextBraceGroup finds the next TCL brace-quoted {...} group, skipping up to three
// leading bareword tokens (for test names or "-db dbname" pairs). Braces are matched with
// nesting depth and backslash-escape awareness. Returns (block, ok, end) where end is the
// position immediately after the closing brace.
func findNextBraceGroup(src string, pos int) (block string, ok bool, end int) {
	n := len(src)
	for tokensSkipped := 0; tokensSkipped <= 2; tokensSkipped++ {
		for pos < n && isTCLSpace(src[pos]) {
			pos++
		}
		if pos >= n {
			return "", false, 0
		}
		if src[pos] == '{' {
			depth := 0
			start := pos + 1
			i := pos
			for i < n {
				switch src[i] {
				case '\\':
					i += 2
					continue
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						return src[start:i], true, i + 1
					}
				}
				i++
			}
			return "", false, 0 // unbalanced -- bail rather than guess
		}
		if src[pos] == '$' || src[pos] == '[' {
			return "", false, 0
		}
		for pos < n && !isTCLSpace(src[pos]) {
			pos++
		}
	}
	return "", false, 0
}

// tclDisqualified reports whether a mined block depends on TCL-side
// evaluation ($ variable substitution, [ ] command substitution) rather than
// being statically-complete SQL text -- see the package doc comment's mining
// section for why such blocks are dropped outright instead of partially
// used.
func tclDisqualified(block string) bool {
	return strings.ContainsAny(block, "$[]")
}

// tclCommentedOut reports whether the keyword match sits on a line whose first
// non-blank character is '#' (a TCL comment).
func tclCommentedOut(src string, pos int) bool {
	start := strings.LastIndexAny(src[:pos], "\n\r") + 1
	for i := start; i < pos; i++ {
		switch src[i] {
		case ' ', '\t':
			continue
		case '#':
			return true
		default:
			return false
		}
	}
	return false
}

// tclBareTxnVerbs is the set of statement keywords recognized as bare (unbraced) word
// arguments to execsql/catchsql/db-eval (BEGIN, COMMIT, ROLLBACK, END, VACUUM, ANALYZE, REINDEX).
var tclBareTxnVerbs = map[string]bool{
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "END": true,
	"VACUUM": true, "ANALYZE": true, "REINDEX": true,
}

// tclBareStmtArg extracts a bare or "-quoted single-word argument after an
// execsql/catchsql/db-eval keyword match, returning the word and position after it.
func tclBareStmtArg(src string, pos int) (word string, after int, ok bool) {
	n := len(src)
	for pos < n && isTCLSpace(src[pos]) {
		pos++
	}
	if pos >= n {
		return "", 0, false
	}
	switch src[pos] {
	case '{', '$', '[':
		return "", 0, false
	case '"':
		end := strings.IndexByte(src[pos+1:], '"')
		if end < 0 {
			return "", 0, false
		}
		return src[pos+1 : pos+1+end], pos + 1 + end + 1, true
	}
	start := pos
	for pos < n && !isTCLSpace(src[pos]) && src[pos] != '}' {
		pos++
	}
	return src[start:pos], pos, true
}

// tclBareStmtConnArg looks for the optional SECOND word execsql/catchsql
// accept -- "execsql {SQL} ?DB?" -- immediately following a mined bare
// argument that ended at pos, without ever crossing a newline: in every
// occurrence in this corpus the two arguments sit on the same source line,
// and reaching a newline (or the '}'/';' that closes the enclosing TCL
// construct) first means there is no second argument at all, not one on a
// later line. Only horizontal whitespace is skipped (deliberately not
// isTCLSpace, which would also cross the newline) so this stays a same-line
// question.
func tclBareStmtConnArg(src string, pos int) (name string, ok bool) {
	n := len(src)
	for pos < n && (src[pos] == ' ' || src[pos] == '\t') {
		pos++
	}
	if pos >= n {
		return "", false
	}
	switch src[pos] {
	case '\n', '\r', '}', ';', '{', '$', '[':
		return "", false
	}
	start := pos
	for pos < n && !isTCLSpace(src[pos]) && src[pos] != '}' && src[pos] != ';' {
		pos++
	}
	return src[start:pos], pos > start
}

// tclBareTxnStmt recognizes bare transaction keywords (from tclBareTxnVerbs) and ensures
// they target the "db" connection, not a different one. Drops statements targeting other connections.
func tclBareTxnStmt(src string, pos int) (stmt string, ok bool) {
	word, after, ok := tclBareStmtArg(src, pos)
	if !ok {
		return "", false
	}
	verb := strings.ToUpper(strings.TrimSuffix(word, ";"))
	if !tclBareTxnVerbs[verb] {
		return "", false
	}
	if conn, has := tclBareStmtConnArg(src, after); has && !strings.EqualFold(strings.TrimSuffix(conn, ";"), "db") {
		return "", false // names a DIFFERENT connection -- see doc comment above
	}
	return verb, true
}

// tclDoExecsqlTestSkipLabel skips do_execsql_test's TCL-substituted test-name argument
// (e.g. "$tn.1.0"), returning the position after it.
func tclDoExecsqlTestSkipLabel(src string, pos int) (after int, ok bool) {
	n := len(src)
	for pos < n && isTCLSpace(src[pos]) {
		pos++
	}
	if pos >= n || (src[pos] != '$' && src[pos] != '[') {
		return 0, false
	}
	for pos < n && !isTCLSpace(src[pos]) {
		pos++
	}
	return pos, true
}

// tclKeywordNamesConnArg reports whether kw can have a trailing connection argument.
// Only true for execsql and catchsql, not do_execsql_test or db eval.
func tclKeywordNamesConnArg(kw string) bool {
	return kw == "execsql" || kw == "catchsql"
}

// tclBraceStmtConnArg extracts a connection name from execsql/catchsql's trailing
// argument after a braced SQL block, rejecting constructs with unresolved TCL substitutions.
func tclBraceStmtConnArg(src string, pos int) (name string, ok bool) {
	n := len(src)
	for pos < n && (src[pos] == ' ' || src[pos] == '\t') {
		pos++
	}
	if pos >= n {
		return "", false
	}
	switch src[pos] {
	case '\n', '\r', '}', ']', ';':
		return "", false
	case '$', '[':
		return "\x00unresolved\x00", true // never equals "db" -- see doc comment
	case '"':
		line := src[pos+1:]
		if nl := strings.IndexAny(line, "\n\r"); nl >= 0 {
			line = line[:nl]
		}
		if end := strings.IndexByte(line, '"'); end >= 0 {
			return line[:end], true
		}
		return "\x00unresolved\x00", true // no close quote on this line -- see doc comment
	}
	start := pos
	for pos < n && !isTCLSpace(src[pos]) && src[pos] != '}' && src[pos] != ']' && src[pos] != ';' {
		pos++
	}
	return src[start:pos], pos > start
}

// tclMinedBlock is a mined brace group with its statements split into individual queries,
// tagged with the byte offset where the keyword match started.
type tclMinedBlock struct {
	pos   int
	stmts []string
}

// extractTCLSQL mines SQL statements from TCL test source in file order as a flat list.
func extractTCLSQL(src string) []string {
	var out []string
	for _, b := range extractTCLBlocks(src) {
		out = append(out, b.stmts...)
	}
	return out
}

// tclHasBindParams reports whether stmt uses SQL bind parameters. Parameterized statements
// run with no bound values aren't a fair SQL-correctness test, so they are dropped.
func tclHasBindParams(stmt string) bool {
	info, err := engine.ParseParamInfo(stmt)
	return err == nil && info.NumParams > 0
}

// extractTCLBlocks is extractTCLSQL's position-tagged form.
func extractTCLBlocks(src string) []tclMinedBlock {
	var out []tclMinedBlock
	for _, m := range tclKeywordRegex.FindAllStringIndex(src, -1) {
		kw := src[m[0]:m[1]]
		if tclCommentedOut(src, m[0]) {
			continue // a TCL comment -- this SQL never ran; see tclCommentedOut
		}
		// do_execsql_test's own grammar (name, then {SQL}, then {expected})
		// leads with a NAME, not the SQL itself -- tclBareTxnStmt (which reads
		// its first token as the STATEMENT) is scoped to the other three
		// constructs, whose first argument really is the SQL.
		var block string
		var ok bool
		if kw != "do_execsql_test" {
			block, ok = tclBareTxnStmt(src, m[1])
		}
		if !ok {
			var end int
			block, ok, end = findNextBraceGroup(src, m[1])
			// The braced form takes the same optional different-connection
			// argument the bare form's tclBareTxnStmt already checks above --
			// findNextBraceGroup itself only ever looks at the brace group,
			// never at what follows it. See tclBraceStmtConnArg's doc comment
			// for the full trailing-token grammar and tclKeywordNamesConnArg
			// for why this is scoped to exactly execsql/catchsql.
			if ok && tclKeywordNamesConnArg(kw) {
				if conn, has := tclBraceStmtConnArg(src, end); has && !strings.EqualFold(conn, "db") {
					ok = false // names a DIFFERENT connection -- drop, don't mis-mine
				}
			}
		}
		if !ok && kw == "do_execsql_test" {
			// The straightforward attempt above failed to find the SQL -- which
			// findNextBraceGroup's shared, 3-token lookahead also does
			// (correctly) for do_execsql_test's "-db dbname" different-
			// connection form. Only retry the ONE case that lookahead gets
			// wrong: a "$"/"["-substituted test-name LABEL, which is never SQL
			// text and must be skipped unconditionally rather than
			// disqualifying the call outright. See tclDoExecsqlTestSkipLabel's
			// doc comment.
			if after, has := tclDoExecsqlTestSkipLabel(src, m[1]); has {
				block, ok = tclBareTxnStmt(src, after)
				if !ok {
					block, ok, _ = findNextBraceGroup(src, after)
				}
			}
		}
		if !ok || tclDisqualified(block) {
			continue
		}
		var stmts []string
		for _, stmt := range splitTopLevelStatements(strings.TrimSpace(block)) {
			if stmt = strings.TrimSpace(stmt); stmt != "" && !tclHasBindParams(stmt) {
				stmts = append(stmts, stmt)
			}
		}
		if len(stmts) > 0 {
			out = append(out, tclMinedBlock{pos: m[0], stmts: stmts})
		}
	}
	return out
}

// tclResetMarkerRegex matches TCL idioms that reset the database (reset_db, forcedelete,
// sqlite3 db reconnect, db close, faultsim_delete_and_reopen).
var tclResetMarkerRegex = regexp.MustCompile(`(?m)^[ \t]*(?:reset_db\b|forcedelete[ \t]|sqlite3[ \t]+db[ \t]|db[ \t]+close\b|faultsim_delete_and_reopen\b)`)

// tclSegments splits mined statements into independent sequences at reset markers
// (reset_db, reconnect, db close, forcedelete, faultsim_delete_and_reopen).
func tclSegments(src string) [][]string {
	resets := tclResetMarkerRegex.FindAllStringIndex(src, -1)
	blocks := extractTCLBlocks(src)

	var segments [][]string
	var cur []string
	ri := 0
	for _, b := range blocks {
		for ri < len(resets) && resets[ri][0] < b.pos {
			if len(cur) > 0 {
				segments = append(segments, cur)
				cur = nil
			}
			ri++
		}
		for _, stmt := range b.stmts {
			cur = append(cur, stmt)
		}
	}
	if len(cur) > 0 {
		segments = append(segments, cur)
	}
	return segments
}

// tclIsQuery classifies a statement as a SELECT-shaped read (SELECT/WITH/VALUES)
// versus everything else.
func tclIsQuery(stmt string) bool {
	verb, ok := engine.LeadingStatementVerb(stmt)
	if !ok {
		// Doesn't lex at all: fall back to the raw leading whitespace-delimited
		// token, exactly as this function always did.
		fields := strings.Fields(stmt)
		if len(fields) == 0 {
			return false
		}
		verb = strings.ToUpper(fields[0])
	}
	switch verb {
	case "SELECT", "VALUES", "WITH":
		// "WITH" only survives LeadingStatementVerb when the CTE list itself
		// couldn't be parsed past; treat it as a query, as before.
		return true
	}
	return false
}

var (
	tclReQuoted = regexp.MustCompile(`"[^"]*"|'[^']*'|` + "`[^`]*`")
	tclReNumber = regexp.MustCompile(`\b[0-9]+\b`)

	// tclNondeterministicFuncRegex matches a call to random() or
	// randomblob() as a whole identifier (word boundary on both sides, so
	// e.g. a column literally named "randomized" never false-matches),
	// case-insensitive. See runTCLSegment's use of tclCallsNondeterministicFunc.
	tclNondeterministicFuncRegex = regexp.MustCompile(`(?i)\brandom(blob)?\s*\(`)
)

// tclCallsNondeterministicFunc reports whether stmt calls random() or randomblob().
func tclCallsNondeterministicFunc(stmt string) bool {
	return tclNondeterministicFuncRegex.MatchString(stmt)
}

// normalizeErrorMessage collapses an engine error message's dynamic content
// (quoted identifiers/literals, bare numbers) into placeholders so that e.g.
// "unsupported CAST target type \"DATE\"" and "...\"BLOB\"" fall into the
// same histogram bucket, then truncates -- this is what turns a raw error
// stream into a reasonably-sized, ranked feature-gap histogram instead of one
// bucket per unique message.
func normalizeErrorMessage(msg string) string {
	m := tclReQuoted.ReplaceAllString(msg, "<X>")
	m = tclReNumber.ReplaceAllString(m, "<N>")
	if len(m) > 140 {
		m = m[:140] + "..."
	}
	return m
}

// tclLeadingKeywords buckets a statement by its leading SQL keyword(s) --
// used specifically for the exec-side dispatcher's generic "this write path
// only supports ..." rejection (insert_write.go's Exec), whose error message
// embeds the entire rejected SQL text (via %q) and so is unique per
// statement; the statement's OWN leading keyword is the actually-stable,
// actually-informative signal for that case (e.g. every rejected PRAGMA
// collapses to one "PRAGMA" bucket, not one bucket per distinct pragma
// invocation).
func tclLeadingKeywords(stmt string) string {
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return "(empty)"
	}
	first := strings.ToUpper(fields[0])
	switch first {
	case "CREATE":
		for _, f := range fields[1:] {
			switch u := strings.ToUpper(f); u {
			case "TEMP", "TEMPORARY", "UNIQUE", "IF", "NOT", "EXISTS":
				continue
			default:
				return "CREATE " + u
			}
		}
		return "CREATE"
	case "DROP":
		if len(fields) > 1 {
			return "DROP " + strings.ToUpper(fields[1])
		}
		return "DROP"
	case "ALTER":
		return "ALTER TABLE"
	default:
		return first
	}
}

// tclConstraintMessages is the allowlist of genuine, engine-agnostic constraint rejections
// (UNIQUE, NOT NULL, CHECK, FOREIGN KEY, constraint failed).
var tclConstraintMessages = []string{
	"UNIQUE constraint failed",
	"NOT NULL constraint failed",
	"CHECK constraint failed",
	"FOREIGN KEY constraint failed",
	"constraint failed",
}

// tclClassifyExecErr classifies a Go engine error as "", "constraint", or "unsupported".
func tclClassifyExecErr(err error) string {
	if err == nil {
		return ""
	}
	// RAISE(FAIL) keeps changes applied, unlike RAISE(ABORT)/(ROLLBACK).
	if engine.ErrorKeepsAutocommitChanges(err) {
		return "constraint"
	}
	msg := err.Error()
	for _, p := range tclConstraintMessages {
		if strings.Contains(msg, p) {
			return "constraint"
		}
	}
	return "unsupported"
}

// tclSavepointTracker tracks which savepoint names are currently open. If a name is
// absent from the stack, C SQLite is guaranteed to reject a RELEASE/ROLLBACK TO.
type tclSavepointTracker struct {
	open []string // stack, innermost (most recently opened) last
}

// recordAccepted updates the tracker for one accepted statement.
func (tr *tclSavepointTracker) recordAccepted(stmt string) {
	kind, name, ok := engine.SavepointStmt(stmt)
	if !ok {
		return
	}
	switch kind {
	case engine.SavepointOpen:
		tr.open = append(tr.open, name)
	case engine.SavepointRelease:
		for i := len(tr.open) - 1; i >= 0; i-- {
			if strings.EqualFold(tr.open[i], name) {
				tr.open = tr.open[:i]
				return
			}
		}
	// ROLLBACK TO leaves the savepoint open; nothing to update.
	case engine.SavepointRollbackTo:
	}
}

// isOpen reports whether name is on the tracked stack.
func (tr *tclSavepointTracker) isOpen(name string) bool {
	for _, n := range tr.open {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// tclSavepointNameMissing reports whether stmt releases or rolls back to a savepoint
// that is provably not open, which the oracle must also reject.
func tclSavepointNameMissing(savepoints *tclSavepointTracker, stmt string) bool {
	kind, name, ok := engine.SavepointStmt(stmt)
	if !ok || kind == engine.SavepointOpen {
		return false
	}
	return !savepoints.isOpen(name)
}

// tclExecProbeSafe reports whether stmt is safe to probe against the oracle inside a
// rolled-back SAVEPOINT. Transactional DML/DDL qualify; PRAGMA/VACUUM/ATTACH/DETACH/ANALYZE
// do not (their side effects persist past rollback).
func tclExecProbeSafe(stmt string) bool {
	if _, ok := engine.TxnStmtKind(stmt); ok {
		return true
	}
	if tclIsFts5ConfigInsert(stmt) {
		// fts5 config inserts leak past rollback (flag cached on connection),
		// so cannot be probed.
		return false
	}
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "PRAGMA":
		// A pragma that only READS the schema changes nothing a savepoint
		// rollback would have to undo -- no connection state, no page writes
		// -- so it is safe to ask the oracle about, unlike the rest of the
		// family. That question is worth asking: 36 mined
		// "PRAGMA foreign_key_check(tN)" statements name a table NEITHER
		// engine has, so both reject them in lockstep, and leaving them
		// unprobed booked all 36 as a coverage gap. See tclPureReadPragma.
		if tclPureReadPragma(stmt) {
			return true
		}
		// ...and a pragma whose own SPELLING is one C SQLite refuses before
		// it assigns anything is safe for the same reason: it never reaches its
		// state. See tclR32OPragmaRejectedAtPrepare for the per-name evidence
		// and for why this must stay spelling-conditional.
		if tclR32OPragmaRejectedAtPrepare(stmt) {
			return true
		}
		return tclCheckpointProbeSafe(stmt)
	case "VACUUM", "ATTACH", "DETACH", "ANALYZE":
		// Not undone by savepoint rollback.
		return false
	case "SAVEPOINT", "RELEASE":
		// Manipulate the savepoint stack itself; cannot be probed.
		return false
	default:
		return true
	}
}

// tclPureReadPragmaNames are pragmas that only read the schema without side effects.
var tclPureReadPragmaNames = map[string]bool{
	"foreign_key_check": true, "integrity_check": true, "quick_check": true,
	"table_info": true, "table_xinfo": true, "index_list": true,
	"index_info": true, "index_xinfo": true, "foreign_key_list": true,
	"compile_options": true,
}

// tclPureReadPragma reports whether stmt is a read-only pragma with an argument.
func tclPureReadPragma(stmt string) bool {
	open := strings.IndexAny(stmt, "(=")
	if open < 0 {
		return false
	}
	name := strings.TrimSpace(stmt[:open])
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "PRAGMA"))
	name = strings.TrimSpace(strings.TrimPrefix(name, "pragma"))
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:] // a schema qualifier: "main.table_info"
	}
	return tclPureReadPragmaNames[strings.ToLower(strings.Trim(name, `"[]`+"`"))]
}

// tclCheckpointProbeSafe reports whether stmt is a "PRAGMA wal_checkpoint",
// which is safe to probe despite being a pragma (checkpoint has no SQL-visible side effects).
func tclCheckpointProbeSafe(stmt string) bool {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stmt), "PRAGMA"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "pragma"))
	if dot := strings.Index(rest, "."); dot >= 0 && !strings.ContainsAny(rest[:dot], " \t=(") {
		rest = rest[dot+1:] // a schema qualifier: "main.wal_checkpoint"
	}
	name := rest
	if i := strings.IndexAny(rest, " \t=(;"); i >= 0 {
		name = rest[:i]
	}
	return strings.EqualFold(strings.Trim(name, `"[]`+"`"), "wal_checkpoint")
}

// tclR32OPragmaSplit splits a PRAGMA statement into its name and trailing text.
func tclR32OPragmaSplit(stmt string) (name, rest string, ok bool) {
	s := strings.TrimSpace(stmt)
	if len(s) < 6 || !strings.EqualFold(s[:6], "PRAGMA") {
		return "", "", false
	}
	s = strings.TrimSpace(s[6:])
	if dot := strings.Index(s, "."); dot >= 0 && !strings.ContainsAny(s[:dot], " \t=(") {
		s = s[dot+1:] // a schema qualifier: "main.encoding"
	}
	i := 0
	for i < len(s) && (s[i] == '_' || (s[i] >= 'a' && s[i] <= 'z') ||
		(s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return "", "", false
	}
	return strings.ToLower(s[:i]), strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[i:]), ";")), true
}

// tclR32OEncNames is the set of valid text encodings (UTF-8, UTF-16 variants).
var tclR32OEncNames = map[string]bool{
	"utf8": true, "utf-8": true, "utf-16le": true, "utf-16be": true,
	"utf16le": true, "utf16be": true, "utf-16": true, "utf16": true,
}

// tclR32OPragmaRejectedAtPrepare reports whether stmt is a pragma that C SQLite
// refuses at prepare time (before any assignment). Spelling-conditional for safe oracle probing.
func tclR32OPragmaRejectedAtPrepare(stmt string) bool {
	name, rest, ok := tclR32OPragmaSplit(stmt)
	if !ok {
		return false
	}
	switch name {
	case "compile_options":
		return rest != "" && rest[0] != '=' && rest[0] != '('
	case "encoding":
		v, isValue := tclR32OPragmaValue(rest)
		return isValue && !tclR32OEncNames[strings.ToLower(v)]
	case "temp_store_directory":
		v, isValue := tclR32OPragmaValue(rest)
		if !isValue || v == "" {
			return false
		}
		// Check if the path is not a directory (which causes rejection).
		st, serr := os.Stat(v)
		return serr != nil || !st.IsDir()
	}
	return false
}

// tclR32OPragmaValue extracts the value from a pragma's "= v" or "(v)" tail, unquoted.
func tclR32OPragmaValue(rest string) (value string, isValue bool) {
	switch {
	case strings.HasPrefix(rest, "="):
		value = strings.TrimSpace(rest[1:])
	case strings.HasPrefix(rest, "("):
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest[1:]), ")"))
	default:
		return "", false
	}
	if len(value) >= 2 {
		if q := value[0]; (q == '\'' || q == '"' || q == '`') && value[len(value)-1] == q {
			value = value[1 : len(value)-1]
		} else if q == '[' && value[len(value)-1] == ']' {
			value = value[1 : len(value)-1]
		}
	}
	return value, true
}

// tclR32OOracleRefusesSyncInTxn checks whether the oracle rejects "PRAGMA synchronous"
// due to an active transaction (outside tclR32OPragmaRejectedAtPrepare's scope).
func tclR32OOracleRefusesSyncInTxn(cgodb *sql.DB, stmt string) bool {
	name, rest, ok := tclR32OPragmaSplit(stmt)
	if !ok || name != "synchronous" {
		return false
	}
	if _, isValue := tclR32OPragmaValue(rest); !isValue {
		return false
	}
	if !tclCGOInTransaction(cgodb) {
		return false
	}
	return tclCGOCannotPrepare(cgodb, stmt)
}

// tclOracleTempDatabaseOpen reports whether the oracle connection's TEMP
// database is currently open (db->aDb[1].pBt != 0), the same pure read
// r35a_tempdb_open_probe_test.go's r35aTempDBIsOpen and
// tclOracleAttachAtLimit's own doc comment already established against
// 3.53.3: PragTyp_DATABASE_LIST (pragma.c) skips every database whose pBt is
// 0, so "temp" appears in pragma_database_list exactly when the TEMP
// database has been opened, and reading the list never opens it itself --
// unlike a "temp."-qualified pragma, which forces the open at pragma.c:457
// before this question could even be asked. Any error answers "not open",
// the same conservative direction every sibling probe in this file takes.
func tclOracleTempDatabaseOpen(cgodb *sql.DB) bool {
	var n int
	if err := cgodb.QueryRow(
		"SELECT count(*) FROM pragma_database_list WHERE lower(name) = 'temp'",
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// tclR32OOracleRefusesTempStoreInTxn is tclR32OOracleRefusesSyncInTxn's
// sibling for "PRAGMA temp_store=<v>", whose rejection rule pragma.c pins one
// condition harder than synchronous's: changeTempStorage (pragma.c:183) calls
// invalidateTempStorage (pragma.c:159), and invalidateTempStorage's own
// "temporary storage cannot be changed from within a transaction" error is
// gated FIRST on `db->aDb[1].pBt!=0` -- the TEMP database must already be
// open -- before it ever looks at the transaction state. A bare "BEGIN;
// PRAGMA temp_store=1" on a connection that has never touched a temp object
// is accepted (invalidateTempStorage's body is never entered), which is
// exactly why tclR32OPragmaRejectedAtPrepare's own doc comment declines to
// treat temp_store like the unconditional temp_store_directory/encoding
// arms: this one condition is not in the statement's text.
//
// Both extra facts are read off the oracle before it is ever asked to
// prepare anything: tclCGOInTransaction (the same pure BEGIN/ROLLBACK probe
// tclR32OOracleRefusesSyncInTxn and tclOracleRefusesVacuumInTxn use) for
// db->autoCommit, and tclOracleTempDatabaseOpen for db->aDb[1].pBt. Only once
// BOTH hold does the verdict come from the oracle itself
// (tclCGOCannotPrepare) -- which also settles, for free, changeTempStorage's
// OWN early-out ("if( db->temp_store==ts ) return SQLITE_OK") for a value
// already in force: that case never reaches invalidateTempStorage at all, so
// the prepare succeeds and tclCGOCannotPrepare correctly answers false.
//
// Engine-side, this is already a faithful, verified port (see
// engine/pragma.go's r35aTempStoreSetter and r35a_temp_store_test.go's
// TestR35ATempStoreTransactionParity) -- pragma.test#21 "PRAGMA temp_store =
// 1", mined inside a transaction that HAD opened the temp database, was being
// counted as a coverage gap purely because tclExecProbeSafe's PRAGMA
// denylist (necessarily) never asks the oracle about it at all.
func tclR32OOracleRefusesTempStoreInTxn(cgodb *sql.DB, stmt string) bool {
	name, rest, ok := tclR32OPragmaSplit(stmt)
	if !ok || name != "temp_store" {
		return false
	}
	if _, isValue := tclR32OPragmaValue(rest); !isValue {
		return false
	}
	if !tclCGOInTransaction(cgodb) {
		return false
	}
	if !tclOracleTempDatabaseOpen(cgodb) {
		return false
	}
	return tclCGOCannotPrepare(cgodb, stmt)
}

// tclFts5ConfigInsertRe matches the SHAPE of an fts5 CONFIGURATION command:
// "INSERT INTO t(t, rank) VALUES(...)", where the table names ITSELF as the
// first target column and "rank" as the second. Whether it really is one is
// settled by tclIsFts5ConfigInsert comparing that first column with the table
// name.
//
// Deliberately NOT the one-column form "INSERT INTO t(t) VALUES('optimize')":
// that is fts5's (and fts3's) DATA command channel, which only moves rows
// around and which a savepoint rollback undoes completely. Widening this to
// cover it moved 84 fts3 statements out of mutualReject and into unsupported
// for nothing -- the accounting misfiling tclExecProbeSafe's own doc comment
// describes.
var tclFts5ConfigInsertRe = regexp.MustCompile(`(?is)^\s*INSERT\s+INTO\s+"?([A-Za-z_][A-Za-z0-9_]*)"?\s*\(\s*"?([A-Za-z_][A-Za-z0-9_]*)"?\s*,\s*"?rank"?\s*\)`)

// tclIsFts5ConfigInsert reports whether stmt is an fts5 configuration command.
func tclIsFts5ConfigInsert(stmt string) bool {
	m := tclFts5ConfigInsertRe.FindStringSubmatch(stmt)
	return m != nil && strings.EqualFold(m[1], m[2])
}

// tclCGOInTransaction reports whether the oracle has an explicit transaction open,
// without disturbing it.
func tclCGOInTransaction(cgodb *sql.DB) bool {
	if _, err := cgodb.Exec("BEGIN"); err != nil {
		return true
	}
	cgodb.Exec("ROLLBACK")
	return false
}

// tclCGOTxnStmtAlsoRejects reports whether the oracle would also reject a
// BEGIN/COMMIT/ROLLBACK that the Go engine rejected for transaction-state reasons.
func tclCGOTxnStmtAlsoRejects(cgodb *sql.DB, kind engine.TxnKind) bool {
	if kind == engine.TxnBegin {
		return tclCGOInTransaction(cgodb)
	}
	return !tclCGOInTransaction(cgodb)
}

// tclCGOExecAlsoRejects probes whether the oracle rejects an exec statement Go declined.
// Probes inside a rolled-back SAVEPOINT to avoid persisting effects. Returns (rejects, txnLost)
// where txnLost indicates a conflict-rollback that aborted the transaction.
func tclCGOExecAlsoRejects(cgodb *sql.DB, stmt string) (rejects, txnLost bool) {
	if kind, ok := engine.TxnStmtKind(stmt); ok {
		return tclCGOTxnStmtAlsoRejects(cgodb, kind), false
	}
	if _, err := cgodb.Exec("SAVEPOINT __musql_probe"); err != nil {
		return false, false
	}
	_, stmtErr := cgodb.Exec(stmt)
	if _, err := cgodb.Exec("ROLLBACK TO __musql_probe"); err != nil {
		// Savepoint is gone (conflict-rollback aborted the transaction).
		cgodb.Exec("RELEASE __musql_probe")
		return stmtErr != nil, true
	}
	if _, err := cgodb.Exec("RELEASE __musql_probe"); err != nil {
		return false, false
	}
	return stmtErr != nil, false
}

// tclQueryMutatesOracle reports whether stmt, though a SELECT, would CHANGE
// the oracle's state if it were probed -- the one place the "queries never
// mutate, so probing is free" rule above breaks down.
//
// FTS3/FTS4 exposes optimize() as an ordinary scalar FUNCTION whose whole
// purpose is a side effect: "SELECT OPTIMIZE(t1) FROM t1 LIMIT 1" merges every
// segment of t1's index into one. Probing it left the oracle with one %_segdir
// row where the engine (which declines optimize) still had three, and the very
// next statement -- "SELECT level, idx FROM t1_segdir" -- was then scored WRONG
// against an oracle the harness itself had desynchronized. fts3d.test/
// fts3e.test contributed four such phantom divergences.
//
// Matching by name is deliberately narrow: this is a specific, known
// side-effecting function, exactly like the specific statement kinds
// tclExecProbeSafe refuses to replay. Skipping the probe costs only the
// mutual-reject/unsupported distinction for that one statement.
func tclQueryMutatesOracle(stmt string) bool {
	upper := strings.ToUpper(stmt)
	return strings.Contains(upper, "OPTIMIZE(") || strings.Contains(upper, "OPTIMIZE (")
}

// tclCGOCannotPrepare reports whether the oracle rejects stmt at PREPARE time,
// which is the one question that can still be asked about a statement
// tclQueryMutatesOracle refuses to run: sqlite3_prepare_v2 compiles and resolves
// names but executes nothing, so a statement that fails to prepare cannot have
// had a side effect, and a statement that DOES prepare is discarded here
// unstepped -- optimize() only merges segments when the function is actually
// called on a row.
//
// This is an ACCOUNTING correction of the same kind tclExecProbeSafe's own doc
// comment describes: "unsupported" is defined as "this engine declined a
// statement the ORACLE would have run", and skipping the probe entirely booked
// every optimize() decline as a coverage gap without ever asking. All six in the
// corpus are mutual rejections, for two different reasons and neither of them a
// gap:
//
//	4  fts2q.test -- "CREATE VIRTUAL TABLE t1 USING fts2" is a module NEITHER
//	   engine has (fts2 was removed from SQLite long ago), so t1 exists nowhere
//	   and "SELECT OPTIMIZE(t1) FROM t1" is "no such table: t1" on both sides.
//	2  e_fts3.test -- t4's "CREATE VIRTUAL TABLE t4 USING fts3(a,b)" is written
//	   as an argument to the file's own ddl_test proc, which is not one of the
//	   four SQL-embedding constructs this miner understands, so the CREATE was
//	   never mined at all and neither engine has t4 either.
//
// Reached from the tclQueryMutatesOracle branch, and -- for the same reason,
// a statement kind that must not be RUN on the shared oracle -- from
// tclAttachCannotPrepare. Every OTHER declined statement already gets the
// stronger question (run it inside a rolled-back savepoint and compare), and a
// prepare-only probe would be a strictly weaker answer there.
func tclCGOCannotPrepare(cgodb *sql.DB, stmt string) bool {
	ps, err := cgodb.Prepare(stmt)
	if err != nil {
		return true
	}
	ps.Close()
	return false
}

// tclExecUnsupportedBucket derives a histogram key for an exec-side (DDL/DML)
// statement Go's ExecArgs declined as "unsupported": the generic dispatcher
// rejection (see tclLeadingKeywords' doc comment) buckets by leading
// keyword; a more specific unsupported error surfaced deeper in
// CreateTable/Insert/Update/Delete (e.g. "AUTOINCREMENT is not supported",
// "WITHOUT ROWID tables are not supported") buckets by its own (normalized)
// message, which is the more informative signal in that case.
func tclExecUnsupportedBucket(stmt string, err error) string {
	msg := err.Error()
	if strings.Contains(msg, "this write path only supports") {
		return tclLeadingKeywords(stmt) + " (statement kind not supported by write path)"
	}
	return normalizeErrorMessage(msg)
}

// tclQueryUnsupportedBucket derives a histogram key for a query-side (SELECT/
// WITH/VALUES) statement the read engine declined: a few coarse, easy-to-spot
// SQL-shape buckets (CTE, window function, compound SELECT) are checked
// first since they're more informative than the underlying error text: e.g.
// query.go rejects CTEs at the FROM-clause level, but the error text and file
// line don't necessarily say "WITH" anywhere. Anything else falls back to
// the normalized engine error message.
func tclQueryUnsupportedBucket(stmt string, err error) string {
	upper := strings.ToUpper(stmt)
	switch {
	case strings.HasPrefix(strings.TrimSpace(upper), "WITH"):
		// The REASON is kept, exactly as the compound case below keeps it. These
		// two buckets used to drop it and label by shape alone, which made 118
		// statements (38 CTE, 80 window) unclassifiable -- and this histogram
		// exists to decide what is closable. A shape label answers "what kind of
		// query", never "what stopped it".
		return "SELECT: WITH (CTE) (" + normalizeErrorMessage(err.Error()) + ")"
	case strings.Contains(upper, " OVER (") || strings.Contains(upper, " OVER("):
		return "SELECT: window function (OVER) (" + normalizeErrorMessage(err.Error()) + ")"
	case strings.Contains(upper, "UNION") || strings.Contains(upper, "INTERSECT") || strings.Contains(upper, "EXCEPT"):
		return "SELECT: compound (" + normalizeErrorMessage(err.Error()) + ")"
	default:
		return "SELECT: " + normalizeErrorMessage(err.Error())
	}
}

// tclNormalizeCGOCell mirrors compat-harness/worker/main.go's normalize
// function (that package can't be imported -- it's package main, built as a
// separate per-engine binary), tagging a database/sql-scanned cgo value by
// SQLite storage class in the exact same scheme normalizeEngineValue
// (pureengine_test.go) uses for the Go engine's own engine.Value, so
// cellsEqual/queryResultsMatch can compare the two directly.
func tclNormalizeCGOCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return fmt.Sprintf("I:%d", x)
	case float64:
		return fmt.Sprintf("F:%g", x)
	case bool:
		if x {
			return "I:1"
		}
		return "I:0"
	case string:
		return "T:" + x
	case []byte:
		if utf8.Valid(x) {
			return "T:" + string(x)
		}
		return "X:" + hex.EncodeToString(x)
	case time.Time:
		// The mattn driver does NOT report this column's stored value: for a
		// column DECLARED date/datetime/timestamp it reports its own PARSE of
		// it (SQLiteTimestampFormats for a TEXT value, a unix epoch for an
		// INTEGER one -- sqlite3.go). Tagged distinctly so
		// tclReconcileDriverTimes can undo that conversion against the
		// engine's own cell; anything it cannot reconcile still fails loudly.
		return "TS:" + x.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("?:%v", v)
	}
}

// tclReconcileDriverTimes undoes the mattn driver's declared-type time
// conversion (see tclNormalizeCGOCell's time.Time case) so a column real
// SQLite and this engine agree on byte-for-byte is not scored WRONG merely
// because the ORACLE'S DRIVER parsed it. It returns cgoRows with every "TS:"
// cell replaced by the engine's cell at the same position -- but ONLY where that
// engine cell, put through the driver's own conversion rules, yields the same
// instant. any reports whether any cell was reconciled at all (false means the
// caller should not bother re-comparing).
//
// Reached only after the strict compare has already failed, exactly like
// tclOrderByTieOK and tclStripColComments: a cell that does not reconcile is
// left exactly as it was, so a genuine divergence in a date column still fails.
func tclReconcileDriverTimes(goRows, cgoRows [][]string) (out [][]string, any bool) {
	if len(goRows) != len(cgoRows) {
		return cgoRows, false
	}
	out = make([][]string, len(cgoRows))
	for r := range cgoRows {
		out[r] = cgoRows[r]
		if len(goRows[r]) != len(cgoRows[r]) {
			continue
		}
		for c, cell := range cgoRows[r] {
			if !strings.HasPrefix(cell, "TS:") {
				continue
			}
			want, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(cell, "TS:"))
			if err != nil || !tclDriverTimeMatches(goRows[r][c], want) {
				continue
			}
			if &out[r][0] == &cgoRows[r][0] {
				out[r] = append([]string(nil), cgoRows[r]...)
			}
			out[r][c] = goRows[r][c]
			any = true
		}
	}
	return out, any
}

// tclDriverTimeMatches reports whether engine cell (in normalizeEngineValue's
// tagged form) is a value the mattn driver would have converted into want. It
// mirrors sqlite3.go's own two conversions: a TEXT value is parsed with
// SQLiteTimestampFormats in UTC after a trailing "Z" is trimmed, and an INTEGER
// is a unix timestamp -- in seconds, or in milliseconds past |1e12|.
func tclDriverTimeMatches(cell string, want time.Time) bool {
	switch {
	case strings.HasPrefix(cell, "T:"):
		s := strings.TrimSuffix(strings.TrimPrefix(cell, "T:"), "Z")
		for _, layout := range sqlite3.SQLiteTimestampFormats {
			if got, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
				return got.Equal(want)
			}
		}
		// An unparseable value becomes the ZERO time on the driver's side.
		return want.IsZero()
	case strings.HasPrefix(cell, "I:"):
		n, err := strconv.ParseInt(strings.TrimPrefix(cell, "I:"), 10, 64)
		if err != nil {
			return false
		}
		if n > 1e12 || n < -1e12 {
			return time.Unix(0, n*int64(time.Millisecond)).UTC().Equal(want)
		}
		return time.Unix(n, 0).UTC().Equal(want)
	}
	return false
}

// tclRunCGOQuery runs stmt against the cgo oracle connection and returns its
// normalized columns/rows, in the same shape queryResultsMatch expects.
func tclRunCGOQuery(cgodb *sql.DB, stmt string) (cols []string, rows [][]string, err error) {
	r, err := cgodb.Query(stmt)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()
	cols, err = r.Columns()
	if err != nil {
		return nil, nil, err
	}
	for r.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cols))
		for i, c := range cells {
			row[i] = tclNormalizeCGOCell(c)
		}
		rows = append(rows, row)
	}
	if err := r.Err(); err != nil {
		return nil, nil, err
	}
	return cols, rows, nil
}

// tclSafeExecArgs runs a DDL/DML statement against the Go engine writer,
// recovering any panic into (panicked=true, panicVal) rather than letting it
// crash the whole test binary -- a panic is always a bug (see the package
// doc comment) and must be reported as such, never silently mixed into an
// ordinary error return.
func tclSafeExecArgs(godb *engine.Session, stmt string) (execErr error, panicked bool, panicVal any) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			panicVal = r
		}
	}()
	_, _, execErr = godb.ExecArgs(stmt, nil)
	return
}

// tclSafeGoQuery runs a query-shaped statement against a fresh
// SnapshotPager of the Go engine's current (possibly mid-transaction,
// not-yet-Close'd) state, normalizing rows via normalizeEngineValue
// (pureengine_test.go) exactly like TestPureEngineSLTCoverage does. Panics
// are recovered exactly like tclSafeExecArgs.
func tclSafeGoQuery(godb *engine.Session, stmt string) (cols []string, rows [][]string, queryErr error, panicked bool, panicVal any) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			panicVal = r
		}
	}()
	pager, err := godb.SnapshotPager()
	if err != nil {
		queryErr = err
		return
	}
	defer pager.Close()
	c, v, err := pager.QueryArgs(stmt, nil)
	if err != nil {
		queryErr = err
		return
	}
	cols = c
	rows = make([][]string, len(v))
	for i, row := range v {
		cells := make([]string, len(row))
		for j, val := range row {
			cells[j] = normalizeEngineValue(val)
		}
		rows[i] = cells
	}
	return
}

// tclAllFiles lists the vendored corpus's .test files in deterministic
// (sorted) order, or t.Skip's with a clear message if the corpus hasn't been
// vendored (so CI/dev environments without testdata/tcl/ still build and
// pass the rest of the suite).
func tclAllFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(tclCorpusDir)
	if err != nil {
		t.Skipf("TCL corpus not vendored at %s: %v", tclCorpusDir, err)
		return nil
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".test") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files
}

// tclStripColComments returns cols with any trailing source COMMENT (and the
// whitespace before it) removed from each name -- see the call site: an
// unaliased select-list expression's auto-generated column NAME is an
// implementation artifact SQL does not define, and cgo folds a trailing source
// comment into it while this engine does not. Stripping identically on both
// sides neutralizes only that artifact; a real name divergence survives.
//
// BOTH comment openers count. "--" was here first; "/*" joined it when the
// lexer stopped rejecting an unterminated block comment, which is legal SQL
// (C SQLite runs "SELECT 1 /* unterminated"). main.test asks for
// "select 123/*abc", where cgo names the column "123/*abc" and this engine
// names it "123" -- the same artifact as the "--" case, one opener along, and
// the values agree.
func tclStripColComments(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		cut := -1
		for _, opener := range []string{"--", "/*"} {
			if j := strings.Index(c, opener); j >= 0 && (cut < 0 || j < cut) {
				cut = j
			}
		}
		if cut >= 0 {
			c = c[:cut]
		}
		out[i] = strings.TrimRight(c, " \t\r\n")
	}
	return out
}

// tclUnspecifiedAutoNameRE matches C SQLite's own internal ":N"
// disambiguation suffix on an auto-generated result-column name (e.g. "a:1",
// "a:3", or -- verified directly against C SQLite over a deeply nested
// parenthesized join -- "a:969282127"; see engine/query.go's
// errIfDuplicateOutputNames doc comment for the full worked example). SQLite
// assigns this suffix, via an internal renaming counter, only to an
// unaliased result column it could not otherwise name uniquely -- most
// commonly one produced by flattening a parenthesized/nested join. SQLite's
// own documentation states such an auto-generated name is UNSPECIFIED, and
// (per that same doc comment) the counter itself is not even a deterministic
// function of the query past a couple of nesting levels -- so a real name
// matching this pattern can never be a contract this engine (or any engine)
// is expected to reproduce byte-for-byte. An ordinary explicit alias or
// plain "tbl.col" reference never produces this shape, so the pattern is a
// narrow, reliable signal that the name it's found on is exactly this kind
// of unspecified artifact, not a real column identity.
var tclUnspecifiedAutoNameRE = regexp.MustCompile(`:[0-9]+$`)

// tclRelaxUnspecifiedColNames returns column-name slices for use in a
// queryResultsMatch (pureengine_test.go) call where any index whose cgo
// (real-SQLite) name matches tclUnspecifiedAutoNameRE has BOTH sides
// overwritten with an identical placeholder -- so that single index's name
// is no longer compared. Every other index is returned completely
// unchanged, so an ordinary "SELECT col"/"SELECT tbl.col" reference, or any
// other well-defined name, still requires an EXACT match: this narrows the
// leniency to precisely the SQLite-documented-unspecified ":N" auto-name
// case (engine/query.go's errIfDuplicateOutputNames, which now executes a
// nested-parenthesized-join query with its own best-effort plain naming
// instead of declining it) and does not touch column COUNT, row COUNT, or
// any row VALUE -- queryResultsMatch still compares those exactly, so a real
// value/order/count divergence is still scored WRONG. gCols/cCols are
// returned unchanged (not copied) when their lengths differ, or when
// neither side has any ":N"-suffixed cgo name, so the common case allocates
// nothing new.
func tclRelaxUnspecifiedColNames(gCols, cCols []string) ([]string, []string) {
	if len(gCols) != len(cCols) {
		return gCols, cCols
	}
	var relaxIdx []int
	for i, c := range cCols {
		if tclUnspecifiedAutoNameRE.MatchString(c) {
			relaxIdx = append(relaxIdx, i)
		}
	}
	if len(relaxIdx) == 0 {
		return gCols, cCols
	}
	outG := append([]string(nil), gCols...)
	outC := append([]string(nil), cCols...)
	for _, i := range relaxIdx {
		outG[i] = "\x00unspecified-auto-name\x00"
		outC[i] = "\x00unspecified-auto-name\x00"
	}
	return outG, outC
}

// tclOrderByKeyIndices maps a statement's ORDER BY terms to 0-based result
// column indices (into cols), or returns ok=false if any term is not a plain
// column-name or 1-based ordinal reference this simple static parse can resolve
// (an expression, a function, a name not in the select list, ...). It is used
// only to recognize a legitimate ORDER BY tie; a false result just means "fall
// back to the strict comparison", never a wrong answer.
func tclOrderByKeyIndices(stmt string, cols []string) (idxs []int, ok bool) {
	low := strings.ToLower(stmt)
	pos := strings.LastIndex(low, "order by")
	if pos < 0 {
		return nil, false
	}
	rest := stmt[pos+len("order by"):]
	// Cut the ORDER BY clause off at a trailing LIMIT/OFFSET, if any.
	restLow := strings.ToLower(rest)
	for _, kw := range []string{" limit ", " limit\t", " limit\n"} {
		if j := strings.Index(restLow, kw); j >= 0 {
			rest = rest[:j]
			restLow = strings.ToLower(rest)
		}
	}
	terms := strings.Split(rest, ",")
	for _, term := range terms {
		t := strings.TrimSpace(term)
		// Drop trailing ASC/DESC and any COLLATE <name>.
		fields := strings.Fields(t)
		if len(fields) == 0 {
			return nil, false
		}
		// Strip a trailing NULLS FIRST/NULLS LAST modifier before the
		// single-token-key shape check below: it changes where NULLs sort
		// relative to non-NULLs, but never which rows tie with which -- two
		// rows still tie here iff their key values (including a NULL key
		// value, compared exactly like any other via cellsEqual in (b)
		// below) are equal -- so it's transparent to tie-detection exactly
		// like ASC/DESC's own direction is. Without this, any NULLS
		// FIRST/LAST ORDER BY (newly reachable now that the engine parses
		// it) fell through to "more than 2 fields" below and lost tie
		// tolerance entirely, even for a plain single-column ORDER BY.
		if n := len(fields); n >= 2 && strings.EqualFold(fields[n-2], "nulls") &&
			(strings.EqualFold(fields[n-1], "first") || strings.EqualFold(fields[n-1], "last")) {
			fields = fields[:n-2]
		}
		if len(fields) == 0 {
			return nil, false
		}
		// Only a single-token key (bare column name or ordinal), optionally
		// followed by ASC/DESC, is resolvable here; anything longer (an
		// expression, a COLLATE clause, a qualified name) bails to strict.
		if len(fields) > 2 {
			return nil, false
		}
		if len(fields) == 2 {
			d := strings.ToLower(fields[1])
			if d != "asc" && d != "desc" {
				return nil, false
			}
		}
		// A leading unary "+" is SQLite's own idiom for "sort by this column
		// but do NOT use an index for it" (autoindex4.test writes
		// "ORDER BY +b"). It changes the query PLAN, never which rows tie, so
		// it is transparent to tie-detection exactly like ASC/DESC.
		key := strings.TrimPrefix(strings.TrimSpace(fields[0]), "+")
		key = strings.Trim(key, "\"'`[]")
		if n, err := strconv.Atoi(key); err == nil { // 1-based ordinal
			if n < 1 || n > len(cols) {
				return nil, false
			}
			idxs = append(idxs, n-1)
			continue
		}
		found := -1
		for ci, c := range cols {
			if strings.EqualFold(c, key) {
				found = ci
				break
			}
		}
		if found < 0 {
			return nil, false
		}
		idxs = append(idxs, found)
	}
	if len(idxs) == 0 {
		return nil, false
	}
	return idxs, true
}

// tclOrderByTieOK reports whether the engine's and cgo's row orderings differ
// ONLY by permuting rows that share every ORDER BY sort key -- a difference SQL
// leaves unspecified and that must not be scored as a wrong answer. It requires
// (a) the ORDER BY to be statically resolvable to result columns, (b) the two
// results to hold the same number of rows with the same ORDER-BY-key value at
// every position (so both are validly ordered), and (c) the full row multisets
// to be equal (so no row was gained, lost, or altered). If any holds false it
// returns false and the caller keeps the strict verdict.
func tclOrderByTieOK(stmt string, gotCols []string, gotRows [][]string, cgoCols []string, cgoRows [][]string) bool {
	if len(gotRows) != len(cgoRows) || len(gotCols) != len(cgoCols) {
		return false
	}
	idxs, ok := tclOrderByKeyIndices(stmt, gotCols)
	if !ok {
		return false
	}
	// (b) same ORDER BY key value at every position -> both orderings honor
	// the ORDER BY; they can only disagree inside tie groups.
	for r := range gotRows {
		if len(gotRows[r]) != len(cgoRows[r]) {
			return false
		}
		for _, ki := range idxs {
			if ki >= len(gotRows[r]) || ki >= len(cgoRows[r]) {
				return false
			}
			if !cellsEqual(gotRows[r][ki], cgoRows[r][ki]) {
				return false
			}
		}
	}
	// (c) identical row multisets (order-insensitive full-row comparison).
	ok2, _ := queryResultsMatch(gotCols, gotRows, cgoCols, cgoRows, false)
	return ok2
}

// tclOrderByKeyHidden reports whether the statement's ORDER BY sorts by a
// column that is NOT among its own output columns -- "SELECT d FROM t2 ORDER
// BY a" (descidx1.test). The row order of a tie group under such a key is
// invisible in the result: with the key absent, this harness CANNOT tell a
// legitimate tie reorder (C SQLite's order there follows whichever index
// its planner picked -- this engine always full-scans, and reproducing the
// planner's choice is explicitly out of scope) from a genuinely wrong order.
//
// Scoring it WRONG would claim a judgement the output does not support, so
// runTCLSegment scores it OUT OF SCOPE instead, and only when the two row
// MULTISETS are identical -- i.e. the two engines returned exactly the same
// rows and can differ in nothing but their order. A row-set difference is
// still a genuine WRONG.
func tclOrderByKeyHidden(stmt string, cols []string) bool {
	if _, ok := tclOrderByKeyIndices(stmt, cols); ok {
		return false // every key IS in the output: tclOrderByTieOK judges it
	}
	low := strings.ToLower(stmt)
	pos := strings.LastIndex(low, "order by")
	if pos < 0 {
		return false
	}
	for _, term := range strings.Split(stmt[pos+len("order by"):], ",") {
		fields := strings.Fields(strings.TrimSpace(term))
		if len(fields) == 0 {
			return false
		}
		name := strings.Trim(fields[0], "\"[]`")
		if name == "" || strings.ContainsAny(name, "()+-*/|") {
			return false // an expression key: not the simple hidden-column shape
		}
		for _, c := range cols {
			if strings.EqualFold(c, name) {
				return false // this key IS visible; the shape covered is "none are"
			}
		}
	}
	return true
}

// tclTally accumulates one segment's (or one file's, or the whole run's)
// outcome counts; runTCLSegment returns one, and TestTCLCorpus sums them.
//
// propertyMatch is the tier between pass and outOfScope: a statement whose
// VALUES cannot be reproduced across two independently-seeded engines, but
// whose column count, column names, row count and per-cell storage classes
// still agreed. It is deliberately its own bucket rather than folded into
// pass, so a reader can see how many statements got the weaker treatment --
// and outOfScope shrinks to only what nothing at all can be asked of. See
// tcl_property_test.go.
type tclTally struct {
	statements, pass, mutualReject, propertyMatch, outOfScope, unsupported, wrong, panics int
}

func (a *tclTally) add(b tclTally) {
	a.statements += b.statements
	a.pass += b.pass
	a.mutualReject += b.mutualReject
	a.propertyMatch += b.propertyMatch
	a.outOfScope += b.outOfScope
	a.unsupported += b.unsupported
	a.wrong += b.wrong
	a.panics += b.panics
}

// tclIsOutOfScope reports whether a statement has no reproducible, specified answer
// (EXPLAIN with non-identical VDBE output, implementation-defined or nondeterministic values).
func tclIsOutOfScope(stmt string, err error) bool {
	// Resolve the verb using the lexer (not strings.Fields) to skip leading comments.
	verb, ok := engine.LeadingStatementVerb(stmt)
	if !ok {
		if fields := strings.Fields(stmt); len(fields) > 0 {
			verb = strings.ToUpper(fields[0])
		}
	}
	if strings.EqualFold(verb, "EXPLAIN") {
		return true
	}
	if tclCompileOptionsBuildIdentity(stmt) {
		return true
	}
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, m := range []string{
		"not reproducible against C SQLite",
		"can never match an independently-seeded oracle",
		"disambiguation is not reproduced",
		// engine/compile_options.go's evalCompileOptionGet, for an index
		// inside the answering build's own option list. Keyed on the phrase
		// rather than on the statement text so ONLY the call the engine
		// itself judges non-comparable is booked here.
		"no two independently-built implementations can agree on it",
	} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// tclPragmaCompileOptionsRe matches a reference to the EPONYMOUS
// "pragma_compile_options" table-valued function -- the query-able form of
// PRAGMA compile_options. Anchored on identifier boundaries so it cannot fire
// on a longer name, and case-insensitive because SQL identifiers are.
var tclPragmaCompileOptionsRe = regexp.MustCompile(`(?i)\bpragma_compile_options\b`)

// tclCompileOptionsBuildIdentity reports whether stmt's result is the
// answering build's own compile-option list, which is that build's identity
// and not a value SQL defines.
//
// Its rows are literally sqlite_compileoption_get(0), get(1), ... :
// PragTyp_COMPILE_OPTIONS (pragma.c:2374-2384) is
//
//	while( (zOpt = sqlite3_compileoption_get(i++))!=0 ){
//	  sqlite3VdbeLoadString(v, 1, zOpt);
//	  sqlite3VdbeAddOp2(v, OP_ResultRow, 1, 1);
//	}
//
// so this table is the same array (sqlite3azCompileOpt[], assembled by the C
// preprocessor from the macros in force where the library was compiled) that
// SQLite's own ctime-2.4 declines to assert a value for. Two consequences,
// both measured rather than assumed:
//
//   - the two oracle builds THIS repo runs already disagree, 42 options plain
//     and 43 under "-tags sqlite_fts5", differing in ENABLE_FTS5 and nothing
//     else (compile_option_used_test.go's file comment records the diff);
//   - bigmmap.test's own mined statement,
//     "SELECT compile_options AS x FROM pragma_compile_options WHERE x LIKE
//     'max_mmap_size=%'", returns MAX_MMAP_SIZE=0x7fff0000 here purely because
//     of sqliteInt.h:1095-1106's platform #if chain (__linux__/_WIN32/__APPLE__/
//     __sun/__FreeBSD__/__DragonFly__ get 0x7fff0000, everything else 0), and
//     nothing in mattn/go-sqlite3's "#cgo CFLAGS" pins it -- so even two C
//     builds of the SAME source disagree across platforms.
//
// A pure-Go engine has no such array to report and may not invent one: an
// honest musql list would omit ATOMIC_INTRINSICS and COMPILER and so
// disagree at row one, and parroting the oracle's would be a wrong answer on
// any other host. So this is excluded from the specified-conformance
// denominator exactly like EXPLAIN, rather than counted as a gap the engine
// could ever close. bigmmap.test uses it the way the category implies -- to
// read its OWN limit and decide whether to skip (bigmmap.test:28-39).
//
// Keyed on the statement text, not on an engine error message, because the
// engine does not implement the table at all: its decline is an ordinary
// "no such table", which carries no judgement about comparability and must
// not be given one it cannot support.
func tclCompileOptionsBuildIdentity(stmt string) bool {
	if !tclPragmaCompileOptionsRe.MatchString(stmt) {
		return false
	}
	// ...and the build-determined VALUE must actually reach the result. Merely
	// naming the table is not enough, and keying on that alone was wrong:
	// "SELECT count(*) FROM pragma_compile_options WHERE 0" answers 0 on every
	// correct implementation, and this predicate booked it non-comparable.
	//
	// That is the exact mechanism this bucket must never become -- a rule that
	// keys on the PRESENCE of a build-identity source rather than on whether the
	// ANSWER depends on it will silently swallow a real gap the day one appears
	// in a shape like that. No corpus statement hits it today; the point is that
	// none can tomorrow either.
	//
	// The value reaches the result through the compile_options column itself or
	// through a star -- and "count(*)" is one: it counts the build's OWN option
	// rows, so it is exactly as build-determined as the option text.
	// TestPragmaCompileOptionsIsBookedOutOfScope pins that spelling. A
	// statement projecting neither (a bare "SELECT 1 FROM ... WHERE 0") yields
	// a build-independent answer this engine is expected to produce.
	//
	// ponytail: syntactic, so "count(*) ... WHERE 0" is still booked out of
	// scope although it answers 0 everywhere. No corpus statement has that
	// shape; telling them apart needs the predicate's value, not its spelling.
	return tclCompileOptionsProjectedRe.MatchString(stmt)
}

// tclCompileOptionsProjectedRe matches a reference that carries build-determined
// rows out to the caller: the column by name, or any star (a select-list star
// expands to the column; count(*) counts the build's rows).
var tclCompileOptionsProjectedRe = regexp.MustCompile(`(?i)\bcompile_options\b|\*`)

// tclOracleLacksBuiltin reports whether cerr is the ORACLE missing an
// extension this engine has built in, rather than a real divergence. The
// engine registers generate_series, rtree and rtree_i32 unconditionally
// (engine/vtab_series.go, vtab_rtree.go) and fts5 on request
// (engine.RegisterFTS5, which fts5_enabled_test.go calls in the
// `-tags sqlite_fts5` build), and real sqlite3 ships all four as optional
// compile-time extensions -- but the CGo oracle this harness links
// (mattn/go-sqlite3, default build tags) has them off, so it answers "no such
// table: generate_series" to a query the engine legitimately serves. Scoring
// that WRONG would blame the engine for being MORE complete than this
// particular oracle build; it is out of scope for a differential instead,
// exactly like the EXPLAIN and nondeterministic-value cases tclIsOutOfScope
// already excludes. Every wrong result in tabfunc01.test was this.
//
// Deliberately keyed on the module NAMES, not on "no such table" generally:
// a query naming any other missing table is still a genuine divergence.
func tclOracleLacksBuiltin(cerr error) bool {
	if cerr == nil {
		return false
	}
	return tclOracleLacksBuiltinRe.MatchString(cerr.Error())
}

// tclOracleLacksBuiltinRe is that check: "no such table"/"no such module" for
// one of the modules this oracle build omits, with or WITHOUT a schema
// qualifier in front of it.
//
// The qualifier is matched generically rather than as the "main."/"temp." pair
// it used to be, because a qualifier on a table-valued function is IGNORED by
// C SQLite -- it resolves an eponymous virtual table by NAME and never even
// validates the database the qualifier names (see engine/sql_parser.go's
// parseTableRef for the enumeration). tabfunc01.test spells that out in three
// consecutive tests, 4.1/4.2/4.3, as main.generate_series, temp.generate_series
// and aux1.generate_series, each expecting 1 2 3 4 -- so once this engine
// started parsing the qualified form, "aux1." fell outside the old pair and its
// correct answer was scored WRONG against an oracle that simply has no
// generate_series to run. rtree_i32 leads rtree so the longer name wins.
var tclOracleLacksBuiltinRe = regexp.MustCompile(
	`(?i)no such (?:table|module): (?:[a-z_][a-z0-9_]*\.)?(?:rtree_i32|rtree|generate_series|fts5)\b`)

// runTCLSegment drives one reset-delimited statement sequence (see
// tclSegments) against a FRESH Go-engine DB and a FRESH cgo connection, in
// lockstep -- see the package doc comment above for the full per-statement
// protocol (query vs exec, unsupported/constraint/accepted classification).
// label identifies this segment in t.Errorf output (e.g. "delete4.test#2").
// tclRandomTainted tracks, per segment, the tables an accepted statement filled
// with values from random()/randomblob(): their CONTENT is structurally
// non-comparable across two independently-seeded engines, so a later query
// reading one of them cannot be judged at all (vacuum3.test seeds t1 with
// "UPDATE t1 SET d = randomblob(1000)" and then plainly selects it). This is
// the stored-content counterpart of tclCallsNondeterministicFunc, which
// already declines a query that calls one of those functions ITSELF.
type tclRandomTainted map[string]tclTaintKind

// tclTaintKind separates the two ways a table stops being comparable, because
// they cost different amounts. tclTaintValues is random CONTENT in rows both
// engines have: the row SET still matches, so a query that cannot filter on
// those values still has a comparable row count (see tclTaintedPlan).
// tclTaintRows is a table whose very rows are one-sided -- the oracle could
// not run the statement that filled it -- where nothing but the schema
// survives.
type tclTaintKind int

const (
	tclTaintValues tclTaintKind = iota + 1
	tclTaintRows
)

// note records stmt's write target as tainted when stmt draws on a
// nondeterministic function, and CLEARS the taint when a statement re-creates
// or drops the table (its content is then whatever the new statement put
// there).
func (tt tclRandomTainted) note(stmt string) {
	verb, target := tclWriteTarget(stmt)
	if target == "" {
		return
	}
	lt := strings.ToLower(target)
	switch verb {
	case "CREATE", "DROP":
		delete(tt, lt)
	}
	// Taint PROPAGATES: a write that reads a tainted table copies content that
	// is already non-comparable into its own target, so the target becomes
	// non-comparable too. join8.test is the case that needs it -- t1..t8 are
	// filled from generate_series (which the oracle build cannot run, so only
	// this engine has the rows), and then "CREATE TABLE t9 AS SELECT ... FROM
	// t1 JOIN t2 ..." succeeds on BOTH engines while producing 255 rows here
	// and 0 there; without propagation every later read of t9 is scored WRONG.
	// The target's own prior taint is excluded so a CREATE/DROP above still
	// clears it.
	// A write that COPIES tainted content can produce a different number of
	// rows on the two sides, so propagation always yields the stronger taint;
	// a write that merely GENERATES random values writes the same rows on
	// both sides and only their values differ.
	switch {
	case tt.readsOther(stmt, lt):
		tt[lt] = tclTaintRows
	case tclCallsNondeterministicFunc(stmt):
		tt[lt] = tclTaintValues
	}
}

// taintTarget marks stmt's write target tainted unconditionally, for the case
// the ORACLE could not run the statement AT ALL (tclOracleLacksBuiltin): this
// engine wrote rows the oracle's copy of that table simply never received, so
// its content is non-comparable from here on for exactly the same reason a
// random()-seeded table's is. Without it, "INSERT INTO t9 SELECT ... FROM
// generate_series(...)" -- which the oracle build cannot run -- leaves the two
// databases desynchronized and every later "SELECT count(*) FROM t9" is scored
// WRONG against a table the oracle never filled (join8.test, 2 of them).
func (tt tclRandomTainted) taintTarget(stmt string) {
	if _, target := tclWriteTarget(stmt); target != "" {
		tt[strings.ToLower(target)] = tclTaintRows
	}
}

// reads reports whether stmt names any tainted table.
func (tt tclRandomTainted) reads(stmt string) bool { return tt.readsOther(stmt, "") }

// readsRowTainted reports whether stmt names a table whose ROWS (not just
// whose values) are one-sided -- the case where not even the row count is
// comparable. See tclTaintKind.
func (tt tclRandomTainted) readsRowTainted(stmt string) bool {
	for _, w := range tclIdentWordRegex.FindAllString(stmt, -1) {
		if tt[strings.ToLower(w)] == tclTaintRows {
			return true
		}
	}
	return false
}

// readsOther is reads() ignoring one table name, so note() can ask "does this
// write draw on a tainted table OTHER than the one it is writing".
func (tt tclRandomTainted) readsOther(stmt, except string) bool {
	if len(tt) == 0 {
		return false
	}
	for _, w := range tclIdentWordRegex.FindAllString(stmt, -1) {
		if lw := strings.ToLower(w); lw != except && tt[lw] != 0 {
			return true
		}
	}
	return false
}

var tclIdentWordRegex = regexp.MustCompile(`[A-Za-z_][A-Za-z_0-9]*`)

// tclWriteTarget returns the leading verb and the table a write statement
// names: the identifier after INTO for INSERT/REPLACE, after UPDATE for an
// UPDATE, and after TABLE for CREATE/DROP TABLE. Anything else yields "".
func tclWriteTarget(stmt string) (verb, target string) {
	f := strings.Fields(stmt)
	if len(f) < 2 {
		return "", ""
	}
	// The verb comes from the LEXER, not from fields[0], for the reason
	// tclIsQuery's own doc comment gives -- and here it has teeth: a
	// WITH-prefixed write ("WITH RECURSIVE c(i) AS (...) INSERT INTO
	// t1(a,ax,b) SELECT printf(...), random(), i FROM c", btree02.test:23)
	// reads as the verb "WITH", matches nothing below, and taints NOTHING.
	up, ok := engine.LeadingStatementVerb(stmt)
	if !ok {
		up = strings.ToUpper(f[0])
	}
	switch up {
	case "INSERT", "REPLACE":
		return up, tclTableToken(tclWordAfter(f, "INTO"))
	case "UPDATE":
		return up, tclTableToken(tclWordAfter(f, "UPDATE"))
	case "CREATE", "DROP":
		return up, tclTableToken(tclWordAfter(f, "TABLE"))
	}
	return "", ""
}

// tclWordAfter returns the whitespace-delimited field following the first
// occurrence of kw (case-insensitive), or "".
func tclWordAfter(fields []string, kw string) string {
	for i := 0; i+1 < len(fields); i++ {
		if strings.EqualFold(fields[i], kw) {
			return fields[i+1]
		}
	}
	return ""
}

// tclTableToken reduces one whitespace-delimited token to the bare table name
// the taint tracker keys on.
//
// The COLUMN LIST is the load-bearing part: "INSERT INTO t1(a,ax,b) SELECT
// ..." puts "t1(a,ax,b)" in ONE field, and a plain quote/paren Trim leaves
// "t1(a,ax,b" -- a key no later read can ever match, so the table was never
// actually tainted. Cut at the paren instead.
//
// A schema qualifier is dropped for the same reason: readsOther matches bare
// identifier WORDS, so a key holding "main.t1" could never match either.
//
// Both bugs, and the WITH one above, were invisible for as long as the engine
// declined every random()-sourced INSERT. The moment it stopped,
// btree02.test's and orderby1.test's random-filled tables came back as WRONG
// ANSWERS over content that is not reproducible by construction.
func tclTableToken(tok string) string {
	if i := strings.IndexByte(tok, '('); i >= 0 {
		tok = tok[:i]
	}
	tok = strings.Trim(tok, `"'`+"`"+`();,`)
	if i := strings.LastIndexByte(tok, '.'); i >= 0 {
		tok = tok[i+1:]
	}
	return tok
}

// tclHasTopLevelOrderBy reports whether stmt carries an ORDER BY that actually
// constrains the ROW ORDER of its result -- i.e. one at parenthesis depth 0.
// An ORDER BY nested inside parentheses orders something else: a window
// function's own OVER(...) spec (which decides that function's VALUES, not the
// statement's row order -- window9.test's
// "SELECT name, color, dense_rank() OVER (ORDER BY name) ... FROM fruits"
// returns its rows in no guaranteed order, and C SQLite's happens to follow
// the LAST window's sorter), a subquery, or an aggregate's own ORDER BY. None
// of those makes the outer result order-sensitive, and treating them as if
// they did scored a correct answer as WRONG purely on row order.
func tclHasTopLevelOrderBy(stmt string) bool {
	low := strings.ToLower(stmt)
	depth := 0
	inStr, inQuo := false, false
	for i := 0; i < len(low); i++ {
		switch c := low[i]; {
		case inStr:
			if c == '\'' {
				inStr = false
			}
		case inQuo:
			if c == '"' {
				inQuo = false
			}
		case c == '\'':
			inStr = true
		case c == '"':
			inQuo = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && c == 'o' && strings.HasPrefix(low[i:], "order by"):
			return true
		}
	}
	return false
}

// ---- the six statement kinds a savepoint rollback cannot undo ----

// tclNameRe is one SQL name as SQLite spells it: bare, 'string', "identifier"
// or [bracketed]. A database name is an EXPRESSION in SQLite's grammar, so all
// four spell the same database.
const tclNameRe = `(?:[A-Za-z_][A-Za-z_0-9]*|'(?:[^']|'')*'|"(?:[^"]|"")*"|\[[^\]]*\])`

var (
	tclDetachDBRe  = regexp.MustCompile(`(?is)^\s*DETACH\s+(?:DATABASE\s+)?(` + tclNameRe + `)\s*;?\s*$`)
	tclPragmaDBRe  = regexp.MustCompile(`(?is)^\s*PRAGMA\s+(` + tclNameRe + `)\s*\.`)
	tclVacuumDBRe  = regexp.MustCompile(`(?is)^\s*VACUUM\s+(` + tclNameRe + `)\s*;?\s*$`)
	tclAnalyzeDBRe = regexp.MustCompile(`(?is)^\s*(?:ANALYZE|REINDEX)\s+(` + tclNameRe + `)\s*\.`)
	// The UNqualified "ANALYZE <name>" form, and the table-naming
	// foreign_key_check pragma. Both name an OBJECT rather than a database,
	// which is why tclStatementDatabaseName cannot answer for them.
	tclAnalyzeObjRe = regexp.MustCompile(`(?is)^\s*ANALYZE\s+(` + tclNameRe + `)\s*;?\s*$`)
	tclFKCheckObjRe = regexp.MustCompile(`(?is)^\s*PRAGMA\s+(?:` + tclNameRe + `\s*\.\s*)?foreign_key_check\s*[=(]\s*(` + tclNameRe + `)\s*\)?\s*;?\s*$`)
	// The name an ATTACH binds. Greedy up to the LAST "AS", because the path is
	// an arbitrary expression ("ATTACH 'a'||'b' AS x") that may contain the word
	// itself inside a string literal.
	tclAttachAsRe = regexp.MustCompile(`(?is)^\s*ATTACH\s+(?:DATABASE\s+)?.*\sAS\s+(` + tclNameRe + `)\s*;?\s*$`)
)

// tclUnquoteName strips one layer of SQLite name quoting.
func tclUnquoteName(s string) string {
	if len(s) < 2 {
		return s
	}
	switch {
	case s[0] == '\'' && s[len(s)-1] == '\'':
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	case s[0] == '"' && s[len(s)-1] == '"':
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	case s[0] == '[' && s[len(s)-1] == ']':
		return s[1 : len(s)-1]
	}
	return s
}

// tclStatementDatabaseName returns the DATABASE a statement names and cannot
// run without, or "" when it names none.
//
// It exists for the six kinds tclExecProbeSafe refuses to probe -- PRAGMA,
// VACUUM, ATTACH, DETACH, REINDEX, ANALYZE -- where a Go decline could never be
// checked against the oracle at all, and so was always filed as a coverage gap
// even when C SQLite rejects the statement for the very same reason. For all
// four forms matched above, C SQLite resolves the name through
// sqlite3FindDb/sqlite3TwoPartName and rejects the WHOLE STATEMENT when it does
// not resolve, so "the oracle does not have this database attached either" is a
// complete answer: it cannot be anything but a mutual reject. Each form was
// verified directly rather than reasoned out -- see TestTCLOracleHasDatabase,
// which runs every one of them against the oracle with nothing attached and
// requires an error.
//
// "main" and "temp" are deliberately never claimed. SQLite resolves both
// unconditionally (aDb[0] and aDb[1]) while pragma_database_list only LISTS
// temp once the temp database has been opened, so consulting the list for those
// two would answer a different question -- and would hide the real
// "PRAGMA temp.default_cache_size" gap the corpus does have.
func tclStatementDatabaseName(stmt string) string {
	var m []string
	for _, re := range []*regexp.Regexp{tclDetachDBRe, tclPragmaDBRe, tclVacuumDBRe, tclAnalyzeDBRe} {
		if m = re.FindStringSubmatch(stmt); m != nil {
			break
		}
	}
	if m == nil {
		return ""
	}
	name := tclUnquoteName(m[1])
	if strings.EqualFold(name, "main") || strings.EqualFold(name, "temp") {
		return ""
	}
	return name
}

// tclOracleRefusesDetach reports whether stmt is a DETACH of "main" or "temp",
// which C SQLite ALWAYS rejects -- the one place tclStatementDatabaseName's
// deliberate silence about those two names costs a mutual reject.
//
// DETACH is the only one of the four forms above for which resolving the name
// is not the end of the question: sqlite3FindDb resolves main and temp fine,
// and detachFunc then refuses them anyway. Both refusals are unconditional and
// neither depends on anything the connection holds, because no ATTACH can ever
// put a database under either name (tclOracleAttachNameInUse's own rule).
// Verified directly against mattn/go-sqlite3 3.53.3, with and without a temp
// table in existence:
//
//	DETACH main / DETACH DATABASE 'MAIN'  -> "cannot detach database main"
//	DETACH temp / DETACH Temp             -> "no such database: temp"
//
// Both are exactly what this engine answers, so booking them as coverage gaps
// was pure accounting: three mined statements in attach.test.
func tclOracleRefusesDetach(stmt string) bool {
	m := tclDetachDBRe.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	name := tclUnquoteName(m[1])
	return strings.EqualFold(name, "main") || strings.EqualFold(name, "temp")
}

// tclLockProxySetValue parses "PRAGMA [main.]lock_proxy_file = <v>" (and the
// equivalent call form) and returns v unquoted. ok is false for the GETTER, for
// any other pragma, and for a qualifier naming anything but main.
//
// The qualifier matters and cannot be dropped the way tclR32OPragmaSplit drops
// it: pragma.c:1104-1105 takes the pager out of pDb->pBt, i.e. out of the
// database the qualifier names, so "PRAGMA aux.lock_proxy_file=p" is about a
// different FILE with its own independent proxy state. Only main is tracked
// here, so only main is claimed.
func tclLockProxySetValue(stmt string) (value string, ok bool) {
	s := strings.TrimSpace(stmt)
	if len(s) < 6 || !strings.EqualFold(s[:6], "PRAGMA") {
		return "", false
	}
	s = strings.TrimSpace(s[6:])
	if dot := strings.Index(s, "."); dot >= 0 && !strings.ContainsAny(s[:dot], " \t=(") {
		if schema := tclUnquoteName(strings.TrimSpace(s[:dot])); !strings.EqualFold(schema, "main") {
			return "", false
		}
		s = strings.TrimSpace(s[dot+1:])
	}
	i := 0
	for i < len(s) && (s[i] == '_' || (s[i] >= 'a' && s[i] <= 'z') ||
		(s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if !strings.EqualFold(s[:i], "lock_proxy_file") {
		return "", false
	}
	return tclR32OPragmaValue(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[i:]), ";")))
}

// tclOracleLockProxy tracks the ORACLE connection's macOS proxy-locking state
// for its MAIN database -- whether proxy locking is on, and the explicit path
// it carries -- purely from the lock_proxy_file setters runTCLSegment actually
// forwarded to the oracle and that the oracle ACCEPTED. It runs nothing of its
// own, exactly like tclSavepointTracker.
//
// It exists to make tclOracleRefusesLockProxySet's probe REVERSIBLE; see that
// function for why the state has to be known in advance rather than read back
// with a getter.
//
// The state machine is proxyFileControl's SET arm, os_unix.c:8248-8282:
//
//	empty value    -> SQLITE_ERROR once proxy locking is on, a no-op while it
//	                  is off; either way nothing to record (an error never
//	                  reaches this tracker, which is only called on an accept).
//	off, any path  -> proxyTransformUnixFile turns it on, and stores a NULL
//	                  lockProxyPath for ":auto:" (os_unix.c:8154-8158).
//	on, ":auto:" or the SAME path -> proxyFileControl's own short-circuit
//	                  (os_unix.c:8268-8272): SQLITE_OK, nothing changes.
//	on, a different path -> switchLockProxyPath stores it (os_unix.c:8086-8100).
type tclOracleLockProxy struct {
	on   bool
	path string // "" while ":auto:" is in force: C stores a NULL lockProxyPath
}

// noteAccepted updates the tracker for one lock_proxy_file setter the ORACLE
// accepted. Call it unconditionally; every other statement is a no-op.
func (lp *tclOracleLockProxy) noteAccepted(stmt string) {
	v, ok := tclLockProxySetValue(stmt)
	if !ok || v == "" {
		return
	}
	switch {
	case !lp.on:
		lp.on = true
		if v != ":auto:" {
			lp.path = v
		}
	case v == ":auto:" || v == lp.path:
		// short-circuited: nothing changed.
	default:
		lp.path = v
	}
}

// tclOracleRefusesLockProxySet answers, for a "PRAGMA lock_proxy_file=<path>"
// this engine refused with pragma.c:1115's own message, whether C SQLite
// refuses it too -- which for the one such statement in the whole corpus
// (lock6.test#1's #8, under the read transaction its #6/#7 opened) it does,
// making the pair differential AGREEMENT rather than a coverage gap.
//
// It has to ASK the oracle rather than reason from this engine's refusal,
// because the two do not refuse in the same places. PragmaTuningDB.LockHeld is
// deliberately conservative -- it answers "assume locked" for any open
// transaction, and for WAL -- while C's rule is the exact pFile->eFileLock,
// which a bare BEGIN with nothing run under it leaves at NO_LOCK
// (TestPragmaLockProxyFileConservativeRefusals pins both over-refusals). Keying
// a mutual reject on this engine's own error would book those as agreement,
// which is the one thing a decline must never be allowed to do.
//
// Asking is safe ONLY in the state this function insists on, and that is what
// every guard below is for. The probe runs the setter for real, and running it
// is a no-op EXCEPT when the oracle accepts it:
//
//   - switchLockProxyPath's and proxyTransformUnixFile's SQLITE_BUSY is each
//     function's very first statement (os_unix.c:8082-8084, :8150-8152), so a
//     refusal changes nothing at all -- no conch, no lock, no path.
//   - an ACCEPT does change the oracle, and only one of the two accepts can be
//     undone: switchLockProxyPath merely swaps the stored path
//     (os_unix.c:8086-8100), so re-setting the old one restores it exactly,
//     while proxyTransformUnixFile is IRREVERSIBLE ("turn off proxy locking -
//     not supported", os_unix.c:8254-8258) and additionally swaps in
//     proxyIoMethods, which silently disables WAL for the rest of the segment
//     (TestPragmaLockProxyFileDisablesWAL). So the probe is taken only when
//     the tracker proves proxy locking is ALREADY on with a known explicit
//     path to restore, and never when it would be the transform.
//
// A ":auto:" or same-path value is excluded too: proxyFileControl short-circuits
// both to SQLITE_OK before the lock is ever consulted (os_unix.c:8268-8272), so
// the oracle would accept where this engine also accepts, and the question
// could not have arisen. Everything not claimed here stays the unsupported gap
// it already was -- the conservative direction every sibling probe takes.
func tclOracleRefusesLockProxySet(cgodb *sql.DB, lp *tclOracleLockProxy, stmt string, execErr error) bool {
	if execErr == nil || !strings.Contains(execErr.Error(), "failed to set lock proxy file") {
		return false
	}
	v, ok := tclLockProxySetValue(stmt)
	if !ok || v == "" || v == ":auto:" {
		return false
	}
	if !lp.on || lp.path == "" || v == lp.path {
		return false
	}
	if _, err := cgodb.Exec(stmt); err != nil {
		return true // SQLITE_BUSY, before anything was touched
	}
	// The oracle took it: a real coverage gap, and one this function now has to
	// put back. Record the new path first so the tracker stays honest even if
	// the restore below fails (it cannot in principle -- switchLockProxyPath
	// just succeeded, so eFileLock is still NO_LOCK -- but a tracker that lied
	// about the oracle's path would poison every later probe).
	old := lp.path
	lp.path = v
	if _, err := cgodb.Exec(`PRAGMA lock_proxy_file='` + strings.ReplaceAll(old, "'", "''") + `'`); err == nil {
		lp.path = old
	}
	return false
}

// tclOracleAttachNameInUse reports whether stmt is an ATTACH whose schema name
// C SQLite would refuse as "database <name> is already in use".
//
// It is tclOracleHasDatabase's exact INVERSE, and the reason ATTACH needed its
// own answer: every other statement in the no-probe set needs the database it
// names to EXIST, while an ATTACH needs the name to be FREE. Without this a
// re-ATTACH of a live name was filed as a coverage gap even though the oracle
// rejects it with the very message this engine produced -- 17 statements
// (attach, attach2, auth, pager1, pagerfault, pragma, savepoint, shared,
// unionvtabfault).
//
// The test C SQLite applies is attachFunc's own: a case-insensitive scan of
// db->aDb, which ALWAYS holds "main" and "temp" whether or not either has a
// b-tree yet. So those two are refused unconditionally, and only the rest come
// from database_list. Verified directly against mattn/go-sqlite3 3.53.3:
//
//	ATTACH 'x.db' AS temp / AS main / AS TeMp -> "database ... is already in use",
//	                       before any temp table exists and after one does
//	ATTACH 'x3.db' AS aux; ATTACH 'x4.db' AS aux  -> in use (and "AS AUX" too)
//	ATTACH 'x3.db' AS aux2                         -> accepted (one file, two names)
//	DETACH aux; ATTACH 'x5.db' AS aux              -> accepted (the name is free)
//
// A pure READ of the oracle, like tclOracleHasDatabase, so it can answer for a
// statement kind tclCGOExecAlsoRejects must not run. Any error answers "not in
// use", which leaves the statement counted as the gap it was before.
func tclOracleAttachNameInUse(cgodb *sql.DB, stmt string) bool {
	m := tclAttachAsRe.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	name := tclUnquoteName(m[1])
	if strings.EqualFold(name, "main") || strings.EqualFold(name, "temp") {
		return true
	}
	var n int
	if err := cgodb.QueryRow(
		"SELECT count(*) FROM pragma_database_list WHERE lower(name) = lower(?)", name,
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// tclOracleAttachAtLimit reports whether the oracle connection already holds
// SQLite's maximum number of ATTACHed databases, so a further ATTACH must fail
// there for the same reason it failed here.
//
// It is the third of the free, PURE-READ questions this file asks about the
// statement kinds tclCGOExecAlsoRejects must not run (tclOracleHasDatabase and
// tclOracleAttachNameInUse are the other two): the answer comes out of
// pragma_database_list, which changes nothing and needs no savepoint.
//
// The limit is SQLITE_MAX_ATTACHED, 10, and both engines enforce it with the
// same wording -- verified directly against 3.53.3, where the 11th ATTACH is
// "too many attached databases - max 10", exactly what engine/attach.go
// reports. attach.test asks for db11, db12 and db13 in one segment, and all
// three were booked as coverage gaps for want of this question.
//
// main and temp are excluded from the count: the limit is on ATTACHments, and
// pragma_database_list lists main always and temp once it has been opened. Any
// error answers "no", leaving the statement counted as the gap it was before.
func tclOracleAttachAtLimit(cgodb *sql.DB, stmt string) bool {
	if tclAttachAsRe.FindStringSubmatch(stmt) == nil {
		return false
	}
	var n int
	if err := cgodb.QueryRow(
		"SELECT count(*) FROM pragma_database_list WHERE lower(name) NOT IN ('main','temp')",
	).Scan(&n); err != nil {
		return false
	}
	return n >= 10
}

// tclOracleLacksAnalyzeTarget reports whether the oracle would reject this
// statement because the OBJECT it names does not exist there.
//
// It is the fourth of this file's free, PURE-READ questions about the statement
// kinds tclCGOExecAlsoRejects must not run (see tclOracleHasDatabase,
// tclOracleAttachNameInUse, tclOracleAttachAtLimit). ANALYZE and PRAGMA are two
// of those kinds, and the existing question only covers a DATABASE qualifier --
// these two name an object instead. Verified against 3.53.3:
//
//	ANALYZE no_such_table                    no such table: no_such_table
//	PRAGMA foreign_key_check=no_such_table   no such table: no_such_table
//	ANALYZE t1 / ANALYZE i1 / ANALYZE main   all fine (table, INDEX, database)
//	PRAGMA table_info=no_such_table          FINE, zero rows -- not in this set
//
// So ANALYZE takes a table, an index OR a database name, which is why the
// lookup accepts any of the three; a name it finds none of is one the oracle
// rejects for the same reason this engine did. Any error answers "no", leaving
// the statement counted as the coverage gap it was before.
func tclOracleLacksAnalyzeTarget(cgodb *sql.DB, stmt string) bool {
	var name string
	if m := tclAnalyzeObjRe.FindStringSubmatch(stmt); m != nil {
		name = tclUnquoteName(m[1])
	} else if m := tclFKCheckObjRe.FindStringSubmatch(stmt); m != nil {
		name = tclUnquoteName(m[1])
	}
	if name == "" {
		return false
	}
	// ANALYZE may name a DATABASE, which is not an object in sqlite_master.
	if strings.EqualFold(name, "main") || strings.EqualFold(name, "temp") {
		return false
	}
	var n int
	if err := cgodb.QueryRow(
		"SELECT (SELECT count(*) FROM sqlite_master WHERE lower(name)=lower(?))"+
			" + (SELECT count(*) FROM sqlite_temp_master WHERE lower(name)=lower(?))"+
			" + (SELECT count(*) FROM pragma_database_list WHERE lower(name)=lower(?))",
		name, name, name,
	).Scan(&n); err != nil {
		return false
	}
	return n == 0
}

// tclOracleQueryOnlyRefuses reports whether the oracle would refuse stmt for
// the identical reason this engine did: "PRAGMA query_only=ON" is in effect.
//
// It is the fifth of this file's free, PURE-READ questions about the
// statement kinds tclCGOExecAlsoRejects must not run -- ANALYZE and VACUUM are
// two of those kinds, and both open an ordinary write transaction that
// query_only refuses outright, unconditional on the statement's shape.
// SQLite's own source settles it: OP_Transaction refuses ANY write-opening
// statement while db->flags&SQLITE_QueryOnly, before touching a single page
// (vdbe.c:4108-4116, "Writes prohibited by the PRAGMA query_only=TRUE
// statement"), and sqlite3Analyze opens exactly that kind of write
// transaction via sqlite3BeginWriteOperation (analyze.c:1402) -- so
// "ANALYZE" under query_only=ON is not a coverage gap, it is what real
// SQLite does too (compat-harness/testdata/tcl/queryonly.test's own
// "catchsql {ANALYZE;}" expects exactly {1 {attempt to write a readonly
// database}}).
//
// The check does not trust Go's error text alone -- errQueryOnlyWrite
// (engine/pragma.go) is the ONLY source of that exact message, so a match
// already proves db.queryOnly was true on the Go side, but the oracle's OWN
// "PRAGMA query_only" is read back too (a pure READ, nothing to undo) as the
// actual evidence the oracle refuses this statement for the same reason: the
// two connections process the identical lockstep statement sequence, and a
// "PRAGMA query_only=ON" the Go engine accepts is always replayed against
// this SAME long-lived cgodb connection for real (the "" classification
// branch), never inside a rolled-back savepoint, so its flag is genuinely
// live here.
func tclOracleQueryOnlyRefuses(cgodb *sql.DB, execErr error) bool {
	if execErr == nil || !strings.Contains(execErr.Error(), "attempt to write a readonly database") {
		return false
	}
	var v int
	if err := cgodb.QueryRow("PRAGMA query_only").Scan(&v); err != nil {
		return false
	}
	return v != 0
}

// tclOracleRefusesVacuumInTxn reports whether the oracle would refuse stmt for
// the identical reason this engine did: an explicit transaction is open.
//
// It is the sixth of this file's free, PURE-READ questions about the
// statement kinds tclCGOExecAlsoRejects must not run. VACUUM (bare, schema-
// qualified, or INTO) is one of those kinds, and its in-transaction guard is
// unconditional on the statement's shape -- vacuum.c:169-171
// (sqlite3RunVacuum, what OP_Vacuum's runtime calls into) checks
// `!db->autoCommit` as the very FIRST thing it does, before opening any Btree
// transaction or writing a single page, and fails with exactly the text
// engine/vacuum_write.go (execVacuum/execVacuumInto) already reproduces
// verbatim: "cannot VACUUM from within a transaction". So an open oracle
// transaction is both necessary and sufficient for the identical rejection --
// no exec probe is needed (and tclExecProbeSafe correctly refuses to run one:
// a savepoint rollback cannot undo a SUCCESSFUL vacuum, but this statement
// never reaches that far).
//
// The check does not trust Go's error text alone -- that exact string is
// emitted nowhere else in this write path -- but the oracle's OWN transaction
// state is still read back (tclCGOInTransaction: a pure BEGIN/ROLLBACK probe,
// nothing to undo) as the actual evidence, the same discipline
// tclOracleQueryOnlyRefuses and tclR32OOracleRefusesSyncInTxn use.
func tclOracleRefusesVacuumInTxn(cgodb *sql.DB, execErr error) bool {
	if execErr == nil || !strings.Contains(execErr.Error(), "cannot VACUUM from within a transaction") {
		return false
	}
	return tclCGOInTransaction(cgodb)
}

// tclOracleHasDatabase reports whether the oracle connection currently has a
// database of that name attached. It is a pure READ of pragma_database_list --
// no savepoint, no side effect, nothing to undo -- which is the whole reason it
// can answer for the statement kinds tclCGOExecAlsoRejects must not touch. Any
// error answers "yes", so a failure to consult the oracle leaves the statement
// counted as the coverage gap it was before.
func tclOracleHasDatabase(cgodb *sql.DB, name string) bool {
	if name == "" {
		return true
	}
	var n int
	if err := cgodb.QueryRow(
		"SELECT count(*) FROM pragma_database_list WHERE lower(name) = lower(?)", name,
	).Scan(&n); err != nil {
		return true
	}
	return n > 0
}

// tclAttachCannotPrepare answers, for a declined ATTACH or DETACH, whether the
// oracle rejects it at PREPARE time -- the one question about this statement
// kind that costs nothing. sqlite3_prepare_v2 compiles and resolves names but
// executes nothing, and an ATTACH's file is only opened when the statement is
// STEPPED, so a prepared-and-discarded ATTACH attaches nothing. Measured
// directly against mattn/go-sqlite3 3.53.3 over every mined ATTACH/DETACH this
// engine declines: after the prepare, pragma_database_list is byte-identical
// and the directory holds exactly the files it held before -- for the ones the
// oracle accepts as readily as the ones it refuses.
//
// It is deliberately restricted to ATTACH/DETACH, and deliberately NOT a
// widening of tclExecProbeSafe: a savepoint rollback does not undo an ATTACH,
// so the statement must never be RUN on the shared oracle. That is also its
// limit, and the limit is worth stating because a previous stream got it
// backwards. Of the mined ATTACH/DETACH declines, the oracle refuses only TWO
// at prepare time:
//
//	alter.test#3   ATTACH 'test3.db' AS ON        -> near "ON": syntax error
//	attach.test#6  DETACH RAISE(IGNORE) IN (...)  -> no such table: AAAAAA
//
// Three more ARE mutual rejections, but only at EXEC, so no read-only probe
// can reach them and they stay counted as gaps:
//
//	attach3.test#1 '/nodir/nofile.x'      -> unable to open database file
//	uri.test#3     'file:...?vfs=tvfs2'   -> no such vfs: tvfs2
//	uri.test#5     'file:test.db2?mode=rw'-> unable to open database file
//
// and the rest are genuine gaps the oracle runs successfully (attach.test#8's
// second ATTACH of a file this session has written, attach.test#9's
// mode=memory&cache=shared URI, exclusive.test#0's ATTACH under exclusive
// locking mode). So this lowers the decline count by MEASURING two statements
// correctly, not by hiding any.
func tclAttachCannotPrepare(cgodb *sql.DB, stmt, cgoStmt string) bool {
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "ATTACH", "DETACH":
		return tclCGOCannotPrepare(cgodb, cgoStmt)
	}
	return false
}

// tclVacuumCannotPrepare answers, for a declined VACUUM (or VACUUM INTO),
// whether the oracle rejects it at PREPARE time -- the one question about
// this statement kind that costs nothing, for the same reason
// tclAttachCannotPrepare's own comment gives: sqlite3_prepare_v2 compiles
// and resolves names but executes nothing, and VACUUM's actual work (the
// page copy, or for VACUUM INTO, the file write) happens only in
// sqlite3RunVacuum, which OP_Vacuum reaches on STEP, never on prepare
// (vacuum.c:105-137's sqlite3Vacuum only emits the opcode; vacuum.c:139ff's
// sqlite3RunVacuum is what actually runs it). So a statement that fails to
// prepare had no side effect, and this is a strictly weaker, always-safe
// question for the whole VACUUM family -- which tclExecProbeSafe refuses to
// RUN for a different, unrelated reason (a savepoint rollback doesn't fully
// undo a completed vacuum).
//
// This closes the specific gap VACUUM INTO's own target-expression rules
// open: resolve.c's sqlite3ResolveSelfReference doc comment (case (4),
// "Expression arguments to VACUUM INTO") says a TK_COLUMN node in the
// target expression is ALWAYS an error, and vacuum.c:128 routes that
// expression through the same resolver execVacuumInto's own doc comment
// (engine/vacuum_write.go, requireZeroedNameContext) cites -- so a target
// like a bare column, an unresolved table-qualified column, or a call to a
// function neither engine has all fail at PREPARE, before OP_Vacuum ever
// runs. The mined statement this was raised for is vacuum-into.test#2,
// "VACUUM INTO target()": target() is a genuine undefined function to a
// bare connection (the full .test file registers it as a TCL-side custom
// function via "db func target target", which this corpus miner does not
// reproduce, but a vanilla oracle connection -- verified directly against
// mattn/go-sqlite3 3.53.3 -- sees exactly what this engine's own resolver
// sees: "no such function: target"). That single statement is the only one
// of the file's self-reference negative cases the miner actually SEES --
// tclKeywordRegex has no do_catchsql_test alternative (only
// do_execsql_test/execsql/catchsql/db-eval, and the leading "do_" defeats a
// bare \bcatchsql\b match the same way \bexecsql\b is defeated inside
// do_execsql_test, see tclKeywordRegex's own doc comment), so the source
// .test file's four do_catchsql_test siblings ("VACUUM INTO x" /
// "t1.nosuchcol" / "main.t1.nosuchcol" / "target2()") are never mined and
// were never counted as gaps at all -- this fix does not move their tally.
// They ARE the same resolver rule, though (confirmed directly against the
// oracle: every one also fails to PREPARE, with "no such column: <expr>"
// or "no such function: target2"), so this probe is written against the
// general rule rather than a string match on "target()" alone, and the
// sibling vacuum_selfref_prepare_probe_test.go pins all five directly so
// the general rule stays covered even though only one is corpus-visible.
//
// "VACUUM INTO null" is NOT covered here on purpose: C SQLite's own
// target-type check (vacuum.c:178-183, "non-text filename") runs inside
// sqlite3RunVacuum at STEP, not at prepare (a NULL literal has no
// TK_COLUMN/TK_FUNCTION node for the resolver in case (4) to reject), so
// the oracle prepares it fine -- probing that one further would mean
// actually EXECUTING it on the shared oracle, which is exactly what
// tclExecProbeSafe's denylist exists to prevent for this family.
func tclVacuumCannotPrepare(cgodb *sql.DB, stmt, cgoStmt string) bool {
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return false
	}
	if strings.ToUpper(fields[0]) != "VACUUM" {
		return false
	}
	return tclCGOCannotPrepare(cgodb, cgoStmt)
}

// tclFts5ConfigCannotPrepare answers, for a declined fts5 CONFIGURATION-channel
// INSERT ("INSERT INTO t(t, rank) VALUES('pgsz', 32)"), whether the oracle
// rejects it at PREPARE time -- the one question about this statement kind that
// costs nothing.
//
// tclExecProbeSafe refuses to RUN these on the shared oracle, and for a good
// reason (its own doc comment): the command writes a %_config row AND flips a
// flag cached on the connection, and a savepoint rollback only takes the row
// back. But that reason is entirely about the RUNNING. sqlite3_prepare_v2
// compiles and resolves names and executes nothing, so a statement that fails
// to prepare cannot have had a side effect, and one that DOES prepare is
// discarded here unstepped -- fts5's xUpdate, which is where the whole command
// channel lives, only runs on a step.
//
// This is an ACCOUNTING correction of the same kind tclCGOCannotPrepare's own
// comment describes, and it is the LARGEST one left in the fts5-gated corpus.
// Measured over the '[d-f]' chunk under -tags sqlite_fts5, 95 of that chunk's
// 338 declines were this single shape, and every one of them is a mutual
// rejection: the fts5 test files write their CREATE as
// "CREATE VIRTUAL TABLE t1 USING fts5(x, y, detail=%DETAIL% %TOKENIZER%)",
// whose TCL placeholders make it `parse error in "detail=%DETAIL% %TOKENIZER%"`
// for REAL fts5 too (ext/fts5/fts5_config.c: the argument gobble leaves
// trailing text, so z is set to 0). Neither engine has t1, both reject the
// configuration INSERT that follows with "no such table: t1", and leaving it
// unprobed booked all 95 as a musql coverage gap.
func tclFts5ConfigCannotPrepare(cgodb *sql.DB, stmt, cgoStmt string) bool {
	if !tclIsFts5ConfigInsert(stmt) {
		return false
	}
	return tclCGOCannotPrepare(cgodb, cgoStmt)
}

// tclAttachPathRe captures the single-quoted path literal of a leading
// "ATTACH [DATABASE] '<path>'".
// tclLeadingNoiseRe is what may precede the keyword: whitespace and SQL comments.
// A mined statement very often opens with the test file's own "-- Create a large
// RBU database." line, and anchoring on \s* alone silently DID NOT MATCH it -- so
// the oracle's copy was not rewritten, both engines resolved the same relative
// path, and they shared ONE attached file in the Go engine's working directory.
//
// That was invisible for as long as both engines wrote the SAME format: C read the
// file musql had just created and the segment passed. The moment musql's ATTACH
// started writing OUR format (engine/attach_write.go), C answered "file is not a
// database" and rbutemplimit.test reported four WRONG results -- an engine change
// exposing a harness bug, not causing one. The isolation this regex exists to
// perform has to see the keyword whatever comes before it.
const tclLeadingNoise = `(?:\s|--[^\n]*|/\*.*?\*/)*`

var tclAttachPathRe = regexp.MustCompile(`(?is)^` + tclLeadingNoise + `ATTACH\s+(?:DATABASE\s+)?'((?:[^']|'')*)'`)

// tclVacuumIntoPathRe captures the single-quoted target of a leading
// "VACUUM [schema] INTO '<path>'". It needs the same isolation as ATTACH, and
// more urgently: VACUUM INTO REFUSES a target that already exists ("output
// file already exists", verified against 3.53.3), so with one working
// directory between the two engines the Go engine would write ./out.db and the
// oracle -- replaying the very same statement a moment later -- would refuse
// it. That is scored "Go accepted a statement that C SQLite rejected", i.e. a
// WRONG answer manufactured entirely by the two engines sharing a directory.
// Six mined statements across e_vacuum.test and vacuum-into.test are this
// shape, every one of them with a relative target.
var tclVacuumIntoPathRe = regexp.MustCompile(`(?is)^` + tclLeadingNoise + `VACUUM\s+(?:[A-Za-z_][A-Za-z_0-9]*\s+)?INTO\s+'((?:[^']|'')*)'`)

// tclIsolateAttach rewrites a RELATIVE-path ATTACH so its database file lands
// in dir instead of the process working directory.
//
// runTCLSegment gives each engine its own MAIN database but runs both from ONE
// working directory, so a relative "ATTACH 'test2.db' AS aux" used to name the
// SAME FILE for both of them -- and they then wrote it in turn. That is not two
// views of one logical database the way the two main files are; it is two
// independent writers on one file, which is neither what the mined script does
// nor anything this harness means to test. attach_diff_test.go already states
// the rule for its own differential ("each engine gets its OWN attached files
// ... sharing one file between two live writers would test the filesystem
// rather than the SQL"); the corpus replay simply never applied it.
//
// What it cost: this engine's write session is rebuild-on-flush (engine/
// writer.go). It loads the attached database at its first write and rewrites
// the whole file at COMMIT, and staleImageCheck correctly REFUSES that commit
// once the file's change counter has moved -- which the oracle's own autocommit
// of the very same statement had just done. Replaying cacheflush.test#1 both
// ways shows exactly that (attach_cluster_test.go):
//
//	ATTACH 'test.db2' AS aux; CREATE TABLE aux.t4(x,y); BEGIN; ...; COMMIT
//	  shared:   COMMIT -> "committing ATTACHed database aux: database is
//	                       locked (SQLITE_BUSY)"
//	  isolated: COMMIT -> accepted by both engines
//
// 19 statements across 13 files (cacheflush, async5, attach, crash8,
// incrvacuum2, io, jrnlmode, notify1, pager1, pagerfault, shared, sync,
// table.test) were that one COMMIT.
//
// Only a relative path is rewritten. ':memory:' and the empty path name a
// private database with no file at all, and an absolute path is already
// unambiguous. A "file:" URI is rewritten too, but only in its PATH component
// and only when that is relative -- see tclR35BIsolateFileURI, and the
// 8_3_names.test#5 lock conflict that skipping it entirely used to manufacture.
//
// A "VACUUM [schema] INTO '<path>'" target is moved by the same rule and for a
// sharper reason -- see tclVacuumIntoPathRe.
func tclIsolateAttach(stmt, dir string) string {
	m := tclAttachPathRe.FindStringSubmatchIndex(stmt)
	if m == nil {
		m = tclVacuumIntoPathRe.FindStringSubmatchIndex(stmt)
	}
	if m == nil {
		return stmt
	}
	path := strings.ReplaceAll(stmt[m[2]:m[3]], "''", "'")
	if uri, ok := tclR35BIsolateFileURI(path, dir); ok {
		return stmt[:m[2]] + strings.ReplaceAll(uri, "'", "''") + stmt[m[3]:]
	}
	if path == "" || strings.EqualFold(path, ":memory:") ||
		strings.HasPrefix(strings.ToLower(path), "file:") || filepath.IsAbs(path) {
		return stmt
	}
	moved := strings.ReplaceAll(filepath.Join(dir, path), "'", "''")
	return stmt[:m[2]] + moved + stmt[m[3]:]
}

// tclFillsToPageCeiling names, per corpus file, the one mined statement whose
// ONLY way to terminate is a "PRAGMA max_page_count" ceiling. runTCLSegment
// books it outOfScope, unrun on either engine, once that segment's ceiling
// setter has been declined.
//
// sqllimits1.test:465-531 builds a tree of four mutually-firing triggers on
// "trig" and says of it, in its own comment, "a single insert adds (N^16 plus
// some) rows to the database. A really long loop...". sqllimits1-7.5 expects
// "database or disk is full" -- C stops it at the 1000-page ceiling 7.1 set,
// in getPageNormal (pager.c:5637, "pgno>pPager->mxPgno" -> SQLITE_FULL), after
// ~0.2s. This engine declines that setter by design
// (docs/segment-format-compatibility.md: our pages are not C's pages), and a
// declined setter is never replayed on the oracle, so NEITHER side has a
// ceiling and the insert runs until memory does: the musql side of the
// sqllimits1 chunk grew until the 2G cap throttled it to a crawl, and the
// sweep killed the chunk at its 1800s guard with no TOTAL line. Its whole
// observable is the ceiling this format declines, so there is nothing left to
// compare. Keyed on the declined setter rather than on the file alone, so the
// exclusion retires itself if max_page_count is ever given a meaning here.
var tclFillsToPageCeiling = map[string]string{
	"sqllimits1.test": "INSERT INTO trig VALUES (1,10)",
}

// tclMaxPageCountSetterRE matches a "PRAGMA [schema.]max_page_count = N" (or
// "(N)") setter; the bare getter does not match.
var tclMaxPageCountSetterRE = regexp.MustCompile(`(?i)^\s*PRAGMA\s+(?:\w+\.)?max_page_count\s*[=(]`)

func runTCLSegment(t *testing.T, label string, stmts []string, histogram map[string]int) tclTally {
	t.Helper()
	if len(stmts) > tclMaxStmtsPerFile {
		stmts = stmts[:tclMaxStmtsPerFile]
	}

	// A mined segment can contain "ATTACH 'test2.db' AS aux", whose path is
	// RELATIVE -- both engines resolve it against the process working
	// directory, which is this package's own source directory. Left alone that
	// would (a) create database files inside the source tree, which this
	// project forbids outright, and (b) let one segment's attached file leak
	// into every later segment that names it, since nothing else here resets
	// it. Run each segment with its own throwaway working directory instead, so
	// a relative ATTACH lands there and dies with the segment. tclAllFiles and
	// the corpus ReadFile both run before this, so nothing else depends on the
	// working directory while it is swapped.
	segDir := t.TempDir()
	prevWD, wdErr := os.Getwd()
	if wdErr != nil {
		t.Fatalf("Getwd: %v", wdErr)
	}
	if err := os.Chdir(segDir); err != nil {
		t.Fatalf("Chdir(%s): %v", segDir, err)
	}
	defer os.Chdir(prevWD)
	// ...and the ORACLE's relative ATTACHes are moved somewhere else again, so
	// the two engines never write ONE file between them. See tclIsolateAttach.
	cgoAttachDir := t.TempDir()

	goPath := filepath.Join(t.TempDir(), "go.db")
	cgoPath := filepath.Join(t.TempDir(), "cgo.db")
	godb, err := engine.Create(goPath)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", cgoPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	// A single logical connection: now that BEGIN/COMMIT/ROLLBACK are
	// replayed against cgodb too (see the package doc comment's "accepted"
	// case), a multi-statement transaction MUST land on the same underlying
	// SQLite connection for its whole lifetime -- database/sql's default
	// pool could otherwise hand a later statement in the same segment a
	// DIFFERENT idle connection, which would never see the BEGIN at all
	// (mirrors the identical fix already applied in vdbe_drop_test.go's
	// sdb.SetMaxOpenConns(1)).
	cgodb.SetMaxOpenConns(1)
	if _, err := cgodb.Exec("PRAGMA synchronous=OFF"); err != nil {
		t.Fatalf("cgo PRAGMA synchronous=OFF: %v", err)
	}
	// TCL_CROSS_INTEGRITY: nil (and every hook below a no-op) unless set. See
	// tcl_cross_integrity_test.go.
	xint := newTCLCrossIntegrity(t, label, cgoPath, cgoAttachDir)

	var tally tclTally
	tally.statements = len(stmts)
	tainted := tclRandomTainted{}
	savepoints := tclSavepointTracker{}
	lockProxy := tclOracleLockProxy{}
	// Set once this segment's "PRAGMA max_page_count" setter has been declined
	// -- which also means it never reached the oracle. See
	// tclFillsToPageCeiling.
	noPageCeiling := false
	file, _, _ := strings.Cut(label, "#")

	for i, stmt := range stmts {
		if noPageCeiling && tclFillsToPageCeiling[file] == stmt {
			tally.outOfScope++
			tclNoteOOS(t, "fillsToDeclinedPageCeiling", label, i, stmt)
			continue
		}
		if tclIsQuery(stmt) {
			if n := tclRandomMaterialization(stmt); n > tclRandomMaterializationBudget {
				// A RESOURCE decline, not a comparability one: this
				// statement's own result is perfectly comparable (see
				// tclRandomMaterializationBudget) -- it just builds gigabytes
				// of random blob on BOTH engines to produce it. Two
				// statements in the whole corpus, both in sort2/sort3.test.
				tally.outOfScope++
				tclNoteOOS(t, "randomBlobTooLarge", label, i, stmt)
				continue
			}
			gotCols, gotRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
			if panicked {
				tally.panics++
				t.Errorf("PANIC (Go query engine panicked -- must never happen)\n  segment: %s\n  stmt #%d: %s\n  panic: %v", label, i, stmt, panicVal)
				continue
			}
			if qerr != nil {
				// Go declined this query. Ask the oracle: if C SQLite ALSO
				// rejects it, that is genuine differential agreement -- both
				// engines reject the same statement given their identically-
				// realized state -- not a coverage gap. This covers TCL-only
				// functions the oracle build also lacks (tointeger()/toreal()/
				// randstr()), build-disabled tables (sqlite_stat4), and queries
				// against a table whose CREATE was itself declined-and-skipped on
				// both sides (so neither engine has it). A Go rejection can never
				// be a WRONG answer (a rejection is not an answer), so probing the
				// oracle here is safe against the never-wrong gate: the only two
				// outcomes are mutual-reject (agreement, counted separately from
				// real passes so cascade agreement stays visible) or oracle-
				// accepts (a real, still-counted coverage gap). Queries never
				// mutate state, so running one on the shared cgo connection can't
				// desynchronize the two engines' schemas -- with the one
				// exception tclQueryMutatesOracle names.
				if tclQueryMutatesOracle(stmt) {
					// ...which still leaves one question that can be asked for
					// free, because the side effect is in the RUNNING and this
					// asks only whether the oracle can PREPARE it. See
					// tclCGOCannotPrepare.
					if tclCGOCannotPrepare(cgodb, stmt) {
						tally.mutualReject++
						continue
					}
					tally.unsupported++
					tclDumpUnsupported(t, label, i, stmt, qerr)
					histogram[tclQueryUnsupportedBucket(stmt, qerr)]++
					continue
				}
				if _, _, cerr := tclRunCGOQuery(cgodb, stmt); cerr != nil {
					tally.mutualReject++
					continue
				}
				if tclIsOutOfScope(stmt, qerr) {
					tally.outOfScope++
					tclNoteOOS(t, "goDeclinedQuery", label, i, stmt)
					continue
				}
				tally.unsupported++
				tclDumpUnsupported(t, label, i, stmt, qerr)
				histogram[tclQueryUnsupportedBucket(stmt, qerr)]++
				continue
			}
			cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, stmt)
			if tclQueryMutatesOracle(stmt) {
				xint.query(i, stmt, cerr == nil)
			}
			if cerr != nil {
				if tclOracleLacksBuiltin(cerr) {
					tally.outOfScope++
					tclNoteOOS(t, "oracleLacksBuiltinQuery", label, i, stmt)
					continue
				}
				tally.wrong++
				tclLogWrong(t, "WRONG (Go accepted a SELECT that C SQLite rejected)\n  segment: %s\n  stmt #%d: %s\n  cgo error: %v\n  go: cols=%v rows=%v", label, i, stmt, cerr, gotCols, gotRows)
				continue
			}
			// An unaliased expression's result-column NAME is not defined by
			// SQL (it's an implementation artifact), and cgo derives it from
			// the verbatim source text -- which, for a select-list item written
			// across lines with a trailing "-- ..." comment (e.g. in.test's
			// "3 NOT IN (NULL,1,2) -- Ambiguous"), INCLUDES the comment, while
			// this engine's reconstructed name does not. The row VALUES are
			// identical; only the auto-name disagrees. Strip any trailing "--"
			// line-comment from BOTH sides before comparing so this pure naming
			// artifact isn't scored as a wrong answer (a genuine name
			// divergence survives the identical strip and is still caught).
			gCols := tclStripColComments(gotCols)
			cCols := tclStripColComments(cgoCols)
			// A result column C SQLite named with its own internal ":N"
			// disambiguation suffix has a NAME that SQLite's docs call
			// unspecified (and, per tclUnspecifiedAutoNameRE's doc comment,
			// not even deterministic past a couple of nesting levels) -- so
			// don't let that one artifact fail an otherwise-correct result.
			// Every other column's name is still compared exactly below.
			gCols, cCols = tclRelaxUnspecifiedColNames(gCols, cCols)
			orderSensitive := tclHasTopLevelOrderBy(stmt)
			ok, reason := queryResultsMatch(gCols, gotRows, cCols, cgoRows, orderSensitive)
			// A legitimate ORDER BY tie (two rows share every sort key; SQL
			// leaves their relative order unspecified) makes the strict
			// order-sensitive compare above spuriously "fail" when the engine
			// and cgo pick different orders for the tied rows. tclOrderByTieOK
			// confirms both results agree on every ORDER BY key at every
			// position (and hold the same multiset of rows) -- i.e. the only
			// difference is within tie groups -- and accepts it. See limit2.test.
			if !ok {
				// The oracle's DRIVER, not the oracle, converted a
				// date/datetime/timestamp-declared column -- see
				// tclReconcileDriverTimes.
				if crows, anyTS := tclReconcileDriverTimes(gotRows, cgoRows); anyTS {
					if same, _ := queryResultsMatch(gCols, gotRows, cCols, crows, orderSensitive); same {
						cgoRows = crows
						ok, reason = true, ""
					}
				}
			}
			if !ok && orderSensitive && tclOrderByTieOK(stmt, gCols, gotRows, cCols, cgoRows) {
				ok, reason = true, ""
			}
			if !ok && orderSensitive && tclOrderByKeyHidden(stmt, gCols) {
				// The ORDER BY key is not in the output, so a tie's row order
				// is unjudgeable from the result -- see tclOrderByKeyHidden.
				// Only a pure REORDERING qualifies; a row-set difference is
				// still WRONG below.
				if same, _ := queryResultsMatch(gCols, gotRows, cCols, cgoRows, false); same {
					tally.outOfScope++
					tclNoteOOS(t, "orderByKeyHidden", label, i, stmt)
					continue
				}
			}
			if !ok {
				// The values disagree. Before calling that WRONG, ask whether
				// this statement HAS a nondeterministic input at all -- it
				// calls random()/randomblob() itself, or it reads a table an
				// earlier statement filled with random content -- and if it
				// does, fall back to the strongest comparison that is still
				// sound instead of to no comparison at all. See
				// tcl_property_test.go: the column count and names are always
				// compared, the row count and the deterministic cells
				// whenever the statement fixes them, and each remaining cell
				// by the storage class (and byte length) its own generator
				// fixes. Reached ONLY here, after the strict compare has
				// already failed, so no statement that matches exactly is
				// ever judged by the weaker rule.
				if plan, nondet := tclPropPlanFor(stmt, gCols, tainted); nondet {
					pok, preason := tclPropertyMatch(plan, gCols, gotRows, cCols, cgoRows)
					if pok {
						tally.propertyMatch++
						continue
					}
					tally.wrong++
					tclLogWrong(t, "WRONG (%s: the VALUES are not reproducible but this PROPERTY is, and it disagrees)\n  segment: %s\n  stmt #%d: %s\n  property: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v", plan.why, label, i, stmt, preason, gotCols, gotRows, cgoCols, cgoRows)
					continue
				}
			}
			if !ok {
				tally.wrong++
				tclLogWrong(t, "WRONG ANSWER (engine claimed success but disagrees with C SQLite)\n  segment: %s\n  stmt #%d: %s\n  reason: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v", label, i, stmt, reason, gotCols, gotRows, cgoCols, cgoRows)
				continue
			}
			tally.pass++
			continue
		}

		// Only the ORACLE's copy of the statement is rewritten: the Go engine
		// keeps segDir, which it already had to itself before this. An ATTACH is
		// never a query, so the query branch above needs none of this.
		cgoStmt := tclIsolateAttach(stmt, cgoAttachDir)

		execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt)
		if panicked {
			tally.panics++
			t.Errorf("PANIC (Go write engine panicked -- must never happen)\n  segment: %s\n  stmt #%d: %s\n  panic: %v", label, i, stmt, panicVal)
			continue
		}
		if execErr != nil && tclMaxPageCountSetterRE.MatchString(stmt) {
			noPageCeiling = true
		}
		if execErr == nil {
			tainted.note(stmt)
			savepoints.recordAccepted(stmt)
		}
		switch tclClassifyExecErr(execErr) {
		case "":
			_, cerr := cgodb.Exec(cgoStmt)
			xint.exec(i, stmt, cerr == nil)
			if cerr != nil {
				// The SAME out-of-scope rule the query side applies: an oracle
				// build without generate_series/rtree/fts5 rejects a statement
				// this engine legitimately serves, and scoring that WRONG would
				// blame the engine for being more complete than this particular
				// oracle. Reachable on the exec side only since a table-valued
				// function became usable as a write statement's SELECT source
				// ("INSERT INTO t1 SELECT value, 1 FROM generate_series(...)",
				// which whereN/join8/merge1/tabfunc01/with2 all do).
				if tclOracleLacksBuiltin(cerr) {
					// The oracle ran NOTHING here, so anything this statement
					// wrote is now present on one side only: taint the target so
					// later reads of it are excluded rather than judged against a
					// table the oracle never filled.
					tainted.taintTarget(stmt)
					tally.outOfScope++
					tclNoteOOS(t, "oracleLacksBuiltinExec", label, i, stmt)
					continue
				}
				tally.wrong++
				tclLogWrong(t, "WRONG (Go accepted a statement that C SQLite rejected)\n  segment: %s\n  stmt #%d: %s\n  cgo error: %v", label, i, stmt, cerr)
				continue
			}
			lockProxy.noteAccepted(cgoStmt)
			tally.pass++
		case "unsupported":
			// Same differential-agreement rule as the query side: if the oracle
			// ALSO rejects this exec statement, that is agreement, not a coverage
			// gap (e.g. INSERT INTO a build-disabled sqlite_stat4, an INSERT with
			// the wrong value-count, a duplicate CREATE, a CREATE using a TCL-only
			// collation neither engine has). Unlike a query, an exec can mutate
			// state, so the probe wraps it in a rolled-back SAVEPOINT and only
			// runs for transactional DML/DDL (tclExecProbeSafe) -- a statement the
			// oracle accepts is fully undone, keeping the two engines' schemas in
			// the same lockstep the harness relies on. A Go rejection is never a
			// WRONG answer, so this stays safe against the never-wrong gate.
			probeRejects := false
			if tclExecProbeSafe(stmt) {
				var txnLost bool
				probeRejects, txnLost = tclCGOExecAlsoRejects(cgodb, cgoStmt)
				if txnLost {
					// The probed statement's ROLLBACK conflict policy tore the
					// oracle's whole transaction down, savepoint and all -- see
					// tclCGOExecAlsoRejects. Put the Go engine back on the same
					// footing (both in autocommit, both holding the state that
					// transaction started from) so the rest of this segment is
					// still a faithful lockstep replay. A ROLLBACK the engine
					// rejects, because it had no transaction of its own, means
					// there was nothing to put back: ignore it.
					tclSafeExecArgs(godb, "ROLLBACK")
					xint.exec(i, "ROLLBACK", false)
				}
			} else {
				// The six kinds a savepoint rollback cannot undo are never RUN on
				// the oracle -- but one question about them can still be answered
				// for free, with a pure READ: does the oracle even have the
				// database this statement names? If not, C SQLite rejects the
				// whole statement for exactly the reason this engine did, so that
				// is differential agreement, not a coverage gap. 36 of the ATTACH
				// cluster were this: a DETACH of a name that was never attached (or
				// was already detached), and a qualified PRAGMA/VACUUM naming a
				// database whose own ATTACH had been declined-and-mirrored earlier
				// in the segment. See tclStatementDatabaseName.
				// ...and the same free question, asked the other way round, for the
				// one kind in the set that needs its database name to be FREE rather
				// than to exist. See tclOracleAttachNameInUse.
				// ...and the same free question for fts5's CONFIGURATION
				// channel, which tclExecProbeSafe excludes for a reason that is
				// entirely about RUNNING it. See tclFts5ConfigCannotPrepare.
				// ...and the same free question for the one pragma spelling
				// whose rejection depends on the oracle's TRANSACTION state
				// rather than on its text. See tclR32OOracleRefusesSyncInTxn.
				// ...and the same free question for temp_store's sibling
				// rejection, which depends on BOTH the oracle's transaction
				// state AND whether its TEMP database is open. See
				// tclR32OOracleRefusesTempStoreInTxn.
				// ...and the same free question for the whole excluded set at
				// once, when the oracle's own "PRAGMA query_only" is ON: real
				// SQLite's OP_Transaction refuses any write-opening statement
				// unconditionally, so an ANALYZE (or VACUUM) this engine
				// declined for that exact reason is a mutual rejection too.
				// See tclOracleQueryOnlyRefuses.
				// ...and the same free question for VACUUM's OWN in-transaction
				// guard specifically (vacuum.c's `!db->autoCommit` check, distinct
				// from query_only): a "BEGIN; VACUUM" mined together is real
				// SQLite refusing itself, not a coverage gap. See
				// tclOracleRefusesVacuumInTxn.
				// ...and the same free question for VACUUM's own
				// target-expression resolution rules (VACUUM INTO x /
				// t1.nosuchcol / target()), which fail at PREPARE on both
				// sides. See tclVacuumCannotPrepare.
				// ...and the same free question for RELEASE/ROLLBACK TO naming
				// a savepoint this segment never opened (or already closed) --
				// SAVEPOINT/RELEASE are excluded from tclExecProbeSafe above
				// because a probe would manipulate the very stack it runs
				// inside, but the name-lookup failure mode needs no probe at
				// all: it is a pure named-stack search independent of
				// everything else. See tclSavepointNameMissing.
				probeRejects = tclOracleAttachNameInUse(cgodb, cgoStmt) ||
					tclOracleAttachAtLimit(cgodb, cgoStmt) ||
					tclOracleLacksAnalyzeTarget(cgodb, cgoStmt) ||
					tclOracleRefusesDetach(stmt) ||
					tclOracleQueryOnlyRefuses(cgodb, execErr) ||
					tclOracleRefusesVacuumInTxn(cgodb, execErr) ||
					tclR32OOracleRefusesSyncInTxn(cgodb, cgoStmt) ||
					tclR32OOracleRefusesTempStoreInTxn(cgodb, cgoStmt) ||
					tclAttachCannotPrepare(cgodb, stmt, cgoStmt) ||
					// ...and the three ATTACHes tclAttachCannotPrepare's own doc
					// comment names as mutual rejections it cannot reach,
					// because theirs happens at EXEC rather than at prepare.
					// See tclR35BAttachFailsIntrinsically.
					tclR35BAttachFailsIntrinsically(t, stmt, cgoStmt, cgoAttachDir) ||
					tclFts5ConfigCannotPrepare(cgodb, stmt, cgoStmt) ||
					tclVacuumCannotPrepare(cgodb, stmt, cgoStmt) ||
					tclSavepointNameMissing(&savepoints, stmt) ||
					// ...and the same free question for the one PRAGMA SETTER
					// whose refusal C SQLite decides from its file lock, and
					// which is the only kind in the excluded set that can be
					// asked by running it -- reversibly, and only in the state
					// that makes it so. See tclOracleRefusesLockProxySet.
					tclOracleRefusesLockProxySet(cgodb, &lockProxy, stmt, execErr) ||
					!tclOracleHasDatabase(cgodb, tclStatementDatabaseName(stmt))
			}
			if probeRejects {
				tally.mutualReject++
			} else if tclIsOutOfScope(stmt, execErr) {
				tally.outOfScope++
				tclNoteOOS(t, "goDeclinedExec", label, i, stmt)
			} else {
				tally.unsupported++
				histogram[tclExecUnsupportedBucket(stmt, execErr)]++
				tclDumpUnsupported(t, label, i, stmt, execErr)
			}
		case "constraint":
			_, cerr := cgodb.Exec(cgoStmt)
			xint.exec(i, stmt, cerr == nil)
			if cerr == nil {
				tally.wrong++
				tclLogWrong(t, "WRONG (Go rejected as a constraint violation, C SQLite ACCEPTED)\n  segment: %s\n  stmt #%d: %s\n  go error: %v", label, i, stmt, execErr)
				continue
			}
			tally.pass++
		}
	}
	xint.finish(godb, goPath, segDir)
	return tally
}

// tclWrongLogged counts WRONG detail lines emitted so far this process.
//
// A WRONG line dumps both engines' full result sets, which for a whole-corpus
// run (1168 files) once produced a 2 GB log -- and the RSS to buffer it --
// while the tallies that are the actual gate are a few KB. The first
// tclWrongLogBudget repros are printed verbatim (that is what a debugging
// session reads); past that the count alone carries. Set TCL_WRONG_LOG=<n> to
// raise the budget, or 0 for the old unbounded behavior when chasing a repro
// in a file that sorts late.
var tclWrongLogged int

const tclWrongLogBudget = 200

// tclMaxWrongLogBytes caps one repro's size: a single mismatching SELECT can
// return tens of thousands of rows, and the first few are enough to classify.
const tclMaxWrongLogBytes = 4 << 10

func tclLogWrong(t *testing.T, format string, args ...any) {
	budget := tclWrongLogBudget
	if s := os.Getenv("TCL_WRONG_LOG"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			budget = n
		}
	}
	tclWrongLogged++
	if budget > 0 && tclWrongLogged > budget {
		if tclWrongLogged == budget+1 {
			t.Logf("WRONG detail logging capped at %d lines (tallies are unaffected); set TCL_WRONG_LOG=0 for all", budget)
		}
		return
	}
	msg := fmt.Sprintf(format, args...)
	if len(msg) > tclMaxWrongLogBytes {
		msg = msg[:tclMaxWrongLogBytes] + fmt.Sprintf("... [%d bytes truncated]", len(msg)-tclMaxWrongLogBytes)
	}
	t.Log(msg)
}

// tclKnownWrong is the documented allowlist of currently-known WRONG results
// per mined file: the pure engine disagrees with C SQLite on exactly this
// many statements in that file, each root-caused below. The gate FAILS if any
// file exceeds its allowlisted count or if any file NOT listed here has a wrong
// (a regression), so the never-wrong invariant is still enforced against new
// divergences while these known gaps are tracked, not silently blessed.
//
// It is EMPTY: the whole mined corpus is wrong-free. Keep it that way -- an
// entry here is a debt, and the history below is what paying it down looked
// like.
//
// It briefly held one entry, and the round trip is the lesson.
// scripts/sweep's chunk list did not cover the corpus: 's[a-h] si so sp sqld
// s[r-z]' never matched sj/sk/sl/sm/sn or sqllo*, and no chunk matched a
// leading digit, so every skipscan*.test and snapshot*.test, sqllog.test and
// 8_3_names.test had been invisible to every sweep this gate ever ran -- 369
// passes and one wrong. **A green gate is only as good as its file list**;
// scripts/sweep now asserts coverage and fails if a corpus file matches no
// chunk.
//
// The wrong it exposed (snapshot2.test 4.7, "PRAGMA aux.journal_mode = delete")
// was then fixed rather than carried, and by an unrelated stream in the same
// round: runTCLSegment gave each engine its own MAIN file but ran both from one
// working directory, so a relative ATTACH named ONE SHARED FILE that both
// engines wrote. That same defect was independently masking two real engine
// bugs -- ROLLBACK unwriting an attachment's autocommit writes, and DETACH
// dropping them -- which scored "pass" only because the ORACLE had written the
// rows into the file this engine had just discarded. Isolating the file made
// all three visible at once.
//
// The previous 21-entry backlog (IN-subquery zero-row masking, IN-subquery
// affinity, an ORDER BY tie, an auto-column-name artifact, and an aggregate in
// a GROUP BY query's WHERE) has been driven to ZERO:
//
//   - IN(subquery) column-count / table-existence / compound-arm-count checks
//     now run at COMPILE time in the VDBE (subqueryResultColumns, engine's
//     subquery_validate.go), propagated as a hard errVDBESemantic rather than
//     deferred to a per-outer-row check at run time -- so an empty outer
//     table no longer masks them (in.test's in-12.*, func6.test). Matches C
//     SQLite, which validates the subquery at prepare time regardless of rows.
//   - "X IN (SELECT y ...)" now combines X's and y's affinity (via an affExpr
//     fed to comparisonAffinity/compareForOp), unlike the asymmetric IN-list
//     rule -- fixing subquery.test's TEXT-column-vs-INTEGER-subquery case.
//   - A GROUP BY query's WHERE column references are now resolved at plan time
//     (execSelectTree), so "WHERE <agg-alias>" errors regardless of row count
//     (tkt3508.test) -- C SQLite rejects it too (as "misuse of aggregate").
//   - The undefined-order ORDER BY tie (limit2.test) and the unaliased-expr
//     auto-column-NAME artifact (in.test's trailing "--" comment) were genuine
//     HARNESS comparator limits, fixed in the comparison here (tclOrderByTieOK,
//     tclStripColComments) -- the row VALUES always matched C SQLite.
//
// The last three entries are closed too:
//
//   - altertab3.test: a NON-UNIQUE expression/partial index used to be a
//     "decorative" no-op with no b-tree and no sqlite_schema row, so "SELECT
//     sql FROM sqlite_master" came back one row short. Such an index is now
//     MATERIALIZED for real -- the key evaluated per row, the partial WHERE
//     applied -- so it gets an ordinary schema row and an ordinary b-tree, one
//     C SQLite both integrity_checks and walks (see
//     vdbe_index_expr_materialize_test.go).
//   - tkt2817.test and view.test: the engine had ONE table namespace, so a
//     TEMP object and a main object of the same name collided ("CREATE TEMP
//     TABLE tbl" then "CREATE TABLE main.tbl" was "table tbl already exists").
//     It now holds two real catalogs -- main and temp -- with SQLite's own
//     resolution rules: unqualified names search temp first, "main."/"temp."
//     select one catalog, an index or trigger belongs to its table's catalog,
//     a main-schema view or trigger cannot reach a temp object, and a temp
//     object does not outlive the connection that made it. See
//     engine/temp_schema.go and vdbe_temp_schema_test.go.
var tclKnownWrong = map[string]int{}

// TestTCLCorpus is the mined-from-SQLite's-own-test-suite differential gate:
// see the package doc comment above for the full mining/segmenting/replay
// design. Like TestPureEngineSLTCoverage, PASS%/UNSUPPORTED% are reported
// metrics that are expected to grow as the engine gains features; PANIC==0 and
// "no wrong beyond tclKnownWrong" are the hard gates.
func TestTCLCorpus(t *testing.T) {
	files := tclAllFiles(t)
	if len(files) == 0 {
		t.Skip("no .test files found under " + tclCorpusDir)
	}
	if testing.Short() && len(files) > tclShortFileCount {
		files = files[:tclShortFileCount]
	}

	var total tclTally
	var totalMinedFiles, totalEmptyFiles, totalSegments int
	histogram := map[string]int{}

	for _, rel := range files {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			path := filepath.Join(tclCorpusDir, rel)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", rel, err)
			}
			if !harnessFTS5 && tclFTS5ModuleRegex.MatchString(string(src)) {
				// Neither side has fts5 in this build, so every statement here
				// would be declined in lockstep -- agreement that proves
				// nothing about fts5. Skip outright; -tags sqlite_fts5 is the
				// build that actually compares the two implementations.
				t.Skip("file needs fts5; rebuild with -tags sqlite_fts5 to replay it")
				return
			}
			segments := tclSegments(string(src))
			if len(segments) == 0 {
				totalEmptyFiles++
				t.Skip("no statically-complete SQL mined from this file")
				return
			}
			totalMinedFiles++

			var fileTally tclTally
			for si, stmts := range segments {
				label := fmt.Sprintf("%s#%d", rel, si)
				fileTally.add(runTCLSegment(t, label, stmts, histogram))
			}

			totalSegments += len(segments)
			total.add(fileTally)
			t.Logf("[%s] segments=%d statements=%d pass=%d mutualReject=%d propertyMatch=%d outOfScope=%d unsupported=%d wrong=%d panics=%d specified=%.1f%% pass/(pass+wrong)=%.1f%%",
				rel, len(segments), fileTally.statements, fileTally.pass, fileTally.mutualReject, fileTally.propertyMatch, fileTally.outOfScope, fileTally.unsupported, fileTally.wrong, fileTally.panics,
				pct(fileTally.pass+fileTally.mutualReject+fileTally.propertyMatch, fileTally.statements-fileTally.outOfScope),
				pct(fileTally.pass, fileTally.pass+fileTally.wrong))
			// Regression gate: a wrong is tolerated only up to this file's
			// documented tclKnownWrong count; anything above (or any wrong in an
			// unlisted file) fails. See the WRONG lines above (t.Logf) for repros.
			if allowed := tclKnownWrong[rel]; fileTally.wrong > allowed {
				t.Errorf("REGRESSION: %s has %d WRONG result(s), allowlist permits %d -- see the WRONG log lines above for repros; a new divergence from C SQLite must be fixed or cleanly rejected, never left wrong", rel, fileTally.wrong, allowed)
			}
		})
	}

	t.Logf("TOTAL: files=%d minedFiles=%d emptyFiles=%d segments=%d statements=%d pass=%d mutualReject=%d propertyMatch=%d outOfScope=%d unsupported=%d wrong=%d panics=%d specified=%.2f%% (agree=%.2f%% over all) pass/(pass+wrong)=%.2f%%",
		len(files), totalMinedFiles, totalEmptyFiles, totalSegments, total.statements, total.pass, total.mutualReject, total.propertyMatch, total.outOfScope, total.unsupported, total.wrong, total.panics,
		pct(total.pass+total.mutualReject+total.propertyMatch, total.statements-total.outOfScope),
		pct(total.pass+total.mutualReject+total.propertyMatch, total.statements),
		pct(total.pass, total.pass+total.wrong))
	tclXIntSummary(t)

	type kv struct {
		k string
		v int
	}
	ranked := make([]kv, 0, len(histogram))
	for k, v := range histogram {
		ranked = append(ranked, kv{k, v})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].v != ranked[j].v {
			return ranked[i].v > ranked[j].v
		}
		return ranked[i].k < ranked[j].k
	})
	t.Logf("UNSUPPORTED FEATURE HISTOGRAM (%d distinct buckets, top 60):", len(ranked))
	for i, e := range ranked {
		if i >= 60 {
			break
		}
		t.Logf("  %6d  %s", e.v, e.k)
	}

	// Per-file regression is enforced in each subtest above against
	// tclKnownWrong. Here we assert the total hasn't drifted past the sum of the
	// allowlist -- a belt-and-suspenders guard on the overall known-wrong budget.
	knownWrongTotal := 0
	for _, n := range tclKnownWrong {
		knownWrongTotal += n
	}
	if total.wrong > knownWrongTotal {
		t.Errorf("mined-TCL differential found %d WRONG result(s), exceeding the documented allowlist budget of %d -- a new divergence from C SQLite; fix it or reject the construct cleanly (never wrong)",
			total.wrong, knownWrongTotal)
	}
	if total.panics != 0 {
		t.Errorf("mined-TCL differential triggered %d PANIC(s) out of %d statements -- the engine must never panic",
			total.panics, total.statements)
	}
}

// tclDumpUnsupported logs every DECLINED statement, with its segment label and
// position, when TCL_DUMP_UNSUPPORTED is set in the environment. Off by
// default; it prints one tab-separated line per declined statement:
//
//	DECLINED<TAB>segment<TAB>index<TAB>statement<TAB>reason
//
// scripts/histogram buckets declines by ERROR MESSAGE, which answers "how
// many" and never "WHICH statement" -- and for a bucket keyed on a SYMPTOM
// rather than a cause that is not enough to decide anything with. The
// 148-statement "no such table: tN" bucket was exactly that, and the roadmap
// had written it off as an unattackable long tail. Grouping this dump BY
// SEGMENT -- a cascade's later statements name the symptom, while its root is
// an earlier line in the same segment -- split it into four unrelated causes,
// one of which was alter.test's own setup line failing from the first CREATE
// TEMP TABLE onwards.
//
// Collect it over the sweep's own chunk list, then take each segment's first
// declined statement:
//
//	TCL_DUMP_UNSUPPORTED=1 go test -run 'TestTCLCorpus$/^<chunk>' -v ./ \
//	  2>&1 | grep -a DECLINED >> declines.txt
//
//	grep -a DECLINED declines.txt | sed 's/^.*DECLINED\t//' \
//	  | awk -F'\t' '{if(!(c[$1]++)) print $1" | "$3" | "$4}'
func tclDumpUnsupported(t *testing.T, label string, i int, stmt string, err error) {
	if os.Getenv("TCL_DUMP_UNSUPPORTED") == "" {
		return
	}
	one := strings.Join(strings.Fields(stmt), " ")
	if len(one) > 300 {
		one = one[:300] + "..."
	}
	t.Logf("DECLINED\t%s\t%d\t%s\t%s", label, i, one, normalizeErrorMessage(err.Error()))
}

// tclNoteOOS dumps every outOfScope statement with the site that classified
// it, under TCL_OOS_DUMP=1. outOfScope is the one bucket that receives no
// differential judgement at all, so the only way to know what is in it -- and
// whether it is growing -- is to enumerate it; a census of DECLINE REASONS
// cannot, because it cannot weigh them by how often each actually occurs.
// Measured with it at 3fb74fb, over the whole corpus: 905 statements, 702 an
// exec the ENGINE declined (352 EXPLAIN, ~325 an INSERT of randomblob
// content), 147 a query the engine declined (146 of them sqlite_master's
// unreproducible rootpage), 30 a query calling a generator itself, 23 an
// oracle build without generate_series/rtree/fts5, 3 a tainted read.
func tclNoteOOS(t *testing.T, site, label string, idx int, stmt string) {
	if os.Getenv("TCL_OOS_DUMP") == "" {
		return
	}
	one := strings.Join(strings.Fields(stmt), " ")
	if len(one) > 300 {
		one = one[:300] + "..."
	}
	t.Logf("OOS\t%s\t%s#%d\t%s", site, label, idx, one)
}
