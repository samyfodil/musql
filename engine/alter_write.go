// ALTER TABLE: RENAME TO, RENAME [COLUMN], ADD [COLUMN] and DROP [COLUMN], with
// C SQLite's accept/reject boundaries and error messages.
//
// Like C, it never re-synthesizes a CREATE TABLE from scratch: it re-lexes the
// stored SQL and splices exact token spans (textEdit/applyEdits), leaving every
// other byte as written. Renaming b to c in "CREATE TABLE t(a INTEGER, b TEXT)"
// changes only b's token; "ADD COLUMN c text" appends the user's own spelling.
// Dependent triggers, views, indexes and foreign keys are rewritten the same
// way; see the cascade sections below.
package engine

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ---- shared text-splicing machinery ----

// textEdit is one substitution/insertion/deletion into an original SQL text:
// replace the byte range [start,end) with repl (repl == "" is a deletion;
// start == end is a pure insertion at that position, used by ADD COLUMN).
type textEdit struct {
	start, end int
	repl       string
}

// applyEdits returns s with every edit applied, left-to-right, sorted by
// position -- the single mechanism every ALTER TABLE form uses to mutate a
// tableMeta.sql/indexMeta.sql string while preserving every untouched byte
// exactly (see this file's package doc comment).
func applyEdits(s string, edits []textEdit) string {
	sorted := append([]textEdit(nil), edits...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1].start > sorted[j].start; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	var sb strings.Builder
	pos := 0
	for _, e := range sorted {
		sb.WriteString(s[pos:e.start])
		sb.WriteString(e.repl)
		pos = e.end
	}
	sb.WriteString(s[pos:])
	return sb.String()
}

// simpleIdentRe matches an identifier that never needs quoting; renderIdent
// leaves such a new column name bare, as C does. Reserved-keyword collisions are
// not detected (C would quote "select").
var simpleIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// renderIdent renders name as it should appear freshly written into SQL
// text: bare if it's already a simple identifier, double-quoted otherwise.
func renderIdent(name string) string {
	if simpleIdentRe.MatchString(name) {
		return name
	}
	return quoteIdent(name)
}

// identRepl is the replacement text for one renamed-identifier occurrence, from
// renameEditSql (alter.c:1272):
//
//	if( bQuote==0 && sqlite3IsIdChar(*(u8*)pBest->t.z) ){
//	  nReplace = nNew;  zReplace = zNew;      /* bare */
//	}else{
//	  nReplace = nQuot; zReplace = zQuot;     /* "new" */
//	}
//
// The quoted form is used when force (C's bQuote: the ALTER's new name was
// written quoted, alter.c:656; always for a table rename, alter.c:1770) or when
// the occurrence itself starts with a quote. Otherwise renderIdent's bare form.
type identRepl struct {
	name  string // the new name, already dequoted
	force bool   // renameEditSql's bQuote
	// isTable says the thing being renamed is a TABLE, which changes which
	// "ident (" positions can be rewritten -- see isCallNameToken.
	isTable bool
}

// at renders the replacement for occurrence t in sqlText. C builds the quoted
// form with a trailing space (zQuot = `"%w" `) and drops it unless the next byte
// is '"', since two adjacent quoted tokens would re-lex as one with an escaped
// quote (altercol.test 23.0: `t1('a'"b",c)` becomes `t1("x" "b",c)`).
func (r identRepl) at(sqlText string, t token) string {
	if r.force || t.quoted || t.kind == tkString {
		out := quoteIdent(r.name)
		if t.End < len(sqlText) && sqlText[t.End] == '"' {
			out += " "
		}
		return out
	}
	return renderIdent(r.name)
}

// renameColumnRepl is RENAME COLUMN's identRepl: bQuote comes from how the
// statement spelled the new name (parseAlterTargetIdent's quoted).
func renameColumnRepl(newName string, newQuoted bool) identRepl {
	return identRepl{name: newName, force: newQuoted}
}

// containsFold reports whether ss contains name, case-insensitively.
func containsFold(ss []string, name string) bool {
	for _, s := range ss {
		if equalFoldName(s, name) {
			return true
		}
	}
	return false
}

// skipParenGroup consumes a balanced "( ... )" group starting at p's current
// position (which must be "("), used by parseAddColumnDef to skip over a
// CHECK(...)/REFERENCES tbl(...) clause's parenthesized body without
// interpreting it.
func skipParenGroup(p *parser) error {
	if !p.peekIsPunct("(") {
		return fmt.Errorf("engine: expected '(', got %q", p.tokenDesc(p.peek()))
	}
	p.next()
	depth := 1
	for depth > 0 {
		t := p.peek()
		if t.kind == tkEOF {
			return fmt.Errorf("engine: unterminated parenthesized group")
		}
		if t.kind == tkPunct && t.text == "(" {
			depth++
		} else if t.kind == tkPunct && t.text == ")" {
			depth--
		}
		p.next()
	}
	return nil
}

// ---- CREATE TABLE / CREATE INDEX token-span helpers ----

// isNameToken reports whether t can spell an object NAME in stored schema
// text: a (possibly quoted) identifier, or a single-quoted STRING literal --
// SQLite's grammar accepts a string wherever a name is expected, and stores
// it verbatim, so "CREATE TABLE 'p 1 \"parent one\"'(...)" (e_fkey.test's own
// shape) carries its table name as a tkString these splice locators must
// recognize like any other name token.
func isNameToken(t token) bool {
	return t.kind == tkIdent || t.kind == tkString
}

// columnDefSegNameMatches reports whether a column-definition segment declares
// name. Its first token may be a single-quoted string (an "nm"), so compare the
// decoded value: "CREATE TABLE t1('a'\"b\",c)" declares a column named a with type
// "b" (altercol.test 23.0).
func columnDefSegNameMatches(seg []token, name string) bool {
	if len(seg) == 0 {
		return false
	}
	got, ok := objectNameToken(seg[0])
	return ok && equalFoldName(got, name)
}

// locateCreateTableNameToken returns the token that spells the table's own
// name in its stored "CREATE [TEMP|TEMPORARY] TABLE [IF NOT EXISTS]
// [schema.]name(...)" text -- exactly the token parseCreateTableNamePrefix
// (schema_write.go) consumes as its name, re-exposed here (rather than
// changing that function's signature) so renameTableTo can splice it in
// place.
func locateCreateTableNameToken(createSQL string) (token, error) {
	toks, err := lex(createSQL)
	if err != nil {
		return token{}, err
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: not a CREATE TABLE statement")
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("TABLE") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: not a CREATE TABLE statement")
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected table name")
	}
	nameTok := toks[i]
	i++
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		if i >= len(toks) || !isNameToken(toks[i]) {
			return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected name after schema qualifier")
		}
		nameTok = toks[i]
	}
	return nameTok, nil
}

// locateIndexOnTableToken returns the table-name token in an explicit
// index's own stored "CREATE [UNIQUE] INDEX [IF NOT EXISTS] [schema.]name ON
// table(...)" text, mirroring locateCreateTableNameToken -- for
// renameTableTo's cascading rewrite of every index registered against the
// renamed table.
func locateIndexOnTableToken(createIndexSQL string) (token, error) {
	toks, err := lex(createIndexSQL)
	if err != nil {
		return token{}, err
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: not a CREATE INDEX statement")
	}
	kw("UNIQUE")
	if !kw("INDEX") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: not a CREATE INDEX statement")
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected index name")
	}
	i++
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		if i >= len(toks) || !isNameToken(toks[i]) {
			return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected name after schema qualifier")
		}
		i++
	}
	if !kw("ON") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected ON")
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected table name")
	}
	return toks[i], nil
}

// triggerOwnNameSpan returns the byte span of a stored trigger's OWN
// "[schema.]name", or false when the text does not start the way a CREATE
// TRIGGER does. A rename never rewrites it: renameTableFunc maps only the ON
// table, the step targets and what the walker resolves (alter.c:1852-1872),
// and a trigger's name is none of those.
func triggerOwnNameSpan(createTriggerSQL string) (byteSpan, bool) {
	toks, err := lex(createTriggerSQL)
	if err != nil {
		return byteSpan{}, false
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return byteSpan{}, false
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("TRIGGER") {
		return byteSpan{}, false
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return byteSpan{}, false
	}
	span := byteSpan{toks[i].Start, toks[i].End}
	if i+2 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." && isNameToken(toks[i+2]) {
		span.end = toks[i+2].End
	}
	return span, true
}

// locateTriggerOnTableToken returns the token naming the table in a stored
// trigger's "ON [schema.]table", walking parseCreateTriggerStmt's prefix ("ON"
// cannot be found by scanning: an "UPDATE OF a, b ON t" list precedes it and the
// trigger may be named "on"). renameTriggerReferences needs the token because C
// rewrites that position even when it is a string (alter.c:1852).
func locateTriggerOnTableToken(createTriggerSQL string) (token, error) {
	toks, err := lex(createTriggerSQL)
	if err != nil {
		return token{}, err
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	name := func() bool {
		if i >= len(toks) || !isNameToken(toks[i]) {
			return false
		}
		i++
		if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
			i++
			if i >= len(toks) || !isNameToken(toks[i]) {
				return false
			}
			i++
		}
		return true
	}
	bad := func() (token, error) {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: not a CREATE TRIGGER statement")
	}
	if !kw("CREATE") {
		return bad()
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("TRIGGER") {
		return bad()
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if !name() { // the trigger's own [schema.]name
		return bad()
	}
	switch {
	case kw("BEFORE"), kw("AFTER"):
	case kw("INSTEAD"):
		kw("OF")
	}
	switch {
	case kw("DELETE"), kw("INSERT"):
	case kw("UPDATE"):
		if kw("OF") {
			for {
				if i >= len(toks) || !isNameToken(toks[i]) {
					return bad()
				}
				i++
				if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "," {
					i++
					continue
				}
				break
			}
		}
	default:
		return bad()
	}
	if !kw("ON") {
		return bad()
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return bad()
	}
	// A schema qualifier ("ON main.'t8'"): the TABLE is the token after the dot.
	if i+2 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." && isNameToken(toks[i+2]) {
		return toks[i+2], nil
	}
	return toks[i], nil
}

// nameStringTokens returns the string tokens of sqlText that spell name and sit
// where the grammar could reduce an "nm" (parse.y:340) rather than a value,
// judged by the preceding token. C rewrites a string in a name position like an
// identifier; callers rewrite the one position they can locate exactly (the ON
// clause) and decline on any other. A comma counts as a name predecessor (FROM
// lists), which also catches some VALUES entries: an extra decline, never a wrong
// answer.
func nameStringTokens(sqlText, name string) []token {
	toks, err := lex(sqlText)
	if err != nil {
		return nil
	}
	var out []token
	for i, t := range toks {
		if t.kind != tkString || !equalFoldName(t.str, name) || i == 0 {
			continue
		}
		prev := toks[i-1]
		if prev.kind == tkPunct && (prev.text == "." || prev.text == ",") {
			out = append(out, t)
			continue
		}
		if prev.kind == tkIdent && !prev.quoted {
			switch prev.upper() {
			case "ON", "INTO", "FROM", "UPDATE", "JOIN", "TABLE", "REFERENCES":
				out = append(out, t)
			}
		}
	}
	return out
}

// indexWhereQualifierEdits returns edits renaming every table qualifier in a
// partial index's WHERE from oldName to newName (the "t3" of "WHERE t3.b>1" and
// the middle of "main.t3.c"). A qualifier is an identifier followed by "."; the
// key list cannot contain one, so only the WHERE region after it is scanned:
//
//	WHERE T3.B>1                 -> WHERE "t9".B>1   (the COLUMN keeps its case)
//	WHERE "t3".b>1 AND main.t3.c<9 -> WHERE "t9".b>1 AND main."t9".c<9
//	WHERE t3.rowid>1             -> WHERE "t9".rowid>1
func indexWhereQualifierEdits(createIndexSQL, oldName, newName string) []textEdit {
	toks, err := lex(createIndexSQL)
	if err != nil {
		return nil
	}
	// Find the key list's opening paren -- the first "(" of the statement,
	// since nothing before it can be parenthesized ("CREATE [UNIQUE] INDEX
	// [IF NOT EXISTS] [schema.]name ON table").
	open := -1
	for i := range toks {
		if toks[i].kind == tkPunct && toks[i].text == "(" {
			open = i
			break
		}
	}
	if open < 0 {
		return nil
	}
	closeIdx, ferr := findMatchingParen(toks, open)
	if ferr != nil {
		return nil
	}
	var out []textEdit
	for k := closeIdx + 1; k < len(toks); k++ {
		if toks[k].kind != tkIdent || !equalFoldName(toks[k].text, oldName) {
			continue
		}
		if k+1 >= len(toks) || !(toks[k+1].kind == tkPunct && toks[k+1].text == ".") {
			continue // a column reference, not a qualifier
		}
		out = append(out, textEdit{toks[k].Start, toks[k].End, quoteIdent(newName)})
	}
	return out
}

// indexColumnRefTokens returns every identifier token of an index's stored SQL
// from the key list's "(" on that is a column-name candidate: not a function name
// (followed by "(") or a qualifier (followed by "."). Starting at the paren keeps
// the ON-clause table name out. A bare keyword like the AND in BETWEEN is also an
// identifier token, which only matters for a quoted column named "and".
//
// renameColumn needs it because an expression/partial index's cols are the
// referenced columns in table order, not the key list, so splicing by key
// position rewrote the wrong token ("CREATE INDEX ip ON t(b) WHERE a>1").
func indexColumnRefTokens(createIndexSQL string) []token {
	toks, err := lex(createIndexSQL)
	if err != nil {
		return nil
	}
	open := -1
	for i := range toks {
		if toks[i].kind == tkPunct && toks[i].text == "(" {
			open = i
			break
		}
	}
	if open < 0 {
		return nil
	}
	var out []token
	for k := open + 1; k < len(toks); k++ {
		if toks[k].kind != tkIdent {
			continue
		}
		if k+1 < len(toks) && toks[k+1].kind == tkPunct && (toks[k+1].text == "(" || toks[k+1].text == ".") {
			continue
		}
		out = append(out, toks[k])
	}
	return out
}

// locateIndexColumnToken returns the token naming the position-th column of a
// plain index's "ON table(col1, ...)" list: the first token of that entry. Used by
// renameColumn to splice the index text in step with cols[].
func locateIndexColumnToken(createIndexSQL string, position int) (token, error) {
	toks, err := lex(createIndexSQL)
	if err != nil {
		return token{}, err
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: not a CREATE INDEX statement")
	}
	kw("UNIQUE")
	if !kw("INDEX") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: not a CREATE INDEX statement")
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if i >= len(toks) {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected index name")
	}
	if _, ok := objectNameToken(toks[i]); !ok {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected index name")
	}
	i++
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		if i >= len(toks) {
			return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected name after schema qualifier")
		}
		if _, ok := objectNameToken(toks[i]); !ok {
			return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected name after schema qualifier")
		}
		i++
	}
	if !kw("ON") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected ON")
	}
	if i >= len(toks) {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected table name")
	}
	if _, ok := objectNameToken(toks[i]); !ok {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected table name")
	}
	i++
	if !(i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "(") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected '('")
	}
	i++
	col := 0
	for i < len(toks) {
		// objectNameToken, not just tkIdent: an index column may be written as a string
		// ("CREATE INDEX i1 ON t1('a')" indexes column a), and C rewrites it on rename
		// (to t1("x")). Not applied in indexColumnRefTokens, where a string in an
		// expression or WHERE is a literal.
		if _, ok := objectNameToken(toks[i]); !ok {
			return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: expected column name")
		}
		colTok := toks[i]
		i++
		for i < len(toks) && !(toks[i].kind == tkPunct && (toks[i].text == "," || toks[i].text == ")")) {
			i++
		}
		if col == position {
			return colTok, nil
		}
		col++
		if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "," {
			i++
			continue
		}
		break
	}
	return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: column position %d not found", position)
}

// tblColumnList is the parsed shape of a CREATE TABLE statement's own
// column/constraint list, as token spans into the original SQL text --
// everything RENAME COLUMN/DROP COLUMN/ADD COLUMN need to splice tbl.sql in
// place. segments holds each top-level comma-separated entry's own tokens
// (a plain column definition, or a table-level CONSTRAINT/PRIMARY
// KEY/UNIQUE/CHECK/FOREIGN KEY clause), in original declaration order.
type tblColumnList struct {
	openParen  token
	closeParen token
	segments   [][]token
}

// firstTableConstraintSegment returns the index of the first segment that is
// a TABLE-LEVEL constraint ("[CONSTRAINT name] PRIMARY KEY(...)/UNIQUE(...)/
// CHECK(...)/FOREIGN KEY(...)") rather than a column definition, or -1 if
// there is none. A quoted identifier is never a keyword here, so a column
// legitimately named "check" is not mistaken for one.
func (l *tblColumnList) firstTableConstraintSegment() int {
	for i, seg := range l.segments {
		j := 0
		if len(seg) > 1 && seg[0].kind == tkIdent && !seg[0].quoted && seg[0].upper() == "CONSTRAINT" {
			j = 2 // skip "CONSTRAINT <name>"
		}
		if j >= len(seg) || seg[j].kind != tkIdent || seg[j].quoted {
			continue
		}
		switch seg[j].upper() {
		case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN":
			return i
		}
	}
	return -1
}

// parseTblColumnList splits createSQL's column list into segments, mirroring
// sql_parser.go's parseCreateTableColumnsAndAutoIndexes' identical top-level
// comma-split -- but returning raw TOKENS (with byte offsets), not resolved
// columnInfo/autoIndexSpec values, since this file's callers need to splice
// the ORIGINAL text, not re-render it from scratch (see package doc
// comment). Kept independent from that function (rather than refactoring it
// to share this walk) so a future change to either one can't silently affect
// the other.
func parseTblColumnList(createSQL string) (*tblColumnList, error) {
	toks, err := lex(createSQL)
	if err != nil {
		return nil, err
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return nil, fmt.Errorf("engine: internal error: not a CREATE TABLE statement")
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("TABLE") {
		return nil, fmt.Errorf("engine: internal error: not a CREATE TABLE statement")
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return nil, fmt.Errorf("engine: internal error: expected table name")
	}
	i++
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		if i >= len(toks) || !isNameToken(toks[i]) {
			return nil, fmt.Errorf("engine: internal error: expected name after schema qualifier")
		}
		i++
	}
	if !(i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "(") {
		return nil, fmt.Errorf("engine: internal error: expected '('")
	}
	openParen := toks[i]
	i++

	var segments [][]token
	var cur []token
	depth := 0
	for i < len(toks) {
		t := toks[i]
		if t.kind == tkEOF {
			break
		}
		switch {
		case t.kind == tkPunct && t.text == "(":
			depth++
			cur = append(cur, t)
			i++
		case t.kind == tkPunct && t.text == ")":
			if depth == 0 {
				if len(cur) > 0 {
					segments = append(segments, cur)
				}
				return &tblColumnList{openParen: openParen, closeParen: t, segments: segments}, nil
			}
			depth--
			cur = append(cur, t)
			i++
		case t.kind == tkPunct && t.text == "," && depth == 0:
			segments = append(segments, cur)
			cur = nil
			i++
		default:
			cur = append(cur, t)
			i++
		}
	}
	return nil, fmt.Errorf("engine: internal error: unterminated column list")
}

// columnListGapComma returns the byte offset of the top-level comma that
// separates two adjacent segments of a column list, searching only the gap
// between them (which holds nothing but that comma, whitespace and comments),
// or -1 if there is none. DROP COLUMN needs it because the segment splitter
// throws the commas away and C's last-column span is measured FROM one.
func columnListGapComma(sql string, from, to int) int {
	if from < 0 || to > len(sql) || from >= to {
		return -1
	}
	for i := from; i < to; i++ {
		switch {
		case sql[i] == ',':
			return i
		case sql[i] == '-' && i+1 < to && sql[i+1] == '-':
			for i < to && sql[i] != '\n' {
				i++
			}
		case sql[i] == '/' && i+1 < to && sql[i+1] == '*':
			i += 2
			for i+1 < to && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			i++
		}
	}
	return -1
}

// tableConstraintLeadKeywords mirrors sql_parser.go's identical (but
// function-local, hence not reusable from here) tableConstraintKeywords set:
// the leading keyword of a table-level constraint segment, as opposed to an
// ordinary column definition.
var tableConstraintLeadKeywords = map[string]bool{
	"PRIMARY": true, "UNIQUE": true, "CHECK": true, "FOREIGN": true, "CONSTRAINT": true,
}

// isTableConstraintSegment reports whether seg (one top-level entry of a
// tblColumnList) is a table-level constraint clause rather than an ordinary
// column definition.
func isTableConstraintSegment(seg []token) bool {
	return len(seg) > 0 && seg[0].kind == tkIdent && tableConstraintLeadKeywords[seg[0].upper()]
}

// checkQualifierEdits returns edits renaming every table qualifier inside a
// CHECK(...) body from oldName to newName ("CHECK( t1.a != t1.b )"). A qualifier
// is an identifier followed by "."; in "main.t1.a" only the middle token names
// the table.
func checkQualifierEdits(sqlText, oldName, newName string) []textEdit {
	toks, err := lex(sqlText)
	if err != nil {
		return nil
	}
	var out []textEdit
	for i := 0; i < len(toks); i++ {
		if !(toks[i].kind == tkIdent && toks[i].upper() == "CHECK") {
			continue
		}
		if i+1 >= len(toks) || !(toks[i+1].kind == tkPunct && toks[i+1].text == "(") {
			continue
		}
		closeIdx, ferr := findMatchingParen(toks, i+1)
		if ferr != nil {
			continue
		}
		for k := i + 2; k < closeIdx; k++ {
			if toks[k].kind != tkIdent || !equalFoldName(toks[k].text, oldName) {
				continue
			}
			if k+1 >= len(toks) || !(toks[k+1].kind == tkPunct && toks[k+1].text == ".") {
				continue // a column reference, not a qualifier
			}
			out = append(out, textEdit{toks[k].Start, toks[k].End, quoteIdent(newName)})
		}
		i = closeIdx
	}
	return out
}

// checkClauseIdentTokens returns every column-name candidate (not followed by
// "(") inside every CHECK(...) body in seg; a column may carry several. C's
// RENAME COLUMN rewrites CHECK references, the column's own or another's.
func checkClauseIdentTokens(seg []token) []token {
	var out []token
	for i := 0; i < len(seg); i++ {
		if !(seg[i].kind == tkIdent && seg[i].upper() == "CHECK") {
			continue
		}
		if i+1 >= len(seg) || !(seg[i+1].kind == tkPunct && seg[i+1].text == "(") {
			continue
		}
		closeIdx, err := findMatchingParen(seg, i+1)
		if err != nil {
			continue
		}
		for k := i + 2; k < closeIdx; k++ {
			if seg[k].kind != tkIdent {
				continue
			}
			if k+1 < len(seg) && seg[k+1].kind == tkPunct && seg[k+1].text == "(" {
				continue // a function call, e.g. length(a) -- "length" is not a column reference
			}
			out = append(out, seg[k])
		}
		i = closeIdx
	}
	return out
}

// generatedClauseIdentTokens is checkClauseIdentTokens for a "[GENERATED
// ALWAYS] AS ( <expr> )" body. C rewrites every column's generated expression on
// RENAME COLUMN (alter.c:1616-1622); without it "c AS (a+1)" kept naming a
// renamed column. Other parenthesized groups (CHECK, DEFAULT) are skipped.
func generatedClauseIdentTokens(seg []token) []token {
	var out []token
	for i := 0; i < len(seg); i++ {
		if !(seg[i].kind == tkPunct && seg[i].text == "(") {
			continue
		}
		closeIdx, err := findMatchingParen(seg, i)
		if err != nil {
			return out
		}
		if i > 0 && seg[i-1].kind == tkIdent && seg[i-1].upper() == "AS" {
			for k := i + 1; k < closeIdx; k++ {
				if seg[k].kind != tkIdent {
					continue
				}
				if k+1 < len(seg) && seg[k+1].kind == tkPunct && seg[k+1].text == "(" {
					continue // a function call, not a column reference
				}
				out = append(out, seg[k])
			}
		}
		i = closeIdx
	}
	return out
}

// checkExprReferencesColumn reports whether e (a checkConstraint.expr,
// already validated by finalizeCheckConstraints to contain nothing but
// unqualified references to the SAME table's own columns -- see that
// function's doc comment) contains a ColumnExpr naming colName
// (case-insensitively) anywhere -- used by dropColumn to tell whether a
// CHECK constraint declared elsewhere in the table still needs the column
// about to be dropped. Mirrors checkExprSupported's traversal shape.
func checkExprReferencesColumn(e Expr, colName string) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return false
	case ColumnExpr:
		return equalFoldName(x.Name, colName)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if checkExprReferencesColumn(a, colName) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return checkExprReferencesColumn(x.X, colName)
	case BinaryExpr:
		return checkExprReferencesColumn(x.L, colName) || checkExprReferencesColumn(x.R, colName)
	case IsNullExpr:
		return checkExprReferencesColumn(x.X, colName)
	case InExpr:
		if checkExprReferencesColumn(x.X, colName) {
			return true
		}
		for _, a := range x.List {
			if checkExprReferencesColumn(a, colName) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return checkExprReferencesColumn(x.X, colName) || checkExprReferencesColumn(x.Lo, colName) || checkExprReferencesColumn(x.Hi, colName)
	case LikeExpr:
		return checkExprReferencesColumn(x.X, colName) || checkExprReferencesColumn(x.Pattern, colName)
	case GlobExpr:
		return checkExprReferencesColumn(x.X, colName) || checkExprReferencesColumn(x.Pattern, colName)
	case CollateExpr:
		return checkExprReferencesColumn(x.X, colName)
	case CastExpr:
		return checkExprReferencesColumn(x.X, colName)
	case CaseExpr:
		if x.Base != nil && checkExprReferencesColumn(x.Base, colName) {
			return true
		}
		for _, w := range x.Whens {
			if checkExprReferencesColumn(w.When, colName) || checkExprReferencesColumn(w.Then, colName) {
				return true
			}
		}
		return x.Else != nil && checkExprReferencesColumn(x.Else, colName)
	default:
		return false
	}
}

// identTokensInParens returns every identifier token inside the first top-level
// "(...)" of seg: the column list of a table-level PRIMARY KEY/UNIQUE, or a
// table-level CHECK's body. renameColumn uses the tokens' offsets to splice.
func identTokensInParens(seg []token) []token {
	i := 0
	for i < len(seg) && !(seg[i].kind == tkPunct && seg[i].text == "(") {
		i++
	}
	if i >= len(seg) {
		return nil
	}
	i++
	var out []token
	depth := 0
	for i < len(seg) {
		t := seg[i]
		if t.kind == tkPunct && t.text == "(" {
			depth++
		} else if t.kind == tkPunct && t.text == ")" {
			if depth == 0 {
				break
			}
			depth--
		} else if t.kind == tkIdent {
			out = append(out, t)
		}
		i++
	}
	return out
}

// ---- view-safety guard (see package doc comment) ----

// viewReferencesTable reports whether any view's stored SQL contains tableName
// as an identifier token, a deliberately over-inclusive test; a view whose SQL
// fails to lex counts as referencing it.
func viewReferencesTable(db *DB, tableName string) bool {
	for _, v := range db.views {
		toks, err := lex(v.sql)
		if err != nil {
			return true
		}
		for _, t := range toks {
			if t.kind == tkIdent && equalFoldName(t.text, tableName) {
				return true
			}
		}
	}
	return false
}

// ---- ALTER TABLE: top-level parse + dispatch ----

// alterVerb identifies which ALTER TABLE form resolveAlterTarget is
// resolving the target table/view for, since C SQLite gives a different
// exact message per verb when the name resolves to a VIEW instead of a
// TABLE (verified directly against each).
type alterVerb int

const (
	alterVerbRenameTo alterVerb = iota
	alterVerbRenameColumn
	alterVerbAddColumn
	alterVerbDropColumn
	alterVerbDropConstraint
)

// resolveAlterTarget looks bare (already schema-qualifier-resolved by
// dropQualifiedName) up as a table; if it's not one, gives the exact
// per-verb error C SQLite gives for a VIEW of that name, or the
// (verb-independent) "no such table: %s" otherwise -- verified directly:
// every one of RENAME TO/RENAME COLUMN/ADD COLUMN/DROP COLUMN against a
// plain nonexistent name (no view either) gives the identical "no such
// table: X" text, but each gives its OWN distinct wording for an existing
// VIEW of that name.
func (db *DB) resolveAlterTarget(bare, display string, scope schemaScope, unknownSchema bool, verb alterVerb) (*tableMeta, error) {
	if !unknownSchema {
		if tbl := db.findTableMetaIn(scope, bare); tbl != nil {
			// See table_load.go's package doc comment: the single choke point
			// every ALTER TABLE verb funnels through -- RENAME COLUMN/ADD
			// COLUMN/DROP COLUMN all read or rewrite tbl's existing rows.
			if err := db.ensureTableLoaded(tbl); err != nil {
				return nil, err
			}
			return tbl, nil
		}
		if db.findViewMetaIn(scope, bare) != nil {
			switch verb {
			case alterVerbRenameTo:
				return nil, fmt.Errorf("engine: view %s may not be altered", bare)
			case alterVerbRenameColumn:
				return nil, fmt.Errorf("engine: cannot rename columns of view %q", bare)
			case alterVerbAddColumn:
				return nil, fmt.Errorf("engine: Cannot add a column to a view")
			case alterVerbDropColumn:
				return nil, fmt.Errorf("engine: cannot drop column from view %q", bare)
			case alterVerbDropConstraint:
				return nil, fmt.Errorf("engine: cannot edit constraints of view %q", bare)
			}
		}
		// A VIRTUAL table's own four answers. RENAME TO never reaches here --
		// AlterTable dispatches it to renameVtabTo (alter_vtab_write.go)
		// before resolving an ordinary target -- and the other three are
		// refusals C SQLite words individually, each verified against
		// 3.53.3 over "CREATE VIRTUAL TABLE ft USING fts4(a,b)".
		if db.findVtabMetaIn(scope, bare) != nil {
			switch verb {
			case alterVerbRenameColumn:
				return nil, fmt.Errorf("engine: cannot rename columns of virtual table %q", bare)
			case alterVerbAddColumn:
				return nil, fmt.Errorf("engine: virtual tables may not be altered")
			case alterVerbDropColumn:
				return nil, fmt.Errorf("engine: cannot drop column from virtual table %q", bare)
			case alterVerbDropConstraint:
				return nil, fmt.Errorf("engine: virtual tables may not be altered")
			}
		}
	}
	return nil, fmt.Errorf("engine: no such table: %s", display)
}

// parseAlterTargetIdent parses one "nm" (parse.y:339):
//
//	nm(A) ::= idj(A).
//	nm(A) ::= STRING(A).
//
// so a single-quoted string is a valid name for RENAME TO, RENAME COLUMN, DROP
// COLUMN and DROP CONSTRAINT (alterqf.test's "RENAME two TO 'four'"). quoted
// reports any quoting, which C reads as bQuote (alter.c:656) to double-quote the
// new column name everywhere it is substituted; a table rename always quotes. A
// schema qualifier is accepted and discarded.
func parseAlterTargetIdent(p *parser) (name string, quoted bool, err error) {
	t := p.peek()
	name, ok := objectNameToken(t)
	if !ok {
		return "", false, fmt.Errorf("engine: ALTER TABLE: expected identifier, got %q", p.tokenDesc(t))
	}
	p.next()
	quoted = t.kind == tkString || t.quoted
	if p.peekIsPunct(".") {
		p.next()
		t2 := p.peek()
		n2, ok2 := objectNameToken(t2)
		if !ok2 {
			return "", false, fmt.Errorf("engine: ALTER TABLE: expected identifier after schema qualifier")
		}
		p.next()
		name = n2
		quoted = t2.kind == tkString || t2.quoted
	}
	return name, quoted, nil
}

// AlterTable parses and executes a single "ALTER TABLE [schema.]name ..."
// statement: RENAME TO newname, RENAME [COLUMN] old TO new, ADD [COLUMN]
// coldef, or DROP [COLUMN] name. See this file's package doc comment for
// exactly what each form supports and declines.
func (db *DB) AlterTable(sqlText string) error {
	err := db.alterTable(sqlText)
	if err == nil {
		// Every ALTER TABLE ends by reloading the whole schema
		// (renameReloadSchema, alter.c), and sqlite3InitOne loads the
		// statistics again with it -- into objects built afresh.
		db.loadAnalysisFresh()
	}
	return err
}

// AlterAddColumnDef returns the column definition an "ALTER TABLE ... ADD
// [COLUMN] <def>" statement adds, verbatim -- the text alterTable splices into
// the table's CREATE statement, parsed the same way -- and false for any other
// statement. A replicator re-issues it against the table's current name.
func AlterAddColumnDef(sqlText string) (string, bool) {
	trimmed := strings.TrimSpace(sqlText)
	toks, err := lex(trimmed)
	if err != nil {
		return "", false
	}
	p := newParser(trimmed, toks)
	const errPrefix = "engine: ALTER TABLE"
	if !p.consumeKeyword("ALTER") || !p.consumeKeyword("TABLE") {
		return "", false
	}
	if _, _, _, _, err := dropQualifiedName(p, errPrefix); err != nil || !p.consumeKeyword("ADD") {
		return "", false
	}
	if first := p.peek(); first.kind == tkIdent && !first.quoted && (first.upper() == "CONSTRAINT" || first.upper() == "CHECK") {
		return "", false
	}
	p.consumeKeyword("COLUMN")
	def, derr := parseAddColumnDef(p)
	if derr != nil || p.pos <= 0 || p.pos > len(toks) {
		return "", false
	}
	text := trimmed[def.rawStart:toks[p.pos-1].End]
	if expectDropTrailer(p, errPrefix) != nil {
		return "", false
	}
	return text, true
}

