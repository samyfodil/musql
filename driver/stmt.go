package driver

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/samyfodil/musql/engine"
)

// Stmt is a prepared statement. It caches the bound-parameter shape and
// routing decision, so repeated Exec/Query calls avoid re-parsing.
type Stmt struct {
	conn    *Conn
	sqlText string
	info    engine.ParamInfo
	isQuery bool
	// script is the individual statements of a ";"-separated SCRIPT, non-nil
	// only for multi-statement text. Such a Stmt is Exec-only.
	script []string
}

var (
	_ driver.Stmt             = (*Stmt)(nil)
	_ driver.StmtExecContext  = (*Stmt)(nil)
	_ driver.StmtQueryContext = (*Stmt)(nil)
)

// Close implements driver.Stmt. This is a no-op since there are no per-statement engine resources.
func (s *Stmt) Close() error { return nil }

// NumInput implements driver.Stmt: the statement's bound-parameter count.
func (s *Stmt) NumInput() int { return s.info.NumParams }

// execScript runs a ";"-separated script statement by statement.
// It stops at the first error, leaving prior statements applied.
// The result is the last statement's, not an aggregate.
func (s *Stmt) execScript(ctx context.Context) (driver.Result, error) {
	var last driver.Result = &execResult{lastInsertID: s.conn.lastInsertRowid}
	for _, one := range s.script {
		sub := &Stmt{conn: s.conn, sqlText: one, isQuery: isSelectStmt(one)}
		res, err := sub.ExecContext(ctx, nil)
		if err != nil {
			return nil, err
		}
		last = res
	}
	return last, nil
}

