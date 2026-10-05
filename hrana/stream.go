// Package hrana serves a musql database over Hrana, the protocol libSQL and
// Turso clients speak, so those clients can use musql unchanged.
//
// The protocol is specified in libsql's docs/HRANA_3_SPEC.md. Messages are the
// generated types in gen/hrana (proto/hrana); this package adds the JSON
// encoding, the HTTP endpoints and the mapping of a stream onto a musql
// connection.
package hrana

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	musqldriver "github.com/samyfodil/musql/driver"
	pb "github.com/samyfodil/musql/gen/hrana"
)

// stream is one Hrana stream: one musql connection, plus the SQL texts stored
// with store_sql (per stream over HTTP, per connection over WebSocket).
type stream struct {
	conn *sql.Conn
	sqls *sqlStore
}

// sqlStore is the SQL texts stored with store_sql.
type sqlStore struct {
	mu sync.Mutex
	m  map[int32]string
}

func newSQLStore() *sqlStore { return &sqlStore{m: map[int32]string{}} }

func (s *sqlStore) get(id int32) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.m[id]
	return q, ok
}

func (s *sqlStore) put(id int32, q string) {
	s.mu.Lock()
	s.m[id] = q
	s.mu.Unlock()
}

func (s *sqlStore) del(id int32) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

func newStream(ctx context.Context, db *sql.DB, sqls *sqlStore) (*stream, error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.Raw(func(dc any) error {
		mc, ok := dc.(*musqldriver.Conn)
		if !ok {
			return fmt.Errorf("hrana: database is not a musql database")
		}
		mc.ReportDeclTypes(true)
		return nil
	}); err != nil {
		c.Close()
		return nil, err
	}
	return &stream{conn: c, sqls: sqls}, nil
}

func (s *stream) close() error { return s.conn.Close() }

// state reads the connection's change counters and autocommit state.
func (s *stream) state() (changes, total, lastID int64, autocommit bool) {
	s.conn.Raw(func(dc any) error {
		mc := dc.(*musqldriver.Conn)
		changes, total, lastID = mc.ChangeState()
		autocommit = mc.IsAutocommit()
		return nil
	})
	return
}

func (s *stream) isAutocommit() bool {
	_, _, _, ac := s.state()
	return ac
}

// sqlOf resolves a statement's text from sql or a stored sql_id.
func (s *stream) sqlOf(sqlText *string, sqlID *int32) (string, error) {
	switch {
	case sqlText != nil && sqlID != nil:
		return "", protoErr("both sql and sql_id were given")
	case sqlText != nil:
		return *sqlText, nil
	case sqlID != nil:
		q, ok := s.sqls.get(*sqlID)
		if !ok {
			return "", protoErr(fmt.Sprintf("no SQL text stored as sql_id %d", *sqlID))
		}
		return q, nil
	}
	return "", protoErr("neither sql nor sql_id was given")
}

