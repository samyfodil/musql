// This file implements CREATE [UNIQUE] INDEX and DROP INDEX: parsing,
// registering an index's metadata, and UNIQUE validation. On this format an
// index stores no entries: the segment file carries its SQL, and seeks and
// UNIQUE checks are derived from the table's rows (segment_seek.go,
// row_store_uniqindex.go); its entries are produced on read-out (read_view.go).
//
// A plain-column index takes "col [COLLATE BINARY|NOCASE|RTRIM] [ASC|DESC]"
// entries. Anything else in the key list, or a trailing WHERE, makes an
// expression/partial index (exprOrPartial): its key is evaluated per row, its
// WHERE applied, and UNIQUE enforced through a key/WHERE-aware pass
// (exprIndexConflicts, validateUniqueIndex). A custom collation is rejected.
//
// Automatic indexes from UNIQUE/PRIMARY KEY constraints are built by
// buildAutoIndexes from the autoIndexSpec values
// parseCreateTableColumnsAndAutoIndexes recovers: named
// sqlite_autoindex_<table>_<N>, unique, sql="" (NULL in the catalog, as in C).
// CreateTable and the catalog load both call it; afterwards an automatic index
// is an ordinary db.indexes entry.
package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// indexMeta is what the write side knows about an index -- created this session
// (CreateIndex) or loaded from the catalog (schema_load_objects.go) -- in order
// to enforce UNIQUE and answer seeks.
type indexMeta struct {
	// schemaSeq is this object's CREATION rank within its database: the order
	// its sqlite_schema row must appear in, which C SQLite gives by simply
	// appending each new object's row to the catalog b-tree. The catalog this
	// engine writes is ordered by it, so the schema reports objects in the order
	// they were created rather than grouped by kind. Assigned by
	// DB.nextSchemaSeq at CREATE time, recovered from the row's position at load,
	// and carried unchanged through ALTER TABLE (which C SQLite likewise applies
	// in place, leaving the row's rowid alone).
	schemaSeq uint64

	isTemp bool // TEMP catalog rather than main; see temp_schema.go

	name   string
	table  string // the table's own tableMeta.name (case-normalized to it), not necessarily sqlText's exact casing
	cols   []string
	colIdx []int // resolved indices into the table's tableMeta.cols, parallel to cols
	// colCollation is the effective collation per indexed column, parallel
	// to cols/colIdx: an explicit "COLLATE name" in CREATE INDEX wins, else
	// the table column's declared collation, else BINARY -- SQLite's
	// documented rule. Always populated, including for automatic indexes
	// (which always inherit). UNIQUE enforcement, findRowConflicts, read-out
	// order and integrity_check must all agree with C on it.
	colCollation []string

	// colDesc is parallel to cols/colIdx: true where the key column is
	// DESC (including a "UNIQUE(a DESC)" constraint's automatic index, as
	// PRAGMA index_xinfo reports). It is part of the index's order
	// (compareIndexRec), so an export written ascending fails C's
	// integrity_check and answers range queries with no rows. nil/short
	// means ascending. Unused for exprOrPartial, whose order is keys[i].desc.
	colDesc []bool

	unique bool
	sql    string // verbatim CREATE INDEX text, stored in sqlite_schema at Close

	// exprOrPartial marks an expression index (ON t(<expr>)) or partial index
	// (WHERE <cond>), UNIQUE or not. It is keyed on keys/where, and cols/
	// colIdx/colCollation hold something else: the distinct table columns
	// the expressions reference, in declaration order, so ALTER TABLE
	// DROP/RENAME COLUMN dependency checks see what C sees. False for
	// plain-column and automatic indexes. A UNIQUE one conflicts through
	// exprIndexConflicts.
	exprOrPartial bool

	// keys is the ORDERED key list of an exprOrPartial index -- one entry per
	// CREATE INDEX column-list entry, each either a plain table column or an
	// expression evaluated per row -- and where is its partial-index WHERE
	// predicate (nil if it has none). Together they are what indexEntryOf
	// keys this index's entries by, and what ANALYZE computes its statistics
	// from. Both nil for
	// every other index (whose key list is cols/colIdx/colCollation).
	keys  []indexKey
	where Expr

	// vec is set for libSQL's vector index, an expression index keyed on
	// libsql_vector_idx(col) (vector_index.go).
	vec *vecIndex

	// isTablePK is true ONLY for the synthetic indexMeta schema_write.go's
	// CreateTable builds for a WITHOUT ROWID
	// table's own PRIMARY KEY (tableMeta.pkIndex) -- see that field's doc
	// comment. It stays registered in db.indexes (unique=true) so ordinary
	// UNIQUE-enforcement/conflict-resolution code treats it exactly like any
	// other PRIMARY KEY-derived auto-index, but it gets no sqlite_schema row
	// (segmentSchemaRows) and no separate b-tree in an export: the table's OWN
	// b-tree already serves that role. Always false for every other
	// indexMeta (an explicit CREATE INDEX, or an ordinary rowid table's own
	// PRIMARY KEY/UNIQUE-derived automatic index).
	isTablePK bool

	// onConflict is this UNIQUE/PRIMARY KEY constraint's declared "ON
	// CONFLICT <action>" (conflictAbort, the zero value, when none, and for
	// every CREATE INDEX index), copied from autoIndexSpec.onConflict.
	// declaredHitAction uses it as the default when a statement gives no
	// OR-clause. A single INTEGER PRIMARY KEY has no indexMeta; its action
	// lives on columnInfo.RowidConflict.
	onConflict conflictAction

	// onConflictSet records whether onConflict came from an EXPLICIT "ON
	// CONFLICT <action>" clause, so buildAutoIndexes can tell an explicit
	// ABORT from an unstated default when merging two constraints that build
	// the same index -- see autoIndexSpec.onConflictSet (sql_parser.go), the
	// only producer. Left false for an explicit CREATE INDEX, which has no
	// ON CONFLICT clause to declare in the first place.
	onConflictSet bool
}

// findIndexMeta returns the indexMeta for name (case-insensitive), or nil.
func (db *DB) findIndexMeta(name string) *indexMeta {
	return db.findIndexMetaIn(scopeAny, name)
}

// parsedCreateIndex is parseCreateIndexStmt's result: enough to register an
// indexMeta once its columns are resolved against the target table.
type parsedCreateIndex struct {
	name  string
	table string
	cols  []string
	// collate holds, per entry in cols, the EXPLICIT per-column "COLLATE
	// name" this CREATE INDEX statement itself gave that column (uppercased:
	// "BINARY"/"NOCASE"/"RTRIM"), or "" if the column had no COLLATE clause
	// at all -- CreateIndex resolves the "" case against the underlying
	// table column's own declared collation (see indexMeta.colCollation's
	// doc comment).
	collate []string
	// desc is parallel to cols: true if that plain-column entry declared a
	// DESC sort order (false for ASC or unspecified, and for expression
	// entries). The write path deliberately ignores it -- a DESC column is
	// materialized/enforced exactly like ASC (see tryParseSimpleIndexColumn's
	// doc comment) -- but the READ path's secondary-index seek
	// (secondaryIndexSeekCandidates, index_seek_read.go) consults desc[0] to
	// DECLINE seeking any index whose LEADING column is DESC: a DESC leading
	// column means a database written by C SQLite stores that index's b-tree
	// in DESCENDING leading-column order, which the ascending-order index seek
	// reader would mis-navigate (dropping rows). Declining such an index is
	// always safe (it just falls back to a full scan).
	desc []bool
	// dquoted is parallel to cols: true if that plain-column entry's name was
	// written in the "..." spelling, the only one eligible for SQLite's
	// double-quoted-string misfeature. demoteDoubleQuotedIndexColumns reads it
	// once the target table is known.
	dquoted []bool
	// r32nDQSpan is parallel to cols: the byte span, in the CREATE INDEX text,
	// of that plain-column entry's own name token, recorded only by
	// r32nParseCreateIndexStmt. The renameFixQuotes port needs it because a bare
	// double-quoted KEY that resolves to no column is one of the tokens SQLite
	// rewrites (it reaches renameQuotefixExprCb as a demoted TK_STRING -- see
	// demoteDoubleQuotedIndexColumns for the same demotion at load time).
	r32nDQSpan []byteSpan
	unique     bool
	ifNotExi   bool

	// exprs is parallel to cols: nil for a plain-column entry (its name/
	// collation live in cols[i]/collate[i]), or the parsed expression for a
	// column-list entry that is anything other than a bare column name. where
	// is the partial-index WHERE condition (nil if none). exprOrPartial is true
	// as soon as ANY column-list entry is an expression OR a WHERE is present
	// -- the whole index then keys on keys/where (indexMeta.exprOrPartial).
	exprs         []Expr
	where         Expr
	exprOrPartial bool

	// scope is the catalog a "main."/"temp." qualifier on the index NAME
	// selects (scopeAny when unqualified) -- see parseCreateIndexStmt.
	scope schemaScope
}

