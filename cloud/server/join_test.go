package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// runStandalone runs a node without peers on dir, at location home, for fn.
func runStandalone(t *testing.T, dir string, fn func(e *env)) {
	t.Helper()
	cfg, err := ParseConfig([]byte("data_dir: " + dir + "\nnode:\n  location: home\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(context.Background(), cfg, "admin@example.com", "admin-password"); err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	defer ts.Close()
	cfg.PublicURL = ts.URL
	s, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	h = s.Handler()
	e := &env{t: t, s: s, ts: ts}
	e.admin = e.tokenFor("admin@example.com")
	fn(e)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestClusterJoin: a standalone node joins a cluster in place, its state and
// databases replicated from its first start with peers. One holding what a
// cluster refuses stops it, named, before anything is changed; once fixed,
// the node joins, its groups placed at its cluster location, and the other
// nodes get its users, orgs and rows.
func TestClusterJoin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	runStandalone(t, dir, func(e *env) {
		_, orgs, dbs := e.clients()
		need(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "acme", Name: "Acme"})))(t)
		need(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app"})))(t)
		need(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "vt"})))(t)
		e.exec("/db/acme/app", e.admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t(v) VALUES ('a'), ('b'), ('c')")
		e.exec("/db/acme/vt", e.admin, "CREATE VIRTUAL TABLE f USING fts4(body)")
	})
	cfgs := clusterConfigs(t, 3, dir)
	if s, err := New(ctx, cfgs[0], nil); err == nil {
		s.Close()
		t.Fatal("a node with a virtual table joined a cluster")
	} else if !strings.Contains(err.Error(), "acme/vt") || !strings.Contains(err.Error(), "virtual table") || strings.Contains(err.Error(), "acme/app") {
		t.Fatalf("the refusal does not name acme/vt alone: %v", err)
	}
	runStandalone(t, dir, func(e *env) {
		e.exec("/db/acme/vt", e.admin, "DROP TABLE f")
	})

	nodes := startCluster(t, cfgs)
	// The node's records come in by import, its group placed at its cluster
	// location rather than the one it had alone.
	eventually(t, "acme's group at fra", func() bool {
		g, err := nodes[0].s.Store().Group(ctx, orgID(nodes[0]), "default")
		return err == nil && slices.Contains(g.Locations, "fra")
	})
	for _, loc := range locations[1:] {
		need(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: loc})))(t)
	}
	eventually(t, "app on every node", func() bool { return placed(nodes, "app") && placed(nodes, "vt") })
	eventually(t, "app's rows on every node", func() bool { return counts(nodes, "app", "t", 3) })
	nodes[2].exec("/db/acme/app", nodes[2].admin, "INSERT INTO t(v) VALUES ('from sea')")
	eventually(t, "a write from a new node", func() bool { return counts(nodes, "app", "t", 4) })
	// The state came along: the standalone node's admin signed in on every
	// node (startCluster), and its org is everyone's.
	_, orgs, _ := nodes[2].clients()
	list := need(orgs.List(ctx, auth(nodes[2].admin, &v1.ListOrgsRequest{})))(t)
	if !slices.ContainsFunc(list.Msg.Orgs, func(o *v1.Org) bool { return o.Slug == "acme" }) {
		t.Fatalf("acme is not on sea: %v", list.Msg.Orgs)
	}
}

// TestClusterJoinRunning: a standalone node joins a cluster already running
// with data of its own: the two states merge, and the node's databases are
// served everywhere once placed there.
func TestClusterJoinRunning(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	runStandalone(t, dir, func(e *env) {
		_, orgs, dbs := e.clients()
		need(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "acme", Name: "Acme"})))(t)
		need(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app"})))(t)
		e.exec("/db/acme/app", e.admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t(v) VALUES ('a'), ('b')")
	})
	cfgs := clusterConfigs(t, 3, dir)
	if err := Init(ctx, cfgs[1], "admin@example.com", "admin-password"); err != nil {
		t.Fatal(err)
	}
	// Two of three run, a majority: the cluster works and gets data of its own.
	running := startCluster(t, cfgs[1:])
	_, orgs, _ := running[0].clients()
	need(orgs.Create(ctx, auth(running[0].admin, &v1.CreateOrgRequest{Slug: "beta", Name: "Beta"})))(t)

	nodes := append(startCluster(t, cfgs[:1]), running...)
	for _, n := range nodes {
		eventually(t, "both orgs on "+n.cfg.Node.Name, func() bool {
			_, orgs, _ := n.clients()
			list, err := orgs.List(ctx, auth(n.admin, &v1.ListOrgsRequest{}))
			if err != nil {
				return false
			}
			has := func(slug string) bool {
				return slices.ContainsFunc(list.Msg.Orgs, func(o *v1.Org) bool { return o.Slug == slug })
			}
			return has("acme") && has("beta")
		})
	}
	for _, loc := range locations[1:] {
		need(nodes[0].groups().AddLocation(ctx, auth(nodes[0].admin, &v1.AddLocationRequest{Org: "acme", Group: "default", Location: loc})))(t)
	}
	eventually(t, "app on every node", func() bool { return placed(nodes, "app") })
	eventually(t, "app's rows on every node", func() bool { return counts(nodes, "app", "t", 2) })
}
