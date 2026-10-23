package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samyfodil/musql/cloud/cluster"
	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

// clusterNode is one node of an in-process cluster: its server behind an
// httptest listener that survives a restart.
type clusterNode struct {
	*env
	cfg *Config
	mu  sync.Mutex
	h   http.Handler
}

var locations = []string{"fra", "iad", "sea"}

// freePort is a TCP port free on 127.0.0.1 a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// newCluster starts n nodes on localhost, each the others' peer, sharing one
// public URL as a deployment behind a load balancer would.
func newCluster(t *testing.T, n int) []*clusterNode {
	t.Helper()
	type ident struct {
		dir, id string
		port    int
	}
	ids := make([]ident, n)
	for i := range ids {
		dir := t.TempDir()
		key, err := cluster.LoadKey(filepath.Join(dir, "node.key"))
		if err != nil {
			t.Fatal(err)
		}
		id, err := cluster.PeerID(key)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = ident{dir, id, freePort(t)}
	}
	nodes := make([]*clusterNode, n)
	for i := range nodes {
		var peers strings.Builder
		for j, p := range ids {
			if j != i {
				fmt.Fprintf(&peers, "  - {name: node-%d, location: %s, peer_id: %s, addrs: [/ip4/127.0.0.1/tcp/%d]}\n", j, locations[j], p.id, p.port)
			}
		}
		cfg, err := ParseConfig([]byte(fmt.Sprintf(`
data_dir: %s
public_url: http://cloud.test
admin:
  email: admin@example.com
  password: "admin-password"
node:
  name: node-%d
  location: %s
  p2p_listen: [/ip4/127.0.0.1/tcp/%d]
peers:
%s`, ids[i].dir, i, locations[i], ids[i].port, peers.String())))
		if err != nil {
			t.Fatal(err)
		}
		cfg.syncInterval = 100 * time.Millisecond
		c := &clusterNode{cfg: cfg}
		c.env = &env{t: t}
		c.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.mu.Lock()
			h := c.h
			c.mu.Unlock()
			h.ServeHTTP(w, r)
		}))
		c.start()
		nodes[i] = c
		t.Cleanup(func() {
			c.ts.Close()
			c.s.Close()
		})
	}
	return nodes
}