// execute runs one statement; rows are kept only when want_rows is not false.
func (s *stream) execute(ctx context.Context, st *pb.Stmt) (*pb.StmtResult, error) {
	q, err := s.sqlOf(st.Sql, st.SqlId)
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(st.Args)+len(st.NamedArgs))
	for _, v := range st.Args {
		args = append(args, fromValue(v))
	}
	for _, na := range st.NamedArgs {
		args = append(args, sql.Named(strings.TrimLeft(na.Name, ":@$"), fromValue(na.Value)))
	}
	wantRows := st.WantRows == nil || *st.WantRows

	_, total0, _, _ := s.state()
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := &pb.StmtResult{}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	for _, ct := range types {
		col := &pb.Col{Name: ptr(ct.Name())}
		if d := ct.DatabaseTypeName(); d != "" {
			col.Decltype = ptr(d)
		}
		res.Cols = append(res.Cols, col)
	}
	vals := make([]any, len(types))
	dest := make([]any, len(types))
	for i := range vals {
		dest[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if !wantRows {
			continue
		}
		row := &pb.Row{Values: make([]*pb.Value, len(vals))}
		for i, v := range vals {
			row.Values[i] = toValue(v)
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	changes, total, lastID, _ := s.state()
	if total != total0 {
		res.AffectedRowCount = uint64(changes)
	}
	res.LastInsertRowid = &lastID
	return res, nil
}

// batch runs a batch's steps in order, each only when its condition holds.
func (s *stream) batch(ctx context.Context, b *pb.Batch) *pb.BatchResult {
	res := &pb.BatchResult{StepResults: map[uint32]*pb.StmtResult{}, StepErrors: map[uint32]*pb.Error{}}
	for i, step := range b.GetSteps() {
		if step.Condition != nil && !s.cond(step.Condition, res) {
			continue
		}
		r, err := s.execute(ctx, step.GetStmt())
		if err != nil {
			res.StepErrors[uint32(i)] = toError(err)
			continue
		}
		res.StepResults[uint32(i)] = r
	}
	return res
}

// cursorEntries encodes a batch's result as the entries a cursor streams:
// step_begin, rows and step_end per executed step, step_error for a failed one,
// nothing for a skipped one.
func cursorEntries(b *pb.Batch, res *pb.BatchResult) []*pb.CursorEntry {
	var out []*pb.CursorEntry
	for i := range b.GetSteps() {
		k := uint32(i)
		if e, ok := res.StepErrors[k]; ok {
			out = append(out, &pb.CursorEntry{Entry: &pb.CursorEntry_StepError{StepError: &pb.StepErrorEntry{Step: k, Error: e}}})
			continue
		}
		sr, ok := res.StepResults[k]
		if !ok {
			continue
		}
		out = append(out, &pb.CursorEntry{Entry: &pb.CursorEntry_StepBegin{StepBegin: &pb.StepBeginEntry{Step: k, Cols: sr.Cols}}})
		for _, row := range sr.Rows {
			out = append(out, &pb.CursorEntry{Entry: &pb.CursorEntry_Row{Row: row}})
		}
		out = append(out, &pb.CursorEntry{Entry: &pb.CursorEntry_StepEnd{StepEnd: &pb.StepEndEntry{
			AffectedRowCount: sr.AffectedRowCount, LastInsertRowid: sr.LastInsertRowid}}})
	}
	return out
}

func (s *stream) cond(c *pb.BatchCond, res *pb.BatchResult) bool {
	switch c := c.Cond.(type) {
	case *pb.BatchCond_StepOk:
		_, ok := res.StepResults[c.StepOk]
		return ok
	case *pb.BatchCond_StepError:
		_, ok := res.StepErrors[c.StepError]
		return ok
	case *pb.BatchCond_Not:
		return !s.cond(c.Not, res)
	case *pb.BatchCond_And:
		for _, sub := range c.And.GetConds() {
			if !s.cond(sub, res) {
				return false
			}
		}
		return true
	case *pb.BatchCond_Or:
		for _, sub := range c.Or.GetConds() {
			if s.cond(sub, res) {
				return true
			}
		}
		return false
	case *pb.BatchCond_IsAutocommit_:
		return s.isAutocommit()
	}
	return false
}

// sequence runs a ";"-separated script, discarding results.
func (s *stream) sequence(ctx context.Context, sqlText *string, sqlID *int32) error {
	q, err := s.sqlOf(sqlText, sqlID)
	if err != nil {
		return err
	}
	_, err = s.conn.ExecContext(ctx, q)
	return err
}

// describe reports a statement's parameters, and its columns when it only
// reads (found by running it with every parameter NULL, which a read-only
// statement does without side effects).
func (s *stream) describe(ctx context.Context, sqlText *string, sqlID *int32) (*pb.DescribeResult, error) {
	q, err := s.sqlOf(sqlText, sqlID)
	if err != nil {
		return nil, err
	}
	res := &pb.DescribeResult{
		IsExplain:  hasPrefixFold(q, "EXPLAIN"),
		IsReadonly: isReadonly(q),
	}
	params, err := paramNames(q)
	if err != nil {
		return nil, err
	}
	for _, n := range params {
		p := &pb.DescribeParam{}
		if n != "" {
			p.Name = ptr(n)
		}
		res.Params = append(res.Params, p)
	}
	if res.IsReadonly {
		args := make([]any, len(params))
		rows, err := s.conn.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		types, err := rows.ColumnTypes()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, ct := range types {
			col := &pb.DescribeCol{Name: ct.Name()}
			if d := ct.DatabaseTypeName(); d != "" {
				col.Decltype = ptr(d)
			}
			res.Cols = append(res.Cols, col)
		}
	}
	return res, nil
}

func hasPrefixFold(s, prefix string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func isReadonly(q string) bool {
	for _, p := range []string{"SELECT", "VALUES", "EXPLAIN"} {
		if hasPrefixFold(q, p) {
			return true
		}
	}
	return false
}

// protocolError is a malformed request, reported to the client with code
// HRANA_PROTO_ERROR.
type protocolError struct{ msg string }

func (e protocolError) Error() string { return e.msg }

func protoErr(msg string) error { return protocolError{msg} }

// toError maps an error onto Hrana's Error, with a SQLite result-code name as
// its code, which is what libSQL clients classify errors by.
func toError(err error) *pb.Error {
	var pe protocolError
	if errors.As(err, &pe) {
		return &pb.Error{Message: pe.msg, Code: ptr("HRANA_PROTO_ERROR")}
	}
	msg := err.Error()
	return &pb.Error{Message: msg, Code: ptr(errorCode(msg))}
}

func errorCode(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "constraint failed"):
		switch {
		case strings.Contains(m, "unique"):
			return "SQLITE_CONSTRAINT_UNIQUE"
		case strings.Contains(m, "primary key"):
			return "SQLITE_CONSTRAINT_PRIMARYKEY"
		case strings.Contains(m, "not null"):
			return "SQLITE_CONSTRAINT_NOTNULL"
		case strings.Contains(m, "foreign key"):
			return "SQLITE_CONSTRAINT_FOREIGNKEY"
		case strings.Contains(m, "check"):
			return "SQLITE_CONSTRAINT_CHECK"
		}
		return "SQLITE_CONSTRAINT"
	case strings.Contains(m, "database is locked"), strings.Contains(m, "sqlite_busy"):
		return "SQLITE_BUSY"
	case strings.Contains(m, "disk is full"):
		return "SQLITE_FULL"
	case strings.Contains(m, "readonly"), strings.Contains(m, "read-only"):
		return "SQLITE_READONLY"
	}
	return "SQLITE_ERROR"
}

func ptr[T any](v T) *T { return &v }

// fromValue converts a Hrana value to a driver argument.
func fromValue(v *pb.Value) any {
	switch x := v.GetValue().(type) {
	case *pb.Value_Integer:
		return x.Integer
	case *pb.Value_Float:
		return x.Float
	case *pb.Value_Text:
		return x.Text
	case *pb.Value_Blob:
		return x.Blob
	}
	return nil
}

// toValue converts a scanned column value to a Hrana value.
func toValue(v any) *pb.Value {
	switch x := v.(type) {
	case nil:
		return &pb.Value{Value: &pb.Value_Null_{Null: &pb.Value_Null{}}}
	case int64:
		return &pb.Value{Value: &pb.Value_Integer{Integer: x}}
	case float64:
		return &pb.Value{Value: &pb.Value_Float{Float: x}}
	case string:
		return &pb.Value{Value: &pb.Value_Text{Text: x}}
	case []byte:
		return &pb.Value{Value: &pb.Value_Blob{Blob: x}}
	case bool:
		if x {
			return &pb.Value{Value: &pb.Value_Integer{Integer: 1}}
		}
		return &pb.Value{Value: &pb.Value_Integer{Integer: 0}}
	case time.Time:
		return &pb.Value{Value: &pb.Value_Text{Text: x.Format("2006-01-02 15:04:05.999999999-07:00")}}
	}
	return &pb.Value{Value: &pb.Value_Text{Text: fmt.Sprint(v)}}
}
