package state

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	hpb "github.com/samyfodil/musql/hrana/gen/hrana"
	hhttp "github.com/samyfodil/musql/hrana/gen/hrana/http"
)

// Master is how a follower's Store reaches the state on the master: the
// master's Hrana server, over Client. A Store with one runs every call there
// unless Here reports that this node is the master.
type Master struct {
	// Here reports whether this node is the master (or about to be): its own
	// state is the one to use.
	Here func() bool
	// URL is the master's Hrana pipeline endpoint ("…/v3-protobuf/pipeline").
	URL    func() (string, error)
	Client *http.Client
}

// remoteDB is a *sql.DB over the master's state. A connection holds a Hrana
// stream only while a transaction is open; a statement outside one is a
// request of its own, so the master may change between two of them. One that
// cannot reach the master (it stopped, and the election has not noticed yet)
// runs on local, this node's copy: a read is answered, and a write fails as
// not the writer, which the Store waits out.
func remoteDB(m *Master, local *sql.DB) *sql.DB {
	db := sql.OpenDB(remoteConnector{m, local})
	db.SetConnMaxIdleTime(time.Minute)
	return db
}

type remoteConnector struct {
	m     *Master
	local *sql.DB
}

func (c remoteConnector) Connect(context.Context) (driver.Conn, error) {
	return &remoteConn{m: c.m, local: c.local}, nil
}

func (c remoteConnector) Driver() driver.Driver { return remoteDriver{} }

type remoteDriver struct{}

func (remoteDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("state: the remote driver is opened through its connector")
}

type remoteConn struct {
	m     *Master
	local *sql.DB
	// url and baton are the open transaction's stream.
	url   string
	baton *string
}

// errUnreachable is a request that did not reach a master that could answer.
var errUnreachable = errors.New("state: the master is unreachable")

// errRemote is a statement the master refused: its message is the master's.
type errRemote struct{ msg string }

func (e *errRemote) Error() string { return e.msg }

// run sends stmt on the connection's stream, or on a stream of its own that
// the request closes when no transaction is open. keep leaves the stream open
// (BEGIN); end closes it (COMMIT, ROLLBACK).
func (c *remoteConn) run(ctx context.Context, stmt *hpb.Stmt, keep, end bool) (*hpb.StmtResult, error) {
	url := c.url
	if c.baton == nil {
		u, err := c.m.URL()
		if err != nil {
			return nil, err
		}
		url = u
	}
	reqs := []*hhttp.StreamRequest{{Request: &hhttp.StreamRequest_Execute{Execute: &hhttp.ExecuteStreamReq{Stmt: stmt}}}}
	if !keep {
		reqs = append(reqs, &hhttp.StreamRequest{Request: &hhttp.StreamRequest_Close{Close: &hhttp.CloseStreamReq{}}})
	}
	body, err := (&hhttp.PipelineReqBody{Baton: c.baton, Requests: reqs}).MarshalVT()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := c.m.Client.Do(req)
	if err != nil {
		c.baton = nil
		return nil, fmt.Errorf("%w: %v", errUnreachable, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.baton = nil
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.baton = nil
		err := fmt.Errorf("state: the master answered %s: %s", resp.Status, bytes.TrimSpace(b))
		if resp.StatusCode >= 500 {
			err = fmt.Errorf("%w: %v", errUnreachable, err)
		}
		return nil, err
	}
	var out hhttp.PipelineRespBody
	if err := out.UnmarshalVT(b); err != nil {
		c.baton = nil
		return nil, err
	}
	switch {
	case keep:
		c.url, c.baton = url, out.Baton
	case end:
		c.url, c.baton = "", nil
	}
	if len(out.Results) == 0 {
		return nil, errors.New("state: the master sent no result")
	}
	switch r := out.Results[0].Result.(type) {
	case *hhttp.StreamResult_Error:
		return nil, &errRemote{r.Error.GetMessage()}
	case *hhttp.StreamResult_Ok:
		if e, ok := r.Ok.Response.(*hhttp.StreamResponse_Execute); ok {
			return e.Execute.GetResult(), nil
		}
	}
	return nil, errors.New("state: the master sent an unexpected result")
}

func stmtOf(query string, args []driver.NamedValue, rows bool) (*hpb.Stmt, error) {
	st := &hpb.Stmt{Sql: &query, WantRows: &rows}
	for _, a := range args {
		v, err := toValue(a.Value)
		if err != nil {
			return nil, err
		}
		if a.Name != "" {
			st.NamedArgs = append(st.NamedArgs, &hpb.NamedArg{Name: a.Name, Value: v})
			continue
		}
		st.Args = append(st.Args, v)
	}
	return st, nil
}

func toValue(v driver.Value) (*hpb.Value, error) {
	switch v := v.(type) {
	case nil:
		return &hpb.Value{Value: &hpb.Value_Null_{Null: &hpb.Value_Null{}}}, nil
	case int64:
		return &hpb.Value{Value: &hpb.Value_Integer{Integer: v}}, nil
	case float64:
		return &hpb.Value{Value: &hpb.Value_Float{Float: v}}, nil
	case bool:
		var n int64
		if v {
			n = 1
		}
		return &hpb.Value{Value: &hpb.Value_Integer{Integer: n}}, nil
	case string:
		return &hpb.Value{Value: &hpb.Value_Text{Text: v}}, nil
	case []byte:
		return &hpb.Value{Value: &hpb.Value_Blob{Blob: v}}, nil
	case time.Time:
		return &hpb.Value{Value: &hpb.Value_Text{Text: v.Format(time.RFC3339Nano)}}, nil
	}
	return nil, fmt.Errorf("state: cannot send a %T", v)
}

func fromValue(v *hpb.Value) driver.Value {
	switch v := v.GetValue().(type) {
	case *hpb.Value_Integer:
		return v.Integer
	case *hpb.Value_Float:
		return v.Float
	case *hpb.Value_Text:
		return v.Text
	case *hpb.Value_Blob:
		return v.Blob
	}
	return nil
}

func (c *remoteConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	st, err := stmtOf(query, args, false)
	if err != nil {
		return nil, err
	}
	inTx := c.baton != nil
	r, err := c.run(ctx, st, inTx, false)
	if errors.Is(err, errUnreachable) && !inTx {
		return c.local.ExecContext(ctx, query, values(args)...)
	}
	if err != nil {
		return nil, err
	}
	return remoteResult{r}, nil
}

func (c *remoteConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	st, err := stmtOf(query, args, true)
	if err != nil {
		return nil, err
	}
	inTx := c.baton != nil
	r, err := c.run(ctx, st, inTx, false)
	if errors.Is(err, errUnreachable) && !inTx {
		return localRows(ctx, c.local, query, values(args))
	}
	if err != nil {
		return nil, err
	}
	return &remoteRows{r: r}, nil
}

func values(args []driver.NamedValue) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Value
		if a.Name != "" {
			out[i] = sql.Named(a.Name, a.Value)
		}
	}
	return out
}

