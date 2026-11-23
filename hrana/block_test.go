package hrana

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	musqldriver "github.com/samyfodil/musql/driver"
)

// confineRO makes every connection opened with "?hranatest=ro" read-only,
// so a read-only pool may hold as many as its streams need.
var confineRO = sync.OnceFunc(func() {
	musqldriver.RegisterConnectionHook(func(c *musqldriver.Conn, dsn string) error {
		if strings.Contains(dsn, "hranatest=ro") {
			c.Confine(true)
		}
		return nil
	})
})

// blockServer serves one file whose block the test sets.
func blockServer(t *testing.T) (string, *atomic.Pointer[Block]) {
	t.Helper()
	confineRO()
	path := filepath.Join(t.TempDir(), "b.musq")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := sql.Open("sqlite", path+"?hranatest=ro")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t(a)"); err != nil {
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

// wsBlockConn is a WebSocket on a blockServer; exec runs q on a stream.
type wsBlockConn struct {
	t  *testing.T
	c  *websocket.Conn
	id int
}

func dialBlock(t *testing.T, url string) *wsBlockConn {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{Subprotocols: []string{"hrana3"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	w := &wsBlockConn{t: t, c: c}
	w.send(`{"type":"hello","jwt":""}`)
	return w
}

func (w *wsBlockConn) send(m string) string {
	w.t.Helper()
	ctx := context.Background()
	if err := w.c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
		w.t.Fatal(err)
	}
	_, r, err := w.c.Read(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(r)
}

func (w *wsBlockConn) request(req string) string {
	w.t.Helper()
	w.id++
	return w.send(`{"type":"request","request_id":` + strconv.Itoa(w.id) + `,"request":` + req + `}`)
}

func (w *wsBlockConn) open(stream int) {
	w.t.Helper()
	if r := w.request(`{"type":"open_stream","stream_id":` + strconv.Itoa(stream) + `}`); !strings.Contains(r, "response_ok") {
		w.t.Fatalf("open_stream: %s", r)
	}
}

func (w *wsBlockConn) exec(stream int, q string) string {
	w.t.Helper()
	return w.request(`{"type":"execute","stream_id":` + strconv.Itoa(stream) + `,"stmt":{"sql":"` + q + `"}}`)
}

// An open stream follows a block: onto the read-only database while writes
// are blocked, where reads still work, and back once they are allowed.
func TestBlockReachesOpenWebSocketStreams(t *testing.T) {
	url, b := blockServer(t)
	w := dialBlock(t, url)
	ok := func(r string) bool { return strings.Contains(r, "response_ok") }
	blocked := func(r string) bool { return strings.Contains(r, `"code":"BLOCKED"`) }
	w.open(1)
	if r := w.exec(1, "INSERT INTO t VALUES (1)"); !ok(r) {
		t.Fatalf("unblocked write: %s", r)
	}

	b.Store(&Block{Writes: true, Reason: "writes are off"})
	if r := w.exec(1, "SELECT count(*) FROM t"); !ok(r) || !strings.Contains(r, `"value":"1"`) {
		t.Errorf("read on a stream opened for writing, once writes are blocked: %s", r)
	}
	if r := w.exec(1, "INSERT INTO t VALUES (2)"); !blocked(r) || !strings.Contains(r, "writes are off") {
		t.Errorf("write on a stream opened for writing, once writes are blocked: %s", r)
	}
	w.open(2)
	if r := w.exec(2, "SELECT count(*) FROM t"); !ok(r) {
		t.Errorf("read on a stream opened while writes are blocked: %s", r)
	}
	if r := w.exec(2, "INSERT INTO t VALUES (2)"); !blocked(r) {
		t.Errorf("write on a stream opened while writes are blocked: %s", r)
	}

	b.Store(&Block{})
	for _, st := range []int{1, 2} {
		if r := w.exec(st, "INSERT INTO t VALUES (3)"); !ok(r) {
			t.Errorf("write on stream %d once writes are allowed again: %s", st, r)
		}
	}

	b.Store(&Block{Reads: true})
	if r := w.exec(2, "SELECT 1"); !blocked(r) {
		t.Errorf("read on an open stream once reads are blocked: %s", r)
	}
}

// A transaction open across a change keeps its connection until it ends:
// one that can write refuses all but ROLLBACK while writes are blocked, and
// one on the read-only database cannot write until it commits.
func TestBlockWaitsForTransaction(t *testing.T) {
	url, b := blockServer(t)
	w := dialBlock(t, url)
	ok := func(r string) bool { return strings.Contains(r, "response_ok") }
	w.open(1)
	for _, q := range []string{"BEGIN", "INSERT INTO t VALUES (1)"} {
		if r := w.exec(1, q); !ok(r) {
			t.Fatalf("%s: %s", q, r)
		}
	}

	b.Store(&Block{Writes: true})
	if r := w.exec(1, "SELECT count(*) FROM t"); !strings.Contains(r, `"code":"BLOCKED"`) {
		t.Errorf("read in a write transaction once writes are blocked: %s", r)
	}
	if r := w.exec(1, "ROLLBACK"); !ok(r) {
		t.Fatalf("rollback once writes are blocked: %s", r)
	}
	if r := w.exec(1, "SELECT count(*) FROM t"); !ok(r) || !strings.Contains(r, `"value":"0"`) {
		t.Errorf("read after the rollback: %s", r)
	}
	for _, q := range []string{"BEGIN", "SELECT count(*) FROM t"} {
		if r := w.exec(1, q); !ok(r) {
			t.Fatalf("read transaction while writes are blocked, %s: %s", q, r)
		}
	}

	b.Store(&Block{})
	if r := w.exec(1, "INSERT INTO t VALUES (2)"); ok(r) || !strings.Contains(r, "readonly") {
		t.Errorf("write in a read-only transaction once writes are allowed: %s", r)
	}
	if r := w.exec(1, "COMMIT"); !ok(r) {
		t.Fatalf("commit: %s", r)
	}
	if r := w.exec(1, "INSERT INTO t VALUES (3)"); !ok(r) {
		t.Errorf("write after the commit: %s", r)
	}
}
