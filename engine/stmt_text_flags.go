package engine

import (
	"strings"
	"sync"
	"sync/atomic"
)

// What a statement's text says, computed once per distinct text.
//
// StatementHasReturning, StatementCallsFts3Optimize, and LeadingStatementVerb
// are pure functions of the SQL string. The answers are memoised by text: the
// same string always gives the same answer, and there is no schema, connection
// state, or parameter dependency, so no invalidation is needed.
type stmtTextFlags struct {
	hasReturning     bool
	callsFts3Optimze bool
	verb             string
	verbOK           bool
	// isCreateVirtualTable is the same question asked of a CATALOG row's stored
	// CREATE text rather than of a statement. It lands in the same memo for the
	// same reason -- it lexes, the text never changes, and it was asked once per
	// catalog row per PAGER BUILD, which a held connection does per statement.
	// 32% of all the lexing a prepared point lookup did.
	isCreateVirtualTable bool
	// opensTempDatabase is r35aStatementOpensTempDatabase, which runs on the
	// statement path for EVERY statement of every session until the temp database
	// is opened -- and starts with two case-folded substring scans of the whole
	// text before it even lexes. It measured at 4% of a bulk UPDATE's CPU, all of
	// it on the seed's 100,000 prepared INSERTs re-answering for one string.
	opensTempDatabase bool
	// isExplain is IsExplainStatement, asked twice per execution by the driver.
	isExplain bool
}

// stmtTextEntry is one memoised text, and what stmtTextLast points at.
type stmtTextEntry struct {
	sql string
	f   stmtTextFlags
}

const stmtTextFlagsMax = 512

var (
	stmtTextMu    sync.RWMutex
	stmtTextCache = map[string]*stmtTextEntry{}
	// stmtTextLast is the text asked about last, checked before the map with no
	// lock: a prepared statement asks about the same string several times per
	// execution, and the RWMutex plus a hash of the whole text was 3% of a
	// prepared batch INSERT. The same string compares equal on its pointer.
	stmtTextLast atomic.Pointer[stmtTextEntry]
)

func stmtTextFlagsFor(sql string) stmtTextFlags {
	if e := stmtTextLast.Load(); e != nil && e.sql == sql {
		return e.f
	}
	stmtTextMu.RLock()
	e, ok := stmtTextCache[sql]
	stmtTextMu.RUnlock()
	if ok {
		stmtTextLast.Store(e)
		return e.f
	}
	f := stmtTextFlags{
		hasReturning:     statementHasReturningUncached(sql),
		callsFts3Optimze: statementCallsFts3OptimizeUncached(sql),
	}
	f.verb, f.verbOK = leadingStatementVerbUncached(sql)
	f.isCreateVirtualTable = isCreateVirtualTableSQLUncached(sql)
	f.opensTempDatabase = r35aStatementOpensTempDatabaseUncached(sql)
	_, mode := splitExplain(strings.TrimSpace(sql))
	f.isExplain = mode != explainNone
	e = &stmtTextEntry{sql: sql, f: f}
	stmtTextMu.Lock()
	if len(stmtTextCache) < stmtTextFlagsMax {
		stmtTextCache[sql] = e
	}
	stmtTextMu.Unlock()
	stmtTextLast.Store(e)
	return f
}

// StatementHasReturning reports whether sqlText carries a RETURNING clause.
func StatementHasReturning(sqlText string) bool { return stmtTextFlagsFor(sqlText).hasReturning }

// StatementCallsFts3Optimize reports whether sqlText invokes fts3's optimize().
func StatementCallsFts3Optimize(sqlText string) bool {
	return stmtTextFlagsFor(sqlText).callsFts3Optimze
}

// LeadingStatementVerb is sqlText's first keyword.
func LeadingStatementVerb(sqlText string) (string, bool) {
	f := stmtTextFlagsFor(sqlText)
	return f.verb, f.verbOK
}

// isCreateVirtualTableSQL reports whether sqlText is a CREATE VIRTUAL TABLE.
func isCreateVirtualTableSQL(sqlText string) bool {
	return stmtTextFlagsFor(sqlText).isCreateVirtualTable
}
