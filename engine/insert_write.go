// INSERT parsing (VALUES, SELECT, DEFAULT VALUES, UPSERT), the row-level
// helpers shared by the write paths (defaults, affinity, NOT NULL, STRICT,
// CHECK, rowid resolution), and the Exec/ExecArgs dispatcher. Every statement
// is compiled to a VDBE program (vdbe_write.go); there is no other executor.
//
// A VALUES item is any expression of the ordinary grammar. A top-level
// INSERT declines an aggregate call at parse time (parseInsertValueExpr); a
// column reference parses and then fails with C's "no such column: NAME",
// since a VALUES tuple has no FROM.
package engine

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// insertStmt is a parsed "INSERT INTO table [(cols)] VALUES (...), ..." or
// "INSERT INTO table [(cols)] SELECT ...". cols is nil when no column list was
// given ("every column, in declaration order"). Exactly one of rows/selectStmt
// is set:
//
//   - rows: each VALUES item as an Expr (see parseInsertValueExpr).
//   - selectStmt: the source SELECT, parsed by the same *parser so its
//     parameters number relative to the whole statement, as in C.
type insertStmt struct {
	table  string
	schema string // database qualifier ("main"/"temp"/attached name), "" if unqualified
	// alias is "AS <name>" on the INSERT target, or "". It only renames the
	// conflicting row for ON CONFLICT ... DO UPDATE, replacing the table name:
	//
	//	INSERT INTO t1 AS t2 ... DO UPDATE SET c=t2.c+1   ok
	//	INSERT INTO t1 AS t2 ... DO UPDATE SET c=t1.c+1   no such column: t1.c
	//
	// RETURNING is not affected ("RETURNING x.a" is "no such column: x.a"). A bare
	// AS-less alias is a syntax error in C, so none is accepted.
	alias      string
	cols       []string
	rows       [][]Expr
	selectStmt *SelectStmt
	returning  []SelectColumn // RETURNING result columns, nil if no RETURNING clause
	params     ParamInfo
	// ctes holds a leading WITH clause on the VALUES form (the SELECT form puts
	// it on selectStmt.CTEs). They are pushed (pushCTEScope, cte.go) while the
	// tuples compile, so a subquery in a tuple resolves them. A CTE shadows a
	// same-named table (select.c:6028/6036), so discarding them would be a wrong
	// answer:
	//
	//	WITH t AS (SELECT 99 AS x) INSERT INTO log VALUES((SELECT count(*) FROM t))
	//
	// logs 1, not the real table's count.
	ctes []CTEDef

	// orAction is the resolution algorithm named by an "INSERT OR ..."
	// clause (or REPLACE INTO's implied conflictReplace), or the default
	// conflictAbort when neither was given -- see conflict.go.
	orAction conflictAction
	// explicitOr is true when the statement itself named a conflict action
	// ("INSERT OR <action>" or "REPLACE INTO"), which orAction alone cannot
	// distinguish from no clause (both may be conflictAbort). An explicit
	// clause overrides every constraint's declared ON CONFLICT default
	// (columnInfo.RowidConflict/NotNullConflict, indexMeta.onConflict); with
	// none, each constraint's own default applies (conflict.test 4.7-4.11,
	// 5.7-5.16).
	explicitOr bool
	// upsert is this statement's trailing "ON CONFLICT ... DO NOTHING/DO
	// UPDATE" clause, or nil when none was given. See parseUpsertClause.
	upsert *upsertClause
	// defaultValues is set for "INSERT INTO t DEFAULT VALUES": cols is empty
	// (non-nil) and rows is one empty tuple. The compiler handles it as
	// insert.c's "nColumn==0 -> load the default value" arm (insert.c:1413-1418;
	// compileInsertStmt).
	defaultValues bool
}

// upsertClause is a parsed "ON CONFLICT [(target-cols)] DO NOTHING" or "ON
// CONFLICT [(target-cols)] DO UPDATE SET col=expr[,...] [WHERE expr]".
// targetCols is nil for a bare "ON CONFLICT" (matches any conflict).
// sets/where are set only for DO UPDATE. where is evaluated against the
// conflicting row and may reference "excluded.col"; false/NULL skips the
// update silently.
type upsertClause struct {
	targetCols []string
	// targetCollations parallels targetCols: an explicit "COLLATE name" on
	// that target column, or "". C's conflict target is a sortlist
	// (parse.y:1100); this engine accepts the "column [COLLATE name]
	// [ASC|DESC]" subset (parseUpsertClause). validateUpsertTarget must
	// reject the statement when the collation does not match the target
	// index's (indexMeta.colCollation): over "b TEXT UNIQUE", "ON CONFLICT(b
	// COLLATE nocase)" makes the whole INSERT fail.
	targetCollations []string
	// targetWhere is an explicit "ON CONFLICT(cols) WHERE <expr>" partial-index
	// conflict target's predicate, nil when none was written. C SQLite
	// (upsert.c's sqlite3UpsertAnalyzeTarget) matches this target ONLY against
	// a UNIQUE index carrying the SAME partial-index WHERE (sqlite3ExprCompare
	// equal, exactly -- not a subset/superset or any other equivalence); a
	// target with NO WHERE in turn matches ONLY a non-partial UNIQUE index,
	// never a partial one, regardless of column overlap. See
	// validateUpsertTarget.
	targetWhere Expr
	doNothing   bool
	sets        []assignment
	where       Expr
	// targetAlias is a copy of insertStmt.alias, whose only consumer is this
	// clause.
	targetAlias string
	// next is the following ON CONFLICT clause (Upsert.pNextUpsert): a
	// statement may chain several, and each constraint takes the first one
	// that names it (sqlite3UpsertOfIndex, upsert.c:247).
	next *upsertClause
}

// parseInsertStmt parses sqlText as a single INSERT statement.
func parseInsertStmt(sqlText string) (*insertStmt, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	stmt, err := parseInsertStmtTokens(p, false)
	if err != nil {
		return nil, err
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: INSERT: unexpected trailing input near %q", p.tokenDesc(p.peek()))
	}
	stmt.params = p.params.info()
	return stmt, nil
}

