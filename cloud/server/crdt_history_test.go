package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// rows renders query's rows on db over Hrana on node c, typed, a line each,
// or "error: …".
func (c *clusterNode) rows(db, query string) string {
	status, res := c.pipeline("/db/acme/"+db, c.admin, query)
	if status != 200 || len(res) != 1 {
		return fmt.Sprintf("error: HTTP %d", status)
	}
	if res[0].Type != "ok" {
		return "error: " + res[0].Error.Message
	}
	var b strings.Builder
	for _, r := range res[0].Response.Result.Rows {
		for i, v := range r {
			if i > 0 {
				b.WriteByte('|')
			}
			fmt.Fprintf(&b, "%s:%v", v.Type, v.Value)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// settle waits until every node answers each query on db alike, and returns
// the answers.
func settle(t *testing.T, nodes []*clusterNode, db string, queries []string) map[string]string {
	t.Helper()
	var got map[string]string
	var diff string
	defer func() {
		if t.Failed() {
			t.Log(diff)
		}
	}()
	eventually(t, db+" alike on every node", func() bool {
		got = map[string]string{}
		for _, q := range queries {
			first := nodes[0].rows(db, q)
			for _, n := range nodes {
				if a := n.rows(db, q); a != first || strings.HasPrefix(a, "error: ") {
					diff = fmt.Sprintf("%s\n%s:\n%s\nnode-0:\n%s", q, n.cfg.Node.Name, a, first)
					return false
				}
			}
			got[q] = first
		}
		return true
	})
	return got
}

// holds waits until db answers each query on every node as want has it.
func holds(t *testing.T, nodes []*clusterNode, db string, want map[string]string) {
	t.Helper()
	var diff string
	defer func() {
		if t.Failed() {
			t.Log(diff)
		}
	}()
	eventually(t, db+" as wanted on every node", func() bool {
		diff = ""
		for _, n := range nodes {
			for q, w := range want {
				if got := n.rows(db, q); got != w {
					diff += fmt.Sprintf("%s on %s: %s\n got %s\nwant %s\n", db, n.cfg.Node.Name, q, got, w)
				}
			}
		}
		return diff == ""
	})
}

type crdtPoint struct {
	at   time.Time
	want map[string]string
}

// crdtMark settles db and records when it held that.
func crdtMark(t *testing.T, nodes []*clusterNode, db string, queries []string) crdtPoint {
	t.Helper()
	want := settle(t, nodes, db, queries)
	time.Sleep(5 * time.Millisecond)
	p := crdtPoint{at: time.Now(), want: want}
	time.Sleep(5 * time.Millisecond)
	return p
}

const userSchema = `SELECT type, name, sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE '\_repl\_%' ESCAPE '\' ORDER BY name`

// A crdt database's export is the user's database, in either format: none of
// replication's bookkeeping, and no guard refusing a plain connection's write.
func TestClusterCRDTExportIsPlain(t *testing.T) {
	nodes := newCluster(t, 2)
	acmeEverywhere(t, nodes)
	nodes[0].exec("/db/acme/app", nodes[0].admin,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "CREATE INDEX t_v ON t(v)", "CREATE TABLE k(a, b)",
		"INSERT INTO t(v) VALUES ('a'), ('b')", "INSERT INTO k VALUES (1, x'01'), (2.5, NULL)")
	eventually(t, "the rows on node 1", func() bool { return nodes[1].count("app", "k") == 2 })
	ctx := context.Background()
	_, _, dbs := nodes[1].clients()
	dir := t.TempDir()
	for _, f := range []v1.FileFormat{v1.FileFormat_FILE_FORMAT_MUSQL, v1.FileFormat_FILE_FORMAT_SQLITE} {
		out := need(dbs.Export(ctx, auth(nodes[1].admin, &v1.ExportRequest{Org: "acme", Name: "app", Format: f})))(t)
		p := filepath.Join(dir, f.String())
		if err := os.WriteFile(p, out.Msg.Content, 0o600); err != nil {
			t.Fatal(err)
		}
		if f == v1.FileFormat_FILE_FORMAT_SQLITE {
			if err := sqliteconv.Import(p, p+".musq", sqliteconv.ImportOptions{}); err != nil {
				t.Fatal(err)
			}
			p += ".musq"
		}
		dump := dumpFile(t, p)
		if strings.Contains(dump, "_repl_") || !strings.Contains(dump, "CREATE INDEX t_v ON t(v)") || !strings.Contains(dump, "string:b|") || !strings.Contains(dump, "float64:2.5|<nil>:<nil>|") {
			t.Fatalf("%v export:\n%s", f, dump)
		}
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("INSERT INTO t(v) VALUES ('c')")
		db.Close()
		if err != nil {
			t.Fatalf("a write to the %v export: %v", f, err)
		}
	}
}

// branchQueries are what the branch tests compare: the user's schema and every
// row of app as historyApp writes it.
var branchQueries = []string{userSchema, "SELECT * FROM u ORDER BY id", "SELECT * FROM p ORDER BY x, y", "SELECT a, b FROM k ORDER BY a"}

// historyApp writes app through three points, the last changed on node 1, and
// returns them.
func historyApp(t *testing.T, nodes []*clusterNode) []crdtPoint {
	t.Helper()
	n0, n1 := nodes[0], nodes[1]
	const app = "/db/acme/app"
	n0.exec(app, n0.admin,
		"CREATE TABLE u(id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT UNIQUE)",
		"CREATE TABLE p(x TEXT, y INT, z, PRIMARY KEY(x, y))",
		"CREATE TABLE k(a, b)",
		"INSERT INTO u VALUES (1, 'one', 'a@'), (2, 'two', 'b@')",
		"INSERT INTO p VALUES ('a', 1, 1.5), ('a', 2, x'00')",
		"INSERT INTO k VALUES (1, 'one'), (2, NULL)")
	p0 := crdtMark(t, nodes, "app", branchQueries)
	n0.exec(app, n0.admin, "UPDATE u SET name = 'uno' WHERE id = 1", "DELETE FROM p WHERE y = 2", "INSERT INTO k VALUES (3, 'three')", "CREATE INDEX u_name ON u(name)")
	p1 := crdtMark(t, nodes, "app", branchQueries)
	n1.exec(app, n1.admin, "INSERT INTO u VALUES (3, 'three', 'c@')", "DELETE FROM k WHERE a = 1")
	return []crdtPoint{p0, p1, crdtMark(t, nodes, "app", branchQueries)}
}

// A branch (now, at a time, schema only) is a new crdt database made from this
// node's history, on every node of its group, apart from its parent.
func TestClusterCRDTBranch(t *testing.T) {
	nodes := newCluster(t, 2)
	acmeEverywhere(t, nodes)
	n0, n1 := nodes[0], nodes[1]
	points := historyApp(t, nodes)
	p1, now := points[1], points[2]

	ctx := context.Background()
	_, _, dbs := n0.clients()
	branchOf := &v1.CreateDatabaseRequest_BranchOf{BranchOf: "app"}
	need(dbs.Create(ctx, auth(n0.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "b-now", Source: branchOf})))(t)
	need(dbs.Create(ctx, auth(n0.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "b-1", Source: branchOf, BranchAt: timestamppb.New(p1.at)})))(t)
	need(dbs.Create(ctx, auth(n0.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "b-schema", Source: branchOf, SchemaOnly: true})))(t)
	holds(t, nodes, "b-now", now.want)
	holds(t, nodes, "b-1", p1.want)
	holds(t, nodes, "b-schema", map[string]string{userSchema: now.want[userSchema], "SELECT (SELECT count(*) FROM u) + (SELECT count(*) FROM p) + (SELECT count(*) FROM k)": "integer:0\n"})

	// A branch is a database of its own.
	n1.exec("/db/acme/b-now", n1.admin, "INSERT INTO u VALUES (9, 'nine', 'n@')")
	eventually(t, "the branch's write on node 0", func() bool { return n0.count("b-now", "u") == 4 })
	if got := n0.rows("app", branchQueries[1]); got != now.want[branchQueries[1]] {
		t.Fatalf("a write to the branch reached its parent:\n%s", got)
	}

	// Made here, so its group must be placed here.
	need(n0.groups().Create(ctx, auth(n0.admin, &v1.CreateGroupRequest{Org: "acme", Name: "far", Location: "iad"})))(t)
	_, err := dbs.Create(ctx, auth(n0.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "b-far", Source: branchOf, Group: "far"}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("a branch into a group not placed here: %v", err)
	}
}

