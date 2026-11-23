package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/tursodatabase/libsql-client-go/libsql"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
	"github.com/samyfodil/musql/hrana"
)

func TestPolicyOver(t *testing.T) {
	none, writes, reads := hrana.Block{}, hrana.Block{Writes: true, Reason: "w"}, hrana.Block{Reads: true, Reason: "r"}
	for _, c := range []struct {
		p    Policy
		b    hrana.Block
		want hrana.Block
	}{
		{Policy{}, none, none},
		{Policy{}, writes, writes},
		{Policy{Access: ReadOnly, Reason: "quota"}, none, hrana.Block{Writes: true, Reason: "quota"}},
		{Policy{Access: ReadOnly, Reason: "quota"}, writes, hrana.Block{Writes: true, Reason: "quota"}},
		{Policy{Access: ReadOnly, Reason: "quota"}, reads, reads},
		{Policy{Access: Deny, Reason: "lapsed"}, reads, hrana.Block{Reads: true, Writes: true, Reason: "lapsed"}},
		{Policy{Access: 7}, none, hrana.Block{Reads: true, Writes: true, Reason: "access to this database is denied"}},
		{Policy{Access: ReadOnly}, none, hrana.Block{Writes: true, Reason: "writes to this database are not allowed"}},
	} {
		if got := c.p.over(c.b); got != c.want {
			t.Errorf("%+v over %+v: %+v, want %+v", c.p, c.b, got, c.want)
		}
	}
}

// policyCache asks at most once a policyTTL, keeps its last answer through
// an ask that fails, and refuses until a first ask succeeds.
func TestPolicyCache(t *testing.T) {
	asks := 0
	err := errors.New("state out of reach")
	c := &policyCache{ask: func() (Policy, error) {
		asks++
		return Policy{Access: Access(asks - 2)}, err
	}}
	if c.get() != policyUnread || c.get() != policyUnread || asks != 2 {
		t.Fatalf("before a first answer: %d asks", asks)
	}
	err = nil
	asks = 0
	c.ask = func() (Policy, error) {
		asks++
		return Policy{Access: Access(asks)}, err
	}
	if c.get().Access != 1 || c.get().Access != 1 || asks != 1 {
		t.Fatalf("within the TTL: %d asks", asks)
	}
	c.at = c.at.Add(-policyTTL)
	if c.get().Access != 2 {
		t.Fatal("after the TTL: not asked again")
	}
	c.at, err = c.at.Add(-policyTTL), errors.New("state out of reach")
	if c.get().Access != 2 || asks != 3 {
		t.Fatalf("failed ask: %d asks", asks)
	}
}

// policies is an access policy that answers per org, set by the test.
type policies struct {
	mu sync.Mutex
	m  map[string]Policy
}

func (p *policies) set(org string, pol Policy) {
	p.mu.Lock()
	p.m[org] = pol
	p.mu.Unlock()
	// An open database asks again once its answer is policyTTL old.
	time.Sleep(policyTTL + 50*time.Millisecond)
}

func (p *policies) policy(ctx context.Context, o *state.Org, d *state.Database) Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[o.Slug]
}

// wsStream is one Hrana stream on a WebSocket kept open across calls.
type wsStream struct {
	t  *testing.T
	c  *websocket.Conn
	id int
}

func (e *env) wsStream(path, tok string) *wsStream {
	e.t.Helper()
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+path+"/",
		&websocket.DialOptions{Subprotocols: []string{"hrana3"}, HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.CloseNow() })
	s := &wsStream{t: e.t, c: c}
	if r := s.send(`{"type":"hello"}`); !strings.Contains(r, "hello_ok") {
		e.t.Fatalf("hello: %s", r)
	}
	if r := s.request(`{"type":"open_stream","stream_id":1}`); !strings.Contains(r, "response_ok") {
		e.t.Fatalf("open_stream: %s", r)
	}
	return s
}

