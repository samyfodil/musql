package hrana

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	_ "github.com/samyfodil/musql/driver"
	pb "github.com/samyfodil/musql/gen/hrana"
	pbhttp "github.com/samyfodil/musql/gen/hrana/http"
	pbws "github.com/samyfodil/musql/gen/hrana/ws"
)

func newTestServer(t *testing.T) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "h.musq"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(db)
	hs := httptest.NewServer(srv)
	t.Cleanup(func() { hs.Close(); srv.Close(); db.Close() })
	return hs.URL
}

// postJSON sends body to path and decodes the JSON response.
func postJSON(t *testing.T, url, path, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v: %s", path, err, b)
	}
	return out
}

func TestV3JSONPipeline(t *testing.T) {
	url := newTestServer(t)
	out := postJSON(t, url, "/v3/pipeline", `{"baton":null,"requests":[
		{"type":"execute","stmt":{"sql":"CREATE TABLE t(a INTEGER, b TEXT)"}},
		{"type":"store_sql","sql_id":1,"sql":"INSERT INTO t VALUES(?, ?)"},
		{"type":"execute","stmt":{"sql_id":1,"args":[{"type":"integer","value":"9007199254740993"},{"type":"text","value":"x"}]}},
		{"type":"batch","batch":{"steps":[
			{"stmt":{"sql":"BEGIN"}},
			{"condition":{"type":"ok","step":0},"stmt":{"sql":"INSERT INTO t VALUES(2, 'y')"}},
			{"condition":{"type":"ok","step":1},"stmt":{"sql":"INSERT INTO nope VALUES(1)"}},
			{"condition":{"type":"error","step":2},"stmt":{"sql":"ROLLBACK"}},
			{"condition":{"type":"not","cond":{"type":"error","step":2}},"stmt":{"sql":"COMMIT"}}
		]}},
		{"type":"get_autocommit"},
		{"type":"execute","stmt":{"sql":"SELECT a, b FROM t ORDER BY a"}},
		{"type":"describe","sql":"SELECT b FROM t WHERE a = :id"}
	]}`)
	baton, _ := out["baton"].(string)
	if baton == "" {
		t.Fatalf("no baton: %v", out)
	}
	results := out["results"].([]any)
	for i, r := range results {
		if r.(map[string]any)["type"] != "ok" {
			t.Fatalf("result %d: %v", i, r)
		}
	}
	batch := results[3].(map[string]any)["response"].(map[string]any)["result"].(map[string]any)
	errs := batch["step_errors"].([]any)
	// Step 2 fails, so 3 (its error branch) runs and 4 (its "not error"
	// branch) is skipped: a skipped step has neither a result nor an error.
	if errs[2] == nil || batch["step_results"].([]any)[3] == nil || batch["step_results"].([]any)[4] != nil || errs[4] != nil {
		t.Fatalf("batch conditions: %v", batch)
	}
	if ac := results[4].(map[string]any)["response"].(map[string]any)["is_autocommit"]; ac != true {
		t.Fatalf("autocommit after the rollback: %v", ac)
	}
	sel := results[5].(map[string]any)["response"].(map[string]any)["result"].(map[string]any)
	rows := sel["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the rolled-back batch left rows: %v", rows)
	}
	v := rows[0].([]any)[0].(map[string]any)
	if v["type"] != "integer" || v["value"] != "9007199254740993" {
		t.Fatalf("integer round trip: %v", v)
	}
	if d := sel["cols"].([]any)[0].(map[string]any)["decltype"]; d != "INTEGER" {
		t.Fatalf("decltype: %v", d)
	}
	desc := results[6].(map[string]any)["response"].(map[string]any)["result"].(map[string]any)
	if p := desc["params"].([]any)[0].(map[string]any)["name"]; p != ":id" || desc["is_readonly"] != true {
		t.Fatalf("describe: %v", desc)
	}

	// The baton continues the stream; the old one is spent.
	next := postJSON(t, url, "/v3/pipeline", `{"baton":"`+baton+`","requests":[{"type":"close"}]}`)
	if next["baton"] != nil {
		t.Fatalf("close kept the stream: %v", next)
	}
	resp, _ := http.Post(url+"/v3/pipeline", "application/json", strings.NewReader(`{"baton":"`+baton+`","requests":[]}`))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a spent baton was accepted")
	}
}