// AlterRenameColumnQuoted reports whether an "ALTER TABLE ... RENAME [COLUMN]
// a TO b" statement quoted its new name b, which decides whether C writes it
// quoted into the schema (alter.c:656, bQuote = sqlite3Isquote(pNew->z[0])).
// ok is false for any other statement.
func AlterRenameColumnQuoted(sqlText string) (quoted, ok bool) {
	trimmed := strings.TrimSpace(sqlText)
	toks, err := lex(trimmed)
	if err != nil {
		return false, false
	}
	p := newParser(trimmed, toks)
	const errPrefix = "engine: ALTER TABLE"
	if !p.consumeKeyword("ALTER") || !p.consumeKeyword("TABLE") {
		return false, false
	}
	if _, _, _, _, err := dropQualifiedName(p, errPrefix); err != nil || !p.consumeKeyword("RENAME") || p.consumeKeyword("TO") {
		return false, false
	}
	p.consumeKeyword("COLUMN")
	if _, _, err := parseAlterTargetIdent(p); err != nil || !p.consumeKeyword("TO") {
		return false, false
	}
	_, quoted, err = parseAlterTargetIdent(p)
	if err != nil || expectDropTrailer(p, errPrefix) != nil {
		return false, false
	}
	return quoted, true
}

func (db *DB) alterTable(sqlText string) error {
	trimmed := strings.TrimSpace(sqlText)
	toks, err := lex(trimmed)
	if err != nil {
		return err
	}
	p := newParser(trimmed, toks)
	const errPrefix = "engine: ALTER TABLE"
	if !p.consumeKeyword("ALTER") {
		return fmt.Errorf("%s: expected ALTER, got %q", errPrefix, p.tokenDesc(p.peek()))
	}
	if !p.consumeKeyword("TABLE") {
		return fmt.Errorf("%s: expected TABLE, got %q", errPrefix, p.tokenDesc(p.peek()))
	}
	bare, display, scope, unknownSchema, err := dropQualifiedName(p, errPrefix)
	if err != nil {
		return err
	}
	if canonical, reserved := canonicalReservedSchemaTable(bare); reserved {
		return fmt.Errorf("engine: table %s may not be altered", canonical)
	}

	switch {
	case p.consumeKeyword("RENAME"):
		if p.consumeKeyword("TO") {
			newName, _, nerr := parseAlterTargetIdent(p)
			if nerr != nil {
				return nerr
			}
			if terr := expectDropTrailer(p, errPrefix); terr != nil {
				return terr
			}
			// A VIRTUAL table is renamed by its own path, which also moves the
			// module's shadow tables (alter_vtab_write.go). Checked before the
			// ordinary target resolution, which knows nothing about vtabs and
			// would answer "no such table".
			if !unknownSchema {
				if vm := db.findVtabMetaIn(scope, bare); vm != nil {
					return db.renameVtabTo(vm, newName)
				}
			}
			tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbRenameTo)
			if verr != nil {
				return verr
			}
			return db.renameTableTo(tbl, newName)
		}
		p.consumeKeyword("COLUMN")
		oldName, _, oerr := parseAlterTargetIdent(p)
		if oerr != nil {
			return oerr
		}
		if !p.consumeKeyword("TO") {
			return fmt.Errorf("%s RENAME COLUMN: expected TO, got %q", errPrefix, p.tokenDesc(p.peek()))
		}
		newName, newQuoted, nerr := parseAlterTargetIdent(p)
		if nerr != nil {
			return nerr
		}
		if terr := expectDropTrailer(p, errPrefix); terr != nil {
			return terr
		}
		tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbRenameColumn)
		if verr != nil {
			return verr
		}
		return db.renameColumn(tbl, oldName, newName, newQuoted)

	case p.consumeKeyword("ADD"):
		// "ADD [CONSTRAINT nm] CHECK(expr) onconf" (parse.y:1915-1918):
		// CONSTRAINT and CHECK never fall back to an identifier, so neither can
		// begin an ADD COLUMN.
		if first := p.peek(); first.kind == tkIdent && !first.quoted && (first.upper() == "CONSTRAINT" || first.upper() == "CHECK") {
			consName := ""
			if p.consumeKeyword("CONSTRAINT") {
				n, _, nerr := parseAlterTargetIdent(p)
				if nerr != nil {
					return nerr
				}
				consName = n
			}
			if !p.consumeKeyword("CHECK") {
				return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
			}
			lp := p.peek()
			if !p.peekIsPunct("(") {
				return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(lp))
			}
			depth := 0
			var rp token
			for {
				t := p.next()
				if t.kind == tkEOF {
					return fmt.Errorf("engine: incomplete input")
				}
				if t.kind == tkPunct && t.text == "(" {
					depth++
				} else if t.kind == tkPunct && t.text == ")" {
					if depth--; depth == 0 {
						rp = t
						break
					}
				}
			}
			if p.consumeKeyword("ON") {
				if !p.consumeKeyword("CONFLICT") || !(p.consumeKeyword("ROLLBACK") || p.consumeKeyword("ABORT") ||
					p.consumeKeyword("FAIL") || p.consumeKeyword("IGNORE") || p.consumeKeyword("REPLACE")) {
					return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
				}
			}
			if terr := expectDropTrailer(p, errPrefix); terr != nil {
				return terr
			}
			tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbDropConstraint)
			if verr != nil {
				return verr
			}
			return db.alterAddCheck(tbl, consName, trimmed[lp.End:rp.Start], alterRtrimConstraint(trimmed[first.Start:]))
		}
		p.consumeKeyword("COLUMN")
		def, derr := parseAddColumnDef(p)
		if derr != nil {
			return derr
		}
		if p.pos <= 0 || p.pos > len(toks) {
			return fmt.Errorf("engine: internal error: %s ADD COLUMN: bad parser position", errPrefix)
		}
		def.rawText = trimmed[def.rawStart:toks[p.pos-1].End]
		if terr := expectDropTrailer(p, errPrefix); terr != nil {
			return terr
		}
		tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbAddColumn)
		if verr != nil {
			return verr
		}
		return db.addColumn(tbl, def)

	case p.consumeKeyword("DROP"):
		if p.consumeKeyword("CONSTRAINT") {
			consName, _, nerr := parseAlterTargetIdent(p)
			if nerr != nil {
				return nerr
			}
			if terr := expectDropTrailer(p, errPrefix); terr != nil {
				return terr
			}
			tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbDropConstraint)
			if verr != nil {
				return verr
			}
			return db.dropConstraint(tbl, consName)
		}
		p.consumeKeyword("COLUMN")
		colName, _, cerr := parseAlterTargetIdent(p)
		if cerr != nil {
			return cerr
		}
		if terr := expectDropTrailer(p, errPrefix); terr != nil {
			return terr
		}
		tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbDropColumn)
		if verr != nil {
			return verr
		}
		return db.dropColumn(tbl, colName)

	case p.consumeKeyword("ALTER"):
		// "ALTER [COLUMN] nm DROP NOT NULL" / "ALTER [COLUMN] nm SET NOT NULL
		// onconf" (parse.y:1909-1914).
		p.consumeKeyword("COLUMN")
		colTok := p.peek()
		colName, ok := objectNameToken(colTok)
		if !ok {
			return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(colTok))
		}
		p.next()
		switch {
		case p.consumeKeyword("DROP"):
			if !p.consumeKeyword("NOT") || !p.consumeKeyword("NULL") {
				return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
			}
			if terr := expectDropTrailer(p, errPrefix); terr != nil {
				return terr
			}
			tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbDropConstraint)
			if verr != nil {
				return verr
			}
			return db.alterDropNotNull(tbl, colName)
		case p.consumeKeyword("SET"):
			notTok := p.peek()
			if !p.consumeKeyword("NOT") || !p.consumeKeyword("NULL") {
				return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
			}
			if p.consumeKeyword("ON") {
				if !p.consumeKeyword("CONFLICT") || !(p.consumeKeyword("ROLLBACK") || p.consumeKeyword("ABORT") ||
					p.consumeKeyword("FAIL") || p.consumeKeyword("IGNORE") || p.consumeKeyword("REPLACE")) {
					return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
				}
			}
			if terr := expectDropTrailer(p, errPrefix); terr != nil {
				return terr
			}
			tbl, verr := db.resolveAlterTarget(bare, display, scope, unknownSchema, alterVerbDropConstraint)
			if verr != nil {
				return verr
			}
			return db.alterSetNotNull(tbl, colName, trimmed[colTok.Start:colTok.End], alterRtrimConstraint(trimmed[notTok.Start:]))
		}
		return fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
	}
	return fmt.Errorf("%s: expected RENAME, ADD, or DROP, got %q", errPrefix, p.tokenDesc(p.peek()))
}

// ---- RENAME TO ----

// renameTableTo implements "ALTER TABLE t RENAME TO newName": renames tbl in
// place (tbl.name and its stored CREATE TABLE sql text) and cascades the
// rename into every index (automatic or explicit) registered against it --
// its OWN sqlite_schema row's tbl_name (indexMeta.table) and, for an
// explicit index, the "ON <name>" clause of its own stored sql text.
// Declines outright if any view references tbl (see package doc comment):
// C SQLite would rewrite that view's stored SQL too, which this write
// path cannot safely reproduce.
func (db *DB) renameTableTo(tbl *tableMeta, newName string) error {
	// PRAGMA legacy_alter_table (SQLITE_LegacyAlter, alter.c:1792, 1852, 1854): C
	// skips the CHECK-qualifier walk, the partial-index WHERE walk, the trigger
	// body/WHEN rewrite and the view-body rewrite, but still rewrites the table's
	// name token and the index and trigger ON clauses. checkSchemaObjectsResolve and
	// checkViewRenameSafe exist for the full rewrite, so they are skipped too, as C
	// skips its deep resolve under legacy (alter.c:2073, 2081).
	legacy := db.LegacyAlterTable()
	if !legacy {
		// C SQLite re-parses the WHOLE schema for this ALTER form and fails
		// if any trigger or view is left dangling by an earlier DROP -- see
		// checkSchemaObjectsResolve. Checked FIRST, so a failure changes
		// nothing.
		if err := db.checkSchemaObjectsResolve(tbl); err != nil {
			return err
		}
	}
	if err := db.checkReservedObjectName(newName); err != nil {
		return err
	}
	if db.findTableMeta(newName) != nil || db.findViewMeta(newName) != nil || db.findIndexMeta(newName) != nil {
		return fmt.Errorf("engine: there is already another table or index with this name: %s", newName)
	}
	// See checkViewRenameSafe's doc comment: unlike the old blanket "any view
	// mentioning this table declines" rule, this only declines when a
	// rewrite genuinely cannot be done safely; affectedViews is applied by
	// applyViewRename once the rest of the cascade below has succeeded. Under
	// legacy neither ever runs (see above), so affectedViews stays nil.
	var affectedViews []*viewMeta
	if !legacy {
		var verr error
		affectedViews, verr = db.checkViewRenameSafe(tbl, "ALTER TABLE RENAME TO")
		if verr != nil {
			return verr
		}
		// Under PRAGMA writable_schema=ON a view C SQLite cannot re-parse
		// keeps its original text (alter.c:1884) -- see
		// renameUnparsableObjects. Filtered AFTER checkViewRenameSafe so a view
		// this write path declines to rewrite still declines for its own
		// reasons rather than being silently skipped here.
		if _, skipViews := db.renameUnparsableObjects(tbl); len(skipViews) > 0 {
			kept := affectedViews[:0]
			for _, v := range affectedViews {
				if !skipViews[v.name] {
					kept = append(kept, v)
				}
			}
			affectedViews = kept
		}
	}

	nameTok, err := locateCreateTableNameToken(tbl.sql)
	if err != nil {
		return fmt.Errorf("engine: ALTER TABLE RENAME TO: %w", err)
	}
	oldName := tbl.name
	savedSQL := tbl.sql
	savedIndexSQL := make([]string, len(db.indexes))
	for i, ix := range db.indexes {
		savedIndexSQL[i] = ix.sql
	}
	// C rewrites a CHECK's qualifier naming the table, quoted:
	//
	//	CREATE TABLE "t1new"(a, b, CHECK("t1new".a != "t1new".b))
	//
	// Under legacy it skips this (alter.c:1828), leaving the old name, after which the
	// ALTER fails (alterlegacy.test 1.2); refreshTableChecks reproduces that failure
	// via checkNoQualifiedColumnRef.
	edits := []textEdit{{nameTok.Start, nameTok.End, quoteIdent(newName)}}
	if !legacy {
		// A CHECK's qualifier gets the empty-IN() exemption like any other
		// expression reference -- see this file's "IN () wrinkle" section.
		edits = append(edits, dropEmptyInEdits(tbl.sql, checkQualifierEdits(tbl.sql, tbl.name, newName))...)
	}
	tbl.sql = applyEdits(tbl.sql, edits)
	tbl.name = newName
	// The stored text just changed, so the PARSED checks must be re-derived
	// from it -- refreshTableChecks' own rule, that the two are consistent by
	// construction rather than by two paths maintained apart. Without this a
	// CHECK's in-memory expression kept the OLD qualifier and every later
	// INSERT failed with "no such table: <oldname>", where the stored SQL was
	// already right. Under legacy this is also what turns a self-qualified
	// CHECK into the correct ALTER-time failure (see above) instead of a
	// silently-stale one.
	if rerr := refreshTableChecks(tbl); rerr != nil {
		tbl.sql, tbl.name = savedSQL, oldName
		return rerr
	}

	savedIndexName := make([]string, len(db.indexes))
	// keys/where are the PARSED half of an expression/partial index, re-derived
	// from the rewritten text at the end of this function (refreshExprIndex);
	// a declined cascade has to put them back with everything else.
	savedIndexKeys := make([][]indexKey, len(db.indexes))
	savedIndexWhere := make([]Expr, len(db.indexes))
	for i, ix := range db.indexes {
		savedIndexName[i] = ix.name
		savedIndexKeys[i], savedIndexWhere[i] = ix.keys, ix.where
	}
	for _, ix := range db.indexes {
		// Indexes are reached ONLY in the altered table's own catalog: the
		// cross-catalog pass alterSkipsCatalog documents is restricted to views
		// and triggers ("WHERE type IN ('view','trigger')"), and an index
		// belongs to whichever table it was created against. Verified directly
		// against 3.53.3: with a main.t1 and a shadowing temp.t1, renaming the
		// temp one to t4 leaves main's own "ix1 ON t1" and its automatic
		// sqlite_autoindex_t1_1 exactly as they were.
		if ix.isTemp != tbl.isTemp || !equalFoldName(ix.table, oldName) {
			continue
		}
		ix.table = newName
		if ix.sql != "" {
			onTok, terr := locateIndexOnTableToken(ix.sql)
			if terr == nil {
				// The index's ON token is rewritten unconditionally (alter.c:1842); qualifiers in
				// a partial index's WHERE only when not legacy (alter.c:1844, see
				// indexWhereQualifierEdits). Under legacy the stale qualifier makes the ALTER
				// fail as in C (alterlegacy.test 1.3), via checkAndCollectIndexExprCols.
				edits := []textEdit{{onTok.Start, onTok.End, quoteIdent(newName)}}
				if !legacy {
					edits = append(edits, dropEmptyInEdits(ix.sql, indexWhereQualifierEdits(ix.sql, oldName, newName))...)
				}
				ix.sql = applyEdits(ix.sql, edits)
			}
			continue
		}
		// An AUTOMATIC index has no SQL and carries the table's name in its OWN
		// name (sqlite_autoindex_<table>_<N>, index_write.go), so the rename has
		// to follow there too: C SQLite reports sqlite_autoindex_ppp_1 after
		// "ALTER TABLE p1 RENAME TO ppp", not sqlite_autoindex_p1_1 -- verified,
		// and visible in sqlite_master and in PRAGMA index_list.
		if suffix, ok := autoIndexNameSuffix(ix.name, oldName); ok {
			renamed := "sqlite_autoindex_" + newName + suffix
			ix.name = renamed
		}
	}

	savedTableSQL := make(map[*tableMeta]string, len(db.tables))
	for _, t := range db.tables {
		savedTableSQL[t] = t.sql
	}
	// alter.c:1817 rewrites FK references when not legacy, or when foreign_keys is
	// ON even under legacy (alterlegacy.test 8.2).
	if !legacy || db.ForeignKeys() {
		db.renameForeignKeyReferences(tbl, oldName, newName)
	}

	// Trigger bodies are rewritten in place (applyTriggerRename mutates each
	// affected *triggerMeta directly), so a snapshot is taken UNCONDITIONALLY
	// here -- before either cascade below runs -- rather than only once the
	// trigger cascade is known to have touched something: applyViewRename
	// runs AFTER it, and a declined view cascade must undo an already-applied
	// trigger cascade just as completely as a declined trigger cascade undoes
	// the table/index edits above.
	savedTriggers := make(map[*triggerMeta]triggerMeta, len(db.triggers))
	for _, tr := range db.triggers {
		savedTriggers[tr] = *tr
	}
	rollback := func() {
		for t, sqlText := range savedTableSQL {
			t.sql = sqlText
		}
		// Put the table back: a declined cascade must change nothing.
		tbl.name = oldName
		tbl.sql = savedSQL
		for _, ix := range db.indexes {
			// Same catalog scoping as the forward loop, or this would revert an
			// index of the OTHER catalog that legitimately sits on a table
			// already called newName.
			if ix.isTemp == tbl.isTemp && equalFoldName(ix.table, newName) {
				ix.table = oldName
			}
		}
		for i, sql := range savedIndexSQL {
			db.indexes[i].sql = sql
		}
		for i, name := range savedIndexName {
			db.indexes[i].name = name
		}
		for i, k := range savedIndexKeys {
			db.indexes[i].keys, db.indexes[i].where = k, savedIndexWhere[i]
		}
		for tr, snap := range savedTriggers {
			*tr = snap
		}
	}

	// Under legacy C still rewrites a trigger's ON clause when it names oldName, but
	// skips the WHEN and body (alter.c:1854-1873); renameTriggerOnClauseLegacy does
	// just that.
	if legacy {
		db.renameTriggerOnClauseLegacy(tbl, oldName, newName)
	} else if terr := db.renameTriggerReferences(tbl, oldName, newName); terr != nil {
		rollback()
		return terr
	}
	// The view body walk is the other `if( isLegacy==0 )` in renameTableFunc
	// (alter.c:1795-1802): under legacy a view naming oldName is left reading
	// it, unrewritten -- alterlegacy.test 3.x's own point, where "SELECT *
	// FROM vvv" after the rename fails with "no such table: main.txx" rather
	// than seeing the new name. affectedViews is nil whenever legacy is on
	// (see above), so this is a no-op then regardless.
	if !legacy {
		if terr := applyViewRename(affectedViews, oldName, identRepl{name: newName, force: true, isTable: true}, "ALTER TABLE RENAME TO", "", nil); terr != nil {
			rollback()
			return terr
		}
	}
	// The stored index text just changed, so any PARSED expression derived
	// from it must be re-derived -- refreshTableChecks' rule, applied to a
	// partial index's WHERE (refreshExprIndex). Last, so a cascade that
	// declines above never has to unwind it.
	for _, ix := range db.indexes {
		if ix.isTemp != tbl.isTemp || !equalFoldName(ix.table, newName) {
			continue
		}
		if rerr := refreshExprIndex(ix, tbl); rerr != nil {
			rollback()
			return rerr
		}
	}
	// sqlite_sequence's rows -- last, for the same reason the index-expression
	// refresh just above is last: nothing follows it, so a failure here never
	// has to be unwound by anything else.
	if rerr := db.renameSqliteSequenceRow(tbl, oldName, newName); rerr != nil {
		rollback()
		return rerr
	}

	db.bumpSchema(tbl.isTemp)
	return nil
}

// renameSqliteSequenceRow renames oldName's row in sqlite_sequence, if the
// renamed table's catalog has that table (alter.c:246):
//
//	if( sqlite3FindTable(db, "sqlite_sequence", zDb) ){
//	  sqlite3NestedParse(pParse,
//	      "UPDATE \"%w\".sqlite_sequence set name = %Q WHERE name = %Q",
//	      zDb, zName, pTab->zName);
//	}
//
// Unconditional, even for a non-AUTOINCREMENT table or a user table that happens
// to be named sqlite_sequence (without_rowid1.test 15.1). tbl.name is already
// newName. Scoped to tbl's own catalog, since main and temp can each have one.
func (db *DB) renameSqliteSequenceRow(tbl *tableMeta, oldName, newName string) error {
	if db.findTableMetaIn(createScope(tbl.isTemp), sqliteSequenceTableName) == nil {
		return nil
	}
	// The row store is written directly rather than through UpdateArgs, which would
	// re-enter the SQL layer from inside DDL (as clearStat1Rows does for stat1). The
	// scoped lookup picks the right catalog. sqlite_sequence has no index, trigger,
	// FK or CHECK, so nothing else applies.
	seqTbl := db.findTableMetaIn(createScope(tbl.isTemp), sqliteSequenceTableName)
	if err := db.ensureTableLoaded(seqTbl); err != nil {
		return err
	}
	for rowid, vals := range seqTbl.rows.all() {
		if len(vals) == 0 || vals[0].Typ != Text || string(vals[0].S) != oldName {
			continue
		}
		next := append([]Value(nil), vals...)
		next[0] = Value{Typ: Text, S: []byte(newName)}
		seqTbl.putRow(rowid, next)
	}
	return nil
}

// renameTriggerOnClauseLegacy rewrites only the "ON <table>" token of every
// trigger whose own target is oldName, for legacy_alter_table=ON: C's ON-clause
// rewrite (alter.c:1854) is outside the isLegacy guard on the body walk
// (alter.c:1859). It cannot fail. Selection follows renameTriggerReferences'
// catalog scoping and tableAttachName guard.
func (db *DB) renameTriggerOnClauseLegacy(tbl *tableMeta, oldName, newName string) {
	for _, tr := range db.triggers {
		if alterSkipsCatalog(tbl.isTemp, tr.isTemp) || tr.tableAttachName != "" {
			continue
		}
		if !equalFoldName(tr.table, oldName) {
			continue
		}
		onTok, terr := locateTriggerOnTableToken(tr.sql)
		if terr != nil {
			continue // every stored trigger re-lexes; unreachable in practice
		}
		tr.sql = applyEdits(tr.sql, []textEdit{{onTok.Start, onTok.End, quoteIdent(newName)}})
		tr.table = newName
	}
}

// renameTriggerReferences cascades "RENAME TO" into every trigger that names
// oldName, in its ON clause or WHEN/body. C rewrites the stored SQL and the
// trigger keeps firing on the new name; leaving it stale silently stops it.
//
// The rewrite is token-based (every identifier spelling oldName becomes the
// quoted newName), so a column anywhere named oldName makes it decline. Catalog
// scoping is alterSkipsCatalog's: a TEMP trigger is rewritten for a MAIN rename,
// never the reverse. A shadowed cross-catalog rename splits in C (a body
// target matched by bare name, other references by resolved identity):
//
//	CREATE TRIGGER ttr AFTER INSERT ON drv
//	  BEGIN INSERT INTO "t9"(a) SELECT a FROM t1; END
//
// so that case is taken only when the whole-text rewrite coincides, else
// declined.
func (db *DB) renameTriggerReferences(tbl *tableMeta, oldName, newName string) error {
	shadowed := !tbl.isTemp && db.findTableMetaIn(createScope(true), oldName) != nil
	// Under PRAGMA writable_schema=ON a trigger C SQLite cannot re-parse
	// keeps its original text (alter.c:1884) -- see renameUnparsableObjects.
	// nil when the pragma is off, which skips nothing.
	skipTriggers, _ := db.renameUnparsableObjects(tbl)
	var affected []*triggerMeta
	for _, tr := range db.triggers {
		if alterSkipsCatalog(tbl.isTemp, tr.isTemp) {
			continue
		}
		if skipTriggers[tr.name] {
			continue
		}
		// tbl is always local (ALTER has no cross-database form), so a trigger bound to
		// an attached database's table is never the target here, whatever its bare name;
		// the whole-text rewrite could not tell those apart (trigger1.test 10.x), so skip
		// it.
		if tr.tableAttachName != "" {
			continue
		}
		if !equalFoldName(tr.table, oldName) && !sqlMentionsIdent(tr.sql, oldName) &&
			len(nameStringTokens(tr.sql, oldName)) == 0 {
			continue
		}
		if shadowed && tr.isTemp {
			rewritable := len(triggerStepTargetsNamed(tr, oldName))
			if equalFoldName(tr.table, oldName) && tr.tableIsTemp == tbl.isTemp {
				rewritable++
			}
			if countIdentMentions(tr.sql, oldName) != rewritable {
				return fmt.Errorf("%w: ALTER TABLE %s RENAME TO: temp trigger %s references it while a TEMP table shadows the name (C SQLite rewrites the trigger's body TARGETS by name and resolves every other reference to the shadow, which this write path's whole-text rewrite cannot reproduce)", errVDBEUnsupported, oldName, tr.name)
			}
		}
		affected = append(affected, tr)
	}
	if len(affected) == 0 {
		return nil
	}
	for _, t := range db.tables {
		for _, c := range t.cols {
			if !equalFoldName(c.Name, oldName) {
				continue
			}
			// A same-named COLUMN only blocks the rewrite where this engine
			// cannot tell it from a table reference. r45TriggerColumnRefSkips
			// proves it can by COUNT agreement with the parser, or reports
			// false and the decline stands. applyTriggerRename recomputes the
			// identical spans.
			for _, tr := range affected {
				if _, ok := r45TriggerColumnRefSkips(tr, oldName); !ok {
					return fmt.Errorf("engine: unsupported: ALTER TABLE RENAME TO: renaming table %s, which a trigger references while column %s.%s shares its name, is not supported by this write path (the trigger-body rewrite could not tell the two apart)", oldName, t.name, c.Name)
				}
			}
		}
	}
	return applyTriggerRename(affected, oldName, identRepl{name: newName, force: true, isTable: true}, true, "ALTER TABLE RENAME TO", "", nil)
}

// applyTriggerRename rewrites oldName to repl in each trigger's stored SQL and
// re-parses it, the text being the source of truth. Nothing is committed until
// every trigger is rewritten and parsed. isTable marks a table rename, where a
// string "nm" is reachable: C rewrites "... ON 't8'" (alter.c:1852), so the ON
// clause is spliced directly and any other name-position string declines.
func applyTriggerRename(affected []*triggerMeta, oldName string, repl identRepl, isTable bool, verb string, ownerTable string, hasCol func(table, col string) bool) error {
	rewritten := make([]*triggerMeta, len(affected))
	for i, tr := range affected {
		// One splice pass over the ORIGINAL text, so every edit shares one
		// coordinate system: the identifier tokens rewriteIdentInSQL finds, plus
		// -- for a table rename -- the ON clause when it is spelled as a string.
		// A trigger body is subject to the same empty-IN() elision a view is
		// (rewriteIdentInSQL honours the exempt spans); this cascade never had
		// the guard the view one did, so an UNSURE span used to be rewritten
		// blind -- altertab3.test 9.x's own trigger came back with the operand
		// of "(SELECT a, b FROM (t1)) IN ()" repointed at the new name.
		if emptyInBlocks(tr.sql, oldName) {
			return fmt.Errorf("engine: unsupported: %s: trigger %s names %s inside the left operand of an empty IN() list whose extent this write path could not pin down (see this file's emptyInSpans)", verb, tr.name, oldName)
		}
		extra := []token(nil)
		if isTable {
			strs := nameStringTokens(tr.sql, oldName)
			onTok, oerr := locateTriggerOnTableToken(tr.sql)
			if oerr == nil && onTok.kind == tkString && equalFoldName(onTok.str, oldName) {
				extra = append(extra, onTok)
				strs = dropTokenAt(strs, onTok.Start)
			}
			if len(strs) > 0 {
				// nameStringTokens is LEXICAL and deliberately generous: a
				// string after a comma is a second FROM item in "FROM a, 'b'"
				// and an ordinary VALUE in "VALUES(x, 'b')", and the tokens
				// alone cannot tell those apart. r45TriggerNameStrings settles
				// it against the parser, by the same count agreement
				// r45TriggerColumnRefSkips uses; only when THAT cannot prove
				// it does the decline stand.
				named, ok := r45TriggerNameStrings(tr, oldName)
				if !ok {
					return fmt.Errorf("engine: unsupported: %s: trigger %s spells %s as a STRING literal outside its ON clause, where this write path cannot tell a name position from a value one (C SQLite resolves it and rewrites only the name)", verb, tr.name, oldName)
				}
				for _, t := range named {
					if oerr == nil && t.Start == onTok.Start {
						continue // already in extra
					}
					extra = append(extra, t)
				}
			}
		}
		// An INSTEAD OF trigger's NEW./OLD. names a VIEW's column, which a
		// base-table COLUMN rename never renames -- see r33rOldNewQualified.
		// (A TABLE rename cannot reach the position: oldName would have to be
		// spelled as a column there.)
		skips := r33rSkips{oldNew: !isTable && tr.insteadOf}
		// ...and never the trigger's own name: "CREATE TRIGGER t1 AFTER INSERT
		// ON tbl" came back as CREATE TRIGGER "t2" after "ALTER TABLE t1 RENAME
		// TO t2", under a schema row still named t1, which C SQLite refuses
		// to load ("malformed database schema (t1)", schema4.test).
		if sp, ok := triggerOwnNameSpan(tr.sql); ok {
			skips.spans = append(skips.spans, sp)
		}
		// ...and, for a TABLE rename, every bare COLUMN reference that merely
		// spells the table's name. The caller has already proved these are
		// recoverable for every affected trigger, or declined.
		if isTable {
			if colRefs, ok := r45TriggerColumnRefSkips(tr, oldName); ok {
				skips.spans = append(skips.spans, colRefs...)
			}
		} else if ownerTable != "" && hasCol != nil {
			// A COLUMN rename: every reference this body carries that belongs
			// to some OTHER table than the one being altered -- see
			// r45TriggerColumnRenameSkips. The caller proved these recoverable
			// for every affected trigger, or declined.
			if others, ok := r45TriggerColumnRenameSkips(tr, ownerTable, oldName, hasCol); ok {
				skips.spans = append(skips.spans, others...)
			}
		}
		newSQL, rerr := rewriteIdentInSQL(tr.sql, oldName, repl, skips, extra...)
		if rerr != nil {
			return rerr
		}
		stmt, perr := parseCreateTriggerStmt(newSQL)
		if perr != nil {
			return fmt.Errorf("engine: %s: rewriting trigger %s: %w", verb, tr.name, perr)
		}
		nt := *tr
		nt.sql = normalizeSchemaSQL("TRIGGER", newSQL)
		nt.table = stmt.table
		nt.when = stmt.when
		nt.body = stmt.body
		nt.updateCols = stmt.updateCols
		rewritten[i] = &nt
	}
	for i, tr := range affected {
		*tr = *rewritten[i]
	}
	return nil
}

