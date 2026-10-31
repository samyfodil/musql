package hrana

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/samyfodil/musql/hrana/gen/hrana"
	pbhttp "github.com/samyfodil/musql/hrana/gen/hrana/http"
)

// Server serves Hrana over HTTP (versions 2 and 3, JSON and Protobuf) for one
// musql database.
type Server struct {
	db *sql.DB
	config

	mu      sync.Mutex
	streams map[string]*httpStream // by current baton
	closed  bool

	// WebSocket connections run on contexts derived from base; Close cancels
	// it and waits on ws, so no stream holds the database after Close returns.
	base   context.Context
	cancel context.CancelFunc
	ws     sync.WaitGroup
}

type httpStream struct {
	*stream
	ro       bool       // opened on the read-only database
	mu       sync.Mutex // a stream runs one pipeline at a time
	lastUsed time.Time
}

// config is what the options set. A Server and a Multi each hold one, so a
// Multi knows the token before any database's Server exists.
type config struct {
	authToken string
	auth      func(token string) (readOnly bool, err error)
	roDB      *sql.DB
	idle      time.Duration
	observe   func(Stmt)
}

func newConfig(opts []Option) config {
	c := config{idle: 10 * time.Second}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// Option configures a Server or a Multi.
type Option func(*config)

// WithAuthToken requires "Authorization: Bearer <token>" on every request.
// Without it the server accepts any client.
func WithAuthToken(token string) Option { return func(c *config) { c.authToken = token } }

// WithAuth checks every token a client presents: the bearer token of an HTTP
// request, and for a WebSocket the bearer token of its upgrade request when
// it has one, else the token in its hello message. A non-nil error refuses
// the client. A read-only client runs on the database given by
// WithReadOnlyDB, and is refused when there is none.
func WithAuth(check func(token string) (readOnly bool, err error)) Option {
	return func(c *config) { c.auth = check }
}

// WithReadOnlyDB sets the database that clients WithAuth marks read-only run
// on. Confining it is the caller's job.
func WithReadOnlyDB(db *sql.DB) Option { return func(c *config) { c.roDB = db } }

// WithIdleTimeout closes a stream after d without a request (default 10s): an
// HTTP client that stops without closing its stream leaves no other signal.
func WithIdleTimeout(d time.Duration) Option { return func(c *config) { c.idle = d } }

// bearer is the token of an "Authorization: Bearer <token>" header.
func bearer(r *http.Request) string {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok
}

// authorize checks a token against WithAuth; the status is 0 when it passes.
func (c *config) authorize(token string) (readOnly bool, status int) {
	if c.auth == nil {
		return false, 0
	}
	ro, err := c.auth(token)
	switch {
	case err != nil:
		return false, http.StatusUnauthorized
	case ro && c.roDB == nil:
		return false, http.StatusForbidden
	}
	return ro, 0
}

// dbFor is the database a client runs on.
func (s *Server) dbFor(readOnly bool) *sql.DB {
	if readOnly {
		return s.roDB
	}
	return s.db
}

// Stmt is one statement a client ran, as WithObserver reports it.
type Stmt struct {
	SQL      string
	Duration time.Duration
	// RowsRead is the rows the statement returned, RowsWritten the rows it
	// changed (total_changes moved by).
	RowsRead, RowsWritten int64
	Err                   error
}

// WithObserver calls fn after every statement a client runs (an execute, a
// batch step, a sequence), on the goroutine that ran it, so fn must be quick.
func WithObserver(fn func(Stmt)) Option { return func(c *config) { c.observe = fn } }

// NewServer serves db, which must be opened with the musql driver.
func NewServer(db *sql.DB, opts ...Option) *Server {
	return newServer(db, newConfig(opts))
}

func newServer(db *sql.DB, cfg config) *Server {
	s := &Server{db: db, config: cfg, streams: map[string]*httpStream{}}
	s.base, s.cancel = context.WithCancel(context.Background())
	return s
}

// Close closes every open stream, WebSocket connections included, and waits
// until none is still using the database.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	for b, st := range s.streams {
		st.close()
		delete(s.streams, b)
	}
	s.mu.Unlock()
	s.cancel()
	s.ws.Wait()
	return nil
}

// track registers work that Close must wait for; false once Close has begun.
func (s *Server) track() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.ws.Add(1)
	return true
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A WebSocket client authenticates in its hello message, not a header.
	if isWebSocket(r) {
		s.serveWS(w, r)
		return
	}
	if s.authToken != "" && !bearerOK(r, s.authToken) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	ro, status := s.authorize(bearer(r))
	if status != 0 {
		http.Error(w, `{"message":"unauthorized"}`, status)
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet && (path == "" || path == "health"):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && (path == "v2" || path == "v3" || path == "v3-protobuf"):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && (path == "v2/pipeline" || path == "v3/pipeline"):
		s.pipeline(w, r, false, ro)
	case r.Method == http.MethodPost && path == "v3-protobuf/pipeline":
		s.pipeline(w, r, true, ro)
	case r.Method == http.MethodPost && path == "v3/cursor":
		s.cursor(w, r, false, ro)
	case r.Method == http.MethodPost && path == "v3-protobuf/cursor":
		s.cursor(w, r, true, ro)
	default:
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}
}

