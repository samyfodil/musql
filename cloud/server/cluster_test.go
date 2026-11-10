package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/replication"

	"github.com/samyfodil/musql/cloud/cluster"
	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/state"
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

// freePorts are n TCP ports free on 127.0.0.1 a moment ago, all different:
// each is held until the last is picked. They are below every platform's
// ephemeral range (Linux's from 32768, macOS's and Windows' from 49152), so
// between this and a node's listen the kernel cannot hand one to an httptest
// server or an outgoing dial, as a port it picked itself could be.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	for try := 0; len(ports) < n; try++ {
		if try > 1000 {
			t.Fatal("no free port below the ephemeral range")
		}
		p := 20000 + rand.IntN(12000)
		if slices.Contains(ports, p) {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		defer l.Close()
		ports = append(ports, p)
	}
	return ports
}

// newCluster starts n nodes on localhost, each the others' peer, sharing one
// public URL as a deployment behind a load balancer would.
func newCluster(t *testing.T, n int) []*clusterNode {
	t.Helper()
	cfgs := clusterConfigs(t, n, "")
	// A cluster is initialised once, on one node, and the others get the
	// admin from it: Init on a node the admin is reaching from a peer fails.
	if err := Init(context.Background(), cfgs[0], "admin@example.com", "admin-password"); err != nil {
		t.Fatal(err)
	}
	return startCluster(t, cfgs)
}

