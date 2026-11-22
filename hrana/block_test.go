package hrana

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

// blockServer serves one file whose block the test sets.
func blockServer(t *testing.T) (string, *atomic.Pointer[Block]) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "b.musq")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ro.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t(a)"); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Exec("PRAGMA query_only = 1"); err != nil {
		t.Fatal(err)
	}
	var b atomic.Pointer[Block]
	b.Store(&Block{})
	srv := NewServer(db, WithReadOnlyDB(ro), WithBlock(func() Block { return *b.Load() }))
	hs := httptest.NewServer(srv)
	t.Cleanup(func() { hs.Close(); srv.Close(); ro.Close(); db.Close() })
	return hs.URL, &b
}

func TestBlockOnHTTP(t *testing.T) {
	url, b := blockServer(t)
	if _, body := post(t, url, "", exec1("INSERT INTO t VALUES (1)")); strings.Contains(body, `"error"`) {
		t.Fatalf("unblocked write: %s", body)
	}

	b.Store(&Block{Writes: true, Reason: "writes are off"})
	_, body := post(t, url, "", exec1("INSERT INTO t VALUES (2)"))
	if !strings.Contains(body, `"code":"BLOCKED"`) || !strings.Contains(body, "Operation was blocked: writes are off") {
		t.Fatalf("write while writes are blocked: %s", body)
	}
	if _, body := post(t, url, "", exec1("SELECT count(*) FROM t")); strings.Contains(body, `"error"`) {
		t.Fatalf("read while writes are blocked: %s", body)
	}
	seq := `{"requests":[{"type":"sequence","sql":"SELECT 1; DELETE FROM t"},{"type":"close"}]}`
	if _, body := post(t, url, "", seq); !strings.Contains(body, `"code":"BLOCKED"`) {
		t.Fatalf("a sequence that writes while writes are blocked: %s", body)
	}
	batch := `{"requests":[{"type":"batch","batch":{"steps":[{"stmt":{"sql":"SELECT 1"}},{"stmt":{"sql":"UPDATE t SET a = 3"}}]}},{"type":"close"}]}`
	_, body = post(t, url, "", batch)
	if !strings.Contains(body, `"code":"BLOCKED"`) || strings.Count(body, `"code":`) != 1 {
		t.Fatalf("a batch whose second step writes while writes are blocked: %s", body)
	}

	b.Store(&Block{Reads: true})
	_, body = post(t, url, "", exec1("SELECT count(*) FROM t"))
	if !strings.Contains(body, `"code":"BLOCKED"`) || !strings.Contains(body, `"Operation was blocked"`) {
		t.Fatalf("read while reads are blocked: %s", body)
	}

	b.Store(&Block{})
	if _, body := post(t, url, "", exec1("SELECT count(*) FROM t")); !strings.Contains(body, `"value":"1"`) {
		t.Fatalf("read once unblocked: %s", body)
	}
}

func TestBlockReachesOpenWebSocketStreams(t *testing.T) {
	url, b := blockServer(t)
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{Subprotocols: []string{"hrana3"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	send := func(m string) string {
		t.Helper()
		if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
		_, r, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return string(r)
	}
	exec2 := func(id, q string) string {
		return `{"type":"request","request_id":` + id + `,"request":{"type":"execute","stream_id":2,"stmt":{"sql":"` + q + `"}}}`
	}
	send(`{"type":"hello","jwt":""}`)
	send(openStream)
	if r := send(execMsg("2", "INSERT INTO t VALUES (1)")); !strings.Contains(r, "response_ok") {
		t.Fatalf("unblocked write: %s", r)
	}

	b.Store(&Block{Writes: true})
	if r := send(execMsg("3", "SELECT 1")); !strings.Contains(r, `"code":"BLOCKED"`) {
		t.Errorf("a stream opened for writing, once writes are blocked: %s", r)
	}
	send(`{"type":"request","request_id":4,"request":{"type":"open_stream","stream_id":2}}`)
	if r := send(exec2("5", "SELECT count(*) FROM t")); !strings.Contains(r, "response_ok") {
		t.Errorf("read on a stream opened while writes are blocked: %s", r)
	}
	if r := send(exec2("6", "INSERT INTO t VALUES (2)")); !strings.Contains(r, `"code":"BLOCKED"`) {
		t.Errorf("write on a stream opened while writes are blocked: %s", r)
	}

	b.Store(&Block{Reads: true})
	if r := send(exec2("7", "SELECT 1")); !strings.Contains(r, `"code":"BLOCKED"`) {
		t.Errorf("read on an open stream once reads are blocked: %s", r)
	}
}