// parseCreateIndexStmt parses "CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON
// table(<indexed-column> [, ...]) [WHERE <cond>]", where each indexed-column
// is either a plain "col [COLLATE BINARY|NOCASE|RTRIM] [ASC|DESC]" (the fully
// supported, materialized form) or an arbitrary expression (which makes the
// whole index expression-keyed -- see this package's doc comment). A COLLATE
// naming anything other than the three built-ins is rejected; the plain-column
// grammar is tried first per entry and anything outside it is re-parsed as an
// expression (see tryParseSimpleIndexColumn).
func parseCreateIndexStmt(sqlText string) (*parsedCreateIndex, error) {
	return r32nParseCreateIndexStmt(sqlText, false)
}

// r32nParseCreateIndexStmt is parseCreateIndexStmt with the parser's
// double-quoted-span recording switched on (recordDQ), so every bare `"..."`
// reference in a key EXPRESSION carries ColumnExpr.R32NDQSpan and every plain
// `"..."` KEY carries r32nDQSpan. Only the renameFixQuotes port passes true.
func r32nParseCreateIndexStmt(sqlText string, recordDQ bool) (*parsedCreateIndex, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	p.r32nDQRecord = recordDQ
	if !p.consumeKeyword("CREATE") {
		return nil, fmt.Errorf("engine: CREATE INDEX: expected CREATE, got %q", p.tokenDesc(p.peek()))
	}
	stmt := &parsedCreateIndex{unique: p.consumeKeyword("UNIQUE")}
	if !p.consumeKeyword("INDEX") {
		return nil, fmt.Errorf("engine: CREATE INDEX: expected INDEX, got %q", p.tokenDesc(p.peek()))
	}
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("NOT") || !p.consumeKeyword("EXISTS") {
			return nil, fmt.Errorf("engine: CREATE INDEX: expected NOT EXISTS after IF")
		}
		stmt.ifNotExi = true
	}

	t := p.peek()
	if t.kind != tkIdent {
		return nil, fmt.Errorf("engine: CREATE INDEX: expected index name, got %q", p.tokenDesc(t))
	}
	p.next()
	stmt.name = t.text
	if p.peekIsPunct(".") { // schema-qualified: CREATE INDEX main.idx ...
		p.next()
		t2 := p.peek()
		if t2.kind != tkIdent {
			return nil, fmt.Errorf("engine: CREATE INDEX: expected index name after schema qualifier")
		}
		p.next()
		// "main."/"temp." select one of this engine's two catalogs
		// (temp_schema.go); the qualifier constrains BOTH where the index is
		// created and which catalog its target table must resolve in, which
		// is C SQLite's own rule -- "CREATE INDEX temp.qi ON q(a)" is
		// valid exactly when q is a TEMP table ("cannot create a TEMP index
		// on non-TEMP table" otherwise, index.test), and "CREATE INDEX
		// main.tbli ON tbl(...)" indexes MAIN's tbl even while a temp tbl
		// shadows it (verified directly; tkt2817.test).
		sc, ok := scopeOfQualifier(t.text)
		if !ok {
			return nil, fmt.Errorf("engine: CREATE INDEX: unknown database %s", t.text)
		}
		stmt.scope = sc
		stmt.name = t2.text
	}

	if !p.consumeKeyword("ON") {
		return nil, fmt.Errorf("engine: CREATE INDEX: expected ON, got %q", p.tokenDesc(p.peek()))
	}
	tt := p.peek()
	if tt.kind != tkIdent {
		return nil, fmt.Errorf("engine: CREATE INDEX: expected table name, got %q", p.tokenDesc(tt))
	}
	p.next()
	stmt.table = tt.text

	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for {
		// The plain, materializable "col [COLLATE ...] [ASC|DESC]" form is
		// tried first (backtrackably); anything else -- a function call, an
		// arithmetic/logical expression, a "table.column" reference, a
		// custom/unknown COLLATE -- is re-parsed as an expression, which makes
		// the whole index expression-keyed (see this package's doc comment).
		save := p.pos
		colTok := p.peek()
		if colName, colCollate, colDesc, ok := tryParseSimpleIndexColumn(p); ok {
			stmt.cols = append(stmt.cols, colName)
			stmt.collate = append(stmt.collate, colCollate)
			stmt.desc = append(stmt.desc, colDesc)
			stmt.dquoted = append(stmt.dquoted, colTok.dquoted)
			stmt.r32nDQSpan = append(stmt.r32nDQSpan, p.r32nDQSpan(colTok))
			stmt.exprs = append(stmt.exprs, nil)
		} else {
			p.pos = save
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			// A trailing ASC/DESC on an expression entry is RECORDED, exactly
			// like a plain column's: it is part of the b-tree's physical cell
			// order, which C SQLite reading this file depends on (see
			// indexMeta.colDesc).
			exprDesc := false
			if !p.consumeKeyword("ASC") {
				exprDesc = p.consumeKeyword("DESC")
			}
			stmt.cols = append(stmt.cols, "")
			stmt.collate = append(stmt.collate, "")
			stmt.desc = append(stmt.desc, exprDesc)
			stmt.dquoted = append(stmt.dquoted, false)
			stmt.r32nDQSpan = append(stmt.r32nDQSpan, byteSpan{})
			stmt.exprs = append(stmt.exprs, e)
			stmt.exprOrPartial = true
		}
		if p.peekIsPunct(",") {
			p.next()
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	if p.consumeKeyword("WHERE") {
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.where = w
		stmt.exprOrPartial = true
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: CREATE INDEX: unexpected trailing input near %q", p.tokenDesc(p.peek()))
	}
	return stmt, nil
}

// demoteDoubleQuotedIndexColumns rewrites every plain-column key entry whose
// double-quoted name resolves to no column of tbl into an expression entry
// holding that string literal -- SQLite's double-quoted-string misfeature.
// "CREATE INDEX x1 ON t1( "y" )" over t1(a,b) indexes the constant 'y'
// (indexexpr1.test 2100): the TK_ID fails sqlite3ResolveSelfReference and
// EP_DblQuoted turns it back into a string. `y`/[y] stays "no such column",
// and a name that resolves stays a plain column (keeping its collation and
// UNIQUE). It needs tbl, so it cannot happen at parse time, and it must run on
// both CreateIndex and catalog recovery or a reopen would fail.
func demoteDoubleQuotedIndexColumns(stmt *parsedCreateIndex, tbl *tableMeta) {
	for i, cname := range stmt.cols {
		if cname == "" || i >= len(stmt.dquoted) || !stmt.dquoted[i] {
			continue
		}
		if indexOfColumn(tbl, cname) >= 0 {
			continue
		}
		lit := Value{Typ: Text, S: []byte(cname)}
		stmt.cols[i] = ""
		stmt.exprs[i] = LiteralExpr{Val: lit}
		stmt.exprOrPartial = true
	}
}

// tryParseSimpleIndexColumn tries to consume one key-list entry of the plain
// form -- a bare column name, optional "COLLATE BINARY|NOCASE|RTRIM",
// optional ASC/DESC -- followed by "," or ")". Anything else returns ok=false,
// possibly having consumed tokens; the caller restores its position and
// re-parses the entry as an expression.
func tryParseSimpleIndexColumn(p *parser) (name, collate string, desc, ok bool) {
	t := p.peek()
	switch t.kind {
	case tkIdent:
		// NULL is a hard keyword in SQLite's grammar, never an identifier:
		// "CREATE TABLE t(null)" is a syntax error there, so a bare NULL in an
		// index key list is the CONSTANT, and "CREATE INDEX i0 ON t1(NULL)" is
		// an accepted expression index (verified against 3.53.3; whereL.test
		// 800). This lexer folds every keyword into tkIdent, so the one bare,
		// unquoted spelling has to be sent down the expression path by hand --
		// a QUOTED "null"/[null]/`null` really is an identifier and stays one.
		if !t.quoted && equalFoldName(t.text, "NULL") {
			return "", "", false, false
		}
		if nt := p.peekAt(1); nt.kind == tkPunct && nt.text == "(" {
			return "", "", false, false // a function call -- an expression entry
		}
		name = t.text
	case tkString:
		// A bare string literal in an index key list is converted to an
		// identifier first: sqlite3CreateIndex calls sqlite3StringToId on every
		// entry, rewriting a top-level TK_STRING (or TK_COLLATE over one) to
		// TK_ID. So "ON t1('z')" is "no such column: z" while "ON t1('b')" over
		// a real column b is a plain-column index. Only the top level converts:
		// "+'y'" stays a literal. It gets no double-quoted rescue, since only a
		// "..." token carries EP_DblQuoted.
		name = t.str
	default:
		return "", "", false, false
	}
	p.next()
	if p.peekIsKeyword("COLLATE") {
		ctok := p.peekAt(1)
		if ctok.kind != tkIdent || !knownCollations[asciiFold(ctok.text, false)] {
			// A custom/unknown collation -- let the expression path re-parse
			// this entry so parseCollate reports SQLite's own exact "no such
			// collation sequence: %s" for it, rather than guessing here.
			return "", "", false, false
		}
		p.next() // COLLATE
		p.next() // collation name
		collate = strings.ToUpper(ctok.text)
	}
	// A per-column ASC/DESC sort order is accepted but not otherwise recorded:
	// this engine's read path never scans an index, and UNIQUE enforcement is
	// by key EQUALITY, which a column's sort direction never changes. The
	// physical b-tree order is the only thing DESC alters, and it is
	// unobservable to every query and comparison this engine's gates make, so
	// a DESC column is enforced and materialized exactly like an ASC one.
	if !p.consumeKeyword("ASC") {
		desc = p.consumeKeyword("DESC")
	}
	if p.peekIsPunct(",") || p.peekIsPunct(")") {
		return name, collate, desc, true
	}
	// The entry's leading token did not terminate it (e.g. "a + b") -- an
	// expression.
	return "", "", false, false
}

// parsedDropIndex is parseDropIndexStmt's result: enough for DropIndex to
// look the name up and, if not found (or the schema qualifier is
// unrecognized -- see dropQualifiedName's doc comment), report the exact
// "no such index" error C SQLite gives.
type parsedDropIndex struct {
	bare          string
	display       string
	ifExists      bool
	unknownSchema bool
	scope         schemaScope // catalog a "main."/"temp." qualifier selects
}

// parseDropIndexStmt parses "DROP INDEX [IF EXISTS] [schema.]name", sharing
// its schema-qualifier and trailing-input grammar with
// parseDropTableOrViewStmt (drop_write.go) -- DROP TABLE/VIEW/INDEX all
// resolve an unrecognized schema qualifier identically (see
// dropQualifiedName's doc comment: "not found," not a separate "unknown
// database" error).
func parseDropIndexStmt(sqlText string) (*parsedDropIndex, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	const errPrefix = "engine: DROP INDEX"
	if !p.consumeKeyword("DROP") {
		return nil, fmt.Errorf("%s: expected DROP, got %q", errPrefix, p.tokenDesc(p.peek()))
	}
	if !p.consumeKeyword("INDEX") {
		return nil, fmt.Errorf("%s: expected INDEX, got %q", errPrefix, p.tokenDesc(p.peek()))
	}
	stmt := &parsedDropIndex{}
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("EXISTS") {
			return nil, fmt.Errorf("%s: expected EXISTS after IF", errPrefix)
		}
		stmt.ifExists = true
	}
	bare, display, scope, unknownSchema, nerr := dropQualifiedName(p, errPrefix)
	if nerr != nil {
		return nil, nerr
	}
	stmt.bare, stmt.display, stmt.scope, stmt.unknownSchema = bare, display, scope, unknownSchema
	if terr := expectDropTrailer(p, errPrefix); terr != nil {
		return nil, terr
	}
	return stmt, nil
}