// renameTriggerColumnReferences cascades RENAME COLUMN into every trigger naming
// the column: "UPDATE OF" lists (altercol.test 7.1.3), NEW./OLD. references and
// body references. Same token-based rewrite and ambiguity limits as
// renameTriggerReferences. repl is the shared rendered replacement, so a quoted
// new name is quoted everywhere, as renameEditSql does.
func (db *DB) renameTriggerColumnReferences(tbl *tableMeta, oldName string, repl identRepl) error {
	shadowed := alterColumnShadowed(db, tbl)
	var affected []*triggerMeta
	for _, tr := range db.triggers {
		// See renameTriggerReferences' identical guard: tbl is always LOCAL,
		// so a trigger bound to an ATTACHed database's table
		// (triggerMeta.tableAttachName) can never legitimately need ITS OWN
		// stored text rewritten for a LOCAL column rename -- a body
		// reference like "new.a" there names a column on the ATTACHMENT's
		// own table, not tbl's, however the bare identifier compares.
		if tr.tableAttachName != "" {
			continue
		}
		if alterSkipsCatalog(tbl.isTemp, tr.isTemp) || (shadowed && tr.isTemp) {
			continue
		}
		// A bare oldName in tr can bind to tbl only if tbl is in scope there, which needs
		// tbl's name in tr's text (alter.c:1015); the same filter as
		// checkViewColumnRenameSafe.
		if sqlMentionsTableName(tr.sql, tbl.name) && sqlMentionsIdent(tr.sql, oldName) {
			affected = append(affected, tr)
		}
	}
	if len(affected) == 0 {
		return nil
	}
	// A name clash only blocks the rewrite where this engine cannot attribute
	// the trigger's references. Usually it can, without resolving anything the
	// hard way: NEW./OLD. name the trigger's own table, any other qualifier
	// names a FROM item, and a bare reference belongs to whichever single table
	// the body can see that CARRIES a column of that name. See
	// r45TriggerColumnRenameSkips, which applyTriggerRename recomputes.
	trigProvable := true
	for _, atr := range affected {
		if _, ok := r45TriggerColumnRenameSkips(atr, tbl.name, oldName, db.tableHasColumn); !ok {
			trigProvable = false
			break
		}
	}
	for _, t := range db.tables {
		if equalFoldName(t.name, oldName) && !trigProvable {
			return fmt.Errorf("engine: unsupported: ALTER TABLE RENAME COLUMN: renaming %s.%s, which a trigger references while table %s shares its name, is not supported by this write path (the trigger-body rewrite could not tell the two apart)", tbl.name, oldName, t.name)
		}
		if t == tbl {
			continue
		}
		for _, c := range t.cols {
			if !equalFoldName(c.Name, oldName) {
				continue
			}
			// A same-named column in another table makes a trigger ambiguous only if that
			// table is mentioned in that trigger (C binds each reference by resolution,
			// alter.c:1015), so check per affected trigger rather than schema-wide (func.test
			// func-33.20).
			for _, atr := range affected {
				if trigProvable {
					continue
				}
				if sqlMentionsIdent(atr.sql, t.name) {
					return fmt.Errorf("engine: unsupported: ALTER TABLE RENAME COLUMN: renaming %s.%s, which trigger %s references while column %s.%s shares its name, is not supported by this write path (the trigger-body rewrite could not tell the two apart)", tbl.name, oldName, atr.name, t.name, c.Name)
				}
			}
		}
	}
	for _, v := range db.views {
		if equalFoldName(v.name, oldName) && !trigProvable {
			return fmt.Errorf("engine: unsupported: ALTER TABLE RENAME COLUMN: renaming %s.%s, which a trigger references while view %s shares its name, is not supported by this write path (the trigger-body rewrite could not tell the two apart)", tbl.name, oldName, v.name)
		}
	}
	for _, tr := range affected {
		for _, sel := range r34vTriggerSelects(tr) {
			if r34vDerivedRebindsColumn(sel, oldName) {
				return fmt.Errorf("engine: unsupported: ALTER TABLE RENAME COLUMN: renaming %s.%s, which trigger %s references THROUGH a derived table that outputs a column of the same name, is not supported by this write path (the reference resolves to the SUBQUERY's column, which C SQLite leaves alone, and the trigger-body rewrite could not tell the two apart)", tbl.name, oldName, tr.name)
			}
		}
	}
	return applyTriggerRename(affected, oldName, repl, false, "ALTER TABLE RENAME COLUMN", tbl.name, db.tableHasColumn)
}

// r34vTriggerSelects returns the SELECT statements written directly in tr's
// body -- a bare body SELECT and an "INSERT ... SELECT" source. Deliberately
// shallow: its one caller only widens a DECLINE, so a SELECT it misses costs
// coverage (the pre-existing whole-text rewrite still runs) and never
// correctness.
func r34vTriggerSelects(tr *triggerMeta) []*SelectStmt {
	var out []*SelectStmt
	for i := range tr.body {
		if s := tr.body[i].sel; s != nil {
			out = append(out, s)
		}
		if ins := tr.body[i].insert; ins != nil && ins.selectStmt != nil {
			out = append(out, ins.selectStmt)
		}
	}
	return out
}

// r34vDerivedRebindsColumn reports whether sel (or a compound arm or nested
// derived table) has a derived FROM item exposing name while the enclosing select
// references name. That reference binds to the subquery's column, which C does
// not rewrite (alter.c:1022), so after renaming the inner column the ALTER fails:
//
//	INSERT INTO t1(a,b) SELECT a, b FROM (SELECT a, b FROM t1)
//
// Declining reproduces that refusal; where the outer select does not reference
// name, the rewrite is right.
func r34vDerivedRebindsColumn(sel *SelectStmt, name string) bool {
	if sel == nil {
		return false
	}
	for _, it := range sel.From {
		// A FROM item naming one of this statement's own CTEs is a derived table
		// too -- "WITH a(a) AS (SELECT 1) SELECT a, a FROM a" resolves both
		// select-list "a"s against the CTE's column, so 3.53.3 rewrites NEITHER
		// (it rewrites only the id-list "a" of the enclosing INSERT step,
		// verified directly). Same rule, same reason as a parenthesized subquery.
		if it.Subquery == nil && it.Table != "" {
			for _, cte := range sel.CTEs {
				if !equalFoldName(cte.Name, it.Table) {
					continue
				}
				if r34vCTEOutputsColumn(cte, name) && r34vSelectRefsColumn(sel, name) {
					return true
				}
			}
			continue
		}
		if it.Subquery == nil {
			continue
		}
		if r34vSubqueryOutputsColumn(it.Subquery, name) && r34vSelectRefsColumn(sel, name) {
			return true
		}
		if r34vDerivedRebindsColumn(it.Subquery, name) {
			return true
		}
	}
	for _, arm := range sel.Compound {
		if r34vDerivedRebindsColumn(arm.Stmt, name) {
			return true
		}
	}
	for _, cte := range sel.CTEs {
		if r34vDerivedRebindsColumn(cte.Select, name) {
			return true
		}
	}
	return false
}

// r34vCTEOutputsColumn reports whether cte's columns can include one named
// name: its explicit "(col, ...)" rename list decides on its own when present,
// otherwise its body's own result columns do.
func r34vCTEOutputsColumn(cte CTEDef, name string) bool {
	if cte.ColNames != nil {
		for _, c := range cte.ColNames {
			if equalFoldName(c, name) {
				return true
			}
		}
		return false
	}
	return r34vSubqueryOutputsColumn(cte.Select, name)
}

// r34vSubqueryOutputsColumn reports whether sub's result columns can include
// one named name: an explicit "AS name", a bare "name" reference, or a "*"
// (whose expansion this static pass cannot see, so it answers yes).
func r34vSubqueryOutputsColumn(sub *SelectStmt, name string) bool {
	if sub == nil {
		return false
	}
	for _, c := range sub.Columns {
		switch {
		case c.Star:
			return true
		case c.HasAlias:
			if equalFoldName(c.Alias, name) {
				return true
			}
		default:
			if col, ok := c.Expr.(ColumnExpr); ok && equalFoldName(col.Name, name) {
				return true
			}
		}
	}
	return false
}

// r34vSelectRefsColumn reports whether sel's OWN expressions name a column
// called name -- its select list, WHERE, GROUP BY/HAVING, ORDER BY and JOIN-ON
// clauses. It deliberately does not walk into a FROM-clause subquery (whose
// references belong to that subquery's scope, not this one); an expression
// subquery IS walked, by exprTreeMentionsColumn, which only over-matches and so
// only widens the decline its caller raises.
func r34vSelectRefsColumn(sel *SelectStmt, name string) bool {
	for _, c := range sel.Columns {
		if !c.Star && exprTreeMentionsColumn(c.Expr, name) {
			return true
		}
	}
	if exprTreeMentionsColumn(sel.Where, name) || exprTreeMentionsColumn(sel.Having, name) {
		return true
	}
	for _, g := range sel.GroupBy {
		if exprTreeMentionsColumn(g, name) {
			return true
		}
	}
	for _, ot := range sel.OrderBy {
		if exprTreeMentionsColumn(ot.Expr, name) {
			return true
		}
	}
	for _, it := range sel.From {
		if it.On != nil && exprTreeMentionsColumn(it.On, name) {
			return true
		}
	}
	return false
}

// sqlMentionsIdent reports whether sqlText has an identifier token spelling
// name (case-insensitively, quoted or not).
func sqlMentionsIdent(sqlText, name string) bool {
	return countIdentMentions(sqlText, name) > 0
}

// sqlMentionsTableName is sqlMentionsIdent plus the string spelling of a table
// name ("INSERT INTO 'a'"), which C resolves the same way. The generous side: a
// false positive only pulls a trigger into checks that must still pass
// (r45TriggerColumnRenameSkips); a miss leaves a stale body.
func sqlMentionsTableName(sqlText, name string) bool {
	return sqlMentionsIdent(sqlText, name) || len(nameStringTokens(sqlText, name)) > 0
}

// viewTransitivelyMentions reports whether v's SQL, or any view its FROM reaches
// recursively, mentions name. An ambiguity through an intermediate view is still
// refused by C (resolve.c:785): in "CREATE VIEW vt AS SELECT q, a FROM t; CREATE
// VIEW av AS SELECT a FROM tbl, vt", "a" is ambiguous though t never appears in
// av. visited guards cycles.
func viewTransitivelyMentions(db *DB, v *viewMeta, name string, visited map[string]bool) bool {
	if visited[v.name] {
		return false
	}
	visited[v.name] = true
	if sqlMentionsIdent(v.sql, name) {
		return true
	}
	for _, other := range db.views {
		if other == v || !sqlMentionsIdent(v.sql, other.name) {
			continue
		}
		if viewTransitivelyMentions(db, other, name, visited) {
			return true
		}
	}
	return false
}

// countIdentMentions counts identifier tokens spelling name, exactly those
// rewriteIdentInSQL would replace; renameTriggerReferences compares it with the
// positions C would rewrite. A lex failure counts 0. Mentions inside an exempt
// empty-IN() operand are not counted, since nothing rewrites them
// (altertab3.test 21.1).
func countIdentMentions(sqlText, name string) int {
	toks, err := lex(sqlText)
	if err != nil {
		return 0
	}
	exempt, _ := emptyInSpans(sqlText)
	n := 0
	for _, t := range toks {
		if t.kind == tkIdent && equalFoldName(t.text, name) && !spansContain(exempt, t.Start) {
			n++
		}
	}
	return n
}

// triggerStepTargetsNamed returns tr's body steps whose TARGET table is name.
// Those are the references renameTableFunc matches by bare name rather than by
// resolved identity -- see renameTriggerReferences' doc comment.
func triggerStepTargetsNamed(tr *triggerMeta, name string) []*triggerBodyStmt {
	var out []*triggerBodyStmt
	for i := range tr.body {
		bs := &tr.body[i]
		switch {
		case bs.insert != nil && equalFoldName(bs.insert.table, name):
			out = append(out, bs)
		case bs.update != nil && equalFoldName(bs.update.table, name):
			out = append(out, bs)
		case bs.delete != nil && equalFoldName(bs.delete.table, name):
			out = append(out, bs)
		}
	}
	return out
}

// r33rSkips names the positions a rewrite must leave alone BEYOND
// rewriteIdentInSQL's own universal rules (the empty-IN() exemption, a call
// name, an AS-bound name and -- for a column rename -- a qualifier).
type r33rSkips struct {
	// oldNew exempts NEW./OLD.-qualified column names -- set for an INSTEAD OF
	// trigger, whose pseudo-row is a VIEW's columns. See r33rOldNewQualified.
	oldNew bool
	// spans exempts byte regions; used for a view's "CREATE VIEW v(c1,c2)" column
	// list, which C's view rename does not walk (alter.c:1584): the declared names
	// keep the old spelling.
	spans []byteSpan
}

// rewriteIdentInSQL replaces every identifier token spelling oldName with repl,
// already rendered by the caller: a table rename force-quotes (FROM "t1x"), a
// column rename stays bare when possible (ORDER BY aaa). String and blob literals
// are left alone. extra adds non-identifier tokens (a string name) the caller has
// located grammatically in this same text; skips adds caller exemptions
// (r33rSkips).
func rewriteIdentInSQL(sqlText, oldName string, repl identRepl, skips r33rSkips, extra ...token) (string, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return "", fmt.Errorf("engine: re-lexing stored SQL: %w", err)
	}
	// The left operand of an empty "IN ()" is unmapped at parse time and so is
	// never rewritten -- see this file's "IN () wrinkle" section.
	exempt, _ := emptyInSpans(sqlText)
	var edits []textEdit
	for i, t := range toks {
		if t.kind == tkIdent && equalFoldName(t.text, oldName) &&
			!spansContain(exempt, t.Start) && !spansContain(skips.spans, t.Start) &&
			!isCallNameToken(toks, i, repl.isTable) && !r33rAfterAS(toks, i) &&
			!(!repl.isTable && r33rQualifierToken(toks, i)) &&
			!(!repl.isTable && r34vBoundNameToken(toks, i)) &&
			!(skips.oldNew && r33rOldNewQualified(toks, i)) {
			edits = append(edits, textEdit{t.Start, t.End, repl.at(sqlText, t)})
		}
	}
	for _, t := range extra {
		edits = append(edits, textEdit{t.Start, t.End, repl.at(sqlText, t)})
	}
	return applyEdits(sqlText, edits), nil
}

// isCallNameToken reports whether toks[i] is followed by "(", i.e. names a
// function, CTE or column list rather than a column or table reference. C's
// walkers never reach one (alter.c:1022, 1725): "SELECT a () FILTER (WHERE a>0)"
// renames only the inner a. For a table rename a name may legitimately be
// followed by "(" ("INSERT INTO t1(a,b)"), so it is skipped only when the
// preceding token cannot introduce a name.
func isCallNameToken(toks []token, i int, isTable bool) bool {
	if i+1 >= len(toks) || toks[i+1].kind != tkPunct || toks[i+1].text != "(" {
		return false
	}
	if !isTable {
		return true
	}
	if i == 0 {
		return true
	}
	prev := toks[i-1]
	if prev.kind == tkPunct && (prev.text == "." || prev.text == ",") {
		return false
	}
	if prev.kind == tkIdent && !prev.quoted {
		switch prev.upper() {
		case "ON", "INTO", "FROM", "UPDATE", "JOIN", "TABLE", "REFERENCES":
			return false
		}
	}
	return true
}

// r33rViewColumnListSpan locates a "CREATE VIEW v(c1,c2,...) AS ..." statement's
// DECLARED column list -- the parenthesized group that precedes the top-level
// AS -- and reports its byte extent. A view written without one ("CREATE VIEW v
// AS SELECT ...") has no top-level "(" before the AS, so ok is false.
func r33rViewColumnListSpan(sqlText string) (byteSpan, bool) {
	toks, err := lex(sqlText)
	if err != nil {
		return byteSpan{}, false
	}
	for i, t := range toks {
		if t.kind == tkIdent && !t.quoted && t.upper() == "AS" {
			return byteSpan{}, false // the AS came first: no declared list
		}
		if t.kind != tkPunct || t.text != "(" {
			continue
		}
		depth := 0
		for j := i; j < len(toks); j++ {
			if toks[j].kind != tkPunct {
				continue
			}
			switch toks[j].text {
			case "(":
				depth++
			case ")":
				if depth--; depth == 0 {
					return byteSpan{start: t.Start, end: toks[j].End}, true
				}
			}
		}
		return byteSpan{}, false
	}
	return byteSpan{}, false
}

// r33rAfterAS reports whether toks[i] directly follows AS: a name being bound
// (result-column alias, FROM alias, WINDOW name) or a CAST type, never a
// reference C's rename walkers reach (alter.c:1584; aliases are rewritten only
// for a trigger step targeting the table, alter.c:1654). Rewriting them renamed a
// view's own output column ("SELECT a AS a" must become "SELECT aaa AS a").
func r33rAfterAS(toks []token, i int) bool {
	if i < 1 {
		return false
	}
	prev := toks[i-1]
	return prev.kind == tkIdent && !prev.quoted && prev.upper() == "AS"
}

// r34vBoundNameToken reports whether toks[i] is not a column reference for a
// column rename: a table name right after an unquoted FROM or JOIN, or a name
// defined by "<name> [(<cols>)] AS (" (a CTE or WINDOW) with its column list.
// Neither produces a TK_COLUMN (alter.c:1015, 1654):
//
//	INSERT INTO t1(a,b) WITH a AS (SELECT 1 AS z) SELECT z,z FROM a
//
// renames only the id-list "a". Column renames only; a table rename does rewrite
// FROM names (alter.c:1725).
func r34vBoundNameToken(toks []token, i int) bool {
	if i > 0 {
		if prev := toks[i-1]; prev.kind == tkIdent && !prev.quoted {
			switch prev.upper() {
			case "FROM", "JOIN":
				return true
			}
		}
	}
	// "<name> AS (" -- the definition itself.
	if r34vFollowedByASParen(toks, i+1) {
		return true
	}
	// Inside a "<name> ( c1, c2 ) AS (" column list: walk back to the "(" that
	// opens this group and test the identifier in front of it.
	depth := 0
	for j := i - 1; j >= 0; j-- {
		t := toks[j]
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case ")":
			depth++
		case "(":
			if depth > 0 {
				depth--
				continue
			}
			return j > 0 && toks[j-1].kind == tkIdent && r34vFollowedByASParen(toks, j)
		}
	}
	return false
}

// r34vFollowedByASParen reports whether the tokens starting at j are either
// "AS (" or "( ... ) AS (" -- the tail of a CTE/window definition's head.
func r34vFollowedByASParen(toks []token, j int) bool {
	if j < len(toks) && toks[j].kind == tkPunct && toks[j].text == "(" {
		depth := 0
		for ; j < len(toks); j++ {
			if toks[j].kind != tkPunct {
				continue
			}
			if toks[j].text == "(" {
				depth++
			} else if toks[j].text == ")" {
				depth--
				if depth == 0 {
					j++
					break
				}
			}
		}
	}
	return j+1 < len(toks) &&
		toks[j].kind == tkIdent && !toks[j].quoted && toks[j].upper() == "AS" &&
		toks[j+1].kind == tkPunct && toks[j+1].text == "("
}

// r33rQualifierToken reports whether toks[i] is followed by "." (the qualifier of
// "x.c"). A column rename never touches it (alter.c:1015); a table rename does
// (alter.c:1696). "SELECT a.a FROM t1 AS a" becomes "SELECT a.aaa ...".
func r33rQualifierToken(toks []token, i int) bool {
	return i+1 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "."
}

// r33rOldNewQualified reports whether toks[i] is the column of a NEW.<col> /
// OLD.<col> reference. In an INSTEAD OF trigger those bind to the view
// (pTriggerTab), not the altered table (alter.c:1022), so a base-table rename
// leaves them alone even when the view projects the column:
//
//	CREATE VIEW v1 AS SELECT a AS a, b FROM t1;
//	CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a,new.b); END;
//	ALTER TABLE t1 RENAME a TO aaa;
//
// leaves the trigger unchanged. "x.new.a" is a schema-qualified table, not the
// pseudo-row.
func r33rOldNewQualified(toks []token, i int) bool {
	if i < 2 || toks[i-1].kind != tkPunct || toks[i-1].text != "." {
		return false
	}
	q := toks[i-2]
	if q.kind != tkIdent || (!equalFoldName(q.text, "new") && !equalFoldName(q.text, "old")) {
		return false
	}
	return i < 3 || toks[i-3].kind != tkPunct || toks[i-3].text != "."
}

// dropTokenAt returns ts without the token starting at byte offset start.
func dropTokenAt(ts []token, start int) []token {
	out := ts[:0]
	for _, t := range ts {
		if t.Start != start {
			out = append(out, t)
		}
	}
	return out
}

// ---- view-rewrite cascade ----
//
// Views are rewritten with the same token-based rewrite as triggers and foreign
// keys, plus one guard: the empty-IN() exemption.
//
// ---- the "IN ()" wrinkle (parse.y:1491) ----
//
//	expr(A) ::= expr(A) in_op(N) LP exprlist(Y) RP. [IN] {
//	  if( Y==0 ){
//	    Expr *pB = sqlite3Expr(db, TK_STRING, N ? "true" : "false");
//	    if( pB ) sqlite3ExprIdToTrueFalse(pB);
//	    if( !ExprHasProperty(A, EP_HasFunc) ){
//	      sqlite3ExprUnmapAndDelete(pParse, A);
//	      A = pB;
//	    }else{
//	      A = sqlite3PExpr(pParse, N ? TK_OR : TK_AND, pB, A);
//	    }
//	  }else{ ... }
//
// sqlite3ExprUnmapAndDelete unmaps the operand's tokens from Parse.pRename
// (expr.c:1472, alter.c:914), so a rename leaves them byte-identical even when
// they name the renamed object; with a function present they stay mapped and are
// rewritten. This parser keeps the operand regardless, so the elided span is
// found in the text (emptyInSpans). EP_HasFunc does not cross a subquery
// (expr.c:850), so "(SELECT abs(b)) IN ()" is elided and "abs(b) IN ()" is not.

// byteSpan is a half-open [start,end) byte range of a stored SQL string.
type byteSpan struct{ start, end int }

func (s byteSpan) contains(pos int) bool { return pos >= s.start && pos < s.end }

func spansContain(spans []byteSpan, pos int) bool {
	for _, s := range spans {
		if s.contains(pos) {
			return true
		}
	}
	return false
}

// exprHardBoundary lists the bare keywords that can never appear INSIDE an
// expression at paren depth 0, so a backward scan for the left operand of an
// "IN ()" can stop at one and be sure it has the whole operand. Operators are
// deliberately absent: everything at IN's own precedence level is %left
// (parse.y:312, "IS MATCH LIKE_KW BETWEEN IN ISNULL NOTNULL NE EQ"), so
// "a=b IN ()" really does have "a=b" as its left operand, and everything below
// that line binds tighter still. AND/OR/NOT (parse.y:309-311) bind LOOSER and
// so DO terminate the operand -- but BETWEEN's own AND does not, and neither
// does a CASE's, so hitting one of those makes emptyInSpans report the span as
// unsure rather than guess.
var exprHardBoundary = map[string]bool{
	"SELECT": true, "DISTINCT": true, "ALL": true, "FROM": true, "WHERE": true,
	"GROUP": true, "HAVING": true, "ORDER": true, "LIMIT": true, "OFFSET": true,
	"BY": true, "VALUES": true, "SET": true, "ON": true, "USING": true,
	"RETURNING": true, "BEGIN": true, "UNION": true, "INTERSECT": true,
	"EXCEPT": true, "WHEN": true, "THEN": true, "ELSE": true, "CASE": true,
	"INTO": true, "JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true,
	"OUTER": true, "CROSS": true, "NATURAL": true, "WINDOW": true, "DO": true,
	"UPDATE": true, "INSERT": true, "DELETE": true, "WITH": true,
	"RECURSIVE": true, "AS": true,
}

// emptyInSpans classifies every "[NOT] IN ()" in sqlText by the byte range of
// its left operand: exempt spans are the ones parse.y unmaps (so the rename
// cascade must leave them byte-identical), unsure spans are the ones whose
// extent this text-level scan could not pin down exactly. A caller rewrites
// nothing inside an exempt span, and declines outright if what it is renaming
// appears inside an unsure one.
func emptyInSpans(sqlText string) (exempt, unsure []byteSpan) {
	toks, err := lex(sqlText)
	if err != nil {
		// An unlexable stored object: treat the whole text as unsure, which is
		// the same "cannot tell, so claim nothing" answer countIdentMentions
		// gives for the same input.
		return nil, []byteSpan{{0, len(sqlText)}}
	}
	for i, t := range toks {
		if !(t.kind == tkIdent && !t.quoted && t.upper() == "IN") {
			continue
		}
		if !(i+2 < len(toks) &&
			toks[i+1].kind == tkPunct && toks[i+1].text == "(" &&
			toks[i+2].kind == tkPunct && toks[i+2].text == ")") {
			continue
		}
		j := i - 1
		if j >= 0 && toks[j].kind == tkIdent && !toks[j].quoted && toks[j].upper() == "NOT" {
			j-- // in_op ::= NOT IN  (parse.y:1490)
		}
		if j < 0 {
			continue
		}
		start, certain := lhsOperandStart(toks, j)
		if start > j {
			continue // no left operand at all: not this production
		}
		span := byteSpan{toks[start].Start, toks[j].End}
		hasFunc, funcUnsure := operandFuncState(sqlText, span, toks[start:j+1])
		switch {
		case !certain || funcUnsure:
			unsure = append(unsure, span)
		case hasFunc:
			// EP_HasFunc: expr1 survives, so its tokens stay mapped.
		default:
			exempt = append(exempt, span)
		}
	}
	return exempt, unsure
}

// lhsOperandStart scans backwards from token j for the first token of the
// expression ending there, stopping at the nearest exprHardBoundary keyword,
// unmatched "(", depth-0 comma or ";", or the start of the text. certain is
// false when an AND/OR/NOT/END was passed at depth 0 on the way: the operand
// really does end at one of those in the simple case, but BETWEEN's AND and a
// CASE's END do not, and telling those apart needs the parse tree.
func lhsOperandStart(toks []token, j int) (start int, certain bool) {
	depth := 0
	certain = true
	for i := j; i >= 0; i-- {
		t := toks[i]
		if t.kind == tkPunct {
			switch t.text {
			case ")":
				depth++
			case "(":
				if depth == 0 {
					return i + 1, certain
				}
				depth--
			case ",", ";":
				if depth == 0 {
					return i + 1, certain
				}
			}
			continue
		}
		if depth > 0 || t.kind != tkIdent || t.quoted {
			continue
		}
		switch u := t.upper(); {
		case exprHardBoundary[u]:
			return i + 1, certain
		case u == "AND" || u == "OR" || u == "NOT" || u == "END":
			certain = false
		}
	}
	return 0, certain
}

// grammarLPKeyword lists the bare keywords that take a "(" from the GRAMMAR
// rather than by being a function name, so the paren after them is not a
// function call and does not set EP_HasFunc: TK_NOT, TK_IN, TK_EXISTS, TK_CASE
// and friends are all built by sqlite3PExpr/sqlite3ExprAlloc, and only
// sqlite3ExprFunction (expr.c:1191) sets the flag.
var grammarLPKeyword = map[string]bool{
	"NOT": true, "IN": true, "IS": true, "EXISTS": true, "AND": true,
	"OR": true, "BETWEEN": true, "COLLATE": true, "ESCAPE": true,
	"DISTINCT": true, "ALL": true, "CASE": true, "WHEN": true, "THEN": true,
	"ELSE": true, "SELECT": true, "VALUES": true, "FILTER": true, "OVER": true,
}

// ambiguousLPKeyword lists the keywords that are BOTH a grammar production
// taking "(" AND legal function names, because parse.y's "%fallback ID" list
// contains them (CAST, RAISE, LIKE_KW, MATCH). "cast(x)" is the cast()
// user function and "CAST(x AS t)" is the grammar production, and one token of
// lookahead cannot tell them apart -- so a span containing one is reported
// UNSURE and the caller declines rather than guessing which way EP_HasFunc
// went. Only the FALLBACK scan needs this: operandFuncState below normally
// answers off a real parse, which is never ambiguous.
var ambiguousLPKeyword = map[string]bool{
	"CAST": true, "RAISE": true, "LIKE": true, "GLOB": true,
	"REGEXP": true, "MATCH": true,
}

// operandFuncState answers EP_HasFunc for an empty IN()'s left operand by
// parsing it, since the flag belongs to the parse tree: "CAST(b AS INT)" is not
// a call while "cast(b)" is (likewise RAISE, LIKE, GLOB, REGEXP, MATCH), and
// COLLATE blocks propagation (sqlite3ExprAddCollateToken), so
// "abs(b) COLLATE nocase IN ()" is elided and must stay byte-identical. Falls
// back to spanFuncState when the span does not parse alone (e.g. RAISE outside
// a trigger). The span is token-aligned.
func operandFuncState(sqlText string, span byteSpan, seg []token) (hasFunc, unsure bool) {
	text := sqlText[span.start:span.end]
	if toks, err := lex(text); err == nil {
		p := newParser(text, toks)
		if e, perr := p.parseExpr(); perr == nil && p.peek().kind == tkEOF {
			return exprHasFuncCall(e), false
		}
	}
	return spanFuncState(seg)
}

// spanFuncState reports whether seg contains a function call outside any
// subquery (an identifier followed by "(", skipping "( SELECT|VALUES|WITH"
// groups), which is EP_HasFunc's reach (expr.c:1191, 850). unsure is set for an
// ambiguousLPKeyword, since a wrong guess either way is wrong; the caller
// declines.
func spanFuncState(seg []token) (hasFunc, unsure bool) {
	for i := 0; i < len(seg); i++ {
		t := seg[i]
		if t.kind == tkPunct && t.text == "(" {
			if !isSubqueryOpen(seg, i) {
				continue
			}
			depth := 0
			for ; i < len(seg); i++ {
				if seg[i].kind != tkPunct {
					continue
				}
				if seg[i].text == "(" {
					depth++
				} else if seg[i].text == ")" {
					if depth--; depth == 0 {
						break
					}
				}
			}
			continue
		}
		if t.kind != tkIdent || i+1 >= len(seg) ||
			seg[i+1].kind != tkPunct || seg[i+1].text != "(" {
			continue
		}
		if t.quoted {
			hasFunc = true // a quoted name is never a keyword production
			continue
		}
		switch u := t.upper(); {
		case grammarLPKeyword[u]:
		case ambiguousLPKeyword[u]:
			unsure = true
		default:
			hasFunc = true
		}
	}
	return hasFunc, unsure
}

// isSubqueryOpen reports whether the "(" at seg[i] opens a SELECT/VALUES/WITH
// subquery rather than a grouping or an argument list.
func isSubqueryOpen(seg []token, i int) bool {
	if i+1 >= len(seg) || seg[i+1].kind != tkIdent || seg[i+1].quoted {
		return false
	}
	switch seg[i+1].upper() {
	case "SELECT", "VALUES", "WITH":
		return true
	}
	return false
}

// emptyInBlocks reports whether renaming name inside sqlText cannot be done at
// all -- i.e. some token spelling name sits inside an UNSURE empty-IN() left
// operand, where this scan cannot say whether C SQLite would rewrite it.
func emptyInBlocks(sqlText, name string) bool {
	_, unsure := emptyInSpans(sqlText)
	if len(unsure) == 0 {
		return false
	}
	toks, err := lex(sqlText)
	if err != nil {
		return true
	}
	for _, t := range toks {
		if t.kind == tkIdent && equalFoldName(t.text, name) && spansContain(unsure, t.Start) {
			return true
		}
	}
	return false
}

// dropEmptyInEdits drops the edits that fall inside an exempt empty-IN() left
// operand of sqlText. The direct splice sites (a CHECK clause, an index's key
// list or partial WHERE, the table's own column list) need it exactly as much
// as rewriteIdentInSQL does: "CREATE TABLE t1(a, b, CHECK(b IN ()))" comes back
// from "ALTER TABLE t1 RENAME b TO bbb" as "CREATE TABLE t1(a, bbb, CHECK(b IN
// ()))" -- the DECLARATION renamed, the CHECK's reference left alone -- and
// "CREATE INDEX i1 ON t1(a) WHERE b IN ()" is left untouched entirely.
func dropEmptyInEdits(sqlText string, edits []textEdit) []textEdit {
	exempt, _ := emptyInSpans(sqlText)
	if len(exempt) == 0 {
		return edits
	}
	kept := edits[:0]
	for _, e := range edits {
		if !spansContain(exempt, e.start) {
			kept = append(kept, e)
		}
	}
	return kept
}

// checkViewRenameSafe returns every view referencing oldName (as
// viewReferencesTable), or an error if the RENAME TO cascade into them is
// unsafe: oldName inside an empty IN() operand whose extent is unclear, or a
// column elsewhere spelled like oldName (the same bail-out as
// renameTriggerReferences). applyViewRename applies them after the rest of the
// cascade succeeds.
//
// Catalog scoping is alterSkipsCatalog's, plus one rule: a view's FROM items match
// by resolved identity (renameTableSelectCb) and a TEMP view resolves TEMP
// first, so with a temp.t1 shadow, "ALTER TABLE main.t1 RENAME TO t9" leaves a
// temp view's "FROM t1" alone.
func (db *DB) checkViewRenameSafe(tbl *tableMeta, verb string) (affected []*viewMeta, err error) {
	oldName := tbl.name
	shadowed := !tbl.isTemp && db.findTableMetaIn(createScope(true), oldName) != nil
	for _, v := range db.views {
		if alterSkipsCatalog(tbl.isTemp, v.isTemp) || (shadowed && v.isTemp) {
			continue
		}
		if sqlMentionsIdent(v.sql, oldName) || len(nameStringTokens(v.sql, oldName)) > 0 {
			affected = append(affected, v)
		}
	}
	if len(affected) == 0 {
		return nil, nil
	}
	for _, v := range affected {
		if emptyInBlocks(v.sql, oldName) {
			return nil, fmt.Errorf("engine: unsupported: %s: renaming table %s, which view %s names inside the left operand of an empty IN() list whose extent this write path could not pin down (see this file's emptyInSpans)", verb, oldName, v.name)
		}
		// "CREATE VIEW v1 AS SELECT * FROM 't8'" -- a FROM item spelled as a
		// STRING is still an "nm" (parse.y:340) and renameTableSelectCb rewrites
		// pItem->zName's token whatever its spelling, so leaving it alone
		// orphaned the view: the oracle answers `FROM "t9"` where this engine
		// answered `FROM 't8'`. Unlike a trigger's ON clause there is no single
		// locatable position here (a view's FROM items are arbitrarily nested),
		// so this declines rather than guesses which strings are names.
		if len(nameStringTokens(v.sql, oldName)) > 0 {
			// ...unless the FROM-clause scan and the parser AGREE on the whole
			// FROM list, which is exactly when this engine knows which string
			// tokens are names: r45FlatViewNameStrings returns them, and
			// applyViewRename recomputes the identical set.
			if _, ok := r45FlatViewNameStrings(v.sql, oldName); !ok {
				return nil, fmt.Errorf("engine: unsupported: %s: renaming table %s, which view %s spells as a STRING literal where this write path cannot tell a name position from a value one (C SQLite resolves it and rewrites only the name)", verb, oldName, v.name)
			}
		}
	}
	for _, t := range db.tables {
		for _, c := range t.cols {
			if !equalFoldName(c.Name, oldName) {
				continue
			}
			// A same-named COLUMN only blocks the rewrite when this engine
			// cannot tell it from the table reference. For a FLAT view body it
			// can, and provably: see r45FlatViewColumnRefSkips, whose scan is
			// trusted only where it agrees with the parser's own From list.
			// applyViewRename recomputes the same spans.
			for _, av := range affected {
				if _, ok := r45FlatViewColumnRefSkips(av.sql, oldName); !ok {
					return nil, fmt.Errorf("engine: unsupported: %s: renaming table %s, which a view references while column %s.%s shares its name, is not supported by this write path (the view-body rewrite could not tell the two apart)", verb, oldName, t.name, c.Name)
				}
			}
		}
	}
	return affected, nil
}

