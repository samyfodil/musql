package engine

import (
	"errors"
	"strings"
)

// The action codes an Authorizer is called with: C SQLite's
// sqlite3_set_authorizer codes, with the same numbers (sqlite.h).
const (
	AuthCreateIndex       = 1  // index name, table name
	AuthCreateTable       = 2  // table name
	AuthCreateTempIndex   = 3  // index name, table name
	AuthCreateTempTable   = 4  // table name
	AuthCreateTempTrigger = 5  // trigger name, table name
	AuthCreateTempView    = 6  // view name
	AuthCreateTrigger     = 7  // trigger name, table name
	AuthCreateView        = 8  // view name
	AuthDelete            = 9  // table name
	AuthDropIndex         = 10 // index name, table name
	AuthDropTable         = 11 // table name
	AuthDropTempIndex     = 12 // index name, table name
	AuthDropTempTable     = 13 // table name
	AuthDropTempTrigger   = 14 // trigger name, table name
	AuthDropTempView      = 15 // view name
	AuthDropTrigger       = 16 // trigger name, table name
	AuthDropView          = 17 // view name
	AuthInsert            = 18 // table name
	AuthPragma            = 19 // pragma name, its argument
	AuthRead              = 20 // table name, column name
	AuthSelect            = 21
	AuthTransaction       = 22 // operation
	AuthUpdate            = 23 // table name, column name
	AuthAttach            = 24 // file name
	AuthDetach            = 25 // database name
	AuthAlterTable        = 26 // database name, table name
	AuthReindex           = 27 // index name
	AuthAnalyze           = 28 // table name
	AuthCreateVtable      = 29 // table name, module name
	AuthDropVtable        = 30 // table name, module name
	AuthFunction          = 31 // "", function name
	AuthSavepoint         = 32 // operation, savepoint name
	AuthRecursive         = 33
)

// What an Authorizer answers, C's SQLITE_OK, SQLITE_DENY and SQLITE_IGNORE.
const (
	AuthOK     = 0
	AuthDeny   = 1
	AuthIgnore = 2
)

// An Authorizer is consulted while a statement is prepared, as C SQLite's
// sqlite3_set_authorizer callback is: once per action the statement would
// take, with the action's code, its two arguments, the database name and the
// trigger or view the action comes from ("" for none or not applicable).
// AuthDeny fails the statement with ErrAuth. AuthIgnore makes a column read
// NULL and a function call NULL, leaves a DELETE as it is, and makes the
// statement do nothing for the action that heads it (an INSERT's target, a
// CREATE, DROP, ALTER, PRAGMA, transaction, savepoint, ATTACH or DETACH). It
// is checked before the statement compiles; the executor never sees it.
type Authorizer func(action int, arg1, arg2, dbName, trigger string) int

// ErrAuth is what a statement an Authorizer denied fails with (errors.Is), C's
// SQLITE_AUTH.
var ErrAuth = errors.New("not authorized")

type authError struct{ msg string }

func (e *authError) Error() string        { return e.msg }
func (e *authError) Is(target error) bool { return target == ErrAuth }

// AuthVerdict is what an Authorizer decided for one statement when it allowed
// it: run SQL in place of the statement's text when it is not empty, or
// nothing at all when Skip is set.
type AuthVerdict struct {
	SQL  string
	Skip bool
}

// errAuthIgnoreUnsupported is an AuthIgnore this engine cannot apply exactly,
// which fails the statement rather than run it without the NULL.
var errAuthIgnoreUnsupported = errors.New("engine: the authorizer's IGNORE cannot be applied to this statement")

// AuthDB is a database attached to a connection whose driver keeps its own
// sessions for them, for Authorize to find its objects.
type AuthDB struct {
	Name string
	DB   *DB
}

// Authorize runs fn over the actions sqlText would take, in the order C
// SQLite's prepare asks them, and returns what fn decided. sqlText is one
// statement; attached are the databases the connection has attached beyond
// the ones db knows.
func (db *DB) Authorize(sqlText string, fn Authorizer, attached ...AuthDB) (AuthVerdict, error) {
	a := &authPass{db: db, fn: fn, src: sqlText, nDb: 2 + len(db.attached) + len(attached), extra: attached}
	a.statement()
	if a.err != nil {
		return AuthVerdict{}, a.err
	}
	if a.skip {
		return AuthVerdict{Skip: true}, nil
	}
	if len(a.splices) == 0 {
		return AuthVerdict{}, nil
	}
	return AuthVerdict{SQL: a.spliced()}, nil
}