// CreateIndex parses and registers a single "CREATE [UNIQUE] INDEX ..."
// statement against a table already registered this session (via
// CreateTable, or recovered by OpenWrite): its sqlite_schema row
// (type='index', name, tbl_name, rootpage, sql=createSQL verbatim) is written
// with the catalog at commit (Session.rewriteFile). A UNIQUE index is validated against the table's CURRENT
// data immediately, so a CREATE UNIQUE INDEX over already-conflicting rows
// fails right away rather than silently at Close.
func (db *DB) CreateIndex(sqlText string) error {
	stmt, err := parseCreateIndexStmt(sqlText)
	if err != nil {
		return err
	}
	if err := db.checkReservedObjectName(stmt.name); err != nil {
		return err
	}
	if existing := db.findIndexMetaIn(stmt.scope, stmt.name); existing != nil {
		if stmt.ifNotExi {
			return nil
		}
		return fmt.Errorf("engine: index %s already exists", stmt.name)
	}
	// Tables, views and indexes share ONE namespace, and SQLite reports a
	// collision with either of the first two as "there is already a table
	// named X" (a view counts as a table here) -- verified directly, and
	// NOT suppressed by IF NOT EXISTS, which only covers index-vs-index.
	if db.findTableMetaIn(stmt.scope, stmt.name) != nil || db.findViewMetaIn(stmt.scope, stmt.name) != nil {
		return fmt.Errorf("engine: there is already a table named %s", stmt.name)
	}
	// SQLite's own internal tables may not be indexed, whatever they are and
	// whether or not they currently exist -- "table sqlite_stat1 may not be
	// indexed" (analyze.test), and the same wording for sqlite_master and
	// sqlite_sequence. Checked before resolution so the message does not
	// depend on whether ANALYZE happens to have created sqlite_stat1 yet.
	if strings.HasPrefix(r33sFoldIdent(stmt.table), "sqlite_") {
		return fmt.Errorf("engine: table %s may not be indexed", stmt.table)
	}
	// The name qualifier constrains the TARGET table's catalog too (see
	// parseCreateIndexStmt): "CREATE INDEX main.tbli ON tbl(...)" indexes
	// MAIN's tbl, and is "no such table: main.tbl" when only a temp tbl
	// exists -- exactly the tkt2817.test case.
	tbl := db.findTableMetaIn(stmt.scope, stmt.table)
	if tbl == nil {
		if db.findViewMetaIn(stmt.scope, stmt.table) != nil {
			return fmt.Errorf("engine: views may not be indexed")
		}
		if stmt.scope == scopeMain {
			return fmt.Errorf("engine: no such table: main.%s", stmt.table)
		}
		if stmt.scope == scopeTemp {
			return fmt.Errorf("engine: cannot create a TEMP index on non-TEMP table")
		}
		// C names the SCHEMA here and no statement context: "CREATE INDEX i2
		// ON nosuch(a)" is `no such table: main.nosuch` (verified against
		// 3.53.3), unlike an ordinary unqualified "no such table: nosuch".
		// parsedCreateIndex carries no schema of its own -- a qualified
		// "CREATE INDEX s.i" is handled before this point -- so main is the
		// only catalog that reaches here.
		return fmt.Errorf("engine: no such table: main.%s", stmt.table)
	}
	// See table_load.go's package doc comment: a UNIQUE (or expression/
	// partial) index validates against tbl's EXISTING rows before it can be
	// registered (validateUniqueIndex/createExprIndex, below), so the table's
	// rows are read first.
	//
	// Decided after the double-quoted-name demotion, which can turn a plain
	// column into an expression.
	demoteDoubleQuotedIndexColumns(stmt, tbl)
	if err := db.ensureTableLoaded(tbl); err != nil {
		return err
	}
	if stmt.exprOrPartial {
		return db.createExprIndex(sqlText, stmt, tbl)
	}

	colIdx, colColl, rerr := resolvePlainIndexColumns(tbl, stmt)
	if rerr != nil {
		return fmt.Errorf("engine: CREATE INDEX %s: %w", stmt.name, rerr)
	}

	idxm := &indexMeta{
		schemaSeq:    db.nextSchemaSeq(tbl.isTemp),
		isTemp:       tbl.isTemp,
		name:         stmt.name,
		table:        tbl.name,
		cols:         stmt.cols,
		colIdx:       colIdx,
		colCollation: colColl,
		colDesc:      append([]bool(nil), stmt.desc...),
		unique:       stmt.unique,
		sql:          normalizeSchemaSQL("INDEX", sqlText),
	}
	if idxm.unique {
		if err := validateUniqueIndex(tbl, idxm, db.encoding()); err != nil {
			return fmt.Errorf("engine: CREATE INDEX %s: %w", stmt.name, err)
		}
	}

	if err := db.checkNewIndex(idxm, tbl); err != nil {
		return err
	}
	db.indexes = append(db.indexes, idxm)
	db.bumpSchema(idxm.isTemp)
	return nil
}