// A restore into a new database, from the history of a node the parent was
// placed on rather than made on, is a new crdt database on every node.
func TestClusterCRDTRestoreAsNew(t *testing.T) {
	nodes := newCluster(t, 2)
	acmeEverywhere(t, nodes)
	p0 := historyApp(t, nodes)[0]
	_, _, dbs := nodes[1].clients()
	r := need(dbs.Restore(context.Background(), auth(nodes[1].admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(p0.at), NewName: "r-0"})))(t)
	if d := r.Msg.Database; d.Parent == "" || d.Type != v1.DatabaseType_DATABASE_TYPE_CRDT {
		t.Fatalf("restored into %+v", d)
	}
	holds(t, nodes, "r-0", p0.want)
}

// An in-place restore writes the difference: every node converges to the
// state at T whatever changed since (updates, deletes, re-inserts, a UNIQUE
// value traded between two rows, a keyless table's rows), the database stays
// live, and a restore to before it undoes it. Across a schema change it is
// declined.
func TestClusterCRDTRestoreInPlace(t *testing.T) {
	nodes := newCluster(t, 3)
	acmeEverywhere(t, nodes)
	const app = "/db/acme/app"
	queries := []string{"SELECT * FROM u ORDER BY id", "SELECT * FROM p ORDER BY x, y", "SELECT rowid, a, b FROM k ORDER BY rowid"}
	nodes[0].exec(app, nodes[0].admin,
		"CREATE TABLE u(id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT UNIQUE, n, twice INT AS (id * 2))",
		"CREATE TABLE p(x TEXT, y INT, z, PRIMARY KEY(x, y))",
		"CREATE TABLE k(a, b)")
	eventually(t, "the tables on every node", func() bool {
		for _, n := range nodes {
			if n.count("app", "k") != 0 {
				return false
			}
		}
		return true
	})
	nodes[0].exec(app, nodes[0].admin, "INSERT INTO u(id, name, email, n) VALUES (1, 'one', 'a@', 1), (2, 'two', 'b@', 2.5), (3, 'three', 'c@', 'x'), (4, 'four', NULL, x'00')")
	nodes[1].exec(app, nodes[1].admin, "INSERT INTO p VALUES ('a', 1, 'first'), ('a', 2, 2), ('b', 1, NULL)")
	nodes[2].exec(app, nodes[2].admin, "INSERT INTO k VALUES (1, 'one'), (2, 'two'), (3, 'three')")
	at := crdtMark(t, nodes, "app", queries)

	nodes[1].exec(app, nodes[1].admin,
		"UPDATE u SET email = NULL WHERE id = 1", "UPDATE u SET email = 'a@' WHERE id = 2", "UPDATE u SET email = 'b@' WHERE id = 1",
		"UPDATE u SET n = 3 WHERE id = 3", "DELETE FROM u WHERE id = 4", "INSERT INTO u(id, name, email, n) VALUES (5, 'five', 'e@', 5)")
	nodes[2].exec(app, nodes[2].admin,
		"DELETE FROM p WHERE x = 'a' AND y = 1", "INSERT INTO p VALUES ('a', 1, 'again')", "UPDATE p SET z = 'zz' WHERE x = 'b'", "DELETE FROM p WHERE y = 2",
		"DELETE FROM k WHERE a = 1", "UPDATE k SET b = 'TWO' WHERE a = 2", "INSERT INTO k VALUES (4, 'four')")
	since := crdtMark(t, nodes, "app", queries)
	for _, q := range queries {
		if since.want[q] == at.want[q] {
			t.Fatalf("%s: nothing changed since T", q)
		}
	}

	ctx := context.Background()
	_, _, dbs := nodes[1].clients()
	inPlace := func(to time.Time) error {
		_, err := dbs.Restore(ctx, auth(nodes[1].admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(to), InPlace: true}))
		return err
	}
	if err := inPlace(at.at); err != nil {
		t.Fatal(err)
	}
	holds(t, nodes, "app", at.want)

	nodes[2].exec(app, nodes[2].admin, "INSERT INTO k VALUES (5, 'five')")
	eventually(t, "a write after the restore on every node", func() bool {
		for _, n := range nodes {
			if n.count("app", "k") != 4 {
				return false
			}
		}
		return true
	})
	if err := inPlace(since.at); err != nil {
		t.Fatal(err)
	}
	holds(t, nodes, "app", since.want)

	nodes[0].exec(app, nodes[0].admin, "ALTER TABLE u ADD COLUMN extra")
	eventually(t, "the new column on node 1", func() bool { return !strings.HasPrefix(nodes[1].rows("app", "SELECT extra FROM u"), "error: ") })
	if err := inPlace(at.at); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "schema changed since") {
		t.Fatalf("an in-place restore across a schema change: %v", err)
	}
}