func (s *wsStream) send(msg string) string {
	s.t.Helper()
	ctx := context.Background()
	if err := s.c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		s.t.Fatal(err)
	}
	_, b, err := s.c.Read(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

func (s *wsStream) request(req string) string {
	s.id++
	return s.send(`{"type":"request","request_id":` + strconv.Itoa(s.id) + `,"request":` + req + `}`)
}

func (s *wsStream) exec(q string) string {
	return s.request(`{"type":"execute","stream_id":1,"stmt":{"sql":"` + q + `"}}`)
}

// TestAccessPolicy: an org's policy flips between Allow, ReadOnly and Deny,
// and every way to its databases' data follows, a WebSocket stream already
// open included; another org is untouched, and management calls stay open.
func TestAccessPolicy(t *testing.T) {
	pols := &policies{m: map[string]Policy{}}
	e := newEnv(t, "", WithAccessPolicy(pols.policy))
	e.setup()
	ctx := context.Background()
	_, orgs, dbs := e.clients()
	must(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "beta", Name: "Beta"})))
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "beta", Name: "app"})))
	e.exec("/db/beta/app", e.admin, "CREATE TABLE t(a)")
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "scratch"})))

	url := must(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"}))).Msg.Database.Url
	tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	open := func(u string) *sql.DB {
		conn, err := libsql.NewConnector(u, libsql.WithAuthToken(tok))
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(conn)
		t.Cleanup(func() { db.Close() })
		return db
	}
	httpDB, wsDB := open(url), open("ws"+strings.TrimPrefix(url, "http"))
	pinned, err := wsDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	clients := []struct {
		name string
		db   interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		}
	}{{"http", httpDB}, {"ws", wsDB}}

	for _, q := range []string{"CREATE TABLE t(a)", "INSERT INTO t VALUES (1)"} {
		if _, err := httpDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("allowed: %s: %v", q, err)
		}
	}
	if _, err := pinned.ExecContext(ctx, "INSERT INTO t VALUES (2)"); err != nil {
		t.Fatalf("allowed, ws: %v", err)
	}
	stream := e.wsStream("/db/acme/app", tok)
	if r := stream.exec("INSERT INTO t VALUES (3)"); !strings.Contains(r, "response_ok") {
		t.Fatalf("allowed, open stream: %s", r)
	}
	refused := func(err error, reason string) bool {
		return err != nil && strings.Contains(err.Error(), "Operation was blocked: "+reason)
	}
	count := func(db *sql.DB) (int, error) {
		var n int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n)
		return n, err
	}

	pols.set("acme", Policy{Access: ReadOnly, Reason: "the plan's write quota is used up"})
	for _, c := range clients {
		if n, err := count(c.db.(*sql.DB)); err != nil || n != 3 {
			t.Fatalf("read-only, %s read: %d %v", c.name, n, err)
		}
		for _, q := range []string{"INSERT INTO t VALUES (4)", "CREATE TABLE u(a)", "DROP TABLE t"} {
			if _, err := c.db.ExecContext(ctx, q); !refused(err, "the plan's write quota is used up") {
				t.Fatalf("read-only, %s %s: %v", c.name, q, err)
			}
		}
	}
	if _, err := pinned.ExecContext(ctx, "INSERT INTO t VALUES (4)"); !refused(err, "the plan's write quota is used up") {
		t.Fatalf("read-only, ws opened before: %v", err)
	}
	if r := stream.exec("INSERT INTO t VALUES (4)"); !strings.Contains(r, `"code":"BLOCKED"`) || !strings.Contains(r, "write quota") {
		t.Fatalf("read-only, open stream: %s", r)
	}
	if _, res := e.pipeline("/db/acme/app", e.admin, "SELECT count(*) FROM t", "INSERT INTO t VALUES (4)"); res[0].Type != "ok" || res[1].Error.Code != "BLOCKED" {
		t.Fatalf("read-only, a user's token (the console's): %+v", res)
	}
	e.exec("/db/beta/app", e.admin, "INSERT INTO t VALUES (1)")
	if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "more"})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "write quota") {
		t.Fatalf("read-only, create: %v", err)
	}
	if _, err := dbs.Restore(ctx, auth(e.admin, &v1.RestoreRequest{Org: "acme", Name: "app", InPlace: true})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("read-only, restore in place: %v", err)
	}
	export := func() error {
		s := must(dbs.Export(ctx, auth(e.admin, &v1.ExportRequest{Org: "acme", Name: "app"})))
		defer s.Close()
		for s.Receive() {
		}
		return s.Err()
	}
	if err := export(); err != nil {
		t.Fatalf("read-only, export: %v", err)
	}

	pols.set("acme", Policy{Access: Deny, Reason: "the subscription has lapsed"})
	for _, c := range clients {
		if _, err := count(c.db.(*sql.DB)); !refused(err, "the subscription has lapsed") {
			t.Fatalf("denied, %s read: %v", c.name, err)
		}
	}
	var n int
	if err := pinned.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); !refused(err, "the subscription has lapsed") {
		t.Fatalf("denied, ws opened before: %v", err)
	}
	if r := stream.exec("SELECT 1"); !strings.Contains(r, `"code":"BLOCKED"`) || !strings.Contains(r, "lapsed") {
		t.Fatalf("denied, open stream: %s", r)
	}
	if err := export(); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "lapsed") {
		t.Fatalf("denied, export: %v", err)
	}
	if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "beta", Name: "copy", Source: &v1.CreateDatabaseRequest_BranchOf{BranchOf: "app"}})); err != nil {
		t.Fatalf("another org's branch: %v", err)
	}
	e.exec("/db/beta/app", e.admin, "INSERT INTO t VALUES (2)")
	// What the org needs to fix its account stays open.
	must(dbs.List(ctx, auth(e.admin, &v1.ListDatabasesRequest{Org: "acme"})))
	must(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"})))
	must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"})))
	must(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme", Name: "app"})))
	must(orgs.ListMembers(ctx, auth(e.admin, &v1.ListMembersRequest{Org: "acme"})))
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "scratch"})))
	l := must(dbs.ListDeleted(ctx, auth(e.admin, &v1.ListDeletedRequest{Org: "acme"}))).Msg
	must(dbs.RestoreDeleted(ctx, auth(e.admin, &v1.RestoreDeletedRequest{Org: "acme", Id: l.Databases[0].Database.Id})))

	pols.set("acme", Policy{})
	if _, err := httpDB.ExecContext(ctx, "INSERT INTO t VALUES (4)"); err != nil {
		t.Fatalf("allowed again: %v", err)
	}
	if _, err := pinned.ExecContext(ctx, "INSERT INTO t VALUES (5)"); err != nil {
		t.Fatalf("allowed again, ws opened before: %v", err)
	}
	if n, err := count(httpDB); err != nil || n != 5 {
		t.Fatalf("allowed again, read: %d %v", n, err)
	}
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "more"})))
}

// Without WithAccessPolicy nothing asks one, and nothing is refused.
func TestNoAccessPolicy(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	e.exec("/db/acme/app", e.admin, "CREATE TABLE t(a)", "INSERT INTO t VALUES (1)")
	ctx := context.Background()
	o := must(e.s.Store().OrgBySlug(ctx, "acme"))
	d := must(e.s.Store().Database(ctx, o.ID, "app"))
	en, release, err := e.s.pool.get(d)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if en.policy != nil || en.blocks() != (hrana.Block{}) {
		t.Fatalf("policy %v, blocks %+v", en.policy, en.blocks())
	}
}
