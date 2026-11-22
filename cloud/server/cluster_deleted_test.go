package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

// TestClusterDeleteAndRestore: blocks, delete protection, a delete and a
// restore are state that reaches every node: each enforces the blocks, closes
// a deleted database and keeps its files, serves it again with its rows once
// restored, and removes its own copy after the retention.
func TestClusterDeleteAndRestore(t *testing.T) {
	for _, typ := range []v1.DatabaseType{v1.DatabaseType_DATABASE_TYPE_CRDT, v1.DatabaseType_DATABASE_TYPE_PLAIN} {
		t.Run(typ.String(), func(t *testing.T) { clusterDeleteAndRestore(t, typ) })
	}
}

func clusterDeleteAndRestore(t *testing.T, typ v1.DatabaseType) {
	nodes := newCluster(t, 3)
	acmeEverywhereAs(t, nodes, typ)
	ctx := context.Background()
	const app = "/db/acme/app"
	nodes[0].exec(app, nodes[0].admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, a)", "INSERT INTO t(a) VALUES (1), (2)")
	eventually(t, "the rows on every node", func() bool { return counts(nodes, "app", "t", 2) })
	id := need(nodes[0].s.Store().Database(ctx, orgID(nodes[0]), "app"))(t).ID
	_, _, dbs := nodes[1].clients()

	need(dbs.Update(ctx, auth(nodes[1].admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockWrites: ptrTo(true), DeleteProtection: ptrTo(true)})))(t)
	eventually(t, "writes blocked on every node", func() bool {
		for _, n := range nodes {
			_, res := n.pipeline(app, n.admin, "INSERT INTO t(a) VALUES (9)")
			if len(res) != 1 || res[0].Error.Code != "BLOCKED" {
				return false
			}
		}
		return true
	})
	if n := nodes[2].count("app", "t"); n != 2 {
		t.Fatalf("reads while writes are blocked: %d", n)
	}
	_, _, dbs2 := nodes[2].clients()
	if _, err := dbs2.Delete(ctx, auth(nodes[2].admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete a protected database: %v", err)
	}
	need(dbs.Update(ctx, auth(nodes[1].admin, &v1.UpdateDatabaseRequest{Org: "acme", Name: "app", BlockWrites: ptrTo(false), DeleteProtection: ptrTo(false)})))(t)

	need(dbs2.Delete(ctx, auth(nodes[2].admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})))(t)
	paths := make([]string, len(nodes))
	eventually(t, "app closed on every node, its files kept", func() bool {
		for i, n := range nodes {
			d, err := n.s.Store().DeletedDatabaseByID(ctx, id)
			if err != nil || n.s.replHandle(id) != nil {
				return false
			}
			paths[i] = n.s.dbPath(&d.Database)
			if _, err := os.Stat(paths[i]); err != nil {
				return false
			}
			if status, _ := n.pipeline(app, n.admin, "SELECT 1"); status != http.StatusNotFound {
				return false
			}
		}
		return true
	})

	r := need(dbs.RestoreDeleted(ctx, auth(nodes[1].admin, &v1.RestoreDeletedRequest{Org: "acme", Id: id})))(t)
	if len(r.Msg.Databases) != 1 || r.Msg.Databases[0].Name != "app" {
		t.Fatalf("restored: %v", r.Msg)
	}
	eventually(t, "app back on every node with its rows", func() bool { return counts(nodes, "app", "t", 2) })
	nodes[2].exec(app, nodes[2].admin, "INSERT INTO t(a) VALUES (3)")
	eventually(t, "a write after the restore on every node", func() bool { return counts(nodes, "app", "t", 3) })

	// A group goes and comes back with its databases the same way.
	need(nodes[0].groups().Delete(ctx, auth(nodes[0].admin, &v1.DeleteGroupRequest{Org: "acme", Name: "default"})))(t)
	eventually(t, "app closed on every node with its group", func() bool {
		for _, n := range nodes {
			if n.s.replHandle(id) != nil {
				return false
			}
		}
		return true
	})
	l := need(dbs.ListDeleted(ctx, auth(nodes[1].admin, &v1.ListDeletedRequest{Org: "acme"})))(t)
	if len(l.Msg.Groups) != 1 {
		t.Fatalf("deleted groups: %v", l.Msg)
	}
	need(dbs2.RestoreDeleted(ctx, auth(nodes[2].admin, &v1.RestoreDeletedRequest{Org: "acme", Id: l.Msg.Groups[0].Group.Id})))(t)
	eventually(t, "app back on every node with its group", func() bool { return counts(nodes, "app", "t", 3) })
	nodes[1].exec(app, nodes[1].admin, "INSERT INTO t(a) VALUES (4)")
	eventually(t, "a write after the group's restore on every node", func() bool { return counts(nodes, "app", "t", 4) })

	need(dbs.Delete(ctx, auth(nodes[1].admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})))(t)
	eventually(t, "app closed again on every node", func() bool {
		for _, n := range nodes {
			if n.s.replHandle(id) != nil {
				return false
			}
		}
		return true
	})
	// Past the retention each node removes its copy; the record stays a
	// while longer for a node that has yet to.
	past := time.Now().Add(5*24*time.Hour + time.Hour)
	for i, n := range nodes {
		if err := n.s.sweepDeleted(ctx, past); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(paths[i]); !os.IsNotExist(err) {
			t.Fatalf("node %d: a deleted database's file past its retention: %v", i, err)
		}
	}
	if _, err := nodes[0].s.Store().DeletedDatabaseByID(ctx, id); err != nil {
		t.Fatalf("the record within the grace: %v", err)
	}
	if err := nodes[0].s.sweepDeleted(ctx, past.Add(purgeGrace)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the record gone from every node", func() bool {
		for _, n := range nodes {
			if _, err := n.s.Store().DeletedDatabaseByID(ctx, id); !errors.Is(err, state.ErrNotFound) {
				return false
			}
		}
		return true
	})
}