// A row withheld by a UNIQUE conflict is alive and in no table, which writes
// to the tables cannot reproduce: an in-place restore is declined, and a copy
// does not carry the row.
func TestClusterCRDTRestoreDeclinesWithheld(t *testing.T) {
	nodes := newCluster(t, 2)
	acmeEverywhere(t, nodes)
	n0, n1 := nodes[0], nodes[1]
	const app = "/db/acme/app"
	n0.exec(app, n0.admin, "CREATE TABLE w(id INTEGER PRIMARY KEY, email TEXT UNIQUE)")
	eventually(t, "w on node 1", func() bool { return n1.count("app", "w") == 0 })
	time.Sleep(5 * time.Millisecond)
	before := time.Now()

	// The same value written on each side while they are apart.
	if err := n1.s.Close(); err != nil {
		t.Fatal(err)
	}
	n0.exec(app, n0.admin, "INSERT INTO w VALUES (1, 'x@')")
	if err := n0.s.Close(); err != nil {
		t.Fatal(err)
	}
	n1.start()
	eventually(t, "node 1 to serve app alone", func() bool { return n1.count("app", "w") == 0 })
	n1.exec(app, n1.admin, "INSERT INTO w VALUES (2, 'x@')")
	n0.start()
	const q = "SELECT id, email FROM w"
	holds(t, nodes, "app", map[string]string{q: "integer:2|text:x@\n"})

	ctx := context.Background()
	_, _, dbs := n0.clients()
	_, err := dbs.Restore(ctx, auth(n0.admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(before), InPlace: true}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("an in-place restore with a withheld row: %v", err)
	}
	need(dbs.Restore(ctx, auth(n0.admin, &v1.RestoreRequest{Org: "acme", Name: "app", NewName: "w-copy"})))(t)
	holds(t, nodes, "w-copy", map[string]string{q: "integer:2|text:x@\n"})
}