// Exec implements driver.Stmt (the legacy, positional-only path; database/
// sql prefers ExecContext, implemented below, when available).
func (s *Stmt) execImpl(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

// Query implements driver.Stmt (the legacy, positional-only path;
// database/sql prefers QueryContext, implemented below, when available).
func (s *Stmt) queryImpl(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

// ExecContext implements driver.StmtExecContext.
// A QUERY Exec'd runs and discards rows; RowsAffected is 0.
// A RETURNING statement's RowsAffected is the preceding statement's change count.
func (s *Stmt) execContextImpl(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if s.script != nil {
		return s.execScript(ctx)
	}
	eargs, err := bindArgs(s.info, args)
	if err != nil {
		return nil, err
	}
	// Check for RETURNING clause via plainDML (cached) and StatementHasReturning.
	if s.isQuery || (!s.conn.plainDML(s.sqlText) && engine.StatementHasReturning(s.sqlText)) {
		// sqlite3_changes() AS OF NOW -- before this statement runs. For a
		// RETURNING statement that is exactly what the oracle reports as
		// RowsAffected; see below.
		prevChanges := s.conn.changes
		rows, qerr := s.conn.queryArgs(s.sqlText, eargs)
		if qerr != nil {
			return nil, qerr
		}
		n := 0
		if r, ok := rows.(*Rows); ok {
			n = len(r.rows)
		}
		rows.Close()
		ra := int64(0)
		if !s.isQuery && n > 0 {
			ra = prevChanges
		}
		return &execResult{rowsAffected: ra, lastInsertID: s.conn.lastInsertRowid}, nil
	}
	if _, err := s.conn.execArgs(s.sqlText, eargs); err != nil {
		return nil, err
	}
	// RowsAffected is the connection's current change count from sqlite3_changes(),
	// which reflects the last statement that actually modified rows.
	return &execResult{rowsAffected: s.conn.changes, lastInsertID: s.conn.lastInsertRowid}, nil
}

// QueryContext implements driver.StmtQueryContext.
func (s *Stmt) queryContextImpl(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.script != nil {
		// A script must be Exec'd; Query can only return one result set.
		return nil, fmt.Errorf("driver: a multi-statement script must be run with Exec, not Query: %s", s.sqlText)
	}
	eargs, err := bindArgs(s.info, args)
	if err != nil {
		return nil, err
	}
	return s.conn.queryArgs(s.sqlText, eargs)
}

// valuesToNamed converts positional driver.Value args to []driver.NamedValue.
func valuesToNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// bindArgs converts a database/sql call's bound arguments to []engine.Value.
// Positional args bind by Ordinal; named args resolve via info.ParamNames,
// trying all three of SQLite's sigils (":name", "@name", "$name").
func bindArgs(info engine.ParamInfo, args []driver.NamedValue) ([]engine.Value, error) {
	out := make([]engine.Value, info.NumParams)
	for _, nv := range args {
		v, err := driverValueToEngine(nv.Value)
		if err != nil {
			return nil, err
		}
		idx := nv.Ordinal
		if nv.Name != "" {
			resolved := false
			for _, sigil := range [...]string{":", "@", "$"} {
				if pos, ok := info.ParamNames[sigil+nv.Name]; ok {
					idx = pos
					resolved = true
					break
				}
			}
			if !resolved {
				return nil, fmt.Errorf("driver: no such named parameter %q in statement", nv.Name)
			}
		}
		if idx < 1 || idx > len(out) {
			return nil, fmt.Errorf("driver: bind parameter index %d out of range [1,%d]", idx, len(out))
		}
		out[idx-1] = v
	}
	return out, nil
}

// isSelectStmt is this driver's version of the leading-keyword statement-
// kind sniff engine.Exec/ExecArgs/ParseParamInfo already use internally
// (see engine/insert_write.go's Exec: toks[0].kind == tkIdent, switched on
// its upper-cased text) to route CREATE/INSERT/UPDATE/DELETE to the write
// path and everything else (in this engine, only SELECT) to the read path.
// It can't call into that logic directly -- the engine's lexer (lex) is
// unexported, and driver is a separate package that only sees engine's
// exported surface -- so it does the same leading-identifier check by hand:
// skip leading whitespace and (like SQL) '--'/'/* */' comments, then compare
// the first run of identifier-ish bytes case-insensitively to "SELECT".
// Anything else (including a statement this engine's write path doesn't
// implement at all, e.g. CREATE VIEW) is routed to Conn.execArgs, whose
// underlying engine.Exec/ExecArgs already reports a clear "unsupported
// statement" error for those -- this sniff only needs to answer "is this a
// SELECT", not fully validate the statement.
func isSelectStmt(sqlText string) bool {
	s := skipLeadingCommentsAndSpace(sqlText)
	i := 0
	for i < len(s) && isIdentByte(s[i]) {
		i++
	}
	kw := s[:i]
	if strings.EqualFold(kw, "EXPLAIN") {
		// EXPLAIN describes a statement instead of running it, so it is a QUERY
		// whatever it wraps -- "EXPLAIN INSERT ..." returns the program's rows
		// and inserts nothing. See engine/explain.go, which owns both forms.
		return true
	}
	if strings.EqualFold(kw, "WITH") {
		// A WITH clause can introduce either a read (SELECT) or a write
		// (INSERT/UPDATE/DELETE) -- the fixed first-token sniff this function
		// otherwise uses can't tell those apart (a CTE's own body is an
		// arbitrary, possibly deeply nested SELECT, so there's no fixed
		// lookahead distance to the keyword that actually matters). Defer to
		// engine.LeadingStatementVerb, which parses far enough to know. Falls
		// back to this function's ordinary (WITH-is-a-query) answer if the
		// statement doesn't even lex -- Conn.execArgs/queryArgs will surface
		// the real lex error either way.
		// A verb of "WITH" means that function could not see PAST the clause --
		// the statement does not parse far enough to have an inner verb -- and
		// that is the same "fall back to WITH-is-a-query" case as a lex failure,
		// not an answer of "not a SELECT". Routing it to Exec instead buried the
		// real parse error under the write path's "unsupported statement"
		// catch-all: a bare "WITH" reported that list of supported verbs where
		// 3.53.3 says "incomplete input", and so did
		// "WITH r AS (SELECT 1), r AS (SELECT 2) SELECT * FROM r", whose real
		// error is "duplicate WITH table name: r".
		if verb, ok := engine.LeadingStatementVerb(sqlText); ok && !strings.EqualFold(verb, "WITH") {
			return strings.EqualFold(verb, "SELECT")
		}
	}
	// "VALUES (...), (...)" is SQLite's row-constructor-as-SELECT shorthand: a
	// select-CORE alternative to SELECT, not a SELECT variant, and it produces a
	// real result set (columns named column1, column2, ...). The ENGINE already
	// parses it -- parseSelectCore checks for a leading VALUES before requiring
	// the SELECT keyword, which is why "WITH c AS (VALUES(1)) SELECT * FROM c"
	// has always worked -- so only this first-token sniff kept a TOP-LEVEL one
	// out of the query path, routing it to the write path where it died as
	// "unsupported statement". The mined SQLite corpus uses the top-level form
	// 207 times.
	return strings.EqualFold(kw, "SELECT") || strings.EqualFold(kw, "VALUES")
}

// isPragmaStmt reports whether sqlText is a PRAGMA statement. A PRAGMA can
// produce rows (a getter) or not (a setter), so -- unlike other non-SELECT
// statements -- it must keep going through the engine's own query path when
// run with Query (see Conn.queryArgs).
func isPragmaStmt(sqlText string) bool {
	s := skipLeadingCommentsAndSpace(sqlText)
	i := 0
	for i < len(s) && isIdentByte(s[i]) {
		i++
	}
	return strings.EqualFold(s[:i], "PRAGMA")
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// skipLeadingCommentsAndSpace strips leading whitespace and SQL "--..." /
// "/*...*/" comments from s, repeatedly, until neither remains at the
// front -- so isSelectStmt sees the statement's true first keyword even
// when it's preceded by a comment.
func skipLeadingCommentsAndSpace(s string) string {
	for {
		t := strings.TrimLeft(s, " \t\r\n")
		if strings.HasPrefix(t, "--") {
			if i := strings.IndexByte(t, '\n'); i >= 0 {
				t = t[i+1:]
			} else {
				t = ""
			}
		} else if strings.HasPrefix(t, "/*") {
			if i := strings.Index(t, "*/"); i >= 0 {
				t = t[i+2:]
			} else {
				t = ""
			}
		}
		if t == s {
			return t
		}
		s = t
	}
}

// Exec normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in execImpl.
func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	v, err := s.execImpl(args)
	return v, parityErr(err)
}

// Query normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in queryImpl.
func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	v, err := s.queryImpl(args)
	return v, parityErr(err)
}

// ExecContext normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in execContextImpl.
func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	v, err := s.execContextImpl(ctx, args)
	return v, parityErr(err)
}

// QueryContext normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in queryContextImpl.
func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	v, err := s.queryContextImpl(ctx, args)
	return v, parityErr(err)
}
