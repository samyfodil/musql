package hrana

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/samyfodil/musql/hrana/gen/hrana"
	pbhttp "github.com/samyfodil/musql/hrana/gen/hrana/http"
)

// discardResponse is an http.ResponseWriter that keeps only the status and
// the byte count, so a benchmark measures the server's work and not a
// recorder's buffer.
type discardResponse struct {
	h      http.Header
	status int
	n      int
}

func (d *discardResponse) Header() http.Header         { return d.h }
func (d *discardResponse) Write(p []byte) (int, error) { d.n += len(p); return len(p), nil }
func (d *discardResponse) WriteHeader(status int)      { d.status = status }

func benchServer(b *testing.B) *Server {
	b.Helper()
	db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "b.musq"))
	if err != nil {
		b.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("BEGIN; CREATE TABLE t(id INTEGER PRIMARY KEY, i INT, r REAL, s TEXT, b BLOB);")
	for i := range 1000 {
		if i%7 == 0 {
			fmt.Fprintf(&sb, "INSERT INTO t VALUES(%d, NULL, %d.5, NULL, x'%032x');", i, i, i)
			continue
		}
		fmt.Fprintf(&sb, "INSERT INTO t VALUES(%d, %d, %d.25, 'row number %08d', x'%032x');", i, i*1000, i, i, i)
	}
	sb.WriteString("COMMIT;")
	if _, err := db.Exec(sb.String()); err != nil {
		b.Fatal(err)
	}
	srv := NewServer(db)
	b.Cleanup(func() { srv.Close(); db.Close() })
	return srv
}

func benchStmt(q string, args ...*pb.Value) *pb.Stmt { return &pb.Stmt{Sql: &q, Args: args} }

func benchRequests() []struct {
	name string
	path string
	body proto.Message
} {
	closeReq := &pbhttp.StreamRequest{Request: &pbhttp.StreamRequest_Close{Close: &pbhttp.CloseStreamReq{}}}
	selectAll := benchStmt("SELECT id, i, r, s, b FROM t")
	var steps []*pb.BatchStep
	for i := range 100 {
		steps = append(steps, &pb.BatchStep{Stmt: benchStmt("SELECT id, ?2, ?3, ?4 FROM t WHERE id = ?1",
			&pb.Value{Value: &pb.Value_Integer{Integer: int64(i) * 7}},
			&pb.Value{Value: &pb.Value_Float{Float: float64(i) + 0.5}},
			&pb.Value{Value: &pb.Value_Text{Text: fmt.Sprintf("argument text number %011d", i)}},
			&pb.Value{Value: &pb.Value_Blob{Blob: bytes.Repeat([]byte{byte(i)}, 32)}},
		)})
	}
	return []struct {
		name string
		path string
		body proto.Message
	}{
		{"pipeline/select1000x5", "/v3-protobuf/pipeline", &pbhttp.PipelineReqBody{Requests: []*pbhttp.StreamRequest{
			{Request: &pbhttp.StreamRequest_Execute{Execute: &pbhttp.ExecuteStreamReq{Stmt: selectAll}}}, closeReq}}},
		{"pipeline/batch100x4args", "/v3-protobuf/pipeline", &pbhttp.PipelineReqBody{Requests: []*pbhttp.StreamRequest{
			{Request: &pbhttp.StreamRequest_Batch{Batch: &pbhttp.BatchStreamReq{Batch: &pb.Batch{Steps: steps}}}}, closeReq}}},
		{"cursor/select1000x5", "/v3-protobuf/cursor", &pbhttp.CursorReqBody{Batch: &pb.Batch{Steps: []*pb.BatchStep{{Stmt: selectAll}}}}},
	}
}

// BenchmarkProtobuf serves Hrana's Protobuf endpoints in process: request
// decode, execution, result conversion and response encode.
func BenchmarkProtobuf(b *testing.B) {
	srv := benchServer(b)
	for _, bc := range benchRequests() {
		body, err := proto.Marshal(bc.body)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			w := &discardResponse{h: http.Header{}}
			for range b.N {
				w.status, w.n = 0, 0
				srv.ServeHTTP(w, httptest.NewRequest(http.MethodPost, bc.path, bytes.NewReader(body)))
				if w.status != 0 && w.status != http.StatusOK {
					b.Fatalf("%s: status %d", bc.path, w.status)
				}
				// A cursor leaves its stream open under a new baton; close it
				// outside the measurement so streams do not pile up.
				b.StopTimer()
				srv.mu.Lock()
				for baton, hs := range srv.streams {
					delete(srv.streams, baton)
					hs.close()
				}
				srv.mu.Unlock()
				b.StartTimer()
			}
			b.ReportMetric(float64(w.n), "resp-bytes")
		})
	}
}