// localRows runs query on this node's copy, its rows read whole, as a
// request's are.
func localRows(ctx context.Context, db *sql.DB, query string, args []any) (driver.Rows, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := &memRows{cols: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]driver.Value, len(vals))
		for i, v := range vals {
			row[i] = v
		}
		out.rows = append(out.rows, row)
	}
	return out, rows.Err()
}

type memRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *memRows) Columns() []string { return r.cols }
func (r *memRows) Close() error      { return nil }

func (r *memRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func (c *remoteConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	q := "BEGIN"
	st := &hpb.Stmt{Sql: &q}
	if _, err := c.run(ctx, st, true, false); err != nil {
		return nil, err
	}
	return remoteTx{c}, nil
}

func (c *remoteConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *remoteConn) Prepare(query string) (driver.Stmt, error) {
	return remoteStmt{c, query}, nil
}

// Close ends a transaction a caller abandoned: the master rolls back a stream
// it is not sent again.
func (c *remoteConn) Close() error {
	if c.baton == nil {
		return nil
	}
	q := "ROLLBACK"
	_, err := c.run(context.Background(), &hpb.Stmt{Sql: &q}, false, true)
	return err
}

// ResetSession refuses a connection whose stream was lost, so database/sql
// opens another.
func (c *remoteConn) ResetSession(context.Context) error {
	if c.baton != nil {
		return driver.ErrBadConn
	}
	return nil
}

type remoteTx struct{ c *remoteConn }

func (t remoteTx) end(q string) error {
	_, err := t.c.run(context.Background(), &hpb.Stmt{Sql: &q}, false, true)
	return err
}

func (t remoteTx) Commit() error   { return t.end("COMMIT") }
func (t remoteTx) Rollback() error { return t.end("ROLLBACK") }

type remoteStmt struct {
	c     *remoteConn
	query string
}

func (s remoteStmt) Close() error  { return nil }
func (s remoteStmt) NumInput() int { return -1 }

func (s remoteStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.query, named(args))
}

func (s remoteStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.query, named(args))
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

type remoteResult struct{ r *hpb.StmtResult }

func (r remoteResult) LastInsertId() (int64, error) { return r.r.GetLastInsertRowid(), nil }
func (r remoteResult) RowsAffected() (int64, error) { return int64(r.r.GetAffectedRowCount()), nil }

type remoteRows struct {
	r *hpb.StmtResult
	i int
}

func (r *remoteRows) Columns() []string {
	out := make([]string, len(r.r.GetCols()))
	for i, c := range r.r.GetCols() {
		out[i] = c.GetName()
	}
	return out
}

func (r *remoteRows) Close() error { return nil }

func (r *remoteRows) Next(dest []driver.Value) error {
	if r.i >= len(r.r.GetRows()) {
		return io.EOF
	}
	for j, v := range r.r.GetRows()[r.i].GetValues() {
		dest[j] = fromValue(v)
	}
	r.i++
	return nil
}