// createExprIndex registers an expression or partial index (UNIQUE or not). It
// first applies the C validation decidable without evaluation: every
// expression and the WHERE may reference only tbl's columns ("no such column:
// %s"), and may not use a subquery, aggregate, bound parameter, "table.column"
// reference or non-deterministic/unknown function. Anything not positively
// confirmed acceptable is declined, which is never a wrong answer since C
// rejects those too.
func (db *DB) createExprIndex(sqlText string, stmt *parsedCreateIndex, tbl *tableMeta) error {

	idxm, err := buildExprIndexMeta(stmt.name, tbl, stmt, normalizeSchemaSQL("INDEX", sqlText))
	if err != nil {
		// An errVDBESemantic is C SQLite's OWN prepare-time diagnosis and
		// is surfaced verbatim, exactly as query.go surfaces one from a
		// FROM-less compile: C answers "second argument to LIKELIHOOD() must
		// be a constant between 0.0 and 1.0" with no statement prefix in
		// front of it. Everything else keeps the prefix, which names which
		// CREATE INDEX failed.
		if errors.Is(err, errVDBESemantic) {
			return err
		}
		return fmt.Errorf("engine: CREATE INDEX %s: %w", stmt.name, err)
	}
	if idxm.vec != nil {
		// libSQL fills a new vector index from the table's rows, each one
		// checked as an INSERT would check it.
		var whereProg *selfRowExpr
		if idxm.where != nil {
			whereProg, _ = compileIndexExprs(tbl, idxm)
		}
		for rowid, vals := range tbl.rows.all() {
			if err := checkVectorIndexRow(tbl, idxm, whereProg, rowid, vals); err != nil {
				return err
			}
		}
	}
	idxm.schemaSeq = db.nextSchemaSeq(tbl.isTemp)
	if err := db.checkNewIndex(idxm, tbl); err != nil {
		return err
	}
	db.indexes = append(db.indexes, idxm)
	db.bumpSchema(idxm.isTemp)
	return nil
}

// buildExprIndexMeta assembles a non-UNIQUE expression/partial index's
// indexMeta: keys/where, plus the referenced columns in cols/colIdx/
// colCollation (see indexMeta.exprOrPartial). Shared by createExprIndex and
// catalog recovery, so a reopened index is identical to the created one.
func buildExprIndexMeta(name string, tbl *tableMeta, stmt *parsedCreateIndex, storedSQL string) (*indexMeta, error) {
	ref := make(map[int]bool)
	for _, e := range stmt.exprs {
		if e == nil {
			continue // a plain-column entry in a mixed index -- validated below
		}
		if err := checkAndCollectIndexExprCols(e, tbl, ref, false); err != nil {
			return nil, err
		}
	}
	// A plain-column entry appearing alongside an expression entry (e.g.
	// "CREATE INDEX i ON t(a, b+1)") still has its bare name in stmt.cols; its
	// column must exist exactly as for a fully-supported plain index.
	for _, cname := range stmt.cols {
		if cname == "" {
			continue // an expression entry -- already handled above
		}
		idx := indexOfColumn(tbl, cname)
		if idx < 0 {
			return nil, fmt.Errorf("no such column: %s", cname)
		}
		ref[idx] = true
	}
	if stmt.where != nil {
		if err := checkAndCollectIndexExprCols(stmt.where, tbl, ref, true); err != nil {
			return nil, err
		}
	}

	cols := make([]string, 0, len(ref))
	colIdx := make([]int, 0, len(ref))
	colColl := make([]string, 0, len(ref))
	for j := range tbl.cols {
		if ref[j] {
			cols = append(cols, tbl.cols[j].Name)
			colIdx = append(colIdx, j)
			colColl = append(colColl, effectiveCollation(tbl.cols[j].Collation))
		}
	}

	vec, err := vectorIndexOf(stmt, tbl)
	if err != nil {
		return nil, err
	}

	keys, err := resolveIndexKeys(tbl, stmt)
	if err != nil {
		return nil, err
	}
	return &indexMeta{
		vec:           vec,
		isTemp:        tbl.isTemp,
		name:          name,
		table:         tbl.name,
		cols:          cols,
		colIdx:        colIdx,
		colCollation:  colColl,
		unique:        stmt.unique,
		exprOrPartial: true,
		keys:          keys,
		where:         stmt.where,
		sql:           storedSQL,
	}, nil
}

// resolvePlainIndexColumns resolves a plain index's key columns against tbl:
// positions and effective collations (indexMeta.colCollation's rule). Shared by
// CreateIndex and catalog recovery so the two cannot drift; recovery once
// rebuilt only colIdx and wrote a NOCASE index in BINARY order.
func resolvePlainIndexColumns(tbl *tableMeta, stmt *parsedCreateIndex) (colIdx []int, colColl []string, err error) {
	colIdx = make([]int, len(stmt.cols))
	colColl = make([]string, len(stmt.cols))
	for i, cname := range stmt.cols {
		idx := indexOfColumn(tbl, cname)
		if idx < 0 {
			// resolve.c:785's own wording: an index column is resolved by
			// ordinary name resolution, so C says "no such column: nosuch"
			// and names neither the table nor the index.
			return nil, nil, fmt.Errorf("engine: no such column: %s", cname)
		}
		colIdx[i] = idx
		if i < len(stmt.collate) && stmt.collate[i] != "" {
			colColl[i] = stmt.collate[i]
		} else {
			colColl[i] = effectiveCollation(tbl.cols[idx].Collation)
		}
	}
	return colIdx, colColl, nil
}

// indexKey is one ORDERED key entry of an expression/partial index
// (indexMeta.keys): either a plain table column (colIdx >= 0) or an expression
// evaluated per row (expr != nil), plus the collating sequence its comparison
// -- the b-tree's own cell order, and ANALYZE's distinct counts -- must use.
type indexKey struct {
	colIdx    int
	expr      Expr
	collation string
	desc      bool // stored DESCENDING; see indexMeta.colDesc
}

// resolveIndexKeys turns a parsed expression/partial index's column list into
// its ordered indexKey list, resolving each plain-column entry against tbl.
//
// The collating sequence follows SQLite's own rule in both shapes: an explicit
// per-entry COLLATE wins; otherwise a plain column keeps its DECLARED collation
// (verified: a partial index over a NOCASE column counts 'x'/'X' as one value)
// while an expression takes whatever collation the expression itself carries
// (exprCollation -- an inner "COLLATE nocase" propagates out), defaulting to
// BINARY, which is what "PRAGMA index_xinfo" reports for an expression key.
func resolveIndexKeys(tbl *tableMeta, parsed *parsedCreateIndex) ([]indexKey, error) {
	keys := make([]indexKey, len(parsed.cols))
	for i := range parsed.cols {
		keys[i] = indexKey{colIdx: -1, collation: effectiveCollation(atOrEmpty(parsed.collate, i)), desc: i < len(parsed.desc) && parsed.desc[i]}
		if i < len(parsed.exprs) && parsed.exprs[i] != nil {
			keys[i].expr = parsed.exprs[i]
			if n, ok := exprCollation(parsed.exprs[i]); ok {
				keys[i].collation = effectiveCollation(n)
			}
			continue
		}
		ci := indexOfColumn(tbl, parsed.cols[i])
		if ci < 0 {
			return nil, fmt.Errorf("no such column: %s", parsed.cols[i])
		}
		keys[i].colIdx = ci
		if atOrEmpty(parsed.collate, i) == "" {
			keys[i].collation = effectiveCollation(tbl.cols[ci].Collation)
		}
	}
	return keys, nil
}

