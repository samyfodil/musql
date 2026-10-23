package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// wsRun sends each message on a new Hrana WebSocket and returns the replies.
func (e *env) wsRun(header http.Header, msgs ...string) []string {
	e.t.Helper()
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/db/acme/app/",
		&websocket.DialOptions{Subprotocols: []string{"hrana3"}, HTTPHeader: header})
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.CloseNow()
	var out []string
	for _, m := range msgs {
		if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			e.t.Fatal(err)
		}
		_, b, err := c.Read(ctx)
		if err != nil {
			return append(out, "closed")
		}
		out = append(out, string(b))
	}
	return out
}

func TestWebSocketTokenInHello(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	full := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	ro := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app", Access: v1.Access_ACCESS_READ_ONLY}))).Msg.Token

	open := `{"type":"request","request_id":1,"request":{"type":"open_stream","stream_id":1}}`
	exec := func(id, sql string) string {
		return `{"type":"request","request_id":` + id + `,"request":{"type":"execute","stream_id":1,"stmt":{"sql":"` + sql + `"}}}`
	}
	hello := func(tok string) string { return `{"type":"hello","jwt":"` + tok + `"}` }

	r := e.wsRun(nil, hello(full), open, exec("2", "CREATE TABLE t(a)"), exec("3", "INSERT INTO t VALUES (1)"))
	if !strings.Contains(r[0], "hello_ok") || !strings.Contains(r[2], "response_ok") || !strings.Contains(r[3], "response_ok") {
		t.Fatalf("full token in hello: %v", r)
	}

	r = e.wsRun(nil, hello(ro), open, exec("2", "SELECT count(*) FROM t"), exec("3", "INSERT INTO t VALUES (2)"), exec("4", "PRAGMA query_only = 0"))
	if !strings.Contains(r[0], "hello_ok") || !strings.Contains(r[2], "response_ok") || !strings.Contains(r[3], "response_error") || !strings.Contains(r[4], "response_error") {
		t.Fatalf("read-only token in hello: %v", r)
	}

	if r = e.wsRun(nil, hello("garbage")); !strings.Contains(r[0], "hello_error") {
		t.Fatalf("bad token in hello: %v", r)
	}
	if r = e.wsRun(nil, `{"type":"hello"}`); !strings.Contains(r[0], "hello_error") {
		t.Fatalf("no token at all: %v", r)
	}

	// A token in the upgrade request still works, and read-only holds.
	h := http.Header{"Authorization": {"Bearer " + ro}}
	r = e.wsRun(h, `{"type":"hello"}`, open, exec("2", "INSERT INTO t VALUES (3)"))
	if !strings.Contains(r[0], "hello_ok") || !strings.Contains(r[2], "response_error") {
		t.Fatalf("read-only token in the upgrade request: %v", r)
	}

	// Invalidating tokens ends them for new sockets.
	must(dbs.InvalidateTokens(ctx, auth(e.admin, &v1.InvalidateTokensRequest{Org: "acme", Name: "app"})))
	if r = e.wsRun(nil, hello(full)); !strings.Contains(r[0], "hello_error") {
		t.Fatalf("invalidated token in hello: %v", r)
	}
}
