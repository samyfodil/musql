package hrana

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// authServer serves one file with tokens "rw" and "ro"; the read-only one runs
// on a second handle with query_only set.
func authServer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.musq")
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
	srv := NewServer(db, WithReadOnlyDB(ro), WithAuth(func(tok string) (bool, error) {
		switch tok {
		case "rw":
			return false, nil
		case "ro":
			return true, nil
		case "busy":
			return false, fmt.Errorf("%w: the records are out of reach", ErrUnavailable)
		}
		return false, errors.New("bad token")
	}))
	hs := httptest.NewServer(srv)
	t.Cleanup(func() { hs.Close(); srv.Close(); ro.Close(); db.Close() })
	return hs.URL
}

func post(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v2/pipeline", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func exec1(sqlText string) string {
	return `{"requests":[{"type":"execute","stmt":{"sql":"` + sqlText + `"}},{"type":"close"}]}`
}

func TestAuthHookOnHTTP(t *testing.T) {
	url := authServer(t)
	if status, _ := post(t, url, "", exec1("SELECT 1")); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	if status, _ := post(t, url, "nope", exec1("SELECT 1")); status != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", status)
	}
	if status, out := post(t, url, "rw", exec1("INSERT INTO t VALUES (1)")); status != 200 || strings.Contains(out, `"error"`) {
		t.Fatalf("rw write: %d %s", status, out)
	}
	if status, out := post(t, url, "ro", exec1("SELECT count(*) FROM t")); status != 200 || strings.Contains(out, `"error"`) {
		t.Fatalf("ro read: %d %s", status, out)
	}
	if status, out := post(t, url, "ro", exec1("INSERT INTO t VALUES (2)")); status != 200 || !strings.Contains(out, `"error"`) {
		t.Fatalf("ro write was not refused: %d %s", status, out)
	}
}