// authSplice replaces src[start:end] with text: how an IGNOREd column read or
// function call becomes NULL.
type authSplice struct {
	start, end int
	text       string
}

type authPass struct {
	db      *DB
	fn      Authorizer
	src     string
	spans   []byteSpan // the statement's reference spans, by authRef-1
	ctx     string     // the trigger or view the current action comes from
	nDb     int
	err     error
	halt    bool // C stops here with an error of its own, which the engine reports
	skip    bool
	trigs   []*authTrigger
	splices []authSplice
	ignored map[int32]bool
	attach  map[string]*authCatalog
	extra   []AuthDB
	// keepReads is RETURNING, where C's IGNORE leaves a column read as it is.
	keepReads bool
}

func (a *authPass) call(code int, arg1, arg2, dbName string) int {
	return a.fn(code, arg1, arg2, dbName, a.ctx)
}

// check is sqlite3AuthCheck (auth.c).
func (a *authPass) check(code int, arg1, arg2, dbName string) int {
	if a.err != nil {
		return AuthDeny
	}
	switch rc := a.call(code, arg1, arg2, dbName); rc {
	case AuthOK, AuthIgnore:
		return rc
	case AuthDeny:
		a.err = &authError{"engine: not authorized"}
		return AuthDeny
	default:
		a.err = errors.New("engine: authorizer malfunction")
		return AuthDeny
	}
}

// head checks the action a statement starts with, where C's IGNORE leaves
// the statement doing nothing. It reports whether to go on.
func (a *authPass) head(code int, arg1, arg2, dbName string) bool {
	switch a.check(code, arg1, arg2, dbName) {
	case AuthOK:
		return true
	case AuthIgnore:
		if len(a.trigs) > 0 {
			a.err = errAuthIgnoreUnsupported // C skips the one trigger step
		} else {
			a.skip = true
		}
	}
	return false
}

// mustOK checks an action whose IGNORE this engine has no exact meaning for.
func (a *authPass) mustOK(code int, arg1, arg2, dbName string) bool {
	switch a.check(code, arg1, arg2, dbName) {
	case AuthOK:
		return true
	case AuthIgnore:
		a.err = errAuthIgnoreUnsupported
	}
	return false
}

// readSchema is a read the statement's nested statements on the schema make.
func (a *authPass) readSchema(tab, col, dbName string) {
	if a.readCol(tab, col, dbName) == AuthIgnore {
		a.err = errAuthIgnoreUnsupported
	}
}

// readCol is sqlite3AuthReadCol: a column read, whose IGNORE reads NULL.
func (a *authPass) readCol(tab, col, dbName string) int {
	if a.err != nil {
		return AuthDeny
	}
	switch rc := a.call(AuthRead, tab, col, dbName); rc {
	case AuthOK, AuthIgnore:
		return rc
	case AuthDeny:
		z := tab + "." + col
		if a.nDb > 2 || !strings.EqualFold(dbName, "main") {
			z = dbName + "." + z
		}
		a.err = &authError{"engine: access to " + z + " is prohibited"}
		return AuthDeny
	default:
		a.err = errors.New("engine: authorizer malfunction")
		return AuthDeny
	}
}

// ignore makes the reference ref (an authRef) read NULL. name, when set, keeps
// the result column's name, which C takes from the text it replaces.
func (a *authPass) ignore(ref int32, name string) {
	text := "NULL"
	if name != "" {
		text = "NULL AS " + quoteIdent(name)
	}
	a.splice(ref, text)
}