// checkViewColumnRenameSafe is checkViewRenameSafe for RENAME COLUMN: a view is
// affected only when it mentions both tbl and oldName ("SELECT * FROM rr" is
// untouched, altermalloc2.test). The column-collision bail-out is scoped per
// view as in renameTriggerColumnReferences; the table/view-name ones stay
// schema-wide.
func (db *DB) checkViewColumnRenameSafe(tbl *tableMeta, oldName, newName, verb string) (affected []*viewMeta, err error) {
	shadowed := alterColumnShadowed(db, tbl)
	for _, v := range db.views {
		if alterSkipsCatalog(tbl.isTemp, v.isTemp) || (shadowed && v.isTemp) {
			continue
		}
		if sqlMentionsTableName(v.sql, tbl.name) && sqlMentionsIdent(v.sql, oldName) {
			affected = append(affected, v)
		}
	}
	if len(affected) == 0 {
		return nil, nil
	}
	// A TEMP table's renamed column named by a TEMP view is an error in C: the
	// post-rename re-parse (renameTestSchema, alter.c:676; alter.c:1170, 2050)
	// resolves the rewritten view against a temp schema not yet reloaded, giving
	// "error in view tv: no such column: z". Main table + temp view, main + main, a
	// view naming another column, or a temp trigger are all accepted.
	if tbl.isTemp {
		for _, v := range affected {
			if v.isTemp {
				// The message names the reference AS WRITTEN, which is what failed to
				// resolve: a view body saying "tt.y" reports "no such column: tt.z",
				// one saying "y" reports "z" (both verified against 3.53.3).
				return nil, fmt.Errorf("engine: error in view %s: no such column: %s",
					v.name, renamedColumnAsWritten(v.sql, tbl.name, oldName, newName))
			}
		}
	}
	for _, v := range affected {
		if emptyInBlocks(v.sql, oldName) {
			return nil, fmt.Errorf("engine: unsupported: %s: renaming %s.%s, which view %s names inside the left operand of an empty IN() list whose extent this write path could not pin down (see this file's emptyInSpans)", verb, tbl.name, oldName, v.name)
		}
	}
	// A collision only blocks the rewrite where this engine cannot tell the
	// two apart, and usually it can: a QUALIFIED reference names its FROM item
	// outright, and a BARE one belongs to whichever single FROM table carries
	// a column of that name. r45ViewColumnRenameSkips proves it per view, or
	// reports false -- a bare reference two FROM tables could both answer --
	// and the declines below stand. applyViewRename recomputes the same spans.
	provable := len(affected) > 0
	for _, av := range affected {
		if _, ok := r45ViewColumnRenameSkips(av.sql, tbl.name, oldName, db.tableHasColumn); !ok {
			provable = false
			break
		}
	}
	for _, t := range db.tables {
		if equalFoldName(t.name, oldName) && !provable {
			return nil, fmt.Errorf("engine: unsupported: %s: renaming %s.%s, which a view references while table %s shares its name, is not supported by this write path (the view-body rewrite could not tell the two apart)", verb, tbl.name, oldName, t.name)
		}
		if t == tbl {
			continue
		}
		for _, c := range t.cols {
			if !equalFoldName(c.Name, oldName) {
				continue
			}
			// A same-named column in another table makes a view ambiguous only if that table
			// is reachable from the view (viewTransitivelyMentions), as C binds each
			// reference by resolution (alter.c:1015).
			for _, av := range affected {
				if provable {
					continue
				}
				if viewTransitivelyMentions(db, av, t.name, map[string]bool{}) {
					return nil, fmt.Errorf("engine: unsupported: %s: renaming %s.%s, which view %s references while column %s.%s shares its name, is not supported by this write path (the view-body rewrite could not tell the two apart)", verb, tbl.name, oldName, av.name, t.name, c.Name)
				}
			}
		}
	}
	for _, v := range db.views {
		if equalFoldName(v.name, oldName) && !provable {
			return nil, fmt.Errorf("engine: unsupported: %s: renaming %s.%s, which a view references while view %s shares its name, is not supported by this write path (the view-body rewrite could not tell the two apart)", verb, tbl.name, oldName, v.name)
		}
	}
	return affected, nil
}

// applyViewRename rewrites oldName to repl in each affected view's SQL and
// re-parses it, committing nothing until all succeed (as applyTriggerRename).
// nil affected is a no-op. ownerTable ("" for a table rename) and hasCol feed
// r45ViewColumnRenameSkips.
func applyViewRename(affected []*viewMeta, oldName string, repl identRepl, verb string, ownerTable string, hasCol func(table, col string) bool) error {
	rewritten := make([]*viewMeta, len(affected))
	for i, v := range affected {
		// A view's DECLARED column list is Table.pCheck, which the view branch
		// of renameColumnFunc never walks -- see r33rSkips.spans.
		var skips r33rSkips
		if span, ok := r33rViewColumnListSpan(v.sql); ok {
			skips.spans = []byteSpan{span}
		}
		// ...and, for a TABLE rename, every bare COLUMN reference that merely
		// spells the table's name. checkViewRenameSafe has already proved
		// these are recoverable for every affected view, or declined; this
		// recomputes the identical spans rather than threading them through.
		var extra []token
		if repl.isTable {
			if colRefs, ok := r45FlatViewColumnRefSkips(v.sql, oldName); ok {
				skips.spans = append(skips.spans, colRefs...)
			}
			// A FROM item spelled as a STRING is still an "nm" (parse.y:340)
			// and renameTableSelectCb rewrites its token whatever the spelling
			// -- rewriteIdentInSQL sees no tkString at all, so those tokens are
			// handed to it directly. checkViewRenameSafe has already proved
			// this set is knowable for every affected view, or declined.
			if strs, ok := r45FlatViewNameStrings(v.sql, oldName); ok {
				extra = append(extra, strs...)
			}
		} else if tableRefs, ok := r45ViewColumnRenameSkips(v.sql, ownerTable, oldName, hasCol); ok {
			// A COLUMN rename over a single-table view body -- see
			// r45ViewColumnRenameSkips. The caller has already proved
			// these are recoverable for every affected view, or declined.
			skips.spans = append(skips.spans, tableRefs...)
		}
		newSQL, rerr := rewriteIdentInSQL(v.sql, oldName, repl, skips, extra...)
		if rerr != nil {
			return rerr
		}
		stmt, perr := parseCreateViewStmt(newSQL)
		if perr != nil {
			return fmt.Errorf("engine: %s: rewriting view %s: %w", verb, v.name, perr)
		}
		// C SQLite re-resolves the rewritten view and fails the whole ALTER
		// if the rewrite broke it -- see viewRenameAmbiguity for the measured
		// case and for why this test is deliberately coarser than SQLite's.
		if dup := viewRenameAmbiguity(stmt.selectStmt); dup != "" {
			return fmt.Errorf("engine: %s: error in view %s after rename: ambiguous column name: %s", verb, v.name, dup)
		}
		nv := *v
		nv.sql = normalizeSchemaSQL("VIEW", newSQL)
		nv.selectStmt = stmt.selectStmt
		rewritten[i] = &nv
	}
	for i, v := range affected {
		*v = *rewritten[i]
	}
	return nil
}

// ---- RENAME COLUMN ----

// renameColumn implements "ALTER TABLE t RENAME [COLUMN] old TO new": renames the
// column, rewrites tbl's stored CREATE TABLE text (the declaration and any
// table-level constraint naming it), and every index, trigger and view that
// references it. newQuoted is C's bQuote (alter.c:656): the new name was quoted,
// so every rewritten occurrence is double-quoted (renameColumnRepl).
func (db *DB) renameColumn(tbl *tableMeta, oldName, newName string, newQuoted bool) error {
	// C SQLite re-parses the WHOLE schema for this ALTER form and fails if
	// any trigger or view is left dangling by an earlier DROP -- see
	// checkSchemaObjectsResolve. Checked FIRST, so a failure changes nothing.
	if err := db.checkSchemaObjectsResolve(tbl); err != nil {
		return err
	}
	colIdx := -1
	for i, c := range tbl.cols {
		if equalFoldName(c.Name, oldName) {
			colIdx = i
			break
		}
	}
	if colIdx < 0 {
		return fmt.Errorf("engine: no such column: %q", oldName)
	}
	// Skip the renamed column itself: C has no duplicate pre-check
	// (alter.c:599); "duplicate column name" comes only from re-parsing the
	// rewritten text (alter.c:676), and "RENAME c TO c" (or a case-only rename)
	// leaves it valid (altertab3.test). Any other column named newName still
	// collides.
	for i, c := range tbl.cols {
		if i != colIdx && equalFoldName(c.Name, newName) {
			return fmt.Errorf("engine: error in table %s after rename: duplicate column name: %s", tbl.name, newName)
		}
	}
	// See checkViewColumnRenameSafe's doc comment: unlike the old blanket "any
	// view mentioning this table declines" rule, this only declines when a
	// rewrite genuinely cannot be done safely; affectedViews is applied by
	// applyViewRename once the rest of the cascade below has succeeded.
	affectedViews, verr := db.checkViewColumnRenameSafe(tbl, oldName, newName, "ALTER TABLE RENAME COLUMN")
	if verr != nil {
		return verr
	}
	// The table's OWN text and its indexes' get the empty-IN() exemption too --
	// a CHECK or a partial WHERE is an expression like any other. Checked here,
	// before anything is spliced, so a decline changes nothing.
	if emptyInBlocks(tbl.sql, oldName) {
		return fmt.Errorf("engine: unsupported: ALTER TABLE RENAME COLUMN: %s.%s appears inside the left operand of an empty IN() list whose extent this write path could not pin down (see this file's emptyInSpans)", tbl.name, oldName)
	}
	for _, ix := range db.indexes {
		if equalFoldName(ix.table, tbl.name) && ix.sql != "" && emptyInBlocks(ix.sql, oldName) {
			return fmt.Errorf("engine: unsupported: ALTER TABLE RENAME COLUMN: index %s names %s inside the left operand of an empty IN() list whose extent this write path could not pin down (see this file's emptyInSpans)", ix.name, oldName)
		}
	}

	// renameFixQuotes (alter.c:646) runs HERE: after the schema has been proved
	// to still parse and before a single byte of the rename is spliced. The
	// ordering is observable -- alterqf.test 2.1 renames two TO 'four' over an
	// index reading `one+"two"+"four"`, and the answer is `one+"four"+'four'`:
	// "four" was quotefixed while no column of that name existed yet, and only
	// then did "two" become "four".
	quoteUndo, qerr := db.r32nRenameFixQuotes(tbl, "after rename")
	if qerr != nil {
		return qerr
	}

	list, err := parseTblColumnList(tbl.sql)
	if err != nil {
		quoteUndo.restore()
		return fmt.Errorf("engine: ALTER TABLE RENAME COLUMN: %w", err)
	}
	repl := renameColumnRepl(newName, newQuoted)
	var edits []textEdit
	foundOwn := false
	for _, seg := range list.segments {
		if len(seg) == 0 {
			continue
		}
		if isTableConstraintSegment(seg) {
			for _, idTok := range identTokensInParens(seg) {
				if equalFoldName(idTok.text, oldName) {
					edits = append(edits, textEdit{idTok.Start, idTok.End, repl.at(tbl.sql, idTok)})
				}
			}
			continue
		}
		if columnDefSegNameMatches(seg, oldName) {
			edits = append(edits, textEdit{seg[0].Start, seg[0].End, repl.at(tbl.sql, seg[0])})
			foundOwn = true
		}
		// A column-level segment's OWN inline CHECK(...) clause(s) -- be it
		// the renamed column's own self-reference or another column's
		// cross-reference to it -- need the same rename applied inside
		// their parenthesized body (see checkClauseIdentTokens' doc
		// comment); the table-level branch above already gets this for
		// free via identTokensInParens' generic first-paren-group scan.
		for _, idTok := range checkClauseIdentTokens(seg) {
			if equalFoldName(idTok.text, oldName) {
				edits = append(edits, textEdit{idTok.Start, idTok.End, repl.at(tbl.sql, idTok)})
			}
		}
		// ...and a GENERATED column's body, which alter.c:1616-1622 walks for
		// every column right after it walks the CHECKs.
		for _, idTok := range generatedClauseIdentTokens(seg) {
			if equalFoldName(idTok.text, oldName) {
				edits = append(edits, textEdit{idTok.Start, idTok.End, repl.at(tbl.sql, idTok)})
			}
		}
	}
	if !foundOwn {
		quoteUndo.restore()
		return fmt.Errorf("engine: internal error: ALTER TABLE RENAME COLUMN: could not locate column %s in stored schema text", oldName)
	}
	edits = dropEmptyInEdits(tbl.sql, edits)
	savedSQL := tbl.sql
	savedIndexSQL := make([]string, len(db.indexes))
	savedIndexCols := make([][]string, len(db.indexes))
	// keys/where are the PARSED half of an expression/partial index, re-derived
	// from the rewritten text below (refreshExprIndex); a declined cascade has
	// to put them back too.
	savedIndexKeys := make([][]indexKey, len(db.indexes))
	savedIndexWhere := make([]Expr, len(db.indexes))
	for i, ix := range db.indexes {
		savedIndexSQL[i] = ix.sql
		savedIndexCols[i] = append([]string(nil), ix.cols...)
		savedIndexKeys[i], savedIndexWhere[i] = ix.keys, ix.where
	}
	savedGenerated := make([]string, len(tbl.cols))
	savedGeneratedOff := make([]int, len(tbl.cols))
	for i, c := range tbl.cols {
		savedGenerated[i], savedGeneratedOff[i] = c.GeneratedExpr, c.R32NGeneratedOff
	}
	tbl.sql = applyEdits(tbl.sql, edits)
	tbl.cols[colIdx].Name = newName
	if err := refreshTableChecks(tbl); err != nil {
		tbl.sql = savedSQL
		tbl.cols[colIdx].Name = oldName
		quoteUndo.restore()
		return fmt.Errorf("engine: ALTER TABLE RENAME COLUMN: %w", err)
	}
	// ...and the generated-column BODIES the edits above just rewrote, which
	// are stored as TEXT (columnInfo.GeneratedExpr) and compiled from it. The
	// rename moves no column, so genProg's positions stay valid, but the body
	// it was compiled from now names a column that no longer exists.
	if err := refreshGeneratedBodies(tbl); err != nil {
		tbl.sql = savedSQL
		tbl.cols[colIdx].Name = oldName
		for i := range tbl.cols {
			tbl.cols[i].GeneratedExpr, tbl.cols[i].R32NGeneratedOff = savedGenerated[i], savedGeneratedOff[i]
		}
		refreshRowPrograms(tbl)
		quoteUndo.restore()
		return fmt.Errorf("engine: ALTER TABLE RENAME COLUMN: %w", err)
	}

	// idxUndo restores every index's pre-rename state. The loop below sets
	// ix.cols[k] before it knows whether the matching text edit can be built,
	// so a mid-loop decline needs the same restore the rollback closure further
	// down performs for the cascade's own declines.
	idxUndo := func() {
		for i, ix := range db.indexes {
			ix.sql = savedIndexSQL[i]
			copy(ix.cols, savedIndexCols[i])
			ix.keys, ix.where = savedIndexKeys[i], savedIndexWhere[i]
		}
	}

	for _, ix := range db.indexes {
		if !equalFoldName(ix.table, tbl.name) {
			continue
		}
		var ixEdits []textEdit
		for k, cn := range ix.cols {
			if !equalFoldName(cn, oldName) {
				continue
			}
			ix.cols[k] = newName
			// The by-KEY-POSITION splice is right only for a plain
			// column-keyed index, whose cols[] IS its key list. An
			// expression/partial index's cols[] is the referenced-column SET
			// in table order, so it is rewritten by TOKEN below instead --
			// see indexColumnRefTokens for what the position splice did to
			// one otherwise.
			if ix.sql != "" && !ix.exprOrPartial {
				tok, terr := locateIndexColumnToken(ix.sql, k)
				if terr != nil {
					// Not swallowed: leaving the index text on the old column name would persist a
					// schema OpenWrite refuses. Decline the whole ALTER instead.
					idxUndo()
					tbl.sql = savedSQL
					tbl.cols[colIdx].Name = oldName
					if rerr := refreshTableChecks(tbl); rerr != nil {
						quoteUndo.restore()
						return fmt.Errorf("engine: ALTER TABLE RENAME COLUMN: %w", rerr)
					}
					quoteUndo.restore()
					return fmt.Errorf("engine: ALTER TABLE RENAME COLUMN: rewriting index %s: %w", ix.name, terr)
				}
				ixEdits = append(ixEdits, textEdit{tok.Start, tok.End, repl.at(ix.sql, tok)})
			}
		}
		if ix.sql != "" && ix.exprOrPartial {
			// Both halves at once: a key EXPRESSION's own column references
			// and a partial WHERE's. C SQLite rewrites both -- verified,
			// "CREATE INDEX ip ON t(a) WHERE b>1" becomes "... WHERE bb>1"
			// after "ALTER TABLE t RENAME COLUMN b TO bb", with the new name
			// rendered unquoted exactly as renderIdent renders it.
			for _, idTok := range indexColumnRefTokens(ix.sql) {
				if equalFoldName(idTok.text, oldName) {
					ixEdits = append(ixEdits, textEdit{idTok.Start, idTok.End, repl.at(ix.sql, idTok)})
				}
			}
		}
		if ixEdits = dropEmptyInEdits(ix.sql, ixEdits); len(ixEdits) > 0 {
			ix.sql = applyEdits(ix.sql, ixEdits)
		}
	}

	// See renameTableTo's identical comment: applyViewRename runs AFTER the
	// trigger cascade, so an already-applied trigger rewrite must be
	// undoable too if the view cascade is the one that ultimately declines.
	savedTriggers := make(map[*triggerMeta]triggerMeta, len(db.triggers))
	for _, tr := range db.triggers {
		savedTriggers[tr] = *tr
	}
	// Every CHILD table's stored text is about to be rewritten too (its
	// "REFERENCES <this table>(<cols>)" list), so it joins the undo set for
	// exactly the reason the triggers above did.
	savedChildSQL := make(map[*tableMeta]string, len(db.tables))
	for _, t := range db.tables {
		savedChildSQL[t] = t.sql
	}
	rollback := func() error {
		// A declined cascade must change nothing.
		for t, s := range savedChildSQL {
			t.sql = s
		}
		tbl.sql = savedSQL
		tbl.cols[colIdx].Name = oldName
		if rerr := refreshTableChecks(tbl); rerr != nil {
			return rerr
		}
		for i, ix := range db.indexes {
			ix.sql = savedIndexSQL[i]
			copy(ix.cols, savedIndexCols[i])
			ix.keys, ix.where = savedIndexKeys[i], savedIndexWhere[i]
		}
		for tr, snap := range savedTriggers {
			*tr = snap
		}
		// ... and so must the quotefix pass that ran before any of it.
		quoteUndo.restore()
		return nil
	}

	// renameColumnFunc's FKey branch (alter.c:1626): a CHILD's
	// "REFERENCES <parent>(<cols>)" list names the PARENT's columns and is
	// rewritten when one of them is the column being renamed.
	db.r34vRenameForeignKeyParentColumns(tbl, oldName, repl)

	if terr := db.renameTriggerColumnReferences(tbl, oldName, repl); terr != nil {
		if rerr := rollback(); rerr != nil {
			return rerr
		}
		return terr
	}
	oldViewSQL := make(map[*viewMeta]string, len(affectedViews))
	for _, v := range affectedViews {
		oldViewSQL[v] = v.sql
	}
	if terr := applyViewRename(affectedViews, oldName, repl, "ALTER TABLE RENAME COLUMN", tbl.name, db.tableHasColumn); terr != nil {
		if rerr := rollback(); rerr != nil {
			return rerr
		}
		return terr
	}
	if tbl.isTemp {
		if terr := db.tempRenameColumnSecondPass(savedTriggers, oldViewSQL); terr != nil {
			if rerr := rollback(); rerr != nil {
				return rerr
			}
			return terr
		}
	}
	// The stored index text just changed, so the PARSED half derived from it
	// must be re-derived -- see refreshExprIndex, and renameTableTo's identical
	// pass.
	for _, ix := range db.indexes {
		if ix.isTemp != tbl.isTemp || !equalFoldName(ix.table, tbl.name) {
			continue
		}
		if ferr := refreshExprIndex(ix, tbl); ferr != nil {
			if rerr := rollback(); rerr != nil {
				return rerr
			}
			return ferr
		}
	}
	// renameTestSchema(...,"after rename",1) (alter.c:676) -- the VIEW-trigger
	// half of it, which only the REWRITTEN schema can answer. See
	// r33rTriggersResolveAfter.
	if verr := db.r33rTriggersResolveAfter(tbl, "after rename"); verr != nil {
		if rerr := rollback(); rerr != nil {
			return rerr
		}
		return verr
	}

	db.bumpSchema(tbl.isTemp)
	return nil
}

