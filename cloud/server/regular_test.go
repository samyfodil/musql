package server

import (
	"context"
	"fmt"
	"testing"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// masterOf is the index of the node in nodes that writes regular database
// db, -1 when none does.
func masterOf(nodes []*clusterNode, db string) int {
	who := -1
	for i, n := range nodes {
		d, err := n.s.Store().Database(context.Background(), orgID(n), db)
		if err != nil {
			continue
		}
		if e := n.s.groupMaster(d); e != nil && e.Writer() {
			if who >= 0 {
				return -1
			}
			who = i
		}
	}
	return who
}

func counts(nodes []*clusterNode, db, tbl string, want int) bool {
	for _, n := range nodes {
		if n.count(db, tbl) != want {
			return false
		}
	}
	return true
}

// TestClusterRegularDatabase: a regular database takes writes on every node
// of its group, transactions included, and its master runs them all. When the
// master stops, the others elect another and go on writing, and the stopped
// node catches up when it is back.
func TestClusterRegularDatabase(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhereAs(t, nodes, v1.DatabaseType_DATABASE_TYPE_PLAIN)
	const app = "/db/acme/app"
	nodes[0].exec(app, nodes[0].admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, node TEXT)")
	for i, n := range nodes {
		n.exec(app, n.admin, fmt.Sprintf("INSERT INTO t(node) VALUES ('n%d')", i))
	}
	eventually(t, "every node's write on every node", func() bool { return counts(nodes, "app", "t", 3) })
	var m int
	eventually(t, "one master", func() bool { m = masterOf(nodes, "app"); return m >= 0 })
	// A transaction through a follower: its statements run on the master, in
	// one stream.
	f := nodes[(m+1)%3]
	f.exec(app, f.admin, "BEGIN", "INSERT INTO t(node) VALUES ('tx-1')", "INSERT INTO t(node) VALUES ('tx-2')", "COMMIT")
	eventually(t, "the transaction on every node", func() bool { return counts(nodes, "app", "t", 5) })

	if err := nodes[m].s.Close(); err != nil {
		t.Fatal(err)
	}
	var rest []*clusterNode
	for i, n := range nodes {
		if i != m {
			rest = append(rest, n)
		}
	}
	eventually(t, "a new master", func() bool { return masterOf(rest, "app") >= 0 })
	for _, n := range rest {
		n.exec(app, n.admin, "INSERT INTO t(node) VALUES ('after')")
	}
	eventually(t, "the writes after on the rest", func() bool { return counts(rest, "app", "t", 7) })
	// The management API and sign-in go on too: the state elected a master of
	// its own among the rest.
	_, orgs, _ := rest[0].clients()
	need(orgs.Create(context.Background(), auth(rest[0].admin, &v1.CreateOrgRequest{Slug: "later", Name: "Later"})))(t)

	nodes[m].start()
	nodes[m].ready()
	eventually(t, "the stopped node to catch up", func() bool { return nodes[m].count("app", "t") == 7 })
}