// splice replaces the text of the reference ref with text.
func (a *authPass) splice(ref int32, text string) {
	if a.err != nil {
		return
	}
	if ref <= 0 || int(ref) > len(a.spans) {
		a.err = errAuthIgnoreUnsupported
		return
	}
	if a.ignored[ref] {
		return
	}
	if a.ignored == nil {
		a.ignored = map[int32]bool{}
	}
	a.ignored[ref] = true
	sp := a.spans[ref-1]
	// A call's arguments may hold parameters, whose numbering the splice
	// would shift.
	toks, err := lex(a.src[sp.start:sp.end])
	if err != nil {
		a.err = errAuthIgnoreUnsupported
		return
	}
	for _, t := range toks {
		if t.kind == tkParam {
			a.err = errAuthIgnoreUnsupported
			return
		}
	}
	a.splices = append(a.splices, authSplice{sp.start, sp.end, text})
}

func (a *authPass) spliced() string {
	ss := a.splices
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j].start < ss[j-1].start; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
	var b strings.Builder
	at := 0
	for _, s := range ss {
		if s.start < at {
			continue // inside a call already replaced
		}
		b.WriteString(a.src[at:s.start])
		b.WriteString(s.text)
		at = s.end
	}
	b.WriteString(a.src[at:])
	return b.String()
}

// parser returns a parser over the statement that records reference spans.
func (a *authPass) parser() (*parser, error) {
	toks, err := lex(a.src)
	if err != nil {
		return nil, err
	}
	p := newParser(a.src, toks)
	p.authSpans = &a.spans
	return p, nil
}

// statement dispatches on the statement's first keyword.
func (a *authPass) statement() {
	toks, err := lex(a.src)
	if err != nil || len(toks) == 0 {
		return // the engine reports the syntax error
	}
	i := 0
	if toks[i].kind == tkIdent && toks[i].isKw("EXPLAIN") {
		i++
		if toks[i].kind == tkIdent && toks[i].isKw("QUERY") {
			i += 2
		}
		a.explain(toks[i].Start)
		return
	}
	a.dispatch(toks)
}

// explain authorizes the statement after an EXPLAIN prefix, as C prepares it.
// Splices keep their offsets into the whole text.
func (a *authPass) explain(at int) {
	whole := a.src
	a.src = whole[at:]
	toks, err := lex(a.src)
	if err == nil && len(toks) > 0 {
		a.dispatch(toks)
	}
	for i := range a.splices {
		a.splices[i].start += at
		a.splices[i].end += at
	}
	a.src = whole
}

func (a *authPass) dispatch(toks []token) {
	t := toks[0]
	if t.kind != tkIdent {
		return
	}
	switch strings.ToUpper(t.text) {
	case "SELECT", "VALUES":
		a.topSelect()
	case "WITH":
		a.with(toks)
	case "INSERT", "REPLACE":
		a.insertText()
	case "UPDATE":
		a.updateText()
	case "DELETE":
		a.deleteText()
	case "CREATE":
		a.create(toks)
	case "DROP":
		a.drop(toks)
	case "ALTER":
		a.alter(toks)
	case "PRAGMA":
		a.pragma()
	case "BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE":
		a.transaction(toks)
	case "ATTACH":
		a.attachStmt()
	case "DETACH":
		a.detachStmt()
	case "ANALYZE":
		a.analyze(toks)
	case "REINDEX":
		a.reindex(toks)
	case "VACUUM":
		a.head(AuthAttach, "", "", "")
	default:
		a.err = &authError{"engine: not authorized"}
	}
}

// with finds the statement a leading WITH clause belongs to.
func (a *authPass) with(toks []token) {
	depth := 0
	for _, t := range toks[1:] {
		if t.kind == tkPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
			}
			continue
		}
		if depth != 0 || t.kind != tkIdent {
			continue
		}
		switch strings.ToUpper(t.text) {
		case "SELECT", "VALUES":
			a.topSelect()
			return
		case "INSERT", "REPLACE":
			a.insertText()
			return
		case "UPDATE":
			a.updateText()
			return
		case "DELETE":
			a.deleteText()
			return
		}
	}
}

func (a *authPass) topSelect() {
	p, err := a.parser()
	if err != nil {
		return
	}
	s, err := p.parseSelectStmt()
	if err != nil {
		return
	}
	if !a.head(AuthSelect, "", "", "") {
		return
	}
	a.selectBody(s, nil, nil, true)
}
