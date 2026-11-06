package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

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

// TestClusterRegularDatabase: a regular database, durable or fast, takes
// writes on every node of its group, transactions included, and its master
// runs them all. When the master stops, the others elect another and go on
// writing, and the stopped node catches up when it is back.
func TestClusterRegularDatabase(t *testing.T) {
	for _, du := range []v1.Durability{v1.Durability_DURABILITY_DURABLE, v1.Durability_DURABILITY_FAST} {
		t.Run(du.String(), func(t *testing.T) { regularDatabase(t, du) })
	}
}

func regularDatabase(t *testing.T, du v1.Durability) {
	nodes := newCluster(t, 3)
	acmeEverywhereWith(t, nodes, v1.DatabaseType_DATABASE_TYPE_PLAIN, du)
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

// ids is t's ids in db on node c, in order, "" on any error.
func (c *clusterNode) ids(db string) string {
	status, res := c.pipeline("/db/acme/"+db, c.admin, "SELECT group_concat(id || v) FROM (SELECT id, v FROM t ORDER BY id)")
	if status != 200 || len(res) != 1 || res[0].Type != "ok" || len(res[0].Response.Result.Rows) != 1 {
		return ""
	}
	return fmt.Sprint(res[0].Response.Result.Rows[0][0].Value)
}

// TestClusterDurableDatabase: a durable database's commit is in its group's
// raft log on a majority of the nodes when it returns, so the master stopping
// right after it loses nothing: the master the others elect has the rows. Its
// rowids are the table's, 1, 2, 3, ... whichever node writes.
func TestClusterDurableDatabase(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhereAs(t, nodes, v1.DatabaseType_DATABASE_TYPE_PLAIN)
	const app = "/db/acme/app"
	var m int
	eventually(t, "one master", func() bool { m = masterOf(nodes, "app"); return m >= 0 })
	nodes[m].exec(app, nodes[m].admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t(v) VALUES ('a'), ('b')", "DELETE FROM t WHERE id = 2")
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
		n.exec(app, n.admin, "INSERT INTO t(v) VALUES ('after')")
	}
	var seen []string
	defer func() {
		if t.Failed() {
			t.Logf("ids on the rest: %q", seen)
		}
	}()
	eventually(t, "every row on the rest", func() bool {
		seen = seen[:0]
		for _, n := range rest {
			seen = append(seen, n.ids("app"))
		}
		return seen[0] == "1a,3after,4after" && seen[1] == "1a,3after,4after"
	})
}

// TestClusterUsageCountsEveryNode: a usage read on one node counts what every
// node served up to it, not what the others last flushed.
func TestClusterUsageCountsEveryNode(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhereAs(t, nodes, v1.DatabaseType_DATABASE_TYPE_CRDT)
	ctx := context.Background()
	nodes[0].exec("/db/acme/app", nodes[0].admin, "CREATE TABLE t(a)")
	eventually(t, "t on every node", func() bool { return counts(nodes, "app", "t", 0) })
	_, _, dbs := nodes[0].clients()
	base := need(dbs.Usage(ctx, auth(nodes[0].admin, &v1.UsageRequest{Org: "acme", Name: "app"})))(t).Msg
	var before int64
	for _, p := range base.Points {
		before += p.Statements
	}
	// Each node serves its own: a crdt database is written where it is reached.
	for i, n := range nodes {
		for range i + 1 {
			n.exec("/db/acme/app", n.admin, "INSERT INTO t VALUES (1)")
		}
	}
	u := need(dbs.Usage(ctx, auth(nodes[0].admin, &v1.UsageRequest{Org: "acme", Name: "app"})))(t).Msg
	var after int64
	for _, p := range u.Points {
		after += p.Statements
	}
	if after-before != 6 {
		t.Fatalf("node 0 counts %d statements since, want the 6 the three nodes served", after-before)
	}
	st := need(dbs.Stats(ctx, auth(nodes[0].admin, &v1.StatsRequest{Org: "acme", Name: "app"})))(t).Msg
	for _, q := range st.TopQueries {
		if q.Sql == "INSERT INTO t VALUES (?)" && q.Count != 6 {
			t.Fatalf("the insert counted %d times, want 6", q.Count)
		}
	}
}

// TestClusterRegularTriggers: a regular database takes triggers and WITHOUT
// ROWID tables. The master runs a trigger once per write; what it wrote
// reaches every node, which applies it without firing its own copy. After a
// failover the new master fires the trigger in its turn.
func TestClusterRegularTriggers(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhereAs(t, nodes, v1.DatabaseType_DATABASE_TYPE_PLAIN)
	const app = "/db/acme/app"
	n := nodes[0]
	n.exec(app, n.admin,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		"CREATE TABLE log(k TEXT, n INT, msg, PRIMARY KEY(k, n)) WITHOUT ROWID",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES ('t', new.id, 'added ' || new.v); END")
	for _, x := range nodes {
		x.exec(app, x.admin, "INSERT INTO t(v) VALUES ('"+x.cfg.Node.Name+"')")
	}
	eventually(t, "one log row per insert on every node", func() bool { return counts(nodes, "app", "log", 3) })

	var m int
	eventually(t, "one master", func() bool { m = masterOf(nodes, "app"); return m >= 0 })
	if err := nodes[m].s.Close(); err != nil {
		t.Fatal(err)
	}
	var rest []*clusterNode
	for i, x := range nodes {
		if i != m {
			rest = append(rest, x)
		}
	}
	eventually(t, "a new master", func() bool { return masterOf(rest, "app") >= 0 })
	rest[0].exec(app, rest[0].admin, "INSERT INTO t(v) VALUES ('after')", "UPDATE log SET msg = upper(msg) WHERE k = 't'")
	var seen []string
	defer func() {
		if t.Failed() {
			t.Logf("log on the rest: %q", seen)
		}
	}()
	eventually(t, "the new master's trigger and the update on the rest", func() bool {
		seen = seen[:0]
		for _, x := range rest {
			status, res := x.pipeline(app, x.admin, "SELECT group_concat(k || ':' || n || ':' || msg, ',') FROM (SELECT * FROM log ORDER BY n)")
			if status != 200 || len(res) != 1 || res[0].Type != "ok" {
				seen = append(seen, fmt.Sprint(status, res))
				continue
			}
			seen = append(seen, fmt.Sprint(res[0].Response.Result.Rows[0][0].Value))
		}
		for _, v := range seen {
			if strings.Count(v, ":ADDED ") != 4 {
				return false
			}
		}
		return true
	})
}

// TestClusterRestoreKeepsTriggers: a regular database restored in place has
// the triggers it had then, and the rows: the restore's own writes fire none,
// so a row it puts back adds nothing a trigger would have.
func TestClusterRestoreKeepsTriggers(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhereAs(t, nodes, v1.DatabaseType_DATABASE_TYPE_PLAIN)
	const app = "/db/acme/app"
	n := nodes[0]
	queries := []string{"SELECT v FROM t ORDER BY v", "SELECT v FROM audit ORDER BY v", "SELECT name FROM sqlite_schema WHERE type = 'trigger' ORDER BY name"}
	n.exec(app, n.admin,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		"CREATE TABLE audit(id INTEGER PRIMARY KEY, v)",
		"CREATE TRIGGER ins AFTER INSERT ON t BEGIN INSERT INTO audit(v) VALUES (new.v); END",
		"INSERT INTO t(v) VALUES ('one'), ('two')")
	at := crdtMark(t, nodes, "app", queries)
	n.exec(app, n.admin,
		"DELETE FROM t WHERE v = 'one'",
		"DROP TRIGGER ins",
		"CREATE TRIGGER other AFTER DELETE ON t BEGIN INSERT INTO audit(v) VALUES ('gone ' || old.v); END",
		"INSERT INTO t(v) VALUES ('three')")
	crdtMark(t, nodes, "app", queries)

	_, _, dbs := nodes[1].clients()
	need(dbs.Restore(context.Background(), auth(nodes[1].admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(at.at), InPlace: true})))(t)
	holds(t, nodes, "app", at.want)
}