// tempRenameColumnSecondPass is the error C gives a TEMP table's RENAME COLUMN
// when a temp view or trigger names the column: after rewriting the table's own
// schema (alter.c:657-665, which already covers temp objects), C runs the
// temp-schema pass meant for main tables (alter.c:667-672), which re-resolves
// the renamed text against the not-yet-reloaded schema and fails on the first
// object ("error in view tv: no such column: z"). Objects go in creation order.
// ponytail: the column is named by the first byte the rename changed, which
// matched C's first failure in every probed shape; a statement resolved out of
// text order could name another reference.
func (db *DB) tempRenameColumnSecondPass(savedTriggers map[*triggerMeta]triggerMeta, oldViewSQL map[*viewMeta]string) error {
	type changed struct {
		seq            uint64
		kind, name     string
		oldSQL, newSQL string
	}
	var hits []changed
	for tr, snap := range savedTriggers {
		if tr.isTemp && tr.sql != snap.sql {
			hits = append(hits, changed{tr.schemaSeq, "trigger", tr.name, snap.sql, tr.sql})
		}
	}
	for v, old := range oldViewSQL {
		if v.isTemp && v.sql != old {
			hits = append(hits, changed{v.schemaSeq, "view", v.name, old, v.sql})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	first := slices.MinFunc(hits, func(a, b changed) int { return cmp.Compare(a.seq, b.seq) })
	return fmt.Errorf("engine: error in %s %s: no such column: %s", first.kind, first.name, firstRenamedRef(first.oldSQL, first.newSQL))
}

// firstRenamedRef is the column reference, qualifier included, at the first
// byte where newSQL differs from oldSQL.
func firstRenamedRef(oldSQL, newSQL string) string {
	i := 0
	for i < len(oldSQL) && i < len(newSQL) && oldSQL[i] == newSQL[i] {
		i++
	}
	start := i
	for start > 0 && (isIdentByte(newSQL[start-1]) || newSQL[start-1] == '.') {
		start--
	}
	end := i
	for end < len(newSQL) && isIdentByte(newSQL[end]) {
		end++
	}
	return strings.Trim(newSQL[start:end], `"`+"`[]")
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '"' || b >= 0x80 || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// refreshTableChecks re-derives tbl.checks from tbl.sql after renameColumn or
// dropColumn has spliced it, through the same pipeline CreateTable uses, so the
// checks always match the stored text.
func refreshTableChecks(tbl *tableMeta) error {
	_, checks, _, err := parseCreateTableColumnsAndAutoIndexes(tbl.sql, tbl.withoutRowid)
	if err != nil {
		return fmt.Errorf("internal error: re-parsing %s after ALTER TABLE: %w", tbl.name, err)
	}
	checks, err = finalizeCheckConstraints(tbl.name, tbl.cols, checks, tbl.withoutRowid)
	if err != nil {
		return fmt.Errorf("internal error: re-validating %s's CHECK constraints after ALTER TABLE: %w", tbl.name, err)
	}
	tbl.checks = checks
	return nil
}

// refreshGeneratedBodies re-derives every generated column's stored body text
// from tbl.sql and re-stamps the programs compiled from it. RENAME COLUMN is
// its one caller: the body text really is rewritten now (see
// generatedClauseIdentTokens), so the compiled program has to follow or the
// column reads NULL through a reference to a name that is gone.
func refreshGeneratedBodies(tbl *tableMeta) error {
	cols, _, _, err := parseCreateTableColumnsAndAutoIndexes(tbl.sql, tbl.withoutRowid)
	if err != nil {
		return fmt.Errorf("internal error: re-parsing %s after RENAME COLUMN: %w", tbl.name, err)
	}
	if len(cols) != len(tbl.cols) {
		return fmt.Errorf("internal error: re-parsing %s after RENAME COLUMN gave %d columns, not %d", tbl.name, len(cols), len(tbl.cols))
	}
	for i := range tbl.cols {
		tbl.cols[i].GeneratedExpr = cols[i].GeneratedExpr
		tbl.cols[i].R32NGeneratedOff = cols[i].R32NGeneratedOff
	}
	refreshRowPrograms(tbl)
	return nil
}

// refreshRowPrograms re-stamps every compiled row expression on tbl's column
// list (generated columns' genProg, CHECKs' prog) against the list as it is now.
// It cannot fail; an expression that no longer compiles loses its program and
// eval recompiles on demand.
//
// Those programs address columns by position (as C's iSelfTab block does,
// expr.c:5047), and most paths re-derive the column list together with them.
// The exceptions splice the live list:
//
//	addColumn   tbl.cols = append(tbl.cols, col)     -- the list gets wider
//	dropColumn  tbl.cols = append(tbl.cols[:i:i], tbl.cols[i+1:]...)
//	                                                 -- everything after i
//	                                                    shifts down one
//
// and "DROP COLUMN a; ADD COLUMN z" keeps the width with everything moved. RENAME
// COLUMN reaches this via refreshGeneratedBodies: positions stay valid but body
// text changes. cloneTableMeta (txn.go) calls it because a snapshot's column
// slice is a new list by identity.
func refreshRowPrograms(tbl *tableMeta) {
	compileGeneratedColumns(tbl.name, tbl.cols)
	if len(tbl.checks) == 0 {
		return
	}
	scope := checkProgramScope(tbl.name, tbl.cols, tbl.withoutRowid)
	for i := range tbl.checks {
		tbl.checks[i].prog = compileSelfRowExpr(scope, tbl.checks[i].expr, true /* NC_IsCheck */, pureCtxCheck)
	}
}

// addColumnCheckTable is the throwaway tableMeta ADD COLUMN validates its
// back-fill against: tbl's columns plus the new one, with the full re-derived
// CHECK list, re-stamped so the back-fill loop compiles each CHECK once
// (TestAddColumnCheckTableIsProgramCurrent).
func addColumnCheckTable(tbl *tableMeta, col columnInfo, checks []checkConstraint) *tableMeta {
	tmp := *tbl
	tmp.cols = append(append([]columnInfo(nil), tbl.cols...), col)
	tmp.checks = checks
	refreshRowPrograms(&tmp)
	return &tmp
}

// refreshExprIndex re-derives an expression/partial index's key list and WHERE
// from its rewritten SQL, as refreshTableChecks does for CHECKs: a WHERE
// qualifier ("t3.b>1") names the new table after RENAME TO. Same derivation as
// OpenWrite's (demoteDoubleQuotedIndexColumns + buildExprIndexMeta). A plain
// column index is left alone.
func refreshExprIndex(ix *indexMeta, tbl *tableMeta) error {
	if !ix.exprOrPartial || ix.sql == "" {
		return nil
	}
	stmt, err := parseCreateIndexStmt(ix.sql)
	if err != nil {
		return fmt.Errorf("internal error: re-parsing index %s after ALTER TABLE: %w", ix.name, err)
	}
	demoteDoubleQuotedIndexColumns(stmt, tbl)
	im, err := buildExprIndexMeta(ix.name, tbl, stmt, ix.sql)
	if err != nil {
		return fmt.Errorf("internal error: re-validating index %s after ALTER TABLE: %w", ix.name, err)
	}
	ix.keys, ix.where = im.keys, im.where
	return nil
}

// ---- ADD COLUMN ----

// addColumnConstraintStart mirrors sql_parser.go's identical (but
// function-local, hence not reusable from here) colConstraintStart set:
// every keyword that ends a column definition's own type-name token run and
// begins its constraint clauses.
var addColumnConstraintStart = map[string]bool{
	"PRIMARY": true, "NOT": true, "UNIQUE": true, "CHECK": true,
	"DEFAULT": true, "COLLATE": true, "REFERENCES": true, "GENERATED": true, "AS": true,
}

// addColumnDef is parseAddColumnDef's result: everything addColumn needs to
// validate and apply a single "ADD [COLUMN] coldef" clause.
type addColumnDef struct {
	name     string
	declType string
	aff      affinity

	isPK     bool
	isUnique bool
	notNull  bool
	hasCheck bool
	// hasReferences records a REFERENCES clause on the added column. With foreign
	// keys enabled, C refuses ADD COLUMN with a non-NULL DEFAULT and REFERENCES
	// ("Cannot add a REFERENCES column with non-NULL default value") over a table
	// with rows; with them off it succeeds, and DEFAULT NULL is always fine.
	hasReferences bool
	hasGenerated  bool
	// generatedStored is the STORED spelling of a generated column, which is
	// the only one alter.c treats differently: sqlite3AlterFinishAddColumn
	// refuses it with "cannot add a STORED column", but through
	// sqlite3ErrorIfNotEmpty, so an EMPTY table takes it.
	generatedStored bool
	hasCollate      bool
	collateName   string

	hasDefault           bool
	defaultIsBareLiteral bool // see parseAddColumnDefaultValue
	defaultValue         Value

	rawStart int    // byte offset (into the ORIGINAL ALTER TABLE text) of the coldef's first token -- see rawText
	rawText  string // verbatim source text of the coldef, filled in by AlterTable once parsing finishes (needs the position of the LAST consumed token, which parseAddColumnDef alone doesn't have easy access to)
}

// parseAddColumnDefaultValue parses the expression after DEFAULT in ADD COLUMN.
// ok is true for a value computable once to back-fill rows and serve as
// columnInfo.DefaultValue: NULL, TRUE, FALSE, a signed number, a string, a blob,
// or a parenthesized expression sqlite3ValueFromExpr can fold. Anything else
// (CURRENT_TIME etc.) is consumed with ok=false; addColumn accepts it over an
// empty table and refuses it over a non-empty one, as C does.
func parseAddColumnDefaultValue(p *parser) (val Value, ok bool, err error) {
	t := p.peek()
	switch {
	case t.kind == tkIdent && t.upper() == "NULL":
		p.next()
		return Value{Typ: Null}, true, nil
	case t.kind == tkIdent && t.upper() == "TRUE":
		p.next()
		return Value{Typ: Int, I: 1}, true, nil
	case t.kind == tkIdent && t.upper() == "FALSE":
		p.next()
		return Value{Typ: Int, I: 0}, true, nil
	case t.kind == tkIdent && (t.upper() == "CURRENT_TIME" || t.upper() == "CURRENT_DATE" || t.upper() == "CURRENT_TIMESTAMP"):
		p.next()
		return Value{}, false, nil
	case t.kind == tkPunct && (t.text == "+" || t.text == "-"):
		sign := t.text
		p.next()
		termTok := p.peek()
		switch termTok.kind {
		case tkNumber:
			p.next()
			v, nerr := literalNumber(sign + termTok.text)
			if nerr != nil {
				return Value{}, false, nerr
			}
			return v, true, nil
		case tkString, tkBlob:
			// "DEFAULT PLUS|MINUS term" takes any term: "DEFAULT -'hello'" folds to 0
			// (vdbemem.c:1895), stored as '0' under TEXT affinity (tkt-8454a207b9.test 3);
			// unary + is stripped (vdbemem.c:1811). Folded with foldDefaultValue so it
			// matches parseColumnDefault's reading of the stored text after a reopen.
			p.next()
			e, perr := newParser(p.src, []token{t, termTok, {kind: tkEOF}}).parseExpr()
			if perr != nil {
				return Value{}, false, nil
			}
			v, ok := foldDefaultValue(e)
			return v, ok, nil
		default:
			return Value{}, false, fmt.Errorf("engine: ALTER TABLE ADD COLUMN: expected a number after %q in DEFAULT", sign)
		}
	case t.kind == tkNumber:
		p.next()
		v, nerr := literalNumber(t.text)
		if nerr != nil {
			return Value{}, false, nerr
		}
		return v, true, nil
	case t.kind == tkString:
		p.next()
		return Value{Typ: Text, S: []byte(t.str)}, true, nil
	case t.kind == tkBlob:
		p.next()
		return Value{Typ: Blob, S: t.blob}, true, nil
	case t.kind == tkPunct && t.text == "(":
		// A parenthesized default is constant exactly when sqlite3ValueFromExpr can fold
		// it (alter.c:389, 395): a shape test, so "(1+2)" is refused while
		// "(CAST('7' AS INTEGER))" is accepted (see constantDefaultValue). Over a
		// non-empty table the folded value back-fills (alter4.test 9.3).
		first := p.pos
		if err := skipParenGroup(p); err != nil {
			return Value{}, false, err
		}
		group := append(append([]token(nil), p.toks[first:p.pos]...), token{kind: tkEOF})
		e, perr := newParser(p.src, group).parseExpr()
		if perr != nil {
			return Value{}, false, nil
		}
		v, ok := constantDefaultValue(e, UTF8)
		if !ok {
			return Value{}, false, nil
		}
		return v, true, nil
	default:
		return Value{}, false, fmt.Errorf("engine: ALTER TABLE ADD COLUMN: unexpected DEFAULT value near %q", p.tokenDesc(t))
	}
}

// parseAddColumnDef parses the coldef after "ADD [COLUMN]" through the end of the
// statement, recognizing every constraint clause parseCreateTableColumnsAndAutoIndexes does,
// so addColumn can decline a clause it does not support with its own message
// rather than a parse error.
func parseAddColumnDef(p *parser) (*addColumnDef, error) {
	nameTok := p.peek()
	if nameTok.kind != tkIdent {
		return nil, fmt.Errorf("engine: ALTER TABLE ADD COLUMN: expected column name, got %q", p.tokenDesc(nameTok))
	}
	p.next()
	def := &addColumnDef{name: nameTok.text, rawStart: nameTok.Start}

	var typeToks []token
	for {
		t := p.peek()
		if t.kind == tkEOF || (t.kind == tkPunct && t.text == ";") {
			break
		}
		if t.kind == tkIdent && addColumnConstraintStart[t.upper()] {
			break
		}
		typeToks = append(typeToks, t)
		p.next()
	}
	def.declType = joinTypeTokens(typeToks)
	def.aff = typeAffinity(def.declType)

	for {
		t := p.peek()
		if t.kind == tkEOF || (t.kind == tkPunct && t.text == ";") {
			break
		}
		if t.kind != tkIdent {
			return nil, fmt.Errorf("engine: ALTER TABLE ADD COLUMN: unexpected token %q", p.tokenDesc(t))
		}
		switch t.upper() {
		case "GENERATED":
			// "GENERATED ALWAYS AS (...)": the prefix is optional in SQLite's
			// own grammar (parse.y's ccons), so ALWAYS is consumed here and
			// the AS that follows is handled by the next iteration.
			def.hasGenerated = true
			p.next()
			p.consumeKeyword("ALWAYS")
		case "AS":
			def.hasGenerated = true
			p.next()
			if err := skipParenGroup(p); err != nil {
				return nil, err
			}
			// "[STORED|VIRTUAL]", default VIRTUAL. Only STORED is a rule of
			// its own here -- see addColumn's "cannot add a STORED column".
			if p.consumeKeyword("STORED") {
				def.generatedStored = true
			} else {
				p.consumeKeyword("VIRTUAL")
			}
		case "PRIMARY":
			def.isPK = true
			p.next()
			p.consumeKeyword("KEY")
			p.consumeKeyword("ASC")
			p.consumeKeyword("DESC")
		case "UNIQUE":
			def.isUnique = true
			p.next()
		case "NOT":
			p.next()
			if !p.consumeKeyword("NULL") {
				return nil, fmt.Errorf("engine: ALTER TABLE ADD COLUMN: expected NULL after NOT")
			}
			def.notNull = true
		case "CHECK":
			def.hasCheck = true
			p.next()
			if err := skipParenGroup(p); err != nil {
				return nil, err
			}
		case "REFERENCES":
			def.hasReferences = true
			p.next()
			if p.peek().kind == tkIdent {
				p.next()
			}
			if p.peekIsPunct("(") {
				if err := skipParenGroup(p); err != nil {
					return nil, err
				}
			}
			for {
				pt := p.peek()
				if pt.kind == tkEOF || (pt.kind == tkPunct && pt.text == ";") {
					break
				}
				if pt.kind == tkIdent && addColumnConstraintStart[pt.upper()] {
					break
				}
				p.next()
			}
		case "COLLATE":
			p.next()
			ct := p.peek()
			def.hasCollate = true
			if ct.kind == tkString {
				def.collateName = ct.str
			} else {
				def.collateName = ct.text
			}
			p.next()
		case "DEFAULT":
			p.next()
			def.hasDefault = true
			val, isLit, derr := parseAddColumnDefaultValue(p)
			if derr != nil {
				return nil, derr
			}
			def.defaultIsBareLiteral = isLit
			def.defaultValue = val
		default:
			return nil, fmt.Errorf("engine: ALTER TABLE ADD COLUMN: unexpected token %q", p.tokenDesc(t))
		}
	}
	return def, nil
}

// addColumnSpliceOffset returns the offset in a table's stored CREATE TABLE
// where ADD COLUMN inserts the new definition: right after the last column and
// before any table constraint, or at the closing paren. "CREATE TABLE q3(a, b,
// c VARCHAR(9), UNIQUE(a,b))" + "ADD COLUMN zz INT" gives "...c VARCHAR(9), zz
// INT, UNIQUE(a,b))". It is C's addColOffset (sqliteInt.h:2454), recomputed from
// tbl.sql (see addColumnUnderWritableSchemaEdit).
func addColumnSpliceOffset(sqlText string) (int, bool) {
	list, err := parseTblColumnList(sqlText)
	if err != nil {
		return 0, false
	}
	at := list.closeParen.Start
	if ci := list.firstTableConstraintSegment(); ci > 0 {
		prev := list.segments[ci-1]
		at = prev[len(prev)-1].End
	}
	return at, true
}

// addColumnUnderWritableSchemaEdit implements ADD COLUMN when t's sqlite_schema
// row has an outstanding, sql-only writable_schema edit (wsEditForObjectName,
// wsEditIsSQLOnly).
//
// C splices at addColOffset (from the table's last real parse, sqlite3EndTable,
// build.c:2981; never updated by a catalog write) into the row's current, edited
// text (alter.c ~406-424). So the original "CREATE TABLE t1(a,b)" offset applied
// to the edited "CREATE TABLE t1(a,b,c)" gives "CREATE TABLE t1(a,b, d,c)". Here
// tbl.sql is only ever reassigned by real DDL (edits live in wsEdits), so
// addColumnSpliceOffset(tbl.sql) is that offset.
//
// C then reloads the schema from the spliced text (alter.c:444), so a phantom
// column from the edit becomes real. This re-derives tbl.cols/tbl.checks the same
// way OpenWrite would and clears the row's edit. Declined:
//
//   - a CHECK on the new column: C then runs quick_check over every column
//     (alter.c:447-463), a larger validation than this has;
//   - a re-derived schema that reorders, renames or removes existing columns:
//     stored values map by position, and fixing that needs DROP COLUMN's
//     row rewrite;
//   - a change in rowid alias, WITHOUT ROWID, STRICT or AUTOINCREMENT;
//   - a table that already has an automatic index: one invented by the edit
//     cannot be told apart without the original parse.
func (db *DB) addColumnUnderWritableSchemaEdit(tbl *tableMeta, def *addColumnDef, edit *wsCatalogEdit) error {
	if def.hasCheck {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN %s ... CHECK(...) after a direct sqlite_master write to this table's own row (C SQLite's own post-ALTER re-validation pass, triggered only because the ADDED column carries a CHECK, validates EVERY column's CHECK/NOT NULL -- including ones the edited text invented -- against every existing row; see addColumnUnderWritableSchemaEdit's doc comment)", tbl.name, def.name)
	}
	editedSQL := string(edit.set[4].S)

	// The splice point comes from tbl.sql (this session's definition) applied to the
	// edited catalog text, which is C's shape (addColOffset applied to the current
	// row):
	//
	//	"sql = printf('%%.%ds, ',sql) || %Q
	//	   || substr(sql,1+length(printf('%%.%ds',sql)))"
	//
	// Reading the offset from the edited text instead puts an added column after a
	// phantom one, where C puts it before:
	//
	//	stored: CREATE TABLE t1(a,b)
	//	edited: CREATE TABLE t1(a,b,c DEFAULT 42)
	//	ADD COLUMN d ->  C: CREATE TABLE t1(a,b, d,c DEFAULT 42)
	//
	// altertab.test 22.1 differs only because a schema_version write reloads the
	// schema first (vdbe.c:4262), which the schema_version setter reproduces, so
	// wsEdits is empty and this path is not reached.
	at, ok := addColumnSpliceOffset(tbl.sql)
	if !ok {
		// Fall back to the edited text: an in-memory definition that
		// addColumnSpliceOffset cannot read is exactly the case the clamp and
		// the tail check below exist for, and refusing here would turn a clean
		// decline into an internal error.
		if at, ok = addColumnSpliceOffset(editedSQL); !ok {
			return fmt.Errorf("engine: internal error: ALTER TABLE ADD COLUMN: could not locate a splice point in %s's own stored CREATE TABLE text", tbl.name)
		}
	}
	// printf('%.Ns', sql) is truncation-safe in C SQLite (it prints at
	// most min(N, len(sql)) bytes) -- alter.c's own splice can never run past
	// the end of whatever the row currently holds, however short an edit
	// left it.
	if at > len(editedSQL) {
		at = len(editedSQL)
	}
	newSQL := editedSQL[:at] + ", " + def.rawText + editedSQL[at:]

	newWithoutRowid, newStrict, tailErr := parseTableTailClauses(newSQL)
	if tailErr != nil || newWithoutRowid != tbl.withoutRowid || newStrict {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: the schema text this table's outstanding edit produces after splicing (%q) is not a shape this write path can install", tbl.name, newSQL)
	}
	newCols, newChecksRaw, newSpecs, perr := parseCreateTableColumnsAndAutoIndexes(newSQL, newWithoutRowid)
	if perr != nil {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: %w", tbl.name, perr)
	}
	if len(newCols) <= len(tbl.cols) {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: could not re-derive the altered schema", tbl.name)
	}
	for i := range tbl.cols {
		if !equalFoldName(newCols[i].Name, tbl.cols[i].Name) {
			return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: this table's outstanding schema edit reorders or renames an existing column (position %d: %q -> %q)", tbl.name, i, tbl.cols[i].Name, newCols[i].Name)
		}
	}
	ipk := -1
	for i, c := range newCols {
		if c.IsRowidAlias {
			ipk = i
			break
		}
	}
	if ipk != tbl.ipkIndex {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: this table's outstanding schema edit changes which column is the INTEGER PRIMARY KEY rowid alias", tbl.name)
	}
	for _, ix := range db.indexes {
		if ix.sql == "" && ix.isTemp == tbl.isTemp && equalFoldName(ix.table, tbl.name) {
			return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: this table already has a PRIMARY KEY/UNIQUE-derived index, which this write path cannot safely re-verify against an outstanding schema edit", tbl.name)
		}
	}
	_, autoIdxBySpec, aerr := buildAutoIndexes(tbl.name, newCols, newSpecs)
	if aerr != nil {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: %w", tbl.name, aerr)
	}
	if len(autoIdxBySpec) > 0 {
		// This table had NO automatic index at all (checked just above), so
		// any found here can only have come from the edited text -- a
		// genuinely NEW PRIMARY KEY/UNIQUE constraint this write path cannot
		// materialize mid-session.
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: this table's outstanding schema edit introduces a PRIMARY KEY/UNIQUE constraint", tbl.name)
	}
	newAutoIncrement, ierr := finalizeAutoIncrement(tbl.name, newSQL, newWithoutRowid, newCols, ipk)
	if ierr != nil {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: %w", tbl.name, ierr)
	}
	if newAutoIncrement != tbl.autoIncrement {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: this table's outstanding schema edit changes AUTOINCREMENT", tbl.name)
	}
	newChecks, cerr := finalizeCheckConstraints(tbl.name, newCols, newChecksRaw, newWithoutRowid)
	if cerr != nil {
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: %w", tbl.name, cerr)
	}

	// Every trailing column, the declared one and any phantom from the edit, must
	// back-fill existing rows (a phantom's DEFAULT does too) from a single constant or
	// NULL; a non-constant default declines here, since the added text need not be
	// last.
	oldColCount := len(tbl.cols)
	fillVals := make([]Value, len(newCols)-oldColCount)
	for i := range fillVals {
		nc := newCols[oldColCount+i]
		if nc.HasDefault && !nc.DefaultKnown {
			return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN: this table's outstanding schema edit gives column %s a non-constant DEFAULT, which this write path cannot back-fill existing rows with here", tbl.name, nc.Name)
		}
		if nc.HasDefault {
			fillVals[i] = applyAffinityToValue(nc.DefaultValue, nc.Aff)
		} else {
			fillVals[i] = Value{Typ: Null}
		}
	}
	tbl.cols = newCols
	tbl.checks = newChecks
	tbl.sql = newSQL
	// The outstanding edit is now fully RESOLVED: tbl.sql (and so whatever
	// materialize derives from it) is exactly what C SQLite's reload left
	// in the row for real -- alter.c's own splice UPDATE physically
	// overwrites sqlite_master.sql, so there is no more "corrupted image vs.
	// live schema" divergence for this row to track. Deleted, not left in
	// place, mirroring writableSchemaDDLDecline's existing DROP precedent
	// (an object's own DDL clears its row's corruption).
	delete(db.wsEdits, wsCatalogKey{name: tbl.name, temp: tbl.isTemp})

	db.bumpSchema(tbl.isTemp)
	return nil
}

// addColumn implements "ALTER TABLE t ADD COLUMN coldef": appends the column to
// tbl.cols and back-fills existing rows with its DEFAULT or NULL, splicing the
// coldef's own source text (def.rawText) into the stored CREATE TABLE, as C does.
//
// Rejected as in C: a duplicate name; PRIMARY KEY; UNIQUE; an unknown COLLATE;
// and, only when the table has rows, a non-constant DEFAULT, NOT NULL without a
// non-NULL default, a REFERENCES with a non-NULL default under foreign_keys, or
// a CHECK the back-filled value violates.
func (db *DB) addColumn(tbl *tableMeta, def *addColumnDef) error {
	for _, c := range tbl.cols {
		if equalFoldName(c.Name, def.name) {
			return fmt.Errorf("engine: duplicate column name: %s", def.name)
		}
	}
	if def.isPK {
		return fmt.Errorf("engine: Cannot add a PRIMARY KEY column")
	}
	if def.isUnique {
		return fmt.Errorf("engine: Cannot add a UNIQUE column")
	}
	// A CHECK on the new column is validated against every existing row with the
	// back-fill value, before anything changes; a definitely-false row refuses the
	// ALTER (NULL passes), and an empty table always succeeds:
	//
	//	CREATE TABLE t1(id INTEGER PRIMARY KEY, y INTEGER);
	//	INSERT INTO t1 VALUES(1,10),(2,-10);
	//	ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > y) DEFAULT 0;
	//	  -> CHECK constraint failed (row 1), schema unchanged.
	//	ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > 0);   -- no DEFAULT
	//	  -> succeeds: NULL > 0 is NULL, not false.
	//
	// A REFERENCES clause is accepted; its verbatim text lands in the schema and
	// foreign_key_list reports it. With foreign keys enabled and rows present, a
	// non-NULL DEFAULT is refused (below); C raises it inside the back-fill loop, so
	// an empty table succeeds.
	//
	// These back-fill rules sit in C's "not generated" arm (alter.c), since a
	// generated column is never back-filled: "ADD COLUMN d INT AS (a*3) NOT NULL" is
	// accepted.
	if !def.hasGenerated && def.hasReferences && db.fkEnforce && def.hasDefault && def.defaultValue.Typ != Null && tbl.rows.len() > 0 {
		return fmt.Errorf("engine: Cannot add a REFERENCES column with non-NULL default value")
	}
	// A STORED generated column is refused -- but only on a NON-EMPTY table:
	// sqlite3AlterFinishAddColumn's own refusal is
	// sqlite3ErrorIfNotEmpty(pParse, zDb, zTab, "cannot add a STORED
	// column") (alter.c), which emits a RAISE(ABORT) inside the back-fill
	// loop rather than a parse error, so an empty table has nothing to raise
	// on. Verified against 3.53.3: over an EMPTY t(a INT), "ADD COLUMN d AS
	// (a*3) STORED" succeeds; with two rows already in it, the same statement
	// is "cannot add a STORED column" and the schema is untouched.
	if def.hasGenerated && def.generatedStored && tbl.rows.len() > 0 {
		return fmt.Errorf("engine: cannot add a STORED column")
	}
	if def.hasCollate && !knownCollations[asciiFold(def.collateName, false)] {
		// callback.c:230's own words, and the same rejection: C's three
		// built-in collations are exactly BINARY/NOCASE/RTRIM and this engine
		// registers no others, so the two refuse the same set of names.
		return fmt.Errorf("engine: no such collation sequence: %s", def.collateName)
	}
	// A non-constant DEFAULT is "Cannot add a column with non-constant default"
	// only when the table has rows (a RAISE inside the back-fill loop). Over an empty
	// table it is added and becomes a real per-row default:
	//
	//	ADD COLUMN g DEFAULT CURRENT_TIME       ok
	//	ADD COLUMN h DEFAULT CURRENT_TIMESTAMP  ok
	//	ADD COLUMN j DEFAULT (1+2)              ok
	//	then INSERT INTO t2(x) VALUES(1) ->     length(g)=8, length(h)=19, j=3
	if !def.hasGenerated && def.hasDefault && !def.defaultIsBareLiteral && tbl.rows.len() > 0 {
		return fmt.Errorf("engine: Cannot add a column with non-constant default")
	}
	if !def.hasGenerated && def.notNull && (!def.hasDefault || def.defaultValue.Typ == Null) && tbl.rows.len() > 0 {
		return fmt.Errorf("engine: Cannot add a NOT NULL column with default value NULL")
	}

	collation := "BINARY"
	if def.hasCollate {
		collation = strings.ToUpper(def.collateName)
	}
	// DefaultKnown mirrors def.hasDefault exactly: the only DEFAULT shapes
	// that survive the defaultIsBareLiteral gate above are the bare literal
	// constants (NULL, a signed number, a string, a blob, TRUE/FALSE), and
	// those are precisely the shapes parseColumnDefault (sql_parser.go) folds
	// to the identical Value when it re-parses this same spliced clause out
	// of tbl.sql after a reopen -- so an INSERT that omits the new column
	// stores the same thing in this session as in the next one.
	col := columnInfo{Name: def.name, DeclType: def.declType, Aff: def.aff, NotNull: def.notNull,
		HasDefault: def.hasDefault, DefaultValue: def.defaultValue, DefaultKnown: def.hasDefault, Collation: collation}

	// A direct sqlite_master write to THIS table's own row -- and only its
	// sql column -- takes a completely different path: C SQLite's own
	// addColOffset splice (see addColumnUnderWritableSchemaEdit's doc
	// comment) is applied against the EDITED text, not tbl.sql. Any other
	// shape of edit on this row (name/tbl_name/rootpage/type touched, or the
	// row deleted) stays out of scope -- writableSchemaDDLDecline's own ALTER
	// gate never lets those reach here to begin with.
	if edit := db.wsEditForObjectName(tbl.name, tbl.isTemp); wsEditIsSQLOnly(edit) {
		return db.addColumnUnderWritableSchemaEdit(tbl, def, edit)
	}

	at, ok := addColumnSpliceOffset(tbl.sql)
	if !ok {
		return fmt.Errorf("engine: internal error: ALTER TABLE ADD COLUMN: could not locate a splice point in %s's own stored CREATE TABLE text", tbl.name)
	}
	newSQL := applyEdits(tbl.sql, []textEdit{{at, at, ", " + def.rawText}})

	// A non-constant DEFAULT's fields are re-derived from the spliced schema text by
	// the same code a reopen uses (schema_load_objects.go), taking the last column,
	// so this session and a reopen agree (including DefaultDeferred). Done before
	// any mutation. checks is the full re-validated CHECK list when the new column
	// has a CHECK, else nil.
	var checks []checkConstraint
	if def.hasCheck || def.hasGenerated || tbl.strict || (def.hasDefault && !def.defaultIsBareLiteral) {
		reparsedCols, reparsedChecks, _, perr := parseCreateTableColumnsAndAutoIndexes(newSQL, tbl.withoutRowid)
		if perr != nil {
			if tbl.strict {
				// A STRICT table's column vocabulary is enforced by the
				// re-parse, and C SQLite reports it the same way -- by
				// RELOADING the altered schema, which is why its message is
				// wrapped: "ALTER TABLE nn ADD COLUMN z VARCHAR(3)" on a
				// STRICT table is `error in table nn after add column:
				// unknown datatype for nn.z: "VARCHAR(3)"` (verified
				// directly against 3.53.3).
				return fmt.Errorf("engine: error in table %s after add column: %s", tbl.name, strings.TrimPrefix(perr.Error(), "engine: "))
			}
			return fmt.Errorf("engine: ALTER TABLE ADD COLUMN: %w", perr)
		}
		if len(reparsedCols) != len(tbl.cols)+1 {
			return fmt.Errorf("engine: unsupported: ALTER TABLE ADD COLUMN: this write path cannot re-derive column %s from the altered schema text", def.name)
		}
		if def.hasGenerated {
			// The same validation CREATE TABLE runs, over the SPLICED column
			// list -- C SQLite reaches it the same way, by reloading the
			// altered schema, which is why its own message here is wrapped
			// ("error in table t after add column: no such column: nosuch").
			// specs is nil because ADD COLUMN cannot add a PRIMARY KEY (the
			// isPK check above refuses that outright) and the table's
			// existing constraints were validated when it was created.
			if verr := validateGeneratedColumns(tbl.name, reparsedCols, nil, false); verr != nil {
				return verr
			}
			// The generated column is taken WHOLE from the re-parse, for the
			// reason the non-constant DEFAULT below is: its expression text,
			// its offset and its COMPILED program (columnInfo.genProg) are
			// derived by the same pipeline CREATE TABLE and a reopen use, so
			// this session and the next one cannot disagree about the same
			// stored text. refreshRowPrograms below re-stamps genProg against
			// the widened column list.
			col = reparsedCols[len(reparsedCols)-1]
		}
		if tbl.strict && !def.hasGenerated {
			// Same reason the generated column above is taken WHOLE: a STRICT
			// table's declared type decides the column's affinity by a
			// vocabulary this path's own clause parser does not implement
			// ("ANY" is NO affinity there, not NUMERIC), so building it by
			// hand would give this session one answer and the next reopen
			// another for the same stored text.
			col = reparsedCols[len(reparsedCols)-1]
		}
		if def.hasDefault && !def.defaultIsBareLiteral {
			nc := reparsedCols[len(reparsedCols)-1]
			col.HasDefault, col.DefaultValue = nc.HasDefault, nc.DefaultValue
			col.DefaultKnown, col.DefaultDeferred = nc.DefaultKnown, nc.DefaultDeferred
			// ...and the deferred clause's COMPILED form with it. It is
			// re-derived by the same re-parse (compileDeferredDefaults,
			// sql_parser.go), so copying it here is what keeps the added
			// column's DEFAULT lowered instead of silently dropped; it
			// needs no later re-stamp because these programs are compiled
			// against an empty scope (see columnInfo.defaultProg).
			col.defaultProg = nc.defaultProg
		}
		if def.hasCheck {
			finalized, cerr := finalizeCheckConstraints(tbl.name, reparsedCols, reparsedChecks, tbl.withoutRowid)
			if cerr != nil {
				return fmt.Errorf("engine: ALTER TABLE ADD COLUMN: %w", cerr)
			}
			checks = finalized
		}
	}

	fillVal := Value{Typ: Null}
	if def.hasDefault && def.defaultIsBareLiteral {
		fillVal = applyAffinityToValue(def.defaultValue, col.Aff)
	}
	// Validate the CHECK against every existing row before mutating, using a
	// temporary tableMeta and checkTableChecks (the same evaluator INSERT/UPDATE
	// use). ignore_check_constraints applies here as in C, where ADD COLUMN runs
	// quick_check, one of the flag's enforcement sites.
	if len(checks) > 0 && tbl.rows.len() > 0 {
		tmpTbl := addColumnCheckTable(tbl, col, checks)
		for rowid, vals := range tbl.rows.all() {
			full := append(append([]Value(nil), vals...), fillVal)
			if cerr := db.checkTableChecks(tmpTbl, tbl.name, rowid, full); cerr != nil {
				return fmt.Errorf("engine: ALTER TABLE ADD COLUMN: %w", cerr)
			}
		}
	}
	// Only a bare-literal default can reach a non-empty table (the
	// non-constant case is refused above), so this loop never has to
	// back-fill a value it could not have computed.
	//
	// C writes no row: sqlite3AlterFinishAddColumn (alter.c:313) edits only the
	// catalog, and a record shorter than the table reads its missing columns
	// as their defaults (sqlite3ColumnDefault, update.c:61).
	tbl.cols = append(tbl.cols, col)
	// Widen a segment-backed table's rows here: its row store holds values in
	// declared order (virtual columns included), while expandStoredRow maps a short
	// row by stored slots, which would shift values after a VIRTUAL column. Rows stay
	// declared-order and full width; rowsMutatedThisSession stops the next commit's
	// rewrite from reloading the table and undoing this (segment_ddl.go).
	if tbl.rows != nil {
		fill := Value{Typ: Null}
		if def.hasDefault && def.defaultIsBareLiteral {
			fill = fillVal
		}
		// COLLECTED FIRST, then rewritten: rewrite() invalidates the store's own
		// iteration order, so mutating inside the range panicked in the segment
		// builder at the next commit. dropColumn takes the same two-pass shape
		// for the same reason.
		type widened struct {
			rowid uint64
			vals  []Value
		}
		var todo []widened
		for rowid, vals := range tbl.rows.all() {
			if len(vals) >= len(tbl.cols) {
				continue
			}
			wide := make([]Value, len(tbl.cols))
			copy(wide, vals)
			for i := len(vals); i < len(wide); i++ {
				wide[i] = Value{Typ: Null}
			}
			wide[len(tbl.cols)-1] = fill
			todo = append(todo, widened{rowid, wide})
		}
		for _, w := range todo {
			tbl.rows.rewrite(w.rowid, w.vals)
			tbl.rowsMutatedThisSession, tbl.rowsWrittenSinceCommit = true, true
		}
	}
	if def.hasCheck {
		// checks is the FULL re-derived list (existing checks plus the new
		// column's own); only overwrite tbl.checks when it was actually
		// computed above -- otherwise (the overwhelmingly common case, no
		// CHECK on the new column) checks is nil and assigning it here would
		// silently WIPE OUT the table's own pre-existing CHECK constraints.
		tbl.checks = checks
	}
	// The list just got one column wider, and it was SPLICED rather than
	// re-derived, so every program compiled against the old one now describes
	// a row of the wrong shape -- see refreshRowPrograms. This also covers the
	// CHECK half in both directions: the checks re-derived above were finalized
	// against reparsedCols, a DIFFERENT slice from the tbl.cols this splice
	// produced, and the ones NOT re-derived (no CHECK on the new column) were
	// compiled against the narrower list.
	refreshRowPrograms(tbl)
	tbl.sql = newSQL

	db.bumpSchema(tbl.isTemp)
	return nil
}

// ---- DROP COLUMN ----

// dropColumn implements "ALTER TABLE t DROP COLUMN name": removes the column
// from tbl.cols, every row, and its segment of the stored CREATE TABLE text,
// shifting later index colIdx entries down.
//
// Rejected with C's wording and priority: no such column; the only column; the
// rowid alias or any PRIMARY KEY column; an inline UNIQUE column ("cannot drop
// UNIQUE column"); a column in a table-level UNIQUE(...), an explicit index, a
// CHECK or generated expression elsewhere (C's generic "error in ... after drop
// column"); a view naming the column.
func (db *DB) dropColumn(tbl *tableMeta, colName string) error {
	// C SQLite re-parses the WHOLE schema for this ALTER form and fails if
	// any trigger or view is left dangling by an earlier DROP -- see
	// checkSchemaObjectsResolve. Checked FIRST, so a failure changes nothing.
	if err := db.checkSchemaObjectsResolve(tbl); err != nil {
		return err
	}
	dropIdx := -1
	for i, c := range tbl.cols {
		if equalFoldName(c.Name, colName) {
			dropIdx = i
			break
		}
	}
	if dropIdx < 0 {
		return fmt.Errorf("engine: no such column: %q", colName)
	}
	if len(tbl.cols) <= 1 {
		return fmt.Errorf("engine: cannot drop column %q: no other columns exist", colName)
	}
	dropName := tbl.cols[dropIdx].Name
	if tbl.ipkIndex == dropIdx {
		return fmt.Errorf("engine: cannot drop PRIMARY KEY column: %q", dropName)
	}

	_, _, specs, perr := parseCreateTableColumnsAndAutoIndexes(tbl.sql, tbl.withoutRowid)
	if perr != nil {
		return fmt.Errorf("engine: ALTER TABLE DROP COLUMN: %w", perr)
	}
	for _, spec := range specs {
		if !containsFold(spec.cols, dropName) {
			continue
		}
		if spec.kind == "pk" {
			return fmt.Errorf("engine: cannot drop PRIMARY KEY column: %q", dropName)
		}
		if spec.kind == "u" && spec.inline {
			return fmt.Errorf("engine: cannot drop UNIQUE column: %q", dropName)
		}
		return fmt.Errorf("engine: error in table %s after drop column: no such column: %s", tbl.name, dropName)
	}
	// A CHECK elsewhere that still references the dropped column is rejected with
	// C's "error in table t after drop column: no such column: a". The column's own
	// inline CHECK goes with it.
	for _, cc := range tbl.checks {
		if equalFoldName(cc.ownerCol, dropName) {
			continue
		}
		if checkExprReferencesColumn(cc.expr, dropName) {
			return fmt.Errorf("engine: error in table %s after drop column: no such column: %s", tbl.name, dropName)
		}
	}
	// Likewise a generated column whose expression reads it (alter.c:2319 re-parses
	// and fails): "c AS (a+1)" blocks dropping a. genDeps records which columns each
	// generated expression reads. The dropped generated column itself is exempt.
	for i, c := range tbl.cols {
		if i == dropIdx || !c.IsGenerated() {
			continue
		}
		if slices.Contains(c.genDeps, dropIdx) {
			return fmt.Errorf("engine: error in table %s after drop column: no such column: %s", tbl.name, dropName)
		}
	}
	for _, ix := range db.indexes {
		if !equalFoldName(ix.table, tbl.name) || ix.sql == "" {
			continue // automatic indexes are already covered by the specs loop above
		}
		if containsFold(ix.cols, dropName) {
			return fmt.Errorf("engine: error in index %s after drop column: no such column: %s", ix.name, dropName)
		}
	}
	// DROP COLUMN never rewrites a view (alter.c:2310 updates only the table row),
	// so only a view naming the dropped column fails C's re-parse ("error in view v1
	// after drop column"). The name test is whole-text and conservative.
	for _, v := range db.views {
		if !viewReferencesTable(db, tbl.name) {
			break
		}
		if sqlMentionsIdent(v.sql, dropName) {
			return fmt.Errorf("engine: unsupported: ALTER TABLE DROP COLUMN: table %s is referenced by view %s, which also names %s (this write path cannot prove the reference survives the drop)", tbl.name, v.name, dropName)
		}
	}

	// renameFixQuotes (alter.c:2309) -- DROP COLUMN runs the same whole-schema
	// quotefix RENAME COLUMN does, and for the same reason, immediately after
	// the schema has been proved to still parse.
	quoteUndo, qerr := db.r32nRenameFixQuotes(tbl, "after drop column")
	if qerr != nil {
		return qerr
	}

	list, err := parseTblColumnList(tbl.sql)
	if err != nil {
		quoteUndo.restore()
		return fmt.Errorf("engine: ALTER TABLE DROP COLUMN: %w", err)
	}
	segIdx := -1
	for i, seg := range list.segments {
		if len(seg) == 0 || isTableConstraintSegment(seg) {
			continue
		}
		if columnDefSegNameMatches(seg, colName) {
			segIdx = i
			break
		}
	}
	if segIdx < 0 {
		quoteUndo.restore()
		return fmt.Errorf("engine: internal error: ALTER TABLE DROP COLUMN: could not locate column %s in stored schema text", colName)
	}
	// dropColumnFunc's two spans (alter.c:2283-2300), and the branch is on the
	// last COLUMN, not the last segment -- a table constraint follows the
	// columns and is never one.
	lastCol := true
	for i := segIdx + 1; i < len(list.segments); i++ {
		if !isTableConstraintSegment(list.segments[i]) {
			lastCol = false
			break
		}
	}
	var edit textEdit
	switch {
	case !lastCol:
		// "iCol<pTab->nCol-1": delete from this column's own first token
		// through the START of the next column's, absorbing the separator
		// between them -- "CREATE TABLE t(a, b, c)" dropping "b" gives
		// "CREATE TABLE t(a, c)", never a lingering double comma.
		edit = textEdit{list.segments[segIdx][0].Start, list.segments[segIdx+1][0].Start, ""}
	case segIdx == 0:
		// Unreachable (a one-column table is refused above), but a span that
		// starts at a segment that is not there would be worse than a
		// conservative one.
		edit = textEdit{list.segments[segIdx][0].Start, list.closeParen.Start, ""}
	default:
		// Dropping the last column: C backs up from the previous column's name to the
		// comma before this one and deletes through addColOffset, so whitespace before
		// that comma survives and the dropped column's trailing whitespace and comments
		// go: "t(a , b , CHECK(a>0))" becomes "t(a , CHECK(a>0))", "t(a,b   )" becomes
		// "t(a)".
		prev := list.segments[segIdx-1]
		start := columnListGapComma(tbl.sql, prev[len(prev)-1].End, list.segments[segIdx][0].Start)
		if start < 0 {
			start = prev[len(prev)-1].End
		}
		seg := list.segments[segIdx]
		end := list.closeParen.Start
		if segIdx+1 < len(list.segments) {
			end = columnListGapComma(tbl.sql, seg[len(seg)-1].End, list.segments[segIdx+1][0].Start)
			if end < 0 {
				end = seg[len(seg)-1].End
			}
		}
		edit = textEdit{start, end, ""}
	}
	savedSQL := tbl.sql
	tbl.sql = applyEdits(tbl.sql, []textEdit{edit})

	// The rows are read before the column list loses the column, as alter.c's
	// rewrite reads each one through the old table (alter.c:2320-2350).
	type keptRow struct {
		rowid uint64
		vals  []Value
	}
	var kept []keptRow
	for rowid, vals := range tbl.rows.all() {
		kept = append(kept, keptRow{rowid, vals})
	}
	savedCols := tbl.cols
	savedIpk := tbl.ipkIndex
	tbl.cols = append(tbl.cols[:dropIdx:dropIdx], tbl.cols[dropIdx+1:]...)
	if tbl.ipkIndex > dropIdx {
		tbl.ipkIndex--
	}
	// renameTestSchema(...,"after drop column",1) (alter.c:2319) -- the
	// trigger half (r33rTriggersResolveAfter). It runs HERE, after
	// the schema-text/column-list edits that decide a view's column set and
	// before the row rewrite below, because that rewrite is the first
	// irreversible step: everything above it is restorable by hand.
	if verr := db.r33rTriggersResolveAfter(tbl, "after drop column"); verr != nil {
		tbl.sql, tbl.cols, tbl.ipkIndex = savedSQL, savedCols, savedIpk
		if rerr := refreshTableChecks(tbl); rerr != nil {
			return rerr
		}
		quoteUndo.restore()
		return verr
	}
	for _, ix := range db.indexes {
		if !equalFoldName(ix.table, tbl.name) {
			continue
		}
		for k, ci := range ix.colIdx {
			if ci > dropIdx {
				ix.colIdx[k] = ci - 1
			}
		}
	}
	for _, r := range kept {
		if len(r.vals) > dropIdx {
			tbl.rows.rewrite(r.rowid, append(r.vals[:dropIdx:dropIdx], r.vals[dropIdx+1:]...))
			tbl.rowsMutatedThisSession, tbl.rowsWrittenSinceCommit = true, true
		}
	}
	if err := refreshTableChecks(tbl); err != nil {
		return fmt.Errorf("engine: ALTER TABLE DROP COLUMN: %w", err)
	}
	// Every column after the dropped one moved down a slot, so a program
	// compiled against the old list now reads its neighbour -- see
	// refreshRowPrograms. The CHECK half is already correct by the time this
	// runs (refreshTableChecks above finalizes against tbl.cols, which IS this
	// spliced list) and re-stamping it a second time is DDL-time noise; the
	// generated columns are reached nowhere else.
	refreshRowPrograms(tbl)

	db.bumpSchema(tbl.isTemp)
	return nil
}

// ---- DROP CONSTRAINT ----

// dropConstraint implements "ALTER TABLE t DROP CONSTRAINT name" by editing the
// stored text with sqliteDropConstraintSQL, a port of sqlite_drop_constraint
// (alter.c:2519-2643), before reloading (alter.c:2783-2825). A constraint ends
// at the next constraint keyword (getConstraint, alter.c:2418), not at a comma.
//
//   - a CHECK, or just a "CONSTRAINT name" label (alter.c:2583): CHECKs are
//     re-derived.
//   - a named NOT NULL: dropped, and the column loses NOT NULL on reload.
//   - PRIMARY KEY / UNIQUE / FOREIGN KEY / REFERENCES: "constraint may not be
//     dropped: %s" (alter.c:2595).
//
// Unlike RENAME and DROP COLUMN, it does not re-validate the whole schema first
// (an unrelated dangling trigger does not block it) and needs no view cascade.
func (db *DB) dropConstraint(tbl *tableMeta, name string) error {
	newSQL, _, err := sqliteDropConstraintSQL(tbl.sql, name)
	if err != nil {
		return err
	}
	return db.alterApplyConstraintSQL(tbl, newSQL)
}

// alterFindCol is alterFindCol (alter.c:2700-2728): the index of the column
// named name, or C's error.
func alterFindCol(tbl *tableMeta, name string) (int, error) {
	for i, c := range tbl.cols {
		if equalFoldName(c.Name, name) {
			return i, nil
		}
	}
	return -1, semanticf("engine: no such column: %s", name)
}

// refreshTableNotNull re-derives every column's NOT NULL, and its conflict
// action, from tbl.sql -- what C's schema reload (renameReloadSchema) does
// after an ALTER that edits the stored text.
func refreshTableNotNull(tbl *tableMeta) error {
	cols, _, _, err := parseCreateTableColumnsAndAutoIndexes(tbl.sql, tbl.withoutRowid)
	if err != nil {
		return fmt.Errorf("internal error: re-parsing %s after ALTER TABLE: %w", tbl.name, err)
	}
	if len(cols) != len(tbl.cols) {
		return fmt.Errorf("internal error: re-parsing %s after ALTER TABLE gave %d columns, not %d", tbl.name, len(cols), len(tbl.cols))
	}
	for i := range cols {
		tbl.cols[i].NotNull, tbl.cols[i].NotNullConflict = cols[i].NotNull, cols[i].NotNullConflict
	}
	return nil
}

// alterViolation runs one of C's nested violation scans -- "SELECT
// sqlite_fail('constraint failed', SQLITE_CONSTRAINT) FROM ... WHERE ..."
// (alter.c:2906-2910, 3030-3034) -- over the table as this session holds it:
// "constraint failed" if any row matches where.
func (db *DB) alterViolation(tbl *tableMeta, where string) error {
	pager, err := db.SnapshotPager()
	if err != nil {
		return err
	}
	zDb, zTab := db.targetSchemaName(tbl), tbl.name // zDb, alterFindTable
	_, rows, err := pager.Query(fmt.Sprintf("SELECT 1 FROM %s.%s AS x WHERE %s LIMIT 1",
		sqliteQuoteBigQ(&zDb), sqliteQuoteBigQ(&zTab), where))
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return fmt.Errorf("engine: constraint failed")
	}
	return nil
}

// alterDropNotNull is "ALTER TABLE t ALTER [COLUMN] c DROP NOT NULL"
// (sqlite3AlterDropConstraint's pCol arm, alter.c:2783-2826): the column's NOT
// NULL, named or not, cut out of the stored text by sqlite_drop_constraint(sql,
// iCol), and the schema reloaded -- which moves the cookie even when the column
// had none (the UPDATE of sqlite_schema and renameReloadSchema run either way).
func (db *DB) alterDropNotNull(tbl *tableMeta, colName string) error {
	iCol, err := alterFindCol(tbl, colName)
	if err != nil {
		return err
	}
	newSQL, err := sqliteDropNotNullSQL(tbl.sql, iCol)
	if err != nil {
		return err
	}
	return db.alterApplyConstraintSQL(tbl, newSQL)
}

// alterSetNotNull is "ALTER TABLE t ALTER [COLUMN] c SET NOT NULL [onconf]"
// (sqlite3AlterSetNotNull, alter.c:2883-2924): refused over a NULL already in
// the column, then the column's old NOT NULL replaced by the new one's own text
// -- sqlite_add_constraint(sqlite_drop_constraint(sql, iCol), cons, iCol). colText
// is the column as the statement spelled it, which is how C's scan names it
// ("x.%.*s IS NULL").
func (db *DB) alterSetNotNull(tbl *tableMeta, colName, colText, cons string) error {
	iCol, err := alterFindCol(tbl, colName)
	if err != nil {
		return err
	}
	if err := db.alterViolation(tbl, "x."+colText+" IS NULL"); err != nil {
		return err
	}
	dropped, err := sqliteDropNotNullSQL(tbl.sql, iCol)
	if err != nil {
		return err
	}
	newSQL, err := sqliteAddConstraintSQL(dropped, cons, iCol)
	if err != nil {
		return err
	}
	return db.alterApplyConstraintSQL(tbl, newSQL)
}

// alterAddCheck is "ALTER TABLE t ADD [CONSTRAINT name] CHECK(expr) [onconf]"
// (sqlite3AlterAddConstraint, alter.c:2985-3055), in C's order: the expression
// resolved as a CHECK over the table, a name already in use refused, a row the
// new CHECK would reject refused, and then cons -- the constraint's own text,
// from its first token -- appended to the column list.
func (db *DB) alterAddCheck(tbl *tableMeta, name, exprText, cons string) error {
	newSQL, err := sqliteAddConstraintSQL(tbl.sql, cons, -1)
	if err != nil {
		return err
	}
	// sqlite3ResolveSelfReference(..., NC_IsCheck, ...) (alter.c:3013): the same
	// resolution the CHECKs of a CREATE TABLE get, so it is theirs that runs.
	probe := *tbl
	probe.cols = slices.Clone(tbl.cols)
	probe.sql = newSQL
	if err := refreshTableChecks(&probe); err != nil {
		return err
	}
	if name != "" && sqliteFindConstraint(tbl.sql, name) {
		return fmt.Errorf("engine: constraint %s already exists", strings.ReplaceAll(name, "'", "''"))
	}
	if err := db.alterViolation(tbl, "("+exprText+") IS NOT TRUE"); err != nil {
		return err
	}
	return db.alterApplyConstraintSQL(tbl, newSQL)
}

// alterApplyConstraintSQL stores a constraint edit's text and reloads what the
// table derives from it.
func (db *DB) alterApplyConstraintSQL(tbl *tableMeta, newSQL string) error {
	tbl.sql = newSQL
	if err := refreshTableNotNull(tbl); err != nil {
		return err
	}
	if err := refreshTableChecks(tbl); err != nil {
		return err
	}
	db.bumpSchema(tbl.isTemp)
	return nil
}

// alterCTok is one token of getConstraintToken's stream (alter.c:2136-2162):
// sqlite3GetToken skipping whitespace and comments -- which lex already does --
// except that a parenthesized group is ONE token. kind is an unquoted keyword
// upper-cased, or LP (a whole group), RP, COMMA, EOF, OTHER.
type alterCTok struct {
	kind       string
	start, end int
	t          token
}

// alterConstraintTokens is skipCreateTable (alter.c:2491-2508) followed by the
// getConstraintToken stream of what comes after the column list's own "(" --
// which is tokenized on its own, so that "(" is not folded into a group with
// everything after it. The stream always ends in an EOF token. base is the
// offset the stream starts at, C's iOff after skipCreateTable.
func alterConstraintTokens(createSQL string) (cToks []alterCTok, base int, err error) {
	toks, err := lex(createSQL)
	if err != nil {
		return nil, 0, err
	}
	i := 0
	for ; i < len(toks); i++ {
		if toks[i].kind == tkPunct && toks[i].text == "(" {
			break
		}
		if toks[i].kind == tkEOF {
			return nil, 0, fmt.Errorf("malformed CREATE TABLE text")
		}
	}
	base = toks[i].End
	bt, err := lex(createSQL[base:])
	if err != nil {
		return nil, 0, err
	}
	for k := 0; k < len(bt); k++ {
		t := bt[k]
		c := alterCTok{kind: "OTHER", start: base + t.Start, end: base + t.End, t: t}
		switch {
		case t.kind == tkEOF:
			c.kind = "EOF"
		case t.kind == tkPunct && t.text == "(":
			depth := 1
			j := k + 1
			for ; j < len(bt) && depth > 0; j++ {
				if bt[j].kind == tkPunct && bt[j].text == "(" {
					depth++
				} else if bt[j].kind == tkPunct && bt[j].text == ")" {
					depth--
				} else if bt[j].kind == tkEOF {
					break
				}
			}
			if depth > 0 {
				c.kind = "EOF"
			} else {
				c.kind = "LP"
				c.end = base + bt[j-1].End
				k = j - 1
			}
		case t.kind == tkPunct && t.text == ")":
			c.kind = "RP"
		case t.kind == tkPunct && t.text == ",":
			c.kind = "COMMA"
		case t.kind == tkIdent && !t.quoted:
			c.kind = t.upper()
		}
		cToks = append(cToks, c)
		if c.kind == "EOF" {
			break
		}
	}
	if len(cToks) == 0 || cToks[len(cToks)-1].kind != "EOF" {
		cToks = append(cToks, alterCTok{kind: "EOF", start: len(createSQL), end: len(createSQL)})
	}
	return cToks, base, nil
}

// alterConstraintNameIs is quotedCompare (alter.c:2456-2481): the token, dequoted,
// compared to name case-insensitively.
func alterConstraintNameIs(c alterCTok, name string) bool {
	switch {
	case c.kind == "EOF":
		return false
	case c.t.kind == tkString:
		return equalFoldName(c.t.str, name)
	default:
		return equalFoldName(c.t.text, name)
	}
}

// sqliteDropConstraintSQL is dropConstraintFunc (alter.c:2519-2643) for its
// named form, sqlite_drop_constraint(SQL, TEXT): the text of createSQL with the
// constraint called name removed, and which kind of constraint that was
// ("CHECK" -- including a bare label -- or "NOT"). Its errors are C's own.
func sqliteDropConstraintSQL(createSQL, name string) (string, string, error) {
	return sqliteDropConstraint(createSQL, &name, -1)
}

// sqliteDropNotNullSQL is dropConstraintFunc's other form,
// sqlite_drop_constraint(SQL, INT): the NOT NULL of column iCol removed, named
// or not. A column with none gets the text back unchanged -- "SQLite follows
// postgres in that a DROP NOT NULL on a column that is not NOT NULL is not an
// error" (alter.c:2622-2623).
func sqliteDropNotNullSQL(createSQL string, iCol int) (string, error) {
	s, _, err := sqliteDropConstraint(createSQL, nil, iCol)
	return s, err
}

// sqliteDropConstraint is dropConstraintFunc itself, name set for the named form
// and iNotNull >= 0 for the NOT NULL one. C's offsets become token offsets: its
// iStart is the END of the token before the one it is about to read, and
// "iEnd += getWhitespace(...)" lands on the START of the next token.
func sqliteDropConstraint(createSQL string, name *string, iNotNull int) (string, string, error) {
	cToks, base, err := alterConstraintTokens(createSQL)
	if err != nil {
		return "", "", fmt.Errorf("engine: ALTER TABLE DROP CONSTRAINT: %w", err)
	}
	// getConstraint (alter.c:2418-2437): a constraint runs to the next of these.
	terminator := func(k string) bool {
		switch k {
		case "CONSTRAINT", "PRIMARY", "NOT", "UNIQUE", "CHECK", "DEFAULT", "COLLATE",
			"REFERENCES", "FOREIGN", "RP", "COMMA", "EOF", "AS", "GENERATED":
			return true
		}
		return false
	}
	pos := 0        // next cToks index to read
	prevEnd := base // C's iOff: the end of the last token read
	// skipConstraint is "iOff += getConstraint(&zSql[iOff])".
	skipConstraint := func() {
		for !terminator(cToks[pos].kind) {
			prevEnd = cToks[pos].end
			pos++
		}
	}
	iStart, iEnd := 0, 0
	found := ""
	for ii := 0; iEnd == 0; ii++ {
		for {
			iStart = prevEnd
			c := cToks[pos]
			pos++
			prevEnd = c.end
			if c.kind == "CONSTRAINT" && (name != nil || iNotNull == ii) {
				nt := cToks[pos]
				pos++
				prevEnd = nt.end
				cmp := name != nil && alterConstraintNameIs(nt, *name)
				kind := cToks[pos].kind
				switch kind {
				case "CONSTRAINT", "DEFAULT", "COLLATE", "COMMA", "RP", "GENERATED", "AS":
					kind = "CHECK" // only the label goes (alter.c:2583-2588)
				default:
					pos++
					prevEnd = cToks[pos-1].end
					skipConstraint()
				}
				if cmp || (iNotNull >= 0 && kind == "NOT") {
					if kind != "NOT" && kind != "CHECK" {
						return "", "", semanticf("engine: constraint may not be dropped: %s", *name)
					}
					iEnd, found = prevEnd, kind
					break
				}
			} else if c.kind == "NOT" && iNotNull == ii {
				skipConstraint()
				iEnd, found = prevEnd, "NOT"
				break
			} else if c.kind == "RP" || c.kind == "EOF" {
				iEnd = -1
				break
			} else if c.kind == "COMMA" {
				break
			}
		}
	}
	if iEnd <= 0 {
		if name != nil {
			return "", "", semanticf("engine: no such constraint: %s", *name)
		}
		return createSQL, "", nil
	}
	// alter.c:2626-2640: whitespace and comments after it go; before a ")" or
	// "," no space is left, and a comma right before it goes with it.
	space := " "
	next := cToks[pos]
	if next.kind == "RP" || next.kind == "COMMA" {
		space = ""
		if iStart > 0 && createSQL[iStart-1] == ',' {
			iStart--
		}
	}
	return createSQL[:iStart] + space + createSQL[next.start:], found, nil
}

// sqliteAddConstraintSQL is addConstraintFunc (alter.c:2654-2691),
// sqlite_add_constraint(SQL, CONSTRAINT-TEXT, ICOL): cons appended to the end of
// column iCol's definition after a space, or, for iCol < 0, to the end of the
// whole list after ", ". Whitespace before the column's terminating "," or ")"
// stays in front of the insertion (the getWhitespace at alter.c:2683).
func sqliteAddConstraintSQL(createSQL, cons string, iCol int) (string, error) {
	cToks, _, err := alterConstraintTokens(createSQL)
	if err != nil {
		return "", fmt.Errorf("engine: ALTER TABLE: %w", err)
	}
	pos := 0
	iOff := 0
	kind := ""
	for ii := 0; ii <= iCol || (iCol < 0 && kind != "RP"); ii++ {
		c := cToks[pos] // iOff += getConstraintToken(...)
		if c.kind == "EOF" {
			return "", fmt.Errorf("database disk image is malformed")
		}
		pos++
		iOff, kind = c.end, c.kind
		for {
			n := cToks[pos]
			kind = n.kind
			if n.kind == "COMMA" || n.kind == "RP" {
				break
			}
			if n.kind == "EOF" { // TK_ILLEGAL: SQLITE_CORRUPT_BKPT
				return "", fmt.Errorf("database disk image is malformed")
			}
			iOff = n.end
			pos++
		}
	}
	iOff = cToks[pos].start // iOff += getWhitespace(...)
	if iCol < 0 {
		return createSQL[:iOff] + ", " + cons + createSQL[iOff:], nil
	}
	return createSQL[:iOff] + " " + cons + createSQL[iOff:], nil
}

// sqliteFindConstraint is findConstraintFunc (alter.c:2932-2969): whether
// createSQL declares a constraint called name anywhere, column or table level.
func sqliteFindConstraint(createSQL, name string) bool {
	cToks, _, err := alterConstraintTokens(createSQL)
	if err != nil {
		return false
	}
	for i, c := range cToks {
		if c.kind == "CONSTRAINT" && i+1 < len(cToks) && alterConstraintNameIs(cToks[i+1], name) {
			return true
		}
	}
	return false
}

// alterRtrimConstraint is alterRtrimConstraint (alter.c:2858-2878): cons with
// trailing whitespace and "--" comments dropped. Every other token counts, a
// C-style comment included, so cons runs to the end of whichever of those comes
// last.
func alterRtrimConstraint(cons string) string {
	toks, err := lex(cons)
	if err != nil {
		return strings.TrimSpace(cons)
	}
	end := 0
	for _, t := range toks {
		if t.kind != tkEOF {
			end = max(end, t.End)
		}
	}
	for i := end; i < len(cons); {
		switch {
		case strings.IndexByte(" \t\n\f\r", cons[i]) >= 0:
			i++
		case strings.HasPrefix(cons[i:], "/*"):
			j := strings.Index(cons[i+2:], "*/")
			if j < 0 {
				return cons // an unterminated comment runs to the end, and is kept
			}
			i += j + 4
			end = i
		case strings.HasPrefix(cons[i:], "--"):
			j := strings.IndexByte(cons[i:], '\n')
			if j < 0 {
				return cons[:end]
			}
			i += j + 1
		default:
			return cons[:end]
		}
	}
	return cons[:end]
}

// ---- ALTER TABLE: re-validating the rest of the schema ----

// alterSkipsCatalog reports whether an ALTER on a table in catalog tblIsTemp
// leaves a view or trigger in catalog objIsTemp alone, neither re-validated nor
// rewritten. The rule is asymmetric: C rewrites the altered table's own schema,
// then makes a second pass over temp restricted to views and triggers (guarded
// "if( iDb!=1 )"; renameTestSchema likewise, "if( bTemp==0 )"):
//
//	altered table in main, object in main   reached
//	altered table in main, object in TEMP   reached (views and triggers ONLY)
//	altered table in temp, object in temp   reached
//	altered table in temp, object in MAIN   NEVER -- not one kind
//
// Tables (REFERENCES) and indexes are scoped by plain catalog equality at their
// call sites.
func alterSkipsCatalog(tblIsTemp, objIsTemp bool) bool {
	return tblIsTemp && !objIsTemp
}

// alterColumnShadowed reports whether RENAME COLUMN's cascade into TEMP views
// and triggers is blocked because a TEMP table shadows the altered MAIN table's
// name. renameColumnFunc resolves every reference by identity, and a temp object
// resolves the bare name temp-first, so with a shadow nothing in temp refers to
// the altered table.
func alterColumnShadowed(db *DB, tbl *tableMeta) bool {
	return !tbl.isTemp && db.findTableMetaIn(createScope(true), tbl.name) != nil
}

// checkSchemaObjectsResolve re-parses every trigger and view and reports the
// first that references a missing table, as C does before renaming (since 3.25):
//
//	ALTER TABLE t1 RENAME TO t9        -> error in trigger tr1: no such table: main.ff
//	ALTER TABLE t1 RENAME COLUMN a TO b -> error in trigger tr1: no such table: main.ff
//	ALTER TABLE t1 DROP COLUMN b       -> error in trigger tr1: no such table: main.ff
//	ALTER TABLE t1 ADD COLUMN c        -> accepted (ADD COLUMN does NOT re-parse)
//	DROP TABLE t1                      -> accepted (only ALTER re-parses)
//
// The broken object can be unrelated to the altered table; a missing column
// trips it too, an unknown function does not. Conservative about what counts as
// a table reference (collectSelectTables): a miss keeps the ALTER accepted.
//
// A no-op under writable_schema=ON: renameTestSchema's error branch is guarded
// "!sqlite3WritableSchema(db)" (alter.c:2101). C also ANDs in defensive mode
// (build.c:1009), which this engine does not have.
func (db *DB) checkSchemaObjectsResolve(tbl *tableMeta) error {
	if db.writableSchema {
		return nil
	}
	schema := localSchemaOr(db.localSchema)
	// missing reports the first name that resolves nowhere. allowAttached is whether
	// the enclosing trigger/view is TEMP: a TEMP object's unqualified names search
	// TEMP, MAIN and attachments (build.c:373), while a MAIN object's are pinned to
	// its own schema at CREATE (attach.c:492; trigger.c:346; build.c:3032). Only
	// attached tables count, not views: that can only decline more.
	missing := func(names []string, allowAttached bool) string {
		for _, n := range names {
			// A VIRTUAL table counts as resolvable, exactly like an ordinary
			// one: C SQLite's re-parse resolves it through sqlite3FindTable
			// too. Leaving it out reported every trigger and view naming an
			// fts/rtree table as DANGLING, so any ALTER TABLE RENAME TO
			// anywhere in the schema failed with "error in trigger tr: no such
			// table: main.<vtab>" -- verified against 3.53.3, which accepts it
			// and rewrites the trigger.
			if db.findTableMeta(n) == nil && db.findViewMeta(n) == nil && db.findVtabMeta(n) == nil {
				if allowAttached && db.firstAttachedTable(n) != nil {
					continue
				}
				return n
			}
		}
		return ""
	}
	for _, tr := range db.triggers {
		if alterSkipsCatalog(tbl.isTemp, tr.isTemp) {
			continue
		}
		var names []string
		for _, bs := range tr.body {
			switch {
			case bs.insert != nil:
				names = append(names, bs.insert.table)
				collectSelectTables(bs.insert.selectStmt, &names)
			case bs.update != nil:
				names = append(names, bs.update.table)
			case bs.delete != nil:
				names = append(names, bs.delete.table)
			case bs.sel != nil:
				collectSelectTables(bs.sel, &names)
			}
		}
		if n := missing(names, tr.isTemp); n != "" {
			return fmt.Errorf("engine: error in trigger %s: no such table: %s.%s", tr.name, schema, n)
		}
		// The re-parse resolves columns too, so a trigger naming a missing column fails
		// the ALTER ("error in trigger tt2: no such column: old.nope"). Only "no such
		// column" is promoted; other validator complaints are this engine's declines,
		// not C's reasons. The table is looked up in the catalog the trigger is bound to
		// (tableIsTemp), so a TEMP shadow is not what OLD/NEW validate against
		// (fkey2.test 14.2tmp).
		if trTbl := db.findTableMetaIn(createScope(tr.tableIsTemp), tr.table); trTbl != nil {
			if verr := db.validateTriggerExprsOnce(trTbl, tr.event, []*triggerMeta{tr}); verr != nil {
				if msg := strings.TrimPrefix(verr.Error(), "engine: "); strings.HasPrefix(msg, "no such column: ") {
					return fmt.Errorf("engine: error in trigger %s: %s", tr.name, msg)
				}
			}
		}
	}
	for _, v := range db.views {
		if alterSkipsCatalog(tbl.isTemp, v.isTemp) {
			continue
		}
		var names []string
		collectSelectTables(v.selectStmt, &names)
		if n := missing(names, v.isTemp); n != "" {
			return fmt.Errorf("engine: error in view %s: no such table: %s.%s", v.name, schema, n)
		}
	}
	return nil
}

// r33rTriggersResolveAfter is the post-cascade half of renameTestSchema
// (alter.c:676, 2319) that checkSchemaObjectsResolve cannot answer: an INSTEAD OF
// trigger on a view. renameResolveTrigger binds NEW/OLD to the view's freshly
// recomputed columns (alter.c:1358, build.c:3210), so renaming or dropping a
// base column can remove a view column the trigger still names:
//
//	CREATE TABLE t1(a,b);
//	CREATE VIEW v1 AS SELECT a,b FROM t1;              -- or "SELECT * FROM t1"
//	CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a,new.b); END;
//	ALTER TABLE t1 RENAME a TO aaa;   -- error in trigger tr after rename: no such column: new.a
//	ALTER TABLE t1 DROP COLUMN a;     -- error in trigger tr after drop column: ...
//
// WHEN clauses count too, and temp objects over a main table are reached
// (alterSkipsCatalog). It runs after the cascade because the rewritten view
// decides: "SELECT a AS a" or an explicit column list keeps the name. Only "no
// such column" is promoted.
func (db *DB) r33rTriggersResolveAfter(tbl *tableMeta, when string) error {
	// Same renameTableTest (alter.c:2050) drives this "after rename"/"after
	// drop column" pass as the pre-flight one -- its error-raising branch
	// (alter.c:2101) is gated "!sqlite3WritableSchema(db)" regardless of
	// which zWhen it was called with, so this half is suppressed too. See
	// checkSchemaObjectsResolve's identical guard for the verified example.
	if db.writableSchema {
		return nil
	}
	var pager *ReadOnlyPager
	for _, tr := range db.triggers {
		// tbl is always LOCAL, so a trigger bound to an ATTACHed database's
		// table (triggerMeta.tableAttachName) is never validated against
		// IT here -- see renameTriggerReferences' identical guard. Without
		// this, db.findTableMetaIn(createScope(tr.tableIsTemp), tr.table)
		// below would resolve by BARE NAME alone and could match a same-named
		// LOCAL table that is not this trigger's real target at all
		// (trigger1.test 10.x's main.t4/temp.t4/attachment-t4), validating
		// the trigger's body against the WRONG schema.
		if tr.tableAttachName != "" {
			continue
		}
		if alterSkipsCatalog(tbl.isTemp, tr.isTemp) {
			continue
		}
		var verr error
		switch {
		case db.findTableMetaIn(createScope(tr.tableIsTemp), tr.table) != nil:
			// A table trigger: re-run checkSchemaObjectsResolve's column check on the
			// post-cascade schema, the only place a DROP COLUMN shows up (alter.c:1367
			// preps every step's SELECT): "INSERT INTO t1(a,b) SELECT a, b FROM t1 ..." then
			// "DROP COLUMN b" is "error in trigger tr after drop column: no such column: b".
			trTbl := db.findTableMetaIn(createScope(tr.tableIsTemp), tr.table)
			verr = db.validateTriggerExprsOnce(trTbl, tr.event, []*triggerMeta{tr})
		case db.findViewMetaIn(createScope(tr.tableIsTemp), tr.table) != nil:
			vm := db.findViewMetaIn(createScope(tr.tableIsTemp), tr.table)
			if pager == nil {
				p, perr := db.SnapshotPager()
				if perr != nil {
					return perr
				}
				pager = p
			}
			cols, cerr := pager.viewColumnInfos(vm.name, viewMetaToParsed(vm))
			if cerr != nil {
				continue
			}
			verr = db.validateViewTriggerExprsOnce(cols, tr.event, []*triggerMeta{tr})
		default:
			continue
		}
		if verr == nil {
			continue
		}
		if msg := strings.TrimPrefix(verr.Error(), "engine: "); strings.HasPrefix(msg, "no such column: ") {
			return fmt.Errorf("engine: error in trigger %s %s: %s", tr.name, when, msg)
		}
	}
	return nil
}

// collectSelectTables appends every FROM-clause table name
// sel refers to, skipping anything that is not plainly a base-table
// reference: a derived table, a table-valued function, and -- importantly --
// any name bound by a WITH clause in scope, which looks like a table but is
// not one. Expression subqueries are not descended into at all. Every
// omission only costs coverage (an ALTER stays accepted); a false positive
// would cost correctness, which is why this errs so far toward omission.
func collectSelectTables(sel *SelectStmt, out *[]string) {
	collectSelectTablesScoped(sel, nil, out)
}

// collectSelectTablesScoped is collectSelectTables with the CTE names bound by
// every enclosing WITH, as qualifyUnqualifiedFromItems threads them. C pushes a
// WITH's CTE list once before walking any body (sqlite3WithPush, select.c:5649)
// and resolves names against the whole stack (searchWith, select.c:5610), so a
// CTE body may name itself, an earlier sibling, or an outer WITH's CTE:
//
//	WITH RECURSIVE t3(x,y,z) AS (SELECT ... UNION SELECT ... FROM t3, t2) SELECT * FROM t3   -- self
//	WITH p AS (SELECT 1 FROM t1), g AS (SELECT 1 FROM p, t1) SELECT 1 FROM g                  -- sibling
//	WITH x AS (WITH y AS (SELECT * FROM x) SELECT 1) SELECT 1                                 -- outer
func collectSelectTablesScoped(sel *SelectStmt, cteNames map[string]bool, out *[]string) {
	if sel == nil {
		return
	}
	if len(sel.CTEs) > 0 {
		next := make(map[string]bool, len(cteNames)+len(sel.CTEs))
		for k := range cteNames {
			next[k] = true
		}
		for _, cte := range sel.CTEs {
			next[r33sFoldIdent(cte.Name)] = true
		}
		cteNames = next
	}
	for _, cte := range sel.CTEs {
		collectSelectTablesScoped(cte.Select, cteNames, out)
	}
	for _, f := range sel.From {
		if f.Table != "" && !f.TableFunc && f.Subquery == nil && !cteNames[r33sFoldIdent(f.Table)] {
			*out = append(*out, f.Table)
		}
		collectSelectTablesScoped(f.Subquery, cteNames, out)
	}
	for _, arm := range sel.Compound {
		collectSelectTablesScoped(arm.Stmt, cteNames, out)
	}
}

// renameForeignKeyReferences cascades RENAME TO into every REFERENCES clause
// naming oldName in every table's stored SQL, the renamed table's own included.
// C quotes the new name:
//
//	CREATE TABLE p1(a PRIMARY KEY);
//	CREATE TABLE c1(x INTEGER PRIMARY KEY, y REFERENCES p1(a));
//	ALTER TABLE p1 RENAME TO ppp;   -- c1: y REFERENCES "ppp"(a)
//
// Foreign keys are resolved from this text (fk.go), so a stale name would point
// the key at a missing table. Only the identifier right after REFERENCES is
// edited, which is unambiguous. Only the altered table's own catalog: a foreign
// key's parent resolves in the child's catalog.
func (db *DB) renameForeignKeyReferences(tbl *tableMeta, oldName, newName string) {
	for _, t := range db.tables {
		if t.sql == "" || t.isTemp != tbl.isTemp {
			continue
		}
		edits := locateReferencesTargets(t.sql, oldName, newName)
		if len(edits) > 0 {
			t.sql = applyEdits(t.sql, edits)
		}
	}
}

// r34vRenameForeignKeyParentColumns cascades RENAME COLUMN into every child's
// "REFERENCES t(<cols>)" list naming the column (alter.c:1631):
//
//	if( 0==sqlite3_stricmp(pFKey->zTo, zTable)
//	 && 0==sqlite3_stricmp(pFKey->aCol[i].zCol, zOld) ){
//	    renameTokenFind(&sParse, &sCtx, (void*)pFKey->aCol[i].zCol);
//	}
//
// ("REFERENCES p1(c, d)" becomes p1(c, "silly name"), altercol.test). Only
// identifiers in the list after "REFERENCES <t>" are edited, with the shared
// repl. Same-catalog tables only.
func (db *DB) r34vRenameForeignKeyParentColumns(tbl *tableMeta, oldName string, repl identRepl) {
	for _, t := range db.tables {
		if t.sql == "" || t.isTemp != tbl.isTemp {
			continue
		}
		edits := r34vLocateReferencesParentColumns(t.sql, tbl.name, oldName, repl)
		if len(edits) > 0 {
			t.sql = applyEdits(t.sql, edits)
		}
	}
}

// r34vLocateReferencesParentColumns returns one edit per identifier spelling
// oldName inside a "REFERENCES <parent>( ... )" column list of sqlText.
func r34vLocateReferencesParentColumns(sqlText, parent, oldName string, repl identRepl) []textEdit {
	toks, err := lex(sqlText)
	if err != nil {
		return nil // an unparsable stored schema is left alone
	}
	var out []textEdit
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].kind != tkIdent || toks[i].quoted || toks[i].upper() != "REFERENCES" {
			continue
		}
		nt := toks[i+1]
		name := nt.text
		if nt.kind == tkString {
			name = nt.str
		} else if nt.kind != tkIdent {
			continue
		}
		if !equalFoldName(name, parent) {
			continue
		}
		// The column list is OPTIONAL ("REFERENCES p1" means the parent's
		// primary key); only a "(" immediately after the name starts one.
		if toks[i+2].kind != tkPunct || toks[i+2].text != "(" {
			continue
		}
		depth := 0
		for j := i + 2; j < len(toks); j++ {
			t := toks[j]
			if t.kind == tkPunct && t.text == "(" {
				depth++
				continue
			}
			if t.kind == tkPunct && t.text == ")" {
				depth--
				if depth == 0 {
					i = j
					break
				}
				continue
			}
			if depth == 1 && t.kind == tkIdent && equalFoldName(t.text, oldName) {
				out = append(out, textEdit{t.Start, t.End, repl.at(sqlText, t)})
			}
		}
	}
	return out
}