// compileIndexExprs compiles an expression/partial index's WHERE and each
// XN_EXPR key against tbl's register-backed row scope, so every per-row use
// runs opcodes. C codes the same two pieces: sqlite3ExprIfFalseDup on
// pPartIdxWhere (insert.c:2419) and sqlite3ExprCodeCopy on each aColExpr entry
// (insert.c:2433), under "pParse->iSelfTab = -(regNewData+1)" (expr.c:5047-5068).
// keyProgs is parallel to idx.keys, nil for a plain-column key.
//
// It is the one lowering of these expressions: validateUniqueIndex,
// checkIndexExprsForRow, exprIndexConflicts, indexEntryOf and ANALYZE all ask
// "is this row in the index, with what key?", and two evaluators would be two
// semantics.
//
// It compiles per call rather than caching on indexMeta: ALTER TABLE can
// reorder tbl's columns under a long-lived indexMeta, and programs address
// columns by position. Every caller scans the table anyway.
func compileIndexExprs(tbl *tableMeta, idx *indexMeta) (whereProg *selfRowExpr, keyProgs []*selfRowExpr) {
	scope := selfRowScope(tbl)
	whereProg = compileSelfRowExpr(scope, idx.where, true /* NC_PartIdx: resolve.c:316 drops the db qualifier */, pureCtxIndex)
	keyProgs = make([]*selfRowExpr, len(idx.keys))
	for i, k := range idx.keys {
		if k.colIdx < 0 {
			keyProgs[i] = compileSelfRowExpr(scope, k.expr, false /* NC_IdxExpr is NOT in resolve.c:316's mask */, pureCtxIndex)
		}
	}
	return whereProg, keyProgs
}

// checkIndexExprsForRow evaluates, for one row being written, every
// non-UNIQUE expression or partial index of tbl on OUR format, where no index
// entry is stored and so nothing else computes its key. C computes every
// index's key as it writes the row (sqlite3GenerateConstraintChecks /
// sqlite3CompleteInsertion), so an error there -- "non-deterministic use of
// julianday() in an index" (sqlite3NotPureFunc, vdbeaux.c:5627) above all --
// fails the statement. A UNIQUE one is already evaluated by its conflict
// check.
//
// ponytail: compiles the index's programs per row; cache them per statement
// if a bulk write into an expression-indexed table ever shows up in a profile.
func (db *DB) checkIndexExprsForRow(tbl *tableMeta, rowid uint64, vals []Value) error {
	for _, idx := range db.indexes {
		if idx.vec != nil {
			// Its key cannot fail (libsql_vector_idx is its argument), but
			// the vector must fit the index, UNIQUE or not.
			if !indexBelongsTo(idx, tbl) {
				continue
			}
			var whereProg *selfRowExpr
			if idx.where != nil {
				whereProg, _ = compileIndexExprs(tbl, idx)
			}
			if err := checkVectorIndexRow(tbl, idx, whereProg, rowid, vals); err != nil {
				return err
			}
			continue
		}
		if idx.unique || !idx.exprOrPartial || !indexBelongsTo(idx, tbl) {
			continue
		}
		whereProg, keyProgs := compileIndexExprs(tbl, idx)
		if err := indexExprsEvalErr(tbl, idx, whereProg, keyProgs, rowid, vals); err != nil {
			return err
		}
	}
	return nil
}

// indexExprsEvalErr is one row's evaluation of idx's WHERE and, when that
// admits the row, its expression keys, in C's order: a partial index's
// excluded row never computes a key (insert.c:2419 jumps past it).
func indexExprsEvalErr(tbl *tableMeta, idx *indexMeta, whereProg *selfRowExpr, keyProgs []*selfRowExpr, rowid uint64, vals []Value) error {
	ctx := rowEvalCtx(tbl, rowid, vals, nil, nil, nil)
	if idx.where != nil {
		v, err := whereProg.eval(ctx)
		if err != nil {
			return err
		}
		if !isTruthy(v) {
			return nil
		}
	}
	for _, kp := range keyProgs {
		if kp == nil {
			continue
		}
		if _, err := kp.eval(ctx); err != nil {
			return err
		}
	}
	return nil
}

// indexOfColumn returns the index of the column named cname (case-insensitive)
// in tbl's declared column list, or -1.
func indexOfColumn(tbl *tableMeta, cname string) int {
	for j, c := range tbl.cols {
		if equalFoldName(c.Name, cname) {
			return j
		}
	}
	return -1
}

// indexExprFuncAllowed reports whether fn may appear in an index expression or
// partial-index WHERE: any supported scalar function except those C rejects as
// "non-deterministic functions prohibited in index expressions" (resolve.c:1220-
// 1228) -- random, randomblob and the two compile-option diagnostics (see
// compileOptionDiagFuncName). Unknown functions are not allowed either, so the
// index is declined, never mis-accepted.
func indexExprFuncAllowed(fn string) bool {
	name := r33sFoldIdent(fn)
	switch {
	case name == "random" || name == "randomblob":
		return false
	case compileOptionDiagFuncName(name), nonConstantExtFuncName(name), udfNonDeterministic(name):
		return false
	}
	return supportedFuncs[name]
}

