package hrana

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	pb "github.com/samyfodil/musql/hrana/gen/hrana"
	pbhttp "github.com/samyfodil/musql/hrana/gen/hrana/http"
)

// WithPrimary makes the server a read replica's. It runs a pipeline or cursor
// request itself only when readOnly accepts every statement in it and its
// baton, if any, is one it handed out. Anything else goes to primary whole:
// a write, a transaction, stored SQL, a baton the primary issued, a client
// this server's auth refuses, and every WebSocket.
//
// A stream begun here holds only read-only statements in autocommit, so when
// a later request on it must go to the primary, the stream is closed and the
// request goes without its baton; the primary's baton leads from then on.
func WithPrimary(primary http.Handler, readOnly func(sql string) bool) Option {
	return func(c *config) { c.primary, c.readOnly = primary, readOnly }
}

// forward hands r to the primary with body, dropping a baton this server
// issued.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, body []byte, protobuf, cursor bool, baton *string) {
	if baton != nil && *baton != "" {
		s.mu.Lock()
		hs, ok := s.streams[*baton]
		delete(s.streams, *baton)
		s.mu.Unlock()
		if ok {
			hs.mu.Lock()
			hs.close()
			hs.mu.Unlock()
			if body = withoutBaton(body, protobuf, cursor); body == nil {
				writeHTTPError(w, http.StatusBadRequest, protoErr("malformed request"), protobuf)
				return
			}
		}
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	s.primary.ServeHTTP(w, r)
}

func withoutBaton(body []byte, protobuf, cursor bool) []byte {
	if protobuf {
		var m proto.Message = &pbhttp.PipelineReqBody{}
		if cursor {
			m = &pbhttp.CursorReqBody{}
		}
		if proto.Unmarshal(body, m) != nil {
			return nil
		}
		switch x := m.(type) {
		case *pbhttp.PipelineReqBody:
			x.Baton = nil
		case *pbhttp.CursorReqBody:
			x.Baton = nil
		}
		out, err := proto.Marshal(m)
		if err != nil {
			return nil
		}
		return out
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	delete(m, "baton")
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// replicaServe serves r on a replica: here, or at the primary.
func (s *Server) replicaServe(w http.ResponseWriter, r *http.Request) {
	if isWebSocket(r) {
		s.primary.ServeHTTP(w, r)
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	cursor := path == "v3/cursor" || path == "v3-protobuf/cursor"
	if r.Method != http.MethodPost || !(cursor || path == "v2/pipeline" || path == "v3/pipeline" || path == "v3-protobuf/pipeline") {
		s.serve(w, r)
		return
	}
	protobuf := strings.HasPrefix(path, "v3-protobuf")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPRequestBody))
	r.Body.Close()
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err, protobuf)
		return
	}
	var baton *string
	here := false
	if cursor {
		if req, err := decodeCursor(body, protobuf); err == nil {
			baton, here = req.Baton, s.batchHere(req.GetBatch())
		}
	} else if req, err := decodePipeline(body, protobuf); err == nil {
		baton, here = req.Baton, s.pipelineHere(req)
	}
	if here && baton != nil && *baton != "" {
		s.mu.Lock()
		_, here = s.streams[*baton]
		s.mu.Unlock()
	}
	if here && s.authToken != "" && !bearerOK(r, s.authToken) {
		here = false
	}
	if here {
		if _, status := s.authorize(bearer(r)); status != 0 {
			here = false
		}
	}
	if !here {
		s.forward(w, r, body, protobuf, cursor, baton)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.serve(w, r)
}

func (s *Server) pipelineHere(req *pbhttp.PipelineReqBody) bool {
	for _, rq := range req.GetRequests() {
		switch x := rq.GetRequest().(type) {
		case *pbhttp.StreamRequest_Close, *pbhttp.StreamRequest_GetAutocommit:
		case *pbhttp.StreamRequest_Execute:
			if !s.stmtHere(x.Execute.GetStmt()) {
				return false
			}
		case *pbhttp.StreamRequest_Batch:
			if !s.batchHere(x.Batch.GetBatch()) {
				return false
			}
		case *pbhttp.StreamRequest_Sequence:
			if x.Sequence.Sql == nil || !s.readOnly(*x.Sequence.Sql) {
				return false
			}
		case *pbhttp.StreamRequest_Describe:
			if x.Describe.Sql == nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Server) batchHere(b *pb.Batch) bool {
	if b == nil {
		return false
	}
	for _, st := range b.GetSteps() {
		if !s.stmtHere(st.GetStmt()) {
			return false
		}
	}
	return true
}

func (s *Server) stmtHere(st *pb.Stmt) bool {
	return st != nil && st.Sql != nil && st.SqlId == nil && s.readOnly(*st.Sql)
}