// locateReferencesTargets returns one edit per "REFERENCES <target>" in sqlText,
// replacing the target identifier with the quoted replacement.
func locateReferencesTargets(sqlText, target, replacement string) []textEdit {
	toks, err := lex(sqlText)
	if err != nil {
		return nil // an unparsable stored schema is left alone
	}
	var out []textEdit
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind != tkIdent || toks[i].quoted || toks[i].upper() != "REFERENCES" {
			continue
		}
		nt := toks[i+1]
		// The target may be spelled as an identifier OR a string literal
		// ("REFERENCES 'p 1 \"parent one\"'" -- e_fkey.test); a string names
		// its decoded value.
		ntName := nt.text
		if nt.kind == tkString {
			ntName = nt.str
		} else if nt.kind != tkIdent {
			continue
		}
		if !equalFoldName(ntName, target) {
			continue
		}
		out = append(out, textEdit{nt.Start, nt.End, quoteIdent(replacement)})
	}
	return out
}

// autoIndexNameSuffix splits an automatic index's name into the part after the
// table name -- "_1", "_2", ... -- reporting false for a name that is not
// sqlite_autoindex_<table>_<N> for this table. Matching the table part
// case-insensitively mirrors how the name was built from the table's own
// spelling.
func autoIndexNameSuffix(indexName, tableName string) (string, bool) {
	const prefix = "sqlite_autoindex_"
	if len(indexName) <= len(prefix)+len(tableName) {
		return "", false
	}
	if !equalFoldName(indexName[:len(prefix)], prefix) {
		return "", false
	}
	rest := indexName[len(prefix):]
	if !equalFoldName(rest[:len(tableName)], tableName) {
		return "", false
	}
	suffix := rest[len(tableName):]
	if len(suffix) < 2 || suffix[0] != '_' {
		return "", false
	}
	for _, c := range suffix[1:] {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return suffix, true
}

// viewRenameAmbiguity reports a source name that appears twice in one of sel's
// FROM lists, or "". C re-resolves a rewritten view and fails the whole ALTER if
// it broke (alterlegacy.test 5.3):
//
//	CREATE VIEW v AS SELECT one.a, one.b, t2.a, t2.b FROM t1 AS one, t2;
//	ALTER TABLE t2 RENAME TO one;
//	  -> error in view v after rename: ambiguous column name: one.a
//
// Coarser than C (any duplicate source name refuses, not only one that makes a
// reference ambiguous), which can only decline more. Each FROM list, subquery and
// compound arm is checked separately.
func viewRenameAmbiguity(sel *SelectStmt) string {
	if sel == nil {
		return ""
	}
	if !viewRenameAmbiguityExempt(sel) {
		seen := make(map[string]bool, len(sel.From))
		for _, f := range sel.From {
			name := f.Alias
			if name == "" {
				name = f.Table
			}
			if name == "" {
				continue
			}
			key := r33sFoldIdent(name)
			if seen[key] {
				return name
			}
			seen[key] = true
		}
	}
	for _, f := range sel.From {
		if dup := viewRenameAmbiguity(f.Subquery); dup != "" {
			return dup
		}
	}
	for _, cte := range sel.CTEs {
		if dup := viewRenameAmbiguity(cte.Select); dup != "" {
			return dup
		}
	}
	for _, arm := range sel.Compound {
		if dup := viewRenameAmbiguity(arm.Stmt); dup != "" {
			return dup
		}
	}
	return ""
}

// viewRenameAmbiguityExempt reports whether sel by itself resolves no column at
// all (a literal-only select list, no WHERE/GROUP BY/HAVING/ORDER BY/CTE/compound,
// no join conditions, subqueries or table-function arguments). Then a duplicate
// FROM name cannot become an ambiguity, since C reports one only while resolving
// a column reference (altertab.test 19.100):
//
//	CREATE VIEW t2 AS SELECT 1 FROM t1, (t1 AS a0, t1);
//	ALTER TABLE t1 RENAME TO t3;
//	  -> accepted; CREATE VIEW t2 AS SELECT 1 FROM "t3", ("t3" AS a0, "t3")
//
// Deliberately narrow: a missed node type in a broader walk could accept a rename
// C rejects.
func viewRenameAmbiguityExempt(sel *SelectStmt) bool {
	if sel.Where != nil || len(sel.GroupBy) != 0 || sel.Having != nil || len(sel.OrderBy) != 0 || len(sel.CTEs) != 0 || len(sel.Compound) != 0 {
		return false
	}
	for _, c := range sel.Columns {
		if c.Star {
			return false
		}
		if _, ok := c.Expr.(LiteralExpr); !ok {
			return false
		}
	}
	for _, f := range sel.From {
		if f.On != nil || len(f.Using) != 0 || f.Natural || f.Subquery != nil || f.TableFunc {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// renameFixQuotes (alter.c:646)
//
// Before RENAME COLUMN (alter.c:646) or DROP COLUMN (alter.c:2309), never RENAME
// TO, C runs one UPDATE over the altered table's database's schema, plus temp's
// unless the table is temp, through sqlite_rename_quotefix (alter.c:1937):
//
//	UPDATE "%w".sqlite_master SET sql = sqlite_rename_quotefix(%Q, sql)
//	 WHERE name NOT LIKE 'sqliteX_%' ESCAPE 'X'
//	   AND sql NOT LIKE 'create virtual%'
//
// It re-parses and resolves each row and rewrites every double-quoted token the
// resolver demoted to a string (renameQuotefixExprCb: TK_STRING with
// EP_DblQuoted, set when a bare unqualified "..." matched no column, resolve.c:721)
// into single quotes. That is a resolution question, not a lexical one (alterqf.test
// 2.1, over x1(one, two, three)):
//
//	CHECK (three!="xyz")   ->  CHECK (three!='xyz')   -- no column "xyz"
//	CHECK (two!="one")     ->  CHECK (two!="one")     -- column "one" exists
//
// Scope per object kind: a table's CHECKs and generated columns against its own
// columns (resolve.c:2299); an index's expressions and WHERE against its table;
// a view's whole prepped SELECT; a trigger per renameResolveTrigger (see
// r32nQuotefixTrigger). The quotefix runs before the rename UPDATE, so in
// alterqf.test 2.1 `x1(one+"two"+"four") WHERE "five"` becomes
// `(one+"four"+'four') WHERE 'five'`.

// r32nQuoteFix is one double-quoted token the quotefix pass must rewrite:
// span is its raw extent in the stored SQL (both quote characters included)
// and name is its DEQUOTED text, which is what SQLite's own "%Q" re-quotes.
type r32nQuoteFix struct {
	span byteSpan
	name string
}

// r32nQuoteEdits renders fixes as textEdits, reproducing renameEditSql's zNew==0
// branch (alter.c:1281): dequote, requote with %Q, and add one space when the next
// character is a single quote. Kept as a code block because gofmt rewrites two
// adjacent single quotes in prose:
//
//	SELECT "string"'alias'    the input
//	SELECT 'string' 'alias'   with the space: two tokens, an aliased string
//	SELECT 'string''alias'    without it: ONE string, 'string'alias'
func r32nQuoteEdits(sqlText string, fixes []r32nQuoteFix) []textEdit {
	edits := make([]textEdit, 0, len(fixes))
	for _, f := range fixes {
		repl := "'" + strings.ReplaceAll(f.name, "'", "''") + "'"
		if f.span.end < len(sqlText) && sqlText[f.span.end] == '\'' {
			repl += " "
		}
		edits = append(edits, textEdit{f.span.start, f.span.end, repl})
	}
	return edits
}

// r32nScope is one NameContext in lookupName's chain: the names a bare reference
// resolves against here, and the enclosing context to fall through to
// (resolve.c:704). Contents are a superset of what C would match (a derived table
// or view contributes every name inside it). That is the safe direction: a name in
// no scope certainly demoted in C; a name wrongly found is just left alone.
type r32nScope struct {
	parent *r32nScope
	names  map[string]bool
}

func r32nNewScope(parent *r32nScope) *r32nScope {
	return &r32nScope{parent: parent, names: map[string]bool{}}
}

func (s *r32nScope) addName(n string) {
	if n != "" {
		s.names[r33sFoldIdent(n)] = true
	}
}

// addRowidNames adds the three spellings sqlite3IsRowid accepts. C SQLite
// resolves them for a rowid table in a CHECK, in a partial-index WHERE and in
// any ordinary SELECT, but NOT inside an index expression or a generated
// column: resolve.c:625 gates the rowid branch on
// `(pNC->ncFlags & (NC_IdxExpr|NC_GenCol))==0`.
func (s *r32nScope) addRowidNames() {
	s.addName("rowid")
	s.addName("oid")
	s.addName("_rowid_")
}

func (s *r32nScope) resolves(n string) bool {
	lower := r33sFoldIdent(n)
	for c := s; c != nil; c = c.parent {
		if c.names[lower] {
			return true
		}
	}
	return false
}

// r32nDQWalk is one object's quotefix analysis: every bare double-quoted
// reference with the scope it was found in. ok goes false on anything whose scope
// cannot be established (a table function, an unresolvable FROM item, vtab hidden
// columns, an unknown node). Decisions wait for unresolved(), since scopes keep
// gaining names during the walk.
type r32nDQWalk struct {
	pending []r32nPendingFix
	depth   int
	ok      bool
}

type r32nPendingFix struct {
	fix   r32nQuoteFix
	scope *r32nScope
}

func r32nNewWalk() *r32nDQWalk { return &r32nDQWalk{ok: true} }

// unresolved keeps only the references whose name matched nothing anywhere in
// their own scope chain -- lookupName's cnt==0, the sole precondition of its
// EP_DblQuoted demotion (resolve.c:721).
func (w *r32nDQWalk) unresolved() []r32nQuoteFix {
	var out []r32nQuoteFix
	for _, p := range w.pending {
		if !p.scope.resolves(p.fix.name) {
			out = append(out, p.fix)
		}
	}
	return out
}

// r32nCollectExpr appends every bare double-quoted reference in e that carries
// a recorded span, tagged with the scope sc it sits in, descending into
// subqueries exactly as sqlite3WalkExpr does (it walks x.pSelect too, which is
// how a quotefix reaches a view body's EXISTS/IN subquery). An EXPRESSION
// subquery gets a CHILD scope and contributes nothing back upward: its columns
// are visible only inside it, which is the whole reason this is a scope CHAIN
// and not one flat name set.
func (db *DB) r32nCollectExpr(e Expr, sc *r32nScope, w *r32nDQWalk) {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
	case ColumnExpr:
		if x.R32NDQSpan.end > x.R32NDQSpan.start {
			w.pending = append(w.pending, r32nPendingFix{
				fix:   r32nQuoteFix{span: x.R32NDQSpan, name: x.Name},
				scope: sc,
			})
		}
	case UnaryExpr:
		db.r32nCollectExpr(x.X, sc, w)
	case BinaryExpr:
		db.r32nCollectExpr(x.L, sc, w)
		db.r32nCollectExpr(x.R, sc, w)
	case IsNullExpr:
		db.r32nCollectExpr(x.X, sc, w)
	case InExpr:
		db.r32nCollectExpr(x.X, sc, w)
		for _, it := range x.List {
			db.r32nCollectExpr(it, sc, w)
		}
		db.r32nCollectSelect(x.Sub, r32nNewScope(sc), w)
	case BetweenExpr:
		db.r32nCollectExpr(x.X, sc, w)
		db.r32nCollectExpr(x.Lo, sc, w)
		db.r32nCollectExpr(x.Hi, sc, w)
	case LikeExpr:
		db.r32nCollectExpr(x.X, sc, w)
		db.r32nCollectExpr(x.Pattern, sc, w)
		db.r32nCollectExpr(x.Escape, sc, w)
	case GlobExpr:
		db.r32nCollectExpr(x.X, sc, w)
		db.r32nCollectExpr(x.Pattern, sc, w)
	case MatchExpr:
		db.r32nCollectExpr(x.X, sc, w)
		db.r32nCollectExpr(x.Pattern, sc, w)
	case CollateExpr:
		db.r32nCollectExpr(x.X, sc, w)
	case CastExpr:
		db.r32nCollectExpr(x.X, sc, w)
	case RowExpr:
		for _, it := range x.Elems {
			db.r32nCollectExpr(it, sc, w)
		}
	case CaseExpr:
		db.r32nCollectExpr(x.Base, sc, w)
		for _, cl := range x.Whens {
			db.r32nCollectExpr(cl.When, sc, w)
			db.r32nCollectExpr(cl.Then, sc, w)
		}
		db.r32nCollectExpr(x.Else, sc, w)
	case RaiseExpr:
		db.r32nCollectExpr(x.Msg, sc, w)
	case FuncExpr:
		for _, a := range x.Args {
			db.r32nCollectExpr(a, sc, w)
		}
		db.r32nCollectExpr(x.Filter, sc, w)
		for _, ob := range x.orderByExprs() {
			db.r32nCollectExpr(ob, sc, w)
		}
		db.r32nCollectWindow(x.Over, sc, w)
	case SubqueryExpr:
		db.r32nCollectSelect(x.Stmt, r32nNewScope(sc), w)
	case ExistsExpr:
		db.r32nCollectSelect(x.Stmt, r32nNewScope(sc), w)
	default:
		// An expression node this walk does not know cannot be proved free of
		// double-quoted references, so the whole object is left alone.
		w.ok = false
	}
}

func (db *DB) r32nCollectWindow(spec *WindowSpec, sc *r32nScope, w *r32nDQWalk) {
	if spec == nil {
		return
	}
	for _, e := range spec.PartitionBy {
		db.r32nCollectExpr(e, sc, w)
	}
	for _, t := range spec.OrderBy {
		db.r32nCollectExpr(t.Expr, sc, w)
	}
	if spec.Frame != nil {
		db.r32nCollectExpr(spec.Frame.Start.Offset, sc, w)
		db.r32nCollectExpr(spec.Frame.End.Offset, sc, w)
	}
}

// r32nCollectSelect walks sel as sqlite3WalkSelect does (select list, FROM with
// subqueries, WHERE, GROUP BY, HAVING, ORDER BY, windows, compound arms), filling
// sc and recording double-quoted references. A FROM subquery is walked in a child
// scope and its names unioned into sc (r32nAddDerivedNames); an expression
// subquery's names do not flow back.
func (db *DB) r32nCollectSelect(sel *SelectStmt, sc *r32nScope, w *r32nDQWalk) {
	if sel == nil || !w.ok {
		return
	}
	if w.depth++; w.depth > r32nMaxDepth {
		// A view whose body reads from itself is creatable (it only fails when
		// queried), so this recursion needs a floor.
		w.ok = false
		return
	}
	defer func() { w.depth-- }()
	cteNames := make(map[string]bool, len(sel.CTEs))
	for _, cte := range sel.CTEs {
		cteNames[r33sFoldIdent(cte.Name)] = true
		for _, cn := range cte.ColNames {
			sc.addName(cn)
		}
		// The CTE body resolves in its own child scope; the names a FROM item
		// referencing the CTE sees come from r32nAddDerivedNames, exactly like
		// a derived table's.
		db.r32nCollectSelect(cte.Select, r32nNewScope(sc), w)
		db.r32nAddDerivedNames(cte.Select, sc, w)
	}
	for _, f := range sel.From {
		sc.addName(f.Alias)
		for _, u := range f.Using {
			sc.addName(u)
		}
		db.r32nCollectExpr(f.On, sc, w)
		for _, a := range f.TableFuncArgs {
			db.r32nCollectExpr(a, sc, w)
		}
		if f.Subquery != nil {
			db.r32nCollectSelect(f.Subquery, r32nNewScope(sc), w)
			db.r32nAddDerivedNames(f.Subquery, sc, w)
			continue
		}
		if f.TableFunc {
			// A table-valued function's output columns are the module's, which
			// this pass has no way to enumerate.
			w.ok = false
			return
		}
		if f.Table == "" || cteNames[r33sFoldIdent(f.Table)] {
			continue
		}
		if f.Schema != "" && !equalFoldName(f.Schema, "main") && !equalFoldName(f.Schema, "temp") {
			// An ATTACHed database's table. Resolving its bare name against
			// THIS file's catalogs could hand back a same-named local table
			// with FEWER columns, which is the one direction that demotes a
			// token SQLite keeps.
			w.ok = false
			return
		}
		if !db.r32nAddSourceNames(f.Table, sc, w) {
			return
		}
	}
	for _, c := range sel.Columns {
		if c.HasAlias {
			sc.addName(c.Alias)
		}
		db.r32nCollectExpr(c.Expr, sc, w)
	}
	db.r32nCollectExpr(sel.Where, sc, w)
	for _, g := range sel.GroupBy {
		db.r32nCollectExpr(g, sc, w)
	}
	db.r32nCollectExpr(sel.Having, sc, w)
	for _, t := range sel.OrderBy {
		db.r32nCollectExpr(t.Expr, sc, w)
	}
	for _, nw := range sel.Windows {
		db.r32nCollectWindow(nw.Spec, sc, w)
	}
	db.r32nCollectExpr(sel.LimitParam, sc, w)
	db.r32nCollectExpr(sel.OffsetParam, sc, w)
	for _, arm := range sel.Compound {
		db.r32nCollectSelect(arm.Stmt, sc, w)
	}
}

// r32nMaxDepth bounds the scope recursion. Nothing legitimate nests this deep;
// the bound exists so a self-referencing view (creatable, and only an error
// once queried) cannot spin.
const r32nMaxDepth = 64

// r32nAddDerivedNames unions every name reachable inside sub into sc (r32nScope's
// over-approximation), discarding references it passes. It inherits w's depth, or
// "CREATE VIEW v AS SELECT * FROM v" would recurse forever.
func (db *DB) r32nAddDerivedNames(sub *SelectStmt, sc *r32nScope, w *r32nDQWalk) {
	if sub == nil || !w.ok {
		return
	}
	names := &r32nDQWalk{ok: true, depth: w.depth}
	db.r32nCollectSelect(sub, sc, names)
	if !names.ok {
		w.ok = false
	}
}

// r32nAddSourceNames adds every column a FROM-clause name contributes. BOTH
// catalogs are unioned deliberately: which one a bare name resolves in depends
// on temp shadowing, and taking only the shadow could produce a SMALLER name
// set than the one really in scope -- the one direction that would demote a
// token SQLite keeps. It returns false (clearing w.ok) for anything whose
// column list this pass cannot enumerate exactly: an unknown name, or a
// virtual table, whose hidden columns resolve without appearing in any
// declared column list.
func (db *DB) r32nAddSourceNames(name string, sc *r32nScope, w *r32nDQWalk) bool {
	found := false
	for _, isTemp := range [2]bool{false, true} {
		scope := createScope(isTemp)
		if db.findVtabMetaIn(scope, name) != nil {
			w.ok = false
			return false
		}
		if t := db.findTableMetaIn(scope, name); t != nil {
			found = true
			for _, c := range t.cols {
				sc.addName(c.Name)
			}
			if !t.withoutRowid {
				sc.addRowidNames()
			}
		}
		if v := db.findViewMetaIn(scope, name); v != nil {
			found = true
			for _, cn := range v.colNames {
				sc.addName(cn)
			}
			// A view contributes its BODY's output names, over-approximated the
			// same way a derived table's are. Its own double-quoted references
			// are not collected here: the view is its own schema row and gets
			// its own pass.
			db.r32nAddDerivedNames(v.selectStmt, sc, w)
			if !w.ok {
				return false
			}
		}
	}
	if !found {
		w.ok = false
	}
	return found
}

// r32nQuotefixTable is renameQuotefixFunc's ordinary-table branch
// (alter.c:1978): walk pCheck and every generated-column expression, resolving
// against the table's OWN columns and nothing else.
func (db *DB) r32nQuotefixTable(t *tableMeta) ([]r32nQuoteFix, bool) {
	cols, checks, _, err := parseCreateTableColumnsAndAutoIndexes(t.sql, t.withoutRowid)
	if err != nil {
		return nil, false
	}
	own := r32nNewScope(nil)
	for _, c := range cols {
		own.addName(c.Name)
	}
	// NC_IsCheck is not in resolve.c:625's exclusion mask, so "rowid" resolves
	// inside a CHECK on a rowid table; NC_GenCol IS in it, so the same spelling
	// does NOT resolve inside a generated column. Two scopes, differing in
	// exactly that, over the same declared columns.
	checkScope := r32nNewScope(own)
	if !t.withoutRowid {
		checkScope.addRowidNames()
	}
	genScope := r32nNewScope(own)
	w := r32nNewWalk()
	for _, cc := range checks {
		e, perr := r32nParseSpannedExpr(cc.exprText, cc.r32nBodyOff)
		if perr != nil {
			return nil, false
		}
		db.r32nCollectExpr(e, checkScope, w)
	}
	for _, c := range cols {
		if c.GeneratedExpr == "" {
			continue
		}
		e, perr := r32nParseSpannedExpr(c.GeneratedExpr, c.R32NGeneratedOff)
		if perr != nil {
			return nil, false
		}
		db.r32nCollectExpr(e, genScope, w)
	}
	if !w.ok {
		return nil, false
	}
	return w.unresolved(), true
}

// r32nQuotefixIndex is renameQuotefixFunc's sParse.pNewIndex branch
// (alter.c:1992): walk aColExpr and pPartIdxWhere against the base table's
// columns. A bare double-quoted KEY reaches the same walker, because
// sqlite3StringToId leaves it a TK_ID (build.c:4217) and the resolver then
// demotes it exactly like any other unresolvable double-quoted identifier --
// which is what demoteDoubleQuotedIndexColumns already reproduces for the
// index's own shape (index_write.go).
func (db *DB) r32nQuotefixIndex(ix *indexMeta) ([]r32nQuoteFix, bool) {
	tbl := db.findTableMetaIn(createScope(ix.isTemp), ix.table)
	if tbl == nil {
		return nil, false
	}
	stmt, err := r32nParseCreateIndexStmt(ix.sql, true)
	if err != nil {
		return nil, false
	}
	own := r32nNewScope(nil)
	for _, c := range tbl.cols {
		own.addName(c.Name)
	}
	keyScope := r32nNewScope(own) // NC_IdxExpr: no rowid (resolve.c:625)
	w := r32nNewWalk()
	for i, e := range stmt.exprs {
		if e == nil {
			// A plain-column key; only the double-quoted spelling can demote.
			if i < len(stmt.r32nDQSpan) && stmt.r32nDQSpan[i].end > stmt.r32nDQSpan[i].start {
				w.pending = append(w.pending, r32nPendingFix{
					fix:   r32nQuoteFix{span: stmt.r32nDQSpan[i], name: stmt.cols[i]},
					scope: keyScope,
				})
			}
			continue
		}
		db.r32nCollectExpr(e, keyScope, w)
	}
	if stmt.where != nil {
		// NC_PartIdx is not in resolve.c:625's exclusion mask, so a partial
		// index's WHERE can name the rowid where a key expression cannot.
		whereScope := r32nNewScope(own)
		if !tbl.withoutRowid {
			whereScope.addRowidNames()
		}
		db.r32nCollectExpr(stmt.where, whereScope, w)
	}
	if !w.ok {
		return nil, false
	}
	fixes := w.unresolved()
	return fixes, true
}

// r32nQuotefixView is renameQuotefixFunc's IsView branch (alter.c:1968): prep
// the whole SELECT and walk it.
func (db *DB) r32nQuotefixView(v *viewMeta) ([]r32nQuoteFix, bool) {
	sel, err := r32nParseSchemaViewSelect(v.sql)
	if err != nil {
		return nil, false
	}
	w := r32nNewWalk()
	db.r32nCollectSelect(sel, r32nNewScope(nil), w)
	if !w.ok {
		return nil, false
	}
	return w.unresolved(), true
}

// r32nReparseTriggerDQ re-parses a stored trigger's WHEN and body with
// double-quoted span recording on, reusing parseTriggerBody. The body starts at
// the first unquoted BEGIN (no earlier header position accepts one) and the WHEN
// is the first unquoted WHEN before it. Anything that fails is left alone.
func r32nReparseTriggerDQ(tr *triggerMeta) (when Expr, body []triggerBodyStmt, ok bool) {
	toks, err := lex(tr.sql)
	if err != nil {
		return nil, nil, false
	}
	beginIdx, whenIdx := -1, -1
	for i, t := range toks {
		if t.kind != tkIdent || t.quoted {
			continue
		}
		switch t.upper() {
		case "BEGIN":
			beginIdx = i
		case "WHEN":
			if whenIdx < 0 {
				whenIdx = i
			}
		}
		if beginIdx >= 0 {
			break
		}
	}
	if beginIdx < 0 {
		return nil, nil, false
	}
	p := newParser(tr.sql, toks)
	p.r32nDQRecord = true
	p.inTriggerBody = true // RAISE(...) is legal from the WHEN clause on
	if whenIdx >= 0 {
		p.pos = whenIdx + 1
		when, err = p.parseExpr()
		if err != nil {
			return nil, nil, false
		}
	}
	p.pos = beginIdx + 1
	body, err = parseTriggerBody(p, tr.event)
	if err != nil {
		return nil, nil, false
	}
	return when, body, true
}

// r32nQuotefixTrigger is renameQuotefixFunc's trigger branch (alter.c:1996), with
// renameResolveTrigger's scopes (alter.c:1289):
//
//   - WHEN resolves with no source list, and NEW/OLD need a qualifier
//     (resolve.c:522), so every bare double-quoted name in WHEN demotes
//     (`WHEN new.a <> "zz"` becomes `<> 'zz'`).
//   - a SELECT step's scope is its own FROM; VALUES has none, so
//     `VALUES("lit")` becomes `VALUES('lit')`.
//   - UPDATE/DELETE resolve SET and WHERE against the target table:
//     `UPDATE t2 SET q = "p" WHERE q = "zz"` keeps "p" and demotes "zz".
//   - an INSERT column list is an IdList the walker never sees.
func (db *DB) r32nQuotefixTrigger(tr *triggerMeta) ([]r32nQuoteFix, bool) {
	when, body, ok := r32nReparseTriggerDQ(tr)
	if !ok {
		return nil, false
	}
	w := r32nNewWalk()
	db.r32nCollectExpr(when, r32nNewScope(nil), w)
	// A step's target-table scope. The trigger's own catalog is not consulted:
	// r32nAddSourceNames unions both, which is the safe direction.
	target := func(name, alias string) (*r32nScope, bool) {
		sc := r32nNewScope(nil)
		if !db.r32nAddSourceNames(name, sc, w) {
			return nil, false
		}
		sc.addName(alias)
		return sc, true
	}
	for _, bs := range body {
		switch {
		case bs.sel != nil:
			db.r32nCollectSelect(bs.sel, r32nNewScope(nil), w)
		case bs.insert != nil:
			if bs.insert.upsert != nil {
				// An upsert resolves its target/SET/WHERE against pUpsertSrc
				// with NC_UUpsert (alter.c:1362), a scope this pass does not
				// model.
				return nil, false
			}
			for _, row := range bs.insert.rows {
				for _, e := range row {
					db.r32nCollectExpr(e, r32nNewScope(nil), w)
				}
			}
			db.r32nCollectSelect(bs.insert.selectStmt, r32nNewScope(nil), w)
		case bs.update != nil:
			if len(bs.update.from) > 0 || len(bs.update.ctes) > 0 {
				return nil, false // extra sources this scope model does not carry
			}
			sc, tok := target(bs.update.table, bs.update.alias)
			if !tok {
				return nil, false
			}
			for _, a := range bs.update.sets {
				db.r32nCollectExpr(a.expr, sc, w)
			}
			db.r32nCollectExpr(bs.update.where, sc, w)
		case bs.delete != nil:
			if len(bs.delete.ctes) > 0 {
				return nil, false
			}
			sc, tok := target(bs.delete.table, bs.delete.alias)
			if !tok {
				return nil, false
			}
			db.r32nCollectExpr(bs.delete.where, sc, w)
		}
		if !w.ok {
			return nil, false
		}
	}
	if !w.ok {
		return nil, false
	}
	return w.unresolved(), true
}

// r32nQuotefixUndo puts back everything r32nRenameFixQuotes changed. The
// quotefix runs BEFORE the rename, but RENAME COLUMN / DROP COLUMN can still
// decline afterwards (a trigger cascade this write path cannot do, a view it
// cannot rewrite), and a declined ALTER must change nothing.
type r32nQuotefixUndo struct {
	tables   []r32nSavedTable
	indexes  []r32nSavedIndex
	views    []r32nSavedView
	triggers []r32nSavedTrigger
}

type r32nSavedTable struct {
	t      *tableMeta
	sql    string
	checks []checkConstraint
}

type r32nSavedIndex struct {
	ix    *indexMeta
	sql   string
	keys  []indexKey
	where Expr
}

type r32nSavedView struct {
	v   *viewMeta
	sql string
	sel *SelectStmt
}

type r32nSavedTrigger struct {
	tr   *triggerMeta
	snap triggerMeta
}

func (u *r32nQuotefixUndo) restore() {
	if u == nil {
		return
	}
	for _, s := range u.tables {
		s.t.sql, s.t.checks = s.sql, s.checks
	}
	for _, s := range u.indexes {
		s.ix.sql, s.ix.keys, s.ix.where = s.sql, s.keys, s.where
	}
	for _, s := range u.views {
		s.v.sql, s.v.selectStmt = s.sql, s.sel
	}
	for _, s := range u.triggers {
		*s.tr = s.snap
	}
}

// r32nQuotefixSkips reports whether renameFixQuotes' own WHERE clause excludes
// this schema row: `name NOT LIKE 'sqliteX_%' ESCAPE 'X' AND sql NOT LIKE
// 'create virtual%'` (alter.c:90). An empty sql is an automatic index, which
// has no stored row of its own at all. The `"` test is not SQLite's, but a
// row with no double-quote character in it has nothing this pass could
// possibly rewrite, and skipping it keeps the whole pass inert -- and free --
// for every schema that never used the misfeature.
func r32nQuotefixSkips(name, sqlText string) bool {
	return sqlText == "" ||
		strings.HasPrefix(r33sFoldIdent(name), "sqlite_") ||
		strings.HasPrefix(r33sFoldIdent(sqlText), "create virtual") ||
		!strings.ContainsRune(sqlText, '"')
}

// r32nRenameFixQuotes is renameFixQuotes (alter.c:646): runs the quotefix over
// every schema row it reaches and applies the edits, returning the undo record.
// Reach is C's: the altered table's database, plus temp unless the table is temp.
// Objects whose analysis cannot complete (r32nDQWalk.ok false) are left alone,
// which is what happened before this pass existed. verb is the "when" clause for
// r32nIndexKeyDemotionError.
func (db *DB) r32nRenameFixQuotes(tbl *tableMeta, verb string) (*r32nQuotefixUndo, error) {
	undo := &r32nQuotefixUndo{}
	fail := func(err error) (*r32nQuotefixUndo, error) {
		undo.restore()
		return nil, err
	}
	for _, t := range db.tables {
		if alterSkipsCatalog(tbl.isTemp, t.isTemp) || r32nQuotefixSkips(t.name, t.sql) {
			continue
		}
		fixes, ok := db.r32nQuotefixTable(t)
		if !ok || len(fixes) == 0 {
			continue
		}
		undo.tables = append(undo.tables, r32nSavedTable{t: t, sql: t.sql, checks: t.checks})
		t.sql = applyEdits(t.sql, r32nQuoteEdits(t.sql, fixes))
		if err := refreshTableChecks(t); err != nil {
			return fail(err)
		}
	}
	for _, ix := range db.indexes {
		if alterSkipsCatalog(tbl.isTemp, ix.isTemp) || r32nQuotefixSkips(ix.name, ix.sql) {
			continue
		}
		fixes, ok := db.r32nQuotefixIndex(ix)
		if !ok || len(fixes) == 0 {
			continue
		}
		if err := db.r32nIndexKeyDemotionError(ix, fixes, tbl, verb); err != nil {
			return fail(err)
		}
		ixTbl := db.findTableMetaIn(createScope(ix.isTemp), ix.table)
		undo.indexes = append(undo.indexes, r32nSavedIndex{ix: ix, sql: ix.sql, keys: ix.keys, where: ix.where})
		ix.sql = applyEdits(ix.sql, r32nQuoteEdits(ix.sql, fixes))
		if ixTbl != nil {
			if err := refreshExprIndex(ix, ixTbl); err != nil {
				return fail(err)
			}
		}
	}
	for _, v := range db.views {
		if alterSkipsCatalog(tbl.isTemp, v.isTemp) || r32nQuotefixSkips(v.name, v.sql) {
			continue
		}
		fixes, ok := db.r32nQuotefixView(v)
		if !ok || len(fixes) == 0 {
			continue
		}
		newSQL := applyEdits(v.sql, r32nQuoteEdits(v.sql, fixes))
		stmt, perr := parseCreateViewStmt(newSQL)
		if perr != nil {
			return fail(fmt.Errorf("engine: error in view %s: %w", v.name, perr))
		}
		undo.views = append(undo.views, r32nSavedView{v: v, sql: v.sql, sel: v.selectStmt})
		v.sql, v.selectStmt = newSQL, stmt.selectStmt
	}
	for _, tr := range db.triggers {
		if alterSkipsCatalog(tbl.isTemp, tr.isTemp) || r32nQuotefixSkips(tr.name, tr.sql) {
			continue
		}
		fixes, ok := db.r32nQuotefixTrigger(tr)
		if !ok || len(fixes) == 0 {
			continue
		}
		newSQL := applyEdits(tr.sql, r32nQuoteEdits(tr.sql, fixes))
		stmt, perr := parseCreateTriggerStmt(newSQL)
		if perr != nil {
			return fail(fmt.Errorf("engine: error in trigger %s: %w", tr.name, perr))
		}
		undo.triggers = append(undo.triggers, r32nSavedTrigger{tr: tr, snap: *tr})
		tr.sql, tr.when, tr.body = newSQL, stmt.when, stmt.body
	}
	return undo, nil
}

// r32nIndexKeyDemotionError reproduces the one way the quotefix fails an ALTER.
// A bare double-quoted index key (`ON t1("val")`) is legal because it demotes to
// a string; quotefixing writes 'val', and the next re-parse turns that TK_STRING
// key back into an identifier without EP_DblQuoted (build.c:4217), so it is
// "no such column" (resolve.c:721). With `ON t1(a || "str", "b", "val")`,
// `RENAME c TO ccc` fails "error in index i1: no such column: val". The "when"
// clause depends on which statement re-parses first: RENAME COLUMN's own UPDATE
// covers the table's indexes with none (alter.c:657); otherwise " after rename"
// or " after drop column" (alter.c:677, 2318).
func (db *DB) r32nIndexKeyDemotionError(ix *indexMeta, fixes []r32nQuoteFix, tbl *tableMeta, verb string) error {
	stmt, err := r32nParseCreateIndexStmt(ix.sql, true)
	if err != nil {
		return nil
	}
	for i, e := range stmt.exprs {
		if e != nil || i >= len(stmt.r32nDQSpan) {
			continue
		}
		span := stmt.r32nDQSpan[i]
		if span.end <= span.start || !r32nHasFixAt(fixes, span) {
			continue
		}
		// sqlite3ExprIdToTrueFalse (resolve.c:746) rescues exactly these two
		// spellings after the demotion branch declines them.
		if equalFoldName(stmt.cols[i], "true") || equalFoldName(stmt.cols[i], "false") {
			continue
		}
		when := " " + verb
		if verb == "after rename" && ix.isTemp == tbl.isTemp && equalFoldName(ix.table, tbl.name) {
			when = ""
		}
		return fmt.Errorf("engine: error in index %s%s: no such column: %s", ix.name, when, stmt.cols[i])
	}
	return nil
}

func r32nHasFixAt(fixes []r32nQuoteFix, span byteSpan) bool {
	for _, f := range fixes {
		if f.span == span {
			return true
		}
	}
	return false
}

// r45FlatViewColumnRefSkips returns the spans of identifier tokens in a view body
// that spell oldName but are not references to the table oldName (bare column
// references a token rewrite would corrupt), or ok=false when unproven. C matches
// FROM items by resolved identity (renameTableSelectCb) and leaves same-named
// columns alone ("CREATE TABLE t(a, t); CREATE VIEW v AS SELECT t FROM t").
//
// The proof: table-name positions scanned from the FROM clause must equal the
// parser's From list exactly, in order, and the body must be flat (one SELECT, no
// CTE, no subquery FROM item). A qualifier ("t.x") is a table reference and is
// rewritten.
func r45FlatViewColumnRefSkips(sqlText, oldName string) (skips []byteSpan, ok bool) {
	stmt, perr := parseCreateViewStmt(sqlText)
	if perr != nil || stmt == nil || stmt.selectStmt == nil {
		return nil, false
	}
	sel := stmt.selectStmt
	if len(sel.CTEs) != 0 || len(sel.Compound) != 0 || len(sel.From) == 0 {
		return nil, false
	}
	aliasShadows := false
	for _, it := range sel.From {
		if it.Subquery != nil || it.TableFunc || it.Table == "" {
			return nil, false
		}
		// A FROM item ALIASED to oldName makes every "oldName.x" qualifier
		// name the ALIAS, which resolves to a DIFFERENT table and which real
		// SQLite therefore leaves alone. Rewriting it is a wrong answer, and
		// the scan below cannot tell the two apart -- verified against 3.53.3
		// over "CREATE TABLE t(a, t); CREATE TABLE o(a); CREATE VIEW v AS
		// SELECT t.a FROM o AS t", where "ALTER TABLE t RENAME TO t2" leaves
		// the view's body byte-identical. Refused outright.
		if equalFoldName(it.Alias, oldName) {
			aliasShadows = true
		}
	}
	if aliasShadows {
		// Every "oldName.x" here names the ALIAS. If NO FROM item is the
		// table itself, the body holds no reference to it at all and real
		// SQLite rewrites nothing -- so skip every token and leave the view
		// byte-identical, which is exactly what 3.53.3 does. With BOTH an
		// alias and a real reference present the qualifiers are genuinely
		// ambiguous to a token scan, so that declines.
		for _, it := range sel.From {
			if equalFoldName(it.Table, oldName) {
				return nil, false
			}
		}
		toks, lerr := lex(sqlText)
		if lerr != nil {
			return nil, false
		}
		for _, t := range toks {
			if t.kind == tkIdent && equalFoldName(t.text, oldName) {
				skips = append(skips, byteSpan{t.Start, t.End})
			}
		}
		return skips, true
	}
	toks, lerr := lex(sqlText)
	if lerr != nil {
		return nil, false
	}
	// Exactly one SELECT keyword: any second one is a subquery with a FROM
	// clause of its own that this scan would not reach.
	selects := 0
	for _, t := range toks {
		if t.kind == tkIdent && !t.quoted && t.upper() == "SELECT" {
			selects++
		}
	}
	if selects != 1 {
		return nil, false
	}
	names, refs, sok := r45ScanFlatFromClause(toks)
	if !sok || len(names) != len(sel.From) {
		return nil, false
	}
	for i := range names {
		if !equalFoldName(names[i], sel.From[i].Table) {
			return nil, false // the scan and the parser disagree: do not trust it
		}
	}
	isRef := map[int]bool{}
	for _, r := range refs {
		isRef[r] = true
	}
	for i, t := range toks {
		if t.kind != tkIdent || !equalFoldName(t.text, oldName) || isRef[i] {
			continue
		}
		if r33rQualifierToken(toks, i) {
			continue // "t.x" -- a table reference, and rewritten as one
		}
		skips = append(skips, byteSpan{t.Start, t.End})
	}
	return skips, true
}

// r45FlatViewNameStrings is the string half: tokens in a flat view body spelling
// oldName as a string that are real FROM-item names, to be rewritten (C treats
// both spellings of a name alike, alter.c:1725). ok is false for anything not
// modelled; the scan must agree with the parser's From list.
func r45FlatViewNameStrings(sqlText, oldName string) (names []token, ok bool) {
	stmt, perr := parseCreateViewStmt(sqlText)
	if perr != nil || stmt == nil || stmt.selectStmt == nil {
		return nil, false
	}
	sel := stmt.selectStmt
	if len(sel.CTEs) != 0 || len(sel.Compound) != 0 || len(sel.From) == 0 {
		return nil, false
	}
	for _, it := range sel.From {
		if it.Subquery != nil || it.TableFunc || it.Table == "" {
			return nil, false
		}
	}
	toks, lerr := lex(sqlText)
	if lerr != nil {
		return nil, false
	}
	selects := 0
	for _, t := range toks {
		if t.kind == tkIdent && !t.quoted && t.upper() == "SELECT" {
			selects++
		}
	}
	if selects != 1 {
		return nil, false
	}
	scanned, refs, sok := r45ScanFlatFromClause(toks)
	if !sok || len(scanned) != len(sel.From) {
		return nil, false
	}
	for i := range scanned {
		if !equalFoldName(scanned[i], sel.From[i].Table) {
			return nil, false
		}
	}
	for i, r := range refs {
		if toks[r].kind == tkString && equalFoldName(scanned[i], oldName) {
			names = append(names, toks[r])
		}
	}
	return names, true
}

// r45ScanFlatFromClause reads the ONE top-level FROM clause out of toks and
// reports each table-NAME it finds plus that name's token index. ok is false
// for anything it does not model -- no FROM at all, a parenthesized join
// group, a nested paren, or a name position it cannot read -- so the caller
// falls back rather than trusting a partial scan.
func r45ScanFlatFromClause(toks []token) (names []string, idx []int, ok bool) {
	i := 0
	for ; i < len(toks); i++ {
		if toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == "FROM" {
			break
		}
	}
	if i >= len(toks) {
		return nil, nil, false
	}
	expectName := true
	for i++; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tkEOF {
			break
		}
		if t.kind == tkPunct && t.text == "(" {
			return nil, nil, false // a parenthesized join group is not modelled
		}
		if t.kind == tkIdent && !t.quoted {
			switch t.upper() {
			case "WHERE", "GROUP", "ORDER", "LIMIT", "HAVING", "WINDOW",
				"UNION", "EXCEPT", "INTERSECT":
				return names, idx, true
			case "JOIN":
				expectName = true
				continue
			case "NATURAL", "LEFT", "RIGHT", "FULL", "INNER", "CROSS", "OUTER":
				continue
			case "ON", "USING", "AS", "INDEXED", "NOT":
				expectName = false
				continue
			}
		}
		if t.kind == tkPunct && t.text == "," {
			expectName = true
			continue
		}
		if !expectName || !isNameToken(t) {
			continue
		}
		// "schema.name": the NAME is the token after the dot.
		if i+2 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." && isNameToken(toks[i+2]) {
			i += 2
			t = toks[i]
		}
		// A FROM item spelled as a STRING is still an "nm" (parse.y:340), so
		// its NAME is the decoded value -- "'a'" names table a. Recording the
		// raw spelling instead made the caller's agreement check against the
		// parser fail for every such item, which is a decline, and for the
		// COLUMN cascade a stale view body.
		nm := t.text
		if t.kind == tkString {
			nm = t.str
		}
		names = append(names, nm)
		idx = append(idx, i)
		expectName = false
	}
	return names, idx, true
}

// r45TriggerColumnRefSkips is r45FlatViewColumnRefSkips for a trigger body. The
// proof is a count agreement: the parser's count of table references to oldName
// (ON clause, step targets via triggerStepTargetsNamed, every reachable SELECT's
// FROM items) must equal the scan's count of table-name positions. A miss or a
// misclassified column changes the count, so only exact agreement is trusted.
func r45TriggerColumnRefSkips(tr *triggerMeta, oldName string) (skips []byteSpan, ok bool) {
	// A FROM item ALIASED to oldName would make a "t.x" qualifier name the
	// ALIAS, not the table, and both counts would agree on rewriting it --
	// the one shape where agreement would still be wrong. Refused outright.
	if r45TriggerAliasShadows(tr, oldName) {
		return nil, false
	}
	want := r45TriggerFromTablesNamed(tr, oldName) +
		r45TriggerQualifiersNamed(tr, oldName) +
		len(triggerStepTargetsNamed(tr, oldName))
	if equalFoldName(tr.table, oldName) {
		want++
	}
	toks, lerr := lex(tr.sql)
	if lerr != nil {
		return nil, false
	}
	ownName, hasOwn := triggerOwnNameSpan(tr.sql)
	got := 0
	for i, t := range toks {
		// A STRING spelling counts too: C SQLite accepts one wherever a
		// table name is expected ("INSERT INTO 'a'"), and the parser's own
		// count above includes it -- so a scan that looked only at identifiers
		// came up SHORT and refused a rename it could have proven. See
		// r45TriggerNameStrings, which reads the same two counts for the
		// rewrite side.
		text := t.text
		switch t.kind {
		case tkIdent:
		case tkString:
			text = t.str
		default:
			continue
		}
		if !equalFoldName(text, oldName) {
			continue
		}
		if hasOwn && t.Start == ownName.start {
			continue // the trigger's OWN name is never a reference to the table
		}
		if r45TriggerTableNamePosition(toks, i) {
			got++
			continue
		}
		skips = append(skips, byteSpan{t.Start, t.End})
	}
	if got != want {
		return nil, false
	}
	return skips, true
}

// r45TriggerNameStrings is the string half of r45TriggerColumnRefSkips: which
// string tokens spelling oldName are table-name positions ("ON 't8'", "INSERT INTO
// 't8'") and which are values to leave alone ("VALUES(new.a, 'a')"), using the
// same count agreement over identifier and string tokens together.
func r45TriggerNameStrings(tr *triggerMeta, oldName string) (names []token, ok bool) {
	if r45TriggerAliasShadows(tr, oldName) {
		return nil, false
	}
	toks, lerr := lex(tr.sql)
	if lerr != nil {
		return nil, false
	}
	want := r45TriggerFromTablesNamed(tr, oldName) +
		r45TriggerQualifiersNamed(tr, oldName) +
		len(triggerStepTargetsNamed(tr, oldName))
	if equalFoldName(tr.table, oldName) {
		want++
	}
	ownName, hasOwn := triggerOwnNameSpan(tr.sql)
	got := 0
	for i, t := range toks {
		text := t.text
		switch t.kind {
		case tkIdent:
		case tkString:
			text = t.str
		default:
			continue
		}
		if !equalFoldName(text, oldName) {
			continue
		}
		if hasOwn && t.Start == ownName.start {
			continue // the trigger's OWN name is never a reference to the table
		}
		if !r45TriggerTableNamePosition(toks, i) {
			continue
		}
		got++
		if t.kind == tkString {
			names = append(names, t)
		}
	}
	if got != want {
		return nil, false
	}
	return names, true
}

// r45TriggerTableNamePosition reports whether toks[i] sits where only a TABLE
// name can: straight after FROM/JOIN/INTO/UPDATE, or as the qualifier of a
// "t.x" reference. Anything else -- a select-list item, a WHERE operand, an
// ORDER BY term -- is a column reference however it is spelled. A position this
// does not model simply is not counted, which the caller's count agreement
// turns into a decline rather than a mis-rewrite.
func r45TriggerTableNamePosition(toks []token, i int) bool {
	if i+1 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." {
		return true
	}
	j := i - 1
	// Step back over a schema qualifier ("main.t").
	if j >= 1 && toks[j].kind == tkPunct && toks[j].text == "." {
		j -= 2
	}
	if j < 0 || toks[j].kind != tkIdent || toks[j].quoted {
		return false
	}
	switch toks[j].upper() {
	case "FROM", "JOIN", "INTO", "UPDATE":
		return true
	case "ON":
		// "ON" introduces a TABLE only in the trigger's HEADER ("... ON t2",
		// or schema-qualified "... ON main.t2"). Past the header it is a join
		// CONSTRAINT, where "ON t1.a = t2.p" is a COLUMN reference -- and
		// calling that a table name left the join's own "t1.a" unrenamed while
		// C rewrote it (alter_r34v_trigger_cascade_test.go's inner-join-on
		// cases, which catch it in all three WHEN spellings).
		return i < r45TriggerBodyStart(toks)
	}
	return false
}