func TestV3ProtobufPipelineAndCursor(t *testing.T) {
	url := newTestServer(t)
	sqlText := func(s string) *pb.Stmt { return &pb.Stmt{Sql: &s} }
	req := &pbhttp.PipelineReqBody{Requests: []*pbhttp.StreamRequest{
		{Request: &pbhttp.StreamRequest_Execute{Execute: &pbhttp.ExecuteStreamReq{Stmt: sqlText("CREATE TABLE t(a, b BLOB)")}}},
		{Request: &pbhttp.StreamRequest_Execute{Execute: &pbhttp.ExecuteStreamReq{Stmt: &pb.Stmt{
			Sql:  proto.String("INSERT INTO t VALUES(?, ?), (?, ?)"),
			Args: []*pb.Value{{Value: &pb.Value_Integer{Integer: -5}}, {Value: &pb.Value_Blob{Blob: []byte{0xff, 0}}}, {Value: &pb.Value_Float{Float: 0.5}}, {Value: &pb.Value_Null_{Null: &pb.Value_Null{}}}},
		}}}},
	}}
	body, _ := proto.Marshal(req)
	resp, err := http.Post(url+"/v3-protobuf/pipeline", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out pbhttp.PipelineRespBody
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Baton == nil || len(out.Results) != 2 || out.Results[1].GetOk() == nil {
		t.Fatalf("pipeline: %v", &out)
	}
	if n := out.Results[1].GetOk().GetExecute().GetResult().GetAffectedRowCount(); n != 2 {
		t.Fatalf("affected rows %d", n)
	}

	// The cursor streams a batch: step_begin, rows, step_end per executed step.
	creq := &pbhttp.CursorReqBody{Baton: out.Baton, Batch: &pb.Batch{Steps: []*pb.BatchStep{
		{Stmt: sqlText("SELECT a, b FROM t ORDER BY a")},
		{Stmt: sqlText("SELECT * FROM missing")},
	}}}
	body, _ = proto.Marshal(creq)
	resp, err = http.Post(url+"/v3-protobuf/cursor", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	var head pbhttp.CursorRespBody
	if err := protodelim.UnmarshalFrom(r, &head); err != nil || head.Baton == nil {
		t.Fatalf("cursor head: %v %v", &head, err)
	}
	var kinds []string
	for {
		var e pb.CursorEntry
		if err := protodelim.UnmarshalFrom(r, &e); err != nil {
			break
		}
		switch x := e.Entry.(type) {
		case *pb.CursorEntry_StepBegin:
			kinds = append(kinds, "begin")
		case *pb.CursorEntry_Row:
			kinds = append(kinds, "row")
			if len(x.Row.Values) != 2 {
				t.Fatalf("row width %d", len(x.Row.Values))
			}
		case *pb.CursorEntry_StepEnd:
			kinds = append(kinds, "end")
		case *pb.CursorEntry_StepError:
			kinds = append(kinds, "error")
		}
	}
	if got := strings.Join(kinds, ","); got != "begin,row,row,end,error" {
		t.Fatalf("cursor entries: %s", got)
	}
}

func TestV3JSONCursor(t *testing.T) {
	url := newTestServer(t)
	resp, err := http.Post(url+"/v3/cursor", "application/json", strings.NewReader(
		`{"baton":null,"batch":{"steps":[{"stmt":{"sql":"SELECT 1 AS one, 'two' AS two"}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("%v: %s", err, sc.Text())
		}
		lines = append(lines, m)
	}
	if len(lines) != 4 || lines[0]["baton"] == nil || lines[1]["type"] != "step_begin" || lines[2]["type"] != "row" || lines[3]["type"] != "step_end" {
		t.Fatalf("cursor lines: %v", lines)
	}
}

func TestAuthRequired(t *testing.T) {
	db, _ := sql.Open("sqlite", filepath.Join(t.TempDir(), "a.musq"))
	defer db.Close()
	srv := NewServer(db, WithAuthToken("tok"))
	hs := httptest.NewServer(srv)
	defer hs.Close()
	defer srv.Close()
	resp, _ := http.Post(hs.URL+"/v3/pipeline", "application/json", strings.NewReader(`{"baton":null,"requests":[]}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestWebSocketProtobuf(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "w.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := NewServer(db, WithAuthToken("tok"))
	hs := httptest.NewServer(srv)
	defer hs.Close()
	defer srv.Close()
	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http")

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"hrana3-protobuf"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != "hrana3-protobuf" {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	send := func(m *pbws.ClientMsg) {
		b, _ := proto.Marshal(m)
		if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() *pbws.ServerMsg {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := &pbws.ServerMsg{}
		if err := proto.Unmarshal(b, m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	req := func(id int32, r *pbws.RequestMsg) {
		r.RequestId = id
		send(&pbws.ClientMsg{Msg: &pbws.ClientMsg_Request{Request: r}})
	}
	send(&pbws.ClientMsg{Msg: &pbws.ClientMsg_Hello{Hello: &pbws.HelloMsg{Jwt: proto.String("tok")}}})
	if recv().GetHelloOk() == nil {
		t.Fatal("no hello_ok")
	}
	// Pipelined without waiting, as the spec allows: open, write, read.
	req(1, &pbws.RequestMsg{Request: &pbws.RequestMsg_OpenStream{OpenStream: &pbws.OpenStreamReq{StreamId: 7}}})
	req(2, &pbws.RequestMsg{Request: &pbws.RequestMsg_Execute{Execute: &pbws.ExecuteReq{StreamId: 7, Stmt: &pb.Stmt{Sql: proto.String("CREATE TABLE t(a)")}}}})
	req(3, &pbws.RequestMsg{Request: &pbws.RequestMsg_Execute{Execute: &pbws.ExecuteReq{StreamId: 7, Stmt: &pb.Stmt{Sql: proto.String("INSERT INTO t VALUES(41),(42)")}}}})
	req(4, &pbws.RequestMsg{Request: &pbws.RequestMsg_OpenCursor{OpenCursor: &pbws.OpenCursorReq{StreamId: 7, CursorId: 1,
		Batch: &pb.Batch{Steps: []*pb.BatchStep{{Stmt: &pb.Stmt{Sql: proto.String("SELECT a FROM t ORDER BY a")}}}}}}})
	req(5, &pbws.RequestMsg{Request: &pbws.RequestMsg_FetchCursor{FetchCursor: &pbws.FetchCursorReq{CursorId: 1, MaxCount: 100}}})
	req(6, &pbws.RequestMsg{Request: &pbws.RequestMsg_Execute{Execute: &pbws.ExecuteReq{StreamId: 99, Stmt: &pb.Stmt{Sql: proto.String("SELECT 1")}}}})
	got := map[int32]*pbws.ServerMsg{}
	for len(got) < 6 {
		m := recv()
		if ok := m.GetResponseOk(); ok != nil {
			got[ok.RequestId] = m
		} else if e := m.GetResponseError(); e != nil {
			got[e.RequestId] = m
		}
	}
	for id := int32(1); id <= 5; id++ {
		if got[id].GetResponseOk() == nil {
			t.Fatalf("request %d: %v", id, got[id])
		}
	}
	if got[6].GetResponseError() == nil {
		t.Fatal("a request on an unknown stream did not fail")
	}
	fc := got[5].GetResponseOk().GetFetchCursor()
	var rows []int64
	for _, e := range fc.GetEntries() {
		if r := e.GetRow(); r != nil {
			rows = append(rows, r.Values[0].GetInteger())
		}
	}
	if !fc.Done || len(rows) != 2 || rows[0] != 41 || rows[1] != 42 {
		t.Fatalf("cursor: done=%v rows=%v", fc.Done, rows)
	}
}

func TestWebSocketRejectsBadToken(t *testing.T) {
	db, _ := sql.Open("sqlite", filepath.Join(t.TempDir(), "w.musq"))
	defer db.Close()
	srv := NewServer(db, WithAuthToken("tok"))
	hs := httptest.NewServer(srv)
	defer hs.Close()
	defer srv.Close()
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), &websocket.DialOptions{Subprotocols: []string{"hrana3"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","jwt":"wrong"}`))
	_, b, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(b), "hello_error") {
		t.Fatalf("bad token: %s %v", b, err)
	}
}

func TestMultiRoutesByHost(t *testing.T) {
	dir := t.TempDir()
	m := NewMulti(dir, true)
	defer m.Close()
	hs := httptest.NewServer(m)
	defer hs.Close()
	post := func(host, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v3/pipeline", strings.NewReader(body))
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	exec := func(sql string) string {
		return `{"baton":null,"requests":[{"type":"execute","stmt":{"sql":"` + sql + `"}}]}`
	}
	post("a.db.test", exec("CREATE TABLE t(x)"))
	post("a.db.test", exec("INSERT INTO t VALUES(1)"))
	post("b.db.test", exec("CREATE TABLE t(x)"))
	_, body := post("b.db.test", exec("SELECT count(*) FROM t"))
	if !strings.Contains(body, `"value":"0"`) {
		t.Fatalf("b saw a's row: %s", body)
	}
	if code, _ := post("127.0.0.1", exec("SELECT 1")); code != http.StatusNotFound {
		t.Fatalf("a host with no database name: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.musq")); err != nil {
		t.Fatalf("a.musq was not created: %v", err)
	}
	strict := httptest.NewServer(NewMulti(dir, false))
	defer strict.Close()
	req, _ := http.NewRequest(http.MethodPost, strict.URL+"/v3/pipeline", strings.NewReader(exec("SELECT 1")))
	req.Host = "nope.db.test"
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown database without -create: %d", resp.StatusCode)
	}
}