func newBaton() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// take returns the stream a baton names (a new one for no baton) with its lock
// held, and removes the baton: every response hands out a fresh one.
//
// A stream opened read-write is never handed to a read-only client.
func (s *Server) take(ctx context.Context, baton *string, ro bool) (*httpStream, error) {
	s.reap()
	if baton == nil || *baton == "" {
		st, err := newStream(context.WithoutCancel(ctx), s.dbFor(ro), newSQLStore(), s.observe)
		if err != nil {
			return nil, err
		}
		hs := &httpStream{stream: st, ro: ro}
		hs.mu.Lock()
		return hs, nil
	}
	s.mu.Lock()
	hs, ok := s.streams[*baton]
	if ok && ro && !hs.ro {
		s.mu.Unlock()
		return nil, protoErr("the baton belongs to a read-write stream")
	}
	delete(s.streams, *baton)
	s.mu.Unlock()
	if !ok {
		return nil, protoErr("unknown or expired baton")
	}
	hs.mu.Lock()
	return hs, nil
}

// give hands a stream back under a new baton, or closes it.
func (s *Server) give(hs *httpStream, keep bool) *string {
	hs.lastUsed = time.Now()
	hs.mu.Unlock()
	if !keep {
		hs.close()
		return nil
	}
	b := newBaton()
	s.mu.Lock()
	s.streams[b] = hs
	s.mu.Unlock()
	return &b
}

// reap closes streams idle past the timeout.
func (s *Server) reap() {
	cut := time.Now().Add(-s.idle)
	s.mu.Lock()
	var dead []*httpStream
	for b, hs := range s.streams {
		if hs.lastUsed.Before(cut) {
			delete(s.streams, b)
			dead = append(dead, hs)
		}
	}
	s.mu.Unlock()
	for _, hs := range dead {
		hs.close()
	}
}

const maxHTTPRequestBody = 64 << 20 // 64 MB

func (s *Server) pipeline(w http.ResponseWriter, r *http.Request, protobuf, ro bool) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPRequestBody)
	body, err := readBody(r)
	defer putBuf(body)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err, protobuf)
		return
	}
	var req *pbhttp.PipelineReqBody
	var batchLens []int
	if protobuf {
		req = &pbhttp.PipelineReqBody{}
		if err := req.UnmarshalVT(body.Bytes()); err != nil {
			writeHTTPError(w, http.StatusBadRequest, err, true)
			return
		}
	} else {
		var j jPipelineReq
		if err := json.Unmarshal(body.Bytes(), &j); err != nil {
			writeHTTPError(w, http.StatusBadRequest, err, false)
			return
		}
		req = &pbhttp.PipelineReqBody{Baton: j.Baton}
		for i := range j.Requests {
			pr, err := j.Requests[i].proto()
			if err != nil {
				writeHTTPError(w, http.StatusBadRequest, err, false)
				return
			}
			req.Requests = append(req.Requests, pr)
		}
	}
	for _, rq := range req.GetRequests() {
		batchLens = append(batchLens, len(rq.GetBatch().GetBatch().GetSteps()))
	}

	hs, err := s.take(r.Context(), req.Baton, ro)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err, protobuf)
		return
	}
	resp := &pbhttp.PipelineRespBody{}
	keep := true
	for _, rq := range req.GetRequests() {
		res, closed := hs.handle(r.Context(), rq)
		resp.Results = append(resp.Results, res)
		if closed {
			keep = false
		}
	}
	resp.Baton = s.give(hs, keep)

	if protobuf {
		out, err := marshal(body, resp, false)
		if err != nil {
			writeHTTPError(w, http.StatusInternalServerError, err, true)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(out)
		return
	}
	results := make([]any, len(resp.Results))
	for i, res := range resp.Results {
		results[i] = jsonStreamResult(res, batchLens[i])
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"baton": resp.Baton, "base_url": nil, "results": results})
}