// r45TriggerBodyStart is the token index where a CREATE TRIGGER's header ends:
// its WHEN clause or its BEGIN, whichever comes first. Everything from there on
// is SQL that can carry its own joins, subqueries and column references.
func r45TriggerBodyStart(toks []token) int {
	for i, t := range toks {
		if t.kind != tkIdent || t.quoted {
			continue
		}
		switch t.upper() {
		case "WHEN", "BEGIN":
			return i
		}
	}
	return len(toks)
}

// r45TriggerFromTablesNamed counts the FROM items spelling name in every
// SELECT reachable from tr's body. An UNDER-count is safe: it makes the
// caller's agreement test fail, which declines.
func r45TriggerFromTablesNamed(tr *triggerMeta, name string) int {
	return r45TriggerWalk(tr, name, false, false)
}

// r45TriggerQualifiersNamed counts the "name.x" QUALIFIERS in tr's body. Real
// SQLite rewrites those with the FROM item (renameTableExprCb matches a
// TK_COLUMN's table, alter.c), so they are table references and belong in the
// agreement count.
func r45TriggerQualifiersNamed(tr *triggerMeta, name string) int {
	return r45TriggerWalk(tr, name, true, false)
}

// r45TriggerAliasShadows reports whether any FROM item in tr's body is ALIASED
// to name.
func r45TriggerAliasShadows(tr *triggerMeta, name string) bool {
	return r45TriggerWalk(tr, name, false, true) > 0
}