// clusterConfigs are the configs of n nodes on localhost, each the others'
// peer; node 0's data directory is dir0 when given.
func clusterConfigs(t *testing.T, n int, dir0 string) []*Config {
	t.Helper()
	startBudget(t)
	type ident struct {
		dir, id string
		port    int
	}
	ids, ports := make([]ident, n), freePorts(t, n)
	for i := range ids {
		dir := t.TempDir()
		if i == 0 && dir0 != "" {
			dir = dir0
		}
		key, err := cluster.LoadKey(filepath.Join(dir, "node.key"))
		if err != nil {
			t.Fatal(err)
		}
		id, err := cluster.PeerID(key)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = ident{dir, id, ports[i]}
	}
	cfgs := make([]*Config, n)
	for i := range cfgs {
		var peers strings.Builder
		for j, p := range ids {
			if j != i {
				fmt.Fprintf(&peers, "  - {name: node-%d, location: %s, peer_id: %s, addrs: [/ip4/127.0.0.1/tcp/%d]}\n", j, locations[j], p.id, p.port)
			}
		}
		cfg, err := ParseConfig([]byte(fmt.Sprintf(`
data_dir: %s
public_url: http://cloud.test
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
		// Calm enough that a loaded machine does not churn leaders mid-test.
		cfg.election = cluster.ElectionTiming{Heartbeat: time.Second, Election: time.Second, Lease: 500 * time.Millisecond}
		cfgs[i] = cfg
	}
	return cfgs
}

// startCluster starts a node per config, then waits for each to serve.
func startCluster(t *testing.T, cfgs []*Config) []*clusterNode {
	t.Helper()
	nodes := make([]*clusterNode, len(cfgs))
	for i, cfg := range cfgs {
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
	// A cluster writes once a majority of its nodes elected a master, so each
	// node signs the admin in only once they all run.
	for _, c := range nodes {
		c.ready()
	}
	return nodes
}

// start starts the node's server.
func (c *clusterNode) start() {
	c.t.Helper()
	s, err := New(context.Background(), c.cfg, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.s, c.h = s, s.Handler()
	c.mu.Unlock()
}

// ready waits for the admin to reach the node and signs it in there.
func (c *clusterNode) ready() {
	c.t.Helper()
	s := c.s
	// The admin, read through the master, and this node's own copy of the
	// state synced: a test that stops the master may only count on what
	// reached the others.
	// Signed from that read: a second one can land on a stale copy mid-election.
	var admin *state.User
	eventually(c.t, "the admin on "+c.cfg.Node.Name, func() bool {
		u, err := s.Store().UserByEmail(context.Background(), "admin@example.com")
		if err != nil || s.cl != nil && s.cl.stateSyncing.Load() {
			return false
		}
		admin = u
		return true
	})
	c.admin = c.sign(admin)
}

func (c *clusterNode) restart() {
	c.t.Helper()
	if err := c.s.Close(); err != nil {
		c.t.Fatal(err)
	}
	c.start()
	c.ready()
}

// clusterBudget is how long one cluster test may run, -race included; a
// test's waits share it, and it ends before the package's -timeout does, so a
// stuck wait fails with what it waited for instead of a goroutine dump.
const clusterBudget = 4 * time.Minute

var budgets sync.Map // *testing.T -> time.Time

func startBudget(t *testing.T) {
	end := time.Now().Add(clusterBudget)
	if d, ok := t.Deadline(); ok && d.Add(-10*time.Second).Before(end) {
		end = d.Add(-10 * time.Second)
	}
	budgets.Store(t, end)
	t.Cleanup(func() { budgets.Delete(t) })
}

// eventually polls fn until it holds, failing with what it waited for once
// the test's budget is spent.
func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	end := time.Now().Add(time.Minute)
	if v, ok := budgets.Load(t); ok {
		end = v.(time.Time)
	}
	start := time.Now()
	for !fn() {
		if time.Now().After(end) {
			t.Fatalf("gave up after %v waiting for %s", time.Since(start).Round(time.Millisecond), what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// need(f())(t) is f's value, failing t on f's error.
func need[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
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

// placed reports whether every node in nodes holds db open, its first sync
// done: a regular database's reads go to its master, so they do not show it.
func placed(nodes []*clusterNode, db string) bool {
	for _, n := range nodes {
		d, err := n.s.Store().Database(context.Background(), orgID(n), db)
		if err != nil || n.s.replHandle(d.ID) == nil || n.s.replSyncing(d.ID) {
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
	acmeEverywhereAs(t, nodes, v1.DatabaseType_DATABASE_TYPE_CRDT)
}

// acmeEverywhereAs is acmeEverywhere with app of type typ.
func acmeEverywhereAs(t *testing.T, nodes []*clusterNode, typ v1.DatabaseType) {
	acmeEverywhereWith(t, nodes, typ, v1.Durability_DURABILITY_UNSPECIFIED)
}

// acmeEverywhereWith is acmeEverywhereAs with app's durability du asked for.
func acmeEverywhereWith(t *testing.T, nodes []*clusterNode, typ v1.DatabaseType, du v1.Durability) {
	ctx := context.Background()
	_, orgs, dbs := nodes[0].clients()
	need(orgs.Create(ctx, auth(nodes[0].admin, &v1.CreateOrgRequest{Slug: "acme", Name: "Acme"})))(t)
	for _, loc := range locations[1:len(nodes)] {
		need(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: loc})))(t)
	}
	d := need(dbs.Create(ctx, auth(nodes[0].admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app", Type: typ, Durability: du})))(t)
	want := du
	if want == v1.Durability_DURABILITY_UNSPECIFIED && typ != v1.DatabaseType_DATABASE_TYPE_CRDT {
		want = v1.Durability_DURABILITY_DURABLE
	}
	if d.Msg.Database.Type != typ || d.Msg.Database.Durability != want {
		t.Fatalf("app is %v %v, not %v %v", d.Msg.Database.Type, d.Msg.Database.Durability, typ, want)
	}
	eventually(t, "app open on every node", func() bool { return placed(nodes, "app") })
	if typ != v1.DatabaseType_DATABASE_TYPE_CRDT {
		eventually(t, "app's writer", func() bool { return masterOf(nodes, "app") >= 0 })
	}
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
	busy := watchBusy(t)
	const each = 100
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Go(func() {
			for k := range each {
				stmt := fmt.Sprintf("INSERT INTO t(node, v) VALUES ('node-%d', %d)", i, k)
				if status, res := n.pipeline("/db/acme/app", n.admin, stmt); status != 200 || len(res) != 1 || res[0].Type != "ok" {
					t.Errorf("%s on node-%d: HTTP %d %+v", stmt, i, status, res)
					return
				}
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	eventually(t, "every write on every node", func() bool {
		for _, n := range nodes {
			if n.count("app", "t") != each*len(nodes) {
				return false
			}
		}
		return true
	})

	// Every op was applied when it arrived: none waited for anti-entropy after
	// losing its commit to a local write.
	if n := busy.n.Load(); n > 0 {
		t.Fatalf("%d replication log lines report SQLITE_BUSY", n)
	}

	// A node that was down catches up on restart.
	nodes[1].s.Close()
	nodes[0].exec("/db/acme/app", nodes[0].admin, "INSERT INTO t(node, v) VALUES ('while-down', 0)")
	nodes[1].start()
	eventually(t, "the restarted node to catch up", func() bool { return nodes[1].count("app", "t") == each*len(nodes)+1 })
}

// watchBusy counts the replication log lines that report SQLITE_BUSY: an op
// whose apply lost to a local commit, left for the next anti-entropy round.
func watchBusy(t *testing.T) *busyLog {
	b := &busyLog{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b
}

type busyLog struct{ n atomic.Int64 }

func (b *busyLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("SQLITE_BUSY")) {
		b.n.Add(1)
	}
	return os.Stderr.Write(p)
}

// A node newly placed serves a database only once it holds all of it: until
// its first sync with the peers completes it answers 503 (a crdt database) or
// its master answers (a regular one), never a part. It takes the database as
// a peer's image, not op by op.
func TestClusterNewPlacementServesAllOrNothing(t *testing.T) {
	for _, typ := range []v1.DatabaseType{v1.DatabaseType_DATABASE_TYPE_CRDT, v1.DatabaseType_DATABASE_TYPE_PLAIN} {
		t.Run(typ.String(), func(t *testing.T) { newPlacementAllOrNothing(t, typ) })
	}
}

func newPlacementAllOrNothing(t *testing.T, typ v1.DatabaseType) {
	nodes := newCluster(t, 3)
	ctx := context.Background()
	_, orgs, dbs := nodes[0].clients()
	need(orgs.Create(ctx, auth(nodes[0].admin, &v1.CreateOrgRequest{Slug: "acme", Name: "Acme"})))(t)
	need(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "iad"})))(t)
	need(dbs.Create(ctx, auth(nodes[0].admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app", Type: typ})))(t)
	eventually(t, "app open on fra and iad", func() bool { return placed(nodes[:2], "app") })
	const rows = 2000 // more ops than a new node pulls one by one
	nodes[0].exec("/db/acme/app", nodes[0].admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)",
		fmt.Sprintf("WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < %d) INSERT INTO t(v) SELECT i FROM c", rows))
	eventually(t, "the rows on iad", func() bool { return nodes[1].count("app", "t") == rows })

	need(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "sea"})))(t)
	eventually(t, "sea to serve app", func() bool {
		status, res := nodes[2].pipeline("/db/acme/app", nodes[2].admin, "SELECT count(*) FROM t")
		switch {
		case status == http.StatusNotFound || status == http.StatusServiceUnavailable:
			return false // not placed yet, or still syncing
		case status != 200 || len(res) != 1:
			t.Fatalf("sea: HTTP %d %+v", status, res)
		case res[0].Type != "ok":
			t.Fatalf("sea served app before its first sync: %s", res[0].Error.Message)
		}
		if n := fmt.Sprint(res[0].Response.Result.Rows[0][0].Value); n != fmt.Sprint(rows) {
			t.Fatalf("sea served %s of %d rows before its first sync completed", n, rows)
		}
		return true
	})
	d, err := nodes[2].s.Store().Database(ctx, orgID(nodes[2]), "app")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "sea's own copy of app", func() bool { return nodes[2].s.replHandle(d.ID) != nil && !nodes[2].s.replSyncing(d.ID) })
	if !replication.ImageInstalled(nodes[2].s.replHandle(d.ID)) {
		t.Fatal("sea took app op by op, not as an image")
	}
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
	need(nodes[1].groups().RemoveLocation(ctx, auth(nodes[1].admin, &v1.RemoveLocationRequest{Org: "acme", Group: "default", Location: "sea"})))(t)
	eventually(t, "sea to close and remove app", func() bool {
		_, err := os.Stat(path)
		return nodes[2].s.replHandle(d.ID) == nil && os.IsNotExist(err)
	})
	if status, _ := nodes[2].pipeline("/db/acme/app", nodes[2].admin, "SELECT 1"); status != http.StatusNotFound {
		t.Fatalf("a database not placed here: HTTP %d, want 404", status)
	}
	_, _, dbs2 := nodes[2].clients()
	if _, err := dbs2.Create(ctx, auth(nodes[2].admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "copy", Source: &v1.CreateDatabaseRequest_BranchOf{BranchOf: "app"}})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("a branch of a database not placed here: %v", err)
	}
	up := nodes[2].upload("acme", nodes[0].export("acme", "app", v1.FileFormat_FILE_FORMAT_MUSQL))
	if _, err := dbs2.Create(ctx, auth(nodes[2].admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "uploaded", Source: &v1.CreateDatabaseRequest_Upload{Upload: up.Upload}})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("an upload into a database not placed here: %v", err)
	}
	if r := need(dbs2.Create(ctx, auth(nodes[2].admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "elsewhere"})))(t); r.Msg.Database.Name != "elsewhere" {
		t.Fatalf("a database recorded for its group's nodes: %v", r.Msg)
	}
	for name, err := range map[string]error{
		"its snapshots":   errOf(dbs2.ListSnapshots(ctx, auth(nodes[2].admin, &v1.ListSnapshotsRequest{Org: "acme", Name: "app"}))),
		"a snapshot":      errOf(dbs2.CreateSnapshot(ctx, auth(nodes[2].admin, &v1.CreateSnapshotRequest{Org: "acme", Name: "app"}))),
		"a restore of it": errOf(dbs2.Restore(ctx, auth(nodes[2].admin, &v1.RestoreRequest{Org: "acme", Name: "app", NewName: "back"}))),
	} {
		if code(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s on a node not holding it: %v", name, err)
		}
	}
	nodes[0].exec("/db/acme/app", nodes[0].admin, "CREATE TABLE back(x)", "INSERT INTO back VALUES (1)")
	need(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: "sea"})))(t)
	eventually(t, "sea to hold app again, rows included", func() bool { return nodes[2].count("app", "back") == 1 })

	// Deleting on one node removes it everywhere.
	_, _, dbs := nodes[1].clients()
	need(dbs.Delete(ctx, auth(nodes[1].admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})))(t)
	eventually(t, "app gone from every node", func() bool {
		for _, n := range nodes {
			if n.s.replHandle(d.ID) != nil {
				return false
			}
		}
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	})
}

// TestClusterDeclinesWhatCannotReplicate: replication refuses a virtual table
// in a crdt database and a regular one alike, and a trigger and a WITHOUT
// ROWID table in a crdt one; a raft database is a regular one.
func TestClusterDeclinesWhatCannotReplicate(t *testing.T) {
	for _, typ := range []v1.DatabaseType{v1.DatabaseType_DATABASE_TYPE_CRDT, v1.DatabaseType_DATABASE_TYPE_PLAIN} {
		t.Run(typ.String(), func(t *testing.T) { declinesWhatCannotReplicate(t, typ) })
	}
}

func declinesWhatCannotReplicate(t *testing.T, typ v1.DatabaseType) {
	nodes := newCluster(t, 2)
	acmeEverywhereAs(t, nodes, typ)
	n := nodes[0]
	n.exec("/db/acme/app", n.admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, v)")
	refused := []struct{ stmt, why string }{{"CREATE VIRTUAL TABLE f USING fts5(body)", "virtual tables are not replicated"}}
	if typ == v1.DatabaseType_DATABASE_TYPE_CRDT {
		refused = append(refused,
			struct{ stmt, why string }{"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END", "triggers are not replicated"},
			struct{ stmt, why string }{"CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID", "WITHOUT ROWID is not replicated"})
	}
	for _, c := range refused {
		status, res := n.pipeline("/db/acme/app", n.admin, c.stmt)
		if status != 200 || len(res) != 1 || res[0].Type != "error" || !strings.Contains(res[0].Error.Message, c.why) {
			t.Fatalf("%s: HTTP %d %+v, want a statement error saying %q", c.stmt, status, res, c.why)
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
	r := need(dbs.Create(ctx, auth(n.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "r", Type: v1.DatabaseType_DATABASE_TYPE_RAFT})))(t)
	if r.Msg.Database.Type != v1.DatabaseType_DATABASE_TYPE_PLAIN {
		t.Fatalf("a raft database is %v, not a regular one", r.Msg.Database.Type)
	}
}

func TestClusterEveryNodeServesIdentity(t *testing.T) {
	nodes := newCluster(t, 3)
	ctx := context.Background()
	// A user made on one node, a token signed on another, the API called on a
	// third: no node forwards anything.
	users, _, _ := nodes[0].clients()
	need(users.Create(ctx, auth(nodes[0].admin, &v1.CreateUserRequest{Email: "bob@example.com", Name: "Bob", Password: "bob-password"})))(t)
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
	for _, n := range nodes {
		if n.s.PeerID() == "" || len(n.s.P2PAddrs()) == 0 {
			t.Fatalf("%s has no peer ID or address", n.cfg.Node.Name)
		}
	}
}

func TestStandaloneIsPlain(t *testing.T) {
	e := newEnv(t, "")
	if e.s.cl != nil || e.s.PeerID() != "" || e.s.P2PAddrs() != nil {
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
	if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "x", Type: v1.DatabaseType_DATABASE_TYPE_CRDT})); err == nil {
		t.Fatal("a standalone node accepted a crdt database")
	}
	// A raft database is a regular one: plain on a node of its own.
	r := must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "r", Type: v1.DatabaseType_DATABASE_TYPE_RAFT})))
	if r.Msg.Database.Type != v1.DatabaseType_DATABASE_TYPE_PLAIN {
		t.Fatalf("a raft database on a standalone node is %v", r.Msg.Database.Type)
	}
}

// TestClusterStateFromCRDT: a node whose state was kept in CRDT mode starts
// in a cluster whose state is in Leader mode: its records come in by import,
// on every node, and the old file stays beside the new one.
func TestClusterStateFromCRDT(t *testing.T) {
	ctx := context.Background()
	cfgs := clusterConfigs(t, 3, "")
	path := filepath.Join(cfgs[0].DataDir, "state.musq")
	db, err := replication.Open(ctx, path, replication.CRDT())
	if err != nil {
		t.Fatal(err)
	}
	old, err := state.OpenReplicated(db, "node-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := old.CreateUser(ctx, "admin@example.com", "Admin", state.HashPassword("admin-password"), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.CreateOrg(ctx, "kept", "Kept", u.ID, "fra"); err != nil {
		t.Fatal(err)
	}
	old.Close()

	nodes := startCluster(t, cfgs)
	for _, n := range nodes {
		eventually(t, "the org on "+n.cfg.Node.Name, func() bool {
			_, err := n.s.Store().OrgBySlug(ctx, "kept")
			return err == nil
		})
	}
	for _, f := range []string{"state.musq.crdt", "state.musq.imported"} {
		if _, err := os.Stat(filepath.Join(cfgs[0].DataDir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