// handle runs one stream request; closed reports a close request.
func (hs *httpStream) handle(ctx context.Context, rq *pbhttp.StreamRequest) (res *pbhttp.StreamResult, closed bool) {
	ok := func(r *pbhttp.StreamResponse) *pbhttp.StreamResult {
		return &pbhttp.StreamResult{Result: &pbhttp.StreamResult_Ok{Ok: r}}
	}
	fail := func(err error) *pbhttp.StreamResult {
		return &pbhttp.StreamResult{Result: &pbhttp.StreamResult_Error{Error: toError(err)}}
	}
	switch x := rq.GetRequest().(type) {
	case *pbhttp.StreamRequest_Close:
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_Close{Close: &pbhttp.CloseStreamResp{}}}), true
	case *pbhttp.StreamRequest_Execute:
		r, err := hs.execute(ctx, x.Execute.GetStmt())
		if err != nil {
			return fail(err), false
		}
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_Execute{Execute: &pbhttp.ExecuteStreamResp{Result: r}}}), false
	case *pbhttp.StreamRequest_Batch:
		r := hs.batch(ctx, x.Batch.GetBatch())
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_Batch{Batch: &pbhttp.BatchStreamResp{Result: r}}}), false
	case *pbhttp.StreamRequest_Sequence:
		if err := hs.sequence(ctx, x.Sequence.Sql, x.Sequence.SqlId); err != nil {
			return fail(err), false
		}
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_Sequence{Sequence: &pbhttp.SequenceStreamResp{}}}), false
	case *pbhttp.StreamRequest_Describe:
		r, err := hs.describe(ctx, x.Describe.Sql, x.Describe.SqlId)
		if err != nil {
			return fail(err), false
		}
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_Describe{Describe: &pbhttp.DescribeStreamResp{Result: r}}}), false
	case *pbhttp.StreamRequest_StoreSql:
		hs.sqls.put(x.StoreSql.GetSqlId(), x.StoreSql.GetSql())
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_StoreSql{StoreSql: &pbhttp.StoreSqlStreamResp{}}}), false
	case *pbhttp.StreamRequest_CloseSql:
		hs.sqls.del(x.CloseSql.GetSqlId())
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_CloseSql{CloseSql: &pbhttp.CloseSqlStreamResp{}}}), false
	case *pbhttp.StreamRequest_GetAutocommit:
		return ok(&pbhttp.StreamResponse{Response: &pbhttp.StreamResponse_GetAutocommit{
			GetAutocommit: &pbhttp.GetAutocommitStreamResp{IsAutocommit: hs.isAutocommit()}}}), false
	}
	return fail(protoErr("unknown request")), false
}

// cursor runs a batch and streams its result entries.
func (s *Server) cursor(w http.ResponseWriter, r *http.Request, protobuf, ro bool) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPRequestBody)
	body, err := readBody(r)
	defer putBuf(body)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err, protobuf)
		return
	}
	var req *pbhttp.CursorReqBody
	if protobuf {
		req = &pbhttp.CursorReqBody{}
		if err := req.UnmarshalVT(body.Bytes()); err != nil {
			writeHTTPError(w, http.StatusBadRequest, err, true)
			return
		}
	} else {
		var j jCursorReq
		if err := json.Unmarshal(body.Bytes(), &j); err != nil {
			writeHTTPError(w, http.StatusBadRequest, err, false)
			return
		}
		b, err := j.Batch.proto()
		if err != nil {
			writeHTTPError(w, http.StatusBadRequest, err, false)
			return
		}
		req = &pbhttp.CursorReqBody{Baton: j.Baton, Batch: b}
	}
	hs, err := s.take(r.Context(), req.Baton, ro)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err, protobuf)
		return
	}
	// The stream's next baton leads the response, so it is issued before the
	// batch runs; the stream is handed back only once the batch is done.
	baton := newBaton()
	bw := bufio.NewWriter(w)
	dw := delimWriter{bw, body}
	if protobuf {
		w.Header().Set("Content-Type", "application/x-protobuf")
		dw.write(&pbhttp.CursorRespBody{Baton: &baton})
	} else {
		w.Header().Set("Content-Type", "text/plain")
		json.NewEncoder(bw).Encode(map[string]any{"baton": baton, "base_url": nil})
	}
	emit := func(e *pb.CursorEntry) {
		if protobuf {
			dw.write(e)
		} else {
			json.NewEncoder(bw).Encode(jsonCursorEntry(e))
		}
	}
	res := hs.batch(r.Context(), req.GetBatch())
	cursorEntries(req.GetBatch(), res, emit)
	bw.Flush()
	hs.lastUsed = time.Now()
	hs.mu.Unlock()
	s.mu.Lock()
	s.streams[baton] = hs
	s.mu.Unlock()
}

func writeHTTPError(w http.ResponseWriter, status int, err error, protobuf bool) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		status = http.StatusRequestEntityTooLarge
	}
	e := toError(err)
	var pe protocolError
	if !errors.As(err, &pe) && status == http.StatusBadRequest {
		e.Code = ptr("HRANA_PROTO_ERROR")
	}
	if protobuf {
		b, err := e.MarshalVT()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(status)
		w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(jsonError(e))
}