// r45TriggerWalk is the one traversal the three above share: it counts FROM
// items spelling name, or "name." qualifiers, or FROM items ALIASED to name.
func r45TriggerWalk(tr *triggerMeta, name string, quals, aliases bool) int {
	n := 0
	var sel func(s *SelectStmt)
	var expr func(e Expr)
	expr = func(e Expr) {
		switch x := e.(type) {
		case nil:
			return
		case ColumnExpr:
			if quals && equalFoldName(x.Qualifier, name) {
				n++
			}
		case SubqueryExpr:
			sel(x.Stmt)
		case ExistsExpr:
			sel(x.Stmt)
		case InExpr:
			sel(x.Sub)
		}
		walkExprOperands(e, expr)
	}
	sel = func(s *SelectStmt) {
		if s == nil {
			return
		}
		for _, it := range s.From {
			if it.Subquery != nil {
				sel(it.Subquery)
				continue
			}
			switch {
			case aliases && equalFoldName(it.Alias, name):
				n++
			case !quals && !aliases && equalFoldName(it.Table, name):
				n++
			}
			expr(it.On)
		}
		for _, c := range s.Columns {
			expr(c.Expr)
		}
		expr(s.Where)
		expr(s.Having)
		for _, g := range s.GroupBy {
			expr(g)
		}
		for _, o := range s.OrderBy {
			expr(o.Expr)
		}
		for i := range s.CTEs {
			sel(s.CTEs[i].Select)
		}
		for i := range s.Compound {
			sel(s.Compound[i].Stmt)
		}
	}
	for i := range tr.body {
		bs := &tr.body[i]
		if bs.sel != nil {
			sel(bs.sel)
		}
		if bs.insert != nil {
			sel(bs.insert.selectStmt)
		}
	}
	return n
}

// r45ViewColumnRenameSkips is r45FlatViewColumnRefSkips for RENAME COLUMN: the
// spans a column rename must not touch in a view body, or ok=false. Each token
// spelling oldName in a column position is attributed to a FROM item: a
// qualified one by its qualifier (alias or table name), a bare one to the single
// FROM table with that column (two candidates means false). hasCol supplies the
// schema knowledge. "SELECT t.a FROM t JOIN o ON t.a=o.a" renaming t.a becomes
// "SELECT t.q FROM t JOIN o ON t.q=o.a".
func r45ViewColumnRenameSkips(sqlText, tblName, oldName string, hasCol func(table, col string) bool) (skips []byteSpan, ok bool) {
	stmt, perr := parseCreateViewStmt(sqlText)
	if perr != nil || stmt == nil || stmt.selectStmt == nil {
		return nil, false
	}
	sel := stmt.selectStmt
	if len(sel.CTEs) != 0 || len(sel.Compound) != 0 || len(sel.From) == 0 {
		return nil, false
	}
	// name-or-alias -> table, plus the set of FROM tables that carry oldName.
	owner := map[string]string{}
	carriers := 0
	var soleCarrier string
	for _, it := range sel.From {
		if it.Subquery != nil || it.TableFunc || it.Table == "" {
			return nil, false
		}
		key := it.Table
		if it.Alias != "" {
			key = it.Alias
		}
		if _, dup := owner[r33sFoldIdent(key)]; dup {
			return nil, false // two items answer to one name: not attributable
		}
		owner[r33sFoldIdent(key)] = it.Table
		if hasCol(it.Table, oldName) {
			carriers++
			soleCarrier = it.Table
		}
	}
	toks, lerr := lex(sqlText)
	if lerr != nil {
		return nil, false
	}
	selects := 0
	for _, t := range toks {
		if t.kind == tkIdent && !t.quoted && t.upper() == "SELECT" {
			selects++
		}
	}
	if selects != 1 {
		return nil, false
	}
	names, refs, sok := r45ScanFlatFromClause(toks)
	if !sok || len(names) != len(sel.From) {
		return nil, false
	}
	for i := range names {
		if !equalFoldName(names[i], sel.From[i].Table) {
			return nil, false
		}
	}
	isRef := map[int]bool{}
	for _, r := range refs {
		isRef[r] = true
	}
	for i, t := range toks {
		if t.kind != tkIdent || !equalFoldName(t.text, oldName) {
			continue
		}
		if isRef[i] {
			skips = append(skips, byteSpan{t.Start, t.End}) // a TABLE name, never this column
			continue
		}
		if i+1 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." {
			continue // a qualifier; rewriteIdentInSQL already leaves it alone
		}
		var ownedBy string
		if i >= 2 && toks[i-1].kind == tkPunct && toks[i-1].text == "." && isNameToken(toks[i-2]) {
			ownedBy = owner[r33sFoldIdent(toks[i-2].text)]
			if ownedBy == "" {
				return nil, false // a qualifier naming no FROM item of this body
			}
		} else {
			if carriers != 1 {
				return nil, false // bare and ambiguous: needs real resolution
			}
			ownedBy = soleCarrier
		}
		if !equalFoldName(ownedBy, tblName) {
			skips = append(skips, byteSpan{t.Start, t.End})
		}
	}
	return skips, true
}


// tableHasColumn reports whether the named table has that column. A name whose
// columns cannot be enumerated (view, CTE, table function) answers true, since
// callers count carriers of a bare reference and an extra one only declines: "a"
// in "SELECT a FROM tbl, vt" is ambiguous when view vt re-exposes a, which C
// refuses (resolve.c:785; TestRenameColumnViewIndirectShadowStillDeclines).
func (db *DB) tableHasColumn(table, col string) bool {
	t := db.findTableMetaIn(scopeAny, table)
	if t == nil {
		return true
	}
	for _, c := range t.cols {
		if equalFoldName(c.Name, col) {
			return true
		}
	}
	return false
}

// r45TriggerColumnRenameSkips is r45ViewColumnRenameSkips for a trigger body,
// asking a body-wide question: of every table the trigger can see (its ON table,
// each step target, every FROM item of every reachable SELECT), how many carry a
// column of this name? With exactly one, every bare reference means it. NEW./OLD.
// name the trigger's own table (INSTEAD OF ones are skipped by r33rSkips.oldNew);
// other qualifiers name a FROM item. False for anything it cannot attribute.
func r45TriggerColumnRenameSkips(tr *triggerMeta, tblName, oldName string, hasCol func(table, col string) bool) (skips []byteSpan, ok bool) {
	owner := map[string]string{} // alias-or-name -> table
	seen := map[string]bool{}
	var carriers []string
	// scoped is the table set a BARE reference can resolve against: a FROM
	// item or a step's write target. tr's OWN table is deliberately NOT in it
	// -- inside a trigger body the NEW/OLD pseudo-tables are reachable only
	// through their qualifiers, so "SELECT a FROM o" means o.a even in a
	// trigger on t. It still goes in owner, so an explicit "t.a" resolves.
	note := func(table, alias string, scoped bool) bool {
		if table == "" {
			return false
		}
		key := table
		if alias != "" {
			key = alias
		}
		k := r33sFoldIdent(key)
		if prev, dup := owner[k]; dup && !equalFoldName(prev, table) {
			return false
		}
		owner[k] = table
		if scoped && !seen[r33sFoldIdent(table)] {
			seen[r33sFoldIdent(table)] = true
			// tblName carries oldName BY CONSTRUCTION -- it is the table
			// being renamed. Asking hasCol about it is not just redundant,
			// it is wrong: this runs from renameTriggerColumnReferences,
			// AFTER the rename has landed in tbl.cols, so hasCol answers
			// about the NEW name's table and reports false, dropping the one
			// real carrier.
			if equalFoldName(table, tblName) || hasCol(table, oldName) {
				carriers = append(carriers, table)
			}
		}
		return true
	}
	if !note(tr.table, "", false) {
		return nil, false
	}
	if !r45TriggerCollectFrom(tr, func(table, alias string) bool { return note(table, alias, true) }) {
		return nil, false
	}
	toks, lerr := lex(tr.sql)
	if lerr != nil {
		return nil, false
	}
	ownName, hasOwn := triggerOwnNameSpan(tr.sql)
	ofLo, ofHi := r45TriggerUpdateOfRange(toks)
	for i, t := range toks {
		if t.kind != tkIdent || !equalFoldName(t.text, oldName) {
			continue
		}
		if hasOwn && t.Start == ownName.start {
			skips = append(skips, byteSpan{t.Start, t.End}) // the trigger's OWN name
			continue
		}
		if r45TriggerTableNamePosition(toks, i) {
			skips = append(skips, byteSpan{t.Start, t.End}) // a TABLE name, not this column
			continue
		}
		if i+1 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." {
			continue // a qualifier; a column rename already leaves those alone
		}
		var ownedBy string
		if i >= ofLo && i < ofHi {
			// The "AFTER UPDATE OF a, b" list names tr's OWN table's columns
			// and nothing else is in scope there: alter.c:1662-1665 renames
			// pNewTrigger->pColumns exactly when sParse.pTriggerTab==pTab.
			ownedBy = tr.table
		} else if i >= 2 && toks[i-1].kind == tkPunct && toks[i-1].text == "." && isNameToken(toks[i-2]) {
			q := toks[i-2].upper()
			if q == "NEW" || q == "OLD" {
				ownedBy = tr.table
			} else if ownedBy = owner[r33sFoldIdent(toks[i-2].text)]; ownedBy == "" {
				return nil, false
			}
		} else {
			if len(carriers) != 1 {
				return nil, false
			}
			ownedBy = carriers[0]
		}
		if !equalFoldName(ownedBy, tblName) {
			skips = append(skips, byteSpan{t.Start, t.End})
		}
	}
	return skips, true
}

// r45TriggerUpdateOfRange returns the token index range [lo,hi) of the column
// list in a "... UPDATE OF a, b ON tbl" trigger header, or (-1,-1) when there
// is none. Those identifiers are columns of the trigger's own table by
// definition, never of anything the body mentions (alter.c:1662-1665).
func r45TriggerUpdateOfRange(toks []token) (lo, hi int) {
	word := func(t token, w string) bool { return t.kind == tkIdent && !t.quoted && t.upper() == w }
	for j := range toks {
		if word(toks[j], "BEGIN") {
			break // past the header; a body UPDATE is a different statement
		}
		if !word(toks[j], "UPDATE") || j+1 >= len(toks) || !word(toks[j+1], "OF") {
			continue
		}
		for k := j + 2; k < len(toks); k++ {
			if word(toks[k], "ON") {
				return j + 2, k
			}
		}
		break
	}
	return -1, -1
}

// r45TriggerCollectFrom reports every table tr's body can see to note, and
// false for a FROM item it cannot model (a subquery or a table-valued
// function, whose columns are its own rather than a base table's).
func r45TriggerCollectFrom(tr *triggerMeta, note func(table, alias string) bool) bool {
	okAll := true
	var sel func(s *SelectStmt)
	var expr func(e Expr)
	expr = func(e Expr) {
		switch x := e.(type) {
		case nil:
			return
		case SubqueryExpr:
			sel(x.Stmt)
		case ExistsExpr:
			sel(x.Stmt)
		case InExpr:
			sel(x.Sub)
		}
		walkExprOperands(e, expr)
	}
	sel = func(s *SelectStmt) {
		if s == nil || !okAll {
			return
		}
		for _, it := range s.From {
			if it.Subquery != nil || it.TableFunc || it.Table == "" {
				okAll = false
				return
			}
			if !note(it.Table, it.Alias) {
				okAll = false
				return
			}
			expr(it.On)
		}
		for _, c := range s.Columns {
			expr(c.Expr)
		}
		expr(s.Where)
		expr(s.Having)
		for _, g := range s.GroupBy {
			expr(g)
		}
		for _, o := range s.OrderBy {
			expr(o.Expr)
		}
		for i := range s.CTEs {
			sel(s.CTEs[i].Select)
		}
		for i := range s.Compound {
			sel(s.Compound[i].Stmt)
		}
	}
	for i := range tr.body {
		bs := &tr.body[i]
		switch {
		case bs.insert != nil:
			if !note(bs.insert.table, bs.insert.alias) {
				return false
			}
			sel(bs.insert.selectStmt)
			for _, row := range bs.insert.rows {
				for _, e := range row {
					expr(e)
				}
			}
		case bs.update != nil:
			if !note(bs.update.table, bs.update.alias) {
				return false
			}
			for _, it := range bs.update.from {
				if it.Subquery != nil || it.TableFunc || it.Table == "" || !note(it.Table, it.Alias) {
					return false
				}
				expr(it.On)
			}
			for _, a := range bs.update.sets {
				expr(a.expr)
			}
			expr(bs.update.where)
		case bs.delete != nil:
			if !note(bs.delete.table, bs.delete.alias) {
				return false
			}
			expr(bs.delete.where)
		case bs.sel != nil:
			sel(bs.sel)
		}
		if !okAll {
			return false
		}
		if !okAll {
			return false
		}
	}
	return okAll
}

// AlterResultColumns is the result-column list C gives an ALTER TABLE whose
// program runs a nested SELECT: the constraint forms' sqlite_fail probes
// (sqlite3NestedParse at alter.c:2906, 3021 and 3030) name the statement's one
// column, so a driver that steps it through Query reports that column over an
// empty result. The FIRST probe coded names it -- for "ADD CONSTRAINT nm", the
// duplicate-name one. DROP NOT NULL and every other statement have none.
func AlterResultColumns(sqlText string) []string {
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil {
		return nil
	}
	kw := func(i int, w string) bool {
		return i < len(toks) && toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == w
	}
	if !kw(0, "ALTER") || !kw(1, "TABLE") {
		return nil
	}
	i := 3 // past the table name
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i += 2
	}
	const failed = "sqlite_fail('constraint failed', 19)" // SQLITE_CONSTRAINT
	switch {
	case kw(i, "ADD") && kw(i+1, "CONSTRAINT") && i+2 < len(toks):
		name, ok := objectNameToken(toks[i+2])
		if !ok {
			return nil
		}
		return []string{"sqlite_fail('constraint " + strings.ReplaceAll(name, "'", "''") + " already exists', 1)"} // SQLITE_ERROR
	case kw(i, "ADD") && kw(i+1, "CHECK"):
		return []string{failed}
	case kw(i, "ALTER"):
		if kw(i+1, "COLUMN") {
			i++
		}
		if kw(i+2, "SET") {
			return []string{failed}
		}
	}
	return nil
}

// renamedColumnAsWritten renders the renamed column the way the view body names
// it: qualified when the body qualifies it, bare otherwise.
//
// It is the spelling C SQLite puts in "error in view %s: no such column: %s"
// -- the token its own re-parse failed to resolve (renameTableTest's
// sqlite3SelectPrep, alter.c:2050) -- so the two engines' messages agree.
func renamedColumnAsWritten(viewSQL, tableName, oldName, newName string) string {
	if sqlMentionsIdent(viewSQL, tableName+"."+oldName) {
		return tableName + "." + newName
	}
	low := strings.ToLower(viewSQL)
	if strings.Contains(low, strings.ToLower(tableName+"."+oldName)) {
		return tableName + "." + newName
	}
	return newName
}