func TestReadOnlyTokenCannotTakeReadWriteBaton(t *testing.T) {
	url := authServer(t)
	_, out := post(t, url, "rw", `{"requests":[{"type":"execute","stmt":{"sql":"BEGIN"}}]}`)
	_, after, ok := strings.Cut(out, `"baton":"`)
	if !ok {
		t.Fatalf("no baton: %s", out)
	}
	baton, _, _ := strings.Cut(after, `"`)
	status, _ := post(t, url, "ro", `{"baton":"`+baton+`","requests":[{"type":"execute","stmt":{"sql":"INSERT INTO t VALUES (3)"}}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("read-only token on a read-write baton: %d", status)
	}
}

// wsSession dials, sends each message and returns the replies, one per
// message; "closed" stands for a reply that never came.
func wsSession(t *testing.T, url string, header http.Header, msgs ...string) (replies []string, dialErr error) {
	t.Helper()
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{Subprotocols: []string{"hrana3"}, HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	defer c.CloseNow()
	for _, m := range msgs {
		if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
		_, b, err := c.Read(ctx)
		if err != nil {
			replies = append(replies, "closed")
			return replies, nil
		}
		replies = append(replies, string(b))
	}
	return replies, nil
}

func execMsg(id string, sqlText string) string {
	return `{"type":"request","request_id":` + id + `,"request":{"type":"execute","stream_id":1,"stmt":{"sql":"` + sqlText + `"}}}`
}

const openStream = `{"type":"request","request_id":1,"request":{"type":"open_stream","stream_id":1}}`

func TestAuthHookOnWebSocketHello(t *testing.T) {
	url := authServer(t)

	r, err := wsSession(t, url, nil, `{"type":"hello","jwt":"rw"}`, openStream, execMsg("2", "INSERT INTO t VALUES (1)"))
	if err != nil || !strings.Contains(r[0], "hello_ok") || !strings.Contains(r[2], "response_ok") {
		t.Fatalf("rw hello: %v %v", r, err)
	}

	r, err = wsSession(t, url, nil, `{"type":"hello","jwt":"ro"}`, openStream, execMsg("2", "SELECT count(*) FROM t"), execMsg("3", "INSERT INTO t VALUES (2)"))
	if err != nil || !strings.Contains(r[0], "hello_ok") || !strings.Contains(r[2], "response_ok") || !strings.Contains(r[3], "response_error") {
		t.Fatalf("ro hello: %v %v", r, err)
	}

	r, err = wsSession(t, url, nil, `{"type":"hello","jwt":"nope"}`)
	if err != nil || !strings.Contains(r[0], "hello_error") {
		t.Fatalf("bad hello: %v %v", r, err)
	}
	r, err = wsSession(t, url, nil, `{"type":"hello"}`)
	if err != nil || !strings.Contains(r[0], "hello_error") {
		t.Fatalf("empty hello: %v %v", r, err)
	}
	r, err = wsSession(t, url, nil, openStream)
	if err != nil || r[0] != "closed" {
		t.Fatalf("request before hello: %v %v", r, err)
	}
}

func TestAuthHookOnWebSocketUpgrade(t *testing.T) {
	url := authServer(t)

	h := http.Header{"Authorization": {"Bearer ro"}}
	r, err := wsSession(t, url, h, `{"type":"hello"}`, openStream, execMsg("2", "INSERT INTO t VALUES (1)"))
	if err != nil || !strings.Contains(r[0], "hello_ok") || !strings.Contains(r[2], "response_error") {
		t.Fatalf("ro header: %v %v", r, err)
	}
	// A token in the hello replaces the one in the header.
	r, err = wsSession(t, url, h, `{"type":"hello","jwt":"rw"}`, openStream, execMsg("2", "INSERT INTO t VALUES (1)"))
	if err != nil || !strings.Contains(r[2], "response_ok") {
		t.Fatalf("hello over header: %v %v", r, err)
	}
	if _, err := wsSession(t, url, http.Header{"Authorization": {"Bearer nope"}}, `{"type":"hello"}`); err == nil {
		t.Fatal("a bad token in the upgrade request got a socket")
	}
}

func TestAuthHookNeedsReadOnlyDB(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "n.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := NewServer(db, WithAuth(func(string) (bool, error) { return true, nil }))
	defer srv.Close()
	hs := httptest.NewServer(srv)
	defer hs.Close()
	if status, _ := post(t, hs.URL, "x", exec1("SELECT 1")); status != http.StatusForbidden {
		t.Fatalf("read-only without a read-only database: %d", status)
	}
	r, err := wsSession(t, hs.URL, nil, `{"type":"hello","jwt":"x"}`)
	if err != nil || !strings.Contains(r[0], "hello_error") {
		t.Fatalf("hello: %v %v", r, err)
	}
}

// TestAuthHookUnavailable: a check that cannot decide refuses the client as
// one to retry, not as unauthorized: HTTP 503, a WebSocket upgrade refused
// the same way, and a hello error coded UNAVAILABLE. A Multi refuses it so
// before it opens anything.
func TestAuthHookUnavailable(t *testing.T) {
	url := authServer(t)
	if status, out := post(t, url, "busy", exec1("SELECT 1")); status != http.StatusServiceUnavailable || !strings.Contains(out, "try again") {
		t.Fatalf("HTTP: %d %s", status, out)
	}
	r, err := wsSession(t, url, nil, `{"type":"hello","jwt":"busy"}`)
	if err != nil || !strings.Contains(r[0], "hello_error") || !strings.Contains(r[0], "UNAVAILABLE") {
		t.Fatalf("hello: %v %v", r, err)
	}
	_, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{
		Subprotocols: []string{"hrana3"}, HTTPHeader: http.Header{"Authorization": {"Bearer busy"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upgrade: %v %v", resp, err)
	}

	m := NewMulti(t.TempDir(), true, WithAuth(func(string) (bool, error) { return false, ErrUnavailable }))
	defer m.Close()
	hs := httptest.NewServer(m)
	defer hs.Close()
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v2/pipeline", strings.NewReader(exec1("SELECT 1")))
	req.Host = "db.example"
	req.Header.Set("Authorization", "Bearer any")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Multi: %d", res.StatusCode)
	}
}