// parseInsertStmtTokens parses an INSERT off p, stopping after its last token
// without requiring ";"/EOF or snapshotting params (parseInsertStmt does both;
// parseTriggerBody keeps parsing the same *parser). One grammar for both entry
// points, except allowExprValues: a trigger body's INSERT (true) skips the
// top-level parse-time aggregate decline, since its VALUES need NEW./OLD.
// references and have a row context.
func parseInsertStmtTokens(p *parser, allowExprValues bool) (*insertStmt, error) {
	stmt := &insertStmt{}

	// A leading "WITH [RECURSIVE] <cte-list>" (parseWithClause) is allowed
	// before INSERT as before SELECT. For INSERT ... SELECT it is attached
	// to stmt.selectStmt.CTEs; for VALUES it is kept on stmt.ctes (see
	// insertStmt.ctes for why discarding it is wrong).
	var leadingCTEs []CTEDef
	if p.peekIsKeyword("WITH") {
		var err error
		leadingCTEs, err = p.parseWithClause()
		if err != nil {
			return nil, err
		}
	}

	// "REPLACE INTO t ..." is documented shorthand for "INSERT OR REPLACE
	// INTO t ..." (verified directly: both produce byte-identical
	// behavior/errors) -- checked before requiring the INSERT keyword so
	// this one grammar entry point handles both spellings, exactly like
	// Exec's own dispatch (insert_write.go) routes a leading REPLACE token
	// here rather than treating it as a separate statement kind.
	if p.consumeKeyword("REPLACE") {
		stmt.orAction = conflictReplace
		stmt.explicitOr = true
	} else {
		if !p.consumeKeyword("INSERT") {
			return nil, fmt.Errorf("engine: INSERT: expected INSERT, got %q", p.tokenDesc(p.peek()))
		}
		if p.consumeKeyword("OR") {
			act, err := parseConflictAction(p)
			if err != nil {
				return nil, err
			}
			stmt.orAction = act
			stmt.explicitOr = true
		}
	}
	if !p.consumeKeyword("INTO") {
		return nil, fmt.Errorf("engine: INSERT: expected INTO, got %q", p.tokenDesc(p.peek()))
	}
	// See parseDeleteStmtTokens' objectNameToken note (write_update_delete.go):
	// "INSERT INTO 'p 1 \"parent one\"' VALUES(...)" names a table.
	t := p.peek()
	tname, ok := objectNameToken(t)
	if !ok {
		return nil, fmt.Errorf("engine: INSERT: expected table name, got %q", p.tokenDesc(t))
	}
	p.next()
	stmt.table = tname
	// Optional database qualifier: "INSERT INTO schema.table ...". checkWriteSchemaQualifier
	// (schema_qualifier.go) validates it at exec time against this session's own schema.
	if p.peekIsPunct(".") {
		p.next() // the "."
		t2 := p.peek()
		n2, ok := objectNameToken(t2)
		if !ok {
			return nil, fmt.Errorf("engine: INSERT: expected table name after schema qualifier, got %q", p.tokenDesc(t2))
		}
		p.next()
		stmt.schema = tname
		stmt.table = n2
	}

	// "AS <alias>" on the target -- see insertStmt.alias. It sits between the
	// (optionally qualified) table name and the column list, exactly as in
	// SQLite's "xfullname ::= nm DOT nm AS nm" production.
	if p.peekIsKeyword("AS") {
		p.next()
		at := p.peek()
		aname, ok := objectNameToken(at)
		if !ok {
			return nil, fmt.Errorf("engine: INSERT: expected an alias after AS, got %q", p.tokenDesc(at))
		}
		p.next()
		stmt.alias = aname
	}

	if p.peekIsPunct("(") {
		p.next()
		for {
			ct := p.peek()
			cname, ok := objectNameToken(ct)
			if !ok {
				return nil, fmt.Errorf("engine: INSERT: expected column name, got %q", p.tokenDesc(ct))
			}
			p.next()
			stmt.cols = append(stmt.cols, cname)
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}

	// INSERT ... SELECT: parsed by this same parser, so parameters share
	// one numbering and the SELECT has the full standalone grammar.
	// peekStartsSubquerySelect also accepts a source with its own WITH
	// after the table name ("select ::= WITH wqlist selectnowith"):
	// "INSERT INTO t6 WITH s(x) AS (...) SELECT * FROM s".
	if p.peekStartsSubquerySelect() {
		sel, err := p.parseSelectStmt()
		if err != nil {
			return nil, err
		}
		// A WITH clause written BEFORE the INSERT keyword (leadingCTEs) has
		// nothing of its own to attach to, so it is attached here -- but only
		// when the select did not bring its own list, which now it can. The
		// two spellings are mutually exclusive in SQLite's grammar (a leading
		// WITH is consumed by the outer statement), so this never has to
		// merge them.
		if len(sel.CTEs) == 0 {
			sel.CTEs = leadingCTEs
		}
		stmt.selectStmt = sel
	} else if p.peekIsKeyword("DEFAULT") {
		// "INSERT INTO t DEFAULT VALUES" inserts one all-defaults row, modelled
		// as an INSERT providing no column: an empty non-nil column list plus
		// one empty tuple. Any OR clause parsed above still governs conflicts.
		p.next() // DEFAULT
		if !p.consumeKeyword("VALUES") {
			return nil, fmt.Errorf("engine: INSERT: expected VALUES after DEFAULT, got %q", p.tokenDesc(p.peek()))
		}
		// A column list before DEFAULT VALUES is an arity error. The grammar
		// accepts it ("insert_cmd INTO xfullname idlist_opt DEFAULT VALUES"),
		// and sqlite3Insert rejects it at prepare time:
		//
		//	if( pColumn!=0 && nColumn!=pColumn->nId ){
		//	  sqlite3ErrorMsg(pParse, "%d values for %d columns",
		//	                  nColumn, pColumn->nId);
		//	  goto insert_cleanup;
		//	}
		//	                                        -- insert.c:1255-1258
		//
		// DEFAULT VALUES is nColumn==0 (insert.c:1214), so any IDLIST
		// disagrees: "0 values for 1 columns" for a table, a view with an
		// INSTEAD OF trigger, and fts4 alike. It must be checked here, before
		// stmt.cols is overwritten below.
		if len(stmt.cols) > 0 {
			return nil, fmt.Errorf("engine: 0 values for %d columns", len(stmt.cols))
		}
		stmt.defaultValues = true
		stmt.cols = []string{}
		stmt.rows = [][]Expr{{}}
	} else {
		valuesPos := p.pos
		var savedParams paramTracker
		if p.params != nil {
			savedParams = paramTracker{maxIndex: p.params.maxIndex, names: maps.Clone(p.params.names)}
		}
		if !p.consumeKeyword("VALUES") {
			return nil, fmt.Errorf("engine: INSERT: expected VALUES or SELECT, got %q", p.tokenDesc(p.peek()))
		}
		for {
			if err := p.expectPunct("("); err != nil {
				return nil, err
			}
			var vals []Expr
			for {
				v, err := parseInsertValueExpr(p, allowExprValues)
				if err != nil {
					return nil, err
				}
				vals = append(vals, v)
				if p.peekIsPunct(",") {
					p.next()
					continue
				}
				break
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			stmt.rows = append(stmt.rows, vals)
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
		// INSERT's source is parse.y's "select", so VALUES followed by UNION,
		// INTERSECT or EXCEPT is the first arm of a compound. A non-constant
		// row (a window function) makes sqlite3MultiValues fall back to a
		// UNION ALL of one-row SELECTs, so "VALUES(1,2),(3,4),(row_number()
		// OVER (),5)" inserts (1,5) for the last row. Both are re-parsed as
		// INSERT ... SELECT from the VALUES keyword, keeping the parameter
		// numbering.
		compound := p.peekIsKeyword("UNION") || p.peekIsKeyword("INTERSECT") || p.peekIsKeyword("EXCEPT")
		// ONE row stays an expression list: sqlite3Insert unwraps a
		// single-row VALUES into pList before resolving it (insert.c), where
		// a window function is the misuse it is anywhere else.
		if !compound && len(stmt.rows) > 1 {
			for _, row := range stmt.rows {
				if slices.ContainsFunc(row, exprHasWindow) {
					compound = true
					break
				}
			}
		}
		if compound {
			p.pos = valuesPos
			if p.params != nil {
				*p.params = savedParams
			}
			sel, err := p.parseSelectStmt()
			if err != nil {
				return nil, err
			}
			if len(sel.CTEs) == 0 {
				sel.CTEs = leadingCTEs
			}
			stmt.rows = nil
			stmt.selectStmt = sel
		}
	}
	if stmt.selectStmt == nil {
		// The VALUES (and DEFAULT VALUES) forms have no selectStmt to hang a
		// leading WITH on, so it is kept here instead of discarded -- see
		// insertStmt.ctes for the wrong answer that discarding it was. Nothing
		// in a DEFAULT VALUES statement can reference a CTE, but keeping the
		// two branches uniform costs nothing and leaves no shape unattached.
		stmt.ctes = leadingCTEs
	}

	// An UPSERT clause is not grammatically allowed after DEFAULT VALUES (real
	// SQLite rejects it): leave a trailing "ON ..." unconsumed so the
	// statement's own trailing-input check declines it, rather than accepting
	// a form C SQLite rejects. The clauses chain, and one with no target
	// ends the chain (parse.y:1098-1107).
	for tail := &stmt.upsert; p.peekIsKeyword("ON") && !stmt.defaultValues; {
		up, err := parseUpsertClause(p)
		if err != nil {
			return nil, err
		}
		up.targetAlias = stmt.alias
		*tail, tail = up, &up.next
		if up.targetCols == nil {
			break
		}
	}

	ret, err := parseReturningClause(p)
	if err != nil {
		return nil, err
	}
	stmt.returning = ret

	return stmt, nil
}

// parseUpsertClause parses a trailing "ON CONFLICT [(col[,...]) ] DO NOTHING"
// or "ON CONFLICT [(col[,...])] DO UPDATE SET col=expr[,...] [WHERE expr]"
// clause (the "ON" keyword is peeked, not yet consumed, by the caller). A
// partial-index conflict target ("ON CONFLICT(cols) WHERE ...") is parsed
// into up.targetWhere -- index_write.go now supports partial (and partial
// UNIQUE) indexes, so such a target can genuinely match one; see
// validateUpsertTarget for how it is matched.
func parseUpsertClause(p *parser) (*upsertClause, error) {
	if !p.consumeKeyword("ON") {
		return nil, fmt.Errorf("engine: INSERT: expected ON, got %q", p.tokenDesc(p.peek()))
	}
	if !p.consumeKeyword("CONFLICT") {
		return nil, fmt.Errorf("engine: INSERT: expected CONFLICT, got %q", p.tokenDesc(p.peek()))
	}
	up := &upsertClause{}
	if p.peekIsPunct("(") {
		p.next()
		for {
			// C's conflict target is a sortlist (parse.y:1100). This accepts
			// the plain "column [COLLATE x]" subset via tryParseSimpleIndexColumn
			// (index_write.go), CREATE INDEX's own column parse. An expression
			// target stays declined.
			save := p.pos
			ct := p.peek()
			cname, collate, _, ok := tryParseSimpleIndexColumn(p)
			if !ok {
				p.pos = save
				return nil, fmt.Errorf("engine: INSERT: expected column name in ON CONFLICT target, got %q", p.tokenDesc(ct))
			}
			up.targetCols = append(up.targetCols, cname)
			up.targetCollations = append(up.targetCollations, collate)
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
			up.targetWhere = w
		}
	}
	if !p.consumeKeyword("DO") {
		return nil, fmt.Errorf("engine: INSERT: expected DO, got %q", p.tokenDesc(p.peek()))
	}
	switch {
	case p.consumeKeyword("NOTHING"):
		up.doNothing = true
	case p.consumeKeyword("UPDATE"):
		if !p.consumeKeyword("SET") {
			return nil, fmt.Errorf("engine: INSERT: expected SET, got %q", p.tokenDesc(p.peek()))
		}
		for {
			// The upsert's DO UPDATE SET takes the same column-list
			// assignment an ordinary UPDATE does -- "ON CONFLICT(a) DO UPDATE
			// SET (b,c)=(SELECT 7,8)" is accepted by C SQLite (verified) --
			// so it shares parseSetColumnList (write_update_delete.go).
			if p.peekIsPunct("(") {
				sets, err := parseSetColumnList(p, "INSERT")
				if err != nil {
					return nil, err
				}
				up.sets = append(up.sets, sets...)
				if p.peekIsPunct(",") {
					p.next()
					continue
				}
				break
			}
			ct := p.peek()
			cname, ok := objectNameToken(ct)
			if !ok {
				return nil, fmt.Errorf("engine: INSERT: expected column name, got %q", p.tokenDesc(ct))
			}
			p.next()
			if err := p.expectPunct("="); err != nil {
				return nil, err
			}
			expr, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			up.sets = append(up.sets, assignment{col: cname, expr: expr})
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
		if p.consumeKeyword("WHERE") {
			expr, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			up.where = expr
		}
	default:
		return nil, fmt.Errorf("engine: INSERT: expected NOTHING or UPDATE after ON CONFLICT ... DO, got %q", p.tokenDesc(p.peek()))
	}
	return up, nil
}

// parseInsertValueExpr parses one VALUES item as a full expression
// (p.parseExpr), matching C's VALUES grammar.
//
// With allowExprValues (a trigger body's INSERT) anything parsed is accepted,
// since NEW./OLD. references need a row context the trigger supplies. A
// top-level INSERT declines an aggregate call here: a VALUES tuple has no
// aggregate context.
//
// A bare column reference is not declined: it fails at compile time with C's
// "no such column: NAME", since the tuple resolves against no tables.
func parseInsertValueExpr(p *parser, allowExprValues bool) (Expr, error) {
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if allowExprValues {
		return expr, nil
	}
	if containsAggregate(expr) {
		return nil, fmt.Errorf("engine: INSERT: an aggregate function is not supported in a VALUES expression by this write path")
	}
	return expr, nil
}

// rowExprsContainSubquery reports whether any expression in any VALUES tuple
// holds a subquery in any form (containsSubquery). Its one caller uses it to
// decide whether to pay for a database snapshot before evaluating them.
func rowExprsContainSubquery(rows [][]Expr) bool {
	for _, row := range rows {
		for _, e := range row {
			if containsSubquery(e) {
				return true
			}
		}
	}
	return false
}

// maxRowidOfTable returns the largest rowid currently stored in tbl's live
// row store, and whether it holds any rows at all (false for an empty
// table) -- the logical-row-store equivalent of the old b-tree-walking
// maxRowid, now that no b-tree exists until Close (see writer.go).
func maxRowidOfTable(tbl *tableMeta) (uint64, bool) {
	// The store caches its maximum (rowStore.max). Without
	// it this ranged the WHOLE row store on every auto-assigned INSERT, making a
	// bulk load O(n^2): 96.6% of sqllimits1.test's CPU, and the reason it never
	// finished inside a ten-minute budget.
	return tbl.rows.maxRowid()
}

// errAutoincrementExhausted is OP_NewRowid's SQLITE_FULL for an AUTOINCREMENT
// table (vdbe.c:5660-5663): its sequence has reached the largest rowid, or its
// largest rowid has, and AUTOINCREMENT never falls back to a random one.
var errAutoincrementExhausted = errors.New("engine: database or disk is full")

// errCorruptSequence is autoIncBegin's SQLITE_CORRUPT_SEQUENCE (insert.c:422-
// 434): the table's database has no sqlite_sequence, or one that is not an
// ordinary two-column rowid table.
var errCorruptSequence = errors.New("engine: database disk image is malformed")

// nextRowidForTable returns the rowid OP_NewRowid assigns a table that is not
// AUTOINCREMENT: one past its current maximum, or 1 for an empty table. An
// AUTOINCREMENT table's comes from writeCtx.aincNewRowid.
// SetRowidRange makes this connection allocate auto-assigned rowids from
// [lo, hi): one past the largest rowid the table holds IN that range, or lo. A
// rowid the statement names explicitly is untouched. replication gives every site a
// range of its own, so two nodes inserting while apart never hand out the same
// rowid. lo >= hi turns it off. Temp tables, which never sync, keep the default.
func (db *DB) SetRowidRange(lo, hi uint64) { db.rowidLo, db.rowidHi = lo, hi }

// errRowidRangeExhausted is OP_NewRowid's SQLITE_FULL when a connection's rowid
// range has no room left -- C's answer when a table's rowids run out and no
// random one can be found (vdbe.c:5660-5680).
var errRowidRangeExhausted = errors.New("engine: database or disk is full")

// SetRowidFloor gives the range allocator a floor per table: fn(name) is the
// largest rowid the range ever handed out for main table name, which may since
// have been deleted, or 0 when it does not know. Ids then come from one past the
// larger of that and the largest the table holds, so a deleted id is not handed
// out again -- to replication the id is the row's identity, and a peer's late update to
// the deleted row would land on the new one.
func (db *DB) SetRowidFloor(fn func(table string) uint64) { db.rowidFloor = fn }

// nextRowidInRange is nextRowidForTable within this connection's rowid range.
func (db *DB) nextRowidInRange(tbl *tableMeta) (uint64, error) {
	v, ok := tbl.rows.maxRowidIn(db.rowidLo, db.rowidHi)
	if db.rowidFloor != nil {
		if f := db.rowidFloor(tbl.name); f >= db.rowidLo && f < db.rowidHi && (!ok || f > v) {
			v, ok = f, true
		}
	}
	if !ok {
		return db.rowidLo, nil
	}
	if v+1 >= db.rowidHi {
		return 0, errRowidRangeExhausted
	}
	return v + 1, nil
}

func nextRowidForTable(tbl *tableMeta) (uint64, error) {
	if v, ok := maxRowidOfTable(tbl); ok {
		return v + 1, nil
	}
	return 1, nil
}

// aincState is one AUTOINCREMENT table's three registers for the running
// statement -- sqlite3AutoincrementBegin's memId, memId+1 and memId+2
// (insert.c:460-511): the largest rowid handed out, and the sqlite_sequence
// row it was read from (its rowid and its seq, integerified) if there was one.
type aincState struct {
	tbl    *tableMeta
	seq    *tableMeta
	max    int64
	hasRow bool
	rowid  uint64
	orig   int64
}

// noteAutoincrement is autoIncBegin (insert.c:410-455), called as an INSERT
// into tbl is compiled: it checks that tbl's database has a usable
// sqlite_sequence and adds tbl to the statement's list, which the top-level
// program reads at its start (writeCtx.aincBegin). Trigger bodies are compiled
// inside the top-level compile, so theirs are on the list too, as in C.
func (db *DB) noteAutoincrement(tbl *tableMeta) error {
	if !tbl.autoIncrement {
		return nil
	}
	seq := db.sequenceTable(tbl.isTemp)
	if seq == nil || seq.withoutRowid || len(seq.cols) != 2 {
		return errCorruptSequence
	}
	if db.aincCollect != nil && !slices.Contains(*db.aincCollect, tbl) {
		*db.aincCollect = append(*db.aincCollect, tbl)
	}
	return nil
}

// ainc returns tbl's state for this statement, reading it from sqlite_sequence
// the first time: the first row, in rowid order, whose name is tbl's (a BINARY
// OP_Ne, so TEXT only), else a sequence of 0 and no row.
func (wc *writeCtx) ainc(tbl *tableMeta) (*aincState, error) {
	for _, st := range wc.aincs {
		if st.tbl == tbl {
			return st, nil
		}
	}
	seq := wc.db.sequenceTable(tbl.isTemp)
	if seq == nil || seq.withoutRowid || len(seq.cols) != 2 {
		return nil, errCorruptSequence
	}
	if err := wc.db.ensureTableLoaded(seq); err != nil {
		return nil, err
	}
	st := &aincState{tbl: tbl, seq: seq}
	for _, rowid := range seq.rows.sortedRowids() {
		vals := seq.rows.row(rowid)
		if len(vals) < 2 || vals[0].Typ != Text || string(vals[0].S) != tbl.name {
			continue
		}
		// OP_AddImm integerifies the seq; OP_Copy keeps that as the original.
		st.max = valueToInt64Trunc(vals[1])
		st.hasRow, st.rowid, st.orig = true, rowid, st.max
		break
	}
	wc.aincs = append(wc.aincs, st)
	return st, nil
}

// aincBegin reads every table on prog's list at the start of the statement.
func (wc *writeCtx) aincBegin(prog *Program) error {
	for _, tbl := range prog.Ainc {
		if _, err := wc.ainc(tbl); err != nil {
			return err
		}
	}
	return nil
}

// aincNewRowid is OP_NewRowid with P3 set (vdbe.c:5606-5670): one past the
// table's largest rowid, or past the sequence if that is larger.
func (wc *writeCtx) aincNewRowid(tbl *tableMeta) (uint64, error) {
	st, err := wc.ainc(tbl)
	if err != nil {
		return 0, err
	}
	if db := wc.db; db != nil && db.rowidLo < db.rowidHi && !tbl.isTemp {
		// A connection with a rowid range (SetRowidRange) allocates inside it,
		// above the range's own floor and above the sequence when the sequence
		// is in the range -- the same "never below anything handed out"
		// OP_NewRowid's P3 register keeps (vdbe.c's OP_NewRowid). A sequence
		// outside the range is another range's ids, applied here; it still
		// rises with them (aincStep), but does not move this range.
		v, err := db.nextRowidInRange(tbl)
		if err != nil {
			return 0, err
		}
		if st.max >= int64(db.rowidLo) && st.max < int64(db.rowidHi) && uint64(st.max) >= v {
			if v = uint64(st.max) + 1; v >= db.rowidHi {
				return 0, errRowidRangeExhausted
			}
		}
		st.max = max(st.max, int64(v))
		return v, nil
	}
	v := int64(1)
	if mx, ok := maxRowidOfTable(tbl); ok {
		if int64(mx) == math.MaxInt64 {
			return 0, errAutoincrementExhausted // useRandomRowid
		}
		v = int64(mx) + 1
	}
	if st.max == math.MaxInt64 {
		return 0, errAutoincrementExhausted
	}
	v = max(v, st.max+1)
	st.max = v
	return uint64(v), nil
}

// aincStep is autoIncStep's OP_MemMax (insert.c:520, vdbe.c:7682): every row
// an INSERT is about to store raises the sequence to its rowid -- before the
// constraint checks, so a row OR IGNORE then skips still counts.
func (wc *writeCtx) aincStep(tbl *tableMeta, rowid Value) error {
	st, err := wc.ainc(tbl)
	if err != nil {
		return err
	}
	st.max = max(st.max, valueToInt64Trunc(rowid))
	return nil
}

// aincEnd is autoIncrementEnd (insert.c:535-575), run when the statement
// completes: each table whose sequence has no row, or has risen past the row's
// original value, writes (name, seq) back -- over the row it was read from, or
// appended. C's list is built by pushing on the front, so the last table
// added is written first.
func (wc *writeCtx) aincEnd() {
	for i := len(wc.aincs) - 1; i >= 0; i-- {
		st := wc.aincs[i]
		if st.hasRow && st.max <= st.orig {
			continue
		}
		seq, rowid := st.seq, st.rowid
		if !st.hasRow {
			rowid, _ = nextRowidForTable(seq)
		}
		old, existed := seq.rows.get(rowid)
		seq.putRow(rowid, []Value{{Typ: Text, S: []byte(st.tbl.name)}, {Typ: Int, I: st.max}})
		wc.undoFn(func() {
			if existed {
				seq.putRow(rowid, old)
			} else {
				seq.dropRow(rowid)
			}
		})
	}
	wc.aincs = nil
}

// applyRowAffinities coerces each of full's values to its column's affinity, as
// C does on store (applyAffinityToValue): a whole-string numeric TEXT into an
// INTEGER/REAL/NUMERIC column becomes a number, a number into TEXT becomes
// text. It must run before the INTEGER PRIMARY KEY check, so '5' into an IPK is
// the explicit rowid 5.
func applyRowAffinities(cols []columnInfo, full []Value) {
	for i, c := range cols {
		full[i] = applyAffinityToValue(full[i], c.Aff)
	}
}

// applyColumnDefaults fills every column the statement omitted that declares a
// DEFAULT, marking it provided. It runs before applyRowAffinities (a default is
// coerced like an explicit value: "t TEXT DEFAULT 5" stores '5') and before
// computeGeneratedInto.
//
// Skipped: tbl.ipkIndex (C ignores a DEFAULT on the rowid alias; see
// columnInfo.DefaultValue) and generated columns. A DEFAULT this path cannot
// reproduce (DefaultKnown false) is left for checkNotNullAndDefault to reject.
func applyColumnDefaults(tbl *tableMeta, full []Value, provided []bool) {
	// Built on first use only: the overwhelming majority of rows have no
	// deferred DEFAULT at all and must not pay for one. The scope is empty
	// because a DEFAULT clause may reference no column -- see
	// columnInfo.defaultProg for why that is what makes these programs
	// runnable here at all (selfRowExpr.runnable's sameColumnList test).
	var defaultCtx *evalCtx
	for i, c := range tbl.cols {
		if i == tbl.ipkIndex || !c.HasDefault || provided[i] || c.IsGenerated() {
			continue
		}
		switch {
		case c.DefaultKnown:
			full[i] = c.DefaultValue
		case c.DefaultDeferred != nil:
			// A clock-reading DEFAULT (columnInfo.DefaultDeferred), evaluated fresh
			// for this row by running the program the schema compiled for it
			// (columnInfo.defaultProg, compileDeferredDefaults), as C codes an
			// omitted column's DEFAULT into its register (insert.c:1395, :1407,
			// :1415). A failure leaves the column unprovided so
			// checkNotNullAndDefault reports it.
			if defaultCtx == nil {
				defaultCtx = &evalCtx{tables: []tableScope{{noRowid: true}}}
			}
			prog := c.defaultProg
			if prog == nil {
				// A column list that did not come through the schema funnel
				// carries no program; eval compiles the clause itself
				// (restamp) rather than this being a call site of its own.
				prog = &selfRowExpr{expr: c.DefaultDeferred}
			}
			v, err := prog.eval(defaultCtx)
			if err != nil {
				continue
			}
			full[i] = v
		default:
			continue
		}
		provided[i] = true
	}
}

// checkNotNullAndDefault enforces NOT NULL (columnInfo.NotNull) on one row and
// rejects, as unsupported, an omitted column whose DEFAULT could not be reduced
// (DefaultKnown false). tbl.ipkIndex is exempt from both: the rowid carries its
// value.
//
// notNullCol is the index of the column that violated NOT NULL, or -1 (for
// success and for the DEFAULT error), so a conflict-aware caller can use that
// column's declared ON CONFLICT (columnInfo.NotNullConflict).
func checkNotNullAndDefault(tbl *tableMeta, full []Value, provided []bool) (notNullCol int, err error) {
	for i, c := range tbl.cols {
		if i == tbl.ipkIndex {
			continue
		}
		if !provided[i] && c.HasDefault {
			// applyColumnDefaults has already filled every default this write
			// path can produce -- a folded constant, or a clock reading
			// evaluated for this row (columnInfo.DefaultDeferred). Reaching
			// here means the clause is one it deliberately declines
			// (random()/randomblob()) or one this path could not evaluate.
			return -1, fmt.Errorf("DEFAULT column values are not supported by this write path (column %s omitted)", c.Name)
		}
		if c.NotNull && full[i].Typ == Null {
			return i, fmt.Errorf("NOT NULL constraint failed: %s.%s", tbl.name, c.Name)
		}
	}
	return -1, nil
}

// checkStrictColumnTypes is the runtime half of STRICT tables (tableMeta.strict;
// CREATE TABLE validates the declared types): it checks every value in full is
// representable in its column's type, and is one bool test for a non-STRICT
// table. It is OpTypeCheck's body (vdbe.go; emitters and C citations in
// vdbe_op.go).
//
// It runs after affinity conversion and judges only the result, as
// OP_TypeCheck does. Over "v(i INT, r REAL, t TEXT, b BLOB, y ANY) STRICT":
//
//   - i: '123', '1e3', 1.0 store as INTEGER; 1.5, '1.5', 1e300 and
//     9223372036854775808 fail "cannot store REAL value in INT column v.i";
//     'abc', '' and '0x10' fail as TEXT.
//   - r: 1 stores as 1.0; x'01' fails.
//   - t: 1.5 stores as '1.5'; a BLOB fails.
//   - b: only blobs (BLOB has no affinity).
//   - y: anything, unconverted.
//   - NULL always passes (NOT NULL runs earlier).
//
// Skipped: the INTEGER PRIMARY KEY, which C validates through the rowid path
// with "datatype mismatch" (as without STRICT); and a column with an unknown
// type, which only ALTER TABLE ADD COLUMN could build and which addColumn
// declines on STRICT tables, so it is an error rather than a silent skip.
//
// Order, as in C: NOT NULL, then type, then CHECK, then UNIQUE; within a row
// the first offending column is reported. The error is not subject to the
// conflict clause (SQLITE_CONSTRAINT_DATATYPE is raised from the opcode), so
// it is a plain error, which writeCtx.resolveHalt treats as ABORT.
func checkStrictColumnTypes(tbl *tableMeta, full []Value) error {
	if !tbl.strict {
		return nil
	}
	for i, c := range tbl.cols {
		if i == tbl.ipkIndex {
			// The rowid alias is not unchecked: C rejects a non-integer with
			// "datatype mismatch" (even under OR IGNORE). decideRowid would
			// instead auto-assign, so check here. Affinity has already turned
			// '5' and 6.0 into integers.
			if full[i].Typ != Null && full[i].Typ != Int {
				return &strictTypeError{msg: "datatype mismatch"}
			}
			continue
		}
		st, ok := strictColTypeOf(c.DeclType)
		if !ok {
			return fmt.Errorf("unsupported: column %s of STRICT table %s has no STRICT datatype (%q)", c.Name, tbl.name, c.DeclType)
		}
		if st == strictColAny || full[i].Typ == Null {
			continue
		}
		want := Null
		switch st {
		case strictColBlob:
			want = Blob
		case strictColInt, strictColInteger:
			want = Int
		case strictColReal:
			want = Float
		case strictColText:
			want = Text
		}
		if full[i].Typ != want {
			return &strictTypeError{msg: fmt.Sprintf("cannot store %s value in %s column %s.%s",
				strictValueTypeName(full[i].Typ), st, tbl.name, c.Name)}
		}
	}
	return nil
}

// strictTypeError is checkStrictColumnTypes' own error type. It carries no
// extra information -- the message is already C SQLite's, verbatim --
// and exists purely so a conflict-aware caller can TELL that this failure
// is unsoftenable and leave the statement's OR IGNORE/REPLACE/... clause
// out of it. See checkStrictColumnTypes' doc comment for the verified
// evidence that SQLite treats it that way.
type strictTypeError struct{ msg string }

func (e *strictTypeError) Error() string { return e.msg }

// strictValueTypeName is the name a STRICT type error gives the OFFENDING
// VALUE's storage class -- SQLite's own vdbeMemTypeName(), which spells the
// integer class "INT" (not "INTEGER", the way sqlite3StdType does for a
// COLUMN of that declared type). Verified directly: an integer stored into
// a BLOB column reports `cannot store INT value in BLOB column g.d`, while
// a real stored into an INTEGER-declared column reports `cannot store REAL
// value in INTEGER column g.a`.
func strictValueTypeName(t ValueType) string {
	switch t {
	case Int:
		return "INT"
	case Float:
		return "REAL"
	case Text:
		return "TEXT"
	case Blob:
		return "BLOB"
	}
	return "NULL"
}

// checkTableChecks evaluates tbl's CHECK constraints against one candidate row
// (rowid, and full in stored shape) and returns the first that is definitely
// FALSE; NULL or nonzero passes, as in C.
//
// C evaluates CHECK after NOT NULL and before UNIQUE/rowid conflicts, even for
// a row whose rowid duplicates an existing one. The row context exposes only
// tbl's own columns and rowid; a CHECK is a schema property and never sees an
// enclosing trigger's NEW/OLD.
func (db *DB) checkTableChecks(tbl *tableMeta, displayName string, rowid uint64, full []Value) error {
	return db.checkTableChecksChanged(tbl, displayName, rowid, full, nil)
}

// checkTableChecksChanged is checkTableChecks with the UPDATE rule: when chng
// is non-nil only CHECKs naming an assigned column run (checkChangeSet); nil
// means INSERT and runs all.
//
// "PRAGMA ignore_check_constraints" is honored here, where every
// program-less caller funnels (insertRowFromValues, fkStoreChildRow, ALTER
// TABLE ADD COLUMN's back-fill); its in-program twin is
// emitCheckConstraintsAction (vdbe_write.go). Each CHECK is compiled once by
// finalizeCheckConstraints (and recompiled by refreshRowPrograms after ALTER)
// and run here over a register-seeded row.
func (db *DB) checkTableChecksChanged(tbl *tableMeta, displayName string, rowid uint64, full []Value, chng *checkChangeSet) error {
	if len(tbl.checks) == 0 || db.IgnoreCheckConstraints() {
		return nil
	}
	ctx := rowEvalCtx(tbl, rowid, full, nil, nil, nil)
	for _, cc := range tbl.checks {
		if !chng.evaluates(cc.expr) {
			continue
		}
		// The compiled body, stamped on the constraint by
		// finalizeCheckConstraints (schema_write.go). A checkConstraint that
		// somehow reached tbl.checks without going through it carries none,
		// and eval compiles the body itself against this row's scope
		// (restamp, vdbe_run.go) rather than walking it.
		prog := cc.prog
		if prog == nil {
			// ignoreDbQualifier true because this IS a CHECK body: the
			// re-compile must run under the same name context
			// finalizeCheckConstraints would have given it (NC_IsCheck,
			// resolve.c:316), or a three-part reference C SQLite accepts
			// would fail to lower here.
			prog = &selfRowExpr{expr: cc.expr, ignoreDbQualifier: true}
		}
		v, err := prog.eval(ctx)
		if err != nil {
			return err
		}
		if v.Typ != Null && !isTruthy(v) {
			if cc.name != "" {
				return fmt.Errorf("CHECK constraint failed: %s", cc.name)
			}
			return fmt.Errorf("CHECK constraint failed: %s", cc.exprText)
		}
	}
	return nil
}

// checkChangeSet is C's aXRef[]/chngRowid for one UPDATE (or upsert DO UPDATE,
// or FK SET NULL/SET DEFAULT/CASCADE action): which columns are assigned and
// whether the rowid moves.
//
// An UPDATE skips a CHECK naming no assigned column
// (sqlite3ExprReferencesUpdatedColumn); a rowid reference counts only when
// the rowid changes. Over t(a,b,CHECK(a>10)) holding (5,1):
//
//	UPDATE t SET b=2                         ok
//	UPDATE t SET a=a                         FAILS   -- assigned, value irrelevant
//	UPDATE t SET b=2 WHERE a>10              ok      -- WHERE does not count
//	CHECK(0)                                 never evaluated by any update
//	id INTEGER PRIMARY KEY, CHECK(id>100):
//	  UPDATE t SET id=2                      FAILS   -- the rowid changes
//	b AS (a*2), CHECK(b<100):
//	  UPDATE t SET a=500                     FAILS   -- generated, transitively
//
// buildChangeSet runs to a fixpoint for generated columns (update.c's loop
// over TF_HasGenerated). nil means "not an UPDATE" and evaluates everything.
type checkChangeSet struct {
	cols  []bool         // len(tbl.cols); true = this statement assigns the column
	idx   map[string]int // tbl's own buildColIndex, for resolving a reference
	rowid bool           // the statement moves the rowid (a rowid target, or the IPK column)
}

// buildChangeSet resolves colIdx -- the per-SET target column indices UPDATE and
// upsert already compute (noColumnRowidTarget for a bare rowid/oid/_rowid_
// target) -- into a checkChangeSet over tbl, closing over generated columns.
func buildChangeSet(tbl *tableMeta, colIdx []int) *checkChangeSet {
	cs := &checkChangeSet{cols: make([]bool, len(tbl.cols)), idx: buildColIndex(tbl.cols)}
	for _, idx := range colIdx {
		switch {
		case idx == noColumnRowidTarget:
			cs.rowid = true
		case idx >= 0 && idx < len(tbl.cols):
			cs.cols[idx] = true
			if idx == tbl.ipkIndex {
				cs.rowid = true
			}
		}
	}
	cs.closeOverGenerated(tbl)
	return cs
}

// closeOverGenerated marks every generated column whose own generator
// expression reads a column already marked -- repeatedly, since one generated
// column may feed another (update.c's bProgress loop).
func (cs *checkChangeSet) closeOverGenerated(tbl *tableMeta) {
	if !hasGeneratedCols(tbl.cols) {
		return
	}
	for progress := true; progress; {
		progress = false
		for i, c := range tbl.cols {
			if cs.cols[i] || !c.IsGenerated() {
				continue
			}
			expr, err := cachedGeneratedExpr(c.GeneratedExpr)
			if err != nil {
				// Unparseable: mark it changed, so a CHECK over it is still
				// evaluated. Erring toward MORE checking can only reject a row
				// SQLite would have stored; erring the other way stores one it
				// would have rejected.
				cs.cols[i], progress = true, true
				continue
			}
			if cs.evaluates(expr) {
				cs.cols[i], progress = true, true
			}
		}
	}
}

// evaluates reports whether a CHECK (or generated-column) expression must be
// evaluated for this statement: C SQLite's checkConstraintExprNode walk. A
// nil receiver -- an INSERT, or a caller with no assigned-column list -- says
// yes to everything.
func (cs *checkChangeSet) evaluates(e Expr) bool {
	if cs == nil {
		return true
	}
	return cs.exprReadsChanged(e)
}

// exprReadsChanged walks e for a column reference naming an assigned column, or
// -- when the rowid is moving -- the rowid pseudo-column. A node shape this walk
// does not know is reported as READING one, which is the conservative direction
// (see closeOverGenerated). validateCheckExpr (schema_write.go) has already
// rejected subqueries, aggregates, parameters and foreign-table qualifiers from
// every expression that reaches here, so the cases below are the whole grammar.
func (cs *checkChangeSet) exprReadsChanged(e Expr) bool {
	any := func(list ...Expr) bool {
		for _, x := range list {
			if x != nil && cs.exprReadsChanged(x) {
				return true
			}
		}
		return false
	}
	switch x := e.(type) {
	case LiteralExpr, ParamExpr:
		return false
	case ColumnExpr:
		if i, ok := cs.idx[r33sFoldIdent(x.Name)]; ok {
			return cs.cols[i]
		}
		// Not a declared column: inside a CHECK the only other thing that
		// resolves is the rowid pseudo-column (validateCheckExpr guarantees it).
		if isRowidAliasName(x.Name) {
			return cs.rowid
		}
		return true
	case UnaryExpr:
		return any(x.X)
	case BinaryExpr:
		return any(x.L, x.R)
	case IsNullExpr:
		return any(x.X)
	case InExpr:
		if x.Sub != nil {
			return true
		}
		return any(x.X) || any(x.List...)
	case BetweenExpr:
		return any(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return any(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return any(x.X, x.Pattern)
	case MatchExpr:
		return any(x.X, x.Pattern)
	case CollateExpr:
		return any(x.X)
	case CastExpr:
		return any(x.X)
	case FuncExpr:
		return any(x.walkArgs()...)
	case RowExpr:
		return any(x.Elems...)
	case CaseExpr:
		if x.Base != nil && cs.exprReadsChanged(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if any(w.When, w.Then) {
				return true
			}
		}
		return x.Else != nil && cs.exprReadsChanged(x.Else)
	default:
		return true
	}
}

// Insert parses and executes a single INSERT statement against a table
// registered earlier this session (via CreateTable, or recovered by
// OpenWrite), assigning each row's rowid as either its explicit INTEGER
// PRIMARY KEY value (when the table has a rowid-alias column and that
// column's value isn't NULL/omitted) or one past the table's current
// maximum rowid, then storing the row into the table's logical row store
// (materialized into an actual b-tree only at Close -- see writer.go). It
// returns the number of rows inserted.
func (db *DB) Insert(sqlText string) (int, error) {
	return db.InsertArgs(sqlText, nil)
}

// InsertArgs is Insert extended with bound-parameter support: args[i] binds
// SQL parameter index i+1 (see ParamExpr, sql_ast.go) wherever a placeholder
// appears in a VALUES tuple. A placeholder beyond len(args) evaluates to
// NULL, matching C SQLite's own unbound-parameter behavior.
func (db *DB) InsertArgs(sqlText string, args []Value) (int, error) {
	// Compile and run through ExecArgs. The parse is kept so InsertArgs
	// rejects a statement of the wrong verb.
	if _, err := parseInsertStmt(sqlText); err != nil {
		return 0, err
	}
	n, _, err := db.ExecArgs(sqlText, args)
	return int(n), err
}

// ---- xfer-optimization eligibility (INSERT INTO t SELECT * FROM src, dest has generated columns) ----
//
// C tries xferOptimization (insert.c:3012) before the column-count check, when
// there is no column list and no INSERT trigger (insert.c:1030-1038); on
// success it skips that check entirely. For a dest with generated columns, a
// "SELECT * FROM src" yields one value per src column (generated ones
// included, insert.c:3140-3146) while the general path counts only dest's
// non-generated columns, so the arity error is correct except exactly where
// xfer applies, which xferOptimizationEligible detects.
//
// Rows here are logical, so instead of copying records this narrows the
// SELECT's rows to dest's non-generated columns and feeds them through the
// ordinary insert pipeline, which enforces indexes/CHECK/FK/NOT NULL per row.
// The gate is a narrowed subset of C's wherever C's rule only protects the raw
// copy (no CHECK/FK/index on dest at all; no DEFAULT on any non-generated
// column); narrowing only costs coverage.
//
// hasReturning is the INSERT's "stmt.returning != nil": RETURNING is a
// synthetic TEMP AFTER trigger (sqlite3AddReturning, build.c:1439;
// trigger.c:64-68), so it makes pTrigger non-nil and disables xfer
// (insert.c:1032), leaving the arity error.
//
// orAction/explicitOr are the INSERT's own; see the onError handling below.
func (db *DB) xferOptimizationEligible(dest *tableMeta, sel *SelectStmt, hasReturning bool, orAction conflictAction, explicitOr bool) (src *tableMeta, ok bool) {
	// insert.c:3036 -- "Do not attempt to process this query if there are any
	// WITH clauses attached to it."
	if len(sel.CTEs) > 0 {
		return nil, false
	}
	// insert.c:3052-3057 -- FROM clause must have exactly one term, which
	// must not be a subquery or (this engine's own addition -- C has no
	// table-valued-function FROM item at all) a table-valued function.
	if len(sel.From) != 1 {
		return nil, false
	}
	it := sel.From[0]
	if it.Subquery != nil || it.TableFunc {
		return nil, false
	}
	// insert.c:3058-3077 -- no WHERE/ORDER BY/GROUP BY/LIMIT, not a compound,
	// not DISTINCT.
	if sel.Where != nil || len(sel.OrderBy) > 0 || len(sel.GroupBy) > 0 {
		return nil, false
	}
	if sel.Limit != nil || sel.LimitParam != nil {
		return nil, false
	}
	if len(sel.Compound) > 0 || sel.Distinct {
		return nil, false
	}
	// insert.c:3078-3086 -- the result set must be exactly the bare "*"
	// operator (not "t.*", not "*, *", not "*, col").
	if len(sel.Columns) != 1 || !sel.Columns[0].Star || sel.Columns[0].StarQualifier != "" {
		return nil, false
	}
	// insert.c:3092-3100 -- FROM clause must name a REAL table (not a view,
	// not a CTE -- findTableMetaIn only ever matches db.tables, matching
	// IsOrdinaryTable), and it must not be the destination itself.
	src = db.findTableMetaIn(fromItemScope(it), it.Table)
	if src == nil || src == dest {
		return nil, false
	}
	// insert.c:3101-3103 -- "source and destination must both be WITHOUT
	// ROWID or not". Narrowed from "must match" to "neither is WITHOUT
	// ROWID": the both-WITHOUT-ROWID case C would still allow is left
	// declined rather than reproducing pkIndex equality here.
	if dest.withoutRowid || src.withoutRowid {
		return nil, false
	}
	// insert.c:3107 -- "Number of columns must be the same in tab1 and tab2".
	if len(dest.cols) != len(src.cols) {
		return nil, false
	}
	// insert.c:3110-3112 -- "Both tables must have the same INTEGER PRIMARY
	// KEY" (compared by COLUMN INDEX, not name -- -1 for neither).
	if dest.ipkIndex != src.ipkIndex {
		return nil, false
	}
	// insert.c:3113-3115 -- "Cannot feed from a non-strict into a strict
	// table". Narrowed from that asymmetry to "dest is never STRICT": when
	// dest is STRICT, C additionally requires src STRICT too, which this
	// skips checking and simply declines instead (a strict subset of C's own
	// rejection).
	if dest.strict {
		return nil, false
	}
	// xfer is only attempted with no INSERT trigger (insert.c:1029-1034) and
	// no RETURNING; otherwise C applies the arity check, so keep the error.
	if db.tableHasTriggers(dest.name, dest.isTemp, triggerInsert) || hasReturning {
		return nil, false
	}
	// insert.c:3046-3049 resolves OE_Default to pDest->keyConf (the dest
	// table's own declared "INTEGER PRIMARY KEY ... ON CONFLICT <x>", if it
	// has one) or OE_Abort otherwise -- so "no explicit OR clause" is NOT
	// automatically the safe conflictAbort case here whenever dest declares
	// its own IPK conflict action.
	effectiveOnError := orAction
	if !explicitOr {
		effectiveOnError = conflictAbort
		if dest.ipkIndex >= 0 {
			effectiveOnError = dest.cols[dest.ipkIndex].RowidConflict
		}
	}
	// insert.c:3247-3249: an onError other than OE_Abort/OE_Rollback makes
	// xferOptimization run guarded and return 0 (insert.c:3388), so the
	// caller also compiles the ordinary path, whose arity check is a hard
	// prepare-time error (insert.c:1244-1253). So only ABORT/ROLLBACK (or no
	// clause) are eligible; OR IGNORE/REPLACE/FAIL get the arity error.
	if effectiveOnError != conflictAbort && effectiveOnError != conflictRollback {
		return nil, false
	}
	srcIdx := buildColIndex(src.cols)
	destIdx := buildColIndex(dest.cols)
	for i := range dest.cols {
		dc, sc := &dest.cols[i], &src.cols[i]
		// insert.c:3143-3146 -- both columns must be generated, or neither.
		if dc.IsGenerated() != sc.IsGenerated() {
			return nil, false
		}
		if dc.IsGenerated() {
			// insert.c:3151-3159 -- "the transfer is only allowed if both the
			// source and destination tables have the exact same expressions
			// for generated columns", compared via sqlite3ExprCompare(0,
			// srcExpr, destExpr, -1) -- see genExprEqualByPosition's own doc
			// comment for why that -1 makes this a POSITIONAL (index-based),
			// not name-based, comparison.
			dExpr, derr := cachedGeneratedExpr(dc.GeneratedExpr)
			sExpr, serr := cachedGeneratedExpr(sc.GeneratedExpr)
			if derr != nil || serr != nil || !genExprEqualByPosition(sExpr, srcIdx, dExpr, destIdx) {
				return nil, false
			}
		} else {
			// insert.c:3171-3185 compares non-first columns' DEFAULT source text.
			// Narrowed to "no column declares a default": DefaultValue is already
			// folded, so the source text is not available here.
			if dc.HasDefault || sc.HasDefault {
				return nil, false
			}
		}
		// insert.c:3161-3163 -- "Affinity must be the same on all columns".
		if dc.Aff != sc.Aff {
			return nil, false
		}
		// insert.c:3164-3167 -- "Collating sequence must be the same on all
		// columns".
		if !equalFoldName(dc.Collation, sc.Collation) {
			return nil, false
		}
		// insert.c:3168-3170 -- "tab2 must be NOT NULL if tab1 is" (dest
		// stricter than src is fine; the reverse is not).
		if dc.NotNull && !sc.NotNull {
			return nil, false
		}
	}
	// insert.c:3187-3211 -- xferCompatibleIndex's per-index matching and the
	// CHECK-constraint-list comparison, both narrowed to "dest has none at
	// all" (rather than "every index/CHECK dest has, src also has an
	// equivalent of"): existing per-row enforcement (checkTableChecks,
	// findRowConflicts) already applies dest's own CHECK/UNIQUE constraints
	// to every inserted row regardless, so this only widens what's declined,
	// never what could be silently under-enforced.
	if len(dest.checks) > 0 {
		return nil, false
	}
	for _, idx := range db.indexes {
		if equalFoldName(idx.table, dest.name) {
			return nil, false
		}
	}
	// insert.c:3213-3225 -- narrowed from "resolves + gated on PRAGMA
	// foreign_keys" to "dest declares no foreign key at all": existing
	// per-row FK enforcement (fk.go) already applies whenever the pragma is
	// on, regardless of this path, so declining here only widens coverage
	// lost, never correctness.
	if len(db.fkOf(dest)) > 0 {
		return nil, false
	}
	// insert.c:3226-3228 (PRAGMA count_changes) is irrelevant here: this path
	// runs the ordinary per-row insert pipeline, which already accounts rows
	// however that pragma requires, unlike xferOptimization's raw copy.
	return src, true
}

// genExprEqualByPosition is exprEqual (sql_group.go) except that a bare column
// compares by its resolved index in its own table (aIdx/bIdx), not by name, as
// sqlite3ExprCompare does for TK_COLUMN (expr.c:6601-6626, :6640).
// xferOptimization passes iTab=-1 (insert.c:3152-3154), so table identity is
// not checked (expr.c:6642): "TYPEOF(c1)" matches "typeof(y)" when both are
// column 1. Node kinds exprEqual does not handle return false.
func genExprEqualByPosition(a Expr, aIdx map[string]int, b Expr, bIdx map[string]int) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case LiteralExpr:
		y, ok := b.(LiteralExpr)
		return ok && valueExactEqual(x.Val, y.Val)
	case ColumnExpr:
		y, ok := b.(ColumnExpr)
		if !ok {
			return false
		}
		ai, aok := aIdx[r33sFoldIdent(x.Name)]
		bi, bok := bIdx[r33sFoldIdent(y.Name)]
		return aok && bok && ai == bi
	case UnaryExpr:
		y, ok := b.(UnaryExpr)
		return ok && x.Op == y.Op && genExprEqualByPosition(x.X, aIdx, y.X, bIdx)
	case BinaryExpr:
		y, ok := b.(BinaryExpr)
		return ok && x.Op == y.Op && genExprEqualByPosition(x.L, aIdx, y.L, bIdx) && genExprEqualByPosition(x.R, aIdx, y.R, bIdx)
	case IsNullExpr:
		y, ok := b.(IsNullExpr)
		return ok && x.Not == y.Not && genExprEqualByPosition(x.X, aIdx, y.X, bIdx)
	case CollateExpr:
		y, ok := b.(CollateExpr)
		return ok && equalFoldName(x.Name, y.Name) && genExprEqualByPosition(x.X, aIdx, y.X, bIdx)
	case FuncExpr:
		y, ok := b.(FuncExpr)
		if !ok || !equalFoldName(x.Name, y.Name) || x.Star != y.Star || x.Distinct != y.Distinct || len(x.Args) != len(y.Args) {
			return false
		}
		for i := range x.Args {
			if !genExprEqualByPosition(x.Args[i], aIdx, y.Args[i], bIdx) {
				return false
			}
		}
		return true
	case CastExpr:
		y, ok := b.(CastExpr)
		return ok && x.Type == y.Type && genExprEqualByPosition(x.X, aIdx, y.X, bIdx)
	default:
		return false
	}
}

// noColumnRowidTarget is returned by resolveColumnOrRowidTarget/
// resolveNamedColumns when a name resolves to the rowid pseudo-column and the
// table has no INTEGER PRIMARY KEY (with one, the alias resolves to
// tbl.ipkIndex). Distinct from every column index and from -1.
const noColumnRowidTarget = -2

// resolveColumnOrRowidTarget resolves one INSERT column-list or UPDATE
// SET-target name against tbl. A real column (case-insensitive) wins, so a
// column literally named "oid" shadows the pseudo-column, as on the read side.
// Otherwise a rowid alias resolves to tbl.ipkIndex, or noColumnRowidTarget if
// there is no IPK. ok is false when neither matches.
func resolveColumnOrRowidTarget(tbl *tableMeta, name string) (idx int, ok bool) {
	for j, c := range tbl.cols {
		if equalFoldName(c.Name, name) {
			return j, true
		}
	}
	// A WITHOUT ROWID table has no rowid/oid/_rowid_ pseudo-column at all
	// (verified directly: C SQLite errors "no such column: rowid"
	// against one) -- checked BEFORE isRowidAliasName below, which would
	// otherwise treat it exactly like an ordinary table with no INTEGER
	// PRIMARY KEY column (tbl.ipkIndex, always -1 here too, is
	// indistinguishable between the two on its own).
	if tbl.withoutRowid {
		return -1, false
	}
	if isRowidAliasName(name) {
		if tbl.ipkIndex >= 0 {
			return tbl.ipkIndex, true
		}
		return noColumnRowidTarget, true
	}
	return -1, false
}

// resolveNamedColumns resolves an INSERT column list (nil for "every column")
// to indices into tbl.cols (or noColumnRowidTarget/tbl.ipkIndex for a rowid
// alias). displayName is the table as written, for the error message. A nil
// list yields nil: SQLite only recognizes rowid aliases in an explicit list.
func resolveNamedColumns(tbl *tableMeta, displayName string, names []string) ([]int, error) {
	if names == nil {
		return nil, nil
	}
	colIdx := make([]int, len(names))
	for i, cname := range names {
		idx, ok := resolveColumnOrRowidTarget(tbl, cname)
		if !ok {
			return nil, fmt.Errorf("engine: table %s has no column named %s", displayName, cname)
		}
		// C SQLite: "cannot INSERT into generated column". A generated
		// column's value is always derived from the row, never supplied --
		// checked here so both the VDBE write compiler (which resolves its
		// column mapping through this function) and buildFullRow reject it.
		if idx >= 0 && idx < len(tbl.cols) && tbl.cols[idx].IsGenerated() {
			return nil, fmt.Errorf("engine: cannot INSERT into generated column %q", tbl.cols[idx].Name)
		}
		colIdx[i] = idx
	}
	return colIdx, nil
}

func buildFullRow(tbl *tableMeta, displayName string, colsGiven bool, colIdx []int, rowVals []Value) (full []Value, rowidOverride *Value, notNullCol int, err error) {
	if lerr := checkRowRecordLength(rowVals); lerr != nil {
		return nil, nil, -1, lerr
	}
	full = make([]Value, len(tbl.cols))
	provided := make([]bool, len(tbl.cols))
	for i := range full {
		full[i] = Value{Typ: Null}
	}
	// rowidOverride, when non-nil, is the value an explicit rowid/oid/
	// _rowid_ column-list entry supplied for a table with NO INTEGER
	// PRIMARY KEY column (tbl.ipkIndex < 0, so there is no real full[]
	// slot for decideRowid to read it back from -- see
	// resolveColumnOrRowidTarget's noColumnRowidTarget sentinel). nil
	// means no such entry was named (or colsGiven is false, which can
	// never name one at all).
	if !colsGiven {
		// A generated column takes NO value from a column-list-less INSERT:
		// C SQLite counts only the non-generated columns, so
		// "CREATE TABLE gc(a, b AS (a*2))" is filled by
		// "INSERT INTO gc VALUES(5)", not VALUES(5, 10). Values map onto the
		// non-generated columns in declaration order; each generated slot is
		// filled by computeGeneratedInto afterwards.
		if hasGeneratedCols(tbl.cols) {
			want := 0
			for _, c := range tbl.cols {
				if !c.IsGenerated() {
					want++
				}
			}
			if len(rowVals) != want {
				// insert.c:1257 -- "%d values for %d columns", values first, and it
				// names neither the table nor the word "supplied".
				return nil, nil, -1, fmt.Errorf("engine: %d values for %d columns", len(rowVals), want)
			}
			k := 0
			for i, c := range tbl.cols {
				if c.IsGenerated() {
					continue
				}
				full[i] = rowVals[k]
				provided[i] = true
				k++
			}
		} else {
			if len(rowVals) != len(tbl.cols) {
				return nil, nil, -1, fmt.Errorf("engine: table %s has %d columns but %d values were supplied", displayName, len(tbl.cols), len(rowVals))
			}
			copy(full, rowVals)
			for i := range provided {
				provided[i] = true
			}
		}
	} else {
		if len(rowVals) != len(colIdx) {
			return nil, nil, -1, fmt.Errorf("engine: %d columns named but %d values supplied", len(colIdx), len(rowVals))
		}
		// A column named twice in the column list ("INSERT INTO t(a,b,a)") is
		// legal in SQLite: the first value wins. The rowid slot (the IPK under
		// any spelling, or noColumnRowidTarget) is the exception: it is
		// last-wins in C ("t(id, rowid, x) VALUES(1, 2, 'a')" sets rowid 2;
		// reordered, 1), so it is overwritten unconditionally below.
		for i, idx := range colIdx {
			if idx == noColumnRowidTarget {
				v := rowVals[i]
				rowidOverride = &v
				continue
			}
			if tbl.ipkIndex >= 0 && idx == tbl.ipkIndex {
				full[idx] = rowVals[i]
				provided[idx] = true
				continue
			}
			if provided[idx] {
				continue
			}
			full[idx] = rowVals[i]
			provided[idx] = true
		}
	}

	applyColumnDefaults(tbl, full, provided)

	applyRowAffinities(tbl.cols, full)

	// Generated columns are derived from the row's other values, so they are
	// computed AFTER affinities have been applied to those values (real
	// SQLite likewise evaluates the expression against the stored,
	// affinity-coerced row) and BEFORE NOT NULL is enforced, so a NOT NULL
	// generated column is checked against its computed value.
	if err := computeGeneratedInto(tbl.name, tbl.cols, full); err != nil {
		return nil, nil, -1, err
	}
	// A generated column is never "omitted": mark every one provided so
	// checkNotNullAndDefault's omitted-DEFAULT rejection does not fire for it.
	for i, c := range tbl.cols {
		if c.IsGenerated() {
			provided[i] = true
		}
	}

	if nnCol, nerr := checkNotNullAndDefault(tbl, full, provided); nerr != nil {
		return nil, nil, nnCol, fmt.Errorf("engine: INSERT into %s: %w", displayName, nerr)
	}
	// STRICT datatype check, after NOT NULL and before CHECK, OP_TypeCheck's
	// position. notNullCol stays -1: a type violation has no per-constraint
	// conflict action.
	if serr := checkStrictColumnTypes(tbl, full); serr != nil {
		return nil, nil, -1, fmt.Errorf("engine: INSERT into %s: %w", displayName, serr)
	}
	return full, rowidOverride, -1, nil
}

// decideRowid resolves the rowid a row will occupy: its INTEGER PRIMARY KEY
// value (NULLing that column, since the rowid carries it), an explicit rowid
// alias value for a table with no IPK (rowidOverride), or one past the current
// maximum.
//
// rowidOverride is strict: a non-integer is "datatype mismatch", after INTEGER
// affinity (so '5' is 5), via rowidFromValue. NULL auto-assigns, like an
// omitted IPK. The IPK branch keeps its lenient auto-assign fallback.
//
// Its one caller is CREATE TABLE ... AS SELECT, whose table is never
// AUTOINCREMENT; compiled INSERTs allocate through OP_NewRowid.
func (db *DB) decideRowid(tbl *tableMeta, full []Value, rowidOverride *Value) (uint64, error) {
	if rowidOverride != nil {
		if rowidOverride.Typ == Null {
			return db.autoRowid(tbl)
		}
		v := applyAffinityToValue(*rowidOverride, affInteger)
		rowid, err := rowidFromValue(v)
		if err != nil {
			return 0, err
		}
		return rowid, nil
	}
	if tbl.ipkIndex >= 0 && full[tbl.ipkIndex].Typ == Int {
		rowid := uint64(full[tbl.ipkIndex].I)
		full[tbl.ipkIndex] = Value{Typ: Null} // stored as NULL; the rowid itself carries the value
		return rowid, nil
	}
	rowid, err := db.autoRowid(tbl)
	if err != nil {
		return 0, err
	}
	if tbl.ipkIndex >= 0 {
		full[tbl.ipkIndex] = Value{Typ: Null}
	}
	return rowid, nil
}

// autoRowid is the rowid an insert that names none gets: the table's largest
// plus one, or the next one in this connection's range when it has one
// (SetRowidRange). Every insert path allocates through it -- OP_NewRowid and
// CREATE TABLE ... AS SELECT's direct inserts alike.
func (db *DB) autoRowid(tbl *tableMeta) (uint64, error) {
	if db != nil && db.rowidLo < db.rowidHi && !tbl.isTemp {
		return db.nextRowidInRange(tbl)
	}
	return nextRowidForTable(tbl)
}

func (db *DB) insertRowFromValues(tbl *tableMeta, displayName string, colsGiven bool, colIdx []int, rowVals []Value) (uint64, error) {
	// Defense-in-depth (table_load.go's package doc comment): every known
	// caller of this function already gated tbl through ensureTableLoaded at
	// its own target-resolution choke point, so this is a cheap (tbl.loaded)
	// no-op there -- but this is also the ONE place every insert path actually
	// stores a row, so gating it too protects against a caller neither of those
	// choke points covers.
	if err := db.ensureTableLoaded(tbl); err != nil {
		return 0, err
	}
	full, rowidOverride, _, err := buildFullRow(tbl, displayName, colsGiven, colIdx, rowVals)
	if err != nil {
		return 0, err
	}
	rowid, err := db.decideRowid(tbl, full, rowidOverride)
	if err != nil {
		return 0, fmt.Errorf("engine: INSERT into %s: %w", displayName, err)
	}

	if cerr := db.checkTableChecks(tbl, displayName, rowid, full); cerr != nil {
		return 0, fmt.Errorf("engine: INSERT into %s: %w", displayName, cerr)
	}

	if _, exists := tbl.rows.get(rowid); exists {
		return 0, fmt.Errorf("engine: INSERT into %s: UNIQUE constraint failed: duplicate rowid %d", displayName, int64(rowid))
	}
	tbl.putRow(rowid, full)
	db.noteRowChange(tbl, RowInsert, rowid, nil, full)
	db.fkRowMutated(tbl, rowid, rowid, nil, full, nil)
	// rowid is a signed SQLite INTEGER stored bit-for-bit in a uint64 (see
	// rowidLess' doc comment in btree_write.go); int64(rowid) is its correct
	// reading. last_insert_rowid() tracks the LAST row of a
	// (possibly multi-row) INSERT, matching C SQLite -- EXCEPT an insert
	// into a WITHOUT ROWID table never updates it at all (verified directly
	// against C SQLite: it stays whatever it was before), since rowid
	// here is only this write path's own internal, opaque row-store key
	// (tableMeta.withoutRowid's doc comment), never a real rowid.
	if !tbl.withoutRowid {
		db.lastInsertRowid = int64(rowid)
	}
	return rowid, nil
}

// validateSelectTablesExist confirms every table named anywhere in stmt (FROM,
// compound arms, and subqueries in FROM/WHERE/HAVING/select list/ORDER BY)
// exists, because C validates table references at prepare time: "... WHERE
// 1=0 AND (SELECT 1 FROM nosuchtable)" is an error even though the subquery is
// never evaluated (insert5.test). CREATE TABLE AS calls it before running its
// source.
func validateSelectTablesExist(pager *ReadOnlyPager, stmt *SelectStmt) error {
	if stmt == nil {
		return nil
	}
	// The statement's CTEs must be in scope, or a FROM item naming one
	// would be rejected as "no such table" (joinD.test).
	if len(stmt.CTEs) > 0 {
		pop := pager.pushCTEScope(stmt.CTEs)
		defer pop()
	}
	if err := validateFromTablesExist(pager, stmt.From); err != nil {
		return err
	}
	for _, c := range stmt.Columns {
		if err := validateExprTablesExist(pager, c.Expr); err != nil {
			return err
		}
	}
	if err := validateExprTablesExist(pager, stmt.Where); err != nil {
		return err
	}
	for _, g := range stmt.GroupBy {
		if err := validateExprTablesExist(pager, g); err != nil {
			return err
		}
	}
	if err := validateExprTablesExist(pager, stmt.Having); err != nil {
		return err
	}
	for _, arm := range stmt.Compound {
		if err := validateSelectTablesExist(pager, arm.Stmt); err != nil {
			return err
		}
	}
	for _, ot := range stmt.OrderBy {
		if err := validateExprTablesExist(pager, ot.Expr); err != nil {
			return err
		}
	}
	return nil
}

// validateFromTablesExist checks every non-derived table in items exists,
// recursing into derived tables via validateSelectTablesExist. A view counts
// as existing without checking its body, since C resolves a view's references
// lazily (view.go).
func validateFromTablesExist(pager *ReadOnlyPager, items []FromItem) error {
	for _, it := range items {
		if it.Subquery != nil {
			if err := validateSelectTablesExist(pager, it.Subquery); err != nil {
				return err
			}
			continue
		}
		// An in-scope CTE exists, like a view below; problems in its body
		// surface when the statement runs.
		if _, ok := pager.lookupCTE(it.Table); ok {
			continue
		}
		// For a cross-database write (attachedReaders set on the snapshot),
		// resolve the item against the pager that OWNS it -- a foreign-qualified
		// name, or an unqualified name living only in an attached database, is a
		// real, existing table there and must not be rejected here. rp is the
		// snapshot pager itself for every ordinary single-database write.
		rp := pager
		if len(pager.attachedReaders) > 0 {
			if owner, oerr := pager.itemOwner(it); oerr == nil {
				rp = owner
			}
		}
		if _, err := rp.resolveTableIn(fromItemScope(it), it.Table); err != nil {
			if _, ok, verr := rp.resolveViewByNameIn(fromItemScope(it), it.Table); verr == nil && ok {
				continue
			}
			// A VIRTUAL TABLE exists too, and resolveTableIn cannot see one: its
			// stored schema is "CREATE VIRTUAL TABLE ...", which that parser
			// rejects with "CREATE TABLE: expected TABLE". Without this,
			// "INSERT INTO plain SELECT x FROM ftstable" failed outright -- a
			// pre-existing wrong answer (C SQLite inserts the rows), found
			// while wiring INSERT ... SELECT into the fts3 write path, where the
			// same guard rejected an fts table sourcing ITSELF.
			if _, _, ok, verr := rp.createdVtabDef(it.Table); verr == nil && ok {
				continue
			}
			// A schema catalog exists too. resolveTableIn refuses one once a TEMP
			// object exists (its raw scan would mix temp rows into sqlite_master),
			// but this only asks whether the name exists, which temp_catalog.go's
			// filtered view answers. Without it "INSERT INTO objlist SELECT ...
			// FROM sqlite_master" (alter.test) failed after the first CREATE TEMP.
			if _, ok, cerr := rp.schemaCatalogSourceScope(it); cerr == nil && ok {
				continue
			}
			// An eponymous virtual-table module ("generate_series(1,10)",
			// "json_each(x)") exists with no schema row. Checked last, so a real
			// table or view of the same name shadows it, as in SQLite and
			// resolveFrom.
			if isEponymousVtabName(it.Table) {
				continue
			}
			return err
		}
	}
	return nil
}

// validateExprTablesExist walks e looking for any subquery (SubqueryExpr,
// ExistsExpr, or InExpr.Sub) and recurses into its statement tree via
// validateSelectTablesExist; every other node shape is walked purely to
// reach a subquery that might be nested inside it (a function argument, a
// CASE branch, ...) -- it never itself resolves a column or evaluates
// anything.
func validateExprTablesExist(pager *ReadOnlyPager, e Expr) error {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr, ColumnExpr:
		return nil
	case UnaryExpr:
		return validateExprTablesExist(pager, x.X)
	case BinaryExpr:
		if err := validateExprTablesExist(pager, x.L); err != nil {
			return err
		}
		return validateExprTablesExist(pager, x.R)
	case IsNullExpr:
		return validateExprTablesExist(pager, x.X)
	case InExpr:
		if err := validateExprTablesExist(pager, x.X); err != nil {
			return err
		}
		if x.Sub != nil {
			return validateSelectTablesExist(pager, x.Sub)
		}
		for _, it := range x.List {
			if err := validateExprTablesExist(pager, it); err != nil {
				return err
			}
		}
		return nil
	case BetweenExpr:
		if err := validateExprTablesExist(pager, x.X); err != nil {
			return err
		}
		if err := validateExprTablesExist(pager, x.Lo); err != nil {
			return err
		}
		return validateExprTablesExist(pager, x.Hi)
	case LikeExpr:
		if err := validateExprTablesExist(pager, x.X); err != nil {
			return err
		}
		if err := validateExprTablesExist(pager, x.Pattern); err != nil {
			return err
		}
		return validateExprTablesExist(pager, x.Escape)
	case GlobExpr:
		if err := validateExprTablesExist(pager, x.X); err != nil {
			return err
		}
		return validateExprTablesExist(pager, x.Pattern)
	case CollateExpr:
		return validateExprTablesExist(pager, x.X)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if err := validateExprTablesExist(pager, a); err != nil {
				return err
			}
		}
		return nil
	case CastExpr:
		return validateExprTablesExist(pager, x.X)
	case CaseExpr:
		if x.Base != nil {
			if err := validateExprTablesExist(pager, x.Base); err != nil {
				return err
			}
		}
		for _, w := range x.Whens {
			if err := validateExprTablesExist(pager, w.When); err != nil {
				return err
			}
			if err := validateExprTablesExist(pager, w.Then); err != nil {
				return err
			}
		}
		if x.Else != nil {
			return validateExprTablesExist(pager, x.Else)
		}
		return nil
	case SubqueryExpr:
		return validateSelectTablesExist(pager, x.Stmt)
	case ExistsExpr:
		return validateSelectTablesExist(pager, x.Stmt)
	default:
		return nil
	}
}

// selectCallsNondeterministicFunc reports whether stmt's tree (CTE bodies,
// compound arms, derived tables) calls random() or randomblob() in a select
// list, WHERE, GROUP BY, HAVING or ORDER BY expression.
func selectCallsNondeterministicFunc(stmt *SelectStmt) bool {
	if stmt == nil {
		return false
	}
	for _, def := range stmt.CTEs {
		if selectCallsNondeterministicFunc(def.Select) {
			return true
		}
	}
	for _, it := range stmt.From {
		if it.Subquery != nil && selectCallsNondeterministicFunc(it.Subquery) {
			return true
		}
		if exprCallsNondeterministicFunc(it.On) {
			return true
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star && exprCallsNondeterministicFunc(c.Expr) {
			return true
		}
	}
	if exprCallsNondeterministicFunc(stmt.Where) {
		return true
	}
	for _, g := range stmt.GroupBy {
		if exprCallsNondeterministicFunc(g) {
			return true
		}
	}
	if exprCallsNondeterministicFunc(stmt.Having) {
		return true
	}
	for _, ot := range stmt.OrderBy {
		if exprCallsNondeterministicFunc(ot.Expr) {
			return true
		}
	}
	for _, arm := range stmt.Compound {
		if selectCallsNondeterministicFunc(arm.Stmt) {
			return true
		}
	}
	return false
}

// exprCallsNondeterministicFunc is selectCallsNondeterministicFunc's
// expression walk, mirroring exprReferencesTable's (cte.go) exhaustive
// type switch over every Expr node kind, except it looks for a call to
// random()/randomblob() (case-insensitive) instead of a table reference.
func exprCallsNondeterministicFunc(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr, ColumnExpr:
		return false
	case FuncExpr:
		switch r33sFoldIdent(x.Name) {
		case "random", "randomblob":
			return true
		}
		for _, a := range x.walkArgs() {
			if exprCallsNondeterministicFunc(a) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return exprCallsNondeterministicFunc(x.X)
	case BinaryExpr:
		return exprCallsNondeterministicFunc(x.L) || exprCallsNondeterministicFunc(x.R)
	case IsNullExpr:
		return exprCallsNondeterministicFunc(x.X)
	case InExpr:
		if x.Sub != nil && selectCallsNondeterministicFunc(x.Sub) {
			return true
		}
		if exprCallsNondeterministicFunc(x.X) {
			return true
		}
		for _, it := range x.List {
			if exprCallsNondeterministicFunc(it) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return exprCallsNondeterministicFunc(x.X) || exprCallsNondeterministicFunc(x.Lo) || exprCallsNondeterministicFunc(x.Hi)
	case LikeExpr:
		return exprCallsNondeterministicFunc(x.X) || exprCallsNondeterministicFunc(x.Pattern) || exprCallsNondeterministicFunc(x.Escape)
	case GlobExpr:
		return exprCallsNondeterministicFunc(x.X) || exprCallsNondeterministicFunc(x.Pattern)
	case CollateExpr:
		return exprCallsNondeterministicFunc(x.X)
	case CastExpr:
		return exprCallsNondeterministicFunc(x.X)
	case CaseExpr:
		if x.Base != nil && exprCallsNondeterministicFunc(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if exprCallsNondeterministicFunc(w.When) || exprCallsNondeterministicFunc(w.Then) {
				return true
			}
		}
		return x.Else != nil && exprCallsNondeterministicFunc(x.Else)
	case SubqueryExpr:
		return selectCallsNondeterministicFunc(x.Stmt)
	case ExistsExpr:
		return selectCallsNondeterministicFunc(x.Stmt)
	default:
		return false
	}
}

// isCreateIndexStmt reports whether toks (a lexed statement already known to
// start with CREATE) is a CREATE INDEX or CREATE UNIQUE INDEX, as opposed to
// CREATE TABLE (or CREATE TEMP/TEMPORARY TABLE): only enough lookahead to
// distinguish the statement kind, not a full parse.
func isCreateIndexStmt(toks []token) bool {
	i := 1
	if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "UNIQUE" {
		i++
	}
	return i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "INDEX"
}

// isCreateViewStmt reports whether toks (a lexed statement already known to
// start with CREATE) is a CREATE [TEMP|TEMPORARY] VIEW, as opposed to CREATE
// TABLE or CREATE [UNIQUE] INDEX.
func isCreateViewStmt(toks []token) bool {
	i := 1
	if i < len(toks) && toks[i].kind == tkIdent && (toks[i].upper() == "TEMP" || toks[i].upper() == "TEMPORARY") {
		i++
	}
	return i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "VIEW"
}

// writeDispatchKeyword returns the keyword Exec/ExecArgs/ParseParamInfo route
// on: toks[0] upper-cased, or for "WITH [RECURSIVE] <cte-list> ..." the verb
// after the CTE list (found by parsing the WITH clause; see
// verbAfterLeadingWith, sql_parser.go). "" if toks is empty or does not start
// with an identifier.
func writeDispatchKeyword(trimmed string, toks []token) string {
	kw, _ := writeDispatchKeywordAt(trimmed, toks)
	return kw
}

// writeDispatchKeywordAt is writeDispatchKeyword plus the verb's index in toks
// (0 for the ordinary spelling), from one parse of the WITH clause, for callers
// that read operands positionally. verbAt is 0 when kw is not a real verb.
func writeDispatchKeywordAt(trimmed string, toks []token) (kw string, verbAt int) {
	if len(toks) == 0 || toks[0].kind != tkIdent {
		return "", 0
	}
	kw = strings.ToUpper(toks[0].text)
	if kw != "WITH" {
		return kw, 0
	}
	if verb, at, ok := verbAfterLeadingWithAt(trimmed, toks); ok {
		return verb, at
	}
	return kw, 0
}

// Exec runs one statement with no bound parameters. Like ExecArgs, it compiles
// to a write Program and runs it on the VM -- there is no other executor.
func (db *DB) Exec(sqlText string) error {
	_, _, err := db.ExecArgs(sqlText, nil)
	return err
}

// unsupportedStatementErr is the one "this write path does not run that"
// error. It is raised at COMPILE time, by the write compiler's own dispatch
// tail (compileWriteProgram, vdbe_write.go), for every verb it has no case
// for -- which is what RULE #1 asks for.
func unsupportedStatementErr(trimmed string) error {
	// A statement starting WITH, SELECT or VALUES is a query that failed
	// to parse, so report the parser's error ("incomplete input",
	// "duplicate WITH table name: r") rather than the list of verbs.
	if toks, lerr := lex(trimmed); lerr == nil && len(toks) > 0 && toks[0].kind == tkIdent {
		switch strings.ToUpper(toks[0].text) {
		case "WITH", "SELECT", "VALUES":
			if _, perr := ParseSelect(trimmed); perr != nil {
				return perr
			}
		}
	}
	return fmt.Errorf("engine: unsupported statement (this write path only supports CREATE TABLE/VIEW/TRIGGER, CREATE/DROP INDEX, DROP TABLE/VIEW/TRIGGER, INSERT, UPDATE, DELETE, ATTACH/DETACH, BEGIN/COMMIT/END/ROLLBACK, and SAVEPOINT/RELEASE/ROLLBACK TO): %q", trimmed)
}

// ExecArgs is Exec with bound parameters: args[i] binds parameter i+1. It
// returns rows affected and this *DB's last_insert_rowid after the statement
// (only INSERT changes it, as with sqlite3_last_insert_rowid()).
func (db *DB) ExecArgs(sqlText string, args []Value) (rowsAffected int64, lastInsertID int64, err error) {
	// Deferred FIRST so it runs LAST: a parse error raised at end of input is
	// "incomplete input", whatever the grammar was in the middle of
	// (parse.y:44-51 -- see incompleteIfAtEOF).
	defer func() { err = incompleteIfAtEOF(err) }()
	if CommitOrderHookForTest != nil {
		db.lastExecSQL, db.lastExecArgs = sqlText, args
	}
	logMark := len(db.changeLog)
	defer func() {
		if perr := db.takeDeferredErr(); perr != nil && err == nil {
			err = perr
		}
		if err == nil {
			db.journalDDL(sqlText, logMark)
		}
	}()
	// "EXPLAIN <stmt>" runs nothing and belongs on the query side; like
	// sqlite3_exec, step the description and discard it (explain.go). The
	// driver routes EXPLAIN itself, but engine-direct callers (the corpus)
	// come through here.
	if IsExplainStatement(sqlText) {
		p, perr := db.SnapshotPager()
		if perr != nil {
			return 0, db.lastInsertRowid, perr
		}
		if _, _, qerr := p.QueryArgs(sqlText, args); qerr != nil {
			return 0, db.lastInsertRowid, qerr
		}
		return 0, db.lastInsertRowid, nil
	}
	// Every statement compiles to a write Program and runs on the VM; a
	// shape the codegen does not model is a compile-time error (RULE #1).
	// A statement targeting an ATTACHed database is delegated whole to that
	// database's write session (attach_write.go); this sits here because
	// DDL reaches db.CreateTable through OpDdl, bypassing the dispatcher.
	// "PRAGMA query_only" refuses a write before routing or compiling, as
	// C's OP_Transaction does (queryOnlyRefusesWrite, pragma.go).
	if qerr := db.queryOnlyRefusesWrite(sqlText); qerr != nil {
		return 0, 0, qerr
	}
	// ...and a FAILED SCHEMA LOAD refuses everything but the one pragma that
	// lifts it, which is C SQLite's own state after a
	// "PRAGMA writable_schema=RESET" whose catalog held an out-of-range
	// rootpage -- see schemaCorruptRefusesStatement
	// (schema_reload_rootpage.go) for the C citations, the measured table and
	// the one way this is deliberately coarser.
	if cerr := db.schemaCorruptRefusesStatement(sqlText); cerr != nil {
		return 0, 0, cerr
	}
	// "PRAGMA defer_foreign_keys" set outside a transaction declines any
	// later top-level statement this session cannot classify as keeping or
	// clearing the flag (deferFKAutocommitGuard). topLevel is taken before
	// db.stmtDepth++ so a nested trigger-body statement never counts.
	topLevel := db.stmtDepth == 0
	if topLevel {
		db.schemaSeqReserved = [2]uint64{} // see nextSchemaSeq
	}
	if derr := db.deferFKAutocommitGuard(sqlText, topLevel); derr != nil {
		return 0, 0, derr
	}
	// Fold this statement into the wal-index state that decides whether
	// "PRAGMA locking_mode=normal" can leave exclusive mode (walIndexKind,
	// pragma.go). Like C's transition it runs after the statement, only at
	// the outermost level. A failed statement is unknown, permanently
	// (noteWalIndexOpaque). The kind is taken before and finished after
	// (mainReadTxnAfter), since a statement may create the first TEMP
	// object.
	kind := db.mainReadTxnOf(sqlText)
	db.stmtDepth++
	defer func() {
		db.stmtDepth--
		if db.stmtDepth != 0 {
			return
		}
		// Applied AFTER the statement runs, success or failure alike -- see
		// deferFKAutocommitApply's own doc comment for why RUN outcome plays no
		// part in C SQLite's own rule.
		db.deferFKAutocommitApply(sqlText, topLevel)
		k := mainReadTxnUnknown
		if err == nil {
			k = db.mainReadTxnAfter(sqlText, kind)
		}
		if k == mainReadTxnUnknown {
			// Every unknown from THIS path is final -- noteWalIndexStatement's
			// own UNKNOWN is the read path's provisional one, and a write
			// statement's uncertainty is not something a later query resolves.
			db.noteWalIndexOpaque()
			return
		}
		db.noteWalIndexStatement(k)
	}()
	db.noteTransactionMayHaveDirtiedMain(sqlText)
	if ra, li, handled, rerr := db.execRoutedToAttached(sqlText, args); handled {
		// A write routed to an ATTACHed database's own session still shares
		// ITS connection's aVTrans list with this one's (C fts3's flush
		// walks every attached db, not just the one the statement named), so
		// it can taint this session's own accumulating fts3 segments exactly
		// like a local statement can -- see fts3TxnMaybeTaint.
		db.fts3TxnMaybeTaint(sqlText)
		return ra, li, rerr
	}
	ra, li, err := db.tryVDBEWrite(sqlText, args)
	if err == nil {
		db.noteTransactionDirtiedMain(sqlText, ra)
	}
	// Taints db.fts3TxnTaint (fts3_txn.go) when sqlText is not POSITIVELY known
	// to be a shape fts3_txn.go's accumulating-segment model accounts for --
	// checked regardless of err, since a statement that FAILS partway (a
	// constraint violation, say) can still have opened the statement journal
	// C fts3 flushes at, before the failure (OP_Transaction runs at the top
	// of the compiled program, ahead of the write that then aborts).
	db.fts3TxnMaybeTaint(sqlText)
	// db.pendingLoadErr's own doc comment (writer.go): a handful of call
	// sites (captureSnapshot's own callers in particular -- txn.go) have no
	// error return of their own to report an ensureTableLoaded failure
	// through, so it is recorded here instead. fkFinishStatement/
	// fkCommitCheck (fk.go) already check it whenever they run; this is the
	// backstop that catches it regardless of whether foreign-key enforcement
	// is even on, at the one checkpoint every top-level write passes through
	// either way.
	if err == nil && db.pendingLoadErr != nil {
		err = db.pendingLoadErr
		db.pendingLoadErr = nil
	}
	return ra, li, err
}

// ParseParamInfo parses sqlText far enough to recover its ParamInfo
// (parameter count and name map) without executing it, for a driver's
// Stmt.NumInput and named binding. Statements that take no parameters report
// ParamInfo{} and nil.
func ParseParamInfo(sqlText string) (ParamInfo, error) {
	info, err := parseParamInfo(sqlText)
	// The driver PREPARES through here, so this is where a truncated statement's
	// parse error first reaches a caller: "incomplete input" at end of input,
	// whatever the grammar was in the middle of (parse.y:44-51).
	return info, incompleteIfAtEOF(err)
}

func parseParamInfo(sqlText string) (ParamInfo, error) {
	trimmed := strings.TrimSpace(sqlText)
	// "EXPLAIN <stmt>" takes exactly the parameters <stmt> takes: it PREPARES
	// the same statement and describes the program instead of running it, so
	// C SQLite binds into it like any other ("EXPLAIN SELECT a FROM t WHERE
	// a=?" prepared with one argument answers its listing -- verified). Without
	// this the wrapped text reaches no parser at all, NumParams stays 0, and
	// database/sql refuses the call with "expected 0 arguments, got 1".
	if inner, mode := splitExplain(trimmed); mode != explainNone {
		return parseParamInfo(inner) // terminates: each strip consumes a token
	}
	toks, err := lex(trimmed)
	if err != nil {
		return ParamInfo{}, err
	}
	// A leading "WITH ..." clause (see LeadingStatementVerb's doc comment,
	// sql_parser.go) can introduce either a SELECT or an INSERT/UPDATE/
	// DELETE; toks[0]'s own literal text is always "WITH" either way, so
	// resolvedKW below unwraps it (falling back to toks[0]'s own text for
	// everything else) to tell the two apart. ParseSelect/parseInsertStmt/
	// parseUpdateStmt/parseDeleteStmt are each independently WITH-aware, so
	// whichever one this dispatches to re-parses (and correctly handles) the
	// same leading WITH clause on its own.
	var resolvedKW string
	if len(toks) > 0 && toks[0].kind == tkIdent {
		resolvedKW = strings.ToUpper(toks[0].text)
		if resolvedKW == "WITH" {
			if verb, ok := verbAfterLeadingWith(trimmed, toks); ok {
				resolvedKW = verb
			}
		}
	}
	switch resolvedKW {
	case "SELECT":
		stmt, err := ParseSelect(trimmed)
		if err != nil {
			return ParamInfo{}, err
		}
		return stmt.Params, nil
	case "INSERT", "REPLACE": // see Exec's identical "REPLACE INTO" case above
		stmt, err := parseInsertStmt(trimmed)
		if err != nil {
			return ParamInfo{}, err
		}
		return stmt.params, nil
	case "UPDATE":
		stmt, err := parseUpdateStmt(trimmed)
		if err != nil {
			return ParamInfo{}, err
		}
		return stmt.params, nil
	case "DELETE":
		stmt, err := parseDeleteStmt(trimmed)
		if err != nil {
			return ParamInfo{}, err
		}
		return stmt.params, nil
	}
	return ParamInfo{}, nil
}