func (c *clusterNode) start() {
	c.t.Helper()
	s, err := New(context.Background(), c.cfg, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.s, c.h = s, s.Handler()
	c.mu.Unlock()
	c.admin = c.tokenFor("admin@example.com")
}

func (c *clusterNode) restart() {
	c.t.Helper()
	if err := c.s.Close(); err != nil {
		c.t.Fatal(err)
	}
	c.start()
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// count is the rows of t in db over Hrana on node c, -1 on any error.
func (c *clusterNode) count(db, tbl string) int {
	status, res := c.pipeline("/db/acme/"+db, c.admin, "SELECT count(*) FROM "+tbl)
	if status != 200 || len(res) != 1 || res[0].Type != "ok" || len(res[0].Response.Result.Rows) != 1 {
		return -1
	}
	var n int
	fmt.Sscan(fmt.Sprint(res[0].Response.Result.Rows[0][0].Value), &n)
	return n
}

func (c *clusterNode) groups() cloudv1connect.GroupServiceClient {
	return cloudv1connect.NewGroupServiceClient(c.ts.Client(), c.ts.URL)
}

// placed reports whether every node in nodes holds db open.
func placed(nodes []*clusterNode, db string) bool {
	for _, n := range nodes {
		d, err := n.s.Store().Database(context.Background(), orgID(n), db)
		if err != nil || n.s.crdtHandle(d.ID) == nil {
			return false
		}
	}
	return true
}

func orgID(n *clusterNode) string {
	o, err := n.s.Store().OrgBySlug(context.Background(), "acme")
	if err != nil {
		return ""
	}
	return o.ID
}

// acmeEverywhere makes org acme on node 0 with its default group placed at
// every node's location, and a crdt database app in it, open on every node.
func acmeEverywhere(t *testing.T, nodes []*clusterNode) {
	ctx := context.Background()
	_, orgs, dbs := nodes[0].clients()
	must(orgs.Create(ctx, auth(nodes[0].admin, &v1.CreateOrgRequest{Slug: "acme", Name: "Acme"})))
	for _, loc := range locations[1:len(nodes)] {
		must(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: loc})))
	}
	d := must(dbs.Create(ctx, auth(nodes[0].admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app"})))
	if d.Msg.Database.Type != v1.DatabaseType_DATABASE_TYPE_CRDT {
		t.Fatalf("a cluster's database is %v", d.Msg.Database.Type)
	}
	eventually(t, "app open on every node", func() bool { return placed(nodes, "app") })
}

func TestClusterConcurrentWritesConverge(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhere(t, nodes)
	nodes[0].exec("/db/acme/app", nodes[0].admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, node TEXT, v INTEGER)")
	eventually(t, "the table on every node", func() bool {
		for _, n := range nodes {
			if n.count("app", "t") != 0 {
				return false
			}
		}
		return true
	})
	const each = 20
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Go(func() {
			for k := range each {
				n.exec("/db/acme/app", n.admin, fmt.Sprintf("INSERT INTO t(node, v) VALUES ('node-%d', %d)", i, k))
			}
		})
	}
	wg.Wait()
	eventually(t, "every write on every node", func() bool {
		for _, n := range nodes {
			if n.count("app", "t") != each*len(nodes) {
				return false
			}
		}
		return true
	})

	// A node that was down catches up on restart.
	nodes[1].s.Close()
	nodes[0].exec("/db/acme/app", nodes[0].admin, "INSERT INTO t(node, v) VALUES ('while-down', 0)")
	nodes[1].start()
	eventually(t, "the restarted node to catch up", func() bool { return nodes[1].count("app", "t") == each*len(nodes)+1 })
}

func TestClusterPlacement(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhere(t, nodes)
	ctx := context.Background()
	d, err := nodes[2].s.Store().Database(ctx, orgID(nodes[2]), "app")
	if err != nil {
		t.Fatal(err)
	}
	path := nodes[2].s.dbPath(d)
	must(nodes[1].groups().RemoveLocation(ctx, auth(nodes[1].admin, &v1.RemoveLocationRequest{Org: "acme", Group: "default", Location: "sea"})))
	eventually(t, "sea to close and remove app", func() bool {
		_, err := os.Stat(path)
		return nodes[2].s.crdtHandle(d.ID) == nil && os.IsNotExist(err)
	})
	if status, _ := nodes[2].pipeline("/db/acme/app", nodes[2].admin, "SELECT 1"); status != http.StatusNotFound {
		t.Fatalf("a database not placed here: HTTP %d, want 404", status)
	}
	nodes[0].exec("/db/acme/app", nodes[0].admin, "CREATE TABLE back(x)", "INSERT INTO back VALUES (1)")
	must(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "sea"})))
	eventually(t, "sea to hold app again, rows included", func() bool { return nodes[2].count("app", "back") == 1 })

	// Deleting on one node removes it everywhere.
	_, _, dbs := nodes[1].clients()
	must(dbs.Delete(ctx, auth(nodes[1].admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})))
	eventually(t, "app gone from every node", func() bool {
		for _, n := range nodes {
			if n.s.crdtHandle(d.ID) != nil {
				return false
			}
		}
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	})
}

func TestClusterDeclinesWhatCRDTCannotReplicate(t *testing.T) {
	nodes := newCluster(t, 2)
	acmeEverywhere(t, nodes)
	n := nodes[0]
	n.exec("/db/acme/app", n.admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, v)")
	for _, stmt := range []string{
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END",
		"CREATE VIRTUAL TABLE f USING fts5(body)",
		"CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID",
	} {
		status, res := n.pipeline("/db/acme/app", n.admin, stmt)
		if status != 200 || len(res) != 1 || res[0].Type != "error" {
			t.Fatalf("%s: HTTP %d %+v, want a statement error", stmt, status, res)
		}
	}
	// And nothing of them reached the other node.
	eventually(t, "t on node 1", func() bool { return nodes[1].count("app", "t") == 0 })
	status, res := nodes[1].pipeline("/db/acme/app", nodes[1].admin, "SELECT count(*) FROM sqlite_schema WHERE name IN ('tr', 'f', 'w')")
	if status != 200 || fmt.Sprint(res[0].Response.Result.Rows[0][0].Value) != "0" {
		t.Fatalf("a declined object reached a peer: %d %+v", status, res)
	}

	ctx := context.Background()
	_, _, dbs := n.clients()
	for _, typ := range []v1.DatabaseType{v1.DatabaseType_DATABASE_TYPE_RAFT, v1.DatabaseType_DATABASE_TYPE_PLAIN} {
		_, err := dbs.Create(ctx, auth(n.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "x", Type: typ}))
		if err == nil || !strings.Contains(err.Error(), "not yet supported") && !strings.Contains(err.Error(), "standalone") {
			t.Fatalf("type %v: %v", typ, err)
		}
	}
	if _, err := dbs.Restore(ctx, auth(n.admin, &v1.RestoreRequest{Org: "acme", Name: "app"})); err == nil || !strings.Contains(err.Error(), "crdt") {
		t.Fatalf("restore of a crdt database: %v", err)
	}
}

