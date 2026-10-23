package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/state"
	"github.com/samyfodil/musql/engine"
)

// node is one server of a test deployment, restartable behind a fixed URL.
type node struct {
	t       *testing.T
	name    string
	cfg     *Config
	ts      *httptest.Server
	h       atomic.Value // http.Handler
	s       *Server
	dbCalls atomic.Int64 // requests to /db/ it served
}

func (n *node) start() {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := New(ctx, n.cfg, nil)
	if err != nil {
		n.t.Fatalf("%s: %v", n.name, err)
	}
	n.s = s
	n.h.Store(s.Handler())
}

func (n *node) stop() {
	n.h.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "restarting", http.StatusServiceUnavailable)
	})))
	if err := n.s.Close(); err != nil {
		n.t.Errorf("%s: close: %v", n.name, err)
	}
}

// newCluster starts a primary in fra and a replica per other location, all
// peers of each other, sharing one public URL (the primary's).
func newCluster(t *testing.T, locs ...string) []*node {
	t.Helper()
	nodes := make([]*node, len(locs))
	for i := range locs {
		n := &node{t: t, name: fmt.Sprintf("node-%d", i+1)}
		n.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/db/") {
				n.dbCalls.Add(1)
			}
			n.h.Load().(http.Handler).ServeHTTP(w, r)
		}))
		nodes[i] = n
	}
	for i, n := range nodes {
		var peers strings.Builder
		for j, p := range nodes {
			if j != i {
				fmt.Fprintf(&peers, "  - {name: %s, location: %s, url: %q, secret: s3cret}\n", p.name, locs[j], p.ts.URL)
			}
		}
		cfg, err := ParseConfig([]byte(`
data_dir: ` + t.TempDir() + `
public_url: ` + nodes[0].ts.URL + `
primary: node-1
admin:
  email: admin@example.com
  password: "admin-password"
node:
  name: ` + n.name + `
  location: ` + locs[i] + `
peers:
` + peers.String()))
		if err != nil {
			t.Fatal(err)
		}
		n.cfg = cfg
	}
	for _, n := range nodes {
		n.start()
	}
	t.Cleanup(func() {
		for i := len(nodes) - 1; i >= 0; i-- {
			nodes[i].ts.Close()
			nodes[i].s.Close()
		}
	})
	return nodes
}

// env is a test env over a node, for the helpers that take one.
func (n *node) env(admin string) *env {
	return &env{t: n.t, s: n.s, ts: n.ts, admin: admin}
}