// checkAndCollectIndexExprCols walks e -- a key expression or the partial WHERE
// -- rejecting any construct C forbids in an index (subquery, aggregate, bound
// parameter, "table.column" key reference, non-deterministic/unknown function)
// with a decline, and resolving each bare column into ref, reporting "no such
// column: %s" for a missing one. Any node type it does not recognize is
// declined, never silently accepted.
func checkAndCollectIndexExprCols(e Expr, tbl *tableMeta, ref map[int]bool, inWhere bool) error {
	switch x := e.(type) {
	case nil, LiteralExpr:
		return nil
	case ParamExpr:
		return fmt.Errorf("bound parameters are not supported in index expressions by this write path")
	case SubqueryExpr, ExistsExpr:
		return fmt.Errorf("subqueries are not supported in index expressions by this write path")
	case ColumnExpr:
		if x.Qualifier != "" {
			// C's rules are opposite on the two sides (over t3(a,b,c)):
			//
			//	CREATE INDEX i ON t3(t3.a)              -> the "." operator
			//	                                           prohibited in index
			//	                                           expressions
			//	CREATE INDEX i ON t3(b) WHERE t3.b>1        -> ACCEPTED
			//	CREATE INDEX i ON t3(b) WHERE main.t3.b>1   -> ACCEPTED
			//	CREATE INDEX i ON t3(b) WHERE xyzzy.t3.b>1  -> ACCEPTED; the
			//	    database part is ignored (index6.test 5.0)
			//	CREATE INDEX i ON t3(a) WHERE nosuchtable.a>1 -> no such column:
			//	    nosuchtable.a  (the table part is NOT ignored)
			//	CREATE INDEX i ON t3(a) WHERE other.a>1       -> no such column:
			//	    other.a, even though "other" is a real table
			if !inWhere {
				return fmt.Errorf(`the "." operator prohibited in index expressions`)
			}
			if !equalFoldName(x.Qualifier, tbl.name) {
				// The TABLE part is resolved, and only against the indexed
				// table. SQLite words the message without the database part
				// even for the three-part form ("xyzzy.nosuchtab.a" reports
				// "no such column: nosuchtab.a").
				return fmt.Errorf("no such column: %s.%s", x.Qualifier, x.Name)
			}
			// The database part (ColumnExpr.Schema) is not checked: the compiled
			// WHERE drops it (compiler.ignoreDbQualifier, since resolve.c:316's
			// mask names NC_PartIdx), so the stored text keeps whatever database
			// name was written. ALTER TABLE RENAME TO must follow the table
			// qualifier or the reopen re-parse would fail; that is
			// indexWhereQualifierEdits (alter_write.go).
			ci := indexOfColumn(tbl, x.Name)
			if ci < 0 {
				// A QUALIFIED reference reports the qualifier too, and gets no
				// double-quoted-string rescue: the misfeature applies to a
				// lone identifier token, not to the column half of a "t.c"
				// pair. Verified against 3.53.3 -- `WHERE t3."nope">1` is
				// `no such column: t3.nope` there, where the same name written
				// bare degrades to the string 'nope'.
				return fmt.Errorf("no such column: %s.%s", x.Qualifier, x.Name)
			}
			ref[ci] = true
			return nil
		}
		idx := indexOfColumn(tbl, x.Name)
		if idx < 0 {
			// A DOUBLE-QUOTED name that resolves to no column degrades to the
			// STRING LITERAL it spells -- SQLite's double-quoted-string
			// misfeature, which ColumnExpr.FallbackLiteral already carries and
			// which the VDBE's own compileColumn already honors.
			// Only this validator did
			// not, so "CREATE INDEX i2 ON q1(x, y, z||"abc")" was
			// "no such column: abc" where C SQLite builds the index and
			// stores the source text verbatim (quote.test; verified against
			// 3.53.3, including that a partial index's WHERE degrades the same
			// way and that integrity_check then passes). It contributes no
			// column reference, exactly like the literal it now is.
			if x.FallbackLiteral != nil {
				return nil
			}
			return fmt.Errorf("no such column: %s", x.Name)
		}
		ref[idx] = true
		return nil
	case UnaryExpr:
		return checkAndCollectIndexExprCols(x.X, tbl, ref, inWhere)
	case BinaryExpr:
		if err := checkAndCollectIndexExprCols(x.L, tbl, ref, inWhere); err != nil {
			return err
		}
		return checkAndCollectIndexExprCols(x.R, tbl, ref, inWhere)
	case IsNullExpr:
		return checkAndCollectIndexExprCols(x.X, tbl, ref, inWhere)
	case CollateExpr:
		return checkAndCollectIndexExprCols(x.X, tbl, ref, inWhere)
	case CastExpr:
		return checkAndCollectIndexExprCols(x.X, tbl, ref, inWhere)
	case InExpr:
		if x.Sub != nil {
			return fmt.Errorf("subqueries are not supported in index expressions by this write path")
		}
		if err := checkAndCollectIndexExprCols(x.X, tbl, ref, inWhere); err != nil {
			return err
		}
		for _, it := range x.List {
			if err := checkAndCollectIndexExprCols(it, tbl, ref, inWhere); err != nil {
				return err
			}
		}
		return nil
	case BetweenExpr:
		for _, s := range []Expr{x.X, x.Lo, x.Hi} {
			if err := checkAndCollectIndexExprCols(s, tbl, ref, inWhere); err != nil {
				return err
			}
		}
		return nil
	case LikeExpr:
		for _, s := range []Expr{x.X, x.Pattern, x.Escape} {
			if err := checkAndCollectIndexExprCols(s, tbl, ref, inWhere); err != nil {
				return err
			}
		}
		return nil
	case GlobExpr:
		for _, s := range []Expr{x.X, x.Pattern} {
			if err := checkAndCollectIndexExprCols(s, tbl, ref, inWhere); err != nil {
				return err
			}
		}
		return nil
	case CaseExpr:
		if err := checkAndCollectIndexExprCols(x.Base, tbl, ref, inWhere); err != nil {
			return err
		}
		for _, w := range x.Whens {
			if err := checkAndCollectIndexExprCols(w.When, tbl, ref, inWhere); err != nil {
				return err
			}
			if err := checkAndCollectIndexExprCols(w.Then, tbl, ref, inWhere); err != nil {
				return err
			}
		}
		return checkAndCollectIndexExprCols(x.Else, tbl, ref, inWhere)
	case FuncExpr:
		if isAggregateCall(x) {
			return fmt.Errorf("aggregate functions are not supported in index expressions by this write path")
		}
		if x.CurrentTimeKw {
			// CURRENT_DATE/TIME/TIMESTAMP are not SQLITE_FUNC_CONSTANT, so
			// resolve.c:1228 refuses them in an index KEY and in a partial
			// index's WHERE alike -- NC_IdxExpr and NC_PartIdx are both in
			// that mask. See FuncExpr.CurrentTimeKw.
			return fmt.Errorf("non-deterministic functions prohibited in index expressions")
		}
		if !indexExprFuncAllowed(x.Name) {
			return fmt.Errorf("function %s() is not supported in index expressions by this write path", x.Name)
		}
		// likelihood()'s probability argument is validated wherever SQLite
		// codes the call, and CREATE INDEX codes its expressions on the spot.
		// compileFunc covers statements that run; this covers the DDL.
		// altertab3.test 5: "CREATE INDEX i2 ON t2((LIKELIHOOD(c0, 100) IN ()))"
		// must be rejected.
		if x.Name == "likelihood" && len(x.Args) == 2 {
			spelling := x.NameAsWritten
			if spelling == "" {
				spelling = x.Name
			}
			if lerr := checkLikelihoodLiteralArgNamed(spelling, x.Args[1]); lerr != nil {
				return lerr
			}
		}
		for _, a := range x.walkArgs() {
			if err := checkAndCollectIndexExprCols(a, tbl, ref, inWhere); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported expression in index by this write path")
	}
}

// DropIndex parses and executes "DROP INDEX [IF EXISTS] [schema.]name".
//
// An automatic index (sql="") is rejected with C's wording, "index associated
// with UNIQUE or PRIMARY KEY constraint cannot be dropped" -- sqlite3DropIndex
// checks idxType != SQLITE_IDXTYPE_APPDEF only after resolving the name, so
// IF EXISTS on a missing name still succeeds silently.
func (db *DB) DropIndex(sqlText string) error {
	stmt, err := parseDropIndexStmt(sqlText)
	if err != nil {
		return err
	}
	if aerr := db.checkDropSchemaQualifier(stmt.display, stmt.unknownSchema); aerr != nil {
		return aerr
	}
	if !stmt.unknownSchema {
		if target := db.findIndexMetaIn(stmt.scope, stmt.bare); target != nil {
			for i, ix := range db.indexes {
				if ix != target {
					continue
				}
				if ix.sql == "" {
					return fmt.Errorf("engine: index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped")
				}
				db.indexes = append(db.indexes[:i], db.indexes[i+1:]...)
				db.bumpSchema(ix.isTemp)
				if !ix.isTemp {
					db.stat1 = db.stat1.forgetIndex(ix.name)
				}
				// C SQLite deletes a dropped index's sqlite_stat1 row along
				// with it -- verified directly (analyze.test): stale stats for
				// an object that no longer exists never survive a DROP.
				return db.dropStat1Rows("idx", ix.name)
			}
		}
	}
	if stmt.ifExists {
		return nil
	}
	return fmt.Errorf("engine: no such index: %s", stmt.display)
}

// indexColumnValue returns column colIdx of vals as it should be encoded
// into an index record: the same raw, on-disk-shaped value tbl.rows already
// stores (see tableMeta's doc comment -- no affinity "fix-up" the way
// normalizeRow applies for read-side display, since the physical index
// record must byte-for-byte match how that same column's value is encoded
// in the table row itself), except for the table's INTEGER PRIMARY KEY
// column: that column is stored as NULL in vals (the real value lives only
// in the rowid), so an index over it must substitute the actual rowid.
func indexColumnValue(tbl *tableMeta, rowid uint64, vals []Value, colIdx int) Value {
	if colIdx == tbl.ipkIndex && vals[colIdx].Typ == Null {
		return Value{Typ: Int, I: int64(rowid)}
	}
	return vals[colIdx]
}

// indexTrailingKeyColsAndCollation returns, for a WITHOUT ROWID table, its
// PRIMARY KEY columns and collations -- the trailing back-pointer a secondary
// index carries instead of a rowid -- or nil, nil for a rowid table (the
// rowid is appended, compared as a plain integer).
//
// A PK column the index already carries under the same collation is not
// repeated: sqlite3CreateIndex drops it from the tail (build.c:4274-4290,
// isDupColumn at :2274). Repeating it fails C's integrity_check.
func indexTrailingKeyColsAndCollation(tbl *tableMeta, keyCols []int, keyColls []string) (colIdx []int, collation []string) {
	if !tbl.withoutRowid {
		return nil, nil
	}
	for j, pc := range tbl.pkIndex.colIdx {
		dup := false
		for i, kc := range keyCols {
			if kc == pc && i < len(keyColls) && strings.EqualFold(effectiveCollation(keyColls[i]), effectiveCollation(tbl.pkIndex.colCollation[j])) {
				dup = true
				break
			}
		}
		if !dup {
			colIdx = append(colIdx, pc)
			collation = append(collation, tbl.pkIndex.colCollation[j])
		}
	}
	return colIdx, collation
}

// validateUniqueIndex checks idx's UNIQUE constraint against tbl's CURRENT
// row store: two rows conflict only if every one of idx's indexed columns
// is non-NULL in both AND compares equal (per compareValues, so e.g.
// integer 3 and real 3.0 do conflict) -- matching SQLite's documented rule
// that "for the purposes of unique indices, all NULL values are considered
// different from all other NULL values," which in practice means any row
// with a NULL in ANY indexed column is exempt from the uniqueness check
// entirely, not just for that one column.
func validateUniqueIndex(tbl *tableMeta, idx *indexMeta, enc TextEncoding) error {
	type tuple struct {
		rowid uint64
		vals  []Value
	}
	// An EXPRESSION or PARTIAL index keys on idx.keys, not on colIdx -- which
	// for one of those holds the columns its expressions merely REFERENCE (see
	// indexMeta.exprOrPartial) -- and only WHERE-matching rows are in it at
	// all. Both are verified against 3.53.3:
	//
	//	CREATE UNIQUE INDEX t5x ON t5(a+b)
	//	  (1,2) then (2,1)          UNIQUE constraint failed: index 't5x'
	//	  (NULL,1) then (NULL,2)    both accepted -- a NULL key never conflicts
	//	CREATE UNIQUE INDEX t6x ON t6(a) WHERE b>0
	//	  (1,1),(1,-1),(1,-2)       all accepted -- only the first is IN the index
	//	  then (1,5)                UNIQUE constraint failed: t6.a
	keyCount := len(idx.colIdx)
	if idx.exprOrPartial {
		keyCount = len(idx.keys)
	}
	var whereProg *selfRowExpr
	var keyProgs []*selfRowExpr
	if idx.exprOrPartial {
		whereProg, keyProgs = compileIndexExprs(tbl, idx)
	}
	var tuples []tuple
	for rowid, vals := range tbl.rows.all() {
		var ctx *evalCtx
		if idx.exprOrPartial {
			ctx = rowEvalCtx(tbl, rowid, vals, nil, nil, nil)
		}
		if idx.exprOrPartial && idx.where != nil {
			v, err := whereProg.eval(ctx)
			if err != nil {
				return fmt.Errorf("index %s: WHERE: %w", idx.name, err)
			}
			if !isTruthy(v) {
				continue
			}
		}
		t := make([]Value, keyCount)
		hasNull := false
		for i := 0; i < keyCount; i++ {
			var v Value
			if idx.exprOrPartial {
				k := idx.keys[i]
				if k.colIdx >= 0 {
					v = indexColumnValue(tbl, rowid, vals, k.colIdx)
				} else {
					ev, err := keyProgs[i].eval(ctx)
					if err != nil {
						return fmt.Errorf("index %s: %w", idx.name, err)
					}
					v = ev
				}
			} else {
				v = indexColumnValue(tbl, rowid, vals, idx.colIdx[i])
			}
			if v.Typ == Null {
				hasNull = true
				break
			}
			t[i] = v
		}
		if hasNull {
			continue
		}
		tuples = append(tuples, tuple{rowid: rowid, vals: t})
	}

	colColl := func(i int) string {
		if idx.exprOrPartial {
			return effectiveCollation(idx.keys[i].collation)
		}
		return effectiveCollation(atOrEmpty(idx.colCollation, i))
	}
	sort.Slice(tuples, func(a, b int) bool {
		for i := range tuples[a].vals {
			if c := compareValuesCollatedEnc(tuples[a].vals[i], tuples[b].vals[i], colColl(i), enc); c != 0 {
				return c < 0
			}
		}
		return rowidLess(tuples[a].rowid, tuples[b].rowid)
	})
	for i := 1; i < len(tuples); i++ {
		equal := true
		for c := range tuples[i].vals {
			if compareValuesCollatedEnc(tuples[i].vals[c], tuples[i-1].vals[c], colColl(c), enc) != 0 {
				equal = false
				break
			}
		}
		if equal {
			return uniqueIndexConflictError(tbl, idx)
		}
	}
	return nil
}

// buildAutoIndexes turns specs (from parseCreateTableColumnsAndAutoIndexes)
// into indexMeta values named sqlite_autoindex_<tableName>_<N>, N = 1, 2, ...
// in CREATE TABLE text order, as C numbers them. Column names resolve
// case-insensitively against cols; an unknown one is an error. sql is "",
// stored as NULL like C's automatic-index rows.
//
// Called by CreateTable (an empty table, nothing to validate) and catalog
// recovery (automatic indexes have no SQL, so they are re-derived from the
// table's CREATE TABLE text).
func buildAutoIndexes(tableName string, cols []columnInfo, specs []autoIndexSpec) (out []*indexMeta, bySpec []*indexMeta, err error) {
	out = make([]*indexMeta, 0, len(specs))
	bySpec = make([]*indexMeta, len(specs))
	for si, spec := range specs {
		colIdx := make([]int, len(spec.cols))
		colColl := make([]string, len(spec.cols))
		colDesc := make([]bool, len(spec.cols))
		for j, cname := range spec.cols {
			idx := -1
			for k, c := range cols {
				if equalFoldName(c.Name, cname) {
					idx = k
					break
				}
			}
			if idx < 0 {
				// Same resolver, same wording (resolve.c:785): a table
				// constraint's column list names columns of the table being
				// created, and an unknown one is "no such column: x".
				return nil, nil, fmt.Errorf("engine: no such column: %s", cname)
			}
			colIdx[j] = idx
			// A CONSTRAINT-LEVEL "COLLATE x" overrides the column's own
			// declared collation for this index, exactly like CREATE INDEX's
			// per-column COLLATE does -- SQLite's indexed-column grammar is
			// the same one in both places. "" means the constraint named none.
			colColl[j] = effectiveCollation(cols[idx].Collation)
			if j < len(spec.colls) && spec.colls[j] != "" {
				if !knownCollations[asciiFold(spec.colls[j], false)] {
					return nil, nil, fmt.Errorf("engine: CREATE TABLE %s: no such collation sequence: %s", tableName, spec.colls[j])
				}
				colColl[j] = effectiveCollation(spec.colls[j])
			}
			colDesc[j] = j < len(spec.desc) && spec.desc[j]
		}
		// A constraint that would build an index IDENTICAL to one an earlier
		// constraint already built is not built twice: C SQLite keeps the
		// first and discards the redundant one, so "CREATE TABLE t(c UNIQUE
		// PRIMARY KEY)" and "CREATE TABLE t(c PRIMARY KEY, UNIQUE(c))" each
		// leave exactly ONE sqlite_autoindex row (verified directly; it is
		// also what SQLite's own index.test index-16.* asserts). "Identical"
		// is same key columns in the same POSITIONS with the same collations
		// -- and nothing else: (c,d) vs (d,c) stay two indexes, differing
		// collations stay two, while DESC is NOT part of the comparison
		// ("UNIQUE(c,d)" and "PRIMARY KEY(c DESC,d)" collapse to one).
		if prior := findRedundantAutoIndex(out, colIdx, colColl); prior != nil {
			act, aerr := mergeAutoIndexConflict(tableName, prior, spec)
			if aerr != nil {
				return nil, nil, aerr
			}
			prior.onConflict = act
			bySpec[si] = prior
			continue
		}
		meta := &indexMeta{
			// Numbering counts SURVIVING indexes, not specs: a discarded
			// redundant constraint consumes no _<N> (C SQLite derives N
			// from the table's current index count at creation time, and a
			// redundant index is never linked into that list).
			name:          fmt.Sprintf("sqlite_autoindex_%s_%d", tableName, len(out)+1),
			table:         tableName,
			cols:          append([]string(nil), spec.cols...),
			colIdx:        colIdx,
			colCollation:  colColl,
			colDesc:       colDesc,
			unique:        true,
			sql:           "", // stored as SQL NULL, like C's automatic indexes
			onConflict:    spec.onConflict,
			onConflictSet: spec.onConflictSet,
		}
		out = append(out, meta)
		bySpec[si] = meta
	}
	return out, bySpec, nil
}

// findRedundantAutoIndex returns the already-built automatic index whose key
// is indistinguishable from (colIdx, colColl) -- see buildAutoIndexes for the
// exact equivalence rule and the evidence behind it -- or nil.
func findRedundantAutoIndex(built []*indexMeta, colIdx []int, colColl []string) *indexMeta {
	for _, prior := range built {
		if len(prior.colIdx) != len(colIdx) {
			continue
		}
		same := true
		for k := range colIdx {
			if prior.colIdx[k] != colIdx[k] || !equalFoldName(atOrEmpty(prior.colCollation, k), colColl[k]) {
				same = false
				break
			}
		}
		if same {
			return prior
		}
	}
	return nil
}

// mergeAutoIndexConflict decides the ON CONFLICT action the surviving index
// carries when a redundant constraint is folded into it. C's rule:
//
//	a PRIMARY KEY ON CONFLICT ABORT, UNIQUE(a) ON CONFLICT IGNORE
//	  -> error: conflicting ON CONFLICT clauses specified
//	a PRIMARY KEY,              UNIQUE(a) ON CONFLICT IGNORE  -> IGNORE
//	a PRIMARY KEY ON CONFLICT IGNORE, UNIQUE(a)               -> IGNORE
//
// Two explicit clauses that disagree are an error, and explicit beats unstated.
// So an explicit ABORT differs from no clause, which is why autoIndexSpec
// records onConflictSet.
func mergeAutoIndexConflict(tableName string, prior *indexMeta, spec autoIndexSpec) (conflictAction, error) {
	if !spec.onConflictSet {
		return prior.onConflict, nil
	}
	if !prior.onConflictSet {
		prior.onConflictSet = true
		return spec.onConflict, nil
	}
	if prior.onConflict != spec.onConflict {
		return 0, fmt.Errorf("conflicting ON CONFLICT clauses specified")
	}
	return prior.onConflict, nil
}

// uniqueIndexConflictError renders idx's violation the way C SQLite words
// it: "UNIQUE constraint failed: <table>.<col>[, <table>.<col>...]", naming
// the COLUMNS and never the index -- verified directly, including for an
// explicit "CREATE UNIQUE INDEX myidx ON t(a)", which still reports "t.a".
// Column names come from the TABLE (not the index's own spelling), so an
// index written over "T(A)" still reports the table's own casing.
//
// This is conflict.go's uniqueConflictError wording, reached from the other
// direction: that one starts from a conflictHit (the pre-store INSERT/UPDATE
// conflict resolver), this one from a whole-table re-validation.
func uniqueIndexConflictError(tbl *tableMeta, idx *indexMeta) error {
	// An index with an EXPRESSION key has no column to name, and C SQLite
	// words it with the index instead: "UNIQUE constraint failed: index 't5x'"
	// -- quoted, and with no table. A partial index over plain COLUMNS keeps
	// the column form ("UNIQUE constraint failed: t6.a"), so the split is on
	// the key kind and not on exprOrPartial. Both verified against 3.53.3.
	if idx.exprOrPartial {
		for _, k := range idx.keys {
			if k.colIdx < 0 {
				return fmt.Errorf("UNIQUE constraint failed: index '%s'", idx.name)
			}
		}
		parts := make([]string, 0, len(idx.keys))
		for _, k := range idx.keys {
			if k.colIdx >= 0 && k.colIdx < len(tbl.cols) {
				parts = append(parts, fmt.Sprintf("%s.%s", tbl.name, tbl.cols[k.colIdx].Name))
			}
		}
		if len(parts) > 0 {
			return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
		}
		return fmt.Errorf("UNIQUE constraint failed: index '%s'", idx.name)
	}
	parts := make([]string, 0, len(idx.colIdx))
	for _, ci := range idx.colIdx {
		if ci >= 0 && ci < len(tbl.cols) {
			parts = append(parts, fmt.Sprintf("%s.%s", tbl.name, tbl.cols[ci].Name))
		}
	}
	if len(parts) == 0 {
		// No resolvable column (should not happen): keep a precise, if
		// non-SQLite, message rather than an empty one.
		return fmt.Errorf("UNIQUE constraint failed: index %s (table %s)", idx.name, tbl.name)
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
}

// indexBelongsTo reports whether idx is one of tbl's own indexes: same catalog
// and same table name, the pair identifying C's pTab->pIndex chain
// (build.c:4485-4486; sqlite3FindIndex looks in one database's idxHash,
// build.c:526-541). The catalog half matters when a TEMP table shadows a MAIN
// one: "CREATE TABLE t(a UNIQUE,b); CREATE TEMP TABLE t(a,b)" -- temp.t has no
// UNIQUE.
func indexBelongsTo(idx *indexMeta, tbl *tableMeta) bool {
	return idx.isTemp == tbl.isTemp && equalFoldName(idx.table, tbl.name)
}

// checkUniqueIndexesForRow asks each UNIQUE index on tbl whether the row just
// written shares its key with a different rowid, right after each
// INSERT/UPDATE stores one row, so the error comes from that statement.
//
// Per row is C's question -- sqlite3GenerateConstraintChecks probes each index
// with this row's key (insert.c:2000-2072) -- and re-validating the whole index
// instead made opUpdateRow, opUpsertStore and ON UPDATE CASCADE quadratic. A
// violation found here was introduced by this row, since every earlier write
// was checked the same way. The error text matches validateUniqueIndex's.
//
// An index the probe cannot answer (expression/partial key, or one the store's
// conflict index declines) falls back to whole-index validation.
func (db *DB) checkUniqueIndexesForRow(tbl *tableMeta, rowid uint64, vals []Value) error {
	for _, idx := range db.indexes {
		if !idx.unique || !indexBelongsTo(idx, tbl) {
			continue
		}
		if !idx.exprOrPartial {
			// Re-validating the WHOLE index per row written made a bulk UPDATE of a
			// UNIQUE column quadratic-and-then-some: 3,200 rows of a 32,000-row
			// table took 49s. The store's conflict index names the rows that may
			// hold this key and each is re-checked exactly (row_store_uniqindex.go),
			// the probe findRowConflicts makes for INSERT.
			if clash, answered := db.uniqueKeyTakenElsewhere(tbl, idx, rowid, vals); answered {
				if clash {
					return uniqueIndexConflictError(tbl, idx)
				}
				continue
			}
		}
		if err := validateUniqueIndex(tbl, idx, db.encoding()); err != nil {
			return err
		}
	}
	return nil
}

// uniqueKeyTakenElsewhere reports whether a row OTHER than rowid carries vals'
// key in the unique index idx, from a map store's conflict index; answered is
// false when that index cannot say, and the caller validates the long way. A key
// with a NULL component is taken by nobody.
func (db *DB) uniqueKeyTakenElsewhere(tbl *tableMeta, idx *indexMeta, rowid uint64, vals []Value) (clash, answered bool) {
	key := make([]Value, 0, len(idx.colIdx))
	for _, ci := range idx.colIdx {
		if ci < 0 || ci >= len(vals) {
			return false, false
		}
		v := indexColumnValue(tbl, rowid, vals, ci)
		if v.Typ == Null {
			return false, true
		}
		key = append(key, v)
	}
	cands, ok := tbl.rows.uniqConflictCandidates(tbl, idx, key, db.encoding())
	if !ok {
		return false, false
	}
	for _, other := range cands {
		if other == rowid {
			continue
		}
		if rv, have := tbl.rows.get(other); have && uniqueKeyMatches(db, tbl, idx, key, other, rv) {
			return true, true
		}
	}
	return false, true
}

// compareIndexRec compares two decoded index records column by column with
// compareValuesCollatedEnc under collations[i], negated where desc[i] is set.
// Both may be shorter than the records; columns past them (including the
// trailing rowid) compare BINARY ascending. Every record ends in a distinct
// rowid, so it is a total order.
//
// collations/desc are the index's colCollation/colDesc, so read-out order and
// integrity checks match C's order for the same index.
func compareIndexRec(a, b []Value, collations []string, desc []bool, enc TextEncoding) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		coll := "BINARY"
		if i < len(collations) {
			coll = collations[i]
		}
		c := compareValuesCollatedEnc(a[i], b[i], coll, enc)
		if i < len(desc) && desc[i] {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

// indexEntryForNewRow computes idx's index record for exactly one row
// (rowid, vals) of an ordinary rowid table, for integrity_check's row-versus-
// index comparison (integrity_rowcheck.go). It never needs the WITHOUT ROWID
// trailing-PRIMARY-KEY-columns case.
func indexEntryForNewRow(tbl *tableMeta, idx *indexMeta, rowid uint64, vals []Value) []Value {
	rec := make([]Value, 0, len(idx.colIdx)+1)
	for _, ci := range idx.colIdx {
		rec = append(rec, indexColumnValue(tbl, rowid, vals, ci))
	}
	rec = append(rec, Value{Typ: Int, I: int64(rowid)})
	return rec
}