func TestClusterEveryNodeServesIdentity(t *testing.T) {
	nodes := newCluster(t, 3)
	ctx := context.Background()
	// A user made on one node, a token signed on another, the API called on a
	// third: no node forwards anything.
	users, _, _ := nodes[0].clients()
	must(users.Create(ctx, auth(nodes[0].admin, &v1.CreateUserRequest{Email: "bob@example.com", Name: "Bob", Password: "bob-password"})))
	eventually(t, "bob on node 1", func() bool {
		_, err := nodes[1].s.Store().UserByEmail(ctx, "bob@example.com")
		return err == nil
	})
	bob := nodes[1].tokenFor("bob@example.com")
	for _, n := range nodes {
		eventually(t, "bob's token to verify on "+n.cfg.Node.Name, func() bool {
			ac := cloudv1connect.NewAuthServiceClient(n.ts.Client(), n.ts.URL)
			r, err := ac.WhoAmI(ctx, auth(bob, &v1.WhoAmIRequest{}))
			return err == nil && r.Msg.User.Email == "bob@example.com"
		})
		for _, p := range []string{"/.well-known/openid-configuration", "/oidc/jwks", "/login"} {
			resp, err := n.ts.Client().Get(n.ts.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: HTTP %d", n.cfg.Node.Name, p, resp.StatusCode)
			}
		}
	}
	// Every node's key is published by every node.
	eventually(t, "every key in every JWKS", func() bool {
		for _, n := range nodes {
			resp, err := n.ts.Client().Get(n.ts.URL + "/oidc/jwks")
			if err != nil {
				return false
			}
			var b strings.Builder
			buf := make([]byte, 4096)
			for {
				k, err := resp.Body.Read(buf)
				b.Write(buf[:k])
				if err != nil {
					break
				}
			}
			resp.Body.Close()
			for _, m := range nodes {
				if !strings.Contains(b.String(), `"`+m.cfg.Node.Name+".") {
					return false
				}
			}
		}
		return true
	})
}

func TestStandaloneIsPlain(t *testing.T) {
	e := newEnv(t, "")
	if e.s.cl != nil || e.s.PeerID() != "" {
		t.Fatal("a node without peers started libp2p")
	}
	if _, err := os.Stat(filepath.Join(e.s.cfg.DataDir, "node.key")); !os.IsNotExist(err) {
		t.Fatalf("a standalone node made a node key: %v", err)
	}
	ctx := context.Background()
	_, orgs, dbs := e.clients()
	must(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "acme", Name: "Acme"})))
	d := must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app"})))
	if d.Msg.Database.Type != v1.DatabaseType_DATABASE_TYPE_PLAIN {
		t.Fatalf("a standalone database is %v", d.Msg.Database.Type)
	}
	e.exec("/db/acme/app", e.admin,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END",
		"CREATE VIRTUAL TABLE f USING fts5(body)",
		"CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID",
		"INSERT INTO t(v) VALUES (1)")
	for _, typ := range []v1.DatabaseType{v1.DatabaseType_DATABASE_TYPE_CRDT, v1.DatabaseType_DATABASE_TYPE_RAFT} {
		if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "x", Type: typ})); err == nil {
			t.Fatalf("a standalone node accepted type %v", typ)
		}
	}
}