func (n *node) groups() cloudv1connect.GroupServiceClient {
	return cloudv1connect.NewGroupServiceClient(n.ts.Client(), n.ts.URL)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// caughtUp waits until r has applied every record p has logged for db.
func caughtUp(t *testing.T, p, r *node, org, db string) *state.Database {
	t.Helper()
	ctx := context.Background()
	o := must(p.s.store.OrgBySlug(ctx, org))
	d := must(p.s.store.Database(ctx, o.ID, db))
	h := must(p.s.history(d))
	if err := h.Sync(); err != nil {
		t.Fatal(err)
	}
	eventually(t, r.name+" catches up with "+db, func() bool {
		f := r.s.replicaFollower(d)
		return f != nil && f.h.Seq() == h.Seq()
	})
	return d
}

// sameCommitted checks that a replica's copy is the primary's committed pair.
func sameCommitted(t *testing.T, primary, replica string) {
	t.Helper()
	var end int64
	if err := engine.CommitState(primary, func(c engine.CommitInfo) error { end = c.DeltaEnd; return nil }); err != nil {
		t.Fatal(err)
	}
	a, b := must(os.ReadFile(primary)), must(os.ReadFile(replica))
	if !bytes.Equal(a, b) {
		t.Fatalf("segment files differ")
	}
	var pd, rd []byte
	if end > 0 {
		pd = must(os.ReadFile(primary + ".delta"))[:end]
	}
	rd, err := os.ReadFile(replica + ".delta")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !bytes.Equal(pd, rd) {
		t.Fatalf("deltas differ: %d and %d bytes", len(pd), len(rd))
	}
}

func values(res []result) string {
	var out []string
	for _, r := range res {
		for _, row := range r.Response.Result.Rows {
			for _, v := range row {
				out = append(out, fmt.Sprint(v.Value))
			}
		}
	}
	return strings.Join(out, ",")
}

// readLocal runs a read on the replica and checks it ran there.
func readLocal(t *testing.T, p, r *node, admin, path, q string) string {
	t.Helper()
	before := p.dbCalls.Load()
	out := values(r.env(admin).exec(path, admin, q))
	if p.dbCalls.Load() != before {
		t.Fatalf("%s: a read on %s went to the primary", q, r.name)
	}
	return out
}

func TestRegions(t *testing.T) {
	nodes := newCluster(t, "fra", "ams", "nyc")
	p, ams, nyc := nodes[0], nodes[1], nodes[2]
	ctx := context.Background()
	pe := p.env("")
	pe.admin = pe.tokenFor("admin@example.com")
	admin := pe.admin
	pe.setup()
	pe.exec("/db/acme/app", admin, `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`, `INSERT INTO t VALUES(1, 'one')`)

	// The management API answers on a replica too (from the primary).
	_, _, rdbs := ams.env(admin).clients()
	if list := must(rdbs.List(ctx, auth(admin, &v1.ListDatabasesRequest{Org: "acme"}))); len(list.Msg.Databases) != 1 {
		t.Fatalf("list on a replica: %v", list.Msg.Databases)
	}
	// A group's primary is the primary node's location.
	if _, err := p.groups().Create(ctx, auth(admin, &v1.CreateGroupRequest{Org: "acme", Name: "eu", Location: "ams"})); code(err) != 3 {
		t.Fatalf("a group written in ams: %v", err)
	}

	// Not placed in ams yet: a read there goes to the primary.
	before := p.dbCalls.Load()
	if v := values(ams.env(admin).exec("/db/acme/app", admin, `SELECT b FROM t`)); v != "one" {
		t.Fatalf("forwarded read: %q", v)
	}
	if p.dbCalls.Load() == before {
		t.Fatal("a database not placed in ams was served there")
	}

	must(p.groups().AddLocation(ctx, auth(admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "ams"})))
	d := caughtUp(t, p, ams, "acme", "app")
	sameCommitted(t, p.s.dbPath(d), ams.s.replicaPath(d))
	if v := readLocal(t, p, ams, admin, "/db/acme/app", `SELECT b FROM t ORDER BY a`); v != "one" {
		t.Fatalf("replica read: %q", v)
	}
	if nyc.s.replicaFollower(d) != nil {
		t.Fatal("nyc follows a database not placed there")
	}

	// A write through the replica lands on the primary; the replica reads
	// it once applied.
	ams.env(admin).exec("/db/acme/app", admin, `INSERT INTO t VALUES(2, 'two')`)
	caughtUp(t, p, ams, "acme", "app")
	if v := readLocal(t, p, ams, admin, "/db/acme/app", `SELECT b FROM t ORDER BY a`); v != "one,two" {
		t.Fatalf("after a forwarded write: %q", v)
	}

	// Reads on the replica while the primary writes: whole commits, in order.
	var stop atomic.Bool
	var wg sync.WaitGroup
	var bad atomic.Value
	wg.Go(func() {
		last := 0
		for !stop.Load() {
			st, res := ams.env(admin).pipeline("/db/acme/app", admin, `SELECT count(*), max(a) FROM t`)
			if st != 200 || len(res) != 1 || res[0].Type != "ok" {
				bad.Store(fmt.Sprintf("read failed: %d %v", st, res))
				return
			}
			row := res[0].Response.Result.Rows[0]
			n, mx := fmt.Sprint(row[0].Value), fmt.Sprint(row[1].Value)
			var ni int
			fmt.Sscan(n, &ni)
			if n != mx || ni < last {
				bad.Store(fmt.Sprintf("torn or backwards: count %s max %s after %d", n, mx, last))
				return
			}
			last = ni
		}
	})
	for i := 3; i <= 40; i++ {
		pe.exec("/db/acme/app", admin, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'x')`, i))
		if i%15 == 0 {
			pe.exec("/db/acme/app", admin, `VACUUM`)
		}
	}
	caughtUp(t, p, ams, "acme", "app")
	stop.Store(true)
	wg.Wait()
	if v := bad.Load(); v != nil {
		t.Fatal(v)
	}
	sameCommitted(t, p.s.dbPath(d), ams.s.replicaPath(d))

	// In-place restore: a new base of the same history.
	mark := time.Now()
	time.Sleep(5 * time.Millisecond)
	pe.exec("/db/acme/app", admin, `DELETE FROM t WHERE a > 2`)
	_, _, pdbs := pe.clients()
	must(pdbs.Restore(ctx, auth(admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(mark), InPlace: true})))
	d = caughtUp(t, p, ams, "acme", "app")
	sameCommitted(t, p.s.dbPath(d), ams.s.replicaPath(d))
	if v := readLocal(t, p, ams, admin, "/db/acme/app", `SELECT count(*) FROM t`); v != "40" {
		t.Fatalf("after an in-place restore: %q", v)
	}

	// A branch is a new database of the group: placed in ams as well.
	must(pdbs.Create(ctx, auth(admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app2", Source: &v1.CreateDatabaseRequest_BranchOf{BranchOf: "app"}})))
	d2 := caughtUp(t, p, ams, "acme", "app2")
	if v := readLocal(t, p, ams, admin, "/db/acme/app2", `SELECT count(*) FROM t`); v != "40" {
		t.Fatalf("branch on the replica: %q", v)
	}
	must(pdbs.Delete(ctx, auth(admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app2"})))
	eventually(t, "the deleted branch leaves ams", func() bool {
		_, err := os.Stat(ams.s.replicaPath(d2))
		return os.IsNotExist(err) && ams.s.replicaFollower(d2) == nil
	})

	// The primary restarts; the replica reconnects and goes on.
	p.stop()
	p.start()
	pe = p.env(admin)
	pe.exec("/db/acme/app", admin, `INSERT INTO t VALUES(100, 'after restart')`)
	caughtUp(t, p, ams, "acme", "app")
	if v := readLocal(t, p, ams, admin, "/db/acme/app", `SELECT b FROM t WHERE a = 100`); v != "after restart" {
		t.Fatalf("after the primary restarted: %q", v)
	}

	// The replica restarts and goes on from its own log.
	seq := ams.s.replicaFollower(d).h.Seq()
	ams.stop()
	pe.exec("/db/acme/app", admin, `INSERT INTO t VALUES(101, 'while away')`)
	ams.start()
	caughtUp(t, p, ams, "acme", "app")
	if got := ams.s.replicaFollower(d).h.Seq(); got <= seq {
		t.Fatalf("seq %d after restarting at %d", got, seq)
	}
	sameCommitted(t, p.s.dbPath(d), ams.s.replicaPath(d))

	// A second replica location, then removing the first.
	must(p.groups().AddLocation(ctx, auth(admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "nyc"})))
	caughtUp(t, p, nyc, "acme", "app")
	if v := readLocal(t, p, nyc, admin, "/db/acme/app", `SELECT count(*) FROM t`); v != "42" {
		t.Fatalf("nyc: %q", v)
	}
	must(p.groups().RemoveLocation(ctx, auth(admin, &v1.RemoveLocationRequest{Org: "acme", Group: "default", Location: "ams"})))
	eventually(t, "ams drops app", func() bool {
		_, err := os.Stat(ams.s.replicaPath(d))
		return os.IsNotExist(err) && ams.s.replicaFollower(d) == nil
	})
	before = p.dbCalls.Load()
	ams.env(admin).exec("/db/acme/app", admin, `SELECT count(*) FROM t`)
	if p.dbCalls.Load() == before {
		t.Fatal("ams served a database removed from it")
	}
}

// TestRegionsTokens: database tokens issued by the primary work on a
// replica, read-only ones stay read-only, and invalidation reaches it.
func TestRegionsTokens(t *testing.T) {
	nodes := newCluster(t, "fra", "ams")
	p, ams := nodes[0], nodes[1]
	ctx := context.Background()
	pe := p.env("")
	pe.admin = pe.tokenFor("admin@example.com")
	admin := pe.admin
	pe.setup()
	pe.exec("/db/acme/app", admin, `CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`)
	must(p.groups().AddLocation(ctx, auth(admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "ams"})))
	_, _, dbs := pe.clients()
	ro := must(dbs.CreateToken(ctx, auth(admin, &v1.CreateTokenRequest{Org: "acme", Name: "app", Access: v1.Access_ACCESS_READ_ONLY}))).Msg.Token
	caughtUp(t, p, ams, "acme", "app")
	eventually(t, "the token's state reaches ams", func() bool {
		st, _ := ams.env(admin).pipeline("/db/acme/app", ro, `SELECT 1`)
		return st == 200
	})
	if v := readLocal(t, p, ams, ro, "/db/acme/app", `SELECT a FROM t`); v != "1" {
		t.Fatalf("read-only token: %q", v)
	}
	if _, res := ams.env(admin).pipeline("/db/acme/app", ro, `INSERT INTO t VALUES(2)`); len(res) != 1 || res[0].Type == "ok" {
		t.Fatalf("a read-only token wrote through a replica: %v", res)
	}
	must(dbs.InvalidateTokens(ctx, auth(admin, &v1.InvalidateTokensRequest{Org: "acme", Name: "app"})))
	eventually(t, "invalidation reaches ams", func() bool {
		st, _ := ams.env(admin).pipeline("/db/acme/app", ro, `SELECT 1`)
		return st == http.StatusUnauthorized
	})
}
